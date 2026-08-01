---
name: localperf
description: Prepare, run, resume, validate, review, compare, and report local LLM inference benchmarks with LocalPerf named suites, deployment files, and model-level SQLite artifacts. Use whenever a user mentions LocalPerf, practical-64k, throughput-4k, context-ladder, benchmark cases, LocalPerf artifacts or reports, or benchmarking a vLLM-managed or external OpenAI-compatible deployment through this repository.
compatibility: Requires a trusted LocalPerf checkout or installed LocalPerf binary. Real managed runs require vLLM; source builds require Go 1.26 or newer. SQLite inspection requires sqlite3.
---

# LocalPerf

Use LocalPerf for the complete benchmark workflow. Do not substitute direct
`vllm bench` calls, ad hoc load scripts, public spec files, or deleted planner
commands.

## Source of truth

Work from the repository root. Read `AGENTS.md`, the current CLI help,
`README.md`, the relevant suite document, the context-semantics document, and
the artifact-format document. Read [CLI and deployment reference](references/cli-and-deployment.md)
before editing a deployment. Read [Benchmark protocol](references/benchmark-protocol.md)
before a real run and [Artifacts and reporting](references/artifacts-and-reporting.md)
before comparing or publishing results.

If code and prose disagree, inspect the implementation and tests. Do not guess
around the mismatch. Update this skill when the CLI, suite contracts, artifact
rules, or reports change.

## Hard rules

- Execute measurements only with `localperf bench run`.
- Use a named suite and a deployment file. There is no public planner or spec
  workflow.
- Run a one-case dry execution before GPU time. Inspect the emitted
  `suite.json`, redacted `deployment.json`, and `execution-plan.json`.
- Keep the memory floor positive and conservative.
- A suite owns cases, token shapes, exact request batches, and repeats. A
  deployment owns model/runtime identity, requested backends, server/client
  options, and safety.
- Do not combine suites or add hidden reference, stress, or diagnostic cases.
- Treat requested token lengths as intent and endpoint usage counts as evidence.
- Keep every attempt, failure, skip, repeat, log, telemetry sample, and exact
  command. Never select the fastest sample.
- Use one SQLite artifact and one HTML report per model.
- Validate an artifact before reading, merging, rendering, or publishing it.
- Resolve runtimes through `manage-runtimes`; a benchmark request does not
  authorize a source build or an untrusted runtime.
- Record requested and observed backends separately. A supported runtime
  fallback may remain valid, but label the observed backend exactly and never
  report it under the requested backend's name.
- Never put credentials in deployment files, logs, commands, reports, or chat.
  Locally configured credentials may be used in place for their normal purpose.

## Suites

- `practical-64k`: `generate-empty` and `generate-full`, c1 and c6, three
  repeats. This is exactly 12 measurements. Every valid row shows both decode
  and effective prefill from the same streamed generation requests.
- `throughput-4k`: the separate 4k generation-throughput suite.
- `context-ladder`: separate prefill and decode cases at 4k, 8k, 16k, 32k,
  64k, and 128k with the declared concurrency ladder.

Do not describe `throughput-4k` as part of `practical-64k`, invent alternate
metric names, or add hidden stress extensions.

## Standard workflow

Record the LocalPerf commit, exact model revision, runtime owner/version/path,
requested backends, hardware, memory, occupied ports, artifact path, and stop
conditions before launch. Do not remove runtimes, caches, models, or evidence
without explicit approval.

Copy or create a strict deployment file, then run one dry case:

```sh
rm -rf /tmp/localperf-onecase-dry /tmp/localperf-onecase-dry.sqlite

localperf bench run \
  --dry-run \
  --suite practical-64k \
  --deployment deployment.json \
  --case generate-empty \
  --concurrency 1 \
  --run-dir /tmp/localperf-onecase-dry

localperf artifact check /tmp/localperf-onecase-dry.sqlite
```

A dry run proves deterministic compilation and persistence, not model fit,
server compatibility, backend execution, or performance.

Run one small real canary before scaling. Confirm process cleanup, health,
model and runtime identity, exact token usage, backend observation, telemetry,
logs, and artifact validation. A supported fallback is acceptable only when
the artifact and report say what actually ran.

Run the complete suite into the model artifact:

```sh
mkdir -p runs/models
localperf bench run \
  --suite practical-64k \
  --deployment deployment.json \
  --timeout 4h \
  --artifact runs/models/<model-slug>.sqlite
```

Use repeatable `--case` and `--concurrency` flags only for a deliberate subset.
Resume only with the same suite, deployment, selection, and run directory:

```sh
localperf bench run \
  --suite practical-64k \
  --deployment deployment.json \
  --run-dir <previous-run-dir> \
  --resume \
  --artifact runs/models/<model-slug>.sqlite
```

Validate and render:

```sh
localperf artifact check runs/models/<model-slug>.sqlite
localperf artifact render --output runs/models/<model-slug>.html runs/models/<model-slug>.sqlite
localperf view --no-open runs/models/<model-slug>.sqlite
```

## Completion standard

A benchmark is complete only when all intended points have outcomes, all
reportable points pass exact-result checks, measured token counts support the
labels, runtime/model identity is recorded, requested and observed backends
are distinguished, safety guards did not invalidate results, all repeats stay
in the artifact, and both the model-level SQLite artifact and HTML report
validate. Otherwise call it diagnostic, partial, failed, or blocked.

## Repository checks

For code changes run the checks in `AGENTS.md`, including the one-case dry run.
For runner or artifact changes also run `scripts/check-crap.sh` and
`scripts/check-mutation.sh`.
