---
title: Default Inference Sweep
author: Bob <dutifulbob@gmail.com>
date: 2026-07-02
---

# Default Inference Sweep

Use this as the default localperf sweep shape when the user asks to characterize
an inference engine across context length and concurrency.

## User Instruction

The original benchmark instruction was:

```text
I want to create 2 reference points

max possible throughput we can have, even though not useful. min 4k context window. maximize concurrency and token throughput we can get

then set to 8k, 16k, 32k, 64k and 128k context window, and try to see how concurrent we can get. try to maximize concurrency

note per worker throughput and total aggregate throughput in each case

try to use as much memory we can use. like 80% of the memory. without ooming the device

first document this the type of benchmark we aim for. then execute the benchmark without OOM'in the device
```

## Baseline Grid

Start with two benchmark families:

- `max-throughput-reference`: minimum `4k` context, intentionally optimized for
  maximum aggregate token throughput even if the setting is not practically
  useful. These are capacity points (`context_semantics: "capacity"`).
- `practical-context-sweep`: **active** context points `4k`, `8k`, `16k`,
  `32k`, and `64k`, with concurrency `1`, `4`, `8`, `16`, and `32` where the
  hardware can safely run them. `128k` is opt-in and capped at `c4`; at that
  KV budget, higher concurrency is an hours-long stress exercise, not a
  baseline grid point.

The baseline grid is the first pass, not the end of the benchmark. After it
finishes, make an explicit extension decision:

- extend the same artifact when the largest completed point still shows useful
  throughput or acceptable tail latency,
- stop only when the result already shows a clear limit, such as failures,
  memory pressure, queueing-dominated latency, or a documented operator cap,
- record the reason when you stop instead of silently treating `c32` or `64k`
  as a final boundary.

`4k-reference` is not a substitute for the active `4k` sweep point. A default
sweep should include both: the reference row for toy/max-throughput comparison
and active `4k` prefill/decode rows for the regular context ladder.

The ladder points mean active context, not server capacity: a `32k` point must
actually move ~32k tokens per request through the KV cache. Setting
`max_model_len=32768` while requesting a ~1k prompt is a capacity experiment,
not a 32k-context data point; see `2026-07-02-context-semantics.md` for the
contract and validation rules.

Each active-context point `N` includes both workload shapes, with input
lengths that track `N`:

- `prefill`: long prompt, minimal output, measured through TTFT. Default
  shape: `input = N - headroom`, `output = 1`. Keep output at 1 (at most a
  few tokens); anything larger spends run time decoding and pollutes the
  prefill numbers.
- `decode`: long prompt, `1024` generated tokens. Default shape:
  `output = min(1024, N/4)`, `input = N - output - headroom`. 1024 tokens is
  still hundreds of steady-state decode steps, and the shorter output keeps
  the measured active range close to `N`. A decode row sweeps an active
  context range from `input` up to `N` as output tokens accumulate; reports
  label it with that range per `2026-07-02-context-semantics.md`.

with `headroom = max(64, N/64)` to absorb chat template and tokenizer drift.
Prefill rows are reported as aggregate and per-user prefill tok/s; decode rows
as aggregate and per-user output tok/s.

Request counts scale with concurrency: `prompts_per_user` (default 2) gives
`num_prompts = 2 x concurrency` per point, so `c1` points stop paying for
`c32`-sized sample counts. Override the default when the benchmark calls for a different batch size.

Long-output behavior is a stress preset, not the baseline:
`localperf sweep plan --stress` adds `4096`-token decode spot checks at
`32k c4` and `64k c1/c4` plus the `128k` points at `c1/c4`.

Default sweeps must be generated with `localperf sweep plan`, for example:

```sh
localperf sweep plan --model <model-id> \
  --contexts 4k,8k,16k,32k,64k --concurrency 1,4,8,16,32 \
  --out spec.json
```

Hand-authored specs stay legal but must declare `context_target` and
`context_semantics` on every workload and pass the same validation.

## Extension Rule

The default grid is not a hard ceiling. If the hardware has clear memory and
latency headroom, continue the same ladder with further powers of two. The
extension pass is part of the default workflow unless the baseline gives a
concrete reason to stop.

Extend one dimension at a time:

- concurrency: `64`, `128`, `256`, `512`, and onward,
- context: `128k`, `256k`, `512k`, `1m`, and onward.

