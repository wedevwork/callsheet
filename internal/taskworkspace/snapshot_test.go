package taskworkspace

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"syscall"
	"testing"
	"time"

	git "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/filemode"
	"github.com/go-git/go-git/v5/plumbing/object"

	"github.com/wedevwork/callsheet/internal/contract"
	"github.com/wedevwork/callsheet/internal/testkit"
	"github.com/wedevwork/callsheet/internal/workspacetransfer"
)

// TestSnapshot is UT-B5 (iteration 10b): the result tree from the visible
// worktree alone with the plain-folder ignore policy, independent of the
// child's repository; unsupported content refused with fixed codes; the
// deterministic result commit and its push selection. Delegated from
// tests/function (TestTaskWorkspaceCommit/matrix). Do not rename it.
func TestSnapshot(t *testing.T) {
	t.Parallel()
	src := newSource(t)
	baseFiles := spec("keep.txt", "k\n", "old.log", "log\n", "src/a.txt", "a\n")
	base := src.commit(baseFiles)
	bm, _ := object.GetCommit(src.s, base)
	m, _ := openCache(t, 0)
	// snap prepares a fresh checkout, applies edit and snapshots.
	snap := func(t *testing.T, runtimeDir bool, edit func(work string)) (string, *Prepared, plumbing.Hash, error) {
		t.Helper()
		dir, p := prepared(t, m, &fetcher{src: src}, binding(base), runtimeDir)
		if edit != nil {
			edit(filepath.Join(dir, WorkName))
		}
		tree, _, err := Snapshot(context.Background(), SnapshotInput{GOOS: runtime.GOOS, TaskDir: dir, Map: p.Map, RuntimeDir: p.RuntimeDir})
		return dir, p, tree, err
	}
	// files reads a stored result tree.
	files := func(t *testing.T, dir string, tree plumbing.Hash) map[string]testkit.FileSpec {
		t.Helper()
		c, h, err := ResultCommit(taskID(1), "worker-a", "model", contract.TaskSucceeded, &base, tree)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := StoreCommit(context.Background(), dir, c); err != nil {
			t.Fatal(err)
		}
		db, err := workspacetransfer.OpenTaskDB(filepath.Join(dir, ObjectsName))
		if err != nil {
			t.Fatal(err)
		}
		defer db.Close()
		got, err := testkit.TreeFiles(db.Store(), h)
		if err != nil {
			t.Fatal(err)
		}
		return got
	}
	t.Run("unchanged", func(t *testing.T) {
		_, _, tree, err := snap(t, true, nil)
		if err != nil || tree != bm.TreeHash {
			t.Fatalf("unchanged tree %s (base %s) %v", tree, bm.TreeHash, err)
		}
	})
	t.Run("ignores-and-bytes", func(t *testing.T) {
		dir, _, tree, err := snap(t, true, func(work string) {
			writeFile(t, filepath.Join(work, ".gitignore"), "build/\n*.log\n!keep.log\n", 0o644)
			writeFile(t, filepath.Join(work, "build", "out.bin"), "obj", 0o644)
			writeFile(t, filepath.Join(work, "keep.log"), "kept", 0o644)
			writeFile(t, filepath.Join(work, "new.log"), "dropped", 0o644)
			writeFile(t, filepath.Join(work, "src", "crlf.txt"), "a\r\nb\r\n", 0o644)
			writeFile(t, filepath.Join(work, "src", "nested", ".gitignore"), "local.tmp\n", 0o644)
			writeFile(t, filepath.Join(work, "src", "nested", "local.tmp"), "x", 0o644)
			writeFile(t, filepath.Join(work, "run.sh"), "#!", 0o755)
			// An ordinary .gitmodules is data; an empty directory has no row.
			writeFile(t, filepath.Join(work, ".gitmodules"), "[submodule \"x\"]\n", 0o644)
			os.MkdirAll(filepath.Join(work, "empty", "dir"), 0o755)
			// A directory merely named like the runtime prefix is data.
			writeFile(t, filepath.Join(work, contract.RuntimeDirPrefix+"0123456789abcdef0123456789abcdef", "f"), "user", 0o644)
		})
		if err != nil {
			t.Fatal(err)
		}
		got := files(t, dir, tree)
		want := spec(".gitignore", "build/\n*.log\n!keep.log\n", "keep.txt", "k\n", "keep.log", "kept", "src/a.txt", "a\n", "src/crlf.txt", "a\r\nb\r\n",
			"src/nested/.gitignore", "local.tmp\n", ".gitmodules", "[submodule \"x\"]\n", contract.RuntimeDirPrefix+"0123456789abcdef0123456789abcdef/f", "user")
		want["run.sh"] = testkit.FileSpec{Mode: filemode.Executable, Content: []byte("#!")}
		// old.log (tracked in the base) is newly ignored: a deletion.
		if err := testkit.EqualFiles(got, want); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("child-repository", func(t *testing.T) {
		// The child's commits, branches, HEAD, index lock and a removed
		// .git never change the result.
		dir, _, tree, err := snap(t, false, func(work string) {
			writeFile(t, filepath.Join(work, "c.txt"), "c", 0o644)
			r, err := git.PlainOpen(work)
			if err != nil {
				t.Fatal(err)
			}
			wt, _ := r.Worktree()
			if err := wt.Checkout(&git.CheckoutOptions{Branch: plumbing.NewBranchReferenceName("child"), Create: true, Keep: true}); err != nil {
				t.Fatal(err)
			}
			wt.AddWithOptions(&git.AddOptions{All: true})
			sig := &object.Signature{Name: "c", Email: "c@x", When: time.Unix(1, 0)}
			if _, err := wt.Commit("child", &git.CommitOptions{Author: sig, Committer: sig}); err != nil {
				t.Fatal(err)
			}
			writeFile(t, filepath.Join(work, ".git", "index.lock"), "lock", 0o644)
		})
		if err != nil {
			t.Fatal(err)
		}
		got := files(t, dir, tree)
		want := spec("keep.txt", "k\n", "old.log", "log\n", "src/a.txt", "a\n", "c.txt", "c")
		if err := testkit.EqualFiles(got, want); err != nil {
			t.Fatal(err)
		}
		_, _, tree2, err := snap(t, false, func(work string) {
			os.RemoveAll(filepath.Join(work, ".git"))
			writeFile(t, filepath.Join(work, "c.txt"), "c", 0o644)
		})
		if err != nil || tree2 != tree {
			t.Fatalf("removed .git: %s vs %s %v", tree2, tree, err)
		}
	})
	t.Run("runtime-excluded", func(t *testing.T) {
		dir, p, tree, err := snap(t, true, func(work string) {})
		if err != nil || p.RuntimeDir == "" || tree != bm.TreeHash {
			t.Fatalf("runtime %+v %s %v", p, tree, err)
		}
		writeFile(t, filepath.Join(dir, WorkName, p.RuntimeDir, CodexFinalProbe), "final", 0o600)
		tree2, _, err := Snapshot(context.Background(), SnapshotInput{GOOS: runtime.GOOS, TaskDir: dir, Map: p.Map, RuntimeDir: p.RuntimeDir})
		if err != nil || tree2 != bm.TreeHash {
			t.Fatalf("runtime content leaked: %s %v", tree2, err)
		}
	})
	t.Run("refusals", func(t *testing.T) {
		for name, c := range map[string]struct {
			edit func(work string)
			code string
		}{
			"nested-repo":  {func(w string) { writeFile(t, filepath.Join(w, "sub", ".git", "HEAD"), "x", 0o644) }, contract.PubErrUnsupportedRepository},
			"nested-alias": {func(w string) { writeFile(t, filepath.Join(w, "sub", ".GIT", "HEAD"), "x", 0o644) }, contract.PubErrUnsupportedRepository},
			"lfs-pointer": {func(w string) {
				writeFile(t, filepath.Join(w, "big.bin"), "version https://git-lfs.github.com/spec/v1\noid sha256:"+
					"4d7a214614ab2935c943f9e0ff69d22eadbb8f32b1258daaa5e2ca24d17e2393\nsize 12345\n", 0o644)
			}, contract.PubErrUnsupportedRepository},
			"fifo": {func(w string) {
				if err := syscall.Mkfifo(filepath.Join(w, "pipe"), 0o644); err != nil {
					t.Fatal(err)
				}
			}, contract.PubErrSnapshotFailed},
			"escaping-link": {func(w string) { os.Symlink("../../../outside", filepath.Join(w, "esc")) }, contract.PubErrSnapshotFailed},
			"absolute-link": {func(w string) { os.Symlink("/etc/passwd", filepath.Join(w, "abs")) }, contract.PubErrSnapshotFailed},
		} {
			_, _, _, err := snap(t, false, c.edit)
			var se *SnapshotError
			if !errors.As(err, &se) || se.Code != c.code || se.Unwrap() == nil || se.Error() == "" {
				t.Fatalf("%s: %v (code %s, want %s)", name, err, PublicationCode(err), c.code)
			}
		}
		// An ignored nested repository does not travel and is no refusal.
		if _, _, _, err := snap(t, false, func(w string) {
			writeFile(t, filepath.Join(w, ".gitignore"), "vendor/\n", 0o644)
			writeFile(t, filepath.Join(w, "vendor", "x", ".git", "HEAD"), "x", 0o644)
		}); err != nil {
			t.Fatal(err)
		}
		// A missing database and a cancelled snapshot.
		if _, _, err := Snapshot(context.Background(), SnapshotInput{GOOS: runtime.GOOS, TaskDir: t.TempDir()}); !isCode(err, contract.PubErrStorageFailed) && !isCode(err, contract.PubErrSnapshotFailed) {
			t.Fatalf("missing database: %v", err)
		}
		dir, p := prepared(t, m, &fetcher{src: src}, binding(base), false)
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		if _, _, err := Snapshot(ctx, SnapshotInput{GOOS: runtime.GOOS, TaskDir: dir, Map: p.Map}); !errors.Is(err, context.Canceled) {
			t.Fatalf("cancelled snapshot: %v", err)
		}
	})
	t.Run("codes", func(t *testing.T) {
		for err, want := range map[error]string{
			&SnapshotError{Code: contract.PubErrMetadataOverflow}:                                           contract.PubErrMetadataOverflow,
			contract.TransferError(contract.CodeInvalidArgument, contract.ReasonUnsupportedRepository, "x"): contract.PubErrUnsupportedRepository,
			contract.TransferError(contract.CodeInternal, contract.PubErrMetadataOverflow, "x"):             contract.PubErrMetadataOverflow,
			contract.TransferError(contract.CodeUnavailable, contract.ReasonStorageFailure, "x"):            contract.PubErrStorageFailed,
			&os.PathError{Op: "write", Path: "x", Err: syscall.ENOSPC}:                                      contract.PubErrStorageFailed,
			errors.New("anything else"): contract.PubErrSnapshotFailed,
		} {
			if got := PublicationCode(err); got != want {
				t.Fatalf("PublicationCode(%v) = %s, want %s", err, got, want)
			}
		}
	})
	t.Run("result-commit", func(t *testing.T) {
		tree := bm.TreeHash
		c1, h1, err := ResultCommit(taskID(1), "worker-a", "m 1", contract.TaskFailed, &base, tree)
		if err != nil {
			t.Fatal(err)
		}
		_, again, _ := ResultCommit(taskID(1), "worker-a", "m 1", contract.TaskFailed, &base, tree)
		_, other, _ := ResultCommit(taskID(2), "worker-a", "m 1", contract.TaskFailed, &base, tree)
		if h1 != again || h1 == other || len(c1.ParentHashes) != 1 || c1.ParentHashes[0] != base || c1.Author.When.Unix() != 0 ||
			c1.Author.Name != contract.TaskResultCommitName || c1.Committer.Email != contract.TaskResultCommitEmail ||
			c1.Message != contract.TaskResultMessage(taskID(1), "worker-a", "m 1", contract.TaskFailed) {
			t.Fatalf("result commit %+v", c1)
		}
		if root, _, _ := ResultCommit(taskID(1), "worker-a", "m", contract.TaskSucceeded, nil, tree); len(root.ParentHashes) != 0 {
			t.Fatal("the empty base's result has a parent")
		}
		if _, _, err := ResultCommit(taskID(1), "worker\na", "m", contract.TaskSucceeded, nil, tree); !isCode(err, contract.PubErrSnapshotFailed) {
			t.Fatalf("control character: %v", err)
		}
		// Stored and synced with its closure; the push selection excludes
		// what the plane advertised.
		dir, _ := prepared(t, m, &fetcher{src: src}, binding(base), false)
		if h, err := StoreCommit(context.Background(), dir, c1); err != nil || h != h1 {
			t.Fatal(err)
		}
		db, _ := workspacetransfer.OpenTaskDB(filepath.Join(dir, ObjectsName))
		defer db.Close()
		all, err := PushObjects(db.Store(), h1, nil)
		if err != nil {
			t.Fatal(err)
		}
		some, err := PushObjects(db.Store(), h1, []plumbing.Hash{base})
		if err != nil || len(some) != 1 || some[0] != h1 || len(all) <= len(some) {
			t.Fatalf("push selection %d of %d %v", len(some), len(all), err)
		}
		if _, err := StoreCommit(context.Background(), t.TempDir(), c1); !isCode(err, contract.PubErrStorageFailed) {
			t.Fatalf("missing database: %v", err)
		}
		// A commit whose parent closure is absent is not stored as complete.
		orphan := *c1
		orphan.ParentHashes = []plumbing.Hash{plumbing.NewHash("1111111111111111111111111111111111111111")}
		if _, err := StoreCommit(context.Background(), dir, &orphan); err == nil {
			t.Fatal("a commit without its parent closure was stored as complete")
		}
	})
}

// CodexFinalProbe is a file name a file-based adapter writes in its owned
// runtime directory.
const CodexFinalProbe = "callsheet-final.txt"

func taskID(n int) string { return "t_" + string(rune('0'+n)) + "0000000000000000000000000000000"[:31] }
