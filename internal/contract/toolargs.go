package contract

import "time"

// Callsheet MCP tool arguments: the complete argument validation (decode
// and value checks) of the tools the development flow's coordinator uses,
// extracted from internal/mcp's handlers so the server and the offline
// verifier (design 12a-dev-flow-verify) judge a call by one rule. The
// handlers call these functions in their original order; nothing here
// contacts a plane.

// toolInvalid is the handlers' invalid_argument error for field.
func toolInvalid(field, format string, args ...any) error {
	return TaskError(CodeInvalidArgument, field, "", format, args...)
}

// toolInvalidTaskID is the handlers' invalid task ID error for field.
func toolInvalidTaskID(field string) error {
	return toolInvalid(field, "invalid task ID; want t_ followed by 32 lowercase hex digits")
}

// DispatchArgs are dispatch's arguments.
type DispatchArgs struct {
	Target struct {
		Kind  string `json:"kind"`
		Value string `json:"value"`
	} `json:"target"`
	Goal       string   `json:"goal"`
	Acceptance string   `json:"acceptance"`
	Payload    []string `json:"payload,omitempty"`
	Override   *struct {
		Model   *string `json:"model,omitempty"`
		Effort  *string `json:"effort,omitempty"`
		Timeout *string `json:"timeout,omitempty"`
	} `json:"override,omitempty"`
	Wait *string `json:"wait,omitempty"`
	// Iteration 10c: the optional workspace selection, forwarded unchanged
	// to the shared contract validator (null and duplicates are refused by
	// the strict decoder).
	Workspace         *string `json:"workspace,omitempty"`
	Base              *string `json:"base,omitempty"`
	WorkspaceInstance *string `json:"workspace_instance,omitempty"`
}

// DecodeDispatchArgs decodes dispatch's arguments by the tool-argument
// rule (the first step; the attribution and the request are checked next
// by Request).
func DecodeDispatchArgs(raw []byte) (DispatchArgs, error) {
	var a DispatchArgs
	err := DecodeToolArgs(raw, &a)
	return a, err
}

// Request builds the plane request with the session's attribution and
// validates it (attribution first) before anything is sent.
func (a DispatchArgs) Request(rb RequestedBy) (DispatchRequest, error) {
	if err := rb.Validate(); err != nil {
		return DispatchRequest{}, err
	}
	payload := a.Payload
	if payload == nil {
		payload = []string{}
	}
	req := DispatchRequest{Target: TaskTarget{Kind: a.Target.Kind, Value: a.Target.Value}, Goal: a.Goal, Payload: payload,
		Acceptance: a.Acceptance, RequestedBy: rb, Workspace: a.Workspace, Base: a.Base, WorkspaceInstance: a.WorkspaceInstance}
	if o := a.Override; o != nil {
		req.Override = &TaskOverride{Model: o.Model, Effort: o.Effort}
		if o.Timeout != nil {
			d, err := ParseRoleTimeout(*o.Timeout)
			if err != nil {
				return req, err
			}
			req.Override.Timeout = &d
		}
	}
	return req, req.Validate()
}

// WaitDuration is the explicit dispatch-with-wait duration (nil for an
// asynchronous dispatch).
func (a DispatchArgs) WaitDuration() (*time.Duration, error) {
	if a.Wait == nil {
		return nil, nil
	}
	d, err := ParseWaitDuration(*a.Wait)
	if err != nil {
		return nil, err
	}
	return &d, nil
}

// TaskListArgs are task_ls's arguments.
type TaskListArgs struct {
	After *string `json:"after,omitempty"`
	Limit *int    `json:"limit,omitempty"`
}

