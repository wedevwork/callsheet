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
)

// ToolNames lists the tools in their fixed discovery order.
var ToolNames = []string{toolNodeLs, toolNodeShow, toolRoleAdd, toolRoleSet, toolRoleLs, toolRoleShow, toolRoleRm,
	toolDispatch, toolTaskLs, toolTaskShow, toolTaskLogs, toolTaskCancel, toolTaskWait}

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

// newTools builds the thirteen definitions for budget b.
func newTools(b Budget) []*tool {
	budgetNote := " This server's call budget is B=" + b.String() + ". " + BudgetDeferralNote + " " + UnverifiedNotice
	return []*tool{
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
			description: "Dispatch a task to a role: the plane reserves one of the role's slots atomically or fails at once listing every candidate (no queue, no reroute). Without wait the answer is the admitted task. With wait (0 to 5m) the same single call also waits for the task to end, capped by the call budget (at most B minus the reserves and a 6s admission allowance): the terminal task or its compact still_running row; follow with task_wait. A lost answer may hide an admitted task: inspect task_ls before dispatching again (never retried). requested_by is this MCP client's self-reported clientInfo name and version plus the coordinator hostname, not an authenticated identity." + admissionNote + mutationNote + freshNote + budgetNote},
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
			return p, contract.RoleError(contract.CodeInvalidArgument, a.ID, "", "adapter", "", "unknown adapter; registered adapters: fake")
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
		Acceptance: a.Acceptance, RequestedBy: rb}
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
