//go:build realadaptersmoke

// Package smoke is the opt-in real worker smoke (iteration 08, FP-9). Every
// Go file here requires the realadaptersmoke build tag, which no devcheck
// or CI command enables, so ordinary and native test discovery never see
// this package. Run it by hand on a machine with the qualified CLIs:
//
//	CALLSHEET_REAL_ADAPTER_SMOKE=1 \
//	CALLSHEET_CLAUDE_PATH=/absolute/path/to/claude \
//	CALLSHEET_CODEX_PATH=/absolute/path/to/codex \
//	go test -tags=realadaptersmoke ./tests/smoke -run '^TestRealWorkerSmoke$' -count=1 -timeout=5m
//
// It calls paid models. A skip is not vendor qualification and not proof
// that M3 was demonstrated.
//
// Iteration 11 adds the wave-2 subtests with their own variables:
//
//	CALLSHEET_REAL_ADAPTER_SMOKE=1 \
//	CALLSHEET_GROK_PATH=/absolute/path/to/grok \
//	CALLSHEET_CURSOR_PATH=/absolute/path/to/cursor-agent \
//	go test -tags=realadaptersmoke ./tests/smoke -run '^TestRealWorkerSmoke$' -count=1 -timeout=5m
//
// Grok on Linux is a paid no-tools answer (pong); on macOS it is an
// expected posture refusal. Cursor on every OS is a non-model refusal
// smoke: neither refusal is vendor qualification.
package smoke

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/wedevwork/callsheet/internal/contract"
	"github.com/wedevwork/callsheet/internal/testkit"
	"github.com/wedevwork/callsheet/internal/testkit/workersmoke"
)

// TestRealWorkerSmoke dispatches the captured no-tools prompt to each
// explicitly supplied real CLI through a real local Callsheet deployment
// and expects exit 0 and the final bytes "pong" within the two-minute
// outer bound. Before its opt-in (and outside CI) it skips without any
// filesystem or process activity; an unset or absent binary skips only its
// vendor; a present but unusable or unqualified binary, and any
// authentication, model or sandbox refusal after opting in, fails.
func TestRealWorkerSmoke(t *testing.T) {
	gate := workersmoke.OSGate()
	if d := gate.Smoke(); !d.Run {
		t.Skip(d.Reason)
	}
	root := testkit.MustRepoRoot(t)
	bin := testkit.BuildBinary(t, "./cmd/callsheet", "callsheet")
	for _, c := range []struct{ vendor, capture string }{
		{"claude", "runs-scratch/claude-stdin-success"},
		{"codex", "runs-scratch/codex-skip-success"},
	} {
		t.Run(c.vendor, func(t *testing.T) {
			d := gate.Vendor(c.vendor)
			if d.Fail != "" {
				t.Fatal(d.Fail)
			}
			if !d.Run {
				t.Skip(d.Skip)
			}
			prompt, err := os.ReadFile(filepath.Join(root, "tests", "testdata", "real-adapters", "linux-2026-09-30", filepath.FromSlash(c.capture), "stdin.txt"))
			if err != nil {
				t.Fatal(err)
			}
			dir := t.TempDir()
			// The operator's own environment and vendor configuration are used
			// as they are (HOME included): the smoke never logs in, changes
			// permissions or relaxes sandbox flags.
			env := os.Environ()
			ctx, cancel := context.WithTimeout(context.Background(), workersmoke.OuterBound+time.Minute)
			defer cancel()
			res, err := workersmoke.Run(ctx, bin, dir, env, env, c.vendor, d.Path, strings.TrimSpace(string(prompt)))
			if err != nil {
				t.Fatalf("%s smoke: %v\nlogs:\n%s", c.vendor, err, res.Logs)
			}
			r := res.View.Result
			if res.View.State != contract.TaskSucceeded || r == nil || r.ExitCode == nil || *r.ExitCode != 0 || r.FinalMessage == nil || *r.FinalMessage != "pong" {
				t.Fatalf("%s smoke: state %s result %+v\nlogs:\n%s", c.vendor, res.View.State, r, res.Logs)
			}
		})
	}
	// Iteration 11 (wave 2): Grok's paid no-tools answer on Linux, and the
	// expected posture refusals (Grok on macOS, Cursor everywhere) after a
	// matching version probe, labelled as refusals, never as qualification.
	prompt, err := os.ReadFile(filepath.Join(root, "tests", "testdata", "real-adapters", "linux-2026-10-04", "runs", "cursor-stdin-success", "stdin.txt"))
	if err != nil {
		t.Fatal(err)
	}
	for _, vendor := range []string{"grok", "cursor"} {
		t.Run(vendor, func(t *testing.T) {
			d := gate.Vendor(vendor)
			if d.Fail != "" {
				t.Fatal(d.Fail)
			}
			if !d.Run {
				t.Skip(d.Skip)
			}
			env := os.Environ()
			ctx, cancel := context.WithTimeout(context.Background(), workersmoke.OuterBound+time.Minute)
			defer cancel()
			if workersmoke.Refused(vendor, runtime.GOOS) {
				ref, err := workersmoke.Refuse(ctx, bin, t.TempDir(), env, env, vendor, d.Path, runtime.GOOS)
				if err != nil {
					t.Fatalf("%s expected posture refusal on %s: %v\nlogs:\n%s", vendor, runtime.GOOS, err, ref.Logs)
				}
				t.Logf("%s on %s: expected posture refusal (not vendor qualification): %s", vendor, runtime.GOOS, ref.Message)
				return
			}
			res, err := workersmoke.Run(ctx, bin, t.TempDir(), env, env, vendor, d.Path, strings.TrimSpace(string(prompt)))
			if err != nil {
				t.Fatalf("%s smoke: %v\nlogs:\n%s", vendor, err, res.Logs)
			}
			r := res.View.Result
			if res.View.State != contract.TaskSucceeded || r == nil || r.ExitCode == nil || *r.ExitCode != 0 || r.FinalMessage == nil || *r.FinalMessage != "pong" {
				t.Fatalf("%s smoke: state %s result %+v\nlogs:\n%s", vendor, res.View.State, r, res.Logs)
			}
			t.Logf("%s on %s: answer-path operability only (no tool work or containment is shown)", vendor, runtime.GOOS)
		})
	}
}
