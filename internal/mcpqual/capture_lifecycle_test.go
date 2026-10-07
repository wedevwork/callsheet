package mcpqual

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// Design decoder-enrollment UT-4: failure states on the injected world.
// No case infers a typed vendor event: an authentication or model failure
// is only an exit, a stderr and the probe's absence. Waits are armed on
// the fake clock's registered timers; both orders of completion versus
// cancellation and of deadline versus completion are fixed by the reap
// hook (the launch's select has taken its branch when Reap is called),
// never by a select between two ready channels.

func probeWith(evs ...func(cf CaseFile, id string) ProbeEvent) func(CaseFile, string) string {
	return func(cf CaseFile, id string) string {
		name, ver := "fake-cli", "1.0.0"
		all := []ProbeEvent{{Kind: EvStart}, {Kind: EvInitialize, ClientName: &name, ClientVersion: &ver}}
		for _, f := range evs {
			all = append(all, f(cf, id))
		}
		return probeLog(cf.RunID, all...)
	}
}

func ev(kind, suffix string) func(CaseFile, string) ProbeEvent {
	return func(_ CaseFile, id string) ProbeEvent {
		return ProbeEvent{Kind: kind, CaseID: id + suffix, RequestID: json.RawMessage(`1`)}
	}
}

func exitEv() func(CaseFile, string) ProbeEvent {
	return func(CaseFile, string) ProbeEvent { return ProbeEvent{Kind: EvExit, Reason: "eof"} }
}

