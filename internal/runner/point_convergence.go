package runner

import (
	"fmt"
	"time"

	"github.com/osolmaz/localperf/internal/convergence"
)

// Adaptive convergence: a point (profile, workload, concurrency) repeats
// until its convergence policy stops it, and the remaining planned samples
// of that point are skipped with the stop reason. See
// docs/2026-10-02-adaptive-convergence.md.

// EventPointStopped records the stop decision of one point.
const EventPointStopped = convergence.StopEventType

type pointState struct {
	values    []float64
	durations []time.Duration
	p99TTFTs  []float64
	decision  *convergence.Decision
}

func pointKey(planned PlannedRun) string {
	return fmt.Sprintf("%s\x00%s\x00%d", planned.Profile.Name, planned.Workload.Name, planned.Concurrency)
}

// PointMetric names the sample value of a phase: aggregate output
// throughput for decode-like phases and aggregate total throughput for
// prefill. It matches phaseThroughput and the measurements columns.
func PointMetric(phase string) string {
	if phase == "prefill" {
		return "aggregate_total_tok_s"
	}
	return "aggregate_output_tok_s"
}

func (session *runSession) point(planned PlannedRun) *pointState {
	key := pointKey(planned)
	state := session.points[key]
	if state == nil {
		state = &pointState{}
		session.points[key] = state
	}
	return state
}

// pointSkipReason reports why a planned sample is not needed, or "".
func (session *runSession) pointSkipReason(planned PlannedRun) string {
	state := session.points[pointKey(planned)]
	if state == nil || state.decision == nil {
		return ""
	}
	return fmt.Sprintf("point stopped (%s) after %d sample(s)", state.decision.Reason, state.decision.N)
}

// skipReason combines point convergence and the adaptive ladder.
func (session *runSession) skipReason(planned PlannedRun) string {
	if reason := session.pointSkipReason(planned); reason != "" {
		return reason
	}
	return session.adaptiveSkipReason(planned)
}

// recordPointSample adds one successful sample and stops the point when its
// policy says so. A sample without a positive throughput cannot be a value
// and stops the point as failed.
func (session *runSession) recordPointSample(planned PlannedRun, row *ReportRow) {
	state := session.point(planned)
	if state.decision != nil {
		return
	}
	value := 0.0
	if row != nil {
		value = phaseThroughput(planned.Workload.Phase, row)
	}
	if value <= 0 {
		session.stopPoint(planned, state, convergence.Failed(state.values), "sample has no positive "+PointMetric(planned.Workload.Phase))
		return
	}
	state.values = append(state.values, value)
	state.durations = append(state.durations, time.Duration(row.DurationSeconds*float64(time.Second)))
	state.p99TTFTs = append(state.p99TTFTs, row.P99TTFTMillis)
	decision := convergence.Evaluate(state.values, state.durations, planned.Workload.Convergence)
	if decision.Stop {
		session.stopPoint(planned, state, decision, "")
	}
}

// recordPointFailure stops the point after a failed sample. There is no
// retry; earlier successful samples stay as recorded.
func (session *runSession) recordPointFailure(planned PlannedRun, err error) {
	state := session.point(planned)
	if state.decision != nil {
		return
	}
	session.stopPoint(planned, state, convergence.Failed(state.values), err.Error())
}

func (session *runSession) stopPoint(planned PlannedRun, state *pointState, decision convergence.Decision, reasonError string) {
	state.decision = &decision
	session.events.Write(Event{
		Timestamp:   time.Now().UTC(),
		Type:        EventPointStopped,
		Profile:     planned.Profile.Name,
		Workload:    planned.Workload.Name,
		Concurrency: planned.Concurrency,
		Repeat:      planned.Repeat,
		Details:     mustJSON(convergence.NewStopRecord(PointMetric(planned.Workload.Phase), decision, state.values, state.durations, planned.Workload.Convergence, reasonError)),
	})
	if decision.Reason != convergence.ReasonFailed {
		session.updateLadder(planned, state)
	}
}
