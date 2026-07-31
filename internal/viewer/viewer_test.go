package viewer

import (
	"context"
	"database/sql"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/osolmaz/localperf/internal/artifact"
	"github.com/osolmaz/localperf/internal/reportmodel"
)

func TestNewHandlerServesTabbedReports(t *testing.T) {
	firstPath := testViewerArtifact(t, "gemma.sqlite", "Gemma Run")
	secondPath := testViewerArtifact(t, "qwen.sqlite", "Qwen Run")
	handler, err := NewHandler(HandlerConfig{
		Title: "Benchmark Viewer",
		Paths: []string{firstPath, secondPath},
	})
	if err != nil {
		t.Fatal(err)
	}
	manifest := handler.Manifest()
	if manifest.Title != "Benchmark Viewer" {
		t.Fatalf("manifest title = %q, want Benchmark Viewer", manifest.Title)
	}
	if len(manifest.Reports) != 2 {
		t.Fatalf("manifest reports = %d, want 2", len(manifest.Reports))
	}
	if manifest.Reports[0].ID == manifest.Reports[1].ID {
		t.Fatalf("report IDs must be unique: %q", manifest.Reports[0].ID)
	}
	if manifest.Reports[0].Label != "gemma" || manifest.Reports[0].RunCount != 1 || manifest.Reports[0].MeasurementCount != 2 {
		t.Fatalf("first summary = %+v, want gemma with one run and two measurements", manifest.Reports[0])
	}

	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)

	indexHTML := getString(t, server.URL+"/")
	for _, want := range []string{
		"<!doctype html>",
		`<div id="root"></div>`,
		"/assets/",
	} {
		if !strings.Contains(indexHTML, want) {
			t.Fatalf("index missing %q:\n%s", want, indexHTML)
		}
	}
	stylesheetMatch := regexp.MustCompile(`href="([^"]+\.css)"`).FindStringSubmatch(indexHTML)
	if len(stylesheetMatch) != 2 {
		t.Fatalf("index stylesheet link not found:\n%s", indexHTML)
	}
	stylesheet := getString(t, server.URL+stylesheetMatch[1])
	for _, want := range []string{".throughput-table", "min-width:1040px", "max-width:100%", "overflow-x:auto"} {
		if !strings.Contains(stylesheet, want) {
			t.Fatalf("viewer stylesheet missing mobile table contract %q", want)
		}
	}

	reportHTML := getString(t, server.URL+"/report/"+manifest.Reports[1].ID)
	for _, want := range []string{"<!doctype html>", "Qwen Run", "Throughput"} {
		if !strings.Contains(reportHTML, want) {
			t.Fatalf("report missing %q:\n%s", want, reportHTML)
		}
	}
	response, err := http.Get(server.URL + "/report/missing")
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusNotFound {
		t.Fatalf("missing report status = %d, want 404", response.StatusCode)
	}
}

func TestHandlerServesJSONAPIs(t *testing.T) {
	path := testViewerArtifact(t, "run.sqlite", "JSON Run")
	handler, err := NewHandler(HandlerConfig{Paths: []string{path}})
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)

	response, err := http.Get(server.URL + "/api/reports")
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if contentType := response.Header.Get("Content-Type"); !strings.Contains(contentType, "application/json") {
		t.Fatalf("content type = %q, want JSON", contentType)
	}
	var manifest Manifest
	if err := json.NewDecoder(response.Body).Decode(&manifest); err != nil {
		t.Fatal(err)
	}
	if len(manifest.Reports) != 1 || manifest.Reports[0].LatestRunName != "JSON Run" {
		t.Fatalf("manifest = %+v, want JSON Run", manifest)
	}
	reportID := manifest.Reports[0].ID

	var summary reportmodel.Summary
	getJSON(t, server.URL+"/api/reports/"+reportID+"/summary", &summary)
	if summary.MeasurementCount != 2 || summary.LatestRun.Name != "JSON Run" {
		t.Fatalf("summary = %+v, want two measurements for JSON Run", summary)
	}

	var throughput reportmodel.ThroughputResponse
	getJSON(t, server.URL+"/api/reports/"+reportID+"/throughput", &throughput)
	if len(throughput.Tables) != 1 {
		t.Fatalf("throughput tables = %d, want 1", len(throughput.Tables))
	}
	table := throughput.Tables[0]
	if len(table.Rows) != 1 {
		t.Fatalf("throughput rows = %d, want 1", len(table.Rows))
	}
	row := table.Rows[0]
	if row.Concurrency != 4 || !row.Decode.Available || !row.Prefill.Available {
		t.Fatalf("combined row = %+v, want decode and prefill at c4", row)
	}
	if row.Result != "D 4/0; P 4/0" {
		t.Fatalf("result = %q, want phase-specific OK/Err", row.Result)
	}
	if row.Decode.MeasurementID == row.Prefill.MeasurementID {
		t.Fatalf("decode and prefill measurement IDs should differ: %+v", row)
	}

	var detail reportmodel.CellDetail
	getJSON(t, server.URL+"/api/reports/"+reportID+"/measurements/"+strconv.FormatInt(row.Decode.MeasurementID, 10), &detail)
	if !detail.Available || detail.Mode != "decode" || detail.Profile != "8k" {
		t.Fatalf("detail = %+v, want decode detail for 8k profile", detail)
	}

	response, err = http.Get(server.URL + "/api/reports/" + reportID + "/measurements/999999")
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusNotFound {
		t.Fatalf("missing measurement status = %d, want 404", response.StatusCode)
	}
}

