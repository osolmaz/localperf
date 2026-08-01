package benchcli

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/osolmaz/localperf/internal/artifact"
	"github.com/osolmaz/localperf/internal/benchmarkconfig"
	"github.com/osolmaz/localperf/internal/collections"
	"github.com/osolmaz/localperf/internal/report"
	"github.com/osolmaz/localperf/internal/viewer"
	"github.com/osolmaz/localperf/internal/vllmbench"
)

type commandHandlers map[string]func([]string)

var rootHandlers = commandHandlers{
	"bench":    BenchMain,
	"artifact": runArtifact,
	"view":     runView,
}

var benchHandlers = commandHandlers{
	"run": runBench,
}

var artifactHandlers = commandHandlers{
	"check":  runArtifactCheck,
	"render": runArtifactRender,
	"merge":  runArtifactMerge,
}

// runArtifactMerge combines run artifacts into one model-level SQLite file;
// see docs/2026-07-02-default-inference-sweep.md, Model-Level Artifacts.
func runArtifactMerge(args []string) {
	flags := flag.NewFlagSet("artifact merge", flag.ExitOnError)
	into := flags.String("into", "", "destination model-level artifact (required)")
	_ = flags.Parse(args)
	sources := flags.Args()
	if strings.TrimSpace(*into) == "" || len(sources) == 0 {
		fmt.Fprintln(os.Stderr, "usage: localperf artifact merge --into runs/models/model.sqlite src1.sqlite [src2.sqlite ...]")
		os.Exit(2)
	}
	summary, err := artifact.Merge(*into, sources)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	for _, run := range summary.MergedRuns {
		fmt.Printf("merged: %s\n", run)
	}
	for _, run := range summary.SkippedRuns {
		fmt.Printf("skipped (already present): %s\n", run)
	}
	fmt.Printf("artifact ok: %s\n", *into)
}

func BenchMain(args []string) {
	dispatchCommand(args, usage, benchHandlers)
}

func Main(args []string) {
	dispatchCommand(args, usageRoot, rootHandlers)
}

func dispatchCommand(args []string, usageFunc func(), handlers commandHandlers) {
	if len(args) < 1 {
		usageFunc()
		os.Exit(2)
	}
	if handler := handlers[args[0]]; handler != nil {
		handler(args[1:])
		return
	}
	usageFunc()
	os.Exit(2)
}

