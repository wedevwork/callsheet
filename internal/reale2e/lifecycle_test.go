package reale2e

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/wedevwork/callsheet/internal/contract"
	"github.com/wedevwork/callsheet/internal/testkit"
)

// childMain is the re-executed helper child of the real process tests.
func childMain(mode string) int {
	switch mode {
	case "echo":
		fmt.Fprint(os.Stdout, "out\n")
		fmt.Fprint(os.Stderr, "err\n")
		return 0
	case "exit3":
		return 3
	case "big":
		fmt.Fprint(os.Stdout, strings.Repeat("x", 10000))
		return 0
	case "sleep":
		time.Sleep(time.Minute)
		return 0
	case "spawn-exit", "spawn-sleep":
		// A descendant that stays in this child's process group.
		exe, _ := os.Executable()
		c := exec.Command(exe, "-test.run=^$")
		c.Env = append(os.Environ(), "REALE2E_TEST_CHILD=sleep")
		if err := c.Start(); err != nil {
			return 98
		}
		fmt.Fprintf(os.Stdout, "%d\n", c.Process.Pid)
		if mode == "spawn-sleep" {
			time.Sleep(time.Minute)
		}
		return 0
	case "ignore-term":
		signal.Ignore(syscall.SIGTERM)
		fmt.Fprint(os.Stdout, "ready\n")
		time.Sleep(time.Minute)
		return 0
	}
	return 99
}

// child returns a spec re-executing this test binary as a helper child.
func childSpec(t *testing.T, mode string) ProcSpec {
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	return ProcSpec{Path: exe, Args: []string{"-test.run=^$"}, Env: append(os.Environ(), "REALE2E_TEST_CHILD="+mode)}
}

// TestExecLauncher (UT-7): real children by captured handle: output
// capture, exit codes, a bounded timeout, SIGTERM and SIGKILL
// escalation, and an unstartable executable.
func TestExecLauncher(t *testing.T) {
	t.Parallel()
	clock := realClock{}
	res, err := runCommand(ExecLauncher{}, clock, childSpec(t, "echo"), 30*time.Second, 1024)
	if err != nil || res.ExitCode != 0 || string(res.Stdout) != "out\n" || string(res.Stderr) != "err\n" || res.TimedOut || res.Truncated ||
		res.Stop.StoppedBy != StoppedExited || !res.Stop.ExitVerified {
		t.Fatalf("echo = %+v %v", res, err)
	}
	if res, err := runCommand(ExecLauncher{}, clock, childSpec(t, "exit3"), 30*time.Second, 1024); err != nil || res.ExitCode != 3 || res.Stop.Exit != "exit 3" {
		t.Fatalf("exit3 = %+v %v", res, err)
	}
	if res, err := runCommand(ExecLauncher{}, clock, childSpec(t, "big"), 30*time.Second, 100); err != nil || !res.Truncated || len(res.Stdout) != 100 {
		t.Fatalf("big = %d %v %v", len(res.Stdout), res.Truncated, err)
	}
	res, err = runCommand(ExecLauncher{}, clock, childSpec(t, "sleep"), 200*time.Millisecond, 1024)
	if err != nil || !res.TimedOut || res.Stop.StoppedBy != StoppedTerm || !res.Stop.ExitVerified || res.Signal != "terminated" {
		t.Fatalf("sleep = %+v %v", res, err)
	}
	if _, err := runCommand(ExecLauncher{}, clock, ProcSpec{Path: "/nonexistent/reale2e-child"}, time.Second, 10); !errors.Is(err, errCommandStart) {
		t.Fatalf("missing executable = %v", err)
	}
	// A child ignoring SIGTERM is killed after the grace.
	r, w, _ := os.Pipe()
	spec := childSpec(t, "ignore-term")
	spec.Stdout = w
	p, err := ExecLauncher{}.Start(spec)
	w.Close()
	if err != nil {
		t.Fatal(err)
	}
	if line, err := bufio.NewReader(r).ReadString('\n'); err != nil || line != "ready\n" {
		t.Fatalf("child not ready: %q %v", line, err)
	}
	rec := stopProc("stubborn", p, clock, 300*time.Millisecond)
	if rec.StoppedBy != StoppedKill || !rec.ExitVerified || rec.Exit != "signal killed" || rec.PID != p.PID() {
		t.Fatalf("stubborn = %+v", rec)
	}
	if err := p.Signal(syscall.SIGTERM); !errors.Is(err, os.ErrProcessDone) {
		t.Fatalf("signal after reap = %v", err)
	}
}

