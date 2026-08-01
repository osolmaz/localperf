package benchmarkconfig

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/osolmaz/localperf/internal/artifact"
	"github.com/osolmaz/localperf/internal/vllmbench"
)

const Version = "1"

type Suite struct {
	Version     string `json:"version"`
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
	Warmup      Warmup `json:"warmup"`
	Cases       []Case `json:"cases"`
}

type Warmup struct {
	Enabled      bool `json:"enabled"`
	InputTokens  int  `json:"input_tokens,omitempty"`
	OutputTokens int  `json:"output_tokens,omitempty"`
	Requests     int  `json:"requests,omitempty"`
	Concurrency  int  `json:"concurrency,omitempty"`
}

type Case struct {
	Name             string            `json:"name"`
	Role             string            `json:"role"`
	Phase            string            `json:"phase"`
	InputTokens      int               `json:"input_tokens"`
	OutputTokens     int               `json:"output_tokens"`
	ContextTarget    int               `json:"context_target"`
	ContextSemantics string            `json:"context_semantics"`
	Batches          []vllmbench.Batch `json:"batches"`
	Repeats          int               `json:"repeats"`
	Temperature      float64           `json:"temperature"`
	IgnoreEOS        bool              `json:"ignore_eos"`
}

type Deployment struct {
	Version       string  `json:"version"`
	Name          string  `json:"name"`
	Model         string  `json:"model"`
	ModelRevision string  `json:"model_revision,omitempty"`
	Runtime       Runtime `json:"runtime"`
	Server        Server  `json:"server"`
	Client        Client  `json:"client"`
	Safety        Safety  `json:"safety"`
}

type Runtime struct {
	Name            string            `json:"name"`
	Type            string            `json:"type"`
	Owner           string            `json:"owner,omitempty"`
	Source          string            `json:"source,omitempty"`
	Version         string            `json:"version,omitempty"`
	Digest          string            `json:"digest,omitempty"`
	Command         string            `json:"command,omitempty"`
	BenchCommand    string            `json:"bench_command,omitempty"`
	Managed         bool              `json:"managed"`
	Host            string            `json:"host,omitempty"`
	Port            int               `json:"port,omitempty"`
	EndpointBaseURL string            `json:"endpoint_base_url,omitempty"`
	HealthPath      string            `json:"health_path,omitempty"`
	Env             map[string]string `json:"env,omitempty"`
	Args            []string          `json:"args,omitempty"`
}

type Server struct {
	MaxNumBatchedTokens  int      `json:"max_num_batched_tokens,omitempty"`
	GPUMemoryUtilization float64  `json:"gpu_memory_utilization,omitempty"`
	KVCacheDType         string   `json:"kv_cache_dtype,omitempty"`
	AttentionBackend     string   `json:"attention_backend,omitempty"`
	MoEBackend           string   `json:"moe_backend,omitempty"`
	EnablePrefixCaching  *bool    `json:"enable_prefix_caching,omitempty"`
	EnableSleepMode      bool     `json:"enable_sleep_mode,omitempty"`
	SleepLevel           *int     `json:"sleep_level,omitempty"`
	SpeculativeDecoding  []string `json:"speculative_decoding,omitempty"`
}

type Client struct {
	LoadGenerator string   `json:"load_generator,omitempty"`
	Backend       string   `json:"backend,omitempty"`
	Endpoint      string   `json:"endpoint,omitempty"`
	Tokenizer     string   `json:"tokenizer,omitempty"`
	ExtraArgs     []string `json:"extra_args,omitempty"`
}

type Safety struct {
	MinMemAvailableGiB float64 `json:"min_mem_available_gib"`
	PollIntervalMillis int     `json:"poll_interval_millis,omitempty"`
	StartupTimeoutSec  int     `json:"startup_timeout_sec,omitempty"`
	WorkloadTimeoutSec int     `json:"workload_timeout_sec,omitempty"`
	HTTPTimeoutSec     int     `json:"http_timeout_sec,omitempty"`
}

