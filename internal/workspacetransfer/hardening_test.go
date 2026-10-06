package workspacetransfer

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/filemode"
	"golang.org/x/sys/unix"

	"github.com/wedevwork/callsheet/internal/contract"
	"github.com/wedevwork/callsheet/internal/testkit"
)

// Regressions of code review r1: repository discovery, export link
// aliases, ref chains and syntax, publication cancellation, source-change
// baselines, the binary macro, bounded ident cleaning, effective LFS
// attributes and durable export modes.

// emptyGitAncestor is a directory whose .git is an empty read-only
// directory (a sandbox stub): not a repository for Git, so not a boundary.
func emptyGitAncestor(t *testing.T) string {
	t.Helper()
	base := tempDir(t)
	if err := os.Mkdir(filepath.Join(base, ".git"), 0o555); err != nil {
		t.Fatal(err)
	}
	return base
}

// Git's discovery (setup.c is_git_directory): a .git directory is a
// repository only with a valid HEAD, objects/ and refs/ (objects and refs
// through commondir when present); a bare ancestor likewise. An empty or
// invalid .git is not a repository boundary and discovery continues.
func TestAncestorDiscoveryMatchesGit(t *testing.T) {
	env := emptyEnv(t)
	base := emptyGitAncestor(t)
	folder := filepath.Join(base, "w", "folder")
	mustWrite(t, filepath.Join(folder, "f"), "f\n")
	if err := pushLocal(t, folder, env); err != nil {
		t.Fatalf("a folder below an empty read-only .git: %v", err)
	}
	if e, err := prepareExport(filepath.Join(base, "w", "out")); err != nil {
		t.Fatalf("an export below an empty read-only .git: %v", err)
	} else {
		e.close()
	}
	hash := strings.Repeat("ab", 20)
	run := sampler(t)
	for i, c := range []struct {
		what   string
		setup  func(g string)
		inside bool
	}{
		{"objects and refs without HEAD", func(g string) { os.MkdirAll(g+"/objects", 0o755); os.MkdirAll(g+"/refs", 0o755) }, false},
		{"a HEAD that is not a ref", func(g string) {
			os.MkdirAll(g+"/objects", 0o755)
			os.MkdirAll(g+"/refs", 0o755)
			mustWrite(t, g+"/HEAD", "nonsense\n")
		}, false},
		{"a symbolic HEAD outside refs/", func(g string) {
			os.MkdirAll(g+"/objects", 0o755)
			os.MkdirAll(g+"/refs", 0o755)
			mustWrite(t, g+"/HEAD", "ref: heads/main\n")
		}, false},
		{"HEAD without objects", func(g string) { os.MkdirAll(g+"/refs", 0o755); mustWrite(t, g+"/HEAD", "ref: refs/heads/main\n") }, false},
		{"a symbolic HEAD", func(g string) {
			os.MkdirAll(g+"/objects", 0o755)
			os.MkdirAll(g+"/refs", 0o755)
			mustWrite(t, g+"/HEAD", "ref:  refs/heads/main\n")
		}, true},
		{"a detached HEAD", func(g string) {
			os.MkdirAll(g+"/objects", 0o755)
			os.MkdirAll(g+"/refs", 0o755)
			mustWrite(t, g+"/HEAD", hash+"\n")
		}, true},
		{"a HEAD symlink into refs/", func(g string) {
			os.MkdirAll(g+"/objects", 0o755)
			os.MkdirAll(g+"/refs", 0o755)
			os.Symlink("refs/heads/main", g+"/HEAD")
		}, true},
		{"commondir objects and refs", func(g string) {
			common := g + "-common"
			os.MkdirAll(common+"/objects", 0o755)
			os.MkdirAll(common+"/refs", 0o755)
			mustWrite(t, g+"/HEAD", "ref: refs/heads/main\n")
			mustWrite(t, g+"/commondir", "../.git-common\n")
		}, true},
		{"CRLF commondir", func(g string) {
			common := g + "-common"
			os.MkdirAll(common+"/objects", 0o755)
			os.MkdirAll(common+"/refs", 0o755)
			mustWrite(t, g+"/HEAD", "ref: refs/heads/main\n")
			mustWrite(t, g+"/commondir", "../.git-common\r\n")
		}, true},
	} {
		if !run(i) {
			continue
		}
		root := tempDir(t)
		c.setup(filepath.Join(root, ".git"))
		err := checkAncestors(filepath.Join(root, "sub", "dir"))
		if c.inside != (err != nil) {
			t.Fatalf("%s: inside=%v, got %v", c.what, c.inside, err)
		}
		if err != nil && contract.TransferReason(err) != contract.ReasonUnsupportedRepository {
			t.Fatalf("%s: %v", c.what, err)
		}
	}
	// A .git file (a gitfile) and a bare ancestor stay boundaries.
	root := tempDir(t)
	mustWrite(t, filepath.Join(root, ".git"), "gitdir: /nowhere\n")
	if checkAncestors(filepath.Join(root, "x")) == nil {
		t.Fatal("a .git file ancestor")
	}
	bare := tempDir(t)
	os.MkdirAll(filepath.Join(bare, "objects"), 0o755)
	os.MkdirAll(filepath.Join(bare, "refs"), 0o755)
	mustWrite(t, filepath.Join(bare, "HEAD"), "ref: refs/heads/main\n")
	if checkAncestors(filepath.Join(bare, "objects", "x")) == nil {
		t.Fatal("a bare ancestor")
	}
}

