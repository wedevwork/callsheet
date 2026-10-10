//go:build realadaptercheck && (linux || darwin)

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
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/wedevwork/callsheet/internal/adapter"
	"github.com/wedevwork/callsheet/internal/contract"
	"github.com/wedevwork/callsheet/internal/testkit"
)

// Iteration 11's tagged sidecar contract (wave 2: Grok and Cursor), the
// wave2-* subtests of TestRealAdapterLocal (same build tag and delegation;
// never a stress shard). The vendors run with probe-free wrappers and the
// counted injected guardians (zero OS processes). Linux task eligibility is
// exercised on either host by overriding only roleEnv.goos: the supervisor
// keeps its native task platform (taskPlatformFor of the Run's native
// GOOS, Darwin's physical /private/var paths included), so scratch and cwd
// assertions use the native path contract. That separation is test-only;
// production supplies roleEnv.goos from RunOptions.GOOS alone.

// Exact wave-2 posture refusals (adapter.ValidateWorkerPosture).
const (
	wave2GrokPosture   = "grok worker execution is not qualified on this OS; consult the support catalog"
	wave2CursorPosture = "cursor worker execution is refused: no qualified unattended recipe preserves the operator posture; consult the support catalog"
)

// wave2Capture reads one of iteration 11's checked-in captures.
func wave2Capture(t testing.TB, rel string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(testkit.MustRepoRoot(t), "tests", "testdata", "real-adapters", "linux-2026-10-04", filepath.FromSlash(rel)))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// failingProbe is a vendor adapter whose probe fails (precedence tests).
type failingProbe struct {
	adapter.Adapter
	n *atomic.Int32
}

func (f failingProbe) Probe(context.Context, string) error {
	f.n.Add(1)
	return &adapter.ProbeError{Reason: "exited unsuccessfully"}
}

// wave2Registry is every built-in adapter behind a counted, probe-free
// wrapper.
func wave2Registry(n *atomic.Int32) adapter.Registry {
	r, err := adapter.NewRegistry(countingProbe{adapter.NewClaude(""), n}, countingProbe{adapter.NewCodex(""), n}, countingProbe{adapter.NewCursor(""), n},
		countingProbe{adapter.NewFake(""), n}, countingProbe{adapter.NewGrok(""), n})
	if err != nil {
		panic(err)
	}
	return r
}

// roleGOOS overrides only the supervisor's roleEnv.goos (the posture OS of
// task preparation), through the existing supervisor observation seam,
// before any task loop runs; the supervisor's native platform is kept.
func roleGOOS(goos string) func(d *deps) {
	return func(d *deps) {
		prev := d.onSupervisor
		d.onSupervisor = func(s *taskSupervisor) {
			s.env.goos = goos
			if prev != nil {
				prev(s)
			}
		}
	}
}

// startWave2Run is a task run on the native GOOS with grok and cursor (and,
// unless options clear them, claude) enabled through never-executed files,
// probe-free adapters, and roleEnv.goos overridden when posture is set.
func startWave2Run(t *testing.T, fp *fakePlane, posture string, options func(o *RunOptions), adjust func(d *deps)) *vendorRun {
	t.Helper()
	n := &atomic.Int32{}
	exe := fakeExeFile(t)
	tr := startTaskRun(t, fp, taskOpts{adapters: func(string) adapter.Registry { return wave2Registry(n) }, adjust: func(d *deps) {
		if posture != "" {
			roleGOOS(posture)(d)
		}
		if adjust != nil {
			adjust(d)
		}
	}, options: func(o *RunOptions) {
		o.GrokAdapterPath, o.CursorAdapterPath, o.ClaudeAdapterPath = exe, exe, exe
		if options != nil {
			options(o)
		}
	}})
	return &vendorRun{taskRun: tr, probes: n}
}

// warnings counts the JSON log records whose msg is exactly msg.
func warnings(logs, msg string) int {
	n := 0
	for line := range strings.SplitSeq(logs, "\n") {
		var rec map[string]any
		if json.Unmarshal([]byte(line), &rec) == nil && rec["msg"] == msg {
			n++
		}
	}
	return n
}

