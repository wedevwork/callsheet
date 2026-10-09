//go:build linux || darwin

package procexec

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/wedevwork/callsheet/internal/mcpqual"
	"github.com/wedevwork/callsheet/internal/spikes/processgroup"
)

// The helper process modes (this test binary re-executed).
const helperEnv = "PROCEXEC_HELPER"

// barrierReleased is the barrier helper's exit code after its release.
const barrierReleased = 4

func TestMain(m *testing.M) {
	switch os.Getenv(helperEnv) {
	case "":
		os.Exit(m.Run())
	case "echo":
		fmt.Printf("pgrp %d pid %d\n", syscall.Getpgrp(), os.Getpid())
		fmt.Fprintln(os.Stderr, "to stderr")
		os.Exit(3)
	case "term-self":
		syscall.Kill(os.Getpid(), syscall.SIGTERM)
		time.Sleep(time.Minute)
	case "parent-exits-first":
		// A descendant keeps stdout open after the leader exits.
		d := exec.Command(os.Args[0])
		d.Env = append(os.Environ(), helperEnv+"=hold")
		d.Stdout = os.Stdout
		d.Start()
		fmt.Printf("descendant %d\n", d.Process.Pid)
		os.Exit(0)
	case "hold":
		time.Sleep(time.Minute)
	case "stderr-held":
		// A descendant keeps stderr (only) open after the leader exits.
		d := exec.Command(os.Args[0])
		d.Env = append(os.Environ(), helperEnv+"=hold")
		d.Stderr = os.Stderr
		d.Start()
		fmt.Printf("descendant %d\n", d.Process.Pid)
		fmt.Fprintln(os.Stderr, "leader stderr")
		os.Exit(6) // nonzero: no race-runtime exit sleep
	case "marker":
		os.WriteFile(os.Getenv("PROCEXEC_MARKER"), nil, 0o600)
		os.Exit(5)
	case "barrier":
		// Blocks until the test writes the FIFO's one release byte, then
		// exits barrierReleased; a barrier that cannot be opened, or that
		// ends without its byte, exits 2. The release is a byte, never the
		// writer's close: Darwin may not end a FIFO read on a zero-byte
		// close. (A nonzero code also skips the race runtime's exit sleep.)
		f, err := os.Open(os.Getenv("PROCEXEC_BARRIER"))
		if err != nil {
			os.Exit(2)
		}
		if _, err := io.ReadFull(f, make([]byte, 1)); err != nil {
			os.Exit(2)
		}
		f.Close()
		os.Exit(barrierReleased)
	}
}

func spec(t *testing.T, mode string) mcpqual.ProcSpec {
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	return mcpqual.ProcSpec{Path: exe, Env: []string{helperEnv + "=" + mode}, Dir: t.TempDir(), StderrPath: filepath.Join(t.TempDir(), "stderr")}
}

// reapWait bounds every wait on a helper: its reaping and, in
// TestLauncherStatusPending, its barrier's release.
const reapWait = 30 * time.Second

func waitExited(t *testing.T, p mcpqual.Proc) {
	t.Helper()
	select {
	case <-p.Exited():
	case <-time.After(reapWait):
		t.Fatal("the leader was not reaped")
	}
}

func TestLauncherExitStdoutStderr(t *testing.T) {
	s := spec(t, "echo")
	p, err := Launcher{}.Start(s)
	if err != nil {
		t.Fatal(err)
	}
	// The child may already have exited and been reaped here; only the
	// final status is checked (TestLauncherStatusPending covers the pending
	// state with a barrier the child cannot pass).
	out, err := io.ReadAll(p.Stdout())
	if err != nil {
		t.Fatal(err)
	}
	waitExited(t, p)
	p.CloseStdout()
	// The launch leads its own group: its process group is its PID.
	if want := fmt.Sprintf("pgrp %d pid %d\n", p.PGID(), p.PGID()); string(out) != want {
		t.Fatalf("stdout %q, want %q", out, want)
	}
	if code, sig := p.Status(); code == nil || *code != 3 || sig != nil {
		t.Fatalf("status %v %v", code, sig)
	}
	// A coverage-instrumented helper may append its own GOCOVERDIR warning.
	if b, _ := os.ReadFile(s.StderrPath); !strings.HasPrefix(string(b), "to stderr\n") {
		t.Fatalf("stderr %q", b)
	}
}

func TestLauncherSignalStatus(t *testing.T) {
	s := spec(t, "term-self")
	s.StderrPath = ""
	p, err := Launcher{}.Start(s)
	if err != nil {
		t.Fatal(err)
	}
	waitExited(t, p)
	if code, sig := p.Status(); code != nil || sig == nil || *sig != syscall.SIGTERM.String() {
		t.Fatalf("status %v %v", code, sig)
	}
	p.CloseStdout()
}

