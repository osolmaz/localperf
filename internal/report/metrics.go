package report

import (
	"database/sql"
	"fmt"
	"math"
	"sort"
	"strings"
	"time"
)

// MetricDef defines one reported quantity exactly once: the report legend
// and the computation both come from this registry, so the definition shown
// to the reader cannot drift from what is rendered.
type MetricDef struct {
	Key        string
	Label      string
	Unit       string
	Weighting  string
	Definition string
}

var ReportMetrics = []MetricDef{
	{
		Key: "decode_tok_s", Label: "decode tok/s", Unit: "tok/s", Weighting: "aggregate",
		Definition: "generated output tokens divided by full run wall time.",
	},
	{
		Key: "prefill_tok_s", Label: "prefill tok/s", Unit: "tok/s", Weighting: "aggregate",
		Definition: "input prompt tokens divided by full run wall time; a workload-level approximation, not engine kernel time.",
	},
	{
		Key: "per_user_tok_s", Label: "/user", Unit: "tok/s", Weighting: "per-request",
		Definition: "row throughput divided by concurrent users.",
	},
	{
		Key: "rps", Label: "RPS", Unit: "req/s", Weighting: "aggregate",
		Definition: "completed requests divided by full run wall time.",
	},
	{
		Key: "ttft", Label: "TTFT", Unit: "ms", Weighting: "per-request",
		Definition: "time to first token; mean, p50, p95, and p99 over completed requests. Measured only on streamed responses: runs without streamed samples show \"-\" instead of a first-byte approximation.",
	},
	{
		Key: "latency", Label: "latency", Unit: "ms", Weighting: "per-request",
		Definition: "end-to-end request latency (E2EL); p50, p95, and p99 over completed requests.",
	},
	{
		Key: "tpot", Label: "TPOT", Unit: "ms", Weighting: "per-request",
		Definition: "request-weighted mean of per-request time per output token; every request counts equally, describing per-user experience.",
	},
	{
		Key: "itl_token_weighted", Label: "ITL (tok-wt)", Unit: "ms", Weighting: "per-token",
		Definition: "token-weighted mean inter-token gap: sum of all gaps divided by total gaps across requests; longer responses weigh more, describing steady-state system behavior. Not interchangeable with TPOT.",
	},
	{
		Key: "achieved_concurrency", Label: "achieved concurrency", Unit: "requests", Weighting: "aggregate",
		Definition: "time-weighted mean in-flight requests (total request busy time / measurement span); shown when it diverges from the requested concurrency by more than 10%.",
	},
	{
		Key: "ok_err", Label: "OK / Err", Unit: "requests", Weighting: "aggregate",
		Definition: "completed requests / failed requests, with failures broken down by error type.",
	},
	{
		Key: "goodput", Label: "goodput", Unit: "req/s", Weighting: "aggregate",
		Definition: "requests per second that met every declared SLO target (see the % in SLO column); rendered only when the workload declares an slo block. High throughput with low goodput means requests completed too slowly to be useful.",
	},
	{
		Key: "gpu_util", Label: "GPU util a/p", Unit: "percent", Weighting: "aggregate",
		Definition: "average / peak GPU utilization sampled every 2s during the measurement, with the telemetry source named; on unified-memory systems cross-check GPU memory against system memory drop.",
	},
	{
		Key: "gpu_mem_peak", Label: "GPU mem peak", Unit: "GiB", Weighting: "aggregate",
		Definition: "peak sampled GPU memory used during the measurement.",
	},
	{
		Key: "prefix_cache", Label: "prefix cache", Unit: "", Weighting: "",
		Definition: "whether engine prefix caching was enabled (on / off / unknown). When on, prefill throughput can be partly served from cache and must be read accordingly.",
	},
	{
		Key: "repeats", Label: "± spread", Unit: "", Weighting: "across repeats",
		Definition: "when a point ran multiple repeats, values render as mean ± sample standard deviation across repeats and per-repeat rows move to the Repeats section.",
	},
	{
		Key: "context_labels", Label: "context labels", Unit: "tokens", Weighting: "",
		Definition: "declared active-context targets confirmed by measured tokens, or the measured request shape. \"Server limit\" is the engine's max_model_len; it is capacity, not the context a workload exercised.",
	},
}

