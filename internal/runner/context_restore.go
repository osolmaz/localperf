package runner

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"
)

// Restored context: a decode case starts every sample from a saved KV cache
// of its base prompt, so the sample measures decode and not a repeated cold
// prefill. See docs/2026-10-02-restored-context-decode.md.

// ContextPreparationRestored is the context_preparation value of a decode
// case that starts from a restored snapshot.
const ContextPreparationRestored = "restored"

// ErrorTypeContextNotRestored marks a restored request whose server
// prefilled the context again instead of using the restored cache.
const ErrorTypeContextNotRestored = "context_not_restored"

// restoredPromptReserve keeps room for the prompt nonce that each request
// appends after the base prompt.
const restoredPromptReserve = 16

// restoresContext reports whether the workload starts from a snapshot.
func (workload Workload) restoresContext() bool {
	return workload.ContextPreparation == ContextPreparationRestored
}

// ContextPreparationEvidence is recorded in the result of every restored
// sample.
type ContextPreparationEvidence struct {
	Mode             string    `json:"mode"`
	SnapshotFile     string    `json:"snapshot_file"`
	BasePromptSHA256 string    `json:"base_prompt_sha256"`
	RestoredTokens   int       `json:"restored_tokens"`
	Primed           bool      `json:"primed"`
	PrimeSeconds     float64   `json:"prime_seconds,omitempty"`
	RestoreMillis    []float64 `json:"restore_ms"`
}

type restoredContext struct {
	basePrompt string
	snapshot   string
	tokens     int
}

// restoredBasePrompt is the deterministic prompt that fills the context of a
// restored case. Requests append their nonce after it.
func restoredBasePrompt(workload Workload) string {
	return syntheticPrompt(max(workload.RandomInputLen-restoredPromptReserve, 1))
}

// restoredSnapshotFile names the snapshot by everything that changes its
// contents: the immutable model revision, the runtime build, the settings
// that shape the KV state, and the base prompt. A later run of the same case
// on the same deployment reuses it; any change primes a new one.
func restoredSnapshotFile(engine EngineConfig, profile Profile, basePrompt string) string {
	settings := LlamaCppSettings{}
	if profile.LlamaCpp != nil {
		settings = *profile.LlamaCpp
	}
	key := strings.Join([]string{
		profile.Model, settings.ModelFile, metadataString(engine.Metadata, "model_revision"),
		engine.Command, metadataString(engine.Metadata, "runtime_version_requested"), metadataString(engine.Metadata, "runtime_digest"),
		settings.CacheTypeK, settings.CacheTypeV, settings.FlashAttn, strconv.Itoa(profile.MaxModelLen),
		sha256Hex([]byte(basePrompt)),
	}, "\x00")
	sum := sha256.Sum256([]byte(key))
	return "localperf-" + hex.EncodeToString(sum[:8]) + ".bin"
}

// prepareRestoredContext makes the snapshot available and restores it into
// slots 0 to concurrency-1. A missing or unusable snapshot is primed from a
// cold prefill of the base prompt and saved once.
func prepareRestoredContext(ctx context.Context, client openAIHTTPClient, engine EngineConfig, planned PlannedRun) (restoredContext, ContextPreparationEvidence, error) {
	base := restoredBasePrompt(planned.Workload)
	prepared := restoredContext{basePrompt: base, snapshot: restoredSnapshotFile(engine, planned.Profile, base)}
	evidence := ContextPreparationEvidence{Mode: ContextPreparationRestored, SnapshotFile: prepared.snapshot, BasePromptSHA256: sha256Hex([]byte(base))}
	tokens, millis, err := client.restoreSlot(ctx, 0, prepared.snapshot)
	if err != nil {
		started := time.Now()
		if err := client.primeSnapshot(ctx, prepared); err != nil {
			return prepared, evidence, err
		}
		evidence.Primed = true
		evidence.PrimeSeconds = time.Since(started).Seconds()
		if tokens, millis, err = client.restoreSlot(ctx, 0, prepared.snapshot); err != nil {
			return prepared, evidence, fmt.Errorf("restore primed snapshot %s: %w", prepared.snapshot, err)
		}
	}
	prepared.tokens = tokens
	evidence.RestoredTokens = tokens
	evidence.RestoreMillis = append(evidence.RestoreMillis, millis)
	for slot := 1; slot < max(planned.Concurrency, 1); slot++ {
		restored, millis, err := client.restoreSlot(ctx, slot, prepared.snapshot)
		if err != nil {
			return prepared, evidence, fmt.Errorf("restore snapshot %s into slot %d: %w", prepared.snapshot, slot, err)
		}
		if restored != tokens {
			return prepared, evidence, fmt.Errorf("slot %d restored %d token(s), slot 0 restored %d", slot, restored, tokens)
		}
		evidence.RestoreMillis = append(evidence.RestoreMillis, millis)
	}
	return prepared, evidence, nil
}

// primeSnapshot fills slot 0 with a cold prefill of the base prompt and saves
// it. One output token is enough: the snapshot holds the prompt.
func (client openAIHTTPClient) primeSnapshot(ctx context.Context, prepared restoredContext) error {
	body := map[string]any{
		"model": client.profile.Model, "prompt": prepared.basePrompt, "max_tokens": 1,
		"temperature": 0, "cache_prompt": true, "id_slot": 0, "stream": false,
	}
	if _, err := client.postJSON(ctx, "/v1/completions", body); err != nil {
		return fmt.Errorf("prime snapshot %s: %w", prepared.snapshot, err)
	}
	if _, err := client.postJSON(ctx, "/slots/0?action=save", map[string]any{"filename": prepared.snapshot}); err != nil {
		return fmt.Errorf("save snapshot %s: %w; a llama.cpp endpoint must run with --slot-save-path", prepared.snapshot, err)
	}
	return nil
}

