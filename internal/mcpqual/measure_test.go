package mcpqual

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/wedevwork/callsheet/internal/testkit"
)

// FP-13 unit tests: every measurement phase, the session and time budgets,
// repeat evidence, lower bounds versus an observed timeout, a missing
// progress token, absent and unauthenticated clients, an unknown absolute
// cap, and probe-only intervals with distinct harness and probe clock
// origins. Every launch is a fake; no process starts and no time passes
// except by explicit fake-clock advances.

func TestMeasureAllPhases(t *testing.T) {
	w := newWorld(t, fullModel())
	r := newRunner(t, w, fullPlan())
	rep := runPlan(t, r, context.Background())
	if strictReport(t, r).RunID != rep.RunID {
		t.Fatal("the written report differs")
	}
	want := []string{
		"setup:conclusive:healthy: obs=1 cases=1",
		"default:conclusive:timeout_observed (300 ms, 500 ms]: obs=2 cases=4",
		"override:conclusive:override_effective_bound_observed (600 ms, 1000 ms]: obs=1 cases=2",
		"progress:conclusive:progress_extends (> 900 ms tested): obs=1 cases=1",
		"absolute:conclusive:absolute_cap_observed (900 ms, 1200 ms]: obs=1 cases=1",
	}
	for i, ph := range rep.Clients[0].Phases {
		if ph.String() != want[i] {
			t.Errorf("phase %d = %s, want %s", i, ph, want[i])
		}
	}
	c := rep.Clients[0]
	if rep.Outcome != StatusConclusive || c.Outcome != StatusConclusive || rep.ExitCode() != 0 || rep.VendorBehavior != VendorMeasured || !rep.Cleanup.OK {
		t.Fatalf("report %+v", rep)
	}
	if *c.ClientInfo.Name != "claude-code" || !*c.ClientInfo.RequesterCompatible || c.ClientInfo.UnqualifiedReason != nil || *c.ObservedVersion != "2.1.282 (Claude Code)" {
		t.Fatalf("client %+v", c.ClientInfo)
	}
	// The progress interval is min(1s, T/4) with T the 300 ms lower bound.
	if ph := phase(t, rep, "claude", PhaseProgress); ph.Cases[0].ProgressIntervalMS != 75 || ph.Cases[0].ProgressSent != 11 {
		t.Fatalf("progress case %+v", ph.Cases[0])
	}
	// The planned upper bound was reported before launching: 1 setup, 3
	// delays and a repeat, 2 override, 1 progress, 1 absolute.
	if rep.PlannedUpperBound.Sessions != 9 || rep.PlannedUpperBound.WallMS != 60000 {
		t.Fatalf("planned %+v", rep.PlannedUpperBound)
	}
	// Sessions ran with the substituted argv, the plan's and the raised
	// recipe's environment and never CI.
	ss := w.sessions()
	if len(ss) != 9 {
		t.Fatalf("%d sessions", len(ss))
	}
	for _, s := range ss {
		env := strings.Join(s.Env, "\n")
		if s.Path != "/fake/claude" || !strings.Contains(env, "FAKE_SECRET=hunter2-secret-value") || strings.Contains(env, "CI=") ||
			placeholderRe.MatchString(strings.Join(s.Args, " ")) || s.Args[7] != "/opt/mcpqual" {
			t.Fatalf("session spec %+v", s)
		}
	}
	if !strings.Contains(strings.Join(ss[5].Env, " "), "FAKE_TIMEOUT=long-raised-value") || strings.Contains(strings.Join(ss[1].Env, " "), "FAKE_TIMEOUT") {
		t.Fatal("the raised recipe's environment was not confined to raised cases")
	}
	md, _ := os.ReadFile(filepath.Join(r.OutDir, "report.md"))
	for _, s := range []string{HarnessVerified, VendorMeasured, "synthetic fixture only", `clientInfo name "claude-code"`} {
		if !strings.Contains(string(md), s) {
			t.Fatalf("report.md lacks %q:\n%s", s, md)
		}
	}
	man, _ := os.ReadFile(filepath.Join(r.OutDir, "manifest.json"))
	if !strings.Contains(string(man), `"path": "report.json"`) || !strings.Contains(string(man), "cases/claude-setup/server-events.jsonl") {
		t.Fatalf("manifest %s", man)
	}
}

