package sidecar

import (
	"io"
	"os"
	"sync"

	"github.com/wedevwork/callsheet/internal/contract"
)

// The task output buffer (iteration 05, Output): both pipes are drained
// concurrently from Start in chunks of at most 16 KiB; a short mutex
// assigns each chunk its merged absolute offset and inserts it into a
// ring that retains at most 10 MiB of unsent output, evicting the oldest
// bytes (partial lines included) so a stalled receiver can neither block
// the child nor grow memory. One immutable in-flight copy of at most
// 16 KiB is kept until the plane acknowledges it; eviction never touches
// it.

// outChunk is one immutable in-flight chunk.
type outChunk struct {
	offset int
	data   []byte
}

// outputRing is the retained unsent output of one task.
type outputRing struct {
	mu    sync.Mutex
	max   int
	buf   []byte // circular storage, grown up to max
	head  int    // index of the oldest retained byte in buf
	n     int    // retained bytes
	base  int    // absolute offset of the oldest retained byte
	end   int    // absolute offset after the newest observed byte
	acked int    // absolute offset the plane acknowledged through
	// inflight is the chunk being sent (nil when none).
	inflight *outChunk
	// overflow: the offset counter reached MaxSafeInteger; new output is
	// drained and discarded, the log is incomplete.
	overflow bool
	// incomplete records a local cutoff (inherited pipes closed after the
	// drain bound) or the overflow.
	incomplete bool
	limit      int // the offset counter's bound (MaxSafeInteger)
}

func newOutputRing(max int) *outputRing {
	return &outputRing{max: max, limit: contract.MaxSafeInteger}
}

// write appends p at the next absolute offset, evicting the oldest
// retained bytes beyond max. It never waits on anything but the mutex.
func (r *outputRing) write(p []byte) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.overflow {
		return
	}
	if len(p) > r.limit-r.end {
		// Theoretical counter exhaustion: keep what fits, then stop
		// retaining and saturate the counter.
		p = p[:r.limit-r.end]
		r.overflow, r.incomplete = true, true
	}
	r.end += len(p)
	if len(p) > r.max {
		drop := len(p) - r.max
		r.evict(r.n)
		r.base += drop
		p = p[drop:]
	}
	if over := r.n + len(p) - r.max; over > 0 {
		r.evict(over)
	}
	if len(p) == 0 {
		return
	}
	r.grow(r.n + len(p))
	// len(r.buf) >= r.n+len(p): the bytes after the tail, then the wrap
	// before the head, hold p.
	tail := (r.head + r.n) % len(r.buf)
	k := copy(r.buf[tail:], p)
	copy(r.buf, p[k:])
	r.n += len(p)
}

// evict drops k of the oldest retained bytes.
func (r *outputRing) evict(k int) {
	k = min(k, r.n)
	if k == 0 {
		return
	}
	r.head = (r.head + k) % len(r.buf)
	r.n -= k
	r.base += k
}

// grow ensures capacity for need bytes (at most max), relinearizing.
func (r *outputRing) grow(need int) {
	if need <= len(r.buf) {
		return
	}
	size := max(len(r.buf)*2, 64<<10, need)
	size = min(size, r.max)
	nb := make([]byte, size)
	r.copyOut(nb, 0, r.n)
	r.buf, r.head = nb, 0
}

// copyOut copies k retained bytes starting at retained position from.
func (r *outputRing) copyOut(dst []byte, from, k int) {
	if len(r.buf) == 0 {
		return
	}
	start := (r.head + from) % len(r.buf)
	c := copy(dst[:k], r.buf[start:min(len(r.buf), start+k)])
	copy(dst[c:k], r.buf[:k-c])
}

// next returns the oldest surviving unsent bytes (at most
// contract.MaxLogChunkBytes) as the new in-flight chunk, or nil when a
// chunk is already in flight or nothing is retained.
func (r *outputRing) next() *outChunk {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.inflight != nil || r.n == 0 {
		return nil
	}
	k := min(r.n, contract.MaxLogChunkBytes)
	c := &outChunk{offset: r.base, data: make([]byte, k)}
	r.copyOut(c.data, 0, k)
	r.inflight = c
	return c
}

// ack records the plane's receipt of the in-flight chunk through end:
// those bytes leave the ring if still present.
func (r *outputRing) ack(end int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.inflight = nil
	if end > r.acked {
		r.acked = end
	}
	if drop := r.acked - r.base; drop > 0 {
		r.evict(drop)
		if r.base < r.acked {
			r.base = r.acked
		}
	}
}

// drained reports that no unsent byte and no in-flight chunk remain.
func (r *outputRing) drained() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.n == 0 && r.inflight == nil
}

// pending reports whether a new chunk could be sent now.
func (r *outputRing) pending() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.inflight == nil && r.n > 0
}

// totals reports the source byte count and the incomplete and overflow
// flags.
func (r *outputRing) totals() (end int, incomplete, overflow bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.end, r.incomplete, r.overflow
}

// markIncomplete records a local cutoff.
func (r *outputRing) markIncomplete() {
	r.mu.Lock()
	r.incomplete = true
	r.mu.Unlock()
}

// retained returns a copy of the retained bytes (tests and diagnostics).
func (r *outputRing) retained() []byte {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]byte, r.n)
	r.copyOut(out, 0, r.n)
	return out
}

// drain reads f until EOF or error in chunks of at most
// contract.MaxLogChunkBytes with one fixed buffer, inserting each into
// the ring and handing it to feed (stdout's extractor), then signals
// notify. It never waits for network or disk progress.
func drain(f io.Reader, r *outputRing, feed func([]byte), notify func()) {
	buf := make([]byte, contract.MaxLogChunkBytes)
	for {
		n, err := f.Read(buf)
		if n > 0 {
			r.write(buf[:n])
			if feed != nil {
				feed(buf[:n])
			}
			notify()
		}
		if err != nil {
			return
		}
	}
}

// pipes are one task's OS pipes: the child's ends are handed to the
// process; the supervisor keeps the others.
type pipes struct {
	stdinR, stdinW   *os.File
	stdoutR, stdoutW *os.File
	stderrR, stderrW *os.File
}

func newPipes() (*pipes, error) {
	p := &pipes{}
	var err error
	if p.stdinR, p.stdinW, err = os.Pipe(); err != nil {
		return nil, err
	}
	if p.stdoutR, p.stdoutW, err = os.Pipe(); err != nil {
		p.closeAll()
		return nil, err
	}
	if p.stderrR, p.stderrW, err = os.Pipe(); err != nil {
		p.closeAll()
		return nil, err
	}
	return p, nil
}

func (p *pipes) closeAll() {
	for _, f := range []*os.File{p.stdinR, p.stdinW, p.stdoutR, p.stdoutW, p.stderrR, p.stderrW} {
		if f != nil {
			f.Close()
		}
	}
}
