package reale2e

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/wedevwork/callsheet/internal/contract"
)

// Unit groups of design 12a-dev-flow-verify for FP-6 and FP-8: the
// derived accounting, the offline owner-environment recheck and the
// qualification probe's record.

func TestDevFullAccounting(t *testing.T) {
	t.Parallel()
	t.Run("sweeps_coalesce_only_identical", func(t *testing.T) {
		t.Parallel()
		clock := newAutoClock()
		r := &sweepRecorder{clock: clock}
		page := []contract.TaskSummary{{TaskID: hexID("t_", 1)}}
		for i := 0; i < 3; i++ {
			s := r.begin(sweepMonitor)
			s.page("", page, nil)
			s.end(nil)
			clock.advance(time.Second)
		}
		changed := r.begin(sweepMonitor)
		changed.page("", append(slices.Clone(page), contract.TaskSummary{TaskID: hexID("t_", 2)}), nil)
		changed.end(nil)
		for i := 0; i < 2; i++ {
			s := r.begin(sweepMonitor)
			s.page("", page, ptr("next"))
			s.end(errors.New("transport closed"))
		}
		final := r.begin(sweepFinal)
		final.page("", page, nil)
		final.end(nil)
		if len(r.sweeps) != 5 || r.sweeps[0].Count != 3 || r.sweeps[0].First == r.sweeps[0].Last || r.sweeps[1].Count != 1 ||
			r.sweeps[2].Error == "" || r.sweeps[3].Error == "" || r.sweeps[4].Kind != sweepFinal || r.sweeps[2].Pages[0].Next != "next" {
			t.Fatalf("sweeps %+v", r.sweeps)
		}
	})
	t.Run("derived", func(t *testing.T) {
		t.Parallel()
		dir := devPassingBundle(t)
		b, err := LoadDevBundle(dir)
		if err != nil {
			t.Fatal(err)
		}
		m := doc[DevManifest](b, devManifestFile)
		c := deriveDevCost(b, m.RunID)
		if len(c.PreflightTasks) != 5 || len(c.WorkflowTasks) != 6 || len(c.RejectedTasks) != 0 || c.WorkerCalls != 11 || c.Completed != 11 ||
			c.Failed != 0 || c.PhaseStarts != 2 || len(c.Phases) != 2 || c.ProviderUsage != usageUnknown || c.AttemptElapsedMS == nil ||
			c.CleanupElapsedMS == nil || c.OwnerWaitMS <= 0 || len(c.QualificationRefs) != 0 {
			t.Fatalf("cost %+v", c)
		}
		for _, tc := range c.Tasks {
			if tc.DurationMS == nil || *tc.DurationMS < 0 {
				t.Fatalf("task %+v has no duration", tc)
			}
		}
		if !bytes.Equal(mustJSON(c), mustJSON(bundleDoc[DevCost](t, dir, devCostFile))) {
			t.Fatal("the stored cost is not the derivation")
		}
	})
	for name, c := range map[string]struct {
		crit   string
		mutate func(t *testing.T, dir string)
	}{
		"hidden_call": {"cost", func(t *testing.T, dir string) {
			editJSON(t, dir, devCostFile, func(m map[string]any) { m["worker_calls"] = 10 })
		}},
		"invented_usage": {"cost", func(t *testing.T, dir string) {
			editJSON(t, dir, devCostFile, func(m map[string]any) { m["provider_usage"] = "1200 tokens" })
		}},
		"unaccounted_settlement": {"cost", func(t *testing.T, dir string) {
			// A settled owned task the stored accounting omits.
			editJSON(t, dir, devCleanupFile, func(m map[string]any) {
				d := m["data"].(map[string]any)
				d["settled"] = append(d["settled"].([]any), map[string]any{"task": hexID("t_", 70), "label": "rejected", "state": "cancelled", "duration_ms": nil})
			})
		}},
		"pid_reused": {"cleanup", func(t *testing.T, dir string) {
			// A stop record whose PID matches but whose instance (start
			// sequence) is another: identity, not PID, reconciles.
			editJSON(t, dir, devCleanupFile, func(m map[string]any) {
				p := m["data"].(map[string]any)["processes"].([]any)[0].(map[string]any)
				seq, _ := p["started_seq"].(json.Number).Int64()
				p["started_seq"] = seq + 1000
			})
		}},
		"scan_failed": {"cleanup", func(t *testing.T, dir string) {
			editJSON(t, dir, devCleanupFile, func(m map[string]any) { m["data"].(map[string]any)["scan_error"] = "list tasks: transport closed" })
		}},
		"signal_without_exit": {"cleanup", func(t *testing.T, dir string) {
			editJSON(t, dir, devCleanupFile, func(m map[string]any) {
				p := m["data"].(map[string]any)["processes"].([]any)[1].(map[string]any)
				p["stopped_by"], p["exit_verified"], p["exit"] = StoppedTerm, false, ""
			})
		}},
		"runtime_kept": {"cleanup", func(t *testing.T, dir string) {
			editJSON(t, dir, devCleanupFile, func(m map[string]any) { m["data"].(map[string]any)["runtime_removed"] = false })
		}},
		"cleanup_over_budget": {"cleanup", func(t *testing.T, dir string) {
			editJSON(t, dir, devCleanupFile, func(m map[string]any) {
				d := m["data"].(map[string]any)
				s, _ := parseTime(d["started_at"].(string))
				d["finished_at"] = formatTime(s.Add(CleanupBound))
			})
		}},
		"unsettled_task": {"cleanup", func(t *testing.T, dir string) {
			editJSON(t, dir, devCleanupFile, func(m map[string]any) {
				d := m["data"].(map[string]any)
				d["settled"] = d["settled"].([]any)[1:]
			})
		}},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			dir := devPassingBundle(t)
			c.mutate(t, dir)
			code, rep := reforge(t, dir)
			if code != 1 || len(devCriterion(rep, c.crit).Codes) == 0 {
				t.Fatalf("exit %d %v", code, devCodes(rep))
			}
		})
	}
	t.Run("deadline_boundary", func(t *testing.T) {
		t.Parallel()
		// An event exactly at the attempt deadline is late (now >= deadline);
		// one second before it is in time.
		base := devPassingBundle(t)
		var last time.Time
		for _, l := range strings.Split(strings.TrimSpace(string(mustRead(t, filepath.Join(base, "events.jsonl")))), "\n") {
			var e Event
			json.Unmarshal([]byte(l), &e)
			if e.Type == EvFinalTests || e.Type == EvValidationFinished {
				if at, ok := parseTime(e.Time); ok && at.After(last) {
					last = at
				}
			}
		}
		for shift, late := range map[time.Duration]bool{0: true, time.Second: false} {
			dir := devPassingBundle(t)
			editJSON(t, dir, devManifestFile, func(m map[string]any) {
				m["attempt_deadline"] = formatTime(last.Add(shift))
				m["attempt_start"] = formatTime(last.Add(shift - AttemptBound))
			})
			_, rep := reforge(t, dir)
			if got := slices.Contains(devCriterion(rep, "deadlines").Codes, "attempt_deadline_exceeded"); got != late {
				t.Fatalf("shift %s: late %t, codes %v", shift, got, devCodes(rep))
			}
		}
	})
}