// Intervals come from the probe clock only: the probe's origin is ~2 h
// away from the harness clock and vendor offsets stay on the harness
// clock; nothing subtracts one from the other.
func TestMeasureClockDomains(t *testing.T) {
	for _, origin := range []int64{1 << 62} {
		w := newWorld(t, fullModel())
		w.origin = origin
		w.clock.Set(epoch.Add(3 * time.Hour))
		rep := runPlan(t, newRunner(t, w, planWith(defaultOnly(100, 300, 900))), context.Background())
		def := phase(t, rep, "claude", PhaseDefault)
		to := def.Cases[2]
		if *def.UpperBoundMS != 500 || *to.ElapsedMS != 500 || *to.StartOffsetNS != origin+2*int64(time.Millisecond) || *to.EndOffsetNS != origin+502*int64(time.Millisecond) {
			t.Fatalf("origin %d: %s %+v", origin, def, to)
		}
		for _, ev := range to.Events {
			if ev.OffsetNS >= int64(time.Second) {
				t.Fatalf("vendor event offset %d is not on the harness clock", ev.OffsetNS)
			}
		}
	}
}

func TestMeasureLowerBoundOnly(t *testing.T) {
	w := newWorld(t, vendorModel{})
	p := fullPlan()
	rep := runPlan(t, newRunner(t, w, p), context.Background())
	got := []string{}
	for _, ph := range rep.Clients[0].Phases {
		got = append(got, ph.String())
	}
	want := "setup:conclusive:healthy: obs=1 cases=1|default:conclusive:lower_bound (> 900 ms tested): obs=2 cases=4|" +
		"override:not_run::no observed default timeout to exceed obs=0 cases=0|progress:not_run::no known failing silent case obs=0 cases=0|" +
		"absolute:not_run::progress extension not established obs=0 cases=0"
	if strings.Join(got, "|") != want {
		t.Fatalf("phases\n%s\nwant\n%s", strings.Join(got, "|"), want)
	}
	if rep.Outcome != "partial" || rep.ExitCode() != 5 || !strings.HasPrefix(*rep.Clients[0].Reason, "override:") {
		t.Fatalf("outcome %s exit %d", rep.Outcome, rep.ExitCode())
	}
}

func TestMeasureProgressOutcomes(t *testing.T) {
	for name, tc := range map[string]struct {
		m        vendorModel
		progress string
		absolute string
	}{
		"no-token":         {vendorModel{timeoutMS: 500, raisedMS: 1000, noToken: true}, "progress:inconclusive::no progress token: the progress experiment is inconclusive obs=0 cases=1", "absolute:not_run::progress extension not established obs=0 cases=0"},
		"does-not-extend":  {vendorModel{timeoutMS: 500, raisedMS: 1000}, "progress:conclusive:progress_does_not_extend (<= 500 ms): obs=1 cases=1", "absolute:conclusive:not_applicable: obs=0 cases=0"},
		"no-cap-observed":  {vendorModel{timeoutMS: 500, raisedMS: 1000, resets: true}, "progress:conclusive:progress_extends (> 900 ms tested): obs=1 cases=1", "absolute:conclusive:no_absolute_cap_observed (> 1500 ms tested): obs=1 cases=1"},
		"cap-before-delay": {vendorModel{timeoutMS: 500, raisedMS: 1000, resets: true, capMS: 800}, "progress:conclusive:progress_extends (500 ms, 800 ms]: obs=1 cases=1", "absolute:conclusive:absolute_cap_observed (500 ms, 800 ms]: obs=1 cases=0"},
	} {
		t.Run(name, func(t *testing.T) {
			p := planWith(Phases{Default: &DefaultPhase{DelaysMS: []int64{300, 900}}, Progress: &struct{}{}, Absolute: &DelayPhase{DelayMS: 1500}})
			rep := runPlan(t, newRunner(t, newWorld(t, tc.m), p), context.Background())
			if got := phase(t, rep, "claude", PhaseProgress).String(); got != tc.progress {
				t.Errorf("progress %s, want %s", got, tc.progress)
			}
			if got := phase(t, rep, "claude", PhaseAbsolute).String(); got != tc.absolute {
				t.Errorf("absolute %s, want %s", got, tc.absolute)
			}
		})
	}
}