// C1: a symlink resolved through a case or Unicode alias of another
// symlink must not escape on a filesystem that aliases names (darwin, or
// a destination probed as aliasing).
func TestExportLinkAliases(t *testing.T) {
	cases := []map[string]testkit.FileSpec{
		{"A": {Mode: filemode.Symlink, Content: []byte(".")}, "B": {Mode: filemode.Symlink, Content: []byte("a/../escape")}},
		{"é": {Mode: filemode.Symlink, Content: []byte(".")}, "x": {Mode: filemode.Symlink, Content: []byte("é/../escape")}},
		{"Dir/Up": {Mode: filemode.Symlink, Content: []byte("..")}, "y": {Mode: filemode.Symlink, Content: []byte("dir/UP/../escape")}},
	}
	run := sampler(t)
	for i, files := range cases {
		if !run(i) {
			continue
		}
		f := newExportFixture(t, files)
		parent := tempDir(t)
		if err := f.export(t, fastDeps(), "darwin", filepath.Join(parent, "out"), f.commit); contract.TransferReason(err) != contract.ReasonUnsafeTree {
			t.Fatalf("case %d darwin: %v", i, err)
		}
		if _, err := os.Lstat(filepath.Join(parent, "out")); !os.IsNotExist(err) {
			t.Fatalf("case %d: published", i)
		}
		// Exact-name semantics (a case- and normalization-sensitive
		// destination): the link stays inside the export.
		d := fastDeps()
		d.aliasProbe = func(int) (bool, error) { return false, nil }
		if err := f.export(t, d, "linux", filepath.Join(parent, "exact"), f.commit); err != nil {
			t.Fatalf("case %d exact: %v", i, err)
		}
		// A linux destination probed as aliasing names is checked the
		// darwin way before anything is materialized.
		d = fastDeps()
		d.aliasProbe = func(int) (bool, error) { return true, nil }
		created := false
		d.fail = func(op string) error {
			if op == "export-create" || op == "export-symlink" {
				created = true
			}
			return nil
		}
		if err := f.export(t, d, "linux", filepath.Join(parent, "probed"), f.commit); contract.TransferReason(err) != contract.ReasonUnsafeTree || created {
			t.Fatalf("case %d probed: %v (created %v)", i, err, created)
		}
		if s := staging(t, parent); len(s) != 0 {
			t.Fatalf("case %d: staging left %v", i, s)
		}
	}
	// The probe itself agrees with an independent check of the host's
	// temporary filesystem.
	dir := tempDir(t)
	fd, err := openPathDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer closeFD(fd)
	got, err := probeAliasing(fd)
	if err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(dir, "Probe"), nil, 0o600)
	_, cerr := os.Lstat(filepath.Join(dir, "pROBE"))
	os.WriteFile(filepath.Join(dir, "é"), nil, 0o600)
	_, nerr := os.Lstat(filepath.Join(dir, "é"))
	if want := cerr == nil || nerr == nil; got != want {
		t.Fatalf("probe %v, independent check %v", got, want)
	}
	if es, _ := os.ReadDir(dir); len(es) != 2 && len(es) != 1 {
		t.Fatalf("probe files left: %v", es)
	}
	// An oversized symlink target is refused before it is read whole.
	big := map[string]testkit.FileSpec{"l": {Mode: filemode.Symlink, Content: []byte(strings.Repeat("a/", 3000))}}
	fb := newExportFixture(t, big)
	if err := fb.export(t, fastDeps(), "linux", filepath.Join(tempDir(t), "out"), fb.commit); contract.TransferReason(err) != contract.ReasonUnsafeTree {
		t.Fatalf("oversized link target: %v", err)
	}
}

// C3: ref names are validated at every hop before any traversal.
func TestRefSyntax(t *testing.T) {
	for _, s := range []string{"ref: refs/../../../outside-ref", "ref: /etc/passwd", "ref: refs/heads/../x", "ref: refs/heads/./x",
		"ref: refs//heads", "ref: refs/heads/x.lock", "ref: refs/heads/.x", "ref: refs/heads/x.", "ref: refs/heads/a b",
		"ref: refs/heads/a~1", "ref: refs/heads/a@{1}", "ref: heads/main", "ref: refs/heads/", "ref: refs/heads/a\\b", "ref: HEAD"} {
		if _, ok := parseRefContent([]byte(s + "\n")); ok {
			t.Fatalf("%q accepted", s)
		}
	}
	for _, s := range []string{"ref: refs/heads/main", "ref: refs/tags/v1.0", "ref: refs/callsheet/ws/heads/a-b_c"} {
		if _, ok := parseRefContent([]byte(s + "\n")); !ok {
			t.Fatalf("%q refused", s)
		}
	}
	// Git accepts a trailing CR on a loose ref (sane isspace is space,
	// tab, CR and LF). LF-only trimming rejects the hash.
	hash := strings.Repeat("ab", 20)
	for _, s := range []string{hash + "\r\n", hash + "\r", hash + " \r\n", "ref: refs/heads/main\r\n"} {
		if _, ok := parseRefContent([]byte(s)); !ok {
			t.Fatalf("%q refused", s)
		}
	}
	r := newRepo(t, map[string]fspec{"f": reg("f")})
	fd, err := openPathDir(filepath.Join(r.root, ".git"))
	if err != nil {
		t.Fatal(err)
	}
	defer closeFD(fd)
	for _, n := range []string{"refs/../HEAD", "../config", "refs/heads/..", "/abs", "refs//x"} {
		if _, err := looseRef(fd, n); !errors.Is(err, errUnsafeMetadata) {
			t.Fatalf("looseRef %q: %v", n, err)
		}
		if _, _, err := resolveRef(fd, n); !errors.Is(err, errUnsafeMetadata) {
			t.Fatalf("resolveRef %q: %v", n, err)
		}
	}
	// A source whose HEAD reaches a hash file outside the repository is
	// refused, never pushed.
	env := emptyEnv(t)
	mustWrite(t, filepath.Join(filepath.Dir(r.root), "outside-ref"), r.head.String()+"\n")
	mustWrite(t, filepath.Join(r.root, ".git/HEAD"), "ref: refs/../../../outside-ref\n")
	wantRefused(t, pushLocal(t, r.root, env), "HEAD traversal")
	mustWrite(t, filepath.Join(r.root, ".git/HEAD"), "ref: refs/heads/main\n")
	mustWrite(t, filepath.Join(r.root, ".git/refs/heads/main"), "ref: refs/heads/../../../../outside-ref\n")
	wantRefused(t, pushLocal(t, r.root, env), "branch traversal")
	// The same for a pull destination's HEAD and branch chain.
	s := newMemSeed(t)
	run := sampler(t)
	for i, c := range []struct{ file, content string }{
		{".git/HEAD", "ref: refs/../../../outside-ref\n"},
		{".git/refs/heads/main", "ref: refs/heads/../../../../outside-ref\n"},
		{".git/refs/heads/other", "ref: refs/../../x\n"},
	} {
		if !run(i) {
			continue
		}
		dst := newRepo(t, map[string]fspec{"x": reg("x")})
		mustWrite(t, filepath.Join(filepath.Dir(dst.root), "outside-ref"), dst.head.String()+"\n")
		mustWrite(t, filepath.Join(dst.root, c.file), c.content)
		before := fingerprint(t, dst.root)
		if _, err := s.pull(t, fastDeps(), "main", dst.root); contract.TransferReason(err) != contract.ReasonUnsupportedRepository &&
			contract.TransferReason(err) != contract.ReasonLocalRefConflict {
			t.Fatalf("destination %s: %v", c.file, err)
		}
		if fingerprint(t, dst.root) != before {
			t.Fatalf("destination %s changed", c.file)
		}
	}
}

