package function

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/wedevwork/callsheet/internal/mcpqual"
	"github.com/wedevwork/callsheet/internal/mcpqual/procexec"
	"github.com/wedevwork/callsheet/internal/testkit"
	"github.com/wedevwork/callsheet/internal/testkit/catalog"
)

// Decoder enrollment, slice B1.5 (design decoder-enrollment B1.5): one
// function parent per new FP (FP-14..FP-16), each with its literal local
// case inventory and no subtest names. They run the production capture and
// qualification runners in process (GOOS/GOARCH linux/amd64 injected
// through the runner's existing fields, on every platform) with the fake
// vendor and the real probe: the fake vendor reaps the probe, then rewrites
// its own server-events fixture to a bounded prefix before it exits, so the
// harness reads exactly those bytes after every writer stopped. Watchdog
// variants hide the session's own exit through the existing Launcher seam
// (the watchdog is then the only branch), never a sleep race; no probe is
// signaled to simulate a missing exit. No installed vendor CLI, login or
// network is used, and every launched group is proven gone.

// b15Capture is one in-process capture of the claude recipe with fake
// settings (FAKE_VENDOR_*), adjusted by mutate; it returns the client, the
// bundle validated from disk, its directory and the capture log.
func b15Capture(t *testing.T, settings map[string]string, mutate func(p map[string]any, c *mcpqual.CaptureRunner)) (mcpqual.CaptureClient, *mcpqual.CaptureBundle, string, string) {
	t.Helper()
	q := newQualEnv(t)
	plan := q.capturePlan(map[string]map[string]string{"claude": settings}, "claude")
	out := filepath.Join(realTemp(t), "out")
	log := &safeBuffer{}
	if mutate != nil {
		mutate(plan, nil)
	}
	inProcessRun(t, q, plan, out, func(c *mcpqual.CaptureRunner) {
		c.Log = log
		if mutate != nil {
			mutate(nil, c)
		}
	})
	b := bundle(t, out)
	return captureClient(t, b.Manifest, "claude"), b, out, log.String()
}

// hiddenExit hides a session's own exit until it is reaped, so the
// launch's watchdog is the only branch it can take; the reaper releases it
// once the real process has exited by itself.
type hiddenExit struct {
	mcpqual.StderrProc
	released chan struct{}
}

func (h *hiddenExit) Exited() <-chan struct{} { return h.released }

// hidingLauncher wraps the real launcher, hiding every session's exit.
type hidingLauncher struct{ inner mcpqual.Launcher }

func (l hidingLauncher) Start(spec mcpqual.ProcSpec) (mcpqual.Proc, error) {
	p, err := l.inner.Start(spec)
	session := len(spec.Args) > 0 && !slices.Equal(spec.Args, []string{"--version"}) && spec.Args[len(spec.Args)-1] != "--help" && !slices.Equal(spec.Args, mcpqual.CursorApprovalArgv())
	if sp, ok := p.(mcpqual.StderrProc); err == nil && ok && session {
		return &hiddenExit{StderrProc: sp, released: make(chan struct{})}, nil
	}
	return p, err
}

// releasingReaper waits (bounded) for a hidden session's real exit, then
// releases it and reaps with the real reaper.
type releasingReaper struct {
	t     *testing.T
	inner mcpqual.Reaper
}

func (r releasingReaper) Reap(p mcpqual.Proc) mcpqual.CaseCleanup {
	if h, ok := p.(*hiddenExit); ok {
		select {
		case <-h.StderrProc.Exited():
		case <-time.After(30 * time.Second):
			r.t.Error("the hidden session never exited by itself")
		}
		close(h.released)
	}
	return r.inner.Reap(p)
}

// b15Qualify is one in-process qualification of the fake claude vendor
// (settings FAKE_VENDOR_*) under the test-only qualified registry with the
// given capability kinds, setup plus one 150 ms default call and its
// repeat, and its evidence directory.
func b15Qualify(t *testing.T, settings map[string]string, kinds []string) (*mcpqual.Report, string) {
	t.Helper()
	q := newQualEnv(t)
	version := captureVersions["claude"]
	reg := mcpqual.DefaultRegistry().WithVersion("claude-json", mcpqual.DecoderVersion{Version: version, Fixture: "claude-json/actual-function-test", Qualified: true,
		Evidence: []mcpqual.DecoderEvidence{{Platform: "linux/amd64", Fixture: "claude-json/actual-function-test", Kinds: kinds}}})
	s := map[string]string{"VERSION": version, "TIMEOUT_MS": "0"}
	for k, v := range settings {
		s[k] = v
	}
	c := q.fakeClient("claude", s, map[string]any{"default": map[string]any{"delays_ms": []int{150}}})
	c["decoder_fixture"] = "claude-json/actual-function-test"
	delete(c["config"].(map[string]any), "raised")
	delete(c, "override")
	plan := map[string]any{"version": 1, "clients": []any{c}, "limits": map[string]any{"max_sessions_per_client": 3, "max_case_ms": 1200, "max_client_ms": 3600}}
	b, _ := json.Marshal(plan)
	p, err := mcpqual.ParsePlan(b, reg)
	if err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(realTemp(t), "out")
	r := &mcpqual.Runner{Plan: p, PlanSHA256: sha(b), OutDir: out, GOOS: "linux", GOARCH: "amd64", Hostname: "function-host", ServerPath: qualBinary(t),
		BaseEnv: q.env(""), Launcher: procexec.Launcher{}, Reaper: mcpqual.GroupReaper{Sig: mcpqual.SysSignaler{}, Clock: mcpqual.RealClock,
			Policy: mcpqual.CleanupPolicy{Grace: mcpqual.CleanupGrace, Limit: mcpqual.CleanupLimit, Poll: mcpqual.CleanupPoll}},
		Clock: mcpqual.RealClock, Registry: reg, Log: io.Discard, RunID: "run-terminal", Nonce: "nonceterminal", CaptureDate: "2026-10-08",
		HarnessVersion: mcpqual.HarnessVersion, Home: q.home}
	rep, err := r.Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	q.groupsGone()
	return rep, out
}

// reportCase is the named case of the claude client's report.
func reportCase(t *testing.T, rep *mcpqual.Report, id string) mcpqual.CaseReport {
	t.Helper()
	for _, ph := range reportClient(t, rep, "claude").Phases {
		for _, cs := range ph.Cases {
			if cs.CaseID == id {
				return cs
			}
		}
	}
	t.Fatalf("no case %s", id)
	return mcpqual.CaseReport{}
}

// observed checks an observation's four members.
func observed(t *testing.T, where string, o *mcpqual.ProbeObservation, end string, intact bool, kind string, clean bool) {
	t.Helper()
	k := ""
	if o != nil && o.TerminalKind != nil {
		k = *o.TerminalKind
	}
	if o == nil || o.EndState != end || o.Intact != intact || k != kind || o.CleanSession != clean {
		t.Fatalf("%s: observation %+v (kind %q), want %s intact=%v kind=%q clean=%v", where, o, k, end, intact, kind, clean)
	}
}

