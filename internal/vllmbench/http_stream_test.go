package vllmbench

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// sseTestServer streams a fixed chat completion: a delay before the first
// content chunk, a delay between content chunks, then finish, usage, [DONE].
func sseTestServer(t *testing.T, firstTokenDelay, interChunkDelay time.Duration, contentChunks int) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/chat/completions" {
			http.NotFound(w, r)
			return
		}
		if !requestWantsStream(r) {
			t.Error("expected a streaming request body")
			http.Error(w, "expected stream", http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		flusher := w.(http.Flusher)
		time.Sleep(firstTokenDelay)
		for i := 0; i < contentChunks; i++ {
			if i > 0 {
				time.Sleep(interChunkDelay)
			}
			_, _ = fmt.Fprintf(w, "data: {\"id\":\"stream-1\",\"choices\":[{\"delta\":{\"content\":\"tok%d \"}}]}\n\n", i)
			flusher.Flush()
		}
		_, _ = fmt.Fprint(w, "data: {\"id\":\"stream-1\",\"choices\":[{\"delta\":{},\"finish_reason\":\"stop\"}]}\n\n")
		_, _ = fmt.Fprintf(w, "data: {\"id\":\"stream-1\",\"choices\":[],\"usage\":{\"prompt_tokens\":32,\"completion_tokens\":%d,\"total_tokens\":%d}}\n\n", contentChunks, 32+contentChunks)
		_, _ = fmt.Fprint(w, "data: [DONE]\n\n")
		flusher.Flush()
	}))
}

func streamTestPlannedRun(serverURL string, stream *bool) PlannedRun {
	workload := Workload{
		Name:             "stream-test",
		ContextTarget:    40,
		ContextSemantics: ContextSemanticsActive,
		LoadGenerator:    LoadGeneratorHTTP,
		NumPrompts:       2,
		MaxConcurrency:   []int{1},
		Stream:           stream,
		BenchmarkTrafficConfig: BenchmarkTrafficConfig{
			Backend:         "openai-chat",
			DatasetName:     "random",
			RequestRate:     "inf",
			RandomInputLen:  32,
			RandomOutputLen: 8,
		},
	}
	profile := Profile{Name: "stream-test", Model: "test-model", EndpointBaseURL: serverURL}
	return PlannedRun{Profile: profile, Workload: workload, Concurrency: 1, ResultFile: "unused.json"}
}

func TestStreamingBenchmarkMeasuresTTFT(t *testing.T) {
	const firstTokenDelay = 60 * time.Millisecond
	server := sseTestServer(t, firstTokenDelay, 10*time.Millisecond, 4)
	defer server.Close()
	result, err := runHTTPBenchmark(context.Background(), streamTestPlannedRun(server.URL, nil))
	if err != nil {
		t.Fatal(err)
	}
	if result.Completed != 2 || result.Failed != 0 {
		t.Fatalf("completed/failed = %d/%d, want 2/0", result.Completed, result.Failed)
	}
	if result.TTFTSource != TTFTSourceStream {
		t.Fatalf("ttft_source = %q, want %q", result.TTFTSource, TTFTSourceStream)
	}
	if result.MeanTTFTMillis < float64(firstTokenDelay/time.Millisecond) {
		t.Fatalf("mean TTFT = %.1fms, want >= first-token delay %.0fms", result.MeanTTFTMillis, float64(firstTokenDelay/time.Millisecond))
	}
	// TTFT must be first-token time, not full-response time: the stream
	// takes ~3 inter-chunk delays after the first token.
	if result.MeanTTFTMillis >= result.MeanLatencyMillis {
		t.Fatalf("mean TTFT %.1fms >= mean latency %.1fms; TTFT looks like E2E latency", result.MeanTTFTMillis, result.MeanLatencyMillis)
	}
	if result.P50TTFTMillis <= 0 || result.P95TTFTMillis <= 0 || result.P99TTFTMillis <= 0 {
		t.Fatalf("TTFT percentiles = %.1f/%.1f/%.1f, want all positive", result.P50TTFTMillis, result.P95TTFTMillis, result.P99TTFTMillis)
	}
	for _, sample := range result.RequestSamples {
		if !sample.Streamed {
			t.Fatalf("sample %d not marked streamed", sample.RequestIndex)
		}
		if sample.TTFTMillis <= 0 || sample.TTFTMillis >= sample.LatencyMillis {
			t.Fatalf("sample TTFT %.1fms outside (0, latency %.1fms)", sample.TTFTMillis, sample.LatencyMillis)
		}
		if sample.FirstTokenAt == nil {
			t.Fatal("streamed sample is missing its observed first-token timestamp")
		}
		observedTTFT := sample.FirstTokenAt.Sub(sample.StartedAt).Seconds() * 1000
		if !near(observedTTFT, sample.TTFTMillis) {
			t.Fatalf("first-token timestamp implies %.3fms TTFT, recorded %.3fms", observedTTFT, sample.TTFTMillis)
		}
		if sample.ITLMeanMillis <= 0 {
			t.Fatalf("sample ITL = %.3f, want positive from chunk gaps", sample.ITLMeanMillis)
		}
		if sample.TPOTMillis <= 0 {
			t.Fatalf("sample TPOT = %.3f, want positive for a multi-token completion", sample.TPOTMillis)
		}
		if sample.PromptTokens != 32 || sample.CompletionTokens != 4 {
			t.Fatalf("sample tokens = %d/%d, want 32/4 from stream usage", sample.PromptTokens, sample.CompletionTokens)
		}
		if sample.ResponseMetadata["finish_reason"] != "stop" {
			t.Fatalf("finish_reason = %v, want stop", sample.ResponseMetadata["finish_reason"])
		}
	}
}

