package workspacetransfer

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/filemode"

	"github.com/wedevwork/callsheet/internal/contract"
	"github.com/wedevwork/callsheet/internal/testkit"
)

// UT-6: folder export.

// exportFixture is a temporary store holding one commit of files.
type exportFixture struct {
	temp   *tempStore
	commit plumbing.Hash
}

func newExportFixture(t *testing.T, files map[string]testkit.FileSpec) *exportFixture {
	t.Helper()
	temp, err := fastDeps().newTempStore()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(temp.close)
	c, err := testkit.CommitFiles(temp.store, files, nil, "export")
	if err != nil {
		t.Fatal(err)
	}
	return &exportFixture{temp: temp, commit: c}
}

// rawCommit stores a commit of a raw (unvalidated) tree.
func (f *exportFixture) rawCommit(t *testing.T, entries []treeEntrySpec, blobs ...string) plumbing.Hash {
	t.Helper()
	for _, b := range blobs {
		testkit.WriteRaw(f.temp.store, plumbing.BlobObject, []byte(b))
	}
	var buf []byte
	for _, e := range entries {
		buf = append(buf, []byte(strings.TrimLeft(e.mode.String(), "0")+" "+e.name+"\x00")...)
		buf = append(buf, e.hash[:]...)
	}
	tree, _ := testkit.WriteRaw(f.temp.store, plumbing.TreeObject, buf)
	c, _ := testkit.WriteCommit(f.temp.store, tree, nil, "raw", testkit.FixedWhen)
	return c
}

// export runs the export of commit to dest for goos with d.
func (f *exportFixture) export(t *testing.T, d *deps, goos, dest string, commit plumbing.Hash) error {
	t.Helper()
	e, err := prepareExport(dest)
	if err != nil {
		return err
	}
	defer e.close()
	entries, err := collectTree(context.Background(), goos, f.temp.store, commit, e)
	if err != nil {
		return err
	}
	return d.export(context.Background(), goos, f.temp.store, entries, e)
}

func staging(t *testing.T, parent string) []string {
	t.Helper()
	var out []string
	es, _ := os.ReadDir(parent)
	for _, e := range es {
		if strings.HasPrefix(e.Name(), stagingPrefix) {
			out = append(out, e.Name())
		}
	}
	return out
}

var exportFiles = map[string]testkit.FileSpec{
	"a.txt":         {Mode: filemode.Regular, Content: []byte("alpha\n")},
	"bin/run":       {Mode: filemode.Executable, Content: []byte("#!/bin/sh\n")},
	"legacy":        {Mode: filemode.Deprecated, Content: []byte("664")},
	"links/rel":     {Mode: filemode.Symlink, Content: []byte("../a.txt")},
	"links/dangle":  {Mode: filemode.Symlink, Content: []byte("missing/inside")},
	"links/chain":   {Mode: filemode.Symlink, Content: []byte("rel")},
	"links/dirlink": {Mode: filemode.Symlink, Content: []byte("../bin")},
	"links/through": {Mode: filemode.Symlink, Content: []byte("dirlink/run")},
	"deep/x/y/z":    {Mode: filemode.Regular, Content: []byte("z")},
}

func TestExportNewAndEmpty(t *testing.T) {
	f := newExportFixture(t, exportFiles)
	old := umask(0o077)
	defer umask(old)
	parent := tempDir(t)
	dest := filepath.Join(parent, "out")
	if err := f.export(t, fastDeps(), hostGOOS(), dest, f.commit); err != nil {
		t.Fatal(err)
	}
	check := func(dest string, rootPerm os.FileMode) {
		t.Helper()
		for p, spec := range exportFiles {
			full := filepath.Join(dest, p)
			fi, err := os.Lstat(full)
			if err != nil {
				t.Fatalf("%s: %v", p, err)
			}
			switch spec.Mode {
			case filemode.Symlink:
				l, _ := os.Readlink(full)
				if fi.Mode()&os.ModeSymlink == 0 || l != string(spec.Content) {
					t.Fatalf("%s link %v %q", p, fi.Mode(), l)
				}
			default:
				b, _ := os.ReadFile(full)
				want := os.FileMode(0o644)
				if spec.Mode == filemode.Executable {
					want = 0o755
				}
				if string(b) != string(spec.Content) || fi.Mode().Perm() != want {
					t.Fatalf("%s: %q %v", p, b, fi.Mode())
				}
			}
		}
		for _, d := range []string{"bin", "deep/x/y"} {
			if fi, _ := os.Stat(filepath.Join(dest, d)); fi.Mode().Perm() != 0o755 {
				t.Fatalf("%s mode %v", d, fi.Mode())
			}
		}
		if fi, _ := os.Stat(dest); fi.Mode().Perm() != rootPerm {
			t.Fatalf("root mode %v", fi.Mode())
		}
		if s := staging(t, filepath.Dir(dest)); len(s) != 0 {
			t.Fatalf("staging left %v", s)
		}
	}
	check(dest, 0o755)
	empty := filepath.Join(parent, "empty")
	os.Mkdir(empty, 0o710)
	os.Chmod(empty, 0o710)
	if err := f.export(t, fastDeps(), hostGOOS(), empty, f.commit); err != nil {
		t.Fatal(err)
	}
	check(empty, 0o710)
}

