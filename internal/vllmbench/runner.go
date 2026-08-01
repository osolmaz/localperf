package vllmbench

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/osolmaz/localperf/internal/artifact"
)

type RunOptions struct {
	RunDir       string
	ArtifactPath string
	DryRun       bool
	// Resume skips planned runs whose result file already exists and
	// parses as completed with no failed requests; requires an explicit
	// RunDir pointing at the previous attempt.
	Resume bool
}

type RunSummary struct {
	RunDir        string      `json:"run_dir"`
	StartedAt     time.Time   `json:"started_at"`
	FinishedAt    time.Time   `json:"finished_at"`
	DryRun        bool        `json:"dry_run"`
	InProgress    bool        `json:"-"`
	Error         string      `json:"error,omitempty"`
	PlannedRuns   int         `json:"planned_runs"`
	CompletedRuns int         `json:"completed_runs"`
	FailedRuns    int         `json:"failed_runs"`
	SkippedRuns   int         `json:"skipped_runs,omitempty"`
	Rows          []ReportRow `json:"rows,omitempty"`
	EventsPath    string      `json:"events_path"`
	ArtifactPath  string      `json:"artifact_path,omitempty"`
	SpecPath      string      `json:"spec_path"`
	MemoryFloor   float64     `json:"memory_floor_gib"`
}

type Event struct {
	Timestamp       time.Time       `json:"timestamp"`
	Type            string          `json:"type"`
	Profile         string          `json:"profile,omitempty"`
	Workload        string          `json:"workload,omitempty"`
	Concurrency     int             `json:"concurrency,omitempty"`
	Repeat          int             `json:"repeat,omitempty"`
	Command         string          `json:"command,omitempty"`
	Args            []string        `json:"args,omitempty"`
	ResultFile      string          `json:"result_file,omitempty"`
	LogFile         string          `json:"log_file,omitempty"`
	DurationSeconds float64         `json:"duration_seconds,omitempty"`
	ExitCode        int             `json:"exit_code,omitempty"`
	Error           string          `json:"error,omitempty"`
	MemAvailableGiB float64         `json:"mem_available_gib,omitempty"`
	Details         json.RawMessage `json:"details,omitempty"`
}

type serverProcess struct {
	profile Profile
	cmd     *exec.Cmd
	pgid    int
	logFile *os.File
	logPath string
	done    chan error
}

type eventWriter struct {
	mu   sync.Mutex
	file *os.File
	enc  *json.Encoder
}

type runSession struct {
	ctx     context.Context
	spec    Spec
	opts    RunOptions
	runDir  string
	summary RunSummary
	events  *eventWriter
	plan    []PlannedRun
	// executionRuns is the plan minus resume-adopted points; preboot and
	// profile execution work from it, while artifact writes keep the full
	// plan so adopted measurements still import.
	executionRuns          []PlannedRun
	processes              map[string]*serverProcess
	ladderRows             map[string]*ReportRow
	ladderStops            map[string]adaptiveStop
	reportedMaxConcurrency map[string]float64
}

func Execute(ctx context.Context, spec Spec, opts RunOptions) (RunSummary, error) {
	ctx, stopSignals := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
	defer stopSignals()

	session, err := newRunSession(ctx, spec, opts)
	if err != nil {
		return sessionSummary(session), err
	}
	defer session.close()

	if opts.DryRun {
		return session.finish(nil)
	}

	return session.finish(session.run())
}

func sessionSummary(session *runSession) RunSummary {
	if session == nil {
		return RunSummary{}
	}
	return session.summary
}

func newRunSession(ctx context.Context, spec Spec, opts RunOptions) (*runSession, error) {
	ApplyDefaults(&spec)
	if err := ValidateSpec(spec); err != nil {
		return nil, err
	}
	if opts.Resume && strings.TrimSpace(opts.RunDir) == "" {
		return nil, fmt.Errorf("--resume requires --run-dir pointing at the previous attempt")
	}
	session := initRunSession(ctx, spec, opts)
	if err := prepareSessionSpec(ctx, session); err != nil {
		return session, err
	}
	events, err := openEventWriter(session.summary.EventsPath, opts.Resume)
	if err != nil {
		return session, err
	}
	session.events = events
	session.plan = BuildPlan(session.spec, session.runDir)
	session.summary.PlannedRuns = len(session.plan)
	// A resumed attempt keeps the previous log's plan events.
	if !opts.Resume {
		writePlanEvents(session)
	}
	return session, nil
}

func prepareSessionSpec(ctx context.Context, session *runSession) error {
	if err := prepareRunDirs(session); err != nil {
		return err
	}
	if err := PrepareDatasets(ctx, &session.spec, session.runDir); err != nil {
		return err
	}
	ApplyDefaults(&session.spec)
	if err := ValidateSpec(session.spec); err != nil {
		return err
	}
	return writeJSONFile(session.summary.SpecPath, RedactedSpec(session.spec))
}

func initRunSession(ctx context.Context, spec Spec, opts RunOptions) *runSession {
	runDir := RunDir(opts.RunDir, spec, time.Now())
	summary := RunSummary{
		RunDir:       runDir,
		StartedAt:    time.Now().UTC(),
		DryRun:       opts.DryRun,
		EventsPath:   filepath.Join(runDir, "events.jsonl"),
		ArtifactPath: artifact.Path(runDir, opts.ArtifactPath),
		SpecPath:     filepath.Join(runDir, "spec.normalized.json"),
		MemoryFloor:  spec.Safety.MinMemAvailableGiB,
	}
	return &runSession{
		ctx: ctx, spec: spec, opts: opts, runDir: runDir, summary: summary,
		processes:              map[string]*serverProcess{},
		ladderRows:             map[string]*ReportRow{},
		ladderStops:            map[string]adaptiveStop{},
		reportedMaxConcurrency: map[string]float64{},
	}
}

func prepareRunDirs(session *runSession) error {
	for _, name := range []string{"results", "logs", "datasets"} {
		if err := os.MkdirAll(filepath.Join(session.runDir, name), 0o755); err != nil {
			return err
		}
	}
	return nil
}

func writePlanEvents(session *runSession) {
	session.events.Write(Event{Timestamp: time.Now().UTC(), Type: "run_start", Details: mustJSON(map[string]any{
		"name":         session.spec.Name,
		"planned_runs": len(session.plan),
		"dry_run":      session.opts.DryRun,
	})})
	for _, planned := range session.plan {
		writePlannedRunEvent(session, planned)
	}
}

func writePlannedRunEvent(session *runSession, planned PlannedRun) {
	command := LoadCommand(session.spec, planned)
	session.events.Write(Event{
		Timestamp:   time.Now().UTC(),
		Type:        "planned_run",
		Profile:     planned.Profile.Name,
		Workload:    planned.Workload.Name,
		Concurrency: planned.Concurrency,
		Repeat:      planned.Repeat,
		Command:     CommandSummary(command),
		Args:        command.Args,
		ResultFile:  planned.ResultFile,
	})
}

