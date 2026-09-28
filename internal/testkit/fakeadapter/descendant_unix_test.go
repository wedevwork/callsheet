//go:build linux || darwin

package fakeadapter

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

const helperFailEnv = "FAKEADAPTER_TEST_FAIL"

func init() {
	// A helper asked to fail exits before announcing readiness.
	if os.Getenv(helperEnv) == "1" && os.Getenv(helperFailEnv) == "1" {
		os.Exit(3)
	}
}

func helperEnviron(extra ...string) func() []string {
	return func() []string {
		return append(append(os.Environ(), helperEnv+"=1"), extra...)
	}
}

func readReady(t *testing.T, p string) ReadyInfo {
	t.Helper()
	waitFile(t, p)
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	var info ReadyInfo
	if err := json.Unmarshal(b, &info); err != nil {
		t.Fatalf("ready %q: %v", b, err)
	}
	return info
}

func alive(pid int) bool { return syscall.Kill(pid, 0) == nil }

func TestDurationCompletionCleansUpDescendant(t *testing.T) {
	for _, mode := range []string{TermExit, TermIgnore} {
		t.Run(mode, func(t *testing.T) {
			dir := t.TempDir()
			after := &controlledAfter{duration: time.Hour, fire: make(chan time.Time, 1)}
			sigs := newFakeSignals()
			ch := startRun(t, context.Background(), Env{
				Args:       []string{"--duration=1h", "--spawn-grandchild", "--grandchild-term-mode=" + mode, "--ready-file=ready.json", "--signal-file=sig.jsonl", "--exit-code=3"},
				Dir:        dir,
				Signals:    sigs,
				After:      after.After,
				Executable: os.Args[0],
				Environ:    helperEnviron(),
			})
			<-sigs.registered
			info := readReady(t, filepath.Join(dir, "ready.json"))
			if info.DescendantPID == 0 || !alive(info.DescendantPID) {
				t.Fatalf("descendant not alive: %+v", info)
			}
			start := time.Now()
			after.fire <- time.Now()
			r := waitResult(t, ch)
			if r.code != 3 {
				t.Fatalf("code = %d stderr=%q", r.code, r.stderr)
			}
			if mode == TermIgnore && time.Since(start) < DescendantGrace {
				t.Fatalf("KILL escalation came before grace: %v", time.Since(start))
			}
			if alive(info.DescendantPID) {
				t.Fatal("descendant survived duration cleanup")
			}
			recs := readSignals(t, filepath.Join(dir, "sig.jsonl"+SignalFileSuffix))
			if len(recs) != 1 || recs[0].PID != info.DescendantPID || recs[0].Signal != "SIGTERM" {
				t.Fatalf("descendant signal records = %+v", recs)
			}
		})
	}
}

func TestSignalExitLeavesDescendantAlone(t *testing.T) {
	dir := t.TempDir()
	lr, lw, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer lr.Close()
	sigs := newFakeSignals()
	ch := startRun(t, context.Background(), Env{
		Args:       []string{"--duration=1h", "--spawn-grandchild", "--grandchild-term-mode=ignore", "--ready-file=ready.json"},
		Dir:        dir,
		Signals:    sigs,
		Executable: os.Args[0],
		Environ:    helperEnviron(),
		Getenv: func(k string) string {
			if k == EnvLifetimeFD {
				return strconv.Itoa(int(lw.Fd()))
			}
			return ""
		},
		NewFile: func(uintptr, string) *os.File { return lw },
	})
	c := <-sigs.registered
	info := readReady(t, filepath.Join(dir, "ready.json"))
	c <- syscall.SIGTERM
	if r := waitResult(t, ch); r.code != 0 {
		t.Fatalf("signal exit code = %d", r.code)
	}
	// The lifetime pipe stays open: the descendant was not signalled,
	// waited for or killed by the leader's signal exit.
	eof := make(chan error, 1)
	go func() {
		_, err := io.ReadAll(lr)
		eof <- err
	}()
	select {
	case <-eof:
		t.Fatal("descendant lifetime closed after leader signal exit")
	case <-time.After(300 * time.Millisecond):
	}
	if !alive(info.DescendantPID) {
		t.Fatal("descendant not alive")
	}
	// The supervising test owns the survivor.
	syscall.Kill(info.DescendantPID, syscall.SIGKILL)
	p, _ := os.FindProcess(info.DescendantPID)
	st, err := p.Wait()
	if err != nil {
		t.Fatal(err)
	}
	if ws, ok := st.Sys().(syscall.WaitStatus); !ok || ws.Signal() != syscall.SIGKILL {
		t.Fatalf("status = %v", st)
	}
	select {
	case <-eof:
	case <-time.After(5 * time.Second):
		t.Fatal("lifetime pipe not closed after descendant exit")
	}
}