// C2: HEAD, branch and linked-worktree HEAD chains are followed through
// every ref namespace; malformed, unreadable, cyclic and overlong chains
// are refused.
func TestPullRefChains(t *testing.T) {
	s := newMemSeed(t)
	ref := "refs/callsheet/ws/heads/main"
	run := sampler(t)
	for i, c := range []struct {
		what  string
		files map[string]string
	}{
		{"HEAD through a tag", map[string]string{".git/refs/heads/main": "ref: refs/tags/alias\n", ".git/refs/tags/alias": "ref: " + ref + "\n"}},
		{"HEAD directly through a tag", map[string]string{".git/HEAD": "ref: refs/tags/alias\n", ".git/refs/tags/alias": "ref: refs/notes/x\n", ".git/refs/notes/x": "ref: " + ref + "\n"}},
		{"a branch through a remote ref", map[string]string{".git/refs/heads/b": "ref: refs/remotes/o/b\n", ".git/refs/remotes/o/b": "ref: " + ref + "\n"}},
		{"a linked worktree's HEAD", map[string]string{".git/worktrees/w/HEAD": "ref: refs/tags/t\n", ".git/refs/tags/t": "ref: " + ref + "\n"}},
		{"a cycle", map[string]string{".git/refs/heads/c1": "ref: refs/tags/c2\n", ".git/refs/tags/c2": "ref: refs/heads/c1\n"}},
		{"an overlong chain", map[string]string{".git/refs/heads/l0": "ref: refs/tags/l1\n", ".git/refs/tags/l1": "ref: refs/tags/l2\n",
			".git/refs/tags/l2": "ref: refs/tags/l3\n", ".git/refs/tags/l3": "ref: refs/tags/l4\n", ".git/refs/tags/l4": "ref: refs/tags/l5\n",
			".git/refs/tags/l5": "ref: refs/tags/l6\n", ".git/refs/tags/l6": "ref: refs/tags/l7\n"}},
		{"a malformed hop", map[string]string{".git/refs/heads/m": "ref: refs/tags/m\n", ".git/refs/tags/m": "garbage\n"}},
		{"a malformed branch", map[string]string{".git/refs/heads/bad": "not a ref\n"}},
		{"a nested branch through a tag", map[string]string{".git/refs/heads/feature/x": "ref: refs/tags/t\n", ".git/refs/tags/t": "ref: " + ref + "\n"}},
		{"an oversized branch file", map[string]string{".git/refs/heads/big": strings.Repeat("a", maxRefFile+1)}},
		{"a malformed worktree HEAD", map[string]string{".git/worktrees/w/HEAD": "nonsense\n"}},
	} {
		if !run(i) {
			continue
		}
		dst := newRepo(t, map[string]fspec{"x": reg("x")})
		for p, v := range c.files {
			mustWrite(t, filepath.Join(dst.root, p), v)
		}
		before := fingerprint(t, dst.root)
		_, err := s.pull(t, fastDeps(), "main", dst.root)
		if contract.TransferReason(err) != contract.ReasonLocalRefConflict && contract.TransferReason(err) != contract.ReasonUnsupportedRepository {
			t.Fatalf("%s: %v", c.what, err)
		}
		if fingerprint(t, dst.root) != before && !strings.Contains(c.what, "HEAD") {
			t.Fatalf("%s: the destination changed", c.what)
		}
		if readRef(t, dst.root, ref) != "" {
			t.Fatalf("%s: the Callsheet ref was written", c.what)
		}
	}
	// A special file among the branches is refused.
	dst := newRepo(t, map[string]fspec{"x": reg("x")})
	unixMkfifo(t, filepath.Join(dst.root, ".git/refs/heads/fifo"))
	if _, err := s.pull(t, fastDeps(), "main", dst.root); contract.TransferReason(err) != contract.ReasonLocalRefConflict {
		t.Fatalf("a FIFO branch: %v", err)
	}
	// A valid chain elsewhere, a lock file and a worktree without HEAD
	// are no conflict.
	dst = newRepo(t, map[string]fspec{"x": reg("x")})
	mustWrite(t, filepath.Join(dst.root, ".git/refs/tags/ok"), "ref: refs/heads/main\n")
	mustWrite(t, filepath.Join(dst.root, ".git/refs/heads/main.lock"), "garbage\n")
	os.MkdirAll(filepath.Join(dst.root, ".git/worktrees/gone"), 0o755)
	if _, err := s.pull(t, fastDeps(), "main", dst.root); err != nil {
		t.Fatalf("unrelated chain: %v", err)
	}
}

