// Package sweepplan generates the default context/concurrency sweep grid
// with contract-compliant shapes and declared context semantics, per
// docs/2026-07-02-default-inference-sweep.md and
// docs/2026-07-02-context-semantics.md. Agents and users should generate
// sweeps here instead of hand-authoring the grid; generated specs must
// round-trip ValidateSpec with zero issues, which is enforced by test and by
// Plan itself.
package sweepplan

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"

	"github.com/osolmaz/localperf/internal/vllmbench"
)

type PlanRequest struct {
	// Model is the model identifier to benchmark (required).
	Model string `json:"model"`
	// Contexts is the active-context ladder in tokens, e.g. 4096..131072.
	Contexts []int `json:"contexts,omitempty"`
	// Concurrency levels per point, e.g. 1,4,8,16,32.
	Concurrency []int `json:"concurrency,omitempty"`
	// Repeats per measurement; defaults to 1.
	Repeats int `json:"repeats,omitempty"`
	// NumPrompts fixes the request count per measurement; when 0, prompts
	// scale with concurrency via PromptsPerUser instead.
	NumPrompts int `json:"num_prompts,omitempty"`
	// PromptsPerUser scales requests with concurrency
	// (num_prompts = max(8, PromptsPerUser * concurrency)); defaults to 2.
	PromptsPerUser int `json:"prompts_per_user,omitempty"`
	// IncludeReference adds the 4k max-throughput-reference capacity family.
	IncludeReference bool `json:"include_reference,omitempty"`
	// IncludeStress adds long-output decode spot checks (4096 tokens at
	// 32k c4 and 64k c1/c4 when those contexts are in the ladder) and the
	// 128k points at c1/c4.
	IncludeStress bool `json:"include_stress,omitempty"`
	// ProfileArgs are appended to every generated profile as serve CLI args.
	ProfileArgs []string `json:"profile_args,omitempty"`
	// ProfileEngineArgs are appended to every generated profile as engine
	// CLI args. Use OmitProfileEngineFlags to remove unsafe inherited flags.
	ProfileEngineArgs []string `json:"profile_engine_args,omitempty"`
	// OmitProfileEngineFlags removes matching "--flag value" and
	// "--flag=value" entries from ProfileEngineArgs before emitting profiles.
	OmitProfileEngineFlags []string `json:"omit_profile_engine_flags,omitempty"`
	// MinMemAvailableGiB is the safety memory floor; defaults to 40.
	MinMemAvailableGiB float64 `json:"min_mem_available_gib,omitempty"`
	// BasePort is the first server port; defaults to 8100.
	BasePort int `json:"base_port,omitempty"`
	// VLLMCommand overrides the vllm executable for managed serves — the
	// machine-specific runtime path that previously forced hand-editing.
	VLLMCommand string `json:"vllm_command,omitempty"`
	// GPUMemoryUtilization applies to every generated profile when set.
	GPUMemoryUtilization float64 `json:"gpu_memory_utilization,omitempty"`
	// KVCacheMemoryBytes pins vLLM's KV cache size on every profile via
	// --kv-cache-memory-bytes when set.
	KVCacheMemoryBytes int64 `json:"kv_cache_memory_bytes,omitempty"`
	// Trims are declared author decisions to cap a context's concurrency
	// ladder; each needs a reason and renders in reports like an adaptive
	// skip, never as a silent hole.
	Trims []vllmbench.LadderTrim `json:"trims,omitempty"`
}

const (
	defaultPromptsPerUser = 2
	defaultMemFloorGiB    = 40
	defaultBasePort       = 8100
	referenceContext      = 4096
	decodeOutputTokens    = 1024
	stressOutputTokens    = 4096
	prefillOutputTokens   = 1
	stressContext         = 131072
	// highContextConcurrencyCap bounds contexts >= 128k to c1/c4: beyond
	// that the KV budget makes higher concurrency an hours-long stress
	// exercise, which belongs in --stress, not the default grid.
	highContextConcurrencyCap = 4
	fixedKVCacheBytesFlag     = "--kv-cache-memory-bytes"
)

// headroom absorbs chat template and tokenizer drift between requested and
// measured token counts; shapes land at roughly 98% of the target, well
// inside the 90% contract floor.
func headroom(context int) int {
	if context/64 > 64 {
		return context / 64
	}
	return 64
}

// PrefillShape returns input/output lengths for an active prefill point:
// minimal output so the run measures prefill through TTFT, not decode.
func PrefillShape(context int) (inputLen, outputLen int) {
	return context - headroom(context) - prefillOutputTokens, prefillOutputTokens
}

