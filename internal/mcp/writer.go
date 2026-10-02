package mcp

// The single owner of stdout (FP-1, FP-8): the bounded frame queue, the
// writer goroutine that writes each frame serially in chunks, and the
// watchdog that owns the writer's progress timer. Invariants: only the
// writer writes to stdout; a frame's write, once started, is never
// recalled; the queue holds at most MaxActiveCalls results and
// maxQueuedControl control replies; wmu is never held while taking
// another lock.

import (
	"errors"
	"io"
	"runtime"
	"time"

	"github.com/wedevwork/callsheet/internal/contract"
)

// ---- Frames ----

// frame is one outgoing JSON-RPC line.
type frame struct {
	data   []byte
	result bool       // a tool call's answer (else a control reply)
	id     *requestID // the reservation freed once written or discarded
	call   *call
	// emitted runs once the frame's last byte was written (the initialize
	// answer's state transition).
	emitted func()
	// late is the writer's late judgement of a call's answer (writer only).
	late lateKind
	// Under the session's wmu.
	started bool
}

// control queues a control reply (lifecycle, ping, lists, protocol and
// capacity errors); id is the reservation it frees (nil for none).
func (s *session) control(data []byte, id *requestID) {
	s.enqueue(&frame{data: data, id: id})
}

// enqueue adds a frame for the writer. Replies beyond the bounded queue
// mean the client is not reading: the session ends.
func (s *session) enqueue(f *frame) {
	s.wmu.Lock()
	if s.ended() {
		s.wmu.Unlock()
		return
	}
	if !f.result {
		if s.nControl >= maxQueuedControl {
			s.wmu.Unlock()
			s.end(ExitOutput, "the client is not reading its replies (more than 8 pending); the session ends")
			return
		}
		s.nControl++
	}
	s.queue = append(s.queue, f)
	s.wmu.Unlock()
	select {
	case s.wake <- struct{}{}:
	default:
	}
}

// discard removes a queued frame whose write has not started.
func (s *session) discard(f *frame) bool {
	s.wmu.Lock()
	defer s.wmu.Unlock()
	if f.started {
		return false
	}
	for i, q := range s.queue {
		if q == f {
			s.queue = append(s.queue[:i], s.queue[i+1:]...)
			return true
		}
	}
	return true
}

// next returns the next frame to write, or nil once the session ended.
func (s *session) next() *frame {
	for {
		s.wmu.Lock()
		if s.ended() {
			s.wmu.Unlock()
			return nil
		}
		if len(s.queue) > 0 {
			f := s.queue[0]
			s.queue = s.queue[1:]
			if !f.result {
				s.nControl--
			}
			f.started = true
			s.wmu.Unlock()
			return f
		}
		s.wmu.Unlock()
		select {
		case <-s.wake:
		case <-s.endCh:
		}
	}
}

// ---- Writer ----

var (
	errEnded = errors.New("the session ended")
	errLate  = errors.New("the answer was delivered after its deadline")
)

// writer owns stdout: frames are written one at a time, in order, each in
// chunks of at most WriteChunk bytes.
func (s *session) writer() {
	defer close(s.writerDone)
	for {
		f := s.next()
		if f == nil {
			return
		}
		key := ""
		if f.id != nil {
			key = f.id.key
		}
		if f.result && f.call != nil {
			f.call.taken()
		}
		err := s.write(f)
		if errors.Is(err, errLate) {
			s.hook(StageLate, frameKey(f))
			s.end(ExitOutput, f.call.lateWriteDiag(f.late))
			return
		}
		if err != nil {
			if !errors.Is(err, errEnded) {
				s.end(ExitOutput, "cannot write to stdout ("+describeWriteError(err)+"); the session ends")
			}
			return
		}
		s.hook(StageFlushed, key)
		s.written(f)
		s.hook(StageWritten, key)
	}
}

