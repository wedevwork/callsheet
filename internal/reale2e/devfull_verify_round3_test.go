package reale2e

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"
)

// Regressions of code review round 3 (design 12a-dev-flow-verify): the
// recorded MCP dispatch is decoded by the server's own argument rule
// (nulls refused) and the recorded response's immutable task identity
// equals the one the prepared document determines and the authoritative
// plane record holds, as whole objects.

// editDispatchResponse rewrites phase 1's first dispatch response task
// view through edit (its decoded JSON object).
func editDispatchResponse(t *testing.T, dir string, edit func(resp map[string]any)) {
	t.Helper()
	use := dispatchUseID(t, dir)
	editStreamRecord(t, dir, func(l string) bool { return strings.Contains(l, `"tool_use_id":"`+use+`"`) }, func(rec map[string]any) {
		b := firstBlock(rec)
		var resp map[string]any
		if err := json.Unmarshal([]byte(b["content"].(string)), &resp); err != nil {
			t.Fatal(err)
		}
		edit(resp)
		data, err := json.Marshal(resp)
		if err != nil {
			t.Fatal(err)
		}
		b["content"] = string(data)
	})
}

// setPath sets a dotted path of nested objects in m.
func setPath(m map[string]any, path string, v any) {
	keys := strings.Split(path, ".")
	for _, k := range keys[:len(keys)-1] {
		m = m[k].(map[string]any)
	}
	m[keys[len(keys)-1]] = v
}

func TestDevFullReviewRound3(t *testing.T) {
	t.Parallel()
	check := func(t *testing.T, mutate func(t *testing.T, dir string)) {
		t.Helper()
		dir := devPassingBundle(t)
		mutate(t, dir)
		remap(t, dir)
		code, rep := reforge(t, dir)
		if code == 0 || rep.Result == ResultPass || len(devCriterion(rep, "transitions").Codes) == 0 {
			t.Fatalf("a forged bundle checked %d %s: %v", code, rep.Result, devCodes(rep))
		}
	}
	// (a) A null argument: the MCP server refuses it, so no such call was
	// the admitted dispatch.
	t.Run("dispatch_null_wait", func(t *testing.T) {
		t.Parallel()
		check(t, func(t *testing.T, dir string) {
			editStreamRecord(t, dir, isToolUse(toolDispatch), func(rec map[string]any) {
				firstBlock(rec)["input"].(map[string]any)["wait"] = nil
			})
		})
	})
	// (b) The response's workspace request and binding both forged.
	t.Run("response_workspace_forged", func(t *testing.T) {
		t.Parallel()
		check(t, func(t *testing.T, dir string) {
			editDispatchResponse(t, dir, func(resp map[string]any) {
				setPath(resp, "task.request.workspace", "forged-workspace")
				setPath(resp, "task.workspace_binding.name", "forged-workspace")
			})
		})
	})
	// Every immutable field of the response's task view, in turn.
	for path, v := range map[string]any{
		"task_id":                              hexID("t_", 99),
		"task.task_id":                         hexID("t_", 99),
		"task.request.target.kind":             "role",
		"task.request.target.value":            "coder",
		"task.request.goal":                    "another goal",
		"task.request.acceptance":              "another acceptance",
		"task.request.payload":                 []any{"extra"},
		"task.request.override":                map[string]any{"model": "forged-model"},
		"task.request.workspace":               "forged-workspace",
		"task.request.base":                    strings.Repeat("0", 40),
		"task.request.workspace_instance":      strings.Repeat("f", 32),
		"task.request.requested_by.name":       "mallory",
		"task.request.requested_by.version":    "9.9.9",
		"task.request.requested_by.hostname":   "elsewhere",
		"task.role.id":                         "coder",
		"task.role.adapter":                    "claude",
		"task.role.node":                       "n_" + strings.Repeat("9", 32),
		"task.role.registration_order":         7,
		"task.effective.model":                 "forged-model",
		"task.effective.effort":                "high",
		"task.effective.timeout":               "1m0s",
		"task.timeout_policy":                  "advisory",
		"task.created_at":                      "2026-01-01T00:00:00Z",
		"task.workspace_binding.name":          "forged-workspace",
		"task.workspace_binding.instance":      strings.Repeat("f", 32),
		"task.workspace_binding.base_selector": strings.Repeat("0", 40),
		"task.workspace_binding.base_commit":   strings.Repeat("0", 40),
	} {
		t.Run("response_"+strings.ReplaceAll(path, ".", "_"), func(t *testing.T) {
			t.Parallel()
			check(t, func(t *testing.T, dir string) {
				editDispatchResponse(t, dir, func(resp map[string]any) { setPath(resp, path, v) })
			})
		})
	}
	// The lost-reply recovery's task_ls entry is the authoritative
	// task's: a listed entry with another identity does not recover it.
	t.Run("task_ls_entry_forged", func(t *testing.T) {
		t.Parallel()
		// The control: a lost reply recovered through valid task_ls pages,
		// the workflow task's complete summary on the last one, passes its
		// transitions.
		dir := devLostBundle(t)
		if code, rep := reforge(t, dir); code != 1 || rep.Stop != "owner_no" || len(devCriterion(rep, "transitions").Codes) != 0 {
			t.Fatalf("the unmutated recovery %d %v", code, devCodes(rep))
		}
		// The forgery: every listed entry's attribution changed.
		pages := editStreamRecords(t, dir, resultsOf(t, dir, toolTaskList), func(rec map[string]any) {
			b := firstBlock(rec)
			var page map[string]any
			json.Unmarshal([]byte(b["content"].(string)), &page)
			for _, x := range page["tasks"].([]any) {
				x.(map[string]any)["requested_by"].(map[string]any)["name"] = "mallory"
			}
			data, _ := json.Marshal(page)
			b["content"] = string(data)
		})
		if pages == 0 {
			t.Fatal("no recovery page")
		}
		remap(t, dir)
		code, rep := reforge(t, dir)
		if code == 0 || !slices.Contains(devCriterion(rep, "transitions").Codes, devDispatchMismatch) {
			t.Fatalf("a forged recovery checked %d: %v", code, devCodes(rep))
		}
	})
}
