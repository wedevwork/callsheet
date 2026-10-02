//go:build linux || darwin

package function

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/go-git/go-billy/v5/osfs"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/cache"
	"github.com/go-git/go-git/v5/plumbing/filemode"
	"github.com/go-git/go-git/v5/plumbing/format/index"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/go-git/go-git/v5/storage/filesystem"
	"github.com/go-git/go-git/v5/storage/memory"

	"github.com/wedevwork/callsheet/internal/contract"
	"github.com/wedevwork/callsheet/internal/testkit"
)

// Iteration 09b function tests: one top-level test per FP (FP-1..FP-10),
// in FP order, through the real CLI binary and the real stdio MCP server
// against a real verified-TLS plane subprocess. Fixture repositories are
// built with go-git in-process (no git executable). Package-local
// contracts that need failure seams or hooks are delegated to the
// compiled internal/workspacetransfer test binary.

const transferPkg = "./internal/workspacetransfer"

// trEnv is a plane with workspace "ws" and its instance.
type trEnv struct {
	*wsEnv
	inst string
}

func startTransfer(t *testing.T, env []string) *trEnv {
	t.Helper()
	e := &trEnv{wsEnv: startWs(t, "", env)}
	var v contract.WorkspaceView
	e.jsonCLI(&v, "ws", "create", "ws")
	e.inst = v.Instance
	return e
}

// push runs ws push of path (flags first) and returns the outcome.
func (e *trEnv) push(path string, flags ...string) result {
	e.t.Helper()
	args := append([]string{"ws", "push", "--instance", e.inst}, flags...)
	args = append(args, "ws")
	if path != "" {
		args = append(args, path)
	}
	return e.run(args...)
}

func (e *trEnv) pushJSON(path string, flags ...string) contract.WorkspacePushResult {
	e.t.Helper()
	r := e.push(path, append([]string{"--json"}, flags...)...)
	if r.code != 0 {
		e.t.Fatalf("push %s: %+v", path, r)
	}
	res, err := contract.ParseWorkspacePushResult([]byte(strings.TrimSuffix(r.stdout, "\n")))
	if err != nil {
		e.t.Fatalf("push result %q: %v", r.stdout, err)
	}
	return res
}

func (e *trEnv) pullJSON(ref, path string) contract.WorkspacePullResult {
	e.t.Helper()
	r := e.run("ws", "pull", "--json", "ws", ref, path)
	if r.code != 0 {
		e.t.Fatalf("pull %s %s: %+v", ref, path, r)
	}
	res, err := contract.ParseWorkspacePullResult([]byte(strings.TrimSuffix(r.stdout, "\n")))
	if err != nil {
		e.t.Fatalf("pull result %q: %v", r.stdout, err)
	}
	return res
}

// fetchCommit fetches commit's closure from the plane into memory.
func (e *trEnv) fetchCommit(commit string) (*memory.Storage, *object.Commit) {
	e.t.Helper()
	s := testkit.NewMemoryStore()
	h := plumbing.NewHash(commit)
	if err := e.remote("ws", "").Fetch(context.Background(), s, []plumbing.Hash{h}, nil); err != nil {
		e.t.Fatal(err)
	}
	c, err := object.GetCommit(s, h)
	if err != nil {
		e.t.Fatal(err)
	}
	return s, c
}

// wantExit requires exit code and a stderr fragment.
func wantExit(t *testing.T, r result, code int, frag string) {
	t.Helper()
	if r.code != code || r.stdout != "" || !strings.Contains(r.stderr, frag) {
		t.Fatalf("want exit %d with %q, got %+v", code, frag, r)
	}
}

// ---- go-git fixture repositories ----

type tfile struct {
	mode    filemode.FileMode
	content string
}

func tr(s string) tfile   { return tfile{filemode.Regular, s} }
func tx(s string) tfile   { return tfile{filemode.Executable, s} }
func tlnk(s string) tfile { return tfile{filemode.Symlink, s} }

type fixRepo struct {
	t     *testing.T
	root  string
	store *filesystem.Storage
	head  plumbing.Hash
	files map[string]tfile
}

func canonTemp(t *testing.T) string {
	t.Helper()
	d, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return d
}

func mkRepo(t *testing.T, files map[string]tfile) *fixRepo {
	t.Helper()
	root := filepath.Join(canonTemp(t), "repo")
	for _, d := range []string{".git/objects/pack", ".git/refs/heads", ".git/info"} {
		os.MkdirAll(filepath.Join(root, d), 0o755)
	}
	os.WriteFile(filepath.Join(root, ".git/HEAD"), []byte("ref: refs/heads/main\n"), 0o644)
	os.WriteFile(filepath.Join(root, ".git/config"), []byte("[core]\n\trepositoryformatversion = 0\n\tfilemode = true\n\tbare = false\n"), 0o644)
	r := &fixRepo{t: t, root: root, files: map[string]tfile{}}
	r.store = filesystem.NewStorage(osfs.New(filepath.Join(root, ".git")), cache.NewObjectLRUDefault())
	t.Cleanup(func() { r.store.Close() })
	if files != nil {
		r.commit(files, "initial")
	}
	return r
}

func (r *fixRepo) write(p string, f tfile) {
	r.t.Helper()
	full := filepath.Join(r.root, filepath.FromSlash(p))
	os.MkdirAll(filepath.Dir(full), 0o755)
	os.Remove(full)
	var err error
	switch f.mode {
	case filemode.Symlink:
		err = os.Symlink(f.content, full)
	case filemode.Executable:
		err = os.WriteFile(full, []byte(f.content), 0o755)
		os.Chmod(full, 0o755)
	default:
		err = os.WriteFile(full, []byte(f.content), 0o644)
		os.Chmod(full, 0o644)
	}
	if err != nil {
		r.t.Fatal(err)
	}
}

func specs(files map[string]tfile) map[string]testkit.FileSpec {
	out := map[string]testkit.FileSpec{}
	for p, f := range files {
		out[p] = testkit.FileSpec{Mode: f.mode, Content: []byte(f.content)}
	}
	return out
}

// commit makes files the committed state: objects, a commit on main
// (message msg, parent the previous head), worktree and index.
func (r *fixRepo) commit(files map[string]tfile, msg string) plumbing.Hash {
	r.t.Helper()
	tree, err := testkit.WriteTree(r.store, specs(files))
	if err != nil {
		r.t.Fatal(err)
	}
	var parents []plumbing.Hash
	if !r.head.IsZero() {
		parents = []plumbing.Hash{r.head}
	}
	h, err := testkit.WriteCommit(r.store, tree, parents, msg, testkit.FixedWhen)
	if err != nil {
		r.t.Fatal(err)
	}
	for p := range r.files {
		if _, ok := files[p]; !ok {
			os.Remove(filepath.Join(r.root, filepath.FromSlash(p)))
		}
	}
	for p, f := range files {
		r.write(p, f)
	}
	r.files, r.head = files, h
	os.WriteFile(filepath.Join(r.root, ".git/refs/heads/main"), []byte(h.String()+"\n"), 0o644)
	r.index(files)
	return h
}

