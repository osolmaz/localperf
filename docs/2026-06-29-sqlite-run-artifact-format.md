---
title: SQLite Run Artifact Format
author: Bob <dutifulbob@gmail.com>
date: 2026-06-29
---

# SQLite Run Artifact Format

localperf should write one canonical SQLite artifact for the scope being
reported. For a single ad hoc run, use:

```text
runs/<run-id>.sqlite
```

For a model sweep or repeated profiling of the same model, use one model-level
artifact:

```text
runs/models/<model-slug>.sqlite
```

That SQLite file is the source of truth for the run or model sweep. Markdown,
JSON, CSV, HTML, and Parquet outputs are exports derived from it.

## Minimal Artifact

A minimal valid artifact is a SQLite database with:

- the `metadata` table,
- one or more `run` rows,
- one `specs` row for the original spec per run,
- one `specs` row for the normalized spec per run.

A single-run artifact normally has one `run` row. A model-level artifact should
have one `run` row per benchmark attempt or batch for that model.

Example inspection:

```sh
sqlite3 runs/diffusiongemma-20260629.sqlite \
  "select key, value from metadata order by key"
```

Required metadata:

```text
format_name     localperf_run
format_version  1
```

## File Rules

- The file extension should be `.sqlite`.
- The database must be readable by standard SQLite tooling.
- `PRAGMA foreign_keys=ON` must be used by localperf.
- `PRAGMA user_version=1` should be set.
- Timestamps are UTC ISO 8601 strings with `Z`, for example
  `2026-06-29T02:30:55Z`.
- Durations are stored as milliseconds in `REAL` columns.
- Token counts and byte counts are stored as integers.
- Booleans are stored as `0` or `1`.
- JSON columns are stored as `TEXT` and should have `json_valid(...)` checks.
- Large raw logs and engine-native outputs live in `artifacts.content`.

SQLite may create temporary journal files while a benchmark is running. A
finished run should leave one canonical `.sqlite` file plus any optional
exports.

For model-level artifacts, writers must not delete the existing database when
adding another run. They should open the existing file, verify `metadata`
compatibility, insert a new unique `run.id`, and then insert all related rows
with that `run_id`. If a benchmark produced a temporary single-run artifact,
merge it into the model-level artifact before rendering the final model report.

## Schema Overview

| Table | Purpose |
| --- | --- |
| `metadata` | Format and artifact-level key/value metadata. |
| `run` | One row describing the benchmark run. |
| `specs` | Original and normalized specs. |
| `engines` | Engine adapter definitions and detected versions. |
| `profiles` | Serve-time engine profiles. |
| `workloads` | Workload definitions. |
| `phases` | Startup, warmup, measurement, sleep, shutdown, and report phases. |
| `measurements` | One row per profile/workload/concurrency/repeat result. |
| `metric_stats` | Queryable distributions for latency, TTFT, TPOT, ITL, and memory metrics. |
| `requests` | Per-request timing, token, and error rows. |
| `request_stream_events` | Optional streaming chunks or token timing events per request. |
| `telemetry_series` | Definitions for sampled telemetry metrics. |
| `telemetry_samples` | Memory, GPU, process, and system telemetry samples. |
| `events` | Append-only lifecycle and diagnostic events. |
| `commands` | Exact subprocess commands and outcomes. |
| `artifacts` | Raw logs, raw benchmark JSON, stdout/stderr, generated blobs. |
| `reports` | Rendered report exports stored inside the artifact. |

## Schema DDL

This is the planned v1 schema. Implementation can add indexes and views, but
must preserve these table meanings.

