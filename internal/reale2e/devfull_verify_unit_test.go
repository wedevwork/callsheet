package reale2e

import (
	"bytes"
	"compress/zlib"
	"crypto/sha1"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"testing"

	"github.com/go-git/go-git/v5/plumbing/filemode"
)

// Unit groups of design 12a-dev-flow-verify for FP-1, FP-2, FP-3 and
// FP-7: the bundle's schema and closed layout, the check's routing, the
// offline replay and dynamic audit, the offline publication recheck and
// the migration's contracts. Each mutates a clone of the shared passing
// bundle at one boundary.

// checkExit is the offline check's exit for dir.
func checkExit(t *testing.T, dir string) (int, string) {
	t.Helper()
	var out, errb bytes.Buffer
	code := CheckEvidence(dir, &out, &errb)
	return code, errb.String()
}

// writeClaimedLooseObject stores a loose object whose header declares n
// decoded bytes: the bound is enforced from the declaration, before any
// byte is inflated, so the body need not follow.
func writeClaimedLooseObject(t *testing.T, repoDir string, n int64) {
	t.Helper()
	var body bytes.Buffer
	zw := zlib.NewWriter(&body)
	fmt.Fprintf(zw, "blob %d\x00short", n)
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	sum := sha1.Sum(body.Bytes())
	name := hex.EncodeToString(sum[:])
	dir := filepath.Join(repoDir, "objects", name[:2])
	os.MkdirAll(dir, 0o755)
	if err := os.WriteFile(filepath.Join(dir, name[2:]), body.Bytes(), 0o444); err != nil {
		t.Fatal(err)
	}
}

// looseObject is one loose object file of the bundle's repository.
func looseObject(t *testing.T, dir string) string {
	t.Helper()
	objects := filepath.Join(dir, filepath.FromSlash(devRepoDir), "objects")
	entries, _ := os.ReadDir(objects)
	for _, e := range entries {
		if len(e.Name()) == 2 {
			files, _ := os.ReadDir(filepath.Join(objects, e.Name()))
			if len(files) > 0 {
				return filepath.Join(objects, e.Name(), files[0].Name())
			}
		}
	}
	t.Fatal("no loose object")
	return ""
}

