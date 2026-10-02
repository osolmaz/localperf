package report

import (
	"strings"
	"testing"

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
