package workspace

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// TestStoreFSAdapter drives the go-billy adapter over managed storage
// directly: every filesystem and file operation takes the fence and the
// path checks, links are refused, a chroot keeps both, and after the fence
// every operation fails with errFenced (iteration 09b's per-file coverage
// rule applies to this 09a file as a whole).
func TestStoreFSAdapter(t *testing.T) {
	base := tempRoot(t)
	d := fastDeps()
	var ops []string
	d.observe = func(op, p string) { ops = append(ops, op+" "+strings.TrimPrefix(p, base)) }
	fs := newStoreFS(base, d)
	if fs.Root() != base {
		t.Fatalf("root %q", fs.Root())
	}

	if err := fs.MkdirAll("objects/ab", 0o755); err != nil {
		t.Fatal(err)
	}
	f, err := fs.Create("objects/ab/cd")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.Write([]byte("hello world")); err != nil {
		t.Fatal(err)
	}
	if err := f.Truncate(5); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 5)
	if n, err := f.ReadAt(buf, 0); n != 5 || string(buf) != "hello" || (err != nil && err != io.EOF) {
		t.Fatalf("readat %d %q %v", n, buf, err)
	}
	if off, err := f.Seek(1, io.SeekStart); err != nil || off != 1 {
		t.Fatalf("seek %d %v", off, err)
	}
	if n, err := f.Read(buf[:2]); n != 2 || string(buf[:2]) != "el" || err != nil {
		t.Fatalf("read %d %v", n, err)
	}
	if err := f.Close(); err != nil || f.Close() != nil {
		t.Fatalf("close %v", err)
	}
	if fi, err := fs.Lstat("objects/ab/cd"); err != nil || fi.Size() != 5 || !fi.Mode().IsRegular() {
		t.Fatalf("lstat %v %v", fi, err)
	}
	if fi, err := fs.Stat("objects/ab"); err != nil || !fi.IsDir() {
		t.Fatalf("stat %v %v", fi, err)
	}

	// Links are refused without touching the disk.
	var unsafe errUnsafe
	if err := fs.Symlink("x", "objects/link"); !errors.As(err, &unsafe) || unsafe.path != filepath.Join(base, "objects/link") {
		t.Fatalf("symlink %v", err)
	}
	if _, err := fs.Readlink("objects/link"); !errors.As(err, &unsafe) {
		t.Fatalf("readlink %v", err)
	}
	if _, err := os.Lstat(filepath.Join(base, "objects/link")); !os.IsNotExist(err) {
		t.Fatalf("link created: %v", err)
	}

	// A chroot keeps the checks and shares the fence.
	sub, err := fs.Chroot("objects")
	if err != nil || sub.Root() != filepath.Join(base, "objects") {
		t.Fatalf("chroot %v %v", sub, err)
	}
	if ents, err := sub.ReadDir("ab"); err != nil || len(ents) != 1 || ents[0].Name() != "cd" {
		t.Fatalf("chroot readdir %v %v", ents, err)
	}
	if _, err := sub.Open("../escape"); err == nil || !strings.Contains(err.Error(), "escapes its repository") {
		t.Fatalf("chroot escape %v", err)
	}
	if err := fs.Remove("objects/ab/cd"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(base, "objects/ab/cd")); !os.IsNotExist(err) {
		t.Fatalf("not removed: %v", err)
	}
	for _, want := range []string{"mkdir /objects/ab", "create /objects/ab/cd", "stat /objects/ab/cd", "stat /objects/ab", "readdir /objects/ab", "remove /objects/ab/cd"} {
		if !slices.Contains(ops, want) {
			t.Fatalf("ops %v lack %q", ops, want)
		}
	}

	// Injected failures and path budgets refuse before the library call.
	injected := errors.New("injected")
	d.fail = func(op, _ string) error {
		if op == "remove" || op == "mkdir" {
			return injected
		}
		return nil
	}
	if err := fs.Remove("objects/ab"); !errors.Is(err, injected) {
		t.Fatalf("remove %v", err)
	}
	if err := fs.MkdirAll("objects/ef", 0o755); !errors.Is(err, injected) {
		t.Fatalf("mkdirall %v", err)
	}
	d.fail = nil
	long := strings.Repeat("x", MaxComponentBytes+1)
	if _, err := fs.Lstat(long); !errors.Is(err, errPathBudget) {
		t.Fatalf("budget %v", err)
	}

	// After the fence, a handle opened before it and every new call fail.
	g, err := fs.Create("objects/ab/gh")
	if err != nil {
		t.Fatal(err)
	}
	fs.g.fence()
	if _, err := g.Read(buf); err == nil {
		t.Fatal("read after fence")
	}
	// The fence closed the handle; its operations take no slot.
	sf := g.(*storeFile)
	for name, call := range map[string]func() error{
		"read":     func() error { _, err := sf.Read(buf); return err },
		"readat":   func() error { _, err := sf.ReadAt(buf, 0); return err },
		"write":    func() error { _, err := sf.Write(buf); return err },
		"seek":     func() error { _, err := sf.Seek(0, io.SeekStart); return err },
		"truncate": func() error { return sf.Truncate(0) },
		"remove":   func() error { return fs.Remove("objects/ab/gh") },
		"mkdirall": func() error { return fs.MkdirAll("objects/x", 0o755) },
		"stat":     func() error { _, err := fs.Stat("objects"); return err },
		"lstat":    func() error { _, err := fs.Lstat("objects"); return err },
		"readdir":  func() error { _, err := fs.ReadDir("objects"); return err },
		"open":     func() error { _, err := fs.Open("objects/ab/gh"); return err },
		"tempfile": func() error { _, err := fs.TempFile("objects", "tmp_"); return err },
		"rename":   func() error { return fs.Rename("objects/ab/gh", "objects/ab/ij") },
	} {
		if err := call(); !errors.Is(err, errFenced) {
			t.Fatalf("%s after fence: %v", name, err)
		}
	}
	if fs.g.active != 0 {
		t.Fatalf("slots leaked: %d", fs.g.active)
	}
}
