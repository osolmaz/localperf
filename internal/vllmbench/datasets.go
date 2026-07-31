package vllmbench

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math/rand"
	"os"
	"path/filepath"
	"strings"

	"github.com/osolmaz/localperf/internal/bench"
)

type DatasetSpec struct {
	Type         string                 `json:"type,omitempty"`
	URI          string                 `json:"uri,omitempty"`
	Path         string                 `json:"path,omitempty"`
	Split        string                 `json:"split,omitempty"`
	SampleCount  int                    `json:"sample_count,omitempty"`
	Seed         *int                   `json:"seed,omitempty"`
	Selection    string                 `json:"selection,omitempty"`
	InputTokens  int                    `json:"input_tokens,omitempty"`
	OutputTokens int                    `json:"output_tokens,omitempty"`
	Metadata     map[string]string      `json:"metadata,omitempty"`
	Extra        map[string]any         `json:"extra,omitempty"`
	Prepared     DatasetMaterialization `json:"prepared,omitempty"`
}

type DatasetMaterialization struct {
	DatasetID      string `json:"dataset_id,omitempty"`
	CanonicalPath  string `json:"canonical_path,omitempty"`
	VLLMCustomPath string `json:"vllm_custom_path,omitempty"`
	RequestCount   int    `json:"request_count,omitempty"`
	SHA256         string `json:"sha256,omitempty"`
}

type RequestSpec struct {
	Mode            string            `json:"mode,omitempty"`
	TurnPolicy      string            `json:"turn_policy,omitempty"`
	MaxOutputTokens int               `json:"max_output_tokens,omitempty"`
	Temperature     *float64          `json:"temperature,omitempty"`
	IgnoreEOS       bool              `json:"ignore_eos,omitempty"`
	Metadata        map[string]string `json:"metadata,omitempty"`
}

type CanonicalRequest struct {
	ID                   string            `json:"id"`
	Ordinal              int               `json:"ordinal"`
	DatasetID            string            `json:"dataset_id"`
	SourceRecordID       string            `json:"source_record_id,omitempty"`
	ConversationID       string            `json:"conversation_id,omitempty"`
	TurnIndex            int               `json:"turn_index,omitempty"`
	Mode                 string            `json:"mode"`
	Messages             []Message         `json:"messages,omitempty"`
	Prompt               string            `json:"prompt,omitempty"`
	Attachments          []Attachment      `json:"attachments,omitempty"`
	MaxOutputTokens      int               `json:"max_output_tokens,omitempty"`
	Temperature          *float64          `json:"temperature,omitempty"`
	IgnoreEOS            bool              `json:"ignore_eos,omitempty"`
	ArrivalOffsetMillis  int64             `json:"arrival_offset_ms,omitempty"`
	InputTokensExpected  int               `json:"input_tokens_expected,omitempty"`
	OutputTokensExpected int               `json:"output_tokens_expected,omitempty"`
	Metadata             map[string]string `json:"metadata,omitempty"`
	Raw                  json.RawMessage   `json:"raw,omitempty"`
}

type Message struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type Attachment struct {
	Type     string            `json:"type"`
	URI      string            `json:"uri,omitempty"`
	MIMEType string            `json:"mime_type,omitempty"`
	Metadata map[string]string `json:"metadata,omitempty"`
}

type DatasetInfo struct {
	Type        string            `json:"type"`
	URI         string            `json:"uri,omitempty"`
	Path        string            `json:"path,omitempty"`
	Split       string            `json:"split,omitempty"`
	Selection   string            `json:"selection,omitempty"`
	SampleCount int               `json:"sample_count,omitempty"`
	Metadata    map[string]string `json:"metadata,omitempty"`
}

type DatasetAdapter interface {
	Type() string
	Open(ctx context.Context, spec DatasetSpec, request RequestSpec) (RequestIterator, DatasetInfo, error)
}

type RequestIterator interface {
	Next(ctx context.Context) (CanonicalRequest, bool, error)
	Close() error
}

type sliceRequestIterator struct {
	requests []CanonicalRequest
	index    int
}

