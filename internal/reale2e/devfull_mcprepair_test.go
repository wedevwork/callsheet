package reale2e

import (
	"encoding/base64"
	"strings"

	"github.com/wedevwork/callsheet/internal/contract"
)

// Coherent mutations for the binding matrix: the contract couples some
// members (a terminal view's state and its result's state, the result's
// public mirrors and its workspace DTO, a binding's identity in the
// request, binding and result, log counters). After one member is
// mutated, its coupled members follow it so the mutated response stays
// well formed and the rejection (or acceptance) is the binding's, never
// the parser's.

// obj is the object at key of m (nil when absent or not an object).
func obj(m map[string]any, key string) map[string]any {
	o, _ := m[key].(map[string]any)
	return o
}

// num reads a JSON number as an int.
func num(m map[string]any, key string) int {
	f, _ := m[key].(float64)
	return int(f)
}

// repairResponse keeps a form's mutated response coherent (path is the
// mutated response member).
func repairResponse(form string, root map[string]any, path string) {
	switch form {
	case "dispatch", "task_show", "task_cancel":
		rel, ok := strings.CutPrefix(path, "task.")
		v := obj(root, "task")
		if v == nil {
			return
		}
		if path == "task_id" {
			// The outer task_id names the nested task.
			v["task_id"] = root["task_id"]
			repairView(v, "task_id")
			return
		}
		if ok {
			repairView(v, rel)
		}
		if form != "task_show" && (path == "task.task_id" || path == "task") {
			root["task_id"] = v["task_id"]
		}
	case "ws_status/task":
		w := obj(root, "workspace")
		if w == nil {
			return
		}
		rel, _ := strings.CutPrefix(path, "workspace.")
		if r := obj(w, "result"); r != nil {
			if wr, ok := strings.CutPrefix(rel, "result."); ok {
				repairWsResult(r, wr)
			}
			b := obj(w, "binding")
			switch rel {
			case "binding.name", "result.name":
				b["name"], r["name"] = valueAt(w, rel), valueAt(w, rel)
			case "binding.instance", "result.instance":
				b["instance"], r["instance"] = valueAt(w, rel), valueAt(w, rel)
			case "binding.base_commit", "result.base_commit", "binding.base_selector":
				val := valueAt(w, rel)
				b["base_commit"], r["base_commit"], b["base_selector"] = val, val, val
			case "task_id":
				if r["ref"] != nil {
					r["ref"] = contract.TaskRefPrefix + w["task_id"].(string)
				}
			}
		}
	case "task_logs":
		repairLogsResponse(root, path)
	case "ws_push":
		if root["changed"] == false {
			root["old_commit"] = root["commit"]
		}
	case "ws_pull/task", "ws_pull/explicit":
		git := func() {
			root["destination_kind"] = contract.KindGit
			root["local_ref"] = contract.LocalRefFor(root["name"].(string), root["selector"].(string))
		}
		switch path {
		case "destination_kind", "local_ref":
			git()
		case "old_commit":
			git()
			root["changed"] = root["old_commit"] != root["commit"]
		case "changed":
			git()
			if root["changed"] == false {
				root["old_commit"] = root["commit"]
			}
		}
	}
	if form == "task_cancel" {
		if v := obj(root, "task"); v != nil && (v["durability_confirmed"] == false || v["stop_requested"] == true) {
			root["accepted"] = true
		}
	}
	if form == "ws_status/task" {
		if w := obj(root, "workspace"); w != nil {
			if r := obj(w, "result"); r == nil || r["publication"] != contract.PublicationPublished {
				w["available"] = false
			}
		}
	}
}

// valueAt is the member at a relative path of m.
func valueAt(m map[string]any, rel string) any {
	v, _ := getAt(m, rel)
	return v
}

