package contract

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

const (
	testTaskID = "t_0123456789abcdef0123456789abcdef"
	testEpoch  = "fedcba9876543210fedcba9876543210"
)

// dispatchJSON is a valid dispatch request with sub replacing whole
// member values (a nil value removes the member).
func dispatchJSON(sub map[string]*string) string {
	fields := []struct{ k, v string }{
		{"target", `{"kind":"id","value":"worker-a"}`},
		{"goal", `"implement the thing"`},
		{"payload", `["repo://a","ticket 7"]`},
		{"acceptance", `"tests pass"`},
		{"requested_by", `{"name":"callsheet","version":"dev","hostname":"coord-1.example"}`},
	}
	var parts []string
	seen := map[string]bool{}
	for _, f := range fields {
		seen[f.k] = true
		if v, ok := sub[f.k]; ok {
			if v == nil {
				continue
			}
			f.v = *v
		}
		parts = append(parts, strconv.Quote(f.k)+":"+f.v)
	}
	for k, v := range sub {
		if !seen[k] && v != nil {
			parts = append(parts, strconv.Quote(k)+":"+*v)
		}
	}
	return "{" + strings.Join(parts, ",") + "}"
}

func s(v string) *string { return &v }

func wantTaskErr(t *testing.T, name string, err error, field, reason, substr string) {
	t.Helper()
	var ce *Error
	if !errors.As(err, &ce) || ce.Code != CodeInvalidArgument {
		t.Fatalf("%s: err = %v, want invalid_argument", name, err)
	}
	if f, _ := ce.Details["field"].(string); field != "" && f != field {
		t.Fatalf("%s: field %q (%v), want %q", name, f, err, field)
	}
	if r, _ := ce.Details["reason"].(string); r != reason {
		t.Fatalf("%s: reason %q (%v), want %q", name, r, err, reason)
	}
	if !strings.Contains(ce.Message, substr) {
		t.Fatalf("%s: %v, want %q", name, err, substr)
	}
}

func validRole() RoleRecord {
	return RoleRecord{RoleConfig: RoleConfig{ID: "worker-a", Name: "implementer", Node: testID, Adapter: "fake", Instruction: "/srv/i.md",
		Runbook: "/srv/r.md", Model: "example", Effort: "medium", Concurrency: 2, Timeout: 2 * time.Hour, HasTimeout: true}, RegistrationOrder: 3}
}

func validRequest() DispatchRequest {
	r, err := ParseDispatchRequest([]byte(dispatchJSON(nil)))
	if err != nil {
		panic(err)
	}
	return r
}

