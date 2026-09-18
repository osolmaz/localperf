package vllmbench

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"testing"
)

func nonceTestWorkload() Workload {
	return Workload{BenchmarkTrafficConfig: BenchmarkTrafficConfig{
		Backend: "openai-chat", DatasetName: "random", RandomInputLen: 64, RandomOutputLen: 8,
	}}
}

func TestPromptNoncePrefixIsUniquePerSend(t *testing.T) {
	nonce := newPromptNonce("abcd")
	first, second := nonce.prefix(), nonce.prefix()
	if first == second {
		t.Fatalf("prefix %q repeated, want a unique stamp per send", first)
	}
	if first != "[localperf abcd:1] " || second != "[localperf abcd:2] " {
		t.Fatalf("prefixes = %q, %q, want the salt and an increasing counter", first, second)
	}
}

func TestPromptNonceSaltIsRandom(t *testing.T) {
	salt := randomPromptNonceSalt()
	if salt == randomPromptNonceSalt() {
		t.Fatal("two salts matched, want a per-run stamp that survives a warm server")
	}
	if len(salt) != 4 {
		t.Fatalf("salt = %q, want a four character salt", salt)
	}
}

func TestStampPromptLeavesASharedSystemTurnAlone(t *testing.T) {
	client := openAIHTTPClient{nonce: newPromptNonce("salt")}
	request := CanonicalRequest{ID: "r1", Messages: []Message{
		{Role: "system", Content: "system prompt"},
		{Role: "user", Content: "hello"},
	}}
	stamped := client.stampPrompt(request)
	if stamped.Messages[0].Content != "system prompt" {
		t.Fatalf("system turn = %q, want it untouched", stamped.Messages[0].Content)
	}
	if !strings.HasPrefix(stamped.Messages[1].Content, "[localperf salt:") {
		t.Fatalf("user turn = %q, want the nonce prefix", stamped.Messages[1].Content)
	}
	if !strings.HasSuffix(stamped.Messages[1].Content, " hello") {
		t.Fatalf("user turn = %q, want the original text kept", stamped.Messages[1].Content)
	}
	if request.Messages[1].Content != "hello" {
		t.Fatalf("source request mutated to %q, want it unchanged", request.Messages[1].Content)
	}
}

func TestStampPromptPrefixesCompletionPrompt(t *testing.T) {
	client := openAIHTTPClient{nonce: newPromptNonce("salt")}
	stamped := client.stampPrompt(CanonicalRequest{ID: "r1", Prompt: "hello"})
	if !strings.HasPrefix(stamped.Prompt, "[localperf salt:") || !strings.HasSuffix(stamped.Prompt, "hello") {
		t.Fatalf("prompt = %q, want the nonce prefix before the prompt", stamped.Prompt)
	}
}

func TestStampPromptFallsBackToFirstTurn(t *testing.T) {
	client := openAIHTTPClient{nonce: newPromptNonce("salt")}
	stamped := client.stampPrompt(CanonicalRequest{ID: "r1", Messages: []Message{{Role: "system", Content: "only"}}})
	if !strings.HasPrefix(stamped.Messages[0].Content, "[localperf salt:") {
		t.Fatalf("message = %q, want the nonce prefix", stamped.Messages[0].Content)
	}
}

func TestPromptNonceForRespectsTheWorkloadFlag(t *testing.T) {
	workload := nonceTestWorkload()
	if promptNonceFor(workload, "salt") == nil {
		t.Fatal("default workload has no nonce, want cache busting on by default")
	}
	enabled := true
	workload.PromptNonce = &enabled
	if promptNonceFor(workload, "salt") == nil {
		t.Fatal("explicit true workload has no nonce")
	}
	disabled := false
	workload.PromptNonce = &disabled
	if nonce := promptNonceFor(workload, "salt"); nonce != nil {
		t.Fatal("disabled workload has a nonce, want the shared prompt kept")
	}
}

func TestStampPromptDisabledKeepsTheRequest(t *testing.T) {
	disabled := false
	workload := nonceTestWorkload()
	workload.PromptNonce = &disabled
	client := openAIHTTPClient{nonce: promptNonceFor(workload, "salt")}
	request := CanonicalRequest{ID: "r1", Prompt: "hello"}
	if stamped := client.stampPrompt(request); !reflect.DeepEqual(stamped, request) {
		t.Fatalf("stamped = %+v, want the original request", stamped)
	}
}

// TestInvokeStampsRepeatedRequests proves the property that matters: sending
// the same canonical request twice reaches the server as two different
// prompts, so no repeat can be answered from a prompt cache.
func TestInvokeStampsRepeatedRequests(t *testing.T) {
	var (
		mu      sync.Mutex
		prompts []string
	)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Messages []Message `json:"messages"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("decode request body: %v", err)
		}
		mu.Lock()
		prompts = append(prompts, messagesPrompt(body.Messages))
		mu.Unlock()
		w.Header().Set("Content-Type", "text/event-stream")
		flusher := w.(http.Flusher)
		_, _ = fmt.Fprint(w, "data: {\"id\":\"c1\",\"choices\":[{\"delta\":{\"content\":\"ok\"},\"finish_reason\":\"stop\"}]}\n\n")
		_, _ = fmt.Fprint(w, "data: {\"id\":\"c1\",\"choices\":[],\"usage\":{\"prompt_tokens\":8,\"completion_tokens\":2,\"total_tokens\":10}}\n\n")
		_, _ = fmt.Fprint(w, "data: [DONE]\n\n")
		flusher.Flush()
	}))
	defer server.Close()

	client := openAIHTTPClient{
		baseURL: server.URL,
		profile: Profile{Model: "model"},
		workload: Workload{BenchmarkTrafficConfig: BenchmarkTrafficConfig{
			Backend: "openai-chat", Endpoint: "/v1/chat/completions",
			DatasetName: "random", RandomInputLen: 64, RandomOutputLen: 8,
		}},
		client: server.Client(),
		nonce:  promptNonceFor(nonceTestWorkload(), "salt"),
	}
	request := CanonicalRequest{ID: "r1", MaxOutputTokens: 4, Messages: []Message{{Role: "user", Content: "same prompt"}}}
	first := client.Invoke(context.Background(), 0, request)
	second := client.Invoke(context.Background(), 0, request)
	if first.Status != "completed" || second.Status != "completed" {
		t.Fatalf("samples = %+v, %+v, want completed requests", first, second)
	}
	if len(prompts) != 2 {
		t.Fatalf("server saw %d prompt(s), want 2", len(prompts))
	}
	if prompts[0] == prompts[1] {
		t.Fatalf("both requests arrived as %q, want distinct prompts", prompts[0])
	}
	for _, prompt := range prompts {
		if !strings.Contains(prompt, "[localperf salt:") || !strings.Contains(prompt, "same prompt") {
			t.Fatalf("prompt = %q, want the nonce stamp and the original text", prompt)
		}
	}
	if first.PromptSHA256 == second.PromptSHA256 {
		t.Fatal("recorded prompt hashes matched, want the sent prompt recorded")
	}
}