func TestDevFullBundleSchema(t *testing.T) {
	t.Parallel()
	for name, c := range map[string]struct {
		mutate func(t *testing.T, dir string)
		want   int
	}{
		// Missing evidence and mismatched identities are failed evidence.
		"missing_record": {func(t *testing.T, dir string) { os.Remove(filepath.Join(dir, "tasks", "03", "wait.json")) }, 1},
		"missing_shard":  {func(t *testing.T, dir string) { os.Remove(filepath.Join(dir, "environment", "after-00.jsonl")) }, 1},
		"inventory_hash": {func(t *testing.T, dir string) {
			rewriteFile(t, dir, "tasks/01/final.txt", "Done.", "Done!")
		}, 1},
		"inventory_extra": {func(t *testing.T, dir string) {
			editJSON(t, dir, devManifestFile, func(m map[string]any) {
				inv := m["inventory"].([]any)
				m["inventory"] = append(inv, map[string]any{"path": "tasks/09/final.txt", "bytes": 1, "sha256": strings.Repeat("0", 64)})
			})
		}, 1},
		"foreign_run": {func(t *testing.T, dir string) {
			editJSON(t, dir, "tasks/02/plan.json", func(m map[string]any) { m["run_id"] = "20261010T120000Z-0a0b0c0d" })
		}, 1},
		// Malformed, unsafe or unsupported evidence is a schema error.
		"malformed_json": {func(t *testing.T, dir string) {
			os.WriteFile(filepath.Join(dir, "tasks", "03", "wait.json"), []byte("{"), 0o600)
		}, 2},
		"trailing_value": {func(t *testing.T, dir string) {
			p := filepath.Join(dir, "ledger.json")
			os.WriteFile(p, append(mustRead(t, p), []byte("{}\n")...), 0o600)
		}, 2},
		"unknown_field": {func(t *testing.T, dir string) {
			editJSON(t, dir, "tasks/02/plan.json", func(m map[string]any) { m["data"].(map[string]any)["extra"] = 1 })
		}, 2},
		"wrong_type": {func(t *testing.T, dir string) {
			editJSON(t, dir, "tasks/02/plan.json", func(m map[string]any) { m["data"].(map[string]any)["ordinal"] = "2" })
		}, 2},
		"wrong_envelope": {func(t *testing.T, dir string) {
			editJSON(t, dir, "tasks/02/plan.json", func(m map[string]any) { m["schema"] = devKind("observation") })
		}, 2},
		"duplicate_key": {func(t *testing.T, dir string) {
			rewriteFile(t, dir, "ledger.json", "{\n", "{\n  \"schema\": \"x\",\n")
		}, 2},
		"duplicate_manifest_key": {func(t *testing.T, dir string) {
			rewriteFile(t, dir, devManifestFile, "{\n", "{\n  \"schema\": \""+DevBundleSchema+"\",\n")
		}, 2},
		"invalid_utf8": {func(t *testing.T, dir string) {
			os.WriteFile(filepath.Join(dir, "tasks", "01", "final.txt"), []byte("Done.\xff\nSTATUS: DESIGN_DRAFT\n"), 0o600)
		}, 2},
		"unexpected_file": {func(t *testing.T, dir string) { os.WriteFile(filepath.Join(dir, "notes.txt"), []byte("x"), 0o600) }, 2},
		"unexpected_dir":  {func(t *testing.T, dir string) { os.MkdirAll(filepath.Join(dir, "tasks", "01", "extra"), 0o700) }, 2},
		"padded_ordinal": {func(t *testing.T, dir string) {
			os.Rename(filepath.Join(dir, "tasks", "06"), filepath.Join(dir, "tasks", "006"))
		}, 2},
		"ordinal_ceiling": {func(t *testing.T, dir string) {
			os.Rename(filepath.Join(dir, "tasks", "06"), filepath.Join(dir, "tasks", "15"))
		}, 2},
		"alternate_case": {func(t *testing.T, dir string) { os.Rename(filepath.Join(dir, "tasks"), filepath.Join(dir, "Tasks")) }, 2},
		"symlink_file": {func(t *testing.T, dir string) {
			p := filepath.Join(dir, "tasks", "01", "final.txt")
			os.Remove(p)
			os.Symlink(filepath.Join(dir, "tasks", "02", "final.txt"), p)
		}, 2},
		"symlink_dir": {func(t *testing.T, dir string) {
			os.RemoveAll(filepath.Join(dir, "tasks", "06"))
			os.Symlink(filepath.Join(dir, "tasks", "05"), filepath.Join(dir, "tasks", "06"))
		}, 2},
		"special_file": {func(t *testing.T, dir string) {
			p := filepath.Join(dir, "tasks", "01", "final.txt")
			os.Remove(p)
			if err := syscall.Mkfifo(p, 0o600); err != nil {
				t.Fatal(err)
			}
		}, 2},
		"text_limit": {func(t *testing.T, dir string) {
			os.WriteFile(filepath.Join(dir, "session", "phase-1.stderr.txt"), bytes.Repeat([]byte("x"), maxTextFile+1), 0o600)
		}, 2},
		"repo_file_limit": {func(t *testing.T, dir string) {
			p := filepath.Join(dir, filepath.FromSlash(devRepoDir), "objects", "ff", strings.Repeat("f", 38))
			os.MkdirAll(filepath.Dir(p), 0o755)
			os.WriteFile(p, bytes.Repeat([]byte{1}, maxRepoFileBytes+1), 0o444)
		}, 2},
		"decoded_limit": {func(t *testing.T, dir string) {
			writeClaimedLooseObject(t, filepath.Join(dir, filepath.FromSlash(devRepoDir)), maxDecodedBytes+1)
		}, 2},
		// A corrupt object is a mismatched hash: failed evidence, as in the
		// legacy checker (the repository criterion fails).
		"corrupt_object": {func(t *testing.T, dir string) {
			p := looseObject(t, dir)
			b := mustRead(t, p)
			b[len(b)/2] ^= 0xff
			os.Chmod(p, 0o644)
			os.WriteFile(p, b, 0o444)
		}, 1},
		"extra_ref": {func(t *testing.T, dir string) {
			refs := filepath.Join(dir, filepath.FromSlash(devRepoDir), "refs", "heads")
			os.WriteFile(filepath.Join(refs, "other"), mustRead(t, filepath.Join(refs, "seed")), 0o644)
		}, 2},
		"pack_dir": {func(t *testing.T, dir string) {
			p := filepath.Join(dir, filepath.FromSlash(devRepoDir), "objects", "pack", "pack-x.pack")
			os.MkdirAll(filepath.Dir(p), 0o755)
			os.WriteFile(p, []byte("PACK"), 0o444)
		}, 2},
		"alternates": {func(t *testing.T, dir string) {
			p := filepath.Join(dir, filepath.FromSlash(devRepoDir), "objects", "info", "alternates")
			os.MkdirAll(filepath.Dir(p), 0o755)
			os.WriteFile(p, []byte("/tmp\n"), 0o644)
		}, 2},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			dir := devPassingBundle(t)
			c.mutate(t, dir)
			if code, errOut := checkExit(t, dir); code != c.want {
				t.Fatalf("exit %d want %d: %s", code, c.want, errOut)
			}
		})
	}
	t.Run("supported", func(t *testing.T) {
		t.Parallel()
		dir := devPassingBundle(t)
		b, err := LoadDevBundle(dir)
		if err != nil {
			t.Fatal(err)
		}
		m := doc[DevManifest](b, devManifestFile)
		if m == nil || m.Schema != DevBundleSchema || len(b.ordinalsOf("tasks")) != 6 || b.Repo == nil {
			t.Fatalf("manifest %+v", m)
		}
		// Every structured record carries its own kind's envelope schema.
		for rel := range b.Files {
			if !strings.HasSuffix(rel, ".json") || strings.HasPrefix(rel, "environment/") {
				continue
			}
			var env struct {
				Schema string `json:"schema"`
			}
			json.Unmarshal(b.Files[rel], &env)
			if env.Schema == "" && rel != "setup/flow.json" && rel != "setup/mcp-config.json" && !strings.HasSuffix(rel, "/task.json") &&
				!strings.HasSuffix(rel, "/wait-result.json") {
				t.Errorf("%s has no schema", rel)
			}
		}
		// The inventory excludes itself and the derived files.
		for _, e := range m.Inventory {
			if slices.Contains([]string{devManifestFile, "report.json", "report.txt", "audit.json", "audit.md", "audit-review.md"}, e.Path) {
				t.Fatalf("the inventory lists %s", e.Path)
			}
		}
	})
	t.Run("audit_review_appendix", func(t *testing.T) {
		t.Parallel()
		// The owner's external appendix is allowed and never judged.
		dir := devPassingBundle(t)
		os.WriteFile(filepath.Join(dir, "audit-review.md"), []byte("# Audit review\n\nEverything is correct.\n"), 0o600)
		if code, errOut := checkExit(t, dir); code != 0 {
			t.Fatalf("exit %d %s", code, errOut)
		}
	})
}