// C4: a cancellation at the last boundary before publication publishes
// nothing and removes only the operation's own temporary state.
func TestPublishCancellation(t *testing.T) {
	s := newMemSeed(t)
	dst := newRepo(t, map[string]fspec{"x": reg("x")})
	ctx, cancel := context.WithCancel(context.Background())
	d := fastDeps()
	d.hook = func(st string) {
		if st == "ref-locked" {
			cancel()
		}
	}
	res, err := d.pull(ctx, Options{GOOS: hostGOOS(), Env: s.env, Cwd: "/", Plane: s.p}, PullRequest{Name: "ws", Ref: "main", Path: dst.root, PathSet: true})
	if !errors.Is(err, context.Canceled) || res.Changed {
		t.Fatalf("cancelled at ref-locked: %+v %v", res, err)
	}
	ref := "refs/callsheet/ws/heads/main"
	if readRef(t, dst.root, ref) != "" {
		t.Fatal("the Callsheet ref was published")
	}
	for _, p := range []string{".git/" + ref + ".lock", ".git/packed-refs.lock"} {
		if _, err := os.Lstat(filepath.Join(dst.root, p)); !os.IsNotExist(err) {
			t.Fatalf("%s left", p)
		}
	}
	parent := tempDir(t)
	dest := filepath.Join(parent, "out")
	ctx2, cancel2 := context.WithCancel(context.Background())
	d2 := fastDeps()
	d2.hook = func(st string) {
		if st == "export-staged" {
			cancel2()
		}
	}
	res, err = d2.pull(ctx2, Options{GOOS: hostGOOS(), Env: s.env, Cwd: "/", Plane: s.p}, PullRequest{Name: "ws", Ref: "main", Path: dest, PathSet: true})
	if !errors.Is(err, context.Canceled) || res.Changed {
		t.Fatalf("cancelled at export-staged: %+v %v", res, err)
	}
	if _, err := os.Lstat(dest); !os.IsNotExist(err) {
		t.Fatal("the export was published")
	}
	if st := staging(t, parent); len(st) != 0 {
		t.Fatalf("staging left %v", st)
	}
	// An existing empty destination is left as it was.
	os.Mkdir(dest, 0o750)
	ctx3, cancel3 := context.WithCancel(context.Background())
	d2.hook = func(st string) {
		if st == "export-staged" {
			cancel3()
		}
	}
	if _, err := d2.pull(ctx3, Options{GOOS: hostGOOS(), Env: s.env, Cwd: "/", Plane: s.p}, PullRequest{Name: "ws", Ref: "main", Path: dest, PathSet: true}); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled into an empty directory: %v", err)
	}
	if es, err := os.ReadDir(dest); err != nil || len(es) != 0 {
		t.Fatalf("the empty destination changed: %v %v", es, err)
	}
	// A push cancelled at its last boundary sends nothing.
	mp := newMemPlane()
	inst := mp.create("ws")
	r := newRepo(t, map[string]fspec{"f": reg("a")})
	ctx4, cancel4 := context.WithCancel(context.Background())
	d4 := fastDeps()
	d4.hook = func(st string) {
		if st == "push-publish" {
			cancel4()
		}
	}
	if _, err := d4.push(ctx4, Options{GOOS: hostGOOS(), Env: s.env, Cwd: "/", Plane: mp}, PushRequest{Name: "ws", Instance: inst, Path: r.root, PathSet: true}); !errors.Is(err, context.Canceled) {
		t.Fatalf("push cancelled at push-publish: %v", err)
	}
	if len(mp.refs("ws")) != 0 {
		t.Fatal("the hub changed")
	}
}

// C5: the source-change baselines are the values openRepo read, so a
// change between admission and the scan is a source change.
func TestSourceChangeBaselines(t *testing.T) {
	env := emptyEnv(t)
	files := map[string]fspec{"f": reg("f\n")}
	run := sampler(t)
	for i, c := range []struct {
		what   string
		mutate func(r *fixtureRepo)
	}{
		{"HEAD advanced to a commit with the same tree", func(r *fixtureRepo) {
			h, err := testkit.WriteCommit(r.store, r.treeOf(files), []plumbing.Hash{r.head}, "same tree", testkit.FixedWhen)
			if err != nil {
				t.Fatal(err)
			}
			mustWrite(t, filepath.Join(r.root, ".git/refs/heads/main"), h.String()+"\n")
		}},
		{"HEAD moved to another branch", func(r *fixtureRepo) {
			mustWrite(t, filepath.Join(r.root, ".git/refs/heads/other"), r.head.String()+"\n")
			mustWrite(t, filepath.Join(r.root, ".git/HEAD"), "ref: refs/heads/other\n")
		}},
		{"the index changed", func(r *fixtureRepo) {
			r.writeIndex(map[string]fspec{"f": reg("f\n"), "g": reg("g\n")})
		}},
		{"the repository configuration changed", func(r *fixtureRepo) { r.config("[core]\n\tautocrlf = true\n") }},
		{"a global configuration appeared", func(r *fixtureRepo) {
			home, _ := env.Lookup("HOME")
			mustWrite(t, filepath.Join(home, ".gitconfig"), "[core]\n\tfilemode = false\n")
			t.Cleanup(func() { os.Remove(filepath.Join(home, ".gitconfig")) })
		}},
	} {
		if !run(i) {
			continue
		}
		r := newRepo(t, files)
		rp, err := openRepo(context.Background(), env, r.root)
		if err != nil {
			t.Fatal(err)
		}
		c.mutate(r)
		man, err := checkClean(context.Background(), fastDeps(), rp, env, "linux")
		if err == nil {
			err = man.verify(context.Background(), rp.rootFD)
		}
		rp.close()
		if !errors.Is(err, errChanged) && contract.TransferReason(err) != contract.ReasonSourceChanged {
			t.Fatalf("%s: %v", c.what, err)
		}
	}
	// Unchanged metadata verifies.
	r := newRepo(t, files)
	rp, err := openRepo(context.Background(), env, r.root)
	if err != nil {
		t.Fatal(err)
	}
	defer rp.close()
	man, err := checkClean(context.Background(), fastDeps(), rp, env, "linux")
	if err != nil || man.verify(context.Background(), rp.rootFD) != nil {
		t.Fatalf("unchanged: %v", err)
	}
}

