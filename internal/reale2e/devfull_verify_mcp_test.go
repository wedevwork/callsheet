package reale2e

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/wedevwork/callsheet/internal/contract"
)

// The public-check layer of the MCP binding closure tests (design
// 12a-dev-flow-verify r0.6, owner decision "two test layers"): one
// positive control and one representative well-formed binding rejection
// per permitted call form through public check (MainWith), and one case
// per enumerated bundle-level scenario: R1 ordering, pairing, phase and
// orphan results, cancellation eligibility, lost-dispatch recovery,
// denial/vendor/error routing, conflicts, forbidden tools and the long
// poll, cross-namespace substitution and multi-page linkage. Every
// mutation recomputes the event map, inventory and derived reports so a
// stale hash is never the rejection.

// publicCheck recomputes a mutated bundle's derived files and judges it
// with the public check entry (MainWith), returning its exit and report.
func publicCheck(t *testing.T, dir string) (int, DevReport) {
	t.Helper()
	remap(t, dir)
	reforge(t, dir)
	var out, errb bytes.Buffer
	h := Host{Args: []string{"check", "--evidence", dir}, Stdout: &out, Stderr: &errb,
		NewWorld: func(Host) (*World, func(), error) { t.Error("check built a world"); return nil, nil, nil }}
	code := MainWith(h)
	var rep DevReport
	if err := json.Unmarshal(out.Bytes(), &rep); err != nil {
		t.Fatalf("check printed no report (%d): %v\n%s%s", code, err, out.String(), errb.String())
	}
	return code, rep
}

// hasCode reports a code anywhere in a report.
func hasCode(rep DevReport, code string) bool {
	for _, cs := range devCodes(rep) {
		if slices.Contains(cs, code) {
			return true
		}
	}
	return false
}

// phaseStream is phase p's stream, one record per element.
func phaseStream(t *testing.T, dir string, p int) []string {
	t.Helper()
	data := string(mustRead(t, filepath.Join(dir, filepath.FromSlash(devStreamFile(p)))))
	return strings.Split(strings.TrimSuffix(data, "\n"), "\n")
}

// writeStream replaces phase p's stream.
func writeStream(t *testing.T, dir string, p int, lines []string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, filepath.FromSlash(devStreamFile(p))), []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
}

// lineRecord decodes one stream line.
func lineRecord(l string) map[string]any {
	var rec map[string]any
	json.Unmarshal([]byte(l), &rec)
	return rec
}

// sessionOf is a stream's session ID.
func sessionOf(lines []string) string {
	for _, l := range lines {
		if s, _ := lineRecord(l)["session_id"].(string); s != "" {
			return s
		}
	}
	return ""
}

// useLine and resultLine are stream lines of one tool use and result.
func useLine(session, id, tool, input string) string {
	return mustMarshalAny(map[string]any{"type": "assistant", "session_id": session, "message": map[string]any{"role": "assistant",
		"content": []any{map[string]any{"type": "tool_use", "id": id, "name": tool, "input": json.RawMessage(input)}}}})
}

func resultLine(session, id, content string, isError bool) string {
	return mustMarshalAny(map[string]any{"type": "user", "session_id": session, "message": map[string]any{"role": "user",
		"content": []any{map[string]any{"type": "tool_result", "tool_use_id": id, "content": content, "is_error": isError}}}})
}

func mustMarshalAny(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return string(b)
}

// resultIndex is the index of phase lines' final result record (len when
// none).
func resultIndex(lines []string) int {
	for i := len(lines) - 1; i >= 0; i-- {
		if lineRecord(lines[i])["type"] == "result" {
			return i
		}
	}
	return len(lines)
}

// insertAt inserts records into phase p's stream at index at (negative:
// before its final result record).
func insertAt(t *testing.T, dir string, p, at int, records ...string) {
	t.Helper()
	lines := phaseStream(t, dir, p)
	if at < 0 {
		at = resultIndex(lines)
	}
	out := append(append(slices.Clone(lines[:at]), records...), lines[at:]...)
	writeStream(t, dir, p, out)
}

// callAt locates a recorded tool use (match on its line) and its result:
// phase, line indexes and tool-use ID.
type callAt struct {
	phase, use, result int
	id                 string
	args, answer       string
}

// findCalls lists every recorded use of tool whose input satisfies match.
func findCalls(t *testing.T, dir, tool string, match func(args string) bool) []callAt {
	t.Helper()
	var out []callAt
	for p := 1; p <= maxPhases; p++ {
		if _, err := os.Stat(filepath.Join(dir, filepath.FromSlash(devStreamFile(p)))); err != nil {
			continue
		}
		lines := phaseStream(t, dir, p)
		uses := map[string]int{}
		for i, l := range lines {
			rec := lineRecord(l)
			msg, _ := rec["message"].(map[string]any)
			content, _ := msg["content"].([]any)
			if len(content) == 0 {
				continue
			}
			b, _ := content[0].(map[string]any)
			switch {
			case b["type"] == "tool_use" && b["name"] == tool:
				args, _ := json.Marshal(b["input"])
				if match == nil || match(string(args)) {
					uses[b["id"].(string)] = len(out)
					out = append(out, callAt{phase: p, use: i, result: -1, id: b["id"].(string), args: string(args)})
				}
			case b["type"] == "tool_result":
				if k, ok := uses[b["tool_use_id"].(string)]; ok {
					out[k].result = i
					out[k].answer, _ = b["content"].(string)
				}
			}
		}
	}
	return out
}

// firstCall is the first recorded use of tool matching match.
func firstCall(t *testing.T, dir, tool string, match func(string) bool) callAt {
	t.Helper()
	calls := findCalls(t, dir, tool, match)
	if len(calls) == 0 {
		t.Fatalf("no recorded %s", tool)
	}
	return calls[0]
}

