# localperf

localperf is a local LLM inference benchmark CLI.
It runs benchmark plans against local inference servers, collects all evidence
in one portable SQLite artifact per model, and renders reports that only label
what the measurements actually confirm.

It is currently focused on vLLM-managed runs, with an engine-neutral benchmark
spec and CLI shape:

```sh
localperf sweep plan --model <model-id> --out spec.json
localperf bench run  --spec spec.json --artifact runs/models/<model-slug>.sqlite
localperf artifact render runs/models/<model-slug>.sqlite
localperf view runs/models/<model-slug>.sqlite
```

## Install

Install with Go:

```sh
go install github.com/osolmaz/localperf/cmd/localperf@v0.1.0
```

Or download a prebuilt binary (linux/darwin, amd64/arm64) from the
[latest release](https://github.com/osolmaz/localperf/releases/latest).
From a repo checkout, `go run ./cmd/localperf` works everywhere `localperf`
appears below.

## Requirements

- vLLM installed and available as `vllm` for real managed benchmark runs.
- Enough available system memory for the model profile you run.
- Go 1.26 or newer only when installing via `go install` or running from
  source.
- `sqlite3` if you want to inspect artifacts from the shell.

## Quick Start

Generate the default context/concurrency sweep spec instead of hand-writing
the grid:

```sh
localperf sweep plan \
  --model nvidia/diffusiongemma-26B-A4B-it-NVFP4 \
  --contexts 4k,8k,16k,32k --concurrency 1,4,8 \
  --out spec.json
```

Preview the planned runs without starting a model:

```sh
localperf bench plan --spec spec.json
```

Run one dry benchmark case and validate the artifact:

```sh
localperf bench run \
  --dry-run \
  --spec spec.json \
  --profile 4k-reference \
  --workload max-throughput-reference \
  --concurrency 1 \
  --run-dir /tmp/localperf-onecase-dry

localperf artifact check /tmp/localperf-onecase-dry.sqlite
```

Run the full spec only when the machine is ready for it, pointing batches at
one model-level artifact:

```sh
localperf bench run --spec spec.json --timeout 4h \
  --artifact runs/models/<model-slug>.sqlite
```

Render the HTML report:

```sh
localperf artifact render runs/models/<model-slug>.sqlite
```

Open one or more SQLite reports in a temporary local viewer:

```sh
localperf view runs/models/<model-slug>.sqlite [runs/models/other.sqlite ...]
```

## Model-Level Artifacts

Keep every run of one model in a single SQLite file and render one HTML
report from it. Pointing `bench run --artifact` at an existing artifact
appends the new run; re-running the same run directory replaces that run.
Combine existing per-run artifacts with:

```sh
localperf artifact merge \
  --into runs/models/<model-slug>.sqlite runs/batch-1.sqlite runs/batch-2.sqlite
```

Merges are idempotent: runs already present are skipped, and a run id that
collides with different provenance is refused instead of silently replaced.
The report lists every run and aggregates repeated points across runs with
mean ± spread.

## Spec Provenance

Create specs with `sweep plan`; machine-specific runtime choices are flags
(`--vllm-command`, `--gpu-memory-utilization`, `--kv-cache-memory-bytes`),
and deliberate ladder caps are declared trims with reasons
(`--trim 64k=8:'12 GiB KV budget'`) that render in reports like adaptive
skips. Generated specs carry a verified `generator` stamp: reports label
runs "Generated default sweep" only while the spec's content hash still
matches, and anything hand-written or edited shows as "Custom grid".

## Context Semantics

Every workload must declare what its context number means:

```json
"context_target": 32768,
"context_semantics": "active"
```

`"active"` claims the workload actually pushes ~N tokens through the KV cache
and is validated: the requested input+output must land within 90–100% of the
target, on the random dataset, with a fixed range ratio. `"capacity"` marks a
server-limit/concurrency point and must match the profile's `max_model_len`.
Specs that conflate the two are refused before any GPU time is spent, and the
report labels rows only by declared-and-measured active context or by the
measured token shape, never by `max_model_len` alone. See
[Context Semantics](docs/2026-07-02-context-semantics.md) for the contract.

Every workload also declares its role:

```json
"role": "benchmark"
```

A benchmark point normally requests at least `max(8, 2 * concurrency)` prompts. A deliberate experiment whose measurement unit is one or more complete concurrent batches may instead set `batches_per_repeat`; each point then requests exactly `batches_per_repeat * concurrency` prompts, and repeats provide the replication axis. Use `"diagnostic"` for smaller probes that declare neither policy. Diagnostic evidence stays in the SQLite artifact but is excluded from benchmark reports and comparisons. Only validated SQLite artifacts written by `localperf bench run` are reportable; raw result JSON and run directories are not accepted report inputs.

These checks apply to every workload, whether a spec was generated or written
by hand. LocalPerf validates the complete spec on load, after filtering and
dataset materialization, immediately before execution, and again before
writing an artifact. It also requires exact request counts, concurrency, token
totals, and throughput fields from every successful result. Artifact append,
merge, check, render, and view run full validation and reject schema drift,
contract violations, broken foreign keys, or mismatched evidence hashes.

Workloads may also declare latency targets for goodput:

```json
"slo": {"ttft_p95_ms": 500}
```

The report then shows the fraction of requests meeting the target and goodput
in requests per second.

## Outputs

Each run writes:

- the SQLite artifact (`--artifact` path, or `runs/<run-id>.sqlite`): the
  canonical record — specs, engine/profile/workload definitions, measurements,
  per-request rows, metric stats, GPU telemetry, hardware inventory, engine
  identity probes, events, commands, and logs. It is also the
  machine-readable export.
- `runs/<run-id>/events.jsonl`, `results/*.json`, `logs/*.log`,
  `summary.json`: raw run data.
- the HTML report (`artifact render`) and the viewer (`view`) are the only
  rendered views.

Example inspection:

```sh
sqlite3 runs/models/<model-slug>.sqlite \
  "select run_id, profile_id, workload_id, concurrency, status, aggregate_output_tok_s from measurements"
```

## Memory Safety

Specs include a `safety.min_mem_available_gib` floor. localperf checks
`/proc/meminfo` before major steps and while subprocesses run. If available
memory drops below the floor, the current step is stopped and skipped/failed
rows are recorded.

On unified-memory systems, do not treat process/cgroup memory as total model
memory. For capacity planning, compare multiple signals:

- whole-machine `MemAvailable` drop,
- process/cgroup memory,
- vLLM KV-cache capacity lines,
- GPU or platform telemetry when available.

localperf samples GPU utilization and memory during measurements from every
available source (`tegrastats`, `nvidia-smi`) and names the source in the
report. See [Measurement Methods](docs/2026-06-23-measurement-methods.md) for
the memory reporting policy.

## Example

`examples/sharegpt-chat-smoke/spec.json` shows the current strict spec format
for a small diagnostic chat run. Replace its model and endpoint settings before
running it.