func (session *runSession) close() {
	if shouldStopManaged(session.spec) || session.ctx.Err() != nil {
		stopAll(session.processes)
	}
	_ = session.events.Close()
}

func (session *runSession) run() error {
	// Resume skimming happens before preboot so profiles with nothing left
	// to run never start or wake a server.
	session.executionRuns = session.plan
	if session.opts.Resume {
		session.executionRuns = session.skipResumedRuns(session.plan)
	}
	if err := session.prebootProfiles(); err != nil {
		return err
	}
	return session.runProfiles()
}

func (session *runSession) finish(runErr error) (RunSummary, error) {
	if runErr == nil {
		runErr = session.ctx.Err()
	}
	err := finalizeRun(session.runDir, &session.summary, session.events, session.spec, runErr)
	return session.summary, err
}

func (session *runSession) prebootProfiles() error {
	if !session.spec.Runner.PrebootProfiles {
		return nil
	}
	err := prebootProfiles(session.ctx, session.spec, session.runDir, session.executionRuns, session.events, session.processes)
	if err == nil {
		return nil
	}
	session.summary.FailedRuns += remainingUnaccountedRuns(session.summary)
	session.events.Write(Event{Timestamp: time.Now().UTC(), Type: "preboot_failed", Error: err.Error()})
	markRunsSkipped(session.events, session.executionRuns, err.Error())
	return fmt.Errorf("preboot profiles failed: %w", err)
}

func (session *runSession) runProfiles() error {
	for _, profile := range profilesInPlanOrder(session.spec.Profiles, session.executionRuns) {
		if err := session.runProfile(profile); err != nil {
			return err
		}
	}
	return nil
}

func (session *runSession) runProfile(profile Profile) error {
	runs := runsForProfile(session.executionRuns, profile.Name)
	// Stops replayed from resumed rows are known before boot; a profile
	// whose remaining points are all adaptively skipped never starts.
	runs = session.applyAdaptiveSkips(runs)
	if len(runs) == 0 {
		return nil
	}
	proc, ok := session.ensureProfileReady(profile, runs)
	if !ok {
		return nil
	}
	session.recordReportedMaxConcurrency(profile, proc)
	if session.runProfileWorkloads(profile, proc, runs) {
		return nil
	}
	if err := session.sleepFinishedProfile(profile, proc); err != nil {
		return err
	}
	session.stopFinishedProfile(profile, proc)
	return nil
}

func (session *runSession) ensureProfileReady(profile Profile, runs []PlannedRun) (*serverProcess, bool) {
	proc := session.processes[profile.Name]
	if proc == nil {
		return session.prepareManagedProfile(profile, runs)
	}
	return proc, session.wakePrebootedProfile(profile, proc, runs)
}

func (session *runSession) prepareManagedProfile(profile Profile, runs []PlannedRun) (*serverProcess, bool) {
	proc, err := prepareProfile(session.ctx, session.spec, session.runDir, profile, session.events, true)
	if err != nil {
		session.failProfile(profile, runs, err)
		return nil, false
	}
	if proc != nil {
		session.processes[profile.Name] = proc
	}
	return proc, true
}

func (session *runSession) wakePrebootedProfile(profile Profile, proc *serverProcess, runs []PlannedRun) bool {
	if err := wakeProfile(session.ctx, session.spec, profile, session.events); err != nil {
		session.failProfileAndStop(profile, proc, runs, err)
		return false
	}
	if err := session.runProfileWarmup(profile, proc, runs); err != nil {
		return false
	}
	return true
}

func (session *runSession) runProfileWarmup(profile Profile, proc *serverProcess, runs []PlannedRun) error {
	if !session.spec.Warmup.Enabled {
		return nil
	}
	if err := runWarmup(session.ctx, session.spec, profile, session.runDir, session.events); err != nil {
		session.failProfile(profile, runs, err)
		session.sleepFailedWarmupProfile(profile, proc)
		return err
	}
	return nil
}

func (session *runSession) failProfile(profile Profile, runs []PlannedRun, err error) {
	session.summary.FailedRuns += len(runs)
	session.events.Write(Event{Timestamp: time.Now().UTC(), Type: "profile_failed", Profile: profile.Name, Error: err.Error()})
	markRunsSkipped(session.events, runs, err.Error())
}

func (session *runSession) failProfileAndStop(profile Profile, proc *serverProcess, runs []PlannedRun, err error) {
	session.failProfile(profile, runs, err)
	stopProcess(proc)
	delete(session.processes, profile.Name)
}

func (session *runSession) sleepFailedWarmupProfile(profile Profile, proc *serverProcess) {
	if !profile.EnableSleepMode {
		return
	}
	if err := sleepProfile(session.ctx, session.spec, profile, session.events); err != nil {
		session.stopAfterWarmupSleepFailure(profile, proc, err)
	}
}

func (session *runSession) stopAfterWarmupSleepFailure(profile Profile, proc *serverProcess, err error) {
	session.events.Write(Event{Timestamp: time.Now().UTC(), Type: "profile_sleep_failed", Profile: profile.Name, Error: err.Error()})
	stopProcess(proc)
	delete(session.processes, profile.Name)
}

// applyAdaptiveSkips records adaptive skips for planned runs whose stop
// state is already known, before any server boots for them.
func (session *runSession) applyAdaptiveSkips(runs []PlannedRun) []PlannedRun {
	remaining := make([]PlannedRun, 0, len(runs))
	for _, planned := range runs {
		if reason := session.adaptiveSkipReason(planned); reason != "" {
			session.skipPlannedRun(planned, reason)
			continue
		}
		remaining = append(remaining, planned)
	}
	return remaining
}

// skipResumedRuns drops planned runs whose result file already exists and
// parses as completed with zero failed requests, counting them as completed
// from the previous attempt. Profiles left with nothing to run never boot.
func (session *runSession) skipResumedRuns(runs []PlannedRun) []PlannedRun {
	remaining := make([]PlannedRun, 0, len(runs))
	for _, planned := range runs {
		row, ok := resumableRow(planned)
		if !ok {
			remaining = append(remaining, planned)
			continue
		}
		session.summary.CompletedRuns++
		session.summary.Rows = append(session.summary.Rows, *row)
		// Resumed rows replay the adaptive ladder so stop state from the
		// previous attempt is not forgotten.
		session.updateLadder(planned, row)
		session.events.Write(Event{
			Timestamp:   time.Now().UTC(),
			Type:        "workload_resumed",
			Profile:     planned.Profile.Name,
			Workload:    planned.Workload.Name,
			Concurrency: planned.Concurrency,
			Repeat:      planned.Repeat,
			ResultFile:  planned.ResultFile,
		})
	}
	return remaining
}

