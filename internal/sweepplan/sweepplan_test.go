package sweepplan

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/osolmaz/localperf/internal/vllmbench"
)

// TestGeneratedSpecRoundTripsValidation is the guarantee that the generator
// and the validator can never drift: every generated spec must validate with
// zero issues.
func TestGeneratedSpecRoundTripsValidation(t *testing.T) {
	spec, err := Plan(PlanRequest{
		Model:            "example/model",
		Contexts:         []int{8192, 16384, 32768, 65536, 131072},
		Concurrency:      []int{1, 4, 8, 16, 32},
		Repeats:          2,
		IncludeReference: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	vllmbench.ApplyDefaults(&spec)
	if err := vllmbench.ValidateSpec(spec); err != nil {
		t.Fatalf("generated spec failed validation: %v", err)
	}
	// 1 reference + 2 workloads per ladder point.
	if len(spec.Workloads) != 11 {
		t.Fatalf("workloads = %d, want 11", len(spec.Workloads))
	}
	if len(spec.Profiles) != 6 {
		t.Fatalf("profiles = %d, want 6", len(spec.Profiles))
	}
}

func TestShapesStayInsideContractBand(t *testing.T) {
	for _, context := range []int{4096, 8192, 16384, 32768, 65536, 131072, 262144} {
		for _, shape := range []struct {
			name             string
			input, output    int
			minimalOutputMax int
		}{
			{"prefill", 0, 0, 1},
			{"decode", 0, 0, 0},
		} {
			input, output := PrefillShape(context)
			if shape.name == "decode" {
				input, output = DecodeShape(context)
			}
			sum := input + output
			if float64(sum) < vllmbench.ContextTargetMinFrac*float64(context) || sum > context {
				t.Fatalf("%s shape for %d = %d+%d (%d), outside [90%%, 100%%] band", shape.name, context, input, output, sum)
			}
			if shape.name == "prefill" && output != 1 {
				t.Fatalf("prefill output = %d, want 1", output)
			}
		}
	}
}

// TestPlanGolden pins byte-stable generator output: same request in, same
// spec out.
func TestPlanGolden(t *testing.T) {
	spec, err := Plan(PlanRequest{
		Model:            "example/model",
		Contexts:         []int{8192},
		Concurrency:      []int{1, 4},
		IncludeReference: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	got, err := json.MarshalIndent(spec, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	got = append(got, '\n')
	goldenPath := filepath.Join("testdata", "default-sweep-8k.golden.json")
	if os.Getenv("UPDATE_GOLDEN") == "1" {
		if err := os.MkdirAll("testdata", 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(goldenPath, got, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(goldenPath)
	if err != nil {
		t.Fatalf("read golden (run with UPDATE_GOLDEN=1 to create): %v", err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("generated spec differs from golden; run with UPDATE_GOLDEN=1 to update.\ngot:\n%s", got)
	}
}

func TestPlanKeeps4KReferenceSeparateFromActive4K(t *testing.T) {
	spec, err := Plan(PlanRequest{
		Model:            "example/model",
		Contexts:         []int{4096},
		Concurrency:      []int{1, 4},
		IncludeReference: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	profiles := map[string]vllmbench.Profile{}
	for _, profile := range spec.Profiles {
		profiles[profile.Name] = profile
	}
	if _, ok := profiles["4k-reference"]; !ok {
		t.Fatal("missing 4k-reference profile")
	}
	if _, ok := profiles["4k"]; !ok {
		t.Fatal("missing active 4k profile")
	}
	workloads := map[string]vllmbench.Workload{}
	for _, workload := range spec.Workloads {
		workloads[workload.Name] = workload
	}
	reference := workloads["max-throughput-reference"]
	if reference.ContextSemantics != vllmbench.ContextSemanticsCapacity || reference.Profiles[0] != "4k-reference" {
		t.Fatalf("reference workload = %+v, want capacity workload on 4k-reference", reference)
	}
	prefill := workloads["prefill-4k"]
	if prefill.ContextSemantics != vllmbench.ContextSemanticsActive || prefill.Phase != "prefill" || prefill.Profiles[0] != "4k" {
		t.Fatalf("prefill-4k = %+v, want active prefill workload on 4k", prefill)
	}
	decode := workloads["decode-4k"]
	if decode.ContextSemantics != vllmbench.ContextSemanticsActive || decode.Phase != "decode" || decode.Profiles[0] != "4k" {
		t.Fatalf("decode-4k = %+v, want active decode workload on 4k", decode)
	}
}

func TestPlanProfileArgsCanOmitUnsafeEngineFlags(t *testing.T) {
	spec, err := Plan(PlanRequest{
		Model:       "example/model",
		Contexts:    []int{8192},
		Concurrency: []int{1},
		ProfileArgs: []string{"--trust-remote-code"},
		ProfileEngineArgs: []string{
			"--kv-cache-memory-bytes", "21474836480",
			"--attention-backend", "flashinfer",
			"--unsafe-flag=1",
		},
		OmitProfileEngineFlags: []string{"--kv-cache-memory-bytes", "--unsafe-flag"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(spec.Profiles) != 1 {
		t.Fatalf("profiles = %d, want 1", len(spec.Profiles))
	}
	profile := spec.Profiles[0]
	if strings.Join(profile.Args, " ") != "--trust-remote-code" {
		t.Fatalf("args = %v, want trust remote code", profile.Args)
	}
	got := strings.Join(profile.EngineArgs, " ")
	if strings.Contains(got, "kv-cache-memory-bytes") || strings.Contains(got, "unsafe-flag") {
		t.Fatalf("engine args = %v, want omitted flags removed", profile.EngineArgs)
	}
	if got != "--attention-backend flashinfer" {
		t.Fatalf("engine args = %q, want retained safe args", got)
	}
}

func TestPlanDropsFixedKVCacheForQwenModels(t *testing.T) {
	spec, err := Plan(PlanRequest{
		Model:       "nvidia/Qwen3.6-27B-NVFP4",
		Contexts:    []int{8192},
		Concurrency: []int{1},
		ProfileEngineArgs: []string{
			"--kv-cache-memory-bytes", "21474836480",
			"--attention-backend", "flashinfer",
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	got := strings.Join(spec.Profiles[0].EngineArgs, " ")
	if strings.Contains(got, "kv-cache-memory-bytes") || strings.Contains(got, "21474836480") {
		t.Fatalf("engine args = %q, want fixed KV cache omitted for Qwen", got)
	}
	if got != "--attention-backend flashinfer" {
		t.Fatalf("engine args = %q, want retained non-KV args", got)
	}
}

func TestPlanKeepsFixedKVCacheForNonQwenModels(t *testing.T) {
	spec, err := Plan(PlanRequest{
		Model:       "google/gemma-4-26b-it",
		Contexts:    []int{8192},
		Concurrency: []int{1},
		ProfileEngineArgs: []string{
			"--kv-cache-memory-bytes", "12884901888",
			"--attention-backend", "flashinfer",
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	got := strings.Join(spec.Profiles[0].EngineArgs, " ")
	if !strings.Contains(got, "--kv-cache-memory-bytes 12884901888") {
		t.Fatalf("engine args = %q, want fixed KV cache retained for non-Qwen", got)
	}
}

func TestStressProfilesAdmitSpotCheckConcurrency(t *testing.T) {
	spec, err := Plan(PlanRequest{
		Model:         "example/model",
		Contexts:      []int{32768, 65536},
		Concurrency:   []int{1},
		IncludeStress: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, profile := range spec.Profiles {
		if (profile.Name == "32k" || profile.Name == "64k") && profile.MaxNumSeqs < 4 {
			t.Fatalf("profile %s max_num_seqs = %d, want >= 4 for stress spot checks", profile.Name, profile.MaxNumSeqs)
		}
	}
}

func TestPlanRequiresModel(t *testing.T) {
	if _, err := Plan(PlanRequest{Contexts: []int{8192}}); err == nil || !strings.Contains(err.Error(), "model is required") {
		t.Fatalf("Plan error = %v, want model required", err)
	}
}

func TestStressPresetAddsSpotChecksAnd128k(t *testing.T) {
	spec, err := Plan(PlanRequest{
		Model:         "example/model",
		Contexts:      []int{8192, 32768, 65536},
		Concurrency:   []int{1, 4, 8, 16},
		IncludeStress: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	vllmbench.ApplyDefaults(&spec)
	if err := vllmbench.ValidateSpec(spec); err != nil {
		t.Fatalf("stress spec failed validation: %v", err)
	}
	workloads := map[string]vllmbench.Workload{}
	for _, workload := range spec.Workloads {
		workloads[workload.Name] = workload
	}
	stress32 := workloads["decode-stress-32k"]
	if stress32.RandomOutputLen != 4096 || len(stress32.MaxConcurrency) != 1 || stress32.MaxConcurrency[0] != 4 {
		t.Fatalf("decode-stress-32k = %+v, want 4096 output at c4", stress32)
	}
	stress64 := workloads["decode-stress-64k"]
	if stress64.RandomOutputLen != 4096 || len(stress64.MaxConcurrency) != 2 {
		t.Fatalf("decode-stress-64k = %+v, want 4096 output at c1,c4", stress64)
	}
	// Stress adds the 128k points, capped at c4.
	decode128 := workloads["decode-128k"]
	if decode128.ContextTarget != 131072 {
		t.Fatalf("decode-128k target = %d, want 131072", decode128.ContextTarget)
	}
	for _, concurrency := range decode128.MaxConcurrency {
		if concurrency > 4 {
			t.Fatalf("decode-128k concurrency %v exceeds the high-context cap", decode128.MaxConcurrency)
		}
	}
	// The 128k profile's server admission cap follows its capped ladder;
	// max_num_seqs 16 there would defeat the memory cap.
	for _, profile := range spec.Profiles {
		if profile.Name == "128k" && profile.MaxNumSeqs > 4 {
			t.Fatalf("128k profile max_num_seqs = %d, want <= 4", profile.MaxNumSeqs)
		}
	}
	// No stress spot for 8k: only contexts with defined spot checks get one.
	if _, ok := workloads["decode-stress-8k"]; ok {
		t.Fatal("unexpected stress workload for 8k")
	}
}

func TestDefaultGridUsesShortDecodeAndScaledPrompts(t *testing.T) {
	spec, err := Plan(PlanRequest{
		Model:       "example/model",
		Contexts:    []int{65536},
		Concurrency: []int{1, 4, 16},
	})
	if err != nil {
		t.Fatal(err)
	}
	vllmbench.ApplyDefaults(&spec)
	var decode vllmbench.Workload
	for _, workload := range spec.Workloads {
		if workload.Name == "decode-64k" {
			decode = workload
		}
	}
	if decode.RandomOutputLen != 1024 {
		t.Fatalf("decode output = %d, want 1024 by default", decode.RandomOutputLen)
	}
	if decode.PromptsPerUser != 2 {
		t.Fatalf("prompts_per_user = %d, want 2 by default", decode.PromptsPerUser)
	}
	// Planned runs resolve prompts directly from prompts_per_user.
	plan := vllmbench.BuildPlan(spec, "runs/example")
	prompts := map[int]int{}
	for _, planned := range plan {
		if planned.Workload.Name == "decode-64k" {
			prompts[planned.Concurrency] = planned.Workload.NumPrompts
		}
	}
	if prompts[1] != 2 || prompts[4] != 8 || prompts[16] != 32 {
		t.Fatalf("resolved prompts = %v, want map[1:2 4:8 16:32]", prompts)
	}
}

func TestOmittedFlagValuesKeepsFollowingFlags(t *testing.T) {
	got := omittedFlagValues(
		[]string{"--disable-log-requests", "--attention-backend", "flashinfer", "--kv-cache-memory-bytes", "123"},
		[]string{"--disable-log-requests", "--kv-cache-memory-bytes"},
	)
	want := []string{"--attention-backend", "flashinfer"}
	if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("omitted args = %v, want %v: a boolean flag must not swallow the next flag", got, want)
	}
}
