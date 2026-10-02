package runner

import (
	"context"
	"database/sql"
	"path/filepath"
	"strings"
	"testing"

	"github.com/osolmaz/localperf/internal/convergence"
)

func ladderAt(concurrency int, p99TTFT float64, values ...float64) *ladderPoint {
	return &ladderPoint{concurrency: concurrency, throughput: convergence.Estimate(values), p99TTFT: p99TTFT}
}

func TestLadderStopReason(t *testing.T) {
	config := AdaptiveConfig{MinThroughputGainPct: 10, TTFTP99CeilingMillis: 500}
	previous := ladderAt(4, 0, 100)
	flat := ladderAt(8, 100, 105)
	if reason := ladderStopReason(config, previous, flat); !strings.Contains(reason, "throughput gained 5.0%") {
		t.Fatalf("reason = %q, want plateau stop", reason)
	}
	improving := ladderAt(8, 100, 150)
	if reason := ladderStopReason(config, previous, improving); reason != "" {
		t.Fatalf("reason = %q, want no stop on 50%% gain", reason)
	}
	slow := ladderAt(8, 900, 200)
	if reason := ladderStopReason(config, previous, slow); !strings.Contains(reason, "TTFT p99") {
		t.Fatalf("reason = %q, want TTFT ceiling stop", reason)
	}
	// Disabled rules never stop.
	off := AdaptiveConfig{MinThroughputGainPct: -1}
	if reason := ladderStopReason(off, previous, flat); reason != "" {
		t.Fatalf("reason = %q, want no stop with plateau rule disabled", reason)
	}
}

func TestLadderStopReasonTreatsOverlappingIntervalsAsTie(t *testing.T) {
	config := AdaptiveConfig{MinThroughputGainPct: 10}
	// Means 100 and 120 (+20%), but both intervals are wide and overlap.
	previous := ladderAt(4, 0, 80, 100, 120)
	noisy := ladderAt(8, 0, 90, 120, 150)
	if reason := ladderStopReason(config, previous, noisy); !strings.Contains(reason, "inside the 95% intervals") {
		t.Fatalf("reason = %q, want interval tie stop", reason)
	}
	// The same +20% with tight intervals is a real gain.
	tightPrevious := ladderAt(4, 0, 99, 100, 101)
	tight := ladderAt(8, 0, 119, 120, 121)
	if reason := ladderStopReason(config, tightPrevious, tight); reason != "" {
		t.Fatalf("reason = %q, want no stop on a separated gain", reason)
	}
	// One sample has no interval, so the rule compares means only.
	if reason := ladderStopReason(config, ladderAt(4, 0, 100), noisy); reason != "" {
		t.Fatalf("reason = %q, want mean comparison without a previous interval", reason)
	}
}

func TestPhaseThroughputSelectsTheMeasuredRate(t *testing.T) {
	row := &ReportRow{OutputTokensPerSec: 500, TotalTokensPerSec: 1010}
	if got := phaseThroughput("prefill", row); got != 1010 {
		t.Fatalf("prefill throughput = %v, want total", got)
	}
	if got := phaseThroughput("decode", row); got != 500 {
		t.Fatalf("decode throughput = %v, want output", got)
	}
	if PointMetric("prefill") != "aggregate_total_tok_s" || PointMetric("decode") != "aggregate_output_tok_s" {
		t.Fatalf("point metrics = %q, %q", PointMetric("prefill"), PointMetric("decode"))
	}
}

func TestParseReportedMaxConcurrency(t *testing.T) {
	log := `INFO 07-05 [core.py] GPU KV cache size: 123 tokens
INFO 07-05 [core.py] Maximum concurrency for 8,192 tokens per request: 3.85x
INFO 07-05 [core.py] Maximum concurrency for 8,192 tokens per request: 4.10x`
	value, ok := parseReportedMaxConcurrency(log)
	if !ok || value != 4.10 {
		t.Fatalf("parsed = %f ok=%t, want last match 4.10", value, ok)
	}
	if _, ok := parseReportedMaxConcurrency("no such line"); ok {
		t.Fatal("parsed reported concurrency from unrelated log")
	}
}

// TestAdaptiveLadderSkipsAfterTTFTCeiling runs a live ladder where c1 trips
// the (deliberately tiny) TTFT ceiling, so every higher point is skipped
// with the reason recorded in the artifact.
func TestAdaptiveLadderSkipsAfterTTFTCeiling(t *testing.T) {
	server, host, port := fakeOpenAIServer(t)
	defer server.Close()
	spec := httpTestSpec(t, host, port, "adaptive-live", 3, 1)
	spec.Workloads[0].MaxConcurrency = []int{1, 2, 3}
	ceiling := 0.001
	spec.Runner.Adaptive = AdaptiveConfig{TTFTP99CeilingMillis: ceiling}
	ApplyDefaults(&spec)
	runDir := filepath.Join(spec.OutputDir, "adaptive-live")
	artifactPath := filepath.Join(spec.OutputDir, "adaptive-live.sqlite")
	summary, err := Execute(context.Background(), spec, RunOptions{RunDir: runDir, ArtifactPath: artifactPath})
	if err != nil {
		t.Fatal(err)
	}
	if summary.CompletedRuns != 1 || summary.SkippedRuns != 2 || summary.FailedRuns != 0 {
		t.Fatalf("summary = completed %d skipped %d failed %d, want 1/2/0", summary.CompletedRuns, summary.SkippedRuns, summary.FailedRuns)
	}
	db, err := sql.Open("sqlite", artifactPath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var skipped int
	var reason string
	if err := db.QueryRow(`SELECT COUNT(*) FROM measurements WHERE status = 'skipped'`).Scan(&skipped); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow(`SELECT COALESCE(error_message, '') FROM measurements WHERE status = 'skipped' LIMIT 1`).Scan(&reason); err != nil {
		t.Fatal(err)
	}
	if skipped != 2 || !strings.Contains(reason, "TTFT p99") {
		t.Fatalf("skipped = %d reason = %q, want 2 skipped with TTFT ceiling reason", skipped, reason)
	}
}
