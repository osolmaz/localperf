package vllmbench

import (
	"os"
	"path/filepath"
	"testing"
)

func TestObserveBackendsRecordsRequestedAndObservedFallback(t *testing.T) {
	path := filepath.Join(t.TempDir(), "server.log")
	data := []byte("INFO Using FLASH_ATTN attention backend\nINFO Using triton backend for MoE\nINFO KV cache dtype: fp8_e4m3\n")
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
	if _, err := file.WriteString("INFO request complete\nINFO selected triton backend for MoE\n"); err != nil {
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