// The leader's reaping never waits for a descendant holding stdout; the
// group is then stopped and proven gone.
func TestLauncherParentExitsFirst(t *testing.T) {
	p, err := Launcher{}.Start(spec(t, "parent-exits-first"))
	if err != nil {
		t.Fatal(err)
	}
	line, err := bufio.NewReader(p.Stdout()).ReadString('\n')
	if err != nil || !strings.HasPrefix(line, "descendant ") {
		t.Fatalf("%q %v", line, err)
	}
	desc, _ := strconv.Atoi(strings.TrimSpace(strings.TrimPrefix(line, "descendant ")))
	waitExited(t, p)
	if alive, err := processgroup.Existence(processgroup.SysSignaler{}, -p.PGID()); err != nil || !alive {
		t.Fatalf("the descendant's group is gone already (%v, %v)", alive, err)
	}
	syscall.Kill(-p.PGID(), syscall.SIGKILL)
	if err := processgroup.WaitGone(processgroup.SysSignaler{}, processgroup.RealClock{}, 10*time.Second, 10*time.Millisecond, -p.PGID(), desc); err != nil {
		t.Fatal(err)
	}
	p.CloseStdout()
}

func TestLauncherStartErrors(t *testing.T) {
	_, err := Launcher{}.Start(mcpqual.ProcSpec{Path: filepath.Join(t.TempDir(), "missing")})
	if !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("missing executable: %v", err)
	}
	s := spec(t, "echo")
	s.StderrPath = filepath.Join(t.TempDir(), "no", "such", "dir", "stderr")
	if _, err := (Launcher{}).Start(s); err == nil {
		t.Fatal("an unwritable stderr path was accepted")
	}
}

// Status stays pending while the leader cannot exit: the child blocks on a
// FIFO until the test releases it, so the ordering is established by the
// barrier, not by scheduling.
func TestLauncherStatusPending(t *testing.T) {
	fifo := filepath.Join(t.TempDir(), "barrier")
	if err := syscall.Mkfifo(fifo, 0o600); err != nil {
		t.Fatal(err)
	}
	s := spec(t, "barrier")
	s.Env = append(s.Env, "PROCEXEC_BARRIER="+fifo)
	p, err := Launcher{}.Start(s)
	if err != nil {
		t.Fatal(err)
	}
	writerDone := make(chan struct{})
	writerStarted := false
	// On every path, a fatal one included: a leader still alive is killed
	// and reaped, a writer still blocked in its open is let through, and
	// stdout is closed, each wait bounded by reapWait.
	t.Cleanup(func() {
		select {
		case <-p.Exited():
		default:
			syscall.Kill(-p.PGID(), syscall.SIGKILL)
			select {
			case <-p.Exited():
			case <-time.After(reapWait):
				t.Error("the leader was not reaped after SIGKILL")
			}
		}
		if writerStarted {
			unblockWriter(t, fifo, writerDone)
		}
		p.CloseStdout()
	})
	select {
	case <-p.Exited():
		t.Fatal("the leader exited before its barrier was released")
	default:
	}
	if code, sig := p.Status(); code != nil || sig != nil {
		t.Fatal("a status before the leader was reaped")
	}
	// Opening for writing blocks until the child has opened for reading;
	// the one byte written then releases it (it stays buffered if the
	// child's read has not started). Exited may be ready together with the
	// release, or alone when the helper never reached its barrier: a reader
	// of our own lets a pending open complete, and the final status
	// (barrierReleased only after the byte) tells the two apart, so a dead
	// helper cannot hang the test. A live helper that never opens its
	// barrier fails the test after reapWait; the cleanup then kills it and
	// lets the writer through.
	opened := make(chan error, 1)
	writerStarted = true
	go func() {
		defer close(writerDone)
		w, err := os.OpenFile(fifo, os.O_WRONLY, 0)
		if err == nil {
			_, err = w.Write([]byte{1})
			if cerr := w.Close(); err == nil {
				err = cerr
			}
		}
		opened <- err
	}()
	select {
	case err := <-opened:
		if err != nil {
			t.Fatal(err)
		}
	case <-p.Exited():
		unblockWriter(t, fifo, writerDone)
		select {
		case err := <-opened:
			if err != nil {
				t.Fatal(err)
			}
		default:
			t.FailNow() // unblockWriter reported why
		}
	case <-time.After(reapWait):
		t.Fatal("the barrier was not released: the helper did not open it")
	}
	waitExited(t, p)
	if code, sig := p.Status(); code == nil || *code != barrierReleased || sig != nil {
		t.Fatalf("status %s, want exit %d after the barrier's release (2: the helper could not open or read it)", statusText(code, sig), barrierReleased)
	}
}

