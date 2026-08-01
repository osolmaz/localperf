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
