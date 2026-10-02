//go:build linux || darwin

package sidecar

import (
	"errors"
	"os"
	"os/exec"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/unix"

	"github.com/wedevwork/callsheet/internal/contract"
	"github.com/wedevwork/callsheet/internal/spikes/processgroup"
	"github.com/wedevwork/callsheet/internal/testkit"
)

// Iteration 10a, UT-A2 (FP-2): the guardian's early group completion in
// process, on the timeout rig's fake clock (control_timeout_unix_test.go)
// with the two primitives injected: subreap records its call, groupAlone
// answers from a script. No process is started and no real signal is sent.

// probeAnswer is one scripted group observation.
type probeAnswer struct {
	st  groupState
	err error
}

// fastRig is a timeout rig with the early-completion primitives injected.
type fastRig struct {
	*timeoutRig
	mu        sync.Mutex
	answer    func(n int) probeAnswer
	probed    int
	subreaped int
}

func newFastRig(t *testing.T, timeout string, answer func(n int) probeAnswer) *fastRig {
	t.Helper()
	r := &fastRig{timeoutRig: newTimeoutRig(t, timeout), answer: answer}
	r.env.subreap = func() error {
		r.mu.Lock()
		r.subreaped++
		r.mu.Unlock()
		return nil
	}
	r.env.groupAlone = func(pgid int) (groupState, error) {
		r.mu.Lock()
		defer r.mu.Unlock()
		if pgid != rigPID {
			return groupUnknown, errors.New("another group was observed")
		}
		r.probed++
		a := r.answer(r.probed)
		return a.st, a.err
	}
	return r
}

// hasTimer reports a one-shot timer of d among ws.
func hasTimer(ws []testkit.Waiter, d time.Duration) bool {
	return slices.ContainsFunc(ws, func(w testkit.Waiter) bool { return !w.Ticker && w.Duration == d })
}

// always answers every probe with a.
func always(a probeAnswer) func(int) probeAnswer { return func(int) probeAnswer { return a } }

func (r *fastRig) probes() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.probed
}

// natural launches the guardian, reads its started status and ends its
// adapter with exit code 0, reading the forwarded exit status.
func (r *fastRig) natural(t *testing.T) {
	t.Helper()
	r.launch(t)
	a := <-r.started
	if s := r.status(t); s.Type != contract.GuardianStarted {
		t.Fatalf("started %+v", s)
	}
	a.exit <- procExit{code: 0}
	if ex := r.status(t); ex.Type != contract.GuardianExit || ex.ExitCode == nil || *ex.ExitCode != 0 {
		t.Fatalf("exit %+v", ex)
	}
}

// wantSignals requires exactly the group signals want.
func (r *fastRig) wantSignals(t *testing.T, want ...syscall.Signal) {
	t.Helper()
	if got := r.group.signals(); !slices.Equal(got, want) {
		t.Fatalf("group signals %v, want %v", got, want)
	}
}