// C6: Git honors a redefined binary macro; one that changes a conversion
// is refused as unsupported, an equivalent one is accepted.
func TestBinaryMacroRedefinition(t *testing.T) {
	for _, def := range []string{"[attr]binary text\n", "[attr]binary -diff\n", "[attr]binary eol=crlf -text\n", "[attr]binary\n"} {
		if _, err := parseAttributes([]byte(def+"f binary\n"), "", true, false); !errors.As(err, new(errUnsupportedMacro)) {
			t.Fatalf("%q: %v", def, err)
		}
	}
	f, err := parseAttributes([]byte("[attr]binary -diff -merge -text\nf binary\n"), "", true, false)
	if err != nil {
		t.Fatal(err)
	}
	if s := resolveAttrs([]*attrFile{f}, "f"); s["text"].state != attrUnset {
		t.Fatalf("equivalent redefinition %+v", s)
	}
	// Through a push: refused as unsupported_repository, never dirty.
	env := emptyEnv(t)
	r := newRepo(t, map[string]fspec{".gitattributes": reg("[attr]binary text\nf binary\n"), "f": reg("a\nb\n")})
	r.writeFile("f", reg("a\r\nb\r\n"))
	wantRefused(t, pushLocal(t, r.root, env), "redefined binary macro")
}

// identToGit is Git's ident_to_git over a whole buffer (the reference).
func identToGit(src []byte) []byte {
	var out []byte
	for {
		i := bytes.IndexByte(src, '$')
		if i < 0 {
			break
		}
		out = append(out, src[:i+1]...)
		src = src[i+1:]
		if len(src) > 3 && string(src[:3]) == "Id:" {
			j := bytes.IndexByte(src[3:], '$')
			if j < 0 {
				break
			}
			if bytes.IndexByte(src[3:3+j], '\n') >= 0 {
				continue
			}
			out = append(out, "Id$"...)
			src = src[3+j+1:]
		}
	}
	return append(out, src...)
}

// drain reads r completely.
func drain(r io.Reader) ([]byte, error) {
	var b bytes.Buffer
	_, err := copyCtx(context.Background(), &b, r)
	return b.Bytes(), err
}

// C7: ident cleaning keeps Git's semantics with bounded memory and sees
// cancellation while scanning a long candidate.
func TestIdentBounded(t *testing.T) {
	ctx := context.Background()
	long := strings.Repeat("x", 1<<20) // far beyond identMemory: the spool path
	run := sampler(t)
	for i, in := range []string{
		"a $Id: abc $ b", "$Id:$", "$Id$", "$Id:", "$Id: x\n$ y", "$Id: x\ny$Id: z$", "$$Id: q$", "$Id",
		"pre $Id:" + long + "$ post", "pre $Id:" + long, "pre $Id:" + long + "\nrest $Id: k$",
		"$Id: a$ $Id: b$\n$Id:\n", strings.Repeat("$Id: r$", 1000),
	} {
		if !run(i) {
			continue
		}
		want := identToGit([]byte(in))
		wantSum := sha256.Sum256(want)
		var stats runtime.MemStats
		runtime.GC()
		runtime.ReadMemStats(&stats)
		before := stats.TotalAlloc
		h := sha256.New()
		n, err := copyCtx(ctx, h, cleanReader(ctx, strings.NewReader(in), false, true, t.TempDir()))
		runtime.ReadMemStats(&stats)
		if err != nil {
			t.Fatal(err)
		}
		if n != int64(len(want)) || !bytes.Equal(h.Sum(nil), wantSum[:]) {
			t.Fatalf("%.60q: got %d bytes, want %.60q", in, n, want)
		}
		// The reader's own allocation is bounded, not proportional to
		// the candidate.
		if alloc := stats.TotalAlloc - before; alloc > 512<<10 {
			t.Fatalf("%.30q: allocated %d bytes for %d", in, alloc, n)
		}
		got, err := drain(cleanReader(ctx, strings.NewReader(in), false, true, t.TempDir()))
		if err != nil || string(got) != string(want) {
			t.Fatalf("%.60q: got %.60q, want %.60q (%v)", in, got, want, err)
		}
	}
	// One output byte from a 1 MiB candidate allocates a bounded amount.
	candidate := "$Id:" + long + "$"
	tmp := t.TempDir()
	var stats runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&stats)
	before := stats.TotalAlloc
	r := cleanReader(ctx, strings.NewReader(candidate), false, true, tmp)
	b := make([]byte, 1)
	if n, err := r.Read(b); n != 1 || err != nil || b[0] != '$' {
		t.Fatalf("first byte %q %v", b[:n], err)
	}
	runtime.ReadMemStats(&stats)
	if alloc := stats.TotalAlloc - before; alloc > 512<<10 {
		t.Fatalf("one byte of a 1 MiB candidate allocated %d bytes", alloc)
	}
	// Cancellation is observed while a candidate is scanned.
	cctx, cancel := context.WithCancel(ctx)
	cancel()
	_, err := drain(cleanReader(cctx, strings.NewReader("$Id:"+long), false, true, t.TempDir()))
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled scan: %v", err)
	}
}

