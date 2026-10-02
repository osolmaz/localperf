package benchmarkconfig

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/osolmaz/localperf/internal/runner"
)

func TestPracticalSuiteCompilesExactlyTwelveMeasurements(t *testing.T) {
	suite, err := LoadSuite("practical-64k")
	if err != nil {
		t.Fatal(err)
	}
	compiled, err := Compile(suite, testDeployment(), Selection{})
	if err != nil {
		t.Fatal(err)
	}
	plan := runner.BuildPlan(compiled.Spec, t.TempDir())
	if compiled.Spec.Provenance != runner.SpecProvenanceGenerated || runner.SpecProvenance(compiled.Spec) != runner.SpecProvenanceGenerated || compiled.Spec.Generator == nil || compiled.Spec.Generator.Tool != "localperf-suite" {
		t.Fatalf("compiled provenance = %q / %+v", compiled.Spec.Provenance, compiled.Spec.Generator)
	}
	// 2 cases x 2 concurrency points x max_repeats 10.
	if len(plan) != 40 {
		t.Fatalf("planned measurements = %d, want 40", len(plan))
	}
	want := map[string]map[int]int{
		"generate-empty": {1: 1, 6: 6},
		"generate-full":  {1: 1, 6: 6},
	}
	counts := map[string]map[int]int{}
	for _, run := range plan {
		if run.Workload.Phase != "decode" {
			t.Fatalf("case %s phase = %q, want decode", run.Workload.Name, run.Workload.Phase)
		}
		if run.Workload.NumPrompts != want[run.Workload.Name][run.Concurrency] {
			t.Fatalf("%s c%d requests = %d, want %d", run.Workload.Name, run.Concurrency, run.Workload.NumPrompts, want[run.Workload.Name][run.Concurrency])
		}
		if counts[run.Workload.Name] == nil {
			counts[run.Workload.Name] = map[int]int{}
		}
		counts[run.Workload.Name][run.Concurrency]++
	}
	for name, byConcurrency := range counts {
		for concurrency, repeats := range byConcurrency {
			if repeats != DefaultConvergence.MaxRepeats {
				t.Fatalf("%s c%d planned repeats = %d, want %d", name, concurrency, repeats, DefaultConvergence.MaxRepeats)
			}
		}
	}
}

func TestBuiltinActiveCasesLeaveTemplateHeadroom(t *testing.T) {
	for _, name := range BuiltinSuiteNames() {
		suite, err := LoadSuite(name)
		if err != nil {
			t.Fatal(err)
		}
		for _, benchmarkCase := range suite.Cases {
			if benchmarkCase.ContextSemantics != runner.ContextSemanticsActive {
				continue
			}
			requested := benchmarkCase.InputTokens + benchmarkCase.OutputTokens
			if requested >= benchmarkCase.ContextTarget {
				t.Fatalf("%s/%s requests %d tokens against limit %d", name, benchmarkCase.Name, requested, benchmarkCase.ContextTarget)
			}
			if float64(requested) < runner.ContextTargetMinFrac*float64(benchmarkCase.ContextTarget) {
				t.Fatalf("%s/%s requests %d tokens below active-context band for %d", name, benchmarkCase.Name, requested, benchmarkCase.ContextTarget)
			}
		}
	}
}

func TestSelectionUsesCaseAndConcurrencyTerms(t *testing.T) {
	suite, _ := LoadSuite("practical-64k")
	compiled, err := Compile(suite, testDeployment(), Selection{Cases: []string{"generate-full"}, Concurrencies: []int{1}})
	if err != nil {
		t.Fatal(err)
	}
	plan := runner.BuildPlan(compiled.Spec, t.TempDir())
	if len(plan) != DefaultConvergence.MaxRepeats {
		t.Fatalf("planned measurements = %d, want %d", len(plan), DefaultConvergence.MaxRepeats)
	}
	for _, run := range plan {
		if run.Workload.Name != "generate-full" || run.Concurrency != 1 {
			t.Fatalf("unexpected selected run: %+v", run)
		}
	}
}

func TestEndpointDeploymentWarmsUpOverHTTP(t *testing.T) {
	suite, _ := LoadSuite("practical-64k")
	deployment := testDeployment()
	deployment.Runtime = Runtime{Name: "hosted", Type: runner.EngineOpenAIEndpoint, EndpointBaseURL: "https://api.example.com/v1"}
	deployment.Server = Server{}
	compiled, err := Compile(suite, deployment, Selection{Cases: []string{"generate-empty"}, Concurrencies: []int{1}})
	if err != nil {
		t.Fatal(err)
	}
	if !compiled.Spec.Warmup.Enabled || compiled.Spec.Warmup.LoadGenerator != runner.LoadGeneratorHTTP {
		t.Fatalf("warmup = %+v, want enabled over localperf_http", compiled.Spec.Warmup)
	}
	if profile := compiled.Spec.Profiles[0]; profile.Managed || profile.HealthPath != "/v1/models" {
		t.Fatalf("endpoint profile = %+v", profile)
	}
}

