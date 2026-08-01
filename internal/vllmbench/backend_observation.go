package vllmbench

import (
	"bufio"
	"os"
	"strings"
)

type backendObservation struct {
	Requested map[string]string `json:"requested"`
	Observed  map[string]string `json:"observed,omitempty"`
	Evidence  []string          `json:"evidence,omitempty"`
}

func observeBackends(profile Profile, logPath string) (backendObservation, bool) {
	observation := backendObservation{
		Requested: map[string]string{
			"attention": strings.TrimSpace(profile.AttentionBackend),
			"moe":       strings.TrimSpace(profile.MoEBackend),
			"kv_cache":  strings.TrimSpace(profile.KVCacheDType),
		},
		Observed: map[string]string{},
	}
	file, err := os.Open(logPath)
	if err != nil {
		return observation, false
	}
	defer file.Close()
	scanner := bufio.NewScanner(file)
	buffer := make([]byte, 64*1024)
	scanner.Buffer(buffer, 1024*1024)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		lower := strings.ToLower(line)
		kind, value := observedBackendLine(lower)
		if kind == "" || observation.Observed[kind] != "" {
			continue
		}
		observation.Observed[kind] = value
		observation.Evidence = append(observation.Evidence, line)
	}
	return observation, len(observation.Observed) > 0
}

func observedBackendLine(line string) (string, string) {
	if strings.Contains(line, "attention") && backendSelectionLine(line) {
		if value := knownBackend(line, attentionBackendNames); value != "" {
			return "attention", value
		}
	}
	if strings.Contains(line, "moe") && backendSelectionLine(line) {
		if value := knownBackend(line, moeBackendNames); value != "" {
			return "moe", value
		}
	}
	if strings.Contains(line, "kv cache dtype") || strings.Contains(line, "kv_cache_dtype") {
		if value := knownBackend(line, kvCacheDTypeNames); value != "" {
			return "kv_cache", value
		}
	}
	return "", ""
}

func backendSelectionLine(line string) bool {
	return strings.Contains(line, "using ") || strings.Contains(line, "selected") ||
		strings.Contains(line, "backend is") || strings.Contains(line, "backend:")
}

type backendName struct {
	Needle string
	Name   string
}

var attentionBackendNames = []backendName{
	{"flashinfer", "flashinfer"},
	{"flash_attn", "flash_attn"},
	{"flash attention", "flash_attn"},
	{"xformers", "xformers"},
	{"triton", "triton"},
	{"torch_sdpa", "torch_sdpa"},
	{"flashmla", "flashmla"},
}

var moeBackendNames = []backendName{
	{"flashinfer_cutlass", "flashinfer_cutlass"},
	{"flashinfer_trtllm", "flashinfer_trtllm"},
	{"deep_gemm", "deep_gemm"},
	{"deepgemm", "deep_gemm"},
	{"triton", "triton"},
	{"cutlass", "cutlass"},
	{"marlin", "marlin"},
	{"pplx", "pplx"},
}

var kvCacheDTypeNames = []backendName{
	{"fp8_e4m3", "fp8_e4m3"},
	{"fp8_e5m2", "fp8_e5m2"},
	{"bfloat16", "bfloat16"},
	{"float16", "float16"},
	{"fp8", "fp8"},
}

func knownBackend(line string, names []backendName) string {
	for _, candidate := range names {
		if strings.Contains(line, candidate.Needle) {
			return candidate.Name
		}
	}
	return ""
}
