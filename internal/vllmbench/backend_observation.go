package vllmbench

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
)

type backendObservation struct {
	Requested     map[string]string `json:"requested"`
	Observed      map[string]string `json:"observed,omitempty"`
	Evidence      []string          `json:"evidence,omitempty"`
	AttestedAfter string            `json:"attested_after_request"`
}

func observeBackends(profile Profile, logPath string) (backendObservation, bool) {
	return observeBackendsSince(profile, logPath, 0, "test request")
}

// observeBackendsSince reads only server output produced while handling a
// validated generation request. Startup selections are deliberately excluded:
// loading a backend is not evidence that its kernels executed for the model.
func observeBackendsSince(profile Profile, logPath string, offset int64, request string) (backendObservation, bool) {
	observation := backendObservation{
		Requested: map[string]string{
			"attention": strings.TrimSpace(profile.AttentionBackend),
			"moe":       strings.TrimSpace(profile.MoEBackend),
			"kv_cache":  strings.TrimSpace(profile.KVCacheDType),
		},
		Observed:      map[string]string{},
		AttestedAfter: request,
	}
	file, err := os.Open(logPath)
	if err != nil {
		return observation, false
	}
	defer file.Close()
	if _, err := file.Seek(offset, io.SeekStart); err != nil {
		return observation, false
	}
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

func serverLogOffset(proc *serverProcess) int64 {
	if proc == nil || strings.TrimSpace(proc.logPath) == "" {
		return 0
	}
	info, err := os.Stat(proc.logPath)
	if err != nil {
		return 0
	}
	return info.Size()
}

func backendRequestLabel(planned PlannedRun) string {
	return fmt.Sprintf("%s c%d repeat %d", planned.Workload.Name, planned.Concurrency, planned.Repeat)
}

func validateBackendObservation(observation backendObservation) error {
	var issues []string
	for _, kind := range []string{"attention", "moe", "kv_cache"} {
		requested := normalizeBackendName(observation.Requested[kind])
		if requested == "" || requested == "auto" {
			continue
		}
		observed := normalizeBackendName(observation.Observed[kind])
		if observed == "" {
			issues = append(issues, fmt.Sprintf("requested %s backend %q was not observed executing", kind, requested))
			continue
		}
		if !backendNamesMatch(requested, observed) {
			issues = append(issues, fmt.Sprintf("requested %s backend %q but observed %q executing", kind, requested, observed))
		}
	}
	if len(issues) > 0 {
		return errors.New(strings.Join(issues, "; "))
	}
	return nil
}

func normalizeBackendName(value string) string {
	return strings.NewReplacer("-", "_", " ", "_").Replace(strings.ToLower(strings.TrimSpace(value)))
}

func backendNamesMatch(requested, observed string) bool {
	if requested == observed {
		return true
	}
	// fp8 lets the runtime choose the supported concrete fp8 encoding.
	return requested == "fp8" && strings.HasPrefix(observed, "fp8_")
}

func observedBackendLine(line string) (string, string) {
	if strings.Contains(line, "attention") && backendExecutionLine(line) {
		if value := knownBackend(line, attentionBackendNames); value != "" {
			return "attention", value
		}
	}
	if strings.Contains(line, "moe") && backendExecutionLine(line) {
		if value := knownBackend(line, moeBackendNames); value != "" {
			return "moe", value
		}
	}
	if (strings.Contains(line, "kv cache dtype") || strings.Contains(line, "kv_cache_dtype")) && kvCacheExecutionLine(line) {
		if value := knownBackend(line, kvCacheDTypeNames); value != "" {
			return "kv_cache", value
		}
	}
	return "", ""
}

func backendExecutionLine(line string) bool {
	if !strings.Contains(line, "kernel") {
		return false
	}
	for _, marker := range []string{"dispatched ", "dispatching ", "executed ", "executing ", "launched ", "launching ", "ran ", "running "} {
		if strings.Contains(line, marker) {
			return true
		}
	}
	return false
}

func kvCacheExecutionLine(line string) bool {
	for _, marker := range []string{"allocated ", "allocating ", "cache block"} {
		if strings.Contains(line, marker) {
			return true
		}
	}
	return false
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
