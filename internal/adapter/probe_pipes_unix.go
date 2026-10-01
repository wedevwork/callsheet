//go:build linux || darwin

package adapter

import (
	"io"
	"os"
	"sync"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

// probePipes are a probe child's stdout and stderr pipes. The read ends
// are raw non-blocking descriptors, so whether a writer is still open is
// decided by poll(2) on the goroutine that reaped the child, never by when
// a reader goroutine happens to be scheduled. While the child runs, a
// drainer goroutine keeps the pipes from filling; once the child is reaped
// the reaping goroutine takes over the reads.
type probePipes struct {
	mu    sync.Mutex
	fds   [2]int
	sinks [2]io.Writer
	eof   [2]bool
	// drainFailed records a poll or read of the drainer's that failed,
	// including a poll in flight across the hand-off: a failed
	// observation never counts as a closed writer. polling is set while
	// the drainer's poll is in flight; drained closes once the drainer
	// has published its last outcome and stopped.
	drainFailed bool
	polling     bool
	drained     chan struct{}
	reaped      bool
	refs        int
	// stopR wakes the drainer when the reaper closes stopW.
	stopR, stopW int
	// poll is unix.Poll; waiting, when non-nil, is called before each
	// timed wait for a writer still open (tests).
	poll    func([]unix.PollFd, int) (int, error)
	waiting func()
}

// finalDrainCap bounds what settle reads after the limit from a stream
// that is readable but has neither hung up nor reached EOF: a closed
// writer leaves at most a pipe's capacity (Linux's pipe-max-size default
// is 1 MiB) behind, a writer still open could refill it forever.
const finalDrainCap = 1 << 20

// openProbePipes returns the pipes and the write ends the child gets as
// its stdout and stderr; the caller closes those after Start.
func openProbePipes(stdout, stderr io.Writer) (*probePipes, *os.File, *os.File, error) {
	return openProbePipesWith(cloexecPipe, stdout, stderr)
}

// openProbePipesWith is openProbePipes with an injectable pipe
// constructor; a failed pipe closes every descriptor made before it.
func openProbePipesWith(mkPipe func() (int, int, error), stdout, stderr io.Writer) (*probePipes, *os.File, *os.File, error) {
	var r, w [3]int
	for i := range r {
		var err error
		if r[i], w[i], err = mkPipe(); err != nil {
			for j := range i {
				unix.Close(r[j])
				unix.Close(w[j])
			}
			return nil, nil, nil, err
		}
	}
	pp := &probePipes{fds: [2]int{r[0], r[1]}, sinks: [2]io.Writer{stdout, stderr}, stopR: r[2], stopW: w[2], poll: unix.Poll}
	return pp, os.NewFile(uintptr(w[0]), "probe-stdout"), os.NewFile(uintptr(w[1]), "probe-stderr"), nil
}

// cloexecPipe makes a close-on-exec pipe (under ForkLock, as os.Pipe does
// where pipe2 is missing) with a non-blocking read end.
func cloexecPipe() (r, w int, err error) {
	var p [2]int
	syscall.ForkLock.RLock()
	err = unix.Pipe(p[:])
	if err == nil {
		unix.CloseOnExec(p[0])
		unix.CloseOnExec(p[1])
	}
	syscall.ForkLock.RUnlock()
	if err == nil {
		if err = unix.SetNonblock(p[0], true); err != nil {
			unix.Close(p[0])
			unix.Close(p[1])
		}
	}
	return p[0], p[1], err
}

// abandon closes every descriptor of pipes whose child never started.
func (pp *probePipes) abandon() {
	for _, fd := range []int{pp.fds[0], pp.fds[1], pp.stopR, pp.stopW} {
		if fd >= 0 {
			unix.Close(fd)
		}
	}
}

// drain starts the drainer, which reads whatever the running child writes
// (the bounded captures discard the excess) until the reaper takes over.
// It reads only before the hand-off, under mu; the outcome of a poll that
// was in flight when the reaper took over is still published.
func (pp *probePipes) drain() {
	pp.refs = 2
	pp.drained = make(chan struct{})
	go func() {
		defer close(pp.drained)
		defer pp.release()
		buf := make([]byte, 16<<10)
		for {
			// After reaped is set the reaper owns eof; never look at it.
			pp.mu.Lock()
			var set []unix.PollFd
			if !pp.reaped && !pp.drainFailed {
				set = pp.open()
			}
			pp.polling = len(set) > 0
			pp.mu.Unlock()
			if len(set) == 0 {
				return
			}
			_, err := pp.poll(append(set, unix.PollFd{Fd: int32(pp.stopR), Events: unix.POLLIN}), -1)
			pp.mu.Lock()
			pp.polling = false
			switch {
			case err != nil && err != unix.EINTR:
				pp.drainFailed = true
			case err == nil && !pp.reaped:
				if _, _, failed := pp.readReady(set, buf); failed {
					pp.drainFailed = true
				}
			}
			pp.mu.Unlock()
		}
	}()
}

// settle runs on the goroutine that reaped the child. A stream's writer is
// closed once its read end hung up (POLLHUP) or a read returned EOF; how a
// system signals a closed pipe (POLLHUP, or POLLIN and a zero read) does
// not change the result. settle polls at once and reads every closed
// stream to EOF; otherwise it keeps polling (and reading) for up to limit,
// and reports settleOpen only if a poll issued after limit still finds a
// stream that has neither hung up nor reached EOF (and, if it is readable,
// still has not after finalDrainCap more bytes). Any failed poll or read,
// the drainer's included, is settleFailed.
//
// The hand-off never blocks on the drainer's scheduling except for a poll
// it had in flight: closing stopW ends that poll at once, and settle takes
// its outcome after its own observation, so it never counts against limit.
func (pp *probePipes) settle(limit time.Duration) settleResult {
	pp.mu.Lock()
	pp.reaped = true
	inFlight, failed := pp.polling, pp.drainFailed
	pp.mu.Unlock()
	unix.Close(pp.stopW)
	defer pp.release()
	r := settleFailed
	if !failed {
		r = pp.observe(limit)
	}
	if inFlight {
		<-pp.drained
		pp.mu.Lock()
		failed = pp.drainFailed
		pp.mu.Unlock()
		if failed {
			r = settleFailed
		}
	}
	return r
}

// observe is settle's own polling and reading, after the hand-off.
func (pp *probePipes) observe(limit time.Duration) settleResult {
	buf := make([]byte, 16<<10)
	var end time.Time
	timeout, final, drained := 0, false, 0
	for {
		set := pp.open()
		if len(set) == 0 {
			return settleClosed
		}
		_, err := pp.poll(set, timeout)
		if err != nil && err != unix.EINTR {
			return settleFailed
		}
		if err == nil {
			closed, n, failed := pp.readReady(set, buf)
			switch {
			case failed:
				return settleFailed
			case closed:
				// Every writer is gone: drain the rest without waiting.
				timeout = 0
				continue
			case final && n > 0 && drained < finalDrainCap:
				// Readable after the limit: read on until EOF shows the
				// writer closed or the cap shows it is still writing.
				drained += n
				continue
			case final:
				return settleOpen
			}
		}
		if end.IsZero() {
			end = time.Now().Add(limit)
		}
		if rem := time.Until(end); rem > 0 {
			timeout = int((rem + time.Millisecond - 1) / time.Millisecond)
			if pp.waiting != nil {
				pp.waiting()
			}
		} else {
			timeout, final = 0, true
		}
	}
}

// open returns a poll set of the read ends not yet at EOF.
func (pp *probePipes) open() []unix.PollFd {
	set := make([]unix.PollFd, 0, 3)
	for i, fd := range pp.fds {
		if !pp.eof[i] {
			set = append(set, unix.PollFd{Fd: int32(fd), Events: unix.POLLIN})
		}
	}
	return set
}

// readReady reads once from each read end poll found ready (POLLIN,
// POLLHUP, POLLERR or POLLNVAL alike). It reports whether every polled
// stream is closed (hung up, or a read returned EOF: zero bytes and no
// error), how many bytes it read, and whether a read failed: an error
// other than EAGAIN or EINTR (EBADF after POLLNVAL, for one) is failed,
// never eof.
func (pp *probePipes) readReady(set []unix.PollFd, buf []byte) (closed bool, read int, failed bool) {
	closed = true
	for _, p := range set {
		i := 0
		if int(p.Fd) == pp.fds[1] {
			i = 1
		}
		if p.Revents != 0 {
			n, err := unix.Read(pp.fds[i], buf)
			switch {
			case n > 0:
				pp.sinks[i].Write(buf[:n])
				read += n
			case n == 0 && err == nil:
				pp.eof[i] = true
			case err == unix.EAGAIN || err == unix.EINTR:
			default:
				failed = true
			}
		}
		if p.Revents&unix.POLLHUP == 0 && !pp.eof[i] {
			closed = false
		}
	}
	return closed, read, failed
}

// release closes the read ends once both the drainer and the reaper are
// done with them.
func (pp *probePipes) release() {
	pp.mu.Lock()
	pp.refs--
	last := pp.refs == 0
	pp.mu.Unlock()
	if last {
		unix.Close(pp.fds[0])
		unix.Close(pp.fds[1])
		unix.Close(pp.stopR)
	}
}
