package workspace

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/filemode"

	"github.com/wedevwork/callsheet/internal/contract"
	"github.com/wedevwork/callsheet/internal/testkit"
)

// refsFixture: main = c1 (c0 <- c1), a task ref at c0, and an orphan commit
// left after a rewind.
type refsFixture struct {
	m              *Manager
	root, inst     string
	c0, c1, orphan plumbing.Hash
}

func newRefsFixture(t *testing.T) *refsFixture {
	t.Helper()
	root := tempRoot(t)
	m := openAt(t, fastDeps(), root)
	v := mustCreate(t, m, "alpha")
	s := testkit.NewMemoryStore()
	f := &refsFixture{root: root, inst: v.Instance}
	f.c0 = commit(t, s, seedFiles(), "c0")
	files := seedFiles()
	files["c1.txt"] = testkit.FileSpec{Mode: filemode.Regular, Content: []byte("c1")}
	f.c1 = commit(t, s, files, "c1", f.c0)
	files["orphan.txt"] = testkit.FileSpec{Mode: filemode.Regular, Content: []byte("orphan")}
	f.orphan = commit(t, s, files, "orphan", f.c1)
	if r := receiveDirect(t, m, "alpha", v.Instance, cmd("refs/heads/main", plumbing.ZeroHash, f.orphan), packOf(t, s, objectsFor(t, s, f.orphan)...)); !r.ok() {
		t.Fatal(r)
	}
	if _, err := setRef(m, "alpha", v.Instance, "main", f.orphan.String(), strp(f.c1.String()), false); err != nil {
		t.Fatal(err)
	}
	m.Close()
	plantTask(t, root, "alpha", taskID('7'), f.c0, testkit.FixedWhen)
	f.m = openAt(t, fastDeps(), root)
	return f
}

// UT-5: every selector form resolves under the lock; hashes must be
// reachable commits (never orphans or trees); a missing selector is
// not_found; stale expected values conflict; there is no implicit main
// movement or merge.
func TestRefSetSelectors(t *testing.T) {
	f := newRefsFixture(t)
	m, inst := f.m, f.inst
	task := contract.TaskRefPrefix + taskID('7')
	for i, target := range []string{f.c0.String(), "main", "refs/heads/main", task} {
		branch := fmt.Sprintf("b%d", i)
		r, err := setRef(m, "alpha", inst, branch, contract.ExpectedAbsent, strp(target), false)
		want := f.c1
		if i == 0 || i == 3 {
			want = f.c0
		}
		if err != nil || *r.NewCommit != want.String() || r.OldCommit != nil || r.Ref != "refs/heads/"+branch {
			t.Fatalf("%s: %+v %v", target, r, err)
		}
	}
	for _, target := range []string{f.orphan.String(), strings.Repeat("0", 40), "nope", "refs/heads/nope", contract.TaskRefPrefix + taskID('8')} {
		if _, err := setRef(m, "alpha", inst, "x", contract.ExpectedAbsent, strp(target), false); codeOf(err) != contract.CodeNotFound {
			t.Fatalf("%s: %v", target, err)
		}
	}
	tree := commitTreeOf(t, testkitStoreFromRepo(t, m), f.c1)
	if _, err := setRef(m, "alpha", inst, "x", contract.ExpectedAbsent, strp(tree.String()), false); codeOf(err) != contract.CodeNotFound {
		t.Fatalf("tree target: %v", err)
	}
	// Stale values conflict, also for a same-value move and a delete.
	if _, err := setRef(m, "alpha", inst, "main", f.c0.String(), strp(f.c1.String()), false); codeOf(err) != contract.CodeConflict {
		t.Fatalf("stale move: %v", err)
	}
	if _, err := setRef(m, "alpha", inst, "main", contract.ExpectedAbsent, strp("main"), false); codeOf(err) != contract.CodeConflict {
		t.Fatalf("create over an existing branch: %v", err)
	}
	if _, err := setRef(m, "alpha", inst, "gone", f.c0.String(), nil, true); codeOf(err) != contract.CodeConflict {
		t.Fatalf("delete of a missing branch: %v", err)
	}
	// A same-value CAS succeeds and still publishes a generation.
	gen := genOf(t, m, "alpha")
	if r, err := setRef(m, "alpha", inst, "main", f.c1.String(), strp("main"), false); err != nil || *r.OldCommit != *r.NewCommit || r.Generation == gen {
		t.Fatalf("same value: %+v %v", r, err)
	}
	// A non-fast-forward explicit move (main back to c0) moves only main.
	before := refsOf(t, m, "alpha")
	if _, err := setRef(m, "alpha", inst, "main", f.c1.String(), strp(f.c0.String()), false); err != nil {
		t.Fatal(err)
	}
	after := refsOf(t, m, "alpha")
	for n, c := range before {
		if n != "refs/heads/main" && after[n] != c {
			t.Fatalf("%s moved", n)
		}
	}
	if after["refs/heads/main"] != f.c0.String() {
		t.Fatalf("main %s", after["refs/heads/main"])
	}
	// Deleting main leaves HEAD unborn: no main row, other branches stay.
	if r, err := setRef(m, "alpha", inst, "main", f.c0.String(), nil, true); err != nil || r.NewCommit != nil || *r.OldCommit != f.c0.String() {
		t.Fatalf("delete main %+v %v", r, err)
	}
	st, _ := m.Status(context.Background(), "alpha", StatusQuery{Limit: 100})
	for _, row := range st.Refs {
		if row.Name == "refs/heads/main" {
			t.Fatal("main still listed")
		}
	}
	if v, _ := m.Show(context.Background(), "alpha"); v.DefaultBranch != contract.DefaultBranchRef {
		t.Fatalf("default branch %+v", v)
	}
	// Namespace conflicts.
	if _, err := setRef(m, "alpha", inst, "b0/x", contract.ExpectedAbsent, strp("b1"), false); codeOf(err) != contract.CodeConflict {
		t.Fatalf("namespace conflict: %v", err)
	}
	if _, err := setRef(m, "alpha", inst, "p/q", contract.ExpectedAbsent, strp("b1"), false); err != nil {
		t.Fatal(err)
	}
	if _, err := setRef(m, "alpha", inst, "p", contract.ExpectedAbsent, strp("b1"), false); codeOf(err) != contract.CodeConflict {
		t.Fatalf("prefix conflict: %v", err)
	}
	// A deleted nested branch leaves no directory in the way.
	if _, err := setRef(m, "alpha", inst, "p/q", f.c1.String(), nil, true); err != nil {
		t.Fatal(err)
	}
	if _, err := setRef(m, "alpha", inst, "p", contract.ExpectedAbsent, strp("b1"), false); err != nil {
		t.Fatalf("after nested delete: %v", err)
	}
	if _, err := setRef(m, "alpha", strings.Repeat("0", 32), "q", contract.ExpectedAbsent, strp("b1"), false); codeOf(err) != contract.CodeConflict {
		t.Fatalf("stale instance: %v", err)
	}
	if _, err := setRef(m, "beta", inst, "q", contract.ExpectedAbsent, strp("b1"), false); codeOf(err) != contract.CodeNotFound {
		t.Fatalf("missing workspace: %v", err)
	}
	// Stale updates keep failing across a restart.
	m.Close()
	m2 := openAt(t, fastDeps(), f.root)
	if _, err := setRef(m2, "alpha", inst, "b1", f.c0.String(), strp("b0"), false); codeOf(err) != contract.CodeConflict {
		t.Fatalf("stale after restart: %v", err)
	}
}

