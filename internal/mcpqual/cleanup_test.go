//go:build linux || darwin

package mcpqual

import (
	"errors"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/wedevwork/callsheet/internal/spikes/processgroup"
	"github.com/wedevwork/callsheet/internal/testkit"
)

// FP-15 unit tests: GroupReaper against a simulated process group with an
// injected Signaler and fake clock. No process is started and no real
// grace elapses; every advance follows the timer it must fire.

type simGroup struct {
	mu         sync.Mutex
	pgid       int
	leader     bool // alive
	desc       bool // a descendant member, alive
	leaderTerm bool // the leader ignores TERM
	descTerm   bool // the descendant ignores TERM
	reapLeader bool // Wait returns once the leader dies
	eperm      int  // EPERM answers to probes of a member-less group (-1: forever)
	proc       *fakeProc
	closed     bool
	signals    []syscall.Signal
	probes     chan error
	kills      chan struct{}
}

func newSim(pgid int, desc bool) *simGroup {
	return &simGroup{pgid: pgid, leader: true, desc: desc, reapLeader: true, proc: &fakeProc{pgid: pgid, exited: make(chan struct{})}, probes: make(chan error, 64), kills: make(chan struct{}, 4)}
}

func (g *simGroup) exitLeaderLocked() {
	g.leader = false
	if g.reapLeader && !g.closed {
		g.closed = true
		close(g.proc.exited)
	}
}

func (g *simGroup) Signal(pid int, sig syscall.Signal) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.signals = append(g.signals, sig)
	if pid != -g.pgid {
		return syscall.ESRCH
	}
	alive := g.leader || g.desc
	switch sig {
	case syscall.SIGTERM:
		if !alive {
			return syscall.ESRCH
		}
		if !g.leaderTerm && g.leader {
			g.exitLeaderLocked()
		}
		if !g.descTerm {
			g.desc = false
		}
	case syscall.SIGKILL:
		g.kills <- struct{}{}
		if !alive {
			return syscall.ESRCH
		}
		if g.leader {
			g.exitLeaderLocked()
		}
		g.desc = false
	case 0:
		var err error
		switch {
		case alive:
		case g.eperm != 0:
			if g.eperm > 0 {
				g.eperm--
			}
			err = syscall.EPERM
		default:
			err = syscall.ESRCH
		}
		g.probes <- err
		return err
	}
	return nil
}

func (g *simGroup) sent(sig syscall.Signal) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	for _, s := range g.signals {
		if s == sig {
			return true
		}
	}
	return false
}

type reapHarness struct {
	t     *testing.T
	sim   *simGroup
	clock *testkit.FakeClock
	res   chan CaseCleanup
}

func startReap(t *testing.T, sim *simGroup, sig Signaler, pol CleanupPolicy) *reapHarness {
	h := &reapHarness{t: t, sim: sim, clock: testkit.NewFakeClock(epoch), res: make(chan CaseCleanup, 1)}
	if sig == nil {
		sig = sim
	}
	go func() { h.res <- GroupReaper{Sig: sig, Clock: h.clock, Policy: pol}.Reap(sim.proc) }()
	return h
}

func (h *reapHarness) timer(d time.Duration) {
	h.t.Helper()
	if err := h.clock.AwaitWaiter(testWait, testkit.HasTimer(d)); err != nil {
		h.t.Fatal(err)
	}
}

func (h *reapHarness) probe(want error) {
	h.t.Helper()
	select {
	case got := <-h.sim.probes:
		if !errors.Is(got, want) && !(got == nil && want == nil) {
			h.t.Fatalf("probe = %v, want %v", got, want)
		}
	case <-time.After(testWait):
		h.t.Fatal("no existence probe")
	}
}

func (h *reapHarness) result() CaseCleanup {
	h.t.Helper()
	select {
	case cc := <-h.res:
		return cc
	case <-time.After(testWait):
		h.t.Fatal("Reap did not return")
		return CaseCleanup{}
	}
}

func (h *reapHarness) pending() {
	h.t.Helper()
	select {
	case cc := <-h.res:
		h.t.Fatalf("Reap returned early: %+v", cc)
	default:
	}
}

var testPolicy = CleanupPolicy{Grace: time.Second, Limit: 5 * time.Second, Poll: 2 * time.Second}

