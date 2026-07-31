package artifact

import (
	"database/sql"
	"fmt"
	"os"
	"time"
)

// MergeSummary reports what a merge did per source artifact.
type MergeSummary struct {
	MergedRuns  []string
	SkippedRuns []string
}

// Merge copies every run from the source artifacts into the destination,
// building the model-level artifact described in
// docs/2026-07-02-default-inference-sweep.md. Runs whose id already exists
// in the destination are skipped and reported, so merging the same source
// twice is safe. Text dimension ids (engines, profiles, workloads, datasets,
// canonical requests) are namespaced per run during the copy, and integer
// ids are offset, so single-run and model-level sources both merge cleanly.
func Merge(dstPath string, srcPaths []string) (MergeSummary, error) {
	summary := MergeSummary{}
	// Validate every source before touching the destination, so a bad
	// source cannot leave behind a freshly created empty artifact.
	for _, srcPath := range srcPaths {
		if err := Check(srcPath); err != nil {
			return summary, fmt.Errorf("merge source %s: %w", srcPath, err)
		}
	}
	info, statErr := os.Stat(dstPath)
	// An empty destination file is initialized by CreateOrAppend; on
	// failure it must be removed like a missing one, not left behind as a
	// schema-only artifact.
	createdFresh := statErr != nil || (info != nil && info.Size() == 0)
	db, err := CreateOrAppend(dstPath, Schema)
	if err != nil {
		return summary, err
	}
	defer db.Close()
	cleanupOnError := func(mergeErr error) error {
		if createdFresh {
			_ = db.Close()
			_ = os.Remove(dstPath)
		}
		return mergeErr
	}
	if err := ensureFormatMetadata(db); err != nil {
		return summary, cleanupOnError(err)
	}
	for _, srcPath := range srcPaths {
		if err := mergeSource(db, srcPath, &summary); err != nil {
			return summary, cleanupOnError(fmt.Errorf("merge %s: %w", srcPath, err))
		}
	}
	if err := Check(dstPath); err != nil {
		return summary, cleanupOnError(err)
	}
	return summary, nil
}

// ensureFormatMetadata seeds the format header on a freshly created merge
// destination; existing artifacts keep theirs.
func ensureFormatMetadata(db *sql.DB) error {
	for key, value := range map[string]string{
		"format_name":    FormatName,
		"format_version": FormatVersion,
		"generated_at":   time.Now().UTC().Format(time.RFC3339),
	} {
		if _, err := db.Exec(`INSERT OR IGNORE INTO metadata (key, value) VALUES (?, ?)`, key, value); err != nil {
			return err
		}
	}
	return nil
}

func mergeSource(db *sql.DB, srcPath string, summary *MergeSummary) error {
	if _, err := db.Exec(`ATTACH DATABASE ? AS src`, srcPath); err != nil {
		return err
	}
	defer func() { _, _ = db.Exec(`DETACH DATABASE src`) }()
	return WithTx(db, func(tx *sql.Tx) error {
		return mergeAttachedRuns(tx, summary)
	})
}

func mergeAttachedRuns(tx *sql.Tx, summary *MergeSummary) error {
	merged, skipped, err := splitMergeRuns(tx)
	if err != nil {
		return err
	}
	summary.MergedRuns = append(summary.MergedRuns, merged...)
	summary.SkippedRuns = append(summary.SkippedRuns, skipped...)
	if len(merged) == 0 {
		return nil
	}
	if err := prepareMergeRunFilter(tx, merged); err != nil {
		return err
	}
	defer func() { _, _ = tx.Exec(`DROP TABLE IF EXISTS temp.merge_runs`) }()
	offsets, err := mergeOffsets(tx)
	if err != nil {
		return err
	}
	for _, statement := range mergeStatements(offsets) {
		if _, err := tx.Exec(statement); err != nil {
			return fmt.Errorf("%w\nstatement: %s", err, statement)
		}
	}
	return nil
}

// splitMergeRuns classifies source runs: absent from the destination means
// merge; present with matching provenance (created_at and recorded run
// directory) means an idempotent skip; present with different provenance is
// a run-id collision that would silently drop results, so it is an error.
func splitMergeRuns(tx *sql.Tx) (merged, skipped []string, err error) {
	rows, err := tx.Query(`SELECT s.id,
		EXISTS (SELECT 1 FROM main.run m WHERE m.id = s.id),
		EXISTS (SELECT 1 FROM main.run m WHERE m.id = s.id
			AND m.created_at = s.created_at
			AND COALESCE(json_extract(m.labels_json, '$.run_dir'), '') = COALESCE(json_extract(s.labels_json, '$.run_dir'), ''))
		FROM src.run s ORDER BY s.created_at, s.id`)
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var id string
		var exists, sameProvenance bool
		if err := rows.Scan(&id, &exists, &sameProvenance); err != nil {
			return nil, nil, err
		}
		switch {
		case !exists:
			merged = append(merged, id)
		case sameProvenance:
			skipped = append(skipped, id)
		default:
			return nil, nil, fmt.Errorf("run id %q already exists in the destination with different provenance; rename the run directory before merging", id)
		}
	}
	return merged, skipped, rows.Err()
}

