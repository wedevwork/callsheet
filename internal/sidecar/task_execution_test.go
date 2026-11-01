package sidecar

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/wedevwork/callsheet/internal/adapter"
	"github.com/wedevwork/callsheet/internal/contract"
	"github.com/wedevwork/callsheet/internal/testkit"
	"github.com/wedevwork/callsheet/internal/testkit/fakeadapter"
)

// The real fake-adapter binary for the /process qualification, built at
// most once per test process.
var (
	fakeOnce sync.Once
	fakePath string
	fakeErr  error
	fakeDir  string
)

func fakeAdapterBinary(t testing.TB) string {
	t.Helper()
	fakeOnce.Do(func() {
		fakeDir, fakeErr = os.MkdirTemp("", "callsheet-sidecar-fake-")
		if fakeErr == nil {
			fakePath, fakeErr = testkit.BuildBinaryAt(fakeDir, "./cmd/fake-adapter", "fake-adapter")
		}
	})
	if fakeErr != nil {
		t.Fatalf("building the fake adapter: %v", fakeErr)
	}
	return fakePath
}

// useProcessProbe installs the /process qualification's ready-probe
// adapter in d and returns it: the product fake (the same prober, deadline
// and outcome as NewFake's) probing in its own directory, counted, and
// tracing its probe's instants for a failure's diagnostics. Its 1 s
// deadline runs on d's injected clock, as the cycle budget does, so the
// real child's answer is judged on the test's clock, never on how fast a
// loaded host ran it.
func useProcessProbe(t *testing.T, d *deps, trace *adapter.ProbeTrace) *countingAdapter {
	t.Helper()
	ca := &countingAdapter{Adapter: adapter.NewFakeTracedClock(t.TempDir(), trace, d.clock)}
	reg, err := adapter.NewRegistry(ca)
	if err != nil {
		t.Fatal(err)
	}
	d.adapters = func(string) adapter.Registry { return reg }
	return ca
}

// countingAdapter is the product fake whose probe executions are counted
// (the probe executor's side of the launch ledger).
type countingAdapter struct {
	adapter.Adapter
	mu     sync.Mutex
	probes int
	// last is the latest probe's returned error (diagnostics).
	last error
}

func (a *countingAdapter) Probe(ctx context.Context, exe string) error {
	a.mu.Lock()
	a.probes++
	a.mu.Unlock()
	err := a.Adapter.Probe(ctx, exe)
	a.mu.Lock()
	a.last = err
	a.mu.Unlock()
	return err
}

// lastErr returns the latest probe's returned error.
func (a *countingAdapter) lastErr() error {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.last
}

func (a *countingAdapter) count() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.probes
}

// refusal requires a start refusal with reason.
func wantRefusal(t *testing.T, r contract.TaskStartResult, reason string) {
	t.Helper()
	if r.Err == nil || r.Err.Details["reason"] != reason {
		t.Fatalf("start = %+v, want refusal %s", r.Err, reason)
	}
}

