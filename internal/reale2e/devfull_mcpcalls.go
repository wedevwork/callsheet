package reale2e

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/filemode"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/go-git/go-git/v5/plumbing/storer"

	"github.com/wedevwork/callsheet/internal/contract"
)

// Recorded Callsheet MCP calls (design 12a-dev-flow-verify r0.6, Replay
// and audit: Recorded Callsheet MCP verification boundary). Every
// Callsheet tool use a coordinator phase records is judged by the
// exercise's closed tool policy, paired with its result, validated by the
// tool's shared server argument validator (internal/contract, which the
// MCP handlers call) whatever its result, and, when answered with
// success, strictly parsed and bound to its request and to the captured
// authority (plane task records, prepared plans, the seed publication,
// the manifest's workspace and the verified evidence objects). Error
// results are tolerated as no evidence; conflicting representations,
// policy, pairing and binding violations fail with mcp_call_invalid.

// mcpToolPrefix names a Callsheet MCP tool in a Claude stream.
const mcpToolPrefix = "mcp__" + mcpServerName + "__"

// Callsheet MCP tools of the coordinator protocol named in this package.
const (
	toolWsPush     = mcpToolPrefix + "ws_push"
	toolWsStatus   = mcpToolPrefix + "ws_status"
	toolWsDiff     = mcpToolPrefix + "ws_diff"
	toolWsPull     = mcpToolPrefix + "ws_pull"
	toolTaskLogs   = mcpToolPrefix + "task_logs"
	toolTaskCancel = mcpToolPrefix + "task_cancel"
)

// mcpPermittedTools are the nine Callsheet tools a coordinator phase may
// use; mcpForbiddenTools are the server's fourteen other registered tools
// (task_wait keeps its own coordinator_long_poll finding). Every tool of
// the server's registration is in exactly one of them
// (TestDevFullMCPPolicyCoverage); an unknown name is forbidden too.
var (
	mcpPermittedTools = []string{"dispatch", "task_ls", "task_show", "task_logs", "task_cancel", "ws_status", "ws_diff", "ws_push", "ws_pull"}
	mcpForbiddenTools = []string{"task_wait", "node_ls", "node_show", "role_add", "role_set", "role_ls", "role_show", "role_rm",
		"ws_create", "ws_show", "ws_ls", "ws_rm", "ws_prune", "ws_ref_set"}
)

// mcpBindingError is a request, response or authority binding violation:
// Field names the bound leaf (a JSON path of the response, or of the
// request for a request-only rule).
type mcpBindingError struct {
	Field  string
	Detail string
}

func (e *mcpBindingError) Error() string { return "binding " + e.Field + ": " + e.Detail }

// bindErr is a binding violation of field.
func bindErr(field, format string, args ...any) error {
	return &mcpBindingError{Field: field, Detail: fmt.Sprintf(format, args...)}
}

// missingAuthority is a successful answer naming what the captured
// authority does not hold.
func missingAuthority(field, what string) error {
	return &mcpBindingError{Field: field, Detail: "no captured authority for " + what}
}

// mcpPolicyError is a call the exercise's tool policy forbids.
type mcpPolicyError struct{ Detail string }

func (e *mcpPolicyError) Error() string { return "policy: " + e.Detail }

// mcpAuthority is the read-only authority a call is bound to, built from
// separately captured records only (never from the MCP answers being
// judged, the event map or a derived report).
type mcpAuthority struct {
	// goos is the MCP server's platform (local path rules).
	goos string
	// views are the captured plane task records by task ID.
	views map[string]contract.TaskView
	// owned maps each owned workflow task (prepared plan plus captured
	// plane admission) to its ordinal; prepared are the prepared dispatch
	// documents by ordinal.
	owned    map[string]int
	prepared map[int]*devDispatchDoc
	// seed is the prepared seed publication and seedPath its supplied
	// local path (nil: unavailable).
	seed     *DevSeedPush
	seedPath string
	// workspace and instance are the exercise workspace's identity; main
	// is the commit refs/heads/main resolves to (the prepared seed).
	workspace, instance, main string
	// objects are the verified evidence objects (nil: none).
	objects storer.EncodedObjectStorer
	// stopped reports a corroborated attempt stop; lastPhase is the last
	// coordinator phase actually launched.
	stopped   bool
	lastPhase int
}

// view is task id's captured plane record.
func (a *mcpAuthority) view(id string) (contract.TaskView, bool) {
	v, ok := a.views[id]
	return v, ok
}

// hasCommit reports a verified evidence commit object.
func (a *mcpAuthority) hasCommit(h string) bool {
	if a.objects == nil || !contract.ValidCommitHash(h) {
		return false
	}
	_, err := object.GetCommit(a.objects, plumbing.NewHash(h))
	return err == nil
}

// published is a captured task's published result commit, present in the
// verified objects.
func (a *mcpAuthority) published(id string) (string, bool) {
	v, ok := a.views[id]
	if !ok || v.Result == nil || v.Result.Workspace == nil || v.Result.Workspace.Publication != contract.PublicationPublished ||
		v.Result.Workspace.Commit == nil || !a.hasCommit(*v.Result.Workspace.Commit) {
		return "", false
	}
	return *v.Result.Workspace.Commit, true
}

// resolve resolves a canonical selector of this exercise: refs/heads/main
// to the prepared seed, a complete task ref to that captured published
// task, a full hash to a verified object and (allowEmpty) empty to no
// commit. Anything else is unknown.
func (a *mcpAuthority) resolve(sel string, allowEmpty bool) (*string, bool) {
	s, err := contract.ParseSelector(sel, "selector", allowEmpty)
	if err != nil {
		return nil, false
	}
	switch s.Kind {
	case contract.SelectorKindEmpty:
		return nil, true
	case contract.SelectorKindHash:
		if a.hasCommit(s.Value) {
			return &s.Value, true
		}
	case contract.SelectorKindBranch:
		if s.Value == contract.DefaultBranchRef && a.main != "" {
			m := a.main
			return &m, true
		}
	case contract.SelectorKindTask:
		if c, ok := a.published(strings.TrimPrefix(s.Value, contract.TaskRefPrefix)); ok {
			return &c, true
		}
	}
	return nil, false
}

// jsonDiff is the first differing JSON leaf of got and want under prefix
// ("" when equal): objects key by key in sorted order, arrays element by
// element, presence and null distinguished.
func jsonDiff(prefix string, got, want any) string {
	var g, w any
	json.Unmarshal(mustJSON(got), &g)
	json.Unmarshal(mustJSON(want), &w)
	return diffValue(prefix, g, w)
}

