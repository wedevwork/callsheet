//go:build linux || darwin

package processgroup

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/wedevwork/callsheet/internal/testkit"
	"github.com/wedevwork/callsheet/internal/testkit/fakeadapter"
)

const (
	envTestAbandon = "CALLSHEET_PG_TEST_ABANDON"
	envTestExit    = "CALLSHEET_PG_TEST_EXIT_EARLY"
	// envTestOrphan: act as a leader that starts a same-group sleeper,
	// records its pid in the named file and exits before readiness.
	envTestOrphan = "CALLSHEET_PG_TEST_ORPHAN_PIDFILE"
	envTestSleep  = "CALLSHEET_PG_TEST_SLEEP"
)

// TestMain dispatches the dedicated helper process: this package's own test
// binary run with EnvHelper=1 performs the experiment (never the main test
// runner). Two test-only modes inject failures.
func TestMain(m *testing.M) {
	if os.Getenv(envTestSleep) == "1" {
		time.Sleep(time.Hour)
		os.Exit(0)
	}
	if pidfile := os.Getenv(envTestOrphan); pidfile != "" {
		os.Exit(orphanLeader(pidfile))
	}
	if os.Getenv(envTestExit) == "1" {
		os.Exit(5)
	}
	if os.Getenv(EnvHelper) == "1" {
		if os.Getenv(envTestAbandon) == "1" {
			os.Exit(abandonGroup())
		}
		os.Exit(RunHelper(os.Getenv))
	}
	os.Exit(m.Run())
}

// orphanLeader starts a sleeper in its own (inherited) process group, records
// the sleeper's pid, and exits without publishing readiness.
func orphanLeader(pidfile string) int {
	cmd := exec.Command(os.Args[0])
	cmd.Env = append(testkit.EnvWithout(os.Environ(), []string{envTestOrphan}), envTestSleep+"=1")
	if err := cmd.Start(); err != nil {
		return 2
	}
	if err := os.WriteFile(pidfile, []byte(strconv.Itoa(cmd.Process.Pid)), 0o600); err != nil {
		return 2
	}
	return 5
}

// abandonGroup simulates a helper that dies after recording a live group.
func abandonGroup() int {
	cmd := exec.Command(os.Getenv(EnvFake), "--duration=1h")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		return 2
	}
	os.WriteFile(os.Getenv(EnvGroups), []byte(strconv.Itoa(cmd.Process.Pid)+"\n"), 0o600)
	return 3
}

// fakeClock auto-advances virtual time on every After call.
type fakeClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeClock) After(d time.Duration) <-chan time.Time {
	c.mu.Lock()
	c.now = c.now.Add(d)
	now := c.now
	c.mu.Unlock()
	ch := make(chan time.Time, 1)
	ch <- now
	return ch
}

type call struct {
	pid int
	sig syscall.Signal
}

// fakeSignaler records calls and returns scripted errors.
type fakeSignaler struct {
	mu    sync.Mutex
	calls []call
	errs  map[syscall.Signal]error
	// alive counts down existence probes before ESRCH, per target.
	alive map[int]int
	probe error
}

func (f *fakeSignaler) Signal(pid int, sig syscall.Signal) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, call{pid, sig})
	if sig == 0 {
		if f.probe != nil {
			return f.probe
		}
		if f.alive[pid] > 0 {
			f.alive[pid]--
			return nil
		}
		return syscall.ESRCH
	}
	return f.errs[sig]
}

func (f *fakeSignaler) sent(sig syscall.Signal) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, c := range f.calls {
		if c.sig == sig {
			n++
		}
	}
	return n
}

func never() <-chan struct{} { return make(chan struct{}) }

func closed() <-chan struct{} {
	c := make(chan struct{})
	close(c)
	return c
}

func TestEscalateResistantKillsAtDeadline(t *testing.T) {
	clock := &fakeClock{now: time.Unix(1000, 0)}
	sig := &fakeSignaler{}
	var probedAt time.Time
	out, err := Escalate(context.Background(), Plan{PGID: 4242, Sig: sig, Clock: clock, Done: never(), BeforeDeadline: func() error {
		probedAt = clock.Now()
		return errors.New("probe note")
	}})
	if err != nil {
		t.Fatal(err)
	}
	if sig.calls[0] != (call{-4242, syscall.SIGTERM}) || sig.calls[1] != (call{-4242, syscall.SIGKILL}) || len(sig.calls) != 2 {
		t.Fatalf("calls = %v", sig.calls)
	}
	if !out.KillSent || out.KillSentAt.Before(out.Deadline) || out.Deadline.Sub(out.TermSentAt) != Grace {
		t.Fatalf("outcome = %+v", out)
	}
	if !out.Probed || out.Deadline.Sub(probedAt) != ProbeLead || out.ProbeErr == nil {
		t.Fatalf("probe at %v, deadline %v", probedAt, out.Deadline)
	}
}

