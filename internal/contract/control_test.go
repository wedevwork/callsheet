package contract

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Iteration 06a contract families: TestControlProtocol (FP-2, delegated
// from tests/function TestControlReconnect together with the plane's and
// the sidecar's), TestControlMigration (FP-8, delegated from
// TestControlLegacy together with the plane's), the journal and guardian
// codecs of FP-4, and BenchmarkControlReplay. Do not rename or skip them.

const (
	ctlEpoch = "0123456789abcdef0123456789abcdef"
	ctlRun   = "fedcba9876543210fedcba9876543210"
	ctlTask  = "t_0123456789abcdef0123456789abcdef"
)

var ctlDigest = strings.Repeat("ab", 32)

func ctlExec() ExecutionToken { return ExecutionToken{Epoch: ctlEpoch, Attachment: 3} }

// ctlResult is a sealed natural result.
func ctlResult(exit int) TaskResultBody {
	msg := "done"
	return TaskResultBody{TaskID: ctlTask, Execution: ctlExec(), Outcome: OutcomeNatural, ExitCode: &exit, FinalMessage: &msg, OutputBytes: 12}.Sealed()
}

// ctlEntries is n inventory entries in phase running.
func ctlEntries(n int) []TaskInventoryEntry {
	out := make([]TaskInventoryEntry, n)
	at := "2026-09-27T10:00:00Z"
	for i := range out {
		id := []byte(ctlTask)
		copy(id[len(id)-2:], []byte{"0123456789abcdef"[i/16], "0123456789abcdef"[i%16]})
		out[i] = TaskInventoryEntry{TaskID: string(id), Execution: ctlExec(), StartDigest: ctlDigest, Phase: PhaseRunning, StartedAt: &at}
	}
	return out
}

// withField returns the JSON object b with key set to raw (or removed
// when raw is empty).
func withField(t testing.TB, b []byte, key, raw string) []byte {
	t.Helper()
	var m map[string]json.RawMessage
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	if raw == "" {
		delete(m, key)
	} else {
		m[key] = json.RawMessage(raw)
	}
	out, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// asSchema2 turns a schema-3 document of legacy history into 06a's exact
// schema-2 form (no stop intent, timeout_enforced false).
func asSchema2(t testing.TB, b []byte) []byte {
	t.Helper()
	return withField(t, withField(t, withField(t, b, "stop_intent", ""), "timeout_enforced", "false"), "schema_version", "2")
}

func mustCompact(t testing.TB, v any) []byte {
	t.Helper()
	b, err := compact(v)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// TestControlProtocol is UT FP-2 on the contract: the protocol 4 frames,
// their strict fields and bounds, the version gate, the sealed result
// digest and the committed acknowledgement.
func TestControlProtocol(t *testing.T) {
	t.Run("version", func(t *testing.T) {
		if ProtocolVersion != 5 {
			t.Fatalf("protocol %d", ProtocolVersion)
		}
		// A protocol 3 (05) peer and an unknown future version are
		// refused before the body, naming both versions.
		for _, v := range []int{4, 6} {
			b := frame(v, FrameTaskInventory, "i1", `{"garbage":true}`)
			f, err := DecodeFrame(b, FromSidecar)
			ce, ok := err.(*Error)
			if !ok || ce.Code != CodeProtocolMismatch || f.RequestID != "i1" || f.Version != v {
				t.Fatalf("version %d: %+v %v", v, f, err)
			}
			l, _ := ce.DetailInt("local_version")
			r, _ := ce.DetailInt("remote_version")
			if l != 5 || r != v || !strings.Contains(ce.Message, "local=5 remote="+FormatInt(v)) {
				t.Fatalf("version %d mismatch %+v", v, ce)
			}
		}
	})
	t.Run("directions", func(t *testing.T) {
		for typ, from := range map[string]Direction{FrameTaskInventory: FromSidecar, FrameTaskReconcileAck: FromSidecar,
			FrameTaskInventoryAck: FromPlane, FrameTaskReconcile: FromPlane} {
			b := frame(ProtocolVersion, typ, "i1", `{}`)
			if _, err := DecodeFrame(b, from); err != nil {
				t.Fatalf("%s from its sender: %v", typ, err)
			}
			if _, err := DecodeFrame(b, 1-from); err == nil || !strings.Contains(err.Error(), "not valid in this direction") {
				t.Fatalf("%s from the other side: %v", typ, err)
			}
		}
		// Protocol 5's control frame and its receipt, each from its sender
		// only; no other control frame exists.
		for typ, from := range map[string]Direction{FrameTaskCancel: FromPlane, FrameTaskCancelAck: FromSidecar} {
			b := frame(ProtocolVersion, typ, "p1", `{}`)
			if _, err := DecodeFrame(b, from); err != nil {
				t.Fatalf("%s from its sender: %v", typ, err)
			}
			if _, err := DecodeFrame(b, 1-from); err == nil || !strings.Contains(err.Error(), "not valid in this direction") {
				t.Fatalf("%s from the other side: %v", typ, err)
			}
		}
		for _, typ := range []string{"task_stop", "task_control", "task_timeout"} {
			if _, err := DecodeFrame(frame(ProtocolVersion, typ, "p1", `{}`), FromPlane); err == nil {
				t.Fatalf("%s accepted", typ)
			}
		}
		for typ, want := range map[string]int{FrameTaskInventory: 64 << 10, FrameTaskReconcile: 64 << 10, FrameTaskInventoryAck: 1 << 10,
			FrameTaskReconcileAck: 1 << 10, FrameTaskResultAck: 1 << 10, FrameTaskResult: 512 << 10} {
			if BodyLimit(typ) != want {
				t.Fatalf("%s limit %d", typ, BodyLimit(typ))
			}
		}
		// A body above its type's limit is refused at the envelope.
		big := `{"pad":"` + strings.Repeat("x", 1<<10) + `"}`
		if _, err := DecodeFrame(frame(ProtocolVersion, FrameTaskInventoryAck, "i1", big), FromPlane); err == nil || !strings.Contains(err.Error(), "exceeds") {
			t.Fatalf("oversized ack: %v", err)
		}
		if _, err := EncodeFrame(ProtocolVersion, FrameTaskReconcileAck, "r1", map[string]string{"pad": strings.Repeat("x", 1<<10)}); err == nil {
			t.Fatal("an oversized outbound ack was encoded")
		}
	})
	t.Run("inventory", func(t *testing.T) {
		// An empty node sends page 0, final, entries [].
		empty := TaskInventoryBody{RunID: ctlRun, Final: true}
		b := mustCompact(t, empty)
		if string(b) != `{"run_id":"`+ctlRun+`","page":0,"final":true,"entries":[]}` {
			t.Fatalf("empty page %s", b)
		}
		if got, err := DecodeTaskInventory(b); err != nil || !got.Final || len(got.Entries) != 0 {
			t.Fatalf("empty page %+v %v", got, err)
		}
		full := TaskInventoryBody{RunID: ctlRun, Page: 7, Entries: ctlEntries(MaxInventoryEntries)}
		b = mustCompact(t, full)
		if len(b) > MaxInventoryBody {
			t.Fatalf("a full page is %d bytes", len(b))
		}
		if got, err := DecodeTaskInventory(b); err != nil || len(got.Entries) != 64 || got.Page != 7 {
			t.Fatalf("full page %v", err)
		}
		at, res := "2026-09-27T10:00:00Z", ctlDigest
		bad := map[string]TaskInventoryBody{
			"run id":           {RunID: "ABC", Final: true},
			"page":             {RunID: ctlRun, Page: -1},
			"65 entries":       {RunID: ctlRun, Entries: ctlEntries(65)},
			"duplicate":        {RunID: ctlRun, Entries: append(ctlEntries(1), ctlEntries(1)...)},
			"task id":          {RunID: ctlRun, Entries: []TaskInventoryEntry{{TaskID: "x", Execution: ctlExec(), StartDigest: ctlDigest, Phase: PhasePreparing}}},
			"start digest":     {RunID: ctlRun, Entries: []TaskInventoryEntry{{TaskID: ctlTask, Execution: ctlExec(), StartDigest: "AB", Phase: PhasePreparing}}},
			"phase":            {RunID: ctlRun, Entries: []TaskInventoryEntry{{TaskID: ctlTask, Execution: ctlExec(), StartDigest: ctlDigest, Phase: "done"}}},
			"preparing start":  {RunID: ctlRun, Entries: []TaskInventoryEntry{{TaskID: ctlTask, Execution: ctlExec(), StartDigest: ctlDigest, Phase: PhasePreparing, StartedAt: &at}}},
			"running result":   {RunID: ctlRun, Entries: []TaskInventoryEntry{{TaskID: ctlTask, Execution: ctlExec(), StartDigest: ctlDigest, Phase: PhaseRunning, ResultDigest: &res}}},
			"result no digest": {RunID: ctlRun, Entries: []TaskInventoryEntry{{TaskID: ctlTask, Execution: ctlExec(), StartDigest: ctlDigest, Phase: PhaseResult}}},
			"lost no digest":   {RunID: ctlRun, Entries: []TaskInventoryEntry{{TaskID: ctlTask, Execution: ctlExec(), StartDigest: ctlDigest, Phase: PhaseLost}}},
		}
		bt := "2026-09-27 10:00"
		bad["started at"] = TaskInventoryBody{RunID: ctlRun, Entries: []TaskInventoryEntry{{TaskID: ctlTask, Execution: ctlExec(), StartDigest: ctlDigest, Phase: PhaseRunning, StartedAt: &bt}}}
		bd := "xyz"
		bad["result digest"] = TaskInventoryBody{RunID: ctlRun, Entries: []TaskInventoryEntry{{TaskID: ctlTask, Execution: ctlExec(), StartDigest: ctlDigest, Phase: PhaseResult, ResultDigest: &bd}}}
		for name, body := range bad {
			if _, err := DecodeTaskInventory(mustCompact(t, body)); CodeOf(err) != CodeInvalidArgument {
				t.Fatalf("%s: %v", name, err)
			}
		}
		// Strict keys: unknown, missing and null members; the body bound.
		for name, raw := range map[string][]byte{
			"unknown":     withField(t, mustCompact(t, empty), "logs", `"x"`),
			"missing":     withField(t, mustCompact(t, empty), "final", ""),
			"null":        withField(t, mustCompact(t, empty), "entries", "null"),
			"stop intent": withField(t, mustCompact(t, empty), "stop_intent", "null"),
			"big":         append(mustCompact(t, full)[:1], append(bytes.Repeat([]byte(" "), MaxInventoryBody), mustCompact(t, full)[1:]...)...),
		} {
			if _, err := DecodeTaskInventory(raw); CodeOf(err) != CodeInvalidArgument {
				t.Fatalf("%s: %v", name, err)
			}
		}
		// Receipt acknowledgement, not reconciliation.
		if a, err := DecodeTaskInventoryAck([]byte(`{"page":3,"received":true}`)); err != nil || a.Page != 3 {
			t.Fatalf("ack %v", err)
		}
		for _, raw := range []string{`{"page":3,"received":false}`, `{"page":-1,"received":true}`, `{"page":3}`, `{"page":3,"received":true,"x":1}`} {
			if _, err := DecodeTaskInventoryAck([]byte(raw)); err == nil {
				t.Fatalf("ack %s accepted", raw)
			}
		}
	})
	t.Run("reconcile", func(t *testing.T) {
		b := mustCompact(t, TaskReconcileBody{Final: true})
		if string(b) != `{"entries":[],"final":true}` {
			t.Fatalf("empty reconcile %s", b)
		}
		var es []TaskReconcileEntry
		for _, e := range ctlEntries(MaxReconcileEntries) {
			es = append(es, TaskReconcileEntry{TaskID: e.TaskID, Execution: e.Execution, Action: ActionStopLost})
		}
		full := mustCompact(t, TaskReconcileBody{Entries: es})
		if len(full) > MaxReconcileBody {
			t.Fatalf("full reconcile %d bytes", len(full))
		}
		if got, err := DecodeTaskReconcile(full); err != nil || len(got.Entries) != 64 || got.Final {
			t.Fatalf("full reconcile %v", err)
		}
		for _, a := range []string{ActionContinue, ActionStopLost, ActionSendResult, ActionForget} {
			body := TaskReconcileBody{Entries: []TaskReconcileEntry{{TaskID: ctlTask, Execution: ctlExec(), Action: a}}, Final: true}
			if _, err := DecodeTaskReconcile(mustCompact(t, body)); err != nil {
				t.Fatalf("%s: %v", a, err)
			}
		}
		for name, body := range map[string]TaskReconcileBody{
			"action":    {Entries: []TaskReconcileEntry{{TaskID: ctlTask, Execution: ctlExec(), Action: "restart"}}},
			"task":      {Entries: []TaskReconcileEntry{{TaskID: "t_x", Execution: ctlExec(), Action: ActionForget}}},
			"duplicate": {Entries: []TaskReconcileEntry{{TaskID: ctlTask, Execution: ctlExec(), Action: ActionForget}, {TaskID: ctlTask, Execution: ctlExec(), Action: ActionContinue}}},
			"65":        {Entries: append(es, TaskReconcileEntry{TaskID: "t_ffffffffffffffffffffffffffffffff", Execution: ctlExec(), Action: ActionForget})},
		} {
			if _, err := DecodeTaskReconcile(mustCompact(t, body)); CodeOf(err) != CodeInvalidArgument {
				t.Fatalf("%s: %v", name, err)
			}
		}
		if _, err := DecodeTaskReconcile(withField(t, b, "stop_id", "null")); err == nil {
			t.Fatal("a stop field was accepted")
		}
		if _, err := DecodeTaskReconcile(append(bytes.Repeat([]byte(" "), MaxReconcileBody), b...)); err == nil {
			t.Fatal("an oversized reconcile page was accepted")
		}
		if _, err := DecodeTaskReconcileAck([]byte(`{"received":true}`)); err != nil {
			t.Fatal(err)
		}
		for _, raw := range []string{`{"received":false}`, `{}`, `{"received":true,"final":true}`} {
			if _, err := DecodeTaskReconcileAck([]byte(raw)); err == nil {
				t.Fatalf("reconcile ack %s accepted", raw)
			}
		}
	})
	t.Run("result", func(t *testing.T) {
		r := ctlResult(0)
		b := mustCompact(t, r)
		got, err := DecodeTaskResult(b)
		if err != nil || got != r && got.Digest != r.Digest {
			t.Fatalf("result %v", err)
		}
		// The digest covers every other field: any change is detected, and
		// a retry of the same frozen outcome has the same digest.
		if ctlResult(0).Digest != r.Digest || ctlResult(1).Digest == r.Digest {
			t.Fatal("the digest is not a function of the fields")
		}
		for name, mut := range map[string]func(*TaskResultBody){
			"exit":      func(x *TaskResultBody) { one := 1; x.ExitCode = &one },
			"output":    func(x *TaskResultBody) { x.OutputBytes++ },
			"execution": func(x *TaskResultBody) { x.Execution.Attachment++ },
			"flag":      func(x *TaskResultBody) { x.LogIncomplete = true },
			"outcome":   func(x *TaskResultBody) { x.Outcome = OutcomeLost },
		} {
			x := r
			mut(&x)
			if _, err := DecodeTaskResult(mustCompact(t, x)); err == nil || !strings.Contains(err.Error(), "digest does not cover") {
				t.Fatalf("%s: %v", name, err)
			}
		}
		// A lost outcome may carry no exit, signal or final message; a
		// natural one keeps 05's exactly-one rule.
		lost := TaskResultBody{TaskID: ctlTask, Execution: ctlExec(), Outcome: OutcomeLost, OutputBytes: 3, LogIncomplete: true}.Sealed()
		if _, err := DecodeTaskResult(mustCompact(t, lost)); err != nil {
			t.Fatalf("lost: %v", err)
		}
		for name, x := range map[string]TaskResultBody{
			"natural without exit": {TaskID: ctlTask, Execution: ctlExec(), Outcome: OutcomeNatural},
			"unknown outcome":      {TaskID: ctlTask, Execution: ctlExec(), Outcome: "exploded"},
			"missing outcome":      {TaskID: ctlTask, Execution: ctlExec()},
		} {
			if _, err := DecodeTaskResult(mustCompact(t, x.Sealed())); CodeOf(err) != CodeInvalidArgument {
				t.Fatalf("%s: %v", name, err)
			}
		}
		for _, key := range []string{"stop_intent", "state"} {
			if _, err := DecodeTaskResult(withField(t, b, key, "null")); err == nil {
				t.Fatalf("%s accepted", key)
			}
		}
		// Protocol 5 carries stop_id on the wire, null included.
		if _, err := DecodeTaskResult(withField(t, b, "stop_id", "")); err == nil {
			t.Fatal("a result without stop_id was accepted")
		}
		if _, err := DecodeTaskResult(withField(t, b, "digest", "")); err == nil {
			t.Fatal("a result without its digest was accepted")
		}
		// The committed acknowledgement.
		ack := TaskResultAckBody{TaskID: ctlTask, Digest: r.Digest, Received: true, Committed: true}
		if a, err := DecodeTaskResultAck(mustCompact(t, ack)); err != nil || !a.Committed {
			t.Fatalf("ack %v", err)
		}
		ack.Committed = false
		if a, err := DecodeTaskResultAck(mustCompact(t, ack)); err != nil || a.Committed {
			t.Fatalf("receipt ack %v", err)
		}
		for name, raw := range map[string][]byte{
			"not received": mustCompact(t, TaskResultAckBody{TaskID: ctlTask, Digest: r.Digest}),
			"digest":       mustCompact(t, TaskResultAckBody{TaskID: ctlTask, Digest: "x", Received: true}),
			"missing":      withField(t, mustCompact(t, ack), "committed", ""),
		} {
			if _, err := DecodeTaskResultAck(raw); err == nil {
				t.Fatalf("ack %s accepted", name)
			}
		}
	})
	t.Run("controls", func(t *testing.T) {
		// Protocol 5's controls are strict: the task_cancel and its receipt
		// (1 KiB each, every field required, no unknown field, cancelled the
		// only plane kind), the reconcile's stop_control with exactly its
		// intent, and the result's stop_id per outcome.
		stop := strings.Repeat("c", 32)
		for _, typ := range []string{FrameTaskCancel, FrameTaskCancelAck} {
			if BodyLimit(typ) != MaxControlBody || MaxControlBody != 1<<10 {
				t.Fatalf("%s limit %d", typ, BodyLimit(typ))
			}
		}
		cb := mustCompact(t, TaskCancelBody{TaskID: ctlTask, Execution: ctlExec(), StopID: stop, Kind: StopKindCancelled})
		if got, err := DecodeTaskCancel(cb); err != nil || got.StopID != stop || got.Execution != ctlExec() {
			t.Fatalf("task_cancel %+v %v", got, err)
		}
		for name, raw := range map[string][]byte{
			"unknown":   withField(t, cb, "reason", `"x"`),
			"no-stop":   withField(t, cb, "stop_id", ""),
			"no-kind":   withField(t, cb, "kind", ""),
			"no-exec":   withField(t, cb, "execution", ""),
			"null-stop": withField(t, cb, "stop_id", "null"),
			"upper":     withField(t, cb, "stop_id", `"`+strings.Repeat("C", 32)+`"`),
			"timed_out": withField(t, cb, "kind", `"timed_out"`),
			"kind":      withField(t, cb, "kind", `"stop"`),
			"task":      withField(t, cb, "task_id", `"t_x"`),
			"oversized": append(bytes.Repeat([]byte(" "), MaxControlBody), cb...),
		} {
			if _, err := DecodeTaskCancel(raw); CodeOf(err) != CodeInvalidArgument {
				t.Fatalf("task_cancel %s: %v", name, err)
			}
		}
		ab := mustCompact(t, TaskCancelAckBody{TaskID: ctlTask, Execution: ctlExec(), StopID: stop, Received: true})
		if got, err := DecodeTaskCancelAck(ab); err != nil || !got.Received {
			t.Fatalf("task_cancel_ack %v", err)
		}
		for name, raw := range map[string][]byte{
			"not-received": withField(t, ab, "received", "false"),
			"unknown":      withField(t, ab, "cleaned", "true"),
			"no-stop":      withField(t, ab, "stop_id", ""),
			"stop":         withField(t, ab, "stop_id", `"x"`),
			"oversized":    append(bytes.Repeat([]byte(" "), MaxControlBody), ab...),
		} {
			if _, err := DecodeTaskCancelAck(raw); CodeOf(err) != CodeInvalidArgument {
				t.Fatalf("task_cancel_ack %s: %v", name, err)
			}
		}
		// The reconcile's stop_control carries exactly its durable intent.
		in := StopIntent{ID: stop, Kind: StopKindCancelled}
		ok := TaskReconcileBody{Entries: []TaskReconcileEntry{{TaskID: ctlTask, Execution: ctlExec(), Action: ActionStopControl, Stop: &in}}, Final: true}
		if got, err := DecodeTaskReconcile(mustCompact(t, ok)); err != nil || got.Entries[0].Stop == nil || *got.Entries[0].Stop != in {
			t.Fatalf("stop_control %+v %v", got, err)
		}
		bad := StopIntent{ID: stop, Kind: OutcomeTimedOut}
		for name, e := range map[string]TaskReconcileEntry{
			"no-intent":   {TaskID: ctlTask, Execution: ctlExec(), Action: ActionStopControl},
			"intent-lost": {TaskID: ctlTask, Execution: ctlExec(), Action: ActionStopLost, Stop: &in},
			"kind":        {TaskID: ctlTask, Execution: ctlExec(), Action: ActionStopControl, Stop: &bad},
		} {
			body := TaskReconcileBody{Entries: []TaskReconcileEntry{e}, Final: true}
			if _, err := DecodeTaskReconcile(mustCompact(t, body)); CodeOf(err) != CodeInvalidArgument {
				t.Fatalf("reconcile %s: %v", name, err)
			}
		}
		// Result stop_id: cancelled and lost may name the intent; natural and
		// timed_out never do; a stop_id changes the digest and a null one
		// keeps protocol 4's.
		sig := "SIGTERM"
		for _, o := range []string{OutcomeCancelled, OutcomeLost} {
			r := TaskResultBody{TaskID: ctlTask, Execution: ctlExec(), Outcome: o, StopID: &stop, Signal: &sig}.Sealed()
			if _, err := DecodeTaskResult(mustCompact(t, r)); err != nil {
				t.Fatalf("%s with stop_id: %v", o, err)
			}
			n := r
			n.StopID = nil
			if n.Sealed().Digest == r.Digest {
				t.Fatalf("%s: stop_id outside the digest", o)
			}
		}
		for _, o := range []string{OutcomeNatural, OutcomeTimedOut} {
			r := TaskResultBody{TaskID: ctlTask, Execution: ctlExec(), Outcome: o, StopID: &stop, Signal: &sig}.Sealed()
			if _, err := DecodeTaskResult(mustCompact(t, r)); CodeOf(err) != CodeInvalidArgument {
				t.Fatalf("%s with stop_id: %v", o, err)
			}
		}
		to := TaskResultBody{TaskID: ctlTask, Execution: ctlExec(), Outcome: OutcomeTimedOut, Signal: &sig}.Sealed()
		if _, err := DecodeTaskResult(mustCompact(t, to)); err != nil {
			t.Fatalf("timed_out: %v", err)
		}
	})
	t.Run("late-log", func(t *testing.T) {
		d := ctlDigest
		lb := TaskLogBody{TaskID: ctlTask, Execution: ctlExec(), Offset: 4, Data: []byte("tail"), LateDigest: &d}
		b := mustCompact(t, lb)
		if got, err := DecodeTaskLog(b); err != nil || got.LateDigest == nil || *got.LateDigest != d {
			t.Fatalf("late log %v", err)
		}
		lb.LateDigest = nil
		if !strings.Contains(string(mustCompact(t, lb)), `"late_digest":null`) {
			t.Fatal("a primary chunk does not carry late_digest null")
		}
		if _, err := DecodeTaskLog(withField(t, b, "late_digest", `"ABC"`)); err == nil {
			t.Fatal("an invalid late digest was accepted")
		}
		if _, err := DecodeTaskLog(withField(t, b, "late_digest", "")); err == nil {
			t.Fatal("a chunk without late_digest was accepted")
		}
	})
	t.Run("start-digest", func(t *testing.T) {
		st := TaskStartBody{TaskID: ctlTask, Execution: ctlExec(), RolesRevision: 1, Role: validRole(), Request: validRequest(),
			Effective: TaskEffective{Model: "m", Effort: "low", Timeout: time.Hour}}
		d := st.StartDigestHex()
		if !ValidDigest(d) || d != st.StartDigestHex() {
			t.Fatalf("digest %s", d)
		}
		st.Request.Goal += "!"
		if st.StartDigestHex() == d {
			t.Fatal("the start digest ignores the request")
		}
		if ValidRunID(strings.Repeat("A", 32)) || !ValidRunID(ctlRun) || ValidDigest(ctlRun) {
			t.Fatal("identifier validation")
		}
	})
}

// schema1 reads a golden iteration 05 document (written by the 05
// encoder, testdata/schema1).
func schema1(t testing.TB, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", "schema1", name+".json"))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// TestControlMigration is UT FP-8 on the contract: golden 05 documents
// decode strictly as schema 1 and convert in memory to schema 2 with the
// legacy timeout policy; a derived recovery flag is never accepted from
// disk; schema 2 carries the start/result digests and the late slot.
func TestControlMigration(t *testing.T) {
	t.Run("golden", func(t *testing.T) {
		for name, state := range map[string]string{"pending": TaskPending, "running": TaskRunning, "succeeded": TaskSucceeded, "failed": TaskFailed,
			"rejected": TaskRejected, "removed-role": TaskSucceeded, "removed-running": TaskRunning} {
			raw := schema1(t, name)
			if bytes.Contains(raw, []byte("recovery_required")) || !bytes.Contains(raw, []byte(`"timeout_enforced": false`)) {
				t.Fatalf("%s is not a 05 document", name)
			}
			rec, err := ParseTaskRecord(raw, testLookup)
			if err != nil {
				t.Fatalf("%s: %v", name, err)
			}
			if rec.Schema != LegacyTaskRecordSchemaVersion || rec.State != state || rec.TimeoutPolicy != TimeoutPolicyLegacy || rec.StartDigest != nil ||
				rec.ResultDigest != nil || rec.Late != nil {
				t.Fatalf("%s converted %+v", name, rec)
			}
			// A schema-3 rewrite round-trips (timeout still unenforced, no
			// stop intent, no timeout_enforced member).
			b, err := EncodeTaskRecord(rec)
			if err != nil {
				t.Fatalf("%s encode: %v", name, err)
			}
			if !bytes.Contains(b, []byte(`"schema_version": 3`)) || bytes.Contains(b, []byte(`"timeout_enforced"`)) || !bytes.Contains(b, []byte(`"stop_intent": null`)) ||
				!bytes.Contains(b, []byte(`"timeout_policy": "legacy_unenforced"`)) || !bytes.Contains(b, []byte(`"start_digest": null`)) {
				t.Fatalf("%s schema 3:\n%s", name, b)
			}
			// Its exact 06a schema-2 form decodes too, reported as schema 2.
			if two, err := ParseTaskRecord(asSchema2(t, b), testLookup); err != nil || two.Schema != TaskRecordSchema2 || two.State != rec.State {
				t.Fatalf("%s schema 2 %+v %v", name, two, err)
			}
			again, err := ParseTaskRecord(b, testLookup)
			if err != nil || again.Schema != TaskRecordSchemaVersion || again.State != rec.State || !bytes.Equal(again.Log.Data, rec.Log.Data) ||
				again.Revision != rec.Revision {
				t.Fatalf("%s round trip %+v %v", name, again, err)
			}
		}
		// The retained 05 tail and result survive exactly.
		rec, _ := ParseTaskRecord(schema1(t, "succeeded"), testLookup)
		if string(rec.Log.Data) != "line one\nline two\n" || *rec.ExitCode != 0 || *rec.FinalMessage != "done" {
			t.Fatalf("succeeded %+v", rec)
		}
	})
	t.Run("strict", func(t *testing.T) {
		raw := schema1(t, "running")
		for name, doc := range map[string][]byte{
			"recovery flag":    withField(t, raw, "recovery_required", "true"),
			"recovery null":    withField(t, raw, "recovery_required", "null"),
			"timeout enforced": withField(t, raw, "timeout_enforced", "true"),
			"lost in 05":       withField(t, withField(t, raw, "state", `"lost"`), "finished_at", `"2026-09-20T10:01:00Z"`),
			"schema 2 fields":  withField(t, raw, "start_digest", "null"),
			"schema 4":         withField(t, raw, "schema_version", "4"),
		} {
			if _, err := ParseTaskRecord(doc, testLookup); err == nil {
				t.Fatalf("%s accepted", name)
			}
		}
		// Schema 2 rejects stop fields even when null, and 06b states.
		rec, _ := ParseTaskRecord(raw, testLookup)
		b3, _ := EncodeTaskRecord(rec)
		b := asSchema2(t, b3)
		if _, err := ParseTaskRecord(b, testLookup); err != nil {
			t.Fatalf("schema 2 base: %v", err)
		}
		for name, doc := range map[string][]byte{
			"enforced flag":  withField(t, b, "timeout_enforced", "true"),
			"stop intent":    withField(t, b, "stop_intent", "null"),
			"stop id":        withField(t, b, "stop_requested", "null"),
			"policy":         withField(t, b, "timeout_policy", `"enforced"`),
			"cancelled":      withField(t, withField(t, b, "state", `"cancelled"`), "finished_at", `"2026-09-20T10:01:00Z"`),
			"missing digest": withField(t, b, "result_digest", ""),
		} {
			if _, err := ParseTaskRecord(doc, testLookup); err == nil {
				t.Fatalf("schema 2 %s accepted", name)
			}
		}
	})
	t.Run("late", func(t *testing.T) {
		rec, _ := ParseTaskRecord(schema1(t, "running"), testLookup)
		fin := rec.StartedAt.Add(time.Minute)
		rec.State, rec.FinishedAt = TaskLost, &fin
		rec.Reason = &TaskReason{Code: ReasonLeaseExpired, Message: "the lease expired"}
		sd := ctlDigest
		rec.StartDigest = &sd
		res := TaskResultBody{TaskID: rec.TaskID, Execution: rec.Execution, Outcome: OutcomeNatural, ExitCode: ptr(0), OutputBytes: 20}.Sealed()
		l := LateFrom(res, fin.Add(time.Second), TaskLog{Data: []byte("late tail"), SourceBytes: 20, ReceivedBytes: 9, Incomplete: true})
		rec.Late = &l
		b, err := EncodeTaskRecord(rec)
		if err != nil {
			t.Fatal(err)
		}
		got, err := ParseTaskRecord(b, testLookup)
		if err != nil || got.Late == nil || string(got.Late.Log.Data) != "late tail" || string(got.Log.Data) != "partial\n" || got.State != TaskLost ||
			got.Late.Digest != res.Digest {
			t.Fatalf("late round trip %+v %v", got.Late, err)
		}
		s := SummaryOf(*got.Late)
		if s.Digest != res.Digest || s.Outcome != OutcomeNatural || s.Log.RetainedBytes != 9 || !s.Log.Truncated || !s.Log.Incomplete {
			t.Fatalf("summary %+v", s)
		}
		// The combined primary and late tails stay within 10 MiB (the
		// bound's arithmetic; 10 MiB documents under the race detector
		// would dominate the packages shard, and the plane's writer trims
		// to it: TestControlLate/budget, BenchmarkControlCommit).
		if checkTails(MaxLogRetainedBytes-4, 5, "record") == nil || checkTails(MaxLogRetainedBytes-5, 5, "record") != nil ||
			checkTails(MaxLogRetainedBytes, 0, "record") != nil {
			t.Fatal("the combined tail bound")
		}
		// A late slot on a nonterminal record is invalid.
		nt := rec
		nt.State, nt.FinishedAt, nt.Reason = TaskRunning, nil, nil
		if b, err := EncodeTaskRecord(nt); err == nil {
			if _, err := ParseTaskRecord(b, testLookup); err == nil {
				t.Fatal("late evidence on a running record was accepted")
			}
		}
	})
}

// TestControlJournalCodec is UT FP-4 on the contract: the sidecar's
// execution journal and the guardian's owner record, invocation, status
// messages, framing and control commands.
func TestControlJournalCodec(t *testing.T) {
	nonce := strings.Repeat("0f", 32)
	base := ExecutionJournal{TaskID: ctlTask, Execution: ctlExec(), StartDigest: ctlDigest, Role: validRole(),
		Effective: TaskEffective{Model: "m", Effort: "low", Timeout: time.Hour}, Phase: JournalPrepared}
	t.Run("execution", func(t *testing.T) {
		started := time.Date(2026, 9, 27, 10, 0, 0, 0, time.UTC)
		res := ctlResult(0)
		lost := TaskResultBody{TaskID: ctlTask, Execution: ctlExec(), Outcome: OutcomeLost, OutputBytes: 5, LogIncomplete: true}.Sealed()
		running := base
		running.Phase, running.OwnerNonce, running.StartedAt = JournalRunning, &nonce, &started
		completed := running
		completed.Phase, completed.Result = JournalCompleted, &res
		completed.Log = TaskLog{Data: []byte("tail"), SourceBytes: 12, ReceivedBytes: 12}
		lostJ := running
		lostJ.Phase, lostJ.Result = JournalLost, &lost
		for name, j := range map[string]ExecutionJournal{"prepared": base, "running": running, "completed": completed, "lost": lostJ} {
			b, err := EncodeExecutionJournal(j)
			if err != nil {
				t.Fatalf("%s: %v", name, err)
			}
			got, err := ParseExecutionJournal(b, testLookup)
			if err != nil || got.Phase != j.Phase || got.TimeoutPolicy != TimeoutPolicyLegacy || !bytes.Equal(got.Log.Data, j.Log.Data) {
				t.Fatalf("%s: %+v %v", name, got, err)
			}
			if bytes.Contains(b, []byte(validRequest().Goal)) {
				t.Fatalf("%s journals the prompt", name)
			}
		}
		other := res
		other.TaskID = "t_ffffffffffffffffffffffffffffffff"
		other = other.Sealed()
		bad := map[string]ExecutionJournal{}
		p := base
		p.OwnerNonce = &nonce
		bad["prepared with nonce"] = p
		r := running
		r.OwnerNonce = nil
		bad["running without nonce"] = r
		c := completed
		c.Result = &lost
		bad["completed lost"] = c
		c2 := completed
		c2.Result = &other
		bad["other execution"] = c2
		c3 := completed
		c3.Result = nil
		bad["completed without result"] = c3
		ph := base
		ph.Phase = "released"
		bad["phase"] = ph
		for name, j := range bad {
			b, err := EncodeExecutionJournal(j)
			if err != nil {
				continue
			}
			if _, err := ParseExecutionJournal(b, testLookup); err == nil {
				t.Fatalf("%s accepted", name)
			}
		}
		b, _ := EncodeExecutionJournal(base)
		for name, doc := range map[string][]byte{
			"schema":  withField(t, b, "schema_version", "3"),
			"unknown": withField(t, b, "prompt", `"x"`),
			"policy":  withField(t, b, "timeout_policy", `"odd"`),
			"digest":  withField(t, b, "start_digest", `"x"`),
			"nonce":   withField(t, b, "owner_nonce", `"x"`),
			"started": withField(t, b, "started_at", `"yesterday"`),
			"big":     append(b, bytes.Repeat([]byte(" "), MaxExecutionJournalBytes)...),
		} {
			if _, err := ParseExecutionJournal(doc, testLookup); err == nil {
				t.Fatalf("journal %s accepted", name)
			}
		}
		huge := completed
		huge.Log = TaskLog{Data: bytes.Repeat([]byte("x"), 13<<20), SourceBytes: 13 << 20, ReceivedBytes: 13 << 20}
		if _, err := EncodeExecutionJournal(huge); err == nil {
			t.Fatal("a journal above 16 MiB was encoded")
		}
	})
	t.Run("owner", func(t *testing.T) {
		o := OwnerRecord{TaskID: ctlTask, Execution: ctlExec(), Nonce: nonce, PGID: 4242, GuardianPID: 4242, Phase: OwnerArmed}
		b, err := EncodeOwner(o)
		if err != nil || len(b) > MaxOwnerBytes {
			t.Fatal(err)
		}
		if got, err := ParseOwner(b); err != nil || got.PGID != 4242 || got.SchemaVersion != 1 {
			t.Fatalf("owner %+v %v", got, err)
		}
		for name, mut := range map[string]func(*OwnerRecord){
			"pid != pgid": func(x *OwnerRecord) { x.GuardianPID = 4243 },
			"pgid 1":      func(x *OwnerRecord) { x.PGID, x.GuardianPID = 1, 1 },
			"phase":       func(x *OwnerRecord) { x.Phase = "running" },
			"nonce":       func(x *OwnerRecord) { x.Nonce = "short" },
			"task":        func(x *OwnerRecord) { x.TaskID = "t_1" },
		} {
			x := o
			mut(&x)
			b, _ := EncodeOwner(x)
			if _, err := ParseOwner(b); err == nil {
				t.Fatalf("owner %s accepted", name)
			}
		}
		if _, err := ParseOwner(withField(t, b, "extra", "1")); err == nil {
			t.Fatal("owner unknown key accepted")
		}
		if _, err := ParseOwner(append(b, bytes.Repeat([]byte(" "), MaxOwnerBytes)...)); err == nil {
			t.Fatal("oversized owner accepted")
		}
	})
	t.Run("guardian", func(t *testing.T) {
		inv := GuardianInvocation{Version: GuardianInvocationVersion, Timeout: "2h0m0s", TimeoutPolicy: TimeoutPolicyEnforced, TaskID: ctlTask, Execution: ctlExec(), StartDigest: ctlDigest, TaskDir: "/state/tasks/" + ctlTask,
			Nonce: nonce, Path: "/bin/fake", Argv: []string{"fake", "--x"}, Env: []string{"A=1"}, Dir: "/tmp/scratch"}
		b, err := EncodeGuardianInvocation(inv)
		if err != nil {
			t.Fatal(err)
		}
		if got, err := ParseGuardianInvocation(b); err != nil || got.TaskDir != inv.TaskDir {
			t.Fatalf("invocation %v", err)
		}
		for name, mut := range map[string]func(*GuardianInvocation){
			"version":  func(x *GuardianInvocation) { x.Version = 1 },
			"timeout":  func(x *GuardianInvocation) { x.Timeout = "2h" },
			"negative": func(x *GuardianInvocation) { x.Timeout = "-1s" },
			"policy":   func(x *GuardianInvocation) { x.TimeoutPolicy = "" },
			"task dir": func(x *GuardianInvocation) { x.TaskDir = "/state/other/" + ctlTask },
			"relative": func(x *GuardianInvocation) { x.Path = "fake" },
			"nul":      func(x *GuardianInvocation) { x.Argv = []string{"a\x00b"} },
			"nonce":    func(x *GuardianInvocation) { x.Nonce = "" },
			"exec":     func(x *GuardianInvocation) { x.Execution.Attachment = 0 },
			"digest":   func(x *GuardianInvocation) { x.StartDigest = "x" },
			"unclean":  func(x *GuardianInvocation) { x.TaskDir = "/state/tasks/../tasks/" + ctlTask },
		} {
			x := inv
			mut(&x)
			if _, err := EncodeGuardianInvocation(x); err == nil {
				t.Fatalf("invocation %s accepted", name)
			}
		}
		if _, err := ParseGuardianInvocation([]byte(`{"version":1}`)); err == nil {
			t.Fatal("partial invocation accepted")
		}
		if _, err := ParseGuardianInvocation(make([]byte, MaxGuardianInvocationBytes+1)); err == nil {
			t.Fatal("oversized invocation accepted")
		}
		// Framing: length prefix, the announced length checked first.
		var buf bytes.Buffer
		if err := WriteFramed(&buf, []byte("hello")); err != nil || buf.Len() != 9 {
			t.Fatal(err)
		}
		if got, err := ReadFramed(bytes.NewReader(buf.Bytes()), 5); err != nil || string(got) != "hello" {
			t.Fatalf("framed %q %v", got, err)
		}
		if _, err := ReadFramed(bytes.NewReader(buf.Bytes()), 4); err == nil {
			t.Fatal("length overflow accepted")
		}
		if _, err := ReadFramed(bytes.NewReader(buf.Bytes()[:6]), 5); err == nil {
			t.Fatal("a short message was accepted")
		}
		if _, err := ReadFramed(bytes.NewReader(nil), 5); err == nil {
			t.Fatal("EOF accepted")
		}
		// Status messages.
		at, code, sig, reason := "2026-09-27T10:00:00Z", 3, "SIGTERM", "start_failed"
		for name, s := range map[string]GuardianStatus{
			"ready":   {Type: GuardianReady, PID: 99, PGID: 99},
			"started": {Type: GuardianStarted, PID: 100, StartedAt: &at},
			"exit":    {Type: GuardianExit, ExitCode: &code},
			"signal":  {Type: GuardianExit, Signal: &sig},
			"error":   {Type: GuardianError, Reason: &reason},
		} {
			s.TaskID, s.Nonce = ctlTask, nonce
			b, err := EncodeGuardianStatus(s)
			if err != nil {
				t.Fatalf("%s: %v", name, err)
			}
			if got, err := ParseGuardianStatus(b); err != nil || got.Type != s.Type {
				t.Fatalf("%s: %v", name, err)
			}
		}
		for name, s := range map[string]GuardianStatus{
			"ready pgid":    {Type: GuardianReady, PID: 99, PGID: 98},
			"started time":  {Type: GuardianStarted, PID: 100},
			"exit both":     {Type: GuardianExit, ExitCode: &code, Signal: &sig},
			"exit neither":  {Type: GuardianExit},
			"error nothing": {Type: GuardianError},
			"type":          {Type: "running"},
		} {
			s.TaskID, s.Nonce = ctlTask, nonce
			if _, err := EncodeGuardianStatus(s); err == nil {
				t.Fatalf("status %s accepted", name)
			}
		}
		if _, err := ParseGuardianStatus(make([]byte, MaxGuardianStatusBytes+1)); err == nil {
			t.Fatal("oversized status accepted")
		}
		if _, err := ParseGuardianStatus([]byte(`{"type":"ready","task_id":"` + ctlTask + `","nonce":"` + nonce + `","pid":9,"pgid":9}`)); err == nil {
			t.Fatal("a status with missing keys was accepted")
		}
		// Commands: one short line; never a PID.
		cmd, err := EncodeGuardianCommand(GuardianCommand{TaskID: ctlTask, Execution: ctlExec(), Nonce: nonce, Command: GuardianStop})
		if err != nil || len(cmd) > MaxGuardianCommandBytes || cmd[len(cmd)-1] != '\n' || bytes.Contains(cmd, []byte("pgid")) {
			t.Fatalf("command %q %v", cmd, err)
		}
		if c, err := ParseGuardianCommand(bytes.TrimSuffix(cmd, []byte("\n"))); err != nil || c.Command != GuardianStop {
			t.Fatalf("command %v", err)
		}
		for name, raw := range map[string]string{
			"kill":    `{"task_id":"` + ctlTask + `","execution":{"epoch":"` + ctlEpoch + `","attachment":3},"nonce":"` + nonce + `","command":"kill"}`,
			"pgid":    `{"task_id":"` + ctlTask + `","execution":{"epoch":"` + ctlEpoch + `","attachment":3},"nonce":"` + nonce + `","command":"stop","pgid":1}`,
			"nonce":   `{"task_id":"` + ctlTask + `","execution":{"epoch":"` + ctlEpoch + `","attachment":3},"nonce":"x","command":"stop"}`,
			"garbage": `stop`,
			"long":    strings.Repeat("x", MaxGuardianCommandBytes),
		} {
			if _, err := ParseGuardianCommand([]byte(raw)); err == nil {
				t.Fatalf("command %s accepted", name)
			}
		}
		if _, err := EncodeGuardianCommand(GuardianCommand{TaskID: strings.Repeat("x", 600)}); err == nil {
			t.Fatal("an oversized command was encoded")
		}
		if !ValidNonce(nonce) || ValidNonce(strings.ToUpper(nonce)) {
			t.Fatal("nonce validation")
		}
	})
}

// BenchmarkControlReplay (contract) encodes one maximum sealed result and
// one 64-entry inventory page, asserting their wire bounds.
func BenchmarkControlReplay(b *testing.B) {
	msg := strings.Repeat("\x02", MaxFinalMessageBytes)
	code := 1
	res := TaskResultBody{TaskID: ctlTask, Execution: ctlExec(), Outcome: OutcomeNatural, ExitCode: &code, FinalMessage: &msg, FinalMessageTruncated: true,
		OutputBytes: MaxSafeInteger, LogIncomplete: true, CounterOverflow: true}.Sealed()
	page := TaskInventoryBody{RunID: ctlRun, Page: 1, Entries: ctlEntries(MaxInventoryEntries)}
	b.Run("result", func(b *testing.B) {
		b.ReportAllocs()
		var n int
		for b.Loop() {
			f, err := EncodeFrame(ProtocolVersion, FrameTaskResult, "b1", res.Sealed())
			if err != nil {
				b.Fatal(err)
			}
			n = len(f)
		}
		if n > MaxTaskResultBody+256 {
			b.Fatalf("maximum result frame %d bytes", n)
		}
		b.ReportMetric(float64(n), "frame-bytes")
	})
	b.Run("inventory", func(b *testing.B) {
		b.ReportAllocs()
		var n int
		for b.Loop() {
			f, err := EncodeFrame(ProtocolVersion, FrameTaskInventory, "i2", page)
			if err != nil {
				b.Fatal(err)
			}
			n = len(f)
		}
		if n > MaxInventoryBody+256 {
			b.Fatalf("64-entry inventory frame %d bytes", n)
		}
		b.ReportMetric(float64(n), "frame-bytes")
	})
}