Only extend one dimension at a time, and stop before the machine OOMs. A failed
startup or memory-guard kill is a result; do not keep pushing the same shape
without changing the profile.

Think of the sweep as tiers:

| Tier | Purpose | Typical contexts | Typical concurrency |
| --- | --- | --- | --- |
| Baseline | characterize normal active-context behavior | `4k,8k,16k,32k,64k` | `1,4,8,16,32` |
| Throughput extension | find the aggregate decode knee | best baseline contexts, usually `32k,64k` | `64,128,256,512` |
| Long-context extension | find the supported context ceiling | `128k,256k,512k,1m` | `1,4` |
| Long-context stress | test queueing and tail latency after long context works | largest working context | `8,16,32+` |

The tiers append to the same model-level artifact. Do not flatten them into
separate reports; the point is to see the baseline, extension, and stop reason
together.

Concurrency extension is usually the first follow-up after a clean baseline:

```sh
localperf sweep plan --model <model-id> \
  --contexts 32k,64k --concurrency 64,128,256,512 \
  --out spec-concurrency-extension.json
```

Use the highest-context decode rows first when the goal is maximum useful
aggregate generation throughput. Add prefill rows when the baseline prefill
ladder reached its largest concurrency without an adaptive skip or unacceptable
TTFT.

Long-context extension requires the server profile to support the requested
context. Do not send a `128k` active-context workload to a server started with
`--max-model-len 65536`; redeploy or start a larger-context profile first, then
run:

```sh
localperf sweep plan --model <model-id> \
  --contexts 128k,256k,512k \
  --concurrency 1,4 \
  --out spec-long-context-extension.json
```

The generator intentionally caps contexts `>=128k` at `c4` in normal planning.
Higher concurrency at those contexts is a deliberate stress run. If you force
`128k c8+` or `256k c8+`, keep it in a separate run batch appended to the same
model-level artifact, state the reason, and expect it to be hours-long or
queueing-dominated.

For a hosted OpenAI-compatible endpoint, verify the served context limit before
running the long-context tier:

```sh
curl -sS "$BASE_URL/v1/models" \
  -H "Authorization: Bearer $TOKEN" | jq '.data[0].max_model_len'
```

If the server fails to start at a requested context because the model's derived
maximum context is lower, record that startup failure as the context ceiling.
Do not override it with engine escape hatches such as
`VLLM_ALLOW_LONG_MAX_MODEL_LEN=1` for benchmark results unless the model owner
documents that longer RoPE scaling is valid.

After `128k` or larger works at `c1,c4`, a separate long-context concurrency
stress run can probe queueing:

```sh
localperf sweep plan --model <model-id> \
  --contexts 128k,256k --concurrency 8,16,32 \
  --out spec-long-context-concurrency.json
```

Useful extension presets:

- **Throughput hunt:** extend decode at the best completed contexts with
  `c64,c128,c256`; stop when output tok/s is flat or p95/p99 latency becomes
  unusable.
- **Tail-latency boundary:** repeat the best throughput context around the
  knee, for example `c16,c32,c64`, and add SLOs such as
  `{"ttft_p95_ms": 2000}` when a product latency target exists.
- **Long-context stress:** run `--stress` first, then extend `128k` or larger
  only after the server profile and KV budget are explicitly sized for it.
- **Endpoint queueing test:** for hosted endpoints, it is valid to run client
  concurrency above server `max_num_seqs`; label it as queueing behavior, not
  extra in-engine parallelism.

## Model-Level Artifacts

For a specific model, keep repeated runs in one SQLite artifact and 1 HTML
report per model. Use model-scoped paths such as:

```text
runs/models/<model-slug>.sqlite
runs/models/<model-slug>.html
```

The shared SQLite file should contain one `run` row per benchmark attempt or
batch, with all profiles, workloads, measurements, requests, telemetry, raw
outputs, and report exports tied back by `run_id`. Do not split `c1`, `c4`,
`8k`, `16k`, or separate retry attempts into separate SQLite files unless the
split is only for debugging or recovering from a failed run.

Point every batch at the model-level artifact and the runner appends:

```sh
localperf bench run --spec spec.json --artifact runs/models/<model-slug>.sqlite ...
```

Existing per-run artifacts combine with:

```sh
localperf artifact merge --into runs/models/<model-slug>.sqlite runs/batch-1.sqlite runs/batch-2.sqlite
```

Re-running the same run directory replaces that run's rows; merging an
already-present run is skipped. Render the HTML from the shared SQLite
artifact after each batch; the report lists every run and aggregates repeated
points across runs with mean ± spread.

The final step of every default sweep is to render the completed SQLite
artifact into 1 HTML report per model. Do not call the sweep complete until:

- the baseline grid is recorded,
- the extension decision is recorded, either as appended extension runs or as a
  concrete stop reason,
- the model-level SQLite artifact and the matching model-level HTML report both
  exist.

## Reporting Requirements

Every row must record:

- model and inference engine,
- exact engine profile, including context limit and concurrency cap,
- workload name and token shape,
- requested concurrency and completed/error count,
- aggregate throughput and per-user throughput,
- average TTFT and p95 TTFT,
- memory headroom or peak memory pressure,
- exact artifact path for the raw result.

Report context and concurrency together. The primary comparison table should make
it easy to answer: "At this context length, how many users can we run, and what
aggregate and per-user throughput do they get?" Context labels in reports must
follow `2026-07-02-context-semantics.md`: label rows by declared-and-measured
active context or by measured token shape, never by `max_model_len` alone.

Reports must also include a short extension note:

- which points were extended beyond the baseline grid,
- which points were not extended and why,
- whether higher client concurrency exceeded server admission limits such as
  `max_num_seqs`.

## Safety Rules

Use at most about 80% of usable memory unless the user explicitly asks to go
higher. Keep the machine responsive and use memory guards where available.

Prefer a sparse search first:

- start with `c1`, then `c4`, `c8`, `c16`, `c32`,
- skip higher concurrency once memory, TTFT, or error rate makes the profile
  impractical,
- confirm near the best working point with the exact production-style profile.

The runner enforces this automatically (`runner.adaptive`, on by default):
per profile and workload, concurrency runs ascending and higher points are
skipped, with the reason recorded, when throughput improves less than 10%
over the previous point, a point's TTFT p99 exceeds a configured ceiling
(`ttft_p99_ceiling_ms`, off unless set), the previous point failed, or the
concurrency exceeds 2x vLLM's reported maximum concurrency. Disable with
`"runner": {"adaptive": {"enabled": false}}`; negative thresholds disable
individual rules.

An adaptive skip inside the baseline grid is not a global stop signal. It only
means that one workload/profile point hit a local rule. After the baseline,
review the completed rows and still extend the contexts or phases that reached
the largest tested concurrency cleanly.

Long sweeps are crash-tolerant: the artifact is refreshed after every point
(run status `running` until the sweep finishes), so partial results render at
any time, and `bench run --resume` with the same `--run-dir` skips points
whose results already completed.

Do not call a sweep complete until all completed, skipped, and failed cases are
recorded with enough metadata to reproduce the run.

## Spec Provenance

Prefer creating sweep specs through the generator:

```sh
localperf sweep plan \
  --model <model-id> \
  --vllm-command /path/to/runtime/bin/vllm \
  --gpu-memory-utilization 0.4 \
  --kv-cache-memory-bytes 12884901888 \
  --trim 64k=8:'12 GiB KV budget' \
  --out spec.json
```

Machine-specific runtime choices for managed vLLM runs (vLLM path, GPU memory,
KV budget, extra serve args) are generator flags, so normal local sweeps should
not require editing the output. A deliberate concurrency cap is a declared trim:
`--trim <context>=<max>:<reason>` removes the higher points from the grid,
records the decision in the spec, and reports render the trimmed points like
adaptive skips — with the reason, never as silent holes.

Some runs are necessarily adapted after generation: deployed endpoints,
non-vLLM servers, custom health routes, auth, or extension points that the
generator intentionally caps as stress. Those specs are valid as custom grids
when they still declare `context_target` and `context_semantics`, pass
validation, and document what was changed. Append those runs to the same
model-level artifact rather than creating a separate final artifact.

Generated specs carry a `generator` stamp (tool, intent, content hash).
`bench run` verifies the hash and prints the provenance; reports and the
viewer label the run "Generated default sweep" only when the hash still
matches. A spec without a stamp, or edited after generation, runs fine but
is labeled "Custom grid" — the label is verified from the stored spec bytes,
never taken on trust.
