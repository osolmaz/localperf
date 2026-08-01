---
title: Generation prefill reporting implementation plan
author: Bob <dutifulbob@gmail.com>
date: 2026-07-31
---

# Generation prefill reporting implementation plan

LocalPerf already stores the request evidence needed to report decode and
effective prefill from one streamed generation measurement. The final report
must present those values through the established LocalPerf throughput table.
It must not add a second headline table or invent a replacement vocabulary.

This plan preserves the useful SQLite evidence work completed on 2026-07-31
and replaces the rejected report presentation. Existing saved results can be
re-imported and rendered without running model inference again.

## Final desired result

Running the `practical-64k` suite produces four reportable points:

- `generate-empty` at c1;
- `generate-empty` at c6;
- `generate-full` at c1;
- `generate-full` at c6.

Every one of those points shows both decode and prefill values in the same
headline row. There is no separate `Full-run timing` section.

The headline table uses the exact pre-2026-07-31 column contract, in this
order:

1. `Users`
2. `Decode tok/s`
3. `Decode/user`
4. `Decode TTFT avg`
5. `Decode TTFT p99`
6. `Prefill tok/s`
7. `Prefill/user`
8. `Prefill TTFT avg`
9. `Prefill TTFT p99`
10. `OK / Err`
11. `SLO / goodput`, only when configured

The report keeps the original context-grouped tables, heatmaps, shape badges,
metric-cell drill-downs, phase details, per-repeat evidence, runs, profiles,
events, commands, telemetry, artifact contents, privacy statement, and metric
legend.

Do not add headline columns named `E2E output`, `output share`, `generation
output`, `post-first-token`, `steady decode`, or `Repeats`. Do not prepend or
append a new full-run summary table.

## Column meanings for generation measurements

### Decode columns

For one completed measurement:

```text
Decode tok/s = total generated output tokens / measurement wall time
Decode/user = Decode tok/s / configured concurrency
```

`Decode TTFT avg` and `Decode TTFT p99` come from the request distribution of
streamed time to first token.

These are the established LocalPerf headline definitions. TPOT remains a
separate timing detail and does not replace either headline decode value.

### Prefill columns

For each completed streamed request `i`:

```text
request effective prefill tok/s =
    prompt_tokens_i / (ttft_ms_i / 1000)
```

For the aggregate measurement:

```text
Prefill tok/s =
    sum(prompt_tokens_i) /
    (latest_first_token_time - earliest_request_start_time)

Prefill/user = Prefill tok/s / configured concurrency
```

`Prefill TTFT avg` and `Prefill TTFT p99` use the same streamed TTFT
distribution. TTFT is both the decode-start boundary and the completion
boundary of this application-level effective prefill measurement, so it is
shown in both historical column groups.

Effective prefill includes queueing, scheduling, API overhead, and decode work
that overlaps chunked prefill. It is not isolated kernel prefill throughput.
The metric source and formula must appear in the legend and cell detail.

### All practical cases must be populated

Both `generate-empty` and `generate-full` must populate all Decode and Prefill
columns at c1 and c6 when their streamed evidence is valid.

The tiny `generate-empty` prompt makes its effective prefill value sensitive to
fixed request overhead. Report the value anyway, together with the measured
prompt shape and a clear limitation note. Do not hide it, replace it with `-`,
or treat it as an isolated prefill benchmark.

## Pure prefill workload precedence

The general LocalPerf report must continue to support ordinary sweeps that
contain separate decode and prefill workloads.

When a context/concurrency row has a valid dedicated prefill workload, that
workload remains the source of the Prefill columns. A derived effective prefill
value from a generation workload must not overwrite it.

When no dedicated prefill workload exists and the generation measurement has
valid streamed evidence, use its effective prefill values to populate the
Prefill columns. This is the path used by `practical-64k`.

## Evidence and artifact requirements

The metric requires:

- a completed benchmark measurement;
- detailed request rows;
- a positive measured prompt-token count for every contributing request;
- streamed TTFT provenance;
- a request start timestamp;
- a first-token timestamp later than the request start;
- exact completed and failed request counts.

The writer must omit a derived value when this evidence is unavailable. It
must never infer prompt tokens from server capacity or requested lengths.
Conflicting stream evidence must fail artifact creation.

The validated GGUF artifact currently used to exercise this plan is:

```text
/home/onur/scratch/qwen36-practical-headroom-20260731/models/unsloth-qwen36-35b-a3b-gguf.sqlite
```

