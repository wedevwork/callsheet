//go:build !linux && !darwin

package sidecar

import "testing"

// realGroupsContract has no wrapper to drive where task execution is
// unsupported (the platform seam refuses every start first).
func realGroupsContract(t *testing.T) { t.Log("no process-group wrapper on this system") }
