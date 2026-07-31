package vllmbench

import (
	"context"
	"math/rand"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestDatasetAdapterRegistryAndHelpers(t *testing.T) {
	for _, name := range []string{"synthetic", "sharegpt", "custom_jsonl", "raw_payload"} {
		adapter, ok := datasetAdapter(name)
		if !ok || adapter.Type() == "" {
			t.Fatalf("datasetAdapter(%q) = %T, %t", name, adapter, ok)
		}
	}
	if _, ok := datasetAdapter("missing"); ok {
		t.Fatal("unknown dataset type unexpectedly resolved")
	}
	if got := datasetLocalPath(DatasetSpec{URI: "file:///tmp/example.jsonl"}); got != "/tmp/example.jsonl" {
		t.Fatalf("file URI path = %q", got)
	}
	if got := datasetLocalPath(DatasetSpec{Path: "local.jsonl", URI: "file:///tmp/example.jsonl"}); got != "local.jsonl" {
		t.Fatalf("explicit path = %q", got)
	}
	if got := messagesPrompt([]Message{{Role: "user", Content: "hello"}}); got != "hello" {
		t.Fatalf("single user prompt = %q", got)
	}
	multi := messagesPrompt([]Message{{Role: "system", Content: "be brief"}, {Role: "user", Content: "hello"}})
	if !strings.Contains(multi, "system: be brief") || !strings.Contains(multi, "user: hello") {
		t.Fatalf("multi-message prompt = %q", multi)
	}
	if messagesPrompt(nil) != "" || messagesPrompt([]Message{{Role: "", Content: " "}}) != "" {
		t.Fatal("empty messages should render empty prompt")
	}
	if syntheticPrompt(0) == "" || strings.Count(syntheticPrompt(3), "benchmark") != 3 {
		t.Fatalf("synthetic prompt output is unexpected")
	}
}

func TestSyntheticDatasetAdapter(t *testing.T) {
	adapter := syntheticDatasetAdapter{}
	iterator, info, err := adapter.Open(context.Background(), DatasetSpec{
		SampleCount: 2,
		InputTokens: 3,
		Metadata:    map[string]string{"source": "synthetic"},
	}, RequestSpec{
		Mode:            "chat",
		MaxOutputTokens: 5,
		IgnoreEOS:       true,
		Metadata:        map[string]string{"suite": "unit"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if info.Type != "synthetic" || info.SampleCount != 2 {
		t.Fatalf("dataset info = %+v", info)
	}
	request, ok, err := iterator.Next(context.Background())
	if err != nil || !ok {
		t.Fatalf("first request ok=%t err=%v", ok, err)
	}
	if request.InputTokensExpected != 3 || request.MaxOutputTokens != 5 || !request.IgnoreEOS || request.Metadata["suite"] != "unit" {
		t.Fatalf("synthetic request = %+v", request)
	}
	if err := iterator.Close(); err != nil {
		t.Fatal(err)
	}
	if _, _, err := adapter.Open(context.Background(), DatasetSpec{SampleCount: 1}, RequestSpec{}); err == nil {
		t.Fatal("synthetic adapter should require output tokens")
	}
}

func TestShareGPTAdapterSupportsJSONLAndSelection(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sharegpt.jsonl")
	writeFile(t, path, `{"id":"a","conversations":[{"from":"human","value":"A"},{"from":"gpt","value":"AA"}]}
{"id":"b","conversations":[{"from":"system","value":"ignored"},{"from":"user","value":"B"},{"from":"assistant","value":"BB"}]}
`)
	seed := 3
	adapter := shareGPTDatasetAdapter{}
	iterator, info, err := adapter.Open(context.Background(), DatasetSpec{
		Path:        path,
		SampleCount: 2,
		Seed:        &seed,
		Selection:   "random",
	}, RequestSpec{Mode: "chat", MaxOutputTokens: 9})
	if err != nil {
		t.Fatal(err)
	}
	if info.Type != "sharegpt" || info.Selection != "random" || info.SampleCount != 2 {
		t.Fatalf("sharegpt info = %+v", info)
	}
	var prompts []string
	for {
		request, ok, err := iterator.Next(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if !ok {
			break
		}
		prompts = append(prompts, request.Prompt)
		if request.MaxOutputTokens != 9 || request.Mode != "chat" || len(request.Raw) == 0 {
			t.Fatalf("sharegpt request = %+v", request)
		}
	}
	if len(prompts) != 2 {
		t.Fatalf("prompts = %#v, want 2", prompts)
	}
	if normalizedRole("gpt") != "assistant" || normalizedRole("human") != "user" || normalizedRole("moderator") != "moderator" {
		t.Fatal("role normalization failed")
	}
}

func TestCustomJSONLAndRawPayloadAdapters(t *testing.T) {
	dir := t.TempDir()
	customPath := filepath.Join(dir, "custom.jsonl")
	writeFile(t, customPath, `{"id":"one","messages":[{"role":"user","content":"hello"}]}
{"id":"two","prompt":"plain","max_output_tokens":3}
`)
	customIterator, customInfo, err := (customJSONLDatasetAdapter{}).Open(context.Background(), DatasetSpec{Path: customPath, SampleCount: 1}, RequestSpec{MaxOutputTokens: 7})
	if err != nil {
		t.Fatal(err)
	}
	if customInfo.Type != "custom_jsonl" || customInfo.SampleCount != 1 {
		t.Fatalf("custom info = %+v", customInfo)
	}
	customRequest, ok, err := customIterator.Next(context.Background())
	if err != nil || !ok {
		t.Fatalf("custom request ok=%t err=%v", ok, err)
	}
	if customRequest.ID != "one" || customRequest.MaxOutputTokens != 7 || len(customRequest.Raw) == 0 {
		t.Fatalf("custom request = %+v", customRequest)
	}

	rawPath := filepath.Join(dir, "raw.jsonl")
	writeFile(t, rawPath, `{"messages":[{"role":"user","content":"raw hello"}],"max_tokens":11}
{"prompt":"raw prompt","max_tokens":4}
`)
	rawIterator, rawInfo, err := (rawPayloadDatasetAdapter{}).Open(context.Background(), DatasetSpec{Path: rawPath, SampleCount: 2}, RequestSpec{})
	if err != nil {
		t.Fatal(err)
	}
	if rawInfo.Type != "raw_payload" || rawInfo.SampleCount != 2 {
		t.Fatalf("raw info = %+v", rawInfo)
	}
	rawRequest, ok, err := rawIterator.Next(context.Background())
	if err != nil || !ok {
		t.Fatalf("raw request ok=%t err=%v", ok, err)
	}
	if rawRequest.Mode != "raw_payload" || rawRequest.MaxOutputTokens != 11 || rawRequest.Messages[0].Content != "raw hello" {
		t.Fatalf("raw request = %+v", rawRequest)
	}
	if _, _, err := (customJSONLDatasetAdapter{}).Open(context.Background(), DatasetSpec{}, RequestSpec{}); err == nil {
		t.Fatal("custom_jsonl should require a path")
	}
	if _, _, err := (rawPayloadDatasetAdapter{}).Open(context.Background(), DatasetSpec{}, RequestSpec{}); err == nil {
		t.Fatal("raw_payload should require a path")
	}
}

func TestCustomJSONLAdapterRejectsUnknownFields(t *testing.T) {
	path := filepath.Join(t.TempDir(), "custom.jsonl")
	writeFile(t, path, `{"id":"one","prompt":"hello","output_tokens":3}
`)
	_, _, err := (customJSONLDatasetAdapter{}).Open(context.Background(), DatasetSpec{Path: path}, RequestSpec{})
	if err == nil || !strings.Contains(err.Error(), "unknown field") {
		t.Fatalf("custom JSONL alias error = %v, want unknown field rejection", err)
	}
}

func TestCustomJSONLAdapterAppliesRandomSelection(t *testing.T) {
	dir := t.TempDir()
	customPath := filepath.Join(dir, "custom.jsonl")
	writeFile(t, customPath, `{"id":"one","prompt":"one"}
{"id":"two","prompt":"two"}
{"id":"three","prompt":"three"}
`)
	orderedIDs := []string{"one", "two", "three"}
	seed := 1
	expected := shuffledIDs(orderedIDs, seed)[:2]
	for strings.Join(expected, ",") == "one,two" {
		seed++
		expected = shuffledIDs(orderedIDs, seed)[:2]
	}
	iterator, _, err := (customJSONLDatasetAdapter{}).Open(context.Background(), DatasetSpec{
		Path:        customPath,
		SampleCount: 2,
		Selection:   "random",
		Seed:        &seed,
	}, RequestSpec{MaxOutputTokens: 4})
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for {
		request, ok, err := iterator.Next(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if !ok {
			break
		}
		got = append(got, request.ID)
	}
	if strings.Join(got, ",") != strings.Join(expected, ",") {
		t.Fatalf("custom JSONL selected ids = %v, want %v", got, expected)
	}
}

func shuffledIDs(ids []string, seed int) []string {
	out := append([]string(nil), ids...)
	rand.New(rand.NewSource(int64(seed))).Shuffle(len(out), func(i, j int) { out[i], out[j] = out[j], out[i] })
	return out
}

func TestWriteVLLMCustomDatasetValidation(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "custom.jsonl")
	err := writeVLLMCustomDataset(path, []CanonicalRequest{{ID: "missing", MaxOutputTokens: 1}})
	if err == nil || !strings.Contains(err.Error(), "without prompt or messages") {
		t.Fatalf("missing prompt error = %v", err)
	}
	err = writeVLLMCustomDataset(path, []CanonicalRequest{{ID: "missing-output", Prompt: "hello"}})
	if err == nil || !strings.Contains(err.Error(), "missing max_output_tokens") {
		t.Fatalf("missing output error = %v", err)
	}
	if err := writeVLLMCustomDataset(path, []CanonicalRequest{{ID: "ok", Messages: []Message{{Role: "user", Content: "hello"}}, MaxOutputTokens: 1}}); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), `"prompt":"hello"`) {
		t.Fatalf("rendered custom dataset:\n%s", data)
	}
}

func TestVLLMOpenAIAdapterRender(t *testing.T) {
	adapter := VLLMOpenAIAdapter{}
	if adapter.Type() != "vllm_openai" {
		t.Fatalf("adapter type = %s", adapter.Type())
	}
	chat, err := adapter.Render(context.Background(), CanonicalRequest{
		ID:              "chat",
		Mode:            "chat",
		Messages:        []Message{{Role: "user", Content: "hello"}},
		MaxOutputTokens: 8,
	})
	if err != nil || chat.Backend != "openai-chat" || chat.Body["messages"] == nil {
		t.Fatalf("chat render = %+v err=%v", chat, err)
	}
	completion, err := adapter.Render(context.Background(), CanonicalRequest{
		ID:              "completion",
		Prompt:          "hello",
		MaxOutputTokens: 4,
	})
	if err != nil || completion.Backend != "openai" || completion.Body["prompt"] != "hello" {
		t.Fatalf("completion render = %+v err=%v", completion, err)
	}
	if _, err := adapter.Render(context.Background(), CanonicalRequest{ID: "raw", Mode: "raw_payload"}); err == nil {
		t.Fatal("raw payload render should fail")
	}
	if _, err := adapter.Render(context.Background(), CanonicalRequest{ID: "empty"}); err == nil {
		t.Fatal("empty request render should fail")
	}
}
