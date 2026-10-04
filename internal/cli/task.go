package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/wedevwork/callsheet/internal/contract"
)

// The task leaves (iteration 05): dispatch and task ls, show and logs
// over the verified client. Exit 0 from dispatch means admission, not
// task success; reading a failed or rejected task succeeds. Nothing is
// cached and no worker is ever contacted directly.

// hostname is the requester hostname source (injectable for tests).
var hostname = os.Hostname

const (
	dispatchUsage   = "(--role-id ID | --role-name NAME) --goal TEXT --acceptance TEXT [--payload POINTER ...] [--model MODEL] [--effort EFFORT] [--timeout DURATION] [--wait DURATION] [--workspace NAME [--base SELECTOR] [--workspace-instance TOKEN]] " + trustUsage + " [--json]"
	taskCancelUsage = "ID " + trustUsage + " [--json]"
	taskWaitUsage   = "ID [ID ...] [--wait DURATION] " + trustUsage + " [--json]"
	taskLsUsage     = "[--limit N] [--after ID] " + trustUsage + " [--json]"
	taskShowUsage   = "ID [--lines N] " + trustUsage + " [--json]"
	taskLogsUsage   = "ID [--late] " + trustUsage + " [--json]"

	taskJSONHelp = "  --json             print the plane's response envelope as one JSON value\n"

	dispatchDetails = "Flags:\n" +
		"  --role-id ID       dispatch to exactly this role instance (or fail)\n" +
		"  --role-name NAME   dispatch to the first role of this name, in registration order,\n" +
		"                     that can accept the task now\n" +
		"  --goal TEXT        what the task must achieve (required; passed verbatim)\n" +
		"  --acceptance TEXT  how the result will be judged (required; passed through, never\n" +
		"                     evaluated)\n" +
		"  --payload POINTER  an opaque pointer for the worker (repeatable; never fetched)\n" +
		"  --model MODEL      override the role's model for this task only\n" +
		"  --effort EFFORT    override the role's effort for this task only (the adapter's efforts)\n" +
		"  --timeout DURATION this task's execution timeout, e.g. 90m; 0 is unlimited (default:\n" +
		"                     the role's). It is enforced on the worker from the adapter's start\n" +
		"                     (queue and preparation time excluded), also while the plane is\n" +
		"                     unreachable; an expired task ends timed_out\n" +
		"  --wait DURATION    after admission, wait at most this long (0 to 5m) for the task to\n" +
		"                     end, then print its result or a compact still-running line; the\n" +
		"                     plane may answer sooner (plane run --max-task-wait, default 30s)\n" +
		"  --workspace NAME   run the task in a private checkout of this workspace and publish\n" +
		"                     its result as refs/callsheet/tasks/TASK_ID (omitted: a scratch\n" +
		"                     directory and no result commit)\n" +
		"  --base SELECTOR    the checkout's base, with --workspace: a branch (default main), a\n" +
		"                     full 40-hex commit hash, a complete task ref, a task ID (its result\n" +
		"                     ref) or empty for the empty tree (refs/heads/t_... selects a branch\n" +
		"                     named like a task ID)\n" +
		"  --workspace-instance TOKEN\n" +
		"                     with --workspace: admit only if the workspace still has this\n" +
		"                     instance (from callsheet ws show or a previous task's binding)\n" +
		trustHelp + taskJSONHelp + "\n" +
		"Exactly one of --role-id and --role-name is required. The plane reserves one of the\n" +
		"role's slots atomically, or fails at once listing every candidate with its state:\n" +
		"there is no queue and no reroute. The worker composes the prompt from its local\n" +
		"manuals and this task, and runs the adapter's CLI child in a fresh scratch directory,\n" +
		"or with --workspace in a private checkout of the selected commit. The selected\n" +
		"commit and the workspace instance are fixed at admission: later branch movement never\n" +
		"changes them. An empty workspace (main unborn) needs --base empty. Nothing moves a\n" +
		"branch: the result is a separate task ref (callsheet ws status, diff and pull TASK_ID).\n" +
		"Exit 0 means the task was admitted, not that it succeeded, with or without --wait:\n" +
		"follow it with callsheet task wait or task show. A lost response may hide an admitted\n" +
		"task; check callsheet task ls before dispatching again (a dispatch is never retried).\n" +
		"The wait bounds are CLI and operator choices, not a verified MCP tool-call limit.\n\n" +
		"Example, on an operator machine:\n" +
		"  callsheet dispatch --role-name implementer --goal \"fix the parser\" \\\n" +
		"    --acceptance \"all tests pass\" --payload repo://callsheet \\\n" +
		"    --plane https://plane.example:8443 --ca plane-ca.crt\n" +
		"A second hop on a first task's published result (its result.workspace.commit and\n" +
		"workspace_binding instance):\n" +
		"  callsheet dispatch --role-name reviewer --goal \"review the change\" \\\n" +
		"    --acceptance \"findings listed\" --workspace myproject --base <commit> \\\n" +
		"    --workspace-instance <instance> --plane https://plane.example:8443 --ca plane-ca.crt\n"
	taskCancelDetails = "Flags:\n" + trustHelp + taskJSONHelp + "\n" +
		"Asks the plane to cancel the task and prints cancel accepted: ID once the request is\n" +
		"durable, or task already terminal: ID STATE. Acceptance is not the outcome: the worker\n" +
		"stops the task's whole process group (TERM, 1 s grace, KILL) and keeps its partial\n" +
		"output; follow it with callsheet task wait ID. A task that already finished keeps its\n" +
		"result. A task whose node is offline is cancelled when the node reconnects, or becomes\n" +
		"lost (stop_requested) when its lease expires. Repeating a cancel is safe and changes\n" +
		"nothing; a lost response may hide an accepted cancel: repeat it or run task show.\n"
	taskWaitDetails = "Flags:\n" +
		"  --wait DURATION    wait at most this long (0 to 5m, default 5s; 0 answers at once)\n" +
		trustHelp + taskJSONHelp + "\n" +
		"Waits for the first of up to 16 tasks to end durably and prints winner: ID and that\n" +
		"task as task show does. When none ends in time it prints one tab-separated line per\n" +
		"task, in the given order: TASK_ID, STATE, ELAPSED_MS, LAST_LOG_LINE (its last at most\n" +
		"96 bytes, control bytes escaped), LOG_TRUNCATED and DURABILITY_CONFIRMED. Both exit 0,\n" +
		"whatever the task's own result. The plane may answer sooner than asked (plane run\n" +
		"--max-task-wait, default 30s); a plane restart is retried within the same overall\n" +
		"wait. These bounds are CLI and operator choices, not a verified MCP tool-call limit.\n"
	taskLsDetails = "Flags:\n" +
		"  --limit N          at most N tasks (1-100, default 100)\n" +
		"  --after ID         start after this task ID (the previous page's next_after)\n" +
		trustHelp + taskJSONHelp + "\n" +
		"Lists tasks from every coordinator, ordered by task ID, as tab-separated columns\n" +
		"TASK_ID, STATE, ROLE_ID, NODE, ELAPSED_MS and RECONCILING, with a next_after line\n" +
		"when more tasks exist. Tasks created after a page was read may sort before its\n" +
		"cursor: list again from the start to see them.\n"
	taskShowDetails = "Flags:\n" +
		"  --lines N          the last N lines of retained output (0-200, default 20)\n" +
		trustHelp + taskJSONHelp + "\n" +
		"Shows one task as label: value lines: request, role, effective settings, state,\n" +
		"timestamps, result, any late result and the last lines of output, with warnings when\n" +
		"the execution awaits reconciliation, durability is unconfirmed or output was\n" +
		"truncated. Output is shown with terminal control bytes escaped. A task whose worker\n" +
		"disconnected stays pending or running while it is reconciling: its worker reconnects\n" +
		"and reports it, or its node's lease expires and it becomes lost (outcome and cleanup\n" +
		"unconfirmed). A lost task is final: a result the worker reports afterwards is kept as\n" +
		"its late result and never changes its state.\n"
	taskLogsDetails = "Flags:\n" +
		"  --late             print the late result's separate output tail instead (a task\n" +
		"                     without a late result is not_found)\n" + trustHelp +
		"  --json             print the plane's response envelope (base64 data, byte exact)\n\n" +
		"Prints the task's complete retained output (at most the last 10 MiB) with terminal\n" +
		"control bytes escaped; truncation and incompleteness notices go to stderr. The\n" +
		"ordinary output never changes after the task is final; output a worker reports with\n" +
		"a late result is kept separately (--late).\n"
)