// DecodeShape returns input/output lengths for an active decode point: long
// prompt plus 1024 generated tokens. That is still hundreds of steady-state
// decode steps, and the shorter output keeps the measured active range close
// to the target; 4096-token outputs are the --stress preset's job.
func DecodeShape(context int) (inputLen, outputLen int) {
	return decodeShapeWithOutput(context, decodeOutputTokens)
}

// StressDecodeShape is the long-output variant used by stress spot checks.
func StressDecodeShape(context int) (inputLen, outputLen int) {
	return decodeShapeWithOutput(context, stressOutputTokens)
}

func decodeShapeWithOutput(context, output int) (inputLen, outputLen int) {
	if output > context/4 {
		output = context / 4
	}
	return context - output - headroom(context), output
}

// Plan generates a validated sweep spec. Every workload carries a declared
// context claim: ladder points are active-context claims, the reference
// family is a capacity claim.
func Plan(request PlanRequest) (vllmbench.Spec, error) {
	if strings.TrimSpace(request.Model) == "" {
		return vllmbench.Spec{}, fmt.Errorf("model is required")
	}
	if len(request.Contexts) == 0 && !request.IncludeReference {
		return vllmbench.Spec{}, fmt.Errorf("at least one context point or the reference family is required")
	}
	for index, trim := range request.Trims {
		if trim.Context <= 0 || trim.MaxConcurrency <= 0 {
			return vllmbench.Spec{}, fmt.Errorf("trims[%d]: context and max_concurrency must be positive", index)
		}
		if strings.TrimSpace(trim.Reason) == "" {
			return vllmbench.Spec{}, fmt.Errorf("trims[%d]: a reason is required — declared trims render in reports", index)
		}
		if !containsInt(request.Contexts, trim.Context) && !(request.IncludeStress && trim.Context == stressContext) {
			return vllmbench.Spec{}, fmt.Errorf("trims[%d]: context %d is not in the generated ladder", index, trim.Context)
		}
	}
	if len(request.Concurrency) == 0 {
		request.Concurrency = []int{1, 4, 8, 16, 32}
	}
	if request.NumPrompts > 0 {
		// A fixed count and scaling are mutually exclusive.
		request.PromptsPerUser = 0
	} else if request.PromptsPerUser <= 0 {
		request.PromptsPerUser = defaultPromptsPerUser
	}
	if request.MinMemAvailableGiB <= 0 {
		request.MinMemAvailableGiB = defaultMemFloorGiB
	}
	if request.BasePort <= 0 {
		request.BasePort = defaultBasePort
	}

	warmupSeed := 0
	vllmCommand := strings.TrimSpace(request.VLLMCommand)
	if vllmCommand == "" {
		vllmCommand = "vllm"
	}
	spec := vllmbench.Spec{
		Version:     "1",
		Name:        fmt.Sprintf("%s-default-sweep", vllmbench.Slug(request.Model)),
		Description: "Default context/concurrency sweep generated by localperf sweep plan.",
		Model:       request.Model,
		Engines: []vllmbench.EngineConfig{{
			Name:         "vllm",
			Type:         "vllm-managed",
			Command:      vllmCommand,
			BenchCommand: vllmCommand,
		}},
		Safety: vllmbench.SafetyConfig{MinMemAvailableGiB: request.MinMemAvailableGiB},
		Warmup: vllmbench.WarmupConfig{
			Enabled:        true,
			NumPrompts:     4,
			MaxConcurrency: 1,
			BenchmarkTrafficConfig: vllmbench.BenchmarkTrafficConfig{
				DatasetName:      "random",
				Seed:             &warmupSeed,
				RandomInputLen:   256,
				RandomOutputLen:  16,
				RandomRangeRatio: "0",
			},
		},
	}
	// The server must admit at least the highest requested concurrency;
	// high-context profiles cap max_num_seqs to their capped ladder.
	maxNumSeqs := maxConcurrencyOf(request.Concurrency)
	port := request.BasePort
	if request.IncludeReference {
		spec.Profiles = append(spec.Profiles, sweepProfile("4k-reference", referenceContext, port, maxNumSeqs, request))
		port++
		spec.Workloads = append(spec.Workloads, sweepWorkload(
			"max-throughput-reference", "decode", "4k-reference",
			referenceContext, vllmbench.ContextSemanticsCapacity,
			1024, 1024, request))
	}
	contexts := append([]int{}, request.Contexts...)
	if request.IncludeStress && !containsInt(contexts, stressContext) {
		contexts = append(contexts, stressContext)
	}
	for _, context := range contexts {
		label := vllmbench.TokenCountLabel(context)
		spec.Profiles = append(spec.Profiles, sweepProfile(label, context, port, contextMaxSeqs(context, request), request))
		port++
		prefillInput, prefillOutput := PrefillShape(context)
		spec.Workloads = append(spec.Workloads, sweepWorkload(
			fmt.Sprintf("prefill-%s", label), "prefill", label,
			context, vllmbench.ContextSemanticsActive,
			prefillInput, prefillOutput, request))
		decodeInput, decodeOutput := DecodeShape(context)
		spec.Workloads = append(spec.Workloads, sweepWorkload(
			fmt.Sprintf("decode-%s", label), "decode", label,
			context, vllmbench.ContextSemanticsActive,
			decodeInput, decodeOutput, request))
	}
	if request.IncludeStress {
		spec.Workloads = append(spec.Workloads, stressWorkloads(contexts, request)...)
	}

	applyRuntimeIntent(&spec, request)

	// Emit the normalized spec: explicit defaults are stable to diff and
	// leave nothing for a reader to guess.
	vllmbench.ApplyDefaults(&spec)
	if err := vllmbench.ValidateSpec(spec); err != nil {
		return vllmbench.Spec{}, fmt.Errorf("generated spec failed validation (generator/validator drift): %w", err)
	}
	if err := stampSpec(&spec, request); err != nil {
		return vllmbench.Spec{}, err
	}
	return spec, nil
}