// TestProcessGroupStop (C4 of the round-1 review, UT-7): a descendant in
// the captured process group is stopped and its end verified, whether the
// leader exits first or is still running. It uses only re-executed test
// children, so it runs on Linux and macOS alike.
func TestProcessGroupStop(t *testing.T) {
	t.Parallel()
	for _, mode := range []string{"spawn-exit", "spawn-sleep"} {
		t.Run(mode, func(t *testing.T) {
			t.Parallel()
			r, w, _ := os.Pipe()
			spec := childSpec(t, mode)
			spec.Stdout = w
			p, err := ExecLauncher{}.Start(spec)
			w.Close()
			if err != nil {
				t.Fatal(err)
			}
			line, err := bufio.NewReader(r).ReadString('\n')
			if err != nil {
				t.Fatalf("no descendant PID: %v", err)
			}
			pid, err := strconv.Atoi(strings.TrimSpace(line))
			if err != nil {
				t.Fatal(err)
			}
			// A failed test must not leave the descendant behind (only
			// then: after a verified stop the PID may already be reused).
			defer func() {
				if t.Failed() {
					syscall.Kill(pid, syscall.SIGKILL)
				}
			}()
			if mode == "spawn-exit" {
				<-p.Done()
				if code, _ := p.Exit(); code != 0 || p.GroupGone() {
					t.Fatalf("the leader exited %d; group gone %v with a live descendant", code, p.GroupGone())
				}
			}
			if err := syscall.Kill(pid, 0); err != nil {
				t.Fatalf("the descendant is not running: %v", err)
			}
			rec := stopProc(mode, p, realClock{}, 10*time.Second)
			if rec.StoppedBy != StoppedTerm || !rec.ExitVerified || !p.GroupGone() {
				t.Fatalf("stop = %+v, group gone %v", rec, p.GroupGone())
			}
			if err := syscall.Kill(pid, 0); !errors.Is(err, syscall.ESRCH) {
				t.Fatalf("the descendant survived the verified stop: %v", err)
			}
			if err := p.Signal(syscall.SIGTERM); !errors.Is(err, os.ErrProcessDone) {
				t.Fatalf("signal to a gone group = %v", err)
			}
		})
	}
}

