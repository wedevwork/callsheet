package mcpqual

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// Session-side reasons (never a measured MCP timeout).
const (
	ReasonAbsentBinary       = "absent_binary"
	ReasonLaunchFailed       = "launch_failed"
	ReasonVersionMismatch    = "version_mismatch"
	ReasonDecoderVersion     = "unsupported_decoder_version"
	ReasonModelCallsDenied   = "model_calls_not_allowed"
	ReasonAuth               = "authentication_failure"
	ReasonPermission         = "tool_approval_denied"
	ReasonRefusal            = "model_refusal"
	ReasonNoCall             = "model_did_not_call"
	ReasonSessionTimeout     = "session_timeout"
	ReasonSessionError       = "session_error"
	ReasonModelUnavailable   = "model_unavailable"
	ReasonConfigFailure      = "config_failure"
	ReasonExtraCalls         = "unexpected_extra_calls"
	ReasonNonce              = "nonce_mismatch"
	ReasonToolError          = "tool_error"
	ReasonNoOutcome          = "process_exit_without_typed_outcome"
	ReasonProbeMissing       = "probe_receipt_missing"
	ReasonProbeFatal         = "probe_ended_early"
	ReasonProbeConcurrent    = "concurrent_call"
	ReasonProbeEndpoint      = "probe_endpoint_missing"
	ReasonProbeLog           = "probe_events_invalid"
	ReasonCleanupFailed      = "cleanup_failed"
	ReasonInterrupted        = "interrupted"
	ReasonBudget             = "budget_exhausted"
	ReasonStdoutHeld         = "stdout_not_closed_after_cleanup"
	ReasonClientBudget       = "budget_exhausted: max_client_ms reached during the session"
	ReasonMetadataNotAllowed = "metadata_command_not_allowed"
	ReasonMetadataFailed     = "metadata_capture_failed"
	ReasonEvidenceTruncated  = "evidence_truncated"
	ReasonReportBound        = "report_bound"
)

type caseSpec struct {
	id       string
	setting  string // "default" or "raised"
	delay    int64
	interval int64
}

// caseRun is the raw result of one launched session.
type caseRun struct {
	transcript  Transcript
	probeRaw    []byte
	exit        *int
	signal      *string
	watchdog    bool
	interrupted bool
	cleanup     CaseCleanup
	launchErr   error
	stdoutHeld  bool
	stderr      []byte
	// budgetCapped: the watchdog was the client's remaining wall time.
	budgetCapped bool
	// evidenceCut: a sanitized evidence file reached its limit.
	evidenceCut bool
}

// launch runs one process to completion or its watchdog (or ctx), then
// always reaps its group. Stdout is captured bounded, each line stamped
// with the harness clock's offset from launch.
func (r *Runner) launch(ctx context.Context, spec ProcSpec, watchdog time.Duration, limit int) caseRun {
	var cr caseRun
	start := r.Clock.Now()
	p, err := r.Launcher.Start(spec)
	if err != nil {
		cr.launchErr = err
		return cr
	}
	readerDone := make(chan struct{})
	var t Transcript
	go func() {
		defer close(readerDone)
		t = readLines(p.Stdout(), limit, func() int64 { return int64(r.Clock.Now().Sub(start)) })
	}()
	tc, stop := r.Clock.NewTimer(watchdog)
	select {
	case <-p.Exited():
	case <-tc:
		cr.watchdog = true
	case <-ctx.Done():
		cr.interrupted = true
	}
	stop()
	cr.cleanup = r.Reaper.Reap(p)
	cr.stdoutHeld = !awaitStdout(r.Clock, p, readerDone, r.stdoutWait())
	<-readerDone
	cr.transcript = t
	cr.exit, cr.signal = p.Status()
	return cr
}

func (r *Runner) stdoutWait() time.Duration {
	if r.StdoutWait > 0 {
		return r.StdoutWait
	}
	return 2 * time.Second
}

// awaitStdout waits for the stdout reader to finish after cleanup,
// bounded; the read end is closed either way (which ends the reader).
func awaitStdout(clock Clock, p Proc, readerDone <-chan struct{}, limit time.Duration) bool {
	tc, stop := clock.NewTimer(limit)
	defer stop()
	defer p.CloseStdout()
	select {
	case <-readerDone:
		return true
	case <-tc:
		return false
	}
}

