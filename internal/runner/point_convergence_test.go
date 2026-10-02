package runner

import (
	"context"
	"database/sql"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/osolmaz/localperf/internal/convergence"
)

var wide = convergence.Policy{MinRepeats: 3, MaxRepeats: 6, TargetRelHalfWidth: 0.05}

// convergeAtMin makes every point converge at min_repeats, because fake
// server timings are too noisy for a real interval. The real rule is covered
// by the convergence package tests.
func convergeAtMin(t *testing.T) {
	t.Helper()
	original := evaluatePoint
	evaluatePoint = func(values []float64, durations []time.Duration, policy convergence.Policy) convergence.Decision {
		decision := original(values, durations, policy)
		if len(values) >= policy.MinRepeats && !policy.IsFixed() {
			decision.Stop, decision.Reason = true, convergence.ReasonConverged
		}
		return decision
	}
	t.Cleanup(func() { evaluatePoint = original })
}

func pointStoppedEvents(t *testing.T, db *sql.DB) []PointStoppedDetails {
	t.Helper()
	rows, err := db.Query(`SELECT data_json FROM events WHERE type = ? ORDER BY id`, EventPointStopped)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []PointStoppedDetails
	for rows.Next() {
		var raw string
		if err := rows.Scan(&raw); err != nil {
			t.Fatal(err)
		}
		var details PointStoppedDetails
		if err := json.Unmarshal([]byte(raw), &details); err != nil {
			t.Fatal(err)
		}
		out = append(out, details)
	}
	return out
}

func measurementStatuses(t *testing.T, db *sql.DB) []string {
	t.Helper()
	rows, err := db.Query(`SELECT status FROM measurements ORDER BY repeat_index`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var status string
		if err := rows.Scan(&status); err != nil {
			t.Fatal(err)
		}
		out = append(out, status)
	}
	return out
}

func TestConvergedPointSkipsRemainingSamples(t *testing.T) {
	convergeAtMin(t)
	server, host, port := fakeOpenAIServer(t)
	defer server.Close()
	spec := httpTestSpec(t, host, port, "converge-live", 2, 1)
	spec.Workloads[0].Convergence = wide
	ApplyDefaults(&spec)
	runDir := filepath.Join(spec.OutputDir, "converge-live")
	artifactPath := filepath.Join(spec.OutputDir, "converge-live.sqlite")
	summary, err := Execute(context.Background(), spec, RunOptions{RunDir: runDir, ArtifactPath: artifactPath})
	if err != nil {
		t.Fatal(err)
	}
	if summary.CompletedRuns != 3 || summary.SkippedRuns != 3 || summary.FailedRuns != 0 {
		t.Fatalf("summary = completed %d skipped %d failed %d, want 3/3/0", summary.CompletedRuns, summary.SkippedRuns, summary.FailedRuns)
	}
	db, err := sql.Open("sqlite", artifactPath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	statuses := strings.Join(measurementStatuses(t, db), ",")
	if statuses != "completed,completed,completed,skipped,skipped,skipped" {
		t.Fatalf("measurement statuses = %s", statuses)
	}
	events := pointStoppedEvents(t, db)
	if len(events) != 1 {
		t.Fatalf("point_stopped events = %d, want 1", len(events))
	}
	stopped := events[0]
	if stopped.Reason != convergence.ReasonConverged || stopped.N != 3 || len(stopped.Values) != 3 || len(stopped.DurationsSeconds) != 3 || !stopped.IntervalKnown || stopped.Policy != wide || stopped.Metric != "aggregate_output_tok_s" {
		t.Fatalf("point_stopped = %+v", stopped)
	}
	var reason string
	if err := db.QueryRow(`SELECT error_message FROM measurements WHERE status = 'skipped' LIMIT 1`).Scan(&reason); err != nil {
		t.Fatal(err)
	}
	if reason != "point stopped (converged) after 3 sample(s)" {
		t.Fatalf("skip reason = %q", reason)
	}
}

func TestResumeReplaysSamplesIntoConvergence(t *testing.T) {
	convergeAtMin(t)
	server, host, port := fakeOpenAIServer(t)
	defer server.Close()
	spec := httpTestSpec(t, host, port, "converge-resume", 2, 1)
	spec.Workloads[0].Convergence = convergence.Fixed(4)
	ApplyDefaults(&spec)
	options := RunOptions{RunDir: filepath.Join(spec.OutputDir, "converge-resume"), ArtifactPath: filepath.Join(spec.OutputDir, "converge-resume.sqlite")}
	if _, err := Execute(context.Background(), spec, options); err != nil {
		t.Fatal(err)
	}
	// The same four planned samples under a converging policy: the first
	// three resumed samples stop the point, and the fourth is skipped
	// although its result file exists.
	spec.Workloads[0].Convergence = convergence.Policy{MinRepeats: 3, MaxRepeats: 4, TargetRelHalfWidth: 0.05}
	options.Resume = true
	summary, err := Execute(context.Background(), spec, options)
	if err != nil {
		t.Fatal(err)
	}
	if summary.CompletedRuns != 3 || summary.SkippedRuns != 1 || summary.FailedRuns != 0 {
		t.Fatalf("resumed summary = completed %d skipped %d failed %d, want 3/1/0", summary.CompletedRuns, summary.SkippedRuns, summary.FailedRuns)
	}
	db, err := sql.Open("sqlite", options.ArtifactPath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var resumed int
	if err := db.QueryRow(`SELECT COUNT(*) FROM events WHERE type = 'workload_resumed'`).Scan(&resumed); err != nil {
		t.Fatal(err)
	}
	if resumed != 3 {
		t.Fatalf("workload_resumed events = %d, want 3", resumed)
	}
	// The first attempt recorded its fixed decision; the resumed attempt
	// appends the converged one.
	events := pointStoppedEvents(t, db)
	if len(events) != 2 || events[0].Reason != convergence.ReasonFixed || events[1].Reason != convergence.ReasonConverged || events[1].N != 3 {
		t.Fatalf("point_stopped = %+v, want fixed then converged at 3", events)
	}
}

func TestRecordPointSampleWithoutThroughputStopsAsFailed(t *testing.T) {
	events, err := newEventWriter(filepath.Join(t.TempDir(), "events.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	defer events.Close()
	session := &runSession{points: map[string]*pointState{}, events: events}
	planned := PlannedRun{Profile: Profile{Name: "p"}, Workload: Workload{Name: "w", Phase: "decode", Convergence: wide}, Concurrency: 1}
	session.recordPointSample(planned, &ReportRow{OutputTokensPerSec: 10, DurationSeconds: 1})
	session.recordPointSample(planned, &ReportRow{})
	if reason := session.pointSkipReason(planned); reason != "point stopped (failed) after 1 sample(s)" {
		t.Fatalf("skip reason = %q", reason)
	}
	// A stopped point ignores later samples and failures.
	session.recordPointSample(planned, &ReportRow{OutputTokensPerSec: 10, DurationSeconds: 1})
	session.recordPointFailure(planned, context.Canceled)
	if state := session.points[pointKey(planned)]; len(state.values) != 1 || state.decision.Reason != convergence.ReasonFailed {
		t.Fatalf("state = %+v", state)
	}
}