// applyRuntimeIntent carries machine-specific runtime choices into the spec
// so nothing forces hand-editing the generated file.
func applyRuntimeIntent(spec *vllmbench.Spec, request PlanRequest) {
	for index := range spec.Profiles {
		if request.GPUMemoryUtilization > 0 {
			spec.Profiles[index].GPUMemoryUtilization = request.GPUMemoryUtilization
		}
		// Qwen runtimes reject a fixed KV cache size; the same rule that
		// strips the inherited engine flag applies to the intent flag.
		if request.KVCacheMemoryBytes > 0 && !shouldOmitFixedKVCache(request.Model) {
			spec.Profiles[index].Args = append(spec.Profiles[index].Args,
				fixedKVCacheBytesFlag, strconv.FormatInt(request.KVCacheMemoryBytes, 10))
		}
	}
}

// generatorTool and generatorVersion identify the stamp format, not the
// binary build: the content hash is what verification relies on.
const (
	generatorTool    = "localperf sweep plan"
	generatorVersion = "1"
)

// stampSpec writes the provenance record: the intent that produced the
// spec, declared ladder trims, and a content hash over the spec with the
// stamp removed. `bench run` and reports verify the hash; any edit after
// generation demotes the spec to a custom grid.
func stampSpec(spec *vllmbench.Spec, request PlanRequest) error {
	intent, err := json.Marshal(request)
	if err != nil {
		return err
	}
	spec.Generator = &vllmbench.GeneratorStamp{
		Tool:        generatorTool,
		Version:     generatorVersion,
		Intent:      intent,
		LadderTrims: append([]vllmbench.LadderTrim{}, request.Trims...),
	}
	hash, err := vllmbench.SpecContentHash(*spec)
	if err != nil {
		return err
	}
	spec.Generator.ContentHash = hash
	return nil
}

func sweepProfile(name string, maxModelLen, port, maxNumSeqs int, request PlanRequest) vllmbench.Profile {
	omittedEngineFlags := append([]string{}, request.OmitProfileEngineFlags...)
	if shouldOmitFixedKVCache(request.Model) {
		omittedEngineFlags = append(omittedEngineFlags, fixedKVCacheBytesFlag)
	}
	return vllmbench.Profile{
		Name:        name,
		Engine:      "vllm",
		Model:       request.Model,
		Host:        "127.0.0.1",
		Port:        port,
		Managed:     true,
		MaxModelLen: maxModelLen,
		MaxNumSeqs:  maxNumSeqs,
		Args:        append([]string{}, request.ProfileArgs...),
		EngineArgs:  omittedFlagValues(request.ProfileEngineArgs, omittedEngineFlags),
	}
}

func shouldOmitFixedKVCache(model string) bool {
	normalized := strings.ToLower(model)
	return strings.Contains(normalized, "qwen")
}