// writeArtifactSnapshot refreshes the artifact after every planned run so a
// crash or abort never loses completed measurements and partial results can
// be rendered mid-sweep. Snapshot failures are recorded but never abort the
// benchmark.
func (session *runSession) writeArtifactSnapshot() {
	if session.summary.ArtifactPath == "" || session.opts.DryRun {
		return
	}
	snapshot := session.summary
	snapshot.InProgress = true
	if err := writeSQLiteArtifact(session.runDir, snapshot.ArtifactPath, session.spec, snapshot); err != nil {
		session.events.Write(Event{Timestamp: time.Now().UTC(), Type: "artifact_snapshot_failed", Error: err.Error()})
	}
}

func (session *runSession) runProfileWorkloads(profile Profile, proc *serverProcess, runs []PlannedRun) bool {
	for i, planned := range runs {
		if aborted := session.runProfileWorkload(profile, proc, runs, i, planned); aborted {
			return true
		}
	}
	return false
}

func (session *runSession) runProfileWorkload(profile Profile, proc *serverProcess, runs []PlannedRun, index int, planned PlannedRun) bool {
	if reason := session.adaptiveSkipReason(planned); reason != "" {
		session.skipPlannedRun(planned, reason)
		return false
	}
	if err := checkMemoryEvent(session.spec, session.events, "before_workload", planned.Profile.Name); err != nil {
		return session.handleWorkloadError(profile, proc, runs, index, planned, "workload_skipped", err)
	}
	result, err := executeBench(session.ctx, session.spec, planned, session.runDir, session.events)
	if err != nil {
		session.stopLadder(planned, fmt.Sprintf("concurrency %d failed: %v", planned.Concurrency, err))
		aborted := session.handleWorkloadError(profile, proc, runs, index, planned, "workload_failed", err)
		session.writeArtifactSnapshot()
		return aborted
	}
	session.summary.CompletedRuns++
	if result != nil {
		session.summary.Rows = append(session.summary.Rows, *result)
	}
	session.updateLadder(planned, result)
	session.writeArtifactSnapshot()
	return false
}

// skipPlannedRun records an adaptive skip: skipped is a first-class outcome
// with a reason, never a silent hole and never a failure.
func (session *runSession) skipPlannedRun(planned PlannedRun, reason string) {
	session.summary.SkippedRuns++
	session.events.Write(Event{
		Timestamp:   time.Now().UTC(),
		Type:        "workload_skipped",
		Profile:     planned.Profile.Name,
		Workload:    planned.Workload.Name,
		Concurrency: planned.Concurrency,
		Repeat:      planned.Repeat,
		Error:       reason,
	})
	session.writeArtifactSnapshot()
}

func (session *runSession) handleWorkloadError(profile Profile, proc *serverProcess, runs []PlannedRun, index int, planned PlannedRun, eventType string, err error) bool {
	session.summary.FailedRuns++
	session.events.Write(Event{Timestamp: time.Now().UTC(), Type: eventType, Profile: planned.Profile.Name, Workload: planned.Workload.Name, Concurrency: planned.Concurrency, Repeat: planned.Repeat, Error: err.Error()})
	if !IsMemoryFloorError(err) {
		return false
	}
	remaining := len(runs) - index - 1
	session.summary.FailedRuns += remaining
	markRunsSkipped(session.events, runs[index+1:], "skipped after memory floor guard tripped")
	stopProfileAfterMemoryFloor(session.events, profile, proc, session.processes, remaining)
	return true
}

func (session *runSession) sleepFinishedProfile(profile Profile, proc *serverProcess) error {
	if !profile.EnableSleepMode {
		return nil
	}
	err := sleepProfile(session.ctx, session.spec, profile, session.events)
	if err == nil {
		return nil
	}
	session.events.Write(Event{Timestamp: time.Now().UTC(), Type: "profile_sleep_failed", Profile: profile.Name, Error: err.Error()})
	stopProcess(proc)
	delete(session.processes, profile.Name)
	remaining := runsAfterProfile(session.spec.Profiles, session.plan, profile.Name)
	session.summary.FailedRuns += len(remaining)
	markRunsSkipped(session.events, remaining, err.Error())
	return fmt.Errorf("profile %s sleep failed: %w", profile.Name, err)
}

func (session *runSession) stopFinishedProfile(profile Profile, proc *serverProcess) {
	if proc != nil && shouldStopManaged(session.spec) && !session.spec.Runner.PrebootProfiles {
		stopProcess(proc)
		delete(session.processes, profile.Name)
	}
}

func finalizeRun(runDir string, summary *RunSummary, events *eventWriter, spec Spec, runErr error) error {
	summary.FinishedAt = time.Now().UTC()
	if runErr != nil {
		summary.Error = runErr.Error()
	}
	writeRunFinishEvent(summary, events, runErr)
	if err := writeJSONFile(filepath.Join(runDir, "summary.json"), summary); err != nil {
		return err
	}
	if err := writeFinalArtifact(runDir, summary, spec); err != nil && runErr == nil {
		return err
	}
	return finalRunError(*summary, runErr)
}

func writeRunFinishEvent(summary *RunSummary, events *eventWriter, runErr error) {
	event := Event{Timestamp: summary.FinishedAt, Type: "run_finish", Details: mustJSON(map[string]any{
		"completed_runs": summary.CompletedRuns,
		"failed_runs":    summary.FailedRuns,
	})}
	if runErr != nil {
		event.Error = runErr.Error()
	}
	events.Write(event)
}

func writeFinalArtifact(runDir string, summary *RunSummary, spec Spec) error {
	if summary.ArtifactPath == "" {
		return nil
	}
	return writeSQLiteArtifact(runDir, summary.ArtifactPath, spec, *summary)
}

func finalRunError(summary RunSummary, runErr error) error {
	if runErr != nil {
		return runErr
	}
	if summary.FailedRuns > 0 && summary.CompletedRuns == 0 {
		return fmt.Errorf("%d benchmark run(s) failed", summary.FailedRuns)
	}
	return nil
}

func remainingUnaccountedRuns(summary RunSummary) int {
	remaining := summary.PlannedRuns - summary.CompletedRuns - summary.FailedRuns
	if remaining < 0 {
		return 0
	}
	return remaining
}

func prebootProfiles(ctx context.Context, spec Spec, runDir string, plan []PlannedRun, events *eventWriter, processes map[string]*serverProcess) error {
	for _, profile := range profilesInPlanOrder(spec.Profiles, plan) {
		proc, err := prepareProfile(ctx, spec, runDir, profile, events, true)
		if err != nil {
			return err
		}
		if proc != nil {
			processes[profile.Name] = proc
		}
		if profile.EnableSleepMode {
			if err := sleepProfile(ctx, spec, profile, events); err != nil {
				return err
			}
		}
	}
	return nil
}