func (iterator *sliceRequestIterator) Next(ctx context.Context) (CanonicalRequest, bool, error) {
	if err := ctx.Err(); err != nil {
		return CanonicalRequest{}, false, err
	}
	if iterator.index >= len(iterator.requests) {
		return CanonicalRequest{}, false, nil
	}
	request := iterator.requests[iterator.index]
	iterator.index++
	return request, true, nil
}

func (iterator *sliceRequestIterator) Close() error { return nil }

func hasStructuredDataset(workload Workload) bool {
	return strings.TrimSpace(workload.Dataset.Type) != ""
}

func PrepareDatasets(ctx context.Context, spec *Spec, runDir string) error {
	for i := range spec.Workloads {
		if !hasStructuredDataset(spec.Workloads[i]) {
			continue
		}
		if err := materializeWorkloadDataset(ctx, runDir, &spec.Workloads[i]); err != nil {
			return fmt.Errorf("workload %s dataset: %w", spec.Workloads[i].Name, err)
		}
	}
	return nil
}

func materializeWorkloadDataset(ctx context.Context, runDir string, workload *Workload) error {
	requests, err := loadWorkloadDatasetRequests(ctx, workload)
	if err != nil {
		return err
	}
	if err := validateVLLMBenchRendererSupport(*workload); err != nil {
		return err
	}
	canonicalPath, vllmPath, sha, err := persistWorkloadDataset(runDir, workload.Name, requests)
	if err != nil {
		return err
	}
	updateMaterializedWorkload(workload, requests, canonicalPath, vllmPath, sha)
	return nil
}

func validateVLLMBenchRendererSupport(workload Workload) error {
	if normalizeDatasetType(workload.Dataset.Type) != "raw-payload" {
		if _, ok := requestModeBackend(workload.Request.Mode); ok {
			return nil
		}
		return fmt.Errorf("request.mode %q cannot be rendered by vLLM bench custom datasets", workload.Request.Mode)
	}
	return fmt.Errorf("dataset.type raw_payload cannot be rendered by vLLM bench without altering raw request bodies")
}

func loadWorkloadDatasetRequests(ctx context.Context, workload *Workload) ([]CanonicalRequest, error) {
	adapter, ok := datasetAdapter(workload.Dataset.Type)
	if !ok {
		return nil, fmt.Errorf("unsupported dataset type %q", workload.Dataset.Type)
	}
	iterator, _, err := adapter.Open(ctx, workload.Dataset, workload.Request)
	if err != nil {
		return nil, err
	}
	defer iterator.Close()

	requests, err := collectCanonicalRequests(ctx, iterator, datasetIDForWorkload(workload.Name))
	if err != nil {
		return nil, err
	}
	if len(requests) == 0 {
		return nil, errors.New("dataset produced no requests")
	}
	return requests, nil
}

func persistWorkloadDataset(runDir, workloadName string, requests []CanonicalRequest) (string, string, string, error) {
	canonicalPath := canonicalDatasetPath(runDir, workloadName)
	if err := writeCanonicalRequests(canonicalPath, requests); err != nil {
		return "", "", "", err
	}
	vllmPath := vllmCustomDatasetPath(runDir, workloadName)
	if err := writeVLLMCustomDataset(vllmPath, requests); err != nil {
		return "", "", "", err
	}
	shaData, err := os.ReadFile(canonicalPath)
	if err != nil {
		return "", "", "", err
	}
	return canonicalPath, vllmPath, sha256Hex(shaData), nil
}

func updateMaterializedWorkload(workload *Workload, requests []CanonicalRequest, canonicalPath, vllmPath, sha string) {
	workload.Dataset.SampleCount = len(requests)
	workload.Dataset.Prepared = DatasetMaterialization{
		DatasetID:      datasetIDForWorkload(workload.Name),
		CanonicalPath:  canonicalPath,
		VLLMCustomPath: vllmPath,
		RequestCount:   len(requests),
		SHA256:         sha,
	}
	applyMaterializedDatasetToWorkload(workload, len(requests), vllmPath)
}

