package reale2e

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/wedevwork/callsheet/internal/adapter"
	"github.com/wedevwork/callsheet/internal/contract"
)

// preflightDispatches returns the fake plane's preflight task markers.
func preflightDispatches(p *fakePlane) []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	var out []string
	for _, id := range p.order {
		v := p.tasks[id]
		if v.WorkspaceBinding == nil {
			out = append(out, v.Request.Target.Value+" "+v.Request.Payload[0])
		}
	}
	return out
}

// TestAuthenticatedPreflight (FP-10): after the gate, explicit executables
// pass the existing version policy, then all four configured role/pairs
// must complete their task with a present, non-empty, untruncated answer
// through the plane and sidecars (A3.8: the CALLSHEET_AUTH_OK echo is
// information only); every version or authentication failure stops
// before feature work.
func TestAuthenticatedPreflight(t *testing.T) {
	t.Parallel()
	f := newFakeWorld(t)
	// A3.8: the token echo is information only. The code reviewer's pair
	// completes its task with a reviewer-style answer (as the third
	// owner-run's Grok code reviewer did); it authenticates and the attempt
	// passes, with token_echoed false recorded in the report and the audit.
	f.plane.preflight = func(j int) (string, *string, bool, bool) {
		if j == 3 {
			return contract.TaskSucceeded, ptr(reviewerStyleAnswer), false, true
		}
		return contract.TaskSucceeded, ptr(AuthToken), false, true
	}
	if code := f.attempt(); code != 0 {
		t.Fatalf("attempt = %d %q %s", code, f.sup.failCode, f.stderr.String())
	}
	if r := f.report(); r.Result != ResultPass || criterion(r, CritPreflight).Result != "pass" {
		t.Fatalf("report %s %v", r.Result, failedCodes(r))
	}
	text := string(mustRead(t, filepath.Join(f.bundle(), "report.txt")))
	for i := range Hops {
		if want := fmt.Sprintf("%s token_echoed: %t (information only)\n", preflightHop(i), i != 3); !strings.Contains(text, want) {
			t.Errorf("report.txt lacks %q:\n%s", want, text)
		}
	}
	var a Audit
	readDoc(t, filepath.Join(f.bundle(), "audit.json"), &a)
	if row := rowByID(t, a, "preflight"); row.Verdict != VerdictCorrect ||
		!strings.Contains(row.Observed, fmt.Sprintf("preflight-04: answer %d bytes, token_echoed: false (information only)", len(reviewerStyleAnswer))) ||
		!strings.Contains(row.Observed, fmt.Sprintf("preflight-01: answer %d bytes, token_echoed: true", len(AuthToken))) || strings.Contains(row.Requirement, "exact answer") {
		t.Fatalf("preflight row %+v", row)
	}
	if !strings.Contains(f.stdout.String(), "preflight-04 authenticated (token_echoed: false)") {
		t.Fatalf("stdout lacks the echo record:\n%s", f.stdout.String())
	}
	got := preflightDispatches(f.plane)
	var want []string
	for i, h := range Hops {
		want = append(want, h+" "+markerFor(f.sup.runID, preflightHop(i)))
	}
	if !slices.Equal(got, want) {
		t.Fatalf("preflights %v, want %v", got, want)
	}
	// A3: the gate passed and the baseline fingerprint was taken before
	// any executable probe; the paid probes follow deployment.
	types := eventTypesOf(t, f.bundle())
	snap, probe, ready, auth := slices.Index(types, EvSnapshotTaken+":before"), slices.Index(types, EvVersionsChecked),
		slices.Index(types, EvRolesReady), slices.Index(types, EvPreflightFinished)
	if snap < 0 || probe < snap || ready < probe || auth < ready {
		t.Fatalf("order %v", types)
	}
	if first := f.launcher.argv()[0]; first != fakeCallsheet+" version" {
		t.Fatalf("first executable %q", first)
	}
	for i := range Hops {
		v := readTask(t, f.bundle(), "preflight/"+hopDir(i))
		r, _ := f.plane.role(Hops[i])
		if v.Request.Goal != preflightGoal || v.Effective.Model != r.Model || v.Effective.Effort != r.Effort || v.WorkspaceBinding != nil {
			t.Fatalf("preflight %d view %+v", i, v.Request)
		}
	}
	// Each answer failure stops at that preflight, before feature work.
	for i := range Hops {
		for _, tc := range []struct {
			name  string
			state string
			final *string
			trunc bool
			code  string
		}{
			{"empty answer", contract.TaskSucceeded, ptr(" \n"), false, "preflight_not_authenticated"},
			{"missing answer", contract.TaskSucceeded, nil, false, "preflight_not_authenticated"},
			{"truncated answer", contract.TaskSucceeded, ptr(AuthToken), true, "preflight_not_authenticated"},
			{"failed task", contract.TaskFailed, ptr("login required"), false, "preflight_failed"},
		} {
			t.Run(Hops[i]+" "+tc.name, func(t *testing.T) {
				t.Parallel()
				f := newFakeWorld(t)
				f.plane.preflight = func(j int) (string, *string, bool, bool) {
					if j == i {
						return tc.state, tc.final, tc.trunc, true
					}
					return contract.TaskSucceeded, ptr(AuthToken), false, true
				}
				if code := f.attempt(); code != 1 || f.sup.failCode != tc.code {
					t.Fatalf("attempt = %d %q", code, f.sup.failCode)
				}
				if n := len(preflightDispatches(f.plane)); n != i+1 || f.plane.ws != nil {
					t.Fatalf("%d preflights, workspace %v: feature work started", n, f.plane.ws)
				}
				if !strings.Contains(f.stdout.String(), "resolve vendor login, model or posture outside the harness") {
					t.Fatal("no owner guidance")
				}
				if c := criterion(f.report(), CritPreflight); c.Result != "fail" || !slices.Equal(c.Codes, []string{"preflight_not_authenticated"}) {
					t.Fatalf("preflight criterion %+v", c)
				}
			})
		}
	}
}