func TestLlamaCppDeploymentStartsOneSlotPerUser(t *testing.T) {
	suite, _ := LoadSuite("practical-64k")
	compiled, err := Compile(suite, testDeployment(), Selection{})
	if err != nil {
		t.Fatal(err)
	}
	profile := compiled.Spec.Profiles[0]
	if !profile.Managed || profile.HealthPath != "/health" {
		t.Fatalf("llama.cpp profile = %+v", profile)
	}
	command := strings.Join(runner.ServeCommand(compiled.Spec, profile).Args, " ")
	for _, want := range []string{"llama-server --model /models/test.gguf --alias test/model", "--parallel 6", "--ctx-size 393216", "--flash-attn on"} {
		if !strings.Contains(command, want) {
			t.Fatalf("serve command %q lacks %q", command, want)
		}
	}
	if strings.Contains(command, "--revision") {
		t.Fatalf("llama-server command carries a vLLM flag: %q", command)
	}
	for _, workload := range compiled.Spec.Workloads {
		if workload.LoadGenerator != runner.LoadGeneratorHTTP || workload.ExtraBody != `{"cache_prompt":false}` {
			t.Fatalf("workload %s = %s / %q, want localperf_http without prompt cache", workload.Name, workload.LoadGenerator, workload.ExtraBody)
		}
	}
}

func TestVLLMDeploymentKeepsRevisionAndBenchCLI(t *testing.T) {
	suite, _ := LoadSuite("practical-64k")
	deployment := vllmDeployment()
	deployment.ModelRevision = "abc123"
	deployment.Client.LoadGenerator = runner.LoadGeneratorVLLMBench
	compiled, err := Compile(suite, deployment, Selection{})
	if err != nil {
		t.Fatal(err)
	}
	command := strings.Join(runner.ServeCommand(compiled.Spec, compiled.Spec.Profiles[0]).Args, " ")
	if !strings.HasPrefix(command, "vllm serve test/model") || !strings.Contains(command, "--revision=abc123") || !strings.Contains(command, "--gpu-memory-utilization 0.5") {
		t.Fatalf("vLLM serve command = %q", command)
	}
	if compiled.Spec.Workloads[0].ExtraBody != "" {
		t.Fatalf("vLLM workload extra body = %q, want none", compiled.Spec.Workloads[0].ExtraBody)
	}
}

func TestDeploymentRuntimeTypeOwnsItsFields(t *testing.T) {
	for name, test := range map[string]struct {
		mutate func(*Deployment)
		want   string
	}{
		"unknown type":            {func(value *Deployment) { value.Runtime.Type = "llama.cpp" }, "runtime.type must be one of"},
		"managed without model":   {func(value *Deployment) { value.Server.LlamaCpp = nil }, "server.llama_cpp.model_file is required"},
		"managed with endpoint":   {func(value *Deployment) { value.Runtime.EndpointBaseURL = "http://127.0.0.1:8080/v1" }, "runtime.endpoint_base_url is for endpoint types"},
		"managed without command": {func(value *Deployment) { value.Runtime.Command = "" }, "runtime.command is required"},
		"endpoint with settings": {func(value *Deployment) {
			value.Runtime = Runtime{Name: "llama.cpp", Type: runner.EngineLlamaCppEndpoint, Port: 8080}
		}, "server.llama_cpp is only for llama-cpp-managed"},
		"endpoint with command": {func(value *Deployment) {
			value.Runtime = Runtime{Name: "llama.cpp", Type: runner.EngineLlamaCppEndpoint, Command: "llama-server", Port: 8080}
			value.Server.LlamaCpp = nil
		}, "runtime.command is for managed types"},
		"vllm block on llama.cpp": {func(value *Deployment) { value.Server.VLLM = &VLLMServer{GPUMemoryUtilization: 0.5} }, "server.vllm is only for vLLM runtimes"},
		"vllm bench on llama.cpp": {func(value *Deployment) { value.Client.LoadGenerator = runner.LoadGeneratorVLLMBench }, "vllm_bench requires a vLLM runtime"},
		"bench command on llama":  {func(value *Deployment) { value.Runtime.BenchCommand = "vllm" }, "runtime.bench_command is for vLLM runtimes"},
	} {
		t.Run(name, func(t *testing.T) {
			deployment := testDeployment()
			test.mutate(&deployment)
			suite, _ := LoadSuite("practical-64k")
			if _, err := Compile(suite, deployment, Selection{}); err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("Compile error = %v, want %q", err, test.want)
			}
		})
	}
}

