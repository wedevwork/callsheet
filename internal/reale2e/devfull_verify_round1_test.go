package reale2e

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// Regressions of code review round 1 (design 12a-dev-flow-verify): each
// reviewer probe's forged bundle, with its inventory, derived summaries
// and (where the forgery touches a stream) its event map recomputed, must
// not pass.

// remap recomputes a bundle's stored event map from its streams, as a
// capable forger would after editing them.
func remap(t *testing.T, dir string) {
	t.Helper()
	b, err := LoadDevBundle(dir)
	if err != nil {
		t.Fatal(err)
	}
	m := b.Manifest
	data, err := encodeJSON(deriveDevEventMap(sessionMapInput(b, m.RunID, m.GOOS, m.Render["REALE2E"], m.Render["RUNTIME"])))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, filepath.FromSlash(devEventMapFile)), data, 0o600); err != nil {
		t.Fatal(err)
	}
}

// moveDispatchAfterWaits moves phase 1's first MCP dispatch and its
// response after every other record of the phase but its result.
func moveDispatchAfterWaits(t *testing.T, dir string) {
	t.Helper()
	p := filepath.Join(dir, "session", "phase-1.stdout.jsonl")
	lines := strings.SplitAfter(string(mustRead(t, p)), "\n")
	if lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	use, res, id := -1, -1, ""
	for i, l := range lines {
		var rec struct {
			Message struct {
				Content []struct {
					Type      string `json:"type"`
					ID        string `json:"id"`
					Name      string `json:"name"`
					ToolUseID string `json:"tool_use_id"`
				} `json:"content"`
			} `json:"message"`
		}
		if json.Unmarshal([]byte(l), &rec) != nil {
			continue
		}
		for _, b := range rec.Message.Content {
			switch {
			case use < 0 && b.Type == "tool_use" && b.Name == toolDispatch:
				use, id = i, b.ID
			case use >= 0 && res < 0 && b.Type == "tool_result" && b.ToolUseID == id:
				res = i
			}
		}
	}
	if use < 0 || res != use+1 {
		t.Fatalf("no adjacent dispatch use and response in phase 1 (%d, %d)", use, res)
	}
	moved := []string{lines[use], lines[res]}
	rest := append(slices.Clone(lines[:use]), lines[res+1:]...)
	last := len(rest) - 1
	out := append(append(slices.Clone(rest[:last]), moved...), rest[last])
	if err := os.WriteFile(p, []byte(strings.Join(out, "")), 0o600); err != nil {
		t.Fatal(err)
	}
}

// editDispatchLine rewrites phase 1's stream lines given the index of its
// first MCP dispatch tool use.
func editDispatchLine(t *testing.T, dir string, edit func(lines []string, use int) []string) {
	t.Helper()
	p := filepath.Join(dir, "session", "phase-1.stdout.jsonl")
	lines := strings.SplitAfter(string(mustRead(t, p)), "\n")
	if lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	for i, l := range lines {
		if strings.Contains(l, `"type":"tool_use"`) && strings.Contains(l, `"name":"`+toolDispatch+`"`) {
			if err := os.WriteFile(p, []byte(strings.Join(edit(lines, i), "")), 0o600); err != nil {
				t.Fatal(err)
			}
			return
		}
	}
	t.Fatal("no dispatch in phase 1")
}

