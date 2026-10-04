package workspacetransfer

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/filemode"
	"github.com/go-git/go-git/v5/plumbing/format/index"
	"github.com/go-git/go-git/v5/plumbing/object"
	"golang.org/x/sys/unix"
	"golang.org/x/text/unicode/norm"

	"github.com/wedevwork/callsheet/internal/contract"
	"github.com/wedevwork/callsheet/internal/testkit"
)

// Iteration 10b UT-B4/UT-B5 primitives: the trusted task database, the
// private checkout with its independent child repository and pathname map,
// and the plain-folder result snapshot.

func newTaskDB(t testing.TB, d *deps) *TaskDB {
	t.Helper()
	db, err := d.createTaskDB(filepath.Join(tempDir(t), "db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(db.Close)
	return db
}

func dbCommit(t testing.TB, db *TaskDB, files map[string]fspec, parents ...plumbing.Hash) plumbing.Hash {
	t.Helper()
	specs := map[string]testkit.FileSpec{}
	for p, f := range files {
		specs[p] = testkit.FileSpec{Mode: f.mode, Content: []byte(f.content)}
	}
	tree, err := testkit.WriteTree(db.store, specs)
	if err != nil {
		t.Fatal(err)
	}
	h, err := testkit.WriteCommit(db.store, tree, parents, "base", testkit.FixedWhen)
	if err != nil {
		t.Fatal(err)
	}
	return h
}

func newWork(t testing.TB) string {
	t.Helper()
	w := filepath.Join(tempDir(t), "work")
	if err := os.Mkdir(w, 0o700); err != nil {
		t.Fatal(err)
	}
	return w
}

func treeFiles(t testing.TB, db *TaskDB, tree plumbing.Hash) map[string]fspec {
	t.Helper()
	tr, err := object.GetTree(db.store, tree)
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]fspec{}
	walker := object.NewTreeWalker(tr, true, nil)
	defer walker.Close()
	for {
		name, e, err := walker.Next()
		if err != nil {
			break
		}
		if e.Mode == filemode.Dir {
			continue
		}
		b, err := readBlob(db.store, e.Hash)
		if err != nil {
			t.Fatal(err)
		}
		out[name] = fspec{e.Mode, string(b)}
	}
	return out
}

func TestTaskCheckout(t *testing.T) {
	d := fastDeps()
	db := newTaskDB(t, d)
	base := map[string]fspec{"a.txt": reg("alpha\r\n"), "bin/run": exe("#!/bin/sh\n"), "dir/sub/b": reg("b"), "ln": link("dir/sub/b"), "callsheet-final.txt": reg("mine")}
	commit := dbCommit(t, db, base)
	work := newWork(t)
	co, err := d.checkoutTask(context.Background(), CheckoutOptions{GOOS: "linux", Work: work, DB: db, Commit: commit})
	if err != nil {
		t.Fatal(err)
	}
	if co.Files != 5 || co.Map.Len() != 0 {
		t.Fatalf("checkout %+v", co)
	}
	for p, want := range map[string]os.FileMode{"a.txt": 0o644, "bin/run": 0o755, "dir": 0o755, "dir/sub": 0o755, "callsheet-final.txt": 0o644} {
		fi, err := os.Lstat(filepath.Join(work, p))
		if err != nil || fi.Mode().Perm() != want {
			t.Fatalf("%s: %v %v", p, fi, err)
		}
	}
	if fi, _ := os.Stat(work); fi.Mode().Perm() != 0o700 {
		t.Fatalf("work mode %v", fi.Mode())
	}
	if b, _ := os.ReadFile(filepath.Join(work, "a.txt")); string(b) != "alpha\r\n" {
		t.Fatalf("CRLF not verbatim: %q", b)
	}
	if tgt, _ := os.Readlink(filepath.Join(work, "ln")); tgt != "dir/sub/b" {
		t.Fatalf("link %q", tgt)
	}
	git := filepath.Join(work, ".git")
	if b, _ := os.ReadFile(filepath.Join(git, "HEAD")); string(b) != commit.String()+"\n" {
		t.Fatalf("HEAD %q", b)
	}
	if b, _ := os.ReadFile(filepath.Join(git, "config")); !strings.Contains(string(b), "autocrlf = false") || strings.Contains(string(b), "remote") {
		t.Fatalf("config %q", b)
	}
	for _, p := range []string{"hooks", "objects/info/alternates", "info"} {
		if _, err := os.Lstat(filepath.Join(git, p)); err == nil {
			t.Fatalf("%s exists", p)
		}
	}
	f, err := os.Open(filepath.Join(git, "index"))
	if err != nil {
		t.Fatal(err)
	}
	idx := &index.Index{}
	if err := index.NewDecoder(f).Decode(idx); err != nil {
		t.Fatal(err)
	}
	f.Close()
	var names []string
	for _, e := range idx.Entries {
		names = append(names, e.Name)
	}
	if strings.Join(names, ",") != "a.txt,bin/run,callsheet-final.txt,dir/sub/b,ln" {
		t.Fatalf("index %v", names)
	}
	// The child's objects are an independent copy (no hard links to the
	// trusted database), and its history is complete on its own.
	hex := commit.String()
	a, err1 := os.Stat(filepath.Join(git, "objects", hex[:2], hex[2:]))
	b, err2 := os.Stat(filepath.Join(db.dir, "objects", hex[:2], hex[2:]))
	if err1 != nil || err2 != nil || os.SameFile(a, b) {
		t.Fatalf("child object not independent: %v %v", err1, err2)
	}
	if st := a.Sys().(*syscall.Stat_t); st.Nlink != 1 {
		t.Fatalf("child object has %d links", st.Nlink)
	}
	// A non-empty work directory is refused.
	if _, err := d.checkoutTask(context.Background(), CheckoutOptions{GOOS: "linux", Work: work, DB: db, Commit: commit}); contract.TransferReason(err) != contract.ReasonDestinationNotEmpty {
		t.Fatalf("non-empty: %v", err)
	}
}

func TestTaskCheckoutEmpty(t *testing.T) {
	d := fastDeps()
	db := newTaskDB(t, d)
	work := newWork(t)
	co, err := d.checkoutTask(context.Background(), CheckoutOptions{GOOS: "linux", Work: work, DB: db})
	if err != nil || co.Files != 0 {
		t.Fatal(co, err)
	}
	if b, _ := os.ReadFile(filepath.Join(work, ".git", "HEAD")); string(b) != "ref: refs/heads/main\n" {
		t.Fatalf("HEAD %q", b)
	}
	names, _ := os.ReadDir(work)
	if len(names) != 1 || names[0].Name() != ".git" {
		t.Fatalf("work %v", names)
	}
}

func TestTaskCheckoutRefusals(t *testing.T) {
	d := fastDeps()
	db := newTaskDB(t, d)
	for name, files := range map[string]map[string]fspec{
		"escape":  {"ln": link("../outside")},
		"abs":     {"ln": link("/etc/passwd")},
		"cycle":   {"a": link("b"), "b": link("a")},
		"through": {"f": reg("x"), "ln": link("f/x")},
	} {
		commit := dbCommit(t, db, files)
		if _, err := d.checkoutTask(context.Background(), CheckoutOptions{GOOS: "linux", Work: newWork(t), DB: db, Commit: commit}); contract.TransferReason(err) != contract.ReasonUnsafeTree {
			t.Errorf("%s: %v", name, err)
		}
	}
	// Case aliases. The GOOS "linux" outcome is the work filesystem's, not
	// the OS's: an exact-name filesystem (ext4, tmpfs) keeps A and a apart
	// and the checkout succeeds; a filesystem that aliases names (APFS,
	// HFS+, a case-folding Linux directory) makes the link to its own case
	// alias resolve to itself, a cycle refused before anything links. The
	// production probe folds case and normalization together once either
	// aliases, so either independent observation selects the refusal.
	commit := dbCommit(t, db, map[string]fspec{"A": reg("1"), "a": link("A")})
	caseFold, normFold := hostNameAliasing(t)
	_, err := d.checkoutTask(context.Background(), CheckoutOptions{GOOS: "linux", Work: newWork(t), DB: db, Commit: commit})
	if caseFold || normFold {
		if contract.TransferReason(err) != contract.ReasonUnsafeTree || !strings.Contains(err.Error(), "symlink cycle") {
			t.Fatalf("native aliasing filesystem (case %v, normalization %v): case alias not refused as a cycle: %v", caseFold, normFold, err)
		}
	} else if err != nil {
		t.Fatalf("exact-name filesystem: case aliases: %v", err)
	}
	// The aliasing filesystem's outcome on any host: the probe seam
	// reports aliasing and the cycle is refused before anything is created.
	aliased := fastDeps()
	aliased.aliasProbe = func(int) (bool, error) { return true, nil }
	created := false
	aliased.fail = func(op string) error {
		if op == "export-create" || op == "export-symlink" {
			created = true
		}
		return nil
	}
	aliasWork := newWork(t)
	if _, err := aliased.checkoutTask(context.Background(), CheckoutOptions{GOOS: "linux", Work: aliasWork, DB: db, Commit: commit}); contract.TransferReason(err) != contract.ReasonUnsafeTree || !strings.Contains(err.Error(), "symlink cycle") || created {
		t.Fatalf("probed aliasing: %v (created %v)", err, created)
	}
	if names, _ := os.ReadDir(aliasWork); len(names) != 0 {
		t.Fatalf("probed aliasing wrote %v", names)
	}
	if _, err := d.checkoutTask(context.Background(), CheckoutOptions{GOOS: "darwin", Work: newWork(t), DB: db, Commit: commit}); contract.TransferReason(err) != contract.ReasonUnsafeTree {
		t.Fatalf("darwin alias: %v", err)
	}
	if _, err := d.checkoutTask(context.Background(), CheckoutOptions{GOOS: "plan9", Work: newWork(t), DB: db, Commit: commit}); codeOf(err) != contract.CodeInvalidArgument {
		t.Fatalf("goos: %v", err)
	}
	if _, err := d.checkoutTask(context.Background(), CheckoutOptions{GOOS: "linux", Work: "rel", DB: db, Commit: commit}); err == nil {
		t.Fatal("relative work accepted")
	}
	if _, err := d.checkoutTask(context.Background(), CheckoutOptions{GOOS: "linux", Work: filepath.Join(tempDir(t), "missing"), DB: db, Commit: commit}); err == nil {
		t.Fatal("missing work accepted")
	}
	// A darwin path budget below the work path refuses before writes.
	// The directory must be creatable on darwin (at most its 1023-byte
	// budget) and still too long for checkout's repository paths.
	limit := contract.MaxPathFor("darwin")
	deep := pathOfLen(t, tempDir(t), limit-16)
	if len(deep) > limit || len(deep)+len("/.git/objects/00/")+38+len(".lock") <= limit {
		t.Fatalf("darwin budget fixture: work path of %d bytes, want at most %d and over %d with its repository paths", len(deep), limit, limit)
	}
	if err := os.MkdirAll(deep, 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := d.checkoutTask(context.Background(), CheckoutOptions{GOOS: "darwin", Work: deep, DB: db, Commit: commit}); contract.TransferReason(err) != contract.ReasonUnsafeTree {
		t.Fatalf("darwin budget: %v", err)
	}
	// Cancellation and injected storage failures leave an error.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	big := dbCommit(t, db, map[string]fspec{"x": reg("x")})
	if _, err := d.checkoutTask(ctx, CheckoutOptions{GOOS: "linux", Work: newWork(t), DB: db, Commit: big}); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancel: %v", err)
	}
	for _, op := range []string{"export-create", "checkout-git", "checkout-dirsync", "checkout-object-create"} {
		fd := fastDeps()
		fd.fail = func(o string) error {
			if o == op {
				return syscall.EIO
			}
			return nil
		}
		if _, err := fd.checkoutTask(context.Background(), CheckoutOptions{GOOS: "linux", Work: newWork(t), DB: db, Commit: big}); err == nil {
			t.Errorf("%s: no failure", op)
		}
	}
}

// hostNameAliasing independently observes whether the test's temporary
// filesystem aliases names by ASCII case and by Unicode normalization
// (APFS and HFS+ do by default; Linux ext4 and tmpfs do not), so a test
// can require the native filesystem's outcome rather than the OS's.
func hostNameAliasing(t testing.TB) (caseFold, normFold bool) {
	t.Helper()
	dir := tempDir(t)
	mustWrite(t, filepath.Join(dir, "Probe"), "")
	_, cerr := os.Lstat(filepath.Join(dir, "pROBE"))
	mustWrite(t, filepath.Join(dir, "é"), "")
	_, nerr := os.Lstat(filepath.Join(dir, "é"))
	return cerr == nil, nerr == nil
}

// wantMappedCheckout requires a successful checkout whose NFC tree
// directory is reached through its NFD native spelling: two files, one
// mapped path, raw index names, and a snapshot through the map that
// round-trips to the base tree (no delete+add).
func wantMappedCheckout(t *testing.T, d *deps, db *TaskDB, commit plumbing.Hash, work, nfc string, co *Checkout, err error) {
	t.Helper()
	nfd := norm.NFD.String(nfc)
	if err != nil || co.Files != 2 || co.Map.Len() != 1 {
		t.Fatalf("mapped checkout: %+v %v", co, err)
	}
	if co.Map.Raw(nfd+"/menu") != nfc+"/menu" || co.Map.nativeOf(nfc+"/menu") != nfd+"/menu" {
		t.Fatal("mapped checkout translation")
	}
	if b, err := os.ReadFile(filepath.Join(work, nfd, "menu")); err != nil || string(b) != "x" {
		t.Fatalf("native spelling unreadable: %q %v", b, err)
	}
	f, err := os.Open(filepath.Join(work, ".git", "index"))
	if err != nil {
		t.Fatal(err)
	}
	idx := &index.Index{}
	err = index.NewDecoder(f).Decode(idx)
	f.Close()
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, e := range idx.Entries {
		names = append(names, e.Name)
	}
	if strings.Join(names, ",") != nfc+"/menu,plain" {
		t.Fatalf("index names %q", names)
	}
	s, err := d.snapshotTask(context.Background(), SnapshotOptions{GOOS: "linux", Work: work, DB: db, Map: co.Map})
	if err != nil {
		t.Fatal(err)
	}
	base, _ := object.GetCommit(db.store, commit)
	if s.Tree != base.TreeHash {
		t.Fatalf("the mapped checkout did not round-trip: %v", treeFiles(t, db, s.Tree))
	}
}

// TestTaskCheckoutNormalization forces darwin's normalization-insensitive
// spelling: readdir returns the NFD form of an NFC tree name, the map
// records it, and a snapshot maps it back (no delete+add). Whether the
// reported NFD spelling can be opened is the work filesystem's property.
func TestTaskCheckoutNormalization(t *testing.T) {
	d := fastDeps()
	db := newTaskDB(t, d)
	nfc := "café"
	commit := dbCommit(t, db, map[string]fspec{nfc + "/menu": reg("x"), "plain": reg("p")})
	work := newWork(t)
	nfd := func(fd int) ([]string, error) {
		names, err := readNames(fd)
		for i, n := range names {
			names[i] = norm.NFD.String(n)
		}
		return names, err
	}
	co, err := d.checkoutTask(context.Background(), CheckoutOptions{GOOS: "darwin", Work: work, DB: db, Commit: commit, listNames: nfd})
	if _, normFold := hostNameAliasing(t); normFold {
		// A normalization-insensitive filesystem (APFS, HFS+) opens the
		// reported NFD spelling as the NFC entry: the mapped checkout
		// succeeds and round-trips.
		wantMappedCheckout(t, d, db, commit, work, nfc, co, err)
	} else if contract.TransferReason(err) != contract.ReasonStorageFailure {
		// An exact-name filesystem (Linux): the NFD names the seam reports
		// do not exist, so reading or indexing through them fails, which
		// proves the map was used.
		t.Fatalf("expected the NFD spelling to be used: %+v %v", co, err)
	}
	// The normalization-insensitive outcome on any host: the seam also
	// gives the NFC directory its NFD spelling (as HFS+ stores it), so the
	// reported name is accessible and the mapped checkout must succeed.
	accWork := newWork(t)
	accessible := func(fd int) ([]string, error) {
		names, err := readNames(fd)
		if err != nil {
			return nil, err
		}
		for _, n := range names {
			if n == nfc {
				if err := unix.Renameat(fd, nfc, fd, norm.NFD.String(nfc)); err != nil {
					return nil, err
				}
			}
		}
		return nfd(fd)
	}
	co, err = d.checkoutTask(context.Background(), CheckoutOptions{GOOS: "darwin", Work: accWork, DB: db, Commit: commit, listNames: accessible})
	wantMappedCheckout(t, d, db, commit, accWork, nfc, co, err)
	m := NewPathMap()
	if err := m.add(norm.NFD.String(nfc), nfc); err != nil {
		t.Fatal(err)
	}
	if m.Raw(norm.NFD.String(nfc)+"/menu") != nfc+"/menu" || m.Raw("plain") != "plain" || m.nativeOf(nfc+"/menu") != norm.NFD.String(nfc)+"/menu" {
		t.Fatal("map translation")
	}
	if err := m.add("other", nfc); err == nil {
		t.Fatal("a second native spelling of one raw path was accepted")
	}
	if err := m.add(norm.NFD.String(nfc), "x"); err == nil {
		t.Fatal("one native path mapped to two raw paths")
	}
	// A real NFD directory on Linux mapped back to its NFC tree name.
	snapWork := newWork(t)
	mustWrite(t, filepath.Join(snapWork, norm.NFD.String(nfc), "menu"), "x")
	mustWrite(t, filepath.Join(snapWork, "plain"), "p")
	s, err := d.snapshotTask(context.Background(), SnapshotOptions{GOOS: "linux", Work: snapWork, DB: db, Map: m})
	if err != nil {
		t.Fatal(err)
	}
	base, _ := object.GetCommit(db.store, commit)
	if s.Tree != base.TreeHash {
		t.Fatalf("an untouched NFD spelling changed the tree: %v", treeFiles(t, db, s.Tree))
	}
	// An ambiguous native spelling is a collision.
	both := func(fd int) ([]string, error) { return []string{norm.NFD.String(nfc), norm.NFD.String(nfc) + ""}, nil }
	if err := observeNames(context.Background(), -1, []exportEntry{{path: nfc}}, NewPathMap(), func(int) ([]string, error) { return []string{"x"}, nil }); err == nil {
		t.Fatal("a missing spelling was accepted")
	}
	_ = both
}

func TestTaskSnapshot(t *testing.T) {
	d := fastDeps()
	db := newTaskDB(t, d)
	base := map[string]fspec{"keep": reg("k"), "tracked.log": reg("t"), "sub/x": reg("x")}
	commit := dbCommit(t, db, base)
	work := newWork(t)
	if _, err := d.checkoutTask(context.Background(), CheckoutOptions{GOOS: "linux", Work: work, DB: db, Commit: commit}); err != nil {
		t.Fatal(err)
	}
	// Unchanged: the base tree exactly.
	s, err := d.snapshotTask(context.Background(), SnapshotOptions{GOOS: "linux", Work: work, DB: db})
	if err != nil {
		t.Fatal(err)
	}
	bc, _ := object.GetCommit(db.store, commit)
	if s.Tree != bc.TreeHash || s.Files != 3 {
		t.Fatalf("unchanged %+v", s)
	}
	// The child rewrites its own repository (ignored), a runtime directory
	// (excluded), an ignore rule that drops a tracked file, CRLF bytes, an
	// executable, an empty directory and a symlink.
	rt := contract.RuntimeDirPrefix + strings.Repeat("a", 32)
	mustWrite(t, filepath.Join(work, ".git", "HEAD"), "garbage")
	mustWrite(t, filepath.Join(work, rt, "callsheet-final.txt"), "answer")
	mustWrite(t, filepath.Join(work, ".gitignore"), "*.log\n!keep.log\nbuild/\n")
	mustWrite(t, filepath.Join(work, "keep.log"), "kept\r\n")
	mustWrite(t, filepath.Join(work, "build", "out"), "o")
	mustWrite(t, filepath.Join(work, "new.sh"), "#!/bin/sh\n")
	os.Chmod(filepath.Join(work, "new.sh"), 0o750)
	os.Mkdir(filepath.Join(work, "emptydir"), 0o755)
	os.Symlink("keep", filepath.Join(work, "lnk"))
	os.Remove(filepath.Join(work, "sub", "x"))
	s, err = d.snapshotTask(context.Background(), SnapshotOptions{GOOS: "linux", Work: work, DB: db, RuntimeDir: rt})
	if err != nil {
		t.Fatal(err)
	}
	got := treeFiles(t, db, s.Tree)
	want := map[string]fspec{"keep": reg("k"), ".gitignore": reg("*.log\n!keep.log\nbuild/\n"), "keep.log": reg("kept\r\n"), "new.sh": exe("#!/bin/sh\n"), "lnk": link("keep")}
	if len(got) != len(want) {
		t.Fatalf("result %v", got)
	}
	for p, f := range want {
		if got[p] != f {
			t.Fatalf("%s = %+v, want %+v (all %v)", p, got[p], f, got)
		}
	}
	// A removed .git and an empty work directory snapshot to the empty tree.
	empty := newWork(t)
	s, err = d.snapshotTask(context.Background(), SnapshotOptions{GOOS: "linux", Work: empty, DB: db})
	if err != nil || s.Tree != EmptyTree {
		t.Fatalf("empty %v %v", s, err)
	}
}

func TestTaskSnapshotRefusals(t *testing.T) {
	d := fastDeps()
	db := newTaskDB(t, d)
	for name, setup := range map[string]func(w string){
		"nested repo":  func(w string) { mustWrite(t, filepath.Join(w, "vendor", ".git", "HEAD"), "x") },
		"nested alias": func(w string) { mustWrite(t, filepath.Join(w, "vendor", ".GIT"), "gitdir: x") },
		"top alias":    func(w string) { mustWrite(t, filepath.Join(w, ".Git"), "x") },
		"fifo":         func(w string) { unixMkfifo(t, filepath.Join(w, "pipe")) },
		"escape link":  func(w string) { os.Symlink("../x", filepath.Join(w, "l")) },
		"abs link":     func(w string) { os.Symlink("/x", filepath.Join(w, "l")) },
	} {
		w := newWork(t)
		setup(w)
		_, err := d.snapshotTask(context.Background(), SnapshotOptions{GOOS: "linux", Work: w, DB: db})
		reason := contract.TransferReason(err)
		switch name {
		case "nested repo", "nested alias", "top alias":
			if reason != contract.ReasonUnsupportedRepository {
				t.Errorf("%s: %v", name, err)
			}
		default:
			if reason != contract.ReasonUnsafeTree {
				t.Errorf("%s: %v", name, err)
			}
		}
	}
	// An ignored nested repository and an ignored FIFO are pruned first.
	w := newWork(t)
	mustWrite(t, filepath.Join(w, ".gitignore"), "vendor/\npipe\n")
	mustWrite(t, filepath.Join(w, "vendor", ".git", "HEAD"), "x")
	unixMkfifo(t, filepath.Join(w, "pipe"))
	if _, err := d.snapshotTask(context.Background(), SnapshotOptions{GOOS: "linux", Work: w, DB: db}); err != nil {
		t.Fatalf("ignored: %v", err)
	}
	// darwin's HFS+ ignorable code points also name .git.
	w = newWork(t)
	mustWrite(t, filepath.Join(w, "v", ".g‌it"), "x")
	if _, err := d.snapshotTask(context.Background(), SnapshotOptions{GOOS: "darwin", Work: w, DB: db}); contract.TransferReason(err) != contract.ReasonUnsupportedRepository {
		t.Fatalf("darwin alias: %v", err)
	}
	// A change during the scan is caught by the stability recheck.
	w = newWork(t)
	mustWrite(t, filepath.Join(w, "f"), "1")
	cd := fastDeps()
	cd.hook = func(stage string) {
		if stage == "task-snapshot-scanned" {
			mustWrite(t, filepath.Join(w, "late"), "2")
		}
	}
	if _, err := cd.snapshotTask(context.Background(), SnapshotOptions{GOOS: "linux", Work: w, DB: db}); contract.TransferReason(err) != contract.ReasonSourceChanged {
		t.Fatalf("changed: %v", err)
	}
	// Cancellation, a bad OS and injected storage failures.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := d.snapshotTask(ctx, SnapshotOptions{GOOS: "linux", Work: w, DB: db}); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancel: %v", err)
	}
	if _, err := d.snapshotTask(context.Background(), SnapshotOptions{GOOS: "windows", Work: w, DB: db}); err == nil {
		t.Fatal("windows accepted")
	}
	if _, err := d.snapshotTask(context.Background(), SnapshotOptions{GOOS: "linux", Work: filepath.Join(w, "missing"), DB: db}); err == nil {
		t.Fatal("missing work accepted")
	}
	for _, op := range []string{"taskdb-object-create", "taskdb-object-write", "taskdb-object-dirsync"} {
		fd := fastDeps()
		fdb := newTaskDB(t, fd)
		fd.fail = func(o string) error {
			if o == op {
				return syscall.ENOSPC
			}
			return nil
		}
		w := newWork(t)
		mustWrite(t, filepath.Join(w, "f"), "fresh "+op)
		if _, err := fd.snapshotTask(context.Background(), SnapshotOptions{GOOS: "linux", Work: w, DB: fdb}); contract.TransferReason(err) != contract.ReasonStorageFailure {
			t.Errorf("%s: %v", op, err)
		}
	}
	var c int64 = contract.MaxSafeInteger
	if err := addSafe(&c, 1); contract.TransferReason(err) != contract.PubErrMetadataOverflow {
		t.Fatalf("overflow: %v", err)
	}
	if err := addSafe(&c, -1); err == nil {
		t.Fatal("negative accepted")
	}
}

