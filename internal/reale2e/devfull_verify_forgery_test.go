package reale2e

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The forgery matrix (design 12a-dev-flow-verify, Testing plan): one
// shared immutable passing bundle, cloned per case, one authority boundary
// mutated at a time with the inventory and derived summaries recomputed.
// Every variant is 1 or 2, never PASS; the positive controls pass; the
// check never changes its input.

// rewriteFile replaces old with new in one bundle file (it must occur).
func rewriteFile(t *testing.T, dir, rel, old, new string) {
	t.Helper()
	p := filepath.Join(dir, filepath.FromSlash(rel))
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(b, []byte(old)) {
		t.Fatalf("%s does not hold %q", rel, old)
	}
	if err := os.WriteFile(p, bytes.Replace(b, []byte(old), []byte(new), 1), 0o600); err != nil {
		t.Fatal(err)
	}
}

// bundleDoc decodes one bundle JSON file.
func bundleDoc[T any](t testing.TB, dir, rel string) T {
	t.Helper()
	var v T
	if err := json.Unmarshal(mustRead(t, filepath.Join(dir, filepath.FromSlash(rel))), &v); err != nil {
		t.Fatal(err)
	}
	return v
}

// taskOf is a stored workflow task's ID and commit.
func taskOf(t *testing.T, dir string, n int) (string, string) {
	t.Helper()
	v := bundleDoc[devEnvelope[DevTaskRecordView]](t, dir, taskDir(n)+"/record.json")
	return v.Data.TaskID, v.Data.Commit
}

// DevTaskRecordView is the identity part of a stored task record.
type DevTaskRecordView struct {
	TaskID string `json:"task_id"`
	Commit string `json:"commit"`
}