func TestMeasureOverrideOutcomes(t *testing.T) {
	fullPlan := func() *Plan {
		return planWith(Phases{Default: &DefaultPhase{DelaysMS: []int64{300, 900}}, Override: &DelayPhase{DelayMS: 600, BoundDelayMS: 2000}})
	}
	rep := runPlan(t, newRunner(t, newWorld(t, vendorModel{timeoutMS: 500, raisedMS: 500}), fullPlan()), context.Background())
	if got := phase(t, rep, "claude", PhaseOverride).String(); got != "override:conclusive:override_not_effective (<= 500 ms): obs=1 cases=1" {
		t.Fatal(got)
	}
	rep = runPlan(t, newRunner(t, newWorld(t, vendorModel{timeoutMS: 500, raisedMS: 5000}), fullPlan()), context.Background())
	if got := phase(t, rep, "claude", PhaseOverride).String(); got != "override:conclusive:override_effective (> 2000 ms tested): obs=1 cases=2" {
		t.Fatal(got)
	}
	p := fullPlan()
	p.Clients[0].Phases.Override.DelayMS = 400
	rep = runPlan(t, newRunner(t, newWorld(t, fullModel()), p), context.Background())
	if got := phase(t, rep, "claude", PhaseOverride).String(); !strings.Contains(got, "override:inconclusive::override delay 400 ms does not exceed the observed default 500 ms") {
		t.Fatal(got)
	}
}

func TestMeasureDefaultPhaseEdges(t *testing.T) {
	t.Run("first-delay-times-out", func(t *testing.T) {
		rep := runPlan(t, newRunner(t, newWorld(t, vendorModel{timeoutMS: 50}), planWith(defaultOnly(100, 300, 900))), context.Background())
		if got := phase(t, rep, "claude", PhaseDefault).String(); got != "default:conclusive:timeout_observed (<= 50 ms): obs=1 cases=1" {
			t.Fatal(got)
		}
	})
	t.Run("probe-endpoint-missing", func(t *testing.T) {
		rep := runPlan(t, newRunner(t, newWorld(t, vendorModel{timeoutMS: 500, noCancelEvent: true}), planWith(defaultOnly(100, 300, 900))), context.Background())
		ph := phase(t, rep, "claude", PhaseDefault)
		if ph.Status != StatusInconclusive || *ph.Reason != ReasonProbeEndpoint || ph.Cases[2].ElapsedMS != nil || *ph.Cases[2].Reason != ReasonProbeEndpoint {
			t.Fatalf("%s %+v", ph, ph.Cases[2])
		}
	})
	t.Run("repeat-fails", func(t *testing.T) {
		m := fullModel()
		m.scenario = func(c string) string {
			if strings.HasSuffix(c, "default-repeat") {
				return scNoCall
			}
			return ""
		}
		rep := runPlan(t, newRunner(t, newWorld(t, m), planWith(defaultOnly(100, 300, 900))), context.Background())
		if got := phase(t, rep, "claude", PhaseDefault).String(); !strings.Contains(got, "repeat observation failed: inconclusive: model_did_not_call") {
			t.Fatal(got)
		}
	})
	t.Run("session-budget", func(t *testing.T) {
		p := planWith(defaultOnly(100, 300, 900))
		p.Limits.MaxSessionsPerClient = 3
		rep := runPlan(t, newRunner(t, newWorld(t, fullModel()), p), context.Background())
		if got := phase(t, rep, "claude", PhaseDefault).String(); got != "default:inconclusive::repeat observation missing: budget_exhausted: max_sessions_per_client obs=0 cases=2" {
			t.Fatal(got)
		}
	})
	t.Run("time-budget", func(t *testing.T) {
		p := planWith(defaultOnly(100, 300, 900))
		p.Limits.MaxClientMS = 10000
		m := vendorModel{advance: true}
		p.Clients[0].Phases.Default.DelaysMS = []int64{4000, 5000, 6000}
		rep := runPlan(t, newRunner(t, newWorld(t, m), p), context.Background())
		if got := phase(t, rep, "claude", PhaseDefault).String(); got != "default:inconclusive::repeat observation missing: budget_exhausted: max_client_ms obs=0 cases=2" {
			t.Fatal(got)
		}
	})
	t.Run("delay-beyond-case-limit", func(t *testing.T) {
		p := planWith(defaultOnly(20000))
		rep := runPlan(t, newRunner(t, newWorld(t, fullModel()), p), context.Background())
		if got := phase(t, rep, "claude", PhaseDefault).String(); !strings.Contains(got, "delay does not fit max_case_ms") {
			t.Fatal(got)
		}
	})
	t.Run("default-delays", func(t *testing.T) {
		p := fullPlan()
		p.Clients[0].Phases = Phases{Default: &DefaultPhase{}}
		p.Limits = nil
		rep := runPlan(t, newRunner(t, newWorld(t, vendorModel{timeoutMS: 20000}), p), context.Background())
		ph := phase(t, rep, "claude", PhaseDefault)
		if ph.String() != "default:conclusive:timeout_observed (15000 ms, 20000 ms]: obs=2 cases=4" || ph.Cases[1].DelayMS != 15000 {
			t.Fatal(ph)
		}
	})
}

