package reportmodel

import (
	"strings"
	"testing"
	"time"

	"github.com/osolmaz/localperf/internal/report"
)

func TestBuildMergesRunsButSplitsActiveContexts(t *testing.T) {
	doc := report.SQLiteReportDocument{
		GeneratedAt: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
		ThroughputRows: []report.SQLiteReportThroughputRow{
			throughputRowWithProfileID("run-1", "run-1/profile-1", "32k", "16k active context", 16384, 1),
			throughputRowWithProfileID("run-2", "run-2/profile-1", "32k", "16k active context", 16384, 4),
			throughputRow("run-3", "32k", "32k active context", 32768, 8),
		},
	}

	model := Build("/tmp/report.sqlite", doc)
	if len(model.Throughput.Tables) != 2 {
		t.Fatalf("tables = %d, want 2", len(model.Throughput.Tables))
	}
	first := model.Throughput.Tables[0]
	if first.ContextLabel != "16k active context" {
		t.Fatalf("first context = %q, want 16k active context", first.ContextLabel)
	}
	if first.RunID != "" || len(first.RunIDs) != 2 {
		t.Fatalf("first run IDs = %q/%v, want merged multi-run table", first.RunID, first.RunIDs)
	}
	if len(first.Rows) != 2 {
		t.Fatalf("first rows = %d, want two merged concurrency points", len(first.Rows))
	}
	second := model.Throughput.Tables[1]
	if second.ContextLabel != "32k active context" || len(second.Rows) != 1 {
		t.Fatalf("second table = %+v, want separate 32k context table", second)
	}
}

func TestBuildLabelsUnverifiedContext(t *testing.T) {
	doc := report.SQLiteReportDocument{
		GeneratedAt: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
		ThroughputRows: []report.SQLiteReportThroughputRow{
			{
				Mode:             "decode",
				RunID:            "run-1",
				MeasurementID:    1,
				ProfileID:        "profile-1",
				Profile:          "64k",
				Model:            "test/model",
				ContextWindow:    65536,
				ContextLabel:     "unverified (declared 64k active)",
				ContextTarget:    65536,
				ContextSemantics: "active",
				Status:           "skipped",
				Concurrency:      1,
				Shape:            "-",
				Detail: report.SQLiteReportCellDetail{
					Available:     true,
					Mode:          "decode",
					MeasurementID: 1,
					ContextLabel:  "unverified (declared 64k active)",
				},
			},
		},
	}

	table := Build("/tmp/report.sqlite", doc).Throughput.Tables[0]
	if table.ContextStatus != "unverified" {
		t.Fatalf("context status = %q, want unverified", table.ContextStatus)
	}
	if table.Warning == "" {
		t.Fatal("warning is empty, want unverified warning")
	}
}

func throughputRow(runID, profile, contextLabel string, contextSortKey, concurrency int) report.SQLiteReportThroughputRow {
	return throughputRowWithProfileID(runID, "profile-1", profile, contextLabel, contextSortKey, concurrency)
}

func throughputRowWithProfileID(runID, profileID, profile, contextLabel string, contextSortKey, concurrency int) report.SQLiteReportThroughputRow {
	return report.SQLiteReportThroughputRow{
		Mode:              "decode",
		RunID:             runID,
		MeasurementID:     int64(concurrency),
		ProfileID:         profileID,
		Profile:           profile,
		Model:             "test/model",
		ContextWindow:     32768,
		ContextLabel:      contextLabel,
		ContextSortKey:    contextSortKey,
		Concurrency:       concurrency,
		Shape:             contextLabel,
		ThroughputTokS:    "100",
		PerUserTokS:       "100",
		CompletedRequests: concurrency,
		Detail: report.SQLiteReportCellDetail{
			Available:     true,
			Mode:          "decode",
			RunID:         runID,
			MeasurementID: int64(concurrency),
			Profile:       profile,
			ContextLabel:  contextLabel,
			Shape:         contextLabel,
		},
	}
}

