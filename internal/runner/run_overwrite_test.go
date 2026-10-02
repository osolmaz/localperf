package runner

import (
	"database/sql"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/osolmaz/localperf/internal/artifact"
)

func TestCheckRunOverwriteFreshPaths(t *testing.T) {
	root := t.TempDir()
	runDir := filepath.Join(root, "run-a")
	spec := Spec{Name: "suite-x"}
	// A missing run directory and a missing artifact are the normal fresh case.
	if err := CheckRunOverwrite(runDir, spec, "", false); err != nil {
		t.Fatalf("fresh paths should pass: %v", err)
	}
	// An empty existing run directory is also fresh: nothing to overwrite.
	if err := os.MkdirAll(runDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := CheckRunOverwrite(runDir, spec, "", false); err != nil {
		t.Fatalf("empty run directory should pass: %v", err)
	}
}

func TestCheckRunOverwriteExistingRunDir(t *testing.T) {
	root := t.TempDir()
	runDir := filepath.Join(root, "run-a")
	if err := os.MkdirAll(runDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(runDir, "events.jsonl"), []byte("{}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	spec := Spec{Name: "suite-x"}
	err := CheckRunOverwrite(runDir, spec, "", false)
	if err == nil {
		t.Fatal("existing run directory content must be refused without --replace")
	}
	if !strings.Contains(err.Error(), "--replace") {
		t.Fatalf("refusal must name the override flag: %v", err)
	}
	if !strings.Contains(err.Error(), "previous attempt") {
		t.Fatalf("refusal must name the run directory evidence: %v", err)
	}
	if err := CheckRunOverwrite(runDir, spec, "", true); err != nil {
		t.Fatalf("--replace must allow the run: %v", err)
	}
}

func TestCheckRunOverwriteExistingArtifactRun(t *testing.T) {
	root := t.TempDir()
	runDir := filepath.Join(root, "run-a")
	artifactPath := filepath.Join(root, "model.sqlite")
	db, err := sql.Open("sqlite", artifactPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(artifact.Schema); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(
		`INSERT INTO run (id, name, status, created_at) VALUES ('run-a', 'run-a', 'completed', '2026-10-02T00:00:00Z')`); err != nil {
		t.Fatal(err)
	}
	db.Close()

	spec := Spec{Name: "suite-x"}
	err = CheckRunOverwrite(runDir, spec, artifactPath, false)
	if err == nil {
		t.Fatal("an artifact run with the same id must be refused without --replace")
	}
	if !strings.Contains(err.Error(), "already records run") {
		t.Fatalf("refusal must name the recorded run: %v", err)
	}
	if err := CheckRunOverwrite(runDir, spec, artifactPath, true); err != nil {
		t.Fatalf("--replace must allow the run: %v", err)
	}
	// A different run id in the artifact is the normal append case.
	other := filepath.Join(root, "run-b")
	if err := CheckRunOverwrite(other, spec, artifactPath, false); err != nil {
		t.Fatalf("a fresh run id against an existing artifact should pass: %v", err)
	}
}

func TestCheckRunOverwriteUnreadableArtifactIsError(t *testing.T) {
	root := t.TempDir()
	runDir := filepath.Join(root, "run-a")
	bogus := filepath.Join(root, "model.sqlite")
	if err := os.WriteFile(bogus, []byte("this is not a database"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := CheckRunOverwrite(runDir, Spec{Name: "suite-x"}, bogus, false); err == nil {
		t.Fatal("an unreadable existing artifact must be an error, not a silent pass")
	}
}
