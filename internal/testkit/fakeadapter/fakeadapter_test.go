package fakeadapter

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

const helperEnv = "FAKEADAPTER_TEST_HELPER"

// TestMain lets this test binary act as the fake executable for descendant
// spawns, so descendant code paths run as real processes.
func TestMain(m *testing.M) {
	if os.Getenv(helperEnv) == "1" {
		os.Exit(Run(context.Background(), Env{Args: os.Args[1:], Stdout: os.Stdout, Stderr: os.Stderr}))
	}
	os.Exit(m.Run())
}

// fakeSignals lets a test deliver signals to an in-process Run.
type fakeSignals struct {
	registered chan chan<- os.Signal
	stopped    chan struct{}
}

func newFakeSignals() *fakeSignals {
	return &fakeSignals{registered: make(chan chan<- os.Signal, 1), stopped: make(chan struct{}, 1)}
}
func (f *fakeSignals) Notify(c chan<- os.Signal) { f.registered <- c }
func (f *fakeSignals) Stop(chan<- os.Signal)     { f.stopped <- struct{}{} }

// controlledAfter returns a manual channel for the fake's run duration and a
// real timer for everything else.
type controlledAfter struct {
	duration time.Duration
	fire     chan time.Time
}

func (c *controlledAfter) After(d time.Duration) <-chan time.Time {
	if d == c.duration {
		return c.fire
	}
	return time.After(d)
}

type result struct {
	code           int
	stdout, stderr string
}

func startRun(t *testing.T, ctx context.Context, env Env) <-chan result {
	t.Helper()
	ch := make(chan result, 1)
	var out, errOut bytes.Buffer
	env.Stdout, env.Stderr = &out, &errOut
	go func() {
		code := Run(ctx, env)
		ch <- result{code, out.String(), errOut.String()}
	}()
	return ch
}

func waitResult(t *testing.T, ch <-chan result) result {
	t.Helper()
	select {
	case r := <-ch:
		return r
	case <-time.After(20 * time.Second):
		t.Fatal("Run did not return")
		return result{}
	}
}

func TestParseValidation(t *testing.T) {
	bad := [][]string{
		{"--bogus"},
		{"--duration"},
		{"--duration=abc"},
		{"--duration=-1s"},
		{"--exit-code=126"},
		{"--exit-code=-1"},
		{"--exit-code=x"},
		{"--term-mode=stop"},
		{"--grandchild-term-mode=stop"},
		{"--edit=/abs/file"},
		{"--edit=../x"},
		{"--edit=a/../../x"},
		{"--edit=."},
		{"--ready-file=../r"},
		{"--signal-file=/tmp/s"},
		{"--content=x"},
		{"--internal-descendant", "--spawn-grandchild"},
	}
	for _, args := range bad {
		if _, err := Parse(args); err == nil {
			t.Errorf("Parse(%v) succeeded", args)
		} else if _, ok := err.(*UsageError); !ok {
			t.Errorf("Parse(%v) error type %T", args, err)
		}
	}
	o, err := Parse([]string{"--model", "m 1", "--effort=high", "--duration=1.5s", "--exit-code=125", "--edit=a/b.txt", "--content=c", "--", "--stdout", "x"})
	if err != nil {
		t.Fatal(err)
	}
	if o.Model != "m 1" || o.Effort != "high" || o.Duration != 1500*time.Millisecond || o.ExitCode != 125 || o.Edit != "a/b.txt" || strings.Join(o.Rest, ",") != "--stdout,x" || o.stdoutSet {
		t.Fatalf("opts = %+v", o)
	}
	if o.TermMode != TermExit || o.GrandchildTermMode != TermExit {
		t.Fatal("defaults")
	}
}

