package runner

import (
	"fmt"
	"os"
	"regexp"
	"time"

	"github.com/osolmaz/localperf/internal/convergence"
)

// Adaptive concurrency ladder: automates the sparse-search rule from
// docs/2026-07-02-default-inference-sweep.md. Per (profile, workload),
// concurrency runs ascending; once a stop rule fires, remaining higher
// points are skipped with the reason recorded, so a skipped cell is never a
// silent hole.

type adaptiveStop struct {
	concurrency int
	reason      string
}

func ladderKey(profile, workload string) string {
	return profile + "\x00" + workload
}

// adaptiveSkipReason reports why a planned run should be skipped, or "".
func (session *runSession) adaptiveSkipReason(planned PlannedRun) string {
	config := session.spec.Runner.Adaptive
	if !config.enabled() {
		return ""
	}
	if stop, ok := session.ladderStops[ladderKey(planned.Profile.Name, planned.Workload.Name)]; ok && planned.Concurrency > stop.concurrency {
		return stop.reason
	}
	if config.MaxConcurrencyFactor > 0 {
		if reported, ok := session.reportedMaxConcurrency[planned.Profile.Name]; ok && float64(planned.Concurrency) > config.MaxConcurrencyFactor*reported {
			return fmt.Sprintf("concurrency %d exceeds %.1fx vLLM reported max concurrency %.2f", planned.Concurrency, config.MaxConcurrencyFactor, reported)
		}
	}
	return ""
}

// ladderPoint is the summary of one completed point that the ladder rules
// compare: its concurrency, the convergence interval of its throughput, and
// the mean TTFT p99 over its samples.
type ladderPoint struct {
	concurrency int
	throughput  convergence.Interval
	p99TTFT     float64
}

// updateLadder evaluates the stop rules once a point has stopped and
// remembers the highest-concurrency point per (profile, workload).
func (session *runSession) updateLadder(planned PlannedRun, state *pointState) {
	config := session.spec.Runner.Adaptive
	if !config.enabled() || state == nil || len(state.values) == 0 {
		return
	}
	current := &ladderPoint{
		concurrency: planned.Concurrency,
		throughput:  convergence.Estimate(state.values),
		p99TTFT:     convergence.Estimate(state.p99TTFTs).Mean,
	}
	key := ladderKey(planned.Profile.Name, planned.Workload.Name)
	previous := session.ladderPoints[key]
	if reason := ladderStopReason(config, previous, current); reason != "" {
		session.stopLadder(planned, reason)
	}
	if previous == nil || current.concurrency >= previous.concurrency {
		session.ladderPoints[key] = current
	}
}

func (session *runSession) stopLadder(planned PlannedRun, reason string) {
	if !session.spec.Runner.Adaptive.enabled() {
		return
	}
	key := ladderKey(planned.Profile.Name, planned.Workload.Name)
	if _, ok := session.ladderStops[key]; ok {
		return
	}
	session.ladderStops[key] = adaptiveStop{concurrency: planned.Concurrency, reason: reason}
}

// ladderStopReason applies the pure stop rules: the TTFT p99 ceiling and a
// throughput plateau against the previous concurrency.
func ladderStopReason(config AdaptiveConfig, previous, current *ladderPoint) string {
	if config.TTFTP99CeilingMillis > 0 && current.p99TTFT > config.TTFTP99CeilingMillis {
		return fmt.Sprintf("TTFT p99 %.0fms exceeded the %.0fms ceiling at concurrency %d", current.p99TTFT, config.TTFTP99CeilingMillis, current.concurrency)
	}
	if config.MinThroughputGainPct <= 0 || previous == nil || previous.concurrency >= current.concurrency || previous.throughput.Mean <= 0 {
		return ""
	}
	return plateauReason(config.MinThroughputGainPct, previous, current)
}

// plateauReason stops the ladder when the gain is below the minimum or, when
// both points have an interval, when the current lower bound is not above
// the previous upper bound: overlapping intervals are a tie.
func plateauReason(minGainPct float64, previous, current *ladderPoint) string {
	gain := (current.throughput.Mean - previous.throughput.Mean) / previous.throughput.Mean * 100
	if gain < minGainPct {
		return fmt.Sprintf("throughput gained %.1f%% (< %.0f%%) from concurrency %d to %d", gain, minGainPct, previous.concurrency, current.concurrency)
	}
	if intervalsOverlap(previous.throughput, current.throughput) {
		return fmt.Sprintf("throughput gain %.1f%% from concurrency %d to %d is inside the 95%% intervals", gain, previous.concurrency, current.concurrency)
	}
	return ""
}

func intervalsOverlap(previous, current convergence.Interval) bool {
	return previous.Known && current.Known && current.Low() <= previous.High()
}

// phaseThroughput picks the throughput that the phase actually measures:
// prefill points are input-dominated, decode points are output-dominated.
func phaseThroughput(phase string, row *ReportRow) float64 {
	if phase == "prefill" {
		return row.TotalTokensPerSec
	}
	return row.OutputTokensPerSec
}

var vllmMaxConcurrencyPattern = regexp.MustCompile(`Maximum concurrency for [0-9,]+ tokens per request: ([0-9.]+)x`)

// recordReportedMaxConcurrency parses vLLM's reported maximum concurrency
// from the server startup log; the value feeds the max-concurrency-factor
// skip rule and is recorded as an event.
func (session *runSession) recordReportedMaxConcurrency(profile Profile, proc *serverProcess) {
	if proc == nil || proc.logPath == "" {
		return
	}
	content, err := os.ReadFile(proc.logPath)
	if err != nil {
		return
	}
	reported, ok := parseReportedMaxConcurrency(string(content))
	if !ok {
		return
	}
	session.reportedMaxConcurrency[profile.Name] = reported
	session.events.Write(Event{
		Timestamp: time.Now().UTC(),
		Type:      "vllm_reported_max_concurrency",
		Profile:   profile.Name,
		Details:   mustJSON(map[string]float64{"reported_max_concurrency": reported}),
	})
}

func parseReportedMaxConcurrency(log string) (float64, bool) {
	matches := vllmMaxConcurrencyPattern.FindAllStringSubmatch(log, -1)
	if len(matches) == 0 {
		return 0, false
	}
	var value float64
	if _, err := fmt.Sscanf(matches[len(matches)-1][1], "%f", &value); err != nil {
		return 0, false
	}
	return value, true
}
