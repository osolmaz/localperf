package runner

import (
	"bufio"
	"context"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

// HostInfo is the hardware inventory captured once at run start and stored
// in run.host_json. Every field is optional: absent hardware or tooling
// records what exists rather than failing the run. See
// docs/2026-06-29-sqlite-run-artifact-format.md, Column Conventions.
type HostInfo struct {
	Hostname         string    `json:"hostname,omitempty"`
	CPUModel         string    `json:"cpu,omitempty"`
	RAMGiB           float64   `json:"ram_gib,omitempty"`
	GPUs             []GPUInfo `json:"gpus,omitempty"`
	TelemetrySources []string  `json:"telemetry_sources,omitempty"`
}

type GPUInfo struct {
	Name    string  `json:"name"`
	VRAMGiB float64 `json:"vram_gib,omitempty"`
	Driver  string  `json:"driver,omitempty"`
}

const hostProbeTimeout = 5 * time.Second

// CollectHostInfo gathers CPU, RAM, GPU inventory, and which telemetry
// sources respond on this machine.
func CollectHostInfo(ctx context.Context) HostInfo {
	info := HostInfo{}
	info.Hostname, _ = os.Hostname()
	info.CPUModel = readCPUModel("/proc/cpuinfo")
	if snapshot, err := ReadMemorySnapshot(); err == nil {
		info.RAMGiB = snapshot.MemTotalGiB
	}
	info.GPUs = collectNvidiaGPUs(ctx)
	for _, tool := range []string{"tegrastats", "nvidia-smi"} {
		if _, err := exec.LookPath(tool); err == nil {
			info.TelemetrySources = append(info.TelemetrySources, tool)
		}
	}
	return info
}

func readCPUModel(path string) string {
	file, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer file.Close()
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		line := scanner.Text()
		for _, key := range []string{"model name", "Model"} {
			if strings.HasPrefix(line, key) {
				if _, value, found := strings.Cut(line, ":"); found {
					return strings.TrimSpace(value)
				}
			}
		}
	}
	return ""
}

func collectNvidiaGPUs(ctx context.Context) []GPUInfo {
	probeCtx, cancel := context.WithTimeout(ctx, hostProbeTimeout)
	defer cancel()
	output, err := exec.CommandContext(probeCtx, "nvidia-smi",
		"--query-gpu=name,memory.total,driver_version", "--format=csv,noheader").Output()
	if err != nil {
		return nil
	}
	return parseNvidiaGPUList(string(output))
}

// parseNvidiaGPUList parses csv,noheader nvidia-smi inventory lines such as
// "NVIDIA GB10, [N/A], 580.126.09".
func parseNvidiaGPUList(output string) []GPUInfo {
	var gpus []GPUInfo
	for _, line := range strings.Split(strings.TrimSpace(output), "\n") {
		fields := strings.Split(line, ",")
		if len(fields) < 3 {
			continue
		}
		gpu := GPUInfo{
			Name:   strings.TrimSpace(fields[0]),
			Driver: strings.TrimSpace(fields[2]),
		}
		gpu.VRAMGiB = parseMiBField(fields[1])
		if gpu.Name != "" {
			gpus = append(gpus, gpu)
		}
	}
	return gpus
}

// parseMiBField parses nvidia-smi memory fields such as "121850 MiB";
// "[N/A]" and unparseable values yield 0 (unknown).
func parseMiBField(field string) float64 {
	value := strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(field), "MiB"))
	parsed, err := strconv.ParseFloat(value, 64)
	if err != nil {
		return 0
	}
	return parsed / 1024
}
