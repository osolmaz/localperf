// Package convergence decides when a benchmark point has enough samples.
//
// A point repeats until the 95% Student's t confidence interval of its sample
// mean is narrow enough relative to the mean, until it reaches a repeat limit,
// or until the next sample would exceed a time limit. See
// docs/2026-10-02-adaptive-convergence.md.
package convergence

import (
	"errors"
	"fmt"
	"math"
	"strings"
	"time"
)

// MaxRepeatsLimit is the largest allowed max_repeats; the t table covers
// every degree of freedom up to MaxRepeatsLimit-1.
const MaxRepeatsLimit = 31

// tTable holds two-sided 95% Student's t values for 1 to 30 degrees of
// freedom.
var tTable = [MaxRepeatsLimit - 1]float64{
	12.706, 4.303, 3.182, 2.776, 2.571, 2.447, 2.365, 2.306, 2.262, 2.228,
	2.201, 2.179, 2.160, 2.145, 2.131, 2.120, 2.110, 2.101, 2.093, 2.086,
	2.080, 2.074, 2.069, 2.064, 2.060, 2.056, 2.052, 2.048, 2.045, 2.042,
}

// Policy is the convergence block of a suite case or workload. A zero
// TargetRelHalfWidth means a fixed count, which requires MinRepeats ==
// MaxRepeats. A zero MaxPointSeconds means no time limit.
type Policy struct {
	MinRepeats         int     `json:"min_repeats"`
	MaxRepeats         int     `json:"max_repeats"`
	TargetRelHalfWidth float64 `json:"target_rel_half_width,omitempty"`
	MaxPointSeconds    float64 `json:"max_point_seconds,omitempty"`
}

// Fixed returns a fixed-count policy of n repeats.
func Fixed(n int) Policy {
	return Policy{MinRepeats: n, MaxRepeats: n}
}

// IsFixed reports whether the policy runs an exact repeat count.
func (policy Policy) IsFixed() bool {
	return policy.TargetRelHalfWidth == 0
}

// Validate returns every bound the policy breaks, prefixed for the caller.
func (policy Policy) Validate(prefix string) error {
	var issues []string
	if policy.MinRepeats < 1 {
		issues = append(issues, "min_repeats must be at least 1")
	}
	if policy.MaxRepeats < policy.MinRepeats {
		issues = append(issues, "max_repeats must be at least min_repeats")
	}
	if policy.MaxRepeats > MaxRepeatsLimit {
		issues = append(issues, fmt.Sprintf("max_repeats must be at most %d", MaxRepeatsLimit))
	}
	if policy.TargetRelHalfWidth < 0 || policy.TargetRelHalfWidth >= 1 {
		issues = append(issues, "target_rel_half_width must be greater than 0 and less than 1")
	}
	if policy.IsFixed() && policy.MinRepeats != policy.MaxRepeats {
		issues = append(issues, "a policy without target_rel_half_width must set min_repeats equal to max_repeats")
	}
	if policy.MaxPointSeconds < 0 {
		issues = append(issues, "max_point_seconds must not be negative")
	}
	if len(issues) == 0 {
		return nil
	}
	return errors.New(prefix + ": convergence " + strings.Join(issues, "; convergence "))
}

// Reason is the recorded stop reason of a point.
type Reason string

const (
	ReasonConverged  Reason = "converged"
	ReasonMaxRepeats Reason = "max_repeats"
	ReasonTimeBudget Reason = "time_budget"
	ReasonFixed      Reason = "fixed"
	ReasonFailed     Reason = "failed"
)

// Interval is the 95% confidence interval of a sample mean. Known is false
// with fewer than two samples, where no interval exists.
type Interval struct {
	N            int     `json:"n"`
	Mean         float64 `json:"mean"`
	HalfWidth    float64 `json:"half_width,omitempty"`
	RelHalfWidth float64 `json:"rel_half_width,omitempty"`
	Known        bool    `json:"known"`
}

// Estimate returns the mean and, from two samples on, the 95% interval.
func Estimate(values []float64) Interval {
	n := len(values)
	if n == 0 {
		return Interval{}
	}
	sum := 0.0
	for _, value := range values {
		sum += value
	}
	mean := sum / float64(n)
	if n < 2 || n > MaxRepeatsLimit {
		return Interval{N: n, Mean: mean}
	}
	squares := 0.0
	for _, value := range values {
		squares += (value - mean) * (value - mean)
	}
	halfWidth := tTable[n-2] * math.Sqrt(squares/float64(n-1)) / math.Sqrt(float64(n))
	interval := Interval{N: n, Mean: mean, HalfWidth: halfWidth, Known: true}
	if mean > 0 {
		interval.RelHalfWidth = halfWidth / mean
	}
	return interval
}

// Low and High are the interval bounds; without an interval both equal the
// mean.
func (interval Interval) Low() float64  { return interval.Mean - interval.HalfWidth }
func (interval Interval) High() float64 { return interval.Mean + interval.HalfWidth }

// Decision is the outcome of evaluating a point after a sample.
type Decision struct {
	Stop   bool   `json:"stop"`
	Reason Reason `json:"reason,omitempty"`
	Interval
}

// Evaluate decides whether a point with these successful sample values and
// durations stops. Durations estimate the next sample for the time limit.
func Evaluate(values []float64, durations []time.Duration, policy Policy) Decision {
	interval := Estimate(values)
	n := len(values)
	switch {
	case !policy.IsFixed() && n >= policy.MinRepeats && converged(interval, policy):
		return Decision{Stop: true, Reason: ReasonConverged, Interval: interval}
	case n >= policy.MaxRepeats && policy.IsFixed():
		return Decision{Stop: true, Reason: ReasonFixed, Interval: interval}
	case n >= policy.MaxRepeats:
		return Decision{Stop: true, Reason: ReasonMaxRepeats, Interval: interval}
	case overBudget(durations, policy):
		return Decision{Stop: true, Reason: ReasonTimeBudget, Interval: interval}
	}
	return Decision{Interval: interval}
}

// Failed is the decision for a point whose latest sample failed; the
// interval covers only the successful samples before it.
func Failed(values []float64) Decision {
	return Decision{Stop: true, Reason: ReasonFailed, Interval: Estimate(values)}
}

func converged(interval Interval, policy Policy) bool {
	return interval.Known && interval.Mean > 0 && interval.HalfWidth <= policy.TargetRelHalfWidth*interval.Mean
}

// overBudget reports whether the next sample, estimated as the mean duration
// of the previous samples, would end after MaxPointSeconds.
func overBudget(durations []time.Duration, policy Policy) bool {
	if policy.MaxPointSeconds <= 0 || len(durations) == 0 {
		return false
	}
	var elapsed time.Duration
	for _, duration := range durations {
		elapsed += duration
	}
	next := elapsed / time.Duration(len(durations))
	return (elapsed + next).Seconds() > policy.MaxPointSeconds
}
