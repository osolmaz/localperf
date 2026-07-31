package reportmodel

import (
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.com/osolmaz/localperf/internal/report"
)

type Document struct {
	Summary    Summary              `json:"summary"`
	Throughput ThroughputResponse   `json:"throughput"`
	Details    map[int64]CellDetail `json:"-"`
}

type Summary struct {
	ArtifactPath        string             `json:"artifact_path"`
	GeneratedAt         string             `json:"generated_at"`
	Metadata            []MetadataItem     `json:"metadata"`
	LatestRun           RunSummary         `json:"latest_run"`
	Runs                []RunSummary       `json:"runs"`
	Profiles            []ProfileSummary   `json:"profiles"`
	MeasurementCount    int                `json:"measurement_count"`
	Warnings            []ReportWarning    `json:"warnings"`
	ContextStatusCounts map[string]int     `json:"context_status_counts"`
	Legend              []report.MetricDef `json:"legend"`
}

type RunSummary struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Status      string `json:"status"`
	CreatedAt   string `json:"created_at"`
	CompletedAt string `json:"completed_at"`
	Hostname    string `json:"hostname"`
	Hardware    string `json:"hardware"`
}

type ProfileSummary struct {
	ID                  string `json:"id"`
	Name                string `json:"name"`
	Model               string `json:"model"`
	ServerLimit         int    `json:"server_limit"`
	ServerLimitLabel    string `json:"server_limit_label"`
	MaxNumSeqs          int    `json:"max_num_seqs"`
	MaxNumBatchedTokens int    `json:"max_num_batched_tokens"`
	KVCacheDtype        string `json:"kv_cache_dtype"`
	PrefixCaching       string `json:"prefix_caching"`
}

type ReportWarning struct {
	Level   string `json:"level"`
	Message string `json:"message"`
}

type MetadataItem struct {
	Label string `json:"label"`
	Value string `json:"value"`
}

type ThroughputResponse struct {
	Tables []ThroughputTable `json:"tables"`
}

type ThroughputTable struct {
	ID                 string          `json:"id"`
	Title              string          `json:"title"`
	RunID              string          `json:"run_id,omitempty"`
	RunIDs             []string        `json:"run_ids,omitempty"`
	ProfileID          string          `json:"profile_id"`
	Profile            string          `json:"profile"`
	Model              string          `json:"model"`
	ServerLimit        int             `json:"server_limit"`
	ServerLimitLabel   string          `json:"server_limit_label"`
	ContextLabel       string          `json:"context_label,omitempty"`
	ContextStatus      string          `json:"context_status"`
	ContextStatusLabel string          `json:"context_status_label"`
	Warning            string          `json:"warning,omitempty"`
	DecodeShape        string          `json:"decode_shape,omitempty"`
	PrefillShape       string          `json:"prefill_shape,omitempty"`
	ShapeNotes         []string        `json:"shape_notes,omitempty"`
	Rows               []ThroughputRow `json:"rows"`
}

type ThroughputRow struct {
	Concurrency int          `json:"concurrency"`
	Baseline    bool         `json:"baseline"`
	Decode      PhaseMetrics `json:"decode"`
	Prefill     PhaseMetrics `json:"prefill"`
	OK          int          `json:"ok"`
	Err         int          `json:"err"`
	Result      string       `json:"result"`
	SLO         string       `json:"slo"`
}

type PhaseMetrics struct {
	Available          bool   `json:"available"`
	MeasurementID      int64  `json:"measurement_id,omitempty"`
	Workload           string `json:"workload,omitempty"`
	Shape              string `json:"shape,omitempty"`
	Status             string `json:"status,omitempty"`
	TokS               string `json:"tok_s,omitempty"`
	PerUserTokS        string `json:"per_user_tok_s,omitempty"`
	TTFTMeanMS         string `json:"ttft_mean_ms,omitempty"`
	TTFTP99MS          string `json:"ttft_p99_ms,omitempty"`
	TokSDisplay        string `json:"tok_s_display,omitempty"`
	PerUserTokSDisplay string `json:"per_user_tok_s_display,omitempty"`
	TTFTMeanDisplay    string `json:"ttft_mean_display,omitempty"`
	TTFTP99Display     string `json:"ttft_p99_display,omitempty"`
	OK                 int    `json:"ok"`
	Err                int    `json:"err"`
	FailureLabel       string `json:"failure_label,omitempty"`
	FailureReason      string `json:"failure_reason,omitempty"`
	DetailURL          string `json:"detail_url,omitempty"`
	Derived            bool   `json:"derived,omitempty"`
	DerivedSource      string `json:"derived_source,omitempty"`
	DerivedFormula     string `json:"derived_formula,omitempty"`
}