// C9: an effective filter=lfs anywhere in the attributes Git would read
// (nested, info or global) with a configured LFS filter is refused for a
// pull destination and a push source; a comment or an unconfigured
// filter is not.
func TestLFSEffectiveAttributes(t *testing.T) {
	s := newMemSeed(t)
	lfs := "[filter \"lfs\"]\n\tclean = git-lfs clean -- %f\n\tsmudge = git-lfs smudge -- %f\n\trequired = true\n"
	run := sampler(t)
	for i, c := range []struct {
		what    string
		setup   func(r *fixtureRepo, home string)
		refused bool
	}{
		{"nested .gitattributes", func(r *fixtureRepo, _ string) {
			r.config(lfs)
			mustWrite(t, filepath.Join(r.root, "sub/.gitattributes"), "*.dat filter=lfs diff=lfs merge=lfs -text\n")
			mustWrite(t, filepath.Join(r.root, "sub/x.dat"), "not a pointer\n")
		}, true},
		{"deep untracked .gitattributes", func(r *fixtureRepo, _ string) {
			r.config(lfs)
			mustWrite(t, filepath.Join(r.root, "a/b/c/.gitattributes"), "big filter=lfs\n")
		}, true},
		{"info/attributes", func(r *fixtureRepo, _ string) {
			r.config(lfs)
			mustWrite(t, filepath.Join(r.root, ".git/info/attributes"), "* filter=lfs\n")
		}, true},
		{"global attributes", func(r *fixtureRepo, home string) {
			r.config(lfs)
			mustWrite(t, filepath.Join(home, ".config/git/attributes"), "*.bin filter=lfs\n")
		}, true},
		{"configured attributesFile", func(r *fixtureRepo, home string) {
			r.config(lfs + "[core]\n\tattributesFile = " + filepath.Join(home, "attrs") + "\n")
			mustWrite(t, filepath.Join(home, "attrs"), "*.bin filter=lfs\n")
		}, true},
		{"a tracked .gitattributes missing from the working tree", func(r *fixtureRepo, _ string) {
			r.config(lfs)
			r.commit(map[string]fspec{"x": reg("x"), "deep/.gitattributes": reg("*.iso filter=lfs\n")}, "attrs")
			os.Remove(filepath.Join(r.root, "deep/.gitattributes"))
		}, true},
		{"a macro routing to LFS", func(r *fixtureRepo, _ string) {
			r.config(lfs)
			mustWrite(t, filepath.Join(r.root, ".gitattributes"), "[attr]big filter=lfs\n*.iso big\n")
		}, true},
		{"a comment only", func(r *fixtureRepo, _ string) {
			r.config(lfs)
			mustWrite(t, filepath.Join(r.root, "sub/.gitattributes"), "# *.dat filter=lfs\n")
		}, false},
		{"no LFS filter configured", func(r *fixtureRepo, _ string) {
			mustWrite(t, filepath.Join(r.root, "sub/.gitattributes"), "*.dat filter=lfs\n")
		}, false},
		{"an ignored .git subtree is not read", func(r *fixtureRepo, _ string) {
			r.config(lfs)
			mustWrite(t, filepath.Join(r.root, "vendor/.git/.gitattributes"), "* filter=lfs\n")
		}, false},
	} {
		if !run(i) {
			continue
		}
		home := tempDir(t)
		env := envOf(map[string]string{"HOME": home, "GIT_CONFIG_NOSYSTEM": "1"})
		dst := newRepo(t, map[string]fspec{"x": reg("x")})
		c.setup(dst, home)
		before := fingerprint(t, dst.root)
		_, err := pullWith(t, fastDeps(), s.p, env, "main", dst.root)
		if c.refused {
			if contract.TransferReason(err) != contract.ReasonUnsupportedRepository || !strings.Contains(err.Error(), "LFS") && !strings.Contains(err.Error(), "macro") {
				t.Fatalf("%s: %v", c.what, err)
			}
			if fingerprint(t, dst.root) != before {
				t.Fatalf("%s: the destination changed", c.what)
			}
		} else if err != nil && !strings.Contains(c.what, ".git subtree") {
			t.Fatalf("%s: %v", c.what, err)
		}
	}
}

// C10: an exported file's final mode is set before its contents are
// synced (the sync seam observes the final mode).
func TestExportModeBeforeSync(t *testing.T) {
	f := newExportFixture(t, exportFiles)
	d := fastDeps()
	var modes []uint32
	d.syncFD = func(fd int) error {
		var st unix.Stat_t
		if err := unix.Fstat(fd, &st); err != nil {
			return err
		}
		if st.Mode&unix.S_IFMT == unix.S_IFREG {
			modes = append(modes, uint32(st.Mode&0o7777))
		}
		return nil
	}
	if err := f.export(t, d, hostGOOS(), filepath.Join(tempDir(t), "out"), f.commit); err != nil {
		t.Fatal(err)
	}
	var exe, plain int
	for _, m := range modes {
		switch m {
		case 0o755:
			exe++
		case 0o644:
			plain++
		default:
			t.Fatalf("a file was synced with mode %o", m)
		}
	}
	if exe != 1 || plain != 3 {
		t.Fatalf("synced %d executable and %d regular files", exe, plain)
	}
}

// openDescriptors counts this process's open file descriptors on Linux
// (/proc/self/fd). Other hosts return -1; the count is a property of the
// host, so the decision goes through hostGOOS.
func openDescriptors(t *testing.T) int {
	t.Helper()
	if hostGOOS() != "linux" {
		return -1
	}
	names, err := os.ReadDir("/proc/self/fd")
	if err != nil {
		t.Fatal(err)
	}
	return len(names)
}