// taskFlags are the task leaves' own flags.
type taskFlags struct {
	roleID, roleName, goal, acceptance, model, effort, timeout, wait single
	payload                                                          multi
	limit, after, lines                                              single
	late                                                             boolFlag
	// Iteration 10c: the optional workspace selection.
	workspace, base, workspaceInstance single
}

func (tf *taskFlags) dispatchFlags(fs *flag.FlagSet) {
	fs.Var(&tf.roleID, "role-id", "")
	fs.Var(&tf.roleName, "role-name", "")
	fs.Var(&tf.goal, "goal", "")
	fs.Var(&tf.acceptance, "acceptance", "")
	fs.Var(&tf.payload, "payload", "")
	fs.Var(&tf.model, "model", "")
	fs.Var(&tf.effort, "effort", "")
	fs.Var(&tf.timeout, "timeout", "")
	fs.Var(&tf.wait, "wait", "")
	fs.Var(&tf.workspace, "workspace", "")
	fs.Var(&tf.base, "base", "")
	fs.Var(&tf.workspaceInstance, "workspace-instance", "")
}

// opt returns a set flag's value, nil when absent (an empty value stays
// present: the contract refuses it).
func opt(s single) *string {
	if !s.set {
		return nil
	}
	v := s.val
	return &v
}

