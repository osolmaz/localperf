package vllmbench

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/user"
	"path/filepath"
	"strings"
	"time"

	"github.com/osolmaz/localperf/internal/artifact"
)

const (
	sqliteFormatName    = artifact.FormatName
	sqliteFormatVersion = artifact.FormatVersion
)

// writeSQLiteArtifact writes the run into the artifact at artifactPath,
// appending when the artifact already exists so repeated runs of one model
// accumulate in a single model-level file; see
// docs/2026-07-02-default-inference-sweep.md. Re-running the same run
// directory replaces that run's rows instead of duplicating them.
func writeSQLiteArtifact(runDir, artifactPath string, spec Spec, summary RunSummary, originalSpecPath string) error {
	if strings.TrimSpace(artifactPath) == "" {
		return nil
	}
	ApplyDefaults(&spec)
	if err := ValidateSpec(spec); err != nil {
		return fmt.Errorf("refuse artifact write for invalid spec: %w", err)
	}
	return persistSQLiteArtifact(runDir, artifactPath, spec, summary, originalSpecPath)
}

func persistSQLiteArtifact(runDir, artifactPath string, spec Spec, summary RunSummary, originalSpecPath string) error {
	createdFresh := artifactPathIsFresh(artifactPath)
	db, err := createSQLiteArtifact(artifactPath)
	if err != nil {
		return err
	}
	defer db.Close()
	plan := BuildPlan(spec, runDir)
	if err := withSQLiteTx(db, func(tx *sql.Tx) error {
		return writeSQLiteRun(tx, runDir, spec, summary, plan, originalSpecPath)
	}); err != nil {
		return cleanupFailedArtifact(db, artifactPath, createdFresh, err)
	}
	if err := artifact.Check(artifactPath); err != nil {
		return cleanupFailedArtifact(db, artifactPath, createdFresh, err)
	}
	return nil
}

func artifactPathIsFresh(path string) bool {
	info, err := os.Stat(path)
	return err != nil || info.Size() == 0
}

func cleanupFailedArtifact(db *sql.DB, path string, createdFresh bool, writeErr error) error {
	if createdFresh {
		// A failed first write must not strand a schema-only file that later
		// appends would reject.
		_ = db.Close()
		_ = os.Remove(path)
	}
	return writeErr
}

func createSQLiteArtifact(path string) (*sql.DB, error) {
	return artifact.CreateOrAppend(path, artifact.Schema)
}

func withSQLiteTx(db *sql.DB, run func(*sql.Tx) error) error {
	return artifact.WithTx(db, run)
}

func writeSQLiteRun(tx *sql.Tx, runDir string, spec Spec, summary RunSummary, plan []PlannedRun, originalSpecPath string) error {
	now := time.Now().UTC()
	runID := sqliteRunID(runDir, spec)
	if err := insertRunMetadata(tx, now); err != nil {
		return err
	}
	if err := replaceExistingRun(tx, runID, runDir); err != nil {
		return err
	}
	if err := insertRunRow(tx, runID, runDir, spec, summary); err != nil {
		return err
	}
	if err := insertRunSpecs(tx, runID, runDir, spec, originalSpecPath, now); err != nil {
		return err
	}
	return insertRunData(tx, runID, runDir, spec, summary, plan, now)
}

func sqliteRunID(runDir string, spec Spec) string {
	runID := filepath.Base(filepath.Clean(runDir))
	if runID == "." || runID == "" {
		return spec.Name
	}
	return runID
}

// dimensionID namespaces dimension primary keys per run so multiple runs can
// share one model-level artifact; names alone would collide across runs.
func dimensionID(runID, name string) string {
	return runID + "/" + name
}

// dimensionRef is dimensionID for nullable foreign-key columns.
func dimensionRef(runID, name string) any {
	if strings.TrimSpace(name) == "" {
		return nil
	}
	return dimensionID(runID, name)
}

func insertRunMetadata(tx *sql.Tx, now time.Time) error {
	values := map[string]string{
		"format_name":    sqliteFormatName,
		"format_version": sqliteFormatVersion,
		"generated_at":   now.Format(time.RFC3339),
	}
	for key, value := range values {
		// Appended runs keep the existing metadata; generated_at records the
		// artifact's first write.
		if _, err := tx.Exec("INSERT OR IGNORE INTO metadata (key, value) VALUES (?, ?)", key, value); err != nil {
			return err
		}
	}
	return nil
}

// replaceExistingRun deletes a previous attempt of the same run directory
// (cascades wipe all child rows) so retries do not duplicate. Run ids are
// run-directory basenames, so a same-id run recorded from a *different*
// directory is a collision, not a retry; deleting it would silently destroy
// unrelated results.
func replaceExistingRun(tx *sql.Tx, runID, runDir string) error {
	var existingDir sql.NullString
	err := tx.QueryRow(`SELECT json_extract(labels_json, '$.run_dir') FROM run WHERE id = ?`, runID).Scan(&existingDir)
	if err == sql.ErrNoRows {
		return nil
	}
	if err != nil {
		return err
	}
	// A run with unknown provenance (no run_dir label) is also a collision;
	// rerun it under the current format.
	if !existingDir.Valid || existingDir.String != absolutePathOrSelf(runDir) {
		return fmt.Errorf("run id %q already exists in this artifact from a different or unknown run directory; rename the run directory or start a fresh artifact", runID)
	}
	_, err = tx.Exec(`DELETE FROM run WHERE id = ?`, runID)
	return err
}

func absolutePathOrSelf(path string) string {
	if absolute, err := filepath.Abs(path); err == nil {
		return absolute
	}
	return path
}

