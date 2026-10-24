package reale2e

import (
	"encoding/json"
	"reflect"
	"strings"

	"github.com/wedevwork/callsheet/internal/contract"
)

// The exhaustive field classification manifest of the coordinator's
// permitted Callsheet call forms (design 12a-dev-flow-verify r0.6,
// Recorded Callsheet MCP verification boundary): every JSON member of
// every permitted request and response form, containers, optional
// branches and array elements included, has exactly one rule —
// contract-only (a named exclusion, or a contract-checked literal or
// form rule), request binding, authority binding, or a derived value or
// page constraint. It is written independently of the production judges;
// TestDevFullMCPFieldCoverage requires it to equal the field universe the
// contract types enumerate, and TestDevFullMCPBindingMatrix mutates every
// entry against the production per-call judge.

// wireShapes are the explicit wire shapes of the contract types whose
// custom marshalers render other members than their struct tags (each
// checked against an encoded sample by TestDevFullMCPFieldCoverage).
var wireShapes = map[reflect.Type][]wireMember{
	reflect.TypeOf(contract.DispatchRequest{}): {{"target", reflect.TypeOf(contract.TaskTarget{})}, {"goal", nil}, {"payload", reflect.TypeOf([]string{})},
		{"acceptance", nil}, {"requested_by", reflect.TypeOf(contract.RequestedBy{})}, {"override", reflect.TypeOf(contract.TaskOverride{})},
		{"workspace", nil}, {"base", nil}, {"workspace_instance", nil}},
	reflect.TypeOf(contract.TaskOverride{}):  {{"model", nil}, {"effort", nil}, {"timeout", nil}},
	reflect.TypeOf(contract.TaskEffective{}): {{"model", nil}, {"effort", nil}, {"timeout", nil}},
	reflect.TypeOf(contract.TaskLogsResponse{}): {{"version", nil}, {"task_id", nil}, {"data", nil}, {"retained_bytes", nil}, {"source_bytes", nil},
		{"dropped_bytes", nil}, {"truncated", nil}, {"incomplete", nil}, {"counter_overflow", nil}, {"log_may_be_incomplete", nil}},
	reflect.TypeOf(contract.TaskWorkspaceStatusResponse{}): {{"version", nil}, {"workspace", reflect.TypeOf(contract.TaskWorkspaceStatus{})}},
	// An asynchronous dispatch's wait_result is the literal null.
	reflect.TypeOf(contract.DispatchResponse{}): {{"version", nil}, {"task_id", nil}, {"task", reflect.TypeOf(contract.TaskView{})}, {"wait_result", nil}},
}

// tagMarshalers are the custom-marshaler types whose wire members are
// exactly their struct tags (empty lists rendered as []).
var tagMarshalers = map[reflect.Type]bool{
	reflect.TypeOf(contract.TaskView{}): true, reflect.TypeOf(contract.TaskResult{}): true, reflect.TypeOf(contract.TaskWorkspaceResult{}): true,
	reflect.TypeOf(contract.TaskListResponse{}): true,
}

// wireMember is one explicit wire member (nil type: a leaf).
type wireMember struct {
	name string
	typ  reflect.Type
}