func TestDevFullCheckDispatch(t *testing.T) {
	t.Parallel()
	t.Run("routes", func(t *testing.T) {
		t.Parallel()
		legacy := passingBundle(t)
		dev := devPassingBundle(t)
		for name, c := range map[string]struct {
			dir    func() string
			code   int
			schema string
			msg    string
		}{
			"legacy_v3": {func() string { return legacy }, 0, ReportSchema, ""},
			"dev_v1":    {func() string { return dev }, 0, devKind("report"), ""},
			"legacy_v1": {func() string {
				d := devPassingBundle(t)
				editJSON(t, d, devManifestFile, func(m map[string]any) { m["schema"] = ManifestSchemaV1 })
				return d
			}, 2, devKind("report"), "unsupported schema"},
			"legacy_v2": {func() string {
				d := devPassingBundle(t)
				editJSON(t, d, devManifestFile, func(m map[string]any) { m["schema"] = "callsheet-real-e2e/v2" })
				return d
			}, 2, devKind("report"), "unsupported schema"},
			"execution_only": {func() string {
				d := t.TempDir()
				os.WriteFile(filepath.Join(d, DevExecutionFile), []byte(`{"schema":"`+DevExecutionSchema+`"}`), 0o600)
				return d
			}, 2, devKind("report"), "execution-only"},
			"execution_with_manifest": {func() string {
				d := devPassingBundle(t)
				os.WriteFile(filepath.Join(d, DevExecutionFile), []byte(`{"schema":"`+DevExecutionSchema+`"}`), 0o600)
				return d
			}, 2, devKind("report"), "execution-only"},
			"qualification": {func() string {
				d := devPassingBundle(t)
				os.WriteFile(filepath.Join(d, devQualificationFile), []byte("{}"), 0o600)
				return d
			}, 2, devKind("report"), "qualification record"},
			"manifest_not_object": {func() string {
				d := devPassingBundle(t)
				os.WriteFile(filepath.Join(d, devManifestFile), []byte(`["x"]`), 0o600)
				return d
			}, 2, devKind("report"), "manifest.json"},
			"manifest_symlink": {func() string {
				d := devPassingBundle(t)
				os.Rename(filepath.Join(d, devManifestFile), filepath.Join(d, "m.json"))
				os.Symlink(filepath.Join(d, "m.json"), filepath.Join(d, devManifestFile))
				return d
			}, 2, devKind("report"), "manifest.json"},
			"no_root": {func() string { return filepath.Join(t.TempDir(), "absent") }, 2, devKind("report"), ""},
		} {
			var out, errb bytes.Buffer
			code := CheckEvidence(c.dir(), &out, &errb)
			var head struct {
				Schema string `json:"schema"`
				Result string `json:"result"`
			}
			json.Unmarshal(out.Bytes(), &head)
			if code != c.code || head.Schema != c.schema || !strings.Contains(errb.String(), c.msg) || code == 2 && head.Result == ResultPass {
				t.Errorf("%s: exit %d schema %s: %s", name, code, head.Schema, errb.String())
			}
		}
	})
	t.Run("checker_has_no_launch_or_network", func(t *testing.T) {
		t.Parallel()
		// The full-flow checker reaches no process, network or plane client.
		root := repoRoot(t)
		for _, name := range []string{"devfull_check.go", "devfull_bundle.go", "devfull_audit.go", "devfull_replay.go", "devfull_criteria.go",
			"devfull_coverage.go", "devfull_reviewcheck.go", "devfull_accounting.go", "devfull_sessionmap.go"} {
			fset := token.NewFileSet()
			f, err := parser.ParseFile(fset, filepath.Join(root, "internal", "reale2e", name), nil, 0)
			if err != nil {
				t.Fatal(err)
			}
			for _, imp := range f.Imports {
				p, _ := strconv.Unquote(imp.Path.Value)
				if p == "os/exec" || p == "net" || p == "net/http" || strings.HasSuffix(p, "/internal/client") || strings.HasSuffix(p, "/internal/adapter") {
					t.Errorf("checker file %s imports %s", name, p)
				}
			}
			ast.Inspect(f, func(n ast.Node) bool {
				if id, ok := n.(*ast.Ident); ok && slices.Contains([]string{"Launcher", "ExecLauncher", "runCommand", "World", "Plane", "WriteFile",
					"Create", "Remove", "RemoveAll", "Mkdir", "MkdirAll", "Rename", "Chmod"}, id.Name) {
					t.Errorf("checker file %s references %s", name, id.Name)
				}
				return true
			})
		}
	})
}

