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
	ReasonDecoderFixture     = "decoder_fixture_mismatch"
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
	// stderrCut: a captured stderr pipe reached its retention limit (the
	// rest was drained, not stored); stderrHeld: it stayed open after
	// cleanup until the post-cleanup deadline.
	stderrCut  bool
	stderrHeld bool
	// probeCut: the probe events file exceeded its read bound.
	probeCut bool
	// budgetCapped: the watchdog was the client's remaining wall time.
	budgetCapped bool
	// evidenceCut: a sanitized evidence file reached its limit; serverCut:
	// the probe events (read or evidence file) were cut; omitted: transcript
	// lines the redaction omitted whole (design decoder-enrollment B1.5,
	// FP-14: each withdraws the clean-session attestation).
	evidenceCut bool
	serverCut   bool
	omitted     int
	// serverRel and vendorRel are the written evidence files (measurement
	// layout); writeErr is the first evidence write failure, observable to
	// every caller (design decoder-enrollment, Capture evidence).
	serverRel, vendorRel *string
	writeErr             error
}

// caseInputs is one prepared case (design decoder-enrollment, Capture
// interface): its specification, the substituted launch and the case
// workspace's case, configuration and probe events paths. evidence writes
// the sanitized files of the run in its caller's layout.
type caseInputs struct {
	spec                                 caseSpec
	proc                                 ProcSpec
	ws, casePath, configPath, eventsPath string
	recipe                               ConfigRecipe
	evidence                             caseEvidence
}

// caseEvidence writes one executed case's sanitized evidence, recording
// what it wrote, any cut and any write failure in run.
type caseEvidence interface {
	write(r *Runner, in caseInputs, run *caseRun)
}

// launch runs one process to completion or its watchdog (or ctx), then
// always reaps its group. Stdout is captured bounded, each line stamped
// with the harness clock's offset from launch; a CaptureStderr launch's
// stderr pipe is drained concurrently into a bounded collector. After
// cleanup both readers get the same bounded wait, and both read ends are
// closed either way.
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
	var outEnd error
	go func() {
		defer close(readerDone)
		t, outEnd = readLinesEnd(p.Stdout(), limit, func() int64 { return int64(r.Clock.Now().Sub(start)) })
	}()
	sp, _ := p.(StderrProc)
	if !spec.CaptureStderr {
		sp = nil
	}
	var errDone chan struct{}
	var errOut []byte
	var errCut bool
	var errEnd error
	if sp != nil {
		errDone = make(chan struct{})
		go func() {
			defer close(errDone)
			errOut, errCut, errEnd = readBoundedStream(sp.Stderr(), limit)
		}()
	}
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
	if sp == nil {
		cr.stdoutHeld = !awaitStdout(r.Clock, p, readerDone, r.stdoutWait())
	} else {
		held := awaitReaders(r.Clock, r.stdoutWait(), []<-chan struct{}{readerDone, errDone}, []func(){p.CloseStdout, sp.CloseStderr},
			[]func() bool{func() bool { return outEnd == io.EOF }, func() bool { return errEnd == io.EOF }})
		cr.stdoutHeld, cr.stderrHeld = held[0], held[1]
		cr.stderr, cr.stderrCut = errOut, errCut
	}
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

// awaitReaders waits, within one bounded post-cleanup deadline, for every
// reader to finish, then closes every read end (which ends any reader
// still blocked) and waits for all of them. A reader is held when it was
// still running at the deadline and did not end at its stream's natural
// EOF: the verdict comes from each reader's own end, never from which of
// two ready channels a select took.
func awaitReaders(clock Clock, limit time.Duration, done []<-chan struct{}, closers []func(), natural []func() bool) []bool {
	all := make(chan struct{})
	go func() {
		for _, d := range done {
			<-d
		}
		close(all)
	}()
	tc, stop := clock.NewTimer(limit)
	select {
	case <-all:
	case <-tc:
	}
	stop()
	early := make([]bool, len(done))
	for i, d := range done {
		select {
		case <-d:
			early[i] = true
		default:
		}
	}
	for _, c := range closers {
		c()
	}
	<-all
	held := make([]bool, len(done))
	for i := range done {
		held[i] = !early[i] && !natural[i]()
	}
	return held
}

// readBoundedStream reads rd to its end, keeping at most limit bytes and
// draining (never storing) the rest; cut reports that bytes were dropped,
// and end is the read's terminal error (io.EOF at the natural end).
func readBoundedStream(rd io.Reader, limit int) ([]byte, bool, error) {
	var out []byte
	cut := false
	buf := make([]byte, 32<<10)
	for {
		n, err := rd.Read(buf)
		keep := min(n, limit-len(out))
		out = append(out, buf[:keep]...)
		cut = cut || keep < n
		if err != nil {
			return out, cut, err
		}
	}
}

