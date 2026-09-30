package sidecar

import (
	"bytes"
	"os"
	"testing"

	"github.com/wedevwork/callsheet/internal/contract"
)

// replaceRecords sends a full snapshot rid of explicit records.
func (c *fakeConn) replaceRecords(rid string, rev int, recs ...contract.RoleRecord) {
	c.t.Helper()
	c.send(contract.ProtocolVersion, contract.FrameRolesReplace, rid, contract.RolesReplaceBody{Revision: rev, Roles: recs})
}

// install replaces the session's snapshot with recs as revision rev and
// waits for the acknowledgement's write.
func (s *taskSession) install(t *testing.T, rev int, recs ...contract.RoleRecord) {
	t.Helper()
	rid := s.nextP()
	s.c.replaceRecords(rid, rev, recs...)
	s.c.expectReplaceAck(rid, rev)
}

// detach ends the session: the plane closes the stream, or (byAck) a
// heartbeat is left unacknowledged past its bound. It returns once the
// workers of fenced (in order) were fenced, the session ended and the
// backoff timer is armed.
func (s *taskSession) detach(t *testing.T, byAck bool, fenced ...string) {
	t.Helper()
	tr := s.tr
	if byAck {
		tr.clk.Advance(heartbeatInterval)
		rid := s.nextB()
		k := rid[1:]
		s.c.expect(contract.FrameHeartbeat, rid)
		tr.ev.awaitMatch(t, evAwaitReply, func(ev event) bool { return itoa(ev.acks) == k })
		tr.clk.Advance(stepTimeout)
	} else {
		s.c.finish()
	}
	// Fencing happens in the session's cleanup, before it ends.
	for _, id := range fenced {
		tr.ev.awaitMatch(t, evTaskFenced, func(ev event) bool { return ev.id == id })
	}
	tr.advanceBackoff(t, backoff(0, 0.5))
}

// sidecarRemainingCapacity is the shared remaining-capacity contract on
// the worker (06a boundary): with one started child whose attachment
// ended, the new attachment's inventory reports it running, the plane
// reconciles it (continue), the snapshot is installed and acknowledged,
// the worker reports ready with the old child still counted, and a second
// task of the same instance starts; the old child's guardian is never
// signaled, its start is never replayed, and its output and result now
// flow on the reconciled attachment.
func sidecarRemainingCapacity(t *testing.T, byAck bool) {
	fp := startFakePlane(t)
	tr := startTaskRun(t, fp, taskOpts{})
	ins, run := manuals(t, tr.dir, "a", "m")
	cfg := roleConfig("a", ins, run)
	s := tr.connect(t, 1, 1, cfg)
	st1, ch1 := s.run(t, 1, 0, "old")
	ch1.prompt(t)
	s.detach(t, byAck, st1.TaskID)
	s, inv := tr.reconnect(t, 2, 2, map[string]string{st1.TaskID: contract.ActionContinue}, cfg)
	if len(inv) != 1 || inv[0].TaskID != st1.TaskID || inv[0].Phase != contract.PhaseRunning || inv[0].StartedAt == nil || inv[0].Execution != st1.Execution ||
		inv[0].StartDigest != st1.StartDigestHex() {
		t.Fatalf("inventory %+v", inv)
	}
	tr.ev.awaitMatch(t, evCycleDone, func(ev event) bool { return ev.rev == 2 })
	tr.clk.Advance(heartbeatInterval)
	if hb := s.beat(t, 2); hb.Roles[0].Inflight != 1 || !hb.Roles[0].CanAccept {
		t.Fatalf("ready heartbeat with the old child %+v", hb.Roles)
	}
	st2, ch2 := s.run(t, 2, 0, "new")
	if st2.Role.RegistrationOrder != 1 {
		t.Fatal("not the same instance")
	}
	ch2.prompt(t)
	if tr.super(t).find(st1.TaskID) == nil || len(tr.groups.signals()) != 0 {
		t.Fatal("the old child was dropped or signaled")
	}
	// The old child exits: its output and result reach the reconciled
	// attachment; its slot is released.
	ch1.out([]byte("old output\n"))
	ch1.exitCode(0)
	tr.settled(t, st1.TaskID)
	if r, out := s.drain(t, st1); string(out) != "old output\n" || *r.ExitCode != 0 || r.Outcome != contract.OutcomeNatural {
		t.Fatalf("old child on the reconciled attachment: %+v %q", r, out)
	}
	ch2.out([]byte("new\n"))
	if out := s.logs(t, st2, 4); string(out) != "new\n" {
		t.Fatalf("new attachment carried %q", out)
	}
	ch2.exitCode(0)
	if r := s.result(t, st2); r.TaskID != st2.TaskID {
		t.Fatal("wrong result")
	}
	tr.clk.Advance(heartbeatInterval)
	if hb := s.beat(t, 2); hb.Roles[0].Inflight != 0 {
		t.Fatalf("after both exits %+v", hb.Roles)
	}
}

