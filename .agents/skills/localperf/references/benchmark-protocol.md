# Benchmark protocol

Use this protocol before spending GPU time or reporting LocalPerf results.

## Define the question

Write down what the run should answer before selecting a grid:

- maximum aggregate throughput;
- c1 per-request decode speed;
- active-context scaling;
- fresh-prefill latency and throughput;
- concurrency capacity;
- queueing behavior;
- SLO goodput;
- memory fit or context ceiling;
- a matched comparison between runtimes, quantizations, or models.

Do not mix these into one headline. Capacity, c1 latency, aggregate throughput,
and fresh prefill are separate results.

For comparisons, declare in advance:

- models and immutable revisions;
- runtime versions or commits;
- quantization and expected kernels;
- context and token shapes;
- sampling settings;
- concurrency and server sequence capacity;
- number of repeats;
- aggregation rule;
- minimum worthwhile difference;
- invalidation and stop conditions.

Use the median or a declared aggregate across all valid repeats. Report the
mean, spread, raw sample count, and failures as well. Never choose the fastest
repeat after seeing the data.

## Default sweep

The normal baseline has two families.

### Maximum-throughput reference

This is a 4k-capacity profile with a shorter workload selected to seek high
aggregate token throughput. It answers a capacity/throughput question. It does
not establish active 4k performance.

Keep this family visibly separate from practical active-context rows.

### Practical active-context sweep

Use active targets:

```text
4k, 8k, 16k, 32k, 64k
```

Use client concurrency:

```text
1, 4, 8, 16, 32
```

Run both phases per target:

- prefill: nearly all input and 1 output token;
- decode: long input and up to 1,024 output tokens.

The generator uses:

```text
headroom = max(64, target / 64)
prefill input = target - headroom
prefill output = 1
decode output = min(1024, target / 4)
decode input = target - decode output - headroom
```

At 4k, the active decode shape is about 3,008 requested input tokens and 1,024
output tokens. This is the correct practical 4k decode point. A 42-token prompt
plus 1,024 output tokens is a short-context diagnostic even when the server can
accept 4k or more.

For each point, the request count is:

```text
prompts_per_user * concurrency
```

The generated default `prompts_per_user` is 2. Override it when the benchmark calls for a different number of requests per concurrent batch.

Use at least three repeats for a comparison unless cost or run time makes that
impractical. If fewer repeats are used, state the limitation. LocalPerf's
planner default is one repeat, so set `--repeats` deliberately.

## Practical c1/c6 64k sweep

When the user requests `practical-c1-c6-64k`, follow
[the fixed sweep definition](../../../../docs/2026-07-31-practical-c1-c6-64k.md). It replaces the default grid for that run. Set `prompts_per_user: 1`: c1 contains one request and c6 contains six requests, repeated three times. Do not add other context lengths, concurrency values, reference workloads, or prefill-only points.

## Extension decision

A baseline is incomplete without a recorded decision about extension.

Extend concurrency by powers of two when the largest safe point still shows
useful throughput and acceptable latency:

```text
64, 128, 256, 512
```

Extend active context by powers of two when the server and model support it:

```text
128k, 256k, 512k, 1m
```

Change one dimension at a time. Normal planning caps contexts of 128k or more at
c4. Higher concurrency there is a separate stress experiment.

Stop with a recorded reason when:

- the previous point failed;
- the memory guard fired;
- throughput flattened below the declared useful gain;
- tail latency or SLO goodput became unacceptable;
- queueing dominates;
- the server-reported context or KV limit is reached;
- the operator imposed a documented cap.

An adaptive skip applies only to that profile/workload ladder. Review other
phases and contexts before declaring a global limit.

## Runtime and model provenance

Before launching, record:

- model owner, repository, exact revision, and file hashes when available;
- runtime owner, source repository, release, commit, executable path, package
  lock, or container digest;
- command and all server arguments;
- driver, CUDA or accelerator stack, compiler settings, and architecture flags;
- tensor parallelism and device count;
- quantization, KV-cache dtype, attention backend, linear backend, MoE backend,
  and speculative decoding settings;
- tokenizer identity and served model name.

Use the canonical runtime or pinned official upstream release unless the user
explicitly approves another source. Resolve runtime availability with the
`manage-runtimes` prebuilt-first gate. A benchmark request does not authorize a
community image, fork, patch, custom build, or any source build, including one
from official upstream source.

Do not mutate an incumbent runtime in place. Create a separately identified
candidate when an approved experiment needs a change. Never promote a candidate
or delete the incumbent merely because one probe is faster.

## Backend attestation

Configuration flags, successful imports, capability checks, startup logs, and
health checks prove only intent or availability. Require evidence from at least
one real measured request that the expected path executed.

Use the strongest available source:

- kernel or backend trace tied to the request;
- runtime debug log naming selected implementations;
- profiler trace;
- an engine's explicit per-layer/backend report;
- a narrow instrumented canary in an approved candidate runtime.

Preserve the evidence in or beside the run artifact with checksums. If any
relevant layer uses fallback, emulation, another backend, or a package different
from the declared environment, invalidate the point. Stop instead of silently
switching to a working backend.

## Prompt and cache discipline

Prefix caching can make repeated prompts look much faster. For fresh-prefill or
matched active-context comparisons:

