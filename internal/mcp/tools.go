package mcp

import (
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
	budgetNote := " This server's call budget is B=" + b.String() + ". " + BudgetDeferralNote + " " + ShortPollNotice
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
			description: "Dispatch a task to a role: the plane reserves one of the role's slots atomically or fails at once listing every candidate (no queue, no reroute). With workspace the task runs in a private checkout of that workspace's base (default main; a branch, full hash, complete task ref, task ID or empty) and publishes its result as refs/callsheet/tasks/TASK_ID; the base commit and instance are fixed at admission (workspace_binding), an empty workspace needs base empty, and nothing moves a branch. To continue from task A, dispatch with A's result.workspace.commit as base and its workspace_binding name and instance as workspace and workspace_instance. Without wait the answer is the admitted task (dispatch stays asynchronous). The optional wait (0 to 5m) is the short-task fast path: the same single call also waits for the task to end, capped by the call budget (at most B minus the reserves and a 6s admission allowance): the terminal task or its compact still_running row; follow a longer task with the background CLI wait, or task_wait on a later turn. A lost answer may hide an admitted task: inspect task_ls before dispatching again (never retried). requested_by is this MCP client's self-reported clientInfo name and version plus the coordinator hostname, not an authenticated identity." + admissionNote + mutationNote + freshNote + budgetNote},
		{name: toolTaskLs, annotations: readOnly(), run: runTaskLs,
			description: "List tasks from every coordinator ordered by task ID, one page per call (limit 1-100, default 100; after: the previous page's next_after); all pages are never fetched automatically." + readOnlyNote + freshNote},
		{name: toolTaskShow, annotations: readOnly(), run: runTaskShow,
			description: "Show one task: request, attribution, role, effective settings, state, timestamps, result, late result summary, durability flags and the last lines of output (lines 0-200, default 20). A failed, rejected, lost, cancelled or timed_out task is a successful observation." + readOnlyNote + freshNote},
		{name: toolTaskLogs, annotations: readOnly(), run: runTaskLogs,
			description: "Return a task's complete retained output (up to 10 MiB, base64 in data, byte exact) or, with late, its late result's separate output (none is not_found). The answer can be large: for routine monitoring use task_show or task_wait instead." + readOnlyNote + freshNote},
		{name: toolTaskCancel, annotations: mutation(true, true), run: runTaskCancel,
			description: "Ask the plane to cancel a task. accepted=true means a durable stop request, accepted=false that the task was already durably terminal; neither proves that a pending cancellation finished: follow with task_wait. Repeating it is safe; this server never retries it." + admissionNote + mutationNote + freshNote},
		{name: toolTaskWait, annotations: readOnly(), run: runTaskWait,
			description: "A compact short poll: wait for the first of 1 to 16 tasks to end durably, within this call's budget: terminal with the winner's view, or still_running with one compact row per task in the given order. The plane may answer sooner (its own wait cap). A failed task is a successful observation. It blocks only this call; never use it as a multi-hour coordinator wait: after still_running poll again on a later turn." + readOnlyNote + freshNote + budgetNote},
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

func runWsCreate(c *call, raw json.RawMessage) (toolResult, error) {
	name, err := contract.ParseWsNameArgs(raw)
	if err != nil {
		return toolResult{}, err
	}
	cl, err := c.client()
	if err != nil {
		return toolResult{}, err
	}
	v, err := cl.CreateWorkspace(c.opCtx, name)
	if err != nil {
		return toolResult{}, withGuidance(err, wsGuidance)
	}
	return encodeJSON(v)
}