func (r *fixRepo) index(files map[string]tfile) {
	r.t.Helper()
	var es []*index.Entry
	for p, f := range files {
		es = append(es, &index.Entry{Name: p, Mode: f.mode, Hash: plumbing.ComputeHash(plumbing.BlobObject, []byte(f.content))})
	}
	sort.Slice(es, func(i, j int) bool { return es[i].Name < es[j].Name })
	var b bytes.Buffer
	if err := index.NewEncoder(&b).Encode(&index.Index{Version: 2, Entries: es}); err != nil {
		r.t.Fatal(err)
	}
	os.WriteFile(filepath.Join(r.root, ".git/index"), b.Bytes(), 0o644)
}

func (r *fixRepo) config(s string) {
	f, _ := os.OpenFile(filepath.Join(r.root, ".git/config"), os.O_APPEND|os.O_WRONLY, 0)
	f.WriteString(s)
	f.Close()
}

// treeDigest fingerprints a directory: every path's type, mode bits,
// content digest and link target.
func treeDigest(t *testing.T, root string, skip ...string) string {
	t.Helper()
	var out []string
	filepath.Walk(root, func(p string, fi os.FileInfo, err error) error {
		if err != nil {
			return nil
		}
		rel, _ := filepath.Rel(root, p)
		for _, s := range skip {
			if rel == s || strings.HasPrefix(rel, s+"/") {
				if fi.IsDir() {
					return filepath.SkipDir
				}
				return nil
			}
		}
		line := rel + " " + fi.Mode().String()
		switch {
		case fi.Mode().IsRegular():
			b, _ := os.ReadFile(p)
			line += fmt.Sprintf(" %x", sha256.Sum256(b))
		case fi.Mode()&os.ModeSymlink != 0:
			l, _ := os.Readlink(p)
			line += " -> " + l
		}
		out = append(out, line)
		return nil
	})
	return strings.Join(out, "\n")
}

func mkFolder(t *testing.T, files map[string]tfile) string {
	t.Helper()
	root := filepath.Join(canonTemp(t), "folder")
	os.MkdirAll(root, 0o755)
	r := &fixRepo{t: t, root: root}
	for p, f := range files {
		r.write(p, f)
	}
	return root
}

// treeFiles flattens a commit into path -> file.
func treeFiles(t *testing.T, s *memory.Storage, c *object.Commit) map[string]tfile {
	t.Helper()
	files, err := testkit.TreeFiles(s, c.Hash)
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]tfile{}
	for p, f := range files {
		out[p] = tfile{f.Mode, string(f.Content)}
	}
	return out
}

func sameTFiles(a, b map[string]tfile) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if b[k] != v {
			return false
		}
	}
	return true
}

// ---- FP-1 ----

// FP-1: a clean repository's exact HEAD and history reach main (default)
// or a named branch; a same-tip push is a changed=false CAS; a
// non-fast-forward is refused with guidance and a stale instance
// conflicts, with no unintended ref movement.
func TestWorkspacePushGit(t *testing.T) {
	t.Parallel() // independent fixtures; overlaps the package's long parallel tests (CI headroom)
	e := startTransfer(t, nil)
	r := mkRepo(t, map[string]tfile{"a.txt": tr("one\n"), "old.log": tr("tracked, later ignored\n"), "bin/run": tx("#!/bin/sh\n"), "l": tlnk("a.txt")})
	first := r.head
	files2 := map[string]tfile{"a.txt": tr("two\n"), "old.log": tr("tracked, later ignored\n"), "bin/run": tx("#!/bin/sh\n"), "l": tlnk("a.txt"), ".gitignore": tr("*.log\n")}
	second := r.commit(files2, "second commit")
	before := treeDigest(t, r.root)
	res := e.pushJSON(r.root)
	if res.Commit != second.String() || res.OldCommit != nil || !res.Changed || res.Branch != "refs/heads/main" || res.SourceKind != "git" {
		t.Fatalf("push %+v", res)
	}
	if treeDigest(t, r.root) != before {
		t.Fatal("the source repository changed")
	}
	s, c := e.fetchCommit(res.Commit)
	if len(c.ParentHashes) != 1 || c.ParentHashes[0] != first || c.Message != "second commit" || c.Author.Name != "Callsheet Fixture" ||
		!sameTFiles(treeFiles(t, s, c), files2) {
		t.Fatalf("history or content rewritten: %+v", c)
	}
	// Text output and a named branch.
	out := e.push(r.root, "--branch", "topic/x")
	want := "name: ws\ninstance: " + e.inst + "\nbranch: refs/heads/topic/x\nold_commit: none\ncommit: " + second.String() + "\nsource_kind: git\nchanged: true\n"
	if out.code != 0 || out.stdout != want {
		t.Fatalf("named branch %+v", out)
	}
	// Same tip: the same CAS with no new objects.
	same := e.pushJSON(r.root)
	if same.Changed || same.OldCommit == nil || *same.OldCommit != second.String() || same.Commit != second.String() {
		t.Fatalf("same tip %+v", same)
	}
	refs := e.statusRefs("ws")
	if len(refs) != 2 || refs["refs/heads/main"] != second.String() || refs["refs/heads/topic/x"] != second.String() {
		t.Fatalf("refs %v", refs)
	}
	// Non-fast-forward: an unrelated history is refused with guidance.
	other := mkRepo(t, map[string]tfile{"z": tr("unrelated")})
	wantExit(t, other.pushOut(e), 4, "push to a new branch, then explicitly move the target with ws ref set")
	// A stale instance conflicts before any mutation.
	r.commit(map[string]tfile{"a.txt": tr("three\n"), ".gitignore": tr("*.log\n")}, "third")
	stale := e.run("ws", "push", "--instance", strings.Repeat("0", 32), "ws", r.root)
	wantExit(t, stale, 4, "stale workspace instance")
	if after := e.statusRefs("ws"); after["refs/heads/main"] != second.String() || len(after) != 2 {
		t.Fatalf("unintended ref movement %v", after)
	}
	// The fast-forward then succeeds and moves only main.
	ff := e.pushJSON(r.root)
	if !ff.Changed || *ff.OldCommit != second.String() || e.statusRefs("ws")["refs/heads/topic/x"] != second.String() {
		t.Fatalf("fast-forward %+v", ff)
	}
}

