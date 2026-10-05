package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"strconv"
	"strings"
	"time"

	"github.com/wedevwork/callsheet/internal/adapter"
	"github.com/wedevwork/callsheet/internal/client"
	"github.com/wedevwork/callsheet/internal/contract"
)

// The thirteen operator tools (FP-3 to FP-7), in the order of tools.md.
// Each translates typed, strictly decoded arguments to one operation of
// the verified plane client and returns the existing serialized contract
// response type as compact JSON text. Nothing is cached, nothing is
// retried at this layer, and the MCP host alone approves tool use.
const (
	toolNodeLs     = "node_ls"
	toolNodeShow   = "node_show"
	toolRoleAdd    = "role_add"
	toolRoleSet    = "role_set"
	toolRoleLs     = "role_ls"
	toolRoleShow   = "role_show"
	toolRoleRm     = "role_rm"
	toolDispatch   = "dispatch"
	toolTaskLs     = "task_ls"
	toolTaskShow   = "task_show"
	toolTaskLogs   = "task_logs"
	toolTaskCancel = "task_cancel"
	toolTaskWait   = "task_wait"
	// Iteration 09a: the workspace hub tools.
	toolWsCreate = "ws_create"
	toolWsLs     = "ws_ls"
	toolWsShow   = "ws_show"
	toolWsRm     = "ws_rm"
	toolWsPrune  = "ws_prune"
	toolWsRefSet = "ws_ref_set"
	toolWsStatus = "ws_status"
	toolWsDiff   = "ws_diff"
	// Iteration 09b: the local transfer tools.
	toolWsPush = "ws_push"
	toolWsPull = "ws_pull"
)

// ToolNames lists the tools in their fixed discovery order.
var ToolNames = []string{toolNodeLs, toolNodeShow, toolRoleAdd, toolRoleSet, toolRoleLs, toolRoleShow, toolRoleRm,
	toolDispatch, toolTaskLs, toolTaskShow, toolTaskLogs, toolTaskCancel, toolTaskWait,
	toolWsCreate, toolWsLs, toolWsShow, toolWsRm, toolWsPrune, toolWsRefSet, toolWsStatus, toolWsDiff,
	toolWsPush, toolWsPull}

// Client is the narrow plane client the tools use; *client.Client
// implements it.
type Client interface {
	ListNodes(ctx context.Context) ([]contract.Node, error)
	ShowNode(ctx context.Context, id string) (contract.Node, error)
	AddRole(ctx context.Context, rc contract.RoleConfig) (contract.RoleView, error)
	SetRole(ctx context.Context, id string, p contract.RolePatch) (contract.RoleView, error)
	ListRoles(ctx context.Context) ([]contract.RoleView, error)
	ShowRole(ctx context.Context, id string) (contract.RoleView, error)
	RemoveRole(ctx context.Context, id string, force bool, operationID string) (contract.RoleRemoveResult, error)
	Dispatch(ctx context.Context, req contract.DispatchRequest) (contract.TaskView, error)
	DispatchWithWait(ctx context.Context, req contract.DispatchRequest, wait time.Duration) (contract.DispatchResponse, error)
	ListTasks(ctx context.Context, after string, limit int) ([]contract.TaskSummary, *string, error)
	ShowTask(ctx context.Context, id string, lines int) (contract.TaskView, error)
	TaskLogs(ctx context.Context, id string) (contract.TaskLogsResponse, error)
	TaskLateLogs(ctx context.Context, id string) (contract.TaskLogsResponse, error)
	CancelTask(ctx context.Context, id string) (contract.CancelResponse, error)
	WaitTasks(ctx context.Context, ids []string, wait time.Duration) (contract.WaitResponse, error)
	CreateWorkspace(ctx context.Context, name string) (contract.WorkspaceView, error)
	ListWorkspaces(ctx context.Context, after string, limit int) (contract.WorkspaceListResponse, error)
	ShowWorkspace(ctx context.Context, name string) (contract.WorkspaceView, error)
	RemoveWorkspace(ctx context.Context, name, instance string) (contract.WorkspaceRemoveResponse, error)
	PruneWorkspace(ctx context.Context, name, instance, before string) (contract.WorkspacePruneResponse, error)
	SetWorkspaceRef(ctx context.Context, name string, req contract.WorkspaceRefSetRequest) (contract.WorkspaceRefSetResponse, error)
	WorkspaceStatus(ctx context.Context, name string, p client.StatusPage) (contract.WorkspaceStatusResponse, error)
	WorkspaceDiff(ctx context.Context, name string, p client.DiffPage) (contract.WorkspaceDiffResponse, error)
	// Iteration 10c: the task-ID forms of ws_status and ws_diff.
	TaskWorkspaceStatus(ctx context.Context, id string) (contract.TaskWorkspaceStatusResponse, error)
	TaskWorkspaceDiff(ctx context.Context, id string, p client.TaskDiffPage) (contract.WorkspaceDiffResponse, error)
	Close()
}

var _ Client = (*client.Client)(nil)

// Factory returns a fresh verified client for one tool call; trust is
// resolved with the call's context.
type Factory func(ctx context.Context) (Client, error)

// PlaneFactory is the production factory: for every call it resolves
// trust afresh (client.ResolveTrust: the CA file is loaded or the pinned
// CA fetched again, and the plane's TLS verified) and builds a new
// verified client. Only the configured URL and trust input persist.
func PlaneFactory(planeURL, caFile, fingerprint string) Factory {
	return func(ctx context.Context) (Client, error) {
		trust, err := client.ResolveTrust(ctx, client.TrustOptions{PlaneURL: planeURL, CAFile: caFile, CAFingerprint: fingerprint})
		if err != nil {
			return nil, err
		}
		c, err := client.New(planeURL, trust)
		if err != nil {
			return nil, err
		}
		return c, nil
	}
}

// toolResult is a tool's successful logical JSON and, for a
// still_running wait answer, its entire wire bound (0: the global bound).
type toolResult struct {
	text      []byte
	wireLimit int
}