func TestDevFullOfflineReplay(t *testing.T) {
	t.Parallel()
	t.Run("forced_routes_agree_offline", func(t *testing.T) {
		t.Parallel()
		for name, c := range map[string]struct {
			steps []devAct
			owner []string
			code  int
			stop  string
		}{
			"recovery_cap": {[]devAct{{marker: markerDesignDraft}, {marker: markerDesignApproved}, {marker: markerQuestions},
				{marker: markerDesignDraft}, {marker: markerDesignApproved}, {marker: markerImplComplete}, {marker: markerReviewChanges},
				{marker: markerNeedsTriage}, {marker: markerTriageConf, designRequired: designRequiredYes}}, []string{"yes 1", "yes 2"}, 1, stopCap},
		} {
			// (FP-2's function test forces the other routes and caps.)
			r := newDevRig(t, c.steps)
			r.owner = c.owner
			r.expect(c.code, c.stop)
			code, rep := r.offline()
			if code != c.code || rep.Stop != c.stop || len(devCriterion(rep, "transitions").Codes) != 0 || len(devCriterion(rep, "owner_receipts").Codes) != 0 {
				t.Fatalf("%s: offline %d %q %v", name, code, rep.Stop, devCodes(rep))
			}
		}
	})
	for name, c := range map[string]struct {
		crit   string
		mutate func(t *testing.T, dir string)
	}{
		"forged_plan_goal": {"transitions", func(t *testing.T, dir string) {
			editJSON(t, dir, "tasks/03/plan.json", func(m map[string]any) {
				d := m["data"].(map[string]any)
				d["goal"] = d["goal"].(string) + "\nAlso rewrite FEATURE.md."
			})
		}},
		"forged_allowed_outputs": {"transitions", func(t *testing.T, dir string) {
			editJSON(t, dir, "tasks/02/plan.json", func(m map[string]any) {
				m["data"].(map[string]any)["allowed_outputs"] = []any{"design-review.md", "main.go"}
			})
		}},
		"forged_ledger": {"transitions", func(t *testing.T, dir string) {
			editJSON(t, dir, "ledger.json", func(m map[string]any) { m["data"].(map[string]any)["code_cycles"] = 0 })
		}},
		"forged_transition": {"transitions", func(t *testing.T, dir string) {
			editJSON(t, dir, "transitions/06.json", func(m map[string]any) { m["data"].(map[string]any)["reason"] = "coder/fix approved" })
		}},
		"forged_event_map": {"waits", func(t *testing.T, dir string) {
			editJSON(t, dir, "session/event-map.json", func(m map[string]any) {
				b, _ := json.Marshal(m)
				s := strings.Replace(string(b), `"first_line":`, `"first_line":1`, 1)
				var out map[string]any
				json.Unmarshal([]byte(s), &out)
				for k := range m {
					delete(m, k)
				}
				for k, v := range out {
					m[k] = v
				}
			})
		}},
		"extra_admission": {"admissions", func(t *testing.T, dir string) {
			editJSON(t, dir, devAdmissionsFile, func(m map[string]any) {
				sw := m["sweeps"].([]any)
				last := sw[len(sw)-1].(map[string]any)
				p := last["pages"].([]any)[0].(map[string]any)
				p["ids"] = append(p["ids"].([]any), hexID("t_", 77))
				last["tasks"] = append(last["tasks"].([]any), map[string]any{"id": hexID("t_", 77), "role": "coder", "created_at": "2026-10-10T12:00:00Z",
					"requested_by": "claude-code"})
			})
		}},
		"missing_admission": {"admissions", func(t *testing.T, dir string) {
			first, _ := taskOf(t, dir, 1)
			editJSON(t, dir, devAdmissionsFile, func(m map[string]any) {
				for _, s := range m["sweeps"].([]any) {
					s := s.(map[string]any)
					for _, p := range s["pages"].([]any) {
						p := p.(map[string]any)
						var ids []any
						for _, id := range p["ids"].([]any) {
							if id != first {
								ids = append(ids, id)
							}
						}
						if ids == nil {
							ids = []any{}
						}
						p["ids"] = ids
					}
					var tasks []any
					for _, x := range s["tasks"].([]any) {
						if x.(map[string]any)["id"] != first {
							tasks = append(tasks, x)
						}
					}
					if tasks == nil {
						tasks = []any{}
					}
					s["tasks"] = tasks
				}
			})
		}},
		"duplicate_admission_event": {"admissions", func(t *testing.T, dir string) {
			p := filepath.Join(dir, "events.jsonl")
			lines := strings.SplitAfter(string(mustRead(t, p)), "\n")
			for i, l := range lines {
				if strings.Contains(l, `"type":"`+EvTaskAdmitted+`"`) && strings.Contains(l, `"hop":"workflow-02"`) {
					// The second task's admission event names the first's ID.
					first, _ := taskOf(t, dir, 1)
					second, _ := taskOf(t, dir, 2)
					lines[i] = strings.Replace(l, second, first, 1)
					break
				}
			}
			os.WriteFile(p, []byte(strings.Join(lines, "")), 0o600)
		}},
		"stale_receipt_generation": {"owner_receipts", func(t *testing.T, dir string) {
			editJSON(t, dir, "receipts/01.json", func(m map[string]any) { m["data"].(map[string]any)["generation"] = 2 })
		}},
		"reordered_owner_input": {"owner_receipts", func(t *testing.T, dir string) {
			editJSON(t, dir, "decisions/01.json", func(m map[string]any) {
				d := m["data"].(map[string]any)
				seq, _ := d["event_seq"].(json.Number).Int64()
				d["event_seq"] = seq - 3
			})
		}},
		"invented_worker_answer": {"owner_receipts", func(t *testing.T, dir string) {
			editJSON(t, dir, "decisions/01.json", func(m map[string]any) { m["data"].(map[string]any)["window"] = "worker" })
		}},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			dir := devPassingBundle(t)
			c.mutate(t, dir)
			code, rep := reforge(t, dir)
			if code == 0 || rep.Result == ResultPass || len(devCriterion(rep, c.crit).Codes) == 0 {
				t.Fatalf("exit %d %v", code, devCodes(rep))
			}
		})
	}
}

