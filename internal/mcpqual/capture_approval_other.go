//go:build !linux && !darwin

package mcpqual

// Open is unavailable outside linux and darwin, where capture never runs.
func (osApprovalFS) Open(string) (approvalFile, error) { return nil, errUnsupportedOS }