// ParseTaskListArgs validates task_ls's arguments and returns the page
// request: the exclusive after cursor ("" from the start) and the limit
// (DefaultTaskListLimit when omitted, else 1 to MaxTaskListLimit).
func ParseTaskListArgs(raw []byte) (after string, limit int, err error) {
	var a TaskListArgs
	if err := DecodeToolArgs(raw, &a); err != nil {
		return "", 0, err
	}
	after, limit = "", DefaultTaskListLimit
	if a.After != nil {
		if !ValidTaskID(*a.After) {
			return "", 0, toolInvalidTaskID("after")
		}
		after = *a.After
	}
	if a.Limit != nil {
		if *a.Limit < 1 || *a.Limit > MaxTaskListLimit {
			return "", 0, toolInvalid("limit", "limit must be an integer from 1 to %d", MaxTaskListLimit)
		}
		limit = *a.Limit
	}
	return after, limit, nil
}

// TaskShowArgs are task_show's arguments.
type TaskShowArgs struct {
	ID    string `json:"id"`
	Lines *int   `json:"lines,omitempty"`
}

// ParseTaskShowArgs validates task_show's arguments and returns the task
// ID and the tail lines (DefaultTailLines when omitted, else 0 to
// MaxTailLines).
func ParseTaskShowArgs(raw []byte) (id string, lines int, err error) {
	var a TaskShowArgs
	if err := DecodeToolArgs(raw, &a); err != nil {
		return "", 0, err
	}
	if !ValidTaskID(a.ID) {
		return "", 0, toolInvalidTaskID("id")
	}
	lines = DefaultTailLines
	if a.Lines != nil {
		if *a.Lines < 0 || *a.Lines > MaxTailLines {
			return "", 0, toolInvalid("lines", "lines must be an integer from 0 to %d", MaxTailLines)
		}
		lines = *a.Lines
	}
	return a.ID, lines, nil
}

// WsPushArgs are ws_push's arguments.
type WsPushArgs struct {
	Name     string  `json:"name"`
	Instance string  `json:"instance"`
	Branch   *string `json:"branch,omitempty"`
	Path     *string `json:"path,omitempty"`
}

// WsPush is a validated ws_push request: the branch ("" for main) and the
// local path (set false: the working directory).
type WsPush struct {
	Name, Instance, Branch, Path string
	PathSet                      bool
}

// ParseWsPushArgs validates ws_push's arguments for a server on goos.
func ParseWsPushArgs(raw []byte, goos string) (WsPush, error) {
	var a WsPushArgs
	if err := DecodeToolArgs(raw, &a); err != nil {
		return WsPush{}, err
	}
	branch := ""
	if a.Branch != nil {
		if *a.Branch == "" {
			return WsPush{}, toolInvalid("branch", "branch must not be empty; omit it for main")
		}
		branch = *a.Branch
	}
	if _, err := ValidatePush(a.Name, a.Instance, branch); err != nil {
		return WsPush{}, err
	}
	p := WsPush{Name: a.Name, Instance: a.Instance, Branch: branch}
	if a.Path != nil {
		if err := ValidateLocalPath(goos, *a.Path); err != nil {
			return WsPush{}, err
		}
		p.Path, p.PathSet = *a.Path, true
	}
	return p, nil
}

// ---- Node and role tools ----

// ParseNoArgs validates the arguments of a tool that takes none (node_ls,
// role_ls).
func ParseNoArgs(raw []byte) error {
	var a struct{}
	return DecodeToolArgs(raw, &a)
}

// ParseNodeShowArgs validates node_show's arguments.
func ParseNodeShowArgs(raw []byte) (string, error) {
	var a struct {
		ID string `json:"id"`
	}
	if err := DecodeToolArgs(raw, &a); err != nil {
		return "", err
	}
	if !ValidNodeID(a.ID) {
		return "", toolInvalid("id", "invalid node ID; want n_ followed by 32 lowercase hex digits")
	}
	return a.ID, nil
}

// toolInvalidRoleID is the role tools' invalid role ID error.
func toolInvalidRoleID() error {
	return toolInvalid("id", "invalid role ID; want a 1-63 character slug of lowercase letters, digits and internal hyphens")
}

