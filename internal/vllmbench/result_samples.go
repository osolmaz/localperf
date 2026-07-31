package vllmbench

import (
	"encoding/json"
	"fmt"
	"math"
	"sort"
	"strings"
	"time"
)

type resultSampleAdapter func(map[string]json.RawMessage) ([]RequestSample, bool, error)

type vllmBenchSampleData struct {
	inputLens  []int
	outputLens []int
	ttfts      []float64
	itls       [][]float64
	startTimes []float64
	errors     []string
	hasErrors  bool
	completed  int
	failed     int
	baseTime   time.Time
}

func requestSamplesFromResultData(data []byte) ([]RequestSample, error) {
	if len(data) == 0 || data[0] != '{' {
		return nil, nil
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil, nil
	}
	for _, adapter := range []resultSampleAdapter{
		httpSampleAdapter,
		vllmBenchSampleAdapter,
	} {
		samples, handled, err := adapter(raw)
		if handled || err != nil {
			return samples, err
		}
	}
	return nil, nil
}

func httpSampleAdapter(raw map[string]json.RawMessage) ([]RequestSample, bool, error) {
	samplesRaw, ok := raw["request_samples"]
	if !ok {
		return nil, false, nil
	}
	var samples []RequestSample
	if err := json.Unmarshal(samplesRaw, &samples); err != nil {
		return nil, true, err
	}
	return samples, true, nil
}

func vllmBenchSampleAdapter(raw map[string]json.RawMessage) ([]RequestSample, bool, error) {
	if !isVLLMBenchSampleResult(raw) {
		return nil, false, nil
	}
	data, err := readVLLMBenchSampleData(raw)
	if err != nil {
		return nil, true, err
	}
	return data.samples(), true, nil
}

func isVLLMBenchSampleResult(raw map[string]json.RawMessage) bool {
	_, hasTTFTs := raw["ttfts"]
	_, hasOutputLens := raw["output_lens"]
	return hasTTFTs && hasOutputLens
}

func readVLLMBenchSampleData(raw map[string]json.RawMessage) (vllmBenchSampleData, error) {
	data := vllmBenchSampleData{baseTime: resultStartTime(raw)}
	if err := readVLLMBenchTokenData(raw, &data); err != nil {
		return vllmBenchSampleData{}, err
	}
	if err := readVLLMBenchTimingData(raw, &data); err != nil {
		return vllmBenchSampleData{}, err
	}
	if err := readVLLMBenchErrorData(raw, &data); err != nil {
		return vllmBenchSampleData{}, err
	}
	data.completed, _ = intScalarField(raw, "completed")
	data.failed, _ = intScalarField(raw, "failed")
	return data, nil
}

func readVLLMBenchTokenData(raw map[string]json.RawMessage, data *vllmBenchSampleData) error {
	var err error
	data.inputLens, _, err = arrayField[int](raw, "input_lens")
	if err != nil {
		return err
	}
	data.outputLens, _, err = arrayField[int](raw, "output_lens")
	return err
}

func readVLLMBenchTimingData(raw map[string]json.RawMessage, data *vllmBenchSampleData) error {
	var err error
	data.ttfts, _, err = arrayField[float64](raw, "ttfts")
	if err != nil {
		return err
	}
	data.itls, _, err = arrayField[[]float64](raw, "itls")
	if err != nil {
		return err
	}
	data.startTimes, _, err = arrayField[float64](raw, "start_times")
	return err
}

func readVLLMBenchErrorData(raw map[string]json.RawMessage, data *vllmBenchSampleData) error {
	var err error
	data.errors, data.hasErrors, err = stringArrayField(raw, "errors")
	return err
}

func (data vllmBenchSampleData) samples() []RequestSample {
	count := data.sampleCount()
	if count == 0 {
		return nil
	}
	samples := make([]RequestSample, 0, count)
	minStart, hasStart := minFinite(data.startTimes)
	completed := data.completionStatuses(count)
	for i := 0; i < count; i++ {
		samples = append(samples, data.sample(i, minStart, hasStart, completed[i]))
	}
	return samples
}

func (data vllmBenchSampleData) sampleCount() int {
	return maxInts(
		len(data.inputLens),
		len(data.outputLens),
		len(data.ttfts),
		len(data.itls),
		len(data.startTimes),
		data.completed+data.failed,
	)
}

func (data vllmBenchSampleData) sample(index int, minStart float64, hasStart bool, completed bool) RequestSample {
	promptTokens := intAt(data.inputLens, index)
	completionTokens := intAt(data.outputLens, index)
	startedAt := derivedStartTime(data.baseTime, data.startTimes, index, minStart, hasStart)
	sample := RequestSample{
		RequestIndex:     index,
		RequestID:        fmt.Sprintf("vllm-bench-%d", index),
		Status:           requestStatus(completed),
		Streamed:         true,
		StartedAt:        startedAt,
		PromptTokens:     promptTokens,
		CompletionTokens: completionTokens,
		TotalTokens:      promptTokens + completionTokens,
		ResponseMetadata: vllmBenchResponseMetadata(),
	}
	data.applySampleError(&sample, index)
	return data.applySampleTimings(sample, index)
}