// editManifest rewrites a copy's manifest.json through a decoded edit.
func editManifest(t *testing.T, dir string, edit func(m map[string]any)) {
	t.Helper()
	p := filepath.Join(dir, mcpqual.CaptureManifestName)
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	edit(m)
	out, _ := json.MarshalIndent(m, "", "  ")
	if err := os.WriteFile(p, out, 0o600); err != nil {
		t.Fatal(err)
	}
}

// client0 is a decoded manifest's first client.
func client0(m map[string]any) map[string]any { return m["clients"].([]any)[0].(map[string]any) }

// tampered requires the production bundle validator to refuse each edit
// of a copy of out.
func tampered(t *testing.T, out string, edits map[string]func(m map[string]any)) {
	t.Helper()
	for name, edit := range edits {
		d := filepath.Join(realTemp(t), "copy")
		copyTree(t, out, d)
		editManifest(t, d, edit)
		if _, err := mcpqual.ValidateCaptureBundle(os.DirFS(d), "."); err == nil {
			t.Fatalf("%s: a tampered manifest passed", name)
		}
	}
}

// FP-14: one shared analyzer gives capture and qualify the same terminal
// observation: an intact log, the clean terminal observation without exit
// (complete capture and nonce success, intact=false, labeled as such), a
// cancelled or eof endpoint (never a complete capture or an implied
// timeout, but the endpoint of an independently typed fake timeout), every
// incomplete variant (typed-timeout-cancelled-exitless-nonzero among them)
// and forged evidence, while the repeated lower bound stays eligible.
func TestMCPProbeTerminalObservation(t *testing.T) {
	t.Parallel()
	success := []string{mcpqual.CapToolCall, mcpqual.CapToolResult, mcpqual.CapTerminalSuccess}
	timeouts := append(append([]string(nil), success...), mcpqual.CapMCPTimeout)
	noTimeout := func(t *testing.T, c mcpqual.CaptureClient) {
		t.Helper()
		b, _ := json.Marshal(c)
		if strings.Contains(string(b), mcpqual.KindMCPTimeout) {
			t.Fatalf("a timeout was inferred: %s", b)
		}
	}
	serverOf := func(b *mcpqual.CaptureBundle) string {
		return string(b.Files[mcpqual.ClientFile("claude", mcpqual.FileServerEvents)])
	}
	// hasExit parses the retained events (never a byte search): whether any
	// record is an exit record.
	hasExit := func(t *testing.T, server string) bool {
		t.Helper()
		evs, _, err := mcpqual.ParseProbeEvents([]byte(server))
		if err != nil {
			t.Fatalf("server events: %v", err)
		}
		return slices.ContainsFunc(evs, func(ev mcpqual.ProbeEvent) bool { return ev.Kind == mcpqual.EvExit })
	}
	var withoutExit string // the completed-without-exit capture, for forged-evidence
	runInventory(t, 5, []string{"intact", "completed-without-exit", "other-terminal", "incomplete", "forged-evidence"}, []captureCase{
		{"intact", func(t *testing.T) {
			c, b, _, log := b15Capture(t, nil, nil)
			if c.State != mcpqual.CaptureComplete || !c.Probe.Intact || !strings.Contains(log, "probe observation: intact (probe exit observed)") ||
				!hasExit(t, serverOf(b)) {
				t.Fatalf("intact capture %s %+v: %s", deref(c.Reason), c.Probe, log)
			}
			observed(t, "capture", c.Probe.Observation, mcpqual.EndIntact, true, mcpqual.EvCompleted, true)
			// Qualify runs the same analyzer: every analyzed case records it.
			rep, _ := b15Qualify(t, nil, success)
			for _, id := range []string{"claude-setup", "claude-default-1", "claude-default-repeat"} {
				cs := reportCase(t, rep, id)
				observed(t, id, cs.ProbeObservation, mcpqual.EndIntact, true, mcpqual.EvCompleted, true)
			}
		}},
		{"completed-without-exit", func(t *testing.T) {
			c, b, out, log := b15Capture(t, map[string]string{"CUT": "terminal"}, nil)
			withoutExit = out
			server := serverOf(b)
			if c.State != mcpqual.CaptureComplete || c.Probe.Intact || !c.Probe.Completed || c.Probe.Receipts != 1 || hasExit(t, server) ||
				!strings.HasSuffix(server, "}\n") || !strings.Contains(log, "probe observation: "+mcpqual.TerminalObservedLabel) {
				t.Fatalf("exit-less capture %s %+v: %s", deref(c.Reason), c.Probe, log)
			}
			observed(t, "capture", c.Probe.Observation, mcpqual.EndTerminalWithoutExit, false, mcpqual.EvCompleted, true)
			for _, never := range []string{"terminated by client", "client killed", "killed server"} {
				if strings.Contains(string(b.ManifestBytes), never) || strings.Contains(log, never) {
					t.Fatalf("a termination cause %q was asserted", never)
				}
			}
			// Qualify: nonce success with the new label, and the repeated
			// lower bound of the existing synthetic qualified fixture stays
			// eligible and publishable.
			rep, qout := b15Qualify(t, map[string]string{"CUT": "terminal"}, success)
			def := reportPhase(t, reportClient(t, rep, "claude"), mcpqual.PhaseDefault)
			if rep.Outcome != mcpqual.StatusConclusive || ms(def.LowerBoundMS) != 150 || def.Observations != 2 || len(def.Cases) != 2 {
				t.Fatalf("exit-less lower bound %+v", def)
			}
			for _, id := range []string{"claude-setup", "claude-default-1", "claude-default-repeat"} {
				cs := reportCase(t, rep, id)
				if cs.Outcome != mcpqual.KindToolResult {
					t.Fatalf("%s: %s %s", id, cs.Outcome, deref(cs.Reason))
				}
				observed(t, id, cs.ProbeObservation, mcpqual.EndTerminalWithoutExit, false, mcpqual.EvCompleted, true)
			}
			md, _ := os.ReadFile(filepath.Join(qout, "report.md"))
			if !strings.Contains(string(md), "probe: "+mcpqual.TerminalObservedLabel) {
				t.Fatalf("report.md lacks the label:\n%s", md)
			}
			repo, base := shortRepo(t, captureVersions["claude"], "linux/amd64")
			if _, err := mcpqual.ProposePatch(rep, base, qout); err != nil {
				t.Fatal(err)
			}
			if err := mcpqual.Publish(qout, repo); err != nil {
				t.Fatal(err)
			}
			if f := repoFacts(t, repo)["claude"].Facts["mcp_timeout"]; f.Status != catalog.Verified || !strings.Contains(f.Value, "this is a lower bound, not the default") {
				t.Fatalf("published %+v", f)
			}
		}},
		{"other-terminal", func(t *testing.T) {
			// A cancelled or eof endpoint is observed, but never completes a
			// setup capture nor implies a timeout.
			for _, kind := range []string{mcpqual.EvCancelled, mcpqual.EvEOF} {
				c, _, _, _ := b15Capture(t, map[string]string{"CUT": kind}, nil)
				if c.State == mcpqual.CaptureComplete || deref(c.Reason) != mcpqual.ReasonProbeIncomplete || c.Probe.Completed {
					t.Fatalf("%s endpoint: %s %s %+v", kind, c.State, deref(c.Reason), c.Probe)
				}
				observed(t, kind, c.Probe.Observation, mcpqual.EndTerminalWithoutExit, false, kind, true)
				noTimeout(t, c)
			}
			// An independently typed fake timeout correlates with the
			// exit-less cancelled endpoint of a clean session.
			rep, _ := b15Qualify(t, map[string]string{"TIMEOUT_MS": "50", "CUT": "terminal", "CUT_ON": "timeout"}, timeouts)
			cs := reportCase(t, rep, "claude-default-1")
			def := reportPhase(t, reportClient(t, rep, "claude"), mcpqual.PhaseDefault)
			if cs.Outcome != mcpqual.KindMCPTimeout || cs.ElapsedMS == nil || def.Status != mcpqual.StatusConclusive || deref(def.Result) != mcpqual.ResultTimeoutObserved {
				t.Fatalf("typed timeout at a cancelled endpoint: %s %s %+v", cs.Outcome, deref(cs.Reason), def)
			}
			observed(t, "timeout", cs.ProbeObservation, mcpqual.EndTerminalWithoutExit, false, mcpqual.EvCancelled, true)
			// A cancelled endpoint under a decoder success is no completion
			// and never a timeout.
			rep, _ = b15Qualify(t, map[string]string{"CUT": "cancelled", "CUT_ON": "success"}, success)
			setup := reportCase(t, rep, "claude-setup")
			if setup.Outcome != mcpqual.OutcomeInconclusive || deref(setup.Reason) != mcpqual.ReasonProbeEndpoint {
				t.Fatalf("cancelled under success: %s %s", setup.Outcome, deref(setup.Reason))
			}
		}},
		{"incomplete", func(t *testing.T) {
			small := func(p map[string]any, c *mcpqual.CaptureRunner) {
				if p != nil {
					p["limits"] = map[string]any{"max_sessions_per_client": 1, "max_case_ms": 2000, "max_client_ms": 60000}
				}
				if c != nil {
					c.Launcher, c.Reaper = hidingLauncher{c.Launcher}, releasingReaper{t, c.Reaper}
				}
			}
			bounded := func(_ map[string]any, c *mcpqual.CaptureRunner) {
				if c != nil {
					c.Limits = mcpqual.CaptureLimits{ServerEvents: 128}
				}
			}
			for name, tc := range map[string]struct {
				settings map[string]string
				mutate   func(p map[string]any, c *mcpqual.CaptureRunner)
				reason   string
			}{
				"no-lf":              {map[string]string{"CUT": "nolf"}, nil, mcpqual.ReasonProbeIncomplete},
				"torn":               {map[string]string{"CUT": "torn"}, nil, mcpqual.ReasonProbeAnomaly + ": events_invalid"},
				"blank-record":       {map[string]string{"CUT": "blank"}, nil, mcpqual.ReasonProbeIncomplete},
				"before-terminal":    {map[string]string{"CUT": "before"}, nil, mcpqual.ReasonProbeIncomplete},
				"duplicate-receipt":  {map[string]string{"CUT": "terminal", "SCENARIO": "extra"}, nil, mcpqual.ReasonProbeAnomaly + ": extra_receipt"},
				"duplicate-terminal": {map[string]string{"CUT": "dup"}, nil, mcpqual.ReasonProbeAnomaly + ": duplicate_completion"},
				"wrong-id":           {map[string]string{"CUT": "wrongid"}, nil, mcpqual.ReasonProbeAnomaly + ": completion_mismatch"},
				"wrong-id-intact":    {map[string]string{"CUT": "wrongid-intact"}, nil, mcpqual.ReasonProbeAnomaly + ": completion_mismatch"},
				"different-instance": {map[string]string{"CUT": "otherrun"}, nil, mcpqual.ReasonProbeAnomaly + ": instance_run_mismatch"},
				"exit-nonzero":       {map[string]string{"CUT": "terminal", "EXIT": "1"}, nil, mcpqual.ReasonProbeIncomplete},
				"watchdog":           {map[string]string{"CUT": "terminal"}, small, mcpqual.ReasonSessionWatchdog},
				"cut":                {map[string]string{"CUT": "terminal"}, bounded, mcpqual.ReasonEvidenceTruncated + ": the probe events"},
			} {
				c, _, _, _ := b15Capture(t, tc.settings, tc.mutate)
				o := c.Probe.Observation
				if c.State == mcpqual.CaptureComplete || !strings.HasPrefix(deref(c.Reason), tc.reason) || o == nil || o.EndState != mcpqual.EndIncomplete {
					t.Fatalf("%s: %s %q %+v", name, c.State, deref(c.Reason), o)
				}
				if (name == "exit-nonzero" || name == "watchdog" || name == "cut") == o.CleanSession {
					t.Fatalf("%s: clean_session %v", name, o.CleanSession)
				}
				if name == "watchdog" && !c.Session.Watchdog {
					t.Fatalf("watchdog: %+v", c.Session)
				}
			}
			// Qualify applies the same accepted end state: an intact log with
			// a mismatched typed request ID, or a call under another run, is no
			// nonce success.
			for _, cut := range []string{"wrongid-intact", "otherrun"} {
				rep, _ := b15Qualify(t, map[string]string{"CUT": cut, "CUT_ON": "success"}, success)
				setup := reportCase(t, rep, "claude-setup")
				if setup.Outcome != mcpqual.OutcomeInconclusive || deref(setup.Reason) != mcpqual.ReasonProbeIncomplete || setup.ProbeObservation.EndState != mcpqual.EndIncomplete {
					t.Fatalf("%s qualify: %s %s %+v", cut, setup.Outcome, deref(setup.Reason), setup.ProbeObservation)
				}
			}
			// typed-timeout-cancelled-exitless-nonzero: a CLI that times the
			// call out, leaves no probe exit and exits nonzero keeps its
			// evidence as inconclusive probe_incomplete, and the decision is
			// UNVERIFIED, never incompatible and never a pass.
			const named = "typed-timeout-cancelled-exitless-nonzero"
			rep, out := b15Qualify(t, map[string]string{"TIMEOUT_MS": "50", "CUT": "terminal", "CUT_ON": "timeout", "EXIT": "1"}, timeouts)
			cs := reportCase(t, rep, "claude-default-1")
			cl := reportClient(t, rep, "claude")
			def := reportPhase(t, cl, mcpqual.PhaseDefault)
			decision := mcpqual.ShortPollDecision(mcpqual.ShortPollBudget, rep, cl)
			if cs.Outcome != mcpqual.OutcomeInconclusive || deref(cs.Reason) != mcpqual.ReasonProbeIncomplete || def.Status != mcpqual.StatusInconclusive ||
				rep.Outcome == mcpqual.StatusConclusive || !strings.Contains(decision, "compatibility UNVERIFIED") || strings.Contains(decision, "not compatible") {
				t.Fatalf("%s: %s %s %+v %s", named, cs.Outcome, deref(cs.Reason), def, decision)
			}
			observed(t, named, cs.ProbeObservation, mcpqual.EndIncomplete, false, mcpqual.EvCancelled, false)
			if cs.ServerEvents == nil {
				t.Fatalf("%s: the probe evidence was not retained", named)
			}
			if b, err := os.ReadFile(filepath.Join(out, filepath.FromSlash(*cs.ServerEvents))); err != nil || !bytes.Contains(b, []byte(`"kind":"cancelled"`)) {
				t.Fatalf("%s: retained evidence %v", named, err)
			}
			// Its intact-log counterpart: the clean-session gate holds for an
			// intact log too.
			rep, _ = b15Qualify(t, map[string]string{"TIMEOUT_MS": "50", "EXIT": "1"}, timeouts)
			cs = reportCase(t, rep, "claude-default-1")
			if cs.Outcome != mcpqual.OutcomeInconclusive || deref(cs.Reason) != mcpqual.ReasonProbeIncomplete {
				t.Fatalf("intact nonzero timeout: %s %s", cs.Outcome, deref(cs.Reason))
			}
			observed(t, "intact nonzero timeout", cs.ProbeObservation, mcpqual.EndIntact, true, mcpqual.EvCancelled, false)
			// The clean intact timeout stays conclusive.
			rep, _ = b15Qualify(t, map[string]string{"TIMEOUT_MS": "50"}, timeouts)
			if cs := reportCase(t, rep, "claude-default-1"); cs.Outcome != mcpqual.KindMCPTimeout || cs.ElapsedMS == nil {
				t.Fatalf("clean timeout: %s %s", cs.Outcome, deref(cs.Reason))
			}
		}},
		{"forged-evidence", func(t *testing.T) {
			if withoutExit == "" {
				t.Fatal("no exit-less capture")
			}
			// A supplied label is never trusted: every mismatch with the
			// replayed server events, the recorded session or the schema fails.
			obs := func(m map[string]any) map[string]any {
				return client0(m)["probe"].(map[string]any)["observation"].(map[string]any)
			}
			tampered(t, withoutExit, map[string]func(m map[string]any){
				"end-intact":       func(m map[string]any) { obs(m)["end_state"] = mcpqual.EndIntact },
				"intact-true":      func(m map[string]any) { obs(m)["intact"] = true; client0(m)["probe"].(map[string]any)["intact"] = true },
				"not-clean":        func(m map[string]any) { obs(m)["clean_session"] = false },
				"cancelled":        func(m map[string]any) { obs(m)["terminal_kind"] = mcpqual.EvCancelled },
				"null-kind":        func(m map[string]any) { obs(m)["terminal_kind"] = nil },
				"unknown-end":      func(m map[string]any) { obs(m)["end_state"] = "server_terminated_by_client" },
				"extra-member":     func(m map[string]any) { obs(m)["cause"] = "killed" },
				"missing-member":   func(m map[string]any) { delete(obs(m), "clean_session") },
				"null-observation": func(m map[string]any) { client0(m)["probe"].(map[string]any)["observation"] = nil },
				// Without the new evidence a legacy record cannot claim the
				// allowance.
				"legacy-claim": func(m map[string]any) { delete(client0(m)["probe"].(map[string]any), "observation") },
				"exit-nonzero": func(m map[string]any) {
					client0(m)["session"].(map[string]any)["exit"] = 1
				},
			})
			// The retained bytes themselves: an added exit record (its hash
			// made consistent) no longer replays to the recorded observation.
			d := filepath.Join(realTemp(t), "copy")
			copyTree(t, withoutExit, d)
			rel := mcpqual.ClientFile("claude", mcpqual.FileServerEvents)
			appendFile(t, filepath.Join(d, filepath.FromSlash(rel)), `{"seq":6,"kind":"exit","offset_ns":1,"run_id":"run-flood","reason":"eof"}`+"\n")
			data, _ := os.ReadFile(filepath.Join(d, filepath.FromSlash(rel)))
			editManifest(t, d, func(m map[string]any) {
				for _, f := range m["files"].([]any) {
					if f := f.(map[string]any); f["path"] == rel {
						f["sha256"], f["bytes"] = sha(data), len(data)
					}
				}
			})
			if _, err := mcpqual.ValidateCaptureBundle(os.DirFS(d), "."); err == nil || !strings.Contains(err.Error(), "probe replay") {
				t.Fatalf("a replay mismatch passed: %v", err)
			}
			// Enrollment accepts the clean terminal observation only through
			// the full replay; a forged observation is refused.
			repo, e, o, reg := enrollCapture(t, withoutExit)
			if _, err := mcpqual.ValidateEnrollment(mcpqual.EnrollmentOptions{FS: os.DirFS(repo), Registry: reg}); err != nil {
				t.Fatalf("enrollment of the terminal observation: %v", err)
			}
			bdir := filepath.Join(repo, filepath.FromSlash(e.Bundle))
			editManifest(t, bdir, func(m map[string]any) {
				client0(m)["probe"].(map[string]any)["observation"].(map[string]any)["end_state"] = mcpqual.EndIntact
			})
			mb, _ := os.ReadFile(filepath.Join(bdir, mcpqual.CaptureManifestName))
			e.ManifestSHA256 = sha(mb)
			writeEnrollment(t, repo, &e, o)
			if _, err := mcpqual.ValidateEnrollment(mcpqual.EnrollmentOptions{FS: os.DirFS(repo), Registry: reg}); err == nil {
				t.Fatal("enrollment accepted a forged observation")
			}
			// The retained 2026-10-08 bundles stay partial: their legacy
			// records hold no observation, so nothing promotes them, and a
			// legacy record forged complete is refused.
			for _, id := range []string{"codex", "grok", "claude-2"} {
				dir := filepath.Join(testkit.MustRepoRoot(t), "design", "iterations", "decoder-enrollment", "captures-2026-10-08", id)
				b, err := mcpqual.ValidateCaptureBundle(os.DirFS(dir), ".")
				if err != nil {
					t.Fatalf("%s: %v", id, err)
				}
				c := b.Manifest.Clients[0]
				if b.Manifest.State != mcpqual.CapturePartial || deref(c.Reason) != mcpqual.ReasonProbeIncomplete || c.Probe.Observation != nil {
					t.Fatalf("%s: %s %s %+v", id, b.Manifest.State, deref(c.Reason), c.Probe)
				}
				tampered(t, dir, map[string]func(m map[string]any){"forged-complete": func(m map[string]any) {
					m["state"], m["reason"] = mcpqual.CaptureComplete, nil
					client0(m)["state"], client0(m)["reason"] = mcpqual.CaptureComplete, nil
				}})
			}
			// Qualify evidence: a report observation contradicting its own
			// record fails the schema; one consistent with it but not with the
			// retained server events fails the evidence replay, so neither
			// can be published.
			_, qout := b15Qualify(t, map[string]string{"CUT": "terminal"}, success)
			rb, err := os.ReadFile(filepath.Join(qout, "report.json"))
			if err != nil {
				t.Fatal(err)
			}
			parsed, err := mcpqual.ParseReport(rb)
			if err != nil || parsed.CheckEvidence(qout) != nil {
				t.Fatalf("the exit-less report: %v", err)
			}
			for name, edit := range map[string][2]string{
				"intact":    {`"end_state": "terminal_observed_without_exit"`, `"end_state": "intact"`},
				"unclean":   {`"clean_session": true`, `"clean_session": false`},
				"cancelled": {`"terminal_kind": "completed"`, `"terminal_kind": "cancelled"`},
			} {
				forged := bytes.Replace(rb, []byte(edit[0]), []byte(edit[1]), 1)
				if bytes.Equal(forged, rb) {
					t.Fatalf("%s: no edit", name)
				}
				p, err := mcpqual.ParseReport(forged)
				if err == nil {
					d := filepath.Join(realTemp(t), "forged")
					copyTree(t, qout, d)
					err = p.CheckEvidence(d)
				}
				if err == nil {
					t.Fatalf("%s: a forged report observation passed", name)
				}
			}
		}},
	})
}