// RoleAddArgs are role_add's arguments.
type RoleAddArgs struct {
	ID          string  `json:"id"`
	Name        string  `json:"name"`
	Node        string  `json:"node"`
	Adapter     string  `json:"adapter"`
	Instruction string  `json:"instruction"`
	Runbook     string  `json:"runbook"`
	Model       string  `json:"model"`
	Effort      string  `json:"effort"`
	Concurrency int     `json:"concurrency"`
	Timeout     *string `json:"timeout,omitempty"`
}

// ParseRoleAddArgs validates role_add's arguments against the registered
// adapters and returns the role configuration (an omitted timeout keeps
// HasTimeout false so the established default applies).
func ParseRoleAddArgs(raw []byte, lookup AdapterLookup) (RoleConfig, error) {
	var a RoleAddArgs
	if err := DecodeToolArgs(raw, &a); err != nil {
		return RoleConfig{}, err
	}
	rc := RoleConfig{ID: a.ID, Name: a.Name, Node: a.Node, Adapter: a.Adapter, Instruction: a.Instruction, Runbook: a.Runbook,
		Model: a.Model, Effort: a.Effort, Concurrency: a.Concurrency}
	if a.Timeout != nil {
		d, err := ParseRoleTimeout(*a.Timeout)
		if err != nil {
			return rc, err
		}
		rc.Timeout, rc.HasTimeout = d, true
	}
	return rc, ValidateRoleConfig(rc, lookup)
}

// RoleSetArgs are role_set's arguments.
type RoleSetArgs struct {
	ID          string  `json:"id"`
	Name        *string `json:"name,omitempty"`
	Adapter     *string `json:"adapter,omitempty"`
	Instruction *string `json:"instruction,omitempty"`
	Runbook     *string `json:"runbook,omitempty"`
	Model       *string `json:"model,omitempty"`
	Effort      *string `json:"effort,omitempty"`
	Concurrency *int    `json:"concurrency,omitempty"`
	Timeout     *string `json:"timeout,omitempty"`
}

// ParseRoleSetArgs validates role_set's arguments and returns the role ID
// and the patch of the present fields only (an explicit timeout 0 is
// kept; an empty patch is refused).
func ParseRoleSetArgs(raw []byte, lookup AdapterLookup) (string, RolePatch, error) {
	var a RoleSetArgs
	if err := DecodeToolArgs(raw, &a); err != nil {
		return "", RolePatch{}, err
	}
	if !ValidSlug(a.ID) {
		return "", RolePatch{}, toolInvalidRoleID()
	}
	p := RolePatch{Name: a.Name, Adapter: a.Adapter, Instruction: a.Instruction, Runbook: a.Runbook, Model: a.Model, Effort: a.Effort,
		Concurrency: a.Concurrency}
	if a.Timeout != nil {
		d, err := ParseRoleTimeout(*a.Timeout)
		if err != nil {
			return a.ID, p, err
		}
		p.Timeout = &d
	}
	if p.Empty() {
		return a.ID, p, toolInvalid("", "role_set needs at least one field to change (name, adapter, instruction, runbook, model, effort, concurrency or timeout)")
	}
	if err := ValidatePatch(p); err != nil {
		return a.ID, p, err
	}
	if p.Adapter != nil {
		if _, known := lookup(*p.Adapter); !known {
			return a.ID, p, RoleError(CodeInvalidArgument, a.ID, "", "adapter", "", "unknown adapter; registered adapters: claude, codex, cursor, fake, grok")
		}
	}
	return a.ID, p, nil
}

// ParseRoleShowArgs validates role_show's arguments.
func ParseRoleShowArgs(raw []byte) (string, error) {
	var a struct {
		ID string `json:"id"`
	}
	if err := DecodeToolArgs(raw, &a); err != nil {
		return "", err
	}
	if !ValidSlug(a.ID) {
		return "", toolInvalidRoleID()
	}
	return a.ID, nil
}

