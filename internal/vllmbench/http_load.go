package vllmbench

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/http/httptrace"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

type HTTPBenchmarkResult struct {
	Date                          string          `json:"date"`
	LoadGenerator                 string          `json:"load_generator"`
	EndpointType                  string          `json:"endpoint_type"`
	Backend                       string          `json:"backend"`
	ModelID                       string          `json:"model_id"`
	TokenizerID                   string          `json:"tokenizer_id,omitempty"`
	NumPrompts                    int             `json:"num_prompts"`
	RequestRate                   string          `json:"request_rate"`
	MaxConcurrency                int             `json:"max_concurrency"`
	Duration                      float64         `json:"duration"`
	Completed                     int             `json:"completed"`
	Failed                        int             `json:"failed"`
	TotalInputTokens              int             `json:"total_input_tokens"`
	TotalOutputTokens             int             `json:"total_output_tokens"`
	TotalTokens                   int             `json:"total_tokens"`
	RequestThroughput             float64         `json:"request_throughput"`
	OutputThroughput              float64         `json:"output_throughput"`
	TotalTokenThroughput          float64         `json:"total_token_throughput"`
	RequestOutputThroughputMean   float64         `json:"request_output_throughput_mean,omitempty"`
	RequestOutputThroughputStdDev float64         `json:"request_output_throughput_stddev,omitempty"`
	RequestOutputThroughputMin    float64         `json:"request_output_throughput_min,omitempty"`
	RequestOutputThroughputP50    float64         `json:"request_output_throughput_p50,omitempty"`
	RequestOutputThroughputP95    float64         `json:"request_output_throughput_p95,omitempty"`
	RequestOutputThroughputP99    float64         `json:"request_output_throughput_p99,omitempty"`
	RequestOutputThroughputMax    float64         `json:"request_output_throughput_max,omitempty"`
	RequestTotalThroughputMean    float64         `json:"request_total_throughput_mean,omitempty"`
	RequestTotalThroughputStdDev  float64         `json:"request_total_throughput_stddev,omitempty"`
	MeanTTFTMillis                float64         `json:"mean_ttft_ms,omitempty"`
	P50TTFTMillis                 float64         `json:"p50_ttft_ms,omitempty"`
	P95TTFTMillis                 float64         `json:"p95_ttft_ms,omitempty"`
	P99TTFTMillis                 float64         `json:"p99_ttft_ms,omitempty"`
	TTFTSource                    string          `json:"ttft_source,omitempty"`
	MeanLatencyMillis             float64         `json:"mean_latency_ms,omitempty"`
	StdLatencyMillis              float64         `json:"std_latency_ms,omitempty"`
	P50LatencyMillis              float64         `json:"p50_latency_ms,omitempty"`
	P95LatencyMillis              float64         `json:"p95_latency_ms,omitempty"`
	P99LatencyMillis              float64         `json:"p99_latency_ms,omitempty"`
	RequestSamples                []RequestSample `json:"request_samples,omitempty"`
}

type RequestSample struct {
	RequestIndex          int            `json:"request_index"`
	RequestID             string         `json:"request_id,omitempty"`
	Status                string         `json:"status"`
	Streamed              bool           `json:"streamed"`
	HTTPStatusCode        int            `json:"http_status_code,omitempty"`
	StartedAt             time.Time      `json:"started_at"`
	FirstByteAt           *time.Time     `json:"first_byte_at,omitempty"`
	CompletedAt           *time.Time     `json:"completed_at,omitempty"`
	LatencyMillis         float64        `json:"latency_ms,omitempty"`
	FirstByteMillis       float64        `json:"first_byte_ms,omitempty"`
	TTFTMillis            float64        `json:"ttft_ms,omitempty"`
	TPOTMillis            float64        `json:"tpot_ms,omitempty"`
	ITLMeanMillis         float64        `json:"itl_mean_ms,omitempty"`
	PromptTokens          int            `json:"prompt_tokens,omitempty"`
	CompletionTokens      int            `json:"completion_tokens,omitempty"`
	TotalTokens           int            `json:"total_tokens,omitempty"`
	OutputTokensPerSecond float64        `json:"output_tokens_per_second,omitempty"`
	TotalTokensPerSecond  float64        `json:"total_tokens_per_second,omitempty"`
	PromptSHA256          string         `json:"prompt_sha256,omitempty"`
	ResponseSHA256        string         `json:"response_sha256,omitempty"`
	ErrorType             string         `json:"error_type,omitempty"`
	ErrorCode             string         `json:"error_code,omitempty"`
	ErrorMessage          string         `json:"error_message,omitempty"`
	ResponseMetadata      map[string]any `json:"response_metadata,omitempty"`
}

type httpJob struct {
	index   int
	request CanonicalRequest
}

type openAIHTTPClient struct {
	baseURL  string
	profile  Profile
	workload Workload
	client   *http.Client
}

type openAIResponse struct {
	ID      string         `json:"id,omitempty"`
	Choices []openAIChoice `json:"choices,omitempty"`
	Usage   openAIUsage    `json:"usage,omitempty"`
	Error   *openAIError   `json:"error,omitempty"`
}

type openAIChoice struct {
	Message      *openAIMessage `json:"message,omitempty"`
	Text         string         `json:"text,omitempty"`
	FinishReason string         `json:"finish_reason,omitempty"`
}

type openAIMessage struct {
	Role             string `json:"role,omitempty"`
	Content          string `json:"content,omitempty"`
	ReasoningContent string `json:"reasoning_content,omitempty"`
}

