package testkit

import (
	"errors"
	"testing"
	"time"
)

func TestFakeClock(t *testing.T) {
	start := time.Date(2026, 9, 26, 12, 0, 0, 0, time.UTC)
	c := NewFakeClock(start)
	if !c.Now().Equal(start) {
		t.Fatal("start")
	}
	timer, stop := c.NewTimer(5 * time.Second)
	tick, stopTick := c.NewTicker(2 * time.Second)
	if ws := c.Waiters(); len(ws) != 2 || !ws[0].Ticker || ws[0].Duration != 2*time.Second || ws[1].Ticker || !ws[1].At.Equal(start.Add(5*time.Second)) {
		t.Fatalf("waiters %+v", ws)
	}
	if err := c.AwaitWaiter(time.Second, HasTimer(5*time.Second)); err != nil {
		t.Fatal(err)
	}
	if err := c.AwaitWaiter(time.Second, HasTicker(2*time.Second)); err != nil {
		t.Fatal(err)
	}
	c.Advance(4999 * time.Millisecond)
	select {
	case <-timer:
		t.Fatal("timer fired early")
	default:
	}
	// Ticks at 2 s and 4 s: the channel holds one and drops the other.
	if at := <-tick; !at.Equal(start.Add(2 * time.Second)) {
		t.Fatalf("first tick at %v", at)
	}
	select {
	case <-tick:
		t.Fatal("a dropped tick was delivered")
	default:
	}
	c.Advance(time.Millisecond)
	if at := <-timer; !at.Equal(start.Add(5 * time.Second)) {
		t.Fatalf("timer at %v", at)
	}
	if stop() {
		t.Fatal("stopped a fired timer")
	}
	c.Advance(time.Second)
	if at := <-tick; !at.Equal(start.Add(6 * time.Second)) {
		t.Fatalf("tick at %v", at)
	}
	stopTick()
	c.Advance(10 * time.Second)
	select {
	case <-tick:
		t.Fatal("stopped ticker ticked")
	default:
	}
	// A stopped timer never fires; stop reports it was active.
	t2, stop2 := c.NewTimer(time.Second)
	if !stop2() {
		t.Fatal("stop of an active timer")
	}
	c.Advance(time.Hour)
	select {
	case <-t2:
		t.Fatal("stopped timer fired")
	default:
	}
	if len(c.Waiters()) != 0 {
		t.Fatalf("waiters %+v", c.Waiters())
	}
	// Set moves the clock (also backward) without firing anything.
	t3, _ := c.NewTimer(time.Second)
	c.Set(start)
	if !c.Now().Equal(start) {
		t.Fatal("set")
	}
	select {
	case <-t3:
		t.Fatal("set fired a timer")
	default:
	}
	err := c.AwaitWaiter(10*time.Millisecond, HasTicker(time.Minute))
	if !errors.Is(err, ErrAwaitTimeout) {
		t.Fatalf("await = %v", err)
	}
	// A waiter registered while a test awaits it wakes the wait.
	done := make(chan error, 1)
	go func() { done <- c.AwaitWaiter(10*time.Second, HasTimer(3*time.Second)) }()
	c.NewTimer(3 * time.Second)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	for _, f := range []func(){func() { c.NewTicker(0) }, func() { c.Advance(-1) }} {
		func() {
			defer func() {
				if recover() == nil {
					t.Fatal("no panic")
				}
			}()
			f()
		}()
	}
}

func TestBuildAtHelpers(t *testing.T) {
	dir := t.TempDir()
	if _, err := BuildBinaryAt(dir, "./cmd/nonexistent", "x"); err == nil {
		t.Fatal("building a missing package succeeded")
	}
	if _, err := BuildTestBinaryAt(dir, "./cmd/nonexistent", "x"); err == nil {
		t.Fatal("building missing tests succeeded")
	}
}