// effectSignaler models the kernel delivering a signal and its effects (the
// descendant dying, its lifetime pipe closing) during the syscall itself: each
// TERM/KILL advances the fake clock, records the time of that effect, and
// advances it again before returning.
type effectSignaler struct {
	fakeSignaler
	clock  *fakeClock
	step   time.Duration
	effect map[syscall.Signal]time.Time
}

func (e *effectSignaler) Signal(pid int, sig syscall.Signal) error {
	if sig == syscall.SIGTERM || sig == syscall.SIGKILL {
		e.clock.mu.Lock()
		e.clock.now = e.clock.now.Add(e.step)
		e.effect[sig] = e.clock.now
		e.clock.now = e.clock.now.Add(e.step) // the syscall returns after its effects
		e.clock.mu.Unlock()
	}
	return e.fakeSignaler.Signal(pid, sig)
}

// Regression (CI run 36171210173): every effect of a signal must be at or
// after the recorded send time, or evaluateFor sees the KILL-caused pipe close
// as preceding the KILL.
func TestEscalateStampsBeforeSignalEffects(t *testing.T) {
	clock := &fakeClock{now: time.Unix(1000, 0)}
	sig := &effectSignaler{clock: clock, step: time.Millisecond, effect: map[syscall.Signal]time.Time{}}
	out, err := Escalate(context.Background(), Plan{PGID: 4242, Sig: sig, Clock: clock, Done: never()})
	if err != nil {
		t.Fatal(err)
	}
	if kill := sig.effect[syscall.SIGKILL]; !out.KillSent || kill.Before(out.KillSentAt) {
		t.Errorf("KILL effect at %v precedes KillSentAt %v", kill, out.KillSentAt)
	}
	if term := sig.effect[syscall.SIGTERM]; term.Before(out.TermSentAt) {
		t.Errorf("TERM effect at %v precedes TermSentAt %v", term, out.TermSentAt)
	}
	if out.KillSentAt.Before(out.Deadline) || out.Deadline.Sub(out.TermSentAt) != Grace {
		t.Errorf("outcome = %+v", out)
	}
}

func TestEscalateCooperativeNoKill(t *testing.T) {
	for i := 0; i < 20; i++ { // select order is random; exercise both arms
		sig := &fakeSignaler{}
		out, err := Escalate(context.Background(), Plan{PGID: 99, Sig: sig, Clock: &fakeClock{}, Done: closed(), BeforeDeadline: func() error {
			t.Fatal("probe must not run once the group is gone")
			return nil
		}})
		if err != nil || out.KillSent || sig.sent(syscall.SIGKILL) != 0 || sig.sent(syscall.SIGTERM) != 1 {
			t.Fatalf("cooperative: %+v %v %v", out, err, sig.calls)
		}
	}
}

// doneAfterProbe closes Done during the probe: no KILL may follow.
func TestEscalateDoneBetweenProbeAndDeadline(t *testing.T) {
	done := make(chan struct{})
	sig := &fakeSignaler{}
	clock := &gatedClock{fakeClock: fakeClock{}, gate: done}
	out, err := Escalate(context.Background(), Plan{PGID: 7, Sig: sig, Clock: clock, Done: done, BeforeDeadline: func() error {
		close(done)
		return nil
	}})
	if err != nil || out.KillSent || sig.sent(syscall.SIGKILL) != 0 || !out.Probed {
		t.Fatalf("%+v %v %v", out, err, sig.calls)
	}
}

// gatedClock never fires After once gate is closed, so Done wins.
type gatedClock struct {
	fakeClock
	gate chan struct{}
}

func (g *gatedClock) After(d time.Duration) <-chan time.Time {
	select {
	case <-g.gate:
		return make(chan time.Time)
	default:
		return g.fakeClock.After(d)
	}
}