func (r *fixRepo) pushOut(e *trEnv) result { return e.push(r.root) }

// ---- FP-2 ----

// FP-2: staged, unstaged and nonignored untracked changes refuse with the
// commit-first message; ignored untracked files, executable bits,
// symlinks and CRLF-clean files pass; the NFD/NFC precompose rules hold
// natively; the index and stored paths are never rewritten.
func TestWorkspacePushCleanliness(t *testing.T) {
	t.Parallel() // independent fixtures; overlaps the package's long parallel tests (CI headroom)
	home := canonTemp(t)
	env := []string{"PATH=" + os.Getenv("PATH"), "GIT_CONFIG_NOSYSTEM=1"}
	e := startTransfer(t, env)
	e.cli.home = home
	e.cli.env = append(e.cli.env, "HOME="+home)
	r := mkRepo(t, map[string]tfile{"a.txt": tr("a\n"), "t.txt": tr("one\ntwo\n"), "run.sh": tx("#!/bin/sh\n"), "ln": tlnk("a.txt"), ".gitignore": tr("*.tmp\n")})
	dirty := func(what string) {
		t.Helper()
		out := e.push(r.root)
		if out.code != 4 || !strings.Contains(out.stderr, contract.DirtyMessage) {
			t.Fatalf("%s: %+v", what, out)
		}
	}
	// Staged: an index entry for a new file.
	r.write("new.txt", tr("n"))
	files := map[string]tfile{"a.txt": tr("a\n"), "t.txt": tr("one\ntwo\n"), "run.sh": tx("#!/bin/sh\n"), "ln": tlnk("a.txt"), ".gitignore": tr("*.tmp\n"), "new.txt": tr("n")}
	r.store.SetEncodedObject(blobOf("n"))
	r.index(files)
	dirty("staged add")
	os.Remove(filepath.Join(r.root, "new.txt"))
	delete(files, "new.txt")
	r.index(files)
	// Unstaged content and untracked files.
	r.write("a.txt", tr("edited\n"))
	dirty("unstaged edit")
	r.write("a.txt", tr("a\n"))
	r.write("untracked.txt", tr("u"))
	dirty("untracked file")
	os.Remove(filepath.Join(r.root, "untracked.txt"))
	// Ignored untracked files, a CRLF working copy with autocrlf and the
	// modes and links are clean.
	r.write("scratch.tmp", tr("ignored"))
	r.write("t.txt", tr("one\r\ntwo\r\n"))
	r.config("[core]\n\tautocrlf = true\n")
	idx, _ := os.ReadFile(filepath.Join(r.root, ".git/index"))
	res := e.pushJSON(r.root)
	if res.Commit != r.head.String() {
		t.Fatalf("clean push %+v", res)
	}
	if after, _ := os.ReadFile(filepath.Join(r.root, ".git/index")); !bytes.Equal(after, idx) {
		t.Fatal("the index was rewritten")
	}
	// Unicode: an NFC index path with the NFD spelling on disk.
	nfc, nfd := "café.txt", "café.txt"
	u := mkRepo(t, map[string]tfile{nfc: tr("x\n")})
	os.Remove(filepath.Join(u.root, nfc))
	os.WriteFile(filepath.Join(u.root, nfd), []byte("x\n"), 0o644)
	darwin := runtime.GOOS == "darwin"
	check := func(what string, clean bool) {
		t.Helper()
		out := e.run("ws", "push", "--instance", e.inst, "--branch", "u", "ws", u.root)
		if clean != (out.code == 0) || (!clean && !strings.Contains(out.stderr, "commit first")) {
			t.Fatalf("%s: want clean=%v, got %+v", what, clean, out)
		}
	}
	check("precompose unset", false)
	u.config("[core]\n\tprecomposeUnicode = true\n")
	check("repository precompose true", darwin)
	u.config("[core]\n\tprecomposeUnicode = false\n")
	check("repository precompose false", false)
	// The global setting applies; the repository setting overrides it.
	os.WriteFile(filepath.Join(home, ".gitconfig"), []byte("[core]\n\tprecomposeUnicode = true\n"), 0o644)
	v := mkRepo(t, map[string]tfile{nfc: tr("x\n")})
	os.Remove(filepath.Join(v.root, nfc))
	os.WriteFile(filepath.Join(v.root, nfd), []byte("x\n"), 0o644)
	out := e.run("ws", "push", "--instance", e.inst, "--branch", "v", "ws", v.root)
	if darwin != (out.code == 0) {
		t.Fatalf("global precompose: %+v", out)
	}
	v.config("[core]\n\tprecomposeUnicode = false\n")
	if out := e.run("ws", "push", "--instance", e.inst, "--branch", "v2", "ws", v.root); out.code != 4 {
		t.Fatalf("repository override: %+v", out)
	}
	// DW1: an NFD index path with an NFD file on disk is dirty on darwin
	// with precompose (a deletion plus an untracked NFC path) and clean on
	// Linux; byte-identical names are clean everywhere.
	w := mkRepo(t, map[string]tfile{nfd: tr("x\n")})
	w.config("[core]\n\tprecomposeUnicode = true\n")
	if out := e.run("ws", "push", "--instance", e.inst, "--branch", "w", "ws", w.root); darwin == (out.code == 0) {
		t.Fatalf("NFD index, NFD disk: %+v", out)
	}
	same := mkRepo(t, map[string]tfile{nfc: tr("x\n")})
	same.config("[core]\n\tprecomposeUnicode = true\n")
	if out := e.run("ws", "push", "--instance", e.inst, "--branch", "same", "ws", same.root); out.code != 0 {
		t.Fatalf("identical bytes: %+v", out)
	}
	if b, _ := os.ReadFile(filepath.Join(u.root, ".git/index")); !bytes.Contains(b, []byte(nfc)) {
		t.Fatal("stored index path rewritten")
	}
}

func blobOf(s string) plumbing.EncodedObject {
	o := &plumbing.MemoryObject{}
	o.SetType(plumbing.BlobObject)
	o.Write([]byte(s))
	return o
}

// ---- FP-3 ----

