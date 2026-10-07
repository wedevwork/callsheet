package taskworkspace

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/go-git/go-git/v5/plumbing/object"

	"github.com/wedevwork/callsheet/internal/contract"
	"github.com/wedevwork/callsheet/internal/workspacetransfer"
)

// TestSyncOverride: with counting overrides installed, the durability of
// the task databases the public constructors open (CreateTaskDB, Prepare's
// database and checkout, Snapshot, StoreCommit's TaskDB.Sync) reaches
// workspacetransfer's override, and this package's own syncs (the cache
// metadata file and its directory in writeMeta, the work directory of the
// runtime allocation) reach this package's: none is a real fsync or
// syncfs. Not parallel: the overrides are process-wide (the parallel tests
// of the run wait), and it calls no helper that installs SkipDurability.
func TestSyncOverride(t *testing.T) {
	var dbSyncs atomic.Int64
	var mu sync.Mutex
	var own []string
	prevDB := workspacetransfer.SetSyncForTest(func(int) error { dbSyncs.Add(1); return nil })
	prevOwn := SetSyncForTest(func(f *os.File) error {
		mu.Lock()
		own = append(own, f.Name())
		mu.Unlock()
		return nil
	})
	t.Cleanup(func() {
		workspacetransfer.SetSyncForTest(prevDB)
		SetSyncForTest(prevOwn)
	})
	ctx := context.Background()
	took := func(what string, err error) {
		t.Helper()
		if err != nil {
			t.Fatalf("%s: %v", what, err)
		}
		if dbSyncs.Swap(0) == 0 {
			t.Fatalf("%s: no task database sync reached the override", what)
		}
	}

	db, err := workspacetransfer.CreateTaskDB(filepath.Join(t.TempDir(), ObjectsName))
	if err != nil {
		t.Fatal(err)
	}
	_, err = db.WriteObject(ctx, &object.Tree{})
	if err == nil {
		err = db.Sync()
	}
	db.Close()
	took("CreateTaskDB, WriteObject and Sync", err)

	src := newSource(t)
	base := src.commit(spec("a.txt", "a\n", "d/b", "b"))
	m, err := OpenCache(ctx, CacheOptions{Root: t.TempDir(), Origin: "https://plane.test fp"})
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	work := filepath.Join(dir, WorkName)
	if err := os.Mkdir(work, 0o700); err != nil {
		t.Fatal(err)
	}
	p, err := Prepare(ctx, PrepareInput{GOOS: runtime.GOOS, TaskDir: dir, Binding: binding(base), Remote: &fetcher{src: src}, Cache: m, RuntimeDir: true})
	took("Prepare", err)
	mu.Lock()
	var meta, metaDir, workDir bool
	for _, n := range own {
		switch {
		case strings.HasPrefix(filepath.Base(n), stagePrefix+"meta-"):
			meta = true
		case n == work:
			workDir = true
		}
	}
	for _, n := range own {
		if _, err := os.Stat(filepath.Join(n, cacheMetaName)); err == nil {
			metaDir = true
		}
	}
	got := strings.Join(own, "\n")
	own = nil
	mu.Unlock()
	if !meta || !metaDir || !workDir {
		t.Fatalf("own syncs through the override (metadata file %v, its directory %v, work directory %v):\n%s", meta, metaDir, workDir, got)
	}

	writeFile(t, filepath.Join(work, "new.txt"), "new\n", 0o644)
	tree, _, err := Snapshot(ctx, SnapshotInput{GOOS: runtime.GOOS, TaskDir: dir, Map: p.Map, RuntimeDir: p.RuntimeDir})
	took("Snapshot", err)
	c, _, err := ResultCommit(taskID(1), "worker-a", "model", contract.TaskSucceeded, &base, tree)
	if err != nil {
		t.Fatal(err)
	}
	_, err = StoreCommit(ctx, dir, c)
	took("StoreCommit", err)

	// Removed, the override returns and (*os.File).Sync is back.
	if old := SetSyncForTest(nil); old == nil {
		t.Fatal("removing the override did not return it")
	}
	if err := syncDir(t.TempDir()); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(own) != 0 {
		t.Fatalf("a removed override still saw %q", own)
	}
}