func TestMeasureClientFailures(t *testing.T) {
	stopped := func(t *testing.T, rep *Report, reason string, launches int, w *fakeLauncher) {
		t.Helper()
		c := rep.Clients[0]
		if c.Outcome == StatusConclusive || c.Reason == nil || !strings.Contains(*c.Reason, reason) || len(w.sessions()) != launches || rep.ExitCode() != 5 {
			t.Fatalf("outcome %s reason %v sessions %d", c.Outcome, c.Reason, len(w.sessions()))
		}
	}
	t.Run("absent", func(t *testing.T) {
		w := newWorld(t, fullModel())
		p := fullPlan()
		p.Clients[0].Executable = "/missing/claude"
		rep := runPlan(t, newRunner(t, w, p), context.Background())
		stopped(t, rep, ReasonAbsentBinary, 0, w)
		if len(w.launches) != 0 || rep.VendorBehavior != VendorNotRun {
			t.Fatal("an absent client was launched")
		}
	})
	t.Run("absent-at-launch", func(t *testing.T) {
		w := newWorld(t, fullModel())
		w.startErr = os.ErrNotExist
		rep := runPlan(t, newRunner(t, w, planWith(defaultOnly(100, 300, 900))), context.Background())
		stopped(t, rep, ReasonAbsentBinary, 0, w)
		w.startErr = errors.New("exec format error")
		rep = runPlan(t, newRunner(t, w, planWith(defaultOnly(100, 300, 900))), context.Background())
		stopped(t, rep, ReasonLaunchFailed, 0, w)
	})
	t.Run("version-mismatch", func(t *testing.T) {
		w := newWorld(t, vendorModel{version: "2.2.0 (Claude Code)"})
		rep := runPlan(t, newRunner(t, w, planWith(defaultOnly(100, 300, 900))), context.Background())
		stopped(t, rep, ReasonVersionMismatch, 0, w)
		if *rep.Clients[0].ObservedVersion != "2.2.0 (Claude Code)" {
			t.Fatal("observed version not recorded")
		}
	})
	t.Run("unsupported-decoder-version", func(t *testing.T) {
		w := newWorld(t, vendorModel{version: "9.9.9 (Claude Code)"})
		p := fullPlan()
		p.Clients[0].ExpectedVersion = "9.9.9 (Claude Code)"
		rep := runPlan(t, newRunner(t, w, p), context.Background())
		stopped(t, rep, ReasonDecoderVersion, 0, w)
	})
	t.Run("unauthenticated", func(t *testing.T) {
		m := fullModel()
		m.scenario = func(string) string { return scAuth }
		w := newWorld(t, m)
		rep := runPlan(t, newRunner(t, w, planWith(defaultOnly(100, 300, 900))), context.Background())
		stopped(t, rep, ReasonAuth, 1, w)
		if ph := phase(t, rep, "claude", PhaseDefault); ph.Status != StatusNotRun {
			t.Fatal(ph)
		}
	})
	t.Run("approval-denied-mid-run", func(t *testing.T) {
		m := fullModel()
		m.scenario = func(c string) string {
			if strings.HasSuffix(c, "default-2") {
				return scPermission
			}
			return ""
		}
		w := newWorld(t, m)
		rep := runPlan(t, newRunner(t, w, planWith(defaultOnly(100, 300, 900))), context.Background())
		stopped(t, rep, ReasonPermission, 3, w)
	})
	t.Run("model-unavailable", func(t *testing.T) {
		m := fullModel()
		m.scenario = func(string) string { return scNoModel }
		w := newWorld(t, m)
		rep := runPlan(t, newRunner(t, w, planWith(defaultOnly(100, 300, 900))), context.Background())
		stopped(t, rep, ReasonModelUnavailable, 1, w)
	})
	t.Run("session-error", func(t *testing.T) {
		m := fullModel()
		m.scenario = func(string) string { return scSession }
		w := newWorld(t, m)
		rep := runPlan(t, newRunner(t, w, planWith(Phases{})), context.Background())
		stopped(t, rep, ReasonSessionError, 1, w)
	})
	t.Run("model-refusal", func(t *testing.T) {
		m := fullModel()
		m.scenario = func(string) string { return scRefusal }
		w := newWorld(t, m)
		rep := runPlan(t, newRunner(t, w, planWith(defaultOnly(100, 300, 900))), context.Background())
		stopped(t, rep, ReasonRefusal, 1, w)
	})
	t.Run("client-info-incompatible", func(t *testing.T) {
		w := newWorld(t, vendorModel{timeoutMS: 500, clientName: "Claude Code", clientVersion: "2.1.282"})
		rep := runPlan(t, newRunner(t, w, planWith(defaultOnly(100, 300, 900))), context.Background())
		ci := rep.Clients[0].ClientInfo
		if *ci.Name != "Claude Code" || *ci.RequesterCompatible || !strings.Contains(*ci.ValidationReason, "without spaces") || !strings.Contains(*ci.UnqualifiedReason, "incompatible") {
			t.Fatalf("client info %+v", ci)
		}
		if phase(t, rep, "claude", PhaseSetup).Status != StatusConclusive {
			t.Fatal("tool discovery success depends on attribution")
		}
	})
}