// tool is one static tool definition.
type tool struct {
	name        string
	description string
	annotations map[string]any
	run         func(c *call, args json.RawMessage) (toolResult, error)
}

// Common description sentences.
const (
	freshNote     = " Results reflect the plane at the time of this call (nothing is cached) and may contain untrusted worker text."
	readOnlyNote  = " Read-only."
	mutationNote  = " Mutation: the MCP host approves its use."
	admissionNote = " Admission or cancel acceptance is not task success."
)

func readOnly() map[string]any { return map[string]any{"readOnlyHint": true, "openWorldHint": false} }

func mutation(destructive, idempotent bool) map[string]any {
	return map[string]any{"readOnlyHint": false, "destructiveHint": destructive, "idempotentHint": idempotent, "openWorldHint": false}
}

// newTools builds the twenty-three definitions for budget b (thirteen
// operator tools, iteration 07a, eight workspace tools, iteration 09a,
// and two local transfer tools, iteration 09b).
func newTools(b Budget) []*tool {
	budgetNote := " This server's call budget is B=" + b.String() + ". " + BudgetDeferralNote + " " + UnverifiedNotice
	tools := []*tool{
		{name: toolNodeLs, annotations: readOnly(), run: runNodeLs,
			description: "List every node the plane knows, sorted by ID: the same global roster as callsheet node ls --json (liveness, last_seen, versions and roles)." + readOnlyNote + freshNote},
		{name: toolNodeShow, annotations: readOnly(), run: runNodeShow,
			description: "Show one node by ID." + readOnlyNote + freshNote},
		{name: toolRoleAdd, annotations: mutation(false, false), run: runRoleAdd,
			description: "Register a role on the plane. The plane asks the node's sidecar to check the manuals and the adapter; an offline node, an unreadable manual or a disabled adapter rejects it and nothing is recorded. Role IDs are unique: repeating an add conflicts; after an ambiguous answer read role_show." + mutationNote + freshNote},
		{name: toolRoleSet, annotations: mutation(true, false), run: runRoleSet,
			description: "Change the given fields of a role; omitted fields keep their values and at least one is required. The ID, node and registration order never change; the node must be online; the change applies to future work only." + mutationNote + freshNote},
		{name: toolRoleLs, annotations: readOnly(), run: runRoleLs,
			description: "List every role sorted by name, then registration order, with readiness (can_accept), inflight and removing fields." + readOnlyNote + freshNote},
		{name: toolRoleShow, annotations: readOnly(), run: runRoleShow,
			description: "Show one role, including a pending forced removal's operation token and instance (removal)." + readOnlyNote + freshNote},
		{name: toolRoleRm, annotations: mutation(true, false), run: runRoleRm,
			description: "Remove a role. Without force a role with tasks in flight is refused (conflict, tasks_inflight). With force the plane fences this role instance, cancels its tasks and removes it once each ended durably; when that takes longer than the plane's budget the answer is a pending removal with an operation token (a success, not a completed deletion): call role_rm again with force and that operation. Nothing is polled or retried automatically. After an ambiguous answer read role_show first; never repeat a bare force against a possibly reused role ID. Recreating a role is not proof that old execution stopped." + mutationNote + freshNote},
		{name: toolDispatch, annotations: mutation(false, false), run: runDispatch,
			description: "Dispatch a task to a role: the plane reserves one of the role's slots atomically or fails at once listing every candidate (no queue, no reroute). With workspace the task runs in a private checkout of that workspace's base (default main; a branch, full hash, complete task ref, task ID or empty) and publishes its result as refs/callsheet/tasks/TASK_ID; the base commit and instance are fixed at admission (workspace_binding), an empty workspace needs base empty, and nothing moves a branch. To continue from task A, dispatch with A's result.workspace.commit as base and its workspace_binding name and instance as workspace and workspace_instance. Without wait the answer is the admitted task. With wait (0 to 5m) the same single call also waits for the task to end, capped by the call budget (at most B minus the reserves and a 6s admission allowance): the terminal task or its compact still_running row; follow with task_wait. A lost answer may hide an admitted task: inspect task_ls before dispatching again (never retried). requested_by is this MCP client's self-reported clientInfo name and version plus the coordinator hostname, not an authenticated identity." + admissionNote + mutationNote + freshNote + budgetNote},
		{name: toolTaskLs, annotations: readOnly(), run: runTaskLs,
			description: "List tasks from every coordinator ordered by task ID, one page per call (limit 1-100, default 100; after: the previous page's next_after); all pages are never fetched automatically." + readOnlyNote + freshNote},
		{name: toolTaskShow, annotations: readOnly(), run: runTaskShow,
			description: "Show one task: request, attribution, role, effective settings, state, timestamps, result, late result summary, durability flags and the last lines of output (lines 0-200, default 20). A failed, rejected, lost, cancelled or timed_out task is a successful observation." + readOnlyNote + freshNote},
		{name: toolTaskLogs, annotations: readOnly(), run: runTaskLogs,
			description: "Return a task's complete retained output (up to 10 MiB, base64 in data, byte exact) or, with late, its late result's separate output (none is not_found). The answer can be large: for routine monitoring use task_show or task_wait instead." + readOnlyNote + freshNote},
		{name: toolTaskCancel, annotations: mutation(true, true), run: runTaskCancel,
			description: "Ask the plane to cancel a task. accepted=true means a durable stop request, accepted=false that the task was already durably terminal; neither proves that a pending cancellation finished: follow with task_wait. Repeating it is safe; this server never retries it." + admissionNote + mutationNote + freshNote},
		{name: toolTaskWait, annotations: readOnly(), run: runTaskWait,
			description: "Wait for the first of 1 to 16 tasks to end durably: terminal with the winner's view, or still_running with one compact row per task in the given order. The plane may answer sooner (its own wait cap). A failed task is a successful observation. Repeat task_wait after still_running." + readOnlyNote + freshNote + budgetNote},
	}
	return append(append(tools, workspaceTools()...), transferTools()...)
}