// TestTaskContract is UT FP-1/3/7 for the task contract; its envelope
// subtest is delegated from tests/function (TestTaskModel/envelope). Do
// not rename or skip its subtests.
func TestTaskContract(t *testing.T) {
	t.Run("envelope", func(t *testing.T) {
		r, err := ParseDispatchRequest([]byte(dispatchJSON(nil)))
		if err != nil {
			t.Fatal(err)
		}
		if r.Target != (TaskTarget{Kind: "id", Value: "worker-a"}) || r.Goal != "implement the thing" || len(r.Payload) != 2 || r.Override != nil ||
			r.RequestedBy.Hostname != "coord-1.example" {
			t.Fatalf("parsed %+v", r)
		}
		// The canonical encoding round-trips exactly, override omitted.
		b, _ := Encode(r)
		if string(b) != dispatchJSON(nil) {
			t.Fatalf("encoded %s", b)
		}
		// Empty payload is legal; goal whitespace and newlines are verbatim.
		r, err = ParseDispatchRequest([]byte(dispatchJSON(map[string]*string{"payload": s(`[]`), "goal": s(`"  line one\n\tline two  "`)})))
		if err != nil || len(r.Payload) != 0 || r.Goal != "  line one\n\tline two  " {
			t.Fatalf("empty payload %+v %v", r, err)
		}
		if b, _ := Encode(r); !strings.Contains(string(b), `"payload":[]`) {
			t.Fatalf("empty payload encoded %s", b)
		}
		// Overrides: any nonempty subset; timeout canonical.
		r, err = ParseDispatchRequest([]byte(dispatchJSON(map[string]*string{"override": s(`{"timeout":"90m","effort":"high"}`)})))
		if err != nil || r.Override == nil || *r.Override.Effort != "high" || *r.Override.Timeout != 90*time.Minute || r.Override.Model != nil {
			t.Fatalf("override %+v %v", r.Override, err)
		}
		if b, _ := Encode(r); !strings.HasSuffix(string(b), `"override":{"effort":"high","timeout":"1h30m0s"}}`) {
			t.Fatalf("override encoded %s", b)
		}
		big := strings.Repeat("g", MaxGoalBytes)
		pointer := strings.Repeat("p", MaxPointerBytes)
		ptrs := make([]string, 32)
		for i := range ptrs {
			ptrs[i] = strconv.Quote(pointer)
		}
		cases := []struct {
			name          string
			body          string
			field, reason string
			msg           string
		}{
			{"not object", `[]`, "", "", "not a JSON object"},
			{"trailing", dispatchJSON(nil) + `{}`, "", "", "trailing data"},
			{"duplicate", strings.Replace(dispatchJSON(nil), `"goal":`, `"goal":"x","goal":`, 1), "", "", "duplicate field"},
			{"unknown", dispatchJSON(map[string]*string{"extra": s(`1`)}), "", "", "unknown field"},
			{"command", dispatchJSON(map[string]*string{"argv": s(`["x"]`)}), "", "", "unknown field"},
			{"manual body", dispatchJSON(map[string]*string{"instruction": s(`"body"`)}), "", "", "unknown field"},
			{"missing goal", dispatchJSON(map[string]*string{"goal": nil}), "goal", "", "lacks the required field"},
			{"missing requester", dispatchJSON(map[string]*string{"requested_by": nil}), "requested_by", "", "lacks the required field"},
			{"null goal", dispatchJSON(map[string]*string{"goal": s(`null`)}), "goal", "", "must not be null"},
			{"null payload", dispatchJSON(map[string]*string{"payload": s(`null`)}), "payload", "", "must not be null"},
			{"null override", dispatchJSON(map[string]*string{"override": s(`null`)}), "override", "", "must not be null"},
			{"workspace", dispatchJSON(map[string]*string{"workspace": s(`"ws"`)}), "workspace", ReasonWorkspaceNotSupported, "iteration 10"},
			{"workspace null", dispatchJSON(map[string]*string{"workspace": s(`null`)}), "workspace", ReasonWorkspaceNotSupported, "iteration 10"},
			{"base empty", dispatchJSON(map[string]*string{"base": s(`""`)}), "base", ReasonWorkspaceNotSupported, "iteration 10"},
			{"wait", dispatchJSON(map[string]*string{"wait": s(`"5s"`)}), "", "", "unknown field"},
			{"bare target", dispatchJSON(map[string]*string{"target": s(`"worker-a"`)}), "target", "", "not a JSON object"},
			{"target kind", dispatchJSON(map[string]*string{"target": s(`{"kind":"role","value":"a"}`)}), "target", "", "target kind"},
			{"target slug", dispatchJSON(map[string]*string{"target": s(`{"kind":"name","value":"A"}`)}), "target", "", "target value"},
			{"target extra", dispatchJSON(map[string]*string{"target": s(`{"kind":"id","value":"a","x":1}`)}), "target", "", "unknown field"},
			{"blank goal", dispatchJSON(map[string]*string{"goal": s(`" \n\t "`)}), "goal", "", "whitespace only"},
			{"empty acceptance", dispatchJSON(map[string]*string{"acceptance": s(`""`)}), "acceptance", "", "whitespace only"},
			{"goal nul", dispatchJSON(map[string]*string{"goal": s(`"a\u0000b"`)}), "goal", "", "NUL"},
			{"goal limit+1", dispatchJSON(map[string]*string{"goal": s(strconv.Quote(big + "g"))}), "goal", "", "at most 16384"},
			{"acceptance limit+1", dispatchJSON(map[string]*string{"acceptance": s(strconv.Quote(big + "a"))}), "acceptance", "", "at most 16384"},
			{"goal invalid utf8", dispatchJSON(map[string]*string{"goal": s("\"a\xffb\"")}), "goal", "", "not valid UTF-8"},
			{"goal lone surrogate", dispatchJSON(map[string]*string{"goal": s(`"a\ud800b"`)}), "goal", "", "not valid UTF-8"},
			{"goal low surrogate", dispatchJSON(map[string]*string{"goal": s(`"\udc00"`)}), "goal", "", "not valid UTF-8"},
			{"payload string", dispatchJSON(map[string]*string{"payload": s(`"a"`)}), "payload", "", "array"},
			{"payload empty pointer", dispatchJSON(map[string]*string{"payload": s(`[""]`)}), "payload", "", "must not be empty"},
			{"payload number", dispatchJSON(map[string]*string{"payload": s(`[1]`)}), "payload", "", "must be a string"},
			{"pointer limit+1", dispatchJSON(map[string]*string{"payload": s(`["` + pointer + `p"]`)}), "payload", "", "at most 2048"},
			{"pointers 65", dispatchJSON(map[string]*string{"payload": s(`[` + strings.Repeat(`"x",`, 64) + `"x"]`)}), "payload", "", "at most 64"},
			{"payload total+1", dispatchJSON(map[string]*string{"payload": s(`[` + strings.Join(ptrs, ",") + `,"x"]`)}), "payload", "", "at most 65536"},
			{"requester host space", dispatchJSON(map[string]*string{"requested_by": s(`{"name":"c","version":"1","hostname":"a b"}`)}), "requested_by", "", "hostname"},
			{"requester host dash", dispatchJSON(map[string]*string{"requested_by": s(`{"name":"c","version":"1","hostname":"-a"}`)}), "requested_by", "", "hostname"},
			{"requester host unicode", dispatchJSON(map[string]*string{"requested_by": s(`{"name":"c","version":"1","hostname":"hé"}`)}), "requested_by", "", "hostname"},
			{"requester host 129", dispatchJSON(map[string]*string{"requested_by": s(`{"name":"c","version":"1","hostname":"` + strings.Repeat("h", 129) + `"}`)}), "requested_by", "", "hostname"},
			{"requester name", dispatchJSON(map[string]*string{"requested_by": s(`{"name":"a b","version":"1","hostname":"h"}`)}), "requested_by", "", "name"},
			{"requester version", dispatchJSON(map[string]*string{"requested_by": s(`{"name":"a","version":"","hostname":"h"}`)}), "requested_by", "", "version"},
			{"requester extra", dispatchJSON(map[string]*string{"requested_by": s(`{"name":"a","version":"1","hostname":"h","ip":"x"}`)}), "requested_by", "", "unknown field"},
			{"override empty", dispatchJSON(map[string]*string{"override": s(`{}`)}), "override", "", "at least one"},
			{"override unknown", dispatchJSON(map[string]*string{"override": s(`{"concurrency":"2"}`)}), "", "", "unknown field"},
			{"override null model", dispatchJSON(map[string]*string{"override": s(`{"model":null}`)}), "model", "", "must not be null"},
			{"override blank model", dispatchJSON(map[string]*string{"override": s(`{"model":"  "}`)}), "model", "", "nonblank"},
			{"override empty effort", dispatchJSON(map[string]*string{"override": s(`{"effort":""}`)}), "effort", "", "must not be empty"},
			{"override negative", dispatchJSON(map[string]*string{"override": s(`{"timeout":"-1s"}`)}), "timeout", "", "negative"},
			{"override overflow", dispatchJSON(map[string]*string{"override": s(`{"timeout":"9999999999h"}`)}), "timeout", "", "Go duration"},
			{"override number", dispatchJSON(map[string]*string{"override": s(`{"timeout":5}`)}), "timeout", "", "must be a string"},
		}
		for _, c := range cases {
			_, err := ParseDispatchRequest([]byte(c.body))
			wantTaskErr(t, c.name, err, c.field, c.reason, c.msg)
		}
		// Byte limits pass at the limit.
		ok := dispatchJSON(map[string]*string{"goal": s(strconv.Quote(big)), "acceptance": s(strconv.Quote(big)), "payload": s(`[` + strings.Join(ptrs, ",") + `]`)})
		if r, err := ParseDispatchRequest([]byte(ok)); err != nil || r.Validate() != nil {
			t.Fatalf("limits: %v %v", err, r.Validate())
		}
		// The encoded request limit applies including escaping and
		// whitespace, independently of the field limits.
		pad := MaxDispatchRequestBytes - len(dispatchJSON(nil)) + 1
		padded := dispatchJSON(nil)[:1] + strings.Repeat(" ", pad) + dispatchJSON(nil)[1:]
		if _, err := ParseDispatchRequest([]byte(padded)); err == nil || !strings.Contains(err.Error(), "larger than 262144") {
			t.Fatalf("encoded limit+1: %v", err)
		}
		if _, err := ParseDispatchRequest([]byte(padded[:len(padded)-2] + "}")); err == nil {
			t.Fatal("malformed padded request accepted")
		}
		atLimit := dispatchJSON(nil)[:1] + strings.Repeat(" ", pad-1) + dispatchJSON(nil)[1:]
		if _, err := ParseDispatchRequest([]byte(atLimit)); err != nil {
			t.Fatalf("encoded limit: %v", err)
		}
		// Validate agrees with the parser for built requests.
		bad := validRequest()
		bad.Goal = ""
		if bad.Validate() == nil {
			t.Fatal("Validate accepted an empty goal")
		}
		bad = validRequest()
		bad.Override = &TaskOverride{}
		if bad.Validate() == nil {
			t.Fatal("Validate accepted an empty override")
		}
		bad = validRequest()
		bad.RequestedBy.Hostname = "a b"
		if bad.Validate() == nil {
			t.Fatal("Validate accepted a bad hostname")
		}
	})
	t.Run("effective", func(t *testing.T) {
		role := validRole()
		e, err := ResolveEffective(role, nil, testLookup)
		if err != nil || e != (TaskEffective{Model: "example", Effort: "medium", Timeout: 2 * time.Hour}) {
			t.Fatalf("inherit %+v %v", e, err)
		}
		m, eff, zero := "other model", "high", time.Duration(0)
		e, err = ResolveEffective(role, &TaskOverride{Model: &m, Effort: &eff, Timeout: &zero}, testLookup)
		if err != nil || e.Model != m || e.Effort != eff || e.Timeout != 0 {
			t.Fatalf("override %+v %v", e, err)
		}
		if b, _ := Encode(e); string(b) != `{"model":"other model","effort":"high","timeout":"0s"}` {
			t.Fatalf("effective %s", b)
		}
		bad := "extreme"
		_, err = ResolveEffective(role, &TaskOverride{Effort: &bad}, testLookup)
		wantField(t, "effort", err, "effort", "not allowed for adapter fake")
		// An omitted role timeout resolves to the 2h default.
		r2 := role
		r2.HasTimeout, r2.Timeout = false, 0
		if e, _ := ResolveEffective(r2, nil, testLookup); e.Timeout != DefaultRoleTimeout {
			t.Fatalf("default timeout %v", e.Timeout)
		}
	})
	t.Run("frames", func(t *testing.T) {
		start := TaskStartBody{TaskID: testTaskID, Execution: ExecutionToken{Epoch: testEpoch, Attachment: 4}, RolesRevision: 7,
			Role: validRole(), Request: validRequest(), Effective: TaskEffective{Model: "example", Effort: "medium", Timeout: time.Hour}}
		m, err := EncodeFrame(ProtocolVersion, FrameTaskStart, "p3", start)
		if err != nil {
			t.Fatal(err)
		}
		f, err := DecodeFrame(m, FromPlane)
		if err != nil || f.Type != FrameTaskStart || f.Version != ProtocolVersion {
			t.Fatalf("frame %+v %v", f, err)
		}
		got, err := DecodeTaskStart(f.Body, testLookup)
		if err != nil || got.Digest() != start.Digest() || got.Role.RegistrationOrder != 3 || got.Execution.Attachment != 4 {
			t.Fatalf("start %+v %v", got, err)
		}
		// Whitespace does not change the canonical digest; data does.
		spaced := bytes.ReplaceAll(f.Body, []byte(`,"`), []byte(`, "`))
		if again, err := DecodeTaskStart(spaced, testLookup); err != nil || again.Digest() != start.Digest() {
			t.Fatalf("spaced digest %v", err)
		}
		changed := start
		changed.Request.Goal = "other"
		if changed.Digest() == start.Digest() {
			t.Fatal("digest ignores the request")
		}
		// Direction: a sidecar never sends task_start; a plane never
		// sends task_log.
		if _, err := DecodeFrame(m, FromSidecar); err == nil || !strings.Contains(err.Error(), "not valid in this direction") {
			t.Fatalf("direction: %v", err)
		}
		for name, c := range map[string]struct{ body, msg string }{
			"bad id":     {strings.Replace(string(f.Body), testTaskID, "t_x", 1), "task_id"},
			"bad epoch":  {strings.Replace(string(f.Body), testEpoch, "EPOCH", 1), "epoch"},
			"attachment": {strings.Replace(string(f.Body), `"attachment":4`, `"attachment":0`, 1), "attachment"},
			"revision":   {strings.Replace(string(f.Body), `"roles_revision":7`, `"roles_revision":-1`, 1), "roles_revision"},
			"effort":     {strings.Replace(string(f.Body), `"effort":"medium","timeout":"1h0m0s"`, `"effort":"extreme","timeout":"1h0m0s"`, 1), "effort"},
			"timeout":    {strings.Replace(string(f.Body), `"timeout":"1h0m0s"}`, `"timeout":"60m"}`, 1), "timeout"},
			"extra":      {strings.Replace(string(f.Body), `"task_id"`, `"x":1,"task_id"`, 1), "unknown field"},
			"workspace":  {strings.Replace(string(f.Body), `"goal"`, `"workspace":"w","goal"`, 1), "workspace"},
			"role":       {strings.Replace(string(f.Body), `"adapter":"fake"`, `"adapter":"nope"`, 1), "adapter"},
		} {
			if _, err := DecodeTaskStart([]byte(c.body), testLookup); err == nil || !strings.Contains(err.Error(), c.msg) {
				t.Fatalf("%s: %v", name, err)
			}
		}
		// task_start_result: exact shapes; refusal codes and reasons are closed.
		for _, r := range []TaskStartResult{{TaskID: testTaskID}, {TaskID: testTaskID, Err: TaskError(CodeUnavailable, "", ReasonLocalFull, "full")}} {
			m, err := EncodeFrame(ProtocolVersion, FrameTaskStartResult, "p3", r)
			if err != nil {
				t.Fatal(err)
			}
			f, err := DecodeFrame(m, FromSidecar)
			if err != nil {
				t.Fatal(err)
			}
			got, err := DecodeTaskStartResult(f.Body)
			if err != nil || got.TaskID != testTaskID || (got.Err == nil) != (r.Err == nil) {
				t.Fatalf("result %s: %+v %v", f.Body, got, err)
			}
		}
		for name, body := range map[string]string{
			"ok extra":      `{"task_id":"` + testTaskID + `","ok":true,"error":{"code":"internal","message":"m"}}`,
			"no ok":         `{"task_id":"` + testTaskID + `"}`,
			"bad reason":    `{"task_id":"` + testTaskID + `","ok":false,"error":{"code":"unavailable","message":"m","details":{"reason":"nope"}}}`,
			"bad code":      `{"task_id":"` + testTaskID + `","ok":false,"error":{"code":"conflict","message":"m","details":{"reason":"local_full"}}}`,
			"no reason":     `{"task_id":"` + testTaskID + `","ok":false,"error":{"code":"unavailable","message":"m"}}`,
			"bad id":        `{"task_id":"x","ok":true}`,
			"missing error": `{"task_id":"` + testTaskID + `","ok":false}`,
		} {
			if _, err := DecodeTaskStartResult([]byte(body)); err == nil {
				t.Fatalf("%s accepted", name)
			}
		}
		// task_log: canonical base64 of 1..16384 bytes; exact offsets.
		chunk := bytes.Repeat([]byte{0, 0xff, '\n'}, MaxLogChunkBytes/3)
		lb := TaskLogBody{TaskID: testTaskID, Execution: ExecutionToken{Epoch: testEpoch, Attachment: 1}, Offset: 10, Data: chunk}
		m, err = EncodeFrame(ProtocolVersion, FrameTaskLog, "b2", lb)
		if err != nil {
			t.Fatal(err)
		}
		f, _ = DecodeFrame(m, FromSidecar)
		if got, err := DecodeTaskLog(f.Body); err != nil || !bytes.Equal(got.Data, chunk) || got.Offset != 10 {
			t.Fatalf("log %v", err)
		}
		lb.Data = make([]byte, MaxLogChunkBytes)
		if _, err := EncodeFrame(ProtocolVersion, FrameTaskLog, "b2", lb); err != nil {
			t.Fatalf("a full chunk must fit the body limit: %v", err)
		}
		logBody := func(data, offset string) string {
			return `{"task_id":"` + testTaskID + `","execution":{"epoch":"` + testEpoch + `","attachment":1},"offset":` + offset + `,"data":` + data + `,"late_digest":null}`
		}
		for name, body := range map[string]string{
			"empty":        logBody(`""`, "0"),
			"too big":      logBody(`"`+EncodeBase64(make([]byte, MaxLogChunkBytes+1))+`"`, "0"),
			"unpadded":     logBody(`"YQ"`, "0"),
			"noncanonical": logBody(`"YR=="`, "0"),
			"url":          logBody(`"-_8="`, "0"),
			"negative":     logBody(`"YQ=="`, "-1"),
			"overflow":     logBody(`"YQ=="`, strconv.Itoa(MaxSafeInteger)),
			"float":        logBody(`"YQ=="`, "1.0"),
		} {
			if _, err := DecodeTaskLog([]byte(body)); err == nil {
				t.Fatalf("%s accepted", name)
			}
		}
		if _, err := DecodeTaskLog([]byte(logBody(`"YQ=="`, strconv.Itoa(MaxSafeInteger-1)))); err != nil {
			t.Fatalf("offset at the bound: %v", err)
		}
		if a, err := DecodeTaskLogAck([]byte(`{"task_id":"` + testTaskID + `","next_offset":5}`)); err != nil || a.NextOffset != 5 {
			t.Fatalf("log ack %+v %v", a, err)
		}
		if _, err := DecodeTaskLogAck([]byte(`{"task_id":"` + testTaskID + `","next_offset":-5}`)); err == nil {
			t.Fatal("negative ack accepted")
		}
		// task_result: exactly one of exit code and signal; closed signals.
		zero, seven := 0, 7
		msg := "done"
		for _, b := range []TaskResultBody{
			{TaskID: testTaskID, Execution: lb.Execution, Outcome: OutcomeNatural, ExitCode: &zero, FinalMessage: &msg, OutputBytes: 3},
			{TaskID: testTaskID, Execution: lb.Execution, Outcome: OutcomeNatural, ExitCode: &seven},
			{TaskID: testTaskID, Execution: lb.Execution, Outcome: OutcomeNatural, Signal: s("SIGKILL"), LogIncomplete: true},
			{TaskID: testTaskID, Execution: lb.Execution, Outcome: OutcomeLost, OutputBytes: 9},
		} {
			m, err := EncodeFrame(ProtocolVersion, FrameTaskResult, "b3", b.Sealed())
			if err != nil {
				t.Fatal(err)
			}
			f, _ := DecodeFrame(m, FromSidecar)
			if got, err := DecodeTaskResult(f.Body); err != nil || got.TaskID != testTaskID {
				t.Fatalf("result %s: %v", f.Body, err)
			}
		}
		neg, big := -1, 256
		long := strings.Repeat("m", MaxFinalMessageBytes+1)
		for name, b := range map[string]TaskResultBody{
			"both":       {TaskID: testTaskID, Execution: lb.Execution, ExitCode: &zero, Signal: s("SIGTERM")},
			"neither":    {TaskID: testTaskID, Execution: lb.Execution},
			"negative":   {TaskID: testTaskID, Execution: lb.Execution, ExitCode: &neg},
			"256":        {TaskID: testTaskID, Execution: lb.Execution, ExitCode: &big},
			"bare TERM":  {TaskID: testTaskID, Execution: lb.Execution, Signal: s("TERM")},
			"alias":      {TaskID: testTaskID, Execution: lb.Execution, Signal: s("SIGIOT")},
			"arbitrary":  {TaskID: testTaskID, Execution: lb.Execution, Signal: s("signal 9")},
			"null trunc": {TaskID: testTaskID, Execution: lb.Execution, ExitCode: &zero, FinalMessageTruncated: true},
			"long":       {TaskID: testTaskID, Execution: lb.Execution, ExitCode: &zero, FinalMessage: &long},
			"invalid":    {TaskID: testTaskID, Execution: lb.Execution, ExitCode: &zero, FinalMessage: s("a\xffb")},
			"output":     {TaskID: testTaskID, Execution: lb.Execution, ExitCode: &zero, OutputBytes: -1},
		} {
			b.Outcome = OutcomeNatural
			if b.Sealed().Validate() == nil {
				t.Fatalf("%s accepted", name)
			}
		}
		if _, err := DecodeTaskResult([]byte(`{"task_id":"` + testTaskID + `","execution":{"epoch":"` + testEpoch + `","attachment":1},"outcome":"natural","exit_code":0,"signal":null,"final_message":null,"final_message_truncated":false,"output_bytes":0,"log_incomplete":false,"digest":"` + strings.Repeat("0", 64) + `"}`)); err == nil {
			t.Fatal("a result without counter_overflow was accepted")
		}
		dg := strings.Repeat("a", 64)
		if a, err := DecodeTaskResultAck([]byte(`{"task_id":"` + testTaskID + `","digest":"` + dg + `","received":true,"committed":false}`)); err != nil || !a.Received || a.Committed {
			t.Fatalf("result ack %v", err)
		}
		if _, err := DecodeTaskResultAck([]byte(`{"task_id":"` + testTaskID + `","digest":"` + dg + `","received":false,"committed":true}`)); err == nil {
			t.Fatal("received:false accepted")
		}
		// Every wire signal is SIG-prefixed and unique; bare names fail.
		seen := map[string]bool{}
		for _, w := range WireSignals {
			if !strings.HasPrefix(w, "SIG") || seen[w] || !ValidWireSignal(w) {
				t.Fatalf("wire signal %q", w)
			}
			seen[w] = true
		}
		for _, bad := range []string{"TERM", "SIGIOT", "SIGCLD", "SIGPOLL", "sigterm", "", "SIGRTMIN+1"} {
			if ValidWireSignal(bad) {
				t.Fatalf("%q accepted", bad)
			}
		}
	})
	t.Run("protocol", func(t *testing.T) {
		if ProtocolVersion != 5 {
			t.Fatalf("protocol %d", ProtocolVersion)
		}
		// A protocol-3 peer is refused with both versions named, before its
		// body is looked at.
		_, err := DecodeFrame(frame(3, FrameTaskLog, "b1", `{"garbage":true}`), FromSidecar)
		var ce *Error
		if !errors.As(err, &ce) || ce.Code != CodeProtocolMismatch || ce.Message != "protocol version mismatch: local=5 remote=3" {
			t.Fatalf("protocol 3 frame: %v", err)
		}
		// Heartbeat inflight is 0..MaxConcurrency, may exceed a lowered
		// concurrency, and forces can_accept false when full.
		for body, ok := range map[string]bool{
			`{"role_id":"r","inflight":3,"concurrency":2,"can_accept":false}`:          true,
			`{"role_id":"r","inflight":1,"concurrency":2,"can_accept":true}`:           true,
			`{"role_id":"r","inflight":2,"concurrency":2,"can_accept":true}`:           false,
			`{"role_id":"r","inflight":-1,"concurrency":2,"can_accept":false}`:         false,
			`{"role_id":"r","inflight":2147483648,"concurrency":2,"can_accept":false}`: false,
		} {
			if _, err := ParseRoleStatus([]byte(body)); (err == nil) != ok {
				t.Fatalf("%s: %v", body, err)
			}
		}
	})
	t.Run("states", func(t *testing.T) {
		now := time.Date(2026, 9, 27, 10, 0, 0, 0, time.UTC)
		later := now.Add(time.Second)
		zero, seven := 0, 7
		base := TaskRecord{TaskID: testTaskID, Request: validRequest(), Role: validRole(), RolesRevision: 4,
			Effective: TaskEffective{Model: "example", Effort: "medium", Timeout: time.Hour}, Execution: ExecutionToken{Epoch: testEpoch, Attachment: 2},
			State: TaskPending, CreatedAt: now, Revision: 1, Log: TaskLog{}}
		good := map[string]func(*TaskRecord){
			"pending": func(r *TaskRecord) {},
			"running": func(r *TaskRecord) { r.State, r.StartedAt = TaskRunning, &later },
			"succeeded": func(r *TaskRecord) {
				r.State, r.StartedAt, r.FinishedAt, r.ExitCode = TaskSucceeded, &later, &later, &zero
			},
			"failed": func(r *TaskRecord) {
				r.State, r.StartedAt, r.FinishedAt, r.ExitCode = TaskFailed, &later, &later, &seven
			},
			"signaled": func(r *TaskRecord) {
				r.State, r.StartedAt, r.FinishedAt, r.Signal = TaskFailed, &later, &later, s("SIGKILL")
			},
			"rejected": func(r *TaskRecord) {
				r.State, r.FinishedAt, r.Reason = TaskRejected, &later, &TaskReason{Code: ReasonLocalFull, Message: "the worker is full"}
				r.Candidates = []TaskCandidate{{RoleID: "worker-a", NodeID: testID, RegistrationOrder: 3, NodeLiveness: LivenessOnline, Inflight: 0, Concurrency: 2, CanAccept: true, Reason: ReasonAvailable}}
			},
			"final": func(r *TaskRecord) {
				r.State, r.StartedAt, r.FinishedAt, r.ExitCode, r.FinalMessage = TaskSucceeded, &later, &later, &zero, s("fake task completed")
				r.Log = TaskLog{Data: []byte("tail\x00\xff"), SourceBytes: 20, ReceivedBytes: 20}
			},
		}
		for name, mut := range good {
			r := base
			mut(&r)
			b, err := EncodeTaskRecord(r)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.HasSuffix(b, []byte("}\n")) || !bytes.Contains(b, []byte("\n  \"task_id\"")) {
				t.Fatalf("%s: not the indented writer form:\n%s", name, b)
			}
			got, err := ParseTaskRecord(b, testLookup)
			if err != nil {
				t.Fatalf("%s: %v\n%s", name, err, b)
			}
			again, _ := EncodeTaskRecord(got)
			if !bytes.Equal(again, b) {
				t.Fatalf("%s: round trip differs", name)
			}
		}
		bad := map[string]func(*TaskRecord){
			"pending started":   func(r *TaskRecord) { r.StartedAt = &later },
			"pending finished":  func(r *TaskRecord) { r.FinishedAt = &later },
			"pending exit":      func(r *TaskRecord) { r.ExitCode = &zero },
			"pending final":     func(r *TaskRecord) { r.FinalMessage = s("x") },
			"running unstarted": func(r *TaskRecord) { r.State = TaskRunning },
			"success nonzero": func(r *TaskRecord) {
				r.State, r.StartedAt, r.FinishedAt, r.ExitCode = TaskSucceeded, &later, &later, &seven
			},
			"success signal": func(r *TaskRecord) {
				r.State, r.StartedAt, r.FinishedAt, r.Signal = TaskSucceeded, &later, &later, s("SIGTERM")
			},
			"failed zero": func(r *TaskRecord) {
				r.State, r.StartedAt, r.FinishedAt, r.ExitCode = TaskFailed, &later, &later, &zero
			},
			"failed nothing": func(r *TaskRecord) { r.State, r.StartedAt, r.FinishedAt = TaskFailed, &later, &later },
			"rejected started": func(r *TaskRecord) {
				r.State, r.StartedAt, r.FinishedAt, r.Reason = TaskRejected, &later, &later, &TaskReason{Code: ReasonLocalFull, Message: "m"}
			},
			"rejected no reason": func(r *TaskRecord) { r.State, r.FinishedAt = TaskRejected, &later },
			"reason code": func(r *TaskRecord) {
				r.State, r.FinishedAt, r.Reason = TaskRejected, &later, &TaskReason{Code: "nope", Message: "m"}
			},
			"reason long": func(r *TaskRecord) {
				r.State, r.FinishedAt, r.Reason = TaskRejected, &later, &TaskReason{Code: ReasonStartNotSent, Message: strings.Repeat("m", 257)}
			},
			"reserved state":  func(r *TaskRecord) { r.State = "cancelled" },
			"lost":            func(r *TaskRecord) { r.State = "lost" },
			"candidates":      func(r *TaskRecord) { r.Candidates = []TaskCandidate{{}} },
			"log counters":    func(r *TaskRecord) { r.Log = TaskLog{Data: []byte("abc"), SourceBytes: 3, ReceivedBytes: 2} },
			"log gap":         func(r *TaskRecord) { r.Log = TaskLog{Data: []byte("abc"), SourceBytes: 9, ReceivedBytes: 3} },
			"log overflow":    func(r *TaskRecord) { r.Log = TaskLog{CounterOverflow: true} },
			"revision":        func(r *TaskRecord) { r.Revision = 0 },
			"effective":       func(r *TaskRecord) { r.Effective.Effort = "extreme" },
			"execution epoch": func(r *TaskRecord) { r.Execution.Epoch = "x" },
		}
		for name, mut := range bad {
			r := base
			mut(&r)
			b, err := EncodeTaskRecord(r)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := ParseTaskRecord(b, testLookup); err == nil {
				t.Fatalf("%s accepted:\n%s", name, b)
			}
		}
		// Gaps are legal only when marked incomplete; leading eviction is.
		r := base
		r.Log = TaskLog{Data: []byte("abc"), SourceBytes: 9, ReceivedBytes: 3, Incomplete: true}
		if b, _ := EncodeTaskRecord(r); func() error { _, err := ParseTaskRecord(b, testLookup); return err }() != nil {
			t.Fatal("an incomplete gap was refused")
		}
		r.Log = TaskLog{Data: []byte("abc"), SourceBytes: 9, ReceivedBytes: 9}
		if b, _ := EncodeTaskRecord(r); func() error { _, err := ParseTaskRecord(b, testLookup); return err }() != nil {
			t.Fatal("leading eviction was refused")
		}
		good2, _ := EncodeTaskRecord(base)
		for name, raw := range map[string]string{
			"schema":        strings.Replace(string(good2), `"schema_version": 3`, `"schema_version": 4`, 1),
			"policy":        strings.Replace(string(good2), `"timeout_policy": "legacy_unenforced"`, `"timeout_policy": "odd"`, 1),
			"enforced flag": strings.Replace(string(good2), `"timeout_policy": "legacy_unenforced"`, `"timeout_policy": "legacy_unenforced", "timeout_enforced": false`, 1),
			"stop intent":   strings.Replace(string(good2), `"stop_intent": null`, `"stop_intent": {"id":"x","kind":"cancelled","requested_at":"2026-09-27T10:00:00Z"}`, 1),
			"extra":         strings.Replace(string(good2), `"revision": 1`, `"revision": 1, "x": 1`, 1),
			"missing":       strings.Replace(string(good2), `"final_message": null,`, ``, 1),
			"created time":  strings.Replace(string(good2), `"created_at": "2026-09-27T10:00:00Z"`, `"created_at": "2026-09-27 10:00:00"`, 1),
			"base64":        strings.Replace(string(good2), `"data": ""`, `"data": "YQ"`, 1),
			"duplicate":     strings.Replace(string(good2), `"revision": 1`, `"revision": 1, "revision": 1`, 1),
		} {
			if raw == string(good2) {
				t.Fatalf("%s: mutation did not apply", name)
			}
			if _, err := ParseTaskRecord([]byte(raw), testLookup); err == nil {
				t.Fatalf("%s accepted", name)
			}
		}
	})
	t.Run("responses", func(t *testing.T) {
		v := maxView()
		// The encoded maximal view is only the parse step's input; it is
		// built once per process and every use gets a fresh copy
		// (maxDispatchResponseJSON). The parse runs every repetition, and
		// the maximal view is still encoded every repetition below
		// (combined, a superset of it).
		b := maxDispatchResponse(t)
		if got, err := ParseDispatchResponse(b); err != nil || got.TaskID != v.TaskID || *got.Result.FinalMessage != *v.Result.FinalMessage {
			t.Fatalf("dispatch response: %v", err)
		}
		// The maximal candidate list and reason parse in a show envelope
		// (the maximal strings are proven by the dispatch parse above).
		mr := smallRejected()
		mr.Candidates, mr.Reason = maxRejected().Candidates, maxRejected().Reason
		rj, _ := Encode(TaskShowResponse{Version: ProtocolVersion, Task: mr})
		if got, err := ParseTaskShowResponse(rj); err != nil || len(got.Candidates) != MaxCandidates || len(got.Reason.Message) != MaxReasonMessageBytes {
			t.Fatalf("maximal rejection: %v", err)
		}
		// Worst case, even combined beyond what one lifecycle allows: a
		// maximum request, a 64 KiB final message escaped sixfold, two
		// appearances of a sixfold-escaped 64 KiB tail, 100 candidates and
		// a full reason, within the 2 MiB single-task bound.
		combined := maxView()
		combined.Candidates, combined.Reason = maxRejected().Candidates, maxRejected().Reason
		cb, _ := Encode(DispatchResponse{Version: ProtocolVersion, TaskID: combined.TaskID, Task: &combined})
		if len(cb)+1 > MaxTaskViewBytes || len(b) < 1<<20 {
			t.Fatalf("worst-case view is %d bytes (success alone %d)", len(cb), len(b))
		}
		// (The maximal success view parses above in its dispatch envelope
		// and the maximal show envelope as the rejection.)
		// Each invariant is checked on a small valid view: the maximum
		// sizes are proven above, and repeating them per rejection only
		// costs time under the race detector. Both bases are valid.
		for _, base := range []TaskView{smallView(), smallRejected()} {
			if b, _ := Encode(TaskShowResponse{Version: ProtocolVersion, Task: base}); func() error { _, err := ParseTaskShowResponse(b); return err }() != nil {
				t.Fatalf("small %s view rejected", base.State)
			}
		}
		for name, c := range map[string]struct {
			base func() TaskView
			mut  func(*TaskView)
		}{
			"result on pending":  {smallView, func(v *TaskView) { v.State, v.FinishedAt, v.Result.State = TaskRunning, nil, TaskRunning }},
			"no result":          {smallView, func(v *TaskView) { v.Result = nil }},
			"state mismatch":     {smallView, func(v *TaskView) { v.Result.State = TaskFailed }},
			"commit":             {smallView, func(v *TaskView) {}},
			"reconciling final":  {smallView, func(v *TaskView) { v.Reconciling = true }},
			"persistence reason": {smallView, func(v *TaskView) { v.PersistenceReason = s("other") }},
			"policy":             {smallView, func(v *TaskView) { v.TimeoutPolicy = "odd" }},
			"stop on success":    {smallView, func(v *TaskView) { v.StopRequested = true }},
			"dropped":            {smallView, func(v *TaskView) { v.Log.DroppedBytes++ }},
			"tail":               {smallView, func(v *TaskView) { v.Result.LogTail = "other" }},
			"success candidates": {smallView, func(v *TaskView) { v.Candidates = smallRejected().Candidates }},
			"candidate reason":   {smallRejected, func(v *TaskView) { v.Candidates[0].Reason = "odd" }},
			"candidate accept":   {smallRejected, func(v *TaskView) { v.Candidates[0].CanAccept = false }},
			"reconciling count":  {smallRejected, func(v *TaskView) { v.Candidates[0].ReconcilingInflight = v.Candidates[0].Inflight + 1 }},
			"rejected reason":    {smallRejected, func(v *TaskView) { v.Reason = nil }},
		} {
			bad := c.base()
			c.mut(&bad)
			b, err := Encode(TaskShowResponse{Version: ProtocolVersion, Task: bad})
			if err != nil {
				t.Fatal(err)
			}
			// TaskResult's encoder always renders result_commit null, so
			// inject a value textually.
			if name == "commit" {
				b = bytes.Replace(b, []byte(`"result_commit":null`), []byte(`"result_commit":"abc"`), 1)
			}
			if _, err := ParseTaskShowResponse(b); err == nil {
				t.Fatalf("%s accepted", name)
			}
		}
		// A protocol-3 envelope is a mismatch naming both versions.
		small, _ := Encode(TaskShowResponse{Version: ProtocolVersion, Task: smallView()})
		if _, err := ParseTaskShowResponse(bytes.Replace(small, []byte(`{"version":5`), []byte(`{"version":3`), 1)); !IsCode(err, CodeProtocolMismatch) {
			t.Fatalf("version 3 view: %v", err)
		}
		// Lists: ascending IDs, at most 100, next_after is the last ID.
		sum := func(id string) TaskSummary {
			return TaskSummary{TaskID: id, Target: v.Request.Target, Role: v.Role, State: TaskPending, CreatedAt: v.CreatedAt,
				Effective: v.Effective, RequestedBy: v.Request.RequestedBy}
		}
		ids := []string{"t_00000000000000000000000000000001", "t_00000000000000000000000000000002"}
		next := ids[1]
		lb, _ := Encode(TaskListResponse{Version: ProtocolVersion, Tasks: []TaskSummary{sum(ids[0]), sum(ids[1])}, NextAfter: &next})
		if got, n, err := ParseTaskListResponse(lb); err != nil || len(got) != 2 || *n != next {
			t.Fatalf("list %v", err)
		}
		if b, _ := Encode(TaskListResponse{Version: ProtocolVersion}); string(b) != `{"version":5,"tasks":[],"next_after":null}` {
			t.Fatalf("empty page %s", b)
		}
		for name, r := range map[string]TaskListResponse{
			"unsorted":  {Version: 5, Tasks: []TaskSummary{sum(ids[1]), sum(ids[0])}},
			"duplicate": {Version: 5, Tasks: []TaskSummary{sum(ids[0]), sum(ids[0])}},
			"cursor":    {Version: 5, Tasks: []TaskSummary{sum(ids[0])}, NextAfter: &next},
			"empty cur": {Version: 5, NextAfter: &next},
		} {
			b, _ := Encode(r)
			if _, _, err := ParseTaskListResponse(b); err == nil {
				t.Fatalf("%s accepted", name)
			}
		}
		many := TaskListResponse{Version: 5}
		for i := 0; i <= MaxTaskListLimit; i++ {
			many.Tasks = append(many.Tasks, sum("t_"+strings.Repeat("0", 29)+strconv.FormatInt(int64(100+i), 10)))
		}
		if b, _ := Encode(many); func() error { _, _, err := ParseTaskListResponse(b); return err }() == nil {
			t.Fatal("101 tasks accepted")
		}
		many.Tasks = many.Tasks[:MaxTaskListLimit]
		if b, _ := Encode(many); len(b)+1 > MaxTaskListBytes {
			t.Fatalf("a full page is %d bytes", len(b))
		}
		// Logs: byte exact base64; counters consistent.
		data := []byte{0, 1, 0xff, '\n', 0x1b}
		lr := TaskLogsResponse{Version: 5, TaskID: testTaskID, Data: data, RetainedBytes: 5, SourceBytes: 9, DroppedBytes: 4, Truncated: true}
		b, _ = Encode(lr)
		if got, err := ParseTaskLogsResponse(b); err != nil || !bytes.Equal(got.Data, data) {
			t.Fatalf("logs %v", err)
		}
		// The spliced encoder renders exactly the generic encoder's form.
		if generic, _ := json.Marshal(lr); string(b) != string(generic) || string(b) != `{"version":5,"task_id":"`+testTaskID+`","data":"AAH/Chs=","retained_bytes":5,"source_bytes":9,"dropped_bytes":4,"truncated":true,"incomplete":false,"counter_overflow":false,"log_may_be_incomplete":false}` {
			t.Fatalf("logs encoding %s", b)
		}
		for name, mut := range map[string]func(*TaskLogsResponse){
			"retained": func(r *TaskLogsResponse) { r.RetainedBytes = 4 },
			"dropped":  func(r *TaskLogsResponse) { r.DroppedBytes = 0 },
			"source":   func(r *TaskLogsResponse) { r.SourceBytes = 1 },
			"overflow": func(r *TaskLogsResponse) { r.CounterOverflow = true },
		} {
			bad := lr
			mut(&bad)
			b, _ := Encode(bad)
			if _, err := ParseTaskLogsResponse(b); err == nil {
				t.Fatalf("logs %s accepted", name)
			}
		}
		full := TaskLogsResponse{Version: 5, TaskID: testTaskID, Data: make([]byte, MaxLogRetainedBytes), RetainedBytes: MaxLogRetainedBytes, SourceBytes: MaxLogRetainedBytes}
		if b, _ := Encode(full); len(b)+1 > MaxTaskLogsBytes || len(b) < base64.StdEncoding.EncodedLen(MaxLogRetainedBytes) {
			t.Fatalf("a full log response is %d bytes", len(b))
		}
	})
	t.Run("tail", func(t *testing.T) {
		for _, c := range []struct {
			data      string
			lines     int
			tail      string
			truncated bool
		}{
			{"a\nb\nc\n", 2, "b\nc\n", true},
			{"a\nb\nc\n", 3, "a\nb\nc\n", false},
			{"a\nb\nc\n", 20, "a\nb\nc\n", false},
			{"a\nb\nc", 1, "c", true},
			{"a\nb\nc", 2, "b\nc", true},
			{"", 20, "", false},
			{"a\n", 0, "", true},
			{"\n\n", 1, "\n", true},
			{"x\xff\n", 1, "x�\n", false},
		} {
			tail, tr := LogTail([]byte(c.data), c.lines)
			if tail != c.tail || tr != c.truncated {
				t.Fatalf("LogTail(%q, %d) = %q %v, want %q %v", c.data, c.lines, tail, tr, c.tail, c.truncated)
			}
		}
		// Only the final 64 KiB are considered; the cut prefix is a
		// replaced partial line.
		big := bytes.Repeat([]byte("é"), MaxTailBytes)
		tail, tr := LogTail(big, 200)
		if !tr || len(tail) > MaxTailBytes+3 || !strings.HasSuffix(tail, "é") {
			t.Fatalf("64 KiB window: %d bytes, %v", len(tail), tr)
		}
	})
	t.Run("meta", func(t *testing.T) {
		m := MetaOf(TaskLog{Data: []byte("abc"), SourceBytes: 10, ReceivedBytes: 8, Incomplete: true}, true)
		if m.RetainedBytes != 3 || m.DroppedBytes != 7 || !m.Truncated || !m.LogMayBeIncomplete {
			t.Fatalf("meta %+v", m)
		}
		if m := MetaOf(TaskLog{Data: []byte("abc"), SourceBytes: 3, ReceivedBytes: 3}, false); m.Truncated || m.DroppedBytes != 0 {
			t.Fatalf("complete meta %+v", m)
		}
		if _, err := ParseCandidates([]any{map[string]any{"role_id": "a"}}); err == nil {
			t.Fatal("a partial candidate was accepted")
		}
		c := maxRejected().Candidates[:2]
		b, _ := json.Marshal(c)
		var generic any
		json.Unmarshal(b, &generic)
		if got, err := ParseCandidates(generic); err != nil || len(got) != 2 {
			t.Fatalf("candidates %v", err)
		}
	})
}