// TestStopEscalation (UT-7): on a fake clock, SIGTERM is followed by
// SIGKILL only after the grace, and an exit never observed (a failed Wait)
// is recorded as unverified.
func TestStopEscalation(t *testing.T) {
	t.Parallel()
	clock := testkit.NewFakeClock(time.Unix(0, 0))
	// run drives the fake clock one group poll at a time until stopProc
	// returns, noting when SIGKILL was first sent.
	run := func(p *fakeProc, grace time.Duration) (ProcRecord, time.Duration) {
		got := make(chan ProcRecord, 1)
		start := clock.Now()
		go func() { got <- stopProc("x", p, clock, grace) }()
		killAt := time.Duration(-1)
		for {
			select {
			case r := <-got:
				p.mu.Lock()
				if slices.Contains(p.signals, syscall.SIGKILL) && killAt < 0 {
					killAt = clock.Now().Sub(start)
				}
				p.mu.Unlock()
				return r, killAt
			default:
			}
			if clock.AwaitWaiter(20*time.Millisecond, func(ws []testkit.Waiter) bool { return len(ws) > 0 }) == nil {
				p.mu.Lock()
				k := slices.Contains(p.signals, syscall.SIGKILL)
				p.mu.Unlock()
				if k && killAt < 0 {
					killAt = clock.Now().Sub(start)
				}
				clock.Advance(groupPoll)
			}
		}
	}
	// SIGTERM ignored: SIGKILL only after the grace.
	p := &fakeProc{pid: 7, done: make(chan struct{}), ignoreTerm: true}
	rec, killAt := run(p, time.Second)
	if rec.StoppedBy != StoppedKill || !rec.ExitVerified || !slices.Equal(p.signals, []syscall.Signal{syscall.SIGTERM, syscall.SIGKILL}) || killAt < time.Second {
		t.Fatalf("escalation = %+v %v kill at %s", rec, p.signals, killAt)
	}
	// Neither signal ends it: the exit is unverified.
	q := &fakeProc{pid: 8, done: make(chan struct{}), ignoreTerm: true, ignoreKill: true}
	if rec, _ := run(q, time.Second); rec.ExitVerified || rec.StoppedBy != StoppedKill || rec.Exit != "" {
		t.Fatalf("unverified = %+v", rec)
	}
	// An exit racing the signal is observed without escalation.
	r := &fakeProc{pid: 9, done: make(chan struct{})}
	r.exit(0, "")
	if rec := stopProc("z", r, clock, time.Second); rec.StoppedBy != StoppedExited || rec.Exit != "exit 0" || len(r.signals) != 0 {
		t.Fatalf("raced exit = %+v", rec)
	}
	// The leader exited first but a descendant stays in its group: the
	// group is still signalled and its end awaited.
	d := &fakeProc{pid: 10, done: make(chan struct{}), lingering: true}
	d.exit(0, "")
	if rec, _ := run(d, time.Second); rec.StoppedBy != StoppedTerm || !rec.ExitVerified || rec.Exit != "exit 0" || !d.GroupGone() {
		t.Fatalf("lingering descendant = %+v", rec)
	}
	e := &fakeProc{pid: 11, done: make(chan struct{}), lingering: true, ignoreTerm: true}
	e.exit(0, "")
	if rec, killAt := run(e, time.Second); rec.StoppedBy != StoppedKill || !rec.ExitVerified || killAt < time.Second {
		t.Fatalf("stubborn descendant = %+v at %s", rec, killAt)
	}
}

