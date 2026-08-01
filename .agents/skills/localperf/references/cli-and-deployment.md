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
Version is always `"1"`.

```json
{
  "version": "1",
  "name": "model-runtime-name",
  "model": "org/model",
  "model_revision": "immutable-revision",
  "runtime": {
    "name": "vllm-official",
    "type": "vllm-managed",
    "owner": "vllm-project",
    "source": "official release",
    "version": "pinned-version",
    "digest": "optional immutable image digest",
    "command": "/absolute/path/to/vllm",
    "bench_command": "/absolute/path/to/vllm",
    "managed": true,
    "host": "127.0.0.1",
    "port": 8101,
    "env": {},
    "args": []
  },
  "server": {
    "gpu_memory_utilization": 0.5,
    "kv_cache_dtype": "fp8",
    "attention_backend": "flashinfer",
    "moe_backend": "auto",
    "enable_prefix_caching": false,
    "speculative_decoding": []
  },
  "client": {
    "load_generator": "vllm_bench",
    "backend": "openai-chat",
    "endpoint": "/v1/chat/completions",
    "tokenizer": "/optional/tokenizer/path",
    "extra_args": []
  },
  "safety": {
    "min_mem_available_gib": 40,
    "poll_interval_millis": 1000,
    "startup_timeout_sec": 900,
    "workload_timeout_sec": 1800,
    "http_timeout_sec": 15
  }
}
```

LocalPerf derives `max_model_len` and `max_num_seqs` from the selected suite
cases. Do not put those flags in `runtime.args`; conflicting derived or
structured server flags are refused.

`model_revision`, runtime command/version, requested attention/MoE/KV
backends, and speculative-decoding arguments are material provenance. Pin and
review them before real GPU work. Requested settings are not backend
attestation; preserve the observed server-log evidence and report fallbacks by
their observed names.

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