func runBench(args []string) {
	flags := flag.NewFlagSet("run", flag.ExitOnError)
	suiteSource := flags.String("suite", "", "built-in suite name or suite JSON file")
	deploymentPath := flags.String("deployment", "", "deployment JSON file")
	runDir := flags.String("run-dir", "", "optional run directory")
	artifactPath := flags.String("artifact", "", "optional artifact path; an existing artifact is appended to (model-level accumulation)")
	resume := flags.Bool("resume", false, "skip planned runs whose result files already completed; requires --run-dir of the previous attempt")
	dryRun := flags.Bool("dry-run", false, "write planned artifacts without launching vLLM or benchmark commands")
	timeout := flags.Duration("timeout", 0, "optional overall timeout, for example 2h")
	var cases stringList
	var concurrencies intList
	flags.Var(&cases, "case", "case name to include; may be repeated")
	flags.Var(&concurrencies, "concurrency", "concurrency value to include; may be repeated")
	_ = flags.Parse(args)
	if flags.NArg() != 0 {
		fmt.Fprintf(os.Stderr, "unexpected positional arguments: %s\n", strings.Join(flags.Args(), " "))
		os.Exit(2)
	}
	suite, err := benchmarkconfig.LoadSuite(*suiteSource)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
	deployment, err := benchmarkconfig.LoadDeployment(*deploymentPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
	compiled, err := benchmarkconfig.Compile(suite, deployment, benchmarkconfig.Selection{Cases: cases, Concurrencies: concurrencies})
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	dir := vllmbench.RunDir(*runDir, compiled.Spec, time.Now())
	if *resume {
		if err := benchmarkconfig.VerifyExecutionFiles(dir, compiled); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
	}
	if err := benchmarkconfig.WriteExecutionFiles(dir, compiled); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	fmt.Printf("suite: %s\n", suite.Name)
	fmt.Printf("deployment: %s\n", deployment.Name)
	ctx := context.Background()
	cancel := func() {}
	if *timeout > 0 {
		ctx, cancel = context.WithTimeout(ctx, *timeout)
	}
	defer cancel()
	summary, err := vllmbench.Execute(ctx, compiled.Spec, vllmbench.RunOptions{
		RunDir: dir, ArtifactPath: *artifactPath, DryRun: *dryRun, Resume: *resume,
	})
	fmt.Printf("run dir: %s\n", summary.RunDir)
	fmt.Printf("planned: %d completed: %d failed: %d skipped: %d\n", summary.PlannedRuns, summary.CompletedRuns, summary.FailedRuns, summary.SkippedRuns)
	if summary.ArtifactPath != "" {
		fmt.Printf("artifact: %s\n", summary.ArtifactPath)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func runView(args []string) {
	config, err := parseViewFlags(args, flag.ExitOnError)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	err = viewer.Serve(ctx, viewer.ServerConfig{
		Addr:        config.addr,
		Title:       config.title,
		Paths:       config.paths,
		OpenBrowser: config.open,
		Out:         os.Stdout,
		Err:         os.Stderr,
	})
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

type viewConfig struct {
	addr  string
	title string
	paths []string
	open  bool
}

func parseViewFlags(args []string, errorHandling flag.ErrorHandling) (viewConfig, error) {
	flags := flag.NewFlagSet("view", errorHandling)
	addr := flags.String("addr", "127.0.0.1:0", "viewer listen address")
	title := flags.String("title", "localperf viewer", "viewer title")
	open := flags.Bool("open", false, "open the viewer in a browser")
	noOpen := flags.Bool("no-open", false, "do not open the viewer in a browser")
	positionalPaths, parseArgs := viewParseArgs(args)
	if err := flags.Parse(parseArgs); err != nil {
		return viewConfig{}, err
	}
	paths := append([]string{}, positionalPaths...)
	paths = append(paths, flags.Args()...)
	if len(paths) == 0 {
		return viewConfig{}, fmt.Errorf("missing SQLite report path")
	}
	if *open && *noOpen {
		return viewConfig{}, fmt.Errorf("--open and --no-open cannot both be set")
	}
	return viewConfig{
		addr:  *addr,
		title: *title,
		paths: paths,
		open:  *open && !*noOpen,
	}, nil
}

func viewParseArgs(args []string) ([]string, []string) {
	var paths []string
	parseArgs := make([]string, 0, len(args))
	for index := 0; index < len(args); index++ {
		arg := args[index]
		if !strings.HasPrefix(arg, "-") {
			paths = append(paths, arg)
			continue
		}
		parseArgs = append(parseArgs, arg)
		if viewFlagNeedsValue(arg) && !strings.Contains(arg, "=") && index+1 < len(args) {
			index++
			parseArgs = append(parseArgs, args[index])
		}
	}
	return paths, parseArgs
}

func viewFlagNeedsValue(arg string) bool {
	if equals := strings.Index(arg, "="); equals >= 0 {
		arg = arg[:equals]
	}
	switch arg {
	case "-addr", "--addr", "-title", "--title":
		return true
	default:
		return false
	}
}

func runArtifact(args []string) {
	dispatchCommand(args, usage, artifactHandlers)
}

func runArtifactCheck(args []string) {
	flags := flag.NewFlagSet("artifact check", flag.ExitOnError)
	path := flags.String("path", "", "SQLite artifact path")
	_ = flags.Parse(args)
	if strings.TrimSpace(*path) == "" {
		if flags.NArg() == 1 {
			*path = flags.Arg(0)
		}
	}
	if strings.TrimSpace(*path) == "" {
		fmt.Fprintln(os.Stderr, "missing artifact path")
		os.Exit(2)
	}
	if err := artifact.Check(*path); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	fmt.Printf("artifact ok: %s\n", *path)
}

func runArtifactRender(args []string) {
	config, err := parseArtifactRenderFlags(args, flag.ExitOnError)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
	if config.includeRaw {
		fmt.Fprintln(os.Stderr, "--include-raw is not implemented yet")
		os.Exit(2)
	}
	if err := report.WriteSQLiteHTMLReport(config.path, config.output, report.HTMLReportOptions{Title: config.title, Store: config.store}); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	outPath := config.output
	if strings.TrimSpace(outPath) == "" {
		outPath = strings.TrimSuffix(config.path, filepath.Ext(config.path)) + ".html"
	}
	fmt.Printf("html: %s\n", outPath)
	if config.store {
		fmt.Printf("stored: %s\n", config.path)
	}
}

type artifactRenderConfig struct {
	path       string
	output     string
	title      string
	store      bool
	includeRaw bool
}

func parseArtifactRenderFlags(args []string, errorHandling flag.ErrorHandling) (artifactRenderConfig, error) {
	flags := flag.NewFlagSet("artifact render", errorHandling)
	path := flags.String("path", "", "SQLite artifact path")
	output := flags.String("output", "", "standalone HTML output path; defaults beside the artifact")
	title := flags.String("title", "", "optional report title")
	store := flags.Bool("store", false, "store report.html back into the SQLite artifact")
	includeRaw := flags.Bool("include-raw", false, "reserved for explicit raw artifact rendering")
	positionalPath, parseArgs := artifactRenderParseArgs(args)
	if err := flags.Parse(parseArgs); err != nil {
		return artifactRenderConfig{}, err
	}
	if strings.TrimSpace(*path) == "" && positionalPath != "" {
		*path = positionalPath
	}
	if strings.TrimSpace(*path) == "" && flags.NArg() > 0 {
		*path = flags.Arg(0)
	}
	if strings.TrimSpace(*path) == "" {
		return artifactRenderConfig{}, fmt.Errorf("missing artifact path")
	}
	return artifactRenderConfig{
		path:       *path,
		output:     *output,
		title:      *title,
		store:      *store,
		includeRaw: *includeRaw,
	}, nil
}

func artifactRenderParseArgs(args []string) (string, []string) {
	positionalPath := ""
	parseArgs := make([]string, 0, len(args))
	for index := 0; index < len(args); index++ {
		arg := args[index]
		if positionalPath == "" && !strings.HasPrefix(arg, "-") {
			positionalPath = arg
			continue
		}
		parseArgs = append(parseArgs, arg)
		if artifactRenderFlagNeedsValue(arg) && !strings.Contains(arg, "=") && index+1 < len(args) {
			index++
			parseArgs = append(parseArgs, args[index])
		}
	}
	return positionalPath, parseArgs
}

func artifactRenderFlagNeedsValue(arg string) bool {
	if equals := strings.Index(arg, "="); equals >= 0 {
		arg = arg[:equals]
	}
	switch arg {
	case "-path", "--path", "-output", "--output", "-title", "--title":
		return true
	default:
		return false
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, `usage:
  localperf bench run --suite practical-64k --deployment deployment.json [--case generate-full] [--concurrency 1] [--dry-run] [--timeout 2h]

  built-in suites: practical-64k, throughput-4k, context-ladder`)
}

func usageRoot() {
	fmt.Fprintln(os.Stderr, `usage:
  localperf bench run --suite practical-64k --deployment deployment.json [--case generate-full] [--concurrency 1] [--dry-run] [--timeout 2h]
  localperf artifact check runs/example.sqlite
  localperf artifact render runs/example.sqlite [--output runs/example.html] [--store]
  localperf artifact merge --into runs/models/model.sqlite src1.sqlite [src2.sqlite ...]
  localperf view runs/model.sqlite [runs/other.sqlite ...] [--addr 127.0.0.1:0] [--open]`)
}

type stringList []string

func (values *stringList) String() string {
	return strings.Join(*values, ",")
}

func (values *stringList) Set(raw string) error {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return fmt.Errorf("empty value")
	}
	*values = append(*values, raw)
	return nil
}

type intList = collections.PositiveIntList
