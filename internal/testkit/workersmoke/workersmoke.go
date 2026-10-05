// Package workersmoke is the opt-in real worker smoke's gate and launch
// harness (iteration 08, FP-9): the gate decides, from an injected
// environment and stat, whether the paid real-CLI smoke runs at all and
// for which vendor; the harness starts a real Callsheet test deployment
// (plane and enrolled sidecar from a built callsheet binary) with an
// explicit worker executable and dispatches one goal-and-answer task.
// tests/smoke (build tag realadaptersmoke, never enabled by CI) uses it
// with real executables; tests/function's TestRealAdapterSmokeGate uses it
// with replay stubs. It is test support: production packages never import
// it, and it never looks an executable up in PATH, logs in, changes vendor
// configuration or permissions, initializes git or relaxes sandbox flags.
package workersmoke

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"time"

	"github.com/wedevwork/callsheet/internal/adapter"
)

// The smoke's opt-in and parameters.
const (
	// EnvOptIn must be exactly "1" for the real smoke to run.
	EnvOptIn = "CALLSHEET_REAL_ADAPTER_SMOKE"
	// EnvCI nonempty always skips (a defense against paid runs in CI, even
	// when the opt-in variable is inherited).
	EnvCI = "CI"
	// Tag is the build tag that alone compiles tests/smoke.
	Tag = "realadaptersmoke"
	// OuterBound bounds one vendor's smoke task from dispatch to its
	// terminal state: a smoke safety policy, not a vendor latency promise.
	OuterBound = 2 * time.Minute
	// Command is the documented manual invocation.
	Command = "CALLSHEET_REAL_ADAPTER_SMOKE=1 \\\n" +
		"CALLSHEET_CLAUDE_PATH=/absolute/path/to/claude \\\n" +
		"CALLSHEET_CODEX_PATH=/absolute/path/to/codex \\\n" +
		"go test -tags=realadaptersmoke ./tests/smoke -run '^TestRealWorkerSmoke$' -count=1 -timeout=5m"
	// Wave2Command is the documented wave-2 invocation (iteration 11): the
	// same test with the Grok and Cursor paths, so Command stays valid.
	Wave2Command = "CALLSHEET_REAL_ADAPTER_SMOKE=1 \\\n" +
		"CALLSHEET_GROK_PATH=/absolute/path/to/grok \\\n" +
		"CALLSHEET_CURSOR_PATH=/absolute/path/to/cursor-agent \\\n" +
		"go test -tags=realadaptersmoke ./tests/smoke -run '^TestRealWorkerSmoke$' -count=1 -timeout=5m"
)

// PathEnv returns the explicit executable variable of vendor id (iteration
// 11 adds grok and cursor), or "" for an unknown vendor.
func PathEnv(id string) string {
	switch id {
	case "claude":
		return "CALLSHEET_CLAUDE_PATH"
	case "codex":
		return "CALLSHEET_CODEX_PATH"
	case "grok":
		return "CALLSHEET_GROK_PATH"
	case "cursor":
		return "CALLSHEET_CURSOR_PATH"
	}
	return ""
}

// Gate decides the smoke from an injected environment and stat (the real
// smoke passes os.Getenv and os.Stat).
type Gate struct {
	Getenv func(string) string
	Stat   func(string) (fs.FileInfo, error)
}

// Decision is the whole smoke's gate: Run, or skip for Reason. It needs no
// filesystem or process activity.
type Decision struct {
	Run    bool
	Reason string
}

// Smoke decides whether the real smoke runs at all.
func (g Gate) Smoke() Decision {
	switch {
	case g.Getenv(EnvOptIn) != "1":
		return Decision{Reason: "the real worker smoke is opt-in: set " + EnvOptIn + "=1 (it calls paid models)"}
	case g.Getenv(EnvCI) != "":
		return Decision{Reason: "CI is set: the real worker smoke never runs in CI"}
	}
	return Decision{Run: true}
}

// VendorDecision is one vendor's gate: Run with Path, skip (Skip, the
// binary is absent), or fail (Fail, a present binary that cannot be the
// explicit executable). Exactly one of Run, Skip and Fail is set.
type VendorDecision struct {
	ID   string
	Path string
	Run  bool
	Skip string
	Fail string
}

// Vendor decides vendor id's subtest (call only after Smoke ran): an
// unset or nonexistent path skips with a reason; a relative path, a
// non-regular file or one without an executable bit fails; no PATH
// lookup is ever made.
func (g Gate) Vendor(id string) VendorDecision {
	d := VendorDecision{ID: id}
	env := PathEnv(id)
	if env == "" {
		d.Fail = "unknown vendor " + id
		return d
	}
	p := g.Getenv(env)
	d.Path = p
	switch {
	case p == "":
		d.Skip = env + " is unset: no " + id + " executable to smoke on this machine"
		return d
	case !filepath.IsAbs(p):
		d.Fail = env + " must be an absolute path (no PATH lookup)"
		return d
	}
	fi, err := g.Stat(p)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		d.Skip = env + " names no file: the " + id + " executable is absent on this machine"
	case err != nil:
		d.Fail = env + " cannot be inspected: " + err.Error()
	case !fi.Mode().IsRegular():
		d.Fail = env + " is not a regular file"
	case fi.Mode().Perm()&0o111 == 0:
		d.Fail = env + " has no executable permission bit"
	default:
		d.Run = true
	}
	return d
}

// OSGate is the real smoke's gate.
func OSGate() Gate { return Gate{Getenv: os.Getenv, Stat: os.Stat} }

// Refused reports whether vendor's smoke on goos is an expected posture
// refusal rather than a paid run (iteration 11): Cursor on every OS and
// Grok outside Linux, as adapter.ValidateWorkerPosture decides from the
// explicit OS value. It needs no filesystem or process activity.
func Refused(vendor, goos string) bool { return adapter.ValidateWorkerPosture(vendor, goos) != nil }
