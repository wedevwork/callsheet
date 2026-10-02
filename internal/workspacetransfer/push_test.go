package workspacetransfer

import (
	"bytes"
	"context"
	"crypto/sha1"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/go-git/go-git/v5/plumbing/revlist"
	"github.com/go-git/go-git/v5/plumbing/storer"

	"github.com/wedevwork/callsheet/internal/contract"
	"github.com/wedevwork/callsheet/internal/testkit"
)

// UT-1: exact committed pushes, fast-forward-only compare-and-swap and
// the same-tip zero-object CAS, proven against the production 09a
// receive handler over verified TLS.

// spyPlane records the packs sent through a real plane.
type spyPlane struct {
	Plane
	mu    sync.Mutex
	packs [][]byte
}

type spyGit struct {
	Git
	p *spyPlane
}

func (s *spyPlane) WorkspaceGit(name string) (Git, error) {
	g, err := s.Plane.WorkspaceGit(name)
	if err != nil {
		return nil, err
	}
	return &spyGit{Git: g, p: s}, nil
}

func (g *spyGit) Push(ctx context.Context, instance, ref string, old, new plumbing.Hash, pack io.Reader) error {
	b, err := io.ReadAll(pack)
	if err != nil {
		return err
	}
	g.p.mu.Lock()
	g.p.packs = append(g.p.packs, b)
	g.p.mu.Unlock()
	return g.Git.Push(ctx, instance, ref, old, new, bytes.NewReader(b))
}

func (s *spyPlane) last() []byte {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.packs[len(s.packs)-1]
}

// pushWith pushes path to name/branch with deps d.
func pushWith(t testing.TB, d *deps, p Plane, env Env, name, instance, branch, path string) (contract.WorkspacePushResult, error) {
	t.Helper()
	return d.push(context.Background(), Options{GOOS: hostGOOS(), Env: env, Cwd: "/", Plane: p},
		PushRequest{Name: name, Instance: instance, Branch: branch, Path: path, PathSet: true})
}

