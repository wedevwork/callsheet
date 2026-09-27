//go:build !linux && !darwin

package sidecar

import "errors"

// Task children are unsupported outside Linux and macOS; the platform
// seam refuses task starts first.
type unsupportedProc struct{}

func newExecProc(procSpec) taskProc { return unsupportedProc{} }

func (unsupportedProc) Start() error {
	return errors.New("task execution is unsupported on this system")
}
func (unsupportedProc) PID() int { return 0 }
func (unsupportedProc) Wait() procExit {
	return procExit{err: errors.New("task execution is unsupported on this system")}
}

type realGroups struct{}

func (realGroups) cleanup(int) error              { return errors.New("unsupported") }
func (realGroups) terminate(int, <-chan struct{}) {}
