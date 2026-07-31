package vllmbench

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestValidateHTTPResult(t *testing.T) {
	dir := t.TempDir()
	good := filepath.Join(dir, "good.json")
	writeFile(t, good, `{"completed":1,"failed":0,"max_concurrency":1,"total_input_tokens":8,"total_output_tokens":4,"total_tokens":12,"output_throughput":12.5,"total_token_throughput":20}`)
	if err := validateParsedResult(good, "localperf_http", 1, 1); err != nil {
		t.Fatalf("validate good result: %v", err)
	}

	failed := filepath.Join(dir, "failed.json")
	writeFile(t, failed, `{"completed":1,"failed":2}`)
	if err := validateParsedResult(failed, "localperf_http", 1, 1); err == nil {
		t.Fatal("expected failed request result to fail validation")
	}

	empty := filepath.Join(dir, "empty.json")
	writeFile(t, empty, "")
	if err := validateParsedResult(empty, "localperf_http", 1, 1); err == nil {
		t.Fatal("expected empty result to fail validation")
	}

	if got := httpExitCode(context.Background(), nil, nil, &HTTPBenchmarkResult{Failed: 1}); got != 1 {
		t.Fatalf("exit code for failed HTTP samples = %d, want 1", got)
	}
}

func TestStructuredHTTPRequestsReadsPreparedCanonicalDataset(t *testing.T) {
	runDir := filepath.Join(t.TempDir(), "run")
	canonicalPath := filepath.Join(runDir, "datasets", "chat.canonical.jsonl")
	writeFile(t, canonicalPath, `{"id":"one","mode":"chat","messages":[{"role":"user","content":"hello"}],"max_output_tokens":8}
{"id":"two","mode":"chat","messages":[{"role":"user","content":"bye"}],"max_output_tokens":8}
`)
	planned := PlannedRun{
		ResultFile: filepath.Join(runDir, "results", "result.json"),
		Workload: Workload{
			NumPrompts: 1,
			Dataset: DatasetSpec{Prepared: DatasetMaterialization{
				CanonicalPath: filepath.Join("datasets", "chat.canonical.jsonl"),
			}},
		},
	}
	requests, err := structuredHTTPRequests(planned)
	if err != nil {
		t.Fatal(err)
	}
	if len(requests) != 1 || requests[0].ID != "one" {
		t.Fatalf("requests = %+v, want first canonical request only", requests)
	}

	planned.Workload.NumPrompts = 3
	if _, err := structuredHTTPRequests(planned); err == nil {
		t.Fatal("expected too-few prepared requests to fail")
	}

	planned.Workload.NumPrompts = 2
	planned.Workload.Dataset.Prepared.CanonicalPath = ""
	planned.Workload.BenchmarkTrafficConfig.DatasetPath = canonicalPath
	requests, err = structuredHTTPRequests(planned)
	if err != nil {
		t.Fatal(err)
	}
	if len(requests) != 2 || requests[1].ID != "two" {
		t.Fatalf("dataset_path requests = %+v, want direct canonical path", requests)
	}
}

func TestRandomHTTPRequestsUseWorkloadBackend(t *testing.T) {
	workload := Workload{
		Name:       "random",
		NumPrompts: 1,
		BenchmarkTrafficConfig: BenchmarkTrafficConfig{
			Backend:         "openai",
			DatasetName:     "random",
			RandomInputLen:  4,
			RandomOutputLen: 8,
		},
	}
	requests, err := randomHTTPRequests(workload)
	if err != nil {
		t.Fatal(err)
	}
	if requests[0].Mode != "" {
		t.Fatalf("random request mode = %q, want workload backend to decide", requests[0].Mode)
	}
	client := openAIHTTPClient{profile: Profile{Model: "model"}, workload: workload}
	body, endpoint, err := client.requestBody(requests[0])
	if err != nil {
		t.Fatal(err)
	}
	if endpoint != "/v1/completions" {
		t.Fatalf("endpoint = %q, want completions endpoint", endpoint)
	}
	if body["prompt"] == nil || body["messages"] != nil {
		t.Fatalf("body = %+v, want completion body", body)
	}
}

