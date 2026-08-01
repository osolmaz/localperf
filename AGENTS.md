# AGENTS.md

This is a Go repository for local inference benchmark tooling.

Before finishing code changes, run:

```sh
go test ./...
go vet ./...
npx -y @simpledoc/simpledoc check
go run github.com/osolmaz/slophammer/go/cmd/slophammer-go@v0.4.1 check .
```

The default CI path is non-mutating. `scripts/check-crap.sh` and
`scripts/check-mutation.sh` are explicit Slophammer debt gates; run them when
working on the core runner or artifact internals and expect them to require
focused cleanup if they fail.

For changes that affect benchmark behavior, artifacts, or reports, also run one
small dry benchmark case and validate the SQLite artifact:

```sh
rm -rf /tmp/localperf-onecase-dry /tmp/localperf-onecase-dry.sqlite
go run ./cmd/localperf bench run \
  --dry-run \
  --suite practical-64k \
  --deployment examples/deployments/vllm-managed.json \
  --case generate-empty \
  --concurrency 1 \
  --run-dir /tmp/localperf-onecase-dry
go run ./cmd/localperf artifact check /tmp/localperf-onecase-dry.sqlite
```

Keep benchmark safety behavior conservative. Do not lower memory floors or
remove guardrails to make a run pass.

When the user asks for the practical sweep, use `practical-64k`. For a 4k
throughput run use `throughput-4k`; for the regular active-context `4k`, `8k`,
`16k`, `32k`, `64k`, `128k` ladder use `context-ladder`. Do not combine these
suite contracts or silently add cases.

For repeated benchmark runs of the same model, keep results in one model-level
SQLite artifact and render 1 HTML report per model. Do not split retry runs,
context lengths, or concurrency points into separate final artifacts unless the
split is temporary debugging data; see
`docs/2026-07-02-default-inference-sweep.md`.

Keep production Go code under `cmd` and `internal`. Treat `examples`, `docs`,
and `runs` as fixtures, documentation, or local run data rather than production
library code.

Slophammer standards are applied through `slophammer.yml`; update that policy
and the matching local scripts/CI together.
