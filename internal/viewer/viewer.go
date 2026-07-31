package viewer

import (
	"context"
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/osolmaz/localperf/internal/report"
	"github.com/osolmaz/localperf/internal/reportmodel"
)

const defaultAddr = "127.0.0.1:0"

type ServerConfig struct {
	Addr        string
	Title       string
	Paths       []string
	OpenBrowser bool
	Out         io.Writer
	Err         io.Writer
}

type HandlerConfig struct {
	Title string
	Paths []string
}

type Manifest struct {
	Title   string          `json:"title"`
	Reports []ReportSummary `json:"reports"`
}

type ReportSummary struct {
	ID               string `json:"id"`
	Label            string `json:"label"`
	Path             string `json:"path"`
	LatestRunID      string `json:"latest_run_id"`
	LatestRunName    string `json:"latest_run_name"`
	LatestRunStatus  string `json:"latest_run_status"`
	RunCount         int    `json:"run_count"`
	MeasurementCount int    `json:"measurement_count"`
	Engine           string `json:"engine,omitempty"`
}

type Handler struct {
	title    string
	reports  []loadedReport
	manifest Manifest
	byID     map[string]*loadedReport
	mux      *http.ServeMux
}

type loadedReport struct {
	ReportSummary
	model reportmodel.Document
	html  string
}

//go:embed web/dist/*
//go:embed web/dist/assets/*
var webAssets embed.FS

