package reale2e

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/wedevwork/callsheet/internal/contract"
)

// The judge-level layer of the MCP binding closure tests (design
// 12a-dev-flow-verify r0.6, MCP binding closure tests, owner decision
// "two test layers"): every member of the independent field manifest is
// mutated against the production per-call judge (judgeMCPCall, the
// checker's own judge) with the captured authority it needs, starting
// from controls the judge accepts: a bound member's well-formed change is
// refused by its binding (request alone, response alone, and request
// plus response coherently with the authority unchanged), an excluded
// member's contract-valid change is accepted, and a contract literal or
// form member's change is refused by the contract.

// mcpControl is one accepted call of a form with the authority it is
// judged against.
type mcpControl struct {
	label        string
	auth         *mcpAuthority
	args, answer string
}

// formOf names a recorded call's form.
func formOf(tool string, args json.RawMessage) string {
	name := strings.TrimPrefix(tool, mcpToolPrefix)
	switch name {
	case "ws_status", "ws_diff", "ws_pull":
		var a struct {
			TaskID *string `json:"task_id"`
		}
		json.Unmarshal(args, &a)
		if a.TaskID != nil {
			return name + "/task"
		}
		return name + "/explicit"
	}
	return name
}

// loadAuthority loads a bundle and its call authority.
func loadAuthority(t *testing.T, dir string) (*DevBundle, *mcpAuthority) {
	t.Helper()
	b, err := LoadDevBundle(dir)
	if err != nil {
		t.Fatal(err)
	}
	return b, devMCPAuthority(b, b.Manifest, b.Manifest.StopCode != nil)
}

// cloneAuthority copies an authority's maps (controls may replace views).
func cloneAuthority(a *mcpAuthority) *mcpAuthority {
	c := *a
	c.views = map[string]contract.TaskView{}
	for k, v := range a.views {
		c.views[k] = v
	}
	c.owned = map[string]int{}
	for k, v := range a.owned {
		c.owned[k] = v
	}
	c.prepared = map[int]*devDispatchDoc{}
	for k, v := range a.prepared {
		c.prepared[k] = v
	}
	return &c
}

