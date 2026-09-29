package mcpqual

import "time"

// Clock is the monotonic time seam of the probe, the scheduler and the
// cleanup. Tests inject testkit.FakeClock (which satisfies it structurally)
// and advance it only after observing the timer they expect armed.
type Clock interface {
	Now() time.Time
	NewTimer(d time.Duration) (<-chan time.Time, func() bool)
	NewTicker(d time.Duration) (<-chan time.Time, func())
}

type realClock struct{}

func (realClock) Now() time.Time { return time.Now() }

func (realClock) NewTimer(d time.Duration) (<-chan time.Time, func() bool) {
	t := time.NewTimer(d)
	return t.C, t.Stop
}

func (realClock) NewTicker(d time.Duration) (<-chan time.Time, func()) {
	t := time.NewTicker(d)
	return t.C, t.Stop
}

// RealClock is the production clock (monotonic readings from time.Now).
var RealClock Clock = realClock{}