```sql
PRAGMA foreign_keys = ON;
PRAGMA user_version = 1;

CREATE TABLE metadata (
  key TEXT PRIMARY KEY,
  value TEXT NOT NULL
);

CREATE TABLE run (
  id TEXT PRIMARY KEY,
  name TEXT NOT NULL,
  description TEXT,
  status TEXT NOT NULL CHECK (status IN (
    'created', 'running', 'completed', 'failed', 'canceled'
  )),
  created_at TEXT NOT NULL,
  started_at TEXT,
  completed_at TEXT,
  localperf_version TEXT,
  localperf_git_commit TEXT,
  hostname TEXT,
  username TEXT,
  cwd TEXT,
  command_line_json TEXT CHECK (
    command_line_json IS NULL OR json_valid(command_line_json)
  ),
  host_json TEXT CHECK (host_json IS NULL OR json_valid(host_json)),
  labels_json TEXT CHECK (labels_json IS NULL OR json_valid(labels_json)),
  notes TEXT
);

CREATE TABLE specs (
  id INTEGER PRIMARY KEY,
  run_id TEXT NOT NULL REFERENCES run(id) ON DELETE CASCADE,
  kind TEXT NOT NULL CHECK (kind IN ('original', 'normalized')),
  format TEXT NOT NULL CHECK (format IN ('json')),
  content TEXT NOT NULL CHECK (json_valid(content)),
  sha256 TEXT NOT NULL,
  created_at TEXT NOT NULL,
  UNIQUE (run_id, kind)
);

CREATE TABLE engines (
  id TEXT PRIMARY KEY,
  run_id TEXT NOT NULL REFERENCES run(id) ON DELETE CASCADE,
  name TEXT NOT NULL,
  type TEXT NOT NULL,
  managed INTEGER NOT NULL CHECK (managed IN (0, 1)),
  command TEXT,
  version TEXT,
  git_commit TEXT,
  endpoint_base_url TEXT,
  env_json TEXT CHECK (env_json IS NULL OR json_valid(env_json)),
  metadata_json TEXT CHECK (
    metadata_json IS NULL OR json_valid(metadata_json)
  ),
  UNIQUE (run_id, name)
);

CREATE TABLE profiles (
  id TEXT PRIMARY KEY,
  run_id TEXT NOT NULL REFERENCES run(id) ON DELETE CASCADE,
  engine_id TEXT NOT NULL REFERENCES engines(id) ON DELETE CASCADE,
  name TEXT NOT NULL,
  model TEXT NOT NULL,
  host TEXT,
  port INTEGER,
  endpoint_base_url TEXT,
  managed INTEGER NOT NULL CHECK (managed IN (0, 1)),
  context_window INTEGER,
  max_num_seqs INTEGER,
  max_num_batched_tokens INTEGER,
  gpu_memory_utilization REAL,
  enable_sleep_mode INTEGER CHECK (enable_sleep_mode IN (0, 1)),
  sleep_level INTEGER,
  serve_json TEXT CHECK (serve_json IS NULL OR json_valid(serve_json)),
  engine_args_json TEXT CHECK (
    engine_args_json IS NULL OR json_valid(engine_args_json)
  ),
  env_json TEXT CHECK (env_json IS NULL OR json_valid(env_json)),
  metadata_json TEXT CHECK (
    metadata_json IS NULL OR json_valid(metadata_json)
  ),
  UNIQUE (run_id, name)
);

CREATE TABLE workloads (
  id TEXT PRIMARY KEY,
  run_id TEXT NOT NULL REFERENCES run(id) ON DELETE CASCADE,
  name TEXT NOT NULL,
  role TEXT NOT NULL CHECK (role IN ('benchmark', 'diagnostic')),
  phase TEXT NOT NULL DEFAULT 'mixed',
  traffic_json TEXT NOT NULL CHECK (json_valid(traffic_json)),
  concurrency_json TEXT NOT NULL CHECK (json_valid(concurrency_json)),
  samples INTEGER NOT NULL CHECK (samples > 0),
  repeats INTEGER NOT NULL DEFAULT 1 CHECK (repeats > 0),
  save_detailed INTEGER NOT NULL DEFAULT 1 CHECK (save_detailed IN (0, 1)),
  capture_payload_artifacts INTEGER NOT NULL DEFAULT 0 CHECK (
    capture_payload_artifacts IN (0, 1)
  ),
  dataset_json TEXT CHECK (dataset_json IS NULL OR json_valid(dataset_json)),
  request_json TEXT CHECK (request_json IS NULL OR json_valid(request_json)),
  metadata_json TEXT CHECK (
    metadata_json IS NULL OR json_valid(metadata_json)
  ),
  UNIQUE (run_id, name)
);

CREATE TABLE phases (
  id INTEGER PRIMARY KEY,
  run_id TEXT NOT NULL REFERENCES run(id) ON DELETE CASCADE,
  profile_id TEXT REFERENCES profiles(id) ON DELETE SET NULL,
  workload_id TEXT REFERENCES workloads(id) ON DELETE SET NULL,
  name TEXT NOT NULL,
  type TEXT NOT NULL CHECK (type IN (
    'startup', 'health_check', 'warmup', 'measurement',
    'sleep', 'wake', 'shutdown', 'report', 'other'
  )),
  status TEXT NOT NULL CHECK (status IN (
    'planned', 'running', 'completed', 'failed', 'skipped', 'canceled'
  )),
  started_at TEXT,
  completed_at TEXT,
  metadata_json TEXT CHECK (
    metadata_json IS NULL OR json_valid(metadata_json)
  )
);

CREATE TABLE measurements (
  id INTEGER PRIMARY KEY,
  run_id TEXT NOT NULL REFERENCES run(id) ON DELETE CASCADE,
  profile_id TEXT NOT NULL REFERENCES profiles(id) ON DELETE CASCADE,
  workload_id TEXT NOT NULL REFERENCES workloads(id) ON DELETE CASCADE,
  phase_id INTEGER REFERENCES phases(id) ON DELETE SET NULL,
  repeat_index INTEGER NOT NULL DEFAULT 0,
  concurrency INTEGER NOT NULL CHECK (concurrency > 0),
  samples_requested INTEGER NOT NULL CHECK (samples_requested > 0),
  status TEXT NOT NULL CHECK (status IN (
    'planned', 'running', 'completed', 'failed', 'skipped', 'canceled'
  )),
  started_at TEXT,
  completed_at TEXT,
  wall_time_ms REAL,
  completed_requests INTEGER NOT NULL DEFAULT 0 CHECK (completed_requests >= 0),
  failed_requests INTEGER NOT NULL DEFAULT 0 CHECK (failed_requests >= 0),
  prompt_tokens INTEGER CHECK (prompt_tokens >= 0),
  completion_tokens INTEGER CHECK (completion_tokens >= 0),
  total_tokens INTEGER CHECK (total_tokens >= 0),
  aggregate_output_tok_s REAL,
  per_user_output_tok_s REAL,
  aggregate_total_tok_s REAL,
  raw_result_artifact_id INTEGER REFERENCES artifacts(id),
  error_type TEXT,
  error_message TEXT,
  metadata_json TEXT CHECK (
    metadata_json IS NULL OR json_valid(metadata_json)
  ),
  UNIQUE (
    run_id,
    profile_id,
    workload_id,
    repeat_index,
    concurrency
  )
);

CREATE TABLE metric_stats (
  id INTEGER PRIMARY KEY,
  measurement_id INTEGER NOT NULL REFERENCES measurements(id) ON DELETE CASCADE,
  metric TEXT NOT NULL,
  unit TEXT NOT NULL,
  mean REAL,
  stddev REAL,
  min REAL,
  p50 REAL,
  p90 REAL,
  p95 REAL,
  p99 REAL,
  max REAL,
  count INTEGER NOT NULL CHECK (count >= 0),
  metadata_json TEXT CHECK (
    metadata_json IS NULL OR json_valid(metadata_json)
  ),
  UNIQUE (measurement_id, metric, unit)
);

CREATE TABLE requests (
  id INTEGER PRIMARY KEY,
  measurement_id INTEGER NOT NULL REFERENCES measurements(id) ON DELETE CASCADE,
  request_index INTEGER NOT NULL,
  request_id TEXT,
  status TEXT NOT NULL CHECK (status IN ('completed', 'failed', 'canceled')),
  streamed INTEGER NOT NULL DEFAULT 0 CHECK (streamed IN (0, 1)),
  http_status_code INTEGER,
  started_at TEXT NOT NULL,
  first_token_at TEXT,
  completed_at TEXT,
  latency_ms REAL,
  ttft_ms REAL,
  tpot_ms REAL,
  itl_mean_ms REAL,
  prompt_tokens INTEGER CHECK (prompt_tokens >= 0),
  completion_tokens INTEGER CHECK (completion_tokens >= 0),
  total_tokens INTEGER CHECK (total_tokens >= 0),
  prompt_sha256 TEXT,
  response_sha256 TEXT,
  prompt_artifact_id INTEGER REFERENCES artifacts(id),
  response_artifact_id INTEGER REFERENCES artifacts(id),
  error_type TEXT,
  error_code TEXT,
  error_message TEXT,
  response_metadata_json TEXT CHECK (
    response_metadata_json IS NULL OR json_valid(response_metadata_json)
  ),
  UNIQUE (measurement_id, request_index)
);

CREATE TABLE request_stream_events (
  id INTEGER PRIMARY KEY,
  request_row_id INTEGER NOT NULL REFERENCES requests(id) ON DELETE CASCADE,
  event_index INTEGER NOT NULL,
  timestamp TEXT NOT NULL,
  kind TEXT NOT NULL CHECK (kind IN (
    'queued', 'sent', 'first_token', 'chunk', 'token', 'completed', 'error'
  )),
  token_count_delta INTEGER CHECK (
    token_count_delta IS NULL OR token_count_delta >= 0
  ),
  text_byte_count_delta INTEGER CHECK (
    text_byte_count_delta IS NULL OR text_byte_count_delta >= 0
  ),
  metadata_json TEXT CHECK (
    metadata_json IS NULL OR json_valid(metadata_json)
  ),
  UNIQUE (request_row_id, event_index)
);

CREATE TABLE telemetry_series (
  id INTEGER PRIMARY KEY,
  run_id TEXT NOT NULL REFERENCES run(id) ON DELETE CASCADE,
  source TEXT NOT NULL,
  metric TEXT NOT NULL,
  unit TEXT,
  target TEXT NOT NULL DEFAULT 'run',
  tags_json TEXT NOT NULL DEFAULT '{}' CHECK (json_valid(tags_json)),
  UNIQUE (run_id, source, metric, target, tags_json)
);

CREATE TABLE telemetry_samples (
  id INTEGER PRIMARY KEY,
  series_id INTEGER NOT NULL REFERENCES telemetry_series(id) ON DELETE CASCADE,
  timestamp TEXT NOT NULL,
  value REAL NOT NULL,
  phase_id INTEGER REFERENCES phases(id) ON DELETE SET NULL,
  measurement_id INTEGER REFERENCES measurements(id) ON DELETE SET NULL
);

CREATE TABLE events (
  id INTEGER PRIMARY KEY,
  run_id TEXT NOT NULL REFERENCES run(id) ON DELETE CASCADE,
  timestamp TEXT NOT NULL,
  level TEXT NOT NULL CHECK (level IN (
    'debug', 'info', 'warn', 'error'
  )),
  type TEXT NOT NULL,
  phase_id INTEGER REFERENCES phases(id) ON DELETE SET NULL,
  profile_id TEXT REFERENCES profiles(id) ON DELETE SET NULL,
  workload_id TEXT REFERENCES workloads(id) ON DELETE SET NULL,
  measurement_id INTEGER REFERENCES measurements(id) ON DELETE SET NULL,
  message TEXT,
  data_json TEXT CHECK (data_json IS NULL OR json_valid(data_json))
);

CREATE TABLE commands (
  id INTEGER PRIMARY KEY,
  run_id TEXT NOT NULL REFERENCES run(id) ON DELETE CASCADE,
  phase_id INTEGER REFERENCES phases(id) ON DELETE SET NULL,
  measurement_id INTEGER REFERENCES measurements(id) ON DELETE SET NULL,
  profile_id TEXT REFERENCES profiles(id) ON DELETE SET NULL,
  phase TEXT NOT NULL,
  cwd TEXT,
  argv_json TEXT NOT NULL CHECK (json_valid(argv_json)),
  env_json TEXT CHECK (env_json IS NULL OR json_valid(env_json)),
  started_at TEXT,
  completed_at TEXT,
  exit_code INTEGER,
  status TEXT NOT NULL CHECK (status IN (
    'planned', 'running', 'completed', 'failed', 'canceled'
  )),
  stdout_artifact_id INTEGER REFERENCES artifacts(id),
  stderr_artifact_id INTEGER REFERENCES artifacts(id),
  metadata_json TEXT CHECK (
    metadata_json IS NULL OR json_valid(metadata_json)
  )
);

CREATE TABLE artifacts (
  id INTEGER PRIMARY KEY,
  run_id TEXT NOT NULL REFERENCES run(id) ON DELETE CASCADE,
  kind TEXT NOT NULL,
  name TEXT NOT NULL,
  media_type TEXT NOT NULL,
  compression TEXT NOT NULL DEFAULT 'none' CHECK (
    compression IN ('none', 'gzip')
  ),
  content BLOB NOT NULL,
  content_size_bytes INTEGER NOT NULL,
  uncompressed_size_bytes INTEGER NOT NULL,
  sha256 TEXT NOT NULL,
  original_path TEXT,
  created_at TEXT NOT NULL,
  metadata_json TEXT CHECK (
    metadata_json IS NULL OR json_valid(metadata_json)
  ),
  UNIQUE (run_id, kind, name)
);

CREATE TABLE reports (
  id INTEGER PRIMARY KEY,
  run_id TEXT NOT NULL REFERENCES run(id) ON DELETE CASCADE,
  name TEXT NOT NULL,
  format TEXT NOT NULL CHECK (format IN ('markdown', 'json', 'csv', 'html')),
  media_type TEXT NOT NULL,
  artifact_id INTEGER NOT NULL REFERENCES artifacts(id) ON DELETE CASCADE,
  created_at TEXT NOT NULL,
  UNIQUE (run_id, name, format)
);
```

