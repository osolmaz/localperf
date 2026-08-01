# localperf

localperf is a local LLM inference benchmark CLI.
It runs named benchmark suites against declared deployments, collects all evidence
in one portable SQLite artifact per model, and renders reports that only label
what the measurements actually confirm.

It is currently focused on vLLM-managed runs:

```sh
localperf bench run --suite practical-64k --deployment deployment.json \
  --artifact runs/models/<model-slug>.sqlite
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

Copy the deployment example and set its model, pinned runtime, server options,
and safety floor:

```sh
cp examples/deployments/vllm-managed.json deployment.json
```

Validate the practical suite and write its exact execution plan without
starting the model:

```sh
localperf bench run --dry-run \
  --suite practical-64k \
  --deployment deployment.json \
  --run-dir /tmp/localperf-practical-dry
```

The run directory contains `suite.json`, a redacted `deployment.json`, and
`execution-plan.json`. Validate its SQLite artifact:

```sh
localperf artifact check /tmp/localperf-practical-dry.sqlite
```

Run the full suite only when the machine is ready for it, pointing batches at
one model-level artifact:

```sh
localperf bench run --suite practical-64k --deployment deployment.json --timeout 4h \
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

## Suites and deployments

The built-in suites are `practical-64k`, `throughput-4k`, and
`context-ladder`. A suite owns the cases, token shapes, exact request batches,
and repeats. A deployment owns the model revision, pinned runtime, requested
backends, server options, client options, and memory floor. LocalPerf derives
`max_model_len` and `max_num_seqs` from the selected suite cases and refuses
runtime arguments that try to override those limits.

`practical-64k` always contains `generate-empty` and `generate-full` at c1 and
c6, with three repeats: exactly 12 measurements. Every successful practical
measurement provides both decode and effective-prefill values in the existing
throughput table. Use repeatable `--case` and `--concurrency` flags only for a
small smoke run or a deliberate subset.

## Context Semantics

Every suite case declares what its context number means:

```json
"context_target": 32768,
"context_semantics": "active"
```

`"active"` claims the workload actually pushes ~N tokens through the KV cache
and is validated: the requested input+output must land within 90–100% of the
target, on the random dataset, with a fixed range ratio. `"capacity"` marks a
server-limit/concurrency point and must match the profile's `max_model_len`.
Suites that conflate the two are refused before any GPU time is spent, and the
report labels rows only by declared-and-measured active context or by the
measured token shape, never by `max_model_len` alone. See
[Context Semantics](docs/2026-07-02-context-semantics.md) for the contract.

Every case also declares its role:

```json
"role": "benchmark"
```

Each case lists explicit `{concurrency, requests}` batches. There is no public
request-scaling rule. Use `"diagnostic"` for probes and troubleshooting.
Diagnostic evidence stays in the SQLite artifact but is excluded from
benchmark reports and comparisons. Only validated SQLite artifacts written by
`localperf bench run` are reportable; raw result JSON and run directories are
not accepted report inputs.

LocalPerf validates the suite and deployment before compiling an exact
execution plan, after dataset materialization, immediately before execution,
and again before writing an artifact. It also requires exact request counts, concurrency, token
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

Deployments include a `safety.min_mem_available_gib` floor. localperf checks
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

`examples/deployments/vllm-managed.json` shows the strict deployment format.
Replace its model and runtime settings before running a suite.