// RoleRemoveArgs are role_rm's arguments.
type RoleRemoveArgs struct {
	ID        string  `json:"id"`
	Force     *bool   `json:"force,omitempty"`
	Operation *string `json:"operation,omitempty"`
}

// ParseRoleRemoveArgs validates role_rm's arguments and returns the role
// ID, whether the removal is forced and its operation token ("" for
// none).
func ParseRoleRemoveArgs(raw []byte) (id string, force bool, op string, err error) {
	var a RoleRemoveArgs
	if err := DecodeToolArgs(raw, &a); err != nil {
		return "", false, "", err
	}
	if !ValidSlug(a.ID) {
		return "", false, "", toolInvalidRoleID()
	}
	force = a.Force != nil && *a.Force
	if a.Operation != nil {
		op = *a.Operation
		if err := ValidateRemovalOperation(force, op); err != nil {
			return "", false, "", err
		}
	}
	return a.ID, force, op, nil
}

// ---- Task tools ----

// TaskLogsArgs are task_logs's arguments.
type TaskLogsArgs struct {
	ID   string `json:"id"`
	Late *bool  `json:"late,omitempty"`
}

// ParseTaskLogsArgs validates task_logs's arguments and returns the task
// ID and whether the late result's output is asked for.
func ParseTaskLogsArgs(raw []byte) (string, bool, error) {
	var a TaskLogsArgs
	if err := DecodeToolArgs(raw, &a); err != nil {
		return "", false, err
	}
	if !ValidTaskID(a.ID) {
		return "", false, toolInvalidTaskID("id")
	}
	return a.ID, a.Late != nil && *a.Late, nil
}

// TaskCancelArgs are task_cancel's arguments.
type TaskCancelArgs struct {
	ID string `json:"id"`
}

// ParseTaskCancelArgs validates task_cancel's arguments.
func ParseTaskCancelArgs(raw []byte) (string, error) {
	var a TaskCancelArgs
	if err := DecodeToolArgs(raw, &a); err != nil {
		return "", err
	}
	if !ValidTaskID(a.ID) {
		return "", toolInvalidTaskID("id")
	}
	return a.ID, nil
}

// ParseTaskWaitArgs validates task_wait's arguments and returns the task
// IDs and the explicit wait (nil when omitted).
func ParseTaskWaitArgs(raw []byte) ([]string, *time.Duration, error) {
	var a struct {
		TaskIDs []string `json:"task_ids"`
		Wait    *string  `json:"wait,omitempty"`
	}
	if err := DecodeToolArgs(raw, &a); err != nil {
		return nil, nil, err
	}
	if err := ValidateWaitIDs(a.TaskIDs); err != nil {
		return nil, nil, err
	}
	if a.Wait == nil {
		return a.TaskIDs, nil, nil
	}
	d, err := ParseWaitDuration(*a.Wait)
	if err != nil {
		return nil, nil, err
	}
	return a.TaskIDs, &d, nil
}

// ---- Workspace tools ----

// ParseWsNameArgs validates the arguments of ws_create and ws_show.
func ParseWsNameArgs(raw []byte) (string, error) {
	var a struct {
		Name string `json:"name"`
	}
	if err := DecodeToolArgs(raw, &a); err != nil {
		return "", err
	}
	if !ValidWorkspaceName(a.Name) {
		return "", InvalidWorkspaceName()
	}
	return a.Name, nil
}

