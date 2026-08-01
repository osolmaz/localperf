package vllmbench

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/osolmaz/localperf/internal/artifact"

	"github.com/osolmaz/localperf/internal/bench"
	"github.com/osolmaz/localperf/internal/collections"
)

const DefaultHealthPath = "/v1/models"

const (
	LoadGeneratorVLLMBench = "vllm_bench"
	LoadGeneratorHTTP      = "localperf_http"

	WorkloadRoleBenchmark  = "benchmark"
	WorkloadRoleDiagnostic = "diagnostic"
)

// TTFTSourceStream marks TTFT stats measured from streamed responses. It is
// the only trusted TTFT provenance: reports render TTFT solely when a
// measurement carries this marker.
const TTFTSourceStream = "stream"

// Context semantics contract; see docs/2026-07-02-context-semantics.md.
const (
	ContextSemanticsActive   = "active"
	ContextSemanticsCapacity = "capacity"
	// ContextTargetMinFrac is the strict lower bound of the active-context
	// band: requested (and later measured) tokens must land within
	// [0.90, 1.00] of context_target. Widen only via the contract doc.
	ContextTargetMinFrac = 0.90
)

type Spec struct {
	Version     string                   `json:"version"`
	Name        string                   `json:"name"`
	Description string                   `json:"description,omitempty"`
	Model       string                   `json:"model"`
	OutputDir   string                   `json:"output_dir,omitempty"`
	Env         map[string]string        `json:"env,omitempty"`
	Engines     []EngineConfig           `json:"engines,omitempty"`
	Runner      RunnerConfig             `json:"runner"`
	Safety      SafetyConfig             `json:"safety"`
	Warmup      WarmupConfig             `json:"warmup,omitempty"`
	Profiles    []Profile                `json:"profiles"`
	Workloads   []Workload               `json:"workloads"`
	Generator   *artifact.GeneratorStamp `json:"generator,omitempty"`

	// Provenance is computed at load time from the generator stamp; it is
	// never serialized or taken on trust.
	Provenance string `json:"-"`
}

// SpecContentHash hashes the canonical spec document with the generator
// stamp removed; see artifact.CanonicalSpecHash.
func SpecContentHash(spec Spec) (string, error) {
	data, err := json.Marshal(spec)
	if err != nil {
		return "", err
	}
	return artifact.CanonicalSpecHash(data)
}

// Provenance types and statuses re-exported for callers of this package,
// so higher layers do not need to import the artifact format directly.
type (
	GeneratorStamp = artifact.GeneratorStamp
	LadderTrim     = artifact.LadderTrim
)

const (
	SpecProvenanceGenerated = artifact.SpecProvenanceGenerated
	SpecProvenanceEdited    = artifact.SpecProvenanceEdited
	SpecProvenanceCustom    = artifact.SpecProvenanceCustom
)

// SpecProvenance verifies a spec's generator stamp: "generated" when the
// content hash matches, "edited" when a stamp exists but the content
// changed after generation, "custom" when there is no stamp.
func SpecProvenance(spec Spec) string {
	data, err := json.Marshal(spec)
	if err != nil {
		return artifact.SpecProvenanceCustom
	}
	status, _ := artifact.VerifySpecProvenance(data)
	return status
}

type EngineConfig struct {
	Name            string            `json:"name"`
	Type            string            `json:"type"`
	Command         string            `json:"command,omitempty"`
	BenchCommand    string            `json:"bench_command,omitempty"`
	Managed         *bool             `json:"managed,omitempty"`
	EndpointBaseURL string            `json:"endpoint_base_url,omitempty"`
	Env             map[string]string `json:"env,omitempty"`
	Metadata        map[string]any    `json:"metadata,omitempty"`
}

type RunnerConfig struct {
	OneAwakeProfile      *bool          `json:"one_awake_profile,omitempty"`
	PrebootProfiles      bool           `json:"preboot_profiles,omitempty"`
	StopManagedOnExit    *bool          `json:"stop_managed_on_exit,omitempty"`
	AppendTimestampToRun *bool          `json:"append_timestamp_to_run,omitempty"`
	Adaptive             AdaptiveConfig `json:"adaptive,omitempty"`
}

// AdaptiveConfig automates the sparse-search rule from
// docs/2026-07-02-default-inference-sweep.md: per profile and workload,
// concurrency runs ascending and higher points are skipped, with the reason
// recorded, once a stop rule fires. Negative thresholds disable a rule.
type AdaptiveConfig struct {
	// Enabled defaults to true; {"enabled": false} opts out entirely.
	Enabled *bool `json:"enabled,omitempty"`
	// MinThroughputGainPct stops the ladder when the phase throughput
	// improved less than this over the previous concurrency (default 10).
	MinThroughputGainPct float64 `json:"min_throughput_gain_pct,omitempty"`
	// TTFTP99CeilingMillis stops the ladder when a point's p99 TTFT
	// exceeds the ceiling; 0 leaves the rule off.
	TTFTP99CeilingMillis float64 `json:"ttft_p99_ceiling_ms,omitempty"`
	// MaxConcurrencyFactor skips points whose concurrency exceeds this
	// multiple of vLLM's reported maximum concurrency (default 2).
	MaxConcurrencyFactor float64 `json:"max_concurrency_factor,omitempty"`
}

const (
	defaultMinThroughputGainPct = 10
	defaultMaxConcurrencyFactor = 2
)

func (config AdaptiveConfig) enabled() bool {
	return config.Enabled == nil || *config.Enabled
}

type SafetyConfig struct {
	MinMemAvailableGiB float64 `json:"min_mem_available_gib"`
	PollIntervalMillis int     `json:"poll_interval_millis,omitempty"`
	StartupTimeoutSec  int     `json:"startup_timeout_sec,omitempty"`
	WorkloadTimeoutSec int     `json:"workload_timeout_sec,omitempty"`
	HTTPTimeoutSec     int     `json:"http_timeout_sec,omitempty"`
}

type WarmupConfig struct {
	BenchmarkTrafficConfig
	Enabled        bool `json:"enabled"`
	NumPrompts     int  `json:"num_prompts,omitempty"`
	MaxConcurrency int  `json:"max_concurrency,omitempty"`
}

type Profile struct {
	Name                 string            `json:"name"`
	Engine               string            `json:"engine,omitempty"`
	Model                string            `json:"model,omitempty"`
	Host                 string            `json:"host,omitempty"`
	Port                 int               `json:"port"`
	EndpointBaseURL      string            `json:"endpoint_base_url,omitempty"`
	Managed              bool              `json:"managed"`
	EnableSleepMode      bool              `json:"enable_sleep_mode,omitempty"`
	SleepLevel           *int              `json:"sleep_level,omitempty"`
	HealthPath           string            `json:"health_path,omitempty"`
	MaxModelLen          int               `json:"max_model_len,omitempty"`
	MaxNumSeqs           int               `json:"max_num_seqs,omitempty"`
	MaxNumBatchedTokens  int               `json:"max_num_batched_tokens,omitempty"`
	GPUMemoryUtilization float64           `json:"gpu_memory_utilization,omitempty"`
	KVCacheDType         string            `json:"kv_cache_dtype,omitempty"`
	AttentionBackend     string            `json:"attention_backend,omitempty"`
	MoEBackend           string            `json:"moe_backend,omitempty"`
	EnablePrefixCaching  *bool             `json:"enable_prefix_caching,omitempty"`
	Env                  map[string]string `json:"env,omitempty"`
	Args                 []string          `json:"args,omitempty"`
	EngineArgs           []string          `json:"engine_args,omitempty"`
}

