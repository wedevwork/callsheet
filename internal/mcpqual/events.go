package mcpqual

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sync"
)

// Probe event kinds, in the order a well-behaved session produces them.
const (
	EvStart      = "start"      // the probe started (offset 0)
	EvInitialize = "initialize" // initialize received: exact clientInfo strings
	EvReceipt    = "receipt"    // tools/call slow received; token presence
	EvProgress   = "progress"   // one notifications/progress written
	EvScheduled  = "scheduled"  // the delay elapsed; the result write begins
	EvCompleted  = "completed"  // the result frame's write returned
	EvWriteFail  = "write_failed"
	EvCancelled  = "cancelled"   // matching notifications/cancelled ended the call
	EvLateCancel = "late_cancel" // a cancellation after the terminal transition
	EvRejected   = "rejected"    // a request refused (a second active call)
	EvEOF        = "eof"         // stdin ended; an active call ends with it
	EvError      = "error"       // a fatal framing or write error
	EvExit       = "exit"        // the probe ended (always last when written)
)

// ProbeEvent is one line of a probe events file. Offsets are nanoseconds on
// the probe's own monotonic clock since its start; they are never compared
// with another process's clock.
type ProbeEvent struct {
	Seq           int             `json:"seq"`
	Kind          string          `json:"kind"`
	OffsetNS      int64           `json:"offset_ns"`
	RunID         string          `json:"run_id"`
	CaseID        string          `json:"case_id,omitempty"`
	RequestID     json.RawMessage `json:"request_id,omitempty"`
	TokenPresent  *bool           `json:"token_present,omitempty"`
	Token         json.RawMessage `json:"token,omitempty"`
	Progress      int             `json:"progress,omitempty"`
	ClientName    *string         `json:"client_name,omitempty"`
	ClientVersion *string         `json:"client_version,omitempty"`
	Reason        string          `json:"reason,omitempty"`
}

// errLogExhausted ends the probe: its event file reached its bound.
var errLogExhausted = errors.New("probe event log exhausted")

// exitReserve is kept free for the final exit event.
const exitReserve = 512

// eventLog appends bounded JSON lines; once full it refuses every event but
// the final exit, which uses the reserve.
type eventLog struct {
	mu    sync.Mutex
	w     io.Writer
	limit int
	n     int
	seq   int
	full  bool
	err   error
}

func newEventLog(w io.Writer, limit int) *eventLog {
	return &eventLog{w: w, limit: limit}
}

func (l *eventLog) add(ev ProbeEvent) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.err != nil {
		return l.err
	}
	final := ev.Kind == EvExit
	if l.full && !final {
		return errLogExhausted
	}
	l.seq++
	ev.Seq = l.seq
	b, err := encodeJSON(ev)
	if err != nil {
		return err
	}
	b = append(b, '\n')
	budget := l.limit - exitReserve
	if final {
		budget = l.limit
	}
	if l.n+len(b) > budget {
		l.seq--
		l.full = true
		return errLogExhausted
	}
	if _, err := l.w.Write(b); err != nil {
		l.err = err
		return err
	}
	l.n += len(b)
	return nil
}

// ParseProbeEvents decodes a probe events file. A vendor that starts the
// server more than once in a session appends one instance per start: each
// instance begins with a start event at seq 1 and has its own clock
// origin, and offsets are compared only within an instance. It reports
// whether the file ended with an exit event (an intact log) and rejects
// oversized or malformed files.
func ParseProbeEvents(b []byte) (evs []ProbeEvent, intact bool, err error) {
	if len(b) > MaxEvidenceFileBytes {
		return nil, false, fmt.Errorf("probe events: %d bytes exceeds %d", len(b), MaxEvidenceFileBytes)
	}
	seq, last := 0, int64(0)
	for i, line := range bytes.Split(b, []byte{'\n'}) {
		if len(line) == 0 {
			continue
		}
		var ev ProbeEvent
		if err := decodeStrict(line, &ev); err != nil {
			return nil, false, fmt.Errorf("probe events line %d: %w", i+1, err)
		}
		if ev.Kind == EvStart && ev.Seq == 1 {
			seq, last = 0, 0
		}
		if ev.Seq != seq+1 || (seq == 0 && ev.Kind != EvStart) {
			return nil, false, fmt.Errorf("probe events line %d: seq %d out of order", i+1, ev.Seq)
		}
		if ev.OffsetNS < last {
			return nil, false, fmt.Errorf("probe events line %d: offset went backwards", i+1)
		}
		seq, last = ev.Seq, ev.OffsetNS
		evs = append(evs, ev)
	}
	intact = len(evs) > 0 && evs[len(evs)-1].Kind == EvExit && bytes.HasSuffix(b, []byte{'\n'})
	return evs, intact, nil
}