func collectCanonicalRequests(ctx context.Context, iterator RequestIterator, datasetID string) ([]CanonicalRequest, error) {
	var requests []CanonicalRequest
	for {
		request, ok, err := iterator.Next(ctx)
		if err != nil || !ok {
			return requests, err
		}
		request.Ordinal = len(requests)
		request.DatasetID = firstNonEmpty(request.DatasetID, datasetID)
		if strings.TrimSpace(request.ID) == "" {
			request.ID = fmt.Sprintf("%s-%06d", datasetID, request.Ordinal+1)
		}
		if strings.TrimSpace(request.SourceRecordID) == "" {
			request.SourceRecordID = request.ID
		}
		request.Mode = firstNonEmpty(request.Mode, "chat")
		requests = append(requests, request)
	}
}

func applyMaterializedDatasetToWorkload(workload *Workload, requestCount int, vllmPath string) {
	workload.NumPrompts = materializedPromptCount(*workload, requestCount)
	workload.BenchmarkTrafficConfig.DatasetName = "custom"
	workload.BenchmarkTrafficConfig.DatasetPath = vllmPath
	workload.BenchmarkTrafficConfig.CustomOutputLen = intPointer(-1)
	workload.BenchmarkTrafficConfig.DisableShuffle = true
	previousBackend := workload.BenchmarkTrafficConfig.Backend
	backend, _ := requestModeBackend(workload.Request.Mode)
	workload.BenchmarkTrafficConfig.Backend = firstNonEmpty(backend, workload.BenchmarkTrafficConfig.Backend, "openai-chat")
	if endpointShouldFollowBackend(workload.BenchmarkTrafficConfig.Endpoint, previousBackend) {
		workload.BenchmarkTrafficConfig.Endpoint = defaultEndpoint(workload.BenchmarkTrafficConfig.Backend)
	}
	if workload.BenchmarkTrafficConfig.Backend == "openai-chat" {
		workload.BenchmarkTrafficConfig.SkipChatTemplate = true
	}
	workload.BenchmarkTrafficConfig.RequestRate = firstNonEmpty(workload.BenchmarkTrafficConfig.RequestRate, "inf")
	if workload.Request.IgnoreEOS {
		workload.IgnoreEOS = true
	}
	if workload.Request.Temperature != nil {
		workload.Temperature = workload.Request.Temperature
	}
}

func requestModeBackend(mode string) (string, bool) {
	switch strings.ToLower(strings.TrimSpace(mode)) {
	case "", "chat", "openai-chat":
		return "openai-chat", true
	case "completion", "completions", "prompt", "openai":
		return "openai", true
	default:
		return "", false
	}
}

func endpointShouldFollowBackend(endpoint, previousBackend string) bool {
	if strings.TrimSpace(endpoint) == "" {
		return true
	}
	for _, backend := range []string{previousBackend, "openai-chat", "openai"} {
		if defaultEndpoint(backend) != "" && endpoint == defaultEndpoint(backend) {
			return true
		}
	}
	return false
}

func writeCanonicalRequests(path string, requests []CanonicalRequest) error {
	return writeJSONLines(path, requests)
}

func writeVLLMCustomDataset(path string, requests []CanonicalRequest) error {
	rows := make([]vllmCustomDatasetRow, 0, len(requests))
	for _, request := range requests {
		prompt := request.Prompt
		if strings.TrimSpace(prompt) == "" {
			prompt = messagesPrompt(request.Messages)
		}
		if strings.TrimSpace(prompt) == "" {
			return fmt.Errorf("request %s cannot be rendered to vLLM custom dataset without prompt or messages", request.ID)
		}
		outputTokens := firstNonZeroInt(request.MaxOutputTokens, request.OutputTokensExpected)
		if outputTokens <= 0 {
			return fmt.Errorf("request %s missing max_output_tokens", request.ID)
		}
		rows = append(rows, vllmCustomDatasetRow{Prompt: prompt, OutputTokens: outputTokens})
	}
	return writeJSONLines(path, rows)
}

type vllmCustomDatasetRow struct {
	Prompt       string `json:"prompt"`
	OutputTokens int    `json:"output_tokens"`
}

