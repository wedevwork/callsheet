//go:build linux || darwin

package adapter

import (
	"io"
	"os"
	"os/exec"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

// TestProbePipes covers the writer-still-open decision on raw pipes: a
// closed writer settles at once with its output read, a writer closed
// during the wait settles once it closes, and a writer still open after
// the limit does not settle.
func TestProbePipes(t *testing.T) {
	open := func(t *testing.T) (*probePipes, *capture, *capture, *os.File, *os.File) {
		t.Helper()
		out, errOut := &capture{max: probeCapture}, &capture{max: probeCapture}
		pp, w0, w1, err := openProbePipes(out, errOut)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { w0.Close(); w1.Close() })
		pp.drain()
		return pp, out, errOut, w0, w1
	}
	t.Run("closed", func(t *testing.T) {
		pp, out, errOut, w0, w1 := open(t)
		w0.WriteString("out\n")
		w1.WriteString("err\n")
		w0.Close()
		w1.Close()
		if r := pp.settle(time.Hour); r != settleClosed {
			t.Fatalf("closed writers settled as %d", r)
		}
		if o, _ := out.result(); o != "out\n" {
			t.Fatalf("stdout %q", o)
		}
		if e, _ := errOut.result(); e != "err\n" {
			t.Fatalf("stderr %q", e)
		}
	})
	t.Run("late", func(t *testing.T) {
		// The writer closes only after settle observed it open and began
		// its timed wait.
		pp, out, _, w0, w1 := open(t)
		w1.Close()
		waiting := make(chan struct{}, 1)
		pp.waiting = func() {
			select {
			case waiting <- struct{}{}:
			default:
			}
		}
		done := make(chan settleResult, 1)
		go func() { done <- pp.settle(testWait) }()
		select {
		case <-waiting:
		case <-time.After(testWait):
			t.Fatal("settle never waited for the open writer")
		}
		w0.WriteString("late\n")
		w0.Close()
		select {
		case r := <-done:
			if r != settleClosed {
				t.Fatalf("a writer closed within the limit settled as %d", r)
			}
		case <-time.After(testWait):
			t.Fatal("settle did not return")
		}
		if o, _ := out.result(); o != "late\n" {
			t.Fatalf("stdout %q", o)
		}
	})
	t.Run("eof-without-hup", func(t *testing.T) {
		// Review C1: closed writers signalled as POLLIN and a zero read,
		// never POLLHUP, with the limit already passed when EOF is first
		// read. EOF alone proves the writer closed. No drainer runs, so
		// settle itself sees every byte.
		out, errOut := &capture{max: probeCapture}, &capture{max: probeCapture}
		pp, w0, w1, err := openProbePipes(out, errOut)
		if err != nil {
			t.Fatal(err)
		}
		pp.refs = 1
		pp.poll = pollWithoutHup
		w0.WriteString("x")
		w0.Close()
		w1.Close()
		if r := pp.settle(0); r != settleClosed {
			t.Fatalf("closed writers settled as %d (eof %v)", r, pp.eof)
		}
		if o, _ := out.result(); o != "x" {
			t.Fatalf("stdout %q", o)
		}
	})
	t.Run("flood", func(t *testing.T) {
		// A writer still open and still writing after the limit is open:
		// settle reads at most finalDrainCap more and gives up.
		out, errOut := &capture{max: probeCapture}, &capture{max: probeCapture}
		pp, w0, w1, err := openProbePipes(out, errOut)
		if err != nil {
			t.Fatal(err)
		}
		pp.refs = 1
		// Each poll waits for the writer's next chunk, so every pass after
		// the limit reads more until the cap ends it.
		pp.poll = func(set []unix.PollFd, _ int) (int, error) {
			return pollWithoutHup(set, int(testWait/time.Millisecond))
		}
		w1.Close()
		wrote := make(chan struct{})
		go func() {
			defer close(wrote)
			b := make([]byte, 4096)
			for {
				if _, err := w0.Write(b); err != nil {
					return
				}
			}
		}()
		if r := pp.settle(0); r != settleOpen {
			t.Fatalf("a flooding writer settled as %d", r)
		}
		if o, over := out.result(); len(o) != probeCapture || !over {
			t.Fatalf("flood captured %d bytes, overflow %v", len(o), over)
		}
		w0.Close()
		<-wrote
	})
	t.Run("read-error", func(t *testing.T) {
		// Review r2 C1: the stderr read end is invalid (POLLNVAL, EBADF)
		// while its writer stays open. A failed observation is never a
		// closed writer, whether the reaper or the drainer meets it.
		for _, drainer := range []bool{false, true} {
			out, errOut := &capture{max: probeCapture}, &capture{max: probeCapture}
			pp, w0, w1, err := openProbePipes(out, errOut)
			if err != nil {
				t.Fatal(err)
			}
			w0.WriteString(ProbeOutput)
			w0.Close()
			unix.Close(pp.fds[1])
			pp.fds[1] = unusedFD(t)
			if drainer {
				pp.drain()
				awaitRefs(t, pp, 1)
				if !pp.drainFailed {
					t.Fatal("the drainer did not record its failed read")
				}
			} else {
				pp.refs = 1
			}
			if r := pp.settle(time.Hour); r != settleFailed {
				t.Fatalf("drainer %v: an unreadable stream with its writer open settled as %d (eof %v)", drainer, r, pp.eof)
			}
			if pp.eof[1] {
				t.Fatalf("drainer %v: a failed read counted as EOF", drainer)
			}
			w1.Close()
		}
	})
	t.Run("poll-error", func(t *testing.T) {
		// A failed poll, the drainer's or the reaper's, is a failed
		// observation; the real runner reports it as errProbeOutput.
		failPoll := func([]unix.PollFd, int) (int, error) { return 0, unix.EINVAL }
		for _, drainer := range []bool{false, true} {
			pp, w0, w1, err := openProbePipes(io.Discard, io.Discard)
			if err != nil {
				t.Fatal(err)
			}
			pp.poll = failPoll
			if drainer {
				pp.drain()
				awaitRefs(t, pp, 1)
			} else {
				pp.refs = 1
			}
			w0.Close()
			w1.Close()
			if r := pp.settle(time.Hour); r != settleFailed {
				t.Fatalf("drainer %v: a failed poll settled as %d", drainer, r)
			}
		}
		pp, w0, w1, err := openProbePipes(io.Discard, io.Discard)
		if err != nil {
			t.Fatal(err)
		}
		pp.poll, pp.refs = failPoll, 1
		cmd := exec.Command("/bin/sh", "-c", "exit 0")
		cmd.Stdout, cmd.Stderr = w0, w1
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		w0.Close()
		w1.Close()
		if err, ps := (execProc{cmd: cmd, pipes: pp, waitDelay: probeWaitDelay}).wait(); err != errProbeOutput || !ps.Success() {
			t.Fatalf("wait = %v, %v", err, ps)
		}
	})
	t.Run("in-flight-poll-error", func(t *testing.T) {
		// Review r3 C1: the drainer's poll is in flight when the reaper
		// takes over, and fails only after the reaper marked the pipes
		// reaped while its own polls see both writers closed. The
		// drainer's failure is still the result.
		out, errOut := &capture{max: probeCapture}, &capture{max: probeCapture}
		pp, w0, w1, err := openProbePipes(out, errOut)
		if err != nil {
			t.Fatal(err)
		}
		inPoll := make(chan struct{})
		pp.poll = func(set []unix.PollFd, timeout int) (int, error) {
			if set[len(set)-1].Fd != int32(pp.stopR) {
				return unix.Poll(set, timeout) // the reaper's own polls
			}
			close(inPoll)
			for deadline := time.Now().Add(testWait); ; time.Sleep(time.Millisecond) {
				pp.mu.Lock()
				reaped := pp.reaped
				pp.mu.Unlock()
				if reaped || time.Now().After(deadline) {
					return 0, unix.ENOMEM
				}
			}
		}
		w0.WriteString(ProbeOutput)
		w0.Close()
		w1.Close()
		pp.drain()
		<-inPoll
		if r := pp.settle(time.Hour); r != settleFailed {
			t.Fatalf("an in-flight drainer poll failure settled as %d (eof %v)", r, pp.eof)
		}
	})
	t.Run("open", func(t *testing.T) {
		pp, _, _, w0, w1 := open(t)
		w1.Close()
		start := time.Now()
		if r := pp.settle(10 * time.Millisecond); r != settleOpen {
			t.Fatalf("a writer still open settled as %d", r)
		}
		if el := time.Since(start); el < 10*time.Millisecond {
			t.Fatalf("gave up after %v, before the limit", el)
		}
		w0.Close()
	})
}

