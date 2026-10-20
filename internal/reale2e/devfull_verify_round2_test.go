package reale2e

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// Regressions of code review round 2 (design 12a-dev-flow-verify): a
// captured MCP record is decoded completely and strictly (nothing silently
// dropped), and every paid-call counter derives from admissions and
// settlements.

// editStreamRecord rewrites the first phase-1 stream record for which
// match holds: its decoded JSON object is passed to edit and re-encoded.
func editStreamRecord(t *testing.T, dir string, match func(line string) bool, edit func(rec map[string]any)) {
	t.Helper()
	p := filepath.Join(dir, "session", "phase-1.stdout.jsonl")
	lines := strings.SplitAfter(string(mustRead(t, p)), "\n")
	for i, l := range lines {
		if !match(l) {
			continue
		}
		var rec map[string]any
		if err := json.Unmarshal([]byte(l), &rec); err != nil {
			t.Fatal(err)
		}
		edit(rec)
		b, err := json.Marshal(rec)
		if err != nil {
			t.Fatal(err)
		}
		lines[i] = string(b) + "\n"
		if err := os.WriteFile(p, []byte(strings.Join(lines, "")), 0o600); err != nil {
			t.Fatal(err)
		}
		return
	}
	t.Fatal("no matching stream record")
}

// firstBlock is a stream record's first content block.
func firstBlock(rec map[string]any) map[string]any {
	return rec["message"].(map[string]any)["content"].([]any)[0].(map[string]any)
}

// isToolUse matches a tool use of name.
func isToolUse(name string) func(string) bool {
	return func(l string) bool {
		return strings.Contains(l, `"type":"tool_use"`) && strings.Contains(l, `"name":"`+name+`"`)
	}
}

// dispatchUseID is phase 1's first dispatch tool use ID.
func dispatchUseID(t *testing.T, dir string) string {
	t.Helper()
	for _, l := range strings.Split(string(mustRead(t, filepath.Join(dir, "session", "phase-1.stdout.jsonl"))), "\n") {
		if isToolUse(toolDispatch)(l) {
			var rec map[string]any
			json.Unmarshal([]byte(l), &rec)
			return firstBlock(rec)["id"].(string)
		}
	}
	t.Fatal("no dispatch")
	return ""
}