// maxView is a succeeded view at the response bounds: a maximal request,
// a 64 KiB final message and a 64 KiB tail (twice) of control bytes that
// JSON escapes sixfold.
func maxView() TaskView {
	req := validRequest()
	req.Goal = strings.Repeat("\x01", MaxGoalBytes)
	req.Acceptance = strings.Repeat("<", MaxAcceptanceBytes)
	for i := 0; i < MaxPayloadPointers; i++ {
		if i < len(req.Payload) {
			req.Payload[i] = strings.Repeat("\"", 1000)
		} else {
			req.Payload = append(req.Payload, strings.Repeat("\"", 1000))
		}
	}
	msg := strings.Repeat("\x02", MaxFinalMessageBytes)
	tail := strings.Repeat("\x1b", MaxTailBytes)
	zero := 0
	now := FormatTime(time.Date(2026, 9, 27, 10, 0, 0, 0, time.UTC))
	return TaskView{TaskID: testTaskID, Request: req, Role: PublicRole(validRole()), Effective: TaskEffective{Model: "example", Effort: "medium", Timeout: time.Hour},
		TimeoutPolicy: TimeoutPolicyLegacy, State: TaskSucceeded, CreatedAt: now, StartedAt: &now, FinishedAt: &now, DurabilityConfirmed: true,
		Log: TaskLogMeta{RetainedBytes: 10, SourceBytes: 10, ReceivedBytes: 10}, LogTail: tail,
		Result: &TaskResult{State: TaskSucceeded, ExitCode: &zero, FinalMessage: &msg, LogTail: tail}}
}