// repairView keeps a mutated task view coherent.
func repairView(v map[string]any, rel string) {
	r := obj(v, "result")
	switch {
	case rel == "state", rel == "result.state":
		s := valueAt(v, rel)
		v["state"] = s
		if r != nil {
			r["state"] = s
			if s == contract.TaskFailed {
				if c, ok := r["exit_code"].(float64); !ok || c == 0 {
					r["exit_code"] = float64(1)
				}
			}
		}
	case rel == "log_tail", rel == "result.log_tail":
		t := valueAt(v, rel)
		v["log_tail"] = t
		if r != nil {
			r["log_tail"] = t
		}
	case strings.HasPrefix(rel, "log."):
		repairLog(obj(v, "log"), strings.TrimPrefix(rel, "log."))
	case strings.HasPrefix(rel, "late_result.log."):
		repairLog(obj(obj(v, "late_result"), "log"), strings.TrimPrefix(rel, "late_result.log."))
	case rel == "late_result.signal":
		obj(v, "late_result")["exit_code"] = nil
	case rel == "late_result.exit_code":
		obj(v, "late_result")["signal"] = nil
	case rel == "result.exit_code" && r != nil:
		r["signal"] = nil
	case rel == "result.signal" && r != nil:
		r["exit_code"] = nil
	case strings.HasPrefix(rel, "result.workspace."):
		repairWsResult(obj(r, "workspace"), strings.TrimPrefix(rel, "result.workspace."))
	case strings.HasPrefix(rel, "result.") && r != nil:
		mirrorToSource(r, strings.TrimPrefix(rel, "result."))
	case strings.HasPrefix(rel, "candidates[]."):
		c := v["candidates"].([]any)[0].(map[string]any)
		switch strings.TrimPrefix(rel, "candidates[].") {
		case "can_accept":
			if c["can_accept"] == true {
				c["reason"] = contract.ReasonAvailable
			} else {
				c["reason"] = contract.ReasonFull
			}
		case "reason":
			c["can_accept"] = c["reason"] == contract.ReasonAvailable
		case "reconciling_inflight":
			if num(c, "reconciling_inflight") > num(c, "inflight") {
				c["inflight"] = c["reconciling_inflight"]
			}
		}
	}
	// The binding's identity in the request, the binding and the result.
	b := obj(v, "workspace_binding")
	req := obj(v, "request")
	var w map[string]any
	if r != nil {
		w = obj(r, "workspace")
	}
	sync := func(val any, members ...[2]any) {
		for _, m := range members {
			if o, _ := m[0].(map[string]any); o != nil {
				o[m[1].(string)] = val
			}
		}
	}
	if b != nil {
		switch rel {
		case "request.workspace", "workspace_binding.name", "result.workspace.name":
			sync(valueAt(v, rel), [2]any{req, "workspace"}, [2]any{b, "name"}, [2]any{w, "name"})
		case "request.workspace_instance", "workspace_binding.instance", "result.workspace.instance":
			sync(valueAt(v, rel), [2]any{req, "workspace_instance"}, [2]any{b, "instance"}, [2]any{w, "instance"})
		case "request.base", "workspace_binding.base_selector", "workspace_binding.base_commit", "result.workspace.base_commit":
			// A hash base: the request's base, the canonical selector and
			// the resolved commit are one value.
			sync(valueAt(v, rel), [2]any{req, "base"}, [2]any{b, "base_selector"}, [2]any{b, "base_commit"}, [2]any{w, "base_commit"})
		case "task_id":
			if w != nil && w["ref"] != nil {
				w["ref"] = contract.TaskRefPrefix + v["task_id"].(string)
			}
		}
	}
	if r != nil {
		mirrorsFromSource(r)
	}
}

// mirrorToSource applies a mutated public mirror of a result to its
// workspace DTO (the mirror's source).
func mirrorToSource(r map[string]any, rel string) {
	w := obj(r, "workspace")
	if w == nil {
		return
	}
	switch {
	case rel == "result_commit":
		w["commit"] = r["result_commit"]
	case strings.HasPrefix(rel, "diffstat"):
		w["diffstat"] = r["diffstat"]
	case rel == "changed_paths", rel == "changed_paths[]":
		paths, _ := r["changed_paths"].([]any)
		changes := []any{}
		for _, p := range paths {
			changes = append(changes, map[string]any{"path_base64": p, "kind": contract.ChangeAdded, "old_mode": nil, "new_mode": "100644",
				"old_bytes": nil, "new_bytes": float64(1)})
		}
		w["changes"] = changes
	case rel == "changed_paths_truncated":
		w["changes_truncated"] = r["changed_paths_truncated"]
		repairWsResult(w, "changes_truncated")
	case rel == "changed_paths_next_after":
		w["next_after"] = r["changed_paths_next_after"]
		repairWsResult(w, "next_after")
	}
}

// mirrorsFromSource recomputes a result's public mirrors from its
// workspace DTO.
func mirrorsFromSource(r map[string]any) {
	w := obj(r, "workspace")
	if w == nil || w["publication"] != contract.PublicationPublished {
		r["result_commit"], r["diffstat"], r["changed_paths"], r["changed_paths_truncated"], r["changed_paths_next_after"] = nil, nil, []any{}, false, nil
		return
	}
	paths := []any{}
	changes, _ := w["changes"].([]any)
	for _, c := range changes {
		paths = append(paths, c.(map[string]any)["path_base64"])
	}
	r["result_commit"], r["diffstat"], r["changed_paths"], r["changed_paths_truncated"], r["changed_paths_next_after"] =
		w["commit"], w["diffstat"], paths, w["changes_truncated"], w["next_after"]
}