func TestCaptureLifecycle(t *testing.T) {
	// The probe and session matrix on the pure classification: probe
	// observations only, never the transcript.
	zero, one, three := 0, 1, 3
	complete := probeWith(ev(EvReceipt, ""), ev(EvCompleted, ""), exitEv())
	cf := CaseFile{RunID: "run-cap-1"}
	const id = "claude-capture-setup"
	for name, tc := range map[string]struct {
		events string
		run    caseRun
		reason string
	}{
		"no-call":    {probeWith(exitEv())(cf, id), caseRun{exit: &zero}, ReasonProbeNotObserved},
		"no-probe":   {"", caseRun{exit: &one}, ReasonProbeNotObserved},
		"extra":      {probeWith(ev(EvReceipt, ""), ev(EvCompleted, ""), ev(EvReceipt, ""), exitEv())(cf, id), caseRun{exit: &zero}, ReasonProbeAnomaly + ": extra_receipt"},
		"marker":     {probeWith(ev(EvReceipt, "-marker"), ev(EvReceipt, ""), ev(EvCompleted, ""), exitEv())(cf, id), caseRun{exit: &zero}, ReasonProbeAnomaly + ": marker_receipt"},
		"other":      {probeWith(ev(EvReceipt, "-other"), exitEv())(cf, id), caseRun{exit: &zero}, ReasonProbeAnomaly + ": unexpected_receipt"},
		"rejected":   {probeWith(ev(EvReceipt, ""), ev(EvRejected, ""), ev(EvCompleted, ""), exitEv())(cf, id), caseRun{exit: &zero}, ReasonProbeAnomaly + ": rejected"},
		"fatal":      {probeWith(ev(EvReceipt, ""), ev(EvError, ""), ev(EvWriteFail, ""))(cf, id), caseRun{exit: &zero}, ReasonProbeAnomaly + ": fatal_error, write_failed"},
		"invalid":    {"not json\n", caseRun{exit: &zero}, ReasonProbeAnomaly + ": events_invalid"},
		"incomplete": {probeWith(ev(EvReceipt, ""), ev(EvCancelled, ""), exitEv())(cf, id), caseRun{exit: &zero}, ReasonProbeIncomplete},
		"not-intact": {probeWith(ev(EvReceipt, ""), ev(EvCompleted, ""))(cf, id), caseRun{exit: &zero}, ReasonProbeIncomplete},
		"exit-after": {complete(cf, id), caseRun{exit: &three}, ReasonSessionExit},
		"signal":     {complete(cf, id), caseRun{signal: sptr("killed")}, ReasonSessionExit},
		"empty":      {complete(cf, id), caseRun{exit: &zero}, ReasonSessionStdoutEmpty},
		"watchdog":   {complete(cf, id), caseRun{exit: &zero, watchdog: true}, ReasonSessionWatchdog},
		"budget":     {complete(cf, id), caseRun{exit: &zero, watchdog: true, budgetCapped: true}, ReasonSessionWatchdog + ": max_client_ms"},
		"interrupt":  {complete(cf, id), caseRun{exit: &zero, interrupted: true}, ReasonInterrupted},
		"cleanup":    {complete(cf, id), caseRun{exit: &zero, cleanup: CaseCleanup{Error: sptr("x")}}, ReasonCleanupFailed},
		"held":       {complete(cf, id), caseRun{exit: &zero, stdoutHeld: true}, ReasonStreamHeld + ": the session"},
		"cut-events": {complete(cf, id), caseRun{exit: &zero, probeCut: true}, ReasonEvidenceTruncated + ": the probe events"},
		"complete":   {complete(cf, id), caseRun{exit: &zero, transcript: Transcript{Lines: []Line{{Data: []byte("x")}}}}, ""},
	} {
		run := tc.run
		run.probeRaw = []byte(tc.events)
		cc := CaptureClient{Probe: observeProbe(run.probeRaw, run.probeCut, id)}
		if got := sessionReason(run, cc); got != tc.reason {
			t.Fatalf("%s: %q, want %q", name, got, tc.reason)
		}
	}
	ok := caseRun{exit: &zero, transcript: Transcript{Lines: []Line{{Data: []byte("x")}}}}
	ok.probeRaw = []byte(complete(cf, id))
	for name, s := range map[string]CaptureStream{"truncated": {OutputTruncated: true}, "omitted": {OmittedLines: 2}} {
		cc := CaptureClient{Probe: observeProbe(ok.probeRaw, false, id), Streams: []CaptureStream{s}}
		if got := sessionReason(ok, cc); !strings.HasPrefix(got, "evidence_"+name) {
			t.Fatalf("%s stream: %q", name, got)
		}
	}
	if p := observeProbe(ok.probeRaw, false, id); *p.ClientName != "fake-cli" || p.Receipts != 1 || !p.Completed || !p.Intact {
		t.Fatalf("probe %+v", p)
	}
	// Integration: an unauthenticated CLI or unknown model (exit 1, a
	// stderr, no probe) is only probe_not_observed, never a typed event;
	// a leader that exits before its group is reaped and proven; a launch
	// without a stderr pipe still completes.
	w := newCapWorld(t)
	sessions := map[string]capBehavior{
		"claude": {stdout: `{"type":"result","subtype":"error_authentication"}` + "\n", stderr: "Error: authentication required (run login)\n", exit: 1},
		"codex":  {stdout: capTranscript, events: goodEvents},
		"grok":   {stdout: capTranscript, plainProc: true, events: goodEvents},
	}
	w.script = func(spec ProcSpec) capBehavior {
		if launchKind(spec) != "session" {
			return w.defaults(spec)
		}
		return sessions[clientOf(spec)]
	}
	w.leaderFirst = func(spec ProcSpec) bool { return clientOf(spec) == "codex" && launchKind(spec) == "session" }
	man, _ := runCapture(t, newCapRunner(t, w, capPlan(t, "claude", "codex", "grok")), context.Background())
	raw, _ := json.Marshal(man)
	if len(w.sessions()) != 3 || strings.Contains(string(raw), KindAuthError) || strings.Contains(string(raw), SafeModelUnavailable) {
		t.Fatalf("%d sessions; a typed event was inferred: %s", len(w.sessions()), raw)
	}
	if cc := capClient(t, man, "claude"); reasonOf(cc) != ReasonProbeNotObserved || *cc.Session.Exit != 1 {
		t.Fatalf("auth: %q", reasonOf(cc))
	}
	if cc := capClient(t, man, "codex"); !cc.Session.Cleanup.LeaderExitedFirst || !cc.Session.Cleanup.KillSent || cc.State != CaptureComplete {
		t.Fatalf("leader first: %+v %q", cc.Session.Cleanup, reasonOf(cc))
	}
	if cc := capClient(t, man, "grok"); cc.State != CaptureComplete {
		t.Fatalf("plain proc: %q", reasonOf(cc))
	}
}

