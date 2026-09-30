//go:build linux || darwin

package sidecar

import (
	"errors"

	"golang.org/x/sys/unix"

	"github.com/wedevwork/callsheet/internal/adapter"
)

// finalDir is a retained scratch directory handle (a raw descriptor,
// close-on-exec, never handed to a child).
type finalDir struct{ fd int }

// retryEINTR repeats a system call interrupted by a signal.
func retryEINTR[T any](f func() (T, error)) (T, error) {
	for {
		v, err := f()
		if !errors.Is(err, unix.EINTR) {
			return v, err
		}
	}
}

// openFinalDir opens the exact scratch directory without following a
// symbolic link.
func openFinalDir(p string) (finalDir, error) {
	fd, err := retryEINTR(func() (int, error) {
		return unix.Open(p, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	})
	if err != nil {
		return finalDir{fd: -1}, err
	}
	return finalDir{fd: fd}, nil
}

// absent requires name not to exist in the directory (no-follow,
// directory-relative lookup).
func (d finalDir) absent(name string) error {
	var st unix.Stat_t
	_, err := retryEINTR(func() (struct{}, error) {
		return struct{}{}, unix.Fstatat(d.fd, name, &st, unix.AT_SYMLINK_NOFOLLOW)
	})
	switch {
	case errors.Is(err, unix.ENOENT):
		return nil
	case err == nil:
		return errFinalExists
	}
	return err
}

func (d finalDir) close() {
	if d.fd >= 0 {
		unix.Close(d.fd)
	}
}

// finalOps are the final-file reader's system calls (tests and the
// benchmark count reads and closes through them).
type finalOps struct {
	read  func(fd int, p []byte) (int, error)
	close func(fd int) error
}

var realFinalOps = finalOps{read: unix.Read, close: unix.Close}

// readFinalFile reads fin's file into ext with the real system calls.
func readFinalFile(fin *finalSource, ext adapter.FinalExtractor) (adapter.FinalMessage, error) {
	return realFinalOps.readFinal(fin, ext)
}

// readFinal opens the basename relative to the retained directory handle
// with O_RDONLY|O_NOFOLLOW|O_NONBLOCK|O_CLOEXEC, requires a regular file
// with one link, and feeds at most MaxVendorFinalBytes+1 bytes in fixed
// chunks to ext (the byte past the bound fails the read). A missing name is
// final_output_missing (null, never an empty answer); a link, directory,
// FIFO, device or hard-linked file final_output_unsafe; an inaccessible
// file or read error final_output_unreadable; the extractor's own code
// otherwise. The file is closed on every path; nothing is sized from the
// file's reported length.
func (o finalOps) readFinal(fin *finalSource, ext adapter.FinalExtractor) (adapter.FinalMessage, error) {
	fd, err := retryEINTR(func() (int, error) {
		return unix.Openat(fin.dir.fd, fin.name, unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_NONBLOCK|unix.O_CLOEXEC, 0)
	})
	switch {
	case errors.Is(err, unix.ENOENT):
		return failed(adapter.FinalMissing)
	case errors.Is(err, unix.ELOOP), errors.Is(err, unix.EMLINK), errors.Is(err, unix.ENXIO), errors.Is(err, unix.EOPNOTSUPP):
		// A symbolic link (O_NOFOLLOW), or a special file refusing opens.
		return failed(adapter.FinalUnsafe)
	case err != nil:
		return failed(adapter.FinalUnreadable)
	}
	defer o.close(fd)
	var st unix.Stat_t
	if _, err := retryEINTR(func() (struct{}, error) { return struct{}{}, unix.Fstat(fd, &st) }); err != nil {
		return failed(adapter.FinalUnreadable)
	}
	if st.Mode&unix.S_IFMT != unix.S_IFREG || uint64(st.Nlink) != 1 {
		return failed(adapter.FinalUnsafe)
	}
	buf := make([]byte, finalChunk)
	total := 0
	for {
		want := min(len(buf), adapter.MaxVendorFinalBytes+1-total)
		n, err := retryEINTR(func() (int, error) { return o.read(fd, buf[:want]) })
		if err != nil {
			return failed(adapter.FinalUnreadable)
		}
		if n == 0 {
			break
		}
		total += n
		if total > adapter.MaxVendorFinalBytes {
			return failed(adapter.FinalTooLarge)
		}
		ext.Feed(buf[:n])
	}
	fm := ext.Finish()
	if fm.Error != "" {
		return fm, &finalError{code: fm.Error}
	}
	return fm, nil
}