Recommended indexes:

```sql
CREATE INDEX idx_measurements_lookup
  ON measurements (run_id, profile_id, workload_id, concurrency);

CREATE INDEX idx_metric_stats_metric
  ON metric_stats (metric, unit, measurement_id);

CREATE INDEX idx_requests_measurement
  ON requests (measurement_id, status);

CREATE INDEX idx_request_stream_events_request
  ON request_stream_events (request_row_id, event_index);

CREATE INDEX idx_telemetry_samples_series_time
  ON telemetry_samples (series_id, timestamp);

CREATE INDEX idx_telemetry_samples_phase_time
  ON telemetry_samples (phase_id, timestamp);

CREATE INDEX idx_phases_run_time
  ON phases (run_id, started_at);

CREATE INDEX idx_events_run_time
  ON events (run_id, timestamp);
```

## Required Performance Data

Each completed measurement must be enough to answer these questions without
rerunning the benchmark:

- What exact spec, profile, engine, command, and environment produced it?
- How many requests were attempted, completed, failed, or canceled?
- How many prompt, completion, and total tokens were processed?
- What were aggregate output token/s, per-user output token/s, and total
  token/s?
- What were latency, TTFT, TPOT, and inter-token latency distributions?
- What memory and GPU telemetry was observed before, during, and after the
  measured workload?