type Selection struct {
	Cases         []string
	Concurrencies []int
}

type Compiled struct {
	Suite      Suite
	Deployment Deployment
	Spec       vllmbench.Spec
}

func LoadSuite(nameOrPath string) (Suite, error) {
	if suite, ok := builtinSuite(strings.TrimSpace(nameOrPath)); ok {
		return suite, nil
	}
	var suite Suite
	if strings.TrimSpace(nameOrPath) == "" {
		return suite, errors.New("missing --suite")
	}
	if err := loadStrictJSON(nameOrPath, &suite); err != nil {
		return suite, fmt.Errorf("load suite: %w", err)
	}
	return suite, validateSuite(suite)
}

func LoadDeployment(path string) (Deployment, error) {
	var deployment Deployment
	if strings.TrimSpace(path) == "" {
		return deployment, errors.New("missing --deployment")
	}
	if err := loadStrictJSON(path, &deployment); err != nil {
		return deployment, fmt.Errorf("load deployment: %w", err)
	}
	return deployment, validateDeployment(deployment)
}

func Compile(suite Suite, deployment Deployment, selection Selection) (Compiled, error) {
	if err := validateSuite(suite); err != nil {
		return Compiled{}, err
	}
	if err := validateDeployment(deployment); err != nil {
		return Compiled{}, err
	}
	cases, err := selectCases(suite.Cases, selection)
	if err != nil {
		return Compiled{}, err
	}
	maxContext, maxConcurrency := suiteLimits(cases)
	if err := rejectOwnedRuntimeArgs(deployment.Runtime.Args); err != nil {
		return Compiled{}, err
	}
	profileName := deployment.Name
	engine := vllmbench.EngineConfig{
		Name: deployment.Runtime.Name, Type: deployment.Runtime.Type,
		Command: deployment.Runtime.Command, BenchCommand: deployment.Runtime.BenchCommand,
		Env: deployment.Runtime.Env,
		Metadata: map[string]any{
			"deployment": deployment.Name, "model_revision": deployment.ModelRevision,
			"runtime_owner": deployment.Runtime.Owner, "runtime_source": deployment.Runtime.Source,
			"runtime_version_requested": deployment.Runtime.Version, "runtime_digest": deployment.Runtime.Digest,
			"requested_attention_backend": deployment.Server.AttentionBackend,
			"requested_moe_backend":       deployment.Server.MoEBackend,
			"requested_kv_cache_dtype":    deployment.Server.KVCacheDType,
			"speculative_decoding":        deployment.Server.SpeculativeDecoding,
		},
	}
	managed := deployment.Runtime.Managed
	engine.Managed = &managed
	profile := vllmbench.Profile{
		Name: profileName, Engine: engine.Name, Model: deployment.Model,
		Host: defaultString(deployment.Runtime.Host, "127.0.0.1"), Port: deployment.Runtime.Port,
		EndpointBaseURL: deployment.Runtime.EndpointBaseURL, Managed: managed,
		HealthPath:  defaultString(deployment.Runtime.HealthPath, vllmbench.DefaultHealthPath),
		MaxModelLen: maxContext, MaxNumSeqs: maxConcurrency,
		MaxNumBatchedTokens:  deployment.Server.MaxNumBatchedTokens,
		GPUMemoryUtilization: deployment.Server.GPUMemoryUtilization,
		KVCacheDType:         deployment.Server.KVCacheDType,
		AttentionBackend:     deployment.Server.AttentionBackend, MoEBackend: deployment.Server.MoEBackend,
		EnablePrefixCaching: deployment.Server.EnablePrefixCaching,
		EnableSleepMode:     deployment.Server.EnableSleepMode, SleepLevel: deployment.Server.SleepLevel,
		Args: append(appendRevision(deployment.Runtime.Args, deployment.ModelRevision), deployment.Server.SpeculativeDecoding...),
	}
	adaptive := false
	appendTimestamp := true
	stopManaged := true
	spec := vllmbench.Spec{
		Version:     Version,
		Name:        deployment.Name + "-" + suite.Name,
		Description: suite.Description,
		Model:       deployment.Model,
		Engines:     []vllmbench.EngineConfig{engine},
		Runner:      vllmbench.RunnerConfig{StopManagedOnExit: &stopManaged, AppendTimestampToRun: &appendTimestamp, Adaptive: vllmbench.AdaptiveConfig{Enabled: &adaptive}},
		Safety: vllmbench.SafetyConfig{
			MinMemAvailableGiB: deployment.Safety.MinMemAvailableGiB,
			PollIntervalMillis: deployment.Safety.PollIntervalMillis,
			StartupTimeoutSec:  deployment.Safety.StartupTimeoutSec,
			WorkloadTimeoutSec: deployment.Safety.WorkloadTimeoutSec,
			HTTPTimeoutSec:     deployment.Safety.HTTPTimeoutSec,
		},
		Warmup:    compileWarmup(suite.Warmup, deployment.Client),
		Profiles:  []vllmbench.Profile{profile},
		Workloads: compileCases(cases, profileName, deployment.Client),
	}
	vllmbench.ApplyDefaults(&spec)
	intent, err := json.Marshal(map[string]any{"suite": suite.Name})
	if err != nil {
		return Compiled{}, err
	}
	spec.Generator = &artifact.GeneratorStamp{Tool: "localperf-suite", Version: Version, Intent: intent}
	// Artifacts persist the redacted execution document. Hash that exact form
	// so credentials never affect provenance and the stored bytes verify.
	hash, err := vllmbench.SpecContentHash(vllmbench.RedactedSpec(spec))
	if err != nil {
		return Compiled{}, err
	}
	spec.Generator.ContentHash = hash
	spec.Provenance = vllmbench.SpecProvenanceGenerated
	if err := vllmbench.ValidateSpec(spec); err != nil {
		return Compiled{}, fmt.Errorf("compiled execution is invalid: %w", err)
	}
	resolvedSuite := suite
	resolvedSuite.Cases = cases
	return Compiled{Suite: resolvedSuite, Deployment: deployment, Spec: spec}, nil
}