// fetched fetches commit's closure from the plane into a memory store.
func fetched(t *testing.T, tp *testPlane, name string, commit plumbing.Hash) *object.Commit {
	t.Helper()
	s := testkit.NewMemoryStore()
	if err := tp.remote(t, name, "").Fetch(context.Background(), s, []plumbing.Hash{commit}, nil); err != nil {
		t.Fatal(err)
	}
	c, err := object.GetCommit(s, commit)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestPushGitExact(t *testing.T) {
	// A deterministic protocol proof: under a repeated run it runs once
	// per CPU setting (sampler).
	if !sampler(t)(0) {
		return
	}
	tp := newTestPlane(t)
	v := tp.create(t, "ws")
	env := emptyEnv(t)
	spy := &spyPlane{Plane: tp.plane(t)}
	r := newRepo(t, map[string]fspec{"a.txt": reg("one\n"), "old.log": reg("tracked, later ignored\n")})
	first := r.head
	second := r.commit(map[string]fspec{"a.txt": reg("two\n"), "old.log": reg("tracked, later ignored\n"), ".gitignore": reg("*.log\n")}, "second")
	res, err := pushWith(t, fastDeps(), spy, env, "ws", v.Instance, "", r.root)
	if err != nil {
		t.Fatal(err)
	}
	if res.Commit != second.String() || res.OldCommit != nil || !res.Changed || res.SourceKind != contract.KindGit || res.Branch != contract.DefaultBranchRef || res.Validate() != nil {
		t.Fatalf("result %+v", res)
	}
	if tp.refs(t, "ws")[contract.DefaultBranchRef] != second.String() {
		t.Fatal("main is not HEAD")
	}
	c := fetched(t, tp, "ws", second)
	if len(c.ParentHashes) != 1 || c.ParentHashes[0] != first || c.Message != "second" {
		t.Fatalf("history rewritten: %+v", c)
	}
	// Same tip: a zero-object pack (PACK, count 0, trailer) through the
	// production handler with the same old/new CAS; changed=false.
	res2, err := pushWith(t, fastDeps(), spy, env, "ws", v.Instance, "", r.root)
	if err != nil || res2.Changed || res2.OldCommit == nil || *res2.OldCommit != second.String() || res2.Commit != second.String() {
		t.Fatalf("same tip %+v %v", res2, err)
	}
	pack := spy.last()
	sum := sha1.Sum(pack[:12])
	if len(pack) != 32 || string(pack[:12]) != "PACK\x00\x00\x00\x02\x00\x00\x00\x00" || !bytes.Equal(pack[12:], sum[:]) {
		t.Fatalf("zero-object pack %x", pack)
	}
	if tp.refs(t, "ws")[contract.DefaultBranchRef] != second.String() {
		t.Fatal("same-tip CAS moved main")
	}
	// A stale old hash with a zero-object pack is rejected by the handler.
	g, _ := tp.client(t).WorkspaceGit("ws")
	defer g.Close()
	err = g.Push(context.Background(), v.Instance, contract.DefaultBranchRef, first, second, bytes.NewReader(pack))
	if contract.CodeOf(err) != contract.CodeConflict {
		t.Fatalf("stale zero-object CAS: %v", err)
	}
	// A named branch; other branches never move.
	res3, err := pushWith(t, fastDeps(), tp.plane(t), env, "ws", v.Instance, "refs/heads/topic/x", r.root)
	if err != nil || res3.Branch != "refs/heads/topic/x" {
		t.Fatalf("named branch %+v %v", res3, err)
	}
	refs := tp.refs(t, "ws")
	if refs[contract.DefaultBranchRef] != second.String() || refs["refs/heads/topic/x"] != second.String() || len(refs) != 2 {
		t.Fatalf("refs %v", refs)
	}
	// An incremental push sends only new objects.
	third := r.commit(map[string]fspec{"a.txt": reg("three\n"), "old.log": reg("tracked, later ignored\n"), ".gitignore": reg("*.log\n")}, "third")
	if _, err := pushWith(t, fastDeps(), spy, env, "ws", v.Instance, "", r.root); err != nil {
		t.Fatal(err)
	}
	if n := int(spy.last()[11]); n != 3 { // commit, tree, one blob
		t.Fatalf("incremental pack has %d objects", n)
	}
	if tp.refs(t, "ws")[contract.DefaultBranchRef] != third.String() {
		t.Fatal("fast-forward")
	}
}

func TestPushFastForwardOnly(t *testing.T) {
	mp := newMemPlane()
	inst := mp.create("ws")
	env := emptyEnv(t)
	a := newRepo(t, map[string]fspec{"f": reg("a")})
	if _, err := pushWith(t, fastDeps(), mp, env, "ws", inst, "", a.root); err != nil {
		t.Fatal(err)
	}
	// A divergent history (unknown target) is refused with guidance.
	b := newRepo(t, map[string]fspec{"f": reg("b")})
	_, err := pushWith(t, fastDeps(), mp, env, "ws", inst, "", b.root)
	if contract.TransferReason(err) != contract.ReasonNonFastForward || !strings.Contains(err.Error(), "ws ref set") {
		t.Fatalf("divergent: %v", err)
	}
	// A local repository whose HEAD is behind (the target is a
	// descendant) is not a fast-forward either.
	first := a.head
	a.commit(map[string]fspec{"f": reg("a2")}, "a2")
	if _, err := pushWith(t, fastDeps(), mp, env, "ws", inst, "", a.root); err != nil {
		t.Fatal(err)
	}
	mustWrite(t, filepath.Join(a.root, ".git/refs/heads/main"), first.String()+"\n")
	a.writeFile("f", reg("a"))
	a.writeIndex(map[string]fspec{"f": reg("a")})
	if _, err := pushWith(t, fastDeps(), mp, env, "ws", inst, "", a.root); contract.TransferReason(err) != contract.ReasonNonFastForward {
		t.Fatalf("behind: %v", err)
	}
	// A merge with the target as its second parent is a fast-forward (all
	// parents are traversed).
	c := newRepo(t, map[string]fspec{"f": reg("c")})
	tip := mp.refs("ws")[contract.DefaultBranchRef]
	if err := copyClosure(mp.ws["ws"].store, c.store, tip); err != nil {
		t.Fatal(err)
	}
	tree := c.treeOf(map[string]fspec{"f": reg("merged")})
	merge, _ := testkit.WriteCommit(c.store, tree, []plumbing.Hash{c.head, tip}, "merge", testkit.FixedWhen)
	c.head = merge
	c.files = map[string]fspec{"f": reg("merged")}
	c.writeFile("f", reg("merged"))
	c.writeIndex(c.files)
	mustWrite(t, filepath.Join(c.root, ".git/refs/heads/main"), merge.String()+"\n")
	res, err := pushWith(t, fastDeps(), mp, env, "ws", inst, "", c.root)
	if err != nil || res.Commit != merge.String() || *res.OldCommit != tip.String() {
		t.Fatalf("merge fast-forward %+v %v", res, err)
	}
	if refs := mp.refs("ws"); refs[contract.DefaultBranchRef] != merge || len(refs) != 1 {
		t.Fatalf("refs %v", refs)
	}
	// A stale instance conflicts; a missing workspace is not found.
	if _, err := pushWith(t, fastDeps(), mp, env, "ws", strings.Repeat("0", 32), "", c.root); contract.CodeOf(err) != contract.CodeConflict {
		t.Fatalf("stale instance: %v", err)
	}
	if _, err := pushWith(t, fastDeps(), mp, env, "nope", inst, "", c.root); contract.CodeOf(err) != contract.CodeNotFound {
		t.Fatalf("missing workspace: %v", err)
	}
}

// copyClosure copies commit's closure between stores.
func copyClosure(from, to storer.EncodedObjectStorer, commit plumbing.Hash) error {
	objs, err := revlist.Objects(from, []plumbing.Hash{commit}, nil)
	if err != nil {
		return err
	}
	for _, h := range objs {
		o, err := from.EncodedObject(plumbing.AnyObject, h)
		if err != nil {
			return err
		}
		if _, err := to.SetEncodedObject(o); err != nil {
			return err
		}
	}
	return nil
}

// The observed tip is the CAS value: a branch moved after observation
// makes the push conflict (never refreshed or retried), and of two
// concurrent writers observing the same absent branch exactly one wins.
// casRuns names each repetition's fresh workspace on the shared plane.
var casRuns atomic.Int64

func TestPushCompareAndSwap(t *testing.T) {
	env := emptyEnv(t)
	tp := sharedPlane(t)
	n := casRuns.Add(1)
	// Two writers, one absent branch: exactly one wins (every repetition,
	// on a fresh tiny workspace).
	race := fmt.Sprintf("race%d", n)
	rv := tp.create(t, race)
	w1 := newRepo(t, map[string]fspec{"f": reg("w1")})
	w2 := newRepo(t, map[string]fspec{"f": reg("w2")})
	var mu sync.Mutex
	observed := 0
	both := make(chan struct{})
	barrier := func(s string) {
		if s != "push-observed" {
			return
		}
		mu.Lock()
		observed++
		if observed == 2 {
			close(both)
		}
		mu.Unlock()
		<-both
	}
	d1, d2 := fastDeps(), fastDeps()
	d1.hook, d2.hook = barrier, barrier
	var wg sync.WaitGroup
	errs := make([]error, 2)
	for i, w := range []struct {
		d *deps
		r *fixtureRepo
	}{{d1, w1}, {d2, w2}} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, errs[i] = pushWith(t, w.d, tp.plane(t), env, race, rv.Instance, "race", w.r.root)
		}()
	}
	wg.Wait()
	wins := 0
	for _, e := range errs {
		switch {
		case e == nil:
			wins++
		case contract.CodeOf(e) != contract.CodeConflict:
			t.Fatalf("loser error %v", e)
		}
	}
	got := tp.refs(t, race)["refs/heads/race"]
	if wins != 1 || (got != w1.head.String() && got != w2.head.String()) {
		t.Fatalf("wins %d, race %s, errs %v", wins, got, errs)
	}
	// A branch moved after observation (a deterministic hook ordering,
	// once per CPU setting under a repeated run).
	if !sampler(t)(0) {
		return
	}
	name := fmt.Sprintf("moved%d", n)
	v := tp.create(t, name)
	a := newRepo(t, map[string]fspec{"f": reg("a")})
	if _, err := pushWith(t, fastDeps(), tp.plane(t), env, name, v.Instance, "", a.root); err != nil {
		t.Fatal(err)
	}
	a.commit(map[string]fspec{"f": reg("a2")}, "a2")
	other := newRepo(t, map[string]fspec{"g": reg("other")})
	if _, err := pushWith(t, fastDeps(), tp.plane(t), env, name, v.Instance, "refs/heads/other", other.root); err != nil {
		t.Fatal(err)
	}
	d := fastDeps()
	d.hook = func(s string) {
		if s == "push-packed" {
			if _, err := tp.m().SetRef(context.Background(), name, contract.RefSetIntent{Instance: v.Instance, Ref: contract.DefaultBranchRef,
				Expected: tp.refs(t, name)[contract.DefaultBranchRef], Target: contract.Selector{Kind: contract.SelectorKindHash, Value: other.head.String()}}); err != nil {
				t.Error(err)
			}
		}
	}
	_, err := pushWith(t, d, tp.plane(t), env, name, v.Instance, "", a.root)
	if contract.CodeOf(err) != contract.CodeConflict || !strings.Contains(err.Error(), "compare-and-swap") {
		t.Fatalf("moved branch: %v", err)
	}
	if tp.refs(t, name)[contract.DefaultBranchRef] != other.head.String() {
		t.Fatal("the other writer's update was replaced")
	}
}