type openAIUsage struct {
	PromptTokens     *int `json:"prompt_tokens,omitempty"`
	CompletionTokens *int `json:"completion_tokens,omitempty"`
	TotalTokens      *int `json:"total_tokens,omitempty"`
}

type openAIError struct {
	Message string `json:"message,omitempty"`
	Type    string `json:"type,omitempty"`
	Code    any    `json:"code,omitempty"`
}

type numericStats struct {
	Count  int
	Mean   float64
	StdDev float64
	Min    float64
	P50    float64
	P90    float64
	P95    float64
	P99    float64
	Max    float64
}

func executeHTTPBench(ctx context.Context, spec Spec, planned PlannedRun, logPath string) (commandResult, error) {
	if err := prepareCommandPaths(httpCommand(spec, planned), logPath); err != nil {
		return commandResult{ExitCode: -1}, err
	}
	runCtx, cancel := context.WithTimeout(ctx, time.Duration(spec.Safety.WorkloadTimeoutSec)*time.Second)
	defer cancel()
	if err := preflightHTTPMemory(spec, logPath); err != nil {
		return commandResult{ExitCode: 1}, err
	}
	memoryMonitor := monitorMemoryFloor(runCtx, cancel, spec.Safety.MinMemAvailableGiB, time.Duration(spec.Safety.PollIntervalMillis)*time.Millisecond)
	start := time.Now()
	result, runErr := runHTTPBenchmark(runCtx, planned)
	duration := time.Since(start)
	memoryErr := stopHTTPMemoryMonitor(cancel, memoryMonitor)
	resultWritten, runErr := persistHTTPResult(planned.ResultFile, logPath, result, duration, runErr, memoryErr)
	commandResult := commandResult{Duration: duration, ExitCode: httpExitCode(runCtx, runErr, memoryErr, result), ResultWritten: resultWritten}
	return commandResult, httpRunError(runCtx, spec, runErr, memoryErr, result)
}

func preflightHTTPMemory(spec Spec, logPath string) error {
	if _, err := checkMemoryFloor(spec.Safety.MinMemAvailableGiB); err != nil {
		_ = writeHTTPLog(logPath, nil, 0, nil, err)
		return err
	}
	return nil
}

func stopHTTPMemoryMonitor(cancel context.CancelFunc, memoryMonitor <-chan error) error {
	cancel()
	return <-memoryMonitor
}

func persistHTTPResult(resultFile, logPath string, result *HTTPBenchmarkResult, duration time.Duration, runErr, memoryErr error) (bool, error) {
	resultWritten, runErr := writeHTTPResultFile(resultFile, result, runErr)
	if err := writeHTTPLog(logPath, result, duration, runErr, memoryErr); err != nil && runErr == nil {
		return resultWritten, err
	}
	return resultWritten, runErr
}

func writeHTTPResultFile(resultFile string, result *HTTPBenchmarkResult, runErr error) (bool, error) {
	if result == nil {
		return false, runErr
	}
	if err := writeJSONFile(resultFile, result); err != nil {
		if runErr == nil {
			return false, err
		}
		return false, runErr
	}
	return true, runErr
}

func httpExitCode(runCtx context.Context, runErr, memoryErr error, result *HTTPBenchmarkResult) int {
	if runErr != nil || memoryErr != nil || runCtx.Err() == context.DeadlineExceeded || hasFailedHTTPRequests(result) {
		return 1
	}
	return 0
}

func hasFailedHTTPRequests(result *HTTPBenchmarkResult) bool {
	return result != nil && result.Failed > 0
}

func httpRunError(runCtx context.Context, spec Spec, runErr, memoryErr error, result *HTTPBenchmarkResult) error {
	if runCtx.Err() == context.DeadlineExceeded {
		return fmt.Errorf("localperf_http benchmark timed out after %s", time.Duration(spec.Safety.WorkloadTimeoutSec)*time.Second)
	}
	if memoryErr != nil {
		return memoryErr
	}
	if runErr != nil {
		return runErr
	}
	if hasFailedHTTPRequests(result) {
		return fmt.Errorf("localperf_http result reported %d failed request(s)", result.Failed)
	}
	return nil
}

func runHTTPBenchmark(ctx context.Context, planned PlannedRun) (*HTTPBenchmarkResult, error) {
	requests, err := plannedRunHTTPRequests(ctx, planned)
	if err != nil {
		return nil, err
	}
	client := openAIHTTPClient{
		baseURL:  baseURL(planned.Profile),
		profile:  planned.Profile,
		workload: planned.Workload,
		client:   &http.Client{},
	}
	start := time.Now().UTC()
	samples, err := scheduleHTTPRequests(ctx, client, requests, planned)
	end := time.Now().UTC()
	result := buildHTTPBenchmarkResult(planned, samples, start, end)
	return result, err
}

func plannedRunHTTPRequests(ctx context.Context, planned PlannedRun) ([]CanonicalRequest, error) {
	if hasHTTPDatasetPath(planned.Workload) || hasStructuredDataset(planned.Workload) {
		return structuredHTTPRequests(planned)
	}
	if planned.Workload.DatasetName == "random" {
		return randomHTTPRequests(planned.Workload)
	}
	return nil, fmt.Errorf("localperf_http supports random and structured datasets, not dataset_name %q", planned.Workload.DatasetName)
}

func hasHTTPDatasetPath(workload Workload) bool {
	return httpDatasetPath(workload) != ""
}

func httpDatasetPath(workload Workload) string {
	if path := strings.TrimSpace(workload.Dataset.Prepared.CanonicalPath); path != "" {
		return path
	}
	return strings.TrimSpace(workload.DatasetPath)
}

