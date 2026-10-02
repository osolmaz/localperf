package runner

import (
	"bufio"
	"context"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

// GPU telemetry sampling during measurement phases. Sources follow
// docs/2026-06-23-measurement-methods.md (tegrastats and nvidia-smi); all
// available sources run concurrently since each may cover fields the other
// cannot. Samples are emitted as run events and ingested into the telemetry
// tables tagged with the measurement; series names follow
// docs/2026-06-29-sqlite-run-artifact-format.md.
const gpuTelemetryInterval = 2 * time.Second

type gpuTelemetrySample struct {
	Source             string   `json:"source"`
	GPUUtilizationPct  *float64 `json:"gpu_utilization_percent,omitempty"`
	GPUMemoryUsedBytes *float64 `json:"gpu_memory_used_bytes,omitempty"`
}

type gpuTelemetrySampler struct {
	cancel context.CancelFunc
	done   chan struct{}
}

// startGPUTelemetrySampler samples GPU utilization and memory during one
// measurement, emitting gpu_telemetry events tagged with the planned run so
// artifact ingestion can attach them to the measurement. All available
// sources run concurrently because they cover different fields (on
// GB10-class machines tegrastats reports unified memory but no GR3D line,
// while nvidia-smi reports utilization but no memory); every series is
// tagged with its source so readers can judge each signal. Returns nil when
// no source is available; recording nothing is honest, substituting nothing
// is not.
func startGPUTelemetrySampler(ctx context.Context, events *eventWriter, planned PlannedRun) *gpuTelemetrySampler {
	sampleCtx, cancel := context.WithCancel(ctx)
	sampler := &gpuTelemetrySampler{cancel: cancel, done: make(chan struct{})}
	emit := func(sample gpuTelemetrySample) {
		events.Write(Event{
			Timestamp:   time.Now().UTC(),
			Type:        "gpu_telemetry",
			Profile:     planned.Profile.Name,
			Workload:    planned.Workload.Name,
			Concurrency: planned.Concurrency,
			Repeat:      planned.Repeat,
			Details:     mustJSON(sample),
		})
	}
	sources := []func(context.Context, func(gpuTelemetrySample)){}
	if _, err := exec.LookPath("tegrastats"); err == nil {
		sources = append(sources, sampleTegrastats)
	}
	if _, err := exec.LookPath("nvidia-smi"); err == nil {
		sources = append(sources, pollNvidiaSMI)
	}
	if len(sources) == 0 {
		cancel()
		close(sampler.done)
		return nil
	}
	var wait sync.WaitGroup
	for _, source := range sources {
		wait.Add(1)
		go func(run func(context.Context, func(gpuTelemetrySample))) {
			defer wait.Done()
			run(sampleCtx, emit)
		}(source)
	}
	go func() {
		wait.Wait()
		close(sampler.done)
	}()
	return sampler
}

func (sampler *gpuTelemetrySampler) Stop() {
	if sampler == nil {
		return
	}
	sampler.cancel()
	<-sampler.done
}

var (
	tegraGR3DPattern = regexp.MustCompile(`GR3D_FREQ (\d+)%`)
	tegraRAMPattern  = regexp.MustCompile(`RAM (\d+)/(\d+)MB`)
)

// sampleTegrastats streams `tegrastats --interval` output; each parseable
// line yields one sample.
func sampleTegrastats(ctx context.Context, emit func(gpuTelemetrySample)) {
	cmd := exec.CommandContext(ctx, "tegrastats", "--interval", strconv.Itoa(int(gpuTelemetryInterval.Milliseconds())))
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return
	}
	if err := cmd.Start(); err != nil {
		return
	}
	defer func() { _ = cmd.Wait() }()
	scanner := bufio.NewScanner(stdout)
	for scanner.Scan() {
		if sample, ok := parseTegrastatsLine(scanner.Text()); ok {
			emit(sample)
		}
	}
}

// parseTegrastatsLine extracts GPU utilization (GR3D) and, on unified-memory
// systems, the RAM line as the closest available GPU-memory signal, recorded
// under the tegrastats source so readers can judge it.
func parseTegrastatsLine(line string) (gpuTelemetrySample, bool) {
	sample := gpuTelemetrySample{Source: "tegrastats"}
	if match := tegraGR3DPattern.FindStringSubmatch(line); match != nil {
		if value, err := strconv.ParseFloat(match[1], 64); err == nil {
			sample.GPUUtilizationPct = &value
		}
	}
	if match := tegraRAMPattern.FindStringSubmatch(line); match != nil {
		if value, err := strconv.ParseFloat(match[1], 64); err == nil {
			bytes := value * 1024 * 1024
			sample.GPUMemoryUsedBytes = &bytes
		}
	}
	return sample, sample.GPUUtilizationPct != nil || sample.GPUMemoryUsedBytes != nil
}

func pollNvidiaSMI(ctx context.Context, emit func(gpuTelemetrySample)) {
	pollSamples(ctx, emit, queryNvidiaSMI)
}

// pollSamples emits one sample immediately and then one per interval until
// the context is canceled.
func pollSamples(ctx context.Context, emit func(gpuTelemetrySample), query func(context.Context) (gpuTelemetrySample, bool)) {
	ticker := time.NewTicker(gpuTelemetryInterval)
	defer ticker.Stop()
	for {
		if sample, ok := query(ctx); ok {
			emit(sample)
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func queryNvidiaSMI(ctx context.Context) (gpuTelemetrySample, bool) {
	queryCtx, cancel := context.WithTimeout(ctx, gpuTelemetryInterval)
	defer cancel()
	output, err := exec.CommandContext(queryCtx, "nvidia-smi",
		"--query-gpu=utilization.gpu,memory.used", "--format=csv,noheader,nounits").Output()
	if err != nil {
		return gpuTelemetrySample{}, false
	}
	return parseNvidiaSMISample(string(output))
}

// parseNvidiaSMISample parses "45, 1234" style query output for the first
// GPU; "[N/A]" fields are skipped. Multi-GPU inventories are in host_json
// and per-GPU series can be added when a multi-GPU machine needs them.
func parseNvidiaSMISample(output string) (gpuTelemetrySample, bool) {
	lines := strings.Split(strings.TrimSpace(output), "\n")
	fields := strings.Split(lines[0], ",")
	if len(fields) < 2 {
		return gpuTelemetrySample{}, false
	}
	sample := gpuTelemetrySample{Source: "nvidia-smi"}
	if value, err := strconv.ParseFloat(strings.TrimSpace(fields[0]), 64); err == nil {
		sample.GPUUtilizationPct = &value
	}
	if value, err := strconv.ParseFloat(strings.TrimSpace(fields[1]), 64); err == nil {
		bytes := value * 1024 * 1024
		sample.GPUMemoryUsedBytes = &bytes
	}
	return sample, sample.GPUUtilizationPct != nil || sample.GPUMemoryUsedBytes != nil
}