type Workload struct {
	BenchmarkTrafficConfig
	Name                    string      `json:"name"`
	Role                    string      `json:"role"`
	Phase                   string      `json:"phase,omitempty"`
	ContextTarget           int         `json:"context_target"`
	ContextSemantics        string      `json:"context_semantics"`
	SLO                     *SLOConfig  `json:"slo,omitempty"`
	LoadGenerator           string      `json:"load_generator,omitempty"`
	Dataset                 DatasetSpec `json:"dataset,omitempty"`
	Request                 RequestSpec `json:"request,omitempty"`
	Profiles                []string    `json:"profiles,omitempty"`
	NumPrompts              int         `json:"num_prompts"`
	PromptsPerUser          int         `json:"prompts_per_user,omitempty"`
	Batches                 []Batch     `json:"batches,omitempty"`
	Repeats                 int         `json:"repeats,omitempty"`
	MaxConcurrency          []int       `json:"max_concurrency"`
	Stream                  *bool       `json:"stream,omitempty"`
	IgnoreEOS               bool        `json:"ignore_eos,omitempty"`
	Temperature             *float64    `json:"temperature,omitempty"`
	CapturePayloadArtifacts bool        `json:"capture_payload_artifacts,omitempty"`
}

// Batch records the exact number of requests in one concurrency point.
// Suite compilation uses this instead of an implicit prompts-per-user rule.
type Batch struct {
	Concurrency int `json:"concurrency"`
	Requests    int `json:"requests"`
}

// SLOConfig declares latency targets for goodput derivation: the fraction of
// completed requests meeting every set target, and goodput as SLO-met
// requests per second. Rendered only when declared; the report never invents
// a quality bar.
type SLOConfig struct {
	TTFTP95Millis float64 `json:"ttft_p95_ms,omitempty"`
	E2ELP95Millis float64 `json:"e2el_p95_ms,omitempty"`
}

type BenchmarkTrafficConfig struct {
	Backend                     string   `json:"backend,omitempty"`
	Endpoint                    string   `json:"endpoint,omitempty"`
	DatasetName                 string   `json:"dataset_name,omitempty"`
	DatasetPath                 string   `json:"dataset_path,omitempty"`
	RequestRate                 string   `json:"request_rate,omitempty"`
	Seed                        *int     `json:"seed,omitempty"`
	RandomInputLen              int      `json:"random_input_len,omitempty"`
	RandomOutputLen             int      `json:"random_output_len,omitempty"`
	RandomRangeRatio            string   `json:"random_range_ratio,omitempty"`
	RandomPrefixLen             int      `json:"random_prefix_len,omitempty"`
	CustomOutputLen             *int     `json:"custom_output_len,omitempty"`
	ShareGPTOutputLen           *int     `json:"sharegpt_output_len,omitempty"`
	SonnetInputLen              int      `json:"sonnet_input_len,omitempty"`
	SonnetOutputLen             int      `json:"sonnet_output_len,omitempty"`
	SonnetPrefixLen             int      `json:"sonnet_prefix_len,omitempty"`
	PrefixRepetitionPrefixLen   int      `json:"prefix_repetition_prefix_len,omitempty"`
	PrefixRepetitionSuffixLen   int      `json:"prefix_repetition_suffix_len,omitempty"`
	PrefixRepetitionNumPrefixes int      `json:"prefix_repetition_num_prefixes,omitempty"`
	PrefixRepetitionOutputLen   int      `json:"prefix_repetition_output_len,omitempty"`
	SpeedBenchDatasetSubset     string   `json:"speed_bench_dataset_subset,omitempty"`
	SpeedBenchOutputLen         int      `json:"speed_bench_output_len,omitempty"`
	SpeedBenchCategory          string   `json:"speed_bench_category,omitempty"`
	DisableShuffle              bool     `json:"disable_shuffle,omitempty"`
	NoOversample                bool     `json:"no_oversample,omitempty"`
	SkipChatTemplate            bool     `json:"skip_chat_template,omitempty"`
	SaveDetailed                *bool    `json:"save_detailed,omitempty"`
	PlotDatasetStats            bool     `json:"plot_dataset_stats,omitempty"`
	ExtraBody                   string   `json:"extra_body,omitempty"`
	Metadata                    []string `json:"metadata,omitempty"`
	Goodput                     []string `json:"goodput,omitempty"`
	ExtraArgs                   []string `json:"extra_args,omitempty"`
}

type PlannedRun struct {
	Profile     Profile  `json:"profile"`
	Workload    Workload `json:"workload"`
	Concurrency int      `json:"concurrency"`
	Repeat      int      `json:"repeat,omitempty"`
	ResultFile  string   `json:"result_file"`
}

func ApplyDefaults(spec *Spec) {
	applyRunnerDefaults(spec)
	applySafetyDefaults(spec)
	applyProfileDefaults(spec)
	applyWarmupDefaults(&spec.Warmup)
	applyWorkloadDefaults(spec.Workloads)
}

func applyRunnerDefaults(spec *Spec) {
	if spec.Runner.Adaptive.MinThroughputGainPct == 0 {
		spec.Runner.Adaptive.MinThroughputGainPct = defaultMinThroughputGainPct
	}
	if spec.Runner.Adaptive.MaxConcurrencyFactor == 0 {
		spec.Runner.Adaptive.MaxConcurrencyFactor = defaultMaxConcurrencyFactor
	}
	defaultTrue(&spec.Runner.OneAwakeProfile)
	defaultTrue(&spec.Runner.StopManagedOnExit)
	defaultTrue(&spec.Runner.AppendTimestampToRun)
}

func defaultTrue(value **bool) {
	if *value == nil {
		yes := true
		*value = &yes
	}
}

func applySafetyDefaults(spec *Spec) {
	defaultPositiveInt(&spec.Safety.PollIntervalMillis, 1000)
	defaultPositiveInt(&spec.Safety.StartupTimeoutSec, 900)
	defaultPositiveInt(&spec.Safety.WorkloadTimeoutSec, 1800)
	defaultPositiveInt(&spec.Safety.HTTPTimeoutSec, 15)
}

func defaultPositiveInt(value *int, fallback int) {
	if *value <= 0 {
		*value = fallback
	}
}

func applyProfileDefaults(spec *Spec) {
	for i := range spec.Profiles {
		applyProfileDefault(&spec.Profiles[i], spec)
	}
}

