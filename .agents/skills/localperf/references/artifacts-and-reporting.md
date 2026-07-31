# Artifacts and reporting

LocalPerf's canonical result is a validated SQLite artifact. Raw run files help
debug and reproduce a run, but they are not accepted report inputs.

## File layout

Use one artifact and one HTML report per model:

```text
runs/models/<model-slug>.sqlite
runs/models/<model-slug>.html
```

A run also writes raw working data:

```text
runs/<run-id>/events.jsonl
runs/<run-id>/results/*.json
runs/<run-id>/logs/*.log
runs/<run-id>/summary.json
runs/<run-id>/datasets/*
```

Keep raw data until the model-level artifact is validated, rendered, backed up,
and reviewed. A model-level artifact may contain many run rows for baseline,
extension, retries, and repeat batches.

## Current format only

The accepted metadata is:

```text
format_name     localperf_run
format_version  1
```

LocalPerf accepts only the exact current v1 schema. It does not provide old-v1
readers, aliases, automatic migration, fallback columns, or run-directory
reconstruction. Re-run the measurement with the current `bench run` workflow
when an older artifact fails validation.

Do not edit an artifact with SQL to make it pass. A manual edit changes the
evidence and commonly breaks hashes, foreign keys, or provenance.

## Validation boundaries

LocalPerf validates at all of these boundaries:

- initial spec load;
- filtered execution spec;
- prepared dataset and normalized spec;
- immediately before each execution path;
- parsed result point;
- artifact write and append;
- artifact merge;
- artifact check;
- HTML render;
- viewer load.

`artifact check` verifies the schema and evidence, including:

- metadata and SQLite `user_version`;
- exact schema objects;
- foreign keys and integrity;
- one original and normalized spec per run;
- spec hashes;
- allowed workload roles;
- explicit context evidence;
- declared concurrency ladders;
- completed request and token counts;
- throughput fields and metric rows;
- detailed request rows when required;
- streaming evidence;
- telemetry units and values;
- embedded artifact hashes;
- payload-capture permission;
- valid final run status.

A file that merely opens in SQLite is not a valid LocalPerf artifact.

## Append behavior

Point every batch for the same model at the same destination:

```sh
localperf bench run \
  --spec batch.json \
  --artifact runs/models/model.sqlite
```

LocalPerf validates an existing artifact in full before appending. A new run ID
adds a run and its children. Re-running the same run directory replaces that
run's rows rather than duplicating them.

Use a new run directory when the spec, runtime, model revision, or material
server configuration changes. Append the new run to the same model artifact if
it is still part of the same model report.

## Merge behavior

Merge temporary valid artifacts with:

```sh
localperf artifact check runs/batch-1.sqlite
localperf artifact check runs/batch-2.sqlite

localperf artifact merge \
  --into runs/models/model.sqlite \
  runs/batch-1.sqlite runs/batch-2.sqlite

localperf artifact check runs/models/model.sqlite
```

Merges are idempotent. Existing run IDs are skipped when provenance matches. A
colliding run ID with different provenance is rejected. LocalPerf rescopes
run-owned IDs and preserves foreign-key relationships.

Do not merge different models into one model artifact merely to simplify a
comparison. Render one report per model, then compare validated model reports or
query their artifacts separately.

## Partial and resumed runs

The runner refreshes the artifact after each point. A long sweep can therefore
leave a valid partial artifact whose run status is still `running` or whose
measurements include failures and skips.

Render partial evidence when needed, but label it partial. Resume only with the
same run directory:

```sh
localperf bench run \
  --spec spec.json \
  --run-dir runs/<previous-run-id> \
  --resume \
  --artifact runs/models/model.sqlite
```

Resume skips result files that already contain completed valid points. It must
not be used to carry completed rows into a changed benchmark contract.

## Benchmark and diagnostic roles

Both workload roles are stored:

```text
benchmark
diagnostic
```

