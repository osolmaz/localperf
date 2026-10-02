---
title: Adaptive convergence stops a point when its number is known
author: Onur Solmaz <2453968+osolmaz@users.noreply.github.com>
date: 2026-10-02
tags: [runner, statistics, suites]
---

# Adaptive convergence stops a point when its number is known

A suite case now runs a fixed number of repeats. Three repeats can be too many
for a stable point and too few for a noisy one. At 64k context one decode repeat
can take several minutes, so a fixed count wastes most of a run on points that
were already known after the first two repeats.

LocalPerf replaces the fixed repeat count with a convergence policy. Each point
repeats until its 95% confidence interval is narrow enough, until it reaches a
repeat limit, or until it reaches a time limit. The report shows which of the
three happened.

This is the same idea as AIPerf's `ci_width` convergence mode. LocalPerf keeps
only that one mode.

## Terms

- A **point** is one profile, workload, and concurrency.
- A **sample** is one measurement of a point: one batch, stored as one
  `measurements` row. Requests inside one batch run against the same server
  state, so they are not independent samples. Prompt nonces keep consecutive
  batches cold, so batches are close to independent. See
  [Prompt nonces](2026-09-18-prompt-nonces.md).
- The **sample value** is the throughput that `phaseThroughput` selects, the
  same value the adaptive ladder already uses. For decode cases it is the
  aggregate output throughput, `aggregate_output_tok_s`, which is the headline
  `Decode tok/s`: total generated output tokens divided by measurement wall
  time. For prefill cases it is the aggregate total throughput,
  `aggregate_total_tok_s`. Both values are stored on every `measurements` row.
  The headline decode value includes TTFT, as defined in
  [Full-run timing and prefill](2026-07-31-full-run-timing-and-prefill.md).

## Stopping rule

After each sample, with `n` samples, mean `m`, and sample standard deviation
`s`:

```text
half_width = t(n-1) * s / sqrt(n)
stop when    n >= min_repeats  and  half_width <= target_rel_half_width * m
```

`t(n-1)` is the two-sided 95% Student's t value. It is large when `n` is small
(4.303 at `n = 3`, 2.776 at `n = 5`), so a few samples that agree by chance do
not stop the point early. A plain coefficient-of-variation threshold does not
have this protection.

The confidence level is fixed at 95%. It is not configurable.

The t values come from a fixed table for 1 to 30 degrees of freedom:

```text
12.706 4.303 3.182 2.776 2.571 2.447 2.365 2.306 2.262 2.228
 2.201 2.179 2.160 2.145 2.131 2.120 2.110 2.101 2.093 2.086
 2.080 2.074 2.069 2.064 2.060 2.056 2.052 2.048 2.045 2.042
```

`max_repeats` must be at most 31, so the table covers every case and there is no
approximation beyond it. LocalPerf adds no statistics dependency.

## Stop reasons

Each completed point records exactly one stop reason:

| Reason        | Meaning                                                              |
| ------------- | -------------------------------------------------------------------- |
| `converged`   | The stopping rule fired.                                             |
| `max_repeats` | The point reached `max_repeats` and the rule did not fire.           |
| `time_budget` | The next sample would exceed `max_point_seconds`.                    |
| `fixed`       | `min_repeats == max_repeats` and there is no target.                 |
| `failed`      | A sample failed. See [Failed samples](#failed-samples).              |

The time limit never interrupts a running sample. Before a new sample starts,
LocalPerf estimates its duration as the mean duration of the previous samples of
that point. It does not start the sample when the elapsed time plus that
estimate exceeds `max_point_seconds`.

A point with one sample has no interval. The report shows it as a single value,
not as a converged result.

## Configuration

A suite case replaces `repeats` with `convergence`:

```json
"convergence": {
  "min_repeats": 3,
  "max_repeats": 10,
  "target_rel_half_width": 0.05,
  "max_point_seconds": 600
}
```

- `min_repeats` is at least 1. `max_repeats` is at least `min_repeats` and at
  most 31.
- `target_rel_half_width` is a fraction of the mean, greater than 0 and less
  than 1. Omit it for a fixed count, which then requires
  `min_repeats == max_repeats`.
- `max_point_seconds` is optional. When it is absent, only `max_repeats` limits
  the point.

This is a hard cutover. The `repeats` field is removed from suites, workloads,
and validation, with no alias. Artifacts that were written before this change
still read correctly: their rows are samples of points with a fixed count.

## Runner behavior

- **The plan stays static.** The planner expands each point to `max_repeats`
  samples, so `--dry-run`, `suite.json`, and time estimates show the worst case.
- **The runner skips samples that a point does not need.** After each sample it
  calls the stopping rule. When the point stops, the remaining planned samples
  of that point are skipped with a `workload_skipped` event and the stop reason.
  This is the same mechanism that the adaptive ladder uses, so a skipped sample
  is never a silent hole.
- **The runner records the decision.** When a point stops, the runner writes a
  `point_stopped` event with the reason, `n`, mean, half-width, relative
  half-width, and the policy.
- **Samples of one point run in sequence.** The current plan order already does
  this. Convergence needs it, because the rule evaluates one point at a time.

### Resume

A resumed run already skips planned samples whose result file parses as
complete with zero failed requests, and it replays those rows into the adaptive
ladder. Resumed samples also feed convergence:

- The runner replays resumed samples of a point in repeat order, with their
  stored values and durations, before it runs any new sample of that point.
- When the replayed samples already satisfy a stop reason, the point stops at
  once, and its remaining planned samples are skipped as usual.
- Otherwise the point continues with the next missing repeat. It never starts
  over and never repeats a sample that already has a valid result.
- The elapsed time for `max_point_seconds` is the sum of the sample durations,
  resumed and new. Time between attempts does not count.

### Failed samples

A sample fails when it returns an error or reports any failed request. Today a
failure stops the ladder above that concurrency, but the remaining repeats of
the same point still run. With convergence:

- A failed sample stops its point with reason `failed`. The remaining planned
  samples of that point are skipped.
- There is no automatic retry. A retry would hide an unstable point.
- The successful samples before the failure stay in the artifact. The report
  keeps its existing failure display for the point, and the cell details show
  the `failed` stop reason with the interval of the earlier samples. It never
  shows the point as converged.
- A failed sample is not a value. It does not enter the interval.
- The ladder rule does not change: a failure still stops the higher
  concurrency points of that profile and workload.

## Interaction with the adaptive ladder

The ladder stop rules in `adaptive.go` now run once per completed point, not
after each sample. They use the point mean of the throughput and the mean of
the samples' TTFT p99 values. A failed point does not feed these rules; the
failure already stops the higher concurrency points.

The throughput-plateau rule also uses the intervals. A gain counts only when it
is at least `min_throughput_gain_pct` and the lower bound of the current point
is above the upper bound of the previous point. When the intervals overlap, the
gain is treated as a tie and the ladder stops. When either point has one sample,
the rule compares means as it does today.

## Artifact and report

- **No schema change.** Samples stay one `measurements` row each. The policy is
  part of the normalized spec, and the decision is in `events`.
- **One function computes the statistics.** The `point_stopped` event holds a
  `convergence.StopRecord`: the metric, the reason, the interval, the sample
  values and durations, and the policy. `artifact check` recomputes every
  record with the same `Evaluate` function. It fails when the recorded reason or
  interval disagrees, when a shorter prefix of the samples had already stopped
  the point, or when the event has no linked measurement. The latest record of
  each point must also match that point's completed rows with a positive value.
  Records from an earlier attempt of a resumed run are only checked on their
  own.
- **Skipped samples are not part of the point.** The report drops samples that
  convergence skipped before it combines repeats, so a converged point keeps
  the status and values of the samples it ran. Skips from the ladder or a guard
  stay visible as before.
- **The headline tables do not change.** The headline value stays the mean. The
  metric-cell details show `mean ± half_width (95%, n)` and the stop reason. No
  `Repeats` headline column is added.
- **Points without a converged result are marked.** A headline throughput cell
  whose point stopped on `max_repeats`, `time_budget`, or `failed` gets a `*`
  marker with the note as its tooltip, in both the HTML report and the viewer.
  Its details show the note and the actual interval.

## Code layout

- `internal/convergence`: the policy type, the t table,
  `Evaluate(values []float64, durations []time.Duration, policy Policy) Decision`,
  and the `StopRecord` event payload with its `Verify` method. It has no I/O.
  Unit tests cover the table edges, `n = 1`, a zero mean, identical samples,
  each stop reason, and the time-budget estimate. The package must pass the
  Slophammer CRAP gate.
- `internal/benchmarkconfig`: suite cases declare `convergence`, and validation
  enforces the bounds above.
- `internal/runner`: the skip hook, the `point_stopped` event, and the ladder
  change.
- `internal/report` and `internal/artifact`: the interval display, the marker,
  and the `artifact check` recomputation.

## Defaults

All built-in suites use one policy:

| Setting                 | Default | Reason                                                                                                                                    |
| ----------------------- | ------- | ----------------------------------------------------------------------------------------------------------------------------------------- |
| `min_repeats`           | 3       | With 2 samples `t(1) = 12.706`, so the interval is too wide to be useful. Three is the smallest useful count.                            |
| `max_repeats`           | 10      | A point that does not converge in 10 samples has a real noise problem. More samples hide it; the report marks it instead.                |
| `target_rel_half_width` | 0.05    | Smaller differences are ties under practical significance. At about 2% sample noise, ±5% converges at 3 samples, and ±3% needs about 5. |
| `max_point_seconds`     | 600     | Keeps slow long-context points bounded. A point that reaches the limit is marked.                                                        |

A suite tightens the target only when a decision needs a smaller difference.
A fixed count stays available with `min_repeats == max_repeats` and no target.
It behaves exactly like the removed `repeats` field.

## Decode output length

Convergence decides how many samples a point gets. The output length decides
what one sample measures. They stay separate settings, and output length stays
a fixed value for each case. The output length does not adapt inside a request:
tokens in one request are correlated, and the context would change during the
measurement.

This change keeps the built-in decode cases at 1024 output tokens.

### Open decision: a shorter output

A shorter output, for example 256 tokens, makes long-context points faster and
puts the decode range closer to the context label. It is not a free change:

- The headline `Decode tok/s` divides output tokens by wall time, and the wall
  time includes TTFT. With a shorter output, prefill takes a larger share of
  the wall time, so the headline decode value at long context drops because of
  the change itself, not because the model got slower.
- A TTFT-free decode rate exists as TPOT detail, but the established headline
  definitions forbid a new headline column.

Changing the output length therefore needs one of these decisions first: accept
the different headline meaning under a new suite version, or change the decode
headline definition in
[Full-run timing and prefill](2026-07-31-full-run-timing-and-prefill.md).

## Suite version and old results

The suite format changes, because `repeats` becomes `convergence`. The change
raises the suite version from `1` to `2`. Deployment files and runner specs keep
version `1`. A suite file with version `1` or a `repeats` field fails to load
with a clear error. Runner specs are only compiled from a suite and a
deployment, never loaded from a file, so the suite loader is the only entry
point that needs this guard.

Old artifacts stay readable. The `workloads.repeats` column keeps its name and
now stores `max_repeats`, the planned upper bound.

[Built-in inference suites](2026-07-02-default-inference-sweep.md) and
[Practical c1/c6 64k](2026-07-31-practical-c1-c6-64k.md) must be updated in the
same change.

## Validation

Before committing the implementation, run the repository checks from
`AGENTS.md` and the one-case dry run. Also run one real converged point against
a local server and confirm that:

- the run stops with `converged` and skips the remaining samples;
- the report shows the interval and the stop reason;
- `artifact check` passes, and fails after a manual edit to the stored decision.
