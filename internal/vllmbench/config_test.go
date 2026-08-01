package vllmbench

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/osolmaz/localperf/internal/artifact"
)

func TestBuildPlanAndBenchCommand(t *testing.T) {
	spec := testSpec()
	runDir := filepath.Join("runs", "example")
	plan := BuildPlan(spec, runDir)
	if len(plan) != 2 {
		t.Fatalf("plan length = %d, want 2", len(plan))
	}
	if plan[0].Profile.Name != "8k" || plan[0].Workload.Name != "prefill-8k" || plan[0].Concurrency != 4 {
		t.Fatalf("unexpected first plan row: %+v", plan[0])
	}
	command := BenchCommand(spec, plan[0])
	got := ShellQuote(command.Args)
	for _, want := range []string{
		"vllm bench serve",
		"--backend openai-chat",
		"--dataset-name random",
		"--random-input-len 8192",
		"--random-output-len 16",
		"--endpoint /v1/chat/completions",
		"--max-concurrency 4",
		"--result-filename runs/example/results/8k__prefill-8k__c4.json",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("command %q missing %q", got, want)
		}
	}
	if strings.Contains(got, "--save-detailed") {
		t.Fatalf("command %q should not include --save-detailed by default", got)
	}
}

func TestLoadGeneratorAliasesAreRejected(t *testing.T) {
	for _, alias := range []string{"vllm-bench", "vllmbench", "localperf-http", "http", "openai-http"} {
		spec := testSpec()
		spec.Workloads[0].LoadGenerator = alias
		ApplyDefaults(&spec)
		if err := ValidateSpec(spec); err == nil || !strings.Contains(err.Error(), "unsupported load_generator") {
			t.Fatalf("ValidateSpec with alias %q = %v, want unsupported load_generator", alias, err)
		}
	}
}

func TestValidateSpecRequiresCurrentVersionRoleAndContextSemantics(t *testing.T) {
	for name, mutate := range map[string]func(*Spec){
		"version":           func(spec *Spec) { spec.Version = "" },
		"role":              func(spec *Spec) { spec.Workloads[0].Role = "" },
		"context semantics": func(spec *Spec) { spec.Workloads[0].ContextSemantics = "" },
	} {
		t.Run(name, func(t *testing.T) {
			spec := testSpec()
			mutate(&spec)
			if err := ValidateSpec(spec); err == nil {
				t.Fatalf("ValidateSpec accepted missing %s", name)
			}
		})
	}
}

func TestValidateSpecAllowsSmallBenchmarkSamples(t *testing.T) {
	for _, tc := range []struct {
		concurrency    int
		numPrompts     int
		promptsPerUser int
	}{
		{concurrency: 1, numPrompts: 1},
		{concurrency: 8, promptsPerUser: 1},
	} {
		spec := testSpec()
		spec.Workloads[0].MaxConcurrency = []int{tc.concurrency}
		spec.Workloads[0].NumPrompts = tc.numPrompts
		spec.Workloads[0].PromptsPerUser = tc.promptsPerUser
		if err := ValidateSpec(spec); err != nil {
			t.Fatalf("ValidateSpec c%d = %v, want small benchmark sample accepted", tc.concurrency, err)
		}
	}
}

func TestExecuteAllowsSmallBenchmarkSamples(t *testing.T) {
	spec := testSpec()
	spec.Workloads[0].NumPrompts = 1
	spec.Workloads[0].PromptsPerUser = 0
	runDir := filepath.Join(t.TempDir(), "small-benchmark")
	if _, err := Execute(context.Background(), spec, RunOptions{DryRun: true, RunDir: runDir}); err != nil {
		t.Fatalf("Execute small benchmark: %v", err)
	}
	if _, err := os.Stat(runDir); err != nil {
		t.Fatalf("small benchmark run directory: %v", err)
	}
}

func TestValidateSpecAllowsUnmanagedEndpointOnlyProfile(t *testing.T) {
	spec := testSpec()
	spec.Profiles[0].Managed = false
	spec.Profiles[0].Port = 0
	spec.Profiles[0].EndpointBaseURL = "https://api.example.com/v1"
	if err := ValidateSpec(spec); err == nil || !strings.Contains(err.Error(), "all referenced workloads use localperf_http") {
		t.Fatalf("ValidateSpec endpoint-only vllm_bench profile = %v, want port issue", err)
	}

	withPort := testSpec()
	withPort.Profiles[0].Managed = false
	withPort.Profiles[0].EndpointBaseURL = "https://api.example.com/v1"
	if err := ValidateSpec(withPort); err == nil || !strings.Contains(err.Error(), "endpoint_base_url") {
		t.Fatalf("ValidateSpec endpoint vllm_bench profile = %v, want endpoint_base_url issue", err)
	}

	spec.Workloads[0].LoadGenerator = LoadGeneratorHTTP
	for _, endpoint := range []string{"api.example.com", "https://api.example.com/v1?x=1", "https://api.example.com/v1#frag"} {
		spec.Profiles[0].EndpointBaseURL = endpoint
		if err := ValidateSpec(spec); err == nil || !strings.Contains(err.Error(), "endpoint_base_url") {
			t.Fatalf("ValidateSpec endpoint %q = %v, want endpoint_base_url issue", endpoint, err)
		}
	}
	spec.Profiles[0].EndpointBaseURL = "https://api.example.com/v1"
	if err := ValidateSpec(spec); err != nil {
		t.Fatalf("ValidateSpec endpoint-only profile: %v", err)
	}
	if got := baseURL(spec.Profiles[0]); got != "https://api.example.com" {
		t.Fatalf("baseURL = %q, want normalized endpoint URL", got)
	}

	spec.Warmup = WarmupConfig{
		Enabled:        true,
		NumPrompts:     1,
		MaxConcurrency: 1,
		BenchmarkTrafficConfig: BenchmarkTrafficConfig{
			Backend:         "openai-chat",
			DatasetName:     "random",
			RandomInputLen:  1,
			RandomOutputLen: 1,
			RequestRate:     "inf",
		},
	}
	if err := ValidateSpec(spec); err == nil || !strings.Contains(err.Error(), "all referenced workloads use localperf_http") {
		t.Fatalf("ValidateSpec endpoint-only warmup profile = %v, want port issue", err)
	}

	spec.Warmup.Enabled = false
	spec.Profiles[0].Managed = true
	if err := ValidateSpec(spec); err == nil || !strings.Contains(err.Error(), "port must be positive") {
		t.Fatalf("ValidateSpec managed endpoint profile = %v, want port issue", err)
	}
}

func TestValidateHTTPRejectsUnsupportedDatasetName(t *testing.T) {
	spec := testSpec()
	spec.Workloads[0].LoadGenerator = LoadGeneratorHTTP
	spec.Workloads[0].BenchmarkTrafficConfig.DatasetName = "sonnet"
	spec.Workloads[0].BenchmarkTrafficConfig.RandomInputLen = 0
	spec.Workloads[0].BenchmarkTrafficConfig.RandomOutputLen = 0
	ApplyDefaults(&spec)
	err := ValidateSpec(spec)
	if err == nil || !strings.Contains(err.Error(), "localperf_http supports random or canonical structured datasets") {
		t.Fatalf("ValidateSpec error = %v, want localperf_http dataset rejection", err)
	}
	spec.Workloads[0].BenchmarkTrafficConfig.DatasetPath = "/tmp/canonical.jsonl"
	if err := ValidateSpec(spec); err != nil {
		t.Fatalf("ValidateSpec with canonical dataset path: %v", err)
	}
}

func TestBenchCommandSupportsStandardDatasetKnobs(t *testing.T) {
	spec := testSpec()
	seed := 7
	customOutputLen := -1
	shareGPTOutputLen := 256
	spec.Workloads = []Workload{{
		Name:     "standard-knobs",
		Profiles: []string{"8k"},
		BenchmarkTrafficConfig: BenchmarkTrafficConfig{
			Backend:                     "openai-chat",
			DatasetName:                 "sonnet",
			DatasetPath:                 "examples/prompts/sonnet.txt",
			SonnetInputLen:              4096,
			SonnetOutputLen:             64,
			SonnetPrefixLen:             128,
			PrefixRepetitionPrefixLen:   256,
			PrefixRepetitionSuffixLen:   512,
			PrefixRepetitionNumPrefixes: 4,
			PrefixRepetitionOutputLen:   32,
			CustomOutputLen:             &customOutputLen,
			ShareGPTOutputLen:           &shareGPTOutputLen,
			SpeedBenchDatasetSubset:     "reasoning",
			SpeedBenchOutputLen:         128,
			SpeedBenchCategory:          "math",
			Seed:                        &seed,
			DisableShuffle:              true,
			NoOversample:                true,
			SkipChatTemplate:            true,
			SaveDetailed:                boolPointer(true),
			PlotDatasetStats:            true,
			ExtraBody:                   `{"guided_decoding_backend":"outlines"}`,
			Metadata:                    []string{"suite=standard", "shape=sonnet"},
			Goodput:                     []string{"ttft:5000"},
			RequestRate:                 "inf",
			ExtraArgs:                   []string{"--request-id-prefix", "standard"},
		},
		NumPrompts:     2,
		MaxConcurrency: []int{1},
	}}
	ApplyDefaults(&spec)
	command := BenchCommand(spec, BuildPlan(spec, "runs/example")[0])
	got := ShellQuote(command.Args)
	for _, want := range []string{
		"--dataset-name sonnet",
		"--dataset-path examples/prompts/sonnet.txt",
		"--seed 7",
		"--disable-shuffle",
		"--no-oversample",
		"--skip-chat-template",
		"--save-detailed",
		"--plot-dataset-stats",
		"--custom-output-len -1",
		"--sharegpt-output-len 256",
		"--sonnet-input-len 4096",
		"--sonnet-output-len 64",
		"--sonnet-prefix-len 128",
		"--prefix-repetition-prefix-len 256",
		"--prefix-repetition-suffix-len 512",
		"--prefix-repetition-num-prefixes 4",
		"--prefix-repetition-output-len 32",
		"--speed-bench-dataset-subset reasoning",
		"--speed-bench-output-len 128",
		"--speed-bench-category math",
		"--extra-body '{\"guided_decoding_backend\":\"outlines\"}'",
		"--metadata suite=standard",
		"--metadata shape=sonnet",
		"--goodput ttft:5000",
		"--request-id-prefix standard",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("command %q missing %q", got, want)
		}
	}
}

func TestApplyDefaultsPreservesSaveDetailedOptOut(t *testing.T) {
	spec := testSpec()
	spec.Workloads[0].BenchmarkTrafficConfig.SaveDetailed = boolPointer(false)
	ApplyDefaults(&spec)
	workload := spec.Workloads[0]
	if workload.SaveDetailed == nil || *workload.SaveDetailed {
		t.Fatalf("save_detailed opt-out was not preserved: %+v", workload.BenchmarkTrafficConfig)
	}
	command := ShellQuote(BenchCommand(spec, BuildPlan(spec, "runs/example")[0]).Args)
	if strings.Contains(command, "--save-detailed") {
		t.Fatalf("command %q should not include --save-detailed", command)
	}
}

func TestApplyDefaultsNormalizesWorkloadPhase(t *testing.T) {
	spec := testSpec()
	spec.Workloads = []Workload{
		testRandomWorkload("explicit", []string{"8k"}, 128, 16, 1, []int{1}),
		testRandomWorkload("long-prefill", []string{"8k"}, 4096, 16, 1, []int{1}),
		testRandomWorkload("long-output", []string{"8k"}, 1024, 512, 1, []int{1}),
		testRandomWorkload("small-mixed", []string{"8k"}, 128, 16, 1, []int{1}),
	}
	spec.Workloads[0].Phase = "Decode Phase"

	ApplyDefaults(&spec)

	got := []string{
		spec.Workloads[0].Phase,
		spec.Workloads[1].Phase,
		spec.Workloads[2].Phase,
		spec.Workloads[3].Phase,
	}
	want := []string{"decode-phase", "prefill", "decode", "mixed"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("workload phases = %#v, want %#v", got, want)
	}
}

func TestApplyDefaultsDoesNotDuplicateEngineArgs(t *testing.T) {
	spec := testSpec()
	spec.Profiles[0].Args = []string{"--served-model-name", "alias"}
	spec.Profiles[0].EngineArgs = []string{"--disable-log-requests"}
	ApplyDefaults(&spec)
	ApplyDefaults(&spec)
	if len(spec.Profiles[0].Args) != 2 {
		t.Fatalf("profile args were mutated by defaults: %+v", spec.Profiles[0].Args)
	}
	got := ShellQuote(ServeCommand(spec, spec.Profiles[0]).Args)
	if count := strings.Count(got, "--disable-log-requests"); count != 1 {
		t.Fatalf("serve command has %d engine arg copies, want 1: %s", count, got)
	}
	if count := strings.Count(got, "--served-model-name"); count != 1 {
		t.Fatalf("serve command has %d args copies, want 1: %s", count, got)
	}
}

func TestCommandsIncludeEngineEnv(t *testing.T) {
	spec := testSpec()
	spec.Env["SPEC_ENV"] = "spec"
	spec.Engines[0].Env = map[string]string{"ENGINE_ENV": "engine"}
	spec.Profiles[0].Env = map[string]string{"PROFILE_ENV": "profile"}
	spec.Warmup.Enabled = true
	serve := ServeCommand(spec, spec.Profiles[0])
	for key, want := range map[string]string{
		"SPEC_ENV":             "spec",
		"ENGINE_ENV":           "engine",
		"PROFILE_ENV":          "profile",
		"VLLM_SERVER_DEV_MODE": "1",
	} {
		if serve.Env[key] != want {
			t.Fatalf("serve env %s = %q, want %q; env=%v", key, serve.Env[key], want, serve.Env)
		}
	}
	planned := BuildPlan(spec, "runs/example")[0]
	bench := BenchCommand(spec, planned)
	warmup := WarmupCommand(spec, spec.Profiles[0], "runs/example")
	for name, command := range map[string]CommandSpec{"bench": bench, "warmup": warmup} {
		if command.Env["SPEC_ENV"] != "spec" || command.Env["ENGINE_ENV"] != "engine" {
			t.Fatalf("%s env did not include spec and engine env: %v", name, command.Env)
		}
		if _, ok := command.Env["PROFILE_ENV"]; ok {
			t.Fatalf("%s env unexpectedly included profile env: %v", name, command.Env)
		}
	}
}

func TestCommandSummaryRedactsSensitiveEnv(t *testing.T) {
	summary := CommandSummary(CommandSpec{
		Env: map[string]string{
			"CUTE_DSL_ARCH":  "sm_121a",
			"HF_TOKEN":       "hf_secret",
			"OPENAI_API_KEY": "sk-secret",
		},
		Args: []string{"vllm", "serve", "model"},
	})
	for _, secret := range []string{"hf_secret", "sk-secret"} {
		if strings.Contains(summary, secret) {
			t.Fatalf("summary leaked secret %q: %s", secret, summary)
		}
	}
	for _, want := range []string{"HF_TOKEN='<redacted>'", "OPENAI_API_KEY='<redacted>'", "CUTE_DSL_ARCH=sm_121a"} {
		if !strings.Contains(summary, want) {
			t.Fatalf("summary %q missing %q", summary, want)
		}
	}
}

func TestValidateSpecRejectsInvalidWarmupTraffic(t *testing.T) {
	spec := testSpec()
	spec.Warmup.Enabled = true
	spec.Warmup.DatasetName = "random"
	spec.Warmup.RandomInputLen = -1
	if err := ValidateSpec(spec); err == nil || !strings.Contains(err.Error(), "warmup: random_input_len") {
		t.Fatalf("ValidateSpec error = %v, want warmup random input issue", err)
	}
}

func TestValidateSpecRejectsUnsupportedStructuredDatasetControls(t *testing.T) {
	spec := testSpec()
	spec.Workloads = []Workload{testShareGPTWorkload("sharegpt.json", []string{"8k"})}
	spec.Workloads[0].Dataset.Selection = "randm"
	if err := ValidateSpec(spec); err == nil || !strings.Contains(err.Error(), "dataset.selection") {
		t.Fatalf("ValidateSpec error = %v, want dataset.selection issue", err)
	}

	spec.Workloads[0].Dataset.Selection = "first_n"
	spec.Workloads[0].Request.TurnPolicy = "last_user_turn"
	if err := ValidateSpec(spec); err == nil || !strings.Contains(err.Error(), "request.turn_policy") {
		t.Fatalf("ValidateSpec error = %v, want request.turn_policy issue", err)
	}
}