// approval blocked or never exiting: the session watchdog (armed first)
// ends it, reaped; never labeled an MCP timeout.
func TestCaptureWatchdogAndHeld(t *testing.T) {
	hang := func(w *capWorld, b capBehavior) {
		w.script = func(spec ProcSpec) capBehavior {
			if launchKind(spec) != "session" {
				return w.defaults(spec)
			}
			return b
		}
	}
	start := func(t *testing.T, w *capWorld, ctx context.Context, ids ...string) (*CaptureRunner, chan *CaptureManifest) {
		c := newCapRunner(t, w, capPlan(t, ids...))
		done := make(chan *CaptureManifest, 1)
		go func() {
			m, err := c.Run(ctx)
			if err != nil {
				t.Error(err)
			}
			done <- m
		}()
		return c, done
	}
	session := time.Duration(CaptureMaxSessionMS) * time.Millisecond
	// Watchdog: the session never exits.
	w := newCapWorld(t)
	hang(w, capBehavior{hang: true, events: probeWith(exitEv())})
	_, done := start(t, w, context.Background(), "cursor")
	awaitTimer(t, w.clock, session)
	w.clock.Advance(session)
	man := <-done
	cc := capClient(t, man, "cursor")
	if reasonOf(cc) != ReasonSessionWatchdog || !cc.Session.Watchdog || cc.Session.Signal == nil || !cc.Session.Cleanup.TermSent {
		t.Fatalf("watchdog: %q %+v", reasonOf(cc), cc.Session)
	}
	// A session's stdout and the next client's help stderr held open
	// after cleanup: each bounded by its post-cleanup wait (armed after
	// its reap), both read ends then closed; a held metadata stream stops
	// that client before its session.
	w = newCapWorld(t)
	w.script = func(spec ProcSpec) capBehavior {
		switch {
		case clientOf(spec) == "claude" && launchKind(spec) == "session":
			return capBehavior{stdout: capTranscript, holdStdout: true, events: goodEvents}
		case clientOf(spec) == "codex" && launchKind(spec) == "help":
			return capBehavior{stdout: "usage\n", holdStderr: true}
		}
		return w.defaults(spec)
	}
	_, done = start(t, w, context.Background(), "claude", "codex")
	w.awaitStart("session")
	awaitTimer(t, w.clock, 2*time.Second)
	w.clock.Advance(2 * time.Second)
	w.awaitStart("help")
	awaitTimer(t, w.clock, 2*time.Second)
	w.clock.Advance(2 * time.Second)
	man = <-done
	cc = capClient(t, man, "claude")
	if p := w.procs[2]; !strings.HasPrefix(reasonOf(cc), ReasonStreamHeld) || !cc.Session.StdoutHeld || cc.Session.StderrHeld || !p.closedOut || !p.closedErrOut {
		t.Fatalf("held stdout: %q %+v", reasonOf(cc), cc.Session)
	}
	if cc := capClient(t, man, "codex"); !strings.HasPrefix(reasonOf(cc), ReasonStreamHeld) || !cc.Help.StderrHeld || cc.Help.StdoutHeld || len(w.sessions()) != 1 {
		t.Fatalf("held help stderr: %q %+v", reasonOf(cc), cc.Help)
	}
	// Cancellation while the session runs: interrupted, the next client
	// is not started, exit 130.
	w = newCapWorld(t)
	hang(w, capBehavior{hang: true, events: probeWith(exitEv())})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	_, done = start(t, w, ctx, "claude", "codex")
	awaitTimer(t, w.clock, session)
	cancel()
	man = <-done
	if cc := capClient(t, man, "claude"); reasonOf(cc) != ReasonInterrupted || !cc.Session.Interrupted || man.State != CaptureInterrupted || man.ExitCode() != 130 ||
		capClient(t, man, "codex").State != CaptureNotRun || len(w.kinds()) != 3 {
		t.Fatalf("cancel: %q %s %v", reasonOf(cc), man.State, w.kinds())
	}
	// A cleanup failure is recorded, the capture cannot complete and the
	// next client is not started.
	w = newCapWorld(t)
	w.reapFail = func(pgid int) bool { return pgid == 3003 }
	_, done = start(t, w, context.Background(), "claude", "grok")
	man = <-done
	if cc := capClient(t, man, "claude"); reasonOf(cc) != ReasonCleanupFailed || man.Cleanup.OK || man.State != CapturePartial ||
		reasonOf(capClient(t, man, "grok")) != ReasonCleanupFailed || len(w.kinds()) != 3 || len(man.Cleanup.Failures) != 1 {
		t.Fatalf("cleanup failure: %q %+v %v", reasonOf(cc), man.Cleanup, w.kinds())
	}
}