func writeJSONLines[T any](path string, rows []T) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	var buffer bytes.Buffer
	encoder := json.NewEncoder(&buffer)
	for _, row := range rows {
		if err := encoder.Encode(row); err != nil {
			return err
		}
	}
	return os.WriteFile(path, buffer.Bytes(), 0o644)
}

func messagesPrompt(messages []Message) string {
	if len(messages) == 0 {
		return ""
	}
	if len(messages) == 1 && messages[0].Role == "user" {
		return messages[0].Content
	}
	parts := make([]string, 0, len(messages))
	for _, message := range messages {
		content := strings.TrimSpace(message.Content)
		if content == "" {
			continue
		}
		role := strings.TrimSpace(message.Role)
		if role == "" {
			parts = append(parts, content)
			continue
		}
		parts = append(parts, role+": "+content)
	}
	return strings.Join(parts, "\n\n")
}

func canonicalDatasetPath(runDir, workloadName string) string {
	return filepath.Join(runDir, "datasets", Slug(workloadName)+".canonical.jsonl")
}

func vllmCustomDatasetPath(runDir, workloadName string) string {
	return filepath.Join(runDir, "datasets", Slug(workloadName)+".vllm-custom.jsonl")
}

func datasetIDForWorkload(workloadName string) string {
	slug := Slug(workloadName)
	if slug == "" {
		return "dataset"
	}
	return slug
}

func datasetAdapter(datasetType string) (DatasetAdapter, bool) {
	switch normalizeDatasetType(datasetType) {
	case "synthetic":
		return syntheticDatasetAdapter{}, true
	case "sharegpt":
		return shareGPTDatasetAdapter{}, true
	case "custom-jsonl":
		return customJSONLDatasetAdapter{}, true
	case "raw-payload":
		return rawPayloadDatasetAdapter{}, true
	default:
		return nil, false
	}
}

func normalizeDatasetType(datasetType string) string {
	return strings.ReplaceAll(Slug(datasetType), "_", "-")
}

func firstNonEmpty(values ...string) string {
	return bench.FirstNonEmpty(values...)
}

type containsMapping struct {
	Pattern string
	Value   string
}

func matchContains(text string, mappings []containsMapping, fallback string) string {
	for _, mapping := range mappings {
		if strings.Contains(text, mapping.Pattern) {
			return mapping.Value
		}
	}
	return fallback
}

type syntheticDatasetAdapter struct{}

func (syntheticDatasetAdapter) Type() string { return "synthetic" }

func (syntheticDatasetAdapter) Open(_ context.Context, spec DatasetSpec, request RequestSpec) (RequestIterator, DatasetInfo, error) {
	count := spec.SampleCount
	if count <= 0 {
		count = 1
	}
	outputTokens := firstNonZeroInt(request.MaxOutputTokens, spec.OutputTokens)
	if outputTokens <= 0 {
		return nil, DatasetInfo{}, errors.New("synthetic dataset requires request.max_output_tokens or dataset.output_tokens")
	}
	inputTokens := spec.InputTokens
	requests := make([]CanonicalRequest, 0, count)
	for i := 0; i < count; i++ {
		raw := mustJSON(map[string]any{"input_tokens": inputTokens, "output_tokens": outputTokens, "index": i})
		requests = append(requests, CanonicalRequest{
			SourceRecordID:       fmt.Sprintf("synthetic-%06d", i+1),
			Mode:                 firstNonEmpty(request.Mode, "chat"),
			Messages:             []Message{{Role: "user", Content: syntheticPrompt(inputTokens)}},
			MaxOutputTokens:      outputTokens,
			Temperature:          request.Temperature,
			IgnoreEOS:            request.IgnoreEOS,
			InputTokensExpected:  inputTokens,
			OutputTokensExpected: outputTokens,
			Metadata:             cloneStringMap(spec.Metadata, request.Metadata),
			Raw:                  raw,
		})
	}
	return &sliceRequestIterator{requests: requests}, datasetInfo(spec, "synthetic", count), nil
}

func syntheticPrompt(inputTokens int) string {
	if inputTokens <= 0 {
		return "Benchmark request."
	}
	parts := make([]string, inputTokens)
	for i := range parts {
		parts[i] = "benchmark"
	}
	return strings.Join(parts, " ")
}