// TestTaskExecutionContract is UT FP-4/5 for execution on the worker;
// its compose, exit and process subtests are delegated from
// tests/function (TestTaskExecution), and process is also required native
// evidence. Only process starts OS children (exactly one probe and two
// guardian/adapter pairs since iteration 06a). Do not rename or skip
// them.
func TestTaskExecutionContract(t *testing.T) {
	t.Parallel()
	t.Run("compose", func(t *testing.T) {
		t.Parallel()
		// The exact stdin envelope, from manuals read completely at start
		// (content changed after registration is used); bytes preserved and
		// quoted unambiguously; every prelaunch refusal is definitive.
		fp := startFakePlane(t)
		tr := startTaskRun(t, fp, taskOpts{})
		ins, run := manuals(t, tr.dir, "a", "old")
		cfg := roleConfig("a", ins, run)
		s := tr.connect(t, 1, 1, cfg)
		instr := "line one\n\t\"quoted\" </script> {\"json\":true} \\ é\n"
		os.WriteFile(ins, []byte(instr), 0o644)
		os.WriteFile(run, []byte(""), 0o644)
		st, ch := s.run(t, 1, 0, "goal \"x\"\nsecond line")
		got := ch.prompt(t)
		want, err := composePrompt([]byte(instr), nil, st, contract.MaxPromptBytes)
		if err != nil || !bytes.Equal(got, want) {
			t.Fatalf("stdin %q, want %q (%v)", got, want, err)
		}
		exact := `{"format":"callsheet-task-v1","instruction":` + mustJSON(t, instr) + `,"runbook":"","task":{"task_id":"` + st.TaskID +
			`","target":{"kind":"id","value":"a"},"role":{"id":"a","name":"coder"},"goal":"goal \"x\"\nsecond line","payload":["p://1"],"acceptance":"done",` +
			`"effective":{"model":"example model","effort":"medium","timeout":"2h0m0s"},"timeout_enforced":false,"requested_by":{"name":"callsheet","version":"dev","hostname":"coord"}}}` + "\n"
		if string(got) != strings.NewReplacer(`\u003c`, "<", `\u003e`, ">", `\u0026`, "&").Replace(exact) {
			t.Fatalf("envelope\n%s\nwant\n%s", got, exact)
		}
		var env map[string]json.RawMessage
		if json.Unmarshal(got, &env) != nil || len(env) != 4 {
			t.Fatal("the envelope is not one JSON object")
		}
		// No prompt file: the scratch directory is empty and the state
		// root unchanged.
		if entries, _ := os.ReadDir(ch.spec.dir); len(entries) != 0 {
			t.Fatalf("scratch holds %v", entries)
		}
		ch.exitCode(0)
		s.result(t, st)
		// Prelaunch refusals: definitive, slot released, no child.
		fifo := filepath.Join(tr.dir, "fifo")
		syscall.Mkfifo(fifo, 0o644)
		big := filepath.Join(tr.dir, "big")
		os.WriteFile(big, bytes.Repeat([]byte("b"), contract.MaxManualBytes+1), 0o644)
		atCap := filepath.Join(tr.dir, "cap")
		os.WriteFile(atCap, bytes.Repeat([]byte("c"), contract.MaxManualBytes), 0o644)
		bad := filepath.Join(tr.dir, "bad")
		os.WriteFile(bad, []byte("a\xffb"), 0o644)
		nul := filepath.Join(tr.dir, "nul")
		os.WriteFile(nul, []byte("a\x00b"), 0o644)
		n := 2
		for _, c := range []struct {
			instruction, reason string
		}{
			{big, contract.ReasonManualTooLarge}, {bad, contract.ReasonManualInvalidText}, {nul, contract.ReasonManualInvalidText},
			{fifo, contract.ReasonManualNotRegular}, {tr.dir, contract.ReasonManualNotRegular}, {filepath.Join(tr.dir, "gone"), contract.ReasonManualUnreadable},
		} {
			os.Rename(ins, ins+".saved")
			if err := os.Symlink(c.instruction, ins); err != nil {
				t.Fatal(err)
			}
			_, r := s.start(t, n, 0, "refused")
			wantRefusal(t, r, c.reason)
			os.Remove(ins)
			os.Rename(ins+".saved", ins)
			n++
		}
		// A manual of exactly 1 MiB is legal.
		os.Rename(ins, ins+".saved")
		os.Symlink(atCap, ins)
		stCap, chCap := s.run(t, n, 0, "at cap")
		if p := chCap.prompt(t); !bytes.Contains(p, bytes.Repeat([]byte("c"), 1024)) {
			t.Fatal("the 1 MiB manual was not delivered")
		}
		chCap.exitCode(0)
		s.result(t, stCap)
		tr.noChild(t)
		// The encoded prompt cap, one case per boundary: exactly at the cap
		// it composes; one byte over fails before launch.
		probe := startBody(99, cfg, 1, 1, "cap")
		base, _ := composePrompt(nil, nil, probe, contract.MaxPromptBytes)
		fill := contract.MaxPromptBytes - len(base)
		if p, err := composePrompt(bytes.Repeat([]byte("a"), fill), nil, probe, contract.MaxPromptBytes); err != nil || len(p) != contract.MaxPromptBytes {
			t.Fatalf("at the cap: %d %v", len(p), err)
		}
		if _, err := composePrompt(bytes.Repeat([]byte("a"), fill+1), nil, probe, contract.MaxPromptBytes); !errors.Is(err, errPromptTooLarge) {
			t.Fatalf("over the cap: %v", err)
		}
		// Manuals alone over the limit are refused before encoding.
		if _, err := composePrompt([]byte("12345"), []byte("67890"), probe, 9); !errors.Is(err, errPromptTooLarge) {
			t.Fatalf("raw manuals over the limit: %v", err)
		}
		if hb := s.report(t, 1, func(r contract.RoleStatus) bool { return r.Inflight == 0 }); hb.Roles[0].Concurrency != 2 {
			t.Fatalf("refusals kept slots: %+v", hb.Roles)
		}
	})
	t.Run("environment", func(t *testing.T) {
		t.Parallel()
		// The child gets the sidecar's environment minus the fixture
		// descriptors, PWD set to its fresh journal-owned work directory
		// (iteration 10b: tasks/<task_id>/work), argv with explicit model and
		// effort, and the enabled executable; adapter, executable, work
		// directory and Start failures are refusals.
		fp := startFakePlane(t)
		var failWork atomic.Bool
		tr := startTaskRun(t, fp, taskOpts{adjust: func(d *deps) {
			d.fail = func(op, name string) error {
				if op == "mkdir" && failWork.Load() && filepath.Base(name) == workName {
					return errors.New("injected work directory failure")
				}
				return nil
			}
		}})
		ins, run := manuals(t, tr.dir, "a", "m")
		s := tr.connect(t, 1, 1, roleConfig("a", ins, run))
		st, ch := s.run(t, 1, 0, "env")
		want := []string{"PATH=" + os.Getenv("PATH"), "KEEP=1", "PWD=" + ch.spec.dir}
		if strings.Join(ch.spec.env, "|") != strings.Join(want, "|") {
			t.Fatalf("env %q", ch.spec.env)
		}
		if ch.spec.argv[0] != adapter.TaskArg || strings.Join(ch.spec.argv[1:], " ") != "--model example model --effort medium" || !filepath.IsAbs(ch.spec.path) {
			t.Fatalf("argv %q path %s", ch.spec.argv, ch.spec.path)
		}
		fi, err := os.Stat(ch.spec.dir)
		if err != nil || fi.Mode().Perm() != 0o700 || ch.spec.dir != workDir(t, runtime.GOOS, tr.root, st.TaskID) {
			t.Fatalf("scratch %s %v", ch.spec.dir, err)
		}
		ch.prompt(t)
		ch.exitCode(0)
		s.result(t, st)
		if _, err := os.Stat(ch.spec.dir); !os.IsNotExist(err) {
			t.Fatal("the work directory was not removed")
		}
		tr.ledger.mu.Lock()
		tr.ledger.startErr = errUnsupported
		tr.ledger.mu.Unlock()
		st2, r := s.start(t, 2, 0, "start fails")
		wantRefusal(t, r, contract.ReasonStartFailed)
		if _, err := os.Stat(filepath.Join(tr.root, journalDir, st2.TaskID)); !os.IsNotExist(err) {
			t.Fatalf("a failed start left its task directory: %v", err)
		}
		tr.ledger.mu.Lock()
		tr.ledger.startErr = nil
		tr.ledger.mu.Unlock()
		failWork.Store(true)
		st3, r := s.start(t, 3, 0, "no scratch")
		wantRefusal(t, r, contract.ReasonScratchUnavailable)
		failWork.Store(false)
		if _, err := os.Stat(filepath.Join(tr.root, journalDir, st3.TaskID)); !os.IsNotExist(err) {
			t.Fatalf("a refused start left its task directory: %v", err)
		}
		tr.noChild(t)
		// A disabled adapter and a vanished executable.
		fp2 := startFakePlane(t)
		tr2 := startTaskRun(t, fp2, taskOpts{exe: filepath.Join(t.TempDir(), "gone")})
		s2 := tr2.connect(t, 1, 1, roleConfig("a", ins, run))
		_, r = s2.start(t, 1, 0, "no exe")
		wantRefusal(t, r, contract.ReasonExecutableUnavailable)
		// Unsupported OS at the seam.
		fp3 := startFakePlane(t)
		tr3 := startTaskRun(t, fp3, taskOpts{goos: "windows"})
		s3 := tr3.connect(t, 1, 1, roleConfig("a", ins, run))
		_, r = s3.start(t, 1, 0, "windows")
		wantRefusal(t, r, contract.ReasonStartFailed)
		tr3.noChild(t)
	})
	t.Run("symlinked-state-root", func(t *testing.T) {
		t.Parallel()
		// A state root reached through a symlink, as Darwin's /var is one to
		// /private/var: the work directory is created below it, named as the
		// platform seam names it (physical on darwin).
		for _, goos := range []string{"linux", "darwin"} {
			t.Run(goos, func(t *testing.T) {
				t.Parallel()
				// The state root itself is a real directory; an ancestor is the
				// symlink.
				link := filepath.Join(t.TempDir(), "state-link")
				if err := os.Symlink(t.TempDir(), link); err != nil {
					t.Fatal(err)
				}
				root := filepath.Join(link, "sidecar")
				fp := startFakePlane(t)
				writeState(t, root, testID, fp.url, fp.caPEM)
				tr := startTaskRun(t, fp, taskOpts{goos: goos, root: root})
				ins, run := manuals(t, tr.dir, "a", "m")
				s := tr.connect(t, 1, 1, roleConfig("a", ins, run))
				st, ch := s.run(t, 1, 0, "symlinked root")
				fi, err := os.Stat(ch.spec.dir)
				if err != nil || fi.Mode().Perm() != 0o700 || ch.spec.dir != workDir(t, goos, root, st.TaskID) {
					t.Fatalf("scratch %s %v", ch.spec.dir, err)
				}
				ch.prompt(t)
				ch.exitCode(0)
				s.result(t, st)
			})
		}
	})
	t.Run("exit", func(t *testing.T) {
		t.Parallel()
		// The direct child's exit decides the result, without retry and
		// without the sidecar exiting: 0, a nonzero code, a signal (null
		// code), an unrecognized wait (SIGUNKNOWN); a successful child is
		// followed by a fresh one; an unconfirmed group cleanup still
		// reports the exit but keeps the slot and the role locally
		// nonaccepting.
		fp := startFakePlane(t)
		tr := startTaskRun(t, fp, taskOpts{})
		ins, run := manuals(t, tr.dir, "a", "m")
		s := tr.connect(t, 1, 1, roleConfig("a", ins, run))
		// The first ready cycle has passed before the clock moves, so the
		// readiness asserted after the exits is fresh.
		if p := tr.ev.awaitMatch(t, evCycleDone, nil).passed; !p[0] {
			t.Fatal("the ready cycle failed")
		}
		var dirs []string
		var st0 contract.TaskStartBody // the first, a proven cleanup
		for i, c := range []struct {
			exit procExit
			code *int
			sig  string
		}{
			{procExit{code: 0}, intp(0), ""}, {procExit{code: 7}, intp(7), ""}, {procExit{signal: "SIGKILL"}, nil, "SIGKILL"},
			{procExit{err: errors.New("wait failed")}, nil, contract.SignalUnknown}, {procExit{code: 0}, intp(0), ""},
		} {
			st, ch := s.run(t, i+1, 0, "exit")
			if i == 0 {
				st0 = st
			}
			dirs = append(dirs, ch.spec.dir)
			ch.prompt(t)
			ch.finish(c.exit)
			r := s.result(t, st)
			if (r.ExitCode == nil) != (c.code == nil) || (r.ExitCode != nil && *r.ExitCode != *c.code) || (c.sig != "") != (r.Signal != nil) || (r.Signal != nil && *r.Signal != c.sig) {
				t.Fatalf("case %d result %+v", i, r)
			}
			tr.noChild(t)
		}
		for i := 1; i < len(dirs); i++ {
			if dirs[i] == dirs[i-1] {
				t.Fatal("a scratch directory was reused")
			}
		}
		// Reported at once (iteration 10a): every slot free, ready.
		s.report(t, 1, func(r contract.RoleStatus) bool { return r.Inflight == 0 && r.CanAccept })
		// A child that removes its scratch directory: the cleanup warning
		// does not rewrite the successful exit.
		st, ch := s.run(t, 10, 0, "messy")
		ch.prompt(t)
		os.RemoveAll(ch.spec.dir)
		os.WriteFile(ch.spec.dir, []byte("not a dir"), 0o600)
		ch.exitCode(0)
		if r := s.result(t, st); *r.ExitCode != 0 {
			t.Fatalf("messy %+v", r)
		}
		os.Remove(ch.spec.dir)
		// Unconfirmed group cleanup.
		tr.groups.mu.Lock()
		tr.groups.err = errors.New("group still present")
		tr.groups.mu.Unlock()
		st, ch = s.run(t, 11, 0, "stuck group")
		ch.prompt(t)
		ch.exitCode(0)
		if r := s.result(t, st); *r.ExitCode != 0 {
			t.Fatalf("unconfirmed cleanup result %+v", r)
		}
		// Readiness is still fresh (the clock has not moved since the
		// passed cycle), so only the unconfirmed cleanup makes it false,
		// reported at once.
		if hb := s.report(t, 1, func(r contract.RoleStatus) bool { return !r.CanAccept }); hb.Roles[0].Inflight != 1 {
			t.Fatalf("after unconfirmed cleanup %+v", hb.Roles)
		}
		if _, err := os.Stat(ch.spec.dir); err != nil {
			t.Fatal("the scratch of an unconfirmed cleanup was removed")
		}
		// Iteration 10a's diagnostics: a proven absence reports its cleanup
		// time from the exit receipt (0 on the unmoved injected clock); an
		// unconfirmed one never reports an absence.
		logs := tr.logs.String()
		if gone, ok := logRecord(logs, "task group gone", st0.TaskID); !ok || gone["cleanup_elapsed_ns"] != float64(0) {
			t.Fatalf("proven absence diagnostic %v", gone)
		}
		if _, ok := logRecord(logs, "task adapter exit observed", st.TaskID); !ok {
			t.Fatal("no exit diagnostic for the stuck group")
		}
		if rec, ok := logRecord(logs, "task group gone", st.TaskID); ok {
			t.Fatalf("an unconfirmed cleanup reported an absence: %v", rec)
		}
		_, r := s.start(t, 12, 0, "blocked")
		wantRefusal(t, r, contract.ReasonLocalFull)
		tr.logs.await(t, "cleanup_unconfirmed", func(s string) bool { return strings.Contains(s, "cleanup_unconfirmed") })
	})
	t.Run("process", func(t *testing.T) {
		t.Parallel()
		// The sole real-process qualification: one explicit ready probe of
		// the built fixture, then two sequential guardian-backed tasks
		// (success, then exit 7) proving actual stdin/stdout/stderr, the
		// guardian's PGID equal to its PID and not the supervisor's, the
		// adapter a distinct member of that group, cwd and environment,
		// private scratch permissions, the guardian's whole-group cleanup,
		// the journal's removal after the committed acknowledgement and
		// continued heartbeats. Launch ledger: exactly 1 probe + 2
		// guardians + 2 adapters.
		bin := fakeAdapterBinary(t)
		cli := cliBinary(t)
		trace := &adapter.ProbeTrace{}
		var ca *countingAdapter
		fp := startFakePlane(t)
		type seen struct{ guardian, gpgid, adapter, apgid int }
		var mu sync.Mutex
		var starts []seen
		tr := startTaskRun(t, fp, taskOpts{exe: bin,
			adjust: func(d *deps) {
				ca = useProcessProbe(t, d, trace)
				d.noCadence = true
				d.taskGroups = nil
				d.guardianExe = func() (string, error) { return cli, nil }
			}})
		tr.ledger.mu.Lock()
		tr.ledger.real = true
		tr.ledger.onStart = func(guardian, adapter int) {
			// The adapter blocks reading stdin until the supervisor's
			// copier runs, after its started message: it and its guardian
			// (the group anchor) are alive here. Getpgid works on Linux and
			// Darwin alike.
			gpgid, _ := syscall.Getpgid(guardian)
			apgid, _ := syscall.Getpgid(adapter)
			mu.Lock()
			starts = append(starts, seen{guardian: guardian, gpgid: gpgid, adapter: adapter, apgid: apgid})
			mu.Unlock()
		}
		tr.ledger.mu.Unlock()
		home := t.TempDir()
		// The sidecar's environment: the child keeps unrelated keys (a
		// future adapter's credential), gets its scratch as PWD and never
		// the fixture's descriptor variables.
		tr.setEnv([]string{"PATH=" + os.Getenv("PATH"), "HOME=" + home, "PWD=/elsewhere", "CALLSHEET_FAKE_READY_FD=9", "KEEP=credential",
			fakeadapter.EnvTaskReport + "=1"})
		ins, run := manuals(t, tr.dir, "a", "instruction body")
		s := tr.connect(t, 1, 1, roleConfig("a", ins, run))
		if p := tr.ev.awaitMatch(t, evCycleDone, nil).passed; !p[0] {
			// Diagnostics (rca: the deadline or a late post-Wait sample):
			// the probe's error, the deadline start to cmd.Start, and the
			// child's state at the deadline check.
			t.Fatalf("the real fake adapter did not pass its ready probe: probes %d, probe error: %v; %v", ca.count(), ca.lastErr(), trace.Last())
		}
		// The passed cycle is reported at once (iteration 10a).
		s.report(t, 1, func(r contract.RoleStatus) bool { return r.CanAccept })
		// Child one: success; its final marker on stdout and the fixture's
		// report of what it actually got (cwd, its permissions, the
		// environment) on stderr.
		st := startBody(1, s.cfgs[0], 1, 1, "real one")
		s.c.sendStart(s.nextP(), st)
		if r := s.c.startResult("p2"); r.Err != nil {
			t.Fatalf("real start: %+v", r)
		}
		r, out := s.drain(t, st)
		if *r.ExitCode != 0 || r.FinalMessage == nil || *r.FinalMessage != "fake task completed" || r.OutputBytes != len(out) {
			t.Fatalf("child one: %+v %q", r, out)
		}
		var rep fakeadapter.TaskReport
		var marker bool
		for _, line := range strings.SplitAfter(string(out), "\n") {
			switch {
			case line == fakeadapter.FinalMarker:
				marker = true
			case strings.HasPrefix(line, `{"type":"callsheet_fake_report"`):
				if err := json.Unmarshal([]byte(line), &rep); err != nil {
					t.Fatalf("report %q: %v", line, err)
				}
			case line != "":
				t.Fatalf("unexpected child output %q", line)
			}
		}
		if !marker || rep.CWD == "" {
			t.Fatalf("child one output %q", out)
		}
		// The committed result lets the janitor delete tasks/<task_id> at any
		// moment: resolve the persistent tasks/ directory and require the
		// exact <task_id>/work components below it.
		want, err := filepath.EvalSymlinks(filepath.Join(tr.root, journalDir))
		if err != nil {
			t.Fatal(err)
		}
		dir, _ := filepath.EvalSymlinks(filepath.Dir(filepath.Dir(rep.CWD)))
		if dir != want || filepath.Base(filepath.Dir(rep.CWD)) != st.TaskID || filepath.Base(rep.CWD) != workName || rep.CWDMode != "0700" {
			t.Fatalf("child cwd %s (mode %s), want the private work directory %s", rep.CWD, rep.CWDMode, filepath.Join(want, st.TaskID, workName))
		}
		wantEnv := []string{fakeadapter.EnvTaskReport + "=1", "HOME=" + home, "KEEP=credential", "PATH=" + os.Getenv("PATH"), "PWD=" + rep.CWD}
		if !slices.Equal(rep.Env, wantEnv) {
			t.Fatalf("child environment %q, want %q", rep.Env, wantEnv)
		}
		// Child two: exit 7 with its stderr line.
		tr.setEnv([]string{"PATH=" + os.Getenv("PATH"), "CALLSHEET_FAKE_TASK_MODE=fail"})
		st2 := startBody(2, s.cfgs[0], 1, 1, "real two")
		s.c.sendStart(s.nextP(), st2)
		if r := s.c.startResult("p3"); r.Err != nil {
			t.Fatalf("real start two: %+v", r)
		}
		if out := s.logs(t, st2, len("fake task failure\n")); string(out) != "fake task failure\n" {
			t.Fatalf("stderr %q", out)
		}
		if r := s.result(t, st2); *r.ExitCode != 7 || r.FinalMessage != nil {
			t.Fatalf("child two: %+v", r)
		}
		for _, id := range []string{st.TaskID, st2.TaskID} {
			tr.ev.awaitCollected(t, id)
			if _, err := os.Stat(filepath.Join(tr.root, "tasks", id)); !os.IsNotExist(err) {
				t.Fatalf("the committed task's journal %s remains: %v", id, err)
			}
		}
		// Heartbeats continue after both exits with the slot free (its
		// readiness then ages out: the cadence is disabled so no second
		// probe runs).
		s.report(t, 1, func(r contract.RoleStatus) bool { return r.Inflight == 0 })
		if hb := s.periodic(t, 1); hb.Roles[0].Inflight != 0 {
			t.Fatalf("heartbeat after the children: %+v", hb.Roles)
		}
		mu.Lock()
		defer mu.Unlock()
		if len(starts) != 2 || starts[0].guardian == starts[1].guardian {
			t.Fatalf("children %+v", starts)
		}
		for _, sn := range starts {
			if sn.gpgid != sn.guardian || sn.gpgid == syscall.Getpgrp() || sn.apgid != sn.guardian || sn.adapter == sn.guardian {
				t.Fatalf("guardian %d in group %d, adapter %d in group %d (supervisor %d)", sn.guardian, sn.gpgid, sn.adapter, sn.apgid, syscall.Getpgrp())
			}
			if err := syscall.Kill(-sn.gpgid, 0); !errors.Is(err, syscall.ESRCH) {
				t.Fatalf("group %d still present: %v", sn.gpgid, err)
			}
		}
		if entries, _ := os.ReadDir(tr.tmp); len(entries) != 0 {
			t.Fatalf("scratch left: %v", entries)
		}
		if ca.count() != 1 || tr.ledger.starts() != 4 {
			t.Fatalf("launch ledger: %d probes, %d guardian and adapter children; want exactly 1 and 4", ca.count(), tr.ledger.starts())
		}
	})
}