// sqliteRequestDerived carries per-measurement metrics derived from the
// requests table at render time. Absent when the measurement was recorded
// without detailed request rows; the fallback is "-", never a substitute
// number.
// GPUUtilStat is one telemetry source's utilization aggregate for a
// measurement.
type GPUUtilStat struct {
	Avg  float64
	Peak float64
}

type sqliteRequestDerived struct {
	ITLTokenWeightedMS   float64
	ITLTokenWeightedOK   bool
	AchievedConcurrency  float64
	AchievedConcurrencyK bool
	FailureCounts        map[string]int
	GPUUtilBySource      map[string]GPUUtilStat
	GPUMemPeakBySource   map[string]float64
}

func loadSQLiteReportRequestDerived(db *sql.DB, doc *SQLiteReportDocument) error {
	doc.RequestDerived = map[int64]sqliteRequestDerived{}
	if err := loadTokenWeightedITL(db, doc); err != nil {
		return err
	}
	if err := loadAchievedConcurrency(db, doc); err != nil {
		return err
	}
	if err := loadGPUTelemetryStats(db, doc); err != nil {
		return err
	}
	return loadFailureBreakdown(db, doc)
}

// loadGPUTelemetryStats aggregates sampled GPU telemetry per measurement;
// the source (tegrastats, nvidia-smi, nvml) is kept so readers can judge the
// signal, which matters on unified-memory systems.
func loadGPUTelemetryStats(db *sql.DB, doc *SQLiteReportDocument) error {
	rows, err := db.Query(`SELECT ts.measurement_id, s.metric, s.source, AVG(ts.value), MAX(ts.value)
		FROM telemetry_samples ts
		JOIN telemetry_series s ON s.id = ts.series_id
		WHERE ts.measurement_id IS NOT NULL
		  AND s.metric IN ('gpu_utilization_percent', 'gpu_memory_used_bytes')
		GROUP BY ts.measurement_id, s.metric, s.source
		ORDER BY s.source`)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var measurementID int64
		var metric, source string
		var avg, peak float64
		if err := rows.Scan(&measurementID, &metric, &source, &avg, &peak); err != nil {
			return err
		}
		derived := doc.RequestDerived[measurementID]
		switch metric {
		case "gpu_utilization_percent":
			if derived.GPUUtilBySource == nil {
				derived.GPUUtilBySource = map[string]GPUUtilStat{}
			}
			derived.GPUUtilBySource[source] = GPUUtilStat{Avg: avg, Peak: peak}
		case "gpu_memory_used_bytes":
			if derived.GPUMemPeakBySource == nil {
				derived.GPUMemPeakBySource = map[string]float64{}
			}
			derived.GPUMemPeakBySource[source] = peak
		}
		doc.RequestDerived[measurementID] = derived
	}
	return rows.Err()
}

// loadTokenWeightedITL computes sum(all gaps)/count(all gaps) exactly from
// per-request means: itl_mean_ms * (completion_tokens - 1) recovers each
// request's gap sum.
func loadTokenWeightedITL(db *sql.DB, doc *SQLiteReportDocument) error {
	rows, err := db.Query(`SELECT measurement_id,
		SUM(CASE WHEN completion_tokens > 1 AND itl_mean_ms IS NOT NULL THEN itl_mean_ms * (completion_tokens - 1) END),
		SUM(CASE WHEN completion_tokens > 1 AND itl_mean_ms IS NOT NULL THEN completion_tokens - 1 END)
		FROM requests WHERE status = 'completed' GROUP BY measurement_id`)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var measurementID int64
		var gapSum, gapCount sql.NullFloat64
		if err := rows.Scan(&measurementID, &gapSum, &gapCount); err != nil {
			return err
		}
		if !gapSum.Valid || !gapCount.Valid || gapCount.Float64 <= 0 {
			continue
		}
		derived := doc.RequestDerived[measurementID]
		derived.ITLTokenWeightedMS = gapSum.Float64 / gapCount.Float64
		derived.ITLTokenWeightedOK = true
		doc.RequestDerived[measurementID] = derived
	}
	return rows.Err()
}

