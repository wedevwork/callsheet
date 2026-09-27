//go:build darwin

package adapter

import "syscall"

// nativeSignalEntries is the Darwin table: the portable set plus SIGEMT
// and SIGINFO; SIGIOT canonicalizes to SIGABRT. Darwin defines no
// SIGSTKFLT, SIGPWR, SIGCLD or SIGPOLL.
var nativeSignalEntries = append(portableSignals(),
	signalEntry{syscall.SIGEMT, "SIGEMT"},
	signalEntry{syscall.SIGINFO, "SIGINFO"},
	signalEntry{syscall.SIGIOT, "SIGABRT"},
)
