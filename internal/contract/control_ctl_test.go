package contract

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

// Iteration 06b contract families: TestControlTimeout (FP-2) and
// TestControlWait (FP-3), and the protocol 5 control frame codecs.

// ctlRecord is a valid enforced running schema-3 record.
func ctlRecord(t *testing.T) TaskRecord {
	t.Helper()
	created := time.Date(2026, 9, 27, 10, 0, 0, 0, time.UTC)
	started := created.Add(time.Second)
	sd := ctlDigest
	return TaskRecord{TaskID: ctlTask, Request: validRequest(), Role: validRole(), RolesRevision: 1, Effective: TaskEffective{Model: "example", Effort: "medium",
		Timeout: 2 * time.Hour}, Execution: ctlExec(), State: TaskRunning, CreatedAt: created, StartedAt: &started, Revision: 3, StartDigest: &sd,
		TimeoutPolicy: TimeoutPolicyEnforced, Log: TaskLog{Data: []byte("x\n"), SourceBytes: 2, ReceivedBytes: 2}}
}

func roundTrip(t *testing.T, r TaskRecord) (TaskRecord, error) {
	t.Helper()
	b, err := EncodeTaskRecord(r)
	if err != nil {
		t.Fatal(err)
	}
	return ParseTaskRecord(b, testLookup)
}

func iptr(n int) *int { return &n }