// loadAchievedConcurrency derives the time-weighted mean number of in-flight
// requests: the integral of in-flight count over the measurement span equals
// the sum of request durations, so achieved = sum(durations) / span. Failed
// requests occupied a slot while running, so every timed request counts.
func loadAchievedConcurrency(db *sql.DB, doc *SQLiteReportDocument) error {
	rows, err := db.Query(`SELECT measurement_id, started_at, completed_at
		FROM requests
		WHERE started_at IS NOT NULL AND completed_at IS NOT NULL`)
	if err != nil {
		return err
	}
	defer rows.Close()
	type window struct {
		busy       time.Duration
		start, end time.Time
		count      int
	}
	windows := map[int64]*window{}
	for rows.Next() {
		var measurementID int64
		var startedAt, completedAt string
		if err := rows.Scan(&measurementID, &startedAt, &completedAt); err != nil {
			return err
		}
		started, err := parseRequestTime(startedAt)
		if err != nil {
			continue
		}
		completed, err := parseRequestTime(completedAt)
		if err != nil || completed.Before(started) {
			continue
		}
		current := windows[measurementID]
		if current == nil {
			current = &window{start: started, end: completed}
			windows[measurementID] = current
		}
		if started.Before(current.start) {
			current.start = started
		}
		if completed.After(current.end) {
			current.end = completed
		}
		current.busy += completed.Sub(started)
		current.count++
	}
	if err := rows.Err(); err != nil {
		return err
	}
	for measurementID, current := range windows {
		span := current.end.Sub(current.start)
		if span <= 0 || current.count == 0 {
			continue
		}
		derived := doc.RequestDerived[measurementID]
		derived.AchievedConcurrency = float64(current.busy) / float64(span)
		derived.AchievedConcurrencyK = true
		doc.RequestDerived[measurementID] = derived
	}
	return nil
}

func parseRequestTime(value string) (time.Time, error) {
	if parsed, err := time.Parse(time.RFC3339Nano, value); err == nil {
		return parsed, nil
	}
	return time.Parse("2006-01-02 15:04:05.999999999-07:00", value)
}

func loadFailureBreakdown(db *sql.DB, doc *SQLiteReportDocument) error {
	rows, err := db.Query(`SELECT measurement_id, COALESCE(NULLIF(error_type, ''), 'unknown'), COUNT(*)
		FROM requests WHERE status != 'completed' GROUP BY 1, 2`)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var measurementID int64
		var errorType string
		var count int
		if err := rows.Scan(&measurementID, &errorType, &count); err != nil {
			return err
		}
		derived := doc.RequestDerived[measurementID]
		if derived.FailureCounts == nil {
			derived.FailureCounts = map[string]int{}
		}
		derived.FailureCounts[errorType] += count
		doc.RequestDerived[measurementID] = derived
	}
	return rows.Err()
}

// loadSLOGoodput derives goodput for measurements whose workload declared an
// SLO: the fraction of completed requests meeting every set target, and
// SLO-met requests per second. Runs after measurements load; requires
// detailed request rows.
func loadSLOGoodput(db *sql.DB, doc *SQLiteReportDocument) error {
	for index := range doc.Measurements {
		measurement := &doc.Measurements[index]
		if measurement.SLOTTFTMillis <= 0 && measurement.SLOE2ELMillis <= 0 {
			continue
		}
		doc.HasSLO = true
		var total, met sql.NullInt64
		err := db.QueryRow(`SELECT COUNT(*),
			SUM(CASE WHEN (? <= 0 OR (ttft_ms IS NOT NULL AND ttft_ms <= ?))
			          AND (? <= 0 OR (latency_ms IS NOT NULL AND latency_ms <= ?)) THEN 1 ELSE 0 END)
			FROM requests WHERE measurement_id = ? AND status = 'completed'`,
			measurement.SLOTTFTMillis, measurement.SLOTTFTMillis,
			measurement.SLOE2ELMillis, measurement.SLOE2ELMillis,
			measurement.ID).Scan(&total, &met)
		if err != nil {
			return err
		}
		if total.Valid && met.Valid {
			measurement.SLORequestCount = total.Int64
			measurement.SLOMetCount = met.Int64
		}
		formatSLODisplays(measurement)
	}
	return nil
}

