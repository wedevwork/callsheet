package adapter

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/wedevwork/callsheet/internal/contract"
	"github.com/wedevwork/callsheet/internal/testkit"
)

// Iteration 11 unit tests of the Grok and Cursor adapters (wave 2). Like
// iteration 08's they are pure or use the injected probe runner and clock:
// no vendor process (and no other child) is ever started here.

// wave2Root is iteration 11's checked-in evidence root (byte-identical
// copies of the coordinator's Linux captures of 2026-10-04).
const wave2Root = "tests/testdata/real-adapters/linux-2026-10-04"

func wave2Capture(t testing.TB, rel string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(testkit.MustRepoRoot(t), filepath.FromSlash(wave2Root), filepath.FromSlash(rel)))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// Exact posture refusals (workers.md, Registration and enablement).
const (
	grokPostureText   = "grok worker execution is not qualified on this OS; consult the support catalog"
	cursorPostureText = "cursor worker execution is refused: no qualified unattended recipe preserves the operator posture; consult the support catalog"
)

// TestWave2RegistrySelection is UT FP-1 (iteration 11): five sorted
// defensive descriptors with only fake test-only, the exact Grok and Cursor
// pairs and their refusals, and a known Cursor pair that is not runnable.
func TestWave2RegistrySelection(t *testing.T) {
	r := Builtin(t.TempDir())
	ds := r.Descriptors()
	ids := []string{"claude", "codex", "cursor", "fake", "grok"}
	if len(ds) != len(ids) {
		t.Fatalf("descriptors %+v", ds)
	}
	for i, d := range ds {
		want := []string{"low"}
		if d.ID == FakeID {
			want = []string{"low", "medium", "high"}
		}
		if d.ID != ids[i] || d.TestOnly != (d.ID == FakeID) || !slices.Equal(d.Efforts, want) {
			t.Fatalf("descriptor %d = %+v", i, d)
		}
	}
	// Defensive copies of the registry, each adapter's descriptor and the
	// qualification table.
	ds[2].Efforts[0], ds[4].ID = "mutated", "mutated"
	for _, id := range []string{GrokID, CursorID} {
		a, ok := r.Lookup(id)
		if !ok {
			t.Fatalf("%s not registered", id)
		}
		d := a.Descriptor()
		d.Efforts[0] = "high"
		if again := a.Descriptor(); again.ID != id || again.TestOnly || !slices.Equal(again.Efforts, []string{"low"}) {
			t.Fatalf("%s descriptor %+v", id, again)
		}
		info, ok := Lookup()(id)
		if !ok || info.TestOnly || !slices.Equal(info.Efforts, []string{"low"}) {
			t.Fatalf("lookup %s = %+v %v", id, info, ok)
		}
	}
	if again := r.Descriptors(); again[2].Efforts[0] != "low" || again[4].ID != GrokID {
		t.Fatalf("registry mutated: %+v", again)
	}
	qs := Qualifications()
	qs[2].Model, qs[3].Version = "mutated", "mutated"
	if q, _ := qualified(CursorID); q.Model != "grok-4.7" || q.Version != "2026.10.01-e373342" {
		t.Fatalf("qualification table mutable: %+v", q)
	}
	if q, _ := qualified(GrokID); q.Version != "grok 1.0.46 (2765805b9442) [stable]" || q.Model != "grok-4.7" || q.Effort != "low" {
		t.Fatalf("grok qualification %+v", q)
	}
	// Construction performs no I/O.
	for _, a := range []Adapter{NewGrok("/nonexistent/callsheet-state"), NewCursor("/nonexistent/callsheet-state")} {
		if a.Descriptor().TestOnly {
			t.Fatal("a vendor is test-only")
		}
	}
	// Exactly the captured pairs are accepted.
	for _, id := range []string{GrokID, CursorID} {
		if err := ValidateSelection(id, "grok-4.7", "low"); err != nil {
			t.Fatalf("%s qualified pair refused: %v", id, err)
		}
	}
	// Every other model or effort is refused with the field-specific fixed
	// message, never echoing the submitted value. Cursor's vendor argument
	// grok-4.7-low is not a Callsheet model (a literal mapping, not a
	// concatenation).
	const secret = "SECRET-MODEL-TEXT"
	for _, c := range []struct{ id, model, effort, field string }{
		{GrokID, "grok-4.7-build", "low", "model"},
		{GrokID, "grok-4.7-low", "low", "model"},
		{GrokID, "grok-4.7-build-fast", "low", "model"},
		{GrokID, "grok-4.6", "low", "model"},
		{GrokID, "auto", "low", "model"},
		{GrokID, "default", "low", "model"},
		{GrokID, "grok-4.7[effort=low]", "low", "model"},
		{GrokID, "GROK-4.7", "low", "model"},
		{GrokID, secret, "low", "model"},
		{GrokID, "grok-4.7", "medium", "effort"},
		{GrokID, "grok-4.7", "high", "effort"},
		{GrokID, "grok-4.7", "xhigh", "effort"},
		{GrokID, "grok-4.7", "", "effort"},
		{CursorID, "grok-4.7-low", "low", "model"},
		{CursorID, "grok-4.7-low-fast", "low", "model"},
		{CursorID, "grok-4.7-medium", "low", "model"},
		{CursorID, "auto", "low", "model"},
		{CursorID, "grok-4.7[effort=low]", "low", "model"},
		{CursorID, "\"grok-4.7[effort=high]\"", "low", "model"},
		{CursorID, secret, "low", "model"},
		{CursorID, "grok-4.7", "medium", "effort"},
		{CursorID, "grok-4.7", "high", "effort"},
		{CursorID, "grok-4.7", "xhigh", "effort"},
		{CursorID, "grok-4.7", "fast", "effort"},
	} {
		err := ValidateSelection(c.id, c.model, c.effort)
		var se *SelectionError
		want := "the " + c.id + " model/effort selection is not qualified; supported: model grok-4.7, effort low"
		if !errors.As(err, &se) || se.Field != c.field || se.Error() != want || strings.Contains(se.Error(), secret) {
			t.Fatalf("ValidateSelection(%q, %q, %q) = %v", c.id, c.model, c.effort, err)
		}
	}
	// The unknown-adapter diagnostic lists all five IDs and never the input.
	var se *SelectionError
	if err := ValidateSelection(secret, "m", "low"); !errors.As(err, &se) || se.Field != "adapter" ||
		se.Error() != "adapter: unknown adapter; registered adapters: claude, codex, cursor, fake, grok" {
		t.Fatalf("unknown adapter = %v", err)
	}
	// Cursor's known pair does not make it runnable: its selection passes,
	// its posture is refused on every OS and its invocation always errors.
	cursor, _ := r.Lookup(CursorID)
	inv, err := cursor.Invocation(TaskInput{TaskID: taskID, Model: "grok-4.7", Effort: "low", Prompt: []byte("prompt")})
	if err == nil || err.Error() != cursorPostureText || inv.Argv != nil || inv.Stdin != nil || inv.FinalFile != "" {
		t.Fatalf("cursor invocation %+v %v", inv, err)
	}
	for _, goos := range []string{"linux", "darwin"} {
		if err := ValidateWorkerPosture(CursorID, goos); err == nil {
			t.Fatalf("cursor eligible on %s", goos)
		}
	}
}

