package benchcli

import (
	"flag"
	"testing"
)

func TestCommandSurfaceContainsOnlyValidatedWorkflows(t *testing.T) {
	for name := range rootHandlers {
		if name != "bench" && name != "artifact" && name != "sweep" && name != "view" {
			t.Fatalf("unexpected root command %q", name)
		}
	}
	if len(benchHandlers) != 2 || benchHandlers["plan"] == nil || benchHandlers["run"] == nil {
		t.Fatalf("bench commands = %v, want plan/run only", benchHandlers)
	}
	if _, ok := benchHandlers["http-load"]; ok {
		t.Fatal("raw HTTP load command is exposed")
	}
	if len(artifactHandlers) != 3 || artifactHandlers["check"] == nil || artifactHandlers["render"] == nil || artifactHandlers["merge"] == nil {
		t.Fatalf("artifact commands = %v, want check/render/merge only", artifactHandlers)
	}
	if _, ok := artifactHandlers["rebuild"]; ok {
		t.Fatal("raw run-directory artifact reconstruction is exposed")
	}
}

func TestParseArtifactRenderFlagsAllowsPathBeforeFlags(t *testing.T) {
	config, err := parseArtifactRenderFlags([]string{"run.sqlite", "--output", "report.html", "--store", "--title", "Run"}, flag.ContinueOnError)
	if err != nil {
		t.Fatal(err)
	}
	if config.path != "run.sqlite" || config.output != "report.html" || config.title != "Run" || !config.store {
		t.Fatalf("config = %+v, want path, output, title, store", config)
	}
}

func TestParseArtifactRenderFlagsAllowsPathBetweenFlags(t *testing.T) {
	config, err := parseArtifactRenderFlags([]string{"--output", "report.html", "run.sqlite", "--store"}, flag.ContinueOnError)
	if err != nil {
		t.Fatal(err)
	}
	if config.path != "run.sqlite" || config.output != "report.html" || !config.store {
		t.Fatalf("config = %+v, want interspersed path and store", config)
	}
}

func TestParseArtifactRenderFlagsAllowsEqualsValueFlags(t *testing.T) {
	config, err := parseArtifactRenderFlags([]string{"--output=report.html", "run.sqlite", "--title=Run", "--store=true"}, flag.ContinueOnError)
	if err != nil {
		t.Fatal(err)
	}
	if config.path != "run.sqlite" || config.output != "report.html" || config.title != "Run" || !config.store {
		t.Fatalf("config = %+v, want equals value flags", config)
	}
}

func TestParseArtifactRenderFlagsAllowsPathFlag(t *testing.T) {
	config, err := parseArtifactRenderFlags([]string{"--path", "run.sqlite", "--output", "report.html"}, flag.ContinueOnError)
	if err != nil {
		t.Fatal(err)
	}
	if config.path != "run.sqlite" || config.output != "report.html" {
		t.Fatalf("config = %+v, want flag path and output", config)
	}
}

func TestParseViewFlagsAllowsMultiplePathsAroundFlags(t *testing.T) {
	config, err := parseViewFlags([]string{"first.sqlite", "--addr", "127.0.0.1:8766", "second.sqlite", "--title", "Runs", "--open"}, flag.ContinueOnError)
	if err != nil {
		t.Fatal(err)
	}
	if config.addr != "127.0.0.1:8766" || config.title != "Runs" || !config.open {
		t.Fatalf("config = %+v, want addr, title, and open", config)
	}
	if len(config.paths) != 2 || config.paths[0] != "first.sqlite" || config.paths[1] != "second.sqlite" {
		t.Fatalf("paths = %v, want first and second", config.paths)
	}
}

func TestParseViewFlagsAllowsEqualsValueFlags(t *testing.T) {
	config, err := parseViewFlags([]string{"--addr=127.0.0.1:0", "--title=Reports", "run.sqlite", "--open=false"}, flag.ContinueOnError)
	if err != nil {
		t.Fatal(err)
	}
	if config.addr != "127.0.0.1:0" || config.title != "Reports" || config.open {
		t.Fatalf("config = %+v, want addr/title and open false", config)
	}
	if len(config.paths) != 1 || config.paths[0] != "run.sqlite" {
		t.Fatalf("paths = %v, want run.sqlite", config.paths)
	}
}

func TestParseViewFlagsRejectsMissingPath(t *testing.T) {
	if _, err := parseViewFlags([]string{"--addr", "127.0.0.1:0"}, flag.ContinueOnError); err == nil {
		t.Fatal("parseViewFlags error = nil, want missing path error")
	}
}
