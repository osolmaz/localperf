package runner

import (
	"context"
	"fmt"
)

type EngineRequest struct {
	Backend string         `json:"backend"`
	Body    map[string]any `json:"body,omitempty"`
}

type EngineAdapter interface {
	Type() string
	Render(ctx context.Context, request CanonicalRequest) (EngineRequest, error)
}

type VLLMOpenAIAdapter struct{}

func (VLLMOpenAIAdapter) Type() string { return "vllm_openai" }

func (VLLMOpenAIAdapter) Render(ctx context.Context, request CanonicalRequest) (EngineRequest, error) {
	if err := ctx.Err(); err != nil {
		return EngineRequest{}, err
	}
	if request.Mode == "raw_payload" {
		return EngineRequest{}, fmt.Errorf("raw_payload requests need a raw-payload renderer")
	}
	body := map[string]any{
		"max_tokens": request.MaxOutputTokens,
	}
	if request.Temperature != nil {
		body["temperature"] = *request.Temperature
	}
	if len(request.Messages) > 0 {
		body["messages"] = request.Messages
		return EngineRequest{Backend: "openai-chat", Body: body}, nil
	}
	if request.Prompt != "" {
		body["prompt"] = request.Prompt
		return EngineRequest{Backend: "openai", Body: body}, nil
	}
	return EngineRequest{}, fmt.Errorf("request %s has no messages or prompt", request.ID)
}
