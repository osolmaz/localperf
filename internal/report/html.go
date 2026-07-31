package report

import (
	"database/sql"
	"embed"
	"encoding/json"
	"fmt"
	"html/template"
	"io"
	"math"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/osolmaz/localperf/internal/artifact"
	"github.com/osolmaz/localperf/internal/bench"
	"github.com/osolmaz/localperf/internal/collections"
)

const htmlReportName = "report.html"

type HTMLReportOptions struct {
	Title      string
	IncludeRaw bool
	Store      bool
}

type SQLiteReportDocument struct {
	ArtifactPath       string
	GeneratedAt        time.Time
	Metadata           map[string]string
	Run                SQLiteReportRun
	Runs               []SQLiteReportRun
	Engines            []SQLiteReportEngine
	Profiles           []SQLiteReportProfile
	Workloads          []SQLiteReportWorkload
	Measurements       []SQLiteReportMeasurement
	MetadataItems      []SQLiteReportMetadataItem
	ThroughputRows     []SQLiteReportThroughputRow
	ThroughputGroups   []SQLiteReportThroughputGroup
	FullRunTimingRows  []SQLiteReportFullRunTimingRow
	PhaseSections      []SQLiteReportPhaseSection
	Charts             []SQLiteReportChart
	RequestSummary     SQLiteReportRequestSummary
	EventCounts        []SQLiteReportCount
	NotableEvents      []SQLiteReportEvent
	Commands           []SQLiteReportCommand
	SpecProvenance     string
	SpecGenerator      *artifact.GeneratorStamp
	SpecConcurrency    []int
	ExistingReports    []SQLiteReportExport
	ArtifactSummaries  []SQLiteReportArtifactSummary
	MeasurementMetrics map[int64]map[string]SQLiteReportMetric
	RequestDerived     map[int64]sqliteRequestDerived
	RepeatDetails      []SQLiteReportMeasurement
	Legend             []MetricDef
	HasSLO             bool
}

type SQLiteReportRun struct {
	ID          string
	Name        string
	Description string
	Status      string
	CreatedAt   string
	StartedAt   string
	CompletedAt string
	Hostname    string
	Username    string
	CWD         string
	GitCommit   string
	Hardware    string
}

type SQLiteReportEngine struct {
	ID              string
	Name            string
	Type            string
	Managed         bool
	Command         string
	Version         string
	GitCommit       string
	EndpointBaseURL string
	// ServedModelsByProfile lists every model the server reported per
	// probed profile, from the engine identity stored under
	// metadata_json.identity. Multi-model servers report all of them.
	ServedModelsByProfile map[string][]string
}

type SQLiteReportProfile struct {
	ID                    string
	Name                  string
	Engine                string
	Model                 string
	EndpointBaseURL       string
	ContextWindow         int
	MaxNumSeqs            int
	MaxNumBatchedTokens   int
	GPUMemoryUtilization  float64
	GPUMemoryUtilizationS string
	Managed               bool
	EnableSleepMode       bool
	KVCacheDtype          string
	PrefixCaching         string
	ServeJSON             string
	EngineArgsJSON        string
	EnvJSON               string
}

type SQLiteReportWorkload struct {
	ID                      string
	Name                    string
	Role                    string
	Phase                   string
	Samples                 int
	Repeats                 int
	SaveDetailed            bool
	CapturePayloadArtifacts bool
}

type SQLiteReportMeasurement struct {
	ID                          int64
	RunID                       string
	ProfileID                   string
	Profile                     string
	Model                       string
	WorkloadID                  string
	Workload                    string
	Phase                       string
	ContextWindow               int
	ContextTarget               int
	ContextSemantics            string
	ContextLabel                string
	ContextSortKey              int
	ContextMismatch             bool
	ContextMismatchNote         string
	ActiveRange                 string
	RepeatIndex                 int
	Concurrency                 int
	SamplesRequested            int
	Status                      string
	StartedAt                   string
	CompletedAt                 string
	WallTimeMS                  string
	WallTimeMSValue             float64
	WallTimeMSKnown             bool
	CompletedRequests           int
	FailedRequests              int
	PromptTokens                string
	PromptTokensValue           int64
	PromptTokensKnown           bool
	CompletionTokens            string
	CompletionTokensValue       int64
	CompletionTokensKnown       bool
	TotalTokens                 string
	TotalTokensValue            int64
	TotalTokensKnown            bool
	OutputTokS                  string
	OutputTokSValue             float64
	OutputTokSKnown             bool
	OutputTokSStdDev            string
	PerUserOutputTokS           string
	TotalTokS                   string
	InputTokSSpread             string
	InputPerUserSpread          string
	RPS                         string
	LatencyMeanMS               string
	LatencyP50MS                string
	LatencyP95MS                string
	LatencyP99MS                string
	TTFTMeanMS                  string
	TTFTP50MS                   string
	TTFTP95MS                   string
	TTFTP99MS                   string
	TPOTMeanMS                  string
	DecodePerUserTokS           string
	EffectivePrefillTokS        string
	RequestEffectivePrefillTokS string
	ITLMeanMS                   string
	ITLTokenWeightedMS          string
	AchievedConcurrency         string
	AchievedValue               float64
	AchievedKnown               bool
	FailureBreakdown            string
	FailureCounts               map[string]int
	GPUUtil                     string
	GPUUtilBySource             map[string]GPUUtilStat
	GPUMemPeak                  string
	GPUMemPeakBySource          map[string]float64
	SLOTTFTMillis               float64
	SLOE2ELMillis               float64
	SLONote                     string
	SLOMetPct                   string
	SLOMetCount                 int64
	SLORequestCount             int64
	GoodputRPS                  string
	RepeatCount                 int
	ContextVerified             bool
	TTFTSource                  string
	ErrorType                   string
	ErrorMessage                string
}

type SQLiteReportMetadataItem struct {
	Label string
	Value string
}

type SQLiteReportThroughputRow struct {
	Phase             string
	Mode              string
	RunID             string
	MeasurementID     int64
	ProfileID         string
	Profile           string
	Model             string
	WorkloadID        string
	Workload          string
	ContextWindow     int
	ContextLabel      string
	ContextSortKey    int
	ContextMismatch   bool
	MismatchNote      string
	ContextTarget     int
	ContextSemantics  string
	ContextVerified   bool
	ActiveRange       string
	Concurrency       int
	Shape             string
	InputTokS         string
	TotalTokS         string
	OutputTokS        string
	PerUserOutputTokS string
	ThroughputTokS    string
	PerUserTokS       string
	TTFTMeanMS        string
	TTFTP99MS         string
	LatencyP95MS      string
	SLODisplay        string
	CompletedRequests int
	FailedRequests    int
	Status            string
	FailureLabel      string
	FailureReason     string
	Detail            SQLiteReportCellDetail
	InputHeat         string
	TotalHeat         string
	OutputHeat        string
	PerUserHeat       string
	ThroughputHeat    string
	PerUserTokSHeat   string
	TTFTHeat          string
	LatencyHeat       string
	FailureHeat       string
	Baseline          bool
}

type SQLiteReportThroughputGroup struct {
	Title          string
	Profile        string
	ContextSortKey int
	ServerLimit    int
	AxisItems      []SQLiteReportMetadataItem
	Rows           []SQLiteReportThroughputComparisonRow
}

type SQLiteReportFullRunTimingRow struct {
	MeasurementID               int64
	Profile                     string
	Workload                    string
	ContextLabel                string
	ContextSortKey              int
	Concurrency                 int
	RepeatCount                 int
	OutputTokS                  string
	EffectivePrefillTokS        string
	RequestEffectivePrefillTokS string
	TTFTMeanMS                  string
	TTFTP99MS                   string
	DecodePerUserTokS           string
	LatencyP95MS                string
	CompletedRequests           int
	FailedRequests              int
	Detail                      SQLiteReportCellDetail
}

type SQLiteReportThroughputComparisonRow struct {
	Concurrency         int
	Baseline            bool
	DecodeTokS          string
	DecodePerUserTokS   string
	DecodeTTFTMeanMS    string
	DecodeTTFTMS        string
	DecodeLatencyMS     string
	DecodeOK            int
	DecodeErr           int
	DecodeShape         string
	DecodeDetail        SQLiteReportCellDetail
	PrefillTokS         string
	PrefillPerUserTokS  string
	PrefillTTFTMeanMS   string
	PrefillTTFTMS       string
	PrefillLatencyMS    string
	PrefillOK           int
	PrefillErr          int
	PrefillShape        string
	PrefillDetail       SQLiteReportCellDetail
	OK                  int
	Err                 int
	Requests            string
	Result              string
	ResultDetail        SQLiteReportCellDetail
	SLO                 string
	DecodeSLO           string
	PrefillSLO          string
	DecodeTokSHeat      string
	DecodeUserHeat      string
	DecodeTTFTMeanHeat  string
	DecodeTTFTHeat      string
	DecodeLatencyHeat   string
	PrefillTokSHeat     string
	PrefillUserHeat     string
	PrefillTTFTMeanHeat string
	PrefillTTFTHeat     string
	PrefillLatencyHeat  string
	ErrHeat             string
}

type SQLiteReportCellDetail struct {
	Available        bool
	Phase            string
	Mode             string
	Status           string
	FailureLabel     string
	FailureReason    string
	Source           string
	RunID            string
	MeasurementID    int64
	Model            string
	Profile          string
	Workload         string
	ContextLabel     string
	ContextWindow    int
	Concurrency      int
	SamplesRequested int
	Shape            string
	ProfileConfig    []SQLiteReportMetadataItem
	Metrics          []SQLiteReportMetadataItem
	ServeCommand     string
	BenchmarkCommand string
	EngineArgs       string
	ServeJSON        string
	EnvJSON          string
}

type SQLiteReportMetric struct {
	MeasurementID int64
	Metric        string
	Unit          string
	Mean          string
	StdDev        string
	Min           string
	P50           string
	P90           string
	P95           string
	P99           string
	Max           string
	Count         int
	MeanValue     float64
	StdDevValue   float64
	MeanKnown     bool
	StdDevKnown   bool
}

type SQLiteReportPhaseSection struct {
	Phase        string
	Title        string
	Measurements []SQLiteReportMeasurement
}

type SQLiteReportChart struct {
	Title  string
	Unit   string
	Height int
	Bars   []SQLiteReportBar
}

type SQLiteReportBar struct {
	Label  string
	Value  string
	LabelY int
	RectY  int
	Width  string
}

type SQLiteReportRequestSummary struct {
	Total          int
	Completed      int
	Failed         int
	Canceled       int
	LatencyMeanMS  string
	TTFTMeanMS     string
	TPOTMeanMS     string
	ITLMeanMS      string
	OutputTokSMean string
}

type SQLiteReportCount struct {
	Name  string
	Count int
}

type SQLiteReportEvent struct {
	Timestamp string
	Level     string
	Type      string
	Profile   string
	Workload  string
	Message   string
}

type SQLiteReportCommand struct {
	Phase         string
	Status        string
	ExitCode      string
	StartedAt     string
	Completed     string
	Argv          string
	ProfileID     string
	MeasurementID int64
}

type SQLiteReportExport struct {
	Name      string
	Format    string
	MediaType string
	CreatedAt string
}

type SQLiteReportArtifactSummary struct {
	Kind                  string
	Count                 int
	UncompressedSizeBytes int64
}

func LoadSQLiteReport(path string) (SQLiteReportDocument, error) {
	if err := artifact.Check(path); err != nil {
		return SQLiteReportDocument{}, err
	}
	db, err := artifact.OpenReadOnly(path)
	if err != nil {
		return SQLiteReportDocument{}, err
	}
	defer db.Close()
	doc := SQLiteReportDocument{
		ArtifactPath:       path,
		GeneratedAt:        time.Now().UTC(),
		Metadata:           map[string]string{},
		MeasurementMetrics: map[int64]map[string]SQLiteReportMetric{},
	}
	for _, load := range []func(*sql.DB, *SQLiteReportDocument) error{
		loadSQLiteReportMetadata,
		loadSQLiteReportRun,
		loadSQLiteReportEngines,
		loadSQLiteReportProfiles,
		loadSQLiteReportWorkloads,
		loadSQLiteReportMetrics,
		loadSQLiteReportRequestDerived,
		loadSQLiteReportMeasurements,
		loadSLOGoodput,
		loadSQLiteReportRequestSummary,
		loadSQLiteReportEventCounts,
		loadSQLiteReportNotableEvents,
		loadSQLiteReportCommands,
		loadSQLiteReportSpecProvenance,
		loadSQLiteReportExports,
		loadSQLiteReportArtifactSummaries,
	} {
		if err := load(db, &doc); err != nil {
			return SQLiteReportDocument{}, err
		}
	}
	doc.Measurements, doc.RepeatDetails = aggregateRepeatMeasurements(doc.Measurements)
	doc.Legend = ReportMetrics
	doc.MetadataItems = sqliteReportMetadataItems(doc)
	realRows := sqliteReportThroughputRows(doc)
	doc.ThroughputRows = append(realRows, trimmedThroughputRows(doc, realRows)...)
	doc.ThroughputGroups = sqliteReportThroughputGroups(doc.ThroughputRows)
	doc.FullRunTimingRows = sqliteReportFullRunTimingRows(doc)
	doc.PhaseSections = sqliteReportPhaseSections(doc.Measurements)
	doc.Charts = sqliteReportCharts(doc.Measurements)
	return doc, nil
}

