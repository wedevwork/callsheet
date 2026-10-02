package sidecar

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/wedevwork/callsheet/internal/contract"
)

// Iteration 06b worker families: TestControlCancel (FP-1) and
// TestControlRemove's worker case (FP-4). Every guardian is the counted
// injected one (zero OS processes); the tests synchronize on the worker's
// named events and the fake plane's frames, never on sleeps.

// t0Sidecar is the task runs' fake clock origin.
var t0Sidecar = time.Date(2026, 9, 26, 12, 0, 0, 0, time.UTC)

// replaceOrdered sends roles_replace rid of revision rev with the exact
// records given (explicit registration orders).
func (c *fakeConn) replaceOrdered(rid string, rev int, recs ...contract.RoleRecord) {
	c.t.Helper()
	c.send(contract.ProtocolVersion, contract.FrameRolesReplace, rid, contract.RolesReplaceBody{Revision: rev, Roles: recs})
}

// stopOf is a plane stop intent with id's hex digit.
func stopOf(digit string) contract.StopIntent {
	return contract.StopIntent{ID: strings.Repeat(digit, 32), Kind: contract.StopKindCancelled, RequestedAt: t0Sidecar}
}

// sendCancel sends task_cancel (next plane request) for st and returns its
// request ID and the acknowledgement's body.
func (s *taskSession) sendCancel(t *testing.T, st contract.TaskStartBody, in contract.StopIntent) (string, contract.TaskCancelAckBody) {
	t.Helper()
	rid := s.nextP()
	s.c.send(contract.ProtocolVersion, contract.FrameTaskCancel, rid, contract.TaskCancelBody{TaskID: st.TaskID, Execution: st.Execution, StopID: in.ID, Kind: in.Kind})
	f := s.c.expect(contract.FrameTaskCancelAck, rid)
	a, err := contract.DecodeTaskCancelAck(f.Body)
	if err != nil || a.TaskID != st.TaskID || a.Execution != st.Execution || a.StopID != in.ID {
		t.Fatalf("task_cancel_ack %s: %+v %v", f.Body, a, err)
	}
	return rid, a
}

// journalNow reads task id's execution journal.
func (tr *taskRun) journalNow(t *testing.T, id string) contract.ExecutionJournal {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(tr.root, journalDir, id, executionName))
	if err != nil {
		t.Fatal(err)
	}
	j, err := contract.ParseExecutionJournal(b, tr.lookup())
	if err != nil {
		t.Fatal(err)
	}
	return j
}

// guardianOf returns the injected guardian launched for task id.
func (tr *taskRun) guardianOf(t *testing.T, id string) *fakeGuardian {
	t.Helper()
	tr.ledger.mu.Lock()
	defer tr.ledger.mu.Unlock()
	for _, g := range tr.ledger.guardians {
		if g.spec.inv.TaskID == id {
			return g
		}
	}
	t.Fatalf("no guardian for %s", id)
	return nil
}

// isKind matches an event of kind about task id.
func isKind(kind eventKind, id string) func(event) bool {
	return func(ev event) bool { return ev.kind == kind && ev.id == id }
}