// interleaved reports whether log interleaves a and b, keeping each
// stream's own byte order (the two pipes drain concurrently).
func interleaved(log, a, b []byte) bool {
	if len(log) != len(a)+len(b) {
		return false
	}
	ok := make([]bool, len(b)+1)
	ok[0] = true
	for j := 1; j <= len(b); j++ {
		ok[j] = ok[j-1] && log[j-1] == b[j-1]
	}
	for i := 1; i <= len(a); i++ {
		ok[0] = ok[0] && log[i-1] == a[i-1]
		for j := 1; j <= len(b); j++ {
			ok[j] = (ok[j] && log[i+j-1] == a[i-1]) || (ok[j-1] && log[i+j-1] == b[j-1])
		}
	}
	return ok[len(b)]
}

// noTaskLeft requires that task id left no journal directory, no scratch
// under the temp root and no occupied slot.
func noTaskLeft(t *testing.T, vr *vendorRun, id string) {
	t.Helper()
	if _, err := os.Stat(filepath.Join(vr.root, journalDir, id)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("task %s left its directory: %v", id, err)
	}
	if entries, _ := os.ReadDir(vr.tmp); len(entries) != 0 {
		t.Fatalf("task %s left scratch %v", id, entries)
	}
	s := vr.super(t)
	s.mu.Lock()
	occupied := 0
	for _, n := range s.occupied {
		occupied += n
	}
	s.mu.Unlock()
	if occupied != 0 {
		t.Fatalf("task %s left %d occupied slots", id, occupied)
	}
}