// workspaceTools are the eight workspace hub tools (iteration 09a). They
// operate on the plane's stored workspaces only: none reads or writes a
// coordinator file, and no argument or result carries file content,
// patches, blobs, repository configuration or shell commands.
func workspaceTools() []*tool {
	const plane = " This tool operates on the plane's stored workspace (a bare git repository on the plane), never on a local repository, working tree or file of this coordinator."
	const pinned = " Every workspace-specific call names its workspace explicitly; there is no default workspace."
	return []*tool{
		{name: toolWsCreate, annotations: mutation(false, false), run: runWsCreate,
			description: "Create an empty, durable workspace (no commit; main is unborn) and return its identity, generation, size and retention. Names are unique: repeating a create conflicts; after an ambiguous answer read ws_show." + plane + mutationNote + freshNote},
		{name: toolWsLs, annotations: readOnly(), run: runWsLs,
			description: "List workspaces sorted by name, one page per call (limit 1-100, default 100; after: the previous page's next_after). Each page is a fresh observation, not a snapshot across concurrent create and remove; all pages are never fetched automatically." + plane + readOnlyNote + freshNote},
		{name: toolWsShow, annotations: readOnly(), run: runWsShow,
			description: "Show a workspace's name, instance token (required by ws_rm, ws_prune and ws_ref_set), creation time, current generation, default branch, retention and size_bytes (the logical size of every regular file the plane owns for it)." + plane + pinned + readOnlyNote + freshNote},
		{name: toolWsRm, annotations: mutation(true, false), run: runWsRm,
			description: "Remove a whole workspace of the given instance after its current readers and writer finish; a stale instance never removes a recreated name. A lost answer may still mean the name disappeared: read ws_ls before acting again (never retried)." + plane + pinned + mutationNote + freshNote},
		{name: toolWsPrune, annotations: mutation(true, false), run: runWsPrune,
			description: "Prune task refs whose plane publication time is strictly before the RFC3339 cutoff (a zone is required) and collect every object no surviving ref reaches; branches and their history are always kept. Returns counts and sizes, never ref lists." + plane + pinned + mutationNote + freshNote},
		{name: toolWsRefSet, annotations: mutation(false, false), run: runWsRefSet,
			description: "Create, move or delete one branch with compare-and-swap: expected is the branch's current full hash, or absent to create it; exactly one of target (a reachable full commit hash, a branch or a complete refs/callsheet/tasks/ ref) and delete=true. No merge or fast-forward requirement; no other branch moves; a stale expected conflicts. Branch names are byte-exact lowercase ASCII (never repaired)." + plane + pinned + mutationNote + freshNote},
		{name: toolWsStatus, annotations: readOnly(), run: runWsStatus,
			description: "Two forms (exactly one). {name, ...}: return one page of a workspace's stored refs on the plane sorted by name (task refs with their publication time, branches with published_at null; an unborn main has no row); continue with after, instance and generation from the previous page; a changed generation conflicts (restart). {task_id}: return that task's workspace status {version, workspace:{task_id, state, workspace_phase, binding, result, available}}: available is whether the current instance still holds the exact result ref and hash (false after a prune, removal or recreation); a scratch task is invalid_argument (no_task_workspace). It never inspects a local working tree." + plane + readOnlyNote + freshNote},
		{name: toolWsDiff, annotations: readOnly(), run: runWsDiff,
			description: "Two forms (exactly one). {name, base, target, ...}: compare two stored snapshots (base, or empty for the empty tree, and target; a bare task ID means its task ref). {task_id, ...}: compare that task's admitted base (or the empty tree) with its published result commit, rechecking the bound instance and exact result ref on every page (a removed or recreated workspace conflicts, a pruned result is not_found). Returns one page of changed-path metadata only: path_base64 (raw path bytes, never contents), kind, modes and blob sizes. No file content, patch or commit message is returned. Continue with after, instance and generation (the explicit form with the returned full hashes as base and target); a changed generation conflicts: restart from the first page." + plane + readOnlyNote + freshNote},
	}
}

// wsGuidance is appended to a workspace mutation's lost answer.
const wsGuidance = "the change may have taken effect: inspect with ws_show or ws_status before repeating it (never retried)"

func invalidWsName() error { return contract.InvalidWorkspaceName() }

type wsNameArgs struct {
	Name string `json:"name"`
}

func runWsCreate(c *call, raw json.RawMessage) (toolResult, error) {
	var a wsNameArgs
	if err := decodeArgs(raw, &a); err != nil {
		return toolResult{}, err
	}
	if !contract.ValidWorkspaceName(a.Name) {
		return toolResult{}, invalidWsName()
	}
	cl, err := c.client()
	if err != nil {
		return toolResult{}, err
	}
	v, err := cl.CreateWorkspace(c.opCtx, a.Name)
	if err != nil {
		return toolResult{}, withGuidance(err, wsGuidance)
	}
	return encodeJSON(v)
}

func runWsLs(c *call, raw json.RawMessage) (toolResult, error) {
	var a struct {
		After *string `json:"after,omitempty"`
		Limit *int    `json:"limit,omitempty"`
	}
	if err := decodeArgs(raw, &a); err != nil {
		return toolResult{}, err
	}
	after, limit := "", contract.DefaultWorkspaceLimit
	if a.After != nil {
		if *a.After == "" || len(*a.After) > 63 {
			return toolResult{}, invalid("after", "after must be a workspace name of 1-63 bytes (the previous page's next_after)")
		}
		after = *a.After
	}
	if a.Limit != nil {
		if *a.Limit < 1 || *a.Limit > contract.MaxWorkspaceLimit {
			return toolResult{}, invalid("limit", "limit must be an integer from 1 to %d", contract.MaxWorkspaceLimit)
		}
		limit = *a.Limit
	}
	cl, err := c.client()
	if err != nil {
		return toolResult{}, err
	}
	r, err := cl.ListWorkspaces(c.opCtx, after, limit)
	if err != nil {
		return toolResult{}, err
	}
	return encodeJSON(r)
}

