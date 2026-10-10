package reale2e

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/go-git/go-git/v5/plumbing"

	"github.com/wedevwork/callsheet/internal/contract"
)

// TestFourHopChain (FP-4): four synthetic publications chain correctly on
// one pinned instance (each result's single parent is its hop's base, the
// Grok reviewer's unchanged tree still publishes the fourth commit), and
// each broken edge fails: a hop's base, parent, instance or marker.
func TestFourHopChain(t *testing.T) {
	t.Parallel()
	b := passingBundle(t)
	var tm TreeManifest
	readDoc(t, b+"/workspace/tree-manifest.json", &tm)
	r := evidenceRepo(t, b)
	parent := tm.SeedCommit
	for i, h := range tm.Hops {
		c := refCommit(t, r, evidenceRefs[i+1])
		if h.Hop != Hops[i] || h.Commit != c.Hash.String() || h.Parent != parent || len(c.ParentHashes) != 1 || c.ParentHashes[0].String() != parent {
			t.Fatalf("hop %d %+v", i, h)
		}
		parent = h.Commit
	}
	if tm.Hops[3].Tree != tm.Hops[2].Tree || tm.Hops[3].Commit == tm.Hops[2].Commit {
		t.Fatal("the reviewer's unchanged tree did not publish its own commit")
	}
	if code, _, _ := checkBundle(t, b); code != 0 {
		t.Fatal("the chain did not pass")
	}
	for i := range Hops {
		edges := map[string]struct {
			crit, code string
			f          func(b string)
		}{
			"base": {CritChain, "base_binding_mismatch", func(b string) {
				rebindHop(t, b, i, func(w *contract.WorkspaceBinding) { w.BaseCommit = ptr(zeros40); w.BaseSelector = zeros40 })
			}},
			"parent": {CritChain, "parent_mismatch", func(b string) {
				files, _ := hopFilesAt(t, b, i+1)
				addCommit(t, evidenceRepo(t, b), evidenceRefs[i+1], files)
			}},
			"instance": {CritChain, "workspace_instance_mismatch", func(b string) {
				rebindHop(t, b, i, func(w *contract.WorkspaceBinding) { w.Instance = fmt.Sprintf("%032x", 77) })
			}},
			"marker": {CritHops, "final_marker_missing", func(b string) { setFinal(t, b, hopDirOf(i), "no marker\n") }},
		}
		for name, e := range edges {
			b := passingBundle(t)
			e.f(b)
			code, r, _ := checkBundle(t, b)
			requireCode(t, fmt.Sprintf("hop %d %s", i+1, name), code, r, e.crit, e.code)
		}
	}
}

