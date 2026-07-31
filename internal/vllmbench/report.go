package vllmbench

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/osolmaz/localperf/internal/bench"
	"github.com/osolmaz/localperf/internal/collections"
)

type ReportRow struct {
	Profile             string  `json:"profile,omitempty"`
	Workload            string  `json:"workload,omitempty"`
	Phase               string  `json:"phase,omitempty"`
	DatasetName         string  `json:"dataset_name,omitempty"`
	Context             int     `json:"context,omitempty"`
	ServerMaxNumSeqs    int     `json:"server_max_num_seqs,omitempty"`
	Concurrency         int     `json:"concurrency,omitempty"`
	Repeat              int     `json:"repeat,omitempty"`
	InputLen            int     `json:"input_len,omitempty"`
	OutputLen           int     `json:"output_len,omitempty"`
	RandomInputLen      int     `json:"random_input_len,omitempty"`
	RandomOutputLen     int     `json:"random_output_len,omitempty"`
	Completed           int     `json:"completed,omitempty"`
	Failed              int     `json:"failed,omitempty"`
	PromptTokens        int     `json:"prompt_tokens,omitempty"`
	CompletionTokens    int     `json:"completion_tokens,omitempty"`
	TotalTokens         int     `json:"total_tokens,omitempty"`
	DurationSeconds     float64 `json:"duration_seconds,omitempty"`
	OutputTokensPerSec  float64 `json:"output_tokens_per_second,omitempty"`
	TotalTokensPerSec   float64 `json:"total_tokens_per_second,omitempty"`
	PerUserOutputTokSec float64 `json:"per_user_output_tokens_per_second,omitempty"`
	OutputTokSecStdDev  float64 `json:"output_tokens_per_second_stddev,omitempty"`
	TotalTokSecStdDev   float64 `json:"total_tokens_per_second_stddev,omitempty"`
	MeanTTFTMillis      float64 `json:"mean_ttft_ms,omitempty"`
	P50TTFTMillis       float64 `json:"p50_ttft_ms,omitempty"`
	P95TTFTMillis       float64 `json:"p95_ttft_ms,omitempty"`
	P99TTFTMillis       float64 `json:"p99_ttft_ms,omitempty"`
	TTFTSource          string  `json:"ttft_source,omitempty"`
	MeanTPOTMillis      float64 `json:"mean_tpot_ms,omitempty"`
	MeanLatencyMillis   float64 `json:"mean_latency_ms,omitempty"`
	StdLatencyMillis    float64 `json:"std_latency_ms,omitempty"`
	P95LatencyMillis    float64 `json:"p95_latency_ms,omitempty"`
	P99LatencyMillis    float64 `json:"p99_latency_ms,omitempty"`
	ResultFile          string  `json:"result_file,omitempty"`

	promptTokensKnown        bool
	completionTokensKnown    bool
	totalTokensKnown         bool
	outputTokensPerSecKnown  bool
	totalTokensPerSecKnown   bool
	perUserOutputTokSecKnown bool
}

func parseResultFile(path string) ([]ReportRow, error) {
	data, err := readTrimmedFile(path)
	if err != nil {
		return nil, err
	}
	if len(data) == 0 {
		return nil, nil
	}
	return parseResultData(data, path)
}

func readTrimmedFile(path string) ([]byte, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	data, err := io.ReadAll(file)
	if err != nil {
		return nil, err
	}
	return []byte(strings.TrimSpace(string(data))), nil
}

func parseResultData(data []byte, path string) ([]ReportRow, error) {
	if data[0] != '{' {
		return nil, fmt.Errorf("%s: result must be one JSON object", path)
	}
	decoder := json.NewDecoder(strings.NewReader(string(data)))
	decoder.UseNumber()
	var raw map[string]any
	if err := decoder.Decode(&raw); err != nil {
		return nil, err
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		if err == nil {
			return nil, fmt.Errorf("%s: result must contain exactly one JSON object", path)
		}
		return nil, err
	}
	return rowsFromRaw([]map[string]any{raw}, path), nil
}

func enrichRowFromEvent(row *ReportRow, event Event) {
	applyEventFields(row, event)
	deriveReportRowFields(row)
}

func applyEventFields(row *ReportRow, event Event) {
	row.Profile = event.Profile
	row.Workload = event.Workload
	row.Concurrency = event.Concurrency
	row.Repeat = event.Repeat
	row.ResultFile = event.ResultFile
}

