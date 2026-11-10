package scale

import (
	"testing"
	"time"
)

func TestSteady(t *testing.T) {
	t0 := time.Date(2026, 11, 10, 15, 6, 45, 0, time.UTC)
	at := func(ms int) time.Time { return t0.Add(time.Duration(ms) * time.Millisecond) }
	still := func(w float64) Frame { return Frame{Weight: w, Unit: "lb", Mode: "gross", Status: StatusOK} }
	moving := func(w float64) Frame { return Frame{Weight: w, Unit: "lb", Mode: "gross", Status: StatusMotion} }

	s := Steady{Settle: time.Second, Tolerance: 20}
	steps := []struct {
		ms     int
		frame  Frame
		steady bool
	}{
		// Rolling on: the indicator calls the first few lines still. Not steady.
		{0, still(20), false},
		{50, still(60), false},
		{100, moving(160), false},
		{1900, moving(2840), false},
		// Settled at 2,840: steady only after a full second of still lines.
		{2000, still(2840), false},
		{2950, still(2840), false},
		{3000, still(2840), true},
		// One graduation of flicker keeps it steady.
		{3200, still(2820), true},
		{3400, still(2840), true},
		// A jump beyond the tolerance starts a new run.
		{3600, still(2880), false},
		{4600, still(2880), true},
		// Motion ends it at once.
		{4650, moving(2700), false},
		{4700, still(2700), false},
	}
	for _, step := range steps {
		got, _ := s.Observe(step.frame, at(step.ms))
		if got != step.steady {
			t.Fatalf("at %dms weight %v status %s: steady=%v, want %v", step.ms, step.frame.Weight, step.frame.Status, got, step.steady)
		}
	}
}

func TestSteadyZeroCountsAsStill(t *testing.T) {
	t0 := time.Now()
	s := Steady{Settle: time.Second, Tolerance: 20}
	s.Observe(Frame{Status: StatusZero}, t0)
	if got, _ := s.Observe(Frame{Status: StatusZero}, t0.Add(time.Second)); !got {
		t.Fatal("a scale resting at zero should be steady")
	}
}

func TestSteadyReset(t *testing.T) {
	t0 := time.Now()
	s := Steady{Settle: time.Second, Tolerance: 20}
	s.Observe(Frame{Weight: 100, Status: StatusOK}, t0)
	s.Reset()
	if got, _ := s.Observe(Frame{Weight: 100, Status: StatusOK}, t0.Add(time.Second)); got {
		t.Fatal("a reset should start the settle time again")
	}
}