func umask(m int) int { return unixUmask(m) }

func TestExportRefusals(t *testing.T) {
	f := newExportFixture(t, exportFiles)
	parent := tempDir(t)
	// Nonempty (a hidden file counts), a file, a symlink, a missing parent
	// and the filesystem root.
	full := filepath.Join(parent, "full")
	os.Mkdir(full, 0o755)
	os.WriteFile(filepath.Join(full, ".hidden"), []byte("keep"), 0o600)
	if err := f.export(t, fastDeps(), hostGOOS(), full, f.commit); contract.TransferReason(err) != contract.ReasonDestinationNotEmpty {
		t.Fatalf("nonempty: %v", err)
	}
	if b, _ := os.ReadFile(filepath.Join(full, ".hidden")); string(b) != "keep" {
		t.Fatal("nonempty destination changed")
	}
	file := filepath.Join(parent, "file")
	os.WriteFile(file, nil, 0o644)
	sym := filepath.Join(parent, "sym")
	os.Symlink(parent, sym)
	for _, p := range []string{file, sym, "/"} {
		if err := f.export(t, fastDeps(), hostGOOS(), p, f.commit); contract.CodeOf(err) != contract.CodeInvalidArgument {
			t.Fatalf("%s: %v", p, err)
		}
	}
	if err := f.export(t, fastDeps(), hostGOOS(), filepath.Join(parent, "no/such/out"), f.commit); contract.CodeOf(err) != contract.CodeNotFound {
		t.Fatalf("missing parent: %v", err)
	}
	// Inside a repository is refused (use the repository root).
	r := newRepo(t, map[string]fspec{"x": reg("x")})
	if err := f.export(t, fastDeps(), hostGOOS(), filepath.Join(r.root, "out"), f.commit); contract.TransferReason(err) != contract.ReasonUnsupportedRepository {
		t.Fatalf("inside a repository: %v", err)
	}
	// A symlinked parent is resolved once.
	if err := f.export(t, fastDeps(), hostGOOS(), filepath.Join(sym, "via-link"), f.commit); err != nil {
		t.Fatalf("symlinked parent: %v", err)
	}
	if _, err := os.Lstat(filepath.Join(parent, "via-link", "a.txt")); err != nil {
		t.Fatal("not exported through the canonical parent")
	}
}

