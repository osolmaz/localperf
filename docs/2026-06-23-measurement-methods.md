---
title: Measurement Methods
author: Bob <dutifulbob@gmail.com>
date: 2026-06-23
---

# Measurement Methods

localperf should not collapse local inference resource use into one generic
`memory_usage` number. On unified-memory machines, especially GB10/DGX Spark
style systems, different tools can report different slices of the same run.

Record the signals separately and label them plainly.

## Required Signals

| Signal | Source | What it means | Use for |
| --- | --- | --- | --- |
| System memory pressure | `/proc/meminfo` `MemAvailable` | Whole-machine memory lost while the model is loaded or serving | Best practical proxy for total unified-memory pressure |
| Process/cgroup memory | `systemctl show ... MemoryCurrent MemoryPeak` or cgroup files | Memory charged to the server process by Linux | Process accounting, not total model footprint |
| vLLM capacity | vLLM logs | KV cache memory, KV token capacity, and max concurrency reported by vLLM | Fit/capacity planning |
| NVIDIA unified-memory telemetry | `tegrastats` when available | NVIDIA platform view of RAM, swap, GPU activity, thermals, and power on Tegra/Jetson/GB10-style systems | Preferred NVIDIA cross-check for unified-memory systems |
| GPU telemetry | `nvtop`, NVML, DCGM, vendor tools | GPU utilization and memory counters when available | Cross-checking and live debugging |
| Request performance | OpenAI-compatible request timings and usage | latency, output token/s, total token/s, failures | Throughput and latency characterization |

## System Memory Pressure

This should be the primary "how much did the machine lose?" memory number.

Record:

- `MemAvailable` before starting the server,
- `MemAvailable` during startup,
- `MemAvailable` while idle after model load,
- `MemAvailable` during request load,
- the lowest observed `MemAvailable`.

Derived metric:

```text
system_memory_drop_gib =
  (baseline_mem_available_bytes - min_mem_available_bytes) / 1024^3
```

Use this for statements like:

```text
This run created about X GiB of machine-level memory pressure.
```

Do not call this "VRAM" unless the platform telemetry proves that is what it is.

## Process/Cgroup Memory

This is still useful, but it must be named correctly.

For systemd-launched runs, record:

```sh
systemctl --user show <unit> \
  --property=MemoryCurrent,MemoryPeak,MainPID,ActiveState,SubState,Result
```

Derived metrics:

```text
cgroup_memory_current_gib = MemoryCurrent / 1024^3
cgroup_memory_peak_gib    = MemoryPeak / 1024^3
```

Use this for statements like:

```text
The server process cgroup peaked at X GiB.
```

Do not use cgroup memory as the total model footprint on unified-memory systems.
It can undercount the memory pressure that matters for OOM risk.

## vLLM Capacity

Parse vLLM logs for the capacity lines:

```text
Available KV cache memory: X GiB
GPU KV cache size: N tokens
Maximum concurrency for C tokens per request: Qx
```

Record:

- available KV cache GiB,
- GPU KV cache token capacity,
- context size used by the vLLM concurrency line,
- vLLM-reported max concurrency.

Use this for fit/capacity planning:

```text
safe_requested_concurrency = floor(safety_margin * reported_max_concurrency)
```

The default safety margin should be explicit in the report. A reasonable
starting value is `0.8`.

## GPU Telemetry

Record GPU telemetry when the platform exposes it, but treat it as another
signal, not automatic ground truth.

Prefer, in order:

1. `tegrastats` on NVIDIA Tegra/Jetson/GB10-style unified-memory systems,
2. other vendor-supported telemetry for the target platform,
3. NVML/DCGM counters,
4. `nvtop` sampled output or screenshots/logs,
5. `nvidia-smi` if it reports useful fields.

For each run, capture:

- whether `tegrastats` was available,
- `tegrastats` RAM and swap lines when available,
- `tegrastats` GR3D/GPU utilization, when available,
- `tegrastats` temperature and power lines, when available,
- GPU memory used, if available,
- GPU utilization,
- memory-controller utilization, if available,
- power,
- temperature,
- whether the fields were `N/A` or known unreliable.