func diffValue(path string, g, w any) string {
	gm, gok := g.(map[string]any)
	wm, wok := w.(map[string]any)
	if gok && wok {
		keys := map[string]bool{}
		for k := range gm {
			keys[k] = true
		}
		for k := range wm {
			keys[k] = true
		}
		names := make([]string, 0, len(keys))
		for k := range keys {
			names = append(names, k)
		}
		sort.Strings(names)
		for _, k := range names {
			gv, gin := gm[k]
			wv, win := wm[k]
			if gin != win {
				return joinPath(path, k)
			}
			if d := diffValue(joinPath(path, k), gv, wv); d != "" {
				return d
			}
		}
		return ""
	}
	ga, gok := g.([]any)
	wa, wok := w.([]any)
	if gok && wok {
		for i := 0; i < len(ga) && i < len(wa); i++ {
			if d := diffValue(path+"[]", ga[i], wa[i]); d != "" {
				return d
			}
		}
		if len(ga) != len(wa) {
			return path + "[]"
		}
		return ""
	}
	if !bytes.Equal(mustJSON(g), mustJSON(w)) {
		if path == "" {
			return "."
		}
		return path
	}
	return ""
}

// joinPath appends a key to a JSON path.
func joinPath(path, k string) string {
	if path == "" {
		return k
	}
	return path + "." + k
}

// bindEqual is a binding error at the first differing leaf of got and
// want under field (nil when equal).
func bindEqual(field string, got, want any) error {
	if d := jsonDiff(field, got, want); d != "" {
		return bindErr(d, "differs from the bound value")
	}
	return nil
}

// bindView binds a returned task view (at field) to its captured plane
// record: the entire immutable identity I(task) always; a terminal view
// additionally exactly the captured terminal state and entire result. A
// nonterminal view may precede the terminal capture and is not compared
// with its later state. displayTail is task_show's form (design
// 12a-dev-flow-verify r0.12): its result.log_tail mirrors the
// request-selected top-level display tail, so only that leaf is left out
// of the result equality (on copies; the captured authority is never
// changed) and the caller binds it to the requested lines. dispatch and
// task_cancel bind the entire result, its log_tail included.
func bindView(field string, v, auth contract.TaskView, displayTail bool) error {
	if err := bindEqual(field, identityOf(v), identityOf(auth)); err != nil {
		return err
	}
	if !contract.TaskTerminal(v.State) {
		return nil
	}
	if v.State != auth.State {
		return bindErr(joinPath(field, "state"), "terminal state %s, captured %s", v.State, auth.State)
	}
	got, want := v.Result, auth.Result
	if displayTail {
		got, want = withoutLogTail(got), withoutLogTail(want)
	}
	return bindEqual(joinPath(field, "result"), got, want)
}

// withoutLogTail is a copy of r with its display tail cleared (nil: nil).
func withoutLogTail(r *contract.TaskResult) *contract.TaskResult {
	if r == nil {
		return nil
	}
	c := *r
	c.LogTail = ""
	return &c
}

// bindSummary binds a task_ls entry (at field) to its captured plane
// record: the immutable summary projection, and a terminal state equal to
// the captured terminal state.
func bindSummary(field string, s contract.TaskSummary, auth contract.TaskView) error {
	type projection struct {
		TaskID      string                 `json:"task_id"`
		Target      contract.TaskTarget    `json:"target"`
		Role        contract.TaskRole      `json:"role"`
		Effective   contract.TaskEffective `json:"effective"`
		CreatedAt   string                 `json:"created_at"`
		RequestedBy contract.RequestedBy   `json:"requested_by"`
	}
	got := projection{s.TaskID, s.Target, s.Role, s.Effective, s.CreatedAt, s.RequestedBy}
	want := projection{auth.TaskID, auth.Request.Target, auth.Role, auth.Effective, auth.CreatedAt, auth.Request.RequestedBy}
	if err := bindEqual(field, got, want); err != nil {
		return err
	}
	if contract.TaskTerminal(s.State) && s.State != auth.State {
		return bindErr(joinPath(field, "state"), "terminal state %s, captured %s", s.State, auth.State)
	}
	return nil
}

// tailLines counts a display tail's lines as contract.LogTail selects
// them: a trailing LF ends the last line without adding one.
func tailLines(tail string) int {
	if tail == "" {
		return 0
	}
	return strings.Count(strings.TrimSuffix(tail, "\n"), "\n") + 1
}

// mcpPage is one paged workspace read: its enumeration key (tool, form
// and selection), its continuation request and the answer's generation
// token, instance and next cursor.
type mcpPage struct {
	key                  string
	after                string
	instance, generation string
	next                 *string
}

// mcpOutcome is what one successful call supplies to the session-level
// rules: a dispatched task, the task IDs a listing holds, a workspace
// page.
type mcpOutcome struct {
	dispatched string
	listed     []string
	page       *mcpPage
}

// mcpTool is one permitted tool: its request validation (the shared
// server validator plus the exercise's request policy, applied whatever
// the result), its success judgment (strict response parse, then the
// request and authority bindings) and its strict success parse (for the
// success/error conflict rule).
type mcpTool struct {
	validate func(a *mcpAuthority, args []byte) error
	judge    func(a *mcpAuthority, args, answer []byte) (mcpOutcome, error)
	success  func(a *mcpAuthority, args, text []byte) bool
}

// placeholderRequester is a well-formed attribution used only to run the
// server's request validation before the captured requester is known.
var placeholderRequester = contract.RequestedBy{Name: "coordinator", Version: "1", Hostname: "coordinator"}

// preparedOrdinal is the ordinal whose prepared document a dispatch's
// validated arguments are exactly (0: none).
func (a *mcpAuthority) preparedOrdinal(args contract.DispatchArgs) int {
	ords := make([]int, 0, len(a.prepared))
	for n := range a.prepared {
		ords = append(ords, n)
	}
	sort.Ints(ords)
	for _, n := range ords {
		if sameDispatchArgs(args, expectedDispatchArgs(a.prepared[n])) {
			return n
		}
	}
	return 0
}

// ownedTask is ordinal n's owned task ("" none).
func (a *mcpAuthority) ownedTask(n int) string {
	for id, o := range a.owned {
		if o == n {
			return id
		}
	}
	return ""
}

// validateDispatch is dispatch's shared validation (decode, the request
// with a well-formed attribution, the wait) and the exercise policy: no
// wait, and exactly a prepared dispatch document.
func validateDispatch(a *mcpAuthority, args []byte) error {
	d, err := contract.DecodeDispatchArgs(args)
	if err != nil {
		return err
	}
	if _, err := d.Request(placeholderRequester); err != nil {
		return err
	}
	wait, err := d.WaitDuration()
	if err != nil {
		return err
	}
	if wait != nil {
		return &mcpPolicyError{"dispatch is asynchronous only: a wait is forbidden"}
	}
	if a.preparedOrdinal(d) == 0 {
		return bindErr("request", "not exactly a prepared dispatch document")
	}
	return nil
}