// FP-3: a plain folder seeds the workspace as a deterministic snapshot,
// an unchanged folder is a no-op, one change makes one child snapshot of
// the observed tip; executables, links and empty directories follow the
// folder rules; the source stays plain; a source failure leaves the hub
// unchanged.
func TestWorkspacePushFolder(t *testing.T) {
	t.Parallel() // independent fixtures; overlaps the package's long parallel tests (CI headroom)
	e := startTransfer(t, nil)
	folder := mkFolder(t, map[string]tfile{"a.txt": tr("alpha\n"), "bin/run": tx("#!/bin/sh\n"), "ln": tlnk("../outside"), "d/e": tr("e")})
	os.MkdirAll(filepath.Join(folder, "empty/dir"), 0o755)
	first := e.pushJSON(folder)
	if first.SourceKind != "folder" || first.OldCommit != nil || !first.Changed {
		t.Fatalf("seed %+v", first)
	}
	s, c := e.fetchCommit(first.Commit)
	sig := object.Signature{Name: "Callsheet", Email: "workspace@callsheet.invalid", When: time.Unix(0, 0).UTC()}
	if len(c.ParentHashes) != 0 || c.Author.Name != sig.Name || c.Author.Email != sig.Email || c.Author.When.Unix() != 0 ||
		c.Committer.Name != sig.Name || c.Message != "Callsheet workspace snapshot\n" || c.PGPSignature != "" {
		t.Fatalf("snapshot metadata %+v", c)
	}
	got := treeFiles(t, s, c)
	if !sameTFiles(got, map[string]tfile{"a.txt": tr("alpha\n"), "bin/run": tx("#!/bin/sh\n"), "ln": tlnk("../outside"), "d/e": tr("e")}) {
		t.Fatalf("snapshot files %v", got)
	}
	if _, err := os.Lstat(filepath.Join(folder, ".git")); err == nil {
		t.Fatal("the source became a repository")
	}
	again := e.pushJSON(folder)
	if again.Changed || again.Commit != first.Commit {
		t.Fatalf("unchanged folder %+v", again)
	}
	os.WriteFile(filepath.Join(folder, "a.txt"), []byte("alpha 2\n"), 0o644)
	next := e.pushJSON(folder)
	_, c2 := e.fetchCommit(next.Commit)
	if !next.Changed || *next.OldCommit != first.Commit || len(c2.ParentHashes) != 1 || c2.ParentHashes[0].String() != first.Commit {
		t.Fatalf("child snapshot %+v %v", next, c2.ParentHashes)
	}
	// A socket or FIFO that is not ignored fails before anything changes.
	syscall.Mkfifo(filepath.Join(folder, "pipe"), 0o644)
	before := e.statusRefs("ws")
	wantExit(t, e.push(folder), 2, "socket, FIFO or device")
	if after := e.statusRefs("ws"); after["refs/heads/main"] != before["refs/heads/main"] {
		t.Fatal("the hub changed")
	}
}

// ---- FP-4 ----

// FP-4: nested folder rules and negation, independence from global
// ignores, git-source global/XDG/info excludes with local precedence,
// ignored tracked content retained, and a matcher corpus through a real
// push.
func TestWorkspaceTransferIgnores(t *testing.T) {
	t.Parallel() // independent fixtures; overlaps the package's long parallel tests (CI headroom)
	home := canonTemp(t)
	e := startTransfer(t, []string{"PATH=" + os.Getenv("PATH"), "GIT_CONFIG_NOSYSTEM=1"})
	e.cli.home = home
	e.cli.env = append(e.cli.env, "HOME="+home)
	folder := mkFolder(t, map[string]tfile{
		".gitignore":       tr("*.log\n!keep.log\nbuild/\nout/*\n!out/keep\nex/\n!ex/keep\n\\#hash\n\\!bang\ntrail   \nx/**/deep\n[[:digit:]]*.num\n"),
		"a.log":            tr("x"),
		"keep.log":         tr("k"),
		"build/b":          tr("x"),
		"out/drop":         tr("x"),
		"out/keep":         tr("k"),
		"ex/keep":          tr("x"),
		"#hash":            tr("x"),
		"!bang":            tr("x"),
		"trail":            tr("x"),
		"x/a/b/deep":       tr("x"),
		"1.num":            tr("x"),
		"a.num":            tr("k"),
		"sub/.gitignore":   tr("!*.log\nlocal\n"),
		"sub/n.log":        tr("k"),
		"sub/local":        tr("x"),
		"other/local":      tr("k"),
		"other/global.txt": tr("k"),
	})
	res := e.pushJSON(folder)
	s, c := e.fetchCommit(res.Commit)
	var names []string
	for p := range treeFiles(t, s, c) {
		names = append(names, p)
	}
	sort.Strings(names)
	want := ".gitignore,a.num,keep.log,other/global.txt,other/local,out/keep,sub/.gitignore,sub/n.log"
	if strings.Join(names, ",") != want {
		t.Fatalf("snapshot %v", names)
	}
	// Global excludes never change a folder snapshot.
	os.MkdirAll(filepath.Join(home, ".config/git"), 0o755)
	os.WriteFile(filepath.Join(home, ".config/git/ignore"), []byte("*.txt\n"), 0o644)
	os.WriteFile(filepath.Join(home, ".gitconfig"), []byte("[core]\n\texcludesFile = ~/.config/git/ignore\n"), 0o644)
	if again := e.pushJSON(folder); again.Changed || again.Commit != res.Commit {
		t.Fatalf("global excludes changed a folder snapshot %+v", again)
	}
	// A git source honors the global file, info/exclude (local wins over
	// global) and keeps ignored tracked files.
	r := mkRepo(t, map[string]tfile{"tracked.log": tr("tracked\n"), ".gitignore": tr("*.log\n")})
	r.write("scratch.txt", tr("global ignore"))
	r.write("keep.txt", tr("re-included locally"))
	os.WriteFile(filepath.Join(r.root, ".git/info/exclude"), []byte("!keep.txt\n"), 0o644)
	wantExit(t, e.push(r.root, "--branch", "g"), 4, "commit first")
	os.Remove(filepath.Join(r.root, "keep.txt"))
	r.write("other.log", tr("ignored untracked"))
	gres := e.pushJSON(r.root, "--branch", "g")
	gs, gc := e.fetchCommit(gres.Commit)
	if treeFiles(t, gs, gc)["tracked.log"].content != "tracked\n" {
		t.Fatal("ignored tracked file dropped")
	}
	// XDG_CONFIG_HOME moves the default global file (no excludesFile).
	os.Remove(filepath.Join(home, ".gitconfig"))
	xdg := canonTemp(t)
	e.cli.env = append(e.cli.env, "XDG_CONFIG_HOME="+xdg)
	wantExit(t, e.push(r.root, "--branch", "g"), 4, "commit first")
	os.MkdirAll(filepath.Join(xdg, "git"), 0o755)
	os.WriteFile(filepath.Join(xdg, "git/ignore"), []byte("*.txt\n"), 0o644)
	if out := e.push(r.root, "--branch", "g"); out.code != 0 {
		t.Fatalf("XDG ignore %+v", out)
	}
	planeContract(t, contractBinary(t, transferPkg), transferPkg, "TestWildmatchCorpus")
}

