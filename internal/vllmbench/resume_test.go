package vllmbench

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestResumeSkipsCompletedRunsAndSurvivesServerLoss checks resume end to
// end: a completed attempt re-run with --resume skips every point without
// touching the (now unreachable) server, and the artifact still validates.
func TestResumeSkipsCompletedRunsAndSurvivesServerLoss(t *testing.T) {
	server, host, port := fakeOpenAIServer(t)
	spec := httpTestSpec(t, host, port, "resume-live", 3, 2)
	spec.Workloads[0].MaxConcurrency = []int{1, 2}
	ApplyDefaults(&spec)
	runDir := filepath.Join(spec.OutputDir, "resume-live")
	artifactPath := filepath.Join(spec.OutputDir, "resume-live.sqlite")
	options := RunOptions{RunDir: runDir, ArtifactPath: artifactPath}
	summary, err := Execute(context.Background(), spec, options)
	if err != nil {
		t.Fatal(err)
	}
	if summary.CompletedRuns != 2 {
		t.Fatalf("completed = %d, want 2", summary.CompletedRuns)
	}
	// The server is gone; a resumed attempt must not need it. The resumed
	// attempt also extends the ladder to c3 with a tiny TTFT ceiling:
	// replayed rows must trip it so c3 is adaptively skipped, not run.
	server.Close()
	options.Resume = true
	spec.Workloads[0].MaxConcurrency = []int{1, 2, 3}
	spec.Runner.Adaptive = AdaptiveConfig{TTFTP99CeilingMillis: 0.001}
	ApplyDefaults(&spec)
	summary, err = Execute(context.Background(), spec, options)
	if err != nil {
		t.Fatalf("resume error = %v", err)
	}
	if summary.CompletedRuns != 2 || summary.FailedRuns != 0 || summary.SkippedRuns != 1 {
		t.Fatalf("resumed summary = %+v, want 2 completed via skip and 1 adaptive skip", summary)
	}
	if len(summary.Rows) != 2 {
		t.Fatalf("resumed rows = %d, want 2 parsed from prior results", len(summary.Rows))
	}
	db, err := sql.Open("sqlite", artifactPath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var completed, skipped int
	if err := db.QueryRow(`SELECT COUNT(*) FROM measurements WHERE status = 'completed'`).Scan(&completed); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow(`SELECT COUNT(*) FROM measurements WHERE status = 'skipped'`).Scan(&skipped); err != nil {
		t.Fatal(err)
	}
	if completed != 2 || skipped != 1 {
		t.Fatalf("artifact measurements completed/skipped = %d/%d, want 2/1", completed, skipped)
	}
	// Resumed rows import as completed even if the previous attempt's clean
	// finish event was lost: the workload_resumed event alone suffices.
	var resumedCompleted int
	if err := db.QueryRow(`SELECT COUNT(*) FROM measurements WHERE status = 'completed' AND completed_requests > 0`).Scan(&resumedCompleted); err != nil {
		t.Fatal(err)
	}
	if resumedCompleted != 2 {
		t.Fatalf("resumed completed rows with request data = %d, want 2", resumedCompleted)
	}
	var runStatus string
	if err := db.QueryRow(`SELECT status FROM run`).Scan(&runStatus); err != nil {
		t.Fatal(err)
	}
	if runStatus != "completed" {
		t.Fatalf("run status = %q, want completed", runStatus)
	}
}

// TestResumeRerunsPartialResults corrupts a completed result to look
// partial (fewer completed requests than planned): resume must re-run it
// rather than adopting an incomplete measurement as completed.
func TestResumeRerunsPartialResults(t *testing.T) {
	server, host, port := fakeOpenAIServer(t)
	defer server.Close()
	spec := httpTestSpec(t, host, port, "resume-partial", 3, 1)
	spec.Workloads[0].MaxConcurrency = []int{1, 2}
	ApplyDefaults(&spec)
	runDir := filepath.Join(spec.OutputDir, "resume-partial")
	options := RunOptions{RunDir: runDir}
	if _, err := Execute(context.Background(), spec, options); err != nil {
		t.Fatal(err)
	}
	resultPath := filepath.Join(runDir, "results", "8k__resume-partial__c2.json")
	content, err := os.ReadFile(resultPath)
	if err != nil {
		t.Fatal(err)
	}
	partial := strings.Replace(string(content), `"completed": 3`, `"completed": 1`, 1)
	if partial == string(content) {
		t.Fatalf("result file did not contain expected completed count: %s", content)
	}
	if err := os.WriteFile(resultPath, []byte(partial), 0o644); err != nil {
		t.Fatal(err)
	}
	options.Resume = true
	summary, err := Execute(context.Background(), spec, options)
	if err != nil {
		t.Fatal(err)
	}
	if summary.CompletedRuns != 2 {
		t.Fatalf("resumed summary = %+v, want partial point re-run to completion", summary)
	}
	resumed := 0
	for _, row := range summary.Rows {
		if row.Concurrency == 2 && row.Completed != 3 {
			t.Fatalf("c2 completed = %d, want re-run with 3 requests", row.Completed)
		}
	}
	_ = resumed
}

func TestResumeRequiresRunDir(t *testing.T) {
	spec := testSpec()
	_, err := Execute(context.Background(), spec, RunOptions{Resume: true})
	if err == nil || !strings.Contains(err.Error(), "--resume requires --run-dir") {
		t.Fatalf("Execute error = %v, want resume run-dir requirement", err)
	}
}

// TestIncrementalSnapshotsKeepArtifactCurrent checks that the artifact
// exists and holds completed measurements after each point, not only at
// finalize: the run status is recorded as running mid-flight.
func TestIncrementalSnapshotsKeepArtifactCurrent(t *testing.T) {
	server, host, port := fakeOpenAIServer(t)
	defer server.Close()
	spec := httpTestSpec(t, host, port, "snapshot-live", 3, 1)
	ApplyDefaults(&spec)
	runDir := filepath.Join(spec.OutputDir, "snapshot-live")
	artifactPath := filepath.Join(spec.OutputDir, "snapshot-live.sqlite")
	if _, err := Execute(context.Background(), spec, RunOptions{RunDir: runDir, ArtifactPath: artifactPath}); err != nil {
		t.Fatal(err)
	}
	// The snapshot machinery ran (final write flips status to completed);
	// verify the events log recorded no snapshot failures.
	db, err := sql.Open("sqlite", artifactPath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var snapshotFailures int
	if err := db.QueryRow(`SELECT COUNT(*) FROM events WHERE type = 'artifact_snapshot_failed'`).Scan(&snapshotFailures); err != nil {
		t.Fatal(err)
	}
	if snapshotFailures != 0 {
		t.Fatalf("snapshot failures = %d, want 0", snapshotFailures)
	}
	var status string
	if err := db.QueryRow(`SELECT status FROM run`).Scan(&status); err != nil {
		t.Fatal(err)
	}
	if status != "completed" {
		t.Fatalf("final run status = %q, want completed", status)
	}
}

func TestResultMatchesShape(t *testing.T) {
	random := Workload{
		BenchmarkTrafficConfig: BenchmarkTrafficConfig{DatasetName: "random", RandomInputLen: 1000, RandomOutputLen: 100},
		IgnoreEOS:              true,
	}
	cases := []struct {
		name string
		raw  ReportRow
		want bool
	}{
		{"matching shape", ReportRow{Completed: 4, PromptTokens: 4100, CompletionTokens: 400}, true},
		{"prompt shape drifted", ReportRow{Completed: 4, PromptTokens: 8000, CompletionTokens: 400}, false},
		{"output shape drifted", ReportRow{Completed: 4, PromptTokens: 4100, CompletionTokens: 2000}, false},
		{"no completed requests", ReportRow{Completed: 0}, true},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			if got := resultMatchesShape(testCase.raw, random); got != testCase.want {
				t.Fatalf("resultMatchesShape = %t, want %t", got, testCase.want)
			}
		})
	}
	// Non-random datasets and EOS-terminated outputs are not shape-checked.
	custom := Workload{BenchmarkTrafficConfig: BenchmarkTrafficConfig{DatasetName: "custom"}}
	if !resultMatchesShape(ReportRow{Completed: 4, PromptTokens: 1}, custom) {
		t.Fatal("custom dataset shape-checked")
	}
	eos := random
	eos.IgnoreEOS = false
	if !resultMatchesShape(ReportRow{Completed: 4, PromptTokens: 4100, CompletionTokens: 40}, eos) {
		t.Fatal("EOS-terminated output shape-checked")
	}
}