func TestDevFullOfflineEnvironment(t *testing.T) {
	t.Parallel()
	for name, mutate := range map[string]func(t *testing.T, dir string){
		"incomplete_shard": func(t *testing.T, dir string) {
			rewriteFile(t, dir, "environment/after-00.jsonl", `"complete":true`, `"complete":false`)
		},
		"record_count": func(t *testing.T, dir string) {
			rewriteFile(t, dir, "environment/before-00.jsonl", `"records":18`, `"records":17`)
		},
		"roots_mismatch": func(t *testing.T, dir string) {
			editJSON(t, dir, devManifestFile, func(m map[string]any) { m["protected_roots"] = []any{"/home/owner/src"} })
		},
		"depth_mismatch": func(t *testing.T, dir string) {
			editJSON(t, dir, devManifestFile, func(m map[string]any) { m["protect_depth"] = 2 })
		},
		"diff_missing": func(t *testing.T, dir string) { os.Remove(filepath.Join(dir, "environment", "diff.json")) },
		"diff_codes_cleared": func(t *testing.T, dir string) {
			editJSON(t, dir, "environment/diff.json", func(m map[string]any) { m["complete"] = false })
		},
		"vendor_home_removed": func(t *testing.T, dir string) {
			p := filepath.Join(dir, "environment", "after-00.jsonl")
			var out []string
			for _, l := range strings.SplitAfter(string(mustRead(t, p)), "\n") {
				if !strings.Contains(l, `"k":"home","s":"codex"`) {
					out = append(out, l)
				}
			}
			os.WriteFile(p, []byte(strings.Join(out, "")), 0o600)
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			dir := devPassingBundle(t)
			mutate(t, dir)
			code, rep := reforge(t, dir)
			if code != 1 || len(devCriterion(rep, "owner_environment").Codes) == 0 {
				t.Fatalf("exit %d %v", code, devCodes(rep))
			}
		})
	}
	t.Run("exclusions_unchanged", func(t *testing.T) {
		t.Parallel()
		// The development report prints exactly the legacy exclusions and
		// limitations; a noisy attempt never widens them.
		dir := devPassingBundle(t)
		text := string(mustRead(t, filepath.Join(dir, "report.txt")))
		var got []string
		for _, l := range strings.Split(text, "\n") {
			if strings.HasPrefix(l, "not covered: ") {
				got = append(got, l)
			}
		}
		want := append(append(slices.Clone(ClaudeStateExclusions), OwnerAcceptedExclusions...), "not covered: "+NestedRepoLimitation)
		if !slices.Equal(got, want) || len(ClaudeStateExclusions) != 3 || len(OwnerAcceptedExclusions) != 4 {
			t.Fatalf("exclusions %q", got)
		}
		if !strings.Contains(text, "protected roots: No repository root was protected") || !strings.Contains(text, "protected depth: repositories at most 3") {
			t.Fatal("the report does not state its protection limits")
		}
	})
	t.Run("synced_skills_classifier", func(t *testing.T) {
		t.Parallel()
		// Design 12a-dev-flow-verify r0.8 (FP-6): exactly the enumerated
		// Claude synced-skills bookkeeping is tool-maintained under the
		// development policy; the legacy policy is unchanged.
		for rel, tool := range map[string]bool{
			"skills/synced/acct/.last-complete-round":       true,
			"skills/synced/acct/.staging":                   true,
			"skills/synced/acct/.staging/greet":             true,
			"skills/synced/acct/.staging/greet/deep/x":      true,
			"skills/synced/.bucket-f21645ff-0001":           true,
			"skills/synced/.bucket-x":                       true,
			"skills":                                        false,
			"skills/synced":                                 false,
			"skills/synced/acct":                            false,
			"skills/synced/acct/manifest.json":              false,
			"skills/synced/acct/greet":                      false,
			"skills/synced/acct/greet/SKILL.md":             false,
			"skills/synced/acct/.bucket-x":                  false,
			"skills/synced/.bucketx":                        false,
			"skills/synced/.bucket-":                        false,
			"skills/synced/.bucket-x/inner":                 false,
			"skills/synced/acct/.last-complete-round/x":     false,
			"skills/synced/acct/greet/.last-complete-round": false,
			"skills/synced/acct/greet/.staging":             false,
			"skills/synced/acct/greet/.staging/x":           false,
			"skills/synced-other/.bucket-x":                 false,
			"skills/synced-other/acct/.staging":             false,
			"skills/other/.staging":                         false,
			"skills/.last-complete-round":                   false,
		} {
			want := ClassOwner
			if tool {
				want = ClassTool
			}
			if got := ownerClassFor(envPolicyDev, "claude", rel); got != want {
				t.Errorf("dev %s: %s want %s", rel, got, want)
			}
			if got := ownerClass("claude", rel); got != ClassOwner {
				t.Errorf("legacy %s: %s, the legacy policy is unchanged", rel, got)
			}
			if !tool && strings.HasPrefix(rel, "skills/synced/") && syncedSkillsBookkeeping("claude", rel) {
				t.Errorf("%s matched", rel)
			}
		}
		// Another vendor home keeps its own classes.
		for _, v := range []string{"codex", "grok", "cursor"} {
			for _, rel := range []string{"skills/synced/acct/.staging", "skills/synced/.bucket-x", "prompts/x"} {
				if ownerClassFor(envPolicyDev, v, rel) != ownerClass(v, rel) || syncedSkillsBookkeeping(v, rel) {
					t.Errorf("%s %s reclassified", v, rel)
				}
			}
		}
	})
	t.Run("synced_skills_rederived", func(t *testing.T) {
		t.Parallel()
		home := t.TempDir()
		synced := filepath.Join(home, ".claude", "skills", "synced")
		acct := filepath.Join(synced, "acct")
		for p, body := range map[string]string{
			filepath.Join(acct, "manifest.json"):                 "{}\n",
			filepath.Join(acct, "greet", "SKILL.md"):             "# greet\n",
			filepath.Join(acct, ".last-complete-round"):          "1\n",
			filepath.Join(acct, ".staging", "greet", "SKILL.md"): "staged\n",
			filepath.Join(acct, ".staging", "old", "x"):          "old\n",
			filepath.Join(synced, ".bucket-a"):                   "a\n",
			filepath.Join(synced, ".bucket-gone"):                "g\n",
			filepath.Join(home, "elsewhere", "target", "t.md"):   "t\n",
		} {
			write(t, p, body)
		}
		take := func(phase string) *Snapshot {
			s := takeSnapshotFor(envPolicyDev, phase, home, nil, nil, realGit(t), pathTokenizer{home: home}, realClock{}, time.Time{}, DefaultProtectDepth)
			if !s.Complete {
				t.Fatalf("%s incomplete: %s", phase, s.Code)
			}
			return s
		}
		before := take("before")
		// Added, modified and deleted bookkeeping of every pattern; the
		// staging entry finally a symlink to an outside tree (its own
		// metadata only, never followed).
		write(t, filepath.Join(acct, ".last-complete-round"), "2\n")
		write(t, filepath.Join(acct, ".staging", "greet", "SKILL.md"), "restaged\n")
		os.RemoveAll(filepath.Join(acct, ".staging", "old"))
		write(t, filepath.Join(acct, ".staging", "new", "y"), "y\n")
		write(t, filepath.Join(synced, ".bucket-a"), "aa\n")
		os.Remove(filepath.Join(synced, ".bucket-gone"))
		write(t, filepath.Join(synced, ".bucket-new"), "n\n")
		mid := take("after")
		d := compareEnvironmentFor(envPolicyDev, before, mid, nil)
		if len(d.Protected) != 0 || d.ToolDetailCount+d.ToolOmittedCount < 8 || slices.Contains(d.Codes, CodeEnvModified) {
			t.Fatalf("bookkeeping changes %+v codes %v tools %d", d.Protected, d.Codes, d.ToolDetailCount+d.ToolOmittedCount)
		}
		if legacy := compareEnvironmentFor(envPolicyLegacy, before, mid, nil); len(legacy.Protected) == 0 {
			t.Fatal("the legacy policy no longer protects synced-skills bookkeeping")
		}
		for _, s := range []*Snapshot{before, mid} {
			for _, r := range s.Records {
				if rel := decodeToken(r.Path); r.Kind == RecEntry && syncedSkillsBookkeeping("claude", rel) && (r.Class != ClassTool || r.SHA256 != "" || r.Method == MethodContent) {
					t.Fatalf("%s is not metadata-only tool state: %+v", rel, r)
				}
			}
		}
		// Offline: the checker re-derives the classes and the diff under
		// the development policy (never a bundle class), and the legacy
		// policy refuses the same records.
		files := map[string][]byte{}
		for _, s := range []*Snapshot{before, mid} {
			shards, err := encodeSnapshot(s)
			if err != nil {
				t.Fatal(err)
			}
			for i, name := range envFiles(s.Phase) {
				files[name] = shards[i]
			}
		}
		_, fitted, err := encodeDiffFor(envPolicyDev, d)
		if err != nil {
			t.Fatal(err)
		}
		for _, name := range append(envFiles("before"), envFiles("after")...) {
			fh := FileHash{Name: name, SHA256: sha256Hex(files[name])}
			if strings.HasPrefix(name, "environment/before") {
				fitted.Before = append(fitted.Before, fh)
			} else {
				fitted.After = append(fitted.After, fh)
			}
		}
		check := func(p envPolicy) []string {
			var codes []string
			checkOwnerEnvironment(envCheckInput{manifest: true, roots: []string{}, depth: DefaultProtectDepth, files: files, diff: &fitted,
				notReached: func() bool { return false }, policy: p}, func(c string) { codes = append(codes, c) })
			return codes
		}
		if codes := check(envPolicyDev); len(codes) != 0 {
			t.Fatalf("offline rederivation %v", codes)
		}
		if codes := check(envPolicyLegacy); !slices.Contains(codes, CodeEnvInventory) {
			t.Fatalf("legacy recheck of development classes %v", codes)
		}
		// A link substituted for the staging entry is its own record.
		os.RemoveAll(filepath.Join(acct, ".staging"))
		if err := os.Symlink(filepath.Join(home, "elsewhere", "target"), filepath.Join(acct, ".staging")); err != nil {
			t.Fatal(err)
		}
		linked := take("after")
		for _, r := range linked.Records {
			if rel := decodeToken(r.Path); strings.HasPrefix(rel, "skills/synced/acct/.staging/") {
				t.Fatalf("the staging link was followed: %s", rel)
			}
		}
		if d := compareEnvironmentFor(envPolicyDev, before, linked, nil); len(d.Protected) != 0 {
			t.Fatalf("staging link %+v", d.Protected)
		}
	})
	t.Run("synced_skills_protected", func(t *testing.T) {
		t.Parallel()
		// Everything else under skills stays protected under the
		// development policy: synced skill content, other skills,
		// near-prefix names, bookkeeping-looking names inside a skill, the
		// account and synced directories, a link substituted for skill
		// content, and a protected repository inside the staging tree.
		for name, mutate := range map[string]func(home string){
			"skill_content":    func(h string) { write(t, filepath.Join(h, ".claude/skills/synced/acct/greet/SKILL.md"), "# changed\n") },
			"manifest_content": func(h string) { write(t, filepath.Join(h, ".claude/skills/synced/acct/manifest.json"), `{"x":1}`+"\n") },
			"other_skill":      func(h string) { write(t, filepath.Join(h, ".claude/skills/mine/SKILL.md"), "# mine\n") },
			"bucket_in_account": func(h string) {
				write(t, filepath.Join(h, ".claude/skills/synced/acct/.bucket-x"), "b\n")
			},
			"bucketx":        func(h string) { write(t, filepath.Join(h, ".claude/skills/synced/.bucketx"), "b\n") },
			"bucket_subtree": func(h string) { write(t, filepath.Join(h, ".claude/skills/synced/.bucket-d/inner"), "b\n") },
			"round_in_skill": func(h string) {
				write(t, filepath.Join(h, ".claude/skills/synced/acct/greet/.last-complete-round"), "1\n")
			},
			"staging_in_skill": func(h string) { write(t, filepath.Join(h, ".claude/skills/synced/acct/greet/.staging/x"), "x\n") },
			"synced_other":     func(h string) { write(t, filepath.Join(h, ".claude/skills/synced-other/acct/.staging/x"), "x\n") },
			"account_mode":     func(h string) { os.Chmod(filepath.Join(h, ".claude/skills/synced/acct"), 0o700) },
			"account_removed":  func(h string) { os.RemoveAll(filepath.Join(h, ".claude/skills/synced/acct")) },
			"skill_link": func(h string) {
				p := filepath.Join(h, ".claude/skills/synced/acct/greet/SKILL.md")
				os.Remove(p)
				os.Symlink(filepath.Join(h, "elsewhere"), p)
			},
		} {
			t.Run(name, func(t *testing.T) {
				t.Parallel()
				home := t.TempDir()
				write(t, filepath.Join(home, ".claude/skills/synced/acct/greet/SKILL.md"), "# greet\n")
				write(t, filepath.Join(home, ".claude/skills/synced/acct/manifest.json"), "{}\n")
				write(t, filepath.Join(home, ".claude/skills/synced/.bucket-d/keep"), "k\n")
				os.Chmod(filepath.Join(home, ".claude/skills/synced/acct"), 0o755)
				take := func(phase string) *Snapshot {
					return takeSnapshotFor(envPolicyDev, phase, home, nil, nil, realGit(t), pathTokenizer{home: home}, realClock{}, time.Time{}, DefaultProtectDepth)
				}
				before := take("before")
				mutate(home)
				d := compareEnvironmentFor(envPolicyDev, before, take("after"), nil)
				if len(d.Protected) == 0 || !slices.Contains(d.Codes, CodeEnvModified) {
					t.Fatalf("%s was not protected: %+v", name, d)
				}
			})
		}
		t.Run("repository_in_staging", func(t *testing.T) {
			t.Parallel()
			home := t.TempDir()
			repo := filepath.Join(home, ".claude/skills/synced/acct/.staging/repo")
			newRepo(t, repo)
			root, err := canonicalPath(filepath.Join(home, ".claude/skills/synced/acct/.staging"))
			if err != nil {
				t.Fatal(err)
			}
			take := func(phase string) *Snapshot {
				s := takeSnapshotFor(envPolicyDev, phase, home, []string{root}, nil, realGit(t), pathTokenizer{home: home}, realClock{}, time.Time{}, DefaultProtectDepth)
				if !s.Complete {
					t.Fatalf("%s incomplete: %s", phase, s.Code)
				}
				return s
			}
			before := take("before")
			os.WriteFile(filepath.Join(repo, "a.txt"), []byte("changed\n"), 0o644)
			d := compareEnvironmentFor(envPolicyDev, before, take("after"), []string{root})
			if len(d.Protected) == 0 || !slices.ContainsFunc(d.Protected, func(c EnvChange) bool { return c.Scope == "" }) {
				t.Fatalf("a protected repository in the staging tree was waived: %+v", d.Protected)
			}
		})
	})
}

