//go:build linux || darwin

package sidecar

import (
	"errors"
	"slices"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/wedevwork/callsheet/internal/spikes/processgroup"
)

// scriptedGroup is an injected process group behind kill(2): no process
// exists. TERM ends it when cooperative, KILL unless unkillable; denied
// makes every probe and signal EPERM (a zombie-only group on macOS).
type scriptedGroup struct {
	mu                             sync.Mutex
	alive, cooperative, unkillable bool
	denied                         bool
	sent                           []syscall.Signal
}

func (g *scriptedGroup) Signal(pid int, sig syscall.Signal) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	if pid >= 0 {
		return errors.New("scripted group: only groups are signaled")
	}
	g.sent = append(g.sent, sig)
	switch {
	case g.denied:
		return syscall.EPERM
	case !g.alive:
		return syscall.ESRCH
	case sig == syscall.SIGTERM && g.cooperative, sig == syscall.SIGKILL && !g.unkillable:
		g.alive = false
	}
	return nil
}

func (g *scriptedGroup) signals() []syscall.Signal {
	g.mu.Lock()
	defer g.mu.Unlock()
	return slices.Clone(g.sent)
}

// stepClock is a virtual clock whose After is ready at once, advancing
// the clock by its duration: bounded waits complete without real time.
type stepClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *stepClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *stepClock) After(d time.Duration) <-chan time.Time {
	c.mu.Lock()
	c.now = c.now.Add(d)
	t := c.now
	c.mu.Unlock()
	ch := make(chan time.Time, 1)
	ch <- t
	return ch
}

// realGroupsContract drives the production group wrapper (the reused
// processgroup mechanics) through injected signals and time: cleanup of
// an absent, cooperative, TERM-resistant, unkillable and permission-denied
// group, run-shutdown termination with and without the child's exit, and
// the direct child's wait classification. It starts no process.
func realGroupsContract(t *testing.T) {
	clk := func() *stepClock { return &stepClock{now: time.Date(2026, 9, 27, 0, 0, 0, 0, time.UTC)} }
	for name, c := range map[string]struct {
		g      *scriptedGroup
		ok     bool
		term   bool
		killed bool
	}{
		"absent":      {&scriptedGroup{}, true, false, false},
		"cooperative": {&scriptedGroup{alive: true, cooperative: true}, true, true, false},
		"resistant":   {&scriptedGroup{alive: true}, true, true, true},
		"unkillable":  {&scriptedGroup{alive: true, unkillable: true}, false, true, true},
		"denied":      {&scriptedGroup{alive: true, denied: true}, false, true, false},
	} {
		g := c.g
		err := realGroups{sig: g, clk: clk()}.cleanup(4242)
		sent := g.signals()
		if (err == nil) != c.ok || slices.Contains(sent, syscall.SIGTERM) != c.term || (c.killed && !slices.Contains(sent, syscall.SIGKILL)) {
			t.Fatalf("%s cleanup: %v, signals %v", name, err, sent)
		}
		if name == "denied" && !errors.Is(err, syscall.EPERM) {
			t.Fatalf("denied cleanup error %v", err)
		}
	}
	// Run shutdown: TERM, then the child's exit ends it; without an exit
	// the grace passes and the group is killed; pgid <= 1 is refused.
	exited := make(chan struct{})
	close(exited)
	g := &scriptedGroup{alive: true}
	realGroups{sig: g, clk: clk()}.terminate(4242, exited)
	if s := g.signals(); len(s) == 0 || s[0] != syscall.SIGTERM {
		t.Fatalf("terminate after exit sent %v", s)
	}
	g = &scriptedGroup{alive: true}
	realGroups{sig: g, clk: clk()}.terminate(4242, make(chan struct{}))
	if s := g.signals(); !slices.Equal(s, []syscall.Signal{syscall.SIGTERM, syscall.SIGKILL}) {
		t.Fatalf("terminate without exit sent %v", s)
	}
	g = &scriptedGroup{alive: true}
	realGroups{sig: g, clk: clk()}.terminate(1, make(chan struct{}))
	if s := g.signals(); len(s) != 0 {
		t.Fatalf("pgid 1 was signaled: %v", s)
	}
	if sig, clock := (realGroups{}).parts(); sig != (processgroup.SysSignaler{}) || clock != (processgroup.RealClock{}) {
		t.Fatal("the production wrapper does not default to kill(2) and real time")
	}
	// Wait classification: exit code, signal death, a wait error and an
	// unrecognized status (never success).
	for name, c := range map[string]struct {
		err        error
		exitStatus bool
		ws         syscall.WaitStatus
		ok         bool
		want       procExit
	}{
		"exit 3":   {errors.New("exit status 3"), true, syscall.WaitStatus(3 << 8), true, procExit{code: 3}},
		"exit 0":   {nil, false, syscall.WaitStatus(0), true, procExit{code: 0}},
		"sigkill":  {errors.New("signal: killed"), true, syscall.WaitStatus(syscall.SIGKILL), true, procExit{signal: "SIGKILL"}},
		"wait err": {errors.New("wait failed"), false, 0, false, procExit{err: errors.New("wait failed")}},
	} {
		got := waitOutcome(c.err, c.exitStatus, c.ws, c.ok)
		if got.code != c.want.code || got.signal != c.want.signal || (got.err == nil) != (c.want.err == nil) {
			t.Fatalf("%s: %+v", name, got)
		}
	}
	if got := waitOutcome(errors.New("exit status ?"), true, 0, false); got.err == nil || got.err.Error() != "unrecognized wait status" {
		t.Fatalf("unrecognized status: %+v", got)
	}
}
