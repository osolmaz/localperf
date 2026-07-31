package vllmbench

import (
	"database/sql"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestInsertMeasurementDetailsImportsVLLMBenchRequestSamples(t *testing.T) {
	runDir := t.TempDir()
	resultFile := filepath.Join("results", "vllm.json")
	writeFile(t, filepath.Join(runDir, resultFile), `{
  "date": "20260102-030405",
  "completed": 2,
  "failed": 0,
  "input_lens": [100, 200],
  "output_lens": [3, 2],
  "ttfts": [0.1, 0.2],
  "itls": [[0.05, 0.07], [0.1]],
  "start_times": [10.0, 11.5]
}`)
	db, err := createSQLiteArtifact(filepath.Join(t.TempDir(), "artifact.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := withSQLiteTx(db, func(tx *sql.Tx) error {
		seedMeasurementParents(t, tx)
		id, err := insertMeasurement(tx, measurementInsert{
			runID: "run",
			planned: PlannedRun{
				Profile:     Profile{Name: "profile"},
				Workload:    Workload{Name: "workload", NumPrompts: 2},
				Concurrency: 2,
			},
			status: "completed",
		})
		if err != nil {
			return err
		}
		return insertMeasurementDetails(tx, runDir, id, ReportRow{TTFTSource: TTFTSourceStream}, resultFile)
	}); err != nil {
		t.Fatal(err)
	}
	var requestRows, promptTokens, completionTokens, firstTokenRows int
	var minTTFT, maxTPOT float64
	if err := db.QueryRow(`
		SELECT COUNT(*), COALESCE(SUM(prompt_tokens), 0), COALESCE(SUM(completion_tokens), 0),
		       COALESCE(MIN(ttft_ms), 0), COALESCE(MAX(tpot_ms), 0), COUNT(first_token_at)
		FROM requests`).Scan(&requestRows, &promptTokens, &completionTokens, &minTTFT, &maxTPOT, &firstTokenRows); err != nil {
		t.Fatal(err)
	}
	if requestRows != 2 || promptTokens != 300 || completionTokens != 5 {
		t.Fatalf("request rows/tokens = %d/%d/%d, want 2/300/5", requestRows, promptTokens, completionTokens)
	}
	if !near(minTTFT, 100) || !near(maxTPOT, 100) || firstTokenRows != 2 {
		t.Fatalf("request timing fields = min ttft %.3f max tpot %.3f first tokens %d, want 100/100/2", minTTFT, maxTPOT, firstTokenRows)
	}
	for _, metric := range []string{"request_output_throughput", "request_decode_throughput", "request_ttft", "request_tpot", "request_itl_mean"} {
		var count int
		var stddev sql.NullFloat64
		if err := db.QueryRow(`SELECT count, stddev FROM metric_stats WHERE metric = ?`, metric).Scan(&count, &stddev); err != nil {
			t.Fatalf("%s metric missing: %v", metric, err)
		}
		if count != 2 || !stddev.Valid || stddev.Float64 <= 0 {
			t.Fatalf("%s metric count/stddev = %d/%v, want 2/positive", metric, count, stddev)
		}
	}
	for _, metric := range []string{"effective_prefill_throughput", "request_effective_prefill_throughput"} {
		var mean float64
		var count int
		if err := db.QueryRow(`SELECT mean, count FROM metric_stats WHERE metric = ?`, metric).Scan(&mean, &count); err != nil {
			t.Fatalf("%s metric missing: %v", metric, err)
		}
		if mean <= 0 || count == 0 {
			t.Fatalf("%s mean/count = %.3f/%d, want positive", metric, mean, count)
		}
	}
}

func TestSQLiteArtifactStoresVLLMBenchStreamTimingProvenance(t *testing.T) {
	dir := t.TempDir()
	runDir := filepath.Join(dir, "run")
	if err := os.MkdirAll(filepath.Join(runDir, "results"), 0o755); err != nil {
		t.Fatal(err)
	}
	spec := testSpec()
	spec.Workloads[0].PromptsPerUser = 1
	spec.Workloads[0].MaxConcurrency = []int{2}
	ApplyDefaults(&spec)
	if err := writeJSONFile(filepath.Join(runDir, "spec.normalized.json"), RedactedSpec(spec)); err != nil {
		t.Fatal(err)
	}
	plan := BuildPlan(spec, runDir)
	if len(plan) != 1 {
		t.Fatalf("plan length = %d, want 1", len(plan))
	}
	planned := plan[0]
	resultFile := filepath.Join("results", "vllm.json")
	writeFile(t, filepath.Join(runDir, resultFile), `{
  "date": "2026-01-02T03:04:05Z",
  "completed": 2,
  "failed": 0,
  "input_lens": [100, 200],
  "output_lens": [3, 2],
  "ttfts": [0.1, 0.2],
  "itls": [[0.05, 0.07], [0.1]],
  "start_times": [10.0, 11.5],
  "mean_ttft_ms": 150,
  "p50_ttft_ms": 150,
  "p95_ttft_ms": 195,
  "p99_ttft_ms": 199
}`)
	event := Event{
		Timestamp:   time.Date(2026, 1, 2, 3, 4, 6, 0, time.UTC),
		Type:        "workload_finish",
		Profile:     planned.Profile.Name,
		Workload:    planned.Workload.Name,
		Concurrency: planned.Concurrency,
		Repeat:      planned.Repeat,
		ResultFile:  resultFile,
	}
	eventsFile, err := os.Create(filepath.Join(runDir, "events.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	if err := json.NewEncoder(eventsFile).Encode(event); err != nil {
		_ = eventsFile.Close()
		t.Fatal(err)
	}
	if err := eventsFile.Close(); err != nil {
		t.Fatal(err)
	}

	artifactPath := filepath.Join(dir, "artifact.sqlite")
	summary := RunSummary{
		RunDir:        runDir,
		StartedAt:     event.Timestamp.Add(-time.Second),
		FinishedAt:    event.Timestamp,
		PlannedRuns:   1,
		CompletedRuns: 1,
	}
	if err := writeSQLiteArtifact(runDir, artifactPath, spec, summary, ""); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", artifactPath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var ttftSource string
	if err := db.QueryRow(`SELECT json_extract(metadata_json, '$.ttft_source') FROM measurements`).Scan(&ttftSource); err != nil {
		t.Fatal(err)
	}
	if ttftSource != TTFTSourceStream {
		t.Fatalf("ttft_source = %q, want %q", ttftSource, TTFTSourceStream)
	}
	var firstTokens, effectiveMetrics int
	if err := db.QueryRow(`SELECT COUNT(first_token_at) FROM requests`).Scan(&firstTokens); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow(`SELECT COUNT(*) FROM metric_stats WHERE metric IN (
		'effective_prefill_throughput', 'request_effective_prefill_throughput', 'request_decode_throughput'
	)`).Scan(&effectiveMetrics); err != nil {
		t.Fatal(err)
	}
	if firstTokens != 2 || effectiveMetrics != 3 {
		t.Fatalf("first tokens/effective metrics = %d/%d, want 2/3", firstTokens, effectiveMetrics)
	}
}

func TestMeasurementEffectivePrefillThroughput(t *testing.T) {
	start := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	firstC1 := start.Add(200 * time.Millisecond)
	if got, ok := measurementEffectivePrefillThroughput([]RequestSample{{
		Status:       "completed",
		Streamed:     true,
		StartedAt:    start,
		FirstByteAt:  &firstC1,
		PromptTokens: 100,
	}}); !ok || !near(got, 500) {
		t.Fatalf("c1 effective prefill = %.3f/%t, want 500/true", got, ok)
	}

	firstA := start.Add(100 * time.Millisecond)
	firstB := start.Add(300 * time.Millisecond)
	got, ok := measurementEffectivePrefillThroughput([]RequestSample{
		{Status: "completed", Streamed: true, StartedAt: start, FirstByteAt: &firstA, PromptTokens: 100},
		{Status: "completed", Streamed: true, StartedAt: start.Add(50 * time.Millisecond), FirstByteAt: &firstB, PromptTokens: 200},
	})
	if !ok || !near(got, 1000) {
		t.Fatalf("staggered batch effective prefill = %.3f/%t, want 1000/true", got, ok)
	}
}

func TestMeasurementEffectivePrefillRequiresCompleteStreamEvidence(t *testing.T) {
	start := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	firstToken := start.Add(100 * time.Millisecond)
	for name, sample := range map[string]RequestSample{
		"non-streamed":  {Status: "completed", StartedAt: start, FirstByteAt: &firstToken, PromptTokens: 100},
		"missing token": {Status: "completed", Streamed: true, StartedAt: start, PromptTokens: 100},
		"missing count": {Status: "completed", Streamed: true, StartedAt: start, FirstByteAt: &firstToken},
	} {
		t.Run(name, func(t *testing.T) {
			if got, ok := measurementEffectivePrefillThroughput([]RequestSample{sample}); ok || got != 0 {
				t.Fatalf("effective prefill = %.3f/%t, want 0/false", got, ok)
			}
		})
	}
}

func TestValidateStreamedTTFTEvidenceRejectsNonStreamedSample(t *testing.T) {
	err := validateStreamedTTFTEvidence(ReportRow{TTFTSource: TTFTSourceStream}, []RequestSample{{
		RequestIndex: 3,
		Status:       "completed",
		TTFTMillis:   100,
	}})
	if err == nil {
		t.Fatal("streamed TTFT provenance accepted a non-streamed sample")
	}
}