func applyProfileDefault(profile *Profile, spec *Spec) {
	if strings.TrimSpace(profile.Engine) == "" {
		profile.Engine = spec.Engines[0].Name
	}
	if strings.TrimSpace(profile.Model) == "" {
		profile.Model = spec.Model
	}
	if strings.TrimSpace(profile.Host) == "" {
		profile.Host = "127.0.0.1"
	}
	if strings.TrimSpace(profile.HealthPath) == "" {
		profile.HealthPath = DefaultHealthPath
	}
	applyPrefixCachingDefault(profile)
	if profile.EnableSleepMode && profile.SleepLevel == nil {
		profile.SleepLevel = intPointer(2)
	}
}

func applyWarmupDefaults(warmup *WarmupConfig) {
	if !warmup.Enabled {
		return
	}
	applyTrafficDefaults(&warmup.BenchmarkTrafficConfig, "random")
	defaultPositiveInt(&warmup.RandomInputLen, 256)
	defaultPositiveInt(&warmup.RandomOutputLen, 16)
	defaultPositiveInt(&warmup.NumPrompts, 4)
	defaultPositiveInt(&warmup.MaxConcurrency, 1)
}

func applyWorkloadDefaults(workloads []Workload) {
	for i := range workloads {
		applyWorkloadDefault(&workloads[i])
	}
}

func applyWorkloadDefault(workload *Workload) {
	applyWorkloadConcurrencyDefaults(workload)
	applyStructuredWorkloadDefaults(workload)
	applyWorkloadExecutionDefaults(workload)
	applyTrafficDefaults(&workload.BenchmarkTrafficConfig, "")
	applyLoadGeneratorDefault(workload)
	workload.Phase = workloadPhase(*workload)
	// Goodput derivation needs per-request rows; an SLO without detailed
	// output would silently render "-" for every run.
	if workload.SLO != nil && workload.SaveDetailed == nil {
		saveDetailed := true
		workload.SaveDetailed = &saveDetailed
	}
}

// applyWorkloadConcurrencyDefaults sorts the declared concurrency ladder.
func applyWorkloadConcurrencyDefaults(workload *Workload) {
	if len(workload.Batches) > 0 {
		workload.MaxConcurrency = workload.MaxConcurrency[:0]
		for _, batch := range workload.Batches {
			workload.MaxConcurrency = append(workload.MaxConcurrency, batch.Concurrency)
		}
	}
	// Ladders run ascending: the documented sparse search depends on it and
	// the adaptive stop rules compare against the previous (lower) point.
	sort.Ints(workload.MaxConcurrency)
}

// largestConcurrency is the workload's biggest ladder point; sample counts
// for structured datasets and workload-level bookkeeping derive from it when
// prompts scale with concurrency.
func largestConcurrency(workload Workload) int {
	largest := 1
	for _, concurrency := range workload.MaxConcurrency {
		if concurrency > largest {
			largest = concurrency
		}
	}
	return largest
}

func applyWorkloadExecutionDefaults(workload *Workload) {
	if workload.Repeats <= 0 {
		workload.Repeats = 1
	}
}

func applyStructuredWorkloadDefaults(workload *Workload) {
	if !hasStructuredDataset(*workload) {
		return
	}
	workload.Dataset.Type = normalizeDatasetType(workload.Dataset.Type)
	applyDatasetDefaults(workload)
	applyRequestDefaults(workload)
}

func applyDatasetDefaults(workload *Workload) {
	if strings.TrimSpace(workload.Dataset.Selection) == "" {
		workload.Dataset.Selection = "first_n"
	}
	if workload.Dataset.SampleCount <= 0 {
		workload.Dataset.SampleCount = defaultDatasetSampleCount(*workload)
	}
	if workload.NumPrompts <= 0 && workload.PromptsPerUser <= 0 && workload.Dataset.SampleCount > 0 {
		workload.NumPrompts = workload.Dataset.SampleCount
	}
}

// defaultDatasetSampleCount prepares enough rows for the workload: the fixed
// request count, or the largest ladder point when prompts scale.
func defaultDatasetSampleCount(workload Workload) int {
	if len(workload.Batches) > 0 {
		return resolvedNumPrompts(workload, largestConcurrency(workload))
	}
	if workload.NumPrompts > 0 {
		return workload.NumPrompts
	}
	if workload.PromptsPerUser > 0 {
		return resolvedNumPrompts(workload, largestConcurrency(workload))
	}
	return 0
}

func applyRequestDefaults(workload *Workload) {
	applyRequestModeDefault(workload)
	applyRequestTurnPolicyDefault(workload)
	applyRequestOutputDefault(workload)
}

func applyRequestModeDefault(workload *Workload) {
	if strings.TrimSpace(workload.Request.Mode) == "" {
		workload.Request.Mode = "chat"
	}
}

func applyRequestTurnPolicyDefault(workload *Workload) {
	if strings.TrimSpace(workload.Request.TurnPolicy) == "" && workload.Dataset.Type == "sharegpt" {
		workload.Request.TurnPolicy = "first_user_turn"
	}
}

func applyRequestOutputDefault(workload *Workload) {
	if workload.Request.MaxOutputTokens <= 0 && workload.Dataset.OutputTokens > 0 {
		workload.Request.MaxOutputTokens = workload.Dataset.OutputTokens
	}
}

func workloadPhase(workload Workload) string {
	if phase := normalizeWorkloadPhase(workload.Phase); phase != "" {
		return phase
	}
	return inferWorkloadPhase(
		workload.Name,
		firstNonZeroInt(trafficInputLen(workload.BenchmarkTrafficConfig), structuredInputLen(workload)),
		firstNonZeroInt(trafficOutputLen(workload.BenchmarkTrafficConfig), structuredOutputLen(workload)),
	)
}

func normalizeWorkloadPhase(phase string) string {
	return bench.NormalizeWorkloadPhase(phase)
}

func inferWorkloadPhase(name string, inputLen, outputLen int) string {
	lowerName := strings.ToLower(name)
	if phase := matchContains(lowerName, []containsMapping{
		{Pattern: "prefill", Value: "prefill"},
		{Pattern: "decode", Value: "decode"},
	}, ""); phase != "" {
		return phase
	}
	if phase := phaseFromTokenShape(inputLen, outputLen); phase != "" {
		return phase
	}
	return "mixed"
}

func phaseFromTokenShape(inputLen, outputLen int) string {
	if inputLen <= 0 || outputLen <= 0 {
		return ""
	}
	if outputLen <= 64 && inputLen >= 1024 && inputLen >= 4*outputLen {
		return "prefill"
	}
	if outputLen >= 256 {
		return "decode"
	}
	return ""
}

func applyPrefixCachingDefault(profile *Profile) {
	if profile.EnablePrefixCaching == nil {
		profile.EnablePrefixCaching = prefixCachingFromArgs(profile.Args, profile.EngineArgs)
	}
}