func insertRunRow(tx *sql.Tx, runID, runDir string, spec Spec, summary RunSummary) error {
	hostname, _ := os.Hostname()
	username := currentUsername()
	cwd, _ := os.Getwd()
	_, err := tx.Exec(`INSERT INTO run (
		id, name, description, status, created_at, started_at, completed_at,
		hostname, username, cwd, command_line_json, host_json, labels_json
	) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		runID, spec.Name, spec.Description, runStatus(summary), summary.StartedAt.Format(time.RFC3339),
		timeOrNull(summary.StartedAt), timeOrNull(summary.FinishedAt), hostname, username, cwd,
		mustJSONString(os.Args), mustJSONString(CollectHostInfo(context.Background())),
		mustJSONString(map[string]string{"run_dir": absolutePathOrSelf(runDir)}))
	return err
}

func runStatus(summary RunSummary) string {
	// Incremental snapshots record the run as still in flight.
	if summary.InProgress {
		return "running"
	}
	if strings.TrimSpace(summary.Error) != "" {
		return "failed"
	}
	if summary.CompletedRuns > 0 {
		return "completed"
	}
	if summary.FailedRuns > 0 {
		return "failed"
	}
	return "completed"
}

func currentUsername() string {
	currentUser, _ := user.Current()
	if currentUser == nil {
		return ""
	}
	return currentUser.Username
}

func insertRunSpecs(tx *sql.Tx, runID, runDir string, spec Spec, originalSpecPath string, now time.Time) error {
	specData, err := json.MarshalIndent(RedactedSpec(spec), "", "  ")
	if err != nil {
		return err
	}
	originalData, err := originalSpecBytes(originalSpecPath, specData)
	if err != nil {
		return err
	}
	if err := insertSpec(tx, runID, "original", originalData, now); err != nil {
		return err
	}
	normalizedData, err := os.ReadFile(filepath.Join(runDir, "spec.normalized.json"))
	if err != nil {
		return fmt.Errorf("read normalized spec: %w", err)
	}
	return insertSpec(tx, runID, "normalized", normalizedData, now)
}

func insertRunData(tx *sql.Tx, runID, runDir string, spec Spec, _ RunSummary, plan []PlannedRun, now time.Time) error {
	if err := insertRunDimensions(tx, runID, runDir, spec); err != nil {
		return err
	}
	events, err := readEvents(filepath.Join(runDir, "events.jsonl"))
	if err != nil {
		return fmt.Errorf("read run events: %w", err)
	}
	return insertRunExecutionData(tx, runID, runDir, spec, plan, events, now)
}

func insertRunDimensions(tx *sql.Tx, runID, runDir string, spec Spec) error {
	for _, insert := range []func(*sql.Tx, string, Spec) error{insertEngines, insertProfiles, insertWorkloads} {
		if err := insert(tx, runID, spec); err != nil {
			return err
		}
	}
	return insertCanonicalDatasets(tx, runID, runDir, spec)
}

func insertRunExecutionData(tx *sql.Tx, runID, runDir string, spec Spec, plan []PlannedRun, events []Event, now time.Time) error {
	if err := applyEngineIdentityEvents(tx, runID, spec, events); err != nil {
		return err
	}
	artifactIDs, err := insertRunArtifacts(tx, runID, runDir, spec, events)
	if err != nil {
		return err
	}
	phaseIDs, err := insertPhases(tx, runID, plan, events)
	if err != nil {
		return err
	}
	measurementIDs, err := insertMeasurements(tx, runID, runDir, plan, events, artifactIDs, phaseIDs)
	if err != nil {
		return err
	}
	return insertRunEventData(tx, runID, events, phaseIDs, measurementIDs, artifactIDs, now)
}

func insertRunEventData(tx *sql.Tx, runID string, events []Event, phaseIDs, measurementIDs, artifactIDs map[string]int64, now time.Time) error {
	if err := insertEvents(tx, runID, events, phaseIDs, measurementIDs); err != nil {
		return err
	}
	if err := insertCommands(tx, runID, events, phaseIDs, measurementIDs, artifactIDs); err != nil {
		return err
	}
	return insertTelemetry(tx, runID, events, phaseIDs, measurementIDs)
}

func insertSpec(tx *sql.Tx, runID, kind string, data []byte, createdAt time.Time) error {
	content := strings.TrimSpace(string(data))
	_, err := tx.Exec(`INSERT INTO specs (run_id, kind, format, content, sha256, created_at)
		VALUES (?, ?, 'json', ?, ?, ?)`,
		runID, kind, content, sha256Hex([]byte(content)), createdAt.Format(time.RFC3339))
	return err
}

func originalSpecBytes(path string, fallback []byte) ([]byte, error) {
	if strings.TrimSpace(path) == "" {
		return fallback, nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	redacted, err := redactedJSONDocument(data)
	if err != nil {
		return nil, err
	}
	return redacted, nil
}

func redactedJSONDocument(data []byte) ([]byte, error) {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	var value any
	if err := decoder.Decode(&value); err != nil {
		return nil, err
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		return nil, fmt.Errorf("extra content after JSON document")
	}
	value = redactSensitiveJSONValue(value)
	return json.MarshalIndent(value, "", "  ")
}

func redactSensitiveJSONValue(value any) any {
	switch typed := value.(type) {
	case map[string]any:
		for key, child := range typed {
			if strings.EqualFold(key, "env") {
				typed[key] = redactJSONEnv(child)
				continue
			}
			if isSensitiveJSONKey(key) {
				typed[key] = "<redacted>"
				continue
			}
			typed[key] = redactSensitiveJSONValue(child)
		}
		return typed
	case []any:
		for i, child := range typed {
			typed[i] = redactSensitiveJSONValue(child)
		}
		return typed
	default:
		return value
	}
}

func redactJSONEnv(value any) any {
	switch typed := value.(type) {
	case map[string]any:
		for key, child := range typed {
			if isSensitiveEnvKey(key) {
				typed[key] = "<redacted>"
				continue
			}
			typed[key] = child
		}
		return typed
	default:
		return redactSensitiveJSONValue(value)
	}
}

func isSensitiveJSONKey(key string) bool {
	upper := strings.ToUpper(strings.ReplaceAll(key, "-", "_"))
	switch upper {
	case "AUTH", "AUTHORIZATION", "COOKIE", "CREDENTIAL", "CREDENTIALS", "KEY", "PASS", "PASSWORD", "SECRET", "TOKEN",
		"API_KEY", "ACCESS_TOKEN", "REFRESH_TOKEN", "CLIENT_SECRET":
		return true
	}
	for _, suffix := range []string{"_API_KEY", "_TOKEN", "_SECRET", "_PASSWORD", "_CREDENTIAL", "_CREDENTIALS"} {
		if strings.HasSuffix(upper, suffix) {
			return true
		}
	}
	return false
}

func insertEngines(tx *sql.Tx, runID string, spec Spec) error {
	for _, engine := range spec.Engines {
		managed := boolToInt(engine.Type == "vllm-managed")
		if engine.Managed != nil {
			managed = boolToInt(*engine.Managed)
		}
		if _, err := tx.Exec(`INSERT INTO engines (
			id, run_id, name, type, managed, command, endpoint_base_url, env_json, metadata_json
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			dimensionID(runID, engine.Name), runID, engine.Name, engine.Type, managed, engine.Command,
			emptyNull(engine.EndpointBaseURL), nullableJSON(redactedEnv(engine.Env)), nullableJSON(engine.Metadata)); err != nil {
			return err
		}
	}
	return nil
}

