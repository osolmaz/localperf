package runner

import (
	"context"
	"database/sql"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
)

// runFakeLlamaServer answers like llama-server: /health, /props with the
// slot count and per-slot context from the command line, and streamed chat
// completions. FAKE_LLAMA_SLOTS overrides the reported slots, and
// FAKE_LLAMA_BODIES records every request body.
func runFakeLlamaServer(args []string) {
	slots, _ := strconv.Atoi(flagValue(args, "--parallel"))
	ctxSize, _ := strconv.Atoi(flagValue(args, "--ctx-size"))
	if override, err := strconv.Atoi(os.Getenv("FAKE_LLAMA_SLOTS")); err == nil {
		slots = override
	}
	var calls atomic.Int64
	mux := http.NewServeMux()
	mux.HandleFunc("/health", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"status": "ok"})
	})
	mux.HandleFunc("/props", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"build_info":                  "b1-fake",
			"model_path":                  flagValue(args, "--model"),
			"total_slots":                 slots,
			"default_generation_settings": map[string]any{"n_ctx": ctxSize / max(slots, 1)},
		})
	})
	mux.HandleFunc("/v1/models", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"data": []any{map[string]any{"id": flagValue(args, "--alias")}}})
	})
	mux.HandleFunc("/v1/chat/completions", func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if path := os.Getenv("FAKE_LLAMA_BODIES"); path != "" {
			file, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
			if err == nil {
				_, _ = file.Write(append(body, '\n'))
				_ = file.Close()
			}
		}
		writeFakeSSEChatResponse(w, calls.Add(1), 64, 8, 72)
	})
	slotState := newFakeLlamaSlots()
	mux.HandleFunc("/v1/completions", slotState.complete(&calls))
	mux.HandleFunc("/slots/", slotState.action)
	serveFakeUntilSignal(flagValue(args, "--port"), mux)
}

func llamaCppTestSpec(t *testing.T, name string) Spec {
	t.Helper()
	spec := testSpec()
	spec.Name = name
	spec.OutputDir = t.TempDir()
	appendTimestamp := false
	spec.Runner.AppendTimestampToRun = &appendTimestamp
	spec.Env = map[string]string{"FAKE_LLAMA_BODIES": filepath.Join(spec.OutputDir, "bodies.jsonl")}
	spec.Engines = []EngineConfig{{Name: "llama.cpp", Type: EngineLlamaCppManaged, Command: fakeVLLMScript(t)}}
	spec.Safety.MinMemAvailableGiB = 0.1
	spec.Safety.StartupTimeoutSec = 10
	spec.Safety.WorkloadTimeoutSec = 10
	spec.Safety.HTTPTimeoutSec = 2
	spec.Warmup = WarmupConfig{Enabled: true}
	spec.Profiles = []Profile{{
		Name: "64k", Engine: "llama.cpp", Model: "fake-model", Port: freeTestPort(), Managed: true,
		HealthPath: "/health", MaxModelLen: 8192, MaxNumSeqs: 2,
		LlamaCpp: &LlamaCppSettings{ModelFile: "/models/fake.gguf", FlashAttn: "on", CacheTypeK: "q8_0", CacheTypeV: "q8_0"},
	}}
	workload := testRandomWorkload("generate", []string{"64k"}, 64, 8, 2, []int{2})
	workload.LoadGenerator = LoadGeneratorHTTP
	workload.ExtraBody = `{"cache_prompt":false}`
	spec.Workloads = []Workload{workload}
	ApplyDefaults(&spec)
	return spec
}

