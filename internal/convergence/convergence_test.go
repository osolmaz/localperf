package convergence

import (
	"math"
	"strings"
	"testing"
	"time"
)

var defaultPolicy = Policy{MinRepeats: 3, MaxRepeats: 10, TargetRelHalfWidth: 0.05, MaxPointSeconds: 600}

func seconds(values ...float64) []time.Duration {
	out := make([]time.Duration, len(values))
	for index, value := range values {
		out[index] = time.Duration(value * float64(time.Second))
	}
	return out
}

func TestTTableEdges(t *testing.T) {
	if tTable[0] != 12.706 || tTable[1] != 4.303 || tTable[len(tTable)-1] != 2.042 {
		t.Fatalf("t table edges = %v, %v, %v", tTable[0], tTable[1], tTable[len(tTable)-1])
	}
	for index := 1; index < len(tTable); index++ {
		if tTable[index] >= tTable[index-1] {
			t.Fatalf("t table must strictly decrease at df %d", index+1)
		}
	}
}

func TestEstimate(t *testing.T) {
	if got := Estimate(nil); got != (Interval{}) {
		t.Fatalf("empty estimate = %+v", got)
	}
	single := Estimate([]float64{42})
	if single.N != 1 || single.Mean != 42 || single.Known || single.HalfWidth != 0 {
		t.Fatalf("single estimate = %+v", single)
	}
	// mean 10, sample stddev 1, n 3: half width = 4.303 / sqrt(3).
	interval := Estimate([]float64{9, 10, 11})
	want := 4.303 / math.Sqrt(3)
	if !interval.Known || interval.Mean != 10 || math.Abs(interval.HalfWidth-want) > 1e-12 || math.Abs(interval.RelHalfWidth-want/10) > 1e-12 {
		t.Fatalf("estimate = %+v, want half width %v", interval, want)
	}
	if interval.Low() != 10-interval.HalfWidth || interval.High() != 10+interval.HalfWidth {
		t.Fatalf("bounds = %v..%v", interval.Low(), interval.High())
	}
	identical := Estimate([]float64{5, 5, 5})
	if !identical.Known || identical.HalfWidth != 0 || identical.RelHalfWidth != 0 {
		t.Fatalf("identical estimate = %+v", identical)
	}
	zero := Estimate([]float64{-1, 0, 1})
	if !zero.Known || zero.Mean != 0 || zero.RelHalfWidth != 0 {
		t.Fatalf("zero-mean estimate = %+v", zero)
	}
	tooMany := Estimate(make([]float64, MaxRepeatsLimit+1))
	if tooMany.Known {
		t.Fatalf("estimate beyond the t table must not report an interval: %+v", tooMany)
	}
}

func TestEvaluateConvergesOnlyFromMinRepeats(t *testing.T) {
	stable := []float64{100, 100.5, 99.5}
	if got := Evaluate(stable[:2], seconds(1, 1), defaultPolicy); got.Stop {
		t.Fatalf("two samples must not stop below min_repeats: %+v", got)
	}
	got := Evaluate(stable, seconds(1, 1, 1), defaultPolicy)
	if !got.Stop || got.Reason != ReasonConverged || got.N != 3 {
		t.Fatalf("stable point = %+v, want converged at 3", got)
	}
}

func TestEvaluateKeepsNoisyPointRunning(t *testing.T) {
	noisy := []float64{80, 100, 120}
	got := Evaluate(noisy, seconds(1, 1, 1), defaultPolicy)
	if got.Stop || got.Reason != "" || !got.Known {
		t.Fatalf("noisy point = %+v, want continue with an interval", got)
	}
}

func TestEvaluateBoundaryIsInclusive(t *testing.T) {
	// half width = 4.303 / sqrt(3) for values 9, 10, 11; set the target to
	// exactly that fraction of the mean.
	policy := Policy{MinRepeats: 3, MaxRepeats: 10, TargetRelHalfWidth: 4.303 / math.Sqrt(3) / 10}
	if got := Evaluate([]float64{9, 10, 11}, nil, policy); got.Reason != ReasonConverged {
		t.Fatalf("half width equal to the target = %+v, want converged", got)
	}
	policy.TargetRelHalfWidth *= 0.999
	if got := Evaluate([]float64{9, 10, 11}, nil, policy); got.Stop {
		t.Fatalf("half width above the target = %+v, want continue", got)
	}
}

func TestEvaluateMaxRepeats(t *testing.T) {
	policy := Policy{MinRepeats: 3, MaxRepeats: 4, TargetRelHalfWidth: 0.01}
	noisy := []float64{80, 100, 120, 90}
	if got := Evaluate(noisy[:3], nil, policy); got.Stop {
		t.Fatalf("below max_repeats = %+v, want continue", got)
	}
	got := Evaluate(noisy, nil, policy)
	if !got.Stop || got.Reason != ReasonMaxRepeats || got.N != 4 {
		t.Fatalf("at max_repeats = %+v", got)
	}
	converging := []float64{100, 100, 100, 100}
	if got := Evaluate(converging, nil, policy); got.Reason != ReasonConverged {
		t.Fatalf("converged at max_repeats = %+v, want converged", got)
	}
}