func TestDevFullReviewRound1(t *testing.T) {
	t.Parallel()
	for name, c := range map[string]struct {
		crit   string
		mutate func(t *testing.T, dir string)
	}{
		// C1: a failed build still claimed by the passing cases.
		"build_failed": {"final_tests", func(t *testing.T, dir string) {
			editJSON(t, dir, "validation/cases.json", func(m map[string]any) {
				m["build"].(map[string]any)["exit_code"] = 1
			})
		}},
		// C1: a reached validation output removed.
		"final_stdout_missing": {"final_tests", func(t *testing.T, dir string) {
			if err := os.Remove(filepath.Join(dir, "validation", "final-stdout.txt")); err != nil {
				t.Fatal(err)
			}
		}},
		// C2: the first dispatch recorded after the phase's waits.
		"dispatch_after_waits": {"transitions", func(t *testing.T, dir string) {
			moveDispatchAfterWaits(t, dir)
			remap(t, dir)
		}},
		// C2: a second dispatch of the same document later in the phase.
		"duplicate_dispatch": {"transitions", func(t *testing.T, dir string) {
			editDispatchLine(t, dir, func(lines []string, use int) []string {
				dup := strings.Replace(lines[use], `"id":"`, `"id":"dup`, 1)
				last := len(lines) - 1
				return append(append(slices.Clone(lines[:last]), dup), lines[last])
			})
			remap(t, dir)
		}},
		// C2: the recorded dispatch request differs from the document.
		"dispatch_request_differs": {"transitions", func(t *testing.T, dir string) {
			editDispatchLine(t, dir, func(lines []string, use int) []string {
				lines[use] = strings.Replace(lines[use], `"acceptance":"`, `"acceptance":"Also edit FEATURE.md. `, 1)
				return lines
			})
			remap(t, dir)
		}},
		// C2: the successor dispatched before the first task's wait was
		// consumed.
		"successor_before_consumption": {"transitions", func(t *testing.T, dir string) {
			editDispatchLine(t, dir, func(lines []string, use int) []string {
				next := -1
				for i := use + 1; i < len(lines); i++ {
					if strings.Contains(lines[i], `"type":"tool_use"`) && strings.Contains(lines[i], `"name":"`+toolDispatch+`"`) {
						next = i
						break
					}
				}
				if next < 0 || next+1 >= len(lines) {
					t.Fatal("no second dispatch in phase 1")
				}
				moved := []string{lines[next], lines[next+1]}
				rest := append(slices.Clone(lines[:next]), lines[next+2:]...)
				return append(append(slices.Clone(rest[:use+2]), moved...), rest[use+2:]...)
			})
			remap(t, dir)
		}},
		// C3: every sweep's captured request identities invented.
		"sweep_identities": {"admissions", func(t *testing.T, dir string) {
			editJSON(t, dir, devAdmissionsFile, func(m map[string]any) {
				for _, s := range m["sweeps"].([]any) {
					for _, x := range s.(map[string]any)["tasks"].([]any) {
						x := x.(map[string]any)
						x["role"], x["created_at"], x["requested_by"] = "coder", "2026-01-01T00:00:00Z", "mallory"
					}
				}
			})
		}},
		// W1: an envelope naming no run.
		"empty_run_id": {"manifest", func(t *testing.T, dir string) {
			editJSON(t, dir, "tasks/03/plan.json", func(m map[string]any) { m["run_id"] = "" })
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
	// C4: the reviewer probe's primary log removed; the stored verdict
	// alone is not evidence.
	t.Run("probe_reviewer_log_missing", func(t *testing.T) {
		t.Parallel()
		dir := devProbeBundle(t)
		if err := os.Remove(filepath.Join(dir, "probe", "reviewer", "01", "logs.txt")); err != nil {
			t.Fatal(err)
		}
		requalify(t, dir)
		if codes := qualificationCodes(t, dir); !slices.Contains(codes, "reviewer_evidence_unobserved") {
			t.Fatalf("codes %v", codes)
		}
	})
	// C5: the qualification's paid calls are all accounted.
	t.Run("probe_cost_accounts_every_call", func(t *testing.T) {
		t.Parallel()
		dir := devProbeBundle(t)
		q := bundleDoc[DevQualification](t, dir, devQualificationFile)
		cost := bundleDoc[DevCost](t, dir, devCostFile)
		if cost.WorkerCalls != q.WorkerCalls || len(cost.Tasks) != q.WorkerCalls || len(cost.WorkflowTasks) != 4 {
			t.Fatalf("qualification %d calls, cost %d calls over %d tasks", q.WorkerCalls, cost.WorkerCalls, len(cost.Tasks))
		}
		editJSON(t, dir, devCostFile, func(m map[string]any) { m["worker_calls"] = 5 })
		requalify(t, dir)
		if codes := qualificationCodes(t, dir); !slices.Contains(codes, "cost_mismatch") {
			t.Fatalf("codes %v", codes)
		}
	})
}