func TestHTTPClientSendsBearerAuth(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer secret" {
			t.Errorf("authorization header = %q, want Bearer secret", got)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"cmpl-1","choices":[{"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":4,"completion_tokens":2,"total_tokens":6}}`))
	}))
	defer server.Close()

	client := openAIHTTPClient{
		baseURL: server.URL,
		profile: Profile{
			Model: "model",
			Env:   map[string]string{"OPENAI_API_KEY": "Bearer secret"},
		},
		workload: Workload{BenchmarkTrafficConfig: BenchmarkTrafficConfig{
			Backend:  "openai-chat",
			Endpoint: "/v1/chat/completions",
		}},
		client: server.Client(),
	}
	payload, endpoint, err := client.requestPayload(CanonicalRequest{
		ID:              "one",
		Messages:        []Message{{Role: "user", Content: "hello"}},
		MaxOutputTokens: 2,
	})
	if err != nil {
		t.Fatal(err)
	}
	response, failure := client.sendRequest(context.Background(), endpoint, payload)
	if failure != nil {
		t.Fatalf("sendRequest failure = %+v", failure)
	}
	sample := response.applyToSample(newRequestSample(0, CanonicalRequest{ID: "one"}), CanonicalRequest{ID: "one"})
	if sample.Status != "completed" {
		t.Fatalf("sample = %+v, want completed", sample)
	}
}

func TestRequestEndpointFollowsRequestModeDefaults(t *testing.T) {
	client := openAIHTTPClient{
		profile: Profile{Model: "model"},
		workload: Workload{
			BenchmarkTrafficConfig: BenchmarkTrafficConfig{
				Backend:  "openai-chat",
				Endpoint: "/v1/chat/completions",
			},
		},
	}
	body, endpoint, err := client.requestBody(CanonicalRequest{
		ID:              "one",
		Mode:            "completion",
		Prompt:          "hello",
		MaxOutputTokens: 8,
	})
	if err != nil {
		t.Fatal(err)
	}
	if endpoint != "/v1/completions" {
		t.Fatalf("endpoint = %q, want mode-specific completions endpoint", endpoint)
	}
	if body["prompt"] != "hello" || body["messages"] != nil {
		t.Fatalf("body = %+v, want completion body", body)
	}

	client.workload.Endpoint = "/custom/completions"
	_, endpoint, err = client.requestBody(CanonicalRequest{
		ID:              "two",
		Mode:            "completion",
		Prompt:          "hello",
		MaxOutputTokens: 8,
	})
	if err != nil {
		t.Fatal(err)
	}
	if endpoint != "/custom/completions" {
		t.Fatalf("endpoint = %q, want custom endpoint", endpoint)
	}
}

func TestRequestBodyRejectsUnsupportedRequestMode(t *testing.T) {
	client := openAIHTTPClient{
		profile: Profile{Model: "model"},
		workload: Workload{
			BenchmarkTrafficConfig: BenchmarkTrafficConfig{Backend: "openai-chat"},
		},
	}
	_, _, err := client.requestBody(CanonicalRequest{
		ID:              "raw",
		Mode:            "raw_payload",
		Prompt:          "hello",
		MaxOutputTokens: 8,
	})
	if err == nil || !strings.Contains(err.Error(), `unsupported mode "raw_payload"`) {
		t.Fatalf("requestBody error = %v, want unsupported mode error", err)
	}
}

func TestRequestBodyRejectsUnsupportedWorkloadBackend(t *testing.T) {
	client := openAIHTTPClient{
		profile: Profile{Model: "model"},
		workload: Workload{
			Name:                   "bad-backend",
			BenchmarkTrafficConfig: BenchmarkTrafficConfig{Backend: "openai-cht"},
		},
	}
	_, _, err := client.requestBody(CanonicalRequest{
		ID:              "random",
		Prompt:          "hello",
		MaxOutputTokens: 8,
	})
	if err == nil || !strings.Contains(err.Error(), `unsupported backend "openai-cht"`) {
		t.Fatalf("requestBody error = %v, want unsupported backend error", err)
	}
}

func TestHTTPResponseRejectsMissingChoices(t *testing.T) {
	completed := time.Now().UTC()
	sample := httpLoadResponse{
		statusCode:  200,
		data:        []byte(`{"object":"list","data":[]}`),
		completedAt: completed,
	}.applyToSample(newRequestSample(0, CanonicalRequest{ID: "bad"}), CanonicalRequest{ID: "bad"})
	if sample.Status != "failed" || sample.ErrorType != "response_shape" {
		t.Fatalf("sample = %+v, want response_shape failure", sample)
	}
}

func TestHTTPResponsePreservesZeroUsageTokens(t *testing.T) {
	completed := time.Now().UTC()
	request := CanonicalRequest{ID: "zero", InputTokensExpected: 512, OutputTokensExpected: 32}
	sample := newRequestSample(0, request)
	sample.StartedAt = completed.Add(-time.Second)
	sample = httpLoadResponse{
		statusCode:  200,
		data:        []byte(`{"choices":[{"finish_reason":"stop"}],"usage":{"prompt_tokens":12,"completion_tokens":0,"total_tokens":12}}`),
		completedAt: completed,
	}.applyToSample(sample, request)
	if sample.Status != "completed" {
		t.Fatalf("sample status = %q, want completed: %+v", sample.Status, sample)
	}
	if sample.PromptTokens != 12 || sample.CompletionTokens != 0 || sample.TotalTokens != 12 {
		t.Fatalf("tokens = prompt %d completion %d total %d, want 12/0/12", sample.PromptTokens, sample.CompletionTokens, sample.TotalTokens)
	}
	if sample.OutputTokensPerSecond != 0 || sample.TotalTokensPerSecond != 12 {
		t.Fatalf("throughput = output %.3f total %.3f, want 0/12", sample.OutputTokensPerSecond, sample.TotalTokensPerSecond)
	}
	stats := statsFromSamples([]RequestSample{sample}, true, func(sample RequestSample) float64 {
		return sample.OutputTokensPerSecond
	})
	if stats.Count != 1 || stats.Mean != 0 {
		t.Fatalf("zero throughput stats = %+v, want count 1 mean 0", stats)
	}
}

func TestFeedHTTPJobsAndSleepContext(t *testing.T) {
	jobs := make(chan httpJob)
	done := make(chan []httpJob, 1)
	go func() {
		var got []httpJob
		for job := range jobs {
			got = append(got, job)
		}
		done <- got
	}()
	requests := []CanonicalRequest{{ID: "one"}, {ID: "two"}}
	if err := feedHTTPJobs(context.Background(), jobs, requests, time.Nanosecond); err != nil {
		t.Fatalf("feedHTTPJobs error = %v", err)
	}
	got := <-done
	if len(got) != 2 || got[1].request.ID != "two" {
		t.Fatalf("jobs = %+v, want two ordered jobs", got)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := sleepContext(ctx, time.Hour); !errors.Is(err, context.Canceled) {
		t.Fatalf("sleepContext error = %v, want context canceled", err)
	}

	jobs = make(chan httpJob)
	if err := feedHTTPJobs(ctx, jobs, []CanonicalRequest{{ID: "blocked"}}, 0); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled feedHTTPJobs error = %v, want context canceled", err)
	}
}

func TestScheduleHTTPRequestsRecordsUndispatchedCancellation(t *testing.T) {
	server, host, port := fakeOpenAIServer(t)
	defer server.Close()
	profile := Profile{Host: host, Port: port, Model: "model"}
	workload := Workload{
		NumPrompts: 2,
		BenchmarkTrafficConfig: BenchmarkTrafficConfig{
			Backend:     "openai-chat",
			Endpoint:    "/v1/chat/completions",
			RequestRate: "1",
		},
	}
	requests := []CanonicalRequest{
		{ID: "one", Messages: []Message{{Role: "user", Content: "hello"}}, MaxOutputTokens: 8},
		{ID: "two", Messages: []Message{{Role: "user", Content: "bye"}}, MaxOutputTokens: 8},
	}
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	samples, err := scheduleHTTPRequests(ctx, openAIHTTPClient{
		baseURL:  baseURL(profile),
		profile:  profile,
		workload: workload,
		client:   server.Client(),
	}, requests, PlannedRun{Profile: profile, Workload: workload, Concurrency: 1})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("scheduleHTTPRequests error = %v, want deadline exceeded", err)
	}
	if len(samples) != 2 {
		t.Fatalf("samples = %+v, want two samples including undispatched request", samples)
	}
	if samples[0].Status != "completed" {
		t.Fatalf("first sample status = %q, want completed", samples[0].Status)
	}
	if samples[1].Status != "failed" || samples[1].ErrorType != "deadline_exceeded" {
		t.Fatalf("second sample = %+v, want failed deadline sample", samples[1])
	}
	result := buildHTTPBenchmarkResult(PlannedRun{Workload: workload, Concurrency: 1}, samples, time.Now().Add(-time.Second), time.Now())
	if result.Completed != 1 || result.Failed != 1 {
		t.Fatalf("result completed/failed = %d/%d, want 1/1", result.Completed, result.Failed)
	}
}

func TestScheduleHTTPRequestsCountsInvalidRateAsFailed(t *testing.T) {
	requests := []CanonicalRequest{
		{ID: "one", Prompt: "hello", MaxOutputTokens: 8},
		{ID: "two", Prompt: "bye", MaxOutputTokens: 8},
	}
	planned := PlannedRun{
		Concurrency: 1,
		Workload: Workload{
			NumPrompts: 2,
			BenchmarkTrafficConfig: BenchmarkTrafficConfig{
				RequestRate: "bad",
			},
		},
	}
	samples, err := scheduleHTTPRequests(context.Background(), openAIHTTPClient{}, requests, planned)
	if err == nil {
		t.Fatal("scheduleHTTPRequests error = nil, want invalid request_rate error")
	}
	result := buildHTTPBenchmarkResult(planned, samples, time.Now().Add(-time.Second), time.Now())
	if len(samples) != 2 || result.Completed != 0 || result.Failed != 2 {
		t.Fatalf("samples=%+v completed/failed=%d/%d, want two failed samples", samples, result.Completed, result.Failed)
	}
}

func TestRequestRateDelayValues(t *testing.T) {
	cases := []struct {
		value string
		want  time.Duration
		ok    bool
	}{
		{"", 0, true},
		{"inf", 0, true},
		{"infinity", 0, true},
		{"2", 500 * time.Millisecond, true},
		{"0", 0, false},
		{"NaN", 0, false},
		{"+Inf", 0, false},
		{"bad", 0, false},
	}
	for _, tt := range cases {
		got, err := requestRateDelay(tt.value)
		if tt.ok && (err != nil || got != tt.want) {
			t.Fatalf("requestRateDelay(%q) = %s, %v; want %s, nil", tt.value, got, err, tt.want)
		}
		if !tt.ok && err == nil {
			t.Fatalf("requestRateDelay(%q) error = nil, want error", tt.value)
		}
	}
}

func TestOpenAIHTTPClientApplyRequestOptions(t *testing.T) {
	workloadTemp := 0.25
	client := openAIHTTPClient{workload: Workload{
		BenchmarkTrafficConfig: BenchmarkTrafficConfig{Backend: "openai-chat"},
		Temperature:            &workloadTemp,
		IgnoreEOS:              true,
	}}
	body := map[string]any{}
	client.applyRequestOptions(body, CanonicalRequest{})
	if body["temperature"] != workloadTemp || body["ignore_eos"] != true {
		t.Fatalf("workload options not applied: %+v", body)
	}

	requestTemp := 0.75
	body = map[string]any{}
	client.applyRequestOptions(body, CanonicalRequest{Temperature: &requestTemp})
	if body["temperature"] != requestTemp {
		t.Fatalf("request temperature should override workload temperature: %+v", body)
	}
}

func TestRequestSamplesForResultReadsSamples(t *testing.T) {
	dir := t.TempDir()
	samples, err := requestSamplesForResult(dir, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(samples) != 0 {
		t.Fatalf("empty result path samples = %+v, want none", samples)
	}

	path := filepath.Join(dir, "result.json")
	writeFile(t, path, `{
  "completed": 1,
  "request_samples": [
    {
      "request_index": 0,
      "request_id": "one",
      "status": "completed",
      "latency_ms": 10,
      "completion_tokens": 5,
      "output_tokens_per_second": 500
    }
  ]
}`)
	samples, err = requestSamplesForResult(dir, "result.json")
	if err != nil {
		t.Fatal(err)
	}
	if len(samples) != 1 || samples[0].RequestID != "one" || samples[0].OutputTokensPerSecond != 500 {
		t.Fatalf("samples = %+v, want parsed request sample", samples)
	}

	writeFile(t, filepath.Join(dir, "no-samples.json"), `{"completed":1}`)
	samples, err = requestSamplesForResult(dir, "no-samples.json")
	if err != nil {
		t.Fatal(err)
	}
	if len(samples) != 0 {
		t.Fatalf("missing request_samples parsed as %+v, want none", samples)
	}

	writeFile(t, filepath.Join(dir, "bad-samples.json"), `{"request_samples":{}}`)
	if _, err := requestSamplesForResult(dir, "bad-samples.json"); err == nil {
		t.Fatal("expected malformed request_samples to fail")
	}
}
