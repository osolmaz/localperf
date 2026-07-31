package vllmbench

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestEngineForProfileRequiresExactMatch(t *testing.T) {
	spec := Spec{Engines: []EngineConfig{
		{Name: "first", Type: "custom", Command: "first-cmd"},
		{Name: "second", Type: "custom", Command: "second-cmd"},
	}}
	if got := EngineForProfile(spec, Profile{Engine: "second"}).Command; got != "second-cmd" {
		t.Fatalf("matched engine command = %s", got)
	}
	if got := EngineForProfile(spec, Profile{Engine: "missing"}); !reflect.DeepEqual(got, EngineConfig{}) {
		t.Fatalf("missing engine = %+v, want zero value", got)
	}
}

func TestDefaultEndpointAndTrafficValidationBranches(t *testing.T) {
	if defaultEndpoint("openai-chat") != "/v1/chat/completions" {
		t.Fatal("openai-chat endpoint mismatch")
	}
	if defaultEndpoint("openai") != "/v1/completions" {
		t.Fatal("openai endpoint mismatch")
	}
	if defaultEndpoint("custom") != "" {
		t.Fatal("custom endpoint should be empty")
	}

	seed := -1
	customOutput := -2
	shareGPTOutput := 0
	traffic := BenchmarkTrafficConfig{
		DatasetName:                 "random",
		RandomInputLen:              0,
		RandomOutputLen:             0,
		Seed:                        &seed,
		RandomPrefixLen:             -1,
		SonnetInputLen:              -1,
		SonnetOutputLen:             -1,
		SonnetPrefixLen:             -1,
		PrefixRepetitionPrefixLen:   -1,
		PrefixRepetitionSuffixLen:   -1,
		PrefixRepetitionNumPrefixes: -1,
		PrefixRepetitionOutputLen:   -1,
		SpeedBenchOutputLen:         -1,
		CustomOutputLen:             &customOutput,
		ShareGPTOutputLen:           &shareGPTOutput,
		Metadata:                    []string{"ok", " "},
		Goodput:                     []string{"ttft:5000", ""},
		ExtraBody:                   "{",
		SpeedBenchDatasetSubset:     "math",
		SpeedBenchCategory:          "reasoning",
		RandomRangeRatio:            "0.5",
		DisableShuffle:              true,
		NoOversample:                true,
		SkipChatTemplate:            true,
		SaveDetailed:                boolPointer(true),
		PlotDatasetStats:            true,
	}
	issues := validateTrafficConfig("workload x", traffic)
	if len(issues) < 13 {
		t.Fatalf("issues = %#v", issues)
	}
	joined := strings.Join(issues, "\n")
	for _, want := range []string{
		"random_input_len must be positive",
		"random_output_len must be positive",
		"seed must be non-negative",
		"custom_output_len must be -1 or greater",
		"sharegpt_output_len must be positive",
		"metadata values must not be empty",
		"goodput values must not be empty",
	} {
		if !strings.Contains(joined, want) {
			t.Fatalf("issues missing %q: %s", want, joined)
		}
	}
}

func TestBaseURLPrefersEndpointBaseURL(t *testing.T) {
	profile := Profile{Host: "127.0.0.1", Port: 8000, EndpointBaseURL: "http://127.0.0.1:8000/proxy/"}
	if got := baseURL(profile); got != "http://127.0.0.1:8000/proxy" {
		t.Fatalf("baseURL = %q, want endpoint base URL without trailing slash", got)
	}
	profile.EndpointBaseURL = "http://127.0.0.1:8000/proxy/v1/"
	if got := baseURL(profile); got != "http://127.0.0.1:8000/proxy" {
		t.Fatalf("baseURL = %q, want endpoint base URL without API root", got)
	}
	profile.EndpointBaseURL = "http://127.0.0.1:8000/v1"
	if got := baseURL(profile); got != "http://127.0.0.1:8000" {
		t.Fatalf("baseURL = %q, want root endpoint base URL", got)
	}
}