// forgeries are the matrix's mutations of a passing bundle; each names the
// criterion that must fail.
func forgeries() map[string]struct {
	crit   string
	mutate func(t *testing.T, dir string)
} {
	return map[string]struct {
		crit   string
		mutate func(t *testing.T, dir string)
	}{
		"counterfeit_receipt": {"owner_receipts", func(t *testing.T, dir string) {
			editJSON(t, dir, "receipts/01.json", func(m map[string]any) {
				m["data"].(map[string]any)["design_sha256"] = sha256Hex([]byte("another design"))
			})
		}},
		"buffered_receipt": {"owner_receipts", func(t *testing.T, dir string) {
			// The yes predates the prompt that asked for it.
			editJSON(t, dir, "receipts/01.json", func(m map[string]any) {
				d := m["data"].(map[string]any)
				d["decision_seq"] = d["consumed_seq"]
			})
		}},
		"omitted_early_task": {"transitions", func(t *testing.T, dir string) {
			os.RemoveAll(filepath.Join(dir, "tasks", "01"))
			os.Remove(filepath.Join(dir, filepath.FromSlash(devRepoDir), "refs", "heads", "task-01"))
		}},
		"reused_id": {"admissions", func(t *testing.T, dir string) {
			first, _ := taskOf(t, dir, 1)
			second, _ := taskOf(t, dir, 2)
			replaceAll(t, dir, second, first)
		}},
		"altered_base": {"transitions", func(t *testing.T, dir string) {
			_, base := taskOf(t, dir, 2)
			seed := bundleDoc[DevManifest](t, dir, devManifestFile).Workspace.SeedCommit
			editJSON(t, dir, "tasks/03/dispatch.json", func(m map[string]any) {
				b, _ := json.Marshal(m)
				if !strings.Contains(string(b), base) {
					t.Fatalf("the dispatch does not name base %s", base)
				}
				var out map[string]any
				json.Unmarshal([]byte(strings.ReplaceAll(string(b), base, seed)), &out)
				for k := range m {
					delete(m, k)
				}
				for k, v := range out {
					m[k] = v
				}
			})
		}},
		"reordered_wake": {"waits", func(t *testing.T, dir string) {
			// The first wake moved after its own consuming task_show.
			p := filepath.Join(dir, "session", "phase-1.stdout.jsonl")
			lines := strings.SplitAfter(string(mustRead(t, p)), "\n")
			i := 0
			for i < len(lines) && !strings.Contains(lines[i], `task-notification`) {
				i++
			}
			if i+2 >= len(lines) {
				t.Fatal("no wake in phase 1")
			}
			lines[i], lines[i+1], lines[i+2] = lines[i+1], lines[i+2], lines[i]
			os.WriteFile(p, []byte(strings.Join(lines, "")), 0o600)
		}},
		"swapped_reviewer_log": {"reviewer_execution", func(t *testing.T, dir string) {
			a, b := mustRead(t, filepath.Join(dir, "tasks", "04", "logs.txt")), mustRead(t, filepath.Join(dir, "tasks", "06", "logs.txt"))
			os.WriteFile(filepath.Join(dir, "tasks", "04", "logs.txt"), b, 0o600)
			os.WriteFile(filepath.Join(dir, "tasks", "06", "logs.txt"), a, 0o600)
			for _, n := range []string{"04", "06"} {
				data := mustRead(t, filepath.Join(dir, "tasks", n, "logs.txt"))
				editJSON(t, dir, "tasks/"+n+"/logs.json", func(m map[string]any) {
					meta := m["meta"].(map[string]any)
					meta["raw_sha256"] = sha256Hex(data)
					for _, k := range []string{"source_bytes", "retained_bytes", "data_bytes", "stored_bytes"} {
						meta[k] = len(data)
					}
				})
			}
		}},
		"prose_receipt": {"reviewer_execution", func(t *testing.T, dir string) {
			// The helper's receipt survives only as text the reviewer wrote.
			p := filepath.Join(dir, "tasks", "06", "logs.txt")
			log := mustRead(t, p)
			data := []byte(codexRecord(map[string]any{"type": "item.completed", "item": map[string]any{"id": "item_0", "type": "agent_message",
				"text": "I ran the helper; its receipt:\n" + string(log)}}))
			os.WriteFile(p, data, 0o600)
			editJSON(t, dir, "tasks/06/logs.json", func(m map[string]any) {
				meta := m["meta"].(map[string]any)
				meta["raw_sha256"] = sha256Hex(data)
				for _, k := range []string{"source_bytes", "retained_bytes", "data_bytes", "stored_bytes"} {
					meta[k] = len(data)
				}
			})
		}},
		"wrong_candidate_exec": {"final_tests", func(t *testing.T, dir string) {
			_, cand := taskOf(t, dir, 5)
			_, early := taskOf(t, dir, 3)
			for _, rel := range []string{"validation/final-test.json", "validation/cases.json", "validation/requirements.json", devCoverageFile} {
				p := filepath.Join(dir, filepath.FromSlash(rel))
				os.WriteFile(p, bytes.ReplaceAll(mustRead(t, p), []byte(cand), []byte(early)), 0o600)
			}
		}},
		"patched_case": {"feature_oracle", func(t *testing.T, dir string) {
			editJSON(t, dir, "validation/cases.json", func(m map[string]any) {
				c := m["cases"].([]any)[0].(map[string]any)
				out := "Hello, wrong!\n"
				c["stdout"] = out
				e := c["exec"].(map[string]any)
				e["stdout_sha256"], e["stdout_bytes"] = sha256Hex([]byte(out)), len(out)
			})
		}},
		"removed_test_event": {"feature_oracle", func(t *testing.T, dir string) {
			p := filepath.Join(dir, "validation", "tests.jsonl")
			var kept []string
			for _, l := range strings.SplitAfter(string(mustRead(t, p)), "\n") {
				if strings.Contains(l, `"TestRun/help"`) && strings.Contains(l, `"Action":"pass"`) {
					continue
				}
				kept = append(kept, l)
			}
			os.WriteFile(p, []byte(strings.Join(kept, "")), 0o600)
		}},
		"fake_clean_diff": {"owner_environment", func(t *testing.T, dir string) {
			// A protected owner file changed in the after shard while the
			// stored diff (its shard digest updated) still claims nothing.
			p := filepath.Join(dir, "environment", "after-00.jsonl")
			var out []string
			for _, l := range strings.SplitAfter(string(mustRead(t, p)), "\n") {
				if strings.Contains(l, `"p":"CLAUDE.md"`) {
					var rec map[string]any
					json.Unmarshal([]byte(l), &rec)
					rec["h"] = sha256Hex([]byte("changed"))
					b, _ := json.Marshal(rec)
					l = string(b) + "\n"
				}
				out = append(out, l)
			}
			data := []byte(strings.Join(out, ""))
			os.WriteFile(p, data, 0o600)
			editJSON(t, dir, "environment/diff.json", func(m map[string]any) {
				m["after"].([]any)[0].(map[string]any)["sha256"] = sha256Hex(data)
			})
		}},
		"incomplete_cleanup": {"cleanup", func(t *testing.T, dir string) {
			editJSON(t, dir, devCleanupFile, func(m map[string]any) {
				p := m["data"].(map[string]any)["processes"].([]any)[0].(map[string]any)
				p["exit_verified"] = false
			})
		}},
		"removed_process": {"cleanup", func(t *testing.T, dir string) {
			editJSON(t, dir, devCleanupFile, func(m map[string]any) {
				d := m["data"].(map[string]any)
				d["processes"] = d["processes"].([]any)[1:]
			})
		}},
	}
}