func TestTaskDB(t *testing.T) {
	d := fastDeps()
	src := newTaskDB(t, d)
	c1 := dbCommit(t, src, map[string]fspec{"a": reg("1")})
	c2 := dbCommit(t, src, map[string]fspec{"a": reg("2"), "b": reg("3")}, c1)
	order, st, err := src.Closure(context.Background(), []plumbing.Hash{c2})
	if err != nil || len(order) != 7 || st.Objects != 7 || order[0] != c2 {
		t.Fatalf("closure %v %+v %v", order, st, err)
	}
	// A pack of the closure received into another database, and a loose
	// copy into a third, both validate on their own.
	var pack bytes.Buffer
	if err := src.WritePack(context.Background(), order, &pack); err != nil {
		t.Fatal(err)
	}
	dst := newTaskDB(t, d)
	if err := dst.ReceivePack(context.Background(), &pack); err != nil {
		t.Fatal(err)
	}
	if _, _, err := dst.Closure(context.Background(), []plumbing.Hash{c2}); err != nil || !dst.Has(c1) {
		t.Fatalf("received: %v", err)
	}
	cp := newTaskDB(t, d)
	cs, err := cp.CopyFrom(context.Background(), src.Store(), order)
	if err != nil || cs.Objects != 7 {
		t.Fatalf("copy %+v %v", cs, err)
	}
	if _, _, err := cp.Closure(context.Background(), []plumbing.Hash{c2}); err != nil {
		t.Fatal(err)
	}
	// Re-copying is verified, never overwritten.
	if _, err := cp.CopyFrom(context.Background(), src.Store(), order[:1]); err != nil {
		t.Fatal(err)
	}
	// An incomplete closure fails.
	partial := newTaskDB(t, d)
	if _, err := partial.CopyFrom(context.Background(), src.Store(), order[:1]); err != nil {
		t.Fatal(err)
	}
	if _, _, err := partial.Closure(context.Background(), []plumbing.Hash{c2}); err == nil {
		t.Fatal("an incomplete closure validated")
	}
	if _, err := partial.CopyFrom(context.Background(), src.Store(), []plumbing.Hash{plumbing.NewHash(strings.Repeat("e", 40))}); err == nil {
		t.Fatal("a missing object copied")
	}
	// The deterministic result commit.
	tree := plumbing.NewHash(strings.Repeat("1", 40))
	a, _ := CommitHash(TaskResultCommit(tree, c1, contract.TaskResultMessage("t_"+strings.Repeat("0", 32), "r", "m", "succeeded")))
	b, _ := CommitHash(TaskResultCommit(tree, c1, contract.TaskResultMessage("t_"+strings.Repeat("0", 32), "r", "m", "succeeded")))
	e, _ := CommitHash(TaskResultCommit(tree, plumbing.ZeroHash, contract.TaskResultMessage("t_"+strings.Repeat("0", 32), "r", "m", "succeeded")))
	if a != b || a == e || len(TaskResultCommit(tree, plumbing.ZeroHash, "m").ParentHashes) != 0 {
		t.Fatal("result commit determinism")
	}
	if h, err := cp.WriteObject(context.Background(), &object.Tree{}); err != nil || h != EmptyTree {
		t.Fatalf("empty tree %v %v", h, err)
	}
	if err := cp.Sync(); err != nil {
		t.Fatal(err)
	}
	n, err := LogicalSize(context.Background(), cp.Dir())
	if err != nil || n <= 0 {
		t.Fatalf("size %d %v", n, err)
	}
	// Removal handles read-only objects and a directory a child locked.
	os.Chmod(filepath.Join(cp.Dir(), "refs"), 0)
	cp.Close()
	if err := RemoveTree(cp.Dir()); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(cp.Dir()); !os.IsNotExist(err) {
		t.Fatal("not removed")
	}
	if err := RemoveTree(cp.Dir()); err != nil {
		t.Fatal("missing tree:", err)
	}
	if err := RemoveTree(filepath.Join(cp.Dir(), "x", "y")); err != nil {
		t.Fatal("missing parent:", err)
	}
	// Open refuses a non-database and a file; create refuses an existing path.
	if _, err := OpenTaskDB(tempDir(t)); err == nil {
		t.Fatal("opened an empty directory")
	}
	if _, err := CreateTaskDB(src.Dir()); err == nil {
		t.Fatal("created over an existing database")
	}
	fd := fastDeps()
	fd.fail = func(string) error { return syscall.EIO }
	if _, err := fd.createTaskDB(filepath.Join(tempDir(t), "x")); err == nil {
		t.Fatal("injected create failure")
	}
	if err := (&TaskDB{d: fd, objFD: src.objFD}).syncPack(); err == nil {
		t.Fatal("injected pack sync failure")
	}
	if err := (&TaskDB{d: fd, objFD: src.objFD, store: src.store}).ReceivePack(context.Background(), &pack); err == nil {
		t.Fatal("injected receive failure")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := dst.ReceivePack(ctx, bytes.NewReader([]byte("PACK"))); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled receive: %v", err)
	}
	if err := dst.ReceivePack(context.Background(), bytes.NewReader([]byte("garbage"))); err == nil {
		t.Fatal("garbage pack received")
	}
	if _, err := cp.CopyFrom(ctx, src.Store(), order); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled copy: %v", err)
	}
}

