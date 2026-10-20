package reale2e

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/wedevwork/callsheet/internal/contract"
)

// Regressions of code review round 5 (design 12a-dev-flow-verify): every
// Callsheet MCP call a coordinator stream records is judged in both
// directions by its own tool's server rules, with no early acceptance: a
// dispatch carrying a wait is refused outright whatever else it holds, a
// transfer's answer is parsed as the server's answer, and the valid
// inspection calls of the coordinator playbook (task_logs, ws_status, the
// paged ws_diff enumeration, ws_pull) are part of a passing attempt.

// recordedCall is one recorded tool use with its answer.
type recordedCall struct {
	id     string
	args   json.RawMessage
	answer string
}

// recordedCalls returns every recorded use of tool in a bundle's phase
// streams with its answer, in stream order.
func recordedCalls(t *testing.T, dir, tool string) []recordedCall {
	t.Helper()
	var calls []recordedCall
	index := map[string]int{}
	for p := 1; p <= maxPhases; p++ {
		data, err := os.ReadFile(filepath.Join(dir, filepath.FromSlash(devStreamFile(p))))
		if err != nil {
			continue
		}
		for _, l := range strings.Split(string(data), "\n") {
			var rec map[string]any
			if json.Unmarshal([]byte(l), &rec) != nil {
				continue
			}
			msg, _ := rec["message"].(map[string]any)
			if content, _ := msg["content"].([]any); len(content) == 0 {
				continue
			}
			b := firstBlock(rec)
			switch {
			case b["type"] == "tool_use" && b["name"] == tool:
				args, _ := json.Marshal(b["input"])
				index[b["id"].(string)] = len(calls)
				calls = append(calls, recordedCall{id: b["id"].(string), args: args})
			case b["type"] == "tool_result":
				if i, ok := index[b["tool_use_id"].(string)]; ok {
					calls[i].answer, _ = b["content"].(string)
				}
			}
		}
	}
	return calls
}

// setContent replaces a tool result's answer text.
func setContent(text string) func(rec map[string]any) {
	return func(rec map[string]any) { firstBlock(rec)["content"] = text }
}

// passes requires a verdict without any problem: PASS for a passing
// bundle, or the lost-reply attempt's owner_no stop.
func passes(t *testing.T, code int, rep DevReport, lost bool) {
	t.Helper()
	if lost && (code != 1 || rep.Stop != "owner_no" || len(devCodes(rep)) != 0) || !lost && (code != 0 || rep.Result != ResultPass) {
		t.Fatalf("a valid attempt was refused: %d %s %v", code, rep.Result, devCodes(rep))
	}
}