func TestEscalateAlreadyExitedAndErrors(t *testing.T) {
	sig := &fakeSignaler{errs: map[syscall.Signal]error{syscall.SIGTERM: syscall.ESRCH}}
	out, err := Escalate(context.Background(), Plan{PGID: 5, Sig: sig, Clock: &fakeClock{}, Done: never()})
	if err != nil || !out.AlreadyExited || sig.sent(syscall.SIGKILL) != 0 {
		t.Fatalf("already exited: %+v %v", out, err)
	}
	sig = &fakeSignaler{errs: map[syscall.Signal]error{syscall.SIGTERM: syscall.EPERM}}
	if _, err := Escalate(context.Background(), Plan{PGID: 5, Sig: sig, Clock: &fakeClock{}, Done: never()}); !errors.Is(err, syscall.EPERM) {
		t.Fatalf("TERM EPERM = %v", err)
	}
	sig = &fakeSignaler{errs: map[syscall.Signal]error{syscall.SIGKILL: syscall.EPERM}}
	out, err = Escalate(context.Background(), Plan{PGID: 5, Sig: sig, Clock: &fakeClock{}, Done: never()})
	if !errors.Is(err, syscall.EPERM) || out.KillSent {
		t.Fatalf("KILL EPERM = %+v %v", out, err)
	}
	sig = &fakeSignaler{errs: map[syscall.Signal]error{syscall.SIGKILL: syscall.ESRCH}}
	out, err = Escalate(context.Background(), Plan{PGID: 5, Sig: sig, Clock: &fakeClock{}, Done: never()})
	if err != nil || out.KillSent {
		t.Fatalf("KILL ESRCH = %+v %v", out, err)
	}
	for _, bad := range []int{0, 1, -5} {
		sig = &fakeSignaler{}
		if _, err := Escalate(context.Background(), Plan{PGID: bad, Sig: sig, Clock: &fakeClock{}}); !errors.Is(err, ErrInvalidGroup) || len(sig.calls) != 0 {
			t.Fatalf("pgid %d: %v %v", bad, err, sig.calls)
		}
	}
}

func TestEscalateContextCancelKills(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	sig := &fakeSignaler{}
	out, err := Escalate(ctx, Plan{PGID: 11, Grace: time.Hour, Sig: sig, Clock: RealClock{}, Done: never()})
	if !errors.Is(err, context.Canceled) || !out.KillSent {
		t.Fatalf("cancel: %+v %v", out, err)
	}
	// Cancellation during the final wait also kills.
	ctx2, cancel2 := context.WithCancel(context.Background())
	sig = &fakeSignaler{}
	out, err = Escalate(ctx2, Plan{PGID: 11, Grace: time.Hour, ProbeLead: time.Hour, Sig: sig, Clock: &stallAfterProbeClock{}, Done: never(), BeforeDeadline: func() error {
		cancel2()
		return nil
	}})
	if !errors.Is(err, context.Canceled) || !out.KillSent || !out.Probed {
		t.Fatalf("cancel after probe: %+v %v", out, err)
	}
}

// stallAfterProbeClock fires the first After and never the later ones.
type stallAfterProbeClock struct {
	fakeClock
	n int
}

func (s *stallAfterProbeClock) After(d time.Duration) <-chan time.Time {
	s.n++
	if s.n == 1 {
		return s.fakeClock.After(d)
	}
	return make(chan time.Time)
}

func TestExistenceAndWaitGone(t *testing.T) {
	sig := &fakeSignaler{alive: map[int]int{10: 2}}
	if alive, err := Existence(sig, 10); !alive || err != nil {
		t.Fatal("alive")
	}
	if err := WaitGone(sig, &fakeClock{}, time.Second, time.Millisecond, 10, -10); err != nil {
		t.Fatalf("WaitGone = %v", err)
	}
	sig = &fakeSignaler{alive: map[int]int{10: 1 << 30}}
	if err := WaitGone(sig, &fakeClock{}, 10*time.Millisecond, time.Millisecond, 10); err == nil || !strings.Contains(err.Error(), "still present") {
		t.Fatalf("timeout = %v", err)
	}
	sig = &fakeSignaler{probe: syscall.EPERM}
	if _, err := Existence(sig, 10); !errors.Is(err, syscall.EPERM) {
		t.Fatal("EPERM must be an error, not absence")
	}
	if err := WaitGone(sig, &fakeClock{}, time.Second, time.Millisecond, 10); !errors.Is(err, syscall.EPERM) {
		t.Fatalf("WaitGone EPERM = %v", err)
	}
	if (SysSignaler{}).Signal(os.Getpid(), 0) != nil {
		t.Fatal("self existence probe")
	}
	if RealClock.Now(RealClock{}).IsZero() {
		t.Fatal("clock")
	}
}

