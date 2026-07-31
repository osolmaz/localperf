package report

import (
	"database/sql"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/osolmaz/localperf/internal/artifact"
)

func TestRenderSQLiteHTMLReportEscapesAndIsStandalone(t *testing.T) {
	artifactPath := testSQLiteHTMLArtifact(t, "Escaping <script>alert(1)</script>")
	doc, err := LoadSQLiteReport(artifactPath)
	if err != nil {
		t.Fatal(err)
	}
	var out strings.Builder
	if err := RenderHTMLReport(&out, doc, HTMLReportOptions{}); err != nil {
		t.Fatal(err)
	}
	html := out.String()
	for _, want := range []string{
		"<!doctype html>",
		"<style>",
		"Escaping &lt;script&gt;alert(1)&lt;/script&gt;",
		"Throughput",
		"throughput-group",
		"Decode tok/s",
		"Decode/user",
		"Prefill tok/s",
		"Prefill/user",
		"decode tok/s",
		"prefill tok/s",
		"Decode TTFT avg",
		"Decode TTFT p99",
		"Prefill TTFT avg",
		"Prefill TTFT p99",
		"OK / Err",
		"table-layout:fixed",
		"@media print",
		"heat-",
		"phone-table",
		"Privacy",
	} {
		if !strings.Contains(html, want) {
			t.Fatalf("HTML report missing %q:\n%s", want, html)
		}
	}
	if strings.Contains(html, "min-width:900px") {
		t.Fatalf("HTML report still has fixed minimum table width:\n%s", html)
	}
	if strings.Contains(html, "Input tok/s") || strings.Contains(html, ">In/s<") {
		t.Fatalf("HTML report includes derived input throughput in headline table:\n%s", html)
	}
	if strings.Contains(html, "<th>Mode</th>") {
		t.Fatalf("HTML report still splits decode/prefill into mode rows:\n%s", html)
	}
	for _, forbidden := range []string{"Decode lat", "Prefill lat", "<th class=\"num\">OK</th><th class=\"num\">Err</th>", "Full-run timing", "E2E output", "Output share/user", "Generation output tok/s", "steady decode"} {
		if strings.Contains(html, forbidden) {
			t.Fatalf("HTML report contains removed headline column %q:\n%s", forbidden, html)
		}
	}
	for _, forbidden := range []string{"<script>alert(1)</script>", "https://", "http://", "<link ", "src="} {
		if strings.Contains(html, forbidden) {
			t.Fatalf("HTML report contains forbidden %q:\n%s", forbidden, html)
		}
	}
}