// TestTaskSyncBatch (iteration 10b): a task database's batched
// durability: the platform batch (syncfs on Linux; per-path fsync and one
// F_FULLFSYNC on darwin), the seam's per-path fallback, an object that
// already exists is never re-synced, and a failing path fails the batch.
func TestTaskSyncBatch(t *testing.T) {
	dir := t.TempDir()
	os.MkdirAll(filepath.Join(dir, "ab"), 0o700)
	os.WriteFile(filepath.Join(dir, "ab", "cdef"), []byte("x"), 0o600)
	fd, err := openPathDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer closeFD(fd)
	if err := syncBatch(fd, []string{"ab/cdef"}, []string{"ab", "."}); err != nil {
		t.Fatalf("platform batch: %v", err)
	}
	var synced []string
	d := &deps{syncFD: func(int) error { synced = append(synced, "x"); return nil }}
	if err := d.syncBatch(fd, []string{"ab/cdef"}, []string{"ab", "."}); err != nil || len(synced) != 3 {
		t.Fatalf("fallback: %v (%d syncs)", err, len(synced))
	}
	if err := d.syncBatch(fd, []string{"ab/missing"}, nil); err == nil {
		t.Fatal("a missing path synced")
	}
	os.Symlink("cdef", filepath.Join(dir, "ab", "link"))
	if err := eachSync(fd, []string{"ab/link"}, func(int) error { return nil }); err == nil {
		t.Fatal("a symlink was followed")
	}
	boom := errors.New("sync failed")
	if err := eachSync(fd, []string{"."}, func(int) error { return boom }); !errors.Is(err, boom) {
		t.Fatalf("failing sync: %v", err)
	}
	// Batched objects: new ones pend until the directory sync; an object
	// that exists is verified, never pending again.
	db := newTaskDB(t, fastDeps())
	h, err := db.WriteObject(context.Background(), &object.Tree{})
	if err != nil || len(db.db.pending) != 1 {
		t.Fatalf("pending %v %v", db.db.pending, err)
	}
	if err := db.Sync(); err != nil || len(db.db.pending) != 0 {
		t.Fatalf("sync %v pending %v", err, db.db.pending)
	}
	if h2, err := db.WriteObject(context.Background(), &object.Tree{}); err != nil || h2 != h || len(db.db.pending) != 0 {
		t.Fatalf("existing object pending %v %v", db.db.pending, err)
	}
	fd2 := fastDeps()
	fd2.fail = func(op string) error {
		if op == "taskdb-object-sync" {
			return boom
		}
		return nil
	}
	fdb := newTaskDB(t, fd2)
	fdb.WriteObject(context.Background(), &object.Commit{TreeHash: h, Message: "m"})
	if err := fdb.Sync(); err == nil {
		t.Fatal("a failing batch synced")
	}
}
