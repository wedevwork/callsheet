package adapter

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/wedevwork/callsheet/internal/contract"
	"github.com/wedevwork/callsheet/internal/testkit"
)

// Iteration 08 unit tests of the Claude and Codex adapters. They are pure
// or use the injected probe runner and clock: no vendor process (and no
// other child) is ever started here; the real replay launches belong to
// the ordinary/native function tests.

// captures is the checked-in evidence root (byte-identical copies of the
// owner's Linux captures, tests/testdata/real-adapters).
func captures(t testing.TB) string {
	t.Helper()
	return filepath.Join(testkit.MustRepoRoot(t), "tests", "testdata", "real-adapters", "linux-2026-09-30")
}

func readCapture(t testing.TB, rel string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(captures(t), filepath.FromSlash(rel)))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// TestRealAdapterRegistry is UT FP-1: the built-in registry of claude,
// codex and fake (with iteration 11's cursor and grok between and after
// them), their immutable descriptors, the qualification table and
// ValidateSelection.
func TestRealAdapterRegistry(t *testing.T) {
	r := Builtin(t.TempDir())
	ds := r.Descriptors()
	want := []Descriptor{{ID: "claude", Efforts: []string{"low"}}, {ID: "codex", Efforts: []string{"low"}}, {ID: "cursor", Efforts: []string{"low"}},
		{ID: "fake", Efforts: []string{"low", "medium", "high"}, TestOnly: true}, {ID: "grok", Efforts: []string{"low"}}}
	if len(ds) != len(want) {
		t.Fatalf("descriptors = %+v", ds)
	}
	for i := range want {
		if ds[i].ID != want[i].ID || ds[i].TestOnly != want[i].TestOnly || !slices.Equal(ds[i].Efforts, want[i].Efforts) {
			t.Fatalf("descriptor %d = %+v, want %+v", i, ds[i], want[i])
		}
	}
	// Defensive copies: neither a returned slice nor a descriptor mutates
	// the registry; only fake is test-only.
	ds[0].Efforts[0], ds[1].ID = "mutated", "mutated"
	for _, id := range []string{"claude", "codex"} {
		a, ok := r.Lookup(id)
		if !ok {
			t.Fatalf("%s missing", id)
		}
		d := a.Descriptor()
		d.Efforts[0] = "high"
		if again := a.Descriptor(); again.TestOnly || !slices.Equal(again.Efforts, []string{"low"}) || again.ID != id {
			t.Fatalf("%s descriptor %+v", id, again)
		}
	}
	if again := r.Descriptors(); again[0].Efforts[0] != "low" || again[1].ID != "codex" {
		t.Fatalf("registry mutated: %+v", again)
	}
	look := Lookup()
	for id, testOnly := range map[string]bool{"claude": false, "codex": false, "fake": true, "grok": false, "cursor": false} {
		if info, ok := look(id); !ok || info.TestOnly != testOnly {
			t.Fatalf("lookup %s = %+v %v", id, info, ok)
		}
	}
	// Exactly the captured pairs and versions (iteration 11's two rows after
	// the unchanged iteration 08 rows, the table sorted by ID).
	qs := Qualifications()
	if len(qs) != 4 || qs[0] != (Qualification{ID: "claude", Version: "2.1.285 (Claude Code)", Model: "sonnet", Effort: "low"}) ||
		qs[1] != (Qualification{ID: "codex", Version: "codex-cli 0.159.0", Model: "gpt-6.1-sol", Effort: "low"}) ||
		qs[2] != (Qualification{ID: "cursor", Version: "2026.10.01-e373342", Model: "grok-4.7", Effort: "low"}) ||
		qs[3] != (Qualification{ID: "grok", Version: "grok 1.0.46 (2765805b9442) [stable]", Model: "grok-4.7", Effort: "low"}) {
		t.Fatalf("qualifications = %+v", qs)
	}
	qs[0].Model = "opus"
	if Qualifications()[0].Model != "sonnet" {
		t.Fatal("qualification table mutable")
	}
	for _, q := range Qualifications() {
		if err := ValidateSelection(q.ID, q.Model, q.Effort); err != nil {
			t.Fatalf("%s qualified pair refused: %v", q.ID, err)
		}
	}
	// Unqualified pairs and unknown IDs: fixed messages naming the adapter
	// and its supported pair, never the submitted text.
	const secret = "SECRET-MODEL-TEXT"
	for _, c := range []struct{ id, model, effort, field, msg string }{
		{"claude", "opus[1m]", "low", "model", "the claude model/effort selection is not qualified; supported: model sonnet, effort low"},
		{"claude", "claude-sonnet-5-5", "low", "model", "supported: model sonnet, effort low"},
		{"claude", "sonnet", "medium", "effort", "the claude model/effort selection is not qualified"},
		{"claude", secret, "high", "model", "claude"},
		{"codex", "gpt-6.1-sol", "medium", "effort", "the codex model/effort selection is not qualified; supported: model gpt-6.1-sol, effort low"},
		{"codex", secret, "low", "model", "codex"},
		{"codex", "GPT-6.1-SOL", "low", "model", "codex"},
		{"unknown-vendor", "m", "low", "adapter", "unknown adapter; registered adapters: claude, codex, cursor, fake, grok"},
		{secret, "m", "low", "adapter", "unknown adapter"},
		{"fake", " ", "low", "model", "invalid model"},
		{"fake", "m", "extreme", "effort", "allowed: low, medium, high"},
	} {
		err := ValidateSelection(c.id, c.model, c.effort)
		var se *SelectionError
		if !errors.As(err, &se) || se.Field != c.field || !strings.Contains(se.Error(), c.msg) || strings.Contains(se.Error(), secret) {
			t.Fatalf("ValidateSelection(%q, %q, %q) = %v", c.id, c.model, c.effort, err)
		}
	}
	// The fake keeps its free model text with every effort.
	for _, e := range []string{"low", "medium", "high"} {
		if err := ValidateSelection("fake", "any future model -x", e); err != nil {
			t.Fatal(err)
		}
	}
	// Registry construction performs no I/O: a nonexistent state root.
	if _, ok := Builtin("/nonexistent/callsheet-state").Lookup("codex"); !ok {
		t.Fatal("registry depends on its directory")
	}
}

