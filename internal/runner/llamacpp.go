package runner

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"
)

// llamaCppProps is the part of llama-server's GET /props that fixes the
// limits a suite depends on: how many requests run at once and how much
// context each of them gets.
type llamaCppProps struct {
	BuildInfo                 string `json:"build_info"`
	ModelPath                 string `json:"model_path"`
	TotalSlots                int    `json:"total_slots"`
	DefaultGenerationSettings struct {
		NCtx int `json:"n_ctx"`
	} `json:"default_generation_settings"`
}

// serverLimits records what the server reported against what the suite
// needs. It is written as a server_limits event for every llama.cpp profile.
type serverLimits struct {
	Source          string `json:"source"`
	BuildInfo       string `json:"build_info,omitempty"`
	ModelPath       string `json:"model_path,omitempty"`
	Slots           int    `json:"slots"`
	SlotContext     int    `json:"slot_context"`
	RequiredSlots   int    `json:"required_slots"`
	RequiredContext int    `json:"required_context"`
}

// verifyServerLimits stops a llama.cpp profile before any measurement when
// the server cannot hold the suite: with too few slots, requests queue and
// the per-user numbers are wrong; with too little context per slot, the
// full-context cases fail or get truncated. It applies to managed servers
// and to servers that were already running.
func verifyServerLimits(ctx context.Context, spec Spec, profile Profile, events *eventWriter) error {
	if profileEngineFamily(spec, profile) != EngineFamilyLlamaCpp {
		return nil
	}
	client := &http.Client{Timeout: time.Duration(spec.Safety.HTTPTimeoutSec) * time.Second}
	body, ok := fetchIdentityJSON(ctx, client, profile, baseURL(profile)+"/props")
	if !ok {
		err := fmt.Errorf("profile %s: llama-server did not answer GET /props; localperf reads the slot count and context size from it", profile.Name)
		events.Write(Event{Timestamp: time.Now().UTC(), Type: "server_limits", Profile: profile.Name, Error: err.Error()})
		return err
	}
	limits, err := checkLlamaCppLimits(body, profile)
	events.Write(Event{Timestamp: time.Now().UTC(), Type: "server_limits", Profile: profile.Name, Details: mustJSON(limits), Error: errorText(err)})
	if err != nil {
		return fmt.Errorf("profile %s: %w", profile.Name, err)
	}
	return nil
}

func checkLlamaCppLimits(body []byte, profile Profile) (serverLimits, error) {
	limits := serverLimits{Source: "llama-server /props", RequiredSlots: profile.MaxNumSeqs, RequiredContext: profile.MaxModelLen}
	var props llamaCppProps
	if err := json.Unmarshal(body, &props); err != nil {
		return limits, fmt.Errorf("parse llama-server /props: %w", err)
	}
	limits.BuildInfo = props.BuildInfo
	limits.ModelPath = props.ModelPath
	limits.Slots = props.TotalSlots
	limits.SlotContext = props.DefaultGenerationSettings.NCtx
	if issues := limits.issues(); len(issues) > 0 {
		return limits, errors.New(strings.Join(issues, "; "))
	}
	return limits, nil
}

func (limits serverLimits) issues() []string {
	if limits.Slots <= 0 || limits.SlotContext <= 0 {
		return []string{"llama-server /props did not report total_slots and default_generation_settings.n_ctx (a router-mode server must be benchmarked through a single-model instance)"}
	}
	var issues []string
	if limits.Slots < limits.RequiredSlots {
		issues = append(issues, fmt.Sprintf("llama-server has %d slots, but the suite sends %d requests at once; start it with --parallel %d", limits.Slots, limits.RequiredSlots, limits.RequiredSlots))
	}
	if limits.SlotContext < limits.RequiredContext {
		issues = append(issues, fmt.Sprintf("llama-server gives each slot %d tokens of context, but the suite needs %d; start it with --ctx-size %d", limits.SlotContext, limits.RequiredContext, limits.RequiredContext*max(limits.RequiredSlots, 1)))
	}
	return issues
}

func errorText(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}