func TestDevFullReviewRound5(t *testing.T) {
	t.Parallel()
	// C1: the playbook's valid inspection calls are recorded by the
	// passing attempt and accepted, the diff enumerated page by page with
	// the server's continuation.
	t.Run("inspection_control", func(t *testing.T) {
		t.Parallel()
		dir := devPassingBundle(t)
		for _, tool := range []string{toolTaskLogs, toolWsStatus, toolWsPull} {
			if len(recordedCalls(t, dir, tool)) == 0 {
				t.Fatalf("the passing attempt records no %s call", tool)
			}
		}
		diffs := recordedCalls(t, dir, toolWsDiff)
		continued := 0
		for _, c := range diffs {
			if strings.Contains(string(c.args), `"after"`) {
				continued++
			}
		}
		if len(diffs) < 2 || continued == 0 {
			t.Fatalf("the diff is not enumerated in pages: %d calls, %d continued", len(diffs), continued)
		}
		code, rep := reforge(t, dir)
		passes(t, code, rep, false)
	})
	// Equivalent valid argument forms of the inspection calls stay
	// accepted.
	for name, c := range map[string]struct {
		tool   string
		fields map[string]any
	}{
		"task_logs_late_false": {toolTaskLogs, map[string]any{"late": false}},
		"ws_pull_no_path":      {toolWsPull, map[string]any{"path": nil}},
	} {
		t.Run(name+"_control", func(t *testing.T) {
			t.Parallel()
			code, rep := verdictAfter(t, devPassingBundle(t), isToolUse(c.tool), setInput(c.fields))
			passes(t, code, rep, false)
		})
	}
	// C2: a dispatch with a wait is refused whatever else it holds; the
	// reproduced pair added a target the server refuses. Each is recorded
	// with the asynchronous answer and with the dispatch-with-wait answer
	// the server gives to a wait (its task_id and a still_running union).
	for name, fields := range map[string]map[string]any{
		"dispatch_wait_bogus_kind": {"wait": "30s", "target": map[string]any{"kind": "bogus", "value": "designer"}},
		"dispatch_wait_valid":      {"wait": "30s"},
		"dispatch_wait_zero":       {"wait": "0s"},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			code, rep := verdictAfter(t, devPassingBundle(t), isToolUse(toolDispatch), setInput(fields))
			rejected(t, code, rep)
		})
		t.Run(name+"_wait_answer", func(t *testing.T) {
			t.Parallel()
			dir := devPassingBundle(t)
			if editStreamRecords(t, dir, isToolUse(toolDispatch), setInput(fields)) == 0 {
				t.Fatal("no dispatch recorded")
			}
			code, rep := verdictAfter(t, dir, resultsOf(t, dir, toolDispatch), func(rec map[string]any) {
				b := firstBlock(rec)
				var v struct {
					TaskID string `json:"task_id"`
				}
				json.Unmarshal([]byte(b["content"].(string)), &v)
				pv := contract.ProtocolVersion
				answer := fmt.Sprintf(`{"version":%d,"task_id":%q,"wait_result":{"version":%d,"status":"still_running","effective_wait_ms":0,"tasks":[`+
					`{"task_id":%q,"state":"running","elapsed_ms":0,"last_log_line":"","log_truncated":false,"durability_confirmed":false}]}}`, pv, v.TaskID, pv, v.TaskID)
				if _, _, err := contract.ParseDispatchWaitResponse([]byte(answer), 0); err != nil {
					t.Fatalf("not the server's dispatch-with-wait answer: %v", err)
				}
				b["content"] = answer
			})
			rejected(t, code, rep)
		})
	}
	// C2: a transfer's answer is the server's answer, never anything that
	// merely decodes.
	for name, c := range map[string]struct{ tool, answer string }{
		"ws_push_answer_empty":   {toolWsPush, `{}`},
		"ws_push_answer_partial": {toolWsPush, `{"commit":"` + strings.Repeat("c", 40) + `"}`},
		"ws_pull_answer_empty":   {toolWsPull, `{}`},
		"task_logs_answer_empty": {toolTaskLogs, `{}`},
		"ws_status_answer_empty": {toolWsStatus, `{}`},
		"ws_diff_answer_empty":   {toolWsDiff, `{}`},
		"task_show_answer_empty": {toolTaskShow, `{}`},
		"dispatch_answer_empty":  {toolDispatch, `{}`},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			dir := devPassingBundle(t)
			code, rep := verdictAfter(t, dir, resultsOf(t, dir, c.tool), setContent(c.answer))
			rejected(t, code, rep)
		})
	}
	// The inspection calls' server-refused arguments (internal/mcp
	// tools_test and transfer_test), each judged by the server's own
	// validator.
	for name, c := range map[string]struct {
		tool string
		raw  bool
		edit map[string]any
		in   string
	}{
		"task_logs_bad_id":         {tool: toolTaskLogs, raw: true, in: `{"id":"T"}`},
		"task_logs_lines":          {tool: toolTaskLogs, edit: map[string]any{"lines": 5}},
		"task_logs_late_null":      {tool: toolTaskLogs, edit: map[string]any{"late": json.RawMessage("null")}},
		"ws_status_task_and_name":  {tool: toolWsStatus, edit: map[string]any{"name": "proj"}},
		"ws_status_task_paged":     {tool: toolWsStatus, edit: map[string]any{"limit": 5}},
		"ws_status_bad_task":       {tool: toolWsStatus, edit: map[string]any{"task_id": "t_x"}},
		"ws_diff_limit_0":          {tool: toolWsDiff, edit: map[string]any{"limit": 0}},
		"ws_diff_limit_101":        {tool: toolWsDiff, edit: map[string]any{"limit": 101}},
		"ws_diff_after_alone":      {tool: toolWsDiff, edit: map[string]any{"after": "YQ=="}},
		"ws_diff_task_and_base":    {tool: toolWsDiff, edit: map[string]any{"base": "main"}},
		"ws_diff_after_not_base64": {tool: toolWsDiff, edit: map[string]any{"after": "!!", "instance": strings.Repeat("a", 32), "generation": strings.Repeat("b", 32)}},
		"ws_pull_task_and_name":    {tool: toolWsPull, edit: map[string]any{"name": "proj"}},
		"ws_pull_path_empty":       {tool: toolWsPull, edit: map[string]any{"path": ""}},
		"ws_pull_neither_form":     {tool: toolWsPull, edit: map[string]any{"task_id": nil}},
		"ws_pull_unknown":          {tool: toolWsPull, edit: map[string]any{"force": true}},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			edit := setInput(c.edit)
			if c.raw {
				edit = rawInput(c.in)
			}
			code, rep := verdictAfter(t, devPassingBundle(t), isToolUse(c.tool), edit)
			rejected(t, code, rep)
		})
	}
	// A recorded answer reconciled with its request: another task's
	// workspace status or logs, and a diff page that leaves the
	// continuation's generation.
	t.Run("ws_status_other_task", func(t *testing.T) {
		t.Parallel()
		code, rep := verdictAfter(t, devPassingBundle(t), isToolUse(toolWsStatus), setInput(map[string]any{"task_id": "t_" + strings.Repeat("f", 32)}))
		rejected(t, code, rep)
	})
	t.Run("task_logs_other_task", func(t *testing.T) {
		t.Parallel()
		code, rep := verdictAfter(t, devPassingBundle(t), isToolUse(toolTaskLogs), setInput(map[string]any{"id": "t_" + strings.Repeat("f", 32)}))
		rejected(t, code, rep)
	})
	t.Run("ws_diff_other_generation", func(t *testing.T) {
		t.Parallel()
		dir := devPassingBundle(t)
		code, rep := verdictAfter(t, dir, func(l string) bool {
			return isToolUse(toolWsDiff)(l) && strings.Contains(l, `"generation"`)
		}, setInput(map[string]any{"generation": strings.Repeat("e", 32)}))
		rejected(t, code, rep)
	})
	// A tool the server does not register is never part of an attempt,
	// and a registered tool's answer must be that tool's answer.
	t.Run("not_a_server_tool", func(t *testing.T) {
		t.Parallel()
		code, rep := verdictAfter(t, devPassingBundle(t), consumingShow, func(rec map[string]any) {
			firstBlock(rec)["name"] = mcpToolPrefix + "task_peek"
		})
		rejected(t, code, rep)
	})
	t.Run("another_tools_answer", func(t *testing.T) {
		t.Parallel()
		// A task_show answer recorded as task_logs: the arguments are valid
		// task_logs arguments; the answer is not a logs answer.
		code, rep := verdictAfter(t, devPassingBundle(t), func(l string) bool {
			return consumingShow(l) && !strings.Contains(l, `"lines"`)
		}, func(rec map[string]any) {
			firstBlock(rec)["name"] = toolTaskLogs
		})
		rejected(t, code, rep)
	})
}