func sloNote(ttftMillis, e2elMillis float64) string {
	parts := []string{}
	if ttftMillis > 0 {
		parts = append(parts, fmt.Sprintf("ttft<=%.0fms", ttftMillis))
	}
	if e2elMillis > 0 {
		parts = append(parts, fmt.Sprintf("e2el<=%.0fms", e2elMillis))
	}
	return strings.Join(parts, ", ")
}

func applyRequestDerived(measurement *SQLiteReportMeasurement, derived sqliteRequestDerived) {
	if derived.ITLTokenWeightedOK {
		measurement.ITLTokenWeightedMS = displayFloat(derived.ITLTokenWeightedMS)
	} else {
		measurement.ITLTokenWeightedMS = "-"
	}
	measurement.AchievedValue = derived.AchievedConcurrency
	measurement.AchievedKnown = derived.AchievedConcurrencyK
	measurement.FailureCounts = derived.FailureCounts
	measurement.GPUUtilBySource = derived.GPUUtilBySource
	measurement.GPUMemPeakBySource = derived.GPUMemPeakBySource
	formatDerivedDisplays(measurement)
}

// formatDerivedDisplays renders the display strings for derived fields from
// their numeric values; repeat aggregation recomputes the values and calls
// this again so aggregated rows never show a single repeat's data.
func formatDerivedDisplays(measurement *SQLiteReportMeasurement) {
	measurement.AchievedConcurrency = ""
	if measurement.AchievedKnown && measurement.Concurrency > 0 {
		requested := float64(measurement.Concurrency)
		if math.Abs(measurement.AchievedValue-requested)/requested > 0.10 {
			measurement.AchievedConcurrency = fmt.Sprintf("~%.0f (of %d)", measurement.AchievedValue, measurement.Concurrency)
		}
	}
	measurement.FailureBreakdown = formatFailureCounts(measurement.FailureCounts)
	if measurement.WallTimeMSKnown && measurement.WallTimeMSValue > 0 && measurement.CompletedRequests > 0 {
		measurement.RPS = displayFloat(float64(measurement.CompletedRequests) / (measurement.WallTimeMSValue / 1000))
	} else {
		measurement.RPS = "-"
	}
	// Every source renders separately: hiding one signal behind another is
	// how disagreements between them go unnoticed.
	measurement.GPUUtil = "-"
	if len(measurement.GPUUtilBySource) > 0 {
		parts := make([]string, 0, len(measurement.GPUUtilBySource))
		for _, source := range sortedKeys(measurement.GPUUtilBySource) {
			stat := measurement.GPUUtilBySource[source]
			parts = append(parts, fmt.Sprintf("%.0f / %.0f%% (%s)", stat.Avg, stat.Peak, source))
		}
		measurement.GPUUtil = strings.Join(parts, "; ")
	}
	measurement.GPUMemPeak = "-"
	if len(measurement.GPUMemPeakBySource) > 0 {
		parts := make([]string, 0, len(measurement.GPUMemPeakBySource))
		for _, source := range sortedKeys(measurement.GPUMemPeakBySource) {
			parts = append(parts, fmt.Sprintf("%.1f GiB (%s)", measurement.GPUMemPeakBySource[source]/(1024*1024*1024), source))
		}
		measurement.GPUMemPeak = strings.Join(parts, "; ")
	}
	formatSLODisplays(measurement)
}

func sortedKeys[V any](values map[string]V) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