func TestDevFullForgeryMatrix(t *testing.T) {
	t.Parallel()
	for name, f := range forgeries() {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			dir := devPassingBundle(t)
			f.mutate(t, dir)
			reforge(t, dir)
			before := treeState(t, dir)
			code, rep, errOut := devCheck(t, dir)
			if code != 1 && code != 2 || rep.Result == ResultPass {
				t.Fatalf("a forged bundle checked %d %s", code, rep.Result)
			}
			if code == 1 && len(devCriterion(rep, f.crit).Codes) == 0 {
				t.Fatalf("criterion %s did not fail: %v %s", f.crit, devCodes(rep), errOut)
			}
			if !maps2Equal(before, treeState(t, dir)) {
				t.Fatal("the check changed its input")
			}
		})
	}
	t.Run("positive_controls", func(t *testing.T) {
		t.Parallel()
		// The genuine changes-requested/fix/review history (its final review
		// a report-only publication over the fix) with unknown provider usage
		// passes, and a re-derivation of unchanged inputs gives the same
		// report and audit bytes.
		dir := devPassingBundle(t)
		before := treeState(t, dir)
		code, rep, _ := devCheck(t, dir)
		cost := bundleDoc[DevCost](t, dir, devCostFile)
		if code != 0 || rep.Result != ResultPass || cost.ProviderUsage != usageUnknown || rep.Tasks != 6 {
			t.Fatalf("control %d %v %+v", code, devCodes(rep), cost)
		}
		if !maps2Equal(before, treeState(t, dir)) {
			t.Fatal("the check changed its input")
		}
		b, err := LoadDevBundle(dir)
		if err != nil {
			t.Fatal(err)
		}
		first, second := checkDevBundle(b, true), checkDevBundle(b, true)
		for n, data := range map[string][2][]byte{"report.json": {first.reportJSON, second.reportJSON}, "audit.json": {first.auditJSON, second.auditJSON},
			"report.txt": {[]byte(first.reportText), []byte(second.reportText)}, "audit.md": {[]byte(first.auditText), []byte(second.auditText)}} {
			if !bytes.Equal(data[0], data[1]) || !bytes.Equal(data[0], mustRead(t, filepath.Join(dir, n))) {
				t.Fatalf("a re-derivation of unchanged inputs changed %s", n)
			}
		}
	})
}
