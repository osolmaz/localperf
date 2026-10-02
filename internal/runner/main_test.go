package runner

import (
	"os"
	"testing"
)

// TestMain keeps context snapshots of every test run out of the user cache
// directory.
func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "localperf-snapshots-")
	if err != nil {
		panic(err)
	}
	if err := os.Setenv("LOCALPERF_SNAPSHOT_DIR", dir); err != nil {
		panic(err)
	}
	code := m.Run()
	_ = os.RemoveAll(dir)
	os.Exit(code)
}
