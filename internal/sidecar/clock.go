package sidecar

import (
	"context"
	"time"
)

// clock is the sidecar's time source for heartbeat ticks, acknowledgement
// and hello timeouts, write deadlines and reconnect backoff. Production
// uses the time package; tests inject a manually advanced clock through
// deps.clock only, never through a flag or an environment variable.
type clock interface {
	Now() time.Time
	// NewTimer returns a channel that receives once after d and a stop
	// function reporting whether it stopped an active timer.
	NewTimer(d time.Duration) (<-chan time.Time, func() bool)
	// NewTicker returns a channel that receives every d (dropping ticks
	// while it is full, so missed heartbeats coalesce) and a stop function.
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

// clockTimeout returns a context canceled when parent ends or when c
// reaches now+d. Its cancel function is synchronous: when it returns, the
// watcher goroutine has ended and the timer is stopped, so nothing of the
// bound outlives the operation it bounded.
func clockTimeout(parent context.Context, c clock, d time.Duration) (context.Context, context.CancelFunc) {
	ctx, cancel := context.WithCancel(parent)
	ch, stop := c.NewTimer(d)
	done := make(chan struct{})
	go func() {
		defer close(done)
		select {
		case <-ch:
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