// judgeDispatch binds a successful dispatch: the validated request with
// the captured plane record's requester equals the returned request, and
// the returned task is the prepared ordinal's captured task.
func judgeDispatch(a *mcpAuthority, args, answer []byte) (mcpOutcome, error) {
	d, _ := contract.DecodeDispatchArgs(args)
	v, err := contract.ParseDispatchResponse(answer)
	if err != nil {
		return mcpOutcome{}, err
	}
	n := a.preparedOrdinal(d)
	id := a.ownedTask(n)
	auth, ok := a.view(id)
	if !ok {
		return mcpOutcome{}, missingAuthority("task", fmt.Sprintf("prepared ordinal %d's task", n))
	}
	req, err := d.Request(auth.Request.RequestedBy)
	if err != nil {
		return mcpOutcome{}, err
	}
	if err := bindEqual("task.request", v.Request, req); err != nil {
		return mcpOutcome{}, err
	}
	if err := bindView("task", v, auth, false); err != nil {
		return mcpOutcome{}, err
	}
	return mcpOutcome{dispatched: v.TaskID}, nil
}

func validateTaskList(_ *mcpAuthority, args []byte) error {
	_, _, err := contract.ParseTaskListArgs(args)
	return err
}

// judgeTaskList binds a listing page: strictly ascending unique IDs (the
// parser), every ID after the exclusive cursor, at most the effective
// limit, and every entry its captured task's summary.
func judgeTaskList(a *mcpAuthority, args, answer []byte) (mcpOutcome, error) {
	after, limit, _ := contract.ParseTaskListArgs(args)
	page, _, err := contract.ParseTaskListResponse(answer)
	if err != nil {
		return mcpOutcome{}, err
	}
	if len(page) > limit {
		return mcpOutcome{}, bindErr("tasks", "%d entries for limit %d", len(page), limit)
	}
	out := mcpOutcome{}
	for _, s := range page {
		if s.TaskID <= after {
			return mcpOutcome{}, bindErr("tasks[].task_id", "entry %s is not after the cursor %s", s.TaskID, after)
		}
		auth, ok := a.view(s.TaskID)
		if !ok {
			return mcpOutcome{}, missingAuthority("tasks[].task_id", s.TaskID)
		}
		if err := bindSummary("tasks[]", s, auth); err != nil {
			return mcpOutcome{}, err
		}
		out.listed = append(out.listed, s.TaskID)
	}
	return out, nil
}

func validateTaskShow(_ *mcpAuthority, args []byte) error {
	_, _, err := contract.ParseTaskShowArgs(args)
	return err
}

// judgeTaskShow binds a shown task to the requested ID and its captured
// record, and its tail to the requested lines.
func judgeTaskShow(a *mcpAuthority, args, answer []byte) (mcpOutcome, error) {
	id, lines, _ := contract.ParseTaskShowArgs(args)
	v, err := contract.ParseTaskShowResponse(answer)
	if err != nil {
		return mcpOutcome{}, err
	}
	if v.TaskID != id {
		return mcpOutcome{}, bindErr("task.task_id", "answers %s for %s", v.TaskID, id)
	}
	auth, ok := a.view(id)
	if !ok {
		return mcpOutcome{}, missingAuthority("task.task_id", id)
	}
	if err := bindView("task", v, auth, true); err != nil {
		return mcpOutcome{}, err
	}
	// The display tails: result.log_tail mirrors log_tail (the strict
	// parser's rule, kept here as the binding's own), and both hold at
	// most the requested lines; equality with the captured
	// DefaultTailLines tail is neither required nor forbidden.
	if v.Result != nil && v.Result.LogTail != v.LogTail {
		return mcpOutcome{}, bindErr("task.result.log_tail", "differs from the display tail log_tail")
	}
	if n := tailLines(v.LogTail); n > lines {
		return mcpOutcome{}, bindErr("task.log_tail", "%d lines for %d requested", n, lines)
	}
	return mcpOutcome{}, nil
}

func validateTaskLogs(_ *mcpAuthority, args []byte) error {
	_, _, err := contract.ParseTaskLogsArgs(args)
	return err
}

// judgeTaskLogs binds a diagnostic log answer to the requested captured
// task (its bytes are contract-only).
func judgeTaskLogs(a *mcpAuthority, args, answer []byte) (mcpOutcome, error) {
	id, _, _ := contract.ParseTaskLogsArgs(args)
	r, err := contract.ParseTaskLogsResponse(answer)
	if err != nil {
		return mcpOutcome{}, err
	}
	if r.TaskID != id {
		return mcpOutcome{}, bindErr("task_id", "answers %s for %s", r.TaskID, id)
	}
	if _, ok := a.view(id); !ok {
		return mcpOutcome{}, missingAuthority("task_id", id)
	}
	return mcpOutcome{}, nil
}

func validateTaskCancel(_ *mcpAuthority, args []byte) error {
	_, err := contract.ParseTaskCancelArgs(args)
	return err
}

// judgeTaskCancel binds a cancellation answer: the requested ID is the
// outer and nested task's, accepted matches its 200/202 semantics (the
// contract parser), and the nested task is its captured record.
func judgeTaskCancel(a *mcpAuthority, args, answer []byte) (mcpOutcome, error) {
	id, _ := contract.ParseTaskCancelArgs(args)
	var head struct {
		Accepted bool   `json:"accepted"`
		TaskID   string `json:"task_id"`
		Task     struct {
			TaskID string `json:"task_id"`
		} `json:"task"`
	}
	if err := json.Unmarshal(answer, &head); err != nil {
		return mcpOutcome{}, contract.New(contract.CodeInvalidArgument, "cancel response is not a JSON object")
	}
	// The requested ID is the outer and nested task's (bound before the
	// strict parse, which checks the same).
	switch {
	case head.TaskID != id:
		return mcpOutcome{}, bindErr("task_id", "answers %s for %s", head.TaskID, id)
	case head.Task.TaskID != id:
		return mcpOutcome{}, bindErr("task.task_id", "answers %s for %s", head.Task.TaskID, id)
	}
	status := 200
	if head.Accepted {
		status = 202
	}
	r, err := contract.ParseCancelResponse(answer, status, id)
	if err != nil {
		return mcpOutcome{}, err
	}
	auth, ok := a.view(id)
	if !ok {
		return mcpOutcome{}, missingAuthority("task_id", id)
	}
	if err := bindView("task", r.Task, auth, false); err != nil {
		return mcpOutcome{}, err
	}
	return mcpOutcome{}, nil
}

// validateWsPush is ws_push's shared validation and the exercise policy:
// exactly the prepared seed publication (its workspace, instance and the
// supplied path, no omitted-path default) to the canonical main branch.
func validateWsPush(a *mcpAuthority, args []byte) error {
	p, err := contract.ParseWsPushArgs(args, a.goos)
	if err != nil {
		return err
	}
	in, err := contract.ValidatePush(p.Name, p.Instance, p.Branch)
	if err != nil {
		return err
	}
	switch {
	case a.seed == nil:
		return missingAuthority("name", "the prepared seed publication")
	case p.Name != a.seed.Workspace:
		return bindErr("name", "not the prepared seed workspace")
	case p.Instance != a.seed.Instance:
		return bindErr("instance", "not the prepared seed instance")
	case !p.PathSet || p.Path != a.seedPath:
		return bindErr("path", "not the supplied seed path")
	case in.Branch != a.seed.Ref || in.Branch != contract.DefaultBranchRef:
		return bindErr("branch", "canonical branch %s is not the seed's %s", in.Branch, a.seed.Ref)
	}
	return nil
}

