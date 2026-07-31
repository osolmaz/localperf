---
title: Full-run timing and prefill reporting implementation plan
author: Bob <dutifulbob@gmail.com>
date: 2026-07-31
---

# Full-run timing and prefill reporting implementation plan

LocalPerf already stores the data needed to report TTFT, decode speed, and practical prefill speed from a full-context generation run. The current SQLite writer drops the streamed-TTFT source marker when it reparses vLLM Bench results. The report then hides valid TTFT values and cannot derive prefill speed from them.

This plan fixes the writer and adds full-run timing metrics to the report. The existing GGUF benchmark will be rebuilt from its saved result files. No model inference will run again.

## Outcome

A completed full-context generation row will report these separate measurements:

- end-to-end output throughput for the whole benchmark.
- effective prefill throughput from request start through the first generated token.
- effective prefill throughput per request.
- TTFT mean, p50, p95, and p99.
- decode speed per request from TPOT.
- end-to-end request latency.

The names must stay explicit. Effective prefill includes queueing, scheduling, API overhead, and any decode work that overlaps chunked prefill. It measures application-level performance. Isolated kernel prefill throughput requires a separate workload.

## Evidence already available

The validated GGUF artifact is:

```text
/home/onur/scratch/qwen36-practical-headroom-20260731/models/unsloth-qwen36-35b-a3b-gguf.sqlite
```

It contains 42 request rows. Every row is marked as streamed and has a measured `ttft_ms`. The artifact also contains `request_ttft` statistics for all 12 measurements, raw vLLM Bench result JSON, commands, run events, server logs, and GPU and memory telemetry.

The missing data is a provenance marker on each measurement. `measurements.metadata_json` should contain:

```json
{"ttft_source":"stream"}
```

The SQLite writer currently reparses each result and applies event fields, but it does not apply the workload fields that add this marker. The request data is intact.

## Metric definitions

### Effective prefill per request

For each completed streamed request `i`, use:

```text
request effective prefill tok/s = prompt_tokens_i / (ttft_ms_i / 1000)
```

The metric requires a positive prompt-token count and a positive streamed TTFT. Requests without both values do not enter the distribution.

### Effective aggregate prefill

For one measurement, use all completed streamed requests:

```text
measurement effective prefill tok/s =
    sum(prompt_tokens_i) /
    (latest_first_token_time - earliest_request_start_time)
```

For streamed responses, `first_byte_at` is the first-token timestamp. The writer will also store that value in `first_token_at`, whose column already exists in the schema.

This formula measures how quickly the system takes the concurrent batch from its first request start until every request has produced a first token. At concurrency 1 it reduces to prompt tokens divided by TTFT.

Across repeats, the report will show the mean and spread of the per-measurement values. It will not recompute one number from summed tokens and summed orchestration time.

### Decode speed per request

For each request with positive TPOT:

```text
decode tok/s per request = 1000 / tpot_ms
```

The report will show the distribution across requests. It will keep the existing end-to-end aggregate output throughput as a separate metric. The report must not label completion tokens divided by total request latency as decode speed.

## SQLite writer changes

The SQLite schema already has the needed columns and generic metric table, so this work does not require a schema version change.

In `internal/vllmbench/sqlite_artifact.go`:

1. Apply the planned workload fields to each reparsed result before inserting the measurement. This preserves `ttft_source: stream` for vLLM Bench.
2. Check detailed request evidence when it is present. Completed requests contributing TTFT must be streamed and have positive TTFT values.
3. Store `first_token_at` from the streamed first-byte timestamp.
4. Add request effective prefill statistics to `metric_stats`.
5. Add one measurement effective prefill value to `metric_stats` when every required timestamp and token count is present.

The writer will omit an unavailable metric. It will never estimate one from server capacity or requested token lengths. Conflicting detailed evidence will fail artifact creation.

## Report and viewer changes

The viewer will keep pure prefill workloads separate from full generation runs. A new full-run timing table will show:

- concurrency.
- end-to-end aggregate output tok/s.
- effective aggregate prefill tok/s.
- effective prefill tok/s per request.
- TTFT mean and p99.
- decode tok/s per request from TPOT.
- latency p95.
- completed and failed requests.

The detailed measurement table will expose the same fields. Cell details will state the formula and evidence source.

The existing prefill columns remain reserved for workloads whose phase is `prefill`. A full generation row will not be presented as an isolated prefill benchmark.

## Evidence retention

`save_detailed: true` remains required for these full-run metrics. LocalPerf will continue to store:

- original and normalized specs with hashes.
- exact commands and run events.
- raw benchmark JSON.
- per-request token counts and timings.
- TTFT, TPOT, ITL, latency, and throughput statistics.
- server logs.
- GPU and host-memory telemetry.
- payload hashes and artifact hashes.

Prompt and response text remain opt-in because they can be large or sensitive. A random vLLM Bench workload cannot store text that the benchmark client does not return. Its raw detailed timing arrays remain embedded in the artifact.

## Tests

Add regression coverage for these cases:

- A vLLM Bench result with streamed TTFT writes `ttft_source: stream`.
- The same result stores `first_token_at` for every completed request.
- A c1 full request reports prompt tokens divided by TTFT as effective prefill.
- A staggered c6 batch uses the earliest request start and latest first-token time.
- A non-streamed result does not report TTFT or effective prefill.
- Missing token or timing evidence leaves the metric unavailable.
- Conflicting stream evidence fails artifact creation.
- The viewer shows full-run prefill and TPOT-derived decode values with their exact labels.
- Repeat summaries retain mean and spread instead of hiding variation.

Run the full Go test suite, `go vet`, SimpleDoc, Slophammer, the CRAP check, and the mutation check before committing.

## Existing artifact rebuild

After the code passes its checks, rebuild the three saved GGUF run slices with `localperf bench run --resume`:

```text
/home/onur/scratch/qwen36-practical-headroom-20260731/runs/gguf-empty
/home/onur/scratch/qwen36-practical-headroom-20260731/runs/gguf-full-c1
/home/onur/scratch/qwen36-practical-headroom-20260731/runs/gguf-full-c6
```

Resume processing happens before server startup when every result already exists. It will re-import the saved JSON into the model-level SQLite artifact without launching llama.cpp or sending a request.

Then run `localperf artifact check`, render the HTML report, and restart the temporary Tailscale viewer. Verify that all 42 requests remain present, all output lengths remain 1,024, TTFT is visible, and the new full-run prefill values match the request evidence.

## Boundaries

This work changes LocalPerf reporting and artifact writing only. It will not change benchmark results, inference runtimes, model files, workload prompts, concurrency, output lengths, or safety guards. It will not patch SQLite by hand or add a compatibility reader for artifacts missing the source marker.