// TestAttemptLifecycle (FP-8): a feature task over its bound is cancelled
// through the plane and the attempt fails with every owned child stopped
// and the evidence kept; an unverifiable child keeps the runtime with
// recovery instructions and fails cleanup; missing owner-child exits fail;
// a rerun is a new, isolated attempt.
func TestAttemptLifecycle(t *testing.T) {
	t.Parallel()
	t.Run("feature timeout", func(t *testing.T) {
		t.Parallel()
		f := newFakeWorld(t)
		// The designer task never finishes; the owner has closed the
		// session and recorded its exits (its wait for hop 1 included).
		f.coord.before = func(c *coordScript) bool {
			if c.step == 1 {
				writeCoordinatorExit(t, c.s, 1)
				return true
			}
			return false
		}
		if code := f.attempt(); code != 1 {
			t.Fatalf("attempt = %d", code)
		}
		s := f.sup
		if s.failCode != "feature_deadline_exceeded" || !slices.Contains(f.plane.cancels, f.coord.tasks[0]) {
			t.Fatalf("failure %q cancels %v", s.failCode, f.plane.cancels)
		}
		b := f.bundle()
		var c CleanupRecord
		readDoc(t, filepath.Join(b, "cleanup.json"), &c)
		if !c.Complete || len(c.Processes) != 7 || !slices.Equal(c.CancelledTasks, []string{f.coord.tasks[0]}) {
			t.Fatalf("cleanup %+v", c)
		}
		for _, p := range f.launcher.procs {
			if _, sig := p.Exit(); !isDone(p) {
				t.Fatalf("child %d still running (%s)", p.pid, sig)
			}
		}
		if _, err := os.Stat(s.runtime); !os.IsNotExist(err) {
			t.Fatalf("runtime kept after a verified cleanup: %v", err)
		}
		if _, err := os.Stat(filepath.Join(b, "hops", "01", "task.json")); err != nil {
			t.Fatal("the cancelled hop's snapshot was not kept")
		}
		r := f.report()
		if r.Result != ResultFail || !slices.Contains(failedCodes(r), "attempt_failed") {
			t.Fatalf("report %v", failedCodes(r))
		}
	})
	t.Run("unverified child", func(t *testing.T) {
		t.Parallel()
		f := newFakeWorld(t)
		f.override = func(s ProcSpec) (behavior, bool) {
			if s.Path == fakeCallsheet && len(s.Args) > 1 && s.Args[0] == "plane" && s.Args[1] == "run" {
				return behavior{long: true, ignoreTerm: true, ignoreKill: true, stderr: `{"msg":"listening","bind":"127.0.0.1:43210"}` + "\n"}, true
			}
			return behavior{}, false
		}
		if code := f.attempt(); code != 1 {
			t.Fatalf("attempt = %d", code)
		}
		if _, err := os.Stat(f.sup.runtime); err != nil {
			t.Fatalf("runtime removed despite an unverified child: %v", err)
		}
		if !strings.Contains(f.stdout.String(), "cleanup is unresolved") || !strings.Contains(f.stdout.String(), "plane pid") {
			t.Fatalf("no recovery instructions:\n%s", f.stdout.String())
		}
		if !slices.Contains(failedCodes(f.report()), "process_exit_unverified") {
			t.Fatal(failedCodes(f.report()))
		}
	})
	t.Run("bounded cleanup", func(t *testing.T) {
		t.Parallel()
		// An unsettled task and four unstoppable children would need
		// 150s of waits; cleanup stays within its 2m bound and says it is
		// incomplete.
		f := newFakeWorld(t)
		f.coord.stopAfter = 1
		f.plane.cancelIgnored = true
		runs := map[string]int{}
		f.override = func(s ProcSpec) (behavior, bool) {
			if s.Path == fakeCallsheet && len(s.Args) > 1 && s.Args[1] == "run" {
				// The plane and the reconnected sidecars resist every
				// signal (the first sidecar runs stop for the reconnect).
				runs[s.Args[3]]++
				b := f.base(s)
				if s.Args[0] == "plane" || runs[s.Args[3]] > 1 {
					b.ignoreTerm, b.ignoreKill = true, true
				}
				return b, true
			}
			return behavior{}, false
		}
		f.attempt()
		var c CleanupRecord
		readDoc(t, filepath.Join(f.bundle(), "cleanup.json"), &c)
		s, _ := parseTime(c.StartedAt)
		e, _ := parseTime(c.FinishedAt)
		if c.Complete || e.Sub(s) > CleanupBound || len(c.UnsettledTasks) == 0 {
			t.Fatalf("cleanup took %s, complete %v, unsettled %v", e.Sub(s), c.Complete, c.UnsettledTasks)
		}
	})
	t.Run("owner children", func(t *testing.T) {
		t.Parallel()
		for name, mutate := range map[string]func(dir string){
			"missing":      func(dir string) { os.Remove(filepath.Join(dir, "coordinator-exit.json")) },
			"session open": func(dir string) { editOwnerExit(t, dir, func(c *CoordinatorExit) { c.SessionClosed = false }) },
			"MCP child":    func(dir string) { editOwnerExit(t, dir, func(c *CoordinatorExit) { c.MCPChildExited = false }) },
			"a wait handle": func(dir string) {
				editOwnerExit(t, dir, func(c *CoordinatorExit) { c.WaitHandles = c.WaitHandles[1:] })
			},
			"invalid record": func(dir string) { os.WriteFile(filepath.Join(dir, "coordinator-exit.json"), []byte("{}"), 0o600) },
		} {
			f := newFakeWorld(t)
			f.coord.mutateOwner = mutate
			if code := f.attempt(); code != 1 || !slices.Contains(failedCodes(f.report()), "owner_children_unverified") {
				t.Fatalf("%s: attempt = %d %v", name, code, failedCodes(f.report()))
			}
			// Without the owner-child exit evidence nothing is removed and
			// the recovery steps are printed.
			var c CleanupRecord
			readDoc(t, filepath.Join(f.bundle(), "cleanup.json"), &c)
			if _, err := os.Stat(f.sup.runtime); err != nil || c.Complete || !strings.Contains(f.stdout.String(), "coordinator-exit.json") {
				t.Fatalf("%s: runtime %v, cleanup complete %v\n%s", name, err, c.Complete, f.stdout.String())
			}
		}
	})
	t.Run("rerun isolation", func(t *testing.T) {
		t.Parallel()
		f := newFakeWorld(t)
		if code := f.attempt(); code != 0 {
			t.Fatalf("first attempt = %d", code)
		}
		first := f.bundle()
		before := treeDigest(t, first)
		firstRun, firstRuntime, firstWS := f.sup.runID, f.sup.runtime, f.sup.workspace.Name
		// Reusing the completed bundle's destination is refused before
		// anything happens.
		r := newFakeWorld(t)
		r.checkout, r.evidence, r.tempRoot = f.checkout, f.evidence, f.tempRoot
		if code := r.attempt(); code != 2 || !strings.Contains(r.stderr.String(), "must not exist yet") || len(r.launcher.argv()) != 0 {
			t.Fatalf("reused destination = %d %q", code, r.stderr.String())
		}
		g := newFakeWorld(t)
		g.checkout, g.tempRoot = f.checkout, f.tempRoot
		g.evidence = filepath.Join(filepath.Dir(f.evidence), "attempt-2")
		g.clock.advance(time.Hour)
		g.randSeed = 100
		g.launcher = &fakeLauncher{script: g.script}
		g.coord.stopAfter = 0
		g.coord.skipOwner = true
		if code := g.attempt(); code != 1 {
			t.Fatalf("second attempt = %d", code)
		}
		if g.sup.runID == firstRun || g.sup.runtime == firstRuntime || g.sup.workspace.Name == firstWS {
			t.Fatal("the rerun reused the first attempt's identity")
		}
		if treeDigest(t, first) != before {
			t.Fatal("the rerun changed the first bundle")
		}
		es, _ := os.ReadDir(filepath.Dir(f.evidence))
		if len(es) != 2 {
			t.Fatalf("bundles %v", es)
		}
		var m Manifest
		readDoc(t, filepath.Join(g.bundle(), "manifest.json"), &m)
		if m.RunID != g.sup.runID {
			t.Fatalf("manifest run ID %q", m.RunID)
		}
	})
	t.Run("interrupted", func(t *testing.T) {
		t.Parallel()
		f := newFakeWorld(t)
		ctx, cancel := context.WithCancel(context.Background())
		f.ctx = ctx
		f.coord.stopAfter = 1
		f.plane.onList = nil
		hooked := false
		f.override = func(s ProcSpec) (behavior, bool) {
			if !hooked && s.Path == fakeGo && strings.HasPrefix(strings.Join(s.Args, " "), "test") {
				hooked = true
				cancel()
			}
			return behavior{}, false
		}
		if code := f.attempt(); code != 1 || f.sup.failCode != "interrupted" {
			t.Fatalf("attempt = %d %q", code, f.sup.failCode)
		}
	})
	t.Run("attempt deadline", func(t *testing.T) {
		t.Parallel()
		f := newFakeWorld(t)
		f.coord.stopAfter = 9 // never finishes
		if code := f.attempt(); code != 1 || f.sup.failCode != "attempt_deadline_exceeded" {
			t.Fatalf("attempt = %d %q", code, f.sup.failCode)
		}
	})
}

