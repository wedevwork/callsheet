//go:build !linux && !darwin

package sidecar

import (
	"errors"
	"io"
	"time"

	"github.com/wedevwork/callsheet/internal/contract"
)

// Task guardians and children are unsupported outside Linux and macOS;
// the platform seam refuses task starts first.

const groupGrace = time.Second

var errUnsupportedTasks = errors.New("task execution is unsupported on this system")

// RunTaskGuardian refuses: there is no guardian on this system.
func RunTaskGuardian(args []string, stderr io.Writer) int {
	io.WriteString(stderr, "callsheet task guardian: unsupported on this system\n")
	return 2
}

type unsupportedGuardian struct{ msgs chan contract.GuardianStatus }

func newExecGuardian(guardianSpec) guardianProc {
	c := make(chan contract.GuardianStatus)
	close(c)
	return unsupportedGuardian{msgs: c}
}

func (unsupportedGuardian) Start() error                             { return errUnsupportedTasks }
func (unsupportedGuardian) PID() int                                 { return 0 }
func (g unsupportedGuardian) Status() <-chan contract.GuardianStatus { return g.msgs }
func (unsupportedGuardian) Release()                                 {}
func (unsupportedGuardian) Revoke()                                  {}
func (unsupportedGuardian) Stop()                                    {}
func (unsupportedGuardian) Wait() procExit                           { return procExit{err: errUnsupportedTasks} }

type realGroups struct{}

func (realGroups) gone(int) error           { return errUnsupportedTasks }
func (realGroups) exists(int) (bool, error) { return false, errUnsupportedTasks }

func sendCommand(string, []byte) error { return errUnsupportedTasks }