// fakeProc is an injected probe child: its wait writes the scripted
// output, then optionally blocks until the probe's context ends or the
// test's release, advances the fake clock, and reports the scripted error.
type fakeProc struct {
	r *fakeRunner
}

func (p fakeProc) pid() int { return 4242 }

func (p fakeProc) wait() (error, *os.ProcessState) {
	r := p.r
	r.mu.Lock()
	r.waits++
	r.mu.Unlock()
	io.WriteString(r.stdoutW, r.out)
	io.WriteString(r.stderrW, r.errOut)
	if r.block {
		close(r.blocking) // the child now waits for its kill
		<-r.ctx.Done()
		return errors.New("signal: killed"), nil
	}
	if r.advance > 0 {
		r.clk.Advance(r.advance)
	}
	return r.err, nil
}

// fakeRunner records every start (argv, dir, environment) and returns
// fakeProcs; startErr refuses to start.
type fakeRunner struct {
	mu               sync.Mutex
	clk              *testkit.FakeClock
	out, errOut      string
	err, startErr    error
	block            bool
	blocking         chan struct{} // closed once a blocking child waits
	advance          time.Duration
	starts, waits    int
	exe, dir         string
	args, env        []string
	waitDelay        time.Duration
	ctx              context.Context
	stdoutW, stderrW io.Writer
}

func (r *fakeRunner) start(ctx context.Context, exe string, args []string, dir string, env []string, stdout, stderr io.Writer, waitDelay time.Duration) (probeProc, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.startErr != nil {
		return nil, r.startErr
	}
	r.starts++
	r.exe, r.args, r.dir, r.env, r.waitDelay, r.ctx = exe, args, dir, env, waitDelay, ctx
	r.stdoutW, r.stderrW = stdout, stderr
	return fakeProc{r: r}, nil
}

// vendorProber returns id's prober with the injected runner and clock.
func vendorProber(t *testing.T, id string, r *fakeRunner, env []string) (*vendor, *prober) {
	t.Helper()
	v := newVendor(id, t.TempDir())
	r.clk = testkit.NewFakeClock(time.Date(2026, 9, 30, 12, 0, 0, 0, time.UTC))
	v.p.clock, v.p.runner, v.p.environ = r.clk, r, func() []string { return env }
	return v, v.p
}

// exeFile writes a never-executed regular file with mode.
func exeFile(t *testing.T, mode os.FileMode) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "vendor cli")
	if err := os.WriteFile(p, []byte("#!/bin/sh\nexit 99\n"), mode); err != nil {
		t.Fatal(err)
	}
	return p
}