- use prompts with different first cache blocks;
- warm the engine with unrelated text that cannot share the benchmark prefix;
- verify prompt hashes or cache metrics;
- preserve all prompt hashes in request rows;
- keep cached-prefix tests in separate workloads with explicit labels.

Random request IDs do not prove distinct prompt content. Inspect the generated
or materialized dataset.

The current `localperf_http` random path repeats the same synthetic prompt text.
Do not report its TTFT as fresh prefill. Use a compatible LocalPerf path that
creates distinct prompts or stop until the dataset contract is extended.

For forced-length decode:

- set deterministic sampling, normally temperature 0;
- set the intended maximum output tokens;
- use `ignore_eos` when the runtime supports it and forced length is part of the
  protocol;
- verify endpoint usage and finish reasons;
- reject early stops instead of scaling or estimating their throughput.

A request for 1,024 tokens is not evidence that 1,024 tokens were generated.

## Safety preflight

Before loading a large model:

- identify the exact target process and command;
- check current inference processes and occupied ports;
- record `/proc/meminfo` `MemAvailable` and swap;
- check whether the platform shares CPU and GPU memory;
- choose a whole-machine `min_mem_available_gib` floor;
- keep the machine's memory protection active;
- estimate model, runtime, KV-cache, and concurrency memory;
- choose a startup timeout and workload timeout;
- confirm where partial evidence will be written;
- confirm how managed processes will stop after cancellation or failure.

Use about 80% of usable memory as an upper planning bound unless the user asks
for another limit. On unified-memory hardware, GPU memory counters alone are
not enough. Use whole-machine `MemAvailable`, process/cgroup memory, runtime KV
capacity, swap, and platform telemetry as separate signals.

Never disable memory guards or lower the floor after a failure merely to obtain
a result. A guard event makes the affected performance point invalid, but the
failure and telemetry remain useful evidence. If an unrelated workload caused
the guard event, do not retry while that workload remains unchanged. Report the
blocker or ask permission to pause it.

## Dry run

A dry run checks:

- strict spec parsing;
- profile/workload filtering;
- context and sample contracts;
- plan generation;
- run-directory layout;
- SQLite schema and persistence;
- reportability roles;
- artifact validation and rendering.

A dry run does not check:

- model loading;
- server flags;
- endpoint compatibility;
- tokenizer behavior;
- actual prompt lengths;
- backend selection;
- memory fit under model load;
- throughput or latency.

Never turn dry-run rows into performance results.

## Real canary

Run the smallest useful real point before the grid. Label the canary diagnostic when it is not part of the declared benchmark.

Check:

1. The server identity matches the model and runtime.
2. The health and model endpoints return the expected served name.
3. One complete request produces exact usage counts.
4. Streaming produces token events when TTFT is needed.
5. Prompt and completion counts support the context claim.
6. The output reaches the required length.
7. No request, server, safety, or telemetry error occurred.
8. The expected backend executed.
9. The result JSON has one canonical object and all required fields.
10. The SQLite artifact passes `localperf artifact check`.

Stop immediately when one condition fails.

## Measurement review

After each point or resumed batch, review:

- planned, completed, failed, skipped, and canceled counts;
- exact prompt, completion, and total token counts;
- aggregate output and total token throughput;
- per-user output throughput;
- latency p50/p95/p99;
- streamed TTFT p50/p95/p99;
- TPOT and ITL with their different weighting;
- system memory floor and minimum observed headroom;
- process/cgroup peak;
- runtime KV-cache capacity;
- GPU utilization, memory, temperature, and power where available;
- server errors, fallback messages, and finish reasons;
- repeat-to-repeat spread.

Do not call first-byte time TTFT. LocalPerf reports TTFT only from streamed
first-token evidence.

## Invalidation conditions

A point is not reportable when any of these applies:

- wrong model, revision, runtime, quantization, or backend;
- unapproved community runtime or patch;
- fallback, emulation, or unexpected kernel;
- missing or contradictory runtime identity;
- any failed request in a point expected to complete exactly;
- completed request count differs from the plan;
- missing prompt, completion, or total token counts;
- output ended before the forced length;
- measured context contradicts the active-context claim;
- warmed or repeated prefix presented as fresh prefill;
- memory guard, OOM, server crash, or timeout affected the measurement;
- cherry-picked repeats or dropped outliers;
- diagnostic workload presented as a benchmark;
- raw JSON or a run directory used as the reporting source;
- artifact validation fails.

Store invalid evidence with a plain reason. Do not delete it and rerun without
recording what changed.

## Comparison rules

Compare like with like:

- same active token shape, not merely the same server limit;
- same request count and concurrency;
- same output length and finish behavior;
- same sampling and reasoning/tool settings;
- same prefix-cache condition;
- c1 per-request values against c1 values;
- aggregate multi-request values against the same concurrency;
- same repeat aggregation;
- same hardware and safety constraints.

Report absolute effects and raw counts before percentages. Account for spread
and failed requests. Treat uncertain or immaterial differences as ties and
prefer the cheaper, simpler, safer setup unless the user selects another
tradeoff.

Do not let an automatic maximum, chart winner, or single metric decide a
runtime promotion or additional spending.