// wave2Posture is TestRealAdapterLocal/wave2-posture (FP-1, FP-6): the
// startup warnings, candidate validation, ready-check cycles and task
// preparation for both OS values, the refusal before any launch or
// workspace preparation, and probe precedence.
func wave2Posture(t *testing.T) {
	// Run validates the new explicit paths itself.
	for _, o := range []RunOptions{{GrokAdapterPath: "grok"}, {CursorAdapterPath: "cursor-agent"}, {CursorAdapterPath: "./agent"}} {
		o.StateDir, o.SoftwareVersion = t.TempDir(), "test-1"
		err := testDeps(nil).run(bg, o)
		wantCode(t, err, contract.CodeInvalidArgument, "adapter must be an absolute path to the")
	}
	// Startup warnings, decided by RunOptions.GOOS (no task launches here):
	// Grok's single warning (with the macOS suffix on darwin), Cursor's,
	// and the unchanged Claude/Codex macOS warning only for those vendors.
	grokDarwin := GrokVendorWarning + " " + GrokDarwinWarningSuffix
	for _, c := range []struct {
		goos                         string
		claude, grok, cursor         bool
		wantGrok, wantSuffixed, wCur int
		wantDarwin                   int
	}{
		{"linux", false, true, true, 1, 0, 1, 0},
		{"darwin", false, true, true, 0, 1, 1, 0},
		{"darwin", false, false, true, 0, 0, 1, 0},
		{"darwin", true, true, false, 0, 1, 0, 1},
		{"linux", true, false, false, 0, 0, 0, 0},
	} {
		fp := startFakePlane(t)
		n := &atomic.Int32{}
		exe := fakeExeFile(t)
		tr := startTaskRun(t, fp, taskOpts{goos: c.goos, adapters: func(string) adapter.Registry { return wave2Registry(n) }, options: func(o *RunOptions) {
			if c.claude {
				o.ClaudeAdapterPath = exe
			}
			if c.grok {
				o.GrokAdapterPath = exe
			}
			if c.cursor {
				o.CursorAdapterPath = exe
			}
		}})
		tr.ev.await(t, evStarted)
		logs := tr.logs.String()
		if warnings(logs, GrokVendorWarning) != c.wantGrok || warnings(logs, grokDarwin) != c.wantSuffixed || warnings(logs, CursorVendorWarning) != c.wCur ||
			warnings(logs, DarwinVendorWarning) != c.wantDarwin || warnings(logs, FakeAdapterWarning) != 1 {
			t.Fatalf("%+v: warnings\n%s", c, logs)
		}
	}
	if CursorVendorWarning != "cursor adapter enabled for version probing only: unattended worker execution is refused because no qualified recipe preserves the operator posture; consult the support catalog" ||
		GrokVendorWarning != "grok adapter enabled: prompts are passed in argv and may be visible to process inspection; the composed prompt limit is 32 KiB; dontAsk cancelled all measured writes, including permitted writes; exit 0 does not prove requested work completed" ||
		GrokDarwinWarningSuffix != "grok worker execution on macOS is refused pending qualification; consult the support catalog" {
		t.Fatal("a wave-2 warning text changed")
	}

	// Candidate validation through the native session: Cursor's matching
	// probe runs, then its posture refuses with the fixed text; Grok passes
	// only on a Linux host. Selection precedes the probe; a disabled
	// adapter names its own flag.
	fp := startFakePlane(t)
	vr := startWave2Run(t, fp, "", func(o *RunOptions) { o.ClaudeAdapterPath = "" }, nil)
	ins, run := manuals(t, vr.dir, "a", "m")
	c := fp.accept(t)
	c.connect()
	c.validate("p1", vendorRole(adapter.CursorID, "cur", ins, run))
	e := c.result("p1")
	wantResult(t, e, contract.CodeInvalidArgument, "adapter", contract.ReasonProbeFailed)
	if e.Message != wave2CursorPosture || vr.probes.Load() != 1 {
		t.Fatalf("cursor candidate %q after %d probes", e.Message, vr.probes.Load())
	}
	c.validate("p2", vendorRole(adapter.GrokID, "grk", ins, run))
	if e := c.result("p2"); runtime.GOOS == "linux" && e != nil || runtime.GOOS == "darwin" && (e == nil || e.Message != wave2GrokPosture) {
		t.Fatalf("native grok candidate on %s: %v", runtime.GOOS, e)
	}
	// Cursor's vendor model name is valid free model text (design
	// 12a-worker-selection): it passes the selection, is probed once more
	// and is then refused by the posture.
	vendorName := vendorRole(adapter.CursorID, "cur", ins, run)
	vendorName.Model = "grok-4.7-low"
	before := vr.probes.Load()
	c.validate("p3", vendorName)
	e = c.result("p3")
	wantResult(t, e, contract.CodeInvalidArgument, "adapter", contract.ReasonProbeFailed)
	if e.Message != wave2CursorPosture || vr.probes.Load()-before != 1 {
		t.Fatalf("cursor vendor model name %q (%d probes)", e.Message, vr.probes.Load()-before)
	}
	c.validate("p4", vendorRole(adapter.ClaudeID, "cl", ins, run))
	e = c.result("p4")
	wantResult(t, e, contract.CodeInvalidArgument, "adapter", contract.ReasonAdapterDisabled)
	if e.Message != "claude adapter is disabled on node; start sidecar with --claude-adapter ABSOLUTE_PATH" {
		t.Fatalf("disabled claude %q", e.Message)
	}
	// Both OS decisions on either host: only roleEnv.goos differs.
	env := vr.super(t).env
	linuxEnv, darwinEnv := env, env
	linuxEnv.goos, darwinEnv.goos = "linux", "darwin"
	grokCfg, cursorCfg, fakeCfg := vendorRole(adapter.GrokID, "g", ins, run), vendorRole(adapter.CursorID, "u", ins, run), roleConfig("f", ins, run)
	for _, ce := range []struct {
		env  roleEnv
		cfg  contract.RoleConfig
		want string
	}{
		{linuxEnv, grokCfg, ""}, {darwinEnv, grokCfg, wave2GrokPosture}, {linuxEnv, cursorCfg, wave2CursorPosture}, {darwinEnv, cursorCfg, wave2CursorPosture},
		{darwinEnv, fakeCfg, ""}, {roleEnv{adapters: env.adapters, executables: env.executables}, grokCfg, wave2GrokPosture},
	} {
		err := vr.d.checkCandidate(bg, ce.env, ce.cfg)
		var ce2 *contract.Error
		switch {
		case ce.want == "" && err != nil:
			t.Fatalf("%s on %q: %v", ce.cfg.Adapter, ce.env.goos, err)
		case ce.want != "" && (!errors.As(err, &ce2) || ce2.Message != ce.want || ce2.Details["field"] != "adapter" || ce2.Details["reason"] != contract.ReasonProbeFailed ||
			ce2.Code != contract.CodeInvalidArgument):
			t.Fatalf("%s on %q: %v", ce.cfg.Adapter, ce.env.goos, err)
		}
	}
	// Every ready-check cycle shares one probe per adapter and refuses the
	// posture per OS.
	roles := []contract.RoleRecord{{RoleConfig: grokCfg, RegistrationOrder: 1}, {RoleConfig: cursorCfg, RegistrationOrder: 2}, {RoleConfig: fakeCfg, RegistrationOrder: 3}}
	for goos, want := range map[string][]bool{"linux": {true, false, true}, "darwin": {false, false, true}} {
		ev := env
		ev.goos = goos
		before := vr.probes.Load()
		if got := vr.d.checkCycle(bg, ev, roles); !slices.Equal(got, want) || vr.probes.Load()-before != 3 {
			t.Fatalf("%s cycle = %v (%d probes)", goos, got, vr.probes.Load()-before)
		}
	}
	// A probe error keeps its precedence over the posture refusal.
	fails := &atomic.Int32{}
	freg, err := adapter.NewRegistry(failingProbe{adapter.NewCursor(""), fails}, failingProbe{adapter.NewGrok(""), fails})
	if err != nil {
		t.Fatal(err)
	}
	for _, cfg := range []contract.RoleConfig{cursorCfg, grokCfg} {
		err := vr.d.checkCandidate(bg, roleEnv{adapters: freg, executables: env.executables, goos: "darwin"}, cfg)
		var pe *contract.Error
		if !errors.As(err, &pe) || pe.Message != "the "+cfg.Adapter+" adapter cannot be invoked on this node: the adapter executable exited unsuccessfully" ||
			pe.Details["reason"] != contract.ReasonProbeFailed {
			t.Fatalf("%s probe failure: %v", cfg.Adapter, err)
		}
	}
	if fails.Load() != 2 {
		t.Fatalf("%d failing probes", fails.Load())
	}

	// Task preparation refuses independently (a stale or persisted role):
	// start_failed, the fixed local diagnostic, no journal, scratch,
	// workspace fetch, guardian or child, and the slot released. The
	// Darwin decision runs on either host; on the Linux decision Grok
	// launches while Cursor is still refused.
	for _, posture := range []string{"darwin", "linux"} {
		ws := newFakeWS(t)
		fp := startFakePlane(t)
		vr := startWave2Run(t, fp, posture, nil, func(d *deps) { d.taskWorkspacePlane = ws })
		ins, run := manuals(t, vr.dir, "a", "SECRET-MANUAL-TEXT")
		s := vr.connect(t, 1, 1, vendorRole(adapter.GrokID, "g", ins, run), vendorRole(adapter.CursorID, "u", ins, run))
		refused := func(role int, b contract.TaskStartBody) {
			t.Helper()
			rid := s.nextP()
			s.c.sendStart(rid, b)
			r := s.c.startResult(rid)
			if r.Err == nil || r.Err.Details["reason"] != contract.ReasonStartFailed || r.Preparing {
				t.Fatalf("%s %s start = %+v", posture, s.cfgs[role].Adapter, r)
			}
			vr.ev.awaitMatch(t, evTaskRefused, func(ev event) bool { return ev.id == b.TaskID })
			vr.noChild(t)
			noTaskLeft(t, vr, b.TaskID)
			if !hasLog(vr.logs.String(), "task worker posture not qualified", "task_id", b.TaskID) ||
				!hasLog(vr.logs.String(), "task worker posture not qualified", "reason", "worker_posture_not_qualified") ||
				strings.Contains(vr.logs.String(), "SECRET-MANUAL-TEXT") {
				t.Fatalf("%s %s diagnostic:\n%s", posture, s.cfgs[role].Adapter, vr.logs.String())
			}
		}
		refused(1, startBody(1, s.cfgs[1], 2, 1, "cursor goal"))
		// A workspace start is refused before any checkout preparation.
		wb := wsStart(2, s.cfgs[1], 1, ws)
		wb.Role.RegistrationOrder = 2
		refused(1, wb)
		if len(ws.entered) != 0 {
			t.Fatalf("%s: a refused workspace start fetched its base", posture)
		}
		if posture == "darwin" {
			refused(0, startBody(3, s.cfgs[0], 1, 1, "grok goal"))
			if vr.ledger.starts() != 0 {
				t.Fatal("a refused start reached a guardian")
			}
			continue
		}
		st, ch := s.run(t, 3, 0, "grok goal")
		ch.out(wave2Capture(t, "runs/grok-stdin-success/stdout.bin"))
		ch.exitCode(0)
		if res, _ := s.drain(t, st); strOf(res.FinalMessage) != "pong" || res.ExitCode == nil || *res.ExitCode != 0 {
			t.Fatalf("grok on the linux posture %+v", res)
		}
		// The slot the refusals released is reused.
		if _, r := s.start(t, 4, 1, "cursor again"); r.Err == nil || r.Err.Details["reason"] != contract.ReasonStartFailed {
			t.Fatalf("cursor after a launch %+v", r)
		}
	}
}

