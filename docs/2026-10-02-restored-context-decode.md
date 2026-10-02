---
title: Decode cases start from a restored context
author: Onur Solmaz <2453968+osolmaz@users.noreply.github.com>
date: 2026-10-02
tags: [runner, suites, llama-cpp]
---

# Decode cases start from a restored context

A decode case asks how fast a model generates with a given amount of context.
Until now every decode sample built that context with a cold prefill. On a 9B
model at 64k context, one sample spent about 148 seconds in prefill and about
30 seconds in decode. The headline `Decode tok/s` divides output tokens by the
full wall time, so it showed about 6 tok/s while the real decode speed was
about 34 tok/s. The run also repeated the same slow prefill for every sample.

LocalPerf now separates the two questions:

- A **prefill case** measures a cold prefill. It keeps the prompt nonce at the
  start of the prompt, so no cache can answer it.
- A **decode case** starts from a prepared context. The prefill happens once,
  outside the measurement, and every sample measures only decode.

## Terms

- The **base prompt** of a decode case is the prompt that fills the context.
  It is deterministic: the same case on the same model always has the same
  base prompt.
- A **snapshot** is the server's saved KV cache of the base prompt.
- A **restored sample** restores the snapshot into every slot, then sends the
  base prompt plus a short unique ending.

## Case setting

A suite case declares `"context_preparation": "restored"`. Without it, the case
runs cold as before. A restored case must:

- have phase `decode` and use the `localperf_http` load generator with the
  random dataset;
- send exactly one request per slot in each batch;
- run on a llama.cpp profile. Other engines have no context preparer yet, so
  the plan is rejected before the run. It never falls back to a cold decode.

## How a restored sample runs

1. **Snapshot.** Before the first sample of a case, the runner restores the
   snapshot file into slot 0. When the file is missing or the restore fails,
   the runner primes it: it sends the base prompt once with one output token,
   then saves slot 0 to the file.
2. **Restore.** Before every sample, the runner restores the snapshot into
   slots 0 to c-1. This time is not part of the sample.
3. **Requests.** Request i goes to slot i through `id_slot`. It is a raw
   completion request (`/v1/completions`) with `cache_prompt` on. Its prompt is
   the base prompt followed by the prompt nonce, so the unique part is at the
   end.
4. **Check.** Every response must report
   `usage.prompt_tokens_details.cached_tokens` at least equal to the restored
   token count. A response with fewer cached tokens prefilled the context
   again. Its request fails with `context_not_restored`, so the sample fails
   and the point stops as failed.

### Why raw completions

The chat template adds tokens after the user text. On a hybrid model, such as
Ternary Bonsai 2, llama.cpp then cannot use a restored cache and prefills the
whole prompt again. A raw prompt that only adds text after the saved prompt
reuses the cache: on the Prism fork, a restored 4,000-token cache answered a
4,005-token prompt with 5 prefilled tokens.

## Snapshot files

- A managed llama.cpp server always starts with `--slot-save-path` set to the
  runner's snapshot directory. `runner.snapshot_dir` sets it; the default is
  `localperf/context-snapshots` under the user cache directory.
- An endpoint llama.cpp server must be started with `--slot-save-path`. When a
  save or restore fails, the sample fails with the server's error.
- The file name is a hash of everything that changes the saved state: the
  model, the model file, the deployment's model revision, the runtime command,
  version, and digest, the KV cache types, flash attention, the per-slot
  context, the extra server arguments, and the base prompt. A later run of the same case on the same
  deployment reuses the file and skips the prime; any change primes a new one.

## Evidence

The result file of every restored sample records the snapshot file name,
whether this sample primed it, the prime time, the restore time for each slot,
and the restored token count. Every request records its cached prompt tokens.

## Effect on the report

- The `Decode tok/s` of a restored case is decode speed from a full context.
  TTFT still includes the few prefilled tokens and the time to the first
  token.
- A restored case never derives effective prefill, and the artifact stores no
  effective-prefill metric for a measurement whose requests report cached
  prompt tokens. Its prefill columns come
  from a dedicated prefill case with the same context target and concurrency,
  or show `-`.
- Restored and cold samples of the same case never pool as repeats. A restored
  row's shape ends in `· restored context`, so it also never shares a table
  row with a cold measurement from an older run in the same model artifact.
- Repeats pooled across runs show no stop decision, because no single
  decision covers them.

## Built-in suites

The suite version changes from `2` to `3`.

| Suite            | Change                                                                  |
| ---------------- | ----------------------------------------------------------------------- |
| `practical-64k`  | `generate-full` is restored. A new cold `prefill-64k` case runs at c1.  |
| `throughput-4k`  | `throughput-4k` is restored.                                            |
| `context-ladder` | Every `decode-*` case is restored. The `prefill-*` cases stay cold.     |

`generate-empty` stays cold: its prompt has 1 token, so there is no context to
prepare. Concurrent prefill at 64k is not part of `practical-64k` any more; the
`prefill-64k` case of `context-ladder` measures it at c1 to c32.

## Not in this change

- A context preparer for vLLM. Its prefix cache can serve the same role, with
  `cached_tokens` as the same check.
- Pruning old snapshot files. They stay in the cache directory until removed.
