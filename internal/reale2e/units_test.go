package reale2e

import (
	"bytes"
	"compress/zlib"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	git "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/filemode"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/go-git/go-git/v5/storage/memory"
)

// TestEvidenceWriter (UT-6, UT-7): a new private bundle, strictly
// increasing events with non-decreasing times, atomic never-overwriting
// files, and sticky write failures.
func TestEvidenceWriter(t *testing.T) {
	t.Parallel()
	root := filepath.Join(t.TempDir(), "run")
	clock := newAutoClock()
	e, err := newEvidenceWriter(root, clock)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := newEvidenceWriter(root, clock); err == nil {
		t.Fatal("an existing bundle was reused")
	}
	a := e.event(Event{Type: EvRunStarted})
	clock.advance(-time.Hour)
	b := e.event(Event{Type: EvPreflightStarted})
	if a.Seq != 1 || b.Seq != 2 || b.Time != a.Time {
		t.Fatalf("events %+v %+v", a, b)
	}
	if err := e.writeFile("a/b.txt", []byte("x")); err != nil {
		t.Fatal(err)
	}
	if err := e.writeFile("a/b.txt", []byte("y")); !errors.Is(err, errExists) || e.failure() == nil {
		t.Fatalf("overwrite = %v, sticky %v", err, e.failure())
	}
	if got, _ := os.ReadFile(filepath.Join(root, "a", "b.txt")); string(got) != "x" {
		t.Fatal("a completed file was replaced")
	}
	if err := e.writeJSON("bad.json", func() {}); err == nil {
		t.Fatal("an unencodable document was written")
	}
	if es, _ := os.ReadDir(filepath.Join(root, "a")); len(es) != 1 {
		t.Fatalf("temporary files left: %v", es)
	}
	e.close()
	// A closed events file makes later events a sticky failure.
	e2, _ := newEvidenceWriter(filepath.Join(t.TempDir(), "r2"), clock)
	e2.close()
	e2.event(Event{Type: EvRunStarted})
	if e2.failure() == nil {
		t.Fatal("a failed event write was not recorded")
	}
	// Unwritable parents.
	ro := t.TempDir()
	os.WriteFile(filepath.Join(ro, "file"), nil, 0o600)
	if err := writeAtomic(ro, "file/x", nil); err == nil {
		t.Fatal("wrote below a file")
	}
	if _, err := newEvidenceWriter(filepath.Join(ro, "file", "x"), clock); err == nil {
		t.Fatal("created below a file")
	}
	if err := os.Chmod(ro, 0o500); err == nil && os.Getuid() != 0 {
		defer os.Chmod(ro, 0o700)
		if err := writeAtomic(ro, "y", nil); err == nil {
			t.Fatal("wrote into a read-only directory")
		}
		if _, err := newEvidenceWriter(filepath.Join(ro, "z"), clock); err == nil {
			t.Fatal("created in a read-only directory")
		}
	}
	if _, ok := parseTime("2026-10-10T12:00:00+02:00"); ok {
		t.Fatal("a non-UTC time parsed")
	}
}

