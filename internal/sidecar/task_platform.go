package sidecar

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/wedevwork/callsheet/internal/adapter"
)

// taskPlatform is the OS-parameter seam of task execution (iteration
// 05). Every OS-specific decision takes the explicit goos that Run
// received from the CLI's runtime entrypoint; nothing here reads the
// host OS. Linux and Darwin both run children with Setpgid (PGID = PID).
type taskPlatform struct {
	goos string
	// physicalScratch resolves the scratch directory's symlinks: Darwin's
	// per-user temp root lives under /var, a symlink to /private/var, and
	// the child's PWD must name the directory the kernel reports as its
	// working directory.
	physicalScratch bool
}

// taskPlatformFor returns the seam for goos; unsupported systems are
// rejected here, before any task preparation.
func taskPlatformFor(goos string) (taskPlatform, error) {
	switch goos {
	case "linux":
		return taskPlatform{goos: goos}, nil
	case "darwin":
		return taskPlatform{goos: goos, physicalScratch: true}, nil
	}
	return taskPlatform{}, fmt.Errorf("task execution is unsupported on %q; supported: linux, darwin", goos)
}

// scratchPrefix starts every task's scratch directory name.
const scratchPrefix = "callsheet-task-"

// scratch creates the task's fresh private working directory under the
// host's normal temp root (TMPDIR honored): mode 0700, an absolute
// cleaned native path, one unique directory per task.
func (p taskPlatform) scratch(tempRoot, taskID string) (string, error) {
	dir, err := os.MkdirTemp(tempRoot, scratchPrefix+taskID+"-")
	if err != nil {
		return "", err
	}
	fail := func(err error) (string, error) {
		os.Remove(dir)
		return "", err
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		return fail(err)
	}
	abs, err := filepath.Abs(dir)
	if err != nil {
		return fail(err)
	}
	if p.physicalScratch {
		if abs, err = filepath.EvalSymlinks(abs); err != nil {
			return fail(err)
		}
	}
	return filepath.Clean(abs), nil
}

// removeScratch deletes the known private directory of a task after its
// group cleanup was verified, without following symlinks out of it.
func removeScratch(dir string) error {
	if fi, err := os.Lstat(dir); err != nil || !fi.IsDir() {
		return fmt.Errorf("the scratch directory is not a directory")
	}
	return os.RemoveAll(dir)
}

// childEnv is a task child's environment: the sidecar's, without the
// fixture file-descriptor variables and without any PWD, then PWD set to
// the scratch directory. No coordinator-supplied value is added.
func childEnv(environ []string, dir string) []string {
	out := make([]string, 0, len(environ)+1)
	for _, kv := range adapterChildEnv(environ) {
		if len(kv) >= 4 && kv[:4] == "PWD=" {
			continue
		}
		out = append(out, kv)
	}
	return append(out, "PWD="+dir)
}

// adapterChildEnv is the adapter's shared fixture-variable filter.
var adapterChildEnv = adapter.ChildEnv