// pollWithoutHup is unix.Poll reporting every hang-up as plain
// readability, the EOF signal some systems give for a closed pipe.
func pollWithoutHup(set []unix.PollFd, timeout int) (int, error) {
	n, err := unix.Poll(set, timeout)
	for i := range set {
		if set[i].Revents&unix.POLLHUP != 0 {
			set[i].Revents = set[i].Revents&^unix.POLLHUP | unix.POLLIN
		}
	}
	return n, err
}

// TestProbePipesOpenFailure is review S1: a pipe that cannot be made
// fails openProbePipes and closes every descriptor made before it.
func TestProbePipesOpenFailure(t *testing.T) {
	for fail := range 3 {
		var made []int
		mk := func() (int, int, error) {
			if len(made) == 2*fail {
				return -1, -1, unix.EMFILE
			}
			r, w, err := cloexecPipe()
			if err == nil {
				made = append(made, r, w)
			}
			return r, w, err
		}
		pp, w0, w1, err := openProbePipesWith(mk, io.Discard, io.Discard)
		if err != unix.EMFILE || pp != nil || w0 != nil || w1 != nil {
			t.Fatalf("pipe %d failing: %v %v %v %v", fail, pp, w0, w1, err)
		}
		if len(made) != 2*fail {
			t.Fatalf("pipe %d failing: made %v", fail, made)
		}
		for _, fd := range made {
			if _, err := unix.FcntlInt(uintptr(fd), unix.F_GETFD, 0); err != unix.EBADF {
				t.Fatalf("pipe %d failing: descriptor %d left open (%v)", fail, fd, err)
			}
		}
	}
}

// unusedFD returns a descriptor number no file uses: poll reports it
// POLLNVAL and read fails with EBADF. The kernel hands out the lowest free
// number, so a high one stays unused for the test's duration.
func unusedFD(t *testing.T) int {
	t.Helper()
	for fd := 4000; fd < 5000; fd++ {
		if _, err := unix.FcntlInt(uintptr(fd), unix.F_GETFD, 0); err == unix.EBADF {
			return fd
		}
	}
	t.Fatal("no unused descriptor number")
	return -1
}

// awaitRefs waits (bounded) until pp's users dropped to n: the drainer
// released the pipes once it stopped.
func awaitRefs(t *testing.T, pp *probePipes, n int) {
	t.Helper()
	deadline := time.Now().Add(testWait)
	for {
		pp.mu.Lock()
		refs := pp.refs
		pp.mu.Unlock()
		if refs == n {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("pipe users stayed %d, want %d", refs, n)
		}
		time.Sleep(time.Millisecond)
	}
}