// waitFlag parses an explicit --wait (def when absent).
func (tf *taskFlags) waitFlag(def time.Duration) (time.Duration, error) {
	if !tf.wait.set {
		return def, nil
	}
	return contract.ParseWaitDuration(tf.wait.val)
}

// requester is this CLI's self-reported attribution: fixed name, the build
// version and the local hostname; an unavailable or invalid hostname
// fails before anything is sent (no invented hostname).
func requester() (contract.RequestedBy, error) {
	h, err := hostname()
	if err != nil || !contract.ValidHostname(h) {
		return contract.RequestedBy{}, contract.TaskError(contract.CodeInvalidArgument, "requested_by", "",
			"this machine's hostname is unavailable or not of the form [A-Za-z0-9][A-Za-z0-9._-]{0,127}; nothing was dispatched")
	}
	r := contract.RequestedBy{Name: "callsheet", Version: Version, Hostname: h}
	return r, r.Validate()
}

func dispatch(ctx context.Context, goos string, c *Command, args []string, out, errOut io.Writer) int {
	tf := &taskFlags{}
	f, _, code, ok := parseRemote(c, args, true, false, true, 0, errOut, tf.dispatchFlags)
	if !ok {
		return code
	}
	if tf.roleID.set == tf.roleName.set {
		return usageError(errOut, c, "give exactly one of --role-id and --role-name")
	}
	for _, req := range []struct {
		name string
		v    *single
	}{{"--goal", &tf.goal}, {"--acceptance", &tf.acceptance}} {
		if !req.v.set {
			field := strings.TrimPrefix(req.name, "--")
			return planeFail(errOut, contract.TaskError(contract.CodeInvalidArgument, field, "", "%s is required", req.name))
		}
	}
	req := contract.DispatchRequest{Target: contract.TaskTarget{Kind: contract.TargetID, Value: tf.roleID.val}, Goal: tf.goal.val,
		Acceptance: tf.acceptance.val, Payload: append([]string{}, tf.payload.vals...)}
	if tf.roleName.set {
		req.Target = contract.TaskTarget{Kind: contract.TargetName, Value: tf.roleName.val}
	}
	if tf.model.set || tf.effort.set || tf.timeout.set {
		o := &contract.TaskOverride{}
		if tf.model.set {
			o.Model = &tf.model.val
		}
		if tf.effort.set {
			o.Effort = &tf.effort.val
		}
		if tf.timeout.set {
			if _, err := time.ParseDuration(tf.timeout.val); err != nil || strings.TrimSpace(tf.timeout.val) == "" {
				return usageError(errOut, c, "--timeout must be a Go duration such as 2h, 90m or 0")
			}
			d, err := contract.ParseRoleTimeout(tf.timeout.val)
			if err != nil {
				return planeFail(errOut, err)
			}
			o.Timeout = &d
		}
		req.Override = o
	}
	// The workspace selection is forwarded unchanged; the shared contract
	// validator (req.Validate below) is the only authority.
	req.Workspace, req.Base, req.WorkspaceInstance = opt(tf.workspace), opt(tf.base), opt(tf.workspaceInstance)
	rb, err := requester()
	if err != nil {
		return planeFail(errOut, err)
	}
	req.RequestedBy = rb
	if err := req.Validate(); err != nil {
		return planeFail(errOut, err)
	}
	wait, err := tf.waitFlag(0)
	if err != nil {
		return planeFail(errOut, err)
	}
	if _, err := f.trustSyntax(); err != nil {
		return planeFail(errOut, err)
	}
	cl, code, ok := roleClient(ctx, f, errOut)
	if !ok {
		return code
	}
	defer cl.Close()
	if tf.wait.set {
		r, err := cl.DispatchWithWait(ctx, req, wait)
		if err != nil {
			return taskFail(errOut, err)
		}
		if f.json.val {
			return writeJSON(out, errOut, r)
		}
		text, err := RenderWait("task_id: "+r.TaskID+"\n", *r.WaitResult)
		if err != nil {
			return planeFail(errOut, err)
		}
		return writeOut(out, errOut, text)
	}
	v, err := cl.Dispatch(ctx, req)
	if err != nil {
		return taskFail(errOut, err)
	}
	if f.json.val {
		return writeJSON(out, errOut, contract.DispatchResponse{Version: contract.ProtocolVersion, TaskID: v.TaskID, Task: &v})
	}
	return writeOut(out, errOut, RenderDispatch(v))
}