// wireLeaves enumerates a type's JSON field universe under prefix: every
// member path, containers included ("[]" marks array elements); unknown
// custom marshalers are reported in unshaped.
func wireLeaves(t reflect.Type, prefix string, out map[string]bool, unshaped map[string]bool) {
	for t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	if t.Kind() == reflect.Slice && t.Elem().Kind() != reflect.Uint8 {
		elem := t.Elem()
		for elem.Kind() == reflect.Pointer {
			elem = elem.Elem()
		}
		if elem.Kind() == reflect.Struct {
			wireLeaves(elem, prefix+"[]", out, unshaped)
		} else {
			out[prefix+"[]"] = true
		}
		return
	}
	if t.Kind() != reflect.Struct {
		return
	}
	if shape, ok := wireShapes[t]; ok {
		for _, m := range shape {
			p := joinPath(prefix, m.name)
			out[p] = true
			if m.typ != nil {
				wireLeaves(m.typ, p, out, unshaped)
			}
		}
		return
	}
	if reflect.PointerTo(t).Implements(reflect.TypeOf((*json.Marshaler)(nil)).Elem()) || t.Implements(reflect.TypeOf((*json.Marshaler)(nil)).Elem()) {
		if !tagMarshalers[t] {
			unshaped[t.String()] = true
			return
		}
	}
	for i := 0; i < t.NumField(); i++ {
		f := t.Field(i)
		if !f.IsExported() {
			continue
		}
		name, _, _ := strings.Cut(f.Tag.Get("json"), ",")
		if name == "-" || name == "" {
			if f.Anonymous {
				wireLeaves(f.Type, prefix, out, unshaped)
			}
			continue
		}
		p := joinPath(prefix, name)
		out[p] = true
		wireLeaves(f.Type, p, out, unshaped)
	}
}

// taskShowResultTailRule is task_show's derived result.log_tail rule.
const taskShowResultTailRule = "equals resp:task.log_tail and satisfies the effective req:lines bound"

// Field rules.
const (
	ruleContract  = "contract"
	ruleRequest   = "request"
	ruleAuthority = "authority"
	ruleDerived   = "derived"
)

// Expected outcomes of a well-formed mutation of one member.
const (
	// expectAccept: an excluded member; a contract-valid change is
	// accepted.
	expectAccept = "accept"
	// expectContract: a contract-checked literal or form rule; a change is
	// refused by the shared validator or strict parser.
	expectContract = "contract"
	// expectBinding: a bound member; a well-formed change is refused by
	// the named binding (an *mcpBindingError, or the strict parser's own
	// equality where parserBound).
	expectBinding = "binding"
)

// fieldSpec is one member's rule, why (the exclusion or binding it
// follows) and the outcome its mutation must have.
type fieldSpec struct {
	rule, why, expect string
}

// mcpForm is one permitted call form: its tool, request and response
// types and its classified members ("req:" and "resp:" paths), with the
// response members bound to a request member (pairs, for coherent
// request-plus-response mutations) and those whose binding the strict
// contract parser itself enforces (parserBound).
type mcpForm struct {
	name, tool  string
	req, resp   reflect.Type
	fields      map[string]fieldSpec
	pairs       map[string]string
	parserBound map[string]bool
}