// readLines reads r to its end, splitting LF-terminated lines (a CR before
// LF is dropped). After limit bytes it marks the transcript truncated and
// keeps draining without storing.
func readLines(rd io.Reader, limit int, offset func() int64) Transcript {
	var t Transcript
	var partial []byte
	total := 0
	buf := make([]byte, 32<<10)
	for {
		n, err := rd.Read(buf)
		chunk := buf[:n]
		for len(chunk) > 0 && !t.Truncated {
			i := bytes.IndexByte(chunk, '\n')
			take := chunk
			if i >= 0 {
				take = chunk[:i]
			}
			if total+len(take)+1 > limit {
				t.Truncated = true
				break
			}
			partial = append(partial, take...)
			if i < 0 {
				total += len(take)
				break
			}
			total += len(take) + 1
			t.Lines = append(t.Lines, Line{OffsetNS: offset(), Data: bytes.TrimSuffix(partial, []byte{'\r'})})
			partial = nil
			chunk = chunk[i+1:]
		}
		if err != nil {
			if len(partial) > 0 && !t.Truncated {
				t.Lines = append(t.Lines, Line{OffsetNS: offset(), Data: partial})
			}
			return t
		}
	}
}

func absentErr(err error) bool {
	return errors.Is(err, fs.ErrNotExist) || errors.Is(err, exec.ErrNotFound)
}

// substitute replaces placeholders in s; esc JSON-string-escapes values.
func substitute(s string, vals map[string]string, esc bool) string {
	for k, v := range vals {
		if esc {
			q := quoteJSON(v)
			v = q[1 : len(q)-1]
		}
		s = strings.ReplaceAll(s, k, v)
	}
	return s
}

// minProgress gates a progress case's result on one delivered progress
// notification, so a completed progress case always carries progress
// evidence (C5) whatever the scheduling.
func minProgress(spec caseSpec) int {
	if spec.interval > 0 && spec.delay > 0 {
		return 1
	}
	return 0
}

// Prompt is the scripted instruction for one case: call slow once with
// the exact case ID, report its result or error, and stop.
func Prompt(caseID string) string {
	return fmt.Sprintf("Call the MCP tool slow exactly once with arguments {\"case_id\": %q}. Do not call any other tool. "+
		"Report the tool's result or error verbatim, then stop.", caseID)
}

// runCase launches one scripted session for spec and classifies it.
func (m *measure) runCase(ctx context.Context, spec caseSpec) CaseReport {
	r, pc := m.r, m.pc
	m.sessions++
	cs := CaseReport{CaseID: spec.id, Setting: spec.setting, DelayMS: spec.delay, ProgressIntervalMS: spec.interval, Events: []Event{}}
	fail := func(outcome, reason string) CaseReport {
		cs.Outcome, cs.Reason = outcome, sptr(reason)
		return cs
	}
	ws := filepath.Join(r.OutDir, workDir, spec.id)
	recipe := pc.Config.Default
	if spec.setting == "raised" {
		recipe = *pc.Config.Raised
	}
	cf := CaseFile{Version: 1, RunID: r.RunID, Nonce: r.Nonce, Cases: []ProbeCase{
		{CaseID: spec.id, DelayMS: spec.delay, ProgressIntervalMS: spec.interval, MinProgress: minProgress(spec)}, {CaseID: spec.id + "-marker"}}}
	cfb, _ := encodeIndent(cf)
	casePath, eventsPath, cfgPath := filepath.Join(ws, "case.json"), filepath.Join(ws, "server-events.jsonl"), filepath.Join(ws, filepath.FromSlash(recipe.Path))
	cfgVals := map[string]string{"{server}": r.ServerPath, "{case_file}": casePath, "{events}": eventsPath, "{workspace}": ws}
	if err := os.MkdirAll(filepath.Dir(cfgPath), 0o700); err != nil {
		return fail(OutcomeInconclusive, ReasonConfigFailure+": "+err.Error())
	}
	if err := os.WriteFile(casePath, cfb, 0o600); err != nil {
		return fail(OutcomeInconclusive, ReasonConfigFailure+": "+err.Error())
	}
	if err := os.WriteFile(cfgPath, []byte(substitute(recipe.Content, cfgVals, true)), 0o600); err != nil {
		return fail(OutcomeInconclusive, ReasonConfigFailure+": "+err.Error())
	}
	argVals := map[string]string{"{prompt}": Prompt(spec.id), "{workspace}": ws, "{config}": cfgPath, "{server}": r.ServerPath, "{case}": spec.id}
	var args []string
	for _, a := range append(append([]string(nil), pc.Session.Argv...), recipe.Argv...) {
		args = append(args, substitute(a, argVals, false))
	}
	env := append([]string(nil), r.BaseEnv...)
	for _, src := range []map[string]string{pc.Env, recipe.Env} {
		for _, k := range sortedKeys(src) {
			env = append(env, k+"="+substitute(src[k], argVals, false))
		}
	}
	wd, capped := r.allowance(time.Duration(r.Plan.EffectiveLimits().MaxCaseMS)*time.Millisecond, m.deadline)
	if wd <= 0 {
		return fail(OutcomeInconclusive, ReasonClientBudget)
	}
	run := r.launch(ctx, ProcSpec{Path: pc.Executable, Args: args, Env: env, Dir: ws, StderrPath: filepath.Join(ws, "vendor-stderr.txt")}, wd, r.stdoutLimit())
	run.budgetCapped = capped
	run.probeRaw, _ = os.ReadFile(eventsPath)
	m.lastEvents = nil
	run.stderr, _ = os.ReadFile(filepath.Join(ws, "vendor-stderr.txt"))
	cs.Cleanup = run.cleanup
	cs.Exit, cs.Signal = run.exit, run.signal
	m.r.recordCleanup(spec.id, run.cleanup)
	if run.launchErr != nil {
		reason := ReasonLaunchFailed
		if absentErr(run.launchErr) {
			reason = ReasonAbsentBinary
		}
		return fail(OutcomeInconclusive, reason)
	}
	dir := path.Join("cases", spec.id)
	if len(run.probeRaw) > 0 {
		rel, cut := r.putEvidence(path.Join(dir, "server-events.jsonl"), run.probeRaw, evidenceJSONL)
		cs.ServerEvents, run.evidenceCut = sptr(rel), cut
	}
	rel, cut := r.putEvidence(path.Join(dir, "vendor-events.jsonl"), r.transcriptJSONL(run.transcript), evidenceSanitized)
	cs.VendorEvents, run.evidenceCut = sptr(rel), run.evidenceCut || cut
	if len(run.stderr) > 0 {
		r.writeAux(path.Join(dir, "vendor-stderr.txt"), run.stderr)
	}
	m.lastEvents = classify(&cs, r.Nonce, m.decode(run.transcript), run)
	return cs
}