// TestTaskExecutionProbeClock is the regression for the /process
// qualification failing its ready probe when a loaded host runs the real
// child past one second of real time (fix.md, the 1.05 s and 1.12 s
// incidents): that qualification's probe deadline runs on the test's
// injected clock, as its cycle budget does, so a child that is slow in real
// time but answers within the injected deadline passes. Its child, a real
// process, is held at a gate the test controls until the probe's deadline
// is pending on the injected clock and that clock stands one tick before
// it; nothing sleeps.
func TestTaskExecutionProbeClock(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	gate := filepath.Join(dir, "gate")
	if err := syscall.Mkfifo(gate, 0o600); err != nil {
		t.Fatal(err)
	}
	// Opening the gate completes the test's open of its write end (so the
	// test knows the child is running); the child answers once the test
	// closes it.
	exe := filepath.Join(dir, "gated-adapter")
	if err := os.WriteFile(exe, []byte("#!/bin/sh\nread -r x < '"+gate+"'\nprintf '"+strings.TrimSuffix(adapter.ProbeOutput, "\n")+"\\n'\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	opened := make(chan *os.File, 1)
	// Recurrence diagnostic, read only at the cycle arm of the first select
	// (macOS flake, rca.md): when the writer entered and left its open, as
	// offsets from base; 0 means not reached.
	base := time.Now()
	var writerEnter, writerReturn atomic.Int64
	writerErr := make(chan error, 1)
	go func() {
		writerEnter.Store(int64(time.Since(base)))
		w, err := os.OpenFile(gate, os.O_WRONLY, 0)
		writerReturn.Store(int64(time.Since(base)))
		if err != nil {
			writerErr <- err
			w = nil
		}
		opened <- w
	}()
	var held *os.File
	received := false
	t.Cleanup(func() {
		if !received {
			// Free an open still waiting for a child that never came.
			if r, err := os.OpenFile(gate, os.O_RDONLY|syscall.O_NONBLOCK, 0); err == nil {
				defer r.Close()
			}
			held = <-opened
		}
		if held != nil {
			held.Close()
		}
	})
	trace := &adapter.ProbeTrace{}
	var ca *countingAdapter
	fp := startFakePlane(t)
	tr := startTaskRun(t, fp, taskOpts{exe: exe, adjust: func(d *deps) {
		ca = useProcessProbe(t, d, trace)
		d.noCadence = true
	}})
	ins, run := manuals(t, tr.dir, "a", "instruction body")
	tr.connect(t, 1, 1, roleConfig("a", ins, run))
	failed := func(ev event) {
		t.Helper()
		t.Fatalf("the ready probe ended while its child was held at the gate (passed %v): probes %d, probe error: %v; %v", ev.passed, ca.count(), ca.lastErr(), trace.Last())
	}
	// The child is running and held.
	select {
	case held = <-opened:
		received = true
		if held == nil {
			t.Fatal("opening the gate failed")
		}
	case ev := <-tr.ev.cycles:
		// The child has already been reaped: record what the writer did,
		// then fail with the usual sentence. Nothing here blocks.
		writerReady, writerNil, sameFile := false, false, false
		var openErr, gateErr, fdErr error
		select {
		case w := <-opened:
			writerReady, writerNil = true, w == nil
			held, received = w, true
			if w != nil {
				var gi, fi os.FileInfo
				gi, gateErr = os.Stat(gate)
				fi, fdErr = w.Stat()
				sameFile = gateErr == nil && fdErr == nil && os.SameFile(gi, fi)
			}
			select {
			case openErr = <-writerErr:
			default:
			}
		default:
		}
		p := trace.Last()
		t.Logf("writer_ready=%v writer_nil=%v writer_err=%v writer_fd_is_gate=%v gate_stat_err=%v writer_stat_err=%v; offsets from test start: writer_enter=%v writer_return=%v armed=%v started=%v waited=%v",
			writerReady, writerNil, openErr, sameFile, gateErr, fdErr, time.Duration(writerEnter.Load()), time.Duration(writerReturn.Load()),
			p.Armed.Sub(base), p.Started.Sub(base), p.Waited.Sub(base))
		failed(ev)
	case <-time.After(testWait):
		dumpGoroutines(t)
		t.Fatal("the probe child never reached the gate")
	}
	// The probe's deadline is pending on the injected clock. The waiter is
	// cancelled and joined on every path: cleanup closes stopWait, wakes
	// the waiter (a timer armed and stopped at once changes the waiter
	// list) and receives its result.
	pending := make(chan error, 1)
	stopWait := make(chan struct{})
	joined := false
	go func() {
		pending <- tr.clk.AwaitWaiter(testWait, func(ws []testkit.Waiter) bool {
			select {
			case <-stopWait:
				return true
			default:
			}
			d := trace.Last().Deadline
			for _, w := range ws {
				if !w.Ticker && w.Duration == adapter.ProbeTimeout && w.At.Equal(d) {
					return true
				}
			}
			return false
		})
	}()
	t.Cleanup(func() {
		if joined {
			return
		}
		close(stopWait)
		_, stop := tr.clk.NewTimer(time.Hour)
		stop()
		<-pending
	})
	select {
	case err := <-pending:
		joined = true
		if err != nil {
			dumpGoroutines(t)
			t.Fatalf("the probe deadline is not on the injected clock: %v; %v", err, trace.Last())
		}
	case ev := <-tr.ev.cycles:
		failed(ev)
	}
	// One tick before the deadline, still held: the probe has not ended.
	tr.clk.Advance(adapter.ProbeTimeout - time.Nanosecond)
	select {
	case ev := <-tr.ev.cycles:
		failed(ev)
	default:
	}
	held.Close()
	held = nil
	// The released child answers: the real process passed the probe.
	select {
	case ev := <-tr.ev.cycles:
		if !ev.passed[0] {
			t.Fatalf("the released child did not pass its ready probe: probes %d, probe error: %v; %v", ca.count(), ca.lastErr(), trace.Last())
		}
	case <-time.After(testWait):
		dumpGoroutines(t)
		t.Fatal("no cycle-done after the gate opened")
	}
	if ca.count() != 1 {
		t.Fatalf("%d probes, want 1", ca.count())
	}
}

func intp(n int) *int { return &n }

// BenchmarkTaskPrompt composes the prompt of two maximum manuals that
// JSON escapes sixfold, checking the 16 MiB bound and that allocation
// stays proportional to one prompt.
func BenchmarkTaskPrompt(b *testing.B) {
	ins := bytes.Repeat([]byte{0x01}, contract.MaxManualBytes)
	run := bytes.Repeat([]byte("\"\\\n"), contract.MaxManualBytes/3)
	st := startBody(1, roleConfig("a", "/i", "/r"), 1, 1, strings.Repeat("g", contract.MaxGoalBytes))
	b.ReportAllocs()
	for b.Loop() {
		p, err := composePrompt(ins, run, st, contract.MaxPromptBytes)
		if err != nil || len(p) > contract.MaxPromptBytes || len(p) < 6*contract.MaxManualBytes {
			b.Fatalf("prompt %d bytes: %v", len(p), err)
		}
	}
}