// prefixCachingFromArgs resolves the prefix-caching tri-state from raw serve
// args when the structured field is unset: hand-written specs often pass the
// flag through args, and "unknown" must mean genuinely unknown. The last
// occurrence wins, matching CLI semantics.
func prefixCachingFromArgs(argLists ...[]string) *bool {
	var value *bool
	for _, args := range argLists {
		for _, arg := range args {
			switch strings.TrimSpace(arg) {
			case "--enable-prefix-caching", "--enable-prefix-caching=true":
				enabled := true
				value = &enabled
			case "--no-enable-prefix-caching", "--enable-prefix-caching=false":
				enabled := false
				value = &enabled
			}
		}
	}
	return value
}

func applyTrafficDefaults(traffic *BenchmarkTrafficConfig, defaultDataset string) {
	if strings.TrimSpace(traffic.DatasetName) == "" {
		traffic.DatasetName = defaultDataset
	}
	if strings.TrimSpace(traffic.Backend) == "" {
		traffic.Backend = "openai-chat"
	}
	if strings.TrimSpace(traffic.Endpoint) == "" {
		traffic.Endpoint = defaultEndpoint(traffic.Backend)
	}
	if strings.TrimSpace(traffic.RequestRate) == "" {
		traffic.RequestRate = "inf"
	}
}

func applyLoadGeneratorDefault(workload *Workload) {
	workload.LoadGenerator = normalizeLoadGenerator(workload.LoadGenerator)
	if workload.LoadGenerator == "" {
		workload.LoadGenerator = LoadGeneratorVLLMBench
	}
}

// workloadStreams reports whether http-load requests stream. Streaming is
// the default: TTFT is only measurable on streamed responses.
func workloadStreams(workload Workload) bool {
	return workload.Stream == nil || *workload.Stream
}

func normalizeLoadGenerator(value string) string {
	return strings.TrimSpace(strings.ToLower(value))
}

func RedactedSpec(spec Spec) Spec {
	out := spec
	out.Env = redactedEnv(spec.Env)
	out.Engines = append([]EngineConfig(nil), spec.Engines...)
	for i := range out.Engines {
		out.Engines[i].Env = redactedEnv(spec.Engines[i].Env)
	}
	out.Profiles = append([]Profile(nil), spec.Profiles...)
	for i := range out.Profiles {
		out.Profiles[i].Env = redactedEnv(spec.Profiles[i].Env)
	}
	return out
}

func defaultEndpoint(backend string) string {
	switch backend {
	case "openai-chat":
		return "/v1/chat/completions"
	case "openai":
		return "/v1/completions"
	default:
		return ""
	}
}

func ValidateSpec(spec Spec) error {
	issues := validateSpecBasics(spec)
	engineIssues, engineNames := validateEngines(spec.Engines)
	profileIssues, profileNames := validateProfiles(spec, engineNames)
	issues = append(issues, engineIssues...)
	issues = append(issues, profileIssues...)
	issues = append(issues, validateEndpointBaseURLProfileUsage(spec)...)
	issues = append(issues, validateWarmup(spec.Warmup)...)
	issues = append(issues, validateBackendAttestationSetup(spec)...)
	issues = append(issues, validateWorkloads(spec.Workloads, profileNames, spec.Profiles)...)
	issues = append(issues, validateGeneratorStamp(spec.Generator)...)
	if len(issues) > 0 {
		return errors.New(strings.Join(issues, "\n"))
	}
	return nil
}

func validateBackendAttestationSetup(spec Spec) []string {
	var issues []string
	for _, profile := range spec.Profiles {
		if !requiresBackendAttestation(profile) {
			continue
		}
		if !profile.Managed {
			issues = append(issues, fmt.Sprintf("profile %q requests a concrete attention or MoE backend; profiler attestation requires a managed server", profile.Name))
			continue
		}
		if !spec.Warmup.Enabled {
			issues = append(issues, fmt.Sprintf("profile %q requests a concrete attention or MoE backend; warmup must be enabled for request-scoped profiler attestation", profile.Name))
		}
	}
	return issues
}

// validateGeneratorStamp requires every declared ladder trim to carry its
// reason: an unexplained trim is a silent hole waiting to render.
func validateGeneratorStamp(stamp *artifact.GeneratorStamp) []string {
	if stamp == nil {
		return nil
	}
	var issues []string
	for index, trim := range stamp.LadderTrims {
		issues = append(issues, validateLadderTrim(index, trim)...)
	}
	return issues
}

func validateLadderTrim(index int, trim artifact.LadderTrim) []string {
	var issues []string
	if trim.Context <= 0 {
		issues = append(issues, fmt.Sprintf("generator.ladder_trims[%d]: context must be positive", index))
	}
	if trim.MaxConcurrency <= 0 {
		issues = append(issues, fmt.Sprintf("generator.ladder_trims[%d]: max_concurrency must be positive", index))
	}
	if strings.TrimSpace(trim.Reason) == "" {
		issues = append(issues, fmt.Sprintf("generator.ladder_trims[%d]: reason is required", index))
	}
	return issues
}

func validateSpecBasics(spec Spec) []string {
	var issues []string
	if spec.Version != "1" {
		issues = append(issues, `version must be "1"`)
	}
	if strings.TrimSpace(spec.Name) == "" {
		issues = append(issues, "name is required")
	}
	if strings.TrimSpace(spec.Model) == "" {
		issues = append(issues, "model is required")
	}
	if spec.Safety.MinMemAvailableGiB <= 0 {
		issues = append(issues, "safety.min_mem_available_gib must be positive")
	}
	return issues
}

func validateEngines(engines []EngineConfig) ([]string, map[string]bool) {
	names := map[string]bool{}
	var issues []string
	if len(engines) == 0 {
		issues = append(issues, "at least one engine is required")
	}
	for i, engine := range engines {
		issues = append(issues, validateEngine(fmt.Sprintf("engines[%d]", i), engine, names)...)
	}
	return issues, names
}

func validateEngine(prefix string, engine EngineConfig, names map[string]bool) []string {
	var issues []string
	name := strings.TrimSpace(engine.Name)
	if name == "" {
		issues = append(issues, prefix+": name is required")
	}
	if names[engine.Name] {
		issues = append(issues, prefix+": duplicate engine name "+engine.Name)
	}
	names[engine.Name] = true
	if strings.TrimSpace(engine.Type) == "" {
		issues = append(issues, prefix+": type is required")
	}
	return issues
}

func validateProfiles(spec Spec, engineNames map[string]bool) ([]string, map[string]bool) {
	names := map[string]bool{}
	slugs := map[string]string{}
	var issues []string
	if len(spec.Profiles) == 0 {
		issues = append(issues, "at least one profile is required")
	}
	for i, profile := range spec.Profiles {
		prefix := fmt.Sprintf("profiles[%d]", i)
		issues = append(issues, validateProfile(prefix, profile, spec.Runner, engineNames, names, slugs)...)
	}
	return issues, names
}

