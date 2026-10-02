package workspacetransfer

import (
	"context"
	"crypto/sha256"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/filemode"
	"github.com/go-git/go-git/v5/plumbing/format/index"

	"github.com/wedevwork/callsheet/internal/client"
	"github.com/wedevwork/callsheet/internal/contract"
	"github.com/wedevwork/callsheet/internal/testkit"
)

// UT-10: repository classification and eligibility.

// localPlane is a plane that must not be reached beyond an empty receive
// advertisement: eligibility refusals happen before any mutation.
type localPlane struct {
	t      *testing.T
	pushes int
}

func (p *localPlane) ShowWorkspace(context.Context, string) (contract.WorkspaceView, error) {
	p.t.Fatal("show reached")
	return contract.WorkspaceView{}, nil
}
func (p *localPlane) WorkspaceStatus(context.Context, string, client.StatusPage) (contract.WorkspaceStatusResponse, error) {
	p.t.Fatal("status reached")
	return contract.WorkspaceStatusResponse{}, nil
}
func (p *localPlane) WorkspaceDiff(context.Context, string, client.DiffPage) (contract.WorkspaceDiffResponse, error) {
	p.t.Fatal("diff reached")
	return contract.WorkspaceDiffResponse{}, nil
}
func (p *localPlane) WorkspaceGit(string) (Git, error) { return p, nil }
func (p *localPlane) ReceiveRefs(context.Context, string) (map[string]plumbing.Hash, error) {
	return map[string]plumbing.Hash{}, nil
}
func (p *localPlane) Push(context.Context, string, string, plumbing.Hash, plumbing.Hash, io.Reader) error {
	p.pushes++
	return nil
}
func (p *localPlane) Fetch(context.Context, []plumbing.Hash, []plumbing.Hash, func(io.Reader) error) error {
	p.t.Fatal("fetch reached")
	return nil
}
func (p *localPlane) Close() {}

const tInst = "0123456789abcdef0123456789abcdef"

// fingerprint lists every path below root with its mode and content.
func fingerprint(t testing.TB, root string) string {
	t.Helper()
	var out []string
	filepath.Walk(root, func(p string, fi os.FileInfo, err error) error {
		if err != nil {
			return nil
		}
		rel, _ := filepath.Rel(root, p)
		s := rel + " " + fi.Mode().String()
		if fi.Mode().IsRegular() {
			b, _ := os.ReadFile(p)
			s += " " + fmt.Sprintf("%x", sha256.Sum256(b))
		}
		if fi.Mode()&os.ModeSymlink != 0 {
			l, _ := os.Readlink(p)
			s += " -> " + l
		}
		out = append(out, s)
		return nil
	})
	sort.Strings(out)
	return strings.Join(out, "\n")
}

// pushLocal runs a push against localPlane and returns its error.
func pushLocal(t *testing.T, path string, env Env) error {
	t.Helper()
	p := &localPlane{t: t}
	_, err := fastDeps().push(context.Background(), Options{GOOS: "linux", Env: env, Cwd: "/", Plane: p},
		PushRequest{Name: "ws", Instance: tInst, Path: path, PathSet: true})
	return err
}

func wantRefused(t *testing.T, err error, what string) {
	t.Helper()
	if contract.CodeOf(err) != contract.CodeInvalidArgument || contract.TransferReason(err) != contract.ReasonUnsupportedRepository {
		t.Fatalf("%s: want unsupported_repository, got %v", what, err)
	}
}

func TestEligibilityLayouts(t *testing.T) {
	env := emptyEnv(t)
	r := newRepo(t, map[string]fspec{"sub/f": reg("f"), "a": reg("a")})
	before := fingerprint(t, r.root)
	err := pushLocal(t, filepath.Join(r.root, "sub"), env)
	wantRefused(t, err, "subdirectory")
	if !strings.Contains(err.Error(), "use the repository root") {
		t.Fatalf("guidance %v", err)
	}
	if fingerprint(t, r.root) != before {
		t.Fatal("the repository changed")
	}
	// A plain folder below a repository is refused, never snapshotted.
	os.MkdirAll(filepath.Join(r.root, "plain"), 0o755)
	wantRefused(t, pushLocal(t, filepath.Join(r.root, "plain"), env), "folder inside a repository")
	// A .git file, a symlinked .git and bare layouts.
	wt := filepath.Join(tempDir(t), "worktree")
	mustWrite(t, filepath.Join(wt, ".git"), "gitdir: "+r.root+"/.git/worktrees/x\n")
	wantRefused(t, pushLocal(t, wt, env), ".git file")
	ln := filepath.Join(tempDir(t), "linked")
	os.MkdirAll(ln, 0o755)
	os.Symlink(filepath.Join(r.root, ".git"), filepath.Join(ln, ".git"))
	wantRefused(t, pushLocal(t, ln, env), "symlinked .git")
	odd := filepath.Join(tempDir(t), "odd")
	os.MkdirAll(odd, 0o755)
	unixMkfifo(t, filepath.Join(odd, ".git"))
	wantRefused(t, pushLocal(t, odd, env), "special .git")
	bare := filepath.Join(r.root, ".git")
	wantRefused(t, pushLocal(t, bare, env), "bare layout (a .git directory itself)")
	wantRefused(t, pushLocal(t, filepath.Join(bare, "objects"), env), "inside a bare layout")
	// A symlinked PATH ancestor is canonicalized once.
	alias := filepath.Join(tempDir(t), "alias")
	os.Symlink(r.root, alias)
	if err := pushLocal(t, alias, env); err != nil {
		t.Fatalf("symlinked path to a clean repository: %v", err)
	}
	if err := pushLocal(t, filepath.Join(tempDir(t), "missing"), env); contract.CodeOf(err) != contract.CodeNotFound {
		t.Fatalf("missing source: %v", err)
	}
	if err := pushLocal(t, filepath.Join(r.root, "a"), env); contract.CodeOf(err) != contract.CodeInvalidArgument {
		t.Fatalf("file source: %v", err)
	}
}

