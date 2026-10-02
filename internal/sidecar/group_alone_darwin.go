//go:build darwin

package sidecar

import (
	"errors"
	"os"
	"syscall"
	"unsafe"
)

// Iteration 10a's macOS group-completion proof: proc_listpids with
// PROC_PGRP_ONLY (the proc_info syscall's LISTPIDS call), which the kernel
// answers by walking both its live and zombie process lists under the
// list lock and copying the IDs of the group's members. With a two-entry
// buffer, a four-byte answer naming only this guardian proves it is the
// group's last member; eight bytes means at least one other member. x/sys
// has no Darwin proc_info wrapper, so the call is the stdlib's direct
// Syscall6 trap. macOS has no subreaper: the group list is the proof.

// proc_info call and flavor constants (xnu bsd/sys/proc_info.h).
const (
	procInfoCallListPIDs = 1
	procPgrpOnly         = 2
)

// syscall6Func is syscall.Syscall6's signature (tests inject it).
type syscall6Func func(trap, a1, a2, a3, a4, a5, a6 uintptr) (r1, r2 uintptr, err syscall.Errno)

// enableGuardianSubreaper is a no-op: macOS has no child subreaper.
func enableGuardianSubreaper() error { return nil }

// probeGuardianGroup is the guardian's native groupAlone.
func probeGuardianGroup(pgid int) (groupState, error) {
	return listGroup(syscall.Syscall6, os.Getpid(), pgid)
}

// listPgrp makes the one proc_info LISTPIDS call through sys. The buffer
// pointer is converted at this named function's call site and
// uintptrescapes makes the compiler treat it as the pointer it is: the
// buffer is heap-allocated (it never moves with a growing stack) and kept
// alive until the call returns, which a uintptr passed to a function value
// alone would not guarantee (the stdlib's special handling of a direct
// syscall.Syscall6 call does not apply to an injected one).
//
//go:uintptrescapes
//go:noinline
func listPgrp(sys syscall6Func, pgid int, buf uintptr) (uintptr, syscall.Errno) {
	r1, _, errno := sys(syscall.SYS_PROC_INFO, procInfoCallListPIDs, procPgrpOnly, uintptr(pgid), 0, buf, 8)
	return r1, errno
}

// listGroup asks the kernel for at most two members of group pgid: alone
// only for exactly one entry that is self, busy for two entries, unknown
// for an error, no entry, a malformed byte count or another sole member.
func listGroup(sys syscall6Func, self, pgid int) (groupState, error) {
	if pgid <= 1 {
		return groupUnknown, errors.New("invalid process group")
	}
	var pids [2]int32
	r1, errno := listPgrp(sys, pgid, uintptr(unsafe.Pointer(&pids[0])))
	switch {
	case errno != 0:
		return groupUnknown, errno
	case r1 == 4 && int(pids[0]) == self:
		return groupAlone, nil
	case r1 == 4:
		return groupUnknown, errors.New("the group's only member is not this guardian")
	case r1 == 8:
		return groupBusy, nil
	}
	return groupUnknown, errors.New("proc_listpids returned an unexpected byte count")
}