func validateProfile(prefix string, profile Profile, runner RunnerConfig, engineNames, names map[string]bool, slugs map[string]string) []string {
	var issues []string
	issues = append(issues, validateRequiredUniqueName(prefix, "profile", profile.Name, names)...)
	issues = append(issues, validateSlug(prefix, "profile name", profile.Name, nil, slugs)...)
	issues = append(issues, validateProfileFields(prefix, profile, engineNames)...)
	issues = append(issues, validateManagedProfile(prefix, profile, runner)...)
	return append(issues, validateSleepLevel(prefix, profile)...)
}

func validateRequiredUniqueName(prefix, label, name string, names map[string]bool) []string {
	var issues []string
	if strings.TrimSpace(name) == "" {
		issues = append(issues, prefix+": name is required")
	}
	if names[name] {
		issues = append(issues, prefix+": duplicate "+label+" name "+name)
	}
	names[name] = true
	return issues
}

func validateSlug(prefix, label, name string, reserved map[string]string, slugs map[string]string) []string {
	if strings.TrimSpace(name) == "" {
		return nil
	}
	slug := Slug(name)
	if slug == "" {
		return []string{prefix + ": name must contain at least one ASCII letter or digit"}
	}
	if message := reserved[slug]; message != "" {
		return []string{prefix + ": " + message}
	}
	if previous, ok := slugs[slug]; ok {
		return []string{prefix + ": " + label + " " + name + " collides with " + previous + " after slug normalization"}
	}
	slugs[slug] = name
	return nil
}

func validateProfileFields(prefix string, profile Profile, engineNames map[string]bool) []string {
	var issues []string
	issues = append(issues, validateProfileEndpointBaseURL(prefix, profile.EndpointBaseURL)...)
	if profile.Port <= 0 && profileRequiresPort(profile) {
		issues = append(issues, prefix+": port must be positive")
	}
	if strings.TrimSpace(profile.Model) == "" {
		issues = append(issues, prefix+": model is required")
	}
	if strings.TrimSpace(profile.Engine) == "" {
		return append(issues, prefix+": engine is required")
	}
	if !engineNames[profile.Engine] {
		issues = append(issues, prefix+": unknown engine "+profile.Engine)
	}
	return issues
}

func profileRequiresPort(profile Profile) bool {
	return profile.Managed || !validEndpointBaseURL(profile.EndpointBaseURL)
}

func validateProfileEndpointBaseURL(prefix, raw string) []string {
	if strings.TrimSpace(raw) == "" || validEndpointBaseURL(raw) {
		return nil
	}
	return []string{prefix + ": endpoint_base_url must be an http(s) URL with a host and no query or fragment"}
}

func validEndpointBaseURL(raw string) bool {
	parsed, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return false
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return false
	}
	return parsed.Host != "" && parsed.RawQuery == "" && parsed.Fragment == ""
}

func validateEndpointBaseURLProfileUsage(spec Spec) []string {
	return endpointBaseURLProfileIssueMessages(invalidEndpointBaseURLProfiles(spec))
}

func invalidEndpointBaseURLProfiles(spec Spec) map[string]bool {
	endpointProfiles := endpointBaseURLProfiles(spec.Profiles)
	if len(endpointProfiles) == 0 {
		return nil
	}
	invalid := map[string]bool{}
	addWarmupEndpointOnlyProfiles(invalid, endpointProfiles, spec.Warmup.Enabled)
	addWorkloadEndpointOnlyProfiles(invalid, endpointProfiles, spec.Workloads)
	return invalid
}

func addWarmupEndpointOnlyProfiles(invalid, endpointOnly map[string]bool, warmupEnabled bool) {
	if !warmupEnabled {
		return
	}
	for name := range endpointOnly {
		invalid[name] = true
	}
}

func addWorkloadEndpointOnlyProfiles(invalid, endpointOnly map[string]bool, workloads []Workload) {
	for _, workload := range workloads {
		if normalizeLoadGenerator(workload.LoadGenerator) == LoadGeneratorHTTP {
			continue
		}
		for name := range endpointOnly {
			if workloadReferencesProfile(workload, name) {
				invalid[name] = true
			}
		}
	}
}

func endpointBaseURLProfileIssueMessages(invalid map[string]bool) []string {
	var issues []string
	for _, name := range collections.SortedKeys(invalid) {
		issues = append(issues, "profile "+name+": endpoint_base_url can only be used when warmup is disabled and all referenced workloads use localperf_http")
	}
	return issues
}

func endpointBaseURLProfiles(profiles []Profile) map[string]bool {
	out := map[string]bool{}
	for _, profile := range profiles {
		if validEndpointBaseURL(profile.EndpointBaseURL) {
			out[profile.Name] = true
		}
	}
	return out
}

func workloadReferencesProfile(workload Workload, profileName string) bool {
	if len(workload.Profiles) == 0 {
		return true
	}
	for _, name := range workload.Profiles {
		if name == profileName {
			return true
		}
	}
	return false
}

func validateManagedProfile(prefix string, profile Profile, runner RunnerConfig) []string {
	if !profile.Managed {
		return nil
	}
	var issues []string
	issues = append(issues, validateManagedProfileSizes(prefix, profile)...)
	if runner.PrebootProfiles && oneAwakeProfile(runner) && !profile.EnableSleepMode {
		issues = append(issues, prefix+": enable_sleep_mode is required when runner.preboot_profiles and runner.one_awake_profile are true")
	}
	return issues
}

func validateManagedProfileSizes(prefix string, profile Profile) []string {
	var issues []string
	if profile.MaxModelLen <= 0 {
		issues = append(issues, prefix+": max_model_len must be positive for managed profiles")
	}
	if profile.MaxNumSeqs <= 0 {
		issues = append(issues, prefix+": max_num_seqs must be positive for managed profiles")
	}
	return issues
}

func oneAwakeProfile(runner RunnerConfig) bool {
	return runner.OneAwakeProfile == nil || *runner.OneAwakeProfile
}

func validateSleepLevel(prefix string, profile Profile) []string {
	if profile.SleepLevel == nil || (*profile.SleepLevel >= 0 && *profile.SleepLevel <= 2) {
		return nil
	}
	return []string{prefix + ": sleep_level must be 0, 1, or 2"}
}

func validateWarmup(warmup WarmupConfig) []string {
	if !warmup.Enabled {
		return nil
	}
	var issues []string
	if warmup.NumPrompts <= 0 {
		issues = append(issues, "warmup: num_prompts must be positive")
	}
	if warmup.MaxConcurrency <= 0 {
		issues = append(issues, "warmup: max_concurrency must be positive")
	}
	if strings.TrimSpace(warmup.DatasetName) == "" {
		issues = append(issues, "warmup: dataset_name is required")
	}
	return append(issues, validateTrafficConfig("warmup", warmup.BenchmarkTrafficConfig)...)
}

