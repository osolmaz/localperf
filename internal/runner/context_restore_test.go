package runner

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/osolmaz/localperf/internal/convergence"
)

// restoredTestSpec is a managed fake llama-server running one restored
// decode case at c2 for two fixed samples.
func restoredTestSpec(t *testing.T, name string) Spec {
	t.Helper()
	spec := llamaCppTestSpec(t, name)
	spec.Warmup.Enabled = false
	workload := &spec.Workloads[0]
	workload.Phase = "decode"
	workload.ContextPreparation = ContextPreparationRestored
	workload.NumPrompts = 0
	workload.Batches = []Batch{{Concurrency: 2, Requests: 2}}
	workload.Convergence = convergence.Fixed(2)
	ApplyDefaults(&spec)
	return spec
}

func readHTTPResult(t *testing.T, path string) HTTPBenchmarkResult {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var result HTTPBenchmarkResult
	if err := json.Unmarshal(data, &result); err != nil {
		t.Fatal(err)
	}
	return result
}

func TestRestoredDecodePrimesOnceAndRestoresEverySample(t *testing.T) {
	spec := restoredTestSpec(t, "restored-e2e")
	summary, err := Execute(context.Background(), spec, RunOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if summary.CompletedRuns != 2 || summary.FailedRuns != 0 {
		t.Fatalf("summary = %+v, want two completed samples", summary)
	}
	events, err := os.ReadFile(summary.EventsPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(events), "--slot-save-path "+spec.Runner.SnapshotDir) {
		t.Fatalf("managed llama-server lacks --slot-save-path %s:\n%s", spec.Runner.SnapshotDir, events)
	}
	plan := BuildPlan(spec, filepath.Dir(summary.EventsPath))
	first, second := readHTTPResult(t, plan[0].ResultFile), readHTTPResult(t, plan[1].ResultFile)
	base := restoredBasePrompt(spec.Workloads[0])
	snapshot := restoredSnapshotFile(spec.Engines[0], spec.Profiles[0], base)
	for index, result := range []HTTPBenchmarkResult{first, second} {
		preparation := result.ContextPreparation
		if preparation == nil || preparation.SnapshotFile != snapshot || preparation.RestoredTokens != wordCount(base) || len(preparation.RestoreMillis) != 2 {
			t.Fatalf("sample %d preparation = %+v", index, preparation)
		}
		for _, sample := range result.RequestSamples {
			if sample.Status != "completed" || sample.CachedPromptTokens == nil || *sample.CachedPromptTokens != wordCount(base) {
				t.Fatalf("sample %d request = %+v", index, sample)
			}
		}
	}
	if !first.ContextPreparation.Primed || second.ContextPreparation.Primed {
		t.Fatalf("primed = %t then %t, want only the first sample to prime", first.ContextPreparation.Primed, second.ContextPreparation.Primed)
	}
	assertRestoredBodies(t, spec.Env["FAKE_LLAMA_BODIES"], base)
}

// assertRestoredBodies checks the wire shape: one prime with the bare base
// prompt, then raw completions to slots 0 and 1 that extend the base prompt
// with a nonce and turn the prompt cache on.
func assertRestoredBodies(t *testing.T, path, base string) {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var primes, measured int
	slots := map[float64]int{}
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		var body map[string]any
		if err := json.Unmarshal([]byte(line), &body); err != nil {
			t.Fatal(err)
		}
		prompt, _ := body["prompt"].(string)
		if body["cache_prompt"] != true || !strings.HasPrefix(prompt, base) {
			t.Fatalf("restored body = %s", line)
		}
		if prompt == base {
			primes++
			continue
		}
		if !strings.Contains(prompt[len(base):], "[localperf ") {
			t.Fatalf("request ending %q lacks the nonce", prompt[len(base):])
		}
		slots[body["id_slot"].(float64)]++
		measured++
	}
	if primes != 1 || measured != 4 || slots[0] != 2 || slots[1] != 2 {
		t.Fatalf("primes=%d measured=%d slots=%v, want 1 prime and 2 requests per slot", primes, measured, slots)
	}
}

func TestRestoredDecodeFailsWhenTheServerPrefillsAgain(t *testing.T) {
	spec := restoredTestSpec(t, "restored-no-cache")
	spec.Env["FAKE_LLAMA_NO_CACHE"] = "1"
	summary, err := Execute(context.Background(), spec, RunOptions{})
	if err == nil {
		t.Fatal("Execute succeeded, want failed samples")
	}
	if summary.CompletedRuns != 0 || summary.FailedRuns != 1 || summary.SkippedRuns != 1 {
		t.Fatalf("summary = %+v, want one failed sample that stops the point", summary)
	}
	result := readHTTPResult(t, BuildPlan(spec, filepath.Dir(summary.EventsPath))[0].ResultFile)
	for _, sample := range result.RequestSamples {
		if sample.ErrorType != ErrorTypeContextNotRestored {
			t.Fatalf("request = %+v, want %s", sample, ErrorTypeContextNotRestored)
		}
	}
}

