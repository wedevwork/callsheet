package workspace

import (
	"context"
	"testing"
	"time"

	"github.com/go-git/go-git/v5/plumbing"

	"github.com/wedevwork/callsheet/internal/contract"
	"github.com/wedevwork/callsheet/internal/testkit"
)

// A complete lifecycle through the production handler and manager: create,
// push, fetch into an independent store, explicit ref set, status, diff,
// prune, restart and remove.
func TestLifecycle(t *testing.T) {
	ctx := context.Background()
	m, root := newManager(t)
	g := serveGit(t, m)
	v := mustCreate(t, m, "alpha")
	if v.DefaultBranch != contract.DefaultBranchRef || v.SizeBytes <= 0 || !contract.ValidWorkspaceToken(v.Generation) {
		t.Fatalf("view %+v", v)
	}
	r := g.remote("alpha", v.Instance)
	refs, err := r.Refs(ctx)
	if err != nil || len(refs) != 0 {
		t.Fatalf("empty refs %v %v", refs, err)
	}
	local := testkit.NewMemoryStore()
	c0 := commit(t, local, seedFiles(), "c0")
	push(t, r, local, "refs/heads/main", plumbing.ZeroHash, c0)
	files := seedFiles()
	files["README.md"] = testkit.FileSpec{Mode: files["README.md"].Mode, Content: []byte("changed\n")}
	c1 := commit(t, local, files, "c1", c0)
	push(t, r, local, "refs/heads/main", c0, c1)
	if got := refsOf(t, m, "alpha"); got["refs/heads/main"] != c1.String() || len(got) != 1 {
		t.Fatalf("refs %v", got)
	}
	other := testkit.NewMemoryStore()
	if err := r.Fetch(ctx, other, []plumbing.Hash{c1}, nil); err != nil {
		t.Fatalf("fetch: %v", err)
	}
	a, err := testkit.TreeFiles(other, c1)
	if err != nil {
		t.Fatal(err)
	}
	if err := testkit.EqualFiles(a, files); err != nil {
		t.Fatal(err)
	}
	rs, err := setRef(m, "alpha", v.Instance, "feature", contract.ExpectedAbsent, strp(c0.String()), false)
	if err != nil || rs.NewCommit == nil || *rs.NewCommit != c0.String() || rs.OldCommit != nil {
		t.Fatalf("ref set %+v %v", rs, err)
	}
	st, err := m.Status(ctx, "alpha", StatusQuery{Limit: 100})
	if err != nil || len(st.Refs) != 2 || st.Refs[0].Name != "refs/heads/feature" || st.Refs[1].Name != "refs/heads/main" {
		t.Fatalf("status %+v %v", st, err)
	}
	d, err := m.Diff(ctx, "alpha", DiffQuery{Base: contract.Selector{Kind: contract.SelectorKindBranch, Value: "refs/heads/feature"},
		Target: contract.Selector{Kind: contract.SelectorKindBranch, Value: "refs/heads/main"}, Limit: 100})
	if err != nil || len(d.Changes) != 1 || d.Changes[0].Kind != contract.ChangeModified {
		t.Fatalf("diff %+v %v", d, err)
	}
	pr, err := m.Prune(ctx, "alpha", v.Instance, time.Now())
	if err != nil || pr.RemovedTaskRefs != 0 {
		t.Fatalf("prune %+v %v", pr, err)
	}
	m.Close()
	m2 := openAt(t, fastDeps(), root)
	if got := refsOf(t, m2, "alpha"); got["refs/heads/main"] != c1.String() || got["refs/heads/feature"] != c0.String() {
		t.Fatalf("reopened refs %v", got)
	}
	if _, err := m2.Remove(ctx, "alpha", v.Instance); err != nil {
		t.Fatalf("remove: %v", err)
	}
	if l, _ := m2.List("", 100); len(l.Workspaces) != 0 {
		t.Fatalf("list after remove %+v", l)
	}
}