// Both deterministic orders of completion and cancellation, and of
// completion and the deadline.
func TestCaptureEventOrders(t *testing.T) {
	session := time.Duration(CaptureMaxSessionMS) * time.Millisecond
	type outcome struct {
		watchdog, interrupted bool
		state                 string
	}
	run := func(t *testing.T, hang bool, during func(w *capWorld, cancel context.CancelFunc), after func(w *capWorld, cancel context.CancelFunc)) (CaptureClient, *CaptureManifest) {
		w := newCapWorld(t)
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		w.script = func(spec ProcSpec) capBehavior {
			if launchKind(spec) != "session" {
				return w.defaults(spec)
			}
			return capBehavior{stdout: capTranscript, hang: hang, events: goodEvents}
		}
		if during != nil {
			w.onReap = func(spec ProcSpec) {
				if launchKind(spec) == "session" {
					during(w, cancel)
				}
			}
		}
		c := newCapRunner(t, w, capPlan(t, "claude"))
		done := make(chan *CaptureManifest, 1)
		go func() { m, _ := c.Run(ctx); done <- m }()
		if after != nil {
			awaitTimer(t, w.clock, session)
			after(w, cancel)
		}
		m := <-done
		return capClient(t, m, "claude"), m
	}
	// Completion, then cancellation: the session completed (its select
	// took the exit); the run as a whole is interrupted.
	cc, m := run(t, false, func(_ *capWorld, cancel context.CancelFunc) { cancel() }, nil)
	if cc.Session.Interrupted || cc.State != CaptureComplete || m.State != CaptureInterrupted {
		t.Fatalf("completion then cancel: %+v %s", cc.Session, m.State)
	}
	// Cancellation, then completion: the session is interrupted.
	cc, m = run(t, true, nil, func(_ *capWorld, cancel context.CancelFunc) { cancel() })
	if !cc.Session.Interrupted || reasonOf(cc) != ReasonInterrupted || m.State != CaptureInterrupted {
		t.Fatalf("cancel then completion: %+v", cc.Session)
	}
	// Deadline, then completion: the watchdog.
	cc, _ = run(t, true, nil, func(w *capWorld, _ context.CancelFunc) { w.clock.Advance(session) })
	if !cc.Session.Watchdog || reasonOf(cc) != ReasonSessionWatchdog {
		t.Fatalf("deadline then completion: %+v", cc.Session)
	}
	// Completion, then the deadline passing during cleanup: complete.
	cc, m = run(t, false, func(w *capWorld, _ context.CancelFunc) { w.clock.Advance(2 * session) }, nil)
	if cc.Session.Watchdog || cc.State != CaptureComplete || m.State != CaptureComplete {
		t.Fatalf("completion then deadline: %+v %q", cc.Session, reasonOf(cc))
	}
}