func insertProfiles(tx *sql.Tx, runID string, spec Spec) error {
	for _, profile := range spec.Profiles {
		serveJSON := mustJSONString(map[string]any{
			"max_model_len":          profile.MaxModelLen,
			"max_num_seqs":           profile.MaxNumSeqs,
			"max_num_batched_tokens": profile.MaxNumBatchedTokens,
			"gpu_memory_utilization": profile.GPUMemoryUtilization,
			"kv_cache_dtype":         profile.KVCacheDType,
			"attention_backend":      profile.AttentionBackend,
			"moe_backend":            profile.MoEBackend,
			"enable_prefix_caching":  profile.EnablePrefixCaching,
			"enable_sleep_mode":      profile.EnableSleepMode,
			"sleep_level":            SleepLevelValue(profile),
		})
		if _, err := tx.Exec(`INSERT INTO profiles (
			id, run_id, engine_id, name, model, host, port, endpoint_base_url,
			managed, context_window, max_num_seqs, max_num_batched_tokens,
			gpu_memory_utilization, enable_sleep_mode, sleep_level,
			serve_json, engine_args_json, env_json
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			dimensionID(runID, profile.Name), runID, dimensionID(runID, profile.Engine), profile.Name, profile.Model, profile.Host, profile.Port,
			baseURL(profile), boolToInt(profile.Managed), intNull(profile.MaxModelLen), intNull(profile.MaxNumSeqs),
			intNull(profile.MaxNumBatchedTokens), floatNull(profile.GPUMemoryUtilization), boolToInt(profile.EnableSleepMode),
			SleepLevelValue(profile), serveJSON, nullableJSON(profileExtraArgs(profile)), nullableJSON(redactedEnv(profile.Env))); err != nil {
			return err
		}
	}
	return nil
}

func insertWorkloads(tx *sql.Tx, runID string, spec Spec) error {
	for _, workload := range spec.Workloads {
		trafficJSON := mustJSONString(workload.BenchmarkTrafficConfig)
		concurrencyJSON := mustJSONString(workload.MaxConcurrency)
		if _, err := tx.Exec(`INSERT INTO workloads (
			id, run_id, name, role, phase, traffic_json, concurrency_json, samples, repeats,
			save_detailed, capture_payload_artifacts, dataset_json, request_json, metadata_json
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			dimensionID(runID, workload.Name), runID, workload.Name, workload.Role, workload.Phase, trafficJSON, concurrencyJSON, workloadSampleCount(workload),
			workload.Repeats, boolToInt(boolValue(workload.BenchmarkTrafficConfig.SaveDetailed)),
			boolToInt(workload.CapturePayloadArtifacts), structuredWorkloadJSON(workload, workload.Dataset),
			structuredWorkloadJSON(workload, workload.Request), workloadClaimsJSON(workload)); err != nil {
			return err
		}
	}
	return nil
}

func structuredWorkloadJSON(workload Workload, value any) any {
	if !hasStructuredDataset(workload) {
		return nil
	}
	return nullableJSON(value)
}

// workloadSampleCount records the largest point's request count for
// workloads whose prompts scale with concurrency.
func workloadSampleCount(workload Workload) int {
	if workload.NumPrompts > 0 {
		return workload.NumPrompts
	}
	return resolvedNumPrompts(workload, largestConcurrency(workload))
}

// workloadClaimsJSON stores declared workload claims keyed by claim type in
// workloads.metadata_json; traffic_json stays strictly engine input. See
// docs/2026-07-02-context-semantics.md.
func workloadClaimsJSON(workload Workload) any {
	claims := map[string]any{}
	if workload.ContextTarget > 0 && workload.ContextSemantics != "" {
		claims["context"] = map[string]any{
			"target":    workload.ContextTarget,
			"semantics": workload.ContextSemantics,
		}
	}
	if workload.SLO != nil {
		claims["slo"] = workload.SLO
	}
	if len(claims) == 0 {
		return nil
	}
	return nullableJSON(claims)
}

func insertCanonicalDatasets(tx *sql.Tx, runID, runDir string, spec Spec) error {
	for _, workload := range spec.Workloads {
		if !hasStructuredDataset(workload) || strings.TrimSpace(workload.Dataset.Prepared.CanonicalPath) == "" {
			continue
		}
		if err := insertCanonicalDataset(tx, runID, runDir, workload); err != nil {
			return err
		}
	}
	return nil
}

func insertCanonicalDataset(tx *sql.Tx, runID, runDir string, workload Workload) error {
	datasetID := dimensionID(runID, firstNonEmpty(workload.Dataset.Prepared.DatasetID, datasetIDForWorkload(workload.Name)))
	requests, err := readCanonicalRequestFile(resolveResultPath(runDir, workload.Dataset.Prepared.CanonicalPath))
	if err != nil {
		return err
	}
	_, err = tx.Exec(`INSERT INTO datasets (
		id, run_id, workload_id, type, uri, path, split, selection, sample_count,
		seed, config_json, canonical_path, rendered_path, request_count, sha256
	) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		datasetID, runID, dimensionID(runID, workload.Name), workload.Dataset.Type, emptyNull(workload.Dataset.URI),
		emptyNull(workload.Dataset.Path), emptyNull(workload.Dataset.Split), emptyNull(workload.Dataset.Selection),
		workload.Dataset.SampleCount, intPointerNull(workload.Dataset.Seed), mustJSONString(workload.Dataset),
		workload.Dataset.Prepared.CanonicalPath, emptyNull(workload.Dataset.Prepared.VLLMCustomPath),
		len(requests), workload.Dataset.Prepared.SHA256)
	if err != nil {
		return err
	}
	if !workload.CapturePayloadArtifacts {
		return nil
	}
	sourceIDs := map[string]int64{}
	for _, request := range requests {
		sourceID, err := insertSourceRecord(tx, runID, datasetID, request, sourceIDs)
		if err != nil {
			return err
		}
		if err := insertCanonicalRequest(tx, runID, datasetID, dimensionID(runID, workload.Name), sourceID, request); err != nil {
			return err
		}
	}
	return nil
}

func readCanonicalRequestFile(path string) ([]CanonicalRequest, error) {
	rawRows, err := readJSONLines(path)
	if err != nil {
		return nil, err
	}
	requests := make([]CanonicalRequest, 0, len(rawRows))
	for _, raw := range rawRows {
		var request CanonicalRequest
		if err := json.Unmarshal(raw, &request); err != nil {
			return nil, err
		}
		requests = append(requests, request)
	}
	return requests, nil
}

func insertSourceRecord(tx *sql.Tx, runID, datasetID string, request CanonicalRequest, seen map[string]int64) (int64, error) {
	sourceRecordID := firstNonEmpty(request.SourceRecordID, request.ID)
	if id := seen[sourceRecordID]; id != 0 {
		return id, nil
	}
	content := request.Raw
	if len(content) == 0 {
		content = mustJSON(request)
	}
	result, err := tx.Exec(`INSERT INTO source_records (
		run_id, dataset_id, source_record_id, ordinal, content_json, sha256, metadata_json
	) VALUES (?, ?, ?, ?, ?, ?, ?)`,
		runID, datasetID, sourceRecordID, request.Ordinal, string(content),
		sha256Hex(content), nullableJSON(request.Metadata))
	if err != nil {
		return 0, err
	}
	id, _ := result.LastInsertId()
	seen[sourceRecordID] = id
	return id, nil
}

func insertCanonicalRequest(tx *sql.Tx, runID, datasetID, workloadID string, sourceRecordRowID int64, request CanonicalRequest) error {
	prompt := request.Prompt
	if strings.TrimSpace(prompt) == "" {
		prompt = messagesPrompt(request.Messages)
	}
	_, err := tx.Exec(`INSERT INTO canonical_requests (
		id, run_id, dataset_id, source_record_row_id, workload_id, request_id, ordinal,
		conversation_id, turn_index, mode, prompt_sha256, max_output_tokens,
		input_tokens_expected, output_tokens_expected, arrival_offset_ms,
		messages_json, attachments_json, metadata_json, canonical_json
	) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		canonicalRequestRowID(datasetID, request), runID, datasetID, sourceRecordRowID, workloadID, request.ID, request.Ordinal,
		emptyNull(request.ConversationID), request.TurnIndex, request.Mode, emptyNull(sha256Maybe(prompt)),
		intNull(request.MaxOutputTokens), intNull(request.InputTokensExpected), intNull(request.OutputTokensExpected),
		request.ArrivalOffsetMillis, nullableJSON(request.Messages), nullableJSON(request.Attachments),
		nullableJSON(request.Metadata), mustJSONString(request))
	return err
}

func canonicalRequestRowID(datasetID string, request CanonicalRequest) string {
	return fmt.Sprintf("%s:%06d:%s", datasetID, request.Ordinal, firstNonEmpty(request.ID, "request"))
}

func insertRunArtifacts(tx *sql.Tx, runID, runDir string, spec Spec, events []Event) (map[string]int64, error) {
	inserter := artifactInserter{tx: tx, runID: runID, runDir: runDir, ids: map[string]int64{}}
	if err := inserter.addDefaultArtifacts(); err != nil {
		return nil, err
	}
	if err := inserter.addDatasetArtifacts(spec); err != nil {
		return nil, err
	}
	if err := inserter.addEventArtifacts(events); err != nil {
		return nil, err
	}
	return inserter.ids, nil
}

type artifactInserter struct {
	tx     *sql.Tx
	runID  string
	runDir string
	ids    map[string]int64
}

type artifactSpec struct {
	kind      string
	name      string
	path      string
	mediaType string
}

func (inserter artifactInserter) addDefaultArtifacts() error {
	for _, artifact := range []artifactSpec{
		{"debug", "events.jsonl", filepath.Join(inserter.runDir, "events.jsonl"), "application/x-ndjson"},
		{"debug", "summary.json", filepath.Join(inserter.runDir, "summary.json"), "application/json"},
	} {
		if err := inserter.add(artifact); err != nil {
			return err
		}
	}
	return nil
}

func (inserter artifactInserter) addDatasetArtifacts(spec Spec) error {
	for _, workload := range spec.Workloads {
		for _, artifact := range datasetArtifactsForWorkload(workload) {
			if err := inserter.add(artifact); err != nil {
				return err
			}
		}
	}
	return nil
}

func datasetArtifactsForWorkload(workload Workload) []artifactSpec {
	if !workload.CapturePayloadArtifacts {
		return nil
	}
	prepared := workload.Dataset.Prepared
	rows := []struct {
		kind string
		path string
	}{
		{"canonical_dataset", prepared.CanonicalPath},
		{"engine_dataset", prepared.VLLMCustomPath},
	}
	artifacts := make([]artifactSpec, 0, len(rows))
	for _, row := range rows {
		if strings.TrimSpace(row.path) == "" {
			continue
		}
		artifacts = append(artifacts, artifactSpec{
			kind:      row.kind,
			name:      filepath.Base(row.path),
			path:      row.path,
			mediaType: "application/x-ndjson",
		})
	}
	return artifacts
}

func (inserter artifactInserter) addEventArtifacts(events []Event) error {
	for _, event := range events {
		if eventHasArtifactResult(event) {
			name := rawResultArtifactName(event)
			if err := inserter.add(artifactSpec{"bench_raw_result", name, event.ResultFile, "application/json"}); err != nil {
				return err
			}
		}
		if event.LogFile != "" {
			name := filepath.Base(event.LogFile)
			if err := inserter.add(artifactSpec{"server_log", name, event.LogFile, "text/plain"}); err != nil {
				return err
			}
		}
	}
	return nil
}

func (inserter artifactInserter) add(artifact artifactSpec) error {
	if strings.TrimSpace(artifact.path) == "" {
		return nil
	}
	resolved, ok, err := existingArtifactPath(inserter.runDir, artifact.path)
	if err != nil || !ok {
		return err
	}
	id, err := insertArtifactPath(inserter.tx, inserter.runID, artifact.kind, artifact.name, resolved, artifact.mediaType)
	if err != nil {
		return err
	}
	inserter.recordPathIDs(artifact.path, resolved, id)
	return nil
}

func existingArtifactPath(runDir, path string) (string, bool, error) {
	resolved := resolveResultPath(runDir, path)
	if _, err := os.Stat(resolved); err != nil {
		return resolved, false, nonMissingFileError(err)
	}
	return resolved, true, nil
}

func nonMissingFileError(err error) error {
	if os.IsNotExist(err) {
		return nil
	}
	return err
}

func (inserter artifactInserter) recordPathIDs(original, resolved string, id int64) {
	inserter.ids[resolved] = id
	inserter.ids[original] = id
	if rel, err := filepath.Rel(inserter.runDir, resolved); err == nil {
		inserter.ids[rel] = id
	}
}

func rawResultArtifactName(event Event) string {
	profile := Slug(event.Profile)
	if profile == "" {
		profile = "profile"
	}
	workload := Slug(event.Workload)
	if workload == "" {
		workload = "workload"
	}
	return fmt.Sprintf("result-%s__%s__c%d__r%d.json", profile, workload, event.Concurrency, event.Repeat+1)
}

func insertArtifactPath(tx *sql.Tx, runID, kind, name, path, mediaType string) (int64, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return 0, err
	}
	content, compression, err := artifactContent(data, mediaType)
	if err != nil {
		return 0, err
	}
	result, err := tx.Exec(`INSERT OR IGNORE INTO artifacts (
		run_id, kind, name, media_type, compression, content, content_size_bytes,
		uncompressed_size_bytes, sha256, original_path, created_at
	) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		runID, kind, name, mediaType, compression, content, len(content), len(data),
		sha256Hex(data), path, time.Now().UTC().Format(time.RFC3339))
	if err != nil {
		return 0, err
	}
	id, _ := result.LastInsertId()
	if id != 0 {
		return id, nil
	}
	err = tx.QueryRow(`SELECT id FROM artifacts WHERE run_id = ? AND kind = ? AND name = ?`, runID, kind, name).Scan(&id)
	return id, err
}

func artifactContent(data []byte, mediaType string) ([]byte, string, error) {
	return artifact.Content(data, mediaType)
}

func insertPhases(tx *sql.Tx, runID string, plan []PlannedRun, events []Event) (map[string]int64, error) {
	phaseIDs := map[string]int64{}
	for _, planned := range plan {
		key := measurementKey(planned.Profile.Name, planned.Workload.Name, planned.Concurrency, planned.Repeat)
		startedAt, completedAt := measurementTimes(events, planned)
		status := measurementStatus(events, planned)
		id, err := insertPhase(tx, runID, planned.Profile.Name, planned.Workload.Name,
			fmt.Sprintf("%s/%s c%d r%d", planned.Profile.Name, planned.Workload.Name, planned.Concurrency, planned.Repeat+1),
			"measurement", status, startedAt, completedAt)
		if err != nil {
			return nil, err
		}
		phaseIDs[key] = id
	}
	return phaseIDs, nil
}

func insertPhase(tx *sql.Tx, runID, profile, workload, name, typ, status string, startedAt, completedAt *time.Time) (int64, error) {
	result, err := tx.Exec(`INSERT INTO phases (
		run_id, profile_id, workload_id, name, type, status, started_at, completed_at
	) VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		runID, dimensionRef(runID, profile), dimensionRef(runID, workload), name, typ, status, timePtrString(startedAt), timePtrString(completedAt))
	if err != nil {
		return 0, err
	}
	return result.LastInsertId()
}