func structuredHTTPRequests(planned PlannedRun) ([]CanonicalRequest, error) {
	path := resolveResultPath(filepath.Dir(filepath.Dir(planned.ResultFile)), httpDatasetPath(planned.Workload))
	requests, err := readCanonicalRequestFile(path)
	if err != nil {
		return nil, err
	}
	if len(requests) < planned.Workload.NumPrompts {
		return nil, fmt.Errorf("prepared dataset has %d request(s), workload needs %d", len(requests), planned.Workload.NumPrompts)
	}
	return requests[:planned.Workload.NumPrompts], nil
}

func randomHTTPRequests(workload Workload) ([]CanonicalRequest, error) {
	if workload.NumPrompts <= 0 {
		return nil, errors.New("num_prompts must be positive")
	}
	if workload.RandomOutputLen <= 0 {
		return nil, errors.New("random_output_len must be positive")
	}
	requests := make([]CanonicalRequest, 0, workload.NumPrompts)
	for i := 0; i < workload.NumPrompts; i++ {
		id := fmt.Sprintf("%s-%06d", datasetIDForWorkload(workload.Name), i+1)
		requests = append(requests, CanonicalRequest{
			ID:                   id,
			Ordinal:              i,
			DatasetID:            datasetIDForWorkload(workload.Name),
			SourceRecordID:       id,
			Messages:             []Message{{Role: "user", Content: syntheticPrompt(workload.RandomInputLen)}},
			MaxOutputTokens:      workload.RandomOutputLen,
			Temperature:          workload.Temperature,
			IgnoreEOS:            workload.IgnoreEOS,
			InputTokensExpected:  workload.RandomInputLen,
			OutputTokensExpected: workload.RandomOutputLen,
		})
	}
	return requests, nil
}

func scheduleHTTPRequests(ctx context.Context, client openAIHTTPClient, requests []CanonicalRequest, planned PlannedRun) ([]RequestSample, error) {
	concurrency := planned.Concurrency
	if concurrency <= 0 {
		concurrency = 1
	}
	delay, err := requestRateDelay(planned.Workload.RequestRate)
	if err != nil {
		return addUndispatchedHTTPSamples(nil, requests, err, time.Now().UTC()), err
	}
	jobs := make(chan httpJob)
	results := make(chan RequestSample, len(requests))
	var workers sync.WaitGroup
	for i := 0; i < concurrency; i++ {
		workers.Add(1)
		go func() {
			defer workers.Done()
			for job := range jobs {
				results <- client.Invoke(ctx, job.index, job.request)
			}
		}()
	}
	feedErr := feedHTTPJobs(ctx, jobs, requests, delay)
	workers.Wait()
	close(results)
	samples := collectHTTPSamples(results)
	if feedErr != nil {
		return addUndispatchedHTTPSamples(samples, requests, feedErr, time.Now().UTC()), feedErr
	}
	if err := ctx.Err(); err != nil {
		return addUndispatchedHTTPSamples(samples, requests, err, time.Now().UTC()), err
	}
	return samples, nil
}

func feedHTTPJobs(ctx context.Context, jobs chan<- httpJob, requests []CanonicalRequest, delay time.Duration) error {
	defer close(jobs)
	for i, request := range requests {
		if i > 0 && delay > 0 {
			if err := sleepContext(ctx, delay); err != nil {
				return err
			}
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case jobs <- httpJob{index: i, request: request}:
		}
	}
	return nil
}

func collectHTTPSamples(results <-chan RequestSample) []RequestSample {
	var samples []RequestSample
	for sample := range results {
		samples = append(samples, sample)
	}
	sort.Slice(samples, func(i, j int) bool {
		return samples[i].RequestIndex < samples[j].RequestIndex
	})
	return samples
}

func addUndispatchedHTTPSamples(samples []RequestSample, requests []CanonicalRequest, cause error, completed time.Time) []RequestSample {
	if len(samples) >= len(requests) {
		return samples
	}
	seen := make([]bool, len(requests))
	for _, sample := range samples {
		if sample.RequestIndex >= 0 && sample.RequestIndex < len(seen) {
			seen[sample.RequestIndex] = true
		}
	}
	for i, request := range requests {
		if seen[i] {
			continue
		}
		samples = append(samples, undispatchedHTTPSample(i, request, cause, completed))
	}
	sort.Slice(samples, func(i, j int) bool {
		return samples[i].RequestIndex < samples[j].RequestIndex
	})
	return samples
}

func undispatchedHTTPSample(index int, request CanonicalRequest, cause error, completed time.Time) RequestSample {
	return RequestSample{
		RequestIndex: index,
		RequestID:    request.ID,
		Status:       "failed",
		Streamed:     false,
		StartedAt:    completed,
		CompletedAt:  &completed,
		PromptSHA256: sha256Maybe(requestPromptText(request)),
		ErrorType:    contextErrorType(cause),
		ErrorMessage: cause.Error(),
	}
}

func contextErrorType(err error) string {
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		return "deadline_exceeded"
	case errors.Is(err, context.Canceled):
		return "context_canceled"
	default:
		return "canceled"
	}
}

func requestRateDelay(value string) (time.Duration, error) {
	value = strings.TrimSpace(strings.ToLower(value))
	if value == "" || value == "inf" || value == "infinity" {
		return 0, nil
	}
	rate, err := strconv.ParseFloat(value, 64)
	if err != nil || rate <= 0 || math.IsNaN(rate) || math.IsInf(rate, 0) {
		return 0, fmt.Errorf("unsupported request_rate %q", value)
	}
	return time.Duration(float64(time.Second) / rate), nil
}

