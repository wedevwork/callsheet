//go:build !linux && !darwin

package mcpqual

import "os"

// Open is unavailable outside linux and darwin, where capture never runs.
func (osApprovalFS) Open(string) (approvalFile, error) { return nil, errUnsupportedOS }

// createNoFollow is unavailable outside linux and darwin.
func createNoFollow(string) (*os.File, error) { return nil, errUnsupportedOS }