func TestMeasureModelCallGate(t *testing.T) {
	p := fullPlan()
	p.Clients[0].Driver, p.Clients[0].Model = DriverModel, "model-x"
	w := newWorld(t, fullModel())
	rep := runPlan(t, newRunner(t, w, p), context.Background())
	if len(w.sessions()) != 0 || len(w.launches) != 1 || *rep.Clients[0].Reason != ReasonModelCallsDenied || rep.PlannedUpperBound.Sessions != 0 {
		t.Fatalf("sessions %d launches %d reason %v", len(w.sessions()), len(w.launches), rep.Clients[0].Reason)
	}
	w = newWorld(t, fullModel())
	p.Clients[0].Phases = Phases{}
	r := newRunner(t, w, p)
	r.AllowModelCalls = true
	if rep := runPlan(t, r, context.Background()); len(w.sessions()) != 1 || rep.ExitCode() != 0 || !rep.ModelCallsAllowed {
		t.Fatalf("allowed run: %d sessions", len(w.sessions()))
	}
}

func TestMeasureWatchdogAndInterrupt(t *testing.T) {
	t.Run("watchdog", func(t *testing.T) {
		w := newWorld(t, fullModel())
		w.hang = func(s ProcSpec) bool { return strings.Contains(strings.Join(s.Args, " "), "claude-default-1") }
		p := fullPlan()
		p.Clients[0].Phases = Phases{Default: &DefaultPhase{DelaysMS: []int64{100}}}
		r := newRunner(t, w, p)
		reaper := &fakeReaper{}
		r.Reaper = reaper
		done := make(chan *Report, 1)
		go func() { rep, _ := r.Run(context.Background()); done <- rep }()
		for s := range w.started {
			if strings.Contains(strings.Join(s.Args, " "), "claude-default-1") {
				break
			}
		}
		if err := w.clock.AwaitWaiter(testWait, testkit.HasTimer(10*time.Second)); err != nil {
			t.Fatal(err)
		}
		w.clock.Advance(10 * time.Second)
		rep := <-done
		cs := phase(t, rep, "claude", PhaseDefault).Cases[0]
		if cs.Outcome != OutcomeInconclusive || *cs.Reason != ReasonSessionTimeout || len(reaper.pgids) != 3 {
			t.Fatalf("case %+v reaps %v", cs, reaper.pgids)
		}
	})
	t.Run("interrupt", func(t *testing.T) {
		w := newWorld(t, fullModel())
		w.hang = func(s ProcSpec) bool { return strings.Contains(strings.Join(s.Args, " "), "claude-default-2") }
		r := newRunner(t, w, fullPlan())
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan *Report, 1)
		go func() { rep, _ := r.Run(ctx); done <- rep }()
		for s := range w.started {
			if strings.Contains(strings.Join(s.Args, " "), "claude-default-2") {
				break
			}
		}
		if err := w.clock.AwaitWaiter(testWait, testkit.HasTimer(10*time.Second)); err != nil {
			t.Fatal(err)
		}
		cancel()
		rep := <-done
		if !rep.Interrupted || rep.ExitCode() != 130 || rep.Outcome != "partial" {
			t.Fatalf("report %+v", rep)
		}
		ph := phase(t, rep, "claude", PhaseDefault)
		if ph.Cases[1].Outcome != OutcomeInterrupted || phase(t, rep, "claude", PhaseOverride).Status != StatusNotRun {
			t.Fatal(ph)
		}
		strictReport(t, r)
	})
	t.Run("interrupted-before-client", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		w := newWorld(t, fullModel())
		rep := runPlan(t, newRunner(t, w, fullPlan()), ctx)
		if len(w.launches) != 0 || *rep.Clients[0].Reason != ReasonInterrupted || rep.ExitCode() != 130 {
			t.Fatalf("launches %d", len(w.launches))
		}
	})
}