func TestEmergencyCleanupFromRecordedGroups(t *testing.T) {
	dir := t.TempDir()
	groups := filepath.Join(dir, "groups.txt")
	if err := EmergencyCleanup(&fakeSignaler{}, &fakeClock{}, groups); err != nil {
		t.Fatal("missing file must be fine")
	}
	os.WriteFile(groups, []byte("100\n200\nabc\n1\n"), 0o600)
	sig := &fakeSignaler{alive: map[int]int{-200: 1}}
	err := EmergencyCleanup(sig, &fakeClock{}, groups)
	if err == nil || !strings.Contains(err.Error(), "group 200 survived") || !strings.Contains(err.Error(), `bad recorded group "abc"`) || !strings.Contains(err.Error(), `"1"`) {
		t.Fatalf("err = %v", err)
	}
	if sig.sent(syscall.SIGKILL) != 1 {
		t.Fatalf("KILLs = %v", sig.calls)
	}
	os.WriteFile(groups, []byte("300\n"), 0o600)
	err = EmergencyCleanup(&fakeSignaler{probe: syscall.EPERM, errs: map[syscall.Signal]error{syscall.SIGKILL: syscall.EPERM}}, &fakeClock{}, groups)
	if err == nil || !strings.Contains(err.Error(), "probe group 300") || !strings.Contains(err.Error(), "emergency KILL 300") {
		t.Fatalf("EPERM cleanup = %v", err)
	}
	os.WriteFile(groups, []byte("400\n"), 0o600)
	if err := EmergencyCleanup(&fakeSignaler{}, &fakeClock{}, groups); err != nil {
		t.Fatalf("gone group = %v", err)
	}
	if err := EmergencyCleanup(&fakeSignaler{}, &fakeClock{}, dir); err == nil {
		t.Fatal("unreadable groups file")
	}
}

func goodResult(name string) CaseResult {
	t0 := time.Unix(100, 0)
	r := CaseResult{
		Case:              caseNamed(name),
		PGID:              10,
		LeaderPID:         10,
		DescendantPID:     11,
		TermSentAt:        t0,
		Deadline:          t0.Add(Grace),
		LeaderSignals:     []string{"SIGTERM"},
		DescendantSignals: []string{"SIGTERM"},
		Leader:            ProcStatus{Exited: true},
		Descendant:        ProcStatus{Exited: true, ReapedBy: "subreaper"},
	}
	switch name {
	case "resistant":
		r.KillSent, r.KillSentAt = true, t0.Add(Grace)
		r.Leader = ProcStatus{Signaled: true, Signal: "SIGKILL"}
		r.Descendant = ProcStatus{Signaled: true, Signal: "SIGKILL", ReapedBy: "subreaper"}
		r.Observations = []Observation{{Label: "pre-deadline", At: t0.Add(175 * time.Millisecond), LeaderAlive: true, DescendantAlive: true, LifetimeOpen: true}}
	case "leader-exits-first":
		r.KillSent, r.KillSentAt = true, t0.Add(Grace)
		r.LeaderExitedAt = t0.Add(time.Millisecond)
		r.LifetimeClosedAt = t0.Add(Grace + time.Millisecond)
		r.Descendant = ProcStatus{Signaled: true, Signal: "SIGKILL", ReapedBy: "subreaper"}
		r.Observations = []Observation{
			{Label: "after-leader-exit", At: t0.Add(2 * time.Millisecond), LeaderExited: true, DescendantAlive: true, LifetimeOpen: true},
			{Label: "pre-deadline", At: t0.Add(175 * time.Millisecond), LeaderExited: true, DescendantAlive: true, LifetimeOpen: true},
		}
	}
	return r
}

func caseNamed(name string) Case {
	for _, c := range Cases {
		if c.Name == name {
			return c
		}
	}
	panic(name)
}

