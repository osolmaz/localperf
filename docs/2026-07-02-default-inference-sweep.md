---
title: Built-in inference suites
author: Onur Solmaz
date: 2026-07-02
---

# Built-in inference suites

LocalPerf keeps different benchmark questions in separate named suites. It
does not generate a combined grid and does not add reference or stress cases
behind flags.

## `practical-64k`

The practical suite contains only generation from minimal and near-full 64k
context. It runs `generate-empty` and `generate-full` at concurrency 1 and 6,
with three repeats. The four cases produce exactly 12 measurements. Each batch
contains exactly one request per concurrent slot.

Each valid generation measurement populates both decode and effective-prefill
columns in the established report table. There is no prefill-only case and no
4k reference in this suite. See
[Practical c1/c6 64k](2026-07-31-practical-c1-c6-64k.md).

## `throughput-4k`

This is the separate 4k aggregate-generation-throughput suite. Use it when the
question is throughput at 4k capacity. Do not attach it to a practical run or
describe it as an active-context result from another suite.

## `context-ladder`

This suite contains dedicated prefill and decode cases at active context
targets `4k`, `8k`, `16k`, `32k`, `64k`, and `128k`, using explicit
concurrency/request batches at `1`, `4`, `8`, `16`, and `32`; the 128k cases
stop at c4.

## Execution

The suite owns the cases, token shapes, exact request batches, and repeats. A
deployment JSON file owns the model revision, pinned runtime, requested
backends, server/client options, and safety floor. LocalPerf derives server
context and sequence limits from the selected suite cases.

When a deployment requests a concrete attention or MoE backend, LocalPerf
profiles a separate canary matching every selected case and concurrency point,
then reads the emitted CUDA execution table before accepting that point.
Missing or mismatched kernel evidence stops the run. The profiler is inactive
during timed measurements.

```sh
localperf bench run \
  --suite practical-64k \
  --deployment deployment.json \
  --artifact runs/models/<model-slug>.sqlite
```

Use `--dry-run` to validate and write `suite.json`, redacted
`deployment.json`, and `execution-plan.json` without launching a server. Use
repeatable `--case` and `--concurrency` filters only for a deliberate subset.

All attempts for one model belong in one model-level SQLite artifact and one
rendered HTML report. Do not split contexts, retries, or concurrency points
into separate final artifacts.
