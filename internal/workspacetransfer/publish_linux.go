//go:build linux

package workspacetransfer

import "golang.org/x/sys/unix"

// The native publication primitives of Linux (the only build-selected
// file of this package besides publish_darwin.go).

// renameNoReplaceAt atomically renames from (below the directory fromDir)
// onto to (below toDir) and fails with EEXIST when to exists:
// renameat2(RENAME_NOREPLACE). A filesystem without it fails (EINVAL);
// there is no check-then-rename fallback.
func renameNoReplaceAt(fromDir int, from string, toDir int, to string) error {
	return unix.Renameat2(fromDir, from, toDir, to, unix.RENAME_NOREPLACE)
}

// fsyncFD makes an open file's or directory's content durable.
func fsyncFD(fd int) error { return unix.Fsync(fd) }