func omittedFlagValues(args, omittedFlags []string) []string {
	if len(args) == 0 {
		return nil
	}
	if len(omittedFlags) == 0 {
		return append([]string{}, args...)
	}
	omitted := map[string]struct{}{}
	for _, flag := range omittedFlags {
		if strings.TrimSpace(flag) != "" {
			omitted[flag] = struct{}{}
		}
	}
	filtered := make([]string, 0, len(args))
	for index := 0; index < len(args); index++ {
		arg := args[index]
		if _, ok := omitted[arg]; ok {
			// Skip a separate value ("--flag value") but never a following
			// flag: boolean flags carry no value token.
			if index+1 < len(args) && !strings.HasPrefix(args[index+1], "--") {
				index++
			}
			continue
		}
		skip := false
		for flag := range omitted {
			if strings.HasPrefix(arg, flag+"=") {
				skip = true
				break
			}
		}
		if !skip {
			filtered = append(filtered, arg)
		}
	}
	return filtered
}

// stressWorkloads adds long-output decode spot checks for ladder contexts
// that have them: 4096-token output at 32k c4 and 64k c1/c4. Kept out of the
// default grid because they dominate sweep wall time.
var stressSpots = []struct {
	context     int
	concurrency []int
}{
	{32768, []int{4}},
	{65536, []int{1, 4}},
}

func stressWorkloads(contexts []int, request PlanRequest) []vllmbench.Workload {
	var workloads []vllmbench.Workload
	for _, spot := range stressSpots {
		if !containsInt(contexts, spot.context) {
			continue
		}
		label := vllmbench.TokenCountLabel(spot.context)
		input, output := StressDecodeShape(spot.context)
		spotRequest := request
		spotRequest.Concurrency = spot.concurrency
		workloads = append(workloads, sweepWorkload(
			fmt.Sprintf("decode-stress-%s", label), "decode", label,
			spot.context, vllmbench.ContextSemanticsActive,
			input, output, spotRequest))
	}
	return workloads
}

// contextMaxSeqs sizes a context profile's server admission for its capped
// ladder plus any stress spot checks attached to that context; a stress c4
// point must not run against a server that only admits c1.
func contextMaxSeqs(context int, request PlanRequest) int {
	seqs := maxConcurrencyOf(pointConcurrency(context, request.Concurrency, request.Trims))
	if request.IncludeStress {
		for _, spot := range stressSpots {
			if spot.context == context {
				seqs = max(seqs, maxConcurrencyOf(spot.concurrency))
			}
		}
	}
	return seqs
}

func maxConcurrencyOf(values []int) int {
	largest := 0
	for _, value := range values {
		if value > largest {
			largest = value
		}
	}
	return largest
}

func containsInt(values []int, wanted int) bool {
	for _, value := range values {
		if value == wanted {
			return true
		}
	}
	return false
}

// pointConcurrency caps contexts >= 128k at c4; higher concurrency at those
// KV budgets is stress territory, not the default grid.
func pointConcurrency(context int, concurrency []int, trims []vllmbench.LadderTrim) []int {
	cap := 0
	if context >= stressContext {
		cap = highContextConcurrencyCap
	}
	for _, trim := range trims {
		if trim.Context == context && (cap == 0 || trim.MaxConcurrency < cap) {
			cap = trim.MaxConcurrency
		}
	}
	if cap == 0 {
		return append([]int{}, concurrency...)
	}
	capped := []int{}
	for _, value := range concurrency {
		if value <= cap {
			capped = append(capped, value)
		}
	}
	if len(capped) == 0 {
		capped = []int{1}
	}
	return capped
}

func sweepWorkload(name, phase, profile string, target int, semantics string, inputLen, outputLen int, request PlanRequest) vllmbench.Workload {
	seed := 0
	temperature := 0.0
	workload := vllmbench.Workload{
		Name:             name,
		Role:             vllmbench.WorkloadRoleBenchmark,
		Phase:            phase,
		Profiles:         []string{profile},
		ContextTarget:    target,
		ContextSemantics: semantics,
		BenchmarkTrafficConfig: vllmbench.BenchmarkTrafficConfig{
			Backend:          "openai-chat",
			DatasetName:      "random",
			Seed:             &seed,
			RandomInputLen:   inputLen,
			RandomOutputLen:  outputLen,
			RandomRangeRatio: "0",
			RequestRate:      "inf",
		},
		NumPrompts:     request.NumPrompts,
		PromptsPerUser: request.PromptsPerUser,
		MaxConcurrency: pointConcurrency(target, request.Concurrency, request.Trims),
		IgnoreEOS:      true,
		Temperature:    &temperature,
	}
	if request.Repeats > 0 {
		workload.Repeats = request.Repeats
	}
	return workload
}
