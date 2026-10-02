package runner

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestGPUTelemetrySamplerWithFakeTools covers the full sampler lifecycle
// (source detection, concurrent sampling, event emission, stop) using fake
// tegrastats and nvidia-smi binaries on PATH, so coverage does not depend on
// GPU tooling being installed on the test machine.
func TestGPUTelemetrySamplerWithFakeTools(t *testing.T) {
	binDir := t.TempDir()
	writeFakeTool(t, binDir, "tegrastats", "#!/bin/sh\nwhile true; do echo 'RAM 100/200MB GR3D_FREQ 10%'; sleep 0.05; done\n")
	writeFakeTool(t, binDir, "nvidia-smi", "#!/bin/sh\necho '50, 100'\n")
	t.Setenv("PATH", binDir+":"+os.Getenv("PATH"))

	eventsPath := filepath.Join(t.TempDir(), "events.jsonl")
	events, err := newEventWriter(eventsPath)
	if err != nil {
		t.Fatal(err)
	}
	planned := PlannedRun{Profile: Profile{Name: "p"}, Workload: Workload{Name: "w"}, Concurrency: 2}
	sampler := startGPUTelemetrySampler(context.Background(), events, planned)
	if sampler == nil {
		t.Fatal("sampler = nil, want sampler with fake tools on PATH")
	}
	deadline := time.Now().Add(5 * time.Second)
	seen := map[string]bool{}
	for time.Now().Before(deadline) && (!seen["tegrastats"] || !seen["nvidia-smi"]) {
		time.Sleep(50 * time.Millisecond)
		data, err := os.ReadFile(eventsPath)
		if err != nil {
			continue
		}
		for _, line := range strings.Split(string(data), "\n") {
			var event Event
			if line == "" || json.Unmarshal([]byte(line), &event) != nil {
				continue
			}
			if sample, ok := parseGPUTelemetryEvent(event); ok {
				seen[sample.Source] = true
			}
		}
	}
	sampler.Stop()
	if !seen["tegrastats"] || !seen["nvidia-smi"] {
		t.Fatalf("sources seen = %v, want samples from both fake tools", seen)
	}
}

func TestStartGPUTelemetrySamplerWithoutTools(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	if sampler := startGPUTelemetrySampler(context.Background(), nil, PlannedRun{}); sampler != nil {
		t.Fatal("sampler != nil with no telemetry tools on PATH")
	}
	// Stop on a nil sampler is a no-op.
	var sampler *gpuTelemetrySampler
	sampler.Stop()
}

func writeFakeTool(t *testing.T, dir, name, script string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
}

func TestParseTegrastatsLine(t *testing.T) {
	line := "11-30-2026 12:00:00 RAM 41234/119896MB (lfb 8x4MB) SWAP 0/0MB CPU [1%@2000] GR3D_FREQ 47% VIC_FREQ 0% APE 200"
	sample, ok := parseTegrastatsLine(line)
	if !ok || sample.Source != "tegrastats" {
		t.Fatalf("sample = %+v ok=%t, want tegrastats sample", sample, ok)
	}
	if sample.GPUUtilizationPct == nil || *sample.GPUUtilizationPct != 47 {
		t.Fatalf("utilization = %v, want 47", sample.GPUUtilizationPct)
	}
	if sample.GPUMemoryUsedBytes == nil || *sample.GPUMemoryUsedBytes != 41234*1024*1024 {
		t.Fatalf("memory = %v, want 41234 MiB in bytes", sample.GPUMemoryUsedBytes)
	}
	// GB10-class output has RAM but no GR3D line.
	sample, ok = parseTegrastatsLine("RAM 38121/124547MB SWAP 3453/16384MB CPU [6%@2808]")
	if !ok || sample.GPUUtilizationPct != nil || sample.GPUMemoryUsedBytes == nil {
		t.Fatalf("sample = %+v ok=%t, want memory-only sample", sample, ok)
	}
	if _, ok := parseTegrastatsLine("no telemetry here"); ok {
		t.Fatal("unparseable line yielded a sample")
	}
}

func TestParseNvidiaSMISample(t *testing.T) {
	sample, ok := parseNvidiaSMISample("95, 1234\n")
	if !ok || sample.Source != "nvidia-smi" {
		t.Fatalf("sample = %+v ok=%t, want nvidia-smi sample", sample, ok)
	}
	if sample.GPUUtilizationPct == nil || *sample.GPUUtilizationPct != 95 {
		t.Fatalf("utilization = %v, want 95", sample.GPUUtilizationPct)
	}
	if sample.GPUMemoryUsedBytes == nil || *sample.GPUMemoryUsedBytes != 1234*1024*1024 {
		t.Fatalf("memory = %v, want 1234 MiB in bytes", sample.GPUMemoryUsedBytes)
	}
	// Unified-memory machines report utilization with memory N/A.
	sample, ok = parseNvidiaSMISample("95, [N/A]")
	if !ok || sample.GPUUtilizationPct == nil || sample.GPUMemoryUsedBytes != nil {
		t.Fatalf("sample = %+v ok=%t, want utilization-only sample", sample, ok)
	}
	if _, ok := parseNvidiaSMISample("[N/A], [N/A]"); ok {
		t.Fatal("all-N/A output yielded a sample")
	}
	if _, ok := parseNvidiaSMISample("garbage"); ok {
		t.Fatal("malformed output yielded a sample")
	}
}

func TestPollSamplesEmitsImmediatelyAndStopsOnCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	emitted := make(chan gpuTelemetrySample, 1)
	util := 12.0
	done := make(chan struct{})
	go func() {
		defer close(done)
		pollSamples(ctx, func(sample gpuTelemetrySample) { emitted <- sample }, func(context.Context) (gpuTelemetrySample, bool) {
			return gpuTelemetrySample{Source: "fake", GPUUtilizationPct: &util}, true
		})
	}()
	sample := <-emitted
	if sample.Source != "fake" || *sample.GPUUtilizationPct != 12 {
		t.Fatalf("sample = %+v, want immediate fake sample", sample)
	}
	cancel()
	<-done
}