// taskViewRules classifies every TaskView member (relative paths; ""
// is the view itself): I(task) and, when terminal, state and the entire
// result are authority bindings (the request subtree by reqRule, which a
// dispatch binds to its validated request); scheduling/timing and log
// members are the design's named exclusions.
func taskViewRules(reqRule, reqWhy string) map[string]fieldSpec {
	m := map[string]fieldSpec{"": {ruleAuthority, "I(task) and terminal result", expectBinding}}
	set := func(spec fieldSpec, paths ...string) {
		for _, p := range paths {
			m[p] = spec
		}
	}
	set(fieldSpec{ruleAuthority, "I(task)", expectBinding}, "task_id", "role", "role.adapter", "role.id", "role.name", "role.node", "role.registration_order",
		"effective", "effective.effort", "effective.model", "effective.timeout", "timeout_policy", "created_at", "workspace_binding",
		"workspace_binding.base_commit", "workspace_binding.base_selector", "workspace_binding.instance", "workspace_binding.name",
		"request.requested_by", "request.requested_by.hostname", "request.requested_by.name", "request.requested_by.version")
	set(fieldSpec{reqRule, reqWhy, expectBinding}, "request", "request.acceptance", "request.base", "request.goal", "request.override",
		"request.override.effort", "request.override.model", "request.override.timeout", "request.payload", "request.payload[]", "request.target",
		"request.target.kind", "request.target.value", "request.workspace", "request.workspace_instance")
	set(fieldSpec{ruleAuthority, "terminal state equals the captured terminal state", expectBinding}, "state")
	set(fieldSpec{ruleAuthority, "terminal result equals the captured result", expectBinding}, "result", "result.changed_paths", "result.changed_paths[]",
		"result.changed_paths_next_after", "result.changed_paths_truncated", "result.diffstat", "result.diffstat.added", "result.diffstat.deleted",
		"result.diffstat.modified", "result.diffstat.new_bytes", "result.diffstat.old_bytes", "result.exit_code", "result.final_message",
		"result.final_message_truncated", "result.log_tail", "result.result_commit", "result.signal", "result.state", "result.workspace",
		"result.workspace.base_commit", "result.workspace.changes", "result.workspace.changes[].kind", "result.workspace.changes[].new_bytes",
		"result.workspace.changes[].new_mode", "result.workspace.changes[].old_bytes", "result.workspace.changes[].old_mode",
		"result.workspace.changes[].path_base64", "result.workspace.changes_truncated", "result.workspace.commit", "result.workspace.diffstat",
		"result.workspace.diffstat.added", "result.workspace.diffstat.deleted", "result.workspace.diffstat.modified", "result.workspace.diffstat.new_bytes",
		"result.workspace.diffstat.old_bytes", "result.workspace.error", "result.workspace.instance", "result.workspace.name", "result.workspace.next_after",
		"result.workspace.publication", "result.workspace.ref")
	set(fieldSpec{ruleContract, "exclusion: intermediate task timing/scheduling/liveness", expectAccept}, "stop_requested", "started_at", "finished_at",
		"elapsed_ms", "reconciling", "completion_pending", "persistence_reason", "durability_confirmed", "reason", "reason.code", "reason.message",
		"candidates", "candidates[].can_accept", "candidates[].concurrency", "candidates[].inflight", "candidates[].node_id", "candidates[].node_liveness",
		"candidates[].reason", "candidates[].reconciling_inflight", "candidates[].registration_order", "candidates[].role_id", "workspace_phase")
	set(fieldSpec{ruleContract, "exclusion: TaskView log, log_tail, tail_truncated, late_result", expectAccept}, "log", "log.counter_overflow",
		"log.dropped_bytes", "log.incomplete", "log.log_may_be_incomplete", "log.received_bytes", "log.retained_bytes", "log.source_bytes", "log.truncated",
		"log_tail", "tail_truncated", "late_result", "late_result.digest", "late_result.exit_code", "late_result.final_message",
		"late_result.final_message_truncated", "late_result.log", "late_result.log.counter_overflow", "late_result.log.dropped_bytes",
		"late_result.log.incomplete", "late_result.log.log_may_be_incomplete", "late_result.log.received_bytes", "late_result.log.retained_bytes",
		"late_result.log.source_bytes", "late_result.log.truncated", "late_result.outcome", "late_result.output_bytes", "late_result.received_at",
		"late_result.signal")
	return m
}

// underPrefix prefixes a rule set's relative paths.
func underPrefix(prefix string, rules map[string]fieldSpec, into map[string]fieldSpec) {
	for rel, spec := range rules {
		if rel == "" {
			into[prefix] = spec
			continue
		}
		into[joinPath(prefix, rel)] = spec
	}
}

// wsWorkspaceResultRules classifies a task workspace result DTO (the
// captured terminal result.workspace) under its relative paths.
func wsWorkspaceResultRules(why string) map[string]fieldSpec {
	spec := fieldSpec{ruleAuthority, why, expectBinding}
	m := map[string]fieldSpec{}
	for _, p := range []string{"", "base_commit", "changes", "changes[].kind", "changes[].new_bytes", "changes[].new_mode", "changes[].old_bytes",
		"changes[].old_mode", "changes[].path_base64", "changes_truncated", "commit", "diffstat", "diffstat.added", "diffstat.deleted", "diffstat.modified",
		"diffstat.new_bytes", "diffstat.old_bytes", "error", "instance", "name", "next_after", "publication", "ref"} {
		m[p] = spec
	}
	return m
}