func sleepContext(ctx context.Context, delay time.Duration) error {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func (client openAIHTTPClient) Invoke(ctx context.Context, index int, request CanonicalRequest) RequestSample {
	sample := newRequestSample(index, request)
	payload, endpoint, err := client.requestPayload(request)
	if err != nil {
		return sample.withError("request_render", "", err.Error(), time.Now().UTC(), nil)
	}
	if client.streams() {
		stream, failure := client.sendStreamingRequest(ctx, endpoint, payload)
		if failure != nil {
			return sample.withError(failure.errorType, failure.errorCode, failure.message, failure.completedAt, failure.firstByteAt)
		}
		sample.HTTPStatusCode = stream.statusCode
		return stream.applyToSample(sample, request)
	}
	response, failure := client.sendRequest(ctx, endpoint, payload)
	if failure != nil {
		return sample.withError(failure.errorType, failure.errorCode, failure.message, failure.completedAt, failure.firstByteAt)
	}
	sample.HTTPStatusCode = response.statusCode
	return response.applyToSample(sample, request)
}

func (client openAIHTTPClient) streams() bool {
	return workloadStreams(client.workload)
}

func newRequestSample(index int, request CanonicalRequest) RequestSample {
	return RequestSample{
		RequestIndex: index,
		RequestID:    request.ID,
		Status:       "failed",
		Streamed:     false,
		StartedAt:    time.Now().UTC(),
		PromptSHA256: sha256Maybe(requestPromptText(request)),
	}
}

func (client openAIHTTPClient) requestPayload(request CanonicalRequest) ([]byte, string, error) {
	body, endpoint, err := client.requestBody(request)
	if err != nil {
		return nil, "", err
	}
	payload, err := json.Marshal(body)
	if err != nil {
		return nil, "", err
	}
	return payload, endpoint, nil
}

type httpLoadResponse struct {
	statusCode  int
	data        []byte
	completedAt time.Time
	firstByteAt *time.Time
}

type httpLoadFailure struct {
	errorType   string
	errorCode   string
	message     string
	completedAt time.Time
	firstByteAt *time.Time
}

func (client openAIHTTPClient) sendRequest(ctx context.Context, endpoint string, payload []byte) (httpLoadResponse, *httpLoadFailure) {
	var firstByteAt *time.Time
	trace := &httptrace.ClientTrace{GotFirstResponseByte: func() {
		now := time.Now().UTC()
		firstByteAt = &now
	}}
	req, err := http.NewRequestWithContext(httptrace.WithClientTrace(ctx, trace), http.MethodPost, client.baseURL+endpoint, bytes.NewReader(payload))
	if err != nil {
		return httpLoadResponse{}, newHTTPLoadFailure("request_create", "", err.Error(), time.Now().UTC(), firstByteAt)
	}
	req.Header.Set("Content-Type", "application/json")
	client.applyAuthHeader(req)
	resp, err := client.client.Do(req)
	if err != nil {
		return httpLoadResponse{}, newHTTPLoadFailure("request_send", "", err.Error(), time.Now().UTC(), firstByteAt)
	}
	defer resp.Body.Close()
	data, readErr := io.ReadAll(io.LimitReader(resp.Body, 256*1024*1024))
	completed := time.Now().UTC()
	if readErr != nil {
		return httpLoadResponse{}, newHTTPLoadFailure("response_read", "", readErr.Error(), completed, firstByteAt)
	}
	return httpLoadResponse{statusCode: resp.StatusCode, data: data, completedAt: completed, firstByteAt: firstByteAt}, nil
}

func newHTTPLoadFailure(errorType, errorCode, message string, completedAt time.Time, firstByteAt *time.Time) *httpLoadFailure {
	return &httpLoadFailure{
		errorType:   errorType,
		errorCode:   errorCode,
		message:     message,
		completedAt: completedAt,
		firstByteAt: firstByteAt,
	}
}

type httpStreamResult struct {
	statusCode   int
	firstTokenAt *time.Time
	lastTokenAt  *time.Time
	tokenChunks  int
	content      strings.Builder
	usage        openAIUsage
	responseID   string
	finishReason string
	completedAt  time.Time
	firstByteAt  *time.Time
}

type openAIStreamChunk struct {
	ID      string               `json:"id,omitempty"`
	Choices []openAIStreamChoice `json:"choices,omitempty"`
	Usage   *openAIUsage         `json:"usage,omitempty"`
	Error   *openAIError         `json:"error,omitempty"`
}

type openAIStreamChoice struct {
	Delta        *openAIMessage `json:"delta,omitempty"`
	Text         string         `json:"text,omitempty"`
	FinishReason string         `json:"finish_reason,omitempty"`
}

func (client openAIHTTPClient) sendStreamingRequest(ctx context.Context, endpoint string, payload []byte) (*httpStreamResult, *httpLoadFailure) {
	var firstByteAt *time.Time
	trace := &httptrace.ClientTrace{GotFirstResponseByte: func() {
		now := time.Now().UTC()
		firstByteAt = &now
	}}
	req, err := http.NewRequestWithContext(httptrace.WithClientTrace(ctx, trace), http.MethodPost, client.baseURL+endpoint, bytes.NewReader(payload))
	if err != nil {
		return nil, newHTTPLoadFailure("request_create", "", err.Error(), time.Now().UTC(), firstByteAt)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "text/event-stream")
	client.applyAuthHeader(req)
	resp, err := client.client.Do(req)
	if err != nil {
		return nil, newHTTPLoadFailure("request_send", "", err.Error(), time.Now().UTC(), firstByteAt)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		data, _ := io.ReadAll(io.LimitReader(resp.Body, 1024*1024))
		return nil, newHTTPLoadFailure("http_status", fmt.Sprint(resp.StatusCode), strings.TrimSpace(string(data)), time.Now().UTC(), firstByteAt)
	}
	stream := &httpStreamResult{statusCode: resp.StatusCode, firstByteAt: firstByteAt}
	if failure := stream.consume(resp.Body); failure != nil {
		failure.firstByteAt = firstByteAt
		return nil, failure
	}
	stream.firstByteAt = firstByteAt
	return stream, nil
}

func (client openAIHTTPClient) applyAuthHeader(req *http.Request) {
	token := client.bearerToken()
	if token == "" {
		return
	}
	req.Header.Set("Authorization", "Bearer "+token)
}

func (client openAIHTTPClient) bearerToken() string {
	for _, value := range []string{
		client.profile.Env["OPENAI_API_KEY"],
		client.profile.Env["HF_TOKEN"],
		os.Getenv("OPENAI_API_KEY"),
		os.Getenv("HF_TOKEN"),
	} {
		if token := normalizeBearerToken(value); token != "" {
			return token
		}
	}
	return ""
}

func normalizeBearerToken(value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return ""
	}
	if token, ok := strings.CutPrefix(value, "Bearer "); ok {
		return strings.TrimSpace(token)
	}
	if token, ok := strings.CutPrefix(value, "bearer "); ok {
		return strings.TrimSpace(token)
	}
	return value
}