func RenderHTMLReport(writer io.Writer, doc SQLiteReportDocument, opts HTMLReportOptions) error {
	title := strings.TrimSpace(opts.Title)
	if title == "" {
		title = bench.FirstNonEmpty(doc.Run.Name, "localperf report")
	}
	view := struct {
		Title string
		Doc   SQLiteReportDocument
		Raw   bool
	}{
		Title: title,
		Doc:   doc,
		Raw:   opts.IncludeRaw,
	}
	return reportTemplates().ExecuteTemplate(writer, "report.gohtml", view)
}

//go:embed templates
var reportTemplateFS embed.FS

// reportTemplates parses the embedded report templates once. The HTML and
// CSS live under templates/ as real files (syntax highlighting, sane diffs)
// while staying compiled into the binary, so rendered reports remain
// standalone with no runtime file dependencies.
var reportTemplates = sync.OnceValue(func() *template.Template {
	return template.Must(template.New("report").Funcs(template.FuncMap{
		"statusClass":  reportStatusClass,
		"contextLabel": contextLabel,
		"tokps":        FormatRateDisplay,
		"seconds":      FormatDurationDisplay,
	}).ParseFS(reportTemplateFS, "templates/report.gohtml", "templates/report.css"))
})

func WriteSQLiteHTMLReport(artifactPath, outputPath string, opts HTMLReportOptions) error {
	resolvedOutputPath, err := htmlReportOutputPath(artifactPath, outputPath)
	if err != nil {
		return err
	}
	doc, err := LoadSQLiteReport(artifactPath)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(resolvedOutputPath), 0o755); err != nil {
		return err
	}
	var builder strings.Builder
	if err := RenderHTMLReport(&builder, doc, opts); err != nil {
		return err
	}
	content := []byte(builder.String())
	if err := os.WriteFile(resolvedOutputPath, content, 0o644); err != nil {
		return err
	}
	if opts.Store {
		return StoreSQLiteHTMLReport(artifactPath, htmlReportName, resolvedOutputPath, content)
	}
	return nil
}

func htmlReportOutputPath(artifactPath, outputPath string) (string, error) {
	if strings.TrimSpace(outputPath) == "" {
		outputPath = defaultHTMLReportPath(artifactPath)
	}
	if err := rejectSourceArtifactOutput(artifactPath, outputPath); err != nil {
		return "", err
	}
	return outputPath, nil
}

func rejectSourceArtifactOutput(artifactPath, outputPath string) error {
	sameFile, err := sameSQLiteArtifactOutput(artifactPath, outputPath)
	if err != nil {
		return err
	}
	if sameFile {
		return fmt.Errorf("HTML output path must differ from SQLite artifact path: %s", outputPath)
	}
	return nil
}

func sameSQLiteArtifactOutput(artifactPath, outputPath string) (bool, error) {
	artifactAbs, err := filepath.Abs(artifactPath)
	if err != nil {
		return false, err
	}
	outputAbs, err := filepath.Abs(outputPath)
	if err != nil {
		return false, err
	}
	if filepath.Clean(artifactAbs) == filepath.Clean(outputAbs) {
		return true, nil
	}
	return sameExistingFile(artifactAbs, outputAbs)
}

func sameExistingFile(firstPath, secondPath string) (bool, error) {
	firstInfo, err := os.Stat(firstPath)
	if err != nil {
		return false, err
	}
	secondInfo, err := os.Stat(secondPath)
	if err != nil {
		if os.IsNotExist(err) {
			return false, nil
		}
		return false, err
	}
	return os.SameFile(firstInfo, secondInfo), nil
}

func StoreSQLiteHTMLReport(artifactPath, name, originalPath string, content []byte) error {
	if strings.TrimSpace(name) == "" {
		name = htmlReportName
	}
	return artifact.StoreReport(artifactPath, name, "text/html", originalPath, content)
}

func defaultHTMLReportPath(artifactPath string) string {
	ext := filepath.Ext(artifactPath)
	if ext == "" {
		return artifactPath + ".html"
	}
	return strings.TrimSuffix(artifactPath, ext) + ".html"
}

func loadSQLiteReportMetadata(db *sql.DB, doc *SQLiteReportDocument) error {
	rows, err := db.Query("SELECT key, value FROM metadata ORDER BY key")
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var key, value string
		if err := rows.Scan(&key, &value); err != nil {
			return err
		}
		doc.Metadata[key] = value
	}
	return rows.Err()
}

// loadSQLiteReportRun loads every run in the artifact: model-level artifacts
// accumulate one run per benchmark attempt or batch. The most recent run is
// the report header; all runs render in the Runs section.
func loadSQLiteReportRun(db *sql.DB, doc *SQLiteReportDocument) error {
	rows, err := db.Query(`SELECT
		id, name, description, status, created_at, started_at, completed_at,
		hostname, username, cwd, localperf_git_commit, host_json
		FROM run ORDER BY created_at DESC, id DESC`)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var run SQLiteReportRun
		var description, startedAt, completedAt, hostname, username, cwd, gitCommit, hostJSON sql.NullString
		if err := rows.Scan(
			&run.ID, &run.Name, &description, &run.Status, &run.CreatedAt,
			&startedAt, &completedAt, &hostname, &username, &cwd, &gitCommit, &hostJSON,
		); err != nil {
			return err
		}
		run.Description = nullStringValue(description)
		run.StartedAt = nullStringValue(startedAt)
		run.CompletedAt = nullStringValue(completedAt)
		run.Hostname = nullStringValue(hostname)
		run.Username = nullStringValue(username)
		run.CWD = nullStringValue(cwd)
		run.GitCommit = nullStringValue(gitCommit)
		run.Hardware = hardwareSummary(nullStringValue(hostJSON))
		doc.Runs = append(doc.Runs, run)
	}
	if err := rows.Err(); err != nil {
		return err
	}
	if len(doc.Runs) == 0 {
		return fmt.Errorf("artifact contains no run rows")
	}
	doc.Run = doc.Runs[0]
	return nil
}

// hardwareSummary renders the host_json hardware inventory, for example
// "NVIDIA GB10 (119 GiB, driver 580.95) / 273 GiB RAM". Absent inventory
// renders "-": missing data is information.
func hardwareSummary(hostJSON string) string {
	var host struct {
		CPU    string  `json:"cpu"`
		RAMGiB float64 `json:"ram_gib"`
		GPUs   []struct {
			Name    string  `json:"name"`
			VRAMGiB float64 `json:"vram_gib"`
			Driver  string  `json:"driver"`
		} `json:"gpus"`
	}
	if err := json.Unmarshal([]byte(hostJSON), &host); err != nil {
		return "-"
	}
	parts := []string{}
	for _, gpu := range host.GPUs {
		detail := []string{}
		if gpu.VRAMGiB > 0 {
			detail = append(detail, fmt.Sprintf("%.0f GiB", gpu.VRAMGiB))
		}
		if gpu.Driver != "" {
			detail = append(detail, "driver "+gpu.Driver)
		}
		if len(detail) > 0 {
			parts = append(parts, fmt.Sprintf("%s (%s)", gpu.Name, strings.Join(detail, ", ")))
			continue
		}
		parts = append(parts, gpu.Name)
	}
	if host.RAMGiB > 0 {
		parts = append(parts, fmt.Sprintf("%.0f GiB RAM", host.RAMGiB))
	}
	if len(parts) == 0 {
		return "-"
	}
	return strings.Join(parts, " / ")
}

func loadSQLiteReportEngines(db *sql.DB, doc *SQLiteReportDocument) error {
	rows, err := db.Query(`SELECT
		id, name, type, managed, COALESCE(command, ''), COALESCE(version, ''),
		COALESCE(git_commit, ''), COALESCE(endpoint_base_url, ''),
		COALESCE(metadata_json, '')
		FROM engines ORDER BY name`)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var engine SQLiteReportEngine
		var managed int
		var metadataJSON string
		if err := rows.Scan(
			&engine.ID, &engine.Name, &engine.Type, &managed, &engine.Command, &engine.Version,
			&engine.GitCommit, &engine.EndpointBaseURL, &metadataJSON,
		); err != nil {
			return err
		}
		engine.Managed = managed != 0
		engine.ServedModelsByProfile = servedModelsByProfile(metadataJSON)
		doc.Engines = append(doc.Engines, engine)
	}
	return rows.Err()
}

func servedModelsByProfile(metadataJSON string) map[string][]string {
	var metadata struct {
		Identity map[string]struct {
			Models struct {
				Data []struct {
					ID string `json:"id"`
				} `json:"data"`
			} `json:"models"`
		} `json:"identity"`
	}
	if err := json.Unmarshal([]byte(metadataJSON), &metadata); err != nil {
		return nil
	}
	served := map[string][]string{}
	for profile, identity := range metadata.Identity {
		for _, model := range identity.Models.Data {
			if model.ID != "" {
				served[profile] = append(served[profile], model.ID)
			}
		}
	}
	return served
}

func loadSQLiteReportProfiles(db *sql.DB, doc *SQLiteReportDocument) error {
	rows, err := db.Query(`SELECT
		id, name, COALESCE(engine_id, ''), model, COALESCE(endpoint_base_url, ''),
		COALESCE(context_window, 0), COALESCE(max_num_seqs, 0),
		COALESCE(max_num_batched_tokens, 0), COALESCE(gpu_memory_utilization, 0),
		managed, COALESCE(enable_sleep_mode, 0),
		COALESCE(json_extract(serve_json, '$.kv_cache_dtype'), ''),
		json_extract(serve_json, '$.enable_prefix_caching'),
		COALESCE(serve_json, ''), COALESCE(engine_args_json, ''), COALESCE(env_json, '')
		FROM profiles ORDER BY context_window, name`)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var profile SQLiteReportProfile
		var managed, sleep int
		var prefixCaching sql.NullInt64
		if err := rows.Scan(
			&profile.ID, &profile.Name, &profile.Engine, &profile.Model, &profile.EndpointBaseURL,
			&profile.ContextWindow,
			&profile.MaxNumSeqs, &profile.MaxNumBatchedTokens, &profile.GPUMemoryUtilization,
			&managed, &sleep, &profile.KVCacheDtype, &prefixCaching,
			&profile.ServeJSON, &profile.EngineArgsJSON, &profile.EnvJSON,
		); err != nil {
			return err
		}
		profile.GPUMemoryUtilizationS = displayFloat(profile.GPUMemoryUtilization)
		profile.Managed = managed != 0
		profile.EnableSleepMode = sleep != 0
		// Tri-state: prefix caching changes how prefill numbers must be
		// read, and "unknown" (unmanaged engines) must stay visible.
		switch {
		case !prefixCaching.Valid:
			profile.PrefixCaching = "unknown"
		case prefixCaching.Int64 != 0:
			profile.PrefixCaching = "on"
		default:
			profile.PrefixCaching = "off"
		}
		doc.Profiles = append(doc.Profiles, profile)
	}
	return rows.Err()
}

func loadSQLiteReportWorkloads(db *sql.DB, doc *SQLiteReportDocument) error {
	rows, err := db.Query(`SELECT
		id, name, role, phase, samples, repeats, save_detailed, capture_payload_artifacts
		FROM workloads WHERE role = 'benchmark' ORDER BY phase, name`)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var workload SQLiteReportWorkload
		var saveDetailed, capture int
		if err := rows.Scan(&workload.ID, &workload.Name, &workload.Role, &workload.Phase, &workload.Samples, &workload.Repeats, &saveDetailed, &capture); err != nil {
			return err
		}
		workload.SaveDetailed = saveDetailed != 0
		workload.CapturePayloadArtifacts = capture != 0
		doc.Workloads = append(doc.Workloads, workload)
	}
	return rows.Err()
}