It contains 12 measurements and 42 completed streamed requests. Every request
generated exactly 1,024 tokens. The saved request rows include prompt tokens,
TTFT, TPOT, request timestamps, raw result evidence, server logs, and telemetry.

## SQLite writer work to retain

The generic v1 schema already has the required columns and metric table. Keep
the writer changes that:

1. preserve `ttft_source: stream` when saved results are reparsed;
2. validate detailed streamed-TTFT evidence;
3. store the streamed first-token timestamp in `first_token_at`;
4. store request effective-prefill distributions in `metric_stats`;
5. store one aggregate effective-prefill value per measurement;
6. preserve original run and measurement timestamps and raw benchmark wall
   times during resume ingestion.

These changes do not require a schema-version change.

The request-level TPOT-derived decode distribution may remain in
`metric_stats` and the detailed report. It is not a new headline-table column.

## Report and viewer changes

Remove the rejected parallel presentation:

- remove `FullRunTimingRows` from the rendered report document;
- remove `full_run_timing` from the viewer response model;
- remove the `Full-run timing` HTML section and React component;
- remove tests that require that section;
- remove tests that forbid the established `Decode tok/s` and `Decode/user`
  headings.

Restore and extend the established throughput mapping:

1. Keep the original context-grouped row builder and headline column order.
2. Keep `Decode tok/s`, `Decode/user`, `Decode TTFT avg`, and
   `Decode TTFT p99` sourced as before.
3. If a dedicated prefill measurement is paired with the row, use it for the
   Prefill columns.
4. Otherwise, populate `Prefill tok/s`, `Prefill/user`, `Prefill TTFT avg`, and
   `Prefill TTFT p99` from the generation measurement's validated effective
   prefill and streamed TTFT evidence.
5. Apply the existing per-column heatmap logic to all populated Decode and
   Prefill cells.
6. Keep exact formulas, source labels, request shape, repeat count, and
   request-level distributions in the existing metric-cell details.
7. Keep TPOT, ITL, latency, RPS, total throughput, telemetry, and memory in the
   existing detail sections.

The standalone HTML report and interactive viewer must use the same report
model and show the same headline values.

## Tests

Add or revise regression coverage for:

- the exact headline column names and order;
- `p99`, not `p95`, in both Decode and Prefill headline groups;
- absence of a `Full-run timing` section;
- absence of invented headline columns;
- `generate-empty` c1 and c6 each showing Decode and Prefill values;
- `generate-full` c1 and c6 each showing Decode and Prefill values;
- `Prefill/user` equaling aggregate effective prefill divided by configured
  concurrency;
- Decode and Prefill TTFT using streamed mean and p99 evidence;
- a dedicated prefill workload taking precedence over derived prefill;
- missing or non-streamed evidence leaving derived prefill unavailable;
- conflicting streamed evidence failing artifact creation;
- repeat summaries retaining mean, sample spread, and every per-repeat row;
- the standalone report and viewer returning identical values.

Before committing production changes, run:

```sh
go test ./...
go vet ./...
npx -y @simpledoc/simpledoc check
go run github.com/osolmaz/slophammer/go/cmd/slophammer-go@v0.4.1 check .
scripts/check-crap.sh
scripts/check-mutation.sh
```

Then run the required one-case dry benchmark and validate its SQLite artifact.

## Existing artifact rebuild

After the implementation passes its checks, re-import these saved result
directories with their exact normalized specs and original run directories:

```text
/home/onur/scratch/qwen36-practical-headroom-20260731/runs/gguf-empty
/home/onur/scratch/qwen36-practical-headroom-20260731/runs/gguf-full-c1
/home/onur/scratch/qwen36-practical-headroom-20260731/runs/gguf-full-c6
```

Use `localperf bench run --resume` only after verifying every completed result
is present. Resume ingestion must finish before server startup and must not
send an inference request.

Then:

1. run `localperf artifact check`;
2. render the standalone HTML report;
3. start the temporary viewer;
4. verify 12 measurements, 42 completed requests, and 1,024 generated tokens
   per request;
5. verify that all four practical points show both Decode and Prefill values;
6. compare every headline value with the saved request evidence;
7. confirm the report contains no `Full-run timing` section or invented
   headline column.

## Boundaries

This work changes LocalPerf reporting and saved-result ingestion only. It does
not change the practical workload grid, prompts, concurrency, repeats, output
lengths, inference runtime, model files, or safety guards. It does not patch
SQLite by hand, add a compatibility reader, or authorize another inference
run.