type shareGPTDatasetAdapter struct{}

func (shareGPTDatasetAdapter) Type() string { return "sharegpt" }

func (shareGPTDatasetAdapter) Open(_ context.Context, spec DatasetSpec, request RequestSpec) (RequestIterator, DatasetInfo, error) {
	path := datasetLocalPath(spec)
	if path == "" {
		return nil, DatasetInfo{}, errors.New("sharegpt dataset requires path or file:// uri")
	}
	records, err := readShareGPTRecords(path)
	if err != nil {
		return nil, DatasetInfo{}, err
	}
	records = selectDatasetRows(records, spec)
	count := sampleCount(spec, len(records))
	requests := make([]CanonicalRequest, 0, count)
	for _, record := range records {
		if len(requests) >= count {
			break
		}
		canonical, ok := shareGPTCanonicalRequest(record, request)
		if ok {
			requests = append(requests, canonical)
		}
	}
	if len(requests) == 0 {
		return nil, DatasetInfo{}, errors.New("sharegpt dataset did not contain usable conversations")
	}
	return &sliceRequestIterator{requests: requests}, datasetInfo(spec, "sharegpt", len(requests)), nil
}

type shareGPTRecord struct {
	Ordinal       int
	ID            string
	Conversations []shareGPTTurn
	Raw           json.RawMessage
}

type shareGPTTurn struct {
	From  string `json:"from"`
	Value string `json:"value"`
}

func readShareGPTRecords(path string) ([]shareGPTRecord, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	trimmed := bytes.TrimSpace(data)
	if len(trimmed) == 0 {
		return nil, errors.New("sharegpt dataset is empty")
	}
	if trimmed[0] == '[' {
		var rawRecords []json.RawMessage
		if err := json.Unmarshal(trimmed, &rawRecords); err != nil {
			return nil, err
		}
		return parseShareGPTRawRecords(rawRecords)
	}
	return readShareGPTJSONLines(path)
}

func readShareGPTJSONLines(path string) ([]shareGPTRecord, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 1024*1024), 64*1024*1024)
	var rawRecords []json.RawMessage
	for scanner.Scan() {
		line := bytes.TrimSpace(scanner.Bytes())
		if len(line) == 0 {
			continue
		}
		rawRecords = append(rawRecords, append(json.RawMessage(nil), line...))
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	return parseShareGPTRawRecords(rawRecords)
}

func parseShareGPTRawRecords(rawRecords []json.RawMessage) ([]shareGPTRecord, error) {
	records := make([]shareGPTRecord, 0, len(rawRecords))
	for i, raw := range rawRecords {
		record, ok, err := parseShareGPTRawRecord(i, raw)
		if err != nil {
			return nil, err
		}
		if !ok {
			continue
		}
		records = append(records, record)
	}
	return records, nil
}

func parseShareGPTRawRecord(index int, raw json.RawMessage) (shareGPTRecord, bool, error) {
	var generic map[string]json.RawMessage
	if err := json.Unmarshal(raw, &generic); err != nil {
		return shareGPTRecord{}, false, err
	}
	var conversations []shareGPTTurn
	if err := json.Unmarshal(generic["conversations"], &conversations); err != nil || len(conversations) == 0 {
		return shareGPTRecord{}, false, nil
	}
	record := shareGPTRecord{Ordinal: index, Conversations: conversations, Raw: append(json.RawMessage(nil), raw...)}
	record.ID = shareGPTRecordID(index, generic)
	return record, true, nil
}

func shareGPTRecordID(index int, generic map[string]json.RawMessage) string {
	for _, key := range []string{"id", "conversation_id", "source_id"} {
		var id string
		if value, ok := generic[key]; ok {
			_ = json.Unmarshal(value, &id)
		}
		if id != "" {
			return id
		}
	}
	return fmt.Sprintf("sharegpt-%06d", index+1)
}

