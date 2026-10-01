package workspacetransfer

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/go-git/go-git/v5/plumbing"

	"github.com/wedevwork/callsheet/internal/contract"
)

// UT-5: pull into a git repository: objects plus exactly one Callsheet
// ref under local compare-and-swap; checkout, index, HEAD, config,
// FETCH_HEAD and every other ref untouched.

// repoFingerprint fingerprints everything a pull must not change: the
// working tree, index, HEAD, config, FETCH_HEAD and refs other than
// refs/callsheet (objects may be added).
func repoFingerprint(t *testing.T, root string) string {
	t.Helper()
	var out []string
	for _, l := range strings.Split(fingerprint(t, root), "\n") {
		if strings.HasPrefix(l, ".git/objects") || strings.HasPrefix(l, ".git/refs/callsheet") {
			continue
		}
		out = append(out, l)
	}
	return strings.Join(out, "\n")
}

func readRef(t *testing.T, root, ref string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(root, ".git", filepath.FromSlash(ref)))
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}

func pullWith(t testing.TB, d *deps, p Plane, env Env, ref, path string) (contract.WorkspacePullResult, error) {
	t.Helper()
	return d.pull(context.Background(), Options{GOOS: hostGOOS(), Env: env, Cwd: "/", Plane: p},
		PullRequest{Name: "ws", Ref: ref, Path: path, PathSet: true})
}

// Every selector form through the production plane (status pagination,
// the diff reachability probe and a planted task ref) into a dirty
// existing repository whose checkout and other state stay byte-identical.
func TestPullGitSelectors(t *testing.T) {
	// Deterministic: once per CPU setting under a repeated run.
	if !sampler(t)(0) {
		return
	}
	tp := sharedPlane(t)
	p := tp.plane(t)
	env := emptyEnv(t)
	dst := newRepo(t, map[string]fspec{"mine": reg("local work")})
	dst.writeFile("mine", reg("uncommitted edit"))
	dst.writeFile("untracked", reg("u"))
	mustWrite(t, filepath.Join(dst.root, ".git/FETCH_HEAD"), "keep\n")
	before := repoFingerprint(t, dst.root)
	task := contract.TaskRefPrefix + shared.task
	for _, c := range []struct {
		ref, selector, local string
		commit               plumbing.Hash
	}{
		{"main", "refs/heads/main", "refs/callsheet/ws/heads/main", shared.second},
		{"refs/heads/topic", "refs/heads/topic", "refs/callsheet/ws/heads/topic", shared.topic},
		{shared.first.String(), shared.first.String(), "refs/callsheet/ws/commits/" + shared.first.String(), shared.first},
		{task, task, "refs/callsheet/ws/tasks/" + shared.task, shared.first},
	} {
		res, err := pullWith(t, fastDeps(), p, env, c.ref, dst.root)
		if err != nil {
			t.Fatalf("%s: %v", c.ref, err)
		}
		if res.Selector != c.selector || res.Commit != c.commit.String() || res.DestinationKind != contract.KindGit || *res.LocalRef != c.local ||
			res.OldCommit != nil || !res.Changed || res.Instance != shared.instance || res.Validate() != nil {
			t.Fatalf("%s: %+v", c.ref, res)
		}
		if readRef(t, dst.root, c.local) != c.commit.String() {
			t.Fatalf("%s: local ref", c.ref)
		}
		if _, err := walkClosure(context.Background(), dst.store, []plumbing.Hash{c.commit}, nil, true); err != nil {
			t.Fatalf("%s: closure %v", c.ref, err)
		}
	}
	if after := repoFingerprint(t, dst.root); after != before {
		t.Fatalf("the checkout, index, HEAD, config, FETCH_HEAD or other refs changed:\n%s\n---\n%s", before, after)
	}
	// Repeating is a verified same-value observation.
	res, err := pullWith(t, fastDeps(), p, env, "main", dst.root)
	if err != nil || res.Changed || *res.OldCommit != shared.second.String() {
		t.Fatalf("repeat %+v %v", res, err)
	}
	// Missing selectors, missing workspace.
	for _, ref := range []string{"nope", "refs/callsheet/tasks/t_" + strings.Repeat("d", 32), strings.Repeat("e", 40)} {
		if _, err := pullWith(t, fastDeps(), p, env, ref, dst.root); contract.CodeOf(err) != contract.CodeNotFound {
			t.Fatalf("%s: %v", ref, err)
		}
	}
	if _, err := fastDeps().pull(context.Background(), Options{GOOS: hostGOOS(), Env: env, Cwd: "/", Plane: p},
		PullRequest{Name: "nope", Ref: "main", Path: dst.root, PathSet: true}); contract.CodeOf(err) != contract.CodeNotFound {
		t.Fatalf("missing workspace: %v", err)
	}
}