// TestWave2Posture is UT FP-1/FP-6 (iteration 11): ValidateWorkerPosture's
// exact decisions from the explicit OS value.
func TestWave2Posture(t *testing.T) {
	if err := ValidateWorkerPosture(GrokID, "linux"); err != nil {
		t.Fatalf("grok on linux: %v", err)
	}
	for _, goos := range []string{"darwin", "", "windows", "freebsd", "Linux", "linux "} {
		if err := ValidateWorkerPosture(GrokID, goos); err == nil || err.Error() != grokPostureText {
			t.Fatalf("grok on %q: %v", goos, err)
		}
	}
	for _, goos := range []string{"linux", "darwin", "", "windows"} {
		if err := ValidateWorkerPosture(CursorID, goos); err == nil || err.Error() != cursorPostureText {
			t.Fatalf("cursor on %q: %v", goos, err)
		}
		// The existing adapters are unchanged here (their own platform
		// checks still apply elsewhere).
		for _, id := range []string{ClaudeID, CodexID, FakeID} {
			if err := ValidateWorkerPosture(id, goos); err != nil {
				t.Fatalf("%s on %q: %v", id, goos, err)
			}
		}
	}
	const secret = "SECRET-ADAPTER"
	for _, id := range []string{"unknown-vendor", "", "Grok", "CURSOR", secret} {
		err := ValidateWorkerPosture(id, "linux")
		if err == nil || err.Error() != "adapter: unknown adapter; registered adapters: claude, codex, cursor, fake, grok" || strings.Contains(err.Error(), secret) {
			t.Fatalf("unknown %q: %v", id, err)
		}
	}
}