func taskCancel(ctx context.Context, goos string, c *Command, args []string, out, errOut io.Writer) int {
	f, ops, code, ok := parseRemote(c, args, true, false, true, 1, errOut)
	if !ok {
		return code
	}
	id, code, ok := taskOperand(c, ops, errOut)
	if !ok {
		return code
	}
	cl, code, ok := roleClient(ctx, f, errOut)
	if !ok {
		return code
	}
	defer cl.Close()
	r, err := cl.CancelTask(ctx, id)
	if err != nil {
		return planeFail(errOut, err)
	}
	if f.json.val {
		return writeJSON(out, errOut, r)
	}
	if r.Accepted {
		return writeOut(out, errOut, "cancel accepted: "+id+"\n")
	}
	return writeOut(out, errOut, "task already terminal: "+id+" "+r.Task.State+"\n")
}

func taskWait(ctx context.Context, goos string, c *Command, args []string, out, errOut io.Writer) int {
	tf := &taskFlags{}
	f, ops, code, ok := parseRemote(c, args, true, false, true, contract.MaxWaitIDs, errOut, func(fs *flag.FlagSet) { fs.Var(&tf.wait, "wait", "") })
	if !ok {
		return code
	}
	if len(ops) == 0 {
		return usageError(errOut, c, c.Path()+" takes 1 to 16 task IDs")
	}
	if err := contract.ValidateWaitIDs(ops); err != nil {
		return planeFail(errOut, err)
	}
	wait, err := tf.waitFlag(contract.DefaultTaskWait)
	if err != nil {
		return planeFail(errOut, err)
	}
	cl, code, ok := roleClient(ctx, f, errOut)
	if !ok {
		return code
	}
	defer cl.Close()
	r, err := cl.WaitTasks(ctx, ops, wait)
	if err != nil {
		return planeFail(errOut, err)
	}
	if f.json.val {
		b, err := contract.EncodeWaitResponse(r)
		if err != nil {
			return planeFail(errOut, err)
		}
		return writeOut(out, errOut, string(b)+"\n")
	}
	text, err := RenderWait("", r)
	if err != nil {
		return planeFail(errOut, err)
	}
	return writeOut(out, errOut, text)
}

// RenderWait renders a wait union after prefix: winner: ID and the task as
// task show renders it, or the still_running table; a still_running
// answer (prefix included) must fit its compact bound, asserted before
// anything is written.
func RenderWait(prefix string, r contract.WaitResponse) (string, error) {
	if r.Status == contract.WaitTerminal && r.Task != nil {
		return prefix + "winner: " + r.Winner + "\n" + RenderTask(*r.Task), nil
	}
	text := prefix + contract.RenderWaitRows(r.Tasks)
	if limit := contract.StillRunningLimit(len(r.Tasks)); len(text) > limit {
		return "", contract.New(contract.CodeInternal, "the still-running answer of "+strconv.Itoa(len(text))+" bytes exceeds its "+strconv.Itoa(limit)+"-byte bound")
	}
	return text, nil
}