type CellDetail struct {
	Available        bool           `json:"available"`
	Phase            string         `json:"phase"`
	Mode             string         `json:"mode"`
	Status           string         `json:"status"`
	FailureLabel     string         `json:"failure_label,omitempty"`
	FailureReason    string         `json:"failure_reason,omitempty"`
	Source           string         `json:"source,omitempty"`
	RunID            string         `json:"run_id,omitempty"`
	MeasurementID    int64          `json:"measurement_id"`
	Model            string         `json:"model,omitempty"`
	Profile          string         `json:"profile,omitempty"`
	Workload         string         `json:"workload,omitempty"`
	ContextLabel     string         `json:"context_label,omitempty"`
	ContextWindow    int            `json:"context_window,omitempty"`
	Concurrency      int            `json:"concurrency,omitempty"`
	SamplesRequested int            `json:"samples_requested,omitempty"`
	Shape            string         `json:"shape,omitempty"`
	ProfileConfig    []MetadataItem `json:"profile_config,omitempty"`
	Metrics          []MetadataItem `json:"metrics,omitempty"`
	ServeCommand     string         `json:"serve_command,omitempty"`
	BenchmarkCommand string         `json:"benchmark_command,omitempty"`
	EngineArgs       string         `json:"engine_args,omitempty"`
	ServeJSON        string         `json:"serve_json,omitempty"`
	EnvJSON          string         `json:"env_json,omitempty"`
}

type tableBuilder struct {
	table               ThroughputTable
	rows                map[int]*ThroughputRow
	decodeShapes        map[string]struct{}
	prefillShapes       map[string]struct{}
	contextMismatches   []string
	runIDs              map[string]struct{}
	claimKey            string
	claimSemantics      string
	claimTarget         int
	claimFallback       string
	anyVerified         bool
	completedRows       int
	completedUnverified int
}

func Build(path string, doc report.SQLiteReportDocument) Document {
	details := map[int64]CellDetail{}
	tables := buildThroughputTables(doc, details)
	return Document{
		Summary: Summary{
			ArtifactPath:        path,
			GeneratedAt:         doc.GeneratedAt.Format("2006-01-02T15:04:05Z07:00"),
			Metadata:            metadataItems(doc.MetadataItems),
			LatestRun:           runSummary(doc.Run),
			Runs:                runSummaries(doc.Runs),
			Profiles:            profileSummaries(doc.Profiles),
			MeasurementCount:    len(doc.Measurements),
			Warnings:            reportWarnings(tables),
			ContextStatusCounts: contextStatusCounts(tables),
			Legend:              doc.Legend,
		},
		Throughput: ThroughputResponse{Tables: tables},
		Details:    details,
	}
}

func buildThroughputTables(doc report.SQLiteReportDocument, details map[int64]CellDetail) []ThroughputTable {
	builders := []*tableBuilder{}
	for _, row := range doc.ThroughputRows {
		builder := compatibleBuilder(builders, row)
		if builder == nil {
			builder = newTableBuilder(row, len(builders)+1)
			builders = append(builders, builder)
		}
		applyRow(builder, row, details)
	}
	tables := make([]ThroughputTable, 0, len(builders))
	for _, builder := range builders {
		finishTable(builder)
		tables = append(tables, builder.table)
	}
	sort.SliceStable(tables, func(i, j int) bool {
		if tables[i].ServerLimit != tables[j].ServerLimit {
			return tables[i].ServerLimit < tables[j].ServerLimit
		}
		if tables[i].Profile != tables[j].Profile {
			return tables[i].Profile < tables[j].Profile
		}
		return tables[i].ID < tables[j].ID
	})
	return tables
}