func applyWorkloadFields(row *ReportRow, workload Workload) {
	stampVLLMBenchTTFTSource(row, workload)
	if phase := normalizeWorkloadPhase(workload.Phase); phase != "" && (row.Phase == "" || row.Phase == "mixed") {
		row.Phase = workload.Phase
	}
	if row.DatasetName == "" {
		row.DatasetName = workload.DatasetName
	}
	if workload.DatasetName == "random" {
		row.RandomInputLen = firstNonZeroInt(row.RandomInputLen, workload.RandomInputLen)
		row.RandomOutputLen = firstNonZeroInt(row.RandomOutputLen, workload.RandomOutputLen)
	}
	row.InputLen = firstNonZeroInt(row.InputLen, trafficInputLen(workload.BenchmarkTrafficConfig), structuredInputLen(workload))
	row.OutputLen = firstNonZeroInt(row.OutputLen, trafficOutputLen(workload.BenchmarkTrafficConfig), structuredOutputLen(workload))
}

// stampVLLMBenchTTFTSource marks vllm bench serve TTFT as streamed: that
// generator always streams, so its TTFT numbers are first-token
// measurements even though the raw result cannot declare provenance.
func stampVLLMBenchTTFTSource(row *ReportRow, workload Workload) {
	if row.TTFTSource == "" && workload.LoadGenerator == LoadGeneratorVLLMBench && row.MeanTTFTMillis > 0 {
		row.TTFTSource = TTFTSourceStream
	}
}

func structuredInputLen(workload Workload) int {
	return workload.Dataset.InputTokens
}

func structuredOutputLen(workload Workload) int {
	return firstNonZeroInt(workload.Request.MaxOutputTokens, workload.Dataset.OutputTokens)
}

func reportPhases(rows []ReportRow) []string {
	seen := map[string]bool{}
	for _, row := range rows {
		seen[reportRowPhase(row)] = true
	}
	phases := collections.SortedKeys(seen)
	sort.SliceStable(phases, func(i, j int) bool {
		left, right := phaseRank(phases[i]), phaseRank(phases[j])
		if left != right {
			return left < right
		}
		return phases[i] < phases[j]
	})
	return phases
}

func rowsForPhase(rows []ReportRow, phase string) []ReportRow {
	out := make([]ReportRow, 0, len(rows))
	for _, row := range rows {
		if reportRowPhase(row) == phase {
			out = append(out, row)
		}
	}
	return out
}

func reportRowPhase(row ReportRow) string {
	if phase := normalizeWorkloadPhase(row.Phase); phase != "" && phase != "mixed" {
		return phase
	}
	return inferWorkloadPhase(row.Workload, row.DisplayInputLen(), row.DisplayOutputLen())
}

func normalizeReportPhase(phase string) string {
	return bench.NormalizeReportPhase(phase)
}

func phaseRank(phase string) int {
	return bench.PhaseRank(phase)
}

