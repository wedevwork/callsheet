package taskworkspace

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/filemode"

	"github.com/wedevwork/callsheet/internal/contract"
	"github.com/wedevwork/callsheet/internal/testkit"
)

// TestPrepare is UT-B4 (iteration 10b): the private checkout from the
// cache's authorized fetch (exact base bytes, modes and links, a detached
// HEAD at the base with an independent repository; an unborn HEAD for the
// empty base), the owned runtime directory, and the fixed reasons of every
// failure, with nothing released. Delegated from tests/function
// (TestTaskWorkspaceCheckout/matrix). Do not rename it.
func TestPrepare(t *testing.T) {
	t.Parallel()
	t.Run("base", func(t *testing.T) {
		src := newSource(t)
		files := spec("README.md", "# r\n", "src/a.txt", "a\n", "callsheet-final.txt", "project data\n")
		files["run.sh"] = testkit.FileSpec{Mode: filemode.Executable, Content: []byte("#!/bin/sh\n")}
		files["link"] = testkit.FileSpec{Mode: filemode.Symlink, Content: []byte("src/a.txt")}
		base := src.commit(files)
		m, _ := openCache(t, 0)
		dir, p := prepared(t, m, &fetcher{src: src}, binding(base), true)
		work := filepath.Join(dir, WorkName)
		for name, want := range map[string]string{"README.md": "# r\n", "src/a.txt": "a\n", "callsheet-final.txt": "project data\n", "run.sh": "#!/bin/sh\n"} {
			b, err := os.ReadFile(filepath.Join(work, name))
			if err != nil || string(b) != want {
				t.Fatalf("%s = %q %v", name, b, err)
			}
		}
		if fi, _ := os.Stat(filepath.Join(work, "run.sh")); fi.Mode().Perm() != 0o755 {
			t.Fatalf("run.sh mode %v", fi.Mode())
		}
		if fi, _ := os.Stat(filepath.Join(work, "README.md")); fi.Mode().Perm() != 0o644 {
			t.Fatalf("README.md mode %v", fi.Mode())
		}
		if l, err := os.Readlink(filepath.Join(work, "link")); err != nil || l != "src/a.txt" {
			t.Fatalf("link %q %v", l, err)
		}
		head, err := os.ReadFile(filepath.Join(work, ".git", "HEAD"))
		if err != nil || string(head) != base.String()+"\n" {
			t.Fatalf("HEAD %q %v", head, err)
		}
		// The child's repository is independent: its own objects, no
		// alternates, no remotes or hooks.
		if _, err := os.Stat(filepath.Join(work, ".git", "objects", "info", "alternates")); !os.IsNotExist(err) {
			t.Fatal("the child repository has alternates")
		}
		if cfg, err := os.ReadFile(filepath.Join(work, ".git", "config")); err != nil || strings.Contains(string(cfg), "[remote") || strings.Contains(string(cfg), "hooksPath") {
			t.Fatalf("child config %q %v", cfg, err)
		}
		if !contract.ValidRuntimeDir(p.RuntimeDir) || p.Files != 5 || p.Map == nil {
			t.Fatalf("prepared %+v", p)
		}
		if fi, err := os.Stat(filepath.Join(work, p.RuntimeDir)); err != nil || !fi.IsDir() || fi.Mode().Perm() != 0o700 {
			t.Fatalf("runtime dir %v %v", fi, err)
		}
		if fi, err := os.Stat(filepath.Join(dir, ObjectsName)); err != nil || !fi.IsDir() {
			t.Fatal("no trusted object database")
		}
	})
	t.Run("empty", func(t *testing.T) {
		m, _ := openCache(t, 0)
		b := contract.WorkspaceBinding{Name: "proj", Instance: instA, BaseSelector: contract.SelectorEmpty}
		dir, p := prepared(t, m, &fetcher{src: newSource(t), refs: map[string]plumbing.Hash{}}, b, false)
		work := filepath.Join(dir, WorkName)
		head, err := os.ReadFile(filepath.Join(work, ".git", "HEAD"))
		es, _ := os.ReadDir(work)
		if err != nil || string(head) != "ref: refs/heads/main\n" || len(es) != 1 || p.RuntimeDir != "" || p.Files != 0 {
			t.Fatalf("empty checkout HEAD %q entries %d %+v", head, len(es), p)
		}
	})
	t.Run("reasons", func(t *testing.T) {
		src := newSource(t)
		base := src.commit(spec("a", "a"))
		run := func(f *fetcher, b contract.WorkspaceBinding, ctx context.Context) error {
			dir := t.TempDir()
			os.Mkdir(filepath.Join(dir, WorkName), 0o700)
			m, _ := openCache(t, 0)
			_, err := Prepare(ctx, PrepareInput{GOOS: runtime.GOOS, TaskDir: dir, Binding: b, Remote: f, Cache: m})
			return err
		}
		bg := context.Background()
		empty := contract.WorkspaceBinding{Name: "proj", Instance: instA, BaseSelector: contract.SelectorEmpty}
		for name, c := range map[string]struct {
			f    *fetcher
			b    contract.WorkspaceBinding
			want string
		}{
			"empty-with-refs": {&fetcher{src: src, refs: map[string]plumbing.Hash{"refs/heads/base": base}}, empty, contract.ReasonWorkspaceUnavailable},
			"empty-denied":    {&fetcher{src: src, refsErr: errDenied}, empty, contract.ReasonWorkspaceUnavailable},
			"denied":          {&fetcher{src: src, before: func(int, context.Context) error { return errDenied }}, binding(base), contract.ReasonWorkspaceUnavailable},
			"base-unavailable": {&fetcher{src: src, before: func(int, context.Context) error {
				return contract.TaskError(contract.CodeConflict, "", contract.ReasonWorkspaceBaseUnavailable, "gone")
			}}, binding(base), contract.ReasonWorkspaceBaseUnavailable},
			"local":            {&fetcher{src: src, before: func(int, context.Context) error { return errors.New("disk") }}, binding(base), contract.ReasonWorkspaceCheckoutFailed},
			"corrupt-response": {&fetcher{src: src, garbage: func(int) bool { return true }}, binding(base), contract.ReasonWorkspaceCheckoutFailed},
		} {
			if got := PrepareReason(run(c.f, c.b, bg)); got != c.want {
				t.Fatalf("%s: reason %s, want %s", name, got, c.want)
			}
		}
		// The preparation deadline is workspace_prepare_timeout; a
		// cancellation is returned as such.
		ctx, cancel := context.WithTimeout(bg, 0)
		defer cancel()
		if got := PrepareReason(run(&fetcher{src: src}, binding(base), ctx)); got != contract.ReasonWorkspacePrepareTimeout {
			t.Fatalf("deadline reason %s", got)
		}
		cctx, ccancel := context.WithCancel(bg)
		ccancel()
		if err := run(&fetcher{src: src}, binding(base), cctx); !errors.Is(err, context.Canceled) {
			t.Fatalf("cancelled: %v", err)
		}
		// A base this native checkout cannot represent refuses the checkout.
		esc := src.commit(map[string]testkit.FileSpec{"out": {Mode: filemode.Symlink, Content: []byte("../../outside")}})
		err := run(&fetcher{src: src}, binding(esc), bg)
		var pe *PrepareError
		if !errors.As(err, &pe) || pe.Reason != contract.ReasonWorkspaceCheckoutFailed || !strings.Contains(pe.Error(), pe.Reason) || pe.Unwrap() == nil {
			t.Fatalf("escaping link: %v", err)
		}
		// A task directory whose objects already exist fails before any
		// fetch.
		dir := t.TempDir()
		os.Mkdir(filepath.Join(dir, ObjectsName), 0o700)
		os.WriteFile(filepath.Join(dir, ObjectsName, "x"), nil, 0o600)
		m, _ := openCache(t, 0)
		f := &fetcher{src: src}
		if _, err := Prepare(bg, PrepareInput{GOOS: runtime.GOOS, TaskDir: dir, Binding: binding(base), Remote: f, Cache: m}); PrepareReason(err) != contract.ReasonWorkspaceCheckoutFailed || f.nCalls() != 0 {
			t.Fatalf("existing objects: %v (%d fetches)", err, f.nCalls())
		}
		if PrepareReason(errors.New("other")) != contract.ReasonWorkspaceCheckoutFailed {
			t.Fatal("default reason")
		}
	})
	t.Run("runtime-collision", func(t *testing.T) {
		work := t.TempDir()
		a, err := allocRuntime(work)
		if err != nil {
			t.Fatal(err)
		}
		b, err := allocRuntime(work)
		if err != nil || a == b || !contract.ValidRuntimeDir(b) {
			t.Fatalf("runtime dirs %s %s %v", a, b, err)
		}
		if _, err := allocRuntime(filepath.Join(work, "missing")); err == nil {
			t.Fatal("a runtime dir was allocated in a missing work directory")
		}
	})
}
