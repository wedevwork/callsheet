//go:build linux || darwin

package sidecar

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

// TestGuardianCommandFIFOJoin (defect fix, PR #18 on macOS): the
// guardian's ordinary return must unblock and join readCommands however
// the control FIFO's read end is adopted. Darwin's Go runtime keeps a FIFO
// out of kqueue, so the read blocks in the kernel and closing the read end
// does not wake it; "blocking" forces that state on every host (adopted
// through os.NewFile with O_NONBLOCK cleared, the guardian's own
// adoption). "pollable" is Linux's poller state (epoll wakes the read on
// Close or EOF). A transient writer (sendCommand) still delivers while the
// guardian's held write end is open. Not parallel, subtests in turn: the
// reader is identified among this process's goroutines (readerParked), so
// no other test's guardian may be reading a command FIFO meanwhile.
func TestGuardianCommandFIFOJoin(t *testing.T) {
	modes := []struct {
		name  string
		adopt func(fd int, name string) *os.File
		state string
	}{{"blocking", adoptBlocking, "syscall"}}
	if runtime.GOOS == "linux" {
		modes = append(modes, struct {
			name  string
			adopt func(fd int, name string) *os.File
			state string
		}{"pollable", func(fd int, name string) *os.File { return os.NewFile(uintptr(fd), name) }, "IO wait"})
	}
	for _, m := range modes {
		t.Run(m.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), controlName)
			if err := unix.Mkfifo(path, 0o600); err != nil {
				t.Fatal(err)
			}
			ctl, err := openControlFIFO(path, m.adopt)
			if err != nil {
				t.Fatal(err)
			}
			diag := newSyncLog()
			g := &guardian{diag: diag}
			done := make(chan struct{})
			go func() {
				defer close(done)
				g.readCommands(ctl.r)
			}()
			// A transient writer's command reaches the parked reader, which
			// then reads on: observed parked in that next Read.
			if err := sendCommand(path, []byte("not a command\n")); err != nil {
				t.Fatal(err)
			}
			diag.await(t, "the ignored command", func(s string) bool { return strings.Contains(s, "was ignored") })
			readerParked(t, m.state)
			select {
			case <-done:
				t.Fatal("readCommands returned before the shutdown")
			default:
			}
			shut := make(chan struct{})
			go func() {
				ctl.shutdown(done)
				close(shut)
			}()
			select {
			case <-shut:
			case <-time.After(time.Second):
				// Both may be ready together: a completed shutdown wins.
				select {
				case <-shut:
				default:
					t.Fatal("the ordinary return's close left readCommands blocked: the join never completes")
				}
			}
		})
	}
}

// readerParked waits (bounded) until readCommands' goroutine is parked in
// its descriptor's Read, as the runtime reports it in a goroutine dump:
// "syscall" for a blocking descriptor (in the read system call; Read took
// the descriptor's read lock before entering it, so no Close can pre-empt
// the call from then on) or "IO wait" for a pollable one (parked in the
// poller). The dump is the runtime's own record of the goroutine's state,
// not an elapsed time.
func readerParked(t *testing.T, state string) {
	t.Helper()
	deadline := time.Now().Add(testWait)
	for {
		buf := make([]byte, 1<<20)
		buf = buf[:runtime.Stack(buf, true)]
		for _, g := range strings.Split(string(buf), "\n\n") {
			header, _, _ := strings.Cut(g, "\n")
			if strings.Contains(g, "sidecar.(*guardian).readCommands") && strings.Contains(g, "os.(*File).Read") &&
				strings.Contains(header, " ["+state) {
				return
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("readCommands never parked in Read (%s):\n%s", state, buf)
		}
		runtime.Gosched()
	}
}

// TestGuardianCommandFIFOOpen: the control FIFO's open fails, leaving
// nothing open, for an absent FIFO (its read end) and for one whose write
// end may not be opened.
func TestGuardianCommandFIFOOpen(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	if _, err := openControlFIFO(filepath.Join(dir, "absent"), adoptBlocking); err == nil {
		t.Fatal("an absent FIFO opened")
	}
	if os.Geteuid() == 0 {
		return // root writes through any mode
	}
	ro := filepath.Join(dir, "read-only")
	if err := unix.Mkfifo(ro, 0o400); err != nil {
		t.Fatal(err)
	}
	var adopted *os.File
	if _, err := openControlFIFO(ro, func(fd int, name string) *os.File { adopted = adoptBlocking(fd, name); return adopted }); err == nil {
		t.Fatal("a FIFO without write permission opened its write end")
	}
	if _, err := adopted.Stat(); err == nil {
		t.Fatal("the read end of a failed open was left open")
	}
}
