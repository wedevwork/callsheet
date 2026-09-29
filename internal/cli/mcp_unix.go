//go:build linux || darwin

package cli

import (
	"io"
	"os"

	"golang.org/x/sys/unix"
)

// mcpStreams returns the mcp leaf's owned stdio handles. cmd/callsheet
// passes the process's stdin and stdout files; a pipe or socket among
// them is switched to non-blocking mode and reopened as a pollable file on
// the same descriptor, so closing it interrupts a pending Read or Write
// (os.Stdin and os.Stdout of a pipe are blocking files whose Close waits
// for the pending call). Terminals, regular files and a stdout that shares
// stderr's pipe keep their mode, and their pending I/O is never waited
// for. Other readers and writers (test pipes) are closed through
// io.Closer.
func mcpStreams(in io.Reader, out, errOut io.Writer) mcpIO {
	st := ownGeneric(in, out)
	if f, ok := in.(*os.File); ok {
		st.in, st.closeIn = f, nil
		if p, ok := pollableFile(f, "mcp-stdin", nil); ok {
			st.in, st.closeIn = p, p.Close
		}
	}
	if f, ok := out.(*os.File); ok {
		st.out, st.closeOut = f, nil
		other, _ := errOut.(*os.File)
		if p, ok := pollableFile(f, "mcp-stdout", other); ok {
			st.out, st.closeOut = p, p.Close
		}
	}
	return st
}

// pollableFile returns a pollable file for f's descriptor when it is a
// pipe or socket (not the same pipe as other).
func pollableFile(f *os.File, name string, other *os.File) (*os.File, bool) {
	fd, st, flags, ok := describeFile(f)
	if !ok {
		return nil, false
	}
	if mode := uint32(st.Mode) & unix.S_IFMT; mode != unix.S_IFIFO && mode != unix.S_IFSOCK {
		return nil, false
	}
	if other != nil {
		if _, ost, _, ok := describeFile(other); ok && ost.Dev == st.Dev && ost.Ino == st.Ino {
			return nil, false
		}
	}
	if flags&unix.O_NONBLOCK != 0 {
		// Already non-blocking: Go polls it (os.Pipe, a socket).
		return f, true
	}
	if unix.SetNonblock(fd, true) != nil {
		return nil, false
	}
	return os.NewFile(uintptr(fd), name), true
}

// describeFile reads f's descriptor, status and flags without changing
// its mode (Fd would make it blocking).
func describeFile(f *os.File) (int, unix.Stat_t, int, bool) {
	var (
		fd    = -1
		st    unix.Stat_t
		flags int
		err   error
	)
	rc, serr := f.SyscallConn()
	if serr != nil {
		return 0, st, 0, false
	}
	if cerr := rc.Control(func(p uintptr) {
		fd = int(p)
		if err = unix.Fstat(fd, &st); err == nil {
			flags, err = unix.FcntlInt(p, unix.F_GETFL, 0)
		}
	}); cerr != nil || err != nil || fd < 0 {
		return 0, st, 0, false
	}
	return fd, st, flags, true
}