func TestOutputEncodingAndEdit(t *testing.T) {
	dir := t.TempDir()
	args := []string{"--echo-argv", "--stdout=你好", "--stderr=diagnostic", "--edit=sub/dir/r.txt", "--content=héllo\n\"q\"", "--exit-code=17", "--", `a "b" c`, "<&>"}
	var out, errOut bytes.Buffer
	code := Run(context.Background(), Env{Args: args, Stdout: &out, Stderr: &errOut, Dir: dir, Signals: newFakeSignalsNoBlock()})
	if code != 17 {
		t.Fatalf("code = %d stderr=%q", code, errOut.String())
	}
	argvJSON, _ := marshalNoEscape(map[string][]string{"argv": args})
	want := string(argvJSON) + "\n你好\n"
	if out.String() != want {
		t.Fatalf("stdout = %q want %q", out.String(), want)
	}
	var decoded struct{ Argv []string }
	json.Unmarshal([]byte(strings.SplitN(out.String(), "\n", 2)[0]), &decoded)
	if strings.Join(decoded.Argv, "|") != strings.Join(args, "|") {
		t.Fatalf("argv round trip = %v", decoded.Argv)
	}
	if errOut.String() != "diagnostic\n" {
		t.Fatalf("stderr = %q", errOut.String())
	}
	b, err := os.ReadFile(filepath.Join(dir, "sub", "dir", "r.txt"))
	if err != nil || string(b) != "héllo\n\"q\"" {
		t.Fatalf("edit = %q %v", b, err)
	}
	// Replace existing content.
	Run(context.Background(), Env{Args: []string{"--edit=sub/dir/r.txt", "--content=new"}, Dir: dir, Signals: newFakeSignalsNoBlock()})
	b, _ = os.ReadFile(filepath.Join(dir, "sub", "dir", "r.txt"))
	if string(b) != "new" {
		t.Fatalf("replace = %q", b)
	}
	// Empty argv echo is an empty array.
	out.Reset()
	Run(context.Background(), Env{Args: nil, Stdout: &out, Dir: dir, Signals: newFakeSignalsNoBlock()})
	if out.String() != "" {
		t.Fatalf("no-op out = %q", out.String())
	}
	b, _ = marshalNoEscape(map[string][]string{"argv": nonNil(nil)})
	if string(b) != `{"argv":[]}` {
		t.Fatalf("empty argv = %s", b)
	}
}

// newFakeSignalsNoBlock returns a signal source whose Notify never blocks.
func newFakeSignalsNoBlock() *fakeSignals {
	return &fakeSignals{registered: make(chan chan<- os.Signal, 16), stopped: make(chan struct{}, 16)}
}

func TestOptionErrorHasNoSideEffects(t *testing.T) {
	dir := t.TempDir()
	var out, errOut bytes.Buffer
	code := Run(context.Background(), Env{Args: []string{"--stdout=x", "--edit=f.txt", "--content=c", "--exit-code=200"}, Stdout: &out, Stderr: &errOut, Dir: dir})
	if code != 2 || out.Len() != 0 || !strings.Contains(errOut.String(), "exit-code") {
		t.Fatalf("code=%d out=%q err=%q", code, out.String(), errOut.String())
	}
	entries, _ := os.ReadDir(dir)
	if len(entries) != 0 {
		t.Fatalf("side effects: %v", entries)
	}
	// Nil stderr on usage error must not panic.
	if Run(context.Background(), Env{Args: []string{"--bogus"}}) != 2 {
		t.Fatal("usage error code")
	}
}

