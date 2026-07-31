package sweepplan

import (
	"testing"

	"github.com/osolmaz/localperf/internal/vllmbench"
)

func TestPlanStampsVerifiableProvenance(t *testing.T) {
	spec, err := Plan(PlanRequest{
		Model:    "example/model",
		Contexts: []int{8192, 65536},
		Trims:    []vllmbench.LadderTrim{{Context: 65536, MaxConcurrency: 8, Reason: "12 GiB KV budget"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if spec.Generator == nil || spec.Generator.Tool != "localperf sweep plan" {
		t.Fatalf("generator stamp = %+v, want tool identity", spec.Generator)
	}
	if got := vllmbench.SpecProvenance(spec); got != vllmbench.SpecProvenanceGenerated {
		t.Fatalf("provenance = %q, want generated for an unedited spec", got)
	}
	spec.Workloads[0].NumPrompts = 999
	if got := vllmbench.SpecProvenance(spec); got != vllmbench.SpecProvenanceEdited {
		t.Fatalf("provenance = %q, want edited after mutation", got)
	}
	// The stamp's trusted fields are covered by the hash: reports rely on
	// declared trims, so editing them must demote the spec too.
	fresh, err := Plan(PlanRequest{
		Model:    "example/model",
		Contexts: []int{8192, 65536},
		Trims:    []vllmbench.LadderTrim{{Context: 65536, MaxConcurrency: 8, Reason: "12 GiB KV budget"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	fresh.Generator.LadderTrims = append(fresh.Generator.LadderTrims, vllmbench.LadderTrim{Context: 8192, MaxConcurrency: 1, Reason: "invented"})
	if got := vllmbench.SpecProvenance(fresh); got != vllmbench.SpecProvenanceEdited {
		t.Fatalf("provenance = %q, want edited after tampering with declared trims", got)
	}
	spec.Generator = nil
	if got := vllmbench.SpecProvenance(spec); got != vllmbench.SpecProvenanceCustom {
		t.Fatalf("provenance = %q, want custom without a stamp", got)
	}
}

func TestTrimsCapLaddersAndRideTheStamp(t *testing.T) {
	spec, err := Plan(PlanRequest{
		Model:    "example/model",
		Contexts: []int{32768, 65536},
		Trims: []vllmbench.LadderTrim{
			{Context: 32768, MaxConcurrency: 16, Reason: "KV budget"},
			{Context: 65536, MaxConcurrency: 8, Reason: "KV budget"},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	maxByWorkload := map[string]int{}
	for _, workload := range spec.Workloads {
		top := 0
		for _, value := range workload.MaxConcurrency {
			if value > top {
				top = value
			}
		}
		maxByWorkload[workload.Name] = top
	}
	if maxByWorkload["decode-32k"] != 16 || maxByWorkload["prefill-32k"] != 16 {
		t.Fatalf("32k ladders = %v, want capped at 16", maxByWorkload)
	}
	if maxByWorkload["decode-64k"] != 8 || maxByWorkload["prefill-64k"] != 8 {
		t.Fatalf("64k ladders = %v, want capped at 8", maxByWorkload)
	}
	if len(spec.Generator.LadderTrims) != 2 {
		t.Fatalf("stamped trims = %+v, want both declared trims", spec.Generator.LadderTrims)
	}
	for _, profile := range spec.Profiles {
		if profile.Name == "64k" && profile.MaxNumSeqs != 8 {
			t.Fatalf("64k max_num_seqs = %d, want sized to trimmed ladder", profile.MaxNumSeqs)
		}
	}
}

func TestRuntimeIntentReachesSpec(t *testing.T) {
	spec, err := Plan(PlanRequest{
		Model:                "example/model",
		Contexts:             []int{8192},
		VLLMCommand:          "/opt/runtimes/vllm/bin/vllm",
		GPUMemoryUtilization: 0.4,
		KVCacheMemoryBytes:   12 << 30,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(spec.Engines) != 1 || spec.Engines[0].Command != "/opt/runtimes/vllm/bin/vllm" {
		t.Fatalf("engines = %+v, want intent runtime path", spec.Engines)
	}
	if spec.Engines[0].BenchCommand != "/opt/runtimes/vllm/bin/vllm" {
		t.Fatalf("bench command = %q, want the same runtime as serve", spec.Engines[0].BenchCommand)
	}
	for _, profile := range spec.Profiles {
		if profile.GPUMemoryUtilization != 0.4 {
			t.Fatalf("profile %s gpu mem = %v, want 0.4", profile.Name, profile.GPUMemoryUtilization)
		}
		found := false
		for index, arg := range profile.Args {
			if arg == "--kv-cache-memory-bytes" && index+1 < len(profile.Args) && profile.Args[index+1] == "12884901888" {
				found = true
			}
		}
		if !found {
			t.Fatalf("profile %s args = %v, want kv cache bytes pinned", profile.Name, profile.Args)
		}
	}
	if got := vllmbench.SpecProvenance(spec); got != vllmbench.SpecProvenanceGenerated {
		t.Fatalf("provenance = %q, want generated", got)
	}
}

func TestTrimWithoutReasonIsRejected(t *testing.T) {
	_, err := Plan(PlanRequest{
		Model:    "example/model",
		Contexts: []int{65536},
		Trims:    []vllmbench.LadderTrim{{Context: 65536, MaxConcurrency: 8}},
	})
	if err == nil {
		t.Fatal("expected a validation error for a trim without a reason")
	}
}