// judgeWsPush binds the seed publication's answer: the canonical branch,
// the seed workspace, instance and commit (a verified object).
func judgeWsPush(a *mcpAuthority, args, answer []byte) (mcpOutcome, error) {
	p, _ := contract.ParseWsPushArgs(args, a.goos)
	in, _ := contract.ValidatePush(p.Name, p.Instance, p.Branch)
	r, err := contract.ParseWorkspacePushResult(answer)
	if err != nil {
		return mcpOutcome{}, err
	}
	switch {
	case r.Branch != in.Branch:
		return mcpOutcome{}, bindErr("branch", "answers %s for %s", r.Branch, in.Branch)
	case r.Name != a.seed.Workspace:
		return mcpOutcome{}, bindErr("name", "not the seed workspace")
	case r.Instance != a.seed.Instance:
		return mcpOutcome{}, bindErr("instance", "not the seed instance")
	case r.Commit != a.seed.Commit || !a.hasCommit(r.Commit):
		return mcpOutcome{}, bindErr("commit", "not the prepared seed commit")
	}
	return mcpOutcome{}, nil
}

func validateWsStatus(_ *mcpAuthority, args []byte) error {
	_, err := contract.ParseWsStatusArgs(args)
	return err
}

// judgeWsStatus binds a status answer. Task form: the requested task's
// captured binding entirely and, when terminal, its captured state and
// entire workspace result. Explicit form: the exercise workspace, every
// ref the seed main ref or a captured published task ref with its commit,
// the continuation's instance and generation.
func judgeWsStatus(a *mcpAuthority, args, answer []byte) (mcpOutcome, error) {
	call, _ := contract.ParseWsStatusArgs(args)
	if call.TaskID != "" {
		r, err := contract.ParseTaskWorkspaceStatusResponse(answer)
		if err != nil {
			return mcpOutcome{}, err
		}
		w := r.Workspace
		if w.TaskID != call.TaskID {
			return mcpOutcome{}, bindErr("workspace.task_id", "answers %s for %s", w.TaskID, call.TaskID)
		}
		auth, ok := a.view(call.TaskID)
		if !ok || auth.WorkspaceBinding == nil {
			return mcpOutcome{}, missingAuthority("workspace.task_id", call.TaskID)
		}
		if err := bindEqual("workspace.binding", w.Binding, *auth.WorkspaceBinding); err != nil {
			return mcpOutcome{}, err
		}
		if contract.TaskTerminal(w.State) {
			if w.State != auth.State {
				return mcpOutcome{}, bindErr("workspace.state", "terminal state %s, captured %s", w.State, auth.State)
			}
			var want *contract.TaskWorkspaceResult
			if auth.Result != nil {
				want = auth.Result.Workspace
			}
			if err := bindEqual("workspace.result", w.Result, want); err != nil {
				return mcpOutcome{}, err
			}
		}
		return mcpOutcome{}, nil
	}
	if call.Name != a.workspace {
		return mcpOutcome{}, bindErr("name", "not the exercise workspace")
	}
	r, err := contract.ParseWorkspaceStatus(answer, call.Page.After, call.Page.Limit)
	if err != nil {
		return mcpOutcome{}, err
	}
	switch {
	case r.Name != a.workspace:
		return mcpOutcome{}, bindErr("name", "not the exercise workspace")
	case r.Instance != a.instance:
		return mcpOutcome{}, bindErr("instance", "not the exercise instance")
	}
	if err := bindContinuation(call.Page, r.Instance, r.Generation); err != nil {
		return mcpOutcome{}, err
	}
	for _, ref := range r.Refs {
		var want string
		var ok bool
		switch {
		case ref.Name == contract.DefaultBranchRef:
			want, ok = a.main, a.main != ""
		case strings.HasPrefix(ref.Name, contract.TaskRefPrefix):
			want, ok = a.published(strings.TrimPrefix(ref.Name, contract.TaskRefPrefix))
		}
		if !ok {
			return mcpOutcome{}, bindErr("refs[].name", "%s is neither the seed main ref nor a captured published task ref", ref.Name)
		}
		if ref.Commit != want {
			return mcpOutcome{}, bindErr("refs[].commit", "%s names another commit", ref.Name)
		}
	}
	return mcpOutcome{page: &mcpPage{key: "ws_status|" + call.Name, after: call.Page.After, instance: r.Instance, generation: r.Generation,
		next: r.NextAfter}}, nil
}

// bindContinuation binds a continuation's instance and generation to its
// answer (a first page binds none).
func bindContinuation(p contract.WsPage, instance, generation string) error {
	switch {
	case p.After == "":
		return nil
	case p.Instance != instance:
		return bindErr("instance", "a continuation of instance %s answered by %s", p.Instance, instance)
	case p.Generation != generation:
		return bindErr("generation", "a continuation of generation %s answered by %s", p.Generation, generation)
	}
	return nil
}

func validateWsDiff(_ *mcpAuthority, args []byte) error {
	_, err := contract.ParseWsDiffArgs(args)
	return err
}

// judgeWsDiff binds a diff page: the resolved workspace, base and target
// (task form: the requested task's captured binding and published result;
// explicit form: the exercise workspace and its selectors resolved
// against the captured seed, task refs and verified objects), the
// continuation, and the page recomputed from the two verified trees.
func judgeWsDiff(a *mcpAuthority, args, answer []byte) (mcpOutcome, error) {
	call, _ := contract.ParseWsDiffArgs(args)
	var name, instance, key string
	var base *string
	var target string
	if call.TaskID != "" {
		auth, ok := a.view(call.TaskID)
		commit, pub := a.published(call.TaskID)
		if !ok || auth.WorkspaceBinding == nil || !pub {
			return mcpOutcome{}, missingAuthority("target_commit", call.TaskID+"'s published result")
		}
		name, instance, base, target = auth.WorkspaceBinding.Name, auth.WorkspaceBinding.Instance, auth.WorkspaceBinding.BaseCommit, commit
		key = "ws_diff|task|" + call.TaskID
	} else {
		if call.Name != a.workspace {
			return mcpOutcome{}, bindErr("name", "not the exercise workspace")
		}
		var ok bool
		if base, ok = a.resolve(call.Base, true); !ok {
			return mcpOutcome{}, bindErr("base_commit", "base %s is not a captured selector", call.Base)
		}
		t, ok := a.resolve(call.Target, false)
		if !ok {
			return mcpOutcome{}, bindErr("target_commit", "target %s is not a captured selector", call.Target)
		}
		name, instance, target = a.workspace, a.instance, *t
		key = "ws_diff|" + call.Name + "|" + call.Base + "|" + call.Target
	}
	r, err := contract.ParseWorkspaceDiff(answer, call.AfterPath, call.Page.Limit)
	if err != nil {
		return mcpOutcome{}, err
	}
	switch {
	case r.Name != name:
		return mcpOutcome{}, bindErr("name", "answers workspace %s for %s", r.Name, name)
	case r.Instance != instance:
		return mcpOutcome{}, bindErr("instance", "answers instance %s for %s", r.Instance, instance)
	case deref(r.BaseCommit) != deref(base) || (r.BaseCommit == nil) != (base == nil):
		return mcpOutcome{}, bindErr("base_commit", "answers %s for %s", deref(r.BaseCommit), deref(base))
	case r.TargetCommit != target:
		return mcpOutcome{}, bindErr("target_commit", "answers %s for %s", r.TargetCommit, target)
	}
	if err := bindContinuation(call.Page, r.Instance, r.Generation); err != nil {
		return mcpOutcome{}, err
	}
	header := contract.WorkspaceDiffResponse{Name: r.Name, Instance: r.Instance, Generation: r.Generation, BaseCommit: r.BaseCommit, TargetCommit: r.TargetCommit}
	changes, next, err := diffPage(a.objects, header, base, target, call.AfterPath, call.Page.Limit)
	if err != nil {
		return mcpOutcome{}, bindErr("changes", "the verified trees do not recompute: %v", err)
	}
	if err := bindEqual("changes", r.Changes, changes); err != nil {
		return mcpOutcome{}, err
	}
	if err := bindEqual("next_after", r.NextAfter, next); err != nil {
		return mcpOutcome{}, err
	}
	return mcpOutcome{page: &mcpPage{key: key, after: call.Page.After, instance: r.Instance, generation: r.Generation, next: r.NextAfter}}, nil
}