// TestGuardianFastCompletion is UT-A2 of iteration 10a (FP-2) on the
// guardian.
func TestGuardianFastCompletion(t *testing.T) {
	t.Parallel()
	t.Run("alone", func(t *testing.T) {
		t.Parallel()
		// Proved alone at the first probe after the exit: no KILL, the
		// status queue flushed, an ordinary return; the subreaper was
		// installed once, before the adapter's start.
		r := newFastRig(t, "0s", always(probeAnswer{st: groupAlone}))
		r.natural(t)
		if code := r.exit(t); code != 0 {
			t.Fatalf("guardian exit %d", code)
		}
		r.wantSignals(t, syscall.SIGTERM)
		if r.probes() != 1 || r.subreaped != 1 || !r.ev.seen("group-alone") {
			t.Fatalf("probes %d, subreaped %d, events %v", r.probes(), r.subreaped, r.ev.all)
		}
	})
	t.Run("busy-then-alone", func(t *testing.T) {
		t.Parallel()
		// Busy observations are retried every groupPoll on the injected
		// clock (armed before it moves); the proof still ends the cleanup
		// long before the grace.
		r := newFastRig(t, "0s", func(n int) probeAnswer {
			if n < 3 {
				return probeAnswer{st: groupBusy}
			}
			return probeAnswer{st: groupAlone}
		})
		r.natural(t)
		start := r.fc.Now()
		for i := 0; i < 2; i++ {
			r.awaitTimer(t, groupPoll)
			r.fc.Advance(groupPoll)
		}
		if code := r.exit(t); code != 0 {
			t.Fatalf("guardian exit %d", code)
		}
		r.wantSignals(t, syscall.SIGTERM)
		if r.probes() != 3 || r.fc.Now().Sub(start) != 2*groupPoll {
			t.Fatalf("probes %d after %v", r.probes(), r.fc.Now().Sub(start))
		}
	})
	t.Run("no-proof", func(t *testing.T) {
		t.Parallel()
		// Busy, unknown and an error (whatever state it returns) never
		// end the grace: the probes continue until the deadline's KILL.
		r := newFastRig(t, "0s", func(n int) probeAnswer {
			switch n {
			case 1:
				return probeAnswer{st: groupAlone, err: errors.New("injected probe failure")}
			case 2:
				return probeAnswer{st: groupBusy}
			}
			return probeAnswer{st: groupUnknown}
		})
		r.natural(t)
		start := r.fc.Now()
		r.ev.await(t, "probe unknown") // the failed probe, whatever its state
		r.awaitTimer(t, groupPoll)
		r.fc.Advance(groupPoll)
		r.ev.await(t, "probe busy")
		if code := r.finishGrace(t); code != 0 {
			t.Fatalf("guardian exit %d", code)
		}
		r.wantSignals(t, syscall.SIGTERM, syscall.SIGKILL)
		if r.probes() < 2 || r.ev.seen("group-alone") || r.fc.Now().Sub(start) < groupGrace {
			t.Fatalf("probes %d, events %v, after %v", r.probes(), r.ev.all, r.fc.Now().Sub(start))
		}
	})
	t.Run("disabled", func(t *testing.T) {
		t.Parallel()
		// A nil primitive, or a subreaper that cannot be installed,
		// disables the fast path: no probe, the full grace and the KILL.
		for name, mut := range map[string]func(r *fastRig){
			"no-observer": func(r *fastRig) { r.env.groupAlone = nil },
			"no-subreap":  func(r *fastRig) { r.env.subreap = nil },
			"subreap-failed": func(r *fastRig) {
				r.env.subreap = func() error { return errors.New("injected prctl failure") }
			},
		} {
			t.Run(name, func(t *testing.T) {
				t.Parallel()
				r := newFastRig(t, "0s", always(probeAnswer{st: groupAlone}))
				mut(r)
				r.natural(t)
				if code := r.finishGrace(t); code != 0 {
					t.Fatalf("guardian exit %d", code)
				}
				r.wantSignals(t, syscall.SIGTERM, syscall.SIGKILL)
				if r.probes() != 0 {
					t.Fatalf("%d probes with the fast path disabled", r.probes())
				}
				if name == "subreap-failed" && (!r.ev.seen("subreap-failed") || !strings.Contains(r.diag.String(), "early group completion disabled")) {
					t.Fatalf("subreap failure not reported: %v %q", r.ev.all, r.diag.String())
				}
			})
		}
	})
	t.Run("lifetime-blocking", func(t *testing.T) {
		t.Parallel()
		// A lifetime pipe that stayed blocking at adoption disables the
		// fast path (its reader could not be joined): no probe, the full
		// grace and the KILL, and a bounded diagnostic.
		r := newFastRig(t, "0s", always(probeAnswer{st: groupAlone}))
		r.p.fds.lifeBlocking = true
		r.natural(t)
		if code := r.finishGrace(t); code != 0 {
			t.Fatalf("guardian exit %d", code)
		}
		r.wantSignals(t, syscall.SIGTERM, syscall.SIGKILL)
		if r.probes() != 0 || r.subreaped != 0 || !r.ev.seen("lifetime-blocking") || !strings.Contains(r.diag.String(), "lifetime pipe stayed blocking") {
			t.Fatalf("probes %d, subreaped %d, events %v, diag %q", r.probes(), r.subreaped, r.ev.all, r.diag.String())
		}
	})
	t.Run("not-joined", func(t *testing.T) {
		t.Parallel()
		// The deadline starts the cleanup while Start is still in
		// progress; no probe is eligible before Start returned and the
		// adapter's Wait joined, though every probe would answer alone.
		r := newFastRig(t, "5s", always(probeAnswer{st: groupAlone}))
		gate, entered := make(chan struct{}), make(chan struct{})
		adapters := make(chan *fakeRunAdapter, 1)
		r.env.startAdapter = func(contract.GuardianInvocation, *os.File, *os.File, *os.File) (adapterRun, error) {
			close(entered)
			<-gate
			// Not the termGroup's: a TERM does not end it.
			a := &fakeRunAdapter{pid: rigPID + 1, exit: make(chan procExit, 1)}
			adapters <- a
			return a, nil
		}
		r.launch(t)
		<-entered
		r.ev.await(t, "timeout-armed")
		r.fc.Advance(5 * time.Second)
		r.awaitSignal(t, syscall.SIGTERM)
		r.awaitTimer(t, groupGrace-processgroup.ProbeLead)
		if r.probes() != 0 {
			t.Fatal("probed while the adapter's Start was in progress")
		}
		close(gate)
		a := <-adapters
		for _, want := range []string{contract.GuardianStarted, contract.GuardianStopping} {
			if s := r.status(t); s.Type != want {
				t.Fatalf("want %s, got %+v", want, s)
			}
		}
		r.awaitSignal(t, syscall.SIGTERM) // the late launch's own TERM
		if r.probes() != 0 {
			t.Fatal("probed before the adapter's Wait joined")
		}
		a.exit <- procExit{signal: "SIGTERM"}
		if ex := r.status(t); ex.Type != contract.GuardianExit || ex.Signal == nil || *ex.Signal != "SIGTERM" {
			t.Fatalf("exit %+v", ex)
		}
		if code := r.exit(t); code != 0 {
			t.Fatalf("guardian exit %d", code)
		}
		r.wantSignals(t, syscall.SIGTERM, syscall.SIGTERM)
		if r.probes() != 1 {
			t.Fatalf("%d probes", r.probes())
		}
	})
	t.Run("deadline-first", func(t *testing.T) {
		t.Parallel()
		// A proof arriving once the deadline's final flush claimed the
		// cleanup is not accepted (one order under one mutex): the KILL
		// follows; no duplicate cleanup.
		var r *fastRig
		r = newFastRig(t, "0s", func(int) probeAnswer {
			if r.ev.seen("deadline-claimed") {
				return probeAnswer{st: groupAlone}
			}
			return probeAnswer{st: groupBusy}
		})
		r.natural(t)
		r.ev.await(t, "probe busy")
		r.awaitTimer(t, groupPoll)
		// To the deadline's final flush (the claim), the observer's next
		// tick due on the way.
		r.fc.Advance(groupGrace - processgroup.ProbeLead)
		r.ev.await(t, "deadline-claimed")
		// Either the tick's probe followed the claim (alone, refused), or
		// it came first (busy) and armed the next tick, whose probe does.
		deadline := time.Now().Add(testWait)
		for !r.ev.seen("probe alone") {
			if hasTimer(r.fc.Waiters(), groupPoll) {
				r.fc.Advance(groupPoll)
				r.ev.await(t, "probe alone")
				break
			}
			if time.Now().After(deadline) {
				t.Fatalf("no probe after the claim: %v", r.ev.all)
			}
			time.Sleep(time.Millisecond)
		}
		if code := r.finishGrace(t); code != 0 {
			t.Fatalf("guardian exit %d", code)
		}
		r.wantSignals(t, syscall.SIGTERM, syscall.SIGKILL)
		if r.ev.seen("group-alone") {
			t.Fatalf("a proof after the claim was accepted: %v", r.ev.all)
		}
	})
	t.Run("proof-first", func(t *testing.T) {
		t.Parallel()
		// A proof accepted first wins, even when the observer is preempted
		// between its acceptance and the escalation's wake-up: the
		// deadline's flush and KILL that follow honor it (no KILL), and the
		// guardian returns normally once the observer resumes.
		r := newFastRig(t, "0s", always(probeAnswer{st: groupAlone}))
		accepted, resume := make(chan struct{}), make(chan struct{})
		events := r.env.events
		r.env.events = func(e string) {
			events(e)
			if e == "proof-accepted" {
				close(accepted)
				<-resume // preempted after the acceptance
			}
		}
		r.natural(t)
		<-accepted
		// The escalation reaches its deadline while the observer waits.
		r.awaitTimer(t, groupGrace-processgroup.ProbeLead)
		r.fc.Advance(groupGrace - processgroup.ProbeLead)
		r.awaitTimer(t, processgroup.ProbeLead)
		r.fc.Advance(processgroup.ProbeLead)
		r.ev.await(t, "escalation-ended")
		close(resume)
		if code := r.exit(t); code != 0 {
			t.Fatalf("guardian exit %d", code)
		}
		r.wantSignals(t, syscall.SIGTERM)
		if !r.ev.seen("group-alone") {
			t.Fatalf("events %v", r.ev.all)
		}
	})
	t.Run("status-stall", func(t *testing.T) {
		t.Parallel()
		// A blocked status pipe: the exit's delivery and the final flush
		// are bounded (100 ms injected), and the guardian still returns
		// without the KILL; a cancel command and the parent's EOF arriving
		// in that window change nothing (natural stays latched).
		r := newFastRig(t, "0s", always(probeAnswer{st: groupAlone}))
		r.launch(t)
		a := <-r.started
		r.status(t)
		fillPipe(t, r.p.fds.status)
		a.exit <- procExit{code: 0}
		r.ev.await(t, "probe alone")
		b, err := contract.EncodeGuardianCommand(contract.GuardianCommand{TaskID: r.inv.TaskID, Execution: r.inv.Execution, Nonce: ctlNonce,
			Command: contract.GuardianStop, Cause: contract.CauseCancelled, StopID: strings.Repeat("a", 32)})
		if err != nil {
			t.Fatal(err)
		}
		r.command(t, b)
		r.p.lifeW.Close()
		// The armed delivery and flush bounds expire (advanced only to
		// armed timers) until the guardian returns.
		if code := r.finishGrace(t); code != 0 {
			t.Fatalf("guardian exit %d", code)
		}
		r.wantSignals(t, syscall.SIGTERM)
		if r.ev.seen("cause "+contract.CauseCancelled) || r.ev.seen("cause "+contract.CauseLost) || r.ev.seen("stopping "+contract.CauseCancelled) {
			t.Fatalf("a late control changed the latched cause: %v", r.ev.all)
		}
	})
	t.Run("parent-eof", func(t *testing.T) {
		t.Parallel()
		// The parent's EOF while the adapter runs latches lost and starts
		// the one cleanup; the adapter's exit (its TERM) joins it, the
		// proof ends it: one TERM, no KILL, ordered statuses.
		r := newFastRig(t, "0s", always(probeAnswer{st: groupAlone}))
		r.launch(t)
		<-r.started
		r.status(t)
		r.p.lifeW.Close()
		if s := r.status(t); s.Type != contract.GuardianStopping || *s.Cause != contract.CauseLost {
			t.Fatalf("stopping %+v", s)
		}
		if ex := r.status(t); ex.Type != contract.GuardianExit || ex.Signal == nil || *ex.Signal != "SIGTERM" {
			t.Fatalf("exit %+v", ex)
		}
		if code := r.exit(t); code != 0 {
			t.Fatalf("guardian exit %d", code)
		}
		r.wantSignals(t, syscall.SIGTERM)
	})
}

