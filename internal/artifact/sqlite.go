package artifact

import (
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

const (
	FormatName    = "localperf_run"
	FormatVersion = "1"
)

func Path(runDir, override string) string {
	if strings.TrimSpace(override) != "" {
		return override
	}
	clean := strings.TrimRight(filepath.Clean(runDir), string(filepath.Separator))
	if clean == "." || clean == "" {
		return "localperf-run.sqlite"
	}
	return clean + ".sqlite"
}

func Create(path, schema string) (*sql.DB, error) {
	if err := preparePath(path); err != nil {
		return nil, err
	}
	return openWithSchema(path, schema)
}

// CreateOrAppend opens an existing artifact for appending another run, or
// creates a fresh one. Model-level artifacts accumulate repeated runs of one
// model in a single file; see docs/2026-07-02-default-inference-sweep.md.
// The existing file must be a valid artifact of the supported format;
// anything else is an error rather than something to silently overwrite.
func CreateOrAppend(path, schema string) (*sql.DB, error) {
	info, err := os.Stat(path)
	if err != nil {
		return Create(path, schema)
	}
	if info.IsDir() {
		return nil, fmt.Errorf("%s is a directory", path)
	}
	if info.Size() == 0 {
		return openWithSchema(path, schema)
	}
	if err := Check(path); err != nil {
		return nil, fmt.Errorf("cannot append to %s: %w", path, err)
	}
	return OpenWritable(path)
}

func preparePath(path string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	_ = os.Remove(path)
	return nil
}

func WithTx(db *sql.DB, run func(*sql.Tx) error) error {
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := run(tx); err != nil {
		return err
	}
	return tx.Commit()
}

func openWithSchema(path, schema string) (*sql.DB, error) {
	db, err := open(path, "")
	if err != nil {
		return nil, err
	}
	if _, err := db.Exec(schema); err != nil {
		_ = db.Close()
		return nil, err
	}
	return db, nil
}

func OpenExisting(path, rawQuery string) (*sql.DB, error) {
	if _, err := os.Stat(path); err != nil {
		return nil, err
	}
	return open(path, rawQuery)
}

func OpenReadOnly(path string) (*sql.DB, error) {
	return OpenExisting(path, "mode=ro")
}

func OpenWritable(path string) (*sql.DB, error) {
	return OpenExisting(path, "")
}

func open(path, rawQuery string) (*sql.DB, error) {
	dsn, err := fileDSNWithQuery(path, rawQuery)
	if err != nil {
		return nil, err
	}
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	if _, err := db.Exec("PRAGMA foreign_keys = ON"); err != nil {
		_ = db.Close()
		return nil, err
	}
	return db, nil
}

func fileDSNWithQuery(path, rawQuery string) (string, error) {
	absolute, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	uri := url.URL{Scheme: "file", Path: absolute, RawQuery: rawQuery}
	return uri.String(), nil
}

func Check(path string) error {
	db, err := OpenReadOnly(path)
	if err != nil {
		return err
	}
	defer db.Close()
	for _, check := range []func(*sql.DB) error{
		checkIntegrity,
		checkMetadata,
		checkExactSchema,
		checkRequiredTables,
		checkWorkloadContracts,
		checkSpecHashes,
		checkRunRowCount,
		checkSpecKindRows,
		checkForeignKeys,
		checkArtifactHashes,
	} {
		if err := check(db); err != nil {
			return err
		}
	}
	return nil
}

func checkIntegrity(db *sql.DB) error {
	var integrity string
	if err := db.QueryRow("PRAGMA integrity_check").Scan(&integrity); err != nil {
		return err
	}
	if integrity != "ok" {
		return fmt.Errorf("sqlite integrity_check = %s", integrity)
	}
	return nil
}

// checkRunRowCount accepts one or more runs: model-level artifacts hold one
// run row per benchmark attempt or batch.
func checkRunRowCount(db *sql.DB) error {
	var runRows int
	if err := db.QueryRow("SELECT COUNT(*) FROM run").Scan(&runRows); err != nil {
		return err
	}
	if runRows < 1 {
		return fmt.Errorf("run rows = %d, want at least 1", runRows)
	}
	return nil
}

// checkSpecKindRows requires every run to carry exactly one original and one
// normalized spec.
func checkSpecKindRows(db *sql.DB) error {
	for _, kind := range []string{"original", "normalized"} {
		var missing int
		if err := db.QueryRow(`SELECT COUNT(*) FROM run
			WHERE (SELECT COUNT(*) FROM specs WHERE specs.run_id = run.id AND specs.kind = ?) != 1`, kind).Scan(&missing); err != nil {
			return err
		}
		if missing != 0 {
			return fmt.Errorf("%d run(s) without exactly one %s spec", missing, kind)
		}
	}
	return nil
}

func checkForeignKeys(db *sql.DB) error {
	rows, err := db.Query("PRAGMA foreign_key_check")
	if err != nil {
		return err
	}
	defer rows.Close()
	if rows.Next() {
		return errors.New("foreign key check reported at least one failure")
	}
	return rows.Err()
}

func checkMetadata(db *sql.DB) error {
	values := map[string]string{}
	rows, err := db.Query("SELECT key, value FROM metadata")
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var key, value string
		if err := rows.Scan(&key, &value); err != nil {
			return err
		}
		values[key] = value
	}
	if values["format_name"] != FormatName {
		return fmt.Errorf("format_name = %q, want %q", values["format_name"], FormatName)
	}
	if values["format_version"] != FormatVersion {
		return fmt.Errorf("format_version = %q, want %q", values["format_version"], FormatVersion)
	}
	return nil
}

