# localperf

<p align="center">
  <img src="assets/cover.svg" alt="localperf: a benchmark CLI for local LLM inference that keeps every run in one SQLite file" width="880">
</p>

localperf is a benchmark CLI for local LLM inference. It runs a named
benchmark suite against one model deployment and keeps every run of that model
in one SQLite file, from which it renders an HTML report.

A deployment is either a vLLM server that localperf starts and stops itself,
or an OpenAI-compatible server that is already running, such as llama.cpp's
`llama-server`. The suite says what to measure, and the deployment says which
model and runtime settings to measure it on.

localperf checks the numbers before it reports them. A row labeled "64k
active" means the requests really put about 64k tokens through the KV cache,
and a run whose backend fell back to a different kernel is marked invalid
instead of reported. Every request gets a unique prefix, so a repeated prompt
cannot be answered from the server's prompt cache.

## Install

Download the archive for your system from the
[releases](https://github.com/osolmaz/localperf/releases) and check it against
`SHA256SUMS` before you unpack `localperf`. Or install it with Go 1.26:

```sh
go install github.com/osolmaz/localperf/cmd/localperf@latest
```

Real managed runs need vLLM installed as `vllm`, and enough free memory for
the model you run. `sqlite3` is useful for looking inside artifacts from the
shell.

## First run

Copy the example deployment and set `model` to your model. Then replace the
`replace-with-...` values with its revision and the pinned vLLM version:

```sh
cp examples/deployments/vllm-managed.json deployment.json
```

A dry run checks the suite and the deployment and writes the exact execution
plan without starting the model:

```sh
localperf bench run --dry-run \
  --suite practical-64k \
  --deployment deployment.json \
  --run-dir /tmp/localperf-practical-dry
localperf artifact check /tmp/localperf-practical-dry.sqlite
```

When the machine is free, run the full suite into the model's artifact, then
render the report or open it in the local viewer:

```sh
localperf bench run --suite practical-64k --deployment deployment.json --timeout 4h \
  --artifact runs/models/<model>.sqlite
localperf artifact render runs/models/<model>.sqlite
localperf view runs/models/<model>.sqlite [runs/models/other.sqlite ...]
```

`view` serves the reports on a temporary local address and puts each artifact
in its own tab.

## Suites

localperf has three built-in suites. Each one fixes its cases, token shapes,
request batches, and repeats, so two runs of the same suite measure the same
thing.

| Suite | What it measures |
| --- | --- |
| `practical-64k` | Generation with an almost empty and an almost full 64k context, at 1 and 6 users, three repeats each. Every point reports decode and prefill speed. |
| `throughput-4k` | Decode throughput at 4k active context with 1, 4, 8, 16, and 32 users, three repeats each. |
| `context-ladder` | Separate decode and prefill cases at 4k, 8k, 16k, 32k, 64k, and 128k active context, with the same user counts up to 64k and 1 and 4 users at 128k. |

localperf sets `max_model_len` and `max_num_seqs` from the suite cases, and
refuses runtime arguments that try to change them. Use `--case` and
`--concurrency` only for a small smoke run or a deliberate subset.

## Deployments

The example in `examples/deployments/vllm-managed.json` starts vLLM itself. To
measure a server that is already running, set `managed` to `false`, point
`endpoint_base_url` at it, and use the built-in HTTP client:

```json
"runtime": {
  "name": "llama.cpp",
  "type": "openai-compatible",
  "owner": "ggml-org",
  "source": "official release",
  "version": "b10156",
  "managed": false,
  "endpoint_base_url": "http://127.0.0.1:8110",
  "health_path": "/health"
},
"client": {
  "load_generator": "localperf_http",
  "backend": "openai-chat",
  "endpoint": "/v1/chat/completions"
}
```

Every deployment has a `safety.min_mem_available_gib` floor. localperf reads
`/proc/meminfo` before each step and while the server and load generator run.
When free memory drops below the floor, it stops the current step and records
the step as skipped or failed. Do not lower the floor to make a run pass.

## Artifacts

An artifact is one SQLite file that holds everything about a model's runs. It
stores the suite and deployment with every measurement and request, along with
GPU telemetry, hardware details, engine identity checks, and the commands and
logs of each step. Pointing `bench run --artifact` at an existing file adds the
new run to it, and running the same run directory again replaces that run.

To combine artifacts that were written separately:

```sh
localperf artifact merge \
  --into runs/models/<model>.sqlite runs/batch-1.sqlite runs/batch-2.sqlite
```

A merge skips runs that are already in the target, and refuses a run whose ID
matches an existing run with different provenance. The report lists every
run and shows each repeated point as mean ± spread.

You can query an artifact directly:

```sh
sqlite3 runs/models/<model>.sqlite \
  "select run_id, workload_id, concurrency, status, aggregate_output_tok_s from measurements"
```

## Context labels

Every suite case says what its context number means. `"active"` means the
requests put about that many tokens through the KV cache. localperf checks
that the input and output land within 90 to 100% of the target. `"capacity"`
means the server's `max_model_len`, which is a limit and says nothing about how
much context a request used. A suite that mixes the two is refused before any
GPU time is spent. [Context Semantics](docs/2026-07-02-context-semantics.md)
has the full rules.

## Prompt cache

The built-in HTTP client adds a short unique prefix to the first user message
of every request, so a repeated prompt cannot hit the server's prompt cache and
report a prefill speed that no cold request reaches. The prefix costs about ten
tokens. To measure prefix reuse on purpose, turn it off on a case or a
workload:

```json
"prompt_nonce": false
```

Requests sent by vLLM's own load generator are outside localperf's control.
See [Prompt Nonces](docs/2026-09-18-prompt-nonces.md) for the details.

## Memory on unified-memory machines

On a machine where the CPU and GPU share memory, such as an NVIDIA DGX Spark,
process or cgroup memory does not show what the model really uses. Compare the
drop in `MemAvailable` with the KV cache size that vLLM logs at startup.
localperf samples GPU use and memory from `tegrastats` and `nvidia-smi` and
names the source in the report. [Measurement
Methods](docs/2026-06-23-measurement-methods.md) explains how memory is
reported.