// ParseWsListArgs validates ws_ls's arguments and returns the page
// request.
func ParseWsListArgs(raw []byte) (after string, limit int, err error) {
	var a struct {
		After *string `json:"after,omitempty"`
		Limit *int    `json:"limit,omitempty"`
	}
	if err := DecodeToolArgs(raw, &a); err != nil {
		return "", 0, err
	}
	after, limit = "", DefaultWorkspaceLimit
	if a.After != nil {
		if *a.After == "" || len(*a.After) > 63 {
			return "", 0, toolInvalid("after", "after must be a workspace name of 1-63 bytes (the previous page's next_after)")
		}
		after = *a.After
	}
	if a.Limit != nil {
		if *a.Limit < 1 || *a.Limit > MaxWorkspaceLimit {
			return "", 0, toolInvalid("limit", "limit must be an integer from 1 to %d", MaxWorkspaceLimit)
		}
		limit = *a.Limit
	}
	return after, limit, nil
}

// ParseWsRemoveArgs validates ws_rm's arguments.
func ParseWsRemoveArgs(raw []byte) (name, instance string, err error) {
	var a struct {
		Name     string `json:"name"`
		Instance string `json:"instance"`
	}
	if err := DecodeToolArgs(raw, &a); err != nil {
		return "", "", err
	}
	if !ValidWorkspaceName(a.Name) {
		return "", "", InvalidWorkspaceName()
	}
	if err := ValidateInstance(a.Instance); err != nil {
		return "", "", err
	}
	return a.Name, a.Instance, nil
}

// ParseWsPruneArgs validates ws_prune's arguments.
func ParseWsPruneArgs(raw []byte) (name, instance, before string, err error) {
	var a struct {
		Name     string `json:"name"`
		Instance string `json:"instance"`
		Before   string `json:"before"`
	}
	if err := DecodeToolArgs(raw, &a); err != nil {
		return "", "", "", err
	}
	if !ValidWorkspaceName(a.Name) {
		return "", "", "", InvalidWorkspaceName()
	}
	if err := ValidateInstance(a.Instance); err != nil {
		return "", "", "", err
	}
	if _, err := ParseCutoff(a.Before); err != nil {
		return "", "", "", err
	}
	return a.Name, a.Instance, a.Before, nil
}

// ParseWsRefSetArgs validates ws_ref_set's arguments and returns the
// workspace name and the ref-set request.
func ParseWsRefSetArgs(raw []byte) (string, WorkspaceRefSetRequest, error) {
	var a struct {
		Name     string  `json:"name"`
		Instance string  `json:"instance"`
		Branch   string  `json:"branch"`
		Expected string  `json:"expected"`
		Target   *string `json:"target,omitempty"`
		Delete   *bool   `json:"delete,omitempty"`
	}
	if err := DecodeToolArgs(raw, &a); err != nil {
		return "", WorkspaceRefSetRequest{}, err
	}
	if !ValidWorkspaceName(a.Name) {
		return "", WorkspaceRefSetRequest{}, InvalidWorkspaceName()
	}
	req := WorkspaceRefSetRequest{Instance: a.Instance, Branch: a.Branch, Expected: a.Expected, Target: a.Target, Delete: a.Delete}
	if _, err := ValidateRefSet(req); err != nil {
		return "", WorkspaceRefSetRequest{}, err
	}
	return a.Name, req, nil
}

// WsFormArgs are ws_status's and ws_diff's flat arguments (iteration 10c):
// the union of the explicit form's keys and the task form's task_id.
// Exactly one form, its required keys and no mixed key are runtime rules
// (the flat schema does not express them).
type WsFormArgs struct {
	TaskID     *string `json:"task_id,omitempty"`
	Name       *string `json:"name,omitempty"`
	Base       *string `json:"base,omitempty"`
	Target     *string `json:"target,omitempty"`
	After      *string `json:"after,omitempty"`
	Instance   *string `json:"instance,omitempty"`
	Generation *string `json:"generation,omitempty"`
	Limit      *int    `json:"limit,omitempty"`
}

