//go:build linux || darwin

package sidecar

import (
	"errors"
	"os"
	"slices"
	"sync"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/unix"

	"github.com/wedevwork/callsheet/internal/contract"
	"github.com/wedevwork/callsheet/internal/spikes/processgroup"
	"github.com/wedevwork/callsheet/internal/testkit"
)

// The guardian's execution deadline in process (TestControlTimeout): the
// rig of control_unix_test.go with the guardian's one injected clock (a
// fake clock: its Now, its After for the escalation's grace and its
// stoppable timers for the deadline and the status bound). Time moves only
// after the timer it tests is armed (the clock's own waiter list or the
// guardian's timeout-armed event); nothing sleeps.

// gClock adapts the fake clock to the guardian's processgroup.Clock.
type gClock struct{ *testkit.FakeClock }

func (c gClock) After(d time.Duration) <-chan time.Time {
	ch, _ := c.NewTimer(d)
	return ch
}

var _ processgroup.Clock = gClock{}

// gEvents records the guardian's arbitration events.
type gEvents struct {
	mu  sync.Mutex
	all []string
	sig chan struct{}
}

func (e *gEvents) add(s string) {
	e.mu.Lock()
	e.all = append(e.all, s)
	e.mu.Unlock()
	select {
	case e.sig <- struct{}{}:
	default:
	}
}

func (e *gEvents) seen(s string) bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	return slices.Contains(e.all, s)
}

func (e *gEvents) await(t *testing.T, s string) {
	t.Helper()
	deadline := time.After(testWait)
	for !e.seen(s) {
		select {
		case <-e.sig:
		case <-deadline:
			if e.seen(s) {
				return
			}
			t.Fatalf("no guardian event %q in %v", s, e.all)
		}
	}
}

// timeoutRig is a guardian rig whose invocation carries timeout, on a
// fake clock.
type timeoutRig struct {
	*guardianRig
	fc *testkit.FakeClock
	ev *gEvents
}

func newTimeoutRig(t *testing.T, timeout string) *timeoutRig {
	t.Helper()
	r := newGuardianRig(t, contract.JournalPrepared)
	fc := testkit.NewFakeClock(time.Date(2026, 9, 27, 0, 0, 0, 0, time.UTC))
	ev := &gEvents{sig: make(chan struct{}, 1)}
	r.inv.Timeout = timeout
	r.env.clock = gClock{fc}
	r.env.timerAt = fc.NewTimerAt
	r.env.events = ev.add
	return &timeoutRig{guardianRig: r, fc: fc, ev: ev}
}

// status reads the next status and waits until its delivery returned
// (its statusDeadline bound stopped): the clock never moves while a
// delivery that already reached the pipe could still time out.
func (r *timeoutRig) status(t *testing.T) contract.GuardianStatus {
	t.Helper()
	s := r.p.status(t)
	r.ev.await(t, "status-delivered "+s.Type)
	return s
}

// launch runs the guardian through ready and release.
func (r *timeoutRig) launch(t *testing.T) {
	t.Helper()
	r.run(t, nil)
	// ready is written synchronously (no queue, no bound timer).
	if s := r.p.status(t); s.Type != contract.GuardianReady {
		t.Fatalf("ready %+v", s)
	}
	r.p.relW.Write([]byte{0x01})
	r.p.relW.Close()
}

// awaitTimer waits until a one-shot timer of d is armed.
func (r *timeoutRig) awaitTimer(t *testing.T, d time.Duration) {
	t.Helper()
	if err := r.fc.AwaitWaiter(testWait, testkit.HasTimer(d)); err != nil {
		t.Fatal(err)
	}
}

// finishGrace drives the escalation's grace to its KILL, advancing the
// clock only to a timer already armed (the grace's probe lead, its
// remainder, a bounded status flush), until the guardian returns.
func (r *timeoutRig) finishGrace(t *testing.T) int {
	t.Helper()
	deadline := time.After(testWait)
	for {
		select {
		case c := <-r.code:
			return c
		case <-deadline:
			t.Fatalf("the guardian did not return (waiters %v)", r.fc.Waiters())
		default:
		}
		if err := r.fc.AwaitWaiter(10*time.Millisecond, func(ws []testkit.Waiter) bool { return len(ws) > 0 }); err != nil {
			continue // recheck the guardian's return
		}
		ws := r.fc.Waiters()
		if len(ws) > 0 {
			r.fc.Advance(max(0, ws[0].At.Sub(r.fc.Now())))
		}
	}
}

