# CLI and deployment reference

LocalPerf exposes this command surface:

```text
localperf bench run
localperf artifact check
localperf artifact merge
localperf artifact render
localperf view
```

There is no public generic planner, spec runner, raw HTTP load command, or
artifact reconstruction command.

## Benchmark runner

```sh
localperf bench run --suite <name-or-path> --deployment deployment.json [flags]
```

| Flag | Meaning |
| --- | --- |
| `--suite` | `practical-64k`, `throughput-4k`, `context-ladder`, or a strict suite JSON path. |
| `--deployment` | Strict deployment JSON path. |
| `--case` | Include one named suite case; repeatable. |
| `--concurrency` | Include one declared concurrency batch; repeatable. |
| `--artifact` | SQLite destination; an existing valid model artifact is appended to. |
| `--run-dir` | Raw run directory; required for resume. |
| `--timeout` | Overall timeout such as `2h`. |
| `--dry-run` | Compile, persist, and validate without launching a server or sending requests. |
| `--resume` | Reuse completed result files from the same execution directory. |

The run directory always records the resolved suite, redacted deployment, exact
execution plan, normalized internal execution document, events, results, logs,
and summary. The internal document is not a public input format.

## Deployment format

Unknown fields, trailing JSON, missing fields, and stale aliases are errors.
Version is always `"1"`. `runtime.type` is one of `llama-cpp-managed`,
`llama-cpp-endpoint`, `vllm-managed`, `vllm-endpoint`, or `openai-endpoint`;
the `-managed` types start and stop the server, the others need
`endpoint_base_url` (or `port`) and must not set `command`.

A managed llama.cpp deployment, the primary form:

```json
{
  "version": "1",
  "name": "model-runtime-name",
  "model": "served-model-alias",
  "model_revision": "immutable-revision",
  "runtime": {
    "name": "llama-cpp-official",
    "type": "llama-cpp-managed",
    "owner": "ggml-org",
    "source": "official release",
    "version": "pinned-build",
    "digest": "optional release asset digest",
    "command": "/absolute/path/to/llama-server",
    "host": "127.0.0.1",
    "port": 8101,
    "env": {},
    "args": []
  },
  "server": {
    "enable_prefix_caching": false,
    "speculative_decoding": [],
    "llama_cpp": {
      "model_file": "/absolute/path/to/model.gguf",
      "gpu_layers": 999,
      "flash_attn": "auto",
      "cache_type_k": "q8_0",
      "cache_type_v": "q8_0",
      "batch_size": 2048,
      "ubatch_size": 512,
      "threads": 8
    }
  },
  "client": {
    "load_generator": "localperf_http",
    "backend": "openai-chat",
    "endpoint": "/v1/chat/completions"
  },
  "safety": {
    "min_mem_available_gib": 8,
    "poll_interval_millis": 1000,
    "startup_timeout_sec": 300,
    "workload_timeout_sec": 1800,
    "http_timeout_sec": 30
  }
}
```

LocalPerf derives the slot count and context from the selected suite cases:
`--parallel` is the largest batch concurrency and `--ctx-size` is that count
times the largest context target, because llama-server splits its context
across slots. It also owns `--model`, `--alias`, `--host`, `--port`, and the
`server.llama_cpp` flags; putting them in `runtime.args` is refused. After
readiness, and before warmup, LocalPerf reads `GET /props` and stops the
profile when `total_slots` or the per-slot `n_ctx` is below the suite's
needs. The check also runs for `llama-cpp-endpoint`, so an external server
must be started with matching `--parallel` and `--ctx-size`; a router-mode
server must be measured through a single-model instance. The result is
recorded as a `server_limits` event, and `/props` is stored with the engine
identity. `server.llama_cpp` is only for `llama-cpp-managed`.

With `enable_prefix_caching: false`, llama.cpp requests carry
`"cache_prompt": false`, so a slot never reuses an earlier prompt. The model
revision is provenance only for llama.cpp; llama-server loads the local file.

vLLM settings go under `server.vllm` (`gpu_memory_utilization`,
`kv_cache_dtype`, `attention_backend`, `moe_backend`, `max_num_batched_tokens`,
`enable_sleep_mode`, `sleep_level`), and only on vLLM runtimes. For
`vllm-managed`, `model_revision` is passed as `--revision`, and LocalPerf owns
`--max-model-len`, `--max-num-seqs`, and the structured `server.vllm` flags.
`client.load_generator` may be `vllm_bench` only on vLLM runtimes; it then
uses `runtime.bench_command` (or `command`), and `client.tokenizer` and
`client.extra_args` apply. The default `localperf_http` rejects both.

`model_revision`, runtime command/version, requested backends, and
speculative-decoding arguments are material provenance. Pin and review them
before real GPU work. On managed vLLM servers, a declared attention or MoE
backend—including `auto`—enables a request-scoped torch-profiler canary for
every selected case and concurrency point. Each canary uses the same token
shape and request count as its timed point. LocalPerf reads the emitted CUDA
execution table before accepting that point; missing evidence, or a mismatch
against a concrete request, stops the run. The profiler is inactive during
timed measurements. External endpoints must use `auto` rather than making a
concrete backend claim that LocalPerf cannot attest from managed server
evidence. llama.cpp has no request-scoped kernel attestation; record
`flash_attn` and cache types as requested settings, not as observed kernels.

Warmup runs through the deployment's load generator, so llama.cpp and
endpoint deployments warm up over HTTP.

## Suite format

A custom suite is a strict version-1 document containing a name, explicit
warmup, and named cases. Each case declares input/output tokens, context target
and semantics, phase, role, repeats, deterministic sampling options, and exact
batches:

```json
"batches": [
  {"concurrency": 1, "requests": 1},
  {"concurrency": 6, "requests": 6}
]
```

There is no implicit request scaling and no hidden reference or stress family.
Prefer the built-in suites whenever one matches the requested benchmark.

## Artifact commands

```sh
localperf artifact check runs/models/model.sqlite
localperf artifact merge --into runs/models/model.sqlite runs/a.sqlite runs/b.sqlite
localperf artifact render --output runs/models/model.html runs/models/model.sqlite
localperf view --no-open runs/models/model.sqlite
```

Validate before every merge, render, comparison, or publication. Keep all runs
for one model in one SQLite artifact and one HTML report.
