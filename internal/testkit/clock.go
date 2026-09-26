package testkit

import (
	"errors"
	"fmt"
	"sort"
	"sync"
	"time"
)

// FakeClock is a manually advanced clock for lease, heartbeat and backoff
// tests (iteration 03). It satisfies the package-private clock interfaces
// of internal/plane and internal/sidecar structurally: Now, NewTimer and
// NewTicker. Time moves only through Advance; timers and tickers fire in
// deadline order, each tick channel has a buffer of one and drops ticks
// while it is full, as the time package's tickers do. Tests observe
// registrations with AwaitWaiter instead of sleeping.
type FakeClock struct {
	mu      sync.Mutex
	now     time.Time
	seq     int
	waiters map[int]*fakeWaiter
	changed chan struct{}
}

type fakeWaiter struct {
	id       int
	at       time.Time
	period   time.Duration // 0 for a one-shot timer
	duration time.Duration
	c        chan time.Time
}

// Waiter describes one active timer or ticker.
type Waiter struct {
	Duration time.Duration
	Ticker   bool
	At       time.Time
}

// NewFakeClock returns a clock reading start.
func NewFakeClock(start time.Time) *FakeClock {
	return &FakeClock{now: start, waiters: map[int]*fakeWaiter{}, changed: make(chan struct{})}
}

// Now returns the current fake instant.
func (c *FakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *FakeClock) notifyLocked() {
	close(c.changed)
	c.changed = make(chan struct{})
}

func (c *FakeClock) add(d, period time.Duration) *fakeWaiter {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.seq++
	w := &fakeWaiter{id: c.seq, at: c.now.Add(d), period: period, duration: d, c: make(chan time.Time, 1)}
	c.waiters[w.id] = w
	c.notifyLocked()
	return w
}

func (c *FakeClock) remove(id int) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	_, ok := c.waiters[id]
	if ok {
		delete(c.waiters, id)
		c.notifyLocked()
	}
	return ok
}

// NewTimer returns a one-shot timer channel firing once Advance reaches
// now+d, and its stop function (true if it stopped an active timer).
func (c *FakeClock) NewTimer(d time.Duration) (<-chan time.Time, func() bool) {
	w := c.add(d, 0)
	return w.c, func() bool { return c.remove(w.id) }
}

// NewTicker returns a channel ticking every d of advanced time and its
// stop function.
func (c *FakeClock) NewTicker(d time.Duration) (<-chan time.Time, func()) {
	if d <= 0 {
		panic("testkit: non-positive ticker period")
	}
	w := c.add(d, d)
	return w.c, func() { c.remove(w.id) }
}

// Advance moves the clock forward by d, delivering every due tick and
// timer in deadline order at its own instant.
func (c *FakeClock) Advance(d time.Duration) {
	if d < 0 {
		panic("testkit: negative advance")
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	end := c.now.Add(d)
	for {
		var next *fakeWaiter
		for _, w := range c.waiters {
			if !w.at.After(end) && (next == nil || w.at.Before(next.at) || (w.at.Equal(next.at) && w.id < next.id)) {
				next = w
			}
		}
		if next == nil {
			break
		}
		if next.at.After(c.now) {
			c.now = next.at
		}
		select {
		case next.c <- c.now:
		default: // a full tick channel drops the tick
		}
		if next.period > 0 {
			next.at = next.at.Add(next.period)
		} else {
			delete(c.waiters, next.id)
		}
	}
	c.now = end
	c.notifyLocked()
}

// Set moves the clock to t, forward or backward, without firing anything
// that becomes due; it models a wall-clock step for instants that carry no
// monotonic reading.
func (c *FakeClock) Set(t time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = t
	c.notifyLocked()
}

// Waiters lists the active timers and tickers in deadline order.
func (c *FakeClock) Waiters() []Waiter {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.waitersLocked()
}

func (c *FakeClock) waitersLocked() []Waiter {
	out := make([]Waiter, 0, len(c.waiters))
	ids := make([]int, 0, len(c.waiters))
	for id := range c.waiters {
		ids = append(ids, id)
	}
	sort.Ints(ids)
	for _, id := range ids {
		w := c.waiters[id]
		out = append(out, Waiter{Duration: w.duration, Ticker: w.period > 0, At: w.at})
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].At.Before(out[j].At) })
	return out
}

// ErrAwaitTimeout reports that a condition was not observed in time.
var ErrAwaitTimeout = errors.New("testkit: fake clock condition not observed")

// AwaitWaiter blocks until cond holds for the active waiters, bounded by
// timeout (real time; it bounds a test's wait, never the code under test).
func (c *FakeClock) AwaitWaiter(timeout time.Duration, cond func([]Waiter) bool) error {
	deadline := time.NewTimer(timeout)
	defer deadline.Stop()
	for {
		c.mu.Lock()
		ok := cond(c.waitersLocked())
		ch := c.changed
		ws := c.waitersLocked()
		c.mu.Unlock()
		if ok {
			return nil
		}
		select {
		case <-ch:
		case <-deadline.C:
			return fmt.Errorf("%w within %v; active waiters %v", ErrAwaitTimeout, timeout, ws)
		}
	}
}

// HasTimer is an AwaitWaiter condition: a one-shot timer of duration d.
func HasTimer(d time.Duration) func([]Waiter) bool {
	return func(ws []Waiter) bool {
		for _, w := range ws {
			if !w.Ticker && w.Duration == d {
				return true
			}
		}
		return false
	}
}

// HasTicker is an AwaitWaiter condition: a ticker of period d.
func HasTicker(d time.Duration) func([]Waiter) bool {
	return func(ws []Waiter) bool {
		for _, w := range ws {
			if w.Ticker && w.Duration == d {
				return true
			}
		}
		return false
	}
}
