//go:build !linux && !darwin

package sidecar

import "github.com/wedevwork/callsheet/internal/adapter"

// The final-file helper exists only on Linux and macOS, where tasks run.
// Elsewhere every operation refuses: no task starts, so none needs it.

// finalDir is the unsupported placeholder handle.
type finalDir struct{}

func openFinalDir(string) (finalDir, error) { return finalDir{}, errUnsupportedTasks }

func (finalDir) absent(string) error { return errUnsupportedTasks }

func (finalDir) close() {}

// readFinalFile refuses: no final file can exist here.
func readFinalFile(*finalSource, adapter.FinalExtractor) (adapter.FinalMessage, error) {
	return failed(adapter.FinalUnreadable)
}
