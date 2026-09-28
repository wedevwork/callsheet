//go:build !linux && !darwin

package sidecar

import "testing"

// guardianEntrypoint has no guardian to drive where task execution is
// unsupported (the platform seam refuses every start first).
func guardianEntrypoint(t *testing.T) { t.Log("no task guardian on this system") }

// journalCrashWindows needs the Unix task journal (FIFOs).
func journalCrashWindows(t *testing.T) { t.Log("no task journal on this system") }
