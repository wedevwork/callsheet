//go:build linux || darwin

package mcpqual

import (
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