// devProbePassing is the shared qualified probe record.
var devProbePassing struct {
	sync.Once
	dir string
	err error
}

// devProbeBundle returns a fresh copy of the cached QUALIFIED record.
func devProbeBundle(t *testing.T) string {
	t.Helper()
	devProbePassing.Do(func() {
		r := probeRig(t)
		if code := r.run(); code != 0 {
			devProbePassing.err = fmt.Errorf("the shared probe failed (%d): %s", code, r.d.stopCode)
			return
		}
		devProbePassing.dir = filepath.Join(bundleCacheRoot, "dev-probe")
		devProbePassing.err = copyTree(r.f.bundle(), devProbePassing.dir)
	})
	if devProbePassing.err != nil {
		t.Fatal(devProbePassing.err)
	}
	dst := filepath.Join(t.TempDir(), "probe")
	if err := copyTree(devProbePassing.dir, dst); err != nil {
		t.Fatal(err)
	}
	return dst
}

// requalify recomputes a mutated record's inventory (a capable forger).
func requalify(t *testing.T, dir string) {
	t.Helper()
	inv, err := diskInventory(dir)
	if err != nil {
		t.Fatal(err)
	}
	editJSON(t, dir, devQualificationFile, func(m map[string]any) {
		var raw []any
		b, _ := json.Marshal(inv)
		json.Unmarshal(b, &raw)
		m["inventory"] = raw
	})
}

