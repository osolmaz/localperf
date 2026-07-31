---
name: localperf
description: Prepare, run, resume, validate, review, compare, and report local LLM inference benchmarks with the LocalPerf CLI and its model-level SQLite artifacts. Use whenever a user mentions LocalPerf, `localperf sweep plan`, LocalPerf specs, active-context sweeps, benchmark versus diagnostic workloads, LocalPerf artifacts or reports, or benchmarking a vLLM-managed or external OpenAI-compatible server through this repository.
compatibility: Requires a trusted LocalPerf checkout or installed LocalPerf binary. Real managed runs require vLLM; source builds require Go 1.26 or newer. SQLite inspection requires sqlite3.
---

# LocalPerf

Use LocalPerf for the complete benchmark workflow. When the user asks for
LocalPerf, do not substitute another benchmark product, a direct `vllm bench`
call, an ad hoc HTTP load script, or a legacy LocalPerf command.

## Source of truth

Work from the repository root. Check these sources in order:

1. `AGENTS.md` for repository checks and safety rules.
2. `localperf <command> --help` for the installed command surface.
3. `README.md` for the current supported workflow.
4. `docs/2026-07-02-default-inference-sweep.md` for the default grid.
5. `docs/2026-07-31-practical-c1-c6-64k.md` when that fixed sweep is requested.
6. `docs/2026-07-02-context-semantics.md` for active versus capacity context.
7. `docs/2026-06-23-measurement-methods.md` for metrics and memory labels.
8. `docs/2026-06-29-sqlite-run-artifact-format.md` for artifact rules.

Read [CLI and spec reference](references/cli-and-spec.md) when creating or
editing a spec. Read [Benchmark protocol](references/benchmark-protocol.md)
before a real run. Read [Artifacts and reporting](references/artifacts-and-reporting.md)
before merging, comparing, or publishing results.

If code and prose disagree, stop and inspect the implementation and tests. Do
not guess around a contract mismatch. Update this skill whenever the CLI,
schema, validation rules, or default sweep changes.

## Hard rules

- Execute measurements only with `localperf bench run`.
- Generate normal managed-vLLM grids with `localperf sweep plan`.
- Run `localperf bench plan` and a filtered dry run before using GPU time.
- Keep the memory floor positive and conservative. Never lower it just to make
  a run pass.
- Use `role: "benchmark"` only for reportable points. Use
  `role: "diagnostic"` for small probes and troubleshooting.
- Use `num_prompts` for a fixed request count or `prompts_per_user` to scale requests with concurrency. LocalPerf does not impose a sample floor.
- Every workload must declare `context_target` and `context_semantics`.
- Treat requested token lengths as intent and endpoint usage counts as evidence.
- Keep active context, server capacity, fresh prefill, decode, cached-prefix
  reuse, and aggregate multi-request throughput separate.
- Keep all attempts, failures, skips, logs, telemetry, and exact commands.
  Never select only the fastest sample.
- Use one SQLite artifact and one rendered HTML report per model.
- Run full artifact validation before reading, merging, rendering, comparing,
  or publishing an artifact.
- Resolve runtimes through the `manage-runtimes` prebuilt-first gate. A
  benchmark request does not authorize compiling a runtime from source.
- Stop if the runtime, model revision, quantization, kernel, backend, or package
  differs from the approved setup. Do not silently fall back or substitute.
- If an unrelated workload causes a memory guard failure, do not retry while
  that workload remains unchanged. Report the blocker or ask permission to
  pause it.
- A configured backend is not proof that it ran. Require evidence from a real
  measured request and preserve the supporting server log or trace.
- Never put credentials in specs, logs, commands, reports, or chat output.
  Use environment variables in place; LocalPerf redacts common secret names in
  stored environment data.

## Choose the execution path

### Managed vLLM

Use this path when LocalPerf should start and stop vLLM. Generate the spec and
pass runtime settings to the generator:

```sh
localperf sweep plan \
  --model <model-id> \
  --contexts 4k,8k,16k,32k,64k \
  --concurrency 1,4,8,16,32 \
  --repeats 3 \
  --vllm-command /absolute/path/to/vllm \
  --min-mem-available-gib <safe-floor> \
  --out spec.json
```

Use `--gpu-memory-utilization`, `--kv-cache-memory-bytes`, repeated
`--profile-arg`, and repeated `--profile-engine-arg` only when the runtime
configuration calls for them. Record deliberate ladder caps with
`--trim <context>=<max-concurrency>:<reason>`.

### External OpenAI-compatible server

Use this path for llama.cpp, a separately managed vLLM server, or a hosted
endpoint. Write a current custom spec with an unmanaged profile,
`endpoint_base_url`, disabled warmup, and `load_generator: "localperf_http"`.
The identifier is an internal LocalPerf execution mode; there is no public raw
HTTP benchmark command.

Before using this path, read the external-server section in
[CLI and spec reference](references/cli-and-spec.md). Confirm that the endpoint
returns exact usage counts and supports streamed usage when TTFT is required.
Also account for the current repeated-prompt limitation described there. Do not
claim fresh-prefill results when requests can share a warmed prefix.

### Unsupported runtime or endpoint