// passingRowIDs are the shared passing bundle's mechanical audit rows in
// order: setup and preflights, the actual events, then the tail.
var passingRowIDs = []string{"preflight/01/selection", "preflight/02/selection", "preflight/03/selection", "preflight/04/selection",
	"preflight/05/selection", "setup/admissions", "setup/manifest", "setup/outcome", "setup/provenance", "setup/roles", "setup/seed", "setup/session",
	"transition/01/route", "phase/1/init", "phase/1/launch", "task/01/dispatch", "task/01/wait", "task/01/marker", "task/01/publication",
	"transition/02/route", "task/02/dispatch", "task/02/wait", "task/02/marker", "task/02/publication", "transition/03/route", "phase/1/exit",
	"decision/01/binding", "receipt/01/authorization", "transition/04/route", "phase/2/init", "phase/2/launch", "task/03/dispatch", "task/03/wait",
	"task/03/marker", "task/03/publication", "transition/05/route", "task/04/dispatch", "task/04/wait", "task/04/marker", "task/04/publication",
	"task/04/reviewer-execution", "transition/06/route", "task/05/dispatch", "task/05/wait", "task/05/marker", "task/05/publication",
	"transition/07/route", "task/06/dispatch", "task/06/wait", "task/06/marker", "task/06/publication", "task/06/reviewer-execution",
	"transition/08/route", "phase/2/exit", "case/empty_name", "case/help", "case/missing_value", "case/name", "case/no_args", "case/positional",
	"case/spaces", "case/unicode", "case/unknown_flag", "case/whitespace", "validation/baseline", "validation/coverage", "validation/signature",
	"validation/table", "validation/tests", "cleanup/coordinator-phase-1-032", "cleanup/coordinator-phase-2-050", "cleanup/plane-006",
	"cleanup/sidecar-claude-008", "cleanup/sidecar-claude-014", "cleanup/sidecar-codex-007", "cleanup/sidecar-codex-012", "cleanup/sidecar-grok-009",
	"cleanup/sidecar-grok-016", "cleanup/summary", "environment/protected", "deadlines/bounds", "cost/accounting"}