// TestControlCancel is UT FP-1 on the worker, delegated from
// tests/function (TestControlCancellation): a control latched while
// preparing prevents the release (cancelled before start); a launched
// execution's guardian is asked to cancel, its trusted cause and the
// group's confirmed disappearance produce cancelled with the partial
// output; an unconfirmed cleanup is lost and keeps the node blocked;
// duplicates are receipts only. Do not rename or skip it.
func TestControlCancel(t *testing.T) {
	t.Parallel()
	t.Run("preparing", func(t *testing.T) {
		t.Parallel()
		// A preparing execution (its guardian not yet ready) whose attachment
		// ended receives the plane's durable intent as stop_control: it never
		// releases an adapter and reports cancelled without start. Review
		// C2: when the never-released guardian's group cannot be proved gone
		// the outcome is lost (naming the intent), never cancelled, and the
		// slot and the node's admission blocker are kept.
		for _, unconfirmed := range []bool{false, true} {
			preparingCancel(t, unconfirmed)
		}
	})
	t.Run("guardian-control", func(t *testing.T) {
		t.Parallel()
		// A launched execution: the receipt is acknowledged before any
		// journal or FIFO I/O; the intent is journaled by its own writer; the
		// guardian gets the cancel; its stopping cause, the adapter's exit
		// during cleanup and the confirmed group absence give cancelled.
		fp := startFakePlane(t)
		tr := startTaskRun(t, fp, taskOpts{})
		tr.ledger.manualControl = true
		ins, run := manuals(t, tr.dir, "a", "m")
		cfg := roleConfig("a", ins, run)
		s := tr.connect(t, 1, 1, cfg)
		st, ch := s.run(t, 1, 0, "running")
		ch.prompt(t)
		in := stopOf("b")
		s.sendCancel(t, st, in)
		tr.ev.awaitAll(t, isKind(evControlLatched, st.TaskID), isKind(evControlSent, st.TaskID), isKind(evIntentJournaled, st.TaskID))
		if got := <-tr.ledger.controls; got != in.ID {
			t.Fatalf("guardian cancel %s", got)
		}
		if j := tr.journalNow(t, st.TaskID); j.Phase != contract.JournalRunning || j.StopIntent == nil || j.StopIntent.ID != in.ID || j.OwnerNonce == nil {
			t.Fatalf("intent journal %+v", j)
		}
		id := in.ID
		tr.guardianOf(t, st.TaskID).stopping(contract.CauseCancelled, &id)
		ch.signaled("SIGTERM")
		tr.ev.awaitAll(t, isKind(evStopping, st.TaskID), isKind(evGroupGone, st.TaskID), isKind(evTaskExited, st.TaskID))
		r := s.result(t, st)
		if r.Outcome != contract.OutcomeCancelled || *r.StopID != in.ID || r.Signal == nil || *r.Signal != "SIGTERM" {
			t.Fatalf("cancelled %+v", r)
		}
		t.Run("delivery-retry", func(t *testing.T) {
			// Review C3: a failed guardian delivery is retried (every
			// controlRetry, the intent unchanged) until the guardian has it;
			// redeliveries meanwhile are receipts only.
			fp := startFakePlane(t)
			tr := startTaskRun(t, fp, taskOpts{})
			tr.ledger.controlErrs = 2
			ins, run := manuals(t, tr.dir, "a", "m")
			cfg := roleConfig("a", ins, run)
			s := tr.connect(t, 1, 1, cfg)
			st, ch := s.run(t, 1, 0, "retry")
			ch.prompt(t)
			in := stopOf("f")
			s.sendCancel(t, st, in)
			for attempt := 1; attempt <= 3; attempt++ {
				select {
				case got := <-tr.ledger.controls:
					if got != in.ID {
						t.Fatalf("attempt %d: guardian cancel %s", attempt, got)
					}
				case <-time.After(testWait):
					t.Fatalf("attempt %d: the failed guardian delivery was never retried", attempt)
				}
				if attempt == 3 {
					break
				}
				// The failed attempt is over (its retry armed): a redelivery
				// is a receipt; only the clock drives the retry.
				tr.ev.awaitMatch(t, evControlSent, func(ev event) bool { return ev.id == st.TaskID })
				s.sendCancel(t, st, in)
				tr.clk.Advance(controlRetry)
			}
			r, _ := s.drain(t, st)
			if r.Outcome != contract.OutcomeCancelled || r.StopID == nil || *r.StopID != in.ID {
				t.Fatalf("cancelled after retries %+v", r)
			}
			select {
			case extra := <-tr.ledger.controls:
				t.Fatalf("a delivered cancel was sent again: %s", extra)
			default:
			}
		})
	})
	t.Run("guardian-fifo", func(t *testing.T) {
		t.Parallel()
		guardianFIFO(t)
	})
	t.Run("partial-output", func(t *testing.T) {
		t.Parallel()
		// Output before and during cleanup is kept and flows as the
		// cancelled result's partial log.
		fp := startFakePlane(t)
		tr := startTaskRun(t, fp, taskOpts{})
		ins, run := manuals(t, tr.dir, "a", "m")
		cfg := roleConfig("a", ins, run)
		s := tr.connect(t, 1, 1, cfg)
		st, ch := s.run(t, 1, 0, "output")
		ch.prompt(t)
		ch.out([]byte("partial one\n"))
		if got := string(s.logs(t, st, 12)); got != "partial one\n" {
			t.Fatalf("logs %q", got)
		}
		in := stopOf("c")
		s.sendCancel(t, st, in)
		r, rest := s.drain(t, st)
		if r.Outcome != contract.OutcomeCancelled || *r.StopID != in.ID || r.OutputBytes != 12 || len(rest) != 0 || r.LogIncomplete {
			t.Fatalf("partial %+v %q", r, rest)
		}
	})
	t.Run("cleanup-unconfirmed", func(t *testing.T) {
		t.Parallel()
		// The group's disappearance is not proved: lost (with the known stop
		// ID), cleanup_unconfirmed in the local log only, the slot kept and
		// the node nonaccepting.
		fp := startFakePlane(t)
		tr := startTaskRun(t, fp, taskOpts{})
		ins, run := manuals(t, tr.dir, "a", "m")
		cfg := roleConfig("a", ins, run)
		s := tr.connect(t, 1, 1, cfg)
		st, ch := s.run(t, 1, 0, "stuck")
		ch.prompt(t)
		tr.groups.mu.Lock()
		tr.groups.err = errors.New("group still present")
		tr.groups.mu.Unlock()
		in := stopOf("d")
		s.sendCancel(t, st, in)
		r, _ := s.drain(t, st)
		if r.Outcome != contract.OutcomeLost || r.StopID == nil || *r.StopID != in.ID {
			t.Fatalf("unconfirmed %+v", r)
		}
		if !strings.Contains(tr.logs.String(), `"reason":"cleanup_unconfirmed"`) {
			t.Fatal("cleanup_unconfirmed was not logged locally")
		}
		if n, blocked := tr.super(t).local(keyOf(st.Role)); n != 1 || !blocked || !tr.super(t).nodeBlocked() {
			t.Fatalf("slot %d blocked %v", n, blocked)
		}
	})
	t.Run("duplicate", func(t *testing.T) {
		t.Parallel()
		// The same stop again, or another stop for a latched execution, is a
		// receipt only: the guardian is asked once and the result names the
		// first. A control for an execution the worker does not hold is
		// execution_missing: the attachment ends (retryable) and nothing is
		// applied.
		fp := startFakePlane(t)
		tr := startTaskRun(t, fp, taskOpts{})
		tr.ledger.manualControl = true
		ins, run := manuals(t, tr.dir, "a", "m")
		cfg := roleConfig("a", ins, run)
		s := tr.connect(t, 1, 1, cfg)
		st, ch := s.run(t, 1, 0, "dup")
		ch.prompt(t)
		in := stopOf("e")
		s.sendCancel(t, st, in)
		s.sendCancel(t, st, in)
		s.sendCancel(t, st, stopOf("f"))
		if got := <-tr.ledger.controls; got != in.ID {
			t.Fatalf("guardian cancel %s", got)
		}
		id := in.ID
		tr.guardianOf(t, st.TaskID).stopping(contract.CauseCancelled, &id)
		ch.signaled("SIGTERM")
		r := s.result(t, st)
		if r.Outcome != contract.OutcomeCancelled || *r.StopID != in.ID {
			t.Fatalf("duplicate %+v", r)
		}
		select {
		case extra := <-tr.ledger.controls:
			t.Fatalf("a second guardian cancel %s", extra)
		default:
		}
		other := startBody(9, cfg, 1, 1, "unknown")
		rid := s.nextP()
		s.c.send(contract.ProtocolVersion, contract.FrameTaskCancel, rid, contract.TaskCancelBody{TaskID: other.TaskID, Execution: other.Execution,
			StopID: in.ID, Kind: contract.StopKindCancelled})
		f := s.c.expect(contract.FrameError, rid)
		e, err := contract.ParseErrorBody(f.Body)
		if err != nil || e.Code != contract.CodeUnavailable || e.Details["reason"] != contract.ReasonExecutionMissing {
			t.Fatalf("execution_missing %+v %v", e, err)
		}
		tr.ev.awaitMatch(t, evEnded, nil)
		if ev := tr.ev.await(t, evBackoff); ev.session != 1 {
			t.Fatalf("not retried: %+v", ev)
		}
	})
}