On GB10-style unified-memory systems, GPU memory counters may be incomplete or
misleading. Cross-check them against `MemAvailable` drop and vLLM KV capacity.

Use `tegrastats` as a sampled time series, not as a one-off screenshot:

```sh
tegrastats --interval 1000
```

If `tegrastats` is not installed on the target machine, record that explicitly
in the run artifact and fall back to the other signals. Do not leave the field
blank.

## Request Performance

For each load phase, record:

- requested concurrency,
- achieved concurrency when it diverges from requested,
- successful responses,
- failed responses, broken down by error type,
- wall time,
- prompt tokens,
- completion tokens,
- total tokens,
- output tokens per second,
- total tokens per second,
- latency p50/p95/p99 when enough samples exist,
- TTFT mean/p50/p95/p99 when streaming,
- TPOT as the request-weighted mean of per-request means,
- ITL as the token-weighted mean over all inter-token gaps.

TPOT and ITL differ in weighting and must not be presented as
interchangeable: TPOT treats every request equally and describes per-user
experience; ITL weights by generated tokens and describes steady-state system
behavior. State the weighting wherever either number is shown; the reporting
metric registry in `internal/report/metrics.go` defines the displayed terms.

Keep the workload shape explicit:

- prompt length,
- max output tokens,
- temperature and sampling settings,
- endpoint type,
- tool/reasoning settings if enabled.

## SLOs and Goodput

A workload may declare latency targets:

```json
"slo": {"ttft_p95_ms": 500, "e2el_p95_ms": 30000}
```

Goodput is derived at report time from detailed request rows: the fraction
of completed requests meeting every set target, and goodput as SLO-met
requests per second. The report renders the `% in SLO` and `Goodput req/s`
columns only when a workload declares an SLO; it never invents a quality
bar. High throughput with low goodput means requests completed too slowly
to be useful.

## Reporting Rules

Reports should present memory columns with specific names:

- `system_memory_drop_gib`,
- `min_mem_available_gib`,
- `cgroup_memory_current_gib`,
- `cgroup_memory_peak_gib`,
- `vllm_available_kv_cache_gib`,
- `vllm_kv_cache_tokens`,
- `vllm_reported_max_concurrency`,
- `gpu_memory_used_gib` when available.

Avoid these ambiguous labels:

- `memory_usage`,
- `gpu_memory` without naming the telemetry source,
- `model_memory`,
- `VRAM` on unified-memory systems unless verified.

When multiple signals disagree, report the disagreement instead of hiding it.

Example:

```text
cgroup peak: 12 GiB
system MemAvailable drop: 83 GiB
vLLM available KV cache: 58 GiB
GPU telemetry: unavailable / N/A / untrusted
```

## Capacity Versus Performance

Capacity and performance answer different questions.

Capacity asks:

```text
Can this context/concurrency fit without failing or OOMing?
```

Use:

- vLLM reported max concurrency,
- system memory drop,
- startup/load failures,
- safety margin.

Performance asks:

```text
How fast and stable is it under load?
```

Use:

- output token/s,
- total token/s,
- request latency,
- errors,
- GPU utilization,
- queueing behavior.

A configuration can fit but still be slow. A configuration can be fast for short
requests but unsafe for long-context load. Keep those conclusions separate.

The per-row version of this rule is the capacity-versus-active-context
contract in `2026-07-02-context-semantics.md`: a performance row is never
labeled with a capacity number (`max_model_len`); it is labeled by the active
context the workload actually exercised.

## Minimum Run Record

Every sample should save a machine-readable record with:

- model identifier,
- server backend and version,
- full server flags,
- context window,
- requested concurrency,
- batched-token setting,
- prompt/output workload shape,
- baseline system memory,
- sampled system memory,
- cgroup memory,
- vLLM capacity lines,
- GPU telemetry if available,
- request performance,
- errors and exit reasons.

Raw logs can be kept locally, but public repos should publish sanitized summaries
unless raw logs have been reviewed for local paths and credentials.
