package sidecar

import (
	"errors"
	"fmt"
	"slices"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/wedevwork/callsheet/internal/contract"
)

// fastAcks holds the session goroutine after each prompt readiness report
// until that report's acknowledgement is queued in the session's inbox, so
// the session processes it before any later loop pass arms a timer (the
// ordering a quick plane produces only rarely).
type fastAcks struct {
	mu     sync.Mutex
	held   []string
	failed []string
}

func (f *fastAcks) install(d *deps) {
	d.heartbeatHook = func(prompt bool, in *inbox) {
		if !prompt {
			return
		}
		id, err := queuedAck(in)
		f.mu.Lock()
		defer f.mu.Unlock()
		if err != nil {
			f.failed = append(f.failed, err.Error())
			return
		}
		f.held = append(f.held, id)
	}
}

// queuedAck waits (bounded) until the inbox holds a message and returns
// the request ID of that heartbeat acknowledgement.
func queuedAck(in *inbox) (string, error) {
	return queuedAckBy(in, time.After(testWait), nil)
}

// queuedAckBy is queuedAck bounded by deadline; empty, when non-nil, runs
// after each check that found the inbox empty (tests of the wait itself).
// An acknowledgement queued as the bound expires makes both the inbox's
// notification and the deadline ready, so the inbox is checked again
// before the timeout is reported.
func queuedAckBy(in *inbox, deadline <-chan time.Time, empty func()) (string, error) {
	for {
		if it := queuedItem(in); it != nil {
			return ackID(in, it)
		}
		if empty != nil {
			empty()
		}
		select {
		case <-in.ready:
		case <-deadline:
			if it := queuedItem(in); it != nil {
				return ackID(in, it)
			}
			return "", errors.New("no acknowledgement was queued while the session was held")
		}
	}
}

// queuedItem returns the inbox's queued message, if any, without taking
// it.
func queuedItem(in *inbox) *readResult {
	in.mu.Lock()
	defer in.mu.Unlock()
	return in.item
}

// ackID returns the request ID of the queued heartbeat acknowledgement it,
// restoring the inbox's notification (the session still has to take it).
func ackID(in *inbox, it *readResult) (string, error) {
	notify(in.ready)
	if it.err != nil {
		return "", it.err
	}
	f, err := contract.DecodeFrame(it.data, contract.FromPlane)
	if err != nil {
		return "", err
	}
	if f.Type != contract.FrameHeartbeatAck {
		return "", fmt.Errorf("queued %s %s, want a heartbeat acknowledgement", f.Type, f.RequestID)
	}
	return f.RequestID, nil
}

// TestQueuedAckAtDeadline is the C13 regression (iteration 10b review r5):
// an acknowledgement queued after queuedAck found the inbox empty, as its
// bound expires, is still the queued acknowledgement. "timeout-selected"
// consumes the inbox's notification so the wait can only take the expired
// deadline; "both-ready" leaves both ready (either may be selected). Each
// must return the acknowledgement, never the timeout, and leave it queued
// with its notification for the session.
func TestQueuedAckAtDeadline(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name    string
		consume bool
	}{{"timeout-selected", true}, {"both-ready", false}} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			ack, err := contract.EncodeFrame(contract.ProtocolVersion, contract.FrameHeartbeatAck, "b7", nil)
			if err != nil {
				t.Fatal(err)
			}
			in := newInbox(realClock{})
			expired := make(chan time.Time, 1)
			expired <- time.Time{}
			empties := 0
			id, err := queuedAckBy(in, expired, func() {
				if empties++; empties > 1 {
					return
				}
				if !in.put(bg, readResult{typ: websocket.MessageText, data: ack}) {
					t.Error("the acknowledgement was not queued")
				}
				if tc.consume {
					<-in.ready
				}
			})
			if err != nil || id != "b7" {
				t.Fatalf("queuedAck = %q, %v; want b7 (queued as the bound expired)", id, err)
			}
			if empties != 1 {
				t.Fatalf("the inbox was found empty %d times, want 1", empties)
			}
			r, ok := in.take()
			if !ok || string(r.data) != string(ack) {
				t.Fatalf("the acknowledgement was not left queued (%v)", ok)
			}
			select {
			case <-in.ready:
			default:
				t.Fatal("the inbox notification was not restored for the session")
			}
		})
	}
	t.Run("timeout", func(t *testing.T) {
		t.Parallel()
		in := newInbox(realClock{})
		expired := make(chan time.Time, 1)
		expired <- time.Time{}
		if id, err := queuedAckBy(in, expired, nil); err == nil {
			t.Fatalf("queuedAck = %q with nothing queued, want the timeout", id)
		}
	})
}

// heldAck requires that heartbeat k was a prompt report whose
// acknowledgement was queued before the session resumed, and that no hold
// failed.
func (f *fastAcks) heldAck(t *testing.T, k int) {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.failed) != 0 {
		t.Fatalf("fast acknowledgement holds failed: %v", f.failed)
	}
	if id := "b" + strconv.Itoa(k); !slices.Contains(f.held, id) {
		t.Fatalf("report %s was not held for its queued acknowledgement (held %v)", id, f.held)
	}
}

// TestHeartbeatArmedAfterFastAck is the C11 regression (iteration 10b): a
// prompt readiness report sent at the instant of the previous heartbeat
// keeps that heartbeat's next periodic instant, and here every report's
// acknowledgement is already queued when the session resumes after the
// write. The session must still arm its periodic timer afresh for the
// report (an evHeartbeatArmed naming it), and each move of the clock to
// the armed instant must bring the periodic heartbeat.
func TestHeartbeatArmedAfterFastAck(t *testing.T) {
	t.Parallel()
	fp := startFakePlane(t)
	fa := &fastAcks{}
	tr := startTaskRun(t, fp, taskOpts{adjust: fa.install})
	ins, run := manuals(t, tr.dir, "a", "m")
	start := tr.clk.Now()
	// b1 (periodic, T0) arms the timer for T0+5s; the revision's report
	// and its first cycle's readiness report follow at T0, each with the
	// same next instant and an acknowledgement queued before resumption.
	s := tr.connect(t, 1, 1, roleConfig("a", ins, run))
	cyc := tr.ev.awaitCycleSince(t, 1, start)
	s.report(t, 1, func(r contract.RoleStatus) bool { return r.Inflight == 0 && r.CanAccept == cyc.passed[0] })
	if !tr.clk.Now().Equal(start) {
		t.Fatalf("the clock moved before the reports (%v)", tr.clk.Now())
	}
	fa.heldAck(t, s.b-1)
	for range 3 {
		s.periodicArmed(t, 1)
	}
	if want := start.Add(3 * heartbeatInterval); !tr.clk.Now().Equal(want) {
		t.Fatalf("clock %v, want %v", tr.clk.Now(), want)
	}
	fa.heldAck(t, 2)
}