func TestNonStreamingBenchmarkRecordsNoTTFT(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if requestWantsStream(r) {
			t.Error("expected a non-streaming request body")
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprint(w, `{"id":"plain-1","choices":[{"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":32,"completion_tokens":8,"total_tokens":40}}`)
	}))
	defer server.Close()
	stream := false
	result, err := runHTTPBenchmark(context.Background(), streamTestPlannedRun(server.URL, &stream))
	if err != nil {
		t.Fatal(err)
	}
	if result.Completed != 2 {
		t.Fatalf("completed = %d, want 2", result.Completed)
	}
	if result.TTFTSource != "" {
		t.Fatalf("ttft_source = %q, want empty without streaming", result.TTFTSource)
	}
	if result.MeanTTFTMillis != 0 || result.P99TTFTMillis != 0 {
		t.Fatalf("TTFT stats = %.1f/%.1f, want zero: non-streamed runs have no TTFT", result.MeanTTFTMillis, result.P99TTFTMillis)
	}
	for _, sample := range result.RequestSamples {
		if sample.Streamed || sample.TTFTMillis != 0 {
			t.Fatalf("sample streamed/TTFT = %v/%.1f, want false/0", sample.Streamed, sample.TTFTMillis)
		}
	}
}

func TestStreamWithNeitherTokenNorFinishFailsShape(t *testing.T) {
	// A stream that ends ([DONE]) having emitted neither a token nor a
	// finish_reason is genuinely malformed and must fail. (A clean finish
	// with zero content is valid — see TestStreamingFinishWithNoContentCompletes.)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fl := w.(http.Flusher)
		fmt.Fprint(w, "data: {\"id\":\"x\",\"choices\":[{\"delta\":{}}]}\n\n")
		fmt.Fprint(w, "data: [DONE]\n\n")
		fl.Flush()
	}))
	defer server.Close()
	result, err := runHTTPBenchmark(context.Background(), streamTestPlannedRun(server.URL, nil))
	if err != nil {
		t.Fatal(err)
	}
	if result.Failed != 2 {
		t.Fatalf("failed = %d, want 2 for streams with no token and no finish reason", result.Failed)
	}
	if result.RequestSamples[0].ErrorType != "response_shape" {
		t.Fatalf("error type = %q, want response_shape", result.RequestSamples[0].ErrorType)
	}
}

