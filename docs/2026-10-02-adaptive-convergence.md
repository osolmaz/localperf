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
- The **sample value** is the headline throughput that the report already shows
  for the phase: `Decode tok/s` for decode cases and `Prefill tok/s` for prefill
  cases. This is the value `phaseThroughput` selects. Decode speed excludes
  TTFT, as defined in
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
| `failed`      | A sample failed, and the point stopped as it does today.             |

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
  "max_repeats": 8,
  "target_rel_half_width": 0.03,
  "max_point_seconds": 900
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

## Interaction with the adaptive ladder

The ladder stop rules in `adaptive.go` now run once per completed point, not
after each sample. They use the point mean.

The throughput-plateau rule also uses the intervals. A gain counts only when it
is at least `min_throughput_gain_pct` and the lower bound of the current point
is above the upper bound of the previous point. When the intervals overlap, the
gain is treated as a tie and the ladder stops. When either point has one sample,
the rule compares means as it does today.

## Artifact and report

- **No schema change.** Samples stay one `measurements` row each. The policy is
  part of the normalized spec, and the decision is in `events`.
- **One function computes the statistics.** The report calls the same pure
  function on the stored rows to show `n`, mean, and the 95% interval.
  `artifact check` recomputes every `point_stopped` decision from the stored
  rows and fails when the stored decision and the recomputed decision disagree.
- **The headline tables do not change.** The headline value stays the mean. The
  metric-cell details show `mean ± half_width (95%, n)` and the stop reason. No
  `Repeats` headline column is added.
- **Points without a converged result are marked.** A cell whose point stopped
  on `max_repeats` or `time_budget` gets a visible marker, and its details show
  the actual interval.

## Code layout

- `internal/convergence`: the policy type, the t table, and
  `Evaluate(values []float64, durations []time.Duration, policy Policy) Decision`.
  It has no I/O. Unit tests cover the table edges, `n = 1`, a zero mean,
  identical samples, each stop reason, and the time-budget estimate. The package
  must pass the Slophammer CRAP and mutation gates.
- `internal/benchmarkconfig`: suite cases declare `convergence`, and validation
  enforces the bounds above.
- `internal/runner`: the skip hook, the `point_stopped` event, and the ladder
  change.
- `internal/report` and `internal/artifact`: the interval display, the marker,
  and the `artifact check` recomputation.

## Built-in suites

| Suite            | Proposed policy                                                  |
| ---------------- | ---------------------------------------------------------------- |
| `practical-64k`  | `min 3`, `max 8`, `target 0.03`, `max_point_seconds 900`         |
| `throughput-4k`  | `min 3`, `max 8`, `target 0.03`, `max_point_seconds 600`         |
| `context-ladder` | `min 3`, `max 6`, `target 0.05`, `max_point_seconds 900`         |

These values are proposals. Onur sets the final values before the suites change.
[Built-in inference suites](2026-07-02-default-inference-sweep.md) must be
updated in the same change.

## Open decision: decode output length

Convergence decides how many samples a point gets. It does not decide how long
each request generates. Decode cases now generate 1024 tokens. At 64k context on
a slow model that is several minutes per request.

With TTFT excluded from decode speed, a shorter output, for example 256 tokens,
probably gives a sample value of the same quality, and convergence then adds
samples where the value is noisy. Changing the output length changes the
contract of `practical-64k` and `context-ladder`, so it is a separate decision.
Make it before the built-in suite policies above are finalized.

## Validation

Before committing the implementation, run the repository checks from
`AGENTS.md` and the one-case dry run. Also run one real converged point against
a local server and confirm that:

- the run stops with `converged` and skips the remaining samples;
- the report shows the interval and the stop reason;
- `artifact check` passes, and fails after a manual edit to the stored decision.
