package reale2e

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// Regressions of code review round 4 (design 12a-dev-flow-verify): every
// recorded Callsheet MCP call is judged by its tool's own server validator
// (internal/contract's per-tool parsers, which the MCP handlers call), and
// a recovery page is reconciled with its requested cursor and limit.

// editStreamRecords rewrites every phase stream record for which match
// holds: its decoded JSON object is passed to edit and re-encoded. It
// returns how many it rewrote.
func editStreamRecords(t *testing.T, dir string, match func(line string) bool, edit func(rec map[string]any)) int {
	t.Helper()
	n := 0
	for p := 1; p <= maxPhases; p++ {
		path := filepath.Join(dir, filepath.FromSlash(devStreamFile(p)))
		data, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		lines := strings.SplitAfter(string(data), "\n")
		for i, l := range lines {
			if !match(l) {
				continue
			}
			var rec map[string]any
			if err := json.Unmarshal([]byte(l), &rec); err != nil {
				t.Fatal(err)
			}
			edit(rec)
			b, _ := json.Marshal(rec)
			lines[i] = string(b) + "\n"
			n++
		}
		if err := os.WriteFile(path, []byte(strings.Join(lines, "")), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return n
}

// setInput replaces a tool use's input fields (nil deletes one).
func setInput(fields map[string]any) func(rec map[string]any) {
	return func(rec map[string]any) {
		in := firstBlock(rec)["input"].(map[string]any)
		for k, v := range fields {
			if v == nil {
				delete(in, k)
			} else {
				in[k] = v
			}
		}
	}
}

// rawInput replaces a tool use's whole input with raw JSON.
func rawInput(raw string) func(rec map[string]any) {
	return func(rec map[string]any) {
		var in any
		if err := json.Unmarshal([]byte(raw), &in); err != nil {
			panic(err)
		}
		firstBlock(rec)["input"] = in
	}
}

// devLost is the cached lost-reply attempt: its first dispatch's answer
// was lost and recovered through task_ls pages.
var devLost struct {
	sync.Once
	dir string
	err error
}

// devLostBundle returns a fresh copy of the lost-reply attempt's bundle.
func devLostBundle(t *testing.T) string {
	t.Helper()
	devLost.Do(func() {
		r := newDevRig(t, ownerNoSteps())
		r.fault = "lost"
		r.owner = []string{"no 1"}
		if code := r.run(); code != 1 || r.d.stopCode != "owner_no" {
			devLost.err = fmt.Errorf("the lost-reply attempt stopped %d %s", code, r.d.stopCode)
			return
		}
		devLost.dir = filepath.Join(bundleCacheRoot, "dev-lost")
		devLost.err = copyTree(r.f.bundle(), devLost.dir)
	})
	if devLost.err != nil {
		t.Fatal(devLost.err)
	}
	dst := filepath.Join(t.TempDir(), "lost")
	if err := copyTree(devLost.dir, dst); err != nil {
		t.Fatal(err)
	}
	return dst
}

// verdictAfter mutates a bundle's streams, recomputes the event map and
// every derived file, and returns the check's verdict.
func verdictAfter(t *testing.T, dir string, match func(string) bool, edit func(map[string]any)) (int, DevReport) {
	t.Helper()
	if editStreamRecords(t, dir, match, edit) == 0 {
		t.Fatal("no record mutated")
	}
	remap(t, dir)
	return reforge(t, dir)
}

// rejected requires a non-PASS verdict whose coordinator setup names the
// invalid MCP call.
func rejected(t *testing.T, code int, rep DevReport) {
	t.Helper()
	if code == 0 || rep.Result == ResultPass || !slicesContains(devCriterion(rep, "coordinator_setup").Codes, devMCPCallInvalid) {
		t.Fatalf("checked %d %s: %v", code, rep.Result, devCodes(rep))
	}
}

func slicesContains(s []string, v string) bool {
	for _, x := range s {
		if x == v {
			return true
		}
	}
	return false
}

// resultsOf matches the tool results of every recorded use of tool.
func resultsOf(t *testing.T, dir, tool string) func(string) bool {
	t.Helper()
	ids := map[string]bool{}
	for p := 1; p <= maxPhases; p++ {
		data, _ := os.ReadFile(filepath.Join(dir, filepath.FromSlash(devStreamFile(p))))
		for _, l := range strings.Split(string(data), "\n") {
			if isToolUse(tool)(l) {
				var rec map[string]any
				json.Unmarshal([]byte(l), &rec)
				ids[firstBlock(rec)["id"].(string)] = true
			}
		}
	}
	return func(l string) bool {
		var rec map[string]any
		if !strings.Contains(l, `"type":"tool_result"`) || json.Unmarshal([]byte(l), &rec) != nil {
			return false
		}
		id, _ := firstBlock(rec)["tool_use_id"].(string)
		return ids[id]
	}
}

// consumingShow matches every task_show tool use.
var consumingShow = isToolUse(toolTaskShow)

// recoveryList matches every task_ls tool use.
var recoveryList = isToolUse(toolTaskList)

func TestDevFullReviewRound4(t *testing.T) {
	t.Parallel()
	// task_show: the reproduced out-of-range tails, the server's own
	// rejected inputs (internal/mcp tools_test), and in-range controls.
	for name, edit := range map[string]func(map[string]any){
		"lines_negative":   setInput(map[string]any{"lines": -1}),
		"lines_over_200":   setInput(map[string]any{"lines": 201}),
		"server_bad_id":    rawInput(`{"id":"T"}`),
		"server_no_id":     rawInput(`{}`),
		"server_null":      setInput(map[string]any{"lines": json.RawMessage("null")}),
		"server_unknown":   setInput(map[string]any{"tail": 5}),
		"server_lines_str": setInput(map[string]any{"lines": "5"}),
	} {
		t.Run("task_show_"+name, func(t *testing.T) {
			t.Parallel()
			code, rep := verdictAfter(t, devPassingBundle(t), consumingShow, edit)
			rejected(t, code, rep)
		})
	}
	for _, lines := range []int{0, 200} {
		t.Run(fmt.Sprintf("task_show_lines_%d_control", lines), func(t *testing.T) {
			t.Parallel()
			code, rep := verdictAfter(t, devPassingBundle(t), consumingShow, setInput(map[string]any{"lines": lines}))
			if code != 0 || rep.Result != ResultPass {
				t.Fatalf("a valid read was refused: %d %v", code, devCodes(rep))
			}
		})
	}
	// task_ls of the lost-reply recovery: the reproduced limits, cursor
	// and page-against-cursor contradictions, the server's own rejected
	// inputs, and valid controls.
	t.Run("recovery_control", func(t *testing.T) {
		t.Parallel()
		code, rep := reforge(t, devLostBundle(t))
		if code != 1 || rep.Stop != "owner_no" || len(devCodes(rep)) != 0 {
			t.Fatalf("the unmutated recovery %d %s %v", code, rep.Stop, devCodes(rep))
		}
	})
	for name, edit := range map[string]func(map[string]any){
		"limit_0":           setInput(map[string]any{"limit": 0}),
		"limit_101":         setInput(map[string]any{"limit": 101}),
		"after_invalid":     setInput(map[string]any{"after": "t_x"}),
		"limit_below_page":  setInput(map[string]any{"limit": 1}),
		"server_after_null": setInput(map[string]any{"after": json.RawMessage("null")}),
		"server_cursor":     setInput(map[string]any{"cursor": "x"}),
	} {
		t.Run("task_ls_"+name, func(t *testing.T) {
			t.Parallel()
			code, rep := verdictAfter(t, devLostBundle(t), recoveryList, edit)
			rejected(t, code, rep)
		})
	}
	t.Run("task_ls_page_holds_its_cursor", func(t *testing.T) {
		t.Parallel()
		// Each later page answers an exclusive after cursor; the forged
		// answer lists the cursor's own task too.
		dir := devLostBundle(t)
		cursors := map[string]string{}
		for p := 1; p <= maxPhases; p++ {
			data, _ := os.ReadFile(filepath.Join(dir, filepath.FromSlash(devStreamFile(p))))
			for _, l := range strings.Split(string(data), "\n") {
				if recoveryList(l) {
					var rec map[string]any
					json.Unmarshal([]byte(l), &rec)
					b := firstBlock(rec)
					if after, ok := b["input"].(map[string]any)["after"].(string); ok {
						cursors[b["id"].(string)] = after
					}
				}
			}
		}
		if len(cursors) == 0 {
			t.Fatal("the recovery read no later page")
		}
		code, rep := verdictAfter(t, dir, func(l string) bool {
			for id := range cursors {
				if strings.Contains(l, `"tool_use_id":"`+id+`"`) {
					return true
				}
			}
			return false
		}, func(rec map[string]any) {
			b := firstBlock(rec)
			var page map[string]any
			json.Unmarshal([]byte(b["content"].(string)), &page)
			tasks := page["tasks"].([]any)
			cursor := cursors[b["tool_use_id"].(string)]
			// The cursor's own task, from the previous page's last entry.
			first := map[string]any{}
			for k, v := range tasks[0].(map[string]any) {
				first[k] = v
			}
			first["task_id"] = cursor
			page["tasks"] = append([]any{first}, tasks...)
			data, _ := json.Marshal(page)
			b["content"] = string(data)
		})
		rejected(t, code, rep)
	})
	for _, limit := range []int{2, 100} {
		t.Run(fmt.Sprintf("task_ls_limit_%d_control", limit), func(t *testing.T) {
			t.Parallel()
			code, rep := verdictAfter(t, devLostBundle(t), recoveryList, setInput(map[string]any{"limit": limit}))
			if code != 1 || rep.Stop != "owner_no" || len(devCodes(rep)) != 0 {
				t.Fatalf("a valid recovery was refused: %d %v", code, devCodes(rep))
			}
		})
	}
	// ws_push of the seed: the server's own rejected inputs
	// (internal/mcp transfer_test).
	for name, fields := range map[string]map[string]any{
		"no_instance":  {"instance": nil},
		"branch_upper": {"branch": "Main"},
		"branch_empty": {"branch": ""},
		"branch_lock":  {"branch": "x.lock"},
		"path_empty":   {"path": ""},
		"path_nul":     {"path": "a\x00b"},
		"path_long":    {"path": strings.Repeat("a", 4096)},
		"force":        {"force": true},
		"commit":       {"commit": true},
	} {
		t.Run("ws_push_"+name, func(t *testing.T) {
			t.Parallel()
			code, rep := verdictAfter(t, devPassingBundle(t), isToolUse(toolWsPush), setInput(fields))
			rejected(t, code, rep)
		})
	}
	// dispatch: values the server's dispatch validator refuses.
	for name, fields := range map[string]map[string]any{
		"wait_invalid":     {"wait": "soon"},
		"override_timeout": {"override": map[string]any{"timeout": "forever"}},
		"target_kind":      {"target": map[string]any{"kind": "bogus", "value": "designer"}},
		"payload_null":     {"payload": json.RawMessage("null")},
	} {
		t.Run("dispatch_"+name, func(t *testing.T) {
			t.Parallel()
			code, rep := verdictAfter(t, devPassingBundle(t), isToolUse(toolDispatch), setInput(fields))
			rejected(t, code, rep)
		})
	}
	// A task_show answer recorded under another server tool's name is not
	// that tool's answer (round 5 judges every registered tool by its own
	// rules; TestDevFullReviewRound5 covers names the server never
	// registers).
	t.Run("unexpected_tool", func(t *testing.T) {
		t.Parallel()
		code, rep := verdictAfter(t, devPassingBundle(t), consumingShow, func(rec map[string]any) {
			firstBlock(rec)["name"] = mcpToolPrefix + "task_logs"
		})
		rejected(t, code, rep)
	})
}
