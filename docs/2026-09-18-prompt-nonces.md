---
title: Prompt nonces keep every prefill cold
author: Bob <dutifulbob@gmail.com>
date: 2026-09-18
---

# Prompt nonces keep every prefill cold

A server that caches prompt prefixes answers a repeated prompt from that cache.
The prefill then costs almost nothing and the row reports a prefill rate that no
cold request can reach. One local run recorded 4,148 prompt tokens in 191 ms,
which is roughly 22,000 tokens per second on a machine that measures about 440
tokens per second on a cold 4k prompt.

LocalPerf now stamps every request that its built-in HTTP client sends. The
stamp makes each request a new prompt, so a cached prefix can never answer it.

## What the stamp is

The stamp is a short prefix, `[localperf nonce <salt> <counter>] `, with a salt
that changes per run and a counter that increases per send.

- The prefix lands at the start of the first user turn, which is where a prefix
  cache would otherwise match. A shared system prompt stays untouched.
- The counter advances per send, not per dataset row, so repeats of one case
  and separate runs against a warm server both send new text.
- The stamp costs about ten tokens. It stays inside the 90–100% active-context
  band, and the recorded `prompt_sha256` covers the stamped request.
- Requests that never reach the server keep the planned prompt hash.

## Turning it off

Set `"prompt_nonce": false` on a suite case or a workload to measure prefix
reuse on purpose:

```json
{
  "name": "shared-prefix",
  "role": "diagnostic",
  "prompt_nonce": false
}
```

The default is on. An artifact records the setting through the execution plan,
so a reader can tell which mode produced the row.

## Scope

The stamp applies to `localperf_http`, the built-in client. Requests started by
the external `vllm` load generator are outside LocalPerf's control, because that
tool generates its own prompts.

## How to check a run

- Every repeat in the server log shows a full `prompt eval` token count.
- `prompt_sha256` differs between rows that replayed the same case.

## Tests

`internal/vllmbench/prompt_nonce_test.go` covers the prefix shape, the placement
in the first user turn, the completion prompt path, the disabled case, and one
end-to-end send that proves two replay sends arrive as two prompts.
`internal/benchmarkconfig/prompt_nonce_test.go` covers the suite and workload
setting.