// encodeAnswer encodes a response as the server does.
func encodeAnswer(t *testing.T, v any) string {
	t.Helper()
	b, err := contract.Encode(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// mcpMatrixControls are the accepted controls of every form: the passing
// attempt's recorded calls (both workspace forms, first pages and
// continuations), the lost-reply recovery's listing, a cancellation of a
// captured terminal task, and synthetic task-view scenarios (a legitimate
// nonterminal view; a cancelled-before-start view with a late result; a
// rejected view with candidates) against an authority holding them.
func mcpMatrixControls(t *testing.T) map[string][]mcpControl {
	t.Helper()
	out := map[string][]mcpControl{}
	add := func(form string, c mcpControl) { out[form] = append(out[form], c) }
	dir := devPassingBundle(t)
	_, auth := loadAuthority(t, dir)
	for _, tool := range []string{toolDispatch, toolTaskShow, toolTaskLogs, toolWsPush, toolWsStatus, toolWsDiff, toolWsPull} {
		seen := map[string]int{}
		for _, c := range recordedCalls(t, dir, tool) {
			form := formOf(tool, c.args)
			kind := "first"
			if strings.Contains(string(c.args), `"after"`) {
				kind = "continuation"
			}
			if seen[form+kind] > 0 || c.answer == "" {
				continue
			}
			seen[form+kind]++
			add(form, mcpControl{label: "recorded " + kind, auth: auth, args: string(c.args), answer: c.answer})
		}
	}
	lostDir := devLostBundle(t)
	_, lostAuth := loadAuthority(t, lostDir)
	for _, c := range recordedCalls(t, lostDir, toolTaskList) {
		if c.answer != "" && !strings.Contains(string(c.args), `"after"`) {
			add("task_ls", mcpControl{label: "recovery page", auth: lostAuth, args: string(c.args), answer: c.answer})
			break
		}
	}
	// A cancellation answered for a captured durably terminal task.
	var done contract.TaskView
	ids := make([]string, 0, len(auth.views))
	for id := range auth.views {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		if v := auth.views[id]; contract.TaskTerminal(v.State) && v.DurabilityConfirmed && v.WorkspaceBinding != nil && done.TaskID == "" {
			done = v
		}
	}
	add("task_cancel", mcpControl{label: "terminal task", auth: auth, args: `{"id":"` + done.TaskID + `"}`,
		answer: encodeAnswer(t, contract.CancelResponse{Version: contract.ProtocolVersion, TaskID: done.TaskID, Task: done})})
	// Synthetic task-view scenarios, each against an authority holding
	// its own captured record, for the dispatch, task_show and
	// task_cancel forms.
	first := out["dispatch"][0]
	var dv contract.TaskView
	dv, _ = contract.ParseDispatchResponse([]byte(first.answer))
	running := auth.views[dv.TaskID]
	running.State, running.Result, running.FinishedAt, running.LogTail, running.TailTruncated = contract.TaskRunning, nil, nil, "", false
	started := cmpOr(deref(running.StartedAt), running.CreatedAt)
	running.StartedAt = &started
	phase := contract.WorkspacePhaseExecuting
	running.WorkspacePhase = &phase
	running.DurabilityConfirmed = false
	running.LogTail = "a\nb\n"
	running.Log = contract.TaskLogMeta{RetainedBytes: 4, SourceBytes: 4, ReceivedBytes: 4}
	cancelled := cancelledView(t, auth.views[dv.TaskID])
	rejected := rejectedView(t, auth.views[dv.TaskID])
	for _, sc := range []struct {
		label string
		view  contract.TaskView
		auth  contract.TaskView
	}{{"nonterminal view", running, auth.views[dv.TaskID]}, {"cancelled with late result", cancelled, cancelled}, {"rejected with candidates", rejected, rejected}} {
		a := cloneAuthority(auth)
		a.views[sc.view.TaskID] = sc.auth
		dispatchArgs := first.args
		if sc.view.WorkspaceBinding == nil {
			// A no-workspace prepared dispatch of the same ordinal.
			n := a.owned[sc.view.TaskID]
			doc := *a.prepared[n]
			doc.Workspace, doc.Base, doc.Instance = "", "", ""
			a.prepared[n] = &doc
			dispatchArgs = mustMarshal(t, dispatchInput(doc))
		}
		add("dispatch", mcpControl{label: sc.label, auth: a, args: dispatchArgs,
			answer: encodeAnswer(t, contract.DispatchResponse{Version: contract.ProtocolVersion, TaskID: sc.view.TaskID, Task: &sc.view})})
		add("task_show", mcpControl{label: sc.label, auth: a, args: `{"id":"` + sc.view.TaskID + `"}`,
			answer: encodeAnswer(t, contract.TaskShowResponse{Version: contract.ProtocolVersion, Task: sc.view})})
		if contract.TaskTerminal(sc.view.State) && sc.view.DurabilityConfirmed {
			add("task_cancel", mcpControl{label: sc.label, auth: a, args: `{"id":"` + sc.view.TaskID + `"}`,
				answer: encodeAnswer(t, contract.CancelResponse{Version: contract.ProtocolVersion, TaskID: sc.view.TaskID, Task: sc.view})})
		}
	}
	// A terminal dispatch answer, and an accepted cancellation of a
	// nonterminal task with its stop requested.
	{
		v := auth.views[dv.TaskID]
		add("dispatch", mcpControl{label: "terminal view", auth: auth, args: first.args,
			answer: encodeAnswer(t, contract.DispatchResponse{Version: contract.ProtocolVersion, TaskID: v.TaskID, Task: &v})})
		r := running
		r.StopRequested = true
		add("task_cancel", mcpControl{label: "nonterminal", auth: auth, args: `{"id":"` + r.TaskID + `"}`,
			answer: encodeAnswer(t, contract.CancelResponse{Version: contract.ProtocolVersion, TaskID: r.TaskID, Accepted: true, Task: r})})
	}
	// Listing pages of a nonterminal and a rejected task.
	for _, sc := range []struct {
		label string
		view  contract.TaskView
		auth  contract.TaskView
	}{{"nonterminal page", running, auth.views[dv.TaskID]}, {"rejected page", rejected, rejected}} {
		a := cloneAuthority(auth)
		a.views[sc.view.TaskID] = sc.auth
		v := sc.view
		page := contract.TaskListResponse{Version: contract.ProtocolVersion, Tasks: []contract.TaskSummary{{TaskID: v.TaskID, Target: v.Request.Target,
			Role: v.Role, State: v.State, StopRequested: v.StopRequested, CreatedAt: v.CreatedAt, StartedAt: v.StartedAt, FinishedAt: v.FinishedAt,
			ElapsedMS: v.ElapsedMS, Effective: v.Effective, RequestedBy: v.Request.RequestedBy, Reconciling: v.Reconciling, CompletionPending: v.CompletionPending,
			PersistenceReason: v.PersistenceReason, DurabilityConfirmed: v.DurabilityConfirmed, Reason: v.Reason}}}
		add("task_ls", mcpControl{label: sc.label, auth: a, args: `{}`, answer: encodeAnswer(t, page)})
	}
	// Task workspace status of a nonterminal task and of a terminal task
	// whose result holds change rows.
	for _, sc := range []struct {
		label string
		view  contract.TaskView
		auth  contract.TaskView
	}{{"nonterminal", running, auth.views[dv.TaskID]}, {"cancelled with rows", cancelled, cancelled}} {
		a := cloneAuthority(auth)
		a.views[sc.view.TaskID] = sc.auth
		v := sc.view
		st := contract.TaskWorkspaceStatus{TaskID: v.TaskID, State: v.State, WorkspacePhase: v.WorkspacePhase, Binding: *v.WorkspaceBinding}
		if v.Result != nil {
			st.Result = v.Result.Workspace
			st.Available = v.Result.Workspace.Publication == contract.PublicationPublished
		}
		add("ws_status/task", mcpControl{label: sc.label, auth: a, args: `{"task_id":"` + v.TaskID + `"}`,
			answer: encodeAnswer(t, contract.TaskWorkspaceStatusResponse{Version: contract.ProtocolVersion, Workspace: st})})
	}
	// Continuation pages of the explicit forms (the recorded explicit
	// reads were single pages): the rest after the first row or ref.
	if c := out["ws_status/explicit"]; len(c) > 0 {
		var r contract.WorkspaceStatusResponse
		json.Unmarshal([]byte(c[0].answer), &r)
		if len(r.Refs) >= 2 {
			args := fmt.Sprintf(`{"name":%q,"after":%q,"instance":%q,"generation":%q}`, r.Name, r.Refs[0].Name, r.Instance, r.Generation)
			r.Refs = r.Refs[1:]
			add("ws_status/explicit", mcpControl{label: "synthetic continuation", auth: c[0].auth, args: args, answer: encodeAnswer(t, r)})
		}
	}
	if c := out["ws_diff/explicit"]; len(c) > 0 {
		var r contract.WorkspaceDiffResponse
		json.Unmarshal([]byte(c[0].answer), &r)
		var a map[string]any
		json.Unmarshal([]byte(c[0].args), &a)
		if len(r.Changes) >= 2 {
			// The first page at limit 1, and its continuation.
			b := map[string]any{}
			for k, v := range a {
				b[k] = v
			}
			b["limit"] = 1
			p := r
			next := r.Changes[0].PathBase64
			p.Changes, p.NextAfter = r.Changes[:1], &next
			add("ws_diff/explicit", mcpControl{label: "synthetic first", auth: c[0].auth, args: mustMarshal(t, b), answer: encodeAnswer(t, p)})
			a["after"], a["instance"], a["generation"], a["limit"] = r.Changes[0].PathBase64, r.Instance, r.Generation, len(r.Changes)-1
			r.Changes, r.NextAfter = r.Changes[1:], nil
			add("ws_diff/explicit", mcpControl{label: "synthetic continuation", auth: c[0].auth, args: mustMarshal(t, a), answer: encodeAnswer(t, r)})
		}
	}
	return out
}

// mustMarshal is v's JSON.
func mustMarshal(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// cancelledView is v cancelled before start, with a reason and a late
// result (every excluded branch present).
func cancelledView(t *testing.T, v contract.TaskView) contract.TaskView {
	t.Helper()
	c := v
	c.State, c.StartedAt, c.WorkspacePhase = contract.TaskCancelled, nil, nil
	c.Reason = &contract.TaskReason{Code: contract.ReasonCancelledBeforeStart, Message: "cancelled before start"}
	r := *v.Result
	r.State, r.ExitCode, r.Signal, r.FinalMessage, r.FinalMessageTruncated = contract.TaskCancelled, nil, nil, nil, false
	if r.Workspace != nil {
		w := *r.Workspace
		mode, n := "100644", int64(1)
		w.Changes = []contract.WorkspaceChange{{PathBase64: "YQ==", Kind: contract.ChangeAdded, NewMode: &mode, NewBytes: &n}}
		ds := contract.TaskDiffstat{Added: 1, NewBytes: 1}
		w.Diffstat = &ds
		r = r.WithWorkspace(&w)
	}
	c.Result = &r
	code := 0
	msg := "late"
	c.LateResult = &contract.TaskLateSummary{Digest: strings.Repeat("a", 64), ReceivedAt: v.CreatedAt, Outcome: "natural", ExitCode: &code,
		FinalMessage: &msg, OutputBytes: 4, Log: contract.TaskLogMeta{RetainedBytes: 4, SourceBytes: 4, ReceivedBytes: 4}}
	if b, err := contract.Encode(contract.TaskShowResponse{Version: contract.ProtocolVersion, Task: c}); err != nil {
		t.Fatal(err)
	} else if _, err := contract.ParseTaskShowResponse(b); err != nil {
		t.Fatalf("cancelled scenario: %v", err)
	}
	return c
}

// rejectedView is v's identity without a workspace, rejected with a
// reason and a candidate.
func rejectedView(t *testing.T, v contract.TaskView) contract.TaskView {
	t.Helper()
	c := v
	c.Request.Workspace, c.Request.Base, c.Request.WorkspaceInstance, c.WorkspaceBinding, c.WorkspacePhase = nil, nil, nil, nil, nil
	c.State, c.StartedAt, c.DurabilityConfirmed = contract.TaskRejected, nil, true
	c.Reason = &contract.TaskReason{Code: contract.ReasonStartNotSent, Message: "no candidate"}
	c.Candidates = []contract.TaskCandidate{{RoleID: v.Role.ID, NodeID: v.Role.Node, RegistrationOrder: v.Role.RegistrationOrder, NodeLiveness: contract.LivenessOnline,
		Inflight: 1, Concurrency: 1, CanAccept: false, Reason: contract.ReasonFull}}
	c.LogTail, c.TailTruncated, c.Log = "", false, contract.TaskLogMeta{}
	c.Result = &contract.TaskResult{State: contract.TaskRejected, ChangedPaths: []string{}}
	if b, err := contract.Encode(contract.TaskShowResponse{Version: contract.ProtocolVersion, Task: c}); err != nil {
		t.Fatal(err)
	} else if _, err := contract.ParseTaskShowResponse(b); err != nil {
		t.Fatalf("rejected scenario: %v", err)
	}
	return c
}

// ---- JSON trees ----

// splitPath splits a manifest path into segments ("x[]" an array's
// first element).
func splitPath(path string) []string { return strings.Split(path, ".") }

// jsonTree decodes JSON.
func jsonTree(s string) any {
	var v any
	if err := json.Unmarshal([]byte(s), &v); err != nil {
		panic(err)
	}
	return v
}

// getAt reads the member at path (the first element of an array).
func getAt(root any, path string) (any, bool) {
	cur := root
	for _, seg := range splitPath(path) {
		key, arr := strings.CutSuffix(seg, "[]")
		m, ok := cur.(map[string]any)
		if !ok {
			return nil, false
		}
		if cur, ok = m[key]; !ok {
			return nil, false
		}
		if arr {
			a, ok := cur.([]any)
			if !ok || len(a) == 0 {
				return nil, false
			}
			cur = a[0]
		}
	}
	return cur, true
}

// setAt writes the member at path, creating an absent object member or
// first array element on the way (a presence change).
func setAt(root any, path string, v any) bool {
	segs := splitPath(path)
	cur := root
	for i, seg := range segs {
		key, arr := strings.CutSuffix(seg, "[]")
		m, ok := cur.(map[string]any)
		if !ok {
			return false
		}
		last := i == len(segs)-1
		if !arr {
			if last {
				m[key] = v
				return true
			}
			next, ok := m[key].(map[string]any)
			if !ok {
				next = map[string]any{}
				m[key] = next
			}
			cur = next
			continue
		}
		a, _ := m[key].([]any)
		if last {
			if len(a) == 0 {
				a = []any{v}
			} else {
				a[0] = v
			}
			m[key] = a
			return true
		}
		if len(a) == 0 {
			a = []any{map[string]any{}}
			m[key] = a
		}
		cur = a[0]
	}
	return false
}

// encodeTree encodes a tree.
func encodeTree(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}

// ---- Well-formed alternatives ----

var (
	hexRE = map[int]bool{32: true, 40: true, 64: true}
)

// flipHex changes a hex string's last digit.
func flipHex(s string) string {
	last := s[len(s)-1]
	r := byte('0')
	if last == '0' {
		r = '1'
	}
	return s[:len(s)-1] + string(r)
}

// isHex reports lowercase hex.
func isHex(s string) bool {
	for i := 0; i < len(s); i++ {
		if !(s[i] >= '0' && s[i] <= '9' || s[i] >= 'a' && s[i] <= 'f') {
			return false
		}
	}
	return s != ""
}

// enumAlternatives are other members of the contract enumerations a
// member may hold.
var enumAlternatives = map[string]string{
	contract.TaskSucceeded: contract.TaskFailed, contract.TaskRunning: contract.TaskPending, contract.TaskPending: contract.TaskRunning,
	"id": "name", "name": "id", contract.ChangeAdded: contract.ChangeModified, contract.ChangeModified: contract.ChangeAdded,
	contract.ChangeDeleted: contract.ChangeModified, contract.TimeoutPolicyEnforced: contract.TimeoutPolicyLegacy,
	contract.TimeoutPolicyLegacy: contract.TimeoutPolicyEnforced, contract.KindFolder: contract.KindGit, contract.KindGit: contract.KindFolder,
	contract.WorkspacePhaseExecuting: contract.WorkspacePhasePublishing, contract.LivenessOnline: contract.LivenessOffline,
	contract.ReasonFull: contract.ReasonNodeOffline, contract.LivenessOffline: contract.LivenessOnline,
	contract.WorkspacePhasePublishing: contract.WorkspacePhaseExecuting, contract.PublicationPublished: contract.PublicationFailed,
	contract.TaskCancelled: contract.TaskTimedOut, contract.TaskRejected: contract.TaskLost, contract.TaskFailed: contract.TaskSucceeded,
}

// altValue is a well-formed alternative of a member's value, by its
// grammar: another task, node, hash or token; a later timestamp; a longer
// duration; another enumeration member; other text, number or flag.
func altValue(path string, v any) (any, bool) {
	leaf := path[strings.LastIndex(path, ".")+1:]
	switch x := v.(type) {
	case bool:
		return !x, true
	case float64:
		return x + 1, true
	case string:
		switch {
		case strings.HasPrefix(x, "t_") && len(x) == 34 && isHex(x[2:]),
			strings.HasPrefix(x, "n_") && len(x) == 34 && isHex(x[2:]):
			return flipHex(x), true
		case hexRE[len(x)] && isHex(x):
			return flipHex(x), true
		case strings.HasPrefix(x, contract.TaskRefPrefix):
			return flipHex(x), true
		case strings.HasPrefix(x, "refs/heads/"):
			return x + "-other", true
		case strings.HasSuffix(leaf, "path_base64") || leaf == "next_after" && strings.Contains(path, "changes") ||
			leaf == "next_after" && !strings.Contains(path, "tasks") && strings.HasSuffix(x, "="):
			b, err := base64.StdEncoding.DecodeString(x)
			if err != nil {
				return nil, false
			}
			return base64.StdEncoding.EncodeToString(append(b, 'x')), true
		}
		if ts, err := time.Parse(time.RFC3339Nano, x); err == nil {
			return ts.Add(time.Second).UTC().Format(time.RFC3339Nano), true
		}
		if d, err := time.ParseDuration(x); err == nil && strings.HasSuffix(leaf, "timeout") {
			return (d + time.Minute).String(), true
		}
		if alt, ok := enumAlternatives[x]; ok {
			return alt, true
		}
		return x + "x", true
	case nil:
		return nil, false
	}
	return nil, false
}

// ---- The matrix ----

// mcpOutcomeOf classifies one judgment: nil (accepted), a binding or
// policy refusal, or a contract refusal.
func mcpOutcomeOf(err error) string {
	var be *mcpBindingError
	var pe *mcpPolicyError
	switch {
	case err == nil:
		return expectAccept
	case errors.As(err, &be), errors.As(err, &pe):
		return expectBinding
	}
	return expectContract
}

// judgeTree judges a control with mutated request and response trees.
func judgeTree(c mcpControl, tool string, args, answer any) error {
	_, err := judgeMCPCall(c.auth, tool, json.RawMessage(encodeTree(args)), encodeTree(answer))
	return err
}

// parsesAs reports that a response still strictly parses as its tool's
// success answer for args (a well-formed mutation).
func parsesAs(c mcpControl, tool string, args, answer any) bool {
	name := strings.TrimPrefix(tool, mcpToolPrefix)
	return mcpTools[name].success(c.auth, []byte(encodeTree(args)), []byte(encodeTree(answer)))
}

// matrixCase is one mutation of a manifest member.
type matrixCase struct {
	form, key, mode string
	control         string
	got, want       string
	err             error
}

func TestDevFullMCPBindingMatrix(t *testing.T) {
	t.Parallel()
	controls := mcpMatrixControls(t)
	var cases []matrixCase
	var uncovered []string
	for _, f := range mcpForms() {
		ctls := controls[f.name]
		if len(ctls) == 0 {
			t.Errorf("form %s has no control", f.name)
			continue
		}
		// Every control is accepted before it is mutated.
		for _, c := range ctls {
			if _, err := judgeMCPCall(c.auth, f.tool, json.RawMessage(c.args), c.answer); err != nil {
				t.Fatalf("%s control %q refused: %v", f.name, c.label, err)
			}
		}
		keys := make([]string, 0, len(f.fields))
		for k := range f.fields {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		reqOf := map[string]string{}
		for resp, req := range f.pairs {
			reqOf[req] = resp
		}
		for _, key := range keys {
			got := matrixMember(t, f, ctls, key, reqOf)
			if got == nil {
				uncovered = append(uncovered, f.name+" "+key)
				continue
			}
			cases = append(cases, got...)
		}
	}
	if len(uncovered) > 0 {
		t.Errorf("%d members have no well-formed mutation in any control:\n%s", len(uncovered), strings.Join(uncovered, "\n"))
	}
	bad := 0
	for _, c := range cases {
		if c.got != c.want {
			bad++
			t.Errorf("%s %s (%s, control %q): got %s, want %s: %v", c.form, c.key, c.mode, c.control, c.got, c.want, c.err)
		}
	}
	t.Logf("%d member mutations, %d failing", len(cases), bad)
	t.Run("missing_authority", func(t *testing.T) { judgeMissingAuthority(t, controls) })
	t.Run("canonical_controls", func(t *testing.T) { judgeCanonicalControls(t, controls) })
	t.Run("regressions", func(t *testing.T) { judgeRegressions(t, controls) })
	t.Run("diff_pages", judgeDiffPages)
	t.Run("call_policy", func(t *testing.T) { judgeCallPolicy(t, controls) })
}

// matrixPrefer names the control a member's mutation must use (one that
// exercises its constraint: a non-empty tail, a continuation page).
var matrixPrefer = map[string]string{
	"task_show req:lines":                           "nonterminal view",
	"ws_status/explicit req:after":                  "continuation",
	"ws_status/explicit req:instance":               "continuation",
	"ws_status/explicit req:generation":             "continuation",
	"ws_status/explicit resp:generation":            "continuation",
	"ws_diff/task req:after":                        "continuation",
	"ws_diff/task req:instance":                     "continuation",
	"ws_diff/task req:generation":                   "continuation",
	"ws_diff/task resp:generation":                  "continuation",
	"ws_diff/task resp:next_after":                  "continuation",
	"ws_diff/explicit req:after":                    "continuation",
	"ws_diff/explicit req:instance":                 "continuation",
	"ws_diff/explicit req:generation":               "continuation",
	"ws_diff/explicit resp:generation":              "continuation",
	"task_ls resp:tasks[].reconciling":              "nonterminal page",
	"task_ls resp:tasks[].stop_requested":           "nonterminal page",
	"task_ls resp:tasks[].reason":                   "rejected page",
	"task_ls resp:tasks[].reason.code":              "rejected page",
	"task_ls resp:tasks[].reason.message":           "rejected page",
	"ws_status/task resp:workspace.workspace_phase": "nonterminal",
	"dispatch resp:task.log_tail":                   "nonterminal",
	"task_cancel resp:task.log_tail":                "nonterminal",
	"task_cancel resp:task.reconciling":             "nonterminal",
	"task_cancel resp:task.workspace_phase":         "nonterminal",
	"ws_diff/task req:limit":                        "recorded first",
	"ws_diff/explicit req:limit":                    "synthetic first",
}

// matrixMember runs one manifest member's mutations (nil: no control
// holds a well-formed mutation of it). A container is exercised through
// a member below it with the same outcome.
func matrixMember(t *testing.T, f mcpForm, ctls []mcpControl, key string, reqOf map[string]string) []matrixCase {
	t.Helper()
	spec := f.fields[key]
	side, path, _ := strings.Cut(key, ":")
	if pref, ok := matrixPrefer[f.name+" "+key]; ok {
		var only []mcpControl
		for _, c := range ctls {
			if strings.Contains(c.label, pref) {
				only = append(only, c)
			}
		}
		ctls = only
	}
	for _, c := range ctls {
		if got := runMember(t, f, c, side, path, spec, reqOf); got != nil {
			return got
		}
	}
	// A container: a member below it with the same expected outcome.
	var children []string
	for k, sp := range f.fields {
		if (strings.HasPrefix(k, key+".") || strings.HasPrefix(k, key+"[]")) && sp.expect == spec.expect {
			children = append(children, k)
		}
	}
	sort.Strings(children)
	for _, child := range children {
		_, cpath, _ := strings.Cut(child, ":")
		for _, c := range ctls {
			if got := runMember(t, f, c, side, cpath, f.fields[child], reqOf); got != nil {
				for i := range got {
					got[i].key = key + " via " + got[i].key
				}
				return got
			}
		}
	}
	return nil
}

// runMember mutates one manifest member in control c and returns its
// cases (nil: this control holds no well-formed mutation of it).
func runMember(t *testing.T, f mcpForm, c mcpControl, side, path string, spec fieldSpec, reqOf map[string]string) []matrixCase {
	t.Helper()
	args, answer := jsonTree(c.args), jsonTree(c.answer)
	root := answer
	if side == "req" {
		root = args
	}
	cur, present := getAt(root, path)
	if _, isMap := cur.(map[string]any); isMap {
		// A container's own mutation is one of its members'.
		return nil
	}
	alt, ok := customAlt(f.name, side+":"+path, c, cur, present)
	if !ok {
		if !present {
			return nil
		}
		if alt, ok = altValue(path, cur); !ok {
			return nil
		}
	}
	mutated := func(tree any) any {
		cp := jsonTree(encodeTree(tree))
		if !setAt(cp, path, alt) {
			return nil
		}
		if side == "resp" {
			if m, ok := cp.(map[string]any); ok {
				repairResponse(f.name, m, path)
				if strings.HasPrefix(f.name, "ws_diff") && strings.HasPrefix(path, "changes[]") {
					repairWsResult(m, path)
					if changes, _ := m["changes"].([]any); m["next_after"] != nil && len(changes) > 0 {
						m["next_after"] = changes[len(changes)-1].(map[string]any)["path_base64"]
					}
				}
			}
		}
		return cp
	}
	var out []matrixCase
	record := func(mode string, a, r any, want string) {
		err := judgeTree(c, f.tool, a, r)
		got := mcpOutcomeOf(err)
		out = append(out, matrixCase{form: f.name, key: side + ":" + path, mode: mode, control: c.label, got: got, want: want, err: err})
	}
	switch {
	case side == "resp":
		r := mutated(answer)
		if r == nil {
			return nil
		}
		switch spec.expect {
		case expectAccept:
			if !parsesAs(c, f.tool, args, r) {
				return nil
			}
			record("response", args, r, expectAccept)
		case expectContract:
			record("response", args, r, expectContract)
		case expectBinding:
			want := expectBinding
			if f.parserBound[side+":"+path] {
				want = expectContract
			} else if !parsesAs(c, f.tool, args, r) {
				// A response well formed for the request it names (its
				// strict parse takes the requested ID).
				req, paired := f.pairs["resp:"+path]
				if !paired {
					return nil
				}
				if a := coherentArgs(f.name, req, args, alt); a == nil || !parsesAs(c, f.tool, a, r) {
					return nil
				}
			}
			record("response", args, r, want)
			if req, ok := f.pairs["resp:"+path]; ok {
				if a := coherentArgs(f.name, req, args, alt); a != nil {
					record("request+response", a, r, expectBinding)
				}
			}
		}
	default:
		a := mutated(args)
		if a == nil {
			return nil
		}
		want := spec.expect
		if f.parserBound[side+":"+path] {
			want = expectContract
		}
		record("request", a, answer, want)
	}
	return out
}

// coherentArgs mutates the request member bound to a mutated response
// member to the same value (the canonical forms translated).
func coherentArgs(form, reqKey string, args any, alt any) any {
	_, path, _ := strings.Cut(reqKey, ":")
	cp := jsonTree(encodeTree(args))
	v := alt
	if s, ok := alt.(string); ok {
		switch {
		case form == "ws_push" && path == "branch":
			v = strings.TrimPrefix(s, "refs/heads/")
		case form == "ws_pull/task" && path == "task_id":
			v = strings.TrimPrefix(s, contract.TaskRefPrefix)
		}
	}
	if !setAt(cp, path, v) {
		return nil
	}
	return cp
}

// customAlt supplies the well-formed alternative of members whose value
// has no generic alternative or whose change must stay coherent with
// another member for the contract (ok false: use the generic one).
func customAlt(form, key string, c mcpControl, cur any, present bool) (any, bool) {
	switch key {
	case "req:wait":
		return "30s", true
	case "req:payload", "req:payload[]":
		if key == "req:payload" {
			return []any{"extra"}, true
		}
		return "extra", true
	case "req:override":
		return map[string]any{"model": "other"}, true
	case "req:override.model", "req:override.effort":
		return "other", true
	case "req:override.timeout":
		return "1h0m0s", true
	case "req:workspace", "req:workspace_instance", "req:base":
		if !present && form == "dispatch" {
			return nil, false
		}
	case "req:lines":
		return float64(0), true
	case "req:late":
		return true, true
	case "req:limit":
		switch {
		case form == "ws_status/explicit", form == "task_ls":
			return float64(1), true
		case present:
			return cur.(float64) + 1, true
		}
		return float64(1), true
	case "req:after":
		// A cursor the page's first row is not after.
		ans, _ := jsonTree(c.answer).(map[string]any)
		switch {
		case strings.HasPrefix(form, "ws_diff"):
			if ch, _ := ans["changes"].([]any); len(ch) > 0 {
				return ch[0].(map[string]any)["path_base64"], true
			}
		case form == "ws_status/explicit":
			if refs, _ := ans["refs"].([]any); len(refs) > 0 {
				return refs[0].(map[string]any)["name"], true
			}
		}
	}
	_, kpath, _ := strings.Cut(key, ":")
	leaf := kpath[strings.LastIndex(kpath, ".")+1:]
	switch {
	case key == "resp:wait_result":
		return map[string]any{}, true
	case strings.HasSuffix(key, "request.override") && !present:
		return map[string]any{"model": "other"}, true
	case strings.HasSuffix(key, "request.override.model"), strings.HasSuffix(key, "request.override.effort"):
		if !present {
			return "other", true
		}
	case strings.HasSuffix(key, "request.override.timeout") && !present:
		return "1h0m0s", true
	case strings.HasSuffix(key, "request.payload[]") && !present:
		return "extra", true
	case strings.HasSuffix(key, "request.payload"):
		return []any{"extra"}, true
	case leaf == "persistence_reason":
		if cur == nil {
			return contract.ReasonResultStorageUnconfirmed, true
		}
		return nil, true
	case leaf == "data" && form == "task_logs":
		b, _ := base64.StdEncoding.DecodeString(cur.(string))
		b = append(b, 'y')
		return base64.StdEncoding.EncodeToString(b), true
	case strings.HasSuffix(key, "changed_paths[]") && present:
		b, _ := base64.StdEncoding.DecodeString(cur.(string))
		return base64.StdEncoding.EncodeToString(append(b, 'x')), true
	case leaf == "code" && strings.Contains(key, "reason") && cur == contract.ReasonStartNotSent:
		return contract.ReasonRoleMissing, true
	case leaf == "outcome" && cur == "natural":
		return "lost", true
	case (leaf == "signal" || leaf == "exit_code") && present && cur == nil:
		if leaf == "signal" {
			return "SIGKILL", true
		}
		return float64(0), true
	case (leaf == "old_commit" || leaf == "base_commit" || leaf == "commit") && present && cur == nil:
		return strings.Repeat("d", 40), true
	case (leaf == "next_after" || leaf == "changed_paths_next_after") && present && cur == nil:
		// The last row's path (a page claiming more rows).
		answer, _ := jsonTree(c.answer).(map[string]any)
		parent := kpath[:max(0, strings.LastIndex(kpath, "."))]
		holder := answer
		if parent != "" {
			holder, _ = valueAt(answer, parent).(map[string]any)
		}
		if holder == nil {
			return nil, false
		}
		var rows []any
		switch {
		case leaf == "changed_paths_next_after":
			rows, _ = holder["changed_paths"].([]any)
			if len(rows) > 0 {
				return rows[len(rows)-1], true
			}
		case holder["changes"] != nil:
			rows, _ = holder["changes"].([]any)
			if len(rows) > 0 {
				return rows[len(rows)-1].(map[string]any)["path_base64"], true
			}
		case holder["refs"] != nil:
			rows, _ = holder["refs"].([]any)
			if len(rows) > 0 {
				return rows[len(rows)-1].(map[string]any)["name"], true
			}
		case holder["tasks"] != nil:
			rows, _ = holder["tasks"].([]any)
			if len(rows) > 0 {
				return rows[len(rows)-1].(map[string]any)["task_id"], true
			}
		}
	case leaf == "error" && present && cur == nil:
		return contract.PubErrSnapshotFailed, true
	case (leaf == "new_mode" || leaf == "old_mode") && present:
		if cur == nil {
			return "100644", true
		}
		return "100755", true
	case (leaf == "new_bytes" || leaf == "old_bytes") && present && cur == nil:
		return float64(1), true
	case leaf == "local_ref" && cur == nil:
		ans, _ := jsonTree(c.answer).(map[string]any)
		return contract.LocalRefFor(ans["name"].(string), ans["selector"].(string)), true
	case key == "req:after" && form == "task_ls":
		// A cursor at or after the page's last entry.
		if tasks, _ := jsonTree(c.answer).(map[string]any)["tasks"].([]any); len(tasks) > 0 {
			return tasks[len(tasks)-1].(map[string]any)["task_id"], true
		}
	}
	if strings.HasPrefix(key, "req:") && !present && form != "dispatch" {
		// A member of another form: any well-formed value.
		switch strings.TrimPrefix(key, "req:") {
		case "name":
			return "w", true
		case "base", "target", "ref":
			return "main", true
		case "after":
			return "YQ==", true
		case "instance", "generation":
			return strings.Repeat("a", 32), true
		case "limit":
			return float64(1), true
		case "task_id":
			return "t_" + strings.Repeat("0", 32), true
		}
	}
	return nil, false
}

// TestDevFullMCPFieldCoverage requires the manifest to classify exactly
// the field universe of every permitted form, each member once with a
// known rule, and the explicit wire shapes to be the custom marshalers'
// actual members.
func TestDevFullMCPFieldCoverage(t *testing.T) {
	t.Parallel()
	forms := mcpForms()
	wantForms := []string{"dispatch", "task_ls", "task_show", "task_logs", "task_cancel", "ws_push", "ws_status/task", "ws_status/explicit",
		"ws_diff/task", "ws_diff/explicit", "ws_pull/task", "ws_pull/explicit"}
	var names []string
	tools := map[string]bool{}
	for _, f := range forms {
		names = append(names, f.name)
		tools[strings.TrimPrefix(f.tool, mcpToolPrefix)] = true
	}
	if !slices.Equal(names, wantForms) {
		t.Fatalf("forms %v", names)
	}
	for _, n := range mcpPermittedTools {
		if !tools[n] {
			t.Errorf("permitted tool %s has no form", n)
		}
	}
	for _, f := range forms {
		universe, unshaped := map[string]bool{}, map[string]bool{}
		req, resp := map[string]bool{}, map[string]bool{}
		wireLeaves(f.req, "", req, unshaped)
		wireLeaves(f.resp, "", resp, unshaped)
		for k := range req {
			universe["req:"+k] = true
		}
		for k := range resp {
			universe["resp:"+k] = true
		}
		if len(unshaped) > 0 {
			t.Errorf("%s: custom wire shapes without an explicit shape: %v", f.name, unshaped)
		}
		for k := range universe {
			if _, ok := f.fields[k]; !ok {
				t.Errorf("%s: member %s is not classified", f.name, k)
			}
		}
		for k, spec := range f.fields {
			if !universe[k] {
				t.Errorf("%s: classified member %s is not in the contract", f.name, k)
			}
			switch {
			case spec.why == "":
				t.Errorf("%s %s: no reason", f.name, k)
			case spec.rule == ruleContract && spec.expect == expectBinding,
				spec.rule != ruleContract && spec.expect != expectBinding,
				!slices.Contains([]string{ruleContract, ruleRequest, ruleAuthority, ruleDerived}, spec.rule):
				t.Errorf("%s %s: rule %s with outcome %s", f.name, k, spec.rule, spec.expect)
			}
		}
		for resp, req := range f.pairs {
			if _, ok := f.fields[resp]; !ok {
				t.Errorf("%s: pair %s is not classified", f.name, resp)
			}
			if _, ok := f.fields[req]; !ok {
				t.Errorf("%s: pair %s is not classified", f.name, req)
			}
		}
	}
	// The explicit wire shapes are the encoded members.
	s, ws, tk := "x", "w", strings.Repeat("a", 32)
	d := time.Hour
	samples := map[reflect.Type]any{
		reflect.TypeOf(contract.DispatchRequest{}):             contract.DispatchRequest{Override: &contract.TaskOverride{Model: &s}, Workspace: &ws, Base: &s, WorkspaceInstance: &tk},
		reflect.TypeOf(contract.TaskOverride{}):                contract.TaskOverride{Model: &s, Effort: &s, Timeout: &d},
		reflect.TypeOf(contract.TaskEffective{}):               contract.TaskEffective{},
		reflect.TypeOf(contract.TaskLogsResponse{}):            contract.TaskLogsResponse{},
		reflect.TypeOf(contract.TaskWorkspaceStatusResponse{}): contract.TaskWorkspaceStatusResponse{},
		reflect.TypeOf(contract.DispatchResponse{}):            contract.DispatchResponse{Task: &contract.TaskView{}},
		reflect.TypeOf(contract.TaskView{}):                    contract.TaskView{},
		reflect.TypeOf(contract.TaskResult{}):                  contract.TaskResult{},
		reflect.TypeOf(contract.TaskWorkspaceResult{}):         contract.TaskWorkspaceResult{},
		reflect.TypeOf(contract.TaskListResponse{}):            contract.TaskListResponse{},
	}
	for typ, v := range samples {
		b, err := json.Marshal(v)
		if err != nil {
			t.Fatalf("%v: %v", typ, err)
		}
		var m map[string]json.RawMessage
		json.Unmarshal(b, &m)
		var got, want []string
		for k := range m {
			got = append(got, k)
		}
		if shape, ok := wireShapes[typ]; ok {
			for _, w := range shape {
				want = append(want, w.name)
			}
		} else {
			for i := 0; i < typ.NumField(); i++ {
				name, _, _ := strings.Cut(typ.Field(i).Tag.Get("json"), ",")
				want = append(want, name)
			}
		}
		sort.Strings(got)
		sort.Strings(want)
		if !slices.Equal(got, want) {
			t.Errorf("%v renders %v, its shape is %v", typ, got, want)
		}
	}
	if len(wireShapes)+len(tagMarshalers) != len(samples) {
		t.Errorf("%d wire shapes and %d tag marshalers, %d samples", len(wireShapes), len(tagMarshalers), len(samples))
	}
	_ = fmt.Sprint
}