func prepareMergeRunFilter(tx *sql.Tx, runIDs []string) error {
	if _, err := tx.Exec(`CREATE TEMP TABLE merge_runs (id TEXT PRIMARY KEY)`); err != nil {
		return err
	}
	for _, id := range runIDs {
		if _, err := tx.Exec(`INSERT INTO temp.merge_runs (id) VALUES (?)`, id); err != nil {
			return err
		}
	}
	return nil
}

// mergeOffsets snapshots the destination's max integer id for every table
// whose rows are referenced by id, so copied rows keep consistent references
// after shifting: new id = old id + offset.
func mergeOffsets(tx *sql.Tx) (map[string]int64, error) {
	offsets := map[string]int64{}
	for _, table := range []string{"source_records", "artifacts", "phases", "measurements", "requests", "telemetry_series"} {
		var max sql.NullInt64
		if err := tx.QueryRow(fmt.Sprintf(`SELECT MAX(id) FROM main.%s`, table)).Scan(&max); err != nil {
			return nil, err
		}
		offsets[table] = max.Int64
	}
	return offsets, nil
}

// rescope namespaces a text dimension id under its run unless it already is:
// old single-run artifacts store bare names, model-level artifacts store
// run-prefixed ids. NULL columns stay NULL through the CASE arithmetic.
func rescope(column string) string {
	return fmt.Sprintf("CASE WHEN instr(%s, s.run_id || '/') = 1 THEN %s ELSE s.run_id || '/' || %s END", column, column, column)
}

// shift moves a nullable integer reference by the destination offset; NULL +
// offset stays NULL.
func shift(column string, offset int64) string {
	return fmt.Sprintf("%s + %d", column, offset)
}

