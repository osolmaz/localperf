package vllmbench

import (
	"os"
	"path/filepath"
	"testing"
)

func TestObserveBackendsRecordsRequestedAndObservedFallback(t *testing.T) {
	path := filepath.Join(t.TempDir(), "server.log")
	data := []byte("INFO executed FLASH_ATTN attention kernel\nINFO dispatched triton kernel for MoE\nINFO allocated KV cache dtype fp8_e4m3 for request\n")
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
	if observation.Observed["moe"] != "triton" || observation.Observed["kv_cache"] != "fp8_e4m3" {
		t.Fatalf("backend observation = %+v", observation)
	}
}

func TestObservedBackendLineIgnoresConfigurationEcho(t *testing.T) {
	if kind, value := observedBackendLine("args: --attention-backend flashinfer"); kind != "" || value != "" {
		t.Fatalf("configuration echo reported as observation: %q %q", kind, value)
	}
}

func TestObservedBackendLineIgnoresSelectionWithoutKernelEvidence(t *testing.T) {
	for _, line := range []string{
		"using flash_attn attention backend",
		"request complete; selected triton backend for moe",
		"kv cache dtype: fp8_e4m3",
	} {
		if kind, value := observedBackendLine(line); kind != "" || value != "" {
			t.Fatalf("selection reported as execution evidence: %q => %q %q", line, kind, value)
		}
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
	if _, err := file.WriteString("INFO dispatched triton MoE kernel while serving request\nINFO request complete\n"); err != nil {
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