func validateWsPull(a *mcpAuthority, args []byte) error {
	_, err := contract.ParseWsPullArgs(args, a.goos)
	return err
}

// judgeWsPull binds a pull answer: task form, the task's canonical ref
// and its captured binding and published result; explicit form, the
// exercise workspace, the canonical validated selector and its resolved
// commit. The local destination effect is contract-only.
func judgeWsPull(a *mcpAuthority, args, answer []byte) (mcpOutcome, error) {
	p, _ := contract.ParseWsPullArgs(args, a.goos)
	r, err := contract.ParseWorkspacePullResult(answer)
	if err != nil {
		return mcpOutcome{}, err
	}
	if p.TaskID != "" {
		auth, ok := a.view(p.TaskID)
		commit, pub := a.published(p.TaskID)
		if !ok || auth.WorkspaceBinding == nil || !pub {
			return mcpOutcome{}, missingAuthority("commit", p.TaskID+"'s published result")
		}
		switch {
		case r.Selector != contract.TaskRefPrefix+p.TaskID:
			return mcpOutcome{}, bindErr("selector", "answers %s for task %s", r.Selector, p.TaskID)
		case r.Name != auth.WorkspaceBinding.Name:
			return mcpOutcome{}, bindErr("name", "not the task's workspace")
		case r.Instance != auth.WorkspaceBinding.Instance:
			return mcpOutcome{}, bindErr("instance", "not the task's instance")
		case r.Commit != commit:
			return mcpOutcome{}, bindErr("commit", "not the task's published result")
		}
		return mcpOutcome{}, nil
	}
	if p.Name != a.workspace {
		return mcpOutcome{}, bindErr("name", "not the exercise workspace")
	}
	in, _ := contract.ValidatePull(p.Name, p.Ref)
	commit, ok := a.resolve(in.Canonical, false)
	switch {
	case r.Name != a.workspace:
		return mcpOutcome{}, bindErr("name", "not the exercise workspace")
	case r.Instance != a.instance:
		return mcpOutcome{}, bindErr("instance", "not the exercise instance")
	case r.Selector != in.Canonical:
		return mcpOutcome{}, bindErr("selector", "answers %s for %s", r.Selector, in.Canonical)
	case !ok:
		return mcpOutcome{}, bindErr("selector", "%s is not a captured selector", in.Canonical)
	case r.Commit != *commit:
		return mcpOutcome{}, bindErr("commit", "not the commit %s resolves to", in.Canonical)
	}
	return mcpOutcome{}, nil
}

// mcpTools are the nine permitted tools, keyed by server name.
var mcpTools = map[string]mcpTool{
	"dispatch": {validate: validateDispatch, judge: judgeDispatch, success: func(_ *mcpAuthority, _, text []byte) bool {
		_, err := contract.ParseDispatchResponse(text)
		return err == nil
	}},
	"task_ls": {validate: validateTaskList, judge: judgeTaskList, success: func(_ *mcpAuthority, _, text []byte) bool {
		_, _, err := contract.ParseTaskListResponse(text)
		return err == nil
	}},
	"task_show": {validate: validateTaskShow, judge: judgeTaskShow, success: func(_ *mcpAuthority, _, text []byte) bool {
		_, err := contract.ParseTaskShowResponse(text)
		return err == nil
	}},
	"task_logs": {validate: validateTaskLogs, judge: judgeTaskLogs, success: func(_ *mcpAuthority, _, text []byte) bool {
		_, err := contract.ParseTaskLogsResponse(text)
		return err == nil
	}},
	"task_cancel": {validate: validateTaskCancel, judge: judgeTaskCancel, success: func(_ *mcpAuthority, args, text []byte) bool {
		id, _ := contract.ParseTaskCancelArgs(args)
		for _, status := range []int{200, 202} {
			if _, err := contract.ParseCancelResponse(text, status, id); err == nil {
				return true
			}
		}
		return false
	}},
	"ws_push": {validate: validateWsPush, judge: judgeWsPush, success: func(_ *mcpAuthority, _, text []byte) bool {
		_, err := contract.ParseWorkspacePushResult(text)
		return err == nil
	}},
	"ws_status": {validate: validateWsStatus, judge: judgeWsStatus, success: func(_ *mcpAuthority, args, text []byte) bool {
		call, _ := contract.ParseWsStatusArgs(args)
		if call.TaskID != "" {
			_, err := contract.ParseTaskWorkspaceStatusResponse(text)
			return err == nil
		}
		_, err := contract.ParseWorkspaceStatus(text, call.Page.After, call.Page.Limit)
		return err == nil
	}},
	"ws_diff": {validate: validateWsDiff, judge: judgeWsDiff, success: func(_ *mcpAuthority, args, text []byte) bool {
		call, _ := contract.ParseWsDiffArgs(args)
		_, err := contract.ParseWorkspaceDiff(text, call.AfterPath, call.Page.Limit)
		return err == nil
	}},
	"ws_pull": {validate: validateWsPull, judge: judgeWsPull, success: func(_ *mcpAuthority, _, text []byte) bool {
		_, err := contract.ParseWorkspacePullResult(text)
		return err == nil
	}},
}

// deref is *p, or "" for nil.
func deref(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}

// judgeMCPCall is the per-call judge the offline checker applies to one
// recorded successful call of tool (its stream name): the tool policy,
// the shared argument validation and request policy, then the strict
// answer and its bindings.
func judgeMCPCall(a *mcpAuthority, tool string, args json.RawMessage, answer string) (mcpOutcome, error) {
	name, err := mcpPermitted(tool)
	if err != nil {
		return mcpOutcome{}, err
	}
	t := mcpTools[name]
	if err := t.validate(a, args); err != nil {
		return mcpOutcome{}, err
	}
	return t.judge(a, args, []byte(answer))
}