func compatibleBuilder(builders []*tableBuilder, row report.SQLiteReportThroughputRow) *tableBuilder {
	for _, builder := range builders {
		if builder.table.Profile != row.Profile ||
			builder.table.Model != row.Model ||
			builder.table.ServerLimit != row.ContextWindow ||
			builder.claimKey != row.ClaimKey() {
			continue
		}
		existing := builder.rows[row.Concurrency]
		if existing == nil {
			return builder
		}
		slot := phaseSlot(existing, row.Mode)
		if !slot.Available || (row.Mode == "prefill" && slot.Derived) {
			return builder
		}
		if phaseSlot(existing, row.Mode).Shape == row.Shape {
			return builder
		}
	}
	return nil
}

func newTableBuilder(row report.SQLiteReportThroughputRow, ordinal int) *tableBuilder {
	title := row.Profile
	if title == "" {
		title = contextLabel(row.ContextWindow)
	}
	return &tableBuilder{
		table: ThroughputTable{
			ID:               fmt.Sprintf("%02d-%s", ordinal, slug(title)),
			Title:            title,
			RunID:            row.RunID,
			ProfileID:        row.ProfileID,
			Profile:          row.Profile,
			Model:            row.Model,
			ServerLimit:      row.ContextWindow,
			ServerLimitLabel: contextLabel(row.ContextWindow),
			ContextLabel:     contextGroupLabel(row),
		},
		rows:           map[int]*ThroughputRow{},
		decodeShapes:   map[string]struct{}{},
		prefillShapes:  map[string]struct{}{},
		runIDs:         map[string]struct{}{row.RunID: {}},
		claimKey:       row.ClaimKey(),
		claimSemantics: row.ContextSemantics,
		claimTarget:    row.ContextTarget,
		claimFallback:  contextGroupLabel(row),
	}
}

func applyRow(builder *tableBuilder, source report.SQLiteReportThroughputRow, details map[int64]CellDetail) {
	target := builder.rows[source.Concurrency]
	if target == nil {
		target = &ThroughputRow{
			Concurrency: source.Concurrency,
			Baseline:    source.Concurrency == 1,
			Result:      "0 / 0",
			SLO:         "-",
		}
		builder.rows[source.Concurrency] = target
	}
	metrics := phaseMetrics(source)
	useSourceSLO := true
	switch source.Mode {
	case "prefill":
		if !target.Prefill.Derived || phaseHasUsableMetric(metrics) {
			target.Prefill = metrics
			if source.Shape != "" && source.Shape != "-" {
				builder.prefillShapes[source.Shape] = struct{}{}
			}
		} else {
			useSourceSLO = false
		}
	case "decode":
		target.Decode = metrics
		if source.Shape != "" && source.Shape != "-" {
			builder.decodeShapes[source.Shape] = struct{}{}
		}
		applyDerivedPrefillMetrics(target, source)
		if target.Prefill.Derived {
			target.SLO = withoutPhaseSLO(target.SLO, "P")
		}
	default:
		target.Decode = metrics
		applyDerivedPrefillMetrics(target, source)
		if target.Prefill.Derived {
			target.SLO = withoutPhaseSLO(target.SLO, "P")
		}
	}
	target.OK = target.Decode.OK
	target.Err = target.Decode.Err
	if !target.Prefill.Derived {
		target.OK += target.Prefill.OK
		target.Err += target.Prefill.Err
	}
	target.Result = phaseResult(target)
	if useSourceSLO {
		target.SLO = phaseSLO(source, target.SLO)
	}
	if source.ContextMismatch && source.MismatchNote != "" {
		builder.contextMismatches = append(builder.contextMismatches, source.MismatchNote)
	}
	if source.ContextVerified {
		builder.anyVerified = true
	}
	if strings.EqualFold(strings.TrimSpace(source.Status), "completed") {
		builder.completedRows++
		if !source.ContextVerified && !source.ContextMismatch {
			builder.completedUnverified++
		}
	}
	builder.runIDs[source.RunID] = struct{}{}
	if detail := cellDetail(source.Detail); detail.Available {
		details[source.MeasurementID] = detail
	}
}