func TestEvaluateFixed(t *testing.T) {
	policy := Fixed(3)
	if !policy.IsFixed() {
		t.Fatal("Fixed policy must report IsFixed")
	}
	same := []float64{100, 100, 100}
	if got := Evaluate(same[:2], nil, policy); got.Stop {
		t.Fatalf("fixed below count = %+v, want continue", got)
	}
	if got := Evaluate(same, nil, policy); !got.Stop || got.Reason != ReasonFixed {
		t.Fatalf("fixed at count = %+v, want fixed", got)
	}
	if got := Evaluate([]float64{7}, nil, Fixed(1)); !got.Stop || got.Reason != ReasonFixed || got.Known {
		t.Fatalf("single fixed sample = %+v", got)
	}
}

func TestEvaluateTimeBudget(t *testing.T) {
	policy := Policy{MinRepeats: 3, MaxRepeats: 10, TargetRelHalfWidth: 0.01, MaxPointSeconds: 100}
	noisy := []float64{80, 120}
	// 2 x 40 s elapsed, next sample estimated 40 s: 120 s > 100 s.
	if got := Evaluate(noisy, seconds(40, 40), policy); !got.Stop || got.Reason != ReasonTimeBudget {
		t.Fatalf("over budget = %+v, want time_budget", got)
	}
	// 2 x 30 s elapsed, next estimated 30 s: 90 s fits.
	if got := Evaluate(noisy, seconds(30, 30), policy); got.Stop {
		t.Fatalf("within budget = %+v, want continue", got)
	}
	// Exactly at the limit still fits.
	if got := Evaluate(noisy, seconds(50, 0), policy); got.Stop {
		t.Fatalf("estimate ending at the limit = %+v, want continue", got)
	}
	unlimited := policy
	unlimited.MaxPointSeconds = 0
	if got := Evaluate(noisy, seconds(1000, 1000), unlimited); got.Stop {
		t.Fatalf("no time limit = %+v, want continue", got)
	}
	if got := Evaluate(nil, nil, policy); got.Stop {
		t.Fatalf("no samples yet = %+v, want continue", got)
	}
}

func TestEvaluateZeroMeanNeverConverges(t *testing.T) {
	if got := Evaluate([]float64{0, 0, 0}, nil, defaultPolicy); got.Stop {
		t.Fatalf("zero mean = %+v, want continue", got)
	}
}

func TestFailed(t *testing.T) {
	got := Failed([]float64{10, 12})
	if !got.Stop || got.Reason != ReasonFailed || got.N != 2 || !got.Known {
		t.Fatalf("failed = %+v", got)
	}
	if got := Failed(nil); !got.Stop || got.Reason != ReasonFailed || got.N != 0 {
		t.Fatalf("failed without samples = %+v", got)
	}
}

func TestValidate(t *testing.T) {
	valid := []Policy{defaultPolicy, Fixed(1), Fixed(MaxRepeatsLimit), {MinRepeats: 1, MaxRepeats: 31, TargetRelHalfWidth: 0.5}}
	for _, policy := range valid {
		if err := policy.Validate("case"); err != nil {
			t.Fatalf("Validate(%+v) = %v", policy, err)
		}
	}
	invalid := map[string]Policy{
		"min_repeats must be at least 1":            {MinRepeats: 0, MaxRepeats: 0},
		"max_repeats must be at least min_repeats":  {MinRepeats: 3, MaxRepeats: 2, TargetRelHalfWidth: 0.05},
		"max_repeats must be at most 31":            {MinRepeats: 3, MaxRepeats: 32, TargetRelHalfWidth: 0.05},
		"target_rel_half_width must be greater":     {MinRepeats: 3, MaxRepeats: 10, TargetRelHalfWidth: 1},
		"min_repeats equal to max_repeats":          {MinRepeats: 3, MaxRepeats: 10},
		"max_point_seconds must not be negative":    {MinRepeats: 3, MaxRepeats: 10, TargetRelHalfWidth: 0.05, MaxPointSeconds: -1},
		"target_rel_half_width must be greater tha": {MinRepeats: 3, MaxRepeats: 10, TargetRelHalfWidth: -0.1},
	}
	for want, policy := range invalid {
		err := policy.Validate("cases[0]")
		if err == nil || !strings.Contains(err.Error(), want) || !strings.HasPrefix(err.Error(), "cases[0]: convergence ") {
			t.Fatalf("Validate(%+v) = %v, want %q", policy, err, want)
		}
	}
}
