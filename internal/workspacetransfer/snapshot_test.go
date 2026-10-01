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
	"github.com/go-git/go-git/v5/plumbing/object"
	"golang.org/x/sys/unix"

	"github.com/wedevwork/callsheet/internal/contract"
	"github.com/wedevwork/callsheet/internal/testkit"
)

// UT-3: plain-folder snapshots.

// snap scans folder into a fresh temporary store.
func snap(t *testing.T, d *deps, folder string) (plumbing.Hash, *tempStore, error) {
	t.Helper()
	temp, err := d.newTempStore()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(temp.close)
	fd, err := openPathDir(folder)
	if err != nil {
		t.Fatal(err)
	}
	defer closeFD(fd)
	tree, _, err := d.snapshotFolder(context.Background(), fd, &looseDB{d: d, objFD: temp.objFD, opPref: "snapshot-"})
	return tree, temp, err
}

// writeFolder materializes files (with exact permission bits for
// regular files) under a new folder.
func writeFolder(t testing.TB, files map[string]fspec, perms map[string]os.FileMode) string {
	t.Helper()
	root := filepath.Join(tempDir(t), "folder")
	os.MkdirAll(root, 0o755)
	r := &fixtureRepo{t: t, root: root}
	for p, f := range files {
		r.writeFile(p, f)
	}
	for p, m := range perms {
		os.Chmod(filepath.Join(root, p), m)
	}
	return root
}

func TestSnapshotDeterministic(t *testing.T) {
	d := fastDeps()
	files := map[string]fspec{"a.txt": reg("alpha\n"), "bin/run": exe("#!/bin/sh\n"), "ln": link("../outside"), "d/e/f": reg("")}
	// Raw non-UTF-8 names are stored as bytes where the filesystem can
	// hold them (APFS refuses them).
	if err := os.WriteFile(filepath.Join(tempDir(t), "\xffraw"), nil, 0o644); err == nil {
		files["\xffraw"] = reg("non-UTF-8 name")
	}
	folder := writeFolder(t, files, map[string]os.FileMode{"bin/run": 0o701})
	os.MkdirAll(filepath.Join(folder, "empty/nested"), 0o755)
	tree, temp, err := snap(t, d, folder)
	if err != nil {
		t.Fatal(err)
	}
	specs := map[string]testkit.FileSpec{}
	for p, f := range files {
		specs[p] = testkit.FileSpec{Mode: f.mode, Content: []byte(f.content)}
	}
	want, _ := testkit.WriteTree(testkit.NewMemoryStore(), specs)
	if tree != want {
		t.Fatalf("tree %s, want %s", tree, want)
	}
	got, err := testkit.TreeFiles(temp.store, mustCommit(t, temp, tree, plumbing.ZeroHash))
	if err != nil || testkit.EqualFiles(got, specs) != nil {
		t.Fatalf("stored files: %v %v", err, testkit.EqualFiles(got, specs))
	}
	// The same folder elsewhere, scanned again, gives the same tree; the
	// commit identity is fixed.
	again := writeFolder(t, files, map[string]os.FileMode{"bin/run": 0o701})
	tree2, _, err := snap(t, d, again)
	if err != nil || tree2 != tree {
		t.Fatalf("second scan %s %v", tree2, err)
	}
	c := mustCommit(t, temp, tree, plumbing.ZeroHash)
	raw := "tree " + tree.String() + "\nauthor Callsheet <workspace@callsheet.invalid> 0 +0000\ncommitter Callsheet <workspace@callsheet.invalid> 0 +0000\n\nCallsheet workspace snapshot\n"
	if c != plumbing.ComputeHash(plumbing.CommitObject, []byte(raw)) {
		t.Fatal("snapshot commit identity")
	}
	parent := plumbing.NewHash(strings.Repeat("1", 40))
	withParent := mustCommit(t, temp, tree, parent)
	rawP := "tree " + tree.String() + "\nparent " + parent.String() + raw[len("tree ")+40:]
	if withParent != plumbing.ComputeHash(plumbing.CommitObject, []byte(rawP)) || withParent == c {
		t.Fatal("parented snapshot identity")
	}
	// An empty folder is the empty tree.
	empty := filepath.Join(tempDir(t), "empty")
	os.MkdirAll(filepath.Join(empty, "only/dirs"), 0o755)
	et, _, err := snap(t, d, empty)
	if err != nil || et.String() != "4b825dc642cb6eb9a060e54bf8d69288fbee4904" {
		t.Fatalf("empty tree %s %v", et, err)
	}
}

func mustCommit(t *testing.T, temp *tempStore, tree, parent plumbing.Hash) plumbing.Hash {
	t.Helper()
	db := &looseDB{d: fastDeps(), objFD: temp.objFD}
	h, err := db.writeObject(context.Background(), snapshotCommit(tree, parent))
	if err != nil {
		t.Fatal(err)
	}
	return h
}

