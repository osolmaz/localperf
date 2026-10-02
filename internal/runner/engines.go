package runner

import "strings"

// Engine types. The type says which runtime serves the model and whether
// localperf starts and stops it; llama.cpp is the primary runtime.
const (
	EngineLlamaCppManaged  = "llama-cpp-managed"
	EngineLlamaCppEndpoint = "llama-cpp-endpoint"
	EngineVLLMManaged      = "vllm-managed"
	EngineVLLMEndpoint     = "vllm-endpoint"
	EngineOpenAIEndpoint   = "openai-endpoint"
)

// Engine families group the managed and endpoint forms of one runtime.
const (
	EngineFamilyLlamaCpp = "llama-cpp"
	EngineFamilyVLLM     = "vllm"
	EngineFamilyOpenAI   = "openai"
)

// EngineTypes lists every supported engine type, primary runtime first.
func EngineTypes() []string {
	return []string{EngineLlamaCppManaged, EngineLlamaCppEndpoint, EngineVLLMManaged, EngineVLLMEndpoint, EngineOpenAIEndpoint}
}

func KnownEngineType(engineType string) bool {
	for _, known := range EngineTypes() {
		if engineType == known {
			return true
		}
	}
	return false
}

// EngineFamily returns the runtime family of an engine type, or "" when the
// type is unknown.
func EngineFamily(engineType string) string {
	if !KnownEngineType(engineType) {
		return ""
	}
	return strings.TrimSuffix(strings.TrimSuffix(engineType, "-managed"), "-endpoint")
}

// ManagedEngineType reports whether localperf starts and stops the server.
func ManagedEngineType(engineType string) bool {
	return KnownEngineType(engineType) && strings.HasSuffix(engineType, "-managed")
}

// DefaultHealthPathFor is the readiness path of each family: llama-server
// answers /health with 503 while the model loads and 200 once it serves.
func DefaultHealthPathFor(engineType string) string {
	if EngineFamily(engineType) == EngineFamilyLlamaCpp {
		return "/health"
	}
	return DefaultHealthPath
}

func profileEngineFamily(spec Spec, profile Profile) string {
	return EngineFamily(EngineForProfile(spec, profile).Type)
}