func mergeStatements(offsets map[string]int64) []string {
	inMergedRuns := "s.run_id IN (SELECT id FROM temp.merge_runs)"
	statements := []string{
		`INSERT INTO main.run SELECT s.* FROM src.run s WHERE s.id IN (SELECT id FROM temp.merge_runs)`,
		fmt.Sprintf(`INSERT INTO main.specs (run_id, kind, format, content, sha256, created_at)
			SELECT s.run_id, s.kind, s.format, s.content, s.sha256, s.created_at FROM src.specs s WHERE %s`, inMergedRuns),
		fmt.Sprintf(`INSERT INTO main.engines (id, run_id, name, type, managed, command, version, git_commit, endpoint_base_url, env_json, metadata_json)
			SELECT %s, s.run_id, s.name, s.type, s.managed, s.command, s.version, s.git_commit, s.endpoint_base_url, s.env_json, s.metadata_json
			FROM src.engines s WHERE %s`, rescope("s.id"), inMergedRuns),
		fmt.Sprintf(`INSERT INTO main.profiles (id, run_id, engine_id, name, model, host, port, endpoint_base_url, managed,
				context_window, max_num_seqs, max_num_batched_tokens, gpu_memory_utilization, enable_sleep_mode, sleep_level,
				serve_json, engine_args_json, env_json, metadata_json)
			SELECT %s, s.run_id, %s, s.name, s.model, s.host, s.port, s.endpoint_base_url, s.managed,
				s.context_window, s.max_num_seqs, s.max_num_batched_tokens, s.gpu_memory_utilization, s.enable_sleep_mode, s.sleep_level,
				s.serve_json, s.engine_args_json, s.env_json, s.metadata_json
			FROM src.profiles s WHERE %s`, rescope("s.id"), rescope("s.engine_id"), inMergedRuns),
		fmt.Sprintf(`INSERT INTO main.workloads (id, run_id, name, role, phase, traffic_json, concurrency_json, samples, repeats,
				save_detailed, capture_payload_artifacts, dataset_json, request_json, metadata_json)
			SELECT %s, s.run_id, s.name, s.role, s.phase, s.traffic_json, s.concurrency_json, s.samples, s.repeats,
				s.save_detailed, s.capture_payload_artifacts, s.dataset_json, s.request_json, s.metadata_json
			FROM src.workloads s WHERE %s`, rescope("s.id"), inMergedRuns),
		fmt.Sprintf(`INSERT INTO main.datasets (id, run_id, workload_id, type, uri, path, split, selection, sample_count, seed,
				config_json, canonical_path, rendered_path, request_count, sha256, metadata_json)
			SELECT %s, s.run_id, %s, s.type, s.uri, s.path, s.split, s.selection, s.sample_count, s.seed,
				s.config_json, s.canonical_path, s.rendered_path, s.request_count, s.sha256, s.metadata_json
			FROM src.datasets s WHERE %s`, rescope("s.id"), rescope("s.workload_id"), inMergedRuns),
		fmt.Sprintf(`INSERT INTO main.source_records (id, run_id, dataset_id, source_record_id, ordinal, content_json, sha256, metadata_json)
			SELECT %s, s.run_id, %s, s.source_record_id, s.ordinal, s.content_json, s.sha256, s.metadata_json
			FROM src.source_records s WHERE %s`, shift("s.id", offsets["source_records"]), rescope("s.dataset_id"), inMergedRuns),
		fmt.Sprintf(`INSERT INTO main.canonical_requests (id, run_id, dataset_id, source_record_row_id, workload_id, request_id,
				ordinal, conversation_id, turn_index, mode, prompt_sha256, max_output_tokens, input_tokens_expected,
				output_tokens_expected, arrival_offset_ms, messages_json, attachments_json, metadata_json, canonical_json)
			SELECT %s, s.run_id, %s, %s, %s, s.request_id,
				s.ordinal, s.conversation_id, s.turn_index, s.mode, s.prompt_sha256, s.max_output_tokens, s.input_tokens_expected,
				s.output_tokens_expected, s.arrival_offset_ms, s.messages_json, s.attachments_json, s.metadata_json, s.canonical_json
			FROM src.canonical_requests s WHERE %s`,
			rescope("s.id"), rescope("s.dataset_id"), shift("s.source_record_row_id", offsets["source_records"]), rescope("s.workload_id"), inMergedRuns),
		fmt.Sprintf(`INSERT INTO main.artifacts (id, run_id, kind, name, media_type, compression, content, content_size_bytes,
				uncompressed_size_bytes, sha256, original_path, created_at, metadata_json)
			SELECT %s, s.run_id, s.kind, s.name, s.media_type, s.compression, s.content, s.content_size_bytes,
				s.uncompressed_size_bytes, s.sha256, s.original_path, s.created_at, s.metadata_json
			FROM src.artifacts s WHERE %s`, shift("s.id", offsets["artifacts"]), inMergedRuns),
		fmt.Sprintf(`INSERT INTO main.phases (id, run_id, profile_id, workload_id, name, type, status, started_at, completed_at, metadata_json)
			SELECT %s, s.run_id, %s, %s, s.name, s.type, s.status, s.started_at, s.completed_at, s.metadata_json
			FROM src.phases s WHERE %s`, shift("s.id", offsets["phases"]), rescope("s.profile_id"), rescope("s.workload_id"), inMergedRuns),
		fmt.Sprintf(`INSERT INTO main.measurements (id, run_id, profile_id, workload_id, phase_id, repeat_index, concurrency,
				samples_requested, status, started_at, completed_at, wall_time_ms, completed_requests, failed_requests,
				prompt_tokens, completion_tokens, total_tokens, aggregate_output_tok_s, per_user_output_tok_s,
				aggregate_total_tok_s, raw_result_artifact_id, error_type, error_message, metadata_json)
			SELECT %s, s.run_id, %s, %s, %s, s.repeat_index, s.concurrency,
				s.samples_requested, s.status, s.started_at, s.completed_at, s.wall_time_ms, s.completed_requests, s.failed_requests,
				s.prompt_tokens, s.completion_tokens, s.total_tokens, s.aggregate_output_tok_s, s.per_user_output_tok_s,
				s.aggregate_total_tok_s, %s, s.error_type, s.error_message, s.metadata_json
			FROM src.measurements s WHERE %s`,
			shift("s.id", offsets["measurements"]), rescope("s.profile_id"), rescope("s.workload_id"),
			shift("s.phase_id", offsets["phases"]), shift("s.raw_result_artifact_id", offsets["artifacts"]), inMergedRuns),
		fmt.Sprintf(`INSERT INTO main.metric_stats (measurement_id, metric, unit, mean, stddev, min, p50, p90, p95, p99, max, count, metadata_json)
			SELECT %s, s.metric, s.unit, s.mean, s.stddev, s.min, s.p50, s.p90, s.p95, s.p99, s.max, s.count, s.metadata_json
			FROM src.metric_stats s
			WHERE s.measurement_id IN (SELECT m.id FROM src.measurements m WHERE m.run_id IN (SELECT id FROM temp.merge_runs))`,
			shift("s.measurement_id", offsets["measurements"])),
		fmt.Sprintf(`INSERT INTO main.requests (id, measurement_id, request_index, request_id, status, streamed, http_status_code,
				started_at, first_byte_at, first_byte_ms, first_token_at, completed_at, latency_ms, ttft_ms, tpot_ms, itl_mean_ms,
				prompt_tokens, completion_tokens, total_tokens, output_tok_s, total_tok_s, prompt_sha256, response_sha256,
				prompt_artifact_id, response_artifact_id, error_type, error_code, error_message, response_metadata_json)
			SELECT %s, %s, s.request_index, s.request_id, s.status, s.streamed, s.http_status_code,
				s.started_at, s.first_byte_at, s.first_byte_ms, s.first_token_at, s.completed_at, s.latency_ms, s.ttft_ms, s.tpot_ms, s.itl_mean_ms,
				s.prompt_tokens, s.completion_tokens, s.total_tokens, s.output_tok_s, s.total_tok_s, s.prompt_sha256, s.response_sha256,
				%s, %s, s.error_type, s.error_code, s.error_message, s.response_metadata_json
			FROM src.requests s
			WHERE s.measurement_id IN (SELECT m.id FROM src.measurements m WHERE m.run_id IN (SELECT id FROM temp.merge_runs))`,
			shift("s.id", offsets["requests"]), shift("s.measurement_id", offsets["measurements"]),
			shift("s.prompt_artifact_id", offsets["artifacts"]), shift("s.response_artifact_id", offsets["artifacts"])),
		fmt.Sprintf(`INSERT INTO main.request_stream_events (request_row_id, event_index, timestamp, kind, token_count_delta, text_byte_count_delta, metadata_json)
			SELECT %s, s.event_index, s.timestamp, s.kind, s.token_count_delta, s.text_byte_count_delta, s.metadata_json
			FROM src.request_stream_events s
			WHERE s.request_row_id IN (SELECT r.id FROM src.requests r JOIN src.measurements m ON m.id = r.measurement_id
				WHERE m.run_id IN (SELECT id FROM temp.merge_runs))`,
			shift("s.request_row_id", offsets["requests"])),
		fmt.Sprintf(`INSERT INTO main.telemetry_series (id, run_id, source, metric, unit, target, tags_json)
			SELECT %s, s.run_id, s.source, s.metric, s.unit, s.target, s.tags_json
			FROM src.telemetry_series s WHERE %s`, shift("s.id", offsets["telemetry_series"]), inMergedRuns),
		fmt.Sprintf(`INSERT INTO main.telemetry_samples (series_id, timestamp, value, phase_id, measurement_id)
			SELECT %s, s.timestamp, s.value, %s, %s
			FROM src.telemetry_samples s
			WHERE s.series_id IN (SELECT t.id FROM src.telemetry_series t WHERE t.run_id IN (SELECT id FROM temp.merge_runs))`,
			shift("s.series_id", offsets["telemetry_series"]), shift("s.phase_id", offsets["phases"]), shift("s.measurement_id", offsets["measurements"])),
		fmt.Sprintf(`INSERT INTO main.events (run_id, timestamp, level, type, phase_id, profile_id, workload_id, measurement_id, message, data_json)
			SELECT s.run_id, s.timestamp, s.level, s.type, %s, %s, %s, %s, s.message, s.data_json
			FROM src.events s WHERE %s`,
			shift("s.phase_id", offsets["phases"]), rescope("s.profile_id"), rescope("s.workload_id"),
			shift("s.measurement_id", offsets["measurements"]), inMergedRuns),
		fmt.Sprintf(`INSERT INTO main.commands (run_id, phase_id, measurement_id, profile_id, phase, cwd, argv_json, env_json,
				started_at, completed_at, exit_code, status, stdout_artifact_id, stderr_artifact_id, metadata_json)
			SELECT s.run_id, %s, %s, %s, s.phase, s.cwd, s.argv_json, s.env_json,
				s.started_at, s.completed_at, s.exit_code, s.status, %s, %s, s.metadata_json
			FROM src.commands s WHERE %s`,
			shift("s.phase_id", offsets["phases"]), shift("s.measurement_id", offsets["measurements"]), rescope("s.profile_id"),
			shift("s.stdout_artifact_id", offsets["artifacts"]), shift("s.stderr_artifact_id", offsets["artifacts"]), inMergedRuns),
		fmt.Sprintf(`INSERT INTO main.reports (run_id, name, format, media_type, artifact_id, created_at)
			SELECT s.run_id, s.name, s.format, s.media_type, %s, s.created_at
			FROM src.reports s WHERE %s`, shift("s.artifact_id", offsets["artifacts"]), inMergedRuns),
	}
	return statements
}