func insertMeasurements(tx *sql.Tx, runID, runDir string, plan []PlannedRun, events []Event, artifactIDs map[string]int64, phaseIDs map[string]int64) (map[string]int64, error) {
	measurementIDs := map[string]int64{}
	reportRows := rowsByMeasurement(runDir, events)
	for _, planned := range plan {
		key := measurementKey(planned.Profile.Name, planned.Workload.Name, planned.Concurrency, planned.Repeat)
		row := reportRows[key]
		applyWorkloadFields(&row, planned.Workload)
		startedAt, completedAt := measurementTimes(events, planned)
		id, err := insertMeasurement(tx, measurementInsert{
			runID:       runID,
			planned:     planned,
			row:         row,
			phaseID:     phaseIDs[key],
			rawID:       measurementRawArtifactID(events, planned, artifactIDs),
			status:      measurementStatus(events, planned),
			errorText:   measurementError(events, planned),
			startedAt:   startedAt,
			completedAt: completedAt,
		})
		if err != nil {
			return nil, err
		}
		measurementIDs[key] = id
		if err := insertMeasurementDetails(tx, runDir, id, row, measurementResultFile(events, planned)); err != nil {
			return nil, err
		}
	}
	return measurementIDs, nil
}

type measurementInsert struct {
	runID       string
	planned     PlannedRun
	row         ReportRow
	phaseID     int64
	rawID       int64
	status      string
	errorText   any
	startedAt   *time.Time
	completedAt *time.Time
}