// mcpPermitted returns a permitted tool's server name, or the policy
// violation of any other Callsheet tool name.
func mcpPermitted(tool string) (string, error) {
	name, ok := strings.CutPrefix(tool, mcpToolPrefix)
	if !ok {
		return "", &mcpPolicyError{tool + " is not a Callsheet MCP tool"}
	}
	if _, permitted := mcpTools[name]; !permitted {
		return "", &mcpPolicyError{name + " is not permitted in a coordinator phase"}
	}
	return name, nil
}

// ---- Session-level rules ----

// mcpPos is a coordinator fact's source position: phase, stream line and
// content-block index, ordered lexicographically.
type mcpPos struct{ phase, line, block int }

func (p mcpPos) before(q mcpPos) bool {
	if p.phase != q.phase {
		return p.phase < q.phase
	}
	if p.line != q.line {
		return p.line < q.line
	}
	return p.block < q.block
}

// mcpResultRec is one recorded tool result.
type mcpResultRec struct {
	pos     mcpPos
	text    string
	isError bool
}

// mcpCallRec is one recorded Callsheet tool use with its paired results.
type mcpCallRec struct {
	pos     mcpPos
	id      string
	tool    string
	args    json.RawMessage
	results []mcpResultRec
}

// mcpVerdict judges every recorded Callsheet call of the phases (by
// phase number) under the authority and returns each failing phase's
// problem (mcp_call_invalid).
func mcpVerdict(phases map[int][]sessionRecord, a *mcpAuthority) map[int]string {
	out, _ := mcpAnalyze(phases, a)
	return out
}

// mcpAnalyze is mcpVerdict with each violation's diagnosis (for tests and
// diagnostics only; the verdict never depends on the text).
func mcpAnalyze(phases map[int][]sessionRecord, a *mcpAuthority) (map[int]string, []string) {
	out := map[int]string{}
	var why []string
	fail := func(p int, format string, args ...any) {
		out[p] = devMCPCallInvalid
		why = append(why, fmt.Sprintf("phase %d: ", p)+fmt.Sprintf(format, args...))
	}
	var calls []*mcpCallRec
	nums := make([]int, 0, len(phases))
	for p := range phases {
		nums = append(nums, p)
	}
	sort.Ints(nums)
	for _, p := range nums {
		recs := phases[p]
		// Every tool use of the phase, vendor tools included, binds its
		// own results.
		uses := map[string]*mcpCallRec{}
		seen := map[string]bool{}
		for _, r := range recs {
			for k, b := range r.blocks {
				pos := mcpPos{p, r.src.FirstLine, k}
				switch {
				case r.kind == "assistant" && b.typ == "tool_use":
					cs := strings.HasPrefix(b.name, mcpToolPrefix)
					if seen[b.id] {
						// A reused tool-use ID cannot bind its results.
						if cs || uses[b.id] != nil {
							fail(p, "tool-use ID %s is reused", b.id)
						}
						continue
					}
					seen[b.id] = true
					if cs {
						c := &mcpCallRec{pos: pos, id: b.id, tool: b.name, args: b.input}
						uses[b.id] = c
						calls = append(calls, c)
					}
				case r.kind == "user" && b.typ == "tool_result":
					if !seen[b.toolUseID] {
						// An orphaned result (no use in the phase), or one
						// reordered before its use: neither binds.
						fail(p, "result %s has no preceding use", b.toolUseID)
						continue
					}
					if c := uses[b.toolUseID]; c != nil {
						c.results = append(c.results, mcpResultRec{pos: pos, text: b.text, isError: b.isError})
					}
				}
			}
		}
	}
	// Judge each call in source order. State a result establishes (a
	// dispatched task, a lost reply awaiting recovery, a page's cursor)
	// takes effect at that result's position: a later call depends on it
	// only when the result precedes that call's own tool use.
	var firstDispatch *mcpPos
	for _, c := range calls {
		if c.tool == toolDispatch && (firstDispatch == nil || c.pos.before(*firstDispatch)) {
			p := c.pos
			firstDispatch = &p
		}
	}
	dispatchedAt := map[string]mcpPos{}
	pendingRecovery := map[int]mcpPos{}
	type enumeration struct {
		key, instance, generation string
		next                      *string
		// at is the latest page's result position.
		at mcpPos
	}
	var enums []*enumeration
	var cancels []*mcpCallRec
	var dispatchUses []mcpPos
	for _, c := range calls {
		name, err := mcpPermitted(c.tool)
		if name == "" && strings.TrimPrefix(c.tool, mcpToolPrefix) == "task_wait" {
			// coordinator_long_poll (the stream qualifier).
			continue
		}
		if err != nil {
			fail(c.pos.phase, "%s: %v", c.id, err)
			continue
		}
		t := mcpTools[name]
		if err := t.validate(a, c.args); err != nil {
			fail(c.pos.phase, "%s %s: %v", name, c.id, err)
			continue
		}
		switch name {
		case "dispatch":
			dispatchUses = append(dispatchUses, c.pos)
		case "ws_push":
			// The seed publication, its answer included, precedes workflow
			// dispatch.
			late := firstDispatch != nil && !c.pos.before(*firstDispatch)
			for _, r := range c.results {
				late = late || firstDispatch != nil && !r.pos.before(*firstDispatch)
			}
			if late {
				fail(c.pos.phase, "ws_push %s or its answer follows a dispatch", c.id)
				continue
			}
		case "task_cancel":
			cancels = append(cancels, c)
		}
		switch len(c.results) {
		case 0:
			if name == "dispatch" {
				d, _ := contract.DecodeDispatchArgs(c.args)
				pendingRecovery[a.preparedOrdinal(d)] = c.pos
			} else if !a.stopped {
				// A missing answer cannot allow completion.
				fail(c.pos.phase, "%s %s has no result", name, c.id)
			}
			continue
		case 1:
		default:
			// A second result for one use is a conflict.
			fail(c.pos.phase, "%s %s has %d results", name, c.id, len(c.results))
			continue
		}
		res := c.results[0]
		if res.isError {
			if t.success(a, c.args, []byte(res.text)) {
				fail(c.pos.phase, "%s %s: an error-flagged success response", name, c.id)
				continue
			}
			if name == "dispatch" {
				// A lost reply: recovery starts after the error answer.
				d, _ := contract.DecodeDispatchArgs(c.args)
				pendingRecovery[a.preparedOrdinal(d)] = res.pos
			}
			continue
		}
		if name == "task_show" && taskShowReplaced(res.text) {
			// The vendor's persisted-output replacement of a task_show
			// answer (design 12a-dev-flow-verify r0.10): no evidence, never
			// a response to bind. Any other malformed success still fails.
			continue
		}
		if _, err := contract.ParseErrorBody([]byte(res.text)); err == nil {
			fail(c.pos.phase, "%s %s: a success-flagged error body", name, c.id)
			continue
		}
		o, err := t.judge(a, c.args, []byte(res.text))
		if err != nil {
			fail(c.pos.phase, "%s %s: %v", name, c.id, err)
			continue
		}
		if o.dispatched != "" {
			dispatchedAt[o.dispatched] = res.pos
		}
		for _, id := range o.listed {
			if n, ok := a.owned[id]; ok {
				// A recovery listing requested after the reply was lost (or
				// after the unanswered dispatch).
				// The earliest qualifying answer wins, whatever order the
				// listings were requested in.
				if at, pending := pendingRecovery[n]; pending && at.before(c.pos) {
					if prev, done := dispatchedAt[id]; !done || res.pos.before(prev) {
						dispatchedAt[id] = res.pos
					}
				}
			}
		}
		if pg := o.page; pg != nil {
			if pg.after == "" {
				enums = append(enums, &enumeration{key: pg.key, instance: pg.instance, generation: pg.generation, next: pg.next, at: res.pos})
				continue
			}
			// The preceding page's answer is recorded before this
			// continuation is requested.
			var hit *enumeration
			for _, e := range enums {
				if e.key == pg.key && e.instance == pg.instance && e.generation == pg.generation && e.next != nil && *e.next == pg.after && e.at.before(c.pos) {
					hit = e
				}
			}
			if hit == nil {
				// A continuation without its preceding page.
				fail(c.pos.phase, "%s %s continues no preceding page answered before it", name, c.id)
				continue
			}
			hit.next, hit.at = pg.next, res.pos
		}
	}
	// The stopped-attempt cancellation rule.
	var firstCancel *mcpPos
	for _, c := range cancels {
		if c.pos.phase == a.lastPhase && (firstCancel == nil || c.pos.before(*firstCancel)) {
			p := c.pos
			firstCancel = &p
		}
	}
	for _, c := range cancels {
		id, _ := contract.ParseTaskCancelArgs(c.args)
		at, dispatched := dispatchedAt[id]
		_, owned := a.owned[id]
		if !a.stopped || c.pos.phase != a.lastPhase || !owned || !dispatched || !at.before(c.pos) {
			fail(c.pos.phase, "task_cancel %s: stopped %t, phase %d of %d, owned %t, dispatched before %t", c.id, a.stopped, c.pos.phase,
				a.lastPhase, owned, dispatched && at.before(c.pos))
		}
	}
	if firstCancel != nil {
		for _, d := range dispatchUses {
			if firstCancel.before(d) {
				fail(d.phase, "a dispatch follows the first task_cancel")
			}
		}
	}
	return out, why
}

