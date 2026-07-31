# CLI and spec reference

Use this reference when generating, reviewing, or editing a LocalPerf spec.
Run the command's `--help` in the current checkout before relying on this file.

## Command surface

LocalPerf intentionally exposes a small command surface:

```text
localperf sweep plan
localperf bench plan
localperf bench run
localperf artifact check
localperf artifact merge
localperf artifact render
localperf view
```

There is no public raw HTTP load command, artifact reconstruction command, or
standalone vLLM benchmark wrapper. Do not recreate those paths.

From a source checkout, replace `localperf` with:

```sh
go run ./cmd/localperf
```

Source execution requires Go 1.26 or newer.

## Sweep planner

```sh
localperf sweep plan [flags]
```

| Flag | Meaning | Default |
| --- | --- | --- |
| `--model` | Model identifier; required. | none |
| `--contexts` | Active-context ladder. Accepts values such as `4k,8k,16k`. | `4k,8k,16k,32k,64k` |
| `--concurrency` | Client concurrency ladder. | `1,4,8,16,32` |
| `--prompts-per-user` | Requests per concurrent user; result is floored at 8. | `2` |
| `--num-prompts` | Fixed requests per point. Overrides scaling. | scaled |
| `--repeats` | Measurement rows per point. | `1` |
| `--reference` | Include the 4k capacity reference family. | `true` |
| `--stress` | Add 4,096-output-token spot checks and 128k points. | `false` |
| `--min-mem-available-gib` | Whole-machine available-memory floor. | `40` |
| `--vllm-command` | Managed vLLM executable. | `vllm` |
| `--gpu-memory-utilization` | Apply vLLM GPU memory utilization to each profile. | unset |
| `--kv-cache-memory-bytes` | Pin vLLM KV-cache bytes on each profile. | unset |
| `--profile-arg` | Add one profile serve argument. Repeatable. | none |
| `--profile-engine-arg` | Add one engine-specific profile argument. Repeatable. | none |
| `--omit-profile-engine-flag` | Remove one generated engine flag. Repeatable. | none |
| `--trim` | Cap one context ladder with a reason. Repeatable. | none |
| `--out` | Output path; stdout when omitted. | stdout |

Examples:

```sh
localperf sweep plan \
  --model org/model \
  --contexts 4k,8k,16k,32k,64k \
  --concurrency 1,4,8,16,32 \
  --prompts-per-user 2 \
  --repeats 3 \
  --out spec.json

localperf sweep plan \
  --model org/model \
  --contexts 32k,64k \
  --concurrency 64,128,256 \
  --trim 64k=128:'operator concurrency cap' \
  --out extension.json
```

The planner caps normal points at 128k and above to c4. Higher concurrency at
those contexts is a deliberate stress run.

## Bench planner

```sh
localperf bench plan --spec spec.json [filters]
```

Filters are repeatable:

- `--profile <name>`
- `--workload <name>`
- `--concurrency <number>`

Use `--json` for a machine-readable plan. Use `--run-dir` to preview exact
result paths for a chosen run directory.

## Benchmark runner

```sh
localperf bench run --spec spec.json [flags]
```

| Flag | Meaning |
| --- | --- |
| `--artifact` | SQLite destination. Existing valid artifacts are appended to. |
| `--run-dir` | Raw run directory. Required when resuming. |
| `--timeout` | Overall timeout, such as `2h` or `4h`. |
| `--dry-run` | Persist planned evidence without starting a server or sending measured requests. |
| `--resume` | Skip completed result files in the same prior run directory. |
| `--profile` | Include one profile. Repeatable. |
| `--workload` | Include one workload. Repeatable. |
| `--concurrency` | Include one concurrency value. Repeatable. |
| `--repeat` | Include one one-based repeat index. Repeatable. This filters execution without changing the stored spec. |

The runner validates the full spec on load, after filters, after dataset
preparation, immediately before execution, and before artifact persistence.
A successful raw subprocess is insufficient: the result must also contain the
exact request count, declared concurrency, token totals, and throughput fields.

## Artifact commands

Check an artifact before any other artifact operation:

```sh
localperf artifact check runs/models/model.sqlite
```

Merge valid artifacts into one model artifact:

```sh
localperf artifact merge \
  --into runs/models/model.sqlite \
  runs/batch-1.sqlite runs/batch-2.sqlite
```

Render beside the input artifact or choose a path:

```sh
localperf artifact render runs/models/model.sqlite
localperf artifact render \
  --output runs/models/model.html \
  --title 'Model benchmark' \
  runs/models/model.sqlite
```

`--store` writes the rendered report into the artifact. `--include-raw` is
reserved; do not rely on it as a raw-evidence escape hatch.