// ---- FP-5 ----

// FP-5: branch, hash and planted task ref into a dirty existing
// repository: the Callsheet ref and objects are exact; HEAD, index,
// working tree, config, FETCH_HEAD and other refs keep their bytes; an
// existing observation ref moves backwards; the returned commit is the
// user's external push input (no external call).
func TestWorkspacePullGit(t *testing.T) {
	t.Parallel() // independent fixtures; overlaps the package's long parallel tests (CI headroom)
	e := startTransfer(t, nil)
	src := mkRepo(t, map[string]tfile{"a.txt": tr("one\n")})
	first := src.head
	second := src.commit(map[string]tfile{"a.txt": tr("two\n"), "b": tx("x")}, "second")
	e.pushJSON(src.root)
	task := "t_" + strings.Repeat("e", 32)
	e.p.stop(t)
	plantTaskRefs(t, e.p.root, "ws", testkit.NewMemoryStore(), map[string]plumbing.Hash{task: first}, map[string]time.Time{task: testkit.FixedWhen})
	e.p.start(t)
	dst := mkRepo(t, map[string]tfile{"mine": tr("mine")})
	dst.write("mine", tr("uncommitted"))
	dst.write("untracked", tr("u"))
	os.WriteFile(filepath.Join(dst.root, ".git/FETCH_HEAD"), []byte("keep\n"), 0o644)
	before := treeDigest(t, dst.root, ".git/objects", ".git/refs/callsheet")
	for _, c := range []struct {
		ref, local string
		commit     plumbing.Hash
	}{
		{"main", "refs/callsheet/ws/heads/main", second},
		{first.String(), "refs/callsheet/ws/commits/" + first.String(), first},
		{"refs/callsheet/tasks/" + task, "refs/callsheet/ws/tasks/" + task, first},
	} {
		res := e.pullJSON(c.ref, dst.root)
		if res.DestinationKind != "git" || *res.LocalRef != c.local || res.Commit != c.commit.String() || !res.Changed {
			t.Fatalf("%s: %+v", c.ref, res)
		}
		b, _ := os.ReadFile(filepath.Join(dst.root, ".git", c.local))
		if strings.TrimSpace(string(b)) != c.commit.String() {
			t.Fatalf("%s: local ref %q", c.ref, b)
		}
		if _, err := object.GetCommit(dst.store, c.commit); err != nil {
			t.Fatalf("%s: commit not installed: %v", c.ref, err)
		}
	}
	if after := treeDigest(t, dst.root, ".git/objects", ".git/refs/callsheet"); after != before {
		t.Fatalf("destination changed:\n%s\n---\n%s", before, after)
	}
	// Move main back with ref set: the observation ref follows.
	e.ok("ws", "ref", "set", "--instance", e.inst, "--expected", second.String(), "ws", "main", first.String())
	back := e.pullJSON("main", dst.root)
	if !back.Changed || *back.OldCommit != second.String() || back.Commit != first.String() {
		t.Fatalf("backwards %+v", back)
	}
	// Text output names the user's next step input.
	out := e.run("ws", "pull", "ws", "main", dst.root)
	if out.code != 0 || !strings.Contains(out.stdout, "commit: "+first.String()+"\n") || !strings.Contains(out.stdout, "changed: false\n") {
		t.Fatalf("text %+v", out)
	}
	if cfg, _ := os.ReadFile(filepath.Join(dst.root, ".git/config")); bytes.Contains(cfg, []byte("remote")) {
		t.Fatal("a remote was configured")
	}
}

// ---- FP-6 ----

// FP-6: export into an absent and an empty directory with exact bytes,
// modes and links; a nonempty destination untouched; unsafe trees and
// filesystem aliases refused before publication (natively); a destination
// populated during staging preserved (delegated contract).
func TestWorkspacePullFolder(t *testing.T) {
	t.Parallel() // independent fixtures; overlaps the package's long parallel tests (CI headroom)
	e := startTransfer(t, nil)
	files := map[string]tfile{"a.txt": tr("alpha\n"), "bin/run": tx("#!/bin/sh\n"), "links/rel": tlnk("../a.txt"), "links/dangle": tlnk("missing"), "d/e/f": tr("")}
	src := mkRepo(t, files)
	res := e.pushJSON(src.root)
	parent := canonTemp(t)
	check := func(dest string) {
		t.Helper()
		for p, f := range files {
			full := filepath.Join(dest, p)
			fi, err := os.Lstat(full)
			if err != nil {
				t.Fatalf("%s: %v", p, err)
			}
			if f.mode == filemode.Symlink {
				if l, _ := os.Readlink(full); l != f.content {
					t.Fatalf("%s link %q", p, l)
				}
				continue
			}
			b, _ := os.ReadFile(full)
			want := os.FileMode(0o644)
			if f.mode == filemode.Executable {
				want = 0o755
			}
			if string(b) != f.content || fi.Mode().Perm() != want {
				t.Fatalf("%s %q %v", p, b, fi.Mode())
			}
		}
	}
	absent := filepath.Join(parent, "new")
	pr := e.pullJSON("main", absent)
	if pr.DestinationKind != "folder" || pr.LocalRef != nil || pr.OldCommit != nil || !pr.Changed || pr.Commit != res.Commit {
		t.Fatalf("export %+v", pr)
	}
	check(absent)
	empty := filepath.Join(parent, "empty")
	os.Mkdir(empty, 0o755)
	e.pullJSON("main", empty)
	check(empty)
	full := filepath.Join(parent, "full")
	os.Mkdir(full, 0o755)
	os.WriteFile(filepath.Join(full, ".keep"), []byte("mine"), 0o600)
	wantExit(t, e.run("ws", "pull", "ws", "main", full), 4, "destination must be new or empty")
	if b, _ := os.ReadFile(filepath.Join(full, ".keep")); string(b) != "mine" {
		t.Fatal("nonempty destination changed")
	}
	// Unsafe trees: an escaping symlink is refused before publication.
	bad := mkRepo(t, map[string]tfile{"up": tlnk("../../etc/passwd")})
	e.run("ws", "push", "--instance", e.inst, "--branch", "bad", "ws", bad.root)
	wantExit(t, e.run("ws", "pull", "ws", "bad", filepath.Join(parent, "bad")), 2, "pull into a git repository instead")
	if _, err := os.Lstat(filepath.Join(parent, "bad")); err == nil {
		t.Fatal("published an unsafe tree")
	}
	// Case aliases: refused exactly where this filesystem aliases them.
	alias := mkRepo(t, nil)
	objs := map[string]testkit.FileSpec{"README": {Mode: filemode.Regular, Content: []byte("1")}, "readme": {Mode: filemode.Regular, Content: []byte("2")}}
	h, _ := testkit.CommitFiles(alias.store, objs, nil, "aliases")
	g := e.remote("ws", e.inst)
	if r := g.Push(context.Background(), alias.store, "refs/heads/alias", plumbing.ZeroHash, h); !r.OK() {
		t.Fatalf("seed alias %v", r.Err)
	}
	probe := filepath.Join(parent, "probe")
	os.Mkdir(probe, 0o755)
	os.WriteFile(filepath.Join(probe, "A"), nil, 0o644)
	_, err := os.Lstat(filepath.Join(probe, "a"))
	folds := err == nil
	out := e.run("ws", "pull", "ws", "alias", filepath.Join(parent, "alias"))
	if folds {
		wantExit(t, out, 2, "cannot keep apart")
		if _, err := os.Lstat(filepath.Join(parent, "alias")); err == nil {
			t.Fatal("published aliased names")
		}
	} else if out.code != 0 {
		t.Fatalf("case-sensitive filesystem: %+v", out)
	}
	if es, _ := os.ReadDir(parent); slicesContainsPrefix(es, ".callsheet-pull-") {
		t.Fatalf("staging leftovers: %v", es)
	}
	planeContract(t, contractBinary(t, transferPkg), transferPkg, "TestExportConcurrentDestination")
}