// sidecarRecoveryRemove is the shared removal contract on the worker (06a
// replaced 05's recovery-only removal): the plane resolved the old
// child's execution lost (its node's lease expired) and tells the
// returning worker to stop it (stop_lost). Its instance is removed from
// the snapshot and re-added as a new instance (new order): while the old
// group's cleanup is unresolved the worker accepts nothing, node-wide, so
// remove/re-add cannot bypass physical isolation; once its guardian's
// cleanup ends and the group is proved gone, the new instance starts at
// zero, and the stopped execution's outcome is sent as late evidence
// (lost, its tail tagged with its digest).
func sidecarRecoveryRemove(t *testing.T, byAck bool) {
	sidecarRecoveryRemoveOpts(t, byAck, taskOpts{})
}

// sidecarRecoveryRemoveOpts is sidecarRecoveryRemove on a task run
// adjusted by o (a delayed drain: TestControlLeaseDelayedDrain).
func sidecarRecoveryRemoveOpts(t *testing.T, byAck bool, o taskOpts) {
	fp := startFakePlane(t)
	tr := startTaskRun(t, fp, o)
	ins, run := manuals(t, tr.dir, "a", "m")
	cfg := roleConfig("a", ins, run)
	cfg.Concurrency = 1
	s := tr.connect(t, 1, 1, cfg)
	st1, ch1 := s.run(t, 1, 0, "old")
	ch1.prompt(t)
	s.detach(t, byAck, st1.TaskID)
	// Output while detached stays unsent until the lost outcome's replay.
	ch1.out([]byte("tail\n"))
	// Hold the stopped group's disappearance: its cleanup stays
	// unresolved.
	release := tr.groups.hold()
	defer release()
	s, _ = tr.reconnect(t, 2, 2, map[string]string{st1.TaskID: contract.ActionStopLost})
	if sig := tr.groups.awaitSignal(t, ch1.gpid); len(sig) != 1 || sig[0] != ch1.gpid {
		t.Fatalf("stop_lost signaled %v, want the old guardian %d", sig, ch1.gpid)
	}
	// The TERMed child's "tail" may still be in its pipe: the supervisor
	// reaches the (held) group check only after both drains finished, so
	// moving the clock before that could fire the drain bound and mark
	// the log incomplete.
	tr.groups.awaitGone(t, ch1.gpid)
	tr.clk.Advance(heartbeatInterval)
	if hb := s.beat(t, 2); len(hb.Roles) != 0 {
		t.Fatalf("the removed instance is still reported: %+v", hb.Roles)
	}
	s.install(t, 3, contract.RoleRecord{RoleConfig: cfg, RegistrationOrder: 2})
	s.cfgs = []contract.RoleConfig{cfg}
	tr.ev.awaitMatch(t, evCycleDone, func(ev event) bool { return ev.rev == 3 })
	tr.clk.Advance(heartbeatInterval)
	cadence := tr.clk.Now() // the next ready-check cycle starts here
	if hb := s.beat(t, 3); hb.Roles[0].Inflight != 0 || hb.Roles[0].CanAccept {
		t.Fatalf("the node accepted with an old group's cleanup unresolved: %+v", hb.Roles)
	}
	blocked := startBody(3, cfg, 2, 2, "while cleaning")
	blocked.RolesRevision = 3
	brid := s.nextP()
	s.c.sendStart(brid, blocked)
	wantRefusal(t, s.c.startResult(brid), contract.ReasonLocalFull)
	// The guardian TERMed the old child; once its group is proved gone
	// the node accepts again, and its lost outcome (late) and tail are
	// sent.
	release()
	tr.settled(t, st1.TaskID)
	r, out := s.drain(t, st1)
	if r.Outcome != contract.OutcomeLost || r.Signal == nil || *r.Signal != "SIGTERM" || string(out) != "tail\n" {
		t.Fatalf("stopped execution %+v %q", r, out)
	}
	// The cycle that keeps readiness fresh at the next heartbeat
	// completed before time moves on.
	tr.ev.awaitCycleSince(t, 3, cadence)
	tr.clk.Advance(heartbeatInterval)
	if hb := s.beat(t, 3); hb.Roles[0].Inflight != 0 || !hb.Roles[0].CanAccept {
		t.Fatalf("the re-added instance inherited activity: %+v", hb.Roles)
	}
	nb := startBody(2, cfg, 2, 2, "new instance")
	nb.RolesRevision = 3
	rid := s.nextP()
	s.c.sendStart(rid, nb)
	if r := s.c.startResult(rid); r.Err != nil {
		t.Fatalf("new instance start %+v", r)
	}
	tr.ev.awaitMatch(t, evStartReplied, func(ev event) bool { return ev.id == nb.TaskID })
	ch2 := tr.child(t)
	ch2.prompt(t)
	ch2.exitCode(0)
	if r, _ := s.drain(t, nb); r.TaskID != nb.TaskID {
		t.Fatal("wrong result")
	}
}