// enrollCapture copies the one-client claude capture in out into a new
// scratch repository as an enrolled fake fixture with its reviewed oracle,
// written from the inspected bytes, and the test-only qualified registry.
func enrollCapture(t *testing.T, out string) (string, mcpqual.EnrollmentEntry, *mcpqual.ExpectedOracle, mcpqual.Registry) {
	t.Helper()
	b := bundle(t, out)
	m := b.Manifest
	c := m.Clients[0]
	platform, version := m.OS+"/"+m.Arch, *c.ObservedVersion
	tr, err := mcpqual.ReplayTranscript(b.Files[mcpqual.ClientFile("claude", mcpqual.FileVendorEvents)])
	if err != nil || len(tr.Lines) != 1 {
		t.Fatalf("transcript: %v", err)
	}
	off := tr.Lines[0].OffsetNS
	o := &mcpqual.ExpectedOracle{CaseID: c.CaseID, Nonce: c.Nonce,
		Events: []mcpqual.Event{{CaseID: c.CaseID, Kind: mcpqual.KindToolCall, OffsetNS: off, RequestID: "toolu_1"},
			{CaseID: c.CaseID, Kind: mcpqual.KindToolResult, OffsetNS: off, RequestID: "toolu_1", Nonce: c.Nonce}},
		Terminal: true, ClientInfo: mcpqual.ExpectedClientInfo{Name: *c.Probe.ClientName, Version: *c.Probe.ClientVersion}, RequesterCompatible: true,
		ErrorKinds: []string{}, Capabilities: []string{mcpqual.CapToolCall, mcpqual.CapToolResult, mcpqual.CapTerminalSuccess}, CaptureState: mcpqual.CaptureComplete,
		SourceCaptureSHA256: sha(b.ManifestBytes), Attestation: mcpqual.Attestation{Owner: "function-test owner", Reviewer: "function-test reviewer", Policy: mcpqual.CaptureRedactionPolicy}}
	e := mcpqual.EnrollmentEntry{Client: "claude", Decoder: "claude-json", Version: version, Platform: platform, Fixture: mcpqual.EnrolledFixtureID("claude-json", version, platform),
		Bundle: mcpqual.EnrolledBundlePath("claude", version, platform, m.RunID), ManifestSHA256: sha(b.ManifestBytes)}
	repo := filepath.Join(realTemp(t), "repo")
	copyTree(t, out, filepath.Join(repo, filepath.FromSlash(e.Bundle)))
	writeEnrollment(t, repo, &e, o)
	reg := mcpqual.DefaultRegistry().WithVersion("claude-json", mcpqual.DecoderVersion{Version: version, Fixture: e.Fixture, Qualified: true,
		Evidence: []mcpqual.DecoderEvidence{{Platform: platform, Fixture: e.Fixture, Kinds: o.Capabilities}}})
	return repo, e, o, reg
}