- What raw engine output supports the normalized row?
- What errors occurred, and at which phase?

`measurements` stores aggregate throughput and token counts.
`metric_stats` stores queryable distributions. `requests` stores per-request
rows. `request_stream_events` stores optional stream timing detail.
`telemetry_samples` stores time series. `artifacts` stores raw engine output.

## Metric Statistics

Use `metric_stats` for distributions that need comparison across profiles,
engines, or runs.

Recommended metric names:

| Metric | Unit | Meaning |
| --- | --- | --- |
| `latency` | `ms` | End-to-end request latency. |
| `ttft` | `ms` | Time to first token. Written only when the measurement carries `{"ttft_source": "stream"}` in `measurements.metadata_json`; reports render TTFT solely from marked measurements, so a run without streamed samples has no TTFT rather than a first-byte approximation. |
| `tpot` | `ms` | Time per output token. |
| `itl` | `ms` | Inter-token latency. |
| `output_throughput` | `tok/s` | Per-request output throughput distribution. |
| `total_throughput` | `tok/s` | Per-request total-token throughput distribution. |
| `mem_available` | `bytes` | Distribution of observed system available memory. |
| `gpu_utilization` | `percent` | Distribution of observed GPU utilization when available. |

Example:

```sql
INSERT INTO metric_stats (
  measurement_id, metric, unit, mean, stddev, min,
  p50, p90, p95, p99, max, count
) VALUES (
  42, 'ttft', 'ms', 1200.4, 300.1, 900.0,
  1100.0, 1700.0, 1800.0, 2100.0, 2400.0, 32
);
```