func formatSLODisplays(measurement *SQLiteReportMeasurement) {
	if measurement.SLOTTFTMillis <= 0 && measurement.SLOE2ELMillis <= 0 {
		return
	}
	measurement.SLONote = sloNote(measurement.SLOTTFTMillis, measurement.SLOE2ELMillis)
	measurement.SLOMetPct = "-"
	measurement.GoodputRPS = "-"
	// A TTFT target is only evaluable against streamed samples; without the
	// marker the run has no TTFT to compare, not a 0% goodput.
	if measurement.SLOTTFTMillis > 0 && measurement.TTFTSource != "stream" {
		measurement.SLONote += " (ttft target requires streamed samples)"
		return
	}
	if measurement.SLORequestCount > 0 {
		measurement.SLOMetPct = fmt.Sprintf("%.0f%%", 100*float64(measurement.SLOMetCount)/float64(measurement.SLORequestCount))
		if measurement.WallTimeMSKnown && measurement.WallTimeMSValue > 0 {
			measurement.GoodputRPS = displayFloat(float64(measurement.SLOMetCount) / (measurement.WallTimeMSValue / 1000))
		}
	}
}

func formatFailureCounts(counts map[string]int) string {
	if len(counts) == 0 {
		return ""
	}
	types := make([]string, 0, len(counts))
	for errorType := range counts {
		types = append(types, errorType)
	}
	sort.Slice(types, func(i, j int) bool {
		if counts[types[i]] != counts[types[j]] {
			return counts[types[i]] > counts[types[j]]
		}
		return types[i] < types[j]
	})
	parts := make([]string, 0, len(types))
	for _, errorType := range types {
		parts = append(parts, fmt.Sprintf("%d %s", counts[errorType], errorType))
	}
	return strings.Join(parts, ", ")
}

// aggregateRepeatMeasurements collapses repeats of the same
// (profile, workload, phase, concurrency) point into one primary row with
// mean ± sample stddev displays; per-repeat rows are returned separately for
// the secondary Repeats section. Comparisons without variance present noise
// as signal.
func aggregateRepeatMeasurements(measurements []SQLiteReportMeasurement) (aggregated, repeats []SQLiteReportMeasurement) {
	type groupKey struct {
		profile, workload, phase string
		concurrency              int
	}
	groups := map[groupKey][]SQLiteReportMeasurement{}
	order := []groupKey{}
	for _, measurement := range measurements {
		key := groupKey{measurement.Profile, measurement.Workload, measurement.Phase, measurement.Concurrency}
		if _, seen := groups[key]; !seen {
			order = append(order, key)
		}
		groups[key] = append(groups[key], measurement)
	}
	for _, key := range order {
		members := groups[key]
		if len(members) == 1 {
			aggregated = append(aggregated, members[0])
			continue
		}
		aggregated = append(aggregated, combineRepeats(members))
		repeats = append(repeats, members...)
	}
	return aggregated, repeats
}

