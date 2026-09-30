package workspace

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/filemode"

	"github.com/wedevwork/callsheet/internal/contract"
	"github.com/wedevwork/callsheet/internal/testkit"
)

func taskID(c byte) string { return "t_" + strings.Repeat(string(c), 32) }

// hasObject reports whether name's current generation stores h.
func hasObject(t testing.TB, m *Manager, name string, h plumbing.Hash) bool {
	t.Helper()
	hd, err := m.lookup(name)
	if err != nil {
		t.Fatal(err)
	}
	hd.lock.RLock(context.Background())
	defer hd.lock.RUnlock()
	s, fs := openStorage(hd.repo(), m.d)
	defer closeStorage(s, fs)
	return s.HasEncodedObject(h) == nil
}

// pruneFixture: main = m1 (m0 <- m1), task refs published at old (a),
// equal (b) and new (c) times, where a's commit is exclusive, b's shares
// main's history and c's parent is an otherwise exclusive commit; plus an
// orphan commit left by a branch rewind. Commit dates are forged into the
// future so they can never be mistaken for age.
type pruneFixture struct {
	root, inst                 string
	m0, m1, ta, tb, tcP, tc, o plumbing.Hash
	cutoff                     time.Time
}

func newPruneFixture(t *testing.T) *pruneFixture {
	t.Helper()
	ctx := context.Background()
	f := &pruneFixture{root: tempRoot(t), cutoff: time.Date(2026, 9, 30, 12, 0, 0, 0, time.FixedZone("plus2", 2*3600))}
	m := openAt(t, fastDeps(), f.root)
	v := mustCreate(t, m, "alpha")
	f.inst = v.Instance
	s := testkit.NewMemoryStore()
	forged := time.Date(1999, 1, 1, 0, 0, 0, 0, time.UTC)
	mk := func(content, msg string, when time.Time, parents ...plumbing.Hash) plumbing.Hash {
		tree, err := testkit.WriteTree(s, map[string]testkit.FileSpec{"f": {Mode: filemode.Regular, Content: []byte(content)}})
		if err != nil {
			t.Fatal(err)
		}
		h, err := testkit.WriteCommit(s, tree, parents, msg, when)
		if err != nil {
			t.Fatal(err)
		}
		return h
	}
	f.m0 = mk("m0", "m0", forged)
	f.m1 = mk("m1", "m1", forged, f.m0)
	f.ta = mk("task-a-exclusive", "a", time.Now().Add(1000*time.Hour))
	f.tb = mk("task-b", "b", forged, f.m1)
	f.tcP = mk("task-c-parent", "cp", forged)
	f.tc = mk("task-c", "c", forged, f.tcP)
	f.o = mk("orphan", "o", forged, f.m1)
	g := serveGit(t, m)
	r := g.remote("alpha", v.Instance)
	push(t, r, s, "refs/heads/main", plumbing.ZeroHash, f.o)
	// Rewind main: o becomes an orphan until prune.
	if _, err := setRef(m, "alpha", v.Instance, "main", f.o.String(), strp(f.m1.String()), false); err != nil {
		t.Fatal(err)
	}
	m.Close()
	plantObjects(t, f.root, "alpha", s)
	utc := f.cutoff.UTC()
	plantTask(t, f.root, "alpha", taskID('a'), f.ta, utc.Add(-time.Nanosecond))
	plantTask(t, f.root, "alpha", taskID('b'), f.tb, utc)
	plantTask(t, f.root, "alpha", taskID('c'), f.tc, utc.Add(time.Second))
	_ = ctx
	return f
}

// UT-3: prune removes task refs published strictly before the cutoff
// (equality stays; offsets normalize to UTC; forged commit dates are
// never age), collects exactly the objects no surviving ref reaches, and
// keeps branch and younger task ancestry.
func TestPruneRetention(t *testing.T) {
	ctx := context.Background()
	f := newPruneFixture(t)
	m := openAt(t, fastDeps(), f.root)
	before, _ := m.Show(ctx, "alpha")
	for _, h := range []plumbing.Hash{f.ta, f.o, f.tcP} {
		if !hasObject(t, m, "alpha", h) {
			t.Fatalf("fixture lacks %s", h)
		}
	}
	r, err := m.Prune(ctx, "alpha", f.inst, f.cutoff)
	if err != nil {
		t.Fatal(err)
	}
	after, _ := m.Show(ctx, "alpha")
	if r.RemovedTaskRefs != 1 || r.SizeBytes != after.SizeBytes || r.ReclaimedBytes != before.SizeBytes-after.SizeBytes || r.ReclaimedBytes <= 0 || r.Generation != after.Generation {
		t.Fatalf("prune %+v before %d after %d", r, before.SizeBytes, after.SizeBytes)
	}
	refs := refsOf(t, m, "alpha")
	if _, ok := refs[contract.TaskRefPrefix+taskID('a')]; ok || refs[contract.TaskRefPrefix+taskID('b')] != f.tb.String() ||
		refs[contract.TaskRefPrefix+taskID('c')] != f.tc.String() || refs["refs/heads/main"] != f.m1.String() {
		t.Fatalf("refs %v", refs)
	}
	for h, want := range map[plumbing.Hash]bool{f.ta: false, f.o: false, f.m0: true, f.m1: true, f.tb: true, f.tc: true, f.tcP: true} {
		if hasObject(t, m, "alpha", h) != want {
			t.Fatalf("object %s present=%v", h, !want)
		}
	}
	st, _ := m.Status(ctx, "alpha", StatusQuery{Limit: 100})
	for _, row := range st.Refs {
		if strings.HasPrefix(row.Name, contract.TaskRefPrefix) && (row.PublishedAt == nil || !strings.HasSuffix(*row.PublishedAt, "Z")) {
			t.Fatalf("task row %+v", row)
		}
	}
	// A second prune with nothing to do still collects (fresh generation)
	// and a zero-candidate result is a success.
	r2, err := m.Prune(ctx, "alpha", f.inst, f.cutoff)
	if err != nil || r2.RemovedTaskRefs != 0 || r2.Generation == r.Generation {
		t.Fatalf("repeat %+v %v", r2, err)
	}
	m.Close()
	m2 := openAt(t, fastDeps(), f.root)
	if got := refsOf(t, m2, "alpha"); len(got) != 3 {
		t.Fatalf("restart refs %v", got)
	}
}

