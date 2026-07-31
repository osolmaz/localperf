package vllmbench

import (
	"encoding/json"
	"math"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/osolmaz/localperf/internal/collections"
)

func TestParseResultFileRequiresCanonicalObject(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "result.json")
	writeFile(t, path, `{"dataset_name":"random","max_concurrency":4,"random_input_len":1024,"random_output_len":256,"completed":8,"failed":0,"total_input_tokens":8192,"total_output_tokens":2048,"total_tokens":10240,"duration":2,"output_throughput":40,"total_token_throughput":80,"mean_ttft_ms":12.5}`)
	rows, err := parseResultFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 {
		t.Fatalf("rows = %d, want 1", len(rows))
	}
	row := rows[0]
	if row.DatasetName != "random" || row.InputLen != 1024 || row.OutputLen != 256 || row.Phase != "decode" {
		t.Fatalf("canonical result not parsed: %+v", row)
	}
	if row.Completed != 8 || row.PerUserOutputTokSec != 10 {
		t.Fatalf("canonical counts/throughput = %d/%v, want 8/10", row.Completed, row.PerUserOutputTokSec)
	}

	for name, content := range map[string]string{
		"array":   `[{"completed":1}]`,
		"jsonl":   "{\"completed\":1}\n{\"completed\":1}\n",
		"aliases": `{"ok":1,"errors":0,"wall_seconds":1,"aggregate_completion_tokens_per_second":2}`,
	} {
		path := filepath.Join(dir, name+".json")
		writeFile(t, path, content)
		rows, err := parseResultFile(path)
		if name == "aliases" {
			if err != nil {
				t.Fatal(err)
			}
			if rows[0].Completed != 0 || rows[0].DurationSeconds != 0 || rows[0].OutputTokensPerSec != 0 {
				t.Fatalf("removed aliases still affected result: %+v", rows[0])
			}
			continue
		}
		if err == nil {
			t.Fatalf("%s result was accepted", name)
		}
	}
}

func TestParseResultFileDerivesExactTotalTokens(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "vllm-result.json")
	writeFile(t, path, `{"max_concurrency":1,"completed":4,"failed":0,"total_input_tokens":1014,"total_output_tokens":64,"duration":1.7,"output_throughput":37.5,"total_token_throughput":633}`)

	rows, err := parseResultFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 {
		t.Fatalf("rows = %d, want 1", len(rows))
	}
	row := rows[0]
	if row.TotalTokens != 1078 || !row.totalTokensKnown {
		t.Fatalf("total tokens = %d, known = %v; want 1078, true", row.TotalTokens, row.totalTokensKnown)
	}
	if err := validateResultPoint(row, "workload", 4, 1); err != nil {
		t.Fatalf("current vLLM result rejected: %v", err)
	}
}

func TestReportTrafficLengthHelpers(t *testing.T) {
	customOutput := 77
	shareGPTOutput := 88
	cases := []struct {
		name      string
		traffic   BenchmarkTrafficConfig
		wantInput int
		wantOut   int
	}{
		{
			name:      "random",
			traffic:   BenchmarkTrafficConfig{DatasetName: "random", RandomInputLen: 100, RandomOutputLen: 20},
			wantInput: 100,
			wantOut:   20,
		},
		{
			name:      "sonnet",
			traffic:   BenchmarkTrafficConfig{DatasetName: "sonnet", SonnetInputLen: 200, SonnetOutputLen: 30},
			wantInput: 200,
			wantOut:   30,
		},
		{
			name:      "prefix repetition",
			traffic:   BenchmarkTrafficConfig{DatasetName: "prefix_repetition", PrefixRepetitionPrefixLen: 300, PrefixRepetitionSuffixLen: 40, PrefixRepetitionOutputLen: 50},
			wantInput: 340,
			wantOut:   50,
		},
		{
			name:    "custom",
			traffic: BenchmarkTrafficConfig{DatasetName: "custom", CustomOutputLen: &customOutput},
			wantOut: customOutput,
		},
		{
			name:    "sharegpt",
			traffic: BenchmarkTrafficConfig{DatasetName: "sharegpt", ShareGPTOutputLen: &shareGPTOutput},
			wantOut: shareGPTOutput,
		},
		{
			name:    "speed bench",
			traffic: BenchmarkTrafficConfig{DatasetName: "speed_bench", SpeedBenchOutputLen: 99},
			wantOut: 99,
		},
		{
			name:    "unknown",
			traffic: BenchmarkTrafficConfig{DatasetName: "unknown"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := trafficInputLen(tc.traffic); got != tc.wantInput {
				t.Fatalf("input len = %d, want %d", got, tc.wantInput)
			}
			if got := trafficOutputLen(tc.traffic); got != tc.wantOut {
				t.Fatalf("output len = %d, want %d", got, tc.wantOut)
			}
		})
	}
}

