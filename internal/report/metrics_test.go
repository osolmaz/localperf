package report

import (
	"database/sql"
	"path/filepath"
	"strings"
	"testing"

	"github.com/osolmaz/localperf/internal/artifact"
)

// TestTokenWeightedITLMatchesBruteForce checks the SQL derivation
// sum(itl_mean * (completion-1)) / sum(completion-1) against gap arrays
// computed by hand, and that it differs from the request-weighted
// mean-of-means on a skewed fixture.
func TestTokenWeightedITLAndDerivedRequestMetrics(t *testing.T) {
	artifactPath := filepath.Join(t.TempDir(), "run.sqlite")
	createTestSQLiteHTMLArtifact(t, artifactPath, "Derived")
	db, err := sql.Open("sqlite", artifactPath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	// Replace the fixture's request rows with hand-computed ones.
	if _, err := db.Exec(`DELETE FROM requests`); err != nil {
		t.Fatal(err)
	}
	// Request A: gaps [10,10,10] -> itl_mean 10ms, 4 completion tokens.
	// Request B: gaps [100] -> itl_mean 100ms, 2 completion tokens.
	// Token-weighted ITL = (30+100)/4 = 32.5ms; mean-of-means would be 55ms.
	insertDerivedRequest(t, db, 1, 0, "completed", "2026-01-01T00:00:00Z", "2026-01-01T00:00:10Z", 10, 4, "")
	insertDerivedRequest(t, db, 1, 1, "completed", "2026-01-01T00:00:00Z", "2026-01-01T00:00:10Z", 100, 2, "")
	insertDerivedRequest(t, db, 1, 2, "failed", "2026-01-01T00:00:00Z", "2026-01-01T00:00:01Z", 0, 0, "timeout")
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	doc, err := LoadSQLiteReport(artifactPath)
	if err != nil {
		t.Fatal(err)
	}
	if len(doc.Measurements) != 1 {
		t.Fatalf("measurements = %d, want 1", len(doc.Measurements))
	}
	measurement := doc.Measurements[0]
	if measurement.ITLTokenWeightedMS != "32.500" {
		t.Fatalf("token-weighted ITL = %q, want 32.500 (request-weighted mean-of-means would be 55)", measurement.ITLTokenWeightedMS)
	}
	// Two 10s requests over a 10s span: achieved concurrency 2 of requested 4.
	if measurement.AchievedConcurrency != "~2 (of 4)" {
		t.Fatalf("achieved concurrency = %q, want ~2 (of 4)", measurement.AchievedConcurrency)
	}
	if measurement.FailureBreakdown != "1 timeout" {
		t.Fatalf("failure breakdown = %q, want 1 timeout", measurement.FailureBreakdown)
	}
	// 2 completed requests over 1000ms wall time.
	if measurement.RPS != "2.000" {
		t.Fatalf("RPS = %q, want 2.000", measurement.RPS)
	}
}

func TestGenerationPopulatesHistoricalDecodeAndPrefillColumns(t *testing.T) {
	artifactPath := testSQLiteHTMLArtifact(t, "Generation Prefill")
	db, err := sql.Open("sqlite", artifactPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`UPDATE workloads SET name = 'generate-full', phase = 'decode' WHERE id = 'workload-1'`); err != nil {
		_ = db.Close()
		t.Fatal(err)
	}
	if _, err := db.Exec(`UPDATE measurements SET metadata_json = '{"ttft_source":"stream"}' WHERE id = 1`); err != nil {
		_ = db.Close()
		t.Fatal(err)
	}
	insertAggregateMetric(t, db, 1, "effective_prefill_throughput", "tok/s", 1764, 2)
	insertAggregateMetric(t, db, 1, "request_effective_prefill_throughput", "tok/s", 1500, 2)
	insertAggregateMetric(t, db, 1, "request_decode_throughput", "tok/s", 40, 2)
	if _, err := db.Exec(`UPDATE metric_stats SET p50 = CASE metric
		WHEN 'request_effective_prefill_throughput' THEN 1400
		WHEN 'request_decode_throughput' THEN 35 END
		WHERE measurement_id = 1 AND metric IN ('request_effective_prefill_throughput', 'request_decode_throughput')`); err != nil {
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
	row := doc.ThroughputGroups[0].Rows[0]
	if row.DecodeTokS != "123.400" || row.DecodePerUserTokS != "61.700" {
		t.Fatalf("decode columns = %q/%q", row.DecodeTokS, row.DecodePerUserTokS)
	}
	if row.PrefillTokS != "1764.000" || row.PrefillPerUserTokS != "441.000" {
		t.Fatalf("prefill columns = %q/%q, want aggregate and aggregate/users", row.PrefillTokS, row.PrefillPerUserTokS)
	}
	if row.DecodeTTFTMeanMS != row.PrefillTTFTMeanMS || row.DecodeTTFTMS != row.PrefillTTFTMS {
		t.Fatalf("decode/prefill TTFT differ: %+v", row)
	}
	if row.OK != 2 || row.Err != 0 || row.Result != "2 / 0" {
		t.Fatalf("derived prefill double-counted requests: %+v", row)
	}
	detailLabels := map[string]bool{}
	for _, item := range row.DecodeDetail.Metrics {
		detailLabels[item.Label] = true
	}
	for _, label := range []string{
		"Request effective prefill mean",
		"Request effective prefill p50",
		"Post-first-token/user mean",
		"Post-first-token/user p50",
	} {
		if !detailLabels[label] {
			t.Fatalf("decode detail is missing request distribution %q: %v", label, detailLabels)
		}
	}

	var out strings.Builder
	if err := RenderHTMLReport(&out, doc, HTMLReportOptions{}); err != nil {
		t.Fatal(err)
	}
	headline := `<th class="num">Users</th><th class="num">Decode tok/s</th><th class="num">Decode/user</th><th class="num">Decode TTFT avg</th><th class="num">Decode TTFT p99</th><th class="num">Prefill tok/s</th><th class="num">Prefill/user</th><th class="num">Prefill TTFT avg</th><th class="num">Prefill TTFT p99</th><th class="num">OK / Err</th>`
	if !strings.Contains(out.String(), headline) {
		t.Fatalf("HTML report does not retain exact historical headline order")
	}
	if strings.Contains(out.String(), "Full-run timing") {
		t.Fatal("HTML report contains rejected full-run timing section")
	}
}

func TestThroughputGroupsPreserveDistinctGenerationWorkloads(t *testing.T) {
	row := func(workload, shape string, concurrency int) SQLiteReportThroughputRow {
		return SQLiteReportThroughputRow{
			Mode:                        "decode",
			Profile:                     "64k",
			Workload:                    workload,
			ContextWindow:               65536,
			ContextLabel:                "64k capacity",
			ContextSortKey:              65536,
			ContextTarget:               65536,
			ContextSemantics:            "capacity",
			Concurrency:                 concurrency,
			Shape:                       shape,
			ThroughputTokS:              "100",
			PerUserTokS:                 "100",
			EffectivePrefillTokS:        "200",
			EffectivePrefillPerUserTokS: "200",
			Status:                      "completed",
			CompletedRequests:           concurrency,
			Detail: SQLiteReportCellDetail{
				Available: true,
				Mode:      "decode",
				Workload:  workload,
				Shape:     shape,
			},
		}
	}
	rows := []SQLiteReportThroughputRow{
		row("generate-empty", "same shape", 1),
		row("generate-full", "same shape", 6),
	}
	groups := sqliteReportThroughputGroups(rows)
	if len(groups) != 2 {
		t.Fatalf("throughput groups = %d, want one table per generation workload", len(groups))
	}
	workloads := map[string]int{}
	for _, group := range groups {
		if len(group.Rows) != 1 {
			t.Fatalf("rows in group = %d, want one disjoint point: %+v", len(group.Rows), group)
		}
		workloads[group.Rows[0].DecodeDetail.Workload]++
		if !metadataHasValue(group.AxisItems, "Workload", group.Rows[0].DecodeDetail.Workload) {
			t.Fatalf("group does not expose workload identity: %+v", group.AxisItems)
		}
	}
	if workloads["generate-empty"] != 1 || workloads["generate-full"] != 1 {
		t.Fatalf("grouped workloads = %v, want both generation workloads preserved", workloads)
	}
}

func TestDedicatedPrefillPopulatesEveryGenerationWorkloadGroup(t *testing.T) {
	decode := func(workload string) SQLiteReportThroughputRow {
		return SQLiteReportThroughputRow{
			Mode: "decode", Profile: "64k", Workload: workload, ContextWindow: 65536,
			ContextLabel: "64k capacity", ContextSortKey: 65536, ContextTarget: 65536,
			ContextSemantics: "capacity", Concurrency: 6, Shape: "decode shape",
			ThroughputTokS: "100", EffectivePrefillTokS: "1500", Status: "completed",
			Detail: SQLiteReportCellDetail{Available: true, Mode: "decode", Workload: workload, Shape: "decode shape"},
		}
	}
	prefill := SQLiteReportThroughputRow{
		Mode: "prefill", Profile: "64k", Workload: "prefill-full", ContextWindow: 65536,
		ContextLabel: "64k capacity", ContextSortKey: 65536, ContextTarget: 65536,
		ContextSemantics: "capacity", Concurrency: 6, Shape: "prefill shape",
		ThroughputTokS: "2200", Status: "completed",
		Detail: SQLiteReportCellDetail{Available: true, Mode: "prefill", Workload: "prefill-full", Shape: "prefill shape"},
	}
	groups := sqliteReportThroughputGroups([]SQLiteReportThroughputRow{prefill, decode("generate-empty"), decode("generate-full")})
	if len(groups) != 2 {
		t.Fatalf("groups = %d, want two generation workloads", len(groups))
	}
	for _, group := range groups {
		got := group.Rows[0]
		if got.PrefillDerived || got.PrefillTokS != "2200" || got.PrefillDetail.Workload != "prefill-full" {
			t.Fatalf("group %q prefill precedence = %+v", group.DecodeWorkload, got)
		}
	}
}

func metadataHasValue(items []SQLiteReportMetadataItem, label, value string) bool {
	for _, item := range items {
		if item.Label == label && item.Value == value {
			return true
		}
	}
	return false
}

func TestGenerationPrefillRequiresStreamedTTFTSource(t *testing.T) {
	artifactPath := testSQLiteHTMLArtifact(t, "Unproven Prefill")
	db, err := sql.Open("sqlite", artifactPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`UPDATE workloads SET phase = 'decode' WHERE id = 'workload-1'`); err != nil {
		_ = db.Close()
		t.Fatal(err)
	}
	insertAggregateMetric(t, db, 1, "effective_prefill_throughput", "tok/s", 1764, 2)
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	doc, err := LoadSQLiteReport(artifactPath)
	if err != nil {
		t.Fatal(err)
	}
	if got := doc.ThroughputGroups[0].Rows[0].PrefillTokS; got != "-" {
		t.Fatalf("unproven effective prefill = %q, want unavailable", got)
	}
}

func TestDiagnosticWorkloadsAreExcludedFromBenchmarkViews(t *testing.T) {
	artifactPath := filepath.Join(t.TempDir(), "run.sqlite")
	createTestSQLiteHTMLArtifact(t, artifactPath, "Diagnostic")
	db, err := sql.Open("sqlite", artifactPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`UPDATE workloads SET role = 'diagnostic'`); err != nil {
		db.Close()
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	doc, err := LoadSQLiteReport(artifactPath)
	if err != nil {
		t.Fatal(err)
	}
	if len(doc.Workloads) != 0 || len(doc.Measurements) != 0 || len(doc.ThroughputRows) != 0 || doc.RequestSummary.Total != 0 {
		t.Fatalf("diagnostic data reached benchmark views: workloads=%d measurements=%d throughput=%d requests=%d", len(doc.Workloads), len(doc.Measurements), len(doc.ThroughputRows), doc.RequestSummary.Total)
	}
}

func TestRepeatAggregationRendersSpreadAndRepeatRows(t *testing.T) {
	artifactPath := filepath.Join(t.TempDir(), "run.sqlite")
	createTestSQLiteHTMLArtifact(t, artifactPath, "Repeats")
	db, err := sql.Open("sqlite", artifactPath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	// Second repeat of the fixture's (profile-1, workload-1, c4) point.
	result, err := db.Exec(`INSERT INTO measurements (
		run_id, profile_id, workload_id, repeat_index, concurrency, samples_requested,
		status, started_at, completed_at, wall_time_ms, completed_requests, failed_requests,
		prompt_tokens, completion_tokens, total_tokens, aggregate_output_tok_s,
		per_user_output_tok_s, aggregate_total_tok_s
	) VALUES (
		'run-1', 'profile-1', 'workload-1', 1, 4, 8, 'completed',
		'2026-01-01T00:02:00Z', '2026-01-01T00:03:00Z', 1000, 2, 0, 200, 20, 220,
		133.4, 66.7, 233.4
	)`)
	if err != nil {
		t.Fatal(err)
	}
	measurementID, err := result.LastInsertId()
	if err != nil {
		t.Fatal(err)
	}
	seedSQLiteHTMLMetrics(t, db, measurementID)
	if _, err := db.Exec(`UPDATE measurements SET metadata_json = '{"ttft_source":"stream"}'`); err != nil {
		t.Fatal(err)
	}
	insertAggregateMetric(t, db, 1, "effective_prefill_throughput", "tok/s", 100, 1)
	insertAggregateMetric(t, db, measurementID, "effective_prefill_throughput", "tok/s", 200, 1)
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	doc, err := LoadSQLiteReport(artifactPath)
	if err != nil {
		t.Fatal(err)
	}
	if len(doc.Measurements) != 1 {
		t.Fatalf("aggregated measurements = %d, want 1", len(doc.Measurements))
	}
	combined := doc.Measurements[0]
	if combined.RepeatCount != 2 {
		t.Fatalf("repeat count = %d, want 2", combined.RepeatCount)
	}
	// mean(123.4, 133.4) = 128.4, sample stddev = 7.071
	if !strings.HasPrefix(combined.OutputTokS, "128.400 ±") {
		t.Fatalf("aggregated output tok/s = %q, want mean ± spread", combined.OutputTokS)
	}
	if !strings.HasPrefix(combined.EffectivePrefillTokS, "150.000 ±") {
		t.Fatalf("aggregated effective prefill = %q, want mean ± spread", combined.EffectivePrefillTokS)
	}
	if combined.CompletedRequests != 4 {
		t.Fatalf("aggregated completed = %d, want summed 4", combined.CompletedRequests)
	}
	// Token totals must sum with request counts so per-request derivations
	// stay exact: 400 prompt / 4 requests keeps the 100 in / 10 out shape.
	if combined.PromptTokensValue != 400 || combined.CompletionTokensValue != 40 {
		t.Fatalf("aggregated tokens = %d/%d, want 400/40", combined.PromptTokensValue, combined.CompletionTokensValue)
	}
	if shape := requestShape(combined); shape != "100 in / 10 out" {
		t.Fatalf("aggregated shape = %q, want 100 in / 10 out", shape)
	}
	if combined.WallTimeMSValue != 2000 {
		t.Fatalf("aggregated wall time = %f, want summed 2000", combined.WallTimeMSValue)
	}
	if len(doc.RepeatDetails) != 2 {
		t.Fatalf("repeat details = %d, want 2", len(doc.RepeatDetails))
	}
	if len(doc.ThroughputRows) != 1 {
		t.Fatalf("throughput rows = %d, want 1", len(doc.ThroughputRows))
	}
	detail := doc.ThroughputRows[0].Detail
	if detail.Source != "aggregate of 2 repeats" || detail.RunID != "" || detail.MeasurementID != 0 || detail.BenchmarkCommand != "" {
		t.Fatalf("aggregate detail = %+v, want aggregate source without first-repeat provenance", detail)
	}

	var out strings.Builder
	if err := RenderHTMLReport(&out, doc, HTMLReportOptions{}); err != nil {
		t.Fatal(err)
	}
	html := out.String()
	for _, want := range []string{"Repeats", "Per-repeat rows", "Effective prefill tok/s", ">100.000</td>", ">200.000</td>", "±", "&times;2"} {
		if !strings.Contains(html, want) {
			t.Fatalf("HTML report missing %q", want)
		}
	}
}

func TestTTFTHiddenWithoutStreamedSource(t *testing.T) {
	artifactPath := filepath.Join(t.TempDir(), "run.sqlite")
	// The fixture seeds request_ttft stats but no ttft_source marker, so the
	// value lacks the provenance needed for reporting.
	createTestSQLiteHTMLArtifact(t, artifactPath, "TTFTGate")
	doc, err := LoadSQLiteReport(artifactPath)
	if err != nil {
		t.Fatal(err)
	}
	measurement := doc.Measurements[0]
	for label, got := range map[string]string{
		"mean": measurement.TTFTMeanMS,
		"p50":  measurement.TTFTP50MS,
		"p95":  measurement.TTFTP95MS,
		"p99":  measurement.TTFTP99MS,
	} {
		if got != "-" {
			t.Fatalf("TTFT %s = %q, want \"-\" without the streamed-source marker", label, got)
		}
	}
}

func TestSLOTTFTTargetRequiresStreamedSource(t *testing.T) {
	artifactPath := filepath.Join(t.TempDir(), "run.sqlite")
	createTestSQLiteHTMLArtifact(t, artifactPath, "SLOGate")
	db, err := sql.Open("sqlite", artifactPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`UPDATE workloads SET metadata_json = '{"context":{"target":8192,"semantics":"capacity"},"slo":{"ttft_p95_ms":500}}' WHERE id = 'workload-1'`); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	doc, err := LoadSQLiteReport(artifactPath)
	if err != nil {
		t.Fatal(err)
	}
	measurement := doc.Measurements[0]
	if measurement.SLOMetPct != "-" || measurement.GoodputRPS != "-" {
		t.Fatalf("SLO met/goodput = %q/%q, want unmeasurable without streamed TTFT", measurement.SLOMetPct, measurement.GoodputRPS)
	}
	if !strings.Contains(measurement.SLONote, "requires streamed samples") {
		t.Fatalf("SLO note = %q, want streamed-samples caveat", measurement.SLONote)
	}
}

func TestSLOGoodputDerivation(t *testing.T) {
	artifactPath := filepath.Join(t.TempDir(), "run.sqlite")
	createTestSQLiteHTMLArtifact(t, artifactPath, "SLO")
	db, err := sql.Open("sqlite", artifactPath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(`UPDATE workloads SET metadata_json = '{"context":{"target":8192,"semantics":"capacity"},"slo":{"ttft_p95_ms":500}}' WHERE id = 'workload-1'`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`UPDATE measurements SET metadata_json = '{"ttft_source":"stream"}' WHERE id = 1`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`DELETE FROM requests`); err != nil {
		t.Fatal(err)
	}
	for index, ttft := range []float64{100, 900} {
		if _, err := db.Exec(`INSERT INTO requests (
			measurement_id, request_index, status, streamed, started_at, completed_at,
			ttft_ms, latency_ms, prompt_tokens, completion_tokens
		) VALUES (1, ?, 'completed', 1, '2026-01-01T00:00:00Z', '2026-01-01T00:00:05Z', ?, 5000, 100, 10)`,
			index, ttft); err != nil {
			t.Fatal(err)
		}
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	doc, err := LoadSQLiteReport(artifactPath)
	if err != nil {
		t.Fatal(err)
	}
	if !doc.HasSLO {
		t.Fatal("HasSLO = false, want true when a workload declares an SLO")
	}
	measurement := doc.Measurements[0]
	if measurement.SLOMetPct != "50%" {
		t.Fatalf("SLO met = %q, want 50%%", measurement.SLOMetPct)
	}
	// 1 SLO-met request over 1000ms wall time.
	if measurement.GoodputRPS != "1.000" {
		t.Fatalf("goodput = %q, want 1.000", measurement.GoodputRPS)
	}
	if measurement.SLONote != "ttft<=500ms" {
		t.Fatalf("SLO note = %q, want ttft<=500ms", measurement.SLONote)
	}
	var out strings.Builder
	if err := RenderHTMLReport(&out, doc, HTMLReportOptions{}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "% in SLO") || !strings.Contains(out.String(), "Goodput req/s") {
		t.Fatal("HTML report missing SLO columns when an SLO is declared")
	}
	// Goodput must be visible in the headline table, not only the hidden
	// detail sections.
	if !strings.Contains(out.String(), "SLO / goodput") || !strings.Contains(out.String(), "50% / 1.000") {
		t.Fatal("HTML report missing visible SLO/goodput in the throughput table")
	}
	if got := strings.Count(out.String(), `class="slo-col"`); got != 2 {
		t.Fatalf("SLO colgroup entries = %d, want desktop and phone widths", got)
	}
}

// TestMultiRunReportAggregatesAcrossRuns checks model-level rendering: all
// runs load, the same point from two runs aggregates like repeats, and the
// per-repeat rows keep run provenance.
func TestMultiRunReportAggregatesAcrossRuns(t *testing.T) {
	artifactPath := filepath.Join(t.TempDir(), "model.sqlite")
	createTestSQLiteHTMLArtifact(t, artifactPath, "Model Level")
	db, err := sql.Open("sqlite", artifactPath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	// Second run measuring the same (profile name, workload name, c4) point.
	statements := []string{
		`INSERT INTO run (id, name, status, created_at, started_at, completed_at, command_line_json, host_json, labels_json)
			VALUES ('run-2', 'Model Level', 'completed', '2026-01-02T00:00:00Z', '2026-01-02T00:00:00Z', '2026-01-02T00:10:00Z', '[]', '{}', '{}')`,
		`INSERT INTO specs (run_id, kind, format, content, sha256, created_at)
			VALUES ('run-2', 'original', 'json', '{}', '` + artifact.SHA256Hex([]byte("{}")) + `', '2026-01-02T00:00:00Z')`,
		`INSERT INTO specs (run_id, kind, format, content, sha256, created_at)
			VALUES ('run-2', 'normalized', 'json', '{}', '` + artifact.SHA256Hex([]byte("{}")) + `', '2026-01-02T00:00:00Z')`,
		`INSERT INTO engines (id, run_id, name, type, managed, command, version, env_json, metadata_json)
			VALUES ('run-2/vllm', 'run-2', 'vllm', 'vllm', 1, 'vllm', 'test', '{}',
			'{"identity":{"8k":{"models":{"data":[{"id":"served/other-model"}]}}}}')`,
		`INSERT INTO profiles (id, run_id, engine_id, name, model, port, managed, context_window, serve_json)
			VALUES ('run-2/8k', 'run-2', 'run-2/vllm', '8k', 'nvidia/diffusiongemma-26B-A4B-it-NVFP4', 8108, 1, 8192, '{}')`,
		`INSERT INTO workloads (id, run_id, name, role, phase, traffic_json, concurrency_json, samples, repeats, save_detailed, capture_payload_artifacts, metadata_json)
			VALUES ('run-2/prefill-8k', 'run-2', 'prefill-8k', 'benchmark', 'prefill', '{}', '[4]', 8, 1, 1, 0, '{"context":{"target":8192,"semantics":"capacity"}}')`,
		`INSERT INTO measurements (run_id, profile_id, workload_id, repeat_index, concurrency, samples_requested,
			status, started_at, completed_at, wall_time_ms, completed_requests, failed_requests,
			prompt_tokens, completion_tokens, total_tokens, aggregate_output_tok_s, per_user_output_tok_s, aggregate_total_tok_s)
			VALUES ('run-2', 'run-2/8k', 'run-2/prefill-8k', 0, 4, 8, 'completed',
			'2026-01-02T00:00:00Z', '2026-01-02T00:01:00Z', 1000, 2, 0, 200, 20, 220, 133.4, 66.7, 233.4)`,
	}
	for _, statement := range statements {
		if _, err := db.Exec(statement); err != nil {
			t.Fatalf("%v\nstatement: %s", err, statement)
		}
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	doc, err := LoadSQLiteReport(artifactPath)
	if err != nil {
		t.Fatal(err)
	}
	if len(doc.Runs) != 2 || doc.Run.ID != "run-2" {
		t.Fatalf("runs = %d latest = %q, want 2 runs with run-2 latest", len(doc.Runs), doc.Run.ID)
	}
	// The fixture's workload name differs per run id but shares the point
	// key (profile 8k, workload prefill-8k, c4), so the two runs aggregate.
	if len(doc.Measurements) != 1 || doc.Measurements[0].RepeatCount != 2 {
		t.Fatalf("aggregated measurements = %d (repeat count %d), want one cross-run aggregate of 2", len(doc.Measurements), doc.Measurements[0].RepeatCount)
	}
	if len(doc.RepeatDetails) != 2 || doc.RepeatDetails[0].RunID == doc.RepeatDetails[1].RunID {
		t.Fatalf("repeat details = %+v, want one row per run", doc.RepeatDetails)
	}
	var out strings.Builder
	if err := RenderHTMLReport(&out, doc, HTMLReportOptions{}); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"Runs", "run-1", "run-2", "Latest Run"} {
		if !strings.Contains(out.String(), want) {
			t.Fatalf("HTML report missing %q", want)
		}
	}
	// Namespaced engine ids must not break the declared-vs-served check:
	// run-2's probe reports a different model for profile 8k.
	if !strings.Contains(out.String(), "Model mismatch") || !strings.Contains(out.String(), "served/other-model") {
		t.Fatal("HTML report missing model mismatch for namespaced engine identity")
	}
}

func insertDerivedRequest(t *testing.T, db *sql.DB, measurementID int64, index int, status, startedAt, completedAt string, itlMeanMS float64, completionTokens int, errorType string) {
	t.Helper()
	var itl any
	if itlMeanMS > 0 {
		itl = itlMeanMS
	}
	var errType any
	if errorType != "" {
		errType = errorType
	}
	if _, err := db.Exec(`INSERT INTO requests (
		measurement_id, request_index, status, streamed, started_at, completed_at,
		itl_mean_ms, prompt_tokens, completion_tokens
	) VALUES (?, ?, ?, 1, ?, ?, ?, 100, ?)`,
		measurementID, index, status, startedAt, completedAt, itl, completionTokens); err != nil {
		t.Fatal(err)
	}
	if errType != nil {
		if _, err := db.Exec(`UPDATE requests SET error_type = ? WHERE measurement_id = ? AND request_index = ?`, errType, measurementID, index); err != nil {
			t.Fatal(err)
		}
	}
}
