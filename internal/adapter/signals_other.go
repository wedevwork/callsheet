//go:build !linux && !darwin

package adapter

// nativeSignalEntries is empty on unsupported systems: every signal is
// SIGUNKNOWN there, and the platform seam rejects task execution first.
var nativeSignalEntries []signalEntry