// fixtureSlug is the observed 2026-10-08 Cursor project name of a
// workspace path: without its leading '/', '/' as '-' and the generated
// .work component as work.
func fixtureSlug(ws string) string {
	return strings.ReplaceAll(strings.ReplaceAll(strings.TrimPrefix(ws, "/"), "/.work/", "/work/"), "/", "-")
}

// projectRun is one in-process capture of the trusted Cursor recipe with
// the owner's .cursor (and, unless bare, its existing projects directory),
// plant run on the fixture before the capture, the fake's settings and the
// runner adjusted by mutate. It returns the environment, the client, the
// validated bundle and the output directory.
func projectRun(t *testing.T, bare bool, settings map[string]string, plant func(q *qualEnv, ws string), mutate func(c *mcpqual.CaptureRunner)) (*qualEnv, mcpqual.CaptureClient, *mcpqual.CaptureBundle, string) {
	t.Helper()
	q := newQualEnv(t)
	ownerCursor(t, q)
	if !bare {
		mkdir(t, filepath.Join(q.home, ".cursor", "projects", "home-owner-earlier-project"))
		os.WriteFile(filepath.Join(q.home, ".cursor", "projects", "home-owner-earlier-project", "mcp-approvals.json"), []byte(`["probe-aaaaaaaaaaaaaaaa"]`), 0o600)
	}
	out := filepath.Join(realTemp(t), "out")
	if plant != nil {
		plant(q, filepath.Join(out, ".work", "cursor-capture-setup"))
	}
	inProcessRun(t, q, cursorPlan(q, settings), out, mutate)
	b := bundle(t, out)
	return q, captureClient(t, b.Manifest, "cursor"), b, out
}

