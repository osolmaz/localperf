package runner

import (
	"fmt"
	"os"
	"regexp"
	"time"
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

// updateLadder evaluates stop rules after a completed point and remembers
// the highest-concurrency row per (profile, workload).
func (session *runSession) updateLadder(planned PlannedRun, row *ReportRow) {
	config := session.spec.Runner.Adaptive
	if !config.enabled() || row == nil {
		return
	}
	key := ladderKey(planned.Profile.Name, planned.Workload.Name)
	previous := session.ladderRows[key]
	if reason := ladderStopReason(config, planned.Workload.Phase, previous, row); reason != "" {
		session.stopLadder(planned, reason)
	}
	if previous == nil || row.Concurrency >= previous.Concurrency {
		session.ladderRows[key] = row
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

// ladderStopReason applies the pure stop rules: throughput plateau against
// the previous concurrency and the TTFT p99 ceiling.
func ladderStopReason(config AdaptiveConfig, phase string, previous, current *ReportRow) string {
	if config.TTFTP99CeilingMillis > 0 && current.P99TTFTMillis > config.TTFTP99CeilingMillis {
		return fmt.Sprintf("TTFT p99 %.0fms exceeded the %.0fms ceiling at concurrency %d", current.P99TTFTMillis, config.TTFTP99CeilingMillis, current.Concurrency)
	}
	if config.MinThroughputGainPct <= 0 || previous == nil || previous.Concurrency >= current.Concurrency {
		return ""
	}
	previousRate := phaseThroughput(phase, previous)
	currentRate := phaseThroughput(phase, current)
	if previousRate <= 0 {
		return ""
	}
	gain := (currentRate - previousRate) / previousRate * 100
	if gain < config.MinThroughputGainPct {
		return fmt.Sprintf("throughput gained %.1f%% (< %.0f%%) from concurrency %d to %d", gain, config.MinThroughputGainPct, previous.Concurrency, current.Concurrency)
	}
	return ""
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