func TestEligibilityFeatures(t *testing.T) {
	env := emptyEnv(t)
	run := sampler(t)
	cases := []struct {
		what  string
		setup func(r *fixtureRepo)
	}{
		{"commondir", func(r *fixtureRepo) { mustWrite(t, filepath.Join(r.root, ".git/commondir"), "..\n") }},
		{"shallow", func(r *fixtureRepo) { mustWrite(t, filepath.Join(r.root, ".git/shallow"), r.head.String()+"\n") }},
		{"alternates", func(r *fixtureRepo) {
			mustWrite(t, filepath.Join(r.root, ".git/objects/info/alternates"), "/elsewhere/objects\n")
		}},
		{"http-alternates", func(r *fixtureRepo) {
			mustWrite(t, filepath.Join(r.root, ".git/objects/info/http-alternates"), "https://x/objects\n")
		}},
		{"promisor pack", func(r *fixtureRepo) { mustWrite(t, filepath.Join(r.root, ".git/objects/pack/pack-1.promisor"), "") }},
		{"partial clone config", func(r *fixtureRepo) { r.config("[remote \"origin\"]\n\tpromisor = true\n") }},
		{"partial clone extension", func(r *fixtureRepo) { r.config("[extensions]\n\tpartialClone = origin\n") }},
		{"sha256", func(r *fixtureRepo) {
			r.config("[core]\n\trepositoryformatversion = 1\n[extensions]\n\tobjectFormat = sha256\n")
		}},
		// Extensions take effect at repository format version 1 (Git
		// ignores an unknown one at version 0).
		{"reftable", func(r *fixtureRepo) {
			r.config("[core]\n\trepositoryformatversion = 1\n[extensions]\n\trefStorage = reftable\n")
		}},
		{"unknown extension", func(r *fixtureRepo) { r.config("[core]\n\trepositoryformatversion = 1\n[extensions]\n\tfuture = x\n") }},
		{"format version 2", func(r *fixtureRepo) { r.config("[core]\n\trepositoryformatversion = 2\n") }},
		{"worktree config", func(r *fixtureRepo) { r.config("[extensions]\n\tworktreeConfig = true\n") }},
		{"core.bare", func(r *fixtureRepo) { r.config("[core]\n\tbare = true\n") }},
		{"core.worktree", func(r *fixtureRepo) { r.config("[core]\n\tworktree = /elsewhere\n") }},
		{"sparse checkout", func(r *fixtureRepo) { r.config("[core]\n\tsparseCheckout = true\n") }},
		{"bad boolean", func(r *fixtureRepo) { r.config("[core]\n\tignorecase = maybe\n") }},
		{"configured submodule", func(r *fixtureRepo) { r.config("[submodule \"lib\"]\n\turl = x\n") }},
		{"modules directory", func(r *fixtureRepo) { os.MkdirAll(filepath.Join(r.root, ".git/modules/lib"), 0o755) }},
		{"gitmodules", func(r *fixtureRepo) {
			r.writeFile(".gitmodules", reg("[submodule \"lib\"]\n\tpath = lib\n\turl = x\n"))
		}},
		{"broken gitmodules", func(r *fixtureRepo) { r.writeFile(".gitmodules", reg("[submodule\n")) }},
		{"lfs", func(r *fixtureRepo) {
			r.config("[filter \"lfs\"]\n\tclean = git-lfs clean -- %f\n")
			r.writeFile(".gitattributes", reg("*.bin filter=lfs diff=lfs merge=lfs -text\n"))
		}},
		{"missing HEAD", func(r *fixtureRepo) { os.Remove(filepath.Join(r.root, ".git/HEAD")) }},
		{"symlinked HEAD", func(r *fixtureRepo) {
			os.Rename(filepath.Join(r.root, ".git/HEAD"), filepath.Join(r.root, ".git/HEAD.real"))
			os.Symlink("HEAD.real", filepath.Join(r.root, ".git/HEAD"))
		}},
		{"corrupt HEAD", func(r *fixtureRepo) { mustWrite(t, filepath.Join(r.root, ".git/HEAD"), "garbage\n") }},
		{"missing objects", func(r *fixtureRepo) { os.RemoveAll(filepath.Join(r.root, ".git/objects")) }},
		{"corrupt index", func(r *fixtureRepo) { mustWrite(t, filepath.Join(r.root, ".git/index"), "DIRC garbage") }},
		{"symlinked index", func(r *fixtureRepo) {
			os.Rename(filepath.Join(r.root, ".git/index"), filepath.Join(r.root, ".git/index.real"))
			os.Symlink("index.real", filepath.Join(r.root, ".git/index"))
		}},
		{"skip-worktree", func(r *fixtureRepo) {
			es := r.indexEntries(r.files)
			es[0].SkipWorktree = true
			r.writeIndexV3(es)
		}},
		{"assume-unchanged", func(r *fixtureRepo) {
			b, _ := os.ReadFile(filepath.Join(r.root, ".git/index"))
			os.WriteFile(filepath.Join(r.root, ".git/index"), indexWithFlags(t, b, 0x8000), 0o644)
		}},
		{"unmerged stage", func(r *fixtureRepo) {
			es := r.indexEntries(r.files)
			es[0].Stage = index.TheirMode
			r.writeIndexEntries(es)
		}},
		{"gitlink in the index", func(r *fixtureRepo) {
			es := r.indexEntries(r.files)
			es = append(es, &index.Entry{Name: "zlib", Mode: filemode.Submodule, Hash: r.head})
			r.writeIndexEntries(es)
		}},
		{"sparse index extension", func(r *fixtureRepo) { writeIndexExt(t, r, "sdir") }},
		{"split index extension", func(r *fixtureRepo) { writeIndexExt(t, r, "link") }},
		{"unknown required extension", func(r *fixtureRepo) { writeIndexExt(t, r, "zzzz") }},
	}
	for i, c := range cases {
		if !run(i) {
			continue
		}
		r := newRepo(t, map[string]fspec{"a": reg("a"), "b.bin": reg("b")})
		c.setup(r)
		before := fingerprint(t, r.root)
		err := pushLocal(t, r.root, env)
		wantRefused(t, err, c.what)
		if fingerprint(t, r.root) != before {
			t.Fatalf("%s: the repository changed", c.what)
		}
	}
	// An optional unknown index extension is ignored, like Git does.
	if !run(len(cases)) {
		return
	}
	r := newRepo(t, map[string]fspec{"a": reg("a")})
	writeIndexExt(t, r, "ZZZZ")
	if err := pushLocal(t, r.root, env); err != nil {
		t.Fatalf("optional extension: %v", err)
	}
	// .gitmodules without a submodule section is an ordinary file.
	r2 := newRepo(t, map[string]fspec{".gitmodules": reg("# none\n")})
	if err := pushLocal(t, r2.root, env); err != nil {
		t.Fatalf("empty .gitmodules: %v", err)
	}
	// Environment redirection is refused unless it names this repository.
	r3 := newRepo(t, map[string]fspec{"a": reg("a")})
	for k, v := range map[string]string{"GIT_DIR": "/elsewhere", "GIT_WORK_TREE": "relative", "GIT_INDEX_FILE": "/tmp/idx",
		"GIT_OBJECT_DIRECTORY": "/o", "GIT_COMMON_DIR": "/c", "GIT_ALTERNATE_OBJECT_DIRECTORIES": "/alt", "GIT_NAMESPACE": "ns"} {
		wantRefused(t, pushLocal(t, r3.root, envOf(map[string]string{"HOME": tempDir(t), "GIT_CONFIG_NOSYSTEM": "1", k: v})), k)
	}
	if err := pushLocal(t, r3.root, envOf(map[string]string{"HOME": tempDir(t), "GIT_CONFIG_NOSYSTEM": "1", "GIT_DIR": filepath.Join(r3.root, ".git"),
		"GIT_WORK_TREE": r3.root})); err != nil {
		t.Fatalf("redirection naming this repository: %v", err)
	}
}

