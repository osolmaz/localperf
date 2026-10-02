package runner

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
)

// fakeLlamaSlots imitates llama-server's slot save and restore and its raw
// completions: a slot holds the prompt it last prefilled, a saved file holds
// a slot's prompt, and a request reports as cached the words its slot already
// holds as a prefix. FAKE_LLAMA_NO_CACHE=1 makes every request prefill again.
type fakeLlamaSlots struct {
	mu    sync.Mutex
	slots map[int]string
	files map[string]string
}

func newFakeLlamaSlots() *fakeLlamaSlots {
	return &fakeLlamaSlots{slots: map[int]string{}, files: map[string]string{}}
}

func (fake *fakeLlamaSlots) action(w http.ResponseWriter, r *http.Request) {
	slot, err := strconv.Atoi(strings.TrimPrefix(r.URL.Path, "/slots/"))
	var body struct {
		Filename string `json:"filename"`
	}
	if err != nil || json.NewDecoder(r.Body).Decode(&body) != nil {
		http.Error(w, "bad slot request", http.StatusBadRequest)
		return
	}
	fake.mu.Lock()
	defer fake.mu.Unlock()
	switch r.URL.Query().Get("action") {
	case "save":
		fake.files[body.Filename] = fake.slots[slot]
		_ = json.NewEncoder(w).Encode(map[string]any{"id_slot": slot, "n_saved": wordCount(fake.slots[slot])})
	case "restore":
		prompt, ok := fake.files[body.Filename]
		if !ok {
			http.Error(w, `{"error":{"message":"failed to restore slot"}}`, http.StatusBadRequest)
			return
		}
		fake.slots[slot] = prompt
		_ = json.NewEncoder(w).Encode(map[string]any{"id_slot": slot, "n_restored": wordCount(prompt)})
	default:
		http.Error(w, "unknown action", http.StatusBadRequest)
	}
}

func (fake *fakeLlamaSlots) complete(calls *atomic.Int64) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		data, _ := io.ReadAll(r.Body)
		recordFakeLlamaBody(data)
		var body struct {
			Prompt    string `json:"prompt"`
			Slot      int    `json:"id_slot"`
			MaxTokens int    `json:"max_tokens"`
			Stream    bool   `json:"stream"`
		}
		_ = json.Unmarshal(data, &body)
		fake.mu.Lock()
		cached := 0
		if held := fake.slots[body.Slot]; held != "" && strings.HasPrefix(body.Prompt, held) && os.Getenv("FAKE_LLAMA_NO_CACHE") != "1" {
			cached = wordCount(held)
		}
		fake.slots[body.Slot] = body.Prompt
		fake.mu.Unlock()
		prompt := wordCount(body.Prompt)
		usage := fmt.Sprintf(`{"prompt_tokens":%d,"completion_tokens":%d,"total_tokens":%d,"prompt_tokens_details":{"cached_tokens":%d}}`, prompt, body.MaxTokens, prompt+body.MaxTokens, cached)
		call := calls.Add(1)
		if !body.Stream {
			_, _ = fmt.Fprintf(w, `{"id":"cmpl-%d","choices":[{"text":"ok","finish_reason":"length"}],"usage":%s}`, call, usage)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		for i := 0; i < body.MaxTokens; i++ {
			_, _ = fmt.Fprintf(w, "data: {\"id\":\"cmpl-%d\",\"choices\":[{\"text\":\"ok\"}]}\n\n", call)
		}
		_, _ = fmt.Fprintf(w, "data: {\"id\":\"cmpl-%d\",\"choices\":[{\"text\":\"\",\"finish_reason\":\"length\"}]}\n\n", call)
		_, _ = fmt.Fprintf(w, "data: {\"id\":\"cmpl-%d\",\"choices\":[],\"usage\":%s}\n\n", call, usage)
		_, _ = fmt.Fprint(w, "data: [DONE]\n\n")
	}
}

func recordFakeLlamaBody(body []byte) {
	path := os.Getenv("FAKE_LLAMA_BODIES")
	if path == "" {
		return
	}
	file, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return
	}
	_, _ = file.Write(append(body, '\n'))
	_ = file.Close()
}

func wordCount(text string) int {
	return len(strings.Fields(text))
}
