package contract

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"
)

// Iteration 10b contract tests (UT-B1/B7): the dispatch selection and the
// immutable binding, the preparation frames, the result DTO and its
// bounds, the publication API envelopes, the durable intent, the protocol
// 6 digest, the public mirrors, the node assignment headers and the
// publication checkpoint. Delegated from tests/function
// (TestTaskWorkspaceMetadata/matrix). Do not rename it.

const (
	wsInst   = "0123456789abcdef0123456789abcdef"
	wsCommit = "0123456789abcdef0123456789abcdef01234567"
	wsTree   = "89abcdef0123456789abcdef0123456789abcdef"
	wsPub    = "fedcba9876543210fedcba9876543210"
)

func wsBinding() WorkspaceBinding {
	c := wsCommit
	return WorkspaceBinding{Name: "proj", Instance: wsInst, BaseSelector: DefaultBranchRef, BaseCommit: &c}
}

// wsRequest is validRequest with a workspace selection.
func wsRequest() DispatchRequest {
	r := validRequest()
	r.Workspace = s("proj")
	return r
}

func i64(n int64) *int64 { return &n }

// wsPublished is a published DTO of b with one added row.
func wsPublished(b WorkspaceBinding, taskID string) TaskWorkspaceResult {
	c, ref := wsTree, TaskRefPrefix+taskID
	mode := "100644"
	return TaskWorkspaceResult{Name: b.Name, Instance: b.Instance, BaseCommit: b.BaseCommit, Publication: PublicationPublished, Commit: &c, Ref: &ref,
		Diffstat: &TaskDiffstat{Added: 1, NewBytes: 3}, Changes: []WorkspaceChange{{PathBase64: EncodePath([]byte("a.txt")), Kind: "added", NewMode: &mode, NewBytes: i64(3)}}}
}

func wantInvalid(t *testing.T, name string, err error) {
	t.Helper()
	if CodeOf(err) != CodeInvalidArgument {
		t.Fatalf("%s: err = %v, want invalid_argument", name, err)
	}
}