func combineRepeats(members []SQLiteReportMeasurement) SQLiteReportMeasurement {
	combined := members[0]
	combined.RepeatCount = len(members)
	combined.CompletedRequests = 0
	combined.FailedRequests = 0
	// Sum token totals and wall time along with request counts so
	// per-request derivations (requestShape, inputThroughput) divide summed
	// numerators by summed denominators instead of mixing the first
	// repeat's totals with all repeats' request counts.
	combined.PromptTokensValue = 0
	combined.CompletionTokensValue = 0
	combined.TotalTokensValue = 0
	combined.WallTimeMSValue = 0
	for _, member := range members {
		combined.CompletedRequests += member.CompletedRequests
		combined.FailedRequests += member.FailedRequests
		combined.PromptTokensKnown = combined.PromptTokensKnown && member.PromptTokensKnown
		combined.CompletionTokensKnown = combined.CompletionTokensKnown && member.CompletionTokensKnown
		combined.TotalTokensKnown = combined.TotalTokensKnown && member.TotalTokensKnown
		combined.WallTimeMSKnown = combined.WallTimeMSKnown && member.WallTimeMSKnown
		combined.PromptTokensValue += member.PromptTokensValue
		combined.CompletionTokensValue += member.CompletionTokensValue
		combined.TotalTokensValue += member.TotalTokensValue
		combined.WallTimeMSValue += member.WallTimeMSValue
		if statusRank(member.Status) > statusRank(combined.Status) {
			combined.Status = member.Status
		}
	}
	combined.PromptTokens = displayInt64Known(combined.PromptTokensValue, combined.PromptTokensKnown)
	combined.CompletionTokens = displayInt64Known(combined.CompletionTokensValue, combined.CompletionTokensKnown)
	combined.TotalTokens = displayInt64Known(combined.TotalTokensValue, combined.TotalTokensKnown)
	if combined.WallTimeMSKnown {
		combined.WallTimeMS = displayFloat(combined.WallTimeMSValue)
	} else {
		combined.WallTimeMS = "-"
	}
	combineDerivedValues(&combined, members)
	mean, known := meanOverRepeats(members, func(m SQLiteReportMeasurement) (float64, bool) {
		return m.OutputTokSValue, m.OutputTokSKnown
	})
	if known {
		combined.OutputTokSValue = mean
	}
	fields := []struct {
		get func(SQLiteReportMeasurement) string
		set func(*SQLiteReportMeasurement, string)
	}{
		{func(m SQLiteReportMeasurement) string { return m.OutputTokS }, func(m *SQLiteReportMeasurement, v string) { m.OutputTokS = v }},
		{func(m SQLiteReportMeasurement) string { return m.TotalTokS }, func(m *SQLiteReportMeasurement, v string) { m.TotalTokS = v }},
		{func(m SQLiteReportMeasurement) string { return m.PerUserOutputTokS }, func(m *SQLiteReportMeasurement, v string) { m.PerUserOutputTokS = v }},
		{func(m SQLiteReportMeasurement) string { return m.TTFTMeanMS }, func(m *SQLiteReportMeasurement, v string) { m.TTFTMeanMS = v }},
		{func(m SQLiteReportMeasurement) string { return m.TTFTP50MS }, func(m *SQLiteReportMeasurement, v string) { m.TTFTP50MS = v }},
		{func(m SQLiteReportMeasurement) string { return m.TTFTP95MS }, func(m *SQLiteReportMeasurement, v string) { m.TTFTP95MS = v }},
		{func(m SQLiteReportMeasurement) string { return m.TTFTP99MS }, func(m *SQLiteReportMeasurement, v string) { m.TTFTP99MS = v }},
		{func(m SQLiteReportMeasurement) string { return m.LatencyP50MS }, func(m *SQLiteReportMeasurement, v string) { m.LatencyP50MS = v }},
		{func(m SQLiteReportMeasurement) string { return m.LatencyP95MS }, func(m *SQLiteReportMeasurement, v string) { m.LatencyP95MS = v }},
		{func(m SQLiteReportMeasurement) string { return m.LatencyP99MS }, func(m *SQLiteReportMeasurement, v string) { m.LatencyP99MS = v }},
		{func(m SQLiteReportMeasurement) string { return m.TPOTMeanMS }, func(m *SQLiteReportMeasurement, v string) { m.TPOTMeanMS = v }},
		{func(m SQLiteReportMeasurement) string { return m.ITLMeanMS }, func(m *SQLiteReportMeasurement, v string) { m.ITLMeanMS = v }},
		{func(m SQLiteReportMeasurement) string { return m.ITLTokenWeightedMS }, func(m *SQLiteReportMeasurement, v string) { m.ITLTokenWeightedMS = v }},
	}
	for _, field := range fields {
		field.set(&combined, meanSpreadDisplay(members, field.get))
	}
	combined.TTFTSource = combinedTTFTSource(members)
	combined.InputTokSSpread = meanSpreadDisplay(members, func(m SQLiteReportMeasurement) string {
		return inputThroughput(m)
	})
	combined.InputPerUserSpread = meanSpreadDisplay(members, func(m SQLiteReportMeasurement) string {
		return perUserMetric(inputThroughput(m), m.Concurrency)
	})
	// Re-derive the context label from the combined token totals: the
	// aggregate must not inherit repeat 0's verification when other repeats
	// failed or measured outside the band.
	combined.ContextVerified = false
	combined.ContextMismatch = false
	combined.ContextMismatchNote = ""
	combined.ActiveRange = ""
	applyContextLabel(&combined)
	// Averaged totals can land back inside the band even when a repeat
	// contradicted the claim; a verified aggregate requires every repeat to
	// verify on its own.
	if combined.ContextVerified {
		unverified := 0
		for _, member := range members {
			if !member.ContextVerified {
				unverified++
			}
		}
		if unverified > 0 {
			combined.ContextVerified = false
			combined.ContextMismatch = true
			combined.ContextLabel = measuredShapeLabel(combined)
			combined.ContextMismatchNote = fmt.Sprintf(
				"declared %s active, but %d of %d repeats did not verify",
				contextLabel(combined.ContextTarget), unverified, len(members))
		}
	}
	return combined
}