Stop when LocalPerf cannot represent or attest the intended setup. Do not call
an internal package function, revive a deleted binary, parse a raw run directory
as a report, or bypass validation with another client.

## Standard workflow

### Inspect and identify

Record before launch:

- LocalPerf version or Git commit.
- Model repository, exact revision, and local weight identity.
- Runtime owner, version or commit, executable or image digest, and environment.
- Intended quantization, attention, linear, MoE, KV-cache, and speculative
  decoding backends.
- Hardware, driver, available memory, swap, and telemetry sources.
- Existing inference processes and occupied ports.
- Artifact path, run directory, comparison dimensions, aggregation rule, and
  stop conditions.

Do not remove existing runtimes, caches, images, weights, or benchmark evidence
as part of preparation unless the user approves the named cleanup targets.

### Generate and inspect the plan

```sh
localperf sweep plan \
  --model <model-id> \
  --contexts 4k,8k,16k,32k,64k \
  --concurrency 1,4,8,16,32 \
  --repeats 3 \
  --min-mem-available-gib <safe-floor> \
  --out spec.json

localperf bench plan --spec spec.json
localperf bench plan --spec spec.json --json > plan.json
```

Inspect every profile and workload. Check ports, runtime command, model,
server limits, active token shapes, concurrency ladders, repeats, request
counts, memory floor, adaptive settings, and artifact destination.

A default sweep includes:

- `max-throughput-reference` on a 4k-capacity profile.
- Active 4k, 8k, 16k, 32k, and 64k prefill and decode workloads.
- Concurrency 1, 4, 8, 16, and 32 where safe.
- Roughly `N - headroom` input plus 1 output token for prefill.
- Roughly `N - output - headroom` input plus up to 1,024 output tokens for
  decode.

The 4k capacity reference is not the active 4k decode point. The generated
active 4k decode shape is about 3,008 requested input tokens plus 1,024 output
tokens, while the reference intentionally uses a shorter capacity workload.

### Run a filtered dry case

```sh
rm -rf /tmp/localperf-onecase-dry /tmp/localperf-onecase-dry.sqlite

localperf bench run \
  --dry-run \
  --spec spec.json \
  --profile 4k-reference \
  --workload max-throughput-reference \
  --concurrency 1 \
  --run-dir /tmp/localperf-onecase-dry

localperf artifact check /tmp/localperf-onecase-dry.sqlite
```

A dry run proves planning and persistence, not server compatibility, model fit,
backend selection, or performance.

### Run a small real canary

Start with one reportable point or a workload explicitly marked diagnostic.
Confirm:

- the memory guard and process cleanup work;
- the server reaches the expected health endpoint;
- model and runtime identity probes match the intended deployment;
- one real request completes with the required output and usage counts;
- the expected backend executes without fallback;
- telemetry and server logs are captured;
- the resulting artifact passes `artifact check`.

Stop after any mismatch. Fix the cause before scaling the sweep.

### Run the benchmark

```sh
mkdir -p runs/models

localperf bench run \
  --spec spec.json \
  --timeout 4h \
  --artifact runs/models/<model-slug>.sqlite
```

Filter a batch with repeated `--profile`, `--workload`, or `--concurrency` flags. Keep all batches for the same model in the same artifact.

For an interrupted run, reuse the exact run directory:

```sh
localperf bench run \
  --spec spec.json \
  --run-dir <previous-run-dir> \
  --resume \
  --artifact runs/models/<model-slug>.sqlite
```

Do not use `--resume` with a new run directory or a materially changed spec.
Start a new run and append it to the model artifact instead.

### Validate and render

```sh
localperf artifact check runs/models/<model-slug>.sqlite
localperf artifact render \
  --output runs/models/<model-slug>.html \
  runs/models/<model-slug>.sqlite
localperf view --no-open runs/models/<model-slug>.sqlite
```

Run `artifact check` again after a merge or after storing a report back into an
artifact.

## Completion standard

A benchmark is complete only when:

- all intended points are completed, failed, or skipped with reasons;
- all reportable points pass exact-result checks;
- measured prompt and output tokens support the declared context labels;
- the runtime and observed backend match the intended setup;
- safety guards did not fire during a result reported as valid;
- every repeat and error remains in the artifact;
- the extension decision is recorded after the baseline grid;
- the model-level SQLite artifact passes full validation;
- the model-level HTML report renders from that artifact;
- comparisons separate c1 per-request decode from aggregate concurrency;
- conclusions include raw counts, absolute differences, spread, failures, and
  operational tradeoffs.

If any item is missing, describe the run as diagnostic, partial, failed, or
blocked. Do not promote it to a completed benchmark by prose.

## Repository checks

When changing LocalPerf behavior, specs, artifacts, reports, or this skill, run
the checks required by `AGENTS.md`:

```sh
go test ./...
go vet ./...
npx -y @simpledoc/simpledoc check
go run github.com/osolmaz/slophammer/go/cmd/slophammer-go@v0.4.1 check .
```

For runner or artifact internals, also run:

```sh
scripts/check-crap.sh
scripts/check-mutation.sh
```

Finish with the one-case dry run and artifact validation from `AGENTS.md`.