// memSeed is a memPlane with workspace "ws": main (first, second) and an
// unrelated topic.
type memSeed struct {
	p                    *memPlane
	first, second, topic plumbing.Hash
	env                  Env
}

func newMemSeed(t *testing.T) *memSeed {
	t.Helper()
	s := &memSeed{p: newMemPlane(), env: emptyEnv(t)}
	s.p.create("ws")
	s.first = s.p.commitFiles("ws", "refs/heads/main", map[string]fspec{"a.txt": reg("one\n"), "l": link("a.txt")}, plumbing.ZeroHash)
	s.second = s.p.commitFiles("ws", "refs/heads/main", map[string]fspec{"a.txt": reg("two\n"), "l": link("a.txt"), "d/e": exe("e")}, s.first)
	s.topic = s.p.commitFiles("ws", "refs/heads/topic", map[string]fspec{"t": reg("topic")}, plumbing.ZeroHash)
	return s
}

func (s *memSeed) pull(t *testing.T, d *deps, ref, path string) (contract.WorkspacePullResult, error) {
	t.Helper()
	return pullWith(t, d, s.p, s.env, ref, path)
}

// Branch observation refs move in either direction (unrelated histories
// included); a hash ref holding another commit is never repaired; status
// pagination continues across pages.
func TestPullGitMoves(t *testing.T) {
	s := newMemSeed(t)
	s.p.setRef("ws", "refs/heads/keep", s.second)
	dst := newRepo(t, map[string]fspec{"x": reg("x")})
	if _, err := s.pull(t, fastDeps(), "main", dst.root); err != nil {
		t.Fatal(err)
	}
	s.p.setRef("ws", "refs/heads/main", s.first)
	res, err := s.pull(t, fastDeps(), "main", dst.root)
	if err != nil || !res.Changed || res.Commit != s.first.String() || *res.OldCommit != s.second.String() {
		t.Fatalf("backwards %+v %v", res, err)
	}
	s.p.setRef("ws", "refs/heads/main", s.topic)
	if res, err := s.pull(t, fastDeps(), "main", dst.root); err != nil || res.Commit != s.topic.String() {
		t.Fatalf("unrelated history %+v %v", res, err)
	}
	mustWrite(t, filepath.Join(dst.root, ".git/refs/callsheet/ws/commits", s.first.String()), s.second.String()+"\n")
	if _, err := s.pull(t, fastDeps(), s.first.String(), dst.root); contract.TransferReason(err) != contract.ReasonLocalRefConflict {
		t.Fatalf("corrupt hash ref: %v", err)
	}
	for i := 0; i < 120; i++ {
		s.p.setRef("ws", "refs/heads/b"+strconv.Itoa(1000+i), s.first)
	}
	if res, err := s.pull(t, fastDeps(), "topic", dst.root); err != nil || res.Commit != s.topic.String() {
		t.Fatalf("second page %+v %v", res, err)
	}
	if _, err := s.pull(t, fastDeps(), "zzz", dst.root); contract.CodeOf(err) != contract.CodeNotFound {
		t.Fatalf("missing after pages: %v", err)
	}
}

// Present objects are reused: no fetch when the closure is local.
func TestPullGitReuse(t *testing.T) {
	s := newMemSeed(t)
	dst := newRepo(t, map[string]fspec{"x": reg("x")})
	if _, err := s.pull(t, fastDeps(), "main", dst.root); err != nil {
		t.Fatal(err)
	}
	if _, err := s.pull(t, fastDeps(), s.first.String(), dst.root); err != nil {
		t.Fatal(err)
	}
	if s.p.fetches != 1 {
		t.Fatalf("fetches %d", s.p.fetches)
	}
}

