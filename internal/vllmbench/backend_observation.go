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
	Source        string            `json:"source"`
}

func observeBackends(profile Profile, logPath string) (backendObservation, bool) {
	return observeBackendsSince(profile, logPath, 0, "test request")
}

// observeBackendsSince reads only the CUDA execution table emitted by the
// request-scoped torch profiler. Startup selections and ordinary log messages
// are deliberately excluded: loading a backend is not execution evidence.
func observeBackendsSince(profile Profile, logPath string, offset int64, request string) (backendObservation, bool) {
	observation := backendObservation{
		Requested: map[string]string{
			"attention": strings.TrimSpace(profile.AttentionBackend),
			"moe":       strings.TrimSpace(profile.MoEBackend),
			"kv_cache":  strings.TrimSpace(profile.KVCacheDType),
		},
		Observed:      map[string]string{},
		AttestedAfter: request,
		Source:        "torch_profiler_cuda_table",
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
	completeCUDATable := scanProfilerTable(scanner, profile, &observation)
	return observation, completeCUDATable && len(observation.Observed) > 0
}

func scanProfilerTable(scanner *bufio.Scanner, profile Profile, observation *backendObservation) bool {
	inCUDATable := false
	completeCUDATable := false
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		lower := strings.ToLower(line)
		if profilerTableHeader(lower) {
			inCUDATable = true
			continue
		}
		if inCUDATable && profilerTableFooter(lower) {
			inCUDATable = false
			completeCUDATable = true
			continue
		}
		if !inCUDATable {
			continue
		}
		kind, value := profilerBackendLine(profile, lower)
		if kind == "" || observation.Observed[kind] != "" {
			continue
		}
		observation.Observed[kind] = value
		observation.Evidence = append(observation.Evidence, line)
	}
	return completeCUDATable
}

func logFileOffset(path string) int64 {
	info, err := os.Stat(path)
	if err != nil {
		return 0
	}
	return info.Size()
}

func validateBackendObservation(observation backendObservation) error {
	var issues []string
	for _, kind := range []string{"attention", "moe"} {
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

func requiresBackendAttestation(profile Profile) bool {
	for _, requested := range []string{profile.AttentionBackend, profile.MoEBackend} {
		normalized := normalizeBackendName(requested)
		if normalized != "" && normalized != "auto" {
			return true
		}
	}
	return false
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

func profilerTableHeader(line string) bool {
	return strings.Contains(line, "name") && (strings.Contains(line, "self cuda") || strings.Contains(line, "self gpu"))
}

func profilerTableFooter(line string) bool {
	return strings.Contains(line, "self cuda time total") || strings.Contains(line, "self gpu time total")
}

func profilerBackendLine(profile Profile, line string) (string, string) {
	requestedAttention := normalizeBackendName(profile.AttentionBackend)
	if profilerLineMatchesBackend("attention", requestedAttention, line) {
		return "attention", requestedAttention
	}
	if value := knownProfilerBackend("attention", line, attentionBackendNames); value != "" {
		return "attention", value
	}
	requestedMoE := normalizeBackendName(profile.MoEBackend)
	if profilerLineMatchesBackend("moe", requestedMoE, line) {
		return "moe", requestedMoE
	}
	if value := knownProfilerBackend("moe", line, moeBackendNames); value != "" {
		return "moe", value
	}
	if strings.Contains(line, "kv") || strings.Contains(line, "cache") {
		if value := knownBackend(line, kvCacheDTypeNames); value != "" {
			return "kv_cache", value
		}
	}
	return "", ""
}

func knownProfilerBackend(kind, line string, names []backendName) string {
	for _, candidate := range names {
		if profilerLineMatchesBackend(kind, candidate.Name, line) {
			return candidate.Name
		}
	}
	return ""
}

func profilerLineMatchesBackend(kind, backend, line string) bool {
	if backend == "" || backend == "auto" {
		return false
	}
	for _, signature := range profilerBackendSignatures {
		if signature.Kind == kind && signature.Name == backend {
			return signature.matches(line)
		}
	}
	return strings.Contains(line, backend)
}

func containsAny(value string, needles ...string) bool {
	for _, needle := range needles {
		if strings.Contains(value, needle) {
			return true
		}
	}
	return false
}

type backendName struct {
	Needle string
	Name   string
}

type profilerBackendSignature struct {
	Kind string
	Name string
	All  []string
	Any  []string
	None []string
}

func (signature profilerBackendSignature) matches(line string) bool {
	if !containsAll(line, signature.All...) || containsAny(line, signature.None...) {
		return false
	}
	return len(signature.Any) == 0 || containsAny(line, signature.Any...)
}

func containsAll(value string, needles ...string) bool {
	for _, needle := range needles {
		if !strings.Contains(value, needle) {
			return false
		}
	}
	return true
}

var profilerBackendSignatures = []profilerBackendSignature{
	{Kind: "attention", Name: "flashinfer", All: []string{"flashinfer"}, None: []string{"moe"}},
	{Kind: "attention", Name: "flash_attn", Any: []string{"flash_attn", "flash attention", "flash_fwd", "flash::"}},
	{Kind: "attention", Name: "xformers", All: []string{"xformers"}},
	{Kind: "attention", Name: "triton", All: []string{"triton"}, Any: []string{"attention", "attn", "paged"}},
	{Kind: "attention", Name: "triton_attn", All: []string{"triton"}, Any: []string{"attention", "attn", "paged"}},
	{Kind: "attention", Name: "torch_sdpa", Any: []string{"torch_sdpa", "scaled_dot_product"}},
	{Kind: "attention", Name: "flashmla", All: []string{"flashmla"}},
	{Kind: "moe", Name: "flashinfer_cutlass", All: []string{"flashinfer", "cutlass"}},
	{Kind: "moe", Name: "flashinfer_trtllm", All: []string{"flashinfer", "trtllm"}},
	{Kind: "moe", Name: "deep_gemm", Any: []string{"deep_gemm", "deepgemm"}},
	{Kind: "moe", Name: "triton", All: []string{"triton", "moe"}},
	{Kind: "moe", Name: "cutlass", All: []string{"cutlass"}},
	{Kind: "moe", Name: "marlin", All: []string{"marlin"}},
	{Kind: "moe", Name: "pplx", All: []string{"pplx"}},
}

var attentionBackendNames = []backendName{
	{"flashinfer", "flashinfer"},
	{"flash_attn", "flash_attn"},
	{"flash attention", "flash_attn"},
	{"xformers", "xformers"},
	{"triton", "triton_attn"},
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