func TestEvaluateExpectations(t *testing.T) {
	for _, goos := range []string{"linux", "darwin"} {
		for _, c := range Cases {
			r := goodResult(c.Name)
			evaluateFor(&r, goos)
			if len(r.Errors) != 0 {
				t.Fatalf("%s: %s good result failed: %v", goos, c.Name, r.Errors)
			}
		}
	}
	// linuxOnly marks mutations of the adopted descendant's own status, which
	// evaluate checks only where the helper reaps it (linux); on darwin launchd
	// reaps orphans, so those mutations must not be flagged there.
	type mutation struct {
		apply     func(*CaseResult)
		linuxOnly bool
	}
	mutations := map[string][]mutation{
		"cooperative": {
			{apply: func(r *CaseResult) { r.KillSent = true }},
			{apply: func(r *CaseResult) { r.Leader.ExitCode = 1 }},
			{apply: func(r *CaseResult) { r.Descendant.Exited = false }, linuxOnly: true},
			{apply: func(r *CaseResult) { r.LeaderSignals = nil }},
			{apply: func(r *CaseResult) { r.DescendantSignals = nil }},
			{apply: func(r *CaseResult) { r.AlreadyExited = true }},
		},
		"resistant": {
			{apply: func(r *CaseResult) { r.KillSentAt = r.Deadline.Add(-time.Millisecond) }},
			{apply: func(r *CaseResult) { r.Observations = nil }},
			{apply: func(r *CaseResult) { r.Observations[0].DescendantAlive = false }},
			{apply: func(r *CaseResult) { r.Leader.Signal = "SIGTERM" }},
			{apply: func(r *CaseResult) { r.Descendant.Signaled = false }, linuxOnly: true},
		},
		"leader-exits-first": {
			{apply: func(r *CaseResult) { r.Observations = r.Observations[1:] }},
			{apply: func(r *CaseResult) { r.Observations[1].LeaderExited = false }},
			{apply: func(r *CaseResult) { r.Observations[0].At = r.Observations[1].At.Add(time.Millisecond) }},
			{apply: func(r *CaseResult) { r.KillSent = false }},
			{apply: func(r *CaseResult) { r.LifetimeClosedAt = r.KillSentAt.Add(-time.Millisecond) }},
			{apply: func(r *CaseResult) { r.Descendant.ReapedBy = "launchd" }, linuxOnly: true},
			{apply: func(r *CaseResult) { r.Leader.Exited = false }},
		},
	}
	for _, goos := range []string{"linux", "darwin"} {
		for name, ms := range mutations {
			for i, m := range ms {
				r := goodResult(name)
				m.apply(&r)
				evaluateFor(&r, goos)
				want := goos == "linux" || !m.linuxOnly
				if got := len(r.Errors) != 0; got != want {
					if want {
						t.Errorf("%s: %s mutation %d not detected", goos, name, i)
					} else {
						t.Errorf("%s: %s mutation %d is linux-only but was flagged: %v", goos, name, i, r.Errors)
					}
				}
			}
		}
	}
	r := CaseResult{}
	evaluate(&r)
	if len(r.Errors) != 0 {
		t.Fatal("no descendant: nothing to evaluate")
	}
	if _, ok := r.Observation("x"); ok {
		t.Fatal("observation lookup")
	}
	rep := &Report{Cases: []CaseResult{{Pass: true}, {Pass: true}, {Pass: false}}}
	if rep.Pass() {
		t.Fatal("failing case passed report")
	}
	rep.Cases[2].Pass = true
	if !rep.Pass() {
		t.Fatal("passing report")
	}
}