func TestReapCooperative(t *testing.T) {
	h := startReap(t, newSim(4242, true), nil, testPolicy)
	cc := h.result()
	if !cc.TermSent || cc.KillSent || !cc.GroupGone || cc.Error != nil || cc.LeaderExitedFirst || h.sim.sent(syscall.SIGKILL) {
		t.Fatalf("cleanup %+v", cc)
	}
}

func TestReapResistant(t *testing.T) {
	sim := newSim(4242, true)
	sim.leaderTerm, sim.descTerm = true, true
	h := startReap(t, sim, nil, testPolicy)
	h.timer(975 * time.Millisecond) // Grace minus the default 25 ms ProbeLead
	h.pending()
	h.clock.Advance(975 * time.Millisecond)
	h.timer(25 * time.Millisecond)
	h.pending()
	h.clock.Advance(25 * time.Millisecond)
	cc := h.result()
	if !cc.TermSent || !cc.KillSent || !cc.GroupGone || cc.Error != nil {
		t.Fatalf("cleanup %+v", cc)
	}
}

// The leader exits on TERM while a descendant keeps the group: the
// leader's Wait alone is not completion; KILL at the grace deadline and a
// later ESRCH are.
func TestReapParentExitsFirst(t *testing.T) {
	sim := newSim(4242, true)
	sim.descTerm = true
	h := startReap(t, sim, nil, testPolicy)
	h.probe(nil) // the group outlives its leader
	h.probe(nil) // WaitGone's first poll: still present
	h.timer(testPolicy.Poll)
	h.timer(975 * time.Millisecond)
	h.pending()
	h.clock.Advance(975 * time.Millisecond)
	h.timer(25 * time.Millisecond)
	h.clock.Advance(25 * time.Millisecond)
	select {
	case <-sim.kills:
	case <-time.After(testWait):
		t.Fatal("no KILL at the grace deadline")
	}
	h.pending() // KILL sent, but absence is not yet proven
	h.clock.Advance(time.Second)
	h.probe(syscall.ESRCH)
	cc := h.result()
	if !cc.LeaderExitedFirst || !cc.KillSent || !cc.GroupGone || cc.Error != nil {
		t.Fatalf("cleanup %+v", cc)
	}
}

// darwin: a member-less (zombie-only) group answers EPERM before ESRCH.
// EPERM is never absence: no success until ESRCH.
func TestReapEPERMThenESRCH(t *testing.T) {
	sim := newSim(4242, false)
	sim.eperm = 3
	pol := CleanupPolicy{Grace: time.Second, Limit: 5 * time.Second, Poll: 100 * time.Millisecond}
	h := startReap(t, sim, nil, pol)
	h.probe(syscall.EPERM) // the leader-first check
	h.probe(syscall.EPERM)
	for i := 0; i < 2; i++ {
		h.timer(pol.Poll)
		h.pending()
		h.clock.Advance(pol.Poll)
		if i == 0 {
			h.probe(syscall.EPERM)
		}
	}
	h.probe(syscall.ESRCH)
	cc := h.result()
	if !cc.GroupGone || cc.Error != nil || cc.KillSent || cc.LeaderExitedFirst {
		t.Fatalf("cleanup %+v", cc)
	}
}

func TestReapPersistentEPERM(t *testing.T) {
	sim := newSim(4242, false)
	sim.eperm = -1
	pol := CleanupPolicy{Grace: time.Second, Limit: 250 * time.Millisecond, Poll: 100 * time.Millisecond}
	h := startReap(t, sim, nil, pol)
	h.probe(syscall.EPERM)
	h.probe(syscall.EPERM)
	for i := 0; i < 3; i++ {
		h.timer(pol.Poll)
		h.pending()
		h.clock.Advance(pol.Poll)
		h.probe(syscall.EPERM)
	}
	cc := h.result()
	if cc.GroupGone || cc.Error == nil || !strings.Contains(*cc.Error, "operation not permitted") {
		t.Fatalf("persistent EPERM was treated as absence: %+v", cc)
	}
}