func TestSnapshotFilesystemSemantics(t *testing.T) {
	d := fastDeps()
	folder := writeFolder(t, map[string]fspec{"a": reg("same"), ".gitignore": reg("*.sock\nignored-dir/\n")}, nil)
	os.Link(filepath.Join(folder, "a"), filepath.Join(folder, "hard"))
	unix.Mkfifo(filepath.Join(folder, "x.sock"), 0o644)
	os.MkdirAll(filepath.Join(folder, "ignored-dir/.git"), 0o755)
	unix.Mkfifo(filepath.Join(folder, "ignored-dir/fifo"), 0o644)
	tree, temp, err := snap(t, d, folder)
	if err != nil {
		t.Fatalf("hard links, ignored FIFO and ignored nested repository: %v", err)
	}
	tr, _ := object.GetTree(temp.store, tree)
	var names []string
	for _, e := range tr.Entries {
		names = append(names, e.Name)
	}
	if strings.Join(names, ",") != ".gitignore,a,hard" || tr.Entries[1].Hash != tr.Entries[2].Hash {
		t.Fatalf("entries %v", names)
	}
	// A nonignored FIFO is refused before anything is published.
	unix.Mkfifo(filepath.Join(folder, "pipe"), 0o644)
	if _, _, err := snap(t, d, folder); contract.TransferReason(err) != contract.ReasonUnsafeTree {
		t.Fatalf("FIFO: %v", err)
	}
	os.Remove(filepath.Join(folder, "pipe"))
	// Any nonignored .git (any case, any depth) is a nested repository.
	for _, p := range []string{"sub/.git", "sub/.GIT"} {
		os.MkdirAll(filepath.Join(folder, p), 0o755)
		if _, _, err := snap(t, d, folder); contract.TransferReason(err) != contract.ReasonUnsupportedRepository {
			t.Fatalf("%s: %v", p, err)
		}
		os.RemoveAll(filepath.Join(folder, "sub"))
	}
	// A symlinked .gitignore is content, not rules.
	f2 := writeFolder(t, map[string]fspec{"rules": reg("*.x\n"), "f.x": reg("x")}, nil)
	os.Symlink("rules", filepath.Join(f2, ".gitignore"))
	tree2, temp2, err := snap(t, d, f2)
	if err != nil {
		t.Fatal(err)
	}
	t2, _ := object.GetTree(temp2.store, tree2)
	if len(t2.Entries) != 3 || t2.Entries[0].Mode != filemode.Symlink {
		t.Fatalf("symlinked .gitignore %v", t2.Entries)
	}
}

func TestSnapshotIgnoreRules(t *testing.T) {
	d := fastDeps()
	folder := writeFolder(t, map[string]fspec{
		".gitignore":         reg("*.log\n!keep.log\nbuild/\ncache/*\n!cache/keep\nout/\n!out/keep\n.gitignore\n"),
		"a.log":              reg("x"),
		"keep.log":           reg("k"),
		"build/x":            reg("x"),
		"cache/drop":         reg("x"),
		"cache/keep":         reg("k"),
		"out/keep":           reg("x"),
		"sub/.gitignore":     reg("!*.log\nlocal\n"),
		"sub/n.log":          reg("n"),
		"sub/local":          reg("x"),
		"sub/deeper/y.log":   reg("y"),
		"sub/deeper/local":   reg("x"),
		"other/local":        reg("kept"),
		"other/nested/k.txt": reg("k"),
	}, nil)
	tree, temp, err := snap(t, d, folder)
	if err != nil {
		t.Fatal(err)
	}
	files, _ := testkit.TreeFiles(temp.store, mustCommit(t, temp, tree, plumbing.ZeroHash))
	// ".gitignore" ignores every .gitignore as payload, yet each is read
	// as rules; global excludes never apply to folders.
	want := []string{"cache/keep", "keep.log", "other/local", "other/nested/k.txt", "sub/deeper/y.log", "sub/n.log"}
	if strings.Join(sortedKeys(files), ",") != strings.Join(want, ",") {
		t.Fatalf("snapshot %v", sortedKeys(files))
	}
}