// ---- FP-7 ----

// FP-7: usage, defaults, selectors, output shapes and exit codes, then S6
// (seed from the current directory) and S18 (both delivery paths).
func TestWorkspaceTransferCLI(t *testing.T) {
	t.Parallel() // independent fixtures; overlaps the package's long parallel tests (CI headroom)
	e := startTransfer(t, nil)
	// Unborn main and missing workspace.
	dst := canonTemp(t)
	wantExit(t, e.run("ws", "pull", "ws", "main", filepath.Join(dst, "x")), 3, "no such branch")
	wantExit(t, e.run("ws", "pull", "nope", "main", filepath.Join(dst, "x")), 3, "")
	for _, c := range []struct {
		args []string
		code int
	}{
		{[]string{"ws", "push", "ws"}, 2},
		{[]string{"ws", "push", "--instance", e.inst}, 2},
		{[]string{"ws", "push", "--instance", e.inst, "ws", "a", "b"}, 2},
		{[]string{"ws", "push", "--instance", e.inst, "--instance", e.inst, "ws"}, 2},
		{[]string{"ws", "push", "--instance", e.inst, "--branch", "Main", "ws"}, 2},
		{[]string{"ws", "push", "--instance", e.inst, "--force", "ws"}, 2},
		{[]string{"ws", "push", "--instance", e.inst, "ws", ""}, 2},
		{[]string{"ws", "pull", "ws"}, 2},
		{[]string{"ws", "pull", "ws", "main~1"}, 2},
		{[]string{"ws", "pull", "ws", "0123abc"}, 3},
		{[]string{"ws", "pull", "ws", "main", "a", "b"}, 2},
	} {
		if r := e.run(c.args...); r.code != c.code {
			t.Fatalf("%q = %+v", c.args, r)
		}
	}
	// S6: seed from the current directory (PATH omitted).
	folder := mkFolder(t, map[string]tfile{"README.md": tr("# project\n"), "src/main.go": tr("package main\n")})
	e.cli.cwd = folder
	seed := e.run("ws", "push", "--instance", e.inst, "ws")
	if seed.code != 0 || !strings.Contains(seed.stdout, "source_kind: folder\n") || !strings.Contains(seed.stdout, "old_commit: none\n") {
		t.Fatalf("S6 seed %+v", seed)
	}
	commit := ""
	for _, l := range strings.Split(seed.stdout, "\n") {
		if v, ok := strings.CutPrefix(l, "commit: "); ok {
			commit = v
		}
	}
	// S18 (a): into the user's repository, then their own push input.
	repo := mkRepo(t, map[string]tfile{"x": tr("x")})
	e.cli.cwd = repo.root
	in := e.run("ws", "pull", "ws", "main")
	if in.code != 0 || !strings.Contains(in.stdout, "destination_kind: git\nlocal_ref: refs/callsheet/ws/heads/main\n") {
		t.Fatalf("S18 repository %+v", in)
	}
	if _, err := object.GetCommit(repo.store, plumbing.NewHash(commit)); err != nil {
		t.Fatalf("returned commit not local: %v", err)
	}
	// S18 (b): into a new folder given relative to the working directory.
	e.cli.cwd = dst
	outp := e.run("ws", "pull", "--json", "ws", "main", "delivery")
	var pr map[string]any
	if outp.code != 0 || json.Unmarshal([]byte(outp.stdout), &pr) != nil || pr["destination_kind"] != "folder" || pr["local_ref"] != nil || len(pr) != 8 {
		t.Fatalf("S18 folder %+v", outp)
	}
	if b, _ := os.ReadFile(filepath.Join(dst, "delivery/src/main.go")); string(b) != "package main\n" {
		t.Fatal("S18 folder content")
	}
	help := e.cli.run(t, "help", "ws", "pull")
	if help.code != 0 || !strings.Contains(help.stdout, "git push <your-remote> <commit>:refs/heads/<delivery-branch>") {
		t.Fatalf("help %+v", help)
	}
}

// ---- FP-8 ----