func WriteExecutionFiles(runDir string, compiled Compiled) error {
	if err := os.MkdirAll(runDir, 0o755); err != nil {
		return err
	}
	plan := vllmbench.BuildPlan(compiled.Spec, runDir)
	files := []struct {
		name  string
		value any
	}{
		{"suite.json", compiled.Suite},
		{"deployment.json", redactedDeployment(compiled.Deployment)},
		{"execution-plan.json", plan},
	}
	for _, file := range files {
		if err := writeJSON(filepath.Join(runDir, file.name), file.value); err != nil {
			return err
		}
	}
	return nil
}

func VerifyExecutionFiles(runDir string, compiled Compiled) error {
	plan := vllmbench.BuildPlan(compiled.Spec, runDir)
	files := []struct {
		name  string
		value any
	}{
		{"suite.json", compiled.Suite},
		{"deployment.json", redactedDeployment(compiled.Deployment)},
		{"execution-plan.json", plan},
	}
	for _, file := range files {
		want, err := json.MarshalIndent(file.value, "", "  ")
		if err != nil {
			return err
		}
		got, err := os.ReadFile(filepath.Join(runDir, file.name))
		if err != nil {
			return fmt.Errorf("resume requires the original %s: %w", file.name, err)
		}
		if strings.TrimSpace(string(got)) != strings.TrimSpace(string(want)) {
			return fmt.Errorf("resume refused: %s differs from the original execution", file.name)
		}
	}
	return nil
}

func BuiltinSuiteNames() []string {
	return []string{"practical-64k", "throughput-4k", "context-ladder"}
}

func builtinSuite(name string) (Suite, bool) {
	switch name {
	case "practical-64k":
		return practicalSuite(), true
	case "throughput-4k":
		return throughputSuite(), true
	case "context-ladder":
		return contextSuite(), true
	default:
		return Suite{}, false
	}
}