func NewHandler(config HandlerConfig) (*Handler, error) {
	title := strings.TrimSpace(config.Title)
	if title == "" {
		title = "localperf viewer"
	}
	reports, err := loadReports(config.Paths)
	if err != nil {
		return nil, err
	}
	summaries := make([]ReportSummary, 0, len(reports))
	for _, item := range reports {
		summaries = append(summaries, item.ReportSummary)
	}
	handler := &Handler{
		title:   title,
		reports: reports,
		manifest: Manifest{
			Title:   title,
			Reports: summaries,
		},
		byID: map[string]*loadedReport{},
	}
	for index := range reports {
		report := &reports[index]
		handler.byID[report.ID] = report
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/reports", handler.handleManifest)
	mux.HandleFunc("GET /api/reports/{id}/summary", handler.reportJSONHandler(func(report *loadedReport) any {
		return report.model.Summary
	}))
	mux.HandleFunc("GET /api/reports/{id}/throughput", handler.reportJSONHandler(func(report *loadedReport) any {
		return report.model.Throughput
	}))
	mux.HandleFunc("GET /api/reports/{id}/measurements/{measurement_id}", handler.handleMeasurementDetail)
	mux.HandleFunc("GET /report/{id}", handler.handleReport)
	mux.Handle("GET /assets/", http.StripPrefix("/", http.FileServer(http.FS(staticFS()))))
	mux.HandleFunc("GET /", handler.handleApp)
	handler.mux = mux
	return handler, nil
}

func (handler *Handler) ServeHTTP(writer http.ResponseWriter, request *http.Request) {
	handler.mux.ServeHTTP(writer, request)
}

func (handler *Handler) Manifest() Manifest {
	return handler.manifest
}

func Serve(ctx context.Context, config ServerConfig) error {
	handler, err := NewHandler(HandlerConfig{Title: config.Title, Paths: config.Paths})
	if err != nil {
		return err
	}
	addr := strings.TrimSpace(config.Addr)
	if addr == "" {
		addr = defaultAddr
	}
	listener, err := net.Listen("tcp", addr)
	if err != nil {
		return err
	}
	server := &http.Server{Handler: handler}
	errCh := make(chan error, 1)
	go func() {
		if err := server.Serve(listener); err != nil && err != http.ErrServerClosed {
			errCh <- err
			return
		}
		errCh <- nil
	}()
	out := config.Out
	if out == nil {
		out = io.Discard
	}
	errOut := config.Err
	if errOut == nil {
		errOut = io.Discard
	}
	url := displayURL(listener.Addr())
	fmt.Fprintf(out, "viewer: %s\n", url)
	fmt.Fprintln(out, "press Ctrl+C to stop")
	if config.OpenBrowser {
		if err := openBrowser(url); err != nil {
			fmt.Fprintf(errOut, "could not open browser: %v\n", err)
		}
	}
	select {
	case err := <-errCh:
		return err
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := server.Shutdown(shutdownCtx); err != nil {
			_ = listener.Close()
			return err
		}
		return <-errCh
	}
}

func loadReports(paths []string) ([]loadedReport, error) {
	if len(paths) == 0 {
		return nil, fmt.Errorf("missing SQLite report path")
	}
	reports := make([]loadedReport, 0, len(paths))
	for index, path := range paths {
		absolute, err := filepath.Abs(path)
		if err != nil {
			return nil, err
		}
		info, err := os.Stat(absolute)
		if err != nil {
			return nil, err
		}
		if info.IsDir() {
			return nil, fmt.Errorf("%s is a directory, want SQLite report file", absolute)
		}
		doc, err := report.LoadSQLiteReport(absolute)
		if err != nil {
			return nil, fmt.Errorf("load %s: %w", absolute, err)
		}
		model := reportmodel.Build(absolute, doc)
		var html strings.Builder
		if err := report.RenderHTMLReport(&html, doc, report.HTMLReportOptions{}); err != nil {
			return nil, fmt.Errorf("render %s: %w", absolute, err)
		}
		reports = append(reports, loadedReport{
			ReportSummary: summarizeReport(index, absolute, doc),
			model:         model,
			html:          html.String(),
		})
	}
	return reports, nil
}

func summarizeReport(index int, path string, doc report.SQLiteReportDocument) ReportSummary {
	return ReportSummary{
		ID:               reportID(index, path),
		Label:            reportLabel(path),
		Path:             path,
		LatestRunID:      doc.Run.ID,
		LatestRunName:    doc.Run.Name,
		LatestRunStatus:  doc.Run.Status,
		RunCount:         len(doc.Runs),
		MeasurementCount: len(doc.Measurements),
		Engine:           reportEngine(doc),
	}
}

// reportEngine summarizes the artifact's engine identity so the app can
// flag cross-report engine differences before anyone compares tabs.
func reportEngine(doc report.SQLiteReportDocument) string {
	for _, item := range doc.MetadataItems {
		if item.Label == "Engine" {
			return item.Value
		}
	}
	return ""
}

func reportID(index int, path string) string {
	hash := sha256.Sum256([]byte(filepath.Clean(path)))
	return fmt.Sprintf("%02d-%s-%s", index+1, slug(filepath.Base(path)), hex.EncodeToString(hash[:])[:8])
}

func reportLabel(path string) string {
	base := filepath.Base(path)
	extension := filepath.Ext(base)
	label := strings.TrimSuffix(base, extension)
	for _, value := range []string{label, base, path} {
		if strings.TrimSpace(value) != "" {
			return value
		}
	}
	return ""
}

func slug(value string) string {
	value = strings.TrimSuffix(strings.ToLower(value), strings.ToLower(filepath.Ext(value)))
	var builder strings.Builder
	lastDash := false
	for _, r := range value {
		switch {
		case unicode.IsLetter(r), unicode.IsDigit(r):
			builder.WriteRune(r)
			lastDash = false
		case !lastDash:
			builder.WriteByte('-')
			lastDash = true
		}
	}
	out := strings.Trim(builder.String(), "-")
	if out == "" {
		return "report"
	}
	return out
}

func (handler *Handler) handleApp(writer http.ResponseWriter, request *http.Request) {
	if request.URL.Path != "/" && strings.HasPrefix(request.URL.Path, "/api/") {
		http.NotFound(writer, request)
		return
	}
	writer.Header().Set("Content-Type", "text/html; charset=utf-8")
	content, err := webAssets.ReadFile("web/dist/index.html")
	if err != nil {
		http.Error(writer, "viewer app is not built", http.StatusInternalServerError)
		return
	}
	_, _ = writer.Write(content)
}

func staticFS() fs.FS {
	out, err := fs.Sub(webAssets, "web/dist")
	if err != nil {
		panic(err)
	}
	return out
}

func (handler *Handler) handleReport(writer http.ResponseWriter, request *http.Request) {
	report, ok := handler.byID[request.PathValue("id")]
	if !ok {
		http.NotFound(writer, request)
		return
	}
	writer.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = io.WriteString(writer, report.html)
}

func (handler *Handler) handleManifest(writer http.ResponseWriter, request *http.Request) {
	writeJSON(writer, handler.manifest)
}

func (handler *Handler) reportJSONHandler(selectValue func(*loadedReport) any) http.HandlerFunc {
	return func(writer http.ResponseWriter, request *http.Request) {
		report, ok := handler.byID[request.PathValue("id")]
		if !ok {
			http.NotFound(writer, request)
			return
		}
		writeJSON(writer, selectValue(report))
	}
}

func (handler *Handler) handleMeasurementDetail(writer http.ResponseWriter, request *http.Request) {
	report, ok := handler.byID[request.PathValue("id")]
	if !ok {
		http.NotFound(writer, request)
		return
	}
	measurementID, err := strconv.ParseInt(request.PathValue("measurement_id"), 10, 64)
	if err != nil {
		http.NotFound(writer, request)
		return
	}
	detail, ok := report.model.Details[measurementID]
	if !ok {
		http.NotFound(writer, request)
		return
	}
	writeJSON(writer, detail)
}

func writeJSON(writer http.ResponseWriter, value any) {
	writer.Header().Set("Content-Type", "application/json; charset=utf-8")
	encoder := json.NewEncoder(writer)
	encoder.SetIndent("", "  ")
	if err := encoder.Encode(value); err != nil {
		http.Error(writer, err.Error(), http.StatusInternalServerError)
	}
}

func displayURL(addr net.Addr) string {
	tcpAddr, ok := addr.(*net.TCPAddr)
	if !ok {
		return "http://" + addr.String()
	}
	host := tcpAddr.IP.String()
	if tcpAddr.IP == nil || tcpAddr.IP.IsUnspecified() {
		host = "127.0.0.1"
	}
	return "http://" + net.JoinHostPort(host, strconv.Itoa(tcpAddr.Port))
}

func openBrowser(url string) error {
	switch runtime.GOOS {
	case "darwin":
		return exec.Command("open", url).Start()
	case "windows":
		return exec.Command("rundll32", "url.dll,FileProtocolHandler", url).Start()
	default:
		return exec.Command("xdg-open", url).Start()
	}
}