// taskFail reports err like planeFail and then the candidate snapshot an
// admission failure carries, one line each on stderr.
func taskFail(errOut io.Writer, err error) int {
	code := planeFail(errOut, err)
	var ce *contract.Error
	if errors.As(err, &ce) && ce.Details != nil {
		if raw, ok := ce.Details["candidates"]; ok {
			if cs, perr := contract.ParseCandidates(raw); perr == nil {
				io.WriteString(errOut, renderCandidates("callsheet: candidate: ", cs))
			}
		}
	}
	return code
}

func taskLs(ctx context.Context, goos string, c *Command, args []string, out, errOut io.Writer) int {
	tf := &taskFlags{}
	f, _, code, ok := parseRemote(c, args, true, false, true, 0, errOut, func(fs *flag.FlagSet) {
		fs.Var(&tf.limit, "limit", "")
		fs.Var(&tf.after, "after", "")
	})
	if !ok {
		return code
	}
	limit := 0
	if tf.limit.set {
		n, valid := contract.ParseInteger(tf.limit.val)
		if !valid || n < 1 || n > contract.MaxTaskListLimit {
			return usageError(errOut, c, "--limit must be an integer from 1 to "+strconv.Itoa(contract.MaxTaskListLimit))
		}
		limit = n
	}
	if tf.after.set && !contract.ValidTaskID(tf.after.val) {
		return planeFail(errOut, contract.New(contract.CodeInvalidArgument, "--after must be a task ID (t_ followed by 32 lowercase hex digits)"))
	}
	cl, code, ok := roleClient(ctx, f, errOut)
	if !ok {
		return code
	}
	defer cl.Close()
	tasks, next, err := cl.ListTasks(ctx, tf.after.val, limit)
	if err != nil {
		return planeFail(errOut, err)
	}
	if f.json.val {
		return writeJSON(out, errOut, contract.TaskListResponse{Version: contract.ProtocolVersion, Tasks: tasks, NextAfter: next})
	}
	return writeOut(out, errOut, RenderTasks(tasks, next))
}

// taskOperand returns the single task ID operand.
func taskOperand(c *Command, ops []string, errOut io.Writer) (string, int, bool) {
	if len(ops) != 1 {
		return "", usageError(errOut, c, c.Path()+" takes exactly one task ID"), false
	}
	if !contract.ValidTaskID(ops[0]) {
		return "", planeFail(errOut, contract.New(contract.CodeInvalidArgument, "invalid task ID; want t_ followed by 32 lowercase hex digits")), false
	}
	return ops[0], 0, true
}

func taskShow(ctx context.Context, goos string, c *Command, args []string, out, errOut io.Writer) int {
	tf := &taskFlags{}
	f, ops, code, ok := parseRemote(c, args, true, false, true, 1, errOut, func(fs *flag.FlagSet) { fs.Var(&tf.lines, "lines", "") })
	if !ok {
		return code
	}
	id, code, ok := taskOperand(c, ops, errOut)
	if !ok {
		return code
	}
	lines := contract.DefaultTailLines
	if tf.lines.set {
		n, valid := contract.ParseInteger(tf.lines.val)
		if !valid || n < 0 || n > contract.MaxTailLines {
			return usageError(errOut, c, "--lines must be an integer from 0 to "+strconv.Itoa(contract.MaxTailLines))
		}
		lines = n
	}
	cl, code, ok := roleClient(ctx, f, errOut)
	if !ok {
		return code
	}
	defer cl.Close()
	v, err := cl.ShowTask(ctx, id, lines)
	if err != nil {
		return planeFail(errOut, err)
	}
	if f.json.val {
		return writeJSON(out, errOut, contract.TaskShowResponse{Version: contract.ProtocolVersion, Task: v})
	}
	return writeOut(out, errOut, RenderTask(v))
}