// sendExit delivers the adapter's exit unless a TERM already did.
func sendExit(a *fakeRunAdapter, e procExit) {
	select {
	case a.exit <- e:
	default:
	}
}

// awaitSignal waits until the group received sig.
func (r *timeoutRig) awaitSignal(t *testing.T, sig syscall.Signal) {
	t.Helper()
	deadline := time.After(testWait)
	for !slices.Contains(r.group.signals(), sig) {
		select {
		case <-deadline:
			t.Fatalf("the group never got %v (got %v)", sig, r.group.signals())
		case <-r.ev.sig:
		case <-time.After(time.Millisecond):
		}
	}
}

// guardianTimeouts are TestControlTimeout's guardian cases.
func guardianTimeouts(t *testing.T, name string) {
	switch name {
	case "hours":
		// Two hours on the injected clock: nothing at 2 h minus one tick;
		// at the deadline the cause is timed_out, reported (stopping, no
		// stop ID) before the adapter's exit and the group KILL.
		r := newTimeoutRig(t, "2h0m0s")
		r.launch(t)
		<-r.started
		r.ev.await(t, "timeout-armed")
		if s := r.status(t); s.Type != contract.GuardianStarted || s.StartedAt == nil {
			t.Fatalf("started %+v", s)
		}
		r.fc.Advance(2*time.Hour - time.Nanosecond)
		r.fc.Advance(time.Nanosecond)
		st := r.status(t)
		if st.Type != contract.GuardianStopping || *st.Cause != contract.CauseTimedOut || st.StopID != nil {
			t.Fatalf("stopping %+v", st)
		}
		if ex := r.status(t); ex.Type != contract.GuardianExit || ex.Signal == nil || *ex.Signal != "SIGTERM" {
			t.Fatalf("exit %+v", ex)
		}
		if code := r.finishGrace(t); code != 0 {
			t.Fatalf("guardian exit %d", code)
		}
		if sig := r.group.signals(); len(sig) < 2 || sig[0] != syscall.SIGTERM || sig[len(sig)-1] != syscall.SIGKILL {
			t.Fatalf("signals %v", sig)
		}
	case "zero":
		// Zero is unlimited: no timer is ever armed. The arming (when
		// enforced) precedes Start and so the started status, whose delivery
		// (and so its bound timer) has returned here.
		r := newTimeoutRig(t, "0s")
		r.launch(t)
		a := <-r.started
		r.status(t)
		if r.ev.seen("timeout-armed") {
			t.Fatalf("a zero timeout armed its deadline: %v", r.ev.all)
		}
		if ws := r.fc.Waiters(); len(ws) != 0 {
			t.Fatalf("a zero timeout armed a timer: %v", ws)
		}
		a.exit <- procExit{code: 0}
		if ex := r.status(t); ex.Type != contract.GuardianExit || *ex.ExitCode != 0 {
			t.Fatalf("exit %+v", ex)
		}
		if code := r.finishGrace(t); code != 0 || r.ev.seen("stopping "+contract.CauseTimedOut) {
			t.Fatalf("guardian exit %d %v", code, r.ev.all)
		}
	case "tie":
		// An exit observed strictly before the deadline is natural (no
		// stopping, even when the timer then fires); one observed at the
		// deadline is timed_out.
		r := newTimeoutRig(t, "10s")
		r.launch(t)
		a := <-r.started
		r.ev.await(t, "timeout-armed")
		r.status(t)
		r.fc.Advance(10*time.Second - time.Nanosecond)
		a.exit <- procExit{code: 4}
		r.ev.await(t, "adapter-exit-observed")
		r.fc.Advance(time.Nanosecond)
		if ex := r.status(t); ex.Type != contract.GuardianExit || *ex.ExitCode != 4 {
			t.Fatalf("natural before the deadline %+v", ex)
		}
		if code := r.finishGrace(t); code != 0 || r.ev.seen("stopping "+contract.CauseTimedOut) {
			t.Fatalf("a natural exit became timed_out: %d %v", code, r.ev.all)
		}
		eq := newTimeoutRig(t, "10s")
		eq.launch(t)
		b := <-eq.started
		eq.ev.await(t, "timeout-armed")
		eq.status(t)
		eq.fc.Advance(10 * time.Second)
		sendExit(b, procExit{code: 0})
		if st := eq.status(t); st.Type != contract.GuardianStopping || *st.Cause != contract.CauseTimedOut {
			t.Fatalf("equality %+v", st)
		}
		if ex := eq.status(t); ex.Type != contract.GuardianExit {
			t.Fatalf("exit %+v", ex)
		}
		eq.finishGrace(t)
	case "slow-start":
		// A Start still outstanding at the deadline: the cleanup begins at
		// once (TERM before Start returns); the late launch gets no fresh
		// duration: started, stopping, then its exit, in order. A Start that
		// then fails is a start error with no timed_out result.
		for _, fail := range []bool{false, true} {
			r := newTimeoutRig(t, "5s")
			gate := make(chan struct{})
			entered := make(chan struct{})
			r.env.startAdapter = func(inv contract.GuardianInvocation, stdin, stdout, stderr *os.File) (adapterRun, error) {
				close(entered)
				<-gate
				if fail {
					return nil, errors.New("injected slow start failure")
				}
				a := &fakeRunAdapter{pid: rigPID + 1, exit: make(chan procExit, 1)}
				r.group.mu.Lock()
				r.group.adapter = a
				r.group.mu.Unlock()
				return a, nil
			}
			r.launch(t)
			<-entered
			r.ev.await(t, "timeout-armed")
			r.fc.Advance(5 * time.Second)
			r.awaitSignal(t, syscall.SIGTERM)
			close(gate)
			if fail {
				if s := r.status(t); s.Type != contract.GuardianError || *s.Reason != guardianStartFailed {
					t.Fatalf("failed slow start %+v", s)
				}
				if code := r.finishGrace(t); code != 1 {
					t.Fatalf("guardian exit %d", code)
				}
				continue
			}
			for _, want := range []string{contract.GuardianStarted, contract.GuardianStopping, contract.GuardianExit} {
				if s := r.status(t); s.Type != want || (want == contract.GuardianStopping && *s.Cause != contract.CauseTimedOut) {
					t.Fatalf("want %s, got %+v", want, s)
				}
			}
			r.finishGrace(t)
		}
	case "status-failure":
		// A closed status pipe, or a full one whose delivery exceeds its
		// 100 ms injected bound, is abandoned; the timeout's TERM and the
		// group KILL happen regardless.
		for _, mode := range []string{"closed", "blocked"} {
			r := newTimeoutRig(t, "3s")
			r.launch(t)
			<-r.started
			r.ev.await(t, "timeout-armed")
			r.status(t)
			switch mode {
			case "closed":
				r.p.statR.Close()
			case "blocked":
				fillPipe(t, r.p.fds.status)
			}
			r.fc.Advance(3 * time.Second)
			r.awaitSignal(t, syscall.SIGTERM)
			if mode == "blocked" {
				r.awaitTimer(t, statusDeadline)
				r.fc.Advance(statusDeadline)
			}
			if code := r.finishGrace(t); code != 0 || !r.ev.seen("stopping "+contract.CauseTimedOut) {
				t.Fatalf("%s: exit %d %v", mode, code, r.ev.all)
			}
			if sig := r.group.signals(); sig[0] != syscall.SIGTERM || !slices.Contains(sig, syscall.SIGKILL) {
				t.Fatalf("%s: signals %v", mode, sig)
			}
		}
	default:
		t.Fatalf("unknown guardian timeout case %s", name)
	}
}

// fillPipe fills the pipe f writes to (nonblocking raw writes until
// EAGAIN), so the next ordinary write blocks.
func fillPipe(t *testing.T, f *os.File) {
	t.Helper()
	rc, err := f.SyscallConn()
	if err != nil {
		t.Fatal(err)
	}
	junk := make([]byte, 4096)
	full := false
	rc.Write(func(fd uintptr) bool {
		for {
			if _, err := unix.Write(int(fd), junk); err != nil {
				full = errors.Is(err, unix.EAGAIN)
				return true
			}
		}
	})
	if !full {
		t.Fatal("the status pipe could not be filled")
	}
}
