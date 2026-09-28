//go:build darwin

package sidecar

import "golang.org/x/sys/unix"

// setStatMode sets a stat mode (its width is the OS's).
func setStatMode(st *unix.Stat_t, m uint32) { st.Mode = uint16(m) }