func taskLogs(ctx context.Context, goos string, c *Command, args []string, out, errOut io.Writer) int {
	tf := &taskFlags{}
	f, ops, code, ok := parseRemote(c, args, true, false, true, 1, errOut, func(fs *flag.FlagSet) { fs.Var(&tf.late, "late", "") })
	if !ok {
		return code
	}
	id, code, ok := taskOperand(c, ops, errOut)
	if !ok {
		return code
	}
	cl, code, ok := roleClient(ctx, f, errOut)
	if !ok {
		return code
	}
	defer cl.Close()
	var r contract.TaskLogsResponse
	var err error
	if tf.late.val {
		r, err = cl.TaskLateLogs(ctx, id)
	} else {
		r, err = cl.TaskLogs(ctx, id)
	}
	if err != nil {
		return planeFail(errOut, err)
	}
	if f.json.val {
		// Only the exact envelope: no duplicate notices.
		return writeJSON(out, errOut, r)
	}
	for _, n := range logNotices(r.Truncated, r.Incomplete, r.CounterOverflow, r.LogMayBeIncomplete, r.DroppedBytes) {
		io.WriteString(errOut, "callsheet: notice: "+n+"\n")
	}
	return writeOut(out, errOut, SafeLog(string(r.Data)))
}

// writeJSON prints one compact JSON value and a final LF.
func writeJSON(out, errOut io.Writer, v any) int {
	b, err := contract.Encode(v)
	if err != nil {
		return planeFail(errOut, err)
	}
	return writeOut(out, errOut, string(b)+"\n")
}

// SafeLog renders child output for a terminal: LF and TAB are kept, every
// other control byte (CR, ESC and DEL included, and C1 controls) is
// escaped, and invalid UTF-8 becomes U+FFFD, so no uncontrolled escape
// sequence reaches the terminal.
func SafeLog(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); {
		r, n := utf8.DecodeRuneInString(s[i:])
		switch {
		case r == utf8.RuneError && n <= 1:
			b.WriteString("�")
		case r == '\n' || r == '\t':
			b.WriteRune(r)
		case r < 0x20 || r == 0x7f:
			fmt.Fprintf(&b, "\\x%02x", r)
		case r >= 0x80 && r <= 0x9f:
			fmt.Fprintf(&b, "\\u%04x", r)
		default:
			b.WriteString(s[i : i+n])
		}
		i += n
	}
	return b.String()
}

// logNotices are the explicit truncation and incompleteness warnings.
func logNotices(truncated, incomplete, overflow, mayBeIncomplete bool, dropped int) []string {
	var out []string
	if truncated {
		out = append(out, fmt.Sprintf("the retained output is truncated: %d earlier bytes are not retained", dropped))
	}
	if incomplete {
		out = append(out, "the output is incomplete: bytes were lost in transport or cut off after the child exited")
	}
	if overflow {
		out = append(out, "the output byte counter overflowed: the dropped byte count is only a lower bound")
	}
	if mayBeIncomplete {
		out = append(out, "the output may be incomplete: output received since the last checkpoint may be missing")
	}
	return out
}

func dash(p *string) string {
	if p == nil {
		return "-"
	}
	return *p
}

// renderCandidates renders one line per candidate with prefix.
func renderCandidates(prefix string, cs []contract.TaskCandidate) string {
	var b strings.Builder
	for _, c := range cs {
		fmt.Fprintf(&b, "%srole_id=%s node=%s registration_order=%d node_liveness=%s inflight=%d reconciling_inflight=%d concurrency=%d can_accept=%t reason=%s\n",
			prefix, c.RoleID, c.NodeID, c.RegistrationOrder, c.NodeLiveness, c.Inflight, c.ReconcilingInflight, c.Concurrency, c.CanAccept, c.Reason)
	}
	return b.String()
}