func (stream *httpStreamResult) consume(body io.Reader) *httpLoadFailure {
	scanner := bufio.NewScanner(body)
	scanner.Buffer(make([]byte, 0, 64*1024), 16*1024*1024)
	terminated := false
	for scanner.Scan() {
		data, ok := ssePayload(scanner.Text())
		if !ok {
			continue
		}
		if data == "[DONE]" {
			terminated = true
			break
		}
		if failure := stream.applyChunk(data); failure != nil {
			return failure
		}
	}
	stream.completedAt = time.Now().UTC()
	return stream.finalize(terminated, scanner.Err())
}

// finalize validates a fully-read stream. EOF before [DONE] is a truncated
// stream (recording it would keep partial output). A stream that finished
// cleanly is valid even with zero streamed tokens — a 1-token prefill point
// or a reasoning model that emitted only a final message; only a stream with
// neither any token nor a finish reason is malformed.
func (stream *httpStreamResult) finalize(terminated bool, scanErr error) *httpLoadFailure {
	switch {
	case scanErr != nil:
		return newHTTPLoadFailure("response_read", "", scanErr.Error(), stream.completedAt, nil)
	case !terminated:
		return newHTTPLoadFailure("response_read", "", "stream ended before [DONE] terminator", stream.completedAt, nil)
	case stream.firstTokenAt == nil && stream.finishReason == "":
		return newHTTPLoadFailure("response_shape", "", "stream produced neither tokens nor a finish reason", stream.completedAt, nil)
	default:
		return nil
	}
}

func ssePayload(line string) (string, bool) {
	line = strings.TrimSpace(line)
	if !strings.HasPrefix(line, "data:") {
		return "", false
	}
	return strings.TrimSpace(strings.TrimPrefix(line, "data:")), true
}

func (stream *httpStreamResult) applyChunk(data string) *httpLoadFailure {
	var chunk openAIStreamChunk
	if err := json.Unmarshal([]byte(data), &chunk); err != nil {
		return newHTTPLoadFailure("response_decode", "", err.Error(), time.Now().UTC(), nil)
	}
	if chunk.Error != nil {
		return newHTTPLoadFailure(firstNonEmpty(chunk.Error.Type, "api_error"), fmt.Sprint(chunk.Error.Code), chunk.Error.Message, time.Now().UTC(), nil)
	}
	if chunk.ID != "" {
		stream.responseID = chunk.ID
	}
	if chunk.Usage != nil {
		stream.usage = *chunk.Usage
	}
	for _, choice := range chunk.Choices {
		stream.applyChoice(choice)
	}
	return nil
}

func (stream *httpStreamResult) applyChoice(choice openAIStreamChoice) {
	if choice.FinishReason != "" {
		stream.finishReason = choice.FinishReason
	}
	text := streamChoiceText(choice)
	if text == "" {
		return
	}
	now := time.Now().UTC()
	if stream.firstTokenAt == nil {
		stream.firstTokenAt = &now
	}
	stream.lastTokenAt = &now
	stream.tokenChunks++
	stream.content.WriteString(text)
}

// streamChoiceText extracts the token text of one stream choice. Chat chunks
// carry a delta message; reasoning models stream their first tokens as
// reasoning_content before any content, so both count for TTFT/ITL timing.
// Completions chunks carry plain text.
func streamChoiceText(choice openAIStreamChoice) string {
	if choice.Delta != nil {
		if choice.Delta.Content != "" {
			return choice.Delta.Content
		}
		if choice.Delta.ReasoningContent != "" {
			return choice.Delta.ReasoningContent
		}
	}
	return choice.Text
}

