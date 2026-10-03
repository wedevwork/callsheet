package sidecar

import (
	"testing"
	"time"

	"github.com/wedevwork/callsheet/internal/contract"
)

// TestControlTimeout is UT FP-2 on the worker and its guardian, delegated
// from tests/function (TestControlExecutionTimeout) with the contract's:
// every start's invocation carries its snapshotted duration (default,
// override, zero) under the enforced policy; the guardian alone arms the
// deadline at the adapter's Start authorization, on its injected clock
// (hours without sleeps); an exit observed strictly before the deadline is
// natural and at it timed_out; a slow Start is cleaned up; a plane outage
// changes nothing; a restart never resumes or reinfers; a failing status
// pipe never holds the escalation. The guardian cases are in
// control_timeout_unix_test.go. Do not rename or skip it.
func TestControlTimeout(t *testing.T) {
	t.Parallel()
	t.Run("default-override-zero", func(t *testing.T) {
		t.Parallel()
		// The invocation (version 2) carries each start's effective timeout
		// and the enforced policy: the role's 2 h default, a 90 m override
		// and zero (unlimited); the guardian arms a timer for the first two
		// only (guardianTimeouts).
		fp := startFakePlane(t)
		tr := startTaskRun(t, fp, taskOpts{})
		ins, run := manuals(t, tr.dir, "a", "m")
		cfg := roleConfig("a", ins, run)
		cfg.Concurrency = 3
		s := tr.connect(t, 1, 1, cfg)
		for i, d := range []time.Duration{2 * time.Hour, 90 * time.Minute, 0} {
			b := startBody(i+1, cfg, 1, 1, "timeout")
			b.Effective.Timeout = d
			rid := s.nextP()
			s.c.sendStart(rid, b)
			if r := s.c.startResult(rid); r.Err != nil {
				t.Fatalf("start %d: %v", i, r.Err)
			}
			tr.child(t)
			g := tr.guardianOf(t, b.TaskID)
			if g.spec.inv.Version != contract.GuardianInvocationVersion || g.spec.inv.Timeout != d.String() ||
				g.spec.inv.TimeoutPolicy != contract.TimeoutPolicyEnforced || g.spec.inv.TimeoutDuration() != d {
				t.Fatalf("invocation %d: %+v", i, g.spec.inv)
			}
		}
		guardianTimeouts(t, "hours")
		guardianTimeouts(t, "zero")
	})
	t.Run("deadline-tie", func(t *testing.T) {
		t.Parallel()
		guardianTimeouts(t, "tie")
	})
	t.Run("slow-start", func(t *testing.T) {
		t.Parallel()
		guardianTimeouts(t, "slow-start")
	})
	t.Run("plane-outage", func(t *testing.T) {
		t.Parallel()
		// The guardian's timer belongs to the group, not the stream: its
		// timeout while the plane is unreachable freezes timed_out (no stop
		// ID, the start kept), reported unchanged on reconnect.
		fp := startFakePlane(t)
		tr := startTaskRun(t, fp, taskOpts{})
		ins, run := manuals(t, tr.dir, "a", "m")
		cfg := roleConfig("a", ins, run)
		s := tr.connect(t, 1, 1, cfg)
		st, ch := s.run(t, 1, 0, "outage")
		ch.prompt(t)
		s.detach(t, false, st.TaskID)
		tr.guardianOf(t, st.TaskID).stopping(contract.CauseTimedOut, nil)
		ch.signaled("SIGTERM")
		tr.settled(t, st.TaskID)
		s, inv := tr.reconnect(t, 2, 1, map[string]string{st.TaskID: contract.ActionSendResult}, cfg)
		if len(inv) != 1 || inv[0].Phase != contract.PhaseResult || inv[0].StartedAt == nil {
			t.Fatalf("inventory %+v", inv)
		}
		r, _ := s.drain(t, st)
		if r.Outcome != contract.OutcomeTimedOut || r.StopID != nil || r.Signal == nil || *r.Signal != "SIGTERM" || r.Digest != *inv[0].ResultDigest {
			t.Fatalf("timed out %+v", r)
		}
	})
	t.Run("restart", func(t *testing.T) {
		t.Parallel()
		// A restart never resumes a timer: a running execution (with or
		// without a latched intent) recovers lost after its cleanup, keeping
		// the intent's stop ID and never inferring cancelled or timed_out; a
		// completed timed_out outbox replays unchanged; a legacy
		// (unenforced, schema 1) journal stays unenforced.
		fp := startFakePlane(t)
		root := newRoot(t)
		g := &fakeGroups{alive: map[int]bool{}}
		var run, timed, legacy contract.TaskStartBody
		var timedRes contract.TaskResultBody
		in := stopOf("3")
		rr := startRecovery(t, fp, root, g, &fakeCommands{}, func(cfg contract.RoleConfig) {
			run = startBody(1, cfg, 1, 1, "running with intent")
			j := journalOf(run, contract.JournalRunning)
			j.StopIntent = &in
			writeJournal(t, root, j, nil)
			timed = startBody(2, cfg, 1, 1, "timed out")
			tj := journalOf(timed, contract.JournalCompleted)
			timedRes = contract.TaskResultBody{TaskID: timed.TaskID, Execution: timed.Execution, Outcome: contract.OutcomeTimedOut, Signal: sp("SIGTERM")}.Sealed()
			tj.Result = &timedRes
			writeJournal(t, root, tj, nil)
			legacy = startBody(3, cfg, 1, 1, "legacy")
			lj, _ := completedJournal(legacy, 0, "")
			lj.TimeoutPolicy = contract.TimeoutPolicyLegacy
			writeJournal(t, root, lj, nil)
		})
		s, inv := rr.reconnect(t, 1, 1, map[string]string{run.TaskID: contract.ActionSendResult, timed.TaskID: contract.ActionSendResult,
			legacy.TaskID: contract.ActionSendResult}, rr.cfg)
		if len(inv) != 3 {
			t.Fatalf("inventory %+v", inv)
		}
		got := map[string]contract.TaskResultBody{}
		for i := 0; i < 3; i++ {
			f := s.request(t)
			rid := f.RequestID
			if f.Type != contract.FrameTaskResult || f.RequestID != rid {
				t.Fatalf("got %s %s", f.Type, f.RequestID)
			}
			r, err := contract.DecodeTaskResult(f.Body)
			if err != nil {
				t.Fatal(err)
			}
			got[r.TaskID] = r
			s.c.ackResult(rid, r.TaskID, r.Digest, true)
		}
		if r := got[run.TaskID]; r.Outcome != contract.OutcomeLost || r.StopID == nil || *r.StopID != in.ID {
			t.Fatalf("running with intent %+v", r)
		}
		if r := got[timed.TaskID]; r.Digest != timedRes.Digest || r.Outcome != contract.OutcomeTimedOut {
			t.Fatalf("replayed timed_out %+v", r)
		}
		if r := got[legacy.TaskID]; r.Outcome != contract.OutcomeNatural {
			t.Fatalf("legacy %+v", r)
		}
		rr.noChild(t)
	})
	t.Run("status-failure", func(t *testing.T) {
		t.Parallel()
		guardianTimeouts(t, "status-failure")
	})
}

func sp(s string) *string { return &s }
