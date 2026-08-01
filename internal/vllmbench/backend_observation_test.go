package vllmbench

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestObserveBackendsRecordsRequestedAndObservedFallback(t *testing.T) {
	path := filepath.Join(t.TempDir(), "server.log")
	data := []byte(`Name                                      Self CPU %      Self CPU   CPU total %     CPU total  Self CUDA   Self CUDA %
void flash_fwd_kernel                     1.0%            1us        1.0%            1us        10us        25%
triton_fused_moe_kernel                   1.0%            1us        1.0%            1us        30us        75%
Self CUDA time total: 40us
`)
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
	observation, ok := observeBackends(Profile{
		AttentionBackend: "flashinfer", MoEBackend: "auto", KVCacheDType: "fp8",
	}, path)
	if !ok {
		t.Fatal("backend observation was not detected")
	}
	if observation.Requested["attention"] != "flashinfer" || observation.Observed["attention"] != "flash_attn" {
		t.Fatalf("attention observation = %+v", observation)
	}
	if observation.Observed["moe"] != "triton" {
		t.Fatalf("backend observation = %+v", observation)
	}
}

func TestWaitBackendObservationReadsDelayedProfilerOutput(t *testing.T) {
	path := filepath.Join(t.TempDir(), "server.log")
	if err := os.WriteFile(path, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	written := make(chan error, 1)
	go func() {
		time.Sleep(50 * time.Millisecond)
		written <- os.WriteFile(path, []byte("Name Self CPU Self CUDA\nvoid flash_fwd_kernel 1us 10us\nSelf CUDA time total: 10us\n"), 0o644)
	}()
	observation := waitBackendObservation(context.Background(), Profile{AttentionBackend: "flash_attn"}, path, 0, "delayed canary", time.Second)
	if err := <-written; err != nil {
		t.Fatal(err)
	}
	if observation.Observed["attention"] != "flash_attn" {
		t.Fatalf("delayed observation = %+v", observation)
	}
}

func TestObserveBackendsIgnoresLogsOutsideProfilerTable(t *testing.T) {
	path := filepath.Join(t.TempDir(), "server.log")
	data := []byte("args: --attention-backend flashinfer\nINFO executed flashinfer attention kernel\n")
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
	observation, ok := observeBackends(Profile{AttentionBackend: "flashinfer"}, path)
	if ok || observation.Observed["attention"] != "" {
		t.Fatalf("ordinary logs reported as profiler evidence: %+v", observation)
	}
}

func TestObserveBackendsRequiresCompleteProfilerTable(t *testing.T) {
	path := filepath.Join(t.TempDir(), "server.log")
	if err := os.WriteFile(path, []byte("Name Self CPU Self CUDA\nvoid flash_fwd_kernel 1us 10us\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if observation, ok := observeBackends(Profile{AttentionBackend: "flash_attn"}, path); ok || observation.Observed["attention"] != "flash_attn" {
		t.Fatalf("incomplete table result = %+v ok=%t", observation, ok)
	}
	if observation, ok := observeBackends(Profile{AttentionBackend: "flash_attn"}, filepath.Join(t.TempDir(), "missing.log")); ok || len(observation.Observed) != 0 {
		t.Fatalf("missing log result = %+v ok=%t", observation, ok)
	}
}

func TestObserveBackendsSinceExcludesStartupSelection(t *testing.T) {
	path := filepath.Join(t.TempDir(), "server.log")
	startup := []byte("INFO Using FLASH_ATTN attention backend\n")
	if err := os.WriteFile(path, startup, 0o644); err != nil {
		t.Fatal(err)
	}
	file, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := file.WriteString("Name Self CPU Self CUDA\ntriton_fused_moe_kernel 1us 10us\nSelf CUDA time total: 10us\n"); err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	observation, ok := observeBackendsSince(Profile{AttentionBackend: "flash_attn", MoEBackend: "triton"}, path, int64(len(startup)), "decode-4k c1 repeat 1")
	if !ok {
		t.Fatal("request-time backend observation was not detected")
	}
	if observation.Observed["attention"] != "" || observation.Observed["moe"] != "triton" {
		t.Fatalf("observation included startup evidence: %+v", observation)
	}
	if observation.AttestedAfter != "decode-4k c1 repeat 1" {
		t.Fatalf("attested after = %q", observation.AttestedAfter)
	}
}

func TestValidateBackendObservationRejectsMissingAndFallbackExecution(t *testing.T) {
	for name, observation := range map[string]backendObservation{
		"missing": {
			Requested: map[string]string{"attention": "flashinfer"},
			Observed:  map[string]string{},
		},
		"fallback": {
			Requested: map[string]string{"attention": "flashinfer"},
			Observed:  map[string]string{"attention": "flash_attn"},
		},
	} {
		t.Run(name, func(t *testing.T) {
			if err := validateBackendObservation(observation); err == nil {
				t.Fatal("backend attestation error = nil")
			}
		})
	}
}

func TestValidateBackendObservationAcceptsAutoAndMatchingEvidence(t *testing.T) {
	observation := backendObservation{
		Requested: map[string]string{"attention": "auto", "moe": "cutlass", "kv_cache": "fp8"},
		Observed:  map[string]string{"attention": "flash_attn", "moe": "cutlass"},
	}
	if err := validateBackendObservation(observation); err != nil {
		t.Fatalf("backend attestation error = %v", err)
	}
}

func TestManagedAutoBackendRequiresObservation(t *testing.T) {
	profile := Profile{Managed: true, AttentionBackend: "auto", MoEBackend: "auto"}
	if !requiresBackendAttestation(profile) {
		t.Fatal("managed auto backend did not require an observation canary")
	}
	if requiresBackendAttestation(Profile{Managed: false, AttentionBackend: "auto"}) {
		t.Fatal("external auto backend incorrectly required inaccessible server evidence")
	}
	if err := validateBackendObservation(backendObservation{Requested: map[string]string{"attention": "auto"}, Observed: map[string]string{}}); err == nil {
		t.Fatal("empty auto backend observation was accepted")
	}
}

func TestProfilerBackendSignatures(t *testing.T) {
	for _, signature := range profilerBackendSignatures {
		line := strings.Join(append(append([]string{}, signature.All...), signature.Any...), " ")
		if len(signature.Any) > 1 {
			line = strings.Join(signature.All, " ") + " " + signature.Any[0]
		}
		if !profilerLineMatchesBackend(signature.Kind, signature.Name, line) {
			t.Errorf("signature %s/%s did not match %q", signature.Kind, signature.Name, line)
		}
	}
	if profilerLineMatchesBackend("attention", "auto", "flashinfer") {
		t.Fatal("auto backend produced execution evidence")
	}
	if profilerLineMatchesBackend("attention", "custom_backend", "unrelated") {
		t.Fatal("unrelated custom backend matched")
	}
}

func TestProfilerBackendLineFindsRequestedAndKVBackends(t *testing.T) {
	profile := Profile{AttentionBackend: "flashinfer", MoEBackend: "cutlass", KVCacheDType: "fp8"}
	for line, wantKind := range map[string]string{
		"flashinfer batchprefillwithpagedkvcachekernel 1us": "attention",
		"cutlass gemm kernel 1us":                           "moe",
		"kv cache fp8_e4m3 kernel 1us":                      "kv_cache",
	} {
		kind, value := profilerBackendLine(profile, line)
		if kind != wantKind || value == "" {
			t.Errorf("profiler line %q = %q/%q, want %q", line, kind, value, wantKind)
		}
	}
	if kind, value := profilerBackendLine(Profile{}, "unrelated kernel"); kind != "" || value != "" {
		t.Fatalf("unrelated profiler line = %q/%q", kind, value)
	}
	compound := Profile{AttentionBackend: "flashinfer", MoEBackend: "flashinfer_cutlass"}
	if kind, value := profilerBackendLine(compound, "flashinfer cutlass fused kernel"); kind != "moe" || value != "flashinfer_cutlass" {
		t.Fatalf("compound MoE profiler line = %q/%q", kind, value)
	}
}
