//go:build !linux && !darwin

package mcpqual

import (
	"errors"
	"syscall"
)

var errUnsupportedOS = errors.New("mcpqual: process groups need linux or darwin")

// SysSignaler is unavailable outside linux and darwin.
type SysSignaler struct{}

// Signal implements Signaler.
func (SysSignaler) Signal(int, syscall.Signal) error { return errUnsupportedOS }

// GroupReaper is unavailable outside linux and darwin.
type GroupReaper struct {
	Sig    Signaler
	Clock  Clock
	Policy CleanupPolicy
}

// Reap implements Reaper.
func (GroupReaper) Reap(Proc) CaseCleanup { return CaseCleanup{Error: sptr(errUnsupportedOS.Error())} }