// A source changed after the scan is refused before publication; a
// cancellation stops the push and removes the private temporary store;
// the hub is unchanged in both cases.
func TestPushSourceChangeAndCancel(t *testing.T) {
	mp := newMemPlane()
	inst := mp.create("ws")
	env := emptyEnv(t)
	tmp := tempDir(t)
	r := newRepo(t, map[string]fspec{"f": reg("a")})
	d := fastDeps()
	d.tempDir = tmp
	d.hook = func(s string) {
		if s == "push-packed" {
			r.writeFile("f", reg("edited during the push"))
		}
	}
	_, err := pushWith(t, d, mp, env, "ws", inst, "", r.root)
	if contract.TransferReason(err) != contract.ReasonSourceChanged {
		t.Fatalf("source change: %v", err)
	}
	if len(mp.refs("ws")) != 0 {
		t.Fatal("the hub changed")
	}
	r.writeFile("f", reg("a"))
	ctx, cancel := context.WithCancel(context.Background())
	d2 := fastDeps()
	d2.tempDir = tmp
	d2.hook = func(s string) {
		if s == "push-observed" {
			cancel()
		}
	}
	_, err = d2.push(ctx, Options{GOOS: hostGOOS(), Env: env, Cwd: "/", Plane: mp}, PushRequest{Name: "ws", Instance: inst, Path: r.root, PathSet: true})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled: %v", err)
	}
	if len(mp.refs("ws")) != 0 {
		t.Fatal("the hub changed")
	}
	if es, _ := os.ReadDir(tmp); len(es) != 0 {
		t.Fatalf("temporary stores left: %v", es)
	}
	// A pack staging failure is a storage failure before publication.
	d3 := fastDeps()
	d3.fail = func(op string) error {
		if op == "push-pack" {
			return errors.New("injected")
		}
		return nil
	}
	if _, err := pushWith(t, d3, mp, env, "ws", inst, "", r.root); contract.TransferReason(err) != contract.ReasonStorageFailure {
		t.Fatalf("pack failure: %v", err)
	}
}