func TestTaskWorkspaceContract(t *testing.T) {
	t.Run("dispatch", func(t *testing.T) {
		for name, sub := range map[string]map[string]*string{
			"empty-workspace":      {"workspace": s(`""`)},
			"null-workspace":       {"workspace": s(`null`)},
			"bad-workspace":        {"workspace": s(`"Bad"`)},
			"base-alone":           {"base": s(`"main"`)},
			"instance-alone":       {"workspace_instance": s(`"` + wsInst + `"`)},
			"null-base":            {"workspace": s(`"proj"`), "base": s(`null`)},
			"bad-instance":         {"workspace": s(`"proj"`), "workspace_instance": s(`"XYZ"`)},
			"spaced-base":          {"workspace": s(`"proj"`), "base": s(`" main"`)},
			"expression":           {"workspace": s(`"proj"`), "base": s(`"main~1"`)},
			"non-string-workspace": {"workspace": s(`7`)},
		} {
			_, err := ParseDispatchRequest([]byte(dispatchJSON(sub)))
			wantInvalid(t, name, err)
		}
		for base, want := range map[string]Selector{
			"":                             {Kind: SelectorKindBranch, Value: DefaultBranchRef},
			"empty":                        {Kind: SelectorKindEmpty},
			"dev":                          {Kind: SelectorKindBranch, Value: "refs/heads/dev"},
			wsCommit:                       {Kind: SelectorKindHash, Value: wsCommit},
			"t_" + strings.Repeat("a", 32): {Kind: SelectorKindTask, Value: TaskRefPrefix + "t_" + strings.Repeat("a", 32)},
		} {
			sub := map[string]*string{"workspace": s(`"proj"`), "workspace_instance": s(`"` + wsInst + `"`)}
			if base != "" {
				sub["base"] = s(`"` + base + `"`)
			}
			r, err := ParseDispatchRequest([]byte(dispatchJSON(sub)))
			if err != nil {
				t.Fatalf("base %q: %v", base, err)
			}
			sel, err := ParseTaskBase(r.Base)
			if err != nil || sel.Kind != want.Kind || (want.Kind != SelectorKindEmpty && sel.Value != want.Value) {
				t.Fatalf("base %q: %+v %v", base, sel, err)
			}
			// The canonical encoding round-trips (workspace fields omitted
			// only when nil).
			enc, err := Encode(r)
			if err != nil {
				t.Fatal(err)
			}
			if again, err := ParseDispatchRequest(enc); err != nil || *again.Workspace != "proj" || *again.WorkspaceInstance != wsInst {
				t.Fatalf("round trip %s: %v", enc, err)
			}
		}
		if enc, _ := Encode(validRequest()); bytes.Contains(enc, []byte("workspace")) || bytes.Contains(enc, []byte(`"base"`)) {
			t.Fatalf("a no-workspace request names workspace fields: %s", enc)
		}
		// Maximum envelope with every added field still parses.
		req := maxView().Request
		req.Workspace, req.Base, req.WorkspaceInstance = s(strings.Repeat("a", 63)), s("refs/heads/"+strings.Repeat("b", 200)), s(wsInst)
		enc, err := Encode(req)
		if err != nil || len(enc) > MaxDispatchRequestBytes {
			t.Fatalf("max request %d bytes: %v", len(enc), err)
		}
		if _, err := ParseDispatchRequest(enc); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("binding", func(t *testing.T) {
		good := wsBinding()
		if err := good.Validate(); err != nil {
			t.Fatal(err)
		}
		empty := WorkspaceBinding{Name: "proj", Instance: wsInst, BaseSelector: SelectorEmpty}
		if err := empty.Validate(); err != nil || !empty.Empty() || good.Empty() {
			t.Fatalf("empty binding: %v", err)
		}
		hash := WorkspaceBinding{Name: "proj", Instance: wsInst, BaseSelector: wsCommit, BaseCommit: s(wsCommit)}
		if err := hash.Validate(); err != nil {
			t.Fatal(err)
		}
		bad := map[string]WorkspaceBinding{
			"name":           {Name: "X", Instance: wsInst, BaseSelector: DefaultBranchRef, BaseCommit: s(wsCommit)},
			"instance":       {Name: "proj", Instance: "x", BaseSelector: DefaultBranchRef, BaseCommit: s(wsCommit)},
			"commit":         {Name: "proj", Instance: wsInst, BaseSelector: DefaultBranchRef, BaseCommit: s("abc")},
			"short-branch":   {Name: "proj", Instance: wsInst, BaseSelector: "main", BaseCommit: s(wsCommit)},
			"empty-commit":   {Name: "proj", Instance: wsInst, BaseSelector: SelectorEmpty, BaseCommit: s(wsCommit)},
			"missing-commit": {Name: "proj", Instance: wsInst, BaseSelector: DefaultBranchRef},
			"hash-mismatch":  {Name: "proj", Instance: wsInst, BaseSelector: wsCommit, BaseCommit: s(wsTree)},
		}
		for name, b := range bad {
			wantInvalid(t, name, b.Validate())
		}
		var got WorkspaceBinding
		raw, _ := json.Marshal(good)
		if err := got.unmarshalStrict(raw, "b"); err != nil || got.Name != "proj" {
			t.Fatal(err)
		}
		raw, _ = json.Marshal(bad["name"])
		wantInvalid(t, "strict", got.unmarshalStrict(raw, "b"))
		wantInvalid(t, "unknown key", got.unmarshalStrict([]byte(`{"name":"proj","x":1}`), "b"))
		if SelectorString(Selector{Kind: SelectorKindEmpty}) != SelectorEmpty || SelectorString(Selector{Kind: SelectorKindHash, Value: wsCommit}) != wsCommit {
			t.Fatal("selector strings")
		}
		// The start body carries the binding, which changes the digest; a
		// no-workspace start's digest and wire stay those of protocol 5.
		st := TaskStartBody{TaskID: testTaskID, Execution: ExecutionToken{Epoch: wsInst, Attachment: 1}, RolesRevision: 1, Role: validRole(),
			Request: validRequest(), Effective: TaskEffective{Model: "example", Effort: "medium", Timeout: time.Hour}}
		plain := st.Digest()
		enc, _ := Encode(st)
		if bytes.Contains(enc, []byte("workspace")) {
			t.Fatalf("no-workspace start names a workspace: %s", enc)
		}
		st.Request, st.Workspace = wsRequest(), &good
		if st.Digest() == plain {
			t.Fatal("the binding is not in the start digest")
		}
		enc, _ = Encode(st)
		dec, err := DecodeTaskStart(enc, testLookup)
		if err != nil || dec.Workspace == nil || dec.Digest() != st.Digest() {
			t.Fatalf("start round trip: %v", err)
		}
		_, err = DecodeTaskStart(withField(t, enc, "workspace", "null"), testLookup)
		wantInvalid(t, "null binding", err)
		_, err = DecodeTaskStart(withField(t, enc, "workspace", ""), testLookup)
		wantInvalid(t, "missing binding for a workspace request", err)
		wantInvalid(t, "binding without request", CheckBinding(validRequest(), &good))
		other := good
		other.Name = "other"
		wantInvalid(t, "binding names another workspace", CheckBinding(wsRequest(), &other))
		r := wsRequest()
		r.WorkspaceInstance = s(strings.Repeat("9", 32))
		wantInvalid(t, "binding names another instance", CheckBinding(r, &good))
	})
	t.Run("start-result", func(t *testing.T) {
		enc, _ := Encode(TaskStartResult{TaskID: testTaskID, Preparing: true})
		if string(enc) != `{"task_id":"`+testTaskID+`","ok":true,"preparing":true}` {
			t.Fatalf("preparing shape %s", enc)
		}
		r, err := DecodeTaskStartResult(enc)
		if err != nil || !r.Preparing || r.Err != nil {
			t.Fatalf("preparing decode %+v %v", r, err)
		}
		for _, body := range []string{
			`{"task_id":"` + testTaskID + `","ok":true,"preparing":false}`,
			`{"task_id":"` + testTaskID + `","ok":true,"preparing":null}`,
			`{"task_id":"` + testTaskID + `","ok":false,"preparing":true,"error":{"code":"conflict","message":"x"}}`,
		} {
			_, err := DecodeTaskStartResult([]byte(body))
			wantInvalid(t, body, err)
		}
		for _, reason := range PreparationReasons {
			found := false
			for _, r := range StartRefusalReasons {
				found = found || r == reason
			}
			if !found {
				t.Fatalf("%s is not a start refusal reason", reason)
			}
		}
	})
	t.Run("prepared-frames", func(t *testing.T) {
		exec := ExecutionToken{Epoch: wsInst, Attachment: 2}
		ok := TaskPreparedBody{TaskID: testTaskID, Execution: exec, StartDigest: strings.Repeat("a", 64), OK: true}
		enc, err := Encode(ok)
		if err != nil {
			t.Fatal(err)
		}
		if got, err := DecodeTaskPrepared(enc); err != nil || !got.OK || got.Error != nil || got.Execution != exec {
			t.Fatalf("ok decode %+v %v", got, err)
		}
		fail := ok
		fail.OK, fail.Error = false, TaskError(CodeUnavailable, "", ReasonWorkspaceBaseUnavailable, "the base is gone")
		enc, err = Encode(fail)
		if err != nil {
			t.Fatal(err)
		}
		if got, err := DecodeTaskPrepared(enc); err != nil || got.OK || got.Error == nil || got.Error.Details["reason"] != ReasonWorkspaceBaseUnavailable {
			t.Fatalf("failure decode %+v %v", got, err)
		}
		okEnc, _ := Encode(ok)
		for name, body := range map[string][]byte{
			"ok-with-error":      withField(t, okEnc, "error", `{"code":"conflict","message":"x"}`),
			"failure-null-error": withField(t, enc, "error", "null"),
			"missing-error":      withField(t, okEnc, "error", ""),
			"bad-task":           withField(t, okEnc, "task_id", `"x"`),
			"bad-digest":         withField(t, okEnc, "start_digest", `"abc"`),
			"unknown-key":        withField(t, okEnc, "extra", `1`),
			"foreign-reason":     withField(t, enc, "error", `{"code":"conflict","message":"x","details":{"reason":"local_full"}}`),
			"oversized":          append(okEnc, bytes.Repeat([]byte(" "), MaxTaskPreparedBody)...),
		} {
			_, err := DecodeTaskPrepared(body)
			wantInvalid(t, name, err)
		}
		ack := TaskPreparedAckBody{TaskID: testTaskID, Execution: exec, Release: true}
		enc, _ = Encode(ack)
		if got, err := DecodeTaskPreparedAck(enc); err != nil || !got.Release {
			t.Fatalf("ack %+v %v", got, err)
		}
		for name, body := range map[string][]byte{
			"bad-task": withField(t, enc, "task_id", `"x"`), "extra": withField(t, enc, "x", "1"), "missing": withField(t, enc, "release", ""),
			"big": append(enc, bytes.Repeat([]byte(" "), MaxTaskPreparedBody)...),
		} {
			_, err := DecodeTaskPreparedAck(body)
			wantInvalid(t, name, err)
		}
		// Both frames are typed in their directions with the 8 KiB limit.
		if limit := BodyLimit(FrameTaskPrepared); limit != MaxTaskPreparedBody {
			t.Fatalf("task_prepared body limit %d", limit)
		}
		if limit := BodyLimit(FrameTaskPreparedAck); limit != MaxTaskPreparedBody {
			t.Fatalf("task_prepared_ack body limit %d", limit)
		}
	})
	t.Run("dto", func(t *testing.T) {
		b := wsBinding()
		pub := wsPublished(b, testTaskID)
		for name, r := range map[string]TaskWorkspaceResult{
			"published": pub, "failed": NewPublicationFailed(b, PubErrStorageFailed), "not-started": NewNotStarted(b), "not-applicable": NewNotApplicable(b),
		} {
			if err := r.Validate(); err != nil {
				t.Fatalf("%s: %v", name, err)
			}
			enc, _ := json.Marshal(r)
			var back TaskWorkspaceResult
			if err := back.unmarshalStrict(enc, "w"); err != nil {
				t.Fatalf("%s round trip: %v", name, err)
			}
			for _, key := range []string{"name", "instance", "base_commit", "publication", "commit", "ref", "error", "diffstat", "changes", "changes_truncated", "next_after"} {
				if !bytes.Contains(enc, []byte(`"`+key+`":`)) {
					t.Fatalf("%s lacks %s: %s", name, key, enc)
				}
			}
		}
		mut := func(f func(r *TaskWorkspaceResult)) TaskWorkspaceResult {
			r := wsPublished(b, testTaskID)
			r.Changes = append([]WorkspaceChange(nil), r.Changes...)
			f(&r)
			return r
		}
		for name, r := range map[string]TaskWorkspaceResult{
			"no-name":          mut(func(r *TaskWorkspaceResult) { r.Name = "" }),
			"bad-base":         mut(func(r *TaskWorkspaceResult) { r.BaseCommit = s("x") }),
			"publication":      mut(func(r *TaskWorkspaceResult) { r.Publication = "maybe" }),
			"no-commit":        mut(func(r *TaskWorkspaceResult) { r.Commit = nil }),
			"bad-ref":          mut(func(r *TaskWorkspaceResult) { r.Ref = s("refs/heads/main") }),
			"error":            mut(func(r *TaskWorkspaceResult) { r.Error = s(PubErrStorageFailed) }),
			"no-totals":        mut(func(r *TaskWorkspaceResult) { r.Diffstat = nil }),
			"negative":         mut(func(r *TaskWorkspaceResult) { r.Diffstat.Added = -1 }),
			"overflow":         mut(func(r *TaskWorkspaceResult) { r.Diffstat.NewBytes = MaxSafeInteger + 1 }),
			"unsorted":         mut(func(r *TaskWorkspaceResult) { r.Changes = append(r.Changes, r.Changes[0]) }),
			"bad-path":         mut(func(r *TaskWorkspaceResult) { r.Changes[0].PathBase64 = "!!" }),
			"bad-row":          mut(func(r *TaskWorkspaceResult) { r.Changes[0].Kind = "renamed" }),
			"next-untruncated": mut(func(r *TaskWorkspaceResult) { r.NextAfter = s(r.Changes[0].PathBase64) }),
			"wrong-next":       mut(func(r *TaskWorkspaceResult) { r.ChangesTruncated, r.NextAfter = true, s(EncodePath([]byte("z"))) }),
			"next-no-rows":     mut(func(r *TaskWorkspaceResult) { r.Changes, r.ChangesTruncated, r.NextAfter = nil, true, s("YQ==") }),
			"failed-with-hash": func() TaskWorkspaceResult {
				r := NewPublicationFailed(b, PubErrStorageFailed)
				r.Commit = s(wsTree)
				return r
			}(),
			"failed-no-error": func() TaskWorkspaceResult { r := NewPublicationFailed(b, PubErrStorageFailed); r.Error = nil; return r }(),
			"failed-unknown":  NewPublicationFailed(b, "disk_on_fire"),
			"not-started-err": func() TaskWorkspaceResult { r := NewNotStarted(b); r.Error = s(PubErrStorageFailed); return r }(),
			"too-many-rows": mut(func(r *TaskWorkspaceResult) {
				r.Changes = nil
				for i := 0; i <= MaxWorkspaceChanges; i++ {
					r.Changes = append(r.Changes, WorkspaceChange{PathBase64: EncodePath([]byte{byte('a' + i/26), byte('a' + i%26)}), Kind: "added", NewMode: s("100644"), NewBytes: i64(1)})
				}
			}),
		} {
			wantInvalid(t, name, r.Validate())
		}
		var back TaskWorkspaceResult
		wantInvalid(t, "huge", back.unmarshalStrict(bytes.Repeat([]byte(" "), MaxWorkspaceDTOBytes*2+1), "w"))
		if enc, _ := json.Marshal(TaskWorkspaceResult{Name: "proj", Instance: wsInst, Publication: PublicationNotApplicable}); !bytes.Contains(enc, []byte(`"changes":[]`)) {
			t.Fatalf("nil changes render %s", enc)
		}
		for _, e := range PublicationErrors {
			if !ValidPublicationError(e) {
				t.Fatal(e)
			}
		}
		if ValidPublicationError("x") || !PublishableState(TaskTimedOut) || PublishableState(TaskLost) || PublishableState(TaskRejected) {
			t.Fatal("enums")
		}
	})
	t.Run("bounds", func(t *testing.T) {
		b := wsBinding()
		rows := func(n, pathLen int) []ChangeRow {
			var out []ChangeRow
			for i := 0; i < n; i++ {
				p := []byte(strings.Repeat("p", pathLen) + string(rune('a'+i/676)) + string(rune('a'+i/26%26)) + string(rune('a'+i%26)))
				out = append(out, ChangeRow{Path: p, Kind: "added", NewMode: s("100644"), NewBytes: i64(int64(i))})
			}
			return out
		}
		check := func(name string, r TaskWorkspaceResult, wantRows int, truncated bool) {
			t.Helper()
			if err := r.Validate(); err != nil {
				t.Fatalf("%s: %v", name, err)
			}
			enc, _ := Encode(r)
			if len(r.Changes) != wantRows || r.ChangesTruncated != truncated || len(enc) > MaxWorkspaceDTOBytes {
				t.Fatalf("%s: %d rows (want %d) truncated %v, %d bytes", name, len(r.Changes), wantRows, r.ChangesTruncated, len(enc))
			}
			if truncated && wantRows > 0 && (r.NextAfter == nil || *r.NextAfter != r.Changes[len(r.Changes)-1].PathBase64) {
				t.Fatalf("%s next_after %v", name, r.NextAfter)
			}
		}
		// Exactly 100 rows fit; the 101st truncates by count.
		r := wsPublished(b, testTaskID)
		if err := BoundChanges(&r, rows(100, 1)); err != nil {
			t.Fatal(err)
		}
		check("100", r, 100, false)
		if err := BoundChanges(&r, rows(101, 1)); err != nil {
			t.Fatal(err)
		}
		check("101", r, 100, true)
		// Long paths truncate by the 32 KiB budget (base64 expansion
		// counted), never past it.
		if err := BoundChanges(&r, rows(100, 400)); err != nil {
			t.Fatal(err)
		}
		if !r.ChangesTruncated || len(r.Changes) == 0 || len(r.Changes) >= 100 {
			t.Fatalf("byte bound: %d rows truncated %v", len(r.Changes), r.ChangesTruncated)
		}
		check("bytes", r, len(r.Changes), true)
		// A first row too large for the DTO: no rows, truncated, null next.
		if err := BoundChanges(&r, []ChangeRow{{Path: bytes.Repeat([]byte("x"), MaxWorkspaceDTOBytes), Kind: "added", NewMode: s("100644"), NewBytes: i64(1)}}); err != nil {
			t.Fatal(err)
		}
		if len(r.Changes) != 0 || !r.ChangesTruncated || r.NextAfter != nil || r.Validate() != nil {
			t.Fatalf("first row too large: %+v", r)
		}
		// Raw bytes: control and non-UTF-8 names travel as base64 only.
		raw := []byte("ctl\x01\xff\xfe")
		if err := BoundChanges(&r, []ChangeRow{{Path: raw, Kind: "added", NewMode: s("100644"), NewBytes: i64(0)}}); err != nil {
			t.Fatal(err)
		}
		got, _ := base64.StdEncoding.DecodeString(r.Changes[0].PathBase64)
		if !bytes.Equal(got, raw) || r.ChangesTruncated {
			t.Fatalf("raw path %q", got)
		}
		// The largest legal DTO fits a sealed result with a maximal final
		// message, its public view with mirrors and every embedding.
		big := wsPublished(b, testTaskID)
		BoundChanges(&big, rows(100, 220))
		msg := strings.Repeat("\x02", MaxFinalMessageBytes)
		zero := 0
		body := TaskResultBody{TaskID: testTaskID, Execution: ExecutionToken{Epoch: wsInst, Attachment: 1}, Outcome: OutcomeNatural, ExitCode: &zero,
			FinalMessage: &msg, Workspace: &big}.Sealed()
		enc, err := Encode(body)
		if err != nil || len(enc) > MaxTaskResultBody {
			t.Fatalf("max result %d bytes: %v", len(enc), err)
		}
		if _, err := DecodeTaskResult(enc); err != nil {
			t.Fatal(err)
		}
		v := maxView()
		bb := b
		v.Request.Workspace, v.WorkspaceBinding = s(b.Name), &bb
		res := v.Result.WithWorkspace(&big)
		v.Result = &res
		resp, err := Encode(DispatchResponse{Version: ProtocolVersion, TaskID: v.TaskID, Task: &v})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := ParseDispatchResponse(resp); err != nil {
			t.Fatalf("max view with the workspace DTO and mirrors (%d bytes): %v", len(resp), err)
		}
		cand := body
		cand.Workspace = nil
		cand = cand.Sealed()
		beg, _ := Encode(PublicationBeginRequest{Candidate: cand, Tree: wsTree})
		if len(beg) > MaxPublicationBeginBody {
			t.Fatalf("max begin %d bytes", len(beg))
		}
		st, _ := Encode(PublicationStatus{Phase: PubPhaseSettled, Result: &body, TaskState: TaskSucceeded, Committed: true})
		if len(st) > MaxPublicationReply {
			t.Fatalf("max status %d bytes", len(st))
		}
	})
	t.Run("publication-api", func(t *testing.T) {
		zero := 0
		cand := TaskResultBody{TaskID: testTaskID, Execution: ExecutionToken{Epoch: wsInst, Attachment: 1}, Outcome: OutcomeNatural, ExitCode: &zero}.Sealed()
		enc, _ := Encode(PublicationBeginRequest{Candidate: cand, Tree: wsTree})
		if r, err := ParsePublicationBegin(enc); err != nil || r.Tree != wsTree || r.Candidate.Digest != cand.Digest {
			t.Fatalf("begin %+v %v", r, err)
		}
		withWS := cand
		pub := wsPublished(wsBinding(), testTaskID)
		withWS.Workspace = &pub
		withWS = withWS.Sealed()
		wsEnc, _ := Encode(PublicationBeginRequest{Candidate: withWS, Tree: wsTree})
		for name, body := range map[string][]byte{
			"tree":      withField(t, enc, "tree", `"abc"`),
			"extra":     withField(t, enc, "x", "1"),
			"missing":   withField(t, enc, "tree", ""),
			"workspace": wsEnc,
			"big":       append(enc, bytes.Repeat([]byte(" "), MaxPublicationBeginBody)...),
			"bad-cand":  withField(t, enc, "candidate", `{"task_id":"x"}`),
		} {
			_, err := ParsePublicationBegin(body)
			wantInvalid(t, name, err)
		}
		resp := PublicationBeginResponse{PublicationID: wsPub, State: TaskCancelled, ExpiresAt: "2026-10-03T10:00:00Z"}
		enc, _ = Encode(resp)
		if r, err := ParsePublicationBeginResponse(enc); err != nil || r != resp {
			t.Fatalf("begin response %+v %v", r, err)
		}
		for name, body := range map[string][]byte{
			"id": withField(t, enc, "publication_id", `"x"`), "state": withField(t, enc, "state", `"lost"`), "time": withField(t, enc, "expires_at", `"soon"`),
			"big": append(enc, bytes.Repeat([]byte(" "), MaxPublicationReply)...),
		} {
			_, err := ParsePublicationBeginResponse(body)
			wantInvalid(t, name, err)
		}
		if r, err := ParsePublicationFinish([]byte(`{"error":"task_ref_exists"}`)); err != nil || r.Error != PubErrTaskRefExists {
			t.Fatal(err)
		}
		for _, body := range []string{`{"error":"nope"}`, `{"error":"task_ref_exists","x":1}`, `{}`, strings.Repeat(" ", MaxPublicationFinish+1)} {
			_, err := ParsePublicationFinish([]byte(body))
			wantInvalid(t, body, err)
		}
		settledBody := cand
		settledBody.Workspace = &pub
		settledBody = settledBody.Sealed()
		id := wsPub
		for name, c := range map[string]struct {
			st     PublicationStatus
			withID bool
		}{
			"none":           {PublicationStatus{WithID: true, Phase: PubPhaseNone, TaskState: TaskRunning}, true},
			"authorized":     {PublicationStatus{Phase: PubPhaseAuthorized, TaskState: TaskRunning}, false},
			"authorized-id":  {PublicationStatus{WithID: true, PublicationID: &id, Phase: PubPhaseAuthorized, TaskState: TaskRunning}, true},
			"settled":        {PublicationStatus{Phase: PubPhaseSettled, Result: &settledBody, TaskState: TaskSucceeded, Committed: true}, false},
			"settled-lookup": {PublicationStatus{WithID: true, PublicationID: &id, Phase: PubPhaseSettled, Result: &settledBody, TaskState: TaskSucceeded, Committed: true}, true},
		} {
			enc, err := Encode(c.st)
			if err != nil {
				t.Fatal(err)
			}
			got, err := ParsePublicationStatus(enc, c.withID)
			if err != nil || got.Phase != c.st.Phase {
				t.Fatalf("%s: %s %v", name, enc, err)
			}
			if _, err := ParsePublicationStatus(enc, !c.withID); err == nil {
				t.Fatalf("%s accepted in the other form", name)
			}
		}
		for name, c := range map[string]PublicationStatus{
			"none-by-id":        {Phase: PubPhaseNone, TaskState: TaskRunning},
			"none-with-result":  {WithID: true, Phase: PubPhaseNone, Result: &settledBody, TaskState: TaskRunning},
			"authorized-result": {Phase: PubPhaseAuthorized, Result: &settledBody, TaskState: TaskRunning},
			"lookup-no-id":      {WithID: true, Phase: PubPhaseAuthorized, TaskState: TaskRunning},
			"settled-no-result": {Phase: PubPhaseSettled, TaskState: TaskSucceeded},
			"settled-no-ws":     {Phase: PubPhaseSettled, Result: &cand, TaskState: TaskSucceeded},
			"phase":             {Phase: "maybe", TaskState: TaskRunning},
			"state":             {Phase: PubPhaseAuthorized, TaskState: "odd"},
			"committed-nonterm": {Phase: PubPhaseAuthorized, TaskState: TaskRunning, Committed: true},
			"bad-id":            {WithID: true, PublicationID: s("x"), Phase: PubPhaseAuthorized, TaskState: TaskRunning},
		} {
			enc, _ := Encode(c)
			_, err := ParsePublicationStatus(enc, c.WithID)
			wantInvalid(t, name, err)
		}
		_, err := ParsePublicationStatus(bytes.Repeat([]byte(" "), MaxPublicationReply+1), false)
		wantInvalid(t, "big", err)
	})
	t.Run("intent", func(t *testing.T) {
		zero := 0
		cand := TaskResultBody{TaskID: testTaskID, Execution: ExecutionToken{Epoch: wsInst, Attachment: 1}, Outcome: OutcomeNatural, ExitCode: &zero}.Sealed()
		p := TaskPublication{ID: wsPub, State: TaskSucceeded, Tree: wsTree, Commit: wsCommit, Instance: wsInst, Deadline: time.Date(2026, 10, 3, 10, 0, 0, 0, time.UTC),
			Phase: PubPhaseAuthorized, Candidate: cand}
		enc, err := Encode(p)
		if err != nil {
			t.Fatal(err)
		}
		var back TaskPublication
		if err := back.unmarshalStrict(enc, "p"); err != nil || !back.Deadline.Equal(p.Deadline) {
			t.Fatalf("intent round trip: %v", err)
		}
		pub, failed := PublicationPublished, PublicationFailed
		code := PubErrPublicationTimeout
		settled := p
		settled.Phase, settled.Status = PubPhaseSettled, &pub
		if err := settled.Validate(); err != nil {
			t.Fatal(err)
		}
		settled.Status, settled.Error = &failed, &code
		if err := settled.Validate(); err != nil {
			t.Fatal(err)
		}
		for name, q := range map[string]TaskPublication{
			"id":             func() TaskPublication { q := p; q.ID = "x"; return q }(),
			"state":          func() TaskPublication { q := p; q.State = TaskLost; return q }(),
			"tree":           func() TaskPublication { q := p; q.Tree = "x"; return q }(),
			"phase":          func() TaskPublication { q := p; q.Phase = "x"; return q }(),
			"auth-status":    func() TaskPublication { q := p; q.Status = &pub; return q }(),
			"settled-none":   func() TaskPublication { q := p; q.Phase = PubPhaseSettled; return q }(),
			"failed-no-code": func() TaskPublication { q := p; q.Phase, q.Status = PubPhaseSettled, &failed; return q }(),
			"pub-with-code":  func() TaskPublication { q := p; q.Phase, q.Status, q.Error = PubPhaseSettled, &pub, &code; return q }(),
			"ws-candidate": func() TaskPublication {
				q := p
				w := NewNotApplicable(wsBinding())
				q.Candidate.Workspace = &w
				return q
			}(),
			"bad-candidate": func() TaskPublication { q := p; q.Candidate.Digest = strings.Repeat("0", 64); return q }(),
		} {
			wantInvalid(t, name, q.Validate())
		}
		wantInvalid(t, "deadline", back.unmarshalStrict(withField(t, enc, "deadline", `"later"`), "p"))
		wantInvalid(t, "strict", back.unmarshalStrict(withField(t, enc, "x", `1`), "p"))
	})
	t.Run("digest-and-message", func(t *testing.T) {
		zero := 0
		plain := TaskResultBody{TaskID: testTaskID, Execution: ExecutionToken{Epoch: wsInst, Attachment: 1}, Outcome: OutcomeNatural, ExitCode: &zero}
		w := wsPublished(wsBinding(), testTaskID)
		withWS := plain
		withWS.Workspace = &w
		if plain.ResultDigest() == withWS.ResultDigest() || withWS.Sealed().Validate() != nil {
			t.Fatal("the workspace DTO is not in the domain-separated digest")
		}
		enc, _ := Encode(plain.Sealed())
		if bytes.Contains(enc, []byte("workspace")) {
			t.Fatalf("a no-workspace result names a workspace: %s", enc)
		}
		_, err := DecodeTaskResult(withField(t, enc, "workspace", "null"))
		wantInvalid(t, "null workspace", err)
		w2 := w
		w2.Changes = nil
		w2.Diffstat = &TaskDiffstat{}
		other := plain
		other.Workspace = &w2
		if other.ResultDigest() == withWS.ResultDigest() {
			t.Fatal("the digest ignores the DTO's rows")
		}
		lost := plain
		lost.Outcome, lost.ExitCode = OutcomeLost, nil
		lost.Workspace = &w
		wantInvalid(t, "lost published", lost.Sealed().Validate())
		na := NewNotApplicable(wsBinding())
		lost.Workspace = &na
		if err := lost.Sealed().Validate(); err != nil {
			t.Fatal(err)
		}
		msg := TaskResultMessage(testTaskID, "worker-a", "example", TaskSucceeded)
		if msg != "Callsheet task result\ntask: "+testTaskID+"\nrole: worker-a\nmodel: example\nstate: succeeded\n" {
			t.Fatalf("message %q", msg)
		}
		for v, ok := range map[string]bool{"example": true, "gpt-5 high": true, "": false, "a\nb": false, "é": false, "\x7f": false} {
			if ValidMessageValue(v) != ok {
				t.Fatalf("ValidMessageValue(%q)", v)
			}
		}
	})
	t.Run("mirrors-and-phase", func(t *testing.T) {
		b := wsBinding()
		w := wsPublished(b, testTaskID)
		v := smallView()
		v.Request.Workspace, v.WorkspaceBinding = s(b.Name), &b
		res := v.Result.WithWorkspace(&w)
		v.Result = &res
		if *res.ResultCommit != *w.Commit || res.Diffstat.Added != 1 || len(res.ChangedPaths) != 1 || res.ChangedPathsTruncated || res.ChangedPathsNextAfter != nil {
			t.Fatalf("mirrors %+v", res)
		}
		roundTrip := func(name string, v TaskView) error {
			t.Helper()
			enc, err := Encode(DispatchResponse{Version: ProtocolVersion, TaskID: v.TaskID, Task: &v})
			if err != nil {
				return err
			}
			_, err = ParseDispatchResponse(enc)
			return err
		}
		if err := roundTrip("published", v); err != nil {
			t.Fatal(err)
		}
		tr := w
		BoundChanges(&tr, []ChangeRow{{Path: []byte("a"), Kind: "added", NewMode: s("100644"), NewBytes: i64(1)}, {Path: bytes.Repeat([]byte("b"), MaxWorkspaceDTOBytes), Kind: "added", NewMode: s("100644"), NewBytes: i64(1)}})
		res2 := v.Result.WithWorkspace(&tr)
		if !res2.ChangedPathsTruncated || res2.ChangedPathsNextAfter == nil || *res2.ChangedPathsNextAfter != EncodePath([]byte("a")) {
			t.Fatalf("truncation mirrors %+v", res2)
		}
		failed := NewPublicationFailed(b, PubErrStorageFailed)
		res3 := v.Result.WithWorkspace(&failed)
		if res3.ResultCommit != nil || res3.Diffstat != nil || len(res3.ChangedPaths) != 0 {
			t.Fatalf("failed mirrors %+v", res3)
		}
		bad := func(f func(v *TaskView)) TaskView {
			c := v
			r := *v.Result
			c.Result = &r
			f(&c)
			return c
		}
		for name, c := range map[string]TaskView{
			"commit-mirror":  bad(func(v *TaskView) { v.Result.ResultCommit = s(wsCommit) }),
			"paths-mirror":   bad(func(v *TaskView) { v.Result.ChangedPaths = []string{"Yg=="} }),
			"paths-count":    bad(func(v *TaskView) { v.Result.ChangedPaths = nil }),
			"truncated":      bad(func(v *TaskView) { v.Result.ChangedPathsTruncated = true }),
			"other-binding":  bad(func(v *TaskView) { o := b; o.Instance = strings.Repeat("9", 32); v.WorkspaceBinding = &o }),
			"no-binding":     bad(func(v *TaskView) { v.WorkspaceBinding = nil }),
			"terminal-phase": bad(func(v *TaskView) { p := WorkspacePhaseExecuting; v.WorkspacePhase = &p }),
			"legacy-mirror":  bad(func(v *TaskView) { v.Result.Workspace = nil }),
		} {
			if err := roundTrip(name, c); err == nil {
				t.Fatalf("%s accepted", name)
			}
		}
		// A nonterminal workspace task has its phase; the derivation.
		for want, c := range map[string]struct {
			state      string
			publishing bool
		}{WorkspacePhasePreparing: {TaskPending, false}, WorkspacePhaseExecuting: {TaskRunning, false}, WorkspacePhasePublishing: {TaskRunning, true}} {
			if p := WorkspacePhaseOf(&b, c.state, c.publishing); p == nil || *p != want {
				t.Fatalf("phase of %+v = %v", c, p)
			}
		}
		if WorkspacePhaseOf(nil, TaskRunning, false) != nil || WorkspacePhaseOf(&b, TaskSucceeded, false) != nil {
			t.Fatal("phase without workspace or when terminal")
		}
	})
	t.Run("assignment", func(t *testing.T) {
		a := NodeAssignment{NodeID: testID, Execution: ExecutionToken{Epoch: wsInst, Attachment: 7}, StartDigest: strings.Repeat("c", 64), Instance: wsInst}
		h := http.Header{}
		for k, v := range a.Headers() {
			h.Set(k, v)
		}
		got, err := ParseNodeAssignment(h.Values)
		if err != nil || got != a {
			t.Fatalf("assignment %+v %v", got, err)
		}
		for _, mut := range []func(h http.Header){
			func(h http.Header) { h.Del(NodeIDHeader) },
			func(h http.Header) { h.Add(NodeIDHeader, testID) },
			func(h http.Header) { h.Set(ExecutionEpochHeader, "x") },
			func(h http.Header) { h.Set(ExecutionAttachmentHeader, "07") },
			func(h http.Header) { h.Set(ExecutionAttachmentHeader, "0") },
			func(h http.Header) { h.Set(StartDigestHeader, "x") },
			func(h http.Header) { h.Set(WorkspaceInstanceHeader, "x") },
		} {
			c := h.Clone()
			mut(c)
			_, err := ParseNodeAssignment(c.Values)
			wantInvalid(t, "header", err)
		}
	})
	t.Run("paths", func(t *testing.T) {
		if NodeWorkspacePath(testTaskID) != "/api/v1/node-workspaces/"+testTaskID+".git/" {
			t.Fatal(NodeWorkspacePath(testTaskID))
		}
		base := PathTasks + "/" + testTaskID
		if PublicationPath(testTaskID, "", false) != base+PublicationSuffix || PublicationPath(testTaskID, wsPub, false) != base+PublicationSuffix+"/"+wsPub ||
			PublicationPath(testTaskID, wsPub, true) != base+PublicationSuffix+"/"+wsPub+FinishSuffix {
			t.Fatal("publication paths")
		}
		for rest, want := range map[string]struct {
			pub        string
			finish, ok bool
		}{
			PublicationSuffix:                           {"", false, true},
			PublicationSuffix + "/" + wsPub:             {wsPub, false, true},
			PublicationSuffix + "/" + wsPub + "/finish": {wsPub, true, true},
			PublicationSuffix + "/" + wsPub + "/":       {"", false, false},
			PublicationSuffix + "/x":                    {"", false, false},
			PublicationSuffix + "x":                     {"", false, false},
			PublicationSuffix + "/" + wsPub + "/other":  {"", false, false},
			"/logs": {"", false, false},
		} {
			pub, finish, ok := SplitPublicationPath(rest)
			if ok != want.ok || (ok && (pub != want.pub || finish != want.finish)) {
				t.Fatalf("%q = %q %v %v", rest, pub, finish, ok)
			}
		}
	})
	t.Run("record-schema-4", func(t *testing.T) {
		b := wsBinding()
		rec := ctlRecord(t)
		rec.Request, rec.Workspace = wsRequest(), &b
		got, err := roundTrip(t, rec)
		if err != nil || got.Workspace == nil || got.Schema != TaskRecordSchemaVersion {
			t.Fatalf("running workspace record: %+v %v", got.Workspace, err)
		}
		zero := 0
		cand := TaskResultBody{TaskID: ctlTask, Execution: ctlExec(), Outcome: OutcomeNatural, ExitCode: &zero}.Sealed()
		intent := TaskPublication{ID: wsPub, State: TaskSucceeded, Tree: wsTree, Commit: wsTree, Instance: wsInst, Deadline: time.Date(2026, 9, 27, 10, 5, 0, 0, time.UTC),
			Phase: PubPhaseAuthorized, Candidate: cand}
		pending := rec
		pending.Publication = &intent
		if _, err := roundTrip(t, pending); err != nil {
			t.Fatalf("authorized intent: %v", err)
		}
		done := pending
		fin := done.StartedAt.Add(time.Second)
		pubStatus := PublicationPublished
		settledIntent := intent
		settledIntent.Phase, settledIntent.Status = PubPhaseSettled, &pubStatus
		w := wsPublished(b, ctlTask)
		done.State, done.FinishedAt, done.ExitCode, done.Publication, done.WorkspaceResult = TaskSucceeded, &fin, &zero, &settledIntent, &w
		if _, err := roundTrip(t, done); err != nil {
			t.Fatalf("settled record: %v", err)
		}
		bad := func(f func(r *TaskRecord)) TaskRecord {
			r := done
			f(&r)
			return r
		}
		other := intent
		other.Instance = strings.Repeat("9", 32)
		wrongCand := intent
		wrongCand.Candidate.Execution.Attachment = 9
		wrongCand.Candidate = wrongCand.Candidate.Sealed()
		failedStatus := PublicationFailed
		code := PubErrStorageFailed
		failedIntent := settledIntent
		failedIntent.Status, failedIntent.Error = &failedStatus, &code
		pubNA := NewNotApplicable(b)
		for name, r := range map[string]TaskRecord{
			"no-binding":      bad(func(r *TaskRecord) { r.Workspace = nil; r.Request = validRequest() }),
			"other-instance":  bad(func(r *TaskRecord) { r.Publication = &other }),
			"terminal-auth":   bad(func(r *TaskRecord) { r.Publication = &intent }),
			"nonterm-settled": bad(func(r *TaskRecord) { r.State, r.FinishedAt, r.ExitCode, r.WorkspaceResult = TaskRunning, nil, nil, nil }),
			"other-candidate": bad(func(r *TaskRecord) {
				c := wrongCand
				c.Phase, c.Status = PubPhaseSettled, &pubStatus
				r.Publication = &c
			}),
			"no-result":         bad(func(r *TaskRecord) { r.WorkspaceResult = nil }),
			"published-failed":  bad(func(r *TaskRecord) { r.Publication = &failedIntent }),
			"failed-mismatch":   bad(func(r *TaskRecord) { f := NewPublicationFailed(b, code); r.WorkspaceResult = &f }),
			"published-no-pub":  bad(func(r *TaskRecord) { r.Publication = nil }),
			"result-other-base": bad(func(r *TaskRecord) { x := w; x.BaseCommit = nil; r.WorkspaceResult = &x }),
			"rejected-pub": bad(func(r *TaskRecord) {
				r.State, r.StartedAt, r.ExitCode, r.Publication = TaskRejected, nil, nil, nil
				r.Reason = &TaskReason{Code: ReasonWorkspaceCheckoutFailed, Message: "x"}
				f := NewNotStarted(b)
				r.WorkspaceResult = &f
			}),
		} {
			b, err := EncodeTaskRecord(r)
			if err == nil {
				_, err = ParseTaskRecord(b, testLookup)
			}
			if err == nil {
				t.Fatalf("%s accepted", name)
			}
		}
		rejected := bad(func(r *TaskRecord) {
			r.State, r.StartedAt, r.ExitCode, r.Publication, r.WorkspaceResult = TaskRejected, nil, nil, nil, &pubNA
			r.Reason = &TaskReason{Code: ReasonWorkspaceCheckoutFailed, Message: "x"}
		})
		if _, err := roundTrip(t, rejected); err != nil {
			t.Fatalf("rejected workspace record: %v", err)
		}
		// A schema-3 document (no workspace fields) still decodes.
		plain := ctlRecord(t)
		enc, _ := EncodeTaskRecord(plain)
		if r, err := ParseTaskRecord(asSchema3(t, enc), testLookup); err != nil || r.Schema != 3 {
			t.Fatalf("schema 3: %v", err)
		}
		_, err = ParseTaskRecord(withField(t, asSchema3(t, enc), "workspace_binding", "null"), testLookup)
		if err == nil {
			t.Fatal("a schema-3 record with schema-4 fields decoded")
		}
	})
	t.Run("journal-schema-3", func(t *testing.T) {
		b := wsBinding()
		rt := RuntimeDirPrefix + wsInst
		j := ExecutionJournal{TaskID: ctlTask, Execution: ctlExec(), StartDigest: ctlDigest, Role: validRole(),
			Effective: TaskEffective{Model: "m", Effort: "low", Timeout: time.Hour}, Phase: JournalPrepared, Work: true, Workspace: &b, RuntimeDir: &rt}
		enc, err := EncodeExecutionJournal(j)
		if err != nil {
			t.Fatal(err)
		}
		got, err := ParseExecutionJournal(enc, testLookup)
		if err != nil || got.Workspace == nil || got.RuntimeDir == nil || *got.RuntimeDir != rt || !got.Work || got.Schema != ExecutionJournalSchemaVersion {
			t.Fatalf("journal: %+v %v", got, err)
		}
		for name, body := range map[string][]byte{
			"runtime-name":      withField(t, enc, "runtime_dir", `".callsheet-runtime-x"`),
			"runtime-no-ws":     withField(t, enc, "workspace", "null"),
			"workspace-no-work": withField(t, enc, "work", "false"),
			"bad-binding":       withField(t, enc, "workspace", `{"name":"X"}`),
		} {
			_, err := ParseExecutionJournal(body, testLookup)
			wantInvalid(t, name, err)
		}
		for v, ok := range map[string]bool{rt: true, RuntimeDirPrefix: false, ".callsheet-runtime-" + strings.ToUpper(wsInst): false, "x" + rt: false} {
			if ValidRuntimeDir(v) != ok {
				t.Fatalf("ValidRuntimeDir(%q)", v)
			}
		}
	})
	t.Run("checkpoint", func(t *testing.T) {
		zero := 0
		exec := ExecutionToken{Epoch: wsInst, Attachment: 1}
		cand := TaskResultBody{TaskID: testTaskID, Execution: exec, Outcome: OutcomeNatural, ExitCode: &zero}.Sealed()
		sealed := PublicationCheckpoint{TaskID: testTaskID, Execution: exec, Phase: CheckpointSealed, Candidate: cand, Tree: wsTree}
		id, state, exp, commit := wsPub, TaskSucceeded, "2026-10-03T10:05:00Z", wsCommit
		auth := sealed
		auth.Phase, auth.PublicationID, auth.State, auth.ExpiresAt, auth.Commit, auth.Pushed = CheckpointAuthorized, &id, &state, &exp, &commit, true
		w := wsPublished(wsBinding(), testTaskID)
		res := cand
		res.Workspace = &w
		res = res.Sealed()
		settled := auth
		settled.Phase, settled.Result = CheckpointSettled, &res
		adopted := sealed
		adopted.Phase, adopted.Result = CheckpointSettled, &res
		for name, c := range map[string]PublicationCheckpoint{"sealed": sealed, "authorized": auth, "settled": settled, "adopted": adopted} {
			enc, err := EncodePublicationCheckpoint(c)
			if err != nil {
				t.Fatalf("%s: %v", name, err)
			}
			if !bytes.HasSuffix(enc, []byte("}\n")) || !bytes.Contains(enc, []byte(`"schema_version": 1`)) {
				t.Fatalf("%s form %s", name, enc)
			}
			back, err := ParsePublicationCheckpoint(enc)
			if err != nil || back.Phase != c.Phase || back.Pushed != c.Pushed {
				t.Fatalf("%s round trip: %v", name, err)
			}
		}
		for name, c := range map[string]PublicationCheckpoint{
			"task":            func() PublicationCheckpoint { c := sealed; c.TaskID = "x"; return c }(),
			"other-exec":      func() PublicationCheckpoint { c := sealed; c.Execution.Attachment = 2; return c }(),
			"tree":            func() PublicationCheckpoint { c := sealed; c.Tree = "x"; return c }(),
			"sealed-pushed":   func() PublicationCheckpoint { c := sealed; c.Pushed = true; return c }(),
			"sealed-intent":   func() PublicationCheckpoint { c := sealed; c.PublicationID = &id; return c }(),
			"auth-partial":    func() PublicationCheckpoint { c := auth; c.Commit = nil; return c }(),
			"auth-result":     func() PublicationCheckpoint { c := auth; c.Result = &res; return c }(),
			"settled-plain":   func() PublicationCheckpoint { c := settled; c.Result = &cand; return c }(),
			"settled-partial": func() PublicationCheckpoint { c := settled; c.State = nil; return c }(),
			"phase":           func() PublicationCheckpoint { c := sealed; c.Phase = "x"; return c }(),
			"bad-candidate":   func() PublicationCheckpoint { c := sealed; c.Candidate.Digest = strings.Repeat("0", 64); return c }(),
		} {
			if _, err := EncodePublicationCheckpoint(c); err == nil {
				t.Fatalf("%s encoded", name)
			}
		}
		enc, _ := EncodePublicationCheckpoint(sealed)
		for name, b := range map[string][]byte{
			"schema": withField(t, enc, "schema_version", "2"), "extra": withField(t, enc, "x", "1"), "big": bytes.Repeat([]byte(" "), MaxPublicationCheckpoint+1),
		} {
			_, err := ParsePublicationCheckpoint(b)
			wantInvalid(t, name, err)
		}
	})
}