// formAbsent classifies the WsFormArgs members another form owns: their
// presence is the shared validator's form refusal.
func formAbsent(fields map[string]fieldSpec, keys ...string) {
	for _, k := range keys {
		fields["req:"+k] = fieldSpec{ruleContract, "not a member of this form (shared validator refuses it)", expectContract}
	}
}

// mcpForms is the manifest: the twelve permitted call forms.
func mcpForms() []mcpForm {
	lit := func(why string) fieldSpec { return fieldSpec{ruleContract, why, expectContract} }
	excl := func(why string) fieldSpec { return fieldSpec{ruleContract, "exclusion: " + why, expectAccept} }
	bind := func(rule, why string) fieldSpec { return fieldSpec{rule, why, expectBinding} }
	var forms []mcpForm

	// dispatch
	f := mcpForm{name: "dispatch", tool: toolDispatch, req: reflect.TypeOf(contract.DispatchArgs{}), resp: reflect.TypeOf(contract.DispatchResponse{}),
		fields: map[string]fieldSpec{}, pairs: map[string]string{}, parserBound: map[string]bool{}}
	prepared := bind(ruleAuthority, "the prepared dispatch document (no payload, override or wait)")
	for _, k := range []string{"acceptance", "base", "goal", "override", "override.effort", "override.model", "override.timeout", "payload", "payload[]",
		"target", "target.kind", "target.value", "wait", "workspace", "workspace_instance"} {
		f.fields["req:"+k] = prepared
	}
	f.fields["resp:version"] = lit("protocol version literal")
	f.fields["resp:wait_result"] = lit("an asynchronous dispatch's wait_result is null")
	f.fields["resp:task_id"] = bind(ruleDerived, "the outer task_id is the nested task's")
	underPrefix("resp:task", taskViewRules(ruleRequest, "the returned request is the validated request with the captured requester"), f.fields)
	for _, k := range []string{"acceptance", "base", "goal", "target.kind", "target.value", "workspace", "workspace_instance"} {
		f.pairs["resp:task.request."+k] = "req:" + k
	}
	forms = append(forms, f)

	// task_ls
	f = mcpForm{name: "task_ls", tool: toolTaskList, req: reflect.TypeOf(contract.TaskListArgs{}), resp: reflect.TypeOf(contract.TaskListResponse{}),
		fields: map[string]fieldSpec{
			"req:after":       bind(ruleDerived, "every entry is after the exclusive cursor"),
			"req:limit":       bind(ruleDerived, "at most the effective limit entries"),
			"resp:version":    lit("protocol version literal"),
			"resp:tasks":      bind(ruleDerived, "strictly ascending unique IDs after the cursor, at most limit"),
			"resp:next_after": excl("admission-set completeness (the supervisor sweeps prove it; next_after keeps the parser's last-ID rule)"),
		}, pairs: map[string]string{}, parserBound: map[string]bool{}}
	summary := bind(ruleAuthority, "the captured task's immutable summary projection")
	for _, k := range []string{"tasks[].task_id", "tasks[].target", "tasks[].target.kind", "tasks[].target.value", "tasks[].role", "tasks[].role.adapter",
		"tasks[].role.id", "tasks[].role.name", "tasks[].role.node", "tasks[].role.registration_order", "tasks[].effective", "tasks[].effective.effort",
		"tasks[].effective.model", "tasks[].effective.timeout", "tasks[].created_at", "tasks[].requested_by", "tasks[].requested_by.hostname",
		"tasks[].requested_by.name", "tasks[].requested_by.version"} {
		f.fields["resp:"+k] = summary
	}
	f.fields["resp:tasks[].state"] = bind(ruleAuthority, "a terminal state equals the captured terminal state")
	for _, k := range []string{"tasks[].stop_requested", "tasks[].started_at", "tasks[].finished_at", "tasks[].elapsed_ms", "tasks[].reconciling",
		"tasks[].completion_pending", "tasks[].persistence_reason", "tasks[].durability_confirmed", "tasks[].reason", "tasks[].reason.code",
		"tasks[].reason.message"} {
		f.fields["resp:"+k] = excl("intermediate task timing/scheduling/liveness")
	}
	forms = append(forms, f)

	// task_show
	f = mcpForm{name: "task_show", tool: toolTaskShow, req: reflect.TypeOf(contract.TaskShowArgs{}), resp: reflect.TypeOf(contract.TaskShowResponse{}),
		fields: map[string]fieldSpec{
			"req:id":       bind(ruleRequest, "the returned task_id is the requested id"),
			"req:lines":    bind(ruleDerived, "log_tail holds at most the requested lines"),
			"resp:version": lit("protocol version literal"),
		}, pairs: map[string]string{"resp:task.task_id": "req:id"}, parserBound: map[string]bool{}}
	underPrefix("resp:task", taskViewRules(ruleAuthority, "I(task)"), f.fields)
	f.fields["resp:task.task_id"] = bind(ruleRequest, "the requested id, resolved in the captured authority")
	f.fields["resp:task.log_tail"] = bind(ruleDerived, "at most the requested lines (contract LogTail counting)")
	// Design r0.12: task_show's nested tail is the request-selected
	// display tail (dispatch and task_cancel keep it an authority binding).
	f.fields["resp:task.result.log_tail"] = bind(ruleDerived, taskShowResultTailRule)
	forms = append(forms, f)

	// task_logs
	f = mcpForm{name: "task_logs", tool: toolTaskLogs, req: reflect.TypeOf(contract.TaskLogsArgs{}), resp: reflect.TypeOf(contract.TaskLogsResponse{}),
		fields: map[string]fieldSpec{
			"req:id":       bind(ruleRequest, "the answered task_id is the requested captured task"),
			"req:late":     excl("diagnostic task_logs: the response does not echo late"),
			"resp:version": lit("protocol version literal"),
			"resp:task_id": bind(ruleRequest, "the requested id, resolved in the captured authority"),
		}, pairs: map[string]string{"resp:task_id": "req:id"}, parserBound: map[string]bool{}}
	for _, k := range []string{"data", "retained_bytes", "source_bytes", "dropped_bytes", "truncated", "incomplete", "counter_overflow", "log_may_be_incomplete"} {
		f.fields["resp:"+k] = excl("diagnostic task_logs fields other than version and task_id")
	}
	forms = append(forms, f)

	// task_cancel
	f = mcpForm{name: "task_cancel", tool: toolTaskCancel, req: reflect.TypeOf(contract.TaskCancelArgs{}), resp: reflect.TypeOf(contract.CancelResponse{}),
		fields: map[string]fieldSpec{
			"req:id":        bind(ruleRequest, "the outer and nested task IDs are the requested id"),
			"resp:version":  lit("protocol version literal"),
			"resp:task_id":  bind(ruleRequest, "the requested id"),
			"resp:accepted": excl("task_cancel accepted is acknowledgement only (200/202 consistency by the parser)"),
		}, pairs: map[string]string{"resp:task_id": "req:id", "resp:task.task_id": "req:id"}, parserBound: map[string]bool{}}
	underPrefix("resp:task", taskViewRules(ruleAuthority, "I(task)"), f.fields)
	forms = append(forms, f)

	// ws_push
	seed := bind(ruleAuthority, "the prepared seed publication")
	f = mcpForm{name: "ws_push", tool: toolWsPush, req: reflect.TypeOf(contract.WsPushArgs{}), resp: reflect.TypeOf(contract.WorkspacePushResult{}),
		fields: map[string]fieldSpec{"req:name": seed, "req:instance": seed, "req:path": seed,
			"req:branch": bind(ruleRequest, "ValidatePush's canonical ref is refs/heads/main and the answered branch"),
			"resp:name":  seed, "resp:instance": seed, "resp:commit": seed,
			"resp:branch":      bind(ruleRequest, "the canonical requested branch"),
			"resp:old_commit":  excl("ws_push old_commit, source_kind, changed"),
			"resp:source_kind": excl("ws_push old_commit, source_kind, changed"),
			"resp:changed":     excl("ws_push old_commit, source_kind, changed"),
		}, pairs: map[string]string{"resp:name": "req:name", "resp:instance": "req:instance", "resp:branch": "req:branch"}, parserBound: map[string]bool{}}
	forms = append(forms, f)

	// ws_status, task form
	f = mcpForm{name: "ws_status/task", tool: toolWsStatus, req: reflect.TypeOf(contract.WsFormArgs{}), resp: reflect.TypeOf(contract.TaskWorkspaceStatusResponse{}),
		fields: map[string]fieldSpec{
			"req:task_id":                    bind(ruleRequest, "workspace.task_id is the requested captured task"),
			"resp:version":                   lit("protocol version literal"),
			"resp:workspace":                 bind(ruleAuthority, "the task's captured binding and terminal result"),
			"resp:workspace.task_id":         bind(ruleRequest, "the requested task"),
			"resp:workspace.state":           bind(ruleAuthority, "a terminal state equals the captured state"),
			"resp:workspace.workspace_phase": excl("ws_status task-form nonterminal state/phase"),
			"resp:workspace.available":       excl("ws_status task-form instantaneous available"),
		}, pairs: map[string]string{"resp:workspace.task_id": "req:task_id"}, parserBound: map[string]bool{"resp:workspace.result.ref": true}}
	formAbsent(f.fields, "name", "base", "target", "after", "instance", "generation", "limit")
	for _, k := range []string{"binding", "binding.base_commit", "binding.base_selector", "binding.instance", "binding.name"} {
		f.fields["resp:workspace."+k] = bind(ruleAuthority, "the task's entire captured workspace_binding")
	}
	underPrefix("resp:workspace.result", wsWorkspaceResultRules("a terminal task's entire captured result.workspace"), f.fields)
	forms = append(forms, f)

	// ws_status, explicit form
	ws := bind(ruleAuthority, "the manifest workspace identity")
	f = mcpForm{name: "ws_status/explicit", tool: toolWsStatus, req: reflect.TypeOf(contract.WsFormArgs{}), resp: reflect.TypeOf(contract.WorkspaceStatusResponse{}),
		fields: map[string]fieldSpec{"req:name": ws,
			"req:after":      bind(ruleDerived, "rows after the exclusive ref cursor, linked to the prior page"),
			"req:instance":   bind(ruleDerived, "a continuation's instance is the answer's"),
			"req:generation": bind(ruleDerived, "a continuation's generation is the answer's"),
			"req:limit":      bind(ruleDerived, "at most limit refs"),
			"resp:name":      ws, "resp:instance": ws,
			"resp:generation":          bind(ruleDerived, "a continuation's generation (a first page's token is opaque)"),
			"resp:default_branch":      lit("refs/heads/main literal"),
			"resp:refs":                bind(ruleDerived, "parser ordering, exclusive after, limit"),
			"resp:refs[].name":         bind(ruleAuthority, "the seed main ref or a captured published task ref"),
			"resp:refs[].commit":       bind(ruleAuthority, "the ref's captured commit"),
			"resp:refs[].published_at": excl("explicit-form published_at"),
			"resp:next_after":          excl("explicit-form live ref-set completeness (next_after keeps the parser's last-ref rule)"),
		}, pairs: map[string]string{"resp:name": "req:name"}, parserBound: map[string]bool{"req:after": true, "req:limit": true}}
	formAbsent(f.fields, "task_id", "base", "target")
	forms = append(forms, f)

	// ws_diff, task form and explicit form
	for _, task := range []bool{true, false} {
		f = mcpForm{name: "ws_diff/explicit", tool: toolWsDiff, req: reflect.TypeOf(contract.WsFormArgs{}), resp: reflect.TypeOf(contract.WorkspaceDiffResponse{}),
			fields: map[string]fieldSpec{
				"req:after":       bind(ruleDerived, "changes after the exclusive byte-path cursor, linked to the prior page"),
				"req:instance":    bind(ruleDerived, "a continuation's instance is the answer's"),
				"req:generation":  bind(ruleDerived, "a continuation's generation is the answer's"),
				"req:limit":       bind(ruleDerived, "the effective row limit"),
				"resp:generation": bind(ruleDerived, "a continuation's generation (a first page's token is opaque)"),
				"resp:changes":    bind(ruleDerived, "the page recomputed from the verified trees"),
				"resp:next_after": bind(ruleDerived, "the last emitted path when rows remain, null at exhaustion"),
			}, pairs: map[string]string{}, parserBound: map[string]bool{"req:after": true}}
		for _, k := range []string{"changes[].kind", "changes[].new_bytes", "changes[].new_mode", "changes[].old_bytes", "changes[].old_mode", "changes[].path_base64"} {
			f.fields["resp:"+k] = bind(ruleDerived, "the recomputed change row")
		}
		if task {
			f.name = "ws_diff/task"
			f.fields["req:task_id"] = bind(ruleRequest, "the requested task's captured binding and published result")
			for _, k := range []string{"name", "instance", "base_commit", "target_commit"} {
				f.fields["resp:"+k] = bind(ruleAuthority, "the requested task's captured binding and published result")
			}
			formAbsent(f.fields, "name", "base", "target")
		} else {
			f.fields["req:name"] = ws
			f.fields["req:base"] = bind(ruleAuthority, "resolved against the captured seed, task refs or verified objects")
			f.fields["req:target"] = bind(ruleAuthority, "resolved against the captured seed, task refs or verified objects")
			f.fields["resp:name"], f.fields["resp:instance"] = ws, ws
			f.fields["resp:base_commit"] = bind(ruleAuthority, "the resolved base")
			f.fields["resp:target_commit"] = bind(ruleAuthority, "the resolved target")
			f.pairs["resp:name"] = "req:name"
			f.pairs["resp:target_commit"] = "req:target"
			formAbsent(f.fields, "task_id")
		}
		forms = append(forms, f)
	}

	// ws_pull, task form and explicit form
	for _, task := range []bool{true, false} {
		f = mcpForm{name: "ws_pull/explicit", tool: toolWsPull, req: reflect.TypeOf(contract.WsPullArgs{}), resp: reflect.TypeOf(contract.WorkspacePullResult{}),
			fields: map[string]fieldSpec{
				"req:path":              excl("ws_pull local destination (path syntax is validated)"),
				"resp:destination_kind": excl("ws_pull destination_kind, local_ref, old_commit, changed"),
				"resp:local_ref":        excl("ws_pull destination_kind, local_ref, old_commit, changed"),
				"resp:old_commit":       excl("ws_pull destination_kind, local_ref, old_commit, changed"),
				"resp:changed":          excl("ws_pull destination_kind, local_ref, old_commit, changed"),
			}, pairs: map[string]string{}, parserBound: map[string]bool{}}
		if task {
			f.name = "ws_pull/task"
			f.fields["req:task_id"] = bind(ruleRequest, "the canonical selector is the requested task's ref")
			f.fields["req:name"] = lit("not a member of this form (shared validator refuses it)")
			f.fields["req:ref"] = lit("not a member of this form (shared validator refuses it)")
			f.fields["resp:selector"] = bind(ruleRequest, "refs/callsheet/tasks/<requested task_id>")
			for _, k := range []string{"name", "instance", "commit"} {
				f.fields["resp:"+k] = bind(ruleAuthority, "the requested task's captured binding and published result")
			}
			f.pairs["resp:selector"] = "req:task_id"
		} else {
			f.fields["req:task_id"] = lit("not a member of this form (shared validator refuses it)")
			f.fields["req:name"] = ws
			f.fields["req:ref"] = bind(ruleRequest, "the canonical validated selector")
			f.fields["resp:name"], f.fields["resp:instance"] = ws, ws
			f.fields["resp:selector"] = bind(ruleRequest, "the canonical validated request selector")
			f.fields["resp:commit"] = bind(ruleAuthority, "the commit the selector resolves to in the captured authority")
			f.pairs["resp:name"] = "req:name"
			f.pairs["resp:selector"] = "req:ref"
		}
		forms = append(forms, f)
	}
	return forms
}