func TestExportUnsafeTrees(t *testing.T) {
	f := newExportFixture(t, exportFiles)
	blob := blobHash("x")
	link := func(target string) plumbing.Hash { return blobHash(target) }
	parent := tempDir(t)
	run := sampler(t)
	cases := []struct {
		what    string
		goos    string
		entries []treeEntrySpec
		blobs   []string
	}{
		{".git", "linux", []treeEntrySpec{{".git", filemode.Regular, blob}}, []string{"x"}},
		{".GIT", "linux", []treeEntrySpec{{".GIT", filemode.Regular, blob}}, []string{"x"}},
		{"HFS .git alias on darwin", "darwin", []treeEntrySpec{{".g‌it", filemode.Regular, blob}}, []string{"x"}},
		{"dot-dot", "linux", []treeEntrySpec{{"..", filemode.Regular, blob}}, []string{"x"}},
		{"duplicate", "linux", []treeEntrySpec{{"a", filemode.Regular, blob}, {"a", filemode.Regular, blob}}, []string{"x"}},
		{"gitlink", "linux", []treeEntrySpec{{"m", filemode.Submodule, blob}}, nil},
		{"too long a name", "linux", []treeEntrySpec{{strings.Repeat("n", 256), filemode.Regular, blob}}, []string{"x"}},
		{"absolute link", "linux", []treeEntrySpec{{"l", filemode.Symlink, link("/etc/passwd")}}, []string{"/etc/passwd"}},
		{"escaping link", "linux", []treeEntrySpec{{"l", filemode.Symlink, link("../x")}}, []string{"../x"}},
		{"escape through a missing name", "linux", []treeEntrySpec{{"l", filemode.Symlink, link("m/../../x")}}, []string{"m/../../x"}},
		{"empty link", "linux", []treeEntrySpec{{"l", filemode.Symlink, link("")}}, []string{""}},
		{"cycle", "linux", []treeEntrySpec{{"a", filemode.Symlink, link("b")}, {"b", filemode.Symlink, link("a")}}, []string{"a", "b"}},
		{"through a file", "linux", []treeEntrySpec{{"f", filemode.Regular, blob}, {"l", filemode.Symlink, link("f/x")}}, []string{"x", "f/x"}},
		{"escape via a link to a parent", "linux", []treeEntrySpec{{"up", filemode.Symlink, link("..")}}, []string{".."}},
	}
	for i, c := range cases {
		if !run(i) {
			continue
		}
		commit := f.rawCommit(t, c.entries, c.blobs...)
		dest := filepath.Join(parent, "u")
		err := f.export(t, fastDeps(), c.goos, dest, commit)
		if contract.TransferReason(err) != contract.ReasonUnsafeTree {
			t.Fatalf("%s: %v", c.what, err)
		}
		if _, err := os.Lstat(dest); err == nil || len(staging(t, parent)) != 0 {
			t.Fatalf("%s: something was written", c.what)
		}
	}
	// The HFS alias is an ordinary name on Linux; a long total path is
	// refused on darwin's limit.
	if !run(len(cases)) {
		return
	}
	ok := f.rawCommit(t, []treeEntrySpec{{".g‌it", filemode.Regular, blob}}, "x")
	if err := f.export(t, fastDeps(), "linux", filepath.Join(parent, "hfs"), ok); err != nil {
		t.Fatalf("HFS alias on linux: %v", err)
	}
	var deep []string
	for i := 0; i < 5; i++ {
		deep = append(deep, strings.Repeat("p", 200))
	}
	long := newExportFixture(t, map[string]testkit.FileSpec{strings.Join(deep, "/"): {Mode: filemode.Regular, Content: []byte("x")}})
	e, err := prepareExport(filepath.Join(parent, "long"))
	if err != nil {
		t.Fatal(err)
	}
	defer e.close()
	if _, err := collectTree(context.Background(), "darwin", long.temp.store, long.commit, e); contract.TransferReason(err) != contract.ReasonUnsafeTree {
		t.Fatalf("darwin path limit: %v", err)
	}
	if _, err := collectTree(context.Background(), "linux", long.temp.store, long.commit, e); err != nil {
		t.Fatalf("linux path limit: %v", err)
	}
}