func (r *Runner) stdoutLimit() int {
	if r.StdoutLimit > 0 {
		return r.StdoutLimit
	}
	return MaxEvidenceFileBytes
}

// transcriptJSONL renders the vendor transcript as JSONL evidence lines
// {offset_ns, data}, each line sanitized before it is wrapped in its JSON
// string (so escaping can never hide a credential from redaction). Lines
// the redactor omits leave only their count.
func (r *Runner) transcriptJSONL(t Transcript) []byte {
	var buf bytes.Buffer
	omitted := 0
	for _, l := range t.Lines {
		data, ok := r.redactor.TranscriptLine(l.Data)
		if !ok {
			omitted++
			continue
		}
		b, _ := encodeJSON(struct {
			OffsetNS int64  `json:"offset_ns"`
			Data     string `json:"data"`
		}{l.OffsetNS, string(data)})
		buf.Write(b)
		buf.WriteByte('\n')
	}
	if omitted > 0 {
		fmt.Fprintf(&buf, `{"omitted_lines":%d}`+"\n", omitted)
	}
	if t.Truncated {
		buf.WriteString(`{"truncated":true}` + "\n")
	}
	return buf.Bytes()
}

// classify decides one case from the typed vendor events and the probe's
// own record. A timeout needs the vendor's typed, case-correlated timeout
// event; cancellation, EOF, process death or the watchdog alone never is
// one. Intervals come from the probe clock only.
func classify(cs *CaseReport, nonce string, dec Decoded, run caseRun) []ProbeEvent {
	set := func(outcome, reason string) {
		cs.Outcome = outcome
		if reason != "" {
			cs.Reason = sptr(reason)
		}
	}
	for _, ev := range dec.Events {
		if (ev.CaseID == cs.CaseID || ev.CaseID == cs.CaseID+"-marker" || ev.CaseID == "") && len(cs.Events) < maxReportEvents {
			cs.Events = append(cs.Events, ev)
		}
	}
	evs, _, perr := parseEventsIfAny(run.probeRaw)
	probe := probeView(evs, cs.CaseID)
	if probe.receipt != nil {
		cs.ProgressTokenPresent = probe.receipt.TokenPresent
		cs.StartOffsetNS = iptr(probe.receipt.OffsetNS)
	}
	cs.ProgressSent = probe.progress
	if probe.end != nil {
		cs.EndOffsetNS = iptr(probe.end.OffsetNS)
		if probe.receipt != nil {
			cs.ElapsedMS = iptr((probe.end.OffsetNS - probe.receipt.OffsetNS) / int64(time.Millisecond))
		}
	}
	calls, others := 0, 0
	var outcome *Event
	var terminal *Event
	for i := range dec.Events {
		ev := &dec.Events[i]
		switch {
		case ev.Kind == KindToolCall && ev.CaseID == cs.CaseID:
			calls++
		case ev.Kind == KindToolCall && ev.CaseID != cs.CaseID+"-marker":
			others++
		case ev.CaseID == cs.CaseID && outcome == nil && ev.Kind != KindToolCall:
			outcome = ev
		case ev.CaseID == "" && ev.Kind != KindToolCall && terminal == nil:
			terminal = ev
		}
	}
	switch {
	case run.interrupted:
		set(OutcomeInterrupted, ReasonInterrupted)
	case run.cleanup.Error != nil:
		set(OutcomeInconclusive, ReasonCleanupFailed)
	case run.evidenceCut:
		set(OutcomeInconclusive, ReasonEvidenceTruncated)
	case run.watchdog && outcome == nil && run.budgetCapped:
		set(OutcomeInconclusive, ReasonClientBudget)
	case run.watchdog && outcome == nil:
		set(OutcomeInconclusive, ReasonSessionTimeout)
	case dec.Inconclusive != "":
		set(OutcomeInconclusive, dec.Inconclusive)
	case terminal != nil && terminal.Kind == KindAuthError:
		set(KindAuthError, ReasonAuth)
	case terminal != nil && terminal.Kind == KindSessionError && terminal.SafeReason == SafeModelUnavailable:
		set(KindSessionError, ReasonModelUnavailable)
	case terminal != nil && terminal.Kind == KindPermissionDenied, outcome != nil && outcome.Kind == KindPermissionDenied:
		set(KindPermissionDenied, ReasonPermission)
	case calls > 1 || others > 0 || probe.rejected:
		set(OutcomeInconclusive, ReasonExtraCalls)
	case calls == 0 && terminal != nil && terminal.Kind == KindModelRefusal:
		set(KindModelRefusal, ReasonRefusal)
	case calls == 0 && terminal != nil && terminal.Kind == KindSessionError:
		set(KindSessionError, ReasonSessionError)
	case calls == 0:
		set(OutcomeInconclusive, ReasonNoCall)
	case perr != nil:
		set(OutcomeInconclusive, ReasonProbeLog+": "+perr.Error())
	case probe.fatal:
		set(OutcomeInconclusive, ReasonProbeFatal)
	case outcome == nil:
		set(OutcomeInconclusive, ReasonNoOutcome)
	case probe.receipt == nil:
		set(OutcomeInconclusive, ReasonProbeMissing)
	case outcome.Kind == KindToolResult:
		if outcome.Nonce != nonce || outcome.SafeReason != "" {
			set(OutcomeInconclusive, ReasonNonce)
			return evs
		}
		if !probe.completed {
			set(OutcomeInconclusive, ReasonProbeEndpoint)
			return evs
		}
		set(KindToolResult, "")
	case outcome.Kind == KindMCPTimeout:
		reason := ""
		if cs.ElapsedMS == nil {
			reason = ReasonProbeEndpoint
		}
		set(KindMCPTimeout, reason)
	case outcome.Kind == KindToolError:
		set(OutcomeInconclusive, ReasonToolError)
	default:
		set(OutcomeInconclusive, ReasonNoOutcome)
	}
	if run.stdoutHeld && cs.Outcome != OutcomeInterrupted && cs.Reason == nil {
		cs.Reason = sptr(ReasonStdoutHeld)
	}
	return evs
}