Only `benchmark` workloads enter report and comparison queries. A diagnostic
probe may preserve useful startup, endpoint, or kernel evidence, but it cannot
become a benchmark by changing its label after execution.

Artifacts record the request count resolved from `num_prompts` or `prompts_per_user`, and the checker verifies exact completed and failed request counts for each measurement.

## Core tables

The current artifact includes these main areas:

| Table | Contents |
| --- | --- |
| `metadata` | Format identity and artifact metadata. |
| `run` | One row per attempt or batch. |
| `specs` | Original and normalized spec bytes with hashes. |
| `engines` | Runtime and endpoint definitions. |
| `profiles` | Server profiles and capacity settings. |
| `workloads` | Role, phase, context, traffic, dataset, and request contract. |
| `measurements` | One profile/workload/concurrency/repeat result. |
| `metric_stats` | Latency, TTFT, TPOT, ITL, throughput, and memory distributions. |
| `requests` | Per-request timings, token counts, hashes, and errors. |
| `request_stream_events` | Optional streamed timing evidence. |
| `telemetry_series` | Named telemetry sources, metrics, targets, and units. |
| `telemetry_samples` | Time-series values. |
| `events` | Lifecycle and diagnostic records. |
| `commands` | Exact subprocess commands and outcomes. |
| `artifacts` | Embedded logs, raw results, telemetry, and report bytes. |
| `reports` | Rendered exports stored back into the artifact. |

Use normalized tables for analysis and embedded raw artifacts for verification.

## Safe inspection queries

Run validation first:

```sh
localperf artifact check runs/models/model.sqlite
```

List runs:

```sh
sqlite3 runs/models/model.sqlite '
  select id, name, status, started_at, completed_at
  from run
  order by created_at;
'
```

List benchmark measurements:

```sh
sqlite3 -header -column runs/models/model.sqlite '
  select
    r.name as run,
    p.name as profile,
    w.name as workload,
    m.repeat_index,
    m.concurrency,
    m.status,
    m.completed_requests,
    m.failed_requests,
    m.prompt_tokens,
    m.completion_tokens,
    m.aggregate_output_tok_s,
    m.per_user_output_tok_s
  from measurements m
  join run r on r.id = m.run_id
  join profiles p on p.id = m.profile_id
  join workloads w on w.id = m.workload_id
  where w.role = 'benchmark'
  order by p.context_window, w.phase, m.concurrency, m.repeat_index;
'
```

List failures and skips:

```sh
sqlite3 -header -column runs/models/model.sqlite '
  select
    p.name as profile,
    w.name as workload,
    m.concurrency,
    m.status,
    m.error_type,
    m.error_message
  from measurements m
  join profiles p on p.id = m.profile_id
  join workloads w on w.id = m.workload_id
  where m.status <> 'completed'
  order by p.name, w.name, m.concurrency;
'
```

Inspect actual request token shapes:

```sh
sqlite3 -header -column runs/models/model.sqlite '
  select
    p.name as profile,
    w.name as workload,
    m.concurrency,
    count(*) as requests,
    round(avg(q.prompt_tokens), 1) as avg_prompt_tokens,
    round(avg(q.completion_tokens), 1) as avg_completion_tokens,
    min(q.prompt_tokens) as min_prompt_tokens,
    max(q.prompt_tokens) as max_prompt_tokens,
    min(q.completion_tokens) as min_completion_tokens,
    max(q.completion_tokens) as max_completion_tokens
  from requests q
  join measurements m on m.id = q.measurement_id
  join profiles p on p.id = m.profile_id
  join workloads w on w.id = m.workload_id
  where q.status = 'completed'
  group by p.name, w.name, m.concurrency;
'
```

Queries are for review, not for rewriting evidence.

## Metric interpretation

### Context

`profiles.context_window` is the server limit. It is not the active context.

For each request:

```text
active_start = prompt tokens
active_end = prompt tokens + completion tokens
active_average = prompt tokens + completion tokens / 2
```