// TestControlTimeout is UT FP-2 on the contract, delegated from
// tests/function (TestControlExecutionTimeout): the enforced policy and
// timed_out/cancelled lifecycle invariants, the result outcomes with
// their stop ID rules and digests, and the migration of 06a documents
// that stay unenforced. Do not rename or skip it.
func TestControlTimeout(t *testing.T) {
	t.Run("policy", func(t *testing.T) {
		// New records are enforced, history legacy; both round-trip; any
		// other policy is refused; the guardian invocation carries the
		// canonical duration (zero: unlimited) and the policy.
		for _, p := range []string{TimeoutPolicyEnforced, TimeoutPolicyLegacy} {
			r := ctlRecord(t)
			r.TimeoutPolicy = p
			if got, err := roundTrip(t, r); err != nil || got.TimeoutPolicy != p || got.Schema != TaskRecordSchemaVersion {
				t.Fatalf("%s: %+v %v", p, got, err)
			}
		}
		r := ctlRecord(t)
		r.TimeoutPolicy = "sometimes"
		if _, err := roundTrip(t, r); err == nil {
			t.Fatal("an unknown policy was accepted")
		}
		for d, want := range map[time.Duration]string{2 * time.Hour: "2h0m0s", 90 * time.Minute: "1h30m0s", 0: "0s"} {
			inv := GuardianInvocation{Version: GuardianInvocationVersion, TaskID: ctlTask, Execution: ctlExec(), StartDigest: ctlDigest,
				TaskDir: "/state/tasks/" + ctlTask, Nonce: strings.Repeat("c", 64), Path: "/bin/fake", Dir: "/tmp/s", Timeout: d.String(),
				TimeoutPolicy: TimeoutPolicyEnforced}
			b, err := EncodeGuardianInvocation(inv)
			if err != nil || !strings.Contains(string(b), `"timeout":"`+want+`"`) {
				t.Fatalf("invocation %v: %s %v", d, b, err)
			}
			if got, err := ParseGuardianInvocation(b); err != nil || got.TimeoutDuration() != d {
				t.Fatalf("invocation %v: %v", d, err)
			}
		}
	})
	t.Run("outcome", func(t *testing.T) {
		// timed_out: both timestamps, at most one of exit and signal, no
		// reason, the enforced policy; cancelled: with a start (no reason)
		// or without one (cancelled_before_start); results carry stop_id
		// only for cancelled and lost; a null stop_id keeps the protocol 4
		// digest; guardian statuses carry the stopping cause.
		base := ctlRecord(t)
		fin := base.StartedAt.Add(time.Hour)
		ok := map[string]func(*TaskRecord){
			"timed_out signal": func(r *TaskRecord) { r.State, r.FinishedAt, r.Signal = TaskTimedOut, &fin, s("SIGTERM") },
			"timed_out bare":   func(r *TaskRecord) { r.State, r.FinishedAt = TaskTimedOut, &fin },
			"cancelled exit": func(r *TaskRecord) {
				r.State, r.FinishedAt, r.ExitCode = TaskCancelled, &fin, iptr(143)
				r.StopIntent = &StopIntent{ID: strings.Repeat("a", 32), Kind: StopKindCancelled, RequestedAt: fin}
			},
			"cancelled before start": func(r *TaskRecord) {
				r.State, r.FinishedAt, r.StartedAt = TaskCancelled, &fin, nil
				r.Reason = &TaskReason{Code: ReasonCancelledBeforeStart, Message: "cancelled before start"}
			},
			"lost with intent": func(r *TaskRecord) {
				r.State, r.FinishedAt = TaskLost, &fin
				r.Reason = &TaskReason{Code: ReasonLeaseExpired, Message: "lease"}
				r.StopIntent = &StopIntent{ID: strings.Repeat("b", 32), Kind: StopKindCancelled, RequestedAt: fin}
			},
			"pending with intent": func(r *TaskRecord) {
				r.State, r.StartedAt = TaskPending, nil
				r.StopIntent = &StopIntent{ID: strings.Repeat("c", 32), Kind: StopKindCancelled, RequestedAt: fin}
			},
		}
		for name, mut := range ok {
			r := base
			mut(&r)
			got, err := roundTrip(t, r)
			if err != nil || got.State != r.State || (r.StopIntent != nil) != (got.StopIntent != nil) {
				t.Fatalf("%s: %+v %v", name, got, err)
			}
		}
		bad := map[string]func(*TaskRecord){
			"timed_out legacy":   func(r *TaskRecord) { r.State, r.FinishedAt, r.TimeoutPolicy = TaskTimedOut, &fin, TimeoutPolicyLegacy },
			"timed_out no start": func(r *TaskRecord) { r.State, r.FinishedAt, r.StartedAt = TaskTimedOut, &fin, nil },
			"timed_out both": func(r *TaskRecord) {
				r.State, r.FinishedAt, r.ExitCode, r.Signal = TaskTimedOut, &fin, iptr(1), s("SIGKILL")
			},
			"timed_out reason": func(r *TaskRecord) {
				r.State, r.FinishedAt, r.Reason = TaskTimedOut, &fin, &TaskReason{Code: ReasonWorkerLost}
			},
			"cancelled no reason": func(r *TaskRecord) { r.State, r.FinishedAt, r.StartedAt = TaskCancelled, &fin, nil },
			"cancelled wrong reason": func(r *TaskRecord) {
				r.State, r.FinishedAt, r.Reason = TaskCancelled, &fin, &TaskReason{Code: ReasonLeaseExpired}
			},
			"intent on success": func(r *TaskRecord) {
				r.State, r.FinishedAt, r.ExitCode = TaskSucceeded, &fin, iptr(0)
				r.StopIntent = &StopIntent{ID: strings.Repeat("d", 32), Kind: StopKindCancelled, RequestedAt: fin}
			},
			"intent kind": func(r *TaskRecord) {
				r.StopIntent = &StopIntent{ID: strings.Repeat("d", 32), Kind: OutcomeTimedOut, RequestedAt: fin}
			},
			"intent id": func(r *TaskRecord) { r.StopIntent = &StopIntent{ID: "x", Kind: StopKindCancelled, RequestedAt: fin} },
		}
		for name, mut := range bad {
			r := base
			mut(&r)
			if _, err := roundTrip(t, r); err == nil {
				t.Fatalf("%s accepted", name)
			}
		}
		id := strings.Repeat("e", 32)
		for name, c := range map[string]struct {
			r  TaskResultBody
			ok bool
		}{
			"timed_out":            {TaskResultBody{TaskID: ctlTask, Execution: ctlExec(), Outcome: OutcomeTimedOut, Signal: s("SIGTERM")}, true},
			"timed_out stop":       {TaskResultBody{TaskID: ctlTask, Execution: ctlExec(), Outcome: OutcomeTimedOut, StopID: &id}, false},
			"cancelled stop":       {TaskResultBody{TaskID: ctlTask, Execution: ctlExec(), Outcome: OutcomeCancelled, StopID: &id}, true},
			"cancelled no stop":    {TaskResultBody{TaskID: ctlTask, Execution: ctlExec(), Outcome: OutcomeCancelled}, true},
			"cancelled both":       {TaskResultBody{TaskID: ctlTask, Execution: ctlExec(), Outcome: OutcomeCancelled, ExitCode: iptr(1), Signal: s("SIGTERM")}, false},
			"lost stop":            {TaskResultBody{TaskID: ctlTask, Execution: ctlExec(), Outcome: OutcomeLost, StopID: &id}, true},
			"natural stop":         {TaskResultBody{TaskID: ctlTask, Execution: ctlExec(), Outcome: OutcomeNatural, ExitCode: iptr(0), StopID: &id}, false},
			"invalid stop":         {TaskResultBody{TaskID: ctlTask, Execution: ctlExec(), Outcome: OutcomeCancelled, StopID: s("ABC")}, false},
			"timed_out no exit ok": {TaskResultBody{TaskID: ctlTask, Execution: ctlExec(), Outcome: OutcomeTimedOut}, true},
		} {
			sealed := c.r.Sealed()
			_, err := DecodeTaskResult(mustCompact(t, sealed))
			if (err == nil) != c.ok {
				t.Fatalf("%s: %v", name, err)
			}
		}
		// A null stop_id keeps the protocol 4 digest (old outboxes stay
		// valid); a nonnull one is covered by it.
		r := ctlResult(0)
		old := HexDigestOf(t, r)
		if r.Digest != old {
			t.Fatalf("null stop_id digest %s, protocol 4 %s", r.Digest, old)
		}
		r2 := TaskResultBody{TaskID: ctlTask, Execution: ctlExec(), Outcome: OutcomeLost, StopID: &id}.Sealed()
		r3 := r2
		other := strings.Repeat("f", 32)
		r3.StopID = &other
		if _, err := DecodeTaskResult(mustCompact(t, r3)); err == nil {
			t.Fatal("a changed stop_id kept its digest")
		}
		// Guardian stopping statuses.
		nonce := strings.Repeat("c", 64)
		for name, c := range map[string]struct {
			st GuardianStatus
			ok bool
		}{
			"cancelled":  {GuardianStatus{Type: GuardianStopping, TaskID: ctlTask, Nonce: nonce, Cause: s(CauseCancelled), StopID: &id}, true},
			"timed_out":  {GuardianStatus{Type: GuardianStopping, TaskID: ctlTask, Nonce: nonce, Cause: s(CauseTimedOut)}, true},
			"lost":       {GuardianStatus{Type: GuardianStopping, TaskID: ctlTask, Nonce: nonce, Cause: s(CauseLost)}, true},
			"no id":      {GuardianStatus{Type: GuardianStopping, TaskID: ctlTask, Nonce: nonce, Cause: s(CauseCancelled)}, false},
			"id late":    {GuardianStatus{Type: GuardianStopping, TaskID: ctlTask, Nonce: nonce, Cause: s(CauseTimedOut), StopID: &id}, false},
			"natural":    {GuardianStatus{Type: GuardianStopping, TaskID: ctlTask, Nonce: nonce, Cause: s("natural")}, false},
			"exit+cause": {GuardianStatus{Type: GuardianExit, TaskID: ctlTask, Nonce: nonce, ExitCode: iptr(0), Cause: s(CauseLost)}, false},
			"pid":        {GuardianStatus{Type: GuardianStopping, TaskID: ctlTask, Nonce: nonce, Cause: s(CauseLost), PID: 3}, false},
		} {
			b, err := EncodeGuardianStatus(c.st)
			if (err == nil) != c.ok {
				t.Fatalf("status %s: %v", name, err)
			}
			if c.ok {
				if got, err := ParseGuardianStatus(b); err != nil || *got.Cause != *c.st.Cause {
					t.Fatalf("status %s round trip %v", name, err)
				}
			}
		}
		// FIFO commands: 06a's cause-less stop and the cancel with its ID.
		base2 := GuardianCommand{TaskID: ctlTask, Execution: ctlExec(), Nonce: nonce, Command: GuardianStop}
		if b, err := EncodeGuardianCommand(base2); err != nil || strings.Contains(string(b), "cause") {
			t.Fatalf("recovery command %s %v", b, err)
		}
		cancel := base2
		cancel.Cause, cancel.StopID = CauseCancelled, id
		b, err := EncodeGuardianCommand(cancel)
		if err != nil || len(b) > MaxGuardianCommandBytes {
			t.Fatal(err)
		}
		if got, err := ParseGuardianCommand(b[:len(b)-1]); err != nil || got.StopID != id {
			t.Fatalf("cancel command %v", err)
		}
		for _, x := range []GuardianCommand{{TaskID: ctlTask, Execution: ctlExec(), Nonce: nonce, Command: GuardianStop, Cause: CauseTimedOut},
			{TaskID: ctlTask, Execution: ctlExec(), Nonce: nonce, Command: GuardianStop, Cause: CauseCancelled},
			{TaskID: ctlTask, Execution: ctlExec(), Nonce: nonce, Command: GuardianStop, StopID: id}} {
			if _, err := EncodeGuardianCommand(x); err == nil {
				t.Fatalf("command %+v accepted", x)
			}
			raw, _ := json.Marshal(x)
			if _, err := ParseGuardianCommand(raw); err == nil {
				t.Fatalf("command %s parsed", raw)
			}
		}
	})
	t.Run("migration", func(t *testing.T) {
		// A 06a schema-1 execution journal decodes (null intent, legacy
		// policy, its original digest) and re-encodes as schema 2 with the
		// same frozen result; a schema-2 journal carries an intent that a
		// cancelled result's stop_id must name; timed_out journals keep
		// their start. Schema-2 task records stay legacy and unenforced.
		st := ctlStartBody()
		res := TaskResultBody{TaskID: st.TaskID, Execution: st.Execution, Outcome: OutcomeNatural, ExitCode: iptr(0), OutputBytes: 3}.Sealed()
		n := strings.Repeat("c", 64)
		at := "2026-09-27T10:00:00Z"
		v1 := `{"schema_version":1,"task_id":"` + st.TaskID + `","execution":` + string(mustCompact(t, st.Execution)) + `,"start_digest":"` + ctlDigest +
			`","role":` + string(mustCompact(t, st.Role)) + `,"effective":` + string(mustCompact(t, st.Effective)) +
			`,"timeout_policy":"legacy_unenforced","owner_nonce":"` + n + `","phase":"completed","started_at":"` + at + `","result":` +
			strings.Replace(string(mustCompact(t, res)), `"stop_id":null,`, ``, 1) + `,"log":{"data":"YWJj","source_bytes":3,"received_bytes":3,"incomplete":false,"counter_overflow":false}}`
		j, err := ParseExecutionJournal([]byte(v1), testLookup)
		if err != nil || j.Schema != LegacyExecutionJournalSchemaVersion || j.StopIntent != nil || j.Result.Digest != res.Digest || j.TimeoutPolicy != TimeoutPolicyLegacy {
			t.Fatalf("schema 1 journal %+v %v", j, err)
		}
		b, err := EncodeExecutionJournal(j)
		if err != nil || !bytes.Contains(b, []byte(`"schema_version": 3`)) || !bytes.Contains(b, []byte(`"stop_intent": null`)) || !bytes.Contains(b, []byte(`"work": false`)) {
			t.Fatalf("schema 2 rewrite %s %v", b, err)
		}
		again, err := ParseExecutionJournal(b, testLookup)
		if err != nil || again.Result.Digest != res.Digest || string(again.Log.Data) != "abc" {
			t.Fatalf("replay %+v %v", again, err)
		}
		for name, raw := range map[string]string{
			"v1 enforced": strings.Replace(v1, "legacy_unenforced", "enforced", 1),
			"v1 intent":   strings.Replace(v1, `"phase":`, `"stop_intent":null,"phase":`, 1),
			"v1 control":  strings.Replace(v1, `"outcome":"natural"`, `"outcome":"cancelled"`, 1),
		} {
			if _, err := ParseExecutionJournal([]byte(raw), testLookup); err == nil {
				t.Fatalf("%s accepted", name)
			}
		}
		in := StopIntent{ID: strings.Repeat("a", 32), Kind: StopKindCancelled, RequestedAt: time.Date(2026, 9, 27, 10, 0, 0, 0, time.UTC)}
		started := time.Date(2026, 9, 27, 9, 0, 0, 0, time.UTC)
		cj := ExecutionJournal{TaskID: st.TaskID, Execution: st.Execution, StartDigest: ctlDigest, Role: st.Role, Effective: st.Effective,
			TimeoutPolicy: TimeoutPolicyEnforced, Phase: JournalCompleted, StopIntent: &in, StartedAt: &started}
		id := in.ID
		cr := TaskResultBody{TaskID: st.TaskID, Execution: st.Execution, Outcome: OutcomeCancelled, StopID: &id, Signal: s("SIGTERM")}.Sealed()
		cj.Result = &cr
		if b, _ := EncodeExecutionJournal(cj); func() error { _, err := ParseExecutionJournal(b, testLookup); return err }() != nil {
			t.Fatal("a cancelled journal was refused")
		}
		foreign := cj
		foreign.StopIntent = nil
		if b, _ := EncodeExecutionJournal(foreign); func() error { _, err := ParseExecutionJournal(b, testLookup); return err }() == nil {
			t.Fatal("a stop_id naming no intent was accepted")
		}
		tj := cj
		tj.StopIntent, tj.StartedAt = nil, nil
		tr := TaskResultBody{TaskID: st.TaskID, Execution: st.Execution, Outcome: OutcomeTimedOut}.Sealed()
		tj.Result = &tr
		if b, _ := EncodeExecutionJournal(tj); func() error { _, err := ParseExecutionJournal(b, testLookup); return err }() == nil {
			t.Fatal("a timed_out journal without its start was accepted")
		}
		// 06a schema-2 task records: legacy only, no control states.
		rec := ctlRecord(t)
		rec.TimeoutPolicy = TimeoutPolicyLegacy
		b3, _ := EncodeTaskRecord(rec)
		two := asSchema2(t, b3)
		got, err := ParseTaskRecord(two, testLookup)
		if err != nil || got.Schema != TaskRecordSchema2 || got.TimeoutPolicy != TimeoutPolicyLegacy || got.StopIntent != nil {
			t.Fatalf("schema 2 %+v %v", got, err)
		}
		if _, err := ParseTaskRecord(withField(t, two, "timeout_policy", `"enforced"`), testLookup); err == nil {
			t.Fatal("an enforced schema 2 record was accepted")
		}
	})
}