func applyDerivedPrefillMetrics(target *ThroughputRow, source report.SQLiteReportThroughputRow) {
	value := strings.TrimSpace(source.EffectivePrefillTokS)
	if !strings.EqualFold(strings.TrimSpace(source.Status), "completed") || source.FailureLabel != "" ||
		value == "" || value == "-" || (!target.Prefill.Derived && phaseHasUsableMetric(target.Prefill)) {
		return
	}
	target.Prefill = PhaseMetrics{
		Available:          true,
		MeasurementID:      source.MeasurementID,
		Workload:           source.Workload,
		Shape:              source.Shape,
		Status:             source.Status,
		TokS:               source.EffectivePrefillTokS,
		PerUserTokS:        source.EffectivePrefillPerUserTokS,
		TTFTMeanMS:         source.TTFTMeanMS,
		TTFTP99MS:          source.TTFTP99MS,
		TokSDisplay:        report.FormatRateDisplay(source.EffectivePrefillTokS),
		PerUserTokSDisplay: report.FormatRateDisplay(source.EffectivePrefillPerUserTokS),
		TTFTMeanDisplay:    report.FormatDurationDisplay(source.TTFTMeanMS),
		TTFTP99Display:     report.FormatDurationDisplay(source.TTFTP99MS),
		OK:                 source.CompletedRequests,
		Err:                source.FailedRequests,
		FailureLabel:       source.FailureLabel,
		FailureReason:      source.FailureReason,
		DetailURL:          fmt.Sprintf("measurements/%d", source.MeasurementID),
		Derived:            true,
		DerivedSource:      "generation-derived from streamed TTFT",
		DerivedFormula:     "sum(prompt tokens) / (latest first token - earliest request start)",
	}
}

func phaseHasUsableMetric(phase PhaseMetrics) bool {
	value := strings.TrimSpace(strings.SplitN(phase.TokS, "±", 2)[0])
	if !phase.Available || phase.FailureLabel != "" || value == "" || value == "-" {
		return false
	}
	_, err := strconv.ParseFloat(value, 64)
	return err == nil
}

func phaseSlot(row *ThroughputRow, mode string) PhaseMetrics {
	if mode == "prefill" {
		return row.Prefill
	}
	return row.Decode
}

func phaseMetrics(source report.SQLiteReportThroughputRow) PhaseMetrics {
	return PhaseMetrics{
		Available:          true,
		MeasurementID:      source.MeasurementID,
		Workload:           source.Workload,
		Shape:              source.Shape,
		Status:             source.Status,
		TokS:               source.ThroughputTokS,
		PerUserTokS:        source.PerUserTokS,
		TTFTMeanMS:         source.TTFTMeanMS,
		TTFTP99MS:          source.TTFTP99MS,
		TokSDisplay:        report.FormatRateDisplay(source.ThroughputTokS),
		PerUserTokSDisplay: report.FormatRateDisplay(source.PerUserTokS),
		TTFTMeanDisplay:    report.FormatDurationDisplay(source.TTFTMeanMS),
		TTFTP99Display:     report.FormatDurationDisplay(source.TTFTP99MS),
		OK:                 source.CompletedRequests,
		Err:                source.FailedRequests,
		FailureLabel:       source.FailureLabel,
		FailureReason:      source.FailureReason,
		DetailURL:          fmt.Sprintf("measurements/%d", source.MeasurementID),
	}
}

func finishTable(builder *tableBuilder) {
	builder.table.RunIDs = sortedMapKeys(builder.runIDs)
	if len(builder.table.RunIDs) != 1 {
		builder.table.RunID = ""
	}
	builder.decodeShapes = map[string]struct{}{}
	builder.prefillShapes = map[string]struct{}{}
	for _, row := range builder.rows {
		if row.Decode.Available && row.Decode.Shape != "" && row.Decode.Shape != "-" {
			builder.decodeShapes[row.Decode.Shape] = struct{}{}
		}
		if row.Prefill.Available && row.Prefill.Shape != "" && row.Prefill.Shape != "-" {
			builder.prefillShapes[row.Prefill.Shape] = struct{}{}
		}
	}
	builder.table.DecodeShape = shapeSummary(builder.decodeShapes)
	builder.table.PrefillShape = shapeSummary(builder.prefillShapes)
	builder.table.ContextLabel = report.ClaimTitle(builder.claimSemantics, builder.claimTarget, builder.anyVerified && builder.completedUnverified == 0, builder.claimFallback)
	builder.table.ContextStatus, builder.table.ContextStatusLabel = tableContextStatus(builder)
	builder.table.Warning = tableWarning(builder.table.ContextStatus, builder.completedRows, builder.contextMismatches)
	for _, row := range builder.rows {
		builder.table.Rows = append(builder.table.Rows, *row)
	}
	sort.SliceStable(builder.table.Rows, func(i, j int) bool {
		return builder.table.Rows[i].Concurrency < builder.table.Rows[j].Concurrency
	})
	if builder.table.DecodeShape == "" && builder.table.PrefillShape == "" {
		builder.table.ShapeNotes = []string{"No completed token shape was recorded for this table."}
	}
}

