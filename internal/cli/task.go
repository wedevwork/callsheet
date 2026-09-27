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
	dispatchUsage = "(--role-id ID | --role-name NAME) --goal TEXT --acceptance TEXT [--payload POINTER ...] [--model MODEL] [--effort EFFORT] [--timeout DURATION] " + trustUsage + " [--json]"
	taskLsUsage   = "[--limit N] [--after ID] " + trustUsage + " [--json]"
	taskShowUsage = "ID [--lines N] " + trustUsage + " [--json]"
	taskLogsUsage = "ID " + trustUsage + " [--json]"

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
		"  --timeout DURATION record a timeout for this task, e.g. 90m; 0 is unlimited. The\n" +
		"                     recorded timeout is not enforced in this build\n" +
		trustHelp + taskJSONHelp + "\n" +
		"Exactly one of --role-id and --role-name is required. The plane reserves one of the\n" +
		"role's slots atomically, or fails at once listing every candidate with its state:\n" +
		"there is no queue and no reroute. The worker composes the prompt from its local\n" +
		"manuals and this task, and runs the adapter's CLI child in a fresh scratch directory.\n" +
		"Exit 0 means the task was admitted, not that it succeeded: follow it with callsheet\n" +
		"task show. A lost response may hide an admitted task; check callsheet task ls before\n" +
		"dispatching again. Workspaces and waiting are not supported in this build.\n\n" +
		"Example, on an operator machine:\n" +
		"  callsheet dispatch --role-name implementer --goal \"fix the parser\" \\\n" +
		"    --acceptance \"all tests pass\" --payload repo://callsheet \\\n" +
		"    --plane https://plane.example:8443 --ca plane-ca.crt\n"
	taskLsDetails = "Flags:\n" +
		"  --limit N          at most N tasks (1-100, default 100)\n" +
		"  --after ID         start after this task ID (the previous page's next_after)\n" +
		trustHelp + taskJSONHelp + "\n" +
		"Lists tasks from every coordinator, ordered by task ID, as tab-separated columns\n" +
		"TASK_ID, STATE, ROLE_ID, NODE, ELAPSED_MS and RECOVERY_REQUIRED, with a next_after\n" +
		"line when more tasks exist. Tasks created after a page was read may sort before its\n" +
		"cursor: list again from the start to see them.\n"
	taskShowDetails = "Flags:\n" +
		"  --lines N          the last N lines of retained output (0-200, default 20)\n" +
		trustHelp + taskJSONHelp + "\n" +
		"Shows one task as label: value lines: request, role, effective settings, state,\n" +
		"timestamps, result and the last lines of output, with warnings when the outcome is\n" +
		"unconfirmed, durability is unconfirmed or output was truncated. Output is shown with\n" +
		"terminal control bytes escaped. A task whose outcome is unconfirmed stays pending or\n" +
		"running: automatic recovery is not available in this build (callsheet role rm can\n" +
		"retire such an instance).\n"
	taskLogsDetails = "Flags:\n" + trustHelp +
		"  --json             print the plane's response envelope (base64 data, byte exact)\n\n" +
		"Prints the task's complete retained output (at most the last 10 MiB) with terminal\n" +
		"control bytes escaped; truncation and incompleteness notices go to stderr.\n"
)

// taskFlags are the task leaves' own flags.
type taskFlags struct {
	roleID, roleName, goal, acceptance, model, effort, timeout single
	payload                                                    multi
	limit, after, lines                                        single
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
	rb, err := requester()
	if err != nil {
		return planeFail(errOut, err)
	}
	req.RequestedBy = rb
	if err := req.Validate(); err != nil {
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
	v, err := cl.Dispatch(ctx, req)
	if err != nil {
		return taskFail(errOut, err)
	}
	if f.json.val {
		return writeJSON(out, errOut, contract.DispatchResponse{Version: contract.ProtocolVersion, TaskID: v.TaskID, Task: v})
	}
	return writeOut(out, errOut, RenderDispatch(v))
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
	r, err := cl.TaskLogs(ctx, id)
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
		fmt.Fprintf(&b, "%srole_id=%s node=%s registration_order=%d node_liveness=%s inflight=%d recovery_inflight=%d concurrency=%d can_accept=%t reason=%s\n",
			prefix, c.RoleID, c.NodeID, c.RegistrationOrder, c.NodeLiveness, c.Inflight, c.RecoveryInflight, c.Concurrency, c.CanAccept, c.Reason)
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
		{"timeout", v.Effective.Timeout.String()}, {"notice", contract.TimeoutNotice},
	} {
		b.WriteString(kv[0] + ": " + kv[1] + "\n")
	}
	if v.Reason != nil {
		b.WriteString("reason: " + v.Reason.Code + " " + jsonString(v.Reason.Message) + "\n")
		b.WriteString(renderCandidates("candidate: ", v.Candidates))
	}
	return b.String()
}

// TaskColumns is the text header of task ls.
var TaskColumns = []string{"TASK_ID", "STATE", "ROLE_ID", "NODE", "ELAPSED_MS", "RECOVERY_REQUIRED"}

// RenderTasks renders the header, one tab-separated row per task and a
// next_after footer when more tasks exist.
func RenderTasks(tasks []contract.TaskSummary, next *string) string {
	var b strings.Builder
	b.WriteString(joinFields(TaskColumns))
	for _, s := range tasks {
		b.WriteString(joinFields([]string{s.TaskID, s.State, s.Role.ID, s.Role.Node, strconv.Itoa(s.ElapsedMS), strconv.FormatBool(s.RecoveryRequired)}))
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
	line("timeout_enforced", "false")
	line("created_at", v.CreatedAt)
	line("started_at", dash(v.StartedAt))
	line("finished_at", dash(v.FinishedAt))
	line("elapsed_ms", strconv.Itoa(v.ElapsedMS))
	line("recovery_required", strconv.FormatBool(v.RecoveryRequired))
	line("recovery_reason", dash(v.RecoveryReason))
	line("completion_pending", strconv.FormatBool(v.CompletionPending))
	line("persistence_reason", dash(v.PersistenceReason))
	line("durability_confirmed", strconv.FormatBool(v.DurabilityConfirmed))
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
	m := v.Log
	line("log", fmt.Sprintf("retained_bytes=%d source_bytes=%d received_bytes=%d dropped_bytes=%d truncated=%t incomplete=%t counter_overflow=%t",
		m.RetainedBytes, m.SourceBytes, m.ReceivedBytes, m.DroppedBytes, m.Truncated, m.Incomplete, m.CounterOverflow))
	if v.RecoveryRequired {
		b.WriteString("warning: " + contract.RecoveryNotice + "\n")
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