// isDone reports a reaped fake child.
func isDone(p *fakeProc) bool {
	select {
	case <-p.done:
		return true
	default:
		return false
	}
}

// readDoc reads a JSON document.
func readDoc(t *testing.T, p string, v any) {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	if err := decodeStrict(b, v); err != nil {
		t.Fatal(err)
	}
}

// treeDigest digests every file of a directory.
func treeDigest(t *testing.T, dir string) string {
	t.Helper()
	var b strings.Builder
	filepath.Walk(dir, func(p string, info os.FileInfo, err error) error {
		if err == nil && info.Mode().IsRegular() {
			data, _ := os.ReadFile(p)
			b.WriteString(p + ":" + sha256Hex(data) + "\n")
		}
		return nil
	})
	return sha256Hex([]byte(b.String()))
}

// TestLifecycleUnits (UT-7): the preflight watchdog cancels at 2m despite
// the 10m role timeout, a lost dispatch answer is adopted only when
// unambiguous, and the runtime is removed only through its own marker and
// canonical path.
func TestLifecycleUnits(t *testing.T) {
	t.Parallel()
	t.Run("preflight watchdog", func(t *testing.T) {
		t.Parallel()
		f := newFakeWorld(t)
		f.plane.preflight = func(i int) (string, *string, bool, bool) {
			return contract.TaskSucceeded, ptr(AuthToken), false, i != 1
		}
		var start time.Time
		f.override = func(s ProcSpec) (behavior, bool) { return behavior{}, false }
		if code := f.attempt(); code != 1 || f.sup.failCode != "deadline_exceeded" {
			t.Fatalf("attempt = %d %q", code, f.sup.failCode)
		}
		_ = start
		v := readTask(t, f.bundle(), "preflight/02")
		d, ok := duration(v)
		if v.State != contract.TaskCancelled || !ok || d < PreflightBound || d >= RoleTimeout {
			t.Fatalf("preflight 2 = %s after %s", v.State, d)
		}
		if len(f.plane.cancels) != 1 {
			t.Fatalf("cancels %v", f.plane.cancels)
		}
		if _, err := os.Stat(filepath.Join(f.bundle(), "hops")); !os.IsNotExist(err) {
			t.Fatal("feature work started after a failed preflight")
		}
	})
	t.Run("preflight never settles", func(t *testing.T) {
		t.Parallel()
		f := newFakeWorld(t)
		f.plane.preflight = func(i int) (string, *string, bool, bool) { return "", nil, false, false }
		f.plane.cancelIgnored = true
		if code := f.attempt(); code != 1 {
			t.Fatalf("attempt = %d", code)
		}
		var c CleanupRecord
		readDoc(t, filepath.Join(f.bundle(), "cleanup.json"), &c)
		if c.Complete || len(c.UnsettledTasks) != 1 {
			t.Fatalf("cleanup %+v", c)
		}
	})
	t.Run("lost dispatch", func(t *testing.T) {
		t.Parallel()
		for _, tc := range []struct {
			name    string
			extra   int
			listErr error
			want    string
		}{{"adopted", 0, nil, ""}, {"ambiguous", 1, nil, "dispatch_lost_ambiguous"}, {"listing fails", 0, errors.New("down"), "dispatch_lost_ambiguous"}} {
			t.Run(tc.name, func(t *testing.T) {
				t.Parallel()
				f := newFakeWorld(t)
				s := &supervisor{w: f.world(), ctx: context.Background(), known: map[string]string{}, runID: "20261010T120000Z-01020304"}
				s.plane = f.plane
				f.plane.AddRole(context.Background(), contract.RoleConfig{ID: HopDesigner, Name: HopDesigner, Node: hexID("n_", 1), Adapter: "codex", Model: "m", Effort: "low", Concurrency: 1})
				req := contract.DispatchRequest{Target: contract.TaskTarget{Kind: contract.TargetID, Value: HopDesigner}, Goal: "g", Payload: []string{markerFor(s.runID, "x")},
					Acceptance: "a", RequestedBy: requester}
				for i := 0; i < tc.extra; i++ {
					f.plane.Dispatch(context.Background(), req)
				}
				f.plane.dispatchLost = true
				f.plane.listErr = tc.listErr
				id, err := s.dispatchOwn(req, s.w.Clock.Now().Add(PreflightBound))
				if tc.want == "" && (err != nil || !contract.ValidTaskID(id)) || tc.want != "" && codeOf(err) != tc.want {
					t.Fatalf("dispatchOwn = %q %v", id, err)
				}
			})
		}
		f := newFakeWorld(t)
		s := &supervisor{w: f.world(), ctx: context.Background(), known: map[string]string{}, runID: "20261010T120000Z-01020304", plane: f.plane}
		f.plane.dispatchErr = func(contract.DispatchRequest) error { return contract.New(contract.CodeInvalidArgument, "bad") }
		if _, err := s.dispatchOwn(contract.DispatchRequest{Payload: []string{"m"}}, s.w.Clock.Now().Add(PreflightBound)); codeOf(err) != "dispatch_refused" {
			t.Fatalf("refused dispatch = %v", err)
		}
	})
	t.Run("premature task settles", func(t *testing.T) {
		t.Parallel()
		// A cancelled out-of-chain admission is awaited too: one that
		// never settles leaves cleanup incomplete.
		f := newFakeWorld(t)
		f.coord.dispatchEarlyCoder = true
		f.plane.cancelIgnored = true
		if code := f.attempt(); code != 1 {
			t.Fatalf("attempt = %d", code)
		}
		var c CleanupRecord
		readDoc(t, filepath.Join(f.bundle(), "cleanup.json"), &c)
		if c.Complete || !slices.Contains(c.UnsettledTasks, f.coord.tasks[2]) {
			t.Fatalf("cleanup %+v", c)
		}
	})
	t.Run("symlinked temporary root", func(t *testing.T) {
		t.Parallel()
		// macOS /tmp and /var are links: the runtime is still canonical
		// and removed after a verified cleanup.
		f := newFakeWorld(t)
		link := filepath.Join(shortTemp(t), "l")
		if err := os.Symlink(f.tempRoot, link); err != nil {
			t.Fatal(err)
		}
		f.tempRoot = link
		if code := f.attempt(); code != 0 {
			t.Fatalf("attempt = %d %s", code, f.stderr.String())
		}
		if _, err := os.Stat(f.sup.runtime); !os.IsNotExist(err) || strings.HasPrefix(f.sup.runtime, link) {
			t.Fatalf("runtime %s kept or not canonical: %v", f.sup.runtime, err)
		}
	})
	t.Run("runtime removal", func(t *testing.T) {
		t.Parallel()
		root := shortTemp(t)
		mk := func(marker string) *supervisor {
			rt, _ := os.MkdirTemp(root, "ce-")
			os.WriteFile(filepath.Join(rt, ownerMarker), []byte(marker), 0o600)
			os.WriteFile(filepath.Join(rt, "receipt"), []byte("x"), 0o400)
			return &supervisor{w: &World{TempRoot: root}, runtime: rt, runID: "20261010T120000Z-01020304", stdout: &syncBuffer{}}
		}
		s := mk("20261010T120000Z-01020304\n")
		if err := s.removeRuntime(); err != nil {
			t.Fatal(err)
		}
		if _, err := os.Stat(s.runtime); !os.IsNotExist(err) {
			t.Fatal("not removed")
		}
		if err := mk("someone else\n").removeRuntime(); err == nil {
			t.Fatal("removed another run's runtime")
		}
		s = mk("20261010T120000Z-01020304\n")
		link := filepath.Join(root, "ce-link")
		os.Symlink(s.runtime, link)
		s.runtime = link
		if err := s.removeRuntime(); err == nil {
			t.Fatal("removed through a symlink")
		}
		other, _ := os.MkdirTemp(root, "zz-")
		s.runtime = other
		if err := s.removeRuntime(); err == nil {
			t.Fatal("removed a directory it did not create")
		}
		s.runtime = ""
		if err := s.removeRuntime(); err != nil {
			t.Fatal(err)
		}
		s.w.TempRoot = filepath.Join(root, "missing")
		s.runtime = other
		if err := s.removeRuntime(); err == nil {
			t.Fatal("removed outside its root")
		}
	})
}