Open a temporary local viewer:

```sh
localperf view runs/models/model.sqlite
localperf view --no-open --addr 127.0.0.1:8766 runs/models/model.sqlite
```

The viewer validates every artifact before loading it.

## Current spec shape

LocalPerf accepts exactly one JSON object. Unknown fields, trailing JSON
objects, missing version, and stale aliases are errors. The version must be:

```json
"version": "1"
```

Top-level sections:

| Section | Purpose |
| --- | --- |
| `name`, `description`, `model` | Run identity. |
| `engines` | Managed runtime or benchmark-client definitions. |
| `runner` | Lifecycle and adaptive-stop behavior. |
| `safety` | Memory polling and phase timeouts. |
| `warmup` | Optional engine warmup traffic. |
| `profiles` | Server configuration or external endpoint identity. |
| `workloads` | Traffic shape, role, context claim, samples, and concurrency. |
| `generator` | Verified stamp written by `sweep plan`. Never hand-author it. |

### Engine fields

Important fields:

- `name`: referenced by profiles.
- `type`: descriptive engine adapter type.
- `command`: managed server command.
- `bench_command`: vLLM executable used for LocalPerf's internal vLLM bench
  execution.
- `managed`: optional engine-level metadata.
- `endpoint_base_url`: optional engine metadata; profile endpoint settings
  control request routing.
- `env`: runtime environment. Secret-looking values are redacted in artifacts.

### Runner fields

Common defaults:

```json
{
  "one_awake_profile": true,
  "stop_managed_on_exit": true,
  "append_timestamp_to_run": true,
  "adaptive": {
    "min_throughput_gain_pct": 10,
    "max_concurrency_factor": 2
  }
}
```

Adaptive execution runs each concurrency ladder in ascending order. It skips
higher points for the same profile/workload when:

- the previous point failed;
- throughput improved by less than the configured percentage;
- TTFT p99 exceeds `ttft_p99_ceiling_ms`, when set;
- requested concurrency exceeds the configured factor times vLLM's reported
  maximum concurrency.

Set `"enabled": false` only for a deliberate fixed-grid experiment. Negative
individual thresholds disable their specific rule.

### Safety fields

```json
{
  "min_mem_available_gib": 40,
  "poll_interval_millis": 1000,
  "startup_timeout_sec": 900,
  "workload_timeout_sec": 1800,
  "http_timeout_sec": 15
}
```

`min_mem_available_gib` must be positive. Pick it from current whole-machine
headroom, not from nominal GPU memory alone.

### Profile fields

Important profile fields:

- `name`, `engine`, and `model` are required after defaults.
- `managed: true` requires positive `port`, `max_model_len`, and
  `max_num_seqs`.
- `managed: false` leaves the server running and measures an existing endpoint.
- `host` and `port` address a local or directly reachable server.
- `endpoint_base_url` accepts a complete HTTP(S) base URL with no query or
  fragment.
- `max_model_len` is server capacity, not measured active context.
- `max_num_seqs` is server admission capacity, not client concurrency.
- `gpu_memory_utilization`, `kv_cache_dtype`, `attention_backend`,
  `moe_backend`, and `enable_prefix_caching` record material server settings.
- `args` and `engine_args` pass profile-specific runtime arguments.
- `env` supplies runtime or endpoint environment values.

Endpoint-only profiles with `endpoint_base_url` require disabled warmup and
all referenced workloads to use `localperf_http`. A host/port external profile
can use the internal `vllm_bench` load generator when a compatible vLLM bench
CLI is available.

### Workload fields

Every workload requires:

- unique `name`;
- `role: "benchmark"` or `role: "diagnostic"`;
- explicit `phase`, normally `prefill`, `decode`, or `mixed`;
- positive `context_target`;
- `context_semantics: "active"` or `"capacity"`;
- exactly one of `num_prompts`, `prompts_per_user`, or `batches_per_repeat`;
- positive `repeats`;
- a nonempty, unique `max_concurrency` list;
- a supported load generator and dataset.

For ordinary benchmark workloads, `prompts_per_user` must be at least 2 and every point must resolve to at least `max(8, 2 * concurrency)` prompts. A deliberate fixed-batch benchmark may instead set positive `batches_per_repeat`; every point then resolves to exactly `batches_per_repeat * concurrency` prompts. For example, `batches_per_repeat: 1` means one request at c1 and one simultaneous six-request batch at c6. Repeats are the independent replication axis for this policy. Diagnostics may use smaller counts but are excluded from report and comparison queries.

Common random-traffic fields:

```json
{
  "backend": "openai-chat",
  "endpoint": "/v1/chat/completions",
  "dataset_name": "random",
  "request_rate": "inf",
  "seed": 0,
  "random_input_len": 3008,
  "random_output_len": 1024,
  "random_range_ratio": "0",
  "ignore_eos": true,
  "temperature": 0
}
```

Use `ignore_eos` when the benchmark requires a forced output length. Treat any
short output, missing usage count, server error, or finish-reason mismatch as a
failed sample rather than quietly averaging it.

Streaming is the default for `localperf_http`. TTFT is reported only when its
stored source is a streamed first-token measurement. First-byte time is not
TTFT.

An SLO may declare `ttft_p95_ms`, `e2el_p95_ms`, or both. SLO workloads need
detailed request rows because goodput is derived from each request.

## Context semantics

For active context:

```json
{
  "context_target": 4096,
  "context_semantics": "active",
  "dataset_name": "random",
  "random_input_len": 3008,
  "random_output_len": 1024,
  "random_range_ratio": "0"
}
```

The requested input plus output must be between 90% and 100% of the target. When a cross-runtime tokenizer round trip changes the endpoint's measured input count, a calibrated workload may set `measured_input_tokens_expected`; preflight then checks that expected endpoint input plus output against the target while preserving the larger client request length. Set this only from a completed diagnostic calibration, start a new run after changing it, and treat endpoint usage as the final evidence. The paired profile must support at least that target. Reports show the target only when measured active-end tokens also confirm the same band.

For capacity:

```json
{
  "context_target": 4096,
  "context_semantics": "capacity"
}
```

The target must equal each paired profile's declared `max_model_len` when that
limit is known. The request may be shorter, and the report must not call it a
4k active-context result.

## Dataset modes

Built-in vLLM traffic uses `dataset_name`, with `random` used by generated
active-context sweeps.

Structured dataset adapters support:

- `synthetic`
- `sharegpt`
- `custom-jsonl`
- `raw-payload`

A structured dataset may set `path` or a `file://` URI, sample count, seed,
selection (`first_n`, `random`, or `shuffle`), expected token counts, and
request settings. LocalPerf materializes canonical JSONL and vLLM custom JSONL
inside the run directory and stores their hashes.

Canonical custom JSONL rows use the current `CanonicalRequest` shape, including
`id`, `mode`, `messages` or `prompt`, `max_output_tokens`, optional expected
token counts, and metadata. Unknown fields are rejected.

Treat structured active-context workloads carefully: the strict active-context
contract currently requires the random dataset shape. Do not disguise custom
text as random traffic merely to pass validation. If exact distinct external
prompts are required and the current contract cannot represent them, stop and
change LocalPerf with tests before collecting reportable data.

## External server patterns

### Existing local server with vLLM bench traffic

For an existing localhost server, an unmanaged host/port profile can still use
LocalPerf's internal `vllm_bench` execution. This is usually the better route
for an active-context sweep because the vLLM random dataset produces the token
shape expected by the context validator.

The profile is unmanaged, so LocalPerf will not start or stop the server. Keep
the engine's `bench_command` pointed at the approved vLLM executable. Verify a
filtered real canary before running the grid because OpenAI-compatible servers
differ in tokenizer behavior, streaming, and usage fields.

### Endpoint URL with LocalPerf HTTP traffic

An endpoint-only profile uses:

```json
{
  "name": "external-4k",
  "engine": "external",
  "model": "served-model-name",
  "endpoint_base_url": "http://127.0.0.1:8000",
  "managed": false,
  "max_model_len": 4096
}
```

Disable warmup and set every referenced workload to:

```json
"load_generator": "localperf_http"
```

Bearer auth is read from `OPENAI_API_KEY` or `HF_TOKEN`, first from the profile
environment and then from the process environment. Never commit a token value
to a spec.

Current pitfall: `localperf_http` random mode creates separate request IDs but
uses the same synthetic prompt text for every request. Prefix caching can make
prefill and aggregate scheduling unrepresentative. Do not label those results
fresh prefill or use them for a comparison that requires distinct prefixes.
Use the unmanaged host/port plus `vllm_bench` path when suitable, or stop and
extend LocalPerf's validated dataset support.

The endpoint must return exact OpenAI usage counts. Streamed TTFT requires a
valid event stream with token chunks and usage. Missing counts or failed
requests invalidate a completed benchmark point.

## Provenance

Generated specs include a content hash over the generator's trusted fields.
Reports label the spec as generated only while that hash matches. Editing the
file after generation changes the provenance label. Never rewrite the hash or
pretend a custom grid was generated.

Machine-specific generator flags and declared trims preserve provenance. Use
them instead of post-generation edits whenever possible.