// TestGuardianFastReturnJoins (not parallel: no other test's guardian
// runs meanwhile) requires an ordinary return to leave no guardian
// goroutine behind: the observer, the escalation, the status writer and
// the command and lifetime readers are all joined (or ended) by then.
func TestGuardianFastReturnJoins(t *testing.T) {
	r := newFastRig(t, "0s", always(probeAnswer{st: groupAlone}))
	r.natural(t)
	if code := r.exit(t); code != 0 {
		t.Fatalf("guardian exit %d", code)
	}
	frames := []string{"sidecar.(*guardian)", "sidecar.(*statusQueue)", "sidecar.runGuardian"}
	deadline := time.Now().Add(testWait)
	for {
		buf := make([]byte, 1<<20)
		buf = buf[:runtime.Stack(buf, true)]
		var left []string
		for _, g := range strings.Split(string(buf), "\n\n") {
			for _, f := range frames {
				if strings.Contains(g, f) && !strings.Contains(g, "TestGuardianFastReturnJoins") {
					left = append(left, g)
					break
				}
			}
		}
		if len(left) == 0 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("guardian goroutines survived its return:\n%s", strings.Join(left, "\n\n"))
		}
		time.Sleep(time.Millisecond)
	}
}

// BenchmarkGuardianCompletion measures the guardian's cleanup decision
// with an injected group observation on the fake clock: alone (the proof
// at the first probe), busy and error (every probe until the deadline's
// KILL): wall time and allocations per cleanup, probes per cleanup. Each
// cleanup must join its observer (no observer timer left armed) and keep
// a bounded number of armed timers. One calibration of the native probe
// runs outside the loop, in a helper process of its own group.
func BenchmarkGuardianCompletion(b *testing.B) {
	for _, c := range []struct {
		name string
		a    probeAnswer
	}{
		{"alone", probeAnswer{st: groupAlone}},
		{"busy", probeAnswer{st: groupBusy}},
		{"error", probeAnswer{st: groupAlone, err: errors.New("injected probe failure")}},
	} {
		b.Run(c.name, func(b *testing.B) {
			probes, maxTimers := 0, 0
			b.ReportAllocs()
			for b.Loop() {
				fc := testkit.NewFakeClock(time.Date(2026, 10, 2, 0, 0, 0, 0, time.UTC))
				st, err := os.CreateTemp(b.TempDir(), "status")
				if err != nil {
					b.Fatal(err)
				}
				n := 0
				g := &guardian{pid: rigPID, stop: make(chan struct{}), parentGone: make(chan struct{}), exitCh: make(chan struct{}), done: make(chan struct{}),
					cleaned: make(chan struct{}), fast: true, fds: guardianFDs{status: st}, env: guardianEnv{
						sig: &scriptedGroup{alive: true}, clock: gClock{fc}, grace: groupGrace, timerAt: fc.NewTimerAt,
						groupAlone: func(int) (groupState, error) { n++; return c.a.st, c.a.err },
					}}
				g.sq = newStatusQueue(g)
				close(g.exitCh) // the adapter's exit fact: the observer may probe at once
				g.cleanup()
				for cleaned := false; !cleaned; {
					select {
					case <-g.cleaned:
						cleaned = true
						continue
					default:
					}
					// The clock moves only once the observer armed its next
					// poll (the proof ends the cleanup without one).
					ws := fc.Waiters()
					maxTimers = max(maxTimers, len(ws))
					if !hasTimer(ws, groupPoll) {
						runtime.Gosched()
						continue
					}
					next := ws[0].At
					for _, w := range ws {
						if w.At.Before(next) {
							next = w.At
						}
					}
					fc.Advance(max(0, next.Sub(fc.Now())))
				}
				if hasTimer(fc.Waiters(), groupPoll) {
					b.Fatal("an observer timer outlived its cleanup")
				}
				g.sq.close()
				close(g.done)
				st.Close()
				probes += n
			}
			if maxTimers > 4 {
				b.Fatalf("%d timers armed at once", maxTimers)
			}
			b.ReportMetric(float64(probes)/float64(b.N), "probes/op")
		})
	}
	b.Run("native-calibration", func(b *testing.B) {
		// Once, outside any repeated subprocess start.
		cmd := exec.Command(os.Args[0])
		cmd.Env = append(os.Environ(), probeCalibrationEnv+"=1")
		cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
		out, err := cmd.Output()
		f := strings.Fields(string(out))
		if err != nil || len(f) != 2 || f[0] != "alone" {
			b.Fatalf("calibration %q: %v", out, err)
		}
		ns, err := strconv.Atoi(f[1])
		if err != nil {
			b.Fatal(err)
		}
		for b.Loop() {
		}
		b.ReportMetric(float64(ns), "native-probe-ns")
	})
}