// tableContextStatus derives a table's status from its declared claim and
// what the completed rows verified — a skipped or failed row never changes
// the claim, it just renders as a row-level outcome.
func tableContextStatus(builder *tableBuilder) (string, string) {
	switch {
	case len(builder.contextMismatches) > 0:
		return "context_mismatch", "Context mismatch"
	case builder.claimSemantics == "active" && builder.claimTarget > 0:
		// Verified requires every completed row to verify: one completed
		// row without confirmed token counts must not hide under a
		// verified label.
		if builder.anyVerified && builder.completedUnverified == 0 {
			return "active_verified", "Active verified"
		}
		return "unverified", "Unverified"
	case builder.claimSemantics == "capacity" && builder.claimTarget > 0:
		return "capacity", "Capacity"
	default:
		return "unverified", "Unverified"
	}
}

func tableWarning(status string, completedRows int, mismatches []string) string {
	if len(mismatches) > 0 {
		return "Context mismatch: " + strings.Join(uniqueStrings(mismatches), "; ")
	}
	switch status {
	case "capacity":
		return "Capacity point: this table is labeled by server limit, not by active request context."
	case "unverified":
		if completedRows == 0 {
			return "Not run: every point in this table was skipped or failed before it measured anything."
		}
		return "Unverified: declared active context was not confirmed by completed token counts."
	default:
		return ""
	}
}

func phaseResult(row *ThroughputRow) string {
	switch {
	case row.Decode.Available && row.Prefill.Available && !row.Prefill.Derived:
		return fmt.Sprintf("D %d/%d; P %d/%d", row.Decode.OK, row.Decode.Err, row.Prefill.OK, row.Prefill.Err)
	case row.Decode.Available && row.Prefill.Derived:
		return fmt.Sprintf("%d / %d", row.Decode.OK, row.Decode.Err)
	case row.Decode.Available:
		return fmt.Sprintf("D %d/%d", row.Decode.OK, row.Decode.Err)
	case row.Prefill.Available:
		return fmt.Sprintf("P %d/%d", row.Prefill.OK, row.Prefill.Err)
	default:
		return "0 / 0"
	}
}

func phaseSLO(source report.SQLiteReportThroughputRow, existing string) string {
	if source.SLODisplay == "" {
		if existing == "" {
			return "-"
		}
		return existing
	}
	prefix := "D"
	if source.Mode == "prefill" {
		prefix = "P"
	}
	if existing == "" || existing == "-" {
		return prefix + " " + source.SLODisplay
	}
	if strings.Contains(existing, prefix+" ") {
		return existing
	}
	return existing + "; " + prefix + " " + source.SLODisplay
}

func withoutPhaseSLO(existing, prefix string) string {
	kept := make([]string, 0, 2)
	for part := range strings.SplitSeq(existing, ";") {
		part = strings.TrimSpace(part)
		if part != "" && part != "-" && !strings.HasPrefix(part, prefix+" ") {
			kept = append(kept, part)
		}
	}
	if len(kept) == 0 {
		return "-"
	}
	return strings.Join(kept, "; ")
}

func cellDetail(detail report.SQLiteReportCellDetail) CellDetail {
	return CellDetail{
		Available:        detail.Available,
		Phase:            detail.Phase,
		Mode:             detail.Mode,
		Status:           detail.Status,
		FailureLabel:     detail.FailureLabel,
		FailureReason:    detail.FailureReason,
		Source:           detail.Source,
		RunID:            detail.RunID,
		MeasurementID:    detail.MeasurementID,
		Model:            detail.Model,
		Profile:          detail.Profile,
		Workload:         detail.Workload,
		ContextLabel:     detail.ContextLabel,
		ContextWindow:    detail.ContextWindow,
		Concurrency:      detail.Concurrency,
		SamplesRequested: detail.SamplesRequested,
		Shape:            detail.Shape,
		ProfileConfig:    metadataItems(detail.ProfileConfig),
		Metrics:          metadataItems(detail.Metrics),
		ServeCommand:     detail.ServeCommand,
		BenchmarkCommand: detail.BenchmarkCommand,
		EngineArgs:       detail.EngineArgs,
		ServeJSON:        detail.ServeJSON,
		EnvJSON:          detail.EnvJSON,
	}
}