func TestHTTPDeploymentRejectsIgnoredCLIClientOptions(t *testing.T) {
	suite, _ := LoadSuite("practical-64k")
	for name, mutate := range map[string]func(*Deployment){
		"tokenizer":  func(value *Deployment) { value.Client.Tokenizer = "custom-tokenizer" },
		"extra_args": func(value *Deployment) { value.Client.ExtraArgs = []string{"--request-id-prefix", "ignored"} },
	} {
		t.Run(name, func(t *testing.T) {
			deployment := testDeployment()
			deployment.Client.LoadGenerator = runner.LoadGeneratorHTTP
			mutate(&deployment)
			if _, err := Compile(suite, deployment, Selection{}); err == nil || !strings.Contains(err.Error(), "unsupported with localperf_http") {
				t.Fatalf("Compile error = %v", err)
			}
		})
	}
}

func TestSuiteDerivesServerLimitsAndRejectsOverrides(t *testing.T) {
	suite, _ := LoadSuite("practical-64k")
	deployment := testDeployment()
	compiled, err := Compile(suite, deployment, Selection{})
	if err != nil {
		t.Fatal(err)
	}
	profile := compiled.Spec.Profiles[0]
	if profile.MaxModelLen != 65536 || profile.MaxNumSeqs != 6 {
		t.Fatalf("derived limits = %d/%d, want 65536/6", profile.MaxModelLen, profile.MaxNumSeqs)
	}
	for _, args := range [][]string{{"--ctx-size=4096"}, {"-np", "2"}, {"--model", "/other.gguf"}} {
		deployment.Runtime.Args = args
		if _, err := Compile(suite, deployment, Selection{}); err == nil || !strings.Contains(err.Error(), "suite-derived limits cannot be overridden") {
			t.Fatalf("override error for %v = %v", args, err)
		}
	}
	deployment = testDeployment()
	deployment.Server.SpeculativeDecoding = []string{"--parallel=2"}
	if _, err := Compile(suite, deployment, Selection{}); err == nil || !strings.Contains(err.Error(), "server.speculative_decoding contains --parallel") {
		t.Fatalf("speculative override error = %v", err)
	}
	deployment = vllmDeployment()
	deployment.Server.SpeculativeDecoding = []string{"--gpu-memory-utilization=0.9"}
	if _, err := Compile(suite, deployment, Selection{}); err == nil || !strings.Contains(err.Error(), "server.speculative_decoding contains --gpu-memory-utilization") {
		t.Fatalf("vLLM speculative override error = %v", err)
	}
	for field, mutate := range map[string]func(*Deployment){
		"runtime.args":                func(value *Deployment) { value.Runtime.Args = []string{"--api-key", "do-not-persist"} },
		"server.speculative_decoding": func(value *Deployment) { value.Server.SpeculativeDecoding = []string{"--hf-token=do-not-persist"} },
		"client.extra_args":           func(value *Deployment) { value.Client.ExtraArgs = []string{"--auth-token=do-not-persist"} },
	} {
		deployment = vllmDeployment()
		deployment.Client.LoadGenerator = runner.LoadGeneratorVLLMBench
		mutate(&deployment)
		_, err := Compile(suite, deployment, Selection{})
		if err == nil || !strings.Contains(err.Error(), field+" contains credential flag") || strings.Contains(err.Error(), "do-not-persist") {
			t.Fatalf("credential argument rejection for %s = %v", field, err)
		}
	}
}

func TestExecutionFilesAreWrittenAndSecretsAreRedacted(t *testing.T) {
	suite, _ := LoadSuite("practical-64k")
	deployment := testDeployment()
	deployment.Runtime.Env = map[string]string{
		"HF_TOKEN": "secret-token", "AWS_ACCESS_KEY_ID": "secret-key", "SSH_KEY": "secret-ssh",
		"AUTH": "secret-auth", "PASS": "secret-pass", "VISIBLE": "yes",
	}
	compiled, err := Compile(suite, deployment, Selection{Cases: []string{"generate-empty"}, Concurrencies: []int{1}})
	if err != nil {
		t.Fatal(err)
	}
	if compiled.Spec.Profiles[0].Env["HF_TOKEN"] != "secret-token" {
		t.Fatal("runtime credential was not propagated to the HTTP profile")
	}
	dir := t.TempDir()
	if err := WriteExecutionFiles(dir, compiled); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"suite.json", "deployment.json", "execution-plan.json"} {
		if _, err := os.Stat(filepath.Join(dir, name)); err != nil {
			t.Fatal(err)
		}
	}
	for _, name := range []string{"deployment.json", "execution-plan.json"} {
		data, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(data), "secret") || !strings.Contains(string(data), "redacted") || !strings.Contains(string(data), "yes") {
			t.Fatalf("unexpected redacted %s: %s", name, data)
		}
	}
}