// wave2Invocation is TestRealAdapterLocal/wave2-invocation (FP-3): Grok's
// exact argv with the composed prompt as its last element, an empty stdin
// closed at once, the native scratch cwd, the 32 KiB composed-prompt
// boundary as a definite refusal, injected start failures with cleanup, and
// no prompt or argv in logs or journals.
func wave2Invocation(t *testing.T) {
	fp := startFakePlane(t)
	vr := startWave2Run(t, fp, "linux", nil, nil)
	insText, runText := "Instruction SECRET-INSTRUCTION \"quoted\" 'single' $(no) é 😀\n", "Runbook\tline\n"
	dir := filepath.Join(vr.dir, "m")
	os.MkdirAll(dir, 0o755)
	ins, run := filepath.Join(dir, "instruction.md"), filepath.Join(dir, "runbook.md")
	os.WriteFile(ins, []byte(insText), 0o644)
	os.WriteFile(run, []byte(runText), 0o644)
	cfg := vendorRole(adapter.GrokID, "g", ins, run)
	s := vr.connect(t, 1, 1, cfg)
	goal := "grok goal -- --model other; rm -rf / \"q\" \\ \n second line"
	st, ch := s.run(t, 1, 0, goal)
	want, err := composePrompt([]byte(insText), []byte(runText), st, contract.MaxPromptBytes)
	if err != nil {
		t.Fatal(err)
	}
	wantArgv := []string{"--output-format", "json", "--model", "grok-4.7", "--reasoning-effort", "low", "--permission-mode", "dontAsk", "-p", string(want)}
	if !slices.Equal(ch.spec.argv, wantArgv) || ch.spec.path != vr.super(t).env.executables[adapter.GrokID] || ch.spec.path == "" {
		t.Fatalf("grok child argv %q (path %s)", ch.spec.argv, ch.spec.path)
	}
	if stdin := ch.prompt(t); len(stdin) != 0 {
		t.Fatalf("grok stdin %q, want empty", stdin)
	}
	// The native private scratch directory is the cwd and PWD.
	if want := workDir(t, runtime.GOOS, vr.root, st.TaskID); ch.spec.dir != want || !slices.Contains(ch.spec.env, "PWD="+want) {
		t.Fatalf("cwd %s (want %s), env %q", ch.spec.dir, want, ch.spec.env)
	}
	ch.out(wave2Capture(t, "runs/grok-stdin-success/stdout.bin"))
	ch.exitCode(0)
	res, logs := s.drain(t, st)
	if strOf(res.FinalMessage) != "pong" || !bytes.Equal(logs, wave2Capture(t, "runs/grok-stdin-success/stdout.bin")) {
		t.Fatalf("grok result %+v", res)
	}
	// Neither the prompt nor manual content is logged or journaled.
	if strings.Contains(vr.logs.String(), "SECRET-INSTRUCTION") || strings.Contains(vr.logs.String(), "second line") {
		t.Fatalf("the prompt reached the sidecar log:\n%s", vr.logs.String())
	}
	if jb, err := os.ReadFile(filepath.Join(vr.root, journalDir, st.TaskID, executionName)); err == nil && bytes.Contains(jb, []byte("SECRET-INSTRUCTION")) {
		t.Fatal("the composed prompt reached the execution journal")
	}
	// The 32 KiB composed-prompt boundary: an instruction padded so the
	// composed prompt is exactly 32 KiB launches; one byte more is a
	// definite start_failed before any guardian, its scratch removed.
	for i, extra := range []int{0, 1} {
		n := 10 + i
		b := startBody(n, cfg, 1, 1, "boundary")
		base, err := composePrompt(nil, []byte(runText), b, contract.MaxPromptBytes)
		if err != nil {
			t.Fatal(err)
		}
		pad := bytes.Repeat([]byte("a"), adapter.MaxGrokPromptBytes-len(base)+extra)
		if p, _ := composePrompt(pad, []byte(runText), b, contract.MaxPromptBytes); len(p) != adapter.MaxGrokPromptBytes+extra {
			t.Fatalf("padded prompt %d bytes", len(p))
		}
		os.WriteFile(ins, pad, 0o644)
		starts := vr.ledger.starts()
		rid := s.nextP()
		s.c.sendStart(rid, b)
		r := s.c.startResult(rid)
		if extra == 0 {
			if r.Err != nil {
				t.Fatalf("a 32 KiB prompt was refused: %+v", r.Err)
			}
			ch := vr.child(t)
			if last := ch.spec.argv[len(ch.spec.argv)-1]; len(last) != adapter.MaxGrokPromptBytes {
				t.Fatalf("prompt argument %d bytes", len(last))
			}
			ch.exitCode(0)
			s.drain(t, b)
			continue
		}
		if r.Err == nil || r.Err.Details["reason"] != contract.ReasonStartFailed {
			t.Fatalf("an over-32-KiB prompt = %+v", r)
		}
		vr.ev.awaitMatch(t, evTaskRefused, func(ev event) bool { return ev.id == b.TaskID })
		vr.noChild(t)
		noTaskLeft(t, vr, b.TaskID)
		if vr.ledger.starts() != starts || !hasLog(vr.logs.String(), "task invocation refused", "task_id", b.TaskID) ||
			!hasLog(vr.logs.String(), "task invocation refused", "reason", "invalid_invocation") {
			t.Fatalf("oversized prompt diagnostic:\n%s", vr.logs.String())
		}
	}
	os.WriteFile(ins, []byte(insText), 0o644)
	// An injected guardian start failure (as an oversized frame or E2BIG
	// would be) is a definite refusal with cleanup, never another transport.
	vr.ledger.mu.Lock()
	vr.ledger.adapterErr = errUnsupported
	vr.ledger.mu.Unlock()
	if _, r := s.start(t, 20, 0, "start failure"); r.Err == nil || r.Err.Details["reason"] != contract.ReasonStartFailed {
		t.Fatalf("an injected start failure = %+v", r)
	}
	vr.ev.awaitMatch(t, evTaskRefused, func(ev event) bool { return ev.id == taskID(20) })
	noTaskLeft(t, vr, taskID(20))
}