type schemaObject struct {
	kind string
	name string
	sql  string
}

func checkExactSchema(db *sql.DB) error {
	expectedDB, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		return err
	}
	expectedDB.SetMaxOpenConns(1)
	defer expectedDB.Close()
	if _, err := expectedDB.Exec(Schema); err != nil {
		return fmt.Errorf("create reference schema: %w", err)
	}
	expected, err := readSchemaObjects(expectedDB)
	if err != nil {
		return err
	}
	actual, err := readSchemaObjects(db)
	if err != nil {
		return err
	}
	if len(actual) != len(expected) {
		return fmt.Errorf("schema object count = %d, want %d", len(actual), len(expected))
	}
	for i := range expected {
		if actual[i] != expected[i] {
			return fmt.Errorf("schema object %q does not match the current format", expected[i].name)
		}
	}
	var userVersion int
	if err := db.QueryRow("PRAGMA user_version").Scan(&userVersion); err != nil {
		return err
	}
	if userVersion != 1 {
		return fmt.Errorf("sqlite user_version = %d, want 1", userVersion)
	}
	return nil
}

func readSchemaObjects(db *sql.DB) ([]schemaObject, error) {
	rows, err := db.Query(`SELECT type, name, sql FROM sqlite_master
		WHERE name NOT LIKE 'sqlite_%' AND type IN ('table', 'index')
		ORDER BY type, name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var objects []schemaObject
	for rows.Next() {
		var object schemaObject
		if err := rows.Scan(&object.kind, &object.name, &object.sql); err != nil {
			return nil, err
		}
		objects = append(objects, object)
	}
	return objects, rows.Err()
}

func checkRequiredTables(db *sql.DB) error {
	required := []string{"metadata", "run", "specs", "engines", "profiles", "workloads", "datasets", "source_records", "canonical_requests", "phases", "measurements", "metric_stats", "requests", "request_stream_events", "telemetry_series", "telemetry_samples", "events", "commands", "artifacts", "reports"}
	present := map[string]bool{}
	rows, err := db.Query("SELECT name FROM sqlite_master WHERE type = 'table'")
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return err
		}
		present[name] = true
	}
	for _, table := range required {
		if !present[table] {
			return fmt.Errorf("missing required table %s", table)
		}
	}
	return nil
}

func checkWorkloadContracts(db *sql.DB) error {
	var invalid int
	if err := db.QueryRow(`SELECT COUNT(*) FROM workloads WHERE role NOT IN ('benchmark', 'diagnostic')`).Scan(&invalid); err != nil {
		return err
	}
	if invalid != 0 {
		return fmt.Errorf("workloads with invalid role = %d", invalid)
	}
	if err := db.QueryRow(`SELECT COUNT(*) FROM workloads
		WHERE metadata_json IS NULL
		   OR COALESCE(json_extract(metadata_json, '$.context.target'), 0) <= 0
		   OR COALESCE(json_extract(metadata_json, '$.context.semantics'), '') NOT IN ('active', 'capacity')`).Scan(&invalid); err != nil {
		return err
	}
	if invalid != 0 {
		return fmt.Errorf("workloads with invalid context semantics = %d", invalid)
	}
	if err := db.QueryRow(`SELECT COUNT(*)
		FROM measurements AS measurement
		JOIN workloads AS workload ON workload.id = measurement.workload_id
		WHERE NOT EXISTS (
			SELECT 1 FROM json_each(workload.concurrency_json) AS point
			WHERE CAST(point.value AS INTEGER) = measurement.concurrency
		)`).Scan(&invalid); err != nil {
		return err
	}
	if invalid != 0 {
		return fmt.Errorf("measurements outside their declared concurrency ladder = %d", invalid)
	}
	return nil
}

func checkSpecHashes(db *sql.DB) error {
	rows, err := db.Query("SELECT kind, content, sha256 FROM specs")
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var kind, content, want string
		if err := rows.Scan(&kind, &content, &want); err != nil {
			return err
		}
		if got := SHA256Hex([]byte(content)); got != want {
			return fmt.Errorf("spec %s sha256 = %s, want %s", kind, got, want)
		}
	}
	return rows.Err()
}

func checkArtifactHashes(db *sql.DB) error {
	rows, err := db.Query("SELECT name, compression, content, sha256 FROM artifacts")
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var name, compression, want string
		var content []byte
		if err := rows.Scan(&name, &compression, &content, &want); err != nil {
			return err
		}
		data, err := hashContent(name, compression, content)
		if err != nil {
			return err
		}
		if got := SHA256Hex(data); got != want {
			return fmt.Errorf("artifact %s sha256 = %s, want %s", name, got, want)
		}
	}
	return rows.Err()
}

func hashContent(name, compression string, content []byte) ([]byte, error) {
	if compression != "gzip" {
		return content, nil
	}
	reader, err := gzip.NewReader(bytes.NewReader(content))
	if err != nil {
		return nil, fmt.Errorf("artifact %s gzip decode: %w", name, err)
	}
	data, err := io.ReadAll(reader)
	_ = reader.Close()
	if err != nil {
		return nil, fmt.Errorf("artifact %s gzip read: %w", name, err)
	}
	return data, nil
}

func Content(data []byte, mediaType string) ([]byte, string, error) {
	if !shouldCompress(data, mediaType) {
		return data, "none", nil
	}
	content, err := gzipBytes(data)
	return content, "gzip", err
}

func shouldCompress(data []byte, mediaType string) bool {
	return len(data) > 64*1024 && (strings.HasPrefix(mediaType, "text/") || strings.Contains(mediaType, "json"))
}

func gzipBytes(data []byte) ([]byte, error) {
	var compressed bytes.Buffer
	gzipWriter := gzip.NewWriter(&compressed)
	if _, err := gzipWriter.Write(data); err != nil {
		return nil, err
	}
	if err := gzipWriter.Close(); err != nil {
		return nil, err
	}
	return compressed.Bytes(), nil
}

func SHA256Hex(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func NullString(value string) any {
	if strings.TrimSpace(value) == "" {
		return nil
	}
	return value
}

func StoreReport(path, name, mediaType, originalPath string, content []byte) error {
	db, err := OpenWritable(path)
	if err != nil {
		return err
	}
	defer db.Close()
	return WithTx(db, func(tx *sql.Tx) error {
		runID, err := singleRunID(tx)
		if err != nil {
			return err
		}
		artifactID, err := upsertReportArtifact(tx, runID, name, mediaType, originalPath, content, time.Now().UTC())
		if err != nil {
			return err
		}
		return upsertReportRow(tx, runID, name, mediaType, artifactID, time.Now().UTC())
	})
}

func singleRunID(tx *sql.Tx) (string, error) {
	var runID string
	err := tx.QueryRow("SELECT id FROM run LIMIT 1").Scan(&runID)
	return runID, err
}

func upsertReportArtifact(tx *sql.Tx, runID, name, mediaType, originalPath string, data []byte, createdAt time.Time) (int64, error) {
	content, compression, err := Content(data, mediaType)
	if err != nil {
		return 0, err
	}
	_, err = tx.Exec(`INSERT INTO artifacts (
		run_id, kind, name, media_type, compression, content, content_size_bytes,
		uncompressed_size_bytes, sha256, original_path, created_at
	) VALUES (?, 'normalized_report', ?, ?, ?, ?, ?, ?, ?, ?, ?)
	ON CONFLICT(run_id, kind, name) DO UPDATE SET
		media_type = excluded.media_type,
		compression = excluded.compression,
		content = excluded.content,
		content_size_bytes = excluded.content_size_bytes,
		uncompressed_size_bytes = excluded.uncompressed_size_bytes,
		sha256 = excluded.sha256,
		original_path = excluded.original_path,
		created_at = excluded.created_at`,
		runID, name, mediaType, compression, content, len(content), len(data), SHA256Hex(data),
		NullString(originalPath), createdAt.Format(time.RFC3339))
	if err != nil {
		return 0, err
	}
	var artifactID int64
	err = tx.QueryRow(`SELECT id FROM artifacts WHERE run_id = ? AND kind = 'normalized_report' AND name = ?`, runID, name).Scan(&artifactID)
	return artifactID, err
}

func upsertReportRow(tx *sql.Tx, runID, name, mediaType string, artifactID int64, createdAt time.Time) error {
	_, err := tx.Exec(`INSERT INTO reports (
		run_id, name, format, media_type, artifact_id, created_at
	) VALUES (?, ?, 'html', ?, ?, ?)
	ON CONFLICT(run_id, name, format) DO UPDATE SET
		media_type = excluded.media_type,
		artifact_id = excluded.artifact_id,
		created_at = excluded.created_at`,
		runID, name, mediaType, artifactID, createdAt.Format(time.RFC3339))
	return err
}