For token throughput variance, store one `measurements` row per repeat and
calculate cross-repeat stats in reports.

## Telemetry Names

Use stable telemetry series names where possible.

| Source | Name | Unit |
| --- | --- | --- |
| `proc_meminfo` | `mem_available_bytes` | `bytes` |
| `proc_meminfo` | `swap_free_bytes` | `bytes` |
| `process` | `rss_bytes` | `bytes` |
| `cgroup` | `memory_current_bytes` | `bytes` |
| `cgroup` | `memory_peak_bytes` | `bytes` |
| `vllm` | `kv_cache_available_bytes` | `bytes` |
| `vllm` | `kv_cache_tokens` | `tokens` |
| `vllm` | `reported_max_concurrency` | `requests` |
| `tegrastats` | `ram_used_bytes` | `bytes` |
| `tegrastats` | `swap_used_bytes` | `bytes` |
| `tegrastats` | `gpu_utilization_percent` | `percent` |
| `nvml` | `gpu_memory_used_bytes` | `bytes` |
| `nvml` | `gpu_utilization_percent` | `percent` |

Unknown telemetry is allowed, but it must use a clear `source`, `name`, and
`unit`.

## Column Conventions

These current-column conventions do not change the schema or format version;
absent optional values render as unavailable.

- `run.host_json`: hardware inventory captured at run start. Expected keys:
  `cpu`, `ram_gib`, and `gpus` (a list of `{name, vram_gib, driver}`), plus
  which telemetry sources were available.