// maxDispatchResponseJSON is maxView encoded in a dispatch envelope, an
// input of TestTaskContract/responses only. Encoding it costs about 0.1 s
// under the race detector, which the stress stage would otherwise pay 60
// times per process, so it is built once per process. It is held as a
// string, which is immutable, and maxDispatchResponse hands every use its
// own []byte copy: a parse or a mutation in one repetition cannot reach the
// next (TestInputFixturesPerUse). Only the input is shared; the parse and
// its assertions run every repetition.
var maxDispatchResponseJSON = sync.OnceValues(func() (string, error) {
	v := maxView()
	b, err := Encode(DispatchResponse{Version: ProtocolVersion, TaskID: v.TaskID, Task: &v})
	return string(b), err
})

// maxDispatchResponse returns a fresh copy of maxDispatchResponseJSON.
func maxDispatchResponse(t testing.TB) []byte {
	t.Helper()
	enc, err := maxDispatchResponseJSON()
	if err != nil {
		t.Fatal(err)
	}
	return []byte(enc)
}

// TestInputFixturesPerUse proves the process-wide test inputs
// (maxDispatchResponseJSON, frameLimitInputs) are copied per use: each
// repetition overwrites its copies with NUL bytes, which neither the
// encoder nor the fixtures ever produce, and both a second copy in the same
// repetition and the first copy of every later repetition (-count) must be
// unscribbled. Nothing here parses the 1.4 MB view; the parse belongs to
// TestTaskContract/responses.
func TestInputFixturesPerUse(t *testing.T) {
	clean := func(name string, b []byte) {
		t.Helper()
		if len(b) == 0 || bytes.IndexByte(b, 0) >= 0 || b[0] != '{' {
			t.Fatalf("%s: a previous use's mutation leaked into the fixture", name)
		}
	}
	scribble := func(b []byte) { clear(b) }
	first := maxDispatchResponse(t)
	clean("dispatch response", first)
	want := string(first)
	scribble(first)
	if again := maxDispatchResponse(t); string(again) != want || &again[0] == &first[0] {
		t.Fatal("dispatch response: a use's mutation reached the next use")
	}
	in := frameLimitInputs()
	for _, typ := range []string{FrameHello, FrameRolesReplace} {
		msg := []byte(in.typed[typ].at)
		clean(typ, msg)
		pristine := string(msg)
		if typ == FrameHello {
			// The decoded body may alias its input; scribble both.
			f, err := DecodeFrame(msg, FromSidecar)
			if err != nil {
				t.Fatal(err)
			}
			scribble(f.Body)
		}
		scribble(msg)
		if again := []byte(in.typed[typ].at); string(again) != pristine {
			t.Fatalf("%s: a use's mutation reached the next use", typ)
		}
	}
	for name, m := range map[string]string{"exact": in.exact, "above": in.aboveExact} {
		b := []byte(m)
		if bytes.IndexByte(b, 0) >= 0 || len(b) < MaxFrameBytes {
			t.Fatalf("%s: fixture changed", name)
		}
		scribble(b)
	}
	if in.pad(MaxFrameBytes) != strings.Repeat("a", MaxFrameBytes) {
		t.Fatal("padding changed")
	}
}