func (stream *httpStreamResult) applyToSample(sample RequestSample, request CanonicalRequest) RequestSample {
	sample.Status = "completed"
	sample.Streamed = true
	sample.CompletedAt = &stream.completedAt
	sample.FirstByteAt = stream.firstByteAt
	sample.LatencyMillis = stream.completedAt.Sub(sample.StartedAt).Seconds() * 1000
	if stream.firstByteAt != nil {
		sample.FirstByteMillis = stream.firstByteAt.Sub(sample.StartedAt).Seconds() * 1000
	}
	sample.PromptTokens = usageInt(stream.usage.PromptTokens, request.InputTokensExpected)
	// Completion tokens fall back to the observed streamed-chunk count, never
	// to the requested output length: a backend that omits usage on an empty
	// stream must not be credited phantom output tokens.
	sample.CompletionTokens = usageInt(stream.usage.CompletionTokens, stream.tokenChunks)
	sample.TotalTokens = usageInt(stream.usage.TotalTokens, sample.PromptTokens+sample.CompletionTokens)
	// firstTokenAt is nil only for a clean finish that streamed no token
	// (accepted above); such a point contributes no TTFT/TPOT/ITL, which is
	// honest rather than a fabricated zero. Token counts are assigned first
	// so TPOT sees the real completion-token count.
	if stream.firstTokenAt != nil {
		sample.TTFTMillis = stream.firstTokenAt.Sub(sample.StartedAt).Seconds() * 1000
		if sample.CompletionTokens > 1 {
			sample.TPOTMillis = stream.completedAt.Sub(*stream.firstTokenAt).Seconds() * 1000 / float64(sample.CompletionTokens-1)
		}
		if stream.tokenChunks > 1 {
			sample.ITLMeanMillis = stream.lastTokenAt.Sub(*stream.firstTokenAt).Seconds() * 1000 / float64(stream.tokenChunks-1)
		}
	}
	sample.ResponseSHA256 = sha256Hex([]byte(stream.content.String()))
	sample.ResponseMetadata = streamResponseMetadata(stream)
	sample.deriveThroughput()
	return sample
}

func streamResponseMetadata(stream *httpStreamResult) map[string]any {
	out := map[string]any{"stream_chunks": stream.tokenChunks}
	if stream.responseID != "" {
		out["id"] = stream.responseID
	}
	if stream.finishReason != "" {
		out["finish_reason"] = stream.finishReason
	}
	return out
}

func (response httpLoadResponse) applyToSample(sample RequestSample, request CanonicalRequest) RequestSample {
	var decoded openAIResponse
	if failure := response.decode(&decoded); failure != nil {
		return sample.withError(failure.errorType, failure.errorCode, failure.message, failure.completedAt, failure.firstByteAt)
	}
	return sample.withSuccess(request, decoded, response.data, response.completedAt, response.firstByteAt)
}

func (response httpLoadResponse) decode(decoded *openAIResponse) *httpLoadFailure {
	if response.statusCode < 200 || response.statusCode >= 300 {
		return newHTTPLoadFailure("http_status", fmt.Sprint(response.statusCode), strings.TrimSpace(string(response.data)), response.completedAt, response.firstByteAt)
	}
	if err := json.Unmarshal(response.data, decoded); err != nil {
		return newHTTPLoadFailure("response_decode", "", err.Error(), response.completedAt, response.firstByteAt)
	}
	if decoded.Error != nil {
		return newHTTPLoadFailure(firstNonEmpty(decoded.Error.Type, "api_error"), fmt.Sprint(decoded.Error.Code), decoded.Error.Message, response.completedAt, response.firstByteAt)
	}
	if !hasCompletionChoice(decoded.Choices) {
		return newHTTPLoadFailure("response_shape", "", "response missing completion choices", response.completedAt, response.firstByteAt)
	}
	return nil
}

func hasCompletionChoice(choices []openAIChoice) bool {
	for _, choice := range choices {
		if choice.Message != nil || strings.TrimSpace(choice.Text) != "" || strings.TrimSpace(choice.FinishReason) != "" {
			return true
		}
	}
	return false
}

func (client openAIHTTPClient) requestBody(request CanonicalRequest) (map[string]any, string, error) {
	body, err := client.baseRequestBody(request)
	if err != nil {
		return nil, "", err
	}
	backend, err := client.requestBackend(request)
	if err != nil {
		return nil, "", err
	}
	build := client.chatRequestBody
	if backend == "openai" {
		build = client.completionRequestBody
	}
	built, endpoint, err := build(body, request)
	if err != nil {
		return nil, "", err
	}
	client.enforceStreamOptions(built)
	return built, endpoint, nil
}

// enforceStreamOptions keeps the workload's stream setting authoritative:
// extra_body must not silently flip streaming, because TTFT is only
// measurable on streamed responses.
func (client openAIHTTPClient) enforceStreamOptions(body map[string]any) {
	body["stream"] = client.streams()
	if client.streams() {
		body["stream_options"] = map[string]any{"include_usage": true}
	} else {
		delete(body, "stream_options")
	}
}

func (client openAIHTTPClient) baseRequestBody(request CanonicalRequest) (map[string]any, error) {
	maxTokens := firstNonZeroInt(request.MaxOutputTokens, request.OutputTokensExpected, client.workload.RandomOutputLen)
	if maxTokens <= 0 {
		return nil, fmt.Errorf("request %s missing max output tokens", request.ID)
	}
	body := map[string]any{
		"model":      client.profile.Model,
		"max_tokens": maxTokens,
		"stream":     client.streams(),
	}
	if client.streams() {
		body["stream_options"] = map[string]any{"include_usage": true}
	}
	client.applyRequestOptions(body, request)
	return body, nil
}

func (client openAIHTTPClient) applyRequestOptions(body map[string]any, request CanonicalRequest) {
	if request.Temperature != nil {
		body["temperature"] = *request.Temperature
	} else if client.workload.Temperature != nil {
		body["temperature"] = *client.workload.Temperature
	}
	if request.IgnoreEOS || client.workload.IgnoreEOS {
		body["ignore_eos"] = true
	}
}

func (client openAIHTTPClient) completionRequestBody(body map[string]any, request CanonicalRequest) (map[string]any, string, error) {
	body["prompt"] = firstNonEmpty(request.Prompt, messagesPrompt(request.Messages))
	if err := mergeExtraBody(body, client.workload.ExtraBody); err != nil {
		return nil, "", err
	}
	return body, client.endpointForBackend("openai"), nil
}