func loadSQLiteReportMeasurements(db *sql.DB, doc *SQLiteReportDocument) error {
	rows, err := db.Query(`SELECT
		m.id, m.run_id, p.id, p.name, p.model, w.id, w.name, w.phase, COALESCE(p.context_window, 0),
		COALESCE(json_extract(w.metadata_json, '$.context.target'), 0),
		COALESCE(json_extract(w.metadata_json, '$.context.semantics'), ''),
		COALESCE(json_extract(w.metadata_json, '$.slo.ttft_p95_ms'), 0),
		COALESCE(json_extract(w.metadata_json, '$.slo.e2el_p95_ms'), 0),
		COALESCE(json_extract(m.metadata_json, '$.ttft_source'), ''),
		m.repeat_index,
		m.concurrency, m.samples_requested, m.status, m.started_at, m.completed_at,
		m.wall_time_ms, m.completed_requests, m.failed_requests, m.prompt_tokens,
		m.completion_tokens, m.total_tokens, m.aggregate_output_tok_s,
		m.per_user_output_tok_s, m.aggregate_total_tok_s, m.error_type, m.error_message
		FROM measurements m
		JOIN profiles p ON p.id = m.profile_id
		JOIN workloads w ON w.id = m.workload_id
		WHERE w.role = 'benchmark'
		ORDER BY w.phase, COALESCE(p.context_window, 0), p.name, w.name, m.concurrency, m.repeat_index`)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var measurement SQLiteReportMeasurement
		var startedAt, completedAt, errorType, errorMessage sql.NullString
		var wallTime, outputTokS, perUserTokS, totalTokS sql.NullFloat64
		var promptTokens, completionTokens, totalTokens sql.NullInt64
		if err := rows.Scan(
			&measurement.ID, &measurement.RunID, &measurement.ProfileID, &measurement.Profile, &measurement.Model,
			&measurement.WorkloadID, &measurement.Workload, &measurement.Phase,
			&measurement.ContextWindow, &measurement.ContextTarget, &measurement.ContextSemantics,
			&measurement.SLOTTFTMillis, &measurement.SLOE2ELMillis,
			&measurement.TTFTSource,
			&measurement.RepeatIndex, &measurement.Concurrency,
			&measurement.SamplesRequested, &measurement.Status, &startedAt, &completedAt,
			&wallTime, &measurement.CompletedRequests, &measurement.FailedRequests,
			&promptTokens, &completionTokens, &totalTokens, &outputTokS, &perUserTokS,
			&totalTokS, &errorType, &errorMessage,
		); err != nil {
			return err
		}
		applySQLiteMeasurementDisplay(&measurement, doc.MeasurementMetrics[measurement.ID], startedAt, completedAt, wallTime, promptTokens, completionTokens, totalTokens, outputTokS, perUserTokS, totalTokS, errorType, errorMessage)
		applyContextLabel(&measurement)
		applyRequestDerived(&measurement, doc.RequestDerived[measurement.ID])
		doc.Measurements = append(doc.Measurements, measurement)
	}
	return rows.Err()
}

// longOutputTokenThreshold separates prefill-style rows (start and end
// active context coincide) from rows whose active context grows materially
// during decode and must display the measured range.
const longOutputTokenThreshold = 8

// applyContextLabel implements the labeling rules in
// docs/2026-07-02-context-semantics.md: a context label may only come from a
// declared target confirmed by measurement; contradicted or undeclared rows
// are labeled by their measured shape, and the server limit is never a
// context label.
func applyContextLabel(measurement *SQLiteReportMeasurement) {
	activeStart, activeEnd, measured := measuredActiveContext(*measurement)
	measurement.ContextSortKey = int(activeEnd)
	if measurement.ContextTarget > 0 {
		measurement.ContextSortKey = measurement.ContextTarget
	}
	longOutput := measured && (activeEnd-activeStart) > longOutputTokenThreshold
	switch {
	case measurement.ContextSemantics == "active" && measurement.ContextTarget > 0:
		switch {
		case measured && withinContextBand(activeEnd, measurement.ContextTarget):
			measurement.ContextVerified = true
			measurement.ContextLabel = contextLabel(measurement.ContextTarget) + " active context"
			if longOutput {
				measurement.ActiveRange = activeRangeLabel(activeStart, activeEnd)
			}
		case measured:
			measurement.ContextLabel = measuredShapeLabel(*measurement)
			measurement.ContextMismatch = true
			measurement.ContextMismatchNote = fmt.Sprintf(
				"declared %s active, measured %s",
				contextLabel(measurement.ContextTarget), activeRangeLabel(activeStart, activeEnd))
		default:
			// No measured tokens (failed, planned, dry run): the claim is
			// declared but unconfirmed, so say so and never count it as a
			// verified active context.
			measurement.ContextLabel = "unverified (declared " + contextLabel(measurement.ContextTarget) + " active)"
		}
	case measurement.ContextSemantics == "capacity" && measurement.ContextTarget > 0:
		measurement.ContextLabel = contextLabel(measurement.ContextTarget) + " capacity"
		if longOutput {
			measurement.ActiveRange = activeRangeLabel(activeStart, activeEnd)
		}
	default:
		measurement.ContextLabel = measuredShapeLabel(*measurement)
	}
}

// measuredActiveContext derives per-request mean active context at the start
// (prompt only) and end (prompt plus completion) of decode from the
// measurement aggregates.
func measuredActiveContext(measurement SQLiteReportMeasurement) (start, end float64, ok bool) {
	if measurement.CompletedRequests <= 0 || !measurement.PromptTokensKnown || !measurement.CompletionTokensKnown {
		return 0, 0, false
	}
	requests := float64(measurement.CompletedRequests)
	start = float64(measurement.PromptTokensValue) / requests
	end = start + float64(measurement.CompletionTokensValue)/requests
	return start, end, true
}

// withinContextBand checks the measured active end against the declared
// target using the contract band [0.90, 1.00].
func withinContextBand(activeEnd float64, target int) bool {
	return activeEnd >= 0.90*float64(target) && activeEnd <= float64(target)
}

func measuredShapeLabel(measurement SQLiteReportMeasurement) string {
	if shape := requestShape(measurement); shape != "-" {
		return shape
	}
	return "unlabeled"
}

func activeRangeLabel(start, end float64) string {
	return approxTokenLabel(start) + " -> " + approxTokenLabel(end) + " active"
}

func approxTokenLabel(value float64) string {
	if value >= 1024 {
		return fmt.Sprintf("~%.0fk", value/1024)
	}
	return fmt.Sprintf("~%.0f", value)
}

func applySQLiteMeasurementDisplay(measurement *SQLiteReportMeasurement, metrics map[string]SQLiteReportMetric, startedAt, completedAt sql.NullString, wallTime sql.NullFloat64, promptTokens, completionTokens, totalTokens sql.NullInt64, outputTokS, perUserTokS, totalTokS sql.NullFloat64, errorType, errorMessage sql.NullString) {
	measurement.StartedAt = nullStringValue(startedAt)
	measurement.CompletedAt = nullStringValue(completedAt)
	measurement.WallTimeMS = displayNullFloat(wallTime)
	measurement.WallTimeMSValue = nullValue(wallTime.Valid, wallTime.Float64)
	measurement.WallTimeMSKnown = wallTime.Valid
	measurement.PromptTokens = displayNullInt(promptTokens)
	measurement.PromptTokensValue = nullValue(promptTokens.Valid, promptTokens.Int64)
	measurement.PromptTokensKnown = promptTokens.Valid
	measurement.CompletionTokens = displayNullInt(completionTokens)
	measurement.CompletionTokensValue = nullValue(completionTokens.Valid, completionTokens.Int64)
	measurement.CompletionTokensKnown = completionTokens.Valid
	measurement.TotalTokens = displayNullInt(totalTokens)
	measurement.TotalTokensValue = nullValue(totalTokens.Valid, totalTokens.Int64)
	measurement.TotalTokensKnown = totalTokens.Valid
	measurement.OutputTokS = displayNullFloat(outputTokS)
	measurement.OutputTokSValue = nullValue(outputTokS.Valid, outputTokS.Float64)
	measurement.OutputTokSKnown = outputTokS.Valid
	measurement.PerUserOutputTokS = displayNullFloat(perUserTokS)
	measurement.TotalTokS = displayNullFloat(totalTokS)
	measurement.ErrorType = nullStringValue(errorType)
	measurement.ErrorMessage = nullStringValue(errorMessage)
	measurement.OutputTokSStdDev = metricDisplay(metrics, "request_output_throughput", "StdDev")
	measurement.LatencyMeanMS = metricDisplay(metrics, "latency", "Mean")
	measurement.LatencyP50MS = metricDisplayFirst(metrics, "P50", "latency")
	measurement.LatencyP95MS = metricDisplayFirst(metrics, "P95", "latency")
	measurement.LatencyP99MS = metricDisplayFirst(metrics, "P99", "latency")
	applyTTFTDisplay(measurement, metrics)
	measurement.TPOTMeanMS = metricDisplayFirst(metrics, "Mean", "request_tpot", "tpot")
	measurement.DecodePerUserTokS = metricDisplay(metrics, "request_decode_throughput", "Mean")
	applyEffectivePrefillDisplay(measurement, metrics)
	measurement.ITLMeanMS = metricDisplay(metrics, "request_itl_mean", "Mean")
}

// applyTTFTDisplay renders TTFT only from measurements whose stats carry the
// streamed-source marker. Anything else shows "-" because an unmarked number
// does not prove that first-token time was measured.
func applyTTFTDisplay(measurement *SQLiteReportMeasurement, metrics map[string]SQLiteReportMetric) {
	if measurement.TTFTSource != "stream" {
		measurement.TTFTMeanMS = "-"
		measurement.TTFTP50MS = "-"
		measurement.TTFTP95MS = "-"
		measurement.TTFTP99MS = "-"
		return
	}
	measurement.TTFTMeanMS = metricDisplayFirst(metrics, "Mean", "request_ttft", "ttft")
	measurement.TTFTP50MS = metricDisplayFirst(metrics, "P50", "request_ttft", "ttft")
	measurement.TTFTP95MS = metricDisplayFirst(metrics, "P95", "request_ttft", "ttft")
	measurement.TTFTP99MS = metricDisplayFirst(metrics, "P99", "request_ttft", "ttft")
}

func applyEffectivePrefillDisplay(measurement *SQLiteReportMeasurement, metrics map[string]SQLiteReportMetric) {
	if measurement.TTFTSource != "stream" {
		measurement.EffectivePrefillTokS = "-"
		measurement.RequestEffectivePrefillTokS = "-"
		return
	}
	measurement.EffectivePrefillTokS = metricDisplay(metrics, "effective_prefill_throughput", "Mean")
	measurement.RequestEffectivePrefillTokS = metricDisplay(metrics, "request_effective_prefill_throughput", "Mean")
}

func loadSQLiteReportMetrics(db *sql.DB, doc *SQLiteReportDocument) error {
	rows, err := db.Query(`SELECT
		stats.measurement_id, stats.metric, stats.unit, stats.mean, stats.stddev, stats.min,
		stats.p50, stats.p90, stats.p95, stats.p99, stats.max, stats.count
		FROM metric_stats AS stats
		JOIN measurements AS measurement ON measurement.id = stats.measurement_id
		JOIN workloads AS workload ON workload.id = measurement.workload_id
		WHERE workload.role = 'benchmark'
		ORDER BY stats.measurement_id, stats.metric, stats.unit`)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var metric SQLiteReportMetric
		var mean, stddev, min, p50, p90, p95, p99, max sql.NullFloat64
		if err := rows.Scan(&metric.MeasurementID, &metric.Metric, &metric.Unit, &mean, &stddev, &min, &p50, &p90, &p95, &p99, &max, &metric.Count); err != nil {
			return err
		}
		metric.Mean = displayNullFloat(mean)
		metric.StdDev = displayNullFloat(stddev)
		metric.Min = displayNullFloat(min)
		metric.P50 = displayNullFloat(p50)
		metric.P90 = displayNullFloat(p90)
		metric.P95 = displayNullFloat(p95)
		metric.P99 = displayNullFloat(p99)
		metric.Max = displayNullFloat(max)
		metric.MeanValue = nullValue(mean.Valid, mean.Float64)
		metric.StdDevValue = nullValue(stddev.Valid, stddev.Float64)
		metric.MeanKnown = mean.Valid
		metric.StdDevKnown = stddev.Valid
		if doc.MeasurementMetrics[metric.MeasurementID] == nil {
			doc.MeasurementMetrics[metric.MeasurementID] = map[string]SQLiteReportMetric{}
		}
		doc.MeasurementMetrics[metric.MeasurementID][metric.Metric] = metric
	}
	return rows.Err()
}

func loadSQLiteReportRequestSummary(db *sql.DB, doc *SQLiteReportDocument) error {
	means, hasDetailedOutputTokS, err := loadSQLiteDetailedRequestSummary(db, doc)
	if err != nil {
		return err
	}
	requestRows, err := sqliteRequestRowsByMeasurement(db)
	if err != nil {
		return err
	}
	applySQLiteAggregateRequestSummary(doc, requestRows, hasDetailedOutputTokS, &means)
	doc.RequestSummary.LatencyMeanMS = displayWeightedMean(means.latency)
	doc.RequestSummary.TTFTMeanMS = displayWeightedMean(means.ttft)
	doc.RequestSummary.TPOTMeanMS = displayWeightedMean(means.tpot)
	doc.RequestSummary.ITLMeanMS = displayWeightedMean(means.itl)
	doc.RequestSummary.OutputTokSMean = displayWeightedMean(means.outputTokS)
	return nil
}

type sqliteReportSummaryMeans struct {
	latency    sqliteReportWeightedMean
	ttft       sqliteReportWeightedMean
	tpot       sqliteReportWeightedMean
	itl        sqliteReportWeightedMean
	outputTokS sqliteReportWeightedMean
}

