package report

import (
	"database/sql"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/osolmaz/localperf/internal/convergence"
)

func TestMeasuredRepeatsDropsConvergenceSkips(t *testing.T) {
	record := &convergence.StopRecord{Reason: convergence.ReasonConverged, N: 2}
	members := []SQLiteReportMeasurement{
		{RepeatIndex: 0, Status: "completed", Convergence: record},
		{RepeatIndex: 1, Status: "completed", Convergence: record},
		{RepeatIndex: 2, Status: "skipped", Convergence: record},
	}
	if got := measuredRepeats(members); len(got) != 2 || got[1].RepeatIndex != 1 {
		t.Fatalf("measured = %+v, want the two completed samples", got)
	}
	// Skips without a convergence decision come from the ladder or a guard
	// and stay visible.
	ladder := []SQLiteReportMeasurement{{Status: "completed"}, {Status: "skipped"}}
	if got := measuredRepeats(ladder); len(got) != 2 {
		t.Fatalf("ladder skip dropped: %+v", got)
	}
	allSkipped := []SQLiteReportMeasurement{{Status: "skipped", Convergence: record}}
	if got := measuredRepeats(allSkipped); len(got) != 1 {
		t.Fatalf("all-skipped point = %+v, want it kept", got)
	}
}

func TestConvergenceNoteMarksOnlyUnconvergedPoints(t *testing.T) {
	cases := map[convergence.Reason]string{
		convergence.ReasonConverged:  "",
		convergence.ReasonFixed:      "",
		convergence.ReasonMaxRepeats: "not converged: max_repeats, ±7.5% at n=10",
		convergence.ReasonTimeBudget: "not converged: time_budget, ±7.5% at n=10",
		convergence.ReasonFailed:     "not converged: failed, ±7.5% at n=10",
	}
	for reason, want := range cases {
		record := &convergence.StopRecord{Reason: reason, N: 10, RelHalfWidth: 0.075, IntervalKnown: true}
		if got := ConvergenceNote(record); got != want {
			t.Fatalf("ConvergenceNote(%s) = %q, want %q", reason, got, want)
		}
	}
	if got := ConvergenceNote(&convergence.StopRecord{Reason: convergence.ReasonFailed}); got != "not converged: failed at n=0" {
		t.Fatalf("note without interval = %q", got)
	}
	if ConvergenceNote(nil) != "" || convergenceDetailItems(nil) != nil {
		t.Fatal("a point without a decision must have no note and no items")
	}
}

func TestConvergenceDetailItems(t *testing.T) {
	items := convergenceDetailItems(&convergence.StopRecord{Reason: convergence.ReasonConverged, N: 3, Mean: 1234.5, HalfWidth: 12.3, RelHalfWidth: 0.00996, IntervalKnown: true})
	if len(items) != 2 || items[0].Value != "converged after 3 sample(s)" || !strings.Contains(items[1].Value, "(±1.0%, n=3)") {
		t.Fatalf("items = %+v", items)
	}
	single := convergenceDetailItems(&convergence.StopRecord{Reason: convergence.ReasonFixed, N: 1})
	if len(single) != 1 {
		t.Fatalf("single-sample items = %+v, want no interval", single)
	}
}