// UT-3: an empty workspace prunes (collection of nothing), and deleting a
// branch lets prune collect its exclusive history.
func TestPruneEmptyAndDeleted(t *testing.T) {
	ctx := context.Background()
	m, _ := newManager(t)
	v := mustCreate(t, m, "alpha")
	if r, err := m.Prune(ctx, "alpha", v.Instance, time.Now()); err != nil || r.RemovedTaskRefs != 0 {
		t.Fatalf("empty %+v %v", r, err)
	}
	g := serveGit(t, m)
	s := testkit.NewMemoryStore()
	c0 := commit(t, s, seedFiles(), "c0")
	push(t, g.remote("alpha", v.Instance), s, "refs/heads/side", plumbing.ZeroHash, c0)
	if _, err := setRef(m, "alpha", v.Instance, "side", c0.String(), nil, true); err != nil {
		t.Fatal(err)
	}
	if !hasObject(t, m, "alpha", c0) {
		t.Fatal("delete collected objects before prune")
	}
	if _, err := m.Prune(ctx, "alpha", v.Instance, time.Now()); err != nil {
		t.Fatal(err)
	}
	if hasObject(t, m, "alpha", c0) {
		t.Fatal("prune kept an unreachable commit")
	}
	if st, _ := m.Status(ctx, "alpha", StatusQuery{Limit: 10}); len(st.Refs) != 0 {
		t.Fatalf("refs %+v", st.Refs)
	}
}

// UT-3: missing or mismatched task metadata is corruption, never a guess.
func TestPruneCorruptMetadata(t *testing.T) {
	ctx := context.Background()
	for _, c := range []struct {
		name   string
		break_ func(gen string)
	}{
		{"missing entry", func(gen string) {
			os.Remove(filepath.Join(gen, refsName))
			fastDeps().writeFile(filepath.Join(gen, refsName), encodeRefsDoc(nil))
		}},
		{"mismatched commit", func(gen string) {
			tasks, _ := readRefsDoc(filepath.Join(gen, refsName))
			for k, v := range tasks {
				v.Commit = strings.Repeat("b", 40)
				tasks[k] = v
			}
			os.Remove(filepath.Join(gen, refsName))
			fastDeps().writeFile(filepath.Join(gen, refsName), encodeRefsDoc(tasks))
		}},
		{"invalid time", func(gen string) {
			b, _ := os.ReadFile(filepath.Join(gen, refsName))
			os.WriteFile(filepath.Join(gen, refsName), []byte(strings.Replace(string(b), "Z\"", "+00:00\"", 1)), 0o600)
		}},
	} {
		t.Run(c.name, func(t *testing.T) {
			f := newPruneFixture(t)
			m := openAt(t, fastDeps(), f.root)
			h, _ := m.lookup("alpha")
			c.break_(h.genDir(h.gen))
			if _, err := m.Prune(ctx, "alpha", f.inst, f.cutoff); err == nil {
				t.Fatal("prune guessed an age")
			}
			m.Close()
			if _, err := fastDeps().open(ctx, f.root, true); codeOf(err) != contract.CodeConflict {
				t.Fatalf("startup accepted corrupt metadata: %v", err)
			}
		})
	}
}

// UT-3: a cleanup failure after the commit fences the workspace and is an
// ambiguous error; the committed result is preserved.
func TestPruneCleanupFailure(t *testing.T) {
	ctx := context.Background()
	f := newPruneFixture(t)
	d := fastDeps()
	m := openAt(t, d, f.root)
	h, _ := m.lookup("alpha")
	old := h.genDir(h.gen)
	d.fail = func(op, p string) error {
		if op == "remove" && strings.HasPrefix(p, old) {
			return os.ErrPermission
		}
		return nil
	}
	_, err := m.Prune(ctx, "alpha", f.inst, f.cutoff)
	d.fail = nil
	if codeOf(err) != contract.CodeInternal || !strings.Contains(err.Error(), "ambiguous") {
		t.Fatalf("cleanup failure: %v", err)
	}
	if _, err := setRef(m, "alpha", f.inst, "x", contract.ExpectedAbsent, strp("main"), false); err == nil || !strings.Contains(err.Error(), "fenced") {
		t.Fatalf("mutation after an ambiguous outcome: %v", err)
	}
	if v, err := m.Show(ctx, "alpha"); err != nil || v.SizeBytes <= 0 {
		t.Fatalf("show while fenced: %v", err)
	}
	if _, err := m.Status(ctx, "alpha", StatusQuery{Limit: 10}); err != nil {
		t.Fatalf("status while fenced: %v", err)
	}
	m.Close()
	m2 := openAt(t, fastDeps(), f.root)
	if refs := refsOf(t, m2, "alpha"); len(refs) != 3 {
		t.Fatalf("committed prune lost: %v", refs)
	}
}
