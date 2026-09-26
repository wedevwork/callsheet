//go:build linux || darwin

package plane

import (
	"errors"
	"os"
	"syscall"
)

// rawFsync is a plain fsync(2) of f's descriptor: the directory sync
// fallback when File.Sync reports ENOTSUP, ENOTTY or EINVAL.
func rawFsync(f *os.File) error {
	for {
		err := syscall.Fsync(int(f.Fd()))
		if !errors.Is(err, syscall.EINTR) {
			return err
		}
	}
}

// flockExclusive takes a nonblocking exclusive advisory flock on f. It is
// build-selected for the supported systems and makes no host decision.
func flockExclusive(f *os.File) error {
	for {
		err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		switch {
		case errors.Is(err, syscall.EINTR):
			continue
		case errors.Is(err, syscall.EWOULDBLOCK):
			return errLocked
		}
		return err
	}
}