// written frees a delivered frame's reservation and marks its call
// answered.
func (s *session) written(f *frame) {
	if f.call != nil {
		f.call.answered()
		return
	}
	if f.id != nil {
		s.mu.Lock()
		delete(s.reserved, f.id.key)
		s.mu.Unlock()
	}
}

// write writes one frame. The progress timer is armed before the first
// chunk and re-armed after every positive write that leaves bytes to
// write (short writes included); a zero-byte write does not reset it.
func (s *session) write(f *frame) error {
	if !s.arm(true) {
		return errEnded
	}
	if err := s.writeChunks(f); err != nil {
		return err
	}
	if !s.arm(false) {
		return errEnded
	}
	return nil
}

// writeChunks writes f's bytes. For a call's answer every Write of its
// last chunk can finish it: the call is told before (beginFinalWrite) and
// after (endFinalWrite), and the completion instant is sampled right
// after that Write returns, before any lock or bookkeeping, so it alone
// decides the delivery against D (see the deliver* states).
func (s *session) writeChunks(f *frame) error {
	data := f.data
	for off := 0; off < len(data); {
		chunk := data[off:min(off+WriteChunk, len(data))]
		for len(chunk) > 0 {
			if s.ended() {
				return errEnded
			}
			final := f.call != nil && off+len(chunk) == len(data)
			if final && !f.call.beginFinalWrite() {
				return errEnded // D was judged missed: its timer ended the session
			}
			if final {
				s.hook(StageFinal, frameKey(f))
			}
			n, err := s.cfg.Out.Write(chunk)
			var at time.Time
			if final {
				at = s.clock.Now()
			}
			if n > 0 {
				s.progress.Add(1)
				chunk = chunk[n:]
				off += n
			}
			if final {
				s.hook(StageSampled, frameKey(f))
				if k := f.call.endFinalWrite(off == len(data), at); k != notLate {
					f.late = k
					return errLate
				}
			}
			if n > 0 {
				if off == len(data) {
					if f.emitted != nil {
						f.emitted()
					}
				} else if !s.arm(true) {
					return errEnded
				}
			}
			if err != nil {
				if s.ended() {
					return errEnded
				}
				return err
			}
			if n == 0 {
				runtime.Gosched()
			}
		}
	}
	return nil
}

// frameKey is a frame's reservation key ("" for none).
func frameKey(f *frame) string {
	if f.id == nil {
		return ""
	}
	return f.id.key
}

// arm (re)arms or disarms the progress timer and returns once the
// watchdog applied it; false once the session ended.
func (s *session) arm(on bool) bool {
	select {
	case s.wd <- on:
	case <-s.endCh:
		return false
	}
	select {
	case <-s.wdAck:
		return true
	case <-s.endCh:
		return false
	}
}

// watchdog owns the writer's progress timer: expiry without progress since
// it was armed closes the owned output (unblocking the write) and ends the
// session with exit 5.
func (s *session) watchdog() {
	defer s.wg.Done()
	var (
		c     <-chan time.Time
		stop  func() bool
		armed uint64
	)
	disarm := func() {
		if stop != nil {
			stop()
		}
		c, stop = nil, nil
	}
	defer disarm()
	for {
		select {
		case on := <-s.wd:
			disarm()
			if on {
				c, stop = s.clock.NewTimer(WriteIdle)
				armed = s.progress.Load()
			}
			select {
			case s.wdAck <- struct{}{}:
			case <-s.endCh:
				return
			}
		case <-c:
			c, stop = nil, nil
			if p := s.progress.Load(); p != armed {
				c, stop = s.clock.NewTimer(WriteIdle)
				armed = p
				continue
			}
			s.end(ExitOutput, "stdout made no progress for "+formatDuration(WriteIdle)+" (the client stopped reading); the session ends")
		case <-s.endCh:
			return
		}
	}
}

func describeWriteError(err error) string {
	if errors.Is(err, io.ErrClosedPipe) {
		return "the pipe is closed"
	}
	msg := err.Error()
	if len(msg) > 120 {
		msg = msg[:120]
	}
	return contract.SafeText(msg, 120)
}
