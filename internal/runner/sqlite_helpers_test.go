package runner

import (
	"database/sql"
	"strings"
	"testing"

	"github.com/osolmaz/localperf/internal/artifact"
)

func TestSQLiteArtifactPathEdges(t *testing.T) {
	if got := artifact.Path("run-dir", "/tmp/custom.sqlite"); got != "/tmp/custom.sqlite" {
		t.Fatalf("override artifact path = %s", got)
	}
	if got := artifact.Path(".", ""); got != "localperf-run.sqlite" {
		t.Fatalf("default artifact path = %s", got)
	}
	if got := artifact.Path("runs/example/", ""); got != "runs/example.sqlite" {
		t.Fatalf("run artifact path = %s", got)
	}
}

func TestInsertArtifactPathStoresPlainAndCompressedContent(t *testing.T) {
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(`CREATE TABLE artifacts (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		run_id TEXT,
		kind TEXT,
		name TEXT,
		media_type TEXT,
		compression TEXT,
		content BLOB,
		content_size_bytes INTEGER,
		uncompressed_size_bytes INTEGER,
		sha256 TEXT,
		original_path TEXT,
		created_at TEXT
	)`); err != nil {
		t.Fatal(err)
	}
	tx, err := db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	plainPath := writeTempArtifact(t, "plain.txt", strings.Repeat("a", 16))
	if id, err := insertArtifactPath(tx, "run", "log", "plain", plainPath, "text/plain"); err != nil || id == 0 {
		t.Fatalf("plain artifact id=%d err=%v", id, err)
	}
	largePath := writeTempArtifact(t, "large.json", strings.Repeat(`{"x":1}`, 10000))
	if id, err := insertArtifactPath(tx, "run", "json", "large", largePath, "application/json"); err != nil || id == 0 {
		t.Fatalf("large artifact id=%d err=%v", id, err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	rows, err := db.Query(`SELECT name, compression FROM artifacts ORDER BY name`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	got := map[string]string{}
	for rows.Next() {
		var name, compression string
		if err := rows.Scan(&name, &compression); err != nil {
			t.Fatal(err)
		}
		got[name] = compression
	}
	if got["plain"] != "none" || got["large"] != "gzip" {
		t.Fatalf("compressions = %#v", got)
	}
}

func writeTempArtifact(t *testing.T, name, content string) string {
	t.Helper()
	path := t.TempDir() + "/" + name
	writeFile(t, path, content)
	return path
}