- `engines.version` and `engines.metadata_json`: filled from startup identity
  probes (`/version` for vLLM, `/v1/models` for any OpenAI-compatible
  server). `metadata_json` stores the server's self-reported identity so
  external engines are verified rather than trusted.
- `profiles.serve_json`: includes `enable_prefix_caching` when known, since
  prefix caching changes how prefill numbers must be read.
- `workloads.metadata_json`: the single home for declared workload claims,
  keyed by claim type. `context`
  (`{"target": 32768, "semantics": "active"}`) per
  `2026-07-02-context-semantics.md`, and optionally `slo` (for example
  `{"ttft_p95_ms": 500, "e2el_p95_ms": 30000}`) used to derive goodput at
  report time.
- `workloads.traffic_json` stays strictly the engine input
  (`BenchmarkTrafficConfig`). Declared claims never ride in it, and warmup
  traffic carries no claims.
- Dimension primary keys (`engines.id`, `profiles.id`, `workloads.id`,
  `datasets.id`, `canonical_requests.id`) are namespaced per run as
  `<run_id>/<name>` so multiple runs coexist in one model-level artifact.
  `name` columns stay bare for display; joins go through the namespaced ids.
- Writers append: an existing artifact at the target path gains a new `run`
  row and its children; re-writing the same run id replaces that run.
  `localperf artifact merge --into <dst> <src>...` combines existing
  artifacts, rescoping dimension ids and shifting integer ids; runs already
  present in the destination are skipped.

