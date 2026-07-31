package artifact

const Schema = `
PRAGMA foreign_keys = ON;
PRAGMA user_version = 1;

CREATE TABLE metadata (key TEXT PRIMARY KEY, value TEXT NOT NULL);
CREATE TABLE run (
  id TEXT PRIMARY KEY,
  name TEXT NOT NULL,
  description TEXT,
  status TEXT NOT NULL CHECK (status IN ('created', 'running', 'completed', 'failed', 'canceled')),
  created_at TEXT NOT NULL,
  started_at TEXT,
  completed_at TEXT,
  localperf_version TEXT,
  localperf_git_commit TEXT,
  hostname TEXT,
  username TEXT,
  cwd TEXT,
  command_line_json TEXT CHECK (command_line_json IS NULL OR json_valid(command_line_json)),
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
  metadata_json TEXT CHECK (metadata_json IS NULL OR json_valid(metadata_json)),
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
  engine_args_json TEXT CHECK (engine_args_json IS NULL OR json_valid(engine_args_json)),
  env_json TEXT CHECK (env_json IS NULL OR json_valid(env_json)),
  metadata_json TEXT CHECK (metadata_json IS NULL OR json_valid(metadata_json)),
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
  capture_payload_artifacts INTEGER NOT NULL DEFAULT 0 CHECK (capture_payload_artifacts IN (0, 1)),
  dataset_json TEXT CHECK (dataset_json IS NULL OR json_valid(dataset_json)),
  request_json TEXT CHECK (request_json IS NULL OR json_valid(request_json)),
  metadata_json TEXT CHECK (metadata_json IS NULL OR json_valid(metadata_json)),
  UNIQUE (run_id, name)
);
CREATE TABLE datasets (
  id TEXT PRIMARY KEY,
  run_id TEXT NOT NULL REFERENCES run(id) ON DELETE CASCADE,
  workload_id TEXT NOT NULL REFERENCES workloads(id) ON DELETE CASCADE,
  type TEXT NOT NULL,
  uri TEXT,
  path TEXT,
  split TEXT,
  selection TEXT,
  sample_count INTEGER NOT NULL CHECK (sample_count > 0),
  seed INTEGER,
  config_json TEXT NOT NULL CHECK (json_valid(config_json)),
  canonical_path TEXT,
  rendered_path TEXT,
  request_count INTEGER NOT NULL CHECK (request_count >= 0),
  sha256 TEXT,
  metadata_json TEXT CHECK (metadata_json IS NULL OR json_valid(metadata_json)),
  UNIQUE (run_id, workload_id)
);
CREATE TABLE source_records (
  id INTEGER PRIMARY KEY,
  run_id TEXT NOT NULL REFERENCES run(id) ON DELETE CASCADE,
  dataset_id TEXT NOT NULL REFERENCES datasets(id) ON DELETE CASCADE,
  source_record_id TEXT NOT NULL,
  ordinal INTEGER NOT NULL CHECK (ordinal >= 0),
  content_json TEXT NOT NULL CHECK (json_valid(content_json)),
  sha256 TEXT NOT NULL,
  metadata_json TEXT CHECK (metadata_json IS NULL OR json_valid(metadata_json)),
  UNIQUE (dataset_id, source_record_id)
);
CREATE TABLE canonical_requests (
  id TEXT PRIMARY KEY,
  run_id TEXT NOT NULL REFERENCES run(id) ON DELETE CASCADE,
  dataset_id TEXT NOT NULL REFERENCES datasets(id) ON DELETE CASCADE,
  source_record_row_id INTEGER REFERENCES source_records(id) ON DELETE SET NULL,
  workload_id TEXT NOT NULL REFERENCES workloads(id) ON DELETE CASCADE,
  request_id TEXT NOT NULL,
  ordinal INTEGER NOT NULL CHECK (ordinal >= 0),
  conversation_id TEXT,
  turn_index INTEGER,
  mode TEXT NOT NULL,
  prompt_sha256 TEXT,
  max_output_tokens INTEGER CHECK (max_output_tokens IS NULL OR max_output_tokens >= 0),
  input_tokens_expected INTEGER CHECK (input_tokens_expected IS NULL OR input_tokens_expected >= 0),
  output_tokens_expected INTEGER CHECK (output_tokens_expected IS NULL OR output_tokens_expected >= 0),
  arrival_offset_ms INTEGER NOT NULL DEFAULT 0,
  messages_json TEXT CHECK (messages_json IS NULL OR json_valid(messages_json)),
  attachments_json TEXT CHECK (attachments_json IS NULL OR json_valid(attachments_json)),
  metadata_json TEXT CHECK (metadata_json IS NULL OR json_valid(metadata_json)),
  canonical_json TEXT NOT NULL CHECK (json_valid(canonical_json)),
  UNIQUE (dataset_id, ordinal)
);
CREATE TABLE phases (
  id INTEGER PRIMARY KEY,
  run_id TEXT NOT NULL REFERENCES run(id) ON DELETE CASCADE,
  profile_id TEXT REFERENCES profiles(id) ON DELETE SET NULL,
  workload_id TEXT REFERENCES workloads(id) ON DELETE SET NULL,
  name TEXT NOT NULL,
  type TEXT NOT NULL CHECK (type IN ('startup', 'health_check', 'warmup', 'measurement', 'sleep', 'wake', 'shutdown', 'report', 'other')),
  status TEXT NOT NULL CHECK (status IN ('planned', 'running', 'completed', 'failed', 'skipped', 'canceled')),
  started_at TEXT,
  completed_at TEXT,
  metadata_json TEXT CHECK (metadata_json IS NULL OR json_valid(metadata_json))
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
  status TEXT NOT NULL CHECK (status IN ('planned', 'running', 'completed', 'failed', 'skipped', 'canceled')),
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
  metadata_json TEXT CHECK (metadata_json IS NULL OR json_valid(metadata_json)),
  UNIQUE (run_id, profile_id, workload_id, repeat_index, concurrency)
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
  metadata_json TEXT CHECK (metadata_json IS NULL OR json_valid(metadata_json)),
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
  first_byte_at TEXT,
  first_byte_ms REAL,
  first_token_at TEXT,
  completed_at TEXT,
  latency_ms REAL,
  ttft_ms REAL,
  tpot_ms REAL,
  itl_mean_ms REAL,
  prompt_tokens INTEGER CHECK (prompt_tokens >= 0),
  completion_tokens INTEGER CHECK (completion_tokens >= 0),
  total_tokens INTEGER CHECK (total_tokens >= 0),
  output_tok_s REAL,
  total_tok_s REAL,
  prompt_sha256 TEXT,
  response_sha256 TEXT,
  prompt_artifact_id INTEGER REFERENCES artifacts(id),
  response_artifact_id INTEGER REFERENCES artifacts(id),
  error_type TEXT,
  error_code TEXT,
  error_message TEXT,
  response_metadata_json TEXT CHECK (response_metadata_json IS NULL OR json_valid(response_metadata_json)),
  UNIQUE (measurement_id, request_index)
);
CREATE TABLE request_stream_events (
  id INTEGER PRIMARY KEY,
  request_row_id INTEGER NOT NULL REFERENCES requests(id) ON DELETE CASCADE,
  event_index INTEGER NOT NULL,
  timestamp TEXT NOT NULL,
  kind TEXT NOT NULL CHECK (kind IN ('queued', 'sent', 'first_token', 'chunk', 'token', 'completed', 'error')),
  token_count_delta INTEGER CHECK (token_count_delta IS NULL OR token_count_delta >= 0),
  text_byte_count_delta INTEGER CHECK (text_byte_count_delta IS NULL OR text_byte_count_delta >= 0),
  metadata_json TEXT CHECK (metadata_json IS NULL OR json_valid(metadata_json)),
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
  level TEXT NOT NULL CHECK (level IN ('debug', 'info', 'warn', 'error')),
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
  status TEXT NOT NULL CHECK (status IN ('planned', 'running', 'completed', 'failed', 'canceled')),
  stdout_artifact_id INTEGER REFERENCES artifacts(id),
  stderr_artifact_id INTEGER REFERENCES artifacts(id),
  metadata_json TEXT CHECK (metadata_json IS NULL OR json_valid(metadata_json))
);
CREATE TABLE artifacts (
  id INTEGER PRIMARY KEY,
  run_id TEXT NOT NULL REFERENCES run(id) ON DELETE CASCADE,
  kind TEXT NOT NULL,
  name TEXT NOT NULL,
  media_type TEXT NOT NULL,
  compression TEXT NOT NULL DEFAULT 'none' CHECK (compression IN ('none', 'gzip')),
  content BLOB NOT NULL,
  content_size_bytes INTEGER NOT NULL,
  uncompressed_size_bytes INTEGER NOT NULL,
  sha256 TEXT NOT NULL,
  original_path TEXT,
  created_at TEXT NOT NULL,
  metadata_json TEXT CHECK (metadata_json IS NULL OR json_valid(metadata_json)),
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
CREATE INDEX idx_measurements_lookup ON measurements (run_id, profile_id, workload_id, concurrency);
CREATE INDEX idx_metric_stats_metric ON metric_stats (metric, unit, measurement_id);
CREATE INDEX idx_canonical_requests_dataset ON canonical_requests (dataset_id, ordinal);
CREATE INDEX idx_source_records_dataset ON source_records (dataset_id, ordinal);
CREATE INDEX idx_requests_measurement ON requests (measurement_id, status);
CREATE INDEX idx_request_stream_events_request ON request_stream_events (request_row_id, event_index);
CREATE INDEX idx_telemetry_samples_series_time ON telemetry_samples (series_id, timestamp);
CREATE INDEX idx_telemetry_samples_phase_time ON telemetry_samples (phase_id, timestamp);
CREATE INDEX idx_phases_run_time ON phases (run_id, started_at);
CREATE INDEX idx_events_run_time ON events (run_id, timestamp);
`
