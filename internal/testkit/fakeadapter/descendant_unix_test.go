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