func TestHelpersAndStatuses(t *testing.T) {
	if envLifetimeFD != fakeadapter.EnvLifetimeFD {
		t.Fatal("lifetime env var mismatch with fakeadapter")
	}
	if signalName(syscall.SIGKILL) != "SIGKILL" || signalName(syscall.SIGTERM) != "SIGTERM" || signalName(syscall.SIGHUP) != "1" {
		t.Fatal("signalName")
	}
	var ws syscall.WaitStatus = 9 // killed by SIGKILL
	if s := statusFromWait(3, ws, "x"); !s.Signaled || s.Signal != "SIGKILL" {
		t.Fatalf("signaled = %+v", s)
	}
	ws = 7 << 8 // exited 7
	if s := statusFromWait(3, ws, "x"); !s.Exited || s.ExitCode != 7 {
		t.Fatalf("exited = %+v", s)
	}
	dir := t.TempDir()
	p := filepath.Join(dir, "s.jsonl")
	os.WriteFile(p, []byte(`{"pid":5,"signal":"SIGTERM"}`+"\nbad\n"+`{"pid":6,"signal":"SIGINT"}`+"\n"), 0o600)
	if got := readSignals(p, 5); len(got) != 1 || got[0] != "SIGTERM" || !contains(got, "SIGTERM") || contains(got, "x") {
		t.Fatalf("signals = %v", got)
	}
	if readSignals(filepath.Join(dir, "missing"), 5) != nil {
		t.Fatal("missing file")
	}
	done := make(chan struct{})
	if _, err := waitReady(filepath.Join(dir, "ready.json"), done, 10*time.Millisecond); err == nil || !strings.Contains(err.Error(), "timeout") {
		t.Fatalf("ready timeout = %v", err)
	}
	close(done)
	if _, err := waitReady(filepath.Join(dir, "ready.json"), done, time.Second); err == nil || !strings.Contains(err.Error(), "before readiness") {
		t.Fatalf("leader exit = %v", err)
	}
	os.WriteFile(filepath.Join(dir, "ready.json"), []byte("{"), 0o600)
	if _, err := waitReady(filepath.Join(dir, "ready.json"), make(chan struct{}), time.Second); err == nil {
		t.Fatal("malformed ready accepted")
	}
	if waitClosed(make(chan struct{}), time.Millisecond) {
		t.Fatal("waitClosed")
	}
}

func TestRunHelperIncompleteEnvironment(t *testing.T) {
	results := filepath.Join(t.TempDir(), "r.json")
	// Without EnvHelper=1 the helper refuses to run (so a test runner can
	// never become a subreaper), even with an otherwise complete environment.
	env := map[string]string{EnvResults: results, EnvFake: "x", EnvWorkDir: "y", EnvGroups: "z"}
	if RunHelper(func(k string) string { return env[k] }) != 1 {
		t.Fatal("non-helper process must refuse")
	}
	b, _ := os.ReadFile(results)
	if !strings.Contains(string(b), "dedicated helper process") {
		t.Fatalf("results = %s", b)
	}
	env = map[string]string{EnvHelper: "1", EnvResults: results}
	if RunHelper(func(k string) string { return env[k] }) != 1 {
		t.Fatal("incomplete helper env must fail")
	}
	b, _ = os.ReadFile(results)
	if !strings.Contains(string(b), "helper environment incomplete") {
		t.Fatalf("results = %s", b)
	}
	env[EnvResults] = filepath.Join(t.TempDir(), "missing-dir", "r.json")
	if RunHelper(func(k string) string { return env[k] }) != 1 {
		t.Fatal("unwritable results must fail")
	}
}

func TestRunCaseCleansUpWhenLeaderDiesEarly(t *testing.T) {
	// The fake (this binary in exit-early mode) exits before readiness: the
	// case fails, and cleanup still verifies the group is gone.
	t.Setenv(envTestExit, "1")
	r := RunCase(context.Background(), Config{Fake: os.Args[0], WorkDir: t.TempDir(), Sys: SysSignaler{}, Clock: RealClock{}}, Cases[0])
	if r.Pass || !strings.Contains(strings.Join(r.Errors, ";"), "before readiness") {
		t.Fatalf("result = %+v", r)
	}
	if !r.LeaderGoneESRCH || !r.GroupGoneESRCH || r.Leader.ExitCode != 5 {
		t.Fatalf("teardown not verified: %+v", r)
	}
	r = RunCase(context.Background(), Config{Fake: filepath.Join(t.TempDir(), "missing"), WorkDir: t.TempDir(), Sys: SysSignaler{}, Clock: RealClock{}}, Cases[0])
	if r.Pass || !strings.Contains(strings.Join(r.Errors, ";"), "start fake") {
		t.Fatalf("missing fake = %+v", r)
	}
	r = RunCase(context.Background(), Config{Fake: "x", WorkDir: "/dev/null/x", Sys: SysSignaler{}, Clock: RealClock{}}, Cases[0])
	if r.Pass || len(r.Errors) == 0 {
		t.Fatal("bad workdir accepted")
	}
}