func TestPullGitRefSafety(t *testing.T) {
	s := newMemSeed(t)
	ref := "refs/callsheet/ws/heads/main"
	cases := []struct {
		what  string
		setup func(r *fixtureRepo)
	}{
		{"symbolic target", func(r *fixtureRepo) { mustWrite(t, filepath.Join(r.root, ".git", ref), "ref: refs/heads/main\n") }},
		{"HEAD points at it", func(r *fixtureRepo) { mustWrite(t, filepath.Join(r.root, ".git/HEAD"), "ref: "+ref+"\n") }},
		{"a branch points at it", func(r *fixtureRepo) { mustWrite(t, filepath.Join(r.root, ".git/refs/heads/alias"), "ref: "+ref+"\n") }},
		{"a chain points at it", func(r *fixtureRepo) {
			mustWrite(t, filepath.Join(r.root, ".git/refs/heads/a1"), "ref: refs/heads/a2\n")
			mustWrite(t, filepath.Join(r.root, ".git/refs/heads/a2"), "ref: "+ref+"\n")
		}},
		{"prefix file", func(r *fixtureRepo) { mustWrite(t, filepath.Join(r.root, ".git/refs/callsheet/ws/heads"), "x\n") }},
		{"directory at the ref", func(r *fixtureRepo) { os.MkdirAll(filepath.Join(r.root, ".git", ref, "sub"), 0o755) }},
		{"case alias directory", func(r *fixtureRepo) { os.MkdirAll(filepath.Join(r.root, ".git/refs/callsheet/WS"), 0o755) }},
		{"packed prefix conflict", func(r *fixtureRepo) {
			mustWrite(t, filepath.Join(r.root, ".git/packed-refs"), "# pack-refs with: peeled\n"+s.first.String()+" refs/callsheet/ws/heads\n")
		}},
		{"packed case alias", func(r *fixtureRepo) {
			mustWrite(t, filepath.Join(r.root, ".git/packed-refs"), s.first.String()+" refs/callsheet/ws/heads/MAIN\n")
		}},
		{"symlinked refs component", func(r *fixtureRepo) {
			os.MkdirAll(filepath.Join(r.root, "elsewhere"), 0o755)
			os.MkdirAll(filepath.Join(r.root, ".git/refs/callsheet"), 0o755)
			os.Symlink(filepath.Join(r.root, "elsewhere"), filepath.Join(r.root, ".git/refs/callsheet/ws"))
		}},
		{"held ref lock", func(r *fixtureRepo) { mustWrite(t, filepath.Join(r.root, ".git", ref+".lock"), "") }},
		{"held packed-refs lock", func(r *fixtureRepo) { mustWrite(t, filepath.Join(r.root, ".git/packed-refs.lock"), "") }},
		{"garbage packed-refs", func(r *fixtureRepo) { mustWrite(t, filepath.Join(r.root, ".git/packed-refs"), "nonsense line\n") }},
	}
	run := sampler(t)
	for i, c := range cases {
		if !run(i) {
			continue
		}
		dst := newRepo(t, map[string]fspec{"x": reg("x")})
		c.setup(dst)
		before := fingerprint(t, dst.root)
		_, err := s.pull(t, fastDeps(), "main", dst.root)
		if contract.TransferReason(err) != contract.ReasonLocalRefConflict && contract.TransferReason(err) != contract.ReasonUnsupportedRepository {
			t.Fatalf("%s: %v", c.what, err)
		}
		// Nothing but possibly installed objects changed.
		stripObjects := func(s string) string {
			var out []string
			for _, l := range strings.Split(s, "\n") {
				if !strings.HasPrefix(l, ".git/objects") {
					out = append(out, l)
				}
			}
			return strings.Join(out, "\n")
		}
		if stripObjects(fingerprint(t, dst.root)) != stripObjects(before) {
			t.Fatalf("%s: changed", c.what)
		}
		if _, err := os.Lstat(filepath.Join(dst.root, "elsewhere", "heads")); err == nil {
			t.Fatalf("%s: wrote through a symlink", c.what)
		}
	}
	// A packed old value is observed and shadowed by the new loose ref;
	// packed-refs is not rewritten; no lock is left.
	if !run(len(cases)) {
		return
	}
	dst := newRepo(t, map[string]fspec{"x": reg("x")})
	packed := "# pack-refs with: peeled\n" + s.first.String() + " " + ref + "\n^" + s.first.String() + "\n"
	mustWrite(t, filepath.Join(dst.root, ".git/packed-refs"), packed)
	res, err := s.pull(t, fastDeps(), "main", dst.root)
	if err != nil || *res.OldCommit != s.first.String() || !res.Changed {
		t.Fatalf("packed old %+v %v", res, err)
	}
	if b, _ := os.ReadFile(filepath.Join(dst.root, ".git/packed-refs")); string(b) != packed || readRef(t, dst.root, ref) != s.second.String() {
		t.Fatal("packed-refs rewritten or loose ref missing")
	}
	for _, l := range []string{".git/packed-refs.lock", ".git/" + ref + ".lock"} {
		if _, err := os.Lstat(filepath.Join(dst.root, l)); err == nil {
			t.Fatalf("%s left", l)
		}
	}
	// A concurrent local writer between observation and the locked
	// re-read makes the pull conflict without writing.
	d := fastDeps()
	d.hook = func(st string) {
		if st == "ref-locked" {
			os.Remove(filepath.Join(dst.root, ".git", ref))
			mustWrite(t, filepath.Join(dst.root, ".git", ref), s.topic.String()+"\n")
		}
	}
	s.p.setRef("ws", "refs/heads/main", s.first)
	if _, err := s.pull(t, d, "main", dst.root); contract.TransferReason(err) != contract.ReasonLocalRefConflict {
		t.Fatalf("concurrent writer: %v", err)
	}
	if readRef(t, dst.root, ref) != s.topic.String() {
		t.Fatal("the other writer's value was replaced")
	}
	// Generated paths are checked against the native limit first.
	long := filepath.Join(tempDir(t), strings.Repeat("d", 200), strings.Repeat("e", 200), strings.Repeat("f", 200), strings.Repeat("g", 200))
	long = filepath.Join(long, strings.Repeat("h", 1000-len(long)))
	deep := newRepoAt(t, long, map[string]fspec{"x": reg("x")})
	if _, err := fastDeps().pull(context.Background(), Options{GOOS: "darwin", Env: s.env, Cwd: "/", Plane: s.p},
		PullRequest{Name: "ws", Ref: "main", Path: deep.root, PathSet: true}); contract.CodeOf(err) != contract.CodeInvalidArgument {
		t.Fatalf("darwin path limit: %v", err)
	}
	if checkRefPath("linux", "/r", "refs/callsheet/ws/heads/"+strings.Repeat("a", 251)) == nil {
		t.Fatal("component limit")
	}
}