func TestStreamWithoutUsageDoesNotFabricateTokens(t *testing.T) {
	// A backend that ignores stream_options.include_usage: content streams
	// but no usage chunk arrives. Completion tokens must reflect observed
	// chunks, never the requested output length (which would inflate
	// throughput). Empty-but-finished streams must land at zero.
	makeServer := func(contentChunks int) *httptest.Server {
		return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "text/event-stream")
			fl := w.(http.Flusher)
			for i := 0; i < contentChunks; i++ {
				fmt.Fprintf(w, "data: {\"id\":\"u\",\"choices\":[{\"delta\":{\"content\":\"t\"}}]}\n\n")
			}
			fmt.Fprint(w, "data: {\"id\":\"u\",\"choices\":[{\"delta\":{},\"finish_reason\":\"stop\"}]}\n\n")
			fmt.Fprint(w, "data: [DONE]\n\n")
			fl.Flush()
		}))
	}
	// request.OutputTokensExpected is 8 (RandomOutputLen); assert we never see it.
	for _, tc := range []struct{ chunks, wantTokens int }{{0, 0}, {3, 3}} {
		srv := makeServer(tc.chunks)
		result, err := runHTTPBenchmark(context.Background(), streamTestPlannedRun(srv.URL, nil))
		srv.Close()
		if err != nil {
			t.Fatalf("chunks=%d: %v", tc.chunks, err)
		}
		if result.Completed != 2 {
			t.Fatalf("chunks=%d: completed=%d, want 2", tc.chunks, result.Completed)
		}
		for _, s := range result.RequestSamples {
			if s.CompletionTokens != tc.wantTokens {
				t.Fatalf("chunks=%d: completion_tokens=%d, want %d (observed chunks, not the requested 8)", tc.chunks, s.CompletionTokens, tc.wantTokens)
			}
		}
	}
}

func TestExtraBodyCannotFlipStreaming(t *testing.T) {
	client := openAIHTTPClient{
		profile: Profile{Model: "m"},
		workload: Workload{
			BenchmarkTrafficConfig: BenchmarkTrafficConfig{
				Backend:   "openai-chat",
				ExtraBody: `{"stream": false, "stream_options": null}`,
			},
		},
	}
	body, _, err := client.requestBody(CanonicalRequest{ID: "r1", Prompt: "hi", MaxOutputTokens: 4})
	if err != nil {
		t.Fatal(err)
	}
	if body["stream"] != true {
		t.Fatalf("stream = %v, want workload streaming to stay authoritative", body["stream"])
	}
	options, ok := body["stream_options"].(map[string]any)
	if !ok || options["include_usage"] != true {
		t.Fatalf("stream_options = %v, want include_usage true", body["stream_options"])
	}
}

func TestSSEPayloadParsing(t *testing.T) {
	if payload, ok := ssePayload("data: {\"x\":1}"); !ok || payload != `{"x":1}` {
		t.Fatalf("ssePayload = %q/%v", payload, ok)
	}
	if payload, ok := ssePayload("data:[DONE]"); !ok || payload != "[DONE]" {
		t.Fatalf("ssePayload compact = %q/%v", payload, ok)
	}
	if _, ok := ssePayload(": keepalive comment"); ok {
		t.Fatal("comment line must not parse as payload")
	}
	if _, ok := ssePayload(""); ok {
		t.Fatal("blank line must not parse as payload")
	}
}

func TestApplyChunkBranches(t *testing.T) {
	stream := &httpStreamResult{}
	if failure := stream.applyChunk("{not json"); failure == nil || failure.errorType != "response_decode" {
		t.Fatalf("bad JSON failure = %+v, want response_decode", failure)
	}
	if failure := stream.applyChunk(`{"error":{"message":"x"}}`); failure == nil || failure.errorType != "api_error" {
		t.Fatalf("error chunk failure = %+v, want api_error default type", failure)
	}
	if failure := stream.applyChunk(`{"id":"a","usage":{"prompt_tokens":3},"choices":[{"delta":{"content":"hi"}}]}`); failure != nil {
		t.Fatalf("content chunk failure = %+v", failure)
	}
	if stream.responseID != "a" || stream.usage.PromptTokens == nil || *stream.usage.PromptTokens != 3 {
		t.Fatalf("stream state = %+v, want id and usage recorded", stream)
	}
	if stream.tokenChunks != 1 || stream.firstTokenAt == nil {
		t.Fatalf("token chunks = %d firstToken=%v, want first content recorded", stream.tokenChunks, stream.firstTokenAt)
	}
	first := *stream.firstTokenAt
	if failure := stream.applyChunk(`{"choices":[{"text":"more"},{"delta":{},"finish_reason":"stop"}]}`); failure != nil {
		t.Fatalf("completions text chunk failure = %+v", failure)
	}
	if stream.tokenChunks != 2 || !stream.firstTokenAt.Equal(first) {
		t.Fatalf("token chunks = %d, want completions text counted without moving first token", stream.tokenChunks)
	}
	if stream.finishReason != "stop" {
		t.Fatalf("finish reason = %q, want stop", stream.finishReason)
	}
	if got := stream.content.String(); got != "himore" {
		t.Fatalf("content = %q, want accumulated chunks", got)
	}
}