func rowsFromRaw(rawRows []map[string]any, path string) []ReportRow {
	rows := make([]ReportRow, 0, len(rawRows))
	for _, raw := range rawRows {
		promptTokens := intValue(raw, "total_input_tokens")
		completionTokens := intValue(raw, "total_output_tokens")
		promptTokensKnown := hasAnyKey(raw, "total_input_tokens")
		completionTokensKnown := hasAnyKey(raw, "total_output_tokens")
		totalTokens := intValue(raw, "total_tokens")
		totalTokensKnown := hasAnyKey(raw, "total_tokens")
		if !totalTokensKnown && promptTokensKnown && completionTokensKnown {
			totalTokens = promptTokens + completionTokens
			totalTokensKnown = true
		}
		row := ReportRow{
			DatasetName:        stringValue(raw, "dataset_name"),
			Concurrency:        intValue(raw, "max_concurrency"),
			InputLen:           intValue(raw, "random_input_len"),
			OutputLen:          intValue(raw, "random_output_len"),
			RandomInputLen:     intValue(raw, "random_input_len"),
			RandomOutputLen:    intValue(raw, "random_output_len"),
			Completed:          intValue(raw, "completed"),
			Failed:             intValue(raw, "failed"),
			PromptTokens:       promptTokens,
			CompletionTokens:   completionTokens,
			TotalTokens:        totalTokens,
			DurationSeconds:    floatValue(raw, "duration"),
			OutputTokensPerSec: floatValue(raw, "output_throughput"),
			TotalTokensPerSec:  floatValue(raw, "total_token_throughput"),
			OutputTokSecStdDev: floatValue(raw, "request_output_throughput_stddev"),
			TotalTokSecStdDev:  floatValue(raw, "request_total_throughput_stddev"),
			MeanTTFTMillis:     floatValue(raw, "mean_ttft_ms"),
			P50TTFTMillis:      floatValue(raw, "p50_ttft_ms"),
			P95TTFTMillis:      floatValue(raw, "p95_ttft_ms"),
			P99TTFTMillis:      floatValue(raw, "p99_ttft_ms"),
			TTFTSource:         stringValue(raw, "ttft_source"),
			MeanTPOTMillis:     floatValue(raw, "mean_tpot_ms"),
			MeanLatencyMillis:  floatValue(raw, "mean_latency_ms"),
			StdLatencyMillis:   floatValue(raw, "std_latency_ms"),
			P95LatencyMillis:   floatValue(raw, "p95_latency_ms"),
			P99LatencyMillis:   floatValue(raw, "p99_latency_ms"),
			ResultFile:         path,

			promptTokensKnown:       promptTokensKnown,
			completionTokensKnown:   completionTokensKnown,
			totalTokensKnown:        totalTokensKnown,
			outputTokensPerSecKnown: hasAnyKey(raw, "output_throughput"),
			totalTokensPerSecKnown:  hasAnyKey(raw, "total_token_throughput"),
		}
		deriveReportRowFields(&row)
		rows = append(rows, row)
	}
	return rows
}

func deriveReportRowFields(row *ReportRow) {
	if row.InputLen == 0 {
		row.InputLen = row.RandomInputLen
	}
	if row.OutputLen == 0 {
		row.OutputLen = row.RandomOutputLen
	}
	if row.Phase == "" || row.Phase == "mixed" {
		row.Phase = reportRowPhase(*row)
	} else {
		row.Phase = normalizeReportPhase(row.Phase)
	}
	if row.Concurrency > 0 && (row.OutputTokensPerSec > 0 || row.outputTokensPerSecKnown) {
		row.PerUserOutputTokSec = row.OutputTokensPerSec / float64(row.Concurrency)
		row.perUserOutputTokSecKnown = row.outputTokensPerSecKnown
	}
}

func firstNonZeroInt(values ...int) int {
	for _, value := range values {
		if value != 0 {
			return value
		}
	}
	return 0
}

func (row ReportRow) DisplayInputLen() int { return firstNonZeroInt(row.InputLen, row.RandomInputLen) }

func (row ReportRow) DisplayOutputLen() int {
	return firstNonZeroInt(row.OutputLen, row.RandomOutputLen)
}

func trafficInputLen(traffic BenchmarkTrafficConfig) int {
	switch traffic.DatasetName {
	case "random":
		return traffic.RandomInputLen
	case "sonnet":
		return traffic.SonnetInputLen
	case "prefix_repetition":
		return traffic.PrefixRepetitionPrefixLen + traffic.PrefixRepetitionSuffixLen
	default:
		return 0
	}
}

func trafficOutputLen(traffic BenchmarkTrafficConfig) int {
	switch traffic.DatasetName {
	case "random":
		return traffic.RandomOutputLen
	case "custom":
		return positiveIntPointer(traffic.CustomOutputLen)
	case "sharegpt":
		return intPointerValue(traffic.ShareGPTOutputLen)
	case "sonnet":
		return traffic.SonnetOutputLen
	case "prefix_repetition":
		return traffic.PrefixRepetitionOutputLen
	case "speed_bench":
		return traffic.SpeedBenchOutputLen
	}
	return 0
}

func positiveIntPointer(value *int) int {
	if value != nil && *value > 0 {
		return *value
	}
	return 0
}

func intPointerValue(value *int) int {
	if value == nil {
		return 0
	}
	return *value
}

func readEvents(path string) ([]Event, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 1024*1024), 64*1024*1024)
	var events []Event
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		var event Event
		if err := json.Unmarshal([]byte(line), &event); err != nil {
			return nil, err
		}
		events = append(events, event)
	}
	return events, scanner.Err()
}