## Artifact Kinds

`artifacts.kind` should use these values first:

| Kind | Meaning |
| --- | --- |
| `server_log` | Engine server stdout/stderr or combined log. |
| `bench_raw_result` | Engine-native benchmark result JSON. |
| `command_stdout` | Captured command stdout. |
| `command_stderr` | Captured command stderr. |
| `normalized_report` | Generated report body. |
| `raw_telemetry` | Original telemetry stream, if captured separately. |
| `debug` | Extra diagnostic blob. |

Use `gzip` compression for large text artifacts. Store the SHA-256 of the
uncompressed bytes. `content_size_bytes` is the stored blob size.
`uncompressed_size_bytes` is the original byte length before compression.

## Privacy Rules

Prompt and response text can leak data. Default behavior:

- save token counts,
- save timings,
- save hashes,
- do not save prompt text,
- do not save response text,
- redact environment variables that look like tokens, keys, passwords, or
  credentials.

`workloads.capture_payload_artifacts=1` is required before localperf stores
prompt or response payloads in `artifacts`. Request rows may link to those
payloads through `prompt_artifact_id` and `response_artifact_id`.

## Lifecycle

The runner writes the SQLite file incrementally:

1. Create the artifact and schema.
2. Insert `metadata`, `run`, `specs`, `engines`, `profiles`, and `workloads`.
3. Mark the run `running`.
4. Insert `phases`, events, and command rows as lifecycle phases start.
5. Insert one `measurements` row for each planned measurement point.
6. Append request rows, request stream events, telemetry samples, raw artifacts,
   and events while the measurement runs.
7. Update each measurement with final aggregate metrics.
8. Insert `metric_stats` rows for latency, TTFT, TPOT, ITL, throughput, and
   memory distributions.
9. Generate report artifacts.
10. Mark the run `completed`, `failed`, or `canceled`.

Each measurement should be finalized in a transaction so partial runs are still
queryable.

## Validation

Future command:

```sh
localperf artifact check runs/<run-id>.sqlite
```

The validator must check:

- `metadata.format_name = localperf_run`.
- `metadata.format_version = 1`.
- required tables exist.
- there is at least one `run` row, each with exactly one original and one
  normalized spec.
- original and normalized specs exist and match their SHA-256 values.
- JSON columns contain valid JSON.
- every workload has an allowed `role` and explicit `context_target` and
  `context_semantics` evidence.
- every measurement records its resolved request count and exact completed and failed request totals.
- every measurement uses a concurrency declared by its workload.
- foreign keys are valid.
- completed measurements have throughput, token, and request counts.
- completed measurements have `metric_stats` rows for the metrics they
  report.
- completed measurements with `save_detailed=1` have request rows.
- streamed request rows have request stream events or a raw stream artifact.
- telemetry samples use numeric `value` rows with a declared series unit.
- referenced artifact hashes match stored content.
- prompt and response payload artifacts are absent unless capture was enabled.
- final run status is one of the allowed values.

## Accepted format

Readers accept only the current schema with these metadata values:

```text
format_name     localperf_run
format_version  1
```

LocalPerf does not provide migration readers, aliases, or fallback behavior for
older v1 schemas. Schema changes replace the v1 contract in place. Recreate an
artifact with the current `localperf bench run` command when validation rejects
an older file. Diagnostic workloads remain stored as evidence but report and
comparison queries include only workloads whose role is `benchmark`.

## Boundaries

The `.sqlite` artifact stores benchmark evidence. It does not store model
weights, Hugging Face cache files, credentials, or enough private environment
state to recreate a user's machine.

Raw artifacts should be useful for debugging and reproduction, but the
normalized tables are the stable API.
