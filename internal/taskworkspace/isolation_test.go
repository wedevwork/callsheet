package taskworkspace

import (
	"context"
	"go/parser"
	gotoken "go/token"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"testing"

	"github.com/wedevwork/callsheet/internal/testkit"
)

// TestIsolation is UT-B9 (iteration 10b): two tasks of one base on one
// node share only the locked disposable cache. Each owns independent
// history objects, repository metadata, index and worktree (no hardlink,
// alternates or shared file), one child rewriting its own repository or
// the cache being evicted changes nothing for the other; and no package of
// the workspace execution executes a program or registers a global git
// transport. Delegated from tests/function
// (TestTaskWorkspaceIsolation/no-git-subprocess). Do not rename it.
func TestIsolation(t *testing.T) {
	t.Parallel()
	t.Run("independent-copies", func(t *testing.T) {
		src := newSource(t)
		base := src.commit(spec("shared.txt", "base\n", "k", "k"))
		m, root := openCache(t, 0)
		f := &fetcher{src: src}
		da, pa := prepared(t, m, f, binding(base), false)
		db, pb := prepared(t, m, f, binding(base), false)
		// No file below either task directory shares an inode with the
		// other's or the cache's.
		inodes := func(dir string) map[uint64]string {
			out := map[uint64]string{}
			filepath.Walk(dir, func(p string, fi os.FileInfo, err error) error {
				if err == nil && fi.Mode().IsRegular() {
					if st, ok := fi.Sys().(*syscall.Stat_t); ok {
						out[uint64(st.Ino)] = p
						if st.Nlink != 1 {
							t.Errorf("%s has %d links", p, st.Nlink)
						}
					}
				}
				return nil
			})
			return out
		}
		ia, ib, ic := inodes(da), inodes(db), inodes(filepath.Join(root, CacheDirName))
		for ino, p := range ia {
			if q, ok := ib[ino]; ok {
				t.Fatalf("%s and %s share an inode", p, q)
			}
			if q, ok := ic[ino]; ok {
				t.Fatalf("%s and the cache's %s share an inode", p, q)
			}
		}
		// A rewrites its repository and work; the cache disappears.
		wa := filepath.Join(da, WorkName)
		writeFile(t, filepath.Join(wa, "shared.txt"), "A", 0o644)
		os.RemoveAll(filepath.Join(wa, ".git"))
		os.MkdirAll(filepath.Join(wa, ".git", "objects"), 0o755)
		writeFile(t, filepath.Join(wa, ".git", "HEAD"), "garbage", 0o644)
		os.RemoveAll(filepath.Join(root, CacheDirName, instA))
		wb := filepath.Join(db, WorkName)
		writeFile(t, filepath.Join(wb, "b.txt"), "B", 0o644)
		ta, _, err := Snapshot(context.Background(), SnapshotInput{GOOS: runtime.GOOS, TaskDir: da, Map: pa.Map})
		if err != nil {
			t.Fatal(err)
		}
		tb, _, err := Snapshot(context.Background(), SnapshotInput{GOOS: runtime.GOOS, TaskDir: db, Map: pb.Map})
		if err != nil {
			t.Fatal(err)
		}
		if b, _ := os.ReadFile(filepath.Join(wb, "shared.txt")); string(b) != "base\n" || ta == tb {
			t.Fatalf("B saw A's change %q (trees %s %s)", b, ta, tb)
		}
		// B's child repository still names the base.
		if head, _ := os.ReadFile(filepath.Join(wb, ".git", "HEAD")); string(head) != base.String()+"\n" {
			t.Fatalf("B's HEAD %q", head)
		}
	})
	t.Run("no-exec-no-global-transport", func(t *testing.T) {
		root := testkit.MustRepoRoot(t)
		forbidden := []string{"os/exec", "github.com/go-git/go-git/v5/plumbing/transport/client", "github.com/go-git/go-git/v5/plumbing/transport/http",
			"github.com/go-git/go-git/v5/plumbing/transport/ssh", "github.com/go-git/go-git/v5/plumbing/transport/file", "github.com/go-git/go-git/v5/plumbing/transport/git"}
		var files []string
		for _, glob := range []string{"internal/taskworkspace/*.go", "internal/taskpublication/*.go", "internal/workspacetransfer/task*.go", "internal/sidecar/task_workspace.go",
			"internal/plane/task_workspace.go", "internal/workspace/tasks.go", "internal/client/node_workspace.go"} {
			m, _ := filepath.Glob(filepath.Join(root, glob))
			files = append(files, m...)
		}
		n := 0
		for _, f := range files {
			if strings.HasSuffix(f, "_test.go") {
				continue
			}
			n++
			src, err := os.ReadFile(f)
			if err != nil {
				t.Fatal(err)
			}
			pf, err := parser.ParseFile(gotoken.NewFileSet(), f, src, parser.ImportsOnly)
			if err != nil {
				t.Fatal(err)
			}
			for _, imp := range pf.Imports {
				p, _ := strconv.Unquote(imp.Path.Value)
				for _, bad := range forbidden {
					if p == bad {
						t.Fatalf("%s imports %s", f, p)
					}
				}
			}
			if strings.Contains(string(src), "InstallProtocol(") {
				t.Fatalf("%s registers a global git transport", f)
			}
		}
		if n < 8 {
			t.Fatalf("only %d production files checked", n)
		}
	})
}