// unblockWriter lets a barrier writer still blocked in its FIFO open
// complete, with a nonblocking reader of our own held until the writer is
// done, and waits for it within reapWait.
func unblockWriter(t *testing.T, fifo string, done <-chan struct{}) {
	t.Helper()
	select {
	case <-done:
		return
	default:
	}
	r, err := os.OpenFile(fifo, os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		t.Error(err)
		return
	}
	defer r.Close()
	select {
	case <-done:
	case <-time.After(reapWait):
		t.Error("the barrier writer did not finish")
	}
}

func statusText(code *int, sig *string) string {
	switch {
	case code != nil:
		return "exit " + strconv.Itoa(*code)
	case sig != nil:
		return "signal " + *sig
	}
	return "pending"
}

// Design decoder-enrollment: the opt-in captured stderr is a second
// harness-owned OS pipe, read concurrently with stdout.
func TestLauncherCaptureStderr(t *testing.T) {
	s := spec(t, "echo")
	s.StderrPath, s.CaptureStderr = "", true
	p, err := Launcher{}.Start(s)
	if err != nil {
		t.Fatal(err)
	}
	sp, ok := p.(mcpqual.StderrProc)
	if !ok {
		t.Fatal("a CaptureStderr launch is not a StderrProc")
	}
	errOut := make(chan []byte, 1)
	go func() { b, _ := io.ReadAll(sp.Stderr()); errOut <- b }()
	out, err := io.ReadAll(p.Stdout())
	if err != nil {
		t.Fatal(err)
	}
	waitExited(t, p)
	var stderr []byte
	select {
	case stderr = <-errOut:
	case <-time.After(30 * time.Second):
		t.Fatal("stderr did not end")
	}
	p.CloseStdout()
	sp.CloseStderr()
	if !strings.HasPrefix(string(out), "pgrp ") || !strings.HasPrefix(string(stderr), "to stderr\n") {
		t.Fatalf("stdout %q stderr %q", out, stderr)
	}
	// A launch without CaptureStderr keeps its file interface.
	if _, ok := mustStart(t, spec(t, "echo")).(mcpqual.StderrProc); ok {
		t.Fatal("a file-stderr launch exposes a stderr pipe")
	}
}

func mustStart(t *testing.T, s mcpqual.ProcSpec) mcpqual.Proc {
	t.Helper()
	p, err := Launcher{}.Start(s)
	if err != nil {
		t.Fatal(err)
	}
	io.Copy(io.Discard, p.Stdout())
	waitExited(t, p)
	p.CloseStdout()
	return p
}

// Both a stderr file and a captured stderr are refused before any process
// starts.
func TestLauncherStderrConflict(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "started")
	s := spec(t, "marker")
	s.Env = append(s.Env, "PROCEXEC_MARKER="+marker)
	s.CaptureStderr = true
	if _, err := (Launcher{}).Start(s); !errors.Is(err, mcpqual.ErrStderrConflict) {
		t.Fatalf("conflict: %v", err)
	}
	if _, err := os.Stat(marker); !errors.Is(err, fs.ErrNotExist) {
		t.Fatal("a process started despite the conflict")
	}
	s.CaptureStderr = false
	mustStart(t, s)
	if _, err := os.Stat(marker); err != nil {
		t.Fatal("the marker helper did not run")
	}
	// A failed launch closes both pipes' ends and reports the error.
	s = spec(t, "echo")
	s.Path, s.StderrPath, s.CaptureStderr = filepath.Join(t.TempDir(), "missing"), "", true
	if _, err := (Launcher{}).Start(s); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("missing executable: %v", err)
	}
}

// A descendant holding the captured stderr never holds the leader's Wait;
// stopping the group ends the stream.
func TestLauncherStderrHeld(t *testing.T) {
	s := spec(t, "stderr-held")
	s.StderrPath, s.CaptureStderr = "", true
	p, err := Launcher{}.Start(s)
	if err != nil {
		t.Fatal(err)
	}
	sp := p.(mcpqual.StderrProc)
	line, err := bufio.NewReader(p.Stdout()).ReadString('\n')
	if err != nil || !strings.HasPrefix(line, "descendant ") {
		t.Fatalf("%q %v", line, err)
	}
	desc, _ := strconv.Atoi(strings.TrimSpace(strings.TrimPrefix(line, "descendant ")))
	waitExited(t, p)
	br := bufio.NewReader(sp.Stderr())
	if l, err := br.ReadString('\n'); err != nil || l != "leader stderr\n" {
		t.Fatalf("stderr %q %v", l, err)
	}
	syscall.Kill(-p.PGID(), syscall.SIGKILL)
	if err := processgroup.WaitGone(processgroup.SysSignaler{}, processgroup.RealClock{}, 10*time.Second, 10*time.Millisecond, -p.PGID(), desc); err != nil {
		t.Fatal(err)
	}
	// With the group gone the stream ends: reading to EOF returns.
	if _, err := io.ReadAll(br); err != nil {
		t.Fatal(err)
	}
	p.CloseStdout()
	sp.CloseStderr()
}