func TestSnapshotFailures(t *testing.T) {
	folder := writeFolder(t, map[string]fspec{"a": reg("alpha"), "b": reg("beta")}, nil)
	for _, op := range []string{"snapshot-object-create", "snapshot-object-write", "snapshot-object-close", "snapshot-object-rename"} {
		d := fastDeps()
		d.fail = func(o string) error {
			if o == op {
				return unix.ENOSPC
			}
			return nil
		}
		_, temp, err := snap(t, d, folder)
		if contract.TransferReason(err) != contract.ReasonStorageFailure || !strings.Contains(err.Error(), "disk is full") {
			t.Fatalf("%s: %v", op, err)
		}
		entries, _ := os.ReadDir(filepath.Join(temp.dir, "objects"))
		for _, e := range entries {
			if strings.HasPrefix(e.Name(), "tmp_obj_") {
				t.Fatalf("%s: temporary object left", op)
			}
		}
	}
	// Cancellation stops the scan.
	d := fastDeps()
	temp, _ := d.newTempStore()
	defer temp.close()
	fd, _ := openPathDir(folder)
	defer closeFD(fd)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, _, err := d.snapshotFolder(ctx, fd, &looseDB{d: d, objFD: temp.objFD}); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled: %v", err)
	}
	// A file whose identity changed between lstat and read, and a stream
	// shorter than its declared size, are source changes.
	w := &snapWalk{ctx: context.Background(), d: d, db: &looseDB{d: d, objFD: temp.objFD}, man: newManifest()}
	st, _ := lstatAt(fd, "a")
	os.WriteFile(filepath.Join(folder, "a"), []byte("longer content now"), 0o644)
	if _, err := w.file(fd, "a", &st); contract.TransferReason(err) != contract.ReasonSourceChanged {
		t.Fatalf("identity change: %v", err)
	}
	if _, err := w.db.write(context.Background(), plumbing.BlobObject, 10, strings.NewReader("short")); !errors.Is(err, errShortRead) {
		t.Fatalf("short read: %v", err)
	}
	if _, err := w.file(fd, "gone", &st); contract.TransferReason(err) != contract.ReasonSourceChanged {
		t.Fatalf("disappeared: %v", err)
	}
	// Temporary stores are private and removed.
	t2, _ := fastDeps().newTempStore()
	fi, _ := os.Stat(t2.dir)
	if fi.Mode().Perm() != 0o700 {
		t.Fatalf("temp mode %v", fi.Mode())
	}
	t2.close()
	if _, err := os.Stat(t2.dir); !os.IsNotExist(err) {
		t.Fatal("temp store left")
	}
	bad := fastDeps()
	bad.tempDir = filepath.Join(tempDir(t), "missing")
	if _, err := bad.newTempStore(); contract.TransferReason(err) != contract.ReasonStorageFailure {
		t.Fatalf("temp dir failure: %v", err)
	}
}

// TestLoosePublishNoHardLinks proves the loose-object publication on a
// filesystem without hard links (the no-replace rename fallback) and the
// existing-object verification on both publication paths.
func TestLoosePublishNoHardLinks(t *testing.T) {
	ctx := context.Background()
	for _, linkErr := range []error{unix.EPERM, unix.ENOTSUP, unix.EMLINK, nil} {
		d := fastDeps()
		if linkErr != nil {
			d.linkAt = func(int, string, int, string, int) error { return linkErr }
		}
		temp, err := d.newTempStore()
		if err != nil {
			t.Fatal(err)
		}
		db := &looseDB{d: d, objFD: temp.objFD}
		write := func(s string) (plumbing.Hash, error) {
			return db.write(ctx, plumbing.BlobObject, int64(len(s)), strings.NewReader(s))
		}
		h, err := write("hello\n")
		if err != nil || h.String() != "ce013625030ba8dba906f756967f9e9ca394464a" {
			t.Fatalf("%v: %s %v", linkErr, h, err)
		}
		// Publishing the same object again verifies the existing one.
		if h2, err := write("hello\n"); err != nil || h2 != h {
			t.Fatalf("%v: again %s %v", linkErr, h2, err)
		}
		if o, err := temp.store.EncodedObject(plumbing.BlobObject, h); err != nil || o.Size() != 6 {
			t.Fatalf("%v: stored %v", linkErr, err)
		}
		// An existing object with the right name and wrong content is an
		// integrity failure, never replaced.
		other := plumbing.ComputeHash(plumbing.BlobObject, []byte("other"))
		fan := filepath.Join(temp.dir, "objects", other.String()[:2])
		os.MkdirAll(fan, 0o700)
		bad := filepath.Join(fan, other.String()[2:])
		os.WriteFile(bad, []byte("not zlib"), 0o400)
		if _, err := write("other"); codeOf(err) != contract.CodeInternal || !strings.Contains(err.Error(), "corrupt") {
			t.Fatalf("%v: corrupt existing object: %v", linkErr, err)
		}
		os.Remove(bad)
		os.Mkdir(bad, 0o700)
		if _, err := write("other"); codeOf(err) != contract.CodeInternal || !strings.Contains(err.Error(), "unreadable") {
			t.Fatalf("%v: unreadable existing object: %v", linkErr, err)
		}
		// No temporary object survives any path.
		ents, _ := os.ReadDir(filepath.Join(temp.dir, "objects"))
		for _, e := range ents {
			if strings.HasPrefix(e.Name(), "tmp_obj_") {
				t.Fatalf("%v: leftover %s", linkErr, e.Name())
			}
		}
		temp.close()
	}
	// Any other link error fails the write and removes the temporary.
	d := fastDeps()
	d.linkAt = func(int, string, int, string, int) error { return unix.EIO }
	temp, _ := d.newTempStore()
	defer temp.close()
	db := &looseDB{d: d, objFD: temp.objFD}
	if _, err := db.write(ctx, plumbing.BlobObject, 1, strings.NewReader("x")); !errors.Is(err, unix.EIO) {
		t.Fatalf("EIO: %v", err)
	}
	if ents, _ := os.ReadDir(filepath.Join(temp.dir, "objects")); len(ents) != 2 { // pack and the fan-out
		t.Fatalf("leftovers %v", ents)
	}
}