// wave2Outcomes is TestRealAdapterLocal/wave2-outcomes (FP-4, FP-6): the
// captured Grok exits and finals through the process semantics, the
// stdin-missing and malformed outputs' single diagnostic, cancelled output
// that is not a Callsheet stop, and a plane control's precedence.
func wave2Outcomes(t *testing.T) {
	fp := startFakePlane(t)
	vr := startWave2Run(t, fp, "linux", nil, nil)
	ins, run := manuals(t, vr.dir, "a", "m")
	s := vr.connect(t, 1, 1, vendorRole(adapter.GrokID, "g", ins, run))
	diag := "callsheet: grok final-message extraction failed (invalid_final_output)\n"
	var cancelled struct{ Text string }
	json.Unmarshal(wave2Capture(t, "runs/grok-shell-home/stdout.bin"), &cancelled)
	failMsg := `Couldn't set model 'does-not-exist': Invalid params: "unknown model id". Run 'grok models' to see available models.`
	for i, c := range []struct {
		run, stdout, stderr string
		exit                int
		final, dx           string
	}{
		{"grok-stdin-success", "", "", 0, "pong", ""},
		{"grok-stdin-fail", "", "", 1, failMsg, ""},
		{"grok-shell-home", "", "", 0, cancelled.Text, ""},
		{"grok-stdin-missing", "", "", 2, "<nil>", diag},
		{"", "not json {\n", "warn\n", 0, "<nil>", diag},
	} {
		stdout, stderr := []byte(c.stdout), []byte(c.stderr)
		if c.run != "" {
			stdout, stderr = wave2Capture(t, "runs/"+c.run+"/stdout.bin"), wave2Capture(t, "runs/"+c.run+"/stderr.bin")
			if e := strings.TrimSpace(string(wave2Capture(t, "runs/"+c.run+"/exit.txt"))); e != strconv.Itoa(c.exit) {
				t.Fatalf("%s captured exit %s", c.run, e)
			}
		}
		st, ch := s.run(t, 10+i, 0, "outcome")
		ch.out(stdout)
		ch.errOut(stderr)
		ch.exitCode(c.exit)
		res, logs := s.drain(t, st)
		if strOf(res.FinalMessage) != c.final || res.ExitCode == nil || *res.ExitCode != c.exit || res.Outcome != contract.OutcomeNatural || res.StopID != nil ||
			!strings.HasSuffix(string(logs), c.dx) || !interleaved(logs[:len(logs)-len(c.dx)], stdout, stderr) || res.OutputBytes != len(logs) {
			t.Fatalf("case %d (%s): %+v final %q logs %q", i, c.run, res, strOf(res.FinalMessage), logs)
		}
		warned := hasLog(vr.logs.String(), "task final-message extraction failed", "task_id", st.TaskID)
		if warned != (c.dx != "") || strings.Count(string(logs), "final-message extraction failed") != strings.Count(c.dx, "final-message extraction failed") {
			t.Fatalf("case %d: warning %v, logs %q", i, warned, logs)
		}
		if c.dx != "" && (!hasLog(vr.logs.String(), "task final-message extraction failed", "adapter", "grok") ||
			!hasLog(vr.logs.String(), "task final-message extraction failed", "classification", adapter.FinalInvalid)) {
			t.Fatalf("case %d: warning attributes:\n%s", i, vr.logs.String())
		}
		// The group is gone and the scratch removed before the result.
		if _, err := os.Stat(ch.spec.dir); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("case %d: scratch kept: %v", i, err)
		}
	}
	// A plane control wins over the vendor's own cancelled output: the
	// outcome is cancelled naming the stop intent.
	st, ch := s.run(t, 30, 0, "cancel me")
	out := wave2Capture(t, "runs/grok-shell-home/stdout.bin")
	ch.out(out)
	if got := s.logs(t, st, len(out)); !bytes.Equal(got, out) {
		t.Fatalf("cancelled task logs %q", got)
	}
	in := stopOf("e")
	s.sendCancel(t, st, in)
	vr.settled(t, st.TaskID)
	r := s.result(t, st)
	if r.Outcome != contract.OutcomeCancelled || r.StopID == nil || *r.StopID != in.ID {
		t.Fatalf("cancelled grok %+v", r)
	}
	vr.groups.mu.Lock()
	gone := slices.Contains(vr.groups.cleaned, ch.gpid)
	vr.groups.mu.Unlock()
	if !gone {
		t.Fatal("the cancelled task's group absence was never checked")
	}
}

