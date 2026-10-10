package reale2e

import (
	"errors"
	"time"
)

// The manual startup gate (design 12a-real-e2e, FP-1): live startup needs
// the explicit opt-in, an absent CI variable (present-empty counts as
// present) and the linux host OS value, checked in that order before any
// filesystem, executable or network activity.

// EnvOptIn must be exactly "1" for a live run.
const EnvOptIn = "CALLSHEET_REAL_E2E"

// EnvCI present in any form refuses a live run.
const EnvCI = "CI"

// Gate refusals: fixed text.
var (
	ErrOptIn    = errors.New("reale2e run refused: set " + EnvOptIn + "=1 to start the paid real-worker exercise")
	ErrCI       = errors.New("reale2e run refused: CI is set (even empty); the real-worker exercise never runs in CI")
	ErrPlatform = errors.New("reale2e run refused: live execution is linux only (the grok worker posture is refused on other systems)")
)

// Gate decides live startup from injected inputs only.
func Gate(goos string, getenv func(string) string, lookupEnv func(string) (string, bool)) error {
	if getenv(EnvOptIn) != "1" {
		return ErrOptIn
	}
	if _, ok := lookupEnv(EnvCI); ok {
		return ErrCI
	}
	if goos != "linux" {
		return ErrPlatform
	}
	return nil
}

// Policy bounds (design 12a-real-e2e, Failure and lifecycle): policy, not
// measured performance claims.
const (
	PreflightBound     = 2 * time.Minute
	FeatureTaskBound   = 10 * time.Minute
	OwnerDecisionBound = 20 * time.Minute
	AttemptBound       = 90 * time.Minute
	CleanupBound       = 2 * time.Minute
	FinalTestBound     = 60 * time.Second
)

// Bounds is the cost and time statement shown before opt-in startup (in
// the usage text and at the start of every run).
const Bounds = `Cost and time bounds (policy, not measurements):
  - four paid authentication preflight tasks (one per role/pair), each at most 2m;
  - four paid feature tasks (designer, design reviewer, coder, code reviewer), each at most 10m;
  - the owner decision at most 20m; the whole attempt at most 90m from preflight start,
    then at most 2m of cleanup; the final local tests at most 60s;
  - the interactive Claude Code coordinator adds vendor-dependent turns and tool calls;
  - no automatic retries or fix loops; no token-price ceiling is claimed.
`