func TestNewHandlerRequiresAtLeastOnePath(t *testing.T) {
	if _, err := NewHandler(HandlerConfig{}); err == nil {
		t.Fatal("NewHandler error = nil, want missing path error")
	}
}

func TestServeStopsWhenContextCanceled(t *testing.T) {
	path := testViewerArtifact(t, "run.sqlite", "Serve Run")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := Serve(ctx, ServerConfig{Paths: []string{path}}); err != nil {
		t.Fatal(err)
	}
}

func TestDisplayURLUsesLocalhostForUnspecifiedBind(t *testing.T) {
	got := displayURL(&net.TCPAddr{IP: net.IPv4zero, Port: 8766})
	if got != "http://127.0.0.1:8766" {
		t.Fatalf("displayURL = %q, want localhost URL", got)
	}
}

func getString(t *testing.T, url string) string {
	t.Helper()
	response, err := http.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("GET %s status = %d, want 200", url, response.StatusCode)
	}
	body, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}
	return string(body)
}

func getJSON(t *testing.T, url string, out any) {
	t.Helper()
	response, err := http.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("GET %s status = %d, want 200", url, response.StatusCode)
	}
	if contentType := response.Header.Get("Content-Type"); !strings.Contains(contentType, "application/json") {
		t.Fatalf("content type = %q, want JSON", contentType)
	}
	if err := json.NewDecoder(response.Body).Decode(out); err != nil {
		t.Fatal(err)
	}
}

func testViewerArtifact(t *testing.T, filename, name string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), filename)
	db, err := artifact.Create(path, artifact.Schema)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	createViewerArtifactRows(t, db, name)
	return path
}

func createViewerArtifactRows(t *testing.T, db *sql.DB, name string) {
	t.Helper()
	runID := "run-1"
	createdAt := "2026-01-01T00:00:00Z"
	if _, err := db.Exec(`INSERT INTO metadata (key, value) VALUES
		('format_name', ?), ('format_version', ?)`, artifact.FormatName, artifact.FormatVersion); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO run (
		id, name, status, created_at, started_at, completed_at, localperf_version,
		hostname, username, cwd, command_line_json, host_json, labels_json
	) VALUES (?, ?, 'completed', ?, ?, ?, 'test', 'localhost', 'tester', '/tmp', '[]', '{}', '{}')`,
		runID, name, createdAt, createdAt, createdAt); err != nil {
		t.Fatal(err)
	}
	insertViewerSpecs(t, db, runID, createdAt)
	if _, err := db.Exec(`INSERT INTO engines (
		id, run_id, name, type, managed, command, version, env_json, metadata_json
	) VALUES ('engine-1', ?, 'vllm', 'vllm', 1, 'vllm', 'test', '{}', '{}')`, runID); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO profiles (
		id, run_id, engine_id, name, model, host, port, endpoint_base_url, managed,
		context_window, max_num_seqs, max_num_batched_tokens, gpu_memory_utilization,
		enable_sleep_mode, sleep_level, serve_json, engine_args_json, env_json, metadata_json
	) VALUES (
		'profile-1', ?, 'engine-1', '8k', 'test/model',
		'127.0.0.1', 8108, 'http://127.0.0.1:8108', 1, 8192, 16, 8192, 0.35,
		1, 2, '{}', '{}', '{}', '{}'
	)`, runID); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO workloads (
		id, run_id, name, role, phase, traffic_json, concurrency_json, samples, repeats,
		save_detailed, capture_payload_artifacts, dataset_json, request_json, metadata_json
	) VALUES (
		'decode-workload', ?, 'decode-8k', 'benchmark', 'decode',
		'{"backend":"openai-chat","dataset_name":"random","random_input_len":8192,"random_output_len":512,"request_rate":"inf"}',
		'[4]', 8, 1, 1, 0, '{}', '{}', '{"context":{"target":8192,"semantics":"capacity"}}'
	)`, runID); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO workloads (
		id, run_id, name, role, phase, traffic_json, concurrency_json, samples, repeats,
		save_detailed, capture_payload_artifacts, dataset_json, request_json, metadata_json
	) VALUES (
		'prefill-workload', ?, 'prefill-8k', 'benchmark', 'prefill',
		'{"backend":"openai-chat","dataset_name":"random","random_input_len":8192,"random_output_len":16,"request_rate":"inf"}',
		'[4]', 8, 1, 1, 0, '{}', '{}', '{"context":{"target":8192,"semantics":"capacity"}}'
	)`, runID); err != nil {
		t.Fatal(err)
	}
	decodePhaseID := insertViewerPhase(t, db, runID, "decode-workload", createdAt)
	decodeMeasurementID := insertViewerMeasurement(t, db, runID, "decode-workload", decodePhaseID, createdAt, 2048, 256.0, 64.0)
	insertViewerMetric(t, db, decodeMeasurementID)
	prefillPhaseID := insertViewerPhase(t, db, runID, "prefill-workload", createdAt)
	prefillMeasurementID := insertViewerMeasurement(t, db, runID, "prefill-workload", prefillPhaseID, createdAt, 64, 32.0, 8.0)
	insertViewerMetric(t, db, prefillMeasurementID)
}