func TestSuiteProvenanceHashesPersistedRedactedExecution(t *testing.T) {
	suite, _ := LoadSuite("practical-64k")
	deployment := testDeployment()
	deployment.Runtime.Env = map[string]string{"AUTH": "secret-auth"}
	compiled, err := Compile(suite, deployment, Selection{})
	if err != nil {
		t.Fatal(err)
	}
	persisted := runner.RedactedSpec(compiled.Spec)
	if got := runner.SpecProvenance(persisted); got != runner.SpecProvenanceGenerated {
		t.Fatalf("redacted execution provenance = %q, want generated", got)
	}
}

func TestResumeVerificationRejectsChangedDeployment(t *testing.T) {
	suite, _ := LoadSuite("practical-64k")
	compiled, err := Compile(suite, testDeployment(), Selection{Cases: []string{"generate-empty"}, Concurrencies: []int{1}})
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	if err := WriteExecutionFiles(dir, compiled); err != nil {
		t.Fatal(err)
	}
	if err := VerifyExecutionFiles(dir, compiled); err != nil {
		t.Fatal(err)
	}
	changed := compiled
	settings := *changed.Deployment.Server.LlamaCpp
	settings.FlashAttn = "off"
	changed.Deployment.Server.LlamaCpp = &settings
	if err := VerifyExecutionFiles(dir, changed); err == nil || !strings.Contains(err.Error(), "deployment.json differs") {
		t.Fatalf("changed deployment verification error = %v", err)
	}
}

func TestPublicDocumentsRejectUnknownFieldsAndTrailingJSON(t *testing.T) {
	for name, test := range map[string]struct {
		contents string
		load     func(string) error
	}{
		"suite unknown":       {`{"version":"1","name":"x","warmup":{},"cases":[],"unknown":true}`, func(path string) error { _, err := LoadSuite(path); return err }},
		"deployment trailing": {`{"version":"1","name":"x","model":"m","runtime":{"name":"r","type":"llama-cpp-endpoint","port":1},"server":{},"client":{},"safety":{"min_mem_available_gib":1}} {}`, func(path string) error { _, err := LoadDeployment(path); return err }},
	} {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "input.json")
			if err := os.WriteFile(path, []byte(test.contents), 0o644); err != nil {
				t.Fatal(err)
			}
			if err := test.load(path); err == nil {
				t.Fatal("load error = nil")
			}
		})
	}
}

func testDeployment() Deployment {
	disabled := false
	return Deployment{
		Version: Version,
		Name:    "test-deployment",
		Model:   "test/model",
		Runtime: Runtime{Name: "llama.cpp", Type: runner.EngineLlamaCppManaged, Command: "llama-server", Port: 8101},
		Server: Server{
			EnablePrefixCaching: &disabled,
			LlamaCpp:            &runner.LlamaCppSettings{ModelFile: "/models/test.gguf", FlashAttn: "on"},
		},
		Safety: Safety{MinMemAvailableGiB: 8},
	}
}

func vllmDeployment() Deployment {
	disabled := false
	return Deployment{
		Version: Version,
		Name:    "test-vllm",
		Model:   "test/model",
		Runtime: Runtime{Name: "vllm", Type: runner.EngineVLLMManaged, Command: "vllm", BenchCommand: "vllm", Port: 8101},
		Server:  Server{EnablePrefixCaching: &disabled, VLLM: &VLLMServer{GPUMemoryUtilization: 0.5}},
		Safety:  Safety{MinMemAvailableGiB: 40},
	}
}

func TestExampleDeploymentsCompile(t *testing.T) {
	paths, err := filepath.Glob("../../examples/deployments/*.json")
	if err != nil || len(paths) == 0 {
		t.Fatalf("example deployments = %v, %v", paths, err)
	}
	suite, _ := LoadSuite("practical-64k")
	for _, path := range paths {
		deployment, err := LoadDeployment(path)
		if err != nil {
			t.Fatalf("%s: %v", path, err)
		}
		if _, err := Compile(suite, deployment, Selection{}); err != nil {
			t.Fatalf("%s: %v", path, err)
		}
	}
}