// setLine replaces one stream line.
func setLine(t *testing.T, dir string, p, i int, line string) {
	t.Helper()
	lines := phaseStream(t, dir, p)
	lines[i] = line
	writeStream(t, dir, p, lines)
}

// editAnswer rewrites a result line's answer through a JSON tree edit.
func editAnswer(t *testing.T, dir string, c callAt, edit func(m map[string]any)) {
	t.Helper()
	tree := jsonTree(c.answer).(map[string]any)
	edit(tree)
	lines := phaseStream(t, dir, c.phase)
	setLine(t, dir, c.phase, c.result, resultLine(sessionOf(lines), c.id, encodeTree(tree), false))
}

// hasKey matches arguments holding key.
func hasKey(key string) func(string) bool {
	return func(args string) bool { return strings.Contains(args, `"`+key+`"`) }
}

// noKey matches arguments without key.
func noKey(key string) func(string) bool {
	return func(args string) bool { return !strings.Contains(args, `"`+key+`"`) }
}

// capturedView is a bundle's captured view of task id.
func capturedView(t *testing.T, dir, id string) contract.TaskView {
	t.Helper()
	b, err := LoadDevBundle(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, v := range b.docs {
		if r, ok := v.(*contract.TaskShowResponse); ok && r.Task.TaskID == id {
			return r.Task
		}
	}
	t.Fatalf("no captured view of %s", id)
	return contract.TaskView{}
}

// devStopped is the cached stopped attempt with two coordinator phases
// (the code loop's cap reached in phase 2).
var devStopped struct {
	sync.Once
	dir string
	err error
}

// devStoppedBundle returns a fresh copy of the stopped two-phase attempt.
func devStoppedBundle(t *testing.T) string {
	t.Helper()
	devStopped.Do(func() {
		r := newDevRig(t, []devAct{{marker: markerDesignDraft}, {marker: markerDesignApproved}, {marker: markerImplComplete}, {marker: markerReviewChanges},
			{marker: markerFixesComplete}, {marker: markerReviewChanges}, {marker: markerFixesComplete}, {marker: markerReviewChanges}})
		r.owner = []string{"yes 1"}
		if code := r.run(); code != 1 || r.d.stopCode != stopCap {
			devStopped.err = fmt.Errorf("the stopped attempt ended %d %s", code, r.d.stopCode)
			return
		}
		devStopped.dir = filepath.Join(bundleCacheRoot, "dev-stopped")
		devStopped.err = copyTree(r.f.bundle(), devStopped.dir)
	})
	if devStopped.err != nil {
		t.Fatal(devStopped.err)
	}
	dst := filepath.Join(t.TempDir(), "stopped")
	if err := copyTree(devStopped.dir, dst); err != nil {
		t.Fatal(err)
	}
	return dst
}

// cancelOf is a valid cancellation call of a captured terminal task.
func cancelOf(t *testing.T, dir, session, id, useID string) []string {
	t.Helper()
	v := capturedView(t, dir, id)
	answer := encodeAnswer(t, contract.CancelResponse{Version: contract.ProtocolVersion, TaskID: id, Task: v})
	return []string{useLine(session, useID, toolTaskCancel, `{"id":"`+id+`"}`), resultLine(session, useID, answer, false)}
}

// lastDispatched is the last dispatch of a phase with its answered task.
func lastDispatched(t *testing.T, dir string, phase int) (callAt, string) {
	t.Helper()
	var last callAt
	var task string
	for _, c := range findCalls(t, dir, toolDispatch, nil) {
		if c.phase == phase && c.answer != "" {
			if v, err := contract.ParseDispatchResponse([]byte(c.answer)); err == nil {
				last, task = c, v.TaskID
			}
		}
	}
	if task == "" {
		t.Fatalf("no answered dispatch in phase %d", phase)
	}
	return last, task
}

// recoveredTask is the lost-reply attempt's recovered task.
func recoveredTask(t *testing.T, dir string) string {
	t.Helper()
	b, err := LoadDevBundle(dir)
	if err != nil {
		t.Fatal(err)
	}
	if obs := doc[DevObservation](b, taskDir(1)+"/observation.json"); obs != nil {
		return obs.Task
	}
	t.Fatal("no observed task")
	return ""
}

// testMCPWiring proves the checker calls the per-call judge: for each of
// the twelve permitted call forms, its positive control through public
// check (a completion PASS, or the stop-only fixture's precise stop with
// no MCP failure) and one well-formed binding rejection (exit 1 with
// mcp_call_invalid).
func testMCPWiring(t *testing.T) {
	type wiring struct {
		base   func(t *testing.T) string
		setup  func(t *testing.T, dir string)
		reject func(t *testing.T, dir string)
		stop   string
	}
	lostCancel := func(t *testing.T, dir string) {
		lines := phaseStream(t, dir, 1)
		insertAt(t, dir, 1, -1, cancelOf(t, dir, sessionOf(lines), recoveredTask(t, dir), "toolu_cancel")...)
	}
	changeTaskRole := func(m map[string]any) {
		v := obj(m, "task")
		obj(v, "role")["name"] = obj(v, "role")["name"].(string) + "x"
	}
	cases := map[string]wiring{
		"dispatch": {base: devPassingBundle, reject: func(t *testing.T, dir string) {
			editAnswer(t, dir, firstCall(t, dir, toolDispatch, nil), changeTaskRole)
		}},
		"task_ls": {base: devLostBundle, stop: "owner_no", reject: func(t *testing.T, dir string) {
			editAnswer(t, dir, firstCall(t, dir, toolTaskList, nil), func(m map[string]any) {
				e := m["tasks"].([]any)[0].(map[string]any)
				obj(e, "role")["name"] = obj(e, "role")["name"].(string) + "x"
			})
		}},
		"task_show": {base: devPassingBundle, reject: func(t *testing.T, dir string) {
			editAnswer(t, dir, firstCall(t, dir, toolTaskShow, nil), changeTaskRole)
		}},
		"task_logs": {base: devPassingBundle, reject: func(t *testing.T, dir string) {
			calls := findCalls(t, dir, toolTaskLogs, nil)
			editAnswer(t, dir, calls[0], func(m map[string]any) { m["task_id"] = jsonTree(calls[1].answer).(map[string]any)["task_id"] })
		}},
		"task_cancel": {base: devLostBundle, setup: lostCancel, stop: "owner_no", reject: func(t *testing.T, dir string) {
			editAnswer(t, dir, firstCall(t, dir, toolTaskCancel, nil), changeTaskRole)
		}},
		"ws_push": {base: devPassingBundle, reject: func(t *testing.T, dir string) {
			editAnswer(t, dir, firstCall(t, dir, toolWsPush, nil), func(m map[string]any) { m["branch"] = "refs/heads/other" })
		}},
		"ws_status/task": {base: devPassingBundle, reject: func(t *testing.T, dir string) {
			editAnswer(t, dir, firstCall(t, dir, toolWsStatus, hasKey("task_id")), func(m map[string]any) {
				w := obj(m, "workspace")
				obj(w, "binding")["base_selector"] = flipHex(obj(w, "binding")["base_selector"].(string))
				obj(w, "binding")["base_commit"] = obj(w, "binding")["base_selector"]
				obj(w, "result")["base_commit"] = obj(w, "binding")["base_selector"]
			})
		}},
		"ws_status/explicit": {base: devPassingBundle, reject: func(t *testing.T, dir string) {
			editAnswer(t, dir, firstCall(t, dir, toolWsStatus, hasKey("name")), func(m map[string]any) {
				r := m["refs"].([]any)[0].(map[string]any)
				r["commit"] = flipHex(r["commit"].(string))
			})
		}},
		"ws_diff/task": {base: devPassingBundle, reject: func(t *testing.T, dir string) {
			editAnswer(t, dir, firstCall(t, dir, toolWsDiff, hasKey("task_id")), func(m map[string]any) {
				c := m["changes"].([]any)[0].(map[string]any)
				c["new_bytes"] = c["new_bytes"].(float64) + 1
			})
		}},
		"ws_diff/explicit": {base: devPassingBundle, reject: func(t *testing.T, dir string) {
			editAnswer(t, dir, firstCall(t, dir, toolWsDiff, hasKey("name")), func(m map[string]any) {
				c := m["changes"].([]any)[0].(map[string]any)
				c["new_bytes"] = c["new_bytes"].(float64) + 1
			})
		}},
		"ws_pull/task": {base: devPassingBundle, reject: func(t *testing.T, dir string) {
			editAnswer(t, dir, firstCall(t, dir, toolWsPull, hasKey("task_id")), func(m map[string]any) { m["commit"] = flipHex(m["commit"].(string)) })
		}},
		"ws_pull/explicit": {base: devPassingBundle, reject: func(t *testing.T, dir string) {
			editAnswer(t, dir, firstCall(t, dir, toolWsPull, hasKey("ref")), func(m map[string]any) { m["commit"] = flipHex(m["commit"].(string)) })
		}},
	}
	if len(cases) != 12 {
		t.Fatalf("%d wiring forms", len(cases))
	}
	for form, c := range cases {
		t.Run(strings.ReplaceAll(form, "/", "_"), func(t *testing.T) {
			t.Parallel()
			control := func() string {
				dir := c.base(t)
				if c.setup != nil {
					c.setup(t, dir)
				}
				return dir
			}
			dir := control()
			code, rep := publicCheck(t, dir)
			switch {
			case c.stop == "" && (code != 0 || rep.Result != ResultPass):
				t.Fatalf("the %s control did not pass: %d %v", form, code, devCodes(rep))
			case c.stop != "" && (code != 1 || rep.Stop != c.stop || hasCode(rep, devMCPCallInvalid)):
				t.Fatalf("the %s control: %d stop %s %v", form, code, rep.Stop, devCodes(rep))
			}
			dir = control()
			c.reject(t, dir)
			if code, rep := publicCheck(t, dir); code != 1 || !slices.Contains(devCriterion(rep, "coordinator_setup").Codes, devMCPCallInvalid) {
				t.Fatalf("the %s binding rejection: %d %v", form, code, devCodes(rep))
			}
		})
	}
}

// r6Role is the round-6 role reproduction's registration.
var r6Role = `{"id":"worker-a","name":"implementer","node":"n_` + strings.Repeat("1", 32) + `","adapter":"fake","instruction":"/srv/manuals/instruction.md",` +
	`"runbook":"/srv/manuals/runbook.md","model":"example model","effort":"medium","concurrency":2,"timeout":"90m"}`

// r6RoleAnswer is the server's role answer to r6Role (its unchanged
// "example model").
func r6RoleAnswer(t *testing.T) string {
	t.Helper()
	rc, err := contract.ParseRoleAddArgs([]byte(r6Role), devAdapterLookup)
	if err != nil {
		t.Fatal(err)
	}
	spec, _ := devAdapterLookup(rc.Adapter)
	return encodeAnswer(t, contract.RoleResponse{Version: contract.ProtocolVersion, Role: contract.RoleView{RoleRecord: contract.RoleRecord{
		RoleConfig: rc.Resolved(), RegistrationOrder: 1}, NodeLiveness: contract.LivenessOffline, AdapterTestOnly: spec.TestOnly}})
}

// bundleScenario is one bundle-level scenario: its base, mutation and
// expected outcome ("pass", "stop" with the base's stop and no MCP
// failure, or a code that must appear with exit 1).
type bundleScenario struct {
	base   func(t *testing.T) string
	mutate func(t *testing.T, dir string)
	want   string
}

// testMCPScenarios runs every enumerated bundle-level scenario through
// public check.
func testMCPScenarios(t *testing.T) {
	show := func(t *testing.T, dir string) callAt { return firstCall(t, dir, toolTaskShow, nil) }
	insertShow := func(args, content string, isError bool) func(t *testing.T, dir string) {
		return func(t *testing.T, dir string) {
			lines := phaseStream(t, dir, 1)
			s := sessionOf(lines)
			insertAt(t, dir, 1, -1, useLine(s, "toolu_diag", toolTaskShow, args), resultLine(s, "toolu_diag", content, isError))
		}
	}
	taskOf := func(t *testing.T, dir string) string {
		return jsonTree(show(t, dir).args).(map[string]any)["id"].(string)
	}
	insertCall := func(tool, args, content string, isError, withResult bool) func(t *testing.T, dir string) {
		return func(t *testing.T, dir string) {
			lines := phaseStream(t, dir, 1)
			s := sessionOf(lines)
			recs := []string{useLine(s, "toolu_extra", tool, strings.ReplaceAll(args, "TASK", taskOf(t, dir)))}
			if withResult {
				recs = append(recs, resultLine(s, "toolu_extra", strings.ReplaceAll(content, "TASK", taskOf(t, dir)), isError))
			}
			insertAt(t, dir, 1, -1, recs...)
		}
	}
	denial := func(tool string, match func(string) bool) func(t *testing.T, dir string) {
		return func(t *testing.T, dir string) {
			c := firstCall(t, dir, tool, match)
			lines := phaseStream(t, dir, c.phase)
			i := resultIndex(lines)
			rec := lineRecord(lines[i])
			rec["permission_denials"] = []any{map[string]any{"tool_name": tool, "tool_use_id": c.id, "tool_input": map[string]any{}}}
			setLine(t, dir, c.phase, i, mustMarshalAny(rec))
		}
	}
	lostDispatch := func(t *testing.T, dir string) callAt { return firstCall(t, dir, toolDispatch, nil) }
	removeRecovery := func(t *testing.T, dir string) {
		for _, c := range findCalls(t, dir, toolTaskList, nil) {
			lines := phaseStream(t, dir, c.phase)
			lines[c.use], lines[c.result] = "", ""
			var kept []string
			for _, l := range lines {
				if l != "" {
					kept = append(kept, l)
				}
			}
			writeStream(t, dir, c.phase, kept)
		}
	}
	setLostResult := func(content string, isError bool) func(t *testing.T, dir string) {
		return func(t *testing.T, dir string) {
			c := lostDispatch(t, dir)
			lines := phaseStream(t, dir, c.phase)
			setLine(t, dir, c.phase, c.result, resultLine(sessionOf(lines), c.id, content, isError))
		}
	}
	dropLostResult := func(t *testing.T, dir string) {
		c := lostDispatch(t, dir)
		lines := phaseStream(t, dir, c.phase)
		writeStream(t, dir, c.phase, append(slices.Clone(lines[:c.result]), lines[c.result+1:]...))
	}
	cancelIn := func(base func(t *testing.T) string, phase int, at func(t *testing.T, dir string) int, target func(t *testing.T, dir string) string) func(t *testing.T, dir string) {
		return func(t *testing.T, dir string) {
			lines := phaseStream(t, dir, phase)
			insertAt(t, dir, phase, at(t, dir), cancelOf(t, dir, sessionOf(lines), target(t, dir), "toolu_cancel")...)
		}
	}
	beforeResult := func(t *testing.T, dir string) int { return -1 }
	scenarios := map[string]bundleScenario{
		// R1: dispatch after its wait, and a duplicate dispatch.
		"R1_dispatch_after_wait": {devPassingBundle, func(t *testing.T, dir string) { moveDispatchAfterWaits(t, dir) }, devDispatchOrder},
		"R1_duplicate_dispatch": {devPassingBundle, func(t *testing.T, dir string) {
			c := firstCall(t, dir, toolDispatch, nil)
			lines := phaseStream(t, dir, c.phase)
			s := sessionOf(lines)
			insertAt(t, dir, c.phase, -1, useLine(s, "toolu_dup", toolDispatch, c.args), resultLine(s, "toolu_dup", c.answer, false))
		}, devDispatchMismatch},
		// R6: the round-6 reproductions, each on the complete bundle.
		"R6_push_branch_only": {devPassingBundle, func(t *testing.T, dir string) {
			c := firstCall(t, dir, toolWsPush, nil)
			lines := phaseStream(t, dir, c.phase)
			setLine(t, dir, c.phase, c.use, useLine(sessionOf(lines), c.id, toolWsPush, withArgs(t, c.args, map[string]any{"branch": "other-branch"})))
		}, devMCPCallInvalid},
		"R6_task_diff_substitution": {devPassingBundle, func(t *testing.T, dir string) {
			// A single-page enumeration, so only the task binding can refuse
			// the substitution.
			var first callAt
			for _, c := range findCalls(t, dir, toolWsDiff, func(a string) bool { return strings.Contains(a, "task_id") && !strings.Contains(a, "after") }) {
				if jsonTree(c.answer).(map[string]any)["next_after"] == nil && first.id == "" {
					first = c
				}
			}
			if first.id == "" {
				t.Fatal("no single-page task diff")
			}
			var other string
			for _, c := range findCalls(t, dir, toolWsDiff, hasKey("task_id")) {
				if id := jsonTree(c.args).(map[string]any)["task_id"].(string); id != jsonTree(first.args).(map[string]any)["task_id"] && other == "" {
					other = id
				}
			}
			if other == "" {
				t.Fatal("no second inspected task")
			}
			lines := phaseStream(t, dir, first.phase)
			setLine(t, dir, first.phase, first.use, useLine(sessionOf(lines), first.id, toolWsDiff, withArgs(t, first.args, map[string]any{"task_id": other})))
		}, devMCPCallInvalid},
		"R6_role_add_model": {devPassingBundle, insertCall(mcpToolPrefix+"role_add", strings.Replace(r6Role, "example model", "other model", 1), r6RoleAnswer(t),
			false, true), devMCPCallInvalid},
		"R6_role_set_model": {devPassingBundle, insertCall(mcpToolPrefix+"role_set", `{"id":"worker-a","model":"other model"}`, r6RoleAnswer(t), false, true),
			devMCPCallInvalid},
		// Source and phase ordering.
		"order_push_after_dispatch": {devPassingBundle, func(t *testing.T, dir string) {
			c := firstCall(t, dir, toolWsPush, nil)
			lines := phaseStream(t, dir, 1)
			use, res := lines[c.use], lines[c.result]
			var kept []string
			for i, l := range lines {
				if i != c.use && i != c.result {
					kept = append(kept, l)
				}
			}
			writeStream(t, dir, 1, kept)
			insertAt(t, dir, 1, -1, use, res)
		}, devMCPCallInvalid},
		"order_continuation_before_page": {devPassingBundle, func(t *testing.T, dir string) {
			first := firstCall(t, dir, toolWsDiff, func(a string) bool { return strings.Contains(a, "task_id") && !strings.Contains(a, "after") })
			lines := phaseStream(t, dir, first.phase)
			use, res := lines[first.use], lines[first.result]
			var kept []string
			for i, l := range lines {
				if i != first.use && i != first.result {
					kept = append(kept, l)
				}
			}
			writeStream(t, dir, first.phase, kept)
			insertAt(t, dir, first.phase, -1, use, res)
		}, devMCPCallInvalid},
		// Pairing, duplicates, orphans and phases.
		"pairing_reused_use_id": {devPassingBundle, func(t *testing.T, dir string) {
			c := firstCall(t, dir, toolTaskLogs, nil)
			lines := phaseStream(t, dir, c.phase)
			s := sessionOf(lines)
			insertAt(t, dir, c.phase, -1, useLine(s, c.id, toolTaskLogs, c.args), resultLine(s, c.id, c.answer, false))
		}, devMCPCallInvalid},
		"pairing_orphaned_result": {devPassingBundle, func(t *testing.T, dir string) {
			c := firstCall(t, dir, toolTaskLogs, nil)
			insertAt(t, dir, c.phase, -1, resultLine(sessionOf(phaseStream(t, dir, c.phase)), "toolu_orphan", c.answer, false))
		}, devMCPCallInvalid},
		"pairing_result_in_another_phase": {devPassingBundle, func(t *testing.T, dir string) {
			c := firstCall(t, dir, toolTaskLogs, func(string) bool { return true })
			lines := phaseStream(t, dir, c.phase)
			res := lines[c.result]
			writeStream(t, dir, c.phase, append(slices.Clone(lines[:c.result]), lines[c.result+1:]...))
			insertAt(t, dir, c.phase+1, -1, res)
		}, devMCPCallInvalid},
		"pairing_identical_duplicate_result": {devPassingBundle, func(t *testing.T, dir string) {
			c := firstCall(t, dir, toolTaskLogs, nil)
			lines := phaseStream(t, dir, c.phase)
			insertAt(t, dir, c.phase, c.result+1, lines[c.result])
		}, devMCPCallInvalid},
		// Missing authority for a successful answer.
		"authority_missing": {devPassingBundle, func(t *testing.T, dir string) {
			c := show(t, dir)
			other := "t_" + strings.Repeat("f", 32)
			tree := jsonTree(c.answer).(map[string]any)
			obj(tree, "task")["task_id"] = other
			insertShow(`{"id":"`+other+`"}`, encodeTree(tree), false)(t, dir)
		}, devMCPCallInvalid},
		// Cancellation eligibility.
		"cancel_control_lost": {devLostBundle, cancelIn(devLostBundle, 1, beforeResult, recoveredTask), "stop"},
		"cancel_control_stopped_final_phase": {devStoppedBundle, cancelIn(devStoppedBundle, 2, beforeResult, func(t *testing.T, dir string) string {
			_, task := lastDispatched(t, dir, 2)
			return task
		}), "stop"},
		"cancel_without_stop": {devPassingBundle, cancelIn(devPassingBundle, 1, beforeResult, func(t *testing.T, dir string) string {
			_, task := lastDispatched(t, dir, 1)
			return task
		}), devMCPCallInvalid},
		"cancel_mismatched_stop": {devLostBundle, func(t *testing.T, dir string) {
			cancelIn(devLostBundle, 1, beforeResult, recoveredTask)(t, dir)
			editJSON(t, dir, devManifestFile, func(m map[string]any) {
				n, _ := strconv.Atoi(fmt.Sprint(m["stop_seq"]))
				m["stop_seq"] = json.Number(strconv.Itoa(n + 1))
			})
		}, devMCPCallInvalid},
		"cancel_nonfinal_phase": {devStoppedBundle, cancelIn(devStoppedBundle, 1, beforeResult, func(t *testing.T, dir string) string {
			_, task := lastDispatched(t, dir, 1)
			return task
		}), devMCPCallInvalid},
		"cancel_unowned_target": {devLostBundle, func(t *testing.T, dir string) {
			b, _ := LoadDevBundle(dir)
			pre := doc[contract.TaskShowResponse](b, "preflight/01/task.json")
			cancelIn(devLostBundle, 1, beforeResult, func(*testing.T, string) string { return pre.Task.TaskID })(t, dir)
		}, devMCPCallInvalid},
		"cancel_before_dispatch_reply": {devStoppedBundle, func(t *testing.T, dir string) {
			// The dispatch use precedes the cancellation, its reply follows
			// it: no dispatch use follows the cancellation.
			c, task := lastDispatched(t, dir, 2)
			lines := phaseStream(t, dir, 2)
			reply := lines[c.result]
			writeStream(t, dir, 2, append(slices.Clone(lines[:c.result]), lines[c.result+1:]...))
			insertAt(t, dir, 2, -1, append(cancelOf(t, dir, sessionOf(lines), task, "toolu_cancel"), reply)...)
		}, devMCPCallInvalid},
		"cancel_then_dispatch": {devStoppedBundle, cancelIn(devStoppedBundle, 2, func(t *testing.T, dir string) int {
			c, _ := lastDispatched(t, dir, 2)
			return c.use
		}, func(t *testing.T, dir string) string {
			calls := findCalls(t, dir, toolDispatch, nil)
			var ids []string
			for _, c := range calls {
				if v, err := contract.ParseDispatchResponse([]byte(c.answer)); err == nil && c.phase == 2 {
					ids = append(ids, v.TaskID)
				}
			}
			return ids[0]
		}), devMCPCallInvalid},
		// Lost-dispatch recovery and its pages.
		"recovery_control":                       {devLostBundle, func(*testing.T, string) {}, "stop"},
		"recovery_invalid_argument_error":        {devLostBundle, setLostResult(errorBody(contract.CodeInvalidArgument), true), "stop"},
		"recovery_absent_reply":                  {devLostBundle, dropLostResult, "stop"},
		"recovery_error_without_recovery":        {devLostBundle, removeRecovery, devDispatchUnobserved},
		"recovery_absent_reply_without_recovery": {devLostBundle, func(t *testing.T, dir string) { dropLostResult(t, dir); removeRecovery(t, dir) }, devDispatchUnobserved},
		"recovery_page_foreign_entry": {devLostBundle, func(t *testing.T, dir string) {
			editAnswer(t, dir, firstCall(t, dir, toolTaskList, nil), func(m map[string]any) {
				tasks := m["tasks"].([]any)
				last := jsonTree(encodeTree(tasks[len(tasks)-1])).(map[string]any)
				last["task_id"] = "t_" + strings.Repeat("f", 32)
				m["tasks"] = append(tasks, last)
				if m["next_after"] != nil {
					m["next_after"] = last["task_id"]
				}
			})
		}, devMCPCallInvalid},
		"recovery_page_cursor": {devLostBundle, func(t *testing.T, dir string) {
			calls := findCalls(t, dir, toolTaskList, hasKey("after"))
			if len(calls) == 0 {
				t.Fatal("no continued recovery page")
			}
			after := jsonTree(calls[0].args).(map[string]any)["after"].(string)
			editAnswer(t, dir, calls[0], func(m map[string]any) {
				tasks := m["tasks"].([]any)
				e := jsonTree(encodeTree(tasks[0])).(map[string]any)
				e["task_id"] = after
				m["tasks"] = append([]any{e}, tasks...)
			})
		}, devMCPCallInvalid},
		// Denials, vendor errors and tolerated Callsheet errors.
		"denial_dispatch":        {devPassingBundle, denial(toolDispatch, nil), devToolDenied},
		"denial_diagnostic_call": {devPassingBundle, denial(toolTaskLogs, nil), devToolDenied},
		"vendor_tool_error":      {devPassingBundle, insertCall("Bash", `{"command":"false"}`, "exit 1", true, true), devToolFailed},
		"opaque_error":           {devPassingBundle, insertCall(toolTaskShow, `{"id":"TASK"}`, "MCP error -32000: Connection closed", true, true), "pass"},
		"not_found_absent_task":  {devPassingBundle, insertShow(`{"id":"t_`+strings.Repeat("e", 32)+`"}`, errorBody(contract.CodeNotFound), true), "pass"},
		"invalid_args_error_answer": {devPassingBundle, insertCall(toolTaskShow, `{"id":"TASK","lines":-1}`, errorBody(contract.CodeInvalidArgument), true, true),
			devMCPCallInvalid},
		// Conflicts, never recovered.
		"conflict_success_flag_error_body": {devPassingBundle, func(t *testing.T, dir string) {
			c := firstCall(t, dir, toolTaskLogs, nil)
			setLine(t, dir, c.phase, c.result, resultLine(sessionOf(phaseStream(t, dir, c.phase)), c.id, errorBody(contract.CodeNotFound), false))
		}, devMCPCallInvalid},
		"conflict_error_flag_success_other_identity": {devPassingBundle, func(t *testing.T, dir string) {
			c := show(t, dir)
			tree := jsonTree(c.answer).(map[string]any)
			changeRole := obj(obj(tree, "task"), "role")
			changeRole["name"] = changeRole["name"].(string) + "x"
			insertShow(c.args, encodeTree(tree), true)(t, dir)
		}, devMCPCallInvalid},
		"conflict_dispatch_error_flag_success": {devLostBundle, func(t *testing.T, dir string) {
			b, _ := LoadDevBundle(dir)
			v := doc[contract.TaskShowResponse](b, taskDir(1)+"/task.json").Task
			setLostResult(encodeAnswer(t, contract.DispatchResponse{Version: contract.ProtocolVersion, TaskID: v.TaskID, Task: &v}), true)(t, dir)
		}, devMCPCallInvalid},
		// Forbidden tools and the long poll.
		"forbidden_role_set":            {devPassingBundle, insertCall(mcpToolPrefix+"role_set", `{"id":"designer","model":"other model"}`, `{}`, false, true), devMCPCallInvalid},
		"forbidden_node_ls_error":       {devPassingBundle, insertCall(mcpToolPrefix+"node_ls", `{}`, errorBody(contract.CodeUnavailable), true, true), devMCPCallInvalid},
		"forbidden_ws_create_no_result": {devPassingBundle, insertCall(mcpToolPrefix+"ws_create", `{"name":"other"}`, "", false, false), devMCPCallInvalid},
		"forbidden_unknown_tool":        {devPassingBundle, insertCall(mcpToolPrefix+"task_peek", `{}`, `{}`, false, true), devMCPCallInvalid},
		"long_poll_omitted_wait":        {devPassingBundle, insertCall(toolTaskWait, `{"task_ids":["TASK"]}`, `{}`, false, true), CodeLongPoll},
		"long_poll_explicit_wait":       {devPassingBundle, insertCall(toolTaskWait, `{"task_ids":["TASK"],"wait":"5s"}`, `{}`, false, true), CodeLongPoll},
		"long_poll_error_reply":         {devPassingBundle, insertCall(toolTaskWait, `{"task_ids":["TASK"]}`, errorBody(contract.CodeUnavailable), true, true), CodeLongPoll},
		"long_poll_absent_reply":        {devPassingBundle, insertCall(toolTaskWait, `{"task_ids":["TASK"]}`, "", false, false), CodeLongPoll},
		// Cross-namespace substitution: a preflight task has no workspace.
		"cross_namespace_preflight_diff": {devPassingBundle, func(t *testing.T, dir string) {
			b, _ := LoadDevBundle(dir)
			pre := doc[contract.TaskShowResponse](b, "preflight/01/task.json").Task.TaskID
			c := firstCall(t, dir, toolWsDiff, hasKey("task_id"))
			lines := phaseStream(t, dir, c.phase)
			s := sessionOf(lines)
			insertAt(t, dir, c.phase, -1, useLine(s, "toolu_xns", toolWsDiff, `{"task_id":"`+pre+`","limit":1}`), resultLine(s, "toolu_xns", c.answer, false))
		}, devMCPCallInvalid},
		// Multi-page and interleaved enumerations.
		"pages_interleaved_control": {devPassingBundle, func(*testing.T, string) {}, "pass"},
		"pages_continuation_without_page": {devPassingBundle, func(t *testing.T, dir string) {
			first := firstCall(t, dir, toolWsDiff, func(a string) bool { return strings.Contains(a, "task_id") && !strings.Contains(a, "after") })
			lines := phaseStream(t, dir, first.phase)
			writeStream(t, dir, first.phase, append(slices.Clone(lines[:first.use]), lines[first.result+1:]...))
		}, devMCPCallInvalid},
		// Result positions: state a result establishes counts only for a
		// tool use recorded after that result.
		"delayed_first_page_reply": {devPassingBundle, func(t *testing.T, dir string) {
			// The task-form first page's answer recorded after its
			// continuation's request and answer.
			first := firstCall(t, dir, toolWsDiff, func(a string) bool { return strings.Contains(a, "task_id") && !strings.Contains(a, "after") })
			task := jsonTree(first.args).(map[string]any)["task_id"].(string)
			var cont callAt
			for _, c := range findCalls(t, dir, toolWsDiff, hasKey("after")) {
				if strings.Contains(c.args, task) && cont.id == "" {
					cont = c
				}
			}
			moveLineAfter(t, dir, first.phase, first.result, cont.result)
		}, devMCPCallInvalid},
		"status_continuation_control": {devPassingBundle, func(t *testing.T, dir string) { statusPages(t, dir, false) }, "pass"},
		"delayed_status_page_reply":   {devPassingBundle, func(t *testing.T, dir string) { statusPages(t, dir, true) }, devMCPCallInvalid},
		"delayed_seed_push_reply": {devPassingBundle, func(t *testing.T, dir string) {
			// The seed push's answer recorded after the first dispatch.
			push := firstCall(t, dir, toolWsPush, nil)
			d := firstCall(t, dir, toolDispatch, nil)
			moveLineAfter(t, dir, push.phase, push.result, d.use)
		}, devMCPCallInvalid},
		"delayed_lost_reply": {devLostBundle, func(t *testing.T, dir string) {
			// The dispatch's error answer recorded after every recovery
			// listing's request: no listing recovers it.
			d := firstCall(t, dir, toolDispatch, nil)
			ls := findCalls(t, dir, toolTaskList, nil)
			moveLineAfter(t, dir, d.phase, d.result, ls[len(ls)-1].use)
		}, devDispatchUnobserved},
		"overlapping_recovery_listings": {devLostBundle, func(t *testing.T, dir string) {
			// Two more recovery listings of the lost reply's task: A
			// requested first but answered after the cancellation, B
			// requested later and answered before it. The cancellation is
			// eligible from B's answer.
			task := recoveredTask(t, dir)
			var page callAt
			for _, c := range findCalls(t, dir, toolTaskList, nil) {
				if strings.Contains(c.answer, task) && page.id == "" {
					page = c
				}
			}
			lines := phaseStream(t, dir, page.phase)
			s := sessionOf(lines)
			cancel := cancelOf(t, dir, s, task, "toolu_cancel")
			recs := []string{useLine(s, "toolu_list_a", toolTaskList, page.args), useLine(s, "toolu_list_b", toolTaskList, page.args),
				resultLine(s, "toolu_list_b", page.answer, false)}
			// They replace the recorded recovery listings, in place: the
			// only recovery is theirs. The cancellation follows the
			// phase's last dispatch, and A's answer follows it.
			drop := map[int]bool{}
			for _, c := range findCalls(t, dir, toolTaskList, nil) {
				drop[c.use], drop[c.result] = true, true
			}
			var out []string
			for i, l := range lines {
				if i == page.use {
					out = append(out, recs...)
				}
				if !drop[i] {
					out = append(out, l)
				}
			}
			writeStream(t, dir, page.phase, out)
			insertAt(t, dir, page.phase, -1, cancel[0], cancel[1], resultLine(s, "toolu_list_a", page.answer, false))
		}, "stop"},
		"delayed_dispatch_reply": {devPassingBundle, func(t *testing.T, dir string) {
			// The dispatch's answer recorded after its background wait
			// started (enforced before this round too).
			d := firstCall(t, dir, toolDispatch, nil)
			lines := phaseStream(t, dir, d.phase)
			start := -1
			for i := d.result; i < len(lines) && start < 0; i++ {
				if strings.Contains(lines[i], "dev-wait-task") && strings.Contains(lines[i], `"tool_use"`) {
					start = i
				}
			}
			moveLineAfter(t, dir, d.phase, d.result, start)
		}, devDispatchOrder},
		"pages_generation_change": {devPassingBundle, func(t *testing.T, dir string) {
			c := firstCall(t, dir, toolWsDiff, hasKey("after"))
			lines := phaseStream(t, dir, c.phase)
			s := sessionOf(lines)
			gen := strings.Repeat("2", 32)
			lines[c.use] = useLine(s, c.id, toolWsDiff, strings.ReplaceAll(c.args, inspectGeneration, gen))
			lines[c.result] = resultLine(s, c.id, strings.ReplaceAll(c.answer, inspectGeneration, gen), false)
			writeStream(t, dir, c.phase, lines)
		}, devMCPCallInvalid},
	}
	for _, code := range []contract.Code{contract.CodeInvalidArgument, contract.CodeNotFound, contract.CodeConflict, contract.CodeUnavailable,
		contract.CodeTrustFailed, contract.CodeProtocolMismatch, contract.CodeNotImplemented, contract.CodeInternal} {
		scenarios["contract_error_"+string(code)] = bundleScenario{devPassingBundle, insertCall(toolTaskShow, `{"id":"TASK"}`, errorBody(code), true, true), "pass"}
	}
	for name, sc := range scenarios {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			base := sc.base(t)
			baseCode, baseRep := publicCheck(t, base)
			if hasCode(baseRep, devMCPCallInvalid) || (baseCode != 0) != (baseRep.Stop != "") {
				t.Fatalf("the shared control: %d stop %q %v", baseCode, baseRep.Stop, devCodes(baseRep))
			}
			dir := sc.base(t)
			sc.mutate(t, dir)
			code, rep := publicCheck(t, dir)
			switch sc.want {
			case "pass":
				if code != 0 || rep.Result != ResultPass {
					t.Fatalf("%d %v", code, devCodes(rep))
				}
			case "stop":
				if code != 1 || rep.Stop != baseRep.Stop || hasCode(rep, devMCPCallInvalid) || !bytes.Equal(mustJSON(devCodes(rep)), mustJSON(devCodes(baseRep))) {
					t.Fatalf("%d stop %q %v (control %q %v)", code, rep.Stop, devCodes(rep), baseRep.Stop, devCodes(baseRep))
				}
			default:
				if code != 1 || !hasCode(rep, sc.want) {
					t.Fatalf("%d %v, want %s", code, devCodes(rep), sc.want)
				}
			}
		})
	}
}

