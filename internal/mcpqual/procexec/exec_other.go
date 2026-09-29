//go:build !linux && !darwin

// Package procexec is the qualification harness's real process launcher;
// it needs linux or darwin process groups.
package procexec

import (
	"errors"

	"github.com/wedevwork/callsheet/internal/mcpqual"
)

// Launcher is unavailable outside linux and darwin.
type Launcher struct{}

// Start implements mcpqual.Launcher.
func (Launcher) Start(mcpqual.ProcSpec) (mcpqual.Proc, error) {
	return nil, errors.New("procexec: process groups need linux or darwin")
}