func TestParseMeminfoErrorBranches(t *testing.T) {
	snapshot, err := ParseMeminfo(strings.NewReader("MemTotal: 2048 kB\ninvalid\nSwapFree: nope kB\n"))
	if err == nil {
		t.Fatalf("snapshot = %+v, want missing MemAvailable error", snapshot)
	}
	snapshot, err = ParseMeminfo(strings.NewReader("MemTotal: 1048576 kB\nMemAvailable: 524288 kB\nSwapFree: 262144 kB\n"))
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.MemTotalGiB != 1 || snapshot.MemAvailableGiB != 0.5 || snapshot.SwapFreeGiB != 0.25 {
		t.Fatalf("snapshot = %+v", snapshot)
	}
}

func TestParseDarwinVMStat(t *testing.T) {
	data := []byte(`Mach Virtual Memory Statistics: (page size of 16384 bytes)
Pages free:                               10.
Pages active:                             99.
Pages inactive:                           20.
Pages speculative:                         5.
Pages purgeable:                           1.
`)
	available, err := parseDarwinVMStat(data)
	if err != nil {
		t.Fatal(err)
	}
	if available != 36*16384 {
		t.Fatalf("available bytes = %d, want %d", available, 36*16384)
	}
}

func TestReadDarwinMemorySnapshotWith(t *testing.T) {
	vmStat := []byte("Mach Virtual Memory Statistics: (page size of 4096 bytes)\nPages free: 262144.\n")
	snapshot, err := readDarwinMemorySnapshotWith(
		func() (uint64, error) { return 8 << 30, nil },
		func() ([]byte, error) { return vmStat, nil },
	)
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.MemTotalGiB != 8 || snapshot.MemAvailableGiB != 1 {
		t.Fatalf("snapshot = %+v, want 8 GiB total and 1 GiB available", snapshot)
	}

	totalErr := errors.New("total memory failed")
	_, err = readDarwinMemorySnapshotWith(
		func() (uint64, error) { return 0, totalErr },
		func() ([]byte, error) { return vmStat, nil },
	)
	if !errors.Is(err, totalErr) {
		t.Fatalf("total memory error = %v, want %v", err, totalErr)
	}

	vmStatErr := errors.New("vm_stat failed")
	_, err = readDarwinMemorySnapshotWith(
		func() (uint64, error) { return 8 << 30, nil },
		func() ([]byte, error) { return nil, vmStatErr },
	)
	if !errors.Is(err, vmStatErr) {
		t.Fatalf("vm_stat error = %v, want %v", err, vmStatErr)
	}

	_, err = readDarwinMemorySnapshotWith(
		func() (uint64, error) { return 8 << 30, nil },
		func() ([]byte, error) { return []byte("invalid vm_stat output"), nil },
	)
	if err == nil {
		t.Fatal("invalid vm_stat output returned no error")
	}
}

func TestParseDarwinTotalMemoryBytes(t *testing.T) {
	total, err := parseDarwinTotalMemoryBytes([]byte("8589934592\n"), nil)
	if err != nil {
		t.Fatal(err)
	}
	if total != 8<<30 {
		t.Fatalf("total bytes = %d, want %d", total, uint64(8<<30))
	}

	commandErr := errors.New("sysctl failed")
	if _, err := parseDarwinTotalMemoryBytes(nil, commandErr); !errors.Is(err, commandErr) {
		t.Fatalf("command error = %v, want %v", err, commandErr)
	}
	if _, err := parseDarwinTotalMemoryBytes([]byte("not-a-number"), nil); err == nil {
		t.Fatal("invalid total memory returned no error")
	}
}