func loadSQLiteDetailedRequestSummary(db *sql.DB, doc *SQLiteReportDocument) (sqliteReportSummaryMeans, bool, error) {
	var means sqliteReportSummaryMeans
	var latencyMean, ttftMean, tpotMean, itlMean, outputTokSMean sql.NullFloat64
	var latencyCount, ttftCount, tpotCount, itlCount, outputTokSCount int
	err := db.QueryRow(sqliteDetailedRequestSummaryQuery()).Scan(
		&doc.RequestSummary.Total, &doc.RequestSummary.Completed, &doc.RequestSummary.Failed,
		&doc.RequestSummary.Canceled, &latencyMean, &latencyCount, &ttftMean, &ttftCount,
		&tpotMean, &tpotCount, &itlMean, &itlCount, &outputTokSMean, &outputTokSCount)
	if err != nil {
		return sqliteReportSummaryMeans{}, false, err
	}
	means.latency.addNullFloat(latencyMean, latencyCount)
	means.ttft.addNullFloat(ttftMean, ttftCount)
	means.tpot.addNullFloat(tpotMean, tpotCount)
	means.itl.addNullFloat(itlMean, itlCount)
	means.outputTokS.addNullFloat(outputTokSMean, outputTokSCount)
	return means, outputTokSCount > 0, nil
}

func sqliteDetailedRequestSummaryQuery() string {
	return `SELECT
		COUNT(*),
		COALESCE(SUM(CASE WHEN r.status = 'completed' THEN 1 ELSE 0 END), 0),
		COALESCE(SUM(CASE WHEN r.status = 'failed' THEN 1 ELSE 0 END), 0),
		COALESCE(SUM(CASE WHEN r.status = 'canceled' THEN 1 ELSE 0 END), 0),
		AVG(r.latency_ms), COUNT(r.latency_ms),
		AVG(r.ttft_ms), COUNT(r.ttft_ms),
		AVG(r.tpot_ms), COUNT(r.tpot_ms),
		AVG(r.itl_mean_ms), COUNT(r.itl_mean_ms),
		AVG(r.output_tok_s), COUNT(r.output_tok_s)
		FROM requests r
		JOIN measurements m ON m.id = r.measurement_id
		JOIN workloads w ON w.id = m.workload_id
		WHERE w.role = 'benchmark'`
}

