// Package processgroup is the iteration 01 process-group risk spike: graceful
// TERM to a child's process group, forced KILL after a grace period, and proof
// that no descendant retaining the group survives. Linux execution is
// mandatory; the Darwin variant compiles and must be proven on a native Mac
// before any macOS signal qualification is claimed. It is experimental,
// test-only code and is never imported by cmd/callsheet.
//
// The guarantee covers descendants that keep the inherited process group. A
// process that calls setsid/setpgid can escape; process groups are not a
// sandbox.
package processgroup