func prepareProfile(ctx context.Context, spec Spec, runDir string, profile Profile, events *eventWriter, shouldWarmup bool) (*serverProcess, error) {
	if err := checkMemoryEvent(spec, events, "before_profile", profile.Name); err != nil {
		return nil, err
	}
	proc, err := startManagedProfile(ctx, spec, runDir, profile, events)
	if err != nil {
		return nil, err
	}
	for _, step := range profilePrepareSteps(shouldWarmup) {
		if err := step(ctx, spec, runDir, profile, events, proc); err != nil {
			stopProcess(proc)
			return nil, err
		}
	}
	return proc, nil
}

func startManagedProfile(ctx context.Context, spec Spec, runDir string, profile Profile, events *eventWriter) (*serverProcess, error) {
	if !profile.Managed {
		return nil, nil
	}
	return startServer(ctx, spec, runDir, profile, events)
}

type profilePrepareStep func(context.Context, Spec, string, Profile, *eventWriter, *serverProcess) error

func profilePrepareSteps(shouldWarmup bool) []profilePrepareStep {
	steps := []profilePrepareStep{waitReadyStep, wakeProfileStep}
	if shouldWarmup {
		steps = append(steps, warmupProfileStep)
	}
	return steps
}

func waitReadyStep(ctx context.Context, spec Spec, _ string, profile Profile, events *eventWriter, proc *serverProcess) error {
	return waitReady(ctx, spec, profile, events, proc)
}

func wakeProfileStep(ctx context.Context, spec Spec, _ string, profile Profile, events *eventWriter, _ *serverProcess) error {
	if profile.EnableSleepMode {
		return wakeProfile(ctx, spec, profile, events)
	}
	return nil
}

func warmupProfileStep(ctx context.Context, spec Spec, runDir string, profile Profile, events *eventWriter, _ *serverProcess) error {
	if spec.Warmup.Enabled {
		return runWarmup(ctx, spec, profile, runDir, events)
	}
	return nil
}

func startServer(_ context.Context, spec Spec, runDir string, profile Profile, events *eventWriter) (*serverProcess, error) {
	command := ServeCommand(spec, profile)
	logPath := filepath.Join(runDir, "logs", Slug(profile.Name)+".server.log")
	logFile, err := os.Create(logPath)
	if err != nil {
		return nil, err
	}
	cmd := exec.Command(command.Args[0], command.Args[1:]...)
	cmd.Env = WithProcessEnv(command.Env)
	cmd.Stdout = logFile
	cmd.Stderr = logFile
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		_ = logFile.Close()
		return nil, err
	}
	pgid, _ := syscall.Getpgid(cmd.Process.Pid)
	proc := &serverProcess{
		profile: profile,
		cmd:     cmd,
		pgid:    pgid,
		logFile: logFile,
		logPath: logPath,
		done:    make(chan error, 1),
	}
	go func() {
		proc.done <- cmd.Wait()
		_ = logFile.Close()
	}()
	events.Write(Event{
		Timestamp: time.Now().UTC(),
		Type:      "server_start",
		Profile:   profile.Name,
		Command:   CommandSummary(command),
		Args:      command.Args,
		LogFile:   logPath,
	})
	return proc, nil
}

func waitReady(ctx context.Context, spec Spec, profile Profile, events *eventWriter, proc *serverProcess) error {
	waiter := newReadinessWaiter(ctx, spec, profile, events, proc)
	defer waiter.poll.stop()
	for {
		if ready, err := waiter.check(); ready || err != nil {
			return err
		}
		if err := waiter.waitNext(); err != nil {
			return err
		}
	}
}

type readinessWaiter struct {
	ctx     context.Context
	spec    Spec
	profile Profile
	events  *eventWriter
	proc    *serverProcess
	start   time.Time
	timeout time.Duration
	client  *http.Client
	poll    pollTimer
}

type pollTimer struct {
	timer  *time.Timer
	ticker *time.Ticker
}

func newPollTimer(timeout, interval time.Duration) pollTimer {
	return pollTimer{
		timer:  time.NewTimer(timeout),
		ticker: time.NewTicker(interval),
	}
}

func (poll pollTimer) stop() {
	poll.timer.Stop()
	poll.ticker.Stop()
}

func newReadinessWaiter(ctx context.Context, spec Spec, profile Profile, events *eventWriter, proc *serverProcess) *readinessWaiter {
	timeout := time.Duration(spec.Safety.StartupTimeoutSec) * time.Second
	poll := time.Duration(spec.Safety.PollIntervalMillis) * time.Millisecond
	return &readinessWaiter{
		ctx:     ctx,
		spec:    spec,
		profile: profile,
		events:  events,
		proc:    proc,
		start:   time.Now(),
		timeout: timeout,
		client:  &http.Client{Timeout: time.Duration(spec.Safety.HTTPTimeoutSec) * time.Second},
		poll:    newPollTimer(timeout, poll),
	}
}

func (waiter *readinessWaiter) check() (bool, error) {
	if err := waiter.serverExitError(); err != nil {
		return false, err
	}
	if err := waiter.checkStartupMemory(); err != nil {
		return false, err
	}
	return waiter.probeReady(), nil
}

func (waiter *readinessWaiter) serverExitError() error {
	if waiter.proc == nil {
		return nil
	}
	select {
	case err := <-waiter.proc.done:
		waiter.proc.done <- err
		return fmt.Errorf("profile %s server exited before readiness: %v; log tail: %s", waiter.profile.Name, err, tailFile(waiter.proc.logPath, 4096))
	default:
		return nil
	}
}

func (waiter *readinessWaiter) checkStartupMemory() error {
	snapshot, err := checkMemoryFloor(waiter.spec.Safety.MinMemAvailableGiB)
	if err != nil {
		waiter.events.Write(Event{Timestamp: time.Now().UTC(), Type: "startup_memory_floor", Profile: waiter.profile.Name, MemAvailableGiB: snapshot.MemAvailableGiB, Error: err.Error()})
	}
	return err
}

func (waiter *readinessWaiter) probeReady() bool {
	req, err := http.NewRequestWithContext(waiter.ctx, http.MethodGet, baseURL(waiter.profile)+waiter.profile.HealthPath, nil)
	if err != nil {
		return false
	}
	openAIHTTPClient{profile: waiter.profile}.applyAuthHeader(req)
	resp, err := waiter.client.Do(req)
	if err != nil {
		return false
	}
	_ = resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return false
	}
	waiter.events.Write(Event{Timestamp: time.Now().UTC(), Type: "server_ready", Profile: waiter.profile.Name, DurationSeconds: time.Since(waiter.start).Seconds()})
	if identity, ok := probeEngineIdentity(waiter.ctx, waiter.client, waiter.profile); ok {
		waiter.events.Write(Event{
			Timestamp: time.Now().UTC(),
			Type:      "engine_identity",
			Profile:   waiter.profile.Name,
			Details:   mustJSON(identity),
		})
	}
	return true
}