// testkitStoreFromRepo copies alpha's current objects into memory.
func testkitStoreFromRepo(t testing.TB, m *Manager) *memStore {
	t.Helper()
	h, _ := m.lookup("alpha")
	h.lock.RLock(context.Background())
	defer h.lock.RUnlock()
	s, fs := openStorage(h.repo(), m.d)
	defer closeStorage(s, fs)
	out := testkit.NewMemoryStore()
	iter, _ := s.IterEncodedObjects(plumbing.AnyObject)
	iter.ForEach(func(o plumbing.EncodedObject) error { _, err := out.SetEncodedObject(o); return err })
	return out
}

// UT-5: the portable branch-name contract applies to ref-set destinations,
// target selectors and status cursors on the contract path, before any
// lookup or write; distinct lowercase names stay distinct rows.
func TestPortableNames(t *testing.T) {
	f := newRefsFixture(t)
	m, inst := f.m, f.inst
	a200 := strings.Repeat("a", 200)
	full512 := "refs/heads/" + a200 + "/" + strings.Repeat("b", 200) + "/" + strings.Repeat("c", 99)
	gen := genOf(t, m, "alpha")
	for _, bad := range []string{"Main", "MAIN", "café", "café", "ma\xffin", "ma�in", strings.Repeat("a", 201),
		strings.Repeat("a", 256), full512 + "d", "refs/heads/Main", "refs/tags/v1", "", "a//b", "HEAD"} {
		_, err := contract.ValidateRefSet(contract.WorkspaceRefSetRequest{Instance: inst, Branch: bad, Expected: contract.ExpectedAbsent, Target: strp("main")})
		if codeOf(err) != contract.CodeInvalidArgument {
			t.Fatalf("destination %q: %v", bad, err)
		}
		_, err = contract.ValidateRefSet(contract.WorkspaceRefSetRequest{Instance: inst, Branch: "ok", Expected: contract.ExpectedAbsent, Target: strp(bad)})
		if codeOf(err) != contract.CodeInvalidArgument {
			t.Fatalf("target %q: %v", bad, err)
		}
		full := bad
		if !strings.HasPrefix(bad, "refs/") {
			full = "refs/heads/" + bad
		}
		if contract.ValidStatusCursor(full) {
			t.Fatalf("cursor %q accepted", full)
		}
	}
	// A task ref is a valid target and cursor but never a destination.
	task := contract.TaskRefPrefix + taskID('7')
	if _, err := contract.ValidateRefSet(contract.WorkspaceRefSetRequest{Instance: inst, Branch: task, Expected: contract.ExpectedAbsent, Target: strp("main")}); codeOf(err) != contract.CodeInvalidArgument {
		t.Fatalf("task destination: %v", err)
	}
	if !contract.ValidStatusCursor(task) {
		t.Fatal("task cursor refused")
	}
	if genOf(t, m, "alpha") != gen {
		t.Fatal("rejected names wrote a generation")
	}
	for _, good := range []string{strings.Repeat("z", 200), full512, "main-x", "a.b_c/d-e", "m", "main2"} {
		if _, err := setRef(m, "alpha", inst, good, contract.ExpectedAbsent, strp("main"), false); err != nil {
			t.Fatalf("%q: %v", good, err)
		}
	}
	st, err := m.Status(context.Background(), "alpha", StatusQuery{Limit: 100})
	if err != nil {
		t.Fatal(err)
	}
	names := map[string]bool{}
	for _, r := range st.Refs {
		names[r.Name] = true
	}
	for _, want := range []string{"refs/heads/main", "refs/heads/main2", "refs/heads/m", full512} {
		if !names[want] {
			t.Fatalf("missing row %s in %v", want, names)
		}
	}
}

