// Package processgroup is the iteration 01 process-group risk spike: graceful
// TERM to a child's process group, forced KILL after a grace period, and proof
// that no descendant retaining the group survives. Linux execution is
// mandatory; the Darwin variant compiles and must be proven on a native Mac
// before any macOS signal qualification is claimed. Its experiment helpers
// are test-only. Since iteration 05 the sidecar (and so cmd/callsheet)
// imports exactly its exported, standard-library-only teardown mechanics
// (Escalate, WaitGone and the Signaler seam) for run-owned task groups; it
// never runs the experiment or enables the Linux test subreaper.
//
// The guarantee covers descendants that keep the inherited process group. A
// process that calls setsid/setpgid can escape; process groups are not a
// sandbox.
package processgroup
