package plane

import (
	"context"
	"time"
)

// nodeClock is the node registry's and node stream's time source. The
// production clock is time.Now, whose instants keep their monotonic
// reading, so lease comparisons never depend on the wall clock. Tests
// inject a manually advanced clock through deps.nodeClock only; no flag or
// environment variable selects a clock.
type nodeClock interface {
	Now() time.Time
	// NewTimer returns a channel that receives once after d and a stop
	// function reporting whether it stopped an active timer.
	NewTimer(d time.Duration) (<-chan time.Time, func() bool)
	// NewTicker returns a channel that receives every d (dropping ticks
	// while it is full) and a stop function.
	NewTicker(d time.Duration) (<-chan time.Time, func())
}

// realClock adapts the time package.
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

// clockTimeout returns a context canceled when parent ends or when clock
// reaches now+d. Its cancel function is synchronous: when it returns, the
// watcher goroutine has ended and the timer is stopped.
func clockTimeout(parent context.Context, clock nodeClock, d time.Duration) (context.Context, context.CancelFunc) {
	ctx, cancel := context.WithCancel(parent)
	c, stop := clock.NewTimer(d)
	done := make(chan struct{})
	go func() {
		defer close(done)
		select {
		case <-c:
			cancel()
		case <-ctx.Done():
		}
	}()
	return ctx, func() {
		cancel()
		<-done
		stop()
	}
}