func practicalSuite() Suite {
	fullBudget := activeTokenBudget(65536)
	return Suite{Version: Version, Name: "practical-64k", Description: "Generation at minimal and near-full 64k context; every point reports decode and effective prefill.", Warmup: defaultWarmup(), Cases: []Case{
		benchmarkCase("generate-empty", "decode", 1, 1024, 65536, vllmbench.ContextSemanticsCapacity, []vllmbench.Batch{{Concurrency: 1, Requests: 1}, {Concurrency: 6, Requests: 6}}, 3),
		benchmarkCase("generate-full", "decode", fullBudget-1024, 1024, 65536, vllmbench.ContextSemanticsActive, []vllmbench.Batch{{Concurrency: 1, Requests: 1}, {Concurrency: 6, Requests: 6}}, 3),
	}}
}

func throughputSuite() Suite {
	budget := activeTokenBudget(4096)
	return Suite{Version: Version, Name: "throughput-4k", Description: "4k generation throughput at an explicit concurrency ladder.", Warmup: defaultWarmup(), Cases: []Case{
		benchmarkCase("throughput-4k", "decode", budget-1024, 1024, 4096, vllmbench.ContextSemanticsActive, standardBatches(), 3),
	}}
}

func contextSuite() Suite {
	suite := Suite{Version: Version, Name: "context-ladder", Description: "Explicit prefill and decode cases across the active-context ladder.", Warmup: defaultWarmup()}
	for _, context := range []int{4096, 8192, 16384, 32768, 65536, 131072} {
		label := vllmbench.TokenCountLabel(context)
		batches := standardBatches()
		if context >= 131072 {
			batches = []vllmbench.Batch{{Concurrency: 1, Requests: 1}, {Concurrency: 4, Requests: 4}}
		}
		budget := activeTokenBudget(context)
		suite.Cases = append(suite.Cases,
			benchmarkCase("prefill-"+label, "prefill", budget-1, 1, context, vllmbench.ContextSemanticsActive, batches, 1),
			benchmarkCase("decode-"+label, "decode", budget-1024, 1024, context, vllmbench.ContextSemanticsActive, batches, 1),
		)
	}
	return suite
}

// activeTokenBudget leaves two percent of the server limit for chat-template
// tokens and tokenizer drift. The resulting request remains well inside the
// validated 90%-100% active-context band.
func activeTokenBudget(context int) int {
	headroom := context / 50
	if headroom < 64 {
		headroom = 64
	}
	return context - headroom
}

func defaultWarmup() Warmup {
	return Warmup{Enabled: true, InputTokens: 16, OutputTokens: 16, Requests: 4, Concurrency: 1}
}

func standardBatches() []vllmbench.Batch {
	return []vllmbench.Batch{{Concurrency: 1, Requests: 1}, {Concurrency: 4, Requests: 4}, {Concurrency: 8, Requests: 8}, {Concurrency: 16, Requests: 16}, {Concurrency: 32, Requests: 32}}
}

func benchmarkCase(name, phase string, input, output, target int, semantics string, batches []vllmbench.Batch, repeats int) Case {
	return Case{Name: name, Role: vllmbench.WorkloadRoleBenchmark, Phase: phase, InputTokens: input, OutputTokens: output, ContextTarget: target, ContextSemantics: semantics, Batches: batches, Repeats: repeats, Temperature: 0, IgnoreEOS: true}
}

func compileWarmup(warmup Warmup, client Client) vllmbench.WarmupConfig {
	return vllmbench.WarmupConfig{Enabled: warmup.Enabled, NumPrompts: warmup.Requests, MaxConcurrency: warmup.Concurrency, BenchmarkTrafficConfig: traffic(client, warmup.InputTokens, warmup.OutputTokens, false)}
}