func runSummary(run report.SQLiteReportRun) RunSummary {
	return RunSummary{
		ID:          run.ID,
		Name:        run.Name,
		Status:      run.Status,
		CreatedAt:   run.CreatedAt,
		CompletedAt: run.CompletedAt,
		Hostname:    run.Hostname,
		Hardware:    run.Hardware,
	}
}

func runSummaries(runs []report.SQLiteReportRun) []RunSummary {
	out := make([]RunSummary, 0, len(runs))
	for _, run := range runs {
		out = append(out, runSummary(run))
	}
	return out
}

func profileSummaries(profiles []report.SQLiteReportProfile) []ProfileSummary {
	out := make([]ProfileSummary, 0, len(profiles))
	for _, profile := range profiles {
		out = append(out, ProfileSummary{
			ID:                  profile.ID,
			Name:                profile.Name,
			Model:               profile.Model,
			ServerLimit:         profile.ContextWindow,
			ServerLimitLabel:    contextLabel(profile.ContextWindow),
			MaxNumSeqs:          profile.MaxNumSeqs,
			MaxNumBatchedTokens: profile.MaxNumBatchedTokens,
			KVCacheDtype:        profile.KVCacheDtype,
			PrefixCaching:       profile.PrefixCaching,
		})
	}
	return out
}

func metadataItems(items []report.SQLiteReportMetadataItem) []MetadataItem {
	out := make([]MetadataItem, 0, len(items))
	for _, item := range items {
		out = append(out, MetadataItem{Label: item.Label, Value: item.Value})
	}
	return out
}

func reportWarnings(tables []ThroughputTable) []ReportWarning {
	seen := map[string]struct{}{}
	warnings := []ReportWarning{}
	for _, table := range tables {
		if table.Warning == "" {
			continue
		}
		if _, ok := seen[table.Warning]; ok {
			continue
		}
		seen[table.Warning] = struct{}{}
		warnings = append(warnings, ReportWarning{Level: "warning", Message: table.Warning})
	}
	return warnings
}

func contextStatusCounts(tables []ThroughputTable) map[string]int {
	counts := map[string]int{}
	for _, table := range tables {
		counts[table.ContextStatus]++
	}
	return counts
}

func shapeSummary(values map[string]struct{}) string {
	shapes := make([]string, 0, len(values))
	for value := range values {
		if strings.TrimSpace(value) != "" && value != "-" {
			shapes = append(shapes, value)
		}
	}
	sort.Strings(shapes)
	return strings.Join(shapes, ", ")
}

func uniqueStrings(values []string) []string {
	seen := map[string]struct{}{}
	out := []string{}
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value == "" {
			continue
		}
		if _, ok := seen[value]; ok {
			continue
		}
		seen[value] = struct{}{}
		out = append(out, value)
	}
	return out
}

func sortedMapKeys(values map[string]struct{}) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		if strings.TrimSpace(key) != "" {
			keys = append(keys, key)
		}
	}
	sort.Strings(keys)
	return keys
}

func contextGroupLabel(row report.SQLiteReportThroughputRow) string {
	if label := strings.TrimSpace(row.ContextLabel); label != "" {
		return label
	}
	return "unverified"
}

func contextLabel(tokens int) string {
	if tokens <= 0 {
		return "-"
	}
	if tokens%1024 == 0 {
		return strconv.Itoa(tokens/1024) + "k"
	}
	return strconv.Itoa(tokens)
}

func slug(value string) string {
	value = strings.ToLower(strings.TrimSpace(value))
	var builder strings.Builder
	lastDash := false
	for _, r := range value {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			builder.WriteRune(r)
			lastDash = false
		case !lastDash:
			builder.WriteByte('-')
			lastDash = true
		}
	}
	out := strings.Trim(builder.String(), "-")
	if out == "" {
		return "table"
	}
	return out
}