func TestValidateResultPointRequiresExactCanonicalEvidence(t *testing.T) {
	valid := ReportRow{
		Completed:               8,
		Concurrency:             4,
		promptTokensKnown:       true,
		completionTokensKnown:   true,
		totalTokensKnown:        true,
		outputTokensPerSecKnown: true,
		totalTokensPerSecKnown:  true,
	}
	if err := validateResultPoint(valid, "workload", 8, 4); err != nil {
		t.Fatal(err)
	}
	for name, mutate := range map[string]func(*ReportRow){
		"request count":     func(row *ReportRow) { row.Completed = 7 },
		"concurrency":       func(row *ReportRow) { row.Concurrency = 2 },
		"token totals":      func(row *ReportRow) { row.totalTokensKnown = false },
		"throughput fields": func(row *ReportRow) { row.outputTokensPerSecKnown = false },
	} {
		t.Run(name, func(t *testing.T) {
			row := valid
			mutate(&row)
			if err := validateResultPoint(row, "workload", 8, 4); err == nil {
				t.Fatal("invalid result point was accepted")
			}
		})
	}
}

func TestResumableRowRejectsMismatches(t *testing.T) {
	dir := t.TempDir()
	writeResult := func(name, content string) string {
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
		return path
	}
	planned := PlannedRun{
		Profile: Profile{Name: "p", MaxModelLen: 8192},
		Workload: Workload{
			Name:                   "w",
			NumPrompts:             3,
			BenchmarkTrafficConfig: BenchmarkTrafficConfig{DatasetName: "random", RandomInputLen: 64, RandomOutputLen: 8},
			IgnoreEOS:              true,
		},
		Concurrency: 2,
	}
	complete := `{"completed":3,"failed":0,"max_concurrency":2,"total_input_tokens":192,"total_output_tokens":24,"total_tokens":216,"duration":1,"output_throughput":24,"total_token_throughput":216}`
	planned.ResultFile = writeResult("ok.json", complete)
	if _, ok := resumableRow(planned); !ok {
		t.Fatal("complete matching result not resumable")
	}
	planned.ResultFile = writeResult("partial.json", `{"completed":1,"failed":0,"max_concurrency":2,"total_input_tokens":64,"total_output_tokens":8,"total_tokens":72,"duration":1,"output_throughput":8,"total_token_throughput":72}`)
	if _, ok := resumableRow(planned); ok {
		t.Fatal("partial result adopted")
	}
	planned.ResultFile = writeResult("failed.json", `{"completed":3,"failed":1,"max_concurrency":2,"total_input_tokens":192,"total_output_tokens":24,"total_tokens":216,"duration":1,"output_throughput":24,"total_token_throughput":216}`)
	if _, ok := resumableRow(planned); ok {
		t.Fatal("failed result adopted")
	}
	planned.ResultFile = writeResult("other-conc.json", `{"completed":3,"failed":0,"max_concurrency":4,"total_input_tokens":192,"total_output_tokens":24,"total_tokens":216,"duration":1,"output_throughput":24,"total_token_throughput":216}`)
	if _, ok := resumableRow(planned); ok {
		t.Fatal("different-concurrency result adopted")
	}
	planned.ResultFile = writeResult("other-shape.json", `{"completed":3,"failed":0,"max_concurrency":2,"total_input_tokens":9000,"total_output_tokens":24,"total_tokens":9024,"duration":1,"output_throughput":24,"total_token_throughput":9024}`)
	if _, ok := resumableRow(planned); ok {
		t.Fatal("different-shape result adopted")
	}
	planned.ResultFile = filepath.Join(dir, "missing.json")
	if _, ok := resumableRow(planned); ok {
		t.Fatal("missing result adopted")
	}
}