func TestPullGitFailures(t *testing.T) {
	s := newMemSeed(t)
	ref := "refs/callsheet/ws/heads/main"
	run := sampler(t)
	ops := []string{"install-object-create", "install-object-write", "install-object-sync", "install-object-rename", "install-object-dirsync",
		"ref-packed-lock", "ref-mkdir", "ref-lock", "ref-write", "ref-sync", "ref-rename", "fetch-write", "temp-create"}
	for i, op := range ops {
		if !run(i) {
			continue
		}
		dst := newRepo(t, map[string]fspec{"x": reg("x")})
		before := repoFingerprint(t, dst.root)
		d := fastDeps()
		d.fail = func(o string) error {
			if o == op {
				return errors.New("injected")
			}
			return nil
		}
		_, err := s.pull(t, d, "main", dst.root)
		if contract.CodeOf(err) != contract.CodeInternal || contract.TransferReason(err) != contract.ReasonStorageFailure {
			t.Fatalf("%s: %v", op, err)
		}
		if after := repoFingerprint(t, dst.root); readRef(t, dst.root, ref) != "" || after != before {
			t.Fatalf("%s: published or changed", op)
		}
		if _, err := os.Lstat(filepath.Join(dst.root, ".git", ref+".lock")); err == nil {
			t.Fatalf("%s: lock left", op)
		}
	}
	// A failure after the rename is ambiguous: the new ref may be there.
	if run(len(ops)) {
		dst := newRepo(t, map[string]fspec{"x": reg("x")})
		d := fastDeps()
		d.fail = func(o string) error {
			if o == "ref-dirsync" {
				return errors.New("injected")
			}
			return nil
		}
		_, err := s.pull(t, d, "main", dst.root)
		if contract.CodeOf(err) != contract.CodeInternal || !strings.Contains(err.Error(), "inspect") || readRef(t, dst.root, ref) != s.second.String() {
			t.Fatalf("post-rename: %v", err)
		}
	}
	// Cancellation before publication writes no ref; after the rename it
	// is not rolled back.
	if !run(len(ops) + 1) {
		return
	}
	dst2 := newRepo(t, map[string]fspec{"x": reg("x")})
	ctx, cancel := context.WithCancel(context.Background())
	d2 := fastDeps()
	d2.hook = func(st string) {
		if st == "pull-installed" {
			cancel()
		}
	}
	_, err := d2.pull(ctx, Options{GOOS: hostGOOS(), Env: s.env, Cwd: "/", Plane: s.p}, PullRequest{Name: "ws", Ref: "main", Path: dst2.root, PathSet: true})
	if !errors.Is(err, context.Canceled) || readRef(t, dst2.root, ref) != "" {
		t.Fatalf("cancel before publication: %v", err)
	}
	ctx3, cancel3 := context.WithCancel(context.Background())
	defer cancel3()
	d3 := fastDeps()
	d3.hook = func(st string) {
		if st == "ref-renamed" {
			cancel3()
		}
	}
	res, err := d3.pull(ctx3, Options{GOOS: hostGOOS(), Env: s.env, Cwd: "/", Plane: s.p}, PullRequest{Name: "ws", Ref: "main", Path: dst2.root, PathSet: true})
	if err != nil || !res.Changed || readRef(t, dst2.root, ref) != s.second.String() {
		t.Fatalf("cancel after publication %+v %v", res, err)
	}
	// A corrupt existing object is detected, never trusted or replaced.
	dst3 := newRepo(t, map[string]fspec{"x": reg("x")})
	h := s.second.String()
	obj := filepath.Join(dst3.root, ".git/objects", h[:2], h[2:])
	mustWrite(t, obj, "not zlib")
	if _, err := s.pull(t, fastDeps(), "main", dst3.root); err == nil || readRef(t, dst3.root, ref) != "" {
		t.Fatalf("corrupt object: %v", err)
	}
	if b, _ := os.ReadFile(obj); string(b) != "not zlib" {
		t.Fatal("the corrupt object was replaced")
	}
}