func TestDevFullDynamicAudit(t *testing.T) {
	t.Parallel()
	t.Run("stable_rows", func(t *testing.T) {
		t.Parallel()
		dir := devPassingBundle(t)
		a := bundleDoc[DevAudit](t, dir, "audit.json")
		var ids []string
		for _, r := range a.Rows {
			ids = append(ids, r.ID)
			if r.Layer != LayerMechanical || r.Verdict != VerdictCorrect || len(r.Codes) != 0 {
				t.Fatalf("row %+v", r)
			}
		}
		if !slices.Equal(ids, passingRowIDs) {
			t.Fatalf("rows %q", ids)
		}
		var judge []string
		for _, r := range a.JudgmentRows {
			judge = append(judge, r.ID)
			if r.Layer != LayerJudgment || r.Verdict != VerdictUnobserved || r.Correction != PendingReview {
				t.Fatalf("judgment %+v", r)
			}
		}
		if !slices.Equal(judge, []string{"judgment/task/01/design", "judgment/task/02/audit", "judgment/task/03/implementation", "judgment/task/04/review",
			"judgment/task/05/implementation", "judgment/task/06/review"}) {
			t.Fatalf("judgment rows %q", judge)
		}
		// Task rows carry the task identity and the cited bytes' digests.
		files := map[string]string{}
		for _, e := range a.Sources {
			files[e.Path] = e.SHA256
		}
		for _, r := range a.Rows {
			if strings.HasPrefix(r.ID, "task/") && (r.Task == "" || r.Ordinal == 0 || r.Role == "") {
				t.Fatalf("row %s lacks its task identity", r.ID)
			}
			for _, s := range r.Sources {
				if s.Commit == "" && files[s.File] != "" && files[s.File] != s.SHA256 {
					t.Fatalf("row %s cites %s with a wrong digest", r.ID, s.File)
				}
			}
		}
		if a.ReportSHA256 != sha256Hex(mustRead(t, filepath.Join(dir, "report.json"))) {
			t.Fatal("the audit is not bound to the report")
		}
		if text := string(mustRead(t, filepath.Join(dir, "audit.md"))); !strings.Contains(text, "# Judgment layer") ||
			strings.Count(text, "\n## ") != len(a.Rows)+len(a.JudgmentRows) {
			t.Fatal("audit.md does not render every row")
		}
	})
	t.Run("stopped_attempt_rows", func(t *testing.T) {
		t.Parallel()
		// A stopped attempt shows its blocked step as unobserved with its
		// cause and invents no unused task.
		r := newDevRig(t, []devAct{{marker: markerDesignDraft}, {marker: markerDesignApproved}})
		r.owner = []string{"no 1"}
		r.expect(1, "owner_no")
		a := bundleDoc[DevAudit](t, r.f.bundle(), "audit.json")
		unreached := 0
		for _, row := range a.Rows {
			if strings.HasPrefix(row.ID, "task/03/") {
				t.Fatalf("an unused task has a row: %s", row.ID)
			}
			if strings.Contains(row.Observation, "not reached: owner_no") {
				unreached++
				if row.Verdict != VerdictUnobserved || !strings.Contains(row.Correction, "not reached: owner_no") {
					t.Fatalf("row %+v", row)
				}
			}
		}
		if unreached == 0 {
			t.Fatal("no row shows the blocked step")
		}
	})
	t.Run("failed_rows_correct", func(t *testing.T) {
		t.Parallel()
		dir := devPassingBundle(t)
		editJSON(t, dir, "receipts/01.json", func(m map[string]any) {
			m["data"].(map[string]any)["review_sha256"] = strings.Repeat("0", 64)
		})
		reforge(t, dir)
		a := bundleDoc[DevAudit](t, dir, "audit.json")
		for _, r := range a.Rows {
			if r.ID == "receipt/01/authorization" {
				if r.Verdict != VerdictIncorrect || len(r.Codes) == 0 || !strings.Contains(r.Correction, "Nothing was repaired") {
					t.Fatalf("row %+v", r)
				}
				return
			}
		}
		t.Fatal("no receipt row")
	})
}