// C1 (review r2): an alias-probe error and an unsafe-link refusal both
// happen after the staging directory is opened. Each call must close that
// descriptor; repeated failures must not accumulate them.
//
// Every call is checked on its own: its transfer reason and, on Linux,
// the process's open descriptor count right after it returns, which must
// equal the count before the first call. A call that leaks fails at that
// call, so the proof does not rest on the number of calls. The
// descriptors under test (the destination's parent, the staging root and
// those removeTreeAt opens to remove the staging directory) are raw, with
// no finalizer, so a leak of one stays open until it is counted. Object
// and HEAD reads go through *os.File, whose finalizer a collection could
// run behind a leak; counting right after each call leaves it the least
// time to. Nothing in the path is keyed to a call count: the calls differ
// only in the random staging name and in the first call reading the
// fixture's objects from disk while later calls find them in the store's
// object cache. Ten calls cover that cold call and nine warm ones (the
// test made 100 calls with one count after the last before the stress
// stage's cost was cut).
func TestExportAliasFailuresReleaseDescriptor(t *testing.T) {
	const calls = 10
	run := func(what string, files map[string]testkit.FileSpec, probe func(int) (bool, error), reason string) {
		t.Run(what, func(t *testing.T) {
			f := newExportFixture(t, files)
			parent := tempDir(t)
			d := fastDeps()
			d.aliasProbe = probe
			before := openDescriptors(t)
			for i := 0; i < calls; i++ {
				err := f.export(t, d, "linux", filepath.Join(parent, "out"), f.commit)
				if contract.TransferReason(err) != reason {
					t.Fatalf("%s call %d: %v", what, i, err)
				}
				if before < 0 {
					continue
				}
				if after := openDescriptors(t); after != before {
					t.Fatalf("%s call %d: descriptors before=%d after=%d leaked=%d", what, i, before, after, after-before)
				}
			}
			if s := staging(t, parent); len(s) != 0 {
				t.Fatalf("%s: staging left %v", what, s)
			}
			if _, err := os.Lstat(filepath.Join(parent, "out")); !os.IsNotExist(err) {
				t.Fatalf("%s: published", what)
			}
		})
	}
	run("probe errors", map[string]testkit.FileSpec{"a": {Mode: filemode.Regular, Content: []byte("a")}},
		func(int) (bool, error) { return false, errors.New("probe failed") }, contract.ReasonStorageFailure)
	run("refused exports", map[string]testkit.FileSpec{
		"A": {Mode: filemode.Symlink, Content: []byte(".")},
		"B": {Mode: filemode.Symlink, Content: []byte("a/../escape")},
	}, func(int) (bool, error) { return true, nil }, contract.ReasonUnsafeTree)
}

// C1 (review r3): Git's sane isspace is space, tab, CR and LF. A loose
// hash followed by vertical tab or form feed is corrupt, and Push refuses
// it. Trailing space, tab, CRLF and LF stay valid.
func TestLooseRefGitSpace(t *testing.T) {
	hash := strings.Repeat("ab", 20)
	t.Run("parser", func(t *testing.T) {
		for _, suf := range []string{"\v", "\f", "\v\n", "\f\n"} {
			if _, ok := parseRefContent([]byte(hash + suf)); ok {
				t.Errorf("hash %q accepted", suf)
			}
			if _, ok := parseRefContent([]byte("ref: refs/heads/main" + suf)); ok {
				t.Errorf("symbolic %q accepted", suf)
			}
		}
		for _, s := range []string{hash + " ", hash + "\t", hash + "\r\n", hash + "\n", "ref: refs/heads/main \t\r\n"} {
			if _, ok := parseRefContent([]byte(s)); !ok {
				t.Errorf("%q refused", s)
			}
		}
	})
	t.Run("head", func(t *testing.T) {
		dir := tempDir(t)
		for _, suf := range []string{"\v", "\f"} {
			mustWrite(t, filepath.Join(dir, "HEAD"), "ref:"+suf+"refs/heads/main\n")
			if validHeadRef(filepath.Join(dir, "HEAD")) {
				t.Errorf("HEAD ref:%qrefs/ accepted", suf)
			}
		}
		for _, s := range []string{"ref:  refs/heads/main\n", "ref:\trefs/heads/main\n"} {
			mustWrite(t, filepath.Join(dir, "HEAD"), s)
			if !validHeadRef(filepath.Join(dir, "HEAD")) {
				t.Errorf("%q refused", s)
			}
		}
	})
	t.Run("push", func(t *testing.T) {
		env := emptyEnv(t)
		for _, suf := range []string{"\v", "\f"} {
			name := "vt"
			if suf == "\f" {
				name = "ff"
			}
			t.Run(name, func(t *testing.T) {
				r := newRepo(t, map[string]fspec{"f": reg("f")})
				ref := filepath.Join(r.root, ".git/refs/heads/main")
				b, err := os.ReadFile(ref)
				if err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(ref, append(b, suf...), 0o644); err != nil {
					t.Fatal(err)
				}
				before := fingerprint(t, r.root)
				p := &localPlane{t: t}
				_, err = Push(context.Background(), Options{GOOS: "linux", Env: env, Cwd: "/", Plane: p},
					PushRequest{Name: "ws", Instance: tInst, Path: r.root, PathSet: true})
				wantRefused(t, err, "refs/heads/main"+suf)
				if p.pushes != 0 {
					t.Fatalf("%q was pushed", suf)
				}
				if fingerprint(t, r.root) != before {
					t.Fatalf("%q changed the source", suf)
				}
			})
		}
	})
	t.Run("packed-refs", func(t *testing.T) {
		r := newRepo(t, map[string]fspec{"f": reg("f")})
		if err := os.Remove(filepath.Join(r.root, ".git/refs/heads/main")); err != nil {
			t.Fatal(err)
		}
		fd, err := openPathDir(filepath.Join(r.root, ".git"))
		if err != nil {
			t.Fatal(err)
		}
		defer closeFD(fd)
		packed := filepath.Join(r.root, ".git/packed-refs")
		hash := r.head.String()
		mustWrite(t, packed, "# pack-refs with: peeled\n"+hash+" refs/heads/main\n")
		if h, found, err := resolveRef(fd, "refs/heads/main"); err != nil || !found || h.String() != hash {
			t.Fatalf("LF packed ref: found=%v err=%v h=%s", found, err, h)
		}
		// Git keeps a trailing CR in the packed ref name and rejects it.
		mustWrite(t, packed, "# pack-refs with: peeled\n"+hash+" refs/heads/main\r\n")
		if h, found, err := resolveRef(fd, "refs/heads/main"); err != nil || found {
			t.Fatalf("CRLF packed ref resolved as refs/heads/main: found=%v err=%v h=%s", found, err, h)
		}
	})
}

