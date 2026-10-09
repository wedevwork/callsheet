package mcpqual

import (
	"bytes"
	"encoding/json"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"testing"
)

// Design decoder-enrollment B1.5 unit tests: UT-14 (the shared probe
// terminal observation), UT-15 (Cursor's per-project approval) and UT-16
// (the Cursor inventory exclusions), on injected execution facts, tiny raw
// logs (including the three retained 2026-10-08 server-events files as
// read-only vectors), tiny injected inventories and the capture world. No
// process starts and no real home is read.

const obsCase = "claude-capture-setup"

// obsLog renders events (seq, run ID and offsets filled) for the requested
// case's tests; ids give request IDs as raw JSON.
func obsEv(kind string, rawID string) ProbeEvent {
	ev := ProbeEvent{Kind: kind, CaseID: obsCase}
	if rawID != "" {
		ev.RequestID = json.RawMessage(rawID)
	}
	return ev
}

var (
	obsStart = ProbeEvent{Kind: EvStart}
	obsInit  = func() ProbeEvent {
		name, ver := "claude-code", "2.1.292"
		return ProbeEvent{Kind: EvInitialize, ClientName: &name, ClientVersion: &ver}
	}()
	obsExit    = ProbeEvent{Kind: EvExit, Reason: "eof"}
	obsIdleEOF = ProbeEvent{Kind: EvEOF}
)

// obsTerminal is a well-formed exit-less log ending at the requested call's
// terminal of kind.
func obsTerminal(kind string) string {
	return probeLog("r", obsStart, obsInit, obsEv(EvReceipt, `1`), obsEv(EvScheduled, `1`), obsEv(kind, `1`))
}