func TestCheckMemoryEventWritesSuccessAndFailure(t *testing.T) {
	original := checkMemoryFloor
	defer func() { checkMemoryFloor = original }()

	dir := t.TempDir()
	writer, err := newEventWriter(filepath.Join(dir, "events.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	checkMemoryFloor = func(float64) (MemorySnapshot, error) {
		return MemorySnapshot{MemAvailableGiB: 123}, nil
	}
	if err := checkMemoryEvent(Spec{Safety: SafetyConfig{MinMemAvailableGiB: 1}}, writer, "memory_ok", "p1"); err != nil {
		t.Fatal(err)
	}
	floorErr := &MemoryFloorError{Snapshot: MemorySnapshot{MemAvailableGiB: 0.5}, MinGiB: 1}
	checkMemoryFloor = func(float64) (MemorySnapshot, error) {
		return floorErr.Snapshot, floorErr
	}
	if err := checkMemoryEvent(Spec{Safety: SafetyConfig{MinMemAvailableGiB: 1}}, writer, "memory_fail", "p2"); !IsMemoryFloorError(err) {
		t.Fatalf("error = %v, want memory floor", err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	events, err := readEvents(filepath.Join(dir, "events.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 2 || events[0].MemAvailableGiB != 123 || events[1].Error == "" {
		t.Fatalf("events = %+v", events)
	}
}

func TestSleepFailedWarmupProfile(t *testing.T) {
	original := checkMemoryFloor
	defer func() { checkMemoryFloor = original }()
	checkMemoryFloor = func(float64) (MemorySnapshot, error) {
		return MemorySnapshot{MemAvailableGiB: 123}, nil
	}

	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		http.Error(writer, "sleep failed", http.StatusInternalServerError)
	}))
	defer server.Close()
	host, rawPort, err := net.SplitHostPort(strings.TrimPrefix(server.URL, "http://"))
	if err != nil {
		t.Fatal(err)
	}
	port, err := strconv.Atoi(rawPort)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	events, err := newEventWriter(filepath.Join(dir, "events.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	session := &runSession{
		ctx:       contextWithTimeout(t),
		spec:      Spec{Safety: SafetyConfig{MinMemAvailableGiB: 1, HTTPTimeoutSec: 1, StartupTimeoutSec: 1, PollIntervalMillis: 10}},
		events:    events,
		processes: map[string]*serverProcess{},
	}
	profile := Profile{Name: "p1", Host: host, Port: port, EnableSleepMode: true}
	session.sleepFailedWarmupProfile(profile, nil)
	profile.EnableSleepMode = false
	session.sleepFailedWarmupProfile(profile, nil)
	if err := events.Close(); err != nil {
		t.Fatal(err)
	}
	rows, err := readEvents(filepath.Join(dir, "events.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 3 || rows[2].Type != "profile_sleep_failed" {
		t.Fatalf("events = %+v", rows)
	}
}

func contextWithTimeout(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	t.Cleanup(cancel)
	return ctx
}

func TestParseSleepingText(t *testing.T) {
	cases := map[string]bool{
		"true":              true,
		`"true"`:            true,
		`{"sleeping":true}`: true,
		"false":             false,
		"":                  false,
	}
	for raw, want := range cases {
		if got := parseSleepingText([]byte(raw)); got != want {
			t.Fatalf("parseSleepingText(%q) = %t, want %t", raw, got, want)
		}
	}
}

func TestTailFileBranches(t *testing.T) {
	if tailFile(filepath.Join(t.TempDir(), "missing.log"), 100) != "" {
		t.Fatal("missing file tail should be empty")
	}
	path := filepath.Join(t.TempDir(), "server.log")
	var lines []string
	for i := 0; i < 20; i++ {
		lines = append(lines, "line "+string(rune('a'+i)))
	}
	writeFile(t, path, strings.Join(lines, "\n"))
	full := tailFile(path, 1024)
	if !strings.Contains(full, "line i") || !strings.Contains(full, "line t") || strings.Contains(full, "line a") {
		t.Fatalf("full tail = %q", full)
	}
	short := tailFile(path, 40)
	if strings.Contains(short, "line a") || !strings.Contains(short, "line t") {
		t.Fatalf("short tail = %q", short)
	}
}

func TestRunDirAndResultHelpers(t *testing.T) {
	spec := Spec{Name: "suite", OutputDir: filepath.Join(t.TempDir(), "runs")}
	now := time.Date(2026, 6, 29, 1, 2, 3, 0, time.UTC)
	if got := RunDir("", spec, now); !strings.Contains(got, filepath.Join("runs", "suite-20260629T010203Z")) {
		t.Fatalf("run dir = %s", got)
	}
	disabled := false
	spec.Runner.AppendTimestampToRun = &disabled
	if got := RunDir("", spec, now); got != filepath.Join(spec.OutputDir, "suite") {
		t.Fatalf("run dir without timestamp = %s", got)
	}
	if got := RunDir("/tmp/explicit", spec, now); got != "/tmp/explicit" {
		t.Fatalf("explicit run dir = %s", got)
	}
	if got := resultFromArgs([]string{"bench", "--result-filename", "out.json"}); got != "out.json" {
		t.Fatalf("resultFromArgs = %s", got)
	}
	if resultFromArgs([]string{"bench"}) != "" {
		t.Fatal("missing result filename should be empty")
	}
	if got := mustJSON(map[string]int{"a": 1}); !strings.Contains(string(got), `"a":1`) {
		t.Fatalf("mustJSON = %s", got)
	}
	if got := mustJSON(make(chan int)); got != nil {
		t.Fatalf("mustJSON invalid value = %s, want nil", got)
	}
}

func TestRemainingUnaccountedRuns(t *testing.T) {
	cases := []struct {
		summary RunSummary
		want    int
	}{
		{summary: RunSummary{PlannedRuns: 5, CompletedRuns: 2, FailedRuns: 1}, want: 2},
		{summary: RunSummary{PlannedRuns: 2, CompletedRuns: 4, FailedRuns: 1}, want: 0},
	}
	for _, tc := range cases {
		if got := remainingUnaccountedRuns(tc.summary); got != tc.want {
			t.Fatalf("remaining = %d, want %d for %+v", got, tc.want, tc.summary)
		}
	}
}

func TestWriteJSONFileError(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "file"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	err := writeJSONFile(filepath.Join(dir, "file", "out.json"), map[string]string{"x": "y"})
	if err == nil {
		t.Fatal("expected writeJSONFile to fail when parent is a file")
	}
}

func TestStopProcessNilInputs(t *testing.T) {
	stopProcess(nil)
	stopProcess(&serverProcess{})
}

func TestMemoryFloorErrorHelpers(t *testing.T) {
	err := &MemoryFloorError{Snapshot: MemorySnapshot{MemAvailableGiB: 1}, MinGiB: 2}
	if !strings.Contains(err.Error(), "below floor") {
		t.Fatalf("error string = %s", err.Error())
	}
	if !IsMemoryFloorError(err) || IsMemoryFloorError(errors.New("other")) {
		t.Fatal("IsMemoryFloorError mismatch")
	}
}

func TestSortReportRowsTieBreakers(t *testing.T) {
	rows := []ReportRow{
		{Profile: "b", Context: 1, Workload: "w", Concurrency: 1, ResultFile: "a"},
		{Profile: "a", Context: 2, Workload: "w", Concurrency: 1, ResultFile: "a"},
		{Profile: "a", Context: 1, Workload: "z", Concurrency: 1, ResultFile: "a"},
		{Profile: "a", Context: 1, Workload: "w", Concurrency: 2, ResultFile: "a"},
		{Profile: "a", Context: 1, Workload: "w", Concurrency: 1, ResultFile: "b"},
	}
	sortReportRows(rows)
	got := []string{
		rows[0].Profile,
		rows[1].ResultFile,
		rows[2].Workload,
		intCSV(rows[3].Context),
		rows[4].Profile,
	}
	want := []string{"a", "a", "z", "2", "b"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("sorted markers = %#v, want %#v", got, want)
	}
}