// insertConvergencePoint adds a decode point with three completed samples,
// skipped samples after them, and the point_stopped event of the decision.
func insertConvergencePoint(t *testing.T, artifactPath string, policy convergence.Policy, want convergence.Reason, skipped int) {
	t.Helper()
	db, err := sql.Open("sqlite", artifactPath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	exec := func(query string, args ...any) sql.Result {
		t.Helper()
		result, err := db.Exec(query, args...)
		if err != nil {
			t.Fatal(err)
		}
		return result
	}
	exec(`INSERT INTO workloads (
		id, run_id, name, role, phase, traffic_json, concurrency_json, samples, repeats,
		save_detailed, capture_payload_artifacts, metadata_json
	) VALUES ('workload-conv', 'run-1', 'decode-conv', 'benchmark', 'decode',
		'{"dataset_name":"random","random_input_len":1024,"random_output_len":256}',
		'[1]', 1, 10, 1, 0, '{"context":{"target":4096,"semantics":"capacity"}}')`)
	values := []float64{100, 101, 99}
	var last int64
	for index, value := range values {
		result := exec(`INSERT INTO measurements (
			run_id, profile_id, workload_id, repeat_index, concurrency, samples_requested,
			status, wall_time_ms, completed_requests, failed_requests, prompt_tokens,
			completion_tokens, total_tokens, aggregate_output_tok_s, per_user_output_tok_s,
			aggregate_total_tok_s, metadata_json
		) VALUES ('run-1', 'profile-1', 'workload-conv', ?, 1, 1, 'completed', 1000, 1, 0,
			1024, 256, 1280, ?, ?, ?, '{}')`, index, value, value, value*5)
		last, _ = result.LastInsertId()
	}
	for index := 0; index < skipped; index++ {
		exec(`INSERT INTO measurements (
			run_id, profile_id, workload_id, repeat_index, concurrency, samples_requested,
			status, completed_requests, failed_requests, error_message
		) VALUES ('run-1', 'profile-1', 'workload-conv', ?, 1, 1, 'skipped', 0, 0, 'point stopped')`, len(values)+index)
	}
	durations := []time.Duration{10 * time.Second, 10 * time.Second, 10 * time.Second}
	decision := convergence.Evaluate(values, durations, policy)
	if decision.Reason != want {
		t.Fatalf("fixture decision = %+v, want %s", decision, want)
	}
	record := convergence.NewStopRecord("aggregate_output_tok_s", decision, values, durations, policy, "")
	data, err := json.Marshal(record)
	if err != nil {
		t.Fatal(err)
	}
	exec(`INSERT INTO events (run_id, timestamp, level, type, profile_id, workload_id, measurement_id, data_json)
		VALUES ('run-1', '2026-10-02T00:00:00Z', 'info', ?, 'profile-1', 'workload-conv', ?, ?)`,
		convergence.StopEventType, last, string(data))
}

func convergenceRow(t *testing.T, doc SQLiteReportDocument) SQLiteReportMeasurement {
	t.Helper()
	for _, measurement := range doc.Measurements {
		if measurement.Workload == "decode-conv" {
			return measurement
		}
	}
	t.Fatalf("no decode-conv point in %d measurement(s)", len(doc.Measurements))
	return SQLiteReportMeasurement{}
}

func renderedReport(t *testing.T, doc SQLiteReportDocument) string {
	t.Helper()
	var out strings.Builder
	if err := RenderHTMLReport(&out, doc, HTMLReportOptions{}); err != nil {
		t.Fatal(err)
	}
	return out.String()
}

func TestReportShowsConvergedPointFromItsSamples(t *testing.T) {
	artifactPath := testSQLiteHTMLArtifact(t, "Converged")
	insertConvergencePoint(t, artifactPath, convergence.Policy{MinRepeats: 3, MaxRepeats: 10, TargetRelHalfWidth: 0.05}, convergence.ReasonConverged, 7)
	doc, err := LoadSQLiteReport(artifactPath)
	if err != nil {
		t.Fatal(err)
	}
	point := convergenceRow(t, doc)
	if point.Status != "completed" || point.RepeatCount != 3 || point.Convergence == nil || point.Convergence.Reason != convergence.ReasonConverged {
		t.Fatalf("point status=%s repeats=%d convergence=%+v, want completed, 3, converged", point.Status, point.RepeatCount, point.Convergence)
	}
	html := renderedReport(t, doc)
	if !strings.Contains(html, "converged after 3 sample(s)") || !strings.Contains(html, "95% interval") {
		t.Fatalf("report lacks the stop reason or interval:\n%s", html)
	}
	if strings.Contains(html, `class="not-converged"`) {
		t.Fatal("a converged point must not carry the not-converged marker")
	}
}

func TestReportMarksUnconvergedPoint(t *testing.T) {
	artifactPath := testSQLiteHTMLArtifact(t, "Unconverged")
	// 3 x 10 s elapsed plus an estimated 10 s exceeds the 35 s limit.
	insertConvergencePoint(t, artifactPath, convergence.Policy{MinRepeats: 3, MaxRepeats: 10, TargetRelHalfWidth: 0.0001, MaxPointSeconds: 35}, convergence.ReasonTimeBudget, 0)
	doc, err := LoadSQLiteReport(artifactPath)
	if err != nil {
		t.Fatal(err)
	}
	if note := ConvergenceNote(convergenceRow(t, doc).Convergence); !strings.HasPrefix(note, "not converged: time_budget") {
		t.Fatalf("note = %q", note)
	}
	if html := renderedReport(t, doc); !strings.Contains(html, `class="not-converged" title="not converged: time_budget`) {
		t.Fatalf("report lacks the not-converged marker:\n%s", html)
	}
}