// vector reads one retained 2026-10-08 server-events file.
func vector(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", "probe-observation", name+"-server-events.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// UT-14: the shared analyzer on raw bytes. Only an LF-terminated log whose
// last record is the requested call's single correlated terminal, from a
// clean session, with one receipt after initialization and nothing else,
// is the terminal observation without exit; intact keeps its parser
// meaning; every other shape is incomplete.
func TestProbeObservationAnalyzer(t *testing.T) {
	completed := obsTerminal(EvCompleted)
	for name, tc := range map[string]struct {
		raw          string
		cut, clean   bool
		end          string
		intact       bool
		kind         string
		anomalyLabel string
	}{
		"intact":                {probeLog("r", obsStart, obsInit, obsEv(EvReceipt, `1`), obsEv(EvScheduled, `1`), obsEv(EvCompleted, `1`), obsIdleEOF, obsExit), false, true, EndIntact, true, EvCompleted, ""},
		"intact-unclean":        {probeLog("r", obsStart, obsInit, obsEv(EvReceipt, `1`), obsEv(EvCompleted, `1`), obsIdleEOF, obsExit), false, false, EndIntact, true, EvCompleted, ""},
		"completed":             {completed, false, true, EndTerminalWithoutExit, false, EvCompleted, ""},
		"cancelled":             {obsTerminal(EvCancelled), false, true, EndTerminalWithoutExit, false, EvCancelled, ""},
		"case-eof":              {obsTerminal(EvEOF), false, true, EndTerminalWithoutExit, false, EvEOF, ""},
		"unclean":               {completed, false, false, EndIncomplete, false, EvCompleted, ""},
		"idle-eof":              {probeLog("r", obsStart, obsInit, obsEv(EvReceipt, `1`), obsIdleEOF), false, true, EndIncomplete, false, "", ""},
		"no-lf":                 {strings.TrimSuffix(completed, "\n"), false, true, EndIncomplete, false, EvCompleted, ""},
		"torn":                  {completed + `{"seq":6,"ki`, false, true, EndIncomplete, false, "", "events_invalid"},
		"torn-json":             {completed + `{"seq":6,"kind":"eof","offset_ns":9000,"run_id":"r"}`, false, true, EndIncomplete, false, EvCompleted, ""},
		"trailing-blank":        {completed + "\n", false, true, EndIncomplete, false, EvCompleted, ""},
		"leading-blank":         {"\n" + completed, false, true, EndIncomplete, false, EvCompleted, ""},
		"inner-blank":           {strings.Replace(completed, "\n", "\n\n", 1), false, true, EndIncomplete, false, EvCompleted, ""},
		"crlf":                  {strings.TrimSuffix(completed, "\n") + "\r\n", false, true, EndIncomplete, false, EvCompleted, ""},
		"earlier-closed":        {probeLog("r", obsStart, obsInit, obsExit) + completed, false, true, EndTerminalWithoutExit, false, EvCompleted, ""},
		"earlier-unclosed":      {probeLog("r", obsStart, obsInit) + completed, false, true, EndIncomplete, false, EvCompleted, "instance_unclosed"},
		"earlier-receipt":       {probeLog("r", obsStart, obsInit, obsEv(EvReceipt, `1`), obsEv(EvCompleted, `1`), obsExit) + completed, false, true, EndIncomplete, false, "", "extra_receipt"},
		"other-case-receipt":    {probeLog("r", obsStart, obsInit, ProbeEvent{Kind: EvReceipt, CaseID: "other", RequestID: json.RawMessage(`7`)}, obsEv(EvReceipt, `1`), obsEv(EvCompleted, `1`)), false, true, EndIncomplete, false, EvCompleted, "unexpected_receipt"},
		"wrong-id":              {probeLog("r", obsStart, obsInit, obsEv(EvReceipt, `1`), obsEv(EvCompleted, `2`)), false, true, EndIncomplete, false, "", "completion_mismatch"},
		"string-id":             {probeLog("r", obsStart, obsInit, obsEv(EvReceipt, `1`), obsEv(EvCompleted, `"1"`)), false, true, EndIncomplete, false, "", "completion_mismatch"},
		"string-id-cancelled":   {probeLog("r", obsStart, obsInit, obsEv(EvReceipt, `1`), obsEv(EvCancelled, `"1"`)), false, true, EndIncomplete, false, "", ""},
		"missing-id":            {probeLog("r", obsStart, obsInit, obsEv(EvReceipt, ""), obsEv(EvCancelled, "")), false, true, EndIncomplete, false, "", ""},
		"padded-id":             {probeLog("r", obsStart, obsInit, obsEv(EvReceipt, ` 1`), obsEv(EvCancelled, `1 `)), false, true, EndTerminalWithoutExit, false, EvCancelled, ""},
		"wrong-case":            {probeLog("r", obsStart, obsInit, obsEv(EvReceipt, `1`), ProbeEvent{Kind: EvCancelled, CaseID: "Claude-capture-setup", RequestID: json.RawMessage(`1`)}), false, true, EndIncomplete, false, "", ""},
		"wrong-case-completion": {probeLog("r", obsStart, obsInit, obsEv(EvReceipt, `1`), ProbeEvent{Kind: EvCompleted, CaseID: "other", RequestID: json.RawMessage(`1`)}), false, true, EndIncomplete, false, "", "unexpected_completion"},
		"duplicate-receipt":     {probeLog("r", obsStart, obsInit, obsEv(EvReceipt, `1`), obsEv(EvReceipt, `1`), obsEv(EvCompleted, `1`)), false, true, EndIncomplete, false, "", "extra_receipt"},
		"duplicate-terminal":    {probeLog("r", obsStart, obsInit, obsEv(EvReceipt, `1`), obsEv(EvScheduled, `1`), obsEv(EvCompleted, `1`), obsEv(EvCompleted, `1`)), false, true, EndIncomplete, false, "", "duplicate_completion"},
		"contradictory":         {probeLog("r", obsStart, obsInit, obsEv(EvReceipt, `1`), obsEv(EvCompleted, `1`), obsEv(EvCancelled, `1`)), false, true, EndIncomplete, false, "", ""},
		"rejected":              {probeLog("r", obsStart, obsInit, obsEv(EvReceipt, `1`), obsEv(EvRejected, `2`), obsEv(EvCompleted, `1`)), false, true, EndIncomplete, false, EvCompleted, "rejected"},
		"error":                 {probeLog("r", obsStart, obsInit, obsEv(EvReceipt, `1`), ProbeEvent{Kind: EvError, Reason: "x"}, obsEv(EvCancelled, `1`)), false, true, EndIncomplete, false, EvCancelled, "fatal_error"},
		"write-failed":          {probeLog("r", obsStart, obsInit, obsEv(EvReceipt, `1`), obsEv(EvWriteFail, `1`), obsEv(EvCompleted, `1`)), false, true, EndIncomplete, false, EvCompleted, "write_failed"},
		"no-initialize":         {probeLog("r", obsStart, obsEv(EvReceipt, `1`), obsEv(EvCompleted, `1`)), false, true, EndIncomplete, false, EvCompleted, "receipt_without_initialize"},
		"late-cancel":           {probeLog("r", obsStart, obsInit, obsEv(EvReceipt, `1`), obsEv(EvCompleted, `1`), obsEv(EvLateCancel, `1`)), false, true, EndIncomplete, false, EvCompleted, ""},
		"late-progress":         {probeLog("r", obsStart, obsInit, obsEv(EvReceipt, `1`), obsEv(EvCancelled, `1`), obsEv(EvProgress, `1`)), false, true, EndIncomplete, false, EvCancelled, ""},
		"progress":              {probeLog("r", obsStart, obsInit, obsEv(EvReceipt, `1`), obsEv(EvProgress, `1`), obsEv(EvScheduled, `1`), obsEv(EvCompleted, `1`)), false, true, EndTerminalWithoutExit, false, EvCompleted, ""},
		"progress-other-id":     {probeLog("r", obsStart, obsInit, obsEv(EvReceipt, `1`), obsEv(EvProgress, `2`), obsEv(EvCompleted, `1`)), false, true, EndIncomplete, false, EvCompleted, ""},
		"progress-before":       {probeLog("r", obsStart, obsInit, obsEv(EvProgress, `1`), obsEv(EvReceipt, `1`), obsEv(EvCompleted, `1`)), false, true, EndIncomplete, false, EvCompleted, ""},
		"completion-first":      {probeLog("r", obsStart, obsInit, obsEv(EvCompleted, `1`), obsEv(EvReceipt, `1`), obsEv(EvCancelled, `1`)), false, true, EndIncomplete, false, "", "completion_without_receipt"},
		"after-exit":            {probeLog("r", obsStart, obsInit, obsEv(EvReceipt, `1`), obsExit, obsEv(EvCompleted, `1`)), false, true, EndIncomplete, false, EvCompleted, "event_after_exit"},
		"other-run":             {probeLog("r", obsStart, obsInit, obsEv(EvReceipt, `1`)) + `{"seq":4,"kind":"cancelled","offset_ns":3000,"run_id":"other","case_id":"claude-capture-setup","request_id":1}` + "\n", false, true, EndIncomplete, false, "", "instance_run_mismatch"},
		// Code review B1.5 round 1, C2: the start's run binds every record.
		"different-instance": {probeLog("r", obsStart, obsInit) + strings.TrimPrefix(probeLog("different-instance", ProbeEvent{Kind: EvStart}, obsInit, obsEv(EvReceipt, `1`), obsEv(EvScheduled, `1`), obsEv(EvCompleted, `1`)),
			probeLog("different-instance", ProbeEvent{Kind: EvStart}, obsInit)), false, true, EndIncomplete, false, EvCompleted, "instance_run_mismatch"},
		"other-run-initialize": {probeLog("r", obsStart) + strings.TrimPrefix(probeLog("x", obsStart, obsInit), probeLog("x", obsStart)) +
			strings.TrimPrefix(probeLog("r", obsStart, obsInit, obsEv(EvReceipt, `1`), obsEv(EvCompleted, `1`)), probeLog("r", obsStart, obsInit)), false, true, EndIncomplete, false, EvCompleted, "instance_run_mismatch"},
		"second-initialize": {probeLog("r", obsStart, obsInit, obsInit, obsEv(EvReceipt, `1`), obsEv(EvCompleted, `1`)), false, true, EndTerminalWithoutExit, false, EvCompleted, ""},
		"cut":               {completed, true, true, EndIncomplete, false, "", "events_truncated"},
		"empty":             {"", false, true, EndIncomplete, false, "", ""},
		"invalid":           {"not json\n", false, true, EndIncomplete, false, "", "events_invalid"},
	} {
		a := analyzeProbe([]byte(tc.raw), obsCase, tc.cut, tc.clean)
		o := a.obs
		kind := ""
		if o.TerminalKind != nil {
			kind = *o.TerminalKind
		}
		if o.EndState != tc.end || o.Intact != tc.intact || kind != tc.kind || o.CleanSession != tc.clean || a.capture.Intact != o.Intact {
			t.Fatalf("%s: %+v (kind %q) anomalies %v", name, o, kind, a.capture.Anomalies)
		}
		if (tc.anomalyLabel != "") != (len(a.capture.Anomalies) > 0) || tc.anomalyLabel != "" && !slices.Contains(a.capture.Anomalies, tc.anomalyLabel) {
			t.Fatalf("%s: anomalies %v, want %q", name, a.capture.Anomalies, tc.anomalyLabel)
		}
		if err := o.validate(); err != nil {
			t.Fatalf("%s: the analyzer produced an invalid observation: %v", name, err)
		}
	}
	// The retained 2026-10-08 logs (Codex, Grok, Claude-2): each a clean
	// session's terminal completion without exit; the same bytes from an
	// unclean session stay incomplete.
	for _, name := range []string{"codex", "grok", "claude-2"} {
		raw := vector(t, name)
		id := strings.TrimSuffix(name, "-2") + "-capture-setup"
		a := analyzeProbe(raw, id, false, true)
		if a.obs.EndState != EndTerminalWithoutExit || a.obs.Intact || *a.obs.TerminalKind != EvCompleted || !a.capture.Completed || a.capture.Receipts != 1 || len(a.capture.Anomalies) > 0 {
			t.Fatalf("%s: %+v %+v", name, a.obs, a.capture)
		}
		if a := analyzeProbe(raw, id, false, false); a.obs.EndState != EndIncomplete {
			t.Fatalf("%s unclean: %+v", name, a.obs)
		}
		if _, intact, err := ParseProbeEvents(raw); err != nil || intact {
			t.Fatalf("%s: ParseProbeEvents changed: %v %v", name, intact, err)
		}
	}
	// Labels: the new state never says intact, killed or terminated.
	for _, o := range []*ProbeObservation{nil, {EndState: EndIntact}, {EndState: EndTerminalWithoutExit}, {EndState: EndIncomplete}} {
		l := o.Label()
		if o != nil && o.EndState == EndTerminalWithoutExit && (l != "terminal observed; probe exit not observed" || strings.Contains(l, "intact") || strings.Contains(l, "kill")) {
			t.Fatalf("label %q", l)
		}
	}
	if !strings.Contains((*ProbeObservation)(nil).Label(), "not analyzed") || observationText(&ProbeObservation{EndState: EndIncomplete}) != "end_state=incomplete intact=false terminal_kind=null clean_session=false" {
		t.Fatal("labels")
	}
}

// UT-14: the clean-session attestation from injected execution facts. Every
// fact withdraws it except a TERM or KILL of descendants after the leader
// exited 0 with the group proven gone.
func TestCleanSessionFacts(t *testing.T) {
	zero, one := 0, 1
	ok := caseRun{exit: &zero, cleanup: CaseCleanup{GroupGone: true}}
	if !ok.cleanRun() {
		t.Fatal("a clean run is not clean")
	}
	for name, tc := range map[string]struct {
		mutate func(r *caseRun)
		clean  bool
	}{
		"term-sent":     {func(r *caseRun) { r.cleanup.TermSent, r.cleanup.LeaderExitedFirst = true, true }, true},
		"kill-sent":     {func(r *caseRun) { r.cleanup.KillSent, r.cleanup.LeaderExitedFirst = true, true }, true},
		"exit-nonzero":  {func(r *caseRun) { r.exit = &one }, false},
		"no-exit":       {func(r *caseRun) { r.exit = nil }, false},
		"signal":        {func(r *caseRun) { r.signal = sptr("killed") }, false},
		"watchdog":      {func(r *caseRun) { r.watchdog = true }, false},
		"interrupted":   {func(r *caseRun) { r.interrupted = true }, false},
		"stdout-held":   {func(r *caseRun) { r.stdoutHeld = true }, false},
		"stderr-held":   {func(r *caseRun) { r.stderrHeld = true }, false},
		"cleanup-error": {func(r *caseRun) { r.cleanup.Error = sptr("x") }, false},
		"group-alive":   {func(r *caseRun) { r.cleanup.GroupGone = false }, false},
		"probe-cut":     {func(r *caseRun) { r.probeCut = true }, false},
		"server-cut":    {func(r *caseRun) { r.serverCut = true }, false},
		"evidence-cut":  {func(r *caseRun) { r.evidenceCut = true }, false},
		"transcript":    {func(r *caseRun) { r.transcript.Truncated = true }, false},
		"stderr-cut":    {func(r *caseRun) { r.stderrCut = true }, false},
		"omitted":       {func(r *caseRun) { r.omitted = 1 }, false},
		"write-error":   {func(r *caseRun) { r.writeErr = os.ErrClosed }, false},
		"launch-error":  {func(r *caseRun) { r.launchErr = os.ErrNotExist }, false},
	} {
		r := ok
		tc.mutate(&r)
		if r.cleanRun() != tc.clean {
			t.Fatalf("%s: clean %v", name, !tc.clean)
		}
	}
	// Capture's attestation is the recorded stage and the session's three
	// streams: term_sent/kill_sent with the group gone are clean; any
	// session stream cut or omission, a signal or an unproven cleanup is not;
	// another stream (help) never is the session's.
	stage := CaptureStage{State: StageRan, Exit: &zero, Cleanup: &CaseCleanup{TermSent: true, KillSent: true, LeaderExitedFirst: true, GroupGone: true}}
	streams := []CaptureStream{{Path: ClientFile("claude", FileServerEvents)}, {Path: ClientFile("claude", FileHelpStdout), OmittedLines: 3}}
	if !captureSessionClean("claude", stage, streams) {
		t.Fatal("descendant cleanup after exit 0 is not clean")
	}
	for name, s := range map[string]CaptureStream{"omitted": {OmittedLines: 1}, "input": {InputTruncated: true}, "output": {OutputTruncated: true}} {
		for _, f := range sessionStreamFiles {
			s.Path = ClientFile("claude", f)
			if captureSessionClean("claude", stage, []CaptureStream{s}) {
				t.Fatalf("%s %s: clean", name, f)
			}
		}
	}
	bad := stage
	bad.Signal = sptr("killed")
	gone := stage
	gone.Cleanup = &CaseCleanup{GroupGone: false}
	if captureSessionClean("claude", bad, nil) || captureSessionClean("claude", gone, nil) || captureSessionClean("claude", CaptureStage{State: StageRan, Exit: &zero}, nil) {
		t.Fatal("an unclean stage is clean")
	}
}

// obsDecoded is a decoder's typed success (or timeout) for the case.
func obsDecoded(kind, nonce string) Decoded {
	d := Decoded{Terminal: true, Events: []Event{{CaseID: obsCase, Kind: KindToolCall}}}
	switch kind {
	case KindToolResult:
		d.Events = append(d.Events, Event{CaseID: obsCase, Kind: KindToolResult, Nonce: nonce})
	case KindMCPTimeout:
		d.Events = append(d.Events, Event{CaseID: obsCase, Kind: KindMCPTimeout})
	}
	return d
}

// UT-14: capture and qualify decide from the same observation: a complete
// capture exactly where qualify accepts the decoder's nonce success, never
// a timeout from the probe log alone, the nonce still required, the intact
// endpoint-missing behavior kept and no fallback past a watchdog or a
// nonzero exit; a typed timeout is conclusive only from a clean session.
func TestProbeObservationParity(t *testing.T) {
	zero, one := 0, 1
	clean := caseRun{exit: &zero, cleanup: CaseCleanup{GroupGone: true}, transcript: Transcript{Lines: []Line{{Data: []byte("x")}}}}
	nonzero := clean
	nonzero.exit = &one
	watchdog := clean
	watchdog.watchdog = true
	intact := probeLog("r", obsStart, obsInit, obsEv(EvReceipt, `1`), obsEv(EvCompleted, `1`), obsIdleEOF, obsExit)
	for name, tc := range map[string]struct {
		raw     string
		run     caseRun
		capture string // capture's reason ("" complete)
		qualify string // classify's outcome for a nonce success
	}{
		"intact":       {intact, clean, "", KindToolResult},
		"exitless":     {obsTerminal(EvCompleted), clean, "", KindToolResult},
		"no-lf":        {strings.TrimSuffix(obsTerminal(EvCompleted), "\n"), clean, ReasonProbeIncomplete, OutcomeInconclusive},
		"cancelled":    {obsTerminal(EvCancelled), clean, ReasonProbeIncomplete, OutcomeInconclusive},
		"nonzero":      {obsTerminal(EvCompleted), nonzero, ReasonProbeIncomplete, OutcomeInconclusive},
		"watchdog":     {obsTerminal(EvCompleted), watchdog, ReasonSessionWatchdog, OutcomeInconclusive},
		"intact-nz":    {intact, nonzero, ReasonSessionExit, KindToolResult},
		"contradicted": {probeLog("r", obsStart, obsInit, obsEv(EvReceipt, `1`), obsEv(EvCompleted, `1`), obsEv(EvCancelled, `1`), obsIdleEOF, obsExit), clean, ReasonProbeIncomplete, OutcomeInconclusive},
		// Code review B1.5 round 1, C1: an intact log with an anomaly is not
		// an accepted end state in qualify either.
		"wrong-typed-id": {probeLog("r", obsStart, obsInit, obsEv(EvReceipt, `1`), obsEv(EvCompleted, `"1"`), obsIdleEOF, obsExit), clean,
			ReasonProbeAnomaly + ": completion_mismatch", OutcomeInconclusive},
		"duplicate-receipts": {probeLog("r", obsStart, obsInit, obsEv(EvReceipt, `1`), obsEv(EvReceipt, `1`), obsEv(EvCompleted, `1`), obsIdleEOF, obsExit), clean,
			ReasonProbeAnomaly + ": extra_receipt", OutcomeInconclusive},
		"no-initialize": {probeLog("r", obsStart, obsEv(EvReceipt, `1`), obsEv(EvCompleted, `1`), obsIdleEOF, obsExit), clean,
			ReasonProbeAnomaly + ": receipt_without_initialize", OutcomeInconclusive},
		"other-run": {probeLog("r", obsStart, obsInit) + strings.TrimPrefix(probeLog("x", obsStart, obsInit, obsEv(EvReceipt, `1`), obsEv(EvCompleted, `1`), obsIdleEOF, obsExit),
			probeLog("x", obsStart, obsInit)), clean, ReasonProbeAnomaly + ": instance_run_mismatch", OutcomeInconclusive},
	} {
		run := tc.run
		run.probeRaw = []byte(tc.raw)
		a := analyzeProbe(run.probeRaw, obsCase, false, run.cleanRun())
		cc := CaptureClient{Probe: a.capture}
		cc.Probe.Observation = &a.obs
		if got := sessionReason(run, cc); got != tc.capture {
			t.Fatalf("%s capture: %q, want %q", name, got, tc.capture)
		}
		cs := CaseReport{CaseID: obsCase}
		classify(&cs, "n1", obsDecoded(KindToolResult, "n1"), run)
		if cs.Outcome != tc.qualify || !cs.ProbeObservation.equal(&a.obs) {
			t.Fatalf("%s qualify: %s %s %+v", name, cs.Outcome, deref(cs.Reason), cs.ProbeObservation)
		}
		if cs.Outcome == OutcomeInconclusive && name != "watchdog" && deref(cs.Reason) != ReasonProbeIncomplete && deref(cs.Reason) != ReasonProbeEndpoint {
			t.Fatalf("%s qualify reason %s", name, deref(cs.Reason))
		}
		// The report validator refuses a conclusive outcome the shared
		// observation does not accept, for either outcome kind.
		for _, outcome := range []string{KindToolResult, KindMCPTimeout} {
			forged := cs
			forged.Outcome, forged.Exit, forged.Signal, forged.Cleanup = outcome, run.exit, run.signal, run.cleanup
			accepted := a.obs.observedEndpoint() && (outcome == KindMCPTimeout || a.obs.TerminalKind != nil && *a.obs.TerminalKind == EvCompleted)
			if err := forged.validateObservation(); (err == nil) != (accepted && (outcome != KindMCPTimeout || a.obs.CleanSession)) {
				t.Fatalf("%s report %s: %v", name, outcome, err)
			}
		}
	}
	// The nonce: the observation is never a substitute for the result.
	run := clean
	run.probeRaw = []byte(obsTerminal(EvCompleted))
	cs := CaseReport{CaseID: obsCase}
	classify(&cs, "n1", obsDecoded(KindToolResult, "other"), run)
	if cs.Outcome != OutcomeInconclusive || deref(cs.Reason) != ReasonNonce {
		t.Fatalf("nonce: %s %s", cs.Outcome, deref(cs.Reason))
	}
	// No timeout from the probe: a cancelled endpoint with no typed result.
	run.probeRaw = []byte(obsTerminal(EvCancelled))
	cs = CaseReport{CaseID: obsCase}
	classify(&cs, "n1", Decoded{Terminal: true, Events: []Event{{CaseID: obsCase, Kind: KindToolCall}}}, run)
	if cs.Outcome == KindMCPTimeout || deref(cs.Reason) != ReasonNoOutcome {
		t.Fatalf("no typed timeout: %s %s", cs.Outcome, deref(cs.Reason))
	}
	// A typed timeout correlates with the clean exit-less cancelled
	// endpoint, its elapsed time from it; unclean it is inconclusive
	// probe_incomplete, intact or not (DW1).
	for name, tc := range map[string]struct {
		raw     string
		run     caseRun
		outcome string
		reason  string
	}{
		"exitless-clean":   {obsTerminal(EvCancelled), clean, KindMCPTimeout, ""},
		"exitless-nonzero": {obsTerminal(EvCancelled), nonzero, OutcomeInconclusive, ReasonProbeIncomplete},
		"intact-nonzero":   {probeLog("r", obsStart, obsInit, obsEv(EvReceipt, `1`), obsEv(EvCancelled, `1`), obsIdleEOF, obsExit), nonzero, OutcomeInconclusive, ReasonProbeIncomplete},
		"intact-clean":     {probeLog("r", obsStart, obsInit, obsEv(EvReceipt, `1`), obsEv(EvCancelled, `1`), obsIdleEOF, obsExit), clean, KindMCPTimeout, ""},
		"endpoint-missing": {probeLog("r", obsStart, obsInit, obsEv(EvReceipt, `1`), obsIdleEOF, obsExit), clean, KindMCPTimeout, ReasonProbeEndpoint},
		"exitless-no-lf":   {strings.TrimSuffix(obsTerminal(EvCancelled), "\n"), clean, OutcomeInconclusive, ReasonProbeIncomplete},
		"watchdog":         {obsTerminal(EvCancelled), watchdog, OutcomeInconclusive, ReasonProbeIncomplete},
	} {
		run := tc.run
		run.probeRaw = []byte(tc.raw)
		cs := CaseReport{CaseID: obsCase}
		classify(&cs, "n1", obsDecoded(KindMCPTimeout, ""), run)
		if cs.Outcome != tc.outcome || deref(cs.Reason) != tc.reason {
			t.Fatalf("%s: %s %q", name, cs.Outcome, deref(cs.Reason))
		}
		if tc.outcome == KindMCPTimeout && tc.reason == "" && cs.ElapsedMS == nil {
			t.Fatalf("%s: no elapsed time", name)
		}
		// The report validator accepts exactly what the runner records.
		cs.Exit, cs.Cleanup = run.exit, run.cleanup
		if err := cs.validateObservation(); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
	}
	// A cut probe log (the read bound or the evidence bound) is never
	// parsed: no endpoints.
	run = clean
	run.probeRaw, run.serverCut = []byte(obsTerminal(EvCompleted)), true
	cs = CaseReport{CaseID: obsCase}
	classify(&cs, "n1", obsDecoded(KindToolResult, "n1"), run)
	if cs.StartOffsetNS != nil || cs.ProbeObservation.EndState != EndIncomplete || cs.ProbeObservation.CleanSession {
		t.Fatalf("cut: %+v", cs)
	}
}

// UT-14: the observation's schema in the manifest and the report: strict
// members, enums and nulls, legacy absence, consistency with the record,
// and replay against the retained bytes; a forged label never passes.
func TestProbeObservationSchema(t *testing.T) {
	w := newCapWorld(t)
	w.session(func(spec ProcSpec) capBehavior {
		return capBehavior{stdout: capTranscript, events: func(cf CaseFile, id string) string {
			name, ver := "fake-cli", "1.0.0"
			return probeLog(cf.RunID, ProbeEvent{Kind: EvStart}, ProbeEvent{Kind: EvInitialize, ClientName: &name, ClientVersion: &ver},
				ProbeEvent{Kind: EvReceipt, CaseID: id, RequestID: json.RawMessage(`1`)}, ProbeEvent{Kind: EvCompleted, CaseID: id, RequestID: json.RawMessage(`1`)})
		}}
	})
	man, b, c := b1Capture(t, w, capPlan(t, "claude"), nil)
	cc := capClient(t, man, "claude")
	if cc.State != CaptureComplete || cc.Probe.Observation == nil || cc.Probe.Observation.EndState != EndTerminalWithoutExit {
		t.Fatalf("exit-less capture %s %+v", reasonOf(cc), cc.Probe)
	}
	// Round trip.
	if m, err := ParseCaptureManifest(b.ManifestBytes); err != nil || !m.Clients[0].Probe.Observation.equal(cc.Probe.Observation) {
		t.Fatalf("round trip: %v", err)
	}
	edit := func(f func(o map[string]any, p map[string]any, c map[string]any)) []byte {
		var m map[string]any
		json.Unmarshal(b.ManifestBytes, &m)
		cl := m["clients"].([]any)[0].(map[string]any)
		p := cl["probe"].(map[string]any)
		o, _ := p["observation"].(map[string]any)
		f(o, p, cl)
		out, _ := json.Marshal(m)
		return out
	}
	for name, f := range map[string]func(o, p, c map[string]any){
		"null":            func(_, p, _ map[string]any) { p["observation"] = nil },
		"array":           func(_, p, _ map[string]any) { p["observation"] = []any{} },
		"extra":           func(o, _, _ map[string]any) { o["cause"] = "x" },
		"missing":         func(o, _, _ map[string]any) { delete(o, "intact") },
		"null-end":        func(o, _, _ map[string]any) { o["end_state"] = nil },
		"null-intact":     func(o, _, _ map[string]any) { o["intact"] = nil },
		"null-clean":      func(o, _, _ map[string]any) { o["clean_session"] = nil },
		"enum":            func(o, _, _ map[string]any) { o["end_state"] = "terminated" },
		"kind-enum":       func(o, _, _ map[string]any) { o["terminal_kind"] = "exit" },
		"null-kind":       func(o, _, _ map[string]any) { o["terminal_kind"] = nil },
		"intact-end":      func(o, _, _ map[string]any) { o["end_state"] = EndIntact },
		"intact-mismatch": func(o, p, _ map[string]any) { o["intact"] = true },
		"intact-both":     func(o, p, _ map[string]any) { o["intact"], p["intact"] = true, true },
		"unclean":         func(o, _, _ map[string]any) { o["clean_session"] = false },
		"incomplete":      func(o, _, _ map[string]any) { o["end_state"] = EndIncomplete },
		"anomaly":         func(_, p, _ map[string]any) { p["anomalies"] = []any{"extra_receipt"} },
		"receipts":        func(_, p, _ map[string]any) { p["receipts"] = 2 },
		"legacy-claim":    func(_, p, _ map[string]any) { delete(p, "observation") },
		"exit-nonzero":    func(_, _, c map[string]any) { c["session"].(map[string]any)["exit"] = 2 },
		"duplicate":       nil,
	} {
		var raw []byte
		if f == nil {
			raw = bytes.Replace(b.ManifestBytes, []byte(`"end_state": "terminal_observed_without_exit"`), []byte(`"end_state": "incomplete", "end_state": "terminal_observed_without_exit"`), 1)
		} else {
			raw = edit(f)
		}
		if _, err := ParseCaptureManifest(raw); err == nil {
			t.Fatalf("%s: accepted", name)
		}
	}
	// A manifest consistent with itself but not with the retained bytes:
	// the replay refuses it. A legacy record (no observation) is never
	// replayed and keeps its old rules.
	if err := replayCaptureObservation(&CaptureClient{ID: "claude"}, nil); err != nil {
		t.Fatal(err)
	}
	c2 := cc
	p2 := *cc.Probe
	c2.Probe = &p2
	if err := replayCaptureObservation(&c2, b.Files); err != nil {
		t.Fatalf("replay: %v", err)
	}
	files := map[string][]byte{}
	for k, v := range b.Files {
		files[k] = v
	}
	files[ClientFile("claude", FileServerEvents)] = append(append([]byte(nil), files[ClientFile("claude", FileServerEvents)]...), []byte(`{"seq":5,"kind":"exit","offset_ns":9000,"run_id":"run-cap-1","reason":"eof"}`+"\n")...)
	if err := replayCaptureObservation(&c2, files); err == nil || !strings.Contains(err.Error(), "probe replay") {
		t.Fatalf("a replay mismatch passed: %v", err)
	}
	// Output-cut retained bytes cannot carry the observation or a clean
	// session.
	c3 := c2
	c3.Streams = append([]CaptureStream(nil), c2.Streams...)
	for i := range c3.Streams {
		if c3.Streams[i].Path == ClientFile("claude", FileServerEvents) {
			c3.Streams[i].OutputTruncated = true
		}
	}
	if err := replayCaptureObservation(&c3, b.Files); err == nil {
		t.Fatal("an output-cut special observation passed")
	}
	o3 := ProbeObservation{EndState: EndIncomplete, CleanSession: false}
	p3 := *c2.Probe
	p3.Observation = &o3
	c3.Probe = &p3
	if err := replayCaptureObservation(&c3, b.Files); err != nil {
		t.Fatalf("an output-cut incomplete observation: %v", err)
	}
	// Enrollment replay: the bundle's own record is required; a legacy
	// record must be intact; a mismatched one fails.
	o := &ExpectedOracle{CaseID: cc.CaseID, ClientInfo: ExpectedClientInfo{Name: "fake-cli", Version: "1.0.0"}}
	for name, bundle := range map[string]*CaptureBundle{
		"no-manifest": {Files: b.Files},
		"other-client": func() *CaptureBundle {
			m := *man
			m.Clients = []CaptureClient{{ID: "codex"}}
			return &CaptureBundle{Manifest: &m, Files: b.Files}
		}(),
		"legacy": func() *CaptureBundle {
			m := *man
			cl := cc
			p := *cc.Probe
			p.Observation = nil
			cl.Probe = &p
			m.Clients = []CaptureClient{cl}
			return &CaptureBundle{Manifest: &m, Files: b.Files}
		}(),
		"mismatch": {Manifest: man, Files: files},
	} {
		if err := replayProbe(bundle, "claude", o); err == nil {
			t.Fatalf("%s: replay passed", name)
		}
	}
	if err := replayProbe(b, "claude", o); err != nil {
		t.Fatalf("replay: %v", err)
	}
	_ = c
}

// UT-14: the report side: the optional case observation's strict members,
// its consistency with the recorded exit, signal, cleanup and outcome, and
// the replay of its structure and endpoints from the retained bytes.
func TestCaseObservationReport(t *testing.T) {
	zero, one := 0, 1
	special := ProbeObservation{EndState: EndTerminalWithoutExit, TerminalKind: sptr(EvCompleted), CleanSession: true}
	base := CaseReport{CaseID: obsCase, Outcome: KindToolResult, Exit: &zero, Cleanup: CaseCleanup{GroupGone: true, TermSent: true, KillSent: true}, ProbeObservation: &special}
	if err := base.validateObservation(); err != nil {
		t.Fatal(err)
	}
	for name, mutate := range map[string]func(c *CaseReport){
		"exit":    func(c *CaseReport) { c.Exit = &one },
		"no-exit": func(c *CaseReport) { c.Exit = nil },
		"signal":  func(c *CaseReport) { c.Signal = sptr("killed") },
		"cleanup": func(c *CaseReport) { c.Cleanup.Error = sptr("x") },
		"group":   func(c *CaseReport) { c.Cleanup.GroupGone = false },
		"incomplete": func(c *CaseReport) {
			o := special
			o.EndState, o.CleanSession = EndIncomplete, false
			c.ProbeObservation, c.Exit = &o, &one
		},
		"timeout": func(c *CaseReport) {
			o := special
			o.EndState, o.CleanSession = EndIncomplete, false
			c.ProbeObservation, c.Outcome = &o, KindMCPTimeout
		},
		"intact-dirty": func(c *CaseReport) {
			o := ProbeObservation{EndState: EndIntact, Intact: true, TerminalKind: sptr(EvCancelled)}
			c.ProbeObservation, c.Outcome = &o, KindMCPTimeout
		},
		"enum": func(c *CaseReport) { o := special; o.EndState = "x"; c.ProbeObservation = &o },
	} {
		c := base
		mutate(&c)
		if err := c.validateObservation(); err == nil {
			t.Fatalf("%s: accepted", name)
		}
	}
	legacy := base
	legacy.ProbeObservation = nil
	if err := legacy.validateObservation(); err != nil || legacy.replayObservation(nil) != nil {
		t.Fatal("a legacy case was checked")
	}
	// Replay from retained bytes: structure and endpoints.
	raw := []byte(obsTerminal(EvCompleted))
	an := analyzeProbe(raw, obsCase, false, true)
	rec := base
	rec.ServerEvents = sptr("cases/x/server-events.jsonl")
	rec.ProgressTokenPresent = an.view.receipt.TokenPresent
	rec.StartOffsetNS, rec.EndOffsetNS = iptr(an.view.receipt.OffsetNS), iptr(an.view.end.OffsetNS)
	rec.ElapsedMS = iptr(0)
	files := map[string][]byte{*rec.ServerEvents: raw}
	if err := rec.replayObservation(files); err != nil {
		t.Fatal(err)
	}
	for name, mutate := range map[string]func(c *CaseReport){
		"start":    func(c *CaseReport) { c.StartOffsetNS = iptr(1) },
		"end":      func(c *CaseReport) { c.EndOffsetNS = nil },
		"elapsed":  func(c *CaseReport) { c.ElapsedMS = iptr(5) },
		"progress": func(c *CaseReport) { c.ProgressSent = 1 },
		"token":    func(c *CaseReport) { c.ProgressTokenPresent = bptr(true) },
		"kind":     func(c *CaseReport) { o := special; o.TerminalKind = sptr(EvCancelled); c.ProbeObservation = &o },
		"no-file":  func(c *CaseReport) { c.ServerEvents = nil },
	} {
		c := rec
		mutate(&c)
		if err := c.replayObservation(files); err == nil {
			t.Fatalf("%s: replay passed", name)
		}
	}
	// The raw schema of a report case.
	r := sampleReport(t)
	b, _ := json.Marshal(r)
	if _, err := ParseReport(b); err != nil {
		t.Fatal(err)
	}
	for name, edit := range map[string][2]string{
		"null":    {`"probe_observation":{`, `"probe_observation":null,"pad":{`},
		"members": {`"clean_session":true}`, `"clean_session":true,"cause":"x"}`},
		"missing": {`,"clean_session":true}`, `}`},
	} {
		forged := bytes.Replace(b, []byte(edit[0]), []byte(edit[1]), 1)
		if bytes.Equal(forged, b) {
			t.Fatalf("%s: no edit", name)
		}
		if _, err := ParseReport(forged); err == nil {
			t.Fatalf("%s: accepted", name)
		}
	}
	nullOnly := bytes.Replace(b, []byte(`"probe_observation":{"end_state":"terminal_observed_without_exit","intact":false,"terminal_kind":"completed","clean_session":true}`), []byte(`"probe_observation":null`), 1)
	if bytes.Equal(nullOnly, b) {
		t.Fatal("no null edit")
	}
	if _, err := ParseReport(nullOnly); err == nil || !strings.Contains(err.Error(), "explicit null") {
		t.Fatalf("null observation: %v", err)
	}
}

// sampleReport is a minimal valid report with one analyzed case.
func sampleReport(t *testing.T) *Report {
	t.Helper()
	zero := 0
	o := ProbeObservation{EndState: EndTerminalWithoutExit, TerminalKind: sptr(EvCompleted), CleanSession: true}
	cs := CaseReport{CaseID: obsCase, Setting: "default", Outcome: KindToolResult, Events: []Event{}, Exit: &zero, Cleanup: CaseCleanup{GroupGone: true}, ProbeObservation: &o}
	name, ver := "claude-code", "2.1.292"
	return &Report{Schema: ReportSchema, RunID: "run-1", CaptureDate: "2026-10-08", OS: "linux", Arch: "amd64", HarnessVersion: "test", PlanSHA256: strings.Repeat("a", 64),
		HarnessStatus: HarnessVerified, VendorBehavior: VendorMeasured, Outcome: StatusConclusive, Evidence: []EvidenceRef{}, Cleanup: CleanupReport{OK: true, Failures: []string{}},
		Clients: []ClientReport{{ID: "claude", ObservedVersion: &ver, Outcome: StatusConclusive, ClientInfo: ClientInfo{Name: &name, Version: &ver, RequesterCompatible: bptr(true)},
			Phases: []PhaseReport{{Name: PhaseSetup, Status: StatusConclusive, Result: sptr(ResultHealthy), Cases: []CaseReport{cs}}}}}}
}

// UT-15: the bounded project-path derivation: the retained 2026-10-08
// golden vector, the permitted ASCII grammar and the .work position; any
// other platform, version, alias or path form derives nothing.
func TestCursorProjectSlug(t *testing.T) {
	m := newMemFS()
	const golden = "/tmp/claude-1000/recapture-2026-10-08/cursor/.work/cursor-capture-setup"
	got, err := cursorProjectSlug(m, "linux", "amd64", "2026.10.01-e373342", golden)
	if err != nil || got != "tmp-claude-1000-recapture-2026-10-08-cursor-work-cursor-capture-setup" {
		t.Fatalf("golden %q %v", got, err)
	}
	if got, err := cursorProjectSlug(m, "linux", "amd64", "2026.10.01-e373342", "/A_b/Z-9/.work/c"); err != nil || got != "A_b-Z-9-work-c" {
		t.Fatalf("grammar %q %v", got, err)
	}
	m.links["/x/out/.work/ws"] = "/real/out/.work/ws"
	for name, tc := range map[string][4]string{
		"darwin":       {"darwin", "arm64", "2026.10.01-e373342", golden},
		"darwin-amd64": {"darwin", "amd64", "2026.10.01-e373342", golden},
		"arm64":        {"linux", "arm64", "2026.10.01-e373342", golden},
		"version":      {"linux", "amd64", "2026.10.01-e373343", golden},
		"relative":     {"linux", "amd64", "2026.10.01-e373342", "tmp/.work/c"},
		"unclean":      {"linux", "amd64", "2026.10.01-e373342", "/tmp/../x/.work/c"},
		"root":         {"linux", "amd64", "2026.10.01-e373342", "/"},
		"alias":        {"linux", "amd64", "2026.10.01-e373342", "/x/out/.work/ws"},
		"no-work":      {"linux", "amd64", "2026.10.01-e373342", "/tmp/out/ws"},
		"work-early":   {"linux", "amd64", "2026.10.01-e373342", "/tmp/.work/out/ws"},
		"work-last":    {"linux", "amd64", "2026.10.01-e373342", "/tmp/out/.work"},
		"dot":          {"linux", "amd64", "2026.10.01-e373342", "/tmp/o.ut/.work/ws"},
		"hidden":       {"linux", "amd64", "2026.10.01-e373342", "/tmp/.cache/.work/ws"},
		"unicode":      {"linux", "amd64", "2026.10.01-e373342", "/tmp/é/.work/ws"},
		"space":        {"linux", "amd64", "2026.10.01-e373342", "/tmp/a b/.work/ws"},
		"plus":         {"linux", "amd64", "2026.10.01-e373342", "/tmp/a+b/.work/ws"},
	} {
		if s, err := cursorProjectSlug(m, tc[0], tc[1], tc[2], tc[3]); err == nil {
			t.Fatalf("%s: derived %q", name, s)
		}
	}
	// The slug is replaced only as a whole token.
	for in, want := range map[string]string{
		"enabled in a-b":       "enabled in <workspace-project>",
		"a-b/mcp":              "<workspace-project>/mcp",
		"xa-b a-bx a-b-c a-b.": "xa-b a-bx a-b-c <workspace-project>.",
		"none":                 "none",
	} {
		if got := normalizeSlug(in, "a-b"); got != want {
			t.Fatalf("normalize %q: %q", in, got)
		}
	}
	if normalizeSlug("a-b", "") != "a-b" {
		t.Fatal("an empty slug normalized")
	}
	if !validChangePath(labelProjectDir) || !validChangePath(labelProjectFile) || validChangePath("<home>/.cursor/<workspace-project>") ||
		validChangePath("<workspace>/<workspace-project>") || validChangePath(labelProjectDir+"/<workspace-project>") {
		t.Fatal("placeholder grammar")
	}
}

// UT-15: the bounded content check of the project approval file.
func TestProjectApprovalsContent(t *testing.T) {
	const p = "/h/.cursor/projects/s/mcp-approvals.json"
	for name, tc := range map[string]struct {
		data   string
		mutate func(n *memNode)
		want   string
	}{
		"observed":      {`["probe-6e58c4b6c129cbd0"]`, nil, ""},
		"others":        {` ["a", "probe-0123456789abcdef", "probe"] `, nil, ""},
		"object":        {`{"probe-6e58c4b6c129cbd0":true}`, nil, "is not a JSON array of strings"},
		"numbers":       {`[1]`, nil, "is not a JSON array of strings"},
		"null-element":  {`[null,"probe-6e58c4b6c129cbd0"]`, nil, "is not a JSON array of strings"},
		"bool-element":  {`["probe-6e58c4b6c129cbd0",false]`, nil, "is not a JSON array of strings"},
		"array-element": {`[["x"],"probe-6e58c4b6c129cbd0"]`, nil, "is not a JSON array of strings"},
		"null":          {`null`, nil, "is not a JSON array of strings"},
		"malformed":     {`["probe-6e58c4b6c129cbd0"`, nil, "is not a JSON array of strings"},
		"duplicate-key": {`[{"a":1,"a":2}]`, nil, "is not a JSON array of strings"},
		"empty":         {``, nil, "is not a JSON array of strings"},
		"none":          {`[]`, nil, "does not hold exactly one probe entry"},
		"duplicate":     {`["probe-6e58c4b6c129cbd0","probe-6e58c4b6c129cbd0"]`, nil, "does not hold exactly one probe entry"},
		"two":           {`["probe-6e58c4b6c129cbd0","probe-0123456789abcdef"]`, nil, "does not hold exactly one probe entry"},
		"other-probe":   {`["probe-6e58c4b6c129cbd0","probe-ABC"]`, nil, "holds another probe- entry"},
		"upper":         {`["probe-6E58C4B6C129CBD0"]`, nil, "holds another probe- entry"},
		"directory":     {``, func(n *memNode) { n.mode = fs.ModeDir | 0o700 }, "is not a regular file"},
		"link":          {``, func(n *memNode) { n.mode = fs.ModeSymlink | 0o777 }, "is not a regular file"},
		"unreadable":    {`[]`, func(n *memNode) { n.openErr = syscall.EACCES }, "cannot be opened"},
		"lookup":        {`[]`, func(n *memNode) { n.lstatErr = syscall.EIO }, "is not a regular file"},
		"racy":          {`["probe-6e58c4b6c129cbd0"]`, func(n *memNode) { n.racy = true }, "changed while it was read"},
		"over-bound":    {`["probe-6e58c4b6c129cbd0",` + strings.Repeat(`"x",`, 9) + `"y"]`, nil, "within the per-file bound"},
		"at-bound":      {`["probe-6e58c4b6c129cbd0",` + strings.Repeat(`"x",`, 8) + `"yyy"]`, nil, ""},
	} {
		m := newMemFS()
		n := m.file(p, tc.data)
		if tc.mutate != nil {
			tc.mutate(n)
		}
		sc, _ := newApprovalScanner(m, ApprovalScanLimits{FileBytes: 64})
		if got := sc.projectApprovals(p); tc.want == "" && got != "" || tc.want != "" && !strings.Contains(got, tc.want) {
			t.Fatalf("%s: %q, want %q", name, got, tc.want)
		}
	}
	m := newMemFS()
	sc, _ := newApprovalScanner(m, ApprovalScanLimits{})
	if got := sc.projectApprovals(p); got == "" {
		t.Fatal("an absent file passed")
	}
}

// projectWorld is a Cursor capture on the capture world whose output and
// owner home are symlink-free test paths, with the owner's projects
// directory present; enable scripts the enable (given the computed project
// directory) and plant prepares the home before the capture.
func projectWorld(t *testing.T, enable func(home, proj string, spec ProcSpec) capBehavior, plant func(home, proj string), mutate func(c *CaptureRunner)) (*capWorld, *CaptureManifest, *CaptureBundle, *CaptureRunner) {
	t.Helper()
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	home := filepath.Join(root, "home")
	os.MkdirAll(filepath.Join(home, ".cursor", "projects", "earlier"), 0o700)
	os.WriteFile(filepath.Join(home, ".cursor", "cli-config.json"), []byte(`{"token":"owner-secret-1"}`), 0o600)
	out := filepath.Join(root, "out")
	slug := strings.ReplaceAll(strings.TrimPrefix(filepath.Join(out, workDir, "cursor-capture-setup"), "/"), "/", "-")
	slug = strings.Replace(slug, "-.work-", "-work-", 1)
	proj := filepath.Join(home, ".cursor", "projects", slug)
	if plant != nil {
		plant(home, proj)
	}
	w := newCapWorld(t)
	w.script = func(spec ProcSpec) capBehavior {
		if launchKind(spec) == "enable" && enable != nil {
			return enable(home, proj, spec)
		}
		return w.defaults(spec)
	}
	man, b, c := b1Capture(t, w, capPlan(t, "cursor"), func(c *CaptureRunner) {
		c.Home, c.OutDir = home, out
		withFixtureRoot(c, root)
		if mutate != nil {
			mutate(c)
		}
	})
	return w, man, b, c
}

// approvalAt is the project approval write of an enable.
func approvalAt(proj, content string) map[string]*string {
	return map[string]*string{filepath.Join(proj, cursorApprovalsFile): strp(content)}
}

// UT-15: the project approval in capture: exact provenance with both
// additions; absent-before success versus pre-existing (or colliding)
// refusal with no enable; forbidden parent, sibling, descendant, other
// project, modification and removal; a lone directory and bad content;
// unsupported adapters; slug normalization; workspace-only unchanged.
func TestCursorProjectApprovalCapture(t *testing.T) {
	const good = `["probe-6e58c4b6c129cbd0"]`
	w, man, b, c := projectWorld(t, func(_, proj string, _ ProcSpec) capBehavior {
		return capBehavior{stdout: "enabled for " + filepath.Base(proj) + "\n", writes: approvalAt(proj, good)}
	}, nil, nil)
	cc := capClient(t, man, "cursor")
	a := cc.Approval
	slug := filepath.Base(filepath.Dir(filepath.Join(c.OutDir, workDir, "cursor-capture-setup")))
	_ = slug
	if cc.State != CaptureComplete || a.Scope != ScopeProjectScoped || a.Reason != nil || a.Project == nil ||
		*a.Project != (CaptureApprovalProject{Adapter: CursorProjectAdapter, Directory: labelProjectDir, File: labelProjectFile, ProbeEntryPresent: true}) ||
		!slices.Equal(a.Changes, []CaptureApprovalChange{{labelProjectDir, ChangeAdded}, {labelProjectFile, ChangeAdded}}) ||
		!slices.Equal(w.kinds(), []string{"cursor:version", "cursor:help", "cursor:enable", "cursor:session"}) {
		t.Fatalf("project approval: %q %+v %v", reasonOf(cc), a, w.kinds())
	}
	if out := string(b.Files[ClientFile("cursor", FileApprovalStdout)]); out != "enabled for <workspace-project>\n" || bytes.Contains(b.ManifestBytes, []byte("-work-cursor-capture-setup")) {
		t.Fatalf("normalization %q", out)
	}
	if _, err := ParseCaptureManifest(b.ManifestBytes); err != nil {
		t.Fatal(err)
	}
	// Code review B1.5 round 1, C3: escaped and nested JSON spellings of
	// the slug in stdout and stderr are normalized by decoded content; an
	// unrelated longer slug and other text stay.
	var slugSeen string
	hyphen := "\\" + "u002d" // the JSON escape of '-'
	escaped := func(slug string) string {
		inner, _ := json.Marshal(`{"path":"/x/` + strings.ReplaceAll(slug, "-", hyphen) + `/mcp-approvals.json","other":"` + slug + `-other"}`)
		return `{"msg":"enabled in ` + strings.ReplaceAll(slug, "-", hyphen) + `","nested":` + string(inner) + `,"deep":` + strconv.Quote(string(inner)) + `}` + "\n" +
			"plain " + slug + " done\n"
	}
	_, man, b, _ = projectWorld(t, func(_, proj string, _ ProcSpec) capBehavior {
		slugSeen = filepath.Base(proj)
		return capBehavior{stdout: escaped(slugSeen), stderr: escaped(slugSeen), writes: approvalAt(proj, good)}
	}, nil, nil)
	if cc := capClient(t, man, "cursor"); cc.State != CaptureComplete || cc.Approval.Scope != ScopeProjectScoped {
		t.Fatalf("escaped output: %q %+v", reasonOf(cc), cc.Approval)
	}
	for _, name := range []string{FileApprovalStdout, FileApprovalStderr} {
		data := string(b.Files[ClientFile("cursor", name)])
		view := data
		for i := 0; i < 4; i++ {
			view = unescapeView(view)
		}
		if normalizeSlug(view, slugSeen) != view || !strings.Contains(view, slugSeen+"-other") || !strings.Contains(data, "plain <workspace-project> done") ||
			strings.Count(view, labelWorkspaceProject) != 4 {
			t.Fatalf("%s keeps the slug: %q", name, data)
		}
	}
	if s := capClient(t, man, "cursor").Streams; !slices.ContainsFunc(s, func(st CaptureStream) bool { return st.Path == ClientFile("cursor", FileApprovalStdout) && st.Clean() }) {
		t.Fatalf("streams %+v", s)
	}
	if got, n := normalizeSlugText([]byte("a\n{\"k\":1}\nb"), ""); string(got) != "a\n{\"k\":1}\nb" || n != 0 {
		t.Fatalf("no slug: %q", got)
	}
	// Code review B1.5 round 2, C3: by construction, no decodable form of
	// the slug survives in either stream: escaped prose inside JSON strings,
	// double-encoded keys and mixed escapes are normalized by their bounded
	// unescaped views; an encoding beyond the decode bound and escaped
	// non-JSON prose are withheld and counted as omitted; plain vendor text
	// is untouched. (Every backslash is built at runtime.)
	bs := "\\"
	layer := func(s string) string { q, _ := json.Marshal(s); return string(q[1 : len(q)-1]) }
	uesc := func(s string) string { return strings.ReplaceAll(s, "-", bs+"u002d") }
	mixed := func(s string) string {
		var out strings.Builder
		for i, r := range s {
			switch {
			case r == '-' && i%3 == 0:
				out.WriteString(bs + "u002D")
			case r == '-':
				out.WriteString(bs + "u002d")
			case r == 't':
				out.WriteString(bs + "u0074")
			default:
				out.WriteRune(r)
			}
		}
		return out.String()
	}
	const plain = "✓ Enabled and approved MCP server: probe"
	normalizable := func(slug string) string {
		return plain + "\n" +
			`{"msg":"` + layer("enabled in "+uesc(slug)) + `"}` + "\n" + // escaped prose, decoded twice
			`{"` + layer(uesc(slug)) + `":1,"` + layer(layer(uesc(slug))) + `":2}` + "\n" + // double- and triple-encoded keys
			`{"mixed":"` + layer(mixed(slug)) + ` and ` + mixed(slug) + `"}` + "\n" + // mixed escapes
			"done " + slug + "\n"
	}
	withheld := func(slug string) string {
		deep := uesc(slug)
		for i := 0; i < maxRedactDepth+1; i++ {
			deep = layer(deep)
		}
		return plain + "\n" + `{"deep":"` + deep + `"}` + "\n" + // beyond the decode bound
			"note: enabled in " + uesc(slug) + "\n" // escaped non-JSON prose
	}
	noSlug := func(t *testing.T, name, data, slug string) {
		t.Helper()
		for _, l := range strings.Split(data, "\n") {
			if view, resolved := unescapeResolved(l); !resolved || normalizeSlug(view, slug) != view || strings.Contains(l, slug) {
				t.Fatalf("%s keeps a decodable slug: %q", name, l)
			}
		}
		if !strings.HasPrefix(data, plain+"\n") {
			t.Fatalf("%s changed the plain vendor text: %q", name, data)
		}
	}
	for _, tc := range []struct {
		output   func(string) string
		omitted  int
		complete bool
	}{{normalizable, 0, true}, {withheld, 2, false}} {
		_, man, b, _ = projectWorld(t, func(_, proj string, _ ProcSpec) capBehavior {
			slugSeen = filepath.Base(proj)
			return capBehavior{stdout: tc.output(slugSeen), stderr: tc.output(slugSeen), writes: approvalAt(proj, good)}
		}, nil, nil)
		cc := capClient(t, man, "cursor")
		if (cc.State == CaptureComplete) != tc.complete || cc.Approval.Scope != ScopeProjectScoped || !tc.complete && reasonOf(cc) != ReasonEvidenceOmitted {
			t.Fatalf("by construction: %q %+v", reasonOf(cc), cc.Approval)
		}
		for _, name := range []string{FileApprovalStdout, FileApprovalStderr} {
			data := string(b.Files[ClientFile("cursor", name)])
			noSlug(t, name, data, slugSeen)
			if s := streamRecord(cc, name); s.OmittedLines != tc.omitted {
				t.Fatalf("%s omitted %d, want %d: %q", name, s.OmittedLines, tc.omitted, data)
			}
			if tc.complete && strings.Count(data, labelWorkspaceProject) < 5 {
				t.Fatalf("%s lost a normalized form: %q", name, data)
			}
		}
	}
	type outcome struct {
		enable func(home, proj string, spec ProcSpec) capBehavior
		plant  func(home, proj string)
		mutate func(c *CaptureRunner)
		reason string
		scope  string
		kinds  int
	}
	stopped := 3 // version, help, enable
	for name, tc := range map[string]outcome{
		"preexists": {func(_, proj string, _ ProcSpec) capBehavior { return capBehavior{writes: approvalAt(proj, good)} },
			func(_, proj string) { os.MkdirAll(proj, 0o700) }, nil,
			// A3.1: the case-ownership check precedes the inventory.
			ReasonCaseProjectExists, ScopeUnverifiable, 2},
		"dir-only": {func(_, proj string, _ ProcSpec) capBehavior {
			os.MkdirAll(proj, 0o700)
			return capBehavior{}
		}, nil, nil, ReasonCursorScopeUnverified + ": the project approval needs both", ScopeUnverifiable, stopped},
		"bad-content": {func(_, proj string, _ ProcSpec) capBehavior { return capBehavior{writes: approvalAt(proj, `["x"]`)} }, nil, nil,
			ReasonCursorScopeUnverified + ": the project approval file does not hold exactly one probe entry", ScopeUnverifiable, stopped},
		"null-element": {func(_, proj string, _ ProcSpec) capBehavior {
			return capBehavior{writes: approvalAt(proj, `[null,"probe-6e58c4b6c129cbd0"]`)}
		}, nil, nil, ReasonCursorScopeUnverified + ": the project approval file is not a JSON array of strings", ScopeUnverifiable, stopped},
		"extra-file": {func(_, proj string, _ ProcSpec) capBehavior {
			return capBehavior{writes: map[string]*string{filepath.Join(proj, cursorApprovalsFile): strp(good), filepath.Join(proj, "x.json"): strp("1")}}
		}, nil, nil, ReasonCursorOutside, ScopeOutside, stopped},
		"approvals-dir": {func(_, proj string, _ ProcSpec) capBehavior {
			return capBehavior{writes: map[string]*string{filepath.Join(proj, cursorApprovalsFile, "x"): strp("1")}}
		}, nil, nil, ReasonCursorOutside, ScopeOutside, stopped},
		"slug-file": {func(_, proj string, _ ProcSpec) capBehavior {
			return capBehavior{writes: map[string]*string{proj: strp("1")}}
		}, nil, nil, ReasonCursorOutside, ScopeOutside, stopped},
		"other-project": {func(_, proj string, _ ProcSpec) capBehavior { return capBehavior{writes: approvalAt(proj+"-x", good)} }, nil, nil,
			ReasonCursorOutside, ScopeOutside, stopped},
		"earlier-modified": {func(home, proj string, _ ProcSpec) capBehavior {
			return capBehavior{writes: map[string]*string{filepath.Join(proj, cursorApprovalsFile): strp(good), filepath.Join(home, ".cursor", "projects", "earlier", "a"): strp("1")}}
		}, nil, nil, ReasonCursorOutside, ScopeOutside, stopped},
		"earlier-removed": {func(home, proj string, _ ProcSpec) capBehavior {
			return capBehavior{writes: map[string]*string{filepath.Join(proj, cursorApprovalsFile): strp(good), filepath.Join(home, ".cursor", "projects", "earlier"): nil}}
		}, nil, nil, ReasonCursorOutside, ScopeOutside, stopped},
		"projects-created": {func(_, proj string, _ ProcSpec) capBehavior { return capBehavior{writes: approvalAt(proj, good)} },
			func(home, _ string) { os.RemoveAll(filepath.Join(home, ".cursor", "projects")) }, nil, ReasonCursorOutside, ScopeOutside, stopped},
		"other-outside": {func(home, proj string, _ ProcSpec) capBehavior {
			return capBehavior{writes: map[string]*string{filepath.Join(proj, cursorApprovalsFile): strp(good), filepath.Join(home, ".cursor", "x"): strp("1")}}
		}, nil, nil, ReasonCursorOutside, ScopeOutside, stopped},
		"darwin": {func(_, proj string, _ ProcSpec) capBehavior { return capBehavior{writes: approvalAt(proj, good)} }, nil,
			func(c *CaptureRunner) { c.GOOS = "darwin" }, ReasonCursorScopeUnverified + ": a change under <home>/.cursor/projects/ cannot be attributed: no project-path adapter for darwin/amd64", ScopeUnverifiable, stopped},
		"darwin-and-outside": {func(home, proj string, _ ProcSpec) capBehavior {
			return capBehavior{writes: map[string]*string{filepath.Join(proj, cursorApprovalsFile): strp(good), filepath.Join(home, ".cursor", "x"): strp("1")}}
		}, nil, func(c *CaptureRunner) { c.GOOS = "darwin" }, ReasonCursorOutside, ScopeOutside, stopped},
		"failed-enable": {func(_, proj string, _ ProcSpec) capBehavior {
			return capBehavior{exit: 1, writes: approvalAt(proj, good)}
		}, nil, nil, ReasonCursorApprovalFailed, ScopeUnverifiable, stopped},
	} {
		w, man, b, _ := projectWorld(t, tc.enable, tc.plant, tc.mutate)
		cc := capClient(t, man, "cursor")
		a := cc.Approval
		if !strings.HasPrefix(reasonOf(cc), tc.reason) || a.Scope != tc.scope || a.Project != nil || len(w.kinds()) != tc.kinds || len(w.sessions()) != 0 {
			t.Fatalf("%s: %q %+v %v", name, reasonOf(cc), a, w.kinds())
		}
		if _, err := ParseCaptureManifest(b.ManifestBytes); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if name == "extra-file" && !slices.Contains(a.Changes, CaptureApprovalChange{labelProjectDir + "/x.json", ChangeAdded}) {
			t.Fatalf("extra file %+v", a.Changes)
		}
	}
	// Workspace-only stays: the default enable, also with the adapter.
	_, man, _, _ = projectWorld(t, nil, nil, nil)
	if cc := capClient(t, man, "cursor"); cc.State != CaptureComplete || cc.Approval.Scope != ScopeWorkspaceOnly || cc.Approval.Project != nil {
		t.Fatalf("workspace-only %q", reasonOf(cc))
	}
}

// UT-15: the strict project/policy schema of the approval record, the
// workspace-only legacy record without policy, and invalid project
// metadata.
func TestCaptureApprovalProjectManifest(t *testing.T) {
	_, man, b, c := projectWorld(t, func(_, proj string, _ ProcSpec) capBehavior {
		return capBehavior{writes: approvalAt(proj, `["probe-6e58c4b6c129cbd0"]`)}
	}, nil, nil)
	if capClient(t, man, "cursor").Approval.Scope != ScopeProjectScoped {
		t.Fatal("no project-scoped record")
	}
	clone := func() *CaptureManifest {
		var m CaptureManifest
		if err := json.Unmarshal(b.ManifestBytes, &m); err != nil {
			t.Fatal(err)
		}
		return &m
	}
	forbidden := CaptureApprovalChange{"<home>/x", ChangeAdded}
	for name, mutate := range map[string]func(a *CaptureApproval){
		"no-project":      func(a *CaptureApproval) { a.Project = nil },
		"adapter":         func(a *CaptureApproval) { a.Project.Adapter = "cursor-darwin-arm64-2026.10.01-e373342" },
		"directory":       func(a *CaptureApproval) { a.Project.Directory = "<home>/.cursor/projects/x" },
		"file":            func(a *CaptureApproval) { a.Project.File = labelProjectDir + "/x.json" },
		"entry":           func(a *CaptureApproval) { a.Project.ProbeEntryPresent = false },
		"no-policy":       func(a *CaptureApproval) { a.InventoryPolicy, a.ExcludedPaths = nil, nil },
		"policy-only":     func(a *CaptureApproval) { a.ExcludedPaths = nil },
		"paths-only":      func(a *CaptureApproval) { a.InventoryPolicy = nil },
		"policy":          func(a *CaptureApproval) { a.InventoryPolicy = sptr("cursor-approval-v1") },
		"reordered":       func(a *CaptureApproval) { a.ExcludedPaths = []string{a.ExcludedPaths[1], a.ExcludedPaths[0]} },
		"missing-dir":     func(a *CaptureApproval) { a.Changes = a.Changes[1:] },
		"missing-file":    func(a *CaptureApproval) { a.Changes = a.Changes[:1] },
		"modified":        func(a *CaptureApproval) { a.Changes[1].Kind = ChangeModified },
		"removed":         func(a *CaptureApproval) { a.Changes[0].Kind = ChangeRemoved },
		"forbidden":       func(a *CaptureApproval) { a.Changes = append(a.Changes, forbidden) },
		"unclean":         func(a *CaptureApproval) { one := 1; a.Stage.Exit = &one },
		"incomplete":      func(a *CaptureApproval) { a.InventoryComplete = false },
		"reason":          func(a *CaptureApproval) { a.Reason = sptr("x") },
		"workspace-scope": func(a *CaptureApproval) { a.Scope = ScopeWorkspaceOnly },
		"project-on-outside": func(a *CaptureApproval) {
			a.Scope, a.Reason = ScopeOutside, sptr("x")
		},
	} {
		m := clone()
		mutate(m.Clients[0].Approval)
		if err := m.Validate(); err == nil {
			t.Fatalf("%s: accepted", name)
		}
	}
	// Code review B1.5 round 1, C4: the provenance is bound to the
	// enclosing linux/amd64 platform and exact version, in the manifest, in
	// bundle validation and in enrollment.
	identity := map[string]func(m map[string]any){
		"darwin": func(m map[string]any) { m["os"] = "darwin" },
		"arm64":  func(m map[string]any) { m["arch"] = "arm64" },
		"version": func(m map[string]any) {
			c := m["clients"].([]any)[0].(map[string]any)
			c["expected_version"], c["observed_version"] = "2026.10.02-0000000", "2026.10.02-0000000"
		},
	}
	for name, mutate := range identity {
		var raw map[string]any
		json.Unmarshal(b.ManifestBytes, &raw)
		mutate(raw)
		forged, _ := json.MarshalIndent(raw, "", "  ")
		if _, err := ParseCaptureManifest(forged); err == nil || !strings.Contains(err.Error(), "project_scoped provenance") {
			t.Fatalf("%s manifest: %v", name, err)
		}
		if err := tamperedBundle(t, c.OutDir, forged); err == nil || !strings.Contains(err.Error(), "project_scoped provenance") {
			t.Fatalf("%s bundle: %v", name, err)
		}
		if err := tamperedEnrollment(t, c.OutDir, forged, raw); err == nil || !strings.Contains(err.Error(), "project_scoped provenance") {
			t.Fatalf("%s enrollment: %v", name, err)
		}
	}
	// Workspace .cursor files may accompany the pair.
	m := clone()
	a := m.Clients[0].Approval
	a.Changes = append(a.Changes, CaptureApprovalChange{"<workspace>/.cursor/a.json", ChangeAdded})
	if err := m.Validate(); err != nil {
		t.Fatalf("with a workspace file: %v", err)
	}
	// Unverifiable with a home-project change only is valid; with any other
	// outside change it is not.
	_, man2, b2, _ := projectWorld(t, func(_, proj string, _ ProcSpec) capBehavior {
		os.MkdirAll(proj, 0o700)
		return capBehavior{}
	}, nil, nil)
	if capClient(t, man2, "cursor").Approval.Scope != ScopeUnverifiable {
		t.Fatal("no unverifiable record")
	}
	var m2 CaptureManifest
	json.Unmarshal(b2.ManifestBytes, &m2)
	if err := m2.Validate(); err != nil {
		t.Fatal(err)
	}
	m2.Clients[0].Approval.Changes = append(m2.Clients[0].Approval.Changes, CaptureApprovalChange{"<home>/.cursor/x", ChangeAdded})
	if err := m2.Validate(); err == nil {
		t.Fatal("unverifiable with an outside change accepted")
	}
	// A legacy workspace-only record without policy fields stays valid.
	_, man3, b3, _ := projectWorld(t, nil, nil, nil)
	_ = man3
	var raw map[string]any
	json.Unmarshal(b3.ManifestBytes, &raw)
	ap := raw["clients"].([]any)[0].(map[string]any)["approval"].(map[string]any)
	delete(ap, "inventory_policy")
	delete(ap, "excluded_paths")
	legacy, _ := json.Marshal(raw)
	if _, err := ParseCaptureManifest(legacy); err != nil {
		t.Fatalf("legacy record: %v", err)
	}
	for name, v := range map[string]any{"null-policy": nil, "null-project": nil} {
		json.Unmarshal(b.ManifestBytes, &raw)
		ap := raw["clients"].([]any)[0].(map[string]any)["approval"].(map[string]any)
		if name == "null-policy" {
			ap["inventory_policy"] = v
		} else {
			ap["project"] = v
		}
		forged, _ := json.Marshal(raw)
		if _, err := ParseCaptureManifest(forged); err == nil {
			t.Fatalf("%s: accepted", name)
		}
	}
	json.Unmarshal(b.ManifestBytes, &raw)
	raw["clients"].([]any)[0].(map[string]any)["approval"].(map[string]any)["excluded_paths"] = nil
	forged, _ := json.Marshal(raw)
	if _, err := ParseCaptureManifest(forged); err == nil {
		t.Fatal("null excluded_paths accepted")
	}
	// Every new record names the policy, a not-run record included.
	if ap := newApproval("x"); ap.InventoryPolicy == nil || *ap.InventoryPolicy != InventoryPolicyV2 || !slices.Equal(ap.ExcludedPaths, []string{"<home>/.cursor/chats", "<home>/.cursor/ai-tracking"}) {
		t.Fatalf("new record %+v", ap)
	}
}

// excludeFS is a memFS whose listing of an excluded directory (or below
// it) is a test failure: the exclusions never descend.
type excludeFS struct {
	*memFS
	t    *testing.T
	deny []string
}

func (e excludeFS) ReadDir(p string) ([]fs.DirEntry, error) {
	for _, d := range e.deny {
		if p == d || strings.HasPrefix(p, d+"/") {
			e.t.Errorf("listed the excluded %s", p)
		}
	}
	return e.memFS.ReadDir(p)
}

// UT-16: the inventory exclusions on tiny injected trees: exactly the two
// owner directories' contents by absolute identity (also through a
// deduplicated resolved alias), never by basename; their boundaries are
// recorded, counted and type-checked; relevant bounds and special files
// still apply at N/N+1.
func TestApprovalInventoryExclusions(t *testing.T) {
	const ws, home = "/c/out/.work/cursor-capture-setup", "/home/owner"
	roots := []scanRoot{{label: labelWorkspace, path: ws}, {label: labelHomeCursor, path: home + "/.cursor"}, {label: "<ancestor-1>/.cursor", path: "/c/out/.work/.cursor"}}
	base := newMemFS()
	base.file(ws+"/.cursor/mcp.json", `{}`)
	base.file(home+"/.cursor/mcp.json", `{"mcpServers":{}}`)
	base.mkdirs(home + "/.cursor/projects/p/chats")
	base.file(home+"/.cursor/projects/p/chats/c.json", "1")
	base.file(ws+"/.cursor/chats/w.json", "1")
	base.file("/c/out/.work/.cursor/ai-tracking/a.db", "1")
	for _, d := range []string{"chats", "ai-tracking"} {
		for i := 0; i < 40; i++ {
			base.file(home+"/.cursor/"+d+"/f"+strings.Repeat("x", i), strings.Repeat("d", 100))
		}
	}
	scan := func(m *memFS, lim ApprovalScanLimits) *snapshot {
		sc, err := newApprovalScanner(excludeFS{m, t, []string{home + "/.cursor/chats", home + "/.cursor/ai-tracking"}}, lim)
		if err != nil {
			t.Fatal(err)
		}
		sc.exclude(home)
		return sc.snapshot(roots)
	}
	pre := scan(base, ApprovalScanLimits{})
	// ws, .cursor, mcp.json, chats, w.json (5); home .cursor, mcp.json,
	// chats, ai-tracking, projects, p, chats, c.json (8); the ancestor root,
	// ai-tracking, a.db (3): 16 entries; the 80 excluded files are neither
	// counted nor charged.
	if !pre.complete || pre.count != 16 || pre.bytes != int64(2+len(`{"mcpServers":{}}`)+1+1+1) {
		t.Fatalf("pre %d entries %d bytes %v", pre.count, pre.bytes, pre.why)
	}
	for _, k := range []string{"<home>/.cursor/chats", "<home>/.cursor/ai-tracking", "<home>/.cursor/projects/p/chats/c.json", "<workspace>/.cursor/chats/w.json", "<ancestor-1>/.cursor/ai-tracking/a.db"} {
		if _, ok := pre.entries[k]; !ok {
			t.Fatalf("%s not inventoried", k)
		}
	}
	for k := range pre.entries {
		if strings.HasPrefix(k, "<home>/.cursor/chats/") || strings.HasPrefix(k, "<home>/.cursor/ai-tracking/") {
			t.Fatalf("excluded content %s inventoried", k)
		}
	}
	// N passes, N+1 fails (entries and relevant bytes).
	if !scan(base, ApprovalScanLimits{Entries: 16}).complete || scan(base, ApprovalScanLimits{Entries: 15}).complete ||
		!scan(base, ApprovalScanLimits{Bytes: pre.bytes}).complete || scan(base, ApprovalScanLimits{Bytes: pre.bytes - 1}).complete {
		t.Fatal("relevant bounds at N/N+1")
	}
	// Contents and directory churn are invisible; the boundaries' own
	// addition, removal and mode changes are not.
	after := base.clone()
	after.file(home+"/.cursor/chats/new", "x")
	delete(after.nodes, home+"/.cursor/ai-tracking/f")
	sc, _ := newApprovalScanner(excludeFS{base, t, []string{home + "/.cursor/chats", home + "/.cursor/ai-tracking"}}, ApprovalScanLimits{})
	sc.exclude(home)
	p1 := sc.snapshot(roots)
	sc.fs = excludeFS{after, t, []string{home + "/.cursor/chats", home + "/.cursor/ai-tracking"}}
	if ch := diffSnapshots(p1, sc.snapshot(roots)); len(ch) != 0 {
		t.Fatalf("excluded content changes %+v", ch)
	}
	moved := base.clone()
	moved.nodes[home+"/.cursor/chats"].mode = fs.ModeDir | 0o755
	for k := range moved.nodes {
		if strings.HasPrefix(k, home+"/.cursor/ai-tracking") {
			delete(moved.nodes, k)
		}
	}
	sc.fs = excludeFS{moved, t, nil}
	if ch := diffSnapshots(p1, sc.snapshot(roots)); !slices.Equal(changesOf(ch), []CaptureApprovalChange{{"<home>/.cursor/ai-tracking", ChangeRemoved}, {"<home>/.cursor/chats", ChangeModified}}) {
		t.Fatalf("boundary changes %+v", ch)
	}
	// Absent boundaries are fine.
	none := base.clone()
	for k := range none.nodes {
		if strings.HasPrefix(k, home+"/.cursor/chats") || strings.HasPrefix(k, home+"/.cursor/ai-tracking") {
			delete(none.nodes, k)
		}
	}
	if s := scan(none, ApprovalScanLimits{}); !s.complete || s.count != 14 {
		t.Fatalf("absent boundaries %d %v", s.count, s.why)
	}
	// A boundary that is a link, a file or unreadable fails closed; a
	// special file in projects still fails.
	for name, mutate := range map[string]func(m *memFS){
		"link":    func(m *memFS) { m.nodes[home+"/.cursor/chats"].mode = fs.ModeSymlink | 0o777 },
		"file":    func(m *memFS) { m.nodes[home+"/.cursor/ai-tracking"].mode = 0o600 },
		"lookup":  func(m *memFS) { m.nodes[home+"/.cursor/chats"].lstatErr = syscall.EACCES },
		"special": func(m *memFS) { m.nodes[home+"/.cursor/projects/p/sock"] = &memNode{mode: fs.ModeSocket | 0o600} },
	} {
		m := base.clone()
		mutate(m)
		if s := scan(m, ApprovalScanLimits{}); s.complete {
			t.Fatalf("%s: complete", name)
		}
	}
	// A deduplicated resolved alias of the home .cursor is excluded by its
	// identity: here the home's .cursor resolves to the ancestor root.
	alias := newMemFS()
	alias.links["/h/.cursor"] = "/c/out/.work/.cursor"
	alias.file("/c/out/.work/.cursor/chats/x", "1")
	alias.file("/c/out/.work/.cursor/ai-tracking/y", "1")
	asc, _ := newApprovalScanner(excludeFS{alias, t, []string{"/c/out/.work/.cursor/chats", "/c/out/.work/.cursor/ai-tracking"}}, ApprovalScanLimits{})
	asc.exclude("/h")
	if s := asc.snapshot([]scanRoot{{label: "<ancestor-1>/.cursor", path: "/c/out/.work/.cursor"}}); !s.complete || s.count != 3 {
		t.Fatalf("alias %d %v", s.count, s.why)
	}
	// Without an exclusion set nothing is excluded (the scanner's default).
	plain, _ := newApprovalScanner(base, ApprovalScanLimits{})
	if s := plain.snapshot(roots); s.count != 96 {
		t.Fatalf("unexcluded count %d", s.count)
	}
	// A resolution failure of the home's .cursor keeps the lexical identity.
	broken := &memPlacementApproval{*base.clone()}
	bsc, _ := newApprovalScanner(broken, ApprovalScanLimits{})
	bsc.exclude(home)
	if s := bsc.snapshot(roots[1:2]); !s.complete || s.count != 8 {
		t.Fatalf("unresolved home %d %v", s.count, s.why)
	}
}

func changesOf(ch []approvalChange) []CaptureApprovalChange {
	out := []CaptureApprovalChange{}
	for _, c := range ch {
		out = append(out, c.CaptureApprovalChange)
	}
	return out
}

// UT-16: the exclusions in capture: the owner's data directories grow
// during the enable without blocking a workspace or project approval, and
// every record names the policy.
func TestCursorInventoryPolicyCapture(t *testing.T) {
	_, man, _, _ := projectWorld(t, func(home, proj string, _ ProcSpec) capBehavior {
		return capBehavior{writes: map[string]*string{filepath.Join(proj, cursorApprovalsFile): strp(`["probe-6e58c4b6c129cbd0"]`),
			filepath.Join(home, ".cursor", "chats", "n1"): strp("1"), filepath.Join(home, ".cursor", "ai-tracking", "n2"): strp("2")}}
	}, func(home, _ string) {
		for _, d := range []string{"chats", "ai-tracking"} {
			os.MkdirAll(filepath.Join(home, ".cursor", d), 0o700)
			for i := 0; i < 12; i++ {
				os.WriteFile(filepath.Join(home, ".cursor", d, strings.Repeat("o", i+1)), []byte("old"), 0o600)
			}
		}
	}, func(c *CaptureRunner) { c.ApprovalLimits = ApprovalScanLimits{Entries: 16} })
	cc := capClient(t, man, "cursor")
	if cc.State != CaptureComplete || cc.Approval.Scope != ScopeProjectScoped || len(cc.Approval.Changes) != 2 || *cc.Approval.InventoryPolicy != InventoryPolicyV2 {
		t.Fatalf("data growth: %q %+v", reasonOf(cc), cc.Approval)
	}
	// A pre-scan failure on a boundary records the policy too, with no
	// enable.
	w, man, _, _ := projectWorld(t, nil, func(home, _ string) { os.Symlink("/", filepath.Join(home, ".cursor", "chats")) }, nil)
	cc = capClient(t, man, "cursor")
	if !strings.HasPrefix(reasonOf(cc), ReasonCursorScopeUnverified+": the inventory before the command is incomplete (an excluded data directory") ||
		cc.Approval.InventoryPolicy == nil || slices.Contains(w.kinds(), "cursor:enable") {
		t.Fatalf("boundary link: %q %+v", reasonOf(cc), cc.Approval)
	}
}

// copyBundle copies the bundle in dir into to, with manifest as its
// manifest.json.
func copyBundle(t *testing.T, dir, to string, manifest []byte) {
	t.Helper()
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		rel, _ := filepath.Rel(dir, p)
		data, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		if rel == CaptureManifestName {
			data = manifest
		}
		os.MkdirAll(filepath.Dir(filepath.Join(to, rel)), 0o700)
		return os.WriteFile(filepath.Join(to, rel), data, 0o600)
	})
	if err != nil {
		t.Fatal(err)
	}
}

