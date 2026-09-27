package adapter

import (
	"fmt"
	"syscall"

	"github.com/wedevwork/callsheet/internal/contract"
)

// signalEntry maps one native signal constant to its wire name. Aliases
// (SIGIOT, SIGCLD, SIGPOLL where defined) map to their canonical name.
type signalEntry struct {
	sig  syscall.Signal
	name string
}

// buildSignals turns the build-selected entries into a lookup table. It
// fails on a name outside the closed wire set or on one native value
// mapped to two names; aliases of one value share their canonical name.
func buildSignals(entries []signalEntry) (map[syscall.Signal]string, error) {
	out := make(map[syscall.Signal]string, len(entries))
	for _, e := range entries {
		if !contract.ValidWireSignal(e.name) || e.name == contract.SignalUnknown {
			return nil, fmt.Errorf("adapter: signal %d maps to %q, not a wire signal name", int(e.sig), e.name)
		}
		if prev, ok := out[e.sig]; ok && prev != e.name {
			return nil, fmt.Errorf("adapter: signal %d maps to both %s and %s", int(e.sig), prev, e.name)
		}
		out[e.sig] = e.name
	}
	return out, nil
}

// nativeSignals is this platform's table (signals_linux.go,
// signals_darwin.go), keyed by its own syscall.SIG* constants: numeric
// values are never shared across platforms.
var nativeSignals = mustSignals(nativeSignalEntries)

func mustSignals(entries []signalEntry) map[syscall.Signal]string {
	t, err := buildSignals(entries)
	if err != nil {
		panic(err) // unreachable: the literal tables are checked by tests
	}
	return t
}

// SignalName returns the wire name of a native signal that ended a
// child (syscall.WaitStatus.Signal()); an unmapped value, including a
// Linux real-time signal, is contract.SignalUnknown, never a POSIX name,
// a localized String() or a success.
func SignalName(sig syscall.Signal) string { return signalNameIn(nativeSignals, sig) }

func signalNameIn(table map[syscall.Signal]string, sig syscall.Signal) string {
	if n, ok := table[sig]; ok {
		return n
	}
	return contract.SignalUnknown
}