func selectDatasetRows[T any](rows []T, spec DatasetSpec) []T {
	selection := firstNonEmpty(spec.Selection, "first_n")
	if selection != "random" && selection != "shuffle" {
		return rows
	}
	seed := int64(0)
	if spec.Seed != nil {
		seed = int64(*spec.Seed)
	}
	out := append([]T(nil), rows...)
	rand.New(rand.NewSource(seed)).Shuffle(len(out), func(i, j int) { out[i], out[j] = out[j], out[i] })
	return out
}

func shareGPTCanonicalRequest(record shareGPTRecord, request RequestSpec) (CanonicalRequest, bool) {
	turnIndex, prompt := firstUserShareGPTTurn(record.Conversations)
	if strings.TrimSpace(prompt) == "" {
		return CanonicalRequest{}, false
	}
	outputTokens := request.MaxOutputTokens
	if outputTokens <= 0 {
		return CanonicalRequest{}, false
	}
	return CanonicalRequest{
		SourceRecordID:       record.ID,
		ConversationID:       record.ID,
		TurnIndex:            turnIndex,
		Mode:                 firstNonEmpty(request.Mode, "chat"),
		Messages:             []Message{{Role: "user", Content: prompt}},
		Prompt:               prompt,
		MaxOutputTokens:      outputTokens,
		Temperature:          request.Temperature,
		IgnoreEOS:            request.IgnoreEOS,
		OutputTokensExpected: outputTokens,
		Metadata:             cloneStringMap(request.Metadata),
		Raw:                  record.Raw,
	}, true
}

func firstUserShareGPTTurn(turns []shareGPTTurn) (int, string) {
	for i, turn := range turns {
		if normalizedRole(turn.From) == "user" {
			return i, turn.Value
		}
	}
	return -1, ""
}

func normalizedRole(role string) string {
	switch strings.ToLower(strings.TrimSpace(role)) {
	case "human", "user":
		return "user"
	case "gpt", "assistant", "bot":
		return "assistant"
	case "system":
		return "system"
	default:
		return strings.ToLower(strings.TrimSpace(role))
	}
}

type customJSONLDatasetAdapter struct{}

func (customJSONLDatasetAdapter) Type() string { return "custom_jsonl" }

func (customJSONLDatasetAdapter) Open(_ context.Context, spec DatasetSpec, request RequestSpec) (RequestIterator, DatasetInfo, error) {
	path := datasetLocalPath(spec)
	if path == "" {
		return nil, DatasetInfo{}, errors.New("custom_jsonl dataset requires path or file:// uri")
	}
	rows, err := readCanonicalJSONL(path, request)
	if err != nil {
		return nil, DatasetInfo{}, err
	}
	rows = selectDatasetRows(rows, spec)
	rows = limitCanonicalRequests(rows, sampleCount(spec, len(rows)))
	return &sliceRequestIterator{requests: rows}, datasetInfo(spec, "custom_jsonl", len(rows)), nil
}

func readCanonicalJSONL(path string, requestSpec RequestSpec) ([]CanonicalRequest, error) {
	rawRows, err := readJSONLines(path)
	if err != nil {
		return nil, err
	}
	requests := make([]CanonicalRequest, 0, len(rawRows))
	for i, raw := range rawRows {
		request, err := canonicalJSONLRequest(i, raw, requestSpec)
		if err != nil {
			return nil, err
		}
		requests = append(requests, request)
	}
	return requests, nil
}

func canonicalJSONLRequest(index int, raw json.RawMessage, requestSpec RequestSpec) (CanonicalRequest, error) {
	var request CanonicalRequest
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&request); err != nil {
		return CanonicalRequest{}, err
	}
	request.Raw = append(json.RawMessage(nil), raw...)
	request.SourceRecordID = firstNonEmpty(request.SourceRecordID, request.ID, fmt.Sprintf("custom-%06d", index+1))
	request.Mode = firstNonEmpty(request.Mode, requestSpec.Mode, "chat")
	if request.MaxOutputTokens <= 0 {
		request.MaxOutputTokens = requestSpec.MaxOutputTokens
	}
	if request.Temperature == nil {
		request.Temperature = requestSpec.Temperature
	}
	request.IgnoreEOS = request.IgnoreEOS || requestSpec.IgnoreEOS
	return request, nil
}

