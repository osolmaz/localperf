package benchcli

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/osolmaz/localperf/internal/artifact"
	"github.com/osolmaz/localperf/internal/collections"
	"github.com/osolmaz/localperf/internal/report"
	"github.com/osolmaz/localperf/internal/sweepplan"
	"github.com/osolmaz/localperf/internal/viewer"
	"github.com/osolmaz/localperf/internal/vllmbench"
)

type commandHandlers map[string]func([]string)

var rootHandlers = commandHandlers{
	"bench":    BenchMain,
	"artifact": runArtifact,
	"sweep":    runSweep,
	"view":     runView,
}

var benchHandlers = commandHandlers{
	"plan": runPlan,
	"run":  runBench,
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

var sweepHandlers = commandHandlers{
	"plan": runSweepPlan,
}

func runSweep(args []string) {
	dispatchCommand(args, usageRoot, sweepHandlers)
}

// runSweepPlan emits the default context/concurrency sweep spec with
// contract-compliant shapes and declared context semantics; see
// docs/2026-07-02-default-inference-sweep.md.
func runSweepPlan(args []string) {
	flags := flag.NewFlagSet("sweep plan", flag.ExitOnError)
	model := flags.String("model", "", "model identifier to benchmark (required)")
	contexts := flags.String("contexts", "4k,8k,16k,32k,64k", "comma-separated active-context ladder (e.g. 4k,8k,16k,32k); 128k and above are capped at c4")
	concurrency := flags.String("concurrency", "1,4,8,16,32", "comma-separated concurrency levels")
	repeats := flags.Int("repeats", 1, "repeats per measurement")
	numPrompts := flags.Int("num-prompts", 0, "fixed prompts per measurement; default scales with concurrency")
	promptsPerUser := flags.Int("prompts-per-user", 0, "prompts per concurrent user (default 2, floor 8 per point)")
	reference := flags.Bool("reference", true, "include the 4k max-throughput-reference capacity family")
	stress := flags.Bool("stress", false, "add long-output decode spot checks (4096 tokens at 32k c4, 64k c1/c4) and the 128k points")
	memFloor := flags.Float64("min-mem-available-gib", 0, "safety memory floor in GiB (default 40)")
	vllmCommand := flags.String("vllm-command", "", "vllm executable for managed serves (machine-specific runtime path)")
	gpuMemUtil := flags.Float64("gpu-memory-utilization", 0, "gpu memory utilization applied to every profile")
	kvCacheBytes := flags.Int64("kv-cache-memory-bytes", 0, "pin vLLM KV cache size on every profile")
	var trims trimFlags
	flags.Var(&trims, "trim", "cap a context's ladder with a reason, e.g. 64k=8:'12 GiB KV budget'; repeatable")
	out := flags.String("out", "", "output spec path (default stdout)")
	var profileArgs repeatedStringFlag
	var profileEngineArgs repeatedStringFlag
	var omitProfileEngineFlags repeatedStringFlag
	flags.Var(&profileArgs, "profile-arg", "extra generated profile arg; repeat once per arg")
	flags.Var(&profileEngineArgs, "profile-engine-arg", "extra generated profile engine arg; repeat once per arg")
	flags.Var(&omitProfileEngineFlags, "omit-profile-engine-flag", "engine flag to remove from generated profile engine args; repeatable")
	_ = flags.Parse(args)
	contextValues, err := parseTokenList(*contexts)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
	concurrencyValues, err := parseIntList(*concurrency)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
	spec, err := sweepplan.Plan(sweepplan.PlanRequest{
		Model:                  *model,
		Contexts:               contextValues,
		Concurrency:            concurrencyValues,
		Repeats:                *repeats,
		NumPrompts:             *numPrompts,
		PromptsPerUser:         *promptsPerUser,
		IncludeReference:       *reference,
		IncludeStress:          *stress,
		ProfileArgs:            profileArgs.values,
		ProfileEngineArgs:      profileEngineArgs.values,
		OmitProfileEngineFlags: omitProfileEngineFlags.values,
		MinMemAvailableGiB:     *memFloor,
		VLLMCommand:            *vllmCommand,
		GPUMemoryUtilization:   *gpuMemUtil,
		KVCacheMemoryBytes:     *kvCacheBytes,
		Trims:                  trims.values,
	})
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	encoded, err := json.MarshalIndent(spec, "", "  ")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	encoded = append(encoded, '\n')
	if strings.TrimSpace(*out) == "" {
		_, _ = os.Stdout.Write(encoded)
		return
	}
	if err := os.WriteFile(*out, encoded, 0o644); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	fmt.Printf("spec: %s\n", *out)
}

// trimFlags parses repeatable --trim values of the form
// "<context>=<max-concurrency>:<reason>", e.g. 64k=8:'12 GiB KV budget'.
type trimFlags struct {
	values []vllmbench.LadderTrim
}

func (flags *trimFlags) String() string {
	parts := make([]string, 0, len(flags.values))
	for _, trim := range flags.values {
		parts = append(parts, fmt.Sprintf("%d=%d:%s", trim.Context, trim.MaxConcurrency, trim.Reason))
	}
	return strings.Join(parts, ",")
}

func (flags *trimFlags) Set(value string) error {
	context, rest, ok := strings.Cut(value, "=")
	if !ok {
		return fmt.Errorf("trim %q: want <context>=<max-concurrency>:<reason>", value)
	}
	maxPart, reason, ok := strings.Cut(rest, ":")
	if !ok || strings.TrimSpace(reason) == "" {
		return fmt.Errorf("trim %q: a reason after ':' is required", value)
	}
	contextValues, err := parseTokenList(context)
	if err != nil || len(contextValues) != 1 {
		return fmt.Errorf("trim %q: want one context like 64k or 65536", value)
	}
	contextTokens := contextValues[0]
	maxConcurrency, err := strconv.Atoi(strings.TrimSpace(maxPart))
	if err != nil || maxConcurrency <= 0 {
		return fmt.Errorf("trim %q: max concurrency must be a positive integer", value)
	}
	flags.values = append(flags.values, vllmbench.LadderTrim{
		Context:        contextTokens,
		MaxConcurrency: maxConcurrency,
		Reason:         strings.TrimSpace(reason),
	})
	return nil
}

type repeatedStringFlag struct {
	values []string
}

func (flag *repeatedStringFlag) Set(value string) error {
	flag.values = append(flag.values, value)
	return nil
}

func (flag *repeatedStringFlag) String() string {
	return strings.Join(flag.values, ",")
}

// parseTokenList parses values such as "4k,8k,32768" into token counts.
func parseTokenList(value string) ([]int, error) {
	var values []int
	for _, part := range strings.Split(value, ",") {
		part = strings.TrimSpace(strings.ToLower(part))
		if part == "" {
			continue
		}
		multiplier := 1
		if strings.HasSuffix(part, "k") {
			multiplier = 1024
			part = strings.TrimSuffix(part, "k")
		}
		parsed, err := strconv.Atoi(part)
		if err != nil {
			return nil, fmt.Errorf("invalid context value %q", part)
		}
		values = append(values, parsed*multiplier)
	}
	return values, nil
}

func parseIntList(value string) ([]int, error) {
	var values []int
	for _, part := range strings.Split(value, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		parsed, err := strconv.Atoi(part)
		if err != nil {
			return nil, fmt.Errorf("invalid concurrency value %q", part)
		}
		values = append(values, parsed)
	}
	return values, nil
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

func runPlan(args []string) {
	flags := flag.NewFlagSet("plan", flag.ExitOnError)
	specPath := flags.String("spec", "", "benchmark spec JSON file")
	runDir := flags.String("run-dir", "", "optional run directory for result path planning")
	jsonOutput := flags.Bool("json", false, "print JSON instead of text")
	filterFlags := addFilterFlags(flags)
	_ = flags.Parse(args)
	spec := mustLoadSpec(*specPath, filterFlags.Filter())
	dir := vllmbench.RunDir(*runDir, spec, time.Now())
	if err := vllmbench.PrepareDatasets(context.Background(), &spec, dir); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	vllmbench.ApplyDefaults(&spec)
	if err := vllmbench.ValidateSpec(spec); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	plan := vllmbench.BuildPlan(spec, dir)
	if *jsonOutput {
		encoder := json.NewEncoder(os.Stdout)
		encoder.SetIndent("", "  ")
		_ = encoder.Encode(plan)
		return
	}
	fmt.Printf("name: %s\n", spec.Name)
	fmt.Printf("model: %s\n", spec.Model)
	fmt.Printf("run dir: %s\n", dir)
	fmt.Printf("memory floor: %.1f GiB MemAvailable\n", spec.Safety.MinMemAvailableGiB)
	fmt.Printf("planned runs: %d\n", len(plan))
	fmt.Println("profiles:")
	for _, profile := range spec.Profiles {
		fmt.Printf("- profile=%s managed=%t sleep=%t port=%d\n", profile.Name, profile.Managed, profile.EnableSleepMode, profile.Port)
		if profile.Managed {
			fmt.Printf("  %s\n", vllmbench.CommandSummary(vllmbench.ServeCommand(spec, profile)))
		}
	}
	fmt.Println("workloads:")
	for _, planned := range plan {
		command := vllmbench.LoadCommand(spec, planned)
		fmt.Printf("- profile=%s workload=%s concurrency=%d result=%s\n", planned.Profile.Name, planned.Workload.Name, planned.Concurrency, planned.ResultFile)
		fmt.Printf("  %s\n", vllmbench.ShellQuote(command.Args))
	}
}

func runBench(args []string) {
	flags := flag.NewFlagSet("run", flag.ExitOnError)
	specPath := flags.String("spec", "", "benchmark spec JSON file")
	runDir := flags.String("run-dir", "", "optional run directory")
	artifactPath := flags.String("artifact", "", "optional artifact path; an existing artifact is appended to (model-level accumulation)")
	resume := flags.Bool("resume", false, "skip planned runs whose result files already completed; requires --run-dir of the previous attempt")
	dryRun := flags.Bool("dry-run", false, "write planned artifacts without launching vLLM or benchmark commands")
	timeout := flags.Duration("timeout", 0, "optional overall timeout, for example 2h")
	filterFlags := addFilterFlags(flags)
	_ = flags.Parse(args)
	spec := mustLoadSpec(*specPath, filterFlags.Filter())
	fmt.Printf("spec provenance: %s\n", spec.Provenance)
	ctx := context.Background()
	cancel := func() {}
	if *timeout > 0 {
		ctx, cancel = context.WithTimeout(ctx, *timeout)
	}
	defer cancel()
	summary, err := vllmbench.Execute(ctx, spec, vllmbench.RunOptions{
		RunDir:           *runDir,
		ArtifactPath:     *artifactPath,
		DryRun:           *dryRun,
		Resume:           *resume,
		OriginalSpecPath: *specPath,
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

func mustLoadSpec(path string, filter vllmbench.Filter) vllmbench.Spec {
	if strings.TrimSpace(path) == "" {
		fmt.Fprintln(os.Stderr, "missing --spec")
		os.Exit(2)
	}
	spec, err := vllmbench.LoadSpec(path)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	if err := vllmbench.ApplyFilter(&spec, filter); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	return spec
}

func usage() {
	fmt.Fprintln(os.Stderr, `usage:
  localperf bench plan --spec spec.json [--run-dir runs/example] [--profile 8k] [--workload decode-8k] [--concurrency 4] [--json]
  localperf bench run  --spec spec.json [--run-dir runs/example] [--profile 8k] [--workload decode-8k] [--concurrency 4] [--dry-run] [--timeout 2h]`)
}

func usageRoot() {
	fmt.Fprintln(os.Stderr, `usage:
  localperf bench plan   --spec spec.json [--run-dir runs/example] [--profile 8k] [--workload decode-8k] [--concurrency 4] [--json]
  localperf bench run    --spec spec.json [--run-dir runs/example] [--profile 8k] [--workload decode-8k] [--concurrency 4] [--dry-run] [--timeout 2h]
  localperf artifact check runs/example.sqlite
  localperf artifact render runs/example.sqlite [--output runs/example.html] [--store]
  localperf artifact merge --into runs/models/model.sqlite src1.sqlite [src2.sqlite ...]
  localperf sweep plan   --model model-id [--contexts 4k,8k,16k,32k,64k] [--concurrency 1,4,8,16,32] [--repeats 1] [--reference] [--stress] [--vllm-command /path/to/vllm] [--gpu-memory-utilization 0.4] [--kv-cache-memory-bytes N] [--trim 64k=8:'reason'] [--out spec.json]
  localperf view runs/model.sqlite [runs/other.sqlite ...] [--addr 127.0.0.1:0] [--open]`)
}

func addFilterFlags(flags *flag.FlagSet) *filterFlags {
	out := &filterFlags{}
	flags.Var(&out.profiles, "profile", "profile name to include; may be repeated")
	flags.Var(&out.workloads, "workload", "workload name to include; may be repeated")
	flags.Var(&out.concurrencies, "concurrency", "concurrency value to include; may be repeated")
	return out
}

type filterFlags struct {
	profiles      stringList
	workloads     stringList
	concurrencies intList
}

func (flags *filterFlags) Filter() vllmbench.Filter {
	return vllmbench.Filter{
		Profiles:      flags.profiles,
		Workloads:     flags.workloads,
		Concurrencies: flags.concurrencies,
	}
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