func runWsShow(c *call, raw json.RawMessage) (toolResult, error) {
	var a wsNameArgs
	if err := decodeArgs(raw, &a); err != nil {
		return toolResult{}, err
	}
	if !contract.ValidWorkspaceName(a.Name) {
		return toolResult{}, invalidWsName()
	}
	cl, err := c.client()
	if err != nil {
		return toolResult{}, err
	}
	v, err := cl.ShowWorkspace(c.opCtx, a.Name)
	if err != nil {
		return toolResult{}, err
	}
	return encodeJSON(v)
}

func runWsRm(c *call, raw json.RawMessage) (toolResult, error) {
	var a struct {
		Name     string `json:"name"`
		Instance string `json:"instance"`
	}
	if err := decodeArgs(raw, &a); err != nil {
		return toolResult{}, err
	}
	if !contract.ValidWorkspaceName(a.Name) {
		return toolResult{}, invalidWsName()
	}
	if err := contract.ValidateInstance(a.Instance); err != nil {
		return toolResult{}, err
	}
	cl, err := c.client()
	if err != nil {
		return toolResult{}, err
	}
	r, err := cl.RemoveWorkspace(c.opCtx, a.Name, a.Instance)
	if err != nil {
		return toolResult{}, withGuidance(err, wsGuidance)
	}
	return encodeJSON(r)
}

func runWsPrune(c *call, raw json.RawMessage) (toolResult, error) {
	var a struct {
		Name     string `json:"name"`
		Instance string `json:"instance"`
		Before   string `json:"before"`
	}
	if err := decodeArgs(raw, &a); err != nil {
		return toolResult{}, err
	}
	if !contract.ValidWorkspaceName(a.Name) {
		return toolResult{}, invalidWsName()
	}
	if err := contract.ValidateInstance(a.Instance); err != nil {
		return toolResult{}, err
	}
	if _, err := contract.ParseCutoff(a.Before); err != nil {
		return toolResult{}, err
	}
	cl, err := c.client()
	if err != nil {
		return toolResult{}, err
	}
	r, err := cl.PruneWorkspace(c.opCtx, a.Name, a.Instance, a.Before)
	if err != nil {
		return toolResult{}, withGuidance(err, wsGuidance)
	}
	return encodeJSON(r)
}

func runWsRefSet(c *call, raw json.RawMessage) (toolResult, error) {
	var a struct {
		Name     string  `json:"name"`
		Instance string  `json:"instance"`
		Branch   string  `json:"branch"`
		Expected string  `json:"expected"`
		Target   *string `json:"target,omitempty"`
		Delete   *bool   `json:"delete,omitempty"`
	}
	if err := decodeArgs(raw, &a); err != nil {
		return toolResult{}, err
	}
	if !contract.ValidWorkspaceName(a.Name) {
		return toolResult{}, invalidWsName()
	}
	req := contract.WorkspaceRefSetRequest{Instance: a.Instance, Branch: a.Branch, Expected: a.Expected, Target: a.Target, Delete: a.Delete}
	if _, err := contract.ValidateRefSet(req); err != nil {
		return toolResult{}, err
	}
	cl, err := c.client()
	if err != nil {
		return toolResult{}, err
	}
	r, err := cl.SetWorkspaceRef(c.opCtx, a.Name, req)
	if err != nil {
		return toolResult{}, withGuidance(err, wsGuidance)
	}
	return encodeJSON(r)
}

type wsPageArgs struct {
	After      *string `json:"after,omitempty"`
	Instance   *string `json:"instance,omitempty"`
	Generation *string `json:"generation,omitempty"`
	Limit      *int    `json:"limit,omitempty"`
}

// page returns the continuation and limit of paged workspace reads.
func (a wsPageArgs) page() (after, instance, generation string, limit int, err error) {
	str := func(p *string) string {
		if p == nil {
			return ""
		}
		return *p
	}
	after, instance, generation = str(a.After), str(a.Instance), str(a.Generation)
	if (a.After == nil) != (a.Instance == nil) || (a.After == nil) != (a.Generation == nil) {
		return "", "", "", 0, invalid("after", "a continuation needs after, instance and generation together; the first page omits all three")
	}
	if a.After != nil && (!contract.ValidWorkspaceToken(instance) || !contract.ValidWorkspaceToken(generation) || after == "") {
		return "", "", "", 0, invalid("after", "after, instance and generation must come from the previous page")
	}
	limit = contract.DefaultWorkspaceLimit
	if a.Limit != nil {
		if *a.Limit < 1 || *a.Limit > contract.MaxWorkspaceLimit {
			return "", "", "", 0, invalid("limit", "limit must be an integer from 1 to %d", contract.MaxWorkspaceLimit)
		}
		limit = *a.Limit
	}
	return after, instance, generation, limit, nil
}

// wsFormArgs are ws_status's and ws_diff's flat arguments (iteration 10c):
// the union of the explicit form's keys and the task form's task_id.
// Exactly one form, its required keys and no mixed key are runtime rules
// (the flat schema does not express them).
type wsFormArgs struct {
	TaskID     *string `json:"task_id,omitempty"`
	Name       *string `json:"name,omitempty"`
	Base       *string `json:"base,omitempty"`
	Target     *string `json:"target,omitempty"`
	After      *string `json:"after,omitempty"`
	Instance   *string `json:"instance,omitempty"`
	Generation *string `json:"generation,omitempty"`
	Limit      *int    `json:"limit,omitempty"`
}

func (a wsFormArgs) paging() wsPageArgs {
	return wsPageArgs{After: a.After, Instance: a.Instance, Generation: a.Generation, Limit: a.Limit}
}