func TestDevFullReviewRound2(t *testing.T) {
	t.Parallel()
	for name, c := range map[string]struct {
		crit   string
		mutate func(t *testing.T, dir string)
	}{
		// A per-task override on the recorded dispatch.
		"dispatch_override": {"transitions", func(t *testing.T, dir string) {
			editStreamRecord(t, dir, isToolUse(toolDispatch), func(rec map[string]any) {
				firstBlock(rec)["input"].(map[string]any)["override"] = map[string]any{"model": "forged-model", "effort": "high", "timeout": "0s"}
			})
			remap(t, dir)
		}},
		// Any other field the MCP dispatch does not define.
		"dispatch_unknown_field": {"transitions", func(t *testing.T, dir string) {
			editStreamRecord(t, dir, isToolUse(toolDispatch), func(rec map[string]any) {
				firstBlock(rec)["input"].(map[string]any)["priority"] = "high"
			})
			remap(t, dir)
		}},
		// A dispatch with its own wait is not the asynchronous dispatch.
		"dispatch_with_wait": {"transitions", func(t *testing.T, dir string) {
			editStreamRecord(t, dir, isToolUse(toolDispatch), func(rec map[string]any) {
				firstBlock(rec)["input"].(map[string]any)["wait"] = "10s"
			})
			remap(t, dir)
		}},
		// The dispatch response reduced to a bare task_id with an extra
		// field: not the strict asynchronous dispatch response.
		"dispatch_response_loose": {"transitions", func(t *testing.T, dir string) {
			use := dispatchUseID(t, dir)
			editStreamRecord(t, dir, func(l string) bool { return strings.Contains(l, `"tool_use_id":"`+use+`"`) }, func(rec map[string]any) {
				b := firstBlock(rec)
				var out map[string]any
				json.Unmarshal([]byte(b["content"].(string)), &out)
				data, _ := json.Marshal(map[string]any{"task_id": out["task_id"], "forged": true})
				b["content"] = string(data)
			})
			remap(t, dir)
		}},
		// The consuming task_show with a field task_show does not define.
		"task_show_unknown_field": {"waits", func(t *testing.T, dir string) {
			editStreamRecord(t, dir, isToolUse(toolTaskShow), func(rec map[string]any) {
				firstBlock(rec)["input"].(map[string]any)["tail"] = 5
			})
			remap(t, dir)
		}},
		// The captured MCP configuration with an extra server field.
		"mcp_config_extra": {"provenance", func(t *testing.T, dir string) {
			editJSON(t, dir, "setup/mcp-config.json", func(m map[string]any) {
				for _, s := range m["mcpServers"].(map[string]any) {
					s.(map[string]any)["env"] = map[string]any{"CALLSHEET_EXTRA": "1"}
				}
			})
		}},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			dir := devPassingBundle(t)
			c.mutate(t, dir)
			code, rep := reforge(t, dir)
			if code == 0 || rep.Result == ResultPass || len(devCriterion(rep, c.crit).Codes) == 0 {
				t.Fatalf("a forged bundle checked %d %s: %v", code, rep.Result, devCodes(rep))
			}
		})
	}
	// The qualification's own recorded dispatches are reconciled the same
	// way: an override on a wake's dispatch does not qualify.
	t.Run("probe_dispatch_override", func(t *testing.T) {
		t.Parallel()
		dir := devProbeBundle(t)
		editStreamRecord(t, dir, isToolUse(toolDispatch), func(rec map[string]any) {
			firstBlock(rec)["input"].(map[string]any)["override"] = map[string]any{"model": "forged-model"}
		})
		requalify(t, dir)
		if codes := qualificationCodes(t, dir); !slices.Contains(codes, "dispatch_mismatch") {
			t.Fatalf("codes %v", codes)
		}
	})
	t.Run("probe_mcp_config_extra", func(t *testing.T) {
		t.Parallel()
		dir := devProbeBundle(t)
		editJSON(t, dir, "setup/mcp-config.json", func(m map[string]any) { m["extra"] = true })
		requalify(t, dir)
		if codes := qualificationCodes(t, dir); !slices.Contains(codes, "mcp_config_invalid") {
			t.Fatalf("codes %v", codes)
		}
	})
	// Paid calls of BLOCKED qualifications: the record counts what the
	// cost accounts, from admissions and settlements.
	agree := func(t *testing.T, r *devRig) {
		t.Helper()
		q := r.qualification()
		var cost DevCost
		if err := json.Unmarshal(r.evidence(devCostFile), &cost); err != nil {
			t.Fatal(err)
		}
		if q.Result != ResultBlocked || q.WorkerCalls != cost.WorkerCalls || cost.WorkerCalls != len(r.f.plane.ids()) {
			t.Fatalf("%s: qualification %d calls, cost %d, plane %d", q.Result, q.WorkerCalls, cost.WorkerCalls, len(r.f.plane.ids()))
		}
	}
	t.Run("probe_failed_wake", func(t *testing.T) {
		t.Parallel()
		r := newDevRig(t, []devAct{{}, {alter: "failed"}, {}, {marker: markerReviewApproved}})
		r.command = "probe-dev-full"
		r.owner = []string{"continue 1", "continue 2"}
		if code := r.run(); code != 1 {
			t.Fatalf("exit %d", code)
		}
		agree(t, r)
	})
	t.Run("probe_failed_reviewer_evidence", func(t *testing.T) {
		t.Parallel()
		r := newDevRig(t, []devAct{{}, {}, {}, {marker: markerReviewApproved, alter: "nested"}})
		r.command = "probe-dev-full"
		r.owner = []string{"continue 1", "continue 2"}
		r.expect(1, CodeReviewUnobserved)
		agree(t, r)
	})
	t.Run("probe_interrupted_call", func(t *testing.T) {
		t.Parallel()
		r := probeRig(t)
		ctx, cancel := context.WithCancel(context.Background())
		r.f.ctx = ctx
		r.onTick = func(r *devRig) {
			if r.d.slot != nil && r.d.slot.observed && r.d.slot.plan.Ordinal == 2 {
				cancel()
			}
		}
		r.expect(1, "interrupted")
		agree(t, r)
	})
}