// smallView is a valid succeeded view with short fields.
func smallView() TaskView {
	msg, tail, zero := "done", "tail\n", 0
	now := FormatTime(time.Date(2026, 9, 27, 10, 0, 0, 0, time.UTC))
	return TaskView{TaskID: testTaskID, Request: validRequest(), Role: PublicRole(validRole()), Effective: TaskEffective{Model: "example", Effort: "medium", Timeout: time.Hour},
		TimeoutPolicy: TimeoutPolicyEnforced, State: TaskSucceeded, CreatedAt: now, StartedAt: &now, FinishedAt: &now, DurabilityConfirmed: true,
		Log: TaskLogMeta{RetainedBytes: 5, SourceBytes: 5, ReceivedBytes: 5}, LogTail: tail,
		Result: &TaskResult{State: TaskSucceeded, ExitCode: &zero, FinalMessage: &msg, LogTail: tail}}
}

// smallRejected is a valid rejected view with three candidates.
func smallRejected() TaskView {
	v := smallView()
	for i := 0; i < 3; i++ {
		v.Candidates = append(v.Candidates, TaskCandidate{RoleID: "role-" + strconv.Itoa(i), NodeID: testID, RegistrationOrder: i + 1, NodeLiveness: LivenessOnline,
			Inflight: 5, ReconcilingInflight: 1, Concurrency: MaxConcurrency, CanAccept: true, Reason: ReasonAvailable})
	}
	v.State, v.StartedAt = TaskRejected, nil
	v.Result = &TaskResult{State: TaskRejected, LogTail: v.LogTail}
	v.Reason = &TaskReason{Code: ReasonStartNotSent, Message: "not sent"}
	return v
}