// engineIdentity is the server's self-reported identity captured right after
// readiness: /version (vLLM) and /v1/models (any OpenAI-compatible server,
// including LM Studio and llama.cpp). It fills engines.version and
// engines.metadata_json so external engines are verified rather than
// trusted.
type engineIdentity struct {
	Version string          `json:"version,omitempty"`
	Models  json.RawMessage `json:"models,omitempty"`
}

func probeEngineIdentity(ctx context.Context, client *http.Client, profile Profile) (engineIdentity, bool) {
	identity := engineIdentity{}
	if body, ok := fetchIdentityJSON(ctx, client, profile, baseURL(profile)+"/version"); ok {
		var payload struct {
			Version string `json:"version"`
		}
		if err := json.Unmarshal(body, &payload); err == nil {
			identity.Version = payload.Version
		}
	}
	if body, ok := fetchIdentityJSON(ctx, client, profile, baseURL(profile)+"/v1/models"); ok {
		identity.Models = body
	}
	return identity, identity.Version != "" || len(identity.Models) > 0
}

const engineIdentityBodyLimit = 64 * 1024

func fetchIdentityJSON(ctx context.Context, client *http.Client, profile Profile, url string) (json.RawMessage, bool) {
	probeCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(probeCtx, http.MethodGet, url, nil)
	if err != nil {
		return nil, false
	}
	openAIHTTPClient{profile: profile}.applyAuthHeader(req)
	resp, err := client.Do(req)
	if err != nil {
		return nil, false
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, false
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, engineIdentityBodyLimit))
	if err != nil || !json.Valid(body) {
		return nil, false
	}
	return body, true
}

func (waiter *readinessWaiter) waitNext() error {
	select {
	case <-waiter.ctx.Done():
		return waiter.ctx.Err()
	case <-waiter.poll.timer.C:
		return fmt.Errorf("profile %s did not become ready within %s", waiter.profile.Name, waiter.timeout)
	case <-waiter.poll.ticker.C:
		return nil
	}
}

func runWarmup(ctx context.Context, spec Spec, profile Profile, runDir string, events *eventWriter) error {
	if err := checkMemoryEvent(spec, events, "before_warmup", profile.Name); err != nil {
		return err
	}
	command := WarmupCommand(spec, profile, runDir)
	logPath := filepath.Join(runDir, "logs", Slug(profile.Name)+"__warmup.log")
	serverLogPath := filepath.Join(runDir, "logs", Slug(profile.Name)+".server.log")
	serverLogStart := logFileOffset(serverLogPath)
	result, err := executeCommand(ctx, command, logPath, time.Duration(spec.Safety.WorkloadTimeoutSec)*time.Second, spec.Safety.MinMemAvailableGiB, time.Duration(spec.Safety.PollIntervalMillis)*time.Millisecond)
	event := Event{
		Timestamp:       time.Now().UTC(),
		Type:            "warmup_finish",
		Profile:         profile.Name,
		Command:         CommandSummary(command),
		Args:            command.Args,
		ResultFile:      resultFromArgs(command.Args),
		LogFile:         logPath,
		DurationSeconds: result.Duration.Seconds(),
		ExitCode:        result.ExitCode,
	}
	if result.ResultWritten {
		event.Details = mustJSON(map[string]any{"result_written": true})
	}
	if err != nil {
		event.Error = err.Error()
		events.Write(event)
		return err
	}
	if err := validateWarmupResult(event.ResultFile, spec.Warmup.NumPrompts, spec.Warmup.MaxConcurrency); err != nil {
		event.Error = err.Error()
		events.Write(event)
		return err
	}
	events.Write(event)
	if requiresBackendAttestation(profile) {
		observation, _ := observeBackendsSince(profile, serverLogPath, serverLogStart, "profiled warmup generation")
		events.Write(Event{Timestamp: time.Now().UTC(), Type: "backend_observation", Profile: profile.Name, Details: mustJSON(observation)})
		if err := validateBackendObservation(observation); err != nil {
			return fmt.Errorf("backend attestation failed after profiled warmup for %s: %w", profile.Name, err)
		}
	}
	return nil
}

func validateWarmupResult(path string, expectedRequests, expectedConcurrency int) error {
	return validateParsedResult(path, "warmup", expectedRequests, expectedConcurrency)
}

func validateParsedResult(path, label string, expectedRequests, expectedConcurrency int) error {
	rows, err := parseResultFile(path)
	if err != nil {
		return err
	}
	if len(rows) == 0 {
		return fmt.Errorf("%s result file did not contain a parseable row", label)
	}
	if failed := failedRequestCount(rows); failed > 0 {
		return fmt.Errorf("%s result reported %d failed request(s)", label, failed)
	}
	return validateResultPoint(rows[0], label, expectedRequests, expectedConcurrency)
}

func executeBench(ctx context.Context, spec Spec, planned PlannedRun, runDir string, events *eventWriter) (*ReportRow, error) {
	command := LoadCommand(spec, planned)
	logPath := benchmarkLogPath(runDir, planned)
	events.Write(Event{
		Timestamp:   time.Now().UTC(),
		Type:        "workload_start",
		Profile:     planned.Profile.Name,
		Workload:    planned.Workload.Name,
		Concurrency: planned.Concurrency,
		Repeat:      planned.Repeat,
		Command:     CommandSummary(command),
		Args:        command.Args,
		ResultFile:  planned.ResultFile,
		LogFile:     logPath,
	})
	sampler := startGPUTelemetrySampler(ctx, events, planned)
	result, err := executeLoadCommand(ctx, spec, planned, command, logPath)
	// Capture the finish time before waiting for the sampler: an in-flight
	// telemetry sample must not stretch the recorded wall time.
	finishedAt := time.Now().UTC()
	sampler.Stop()
	event := Event{
		Timestamp:       finishedAt,
		Type:            "workload_finish",
		Profile:         planned.Profile.Name,
		Workload:        planned.Workload.Name,
		Concurrency:     planned.Concurrency,
		Repeat:          planned.Repeat,
		Command:         CommandSummary(command),
		Args:            command.Args,
		ResultFile:      planned.ResultFile,
		LogFile:         logPath,
		DurationSeconds: result.Duration.Seconds(),
		ExitCode:        result.ExitCode,
	}
	if result.ResultWritten {
		event.Details = mustJSON(map[string]any{"result_written": true})
	}
	if err != nil {
		event.Error = err.Error()
		events.Write(event)
		return nil, err
	}
	events.Write(event)
	row, failed, err := parsedPlannedRow(planned)
	if err != nil {
		return nil, err
	}
	if failed > 0 {
		return row, fmt.Errorf("benchmark result reported %d failed request(s)", failed)
	}
	return row, nil
}