// TestRepositoryUnits (UT-6): object closure and the evidence repository's
// writer and reader refuse what they cannot represent or trust.
func TestRepositoryUnits(t *testing.T) {
	t.Parallel()
	st := memory.NewStorage()
	seed, _ := writeCommit(t, st, map[string][]byte{"a": []byte("1")})
	if _, err := closure(st, []plumbing.Hash{plumbing.NewHash(zeros40)}); err == nil {
		t.Fatal("a missing commit closed")
	}
	// A tree with a submodule or a missing blob or subtree.
	for name, entry := range map[string]object.TreeEntry{
		"submodule":    {Name: "sub", Mode: filemode.Submodule, Hash: seed},
		"missing blob": {Name: "b", Mode: filemode.Regular, Hash: plumbing.NewHash(strings.Repeat("1", 40))},
		"missing tree": {Name: "d", Mode: filemode.Dir, Hash: plumbing.NewHash(strings.Repeat("2", 40))},
	} {
		tree := &object.Tree{Entries: []object.TreeEntry{entry}}
		to := st.NewEncodedObject()
		tree.Encode(to)
		th, _ := st.SetEncodedObject(to)
		c := &object.Commit{TreeHash: th, Message: name}
		co := st.NewEncodedObject()
		c.Encode(co)
		ch, _ := st.SetEncodedObject(co)
		if _, err := closure(st, []plumbing.Hash{ch}); err == nil {
			t.Errorf("%s closed", name)
		}
		if err := writeEvidenceRepo(filepath.Join(t.TempDir(), "r.git"), st, []plumbing.Hash{ch}); err == nil {
			t.Errorf("%s written", name)
		}
	}
	c := &object.Commit{TreeHash: plumbing.NewHash(strings.Repeat("3", 40)), Message: "bad tree"}
	co := st.NewEncodedObject()
	c.Encode(co)
	bad, _ := st.SetEncodedObject(co)
	if _, err := closure(st, []plumbing.Hash{bad}); err == nil {
		t.Fatal("a commit without its tree closed")
	}
	if err := writeEvidenceRepo(t.TempDir(), st, nil); err == nil {
		t.Fatal("an empty commit list was written")
	}
	if err := writeEvidenceRepo(t.TempDir(), st, make([]plumbing.Hash, 6)); err == nil {
		t.Fatal("six commits were written")
	}
	file := filepath.Join(t.TempDir(), "f")
	os.WriteFile(file, nil, 0o600)
	if err := writeEvidenceRepo(filepath.Join(file, "r.git"), st, []plumbing.Hash{seed}); err == nil {
		t.Fatal("a repository was created below a file")
	}
	dst := memory.NewStorage()
	if err := copyObjects(st, dst, []plumbing.Hash{plumbing.NewHash(zeros40)}); err == nil {
		t.Fatal("a missing root was copied")
	}
	// The reader: a repository holding a seed and one result.
	dir := filepath.Join(t.TempDir(), "repo.git")
	h1, _ := writeCommit(t, st, map[string][]byte{"a": []byte("2")}, seed)
	if err := writeEvidenceRepo(dir, st, []plumbing.Hash{seed, h1}); err != nil {
		t.Fatal(err)
	}
	v, err := openEvidenceRepo(dir)
	if err != nil {
		t.Fatal(err)
	}
	if c, ok, err := v.commit("refs/heads/hop-01"); !ok || err != nil || c.Hash != h1 {
		t.Fatalf("hop-01 = %v %v %v", c, ok, err)
	}
	if _, ok, _ := v.commit("refs/heads/hop-02"); ok {
		t.Fatal("an absent ref resolved")
	}
	v.refs["refs/heads/hop-02"] = plumbing.NewHash(strings.Repeat("4", 40))
	if _, ok, err := v.commit("refs/heads/hop-02"); !ok || err == nil {
		t.Fatal("a missing commit resolved")
	}
	// A symbolic non-HEAD ref, and HEAD as a hash.
	os.WriteFile(filepath.Join(dir, "refs", "heads", "hop-02"), []byte("ref: refs/heads/seed\n"), 0o600)
	if _, err := openEvidenceRepo(dir); err == nil {
		t.Fatal("a symbolic hop ref opened")
	}
	os.Remove(filepath.Join(dir, "refs", "heads", "hop-02"))
	os.WriteFile(filepath.Join(dir, "HEAD"), []byte(seed.String()+"\n"), 0o600)
	if _, err := openEvidenceRepo(dir); err == nil {
		t.Fatal("a detached HEAD opened")
	}
	os.Remove(filepath.Join(dir, "config"))
	os.Remove(filepath.Join(dir, "HEAD"))
	os.RemoveAll(filepath.Join(dir, "refs"))
	if _, err := openEvidenceRepo(dir); err == nil {
		t.Fatal("a repository without metadata opened")
	}
	if _, err := validateRepoLayout(filepath.Join(t.TempDir(), "missing"), 10); err == nil {
		t.Fatal("a missing root validated")
	}
	// Loose object reader corner cases.
	odir := t.TempDir()
	write := func(raw []byte) string {
		var z bytes.Buffer
		w := zlib.NewWriter(&z)
		w.Write(raw)
		w.Close()
		p := filepath.Join(odir, "o")
		os.WriteFile(p, z.Bytes(), 0o600)
		return p
	}
	for name, raw := range map[string][]byte{
		"no terminator": []byte("blob 3"),
		"long header":   []byte(strings.Repeat("x", 40) + "\x00"),
		"bad size":      []byte("blob x\x00"),
		"short body":    []byte("blob 5\x00ab"),
	} {
		if _, err := verifyLooseObject(write(raw), zeros40, 100); err == nil {
			t.Errorf("%s verified", name)
		}
	}
	os.WriteFile(filepath.Join(odir, "plain"), []byte("not zlib"), 0o600)
	if _, err := verifyLooseObject(filepath.Join(odir, "plain"), zeros40, 100); err == nil {
		t.Fatal("a non-zlib object verified")
	}
	if _, err := verifyLooseObject(filepath.Join(odir, "missing"), zeros40, 100); err == nil {
		t.Fatal("a missing object verified")
	}
	// Tree files: a symlink entry is not a project file; a bound applies.
	blob := st.NewEncodedObject()
	blob.SetType(plumbing.BlobObject)
	bw, _ := blob.Writer()
	bw.Write([]byte("target"))
	bw.Close()
	bh, _ := st.SetEncodedObject(blob)
	lt := &object.Tree{Entries: []object.TreeEntry{{Name: "l", Mode: filemode.Symlink, Hash: bh}}}
	lo := st.NewEncodedObject()
	lt.Encode(lo)
	lh, _ := st.SetEncodedObject(lo)
	link, err := object.GetTree(st, lh)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := treeFiles(link, 100); err == nil {
		t.Fatal("a symlink entry was read")
	}
	sc, _ := object.GetCommit(st, seed)
	tr, _ := sc.Tree()
	if _, err := treeFiles(tr, 0); !errors.Is(err, errObjectBound) {
		t.Fatalf("tree bound = %v", err)
	}
}

