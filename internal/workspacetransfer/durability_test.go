package workspacetransfer

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"sync/atomic"
	"testing"

	"github.com/go-git/go-git/v5/plumbing/filemode"
	"github.com/go-git/go-git/v5/plumbing/object"
)

// funcPC is the code address of f (comparing the deps' primitives).
func funcPC(f any) uintptr { return reflect.ValueOf(f).Pointer() }

// TestEntrySyncOverride: SetSyncForTest is the exported entry points' only
// durability with an override installed (every sync and every task
// database batch of CreateTaskDB, OpenTaskDB, CheckoutTask, SnapshotTask
// and Pull reaches it, the platform primitives none: the deps fields are
// the override, never fsyncFD or syncBatch, so syncfs is never reached),
// and nothing else
// changes. Unset, the entry points use the platform primitives; deps a
// test builds (defaultDeps, fastDeps) and fsyncFD itself are never
// wrapped (TestFsyncFDFallback calls fsyncFD directly). Not parallel: the
// override is process-wide.
func TestEntrySyncOverride(t *testing.T) {
	prev := SetSyncForTest(nil)
	t.Cleanup(func() { SetSyncForTest(prev) })

	// Unset: production durability.
	if d := entryDeps(); funcPC(d.syncFD) != funcPC(fsyncFD) || d.syncBatchFD == nil || funcPC(d.syncBatchFD) != funcPC(syncBatch) {
		t.Fatal("without an override the entry points do not use fsyncFD and syncBatch")
	}

	var n atomic.Int64
	count := func(int) error { n.Add(1); return nil }
	if old := SetSyncForTest(count); old != nil {
		t.Fatal("an override was already installed")
	}
	d := entryDeps()
	if funcPC(d.syncFD) != funcPC(count) || d.syncBatchFD == nil || funcPC(d.syncBatchFD) == funcPC(syncBatch) {
		t.Fatal("the override does not replace the entry points' primitives")
	}
	// A batch is one override call on its directory, whatever it holds.
	dir := tempDir(t)
	fd, err := openPathDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer closeFD(fd)
	if err := d.syncBatch(fd, []string{"no/such/file"}, []string{"no/such", "."}); err != nil || n.Load() != 1 {
		t.Fatalf("batch through the override: %v (%d calls)", err, n.Load())
	}

	// Each exported entry point's durability reaches the override.
	steps := 0
	grew := func(what string, err error) {
		t.Helper()
		steps++
		if err != nil {
			t.Fatalf("%s: %v", what, err)
		}
		if got := n.Swap(0); got == 0 {
			t.Fatalf("%s made no durability call through the override", what)
		}
	}
	n.Store(0)
	ctx := context.Background()
	dbDir := filepath.Join(tempDir(t), "db")
	db, err := CreateTaskDB(dbDir)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	commit := dbCommit(t, db, map[string]fspec{"a.txt": reg("a\n"), "d/b": reg("b")})
	_, err = db.WriteObject(ctx, &object.Tree{})
	if err == nil {
		err = db.Sync()
	}
	grew("CreateTaskDB, WriteObject and Sync", err)

	again, err := OpenTaskDB(dbDir)
	if err != nil {
		t.Fatal(err)
	}
	defer again.Close()
	_, err = again.WriteObject(ctx, &object.Tree{Entries: []object.TreeEntry{{Name: "x", Mode: filemode.Regular, Hash: commit}}})
	if err == nil {
		err = again.Sync()
	}
	grew("OpenTaskDB, WriteObject and Sync", err)

	work := newWork(t)
	co, err := CheckoutTask(ctx, CheckoutOptions{GOOS: hostGOOS(), Work: work, DB: db, Commit: commit})
	grew("CheckoutTask", err)
	if err := os.WriteFile(filepath.Join(work, "new.txt"), []byte("new"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err = SnapshotTask(ctx, SnapshotOptions{GOOS: hostGOOS(), Work: work, DB: db, Map: co.Map})
	grew("SnapshotTask", err)

	s := newMemSeed(t)
	o := Options{GOOS: hostGOOS(), Env: s.env, Cwd: "/", Plane: s.p}
	_, err = Pull(ctx, o, PullRequest{Name: "ws", Ref: "main", Path: filepath.Join(tempDir(t), "out"), PathSet: true})
	grew("Pull", err)
	// Push makes no local durability call (it uploads); it constructs
	// entryDeps like the others, which the field checks above cover.
	if steps != 5 {
		t.Fatalf("%d entry steps", steps)
	}

	// Never wrapped: fsyncFD, and deps a test builds itself.
	f, err := os.Create(filepath.Join(tempDir(t), "f"))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if err := fsyncFD(int(f.Fd())); err != nil || n.Load() != 0 {
		t.Fatalf("fsyncFD reached the override: %v (%d calls)", err, n.Load())
	}
	if d := defaultDeps(); funcPC(d.syncFD) != funcPC(fsyncFD) || funcPC(d.syncBatchFD) != funcPC(syncBatch) {
		t.Fatal("the override changed defaultDeps")
	}
	if d := fastDeps(); funcPC(d.syncFD) == funcPC(count) || d.syncBatchFD != nil {
		t.Fatal("the override changed fastDeps")
	}

	// Removing it returns it and restores the platform primitives.
	if old := SetSyncForTest(nil); old == nil || funcPC(old) != funcPC(count) {
		t.Fatal("removing the override did not return it")
	}
	if d := entryDeps(); funcPC(d.syncFD) != funcPC(fsyncFD) || funcPC(d.syncBatchFD) != funcPC(syncBatch) {
		t.Fatal("removing the override did not restore the platform primitives")
	}
}