// writeCoordinatorExit writes the owner's exit record for the first n
// hops' waits.
func writeCoordinatorExit(t *testing.T, s *supervisor, n int) {
	t.Helper()
	ce := CoordinatorExit{Schema: CoordExitSchema, SessionClosed: true, MCPChildExited: true, WaitHandles: []WaitHandleExit{}}
	for i := 0; i < n; i++ {
		ce.WaitHandles = append(ce.WaitHandles, WaitHandleExit{Handle: waitHandle(i)})
	}
	b, _ := encodeJSON(ce)
	if err := os.WriteFile(filepath.Join(s.runtime, "owner", "coordinator-exit.json"), b, 0o600); err != nil {
		t.Fatal(err)
	}
}

// editOwnerExit edits the owner's exit record in dir.
func editOwnerExit(t *testing.T, dir string, f func(c *CoordinatorExit)) {
	t.Helper()
	p := filepath.Join(dir, "coordinator-exit.json")
	var c CoordinatorExit
	readDoc(t, p, &c)
	f(&c)
	b, _ := encodeJSON(c)
	os.WriteFile(p, b, 0o600)
}

// TestUnresolvedCommandGroups (C4 of the round-2 review): a short command
// whose process group cannot be proved gone (a descendant that resists
// SIGTERM and SIGKILL) fails its step, keeps its captured handle for
// cleanup, and keeps the runtime with recovery instructions: never a PASS
// with a complete cleanup. A descendant that does stop is harmless.
func TestUnresolvedCommandGroups(t *testing.T) {
	t.Parallel()
	stubborn := behavior{lingering: true, ignoreTerm: true, ignoreKill: true}
	isFinal := func(s ProcSpec) bool { return strings.HasSuffix(s.Dir, filepath.Join("final", "src")) }
	isBaseline := func(s ProcSpec) bool { return strings.HasSuffix(s.Dir, filepath.Join("baseline", "src")) }
	cases := map[string]struct {
		match func(s ProcSpec) bool
		code  string
	}{
		"baseline go test": {func(s ProcSpec) bool { return s.Path == fakeGo && s.Args[0] == "test" && isBaseline(s) }, "baseline_tests_failed"},
		"final go test":    {func(s ProcSpec) bool { return s.Path == fakeGo && s.Args[0] == "test" && isFinal(s) }, "final_tests_failed"},
		"final go version": {func(s ProcSpec) bool { return s.Path == fakeGo && s.Args[0] == "version" && isFinal(s) }, "final_tests_failed"},
		"vendor version":   {func(s ProcSpec) bool { return s.Path == fakeGrok }, "version_preflight_failed"},
		"git version":      {func(s ProcSpec) bool { return s.Path == fakeGit }, "version_preflight_failed"},
		"plane init":       {func(s ProcSpec) bool { return s.Path == fakeCallsheet && s.Args[0] == "plane" && s.Args[1] == "init" }, "plane_start_failed"},
		"sidecar enroll": {func(s ProcSpec) bool {
			return s.Path == fakeCallsheet && s.Args[0] == "sidecar" && s.Args[1] == "enroll"
		}, "sidecar_start_failed"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			f := newFakeWorld(t)
			f.override = func(s ProcSpec) (behavior, bool) {
				if tc.match(s) {
					b := f.base(s)
					b.lingering, b.ignoreTerm, b.ignoreKill = stubborn.lingering, stubborn.ignoreTerm, stubborn.ignoreKill
					return b, true
				}
				return behavior{}, false
			}
			if code := f.attempt(); code != 1 || f.sup.failCode != tc.code {
				t.Fatalf("attempt = %d %q, want %s", code, f.sup.failCode, tc.code)
			}
			var c CleanupRecord
			readDoc(t, filepath.Join(f.bundle(), "cleanup.json"), &c)
			unverified := 0
			for _, p := range c.Processes {
				if !p.ExitVerified {
					unverified++
				}
			}
			if c.Complete || unverified == 0 {
				t.Fatalf("cleanup complete %v with %d unverified groups: %+v", c.Complete, unverified, c.Processes)
			}
			// The runtime (when one exists yet) is kept and the unresolved
			// group's PID is printed for recovery.
			if f.sup.runtime != "" {
				if _, err := os.Stat(f.sup.runtime); err != nil {
					t.Fatalf("runtime removed: %v", err)
				}
			}
			out := f.stdout.String()
			if !strings.Contains(out, "cleanup is unresolved") || !strings.Contains(out, "  command ") {
				t.Fatalf("no recovery instructions:\n%s", out)
			}
			if r := f.report(); r.Result == ResultPass {
				t.Fatal("PASS with an unresolved command group")
			}
		})
	}
	t.Run("descendant that stops", func(t *testing.T) {
		t.Parallel()
		f := newFakeWorld(t)
		f.override = func(s ProcSpec) (behavior, bool) {
			if s.Path == fakeGo && s.Args[0] == "test" {
				b := f.base(s)
				b.lingering = true
				return b, true
			}
			return behavior{}, false
		}
		if code := f.attempt(); code != 0 {
			t.Fatalf("attempt = %d %q", code, f.sup.failCode)
		}
	})
}