func TestStreamChoiceText(t *testing.T) {
	if got := streamChoiceText(openAIStreamChoice{Delta: &openAIMessage{Content: "d"}, Text: "t"}); got != "d" {
		t.Fatalf("delta priority = %q, want d", got)
	}
	if got := streamChoiceText(openAIStreamChoice{Text: "t"}); got != "t" {
		t.Fatalf("text fallback = %q, want t", got)
	}
	if got := streamChoiceText(openAIStreamChoice{Delta: &openAIMessage{}}); got != "" {
		t.Fatalf("empty delta = %q, want empty", got)
	}
}

func TestStampVLLMBenchTTFTSource(t *testing.T) {
	row := ReportRow{MeanTTFTMillis: 12}
	stampVLLMBenchTTFTSource(&row, Workload{LoadGenerator: LoadGeneratorVLLMBench})
	if row.TTFTSource != TTFTSourceStream {
		t.Fatalf("ttft_source = %q, want stamped for vllm bench", row.TTFTSource)
	}
	row = ReportRow{MeanTTFTMillis: 12}
	stampVLLMBenchTTFTSource(&row, Workload{LoadGenerator: LoadGeneratorHTTP})
	if row.TTFTSource != "" {
		t.Fatalf("ttft_source = %q, want empty for http generator", row.TTFTSource)
	}
	row = ReportRow{}
	stampVLLMBenchTTFTSource(&row, Workload{LoadGenerator: LoadGeneratorVLLMBench})
	if row.TTFTSource != "" {
		t.Fatalf("ttft_source = %q, want empty without TTFT data", row.TTFTSource)
	}
	row = ReportRow{MeanTTFTMillis: 12, TTFTSource: "declared"}
	stampVLLMBenchTTFTSource(&row, Workload{LoadGenerator: LoadGeneratorVLLMBench})
	if row.TTFTSource != "declared" {
		t.Fatalf("ttft_source = %q, want declared value preserved", row.TTFTSource)
	}
}

func TestTruncatedStreamFailsRequest(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		// Content arrives but the stream ends without a [DONE] terminator.
		_, _ = fmt.Fprint(w, "data: {\"id\":\"trunc-1\",\"choices\":[{\"delta\":{\"content\":\"partial\"}}]}\n\n")
	}))
	defer server.Close()
	result, err := runHTTPBenchmark(context.Background(), streamTestPlannedRun(server.URL, nil))
	if err != nil {
		t.Fatal(err)
	}
	if result.Failed != 2 || result.Completed != 0 {
		t.Fatalf("completed/failed = %d/%d, want 0/2 for truncated streams", result.Completed, result.Failed)
	}
	sample := result.RequestSamples[0]
	if sample.ErrorType != "response_read" || !strings.Contains(sample.ErrorMessage, "[DONE]") {
		t.Fatalf("error = %q/%q, want response_read truncation", sample.ErrorType, sample.ErrorMessage)
	}
}