// TestVendorProbe is UT FP-2: the explicit-path version probe on the
// injected runner and fake clock (the fake's real-process probe tests are
// unchanged in TestAdapterContract).
func TestVendorProbe(t *testing.T) {
	exe := exeFile(t, 0o755)
	for _, q := range Qualifications() {
		for _, out := range []string{q.Version + "\n", q.Version + "\r\n", q.Version} {
			r := &fakeRunner{out: out, errOut: "warning: SECRET-STDERR environment note\n"}
			v, _ := vendorProber(t, q.ID, r, []string{"PATH=/nowhere", "CALLSHEET_FAKE_READY_FD=3", "KEEP=1"})
			if err := v.Probe(bg, exe); err != nil {
				t.Fatalf("%s %q: %v", q.ID, out, err)
			}
			// Exactly "exe --version" in the state root with the filtered
			// environment, the 100 ms pipe bound, waited exactly once.
			if r.starts != 1 || r.waits != 1 || r.exe != exe || !slices.Equal(r.args, []string{"--version"}) || r.dir != v.p.dir ||
				!slices.Equal(r.env, []string{"PATH=/nowhere", "KEEP=1"}) || r.waitDelay != probeWaitDelay {
				t.Fatalf("%s probe ran %+v", q.ID, r)
			}
		}
	}
	claudeV, codexV := Qualifications()[0].Version, Qualifications()[1].Version
	for name, c := range map[string]struct {
		id, out, errOut string
		err             error
		reason          string
	}{
		"claude wrong version":  {"claude", "2.1.284 (Claude Code)\n", "", nil, "has an unqualified claude version; expected 2.1.285 (Claude Code)"},
		"codex wrong version":   {"codex", "codex-cli 0.160.0\n", "", nil, "has an unqualified codex version; expected codex-cli 0.159.0"},
		"claude wrong vendor":   {"claude", codexV + "\n", "", nil, "is not the claude CLI (unexpected version output)"},
		"codex wrong vendor":    {"codex", claudeV + "\n", "", nil, "is not the codex CLI (unexpected version output)"},
		"banner":                {"claude", "Welcome!\n" + claudeV + "\n", "", nil, "is not the claude CLI"},
		"extra line":            {"codex", codexV + "\nupdate available\n", "", nil, "is not the codex CLI"},
		"two newlines":          {"claude", claudeV + "\n\n", "", nil, "is not the claude CLI"},
		"leading space":         {"codex", " " + codexV + "\n", "", nil, "is not the codex CLI"},
		"empty":                 {"claude", "", "", nil, "is not the claude CLI"},
		"nonzero exit":          {"claude", claudeV + "\n", "", errors.New("exit status 1"), "exited unsuccessfully"},
		"stdout overflow":       {"codex", strings.Repeat("x", probeCapture+1), "", nil, "more output than a probe allows"},
		"stderr overflow":       {"claude", claudeV + "\n", strings.Repeat("SECRET", probeCapture), nil, "more output than a probe allows"},
		"inherited pipe closed": {"codex", codexV + "\n", "", exec.ErrWaitDelay, "left its output open after exiting"},
		"output unreadable":     {"claude", claudeV + "\n", "", errProbeOutput, "had output the probe could not read"},
	} {
		r := &fakeRunner{out: c.out, errOut: c.errOut, err: c.err}
		v, _ := vendorProber(t, c.id, r, nil)
		err := v.Probe(bg, exe)
		wantProbeErr(t, err, c.reason)
		if strings.Contains(err.Error(), "SECRET") || strings.Contains(err.Error(), "update available") {
			t.Fatalf("%s: output leaked: %v", name, err)
		}
		if r.waits != 1 {
			t.Fatalf("%s: waited %d times", name, r.waits)
		}
	}
	// The path rules run before any start: a relative or bare name never
	// consults PATH; missing, directory and non-executable files fail.
	r := &fakeRunner{out: claudeV}
	v, _ := vendorProber(t, "claude", r, []string{"PATH=" + filepath.Dir(exe)})
	for p, reason := range map[string]string{
		"claude": "not absolute", "./claude": "not absolute", "": "not absolute",
		filepath.Join(t.TempDir(), "absent"): "does not exist", t.TempDir(): "not a regular file",
		exeFile(t, 0o644): "no executable permission bit",
	} {
		wantProbeErr(t, v.Probe(bg, p), reason)
	}
	if r.starts != 0 {
		t.Fatalf("a refused path started %d probes", r.starts)
	}
	r.startErr = errors.New("exec format error")
	wantProbeErr(t, v.Probe(bg, exe), "cannot be executed")
	// Cancellation wins before a start and while the child runs.
	r = &fakeRunner{out: claudeV, block: true, blocking: make(chan struct{})}
	v, _ = vendorProber(t, "claude", r, nil)
	ctx, cancel := context.WithCancel(bg)
	cancel()
	if err := v.Probe(ctx, exe); !errors.Is(err, context.Canceled) || r.starts != 0 {
		t.Fatalf("canceled before start = %v (%d starts)", err, r.starts)
	}
	ctx, cancel = context.WithCancel(bg)
	done := make(chan error, 1)
	go func() { done <- v.Probe(ctx, exe) }()
	if err := r.clkAwait(); err != nil {
		t.Fatal(err)
	}
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) || r.waits != 1 {
		t.Fatalf("canceled while running = %v (%d waits)", err, r.waits)
	}
	// The deadline: the timer kills a child that never finishes; a child
	// completing exactly at the deadline is a timeout and one completing
	// just before it is a success (the completion instant decides).
	r = &fakeRunner{out: claudeV, block: true, blocking: make(chan struct{})}
	v, _ = vendorProber(t, "claude", r, nil)
	go func() { done <- v.Probe(bg, exe) }()
	if err := r.clkAwait(); err != nil {
		t.Fatal(err)
	}
	r.clk.Advance(ProbeTimeout)
	wantProbeErr(t, <-done, "did not answer within 1s")
	for advance, reason := range map[time.Duration]string{ProbeTimeout: "did not answer within 1s", ProbeTimeout - time.Nanosecond: ""} {
		r = &fakeRunner{out: claudeV, advance: advance}
		v, _ = vendorProber(t, "claude", r, nil)
		err := v.Probe(bg, exe)
		if reason == "" && err != nil {
			t.Fatalf("completion %v after the start: %v", advance, err)
		}
		if reason != "" {
			wantProbeErr(t, err, reason)
		}
		if ws := r.clk.Waiters(); len(ws) != 0 || r.waits != 1 {
			t.Fatalf("timer left %v, waits %d", ws, r.waits)
		}
	}
}