// TestTaskRoleIntegration is UT FP-8 on the worker: local counts in
// heartbeats, snapshot changes while children run; its counts, mutation,
// remaining-capacity and recovery-remove subtests are delegated from
// tests/function (TestTaskRoles), together with the plane's. Do not
// rename or skip them.
func TestTaskRoleIntegration(t *testing.T) {
	t.Parallel()
	t.Run("counts", func(t *testing.T) {
		t.Parallel()
		// Heartbeat inflight is the local occupied count of the instance:
		// it may exceed a lowered concurrency (can_accept false), a
		// refusal releases at once, and exit plus drain releases before
		// the result's acknowledgement.
		fp := startFakePlane(t)
		tr := startTaskRun(t, fp, taskOpts{})
		ins, run := manuals(t, tr.dir, "a", "m")
		cfg := roleConfig("a", ins, run)
		s := tr.connect(t, 1, 1, cfg)
		st1, ch1 := s.run(t, 1, 0, "one")
		st2, ch2 := s.run(t, 2, 0, "two")
		ch1.prompt(t)
		ch2.prompt(t)
		tr.clk.Advance(heartbeatInterval)
		if hb := s.beat(t, 1); hb.Roles[0].Inflight != 2 || hb.Roles[0].CanAccept {
			t.Fatalf("full %+v", hb.Roles)
		}
		lowered := cfg
		lowered.Concurrency = 1
		s.install(t, 2, contract.RoleRecord{RoleConfig: lowered, RegistrationOrder: 1})
		s.cfgs = []contract.RoleConfig{lowered}
		tr.clk.Advance(heartbeatInterval)
		if hb := s.beat(t, 2); hb.Roles[0].Inflight != 2 || hb.Roles[0].Concurrency != 1 || hb.Roles[0].CanAccept {
			t.Fatalf("lowered %+v", hb.Roles)
		}
		b := startBody(3, lowered, 1, 1, "over")
		b.RolesRevision = 2
		rid := s.nextP()
		s.c.sendStart(rid, b)
		wantRefusal(t, s.c.startResult(rid), contract.ReasonLocalFull)
		// Exit and drain release the slot before the result is sent or
		// acknowledged (observed at the exit event, the ack withheld).
		ch1.exitCode(0)
		tr.settled(t, st1.TaskID)
		if n, _ := tr.super(t).local(keyOf(contract.RoleRecord{RoleConfig: lowered, RegistrationOrder: 1})); n != 1 {
			t.Fatalf("occupied %d after the exit", n)
		}
		s.result(t, st1)
		tr.clk.Advance(heartbeatInterval)
		if hb := s.beat(t, 2); hb.Roles[0].Inflight != 1 {
			t.Fatalf("after the first exit %+v", hb.Roles)
		}
		ch2.exitCode(0)
		s.result(t, st2)
		tr.clk.Advance(heartbeatInterval)
		if hb := s.beat(t, 2); hb.Roles[0].Inflight != 0 {
			t.Fatalf("after both %+v", hb.Roles)
		}
	})
	t.Run("mutation", func(t *testing.T) {
		t.Parallel()
		// A running child keeps the prompt it was started with; a later
		// start reads the manuals as they are now; the snapshot's
		// installation keeps the active count; a removed instance leaves
		// the heartbeat while its child keeps running.
		fp := startFakePlane(t)
		tr := startTaskRun(t, fp, taskOpts{})
		ins, run := manuals(t, tr.dir, "a", "OLD-CONTENT")
		ins2, run2 := manuals(t, tr.dir, "b", "b")
		cfg, other := roleConfig("a", ins, run), roleConfig("b", ins2, run2)
		s := tr.connect(t, 1, 1, cfg, other)
		st1, ch1 := s.run(t, 1, 0, "first")
		if p := ch1.prompt(t); !bytes.Contains(p, []byte("OLD-CONTENT")) {
			t.Fatal("old content missing")
		}
		os.WriteFile(ins, []byte("NEW-CONTENT"), 0o644)
		st2, ch2 := s.run(t, 2, 0, "second")
		if p := ch2.prompt(t); !bytes.Contains(p, []byte("NEW-CONTENT")) {
			t.Fatal("a start did not read the manual at start")
		}
		s.install(t, 2, contract.RoleRecord{RoleConfig: cfg, RegistrationOrder: 1}, contract.RoleRecord{RoleConfig: other, RegistrationOrder: 2})
		tr.clk.Advance(heartbeatInterval)
		if hb := s.beat(t, 2); hb.Roles[0].Inflight != 2 {
			t.Fatalf("installation reset the count %+v", hb.Roles)
		}
		s.install(t, 3, contract.RoleRecord{RoleConfig: other, RegistrationOrder: 2})
		tr.clk.Advance(heartbeatInterval)
		if hb := s.beat(t, 3); len(hb.Roles) != 1 || hb.Roles[0].RoleID != "b" || hb.Roles[0].Inflight != 0 {
			t.Fatalf("after removal %+v", hb.Roles)
		}
		if len(tr.groups.signals()) != 0 {
			t.Fatal("removal signaled a child")
		}
		ch1.exitCode(0)
		s.result(t, st1)
		ch2.exitCode(0)
		s.result(t, st2)
	})
	t.Run("remaining-capacity", func(t *testing.T) {
		t.Parallel()
		sidecarRemainingCapacity(t, false)
	})
	t.Run("recovery-remove", func(t *testing.T) {
		t.Parallel()
		sidecarRecoveryRemove(t, false)
	})
}
