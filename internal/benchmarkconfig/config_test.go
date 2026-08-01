package benchmarkconfig

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/osolmaz/localperf/internal/vllmbench"
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
	plan := vllmbench.BuildPlan(compiled.Spec, t.TempDir())
	if compiled.Spec.Provenance != vllmbench.SpecProvenanceGenerated || vllmbench.SpecProvenance(compiled.Spec) != vllmbench.SpecProvenanceGenerated || compiled.Spec.Generator == nil || compiled.Spec.Generator.Tool != "localperf-suite" {
		t.Fatalf("compiled provenance = %q / %+v", compiled.Spec.Provenance, compiled.Spec.Generator)
	}
	if len(plan) != 12 {
		t.Fatalf("planned measurements = %d, want 12", len(plan))
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
			if repeats != 3 {
				t.Fatalf("%s c%d repeats = %d, want 3", name, concurrency, repeats)
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
			if benchmarkCase.ContextSemantics != vllmbench.ContextSemanticsActive {
				continue
			}
			requested := benchmarkCase.InputTokens + benchmarkCase.OutputTokens
			if requested >= benchmarkCase.ContextTarget {
				t.Fatalf("%s/%s requests %d tokens against limit %d", name, benchmarkCase.Name, requested, benchmarkCase.ContextTarget)
			}
			if float64(requested) < vllmbench.ContextTargetMinFrac*float64(benchmarkCase.ContextTarget) {
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
	plan := vllmbench.BuildPlan(compiled.Spec, t.TempDir())
	if len(plan) != 3 {
		t.Fatalf("planned measurements = %d, want 3", len(plan))
	}
	for _, run := range plan {
		if run.Workload.Name != "generate-full" || run.Concurrency != 1 {
			t.Fatalf("unexpected selected run: %+v", run)
		}
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
	deployment.Runtime.Args = []string{"--max-model-len=4096"}
	if _, err := Compile(suite, deployment, Selection{}); err == nil || !strings.Contains(err.Error(), "suite-derived limits cannot be overridden") {
		t.Fatalf("override error = %v", err)
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
	dir := t.TempDir()
	if err := WriteExecutionFiles(dir, compiled); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"suite.json", "deployment.json", "execution-plan.json"} {
		if _, err := os.Stat(filepath.Join(dir, name)); err != nil {
			t.Fatal(err)
		}
	}
	data, err := os.ReadFile(filepath.Join(dir, "deployment.json"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "secret") || strings.Count(string(data), "redacted") != 5 || !strings.Contains(string(data), "yes") {
		t.Fatalf("unexpected redacted deployment: %s", data)
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
	persisted := vllmbench.RedactedSpec(compiled.Spec)
	if got := vllmbench.SpecProvenance(persisted); got != vllmbench.SpecProvenanceGenerated {
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
	changed.Deployment.Server.GPUMemoryUtilization = 0.6
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
		"deployment trailing": {`{"version":"1","name":"x","model":"m","runtime":{"name":"r","type":"vllm","managed":false,"port":1},"server":{},"client":{},"safety":{"min_mem_available_gib":1}} {}`, func(path string) error { _, err := LoadDeployment(path); return err }},
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
		Runtime: Runtime{Name: "vllm", Type: "vllm-managed", Command: "vllm", BenchCommand: "vllm", Managed: true, Port: 8101},
		Server:  Server{GPUMemoryUtilization: 0.5, EnablePrefixCaching: &disabled},
		Safety:  Safety{MinMemAvailableGiB: 40},
	}
}