// maxRejected is a rejected view carrying the maximum candidate list.
func maxRejected() TaskView {
	v := maxView()
	var cands []TaskCandidate
	for i := 0; i < MaxCandidates; i++ {
		cands = append(cands, TaskCandidate{RoleID: "role-" + strconv.Itoa(i), NodeID: testID, RegistrationOrder: i + 1, NodeLiveness: LivenessOnline,
			Inflight: 5, ReconcilingInflight: 1, Concurrency: MaxConcurrency, CanAccept: true, Reason: ReasonAvailable})
	}
	v.Candidates = cands
	v.State, v.StartedAt = TaskRejected, nil
	v.Result = &TaskResult{State: TaskRejected, LogTail: v.LogTail}
	v.Reason = &TaskReason{Code: ReasonStartNotSent, Message: strings.Repeat("m", MaxReasonMessageBytes)}
	return v
}

// BenchmarkTaskEnvelope measures the maximum legal dispatch request's
// strict decode and canonical encode, checking the round trip and the
// bounds every iteration.
func BenchmarkTaskEnvelope(b *testing.B) {
	pointer := strings.Repeat("\"", MaxPointerBytes/2)
	ptrs := make([]string, MaxPayloadPointers)
	for i := range ptrs {
		ptrs[i] = strconv.Quote(pointer)
	}
	// JSON quoting: strconv.Quote writes \x escapes, which are not JSON.
	quote := func(v string) string { q, _ := json.Marshal(v); return string(q) }
	body := dispatchJSON(map[string]*string{"goal": s(quote(strings.Repeat("\x01", MaxGoalBytes))), "acceptance": s(quote(strings.Repeat("a", MaxAcceptanceBytes))),
		"payload": s(`[` + strings.Join(ptrs, ",") + `]`), "override": s(`{"model":"m","effort":"high","timeout":"0s"}`)})
	if len(body) > MaxDispatchRequestBytes {
		b.Fatalf("fixture is %d bytes", len(body))
	}
	b.ReportAllocs()
	for b.Loop() {
		r, err := ParseDispatchRequest([]byte(body))
		if err != nil {
			b.Fatal(err)
		}
		enc, err := Encode(r)
		if err != nil || len(enc) > MaxDispatchRequestBytes || len(r.Goal) != MaxGoalBytes {
			b.Fatalf("encode %d bytes: %v", len(enc), err)
		}
	}
}