func TestStreamErrorChunkFailsRequest(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = fmt.Fprint(w, "data: {\"error\":{\"message\":\"boom\",\"type\":\"server_error\"}}\n\n")
	}))
	defer server.Close()
	result, err := runHTTPBenchmark(context.Background(), streamTestPlannedRun(server.URL, nil))
	if err != nil {
		t.Fatal(err)
	}
	if result.Failed != 2 {
		t.Fatalf("failed = %d, want 2", result.Failed)
	}
	sample := result.RequestSamples[0]
	if sample.ErrorType != "server_error" || !strings.Contains(sample.ErrorMessage, "boom") {
		t.Fatalf("error = %q/%q, want server_error/boom", sample.ErrorType, sample.ErrorMessage)
	}
}

func TestStreamingReasoningContentCountsForTTFT(t *testing.T) {
	// A reasoning model streams its first tokens as reasoning_content, then
	// a normal content token, then finishes. TTFT must be observed from the
	// first reasoning token, not lost.
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fl := w.(http.Flusher)
		time.Sleep(20 * time.Millisecond)
		fmt.Fprint(w, "data: {\"id\":\"r1\",\"choices\":[{\"delta\":{\"reasoning_content\":\"think\"}}]}\n\n")
		fl.Flush()
		time.Sleep(5 * time.Millisecond)
		fmt.Fprint(w, "data: {\"id\":\"r1\",\"choices\":[{\"delta\":{\"content\":\"answer\"}}]}\n\n")
		fl.Flush()
		fmt.Fprint(w, "data: {\"id\":\"r1\",\"choices\":[{\"delta\":{},\"finish_reason\":\"stop\"}]}\n\n")
		fmt.Fprint(w, "data: {\"id\":\"r1\",\"choices\":[],\"usage\":{\"prompt_tokens\":32,\"completion_tokens\":2,\"total_tokens\":34}}\n\n")
		fmt.Fprint(w, "data: [DONE]\n\n")
		fl.Flush()
	}))
	defer server.Close()
	result, err := runHTTPBenchmark(context.Background(), streamTestPlannedRun(server.URL, nil))
	if err != nil {
		t.Fatal(err)
	}
	if result.Completed != 2 || result.TTFTSource != TTFTSourceStream {
		t.Fatalf("completed=%d ttft_source=%q, want 2 / stream", result.Completed, result.TTFTSource)
	}
	for _, s := range result.RequestSamples {
		if s.TTFTMillis <= 0 {
			t.Fatalf("TTFT=%.1f, want reasoning_content to seed TTFT", s.TTFTMillis)
		}
		if s.ITLMeanMillis <= 0 {
			t.Fatalf("ITL=%.3f, want gap between reasoning and content tokens", s.ITLMeanMillis)
		}
	}
}

func TestStreamingFinishWithNoContentCompletes(t *testing.T) {
	// A 1-token prefill / empty-generation stream that finishes cleanly with
	// zero streamed tokens must complete, not fail response_shape. It carries
	// no TTFT (honest), and the request still counts as completed.
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fl := w.(http.Flusher)
		fmt.Fprint(w, "data: {\"id\":\"p1\",\"choices\":[{\"delta\":{},\"finish_reason\":\"length\"}]}\n\n")
		fl.Flush()
		fmt.Fprint(w, "data: {\"id\":\"p1\",\"choices\":[],\"usage\":{\"prompt_tokens\":16000,\"completion_tokens\":1,\"total_tokens\":16001}}\n\n")
		fmt.Fprint(w, "data: [DONE]\n\n")
		fl.Flush()
	}))
	defer server.Close()
	result, err := runHTTPBenchmark(context.Background(), streamTestPlannedRun(server.URL, nil))
	if err != nil {
		t.Fatal(err)
	}
	if result.Completed != 2 || result.Failed != 0 {
		t.Fatalf("completed/failed=%d/%d, want 2/0: a clean finish with no content is valid", result.Completed, result.Failed)
	}
	for _, s := range result.RequestSamples {
		if s.Status != "completed" || !s.Streamed {
			t.Fatalf("sample status/streamed=%s/%v, want completed/true", s.Status, s.Streamed)
		}
		if s.TTFTMillis != 0 {
			t.Fatalf("TTFT=%.1f, want 0 (no token was streamed)", s.TTFTMillis)
		}
	}
	if result.TTFTSource != "" {
		t.Fatalf("ttft_source=%q, want empty when no point produced a streamed token", result.TTFTSource)
	}
}