func TestDevFullOfflinePublications(t *testing.T) {
	t.Parallel()
	type edit = func(files map[string][]byte) map[string]filemode.FileMode
	set := func(path, content string) edit {
		return func(f map[string][]byte) map[string]filemode.FileMode { f[path] = []byte(content); return nil }
	}
	for name, c := range map[string]struct {
		ordinal int
		edit    edit
	}{
		"designer_adds_source":      {1, set("main_test.go", "package main\n")},
		"designer_creates_question": {1, set("design-questions.md", "Why?\n")},
		"auditor_edits_design":      {2, set("design.md", "# Other design\n")},
		"auditor_ignore_file":       {2, set(".gitignore", "*\n")},
		"coder_edits_feature":       {3, set("FEATURE.md", "# Another feature\n")},
		"coder_edits_gomod":         {3, set("go.mod", "module example.com/other\n\ngo 1.26\n")},
		"coder_nested":              {3, set("internal/x.go", "package internal\n")},
		"coder_scratch":             {3, set(reviewScratch+"/t/after/main.go", "package main\n")},
		"coder_deletes_test": {3, func(f map[string][]byte) map[string]filemode.FileMode {
			delete(f, "main_test.go")
			return nil
		}},
		"reviewer_mode": {4, func(f map[string][]byte) map[string]filemode.FileMode {
			return map[string]filemode.FileMode{"review.md": filemode.Executable}
		}},
		"reviewer_deletes_history": {4, func(f map[string][]byte) map[string]filemode.FileMode {
			delete(f, "revision-history.md")
			return nil
		}},
		"reviewer_oversized_report": {6, set("review.md", strings.Repeat("x", maxArtifactBytes+1))},
		"fixer_edits_design":        {5, set("design.md", "# Rewritten\n")},
		"fixer_non_utf8":            {5, set("main.go", "package main\n// \xff\n")},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			dir := devPassingBundle(t)
			code, rep := forgeTask(t, dir, c.ordinal, c.edit)
			if code == 0 || rep.Result == ResultPass || len(devCriterion(rep, "publications").Codes) == 0 {
				t.Fatalf("exit %d %v", code, devCodes(rep))
			}
		})
	}
	t.Run("report_only_chain", func(t *testing.T) {
		t.Parallel()
		// The final review's publication adds only review.md over the fix's
		// candidate; the oracle judges the candidate, not the publication.
		dir := devPassingBundle(t)
		b, err := LoadDevBundle(dir)
		if err != nil {
			t.Fatal(err)
		}
		c := newDevChecker(b)
		c.loadTrees()
		_, cand := taskOf(t, dir, 5)
		_, review := taskOf(t, dir, 6)
		ct, rt := c.trees[cand], c.trees[review]
		if len(rt) != len(ct) {
			// review.md may replace the previous review's report.
			if _, had := ct["review.md"]; had || len(rt) != len(ct)+1 {
				t.Fatalf("the review changed more than review.md: %d vs %d", len(rt), len(ct))
			}
		}
		for p, f := range ct {
			if p != "review.md" && !bytes.Equal(rt[p].Content, f.Content) {
				t.Fatalf("the review changed %s", p)
			}
		}
		if m := bundleDoc[DevManifest](t, dir, devManifestFile); m.FinalCandidate != cand || m.FinalReview != review {
			t.Fatalf("final identities %s %s", m.FinalCandidate, m.FinalReview)
		}
	})
}

// legacyFixturePins are the labelled legacy four-role example files, byte
// for byte as they shipped under docs/examples/real-e2e at main ee4e7ff.
var legacyFixturePins = map[string]string{
	"code-reviewer-instruction.md":   "8599b53a8b77860575ae8557dce4dc4f63bd5c8b27848a87a5d8179dd529d996",
	"code-reviewer-runbook.md":       "d12681d29c8d61eb1b6a8a55ccc0871fd407ca71f6d4f26fe5806c7733d82cb3",
	"coder-instruction.md":           "a3f999c7729c18716be821bc94081e6b2994e102e6bc132317c9fc6b87b3111c",
	"coder-runbook.md":               "0aa44cbb4418889e7bd37734b8eea392b6d9e910ed4437913af232f2ccef135e",
	"coordinator-phase-1.md":         "bf97ad8f6977bcbad07bd200d3a6308c51920552a3dc2d22b63e962e71afaaad",
	"coordinator-phase-2.md":         "84a9bc8d1e6769b808860944371885fc3e16e575f9c878429996cc2d5eaa4b86",
	"coordinator-prompt.md":          "be37d521905c030761a08060cf9cd62fbcc44cb1769fbbc9f26d3a221d44e34e",
	"designer-instruction.md":        "f61204b2a6bd0da5340de66826ba5eeb88ba362b8eb31063b9714fa379bf543e",
	"designer-runbook.md":            "11579a8d0f61d57c68d3dc2d2c1d29c7accb1c5806e7d28713f2f814f713eaf6",
	"design-reviewer-instruction.md": "4becc1a6d2162aad4491b0a0e7816d2af905d3eb11adf843472fcca5a56e6867",
	"design-reviewer-runbook.md":     "c50c1b26232bdb6e1154a9717b08131c6f7df1382dc457f84e910357ed97cbb9",
	"flow.json":                      "d2727cc36efa6071773f8f992dcbfc8d9dbf22ac719a51f28502dca3a6a1bd6f",
}