// TestChainUnits (UT-4): the supervisor refuses a wrong base, instance,
// selection or marker at observation (and cancels that task), and a
// wrong parent, a disallowed edit, a missing marker or a Grok source
// mismatch when results land.
func TestChainUnits(t *testing.T) {
	t.Parallel()
	observeRefusal := func(t *testing.T, f *fakeWorld, hop int) {
		t.Helper()
		if code := f.attempt(); code != 1 || f.sup.failCode != "binding_mismatch" {
			t.Fatalf("attempt = %d %q %v", code, f.sup.failCode, f.coord.codes)
		}
		if !slices.Contains(f.coord.codes, CodeTaskMismatch) || !slices.Contains(f.plane.cancels, f.coord.tasks[hop]) {
			t.Fatalf("codes %v cancels %v", f.coord.codes, f.plane.cancels)
		}
	}
	t.Run("wrong base", func(t *testing.T) {
		t.Parallel()
		f := newFakeWorld(t)
		f.coord.baseOf = func(i int, base string) string {
			if i == 1 {
				return f.coord.commit[0].String()
			}
			return base
		}
		observeRefusal(t, f, 1)
	})
	t.Run("wrong marker", func(t *testing.T) {
		t.Parallel()
		f := newFakeWorld(t)
		f.plane.dispatchErr = func(req contract.DispatchRequest) error {
			if req.Target.Value == HopDesigner && req.Workspace != nil {
				req.Payload[0] = "callsheet-real-e2e run=other hop=designer"
			}
			return nil
		}
		observeRefusal(t, f, 0)
	})
	t.Run("selection override", func(t *testing.T) {
		t.Parallel()
		f := newFakeWorld(t)
		f.coord.overrideHop = 1
		observeRefusal(t, f, 1)
	})
	t.Run("wrong instance", func(t *testing.T) {
		t.Parallel()
		f := newFakeWorld(t)
		f.coord.instanceOf = func(i int) string {
			if i == 2 {
				return fmt.Sprintf("%032x", 5)
			}
			return ""
		}
		if code := f.attempt(); code != 1 || f.sup.failCode != "binding_mismatch" {
			t.Fatalf("attempt = %d %q", code, f.sup.failCode)
		}
	})
	t.Run("grok source mismatch", func(t *testing.T) {
		t.Parallel()
		f := newFakeWorld(t)
		f.coord.goalOf = func(i int, g string) string {
			if i == 3 {
				return "Review.\n" + fileBlockBegin("design.md", sha256Hex([]byte("old\n"))) + "old\n" + fileBlockEnd("design.md")
			}
			return g
		}
		observeRefusal(t, f, 3)
	})
	t.Run("grok prompt overflow", func(t *testing.T) {
		t.Parallel()
		// A goal within the plane's limit plus large manuals exceeds the
		// 32 KiB composed Grok prompt: refused, never truncated.
		f := newFakeWorld(t)
		os.WriteFile(filepath.Join(f.checkout, filepath.FromSlash(ExamplesDir), "code-reviewer-runbook.md"), []byte(longComment(20<<10)), 0o644)
		f.coord.hopFiles[2]["main_test.go"] = append(append([]byte{}, codedMainTest...), []byte("// "+longComment(9<<10))...)
		observeRefusal(t, f, 3)
	})
	results := map[string]struct {
		setup func(f *fakeWorld)
		code  string
	}{
		"wrong parent": {func(f *fakeWorld) {
			f.coord.parentOf = func(i int) plumbing.Hash {
				if i == 2 {
					return f.coord.commit[0]
				}
				return f.coord.commit[i]
			}
		}, "parent_mismatch"},
		"designer edits code": {func(f *fakeWorld) { f.coord.hopFiles[0]["main.go"] = []byte("package main\n") }, "disallowed_change"},
		"reviewer writes":     {func(f *fakeWorld) { f.coord.hopFiles[3] = map[string][]byte{"review.md": []byte("x\n")} }, "disallowed_change"},
		"missing marker":      {func(f *fakeWorld) { f.coord.finals[0] = "done\n" }, "hop_not_approved"},
		"changes requested":   {func(f *fakeWorld) { f.coord.finals[1] = "no\nSTATUS: DESIGN_CHANGES_REQUESTED\n" }, "hop_not_approved"},
		"review not approved": {func(f *fakeWorld) { f.coord.finals[3] = "bugs\nSTATUS: REVIEW_CHANGES_REQUESTED\n" }, "hop_not_approved"},
		"hop failed":          {func(f *fakeWorld) { f.coord.failHop = 2 }, "hop_failed"},
		"report unapproved": {func(f *fakeWorld) {
			f.coord.hopFiles[1] = map[string][]byte{"design-review.md": []byte("fine\n")}
		}, "design_review_not_approved"},
		"pull fails":   {func(f *fakeWorld) { f.puller.err = fmt.Errorf("transfer refused") }, "result_pull_failed"},
		"final tests":  {func(f *fakeWorld) { f.override = failGoTest }, "final_tests_failed"},
		"no final msg": {func(f *fakeWorld) { f.coord.nilFinal = 1 }, "final_message_missing"},
		"unpublished":  {func(f *fakeWorld) { f.coord.unpublished = 0 }, "publication_missing"},
		"mode change": {func(f *fakeWorld) {
			f.coord.reviewerMode = true
		}, "reviewer_tree_changed"},
	}
	for name, tc := range results {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			f := newFakeWorld(t)
			tc.setup(f)
			if code := f.attempt(); code != 1 || f.sup.failCode != tc.code {
				t.Fatalf("attempt = %d %q, want %s\n%s", code, f.sup.failCode, tc.code, f.stderr.String())
			}
		})
	}
}

// longComment returns n bytes of comment text.
func longComment(n int) string {
	b := make([]byte, n)
	for i := range b {
		b[i] = 'x'
	}
	return string(b) + "\n"
}

// failGoTest fails only the final go test run (the baseline passes).
func failGoTest(s ProcSpec) (behavior, bool) {
	if s.Path == fakeGo && len(s.Args) > 0 && s.Args[0] == "test" && pathHasSuffix(s.Dir, "final/src") {
		return behavior{exit: 1, stdout: "--- FAIL\n"}, true
	}
	return behavior{}, false
}

func pathHasSuffix(p, suffix string) bool {
	return len(p) >= len(suffix) && p[len(p)-len(suffix):] == suffix
}