// ---- The workspace diff recomputation ----

// diffEntry is one tree entry.
type diffEntry struct {
	name []byte
	mode filemode.FileMode
	hash plumbing.Hash
}

// key is the entry's ordering key: a directory sorts as name/.
func (e diffEntry) key() []byte {
	if e.mode == filemode.Dir {
		return append(append([]byte(nil), e.name...), '/')
	}
	return e.name
}

// diffWalker merges two trees in raw path byte order with the
// repository's workspace diff semantics (internal/workspace/diff.go):
// equal subtrees and subtrees wholly at or before the cursor are skipped,
// rows at or before the cursor are dropped, and the walk stops after max
// rows.
type diffWalker struct {
	s     storer.EncodedObjectStorer
	after []byte
	max   int
	rows  []contract.WorkspaceChange
}

var errDiffPageFull = errors.New("diff page full")

func (w *diffWalker) entries(h plumbing.Hash) ([]diffEntry, error) {
	if h.IsZero() {
		return nil, nil
	}
	t, err := object.GetTree(w.s, h)
	if err != nil {
		return nil, err
	}
	out := make([]diffEntry, len(t.Entries))
	for i, e := range t.Entries {
		out[i] = diffEntry{name: []byte(e.Name), mode: e.Mode, hash: e.Hash}
	}
	sort.SliceStable(out, func(i, j int) bool { return bytes.Compare(out[i].key(), out[j].key()) < 0 })
	return out, nil
}

func (w *diffWalker) before(prefix []byte) bool {
	return w.after != nil && len(prefix) > 0 && bytes.Compare(prefix, w.after) < 0 && !bytes.HasPrefix(w.after, prefix)
}

func (w *diffWalker) walk(prefix []byte, a, b plumbing.Hash) error {
	if a == b || w.before(prefix) {
		return nil
	}
	ea, err := w.entries(a)
	if err != nil {
		return err
	}
	eb, err := w.entries(b)
	if err != nil {
		return err
	}
	i, j := 0, 0
	for i < len(ea) || j < len(eb) {
		var c int
		switch {
		case i == len(ea):
			c = 1
		case j == len(eb):
			c = -1
		default:
			c = bytes.Compare(ea[i].key(), eb[j].key())
		}
		switch {
		case c < 0:
			err = w.side(prefix, ea[i], true)
			i++
		case c > 0:
			err = w.side(prefix, eb[j], false)
			j++
		default:
			x, y := ea[i], eb[j]
			i, j = i+1, j+1
			if x.mode == filemode.Dir {
				err = w.walk(diffJoin(prefix, x.name, true), x.hash, y.hash)
			} else if x.hash != y.hash || x.mode != y.mode {
				err = w.row(diffJoin(prefix, x.name, false), &x, &y)
			}
		}
		if err != nil {
			return err
		}
	}
	return nil
}

func diffJoin(prefix, name []byte, dir bool) []byte {
	p := append(append([]byte(nil), prefix...), name...)
	if dir {
		p = append(p, '/')
	}
	return p
}

func (w *diffWalker) side(prefix []byte, e diffEntry, deleted bool) error {
	if e.mode == filemode.Dir {
		if deleted {
			return w.walk(diffJoin(prefix, e.name, true), e.hash, plumbing.ZeroHash)
		}
		return w.walk(diffJoin(prefix, e.name, true), plumbing.ZeroHash, e.hash)
	}
	if deleted {
		return w.row(diffJoin(prefix, e.name, false), &e, nil)
	}
	return w.row(diffJoin(prefix, e.name, false), nil, &e)
}

func (w *diffWalker) size(h plumbing.Hash) (*int64, error) {
	o, err := w.s.EncodedObject(plumbing.BlobObject, h)
	if err != nil {
		return nil, err
	}
	n := o.Size()
	return &n, nil
}

func (w *diffWalker) row(path []byte, old, new *diffEntry) error {
	if w.after != nil && bytes.Compare(path, w.after) <= 0 {
		return nil
	}
	r := contract.WorkspaceChange{PathBase64: base64.StdEncoding.EncodeToString(path)}
	var err error
	switch {
	case old == nil:
		r.Kind = contract.ChangeAdded
	case new == nil:
		r.Kind = contract.ChangeDeleted
	default:
		r.Kind = contract.ChangeModified
	}
	mode := func(m filemode.FileMode) *string {
		s := fmt.Sprintf("%06o", uint32(m))
		return &s
	}
	if old != nil {
		r.OldMode = mode(old.mode)
		if r.OldBytes, err = w.size(old.hash); err != nil {
			return err
		}
	}
	if new != nil {
		r.NewMode = mode(new.mode)
		if r.NewBytes, err = w.size(new.hash); err != nil {
			return err
		}
	}
	w.rows = append(w.rows, r)
	if len(w.rows) >= w.max {
		return errDiffPageFull
	}
	return nil
}