func TestReportValueAndCellHelpers(t *testing.T) {
	row := map[string]any{
		"string":      "value",
		"string_int":  12,
		"int":         3,
		"int64":       int64(4),
		"float":       5.9,
		"json_int":    json.Number("6"),
		"json_float":  json.Number("7.25"),
		"invalid_int": "x",
	}
	if stringValue(row, "string_int") != "12" || stringValue(row, "missing") != "" {
		t.Fatalf("stringValue returned unexpected values")
	}
	if intValue(row, "int") != 3 || intValue(row, "int64") != 4 || intValue(row, "float") != 5 || intValue(row, "json_int") != 6 {
		t.Fatalf("intValue returned unexpected values")
	}
	if intValue(row, "invalid_int") != 0 || intValue(row, "missing") != 0 {
		t.Fatalf("intValue fallback failed")
	}
	if floatValue(row, "int") != 3 || floatValue(row, "int64") != 4 || floatValue(row, "float") != 5.9 || floatValue(row, "json_float") != 7.25 {
		t.Fatalf("floatValue returned unexpected values")
	}
	if floatValue(row, "invalid_int") != 0 || floatValue(row, "missing") != 0 {
		t.Fatalf("floatValue fallback failed")
	}
	if cell(" a|b ") != "a\\|b" || cell(" ") != "-" {
		t.Fatalf("cell escaping failed")
	}
	if intCell(0) != "-" || intCell(9) != "9" || intCSV(0) != "" || intCSV(9) != "9" {
		t.Fatalf("integer cell helpers failed")
	}
	if floatCell(0) != "-" || floatCell(math.NaN()) != "-" || floatCell(math.Inf(1)) != "-" || floatCell(1.25) != "1.2" {
		t.Fatalf("floatCell returned unexpected values")
	}
	if floatCSV(0) != "" || floatCSV(math.NaN()) != "" || floatCSV(math.Inf(1)) != "" || floatCSV(1.25) != "1.25" {
		t.Fatalf("floatCSV returned unexpected values")
	}
}

func TestReportPathHelpers(t *testing.T) {
	dir := t.TempDir()
	runDir := filepath.Join(dir, "run")
	resultPath := filepath.Join(runDir, "results", "one.json")
	writeFile(t, resultPath, "{}")

	if resolveResultPath(runDir, "") != "" {
		t.Fatal("empty result path should remain empty")
	}
	if resolveResultPath(runDir, resultPath) != resultPath {
		t.Fatal("absolute result path should remain unchanged")
	}
	if got := resolveResultPath(runDir, filepath.Join("results", "one.json")); got != resultPath {
		t.Fatalf("resolved path = %s, want %s", got, resultPath)
	}
	if got := resolveResultPath(runDir, filepath.Join(filepath.Base(runDir), "results", "one.json")); got != resultPath {
		t.Fatalf("stripped path = %s, want %s", got, resultPath)
	}
	if got := fileCell(runDir, resultPath); got != filepath.Join("results", "one.json") {
		t.Fatalf("file cell = %s", got)
	}
	if got := fileCell(runDir, filepath.Join(dir, "outside.json")); got != filepath.Join(dir, "outside.json") {
		t.Fatalf("outside file cell = %s", got)
	}
	if stripped, ok := stripRunDirPrefix(runDir, filepath.Join(filepath.Base(runDir), "results", "one.json")); !ok || stripped != filepath.Join("results", "one.json") {
		t.Fatalf("stripRunDirPrefix = %q, %t", stripped, ok)
	}
}

func TestReportPhasesGroupRowsByExplicitAndInferredPhase(t *testing.T) {
	rows := []ReportRow{
		{Profile: "p", Workload: "long-input", InputLen: 4096, OutputLen: 16},
		{Profile: "p", Workload: "explicit decode", Phase: "decode", InputLen: 1024, OutputLen: 16},
		{Profile: "p", Workload: "mixed", InputLen: 128, OutputLen: 16},
	}
	for index := range rows {
		deriveReportRowFields(&rows[index])
	}
	phases := reportPhases(rows)
	if len(phases) != 3 || phases[0] != "decode" || phases[1] != "prefill" || phases[2] != "mixed" {
		t.Fatalf("phases = %v, want decode/prefill/mixed order", phases)
	}
	for _, phase := range phases {
		if len(rowsForPhase(rows, phase)) != 1 {
			t.Fatalf("phase %q rows = %d, want 1", phase, len(rowsForPhase(rows, phase)))
		}
	}
	if phase := reportRowPhase(ReportRow{Profile: "p", Workload: "default-mixed", Phase: "mixed", InputLen: 4096, OutputLen: 16}); phase != "prefill" {
		t.Fatalf("default mixed row phase = %q, want prefill", phase)
	}
}

func TestSortedProfileNames(t *testing.T) {
	got := collections.SortedKeys(map[string]Profile{
		"z": {Name: "z"},
		"a": {Name: "a"},
		"m": {Name: "m"},
	})
	if !reflect.DeepEqual(got, []string{"a", "m", "z"}) {
		t.Fatalf("sorted names = %#v", got)
	}
}