// parseEventsIfAny parses a probe events file; an absent file has none.
func parseEventsIfAny(raw []byte) ([]ProbeEvent, bool, error) {
	if len(raw) == 0 {
		return nil, false, nil
	}
	return ParseProbeEvents(raw)
}

// maxReportEvents bounds the decoded events a case keeps in report.json
// (the complete sanitized transcript is the vendor evidence file).
const maxReportEvents = 64

type probeSummary struct {
	receipt   *ProbeEvent
	end       *ProbeEvent
	completed bool
	progress  int
	rejected  bool
	fatal     bool
}

// probeView extracts the case's receipt, its terminal endpoint (completed,
// cancelled or EOF), progress count, rejections and fatal errors.
func probeView(evs []ProbeEvent, caseID string) probeSummary {
	var s probeSummary
	instance, receiptInstance := 0, -1
	for i := range evs {
		ev := &evs[i]
		switch ev.Kind {
		case EvStart:
			instance++
		case EvReceipt:
			if ev.CaseID == caseID && s.receipt == nil {
				s.receipt, receiptInstance = ev, instance
			}
		case EvProgress:
			if s.receipt != nil && s.end == nil && ev.CaseID == caseID && instance == receiptInstance && string(ev.RequestID) == string(s.receipt.RequestID) {
				s.progress++
			}
		case EvCompleted, EvCancelled, EvEOF:
			if s.receipt != nil && s.end == nil && ev.CaseID == caseID && instance == receiptInstance {
				s.end = ev
				s.completed = ev.Kind == EvCompleted
			}
		case EvRejected:
			s.rejected = true
		case EvError, EvWriteFail:
			s.fatal = true
		}
	}
	return s
}

func sortedKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