// TestSeedUnits (UT-2): the seed and test exports refuse unexpected files
// and report unusable directories or tools.
func TestSeedUnits(t *testing.T) {
	t.Parallel()
	f := newFakeWorld(t)
	files := seedFiles("1.26.0")
	run, _, _, err := runGoTests(runnerOf(f.launcher, f.clock), fakeGo, "/bin", t.TempDir(), withFiles(files, map[string][]byte{"evil.sh": []byte("x")}), "c", "t")
	if err == nil || !strings.Contains(err.Error(), "evil.sh") || run.Toolchain != "" {
		t.Fatalf("unexpected file = %v", err)
	}
	blocked := filepath.Join(t.TempDir(), "blocked")
	os.WriteFile(blocked, nil, 0o600)
	if _, _, _, err := runGoTests(runnerOf(f.launcher, f.clock), fakeGo, "/bin", blocked, files, "c", "t"); err == nil {
		t.Fatal("exported below a file")
	}
	start := &fakeLauncher{script: func(s ProcSpec) behavior { return behavior{startErr: errors.New("no go")} }}
	if _, _, _, err := runGoTests(runnerOf(start, f.clock), fakeGo, "/bin", t.TempDir(), files, "c", "t"); err == nil {
		t.Fatal("an unstartable go ran")
	}
	n := 0
	second := &fakeLauncher{script: func(s ProcSpec) behavior {
		n++
		if n == 2 {
			return behavior{startErr: errors.New("no test")}
		}
		return behavior{stdout: "go version x\n"}
	}}
	if _, _, _, err := runGoTests(runnerOf(second, f.clock), fakeGo, "/bin", t.TempDir(), files, "c", "t"); err == nil {
		t.Fatal("an unstartable go test ran")
	}
	var spec ProcSpec
	ok := &fakeLauncher{script: func(s ProcSpec) behavior { spec = s; return behavior{stdout: "go version go1\n"} }}
	dir := t.TempDir()
	run, _, _, err = runGoTests(runnerOf(ok, f.clock), fakeGo, "/bin", dir, files, "c", "t")
	if err != nil || run.Toolchain != "go version go1" || spec.Dir != filepath.Join(dir, "src") || !strings.Contains(strings.Join(spec.Env, " "), "GOCACHE="+dir) {
		t.Fatalf("run %+v spec %+v %v", run, spec, err)
	}
	for _, e := range []string{"GOTOOLCHAIN=local", "GOPROXY=off", "GOSUMDB=off", "GOWORK=off"} {
		if !strings.Contains(strings.Join(run.Env, " "), e) {
			t.Fatalf("env %v lacks %s", run.Env, e)
		}
	}
	// createSeed refuses a directory it cannot initialize or fill.
	if _, err := createSeed(blocked, files); err == nil {
		t.Fatal("seeded a file")
	}
	ro := t.TempDir()
	if _, err := git.PlainInit(ro, false); err != nil {
		t.Fatal(err)
	}
	if _, err := createSeed(ro, files); err == nil {
		t.Fatal("seeded an existing repository")
	}
	bad := map[string][]byte{"sub/x": []byte("x")}
	if _, err := createSeed(t.TempDir(), bad); err == nil {
		t.Fatal("seeded a file in a missing directory")
	}
}