// commitTreeHash is a verified commit's tree (zero for no commit).
func commitTreeHash(s storer.EncodedObjectStorer, c *string) (plumbing.Hash, error) {
	if c == nil {
		return plumbing.ZeroHash, nil
	}
	cm, err := object.GetCommit(s, plumbing.NewHash(*c))
	if err != nil {
		return plumbing.ZeroHash, err
	}
	return cm.TreeHash, nil
}

// diffPage recomputes one diff page from the verified objects: the rows
// after the exclusive byte-path cursor, then the page of at most limit
// rows that fits the serialized response bound with room reserved for
// next_after (internal/workspace/manager.go page); next names the last
// emitted path when rows remain, nil at exhaustion.
func diffPage(s storer.EncodedObjectStorer, header contract.WorkspaceDiffResponse, base *string, target string, after []byte, limit int) ([]contract.WorkspaceChange, *string, error) {
	if s == nil {
		return nil, nil, errors.New("no verified objects")
	}
	bt, err := commitTreeHash(s, base)
	if err != nil {
		return nil, nil, err
	}
	tt, err := commitTreeHash(s, &target)
	if err != nil {
		return nil, nil, err
	}
	w := &diffWalker{s: s, after: after, max: limit + 1}
	if err := w.walk(nil, bt, tt); err != nil && err != errDiffPageFull {
		return nil, nil, err
	}
	resp := header
	resp.Changes, resp.NextAfter = []contract.WorkspaceChange{}, nil
	b, err := contract.Encode(resp)
	if err != nil {
		return nil, nil, err
	}
	size := len(b) + 1
	out := []contract.WorkspaceChange{}
	var next *string
	key := func(r contract.WorkspaceChange) string { return r.PathBase64 }
	for i, r := range w.rows {
		if i == limit {
			k := key(w.rows[i-1])
			next = &k
			break
		}
		rb, err := contract.Encode(r)
		if err != nil {
			return nil, nil, err
		}
		add := len(rb)
		if i > 0 {
			add++
		}
		kb, _ := contract.Encode(key(r))
		if size+add+len(kb)-len("null") > contract.MaxWorkspaceResponse {
			if i == 0 {
				return nil, nil, errors.New("a single row does not fit the response bound")
			}
			k := key(w.rows[i-1])
			next = &k
			break
		}
		size += add
		out = append(out, r)
	}
	return out, next, nil
}

// ---- Authority from captured evidence ----

// mcpAuthorityInput names a bundle's authority sources: the platform, the
// exercise workspace with its prepared seed commit, the supplied seed path
// (from the regenerated render values), the corroborated stop, the task
// record directories by ordinal and their plane admission labels.
type mcpAuthorityInput struct {
	// mainRef is the evidence ref holding the prepared commit main
	// resolves to (the seed; the probe's candidate).
	mainRef  string
	goos     string
	ws       WorkspaceRecord
	seedPath string
	stopped  bool
	dirs     map[int]string
	labels   map[string]int
}

// buildMCPAuthority builds the authority from separately captured records
// only: every captured plane task record (preflights and workflow or probe
// tasks), the prepared dispatch documents, the owned tasks (a prepared
// plan with its plane admission event), the seed publication record when
// it agrees with the workspace identity, the verified evidence objects,
// and the last coordinator phase actually launched (its launch record and
// phase_started event).
func buildMCPAuthority(b *DevBundle, in mcpAuthorityInput) *mcpAuthority {
	a := &mcpAuthority{goos: in.goos, views: map[string]contract.TaskView{}, owned: map[string]int{}, prepared: map[int]*devDispatchDoc{},
		seedPath: in.seedPath, workspace: in.ws.Name, instance: in.ws.Instance, stopped: in.stopped}
	for rel, v := range b.docs {
		if r, ok := v.(*contract.TaskShowResponse); ok && strings.HasSuffix(rel, "/task.json") {
			a.views[r.Task.TaskID] = r.Task
		}
	}
	for n, dir := range in.dirs {
		if dd := doc[devDispatchDoc](b, dir+"/dispatch.json"); dd != nil {
			a.prepared[n] = dd
		}
	}
	for _, e := range b.Events {
		if n, ok := in.labels[e.Hop]; ok && e.Type == EvTaskAdmitted && e.Task != "" {
			if doc[devEnvelope[TaskPlan]](b, in.dirs[n]+"/plan.json") != nil {
				a.owned[e.Task] = n
			}
		}
	}
	if b.Repo != nil {
		a.objects = b.Repo.repo.Storer
		if h, ok := b.Repo.refs[in.mainRef]; ok && h.String() == in.ws.SeedCommit {
			a.main = in.ws.SeedCommit
		}
	}
	if sp := doc[DevSeedPush](b, "setup/seed-push.json"); sp != nil && sp.Workspace == in.ws.Name && sp.Instance == in.ws.Instance &&
		sp.Commit == in.ws.SeedCommit && sp.Ref == contract.DefaultBranchRef && a.main != "" {
		a.seed = sp
	}
	for p := 1; p <= maxPhases; p++ {
		_, launched := b.Files[fmt.Sprintf("setup/launch-phase-%d.json", p)]
		started := false
		for _, e := range b.Events {
			started = started || e.Type == EvPhaseStarted && e.Handle == phaseHandle(p)
		}
		if launched && started {
			a.lastPhase = p
		}
	}
	return a
}

// mcpPhaseRecords decodes the records of every coordinator phase stream.
func mcpPhaseRecords(b *DevBundle, phases int) map[int][]sessionRecord {
	out := map[int][]sessionRecord{}
	for p := 1; p <= phases; p++ {
		if data, ok := b.Files[devStreamFile(p)]; ok {
			out[p] = decodeDevStreamAs(devStreamFile(p), data, "").records
		}
	}
	return out
}

// devMCPProblems judges a full-flow bundle's recorded Callsheet calls
// (stopped: the manifest's stop corroborated by its attempt_failed event).
func devMCPProblems(b *DevBundle, m *DevManifest, stopped bool) map[int]string {
	return mcpVerdict(mcpPhaseRecords(b, maxPhases), devMCPAuthority(b, m, stopped))
}

// devMCPAuthority is a full-flow bundle's call authority.
func devMCPAuthority(b *DevBundle, m *DevManifest, stopped bool) *mcpAuthority {
	in := mcpAuthorityInput{mainRef: "refs/heads/seed", goos: m.GOOS, ws: m.Workspace, seedPath: m.Render["SEED_PATH"], stopped: stopped, dirs: map[int]string{},
		labels: map[string]int{}}
	for _, n := range b.ordinalsOf("tasks") {
		in.dirs[n] = taskDir(n)
		in.labels[workflowLabel(n)] = n
	}
	return buildMCPAuthority(b, in)
}
