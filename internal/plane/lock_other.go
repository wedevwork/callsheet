//go:build !linux && !darwin

package plane

import (
	"errors"
	"os"
)

// rawFsync has no fallback outside the supported systems.
func rawFsync(*os.File) error {
	return errors.New("directory fsync fallback is supported on linux and darwin only")
}

// flockExclusive is unavailable outside the supported systems; the CLI
// rejects unsupported systems before any plane operation.
func flockExclusive(*os.File) error {
	return errors.New("advisory state locking is supported on linux and darwin only")
}
