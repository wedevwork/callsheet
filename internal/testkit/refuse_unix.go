//go:build linux || darwin

package testkit

import (
	"strconv"
	"syscall"
	"testing"
)

// RefusingAddr returns a loopback address that refuses every connection
// for the rest of the test: a TCP socket bound to it but never listening
// (so connections get RST) and held until cleanup (so no other listener,
// such as a parallel test's server, can take the port meanwhile). It
// replaces the released-ephemeral-port pattern, whose port another
// listener may reuse.
func RefusingAddr(t testing.TB) string {
	t.Helper()
	fd, err := syscall.Socket(syscall.AF_INET, syscall.SOCK_STREAM, 0)
	if err != nil {
		t.Fatalf("refusing address: socket: %v", err)
	}
	syscall.CloseOnExec(fd)
	t.Cleanup(func() { syscall.Close(fd) })
	if err := syscall.Bind(fd, &syscall.SockaddrInet4{Addr: [4]byte{127, 0, 0, 1}}); err != nil {
		t.Fatalf("refusing address: bind: %v", err)
	}
	sa, err := syscall.Getsockname(fd)
	in, ok := sa.(*syscall.SockaddrInet4)
	if err != nil || !ok || in.Port == 0 {
		t.Fatalf("refusing address: getsockname: %v", err)
	}
	return "127.0.0.1:" + strconv.Itoa(in.Port)
}