type rawPayloadDatasetAdapter struct{}

func (rawPayloadDatasetAdapter) Type() string { return "raw_payload" }

func (rawPayloadDatasetAdapter) Open(_ context.Context, spec DatasetSpec, request RequestSpec) (RequestIterator, DatasetInfo, error) {
	path := datasetLocalPath(spec)
	if path == "" {
		return nil, DatasetInfo{}, errors.New("raw_payload dataset requires path or file:// uri")
	}
	rawRows, err := readJSONLines(path)
	if err != nil {
		return nil, DatasetInfo{}, err
	}
	limit := sampleCount(spec, len(rawRows))
	requests := make([]CanonicalRequest, 0, limit)
	for i, raw := range rawRows {
		if len(requests) >= limit {
			break
		}
		requests = append(requests, rawPayloadCanonicalRequest(i, raw, request))
	}
	return &sliceRequestIterator{requests: requests}, datasetInfo(spec, "raw_payload", len(requests)), nil
}

func rawPayloadCanonicalRequest(index int, raw json.RawMessage, requestSpec RequestSpec) CanonicalRequest {
	var payload struct {
		Messages  []Message `json:"messages"`
		Prompt    string    `json:"prompt"`
		MaxTokens int       `json:"max_tokens"`
	}
	_ = json.Unmarshal(raw, &payload)
	outputTokens := firstNonZeroInt(requestSpec.MaxOutputTokens, payload.MaxTokens)
	return CanonicalRequest{
		SourceRecordID:       fmt.Sprintf("raw-payload-%06d", index+1),
		Mode:                 "raw_payload",
		Messages:             payload.Messages,
		Prompt:               payload.Prompt,
		MaxOutputTokens:      outputTokens,
		Temperature:          requestSpec.Temperature,
		IgnoreEOS:            requestSpec.IgnoreEOS,
		OutputTokensExpected: outputTokens,
		Raw:                  append(json.RawMessage(nil), raw...),
	}
}

func readJSONLines(path string) ([]json.RawMessage, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 1024*1024), 64*1024*1024)
	var rows []json.RawMessage
	for scanner.Scan() {
		line := bytes.TrimSpace(scanner.Bytes())
		if len(line) == 0 {
			continue
		}
		rows = append(rows, append(json.RawMessage(nil), line...))
	}
	return rows, scanner.Err()
}

func datasetLocalPath(spec DatasetSpec) string {
	if strings.TrimSpace(spec.Path) != "" {
		return spec.Path
	}
	if strings.HasPrefix(spec.URI, "file://") {
		return strings.TrimPrefix(spec.URI, "file://")
	}
	return ""
}

func datasetInfo(spec DatasetSpec, datasetType string, sampleCount int) DatasetInfo {
	return DatasetInfo{
		Type:        datasetType,
		URI:         spec.URI,
		Path:        spec.Path,
		Split:       spec.Split,
		Selection:   firstNonEmpty(spec.Selection, "first_n"),
		SampleCount: sampleCount,
		Metadata:    spec.Metadata,
	}
}

func sampleCount(spec DatasetSpec, available int) int {
	if spec.SampleCount > 0 && spec.SampleCount < available {
		return spec.SampleCount
	}
	return available
}

func limitCanonicalRequests(requests []CanonicalRequest, limit int) []CanonicalRequest {
	if limit >= len(requests) {
		return requests
	}
	return requests[:limit]
}

func cloneStringMap(maps ...map[string]string) map[string]string {
	var out map[string]string
	for _, values := range maps {
		for key, value := range values {
			if out == nil {
				out = map[string]string{}
			}
			out[key] = value
		}
	}
	return out
}

// materializedPromptCount pins the request count for fixed workloads only:
// concurrency-scaled workloads keep resolving prompts per planned run (the
// materialized rows already cover the largest ladder point), and pinning
// num_prompts would trip the mutual-exclusion validation.
func materializedPromptCount(workload Workload, requestCount int) int {
	if workload.PromptsPerUser > 0 || workload.BatchesPerRepeat > 0 {
		return 0
	}
	return requestCount
}
