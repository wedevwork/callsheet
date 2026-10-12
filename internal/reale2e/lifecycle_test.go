package reale2e

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
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
		// The designer task never finishes while phase 1 waits for it: the
		// still running coordinator group is stopped by its handle.
		f.coord.before = func(c *coordScript) bool { return c.step == 1 }
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
		if !c.Complete || len(c.Processes) != 8 || !slices.Equal(c.CancelledTasks, []string{f.coord.tasks[0]}) {
			t.Fatalf("cleanup %+v", c)
		}
		var ce CoordinatorExit
		readDoc(t, filepath.Join(b, filepath.FromSlash(coordinatorExitFile)), &ce)
		if len(ce.Phases) != 1 || ce.Phases[0].StoppedBy != StoppedTerm || !ce.Phases[0].GroupGone || ce.Phases[0].ExitCode != nil {
			t.Fatalf("coordinator exit %+v", ce)
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
		// A3.8: the timeout itself fails (deadlines); the lifecycle and the
		// hop's outcome it cut short (the watchdog cancelled the task after
		// the stop, review C1) are not reached and name it, and that
		// cancellation of the chain's own task is no extra admission.
		r := f.report()
		ev, hops := criterion(r, CritEvents), criterion(r, CritHops)
		if r.Result != ResultFail || ev.Result != ResultCriterionNotReached || ev.Cause != "feature_deadline_exceeded" ||
			!slices.Equal(criterion(r, CritDeadlines).Codes, []string{"feature_deadline_exceeded"}) || hops.Result != ResultCriterionNotReached ||
			criterion(r, CritAdmissions).Result == "fail" {
			t.Fatalf("report %v, events %+v, hops %+v", failedCodes(r), ev, hops)
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
	t.Run("early stop", earlyStopCases)
	t.Run("preflight stop", preflightStopCases)
	t.Run("stop before the wait", stopBeforeWaitCases)
	t.Run("coordinator phases", func(t *testing.T) {
		t.Parallel()
		for name, tc := range map[string]struct {
			set  func(c *coordScript)
			code string
		}{
			"leader reaped but group alive":      {func(c *coordScript) { c.lingering[0] = true }, "coordinator_group_alive"},
			"phase 2 group alive":                {func(c *coordScript) { c.lingering[1] = true }, "coordinator_group_alive"},
			"group survives cleanup":             {func(c *coordScript) { c.lingering[0], c.stubborn[0] = true, true }, "coordinator_group_alive"},
			"nonzero exit":                       {func(c *coordScript) { c.phaseExit[0] = 1 }, "coordinator_exit_nonzero"},
			"exit while background work remains": {func(c *coordScript) { c.earlyResult = true }, CodePhaseEarly},
			"transcript missing":                 {func(c *coordScript) { c.noTranscript = true }, CodeSessionUnobserved},
			"error result":                       {func(c *coordScript) { c.failResult[0] = true }, CodeSessionUnobserved},
			"denied tool":                        {func(c *coordScript) { c.denied = true }, CodeToolDenied},
			"failed tool in phase 2":             {func(c *coordScript) { c.toolFailed = true }, CodeToolFailed},
			"changed session on resume":          {func(c *coordScript) { c.phase2SID = "aaaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee" }, CodeSessionMismatch},
			"rewritten transcript":               {func(c *coordScript) { c.rewrite = true }, CodeSessionUnobserved},
			"stream overflow": {func(c *coordScript) {
				c.before = func(c *coordScript) bool {
					if c.step == 1 {
						io.WriteString(c.specs[0].Stdout, strings.Repeat("x", maxTextFile+1))
						return true
					}
					return false
				}
			}, "coordinator_stream_limit"},
		} {
			t.Run(name, func(t *testing.T) {
				t.Parallel()
				f := newFakeWorld(t)
				tc.set(f.coord)
				if code := f.attempt(); code != 1 || f.sup.failCode != tc.code {
					t.Fatalf("attempt = %d %q, want %s\n%s", code, f.sup.failCode, tc.code, f.stderr.String())
				}
				var c CleanupRecord
				readDoc(t, filepath.Join(f.bundle(), "cleanup.json"), &c)
				var ce CoordinatorExit
				readDoc(t, filepath.Join(f.bundle(), filepath.FromSlash(coordinatorExitFile)), &ce)
				switch {
				case strings.Contains(name, "group alive"):
					// A3.6: the group cleanup's last stop ended is proved
					// gone on the same captured handle and the observation
					// updated; the attempt still fails for the early
					// survival.
					if _, err := os.Stat(f.sup.runtime); !os.IsNotExist(err) || !c.Complete || !ce.Phases[len(ce.Phases)-1].GroupGone {
						t.Fatalf("runtime %v complete %v exit %+v", err, c.Complete, ce.Phases)
					}
					if r := f.report(); r.Result != ResultFail || criterion(r, CritCleanup).Result != "pass" {
						t.Fatalf("report %v", failedCodes(r))
					}
					return
				case name == "group survives cleanup":
					// A group no signal ends is not cleanup: the runtime is
					// kept with recovery instructions and the observation
					// stays unverified.
					if _, err := os.Stat(f.sup.runtime); err != nil || c.Complete || !strings.Contains(f.stdout.String(), "cleanup is unresolved") ||
						ce.Phases[0].GroupGone {
						t.Fatalf("runtime %v complete %v exit %+v", err, c.Complete, ce.Phases)
					}
					if !slices.Contains(criterion(f.report(), CritCleanup).Codes, "coordinator_group_unverified") {
						t.Fatalf("cleanup %v", failedCodes(f.report()))
					}
					return
				}
				if !c.Complete || coderDispatched(f.plane) != (f.sup.decision != nil) {
					t.Fatalf("cleanup %+v", c)
				}
				if f.term.activated != 0 && f.sup.decision == nil {
					t.Fatal("the owner was prompted after a failed phase 1")
				}
				if r := f.report(); r.Result != ResultFail {
					t.Fatal("PASS")
				}
				if name == "denied tool" {
					// W2: the private audit names the denied command and
					// cites its raw input line; report.txt does not.
					audit := string(mustRead(t, filepath.Join(f.bundle(), "audit.md")))
					if !strings.Contains(audit, "coordinator_tool_denied: Bash tool use toolu_") || !strings.Contains(audit, "command rm -rf /tmp/whatever") {
						t.Fatalf("the audit does not name the denied command:\n%s", audit)
					}
					var a Audit
					readDoc(t, filepath.Join(f.bundle(), "audit.json"), &a)
					cited := 0
					for _, row := range a.Rows {
						for _, ref := range row.Evidence {
							if ref.File == phaseStreamFiles[0] && len(ref.Lines) == 2 {
								cited++
							}
						}
					}
					if cited == 0 || strings.Contains(string(mustRead(t, filepath.Join(f.bundle(), "report.txt"))), "rm -rf") {
						t.Fatalf("raw input cited %d times, or report.txt names the arguments", cited)
					}
				}
			})
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
	t.Run("interrupted in phase 1", func(t *testing.T) {
		t.Parallel()
		f := newFakeWorld(t)
		ctx, cancel := context.WithCancel(context.Background())
		f.ctx = ctx
		f.coord.before = func(c *coordScript) bool {
			if c.step == 1 {
				cancel()
				return true
			}
			return false
		}
		if code := f.attempt(); code != 1 || f.sup.failCode != "interrupted" {
			t.Fatalf("attempt = %d %q", code, f.sup.failCode)
		}
		var ce CoordinatorExit
		readDoc(t, filepath.Join(f.bundle(), filepath.FromSlash(coordinatorExitFile)), &ce)
		if len(ce.Phases) != 1 || ce.Phases[0].StoppedBy != StoppedTerm {
			t.Fatalf("the running phase was not stopped by its group: %+v", ce)
		}
	})
	t.Run("evidence destination", func(t *testing.T) {
		t.Parallel()
		for name, git := range map[string]GitRunner{
			"not ignored": func(string, io.Writer, ...string) ([]byte, int, error) { return nil, 1, nil },
			"tracked path": func(dir string, _ io.Writer, args ...string) ([]byte, int, error) {
				if args[0] == "ls-files" {
					return []byte("design/real-e2e-runs/attempt/x\x00"), 0, nil
				}
				return nil, 0, nil
			},
			"git unavailable": func(string, io.Writer, ...string) ([]byte, int, error) { return nil, 0, errors.New("no git") },
		} {
			f := newFakeWorld(t)
			f.git = git
			if code := f.attempt(); code != 2 || !strings.Contains(f.stderr.String(), "Git-ignored") {
				t.Fatalf("%s: attempt = %d %q", name, code, f.stderr.String())
			}
			if _, err := os.Stat(f.evidence); !os.IsNotExist(err) || len(f.launcher.argv()) != 0 {
				t.Fatalf("%s: evidence or executables after a refused destination", name)
			}
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
		f.term.lines = nil
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

// preflightStopCases (FP-8, UT-7, A3.8): an attempt that stops in
// preflight (the fourth pair answers nothing) reports only that cause.
// Preflight fails with its code; every criterion whose producing step the
// stop left unreached is not_reached, references the stopping cause and
// carries no missing-evidence code of its own; those whose step ran and
// whose evidence holds (deployment, cleanup, environment, deadlines)
// pass; the overall result stays FAIL, not demonstrated. Evidence missing
// after its producing step still fails.
func preflightStopCases(t *testing.T) {
	t.Parallel()
	f := newFakeWorld(t)
	f.plane.preflight = func(j int) (string, *string, bool, bool) {
		if j == 3 {
			return contract.TaskSucceeded, ptr(""), false, true
		}
		return contract.TaskSucceeded, ptr(AuthToken), false, true
	}
	const cause = "preflight_not_authenticated"
	if code := f.attempt(); code != 1 || f.sup.failCode != cause {
		t.Fatalf("attempt = %d %q", code, f.sup.failCode)
	}
	b := f.bundle()
	var evs []Event
	for _, l := range strings.Split(strings.TrimSpace(string(mustRead(t, filepath.Join(b, "events.jsonl")))), "\n") {
		var e Event
		json.Unmarshal([]byte(l), &e)
		evs = append(evs, e)
	}
	stopSeq := evs[slices.IndexFunc(evs, func(e Event) bool { return e.Type == EvAttemptFailed })].Seq
	r := f.report()
	if r.Result != ResultFail || r.Claim != "not demonstrated" {
		t.Fatalf("result %s claim %q", r.Result, r.Claim)
	}
	passes := []string{CritRoles, CritCleanup, CritOwnerEnv, CritDeadlines}
	for _, c := range r.Criteria {
		switch {
		case c.ID == CritPreflight:
			if c.Result != "fail" || !slices.Equal(c.Codes, []string{cause}) || c.Cause != "" {
				t.Errorf("preflight %+v", c)
			}
		case slices.Contains(passes, c.ID):
			if c.Result != "pass" {
				t.Errorf("%s %+v, want pass", c.ID, c)
			}
		case c.Result != ResultCriterionNotReached || len(c.Codes) != 0 || c.Cause != cause || c.CauseSeq != stopSeq:
			t.Errorf("%s %+v, want not_reached by %s at %d", c.ID, c, cause, stopSeq)
		}
	}
	text := string(mustRead(t, filepath.Join(b, "report.txt")))
	for _, want := range []string{"result: FAIL", "claim: not demonstrated", "criterion preflight: fail (" + cause + ")\n",
		fmt.Sprintf("criterion hops: not_reached (not reached: %s, attempt failed at event %d)\n", cause, stopSeq), "preflight-03 token_echoed: true",
		"preflight-04 token_echoed: false"} {
		if !strings.Contains(text, want) {
			t.Errorf("report.txt lacks %q:\n%s", want, text)
		}
	}
	// The audit: preflight incorrect, deployment, cleanup and environment
	// correct, every other mechanical row unobserved as not reached.
	var a Audit
	readDoc(t, filepath.Join(b, "audit.json"), &a)
	for _, row := range a.Rows {
		switch {
		case row.Layer == LayerJudgment:
		case row.ID == "preflight":
			if row.Verdict != VerdictIncorrect || !slices.Contains(row.Criteria, "code:"+cause) {
				t.Errorf("preflight row %+v", row)
			}
		case row.ID == "deployment_roles" || row.ID == "cleanup" || row.ID == "owner_environment":
			if row.Verdict != VerdictCorrect {
				t.Errorf("%s row %+v", row.ID, row)
			}
		case row.Verdict != VerdictUnobserved || !strings.Contains(row.Observed, "not reached: "+cause) || !strings.HasPrefix(row.Correction, "not reached: "+cause) ||
			strings.Contains(row.Observed, "missing:"):
			t.Errorf("%s row %+v, want unobserved, not reached", row.ID, row)
		}
	}
	// Evidence missing after its producing step is still missing: the roles
	// were ready and the first preflights admitted before the stop. With
	// no setup copy of the flow, a pair other than the configured one (the
	// manifest's) still fails.
	for name, tc := range map[string]struct {
		rm         []string
		crit, want string
	}{
		"roles record removed":       {[]string{"setup/roles.json"}, CritRoles, "roles_missing"},
		"admitted preflight removed": {[]string{"preflight/02/task.json", "preflight/02/final.txt"}, CritPreflight, "preflight_missing"},
		"preflight pair differs":     {nil, CritPreflight, "preflight_selection_mismatch"},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			c := filepath.Join(t.TempDir(), "b")
			if err := copyTree(b, c); err != nil {
				t.Fatal(err)
			}
			for _, p := range tc.rm {
				removeBundleFile(t, c, p)
			}
			if tc.rm == nil {
				editTask(t, c, "preflight/01", func(v *contract.TaskView) { v.Effective.Model += "-other" })
			}
			bl, err := LoadBundle(c)
			if err != nil {
				t.Fatal(err)
			}
			for _, cr := range Check(bl) {
				if cr.ID == tc.crit && (cr.Result != "fail" || !slices.Contains(cr.Codes, tc.want)) {
					t.Fatalf("%s = %+v, want fail with %s", tc.crit, cr, tc.want)
				}
			}
		})
	}
}

// stopBeforeWaitCases (FP-8, UT-7, A3.8 review C1): a hop admitted and
// observed is not a wait started. The designer is dispatched and
// observed, then a denied Bash command ends phase 1 before its wait
// starts (the second owner-run's shape); the cleanup then cancels the
// running designer task. The wait, the hop's outcome, its result commit
// and tree were never produced: not_reached with the denial as the cause,
// in the report and the audit, while the dispatch and the denial itself
// are judged. A wait that started and whose task ended before the stop
// must still have its evidence.
func stopBeforeWaitCases(t *testing.T) {
	t.Parallel()
	f := newFakeWorld(t)
	f.coord.deniedBeforeWait = true
	if code := f.attempt(); code != 1 || f.sup.failCode != CodeToolDenied {
		t.Fatalf("attempt = %d %q", code, f.sup.failCode)
	}
	r := f.report()
	for _, id := range []string{CritWaits, CritHops, CritChain, CritArtifacts, CritSetup, CritOwner, CritGrok, CritFinalTests} {
		if c := criterion(r, id); c.Result != ResultCriterionNotReached || len(c.Codes) != 0 || c.Cause != CodeToolDenied {
			t.Errorf("%s %+v, want not_reached by %s", id, c, CodeToolDenied)
		}
	}
	// The denial and what the stream shows of the phase's own early end
	// are judged; the entries of steps never reached are not missing.
	if c := criterion(r, CritEventMap); c.Result != "fail" || !slices.Contains(c.Codes, CodeToolDenied) || slices.Contains(c.Codes, "event_map_incomplete") ||
		slices.Contains(c.Codes, "dispatch_not_observed") {
		t.Errorf("event_map %+v, want the denial and no unreached entry", c)
	}
	for _, id := range []string{CritRoles, CritPreflight, CritCleanup, CritDeadlines} {
		if c := criterion(r, id); c.Result != "pass" {
			t.Errorf("%s %+v, want pass", id, c)
		}
	}
	var a Audit
	readDoc(t, filepath.Join(f.bundle(), "audit.json"), &a)
	if row := rowByID(t, a, "hop01_dispatch"); row.Verdict != VerdictCorrect {
		t.Errorf("dispatch row %+v", row)
	}
	for _, id := range []string{"hop01_wait", "hop01_result"} {
		if row := rowByID(t, a, id); row.Verdict != VerdictUnobserved || !strings.HasPrefix(row.Correction, "not reached: "+CodeToolDenied) ||
			strings.Contains(row.Observed, "missing:") {
			t.Errorf("%s row %+v, want unobserved, not reached", id, row)
		}
	}
	// The wait started and the designer's task ended before the stop: its
	// wait files are due, and removing them fails.
	c := passingBundle(t)
	editEvents(t, c, true, func(evs []Event) []Event {
		i := slices.IndexFunc(evs, func(e Event) bool { return e.Type == EvTaskTerminal && e.Hop == HopDesigner })
		return slices.Insert(evs, i+1, Event{Type: EvAttemptFailed, Time: evs[i].Time, Code: "hop_failed"})
	})
	removeBundleFile(t, c, "hops/01/"+waitResultFile)
	removeBundleFile(t, c, "hops/01/"+waitExitFile)
	bl, err := LoadBundle(c)
	if err != nil {
		t.Fatal(err)
	}
	for _, cr := range Check(bl) {
		if cr.ID == CritWaits && (cr.Result != "fail" || !slices.Contains(cr.Codes, "wait_files_missing")) {
			t.Fatalf("waits = %+v, want wait_files_missing", cr)
		}
	}
	// A coder admitted after the receipt that the stop kept from being
	// observed is no premature coding; observed before the receipt, it is.
	for name, early := range map[string]bool{"unobserved at the stop": false, "observed before the receipt": true} {
		b := passingBundle(t)
		editEvents(t, b, true, func(evs []Event) []Event {
			var out []Event
			for _, e := range evs {
				switch {
				case e.Type == EvObserved && e.Hop == HopCoder:
					continue
				case e.Type == EvReceiptCreated && early:
					out = append(out, Event{Type: EvObserved, Hop: HopCoder, Time: e.Time}, e)
					continue
				}
				out = append(out, e)
				if e.Type == EvTaskAdmitted && e.Hop == HopCoder {
					out = append(out, Event{Type: EvAttemptFailed, Time: e.Time, Code: CodeToolDenied})
				}
			}
			return out
		})
		bl, err := LoadBundle(b)
		if err != nil {
			t.Fatal(err)
		}
		if got := newChecker(bl).methodCodes((*checker).owner)[CritOwner]; slices.Contains(got, "coding_before_approval") != early {
			t.Errorf("%s: owner %v", name, got)
		}
	}
}

// earlyStopCases (FP-8, UT-7, A3.6): cleanup accounts for exactly the
// processes that started. The first-run shape (a first-call denial, then
// phase 1's result and exit 0) has one coordinator group and eight
// verified instances, no invented phase 2, a passing cleanup and a failing
// run; a missing, duplicated or conflicting started-instance record or
// observation fails, and so does a phase whose start failed or whose
// launch evidence could not be written only where evidence is missing.
func earlyStopCases(t *testing.T) {
	t.Parallel()
	f := newFakeWorld(t)
	f.coord.firstDenied = true
	if code := f.attempt(); code != 1 {
		t.Fatalf("attempt = %d %q", code, f.sup.failCode)
	}
	b := f.bundle()
	var cl CleanupRecord
	readDoc(t, filepath.Join(b, "cleanup.json"), &cl)
	names := map[string]int{}
	for _, s := range cl.Started {
		names[s.Name]++
		i := slices.IndexFunc(cl.Processes, func(p ProcRecord) bool { return p.StartedSeq == s.StartedSeq })
		if s.PID <= 0 || i < 0 || cl.Processes[i].Name != s.Name || cl.Processes[i].PID != s.PID || !cl.Processes[i].ExitVerified {
			t.Fatalf("started %+v has no verified stop in %+v", s, cl.Processes)
		}
	}
	if len(cl.Started) != 8 || len(cl.Processes) != 8 || names[phaseHandle(1)] != 1 || names[phaseHandle(2)] != 0 || names["plane"] != 1 || !cl.Complete {
		t.Fatalf("started %v (%d), cleanup %+v", names, len(cl.Started), cl)
	}
	for name, n := range names {
		if name != "plane" && name != phaseHandle(1) && n != 2 {
			t.Errorf("sidecar %s has %d generations", name, n)
		}
	}
	var ce CoordinatorExit
	readDoc(t, filepath.Join(b, filepath.FromSlash(coordinatorExitFile)), &ce)
	if len(ce.Phases) != 1 || ce.Phases[0].Phase != 1 || !ce.Phases[0].Reaped || !ce.Phases[0].GroupGone || ce.Phases[0].ExitCode == nil ||
		*ce.Phases[0].ExitCode != 0 || len(f.launcher.phaseSpecs()) != 1 {
		t.Fatalf("coordinator exit %+v, %d launches", ce.Phases, len(f.launcher.phaseSpecs()))
	}
	// No stale PID: the reaped leader was never signalled afterwards.
	if p := f.coord.procs[0]; len(p.signals) != 0 {
		t.Fatalf("the reaped phase-1 leader was signalled: %v", p.signals)
	}
	// A3.8: phase 2 was never launched because the attempt stopped first:
	// setup is not reached (never launch_missing or
	// coordinator_exit_unobserved), the denial itself still fails.
	r := f.report()
	if su := criterion(r, CritSetup); r.Result != ResultFail || criterion(r, CritCleanup).Result != "pass" || su.Result != ResultCriterionNotReached ||
		len(su.Codes) != 0 || su.Cause != f.sup.failCode || !slices.Contains(criterion(r, CritEventMap).Codes, CodeToolDenied) {
		t.Fatalf("report %v, setup %+v", failedCodes(r), su)
	}
	// Mutations of the stored evidence, each on its own copy.
	for name, tc := range map[string]struct {
		edit       func(t *testing.T, b string)
		crit, want string
	}{
		"phase 1 exit observation removed": {func(t *testing.T, b string) {
			editDoc(t, b, coordinatorExitFile, func(c *CoordinatorExit) { c.Phases = []PhaseProcess{} })
		}, CritSetup, "coordinator_exit_unobserved"},
		"observation duplicated": {func(t *testing.T, b string) {
			editDoc(t, b, coordinatorExitFile, func(c *CoordinatorExit) { c.Phases = append(c.Phases, c.Phases[0]) })
		}, CritCleanup, "coordinator_group_unverified"},
		"invented phase 2": {func(t *testing.T, b string) {
			editDoc(t, b, coordinatorExitFile, func(c *CoordinatorExit) { p := c.Phases[0]; p.Phase = 2; c.Phases = append(c.Phases, p) })
		}, CritCleanup, "coordinator_group_unverified"},
		"group alive": {func(t *testing.T, b string) {
			editDoc(t, b, coordinatorExitFile, func(c *CoordinatorExit) { c.Phases[0].GroupGone = false })
		}, CritCleanup, "coordinator_group_unverified"},
		"observed PID differs": {func(t *testing.T, b string) {
			editDoc(t, b, coordinatorExitFile, func(c *CoordinatorExit) { c.Phases[0].PID++ })
		}, CritCleanup, "coordinator_group_unverified"},
		"sidecar generation stop removed": {func(t *testing.T, b string) {
			editDoc(t, b, "cleanup.json", func(c *CleanupRecord) {
				i := slices.IndexFunc(c.Processes, func(p ProcRecord) bool { return p.Name != "plane" && p.Name != phaseHandle(1) })
				c.Processes = slices.Delete(c.Processes, i, i+1)
			})
		}, CritCleanup, "process_record_missing"},
		"phase stop removed": {func(t *testing.T, b string) {
			editDoc(t, b, "cleanup.json", func(c *CleanupRecord) {
				c.Processes = slices.DeleteFunc(c.Processes, func(p ProcRecord) bool { return p.Name == phaseHandle(1) })
			})
		}, CritCleanup, "process_record_missing"},
		"stop duplicated": {func(t *testing.T, b string) {
			editDoc(t, b, "cleanup.json", func(c *CleanupRecord) { c.Processes = append(c.Processes, c.Processes[0]) })
		}, CritCleanup, "process_record_conflict"},
		"stop PID conflicts": {func(t *testing.T, b string) {
			editDoc(t, b, "cleanup.json", func(c *CleanupRecord) { c.Processes[0].PID++ })
		}, CritCleanup, "process_record_conflict"},
		"started instance removed": {func(t *testing.T, b string) {
			editDoc(t, b, "cleanup.json", func(c *CleanupRecord) { c.Started = c.Started[1:] })
		}, CritCleanup, "process_record_conflict"},
		"started instance duplicated": {func(t *testing.T, b string) {
			editDoc(t, b, "cleanup.json", func(c *CleanupRecord) { c.Started = append(c.Started, c.Started[0]) })
		}, CritCleanup, "process_record_conflict"},
		"start event removed": {func(t *testing.T, b string) {
			editEvents(t, b, false, func(e []Event) []Event {
				return slices.DeleteFunc(e, func(x Event) bool { return x.Type == EvProcessStarted && x.Handle == "plane" })
			})
		}, CritCleanup, "process_record_conflict"},
		"start event without PID": {func(t *testing.T, b string) {
			editEvents(t, b, false, func(e []Event) []Event {
				i := slices.IndexFunc(e, func(x Event) bool { return x.Type == EvPhaseStarted })
				e[i].PID = 0
				return e
			})
		}, CritCleanup, "process_record_conflict"},
		"unverified stop": {func(t *testing.T, b string) {
			editDoc(t, b, "cleanup.json", func(c *CleanupRecord) { c.Processes[1].ExitVerified = false })
		}, CritCleanup, "process_exit_unverified"},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			c := filepath.Join(t.TempDir(), "b")
			if err := copyTree(b, c); err != nil {
				t.Fatal(err)
			}
			tc.edit(t, c)
			bl, err := LoadBundle(c)
			if err != nil {
				t.Fatal(err)
			}
			ck := newChecker(bl)
			method := (*checker).cleanup
			if tc.crit == CritSetup {
				method = (*checker).setup
			}
			if got := ck.methodCodes(method)[tc.crit]; !slices.Contains(got, tc.want) {
				t.Fatalf("%s = %v", tc.crit, got)
			}
			if got := ck.methodCodes((*checker).cleanup)[CritCleanup]; name == "phase 1 exit observation removed" && !slices.Contains(got, "coordinator_group_unverified") {
				t.Fatalf("cleanup = %v", got)
			}
		})
	}
	// A failed phase-1 Start invents no started instance: no phase,
	// no observation, cleanup still passes.
	t.Run("start failure", func(t *testing.T) {
		t.Parallel()
		f := newFakeWorld(t)
		f.override = func(s ProcSpec) (behavior, bool) {
			if s.Path == fakeClaude && len(s.Args) > 0 && s.Args[0] == "-p" {
				return behavior{startErr: errors.New("exec format error")}, true
			}
			return behavior{}, false
		}
		if code := f.attempt(); code != 1 {
			t.Fatalf("attempt = %d", code)
		}
		var cl CleanupRecord
		readDoc(t, filepath.Join(f.bundle(), "cleanup.json"), &cl)
		var ce CoordinatorExit
		readDoc(t, filepath.Join(f.bundle(), filepath.FromSlash(coordinatorExitFile)), &ce)
		if slices.ContainsFunc(cl.Started, func(s StartedProc) bool { return s.Name == phaseHandle(1) }) || len(ce.Phases) != 0 || len(cl.Started) != len(cl.Processes) {
			t.Fatalf("started %+v phases %+v", cl.Started, ce.Phases)
		}
		// A3.8: the failed start is the stopping cause; phase 1's launch
		// is not reached rather than missing.
		r := f.report()
		if su := criterion(r, CritSetup); r.Result != ResultFail || criterion(r, CritCleanup).Result != "pass" || su.Result != ResultCriterionNotReached ||
			su.Cause != f.sup.failCode || f.sup.failCode == "" {
			t.Fatalf("report %v, setup %+v", failedCodes(r), su)
		}
	})
	// Launch evidence that cannot be written after a successful Start:
	// the started group is still stopped, observed and accounted for.
	t.Run("launch write failure", func(t *testing.T) {
		t.Parallel()
		f := newFakeWorld(t)
		f.override = func(s ProcSpec) (behavior, bool) {
			if s.Path == fakeClaude && len(s.Args) > 0 && s.Args[0] == "-p" {
				os.MkdirAll(filepath.Join(f.evidence, filepath.FromSlash(launchFiles[0]), "blocked"), 0o700)
			}
			return behavior{}, false
		}
		if code := f.attempt(); code == 0 {
			t.Fatal("PASS without launch evidence")
		}
		var cl CleanupRecord
		readDoc(t, filepath.Join(f.evidence, "cleanup.json"), &cl)
		i := slices.IndexFunc(cl.Started, func(s StartedProc) bool { return s.Name == phaseHandle(1) })
		if i < 0 || !slices.ContainsFunc(cl.Processes, func(p ProcRecord) bool { return p.StartedSeq == cl.Started[i].StartedSeq && p.ExitVerified }) {
			t.Fatalf("the started phase is not accounted for: %+v %+v", cl.Started, cl.Processes)
		}
		var ce CoordinatorExit
		readDoc(t, filepath.Join(f.evidence, filepath.FromSlash(coordinatorExitFile)), &ce)
		if len(ce.Phases) != 1 || ce.Phases[0].StartedSeq != cl.Started[i].StartedSeq || !ce.Phases[0].GroupGone {
			t.Fatalf("observation %+v", ce.Phases)
		}
	})
}
