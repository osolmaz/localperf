package artifact

import (
	"bytes"
	"compress/gzip"
	"database/sql"
	"strings"
	"testing"
)

func TestCheckExactSchemaRejectsExtraColumns(t *testing.T) {
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	if _, err := db.Exec(Schema); err != nil {
		t.Fatal(err)
	}
	if err := checkExactSchema(db); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`ALTER TABLE workloads ADD COLUMN obsolete_json TEXT`); err != nil {
		t.Fatal(err)
	}
	if err := checkExactSchema(db); err == nil || !strings.Contains(err.Error(), "schema object") {
		t.Fatalf("checkExactSchema = %v, want schema mismatch", err)
	}
}

func TestCheckWorkloadContractsAllowsSmallBenchmarkSamples(t *testing.T) {
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(Schema); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO run (id, name, status, created_at) VALUES ('run', 'run', 'completed', '2026-01-01T00:00:00Z')`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO workloads (
		id, run_id, name, role, traffic_json, concurrency_json, samples, metadata_json
	) VALUES ('benchmark', 'run', 'benchmark', 'benchmark', '{}', '[4]', 1, '{"context":{"target":4096,"semantics":"active"}}')`); err != nil {
		t.Fatal(err)
	}
	if err := checkWorkloadContracts(db); err != nil {
		t.Fatalf("small benchmark workload: %v", err)
	}
	if _, err := db.Exec(`UPDATE workloads SET metadata_json = NULL WHERE id = 'benchmark'`); err != nil {
		t.Fatal(err)
	}
	if err := checkWorkloadContracts(db); err == nil || !strings.Contains(err.Error(), "context semantics") {
		t.Fatalf("checkWorkloadContracts = %v, want context rejection", err)
	}
	if _, err := db.Exec(`UPDATE workloads SET metadata_json = '{"context":{"target":4096,"semantics":"active"}}' WHERE id = 'benchmark'`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`PRAGMA foreign_keys = OFF`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO measurements (
		run_id, profile_id, workload_id, concurrency, samples_requested, status
	) VALUES ('run', 'profile', 'benchmark', 4, 1, 'completed')`); err != nil {
		t.Fatal(err)
	}
	if err := checkWorkloadContracts(db); err != nil {
		t.Fatalf("small benchmark measurement: %v", err)
	}
	if _, err := db.Exec(`UPDATE measurements SET concurrency = 5`); err != nil {
		t.Fatal(err)
	}
	if err := checkWorkloadContracts(db); err == nil || !strings.Contains(err.Error(), "declared concurrency ladder") {
		t.Fatalf("checkWorkloadContracts = %v, want concurrency ladder rejection", err)
	}
}

func TestCheckArtifactHashesHandlesPlainGzipAndFailures(t *testing.T) {
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(`CREATE TABLE artifacts (name TEXT, compression TEXT, content BLOB, sha256 TEXT)`); err != nil {
		t.Fatal(err)
	}
	plain := []byte("plain artifact")
	if _, err := db.Exec(`INSERT INTO artifacts VALUES (?, ?, ?, ?)`, "plain", "none", plain, SHA256Hex(plain)); err != nil {
		t.Fatal(err)
	}
	var compressed bytes.Buffer
	writer := gzip.NewWriter(&compressed)
	if _, err := writer.Write([]byte("gzipped artifact")); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO artifacts VALUES (?, ?, ?, ?)`, "gzipped", "gzip", compressed.Bytes(), SHA256Hex([]byte("gzipped artifact"))); err != nil {
		t.Fatal(err)
	}
	if err := checkArtifactHashes(db); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`UPDATE artifacts SET sha256 = 'bad' WHERE name = 'plain'`); err != nil {
		t.Fatal(err)
	}
	if err := checkArtifactHashes(db); err == nil || !strings.Contains(err.Error(), "plain sha256") {
		t.Fatalf("hash error = %v", err)
	}
	if _, err := db.Exec(`UPDATE artifacts SET sha256 = ? WHERE name = 'plain'`, SHA256Hex(plain)); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`UPDATE artifacts SET content = ? WHERE name = 'gzipped'`, []byte("not gzip")); err != nil {
		t.Fatal(err)
	}
	if err := checkArtifactHashes(db); err == nil || !strings.Contains(err.Error(), "gzip decode") {
		t.Fatalf("gzip error = %v", err)
	}
}