func sqliteRequestRowsByMeasurement(db *sql.DB) (map[int64]int, error) {
	rows, err := db.Query(`SELECT r.measurement_id, COUNT(*)
		FROM requests r
		JOIN measurements m ON m.id = r.measurement_id
		JOIN workloads w ON w.id = m.workload_id
		WHERE w.role = 'benchmark'
		GROUP BY r.measurement_id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[int64]int{}
	for rows.Next() {
		var measurementID int64
		var count int
		if err := rows.Scan(&measurementID, &count); err != nil {
			return nil, err
		}
		out[measurementID] = count
	}
	return out, rows.Err()
}

func applySQLiteAggregateRequestSummary(doc *SQLiteReportDocument, requestRows map[int64]int, hasDetailedOutputTokS bool, means *sqliteReportSummaryMeans) {
	for _, measurement := range doc.Measurements {
		hasRequestRows := requestRows[measurement.ID] > 0
		if !hasDetailedOutputTokS || !hasRequestRows {
			means.addMeasurementOutputTokS(measurement)
		}
		if hasRequestRows {
			continue
		}
		means.addAggregateMeasurement(doc, measurement)
	}
	doc.RequestSummary.Total = doc.RequestSummary.Completed + doc.RequestSummary.Failed + doc.RequestSummary.Canceled
}

func (means *sqliteReportSummaryMeans) addMeasurementOutputTokS(measurement SQLiteReportMeasurement) {
	if measurement.OutputTokSKnown {
		means.outputTokS.add(measurement.OutputTokSValue, 1)
	}
}

func (means *sqliteReportSummaryMeans) addAggregateMeasurement(doc *SQLiteReportDocument, measurement SQLiteReportMeasurement) {
	doc.RequestSummary.Completed += measurement.CompletedRequests
	doc.RequestSummary.Failed += measurement.FailedRequests
	doc.RequestSummary.Canceled += canceledRequestEstimate(measurement)
	metrics := doc.MeasurementMetrics[measurement.ID]
	means.latency.addMetric(metricFirst(metrics, "latency"))
	means.ttft.addMetric(metricFirst(metrics, "request_ttft", "ttft"))
	means.tpot.addMetric(metricFirst(metrics, "request_tpot", "tpot"))
	means.itl.addMetric(metricFirst(metrics, "request_itl_mean"))
}

func canceledRequestEstimate(measurement SQLiteReportMeasurement) int {
	if measurement.Status != "canceled" {
		return 0
	}
	remaining := measurement.SamplesRequested - measurement.CompletedRequests - measurement.FailedRequests
	if remaining < 0 {
		return 0
	}
	return remaining
}

type sqliteReportWeightedMean struct {
	total  float64
	weight int
}

func (mean *sqliteReportWeightedMean) add(value float64, weight int) {
	if weight <= 0 {
		weight = 1
	}
	mean.total += value * float64(weight)
	mean.weight += weight
}

func (mean *sqliteReportWeightedMean) addNullFloat(value sql.NullFloat64, weight int) {
	if !value.Valid {
		return
	}
	mean.add(value.Float64, weight)
}

func (mean *sqliteReportWeightedMean) addMetric(metric SQLiteReportMetric, ok bool) {
	if !ok || !metric.MeanKnown {
		return
	}
	mean.add(metric.MeanValue, metric.Count)
}

func displayWeightedMean(mean sqliteReportWeightedMean) string {
	if mean.weight == 0 {
		return "-"
	}
	return displayFloat(mean.total / float64(mean.weight))
}

func loadSQLiteReportEventCounts(db *sql.DB, doc *SQLiteReportDocument) error {
	rows, err := db.Query(`SELECT type, COUNT(*) FROM events GROUP BY type ORDER BY type`)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var count SQLiteReportCount
		if err := rows.Scan(&count.Name, &count.Count); err != nil {
			return err
		}
		doc.EventCounts = append(doc.EventCounts, count)
	}
	return rows.Err()
}

func loadSQLiteReportNotableEvents(db *sql.DB, doc *SQLiteReportDocument) error {
	rows, err := db.Query(`SELECT
		timestamp, level, type, COALESCE(profile_id, ''), COALESCE(workload_id, ''), COALESCE(message, '')
		FROM events
		WHERE level IN ('warn', 'error') OR type LIKE '%failed%' OR TRIM(COALESCE(message, '')) <> ''
		ORDER BY timestamp DESC LIMIT 50`)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var event SQLiteReportEvent
		if err := rows.Scan(&event.Timestamp, &event.Level, &event.Type, &event.Profile, &event.Workload, &event.Message); err != nil {
			return err
		}
		doc.NotableEvents = append(doc.NotableEvents, event)
	}
	return rows.Err()
}

func loadSQLiteReportCommands(db *sql.DB, doc *SQLiteReportDocument) error {
	rows, err := db.Query(`SELECT
		phase, status, exit_code, started_at, completed_at, argv_json,
		COALESCE(profile_id, ''), COALESCE(measurement_id, 0)
		FROM commands ORDER BY id`)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var command SQLiteReportCommand
		var exitCode sql.NullInt64
		var startedAt, completedAt sql.NullString
		var argvJSON string
		if err := rows.Scan(&command.Phase, &command.Status, &exitCode, &startedAt, &completedAt, &argvJSON, &command.ProfileID, &command.MeasurementID); err != nil {
			return err
		}
		command.ExitCode = displayNullInt(exitCode)
		command.StartedAt = nullStringValue(startedAt)
		command.Completed = nullStringValue(completedAt)
		command.Argv = commandSummaryFromJSON(argvJSON)
		doc.Commands = append(doc.Commands, command)
	}
	return rows.Err()
}

func loadSQLiteReportExports(db *sql.DB, doc *SQLiteReportDocument) error {
	rows, err := db.Query(`SELECT name, format, media_type, created_at FROM reports ORDER BY created_at, name`)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var export SQLiteReportExport
		if err := rows.Scan(&export.Name, &export.Format, &export.MediaType, &export.CreatedAt); err != nil {
			return err
		}
		doc.ExistingReports = append(doc.ExistingReports, export)
	}
	return rows.Err()
}

func loadSQLiteReportArtifactSummaries(db *sql.DB, doc *SQLiteReportDocument) error {
	rows, err := db.Query(`SELECT kind, COUNT(*), COALESCE(SUM(uncompressed_size_bytes), 0) FROM artifacts GROUP BY kind ORDER BY kind`)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var summary SQLiteReportArtifactSummary
		if err := rows.Scan(&summary.Kind, &summary.Count, &summary.UncompressedSizeBytes); err != nil {
			return err
		}
		doc.ArtifactSummaries = append(doc.ArtifactSummaries, summary)
	}
	return rows.Err()
}

func sqliteReportFullRunTimingRows(doc SQLiteReportDocument) []SQLiteReportFullRunTimingRow {
	rows := make([]SQLiteReportFullRunTimingRow, 0, len(doc.Measurements))
	for _, measurement := range doc.Measurements {
		if !isFullGenerationMeasurement(measurement) {
			continue
		}
		failureLabel, failureReason := measurementFailure(measurement)
		rows = append(rows, SQLiteReportFullRunTimingRow{
			MeasurementID:               measurement.ID,
			Profile:                     measurement.Profile,
			Workload:                    measurement.Workload,
			ContextLabel:                measurement.ContextLabel,
			ContextSortKey:              measurement.ContextSortKey,
			Concurrency:                 measurement.Concurrency,
			RepeatCount:                 measurement.RepeatCount,
			OutputTokS:                  measurement.OutputTokS,
			EffectivePrefillTokS:        measurement.EffectivePrefillTokS,
			RequestEffectivePrefillTokS: measurement.RequestEffectivePrefillTokS,
			TTFTMeanMS:                  measurement.TTFTMeanMS,
			TTFTP99MS:                   measurement.TTFTP99MS,
			DecodePerUserTokS:           measurement.DecodePerUserTokS,
			LatencyP95MS:                measurement.LatencyP95MS,
			CompletedRequests:           measurement.CompletedRequests,
			FailedRequests:              measurement.FailedRequests,
			Detail: sqliteReportCellDetail(
				doc,
				measurement,
				throughputMode(measurement.Phase),
				throughputRowShape(measurement),
				failureLabel,
				failureReason,
			),
		})
	}
	sort.SliceStable(rows, func(i, j int) bool {
		if rows[i].ContextSortKey != rows[j].ContextSortKey {
			return rows[i].ContextSortKey < rows[j].ContextSortKey
		}
		if rows[i].Profile != rows[j].Profile {
			return rows[i].Profile < rows[j].Profile
		}
		if rows[i].Workload != rows[j].Workload {
			return rows[i].Workload < rows[j].Workload
		}
		return rows[i].Concurrency < rows[j].Concurrency
	})
	return rows
}

func isFullGenerationMeasurement(measurement SQLiteReportMeasurement) bool {
	if measurement.Status != "completed" || bench.NormalizeReportPhase(measurement.Phase) == "prefill" || measurement.CompletedRequests <= 0 {
		return false
	}
	completionPerRequest := float64(measurement.CompletionTokensValue) / float64(measurement.CompletedRequests)
	return measurement.CompletionTokensKnown && completionPerRequest > longOutputTokenThreshold &&
		(measurement.EffectivePrefillTokS != "-" || measurement.DecodePerUserTokS != "-")
}

func sqliteReportPhaseSections(measurements []SQLiteReportMeasurement) []SQLiteReportPhaseSection {
	byPhase := map[string][]SQLiteReportMeasurement{}
	for _, measurement := range measurements {
		phase := bench.NormalizeReportPhase(measurement.Phase)
		byPhase[phase] = append(byPhase[phase], measurement)
	}
	phases := collections.SortedKeys(byPhase)
	sort.SliceStable(phases, func(i, j int) bool {
		left, right := bench.PhaseRank(phases[i]), bench.PhaseRank(phases[j])
		if left != right {
			return left < right
		}
		return phases[i] < phases[j]
	})
	out := make([]SQLiteReportPhaseSection, 0, len(phases))
	for _, phase := range phases {
		out = append(out, SQLiteReportPhaseSection{Phase: phase, Title: bench.PhaseTitle(phase), Measurements: byPhase[phase]})
	}
	return out
}

func sqliteReportCharts(measurements []SQLiteReportMeasurement) []SQLiteReportChart {
	bars := make([]SQLiteReportBar, 0, len(measurements))
	maxValue := 0.0
	for _, measurement := range measurements {
		if measurement.OutputTokSKnown && measurement.OutputTokSValue > maxValue {
			maxValue = measurement.OutputTokSValue
		}
	}
	if maxValue <= 0 {
		return nil
	}
	for _, measurement := range measurements {
		if !measurement.OutputTokSKnown {
			continue
		}
		index := len(bars)
		label := fmt.Sprintf("%s / %s / c%d", measurement.Profile, measurement.Workload, measurement.Concurrency)
		bars = append(bars, SQLiteReportBar{
			Label:  label,
			Value:  displayFloat(measurement.OutputTokSValue),
			LabelY: 24 + index*28,
			RectY:  10 + index*28,
			Width:  displayFloat(560 * measurement.OutputTokSValue / maxValue),
		})
	}
	return []SQLiteReportChart{{Title: "Aggregate Output Throughput", Unit: "tok/s", Height: 24 + len(bars)*28, Bars: bars}}
}

// loadSQLiteReportSpecProvenance verifies the latest run's original spec
// against its generator stamp: generated, edited after generation, or
// custom (no stamp). Verification happens here, from the stored bytes —
// the label is never taken on trust from the runner.
func loadSQLiteReportSpecProvenance(db *sql.DB, doc *SQLiteReportDocument) error {
	var content string
	err := db.QueryRow(`SELECT content FROM specs WHERE run_id = ? AND kind = 'original'`, doc.Run.ID).Scan(&content)
	if err == sql.ErrNoRows {
		doc.SpecProvenance = artifact.SpecProvenanceCustom
		return nil
	}
	if err != nil {
		return err
	}
	status, stamp := artifact.VerifySpecProvenance([]byte(content))
	doc.SpecProvenance = status
	doc.SpecGenerator = stamp
	if status == artifact.SpecProvenanceGenerated && stamp != nil && len(stamp.Intent) > 0 {
		var intent struct {
			Concurrency []int `json:"concurrency"`
		}
		if err := json.Unmarshal(stamp.Intent, &intent); err == nil {
			doc.SpecConcurrency = intent.Concurrency
		}
	}
	return nil
}

func specProvenanceDisplay(doc SQLiteReportDocument) string {
	switch doc.SpecProvenance {
	case artifact.SpecProvenanceGenerated:
		return "Generated default sweep (" + doc.SpecGenerator.Tool + ")"
	case artifact.SpecProvenanceEdited:
		return "Custom grid (edited after generation)"
	default:
		return "Custom grid (hand-authored)"
	}
}

// trimmedThroughputRows synthesizes rows for author-trimmed ladder points so
// declared trims render like adaptive skips, never as silent holes. Only
// verified generated specs are trusted for this.
func trimmedThroughputRows(doc SQLiteReportDocument, existing []SQLiteReportThroughputRow) []SQLiteReportThroughputRow {
	if doc.SpecProvenance != artifact.SpecProvenanceGenerated || doc.SpecGenerator == nil {
		return nil
	}
	ladder := doc.SpecConcurrency
	if len(ladder) == 0 {
		return nil
	}
	// A real measurement — from any run in a model-level artifact — always
	// wins over a synthesized trim marker for the same point.
	measured := map[string]struct{}{}
	for _, row := range existing {
		measured[fmt.Sprintf("%d/%s/%d", row.ContextTarget, row.Mode, row.Concurrency)] = struct{}{}
	}
	var rows []SQLiteReportThroughputRow
	for _, trim := range doc.SpecGenerator.LadderTrims {
		label := contextLabel(trim.Context)
		for _, concurrency := range ladder {
			if concurrency <= trim.MaxConcurrency {
				continue
			}
			for _, mode := range []string{"decode", "prefill"} {
				if _, ok := measured[fmt.Sprintf("%d/%s/%d", trim.Context, mode, concurrency)]; ok {
					continue
				}
				rows = append(rows, SQLiteReportThroughputRow{
					Phase:            bench.PhaseTitle(mode),
					Mode:             mode,
					RunID:            doc.Run.ID,
					Profile:          label,
					Model:            doc.Run.Name,
					ContextWindow:    trim.Context,
					ContextLabel:     "unverified (declared " + label + " active)",
					ContextSortKey:   trim.Context,
					ContextTarget:    trim.Context,
					ContextSemantics: "active",
					Concurrency:      concurrency,
					Shape:            "-",
					ThroughputTokS:   "trimmed",
					PerUserTokS:      "trimmed",
					TTFTMeanMS:       "-",
					TTFTP99MS:        "-",
					LatencyP95MS:     "-",
					Status:           "skipped",
					FailureLabel:     "trimmed",
					FailureReason:    "trimmed by author: " + trim.Reason,
					Detail: SQLiteReportCellDetail{
						Available:     true,
						Phase:         bench.PhaseTitle(mode),
						Mode:          mode,
						Status:        "skipped",
						FailureLabel:  "trimmed",
						FailureReason: "trimmed by author: " + trim.Reason,
						Profile:       label,
						ContextLabel:  "unverified (declared " + label + " active)",
						ContextWindow: trim.Context,
						Concurrency:   concurrency,
					},
				})
			}
		}
	}
	return rows
}

func sqliteReportMetadataItems(doc SQLiteReportDocument) []SQLiteReportMetadataItem {
	items := []SQLiteReportMetadataItem{
		{Label: "Spec", Value: specProvenanceDisplay(doc)},
		{Label: "Engine", Value: joinUnique(engineSummaries(doc.Engines), ", ")},
		{Label: "Runs", Value: fmt.Sprint(len(doc.Runs))},
		{Label: "Hardware", Value: bench.FirstNonEmpty(doc.Run.Hardware, "-")},
		{Label: "Quant", Value: bench.FirstNonEmpty(inferQuantization(doc.Profiles), "-")},
		{Label: "KV", Value: bench.FirstNonEmpty(joinUnique(profileKVDtypes(doc.Profiles), ", "), "-")},
		// Active contexts come only from declared-and-verified claims; the
		// server limit is reported separately and never as a context.
		{Label: "Active contexts", Value: formatContextList(measurementPositiveInts(doc.Measurements, func(measurement SQLiteReportMeasurement) int {
			if measurement.ContextVerified {
				return measurement.ContextTarget
			}
			return 0
		}))},
		{Label: "Server limits", Value: formatContextList(profilePositiveInts(doc.Profiles, func(profile SQLiteReportProfile) int {
			return profile.ContextWindow
		}))},
		{Label: "Users", Value: formatIntList(measurementPositiveInts(doc.Measurements, func(measurement SQLiteReportMeasurement) int {
			return measurement.Concurrency
		}))},
		{Label: "Requests", Value: fmt.Sprintf("%d ok / %d err", doc.RequestSummary.Completed, doc.RequestSummary.Failed)},
	}
	items = append(items, servedModelMismatchItems(doc)...)
	return items
}

// servedModelMismatchItems surfaces declared-versus-self-reported model
// disagreements from the engine identity probe. Declared, checked, then
// shown: a silent mismatch is how a benchmark reports the wrong model.
func servedModelMismatchItems(doc SQLiteReportDocument) []SQLiteReportMetadataItem {
	servedByEngine := map[string]map[string][]string{}
	for _, engine := range doc.Engines {
		if len(engine.ServedModelsByProfile) > 0 {
			// Keyed by engine id: profiles.engine_id stores the id in both
			// old (bare name) and namespaced artifacts.
			servedByEngine[engine.ID] = engine.ServedModelsByProfile
		}
	}
	if len(servedByEngine) == 0 {
		return nil
	}
	var items []SQLiteReportMetadataItem
	seen := map[string]bool{}
	for _, profile := range doc.Profiles {
		// Compare each profile only against its own probe result, and only
		// warn when the declared model is absent from everything the server
		// reported; multi-model servers may list it anywhere.
		servedModels := servedByEngine[profile.Engine][profile.Name]
		if profile.Model == "" || len(servedModels) == 0 || slices.Contains(servedModels, profile.Model) {
			continue
		}
		served := strings.Join(servedModels, ", ")
		if seen[profile.Model+served] {
			continue
		}
		seen[profile.Model+served] = true
		items = append(items, SQLiteReportMetadataItem{
			Label: "Model mismatch",
			Value: fmt.Sprintf("spec declares %s, server reports %s", profile.Model, served),
		})
	}
	return items
}

func profilePositiveInts(profiles []SQLiteReportProfile, value func(SQLiteReportProfile) int) []int {
	values := make([]int, 0, len(profiles))
	for _, profile := range profiles {
		if current := value(profile); current > 0 {
			values = append(values, current)
		}
	}
	return uniqueSortedInts(values)
}

func sqliteReportThroughputRows(doc SQLiteReportDocument) []SQLiteReportThroughputRow {
	rows := make([]SQLiteReportThroughputRow, 0, len(doc.Measurements))
	for _, measurement := range doc.Measurements {
		mode := throughputMode(measurement.Phase)
		inputTokS := inputThroughput(measurement)
		// Aggregated repeat rows carry the mean ± spread across repeats;
		// the recomputed sum/sum value would hide repeat variability.
		if measurement.InputTokSSpread != "" {
			inputTokS = measurement.InputTokSSpread
		}
		throughputTokS, perUserTokS := phaseThroughputMetrics(mode, inputTokS, measurement)
		if mode == "prefill" && measurement.InputPerUserSpread != "" {
			perUserTokS = measurement.InputPerUserSpread
		}
		failureLabel, failureReason := measurementFailure(measurement)
		if failureLabel != "" {
			throughputTokS = displayFailureMetric(throughputTokS, failureLabel)
			perUserTokS = displayFailureMetric(perUserTokS, failureLabel)
		}
		rows = append(rows, SQLiteReportThroughputRow{
			Phase:             bench.PhaseTitle(bench.NormalizeReportPhase(measurement.Phase)),
			Mode:              mode,
			RunID:             measurement.RunID,
			MeasurementID:     measurement.ID,
			ProfileID:         measurement.ProfileID,
			Profile:           measurement.Profile,
			Model:             measurement.Model,
			WorkloadID:        measurement.WorkloadID,
			Workload:          measurement.Workload,
			ContextWindow:     measurement.ContextWindow,
			ContextLabel:      measurement.ContextLabel,
			ContextSortKey:    measurement.ContextSortKey,
			ContextMismatch:   measurement.ContextMismatch,
			MismatchNote:      measurement.ContextMismatchNote,
			ContextTarget:     measurement.ContextTarget,
			ContextSemantics:  measurement.ContextSemantics,
			ContextVerified:   measurement.ContextVerified,
			ActiveRange:       measurement.ActiveRange,
			Concurrency:       measurement.Concurrency,
			Shape:             throughputRowShape(measurement),
			InputTokS:         inputTokS,
			TotalTokS:         measurement.TotalTokS,
			OutputTokS:        measurement.OutputTokS,
			PerUserOutputTokS: measurement.PerUserOutputTokS,
			ThroughputTokS:    throughputTokS,
			PerUserTokS:       perUserTokS,
			TTFTMeanMS:        measurement.TTFTMeanMS,
			TTFTP99MS:         measurement.TTFTP99MS,
			LatencyP95MS:      measurement.LatencyP95MS,
			SLODisplay:        sloRowDisplay(measurement),
			CompletedRequests: measurement.CompletedRequests,
			FailedRequests:    measurement.FailedRequests,
			Status:            measurement.Status,
			FailureLabel:      failureLabel,
			FailureReason:     failureReason,
			Detail:            sqliteReportCellDetail(doc, measurement, mode, throughputRowShape(measurement), failureLabel, failureReason),
			Baseline:          measurement.Concurrency == 1,
		})
	}
	sort.SliceStable(rows, func(i, j int) bool {
		if rows[i].ContextSortKey != rows[j].ContextSortKey {
			return rows[i].ContextSortKey < rows[j].ContextSortKey
		}
		leftMode, rightMode := throughputModeRank(rows[i].Mode), throughputModeRank(rows[j].Mode)
		if leftMode != rightMode {
			return leftMode < rightMode
		}
		return rows[i].Concurrency < rows[j].Concurrency
	})
	return rows
}

// sloRowDisplay renders "% in SLO / goodput req/s" for rows whose workload
// declared an SLO; goodput must stay visible in the headline table, not only
// in hidden detail sections.
func sloRowDisplay(measurement SQLiteReportMeasurement) string {
	if measurement.SLONote == "" {
		return ""
	}
	return measurement.SLOMetPct + " / " + measurement.GoodputRPS
}

// throughputRowShape renders the measured token shape, extended with the
// active-context range for long-output rows.
func throughputRowShape(measurement SQLiteReportMeasurement) string {
	shape := requestShape(measurement)
	if measurement.ActiveRange != "" && shape != "-" {
		return shape + " (" + measurement.ActiveRange + ")"
	}
	return shape
}

func measurementFailure(measurement SQLiteReportMeasurement) (label, reason string) {
	status := strings.ToLower(strings.TrimSpace(measurement.Status))
	switch status {
	case "failed", "skipped", "canceled":
	default:
		return "", ""
	}
	reason = strings.TrimSpace(bench.FirstNonEmpty(measurement.ErrorMessage, measurement.ErrorType))
	if reason == "" {
		reason = status
	}
	return compactFailureLabel(status, reason), reason
}

func compactFailureLabel(status, reason string) string {
	lowerReason := strings.ToLower(reason)
	switch {
	case strings.Contains(lowerReason, "memavailable"), strings.Contains(lowerReason, "memory floor"):
		return "mem floor"
	case strings.Contains(lowerReason, "did not become ready"):
		return "not ready"
	case strings.Contains(lowerReason, "server exited"):
		return "server exit"
	case strings.Contains(lowerReason, "oom"), strings.Contains(lowerReason, "out of memory"):
		return "oom"
	case strings.Contains(lowerReason, "canceled"), strings.Contains(lowerReason, "cancelled"):
		return "canceled"
	case strings.TrimSpace(status) != "":
		return strings.TrimSpace(status)
	default:
		return "failed"
	}
}

// displayFailureMetric shows the outcome, not a residual number: a failed
// or skipped point renders its failure label ("failed", "skipped",
// "mem floor") in metric cells instead of a misleading "0.000".
func displayFailureMetric(value, failureLabel string) string {
	if strings.TrimSpace(failureLabel) != "" {
		return failureLabel
	}
	return value
}

func sqliteReportCellDetail(doc SQLiteReportDocument, measurement SQLiteReportMeasurement, mode, shape, failureLabel, failureReason string) SQLiteReportCellDetail {
	profile := findReportProfile(doc.Profiles, measurement.ProfileID, measurement.Profile)
	detail := SQLiteReportCellDetail{
		Available:        true,
		Phase:            bench.PhaseTitle(bench.NormalizeReportPhase(measurement.Phase)),
		Mode:             mode,
		Status:           measurement.Status,
		FailureLabel:     failureLabel,
		FailureReason:    failureReason,
		RunID:            measurement.RunID,
		MeasurementID:    measurement.ID,
		Model:            bench.FirstNonEmpty(measurement.Model, profile.Model),
		Profile:          measurement.Profile,
		Workload:         measurement.Workload,
		ContextLabel:     measurement.ContextLabel,
		ContextWindow:    measurement.ContextWindow,
		Concurrency:      measurement.Concurrency,
		SamplesRequested: measurement.SamplesRequested,
		Shape:            shape,
		ServeCommand:     commandForProfile(doc.Commands, profile.ID),
		BenchmarkCommand: commandForMeasurement(doc.Commands, measurement.ID),
		EngineArgs:       commandSummaryFromJSON(profile.EngineArgsJSON),
		ServeJSON:        compactJSONForDetail(profile.ServeJSON),
		EnvJSON:          compactJSONForDetail(profile.EnvJSON),
	}
	if measurement.RepeatCount > 1 {
		detail.Source = fmt.Sprintf("aggregate of %d repeats", measurement.RepeatCount)
		detail.RunID = ""
		detail.MeasurementID = 0
		detail.BenchmarkCommand = ""
	}
	detail.ProfileConfig = []SQLiteReportMetadataItem{
		{Label: "Server limit", Value: displayContextWindow(profile.ContextWindow)},
		{Label: "Max seqs", Value: displayPositiveInt(profile.MaxNumSeqs)},
		{Label: "Batched tokens", Value: displayPositiveInt(profile.MaxNumBatchedTokens)},
		{Label: "GPU memory", Value: dashIfEmpty(profile.GPUMemoryUtilizationS)},
		{Label: "KV cache", Value: dashIfEmpty(profile.KVCacheDtype)},
		{Label: "Prefix cache", Value: dashIfEmpty(profile.PrefixCaching)},
		{Label: "Sleep", Value: fmt.Sprint(profile.EnableSleepMode)},
	}
	detail.Metrics = cellDetailMetrics(measurement)
	return detail
}

// cellDetailMetrics carries the measurement's numbers into the detail view:
// the artifact records them, so the detail must show them, formatted by the
// shared display rules. Empty or unmeasured values are dropped, not dashed.
func cellDetailMetrics(measurement SQLiteReportMeasurement) []SQLiteReportMetadataItem {
	items := []SQLiteReportMetadataItem{
		{Label: "Requests ok/err", Value: fmt.Sprintf("%d / %d", measurement.CompletedRequests, measurement.FailedRequests)},
		{Label: "Wall time", Value: FormatDurationDisplay(measurement.WallTimeMS)},
		{Label: "RPS", Value: FormatRateDisplay(measurement.RPS)},
		{Label: "Output tok/s", Value: FormatRateDisplay(measurement.OutputTokS)},
		{Label: "Out/user tok/s", Value: FormatRateDisplay(measurement.PerUserOutputTokS)},
		{Label: "Effective prefill tok/s", Value: FormatRateDisplay(measurement.EffectivePrefillTokS)},
		{Label: "Effective prefill/user tok/s", Value: FormatRateDisplay(measurement.RequestEffectivePrefillTokS)},
		{Label: "Decode/user tok/s", Value: FormatRateDisplay(measurement.DecodePerUserTokS)},
		{Label: "Total tok/s", Value: FormatRateDisplay(measurement.TotalTokS)},
		{Label: "Prompt tokens", Value: measurement.PromptTokens},
		{Label: "Completion tokens", Value: measurement.CompletionTokens},
		{Label: "TTFT mean/p50/p95/p99", Value: durationSeries(measurement.TTFTMeanMS, measurement.TTFTP50MS, measurement.TTFTP95MS, measurement.TTFTP99MS)},
		{Label: "Latency p50/p95/p99", Value: durationSeries(measurement.LatencyP50MS, measurement.LatencyP95MS, measurement.LatencyP99MS)},
		{Label: "TPOT mean", Value: FormatDurationDisplay(measurement.TPOTMeanMS)},
		{Label: "ITL tok-wt", Value: FormatDurationDisplay(measurement.ITLTokenWeightedMS)},
		{Label: "Achieved users", Value: measurement.AchievedConcurrency},
		{Label: "Failures", Value: measurement.FailureBreakdown},
		{Label: "GPU util", Value: measurement.GPUUtil},
		{Label: "GPU mem peak", Value: measurement.GPUMemPeak},
	}
	if measurement.SLONote != "" {
		items = append(items,
			SQLiteReportMetadataItem{Label: "SLO (" + measurement.SLONote + ")", Value: measurement.SLOMetPct},
			SQLiteReportMetadataItem{Label: "Goodput req/s", Value: FormatRateDisplay(measurement.GoodputRPS)},
		)
	}
	out := items[:0]
	for _, item := range items {
		value := strings.TrimSpace(item.Value)
		if value == "" || value == "-" || value == "- / - / -" || value == "- / - / - / -" {
			continue
		}
		out = append(out, item)
	}
	return out
}

// durationSeries renders a compact percentile series like
// "1.2s / 1.4s / 2.1s", keeping "-" for unmeasured entries.
func durationSeries(values ...string) string {
	parts := make([]string, 0, len(values))
	for _, value := range values {
		parts = append(parts, FormatDurationDisplay(value))
	}
	return strings.Join(parts, " / ")
}

func findReportProfile(profiles []SQLiteReportProfile, id, name string) SQLiteReportProfile {
	for _, profile := range profiles {
		if profile.ID == id && id != "" {
			return profile
		}
	}
	for _, profile := range profiles {
		if profile.Name == name && name != "" {
			return profile
		}
	}
	return SQLiteReportProfile{}
}

func commandForProfile(commands []SQLiteReportCommand, profileID string) string {
	for _, command := range commands {
		if command.ProfileID == profileID && command.Phase == "server_start" && strings.TrimSpace(command.Argv) != "" {
			return command.Argv
		}
	}
	return ""
}

func commandForMeasurement(commands []SQLiteReportCommand, measurementID int64) string {
	if measurementID <= 0 {
		return ""
	}
	for _, phase := range []string{"workload_finish", "workload_start", "measurement", "benchmark", "planned_run"} {
		for _, command := range commands {
			if command.MeasurementID == measurementID && command.Phase == phase && strings.TrimSpace(command.Argv) != "" {
				return command.Argv
			}
		}
	}
	for _, command := range commands {
		if command.MeasurementID == measurementID && strings.TrimSpace(command.Argv) != "" {
			return command.Argv
		}
	}
	return ""
}

func compactJSONForDetail(data string) string {
	data = strings.TrimSpace(data)
	if data == "" || data == "{}" || data == "[]" || data == "null" {
		return ""
	}
	var value any
	if err := json.Unmarshal([]byte(data), &value); err != nil {
		return data
	}
	encoded, err := json.Marshal(value)
	if err != nil {
		return data
	}
	return string(encoded)
}

func displayPositiveInt(value int) string {
	if value <= 0 {
		return "-"
	}
	return fmt.Sprint(value)
}

func displayContextWindow(value int) string {
	if value <= 0 {
		return "-"
	}
	return contextLabel(value)
}

func dashIfEmpty(value string) string {
	if strings.TrimSpace(value) == "" {
		return "-"
	}
	return value
}

func throughputMode(phase string) string {
	normalized := bench.NormalizeReportPhase(phase)
	switch normalized {
	case "prefill":
		return "prefill"
	case "decode":
		return "decode"
	default:
		value := strings.ToLower(strings.TrimSpace(bench.PhaseTitle(normalized)))
		if value == "" || value == "-" {
			return "run"
		}
		return value
	}
}

func throughputModeRank(mode string) int {
	switch strings.ToLower(strings.TrimSpace(mode)) {
	case "decode":
		return 0
	case "prefill":
		return 1
	default:
		return 2
	}
}

func phaseThroughputMetrics(mode string, inputTokS string, measurement SQLiteReportMeasurement) (string, string) {
	if mode == "prefill" {
		return inputTokS, perUserMetric(inputTokS, measurement.Concurrency)
	}
	return measurement.OutputTokS, measurement.PerUserOutputTokS
}

func perUserMetric(value string, concurrency int) string {
	parsed, ok := parseDisplayedFloat(value)
	if !ok || concurrency <= 0 {
		return "-"
	}
	return displayFloat(parsed / float64(concurrency))
}

type throughputGroupKey struct {
	profile      string
	contextLabel string
}

// ClaimKey groups throughput rows by their declared context claim, not
// their outcome label: a skipped or failed point stays in its declared
// context's table as a row instead of fragmenting into a second
// "unverified" table. Undeclared rows fall back to their measured label.
func (row SQLiteReportThroughputRow) ClaimKey() string {
	if row.ContextTarget > 0 && (row.ContextSemantics == "active" || row.ContextSemantics == "capacity") {
		return fmt.Sprintf("%s:%d", row.ContextSemantics, row.ContextTarget)
	}
	return "label:" + row.ContextLabel
}

// ClaimTitle labels a table by its declared claim and the verification state
// of its completed rows: verified active claims earn the active-context
// label, unverified ones say so, capacity claims are labeled by limit, and
// undeclared rows keep their measured-shape label.
func ClaimTitle(semantics string, target int, anyVerified bool, fallback string) string {
	switch {
	case semantics == "active" && target > 0:
		if anyVerified {
			return contextLabel(target) + " active context"
		}
		return "unverified (declared " + contextLabel(target) + " active)"
	case semantics == "capacity" && target > 0:
		return contextLabel(target) + " capacity"
	default:
		return fallback
	}
}

type throughputAxisVisibility struct {
	profile bool
}

func sqliteReportThroughputGroups(rows []SQLiteReportThroughputRow) []SQLiteReportThroughputGroup {
	visibility := throughputAxisVisibilityForRows(rows)
	groups := []SQLiteReportThroughputGroup{}
	groupIndexes := map[throughputGroupKey]int{}
	rowIndexes := []map[int]int{}
	mismatchNotes := make([]string, 0)
	claims := []throughputGroupClaim{}
	for _, row := range rows {
		key := throughputGroupKey{
			profile:      row.Profile,
			contextLabel: row.ClaimKey(),
		}
		index, ok := groupIndexes[key]
		if !ok {
			index = len(groups)
			groupIndexes[key] = index
			groups = append(groups, SQLiteReportThroughputGroup{
				Title:          row.ContextLabel,
				Profile:        key.profile,
				ContextSortKey: row.ContextSortKey,
				ServerLimit:    row.ContextWindow,
			})
			rowIndexes = append(rowIndexes, map[int]int{})
			mismatchNotes = append(mismatchNotes, "")
			claims = append(claims, throughputGroupClaim{semantics: row.ContextSemantics, target: row.ContextTarget, fallback: row.ContextLabel})
		}
		if row.ContextVerified {
			claims[index].anyVerified = true
		}
		if strings.EqualFold(strings.TrimSpace(row.Status), "completed") && !row.ContextVerified && !row.ContextMismatch {
			claims[index].completedUnverified++
		}
		if row.ContextMismatch && row.MismatchNote != "" {
			mismatchNotes[index] = row.MismatchNote
		}
		rowIndex, ok := rowIndexes[index][row.Concurrency]
		if !ok {
			rowIndex = len(groups[index].Rows)
			rowIndexes[index][row.Concurrency] = rowIndex
			groups[index].Rows = append(groups[index].Rows, emptyThroughputComparisonRow(row.Concurrency))
		}
		applyThroughputComparisonSource(&groups[index].Rows[rowIndex], row)
	}
	for index := range groups {
		groups[index].Title = ClaimTitle(claims[index].semantics, claims[index].target, claims[index].verified(), claims[index].fallback)
		sort.SliceStable(groups[index].Rows, func(i, j int) bool {
			return groups[index].Rows[i].Concurrency < groups[index].Rows[j].Concurrency
		})
		applyThroughputComparisonHeatmaps(groups[index].Rows)
		key := throughputGroupKey{profile: groups[index].Profile, contextLabel: groups[index].Title}
		groups[index].AxisItems = throughputGroupAxisItems(key, visibility, groups[index].ServerLimit, mismatchNotes[index], groups[index].Rows)
	}
	sort.SliceStable(groups, func(i, j int) bool {
		left, right := throughputGroupSortKey(groups[i]), throughputGroupSortKey(groups[j])
		return left < right
	})
	return groups
}

type throughputGroupClaim struct {
	semantics           string
	target              int
	fallback            string
	anyVerified         bool
	completedUnverified int
}

// verified requires every completed row to verify the claim; one completed
// row without confirmed token counts demotes the whole table.
func (claim throughputGroupClaim) verified() bool {
	return claim.anyVerified && claim.completedUnverified == 0
}

func emptyThroughputComparisonRow(concurrency int) SQLiteReportThroughputComparisonRow {
	return SQLiteReportThroughputComparisonRow{
		Concurrency:         concurrency,
		Baseline:            concurrency == 1,
		DecodeTokS:          "-",
		DecodePerUserTokS:   "-",
		DecodeTTFTMeanMS:    "-",
		DecodeTTFTMS:        "-",
		DecodeLatencyMS:     "-",
		DecodeShape:         "-",
		PrefillTokS:         "-",
		PrefillPerUserTokS:  "-",
		PrefillTTFTMeanMS:   "-",
		PrefillTTFTMS:       "-",
		PrefillLatencyMS:    "-",
		Requests:            "0 / 0",
		Result:              "0 / 0",
		PrefillShape:        "-",
		DecodeTokSHeat:      "heat-neutral",
		DecodeUserHeat:      "heat-neutral",
		DecodeTTFTMeanHeat:  "heat-neutral",
		DecodeTTFTHeat:      "heat-neutral",
		DecodeLatencyHeat:   "heat-neutral",
		PrefillTokSHeat:     "heat-neutral",
		PrefillUserHeat:     "heat-neutral",
		PrefillTTFTMeanHeat: "heat-neutral",
		PrefillTTFTHeat:     "heat-neutral",
		PrefillLatencyHeat:  "heat-neutral",
		ErrHeat:             "heat-neutral",
	}
}

func applyThroughputComparisonSource(target *SQLiteReportThroughputComparisonRow, source SQLiteReportThroughputRow) {
	switch source.Mode {
	case "prefill":
		target.PrefillTokS = source.ThroughputTokS
		target.PrefillPerUserTokS = source.PerUserTokS
		target.PrefillTTFTMeanMS = displayFailureMetric(source.TTFTMeanMS, source.FailureLabel)
		target.PrefillTTFTMS = displayFailureMetric(source.TTFTP99MS, source.FailureLabel)
		target.PrefillLatencyMS = source.LatencyP95MS
		target.PrefillOK = source.CompletedRequests
		target.PrefillErr = source.FailedRequests
		target.PrefillShape = source.Shape
		target.PrefillDetail = source.Detail
	case "decode":
		target.DecodeTokS = source.ThroughputTokS
		target.DecodePerUserTokS = source.PerUserTokS
		target.DecodeTTFTMeanMS = displayFailureMetric(source.TTFTMeanMS, source.FailureLabel)
		target.DecodeTTFTMS = displayFailureMetric(source.TTFTP99MS, source.FailureLabel)
		target.DecodeLatencyMS = source.LatencyP95MS
		target.DecodeOK = source.CompletedRequests
		target.DecodeErr = source.FailedRequests
		target.DecodeShape = source.Shape
		target.DecodeDetail = source.Detail
	default:
		target.DecodeTokS = source.ThroughputTokS
		target.DecodePerUserTokS = source.PerUserTokS
		target.DecodeTTFTMeanMS = displayFailureMetric(source.TTFTMeanMS, source.FailureLabel)
		target.DecodeTTFTMS = displayFailureMetric(source.TTFTP99MS, source.FailureLabel)
		target.DecodeLatencyMS = source.LatencyP95MS
		target.DecodeOK = source.CompletedRequests
		target.DecodeErr = source.FailedRequests
		target.DecodeShape = source.Shape
		target.DecodeDetail = source.Detail
	}
	target.OK = target.DecodeOK + target.PrefillOK
	target.Err = target.DecodeErr + target.PrefillErr
	target.Requests = fmt.Sprintf("%d / %d", target.OK, target.Err)
	target.Result, target.ResultDetail = comparisonResult(source, *target)
	if source.SLODisplay != "" {
		if source.Mode == "prefill" {
			target.PrefillSLO = source.SLODisplay
		} else {
			target.DecodeSLO = source.SLODisplay
		}
	}
	// Both phases can declare SLOs; neither result may silently overwrite
	// the other.
	switch {
	case target.DecodeSLO != "" && target.PrefillSLO != "":
		target.SLO = "D " + target.DecodeSLO + " · P " + target.PrefillSLO
	case target.DecodeSLO != "":
		target.SLO = target.DecodeSLO
	case target.PrefillSLO != "":
		target.SLO = target.PrefillSLO
	default:
		target.SLO = "-"
	}
}

func comparisonResult(source SQLiteReportThroughputRow, target SQLiteReportThroughputComparisonRow) (string, SQLiteReportCellDetail) {
	if source.FailureLabel == "" {
		if resultCarriesFailure(target.Result, target.ResultDetail) {
			return resultWithCurrentCounts(target.Result, target), target.ResultDetail
		}
		return fmt.Sprintf("%d / %d", target.OK, target.Err), source.Detail
	}
	prefix := strings.ToUpper(firstNonEmptyLetter(source.Mode))
	if prefix == "" {
		prefix = "Run"
	}
	label := prefix + " " + source.FailureLabel
	if target.OK > 0 || target.Err > 0 {
		label = fmt.Sprintf("%d / %d · %s", target.OK, target.Err, label)
	}
	return label, source.Detail
}

func resultCarriesFailure(result string, detail SQLiteReportCellDetail) bool {
	if strings.TrimSpace(detail.FailureLabel) != "" {
		return true
	}
	result = strings.TrimSpace(result)
	return result != "" && !isRequestCountLabel(result)
}

func isRequestCountLabel(value string) bool {
	parts := strings.Fields(strings.TrimSpace(value))
	if len(parts) != 3 || parts[1] != "/" {
		return false
	}
	if _, err := strconv.Atoi(parts[0]); err != nil {
		return false
	}
	if _, err := strconv.Atoi(parts[2]); err != nil {
		return false
	}
	return true
}

func resultWithCurrentCounts(existing string, target SQLiteReportThroughputComparisonRow) string {
	label := strings.TrimSpace(existing)
	if _, after, ok := strings.Cut(label, " · "); ok {
		label = strings.TrimSpace(after)
	}
	if target.OK == 0 && target.Err == 0 {
		return label
	}
	return fmt.Sprintf("%s · %s", target.Requests, label)
}

func firstNonEmptyLetter(value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return ""
	}
	return value[:1]
}

func throughputAxisVisibilityForRows(rows []SQLiteReportThroughputRow) throughputAxisVisibility {
	profiles := map[string]struct{}{}
	for _, row := range rows {
		if strings.TrimSpace(row.Profile) != "" {
			profiles[row.Profile] = struct{}{}
		}
	}
	return throughputAxisVisibility{
		profile: len(profiles) > 1,
	}
}

func throughputGroupAxisItems(key throughputGroupKey, visibility throughputAxisVisibility, serverLimit int, mismatchNote string, rows []SQLiteReportThroughputComparisonRow) []SQLiteReportMetadataItem {
	items := []SQLiteReportMetadataItem{}
	if visibility.profile && strings.TrimSpace(key.profile) != "" && key.profile != key.contextLabel {
		items = append(items, SQLiteReportMetadataItem{Label: "Profile", Value: key.profile})
	}
	if serverLimit > 0 {
		items = append(items, SQLiteReportMetadataItem{Label: "Server limit", Value: contextLabel(serverLimit)})
	}
	if mismatchNote != "" {
		items = append(items, SQLiteReportMetadataItem{Label: "Context mismatch", Value: mismatchNote})
	}
	if shape := comparisonShapeSummary(rows, "decode"); shape != "" {
		items = append(items, SQLiteReportMetadataItem{Label: "Decode", Value: shape})
	}
	if shape := comparisonShapeSummary(rows, "prefill"); shape != "" {
		items = append(items, SQLiteReportMetadataItem{Label: "Prefill", Value: shape})
	}
	return items
}

func comparisonShapeSummary(rows []SQLiteReportThroughputComparisonRow, mode string) string {
	values := make([]string, 0, len(rows))
	for _, row := range rows {
		value := row.DecodeShape
		if mode == "prefill" {
			value = row.PrefillShape
		}
		value = strings.TrimSpace(value)
		if value == "" || value == "-" {
			continue
		}
		values = append(values, value)
	}
	return joinUnique(values, " / ")
}

func throughputGroupSortKey(group SQLiteReportThroughputGroup) string {
	return fmt.Sprintf("%012d:%s:%s", group.ContextSortKey, group.Profile, group.Title)
}

type throughputComparisonHeatmapColumn struct {
	higherIsBetter bool
	value          func(SQLiteReportThroughputComparisonRow) (float64, bool)
	set            func(*SQLiteReportThroughputComparisonRow, string)
}

type comparisonHeatmapStats struct {
	values []float64
	valid  []bool
	min    float64
	max    float64
	count  int
}

type metricDisplayField struct {
	value string
	known bool
}

func applyThroughputComparisonHeatmaps(rows []SQLiteReportThroughputComparisonRow) {
	columns := []throughputComparisonHeatmapColumn{
		{
			higherIsBetter: true,
			value: func(row SQLiteReportThroughputComparisonRow) (float64, bool) {
				return parseDisplayedFloat(row.DecodeTokS)
			},
			set: func(row *SQLiteReportThroughputComparisonRow, class string) { row.DecodeTokSHeat = class },
		},
		{
			higherIsBetter: true,
			value: func(row SQLiteReportThroughputComparisonRow) (float64, bool) {
				return parseDisplayedFloat(row.DecodePerUserTokS)
			},
			set: func(row *SQLiteReportThroughputComparisonRow, class string) { row.DecodeUserHeat = class },
		},
		{
			higherIsBetter: false,
			value: func(row SQLiteReportThroughputComparisonRow) (float64, bool) {
				return parseDisplayedFloat(row.DecodeTTFTMeanMS)
			},
			set: func(row *SQLiteReportThroughputComparisonRow, class string) {
				row.DecodeTTFTMeanHeat = class
			},
		},
		{
			higherIsBetter: false,
			value: func(row SQLiteReportThroughputComparisonRow) (float64, bool) {
				return parseDisplayedFloat(row.DecodeTTFTMS)
			},
			set: func(row *SQLiteReportThroughputComparisonRow, class string) {
				row.DecodeTTFTHeat = class
			},
		},
		{
			higherIsBetter: false,
			value: func(row SQLiteReportThroughputComparisonRow) (float64, bool) {
				return parseDisplayedFloat(row.DecodeLatencyMS)
			},
			set: func(row *SQLiteReportThroughputComparisonRow, class string) { row.DecodeLatencyHeat = class },
		},
		{
			higherIsBetter: true,
			value: func(row SQLiteReportThroughputComparisonRow) (float64, bool) {
				return parseDisplayedFloat(row.PrefillTokS)
			},
			set: func(row *SQLiteReportThroughputComparisonRow, class string) { row.PrefillTokSHeat = class },
		},
		{
			higherIsBetter: true,
			value: func(row SQLiteReportThroughputComparisonRow) (float64, bool) {
				return parseDisplayedFloat(row.PrefillPerUserTokS)
			},
			set: func(row *SQLiteReportThroughputComparisonRow, class string) { row.PrefillUserHeat = class },
		},
		{
			higherIsBetter: false,
			value: func(row SQLiteReportThroughputComparisonRow) (float64, bool) {
				return parseDisplayedFloat(row.PrefillTTFTMeanMS)
			},
			set: func(row *SQLiteReportThroughputComparisonRow, class string) {
				row.PrefillTTFTMeanHeat = class
			},
		},
		{
			higherIsBetter: false,
			value: func(row SQLiteReportThroughputComparisonRow) (float64, bool) {
				return parseDisplayedFloat(row.PrefillTTFTMS)
			},
			set: func(row *SQLiteReportThroughputComparisonRow, class string) {
				row.PrefillTTFTHeat = class
			},
		},
		{
			higherIsBetter: false,
			value: func(row SQLiteReportThroughputComparisonRow) (float64, bool) {
				return parseDisplayedFloat(row.PrefillLatencyMS)
			},
			set: func(row *SQLiteReportThroughputComparisonRow, class string) { row.PrefillLatencyHeat = class },
		},
		{
			higherIsBetter: false,
			value:          func(row SQLiteReportThroughputComparisonRow) (float64, bool) { return float64(row.Err), true },
			set:            func(row *SQLiteReportThroughputComparisonRow, class string) { row.ErrHeat = class },
		},
	}
	for _, column := range columns {
		applyThroughputComparisonHeatmapColumn(rows, column)
	}
}

func applyThroughputComparisonHeatmapColumn(rows []SQLiteReportThroughputComparisonRow, column throughputComparisonHeatmapColumn) {
	stats := collectComparisonHeatmapStats(rows, column)
	if !stats.hasSpread() {
		setComparisonHeatmapClass(rows, column, "heat-neutral")
		return
	}
	for index := range rows {
		column.set(&rows[index], stats.class(index, column))
	}
}

func collectComparisonHeatmapStats(rows []SQLiteReportThroughputComparisonRow, column throughputComparisonHeatmapColumn) comparisonHeatmapStats {
	stats := comparisonHeatmapStats{
		values: make([]float64, len(rows)),
		valid:  make([]bool, len(rows)),
		min:    math.Inf(1),
		max:    math.Inf(-1),
	}
	for index, row := range rows {
		value, ok := column.value(row)
		if ok {
			stats.record(index, value)
		}
	}
	return stats
}

func (stats *comparisonHeatmapStats) record(index int, value float64) {
	stats.values[index] = value
	stats.valid[index] = true
	stats.count++
	stats.min = math.Min(stats.min, value)
	stats.max = math.Max(stats.max, value)
}

func (stats comparisonHeatmapStats) hasSpread() bool {
	return stats.count > 0 && stats.min != stats.max
}

func (stats comparisonHeatmapStats) class(index int, column throughputComparisonHeatmapColumn) string {
	if !stats.valid[index] {
		return "heat-neutral"
	}
	ratio := (stats.values[index] - stats.min) / (stats.max - stats.min)
	if !column.higherIsBetter {
		ratio = 1 - ratio
	}
	return heatClass(ratio)
}

func setComparisonHeatmapClass(rows []SQLiteReportThroughputComparisonRow, column throughputComparisonHeatmapColumn, class string) {
	for index := range rows {
		column.set(&rows[index], class)
	}
}

func heatClass(ratio float64) string {
	if ratio < 0 {
		ratio = 0
	}
	if ratio > 1 {
		ratio = 1
	}
	return fmt.Sprintf("heat-%d", int(math.Round(ratio*5)))
}

func profileKVDtypes(profiles []SQLiteReportProfile) []string {
	values := make([]string, 0, len(profiles))
	for _, profile := range profiles {
		values = append(values, profile.KVCacheDtype)
	}
	return values
}

func engineSummaries(engines []SQLiteReportEngine) []string {
	values := make([]string, 0, len(engines))
	for _, engine := range engines {
		name := bench.FirstNonEmpty(engine.Name, engine.Type, "engine")
		if engine.Version != "" {
			values = append(values, fmt.Sprintf("%s %s", name, engine.Version))
			continue
		}
		if engine.Command != "" {
			values = append(values, fmt.Sprintf("%s via %s", name, filepath.Base(engine.Command)))
			continue
		}
		values = append(values, name)
	}
	return values
}

func inferQuantization(profiles []SQLiteReportProfile) string {
	quantizations := []string{}
	for _, profile := range profiles {
		model := strings.ToLower(profile.Model)
		switch {
		case strings.Contains(model, "nvfp4"):
			quantizations = append(quantizations, "NVFP4")
		case strings.Contains(model, "fp8"):
			quantizations = append(quantizations, "FP8")
		case strings.Contains(model, "int4"):
			quantizations = append(quantizations, "INT4")
		case strings.Contains(model, "int8"):
			quantizations = append(quantizations, "INT8")
		}
	}
	return joinUnique(quantizations, ", ")
}

func measurementPositiveInts(measurements []SQLiteReportMeasurement, value func(SQLiteReportMeasurement) int) []int {
	values := make([]int, 0, len(measurements))
	for _, measurement := range measurements {
		current := value(measurement)
		if current > 0 {
			values = append(values, current)
		}
	}
	return uniqueSortedInts(values)
}

func requestShape(measurement SQLiteReportMeasurement) string {
	if measurement.CompletedRequests <= 0 || !measurement.PromptTokensKnown || !measurement.CompletionTokensKnown {
		return "-"
	}
	input := float64(measurement.PromptTokensValue) / float64(measurement.CompletedRequests)
	output := float64(measurement.CompletionTokensValue) / float64(measurement.CompletedRequests)
	return fmt.Sprintf("%s in / %s out", displayTokenCount(input), displayTokenCount(output))
}

func inputThroughput(measurement SQLiteReportMeasurement) string {
	if !measurement.PromptTokensKnown || !measurement.WallTimeMSKnown || measurement.WallTimeMSValue <= 0 {
		return "-"
	}
	return displayFloat(float64(measurement.PromptTokensValue) / (measurement.WallTimeMSValue / 1000))
}

func displayTokenCount(value float64) string {
	return fmt.Sprintf("%.0f", value)
}

func joinUnique(values []string, sep string) string {
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
	sort.Strings(out)
	return strings.Join(out, sep)
}

func uniqueSortedInts(values []int) []int {
	seen := map[int]struct{}{}
	out := []int{}
	for _, value := range values {
		if _, ok := seen[value]; ok {
			continue
		}
		seen[value] = struct{}{}
		out = append(out, value)
	}
	sort.Ints(out)
	return out
}

func formatContextList(values []int) string {
	if len(values) == 0 {
		return "-"
	}
	parts := make([]string, 0, len(values))
	for _, value := range values {
		if value%1024 == 0 {
			parts = append(parts, fmt.Sprintf("%dk", value/1024))
			continue
		}
		parts = append(parts, fmt.Sprintf("%d", value))
	}
	return strings.Join(parts, " / ")
}

func contextLabel(value int) string {
	if value > 0 && value%1024 == 0 {
		return fmt.Sprintf("%dk", value/1024)
	}
	return fmt.Sprintf("%d", value)
}

// Context labels for groups and rows come from applyContextLabel; the
// server limit (max_model_len) is never rendered as a context title. See
// docs/2026-07-02-context-semantics.md.

func formatIntList(values []int) string {
	if len(values) == 0 {
		return "-"
	}
	parts := make([]string, 0, len(values))
	for _, value := range values {
		parts = append(parts, fmt.Sprintf("%d", value))
	}
	return strings.Join(parts, " / ")
}

func metricDisplay(metrics map[string]SQLiteReportMetric, name, field string) string {
	return metricDisplayFirst(metrics, field, name)
}

func metricDisplayFirst(metrics map[string]SQLiteReportMetric, field string, names ...string) string {
	for _, name := range names {
		metric, ok := metricFirst(metrics, name)
		if !ok {
			continue
		}
		if value, ok := metricFieldDisplay(metric, field); ok {
			return value
		}
	}
	return "-"
}

func metricFirst(metrics map[string]SQLiteReportMetric, names ...string) (SQLiteReportMetric, bool) {
	for _, name := range names {
		metric, ok := metrics[name]
		if ok {
			return metric, true
		}
	}
	return SQLiteReportMetric{}, false
}

func metricFieldDisplay(metric SQLiteReportMetric, field string) (string, bool) {
	value, ok := metricDisplayFields(metric)[field]
	if !ok {
		value = metricDisplayField{value: metric.Mean, known: metric.MeanKnown}
	}
	return value.display()
}

func metricDisplayFields(metric SQLiteReportMetric) map[string]metricDisplayField {
	return map[string]metricDisplayField{
		"StdDev": {value: metric.StdDev, known: metric.StdDevKnown},
		"P50":    {value: metric.P50, known: metric.P50 != "-"},
		"P90":    {value: metric.P90, known: metric.P90 != "-"},
		"P95":    {value: metric.P95, known: metric.P95 != "-"},
		"P99":    {value: metric.P99, known: metric.P99 != "-"},
	}
}

func (field metricDisplayField) display() (string, bool) {
	if !field.known {
		return "", false
	}
	return field.value, true
}

func commandSummaryFromJSON(data string) string {
	var args []string
	if err := json.Unmarshal([]byte(data), &args); err != nil || len(args) == 0 {
		return ""
	}
	return bench.ShellQuote(redactedCommandArgs(args))
}

func redactedCommandArgs(args []string) []string {
	const redacted = "<redacted>"
	out := append([]string(nil), args...)
	redactNext := false
	for i, arg := range out {
		if redactNext {
			out[i] = redacted
			redactNext = false
			continue
		}
		name, hasName, hasInlineValue := commandArgName(arg)
		if !hasName || !isSensitiveCommandArgName(name) {
			continue
		}
		if hasInlineValue {
			out[i] = name + "=" + redacted
			continue
		}
		redactNext = true
	}
	return out
}

func commandArgName(arg string) (name string, ok, hasInlineValue bool) {
	if strings.HasPrefix(arg, "-") {
		name = arg
		if before, _, found := strings.Cut(arg, "="); found {
			name = before
			hasInlineValue = true
		}
		return name, true, hasInlineValue
	}
	if before, _, found := strings.Cut(arg, "="); found && isSensitiveCommandArgName(before) {
		return before, true, true
	}
	return "", false, false
}

func isSensitiveCommandArgName(name string) bool {
	normalized := strings.ToUpper(strings.TrimLeft(strings.TrimSpace(name), "-"))
	for _, part := range strings.FieldsFunc(normalized, isCommandArgSeparator) {
		switch part {
		case "AUTH", "AUTHORIZATION", "COOKIE", "CREDENTIAL", "CREDENTIALS", "HEADER", "HEADERS", "KEY", "PASS", "PASSWORD", "SECRET", "TOKEN":
			return true
		}
	}
	for _, marker := range []string{"APIKEY", "API_KEY", "ACCESS_TOKEN", "REFRESH_TOKEN", "CLIENT_SECRET"} {
		if strings.Contains(normalized, marker) {
			return true
		}
	}
	return false
}

func isCommandArgSeparator(r rune) bool {
	return !((r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9'))
}

func reportStatusClass(status string) string {
	switch strings.ToLower(strings.TrimSpace(status)) {
	case "completed":
		return "status-ok"
	case "failed", "canceled":
		return "status-bad"
	case "skipped":
		return "status-warn"
	default:
		return "status-neutral"
	}
}

func nullStringValue(value sql.NullString) string {
	if value.Valid {
		return value.String
	}
	return ""
}

func nullValue[T any](valid bool, value T) T {
	if valid {
		return value
	}
	var zero T
	return zero
}

func displayNullInt(value sql.NullInt64) string {
	if value.Valid {
		return fmt.Sprintf("%d", value.Int64)
	}
	return "-"
}

func displayNullFloat(value sql.NullFloat64) string {
	if value.Valid {
		return displayFloat(value.Float64)
	}
	return "-"
}

func displayFloat(value float64) string {
	return fmt.Sprintf("%.3f", value)
}

// FormatRateDisplay renders a rate (tok/s, req/s) at roughly three
// significant digits: >= 100 no decimals, 10-100 one decimal, < 10 two
// decimals. Handles "mean ± spread" composites; non-numeric strings
// (failure labels, "-") pass through untouched.
func FormatRateDisplay(value string) string {
	return formatDisplayParts(value, tokenThroughputMetric)
}

// FormatDurationDisplay renders a millisecond quantity with unit promotion:
// "321ms", "9.2s", "102s", "4m37s". Handles "mean ± spread"
// composites; non-numeric strings pass through untouched.
func FormatDurationDisplay(value string) string {
	return formatDisplayParts(value, compactMilliseconds)
}

// formatDisplayParts applies a numeric formatter to each part of a
// "mean ± spread" composite, or to the whole value when it is plain.
func formatDisplayParts(value string, format func(string) string) string {
	parts := strings.Split(value, "±")
	for index, part := range parts {
		parts[index] = format(strings.TrimSpace(part))
	}
	return strings.Join(parts, " ± ")
}

func tokenThroughputMetric(value string) string {
	parsed, ok := parseDisplayedFloat(value)
	if !ok {
		return value
	}
	abs := math.Abs(parsed)
	switch {
	case math.Round(abs*10)/10 >= 100:
		return fmt.Sprintf("%.0f", parsed)
	case math.Round(abs*100)/100 >= 10:
		return fmt.Sprintf("%.1f", parsed)
	default:
		return fmt.Sprintf("%.2f", parsed)
	}
}

func compactMilliseconds(value string) string {
	parsed, ok := parseDisplayedFloat(value)
	if !ok {
		return value
	}
	sign := ""
	if parsed < 0 {
		sign = "-"
	}
	absMS := math.Abs(parsed)
	switch {
	case absMS < 1000:
		return fmt.Sprintf("%s%.0fms", sign, absMS)
	case absMS < 10_000:
		return sign + trimTrailingZero(fmt.Sprintf("%.1fs", absMS/1000))
	default:
		totalSeconds := int(math.Round(absMS / 1000))
		if totalSeconds < 60 {
			return fmt.Sprintf("%s%ds", sign, totalSeconds)
		}
		return fmt.Sprintf("%s%dm%02ds", sign, totalSeconds/60, totalSeconds%60)
	}
}

func parseDisplayedFloat(value string) (float64, bool) {
	value = strings.TrimSpace(value)
	if value == "" || value == "-" {
		return 0, false
	}
	parsed, err := strconv.ParseFloat(value, 64)
	return parsed, err == nil
}

func trimTrailingZero(value string) string {
	suffix := ""
	if len(value) > 0 {
		last := value[len(value)-1]
		if (last >= 'a' && last <= 'z') || (last >= 'A' && last <= 'Z') {
			suffix = value[len(value)-1:]
			value = value[:len(value)-1]
		}
	}
	if strings.Contains(value, ".") {
		value = strings.TrimRight(value, "0")
		value = strings.TrimSuffix(value, ".")
	}
	return value + suffix
}