// approvalStopped requires a stop before the model session with reason,
// scope and the number of enable launches.
func approvalStopped(t *testing.T, q *qualEnv, c mcpqual.CaptureClient, reason, scope string, enables int) {
	t.Helper()
	log := q.launchLog()
	if !strings.HasPrefix(deref(c.Reason), reason) || c.Approval == nil || c.Approval.Scope != scope || len(log["enable"]) != enables || len(log["session"]) != 0 ||
		c.Session.State == mcpqual.StageRan || c.State == mcpqual.CaptureComplete {
		t.Fatalf("stopped: %s %+v %v", deref(c.Reason), c.Approval, log)
	}
}

// FP-15: the observed Cursor per-project approval is accepted only for the
// linux/amd64 2026.10.01-e373342 adapter with the computed project
// directory absent before the enable and exactly it and its validated
// mcp-approvals.json added after it; a pre-existing (or colliding)
// directory blocks the enable and the session; any other outside change
// is outside the workspace; an unsupported adapter, path or content is
// unverified. Workspace-only approval is unchanged.
func TestMCPCaptureCursorProjectApproval(t *testing.T) {
	t.Parallel()
	const dirLabel, fileLabel = "<home>/.cursor/projects/<workspace-project>", "<home>/.cursor/projects/<workspace-project>/mcp-approvals.json"
	runInventory(t, 4, []string{"project-added", "project-preexists", "outside-change", "unverifiable"}, []captureCase{
		{"project-added", func(t *testing.T) {
			q, c, b, out := projectRun(t, false, map[string]string{"ENABLE": "project"}, nil, nil)
			a := c.Approval
			ws := filepath.Join(out, ".work", "cursor-capture-setup")
			slug := fixtureSlug(ws)
			log := q.launchLog()
			if c.State != mcpqual.CaptureComplete || a.Scope != mcpqual.ScopeProjectScoped || a.Reason != nil || !a.InventoryComplete || len(log["enable"]) != 1 || len(log["session"]) != 1 {
				t.Fatalf("project approval: %s %+v %v", deref(c.Reason), a, log)
			}
			if a.Project == nil || *a.Project != (mcpqual.CaptureApprovalProject{Adapter: "cursor-linux-amd64-2026.10.01-e373342", Directory: dirLabel, File: fileLabel, ProbeEntryPresent: true}) ||
				!slices.Equal(a.Changes, []mcpqual.CaptureApprovalChange{{Path: dirLabel, Kind: "added"}, {Path: fileLabel, Kind: "added"}}) {
				t.Fatalf("project provenance %+v changes %+v", a.Project, a.Changes)
			}
			// The enable ran first, once: the launch log records it before
			// the only session.
			lb, _ := os.ReadFile(q.launches)
			if i, j := strings.Index(string(lb), "enable "), strings.Index(string(lb), "session "); i < 0 || j < i {
				t.Fatalf("launch order %s", lb)
			}
			// Sanitized: the slug (the absolute workspace) never leaves.
			stdout := string(b.Files[mcpqual.ClientFile("cursor", mcpqual.FileApprovalStdout)])
			if stdout != "probe enabled in project <workspace-project>\n" || bytes.Contains(b.ManifestBytes, []byte(slug)) {
				t.Fatalf("approval output %q", stdout)
			}
			for p, data := range b.Files {
				if bytes.Contains(data, []byte(slug)) || bytes.Contains(data, []byte("probe-6e58c4b6c129cbd0")) {
					t.Fatalf("%s exports the slug or the approval content", p)
				}
			}
			// No retry, rollback or deletion: the approval stays for the owner.
			if b, err := os.ReadFile(filepath.Join(q.home, ".cursor", "projects", slug, "mcp-approvals.json")); err != nil || string(b) != `["probe-6e58c4b6c129cbd0"]` {
				t.Fatalf("the project approval was changed: %q %v", b, err)
			}
			// Other non-probe entries are permitted; a workspace approval may
			// accompany the pair; the old workspace-only approval is unchanged.
			_, c, _, _ = projectRun(t, false, map[string]string{"ENABLE": "workspace,project", "APPROVALS": `["other-server","probe-0123456789abcdef"]`}, nil, nil)
			if c.State != mcpqual.CaptureComplete || c.Approval.Scope != mcpqual.ScopeProjectScoped || len(c.Approval.Changes) != 3 {
				t.Fatalf("project with workspace: %s %+v", deref(c.Reason), c.Approval)
			}
			_, c, _, _ = projectRun(t, false, nil, nil, nil)
			if c.State != mcpqual.CaptureComplete || c.Approval.Scope != mcpqual.ScopeWorkspaceOnly || c.Approval.Project != nil {
				t.Fatalf("workspace-only: %s %+v", deref(c.Reason), c.Approval)
			}
		}},
		{"project-preexists", func(t *testing.T) {
			// The computed directory before the enable (a prior run, or a
			// workspace whose path collides with this one): no enable, no
			// session, the existing approval untouched.
			existing := []byte(`["probe-0000000000000000"]`)
			for name, other := range map[string]func(ws string) string{
				"prior-run": func(ws string) string { return ws },
				"colliding": func(ws string) string {
					// <out>-work/cursor-capture/setup maps to the same name.
					out := filepath.Dir(filepath.Dir(ws))
					return filepath.Join(out+"-work", "cursor-capture", "setup")
				},
			} {
				var planted string
				q, c, _, _ := projectRun(t, false, map[string]string{"ENABLE": "project"}, func(q *qualEnv, ws string) {
					if fixtureSlug(other(ws)) != fixtureSlug(ws) {
						t.Fatalf("%s: %s does not collide with %s", name, other(ws), ws)
					}
					planted = filepath.Join(q.home, ".cursor", "projects", fixtureSlug(other(ws)), "mcp-approvals.json")
					mkdir(t, filepath.Dir(planted))
					os.WriteFile(planted, existing, 0o600)
				}, nil)
				approvalStopped(t, q, c, mcpqual.ReasonCursorScopeUnverified+": the computed Cursor project directory exists before the command", mcpqual.ScopeUnverifiable, 0)
				if c.Approval.Stage.State != mcpqual.StageNotRun {
					t.Fatalf("%s: stage %+v", name, c.Approval.Stage)
				}
				if b, err := os.ReadFile(planted); err != nil || !bytes.Equal(b, existing) {
					t.Fatalf("%s: the existing approval changed: %q %v", name, b, err)
				}
			}
		}},
		{"outside-change", func(t *testing.T) {
			for name, tc := range map[string]struct {
				bare   bool
				enable string
				change mcpqual.CaptureApprovalChange
			}{
				"wrong-slug":     {false, "project-other", mcpqual.CaptureApprovalChange{Kind: "added"}},
				"extra-file":     {false, "project-extra", mcpqual.CaptureApprovalChange{Path: dirLabel + "/extra.json", Kind: "added"}},
				"projects-added": {true, "project", mcpqual.CaptureApprovalChange{Path: "<home>/.cursor/projects", Kind: "added"}},
				"unrelated-file": {false, "owner", mcpqual.CaptureApprovalChange{Path: "<home>/.cursor/approved-servers.json", Kind: "added"}},
			} {
				q, c, _, _ := projectRun(t, tc.bare, map[string]string{"ENABLE": tc.enable}, nil, nil)
				approvalStopped(t, q, c, mcpqual.ReasonCursorOutside, mcpqual.ScopeOutside, 1)
				if tc.change.Path != "" && !slices.Contains(c.Approval.Changes, tc.change) {
					t.Fatalf("%s: changes %+v", name, c.Approval.Changes)
				}
				if c.Approval.Project != nil {
					t.Fatalf("%s: a project record on %s", name, c.Approval.Scope)
				}
			}
		}},
		{"unverifiable", func(t *testing.T) {
			for name, tc := range map[string]struct {
				settings map[string]string
				mutate   func(c *mcpqual.CaptureRunner)
				want     string
			}{
				"platform": {map[string]string{"ENABLE": "project"}, func(c *mcpqual.CaptureRunner) { c.GOOS, c.GOARCH = "darwin", "arm64" },
					"cannot be attributed: no project-path adapter for darwin/arm64"},
				"architecture":   {map[string]string{"ENABLE": "project"}, func(c *mcpqual.CaptureRunner) { c.GOARCH = "arm64" }, "cannot be attributed: no project-path adapter for linux/arm64"},
				"directory-only": {map[string]string{"ENABLE": "project-dir"}, nil, "the project approval needs both"},
				"not-array":      {map[string]string{"ENABLE": "project", "APPROVALS": `{"probe":true}`}, nil, "the project approval file is not a JSON array of strings"},
				"not-strings":    {map[string]string{"ENABLE": "project", "APPROVALS": `[1]`}, nil, "the project approval file is not a JSON array of strings"},
				"null-element":   {map[string]string{"ENABLE": "project", "APPROVALS": `[null,"probe-6e58c4b6c129cbd0"]`}, nil, "the project approval file is not a JSON array of strings"},
				"no-probe":       {map[string]string{"ENABLE": "project", "APPROVALS": `["other"]`}, nil, "does not hold exactly one probe entry"},
				"duplicate":      {map[string]string{"ENABLE": "project", "APPROVALS": `["probe-0123456789abcdef","probe-0123456789abcdef"]`}, nil, "does not hold exactly one probe entry"},
				"other-probe":    {map[string]string{"ENABLE": "project", "APPROVALS": `["probe-0123456789abcdef","probe-XYZ"]`}, nil, "holds another probe- entry"},
			} {
				q, c, _, _ := projectRun(t, false, tc.settings, nil, tc.mutate)
				approvalStopped(t, q, c, mcpqual.ReasonCursorScopeUnverified+": ", mcpqual.ScopeUnverifiable, 1)
				if !strings.Contains(deref(c.Reason), tc.want) {
					t.Fatalf("%s: %s", name, deref(c.Reason))
				}
			}
			// An unsupported version and an unsupported path derive nothing:
			// a home-project change is unverified, while the workspace-only
			// rule still succeeds.
			version := func(c *mcpqual.CaptureRunner) {
				pc := &c.Plan.Clients[0]
				pc.ExpectedVersion = "2026.10.02-0000000"
				pc.Env["FAKE_VENDOR_VERSION"] = pc.ExpectedVersion
			}
			q, c, _, _ := projectRun(t, false, map[string]string{"ENABLE": "project"}, nil, version)
			approvalStopped(t, q, c, mcpqual.ReasonCursorScopeUnverified+": a change under <home>/.cursor/projects/ cannot be attributed: no project-path adapter for this Cursor version", mcpqual.ScopeUnverifiable, 1)
			_, c, _, _ = projectRun(t, false, nil, nil, version)
			if c.State != mcpqual.CaptureComplete || c.Approval.Scope != mcpqual.ScopeWorkspaceOnly {
				t.Fatalf("workspace-only on another version: %s", deref(c.Reason))
			}
			for _, enable := range []string{"project", ""} {
				q := newQualEnv(t)
				ownerCursor(t, q)
				mkdir(t, filepath.Join(q.home, ".cursor", "projects"))
				out := filepath.Join(realTemp(t), "o.ut")
				inProcessRun(t, q, cursorPlan(q, map[string]string{"ENABLE": enable}), out, nil)
				c := captureClient(t, bundle(t, out).Manifest, "cursor")
				if enable == "project" {
					approvalStopped(t, q, c, mcpqual.ReasonCursorScopeUnverified+": a change under <home>/.cursor/projects/ cannot be attributed: a workspace path component", mcpqual.ScopeUnverifiable, 1)
				} else if c.State != mcpqual.CaptureComplete || c.Approval.Scope != mcpqual.ScopeWorkspaceOnly {
					t.Fatalf("workspace-only on an unsupported path: %s", deref(c.Reason))
				}
			}
		}},
	})
}