// form selects the accepted form: task (task_id, with paging only when
// paged) or explicit (name, and with diff base and target); a mixed or
// incomplete set is invalid_argument.
func (a WsFormArgs) form(tool string, diff bool) (bool, error) {
	if a.TaskID != nil {
		switch {
		case a.Name != nil || a.Base != nil || a.Target != nil:
			return false, toolInvalid("task_id", "%s takes either task_id or the explicit workspace arguments, never both", tool)
		case !diff && (a.After != nil || a.Instance != nil || a.Generation != nil || a.Limit != nil):
			return false, toolInvalid("task_id", "%s with task_id takes no paging arguments", tool)
		case !ValidTaskID(*a.TaskID):
			return false, toolInvalidTaskID("task_id")
		}
		return true, nil
	}
	switch {
	case a.Name == nil && diff:
		return false, toolInvalid("name", "%s needs task_id, or name with base and target", tool)
	case a.Name == nil:
		return false, toolInvalid("name", "%s needs task_id or name", tool)
	case diff && (a.Base == nil || a.Target == nil):
		return false, toolInvalid("base", "%s's explicit form needs name, base and target together", tool)
	case !diff && (a.Base != nil || a.Target != nil):
		return false, toolInvalid("base", "%s takes no base or target", tool)
	}
	return false, nil
}

// page returns the continuation and limit of paged workspace reads.
func (a WsFormArgs) page() (after, instance, generation string, limit int, err error) {
	str := func(p *string) string {
		if p == nil {
			return ""
		}
		return *p
	}
	after, instance, generation = str(a.After), str(a.Instance), str(a.Generation)
	if (a.After == nil) != (a.Instance == nil) || (a.After == nil) != (a.Generation == nil) {
		return "", "", "", 0, toolInvalid("after", "a continuation needs after, instance and generation together; the first page omits all three")
	}
	if a.After != nil && (!ValidWorkspaceToken(instance) || !ValidWorkspaceToken(generation) || after == "") {
		return "", "", "", 0, toolInvalid("after", "after, instance and generation must come from the previous page")
	}
	limit = DefaultWorkspaceLimit
	if a.Limit != nil {
		if *a.Limit < 1 || *a.Limit > MaxWorkspaceLimit {
			return "", "", "", 0, toolInvalid("limit", "limit must be an integer from 1 to %d", MaxWorkspaceLimit)
		}
		limit = *a.Limit
	}
	return after, instance, generation, limit, nil
}

// WsPage is a paged workspace read's continuation (all empty on the first
// page) and limit.
type WsPage struct {
	After, Instance, Generation string
	Limit                       int
}

// WsStatusCall is a validated ws_status call: the task form (TaskID set)
// or the explicit form's workspace and page.
type WsStatusCall struct {
	TaskID string
	Name   string
	Page   WsPage
}

// ParseWsStatusArgs validates ws_status's arguments.
func ParseWsStatusArgs(raw []byte) (WsStatusCall, error) {
	var a WsFormArgs
	if err := DecodeToolArgs(raw, &a); err != nil {
		return WsStatusCall{}, err
	}
	task, err := a.form("ws_status", false)
	if err != nil {
		return WsStatusCall{}, err
	}
	if task {
		return WsStatusCall{TaskID: *a.TaskID}, nil
	}
	if !ValidWorkspaceName(*a.Name) {
		return WsStatusCall{}, InvalidWorkspaceName()
	}
	after, instance, generation, limit, err := a.page()
	if err != nil {
		return WsStatusCall{}, err
	}
	if after != "" && !ValidStatusCursor(after) {
		return WsStatusCall{}, toolInvalid("after", "after must be a complete portable branch ref (refs/heads/...) or task ref (refs/callsheet/tasks/...) from the previous page")
	}
	return WsStatusCall{Name: *a.Name, Page: WsPage{After: after, Instance: instance, Generation: generation, Limit: limit}}, nil
}

// WsDiffCall is a validated ws_diff call: the task form (TaskID set) or
// the explicit form's workspace and canonical base and target selectors,
// with the page (its decoded path cursor in AfterPath).
type WsDiffCall struct {
	TaskID       string
	Name         string
	Base, Target string
	Page         WsPage
	AfterPath    []byte
}