// TestRunCaseKillsGroupWhenLeaderDiesEarlyWithLiveDescendant covers the
// failed-startup window: the leader exits before readiness while a
// descendant in its group is still alive. Cleanup must KILL the group
// rather than spend ReapLimit waiting on it.
func TestRunCaseKillsGroupWhenLeaderDiesEarlyWithLiveDescendant(t *testing.T) {
	pidfile := filepath.Join(t.TempDir(), "sleeper.pid")
	t.Setenv(envTestOrphan, pidfile)
	sleeper := 0
	t.Cleanup(func() {
		if sleeper > 1 {
			syscall.Kill(sleeper, syscall.SIGKILL)
		}
	})
	start := time.Now()
	r := RunCase(context.Background(), Config{Fake: os.Args[0], WorkDir: t.TempDir(), Sys: SysSignaler{}, Clock: RealClock{}}, Cases[0])
	elapsed := time.Since(start)
	b, err := os.ReadFile(pidfile)
	if err != nil {
		t.Fatalf("sleeper pid not recorded: %v", err)
	}
	sleeper, _ = strconv.Atoi(string(b))
	if sleeper <= 1 {
		t.Fatalf("bad sleeper pid %q", b)
	}
	if !strings.Contains(strings.Join(r.Errors, ";"), "before readiness") || r.Pass {
		t.Fatalf("result errors = %v", r.Errors)
	}
	if !r.EmergencyKill {
		t.Fatal("no group KILL in the failed-startup window")
	}
	if !r.GroupGoneESRCH || !r.LeaderGoneESRCH {
		t.Fatalf("group not verified gone: %v", r.Errors)
	}
	if alive, err := Existence(SysSignaler{}, sleeper); alive || err != nil {
		t.Fatalf("descendant %d survived (%v)", sleeper, err)
	}
	if elapsed >= ReapLimit {
		t.Fatalf("cleanup took %v: waited out ReapLimit instead of killing the group", elapsed)
	}
}

// TestExperiment runs the real Linux (or native Darwin) experiment inside a
// dedicated helper process: this package's test binary in helper mode.
func TestExperiment(t *testing.T) {
	fake := testkit.BuildBinary(t, "./cmd/fake-adapter", "fake-adapter")
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	rep, err := RunExperiment(ctx, []string{os.Args[0]}, os.Environ(), fake, t.TempDir())
	if err != nil {
		t.Fatalf("experiment: %v\nreport: %+v", err, rep)
	}
	if !rep.Pass() {
		t.Fatalf("experiment failed: %+v", rep)
	}
	for _, c := range rep.Cases {
		t.Logf("%s: kill=%v grace=%.1fms leader=%+v descendant=%+v", c.Case.Name, c.KillSent, c.ObservedGraceMillis, c.Leader, c.Descendant)
	}
}

// TestEmergencyCleanupAfterHelperFailure: a helper that dies leaving a live
// recorded group must be cleaned up by its parent, and reported.
func TestEmergencyCleanupAfterHelperFailure(t *testing.T) {
	fake := testkit.BuildBinary(t, "./cmd/fake-adapter", "fake-adapter")
	dir := t.TempDir()
	rep, err := RunExperiment(context.Background(), []string{os.Args[0]}, append(os.Environ(), envTestAbandon+"=1"), fake, dir)
	if err == nil || !strings.Contains(err.Error(), "survived the helper") {
		t.Fatalf("err = %v rep = %+v", err, rep)
	}
	b, _ := os.ReadFile(filepath.Join(dir, "groups.txt"))
	pgid, _ := strconv.Atoi(strings.TrimSpace(string(b)))
	if pgid <= 1 {
		t.Fatalf("no recorded group: %q", b)
	}
	if alive, perr := Existence(SysSignaler{}, -pgid); alive || perr != nil {
		t.Fatalf("group %d still present (%v)", pgid, perr)
	}
	// A cancelled parent context kills the helper and still cleans up.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := RunExperiment(ctx, []string{os.Args[0]}, os.Environ(), fake, t.TempDir()); err == nil {
		t.Fatal("cancelled experiment returned success")
	}
	if _, err := RunExperiment(context.Background(), []string{filepath.Join(dir, "missing")}, nil, fake, t.TempDir()); err == nil {
		t.Fatal("missing helper started")
	}
	if _, err := RunExperiment(context.Background(), []string{os.Args[0]}, nil, fake, "/dev/null/x"); err == nil {
		t.Fatal("bad dir accepted")
	}
	_ = fmt.Sprint
}