func compileCases(cases []Case, profile string, client Client) []vllmbench.Workload {
	workloads := make([]vllmbench.Workload, 0, len(cases))
	for _, item := range cases {
		temperature := item.Temperature
		workloads = append(workloads, vllmbench.Workload{
			BenchmarkTrafficConfig: traffic(client, item.InputTokens, item.OutputTokens, true),
			Name:                   item.Name, Role: item.Role, Phase: item.Phase,
			ContextTarget: item.ContextTarget, ContextSemantics: item.ContextSemantics,
			LoadGenerator: defaultString(client.LoadGenerator, vllmbench.LoadGeneratorVLLMBench),
			Profiles:      []string{profile}, Batches: append([]vllmbench.Batch(nil), item.Batches...),
			Repeats: item.Repeats, IgnoreEOS: item.IgnoreEOS, Temperature: &temperature,
		})
	}
	return workloads
}

func traffic(client Client, input, output int, detailed bool) vllmbench.BenchmarkTrafficConfig {
	extra := append([]string(nil), client.ExtraArgs...)
	if strings.TrimSpace(client.Tokenizer) != "" {
		extra = append(extra, "--tokenizer", client.Tokenizer)
	}
	return vllmbench.BenchmarkTrafficConfig{
		Backend: defaultString(client.Backend, "openai-chat"), Endpoint: client.Endpoint,
		DatasetName: "random", RequestRate: "inf", RandomInputLen: input,
		RandomOutputLen: output, RandomRangeRatio: "0", SaveDetailed: boolPointer(detailed), ExtraArgs: extra,
	}
}

func selectCases(cases []Case, selection Selection) ([]Case, error) {
	wantedCases := stringSet(selection.Cases)
	wantedConcurrency := intSet(selection.Concurrencies)
	seenCases := map[string]bool{}
	seenConcurrency := map[int]bool{}
	var out []Case
	for _, item := range cases {
		if len(wantedCases) > 0 && !wantedCases[item.Name] {
			continue
		}
		seenCases[item.Name] = true
		copy := item
		copy.Batches = nil
		for _, batch := range item.Batches {
			if len(wantedConcurrency) == 0 || wantedConcurrency[batch.Concurrency] {
				copy.Batches = append(copy.Batches, batch)
				seenConcurrency[batch.Concurrency] = true
			}
		}
		if len(copy.Batches) > 0 {
			out = append(out, copy)
		}
	}
	for name := range wantedCases {
		if !seenCases[name] {
			return nil, fmt.Errorf("unknown case %q", name)
		}
	}
	for value := range wantedConcurrency {
		if !seenConcurrency[value] {
			return nil, fmt.Errorf("concurrency %d is not present in the selected cases", value)
		}
	}
	if len(out) == 0 {
		return nil, errors.New("selection contains no benchmark cases")
	}
	return out, nil
}

func suiteLimits(cases []Case) (int, int) {
	maxContext, maxConcurrency := 0, 0
	for _, item := range cases {
		if item.ContextTarget > maxContext {
			maxContext = item.ContextTarget
		}
		for _, batch := range item.Batches {
			if batch.Concurrency > maxConcurrency {
				maxConcurrency = batch.Concurrency
			}
		}
	}
	return maxContext, maxConcurrency
}

func validateSuite(suite Suite) error {
	var issues []string
	if suite.Version != Version {
		issues = append(issues, `version must be "1"`)
	}
	if strings.TrimSpace(suite.Name) == "" {
		issues = append(issues, "name is required")
	}
	if len(suite.Cases) == 0 {
		issues = append(issues, "at least one case is required")
	}
	names := map[string]bool{}
	for index, item := range suite.Cases {
		prefix := fmt.Sprintf("cases[%d]", index)
		if strings.TrimSpace(item.Name) == "" {
			issues = append(issues, prefix+": name is required")
		}
		if names[item.Name] {
			issues = append(issues, prefix+": duplicate name "+item.Name)
		}
		names[item.Name] = true
		if item.InputTokens <= 0 || item.OutputTokens <= 0 || item.ContextTarget <= 0 || item.Repeats <= 0 {
			issues = append(issues, prefix+": token counts, context_target, and repeats must be positive")
		}
		if len(item.Batches) == 0 {
			issues = append(issues, prefix+": batches must not be empty")
		}
	}
	if len(issues) > 0 {
		return errors.New(strings.Join(issues, "\n"))
	}
	return nil
}