// form selects the accepted form: task (task_id, with paging only when
// paged) or explicit (name, and with diff base and target); a mixed or
// incomplete set is invalid_argument.
func (a wsFormArgs) form(tool string, diff bool) (bool, error) {
	if a.TaskID != nil {
		switch {
		case a.Name != nil || a.Base != nil || a.Target != nil:
			return false, invalid("task_id", "%s takes either task_id or the explicit workspace arguments, never both", tool)
		case !diff && (a.After != nil || a.Instance != nil || a.Generation != nil || a.Limit != nil):
			return false, invalid("task_id", "%s with task_id takes no paging arguments", tool)
		case !contract.ValidTaskID(*a.TaskID):
			return false, invalidTaskID("task_id")
		}
		return true, nil
	}
	switch {
	case a.Name == nil && diff:
		return false, invalid("name", "%s needs task_id, or name with base and target", tool)
	case a.Name == nil:
		return false, invalid("name", "%s needs task_id or name", tool)
	case diff && (a.Base == nil || a.Target == nil):
		return false, invalid("base", "%s's explicit form needs name, base and target together", tool)
	case !diff && (a.Base != nil || a.Target != nil):
		return false, invalid("base", "%s takes no base or target", tool)
	}
	return false, nil
}

func runWsStatus(c *call, raw json.RawMessage) (toolResult, error) {
	var a wsFormArgs
	if err := decodeArgs(raw, &a); err != nil {
		return toolResult{}, err
	}
	task, err := a.form(toolWsStatus, false)
	if err != nil {
		return toolResult{}, err
	}
	if task {
		cl, err := c.client()
		if err != nil {
			return toolResult{}, err
		}
		r, err := cl.TaskWorkspaceStatus(c.opCtx, *a.TaskID)
		if err != nil {
			return toolResult{}, err
		}
		return encodeJSON(r)
	}
	name, p := *a.Name, a.paging()
	if !contract.ValidWorkspaceName(name) {
		return toolResult{}, invalidWsName()
	}
	after, instance, generation, limit, err := p.page()
	if err != nil {
		return toolResult{}, err
	}
	if after != "" && !contract.ValidStatusCursor(after) {
		return toolResult{}, invalid("after", "after must be a complete portable branch ref (refs/heads/...) or task ref (refs/callsheet/tasks/...) from the previous page")
	}
	cl, err := c.client()
	if err != nil {
		return toolResult{}, err
	}
	r, err := cl.WorkspaceStatus(c.opCtx, name, client.StatusPage{After: after, Instance: instance, Generation: generation, Limit: limit})
	if err != nil {
		return toolResult{}, err
	}
	return encodeJSON(r)
}

func runWsDiff(c *call, raw json.RawMessage) (toolResult, error) {
	var a wsFormArgs
	if err := decodeArgs(raw, &a); err != nil {
		return toolResult{}, err
	}
	task, err := a.form(toolWsDiff, true)
	if err != nil {
		return toolResult{}, err
	}
	if !task && !contract.ValidWorkspaceName(*a.Name) {
		return toolResult{}, invalidWsName()
	}
	after, instance, generation, limit, err := a.paging().page()
	if err != nil {
		return toolResult{}, err
	}
	if after != "" {
		if _, err := contract.ParsePathCursor(after); err != nil {
			return toolResult{}, err
		}
	}
	if task {
		cl, err := c.client()
		if err != nil {
			return toolResult{}, err
		}
		r, err := cl.TaskWorkspaceDiff(c.opCtx, *a.TaskID, client.TaskDiffPage{After: after, Instance: instance, Generation: generation, Limit: limit})
		if err != nil {
			return toolResult{}, err
		}
		return encodeJSON(r)
	}
	// A bare task ID as base or target means its full task ref.
	base, err := contract.ParseTaskSelector(*a.Base, "base", true)
	if err != nil {
		return toolResult{}, err
	}
	target, err := contract.ParseTaskSelector(*a.Target, "target", false)
	if err != nil {
		return toolResult{}, err
	}
	cl, err := c.client()
	if err != nil {
		return toolResult{}, err
	}
	r, err := cl.WorkspaceDiff(c.opCtx, *a.Name, client.DiffPage{Base: contract.SelectorString(base), Target: contract.SelectorString(target), After: after,
		Instance: instance, Generation: generation, Limit: limit})
	if err != nil {
		return toolResult{}, err
	}
	return encodeJSON(r)
}

// ---- Argument decoding ----

// decodeArgs strictly decodes a tool's arguments object into dto: no
// null anywhere (optional means absent), no unknown or duplicate member,
// exact integer, boolean, string and array types.
func decodeArgs(raw json.RawMessage, dto any) error {
	if len(bytes.TrimSpace(raw)) == 0 {
		raw = json.RawMessage("{}")
	}
	if containsNull(raw) {
		return contract.New(contract.CodeInvalidArgument, "tool arguments must not contain null: omit an optional argument instead")
	}
	return contract.DecodeStrict(raw, dto, "tool arguments")
}

func invalid(field, format string, args ...any) error {
	return contract.TaskError(contract.CodeInvalidArgument, field, "", format, args...)
}

func invalidTaskID(field string) error {
	return invalid(field, "invalid task ID; want t_ followed by 32 lowercase hex digits")
}

func invalidRoleID() error {
	return invalid("id", "invalid role ID; want a 1-63 character slug of lowercase letters, digits and internal hyphens")
}

// parseTimeout parses a role or task timeout argument.
func parseTimeout(s string) (time.Duration, error) { return contract.ParseRoleTimeout(s) }

// encodeJSON renders v compactly with the contract encoder.
func encodeJSON(v any) (toolResult, error) {
	b, err := contract.Encode(v)
	if err != nil {
		return toolResult{}, contract.Wrap(contract.CodeInternal, fixedInternal, err)
	}
	return toolResult{text: b}, nil
}

// ---- Node tools (FP-3) ----