// parsedPlannedRow parses and enriches the planned run's result file.
func parsedPlannedRow(planned PlannedRun) (*ReportRow, int, error) {
	rows, err := parseResultFile(planned.ResultFile)
	if err != nil {
		return nil, 0, err
	}
	if len(rows) == 0 {
		return nil, 0, errors.New("benchmark result file did not contain a parseable row")
	}
	row := rows[0]
	failed := failedRequestCount(rows)
	if failed == 0 {
		if err := validateResultPoint(row, "workload", resolvedNumPrompts(planned.Workload, planned.Concurrency), planned.Concurrency); err != nil {
			return nil, 0, err
		}
	}
	enrichPlannedRow(&row, planned)
	return &row, failed, nil
}

func enrichPlannedRow(row *ReportRow, planned PlannedRun) {
	row.Profile = planned.Profile.Name
	row.Workload = planned.Workload.Name
	row.Context = planned.Profile.MaxModelLen
	row.ServerMaxNumSeqs = planned.Profile.MaxNumSeqs
	row.Concurrency = planned.Concurrency
	row.Repeat = planned.Repeat
	row.Phase = planned.Workload.Phase
	row.RandomInputLen = planned.Workload.RandomInputLen
	row.RandomOutputLen = planned.Workload.RandomOutputLen
	row.DatasetName = planned.Workload.DatasetName
	row.ResultFile = planned.ResultFile
	applyWorkloadFields(row, planned.Workload)
	deriveReportRowFields(row)
}

// resumableRow adopts a previous attempt's result only when it demonstrably
// measured the same point: parses cleanly with zero failures, completed at
// least the planned request count, ran at the planned concurrency, and (for
// random workloads) the measured token shape matches the current spec, so an
// edited spec re-runs instead of silently inheriting stale results.
func resumableRow(planned PlannedRun) (*ReportRow, bool) {
	rows, err := parseResultFile(planned.ResultFile)
	if err != nil || len(rows) == 0 || failedRequestCount(rows) > 0 {
		return nil, false
	}
	raw := rows[0]
	if validateResultPoint(raw, "workload", resolvedNumPrompts(planned.Workload, planned.Concurrency), planned.Concurrency) != nil {
		return nil, false
	}
	if !resultMatchesShape(raw, planned.Workload) {
		return nil, false
	}
	enrichPlannedRow(&raw, planned)
	return &raw, true
}

func validateResultPoint(row ReportRow, label string, expectedRequests, expectedConcurrency int) error {
	if row.Completed != expectedRequests {
		return fmt.Errorf("%s result completed %d request(s), want exactly %d", label, row.Completed, expectedRequests)
	}
	if row.Concurrency != expectedConcurrency {
		return fmt.Errorf("%s result concurrency = %d, want %d", label, row.Concurrency, expectedConcurrency)
	}
	if !row.promptTokensKnown || !row.completionTokensKnown || !row.totalTokensKnown {
		return fmt.Errorf("%s result is missing canonical token totals", label)
	}
	if !row.outputTokensPerSecKnown || !row.totalTokensPerSecKnown {
		return fmt.Errorf("%s result is missing canonical throughput fields", label)
	}
	return nil
}

// resultMatchesShape compares the recorded mean token shape against the
// current workload for random traffic; a 20% + 16 token band absorbs
// tokenizer and template drift while catching real spec edits.
func resultMatchesShape(raw ReportRow, workload Workload) bool {
	if workload.DatasetName != "random" || raw.Completed <= 0 {
		return true
	}
	completed := float64(raw.Completed)
	if workload.RandomInputLen > 0 && shapeDiffers(float64(raw.PromptTokens)/completed, float64(workload.RandomInputLen)) {
		return false
	}
	if workload.IgnoreEOS && workload.RandomOutputLen > 0 && shapeDiffers(float64(raw.CompletionTokens)/completed, float64(workload.RandomOutputLen)) {
		return false
	}
	return true
}

func shapeDiffers(measured, requested float64) bool {
	return math.Abs(measured-requested) > 0.2*requested+16
}

func executeLoadCommand(ctx context.Context, spec Spec, planned PlannedRun, command CommandSpec, logPath string) (commandResult, error) {
	if planned.Workload.LoadGenerator == LoadGeneratorHTTP {
		return executeHTTPBench(ctx, spec, planned, logPath)
	}
	return executeCommand(ctx, command, logPath, time.Duration(spec.Safety.WorkloadTimeoutSec)*time.Second, spec.Safety.MinMemAvailableGiB, time.Duration(spec.Safety.PollIntervalMillis)*time.Millisecond)
}

func benchmarkLogPath(runDir string, planned PlannedRun) string {
	name := fmt.Sprintf("%s__%s__c%d", Slug(planned.Profile.Name), Slug(planned.Workload.Name), planned.Concurrency)
	if planned.Workload.Repeats > 1 {
		name += fmt.Sprintf("__r%d", planned.Repeat+1)
	}
	return filepath.Join(runDir, "logs", name+".log")
}

func failedRequestCount(rows []ReportRow) int {
	failed := 0
	for _, row := range rows {
		failed += row.Failed
	}
	return failed
}

type commandResult struct {
	Duration      time.Duration
	ExitCode      int
	ResultWritten bool
}

func executeCommand(ctx context.Context, command CommandSpec, logPath string, timeout time.Duration, minMemAvailableGiB float64, pollInterval time.Duration) (commandResult, error) {
	if len(command.Args) == 0 {
		return commandResult{ExitCode: -1}, errors.New("empty command")
	}
	if err := prepareCommandPaths(command, logPath); err != nil {
		return commandResult{ExitCode: -1}, err
	}
	logFile, err := os.Create(logPath)
	if err != nil {
		return commandResult{ExitCode: -1}, err
	}
	defer logFile.Close()
	return runLoggedCommand(ctx, command, logPath, logFile, timeout, minMemAvailableGiB, pollInterval)
}

func prepareCommandPaths(command CommandSpec, logPath string) error {
	if err := os.MkdirAll(filepath.Dir(logPath), 0o755); err != nil {
		return err
	}
	result := resultFromArgs(command.Args)
	if result == "" {
		return nil
	}
	return os.MkdirAll(filepath.Dir(result), 0o755)
}

func runLoggedCommand(ctx context.Context, command CommandSpec, logPath string, logFile *os.File, timeout time.Duration, minMemAvailableGiB float64, pollInterval time.Duration) (commandResult, error) {
	runCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	memoryMonitor := monitorMemoryFloor(runCtx, cancel, minMemAvailableGiB, pollInterval)
	cmd := exec.CommandContext(runCtx, command.Args[0], command.Args[1:]...)
	cmd.Env = WithProcessEnv(command.Env)
	cmd.Stdout = logFile
	cmd.Stderr = logFile
	start := time.Now()
	runErr := cmd.Run()
	duration := time.Since(start)
	cancel()
	memoryErr := <-memoryMonitor
	result := commandResult{Duration: duration}
	if cmd.ProcessState != nil {
		result.ExitCode = cmd.ProcessState.ExitCode()
	} else {
		result.ExitCode = -1
	}
	return classifyCommandResult(runCtx, result, runErr, memoryErr, timeout, logPath)
}