func TestExecuteManagedLlamaCppEndToEnd(t *testing.T) {
	spec := llamaCppTestSpec(t, "fake-llama-e2e")
	summary, err := Execute(context.Background(), spec, RunOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if summary.CompletedRuns != 1 || summary.FailedRuns != 0 {
		t.Fatalf("summary = %+v, want one completed run", summary)
	}
	events, err := os.ReadFile(summary.EventsPath)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		`--model /models/fake.gguf --alias fake-model`, `--parallel 2`, `--ctx-size 16384`, `--flash-attn on`,
		`"type":"server_limits"`, `"slots":2`, `"slot_context":8192`, `"type":"warmup_finish"`, `internal:http-load`,
	} {
		if !strings.Contains(string(events), want) {
			t.Fatalf("events lack %q:\n%s", want, events)
		}
	}
	bodies, err := os.ReadFile(spec.Env["FAKE_LLAMA_BODIES"])
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Count(string(bodies), `"cache_prompt":false`); got != 2 {
		t.Fatalf("measured requests without the prompt cache = %d, want 2:\n%s", got, bodies)
	}
	db, err := sql.Open("sqlite", summary.ArtifactPath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var managed int
	var version, serve string
	if err := db.QueryRow(`SELECT e.managed, COALESCE(e.version, ''), p.serve_json FROM engines e JOIN profiles p ON p.engine_id = e.id`).Scan(&managed, &version, &serve); err != nil {
		t.Fatal(err)
	}
	if managed != 1 || version != "b1-fake" || !strings.Contains(serve, `"model_file":"/models/fake.gguf"`) || !strings.Contains(serve, `"kv_cache_dtype":"q8_0"`) || strings.Contains(serve, "gpu_memory_utilization") {
		t.Fatalf("engine managed=%d version=%q serve=%s", managed, version, serve)
	}
}

func TestExecuteRefusesLlamaCppServerWithTooFewSlots(t *testing.T) {
	spec := llamaCppTestSpec(t, "fake-llama-slots")
	spec.Env["FAKE_LLAMA_SLOTS"] = "1"
	summary, err := Execute(context.Background(), spec, RunOptions{})
	if err == nil || summary.CompletedRuns != 0 {
		t.Fatalf("summary = %+v, err = %v; want the profile refused before measuring", summary, err)
	}
	events, readErr := os.ReadFile(summary.EventsPath)
	if readErr != nil {
		t.Fatal(readErr)
	}
	if !strings.Contains(string(events), "llama-server has 1 slots, but the suite sends 2 requests at once; start it with --parallel 2") {
		t.Fatalf("events lack the slot refusal:\n%s", events)
	}
}

func TestCheckLlamaCppLimits(t *testing.T) {
	profile := Profile{MaxModelLen: 65536, MaxNumSeqs: 6}
	for name, test := range map[string]struct {
		body string
		want string
	}{
		"fits":         {`{"build_info":"b1","total_slots":6,"default_generation_settings":{"n_ctx":65536}}`, ""},
		"small slots":  {`{"total_slots":6,"default_generation_settings":{"n_ctx":16384}}`, "start it with --ctx-size 393216"},
		"router":       {`{"role":"router"}`, "router-mode server"},
		"not json":     {`<html>`, "parse llama-server /props"},
		"extra slots":  {`{"total_slots":8,"default_generation_settings":{"n_ctx":131072}}`, ""},
		"too few both": {`{"total_slots":4,"default_generation_settings":{"n_ctx":1024}}`, "--parallel 6"},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := checkLlamaCppLimits([]byte(test.body), profile)
			if test.want == "" && err != nil {
				t.Fatalf("limits error = %v", err)
			}
			if test.want != "" && (err == nil || !strings.Contains(err.Error(), test.want)) {
				t.Fatalf("limits error = %v, want %q", err, test.want)
			}
		})
	}
}

func TestLlamaCppKVCacheLabel(t *testing.T) {
	for _, test := range []struct {
		k, v, want string
	}{
		{"", "", ""}, {"q8_0", "q8_0", "q8_0"}, {"q8_0", "", "q8_0/default"}, {"", "q4_0", "default/q4_0"}, {"f16", "q8_0", "f16/q8_0"},
	} {
		if got := llamaCppKVCacheLabel(LlamaCppSettings{CacheTypeK: test.k, CacheTypeV: test.v}); got != test.want {
			t.Fatalf("label(%q, %q) = %q, want %q", test.k, test.v, got, test.want)
		}
	}
}