// FP-8: discovery and schemas, both tools for git and folders, strict
// errors, paths relative to the server's working directory, no content
// sentinel in any frame, and no owned work left after cancellation or EOF.
func TestWorkspaceTransferMCP(t *testing.T) {
	t.Parallel() // independent fixtures; overlaps the package's long parallel tests (CI headroom)
	tmp := canonTemp(t)
	env := []string{"PATH=" + os.Getenv("PATH"), "TMPDIR=" + tmp}
	e := startTransfer(t, env)
	const sentinel = "SENTINEL-09B-CONTENT"
	list := e.mcp.request("tools/list", nil)
	for _, name := range []string{`"name":"ws_push"`, `"name":"ws_pull"`} {
		if !strings.Contains(list.raw, name) {
			t.Fatalf("discovery lacks %s", name)
		}
	}
	if strings.Count(list.raw, `"inputSchema":`) != 23 || strings.Index(list.raw, `"ws_push"`) > strings.Index(list.raw, `"ws_pull"`) {
		t.Fatal("tool order or count")
	}
	// A git repository with sentinels in a file, a commit message and a
	// symlink target, pushed by an absolute path.
	r := mkRepo(t, map[string]tfile{"secret.txt": tr(sentinel + " file"), "l": tlnk(sentinel + "-target")})
	r.commit(map[string]tfile{"secret.txt": tr(sentinel + " file 2"), "l": tlnk(sentinel + "-target")}, sentinel+" message")
	var pres contract.WorkspacePushResult
	e.mcpJSON(&pres, "ws_push", map[string]any{"name": "ws", "instance": e.inst, "path": r.root})
	if pres.Commit != r.head.String() || pres.SourceKind != "git" {
		t.Fatalf("mcp push %+v", pres)
	}
	// Relative paths resolve against the server's working directory.
	cwd := e.mcp.cmd.Dir
	os.MkdirAll(filepath.Join(cwd, "rel-src"), 0o755)
	os.WriteFile(filepath.Join(cwd, "rel-src", "f"), []byte(sentinel), 0o644)
	e.mcpJSON(&pres, "ws_push", map[string]any{"name": "ws", "instance": e.inst, "branch": "folder", "path": "rel-src"})
	if pres.SourceKind != "folder" {
		t.Fatalf("mcp folder push %+v", pres)
	}
	var lres contract.WorkspacePullResult
	e.mcpJSON(&lres, "ws_pull", map[string]any{"name": "ws", "ref": "folder", "path": "rel-out"})
	if b, _ := os.ReadFile(filepath.Join(cwd, "rel-out", "f")); string(b) != sentinel || lres.DestinationKind != "folder" {
		t.Fatal("relative export")
	}
	dst := mkRepo(t, map[string]tfile{"x": tr("x")})
	e.mcpJSON(&lres, "ws_pull", map[string]any{"name": "ws", "ref": "main", "path": dst.root})
	if lres.LocalRef == nil || *lres.LocalRef != "refs/callsheet/ws/heads/main" {
		t.Fatalf("mcp git pull %+v", lres)
	}
	// Strict errors and refusals carry no sentinel.
	e.mcp.fails("ws_push", map[string]any{"name": "ws", "instance": e.inst, "path": r.root, "force": true}, contract.CodeInvalidArgument)
	e.mcp.fails("ws_push", map[string]any{"name": "ws", "instance": e.inst, "path": ""}, contract.CodeInvalidArgument)
	e.mcp.fails("ws_pull", map[string]any{"name": "ws", "ref": "main", "path": dst.root + "/missing/deeper"}, contract.CodeNotFound)
	r.write("secret.txt", tr(sentinel+" dirty"))
	d := e.mcp.fails("ws_push", map[string]any{"name": "ws", "instance": e.inst, "path": r.root}, contract.CodeConflict)
	if d.Details["reason"] != contract.ReasonDirtySource {
		t.Fatalf("dirty %+v", d)
	}
	e.mcp.fails("ws_pull", map[string]any{"name": "ws", "ref": "main", "path": filepath.Join(cwd, "rel-out")}, contract.CodeConflict)
	e.mcp.mu.Lock()
	all := strings.Join(e.mcp.all, "")
	e.mcp.mu.Unlock()
	if strings.Contains(all, sentinel) || strings.Contains(all, r.root) || strings.Contains(all, cwd) {
		t.Fatal("content, a local path or a sentinel reached an MCP frame")
	}
	// Cancellation: the call's work ends and its temporary store is gone;
	// the session stays usable. EOF: the server exits after joining.
	cancelled, eof := canonTemp(t), canonTemp(t)
	id, _ := e.mcp.callAsync("ws_pull", map[string]any{"name": "ws", "ref": "main", "path": filepath.Join(cancelled, "out")})
	e.mcp.notify("notifications/cancelled", map[string]any{"requestId": json.Number(id)})
	e.mcpJSON(&lres, "ws_pull", map[string]any{"name": "ws", "ref": "main", "path": dst.root})
	e.mcp.callAsync("ws_pull", map[string]any{"name": "ws", "ref": "main", "path": filepath.Join(eof, "out")})
	e.mcp.stdin.Close()
	select {
	case <-e.mcp.exited:
	case <-time.After(mcpWait):
		t.Fatal("the MCP server did not exit on EOF")
	}
	if !e.mcp.state.Success() {
		t.Fatalf("EOF exit %v", e.mcp.state)
	}
	if es, _ := os.ReadDir(tmp); len(es) != 0 {
		var names []string
		for _, x := range es {
			names = append(names, x.Name())
		}
		t.Fatalf("owned temporary work left: %v", names)
	}
	// Each destination holds either nothing or the complete export, and
	// never a staging directory.
	for _, parent := range []string{cancelled, eof} {
		es, _ := os.ReadDir(parent)
		if slicesContainsPrefix(es, ".callsheet-pull-") || len(es) > 1 {
			t.Fatalf("%s: %v", parent, es)
		}
		if len(es) == 1 {
			if b, err := os.ReadFile(filepath.Join(parent, "out", "secret.txt")); err != nil || !bytes.HasPrefix(b, []byte(sentinel)) {
				t.Fatalf("partial export in %s", parent)
			}
		}
	}
}

func slicesContainsPrefix(es []os.DirEntry, prefix string) bool {
	for _, e := range es {
		if strings.HasPrefix(e.Name(), prefix) {
			return true
		}
	}
	return false
}

// ---- FP-9 ----

// FP-9: with PATH an empty directory and no git executable reachable,
// the absolute-path binaries push a repository, refuse a dirty one,
// snapshot an ignored folder, pull into a repository and a folder, and
// serve both MCP tools.
func TestWorkspaceTransferNoGit(t *testing.T) {
	nodeBinary(t)
	empty := testkit.EmptyDir(t)
	t.Setenv("PATH", empty)
	if _, err := exec.LookPath("git"); err == nil {
		t.Fatal("git is reachable")
	}
	e := startTransfer(t, []string{"PATH=" + empty})
	r := mkRepo(t, map[string]tfile{"a": tr("a"), ".gitignore": tr("*.tmp\n")})
	if res := e.pushJSON(r.root); res.Commit != r.head.String() {
		t.Fatalf("push %+v", res)
	}
	r.write("a", tr("dirty"))
	wantExit(t, e.push(r.root), 4, "commit first")
	r.write("a", tr("a"))
	folder := mkFolder(t, map[string]tfile{"f": tr("f"), "x.tmp": tr("ignored"), ".gitignore": tr("*.tmp\n")})
	fres := e.pushJSON(folder, "--branch", "folder")
	s, c := e.fetchCommit(fres.Commit)
	if _, ok := treeFiles(t, s, c)["x.tmp"]; ok {
		t.Fatal("ignored file snapshotted")
	}
	dst := mkRepo(t, map[string]tfile{"x": tr("x")})
	if res := e.pullJSON("main", dst.root); res.Commit != r.head.String() {
		t.Fatalf("pull %+v", res)
	}
	out := filepath.Join(canonTemp(t), "out")
	e.pullJSON("folder", out)
	if b, _ := os.ReadFile(filepath.Join(out, "f")); string(b) != "f" {
		t.Fatal("export")
	}
	var pres contract.WorkspacePushResult
	e.mcpJSON(&pres, "ws_push", map[string]any{"name": "ws", "instance": e.inst, "path": r.root})
	var lres contract.WorkspacePullResult
	e.mcpJSON(&lres, "ws_pull", map[string]any{"name": "ws", "ref": "folder", "path": filepath.Join(canonTemp(t), "mcp-out")})
	if pres.Changed || lres.Commit != fres.Commit {
		t.Fatalf("mcp %+v %+v", pres, lres)
	}
}