// ParseWsDiffArgs validates ws_diff's arguments.
func ParseWsDiffArgs(raw []byte) (WsDiffCall, error) {
	var a WsFormArgs
	if err := DecodeToolArgs(raw, &a); err != nil {
		return WsDiffCall{}, err
	}
	task, err := a.form("ws_diff", true)
	if err != nil {
		return WsDiffCall{}, err
	}
	if !task && !ValidWorkspaceName(*a.Name) {
		return WsDiffCall{}, InvalidWorkspaceName()
	}
	after, instance, generation, limit, err := a.page()
	if err != nil {
		return WsDiffCall{}, err
	}
	call := WsDiffCall{Page: WsPage{After: after, Instance: instance, Generation: generation, Limit: limit}}
	if after != "" {
		if call.AfterPath, err = ParsePathCursor(after); err != nil {
			return WsDiffCall{}, err
		}
	}
	if task {
		call.TaskID = *a.TaskID
		return call, nil
	}
	// A bare task ID as base or target means its full task ref.
	base, err := ParseTaskSelector(*a.Base, "base", true)
	if err != nil {
		return WsDiffCall{}, err
	}
	target, err := ParseTaskSelector(*a.Target, "target", false)
	if err != nil {
		return WsDiffCall{}, err
	}
	call.Name, call.Base, call.Target = *a.Name, SelectorString(base), SelectorString(target)
	return call, nil
}

// WsPullArgs are ws_pull's flat arguments (iteration 10c): the explicit
// form {name, ref, path?} or the task form {task_id, path?}; exactly one
// form and no mixed key are runtime rules.
type WsPullArgs struct {
	TaskID *string `json:"task_id,omitempty"`
	Name   *string `json:"name,omitempty"`
	Ref    *string `json:"ref,omitempty"`
	Path   *string `json:"path,omitempty"`
}

// WsPull is a validated ws_pull call: the task form (TaskID set) or the
// explicit form's workspace and canonical ref, and the local path (set
// false: the working directory).
type WsPull struct {
	TaskID, Name, Ref, Path string
	PathSet                 bool
}

// ParseWsPullArgs validates ws_pull's arguments for a server on goos.
func ParseWsPullArgs(raw []byte, goos string) (WsPull, error) {
	var a WsPullArgs
	if err := DecodeToolArgs(raw, &a); err != nil {
		return WsPull{}, err
	}
	var p WsPull
	switch {
	case a.TaskID != nil && (a.Name != nil || a.Ref != nil):
		return WsPull{}, toolInvalid("task_id", "ws_pull takes either task_id or name and ref, never both")
	case a.TaskID != nil && !ValidTaskID(*a.TaskID):
		return WsPull{}, toolInvalidTaskID("task_id")
	case a.TaskID != nil:
		p.TaskID = *a.TaskID
	case a.Name == nil || a.Ref == nil:
		return WsPull{}, toolInvalid("name", "ws_pull needs task_id, or name and ref together")
	default:
		if !ValidWorkspaceName(*a.Name) {
			return WsPull{}, InvalidWorkspaceName()
		}
		// A bare task ID means its full task ref (refs/heads/t_... escapes to
		// a branch); the unchanged ValidatePull then checks the result.
		sel, err := ParseTaskSelector(*a.Ref, "ref", false)
		if err != nil {
			return WsPull{}, err
		}
		p.Name, p.Ref = *a.Name, SelectorString(sel)
		if _, err := ValidatePull(p.Name, p.Ref); err != nil {
			return WsPull{}, err
		}
	}
	if a.Path != nil {
		if err := ValidateLocalPath(goos, *a.Path); err != nil {
			return WsPull{}, err
		}
		p.Path, p.PathSet = *a.Path, true
	}
	return p, nil
}