func sortReportRows(rows []ReportRow) {
	sort.Slice(rows, func(i, j int) bool {
		a, b := rows[i], rows[j]
		aPhase := reportRowPhase(a)
		bPhase := reportRowPhase(b)
		if phaseRank(aPhase) != phaseRank(bPhase) {
			return phaseRank(aPhase) < phaseRank(bPhase)
		}
		if aPhase != bPhase {
			return aPhase < bPhase
		}
		if a.Profile != b.Profile {
			return a.Profile < b.Profile
		}
		if a.Context != b.Context {
			return a.Context < b.Context
		}
		if a.Workload != b.Workload {
			return a.Workload < b.Workload
		}
		if a.Concurrency != b.Concurrency {
			return a.Concurrency < b.Concurrency
		}
		return a.ResultFile < b.ResultFile
	})
}

func stringValue(row map[string]any, key string) string {
	value, ok := row[key]
	if !ok || value == nil {
		return ""
	}
	switch typed := value.(type) {
	case string:
		return typed
	default:
		return fmt.Sprint(typed)
	}
}

func hasAnyKey(row map[string]any, keys ...string) bool {
	for _, key := range keys {
		if _, ok := row[key]; ok {
			return true
		}
	}
	return false
}

func intValue(row map[string]any, key string) int {
	value, ok := row[key]
	if !ok || value == nil {
		return 0
	}
	switch typed := value.(type) {
	case int:
		return typed
	case int64:
		return int(typed)
	case float64:
		return int(typed)
	case json.Number:
		number, _ := typed.Int64()
		return int(number)
	default:
		return 0
	}
}

func floatValue(row map[string]any, key string) float64 {
	value, ok := row[key]
	if !ok || value == nil {
		return 0
	}
	switch typed := value.(type) {
	case int:
		return float64(typed)
	case int64:
		return float64(typed)
	case float64:
		return typed
	case json.Number:
		number, _ := typed.Float64()
		return number
	default:
		return 0
	}
}

func cell(value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return "-"
	}
	return strings.ReplaceAll(value, "|", "\\|")
}

func intCell(value int) string {
	return formatZeroInt(value, "-")
}

func intCSV(value int) string {
	return formatZeroInt(value, "")
}

func formatZeroInt(value int, zero string) string {
	if value == 0 {
		return zero
	}
	return fmt.Sprint(value)
}

func floatCell(value float64) string {
	if value == 0 || math.IsNaN(value) || math.IsInf(value, 0) {
		return "-"
	}
	return fmt.Sprintf("%.1f", value)
}

func floatCSV(value float64) string {
	if value == 0 || math.IsNaN(value) || math.IsInf(value, 0) {
		return ""
	}
	return strconv.FormatFloat(value, 'f', -1, 64)
}

func fileCell(runDir, path string) string {
	if path == "" {
		return ""
	}
	if rel, err := filepath.Rel(runDir, path); err == nil && !strings.HasPrefix(rel, "..") {
		return rel
	}
	return path
}

func resolveResultPath(runDir, path string) string {
	if path == "" || filepath.IsAbs(path) {
		return path
	}
	cleanPath := filepath.Clean(path)
	cleanRunDir := filepath.Clean(runDir)
	return resolveRunDirResultPath(cleanRunDir, cleanPath)
}

func resolveRunDirResultPath(runDir, path string) string {
	candidate := filepath.Join(runDir, path)
	if fileExists(candidate) {
		return candidate
	}
	if stripped, ok := stripRunDirPrefix(runDir, path); ok {
		return filepath.Join(runDir, stripped)
	}
	if fileExists(path) {
		return path
	}
	return candidate
}

func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

func stripRunDirPrefix(runDir, path string) (string, bool) {
	parts := strings.Split(filepath.ToSlash(filepath.Clean(runDir)), "/")
	path = filepath.Clean(path)
	for i := 0; i < len(parts); i++ {
		if parts[i] == "" {
			continue
		}
		suffix := filepath.FromSlash(strings.Join(parts[i:], "/"))
		if suffix == "." || suffix == "" {
			continue
		}
		if path == suffix {
			return "", true
		}
		prefix := suffix + string(filepath.Separator)
		if strings.HasPrefix(path, prefix) {
			return strings.TrimPrefix(path, prefix), true
		}
	}
	return "", false
}