func validateWorkloads(workloads []Workload, profileNames map[string]bool, profiles []Profile) []string {
	var issues []string
	names := map[string]bool{}
	slugs := map[string]string{}
	if len(workloads) == 0 {
		issues = append(issues, "at least one workload is required")
	}
	for i, workload := range workloads {
		prefix := fmt.Sprintf("workloads[%d]", i)
		issues = append(issues, validateWorkload(prefix, workload, profileNames, names, slugs)...)
		issues = append(issues, validateWorkloadContextSemantics(prefix, workload, profiles)...)
		issues = append(issues, validateWorkloadSLO(prefix, workload)...)
	}
	return issues
}

func validateWorkload(prefix string, workload Workload, profileNames, names map[string]bool, slugs map[string]string) []string {
	var issues []string
	reserved := map[string]string{"warmup": "workload name warmup is reserved for warmup artifacts"}
	issues = append(issues, validateRequiredUniqueName(prefix, "workload", workload.Name, names)...)
	issues = append(issues, validateSlug(prefix, "workload name", workload.Name, reserved, slugs)...)
	issues = append(issues, validateWorkloadFields(prefix, workload)...)
	issues = append(issues, validateConcurrencyValues(prefix, workload.MaxConcurrency)...)
	issues = append(issues, validateProfileRefs(prefix, workload.Profiles, profileNames)...)
	return append(issues, validateTrafficConfig(prefix, workload.BenchmarkTrafficConfig)...)
}

// validateWorkloadContextSemantics enforces the contract in
// docs/2026-07-02-context-semantics.md: every workload declares whether its
// context number means active context or server capacity, and an active
// claim must be backed by the requested token shape.
func validateWorkloadContextSemantics(prefix string, workload Workload, profiles []Profile) []string {
	if workload.ContextTarget <= 0 || strings.TrimSpace(workload.ContextSemantics) == "" {
		return []string{prefix + ": context_target and context_semantics are required on every workload; see docs/2026-07-02-context-semantics.md"}
	}
	switch workload.ContextSemantics {
	case ContextSemanticsActive:
		return validateActiveContextClaim(prefix, workload, profiles)
	case ContextSemanticsCapacity:
		return validateCapacityContextClaim(prefix, workload, profiles)
	default:
		return []string{prefix + `: context_semantics must be "active" or "capacity"`}
	}
}

func validateWorkloadSLO(prefix string, workload Workload) []string {
	if workload.SLO == nil {
		return nil
	}
	var issues []string
	if workload.SLO.TTFTP95Millis < 0 {
		issues = append(issues, prefix+": slo.ttft_p95_ms must not be negative")
	}
	if workload.SLO.E2ELP95Millis < 0 {
		issues = append(issues, prefix+": slo.e2el_p95_ms must not be negative")
	}
	if workload.SLO.TTFTP95Millis == 0 && workload.SLO.E2ELP95Millis == 0 {
		issues = append(issues, prefix+": slo must set at least one target")
	}
	if workload.SaveDetailed != nil && !*workload.SaveDetailed {
		issues = append(issues, prefix+": slo requires save_detailed (goodput is derived from per-request rows)")
	}
	return issues
}

func validateActiveContextClaim(prefix string, workload Workload, profiles []Profile) []string {
	if workload.DatasetName != "random" {
		return []string{prefix + `: context_semantics "active" requires the random dataset so requested token counts are exact`}
	}
	if !zeroRangeRatio(workload.RandomRangeRatio) {
		return []string{prefix + `: context_semantics "active" requires random_range_ratio 0 so every request stays inside the claimed band`}
	}
	var issues []string
	requested := workload.RandomInputLen + workload.RandomOutputLen
	target := workload.ContextTarget
	if float64(requested) < ContextTargetMinFrac*float64(target) || requested > target {
		issues = append(issues, fmt.Sprintf(
			"%s: claims active context %d but requests %d+%d=%d tokens (%.0f%% of target); this measures a ~%s active workload on a %s-capacity server. Either set context_target to %d, adjust random_input_len, or declare context_semantics: %q",
			prefix, target, workload.RandomInputLen, workload.RandomOutputLen, requested,
			100*float64(requested)/float64(target), TokenCountLabel(requested), TokenCountLabel(target), requested, ContextSemanticsCapacity))
	}
	for _, profile := range pairedProfiles(workload, profiles) {
		// A profile without a declared max_model_len (endpoint-only,
		// unmanaged) has no locally checkable limit; skip the comparison
		// rather than guess.
		if profile.MaxModelLen > 0 && profile.MaxModelLen < target {
			issues = append(issues, fmt.Sprintf("%s: profile %s max_model_len %d is below context_target %d", prefix, profile.Name, profile.MaxModelLen, target))
		}
	}
	return issues
}

func validateCapacityContextClaim(prefix string, workload Workload, profiles []Profile) []string {
	var issues []string
	for _, profile := range pairedProfiles(workload, profiles) {
		if profile.MaxModelLen > 0 && profile.MaxModelLen != workload.ContextTarget {
			issues = append(issues, fmt.Sprintf(
				"%s: context_semantics %q requires context_target to equal profile %s max_model_len %d, got %d",
				prefix, ContextSemanticsCapacity, profile.Name, profile.MaxModelLen, workload.ContextTarget))
		}
	}
	return issues
}

// zeroRangeRatio reports whether random_range_ratio is unset or exactly
// zero; any other value makes requested token counts a range, not a number.
func zeroRangeRatio(value string) bool {
	value = strings.TrimSpace(value)
	if value == "" {
		return true
	}
	parsed, err := strconv.ParseFloat(value, 64)
	return err == nil && parsed == 0
}

// pairedProfiles mirrors plannedProfileNames: an empty profile list pairs
// the workload with every profile in the spec.
func pairedProfiles(workload Workload, profiles []Profile) []Profile {
	if len(workload.Profiles) == 0 {
		return profiles
	}
	wanted := map[string]bool{}
	for _, name := range workload.Profiles {
		wanted[name] = true
	}
	var paired []Profile
	for _, profile := range profiles {
		if wanted[profile.Name] {
			paired = append(paired, profile)
		}
	}
	return paired
}

// TokenCountLabel renders a token count as a compact label such as "5k" or
// "32k"; small values render as plain numbers.
func TokenCountLabel(value int) string {
	if value >= 1024 {
		return fmt.Sprintf("%.0fk", float64(value)/1024)
	}
	return fmt.Sprint(value)
}

func validateWorkloadFields(prefix string, workload Workload) []string {
	var issues []string
	issues = append(issues, validateWorkloadRole(prefix, workload)...)
	issues = append(issues, validateWorkloadDatasetName(prefix, workload)...)
	issues = append(issues, validateWorkloadPositiveFields(prefix, workload)...)
	issues = append(issues, validateWorkloadPhase(prefix, workload)...)
	issues = append(issues, validateLoadGenerator(prefix, workload.LoadGenerator)...)
	issues = append(issues, validateLoadGeneratorDataset(prefix, workload)...)
	return append(issues, validateStructuredDataset(prefix, workload)...)
}

