//go:build !linux && !darwin

package fakeadapter

import "os"

// Signal and process-group modes are Unix-only; Parse rejects them here.
const signalsSupported = false

type osSignals struct{}

func (osSignals) Notify(chan<- os.Signal) {}
func (osSignals) Stop(chan<- os.Signal)   {}

func signalName(sig os.Signal) string { return sig.String() }

func sendTerm(p *os.Process) error { return p.Kill() }
