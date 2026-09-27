//go:build linux || darwin

package adapter

import "syscall"

// portableSignals are the signals both supported platforms define, by
// each platform's own constants.
func portableSignals() []signalEntry {
	return []signalEntry{
		{syscall.SIGHUP, "SIGHUP"}, {syscall.SIGINT, "SIGINT"}, {syscall.SIGQUIT, "SIGQUIT"}, {syscall.SIGILL, "SIGILL"},
		{syscall.SIGTRAP, "SIGTRAP"}, {syscall.SIGABRT, "SIGABRT"}, {syscall.SIGBUS, "SIGBUS"}, {syscall.SIGFPE, "SIGFPE"},
		{syscall.SIGKILL, "SIGKILL"}, {syscall.SIGUSR1, "SIGUSR1"}, {syscall.SIGSEGV, "SIGSEGV"}, {syscall.SIGUSR2, "SIGUSR2"},
		{syscall.SIGPIPE, "SIGPIPE"}, {syscall.SIGALRM, "SIGALRM"}, {syscall.SIGTERM, "SIGTERM"}, {syscall.SIGCHLD, "SIGCHLD"},
		{syscall.SIGCONT, "SIGCONT"}, {syscall.SIGSTOP, "SIGSTOP"}, {syscall.SIGTSTP, "SIGTSTP"}, {syscall.SIGTTIN, "SIGTTIN"},
		{syscall.SIGTTOU, "SIGTTOU"}, {syscall.SIGURG, "SIGURG"}, {syscall.SIGXCPU, "SIGXCPU"}, {syscall.SIGXFSZ, "SIGXFSZ"},
		{syscall.SIGVTALRM, "SIGVTALRM"}, {syscall.SIGPROF, "SIGPROF"}, {syscall.SIGWINCH, "SIGWINCH"}, {syscall.SIGIO, "SIGIO"},
		{syscall.SIGSYS, "SIGSYS"},
	}
}