func TestRenderSQLiteHTMLReportShowsFailedCellsAndProvenance(t *testing.T) {
	artifactPath := testSQLiteHTMLArtifact(t, "Failed Cell")
	db, err := sql.Open("sqlite", artifactPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO workloads (
		id, run_id, name, role, phase, traffic_json, concurrency_json, samples, repeats,
		save_detailed, capture_payload_artifacts, metadata_json
	) VALUES (
		'workload-failed', 'run-1', 'decode-8k', 'benchmark', 'decode',
		'{"dataset_name":"random","random_input_len":8192,"random_output_len":1024}',
		'[16]', 32, 1, 1, 0,
		'{"context":{"target":8192,"semantics":"active"}}'
	)`); err != nil {
		_ = db.Close()
		t.Fatal(err)
	}
	result, err := db.Exec(`INSERT INTO measurements (
		run_id, profile_id, workload_id, repeat_index, concurrency, samples_requested,
		status, completed_requests, failed_requests, error_type, error_message
	) VALUES (
		'run-1', 'profile-1', 'workload-failed', 0, 16, 32,
		'skipped', 0, 0, 'memory_floor',
		'MemAvailable 34.2 GiB is below memory floor 40.0 GiB'
	)`)
	if err != nil {
		_ = db.Close()
		t.Fatal(err)
	}
	measurementID, err := result.LastInsertId()
	if err != nil {
		_ = db.Close()
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO commands (
		run_id, profile_id, measurement_id, phase, argv_json, status
	) VALUES
		('run-1', 'profile-1', NULL, 'server_start',
		 '["vllm","serve","nvidia/diffusiongemma-26B-A4B-it-NVFP4","--max-model-len","8192","--max-num-seqs","16"]',
		 'completed'),
		('run-1', 'profile-1', ?, 'workload_start',
		 '["localperf","bench","run","--workload","decode-8k","--concurrency","16"]',
		 'failed')`, measurementID); err != nil {
		_ = db.Close()
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	doc, err := LoadSQLiteReport(artifactPath)
	if err != nil {
		t.Fatal(err)
	}
	var out strings.Builder
	if err := RenderHTMLReport(&out, doc, HTMLReportOptions{}); err != nil {
		t.Fatal(err)
	}
	html := out.String()
	for _, want := range []string{
		"mem floor",
		"cell-detail",
		"cell-popover",
		"MemAvailable 34.2 GiB is below memory floor 40.0 GiB",
		"vllm serve nvidia/diffusiongemma-26B-A4B-it-NVFP4 --max-model-len 8192 --max-num-seqs 16",
		"localperf bench run --workload decode-8k --concurrency 16",
		"Max seqs",
		"Batched tokens",
		"unverified (declared 8k active)",
	} {
		if !strings.Contains(html, want) {
			t.Fatalf("HTML report missing %q:\n%s", want, html)
		}
	}
}

func TestRenderSQLiteHTMLReportDoesNotLabelPlannedRowsAsFailures(t *testing.T) {
	artifactPath := testSQLiteHTMLArtifact(t, "Dry Run")
	db, err := sql.Open("sqlite", artifactPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO workloads (
		id, run_id, name, role, phase, traffic_json, concurrency_json, samples, repeats,
		save_detailed, capture_payload_artifacts, metadata_json
	) VALUES (
		'workload-planned', 'run-1', 'decode-dry-run', 'benchmark', 'decode',
		'{"dataset_name":"random","random_input_len":1024,"random_output_len":256}',
		'[1]', 8, 1, 1, 0,
		'{"context":{"target":4096,"semantics":"capacity"}}'
	)`); err != nil {
		_ = db.Close()
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO measurements (
		run_id, profile_id, workload_id, repeat_index, concurrency, samples_requested,
		status, completed_requests, failed_requests
	) VALUES (
		'run-1', 'profile-1', 'workload-planned', 0, 1, 8,
		'planned', 0, 0
	)`); err != nil {
		_ = db.Close()
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	doc, err := LoadSQLiteReport(artifactPath)
	if err != nil {
		t.Fatal(err)
	}
	var out strings.Builder
	if err := RenderHTMLReport(&out, doc, HTMLReportOptions{}); err != nil {
		t.Fatal(err)
	}
	html := out.String()
	for _, forbidden := range []string{"D planned", "P planned", "<summary>planned</summary>"} {
		if strings.Contains(html, forbidden) {
			t.Fatalf("HTML report labels planned measurement as failed via %q:\n%s", forbidden, html)
		}
	}
	if !strings.Contains(html, `status-neutral">planned</span>`) {
		t.Fatalf("HTML report should still expose planned status in details:\n%s", html)
	}
}

func TestContextLabelsFollowContract(t *testing.T) {
	artifactPath := filepath.Join(t.TempDir(), "run.sqlite")
	createTestSQLiteHTMLArtifact(t, artifactPath, "Context Labels")
	db, err := sql.Open("sqlite", artifactPath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	// Verified active: 2 requests, 8000 prompt + 100 completion per request
	// lands inside [0.90, 1.00] x 8192.
	insertContextWorkloadMeasurement(t, db, "wl-active", "decode",
		`{"context":{"target":8192,"semantics":"active"}}`, 16000, 200)
	// Capacity point: shape unconstrained, labeled as capacity.
	insertContextWorkloadMeasurement(t, db, "wl-capacity", "decode",
		`{"context":{"target":8192,"semantics":"capacity"}}`, 2000, 1024)
	// The old Gemma conflation: declared 32k active, actually ~1k -> ~5k.
	insertContextWorkloadMeasurement(t, db, "wl-mismatch", "decode",
		`{"context":{"target":32768,"semantics":"active"}}`, 2074, 8192)
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	doc, err := LoadSQLiteReport(artifactPath)
	if err != nil {
		t.Fatal(err)
	}
	var out strings.Builder
	if err := RenderHTMLReport(&out, doc, HTMLReportOptions{}); err != nil {
		t.Fatal(err)
	}
	html := out.String()
	for _, want := range []string{
		"8k active context",
		"8k capacity",
		"1037 in / 4096 out",
		"declared 32k active, measured ~1k -&gt; ~5k active",
		"Server limit",
		"Active contexts",
		"Server limits",
		"mismatch",
	} {
		if !strings.Contains(html, want) {
			t.Fatalf("HTML report missing %q", want)
		}
	}
	if strings.Contains(html, "32k active context") {
		t.Fatal("HTML report labels a contradicted claim as 32k active context")
	}
	if strings.Contains(html, "32k context") {
		t.Fatal("HTML report labels a row by server capacity")
	}
}

func insertContextWorkloadMeasurement(t *testing.T, db *sql.DB, workloadID, phase, claims string, promptTokens, completionTokens int) {
	t.Helper()
	if _, err := db.Exec(`INSERT INTO workloads (
		id, run_id, name, role, phase, traffic_json, concurrency_json, samples, repeats,
		save_detailed, capture_payload_artifacts, metadata_json
	) VALUES (?, 'run-1', ?, 'benchmark', ?, '{"dataset_name":"random"}', '[1]', 8, 1, 1, 0, ?)`,
		workloadID, workloadID, phase, claims); err != nil {
		t.Fatal(err)
	}
	result, err := db.Exec(`INSERT INTO measurements (
		run_id, profile_id, workload_id, repeat_index, concurrency, samples_requested,
		status, started_at, completed_at, wall_time_ms, completed_requests, failed_requests,
		prompt_tokens, completion_tokens, total_tokens, aggregate_output_tok_s,
		per_user_output_tok_s, aggregate_total_tok_s
	) VALUES (
		'run-1', 'profile-1', ?, 0, 1, 8, 'completed',
		'2026-01-01T00:00:00Z', '2026-01-01T00:01:00Z', 60000, 2, 0, ?, ?, ?, 10, 10, 12
	)`, workloadID, promptTokens, completionTokens, promptTokens+completionTokens)
	if err != nil {
		t.Fatal(err)
	}
	measurementID, err := result.LastInsertId()
	if err != nil {
		t.Fatal(err)
	}
	seedSQLiteHTMLMetrics(t, db, measurementID)
}

func TestTokenThroughputMetricDisplay(t *testing.T) {
	for _, tc := range []struct {
		value string
		want  string
	}{
		{value: "522.119", want: "522"},
		{value: "100.000", want: "100"},
		{value: "99.950", want: "100"},
		{value: "99.940", want: "99.9"},
		{value: "27.765", want: "27.8"},
		{value: "4.248", want: "4.25"},
		{value: "9.996", want: "10.0"},
		{value: "10.02", want: "10.0"},
		{value: "-", want: "-"},
		{value: "35.986 ± 1.2", want: "36.0 ± 1.20"},
	} {
		if got := FormatRateDisplay(tc.value); got != tc.want {
			t.Fatalf("FormatRateDisplay(%q) = %q, want %q", tc.value, got, tc.want)
		}
	}
}

func TestDisplayOptionalPositiveFloat(t *testing.T) {
	if got := displayOptionalPositiveFloat(0); got != "-" {
		t.Fatalf("missing utilization = %q, want unavailable", got)
	}
	if got := displayOptionalPositiveFloat(0.35); got != "0.350" {
		t.Fatalf("configured utilization = %q, want 0.350", got)
	}
}

func TestInferQuantizationUsesServerReportedGGUFFType(t *testing.T) {
	metadata := `{"identity":{"64k":{"models":{"data":[{"id":"model","meta":{"ftype":"Q4_K - Medium"}}]}}}}`
	quantizations := servedQuantizationsByProfile(metadata)
	got := inferQuantization(nil, []SQLiteReportEngine{{QuantizationsByProfile: quantizations}})
	if got != "Q4_K - Medium" {
		t.Fatalf("quantization = %q, want server-reported GGUF ftype", got)
	}
}

func TestGenerationPrefillFallbackSurvivesFailedDedicatedPoint(t *testing.T) {
	for _, dedicatedFirst := range []bool{false, true} {
		row := emptyThroughputComparisonRow(6)
		decode := SQLiteReportThroughputRow{
			Mode:                        "decode",
			Status:                      "completed",
			EffectivePrefillTokS:        "1600",
			EffectivePrefillPerUserTokS: "266.667",
			TTFTMeanMS:                  "100",
			TTFTP99MS:                   "200",
			CompletedRequests:           18,
			Detail:                      SQLiteReportCellDetail{Available: true},
		}
		failedPrefill := SQLiteReportThroughputRow{
			Mode:           "prefill",
			ThroughputTokS: "failed",
			FailureLabel:   "failed",
			Detail:         SQLiteReportCellDetail{Available: true, FailureLabel: "failed"},
		}
		if dedicatedFirst {
			applyThroughputComparisonSource(&row, failedPrefill)
			applyThroughputComparisonSource(&row, decode)
		} else {
			applyThroughputComparisonSource(&row, decode)
			applyThroughputComparisonSource(&row, failedPrefill)
		}
		if !row.PrefillDerived || row.PrefillTokS != "1600" || row.Requests != "18 / 0" {
			t.Fatalf("dedicatedFirst=%v fallback = %+v", dedicatedFirst, row)
		}
		if row.PrefillDetail.Mode != "prefill" || !strings.Contains(row.PrefillDetail.Source, "streamed TTFT") || len(row.PrefillDetail.Metrics) < 2 {
			t.Fatalf("dedicatedFirst=%v derived detail = %+v", dedicatedFirst, row.PrefillDetail)
		}
	}
}

func TestFailedGenerationDoesNotDerivePrefill(t *testing.T) {
	row := emptyThroughputComparisonRow(6)
	applyThroughputComparisonSource(&row, SQLiteReportThroughputRow{
		Mode:                 "decode",
		Status:               "failed",
		FailureLabel:         "failed",
		EffectivePrefillTokS: "1600",
		CompletedRequests:    5,
		FailedRequests:       1,
		Detail:               SQLiteReportCellDetail{Available: true, FailureLabel: "failed"},
	})
	if row.PrefillDerived || row.PrefillTokS != "-" || !strings.Contains(row.Result, "failed") {
		t.Fatalf("failed generation fallback = %+v", row)
	}
}

func TestCompactMillisecondsDisplay(t *testing.T) {
	for _, tc := range []struct {
		value string
		want  string
	}{
		{value: "805.907", want: "806ms"},
		{value: "3766.128", want: "3.8s"},
		{value: "22519.610", want: "23s"},
		{value: "41645.481", want: "42s"},
		{value: "59999.900", want: "1m00s"},
		{value: "352063.414", want: "5m52s"},
		{value: "-", want: "-"},
	} {
		if got := compactMilliseconds(tc.value); got != tc.want {
			t.Fatalf("compactMilliseconds(%q) = %q, want %q", tc.value, got, tc.want)
		}
	}
}

func TestThroughputMode(t *testing.T) {
	for _, tc := range []struct {
		phase string
		want  string
	}{
		{phase: "decode", want: "decode"},
		{phase: "prefill", want: "prefill"},
		{phase: "", want: "mixed"},
		{phase: "custom-phase", want: "custom phase"},
	} {
		if got := throughputMode(tc.phase); got != tc.want {
			t.Fatalf("throughputMode(%q) = %q, want %q", tc.phase, got, tc.want)
		}
	}
}

func TestApplyThroughputComparisonHeatmapColumn(t *testing.T) {
	rows := []SQLiteReportThroughputComparisonRow{
		{DecodeTokS: "100"},
		{DecodeTokS: "-"},
		{DecodeTokS: "300"},
	}
	applyThroughputComparisonHeatmapColumn(rows, throughputComparisonHeatmapColumn{
		higherIsBetter: true,
		value: func(row SQLiteReportThroughputComparisonRow) (float64, bool) {
			return parseDisplayedFloat(row.DecodeTokS)
		},
		set: func(row *SQLiteReportThroughputComparisonRow, class string) {
			row.DecodeTokSHeat = class
		},
	})
	if rows[0].DecodeTokSHeat != "heat-0" || rows[1].DecodeTokSHeat != "heat-neutral" || rows[2].DecodeTokSHeat != "heat-5" {
		t.Fatalf("higher-is-better heat classes = %#v", rows)
	}

	applyThroughputComparisonHeatmapColumn(rows, throughputComparisonHeatmapColumn{
		higherIsBetter: false,
		value: func(row SQLiteReportThroughputComparisonRow) (float64, bool) {
			return parseDisplayedFloat(row.DecodeTokS)
		},
		set: func(row *SQLiteReportThroughputComparisonRow, class string) {
			row.DecodeTTFTHeat = class
		},
	})
	if rows[0].DecodeTTFTHeat != "heat-5" || rows[1].DecodeTTFTHeat != "heat-neutral" || rows[2].DecodeTTFTHeat != "heat-0" {
		t.Fatalf("lower-is-better heat classes = %#v", rows)
	}
}

func TestApplyThroughputComparisonHeatmapColumnNeutral(t *testing.T) {
	rows := []SQLiteReportThroughputComparisonRow{
		{DecodeTokS: "100"},
		{DecodeTokS: "100"},
	}
	applyThroughputComparisonHeatmapColumn(rows, throughputComparisonHeatmapColumn{
		higherIsBetter: true,
		value: func(row SQLiteReportThroughputComparisonRow) (float64, bool) {
			return parseDisplayedFloat(row.DecodeTokS)
		},
		set: func(row *SQLiteReportThroughputComparisonRow, class string) {
			row.DecodeTokSHeat = class
		},
	})
	if rows[0].DecodeTokSHeat != "heat-neutral" || rows[1].DecodeTokSHeat != "heat-neutral" {
		t.Fatalf("equal-value heat classes = %#v", rows)
	}
}

func TestThroughputComparisonResultPreservesCountsAfterPriorFailure(t *testing.T) {
	row := emptyThroughputComparisonRow(4)
	applyThroughputComparisonSource(&row, SQLiteReportThroughputRow{
		Mode:         "decode",
		FailureLabel: "mem floor",
		Detail:       SQLiteReportCellDetail{Available: true},
	})
	applyThroughputComparisonSource(&row, SQLiteReportThroughputRow{
		Mode:              "prefill",
		CompletedRequests: 2,
		Detail:            SQLiteReportCellDetail{Available: true},
	})
	if row.Result != "2 / 0 · D mem floor" {
		t.Fatalf("comparison result = %q, want counts plus prior failure", row.Result)
	}
}

func TestThroughputComparisonResultReplacesPlaceholderAfterSuccess(t *testing.T) {
	row := emptyThroughputComparisonRow(4)
	applyThroughputComparisonSource(&row, SQLiteReportThroughputRow{
		Mode:              "prefill",
		CompletedRequests: 1,
	})
	if row.Result != "1 / 0" {
		t.Fatalf("comparison result = %q, want current counts after first success", row.Result)
	}
	applyThroughputComparisonSource(&row, SQLiteReportThroughputRow{
		Mode:              "decode",
		CompletedRequests: 1,
	})
	if row.Result != "2 / 0" {
		t.Fatalf("comparison result = %q, want current counts after second success", row.Result)
	}
}

func TestCommandForProfileOnlyReturnsServerStartCommand(t *testing.T) {
	commands := []SQLiteReportCommand{
		{ProfileID: "profile-1", Phase: "planned_run", Argv: "localperf bench run"},
	}
	if got := commandForProfile(commands, "profile-1"); got != "" {
		t.Fatalf("commandForProfile() = %q, want no mislabeled serve command", got)
	}
	commands = append(commands, SQLiteReportCommand{ProfileID: "profile-1", Phase: "server_start", Argv: "vllm serve model"})
	if got := commandForProfile(commands, "profile-1"); got != "vllm serve model" {
		t.Fatalf("commandForProfile() = %q, want server_start command", got)
	}
}

func TestCommandSummaryFromJSONRedactsSecretFlags(t *testing.T) {
	got := commandSummaryFromJSON(`[
		"vllm", "serve", "model",
		"--api-key", "sk-live-secret",
		"--hf-token=hf-secret",
		"HF_TOKEN=env-secret",
		"--header", "Authorization: Bearer token-secret",
		"--max-model-len", "8192"
	]`)
	for _, forbidden := range []string{"sk-live-secret", "hf-secret", "env-secret", "token-secret"} {
		if strings.Contains(got, forbidden) {
			t.Fatalf("commandSummaryFromJSON leaked %q in %q", forbidden, got)
		}
	}
	if strings.Count(got, "<redacted>") != 4 {
		t.Fatalf("commandSummaryFromJSON redacted count in %q, want 4 redactions", got)
	}
	if !strings.Contains(got, "vllm serve model") || !strings.Contains(got, "--max-model-len 8192") {
		t.Fatalf("commandSummaryFromJSON over-redacted normal args: %q", got)
	}
}

func TestMetricFieldDisplay(t *testing.T) {
	metric := SQLiteReportMetric{
		Mean:        "10",
		MeanKnown:   true,
		StdDev:      "1",
		StdDevKnown: true,
		P50:         "50",
		P90:         "90",
		P95:         "95",
		P99:         "99",
	}
	for _, tc := range []struct {
		field string
		want  string
	}{
		{field: "StdDev", want: "1"},
		{field: "P50", want: "50"},
		{field: "P90", want: "90"},
		{field: "P95", want: "95"},
		{field: "P99", want: "99"},
		{field: "Mean", want: "10"},
		{field: "Other", want: "10"},
	} {
		got, ok := metricFieldDisplay(metric, tc.field)
		if !ok || got != tc.want {
			t.Fatalf("metricFieldDisplay(%q) = %q/%v, want %q/true", tc.field, got, ok, tc.want)
		}
	}

	for _, tc := range []struct {
		field  string
		metric SQLiteReportMetric
	}{
		{field: "StdDev", metric: SQLiteReportMetric{}},
		{field: "P95", metric: SQLiteReportMetric{P95: "-"}},
		{field: "Mean", metric: SQLiteReportMetric{}},
	} {
		if got, ok := metricFieldDisplay(tc.metric, tc.field); ok {
			t.Fatalf("metricFieldDisplay(%q) = %q/true, want false", tc.field, got)
		}
	}
}

func TestWriteSQLiteHTMLReportStoresArtifact(t *testing.T) {
	artifactPath := testSQLiteHTMLArtifact(t, "Stored HTML")
	outputPath := filepath.Join(t.TempDir(), "report.html")
	if err := WriteSQLiteHTMLReport(artifactPath, outputPath, HTMLReportOptions{Store: true}); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(outputPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "Stored HTML") {
		t.Fatalf("rendered HTML missing run title:\n%s", data)
	}
	if err := artifact.Check(artifactPath); err != nil {
		t.Fatalf("artifact check after storing HTML: %v", err)
	}
	db, err := sql.Open("sqlite", artifactPath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var reportRows, artifactRows int
	if err := db.QueryRow(`SELECT COUNT(*) FROM reports WHERE name = 'report.html' AND format = 'html'`).Scan(&reportRows); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow(`SELECT COUNT(*) FROM artifacts WHERE kind = 'normalized_report' AND name = 'report.html' AND media_type = 'text/html'`).Scan(&artifactRows); err != nil {
		t.Fatal(err)
	}
	if reportRows != 1 || artifactRows != 1 {
		t.Fatalf("stored report/artifact rows = %d/%d, want 1/1", reportRows, artifactRows)
	}
}

func TestWriteSQLiteHTMLReportRequiresFullArtifactValidation(t *testing.T) {
	artifactPath := testSQLiteHTMLArtifact(t, "Readable With Bad Raw Artifact")
	db, err := sql.Open("sqlite", artifactPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO artifacts (
		run_id, kind, name, media_type, compression, content, content_size_bytes,
		uncompressed_size_bytes, sha256, created_at
	) SELECT id, 'debug', 'bad.log', 'text/plain', 'none', CAST('bad raw bytes' AS BLOB),
		13, 13, 'wrong-hash', created_at FROM run LIMIT 1`); err != nil {
		_ = db.Close()
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	if err := artifact.Check(artifactPath); err == nil {
		t.Fatal("artifact.Check error = nil, want full validation failure")
	}
	outputPath := filepath.Join(t.TempDir(), "report.html")
	if err := WriteSQLiteHTMLReport(artifactPath, outputPath, HTMLReportOptions{}); err == nil {
		t.Fatal("render accepted an artifact with invalid evidence hash")
	}
	if _, err := os.Stat(outputPath); !os.IsNotExist(err) {
		t.Fatalf("failed render wrote output: %v", err)
	}
}

func TestWriteSQLiteHTMLReportUsesDefaultOutput(t *testing.T) {
	dir := t.TempDir()
	artifactPath := testSQLiteHTMLArtifact(t, "Default Output")
	copyPath := filepath.Join(dir, "run.sqlite")
	data, err := os.ReadFile(artifactPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(copyPath, data, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := WriteSQLiteHTMLReport(copyPath, "", HTMLReportOptions{}); err != nil {
		t.Fatal(err)
	}
	outputPath := filepath.Join(dir, "run.html")
	html, err := os.ReadFile(outputPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(html), "Default Output") {
		t.Fatalf("default HTML output missing run title:\n%s", html)
	}
}

func TestWriteSQLiteHTMLReportRejectsOverwritingSourceArtifact(t *testing.T) {
	artifactPath := testSQLiteHTMLArtifact(t, "No Overwrite")
	if err := WriteSQLiteHTMLReport(artifactPath, artifactPath, HTMLReportOptions{}); err == nil {
		t.Fatal("WriteSQLiteHTMLReport error = nil, want same-path rejection")
	}
	if err := artifact.Check(artifactPath); err != nil {
		t.Fatalf("source artifact was corrupted after rejected render: %v", err)
	}
}

func TestWriteSQLiteHTMLReportRejectsSymlinkOutputToSourceArtifact(t *testing.T) {
	artifactPath := testSQLiteHTMLArtifact(t, "No Symlink Overwrite")
	outputPath := filepath.Join(t.TempDir(), "report.html")
	if err := os.Symlink(artifactPath, outputPath); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	if err := WriteSQLiteHTMLReport(artifactPath, outputPath, HTMLReportOptions{}); err == nil {
		t.Fatal("WriteSQLiteHTMLReport error = nil, want symlink output rejection")
	}
	if err := artifact.Check(artifactPath); err != nil {
		t.Fatalf("source artifact was corrupted after rejected symlink render: %v", err)
	}
}

func TestWriteSQLiteHTMLReportRejectsHardlinkOutputToSourceArtifact(t *testing.T) {
	artifactPath := testSQLiteHTMLArtifact(t, "No Hardlink Overwrite")
	outputPath := filepath.Join(t.TempDir(), "report.html")
	if err := os.Link(artifactPath, outputPath); err != nil {
		t.Skipf("hardlink unavailable: %v", err)
	}
	if err := WriteSQLiteHTMLReport(artifactPath, outputPath, HTMLReportOptions{}); err == nil {
		t.Fatal("WriteSQLiteHTMLReport error = nil, want hardlink output rejection")
	}
	if err := artifact.Check(artifactPath); err != nil {
		t.Fatalf("source artifact was corrupted after rejected hardlink render: %v", err)
	}
}

func TestWriteSQLiteHTMLReportRejectsDefaultOutputOverSourceArtifact(t *testing.T) {
	dir := t.TempDir()
	artifactPath := testSQLiteHTMLArtifact(t, "HTML Named Artifact")
	htmlNamedArtifact := filepath.Join(dir, "run.html")
	data, err := os.ReadFile(artifactPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(htmlNamedArtifact, data, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := WriteSQLiteHTMLReport(htmlNamedArtifact, "", HTMLReportOptions{}); err == nil {
		t.Fatal("WriteSQLiteHTMLReport error = nil, want default same-path rejection")
	}
	if err := artifact.Check(htmlNamedArtifact); err != nil {
		t.Fatalf("HTML-named source artifact was corrupted after rejected render: %v", err)
	}
}

func TestLoadSQLiteReportFallsBackToAggregateOnlyArtifact(t *testing.T) {
	artifactPath := testSQLiteHTMLArtifact(t, "Aggregate Only")
	db, err := sql.Open("sqlite", artifactPath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(`UPDATE workloads SET save_detailed = 0`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`DELETE FROM requests`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`DELETE FROM metric_stats WHERE metric IN (
		'request_output_throughput', 'request_ttft', 'request_tpot', 'request_itl_mean', 'latency'
	)`); err != nil {
		t.Fatal(err)
	}
	var measurementID int64
	if err := db.QueryRow(`SELECT id FROM measurements ORDER BY id LIMIT 1`).Scan(&measurementID); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`UPDATE measurements SET metadata_json = '{"ttft_source":"stream"}' WHERE id = ?`, measurementID); err != nil {
		t.Fatal(err)
	}
	for _, metric := range []struct {
		name  string
		mean  float64
		count int
	}{
		{"ttft", 321, 2},
		{"tpot", 45, 2},
	} {
		if _, err := db.Exec(`INSERT INTO metric_stats (
			measurement_id, metric, unit, mean, count
		) VALUES (?, ?, 'ms', ?, ?)`, measurementID, metric.name, metric.mean, metric.count); err != nil {
			t.Fatal(err)
		}
	}
	doc, err := LoadSQLiteReport(artifactPath)
	if err != nil {
		t.Fatal(err)
	}
	if doc.RequestSummary.Total != 2 || doc.RequestSummary.Completed != 2 || doc.RequestSummary.Failed != 0 {
		t.Fatalf("request summary = %+v, want aggregate measurement counts", doc.RequestSummary)
	}
	if doc.RequestSummary.OutputTokSMean != "123.400" || doc.RequestSummary.TTFTMeanMS != "321.000" || doc.RequestSummary.TPOTMeanMS != "45.000" {
		t.Fatalf("aggregate request summary = %+v", doc.RequestSummary)
	}
	if got := doc.Measurements[0].TTFTMeanMS; got != "321.000" {
		t.Fatalf("measurement TTFT = %q, want aggregate fallback", got)
	}
	if got := doc.Measurements[0].TPOTMeanMS; got != "45.000" {
		t.Fatalf("measurement TPOT = %q, want aggregate fallback", got)
	}
}

func TestLoadSQLiteReportMergesDetailedAndAggregateOnlySummary(t *testing.T) {
	artifactPath := testSQLiteHTMLArtifact(t, "Mixed Summary")
	db, err := sql.Open("sqlite", artifactPath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	insertAggregateOnlyMeasurement(t, db, 3, 300, 50, 200)

	doc, err := LoadSQLiteReport(artifactPath)
	if err != nil {
		t.Fatal(err)
	}
	if doc.RequestSummary.Total != 5 || doc.RequestSummary.Completed != 5 || doc.RequestSummary.Failed != 0 {
		t.Fatalf("mixed request summary = %+v, want detailed plus aggregate counts", doc.RequestSummary)
	}
	if doc.RequestSummary.TTFTMeanMS != "260.000" || doc.RequestSummary.TPOTMeanMS != "42.000" {
		t.Fatalf("mixed request summary did not merge weighted metrics: %+v", doc.RequestSummary)
	}
}

func TestWriteSQLiteHTMLReportHandlesQueryCharacterPath(t *testing.T) {
	artifactPath := filepath.Join(t.TempDir(), "a?b", "run.sqlite")
	createTestSQLiteHTMLArtifact(t, artifactPath, "Query Character Path")
	if _, err := os.Stat(artifactPath); err != nil {
		t.Fatalf("artifact was not written at query-character path: %v", err)
	}
	outputPath := filepath.Join(filepath.Dir(artifactPath), "report.html")
	if err := WriteSQLiteHTMLReport(artifactPath, outputPath, HTMLReportOptions{Store: true}); err != nil {
		t.Fatal(err)
	}
	if err := artifact.Check(artifactPath); err != nil {
		t.Fatalf("query-character artifact check: %v", err)
	}
	if _, err := os.Stat(outputPath); err != nil {
		t.Fatalf("HTML report was not written at query-character path: %v", err)
	}
}

func TestLoadSQLiteReportIgnoresEmptyNotableEventMessages(t *testing.T) {
	artifactPath := testSQLiteHTMLArtifact(t, "Empty Events")
	doc, err := LoadSQLiteReport(artifactPath)
	if err != nil {
		t.Fatal(err)
	}
	if len(doc.NotableEvents) != 0 {
		t.Fatalf("notable events = %+v, want empty routine events ignored", doc.NotableEvents)
	}
}

func insertAggregateOnlyMeasurement(t *testing.T, db *sql.DB, completed int, ttft, tpot, outputTokS float64) int64 {
	t.Helper()
	if _, err := db.Exec(`INSERT INTO workloads (
		id, run_id, name, role, phase, traffic_json, concurrency_json, samples, repeats,
		save_detailed, capture_payload_artifacts, metadata_json
	) SELECT 'aggregate-only', run_id, 'aggregate-only', role, phase, traffic_json, '[2]', 8, 1, 0, capture_payload_artifacts, metadata_json
		FROM workloads ORDER BY id LIMIT 1`); err != nil {
		t.Fatal(err)
	}
	result, err := db.Exec(`INSERT INTO measurements (
		run_id, profile_id, workload_id, repeat_index, concurrency, samples_requested, status,
		completed_requests, failed_requests, prompt_tokens, completion_tokens, total_tokens,
		aggregate_output_tok_s, per_user_output_tok_s, aggregate_total_tok_s
	) SELECT run_id, profile_id, 'aggregate-only', 0, 2, 8, 'completed',
		?, 0, 300, 30, 330, ?, ? / 2.0, ? + 100.0
		FROM measurements ORDER BY id LIMIT 1`, completed, outputTokS, outputTokS, outputTokS)
	if err != nil {
		t.Fatal(err)
	}
	measurementID, err := result.LastInsertId()
	if err != nil {
		t.Fatal(err)
	}
	insertAggregateMetric(t, db, measurementID, "ttft", "ms", ttft, completed)
	insertAggregateMetric(t, db, measurementID, "tpot", "ms", tpot, completed)
	return measurementID
}

func insertAggregateMetric(t *testing.T, db *sql.DB, measurementID int64, name, unit string, mean float64, count int) {
	t.Helper()
	if _, err := db.Exec(`INSERT INTO metric_stats (
		measurement_id, metric, unit, mean, count
	) VALUES (?, ?, ?, ?, ?)`, measurementID, name, unit, mean, count); err != nil {
		t.Fatal(err)
	}
}

func testSQLiteHTMLArtifact(t *testing.T, name string) string {
	t.Helper()
	artifactPath := filepath.Join(t.TempDir(), "run.sqlite")
	createTestSQLiteHTMLArtifact(t, artifactPath, name)
	return artifactPath
}

func createTestSQLiteHTMLArtifact(t *testing.T, artifactPath, name string) {
	t.Helper()
	db, err := artifact.Create(artifactPath, artifact.Schema)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	runID := "run-1"
	createdAt := "2026-01-01T00:00:00Z"
	originalSpec := `{"version":"1","name":"original"}`
	normalizedSpec := `{"version":"1","name":"normalized"}`
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
	for _, spec := range []struct {
		kind    string
		content string
	}{
		{kind: "original", content: originalSpec},
		{kind: "normalized", content: normalizedSpec},
	} {
		if _, err := db.Exec(`INSERT INTO specs (
			run_id, kind, format, content, sha256, created_at
		) VALUES (?, ?, 'json', ?, ?, ?)`, runID, spec.kind, spec.content, artifact.SHA256Hex([]byte(spec.content)), createdAt); err != nil {
			t.Fatal(err)
		}
	}
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
		'profile-1', ?, 'engine-1', '8k', 'nvidia/diffusiongemma-26B-A4B-it-NVFP4',
		'127.0.0.1', 8108, 'http://127.0.0.1:8108', 1, 8192, 16, 8192, 0.35,
		1, 2, '{}', '{}', '{}', '{}'
	)`, runID); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO workloads (
		id, run_id, name, role, phase, traffic_json, concurrency_json, samples, repeats,
		save_detailed, capture_payload_artifacts, dataset_json, request_json, metadata_json
	) VALUES (
		'workload-1', ?, 'prefill-8k', 'benchmark', 'prefill',
		'{"backend":"openai-chat","dataset_name":"random","random_input_len":8192,"random_output_len":16,"request_rate":"inf"}',
		'[4,8]', 16, 1, 1, 0, '{}', '{}',
		'{"context":{"target":8192,"semantics":"capacity"}}'
	)`, runID); err != nil {
		t.Fatal(err)
	}
	phaseResult, err := db.Exec(`INSERT INTO phases (
		run_id, profile_id, workload_id, name, type, status, started_at, completed_at, metadata_json
	) VALUES (?, 'profile-1', 'workload-1', 'measurement', 'measurement', 'completed', ?, ?, '{}')`,
		runID, createdAt, createdAt)
	if err != nil {
		t.Fatal(err)
	}
	phaseID, err := phaseResult.LastInsertId()
	if err != nil {
		t.Fatal(err)
	}
	measurementResult, err := db.Exec(`INSERT INTO measurements (
		run_id, profile_id, workload_id, phase_id, repeat_index, concurrency, samples_requested,
		status, started_at, completed_at, wall_time_ms, completed_requests, failed_requests,
		prompt_tokens, completion_tokens, total_tokens, aggregate_output_tok_s,
		per_user_output_tok_s, aggregate_total_tok_s, metadata_json
	) VALUES (
		?, 'profile-1', 'workload-1', ?, 0, 4, 8, 'completed', ?, ?, 1000,
		2, 0, 200, 20, 220, 123.4, 61.7, 223.4, '{}'
	)`, runID, phaseID, createdAt, createdAt)
	if err != nil {
		t.Fatal(err)
	}
	measurementID, err := measurementResult.LastInsertId()
	if err != nil {
		t.Fatal(err)
	}
	seedSQLiteHTMLMetrics(t, db, measurementID)
}

func seedSQLiteHTMLMetrics(t *testing.T, db *sql.DB, measurementID int64) {
	t.Helper()
	for _, metric := range []struct {
		name   string
		unit   string
		mean   float64
		stddev float64
	}{
		{"request_output_throughput", "tok/s", 61.7, 2.5},
		{"latency", "ms", 1200, 50},
		{"request_ttft", "ms", 200, 10},
		{"request_tpot", "ms", 30, 3},
		{"request_itl_mean", "ms", 28, 2},
	} {
		if _, err := db.Exec(`INSERT INTO metric_stats (
			measurement_id, metric, unit, mean, stddev, min, p50, p90, p95, p99, max, count
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, 2)`,
			measurementID, metric.name, metric.unit, metric.mean, metric.stddev,
			metric.mean-1, metric.mean, metric.mean+1, metric.mean+2, metric.mean+3, metric.mean+4); err != nil {
			t.Fatal(err)
		}
	}
	for i := 0; i < 2; i++ {
		if _, err := db.Exec(`INSERT INTO requests (
			measurement_id, request_index, request_id, status, streamed, started_at,
			completed_at, latency_ms, ttft_ms, tpot_ms, itl_mean_ms,
			prompt_tokens, completion_tokens, total_tokens, output_tok_s, total_tok_s
		) VALUES (?, ?, ?, 'completed', 1, '2026-01-01T00:00:00Z',
			'2026-01-01T00:00:01Z', 1000, 200, 30, 28, 100, 10, 110, 61.7, 111.7)`,
			measurementID, i, "request"); err != nil {
			t.Fatal(err)
		}
	}
}

func TestDisplayFailureMetricPrefersOutcome(t *testing.T) {
	if got := displayFailureMetric("0.000", "failed"); got != "failed" {
		t.Fatalf("failed cell = %q, want the failure label instead of a residual number", got)
	}
	if got := displayFailureMetric("35.9", ""); got != "35.9" {
		t.Fatalf("healthy cell = %q, want the value", got)
	}
}

func TestFormatDurationDisplayComposites(t *testing.T) {
	for value, want := range map[string]string{
		"144744.403":          "2m25s",
		"9187.587":            "9.2s",
		"321.4":               "321ms",
		"144744.403 ± 1200.5": "2m25s ± 1.2s",
		"-":                   "-",
		"skipped":             "skipped",
	} {
		if got := FormatDurationDisplay(value); got != want {
			t.Fatalf("FormatDurationDisplay(%q) = %q, want %q", value, got, want)
		}
	}
}

func TestCellDetailIncludesMetrics(t *testing.T) {
	artifactPath := filepath.Join(t.TempDir(), "run.sqlite")
	createTestSQLiteHTMLArtifact(t, artifactPath, "DetailMetrics")
	doc, err := LoadSQLiteReport(artifactPath)
	if err != nil {
		t.Fatal(err)
	}
	detail := doc.ThroughputRows[0].Detail
	if len(detail.Metrics) == 0 {
		t.Fatal("detail metrics empty, want measurement numbers in the detail view")
	}
	labels := map[string]bool{}
	for _, item := range detail.Metrics {
		labels[item.Label] = true
	}
	for _, want := range []string{"Requests ok/err", "Output tok/s", "Latency p50/p95/p99"} {
		if !labels[want] {
			t.Fatalf("detail metrics missing %q; got %v", want, labels)
		}
	}
}

func TestSpecProvenanceRendersInReport(t *testing.T) {
	artifactPath := filepath.Join(t.TempDir(), "run.sqlite")
	createTestSQLiteHTMLArtifact(t, artifactPath, "Provenance")
	doc, err := LoadSQLiteReport(artifactPath)
	if err != nil {
		t.Fatal(err)
	}
	// The fixture's original spec has no generator stamp.
	found := false
	for _, item := range doc.MetadataItems {
		if item.Label == "Spec" {
			found = true
			if item.Value != "Custom grid (hand-authored)" {
				t.Fatalf("spec provenance = %q, want custom grid for an unstamped spec", item.Value)
			}
		}
	}
	if !found {
		t.Fatal("metadata missing Spec provenance item")
	}
}

func TestGeneratedSpecTrimsRenderAsRows(t *testing.T) {
	artifactPath := filepath.Join(t.TempDir(), "run.sqlite")
	createTestSQLiteHTMLArtifact(t, artifactPath, "Trims")
	base := map[string]any{"version": "1", "name": "stamped", "model": "example/model"}
	stamp := artifact.GeneratorStamp{
		Tool:        "localperf sweep plan",
		Version:     "1",
		Intent:      json.RawMessage(`{"concurrency":[1,4,8,16]}`),
		LadderTrims: []artifact.LadderTrim{{Context: 65536, MaxConcurrency: 8, Reason: "12 GiB KV budget"}},
	}
	base["generator"] = stamp
	unhashed, err := json.Marshal(base)
	if err != nil {
		t.Fatal(err)
	}
	// The hash covers the stamp's trusted fields; only content_hash itself
	// is excluded.
	hash, err := artifact.CanonicalSpecHash(unhashed)
	if err != nil {
		t.Fatal(err)
	}
	stamp.ContentHash = hash
	base["generator"] = stamp
	content, err := json.Marshal(base)
	if err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", artifactPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`UPDATE specs SET content = ?, sha256 = ? WHERE kind = 'original'`, string(content), artifact.SHA256Hex(content)); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	doc, err := LoadSQLiteReport(artifactPath)
	if err != nil {
		t.Fatal(err)
	}
	trimmed := []SQLiteReportThroughputRow{}
	for _, row := range doc.ThroughputRows {
		if row.FailureLabel == "trimmed" {
			trimmed = append(trimmed, row)
		}
	}
	// c16 above the trim cap, decode + prefill.
	if len(trimmed) != 2 {
		t.Fatalf("trimmed rows = %d, want 2 synthesized rows for c16", len(trimmed))
	}
	row := trimmed[0]
	if row.Concurrency != 16 || row.ContextTarget != 65536 || !strings.Contains(row.FailureReason, "12 GiB KV budget") {
		t.Fatalf("trimmed row = %+v, want c16 at 64k with the declared reason", row)
	}
	for _, item := range doc.MetadataItems {
		if item.Label == "Spec" && !strings.Contains(item.Value, "Generated default sweep") {
			t.Fatalf("spec provenance = %q, want generated", item.Value)
		}
	}
}