// qualificationCodes loads and re-checks a record.
func qualificationCodes(t *testing.T, dir string) []string {
	t.Helper()
	b, err := LoadDevQualification(dir)
	if err != nil {
		t.Fatalf("the record does not load: %v", err)
	}
	return CheckDevQualification(b)
}

func TestDevFullQualification(t *testing.T) {
	t.Parallel()
	t.Run("qualified_record", func(t *testing.T) {
		t.Parallel()
		dir := devProbeBundle(t)
		if codes := qualificationCodes(t, dir); len(codes) != 0 {
			t.Fatalf("codes %v", codes)
		}
		q := bundleDoc[DevQualification](t, dir, devQualificationFile)
		names := []string{}
		for _, s := range q.Stages {
			names = append(names, s.Name)
		}
		if !slices.Equal(names, []string{"models", "resume_wake", "print_notification", "reviewer_evidence"}) || q.WorkerCalls != 9 || q.Phases != 3 {
			t.Fatalf("stages %v calls %d phases %d", names, q.WorkerCalls, q.Phases)
		}
		// The pinned fixture is the repository's testdata, byte for byte.
		root := repoRoot(t)
		for _, name := range probeFixtureFiles {
			if !bytes.Equal(mustRead(t, filepath.Join(dir, "setup", "probe-fixture", name)), mustRead(t, filepath.Join(root, filepath.FromSlash(devProbeFixtureDir), name))) {
				t.Fatalf("fixture %s differs", name)
			}
		}
		// The unchanged fifteen-source method inventory, as a full run's.
		probeSources := bundleDoc[devEnvelope[[]SourceRecord]](t, dir, "setup/sources.json").Data
		runSources := bundleDoc[devEnvelope[[]SourceRecord]](t, devPassingBundle(t), "setup/sources.json").Data
		if len(probeSources) != 15 || !bytes.Equal(mustJSON(probeSources), mustJSON(runSources)) {
			t.Fatalf("sources %d", len(probeSources))
		}
		// Disjoint namespaces: no workflow task directory, probe refs only.
		for _, n := range []string{"tasks", "transitions", "receipts", "validation", devManifestFile} {
			if _, err := os.Stat(filepath.Join(dir, n)); err == nil {
				t.Fatalf("the record holds %s", n)
			}
		}
	})
	for name, c := range map[string]struct {
		mutate func(t *testing.T, dir string)
		code   string
	}{
		"prompt_source": {func(t *testing.T, dir string) {
			p := filepath.Join(dir, filepath.FromSlash(probeSystemSourceFile))
			os.WriteFile(p, append(mustRead(t, p), "Also design something.\n"...), 0o600)
		}, "prompt_provenance_mismatch"},
		"rendered_phase": {func(t *testing.T, dir string) {
			p := filepath.Join(dir, "setup", "probe-phase-2.md")
			os.WriteFile(p, append(mustRead(t, p), "x\n"...), 0o600)
		}, "prompt_provenance_mismatch"},
		"fixture": {func(t *testing.T, dir string) {
			os.WriteFile(filepath.Join(dir, "setup", "probe-fixture", "main.go"), []byte("package main\n"), 0o600)
		}, "fixture_mismatch"},
		"stage_claim": {func(t *testing.T, dir string) {
			editJSON(t, dir, devQualificationFile, func(m map[string]any) {
				m["stages"].([]any)[2].(map[string]any)["result"] = "fail"
			})
		}, "not_qualified"},
		"session": {func(t *testing.T, dir string) {
			editJSON(t, dir, devQualificationFile, func(m map[string]any) { m["session_id"] = "11111111-2222-4333-8444-555555555555" })
		}, "session_mismatch"},
		"phase_gate": {func(t *testing.T, dir string) {
			rewriteFile(t, dir, "session/phase-2.stdout.jsonl", `gate=probe generation=2`, `gate=probe generation=1`)
		}, "phase_result"},
		"wake_goal": {func(t *testing.T, dir string) {
			rewriteFile(t, dir, "probe/wake/02/task.json", "Do not use tools", "Use any tools")
		}, "wake_binding_mismatch"},
		"reviewer_evidence": {func(t *testing.T, dir string) {
			rewriteFile(t, dir, "probe/reviewer/01/review-evidence.json", `"status": "verified"`, `"status": "unobserved"`)
		}, "reviewer_evidence_mismatch"},
		"cleanup": {func(t *testing.T, dir string) {
			editJSON(t, dir, devCleanupFile, func(m map[string]any) { m["data"].(map[string]any)["complete"] = false })
		}, "cleanup_incomplete"},
	} {
		t.Run("rejects_"+name, func(t *testing.T) {
			t.Parallel()
			dir := devProbeBundle(t)
			c.mutate(t, dir)
			requalify(t, dir)
			if codes := qualificationCodes(t, dir); !slices.Contains(codes, c.code) {
				t.Fatalf("codes %v, want %s", codes, c.code)
			}
		})
	}
	t.Run("inventory", func(t *testing.T) {
		t.Parallel()
		dir := devProbeBundle(t)
		rewriteFile(t, dir, "probe/wake/01/final.txt", "CALLSHEET_PROBE_OK", "CALLSHEET_PROBE_KO")
		if codes := qualificationCodes(t, dir); !slices.Contains(codes, "inventory_mismatch") {
			t.Fatalf("codes %v", codes)
		}
	})
	for name, mutate := range map[string]func(t *testing.T, dir string){
		"fourth_wake": func(t *testing.T, dir string) {
			os.MkdirAll(filepath.Join(dir, "probe", "wake", "04"), 0o700)
			os.WriteFile(filepath.Join(dir, "probe", "wake", "04", "final.txt"), []byte("x"), 0o600)
		},
		"second_reviewer": func(t *testing.T, dir string) {
			os.MkdirAll(filepath.Join(dir, "probe", "reviewer", "02"), 0o700)
		},
		"workflow_task": func(t *testing.T, dir string) {
			os.MkdirAll(filepath.Join(dir, "tasks", "01"), 0o700)
			os.WriteFile(filepath.Join(dir, "tasks", "01", "final.txt"), []byte("x"), 0o600)
		},
		"unknown_file": func(t *testing.T, dir string) {
			os.WriteFile(filepath.Join(dir, "probe", "notes.txt"), []byte("x"), 0o600)
		},
		"fourth_phase": func(t *testing.T, dir string) {
			os.WriteFile(filepath.Join(dir, "session", "phase-4.stdout.jsonl"), []byte("{}\n"), 0o600)
		},
		"workflow_ref": func(t *testing.T, dir string) {
			refs := filepath.Join(dir, filepath.FromSlash(devRepoDir), "refs", "heads")
			os.WriteFile(filepath.Join(refs, "task-01"), mustRead(t, filepath.Join(refs, "seed")), 0o644)
		},
	} {
		t.Run("layout_"+name, func(t *testing.T) {
			t.Parallel()
			dir := devProbeBundle(t)
			mutate(t, dir)
			if _, err := LoadDevQualification(dir); err == nil {
				t.Fatal("an unexpected path loaded")
			}
		})
	}
	t.Run("full_flow_loader_refuses", func(t *testing.T) {
		t.Parallel()
		dir := devProbeBundle(t)
		if _, err := LoadDevBundle(dir); err == nil {
			t.Fatal("the full-flow loader admitted a qualification record")
		}
		if code, errOut := checkExit(t, dir); code != 2 || !strings.Contains(errOut, "qualification record") {
			t.Fatalf("check %d %s", code, errOut)
		}
	})
	t.Run("gate_and_owner_grammar", func(t *testing.T) {
		t.Parallel()
		for text, want := range map[string]string{"gate=probe generation=1": "1", "gate=probe generation=2\n": "2", "gate=probe generation=2\r\n": "2",
			"probe=complete": "complete", "probe=complete\n": "complete"} {
			gen, complete, err := parseProbePhaseGate(text)
			if err != nil || (want == "complete") != complete || !complete && itoa(gen) != want {
				t.Errorf("%q: %d %t %v", text, gen, complete, err)
			}
		}
		for _, text := range []string{"", "gate=probe generation=3", "gate=probe generation=1 ", " probe=complete", "gate=probe generation=1\nprobe=complete",
			"gate=full generation=1", "probe=complete\n\n", "Probe=complete"} {
			if _, _, err := parseProbePhaseGate(text); err == nil {
				t.Errorf("%q accepted", text)
			}
		}
		for line, want := range map[[2]string]string{{"continue 1", "1"}: "continue", {"no 2", "2"}: "no", {"continue 2\r", "2"}: "continue"} {
			n, _ := atoi(line[1])
			if got, ok := probeLineKind(line[0], n); !ok || got != want {
				t.Errorf("%q: %q %t", line[0], got, ok)
			}
		}
		// A stale window's answer, a trimmed or padded line and a full-flow
		// answer are not decisions.
		for _, line := range [][2]string{{"continue 1", "2"}, {" continue 2", "2"}, {"continue 2 ", "2"}, {"yes 2", "2"}, {"CONTINUE 2", "2"}} {
			n, _ := atoi(line[1])
			if _, ok := probeLineKind(line[0], n); ok {
				t.Errorf("%q accepted in phase %s", line[0], line[1])
			}
		}
		if probePhaseEnd(1) != "gate=probe generation=1" || probePhaseEnd(3) != "probe=complete" {
			t.Fatal("phase ends")
		}
	})
	t.Run("prompt_render", func(t *testing.T) {
		t.Parallel()
		if _, err := renderProbePrompt("p", "Run {{RUN_ID}} {{MISSING}}", map[string]string{"RUN_ID": "x"}); err == nil {
			t.Fatal("a missing render value was accepted")
		}
		if !strings.Contains(wakeGoal("RUN", 2), "Run RUN, phase 2.") || strings.Contains(wakeGoal("RUN", 2), "phase P") {
			t.Fatalf("goal %q", wakeGoal("RUN", 2))
		}
	})
	t.Run("reviewer_unsupported_shape", func(t *testing.T) {
		t.Parallel()
		r := newDevRig(t, []devAct{{}, {}, {}, {marker: markerReviewApproved, alter: "nested"}})
		r.command = "probe-dev-full"
		r.owner = []string{"continue 1", "continue 2"}
		r.expect(1, CodeReviewUnobserved)
		q := r.qualification()
		if q.Result != ResultBlocked || q.Stages[3].Result == stagePass || q.Stages[2].Result != stagePass {
			t.Fatalf("stages %+v", q.Stages)
		}
	})
	t.Run("failed_wake", func(t *testing.T) {
		t.Parallel()
		r := newDevRig(t, []devAct{{}, {alter: "failed"}, {}, {marker: markerReviewApproved}})
		r.command = "probe-dev-full"
		r.owner = []string{"continue 1", "continue 2"}
		if code := r.run(); code != 1 {
			t.Fatalf("exit %d", code)
		}
		if q := r.qualification(); q.Result != ResultBlocked || len(r.workflowTasks()) != 2 {
			t.Fatalf("a failed wake did not stop the probe: %+v", q.Stages)
		}
	})
	t.Run("opt_in_first", func(t *testing.T) {
		t.Parallel()
		var out, errb bytes.Buffer
		h := Host{GOOS: "linux", Args: []string{"probe-dev-full", "--callsheet", "/c", "--claude", "/a", "--codex", "/b", "--grok", "/g", "--evidence", "/e"},
			Getenv: func(string) string { return "" }, LookupEnv: func(string) (string, bool) { return "", false },
			Stdout: &out, Stderr: &errb, NewWorld: func(Host) (*World, func(), error) {
				t.Error("the probe built a world without opt-in")
				return nil, nil, nil
			}}
		if code := MainWith(h); code != 2 || !strings.Contains(errb.String(), ErrOptIn.Error()) {
			t.Fatalf("probe without opt-in %d %s", code, errb.String())
		}
	})
}