func TestEngineTypesDecideFamilyAndManagement(t *testing.T) {
	for _, test := range []struct {
		engineType, family string
		managed            bool
		health             string
	}{
		{EngineLlamaCppManaged, EngineFamilyLlamaCpp, true, "/health"},
		{EngineLlamaCppEndpoint, EngineFamilyLlamaCpp, false, "/health"},
		{EngineVLLMManaged, EngineFamilyVLLM, true, DefaultHealthPath},
		{EngineVLLMEndpoint, EngineFamilyVLLM, false, DefaultHealthPath},
		{EngineOpenAIEndpoint, EngineFamilyOpenAI, false, DefaultHealthPath},
		{"llama.cpp", "", false, DefaultHealthPath},
	} {
		if EngineFamily(test.engineType) != test.family || ManagedEngineType(test.engineType) != test.managed || DefaultHealthPathFor(test.engineType) != test.health {
			t.Fatalf("%s = %q/%t/%q", test.engineType, EngineFamily(test.engineType), ManagedEngineType(test.engineType), DefaultHealthPathFor(test.engineType))
		}
	}
}

func TestValidateSpecTiesProfilesAndGeneratorsToEngines(t *testing.T) {
	for name, test := range map[string]struct {
		mutate func(*Spec)
		want   string
	}{
		"unknown engine":     {func(spec *Spec) { spec.Engines[0].Type = "custom" }, "type must be one of"},
		"managed mismatch":   {func(spec *Spec) { spec.Profiles[0].Managed = false }, "managed=false does not match engine type llama-cpp-managed"},
		"missing model file": {func(spec *Spec) { spec.Profiles[0].LlamaCpp = nil }, "llama_cpp.model_file is required"},
		"bad flash attn":     {func(spec *Spec) { spec.Profiles[0].LlamaCpp.FlashAttn = "yes" }, `llama_cpp.flash_attn must be "on", "off", or "auto"`},
		"negative layers":    {func(spec *Spec) { layers := -1; spec.Profiles[0].LlamaCpp.GPULayers = &layers }, "gpu_layers must not be negative"},
		"negative batch":     {func(spec *Spec) { spec.Profiles[0].LlamaCpp.BatchSize = -1 }, "batch_size, ubatch_size, and threads must not be negative"},
		"settings on vllm": {func(spec *Spec) {
			spec.Engines[0].Type = EngineVLLMManaged
		}, "llama_cpp settings require engine type llama-cpp-managed"},
		"vllm bench workload": {func(spec *Spec) { spec.Workloads[0].LoadGenerator = LoadGeneratorVLLMBench }, "workload generate: load_generator vllm_bench requires a vLLM engine"},
		"vllm bench warmup":   {func(spec *Spec) { spec.Warmup.LoadGenerator = LoadGeneratorVLLMBench }, "warmup: load_generator vllm_bench requires a vLLM engine"},
	} {
		t.Run(name, func(t *testing.T) {
			spec := llamaCppTestSpec(t, "validate")
			settings := *spec.Profiles[0].LlamaCpp
			spec.Profiles[0].LlamaCpp = &settings
			test.mutate(&spec)
			if err := ValidateSpec(spec); err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("ValidateSpec = %v, want %q", err, test.want)
			}
		})
	}
}

func serveFakeUntilSignal(port string, handler http.Handler) {
	server := &http.Server{Addr: "127.0.0.1:" + port, Handler: handler}
	signals := make(chan os.Signal, 1)
	signal.Notify(signals, syscall.SIGTERM, syscall.SIGINT)
	go func() {
		<-signals
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_ = server.Shutdown(ctx)
	}()
	if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		os.Exit(1)
	}
	os.Exit(0)
}
