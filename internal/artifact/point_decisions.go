package artifact

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"math"
	"sort"

	"github.com/osolmaz/localperf/internal/convergence"
)

// pointMetricColumns maps a recorded convergence metric to the measurements
// column that stores it.
var pointMetricColumns = map[string]string{
	"aggregate_output_tok_s": "aggregate_output_tok_s",
	"aggregate_total_tok_s":  "aggregate_total_tok_s",
}

type pointDecision struct {
	eventID     int64
	runID       string
	profileID   string
	workloadID  string
	concurrency int
	record      convergence.StopRecord
}

// checkPointDecisions recomputes every recorded convergence decision from
// its own samples and policy. The latest decision of each point must also
// match the point's completed measurement rows, so a stored decision cannot
// disagree with the stored data.
func checkPointDecisions(db *sql.DB) error {
	var unlinked int
	if err := db.QueryRow(`SELECT COUNT(*) FROM events WHERE type = ? AND measurement_id IS NULL`, convergence.StopEventType).Scan(&unlinked); err != nil {
		return err
	}
	if unlinked != 0 {
		return fmt.Errorf("%s events without a measurement = %d", convergence.StopEventType, unlinked)
	}
	decisions, err := readPointDecisions(db)
	if err != nil {
		return err
	}
	latest := map[string]pointDecision{}
	for _, decision := range decisions {
		if err := decision.record.Verify(); err != nil {
			return fmt.Errorf("%s event %d: %w", convergence.StopEventType, decision.eventID, err)
		}
		latest[decision.key()] = decision
	}
	for _, decision := range latest {
		if err := checkDecisionSamples(db, decision); err != nil {
			return fmt.Errorf("%s event %d: %w", convergence.StopEventType, decision.eventID, err)
		}
	}
	return nil
}

func (decision pointDecision) key() string {
	return fmt.Sprintf("%s\x00%s\x00%s\x00%d", decision.runID, decision.profileID, decision.workloadID, decision.concurrency)
}

func readPointDecisions(db *sql.DB) ([]pointDecision, error) {
	rows, err := db.Query(`SELECT event.id, COALESCE(event.data_json, ''), measurement.run_id, measurement.profile_id,
			measurement.workload_id, measurement.concurrency
		FROM events AS event
		JOIN measurements AS measurement ON measurement.id = event.measurement_id
		WHERE event.type = ?
		ORDER BY event.id`, convergence.StopEventType)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []pointDecision
	for rows.Next() {
		var decision pointDecision
		var data string
		if err := rows.Scan(&decision.eventID, &data, &decision.runID, &decision.profileID, &decision.workloadID, &decision.concurrency); err != nil {
			return nil, err
		}
		if err := json.Unmarshal([]byte(data), &decision.record); err != nil {
			return nil, fmt.Errorf("%s event %d: %w", convergence.StopEventType, decision.eventID, err)
		}
		out = append(out, decision)
	}
	return out, rows.Err()
}

// checkDecisionSamples compares the recorded values with the completed rows
// of the point. Order is not compared: a resumed point can fill a missing
// repeat after later repeats.
func checkDecisionSamples(db *sql.DB, decision pointDecision) error {
	column, ok := pointMetricColumns[decision.record.Metric]
	if !ok {
		return fmt.Errorf("unknown metric %q", decision.record.Metric)
	}
	// A completed row without a positive value is not a sample; the runner
	// stopped the point as failed on it.
	rows, err := db.Query(fmt.Sprintf(`SELECT %[1]s FROM measurements
		WHERE run_id = ? AND profile_id = ? AND workload_id = ? AND concurrency = ? AND status = 'completed'
		  AND COALESCE(%[1]s, 0) > 0`, column),
		decision.runID, decision.profileID, decision.workloadID, decision.concurrency)
	if err != nil {
		return err
	}
	defer rows.Close()
	var stored []float64
	for rows.Next() {
		var value float64
		if err := rows.Scan(&value); err != nil {
			return err
		}
		stored = append(stored, value)
	}
	if err := rows.Err(); err != nil {
		return err
	}
	if !sameValues(stored, decision.record.Values) {
		return fmt.Errorf("recorded %s values %v, completed measurements %v", decision.record.Metric, decision.record.Values, stored)
	}
	return nil
}

func sameValues(a, b []float64) bool {
	if len(a) != len(b) {
		return false
	}
	left := append([]float64{}, a...)
	right := append([]float64{}, b...)
	sort.Float64s(left)
	sort.Float64s(right)
	for index := range left {
		if math.Abs(left[index]-right[index]) > 1e-9*math.Max(1, math.Abs(left[index])) {
			return false
		}
	}
	return true
}