// wave2Retry is TestRealAdapterLocal/wave2-retry (FP-6): a sealed Grok
// outcome's retransmission and a restarted Run's recovery are
// byte-identical, with the extraction diagnostic once.
func wave2Retry(t *testing.T) {
	for _, c := range []struct {
		stdout            []byte
		wantFinal, wantLg string
	}{
		{[]byte("garbage"), "<nil>", "garbage" + "callsheet: grok final-message extraction failed (invalid_final_output)\n"},
		{wave2Capture(t, "runs/grok-stdin-success/stdout.bin"), "pong", string(wave2Capture(t, "runs/grok-stdin-success/stdout.bin"))},
	} {
		fp := startFakePlane(t)
		vr := startWave2Run(t, fp, "linux", nil, nil)
		ins, run := manuals(t, vr.dir, "a", "m")
		cfg := vendorRole(adapter.GrokID, "g", ins, run)
		s := vr.connect(t, 1, 1, cfg)
		st, ch := s.run(t, 1, 0, "retry")
		ch.out(c.stdout)
		ch.exitCode(0)
		vr.settled(t, st.TaskID)
		if got := s.logs(t, st, len(c.wantLg)); string(got) != c.wantLg {
			t.Fatalf("logs %q", got)
		}
		first := s.resultAck(t, st, false)
		if err := vr.clk.AwaitWaiter(testWait, testkit.HasTimer(resultRetry)); err != nil {
			t.Fatal(err)
		}
		vr.clk.Advance(resultRetry)
		again := s.resultAck(t, st, false)
		if again.Digest != first.Digest || strOf(first.FinalMessage) != c.wantFinal || strOf(again.FinalMessage) != c.wantFinal {
			t.Fatalf("retry %+v then %+v", first, again)
		}
		jb, err := os.ReadFile(filepath.Join(vr.root, journalDir, st.TaskID, executionName))
		if err != nil {
			t.Fatal(err)
		}
		j, err := contract.ParseExecutionJournal(jb, adapter.Lookup())
		if err != nil || j.Result == nil || j.Result.Digest != first.Digest || strings.Count(string(j.Log.Data), "final-message extraction failed") > 1 {
			t.Fatalf("journal %+v %v", j, err)
		}
		vr.run.cancel()
		select {
		case err := <-vr.run.done:
			vr.run.done <- err
		case <-time.After(testWait):
			t.Fatal("the first Run did not stop")
		}
		n := &atomic.Int32{}
		exe := fakeExeFile(t)
		tr2 := startTaskRun(t, fp, taskOpts{root: vr.root, adapters: func(string) adapter.Registry { return wave2Registry(n) }, adjust: roleGOOS("linux"),
			options: func(o *RunOptions) { o.GrokAdapterPath, o.CursorAdapterPath = exe, exe }})
		s2, entries := tr2.reconnect(t, 2, 2, map[string]string{st.TaskID: contract.ActionSendResult}, cfg)
		if len(entries) != 1 || entries[0].TaskID != st.TaskID {
			t.Fatalf("inventory %+v", entries)
		}
		res, logs := s2.drain(t, st)
		if res.Digest != first.Digest || strOf(res.FinalMessage) != c.wantFinal || string(logs) != c.wantLg {
			t.Fatalf("recovered %+v logs %q", res, logs)
		}
		tr2.noChild(t)
	}
}
