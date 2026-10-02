package report

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/osolmaz/localperf/internal/convergence"
)

// loadSQLiteReportConvergence attaches the latest recorded convergence
// decision of each point to every measurement of that point.
func loadSQLiteReportConvergence(db *sql.DB, doc *SQLiteReportDocument) error {
	rows, err := db.Query(`SELECT m.run_id, m.profile_id, m.workload_id, m.concurrency, COALESCE(e.data_json, '')
		FROM events e
		JOIN measurements m ON m.id = e.measurement_id
		WHERE e.type = ?
		ORDER BY e.id`, convergence.StopEventType)
	if err != nil {
		return err
	}
	defer rows.Close()
	latest := map[string]*convergence.StopRecord{}
	for rows.Next() {
		var runID, profileID, workloadID, data string
		var concurrency int
		if err := rows.Scan(&runID, &profileID, &workloadID, &concurrency, &data); err != nil {
			return err
		}
		var record convergence.StopRecord
		if err := json.Unmarshal([]byte(data), &record); err != nil {
			return fmt.Errorf("point_stopped event: %w", err)
		}
		latest[convergencePointKey(runID, profileID, workloadID, concurrency)] = &record
	}
	if err := rows.Err(); err != nil {
		return err
	}
	for index := range doc.Measurements {
		measurement := &doc.Measurements[index]
		measurement.Convergence = latest[convergencePointKey(measurement.RunID, measurement.ProfileID, measurement.WorkloadID, measurement.Concurrency)]
	}
	return nil
}

func convergencePointKey(runID, profileID, workloadID string, concurrency int) string {
	return fmt.Sprintf("%s\x00%s\x00%s\x00%d", runID, profileID, workloadID, concurrency)
}

// measuredRepeats drops samples that convergence skipped, so a converged
// point keeps the status and values of the samples it ran. A point whose
// every sample was skipped keeps them all.
func measuredRepeats(members []SQLiteReportMeasurement) []SQLiteReportMeasurement {
	measured := make([]SQLiteReportMeasurement, 0, len(members))
	for _, member := range members {
		if member.Convergence != nil && strings.EqualFold(strings.TrimSpace(member.Status), "skipped") {
			continue
		}
		measured = append(measured, member)
	}
	if len(measured) == 0 {
		return members
	}
	return measured
}

// sharedConvergence is the decision of a combined row only when every member
// belongs to that one decision. Repeats pooled across runs have no single
// decision, so the row shows none rather than one run's interval.
func sharedConvergence(members []SQLiteReportMeasurement) *convergence.StopRecord {
	record := members[0].Convergence
	for _, member := range members[1:] {
		if member.Convergence != record {
			return nil
		}
	}
	return record
}

// convergenceDetailItems shows the stop reason and the 95% interval of the
// point's throughput in the metric-cell details.
func convergenceDetailItems(record *convergence.StopRecord) []SQLiteReportMetadataItem {
	if record == nil {
		return nil
	}
	items := []SQLiteReportMetadataItem{{Label: "Stop reason", Value: fmt.Sprintf("%s after %d sample(s)", record.Reason, record.N)}}
	if record.IntervalKnown {
		items = append(items, SQLiteReportMetadataItem{
			Label: "95% interval",
			Value: fmt.Sprintf("%s ± %s tok/s (±%.1f%%, n=%d)", FormatRateDisplay(displayFloat(record.Mean)), FormatRateDisplay(displayFloat(record.HalfWidth)), record.RelHalfWidth*100, record.N),
		})
	}
	return items
}

// ConvergenceNote is the headline marker of a point that stopped without a
// converged result; it is empty for converged and fixed-count points.
func ConvergenceNote(record *convergence.StopRecord) string {
	if record == nil {
		return ""
	}
	switch record.Reason {
	case convergence.ReasonMaxRepeats, convergence.ReasonTimeBudget, convergence.ReasonFailed:
	default:
		return ""
	}
	if record.IntervalKnown {
		return fmt.Sprintf("not converged: %s, ±%.1f%% at n=%d", record.Reason, record.RelHalfWidth*100, record.N)
	}
	return fmt.Sprintf("not converged: %s at n=%d", record.Reason, record.N)
}
