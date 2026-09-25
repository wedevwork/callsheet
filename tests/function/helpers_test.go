// Package function holds the black-box functional tests, one top-level test
// per functional point (TestFP1 ... TestFP8). Tests build real binaries with
// the local Go toolchain and never require vendor CLIs, git, Docker,
// credentials or external services.
package function

import (
	"bytes"
	"errors"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

type result struct {
	code           int
	stdout, stderr string
}

// runBin runs bin with args in dir using env and returns its outcome.
func runBin(t *testing.T, bin, dir string, env []string, args ...string) result {
	t.Helper()
	cmd := exec.Command(bin, args...)
	cmd.Dir = dir
	cmd.Env = env
	var out, errOut bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errOut
	err := cmd.Run()
	code := 0
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		code = ee.ExitCode()
	} else if err != nil {
		t.Fatalf("run %s %v: %v", bin, args, err)
	}
	return result{code, out.String(), errOut.String()}
}

// listTree returns every path below dir (relative), or nil if empty.
func listTree(t *testing.T, dir string) []string {
	t.Helper()
	var out []string
	filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			t.Fatalf("walk %s: %v", p, err)
		}
		if p != dir {
			rel, _ := filepath.Rel(dir, p)
			out = append(out, rel)
		}
		return nil
	})
	return out
}

func mkdir(t *testing.T, p string) string {
	t.Helper()
	if err := os.MkdirAll(p, 0o755); err != nil {
		t.Fatal(err)
	}
	return p
}