// C3 (review r2): a subdirectory of a repository whose commondir ends in
// CRLF is inside that repository. Push refuses it; it is not snapshotted
// as a plain folder.
func TestCRLFCommondirSubdirectoryRefused(t *testing.T) {
	env := emptyEnv(t)
	root := tempDir(t)
	git := filepath.Join(root, ".git")
	common := filepath.Join(root, ".git-common")
	if err := os.MkdirAll(filepath.Join(common, "objects"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(common, "refs"), 0o755); err != nil {
		t.Fatal(err)
	}
	mustWrite(t, filepath.Join(git, "HEAD"), "ref: refs/heads/main\n")
	mustWrite(t, filepath.Join(git, "commondir"), "../.git-common\r\n")
	sub := filepath.Join(root, "sub")
	mustWrite(t, filepath.Join(sub, "f"), "f\n")
	err := pushLocal(t, sub, env)
	if err == nil {
		t.Fatal("repository subdirectory accepted and pushed as a plain folder")
	}
	wantRefused(t, err, "CRLF commondir")
	if !strings.Contains(err.Error(), "use the repository root") {
		t.Fatalf("guidance %v", err)
	}
	if _, statErr := os.Lstat(filepath.Join(sub, ".git")); !os.IsNotExist(statErr) {
		t.Fatal("a subdirectory push created repository metadata")
	}
}

// identSpoolText is a candidate long enough to spill, with output bytes
// before the candidate so a spool error arrives after a partial Read.
func identSpoolText() []byte {
	return []byte("prefix$Id:" + strings.Repeat("x", identMemory+32<<10) + "$ suffix")
}

// C2 (review r2): a spool creation or write failure after earlier output
// must fail the read and the cleanliness check. Truncated content is not
// a clean or dirty decision.
func TestIdentSpoolFailure(t *testing.T) {
	ctx := context.Background()
	raw := identSpoolText()
	healthy, err := io.ReadAll(cleanReader(ctx, bytes.NewReader(raw), false, true, t.TempDir()))
	if err != nil || string(healthy) != "prefix$Id$ suffix" {
		t.Fatalf("clean fixture: %q (%d bytes) %v", healthy, len(healthy), err)
	}
	spools := []struct {
		name string
		open func(dir, pattern string) (*os.File, error)
	}{
		{"creation", func(string, string) (*os.File, error) {
			return nil, errors.New("ident spool create failed")
		}},
		{"write", func(dir, pattern string) (*os.File, error) {
			f, err := os.CreateTemp(dir, pattern)
			if err != nil {
				return nil, err
			}
			name := f.Name()
			if err := f.Close(); err != nil {
				os.Remove(name)
				return nil, err
			}
			// Read-only: the spill Write fails after the file exists.
			return os.Open(name)
		}},
	}
	for _, sp := range spools {
		t.Run(sp.name, func(t *testing.T) {
			prev := createIdentSpool
			createIdentSpool = sp.open
			t.Cleanup(func() { createIdentSpool = prev })

			got, err := io.ReadAll(cleanReader(ctx, bytes.NewReader(raw), false, true, t.TempDir()))
			if err == nil {
				t.Errorf("ReadAll returned err=nil and %d output bytes", len(got))
			}
			// The same failure stays terminal: a later Read must not
			// continue from the truncated candidate.
			rd := cleanReader(ctx, bytes.NewReader(raw), false, true, t.TempDir())
			buf := make([]byte, 64)
			if n, rerr := rd.Read(buf); rerr == nil || n == 0 {
				t.Fatalf("first read n=%d err=%v", n, rerr)
			}
			if _, rerr := rd.Read(buf); rerr == nil {
				t.Fatal("the next Read dropped the spool error")
			}
			closeReader(rd)

			// When the read swallowed the error, the index holds that
			// truncated output, so a buggy check accepts the file as clean.
			indexed := raw
			if err == nil {
				indexed = append([]byte(nil), got...)
			}
			env := emptyEnv(t)
			r := newRepo(t, map[string]fspec{
				".gitattributes": reg("*.c ident\n"),
				"f.c":            reg(string(indexed)),
			})
			r.writeFile("f.c", reg(string(raw)))
			rp, oerr := openRepo(context.Background(), env, r.root)
			if oerr != nil {
				t.Fatal(oerr)
			}
			defer rp.close()
			_, cerr := checkClean(context.Background(), fastDeps(), rp, env, "linux")
			if contract.TransferReason(cerr) != contract.ReasonStorageFailure {
				if cerr == nil {
					t.Fatal("dirty working-tree file accepted as CLEAN after ident spool IO failure")
				}
				t.Fatalf("ident spool IO failure: %v", cerr)
			}
		})
	}
}
