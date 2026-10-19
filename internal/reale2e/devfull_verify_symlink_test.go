package reale2e

import (
	"os"
	"path/filepath"
	"regexp"
	"testing"
)

// The reviewer checkout under a symlinked temporary parent (PR #57's
// macOS runner: t.TempDir is under /var, a symlink to /private/var). The
// helper refuses a non-canonical checkout path, so the rig must hand it
// the resolved path, as a real task workspace does.

// symlinkedTempDir points TMPDIR at a symlink of a real directory, so
// every t.TempDir of the test's subtests lies under a symlinked parent.
func symlinkedTempDir(t *testing.T) {
	t.Helper()
	real, err := os.MkdirTemp("", "real")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(real) })
	link := real + "-link"
	if err := os.Symlink(real, link); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Remove(link) })
	// t.TempDir prefers GOTMPDIR to TMPDIR: clear it so the symlink is
	// the parent.
	t.Setenv("GOTMPDIR", "")
	t.Setenv("TMPDIR", link)
}

// requireSymlinkedParent fails the test unless its temporary directories
// lie under a symlinked parent (one path and its own resolution differ).
func requireSymlinkedParent(t *testing.T) {
	t.Helper()
	dir := t.TempDir()
	canon, err := filepath.EvalSymlinks(dir)
	if err != nil {
		t.Fatalf("resolve %s: %v", dir, err)
	}
	if canon == dir {
		t.Fatalf("the fixture is ineffective: %s is not under a symlinked parent", dir)
	}
}

func TestDevFullReviewRunnerSymlinkedCheckout(t *testing.T) {
	symlinkedTempDir(t)
	t.Run("helper", func(t *testing.T) {
		requireSymlinkedParent(t)
		f := newReviewFix(t)
		rec, out, code := f.run()
		if code != 0 || rec.Status != reviewPassed || !regexp.MustCompile(`^[0-9a-f]{64}$`).MatchString(rec.Go.SHA256) {
			t.Fatalf("the helper under a symlinked parent: %d %s", code, out)
		}
	})
	t.Run("attempt", func(t *testing.T) {
		requireSymlinkedParent(t)
		r := newDevRig(t, happy())
		r.owner = []string{"yes 1"}
		r.expect(0, "")
	})
}