// repairWsResult keeps a mutated task workspace result DTO coherent.
func repairWsResult(w map[string]any, rel string) {
	if w == nil {
		return
	}
	changes, _ := w["changes"].([]any)
	last := func() any {
		if len(changes) == 0 {
			return nil
		}
		return changes[len(changes)-1].(map[string]any)["path_base64"]
	}
	switch rel {
	case "publication", "error":
		if rel == "publication" && w["publication"] == contract.PublicationPublished {
			return
		}
		w["publication"], w["commit"], w["ref"], w["diffstat"], w["next_after"], w["changes"], w["changes_truncated"] =
			contract.PublicationFailed, nil, nil, nil, nil, []any{}, false
		if e, ok := w["error"].(string); !ok || !contract.ValidPublicationError(e) {
			w["error"] = contract.PubErrSnapshotFailed
		}
	case "changes_truncated":
		if w["changes_truncated"] == true {
			w["next_after"] = last()
		} else {
			w["next_after"] = nil
		}
	case "next_after":
		w["changes_truncated"] = true
		if len(changes) > 0 {
			changes[len(changes)-1].(map[string]any)["path_base64"] = w["next_after"]
		}
	case "changes[].kind", "changes[].old_mode", "changes[].old_bytes", "changes[].new_mode", "changes[].new_bytes":
		c := changes[0].(map[string]any)
		switch rel {
		case "changes[].kind":
			switch c["kind"] {
			case contract.ChangeModified:
				c["old_mode"], c["old_bytes"], c["new_mode"], c["new_bytes"] = "100644", float64(1), "100644", float64(1)
			case contract.ChangeAdded:
				c["old_mode"], c["old_bytes"] = nil, nil
				c["new_mode"], c["new_bytes"] = "100644", float64(1)
			case contract.ChangeDeleted:
				c["new_mode"], c["new_bytes"] = nil, nil
				c["old_mode"], c["old_bytes"] = "100644", float64(1)
			}
		case "changes[].old_mode", "changes[].old_bytes":
			if c["old_mode"] == nil || c["old_bytes"] == nil {
				c["old_mode"], c["old_bytes"] = "100644", c["old_bytes"]
				if c["old_bytes"] == nil {
					c["old_bytes"] = float64(1)
				}
			}
			if c["kind"] == contract.ChangeAdded {
				c["kind"] = contract.ChangeModified
			}
		case "changes[].new_mode", "changes[].new_bytes":
			if c["new_mode"] == nil || c["new_bytes"] == nil {
				c["new_mode"] = "100644"
				if c["new_bytes"] == nil {
					c["new_bytes"] = float64(1)
				}
			}
			if c["kind"] == contract.ChangeDeleted {
				c["kind"] = contract.ChangeModified
			}
		}
	}
	if w["changes_truncated"] == true && len(changes) > 0 {
		w["next_after"] = last()
	}
}

// repairLog keeps mutated log metadata coherent (counters ordered,
// dropped and truncated derived, overflow only when incomplete).
func repairLog(m map[string]any, rel string) {
	if m == nil {
		return
	}
	r, s, rc := num(m, "retained_bytes"), num(m, "source_bytes"), num(m, "received_bytes")
	switch rel {
	case "retained_bytes":
		rc, s = max(rc, r), max(s, max(rc, r))
	case "received_bytes":
		s, r = max(s, rc), min(r, rc)
	case "source_bytes":
		rc = min(rc, s)
		r = min(r, rc)
	case "dropped_bytes":
		s = r + num(m, "dropped_bytes")
		rc = max(rc, r)
	case "truncated":
		if m["truncated"] == true && rc == r {
			m["incomplete"] = true
		}
		if m["truncated"] == false {
			m["incomplete"], m["counter_overflow"], rc = false, false, r
		}
	case "incomplete":
		if m["incomplete"] == false {
			m["counter_overflow"] = false
		}
	case "counter_overflow":
		if m["counter_overflow"] == true {
			m["incomplete"] = true
		}
	}
	m["retained_bytes"], m["source_bytes"], m["received_bytes"] = float64(r), float64(s), float64(rc)
	m["dropped_bytes"] = float64(max(0, s-r))
	m["truncated"] = rc > r || m["incomplete"] == true
}

// repairLogsResponse keeps a mutated task_logs answer coherent.
func repairLogsResponse(m map[string]any, path string) {
	data, _ := base64.StdEncoding.DecodeString(m["data"].(string))
	switch path {
	case "retained_bytes":
		for len(data) < num(m, "retained_bytes") {
			data = append(data, 'x')
		}
		m["data"] = base64.StdEncoding.EncodeToString(data)
	case "data":
		m["retained_bytes"] = float64(len(data))
	case "dropped_bytes":
		m["source_bytes"] = float64(len(data) + num(m, "dropped_bytes"))
	case "incomplete":
		if m["incomplete"] == true {
			m["truncated"] = true
		} else {
			m["counter_overflow"] = false
		}
	case "counter_overflow":
		if m["counter_overflow"] == true {
			m["incomplete"], m["truncated"] = true, true
		}
	case "truncated":
		if m["truncated"] == false {
			m["incomplete"], m["counter_overflow"] = false, false
		}
	}
	m["retained_bytes"] = float64(len(data))
	m["source_bytes"] = float64(max(num(m, "source_bytes"), len(data)))
	m["dropped_bytes"] = float64(max(0, num(m, "source_bytes")-len(data)))
}
