package scale

import (
	"math"
	"time"
)

// Steady decides when a weight can be trusted, which is stricter than the
// indicator's own motion flag.
//
// The flag lags. In the yard's recording both indicators still called 0 to 60 lb
// and 2,800 lb "not moving" for a line or three while a truck was rolling on or
// off, before switching to motion. So a weight counts as steady only once every
// line for Settle has been flagged still and stayed within Tolerance of where
// the run started. A one-graduation flicker (2,820 / 2,840) does not restart it.
type Steady struct {
	Settle    time.Duration
	Tolerance float64

	anchor float64
	since  time.Time // when the current still run began; zero when moving
}

// Observe feeds the next line and reports whether the weight is steady, and
// since when the current still run has lasted.
func (s *Steady) Observe(f Frame, at time.Time) (steady bool, since time.Time) {
	if !f.Still() {
		s.since = time.Time{}
		return false, time.Time{}
	}
	if s.since.IsZero() || math.Abs(f.Weight-s.anchor) > s.Tolerance {
		s.anchor = f.Weight
		s.since = at
	}
	return at.Sub(s.since) >= s.Settle, s.since
}

// Reset forgets the current run, for when the line to the scale drops.
func (s *Steady) Reset() { s.since = time.Time{} }
