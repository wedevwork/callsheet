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
	// resend: the in-flight chunk's attachment ended before its receipt
	// (iteration 06a); the next attachment resends exactly that chunk.
	resend bool
	// unloaded counts a recovered execution's retained tail bytes that are
	// still only in its journal (starting at base); replay marks such a
	// ring, whose storage is released once its replayed bytes are
	// acknowledged (iteration 06a: recovery keeps metadata only).
	unloaded int
	replay   bool
}

func newOutputRing(max int) *outputRing {
	return &outputRing{max: max, limit: contract.MaxSafeInteger}
}

// newOutputRingMeta is a recovered execution's ring holding only the
// metadata of its journal's retained tail (ending at the log's source
// count; everything before it was acknowledged by some plane): the bytes
// stay on disk until load.
func newOutputRingMeta(max int, lg contract.TaskLog) *outputRing {
	r := &outputRing{max: max, limit: contract.MaxSafeInteger, incomplete: lg.Incomplete, overflow: lg.CounterOverflow, replay: true}
	r.base = lg.SourceBytes - len(lg.Data)
	r.end, r.acked, r.unloaded = lg.SourceBytes, r.base, len(lg.Data)
	return r
}

// needsLoad reports a recovered tail still only in its journal.
func (r *outputRing) needsLoad() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.unloaded > 0
}

// holding reports replayed bytes in memory (unsent or in flight).
func (r *outputRing) holding() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.n > 0 || r.inflight != nil
}

// load installs a recovered tail read back from the journal. A tail that
// does not match the recorded metadata (or could not be read: nil) is
// dropped and the log marked incomplete.
func (r *outputRing) load(data []byte) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.unloaded == 0 {
		return
	}
	if len(data) != r.unloaded || len(data) > r.max {
		r.unloaded, r.incomplete = 0, true
		r.base = r.end
		r.acked = max(r.acked, r.base)
		return
	}
	r.unloaded = 0
	r.grow(len(data))
	copy(r.buf, data)
	r.head, r.n = 0, len(data)
}

// journalLog is the journal's log: the unsent retained tail, ending at
// the observed source end, and the flags.
func (r *outputRing) journalLog() contract.TaskLog {
	r.mu.Lock()
	defer r.mu.Unlock()
	data := make([]byte, r.n)
	r.copyOut(data, 0, r.n)
	return contract.TaskLog{Data: data, SourceBytes: r.end, ReceivedBytes: r.end, Incomplete: r.incomplete, CounterOverflow: r.overflow}
}

// unbind marks an in-flight chunk for an exact resend: its attachment
// ended before its receipt, and the plane deduplicates whole chunks.
func (r *outputRing) unbind() {
	r.mu.Lock()
	r.resend = r.inflight != nil
	r.mu.Unlock()
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
	if r.inflight != nil && r.resend {
		r.resend = false
		return r.inflight
	}
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
	r.inflight, r.resend = nil, false
	if end > r.acked {
		r.acked = end
	}
	if drop := r.acked - r.base; drop > 0 {
		r.evict(drop)
		if r.base < r.acked {
			r.base = r.acked
		}
	}
	if r.replay && r.n == 0 {
		r.buf, r.head = nil, 0 // a replayed tail leaves memory once sent
	}
}

// drained reports that no unsent byte (in memory or still to load from
// the journal) and no in-flight chunk remain.
func (r *outputRing) drained() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.n == 0 && r.inflight == nil && r.unloaded == 0
}

// pending reports whether a new chunk could be sent now.
func (r *outputRing) pending() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return (r.inflight == nil && r.n > 0) || (r.inflight != nil && r.resend)
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

// closeParent closes the supervisor's ends (the child's ends were handed
// to the launcher).
func (p *pipes) closeParent() {
	for _, f := range []*os.File{p.stdinW, p.stdoutR, p.stderrR} {
		if f != nil {
			f.Close()
		}
	}
}

func (p *pipes) closeAll() {
	for _, f := range []*os.File{p.stdinR, p.stdinW, p.stdoutR, p.stdoutW, p.stderrR, p.stderrW} {
		if f != nil {
			f.Close()
		}
	}
}