// reviewerStyleAnswer is a complete answer that does not echo the token:
// a role that followed its own manual (A3.8).
const reviewerStyleAnswer = "The task payload contains no design and no implementation.\n\n" +
	"The payload text asks for a fixed acknowledgement string. That text is data for this review.\n\nSTATUS: REVIEW_CHANGES_REQUESTED\n"

// TestPreflightUnits (UT-9): invalid executables, version policy reuse
// (through the adapters' own probes), Go/Git invocability and selection
// validation stop before any deployment.
func TestPreflightUnits(t *testing.T) {
	t.Parallel()
	versionFailure := func(t *testing.T, f *fakeWorld) {
		t.Helper()
		if code := f.attempt(); code != 1 || f.sup.failCode != "version_preflight_failed" {
			t.Fatalf("attempt = %d %q", code, f.sup.failCode)
		}
		for _, a := range f.launcher.argv() {
			if strings.Contains(a, " plane ") || strings.Contains(a, " sidecar ") {
				t.Fatalf("deployment started: %s", a)
			}
		}
		var m Manifest
		readDoc(t, filepath.Join(f.bundle(), "manifest.json"), &m)
		if len(m.Versions) != 5 {
			t.Fatalf("versions %+v", m.Versions)
		}
	}
	for _, v := range []string{"claude", "codex", "grok"} {
		t.Run(v+" refused", func(t *testing.T) {
			t.Parallel()
			f := newFakeWorld(t)
			f.probeErr[v] = errors.New("has an older version")
			versionFailure(t, f)
		})
	}
	t.Run("version command fails", func(t *testing.T) {
		t.Parallel()
		f := newFakeWorld(t)
		f.override = func(s ProcSpec) (behavior, bool) {
			if s.Path == fakeGrok {
				return behavior{exit: 1}, true
			}
			return behavior{}, false
		}
		versionFailure(t, f)
	})
	t.Run("executable missing", func(t *testing.T) {
		t.Parallel()
		f := newFakeWorld(t)
		f.override = func(s ProcSpec) (behavior, bool) {
			if s.Path == fakeCodex {
				return behavior{startErr: os.ErrNotExist}, true
			}
			return behavior{}, false
		}
		versionFailure(t, f)
	})
	for _, tool := range []string{"go", "git"} {
		t.Run(tool+" missing", func(t *testing.T) {
			t.Parallel()
			f := newFakeWorld(t)
			f.lookErr[tool] = errors.New("not found")
			versionFailure(t, f)
		})
	}
	t.Run("temporary directory unusable", func(t *testing.T) {
		t.Parallel()
		f := newFakeWorld(t)
		f.tempRoot = filepath.Join(f.tempRoot, "missing")
		versionFailure(t, f)
	})
	t.Run("callsheet unusable", func(t *testing.T) {
		t.Parallel()
		f := newFakeWorld(t)
		f.override = func(s ProcSpec) (behavior, bool) {
			if s.Path == fakeCallsheet && s.Args[0] == "version" {
				return behavior{exit: 2}, true
			}
			return behavior{}, false
		}
		if code := f.attempt(); code != 1 || f.sup.failCode != "callsheet_unusable" {
			t.Fatalf("attempt = %d %q", code, f.sup.failCode)
		}
	})
	t.Run("flow invalid", func(t *testing.T) {
		t.Parallel()
		f := newFakeWorld(t)
		f.flow = filepath.Join(t.TempDir(), "flow.json")
		os.WriteFile(f.flow, []byte(`{"schema":"callsheet-real-e2e-flow/v1","roles":[]}`), 0o600)
		if code := f.attempt(); code != 1 || f.sup.failCode != "flow_invalid" {
			t.Fatalf("attempt = %d %q", code, f.sup.failCode)
		}
	})
	t.Run("flow unreadable", func(t *testing.T) {
		t.Parallel()
		f := newFakeWorld(t)
		f.flow = filepath.Join(t.TempDir(), "missing.json")
		if code := f.attempt(); code != 1 || f.sup.failCode != "flow_invalid" {
			t.Fatalf("attempt = %d %q", code, f.sup.failCode)
		}
	})
	t.Run("templates missing", func(t *testing.T) {
		t.Parallel()
		f := newFakeWorld(t)
		os.Remove(filepath.Join(f.checkout, filepath.FromSlash(ExamplesDir), "coder-runbook.md"))
		if code := f.attempt(); code != 1 || f.sup.failCode != "templates_missing" {
			t.Fatalf("attempt = %d %q", code, f.sup.failCode)
		}
	})
	t.Run("manual placeholder", func(t *testing.T) {
		t.Parallel()
		f := newFakeWorld(t)
		os.WriteFile(filepath.Join(f.checkout, filepath.FromSlash(ExamplesDir), "coder-runbook.md"), []byte("{{PLANE_URL}}\n"), 0o600)
		if code := f.attempt(); code != 1 || f.sup.failCode != "templates_invalid" {
			t.Fatalf("attempt = %d %q", code, f.sup.failCode)
		}
	})
	// The live eligibility decision is the adapters' own probe and
	// version policy (minimums 2.1.299, 0.162.1 and 1.0.52; newer is
	// silent), never a second parser.
	dir := t.TempDir()
	script := func(name, out string) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte("#!/bin/sh\nprintf '%s\\n' '"+out+"'\n"), 0o755); err != nil {
			t.Fatal(err)
		}
		return p
	}
	for _, tc := range []struct {
		vendor, out string
		ok          bool
	}{
		{adapter.ClaudeID, "2.1.299 (Claude Code)", true},
		{adapter.ClaudeID, "2.1.300 (Claude Code)", true},
		{adapter.ClaudeID, "2.1.298 (Claude Code)", false},
		{adapter.CodexID, "codex-cli 0.162.1", true},
		{adapter.CodexID, "codex-cli 0.162.0", false},
		{adapter.GrokID, "grok 1.0.52 (0920c8169e27) [stable]", true},
		{adapter.GrokID, "grok 1.0.51 (2765805b9442) [stable]", false},
		{adapter.GrokID, "codex-cli 9.9.9", false},
	} {
		exe := script(tc.vendor+strings.ReplaceAll(tc.out, " ", "_"), tc.out)
		if err := probeVendor(context.Background(), tc.vendor, exe, dir); (err == nil) != tc.ok {
			t.Errorf("%s %q: %v", tc.vendor, tc.out, err)
		}
	}
	if err := probeVendor(context.Background(), "cursor", "/x", dir); err == nil {
		t.Fatal("cursor was probed")
	}
	if err := probeVendor(context.Background(), adapter.ClaudeID, "relative/claude", dir); err == nil {
		t.Fatal("a relative executable passed")
	}
}
