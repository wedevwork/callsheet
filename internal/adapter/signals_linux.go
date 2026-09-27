//go:build linux

package adapter

import "syscall"

// nativeSignalEntries is the Linux table: the portable set plus SIGSTKFLT
// and SIGPWR; SIGIOT, SIGCLD and SIGPOLL canonicalize to SIGABRT, SIGCHLD
// and SIGIO. Linux has no SIGEMT or SIGINFO.
var nativeSignalEntries = append(portableSignals(),
	signalEntry{syscall.SIGSTKFLT, "SIGSTKFLT"},
	signalEntry{syscall.SIGPWR, "SIGPWR"},
	signalEntry{syscall.SIGIOT, "SIGABRT"},
	signalEntry{syscall.SIGCLD, "SIGCHLD"},
	signalEntry{syscall.SIGPOLL, "SIGIO"},
)