// Exclusive creation catches names the filesystem aliases: a repeated
// path (what a case-insensitive or normalizing filesystem makes of two
// distinct names) collides instead of being reused.
func TestExportCollisions(t *testing.T) {
	f := newExportFixture(t, exportFiles)
	parent := tempDir(t)
	for _, entries := range [][]exportEntry{
		{{path: "d", mode: filemode.Dir}, {path: "d", mode: filemode.Dir}},
		{{path: "a", mode: filemode.Regular, hash: blobHash("alpha\n")}, {path: "a", mode: filemode.Regular, hash: blobHash("alpha\n")}},
		{{path: "a", mode: filemode.Regular, hash: blobHash("alpha\n")}, {path: "a", mode: filemode.Symlink, hash: blobHash("../a.txt")}},
	} {
		testkit.WriteRaw(f.temp.store, plumbing.BlobObject, []byte("alpha\n"))
		e, err := prepareExport(filepath.Join(parent, "c"))
		if err != nil {
			t.Fatal(err)
		}
		err = fastDeps().export(context.Background(), hostGOOS(), f.temp.store, entries, e)
		e.close()
		if contract.TransferReason(err) != contract.ReasonUnsafeTree {
			t.Fatalf("%v: %v", entries, err)
		}
		if _, err := os.Lstat(filepath.Join(parent, "c")); err == nil || len(staging(t, parent)) != 0 {
			t.Fatal("published or staging left")
		}
	}
	// Natively: case and Unicode normalization aliases collide exactly
	// when this filesystem aliases them.
	probe := filepath.Join(parent, "probe")
	os.Mkdir(probe, 0o755)
	os.WriteFile(filepath.Join(probe, "A"), nil, 0o644)
	_, err := os.Lstat(filepath.Join(probe, "a"))
	caseFolds := err == nil
	os.WriteFile(filepath.Join(probe, "café"), nil, 0o644)
	_, err = os.Lstat(filepath.Join(probe, "café"))
	normalizes := err == nil
	for _, c := range []struct {
		names   [2]string
		aliases bool
	}{{[2]string{"A/x", "a/y"}, caseFolds}, {[2]string{"README", "readme"}, caseFolds}, {[2]string{"café", "café"}, normalizes}} {
		fx := newExportFixture(t, map[string]testkit.FileSpec{c.names[0]: {Mode: filemode.Regular, Content: []byte("1")}, c.names[1]: {Mode: filemode.Regular, Content: []byte("2")}})
		dest := filepath.Join(parent, "n")
		os.RemoveAll(dest)
		err := fx.export(t, fastDeps(), hostGOOS(), dest, fx.commit)
		if c.aliases {
			if contract.TransferReason(err) != contract.ReasonUnsafeTree {
				t.Fatalf("%v on an aliasing filesystem: %v", c.names, err)
			}
			if _, err := os.Lstat(dest); err == nil {
				t.Fatal("published")
			}
		} else if err != nil {
			t.Fatalf("%v on a byte-exact filesystem: %v", c.names, err)
		}
	}
}

func TestExportFailures(t *testing.T) {
	f := newExportFixture(t, exportFiles)
	parent := tempDir(t)
	run := sampler(t)
	n := 0
	for _, op := range []string{"export-mkdir", "export-create", "export-write", "export-sync", "export-close", "export-symlink", "export-dirsync", "export-rename"} {
		for _, existing := range []bool{false, true} {
			n++
			if !run(n - 1) {
				continue
			}
			dest := filepath.Join(parent, "d")
			os.RemoveAll(dest)
			if existing {
				os.Mkdir(dest, 0o755)
			}
			d := fastDeps()
			d.fail = func(o string) error {
				if o == op {
					return errors.New("injected")
				}
				return nil
			}
			err := f.export(t, d, hostGOOS(), dest, f.commit)
			if contract.TransferReason(err) != contract.ReasonStorageFailure {
				t.Fatalf("%s: %v", op, err)
			}
			if existing {
				if es, err := os.ReadDir(dest); err != nil || len(es) != 0 {
					t.Fatalf("%s: existing empty destination changed", op)
				}
			} else if _, err := os.Lstat(dest); err == nil {
				t.Fatalf("%s: destination created", op)
			}
			if s := staging(t, parent); len(s) != 0 {
				t.Fatalf("%s: staging left", op)
			}
		}
	}
	// After the rename a failed parent sync is ambiguous and the complete
	// tree is present.
	if !run(n) {
		return
	}
	dest := filepath.Join(parent, "post")
	d := fastDeps()
	d.fail = func(o string) error {
		if o == "export-parent-sync" {
			return errors.New("injected")
		}
		return nil
	}
	err := f.export(t, d, hostGOOS(), dest, f.commit)
	if contract.CodeOf(err) != contract.CodeInternal || !strings.Contains(err.Error(), "inspect the destination") {
		t.Fatalf("post-rename: %v", err)
	}
	if b, _ := os.ReadFile(filepath.Join(dest, "deep/x/y/z")); string(b) != "z" {
		t.Fatal("complete tree not present")
	}
}