func TestValidateSpecRequiresMemoryFloor(t *testing.T) {
	spec := testSpec()
	spec.Safety.MinMemAvailableGiB = 0
	if err := ValidateSpec(spec); err == nil || !strings.Contains(err.Error(), "min_mem_available_gib") {
		t.Fatalf("ValidateSpec error = %v, want min_mem_available_gib issue", err)
	}
}

func TestValidatePrebootOneAwakeRequiresSleepMode(t *testing.T) {
	spec := testSpec()
	spec.Runner.PrebootProfiles = true
	spec.Profiles[0].EnableSleepMode = false
	if err := ValidateSpec(spec); err == nil || !strings.Contains(err.Error(), "enable_sleep_mode") {
		t.Fatalf("ValidateSpec error = %v, want enable_sleep_mode issue", err)
	}
	oneAwake := false
	spec.Runner.OneAwakeProfile = &oneAwake
	if err := ValidateSpec(spec); err != nil {
		t.Fatalf("ValidateSpec with one_awake_profile=false = %v", err)
	}
}

func TestValidateSpecRejectsProfileSlugCollisions(t *testing.T) {
	spec := testSpec()
	colliding := spec.Profiles[0]
	colliding.Name = "8K"
	colliding.Port = 8109
	spec.Profiles = append(spec.Profiles, colliding)
	if err := ValidateSpec(spec); err == nil || !strings.Contains(err.Error(), "collides") {
		t.Fatalf("ValidateSpec error = %v, want slug collision issue", err)
	}
}

func TestValidateSpecRejectsWorkloadSlugCollisions(t *testing.T) {
	spec := testSpec()
	colliding := spec.Workloads[0]
	colliding.Name = "prefill/8k"
	spec.Workloads = append(spec.Workloads, colliding)
	if err := ValidateSpec(spec); err == nil || !strings.Contains(err.Error(), "collides") {
		t.Fatalf("ValidateSpec error = %v, want slug collision issue", err)
	}
}

func TestValidateSpecRejectsDuplicateConcurrencyValues(t *testing.T) {
	spec := testSpec()
	spec.Workloads[0].MaxConcurrency = []int{4, 4}
	if err := ValidateSpec(spec); err == nil || !strings.Contains(err.Error(), "duplicate max_concurrency") {
		t.Fatalf("ValidateSpec error = %v, want duplicate concurrency issue", err)
	}
}

func TestValidateSpecRejectsDuplicateWorkloadProfileReferences(t *testing.T) {
	spec := testSpec()
	spec.Workloads[0].Profiles = []string{"8k", "8k"}
	if err := ValidateSpec(spec); err == nil || !strings.Contains(err.Error(), "duplicate profile reference") {
		t.Fatalf("ValidateSpec error = %v, want duplicate profile reference issue", err)
	}
}

func TestValidateContextSemantics(t *testing.T) {
	cases := []struct {
		name    string
		mutate  func(*Workload)
		wantErr string
	}{
		{
			name:    "missing fields",
			mutate:  func(w *Workload) { w.ContextTarget = 0; w.ContextSemantics = "" },
			wantErr: "context_target and context_semantics are required",
		},
		{
			name:    "target without semantics",
			mutate:  func(w *Workload) { w.ContextTarget = 8192; w.ContextSemantics = "" },
			wantErr: "context_target and context_semantics are required",
		},
		{
			name:    "unknown semantics",
			mutate:  func(w *Workload) { w.ContextTarget = 8192; w.ContextSemantics = "peak" },
			wantErr: `context_semantics must be "active" or "capacity"`,
		},
		{
			// The exact conflation from the old Gemma sweep: a 32k capacity
			// server running a ~5k active workload, claimed as 32k active.
			name: "gemma shape refused",
			mutate: func(w *Workload) {
				w.ContextTarget = 32768
				w.ContextSemantics = ContextSemanticsActive
				w.RandomInputLen = 1024
				w.RandomOutputLen = 4096
			},
			wantErr: "claims active context 32768 but requests 1024+4096=5120 tokens (16% of target)",
		},
		{
			name: "active below band",
			mutate: func(w *Workload) {
				w.ContextTarget = 8192
				w.ContextSemantics = ContextSemanticsActive
				w.RandomInputLen = 7168
				w.RandomOutputLen = 16
			},
			wantErr: "claims active context 8192",
		},
		{
			name: "active above target",
			mutate: func(w *Workload) {
				w.ContextTarget = 8192
				w.ContextSemantics = ContextSemanticsActive
				w.RandomInputLen = 8192
				w.RandomOutputLen = 128
			},
			wantErr: "claims active context 8192",
		},
		{
			name: "active target above server limit",
			mutate: func(w *Workload) {
				w.ContextTarget = 16384
				w.ContextSemantics = ContextSemanticsActive
				w.RandomInputLen = 16000
				w.RandomOutputLen = 128
			},
			wantErr: "max_model_len 8192 is below context_target 16384",
		},
		{
			name: "capacity target must match server limit",
			mutate: func(w *Workload) {
				w.ContextTarget = 4096
				w.ContextSemantics = ContextSemanticsCapacity
			},
			wantErr: "requires context_target to equal profile 8k max_model_len 8192",
		},
		{
			name: "active requires random dataset",
			mutate: func(w *Workload) {
				w.ContextTarget = 8192
				w.ContextSemantics = ContextSemanticsActive
				w.DatasetName = "sharegpt"
				w.DatasetPath = "sharegpt.json"
			},
			wantErr: `context_semantics "active" requires the random dataset`,
		},
		{
			name: "active rejects ranged random lengths",
			mutate: func(w *Workload) {
				w.ContextTarget = 8192
				w.ContextSemantics = ContextSemanticsActive
				w.RandomInputLen = 8063
				w.RandomOutputLen = 1
				w.RandomRangeRatio = "0.5"
			},
			wantErr: `context_semantics "active" requires random_range_ratio 0`,
		},
		{
			name: "slo rejects explicit save_detailed false",
			mutate: func(w *Workload) {
				w.SLO = &SLOConfig{TTFTP95Millis: 500}
				saveDetailed := false
				w.SaveDetailed = &saveDetailed
			},
			wantErr: "slo requires save_detailed",
		},
		{
			name:    "slo rejects negative ttft target",
			mutate:  func(w *Workload) { w.SLO = &SLOConfig{TTFTP95Millis: -1} },
			wantErr: "slo.ttft_p95_ms must not be negative",
		},
		{
			name:    "slo rejects negative e2el target",
			mutate:  func(w *Workload) { w.SLO = &SLOConfig{E2ELP95Millis: -1} },
			wantErr: "slo.e2el_p95_ms must not be negative",
		},
		{
			name:    "slo requires at least one target",
			mutate:  func(w *Workload) { w.SLO = &SLOConfig{} },
			wantErr: "slo must set at least one target",
		},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			spec := testSpec()
			testCase.mutate(&spec.Workloads[0])
			err := ValidateSpec(spec)
			if err == nil || !strings.Contains(err.Error(), testCase.wantErr) {
				t.Fatalf("ValidateSpec error = %v, want %q", err, testCase.wantErr)
			}
		})
	}
}

func TestPromptsPerUserWorksForStructuredDatasets(t *testing.T) {
	spec := testSpec()
	workload := testCustomJSONLWorkload("structured-ppu", "requests.jsonl", []string{"8k"})
	workload.Dataset.SampleCount = 0
	workload.PromptsPerUser = 2
	workload.MaxConcurrency = []int{1, 4}
	spec.Workloads = []Workload{workload}
	ApplyDefaults(&spec)
	if err := ValidateSpec(spec); err != nil {
		t.Fatalf("ValidateSpec error = %v, want prompts_per_user to satisfy structured dataset defaults", err)
	}
	if spec.Workloads[0].Dataset.SampleCount != 8 {
		t.Fatalf("dataset sample count = %d, want 8 derived from prompts_per_user", spec.Workloads[0].Dataset.SampleCount)
	}
}

func TestStructuredScaledWorkloadSurvivesMaterialization(t *testing.T) {
	workload := testCustomJSONLWorkload("scaled-materialized", "requests.jsonl", []string{"8k"})
	workload.Dataset.SampleCount = 0
	workload.PromptsPerUser = 2
	workload.MaxConcurrency = []int{1, 4}
	applyWorkloadDefault(&workload)
	applyMaterializedDatasetToWorkload(&workload, 8, "rendered.jsonl")
	if workload.NumPrompts != 0 || workload.PromptsPerUser != 2 {
		t.Fatalf("after materialization num_prompts=%d ppu=%d, want scaling preserved", workload.NumPrompts, workload.PromptsPerUser)
	}
	spec := testSpec()
	spec.Workloads = []Workload{workload}
	ApplyDefaults(&spec)
	if err := ValidateSpec(spec); err != nil {
		t.Fatalf("ValidateSpec after materialization = %v, want valid", err)
	}
}

func TestValidateContextSemanticsAcceptsCompliantClaims(t *testing.T) {
	spec := testSpec()
	spec.Workloads[0].ContextTarget = 8192
	spec.Workloads[0].ContextSemantics = ContextSemanticsActive
	spec.Workloads[0].RandomInputLen = 8063
	spec.Workloads[0].RandomOutputLen = 1
	if err := ValidateSpec(spec); err != nil {
		t.Fatalf("ValidateSpec error = %v, want valid active claim", err)
	}
	spec.Workloads[0].ContextSemantics = ContextSemanticsCapacity
	spec.Workloads[0].RandomInputLen = 128
	spec.Workloads[0].RandomOutputLen = 16
	if err := ValidateSpec(spec); err != nil {
		t.Fatalf("ValidateSpec error = %v, want valid capacity claim", err)
	}
}

func TestApplyDefaultsHonorsSleepLevelZero(t *testing.T) {
	spec := testSpec()
	spec.Profiles[0].SleepLevel = testIntPointer(0)
	ApplyDefaults(&spec)
	if got := SleepLevelValue(spec.Profiles[0]); got != 0 {
		t.Fatalf("sleep level = %d, want explicit zero", got)
	}
}

func TestApplyDefaultsSetsOmittedSleepLevelToTwo(t *testing.T) {
	spec := testSpec()
	spec.Profiles[0].SleepLevel = nil
	ApplyDefaults(&spec)
	if got := SleepLevelValue(spec.Profiles[0]); got != 2 {
		t.Fatalf("sleep level = %d, want default level 2", got)
	}
}

func TestParseMeminfo(t *testing.T) {
	meminfo := strings.NewReader(`MemTotal:       131072000 kB
MemFree:         1000000 kB
MemAvailable:    65536000 kB
SwapFree:         4194304 kB
`)
	snapshot, err := ParseMeminfo(meminfo)
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.MemAvailableGiB != 62.5 {
		t.Fatalf("MemAvailableGiB = %v, want 62.5", snapshot.MemAvailableGiB)
	}
	if snapshot.SwapFreeGiB != 4 {
		t.Fatalf("SwapFreeGiB = %v, want 4", snapshot.SwapFreeGiB)
	}
}

func TestParseVLLMBenchResult(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "result.json")
	writeFile(t, path, `{"backend":"openai-chat","model_id":"nvidia/diffusiongemma-26B-A4B-it-NVFP4","num_prompts":4,"max_concurrency":1,"duration":13.1517,"completed":4,"failed":0,"output_throughput":311.441,"total_token_throughput":619.612,"mean_ttft_ms":2597.32}`)
	rows, err := parseResultFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 {
		t.Fatalf("rows = %d, want 1", len(rows))
	}
	row := rows[0]
	if row.Concurrency != 1 || row.Completed != 4 || row.OutputTokensPerSec != 311.441 {
		t.Fatalf("unexpected row: %+v", row)
	}
	if row.PerUserOutputTokSec != 311.441 {
		t.Fatalf("per-user throughput = %v, want 311.441", row.PerUserOutputTokSec)
	}
}

func TestRequestSamplesForResultSkipsJSONL(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "result.jsonl")
	writeFile(t, path, "{\"completed\":1}\n{\"completed\":2}\n")
	samples, err := requestSamplesForResult(dir, "result.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	if len(samples) != 0 {
		t.Fatalf("samples = %d, want 0", len(samples))
	}
}

func TestExecuteDryRunAndReport(t *testing.T) {
	spec := testSpec()
	spec.OutputDir = t.TempDir()
	appendTimestamp := false
	spec.Runner.AppendTimestampToRun = &appendTimestamp
	summary, err := Execute(context.Background(), spec, RunOptions{DryRun: true})
	if err != nil {
		t.Fatal(err)
	}
	if summary.PlannedRuns != 2 {
		t.Fatalf("planned runs = %d, want 2", summary.PlannedRuns)
	}
	for _, path := range []string{
		filepath.Join(summary.RunDir, "events.jsonl"),
		filepath.Join(summary.RunDir, "spec.normalized.json"),
		filepath.Join(summary.RunDir, "summary.json"),
	} {
		if _, err := os.Stat(path); err != nil {
			t.Fatalf("expected artifact %s: %v", path, err)
		}
	}
}