// TestControlRemove is UT FP-4 on the worker, delegated from
// tests/function (TestControlForceRemove): a forced removal's cancelled
// execution of a removed (and re-added) role keeps the node nonaccepting
// until its group is proved gone. Do not rename or skip it.
func TestControlRemove(t *testing.T) {
	t.Parallel()
	t.Run("removed-instance-cleanup", func(t *testing.T) {
		t.Parallel()
		fp := startFakePlane(t)
		tr := startTaskRun(t, fp, taskOpts{})
		ins, run := manuals(t, tr.dir, "a", "m")
		cfg := roleConfig("a", ins, run)
		cfg.Concurrency = 1
		s := tr.connect(t, 1, 1, cfg)
		st, ch := s.run(t, 1, 0, "removed")
		ch.prompt(t)
		s.detach(t, false, st.TaskID)
		// The plane fenced and removed role a and re-added it (a new
		// instance, registration order 2); its old execution is stopped by
		// the durable intent while its cleanup is still in progress.
		release := tr.groups.hold()
		defer release()
		in := stopOf("9")
		c := tr.fp.accept(t)
		c.helloOK(testID)
		c.heartbeatAt(1, 0)
		inv := c.inventory()
		c.reconcileStops(inv, map[string]string{st.TaskID: contract.ActionStopControl}, map[string]contract.StopIntent{st.TaskID: in})
		s = &taskSession{c: c, tr: tr, b: 2, p: 1, gen: 2}
		readd := roleConfig("a", ins, run)
		readd.Concurrency = 1
		c.replaceOrdered("p1", 2, contract.RoleRecord{RoleConfig: readd.Resolved(), RegistrationOrder: 2})
		c.expectReplaceAck("p1", 2)
		tr.ev.awaitMatch(t, evAckWritten, func(ev event) bool { return ev.rev == 2 })
		s.p = 2
		tr.ev.awaitMatch(t, evCycleDone, func(ev event) bool { return ev.rev == 2 })
		// The snapshot's report, then (no change after the passed cycle)
		// the periodic heartbeat.
		s.report(t, 2, func(r contract.RoleStatus) bool { return !r.CanAccept })
		if hb := s.periodic(t, 2); hb.Roles[0].CanAccept {
			t.Fatalf("the new instance accepts while the old group is cleaned: %+v", hb.Roles)
		}
		nb := startBody(2, readd, 2, 2, "new instance")
		nb.RolesRevision = 2
		rid := s.nextP()
		s.c.sendStart(rid, nb)
		wantRefusal(t, s.c.startResult(rid), contract.ReasonLocalFull)
		release()
		r, _ := s.drain(t, st)
		if r.Outcome != contract.OutcomeCancelled || *r.StopID != in.ID {
			t.Fatalf("old execution %+v", r)
		}
		if tr.super(t).nodeBlocked() {
			t.Fatal("the node stayed blocked after the cleanup")
		}
	})
}