// TestGuardianLifetimeAdoption (UT-A2): the guardian adopts its lifetime
// pipe nonblocking, so the ordinary return's Close unblocks a reader that
// is still waiting for the parent's EOF; every other descriptor keeps its
// inherited (blocking) mode; a failure to make it nonblocking is reported.
// Run on duplicated pipe descriptors of this process, never on its real
// fd 4.
func TestGuardianLifetimeAdoption(t *testing.T) {
	t.Parallel()
	dup := func() (uintptr, *os.File) {
		r, w, err := os.Pipe()
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { r.Close(); w.Close() })
		raw, err := r.SyscallConn()
		if err != nil {
			t.Fatal(err)
		}
		var fd int
		var derr error
		raw.Control(func(f uintptr) {
			fd, derr = unix.Dup(int(f))
			if derr == nil {
				derr = unix.SetNonblock(fd, false) // inherited fds arrive blocking
			}
		})
		if derr != nil {
			t.Fatal(derr)
		}
		return uintptr(fd), w
	}
	nonblock := func(f *os.File) bool {
		var flags int
		raw, err := f.SyscallConn()
		if err != nil {
			t.Fatal(err)
		}
		raw.Control(func(fd uintptr) { flags, err = unix.FcntlInt(fd, unix.F_GETFL, 0) })
		if err != nil {
			t.Fatal(err)
		}
		return flags&unix.O_NONBLOCK != 0
	}
	// The lifetime pipe: nonblocking and pollable, so Close unblocks its
	// reader.
	fd, _ := dup()
	adopt, blocking := lifetimeAdopter(fd, unix.SetNonblock)
	f := adopt(fd, "lifetime")
	if blocking() {
		t.Fatal("a successful adoption reported a blocking lifetime pipe")
	}
	done := make(chan error, 1)
	go func() {
		_, err := f.Read(make([]byte, 1))
		done <- err
	}()
	time.Sleep(10 * time.Millisecond) // the reader is parked in the poller
	f.Close()
	select {
	case err := <-done:
		if !errors.Is(err, os.ErrClosed) {
			t.Fatalf("read ended with %v, want os.ErrClosed", err)
		}
	case <-time.After(testWait):
		t.Fatal("Close did not unblock the lifetime reader")
	}
	// Another descriptor is adopted as inherited (never made nonblocking).
	other, w := dup()
	calls := 0
	adopt, blocking = lifetimeAdopter(fd+1<<20, func(int, bool) error { calls++; return nil })
	plain := adopt(other, "status")
	if calls != 0 || blocking() || nonblock(plain) {
		t.Fatalf("a non-lifetime descriptor: %d nonblocking calls, blocking %v", calls, blocking())
	}
	w.Write([]byte{1})
	if n, err := plain.Read(make([]byte, 1)); n != 1 || err != nil {
		t.Fatalf("read %d %v", n, err)
	}
	plain.Close()
	// The failure boundary: the lifetime pipe stays blocking, reported.
	stuck, _ := dup()
	adopt, blocking = lifetimeAdopter(stuck, func(int, bool) error { return unix.EINVAL })
	sf := adopt(stuck, "lifetime")
	if !blocking() || nonblock(sf) {
		t.Fatalf("a failed nonblocking adoption: blocking %v", blocking())
	}
	sf.Close()
}