// clkAwait waits (bounded) until the blocked probe child is waiting. Its
// deadline timer was armed before the child started, so exactly that one
// timer is then active.
func (r *fakeRunner) clkAwait() error {
	select {
	case <-r.blocking:
	case <-time.After(testWait):
		return errors.New("the blocked probe child never waited")
	}
	if ws := r.clk.Waiters(); len(ws) != 1 {
		return errors.New("the probe deadline timer is not the one active waiter")
	}
	return nil
}

// forbidden are flags no worker argv may carry (sandbox, permission and
// session overrides).
var forbidden = []string{"--dangerously-skip-permissions", "--dangerously-bypass-approvals-and-sandbox", "--sandbox", "-s", "--ignore-user-config",
	"--resume", "--continue", "--worktree", "--permission-mode", "--approve-for-me", "--full-auto", "--yolo", "--cloud", "--mcp-server", "--auto-review", "-m"}

// TestVendorInvocation is UT FP-3: the exact captured argv arrays, owned
// stdin, the prompt bound and the refusals, with no process.
func TestVendorInvocation(t *testing.T) {
	scratch := filepath.Join(t.TempDir(), "callsheet-task-"+taskID+"-1")
	prompt := []byte("{\"format\":\"callsheet-task-v1\"}\n\x00\xff 'quoted' \"double\" $(not a shell) \\n`tick`")
	claude := NewClaude("")
	inv, err := claude.Invocation(TaskInput{TaskID: taskID, Model: "sonnet", Effort: "low", Prompt: prompt, ScratchDir: scratch})
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"-p", "--model", "sonnet", "--effort", "low", "--permission-prompts", "none", "--output-format", "json"}; !slices.Equal(inv.Argv, want) ||
		!bytes.Equal(inv.Stdin, prompt) || inv.FinalFile != "" {
		t.Fatalf("claude invocation = %q %q %q", inv.Argv, inv.Stdin, inv.FinalFile)
	}
	codex := NewCodex("")
	cinv, err := codex.Invocation(TaskInput{TaskID: taskID, Model: "gpt-6.1-sol", Effort: "low", Prompt: prompt, ScratchDir: scratch})
	if err != nil {
		t.Fatal(err)
	}
	final := scratch + "/callsheet-final.txt"
	if want := []string{"-a", "never", "exec", "--model", "gpt-6.1-sol", "-c", `model_reasoning_effort="low"`, "--json", "--skip-git-repo-check",
		"--output-last-message", final, "-"}; !slices.Equal(cinv.Argv, want) || !bytes.Equal(cinv.Stdin, prompt) || cinv.FinalFile != final {
		t.Fatalf("codex invocation = %q %q", cinv.Argv, cinv.FinalFile)
	}
	// The effort value is one argv element with literal quote bytes.
	if cinv.Argv[6] != "model_reasoning_effort=\x22low\x22" || len(cinv.Argv[6]) != len("model_reasoning_effort=")+5 {
		t.Fatalf("effort element %q", cinv.Argv[6])
	}
	for _, in := range []Invocation{inv, cinv} {
		for _, a := range in.Argv {
			if slices.Contains(forbidden, a) || strings.Contains(a, "\x00") || bytes.Contains(prompt, []byte(a)) && len(a) > 8 {
				t.Fatalf("argv %q carries %q", in.Argv, a)
			}
		}
	}
	// Owned copies: new argv and a stdin clone every time.
	prompt[0] = 'X'
	if inv.Stdin[0] != '{' || cinv.Stdin[0] != '{' {
		t.Fatal("stdin aliases the prompt")
	}
	inv.Argv[2], cinv.Argv[4] = "mutated", "mutated"
	again, _ := claude.Invocation(TaskInput{TaskID: taskID, Model: "sonnet", Effort: "low"})
	cagain, _ := codex.Invocation(TaskInput{TaskID: taskID, Model: "gpt-6.1-sol", Effort: "low", ScratchDir: scratch})
	if again.Argv[2] != "sonnet" || cagain.Argv[4] != "gpt-6.1-sol" || len(again.Stdin) != 0 {
		t.Fatalf("second invocations %q %q", again.Argv, cagain.Argv)
	}
	// The 16 MiB bound: at the limit accepted, one byte over refused.
	big := make([]byte, contract.MaxPromptBytes)
	for _, a := range []Adapter{claude, codex} {
		d := a.Descriptor()
		q, _ := qualified(d.ID)
		if _, err := a.Invocation(TaskInput{TaskID: taskID, Model: q.Model, Effort: q.Effort, Prompt: big, ScratchDir: scratch}); err != nil {
			t.Fatalf("%s 16 MiB: %v", d.ID, err)
		}
		for name, in := range map[string]TaskInput{
			"task id":    {TaskID: "t_x", Model: q.Model, Effort: q.Effort, ScratchDir: scratch},
			"no model":   {TaskID: taskID, Effort: q.Effort, ScratchDir: scratch},
			"control":    {TaskID: taskID, Model: q.Model + "\n", Effort: q.Effort, ScratchDir: scratch},
			"model":      {TaskID: taskID, Model: "other", Effort: q.Effort, ScratchDir: scratch},
			"effort":     {TaskID: taskID, Model: q.Model, Effort: "high", ScratchDir: scratch},
			"prompt":     {TaskID: taskID, Model: q.Model, Effort: q.Effort, Prompt: make([]byte, contract.MaxPromptBytes+1), ScratchDir: scratch},
			"no default": {TaskID: taskID, ScratchDir: scratch},
		} {
			if _, err := a.Invocation(in); err == nil {
				t.Fatalf("%s %s accepted", d.ID, name)
			}
		}
	}
	// Claude ignores the scratch directory (its answer is on stdout).
	if _, err := claude.Invocation(TaskInput{TaskID: taskID, Model: "sonnet", Effort: "low", ScratchDir: "relative"}); err != nil {
		t.Fatalf("claude with an unused scratch value: %v", err)
	}
	// Codex requires the task's absolute clean scratch path.
	for _, s := range []string{"", "relative/dir", scratch + "/", scratch + "/../x", "/a/./b"} {
		if _, err := codex.Invocation(TaskInput{TaskID: taskID, Model: "gpt-6.1-sol", Effort: "low", ScratchDir: s}); err == nil {
			t.Fatalf("scratch %q accepted", s)
		}
	}
}