func TestMeasureCleanupFailureStopsRun(t *testing.T) {
	p := fullPlan()
	second := p.Clients[0]
	second.ID, second.Decoder, second.DecoderFixture, second.Executable, second.ExpectedVersion = "codex", "codex-jsonl", "codex-jsonl/synthetic", "/fake/codex", "codex-cli 0.156.1"
	p.Clients = append(p.Clients, second)
	w := newWorld(t, fullModel())
	r := newRunner(t, w, p)
	r.Reaper = &fakeReaper{fail: func(pgid int) bool { return pgid == 1003 }}
	rep := runPlan(t, r, context.Background())
	if rep.Cleanup.OK || len(rep.Cleanup.Failures) != 1 || !strings.Contains(rep.Cleanup.Failures[0], "claude-default-1") || rep.ExitCode() != 5 {
		t.Fatalf("cleanup %+v", rep.Cleanup)
	}
	if *rep.Clients[1].Reason != ReasonCleanupFailed || len(w.launches) != 3 {
		t.Fatalf("the run continued after a cleanup failure: %d launches", len(w.launches))
	}
	md, _ := os.ReadFile(filepath.Join(r.OutDir, "report.md"))
	if !strings.Contains(string(md), "Cleanup FAILED") {
		t.Fatal(string(md))
	}
}