// tamperedBundle validates a copy of the bundle in dir with manifest.
func tamperedBundle(t *testing.T, dir string, manifest []byte) error {
	t.Helper()
	to := t.TempDir()
	copyBundle(t, dir, to, manifest)
	_, err := ValidateCaptureBundle(os.DirFS(to), ".")
	return err
}

// tamperedEnrollment indexes a copy of the Cursor bundle in dir, with
// manifest (decoded as raw), as an enrolled fixture of the identity the
// manifest claims and runs the enrollment self-check.
func tamperedEnrollment(t *testing.T, dir string, manifest []byte, raw map[string]any) error {
	t.Helper()
	cl := raw["clients"].([]any)[0].(map[string]any)
	version, platform := cl["observed_version"].(string), raw["os"].(string)+"/"+raw["arch"].(string)
	e := EnrollmentEntry{Client: "cursor", Decoder: "cursor-jsonl", Version: version, Platform: platform, Fixture: EnrolledFixtureID("cursor-jsonl", version, platform),
		Bundle: EnrolledBundlePath("cursor", version, platform, raw["run_id"].(string)), ManifestSHA256: sha256Hex(manifest)}
	repo := t.TempDir()
	copyBundle(t, dir, filepath.Join(repo, filepath.FromSlash(e.Bundle)), manifest)
	oracle := []byte("{}")
	os.WriteFile(filepath.Join(repo, filepath.FromSlash(e.Bundle), ExpectedName), oracle, 0o600)
	e.ExpectedSHA256 = sha256Hex(oracle)
	idx, _ := json.Marshal(EnrollmentIndex{Schema: EnrollmentSchema, Entries: []EnrollmentEntry{e}})
	os.MkdirAll(filepath.Join(repo, filepath.FromSlash(EnrollmentRoot)), 0o700)
	os.WriteFile(filepath.Join(repo, filepath.FromSlash(EnrollmentIndexPath)), idx, 0o600)
	_, err := ValidateEnrollment(EnrollmentOptions{FS: os.DirFS(repo), Registry: DefaultRegistry()})
	return err
}