// TestWave2VendorProbe is UT FP-2 (iteration 11): Grok's and Cursor's
// explicit-path version probes on the injected runner and fake clock.
func TestWave2VendorProbe(t *testing.T) {
	exe := exeFile(t, 0o755)
	grokV, cursorV := "grok 1.0.46 (2765805b9442) [stable]", "2026.10.01-e373342"
	for id, v := range map[string]string{GrokID: grokV, CursorID: cursorV} {
		for _, out := range []string{v + "\n", v + "\r\n", v} {
			r := &fakeRunner{out: out, errOut: "warning: SECRET-STDERR environment note\n"}
			a, _ := vendorProber(t, id, r, []string{"PATH=/nowhere", "CALLSHEET_FAKE_DESCENDANT_LIFETIME_FD=3", "KEEP=1"})
			if err := a.Probe(bg, exe); err != nil {
				t.Fatalf("%s %q: %v", id, out, err)
			}
			// Exactly "exe --version" (never a model, login, sandbox or task
			// call) in the state root with the filtered environment, the
			// 100 ms pipe bound, waited exactly once.
			if r.starts != 1 || r.waits != 1 || r.exe != exe || !slices.Equal(r.args, []string{"--version"}) || r.dir != a.p.dir ||
				!slices.Equal(r.env, []string{"PATH=/nowhere", "KEEP=1"}) || r.waitDelay != probeWaitDelay {
				t.Fatalf("%s probe ran %+v", id, r)
			}
		}
	}
	// A matching Cursor version is invocability only: Probe succeeds, the
	// posture still refuses.
	if ValidateWorkerPosture(CursorID, "linux") == nil {
		t.Fatal("a probed cursor became eligible")
	}
	for name, c := range map[string]struct {
		id, out, errOut string
		err             error
		reason          string
	}{
		"grok wrong version":     {GrokID, "grok 1.0.45 (1111111aaaaa) [stable]\n", "", nil, "has an unqualified grok version; expected grok 1.0.46 (2765805b9442) [stable]"},
		"grok other channel":     {GrokID, "grok 1.0.46 (2765805b9442) [beta]\n", "", nil, "has an unqualified grok version; expected grok 1.0.46 (2765805b9442) [stable]"},
		"cursor wrong version":   {CursorID, "2026.10.02-abcdef0\n", "", nil, "has an unqualified cursor version; expected 2026.10.01-e373342"},
		"cursor lexical shape":   {CursorID, "2026.13.45-e373342\n", "", nil, "has an unqualified cursor version; expected 2026.10.01-e373342"},
		"grok given cursor":      {GrokID, cursorV + "\n", "", nil, "is not the grok CLI (unexpected version output)"},
		"cursor given grok":      {CursorID, grokV + "\n", "", nil, "is not the cursor CLI (unexpected version output)"},
		"grok given codex":       {GrokID, "codex-cli 0.159.0\n", "", nil, "is not the grok CLI (unexpected version output)"},
		"cursor given codex":     {CursorID, "codex-cli 0.159.0\n", "", nil, "is not the cursor CLI (unexpected version output)"},
		"cursor given claude":    {CursorID, "2.1.285 (Claude Code)\n", "", nil, "is not the cursor CLI"},
		"cursor uppercase hex":   {CursorID, "2026.10.01-E373342\n", "", nil, "is not the cursor CLI"},
		"cursor short year":      {CursorID, "26.10.01-e373342\n", "", nil, "is not the cursor CLI"},
		"cursor short month":     {CursorID, "2026.1.01-e373342\n", "", nil, "is not the cursor CLI"},
		"cursor empty hash":      {CursorID, "2026.10.01-\n", "", nil, "is not the cursor CLI"},
		"cursor trailing space":  {CursorID, cursorV + " \n", "", nil, "is not the cursor CLI"},
		"cursor named":           {CursorID, "cursor-agent " + cursorV + "\n", "", nil, "is not the cursor CLI"},
		"grok bare name":         {GrokID, "grok\n", "", nil, "is not the grok CLI"},
		"grok extra line":        {GrokID, grokV + "\nupdate available\n", "", nil, "is not the grok CLI"},
		"cursor extra line":      {CursorID, cursorV + "\n" + cursorV + "\n", "", nil, "is not the cursor CLI"},
		"grok banner":            {GrokID, "Welcome!\n" + grokV + "\n", "", nil, "is not the grok CLI"},
		"grok two newlines":      {GrokID, grokV + "\n\n", "", nil, "is not the grok CLI"},
		"cursor carriage return": {CursorID, "2026.10.01\r-e373342\n", "", nil, "is not the cursor CLI"},
		"grok empty":             {GrokID, "", "", nil, "is not the grok CLI"},
		"cursor nonzero exit":    {CursorID, cursorV + "\n", "", errors.New("exit status 1"), "exited unsuccessfully"},
		"grok stdout overflow":   {GrokID, strings.Repeat("g", probeCapture+1), "", nil, "more output than a probe allows"},
		"cursor stderr overflow": {CursorID, cursorV + "\n", strings.Repeat("SECRET", probeCapture), nil, "more output than a probe allows"},
		"grok inherited pipe":    {GrokID, grokV + "\n", "", exec.ErrWaitDelay, "left its output open after exiting"},
		"cursor unreadable":      {CursorID, cursorV + "\n", "", errProbeOutput, "had output the probe could not read"},
	} {
		r := &fakeRunner{out: c.out, errOut: c.errOut, err: c.err}
		a, _ := vendorProber(t, c.id, r, nil)
		err := a.Probe(bg, exe)
		wantProbeErr(t, err, c.reason)
		if strings.Contains(err.Error(), "SECRET") || strings.Contains(err.Error(), "update available") || strings.Contains(err.Error(), "Welcome") {
			t.Fatalf("%s: output leaked: %v", name, err)
		}
		if r.waits != 1 || r.starts != 1 || !slices.Equal(r.args, []string{"--version"}) {
			t.Fatalf("%s: %d starts, %d waits, args %q", name, r.starts, r.waits, r.args)
		}
	}
	// The path rules run before any start, never consulting PATH.
	for _, id := range []string{GrokID, CursorID} {
		r := &fakeRunner{out: grokV}
		a, _ := vendorProber(t, id, r, []string{"PATH=" + filepath.Dir(exe)})
		for p, reason := range map[string]string{
			"grok": "not absolute", "cursor-agent": "not absolute", "agent": "not absolute", "./grok": "not absolute", "": "not absolute",
			filepath.Join(t.TempDir(), "absent"): "does not exist", t.TempDir(): "not a regular file",
			exeFile(t, 0o644): "no executable permission bit",
		} {
			wantProbeErr(t, a.Probe(bg, p), reason)
		}
		if r.starts != 0 {
			t.Fatalf("%s: a refused path started %d probes", id, r.starts)
		}
		r.startErr = errors.New("exec format error")
		wantProbeErr(t, a.Probe(bg, exe), "cannot be executed")
	}
	// Cancellation wins before a start and while the child runs; the
	// deadline kills a child that never answers; the completion instant
	// decides at the deadline. Every child is waited exactly once.
	for _, id := range []string{GrokID, CursorID} {
		r := &fakeRunner{out: cursorV, block: true, blocking: make(chan struct{})}
		a, _ := vendorProber(t, id, r, nil)
		ctx, cancel := context.WithCancel(bg)
		cancel()
		if err := a.Probe(ctx, exe); !errors.Is(err, context.Canceled) || r.starts != 0 {
			t.Fatalf("%s canceled before start = %v (%d starts)", id, err, r.starts)
		}
		ctx, cancel = context.WithCancel(bg)
		done := make(chan error, 1)
		go func() { done <- a.Probe(ctx, exe) }()
		if err := r.clkAwait(); err != nil {
			t.Fatal(err)
		}
		cancel()
		if err := <-done; !errors.Is(err, context.Canceled) || r.waits != 1 {
			t.Fatalf("%s canceled while running = %v (%d waits)", id, err, r.waits)
		}
		r = &fakeRunner{out: cursorV, block: true, blocking: make(chan struct{})}
		a, _ = vendorProber(t, id, r, nil)
		go func() { done <- a.Probe(bg, exe) }()
		if err := r.clkAwait(); err != nil {
			t.Fatal(err)
		}
		r.clk.Advance(ProbeTimeout)
		wantProbeErr(t, <-done, "did not answer within 1s")
		if r.waits != 1 {
			t.Fatalf("%s deadline: %d waits", id, r.waits)
		}
		version := map[string]string{GrokID: grokV, CursorID: cursorV}[id]
		for advance, ok := range map[time.Duration]bool{ProbeTimeout: false, ProbeTimeout - time.Nanosecond: true} {
			r = &fakeRunner{out: version, advance: advance}
			a, _ = vendorProber(t, id, r, nil)
			err := a.Probe(bg, exe)
			if ok != (err == nil) {
				t.Fatalf("%s completion %v after the start: %v", id, advance, err)
			}
			if ws := r.clk.Waiters(); len(ws) != 0 || r.waits != 1 {
				t.Fatalf("%s timer left %v, waits %d", id, ws, r.waits)
			}
		}
	}
}