func TestMeasureEvidenceRedaction(t *testing.T) {
	m := fullModel()
	m.transcriptTail = `{"type":"noise","api_key":"sk-ant-api03-SECRETSECRETSECRET","path":"/home/owner/.claude","env":"hunter2-secret-value","auth":"Bearer abcdefghijklmnop"}`
	m.decoder = "codex-jsonl"
	m.version = "codex-cli 0.156.1"
	p := planWith(Phases{})
	c := &p.Clients[0]
	c.ID, c.Decoder, c.DecoderFixture, c.Executable, c.ExpectedVersion, c.HelpArgv = "codex", "codex-jsonl", "codex-jsonl/synthetic", "/fake/codex", "codex-cli 0.156.1", []string{"--help"}
	r := newRunner(t, newWorld(t, m), p)
	rep := runPlan(t, r, context.Background())
	var all bytes.Buffer
	filepath.Walk(r.OutDir, func(p string, info os.FileInfo, err error) error {
		if err == nil && !info.IsDir() {
			b, _ := os.ReadFile(p)
			all.Write(b)
		}
		return nil
	})
	for _, secret := range []string{"SECRETSECRETSECRET", "hunter2-secret-value", "/home/owner", "abcdefghijklmnop", "sk-live-ABCDEFGHIJKLMNOPQRS"} {
		if bytes.Contains(all.Bytes(), []byte(secret)) {
			t.Fatalf("%q survived redaction", secret)
		}
	}
	if !bytes.Contains(all.Bytes(), []byte(Redacted)) || !bytes.Contains(all.Bytes(), []byte("<home>/.claude")) {
		t.Fatal("nothing was redacted")
	}
	// Hashes cover exactly the sanitized bytes.
	strictReport(t, r)
	if err := rep.CheckEvidence(r.OutDir); err != nil {
		t.Fatal(err)
	}
	if rep.ExitCode() != 0 {
		t.Fatalf("exit %d: %s", rep.ExitCode(), *rep.Clients[0].Reason)
	}
}

func TestMeasureTranscriptFaults(t *testing.T) {
	t.Run("truncated", func(t *testing.T) {
		r := newRunner(t, newWorld(t, fullModel()), planWith(Phases{}))
		r.StdoutLimit = 64
		rep := runPlan(t, r, context.Background())
		if cs := phase(t, rep, "claude", PhaseSetup).Cases[0]; *cs.Reason != ReasonTruncated {
			t.Fatalf("case %+v", cs)
		}
	})
	t.Run("extra-calls", func(t *testing.T) {
		m := fullModel()
		m.scenario = func(string) string { return scTwoCalls }
		rep := runPlan(t, newRunner(t, newWorld(t, m), planWith(Phases{})), context.Background())
		if cs := phase(t, rep, "claude", PhaseSetup).Cases[0]; *cs.Reason != ReasonExtraCalls {
			t.Fatalf("case %+v", cs)
		}
	})
	t.Run("tool-error", func(t *testing.T) {
		m := fullModel()
		m.scenario = func(string) string { return scToolError }
		rep := runPlan(t, newRunner(t, newWorld(t, m), planWith(Phases{})), context.Background())
		if cs := phase(t, rep, "claude", PhaseSetup).Cases[0]; *cs.Reason != ReasonToolError {
			t.Fatalf("case %+v", cs)
		}
	})
}

func TestMeasureOutDir(t *testing.T) {
	w := newWorld(t, fullModel())
	r := newRunner(t, w, fullPlan())
	r.OutDir = "relative/out"
	if _, err := r.Run(context.Background()); err == nil {
		t.Fatal("relative out dir accepted")
	}
	r.OutDir = t.TempDir()
	os.WriteFile(filepath.Join(r.OutDir, "old"), []byte("x"), 0o600)
	if _, err := r.Run(context.Background()); err == nil || !strings.Contains(err.Error(), "must be empty") {
		t.Fatalf("non-empty out dir: %v", err)
	}
	f := filepath.Join(t.TempDir(), "file")
	os.WriteFile(f, nil, 0o600)
	r.OutDir = f
	if _, err := r.Run(context.Background()); err == nil {
		t.Fatal("a file accepted as out dir")
	}
	if len(w.launches) != 0 {
		t.Fatal("launched before the out dir was usable")
	}
}
