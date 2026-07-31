package vllmbench

import (
	"context"
	"testing"
	"time"
)

func TestFinalRunErrorAllowsPartialSweepFailures(t *testing.T) {
	err := finalRunError(RunSummary{CompletedRuns: 3, FailedRuns: 1}, nil)
	if err != nil {
		t.Fatalf("finalRunError() = %v, want nil for partial sweep failure", err)
	}
}

func TestFinalRunErrorKeepsFatalRunErrorAfterPartialProgress(t *testing.T) {
	err := finalRunError(RunSummary{CompletedRuns: 3, FailedRuns: 1}, context.Canceled)
	if err == nil {
		t.Fatal("finalRunError() = nil, want fatal run error to stay fatal")
	}
}

func TestFinalRunErrorFailsAllFailedSweep(t *testing.T) {
	err := finalRunError(RunSummary{CompletedRuns: 0, FailedRuns: 2}, nil)
	if err == nil {
		t.Fatal("finalRunError() = nil, want error when every attempted run failed")
	}
}

func TestRunStatusCompletedWhenSweepHasSomeSuccess(t *testing.T) {
	if got := runStatus(RunSummary{CompletedRuns: 1, FailedRuns: 1}); got != "completed" {
		t.Fatalf("runStatus() = %q, want completed for partial sweep failure", got)
	}
}

func TestRunStatusFailedWhenFatalRunErrorHasSomeSuccess(t *testing.T) {
	if got := runStatus(RunSummary{CompletedRuns: 1, FailedRuns: 1, Error: "context canceled"}); got != "failed" {
		t.Fatalf("runStatus() = %q, want failed for partial fatal run", got)
	}
}

func TestRunStatusFailedWhenSweepHasNoSuccess(t *testing.T) {
	if got := runStatus(RunSummary{CompletedRuns: 0, FailedRuns: 1}); got != "failed" {
		t.Fatalf("runStatus() = %q, want failed when every attempted run failed", got)
	}
}

func TestApplyRecordedRunTimesKeepsOriginalLifecycleAcrossResume(t *testing.T) {
	originalStart := time.Date(2026, 7, 31, 10, 0, 0, 0, time.UTC)
	originalFinish := originalStart.Add(10 * time.Minute)
	resumeFinish := originalStart.Add(2 * time.Hour)
	summary := applyRecordedRunTimes(RunSummary{StartedAt: resumeFinish, FinishedAt: resumeFinish}, []Event{
		{Type: "run_start", Timestamp: originalStart},
		{Type: "run_finish", Timestamp: originalFinish},
		{Type: "workload_resumed", Timestamp: resumeFinish},
		{Type: "run_finish", Timestamp: resumeFinish},
	})
	if !summary.StartedAt.Equal(originalStart) || !summary.FinishedAt.Equal(originalFinish) {
		t.Fatalf("run times = %s/%s, want %s/%s", summary.StartedAt, summary.FinishedAt, originalStart, originalFinish)
	}
}

func TestMeasurementTimesIgnoreArtifactResumeTimestamp(t *testing.T) {
	planned := PlannedRun{Profile: Profile{Name: "p"}, Workload: Workload{Name: "w"}, Concurrency: 1}
	start := time.Date(2026, 7, 31, 10, 0, 0, 0, time.UTC)
	finish := start.Add(time.Minute)
	resume := start.Add(2 * time.Hour)
	events := []Event{
		{Type: "workload_start", Profile: "p", Workload: "w", Concurrency: 1, Timestamp: start},
		{Type: "workload_finish", Profile: "p", Workload: "w", Concurrency: 1, Timestamp: finish},
		{Type: "workload_resumed", Profile: "p", Workload: "w", Concurrency: 1, Timestamp: resume},
	}
	gotStart, gotFinish := measurementTimes(events, planned)
	if gotStart == nil || gotFinish == nil || !gotStart.Equal(start) || !gotFinish.Equal(finish) {
		t.Fatalf("measurement times = %v/%v, want %s/%s", gotStart, gotFinish, start, finish)
	}
}

func TestMeasurementWallTimePrefersResultDuration(t *testing.T) {
	start := time.Date(2026, 7, 31, 10, 0, 0, 0, time.UTC)
	finish := start.Add(2 * time.Hour)
	got := measurementWallTimeMillis(ReportRow{DurationSeconds: 59.25}, &start, &finish)
	if got != 59250.0 {
		t.Fatalf("measurement wall time = %v, want 59250ms from result duration", got)
	}
}

func TestMeasurementStatusLastDecisiveEventWins(t *testing.T) {
	planned := PlannedRun{Profile: Profile{Name: "p"}, Workload: Workload{Name: "w"}, Concurrency: 1}
	event := func(kind, errText string) Event {
		return Event{Type: kind, Profile: "p", Workload: "w", Concurrency: 1, Error: errText}
	}
	cases := []struct {
		name       string
		events     []Event
		wantStatus string
		wantError  bool
	}{
		{"no events", nil, "planned", false},
		{"clean finish", []Event{event("workload_finish", "")}, "completed", false},
		{"failure", []Event{event("workload_finish", "boom"), event("workload_failed", "boom")}, "failed", true},
		{"skip", []Event{event("workload_skipped", "reason")}, "skipped", true},
		{"retry succeeds after failure", []Event{event("workload_failed", "boom"), event("workload_start", ""), event("workload_finish", "")}, "completed", false},
		{"resumed adoption completes", []Event{event("workload_failed", "boom"), event("workload_resumed", "")}, "completed", false},
		{"unrelated events ignored", []Event{{Type: "workload_failed", Profile: "other", Concurrency: 1, Error: "x"}}, "planned", false},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			if got := measurementStatus(testCase.events, planned); got != testCase.wantStatus {
				t.Fatalf("status = %q, want %q", got, testCase.wantStatus)
			}
			if gotError := measurementError(testCase.events, planned) != nil; gotError != testCase.wantError {
				t.Fatalf("error present = %t, want %t", gotError, testCase.wantError)
			}
		})
	}
}