func (data vllmBenchSampleData) applySampleTimings(sample RequestSample, index int) RequestSample {
	ttftSeconds, hasTTFT := finiteAt(data.ttfts, index)
	itlValues := floatSliceAt(data.itls, index)
	latencySeconds := sampleLatencySeconds(ttftSeconds, hasTTFT, itlValues)
	if hasTTFT && (sample.Status == "completed" || ttftSeconds > 0) {
		sample.applyTTFT(ttftSeconds)
	}
	if len(itlValues) > 0 {
		sample.ITLMeanMillis = meanFinite(itlValues) * 1000
	}
	if sample.CompletionTokens > 1 && latencySeconds > ttftSeconds {
		sample.TPOTMillis = ((latencySeconds - ttftSeconds) / float64(sample.CompletionTokens-1)) * 1000
	}
	if latencySeconds > 0 {
		sample.applyLatency(latencySeconds)
	}
	return sample
}

func sampleLatencySeconds(ttftSeconds float64, hasTTFT bool, itlValues []float64) float64 {
	latencySeconds := sumFinite(itlValues)
	if hasTTFT {
		latencySeconds += ttftSeconds
	}
	return latencySeconds
}

func (sample *RequestSample) applyTTFT(ttftSeconds float64) {
	sample.TTFTMillis = ttftSeconds * 1000
	sample.FirstByteMillis = sample.TTFTMillis
	firstByteAt := sample.StartedAt.Add(secondsDuration(ttftSeconds))
	sample.FirstByteAt = &firstByteAt
	sample.FirstTokenAt = &firstByteAt
}

func (sample *RequestSample) applyLatency(latencySeconds float64) {
	sample.LatencyMillis = latencySeconds * 1000
	completedAt := sample.StartedAt.Add(secondsDuration(latencySeconds))
	sample.CompletedAt = &completedAt
	sample.deriveThroughput()
}

func vllmBenchResponseMetadata() map[string]any {
	return map[string]any{
		"source":           "vllm_bench",
		"timing_source":    "client_observed",
		"timestamp_source": "derived_from_result_date_and_start_times",
	}
}

func requestStatus(completed bool) string {
	if completed {
		return "completed"
	}
	return "failed"
}

func (data vllmBenchSampleData) completionStatuses(count int) []bool {
	completed := make([]bool, count)
	if data.aggregateCountsKnown() {
		remaining := data.completed
		data.markCompletions(completed, &remaining, data.hasOutputTokens)
		data.markCompletions(completed, &remaining, data.hasZeroOutputTTFTCandidate)
		data.markCompletions(completed, &remaining, data.hasZeroOutputCandidate)
		return completed
	}
	for i := range completed {
		completed[i] = !data.hasRequestError(i) && (data.hasOutputTokens(i) || data.hasEmptyErrorEntry(i))
	}
	return completed
}

func (data vllmBenchSampleData) aggregateCountsKnown() bool {
	return data.completed+data.failed > 0
}

func (data vllmBenchSampleData) markCompletions(completed []bool, remaining *int, candidate func(int) bool) {
	for i := range completed {
		if *remaining <= 0 {
			return
		}
		if completed[i] || data.hasRequestError(i) || !candidate(i) {
			continue
		}
		completed[i] = true
		*remaining--
	}
}

func (data vllmBenchSampleData) hasOutputTokens(index int) bool {
	return intAt(data.outputLens, index) > 0
}

func (data vllmBenchSampleData) hasZeroOutputTTFTCandidate(index int) bool {
	ttft, ok := finiteAt(data.ttfts, index)
	return ok && ttft > 0 && data.hasZeroOutputCandidate(index)
}

func (data vllmBenchSampleData) hasZeroOutputCandidate(index int) bool {
	return !data.hasOutputTokens(index) && data.hasZeroOutputCompletionEvidence(index)
}

func (data vllmBenchSampleData) hasRequestError(index int) bool {
	return strings.TrimSpace(stringAt(data.errors, index)) != ""
}

func (data vllmBenchSampleData) hasZeroOutputCompletionEvidence(index int) bool {
	if data.hasErrors {
		return data.hasEmptyErrorEntry(index)
	}
	return true
}

func (data vllmBenchSampleData) hasEmptyErrorEntry(index int) bool {
	return len(data.errors) > index
}

func (data vllmBenchSampleData) applySampleError(sample *RequestSample, index int) {
	message := strings.TrimSpace(stringAt(data.errors, index))
	if message == "" {
		return
	}
	sample.ErrorType = "vllm_bench_error"
	sample.ErrorMessage = message
}