func TestExecuteDryRunStoresInternalExecutionDocumentAndPlannedCommandStatus(t *testing.T) {
	dir := t.TempDir()
	specPath := filepath.Join(dir, "spec.json")
	writeFile(t, specPath, `{
  "version": "1",
  "name": "original-spec-artifact",
  "model": "example/model",
  "env": {"HF_TOKEN": "hf_secret", "CUTE_DSL_ARCH": "sm_121a"},
  "engines": [{"name": "vllm", "type": "vllm-managed", "command": "vllm"}],
  "safety": {"min_mem_available_gib": 1},
  "profiles": [
    {
      "name": "4k",
      "engine": "vllm",
      "managed": true,
      "port": 8104,
      "max_model_len": 4096,
      "max_num_seqs": 4,
      "max_num_batched_tokens": 4096
    }
  ],
  "workloads": [
    {
      "name": "decode",
      "role": "benchmark",
      "profiles": ["4k"],
      "context_target": 4096,
      "context_semantics": "capacity",
      "dataset_name": "random",
      "random_input_len": 128,
      "random_output_len": 16,
      "num_prompts": 8,
      "max_concurrency": [1]
    }
  ]
}`)
	data, err := os.ReadFile(specPath)
	if err != nil {
		t.Fatal(err)
	}
	var spec Spec
	if err := json.Unmarshal(data, &spec); err != nil {
		t.Fatal(err)
	}
	ApplyDefaults(&spec)
	if err := ValidateSpec(spec); err != nil {
		t.Fatal(err)
	}
	summary, err := Execute(context.Background(), spec, RunOptions{
		DryRun: true,
		RunDir: filepath.Join(dir, "run"),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := artifact.Check(summary.ArtifactPath); err != nil {
		t.Fatalf("artifact check failed: %v", err)
	}
	db, err := sql.Open("sqlite", summary.ArtifactPath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	specs := map[string]string{}
	rows, err := db.Query("SELECT kind, content, sha256 FROM specs")
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var kind, content, hash string
		if err := rows.Scan(&kind, &content, &hash); err != nil {
			t.Fatal(err)
		}
		if got := sha256Hex([]byte(content)); got != hash {
			t.Fatalf("%s spec hash = %s, want %s", kind, got, hash)
		}
		specs[kind] = content
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(specs["original"], `"num_prompts": 8`) || !strings.Contains(specs["original"], `"role": "benchmark"`) {
		t.Fatalf("original spec did not preserve canonical workload fields:\n%s", specs["original"])
	}
	if !strings.Contains(specs["original"], `"max_num_batched_tokens": 4096`) {
		t.Fatalf("original spec redacted or dropped token-count field:\n%s", specs["original"])
	}
	if strings.Contains(specs["original"], "hf_secret") || !containsRedactedMarker(specs["original"]) {
		t.Fatalf("original spec did not redact env secret:\n%s", specs["original"])
	}
	if !strings.Contains(specs["original"], `"CUTE_DSL_ARCH": "sm_121a"`) {
		t.Fatalf("original spec redacted non-secret env value:\n%s", specs["original"])
	}
	if !strings.Contains(specs["normalized"], `"num_prompts": 8`) {
		t.Fatalf("normalized spec did not preserve num_prompts:\n%s", specs["normalized"])
	}
	var status string
	var exitCode sql.NullInt64
	if err := db.QueryRow("SELECT status, exit_code FROM commands WHERE phase = 'planned_run'").Scan(&status, &exitCode); err != nil {
		t.Fatal(err)
	}
	if status != "planned" || exitCode.Valid {
		t.Fatalf("planned command status=%q exit_valid=%t, want planned with null exit", status, exitCode.Valid)
	}
	var workloadClaims string
	if err := db.QueryRow("SELECT json_extract(metadata_json, '$.context.target') || ':' || json_extract(metadata_json, '$.context.semantics') FROM workloads WHERE name = 'decode'").Scan(&workloadClaims); err != nil {
		t.Fatal(err)
	}
	if workloadClaims != "4096:capacity" {
		t.Fatalf("workload context claim = %q, want 4096:capacity", workloadClaims)
	}
	var workloadPhase string
	if err := db.QueryRow("SELECT phase FROM workloads WHERE name = 'decode'").Scan(&workloadPhase); err != nil {
		t.Fatal(err)
	}
	if workloadPhase != "decode" {
		t.Fatalf("workload phase = %q, want decode", workloadPhase)
	}
	var role string
	var datasetJSON, requestJSON sql.NullString
	if err := db.QueryRow("SELECT role, dataset_json, request_json FROM workloads WHERE name = 'decode'").Scan(&role, &datasetJSON, &requestJSON); err != nil {
		t.Fatal(err)
	}
	if role != WorkloadRoleBenchmark || datasetJSON.Valid || requestJSON.Valid {
		t.Fatalf("workload role=%q dataset=%v request=%v, want benchmark and no structured data", role, datasetJSON, requestJSON)
	}
}

func TestCheckSQLiteArtifactDoesNotCreateMissingFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "missing.sqlite")
	if err := artifact.Check(path); err == nil {
		t.Fatal("artifact.Check error = nil, want missing file error")
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("artifact check created missing file or returned unexpected stat error: %v", err)
	}
}

func TestExecuteRedactsSensitiveEnvInArtifacts(t *testing.T) {
	spec := testSpec()
	spec.OutputDir = t.TempDir()
	appendTimestamp := false
	spec.Runner.AppendTimestampToRun = &appendTimestamp
	spec.Env["HF_TOKEN"] = "hf_secret"
	spec.Profiles[0].Env = map[string]string{"OPENAI_API_KEY": "sk-secret"}
	summary, err := Execute(context.Background(), spec, RunOptions{DryRun: true})
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{summary.SpecPath, summary.EventsPath} {
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		text := string(data)
		for _, secret := range []string{"hf_secret", "sk-secret"} {
			if strings.Contains(text, secret) {
				t.Fatalf("%s leaked secret %q:\n%s", path, secret, text)
			}
		}
		if !containsRedactedMarker(text) {
			t.Fatalf("%s did not contain redacted marker:\n%s", path, text)
		}
	}
}

func containsRedactedMarker(text string) bool {
	return strings.Contains(text, "<redacted>") || strings.Contains(text, `\u003credacted\u003e`)
}

func TestExecuteWithFakeVLLMEndToEnd(t *testing.T) {
	spec := testSpec()
	spec.Name = "fake-vllm-e2e"
	spec.OutputDir = t.TempDir()
	appendTimestamp := false
	spec.Runner.AppendTimestampToRun = &appendTimestamp
	configureFakeVLLM(t, &spec)
	spec.Safety.MinMemAvailableGiB = 0.1
	spec.Safety.StartupTimeoutSec = 10
	spec.Safety.WorkloadTimeoutSec = 10
	spec.Safety.HTTPTimeoutSec = 2
	spec.Warmup.Enabled = true
	spec.Profiles = spec.Profiles[:1]
	spec.Profiles[0].Port = freeTestPort()
	spec.Workloads = []Workload{testRandomWorkload("fake-random", []string{spec.Profiles[0].Name}, 128, 16, 2, []int{2})}
	ApplyDefaults(&spec)
	summary, err := Execute(context.Background(), spec, RunOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if summary.CompletedRuns != 1 || summary.FailedRuns != 0 {
		t.Fatalf("summary = %+v, want one completed run", summary)
	}
	if len(summary.Rows) != 1 {
		t.Fatalf("summary rows = %d, want 1", len(summary.Rows))
	}
	if summary.Rows[0].OutputTokensPerSec != 20 {
		t.Fatalf("output throughput = %v, want 20", summary.Rows[0].OutputTokensPerSec)
	}
	assertSQLiteArtifact(t, summary.ArtifactPath)
}

func TestExecuteRepeatsUseDistinctLogsAndMeasurements(t *testing.T) {
	spec := testSpec()
	spec.Name = "fake-vllm-repeats"
	spec.OutputDir = t.TempDir()
	appendTimestamp := false
	spec.Runner.AppendTimestampToRun = &appendTimestamp
	configureFakeVLLM(t, &spec)
	spec.Safety.MinMemAvailableGiB = 0.1
	spec.Safety.StartupTimeoutSec = 10
	spec.Safety.WorkloadTimeoutSec = 10
	spec.Safety.HTTPTimeoutSec = 2
	spec.Warmup.Enabled = false
	spec.Profiles = spec.Profiles[:1]
	spec.Profiles[0].Port = freeTestPort()
	spec.Profiles[0].EnableSleepMode = false
	spec.Workloads = []Workload{testRandomWorkload("fake-random", []string{spec.Profiles[0].Name}, 128, 16, 1, []int{1})}
	spec.Workloads[0].Repeats = 2
	ApplyDefaults(&spec)
	summary, err := Execute(context.Background(), spec, RunOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if summary.CompletedRuns != 2 || summary.FailedRuns != 0 {
		t.Fatalf("summary = %+v, want two completed repeat runs", summary)
	}
	if len(summary.Rows) != 2 || summary.Rows[0].Repeat != 0 || summary.Rows[1].Repeat != 1 {
		t.Fatalf("summary row repeats = %+v, want repeat indexes 0 and 1", summary.Rows)
	}
	for _, name := range []string{
		"8k__fake-random__c1__r1.log",
		"8k__fake-random__c1__r2.log",
	} {
		if _, err := os.Stat(filepath.Join(summary.RunDir, "logs", name)); err != nil {
			t.Fatalf("expected repeat log %s: %v", name, err)
		}
	}
	db, err := sql.Open("sqlite", summary.ArtifactPath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	rows, err := db.Query("SELECT repeat_index, status FROM measurements ORDER BY repeat_index")
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var repeats []int
	for rows.Next() {
		var repeat int
		var status string
		if err := rows.Scan(&repeat, &status); err != nil {
			t.Fatal(err)
		}
		if status != "completed" {
			t.Fatalf("repeat %d status = %s, want completed", repeat, status)
		}
		repeats = append(repeats, repeat)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if fmt.Sprint(repeats) != "[0 1]" {
		t.Fatalf("measurement repeats = %v, want [0 1]", repeats)
	}
	var logArtifacts int
	if err := db.QueryRow("SELECT COUNT(*) FROM artifacts WHERE kind = 'server_log' AND (name LIKE '%__r1.log' OR name LIKE '%__r2.log')").Scan(&logArtifacts); err != nil {
		t.Fatal(err)
	}
	if logArtifacts != 2 {
		t.Fatalf("repeat log artifacts = %d, want 2", logArtifacts)
	}
}

func TestPrepareDatasetsMaterializesShareGPTWorkload(t *testing.T) {
	dir := t.TempDir()
	datasetPath := writeShareGPTFixture(t, dir)
	spec := testSpec()
	spec.Workloads = []Workload{testShareGPTWorkload(datasetPath, []string{"8k"})}
	ApplyDefaults(&spec)
	if err := ValidateSpec(spec); err != nil {
		t.Fatal(err)
	}
	runDir := filepath.Join(dir, "run")
	if err := PrepareDatasets(context.Background(), &spec, runDir); err != nil {
		t.Fatal(err)
	}

	workload := spec.Workloads[0]
	if workload.DatasetName != "custom" || workload.NumPrompts != 1 || workload.RequestRate != "inf" {
		t.Fatalf("workload was not materialized for vLLM custom dataset: %+v", workload)
	}
	if workload.Dataset.Prepared.RequestCount != 1 || workload.Dataset.Prepared.CanonicalPath == "" || workload.Dataset.Prepared.VLLMCustomPath == "" {
		t.Fatalf("prepared dataset metadata missing: %+v", workload.Dataset.Prepared)
	}
	command := ShellQuote(BenchCommand(spec, BuildPlan(spec, runDir)[0]).Args)
	for _, want := range []string{"--dataset-name custom", "--dataset-path " + workload.Dataset.Prepared.VLLMCustomPath, "--custom-output-len -1", "--disable-shuffle", "--skip-chat-template", "--num-prompts 1", "--max-concurrency 1"} {
		if !strings.Contains(command, want) {
			t.Fatalf("command %q missing %q", command, want)
		}
	}
	canonicalRows, err := readCanonicalRequestFile(workload.Dataset.Prepared.CanonicalPath)
	if err != nil {
		t.Fatal(err)
	}
	if len(canonicalRows) != 1 || canonicalRows[0].Messages[0].Content != "Explain TTFT in one sentence." || canonicalRows[0].MaxOutputTokens != 512 {
		t.Fatalf("canonical rows = %+v", canonicalRows)
	}
	vllmData, err := os.ReadFile(workload.Dataset.Prepared.VLLMCustomPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(vllmData), `"prompt":"Explain TTFT in one sentence."`) || !strings.Contains(string(vllmData), `"output_tokens":512`) {
		t.Fatalf("vLLM custom dataset was not rendered correctly:\n%s", vllmData)
	}
}

func TestInternalHTTPPlanUsesPreparedCanonicalDataset(t *testing.T) {
	dir := t.TempDir()
	datasetPath := writeShareGPTFixture(t, dir)
	spec := testSpec()
	spec.Workloads = []Workload{testShareGPTWorkload(datasetPath, []string{"8k"})}
	spec.Workloads[0].LoadGenerator = LoadGeneratorHTTP
	spec.Workloads[0].ExtraBody = `{"guided_decoding_backend":"outlines"}`
	ApplyDefaults(&spec)
	if err := ValidateSpec(spec); err != nil {
		t.Fatal(err)
	}

	runDir := filepath.Join(dir, "run")
	if err := PrepareDatasets(context.Background(), &spec, runDir); err != nil {
		t.Fatal(err)
	}
	workload := spec.Workloads[0]
	command := ShellQuote(LoadCommand(spec, BuildPlan(spec, runDir)[0]).Args)
	for _, want := range []string{
		"internal:http-load",
		"--dataset-name custom",
		"--dataset-path " + workload.Dataset.Prepared.CanonicalPath,
		"--min-mem-available-gib 40",
		"--num-prompts 1",
		"--max-concurrency 1",
		"--extra-body '{\"guided_decoding_backend\":\"outlines\"}'",
	} {
		if !strings.Contains(command, want) {
			t.Fatalf("command %q missing %q", command, want)
		}
	}
	if strings.Contains(command, "localperf bench http-load") {
		t.Fatalf("internal HTTP plan exposed a runnable raw-load command: %q", command)
	}
	if strings.Contains(command, workload.Dataset.Prepared.VLLMCustomPath) {
		t.Fatalf("internal HTTP plan should use canonical dataset path, got %q", command)
	}
}

func TestInternalHTTPPlanUsesDirectDatasetPath(t *testing.T) {
	spec := testSpec()
	spec.Workloads = []Workload{testRandomWorkload("http-custom", []string{"8k"}, 0, 8, 1, []int{1})}
	spec.Workloads[0].LoadGenerator = LoadGeneratorHTTP
	spec.Workloads[0].BenchmarkTrafficConfig.DatasetName = "custom"
	spec.Workloads[0].BenchmarkTrafficConfig.DatasetPath = "/tmp/direct.canonical.jsonl"
	spec.Safety.WorkloadTimeoutSec = 7
	ApplyDefaults(&spec)
	command := ShellQuote(LoadCommand(spec, BuildPlan(spec, t.TempDir())[0]).Args)
	if !strings.Contains(command, "--dataset-path /tmp/direct.canonical.jsonl") {
		t.Fatalf("command %q missing direct dataset path", command)
	}
	if !strings.Contains(command, "--timeout 7s") {
		t.Fatalf("command %q missing workload timeout", command)
	}
}

func TestPrepareDatasetsAllowsPerRowCustomOutputTokens(t *testing.T) {
	dir := t.TempDir()
	datasetPath := filepath.Join(dir, "custom.jsonl")
	writeFile(t, datasetPath, `{"id":"one","prompt":"hello","max_output_tokens":3}
`)
	spec := testSpec()
	spec.Workloads = []Workload{testCustomJSONLWorkload("row-output", datasetPath, []string{"8k"})}
	spec.Workloads[0].Request.MaxOutputTokens = 0
	ApplyDefaults(&spec)
	if err := ValidateSpec(spec); err != nil {
		t.Fatal(err)
	}

	runDir := filepath.Join(dir, "run")
	if err := PrepareDatasets(context.Background(), &spec, runDir); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(spec.Workloads[0].Dataset.Prepared.VLLMCustomPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), `"output_tokens":3`) {
		t.Fatalf("vLLM custom dataset did not preserve row output length:\n%s", data)
	}
}

func TestPrepareDatasetsKeepsRequestRate(t *testing.T) {
	dir := t.TempDir()
	datasetPath := writeShareGPTFixture(t, dir)
	spec := testSpec()
	spec.Workloads = []Workload{testShareGPTWorkload(datasetPath, []string{"8k"})}
	spec.Workloads[0].BenchmarkTrafficConfig.RequestRate = "5"
	ApplyDefaults(&spec)
	if err := ValidateSpec(spec); err != nil {
		t.Fatal(err)
	}

	runDir := filepath.Join(dir, "run")
	if err := PrepareDatasets(context.Background(), &spec, runDir); err != nil {
		t.Fatal(err)
	}
	if got := spec.Workloads[0].RequestRate; got != "5" {
		t.Fatalf("request rate = %q, want explicit traffic value 5", got)
	}
}

func TestPrepareDatasetsHonorsCompletionRequestMode(t *testing.T) {
	dir := t.TempDir()
	datasetPath := writeShareGPTFixture(t, dir)
	spec := testSpec()
	spec.Workloads = []Workload{testShareGPTWorkload(datasetPath, []string{"8k"})}
	spec.Workloads[0].Request.Mode = "completion"
	ApplyDefaults(&spec)
	if err := ValidateSpec(spec); err != nil {
		t.Fatal(err)
	}

	runDir := filepath.Join(dir, "run")
	if err := PrepareDatasets(context.Background(), &spec, runDir); err != nil {
		t.Fatal(err)
	}
	command := ShellQuote(BenchCommand(spec, BuildPlan(spec, runDir)[0]).Args)
	for _, want := range []string{"--backend openai", "--endpoint /v1/completions"} {
		if !strings.Contains(command, want) {
			t.Fatalf("command %q missing %q", command, want)
		}
	}
	if strings.Contains(command, "--skip-chat-template") {
		t.Fatalf("completion-mode command should not skip chat template: %q", command)
	}
}

func TestPrepareDatasetsPreservesCompletionSkipChatTemplate(t *testing.T) {
	dir := t.TempDir()
	datasetPath := writeShareGPTFixture(t, dir)
	spec := testSpec()
	spec.Workloads = []Workload{testShareGPTWorkload(datasetPath, []string{"8k"})}
	spec.Workloads[0].Request.Mode = "completion"
	spec.Workloads[0].BenchmarkTrafficConfig.SkipChatTemplate = true
	ApplyDefaults(&spec)
	if err := ValidateSpec(spec); err != nil {
		t.Fatal(err)
	}

	runDir := filepath.Join(dir, "run")
	if err := PrepareDatasets(context.Background(), &spec, runDir); err != nil {
		t.Fatal(err)
	}
	command := ShellQuote(BenchCommand(spec, BuildPlan(spec, runDir)[0]).Args)
	if !strings.Contains(command, "--skip-chat-template") {
		t.Fatalf("completion-mode command did not preserve skip_chat_template: %q", command)
	}
}

func TestPrepareDatasetsRejectsUnsupportedRequestMode(t *testing.T) {
	dir := t.TempDir()
	datasetPath := writeShareGPTFixture(t, dir)
	spec := testSpec()
	spec.Workloads = []Workload{testShareGPTWorkload(datasetPath, []string{"8k"})}
	spec.Workloads[0].Request.Mode = "embedding"
	ApplyDefaults(&spec)
	if err := ValidateSpec(spec); err != nil {
		t.Fatal(err)
	}

	err := PrepareDatasets(context.Background(), &spec, filepath.Join(dir, "run"))
	if err == nil || !strings.Contains(err.Error(), `request.mode "embedding"`) {
		t.Fatalf("unsupported mode error = %v", err)
	}
}

func TestSQLiteArtifactIgnoresStaleDatasetFiles(t *testing.T) {
	dir := t.TempDir()
	runDir := filepath.Join(dir, "run")
	staleDatasetDir := filepath.Join(runDir, "datasets")
	writeFile(t, filepath.Join(staleDatasetDir, "stale.canonical.jsonl"), `{"id":"stale","prompt":"old","max_output_tokens":1}
`)
	writeFile(t, filepath.Join(staleDatasetDir, "stale.vllm-custom.jsonl"), `{"prompt":"old","output_tokens":1}
`)
	spec := testSpec()
	spec.OutputDir = dir
	appendTimestamp := false
	spec.Runner.AppendTimestampToRun = &appendTimestamp
	spec.Safety.MinMemAvailableGiB = 0.1
	spec.Profiles = spec.Profiles[:1]
	spec.Profiles[0].Managed = false
	spec.Profiles[0].Port = freeTestPort()
	spec.Workloads = []Workload{testRandomWorkload("random", []string{spec.Profiles[0].Name}, 128, 16, 1, []int{1})}

	summary, err := Execute(context.Background(), spec, RunOptions{DryRun: true, RunDir: runDir})
	if err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", summary.ArtifactPath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var datasetArtifacts int
	if err := db.QueryRow("SELECT COUNT(*) FROM artifacts WHERE kind IN ('canonical_dataset','engine_dataset')").Scan(&datasetArtifacts); err != nil {
		t.Fatal(err)
	}
	if datasetArtifacts != 0 {
		t.Fatalf("dataset artifacts = %d, want 0 for random workload with stale files", datasetArtifacts)
	}
}

func TestPrepareDatasetsRejectsRawPayloadVLLMBenchRenderer(t *testing.T) {
	dir := t.TempDir()
	datasetPath := filepath.Join(dir, "raw.jsonl")
	writeFile(t, datasetPath, `{"messages":[{"role":"user","content":"hello"}],"max_tokens":3,"response_format":{"type":"json_object"}}
`)
	spec := testSpec()
	spec.Workloads = []Workload{testRawPayloadWorkload("raw", datasetPath, []string{"8k"})}
	ApplyDefaults(&spec)
	if err := ValidateSpec(spec); err != nil {
		t.Fatal(err)
	}

	err := PrepareDatasets(context.Background(), &spec, filepath.Join(dir, "run"))
	if err == nil || !strings.Contains(err.Error(), "raw_payload cannot be rendered") {
		t.Fatalf("raw payload renderer error = %v", err)
	}
}

func TestExecuteWithShareGPTDatasetStoresCanonicalArtifactRows(t *testing.T) {
	dir := t.TempDir()
	datasetPath := writeShareGPTFixture(t, dir)
	spec := testSpec()
	spec.Name = "fake-vllm-sharegpt"
	spec.OutputDir = dir
	appendTimestamp := false
	spec.Runner.AppendTimestampToRun = &appendTimestamp
	configureFakeVLLM(t, &spec)
	spec.Safety.MinMemAvailableGiB = 0.1
	spec.Safety.StartupTimeoutSec = 10
	spec.Safety.WorkloadTimeoutSec = 10
	spec.Safety.HTTPTimeoutSec = 2
	spec.Warmup.Enabled = false
	spec.Profiles = spec.Profiles[:1]
	spec.Profiles[0].Port = freeTestPort()
	spec.Profiles[0].EnableSleepMode = false
	spec.Workloads = []Workload{testShareGPTWorkload(datasetPath, []string{spec.Profiles[0].Name})}
	spec.Workloads[0].CapturePayloadArtifacts = true
	ApplyDefaults(&spec)

	summary, err := Execute(context.Background(), spec, RunOptions{})
	if err != nil {
		events, _ := os.ReadFile(summary.EventsPath)
		t.Fatalf("Execute: %v (run dir %s)\n%s", err, summary.RunDir, events)
	}
	if summary.CompletedRuns != 1 || summary.FailedRuns != 0 {
		t.Fatalf("summary = %+v, want one completed run", summary)
	}
	assertSQLiteArtifact(t, summary.ArtifactPath)
	db, err := sql.Open("sqlite", summary.ArtifactPath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	for table, want := range map[string]int{"datasets": 1, "source_records": 1, "canonical_requests": 1} {
		var got int
		if err := db.QueryRow("SELECT COUNT(*) FROM " + table).Scan(&got); err != nil {
			t.Fatal(err)
		}
		if got != want {
			t.Fatalf("%s rows = %d, want %d", table, got, want)
		}
	}
	var datasetType, mode string
	var maxOutput int
	if err := db.QueryRow(`SELECT d.type, cr.mode, cr.max_output_tokens
		FROM datasets d JOIN canonical_requests cr ON cr.dataset_id = d.id`).Scan(&datasetType, &mode, &maxOutput); err != nil {
		t.Fatal(err)
	}
	if datasetType != "sharegpt" || mode != "chat" || maxOutput != 512 {
		t.Fatalf("dataset/request row = type %q mode %q max_output %d", datasetType, mode, maxOutput)
	}
	var datasetArtifacts int
	if err := db.QueryRow("SELECT COUNT(*) FROM artifacts WHERE kind IN ('canonical_dataset', 'engine_dataset')").Scan(&datasetArtifacts); err != nil {
		t.Fatal(err)
	}
	if datasetArtifacts != 2 {
		t.Fatalf("dataset artifacts = %d, want 2", datasetArtifacts)
	}
}

func TestExecuteWithShareGPTDatasetSkipsPayloadArtifactsByDefault(t *testing.T) {
	dir := t.TempDir()
	datasetPath := writeShareGPTFixture(t, dir)
	spec := testSpec()
	spec.Name = "sharegpt-default-private"
	spec.OutputDir = dir
	appendTimestamp := false
	spec.Runner.AppendTimestampToRun = &appendTimestamp
	spec.Safety.MinMemAvailableGiB = 0.1
	spec.Profiles = spec.Profiles[:1]
	spec.Profiles[0].Managed = false
	spec.Workloads = []Workload{testShareGPTWorkload(datasetPath, []string{spec.Profiles[0].Name})}
	ApplyDefaults(&spec)

	summary, err := Execute(context.Background(), spec, RunOptions{DryRun: true, RunDir: filepath.Join(dir, "run")})
	if err != nil {
		t.Fatal(err)
	}
	if err := artifact.Check(summary.ArtifactPath); err != nil {
		t.Fatalf("artifact check failed: %v", err)
	}
	db, err := sql.Open("sqlite", summary.ArtifactPath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	for table, want := range map[string]int{"datasets": 1, "source_records": 0, "canonical_requests": 0} {
		var got int
		if err := db.QueryRow("SELECT COUNT(*) FROM " + table).Scan(&got); err != nil {
			t.Fatal(err)
		}
		if got != want {
			t.Fatalf("%s rows = %d, want %d", table, got, want)
		}
	}
	var datasetArtifacts int
	if err := db.QueryRow("SELECT COUNT(*) FROM artifacts WHERE kind IN ('canonical_dataset', 'engine_dataset')").Scan(&datasetArtifacts); err != nil {
		t.Fatal(err)
	}
	if datasetArtifacts != 0 {
		t.Fatalf("dataset artifacts = %d, want 0 without payload capture", datasetArtifacts)
	}
}

func TestExecuteStructuredSyntheticDatasetEnrichesSummaryRows(t *testing.T) {
	dir := t.TempDir()
	spec := testSpec()
	spec.Name = "structured-synthetic-summary"
	spec.OutputDir = dir
	appendTimestamp := false
	spec.Runner.AppendTimestampToRun = &appendTimestamp
	configureFakeVLLM(t, &spec)
	spec.Safety.MinMemAvailableGiB = 0.1
	spec.Safety.StartupTimeoutSec = 10
	spec.Safety.WorkloadTimeoutSec = 10
	spec.Safety.HTTPTimeoutSec = 2
	spec.Warmup.Enabled = false
	spec.Profiles = spec.Profiles[:1]
	spec.Profiles[0].Port = freeTestPort()
	spec.Profiles[0].EnableSleepMode = false
	spec.Workloads = []Workload{{
		Name:             "structured-synthetic",
		Role:             WorkloadRoleDiagnostic,
		Profiles:         []string{spec.Profiles[0].Name},
		ContextTarget:    8192,
		ContextSemantics: ContextSemanticsCapacity,
		Dataset: DatasetSpec{
			Type:         "synthetic",
			SampleCount:  1,
			InputTokens:  8192,
			OutputTokens: 16,
		},
		Request:                RequestSpec{Mode: "chat", MaxOutputTokens: 16},
		LoadGenerator:          LoadGeneratorVLLMBench,
		MaxConcurrency:         []int{1},
		BenchmarkTrafficConfig: BenchmarkTrafficConfig{RequestRate: "inf"},
	}}
	ApplyDefaults(&spec)

	summary, err := Execute(context.Background(), spec, RunOptions{})
	if err != nil {
		events, _ := os.ReadFile(summary.EventsPath)
		t.Fatalf("Execute: %v (run dir %s)\n%s", err, summary.RunDir, events)
	}
	if len(summary.Rows) != 1 {
		t.Fatalf("summary rows = %d, want 1", len(summary.Rows))
	}
	row := summary.Rows[0]
	if row.InputLen != 8192 || row.OutputLen != 16 || row.Phase != "prefill" {
		t.Fatalf("summary row was not enriched from structured workload: %+v", row)
	}
}

func TestDryRunStoresDuplicateCustomRequestIDsAcrossWorkloads(t *testing.T) {
	dir := t.TempDir()
	datasetPath := filepath.Join(dir, "custom.jsonl")
	writeFile(t, datasetPath, `{"id":"one","prompt":"hello","max_output_tokens":1}
`)
	spec := testSpec()
	spec.Name = "duplicate-custom-request-ids"
	spec.OutputDir = dir
	appendTimestamp := false
	spec.Runner.AppendTimestampToRun = &appendTimestamp
	spec.Safety.MinMemAvailableGiB = 0.1
	spec.Profiles = spec.Profiles[:1]
	spec.Profiles[0].Managed = false
	spec.Profiles[0].Port = freeTestPort()
	spec.Workloads = []Workload{
		testCustomJSONLWorkload("w1", datasetPath, []string{spec.Profiles[0].Name}),
		testCustomJSONLWorkload("w2", datasetPath, []string{spec.Profiles[0].Name}),
	}
	spec.Workloads[0].CapturePayloadArtifacts = true
	spec.Workloads[1].CapturePayloadArtifacts = true

	summary, err := Execute(context.Background(), spec, RunOptions{DryRun: true, RunDir: filepath.Join(dir, "run")})
	if err != nil {
		t.Fatal(err)
	}
	if summary.ArtifactPath == "" {
		t.Fatal("summary artifact path is empty")
	}
	db, err := sql.Open("sqlite", summary.ArtifactPath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var rowCount, distinctRequestIDs int
	if err := db.QueryRow("SELECT COUNT(*), COUNT(DISTINCT request_id) FROM canonical_requests").Scan(&rowCount, &distinctRequestIDs); err != nil {
		t.Fatal(err)
	}
	if rowCount != 2 || distinctRequestIDs != 1 {
		t.Fatalf("canonical request rows = %d distinct request ids = %d, want 2 and 1", rowCount, distinctRequestIDs)
	}
}

func TestRawResultArtifactNameAvoidsHyphenCollisions(t *testing.T) {
	first := rawResultArtifactName(Event{Profile: "a-b", Workload: "c", Concurrency: 1})
	second := rawResultArtifactName(Event{Profile: "a", Workload: "b-c", Concurrency: 1})
	if first == second {
		t.Fatalf("raw result artifact names collide: %s", first)
	}
	if !strings.Contains(first, "a-b__c") || !strings.Contains(second, "a__b-c") {
		t.Fatalf("artifact names do not preserve component boundaries: %q %q", first, second)
	}
}

func assertSQLiteArtifact(t *testing.T, path string) {
	t.Helper()
	if path == "" {
		t.Fatal("summary artifact path is empty")
	}
	if err := artifact.Check(path); err != nil {
		t.Fatalf("artifact check failed: %v", err)
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	for table, want := range map[string]int{
		"run":              1,
		"specs":            2,
		"engines":          1,
		"profiles":         1,
		"workloads":        1,
		"measurements":     1,
		"metric_stats":     2,
		"artifacts":        1,
		"events":           1,
		"telemetry_series": 1,
	} {
		var got int
		if err := db.QueryRow("SELECT COUNT(*) FROM " + table).Scan(&got); err != nil {
			t.Fatalf("count %s: %v", table, err)
		}
		if got < want {
			t.Fatalf("%s rows = %d, want at least %d", table, got, want)
		}
	}
	var outputThroughput float64
	if err := db.QueryRow("SELECT aggregate_output_tok_s FROM measurements LIMIT 1").Scan(&outputThroughput); err != nil {
		t.Fatal(err)
	}
	if outputThroughput <= 0 {
		t.Fatalf("artifact aggregate_output_tok_s = %v, want positive value", outputThroughput)
	}
}

func TestExecuteFailsWhenBenchmarkReportsRequestFailures(t *testing.T) {
	spec := testSpec()
	spec.Name = "fake-vllm-request-failure"
	spec.OutputDir = t.TempDir()
	appendTimestamp := false
	spec.Runner.AppendTimestampToRun = &appendTimestamp
	configureFakeVLLM(t, &spec)
	spec.Env["FAKE_BENCH_FAILED"] = "1"
	spec.Safety.MinMemAvailableGiB = 0.1
	spec.Safety.StartupTimeoutSec = 10
	spec.Safety.WorkloadTimeoutSec = 10
	spec.Safety.HTTPTimeoutSec = 2
	spec.Warmup.Enabled = false
	spec.Profiles = spec.Profiles[:1]
	spec.Profiles[0].Port = freeTestPort()
	spec.Profiles[0].EnableSleepMode = false
	spec.Workloads = []Workload{testRandomWorkload("fake-random", []string{spec.Profiles[0].Name}, 128, 16, 2, []int{2})}
	ApplyDefaults(&spec)
	summary, err := Execute(context.Background(), spec, RunOptions{})
	if err == nil || !strings.Contains(err.Error(), "benchmark run") {
		t.Fatalf("Execute error = %v, want failed benchmark run", err)
	}
	if summary.CompletedRuns != 0 || summary.FailedRuns != 1 {
		t.Fatalf("summary = %+v, want failed workload", summary)
	}
	events, err := os.ReadFile(filepath.Join(summary.RunDir, "events.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(events), "failed request") {
		t.Fatalf("events did not record failed request:\n%s", events)
	}
}

func TestExecuteHTTPRecordsRequestSamples(t *testing.T) {
	server, host, port := fakeOpenAIServer(t)
	defer server.Close()
	spec := httpTestSpec(t, host, port, "localperf-http-request-samples", 3, 2)
	summary, err := Execute(context.Background(), spec, RunOptions{})
	if err != nil {
		t.Fatalf("Execute error = %v", err)
	}
	if summary.CompletedRuns != 1 || summary.FailedRuns != 0 {
		t.Fatalf("summary = %+v, want one completed run", summary)
	}
	if len(summary.Rows) != 1 {
		t.Fatalf("summary rows = %d, want 1", len(summary.Rows))
	}
	row := summary.Rows[0]
	if row.OutputTokensPerSec <= 0 || row.OutputTokSecStdDev <= 0 {
		t.Fatalf("row throughput = %+v, want positive aggregate and stddev", row)
	}
	if row.PromptTokens != 192 || row.CompletionTokens != 24 || row.TotalTokens != 216 {
		t.Fatalf("row tokens = prompt %d completion %d total %d, want 192/24/216", row.PromptTokens, row.CompletionTokens, row.TotalTokens)
	}
	db, err := sql.Open("sqlite", summary.ArtifactPath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var requestRows, completionTokens int
	if err := db.QueryRow("SELECT COUNT(*), COALESCE(SUM(completion_tokens), 0) FROM requests").Scan(&requestRows, &completionTokens); err != nil {
		t.Fatal(err)
	}
	if requestRows != 3 || completionTokens != 24 {
		t.Fatalf("requests rows = %d completion tokens = %d, want 3/24", requestRows, completionTokens)
	}
	var stddev float64
	var count int
	if err := db.QueryRow("SELECT stddev, count FROM metric_stats WHERE metric = 'request_output_throughput'").Scan(&stddev, &count); err != nil {
		t.Fatal(err)
	}
	if stddev <= 0 || count != 3 {
		t.Fatalf("request_output_throughput stddev/count = %v/%d, want positive/3", stddev, count)
	}
	var ttftSource string
	if err := db.QueryRow(`SELECT COALESCE(json_extract(metadata_json, '$.ttft_source'), '') FROM measurements LIMIT 1`).Scan(&ttftSource); err != nil {
		t.Fatal(err)
	}
	if ttftSource != TTFTSourceStream {
		t.Fatalf("measurement ttft_source = %q, want %q", ttftSource, TTFTSourceStream)
	}
	var ttftMean, ttftP50, ttftP95, ttftP99 float64
	if err := db.QueryRow(`SELECT mean, COALESCE(p50, 0), COALESCE(p95, 0), COALESCE(p99, 0)
		FROM metric_stats WHERE metric = 'ttft'`).Scan(&ttftMean, &ttftP50, &ttftP95, &ttftP99); err != nil {
		t.Fatal(err)
	}
	if ttftMean <= 0 || ttftP50 <= 0 || ttftP95 <= 0 || ttftP99 <= 0 {
		t.Fatalf("ttft stats = %.1f/%.1f/%.1f/%.1f, want all positive from streamed samples", ttftMean, ttftP50, ttftP95, ttftP99)
	}
}

func TestExecuteHTTPNoStreamPersistsNoTTFT(t *testing.T) {
	server, host, port := fakeOpenAIServer(t)
	defer server.Close()
	spec := httpTestSpec(t, host, port, "localperf-http-no-stream", 2, 1)
	stream := false
	for index := range spec.Workloads {
		spec.Workloads[index].Stream = &stream
	}
	summary, err := Execute(context.Background(), spec, RunOptions{})
	if err != nil {
		t.Fatalf("Execute error = %v", err)
	}
	db, err := sql.Open("sqlite", summary.ArtifactPath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var ttftRows int
	if err := db.QueryRow(`SELECT COUNT(*) FROM metric_stats WHERE metric IN ('ttft', 'request_ttft')`).Scan(&ttftRows); err != nil {
		t.Fatal(err)
	}
	if ttftRows != 0 {
		t.Fatalf("ttft metric rows = %d, want none for a non-streamed run", ttftRows)
	}
	var metadata sql.NullString
	if err := db.QueryRow(`SELECT metadata_json FROM measurements LIMIT 1`).Scan(&metadata); err != nil {
		t.Fatal(err)
	}
	if metadata.Valid && strings.Contains(metadata.String, "ttft_source") {
		t.Fatalf("measurement metadata = %q, want no ttft_source marker", metadata.String)
	}
}

func TestExecuteHTTPPreservesZeroTokenArtifacts(t *testing.T) {
	server, host, port := fakeOpenAIServerWithUsage(t, 12, 0, 12)
	defer server.Close()
	spec := httpTestSpec(t, host, port, "localperf-http-zero-tokens", 1, 1)
	// A zero-completion-token response only exists on the non-streamed
	// path: a stream with no content chunks is a shape failure.
	stream := false
	for index := range spec.Workloads {
		spec.Workloads[index].Stream = &stream
	}
	summary, err := Execute(context.Background(), spec, RunOptions{})
	if err != nil {
		t.Fatalf("Execute error = %v", err)
	}
	if summary.CompletedRuns != 1 || summary.FailedRuns != 0 {
		t.Fatalf("summary = %+v, want one completed run", summary)
	}
	db, err := sql.Open("sqlite", summary.ArtifactPath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	assertZeroTokenArtifactRows(t, db)
}

func TestExecuteHTTPFailedSamplesRemainReportable(t *testing.T) {
	server, host, port := fakeOpenAIErrorServer(t)
	defer server.Close()
	spec := httpTestSpec(t, host, port, "localperf-http-failed-samples", 1, 1)
	summary, err := Execute(context.Background(), spec, RunOptions{})
	if err == nil || !strings.Contains(err.Error(), "benchmark run") {
		t.Fatalf("Execute error = %v, want benchmark run failure", err)
	}
	if summary.FailedRuns != 1 {
		t.Fatalf("summary = %+v, want one failed run", summary)
	}
	events, err := os.ReadFile(summary.EventsPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(events), `"result_written":true`) || !strings.Contains(string(events), "localperf_http result reported 1 failed request") {
		t.Fatalf("events did not mark failed HTTP samples as errored:\n%s", events)
	}
	db, err := sql.Open("sqlite", summary.ArtifactPath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var status string
	var completedRequests, failedRequests int
	if err := db.QueryRow("SELECT status, completed_requests, failed_requests FROM measurements LIMIT 1").Scan(&status, &completedRequests, &failedRequests); err != nil {
		t.Fatal(err)
	}
	if status != "failed" || completedRequests != 0 || failedRequests != 1 {
		t.Fatalf("measurement status/completed/failed = %s/%d/%d, want failed/0/1", status, completedRequests, failedRequests)
	}
	var requestRows int
	if err := db.QueryRow("SELECT COUNT(*) FROM requests").Scan(&requestRows); err != nil {
		t.Fatal(err)
	}
	if requestRows != 1 {
		t.Fatalf("request rows = %d, want failed sample imported", requestRows)
	}
}

func TestInsertMeasurementPreservesUnknownTokenNulls(t *testing.T) {
	db, err := createSQLiteArtifact(filepath.Join(t.TempDir(), "artifact.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := withSQLiteTx(db, func(tx *sql.Tx) error {
		seedMeasurementParents(t, tx)
		_, err := insertMeasurement(tx, measurementInsert{
			runID: "run",
			planned: PlannedRun{
				Profile:     Profile{Name: "profile"},
				Workload:    Workload{Name: "workload", NumPrompts: 1},
				Concurrency: 1,
			},
			row:    ReportRow{Completed: 1},
			status: "completed",
		})
		return err
	}); err != nil {
		t.Fatal(err)
	}
	var promptTokens, completionTokens, totalTokens sql.NullInt64
	if err := db.QueryRow("SELECT prompt_tokens, completion_tokens, total_tokens FROM measurements LIMIT 1").Scan(&promptTokens, &completionTokens, &totalTokens); err != nil {
		t.Fatal(err)
	}
	if promptTokens.Valid || completionTokens.Valid || totalTokens.Valid {
		t.Fatalf("token fields = %v/%v/%v, want NULLs for unknown token totals", promptTokens, completionTokens, totalTokens)
	}
}

func TestInsertMeasurementDetailsReadsPlannedResultFile(t *testing.T) {
	runDir := t.TempDir()
	resultFile := filepath.Join("results", "failed.json")
	writeFile(t, filepath.Join(runDir, resultFile), `{
  "request_samples": [
    {"request_index":0,"status":"failed","started_at":"2026-01-01T00:00:00Z","error_type":"http_status"}
  ]
}`)
	db, err := createSQLiteArtifact(filepath.Join(t.TempDir(), "artifact.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := withSQLiteTx(db, func(tx *sql.Tx) error {
		seedMeasurementParents(t, tx)
		id, err := insertMeasurement(tx, measurementInsert{
			runID: "run",
			planned: PlannedRun{
				Profile:     Profile{Name: "profile"},
				Workload:    Workload{Name: "workload", NumPrompts: 1},
				Concurrency: 1,
			},
			status: "failed",
		})
		if err != nil {
			return err
		}
		return insertMeasurementDetails(tx, runDir, id, ReportRow{}, resultFile)
	}); err != nil {
		t.Fatal(err)
	}
	var requestRows int
	if err := db.QueryRow("SELECT COUNT(*) FROM requests").Scan(&requestRows); err != nil {
		t.Fatal(err)
	}
	if requestRows != 1 {
		t.Fatalf("request rows = %d, want planned result sample imported", requestRows)
	}
}

func TestRowsByMeasurementRequiresResultWrittenForErroredEvents(t *testing.T) {
	runDir := t.TempDir()
	resultFile := filepath.Join("results", "partial.json")
	writeFile(t, filepath.Join(runDir, resultFile), `{"completed":1,"failed":1}`)
	event := Event{
		Timestamp:   time.Now().UTC(),
		Type:        "workload_finish",
		Profile:     "profile",
		Workload:    "workload",
		Concurrency: 1,
		ResultFile:  resultFile,
		Error:       "failed before writing a result",
	}
	if rows := rowsByMeasurement(runDir, []Event{event}); len(rows) != 0 {
		t.Fatalf("rows = %+v, want errored event without result_written ignored", rows)
	}

	event.Details = mustJSON(map[string]any{"result_written": true})
	rows := rowsByMeasurement(runDir, []Event{event})
	row := rows[measurementKey("profile", "workload", 1, 0)]
	if row.Completed != 1 || row.Failed != 1 {
		t.Fatalf("row completed/failed = %d/%d, want partial result imported", row.Completed, row.Failed)
	}
}

func TestEventHasArtifactResultIncludesWarmup(t *testing.T) {
	event := Event{Type: "warmup_finish", ResultFile: filepath.Join("results", "warmup.json"), Error: "warmup result failed validation"}
	if !eventHasArtifactResult(event) {
		t.Fatal("warmup result event should be preserved as a raw artifact")
	}
	if eventHasImportableResult(event) {
		t.Fatal("warmup result event should not be imported as a measurement row")
	}

	event = Event{Type: "workload_finish", ResultFile: filepath.Join("results", "failed.json"), Error: "external benchmark failed"}
	if !eventHasArtifactResult(event) {
		t.Fatal("failed workload result should be preserved as a raw artifact")
	}
	if eventHasImportableResult(event) {
		t.Fatal("failed workload result without result_written should not be imported as a measurement row")
	}
}

func TestMeasurementRawArtifactIDLinksFailedResultArtifact(t *testing.T) {
	planned := PlannedRun{
		Profile:     Profile{Name: "profile"},
		Workload:    Workload{Name: "workload"},
		Concurrency: 1,
	}
	resultFile := filepath.Join("results", "failed.json")
	events := []Event{{
		Type:        "workload_finish",
		Profile:     "profile",
		Workload:    "workload",
		Concurrency: 1,
		ResultFile:  resultFile,
		Error:       "external benchmark failed",
	}}
	if got := measurementRawArtifactID(events, planned, map[string]int64{resultFile: 42}); got != 42 {
		t.Fatalf("raw artifact id = %d, want failed result artifact linked", got)
	}
	if got := measurementResultFile(events, planned); got != "" {
		t.Fatalf("measurement result file = %q, want no sample import for unmarked failed result", got)
	}
}

func TestArtifactIDForPathVariants(t *testing.T) {
	ids := map[string]int64{
		"results/result.json":                  7,
		filepath.Clean("nested/../clean.json"): 11,
	}
	for _, tt := range []struct {
		path string
		want int64
	}{
		{"", 0},
		{"results/result.json", 7},
		{"./results/result.json", 7},
		{"nested/../clean.json", 11},
		{"missing.json", 0},
	} {
		if got := artifactIDForPath(ids, tt.path); got != tt.want {
			t.Fatalf("artifactIDForPath(%q) = %d, want %d", tt.path, got, tt.want)
		}
	}
}

func seedMeasurementParents(t *testing.T, tx *sql.Tx) {
	t.Helper()
	for _, statement := range []string{
		`INSERT INTO run (id, name, status, created_at) VALUES ('run', 'run', 'completed', '2026-01-01T00:00:00Z')`,
		`INSERT INTO engines (id, run_id, name, type, managed) VALUES ('run/engine', 'run', 'engine', 'test', 0)`,
		`INSERT INTO profiles (id, run_id, engine_id, name, model, managed) VALUES ('run/profile', 'run', 'run/engine', 'profile', 'model', 0)`,
		`INSERT INTO workloads (id, run_id, name, role, traffic_json, concurrency_json, samples) VALUES ('run/workload', 'run', 'workload', 'diagnostic', '{}', '[1]', 1)`,
	} {
		if _, err := tx.Exec(statement); err != nil {
			t.Fatal(err)
		}
	}
}

func assertZeroTokenArtifactRows(t *testing.T, db *sql.DB) {
	t.Helper()
	var requestCompletion sql.NullInt64
	var requestOutput sql.NullFloat64
	if err := db.QueryRow("SELECT completion_tokens, output_tok_s FROM requests LIMIT 1").Scan(&requestCompletion, &requestOutput); err != nil {
		t.Fatal(err)
	}
	if !requestCompletion.Valid || requestCompletion.Int64 != 0 || !requestOutput.Valid || requestOutput.Float64 != 0 {
		t.Fatalf("request completion/output = %v/%v, want measured zeroes", requestCompletion, requestOutput)
	}
	var measurementCompletion sql.NullInt64
	var measurementOutput sql.NullFloat64
	if err := db.QueryRow("SELECT completion_tokens, aggregate_output_tok_s FROM measurements LIMIT 1").Scan(&measurementCompletion, &measurementOutput); err != nil {
		t.Fatal(err)
	}
	if !measurementCompletion.Valid || measurementCompletion.Int64 != 0 || !measurementOutput.Valid || measurementOutput.Float64 != 0 {
		t.Fatalf("measurement completion/output = %v/%v, want measured zeroes", measurementCompletion, measurementOutput)
	}
	var mean float64
	var count int
	if err := db.QueryRow("SELECT mean, count FROM metric_stats WHERE metric = 'request_output_throughput'").Scan(&mean, &count); err != nil {
		t.Fatal(err)
	}
	if mean != 0 || count != 1 {
		t.Fatalf("request_output_throughput mean/count = %v/%d, want 0/1", mean, count)
	}
}

func httpTestSpec(t *testing.T, host string, port int, name string, numPrompts, concurrency int) Spec {
	t.Helper()
	spec := testSpec()
	spec.Name = name
	spec.OutputDir = t.TempDir()
	appendTimestamp := false
	spec.Runner.AppendTimestampToRun = &appendTimestamp
	spec.Safety.MinMemAvailableGiB = 0.1
	spec.Safety.StartupTimeoutSec = 10
	spec.Safety.WorkloadTimeoutSec = 10
	spec.Safety.HTTPTimeoutSec = 2
	spec.Warmup.Enabled = false
	spec.Profiles = spec.Profiles[:1]
	spec.Profiles[0].Host = host
	spec.Profiles[0].Port = port
	spec.Profiles[0].Managed = false
	spec.Profiles[0].EnableSleepMode = false
	// This generic OpenAI test server exposes no kernel evidence. Do not make
	// an explicit backend claim that the runner must attest.
	spec.Profiles[0].AttentionBackend = "auto"
	spec.Profiles[0].MoEBackend = "auto"
	spec.Profiles[0].KVCacheDType = "auto"
	spec.Workloads = []Workload{testRandomWorkload(name, []string{spec.Profiles[0].Name}, 64, 8, numPrompts, []int{concurrency})}
	spec.Workloads[0].LoadGenerator = LoadGeneratorHTTP
	ApplyDefaults(&spec)
	return spec
}

func fakeOpenAIErrorServer(t *testing.T) (*httptest.Server, string, int) {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/models":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"data":[]}`))
		case "/v1/chat/completions":
			http.Error(w, `{"error":{"message":"boom","type":"server_error","code":"boom"}}`, http.StatusInternalServerError)
		default:
			http.NotFound(w, r)
		}
	}))
	host, port := testServerHostPort(t, server)
	return server, host, port
}

func TestHTTPMergesExtraBody(t *testing.T) {
	client := openAIHTTPClient{
		profile: Profile{Model: "model"},
		workload: Workload{BenchmarkTrafficConfig: BenchmarkTrafficConfig{
			Backend:   "openai-chat",
			ExtraBody: `{"guided_decoding_backend":"outlines","add_generation_prompt":false}`,
		}},
	}
	body, endpoint, err := client.requestBody(CanonicalRequest{
		ID:              "request-1",
		Messages:        []Message{{Role: "user", Content: "hello"}},
		MaxOutputTokens: 8,
	})
	if err != nil {
		t.Fatal(err)
	}
	if endpoint != "/v1/chat/completions" {
		t.Fatalf("endpoint = %q, want chat completions", endpoint)
	}
	if body["guided_decoding_backend"] != "outlines" || body["add_generation_prompt"] != false {
		t.Fatalf("extra body was not merged: %+v", body)
	}
	client.workload.ExtraBody = `[1]`
	if _, _, err := client.requestBody(CanonicalRequest{
		ID:              "request-1",
		Messages:        []Message{{Role: "user", Content: "hello"}},
		MaxOutputTokens: 8,
	}); err == nil {
		t.Fatal("expected non-object extra_body to fail")
	}
}

func TestHTTPUsesCanonicalRequestMode(t *testing.T) {
	client := openAIHTTPClient{
		profile: Profile{Model: "model"},
		workload: Workload{BenchmarkTrafficConfig: BenchmarkTrafficConfig{
			Backend: "openai-chat",
		}},
	}
	body, endpoint, err := client.requestBody(CanonicalRequest{
		ID:              "request-1",
		Mode:            "completion",
		Prompt:          "finish this",
		MaxOutputTokens: 8,
	})
	if err != nil {
		t.Fatal(err)
	}
	if endpoint != "/v1/completions" {
		t.Fatalf("endpoint = %q, want completions", endpoint)
	}
	if body["prompt"] != "finish this" {
		t.Fatalf("body prompt = %v, want completion prompt body", body["prompt"])
	}
	if _, ok := body["messages"]; ok {
		t.Fatalf("completion request should not carry messages: %+v", body)
	}
}

func TestRequestRateDelayRejectsNonFiniteValues(t *testing.T) {
	if _, err := requestRateDelay("NaN"); err == nil {
		t.Fatal("expected NaN request_rate to fail")
	}
	if _, err := requestRateDelay("+Inf"); err == nil {
		t.Fatal("expected +Inf request_rate to fail")
	}
}

func TestExecuteHTTPChecksMemoryBeforeRun(t *testing.T) {
	original := checkMemoryFloor
	defer func() { checkMemoryFloor = original }()
	var calls atomic.Int64
	checkMemoryFloor = func(minGiB float64) (MemorySnapshot, error) {
		calls.Add(1)
		snapshot := MemorySnapshot{MemTotalGiB: 128, MemAvailableGiB: 1}
		return snapshot, &MemoryFloorError{Snapshot: snapshot, MinGiB: minGiB}
	}

	dir := t.TempDir()
	spec := testSpec()
	spec.Safety.MinMemAvailableGiB = 40
	spec.Safety.WorkloadTimeoutSec = 10
	spec.Safety.PollIntervalMillis = 1000
	planned := PlannedRun{
		Profile:     Profile{Name: "http-load", Host: "127.0.0.1", Port: 1, Model: "model"},
		Workload:    testRandomWorkload("localperf-http", []string{"http-load"}, 64, 8, 1, []int{1}),
		Concurrency: 1,
		ResultFile:  filepath.Join(dir, "result.json"),
	}
	logPath := filepath.Join(dir, "result.log")
	result, err := executeHTTPBench(context.Background(), spec, planned, logPath)
	if err == nil || !IsMemoryFloorError(err) {
		t.Fatalf("executeHTTPBench error = %v, want memory floor", err)
	}
	if result.ExitCode != 1 {
		t.Fatalf("exit code = %d, want 1", result.ExitCode)
	}
	if calls.Load() != 1 {
		t.Fatalf("memory checks = %d, want only the preflight check", calls.Load())
	}
	if _, err := os.Stat(planned.ResultFile); !os.IsNotExist(err) {
		t.Fatalf("result file exists or stat failed unexpectedly: %v", err)
	}
	logData, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(logData), "MemAvailable") {
		t.Fatalf("log did not include memory error:\n%s", logData)
	}
}

func TestExecuteFailedRepeatsAttachToCorrectMeasurements(t *testing.T) {
	spec := testSpec()
	spec.Name = "fake-vllm-repeat-failures"
	spec.OutputDir = t.TempDir()
	appendTimestamp := false
	spec.Runner.AppendTimestampToRun = &appendTimestamp
	configureFakeVLLM(t, &spec)
	spec.Env["FAKE_BENCH_FAILED"] = "1"
	spec.Safety.MinMemAvailableGiB = 0.1
	spec.Safety.StartupTimeoutSec = 10
	spec.Safety.WorkloadTimeoutSec = 10
	spec.Safety.HTTPTimeoutSec = 2
	spec.Warmup.Enabled = false
	spec.Profiles = spec.Profiles[:1]
	spec.Profiles[0].Port = freeTestPort()
	spec.Profiles[0].EnableSleepMode = false
	spec.Workloads = []Workload{testRandomWorkload("fake-random", []string{spec.Profiles[0].Name}, 128, 16, 1, []int{1})}
	spec.Workloads[0].Repeats = 2
	ApplyDefaults(&spec)
	summary, err := Execute(context.Background(), spec, RunOptions{})
	if err == nil || !strings.Contains(err.Error(), "benchmark run") {
		t.Fatalf("Execute error = %v, want failed benchmark run", err)
	}
	if summary.CompletedRuns != 0 || summary.FailedRuns != 2 {
		t.Fatalf("summary = %+v, want two failed repeats", summary)
	}
	db, err := sql.Open("sqlite", summary.ArtifactPath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	rows, err := db.Query("SELECT repeat_index, status, error_message FROM measurements ORDER BY repeat_index")
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var repeats []int
	for rows.Next() {
		var repeat int
		var status string
		var message sql.NullString
		if err := rows.Scan(&repeat, &status, &message); err != nil {
			t.Fatal(err)
		}
		if status != "failed" || !message.Valid || !strings.Contains(message.String, "failed request") {
			t.Fatalf("repeat %d status=%s message=%q, want failed request error", repeat, status, message.String)
		}
		repeats = append(repeats, repeat)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if fmt.Sprint(repeats) != "[0 1]" {
		t.Fatalf("measurement repeats = %v, want [0 1]", repeats)
	}
}

func TestExecuteStopsManagedProfileAfterWorkloadMemoryFloorAbort(t *testing.T) {
	startFile := filepath.Join(t.TempDir(), "bench.started")
	oldCheckMemoryFloor := checkMemoryFloor
	checkMemoryFloor = func(minGiB float64) (MemorySnapshot, error) {
		snapshot := MemorySnapshot{MemTotalGiB: 128, MemAvailableGiB: minGiB + 1}
		if _, err := os.Stat(startFile); err == nil {
			snapshot.MemAvailableGiB = minGiB - 1
			return snapshot, &MemoryFloorError{Snapshot: snapshot, MinGiB: minGiB}
		}
		return snapshot, nil
	}
	defer func() {
		checkMemoryFloor = oldCheckMemoryFloor
	}()

	spec := testSpec()
	spec.Name = "fake-vllm-memory-floor-abort"
	spec.OutputDir = t.TempDir()
	appendTimestamp := false
	spec.Runner.AppendTimestampToRun = &appendTimestamp
	configureFakeVLLM(t, &spec)
	spec.Env["FAKE_BENCH_STARTED_FILE"] = startFile
	spec.Env["FAKE_BENCH_SLEEP_MS"] = "500"
	spec.Safety.MinMemAvailableGiB = 40
	spec.Safety.PollIntervalMillis = 20
	spec.Safety.StartupTimeoutSec = 10
	spec.Safety.WorkloadTimeoutSec = 10
	spec.Safety.HTTPTimeoutSec = 2
	spec.Warmup.Enabled = false
	spec.Profiles = spec.Profiles[:1]
	spec.Profiles[0].Port = freeTestPort()
	spec.Profiles[0].EnableSleepMode = false
	spec.Workloads = []Workload{testRandomWorkload("fake-random", []string{spec.Profiles[0].Name}, 128, 16, 1, []int{1, 2})}
	ApplyDefaults(&spec)
	summary, err := Execute(context.Background(), spec, RunOptions{})
	if err == nil || !strings.Contains(err.Error(), "benchmark run") {
		t.Fatalf("Execute error = %v, want failed benchmark run", err)
	}
	if summary.CompletedRuns != 0 || summary.FailedRuns != 2 {
		t.Fatalf("summary = %+v, want current and remaining profile runs failed", summary)
	}
	events, err := os.ReadFile(filepath.Join(summary.RunDir, "events.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(events), `"type":"profile_memory_floor_abort"`) {
		t.Fatalf("events did not record profile memory-floor abort:\n%s", events)
	}
	client := &http.Client{Timeout: 200 * time.Millisecond}
	resp, err := client.Get(fmt.Sprintf("http://127.0.0.1:%d/v1/models", spec.Profiles[0].Port))
	if err == nil {
		_ = resp.Body.Close()
		t.Fatalf("expected managed server to be stopped after memory-floor abort, got HTTP %d", resp.StatusCode)
	}
}

func TestExecuteFailsWhenWarmupReportsRequestFailures(t *testing.T) {
	spec := testSpec()
	spec.Name = "fake-vllm-warmup-failure"
	spec.OutputDir = t.TempDir()
	appendTimestamp := false
	spec.Runner.AppendTimestampToRun = &appendTimestamp
	configureFakeVLLM(t, &spec)
	spec.Env["FAKE_BENCH_FAILED"] = "1"
	spec.Safety.MinMemAvailableGiB = 0.1
	spec.Safety.StartupTimeoutSec = 10
	spec.Safety.WorkloadTimeoutSec = 10
	spec.Safety.HTTPTimeoutSec = 2
	spec.Warmup.Enabled = true
	spec.Profiles = spec.Profiles[:1]
	spec.Profiles[0].Port = freeTestPort()
	spec.Profiles[0].EnableSleepMode = false
	spec.Workloads = []Workload{testRandomWorkload("fake-random", []string{spec.Profiles[0].Name}, 128, 16, 1, []int{1})}
	ApplyDefaults(&spec)
	summary, err := Execute(context.Background(), spec, RunOptions{})
	if err == nil || !strings.Contains(err.Error(), "benchmark run") {
		t.Fatalf("Execute error = %v, want failed benchmark run", err)
	}
	if summary.CompletedRuns != 0 || summary.FailedRuns != 1 {
		t.Fatalf("summary = %+v, want failed profile before workload", summary)
	}
	events, err := os.ReadFile(filepath.Join(summary.RunDir, "events.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(events), `"type":"warmup_finish"`) || !strings.Contains(string(events), "warmup result reported") {
		t.Fatalf("events did not record failed warmup:\n%s", events)
	}
	db, err := sql.Open("sqlite", summary.ArtifactPath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var status string
	var message sql.NullString
	if err := db.QueryRow("SELECT status, error_message FROM measurements").Scan(&status, &message); err != nil {
		t.Fatal(err)
	}
	if status != "skipped" || !message.Valid || !strings.Contains(message.String, "warmup result reported") {
		t.Fatalf("measurement status=%s message=%q, want skipped warmup failure", status, message.String)
	}
}

func TestExecuteWarmsPrebootedProfileAfterWake(t *testing.T) {
	spec := testSpec()
	spec.Name = "fake-vllm-preboot-warm-after-wake"
	spec.OutputDir = t.TempDir()
	appendTimestamp := false
	spec.Runner.AppendTimestampToRun = &appendTimestamp
	spec.Runner.PrebootProfiles = true
	configureFakeVLLM(t, &spec)
	spec.Safety.MinMemAvailableGiB = 0.1
	spec.Safety.StartupTimeoutSec = 10
	spec.Safety.WorkloadTimeoutSec = 10
	spec.Safety.HTTPTimeoutSec = 2
	spec.Warmup.Enabled = true
	spec.Profiles = spec.Profiles[:1]
	spec.Profiles[0].Port = freeTestPort()
	spec.Workloads = []Workload{testRandomWorkload("fake-random", []string{spec.Profiles[0].Name}, 128, 16, 1, []int{1})}
	ApplyDefaults(&spec)
	summary, err := Execute(context.Background(), spec, RunOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if summary.CompletedRuns != 1 || summary.FailedRuns != 0 {
		t.Fatalf("summary = %+v, want one completed run", summary)
	}
	events, err := os.ReadFile(filepath.Join(summary.RunDir, "events.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Count(string(events), `"type":"warmup_finish"`); got != 2 {
		t.Fatalf("warmup_finish events = %d, want preboot and post-wake warmups:\n%s", got, events)
	}
}

func TestSleepProfileWaitsForSleepingState(t *testing.T) {
	spec := testSpec()
	spec.Name = "fake-vllm-delayed-sleep"
	spec.OutputDir = t.TempDir()
	configureFakeVLLM(t, &spec)
	spec.Safety.MinMemAvailableGiB = 0.1
	spec.Safety.PollIntervalMillis = 20
	spec.Safety.StartupTimeoutSec = 5
	spec.Safety.HTTPTimeoutSec = 2
	spec.Warmup.Enabled = false
	spec.Profiles = spec.Profiles[:1]
	spec.Profiles[0].Port = freeTestPort()
	spec.Profiles[0].Env = map[string]string{"FAKE_SLEEP_DELAY_MS": "250"}
	ApplyDefaults(&spec)
	runDir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(runDir, "logs"), 0o755); err != nil {
		t.Fatal(err)
	}
	events, err := newEventWriter(filepath.Join(runDir, "events.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	defer events.Close()
	proc, err := prepareProfile(context.Background(), spec, runDir, spec.Profiles[0], events, false)
	if err != nil {
		t.Fatal(err)
	}
	defer stopProcess(proc)
	start := time.Now()
	if err := sleepProfile(context.Background(), spec, spec.Profiles[0], events); err != nil {
		t.Fatal(err)
	}
	if elapsed := time.Since(start); elapsed < 200*time.Millisecond {
		t.Fatalf("sleepProfile returned before delayed sleep completed: %s", elapsed)
	}
}

func TestWakeProfileWaitsForAwakeState(t *testing.T) {
	spec := testSpec()
	spec.Name = "fake-vllm-delayed-wake"
	spec.OutputDir = t.TempDir()
	configureFakeVLLM(t, &spec)
	spec.Safety.MinMemAvailableGiB = 0.1
	spec.Safety.PollIntervalMillis = 20
	spec.Safety.StartupTimeoutSec = 5
	spec.Safety.HTTPTimeoutSec = 2
	spec.Warmup.Enabled = false
	spec.Profiles = spec.Profiles[:1]
	spec.Profiles[0].Port = freeTestPort()
	spec.Profiles[0].Env = map[string]string{"FAKE_WAKE_DELAY_MS": "250"}
	ApplyDefaults(&spec)
	runDir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(runDir, "logs"), 0o755); err != nil {
		t.Fatal(err)
	}
	events, err := newEventWriter(filepath.Join(runDir, "events.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	defer events.Close()
	proc, err := prepareProfile(context.Background(), spec, runDir, spec.Profiles[0], events, false)
	if err != nil {
		t.Fatal(err)
	}
	defer stopProcess(proc)
	if err := sleepProfile(context.Background(), spec, spec.Profiles[0], events); err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	if err := wakeProfile(context.Background(), spec, spec.Profiles[0], events); err != nil {
		t.Fatal(err)
	}
	if elapsed := time.Since(start); elapsed < 200*time.Millisecond {
		t.Fatalf("wakeProfile returned before delayed wake completed: %s", elapsed)
	}
}

func TestExecuteStopsPrebootedProfileAfterWakeFailure(t *testing.T) {
	spec := testSpec()
	spec.Name = "fake-vllm-preboot-wake-failure"
	spec.OutputDir = t.TempDir()
	appendTimestamp := false
	spec.Runner.AppendTimestampToRun = &appendTimestamp
	spec.Runner.PrebootProfiles = true
	configureFakeVLLM(t, &spec)
	spec.Safety.MinMemAvailableGiB = 0.1
	spec.Safety.StartupTimeoutSec = 10
	spec.Safety.WorkloadTimeoutSec = 10
	spec.Safety.HTTPTimeoutSec = 2
	spec.Warmup.Enabled = false
	spec.Profiles = spec.Profiles[:1]
	spec.Profiles[0].Port = freeTestPort()
	spec.Profiles[0].Env = map[string]string{"FAKE_WAKE_FAIL": "1"}
	spec.Workloads = []Workload{testRandomWorkload("fake-random", []string{spec.Profiles[0].Name}, 128, 16, 1, []int{1})}
	ApplyDefaults(&spec)
	summary, err := Execute(context.Background(), spec, RunOptions{})
	if err == nil || !strings.Contains(err.Error(), "benchmark run") {
		t.Fatalf("Execute error = %v, want failed benchmark run", err)
	}
	if summary.CompletedRuns != 0 || summary.FailedRuns != 1 {
		t.Fatalf("summary = %+v, want failed profile run", summary)
	}
	events, err := os.ReadFile(filepath.Join(summary.RunDir, "events.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(events), `"type":"profile_failed"`) || !strings.Contains(string(events), "wake failed") {
		t.Fatalf("events did not record wake failure:\n%s", events)
	}
	client := &http.Client{Timeout: 200 * time.Millisecond}
	resp, err := client.Get(fmt.Sprintf("http://127.0.0.1:%d/v1/models", spec.Profiles[0].Port))
	if err == nil {
		_ = resp.Body.Close()
		t.Fatalf("expected prebooted server to be stopped after wake failure, got HTTP %d", resp.StatusCode)
	}
}

func TestExecuteStopsManagedProfileOnInterrupt(t *testing.T) {
	startFile := filepath.Join(t.TempDir(), "bench.started")
	spec := testSpec()
	spec.Name = "fake-vllm-interrupt"
	spec.OutputDir = t.TempDir()
	appendTimestamp := false
	spec.Runner.AppendTimestampToRun = &appendTimestamp
	configureFakeVLLM(t, &spec)
	spec.Env["FAKE_BENCH_STARTED_FILE"] = startFile
	spec.Env["FAKE_BENCH_SLEEP_MS"] = "5000"
	spec.Safety.MinMemAvailableGiB = 0.1
	spec.Safety.StartupTimeoutSec = 10
	spec.Safety.WorkloadTimeoutSec = 10
	spec.Safety.HTTPTimeoutSec = 2
	spec.Warmup.Enabled = false
	spec.Profiles = spec.Profiles[:1]
	spec.Profiles[0].Port = freeTestPort()
	spec.Profiles[0].EnableSleepMode = false
	spec.Workloads = []Workload{testRandomWorkload("fake-random", []string{spec.Profiles[0].Name}, 128, 16, 1, []int{1})}
	ApplyDefaults(&spec)
	type result struct {
		summary RunSummary
		err     error
	}
	done := make(chan result, 1)
	go func() {
		summary, err := Execute(context.Background(), spec, RunOptions{})
		done <- result{summary: summary, err: err}
	}()
	waitForFile(t, startFile)
	process, err := os.FindProcess(os.Getpid())
	if err != nil {
		t.Fatal(err)
	}
	if err := process.Signal(os.Interrupt); err != nil {
		t.Fatal(err)
	}
	var got result
	select {
	case got = <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Execute did not return after interrupt")
	}
	if got.err == nil {
		t.Fatalf("Execute error = nil, want interrupted run failure; summary = %+v", got.summary)
	}
	client := &http.Client{Timeout: 200 * time.Millisecond}
	resp, err := client.Get(fmt.Sprintf("http://127.0.0.1:%d/v1/models", spec.Profiles[0].Port))
	if err == nil {
		_ = resp.Body.Close()
		t.Fatalf("expected managed server to be stopped after interrupt, got HTTP %d", resp.StatusCode)
	}
}

func TestExecuteContextCancelAfterPartialProgressIsFatal(t *testing.T) {
	startedDir := t.TempDir()
	spec := testSpec()
	spec.Name = "fake-vllm-partial-context-cancel"
	spec.OutputDir = t.TempDir()
	appendTimestamp := false
	spec.Runner.AppendTimestampToRun = &appendTimestamp
	configureFakeVLLM(t, &spec)
	spec.Env["FAKE_BENCH_STARTED_DIR"] = startedDir
	spec.Env["FAKE_BENCH_SLEEP_MS"] = "5000"
	spec.Env["FAKE_BENCH_SLEEP_CONCURRENCY"] = "2"
	spec.Safety.MinMemAvailableGiB = 0.1
	spec.Safety.StartupTimeoutSec = 10
	spec.Safety.WorkloadTimeoutSec = 10
	spec.Safety.HTTPTimeoutSec = 2
	spec.Warmup.Enabled = false
	spec.Profiles = spec.Profiles[:1]
	spec.Profiles[0].Port = freeTestPort()
	spec.Profiles[0].EnableSleepMode = false
	spec.Workloads = []Workload{testRandomWorkload("fake-random", []string{spec.Profiles[0].Name}, 128, 16, 1, []int{1, 2})}
	ApplyDefaults(&spec)

	ctx, cancel := context.WithCancel(context.Background())
	type result struct {
		summary RunSummary
		err     error
	}
	done := make(chan result, 1)
	go func() {
		summary, err := Execute(ctx, spec, RunOptions{})
		done <- result{summary: summary, err: err}
	}()
	waitForFile(t, filepath.Join(startedDir, "c2.started"))
	cancel()

	var got result
	select {
	case got = <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Execute did not return after context cancellation")
	}
	if got.err == nil || !strings.Contains(got.err.Error(), "context canceled") {
		t.Fatalf("Execute error = %v, want context cancellation", got.err)
	}
	if got.summary.CompletedRuns != 1 || got.summary.FailedRuns != 1 || got.summary.Error == "" {
		t.Fatalf("summary = %+v, want one completed run, one failed run, and fatal summary error", got.summary)
	}
	if status := sqliteRunStatus(t, got.summary.ArtifactPath); status != "failed" {
		t.Fatalf("artifact run status = %q, want failed", status)
	}
}

func TestStopProcessUsesSavedProcessGroupAfterParentExit(t *testing.T) {
	childFile := filepath.Join(t.TempDir(), "child.pid")
	cmd := exec.Command("sh", "-c", fmt.Sprintf("sleep 60 & echo $! > %s", shellSingleQuote(childFile)))
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	pgid, err := syscall.Getpgid(cmd.Process.Pid)
	if err != nil {
		t.Fatal(err)
	}
	proc := &serverProcess{cmd: cmd, pgid: pgid, done: make(chan error, 1)}
	go func() {
		proc.done <- cmd.Wait()
	}()
	childPID := waitForPIDFile(t, childFile)
	defer func() {
		_ = syscall.Kill(-pgid, syscall.SIGKILL)
	}()
	select {
	case err := <-proc.done:
		proc.done <- err
	case <-time.After(2 * time.Second):
		t.Fatal("launcher did not exit")
	}
	if !processExists(childPID) {
		t.Fatalf("child process %d exited before cleanup", childPID)
	}
	stopProcess(proc)
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if !processExists(childPID) {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("child process %d survived process-group cleanup", childPID)
}

func TestExecuteHonorsStopManagedOnExitFalse(t *testing.T) {
	spec := testSpec()
	spec.Name = "fake-vllm-keepalive"
	spec.OutputDir = t.TempDir()
	appendTimestamp := false
	stopManaged := false
	spec.Runner.AppendTimestampToRun = &appendTimestamp
	spec.Runner.StopManagedOnExit = &stopManaged
	configureFakeVLLM(t, &spec)
	spec.Safety.MinMemAvailableGiB = 0.1
	spec.Safety.StartupTimeoutSec = 10
	spec.Safety.WorkloadTimeoutSec = 10
	spec.Safety.HTTPTimeoutSec = 2
	spec.Warmup.Enabled = false
	spec.Profiles = spec.Profiles[:1]
	spec.Profiles[0].Port = freeTestPort()
	spec.Profiles[0].EnableSleepMode = false
	spec.Workloads = []Workload{testRandomWorkload("fake-random", []string{spec.Profiles[0].Name}, 128, 16, 1, []int{1})}
	ApplyDefaults(&spec)
	summary, err := Execute(context.Background(), spec, RunOptions{})
	defer shutdownFakeServer(spec.Profiles[0].Port)
	if err != nil {
		t.Fatal(err)
	}
	if summary.CompletedRuns != 1 || summary.FailedRuns != 0 {
		t.Fatalf("summary = %+v, want one completed run", summary)
	}
	resp, err := http.Get(fmt.Sprintf("http://127.0.0.1:%d/v1/models", spec.Profiles[0].Port))
	if err != nil {
		t.Fatalf("expected managed server to remain alive: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("health status = %d, want 200", resp.StatusCode)
	}
}

func TestExecuteFailsWhenSleepFails(t *testing.T) {
	spec := testSpec()
	spec.Name = "fake-vllm-sleep-failure"
	spec.OutputDir = t.TempDir()
	appendTimestamp := false
	spec.Runner.AppendTimestampToRun = &appendTimestamp
	configureFakeVLLM(t, &spec)
	spec.Safety.MinMemAvailableGiB = 0.1
	spec.Safety.StartupTimeoutSec = 10
	spec.Safety.WorkloadTimeoutSec = 10
	spec.Safety.HTTPTimeoutSec = 2
	spec.Warmup.Enabled = false
	spec.Profiles = spec.Profiles[:1]
	spec.Profiles[0].Port = freeTestPort()
	spec.Profiles[0].Env = map[string]string{"FAKE_SLEEP_FAIL": "1"}
	spec.Workloads = []Workload{testRandomWorkload("fake-random", []string{spec.Profiles[0].Name}, 128, 16, 1, []int{1})}
	ApplyDefaults(&spec)
	summary, err := Execute(context.Background(), spec, RunOptions{})
	if err == nil || !strings.Contains(err.Error(), "sleep failed") {
		t.Fatalf("Execute error = %v, want sleep failure", err)
	}
	if summary.CompletedRuns != 1 {
		t.Fatalf("completed runs = %d, want measured workload to complete before sleep failure", summary.CompletedRuns)
	}
	if got := sqliteRunStatus(t, summary.ArtifactPath); got != "failed" {
		t.Fatalf("artifact run status = %q, want failed", got)
	}
}

func sqliteRunStatus(t *testing.T, artifactPath string) string {
	t.Helper()
	db, err := sql.Open("sqlite", artifactPath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var status string
	if err := db.QueryRow("SELECT status FROM run ORDER BY id LIMIT 1").Scan(&status); err != nil {
		t.Fatal(err)
	}
	return status
}

func TestExecuteFinalizesArtifactsWhenPrebootFails(t *testing.T) {
	spec := testSpec()
	spec.Name = "fake-vllm-preboot-failure"
	spec.OutputDir = t.TempDir()
	appendTimestamp := false
	spec.Runner.AppendTimestampToRun = &appendTimestamp
	spec.Runner.PrebootProfiles = true
	configureFakeVLLM(t, &spec)
	spec.Safety.MinMemAvailableGiB = 0.1
	spec.Safety.StartupTimeoutSec = 10
	spec.Safety.WorkloadTimeoutSec = 10
	spec.Safety.HTTPTimeoutSec = 2
	spec.Warmup.Enabled = false
	spec.Profiles = spec.Profiles[:1]
	spec.Profiles[0].Port = freeTestPort()
	spec.Profiles[0].Env = map[string]string{"FAKE_SLEEP_FAIL": "1"}
	spec.Workloads = []Workload{testRandomWorkload("fake-random", []string{spec.Profiles[0].Name}, 128, 16, 1, []int{1})}
	ApplyDefaults(&spec)
	summary, err := Execute(context.Background(), spec, RunOptions{})
	if err == nil || !strings.Contains(err.Error(), "preboot profiles failed") {
		t.Fatalf("Execute error = %v, want preboot failure", err)
	}
	if summary.CompletedRuns != 0 || summary.FailedRuns != 1 || summary.FinishedAt.IsZero() {
		t.Fatalf("summary = %+v, want finalized failed run", summary)
	}
	for _, path := range []string{
		filepath.Join(summary.RunDir, "events.jsonl"),
		filepath.Join(summary.RunDir, "summary.json"),
	} {
		if _, err := os.Stat(path); err != nil {
			t.Fatalf("expected artifact %s: %v", path, err)
		}
	}
	events, err := os.ReadFile(filepath.Join(summary.RunDir, "events.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(events), `"type":"preboot_failed"`) || !strings.Contains(string(events), `"type":"run_finish"`) {
		t.Fatalf("events did not record preboot failure and run finish:\n%s", events)
	}
}

func testSpec() Spec {
	temp := 0.0
	oneAwake := true
	stopManaged := true
	spec := Spec{
		Version: "1",
		Name:    "DiffusionGemma Standard",
		Model:   "nvidia/diffusiongemma-26B-A4B-it-NVFP4",
		Env: map[string]string{
			"VLLM_USE_V2_MODEL_RUNNER": "1",
		},
		Engines: []EngineConfig{{
			Name:         "vllm",
			Type:         "vllm-managed",
			Command:      "vllm",
			BenchCommand: "vllm",
		}},
		Runner: RunnerConfig{
			OneAwakeProfile:   &oneAwake,
			StopManagedOnExit: &stopManaged,
		},
		Safety: SafetyConfig{
			MinMemAvailableGiB: 40,
		},
		Profiles: []Profile{
			{
				Name:                 "8k",
				Engine:               "vllm",
				Host:                 "127.0.0.1",
				Port:                 8108,
				Managed:              true,
				EnableSleepMode:      true,
				SleepLevel:           testIntPointer(2),
				MaxModelLen:          8192,
				MaxNumSeqs:           16,
				MaxNumBatchedTokens:  8192,
				GPUMemoryUtilization: 0.35,
				AttentionBackend:     "TRITON_ATTN",
				MoEBackend:           "cutlass",
			},
		},
		Workloads: []Workload{
			{
				Name:             "prefill-8k",
				Role:             WorkloadRoleBenchmark,
				Profiles:         []string{"8k"},
				ContextTarget:    8192,
				ContextSemantics: ContextSemanticsCapacity,
				BenchmarkTrafficConfig: BenchmarkTrafficConfig{
					Backend:         "openai-chat",
					DatasetName:     "random",
					RandomInputLen:  8192,
					RandomOutputLen: 16,
					RequestRate:     "inf",
				},
				PromptsPerUser: 2,
				MaxConcurrency: []int{4, 8},
				Temperature:    &temp,
			},
		},
	}
	ApplyDefaults(&spec)
	return spec
}

func fakeOpenAIServer(t *testing.T) (*httptest.Server, string, int) {
	t.Helper()
	return fakeOpenAIServerWithUsage(t, 64, 8, 72)
}

func fakeOpenAIServerWithUsage(t *testing.T, promptTokens, completionTokens, totalTokens int) (*httptest.Server, string, int) {
	t.Helper()
	var calls atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/models":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"data":[]}`))
		case "/v1/chat/completions":
			call := calls.Add(1)
			time.Sleep(time.Duration(call*10) * time.Millisecond)
			if requestWantsStream(r) {
				writeFakeSSEChatResponse(w, call, promptTokens, completionTokens, totalTokens)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = fmt.Fprintf(w, `{"id":"cmpl-%d","choices":[{"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":%d,"completion_tokens":%d,"total_tokens":%d}}`, call, promptTokens, completionTokens, totalTokens)
		default:
			http.NotFound(w, r)
		}
	}))
	host, port := testServerHostPort(t, server)
	return server, host, port
}

func requestWantsStream(r *http.Request) bool {
	data, err := io.ReadAll(r.Body)
	if err != nil {
		return false
	}
	var body struct {
		Stream bool `json:"stream"`
	}
	if err := json.Unmarshal(data, &body); err != nil {
		return false
	}
	return body.Stream
}

// writeFakeSSEChatResponse emits an OpenAI-style chat completion stream:
// content chunks (none when completionTokens is zero), a finish chunk, a
// usage chunk, then [DONE].
func writeFakeSSEChatResponse(w http.ResponseWriter, call int64, promptTokens, completionTokens, totalTokens int) {
	w.Header().Set("Content-Type", "text/event-stream")
	flusher, _ := w.(http.Flusher)
	if completionTokens > 0 {
		for i := 0; i < 2; i++ {
			_, _ = fmt.Fprintf(w, "data: {\"id\":\"cmpl-%d\",\"choices\":[{\"delta\":{\"content\":\"ok\"}}]}\n\n", call)
			if flusher != nil {
				flusher.Flush()
			}
			time.Sleep(time.Millisecond)
		}
	}
	_, _ = fmt.Fprintf(w, "data: {\"id\":\"cmpl-%d\",\"choices\":[{\"delta\":{},\"finish_reason\":\"stop\"}]}\n\n", call)
	_, _ = fmt.Fprintf(w, "data: {\"id\":\"cmpl-%d\",\"choices\":[],\"usage\":{\"prompt_tokens\":%d,\"completion_tokens\":%d,\"total_tokens\":%d}}\n\n", call, promptTokens, completionTokens, totalTokens)
	_, _ = fmt.Fprint(w, "data: [DONE]\n\n")
	if flusher != nil {
		flusher.Flush()
	}
}

func testServerHostPort(t *testing.T, server *httptest.Server) (string, int) {
	t.Helper()
	parsed, err := url.Parse(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	host, portString, err := net.SplitHostPort(parsed.Host)
	if err != nil {
		t.Fatal(err)
	}
	port, err := strconv.Atoi(portString)
	if err != nil {
		t.Fatal(err)
	}
	return host, port
}

func testRandomWorkload(name string, profiles []string, inputLen, outputLen, numPrompts int, concurrencies []int) Workload {
	return Workload{
		Name:             name,
		Role:             WorkloadRoleDiagnostic,
		Profiles:         profiles,
		ContextTarget:    8192,
		ContextSemantics: ContextSemanticsCapacity,
		BenchmarkTrafficConfig: BenchmarkTrafficConfig{
			Backend:         "openai-chat",
			DatasetName:     "random",
			RandomInputLen:  inputLen,
			RandomOutputLen: outputLen,
			RequestRate:     "inf",
		},
		NumPrompts:     numPrompts,
		MaxConcurrency: concurrencies,
	}
}

func testShareGPTWorkload(path string, profiles []string) Workload {
	seed := 1
	temp := 0.0
	return Workload{
		Name:             "sharegpt-chat-smoke",
		Role:             WorkloadRoleDiagnostic,
		Phase:            "decode",
		Profiles:         profiles,
		ContextTarget:    8192,
		ContextSemantics: ContextSemanticsCapacity,
		Dataset: DatasetSpec{
			Type:        "sharegpt",
			Path:        path,
			Split:       "train",
			SampleCount: 1,
			Seed:        &seed,
			Selection:   "first_n",
		},
		Request: RequestSpec{
			Mode:            "chat",
			TurnPolicy:      "first_user_turn",
			MaxOutputTokens: 512,
			Temperature:     &temp,
		},
		LoadGenerator:  LoadGeneratorVLLMBench,
		MaxConcurrency: []int{1},
		BenchmarkTrafficConfig: BenchmarkTrafficConfig{
			RequestRate: "inf",
		},
	}
}

func testCustomJSONLWorkload(name, path string, profiles []string) Workload {
	return Workload{
		Name:             name,
		Role:             WorkloadRoleDiagnostic,
		Phase:            "decode",
		Profiles:         profiles,
		ContextTarget:    8192,
		ContextSemantics: ContextSemanticsCapacity,
		Dataset: DatasetSpec{
			Type:        "custom_jsonl",
			Path:        path,
			SampleCount: 1,
			Selection:   "first_n",
		},
		Request: RequestSpec{
			Mode:            "chat",
			MaxOutputTokens: 1,
		},
		LoadGenerator:  LoadGeneratorHTTP,
		MaxConcurrency: []int{1},
		BenchmarkTrafficConfig: BenchmarkTrafficConfig{
			RequestRate: "inf",
		},
	}
}

func testRawPayloadWorkload(name, path string, profiles []string) Workload {
	return Workload{
		Name:             name,
		Role:             WorkloadRoleDiagnostic,
		Phase:            "decode",
		Profiles:         profiles,
		ContextTarget:    8192,
		ContextSemantics: ContextSemanticsCapacity,
		Dataset: DatasetSpec{
			Type:        "raw_payload",
			Path:        path,
			SampleCount: 1,
			Selection:   "first_n",
		},
		Request: RequestSpec{
			Mode: "raw_payload",
		},
		LoadGenerator:  LoadGeneratorHTTP,
		MaxConcurrency: []int{1},
		BenchmarkTrafficConfig: BenchmarkTrafficConfig{
			RequestRate: "inf",
		},
	}
}

func writeShareGPTFixture(t *testing.T, dir string) string {
	t.Helper()
	path := filepath.Join(dir, "sharegpt.json")
	writeFile(t, path, `[
  {
    "id": "fixture-1",
    "conversations": [
      {"from": "human", "value": "Explain TTFT in one sentence."},
      {"from": "gpt", "value": "TTFT is the delay until the first generated token arrives."}
    ]
  }
]`)
	return path
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func testIntPointer(value int) *int {
	return &value
}

func configureFakeVLLM(t *testing.T, spec *Spec) {
	t.Helper()
	command := fakeVLLMScript(t)
	for i := range spec.Engines {
		spec.Engines[i].Command = command
		spec.Engines[i].BenchCommand = command
	}
	for i := range spec.Profiles {
		// The process is a command/runner fixture, not a kernel implementation.
		// Explicit backend claims belong only in tests that provide execution
		// evidence for attestation.
		spec.Profiles[i].AttentionBackend = "auto"
		spec.Profiles[i].MoEBackend = "auto"
		spec.Profiles[i].KVCacheDType = "auto"
	}
}

func fakeVLLMScript(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "fake-vllm")
	script := fmt.Sprintf("#!/bin/sh\nGO_WANT_VLLMBENCH_HELPER=1 exec %s -test.run=TestHelperProcess -- \"$@\"\n", shellSingleQuote(os.Args[0]))
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

func shellSingleQuote(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "'\"'\"'") + "'"
}

func TestHelperProcess(t *testing.T) {
	if os.Getenv("GO_WANT_VLLMBENCH_HELPER") != "1" {
		return
	}
	args := helperArgs()
	if len(args) == 0 {
		os.Exit(2)
	}
	switch args[0] {
	case "serve":
		runFakeServe(args[1:])
	case "bench":
		runFakeBench(args[1:])
	default:
		os.Exit(2)
	}
}

func helperArgs() []string {
	for i, arg := range os.Args {
		if arg == "--" {
			return os.Args[i+1:]
		}
	}
	return nil
}

func runFakeServe(args []string) {
	port := flagValue(args, "--port")
	if port == "" {
		os.Exit(2)
	}
	sleepDelay := durationFromEnv("FAKE_SLEEP_DELAY_MS")
	wakeDelay := durationFromEnv("FAKE_WAKE_DELAY_MS")
	var sleeping atomic.Bool
	mux := http.NewServeMux()
	var server *http.Server
	mux.HandleFunc("/v1/models", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"data": []any{}})
	})
	mux.HandleFunc("/is_sleeping", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"is_sleeping": sleeping.Load()})
	})
	mux.HandleFunc("/sleep", func(w http.ResponseWriter, _ *http.Request) {
		if os.Getenv("FAKE_SLEEP_FAIL") == "1" {
			http.Error(w, "sleep failed", http.StatusInternalServerError)
			return
		}
		setSleepingAfter(&sleeping, true, sleepDelay)
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true})
	})
	mux.HandleFunc("/wake_up", func(w http.ResponseWriter, _ *http.Request) {
		if os.Getenv("FAKE_WAKE_FAIL") == "1" {
			http.Error(w, "wake failed", http.StatusInternalServerError)
			return
		}
		setSleepingAfter(&sleeping, false, wakeDelay)
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true})
	})
	mux.HandleFunc("/shutdown", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true})
		go func() {
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			_ = server.Shutdown(ctx)
		}()
	})
	server = &http.Server{Addr: "127.0.0.1:" + port, Handler: mux}
	signals := make(chan os.Signal, 1)
	signal.Notify(signals, syscall.SIGTERM, syscall.SIGINT)
	go func() {
		<-signals
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_ = server.Shutdown(ctx)
	}()
	if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		os.Exit(1)
	}
	os.Exit(0)
}

func durationFromEnv(key string) time.Duration {
	value, _ := strconv.Atoi(os.Getenv(key))
	if value <= 0 {
		return 0
	}
	return time.Duration(value) * time.Millisecond
}

func setSleepingAfter(sleeping *atomic.Bool, value bool, delay time.Duration) {
	if delay <= 0 {
		sleeping.Store(value)
		return
	}
	go func() {
		time.Sleep(delay)
		sleeping.Store(value)
	}()
}

func runFakeBench(args []string) {
	if len(args) == 0 || args[0] != "serve" {
		os.Exit(2)
	}
	resultPath := flagValue(args, "--result-filename")
	if resultPath == "" {
		os.Exit(2)
	}
	concurrency, _ := strconv.Atoi(flagValue(args, "--max-concurrency"))
	numPrompts, _ := strconv.Atoi(flagValue(args, "--num-prompts"))
	if concurrency <= 0 {
		concurrency = 1
	}
	if numPrompts <= 0 {
		numPrompts = concurrency
	}
	if startFile := os.Getenv("FAKE_BENCH_STARTED_FILE"); startFile != "" {
		_ = os.MkdirAll(filepath.Dir(startFile), 0o755)
		_ = os.WriteFile(startFile, []byte("1\n"), 0o644)
	}
	if startDir := os.Getenv("FAKE_BENCH_STARTED_DIR"); startDir != "" {
		_ = os.MkdirAll(startDir, 0o755)
		_ = os.WriteFile(filepath.Join(startDir, fmt.Sprintf("c%d.started", concurrency)), []byte("1\n"), 0o644)
	}
	if rawSleepMillis := os.Getenv("FAKE_BENCH_SLEEP_MS"); rawSleepMillis != "" {
		sleepMillis, _ := strconv.Atoi(rawSleepMillis)
		sleepConcurrency, _ := strconv.Atoi(os.Getenv("FAKE_BENCH_SLEEP_CONCURRENCY"))
		if sleepMillis > 0 && (sleepConcurrency == 0 || sleepConcurrency == concurrency) {
			time.Sleep(time.Duration(sleepMillis) * time.Millisecond)
		}
	}
	failed, _ := strconv.Atoi(os.Getenv("FAKE_BENCH_FAILED"))
	if failed < 0 {
		failed = 0
	}
	if failed > numPrompts {
		failed = numPrompts
	}
	if err := os.MkdirAll(filepath.Dir(resultPath), 0o755); err != nil {
		os.Exit(1)
	}
	row := map[string]any{
		"completed":              numPrompts - failed,
		"failed":                 failed,
		"duration":               1.0,
		"total_input_tokens":     numPrompts * 128,
		"total_output_tokens":    numPrompts * 16,
		"total_tokens":           numPrompts * 144,
		"output_throughput":      float64(concurrency * 10),
		"total_token_throughput": float64(concurrency * 12),
	}
	row["max_concurrency"] = concurrency
	data, _ := json.Marshal(row)
	if err := os.WriteFile(resultPath, append(data, '\n'), 0o644); err != nil {
		os.Exit(1)
	}
	os.Exit(0)
}

func flagValue(args []string, name string) string {
	for i, arg := range args {
		if arg == name && i+1 < len(args) {
			return args[i+1]
		}
	}
	return ""
}

func shutdownFakeServer(port int) {
	client := &http.Client{Timeout: time.Second}
	req, err := http.NewRequest(http.MethodPost, fmt.Sprintf("http://127.0.0.1:%d/shutdown", port), nil)
	if err == nil {
		if resp, err := client.Do(req); err == nil {
			_ = resp.Body.Close()
		}
	}
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		resp, err := client.Get(fmt.Sprintf("http://127.0.0.1:%d/v1/models", port))
		if err != nil {
			return
		}
		_ = resp.Body.Close()
		time.Sleep(20 * time.Millisecond)
	}
}

func waitForPIDFile(t *testing.T, path string) int {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		data, err := os.ReadFile(path)
		if err == nil {
			pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
			if err == nil && pid > 0 {
				return pid
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("pid file %s was not written", path)
	return 0
}

func waitForFile(t *testing.T, path string) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(path); err == nil {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("file %s was not written", path)
}

func processExists(pid int) bool {
	err := syscall.Kill(pid, 0)
	return err == nil || err == syscall.EPERM
}

func freeTestPort() int {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 19191
	}
	defer listener.Close()
	return listener.Addr().(*net.TCPAddr).Port
}

func TestWorkloadPromptFieldValidation(t *testing.T) {
	spec := testSpec()
	spec.Workloads[0].NumPrompts = 8
	spec.Workloads[0].PromptsPerUser = 2
	if err := ValidateSpec(spec); err == nil || !strings.Contains(err.Error(), "not both") {
		t.Fatalf("ValidateSpec error = %v, want mutual exclusion", err)
	}
	spec.Workloads[0].NumPrompts = 0
	spec.Workloads[0].PromptsPerUser = -1
	if err := ValidateSpec(spec); err == nil || !strings.Contains(err.Error(), "must not be negative") {
		t.Fatalf("ValidateSpec error = %v, want negative rejection", err)
	}
	spec.Workloads[0].PromptsPerUser = 0
	if err := ValidateSpec(spec); err == nil || !strings.Contains(err.Error(), "num_prompts or prompts_per_user") {
		t.Fatalf("ValidateSpec error = %v, want either-field requirement", err)
	}
}

func TestDatasetSampleCountDefaults(t *testing.T) {
	// Fixed num_prompts flows into the sample count.
	fixed := testCustomJSONLWorkload("fixed", "requests.jsonl", []string{"8k"})
	fixed.Dataset.SampleCount = 0
	fixed.NumPrompts = 5
	applyWorkloadDefault(&fixed)
	if fixed.Dataset.SampleCount != 5 {
		t.Fatalf("sample count = %d, want 5 from num_prompts", fixed.Dataset.SampleCount)
	}
	// An explicit sample count backfills num_prompts only without scaling.
	backfill := testCustomJSONLWorkload("backfill", "requests.jsonl", []string{"8k"})
	backfill.Dataset.SampleCount = 7
	backfill.NumPrompts = 0
	applyWorkloadDefault(&backfill)
	if backfill.NumPrompts != 7 {
		t.Fatalf("num_prompts = %d, want 7 backfilled", backfill.NumPrompts)
	}
	scaled := testCustomJSONLWorkload("scaled", "requests.jsonl", []string{"8k"})
	scaled.Dataset.SampleCount = 7
	scaled.NumPrompts = 0
	scaled.PromptsPerUser = 2
	applyWorkloadDefault(&scaled)
	if scaled.NumPrompts != 0 {
		t.Fatalf("num_prompts = %d, want 0 when prompts scale", scaled.NumPrompts)
	}
}

func TestPrefixCachingResolvedFromArgs(t *testing.T) {
	spec := testSpec()
	spec.Profiles[0].EnablePrefixCaching = nil
	spec.Profiles[0].Args = append(spec.Profiles[0].Args, "--no-enable-prefix-caching")
	ApplyDefaults(&spec)
	if got := spec.Profiles[0].EnablePrefixCaching; got == nil || *got {
		t.Fatalf("prefix caching = %v, want resolved false from --no-enable-prefix-caching", got)
	}

	spec = testSpec()
	spec.Profiles[0].EnablePrefixCaching = nil
	spec.Profiles[0].Args = append(spec.Profiles[0].Args, "--no-enable-prefix-caching")
	spec.Profiles[0].EngineArgs = append(spec.Profiles[0].EngineArgs, "--enable-prefix-caching=true")
	ApplyDefaults(&spec)
	if got := spec.Profiles[0].EnablePrefixCaching; got == nil || !*got {
		t.Fatalf("prefix caching = %v, want last occurrence true", got)
	}

	spec = testSpec()
	spec.Profiles[0].EnablePrefixCaching = nil
	ApplyDefaults(&spec)
	if got := spec.Profiles[0].EnablePrefixCaching; got != nil {
		t.Fatalf("prefix caching = %v, want unknown without any signal", got)
	}
}

func TestValidateLadderTrimBranches(t *testing.T) {
	if issues := validateGeneratorStamp(nil); len(issues) != 0 {
		t.Fatalf("nil stamp issues = %v, want none", issues)
	}
	stamp := &GeneratorStamp{LadderTrims: []LadderTrim{
		{Context: 0, MaxConcurrency: 8, Reason: "r"},
		{Context: 65536, MaxConcurrency: 0, Reason: "r"},
		{Context: 65536, MaxConcurrency: 8, Reason: "  "},
		{Context: 65536, MaxConcurrency: 8, Reason: "ok"},
	}}
	issues := validateGeneratorStamp(stamp)
	if len(issues) != 3 {
		t.Fatalf("issues = %v, want one per invalid trim", issues)
	}
}