func TestFileIOFailures(t *testing.T) {
	dir := t.TempDir()
	outside := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(dir, "link")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	var errOut bytes.Buffer
	code := Run(context.Background(), Env{Args: []string{"--edit=link/escape.txt", "--content=x"}, Stderr: &errOut, Dir: dir, Signals: newFakeSignalsNoBlock()})
	if code != 1 || !strings.Contains(errOut.String(), "edit") {
		t.Fatalf("symlink escape code=%d err=%q", code, errOut.String())
	}
	if _, err := os.Stat(filepath.Join(outside, "escape.txt")); !os.IsNotExist(err) {
		t.Fatal("symlink escape wrote outside cwd")
	}
	// Ready file through the escaping symlink fails with 1.
	errOut.Reset()
	code = Run(context.Background(), Env{Args: []string{"--ready-file=link/ready.json"}, Stderr: &errOut, Dir: dir, Signals: newFakeSignalsNoBlock()})
	if code != 1 || !strings.Contains(errOut.String(), "ready file") {
		t.Fatalf("ready escape code=%d err=%q", code, errOut.String())
	}
	// Missing working directory.
	code = Run(context.Background(), Env{Args: nil, Stderr: &errOut, Dir: filepath.Join(dir, "missing")})
	if code != 1 {
		t.Fatalf("missing dir code=%d", code)
	}
	// Parent path component is a file.
	os.WriteFile(filepath.Join(dir, "file"), []byte("x"), 0o644)
	code = Run(context.Background(), Env{Args: []string{"--edit=file/x.txt"}, Stderr: &errOut, Dir: dir, Signals: newFakeSignalsNoBlock()})
	if code != 1 {
		t.Fatalf("file-as-dir code=%d", code)
	}
	// Signal file write failure is reported but does not change the outcome.
	sigs := newFakeSignals()
	errOut.Reset()
	ch := startRunWithErr(t, Env{Args: []string{"--signal-file=file/s.jsonl", "--duration=1h"}, Dir: dir, Signals: sigs}, &errOut)
	(<-sigs.registered) <- os.Interrupt
	if r := waitResult(t, ch); r.code != 0 || !strings.Contains(r.stderr, "signal file") {
		t.Fatalf("signal file failure: %+v", r)
	}
}

func startRunWithErr(t *testing.T, env Env, _ *bytes.Buffer) <-chan result {
	return startRun(t, context.Background(), env)
}

func TestDurationAndCancel(t *testing.T) {
	dir := t.TempDir()
	after := &controlledAfter{duration: time.Hour, fire: make(chan time.Time, 1)}
	sigs := newFakeSignals()
	ch := startRun(t, context.Background(), Env{Args: []string{"--duration=1h", "--exit-code=9", "--ready-file=r/ready.json"}, Dir: dir, Signals: sigs, After: after.After, PID: 4242})
	<-sigs.registered
	waitFile(t, filepath.Join(dir, "r", "ready.json"))
	var info ReadyInfo
	b, _ := os.ReadFile(filepath.Join(dir, "r", "ready.json"))
	if err := json.Unmarshal(b, &info); err != nil || info.PID != 4242 || info.DescendantPID != 0 {
		t.Fatalf("ready = %s %v", b, err)
	}
	select {
	case r := <-ch:
		t.Fatalf("returned before duration: %+v", r)
	case <-time.After(50 * time.Millisecond):
	}
	after.fire <- time.Now()
	if r := waitResult(t, ch); r.code != 9 {
		t.Fatalf("code = %d", r.code)
	}
	// Ready temp files are not left behind.
	entries, _ := os.ReadDir(filepath.Join(dir, "r"))
	if len(entries) != 1 {
		t.Fatalf("ready dir = %v", entries)
	}

	// Context cancellation.
	ctx, cancel := context.WithCancel(context.Background())
	ch = startRun(t, ctx, Env{Args: []string{"--duration=1h"}, Dir: dir, Signals: newFakeSignals(), After: after.After})
	cancel()
	if r := waitResult(t, ch); r.code != 1 || !strings.Contains(r.stderr, "cancelled") {
		t.Fatalf("cancel = %+v", r)
	}

	// Zero duration returns promptly with the exit code.
	start := time.Now()
	if code := Run(context.Background(), Env{Args: []string{"--duration=0s", "--exit-code=4"}, Dir: dir, Signals: newFakeSignalsNoBlock()}); code != 4 {
		t.Fatalf("zero duration code = %d", code)
	}
	if time.Since(start) > 2*time.Second {
		t.Fatal("zero duration too slow")
	}
}