// moveLineAfter moves phase p's line from to just after line after.
func moveLineAfter(t *testing.T, dir string, p, from, after int) {
	t.Helper()
	lines := phaseStream(t, dir, p)
	if from < 0 || after < 0 || from >= len(lines) || after >= len(lines) || after <= from {
		t.Fatalf("cannot move line %d after line %d", from, after)
	}
	moved := lines[from]
	out := append(slices.Clone(lines[:from]), lines[from+1:after+1]...)
	out = append(append(out, moved), lines[after+1:]...)
	writeStream(t, dir, p, out)
}

// statusPages turns the explicit ws_status read into a two-page
// enumeration (limit 1: the first ref, then main), with the first page's
// answer optionally recorded after the continuation's.
func statusPages(t *testing.T, dir string, delayed bool) {
	t.Helper()
	c := firstCall(t, dir, toolWsStatus, hasKey("name"))
	var r contract.WorkspaceStatusResponse
	if err := json.Unmarshal([]byte(c.answer), &r); err != nil || len(r.Refs) != 2 {
		t.Fatalf("explicit status %v %d", err, len(r.Refs))
	}
	lines := phaseStream(t, dir, c.phase)
	s := sessionOf(lines)
	first, second := r, r
	first.Refs, second.Refs = r.Refs[:1], r.Refs[1:]
	next := r.Refs[0].Name
	first.NextAfter = &next
	args := withArgs(t, c.args, map[string]any{"limit": 1})
	contArgs := withArgs(t, c.args, map[string]any{"limit": 1, "after": next, "instance": r.Instance, "generation": r.Generation})
	lines[c.use] = useLine(s, c.id, toolWsStatus, args)
	lines[c.result] = resultLine(s, c.id, encodeAnswer(t, first), false)
	cont := []string{useLine(s, "toolu_status2", toolWsStatus, contArgs), resultLine(s, "toolu_status2", encodeAnswer(t, second), false)}
	out := append(append(slices.Clone(lines[:c.result+1]), cont...), lines[c.result+1:]...)
	writeStream(t, dir, c.phase, out)
	if delayed {
		moveLineAfter(t, dir, c.phase, c.result, c.result+2)
	}
}

// testMCPPositiveAttempts checks the complete positive attempts: with
// optional workspace inspection (both forms, interleaved pages), with a
// valid partial enumeration, and with no coordinator inspection at all.
func testMCPPositiveAttempts(t *testing.T) {
	for _, mode := range []string{inspectFull, inspectPartial, inspectNone} {
		t.Run("inspection_"+cmpOr(mode, "none"), func(t *testing.T) {
			t.Parallel()
			dir := devPassingBundleOf(t, mode)
			calls := len(findCalls(t, dir, toolWsStatus, nil)) + len(findCalls(t, dir, toolWsDiff, nil)) + len(findCalls(t, dir, toolWsPull, nil))
			switch {
			case mode == inspectNone && calls != 0, mode != inspectNone && calls == 0:
				t.Fatalf("%d inspection calls in mode %q", calls, mode)
			}
			if code, rep := publicCheck(t, dir); code != 0 || rep.Result != ResultPass {
				t.Fatalf("%d %v", code, devCodes(rep))
			}
		})
	}
}