func insertViewerSpecs(t *testing.T, db *sql.DB, runID, createdAt string) {
	t.Helper()
	for _, spec := range []struct {
		kind    string
		content string
	}{
		{kind: "original", content: `{"version":"1","name":"original"}`},
		{kind: "normalized", content: `{"version":"1","name":"normalized"}`},
	} {
		if _, err := db.Exec(`INSERT INTO specs (
			run_id, kind, format, content, sha256, created_at
		) VALUES (?, ?, 'json', ?, ?, ?)`, runID, spec.kind, spec.content, artifact.SHA256Hex([]byte(spec.content)), createdAt); err != nil {
			t.Fatal(err)
		}
	}
}

func insertViewerPhase(t *testing.T, db *sql.DB, runID, workloadID, createdAt string) int64 {
	t.Helper()
	result, err := db.Exec(`INSERT INTO phases (
		run_id, profile_id, workload_id, name, type, status, started_at, completed_at, metadata_json
	) VALUES (?, 'profile-1', ?, 'measurement', 'measurement', 'completed', ?, ?, '{}')`,
		runID, workloadID, createdAt, createdAt)
	if err != nil {
		t.Fatal(err)
	}
	id, err := result.LastInsertId()
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func insertViewerMeasurement(t *testing.T, db *sql.DB, runID, workloadID string, phaseID int64, createdAt string, completionTokens int, outputTokS, perUserTokS float64) int64 {
	t.Helper()
	result, err := db.Exec(`INSERT INTO measurements (
		run_id, profile_id, workload_id, phase_id, repeat_index, concurrency, samples_requested,
		status, started_at, completed_at, wall_time_ms, completed_requests, failed_requests,
		prompt_tokens, completion_tokens, total_tokens, aggregate_output_tok_s,
		per_user_output_tok_s, aggregate_total_tok_s, metadata_json
	) VALUES (
		?, 'profile-1', ?, ?, 0, 4, 8, 'completed', ?, ?, 1000,
		4, 0, 32768, ?, 32768 + ?, ?, ?, 512.0, '{}'
)`, runID, workloadID, phaseID, createdAt, createdAt, completionTokens, completionTokens, outputTokS, perUserTokS)
	if err != nil {
		t.Fatal(err)
	}
	id, err := result.LastInsertId()
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func insertViewerMetric(t *testing.T, db *sql.DB, measurementID int64) {
	t.Helper()
	if _, err := db.Exec(`INSERT INTO metric_stats (
		measurement_id, metric, unit, mean, stddev, min, p50, p90, p95, p99, max, count
	) VALUES
		(?, 'request_output_throughput', 'tok/s', 64, 4, 58, 64, 68, 70, 72, 74, 4),
		(?, 'request_ttft', 'ms', 500, 40, 440, 500, 540, 560, 580, 600, 4)`,
		measurementID, measurementID); err != nil {
		t.Fatal(err)
	}
}