func TestDevFullMigration(t *testing.T) {
	t.Parallel()
	root := repoRoot(t)
	t.Run("legacy_fixture_pins", func(t *testing.T) {
		t.Parallel()
		entries, err := os.ReadDir(filepath.Join(root, filepath.FromSlash(legacyExampleDir)))
		if err != nil {
			t.Fatal(err)
		}
		var names []string
		for _, e := range entries {
			names = append(names, e.Name())
			if want, ok := legacyFixturePins[e.Name()]; !ok || sha256Hex(mustRead(t, filepath.Join(root, filepath.FromSlash(legacyExampleDir), e.Name()))) != want {
				t.Errorf("legacy fixture %s is not its pinned bytes", e.Name())
			}
		}
		if len(names) != len(legacyFixturePins) {
			t.Fatalf("legacy fixture holds %v", names)
		}
		if !strings.HasPrefix(legacyExampleDir, "internal/reale2e/testdata/") {
			t.Fatalf("the legacy example is not a test fixture: %s", legacyExampleDir)
		}
	})
	t.Run("docs_contract", func(t *testing.T) {
		t.Parallel()
		doc := string(mustRead(t, filepath.Join(root, "docs", "maintainers", "real-e2e.md")))
		for _, want := range []string{"reale2e run", "run-dev-full", "\n\n" + liveStatusParagraph + "\n\n", "dev-review-verify", "probe-dev-full",
			"exit 0", "callsheet-dev-full-evidence/v1"} {
			if !strings.Contains(doc, want) {
				t.Errorf("docs/maintainers/real-e2e.md does not say %q", want)
			}
		}
		readme := string(mustRead(t, filepath.Join(root, filepath.FromSlash(ExamplesDir), "README.md")))
		if !strings.Contains(readme, "dev-full") || strings.Contains(readme, "coordinator-phase-1.md") && !strings.Contains(readme, "legacy") {
			t.Fatalf("the examples README %q", readme)
		}
		for _, p := range devExerciseFiles() {
			if !strings.Contains(doc, "](../"+strings.TrimPrefix(p, "docs/")+")") {
				t.Errorf("docs/maintainers/real-e2e.md does not link %s", p)
			}
		}
	})
	t.Run("routing_and_exits", func(t *testing.T) {
		t.Parallel()
		// Every usage error is 2 before any effect; the legacy runtime and the
		// development runtime refuse each other's helpers.
		dev := r2DevRuntime(t)
		for name, args := range map[string][]string{
			"unknown_command": {"run-legacy"},
			"check_no_flag":   {"check"},
			"check_extra":     {"check", "--evidence", "/x", "--y"},
			"dev_next_legacy": {"dev-next", "--run", legacyRuntime(t)},
			"observe_dev":     {"observe", "--run", dev, "--task", hexID("t_", 1), "--hop", "designer"},
			"wait_dev":        {"wait-task", "--run", dev, "--task", hexID("t_", 1), "--hop", "designer"},
			"review_material": {"review-material", "--run", dev},
			"verify_no_args":  {"dev-review-verify"},
		} {
			var out, errb bytes.Buffer
			h := Host{Args: args, Stdout: &out, Stderr: &errb, NewWorld: func(Host) (*World, func(), error) {
				t.Errorf("%s built a world", name)
				return nil, nil, nil
			}}
			if code := MainWith(h); code != 2 {
				t.Errorf("%s: exit %d %s", name, code, errb.String())
			}
		}
	})
}

// devExerciseFiles are the shipped development method's repository paths.
func devExerciseFiles() []string {
	var out []string
	for p := range devSourcePaths() {
		out = append(out, strings.TrimPrefix(p, "setup/sources/"))
	}
	slices.Sort(out)
	return out
}

// legacyRuntime is a legacy runtime directory with its run file.
func legacyRuntime(t *testing.T) string {
	t.Helper()
	rt := shortTemp(t)
	run := "20261018T000000Z-01020304"
	b, _ := encodeJSON(RunFile{Schema: RunFileSchema, RunID: run, Callsheet: "/c", Plane: "https://127.0.0.1:1", CA: "/rt/ca.crt"})
	os.WriteFile(filepath.Join(rt, RunFileName), b, 0o600)
	os.WriteFile(filepath.Join(rt, ownerMarker), []byte(run+"\n"), 0o600)
	return rt
}