func validateWorkloadRole(prefix string, workload Workload) []string {
	switch workload.Role {
	case WorkloadRoleBenchmark, WorkloadRoleDiagnostic:
		return nil
	default:
		return []string{prefix + `: role must be "benchmark" or "diagnostic"`}
	}
}

func validateWorkloadDatasetName(prefix string, workload Workload) []string {
	if strings.TrimSpace(workload.DatasetName) == "" && !hasStructuredDataset(workload) {
		return []string{prefix + ": dataset_name is required"}
	}
	return nil
}

func validateWorkloadPositiveFields(prefix string, workload Workload) []string {
	var issues []string
	issues = append(issues, validateWorkloadCountMode(prefix, workload)...)
	if workload.PromptsPerUser < 0 {
		issues = append(issues, prefix+": prompts_per_user must not be negative")
	}
	if workload.Repeats <= 0 {
		issues = append(issues, prefix+": repeats must be positive")
	}
	if len(workload.MaxConcurrency) == 0 {
		issues = append(issues, prefix+": max_concurrency must not be empty")
	}
	issues = append(issues, validateWorkloadBatches(prefix, workload.Batches)...)
	return issues
}

func validateWorkloadCountMode(prefix string, workload Workload) []string {
	countModes := 0
	for _, active := range []bool{workload.NumPrompts > 0, workload.PromptsPerUser > 0, len(workload.Batches) > 0} {
		if active {
			countModes++
		}
	}
	if countModes == 0 {
		return []string{prefix + ": num_prompts or prompts_per_user must be positive, or batches must be set"}
	}
	if countModes > 1 {
		return []string{prefix + ": set exactly one of num_prompts, prompts_per_user, or batches, not both"}
	}
	return nil
}

func validateWorkloadBatches(prefix string, batches []Batch) []string {
	var issues []string
	seen := map[int]bool{}
	for index, batch := range batches {
		if batch.Concurrency <= 0 || batch.Requests <= 0 {
			issues = append(issues, fmt.Sprintf("%s: batches[%d] concurrency and requests must be positive", prefix, index))
		}
		if seen[batch.Concurrency] {
			issues = append(issues, fmt.Sprintf("%s: duplicate batch concurrency %d", prefix, batch.Concurrency))
		}
		seen[batch.Concurrency] = true
	}
	return issues
}

func validateWorkloadPhase(prefix string, workload Workload) []string {
	if strings.TrimSpace(workload.Phase) != "" && Slug(workload.Phase) == "" {
		return []string{prefix + ": phase must contain at least one ASCII letter or digit"}
	}
	return nil
}

func validateLoadGenerator(prefix, generator string) []string {
	switch normalizeLoadGenerator(generator) {
	case "", LoadGeneratorVLLMBench, LoadGeneratorHTTP:
		return nil
	default:
		return []string{prefix + ": unsupported load_generator " + generator}
	}
}

func validateLoadGeneratorDataset(prefix string, workload Workload) []string {
	if normalizeLoadGenerator(workload.LoadGenerator) != LoadGeneratorHTTP {
		return nil
	}
	if workload.DatasetName == "random" || hasHTTPDatasetPath(workload) || hasStructuredDataset(workload) {
		return nil
	}
	return []string{prefix + ": localperf_http supports random or canonical structured datasets, not dataset_name " + workload.DatasetName}
}

func validateStructuredDataset(prefix string, workload Workload) []string {
	if !hasStructuredDataset(workload) {
		return nil
	}
	var issues []string
	issues = append(issues, validateStructuredDatasetType(prefix, workload)...)
	issues = append(issues, validateStructuredDatasetCounts(prefix, workload)...)
	issues = append(issues, validateStructuredDatasetSelection(prefix, workload)...)
	issues = append(issues, validateStructuredDatasetRequest(prefix, workload)...)
	issues = append(issues, validateStructuredDatasetLocation(prefix, workload)...)
	return issues
}

func validateStructuredDatasetType(prefix string, workload Workload) []string {
	if _, ok := datasetAdapter(workload.Dataset.Type); !ok {
		return []string{prefix + ": unsupported dataset.type " + workload.Dataset.Type}
	}
	return nil
}

func validateStructuredDatasetCounts(prefix string, workload Workload) []string {
	var issues []string
	if workload.Dataset.SampleCount <= 0 {
		issues = append(issues, prefix+": dataset.sample_count must be positive")
	}
	if workload.Dataset.Seed != nil && *workload.Dataset.Seed < 0 {
		issues = append(issues, prefix+": dataset.seed must be non-negative")
	}
	return append(issues, validateStructuredDatasetTokenCounts(prefix, workload)...)
}

func validateStructuredDatasetSelection(prefix string, workload Workload) []string {
	switch strings.TrimSpace(workload.Dataset.Selection) {
	case "first_n", "random", "shuffle":
		return nil
	default:
		return []string{prefix + ": unsupported dataset.selection " + workload.Dataset.Selection}
	}
}

func validateStructuredDatasetTokenCounts(prefix string, workload Workload) []string {
	var issues []string
	if workload.Dataset.InputTokens < 0 {
		issues = append(issues, prefix+": dataset.input_tokens must not be negative")
	}
	if workload.Dataset.OutputTokens < 0 {
		issues = append(issues, prefix+": dataset.output_tokens must not be negative")
	}
	return issues
}

func validateStructuredDatasetRequest(prefix string, workload Workload) []string {
	var issues []string
	if structuredDatasetNeedsDefaultOutput(workload.Dataset.Type) && workload.Request.MaxOutputTokens <= 0 && workload.Dataset.OutputTokens <= 0 {
		issues = append(issues, prefix+": request.max_output_tokens or dataset.output_tokens must be positive")
	}
	if strings.TrimSpace(workload.Request.Mode) == "" {
		issues = append(issues, prefix+": request.mode is required")
	}
	if workload.Dataset.Type == "sharegpt" && strings.TrimSpace(workload.Request.TurnPolicy) != "first_user_turn" {
		issues = append(issues, prefix+": unsupported request.turn_policy "+workload.Request.TurnPolicy)
	}
	return issues
}

func structuredDatasetNeedsDefaultOutput(datasetType string) bool {
	switch normalizeDatasetType(datasetType) {
	case "custom-jsonl", "raw-payload":
		return false
	default:
		return true
	}
}

func validateStructuredDatasetLocation(prefix string, workload Workload) []string {
	if workload.Dataset.Type != "synthetic" && strings.TrimSpace(datasetLocalPath(workload.Dataset)) == "" {
		return []string{prefix + ": dataset.path or file:// dataset.uri is required"}
	}
	return nil
}

func validateConcurrencyValues(prefix string, values []int) []string {
	var issues []string
	seen := map[int]bool{}
	for _, value := range values {
		if value <= 0 {
			issues = append(issues, prefix+": max_concurrency values must be positive")
		}
		if seen[value] {
			issues = append(issues, prefix+": duplicate max_concurrency value "+fmt.Sprint(value))
		}
		seen[value] = true
	}
	return issues
}