// preparingCancel is TestControlCancel/preparing, with the never-released
// guardian's group proved gone or not.
func preparingCancel(t *testing.T, unconfirmed bool) {
	fp := startFakePlane(t)
	tr := startTaskRun(t, fp, taskOpts{})
	tr.ledger.holdReady = make(chan struct{})
	ins, run := manuals(t, tr.dir, "a", "m")
	cfg := roleConfig("a", ins, run)
	s := tr.connect(t, 1, 1, cfg)
	st := startBody(1, cfg, 1, 1, "preparing")
	s.c.sendStart(s.nextP(), st)
	tr.ev.awaitMatch(t, evTaskPreparing, func(ev event) bool { return ev.id == st.TaskID })
	if unconfirmed {
		// The guardian exists (the pre-spawn control check is passed): the
		// control reaches the abandoned preparation, whose group check runs.
		select {
		case id := <-tr.ledger.spawned:
			if id != st.TaskID {
				t.Fatalf("spawned %s", id)
			}
		case <-time.After(testWait):
			t.Fatal("no guardian was spawned")
		}
	}
	s.detach(t, false, st.TaskID)
	if unconfirmed {
		tr.groups.mu.Lock()
		tr.groups.err = errors.New("group still present")
		tr.groups.mu.Unlock()
	}
	in := stopOf("a")
	s, inv := tr.reconnectStops(t, 2, 1, map[string]string{st.TaskID: contract.ActionStopControl}, map[string]contract.StopIntent{st.TaskID: in}, cfg)
	if len(inv) != 1 || inv[0].Phase != contract.PhasePreparing {
		t.Fatalf("inventory %+v", inv)
	}
	// (The latch and exit events precede the snapshot's acknowledgement
	// reconnect awaited; the result frame is the observable outcome.)
	rid, r := s.nextResult(t)
	want, phase := contract.OutcomeCancelled, contract.JournalCompleted
	if unconfirmed {
		want, phase = contract.OutcomeLost, contract.JournalLost
	}
	if r.Outcome != want || r.StopID == nil || *r.StopID != in.ID || r.ExitCode != nil || r.Signal != nil || r.OutputBytes != 0 {
		t.Fatalf("unconfirmed=%v: cancelled before start %+v", unconfirmed, r)
	}
	j := tr.journalNow(t, st.TaskID)
	if j.Phase != phase || j.StopIntent == nil || j.StopIntent.ID != in.ID || j.StartedAt != nil || j.TimeoutPolicy != contract.TimeoutPolicyEnforced {
		t.Fatalf("journal %+v", j)
	}
	s.c.ackResult(rid, st.TaskID, r.Digest, true)
	tr.noChild(t)
	if unconfirmed {
		if !strings.Contains(tr.logs.String(), `"reason":"cleanup_unconfirmed"`) {
			t.Fatal("cleanup_unconfirmed was not logged locally")
		}
		if n, blocked := tr.super(t).local(keyOf(st.Role)); n != 1 || !blocked || !tr.super(t).nodeBlocked() {
			t.Fatalf("unconfirmed: slot %d blocked %v", n, blocked)
		}
		return
	}
	if n, blocked := tr.super(t).local(keyOf(st.Role)); n != 0 || blocked {
		t.Fatalf("slot %d blocked %v", n, blocked)
	}
}