func classifyCommandResult(runCtx context.Context, result commandResult, runErr, memoryErr error, timeout time.Duration, logPath string) (commandResult, error) {
	if runCtx.Err() == context.DeadlineExceeded {
		return result, fmt.Errorf("command timed out after %s", timeout)
	}
	if memoryErr != nil {
		return result, memoryErr
	}
	if runErr != nil {
		return result, fmt.Errorf("%w; log tail: %s", runErr, strings.TrimSpace(tailFile(logPath, 4096)))
	}
	return result, nil
}

func monitorMemoryFloor(ctx context.Context, cancel context.CancelFunc, minGiB float64, interval time.Duration) <-chan error {
	done := make(chan error, 1)
	go func() {
		if minGiB <= 0 {
			<-ctx.Done()
			done <- nil
			return
		}
		if interval <= 0 {
			interval = time.Second
		}
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				done <- nil
				return
			case <-ticker.C:
				if _, err := checkMemoryFloor(minGiB); err != nil {
					cancel()
					done <- err
					return
				}
			}
		}
	}()
	return done
}

func sleepProfile(ctx context.Context, spec Spec, profile Profile, events *eventWriter) error {
	if !profile.EnableSleepMode {
		return nil
	}
	if err := checkMemoryEvent(spec, events, "before_sleep", profile.Name); err != nil {
		return err
	}
	url := fmt.Sprintf("%s/sleep?level=%d", baseURL(profile), SleepLevelValue(profile))
	start := time.Now()
	err := postAdmin(ctx, spec, url)
	event := Event{Timestamp: time.Now().UTC(), Type: "profile_sleep", Profile: profile.Name, DurationSeconds: time.Since(start).Seconds()}
	if err == nil {
		err = waitSleepState(ctx, spec, profile, true)
		event.DurationSeconds = time.Since(start).Seconds()
	}
	if err != nil {
		event.Error = err.Error()
		events.Write(event)
		return err
	}
	events.Write(event)
	return nil
}

func wakeProfile(ctx context.Context, spec Spec, profile Profile, events *eventWriter) error {
	if !profile.EnableSleepMode {
		return nil
	}
	if err := checkMemoryEvent(spec, events, "before_wake", profile.Name); err != nil {
		return err
	}
	sleeping, err := isSleeping(ctx, spec, profile)
	if err != nil {
		events.Write(Event{Timestamp: time.Now().UTC(), Type: "profile_wake_check_failed", Profile: profile.Name, Error: err.Error()})
		sleeping = true
	}
	if !sleeping {
		events.Write(Event{Timestamp: time.Now().UTC(), Type: "profile_wake_skipped", Profile: profile.Name})
		return nil
	}
	start := time.Now()
	err = postAdmin(ctx, spec, baseURL(profile)+"/wake_up")
	event := Event{Timestamp: time.Now().UTC(), Type: "profile_wake", Profile: profile.Name, DurationSeconds: time.Since(start).Seconds()}
	if err == nil {
		err = waitSleepState(ctx, spec, profile, false)
		event.DurationSeconds = time.Since(start).Seconds()
	}
	if err != nil {
		event.Error = err.Error()
		events.Write(event)
		return err
	}
	events.Write(event)
	return waitReady(ctx, spec, profile, events, nil)
}

func waitSleepState(ctx context.Context, spec Spec, profile Profile, wantSleeping bool) error {
	waiter := newSleepStateWaiter(ctx, spec, profile, wantSleeping)
	defer waiter.poll.stop()
	var lastErr error
	for {
		if done, err := waiter.check(); done {
			return nil
		} else if err != nil {
			lastErr = err
		}
		if err := waiter.waitNext(lastErr); err != nil {
			return err
		}
	}
}

type sleepStateWaiter struct {
	ctx          context.Context
	spec         Spec
	profile      Profile
	wantSleeping bool
	state        string
	start        time.Time
	poll         pollTimer
}

func newSleepStateWaiter(ctx context.Context, spec Spec, profile Profile, wantSleeping bool) *sleepStateWaiter {
	state := "awake"
	if wantSleeping {
		state = "sleeping"
	}
	return &sleepStateWaiter{
		ctx:          ctx,
		spec:         spec,
		profile:      profile,
		wantSleeping: wantSleeping,
		state:        state,
		start:        time.Now(),
		poll: newPollTimer(
			time.Duration(spec.Safety.StartupTimeoutSec)*time.Second,
			time.Duration(spec.Safety.PollIntervalMillis)*time.Millisecond,
		),
	}
}

func (waiter *sleepStateWaiter) check() (bool, error) {
	sleeping, err := isSleeping(waiter.ctx, waiter.spec, waiter.profile)
	return err == nil && sleeping == waiter.wantSleeping, err
}

func (waiter *sleepStateWaiter) waitNext(lastErr error) error {
	select {
	case <-waiter.ctx.Done():
		return waiter.ctx.Err()
	case <-waiter.poll.timer.C:
		return waiter.timeoutError(lastErr)
	case <-waiter.poll.ticker.C:
		return nil
	}
}

func (waiter *sleepStateWaiter) timeoutError(lastErr error) error {
	elapsed := time.Since(waiter.start).Round(time.Millisecond)
	if lastErr != nil {
		return fmt.Errorf("profile %s did not become %s within %s: %w", waiter.profile.Name, waiter.state, elapsed, lastErr)
	}
	return fmt.Errorf("profile %s did not become %s within %s", waiter.profile.Name, waiter.state, elapsed)
}

func isSleeping(ctx context.Context, spec Spec, profile Profile) (bool, error) {
	client := &http.Client{Timeout: time.Duration(spec.Safety.HTTPTimeoutSec) * time.Second}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, baseURL(profile)+"/is_sleeping", nil)
	if err != nil {
		return false, err
	}
	resp, err := client.Do(req)
	if err != nil {
		return false, err
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return false, fmt.Errorf("%s/is_sleeping returned HTTP %d: %s", baseURL(profile), resp.StatusCode, strings.TrimSpace(string(data)))
	}
	return parseSleepingResponse(data), nil
}

func parseSleepingResponse(data []byte) bool {
	if sleeping, ok := parseSleepingJSON(data); ok {
		return sleeping
	}
	return parseSleepingText(data)
}

func parseSleepingJSON(data []byte) (bool, bool) {
	var object map[string]any
	if err := json.Unmarshal(data, &object); err == nil {
		for _, key := range []string{"is_sleeping", "sleeping"} {
			if value, ok := object[key].(bool); ok {
				return value, true
			}
		}
	}
	return false, false
}

func parseSleepingText(data []byte) bool {
	text := strings.TrimSpace(strings.ToLower(string(data)))
	return text == "true" || text == `"true"` || strings.Contains(text, ":true")
}

