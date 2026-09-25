//go:build linux || darwin

package fakeadapter

import (
	"os"
	"os/signal"
	"syscall"
)

const signalsSupported = true

type osSignals struct{}

func (osSignals) Notify(c chan<- os.Signal) { signal.Notify(c, syscall.SIGTERM, syscall.SIGINT) }
func (osSignals) Stop(c chan<- os.Signal)   { signal.Stop(c) }

func signalName(sig os.Signal) string {
	switch sig {
	case syscall.SIGTERM:
		return "SIGTERM"
	case syscall.SIGINT:
		return "SIGINT"
	default:
		return sig.String()
	}
}

func sendTerm(p *os.Process) error { return p.Signal(syscall.SIGTERM) }