func runNodeLs(c *call, raw json.RawMessage) (toolResult, error) {
	var a struct{}
	if err := decodeArgs(raw, &a); err != nil {
		return toolResult{}, err
	}
	cl, err := c.client()
	if err != nil {
		return toolResult{}, err
	}
	nodes, err := cl.ListNodes(c.opCtx)
	if err != nil {
		return toolResult{}, err
	}
	return encodeJSON(contract.NodeListResponse{Version: contract.ProtocolVersion, Nodes: nodes})
}

func runNodeShow(c *call, raw json.RawMessage) (toolResult, error) {
	var a struct {
		ID string `json:"id"`
	}
	if err := decodeArgs(raw, &a); err != nil {
		return toolResult{}, err
	}
	if !contract.ValidNodeID(a.ID) {
		return toolResult{}, invalid("id", "invalid node ID; want n_ followed by 32 lowercase hex digits")
	}
	cl, err := c.client()
	if err != nil {
		return toolResult{}, err
	}
	n, err := cl.ShowNode(c.opCtx, a.ID)
	if err != nil {
		return toolResult{}, err
	}
	return encodeJSON(contract.NodeResponse{Version: contract.ProtocolVersion, Node: n})
}

// ---- Role tools (FP-4) ----

type roleAddArgs struct {
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

// roleConfig maps role_add arguments directly to a RoleConfig; an omitted
// timeout keeps HasTimeout false so the established default applies.
func (a roleAddArgs) roleConfig() (contract.RoleConfig, error) {
	rc := contract.RoleConfig{ID: a.ID, Name: a.Name, Node: a.Node, Adapter: a.Adapter, Instruction: a.Instruction, Runbook: a.Runbook,
		Model: a.Model, Effort: a.Effort, Concurrency: a.Concurrency}
	if a.Timeout != nil {
		d, err := parseTimeout(*a.Timeout)
		if err != nil {
			return rc, err
		}
		rc.Timeout, rc.HasTimeout = d, true
	}
	return rc, contract.ValidateRoleConfig(rc, adapter.Lookup())
}

func runRoleAdd(c *call, raw json.RawMessage) (toolResult, error) {
	var a roleAddArgs
	if err := decodeArgs(raw, &a); err != nil {
		return toolResult{}, err
	}
	rc, err := a.roleConfig()
	if err != nil {
		return toolResult{}, err
	}
	cl, err := c.client()
	if err != nil {
		return toolResult{}, err
	}
	v, err := cl.AddRole(c.opCtx, rc)
	if err != nil {
		return toolResult{}, err
	}
	return encodeJSON(contract.RoleResponse{Version: contract.ProtocolVersion, Role: v})
}

type roleSetArgs struct {
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

// patch builds the RolePatch of the present fields only; an explicit
// timeout 0 (unlimited) is kept, and an empty patch is refused.
func (a roleSetArgs) patch() (contract.RolePatch, error) {
	if !contract.ValidSlug(a.ID) {
		return contract.RolePatch{}, invalidRoleID()
	}
	p := contract.RolePatch{Name: a.Name, Adapter: a.Adapter, Instruction: a.Instruction, Runbook: a.Runbook, Model: a.Model, Effort: a.Effort,
		Concurrency: a.Concurrency}
	if a.Timeout != nil {
		d, err := parseTimeout(*a.Timeout)
		if err != nil {
			return p, err
		}
		p.Timeout = &d
	}
	if p.Empty() {
		return p, invalid("", "role_set needs at least one field to change (name, adapter, instruction, runbook, model, effort, concurrency or timeout)")
	}
	if err := contract.ValidatePatch(p); err != nil {
		return p, err
	}
	if p.Adapter != nil {
		if _, known := adapter.Lookup()(*p.Adapter); !known {
			return p, contract.RoleError(contract.CodeInvalidArgument, a.ID, "", "adapter", "", "unknown adapter; registered adapters: claude, codex, cursor, fake, grok")
		}
	}
	return p, nil
}

func runRoleSet(c *call, raw json.RawMessage) (toolResult, error) {
	var a roleSetArgs
	if err := decodeArgs(raw, &a); err != nil {
		return toolResult{}, err
	}
	p, err := a.patch()
	if err != nil {
		return toolResult{}, err
	}
	cl, err := c.client()
	if err != nil {
		return toolResult{}, err
	}
	v, err := cl.SetRole(c.opCtx, a.ID, p)
	if err != nil {
		return toolResult{}, err
	}
	return encodeJSON(contract.RoleResponse{Version: contract.ProtocolVersion, Role: v})
}

func runRoleLs(c *call, raw json.RawMessage) (toolResult, error) {
	var a struct{}
	if err := decodeArgs(raw, &a); err != nil {
		return toolResult{}, err
	}
	cl, err := c.client()
	if err != nil {
		return toolResult{}, err
	}
	views, err := cl.ListRoles(c.opCtx)
	if err != nil {
		return toolResult{}, err
	}
	return encodeJSON(contract.RoleListResponse{Version: contract.ProtocolVersion, Roles: views})
}

func runRoleShow(c *call, raw json.RawMessage) (toolResult, error) {
	var a struct {
		ID string `json:"id"`
	}
	if err := decodeArgs(raw, &a); err != nil {
		return toolResult{}, err
	}
	if !contract.ValidSlug(a.ID) {
		return toolResult{}, invalidRoleID()
	}
	cl, err := c.client()
	if err != nil {
		return toolResult{}, err
	}
	v, err := cl.ShowRole(c.opCtx, a.ID)
	if err != nil {
		return toolResult{}, err
	}
	return encodeJSON(contract.RoleResponse{Version: contract.ProtocolVersion, Role: v})
}

type roleRmArgs struct {
	ID        string  `json:"id"`
	Force     *bool   `json:"force,omitempty"`
	Operation *string `json:"operation,omitempty"`
}

func runRoleRm(c *call, raw json.RawMessage) (toolResult, error) {
	var a roleRmArgs
	if err := decodeArgs(raw, &a); err != nil {
		return toolResult{}, err
	}
	if !contract.ValidSlug(a.ID) {
		return toolResult{}, invalidRoleID()
	}
	force := a.Force != nil && *a.Force
	op := ""
	if a.Operation != nil {
		op = *a.Operation
		if err := contract.ValidateRemovalOperation(force, op); err != nil {
			return toolResult{}, err
		}
	}
	cl, err := c.client()
	if err != nil {
		return toolResult{}, err
	}
	res, err := cl.RemoveRole(c.opCtx, a.ID, force, op)
	if err != nil {
		if force {
			return toolResult{}, withGuidance(err, removeGuidance)
		}
		return toolResult{}, err
	}
	switch {
	case res.Pending != nil && res.Completed == nil:
		return encodeJSON(*res.Pending)
	case res.Completed != nil && res.Pending == nil:
		return encodeJSON(contract.RoleRemoveResponse{Version: contract.ProtocolVersion, Removed: res.Completed.Removed})
	}
	return toolResult{}, contract.New(contract.CodeInternal, "the role removal result has no single branch")
}

// ---- Dispatch (FP-5) ----

type dispatchArgs struct {
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

// request builds the plane request with the session's attribution and
// validates it (attribution first) before anything is sent.
func (a dispatchArgs) request(rb contract.RequestedBy) (contract.DispatchRequest, error) {
	if err := rb.Validate(); err != nil {
		return contract.DispatchRequest{}, err
	}
	payload := a.Payload
	if payload == nil {
		payload = []string{}
	}
	req := contract.DispatchRequest{Target: contract.TaskTarget{Kind: a.Target.Kind, Value: a.Target.Value}, Goal: a.Goal, Payload: payload,
		Acceptance: a.Acceptance, RequestedBy: rb, Workspace: a.Workspace, Base: a.Base, WorkspaceInstance: a.WorkspaceInstance}
	if o := a.Override; o != nil {
		req.Override = &contract.TaskOverride{Model: o.Model, Effort: o.Effort}
		if o.Timeout != nil {
			d, err := parseTimeout(*o.Timeout)
			if err != nil {
				return req, err
			}
			req.Override.Timeout = &d
		}
	}
	return req, req.Validate()
}

func runDispatch(c *call, raw json.RawMessage) (toolResult, error) {
	var a dispatchArgs
	if err := decodeArgs(raw, &a); err != nil {
		return toolResult{}, err
	}
	rb, err := c.s.requester()
	if err != nil {
		return toolResult{}, err
	}
	req, err := a.request(rb)
	if err != nil {
		return toolResult{}, err
	}
	var explicit *time.Duration
	if a.Wait != nil {
		d, err := contract.ParseWaitDuration(*a.Wait)
		if err != nil {
			return toolResult{}, err
		}
		explicit = &d
		c.budgeted()
	}
	cl, err := c.client()
	if err != nil {
		return toolResult{}, err
	}
	if explicit == nil {
		v, err := cl.Dispatch(c.opCtx, req)
		if err != nil {
			return toolResult{}, withGuidance(err, dispatchGuidance)
		}
		return encodeJSON(contract.DispatchResponse{Version: contract.ProtocolVersion, TaskID: v.TaskID, Task: &v})
	}
	available, ok := c.available(true)
	if !ok {
		return toolResult{}, errNoTime
	}
	r, err := cl.DispatchWithWait(c.opCtx, req, EffectiveWait(explicit, available))
	if err != nil {
		return toolResult{}, withGuidance(err, dispatchGuidance)
	}
	res, err := encodeJSON(r)
	if err == nil && r.WaitResult != nil && r.WaitResult.Status == contract.WaitStillRunning {
		// The compact union keeps its own bound; the dispatch envelope adds
		// at most 256 bytes, and the whole frame its wire bound.
		if _, err := contract.EncodeWaitResponse(*r.WaitResult); err != nil {
			return toolResult{}, err
		}
		if limit := contract.StillRunningLimit(len(r.WaitResult.Tasks)) + dispatchEnvelope; len(res.text)+1 > limit {
			return toolResult{}, contract.New(contract.CodeInternal, "the dispatch's still_running answer of "+strconv.Itoa(len(res.text)+1)+
				" bytes exceeds its "+strconv.Itoa(limit)+"-byte bound")
		}
		res.wireLimit = stillRunningWireLimit(len(r.WaitResult.Tasks))
	}
	return res, err
}

// requester is the session's attribution: the client's retained
// clientInfo name and version and the coordinator hostname, exactly as
// given (no normalization, truncation or substitution).
func (s *session) requester() (contract.RequestedBy, error) {
	s.mu.Lock()
	name, version := s.clientName, s.clientVersion
	s.mu.Unlock()
	rb := contract.RequestedBy{Name: name, Version: version}
	if !contract.ValidSoftwareVersion(name) || !contract.ValidSoftwareVersion(version) {
		return rb, invalid("requested_by", "this MCP client's clientInfo name and version must each be 1-128 printable ASCII bytes without spaces to attribute a dispatch (Callsheet's requested_by grammar); nothing was dispatched. The client can still use the read tools.")
	}
	h, err := s.cfg.Hostname()
	if err != nil || !contract.ValidHostname(h) {
		return rb, invalid("requested_by", "this coordinator's hostname is unavailable or not of the form [A-Za-z0-9][A-Za-z0-9._-]{0,127}; nothing was dispatched")
	}
	rb.Hostname = h
	return rb, rb.Validate()
}

// ---- Task reads (FP-6) ----

func runTaskLs(c *call, raw json.RawMessage) (toolResult, error) {
	var a struct {
		After *string `json:"after,omitempty"`
		Limit *int    `json:"limit,omitempty"`
	}
	if err := decodeArgs(raw, &a); err != nil {
		return toolResult{}, err
	}
	after, limit := "", contract.DefaultTaskListLimit
	if a.After != nil {
		if !contract.ValidTaskID(*a.After) {
			return toolResult{}, invalidTaskID("after")
		}
		after = *a.After
	}
	if a.Limit != nil {
		if *a.Limit < 1 || *a.Limit > contract.MaxTaskListLimit {
			return toolResult{}, invalid("limit", "limit must be an integer from 1 to %d", contract.MaxTaskListLimit)
		}
		limit = *a.Limit
	}
	cl, err := c.client()
	if err != nil {
		return toolResult{}, err
	}
	tasks, next, err := cl.ListTasks(c.opCtx, after, limit)
	if err != nil {
		return toolResult{}, err
	}
	return encodeJSON(contract.TaskListResponse{Version: contract.ProtocolVersion, Tasks: tasks, NextAfter: next})
}

func runTaskShow(c *call, raw json.RawMessage) (toolResult, error) {
	var a struct {
		ID    string `json:"id"`
		Lines *int   `json:"lines,omitempty"`
	}
	if err := decodeArgs(raw, &a); err != nil {
		return toolResult{}, err
	}
	if !contract.ValidTaskID(a.ID) {
		return toolResult{}, invalidTaskID("id")
	}
	lines := contract.DefaultTailLines
	if a.Lines != nil {
		if *a.Lines < 0 || *a.Lines > contract.MaxTailLines {
			return toolResult{}, invalid("lines", "lines must be an integer from 0 to %d", contract.MaxTailLines)
		}
		lines = *a.Lines
	}
	cl, err := c.client()
	if err != nil {
		return toolResult{}, err
	}
	v, err := cl.ShowTask(c.opCtx, a.ID, lines)
	if err != nil {
		return toolResult{}, err
	}
	return encodeJSON(contract.TaskShowResponse{Version: contract.ProtocolVersion, Task: v})
}

func runTaskLogs(c *call, raw json.RawMessage) (toolResult, error) {
	var a struct {
		ID   string `json:"id"`
		Late *bool  `json:"late,omitempty"`
	}
	if err := decodeArgs(raw, &a); err != nil {
		return toolResult{}, err
	}
	if !contract.ValidTaskID(a.ID) {
		return toolResult{}, invalidTaskID("id")
	}
	cl, err := c.client()
	if err != nil {
		return toolResult{}, err
	}
	var r contract.TaskLogsResponse
	if a.Late != nil && *a.Late {
		r, err = cl.TaskLateLogs(c.opCtx, a.ID)
	} else {
		r, err = cl.TaskLogs(c.opCtx, a.ID)
	}
	if err != nil {
		return toolResult{}, err
	}
	return encodeJSON(r)
}

// ---- Cancel and bounded waits (FP-7) ----

func runTaskCancel(c *call, raw json.RawMessage) (toolResult, error) {
	var a struct {
		ID string `json:"id"`
	}
	if err := decodeArgs(raw, &a); err != nil {
		return toolResult{}, err
	}
	if !contract.ValidTaskID(a.ID) {
		return toolResult{}, invalidTaskID("id")
	}
	cl, err := c.client()
	if err != nil {
		return toolResult{}, err
	}
	r, err := cl.CancelTask(c.opCtx, a.ID)
	if err != nil {
		return toolResult{}, err
	}
	return encodeJSON(r)
}

func runTaskWait(c *call, raw json.RawMessage) (toolResult, error) {
	var a struct {
		TaskIDs []string `json:"task_ids"`
		Wait    *string  `json:"wait,omitempty"`
	}
	if err := decodeArgs(raw, &a); err != nil {
		return toolResult{}, err
	}
	if err := contract.ValidateWaitIDs(a.TaskIDs); err != nil {
		return toolResult{}, err
	}
	var explicit *time.Duration
	if a.Wait != nil {
		d, err := contract.ParseWaitDuration(*a.Wait)
		if err != nil {
			return toolResult{}, err
		}
		explicit = &d
	}
	c.budgeted()
	cl, err := c.client()
	if err != nil {
		return toolResult{}, err
	}
	available, ok := c.available(false)
	if !ok {
		return toolResult{}, errNoTime
	}
	r, err := cl.WaitTasks(c.opCtx, a.TaskIDs, EffectiveWait(explicit, available))
	if err != nil {
		return toolResult{}, err
	}
	b, err := contract.EncodeWaitResponse(r)
	if err != nil {
		return toolResult{}, err
	}
	res := toolResult{text: b}
	if r.Status == contract.WaitStillRunning {
		res.wireLimit = stillRunningWireLimit(len(r.Tasks))
	}
	return res, nil
}

// Entire wire bounds of a still_running answer (one JSON-string text
// block, an ID of at most 128 bytes, worst-case escaping and the newline
// included): 16 KiB for one ID (dispatch with a wait included) and
// 100 KiB for several.
const (
	stillRunningWireOne  = 16 << 10
	stillRunningWireMany = 100 << 10
	// dispatchEnvelope is the most a dispatch's outer envelope adds to its
	// compact still_running union.
	dispatchEnvelope = 256
)

func stillRunningWireLimit(n int) int {
	if n == 1 {
		return stillRunningWireOne
	}
	return stillRunningWireMany
}

// describeTools renders the tools/list result for tools.
func describeTools(tools []*tool) ([]byte, error) {
	type def struct {
		Name        string         `json:"name"`
		Description string         `json:"description"`
		InputSchema schema         `json:"inputSchema"`
		Annotations map[string]any `json:"annotations"`
	}
	defs := make([]def, 0, len(tools))
	for _, t := range tools {
		defs = append(defs, def{Name: t.name, Description: t.description, InputSchema: schemaFor(t.name), Annotations: t.annotations})
	}
	return contract.Encode(struct {
		Tools []def `json:"tools"`
	}{defs})
}

// toolNamed returns the tool called name, or nil.
func toolNamed(tools []*tool, name string) *tool {
	for _, t := range tools {
		if t.name == name {
			return t
		}
	}
	return nil
}

// quoteName renders a peer-supplied name safely for a protocol message.
func quoteName(s string) string {
	if len(s) > 64 {
		s = s[:64]
	}
	return strconv.QuoteToASCII(strings.ToValidUTF8(s, "?"))
}
