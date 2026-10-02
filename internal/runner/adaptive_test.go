package runner

import (
	"context"
	"database/sql"
	"path/filepath"
	"strings"
	"testing"
)

func TestLadderStopReason(t *testing.T) {
	config := AdaptiveConfig{MinThroughputGainPct: 10, TTFTP99CeilingMillis: 500}
	previous := &ReportRow{Concurrency: 4, OutputTokensPerSec: 100, TotalTokensPerSec: 1000}
	flat := &ReportRow{Concurrency: 8, OutputTokensPerSec: 105, TotalTokensPerSec: 1050, P99TTFTMillis: 100}
	if reason := ladderStopReason(config, "decode", previous, flat); !strings.Contains(reason, "throughput gained 5.0%") {
		t.Fatalf("reason = %q, want plateau stop", reason)
	}
	improving := &ReportRow{Concurrency: 8, OutputTokensPerSec: 150, P99TTFTMillis: 100}
	if reason := ladderStopReason(config, "decode", previous, improving); reason != "" {
		t.Fatalf("reason = %q, want no stop on 50%% gain", reason)
	}
	slow := &ReportRow{Concurrency: 8, OutputTokensPerSec: 200, P99TTFTMillis: 900}
	if reason := ladderStopReason(config, "decode", previous, slow); !strings.Contains(reason, "TTFT p99") {
		t.Fatalf("reason = %q, want TTFT ceiling stop", reason)
	}
	// Prefill points judge input-dominated throughput.
	prefillFlat := &ReportRow{Concurrency: 8, OutputTokensPerSec: 500, TotalTokensPerSec: 1010, P99TTFTMillis: 100}
	if reason := ladderStopReason(config, "prefill", previous, prefillFlat); !strings.Contains(reason, "throughput gained 1.0%") {
		t.Fatalf("reason = %q, want prefill plateau on total throughput", reason)
	}
	// Disabled rules never stop.
	off := AdaptiveConfig{MinThroughputGainPct: -1}
	if reason := ladderStopReason(off, "decode", previous, flat); reason != "" {
		t.Fatalf("reason = %q, want no stop with plateau rule disabled", reason)
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