func (client openAIHTTPClient) restoreSlot(ctx context.Context, slot int, snapshot string) (int, float64, error) {
	started := time.Now()
	data, err := client.postJSON(ctx, fmt.Sprintf("/slots/%d?action=restore", slot), map[string]any{"filename": snapshot})
	millis := float64(time.Since(started).Microseconds()) / 1000
	if err != nil {
		return 0, millis, err
	}
	var response struct {
		Restored int `json:"n_restored"`
	}
	if err := json.Unmarshal(data, &response); err != nil {
		return 0, millis, fmt.Errorf("decode restore response: %w", err)
	}
	if response.Restored <= 0 {
		return 0, millis, fmt.Errorf("restore of %s returned no tokens", snapshot)
	}
	return response.Restored, millis, nil
}

func (client openAIHTTPClient) postJSON(ctx context.Context, path string, body map[string]any) ([]byte, error) {
	payload, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, client.baseURL+path, bytes.NewReader(payload))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	client.applyAuthHeader(req)
	resp, err := client.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("%s returned HTTP %d: %s", path, resp.StatusCode, strings.TrimSpace(string(data)))
	}
	return data, nil
}

func metadataString(metadata map[string]any, key string) string {
	value, _ := metadata[key].(string)
	return value
}

// restoredRequests turns the planned requests into raw prompts that start
// with the base prompt. The nonce is appended when each request is sent.
func restoredRequests(requests []CanonicalRequest, prepared restoredContext) []CanonicalRequest {
	out := make([]CanonicalRequest, len(requests))
	for index, request := range requests {
		request.Messages = nil
		request.Prompt = prepared.basePrompt
		out[index] = request
	}
	return out
}

// restoredRequestBody sends a raw completion to the request's own slot with
// the prompt cache on. A chat template would add tokens after the user text,
// and llama.cpp cannot then reuse a restored cache on a hybrid model.
func (client openAIHTTPClient) restoredRequestBody(body map[string]any, request CanonicalRequest, slot int) (map[string]any, string, error) {
	body["prompt"] = request.Prompt
	if err := mergeExtraBody(body, client.workload.ExtraBody); err != nil {
		return nil, "", err
	}
	body["cache_prompt"] = true
	body["id_slot"] = slot
	return body, "/v1/completions", nil
}

// checkRestored fails a completed request whose server did not answer the
// base prompt from the restored cache.
func (client openAIHTTPClient) checkRestored(sample RequestSample) RequestSample {
	if client.restored == nil || sample.Status != "completed" {
		return sample
	}
	cached := 0
	if sample.CachedPromptTokens != nil {
		cached = *sample.CachedPromptTokens
	}
	if cached >= client.restored.tokens {
		return sample
	}
	completed := time.Now().UTC()
	if sample.CompletedAt != nil {
		completed = *sample.CompletedAt
	}
	// withError keeps the status it is given; a request that already
	// completed must be marked failed here.
	sample.Status = "failed"
	return sample.withError(ErrorTypeContextNotRestored, "", fmt.Sprintf("server reported %d cached prompt token(s), want at least the %d restored", cached, client.restored.tokens), completed, sample.FirstByteAt)
}

// validateRestoredContexts rejects restored cases that the runner cannot
// prepare, before any server starts.
func validateRestoredContexts(spec Spec) []string {
	var issues []string
	for index, workload := range spec.Workloads {
		prefix := fmt.Sprintf("workloads[%d]", index)
		switch workload.ContextPreparation {
		case "":
			continue
		case ContextPreparationRestored:
		default:
			issues = append(issues, fmt.Sprintf("%s: context_preparation must be empty or %q", prefix, ContextPreparationRestored))
			continue
		}
		issues = append(issues, restoredWorkloadIssues(prefix, workload)...)
		issues = append(issues, restoredProfileIssues(prefix, spec, workload)...)
	}
	return issues
}

func restoredWorkloadIssues(prefix string, workload Workload) []string {
	var issues []string
	if workload.Phase != "decode" {
		issues = append(issues, prefix+": a restored context needs phase decode")
	}
	if workload.LoadGenerator != LoadGeneratorHTTP || workload.DatasetName != "random" {
		issues = append(issues, prefix+": a restored context needs the localperf_http load generator with the random dataset")
	}
	if !workload.promptNonceEnabled() {
		issues = append(issues, prefix+": a restored context needs the prompt nonce, which ends every request after the base prompt")
	}
	if len(workload.Batches) == 0 {
		issues = append(issues, prefix+": a restored context needs explicit batches with one request per slot")
	}
	for _, batch := range workload.Batches {
		if batch.Requests != batch.Concurrency {
			issues = append(issues, fmt.Sprintf("%s: a restored context needs one request per slot, but the c%d batch sends %d", prefix, batch.Concurrency, batch.Requests))
		}
	}
	return issues
}

func restoredProfileIssues(prefix string, spec Spec, workload Workload) []string {
	var issues []string
	for _, profile := range spec.Profiles {
		if len(workload.Profiles) > 0 && !slices.Contains(workload.Profiles, profile.Name) {
			continue
		}
		if family := profileEngineFamily(spec, profile); family != EngineFamilyLlamaCpp {
			issues = append(issues, fmt.Sprintf("%s: profile %s runs %s, which has no context preparer; a restored context needs llama.cpp", prefix, profile.Name, firstNonEmpty(family, "an unknown engine")))
		}
	}
	return issues
}
