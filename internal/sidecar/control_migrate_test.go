package sidecar

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/wedevwork/callsheet/internal/contract"
)

// Iteration 06b extensions of TestControlRestart and TestControlLate:
// journal migration (06a's schema 1 on disk) and intent replay.

// writeJournalV1 writes j as iteration 06a's schema-1 journal: no
// stop_intent member, a result without stop_id, the legacy policy.
func writeJournalV1(t *testing.T, root string, j contract.ExecutionJournal) {
	t.Helper()
	j.TimeoutPolicy = contract.TimeoutPolicyLegacy
	writeJournal(t, root, j, nil)
	p := filepath.Join(root, journalDir, j.TaskID, executionName)
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	delete(m, "stop_intent")
	m["schema_version"] = json.RawMessage("1")
	if r := m["result"]; string(r) != "null" {
		var rm map[string]json.RawMessage
		if err := json.Unmarshal(r, &rm); err != nil {
			t.Fatal(err)
		}
		delete(rm, "stop_id")
		m["result"], _ = json.Marshal(rm)
	}
	out, _ := json.Marshal(m)
	if err := os.WriteFile(p, out, 0o600); err != nil {
		t.Fatal(err)
	}
	if got, err := contract.ParseExecutionJournal(out, nil); err == nil && got.Schema != contract.LegacyExecutionJournalSchemaVersion {
		t.Fatalf("not a schema 1 journal: %d", got.Schema)
	}
}

// restartMigration: 06a journals on disk after the upgrade. A completed
// schema-1 outbox replays its frozen result with its original digest (a
// null stop_id keeps protocol 4's) and never gains an intent; a running
// schema-1 execution is recovered lost (never resumed, no timer: its
// policy stays legacy_unenforced) and its rewritten journal is schema 2.
func restartMigration(t *testing.T) {
	fp := startFakePlane(t)
	root := newRoot(t)
	g := &fakeGroups{alive: map[int]bool{}}
	var done, running contract.TaskStartBody
	var doneRes contract.TaskResultBody
	rr := startRecovery(t, fp, root, g, &fakeCommands{}, func(cfg contract.RoleConfig) {
		done = startBody(1, cfg, 1, 1, "06a outbox")
		var j contract.ExecutionJournal
		j, doneRes = completedJournal(done, 3, "tail")
		writeJournalV1(t, root, j)
		running = startBody(2, cfg, 1, 1, "06a running")
		writeJournalV1(t, root, journalOf(running, contract.JournalRunning))
	})
	s, inv := rr.reconnect(t, 1, 1, map[string]string{done.TaskID: contract.ActionSendResult, running.TaskID: contract.ActionSendResult}, rr.cfg)
	if len(inv) != 2 {
		t.Fatalf("inventory %+v", inv)
	}
	got := map[string]contract.TaskResultBody{}
	for len(got) < 2 {
		rid := s.nextB()
		f := s.c.recv()
		if f.Type == contract.FrameTaskLog {
			lb, err := contract.DecodeTaskLog(f.Body)
			if err != nil {
				t.Fatal(err)
			}
			s.c.ackLog(rid, lb.TaskID, lb.Offset+len(lb.Data))
			continue
		}
		if f.Type != contract.FrameTaskResult || f.RequestID != rid {
			t.Fatalf("got %s %s", f.Type, f.RequestID)
		}
		r, err := contract.DecodeTaskResult(f.Body)
		if err != nil {
			t.Fatal(err)
		}
		got[r.TaskID] = r
		// Receipt only: the outboxes stay for the journal checks.
		s.c.ackResult(rid, r.TaskID, r.Digest, false)
	}
	if r := got[done.TaskID]; r.Digest != doneRes.Digest || r.StopID != nil || r.Outcome != contract.OutcomeNatural || *r.ExitCode != 3 {
		t.Fatalf("06a outbox %+v", r)
	}
	if r := got[running.TaskID]; r.Outcome != contract.OutcomeLost || r.StopID != nil {
		t.Fatalf("06a running %+v", r)
	}
	if j := rr.journalNow(t, running.TaskID); j.Schema != contract.ExecutionJournalSchemaVersion || j.TimeoutPolicy != contract.TimeoutPolicyLegacy ||
		j.StopIntent != nil || j.Phase != contract.JournalLost {
		t.Fatalf("migrated journal %+v", j)
	}
	if j := rr.journalNow(t, done.TaskID); j.TimeoutPolicy != contract.TimeoutPolicyLegacy || j.StopIntent != nil || j.Result.Digest != doneRes.Digest {
		t.Fatalf("06a outbox journal %+v", j)
	}
	rr.noChild(t)
}

