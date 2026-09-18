package vllmbench

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"strings"
	"sync/atomic"
)

// promptNonce stamps every request with a unique prefix. Without it a repeat
// of an earlier request can be answered from the server's prompt cache: the
// prefill collapses to a few tokens and the row reports a prefill rate that no
// cold request can reach. The stamp is applied when a request is sent, so
// repeats of one case and separate runs against a warm server both stay cold.
type promptNonce struct {
	salt  string
	index atomic.Int64
}

func newPromptNonce(salt string) *promptNonce {
	return &promptNonce{salt: salt}
}

// promptNonceFor returns nil when the workload asks for shared prompts, which
// keeps a deliberate prefix-cache measurement possible.
func promptNonceFor(workload Workload, salt string) *promptNonce {
	if !workload.promptNonceEnabled() {
		return nil
	}
	return newPromptNonce(salt)
}

// randomPromptNonceSalt separates runs that reuse the same suite and prompts
// against a server that stays up between them.
func randomPromptNonceSalt() string {
	buffer := make([]byte, 4)
	if _, err := rand.Read(buffer); err != nil {
		return "unseeded"
	}
	return hex.EncodeToString(buffer)
}

func (nonce *promptNonce) prefix() string {
	return fmt.Sprintf("[localperf nonce %s %06d] ", nonce.salt, nonce.index.Add(1))
}

// promptNonceEnabled defaults to true: a run must not silently measure a
// cached prompt. Set "prompt_nonce": false to measure prefix reuse on purpose.
func (workload Workload) promptNonceEnabled() bool {
	return workload.PromptNonce == nil || *workload.PromptNonce
}

// stampPrompt returns the request as it goes on the wire. The stamp rides in
// the first user turn so it lands at the very start of the token stream, where
// a prefix cache would otherwise match, while a shared system prompt stays
// untouched.
func (client openAIHTTPClient) stampPrompt(request CanonicalRequest) CanonicalRequest {
	if client.nonce == nil {
		return request
	}
	prefix := client.nonce.prefix()
	if index := firstUserMessage(request.Messages); index >= 0 {
		messages := append([]Message(nil), request.Messages...)
		messages[index].Content = prefix + messages[index].Content
		request.Messages = messages
		request.Prompt = ""
		return request
	}
	if strings.TrimSpace(request.Prompt) != "" {
		request.Prompt = prefix + request.Prompt
	}
	return request
}

// firstUserMessage prefers the first user turn and falls back to the first
// turn, so a prompt with only system text is still stamped.
func firstUserMessage(messages []Message) int {
	fallback := -1
	for index, message := range messages {
		if strings.EqualFold(strings.TrimSpace(message.Role), "user") {
			return index
		}
		if fallback < 0 {
			fallback = index
		}
	}
	return fallback
}
