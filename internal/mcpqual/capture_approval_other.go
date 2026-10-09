//go:build !linux && !darwin

package mcpqual

import (
	"io/fs"
	"os"
)

// Open is unavailable outside linux and darwin, where capture never runs.
func (osApprovalFS) Open(string) (approvalFile, error) { return nil, errUnsupportedOS }

// createNoFollow is unavailable outside linux and darwin.
func createNoFollow(string) (*os.File, error) { return nil, errUnsupportedOS }

// residueSocket is unavailable outside linux and darwin: a residue check
// there reports the unsupported platform (design decoder-enrollment B3,
// FP-25), never a guessed type.
func residueSocket(fs.FileInfo) (bool, error) { return false, errUnsupportedOS }