// A destination populated or created while staging is never replaced.
func TestExportConcurrentDestination(t *testing.T) {
	f := newExportFixture(t, exportFiles)
	parent := tempDir(t)
	empty := filepath.Join(parent, "empty")
	os.Mkdir(empty, 0o755)
	d := fastDeps()
	d.hook = func(s string) {
		if s == "export-staged" {
			os.WriteFile(filepath.Join(empty, "late"), []byte("mine"), 0o600)
		}
	}
	if err := f.export(t, d, hostGOOS(), empty, f.commit); contract.TransferReason(err) != contract.ReasonDestinationNotEmpty {
		t.Fatalf("populated: %v", err)
	}
	if b, _ := os.ReadFile(filepath.Join(empty, "late")); string(b) != "mine" {
		t.Fatal("concurrent content lost")
	}
	absent := filepath.Join(parent, "absent")
	d.hook = func(s string) {
		if s == "export-staged" {
			os.Mkdir(absent, 0o755)
			os.WriteFile(filepath.Join(absent, "theirs"), []byte("t"), 0o600)
		}
	}
	if err := f.export(t, d, hostGOOS(), absent, f.commit); contract.TransferReason(err) != contract.ReasonDestinationNotEmpty {
		t.Fatalf("appeared: %v", err)
	}
	if b, _ := os.ReadFile(filepath.Join(absent, "theirs")); string(b) != "t" {
		t.Fatal("appeared destination replaced")
	}
	// The raw publication race: the no-replace rename itself refuses an
	// existing name, and the replacing rename refuses a populated one.
	if staging(t, parent) != nil {
		t.Fatal("staging left")
	}
	if publishFailure("linux", errUnixEEXIST) == nil || contract.TransferReason(publishFailure("darwin", errUnixENOTEMPTY)) != contract.ReasonDestinationNotEmpty {
		t.Fatal("publish failure mapping")
	}
	for goos, e := range map[string]error{"linux": errUnixEINVAL, "darwin": errUnixENOTSUP} {
		if err := publishFailure(goos, e); contract.TransferReason(err) != contract.ReasonStorageFailure || !strings.Contains(err.Error(), "atomic no-replace") {
			t.Fatalf("%s unsupported: %v", goos, err)
		}
	}
	if err := publishFailure("linux", errUnixEXDEV); !strings.Contains(err.Error(), "mount point") {
		t.Fatalf("EXDEV: %v", err)
	}
	if err := publishFailure("darwin", errors.New("other")); contract.TransferReason(err) != contract.ReasonStorageFailure {
		t.Fatalf("other: %v", err)
	}
}

// A full pull into a folder through the production plane: bytes, modes
// and links; a repeat into the published folder is refused; a relative
// path resolves against the working directory.
func TestPullFolder(t *testing.T) {
	// Deterministic: once per CPU setting under a repeated run.
	if !sampler(t)(0) {
		return
	}
	tp := sharedPlane(t)
	env := emptyEnv(t)
	parent := tempDir(t)
	res, err := pullWith(t, fastDeps(), tp.plane(t), env, "main", filepath.Join(parent, "out"))
	if err != nil || res.DestinationKind != contract.KindFolder || res.LocalRef != nil || !res.Changed || res.Validate() != nil {
		t.Fatalf("%+v %v", res, err)
	}
	if b, _ := os.ReadFile(filepath.Join(parent, "out/a.txt")); string(b) != "two\n" {
		t.Fatal("content")
	}
	if l, _ := os.Readlink(filepath.Join(parent, "out/l")); l != "a.txt" {
		t.Fatal("link")
	}
	if fi, _ := os.Stat(filepath.Join(parent, "out/bin/run")); fi.Mode().Perm() != 0o755 {
		t.Fatal("executable")
	}
	if _, err := pullWith(t, fastDeps(), tp.plane(t), env, "main", filepath.Join(parent, "out")); contract.TransferReason(err) != contract.ReasonDestinationNotEmpty {
		t.Fatalf("repeat into the published folder: %v", err)
	}
	if _, err := fastDeps().pull(context.Background(), Options{GOOS: hostGOOS(), Env: env, Cwd: parent, Plane: tp.plane(t)},
		PullRequest{Name: "ws", Ref: "topic", Path: "rel", PathSet: true}); err != nil {
		t.Fatalf("relative path: %v", err)
	}
	if _, err := os.Lstat(filepath.Join(parent, "rel/t")); err != nil {
		t.Fatal("relative path resolved against cwd")
	}
}
