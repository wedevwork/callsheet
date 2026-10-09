//go:build linux || darwin

package mcpqual

import (
	"io/fs"
	"os"
	"syscall"
)

// Open never follows a final symbolic link (O_NOFOLLOW) and never blocks
// on a FIFO swapped in after the lookup (O_NONBLOCK; it is then refused as
// not the looked-up file).
func (osApprovalFS) Open(name string) (approvalFile, error) {
	return os.OpenFile(name, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
}

// createNoFollow creates name as a new regular file, mode 0600, never
// following a final symbolic link and never opening an existing entry
// (design decoder-enrollment B2, FP-20).
func createNoFollow(name string) (*os.File, error) {
	return os.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_EXCL|syscall.O_NOFOLLOW, 0o600)
}

// residueSocket reports whether a no-follow lookup is a Unix socket (not
// a link, FIFO, device, directory or regular file): the type check of a
// Cursor worker socket residue (design decoder-enrollment B3, FP-25). The
// socket is never opened, connected to or removed.
func residueSocket(info fs.FileInfo) (bool, error) {
	return info.Mode().Type() == fs.ModeSocket, nil
}