`context_target` refers to active end for active-context workloads. Reports show
a verified context target only when measured active end remains in the 90% to
100% target band. Long-output decode rows should show the measured active range,
not one constant context value.

### Throughput

Keep these separate:

- aggregate output tok/s: all generated tokens divided by point wall time;
- per-user output tok/s: user-level view at the declared concurrency;
- aggregate total tok/s: prompt plus completion tokens divided by wall time;
- request throughput: completed requests per second.

Do not describe aggregate cN throughput as single-user generation speed.

### Latency and token timing

- latency is end-to-end request time;
- TTFT is time to first streamed token;
- TPOT is the request-weighted mean of each request's output-token intervals;
- ITL is token-weighted over inter-token gaps.

TPOT and ITL use different weighting and are not interchangeable. A first byte
without a token event is not TTFT.

For a streamed full generation run, report effective prefill from the saved
request evidence. Per-request effective prefill is prompt tokens divided by
TTFT. Aggregate effective prefill is all prompt tokens in the concurrent batch
divided by the time from the earliest request start to the latest first token.
This includes queueing, scheduling, API overhead, and overlapping decode work.
Keep it separate from isolated prefill-only throughput.

Report per-request decode speed as 1,000 divided by TPOT in milliseconds. Keep
that value separate from end-to-end output throughput, which includes TTFT.

### Goodput

Goodput appears only when a workload declares an SLO. It is the number of
requests per second that meet every declared latency target. High throughput
with low goodput means the server completed work too slowly for the stated
service goal.

### Memory

Use precise labels:

- whole-machine `MemAvailable` drop;
- minimum available memory;
- process/cgroup current and peak memory;
- runtime-reported KV-cache bytes, tokens, and concurrency;
- named GPU or platform telemetry source.

On unified-memory systems, do not call system memory pressure VRAM. Report
conflicting signals rather than selecting the most favorable one.

## Rendered reports

Render only after full validation:

```sh
localperf artifact render \
  --output runs/models/model.html \
  runs/models/model.sqlite
```

The report intentionally excludes diagnostic workloads. It shows declared
trim decisions, adaptive skips, failures, repeats, and context mismatches. Do
not edit the HTML to hide them. Fix the source benchmark or explain the result.

Use `--store` when the report bytes should also be embedded in the artifact:

```sh
localperf artifact render --store runs/models/model.sqlite
localperf artifact check runs/models/model.sqlite
```

## Comparison checklist

Before comparing model artifacts, confirm:

- each artifact passes current validation;
- model and runtime identities are complete;
- workloads have role `benchmark`;
- active token shapes match;
- output lengths and finish behavior match;
- concurrency and server admission limits match;
- cache conditions match;
- repeats and aggregation match;
- safety events and failed requests are included;
- metrics use the same definitions and units.

Present:

- raw completed and failed request counts;
- exact average prompt and completion tokens;
- every repeat or a link to the artifact rows;
- median or declared center, mean, and spread;
- absolute and percentage difference;
- memory and operational tradeoffs;
- any uncertainty or mismatch.

Treat an uncertain or operationally unimportant lead as a tie. Do not infer a
production decision from a diagnostic probe or one automatic chart maximum.

## Privacy review

Before sharing an artifact or report:

- inspect commands and environment metadata;
- inspect embedded server logs and raw responses;
- confirm common secret fields were redacted;
- search for tokens, passwords, usernames, hostnames, private paths, prompts,
  and response text;
- confirm payload capture was explicitly enabled if prompt or response bodies
  are present.

LocalPerf stores timing and hashes by default. Prompt and response payloads
require explicit capture permission. Redaction is a safeguard, not a substitute
for reviewing the final file.

## Completion record

Record these paths and facts at the end:

```text
spec:
run directory:
model SQLite artifact:
HTML report:
LocalPerf revision:
model revision:
runtime revision:
backend attestation evidence:
completed/failed/skipped points:
memory guard events:
extension decision:
artifact check result:
```

A valid artifact plus a clear completion record is the handoff surface for
future reruns and comparisons.
