package vllmbench

import (
	"database/sql"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	_ "modernc.org/sqlite"
)

// TestArtifactIngestsIdentityAndGPUTelemetryEvents checks that
// engine_identity events fill engines.version and metadata_json.identity,
// and that gpu_telemetry events land in the telemetry tables tagged with
// their measurement.
func TestArtifactIngestsIdentityAndGPUTelemetryEvents(t *testing.T) {
	dir := t.TempDir()
	runDir := filepath.Join(dir, "run")
	if err := os.MkdirAll(runDir, 0o755); err != nil {
		t.Fatal(err)
	}
	spec := testSpec()
	if err := writeJSONFile(filepath.Join(runDir, "spec.normalized.json"), RedactedSpec(spec)); err != nil {
		t.Fatal(err)
	}
	plan := BuildPlan(spec, runDir)
	if len(plan) == 0 {
		t.Fatal("plan is empty")
	}
	planned := plan[0]
	now := time.Now().UTC()
	util := 47.0
	memBytes := 41234.0 * 1024 * 1024
	events := []Event{
		{
			Timestamp: now,
			Type:      "engine_identity",
			Profile:   planned.Profile.Name,
			Details: mustJSON(engineIdentity{
				Version: "0.11.0",
				Models:  json.RawMessage(`{"data":[{"id":"served/other-model"}]}`),
			}),
		},
		{
			Timestamp:   now,
			Type:        "gpu_telemetry",
			Profile:     planned.Profile.Name,
			Workload:    planned.Workload.Name,
			Concurrency: planned.Concurrency,
			Repeat:      planned.Repeat,
			Details: mustJSON(gpuTelemetrySample{
				Source:             "tegrastats",
				GPUUtilizationPct:  &util,
				GPUMemoryUsedBytes: &memBytes,
			}),
		},
	}
	eventsFile, err := os.Create(filepath.Join(runDir, "events.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	encoder := json.NewEncoder(eventsFile)
	for _, event := range events {
		if err := encoder.Encode(event); err != nil {
			t.Fatal(err)
		}
	}
	if err := eventsFile.Close(); err != nil {
		t.Fatal(err)
	}

	artifactPath := filepath.Join(dir, "run.sqlite")
	summary := RunSummary{RunDir: runDir, StartedAt: now, FinishedAt: now, PlannedRuns: len(plan)}
	if err := writeSQLiteArtifact(runDir, artifactPath, spec, summary, ""); err != nil {
		t.Fatal(err)
	}

	db, err := sql.Open("sqlite", artifactPath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var version, servedModel string
	if err := db.QueryRow(`SELECT COALESCE(version, ''),
		COALESCE(json_extract(metadata_json, '$.identity.' || ? || '.models.data[0].id'), '')
		FROM engines LIMIT 1`, planned.Profile.Name).Scan(&version, &servedModel); err != nil {
		t.Fatal(err)
	}
	if version != "0.11.0" || servedModel != "served/other-model" {
		t.Fatalf("engine identity = %q / %q, want 0.11.0 / served/other-model", version, servedModel)
	}
	var utilValue float64
	var measurementID sql.NullInt64
	if err := db.QueryRow(`SELECT ts.value, ts.measurement_id
		FROM telemetry_samples ts JOIN telemetry_series s ON s.id = ts.series_id
		WHERE s.metric = 'gpu_utilization_percent' AND s.source = 'tegrastats'`).Scan(&utilValue, &measurementID); err != nil {
		t.Fatal(err)
	}
	if utilValue != 47 || !measurementID.Valid {
		t.Fatalf("gpu utilization sample = %f (measurement valid %t), want 47 tagged with measurement", utilValue, measurementID.Valid)
	}
	var memValue float64
	if err := db.QueryRow(`SELECT ts.value
		FROM telemetry_samples ts JOIN telemetry_series s ON s.id = ts.series_id
		WHERE s.metric = 'gpu_memory_used_bytes' AND s.source = 'tegrastats'`).Scan(&memValue); err != nil {
		t.Fatal(err)
	}
	if memValue != memBytes {
		t.Fatalf("gpu memory sample = %f, want %f", memValue, memBytes)
	}
}