// The workspace is observed again after the fetch: a recreated workspace
// or a selected commit that became unreachable publishes nothing.
func TestPullGitMovingWorkspace(t *testing.T) {
	s := newMemSeed(t)
	dst := newRepo(t, map[string]fspec{"x": reg("x")})
	d := fastDeps()
	d.hook = func(st string) {
		if st == "pull-fetched" {
			s.p.create("ws")
		}
	}
	if _, err := s.pull(t, d, "main", dst.root); contract.CodeOf(err) != contract.CodeConflict {
		t.Fatalf("recreated: %v", err)
	}
	if readRef(t, dst.root, "refs/callsheet/ws/heads/main") != "" {
		t.Fatal("published")
	}
	s2 := newMemSeed(t)
	d2 := fastDeps()
	d2.hook = func(st string) {
		if st == "pull-resolved" {
			s2.p.setRef("ws", "refs/heads/main", s2.topic)
		}
	}
	dst2 := newRepo(t, map[string]fspec{"x": reg("x")})
	if _, err := s2.pull(t, d2, "main", dst2.root); contract.CodeOf(err) != contract.CodeConflict {
		t.Fatalf("unreachable selection: %v", err)
	}
	if readRef(t, dst2.root, "refs/callsheet/ws/heads/main") != "" {
		t.Fatal("published")
	}
	// A path inside a repository is refused.
	os.MkdirAll(filepath.Join(dst.root, "sub"), 0o755)
	if _, err := s.pull(t, fastDeps(), "main", filepath.Join(dst.root, "sub")); contract.TransferReason(err) != contract.ReasonUnsupportedRepository {
		t.Fatalf("inside: %v", err)
	}
	// Argument errors come first.
	for _, req := range []PullRequest{{Name: "Ws", Ref: "main"}, {Name: "ws", Ref: "HEAD~1"}, {Name: "ws", Ref: "main", PathSet: true}} {
		if _, err := fastDeps().pull(context.Background(), Options{GOOS: "linux", Env: s.env, Cwd: "/", Plane: s.p}, req); contract.CodeOf(err) != contract.CodeInvalidArgument {
			t.Fatalf("%+v: %v", req, err)
		}
	}
	if _, err := fastDeps().pull(context.Background(), Options{GOOS: "plan9", Plane: s.p}, PullRequest{Name: "ws", Ref: "main"}); contract.CodeOf(err) != contract.CodeInvalidArgument {
		t.Fatalf("goos: %v", err)
	}
}

// The exported entry points use the production durability (real syncs).
func TestProductionEntryPoints(t *testing.T) {
	// Deterministic: once per CPU setting under a repeated run.
	if !sampler(t)(0) {
		return
	}
	s := newMemSeed(t)
	dst := newRepo(t, map[string]fspec{"x": reg("x")})
	o := Options{GOOS: hostGOOS(), Env: s.env, Cwd: "/", Plane: s.p}
	if _, err := Pull(context.Background(), o, PullRequest{Name: "ws", Ref: "main", Path: dst.root, PathSet: true}); err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(tempDir(t), "out")
	if _, err := Pull(context.Background(), o, PullRequest{Name: "ws", Ref: "main", Path: out, PathSet: true}); err != nil {
		t.Fatal(err)
	}
	folder := writeFolder(t, map[string]fspec{"f": reg("f")}, nil)
	inst := s.p.create("fresh")
	if _, err := Push(context.Background(), o, PushRequest{Name: "fresh", Instance: inst, Path: folder, PathSet: true}); err != nil {
		t.Fatal(err)
	}
}