// HexDigestOf is r's protocol 4 digest (the field sequence without
// stop_id).
func HexDigestOf(t *testing.T, r TaskResultBody) string {
	t.Helper()
	b := mustCompact(t, resultDigestBody{r.TaskID, r.Execution, r.Outcome, r.ExitCode, r.Signal, r.FinalMessage, r.FinalMessageTruncated,
		r.OutputBytes, r.LogIncomplete, r.CounterOverflow})
	return HexDigest(sha256Sum(b))
}

func sha256Sum(b []byte) [32]byte { return sha256.Sum256(b) }

// ctlStartBody is a valid start for the journal cases.
func ctlStartBody() TaskStartBody {
	return TaskStartBody{TaskID: ctlTask, Execution: ctlExec(), RolesRevision: 1, Role: validRole(), Request: validRequest(),
		Effective: TaskEffective{Model: "example", Effort: "medium", Timeout: time.Hour}}
}

// TestControlWait is UT FP-3 on the contract, delegated from
// tests/function (TestControlBoundedWait): the wait request's strict
// validation and the wait union's byte bounds, worst cases included (all
// sixteen IDs, control and multi-byte lines, maximal integers). Do not
// rename or skip it.
func TestControlWait(t *testing.T) {
	ids := make([]string, MaxWaitIDs)
	for i := range ids {
		ids[i] = "t_" + strings.Repeat("0", 30) + string("0123456789abcdef"[i]) + "f"
	}
	t.Run("validation", func(t *testing.T) {
		req := TaskWaitRequest{TaskIDs: ids, Wait: 5 * time.Second}
		b, _ := Encode(req)
		got, err := ParseTaskWaitRequest(b)
		if err != nil || len(got.TaskIDs) != MaxWaitIDs || got.Wait != 5*time.Second || len(b) > MaxWaitRequestBytes {
			t.Fatalf("request %s %v", b, err)
		}
		for name, raw := range map[string]string{
			"empty":         `{"task_ids":[],"wait":"1s"}`,
			"17":            `{"task_ids":["` + strings.Join(append(append([]string{}, ids...), "t_"+strings.Repeat("e", 32)), `","`) + `"],"wait":"1s"}`,
			"duplicate":     `{"task_ids":["` + ids[0] + `","` + ids[0] + `"],"wait":"1s"}`,
			"bad id":        `{"task_ids":["t_X"],"wait":"1s"}`,
			"negative":      `{"task_ids":["` + ids[0] + `"],"wait":"-1s"}`,
			"over":          `{"task_ids":["` + ids[0] + `"],"wait":"5m0.001s"}`,
			"overflow":      `{"task_ids":["` + ids[0] + `"],"wait":"99999999999h"}`,
			"blank":         `{"task_ids":["` + ids[0] + `"],"wait":" 1s"}`,
			"number":        `{"task_ids":["` + ids[0] + `"],"wait":5}`,
			"missing":       `{"task_ids":["` + ids[0] + `"]}`,
			"extra":         `{"task_ids":["` + ids[0] + `"],"wait":"1s","x":1}`,
			"duplicate key": `{"task_ids":["` + ids[0] + `"],"wait":"1s","wait":"2s"}`,
			"big":           `{"task_ids":["` + ids[0] + `"],"wait":"1s"` + strings.Repeat(" ", MaxWaitRequestBytes) + `}`,
		} {
			if _, err := ParseTaskWaitRequest([]byte(raw)); CodeOf(err) != CodeInvalidArgument {
				t.Fatalf("%s: %v", name, err)
			}
		}
		for s, want := range map[string]time.Duration{"0": 0, "0s": 0, "5m": 5 * time.Minute, "100ms": 100 * time.Millisecond} {
			if d, err := ParseWaitDuration(s); err != nil || d != want {
				t.Fatalf("%s = %v %v", s, d, err)
			}
		}
		// Dispatch envelope: wait is a transport option, never stored.
		env := strings.Replace(string(mustCompact(t, validRequest())), `{`, `{"wait":"2s",`, 1)
		r, w, err := ParseDispatchEnvelope([]byte(env))
		if err != nil || w == nil || *w != 2*time.Second || bytes.Contains(mustCompact(t, r), []byte("wait")) {
			t.Fatalf("envelope %+v %v %v", r, w, err)
		}
		if _, w, err := ParseDispatchEnvelope(mustCompact(t, validRequest())); err != nil || w != nil {
			t.Fatalf("no wait %v %v", w, err)
		}
		for _, bad := range []string{`"wait":null,`, `"wait":"6m",`, `"wait":"x",`} {
			if _, _, err := ParseDispatchEnvelope([]byte(strings.Replace(string(mustCompact(t, validRequest())), `{`, `{`+bad, 1))); err == nil {
				t.Fatalf("envelope %s accepted", bad)
			}
		}
		// Cancel request and response.
		if ParseCancelRequest([]byte(`{}`)) != nil || ParseCancelRequest([]byte(`{"x":1}`)) == nil || ParseCancelRequest([]byte(``)) == nil {
			t.Fatal("cancel request")
		}
	})
	t.Run("bounds", func(t *testing.T) {
		// The worst still_running: sixteen rows, every line 96 bytes of
		// control bytes (six-byte escapes), maximal elapsed values; one row
		// alone within 2 KiB; text within the same bounds.
		worst := strings.Repeat("\x01", MaxLastLogLineBytes)
		var rows []WaitRow
		for _, id := range ids {
			rows = append(rows, WaitRow{TaskID: id, State: TaskRunning, ElapsedMS: MaxSafeInteger, LastLogLine: worst, LogTruncated: true})
		}
		r := WaitResponse{Version: ProtocolVersion, Status: WaitStillRunning, EffectiveWaitMS: 300000, Tasks: rows}
		b, err := EncodeWaitResponse(r)
		if err != nil || len(b)+1 > MaxStillRunningBytes {
			t.Fatalf("sixteen rows: %d bytes %v", len(b)+1, err)
		}
		if got, err := ParseWaitResponse(b, ids, MaxWait); err != nil || len(got.Tasks) != MaxWaitIDs || got.Tasks[3].LastLogLine != worst {
			t.Fatalf("parse %v", err)
		}
		one := WaitResponse{Version: ProtocolVersion, Status: WaitStillRunning, EffectiveWaitMS: 300000, Tasks: rows[:1]}
		b1, err := EncodeWaitResponse(one)
		if err != nil || len(b1)+1 > MaxStillRunningOneBytes {
			t.Fatalf("one row: %d %v", len(b1)+1, err)
		}
		// A dispatch envelope around the one row stays within 2 KiB.
		d := DispatchResponse{Version: ProtocolVersion, TaskID: ids[0], WaitResult: &one}
		db, _ := Encode(d)
		if len(db)+1 > MaxStillRunningOneBytes {
			t.Fatalf("dispatch envelope %d", len(db)+1)
		}
		if id, got, err := ParseDispatchWaitResponse(db, MaxWait); err != nil || id != ids[0] || got.Status != WaitStillRunning {
			t.Fatalf("dispatch wait %v", err)
		}
		text := RenderWaitRows(rows)
		if len(text) > MaxStillRunningBytes || strings.ContainsAny(text, "\x01") || strings.Count(text, "\n") != MaxWaitIDs+1 {
			t.Fatalf("text %d bytes", len(text))
		}
		// An over-bound line is an internal error, never a partial answer.
		big := r
		big.Tasks = append([]WaitRow(nil), rows...)
		big.Tasks[0].LastLogLine = strings.Repeat("\x01", 3000)
		if _, err := EncodeWaitResponse(big); CodeOf(err) != CodeInternal {
			t.Fatalf("over bound %v", err)
		}
		// LastLogLine: the final line or fragment, invalid UTF-8 replaced,
		// its last 96 bytes on rune boundaries.
		for in, want := range map[string]struct {
			line string
			cut  bool
		}{
			"a\nb\n":                 {"b", false},
			"a\nfrag":                {"frag", false},
			"":                       {"", false},
			"\xff\xfe\n":             {"\uFFFD", false},
			strings.Repeat("é", 60):  {strings.Repeat("é", 48), true},
			strings.Repeat("x", 200): {strings.Repeat("x", 96), true},
		} {
			got, cut := LastLogLine([]byte(in))
			if got != want.line || cut != want.cut || !utf8.ValidString(got) {
				t.Fatalf("%q = %q %v", in, got, cut)
			}
		}
		if EscapeTerminal("a\tb\n\x1b\\\u0085") != `a\x09b\x0a\x1b\\\u0085` {
			t.Fatalf("escape %q", EscapeTerminal("a\tb\n\x1b\\\u0085"))
		}
		// Response validation: rows in caller order, no durable terminal
		// row, effective wait within the request, winner durably terminal.
		for name, mut := range map[string]func(*WaitResponse){
			"order":     func(w *WaitResponse) { w.Tasks[0], w.Tasks[1] = w.Tasks[1], w.Tasks[0] },
			"missing":   func(w *WaitResponse) { w.Tasks = w.Tasks[1:] },
			"terminal":  func(w *WaitResponse) { w.Tasks[0].State, w.Tasks[0].DurabilityConfirmed = TaskSucceeded, true },
			"effective": func(w *WaitResponse) { w.EffectiveWaitMS = 5001 },
			"elapsed":   func(w *WaitResponse) { w.Tasks[0].ElapsedMS = -1 },
		} {
			x := WaitResponse{Version: ProtocolVersion, Status: WaitStillRunning, EffectiveWaitMS: 5000, Tasks: append([]WaitRow(nil), rows[:2]...)}
			mut(&x)
			b, _ := compact(x)
			if _, err := ParseWaitResponse(b, ids[:2], 5*time.Second); err == nil {
				t.Fatalf("%s accepted", name)
			}
		}
		v := smallView()
		v.TaskID = ids[1]
		term := WaitResponse{Version: ProtocolVersion, Status: WaitTerminal, EffectiveWaitMS: 5000, Winner: ids[1], Task: &v}
		tb, _ := EncodeWaitResponse(term)
		if got, err := ParseWaitResponse(tb, ids[:2], 5*time.Second); err != nil || got.Winner != ids[1] {
			t.Fatalf("terminal %v", err)
		}
		if _, err := ParseWaitResponse(tb, ids[2:4], 5*time.Second); err == nil {
			t.Fatal("a foreign winner was accepted")
		}
		nd := v
		nd.DurabilityConfirmed = false
		term.Task = &nd
		tb, _ = compact(term)
		if _, err := ParseWaitResponse(tb, ids[:2], 5*time.Second); err == nil {
			t.Fatal("an unconfirmed winner was accepted")
		}
	})
}