func TestSignalModes(t *testing.T) {
	dir := t.TempDir()
	after := &controlledAfter{duration: time.Hour, fire: make(chan time.Time, 1)}

	// exit mode: record the signal, exit 0.
	sigs := newFakeSignals()
	ch := startRun(t, context.Background(), Env{Args: []string{"--duration=1h", "--signal-file=s/sig.jsonl", "--exit-code=5"}, Dir: dir, Signals: sigs, After: after.After, PID: 77})
	c := <-sigs.registered
	c <- os.Interrupt
	r := waitResult(t, ch)
	if r.code != 0 || !strings.Contains(r.stderr, "received") {
		t.Fatalf("exit mode = %+v", r)
	}
	recs := readSignals(t, filepath.Join(dir, "s", "sig.jsonl"))
	if len(recs) != 1 || recs[0].PID != 77 {
		t.Fatalf("signal records = %+v", recs)
	}

	// ignore mode: record each signal and keep running until duration.
	sigs = newFakeSignals()
	ch = startRun(t, context.Background(), Env{Args: []string{"--duration=1h", "--term-mode=ignore", "--signal-file=s/ign.jsonl", "--exit-code=6"}, Dir: dir, Signals: sigs, After: after.After, PID: 78})
	c = <-sigs.registered
	c <- os.Interrupt
	c <- os.Interrupt
	waitLines(t, filepath.Join(dir, "s", "ign.jsonl"), 2)
	select {
	case r := <-ch:
		t.Fatalf("ignore mode exited: %+v", r)
	case <-time.After(50 * time.Millisecond):
	}
	after.fire <- time.Now()
	if r := waitResult(t, ch); r.code != 6 {
		t.Fatalf("ignore mode code = %d", r.code)
	}
	if len(readSignals(t, filepath.Join(dir, "s", "ign.jsonl"))) != 2 {
		t.Fatal("expected two records")
	}
	select {
	case <-sigs.stopped:
	default:
		t.Fatal("handlers not stopped")
	}
}

func TestInheritedFileParsing(t *testing.T) {
	env := Env{Getenv: func(k string) string {
		return map[string]string{"A": "x", "B": "2", "C": "7"}[k]
	}, NewFile: func(fd uintptr, name string) *os.File { return os.NewFile(fd, name) }}
	if env.inheritedFile("A", "a") != nil || env.inheritedFile("B", "b") != nil || env.inheritedFile("Z", "z") != nil {
		t.Fatal("invalid fds must be ignored")
	}
	var got uintptr
	env.NewFile = func(fd uintptr, name string) *os.File { got = fd; return nil }
	env.inheritedFile("C", "c")
	if got != 7 {
		t.Fatalf("fd = %d", got)
	}
	f := filteredEnv([]string{"X=1", EnvLifetimeFD + "=3", envReadyFD + "=3", "Y=2"})
	if strings.Join(f, ",") != "X=1,Y=2" {
		t.Fatalf("filtered = %v", f)
	}
	if runtime.GOOS != "windows" && !signalsSupported {
		t.Fatal("signals must be supported on unix")
	}
}

func TestEnvDefaults(t *testing.T) {
	e, err := Env{}.withDefaults()
	if err != nil {
		t.Fatal(err)
	}
	if e.Stdout == nil || e.Stderr == nil || e.Dir == "" || e.Executable == "" || e.PID != os.Getpid() || e.After == nil || e.NewFile == nil || e.Environ == nil || e.Getenv == nil || e.Signals == nil {
		t.Fatalf("defaults = %+v", e)
	}
}

func waitFile(t *testing.T, p string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(p); err == nil {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("file %s did not appear", p)
}

func waitLines(t *testing.T, p string, n int) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		b, _ := os.ReadFile(p)
		if bytes.Count(b, []byte("\n")) >= n {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("%s did not reach %d lines", p, n)
}

func readSignals(t *testing.T, p string) []SignalRecord {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	var out []SignalRecord
	for _, line := range strings.Split(strings.TrimSpace(string(b)), "\n") {
		if line == "" {
			continue
		}
		var r SignalRecord
		if err := json.Unmarshal([]byte(line), &r); err != nil {
			t.Fatalf("bad line %q", line)
		}
		out = append(out, r)
	}
	return out
}