func runWsLs(c *call, raw json.RawMessage) (toolResult, error) {
	after, limit, err := contract.ParseWsListArgs(raw)
	if err != nil {
		return toolResult{}, err
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
	name, err := contract.ParseWsNameArgs(raw)
	if err != nil {
		return toolResult{}, err
	}
	cl, err := c.client()
	if err != nil {
		return toolResult{}, err
	}
	v, err := cl.ShowWorkspace(c.opCtx, name)
	if err != nil {
		return toolResult{}, err
	}
	return encodeJSON(v)
}

func runWsRm(c *call, raw json.RawMessage) (toolResult, error) {
	name, instance, err := contract.ParseWsRemoveArgs(raw)
	if err != nil {
		return toolResult{}, err
	}
	cl, err := c.client()
	if err != nil {
		return toolResult{}, err
	}
	r, err := cl.RemoveWorkspace(c.opCtx, name, instance)
	if err != nil {
		return toolResult{}, withGuidance(err, wsGuidance)
	}
	return encodeJSON(r)
}

func runWsPrune(c *call, raw json.RawMessage) (toolResult, error) {
	name, instance, before, err := contract.ParseWsPruneArgs(raw)
	if err != nil {
		return toolResult{}, err
	}
	cl, err := c.client()
	if err != nil {
		return toolResult{}, err
	}
	r, err := cl.PruneWorkspace(c.opCtx, name, instance, before)
	if err != nil {
		return toolResult{}, withGuidance(err, wsGuidance)
	}
	return encodeJSON(r)
}

func runWsRefSet(c *call, raw json.RawMessage) (toolResult, error) {
	name, req, err := contract.ParseWsRefSetArgs(raw)
	if err != nil {
		return toolResult{}, err
	}
	cl, err := c.client()
	if err != nil {
		return toolResult{}, err
	}
	r, err := cl.SetWorkspaceRef(c.opCtx, name, req)
	if err != nil {
		return toolResult{}, withGuidance(err, wsGuidance)
	}
	return encodeJSON(r)
}

func runWsStatus(c *call, raw json.RawMessage) (toolResult, error) {
	a, err := contract.ParseWsStatusArgs(raw)
	if err != nil {
		return toolResult{}, err
	}
	cl, err := c.client()
	if err != nil {
		return toolResult{}, err
	}
	if a.TaskID != "" {
		r, err := cl.TaskWorkspaceStatus(c.opCtx, a.TaskID)
		if err != nil {
			return toolResult{}, err
		}
		return encodeJSON(r)
	}
	r, err := cl.WorkspaceStatus(c.opCtx, a.Name, client.StatusPage{After: a.Page.After, Instance: a.Page.Instance, Generation: a.Page.Generation,
		Limit: a.Page.Limit})
	if err != nil {
		return toolResult{}, err
	}
	return encodeJSON(r)
}

func runWsDiff(c *call, raw json.RawMessage) (toolResult, error) {
	a, err := contract.ParseWsDiffArgs(raw)
	if err != nil {
		return toolResult{}, err
	}
	cl, err := c.client()
	if err != nil {
		return toolResult{}, err
	}
	p := a.Page
	if a.TaskID != "" {
		r, err := cl.TaskWorkspaceDiff(c.opCtx, a.TaskID, client.TaskDiffPage{After: p.After, Instance: p.Instance, Generation: p.Generation, Limit: p.Limit})
		if err != nil {
			return toolResult{}, err
		}
		return encodeJSON(r)
	}
	r, err := cl.WorkspaceDiff(c.opCtx, a.Name, client.DiffPage{Base: a.Base, Target: a.Target, After: p.After, Instance: p.Instance,
		Generation: p.Generation, Limit: p.Limit})
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
	return contract.DecodeToolArgs(raw, dto)
}

func invalid(field, format string, args ...any) error {
	return contract.TaskError(contract.CodeInvalidArgument, field, "", format, args...)
}

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
	if err := contract.ParseNoArgs(raw); err != nil {
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
	id, err := contract.ParseNodeShowArgs(raw)
	if err != nil {
		return toolResult{}, err
	}
	cl, err := c.client()
	if err != nil {
		return toolResult{}, err
	}
	n, err := cl.ShowNode(c.opCtx, id)
	if err != nil {
		return toolResult{}, err
	}
	return encodeJSON(contract.NodeResponse{Version: contract.ProtocolVersion, Node: n})
}

// ---- Role tools (FP-4) ----

func runRoleAdd(c *call, raw json.RawMessage) (toolResult, error) {
	rc, err := contract.ParseRoleAddArgs(raw, adapter.Lookup())
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

func runRoleSet(c *call, raw json.RawMessage) (toolResult, error) {
	id, p, err := contract.ParseRoleSetArgs(raw, adapter.Lookup())
	if err != nil {
		return toolResult{}, err
	}
	cl, err := c.client()
	if err != nil {
		return toolResult{}, err
	}
	v, err := cl.SetRole(c.opCtx, id, p)
	if err != nil {
		return toolResult{}, err
	}
	return encodeJSON(contract.RoleResponse{Version: contract.ProtocolVersion, Role: v})
}

func runRoleLs(c *call, raw json.RawMessage) (toolResult, error) {
	if err := contract.ParseNoArgs(raw); err != nil {
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
	id, err := contract.ParseRoleShowArgs(raw)
	if err != nil {
		return toolResult{}, err
	}
	cl, err := c.client()
	if err != nil {
		return toolResult{}, err
	}
	v, err := cl.ShowRole(c.opCtx, id)
	if err != nil {
		return toolResult{}, err
	}
	return encodeJSON(contract.RoleResponse{Version: contract.ProtocolVersion, Role: v})
}

func runRoleRm(c *call, raw json.RawMessage) (toolResult, error) {
	id, force, op, err := contract.ParseRoleRemoveArgs(raw)
	if err != nil {
		return toolResult{}, err
	}
	cl, err := c.client()
	if err != nil {
		return toolResult{}, err
	}
	res, err := cl.RemoveRole(c.opCtx, id, force, op)
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

// runDispatch validates its arguments with the contract's dispatch rule
// (decode, then the session's attribution and the request, then the
// explicit wait), the one the offline verifier also applies.
func runDispatch(c *call, raw json.RawMessage) (toolResult, error) {
	a, err := contract.DecodeDispatchArgs(raw)
	if err != nil {
		return toolResult{}, err
	}
	rb, err := c.s.requester()
	if err != nil {
		return toolResult{}, err
	}
	req, err := a.Request(rb)
	if err != nil {
		return toolResult{}, err
	}
	explicit, err := a.WaitDuration()
	if err != nil {
		return toolResult{}, err
	}
	if explicit != nil {
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
	after, limit, err := contract.ParseTaskListArgs(raw)
	if err != nil {
		return toolResult{}, err
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
	id, lines, err := contract.ParseTaskShowArgs(raw)
	if err != nil {
		return toolResult{}, err
	}
	cl, err := c.client()
	if err != nil {
		return toolResult{}, err
	}
	v, err := cl.ShowTask(c.opCtx, id, lines)
	if err != nil {
		return toolResult{}, err
	}
	return encodeJSON(contract.TaskShowResponse{Version: contract.ProtocolVersion, Task: v})
}

func runTaskLogs(c *call, raw json.RawMessage) (toolResult, error) {
	id, late, err := contract.ParseTaskLogsArgs(raw)
	if err != nil {
		return toolResult{}, err
	}
	cl, err := c.client()
	if err != nil {
		return toolResult{}, err
	}
	var r contract.TaskLogsResponse
	if late {
		r, err = cl.TaskLateLogs(c.opCtx, id)
	} else {
		r, err = cl.TaskLogs(c.opCtx, id)
	}
	if err != nil {
		return toolResult{}, err
	}
	return encodeJSON(r)
}

// ---- Cancel and bounded waits (FP-7) ----

func runTaskCancel(c *call, raw json.RawMessage) (toolResult, error) {
	id, err := contract.ParseTaskCancelArgs(raw)
	if err != nil {
		return toolResult{}, err
	}
	cl, err := c.client()
	if err != nil {
		return toolResult{}, err
	}
	r, err := cl.CancelTask(c.opCtx, id)
	if err != nil {
		return toolResult{}, err
	}
	return encodeJSON(r)
}

func runTaskWait(c *call, raw json.RawMessage) (toolResult, error) {
	ids, explicit, err := contract.ParseTaskWaitArgs(raw)
	if err != nil {
		return toolResult{}, err
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
	r, err := cl.WaitTasks(c.opCtx, ids, EffectiveWait(explicit, available))
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