// dataDirs plants n files in each of the owner's excluded data
// directories.
func dataDirs(t *testing.T, q *qualEnv, n int) {
	t.Helper()
	for _, d := range []string{"chats", "ai-tracking"} {
		mkdir(t, filepath.Join(q.home, ".cursor", d))
		for i := 0; i < n; i++ {
			os.WriteFile(filepath.Join(q.home, ".cursor", d, "item-"+strings.Repeat("x", i%3)+string(rune('a'+i%26))+".json"), []byte(`{"chat":"old"}`), 0o600)
		}
	}
}

// FP-16: only the contents of the owner's .cursor/chats and
// .cursor/ai-tracking are excluded, by absolute identity: their growth no
// longer blocks a capture under small injected caps, while an unrelated
// directory of the same name is monitored; a boundary symbolic link, file
// or replacement fails closed; relevant over-cap and special paths still
// block; every new record names the exact policy, validated strictly, and
// the owner precheck documents the same policy.
func TestMCPCaptureCursorInventoryPolicy(t *testing.T) {
	t.Parallel()
	caps := func(n int) func(c *mcpqual.CaptureRunner) {
		return func(c *mcpqual.CaptureRunner) { c.ApprovalLimits = mcpqual.ApprovalScanLimits{Entries: n} }
	}
	runInventory(t, 4, []string{"excluded-data", "boundary-invalid", "relevant-limit", "policy-evidence"}, []captureCase{
		{"excluded-data", func(t *testing.T) {
			// 2 x 26 excluded files under a 25-entry cap; the enable adds more
			// to both and a valid project approval.
			q, c, b, _ := projectRun(t, false, map[string]string{"ENABLE": "project,data"}, func(q *qualEnv, _ string) { dataDirs(t, q, 26) }, caps(25))
			if c.State != mcpqual.CaptureComplete || c.Approval.Scope != mcpqual.ScopeProjectScoped || !c.Approval.InventoryComplete || len(c.Approval.Changes) != 2 {
				t.Fatalf("excluded data: %s %+v", deref(c.Reason), c.Approval)
			}
			if bytes.Contains(b.ManifestBytes, []byte("enable-chat")) || len(q.launchLog()["session"]) != 1 {
				t.Fatal("an excluded data change was inventoried")
			}
			// The same directories on an absent boundary appear as boundary
			// additions: outside the workspace, contents never listed.
			q, c, _, _ = projectRun(t, false, map[string]string{"ENABLE": "workspace,data"}, nil, nil)
			approvalStopped(t, q, c, mcpqual.ReasonCursorOutside, mcpqual.ScopeOutside, 1)
			if !slices.Contains(c.Approval.Changes, mcpqual.CaptureApprovalChange{Path: "<home>/.cursor/chats", Kind: "added"}) ||
				slices.ContainsFunc(c.Approval.Changes, func(ch mcpqual.CaptureApprovalChange) bool {
					return strings.HasPrefix(ch.Path, "<home>/.cursor/chats/")
				}) {
				t.Fatalf("boundary addition %+v", c.Approval.Changes)
			}
			// An unrelated chats directory (an ancestor's .cursor) is scanned.
			q, c, _, _ = projectRun(t, false, map[string]string{"ENABLE": "workspace,ancestor-chats"}, nil, nil)
			approvalStopped(t, q, c, mcpqual.ReasonCursorOutside, mcpqual.ScopeOutside, 1)
			if !slices.Contains(c.Approval.Changes, mcpqual.CaptureApprovalChange{Path: "<ancestor-1>/.cursor/chats/note.json", Kind: "added"}) {
				t.Fatalf("ancestor chats %+v", c.Approval.Changes)
			}
		}},
		{"boundary-invalid", func(t *testing.T) {
			for name, plant := range map[string]func(q *qualEnv, _ string){
				"chats-link": func(q *qualEnv, _ string) { os.Symlink("/", filepath.Join(q.home, ".cursor", "chats")) },
				"tracking-file": func(q *qualEnv, _ string) {
					os.WriteFile(filepath.Join(q.home, ".cursor", "ai-tracking"), []byte("x"), 0o600)
				},
			} {
				q, c, _, _ := projectRun(t, false, map[string]string{"ENABLE": "project"}, plant, nil)
				approvalStopped(t, q, c, mcpqual.ReasonCursorScopeUnverified+": the inventory before the command is incomplete (an excluded data directory", mcpqual.ScopeUnverifiable, 0)
				_ = name
			}
			// Replaced by a symbolic link during the enable: the inventory
			// after it is incomplete, so no session.
			q, c, _, _ := projectRun(t, false, map[string]string{"ENABLE": "workspace,chats-link"}, func(q *qualEnv, _ string) { dataDirs(t, q, 2) }, nil)
			approvalStopped(t, q, c, mcpqual.ReasonCursorScopeUnverified+": the inventory after the command is incomplete (an excluded data directory", mcpqual.ScopeUnverifiable, 1)
			if c.Approval.InventoryComplete {
				t.Fatalf("replaced boundary %+v", c.Approval)
			}
		}},
		{"relevant-limit", func(t *testing.T) {
			// Relevant entries over the cap block before the enable; a special
			// file in projects still fails; growth past the cap during the
			// enable stops the session.
			q, c, _, _ := projectRun(t, false, map[string]string{"ENABLE": "project"}, func(q *qualEnv, _ string) {
				for i := 0; i < 26; i++ {
					os.WriteFile(filepath.Join(q.home, ".cursor", "projects", "home-owner-earlier-project", "f"+string(rune('a'+i))), []byte("1"), 0o600)
				}
			}, caps(25))
			approvalStopped(t, q, c, mcpqual.ReasonCursorScopeUnverified+": the inventory before the command is incomplete (the entry bound", mcpqual.ScopeUnverifiable, 0)
			q, c, _, _ = projectRun(t, false, map[string]string{"ENABLE": "project"}, func(q *qualEnv, _ string) {
				if err := syscall.Mkfifo(filepath.Join(q.home, ".cursor", "projects", "home-owner-earlier-project", "socket"), 0o600); err != nil {
					t.Fatal(err)
				}
			}, nil)
			approvalStopped(t, q, c, mcpqual.ReasonCursorScopeUnverified+": the inventory before the command is incomplete (a special file", mcpqual.ScopeUnverifiable, 0)
			q, c, _, _ = projectRun(t, false, map[string]string{"ENABLE": "workspace-flood", "FLOOD_FILES": "30"}, nil, caps(25))
			approvalStopped(t, q, c, mcpqual.ReasonCursorScopeUnverified+": the inventory after the command is incomplete (the entry bound", mcpqual.ScopeUnverifiable, 1)
		}},
		{"policy-evidence", func(t *testing.T) {
			_, c, _, out := projectRun(t, false, map[string]string{"ENABLE": "project"}, nil, nil)
			want := []string{"<home>/.cursor/chats", "<home>/.cursor/ai-tracking"}
			if a := c.Approval; a.InventoryPolicy == nil || *a.InventoryPolicy != "cursor-approval-v2" || !slices.Equal(a.ExcludedPaths, want) {
				t.Fatalf("policy %+v", a)
			}
			// A failed pre-scan records the same policy.
			_, failed, _, _ := projectRun(t, false, nil, func(q *qualEnv, _ string) { os.Symlink("/", filepath.Join(q.home, ".cursor", "chats")) }, nil)
			if a := failed.Approval; a.InventoryPolicy == nil || !slices.Equal(a.ExcludedPaths, want) {
				t.Fatalf("failed pre-scan policy %+v", a)
			}
			approval := func(m map[string]any) map[string]any { return client0(m)["approval"].(map[string]any) }
			tampered(t, out, map[string]func(m map[string]any){
				"unknown-policy": func(m map[string]any) { approval(m)["inventory_policy"] = "cursor-approval-v3" },
				"reordered":      func(m map[string]any) { approval(m)["excluded_paths"] = []any{want[1], want[0]} },
				"one-path":       func(m map[string]any) { approval(m)["excluded_paths"] = []any{want[0]} },
				"extra-path": func(m map[string]any) {
					approval(m)["excluded_paths"] = []any{want[0], want[1], "<home>/.cursor/projects"}
				},
				"null-policy":  func(m map[string]any) { approval(m)["inventory_policy"] = nil },
				"null-paths":   func(m map[string]any) { approval(m)["excluded_paths"] = nil },
				"policy-only":  func(m map[string]any) { delete(approval(m), "excluded_paths") },
				"no-policy":    func(m map[string]any) { delete(approval(m), "excluded_paths"); delete(approval(m), "inventory_policy") },
				"null-project": func(m map[string]any) { approval(m)["project"] = nil },
				"no-project":   func(m map[string]any) { delete(approval(m), "project") },
				"no-entry":     func(m map[string]any) { approval(m)["project"].(map[string]any)["probe_entry_present"] = false },
				"other-adapter": func(m map[string]any) {
					approval(m)["project"].(map[string]any)["adapter"] = "cursor-darwin-arm64-2026.10.01-e373342"
				},
				"extra-member":  func(m map[string]any) { approval(m)["project"].(map[string]any)["slug"] = "x" },
				"modified-file": func(m map[string]any) { approval(m)["changes"].([]any)[1].(map[string]any)["kind"] = "modified" },
				"missing-dir":   func(m map[string]any) { approval(m)["changes"] = approval(m)["changes"].([]any)[1:] },
				"forbidden-path": func(m map[string]any) {
					approval(m)["changes"].([]any)[1].(map[string]any)["path"] = "<home>/.cursor/projects/<workspace-project>/x.json"
				},
				"misplaced-label": func(m map[string]any) {
					approval(m)["changes"].([]any)[0].(map[string]any)["path"] = "<home>/.cursor/<workspace-project>"
				},
				// The provenance is bound to linux/amd64 and the exact version.
				"darwin": func(m map[string]any) { m["os"] = "darwin" },
				"arm64":  func(m map[string]any) { m["arch"] = "arm64" },
				"version": func(m map[string]any) {
					client0(m)["expected_version"], client0(m)["observed_version"] = "2026.10.02-0000000", "2026.10.02-0000000"
				},
			})
			// A legacy workspace-only record (neither policy field) stays
			// valid and means the original full inventory.
			_, _, _, wout := projectRun(t, false, nil, nil, nil)
			d := filepath.Join(realTemp(t), "legacy")
			copyTree(t, wout, d)
			editManifest(t, d, func(m map[string]any) { delete(approval(m), "inventory_policy"); delete(approval(m), "excluded_paths") })
			if _, err := mcpqual.ValidateCaptureBundle(os.DirFS(d), "."); err != nil {
				t.Fatalf("legacy record: %v", err)
			}
			// The owner precheck runs the same policy.
			guide := string(repoFile(t, mcpqual.SetupDocPath))
			for _, w := range []string{"`cursor-approval-v2`", "`~/.cursor/chats`", "`~/.cursor/ai-tracking`", "contents only",
				"a symbolic link, a file or an unreadable entry in place of either directory", "every `.cursor/projects` entry"} {
				if !strings.Contains(guide, w) {
					t.Fatalf("the precheck lacks %q", w)
				}
			}
		}},
	})
}