// grokForbidden are flags Grok's argv never carries (sandbox, approval
// bypass, trust, resume and alternate prompt transports).
var grokForbidden = []string{"--sandbox", "--always-approve", "--force", "-f", "--trust", "--yolo", "--auto-review", "--approve-mcps",
	"--resume", "--continue", "--worktree", "--prompt-file", "-", "--single", "--dangerously-skip-permissions", "--rules", "--system-prompt-override"}

// TestWave2VendorInvocation is UT FP-3 (iteration 11): Grok's exact argv
// with the composed prompt as its last element and an empty stdin, the
// 32 KiB policy and its ordered refusals, ownership and forbidden flags;
// Cursor always errors with a zero Invocation.
func TestWave2VendorInvocation(t *testing.T) {
	grok := NewGrok("")
	prompt := []byte("{\"format\":\"callsheet-task-v1\"}\n'quoted' \"double\" $(not a shell) \\n `tick` -- --model other\té 😀\n")
	inv, err := grok.Invocation(TaskInput{TaskID: taskID, Model: "grok-4.7", Effort: "low", Prompt: prompt, ScratchDir: "relative/ignored", FinalDir: "also ignored"})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"--output-format", "json", "--model", "grok-4.7", "--reasoning-effort", "low", "--permission-mode", "dontAsk", "-p", string(prompt)}
	if !slices.Equal(inv.Argv, want) || len(inv.Stdin) != 0 || inv.Stdin != nil || inv.FinalFile != "" {
		t.Fatalf("grok invocation %q stdin %q final %q", inv.Argv, inv.Stdin, inv.FinalFile)
	}
	// The last argument is exactly the composed prompt: no trimming, added
	// newline, quoting or splitting; nothing else carries prompt text.
	if last := inv.Argv[len(inv.Argv)-1]; last != string(prompt) || len(last) != len(prompt) {
		t.Fatalf("prompt element %q", last)
	}
	for _, a := range inv.Argv[:len(inv.Argv)-1] {
		if slices.Contains(grokForbidden, a) || strings.Contains(string(prompt), a) && len(a) > 8 {
			t.Fatalf("argv %q carries %q", inv.Argv, a)
		}
	}
	// Owned: neither the caller's prompt nor the returned argv aliases the
	// other or a later invocation.
	prompt[0] = 'X'
	if inv.Argv[9][0] != '{' {
		t.Fatal("the prompt argument aliases the caller's bytes")
	}
	inv.Argv[3] = "mutated"
	again, err := grok.Invocation(TaskInput{TaskID: taskID, Model: "grok-4.7", Effort: "low", Prompt: []byte("p")})
	if err != nil || again.Argv[3] != "grok-4.7" || again.Argv[9] != "p" {
		t.Fatalf("second invocation %q %v", again.Argv, err)
	}
	// The 32 KiB composed-prompt boundary (multi-byte runes included).
	for _, p := range [][]byte{bytes.Repeat([]byte("p"), MaxGrokPromptBytes), bytes.Repeat([]byte("é"), MaxGrokPromptBytes/2), []byte("x")} {
		if inv, err := grok.Invocation(TaskInput{TaskID: taskID, Model: "grok-4.7", Effort: "low", Prompt: p}); err != nil || inv.Argv[9] != string(p) {
			t.Fatalf("%d-byte prompt: %v", len(p), err)
		}
	}
	// Refusals, first match wins: empty, more than 32 KiB, invalid UTF-8,
	// NUL; each passed through errInvocation's prefix.
	over := bytes.Repeat([]byte("p"), MaxGrokPromptBytes+1)
	for name, c := range map[string]struct {
		prompt []byte
		reason string
	}{
		"empty":           {nil, grokPromptEmptyReason},
		"empty non-nil":   {[]byte{}, grokPromptEmptyReason},
		"one over":        {over, grokPromptTooLargeReason},
		"16 MiB":          {make([]byte, contract.MaxPromptBytes), grokPromptTooLargeReason},
		"over and bad":    {append(slices.Clone(over), 0xff), grokPromptTooLargeReason},
		"invalid utf8":    {[]byte("po\xffng"), grokPromptUTF8Reason},
		"cut rune":        {[]byte("pong\xe2\x82"), grokPromptUTF8Reason},
		"surrogate bytes": {[]byte("\xed\xa0\x80"), grokPromptUTF8Reason},
		"bad and nul":     {[]byte("\xff\x00"), grokPromptUTF8Reason},
		"nul":             {[]byte("po\x00ng"), grokPromptNULReason},
		"only nul":        {[]byte{0}, grokPromptNULReason},
	} {
		inv, err := grok.Invocation(TaskInput{TaskID: taskID, Model: "grok-4.7", Effort: "low", Prompt: c.prompt})
		if err == nil || err.Error() != "adapter: "+c.reason || inv.Argv != nil {
			t.Fatalf("%s: %q %v", name, inv.Argv, err)
		}
	}
	if grokPromptEmptyReason != "the grok prompt must not be empty" || grokPromptTooLargeReason != "the grok prompt exceeds 32 KiB" ||
		grokPromptUTF8Reason != "the grok prompt must be valid UTF-8" || grokPromptNULReason != "the grok prompt must not contain NUL" || MaxGrokPromptBytes != 32768 {
		t.Fatal("the grok prompt policy changed")
	}
	// Task ID, model grammar and selection are validated before the prompt.
	for name, c := range map[string]struct {
		in   TaskInput
		want string
	}{
		"task id":   {TaskInput{TaskID: "t_x", Model: "grok-4.7", Effort: "low"}, "adapter: invalid task ID"},
		"model":     {TaskInput{TaskID: taskID, Model: " ", Effort: "low"}, "adapter: invalid model"},
		"selection": {TaskInput{TaskID: taskID, Model: "grok-4.7-build", Effort: "low"}, "the grok model/effort selection is not qualified; supported: model grok-4.7, effort low"},
		"effort":    {TaskInput{TaskID: taskID, Model: "grok-4.7", Effort: "high"}, "the grok model/effort selection is not qualified"},
	} {
		if _, err := grok.Invocation(c.in); err == nil || !strings.HasPrefix(err.Error(), c.want) {
			t.Fatalf("%s: %v", name, err)
		}
	}
	// The real guardian encoder fits a maximum worst-case-escaped Grok
	// prompt with a normal fixture environment inside its 1 MiB frame; an
	// oversized environment is a definite encoding failure, never a
	// fallback to another transport.
	escaped := bytes.Repeat([]byte{0x01}, MaxGrokPromptBytes)
	ginv, err := grok.Invocation(TaskInput{TaskID: taskID, Model: "grok-4.7", Effort: "low", Prompt: escaped})
	if err != nil {
		t.Fatal(err)
	}
	g := contract.GuardianInvocation{Version: contract.GuardianInvocationVersion, TaskID: taskID,
		Execution: contract.ExecutionToken{Epoch: strings.Repeat("a", 32), Attachment: 1}, StartDigest: strings.Repeat("b", 64),
		TaskDir: "/state/tasks/" + taskID, Nonce: strings.Repeat("c", 64), Path: "/opt/vendor tools/grok", Argv: ginv.Argv,
		Env: []string{"PATH=/usr/local/bin:/usr/bin:/bin", "HOME=/home/worker", "LANG=C.UTF-8", "PWD=/state/tasks/" + taskID + "/work", "KEEP=1"},
		Dir: "/state/tasks/" + taskID + "/work", Timeout: "2h0m0s", TimeoutPolicy: contract.TimeoutPolicyEnforced}
	frame, err := contract.EncodeGuardianInvocation(g)
	if err != nil || len(frame) > contract.MaxGuardianInvocationBytes || len(frame) < 6*MaxGrokPromptBytes {
		t.Fatalf("maximum escaped prompt: %d bytes, %v", len(frame), err)
	}
	if back, err := contract.ParseGuardianInvocation(frame); err != nil || back.Argv[9] != string(escaped) {
		t.Fatalf("decoded frame: %v", err)
	}
	g.Env = append(g.Env, "BIG="+strings.Repeat("e", contract.MaxGuardianInvocationBytes))
	if _, err := contract.EncodeGuardianInvocation(g); err == nil {
		t.Fatal("an oversized guardian frame was encoded")
	}
	// The explicit vendor switch keeps Codex's own file-output branch (its
	// workspace final-output directory included) and never routes a new ID
	// there: Grok declares no final file whatever directories it is given.
	codex := NewCodex("")
	scratch, finalDir := "/state/tasks/"+taskID+"/work", "/state/tasks/"+taskID+"/work/.callsheet-runtime"
	cinv, err := codex.Invocation(TaskInput{TaskID: taskID, Model: "gpt-6.1-sol", Effort: "low", Prompt: []byte("p"), ScratchDir: scratch, FinalDir: finalDir})
	if err != nil || cinv.FinalFile != finalDir+"/"+CodexFinalName || cinv.Argv[10] != cinv.FinalFile {
		t.Fatalf("codex final-output directory %+v %v", cinv, err)
	}
	for _, bad := range []string{"relative/final", finalDir + "/", finalDir + "/../x"} {
		if _, err := codex.Invocation(TaskInput{TaskID: taskID, Model: "gpt-6.1-sol", Effort: "low", ScratchDir: scratch, FinalDir: bad}); err == nil ||
			err.Error() != "adapter: codex needs the task's absolute clean final-output directory" {
			t.Fatalf("codex final dir %q: %v", bad, err)
		}
	}
	if ginv, err := grok.Invocation(TaskInput{TaskID: taskID, Model: "grok-4.7", Effort: "low", Prompt: []byte("p"), ScratchDir: scratch, FinalDir: finalDir}); err != nil ||
		ginv.FinalFile != "" || slices.Contains(ginv.Argv, "--output-last-message") {
		t.Fatalf("grok with directories %+v %v", ginv, err)
	}
	// Cursor: a valid selection always errors with the fixed posture
	// refusal and a zero Invocation, whatever the prompt or directories; it
	// never builds a force/trust or sandbox-enabled argv.
	cursor := NewCursor("")
	for _, p := range [][]byte{nil, []byte("Reply with exactly the single word pong."), over, []byte("\xff\x00"), make([]byte, contract.MaxPromptBytes+1)} {
		inv, err := cursor.Invocation(TaskInput{TaskID: taskID, Model: "grok-4.7", Effort: "low", Prompt: p, ScratchDir: "/s", FinalDir: "/s/f"})
		if err == nil || err.Error() != cursorPostureText || inv.Argv != nil || inv.Stdin != nil || inv.FinalFile != "" {
			t.Fatalf("cursor %d-byte prompt: %+v %v", len(p), inv, err)
		}
	}
	for _, in := range []TaskInput{{TaskID: "t_x", Model: "grok-4.7", Effort: "low"}, {TaskID: taskID, Model: "grok-4.7-low", Effort: "low"}} {
		if inv, err := cursor.Invocation(in); err == nil || err.Error() == cursorPostureText || inv.Argv != nil {
			t.Fatalf("cursor invalid input %+v: %v", in, err)
		}
	}
}