func insertMeasurement(tx *sql.Tx, insert measurementInsert) (int64, error) {
	result, err := tx.Exec(`INSERT INTO measurements (
		run_id, profile_id, workload_id, phase_id, repeat_index, concurrency,
		samples_requested, status, started_at, completed_at, wall_time_ms,
		completed_requests, failed_requests, prompt_tokens, completion_tokens,
		total_tokens, aggregate_output_tok_s, per_user_output_tok_s,
		aggregate_total_tok_s, raw_result_artifact_id, error_type, error_message,
		metadata_json
	) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		insert.runID, dimensionID(insert.runID, insert.planned.Profile.Name), dimensionID(insert.runID, insert.planned.Workload.Name), zeroNullInt(insert.phaseID),
		insert.planned.Repeat, insert.planned.Concurrency, resolvedNumPrompts(insert.planned.Workload, insert.planned.Concurrency), insert.status,
		timePtrString(insert.startedAt), timePtrString(insert.completedAt), durationMillis(insert.startedAt, insert.completedAt),
		insert.row.Completed, insert.row.Failed, knownIntNull(insert.row.promptTokensKnown, insert.row.PromptTokens),
		knownIntNull(insert.row.completionTokensKnown, insert.row.CompletionTokens), knownIntNull(insert.row.totalTokensKnown, insert.row.TotalTokens),
		knownFloatNull(insert.row.outputTokensPerSecKnown, insert.row.OutputTokensPerSec),
		knownFloatNull(insert.row.perUserOutputTokSecKnown, insert.row.PerUserOutputTokSec),
		knownFloatNull(insert.row.totalTokensPerSecKnown, insert.row.TotalTokensPerSec),
		zeroNullInt(insert.rawID), nil, insert.errorText, measurementMetadataJSON(insert.row))
	if err != nil {
		return 0, err
	}
	return result.LastInsertId()
}

// measurementMetadataJSON records measurement-level provenance; today that
// is only the TTFT source marker reports require to trust TTFT stats.
func measurementMetadataJSON(row ReportRow) any {
	if strings.TrimSpace(row.TTFTSource) == "" {
		return nil
	}
	data, err := json.Marshal(map[string]string{"ttft_source": row.TTFTSource})
	if err != nil {
		return nil
	}
	return string(data)
}

func measurementRawArtifactID(events []Event, planned PlannedRun, artifactIDs map[string]int64) int64 {
	if event := artifactFinishEvent(events, planned); event.ResultFile != "" {
		return artifactIDForPath(artifactIDs, event.ResultFile)
	}
	return 0
}

func measurementResultFile(events []Event, planned PlannedRun) string {
	return importableFinishEvent(events, planned).ResultFile
}

func insertMeasurementDetails(tx *sql.Tx, runDir string, measurementID int64, row ReportRow, resultFile string) error {
	samples, err := measurementRequestSamples(runDir, resultFile)
	if err != nil {
		return err
	}
	if err := validateStreamedTTFTEvidence(row, samples); err != nil {
		return err
	}
	if err := insertRequestSamples(tx, measurementID, samples); err != nil {
		return err
	}
	return insertMetricStats(tx, measurementID, row, samples)
}

func measurementRequestSamples(runDir, resultFile string) ([]RequestSample, error) {
	samples, err := requestSamplesForResult(runDir, resultFile)
	if os.IsNotExist(err) {
		return nil, nil
	}
	return samples, err
}

func validateStreamedTTFTEvidence(row ReportRow, samples []RequestSample) error {
	if row.TTFTSource != TTFTSourceStream {
		return nil
	}
	for _, sample := range samples {
		if sample.Status == "completed" && sample.TTFTMillis > 0 && !sample.Streamed {
			return fmt.Errorf("request %d has TTFT but is not streamed", sample.RequestIndex)
		}
	}
	return nil
}

func insertMetricStats(tx *sql.Tx, measurementID int64, row ReportRow, samples []RequestSample) error {
	if err := insertAggregateMetricStats(tx, measurementID, row); err != nil {
		return err
	}
	if row.TTFTSource == TTFTSourceStream {
		if value, ok := measurementEffectivePrefillThroughput(samples); ok {
			if err := insertAggregateMetricStat(tx, measurementID, aggregateMetricStat{
				metric: "effective_prefill_throughput",
				unit:   "tok/s",
				mean:   value,
				count:  1,
			}); err != nil {
				return err
			}
		}
	}
	return insertSampleMetricStats(tx, measurementID, row, samples)
}

type aggregateMetricStat struct {
	metric string
	unit   string
	mean   float64
	p50    float64
	p95    float64
	p99    float64
	count  int
}

func insertAggregateMetricStats(tx *sql.Tx, measurementID int64, row ReportRow) error {
	for _, stat := range aggregateMetricStats(row) {
		if err := insertAggregateMetricStat(tx, measurementID, stat); err != nil {
			return err
		}
	}
	return nil
}

func aggregateMetricStats(row ReportRow) []aggregateMetricStat {
	stats := []aggregateMetricStat{
		{"tpot", "ms", row.MeanTPOTMillis, 0, 0, 0, row.Completed},
		{"output_throughput", "tok/s", row.OutputTokensPerSec, 0, 0, 0, 1},
		{"total_throughput", "tok/s", row.TotalTokensPerSec, 0, 0, 0, 1},
	}
	// TTFT persists only with trusted provenance: a run without streamed
	// samples has no TTFT, not a fallback approximation.
	if row.TTFTSource == TTFTSourceStream {
		stats = append(stats, aggregateMetricStat{
			"ttft", "ms", row.MeanTTFTMillis, row.P50TTFTMillis, row.P95TTFTMillis, row.P99TTFTMillis, row.Completed,
		})
	}
	return stats
}

func insertAggregateMetricStat(tx *sql.Tx, measurementID int64, stat aggregateMetricStat) error {
	if stat.mean == 0 && stat.p99 == 0 {
		return nil
	}
	_, err := tx.Exec(`INSERT INTO metric_stats (
		measurement_id, metric, unit, mean, p50, p95, p99, count
	) VALUES (?, ?, ?, ?, ?, ?, ?, ?)`, measurementID, stat.metric, stat.unit, stat.mean,
		floatNull(stat.p50), floatNull(stat.p95), floatNull(stat.p99), stat.count)
	return err
}

func insertSampleMetricStats(tx *sql.Tx, measurementID int64, row ReportRow, samples []RequestSample) error {
	for _, distribution := range sampleMetricDistributions(row, samples) {
		if distribution.stats.Count == 0 {
			continue
		}
		if err := insertMetricDistribution(tx, measurementID, distribution.metric, distribution.unit, distribution.stats); err != nil {
			return err
		}
	}
	return nil
}

type sampleMetricDistribution struct {
	metric string
	unit   string
	stats  numericStats
}

func sampleMetricDistributions(row ReportRow, samples []RequestSample) []sampleMetricDistribution {
	distributions := []sampleMetricDistribution{
		{"request_output_throughput", "tok/s", statsFromSamples(samples, true, func(sample RequestSample) float64 { return sample.OutputTokensPerSecond })},
		{"request_total_throughput", "tok/s", statsFromSamples(samples, true, func(sample RequestSample) float64 { return sample.TotalTokensPerSecond })},
		{"request_decode_throughput", "tok/s", statsFromSamples(samples, false, requestDecodeThroughput)},
		{"latency", "ms", statsFromSamples(samples, false, func(sample RequestSample) float64 { return sample.LatencyMillis })},
		{"first_byte", "ms", statsFromSamples(samples, false, func(sample RequestSample) float64 { return sample.FirstByteMillis })},
		{"request_ttft", "ms", statsFromSamples(samples, false, streamedTTFT)},
		{"request_tpot", "ms", statsFromSamples(samples, false, func(sample RequestSample) float64 { return sample.TPOTMillis })},
		{"request_itl_mean", "ms", statsFromSamples(samples, false, func(sample RequestSample) float64 { return sample.ITLMeanMillis })},
	}
	if row.TTFTSource == TTFTSourceStream {
		distributions = append(distributions, sampleMetricDistribution{
			"request_effective_prefill_throughput",
			"tok/s",
			statsFromSamples(samples, false, requestEffectivePrefillThroughput),
		})
	}
	return distributions
}

func requestEffectivePrefillThroughput(sample RequestSample) float64 {
	if !sample.Streamed || sample.PromptTokens <= 0 || sample.TTFTMillis <= 0 {
		return 0
	}
	return float64(sample.PromptTokens) / (sample.TTFTMillis / 1000)
}

func requestDecodeThroughput(sample RequestSample) float64 {
	if sample.TPOTMillis <= 0 {
		return 0
	}
	return 1000 / sample.TPOTMillis
}

func measurementEffectivePrefillThroughput(samples []RequestSample) (float64, bool) {
	var window effectivePrefillWindow
	for _, sample := range samples {
		if sample.Status != "completed" {
			continue
		}
		if !validEffectivePrefillSample(sample) {
			return 0, false
		}
		window.add(sample)
	}
	return window.throughput()
}

func validEffectivePrefillSample(sample RequestSample) bool {
	return sample.Streamed && sample.PromptTokens > 0 && !sample.StartedAt.IsZero() &&
		sample.FirstByteAt != nil && sample.FirstByteAt.After(sample.StartedAt)
}

type effectivePrefillWindow struct {
	promptTokens               int
	earliestStart, latestToken time.Time
}

func (window *effectivePrefillWindow) add(sample RequestSample) {
	window.promptTokens += sample.PromptTokens
	if window.earliestStart.IsZero() || sample.StartedAt.Before(window.earliestStart) {
		window.earliestStart = sample.StartedAt
	}
	if window.latestToken.IsZero() || sample.FirstByteAt.After(window.latestToken) {
		window.latestToken = *sample.FirstByteAt
	}
}

func (window effectivePrefillWindow) throughput() (float64, bool) {
	span := window.latestToken.Sub(window.earliestStart)
	if window.promptTokens <= 0 || span <= 0 {
		return 0, false
	}
	return float64(window.promptTokens) / span.Seconds(), true
}

func insertMetricDistribution(tx *sql.Tx, measurementID int64, metric, unit string, stats numericStats) error {
	_, err := tx.Exec(`INSERT INTO metric_stats (
		measurement_id, metric, unit, mean, stddev, min, p50, p90, p95, p99, max, count
	) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		measurementID, metric, unit, stats.Mean, stats.StdDev, stats.Min, stats.P50,
		stats.P90, stats.P95, stats.P99, stats.Max, stats.Count)
	return err
}

