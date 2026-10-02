package runner

import (
	"testing"
	"time"
)

func TestRequestSamplesFromResultReadsVLLMBenchArrays(t *testing.T) {
	samples, err := requestSamplesFromResultData([]byte(`{
  "date": "2026-01-02T03:04:05Z",
  "duration": 10.0,
  "completed": 2,
  "failed": 0,
  "input_lens": [100, 200],
  "output_lens": [3, 2],
  "ttfts": [0.1, 0.2],
  "itls": [[0.05, 0.07], [0.1]],
  "start_times": [10.0, 11.5]
}`))
	if err != nil {
		t.Fatal(err)
	}
	if len(samples) != 2 {
		t.Fatalf("samples = %d, want 2", len(samples))
	}
	first := samples[0]
	if first.RequestID != "vllm-bench-0" || first.Status != "completed" || !first.Streamed {
		t.Fatalf("first sample identity/status = %+v", first)
	}
	if first.PromptTokens != 100 || first.CompletionTokens != 3 || first.TotalTokens != 103 {
		t.Fatalf("first sample tokens = %+v", first)
	}
	if !near(first.TTFTMillis, 100) || !near(first.ITLMeanMillis, 60) || !near(first.TPOTMillis, 60) {
		t.Fatalf("first sample timings = ttft %.3f itl %.3f tpot %.3f, want 100/60/60", first.TTFTMillis, first.ITLMeanMillis, first.TPOTMillis)
	}
	if !near(first.LatencyMillis, 220) || !near(first.OutputTokensPerSecond, 13.636363636363637) {
		t.Fatalf("first sample latency/output = %.6f/%.6f", first.LatencyMillis, first.OutputTokensPerSecond)
	}
	if first.FirstByteAt == nil || first.CompletedAt == nil {
		t.Fatalf("first sample timestamps missing: %+v", first)
	}
	wantStart := time.Date(2026, 1, 2, 3, 3, 55, 0, time.UTC)
	if !samples[0].StartedAt.Equal(wantStart) {
		t.Fatalf("first sample start = %s, want %s", samples[0].StartedAt, wantStart)
	}
	if got := samples[1].StartedAt.Sub(samples[0].StartedAt); got != 1500*time.Millisecond {
		t.Fatalf("start delta = %s, want 1.5s", got)
	}
	if samples[0].ResponseMetadata["source"] != "vllm_bench" {
		t.Fatalf("metadata = %+v", samples[0].ResponseMetadata)
	}
}

func TestRequestSamplesFromResultUsesVLLMBenchPerRequestErrors(t *testing.T) {
	samples, err := requestSamplesFromResultData([]byte(`{
  "date": "20260102-030405",
  "completed": 2,
  "failed": 1,
  "input_lens": [100, 150, 200],
  "output_lens": [0, 0, 10],
  "ttfts": [0.0, 0.05, 0.1],
  "itls": [[], [], [0.2]],
  "errors": ["boom", "", ""]
}`))
	if err != nil {
		t.Fatal(err)
	}
	if len(samples) != 3 {
		t.Fatalf("samples = %d, want 3", len(samples))
	}
	if samples[0].Status != "failed" || samples[0].ErrorType != "vllm_bench_error" || samples[0].ErrorMessage != "boom" {
		t.Fatalf("first sample = %+v, want failed vLLM error", samples[0])
	}
	if samples[0].FirstByteAt != nil || samples[0].FirstByteMillis != 0 {
		t.Fatalf("first failed sample first byte = %v/%.3f, want absent", samples[0].FirstByteAt, samples[0].FirstByteMillis)
	}
	if samples[1].Status != "completed" || samples[1].CompletionTokens != 0 {
		t.Fatalf("second sample = %+v, want completed zero-output request", samples[1])
	}
	if samples[2].Status != "completed" || samples[2].CompletionTokens != 10 {
		t.Fatalf("third sample = %+v, want completed request", samples[2])
	}
}

func TestRequestSamplesFromResultCapsBlankVLLMErrorsByCompletedCount(t *testing.T) {
	samples, err := requestSamplesFromResultData([]byte(`{
  "date": "20260102-030405",
  "completed": 2,
  "failed": 1,
  "input_lens": [100, 150, 200],
  "output_lens": [0, 0, 10],
  "ttfts": [0.0, 0.05, 0.1],
  "itls": [[], [], [0.2]],
  "errors": ["", "", ""]
}`))
	if err != nil {
		t.Fatal(err)
	}
	if len(samples) != 3 {
		t.Fatalf("samples = %d, want 3", len(samples))
	}
	if samples[0].Status != "failed" {
		t.Fatalf("first sample status = %q, want failed", samples[0].Status)
	}
	if samples[1].Status != "completed" || samples[2].Status != "completed" {
		t.Fatalf("sample statuses = %q/%q/%q, want failed/completed/completed", samples[0].Status, samples[1].Status, samples[2].Status)
	}
}

func TestRequestSamplesFromResultParsesVLLMWallClockInLocalTime(t *testing.T) {
	originalLocal := time.Local
	time.Local = time.FixedZone("test-local", 3*60*60)
	defer func() { time.Local = originalLocal }()

	samples, err := requestSamplesFromResultData([]byte(`{
  "date": "20260102-030405",
  "duration": 5.0,
  "completed": 1,
  "failed": 0,
  "input_lens": [100],
  "output_lens": [1],
  "ttfts": [0.1],
  "itls": [[]],
  "start_times": [0.0]
}`))
	if err != nil {
		t.Fatal(err)
	}
	wantStart := time.Date(2026, 1, 2, 0, 4, 0, 0, time.UTC)
	if len(samples) != 1 || !samples[0].StartedAt.Equal(wantStart) {
		t.Fatalf("sample start = %+v, want %s", samples, wantStart)
	}
}

func near(got, want float64) bool {
	const epsilon = 0.000001
	diff := got - want
	if diff < 0 {
		diff = -diff
	}
	return diff <= epsilon
}
