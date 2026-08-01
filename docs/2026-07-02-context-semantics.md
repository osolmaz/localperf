---
title: Context Semantics for Benchmark Specs and Reports
author: Bob <dutifulbob@gmail.com>
date: 2026-07-02
---

# Context Semantics for Benchmark Specs and Reports

This doc defines how localperf distinguishes server context capacity from the
active context a workload actually exercises, and how specs, validation, and
reports must handle the two. It is the contract for the fix; see
[Implementation status](#implementation-status) for what is landed.

## Problem

A profile's `max_model_len` (stored as `profiles.context_window`) is a server
capacity limit. It says the server may accept sequences up to that length. It
says nothing about how many tokens a benchmark request actually put through
the model.

The old report grouped and titled rows by that capacity value, so a row could
read as "32k context" while the workload was ~1k prompt / 4k output. Concrete
example from the Gemma sweep
(`gemma4-merged-practical-long-output-20260701.sqlite`):

- `profiles.context_window`: 32768
- requested: `random_input_len=1024`, `random_output_len=4096`
- measured: ~1037 prompt tokens, 4096 completion tokens per request

Decode cost depends on the tokens actually in the KV cache, and prefill cost
depends on the actual prompt length. A ~1k-prompt workload on a 32k-capacity
server is a valid experiment, but labeling it "32k context" conflates
capacity with active context and misleads every reader of the report.

## Definitions

- **Server limit** (`max_model_len`, `profiles.context_window`): the maximum
  sequence length the server was configured to accept. A capacity setting.
  It affects memory allocation and achievable concurrency, so it is a real
  experimental variable, but it is never a workload measurement.
- **Active context**: the number of tokens in the KV cache for a request. It
  starts at the prompt length after prefill and grows to prompt length plus
  output length by the end of decode. Because it changes over the run, a
  measurement has three reference values, all per-request means over
  completed requests:
  - `active_start = avg(prompt_tokens)`: context during prefill and at the
    first decode step.
  - `active_end = avg(prompt_tokens) + avg(completion_tokens)`: peak context
    at the last decode step.
  - `active_avg = avg(prompt_tokens) + avg(completion_tokens) / 2`: mean
    context across decode steps, the value sustained decode throughput is
    most representative of.

  `context_target` always refers to `active_end` (peak KV usage). For
  long-output workloads the three values differ materially, so reports must
  not collapse them into one bare number; see the labeling rules.
- **Requested shape**: `random_input_len` and `random_output_len` on the
  workload. These are intents; measured token counts are the ground truth.

## Spec contract

Workloads declare what their context number means:

```json
{
  "name": "decode-28k-out4k",
  "phase": "decode",
  "context_target": 32768,
  "context_semantics": "active",
  "random_input_len": 28160,
  "random_output_len": 4096
}
```

- `context_target` (int): the context point this workload claims to measure.
- `context_semantics` (`"active"` or `"capacity"`): what the target means.

Validation rules, enforced as hard errors at spec load:

1. `context_semantics: "active"` requires
   `random_input_len + random_output_len` to be within 90% to 100% of
   `context_target`, and every profile the workload runs against must have
   `max_model_len >= context_target`.
2. `context_semantics: "capacity"` requires `context_target` to equal the
   profile's `max_model_len`. The workload shape is unconstrained; the row is
   a capacity/concurrency data point, never a context-scaling data point.
3. `context_target` without `context_semantics` (or the reverse) is invalid.
4. Both fields are required on every workload. A spec without them is
   refused.

The error message for rule 1 must explain the distinction, because spec
authors (human or agent) correct themselves from it, for example:

```text
workload "decode-1k-out4096": claims active context 32768 but requests
1024+4096=5120 tokens (16% of target); this measures a ~5k active workload on
a 32k-capacity server. Either set context_target to 5120, raise
random_input_len, or declare context_semantics: "capacity".
```

The 90% floor leaves room for chat template overhead and tokenizer drift
between requested and measured token counts while keeping the label strict: a
24k workload cannot pass as a 32k point. The default sweep shapes land at
roughly 98% of target, so well-formed specs clear the floor with margin.
Widen the band only deliberately and in this doc, not ad hoc in code.

## Report labeling rules

1. A context label in a group title or heading may only come from a declared
   `context_target` whose measured `active_end` also lands within the 90% to
   100% band of the target. Declared and confirmed by measurement, or not
   shown.
2. `context_target` names the peak (`active_end`). Rows whose output length
   is more than a few tokens must also display the measured active range
   `active_start -> active_end`, for example `28k -> 32k active`, so a
   long-output decode row is not read as decoding at a constant 32k context.
   Prefill rows with minimal output may display a single number, since start
   and end coincide.
3. Rows whose measurement disagrees with the declared target are labeled by
   measured shape instead, for example `~1k in / 4k out`, with an explicit
   mismatch warning. The contradicted target never renders as the label.
   Rows with no measured tokens at all (failed, planned, dry run) render the
   declared claim explicitly marked unverified, for example
   `unverified (declared 8k active)`, and are never counted among the
   report's verified active contexts.
4. `max_model_len` always renders as a server limit attribute (for example
   `server limit: 32k`), never as a group title, and never on the report's
   "Contexts" summary line. The summary line reports active-context points.
5. Prefill throughput is interpreted from measured prompt tokens and TTFT;
   decode throughput from measured completion tokens. Neither is ever
   attributed to the server limit.

These rules apply at render time from data in the SQLite artifact
(`requests.prompt_tokens`, `requests.completion_tokens`).

## Suite compilation

Built-in suites carry context shapes that satisfy rule 1 by construction and
fill in `context_target` and `context_semantics`. Custom suites must declare
the same semantics explicitly and pass the same validation. The built-in
contracts live in `2026-07-02-default-inference-sweep.md`.

## Implementation status

Implemented:

1. Spec fields and validation in `internal/vllmbench/config.go`
   (`validateWorkloadContextSemantics`).
2. Report labeling rules in `internal/report/html.go`
   (`applyContextLabel`).
3. built-in suite definitions in `internal/benchmarkconfig`.