func TestSpawnFailures(t *testing.T) {
	dir := t.TempDir()
	r := Run(context.Background(), Env{Args: []string{"--spawn-grandchild"}, Dir: dir, Signals: newFakeSignalsNoBlock(), Executable: filepath.Join(dir, "missing-exe")})
	if r != 1 {
		t.Fatalf("missing executable code = %d", r)
	}
	// Descendant exits without announcing readiness.
	r = Run(context.Background(), Env{Args: []string{"--spawn-grandchild"}, Dir: dir, Signals: newFakeSignalsNoBlock(), Executable: os.Args[0], Environ: helperEnviron(helperFailEnv + "=1")})
	if r != 1 {
		t.Fatalf("unready descendant code = %d", r)
	}
	// Ready publication failure kills and reaps the descendant.
	outside := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(dir, "link")); err != nil {
		t.Fatal(err)
	}
	r = Run(context.Background(), Env{Args: []string{"--spawn-grandchild", "--ready-file=link/r.json"}, Dir: dir, Signals: newFakeSignalsNoBlock(), Executable: os.Args[0], Environ: helperEnviron()})
	if r != 1 {
		t.Fatalf("ready failure code = %d", r)
	}
	var ws syscall.WaitStatus
	if _, err := syscall.Wait4(-1, &ws, syscall.WNOHANG, nil); !errors.Is(err, syscall.ECHILD) && err != nil {
		t.Fatalf("wait4: %v", err)
	}
}

func TestOSSignalsAndNames(t *testing.T) {
	c := make(chan os.Signal, 1)
	osSignals{}.Notify(c)
	osSignals{}.Stop(c)
	if signalName(syscall.SIGTERM) != "SIGTERM" || signalName(syscall.SIGINT) != "SIGINT" || signalName(syscall.SIGHUP) == "" {
		t.Fatal("signal names")
	}
}

// TestGroupMode is the iteration 06a group task mode, with a real
// descendant (this test binary as the fake): group.json names the leader
// and a live descendant in the leader's process group; the trigger file
// completes the leader with its PID in the output; the descendant is left
// to the supervisor (killed and reaped here). Invalid settings exit 2
// before any spawn.
func TestGroupMode(t *testing.T) {
	args := []string{TaskFlag, "--model", "example model", "--effort", "medium"}
	good := prompt("example model", "medium")
	run := func(vars map[string]string, stdout io.Writer) int {
		var errOut strings.Builder
		return Run(context.Background(), Env{Args: args, Stdout: stdout, Stderr: &errOut, Stdin: strings.NewReader(string(good)),
			Getenv:     func(k string) string { return vars[k] },
			Executable: os.Args[0], Environ: helperEnviron()})
	}
	for name, vars := range map[string]map[string]string{
		"relative dir": {EnvTaskMode: "group", EnvTaskGroupDir: "rel"},
		"term":         {EnvTaskMode: "group", EnvTaskGroupDir: t.TempDir(), EnvTaskDescendantTerm: "maybe"},
	} {
		if code := run(vars, io.Discard); code != 2 {
			t.Fatalf("%s = %d", name, code)
		}
	}
	// The group file cannot be published: the descendant is killed and
	// reaped, exit 1.
	if code := run(map[string]string{EnvTaskMode: "group", EnvTaskGroupDir: filepath.Join(t.TempDir(), "missing")}, io.Discard); code != 1 {
		t.Fatalf("unpublished group = %d", code)
	}
	dir := t.TempDir()
	var out strings.Builder
	done := make(chan int, 1)
	go func() {
		done <- run(map[string]string{EnvTaskMode: "group", EnvTaskGroupDir: dir, EnvTaskDescendantTerm: TermIgnore}, &out)
	}()
	waitFile(t, filepath.Join(dir, GroupFile))
	b, err := os.ReadFile(filepath.Join(dir, GroupFile))
	if err != nil {
		t.Fatal(err)
	}
	var info GroupInfo
	if err := json.Unmarshal(b, &info); err != nil || info.LeaderPID != os.Getpid() || !alive(info.DescendantPID) {
		t.Fatalf("group %s %v", b, err)
	}
	if pg, err := syscall.Getpgid(info.DescendantPID); err != nil || pg != syscall.Getpgrp() {
		t.Fatalf("descendant group %d %v", pg, err)
	}
	os.WriteFile(filepath.Join(dir, GroupTrigger), nil, 0o644)
	select {
	case code := <-done:
		if code != 0 || out.String() != "native started pid="+strconv.Itoa(os.Getpid())+"\nnative output pid="+strconv.Itoa(os.Getpid())+"\n"+FinalMarker {
			t.Fatalf("group = %d %q", code, out.String())
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the group leader did not complete")
	}
	syscall.Kill(info.DescendantPID, syscall.SIGKILL)
	p, _ := os.FindProcess(info.DescendantPID)
	p.Wait()
}