// An injected probe failure ends the escalation at once with KILL and is a
// cleanup failure; TERM and KILL still reach the real group.
func TestReapInjectedProbeFailure(t *testing.T) {
	sim := newSim(4242, true)
	sim.leaderTerm, sim.descTerm = true, true
	sig, err := SignalerFor(FaultValue, sim)
	if err != nil {
		t.Fatal(err)
	}
	h := startReap(t, sim, sig, testPolicy)
	// The leader never exits on TERM; its Wait is the 6 s bound.
	h.timer(testPolicy.Grace + testPolicy.Limit)
	h.timer(975 * time.Millisecond)
	h.clock.Advance(975 * time.Millisecond)
	h.timer(25 * time.Millisecond)
	h.clock.Advance(25 * time.Millisecond)
	cc := h.result()
	if cc.GroupGone || cc.Error == nil || !strings.Contains(*cc.Error, "injected") || !sim.sent(syscall.SIGKILL) || !sim.sent(syscall.SIGTERM) {
		t.Fatalf("cleanup %+v signals %v", cc, sim.signals)
	}
}

func TestReapInjectedProbeAfterExit(t *testing.T) {
	sim := newSim(4242, false)
	h := startReap(t, sim, FaultSignaler{Next: sim}, testPolicy)
	cc := h.result()
	if cc.GroupGone || cc.Error == nil || !errors.Is(ErrInjectedProbe, ErrInjectedProbe) || !strings.Contains(*cc.Error, "injected") {
		t.Fatalf("cleanup %+v", cc)
	}
}

func TestReapAlreadyGone(t *testing.T) {
	sim := newSim(4242, false)
	sim.leader = false
	sim.closed = true
	close(sim.proc.exited)
	h := startReap(t, sim, nil, testPolicy)
	cc := h.result()
	if cc.TermSent || cc.KillSent || !cc.GroupGone || cc.Error != nil {
		t.Fatalf("cleanup %+v", cc)
	}
}

func TestReapLeaderNeverReaped(t *testing.T) {
	sim := newSim(4242, false)
	sim.leaderTerm, sim.reapLeader = true, false
	h := startReap(t, sim, nil, testPolicy)
	h.timer(testPolicy.Grace + testPolicy.Limit)
	h.timer(975 * time.Millisecond)
	h.clock.Advance(975 * time.Millisecond)
	h.timer(25 * time.Millisecond)
	h.clock.Advance(25 * time.Millisecond)
	h.pending()
	h.clock.Advance(testPolicy.Limit)
	cc := h.result()
	if cc.GroupGone || cc.Error == nil || !strings.Contains(*cc.Error, "was not reaped") {
		t.Fatalf("cleanup %+v", cc)
	}
}

func TestReapInvalidGroup(t *testing.T) {
	sim := newSim(1, false)
	h := startReap(t, sim, nil, testPolicy)
	cc := h.result()
	if cc.Error == nil || !strings.Contains(*cc.Error, processgroup.ErrInvalidGroup.Error()) {
		t.Fatalf("cleanup %+v", cc)
	}
}

func TestCleanupPolicyAndSignalers(t *testing.T) {
	for _, goos := range []string{"linux", "darwin"} {
		p, ok := PolicyFor(goos)
		if !ok || p.Grace != time.Second || p.Limit != 5*time.Second || p.Poll != 10*time.Millisecond || p.EPERMIsAbsence {
			t.Fatalf("%s policy %+v", goos, p)
		}
	}
	if _, ok := PolicyFor("windows"); ok {
		t.Fatal("windows supported")
	}
	sim := newSim(99, false)
	if s, err := SignalerFor("", sim); err != nil || s != Signaler(sim) {
		t.Fatal("unset fault did not select the real signaler")
	}
	if _, err := SignalerFor("kill-everything", sim); err == nil || !strings.Contains(err.Error(), "invalid_argument") {
		t.Fatalf("unsupported fault: %v", err)
	}
	f := FaultSignaler{Next: sim}
	if err := f.Signal(-99, 0); !errors.Is(err, ErrInjectedProbe) {
		t.Fatal(err)
	}
	if err := f.Signal(-99, syscall.SIGTERM); err != nil || !sim.sent(syscall.SIGTERM) {
		t.Fatal("TERM was not forwarded")
	}
	var _ processgroup.Signaler = f
	if err := (SysSignaler{}).Signal(-1<<30, 0); err == nil {
		t.Fatal("kill(2) of an impossible group succeeded")
	}
}