// lateIntentReplay: a durable intent and its frozen cancelled outcome
// survive the sidecar's restart unchanged: the cancelled outbox replays
// (the plane's send_result for its still undecided task) with the same
// stop_id and digest, and the plane's task_cancel redelivering the same
// intent is a receipt that changes nothing (no guardian command, no new
// outcome).
func lateIntentReplay(t *testing.T) {
	fp := startFakePlane(t)
	root := newRoot(t)
	g := &fakeGroups{alive: map[int]bool{}}
	cmds := &fakeCommands{}
	in := stopOf("4")
	var st contract.TaskStartBody
	var res contract.TaskResultBody
	rr := startRecovery(t, fp, root, g, cmds, func(cfg contract.RoleConfig) {
		st = startBody(1, cfg, 1, 1, "cancelled outbox")
		j := journalOf(st, contract.JournalCompleted)
		j.StopIntent = &in
		id := in.ID
		res = contract.TaskResultBody{TaskID: st.TaskID, Execution: st.Execution, Outcome: contract.OutcomeCancelled, StopID: &id, Signal: sp("SIGTERM"),
			OutputBytes: 5, LogIncomplete: true}.Sealed()
		j.Result, j.Log = &res, contract.TaskLog{Data: []byte("lat"), SourceBytes: 5, ReceivedBytes: 3, Incomplete: true}
		writeJournal(t, root, j, nil)
	})
	s, inv := rr.reconnect(t, 1, 1, map[string]string{st.TaskID: contract.ActionSendResult}, rr.cfg)
	if len(inv) != 1 || inv[0].Phase != contract.PhaseResult || inv[0].ResultDigest == nil || *inv[0].ResultDigest != res.Digest {
		t.Fatalf("inventory %+v", inv)
	}
	var tail []byte
	for {
		rid := s.nextB()
		f := s.c.recv()
		if f.Type == contract.FrameTaskLog {
			lb, err := contract.DecodeTaskLog(f.Body)
			if err != nil || lb.LateDigest == nil || *lb.LateDigest != res.Digest {
				t.Fatalf("late log %+v %v", lb, err)
			}
			tail = append(tail, lb.Data...)
			s.c.ackLog(rid, lb.TaskID, lb.Offset+len(lb.Data))
			continue
		}
		r, err := contract.DecodeTaskResult(f.Body)
		if err != nil || f.RequestID != rid || r.Digest != res.Digest || r.StopID == nil || *r.StopID != in.ID || r.Outcome != contract.OutcomeCancelled {
			t.Fatalf("replayed %s %+v %v", f.Type, r, err)
		}
		// The result's request slot is held until its ack: the task_cancel
		// receipt is the next frame.
		if _, a := s.sendCancel(t, st, in); !a.Received {
			t.Fatalf("redelivered intent %+v", a)
		}
		s.c.ackResult(rid, r.TaskID, r.Digest, true)
		break
	}
	if string(tail) != "lat" {
		t.Fatalf("tail %q", tail)
	}
	rr.ev.awaitMatch(t, evTaskForgotten, func(ev event) bool { return ev.id == st.TaskID })
	if sent := cmds.all(); len(sent) != 0 {
		t.Fatalf("a decided execution got guardian commands %v", sent)
	}
	rr.noChild(t)
}