func TestSkippedPointStaysInDeclaredContextTable(t *testing.T) {
	verified := throughputRow("run-1", "16k", "16k active context", 16384, 1)
	verified.ContextTarget = 16384
	verified.ContextSemantics = "active"
	verified.ContextVerified = true
	verified.Status = "completed"
	skipped := throughputRow("run-1", "16k", "unverified (declared 16k active)", 16384, 32)
	skipped.ContextTarget = 16384
	skipped.ContextSemantics = "active"
	skipped.Status = "skipped"
	skipped.FailureLabel = "skipped"
	skipped.FailureReason = "concurrency 32 exceeds 2.0x reported max"
	doc := report.SQLiteReportDocument{
		GeneratedAt:    time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
		ThroughputRows: []report.SQLiteReportThroughputRow{verified, skipped},
	}
	tables := Build("/tmp/report.sqlite", doc).Throughput.Tables
	if len(tables) != 1 {
		t.Fatalf("tables = %d, want the skipped point inside its declared context table", len(tables))
	}
	table := tables[0]
	if table.ContextLabel != "16k active context" || table.ContextStatus != "active_verified" {
		t.Fatalf("table = %q/%q, want verified 16k table", table.ContextLabel, table.ContextStatus)
	}
	if table.Warning != "" {
		t.Fatalf("warning = %q, want none: the skip is a row-level outcome", table.Warning)
	}
	if len(table.Rows) != 2 {
		t.Fatalf("rows = %d, want both concurrency points", len(table.Rows))
	}
	last := table.Rows[1]
	if last.Decode.FailureLabel != "skipped" || last.Decode.FailureReason == "" {
		t.Fatalf("skipped cell = %+v, want failure label and reason inline", last.Decode)
	}
}

func TestAllSkippedTableGetsNotRunWarning(t *testing.T) {
	skipped := throughputRow("run-1", "32k", "unverified (declared 32k active)", 32768, 16)
	skipped.ContextTarget = 32768
	skipped.ContextSemantics = "active"
	skipped.Status = "skipped"
	skipped.FailureLabel = "skipped"
	doc := report.SQLiteReportDocument{
		GeneratedAt:    time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
		ThroughputRows: []report.SQLiteReportThroughputRow{skipped},
	}
	table := Build("/tmp/report.sqlite", doc).Throughput.Tables[0]
	if table.ContextStatus != "unverified" {
		t.Fatalf("status = %q, want unverified", table.ContextStatus)
	}
	if !strings.Contains(table.Warning, "Not run") {
		t.Fatalf("warning = %q, want a not-run explanation instead of a token-count complaint", table.Warning)
	}
}

func TestPhaseMetricsCarryDisplayStrings(t *testing.T) {
	row := throughputRow("run-1", "8k", "8k active context", 8192, 1)
	row.ThroughputTokS = "878.846"
	row.PerUserTokS = "878.846"
	row.TTFTMeanMS = "102175.763"
	row.TTFTP99MS = "110000.5"
	doc := report.SQLiteReportDocument{
		GeneratedAt:    time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
		ThroughputRows: []report.SQLiteReportThroughputRow{row},
	}
	metrics := Build("/tmp/report.sqlite", doc).Throughput.Tables[0].Rows[0].Decode
	if metrics.TokS != "878.846" || metrics.TokSDisplay != "879" {
		t.Fatalf("tok_s raw/display = %q/%q, want raw preserved and display rounded", metrics.TokS, metrics.TokSDisplay)
	}
	if metrics.TTFTMeanDisplay != "1m42s" {
		t.Fatalf("ttft display = %q, want unit-promoted 1m42s", metrics.TTFTMeanDisplay)
	}
}

func TestCompletedUnverifiedRowDemotesTable(t *testing.T) {
	verified := throughputRow("run-1", "16k", "16k active context", 16384, 1)
	verified.ContextTarget = 16384
	verified.ContextSemantics = "active"
	verified.ContextVerified = true
	verified.Status = "completed"
	unverified := throughputRow("run-1", "16k", "unverified (declared 16k active)", 16384, 4)
	unverified.ContextTarget = 16384
	unverified.ContextSemantics = "active"
	unverified.Status = "completed"
	doc := report.SQLiteReportDocument{
		GeneratedAt:    time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
		ThroughputRows: []report.SQLiteReportThroughputRow{verified, unverified},
	}
	table := Build("/tmp/report.sqlite", doc).Throughput.Tables[0]
	if table.ContextStatus != "unverified" {
		t.Fatalf("status = %q, want unverified: a completed row without token counts must demote the table", table.ContextStatus)
	}
	if !strings.Contains(table.ContextLabel, "unverified") {
		t.Fatalf("label = %q, want unverified label", table.ContextLabel)
	}
	if !strings.Contains(table.Warning, "not confirmed by completed token counts") {
		t.Fatalf("warning = %q, want the completed-but-unverifiable warning", table.Warning)
	}
}