func validateDeployment(deployment Deployment) error {
	var issues []string
	if deployment.Version != Version {
		issues = append(issues, `version must be "1"`)
	}
	if strings.TrimSpace(deployment.Name) == "" {
		issues = append(issues, "name is required")
	}
	if strings.TrimSpace(deployment.Model) == "" {
		issues = append(issues, "model is required")
	}
	if strings.TrimSpace(deployment.Runtime.Name) == "" || strings.TrimSpace(deployment.Runtime.Type) == "" {
		issues = append(issues, "runtime.name and runtime.type are required")
	}
	if deployment.Runtime.Port <= 0 && strings.TrimSpace(deployment.Runtime.EndpointBaseURL) == "" {
		issues = append(issues, "runtime.port is required without endpoint_base_url")
	}
	if deployment.Runtime.Managed && strings.TrimSpace(deployment.Runtime.Command) == "" {
		issues = append(issues, "runtime.command is required for a managed deployment")
	}
	if deployment.Safety.MinMemAvailableGiB <= 0 {
		issues = append(issues, "safety.min_mem_available_gib must be positive")
	}
	if len(issues) > 0 {
		return errors.New(strings.Join(issues, "\n"))
	}
	return nil
}

func rejectOwnedRuntimeArgs(args []string) error {
	owned := []string{"--max-model-len", "--max-num-seqs", "--max-num-batched-tokens", "--gpu-memory-utilization", "--kv-cache-dtype", "--attention-backend", "--moe-backend", "--enable-prefix-caching", "--no-enable-prefix-caching", "--enable-sleep-mode"}
	for _, arg := range args {
		for _, flag := range owned {
			if arg == flag || strings.HasPrefix(arg, flag+"=") {
				return fmt.Errorf("runtime.args contains %s; set the deployment server field instead (suite-derived limits cannot be overridden)", flag)
			}
		}
	}
	return nil
}

func appendRevision(args []string, revision string) []string {
	out := append([]string(nil), args...)
	if strings.TrimSpace(revision) != "" {
		out = append(out, "--revision="+revision)
	}
	return out
}

func loadStrictJSON(path string, out any) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(out); err != nil {
		return err
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		if err == nil {
			return errors.New("file must contain exactly one JSON document")
		}
		return err
	}
	return nil
}

func writeJSON(path string, value any) error {
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(data, '\n'), 0o644)
}

func redactedDeployment(deployment Deployment) Deployment {
	copy := deployment
	copy.Runtime.Env = map[string]string{}
	keys := make([]string, 0, len(deployment.Runtime.Env))
	for key := range deployment.Runtime.Env {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		value := deployment.Runtime.Env[key]
		if sensitiveKey(key) {
			value = "<redacted>"
		}
		copy.Runtime.Env[key] = value
	}
	return copy
}

func sensitiveKey(key string) bool {
	upper := strings.ToUpper(strings.ReplaceAll(key, "-", "_"))
	for _, token := range []string{"TOKEN", "SECRET", "PASSWORD", "CREDENTIAL", "API_KEY", "AUTHORIZATION", "COOKIE"} {
		if strings.Contains(upper, token) {
			return true
		}
	}
	for _, part := range strings.FieldsFunc(upper, func(char rune) bool {
		return !((char >= 'A' && char <= 'Z') || (char >= '0' && char <= '9'))
	}) {
		switch part {
		case "AUTH", "KEY", "PASS", "PASSPHRASE", "PRIVATE":
			return true
		}
	}
	return false
}

func defaultString(value, fallback string) string {
	if strings.TrimSpace(value) == "" {
		return fallback
	}
	return value
}
func boolPointer(value bool) *bool { return &value }
func stringSet(values []string) map[string]bool {
	out := map[string]bool{}
	for _, value := range values {
		out[value] = true
	}
	return out
}
func intSet(values []int) map[int]bool {
	out := map[int]bool{}
	for _, value := range values {
		out[value] = true
	}
	return out
}
