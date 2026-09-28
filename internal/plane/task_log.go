package plane

import (
	"github.com/wedevwork/callsheet/internal/contract"
)

// planeLog is a live task's received output (iteration 05): exactly the
// newest at most 10 MiB of received bytes (leading bytes, partial lines
// included, are evicted), the next expected offset, the highest observed
// source end, the unique received byte count and the gap flag. Receipt
// into this bounded memory is what task_log_ack acknowledges; it is not
// disk durability. The task observation lock guards it.
type planeLog struct {
	buf        []byte
	head, n    int
	next       int
	source     int
	received   int
	incomplete bool
	overflow   bool
	// late: a late-evidence staging ring (iteration 06a) that starts at
	// its first chunk's offset; its bytes before that are not received,
	// so its finalized log is incomplete unless it starts at zero.
	late bool
}

func newPlaneLog() *planeLog { return &planeLog{} }

// newPlaneLogFrom seeds a ring with a persisted nonterminal log (a loaded
// task reconciled on this plane run): its retained tail and counters,
// further output continuing at its source end.
func newPlaneLogFrom(lg contract.TaskLog) *planeLog {
	l := &planeLog{next: lg.SourceBytes, source: lg.SourceBytes, received: lg.ReceivedBytes, incomplete: lg.Incomplete, overflow: lg.CounterOverflow}
	if len(lg.Data) > 0 {
		l.push(lg.Data)
	}
	return l
}

// newPlaneLogAt is a late staging ring whose first expected byte is at
// offset (a replayed tail's absolute start).
func newPlaneLogAt(offset int) *planeLog {
	return &planeLog{next: offset, source: offset, late: true}
}

// append accepts one chunk at an absolute offset and returns the next
// offset to acknowledge. A range entirely below next is an idempotent
// duplicate (acknowledged, not appended); a partial overlap is a protocol
// error; a range beyond next is a gap, recorded as incomplete.
func (l *planeLog) append(offset int, data []byte) (int, error) {
	end := offset + len(data)
	switch {
	case end <= l.next:
		return l.next, nil
	case offset < l.next:
		return 0, errf(contract.CodeInvalidArgument, "task output at offset %d partially overlaps the %d bytes already received", offset, l.next)
	case offset > l.next:
		l.incomplete = true
	}
	l.push(data)
	l.received = min(contract.MaxSafeInteger, l.received+len(data))
	l.next = end
	l.source = max(l.source, end)
	return end, nil
}

// appendReplay is append for a reconciled worker's replayed tail (a
// tagged chunk): the worker cannot know how much of it arrived before its
// attachment ended or it restarted, so a chunk straddling the next offset
// keeps only its unreceived suffix (the bytes of one output stream are the
// same at the same offsets); everything else is append's.
func (l *planeLog) appendReplay(offset int, data []byte) (int, error) {
	if end := offset + len(data); offset < l.next && end > l.next {
		data, offset = data[l.next-offset:], l.next
	}
	return l.append(offset, data)
}

// push inserts data, evicting the oldest bytes beyond the retention cap.
func (l *planeLog) push(data []byte) {
	const max = contract.MaxLogRetainedBytes
	if len(data) >= max {
		data = data[len(data)-max:]
		l.head, l.n = 0, 0
	}
	if over := l.n + len(data) - max; over > 0 {
		l.head = (l.head + over) % len(l.buf)
		l.n -= over
	}
	if need := l.n + len(data); need > len(l.buf) {
		size := min(max, maxInt(2*len(l.buf), 64<<10, need))
		nb := make([]byte, size)
		l.copyOut(nb, l.n)
		l.buf, l.head = nb, 0
	}
	tail := (l.head + l.n) % len(l.buf)
	k := copy(l.buf[tail:], data)
	copy(l.buf, data[k:])
	l.n += len(data)
}

func maxInt(v ...int) int {
	m := v[0]
	for _, x := range v[1:] {
		if x > m {
			m = x
		}
	}
	return m
}

// copyOut copies the first k retained bytes into dst.
func (l *planeLog) copyOut(dst []byte, k int) {
	if len(l.buf) == 0 || k == 0 {
		return
	}
	c := copy(dst[:k], l.buf[l.head:min(len(l.buf), l.head+k)])
	copy(dst[c:k], l.buf[:k-c])
}

// tail returns a copy of the newest at most k retained bytes.
func (l *planeLog) tail(k int) []byte {
	k = min(k, l.n)
	out := make([]byte, k)
	if k == 0 {
		return out
	}
	start := (l.head + l.n - k) % len(l.buf)
	c := copy(out, l.buf[start:min(len(l.buf), start+k)])
	copy(out[c:], l.buf[:k-c])
	return out
}

// meta returns the counters (without data) and the retained length.
func (l *planeLog) meta() (contract.TaskLog, int) {
	return contract.TaskLog{SourceBytes: l.source, ReceivedBytes: l.received, Incomplete: l.incomplete, CounterOverflow: l.overflow}, l.n
}

// snapshot returns an immutable copy of the retained bytes and counters
// (a checkpoint's or the terminal commit's log).
func (l *planeLog) snapshot() contract.TaskLog {
	lg, _ := l.meta()
	lg.Data = make([]byte, l.n)
	l.copyOut(lg.Data, l.n)
	return lg
}

// finalize applies the result's final source count and the sidecar's
// explicit incomplete and overflow flags, then snapshots: bytes the
// result reports beyond what arrived are a trailing gap.
func (l *planeLog) finalize(outputBytes int, incomplete, overflow bool) contract.TaskLog {
	if outputBytes > l.next {
		l.incomplete = true
	}
	l.source = max(l.source, outputBytes)
	l.incomplete = l.incomplete || incomplete || overflow || (l.late && l.received < l.source)
	l.overflow = l.overflow || overflow
	return l.snapshot()
}