// combineDerivedValues recomputes derived numeric fields across all repeats
// before displays are formatted: an aggregated row must never carry a single
// repeat's goodput, GPU, achieved-concurrency, or failure data.
func combineDerivedValues(combined *SQLiteReportMeasurement, members []SQLiteReportMeasurement) {
	combined.AchievedValue, combined.AchievedKnown = meanOverRepeats(members, func(m SQLiteReportMeasurement) (float64, bool) {
		return m.AchievedValue, m.AchievedKnown
	})
	combined.SLOMetCount = 0
	combined.SLORequestCount = 0
	combined.FailureCounts = map[string]int{}
	combined.GPUUtilBySource = map[string]GPUUtilStat{}
	combined.GPUMemPeakBySource = map[string]float64{}
	utilCounts := map[string]int{}
	for _, member := range members {
		for source, stat := range member.GPUUtilBySource {
			current := combined.GPUUtilBySource[source]
			current.Avg += stat.Avg
			if stat.Peak > current.Peak {
				current.Peak = stat.Peak
			}
			combined.GPUUtilBySource[source] = current
			utilCounts[source]++
		}
		for source, peak := range member.GPUMemPeakBySource {
			if peak > combined.GPUMemPeakBySource[source] {
				combined.GPUMemPeakBySource[source] = peak
			}
		}
		combined.SLOMetCount += member.SLOMetCount
		combined.SLORequestCount += member.SLORequestCount
		for errorType, count := range member.FailureCounts {
			combined.FailureCounts[errorType] += count
		}
	}
	for source, count := range utilCounts {
		current := combined.GPUUtilBySource[source]
		current.Avg /= float64(count)
		combined.GPUUtilBySource[source] = current
	}
	formatDerivedDisplays(combined)
}

func meanOverRepeats(members []SQLiteReportMeasurement, value func(SQLiteReportMeasurement) (float64, bool)) (float64, bool) {
	sum := 0.0
	count := 0
	for _, member := range members {
		current, known := value(member)
		if !known {
			continue
		}
		sum += current
		count++
	}
	if count == 0 {
		return 0, false
	}
	return sum / float64(count), true
}

// meanSpreadDisplay renders "mean ± stddev" (sample stddev, n-1) across the
// repeats' displayed values; if any repeat has no value, the spread cannot
// be computed honestly and "-" is rendered.
// combinedTTFTSource keeps the streamed marker on an aggregate only when
// every repeat carries it; one unstreamed repeat makes the combined TTFT
// untrusted.
func combinedTTFTSource(members []SQLiteReportMeasurement) string {
	for _, member := range members {
		if member.TTFTSource != "stream" {
			return ""
		}
	}
	if len(members) == 0 {
		return ""
	}
	return "stream"
}

func meanSpreadDisplay(members []SQLiteReportMeasurement, value func(SQLiteReportMeasurement) string) string {
	values := make([]float64, 0, len(members))
	for _, member := range members {
		parsed, ok := parseDisplayedFloat(value(member))
		if !ok {
			return "-"
		}
		values = append(values, parsed)
	}
	mean := 0.0
	for _, v := range values {
		mean += v
	}
	mean /= float64(len(values))
	variance := 0.0
	for _, v := range values {
		variance += (v - mean) * (v - mean)
	}
	stddev := math.Sqrt(variance / float64(len(values)-1))
	return displayFloat(mean) + " ± " + displayFloat(stddev)
}

func displayInt64Known(value int64, known bool) string {
	if !known {
		return "-"
	}
	return fmt.Sprintf("%d", value)
}

func statusRank(status string) int {
	ranks := map[string]int{"completed": 0, "planned": 1, "running": 1, "skipped": 2, "canceled": 3, "failed": 4}
	return ranks[strings.ToLower(strings.TrimSpace(status))]
}