func arrayField[T any](raw map[string]json.RawMessage, key string) ([]T, bool, error) {
	value, ok := raw[key]
	if !ok {
		return nil, false, nil
	}
	var out []T
	if err := json.Unmarshal(value, &out); err != nil {
		return nil, true, fmt.Errorf("%s: %w", key, err)
	}
	return out, true, nil
}

func stringArrayField(raw map[string]json.RawMessage, key string) ([]string, bool, error) {
	value, ok := raw[key]
	if !ok {
		return nil, false, nil
	}
	var values []any
	if err := json.Unmarshal(value, &values); err != nil {
		return nil, true, fmt.Errorf("%s: %w", key, err)
	}
	out := make([]string, len(values))
	for i, value := range values {
		out[i] = stringValueAny(value)
	}
	return out, true, nil
}

func stringValueAny(value any) string {
	switch typed := value.(type) {
	case nil:
		return ""
	case string:
		return typed
	default:
		return fmt.Sprint(typed)
	}
}

func intScalarField(raw map[string]json.RawMessage, key string) (int, bool) {
	value, ok := raw[key]
	if !ok {
		return 0, false
	}
	var out int
	if err := json.Unmarshal(value, &out); err != nil {
		return 0, false
	}
	return out, true
}

func floatScalarField(raw map[string]json.RawMessage, key string) float64 {
	value, ok := raw[key]
	if !ok {
		return 0
	}
	var out float64
	if err := json.Unmarshal(value, &out); err != nil || !isFinite(out) || out < 0 {
		return 0
	}
	return out
}

func resultDate(raw map[string]json.RawMessage) time.Time {
	value, ok := raw["date"]
	if !ok {
		return time.Unix(0, 0).UTC()
	}
	var text string
	if err := json.Unmarshal(value, &text); err != nil {
		return time.Unix(0, 0).UTC()
	}
	text = strings.TrimSpace(text)
	for _, candidate := range []struct {
		layout   string
		location *time.Location
	}{
		{"20060102-150405", time.Local},
		{time.RFC3339Nano, time.UTC},
		{time.RFC3339, time.UTC},
		{"2006-01-02 15:04:05.999999", time.Local},
		{"2006-01-02 15:04:05", time.Local},
	} {
		if parsed, err := time.ParseInLocation(candidate.layout, text, candidate.location); err == nil {
			return parsed.UTC()
		}
	}
	return time.Unix(0, 0).UTC()
}

func resultStartTime(raw map[string]json.RawMessage) time.Time {
	return resultDate(raw).Add(-secondsDuration(floatScalarField(raw, "duration")))
}

func derivedStartTime(base time.Time, startTimes []float64, index int, minStart float64, hasStart bool) time.Time {
	if hasStart {
		if value, ok := finiteAt(startTimes, index); ok {
			return base.Add(secondsDuration(value - minStart))
		}
	}
	return base.Add(time.Duration(index) * time.Millisecond)
}

func minFinite(values []float64) (float64, bool) {
	finite := make([]float64, 0, len(values))
	for _, value := range values {
		if isFinite(value) {
			finite = append(finite, value)
		}
	}
	if len(finite) == 0 {
		return 0, false
	}
	sort.Float64s(finite)
	return finite[0], true
}

func intAt(values []int, index int) int {
	if index < 0 || index >= len(values) {
		return 0
	}
	if values[index] < 0 {
		return 0
	}
	return values[index]
}

func finiteAt(values []float64, index int) (float64, bool) {
	if index < 0 || index >= len(values) {
		return 0, false
	}
	value := values[index]
	if !isFinite(value) || value < 0 {
		return 0, false
	}
	return value, true
}

func floatSliceAt(values [][]float64, index int) []float64 {
	if index < 0 || index >= len(values) {
		return nil
	}
	return values[index]
}

func stringAt(values []string, index int) string {
	if index < 0 || index >= len(values) {
		return ""
	}
	return values[index]
}

func sumFinite(values []float64) float64 {
	sum := 0.0
	for _, value := range values {
		if isFinite(value) && value > 0 {
			sum += value
		}
	}
	return sum
}

func meanFinite(values []float64) float64 {
	count := 0
	sum := 0.0
	for _, value := range values {
		if isFinite(value) && value >= 0 {
			sum += value
			count++
		}
	}
	if count == 0 {
		return 0
	}
	return sum / float64(count)
}

func isFinite(value float64) bool {
	return !math.IsNaN(value) && !math.IsInf(value, 0)
}

func secondsDuration(seconds float64) time.Duration {
	return time.Duration(seconds * float64(time.Second))
}

func maxInts(values ...int) int {
	max := 0
	for _, value := range values {
		if value > max {
			max = value
		}
	}
	return max
}
