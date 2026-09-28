//go:build !linux && !darwin

package testkit

import (
	"net"
	"testing"
)

// RefusingAddr returns a loopback address nothing listens on (a released
// ephemeral port: best effort where the bound-socket reservation is not
// available; callsheet's components are Linux/macOS only).
func RefusingAddr(t testing.TB) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	ln.Close()
	return addr
}
