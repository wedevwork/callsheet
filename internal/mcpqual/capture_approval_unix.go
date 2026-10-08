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