// RenderDispatch renders an admitted task: its ID, state and resolved
// role, the timeout notice, and a rejection's reason and candidates.
func RenderDispatch(v contract.TaskView) string {
	var b strings.Builder
	for _, kv := range [][2]string{
		{"task_id", v.TaskID}, {"state", v.State}, {"role_id", v.Role.ID}, {"role_name", v.Role.Name}, {"node", v.Role.Node},
		{"registration_order", strconv.Itoa(v.Role.RegistrationOrder)}, {"model", jsonString(v.Effective.Model)}, {"effort", v.Effective.Effort},
		{"timeout", v.Effective.Timeout.String()}, {"timeout_policy", v.TimeoutPolicy},
	} {
		b.WriteString(kv[0] + ": " + kv[1] + "\n")
	}
	renderTaskWorkspace(&b, v)
	if v.TimeoutPolicy == contract.TimeoutPolicyLegacy {
		b.WriteString("notice: " + contract.TimeoutNotice + "\n")
	}
	if v.Reason != nil {
		b.WriteString("reason: " + v.Reason.Code + " " + jsonString(v.Reason.Message) + "\n")
		b.WriteString(renderCandidates("candidate: ", v.Candidates))
	}
	return b.String()
}

// renderTaskWorkspace renders a workspace task's binding and phase and,
// with a terminal workspace result, its publication metadata: names,
// tokens, hashes, fixed codes, totals and escaped paths only (never blob
// bytes, patches, pack errors or commit messages). An absent or null value
// is "-". A task without a workspace renders nothing (scratch output is
// unchanged).
func renderTaskWorkspace(b *strings.Builder, v contract.TaskView) {
	ws := v.WorkspaceBinding
	if ws == nil {
		return
	}
	line := func(k, val string) { b.WriteString(k + ": " + val + "\n") }
	line("workspace", ws.Name)
	line("workspace_instance", ws.Instance)
	line("base_commit", dash(ws.BaseCommit))
	line("workspace_phase", dash(v.WorkspacePhase))
	if v.Result == nil || v.Result.Workspace == nil {
		return
	}
	r, w := v.Result, v.Result.Workspace
	line("publication", w.Publication)
	line("result_commit", dash(r.ResultCommit))
	line("result_ref", dash(w.Ref))
	line("publication_error", dash(w.Error))
	ds := "-"
	if d := r.Diffstat; d != nil {
		ds = fmt.Sprintf("added=%d modified=%d deleted=%d old_bytes=%d new_bytes=%d", d.Added, d.Modified, d.Deleted, d.OldBytes, d.NewBytes)
	}
	line("diffstat", ds)
	for _, p := range r.ChangedPaths {
		line("changed_path", DisplayPath(p))
	}
	if r.ChangedPathsTruncated {
		line("changed_paths_truncated", "true")
		line("changed_paths_next_after", dash(r.ChangedPathsNextAfter))
	}
}

// TaskColumns is the text header of task ls.
var TaskColumns = []string{"TASK_ID", "STATE", "ROLE_ID", "NODE", "ELAPSED_MS", "RECONCILING"}

// RenderTasks renders the header, one tab-separated row per task and a
// next_after footer when more tasks exist.
func RenderTasks(tasks []contract.TaskSummary, next *string) string {
	var b strings.Builder
	b.WriteString(joinFields(TaskColumns))
	for _, s := range tasks {
		b.WriteString(joinFields([]string{s.TaskID, s.State, s.Role.ID, s.Role.Node, strconv.Itoa(s.ElapsedMS), strconv.FormatBool(s.Reconciling)}))
	}
	if next != nil {
		b.WriteString("next_after: " + *next + "\n")
	}
	return b.String()
}