func requestSamplesForResult(runDir, resultFile string) ([]RequestSample, error) {
	path := resolveResultPath(runDir, resultFile)
	if strings.TrimSpace(path) == "" {
		return nil, nil
	}
	data, err := readTrimmedFile(path)
	if err != nil {
		return nil, err
	}
	return requestSamplesFromResultData(data)
}

func insertRequestSamples(tx *sql.Tx, measurementID int64, samples []RequestSample) error {
	for _, sample := range samples {
		completedSample := sample.Status == "completed"
		if _, err := tx.Exec(`INSERT INTO requests (
			measurement_id, request_index, request_id, status, streamed,
			http_status_code, started_at, first_byte_at, first_byte_ms,
			first_token_at, completed_at, latency_ms, ttft_ms, tpot_ms,
			itl_mean_ms, prompt_tokens, completion_tokens, total_tokens,
			output_tok_s, total_tok_s, prompt_sha256, response_sha256,
			error_type, error_code, error_message, response_metadata_json
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			measurementID, sample.RequestIndex, nullString(sample.RequestID),
			firstNonEmpty(sample.Status, "failed"), boolToInt(sample.Streamed),
			intNull(sample.HTTPStatusCode), sample.StartedAt.Format(time.RFC3339Nano),
			timePtrString(sample.FirstByteAt), floatNull(sample.FirstByteMillis),
			timePtrString(streamedFirstTokenAt(sample)), timePtrString(sample.CompletedAt), floatNull(sample.LatencyMillis),
			floatNull(sample.TTFTMillis), floatNull(sample.TPOTMillis), floatNull(sample.ITLMeanMillis),
			knownIntNull(completedSample, sample.PromptTokens), knownIntNull(completedSample, sample.CompletionTokens),
			knownIntNull(completedSample, sample.TotalTokens), knownFloatNull(completedSample, sample.OutputTokensPerSecond),
			knownFloatNull(completedSample, sample.TotalTokensPerSecond), nullString(sample.PromptSHA256),
			nullString(sample.ResponseSHA256), nullString(sample.ErrorType),
			nullString(sample.ErrorCode), nullString(sample.ErrorMessage),
			nullableJSON(sample.ResponseMetadata)); err != nil {
			return err
		}
	}
	return nil
}

func streamedFirstTokenAt(sample RequestSample) *time.Time {
	if !sample.Streamed {
		return nil
	}
	return sample.FirstByteAt
}

func insertEvents(tx *sql.Tx, runID string, events []Event, phaseIDs, measurementIDs map[string]int64) error {
	for _, event := range events {
		key := eventMeasurementKey(event)
		_, err := tx.Exec(`INSERT INTO events (
			run_id, timestamp, level, type, phase_id, profile_id, workload_id,
			measurement_id, message, data_json
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			runID, event.Timestamp.Format(time.RFC3339), eventLevel(event), event.Type,
			zeroNullInt(phaseIDs[key]), dimensionRef(runID, event.Profile), dimensionRef(runID, event.Workload),
			zeroNullInt(measurementIDs[key]), event.Error, nullableRawJSON(event.Details))
		if err != nil {
			return err
		}
	}
	return nil
}

func insertCommands(tx *sql.Tx, runID string, events []Event, phaseIDs, measurementIDs, artifactIDs map[string]int64) error {
	for _, event := range events {
		if event.Command == "" && len(event.Args) == 0 {
			continue
		}
		key := eventMeasurementKey(event)
		status := commandStatus(event)
		_, err := tx.Exec(`INSERT INTO commands (
			run_id, phase_id, measurement_id, profile_id, phase, argv_json,
			started_at, completed_at, exit_code, status, stdout_artifact_id, stderr_artifact_id
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			runID, zeroNullInt(phaseIDs[key]), zeroNullInt(measurementIDs[key]),
			dimensionRef(runID, event.Profile), event.Type, mustJSONString(event.Args),
			commandStartedAt(event, status), commandCompletedAt(event, status),
			commandExitCode(event, status), status, zeroNullInt(artifactIDForPath(artifactIDs, event.LogFile)), nil)
		if err != nil {
			return err
		}
	}
	return nil
}

func commandStatus(event Event) string {
	if event.Type == "planned_run" {
		return "planned"
	}
	if strings.HasSuffix(event.Type, "_start") {
		return "running"
	}
	if event.Error != "" {
		return "failed"
	}
	return "completed"
}

func commandStartedAt(event Event, status string) any {
	if status == "planned" {
		return nil
	}
	return event.Timestamp.Format(time.RFC3339)
}

func commandCompletedAt(event Event, status string) any {
	if status == "completed" || status == "failed" || status == "canceled" {
		return event.Timestamp.Format(time.RFC3339)
	}
	return nil
}

func commandExitCode(event Event, status string) any {
	if status == "completed" || status == "failed" || status == "canceled" {
		return event.ExitCode
	}
	return nil
}

func insertTelemetry(tx *sql.Tx, runID string, events []Event, phaseIDs, measurementIDs map[string]int64) error {
	seriesID, err := ensureTelemetrySeries(tx, runID, "proc_meminfo", "mem_available_bytes", "bytes", "run", "{}")
	if err != nil {
		return err
	}
	for _, event := range events {
		if err := insertGPUTelemetryEvent(tx, runID, event, phaseIDs, measurementIDs); err != nil {
			return err
		}
		if event.MemAvailableGiB == 0 {
			continue
		}
		key := eventMeasurementKey(event)
		_, err := tx.Exec(`INSERT INTO telemetry_samples (
			series_id, timestamp, value, phase_id, measurement_id
		) VALUES (?, ?, ?, ?, ?)`,
			seriesID, event.Timestamp.Format(time.RFC3339), event.MemAvailableGiB*1024*1024*1024,
			zeroNullInt(phaseIDs[key]), zeroNullInt(measurementIDs[key]))
		if err != nil {
			return err
		}
	}
	return nil
}

func insertGPUTelemetryEvent(tx *sql.Tx, runID string, event Event, phaseIDs, measurementIDs map[string]int64) error {
	sample, ok := parseGPUTelemetryEvent(event)
	if !ok {
		return nil
	}
	key := eventMeasurementKey(event)
	metrics := []struct {
		name  string
		unit  string
		value *float64
	}{
		{"gpu_utilization_percent", "percent", sample.GPUUtilizationPct},
		{"gpu_memory_used_bytes", "bytes", sample.GPUMemoryUsedBytes},
	}
	for _, metric := range metrics {
		if metric.value == nil {
			continue
		}
		seriesID, err := ensureTelemetrySeries(tx, runID, sample.Source, metric.name, metric.unit, "run", "{}")
		if err != nil {
			return err
		}
		if _, err := tx.Exec(`INSERT INTO telemetry_samples (
			series_id, timestamp, value, phase_id, measurement_id
		) VALUES (?, ?, ?, ?, ?)`,
			seriesID, event.Timestamp.Format(time.RFC3339), *metric.value,
			zeroNullInt(phaseIDs[key]), zeroNullInt(measurementIDs[key])); err != nil {
			return err
		}
	}
	return nil
}

func parseGPUTelemetryEvent(event Event) (gpuTelemetrySample, bool) {
	if event.Type != "gpu_telemetry" || len(event.Details) == 0 {
		return gpuTelemetrySample{}, false
	}
	var sample gpuTelemetrySample
	if err := json.Unmarshal(event.Details, &sample); err != nil || sample.Source == "" {
		return gpuTelemetrySample{}, false
	}
	return sample, true
}

// applyEngineIdentityEvents fills engines.version and stores the server's
// self-reported identity under metadata_json.identity, so the artifact
// records what the engine said about itself, not only what the spec
// declared.
func applyEngineIdentityEvents(tx *sql.Tx, runID string, spec Spec, events []Event) error {
	engineByProfile := map[string]string{}
	for _, profile := range spec.Profiles {
		engineByProfile[profile.Name] = profile.Engine
	}
	for _, event := range events {
		if err := applyEngineIdentityEvent(tx, runID, engineByProfile, event); err != nil {
			return err
		}
	}
	return nil
}

func applyEngineIdentityEvent(tx *sql.Tx, runID string, engineByProfile map[string]string, event Event) error {
	if event.Type != "engine_identity" || len(event.Details) == 0 {
		return nil
	}
	engineName := engineByProfile[event.Profile]
	if engineName == "" {
		return nil
	}
	var identity engineIdentity
	if err := json.Unmarshal(event.Details, &identity); err != nil {
		return nil
	}
	// Key the identity by profile: one engine definition can serve several
	// profiles with different models, and the last probe must not overwrite
	// the others.
	_, err := tx.Exec(`UPDATE engines SET
		version = COALESCE(NULLIF(?, ''), version),
		metadata_json = json_patch(COALESCE(metadata_json, '{}'), json_object('identity', json_object(?, json(?))))
		WHERE run_id = ? AND id = ?`,
		identity.Version, event.Profile, string(event.Details), runID, dimensionID(runID, engineName))
	return err
}

func ensureTelemetrySeries(tx *sql.Tx, runID, source, metric, unit, target, tags string) (int64, error) {
	if _, err := tx.Exec(`INSERT OR IGNORE INTO telemetry_series (
		run_id, source, metric, unit, target, tags_json
	) VALUES (?, ?, ?, ?, ?, ?)`, runID, source, metric, unit, target, tags); err != nil {
		return 0, err
	}
	var id int64
	err := tx.QueryRow(`SELECT id FROM telemetry_series
		WHERE run_id = ? AND source = ? AND metric = ? AND target = ? AND tags_json = ?`,
		runID, source, metric, target, tags).Scan(&id)
	return id, err
}

func rowsByMeasurement(runDir string, events []Event) map[string]ReportRow {
	out := map[string]ReportRow{}
	for _, event := range events {
		if !eventHasImportableResult(event) {
			continue
		}
		rows, err := parseResultFile(resolveResultPath(runDir, event.ResultFile))
		if err != nil || len(rows) == 0 {
			continue
		}
		row := rows[0]
		enrichRowFromEvent(&row, event)
		out[eventMeasurementKey(event)] = row
	}
	return out
}

func importableFinishEvent(events []Event, planned PlannedRun) Event {
	for _, event := range events {
		if eventMatchesPlanned(event, planned) && eventHasImportableResult(event) {
			return event
		}
	}
	return Event{}
}

func artifactFinishEvent(events []Event, planned PlannedRun) Event {
	for _, event := range events {
		if eventMatchesPlanned(event, planned) && event.Type == "workload_finish" && eventHasArtifactResult(event) {
			return event
		}
	}
	return Event{}
}

func eventHasImportableResult(event Event) bool {
	// workload_resumed marks a completed result adopted from a previous
	// attempt whose clean finish event may have been lost to a crash.
	if event.Type == "workload_resumed" && event.ResultFile != "" {
		return true
	}
	return event.Type == "workload_finish" && event.ResultFile != "" && (event.Error == "" || eventDetailBool(event, "result_written"))
}

func eventHasArtifactResult(event Event) bool {
	return event.ResultFile != "" && (event.Type == "workload_finish" || event.Type == "warmup_finish" || event.Type == "workload_resumed")
}

func eventDetailBool(event Event, key string) bool {
	if len(event.Details) == 0 {
		return false
	}
	var details map[string]bool
	if err := json.Unmarshal(event.Details, &details); err != nil {
		return false
	}
	return details[key]
}

// measurementStatus takes the last decisive event: resumed runs append a
// fresh attempt's events after a failed one, and the retry outcome must win
// over the stale failure.
func measurementStatus(events []Event, planned PlannedRun) string {
	status := "planned"
	for _, event := range events {
		if !eventMatchesPlanned(event, planned) {
			continue
		}
		if next := eventMeasurementStatus(event); next != "" {
			status = next
		}
	}
	return status
}

// eventMeasurementStatus maps one event to the measurement status it
// implies, or "" for non-decisive events. workload_resumed adopts a
// completed result from a previous attempt.
func eventMeasurementStatus(event Event) string {
	switch {
	case event.Type == "workload_skipped":
		return "skipped"
	case event.Type == "workload_resumed":
		return "completed"
	case event.Type == "workload_failed" || event.Error != "":
		return "failed"
	case event.Type == "workload_finish":
		return "completed"
	default:
		return ""
	}
}

// measurementError keeps the last error and clears it on a later clean
// finish, matching measurementStatus's last-attempt-wins view.
func measurementError(events []Event, planned PlannedRun) any {
	var lastError any
	for _, event := range events {
		if !eventMatchesPlanned(event, planned) {
			continue
		}
		if event.Error != "" {
			lastError = event.Error
		}
		if event.Type == "workload_resumed" || (event.Type == "workload_finish" && event.Error == "") {
			lastError = nil
		}
	}
	return lastError
}

func measurementTimes(events []Event, planned PlannedRun) (*time.Time, *time.Time) {
	var start, end *time.Time
	for _, event := range events {
		if !eventMatchesPlanned(event, planned) {
			continue
		}
		if event.Type == "workload_start" {
			t := event.Timestamp
			start = &t
		}
		if event.Type == "workload_finish" || event.Type == "workload_failed" || event.Type == "workload_skipped" || event.Type == "workload_resumed" {
			t := event.Timestamp
			end = &t
		}
	}
	return start, end
}

func eventMatchesPlanned(event Event, planned PlannedRun) bool {
	return event.Profile == planned.Profile.Name &&
		event.Workload == planned.Workload.Name &&
		event.Concurrency == planned.Concurrency &&
		event.Repeat == planned.Repeat
}

func eventMeasurementKey(event Event) string {
	if event.Profile == "" || event.Workload == "" || event.Concurrency == 0 {
		return ""
	}
	return measurementKey(event.Profile, event.Workload, event.Concurrency, event.Repeat)
}

func measurementKey(profile, workload string, concurrency, repeat int) string {
	return fmt.Sprintf("%s\x00%s\x00%d\x00%d", profile, workload, concurrency, repeat)
}

func artifactIDForPath(ids map[string]int64, path string) int64 {
	if path == "" {
		return 0
	}
	if id := ids[path]; id != 0 {
		return id
	}
	if id := ids[filepath.Clean(path)]; id != 0 {
		return id
	}
	if rel := strings.TrimPrefix(filepath.Clean(path), "."+string(filepath.Separator)); rel != path {
		return ids[rel]
	}
	return 0
}

func sha256Hex(data []byte) string {
	return artifact.SHA256Hex(data)
}

func sha256Maybe(value string) string {
	if strings.TrimSpace(value) == "" {
		return ""
	}
	return sha256Hex([]byte(value))
}

func nullableJSON(value any) any {
	data, err := json.Marshal(value)
	if err != nil || string(data) == "null" || string(data) == "{}" {
		return nil
	}
	return string(data)
}

func nullableRawJSON(value json.RawMessage) any {
	if len(value) == 0 {
		return nil
	}
	return string(value)
}

func mustJSONString(value any) string {
	data, err := json.Marshal(value)
	if err != nil {
		return "{}"
	}
	return string(data)
}

func timeOrNull(value time.Time) any {
	if value.IsZero() {
		return nil
	}
	return value.Format(time.RFC3339)
}

func timePtrString(value *time.Time) any {
	if value == nil || value.IsZero() {
		return nil
	}
	return value.UTC().Format(time.RFC3339Nano)
}

func durationMillis(start, end *time.Time) any {
	if start == nil || end == nil {
		return nil
	}
	return end.Sub(*start).Seconds() * 1000
}

func boolToInt(value bool) int {
	if value {
		return 1
	}
	return 0
}

func nullString(value string) any {
	return artifact.NullString(value)
}

func emptyNull(value string) any {
	return nullString(value)
}

func intNull(value int) any {
	if value == 0 {
		return nil
	}
	return value
}

func knownIntNull(known bool, value int) any {
	return knownNull(known, value, intNull(value))
}

func intPointerNull(value *int) any {
	if value == nil {
		return nil
	}
	return *value
}

func zeroNullInt(value int64) any {
	if value == 0 {
		return nil
	}
	return value
}

func floatNull(value float64) any {
	if value == 0 {
		return nil
	}
	return value
}

func knownFloatNull(known bool, value float64) any {
	return knownNull(known, value, floatNull(value))
}

func knownNull(known bool, value, fallback any) any {
	if known {
		return value
	}
	return fallback
}

func eventLevel(event Event) string {
	if event.Error != "" {
		return "error"
	}
	if strings.Contains(event.Type, "failed") || strings.Contains(event.Type, "floor") {
		return "warn"
	}
	return "info"
}