// UT-5: status pages are sorted by raw bytes with an exclusive cursor; a
// changed generation or instance between pages conflicts.
func TestStatusPagination(t *testing.T) {
	f := newRefsFixture(t)
	m, inst := f.m, f.inst
	for _, b := range []string{"z", "a-1", "a.1", "a/1", "a0"} {
		if _, err := setRef(m, "alpha", inst, b, contract.ExpectedAbsent, strp("main"), false); err != nil {
			t.Fatal(err)
		}
	}
	ctx := context.Background()
	var all []string
	q := StatusQuery{Limit: 2}
	first := true
	for {
		p, err := m.Status(ctx, "alpha", q)
		if err != nil {
			t.Fatal(err)
		}
		if first && p.DefaultBranch != contract.DefaultBranchRef {
			t.Fatalf("header %+v", p)
		}
		first = false
		for _, r := range p.Refs {
			all = append(all, r.Name)
			if strings.HasPrefix(r.Name, contract.TaskRefPrefix) != (r.PublishedAt != nil) {
				t.Fatalf("published_at %+v", r)
			}
		}
		if p.NextAfter == nil {
			break
		}
		q = StatusQuery{After: *p.NextAfter, Instance: p.Instance, Generation: p.Generation, Limit: 2}
	}
	want := []string{"refs/callsheet/tasks/" + taskID('7'), "refs/heads/a-1", "refs/heads/a.1", "refs/heads/a/1", "refs/heads/a0", "refs/heads/main", "refs/heads/z"}
	if strings.Join(all, ",") != strings.Join(want, ",") {
		t.Fatalf("pages %v", all)
	}
	p, _ := m.Status(ctx, "alpha", StatusQuery{Limit: 1})
	if _, err := setRef(m, "alpha", inst, "late", contract.ExpectedAbsent, strp("main"), false); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Status(ctx, "alpha", StatusQuery{After: *p.NextAfter, Instance: p.Instance, Generation: p.Generation, Limit: 1}); codeOf(err) != contract.CodeConflict {
		t.Fatalf("pinned generation: %v", err)
	}
	if _, err := m.Status(ctx, "alpha", StatusQuery{After: *p.NextAfter, Instance: strings.Repeat("0", 32), Generation: genOf(t, m, "alpha"), Limit: 1}); codeOf(err) != contract.CodeConflict {
		t.Fatalf("pinned instance: %v", err)
	}
}

// UT-5: concurrent equal-old ref sets: exactly one of the value-changing
// writers wins; two same-value writers may both succeed serially.
func TestRefSetConcurrentCAS(t *testing.T) {
	f := newRefsFixture(t)
	m, inst := f.m, f.inst
	var wg sync.WaitGroup
	wins := make(chan string, 8)
	start := make(chan struct{})
	targets := []string{f.c0.String(), f.c0.String(), f.c0.String(), "refs/callsheet/tasks/" + taskID('7')}
	for i, target := range targets {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			if _, err := setRef(m, "alpha", inst, "main", f.c1.String(), strp(target), false); err == nil {
				wins <- fmt.Sprint(i)
			} else if codeOf(err) != contract.CodeConflict {
				t.Errorf("writer %d: %v", i, err)
			}
		}()
	}
	close(start)
	wg.Wait()
	close(wins)
	if n := len(wins); n != 1 {
		t.Fatalf("%d winners", n)
	}
	var same sync.WaitGroup
	errs := make(chan error, 2)
	for range 2 {
		same.Add(1)
		go func() {
			defer same.Done()
			_, err := setRef(m, "alpha", inst, "main", f.c0.String(), strp(f.c0.String()), false)
			errs <- err
		}()
	}
	same.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("same-value writer: %v", err)
		}
	}
}