// TestInlineFiles (UT-4): the code reviewer's labelled files parse only
// when framed, hashed and unique.
func TestInlineFiles(t *testing.T) {
	t.Parallel()
	block := func(p, body string) string {
		return fileBlockBegin(p, sha256Hex([]byte(body))) + body + fileBlockEnd(p)
	}
	got, err := parseInlineFiles("intro\n" + block("a.go", "x\n") + block("b.go", ""))
	if err != nil || string(got["a.go"]) != "x\n" || len(got["b.go"]) != 0 || len(got) != 2 {
		t.Fatalf("parse = %v %v", got, err)
	}
	for name, goal := range map[string]string{
		"unterminated header": "----- BEGIN FILE a.go sha256=" + zeros64,
		"no hash":             "----- BEGIN FILE a.go -----\nx\n----- END FILE a.go -----\n",
		"short hash":          "----- BEGIN FILE a.go sha256=abc -----\nx\n----- END FILE a.go -----\n",
		"no end":              fileBlockBegin("a.go", sha256Hex([]byte("x"))) + "x",
		"duplicate":           block("a.go", "x") + block("a.go", "x"),
		"hash mismatch":       fileBlockBegin("a.go", zeros64) + "x" + fileBlockEnd("a.go"),
	} {
		if _, err := parseInlineFiles(goal); err == nil {
			t.Errorf("%s parsed", name)
		}
	}
}

// TestCoordinatorSetupFailures (UT-3): a prompt template with an unknown
// placeholder or an unusable control socket stops before the coordinator
// is given anything.
func TestCoordinatorSetupFailures(t *testing.T) {
	t.Parallel()
	f := newFakeWorld(t)
	os.WriteFile(filepath.Join(f.checkout, filepath.FromSlash(ExamplesDir), CoordinatorPrompt), []byte("use {{SECRET}}\n"), 0o644)
	if code := f.attempt(); code != 1 || f.sup.failCode != "templates_invalid" {
		t.Fatalf("attempt = %d %q", code, f.sup.failCode)
	}
	f = newFakeWorld(t)
	f.override = func(s ProcSpec) (behavior, bool) {
		if s.Path == fakeGo && len(s.Args) > 0 && s.Args[0] == "test" && f.sup != nil && f.sup.runtime != "" {
			os.Mkdir(filepath.Join(f.sup.runtime, SocketName), 0o700)
		}
		return behavior{}, false
	}
	if code := f.attempt(); code != 1 || f.sup.failCode != "control_socket_failed" {
		t.Fatalf("attempt = %d %q", code, f.sup.failCode)
	}
	if strings.Contains(f.stdout.String(), "ready for the coordinator") {
		t.Fatal("launch instructions after a failed setup")
	}
}

// runnerOf is a commandRunner over runCommand without a supervisor.
func runnerOf(l Launcher, c Clock) commandRunner {
	return func(spec ProcSpec, timeout time.Duration, limit int) (CommandResult, error) {
		return runCommand(l, c, spec, timeout, limit)
	}
}