// writeIndexExt appends an extension to r's index.
func writeIndexExt(t *testing.T, r *fixtureRepo, sig string) {
	t.Helper()
	b, _ := os.ReadFile(filepath.Join(r.root, ".git/index"))
	body := append([]byte(nil), b[:len(b)-20]...)
	body = append(body, sig...)
	body = append(body, 0, 0, 0, 4, 1, 2, 3, 4)
	sum := sha1Sum(body)
	os.WriteFile(filepath.Join(r.root, ".git/index"), append(body, sum...), 0o644)
}

// History features: gitlinks and LFS pointers anywhere in the reachable
// history refuse the whole push, and unsafe tree names are refused.
func TestEligibilityHistory(t *testing.T) {
	env := emptyEnv(t)
	pointer := "version https://git-lfs.github.com/spec/v1\noid sha256:" + strings.Repeat("a", 64) + "\nsize 12\n"
	r := newRepo(t, map[string]fspec{"big.bin": reg(pointer)})
	r.commit(map[string]fspec{"a": reg("a")}, "pointer only in history")
	err := pushLocal(t, r.root, env)
	wantRefused(t, err, "LFS pointer in history")
	crlfPointer := strings.ReplaceAll(pointer, "\n", "\r\n")
	for _, p := range []string{crlfPointer, "version https://git-lfs.github.com/spec/v1\next-0-foo sha256:" + strings.Repeat("b", 64) + "\noid sha256:" + strings.Repeat("a", 64) + "\nsize 1\n"} {
		if !isLFSPointer([]byte(p)) {
			t.Fatalf("pointer %q", p)
		}
	}
	for _, p := range []string{"version https://git-lfs.github.com/spec/v1\nsize 1\n", "version https://git-lfs.github.com/spec/v2\noid sha256:x\nsize 1\n",
		"version https://git-lfs.github.com/spec/v1\noid sha256:" + strings.Repeat("a", 64) + "\nsize 1\njunk\n", pointer + strings.Repeat("x", 1024)} {
		if isLFSPointer([]byte(p)) {
			t.Fatalf("not a pointer %q", p)
		}
	}
	// A gitlink in an old commit.
	r2 := newRepo(t, map[string]fspec{"a": reg("a")})
	tree, _ := testkit.WriteTree(r2.store, map[string]testkit.FileSpec{"a": {Mode: filemode.Regular, Content: []byte("a")}})
	_ = tree
	gl := writeTreeEntries(t, r2, []treeEntrySpec{{"a", filemode.Regular, blobHash("a")}, {"sub", filemode.Submodule, r2.head}})
	c1, _ := testkit.WriteCommit(r2.store, gl, []plumbing.Hash{r2.head}, "gitlink", testkit.FixedWhen)
	r2.head = c1
	r2.commit(map[string]fspec{"a": reg("a")}, "after")
	wantRefused(t, pushLocal(t, r2.root, env), "gitlink in history")
	// An unsafe name (".GIT") in history.
	r3 := newRepo(t, map[string]fspec{"a": reg("a")})
	bad := writeTreeEntries(t, r3, []treeEntrySpec{{".GIT", filemode.Regular, blobHash("a")}})
	c3, _ := testkit.WriteCommit(r3.store, bad, []plumbing.Hash{r3.head}, "bad", testkit.FixedWhen)
	r3.head = c3
	r3.commit(map[string]fspec{"a": reg("a")}, "after")
	if err := pushLocal(t, r3.root, env); contract.TransferReason(err) != contract.ReasonUnsafeTree {
		t.Fatalf("unsafe name: %v", err)
	}
	// A missing object is a refusal, not a push.
	r4 := newRepo(t, map[string]fspec{"a": reg("a"), "b": reg("b")})
	os.RemoveAll(filepath.Join(r4.root, ".git/objects", blobHash("b").String()[:2]))
	err = pushLocal(t, r4.root, env)
	if contract.CodeOf(err) != contract.CodeInvalidArgument && contract.CodeOf(err) != contract.CodeConflict {
		t.Fatalf("missing object: %v", err)
	}
}

type treeEntrySpec struct {
	name string
	mode filemode.FileMode
	hash plumbing.Hash
}

func blobHash(s string) plumbing.Hash { return plumbing.ComputeHash(plumbing.BlobObject, []byte(s)) }

// writeTreeEntries stores a raw tree (no validation) in r.
func writeTreeEntries(t *testing.T, r *fixtureRepo, es []treeEntrySpec) plumbing.Hash {
	t.Helper()
	var buf []byte
	for _, e := range es {
		if e.mode != filemode.Submodule {
			if _, err := testkit.WriteRaw(r.store, plumbing.BlobObject, []byte("a")); err != nil {
				t.Fatal(err)
			}
		}
		buf = append(buf, []byte(strings.TrimLeft(e.mode.String(), "0")+" "+e.name+"\x00")...)
		buf = append(buf, e.hash[:]...)
	}
	h, err := testkit.WriteRaw(r.store, plumbing.TreeObject, buf)
	if err != nil {
		t.Fatal(err)
	}
	return h
}