// readLines reads r to its end, splitting LF-terminated lines (a CR before
// LF is dropped). After limit bytes it marks the transcript truncated and
// keeps draining without storing.
func readLines(rd io.Reader, limit int, offset func() int64) Transcript {
	t, _ := readLinesEnd(rd, limit, offset)
	return t
}

// readLinesEnd is readLines also returning the read's terminal error
// (io.EOF at the natural end).
func readLinesEnd(rd io.Reader, limit int, offset func() int64) (Transcript, error) {
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
			return t, err
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
func Prompt(caseID string) string { return promptFor("slow", caseID) }

// GrokProbeTool is the qualified name Grok gives the probe's slow tool
// (server__tool; design decoder-enrollment B1, FP-12).
const GrokProbeTool = "probe__slow"

// promptForClient is the scripted instruction a client's session gets:
// Prompt, except that Grok names its qualified tool. The tool name is a
// parameter of the text, never a replacement over the case ID.
func promptForClient(clientID, caseID string) string {
	if clientID == "grok" {
		return promptFor(GrokProbeTool, caseID)
	}
	return Prompt(caseID)
}

// PromptForClient is promptForClient for the function tests' fake vendor
// expectations.
func PromptForClient(clientID, caseID string) string { return promptForClient(clientID, caseID) }

func promptFor(tool, caseID string) string {
	return fmt.Sprintf("Call the MCP tool %s exactly once with arguments {\"case_id\": %q}. Do not call any other tool. "+
		"Report the tool's result or error verbatim, then stop.", tool, caseID)
}

// prepareCase writes the case workspace of spec for client pc (its case
// file, naming the case and its marker, and its rendered probe
// configuration) and substitutes the session launch: the plan's session
// argv, then the configuration's, the inherited environment plus the
// plan's and the configuration's, the workspace as working directory and
// the measurement's stderr file. No shell interprets anything.
func (r *Runner) prepareCase(pc *PlanClient, spec caseSpec) (caseInputs, error) {
	ws := filepath.Join(r.OutDir, workDir, spec.id)
	recipe := pc.Config.Default
	if spec.setting == "raised" {
		recipe = *pc.Config.Raised
	}
	in := caseInputs{spec: spec, ws: ws, recipe: recipe, casePath: filepath.Join(ws, "case.json"), eventsPath: filepath.Join(ws, "server-events.jsonl"),
		configPath: filepath.Join(ws, filepath.FromSlash(recipe.Path))}
	cf := CaseFile{Version: 1, RunID: r.RunID, Nonce: r.Nonce, Cases: []ProbeCase{
		{CaseID: spec.id, DelayMS: spec.delay, ProgressIntervalMS: spec.interval, MinProgress: minProgress(spec)}, {CaseID: spec.id + "-marker"}}}
	cfb, _ := encodeIndent(cf)
	cfgVals := map[string]string{"{server}": r.ServerPath, "{case_file}": in.casePath, "{events}": in.eventsPath, "{workspace}": ws}
	if err := os.MkdirAll(filepath.Dir(in.configPath), 0o700); err != nil {
		return in, err
	}
	if err := os.WriteFile(in.casePath, cfb, 0o600); err != nil {
		return in, err
	}
	if err := os.WriteFile(in.configPath, []byte(substitute(recipe.Content, cfgVals, true)), 0o600); err != nil {
		return in, err
	}
	argVals := map[string]string{"{prompt}": promptForClient(pc.ID, spec.id), "{workspace}": ws, "{config}": in.configPath, "{server}": r.ServerPath, "{case}": spec.id}
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
	in.proc = ProcSpec{Path: pc.Executable, Args: args, Env: env, Dir: ws, StderrPath: filepath.Join(ws, "vendor-stderr.txt")}
	return in, nil
}

// executeCase launches a prepared case under watchdog, cleans it up,
// collects its bounded streams and probe events (the events file read to
// at most its limit plus one byte) and writes the sanitized evidence
// through in.evidence. Nothing here decodes the vendor transcript.
func (r *Runner) executeCase(ctx context.Context, in caseInputs, watchdog time.Duration) caseRun {
	run := r.launch(ctx, in.proc, watchdog, r.stdoutLimit())
	limit := r.evidenceLimit()
	run.probeRaw, run.probeCut = readFileBounded(in.eventsPath, limit)
	if !in.proc.CaptureStderr && in.proc.StderrPath != "" {
		run.stderr, _ = os.ReadFile(in.proc.StderrPath)
	}
	if run.launchErr == nil && in.evidence != nil {
		in.evidence.write(r, in, &run)
	}
	return run
}

// readFileBounded reads at most limit+1 bytes of p (an absent file is
// empty); cut reports a file over limit, whose first limit+1 bytes are
// returned.
func readFileBounded(p string, limit int) ([]byte, bool) {
	f, err := os.Open(p)
	if err != nil {
		return nil, false
	}
	defer f.Close()
	b, _ := io.ReadAll(io.LimitReader(f, int64(limit)+1))
	return b, len(b) > limit
}

// measureEvidence is the qualification layout: cases/<id>/ holds the
// probe events (when any), the vendor transcript and a diagnostic stderr.
type measureEvidence struct{}

func (measureEvidence) write(r *Runner, in caseInputs, run *caseRun) {
	dir := path.Join("cases", in.spec.id)
	if len(run.probeRaw) > 0 {
		raw := run.probeRaw
		if run.probeCut {
			// A probe log over its read bound keeps its complete lines and
			// always ends in the truncation marker, so no replay of the
			// retained bytes can see an unbroken log (FP-14).
			raw = append(append([]byte(nil), raw[:bytes.LastIndexByte(raw, '\n')+1]...), `{"truncated":true}`+"\n"...)
		}
		rel, cut, err := r.putEvidence(path.Join(dir, "server-events.jsonl"), raw, evidenceJSONL)
		run.serverRel, run.evidenceCut, run.serverCut, run.writeErr = sptr(rel), cut || run.probeCut, cut || run.probeCut, err
	}
	vendor, omitted := transcriptLines(r.redactor, run.transcript)
	rel, cut, err := r.putEvidence(path.Join(dir, "vendor-events.jsonl"), vendor, evidenceSanitized)
	run.vendorRel, run.evidenceCut, run.omitted = sptr(rel), run.evidenceCut || cut, omitted
	if run.writeErr == nil {
		run.writeErr = err
	}
	if len(run.stderr) > 0 {
		r.writeAux(path.Join(dir, "vendor-stderr.txt"), run.stderr)
	}
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
	in, err := r.prepareCase(pc, spec)
	if err != nil {
		return fail(OutcomeInconclusive, ReasonConfigFailure+": "+err.Error())
	}
	in.evidence = measureEvidence{}
	wd, capped := r.allowance(time.Duration(r.Plan.EffectiveLimits().MaxCaseMS)*time.Millisecond, m.deadline)
	if wd <= 0 {
		return fail(OutcomeInconclusive, ReasonClientBudget)
	}
	m.lastEvents = nil
	run := r.executeCase(ctx, in, wd)
	run.budgetCapped = capped
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
	cs.ServerEvents, cs.VendorEvents = run.serverRel, run.vendorRel
	m.lastEvents = classify(&cs, r.Nonce, m.decode(run.transcript), run)
	m.checkCapability(&cs)
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
	b, _ := transcriptLines(r.redactor, t)
	return b
}

// transcriptLines is transcriptJSONL under red, also returning the number
// of lines omitted whole (the {"omitted_lines":N} marker's N).
func transcriptLines(red *Redactor, t Transcript) ([]byte, int) {
	var buf bytes.Buffer
	omitted := 0
	for _, l := range t.Lines {
		data, ok := red.TranscriptLine(l.Data)
		if !ok {
			omitted++
			continue
		}
		buf.Write(wrapLine(l.OffsetNS, data))
		buf.WriteByte('\n')
	}
	if omitted > 0 {
		fmt.Fprintf(&buf, `{"omitted_lines":%d}`+"\n", omitted)
	}
	if t.Truncated {
		buf.WriteString(`{"truncated":true}` + "\n")
	}
	return buf.Bytes(), omitted
}

// transcriptWrapper is one vendor-events.jsonl line: a transcript line's
// recorded monotonic offset and its sanitized bytes.
type transcriptWrapper struct {
	OffsetNS int64  `json:"offset_ns"`
	Data     string `json:"data"`
}

func wrapLine(off int64, data []byte) []byte {
	b, _ := encodeJSON(transcriptWrapper{off, string(data)})
	return b
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
	// The shared analyzer (design decoder-enrollment B1.5, FP-14): the same
	// endpoints and observation as capture, with the runner's own
	// clean-session facts.
	an := analyzeProbe(run.probeRaw, cs.CaseID, run.probeCut || run.serverCut, run.cleanRun())
	evs, perr, probe := an.events, an.err, an.view
	obs := an.obs
	cs.ProbeObservation = &obs
	// A conclusive probe-derived outcome needs the shared observation's
	// accepted end state: an intact log without any anomaly, or the clean
	// terminal observation without exit (code review B1.5 round 1, C1).
	endpoint := obs.observedEndpoint()
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
		if !endpoint {
			set(OutcomeInconclusive, ReasonProbeIncomplete)
			return evs
		}
		if !probe.completed {
			set(OutcomeInconclusive, ReasonProbeEndpoint)
			return evs
		}
		set(KindToolResult, "")
	case outcome.Kind == KindMCPTimeout && (!obs.CleanSession || !endpoint):
		// A typed timeout is conclusive only from a clean session (exit 0
		// and every other clean-session condition), intact log or not; a
		// nonzero teardown keeps its evidence and stays inconclusive, never
		// incompatible or a pass (design decoder-enrollment B1.5, DW1).
		set(OutcomeInconclusive, ReasonProbeIncomplete)
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