func validateProfileRefs(prefix string, refs []string, profileNames map[string]bool) []string {
	var issues []string
	seen := map[string]bool{}
	for _, name := range refs {
		if seen[name] {
			issues = append(issues, prefix+": duplicate profile reference "+name)
		}
		seen[name] = true
		if !profileNames[name] {
			issues = append(issues, prefix+": unknown profile "+name)
		}
	}
	return issues
}

func validateTrafficConfig(prefix string, traffic BenchmarkTrafficConfig) []string {
	var issues []string
	issues = append(issues, validateRandomTraffic(prefix, traffic)...)
	issues = append(issues, validateNegativeTrafficFields(prefix, traffic)...)
	issues = append(issues, validateTrafficPointerFields(prefix, traffic)...)
	issues = append(issues, validateNonEmptyValues(prefix, "metadata", traffic.Metadata)...)
	issues = append(issues, validateNonEmptyValues(prefix, "goodput", traffic.Goodput)...)
	return issues
}

func validateRandomTraffic(prefix string, traffic BenchmarkTrafficConfig) []string {
	var issues []string
	if traffic.DatasetName == "random" {
		if traffic.RandomInputLen <= 0 {
			issues = append(issues, prefix+": random_input_len must be positive for random dataset")
		}
		if traffic.RandomOutputLen <= 0 {
			issues = append(issues, prefix+": random_output_len must be positive for random dataset")
		}
	}
	if traffic.Seed != nil && *traffic.Seed < 0 {
		issues = append(issues, prefix+": seed must be non-negative")
	}
	return issues
}

func validateNegativeTrafficFields(prefix string, traffic BenchmarkTrafficConfig) []string {
	var issues []string
	fields := map[string]int{
		"random_prefix_len":              traffic.RandomPrefixLen,
		"sonnet_input_len":               traffic.SonnetInputLen,
		"sonnet_output_len":              traffic.SonnetOutputLen,
		"sonnet_prefix_len":              traffic.SonnetPrefixLen,
		"prefix_repetition_prefix_len":   traffic.PrefixRepetitionPrefixLen,
		"prefix_repetition_suffix_len":   traffic.PrefixRepetitionSuffixLen,
		"prefix_repetition_num_prefixes": traffic.PrefixRepetitionNumPrefixes,
		"prefix_repetition_output_len":   traffic.PrefixRepetitionOutputLen,
		"speed_bench_output_len":         traffic.SpeedBenchOutputLen,
	}
	for field, value := range fields {
		if value < 0 {
			issues = append(issues, prefix+": "+field+" must not be negative")
		}
	}
	return issues
}

func validateTrafficPointerFields(prefix string, traffic BenchmarkTrafficConfig) []string {
	var issues []string
	if traffic.CustomOutputLen != nil && *traffic.CustomOutputLen < -1 {
		issues = append(issues, prefix+": custom_output_len must be -1 or greater")
	}
	if traffic.ShareGPTOutputLen != nil && *traffic.ShareGPTOutputLen <= 0 {
		issues = append(issues, prefix+": sharegpt_output_len must be positive")
	}
	return issues
}

func validateNonEmptyValues(prefix, field string, values []string) []string {
	var issues []string
	for _, value := range values {
		if strings.TrimSpace(value) == "" {
			issues = append(issues, prefix+": "+field+" values must not be empty")
		}
	}
	return issues
}

func BuildPlan(spec Spec, runDir string) []PlannedRun {
	profiles := map[string]Profile{}
	for _, profile := range spec.Profiles {
		profiles[profile.Name] = profile
	}
	var runs []PlannedRun
	for _, workload := range spec.Workloads {
		for _, profileName := range plannedProfileNames(workload, profiles) {
			profile := profiles[profileName]
			for _, concurrency := range workload.MaxConcurrency {
				for repeat := 0; repeat < plannedRepeats(workload); repeat++ {
					runs = append(runs, buildPlannedRun(runDir, profile, workload, concurrency, repeat))
				}
			}
		}
	}
	return runs
}

func plannedProfileNames(workload Workload, profiles map[string]Profile) []string {
	if len(workload.Profiles) > 0 {
		return workload.Profiles
	}
	return collections.SortedKeys(profiles)
}

func plannedRepeats(workload Workload) int {
	if workload.Repeats > 0 {
		return workload.Repeats
	}
	return 1
}

func buildPlannedRun(runDir string, profile Profile, workload Workload, concurrency, repeat int) PlannedRun {
	repeats := plannedRepeats(workload)
	workload.NumPrompts = resolvedNumPrompts(workload, concurrency)
	return PlannedRun{
		Profile:     profile,
		Workload:    workload,
		Concurrency: concurrency,
		Repeat:      repeat,
		ResultFile:  ResultPath(runDir, profile.Name, workload.Name, concurrency, repeat, repeats),
	}
}

// resolvedNumPrompts scales the request count with concurrency when the
// workload declares prompts_per_user.
func resolvedNumPrompts(workload Workload, concurrency int) int {
	for _, batch := range workload.Batches {
		if batch.Concurrency == concurrency {
			return batch.Requests
		}
	}
	if workload.PromptsPerUser <= 0 {
		return workload.NumPrompts
	}
	return workload.PromptsPerUser * concurrency
}

func RunDir(base string, spec Spec, now time.Time) string {
	if strings.TrimSpace(base) != "" {
		return base
	}
	parent := strings.TrimSpace(spec.OutputDir)
	if parent == "" {
		parent = "runs"
	}
	name := Slug(spec.Name)
	if name == "" {
		name = "vllm-bench"
	}
	if spec.Runner.AppendTimestampToRun == nil || *spec.Runner.AppendTimestampToRun {
		name += "-" + now.UTC().Format("20060102T150405Z")
	}
	return filepath.Join(parent, name)
}

func ResultPath(runDir, profileName, workloadName string, concurrency int, repeatInfo ...int) string {
	repeat := 0
	repeats := 1
	if len(repeatInfo) > 0 {
		repeat = repeatInfo[0]
	}
	if len(repeatInfo) > 1 {
		repeats = repeatInfo[1]
	}
	name := fmt.Sprintf("%s__%s__c%d", Slug(profileName), Slug(workloadName), concurrency)
	if repeats > 1 {
		name += fmt.Sprintf("__r%d", repeat+1)
	}
	name += ".json"
	return filepath.Join(runDir, "results", name)
}

func SleepLevelValue(profile Profile) int {
	if profile.SleepLevel == nil {
		return 2
	}
	return *profile.SleepLevel
}

func intPointer(value int) *int {
	return &value
}

func boolPointer(value bool) *bool {
	return &value
}

func boolValue(value *bool) bool {
	return value != nil && *value
}

func Slug(text string) string {
	return bench.Slug(text)
}