func (client openAIHTTPClient) chatRequestBody(body map[string]any, request CanonicalRequest) (map[string]any, string, error) {
	messages := request.Messages
	if len(messages) == 0 {
		prompt := firstNonEmpty(request.Prompt, messagesPrompt(request.Messages))
		if strings.TrimSpace(prompt) == "" {
			return nil, "", fmt.Errorf("request %s has no prompt or messages", request.ID)
		}
		messages = []Message{{Role: "user", Content: prompt}}
	}
	body["messages"] = messages
	if err := mergeExtraBody(body, client.workload.ExtraBody); err != nil {
		return nil, "", err
	}
	return body, client.endpointForBackend("openai-chat"), nil
}

func (client openAIHTTPClient) requestBackend(request CanonicalRequest) (string, error) {
	if mode := strings.TrimSpace(request.Mode); mode != "" {
		backend, ok := requestModeBackend(mode)
		if !ok {
			return "", fmt.Errorf("request %s has unsupported mode %q", request.ID, mode)
		}
		return backend, nil
	}
	backend, ok := requestModeBackend(firstNonEmpty(client.workload.Backend, "openai-chat"))
	if !ok {
		return "", fmt.Errorf("workload %s has unsupported backend %q", client.workload.Name, client.workload.Backend)
	}
	return backend, nil
}

func (client openAIHTTPClient) endpointForBackend(backend string) string {
	endpoint := strings.TrimSpace(client.workload.Endpoint)
	if endpointShouldFollowBackend(endpoint, client.workload.Backend) {
		return defaultEndpoint(backend)
	}
	return endpoint
}

func mergeExtraBody(body map[string]any, raw string) error {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil
	}
	decoder := json.NewDecoder(strings.NewReader(raw))
	decoder.UseNumber()
	extra := map[string]any{}
	if err := decoder.Decode(&extra); err != nil {
		return fmt.Errorf("invalid extra_body: %w", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		return fmt.Errorf("invalid extra_body: extra content after JSON object")
	}
	for key, value := range extra {
		body[key] = value
	}
	return nil
}

func (sample RequestSample) withSuccess(request CanonicalRequest, response openAIResponse, data []byte, completed time.Time, firstByteAt *time.Time) RequestSample {
	sample.Status = "completed"
	sample.CompletedAt = &completed
	sample.FirstByteAt = firstByteAt
	sample.LatencyMillis = completed.Sub(sample.StartedAt).Seconds() * 1000
	if firstByteAt != nil {
		sample.FirstByteMillis = firstByteAt.Sub(sample.StartedAt).Seconds() * 1000
	}
	sample.PromptTokens = usageInt(response.Usage.PromptTokens, request.InputTokensExpected)
	sample.CompletionTokens = usageInt(response.Usage.CompletionTokens, request.OutputTokensExpected)
	sample.TotalTokens = usageInt(response.Usage.TotalTokens, sample.PromptTokens+sample.CompletionTokens)
	sample.ResponseSHA256 = sha256Hex(data)
	sample.ResponseMetadata = responseMetadata(response)
	sample.deriveThroughput()
	return sample
}

func usageInt(value *int, fallback int) int {
	if value != nil {
		return *value
	}
	return fallback
}

func (sample RequestSample) withError(errorType, errorCode, message string, completed time.Time, firstByteAt *time.Time) RequestSample {
	sample.CompletedAt = &completed
	sample.FirstByteAt = firstByteAt
	sample.LatencyMillis = completed.Sub(sample.StartedAt).Seconds() * 1000
	if firstByteAt != nil {
		sample.FirstByteMillis = firstByteAt.Sub(sample.StartedAt).Seconds() * 1000
	}
	sample.ErrorType = errorType
	sample.ErrorCode = errorCode
	sample.ErrorMessage = message
	return sample
}

func (sample *RequestSample) deriveThroughput() {
	if sample.LatencyMillis <= 0 {
		return
	}
	seconds := sample.LatencyMillis / 1000
	if sample.CompletionTokens > 0 {
		sample.OutputTokensPerSecond = float64(sample.CompletionTokens) / seconds
	}
	if sample.TotalTokens > 0 {
		sample.TotalTokensPerSecond = float64(sample.TotalTokens) / seconds
	}
}