// RenderTask renders one task as label: value lines; free text is a JSON
// string literal and output is escaped for a terminal.
func RenderTask(v contract.TaskView) string {
	var b strings.Builder
	line := func(k, val string) { b.WriteString(k + ": " + val + "\n") }
	payload, _ := contract.Encode(v.Request.Payload)
	if len(v.Request.Payload) == 0 {
		payload = []byte("[]")
	}
	line("task_id", v.TaskID)
	line("state", v.State)
	line("target", v.Request.Target.Kind+" "+v.Request.Target.Value)
	line("role_id", v.Role.ID)
	line("role_name", v.Role.Name)
	line("node", v.Role.Node)
	line("registration_order", strconv.Itoa(v.Role.RegistrationOrder))
	line("goal", jsonString(v.Request.Goal))
	line("payload", string(payload))
	line("acceptance", jsonString(v.Request.Acceptance))
	line("requested_by", jsonString(v.Request.RequestedBy.Name)+" "+jsonString(v.Request.RequestedBy.Version)+" "+jsonString(v.Request.RequestedBy.Hostname))
	line("model", jsonString(v.Effective.Model))
	line("effort", v.Effective.Effort)
	line("timeout", v.Effective.Timeout.String())
	line("timeout_policy", v.TimeoutPolicy)
	line("stop_requested", strconv.FormatBool(v.StopRequested))
	line("created_at", v.CreatedAt)
	line("started_at", dash(v.StartedAt))
	line("finished_at", dash(v.FinishedAt))
	line("elapsed_ms", strconv.Itoa(v.ElapsedMS))
	line("reconciling", strconv.FormatBool(v.Reconciling))
	line("completion_pending", strconv.FormatBool(v.CompletionPending))
	line("persistence_reason", dash(v.PersistenceReason))
	line("durability_confirmed", strconv.FormatBool(v.DurabilityConfirmed))
	renderTaskWorkspace(&b, v)
	if v.Reason != nil {
		line("reason", v.Reason.Code+" "+jsonString(v.Reason.Message))
		b.WriteString(renderCandidates("candidate: ", v.Candidates))
	}
	if r := v.Result; r != nil {
		exit := "-"
		if r.ExitCode != nil {
			exit = strconv.Itoa(*r.ExitCode)
		}
		msg := "null"
		if r.FinalMessage != nil {
			msg = jsonString(*r.FinalMessage)
		}
		line("exit_code", exit)
		line("signal", dash(r.Signal))
		line("final_message", msg)
		line("final_message_truncated", strconv.FormatBool(r.FinalMessageTruncated))
	}
	if l := v.LateResult; l != nil {
		exit := "-"
		if l.ExitCode != nil {
			exit = strconv.Itoa(*l.ExitCode)
		}
		msg := "null"
		if l.FinalMessage != nil {
			msg = jsonString(*l.FinalMessage)
		}
		line("late_result", "outcome="+l.Outcome+" digest="+l.Digest+" received_at="+l.ReceivedAt)
		line("late_exit_code", exit)
		line("late_signal", dash(l.Signal))
		line("late_final_message", msg)
		line("late_final_message_truncated", strconv.FormatBool(l.FinalMessageTruncated))
		line("late_log", fmt.Sprintf("retained_bytes=%d source_bytes=%d received_bytes=%d truncated=%t incomplete=%t",
			l.Log.RetainedBytes, l.Log.SourceBytes, l.Log.ReceivedBytes, l.Log.Truncated, l.Log.Incomplete))
	}
	m := v.Log
	line("log", fmt.Sprintf("retained_bytes=%d source_bytes=%d received_bytes=%d dropped_bytes=%d truncated=%t incomplete=%t counter_overflow=%t",
		m.RetainedBytes, m.SourceBytes, m.ReceivedBytes, m.DroppedBytes, m.Truncated, m.Incomplete, m.CounterOverflow))
	if v.Reconciling {
		b.WriteString("warning: " + contract.ReconcilingNotice + "\n")
	}
	if v.State == contract.TaskLost {
		b.WriteString("warning: the execution's outcome and cleanup are unconfirmed (lost); it is never retried or rerouted\n")
	}
	if v.TimeoutPolicy == contract.TimeoutPolicyLegacy {
		b.WriteString("notice: " + contract.TimeoutNotice + "\n")
	}
	if v.LateResult != nil {
		b.WriteString("notice: a late result was recorded after the task became final; the state is unchanged (callsheet task logs --late prints its output)\n")
	}
	if !v.DurabilityConfirmed {
		b.WriteString("warning: this task's durability is unconfirmed: the plane could not confirm its record on disk yet\n")
	}
	if v.PersistenceReason != nil {
		b.WriteString("warning: the task's result was received but could not be stored durably yet\n")
	}
	for _, n := range logNotices(m.Truncated, m.Incomplete, m.CounterOverflow, m.LogMayBeIncomplete, m.DroppedBytes) {
		b.WriteString("warning: " + n + "\n")
	}
	if v.TailTruncated {
		b.WriteString("warning: only the last lines are shown (callsheet task logs prints all retained output)\n")
	}
	b.WriteString("log_tail:\n")
	tail := SafeLog(v.LogTail)
	b.WriteString(tail)
	if tail != "" && !strings.HasSuffix(tail, "\n") {
		b.WriteString("\n")
	}
	return b.String()
}