func postAdmin(ctx context.Context, spec Spec, url string) error {
	client := &http.Client{Timeout: time.Duration(spec.Safety.HTTPTimeoutSec) * time.Second}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, nil)
	if err != nil {
		return err
	}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return fmt.Errorf("%s returned HTTP %d: %s", url, resp.StatusCode, strings.TrimSpace(string(body)))
	}
	return nil
}

func checkMemoryEvent(spec Spec, events *eventWriter, eventType, profile string) error {
	snapshot, err := checkMemoryFloor(spec.Safety.MinMemAvailableGiB)
	event := Event{Timestamp: time.Now().UTC(), Type: eventType, Profile: profile, MemAvailableGiB: snapshot.MemAvailableGiB}
	if err != nil {
		event.Error = err.Error()
		events.Write(event)
		return err
	}
	events.Write(event)
	return nil
}

func newEventWriter(path string) (*eventWriter, error) {
	return openEventWriter(path, false)
}

// openEventWriter appends when resuming so the previous attempt's events
// stay part of the run's history; otherwise it truncates.
func openEventWriter(path string, appendEvents bool) (*eventWriter, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, err
	}
	mode := os.O_CREATE | os.O_WRONLY | os.O_TRUNC
	if appendEvents {
		mode = os.O_CREATE | os.O_WRONLY | os.O_APPEND
	}
	file, err := os.OpenFile(path, mode, 0o644)
	if err != nil {
		return nil, err
	}
	return &eventWriter{file: file, enc: json.NewEncoder(file)}, nil
}

func (writer *eventWriter) Write(event Event) {
	writer.mu.Lock()
	defer writer.mu.Unlock()
	_ = writer.enc.Encode(event)
}

func (writer *eventWriter) Close() error {
	writer.mu.Lock()
	defer writer.mu.Unlock()
	return writer.file.Close()
}

func baseURL(profile Profile) string {
	if endpoint := NormalizeEndpointBaseURL(profile.EndpointBaseURL); endpoint != "" {
		return endpoint
	}
	return fmt.Sprintf("http://%s:%d", profile.Host, profile.Port)
}

func NormalizeEndpointBaseURL(raw string) string {
	endpoint := strings.TrimRight(strings.TrimSpace(raw), "/")
	return strings.TrimSuffix(endpoint, "/v1")
}

func resultFromArgs(args []string) string {
	for i, arg := range args {
		if arg == "--result-filename" && i+1 < len(args) {
			return args[i+1]
		}
	}
	return ""
}

func writeJSONFile(path string, value any) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	return os.WriteFile(path, data, 0o644)
}

func mustJSON(value any) json.RawMessage {
	data, err := json.Marshal(value)
	if err != nil {
		return nil
	}
	return data
}

func profilesInPlanOrder(profiles []Profile, plan []PlannedRun) []Profile {
	needed := map[string]bool{}
	for _, run := range plan {
		needed[run.Profile.Name] = true
	}
	out := make([]Profile, 0, len(needed))
	for _, profile := range profiles {
		if needed[profile.Name] {
			out = append(out, profile)
		}
	}
	return out
}

func runsForProfile(plan []PlannedRun, profileName string) []PlannedRun {
	var out []PlannedRun
	for _, run := range plan {
		if run.Profile.Name == profileName {
			out = append(out, run)
		}
	}
	return out
}

func runsAfterProfile(profiles []Profile, plan []PlannedRun, profileName string) []PlannedRun {
	seenProfile := false
	var remaining []PlannedRun
	for _, profile := range profilesInPlanOrder(profiles, plan) {
		if profile.Name == profileName {
			seenProfile = true
			continue
		}
		if seenProfile {
			remaining = append(remaining, runsForProfile(plan, profile.Name)...)
		}
	}
	return remaining
}

func markRunsSkipped(events *eventWriter, runs []PlannedRun, message string) {
	for _, planned := range runs {
		events.Write(Event{
			Timestamp:   time.Now().UTC(),
			Type:        "workload_skipped",
			Profile:     planned.Profile.Name,
			Workload:    planned.Workload.Name,
			Concurrency: planned.Concurrency,
			Repeat:      planned.Repeat,
			Error:       message,
		})
	}
}

func shouldStopManaged(spec Spec) bool {
	return spec.Runner.StopManagedOnExit == nil || *spec.Runner.StopManagedOnExit
}

func stopProfileAfterMemoryFloor(events *eventWriter, profile Profile, proc *serverProcess, processes map[string]*serverProcess, remaining int) {
	if proc != nil {
		stopProcess(proc)
		delete(processes, profile.Name)
	}
	events.Write(Event{Timestamp: time.Now().UTC(), Type: "profile_memory_floor_abort", Profile: profile.Name, Error: fmt.Sprintf("memory floor guard tripped; skipped %d remaining profile run(s)", remaining)})
}

func stopAll(processes map[string]*serverProcess) {
	for name, proc := range processes {
		stopProcess(proc)
		delete(processes, name)
	}
}

func stopProcess(proc *serverProcess) {
	if proc == nil || proc.cmd == nil || proc.cmd.Process == nil {
		return
	}
	pgid := processGroupID(proc)
	signalProcess(proc, pgid, syscall.SIGTERM)
	waitForProcessExit(proc, pgid)
}

func processGroupID(proc *serverProcess) int {
	if proc.pgid > 0 {
		return proc.pgid
	}
	pgid, err := syscall.Getpgid(proc.cmd.Process.Pid)
	if err != nil {
		return 0
	}
	return pgid
}

func signalProcess(proc *serverProcess, pgid int, signal syscall.Signal) {
	if pgid > 0 {
		_ = syscall.Kill(-pgid, signal)
		return
	}
	if signal == syscall.SIGKILL {
		_ = proc.cmd.Process.Kill()
		return
	}
	_ = proc.cmd.Process.Signal(signal)
}

func waitForProcessExit(proc *serverProcess, pgid int) {
	select {
	case <-proc.done:
	case <-time.After(20 * time.Second):
		signalProcess(proc, pgid, syscall.SIGKILL)
		<-proc.done
	}
}

func tailFile(path string, maxBytes int64) string {
	file, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return ""
	}
	start := info.Size() - maxBytes
	if start < 0 {
		start = 0
	}
	if _, err := file.Seek(start, io.SeekStart); err != nil {
		return ""
	}
	data, _ := io.ReadAll(file)
	data = trimPartialFirstLine(data, start)
	return nonEmptyTailLines(data, 12)
}

func trimPartialFirstLine(data []byte, start int64) []byte {
	if start <= 0 {
		return data
	}
	index := bytes.IndexByte(data, '\n')
	if index >= 0 && index+1 < len(data) {
		return data[index+1:]
	}
	return data
}

func nonEmptyTailLines(data []byte, maxLines int) string {
	scanner := bufio.NewScanner(bytes.NewReader(data))
	var lines []string
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line != "" {
			lines = append(lines, line)
		}
	}
	if len(lines) > maxLines {
		lines = lines[len(lines)-maxLines:]
	}
	return strings.Join(lines, " | ")
}