func responseMetadata(response openAIResponse) map[string]any {
	out := map[string]any{}
	if response.ID != "" {
		out["id"] = response.ID
	}
	if len(response.Choices) > 0 && response.Choices[0].FinishReason != "" {
		out["finish_reason"] = response.Choices[0].FinishReason
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func requestPromptText(request CanonicalRequest) string {
	if strings.TrimSpace(request.Prompt) != "" {
		return request.Prompt
	}
	return messagesPrompt(request.Messages)
}

func buildHTTPBenchmarkResult(planned PlannedRun, samples []RequestSample, started, completed time.Time) *HTTPBenchmarkResult {
	duration := completed.Sub(started).Seconds()
	result := &HTTPBenchmarkResult{
		Date:           completed.Format("20060102-150405"),
		LoadGenerator:  LoadGeneratorHTTP,
		EndpointType:   planned.Workload.Backend,
		Backend:        planned.Workload.Backend,
		ModelID:        planned.Profile.Model,
		TokenizerID:    planned.Profile.Model,
		NumPrompts:     planned.Workload.NumPrompts,
		RequestRate:    planned.Workload.RequestRate,
		MaxConcurrency: planned.Concurrency,
		Duration:       duration,
		RequestSamples: samples,
	}
	for _, sample := range samples {
		if sample.Status == "completed" {
			result.Completed++
			result.TotalInputTokens += sample.PromptTokens
			result.TotalOutputTokens += sample.CompletionTokens
			result.TotalTokens += sample.TotalTokens
			continue
		}
		result.Failed++
	}
	if duration > 0 {
		result.RequestThroughput = float64(result.Completed) / duration
		result.OutputThroughput = float64(result.TotalOutputTokens) / duration
		result.TotalTokenThroughput = float64(result.TotalTokens) / duration
	}
	applyRequestStats(result, samples)
	return result
}

func applyRequestStats(result *HTTPBenchmarkResult, samples []RequestSample) {
	outputStats := statsFromSamples(samples, true, func(sample RequestSample) float64 { return sample.OutputTokensPerSecond })
	totalStats := statsFromSamples(samples, true, func(sample RequestSample) float64 { return sample.TotalTokensPerSecond })
	latencyStats := statsFromSamples(samples, false, func(sample RequestSample) float64 { return sample.LatencyMillis })
	// TTFT exists only on streamed responses. Non-streamed samples carry no
	// TTFT at all: first-byte time is full-response arrival, not first token.
	ttftStats := statsFromSamples(samples, false, streamedTTFT)
	result.RequestOutputThroughputMean = outputStats.Mean
	result.RequestOutputThroughputStdDev = outputStats.StdDev
	result.RequestOutputThroughputMin = outputStats.Min
	result.RequestOutputThroughputP50 = outputStats.P50
	result.RequestOutputThroughputP95 = outputStats.P95
	result.RequestOutputThroughputP99 = outputStats.P99
	result.RequestOutputThroughputMax = outputStats.Max
	result.RequestTotalThroughputMean = totalStats.Mean
	result.RequestTotalThroughputStdDev = totalStats.StdDev
	result.MeanTTFTMillis = ttftStats.Mean
	result.P50TTFTMillis = ttftStats.P50
	result.P95TTFTMillis = ttftStats.P95
	result.P99TTFTMillis = ttftStats.P99
	if ttftStats.Count > 0 {
		result.TTFTSource = TTFTSourceStream
	}
	result.MeanLatencyMillis = latencyStats.Mean
	result.StdLatencyMillis = latencyStats.StdDev
	result.P50LatencyMillis = latencyStats.P50
	result.P95LatencyMillis = latencyStats.P95
	result.P99LatencyMillis = latencyStats.P99
}

// streamedTTFT admits TTFT only from streamed samples, where the first
// token's arrival was actually observed.
func streamedTTFT(sample RequestSample) float64 {
	if !sample.Streamed {
		return 0
	}
	return sample.TTFTMillis
}

func statsFromSamples(samples []RequestSample, includeZero bool, value func(RequestSample) float64) numericStats {
	values := make([]float64, 0, len(samples))
	for _, sample := range samples {
		if v, ok := sampleStatValue(sample, includeZero, value); ok {
			values = append(values, v)
		}
	}
	return numericStatsFromValues(values)
}

func sampleStatValue(sample RequestSample, includeZero bool, value func(RequestSample) float64) (float64, bool) {
	if sample.Status != "completed" {
		return 0, false
	}
	return usableStatValue(value(sample), includeZero)
}

func usableStatValue(value float64, includeZero bool) (float64, bool) {
	if math.IsNaN(value) || math.IsInf(value, 0) || value < 0 {
		return 0, false
	}
	return value, value > 0 || includeZero
}

func numericStatsFromValues(values []float64) numericStats {
	if len(values) == 0 {
		return numericStats{}
	}
	sort.Float64s(values)
	sum := 0.0
	for _, value := range values {
		sum += value
	}
	mean := sum / float64(len(values))
	varianceSum := 0.0
	for _, value := range values {
		delta := value - mean
		varianceSum += delta * delta
	}
	stddev := 0.0
	if len(values) > 1 {
		stddev = math.Sqrt(varianceSum / float64(len(values)-1))
	}
	return numericStats{
		Count:  len(values),
		Mean:   mean,
		StdDev: stddev,
		Min:    values[0],
		P50:    percentile(values, 50),
		P90:    percentile(values, 90),
		P95:    percentile(values, 95),
		P99:    percentile(values, 99),
		Max:    values[len(values)-1],
	}
}

func percentile(sorted []float64, p float64) float64 {
	if len(sorted) == 0 {
		return 0
	}
	if len(sorted) == 1 {
		return sorted[0]
	}
	rank := (p / 100) * float64(len(sorted)-1)
	lower := int(math.Floor(rank))
	upper := int(math.Ceil(rank))
	if lower == upper {
		return sorted[lower]
	}
	weight := rank - float64(lower)
	return sorted[lower]*(1-weight) + sorted[upper]*weight
}

func writeHTTPLog(logPath string, result *HTTPBenchmarkResult, duration time.Duration, runErr, memoryErr error) error {
	if err := os.MkdirAll(filepath.Dir(logPath), 0o755); err != nil {
		return err
	}
	log := map[string]any{
		"load_generator":   LoadGeneratorHTTP,
		"duration_seconds": duration.Seconds(),
	}
	if result != nil {
		log["completed"] = result.Completed
		log["failed"] = result.Failed
		log["output_throughput"] = result.OutputThroughput
		log["request_output_throughput_stddev"] = result.RequestOutputThroughputStdDev
	}
	if runErr != nil {
		log["error"] = runErr.Error()
	}
	if memoryErr != nil {
		log["memory_error"] = memoryErr.Error()
	}
	data, err := json.MarshalIndent(log, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	return os.WriteFile(logPath, data, 0o644)
}