// Plain folders: the first push creates a root snapshot, an unchanged
// folder is a same-value no-op, a change creates one child snapshot of
// the observed tip; ambient configuration never changes a snapshot.
func TestPushFolder(t *testing.T) {
	mp := newMemPlane()
	inst := mp.create("ws")
	folder := writeFolder(t, map[string]fspec{"a": reg("a"), "x/b": exe("b"), ".gitignore": reg("*.tmp\n")}, nil)
	os.WriteFile(filepath.Join(folder, "junk.tmp"), []byte("j"), 0o644)
	env := emptyEnv(t)
	res, err := pushWith(t, fastDeps(), mp, env, "ws", inst, "", folder)
	if err != nil || res.SourceKind != contract.KindFolder || res.OldCommit != nil || !res.Changed {
		t.Fatalf("seed %+v %v", res, err)
	}
	c := mp.commit("ws", plumbing.NewHash(res.Commit))
	if len(c.ParentHashes) != 0 || c.Author.Name != "Callsheet" || c.Author.Email != "workspace@callsheet.invalid" || c.Author.When.Unix() != 0 ||
		c.Message != "Callsheet workspace snapshot\n" {
		t.Fatalf("snapshot commit %+v", c)
	}
	home := tempDir(t)
	mustWrite(t, filepath.Join(home, ".gitconfig"), "[core]\n\texcludesFile = ~/ig\n\tautocrlf = true\n")
	mustWrite(t, filepath.Join(home, "ig"), "a\n")
	env2 := envOf(map[string]string{"HOME": home})
	res2, err := pushWith(t, fastDeps(), mp, env2, "ws", inst, "", folder)
	if err != nil || res2.Changed || res2.Commit != res.Commit {
		t.Fatalf("unchanged folder with other ambient configuration %+v %v", res2, err)
	}
	os.WriteFile(filepath.Join(folder, "a"), []byte("a2"), 0o644)
	res3, err := pushWith(t, fastDeps(), mp, env, "ws", inst, "", folder)
	if err != nil || !res3.Changed || *res3.OldCommit != res.Commit {
		t.Fatalf("change %+v %v", res3, err)
	}
	c3 := mp.commit("ws", plumbing.NewHash(res3.Commit))
	if len(c3.ParentHashes) != 1 || c3.ParentHashes[0].String() != res.Commit {
		t.Fatalf("parent %v", c3.ParentHashes)
	}
	// Removing a file removes it from the snapshot (not a merge).
	os.Remove(filepath.Join(folder, "x/b"))
	res4, err := pushWith(t, fastDeps(), mp, env, "ws", inst, "", folder)
	if err != nil {
		t.Fatal(err)
	}
	c4 := mp.commit("ws", plumbing.NewHash(res4.Commit))
	tree, _ := c4.Tree()
	if _, err := tree.File("x/b"); err == nil {
		t.Fatal("deleted file kept")
	}
	// An empty folder into a new branch is the empty-tree root commit;
	// the same folder and parent always give the same commit.
	empty := filepath.Join(tempDir(t), "e")
	os.Mkdir(empty, 0o755)
	r5, err := pushWith(t, fastDeps(), mp, env, "ws", inst, "empty", empty)
	if err != nil {
		t.Fatal(err)
	}
	if c5 := mp.commit("ws", plumbing.NewHash(r5.Commit)); c5.TreeHash.String() != "4b825dc642cb6eb9a060e54bf8d69288fbee4904" || len(c5.ParentHashes) != 0 {
		t.Fatalf("empty snapshot %+v", c5)
	}
	if r5.Commit != snapshotCommitHash(t, plumbing.NewHash("4b825dc642cb6eb9a060e54bf8d69288fbee4904"), plumbing.ZeroHash).String() {
		t.Fatal("empty snapshot identity")
	}
	// A failure while scanning leaves the hub unchanged.
	before := mp.refs("ws")
	os.MkdirAll(filepath.Join(folder, "nested/.git"), 0o755)
	if _, err := pushWith(t, fastDeps(), mp, env, "ws", inst, "", folder); contract.TransferReason(err) != contract.ReasonUnsupportedRepository {
		t.Fatalf("nested repository: %v", err)
	}
	if after := mp.refs("ws"); len(after) != len(before) || after[contract.DefaultBranchRef] != before[contract.DefaultBranchRef] {
		t.Fatal("the hub changed")
	}
}