// ---- FP-10 ----

// FP-10: worktrees, bare, shallow, submodule, LFS and nested-repository
// layouts are refused as unsupported_repository through the CLI and MCP,
// never as a dirty source or a folder; nothing changes.
func TestWorkspaceTransferEligibility(t *testing.T) {
	t.Parallel() // independent fixtures; overlaps the package's long parallel tests (CI headroom)
	e := startTransfer(t, nil)
	cases := map[string]string{}
	add := func(name, path string) { cases[name] = path }
	wt := filepath.Join(canonTemp(t), "worktree")
	os.MkdirAll(wt, 0o755)
	os.WriteFile(filepath.Join(wt, ".git"), []byte("gitdir: /elsewhere/.git/worktrees/x\n"), 0o644)
	add("worktree", wt)
	b := mkRepo(t, map[string]tfile{"a": tr("a")})
	add("bare", filepath.Join(b.root, ".git"))
	sh := mkRepo(t, map[string]tfile{"a": tr("a")})
	os.WriteFile(filepath.Join(sh.root, ".git/shallow"), []byte(sh.head.String()+"\n"), 0o644)
	add("shallow", sh.root)
	sub := mkRepo(t, map[string]tfile{"a": tr("a"), ".gitmodules": tr("[submodule \"lib\"]\n\tpath = lib\n\turl = x\n")})
	add("submodule", sub.root)
	lfs := mkRepo(t, map[string]tfile{"a.bin": tr("version https://git-lfs.github.com/spec/v1\noid sha256:" + strings.Repeat("a", 64) + "\nsize 3\n"),
		".gitattributes": tr("*.bin filter=lfs diff=lfs merge=lfs -text\n")})
	lfs.config("[filter \"lfs\"]\n\tclean = git-lfs clean -- %f\n\tsmudge = git-lfs smudge -- %f\n")
	add("lfs", lfs.root)
	pointer := mkRepo(t, map[string]tfile{"a.bin": tr("version https://git-lfs.github.com/spec/v1\noid sha256:" + strings.Repeat("b", 64) + "\nsize 3\n")})
	add("lfs pointer in history", pointer.root)
	nested := mkRepo(t, map[string]tfile{"a": tr("a")})
	inner := filepath.Join(nested.root, "vendor", "inner", ".git")
	os.MkdirAll(filepath.Join(inner, "objects"), 0o755)
	os.MkdirAll(filepath.Join(inner, "refs"), 0o755)
	os.WriteFile(filepath.Join(inner, "HEAD"), []byte("ref: refs/heads/main\n"), 0o644)
	add("nested repository", nested.root)
	nestedFolder := mkFolder(t, map[string]tfile{"a": tr("a")})
	os.MkdirAll(filepath.Join(nestedFolder, "sub", ".git"), 0o755)
	add("nested repository in a folder", nestedFolder)
	os.MkdirAll(filepath.Join(b.root, "sub"), 0o755)
	add("subdirectory", filepath.Join(b.root, "sub"))
	os.MkdirAll(filepath.Join(nested.root, "vendor"), 0o755)
	add("inside a repository", filepath.Join(nested.root, "vendor"))
	for name, path := range cases {
		before := treeDigest(t, filepath.Dir(path))
		out := e.push(path)
		if out.code != 2 || !strings.Contains(out.stderr, "invalid_argument") || strings.Contains(out.stderr, "commit first") {
			t.Fatalf("%s: CLI %+v", name, out)
		}
		d := e.mcp.fails("ws_push", map[string]any{"name": "ws", "instance": e.inst, "path": path}, contract.CodeInvalidArgument)
		if d.Details["reason"] != contract.ReasonUnsupportedRepository {
			t.Fatalf("%s: MCP %+v", name, d)
		}
		if name != "nested repository in a folder" && name != "nested repository" && name != "lfs pointer in history" {
			if p := e.run("ws", "pull", "ws", "main", path); p.code != 2 {
				t.Fatalf("%s: pull destination %+v", name, p)
			}
		}
		if after := treeDigest(t, filepath.Dir(path)); after != before {
			t.Fatalf("%s: changed", name)
		}
	}
	// Code review r1 C9: an active LFS filter routed by a nested
	// .gitattributes (its file not even a pointer) refuses a pull into
	// that repository through the CLI and MCP; nothing changes.
	nl := mkRepo(t, map[string]tfile{"a": tr("a"), "sub/.gitattributes": tr("*.dat filter=lfs diff=lfs merge=lfs -text\n"), "sub/x.dat": tr("not a pointer\n")})
	nl.config("[filter \"lfs\"]\n\tclean = git-lfs clean -- %f\n\tsmudge = git-lfs smudge -- %f\n\trequired = true\n")
	before := treeDigest(t, nl.root)
	if p := e.run("ws", "pull", "ws", "main", nl.root); p.code != 2 || !strings.Contains(p.stderr, "LFS") {
		t.Fatalf("nested LFS attributes: CLI pull %+v", p)
	}
	d := e.mcp.fails("ws_pull", map[string]any{"name": "ws", "ref": "main", "path": nl.root}, contract.CodeInvalidArgument)
	if d.Details["reason"] != contract.ReasonUnsupportedRepository {
		t.Fatalf("nested LFS attributes: MCP pull %+v", d)
	}
	if treeDigest(t, nl.root) != before {
		t.Fatal("nested LFS attributes: the destination changed")
	}
	if refs := e.statusRefs("ws"); len(refs) != 0 {
		t.Fatalf("hub changed: %v", refs)
	}
}