func TestValidateRestoredContexts(t *testing.T) {
	valid := restoredTestSpec(t, "restored-valid")
	if issues := validateRestoredContexts(valid); len(issues) != 0 {
		t.Fatalf("valid restored spec issues = %v", issues)
	}
	disabled := false
	cases := map[string]func(*Spec){
		"needs phase decode":            func(spec *Spec) { spec.Workloads[0].Phase = "prefill" },
		"needs the prompt nonce":        func(spec *Spec) { spec.Workloads[0].PromptNonce = &disabled },
		"one request per slot":          func(spec *Spec) { spec.Workloads[0].Batches = []Batch{{Concurrency: 2, Requests: 4}} },
		"needs explicit batches":        func(spec *Spec) { spec.Workloads[0].Batches = nil },
		"localperf_http load":           func(spec *Spec) { spec.Workloads[0].LoadGenerator = LoadGeneratorVLLMBench },
		"has no context preparer":       func(spec *Spec) { spec.Engines[0].Type = EngineVLLMManaged },
		"must be empty or \"restored\"": func(spec *Spec) { spec.Workloads[0].ContextPreparation = "warm" },
	}
	for want, mutate := range cases {
		spec := restoredTestSpec(t, "restored-invalid")
		mutate(&spec)
		issues := strings.Join(validateRestoredContexts(spec), "\n")
		if !strings.Contains(issues, want) {
			t.Fatalf("issues = %q, want %q", issues, want)
		}
	}
}

func TestRestoredSnapshotFileChangesWithItsInputs(t *testing.T) {
	engine := EngineConfig{Command: "llama-server", Metadata: map[string]any{"model_revision": "r1", "runtime_version_requested": "b1", "runtime_digest": "d1"}}
	profile := Profile{Model: "m", MaxModelLen: 8192, LlamaCpp: &LlamaCppSettings{ModelFile: "/a.gguf", CacheTypeK: "f16"}}
	base := restoredSnapshotFile(engine, profile, "prompt")
	if base != restoredSnapshotFile(engine, profile, "prompt") || !strings.HasPrefix(base, "localperf-") || !strings.HasSuffix(base, ".bin") {
		t.Fatalf("snapshot name %q is not stable", base)
	}
	changes := map[string]func(*EngineConfig, *Profile){
		"model revision": func(e *EngineConfig, _ *Profile) {
			e.Metadata = map[string]any{"model_revision": "r2", "runtime_version_requested": "b1", "runtime_digest": "d1"}
		},
		"runtime version": func(e *EngineConfig, _ *Profile) {
			e.Metadata = map[string]any{"model_revision": "r1", "runtime_version_requested": "b2", "runtime_digest": "d1"}
		},
		"runtime digest": func(e *EngineConfig, _ *Profile) {
			e.Metadata = map[string]any{"model_revision": "r1", "runtime_version_requested": "b1", "runtime_digest": "d2"}
		},
		"runtime command": func(e *EngineConfig, _ *Profile) { e.Command = "/other/llama-server" },
		"KV cache type": func(_ *EngineConfig, p *Profile) {
			p.LlamaCpp = &LlamaCppSettings{ModelFile: "/a.gguf", CacheTypeK: "q8_0"}
		},
		"flash attention": func(_ *EngineConfig, p *Profile) {
			p.LlamaCpp = &LlamaCppSettings{ModelFile: "/a.gguf", CacheTypeK: "f16", FlashAttn: "on"}
		},
		"slot context": func(_ *EngineConfig, p *Profile) { p.MaxModelLen = 16384 },
	}
	for name, change := range changes {
		changedEngine, changedProfile := engine, profile
		change(&changedEngine, &changedProfile)
		if restoredSnapshotFile(changedEngine, changedProfile, "prompt") == base {
			t.Fatalf("snapshot name must change with the %s", name)
		}
	}
	if restoredSnapshotFile(engine, profile, "other prompt") == base {
		t.Fatal("snapshot name must change with the base prompt")
	}
	if got := wordCount(restoredBasePrompt(Workload{BenchmarkTrafficConfig: BenchmarkTrafficConfig{RandomInputLen: 100}})); got != 100-restoredPromptReserve {
		t.Fatalf("base prompt words = %d, want %d", got, 100-restoredPromptReserve)
	}
}