func snapshotCommitHash(t testing.TB, tree, parent plumbing.Hash) plumbing.Hash {
	t.Helper()
	o := &plumbing.MemoryObject{}
	if err := snapshotCommit(tree, parent).Encode(o); err != nil {
		t.Fatal(err)
	}
	return o.Hash()
}

// Argument and path errors are refused before anything local or remote
// happens.
func TestPushArguments(t *testing.T) {
	p := &localPlane{t: t}
	for _, c := range []struct {
		o   Options
		req PushRequest
	}{
		{Options{GOOS: "windows", Plane: p}, PushRequest{Name: "ws", Instance: tInst}},
		{Options{GOOS: "linux", Plane: p}, PushRequest{Name: "Ws", Instance: tInst}},
		{Options{GOOS: "linux", Plane: p}, PushRequest{Name: "ws", Instance: tInst, Branch: "Main"}},
		{Options{GOOS: "linux", Plane: p, Cwd: "/"}, PushRequest{Name: "ws", Instance: tInst, PathSet: true}},
		{Options{GOOS: "linux", Plane: p, Cwd: "relative"}, PushRequest{Name: "ws", Instance: tInst, Path: "x", PathSet: true}},
		{Options{GOOS: "darwin", Plane: p, Cwd: "/" + strings.Repeat("a", 1000)}, PushRequest{Name: "ws", Instance: tInst, Path: strings.Repeat("b", 100), PathSet: true}},
	} {
		if _, err := fastDeps().push(context.Background(), c.o, c.req); contract.CodeOf(err) != contract.CodeInvalidArgument {
			t.Fatalf("%+v: %v", c.req, err)
		}
	}
	if p, err := ResolvePath("linux", "/w", "", false); err != nil || p != "/w" {
		t.Fatalf("default path %q %v", p, err)
	}
	if p, err := ResolvePath("linux", "/w", "../x/./y", true); err != nil || p != "/x/y" {
		t.Fatalf("relative path %q %v", p, err)
	}
}
