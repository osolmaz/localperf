package runner

import (
	"database/sql"
	"fmt"
	"os"
	"strings"

	"github.com/osolmaz/localperf/internal/artifact"
)

// CheckRunOverwrite refuses to start a run that would replace an existing run
// unless the caller passes an explicit override. Two conditions trigger the
// refusal, and either alone is enough:
//
//  1. the run directory already holds a previous attempt. Re-running the same
//     run directory rewrites its execution files and, at artifact write time,
//     replaces that run's rows in the artifact wholesale (replaceExistingRun);
//  2. the target artifact already records a run with the same id, which is the
//     run directory basename (sqliteRunID).
//
// allowReplace (--replace) is the explicit override. Resume bypasses the check
// by design: resuming an interrupted run is the documented way to reuse its
// run directory, and the caller has already named it.
func CheckRunOverwrite(runDir string, spec Spec, artifactOverride string, allowReplace bool) error {
	if allowReplace {
		return nil
	}
	runID := sqliteRunID(runDir, spec)
	var reasons []string
	if entries, hasPrior := priorAttemptFiles(runDir); hasPrior {
		reasons = append(reasons, fmt.Sprintf(
			"run directory %s already holds a previous attempt (%s)",
			runDir, strings.Join(entries, ", ")))
	}
	artifactPath := artifact.Path(runDir, artifactOverride)
	runExists, measurementRows, err := artifactRunRows(artifactPath, runID)
	if err != nil {
		return err
	}
	if runExists {
		reasons = append(reasons, fmt.Sprintf(
			"artifact %s already records run %q (%d measurement row(s))",
			artifactPath, runID, measurementRows))
	}
	if len(reasons) == 0 {
		return nil
	}
	return fmt.Errorf(
		"this run would replace existing results: %s. Pass --replace to overwrite on purpose, or choose a fresh --run-dir",
		strings.Join(reasons, "; "))
}

// priorAttemptFiles lists the entries of an existing run directory. Any
// content counts as a previous attempt: at check time the run has not written
// anything yet, so whatever is there came from an earlier invocation.
func priorAttemptFiles(runDir string) ([]string, bool) {
	entries, err := os.ReadDir(runDir)
	if err != nil || len(entries) == 0 {
		return nil, false
	}
	names := make([]string, 0, len(entries))
	for i, entry := range entries {
		if i == 4 {
			names = append(names, fmt.Sprintf("and %d more", len(entries)-4))
			break
		}
		names = append(names, entry.Name())
	}
	return names, true
}

// artifactRunRows reports whether the artifact file exists and already records
// the run id, together with the number of measurement rows recorded for it.
// A missing or empty file is the normal fresh case. An existing file that
// cannot be queried is an error, not a silent pass.
func artifactRunRows(artifactPath, runID string) (runExists bool, measurementRows int64, err error) {
	if strings.TrimSpace(artifactPath) == "" {
		return false, 0, nil
	}
	info, statErr := os.Stat(artifactPath)
	if statErr != nil {
		if os.IsNotExist(statErr) {
			return false, 0, nil
		}
		return false, 0, fmt.Errorf("stat artifact %s: %w", artifactPath, statErr)
	}
	if info.Size() == 0 {
		return false, 0, nil
	}
	db, err := sql.Open("sqlite", artifactPath)
	if err != nil {
		return false, 0, fmt.Errorf("open artifact %s: %w", artifactPath, err)
	}
	defer db.Close()
	var runCount int64
	err = db.QueryRow(
		`SELECT (SELECT count(*) FROM run WHERE id = ?), (SELECT count(*) FROM measurements WHERE run_id = ?)`,
		runID, runID).Scan(&runCount, &measurementRows)
	if err != nil {
		return false, 0, fmt.Errorf("query artifact %s for run %q: %w", artifactPath, runID, err)
	}
	return runCount > 0, measurementRows, nil
}
