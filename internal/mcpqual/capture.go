package mcpqual

// Decoder-free setup capture (design decoder-enrollment, FP-1..FP-4): for
// each planned client in turn, the exact version and help commands, then
// one zero-delay setup session against the probe with the plan's exact
// session argv and configuration, recorded as a bounded, redacted,
// hash-addressed bundle. Capture never selects, consults or runs a
// decoder, never evaluates vendor behavior and never launches a second
// model session. It reuses qualification's launch, cleanup proof and case
// preparation (prepareCase/executeCase).

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"time"

	"github.com/wedevwork/callsheet/internal/contract"
)

// Capture caps (design decoder-enrollment, Capture interface): plan limits
// may lower them per dimension, never raise them.
const (
	CaptureMaxClients        = 4
	CaptureSessionsPerClient = 1
	CaptureMaxSessionMS      = 120_000
	CaptureMaxClientMS       = 180_000
)

// CaptureLimits are capture's byte bounds. Tests inject small values
// (following Runner.StdoutLimit); production uses DefaultCaptureLimits.
type CaptureLimits struct {
	// Stream bounds the raw stdout and the raw stderr retained per
	// invocation (the rest is drained, not stored).
	Stream int
	// ServerEvents bounds the probe events file read.
	ServerEvents int
	// File bounds each final payload file (after escaping, wrappers and
	// redaction expansion).
	File int
	// Client and Bundle bound one client's payloads and all payloads.
	Client, Bundle int
	// Manifest bounds manifest.json.
	Manifest int
}

// DefaultCaptureLimits are the production bounds: 8 MiB streams, events
// files, payload files and manifest; 64 MiB per client; 256 MiB per bundle.
func DefaultCaptureLimits() CaptureLimits {
	return CaptureLimits{Stream: MaxEvidenceFileBytes, ServerEvents: MaxEvidenceFileBytes, File: MaxEvidenceFileBytes,
		Client: 64 << 20, Bundle: 256 << 20, Manifest: MaxEvidenceFileBytes}
}

// orDefault fills every unset (nonpositive) bound with its default and
// lowers any bound above its default to the default.
func (l CaptureLimits) orDefault() CaptureLimits {
	d := DefaultCaptureLimits()
	pick := func(v, def int) int {
		if v <= 0 || v > def {
			return def
		}
		return v
	}
	return CaptureLimits{Stream: pick(l.Stream, d.Stream), ServerEvents: pick(l.ServerEvents, d.ServerEvents), File: pick(l.File, d.File),
		Client: pick(l.Client, d.Client), Bundle: pick(l.Bundle, d.Bundle), Manifest: pick(l.Manifest, d.Manifest)}
}

// CaptureRunner captures one validated capture plan (ParseCapturePlan).
type CaptureRunner struct {
	Plan   *Plan
	OutDir string
	// GOOS and GOARCH come from Env (never the runtime) and label the
	// manifest.
	GOOS, GOARCH string
	// ServerPath is the absolute mcpqual executable for {server}.
	ServerPath string
	// BaseEnv is the vendor processes' inherited environment; it is never
	// serialized, and its credential-named values are redacted.
	BaseEnv        []string
	Launcher       Launcher
	Reaper         Reaper
	Clock          Clock
	Log            io.Writer
	RunID, Nonce   string
	CapturedAt     time.Time
	HarnessVersion string
	// Home and User are redacted from evidence.
	Home, User string
	// HashFile hashes an executable; nil uses the file's bytes.
	HashFile func(string) (string, error)
	// Limits are the byte bounds (zero fields: DefaultCaptureLimits).
	Limits CaptureLimits
	// StreamWait bounds the wait for both streams after cleanup (default
	// 2 s).
	StreamWait time.Duration

	r       *Runner
	lim     CaptureLimits
	w       *captureWriter
	caseMS  int64
	client  int64
	meta    int64
	ioError error
}

// errCaptureIO marks a filesystem failure of the capture (exit 1).
var errCaptureIO = errors.New("capture filesystem failure")

// EffectiveLimits are the run's limits: min(plan limit, capture cap).
func (c *CaptureRunner) EffectiveLimits() CaptureLimitsRecord {
	pl := c.Plan.EffectiveLimits()
	caseMS := min(pl.MaxCaseMS, CaptureMaxSessionMS)
	lim := c.Limits.orDefault()
	return CaptureLimitsRecord{Clients: len(c.Plan.Clients), SessionsPerClient: min(pl.MaxSessionsPerClient, CaptureSessionsPerClient),
		SessionMS: caseMS, ClientMS: min(pl.MaxClientMS, CaptureMaxClientMS), MetadataMS: min(int64(versionWatchdog/time.Millisecond), caseMS),
		StreamBytes: lim.Stream, ServerEventsBytes: lim.ServerEvents, FileBytes: lim.File, ClientBytes: lim.Client, BundleBytes: lim.Bundle, ManifestBytes: lim.Manifest}
}

// ExitCode maps a finished capture: 0 complete, 130 interrupted, 5
// (unavailable) partial.
func (m *CaptureManifest) ExitCode() int {
	switch m.State {
	case CaptureComplete:
		return contract.ExitOK
	case CaptureInterrupted:
		return contract.ExitInterrupted
	}
	return contract.ExitCode(contract.New(contract.CodeUnavailable, "partial"))
}

// Run captures every planned client, sequentially, and writes the bundle
// with its manifest last (temporary file and rename). It returns an error
// only for an unusable output directory (invalid_argument) or a
// filesystem failure, in which case no complete manifest exists. The
// disposable workspace is removed on every path after cleanup.
func (c *CaptureRunner) Run(ctx context.Context) (*CaptureManifest, error) {
	if err := prepareOutDir(c.OutDir); err != nil {
		return nil, err
	}
	defer os.RemoveAll(filepath.Join(c.OutDir, workDir))
	c.lim = c.Limits.orDefault()
	eff := c.EffectiveLimits()
	c.caseMS, c.client, c.meta = eff.SessionMS, eff.ClientMS, eff.MetadataMS
	c.r = &Runner{Plan: c.Plan, OutDir: c.OutDir, GOOS: c.GOOS, GOARCH: c.GOARCH, ServerPath: c.ServerPath, BaseEnv: c.BaseEnv, Launcher: c.Launcher,
		Reaper: c.Reaper, Clock: c.Clock, Log: c.Log, RunID: c.RunID, Nonce: c.Nonce, HarnessVersion: c.HarnessVersion, Home: c.Home, User: c.User,
		HashFile: c.HashFile, StdoutLimit: c.lim.Stream, StdoutWait: c.StreamWait, EvidenceLimit: c.lim.ServerEvents}
	c.r.redactor = c.redactor()
	c.r.cleanup = CleanupReport{OK: true, Failures: []string{}}
	c.w = &captureWriter{root: c.OutDir, lim: c.lim, perClient: map[string]int{}}
	fmt.Fprintf(c.Log, "mcpqual: capture run %s: setup only; at most %d clients sequentially, %d session per client, %d ms per session, %d ms per client including version and help "+
		"(each metadata command at most %d ms); planned operational deadline %d ms plus bounded cleanup of up to %d process groups\n",
		c.RunID, eff.Clients, eff.SessionsPerClient, eff.SessionMS, eff.ClientMS, eff.MetadataMS, int64(eff.Clients)*eff.ClientMS, 3*eff.Clients)
	if c.Plan.HasIgnoredDefault() {
		fmt.Fprintln(c.Log, "mcpqual: setup only; default phase not executed")
	}
	man := &CaptureManifest{Schema: CaptureSchema, RunID: c.RunID, CapturedAt: c.CapturedAt.UTC().Format(time.RFC3339), HarnessVersion: c.HarnessVersion,
		OS: c.GOOS, Arch: c.GOARCH, Limits: eff, RedactionPolicy: CaptureRedactionPolicy, VendorBehavior: VendorNotEvaluated}
	planBytes, err := c.sanitizedPlan()
	if err != nil {
		return nil, err
	}
	ref, planCut, err := c.w.put("", CapturePlanName, planBytes, false)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", errCaptureIO, err)
	}
	man.PlanSHA256 = ref.SHA256
	man.Plan = CaptureStream{Path: CapturePlanName, OutputTruncated: planCut}
	for i := range c.Plan.Clients {
		pc := &c.Plan.Clients[i]
		var cc CaptureClient
		switch {
		case ctx.Err() != nil:
			cc = c.skipped(pc, ReasonInterrupted)
		case !c.r.cleanupOK():
			cc = c.skipped(pc, ReasonCleanupFailed)
		default:
			cc = c.captureClient(ctx, pc)
		}
		if c.ioError != nil {
			return nil, fmt.Errorf("%w: %v", errCaptureIO, c.ioError)
		}
		man.Clients = append(man.Clients, cc)
	}
	man.Cleanup = c.r.cleanupReport()
	man.State = CaptureComplete
	partial := func(reason string) {
		if man.Reason == nil {
			man.State, man.Reason = CapturePartial, sptr(reason)
		}
	}
	if ctx.Err() != nil {
		man.State, man.Reason = CaptureInterrupted, sptr(ReasonInterrupted)
	}
	if !man.Cleanup.OK {
		partial(ReasonCleanupFailed)
	}
	// A cut plan.json (redaction expansion past its bound) is never a
	// complete capture (code review C4).
	if planCut {
		partial(ReasonEvidenceTruncated + ": " + CapturePlanName)
	}
	for _, cc := range man.Clients {
		if cc.State != CaptureComplete {
			partial("client " + cc.ID + " is " + cc.State)
		}
	}
	man.Files = c.w.sorted()
	if err := os.RemoveAll(filepath.Join(c.OutDir, workDir)); err != nil {
		return nil, fmt.Errorf("%w: removing the workspace: %v", errCaptureIO, err)
	}
	if err := c.finalize(man); err != nil {
		return nil, fmt.Errorf("%w: %v", errCaptureIO, err)
	}
	return man, nil
}

// redactor is the capture policy over every known literal: credential-named
// values of the inherited and plan/config environments, the user name, and
// the home, output and workspace paths (each also in its symlink-resolved
// form).
func (c *CaptureRunner) redactor() *Redactor {
	secrets := CredentialValues(c.BaseEnv)
	for _, pc := range c.Plan.Clients {
		for _, env := range []map[string]string{pc.Env, pc.Config.Default.Env} {
			for k, v := range env {
				if secretName.MatchString(k) {
					secrets = append(secrets, v)
				}
			}
		}
	}
	if c.User != "" {
		secrets = append(secrets, c.User)
	}
	paths := map[string]string{}
	label := func(p, l string) {
		if len(p) > 1 {
			paths[p] = l
			if real, err := filepath.EvalSymlinks(p); err == nil && real != p {
				paths[real] = l
			}
		}
	}
	label(c.Home, "<home>")
	label(c.OutDir, "<out>")
	for _, pc := range c.Plan.Clients {
		for _, ws := range []string{pc.ID + "-capture-setup", pc.ID + "-version", pc.ID + "-help"} {
			paths[filepath.Join(c.OutDir, workDir, ws)] = "<workspace>"
		}
	}
	if real, err := filepath.EvalSymlinks(c.OutDir); err == nil && real != c.OutDir {
		for _, pc := range c.Plan.Clients {
			paths[filepath.Join(real, workDir, pc.ID+"-capture-setup")] = "<workspace>"
		}
	}
	return NewCaptureRedactor(secrets, paths)
}

// sanitizedPlan is the parsed input plan with every string redacted by
// its decoded content (and every credential-named environment value
// replaced), then encoded as one compact JSON line, whose strings are
// therefore already a fixed point of Redactor.Line.
func (c *CaptureRunner) sanitizedPlan() ([]byte, error) {
	red := c.r.redactor
	p := *c.Plan
	p.Clients = make([]PlanClient, len(c.Plan.Clients))
	env := func(m map[string]string) map[string]string {
		if m == nil {
			return nil
		}
		out := make(map[string]string, len(m))
		for k, v := range m {
			if secretMember.MatchString(k) {
				v = Redacted
			}
			out[red.String(k)] = red.String(v)
		}
		return out
	}
	for i, pc := range c.Plan.Clients {
		cp := pc
		cp.Env, cp.Config.Default.Env = env(pc.Env), env(pc.Config.Default.Env)
		cp.VersionArgv, cp.HelpArgv = slicesClone(pc.VersionArgv), slicesClone(pc.HelpArgv)
		cp.Session.Argv, cp.Config.Default.Argv = slicesClone(pc.Session.Argv), slicesClone(pc.Config.Default.Argv)
		red.Struct(reflect.ValueOf(&cp).Elem())
		p.Clients[i] = cp
	}
	b, err := encodeJSON(&p)
	if err != nil {
		return nil, err
	}
	return append(b, '\n'), nil
}

func slicesClone(xs []string) []string { return append([]string(nil), xs...) }

// newClient fills a client record's static fields.
func (c *CaptureRunner) newClient(pc *PlanClient) CaptureClient {
	caseID := pc.ID + "-capture-setup"
	red := c.r.redactor
	cc := CaptureClient{ID: pc.ID, ExpectedVersion: pc.ExpectedVersion, Decoder: pc.Decoder, DecoderFixture: red.String(pc.DecoderFixture),
		Model: red.String(pc.Model), CaseID: caseID, Nonce: c.Nonce, Streams: []CaptureStream{},
		Config:  CaptureConfig{Path: red.String(pc.Config.Default.Path), TimeoutOverride: TimeoutOverrideNone},
		Version: notRunStage("not reached"), Help: notRunStage("not reached"), Session: notRunStage("not reached"),
		ObservedVersionReason: sptr("the version command did not run"), ExecutableReason: sptr("not hashed"), ProbeReason: sptr("the session did not run")}
	if pc.Effort != "" {
		cc.Effort = sptr(red.String(pc.Effort))
	} else {
		cc.EffortReason = sptr("the plan names no effort")
	}
	if pre := pc.Config.Default.Prerequisite; pre != "" {
		cc.Config.Prerequisite = sptr(red.String(pre))
	}
	labels := map[string]string{"{prompt}": Prompt(caseID), "{workspace}": "<workspace>", "{config}": "<workspace>/" + pc.Config.Default.Path,
		"{server}": "<server>", "{case}": caseID}
	cc.Argv = []string{}
	for _, a := range append(append([]string(nil), pc.Session.Argv...), pc.Config.Default.Argv...) {
		cc.Argv = append(cc.Argv, red.String(substitute(a, labels, false)))
	}
	return cc
}

func notRunStage(reason string) CaptureStage {
	return CaptureStage{State: StageNotRun, Reason: sptr(reason)}
}

// skipped is a client none of whose stages ran.
func (c *CaptureRunner) skipped(pc *PlanClient, reason string) CaptureClient {
	cc := c.newClient(pc)
	cc.State, cc.Reason = CaptureNotRun, sptr(reason)
	cc.Version, cc.Help, cc.Session = notRunStage(reason), notRunStage(reason), notRunStage(reason)
	return cc
}

// stop ends a client as partial with reason.
func stop(cc CaptureClient, reason string) CaptureClient {
	cc.State, cc.Reason = CapturePartial, sptr(reason)
	return cc
}

// captureClient runs one client's version, help and setup session within
// its wall-time budget; a metadata failure or a different exact version
// prevents the session.
func (c *CaptureRunner) captureClient(ctx context.Context, pc *PlanClient) CaptureClient {
	cc := c.newClient(pc)
	deadline := c.Clock.Now().Add(time.Duration(c.client) * time.Millisecond)
	h, err := c.r.hashExecutable(pc.Executable)
	if err != nil {
		cc.ExecutableReason = sptr(ReasonAbsentBinary + ": the executable cannot be read")
		cc.Version.Reason = sptr(ReasonAbsentBinary)
		return stop(cc, ReasonAbsentBinary)
	}
	cc.ExecutableSHA256, cc.ExecutableReason = sptr(h), nil
	var out []byte
	var run caseRun
	cc.Version, out, run = c.metadata(ctx, pc, &cc, pc.VersionArgv, "version", FileVersionStdout, FileVersionStderr, deadline)
	if reason := metadataReason(cc.Version, run, "version"); reason != "" {
		if cc.Version.State == StageRan {
			cc.ObservedVersion, cc.ObservedVersionReason = sptr(c.r.redactor.String(firstLine(string(out)))), nil
		}
		return stop(cc, reason)
	}
	observed, single := observedVersion(string(out))
	cc.ObservedVersion, cc.ObservedVersionReason = sptr(c.r.redactor.String(firstLine(string(out)))), nil
	if !single || observed != pc.ExpectedVersion {
		cc.Help.Reason, cc.Session.Reason = sptr(ReasonVersionMismatch), sptr(ReasonVersionMismatch)
		return stop(cc, ReasonVersionMismatch)
	}
	cc.Help, _, run = c.metadata(ctx, pc, &cc, pc.HelpArgv, "help", FileHelpStdout, FileHelpStderr, deadline)
	if reason := metadataReason(cc.Help, run, "help"); reason != "" {
		cc.Session.Reason = sptr(reason)
		return stop(cc, reason)
	}
	return c.session(ctx, pc, cc, deadline)
}

// metadataReason is why a metadata stage prevents the session ("" when
// it succeeded): launch, cleanup, interruption, budget, stream and
// success checks in that order.
func metadataReason(st CaptureStage, run caseRun, name string) string {
	switch {
	case st.State == StageNotRun:
		return *st.Reason
	case run.launchErr != nil && absentErr(run.launchErr):
		return ReasonAbsentBinary
	case run.launchErr != nil:
		return ReasonLaunchFailed
	case run.cleanup.Error != nil:
		return ReasonCleanupFailed
	case run.interrupted:
		return ReasonInterrupted
	case run.stdoutHeld || run.stderrHeld:
		return ReasonStreamHeld + ": the " + name + " command"
	case run.watchdog && run.budgetCapped:
		return ReasonBudget + ": max_client_ms"
	case run.transcript.Truncated || run.stderrCut:
		return ReasonEvidenceTruncated + ": the " + name + " command"
	case !metadataOK(run):
		return ReasonMetadataFailed + ": the " + name + " command did not succeed"
	}
	return ""
}

// stageOf records a launched (or failed) process's stage.
func stageOf(run caseRun, watchdog time.Duration) CaptureStage {
	st := CaptureStage{State: StageRan, WatchdogMS: int64(watchdog / time.Millisecond), Exit: run.exit, Signal: run.signal, Watchdog: run.watchdog,
		Interrupted: run.interrupted, StdoutHeld: run.stdoutHeld, StderrHeld: run.stderrHeld}
	if run.launchErr != nil {
		st.State = StageLaunchFailed
		st.Reason = sptr(ReasonLaunchFailed)
		if absentErr(run.launchErr) {
			st.Reason = sptr(ReasonAbsentBinary)
		}
		return st
	}
	cleanup := run.cleanup
	st.Cleanup = &cleanup
	return st
}

// metadata runs one allowed metadata command (never a model session) with
// its 30 s watchdog inside the client's remaining time, both streams
// captured bounded, and writes its stdout and stderr files once launched.
func (c *CaptureRunner) metadata(ctx context.Context, pc *PlanClient, cc *CaptureClient, argv []string, name, outName, errName string, deadline time.Time) (CaptureStage, []byte, caseRun) {
	ws := filepath.Join(c.OutDir, workDir, pc.ID+"-"+name)
	if err := os.MkdirAll(ws, 0o700); err != nil {
		c.ioError = err
		return notRunStage("workspace: " + err.Error()), nil, caseRun{}
	}
	wd, capped := c.r.allowance(time.Duration(c.meta)*time.Millisecond, deadline)
	if wd <= 0 {
		return notRunStage(ReasonBudget + ": max_client_ms"), nil, caseRun{}
	}
	env := append([]string(nil), c.BaseEnv...)
	for _, k := range sortedKeys(pc.Env) {
		env = append(env, k+"="+pc.Env[k])
	}
	run := c.r.launch(ctx, ProcSpec{Path: pc.Executable, Args: argv, Env: env, Dir: ws, CaptureStderr: true}, wd, c.lim.Stream)
	run.budgetCapped = capped
	c.r.recordCleanup(pc.ID+" "+name, run.cleanup)
	st := stageOf(run, wd)
	if st.State != StageRan {
		return st, nil, run
	}
	var out bytes.Buffer
	for _, l := range run.transcript.Lines {
		out.Write(l.Data)
		out.WriteByte('\n')
	}
	c.text(cc, ClientFile(pc.ID, outName), out.Bytes(), run.transcript.Truncated)
	c.text(cc, ClientFile(pc.ID, errName), run.stderr, run.stderrCut)
	return st, out.Bytes(), run
}

// text writes a conservatively sanitized text payload and its stream
// record.
func (c *CaptureRunner) text(cc *CaptureClient, rel string, raw []byte, inCut bool) {
	clean, omitted := captureText(c.r.redactor, raw)
	c.put(cc, rel, clean, false, omitted, inCut)
}

// put writes one client payload within its bounds and records its stream;
// a write failure ends the capture (exit 1).
func (c *CaptureRunner) put(cc *CaptureClient, rel string, clean []byte, jsonl bool, omitted int, inCut bool) {
	if c.ioError != nil {
		return
	}
	_, outCut, err := c.w.put(cc.ID, rel, clean, jsonl)
	if err != nil {
		c.ioError = err
		return
	}
	cc.Streams = append(cc.Streams, CaptureStream{Path: rel, OmittedLines: omitted, InputTruncated: inCut, OutputTruncated: outCut})
}

// session prepares and runs the one zero-delay setup session.
func (c *CaptureRunner) session(ctx context.Context, pc *PlanClient, cc CaptureClient, deadline time.Time) CaptureClient {
	in, err := c.r.prepareCase(pc, caseSpec{id: cc.CaseID, setting: "default"})
	if err != nil {
		c.ioError = err
		return stop(cc, "workspace: "+err.Error())
	}
	in.proc.CaptureStderr, in.proc.StderrPath = true, ""
	labels := map[string]string{"{server}": "<server>", "{case_file}": "<workspace>/case.json", "{events}": "<workspace>/server-events.jsonl", "{workspace}": "<workspace>"}
	cc.Session = CaptureStage{State: StagePrepared, Reason: sptr("prepared, not launched")}
	c.text(&cc, ClientFile(pc.ID, FileConfig), []byte(substitute(in.recipe.Content, labels, false)), false)
	wd, capped := c.r.allowance(time.Duration(c.caseMS)*time.Millisecond, deadline)
	if wd <= 0 {
		cc.Session.Reason = sptr(ReasonBudget + ": max_client_ms")
		return stop(cc, *cc.Session.Reason)
	}
	ev := &captureSessionEvidence{c: c, cc: &cc}
	in.evidence = ev
	run := c.r.executeCase(ctx, in, wd)
	run.budgetCapped = capped
	c.r.recordCleanup(cc.CaseID, run.cleanup)
	cc.Session = stageOf(run, wd)
	if run.launchErr != nil {
		return stop(cc, *cc.Session.Reason)
	}
	cc.Probe, cc.ProbeReason = observeProbe(run.probeRaw, run.probeCut, cc.CaseID), nil
	if reason := sessionReason(run, cc); reason != "" {
		return stop(cc, reason)
	}
	cc.State, cc.Reason = CaptureComplete, nil
	return cc
}

// sessionReason classifies collection integrity and probe observations
// only (never the transcript): "" is a complete capture.
func sessionReason(run caseRun, cc CaptureClient) string {
	p := cc.Probe
	dirty := func(trunc bool) bool {
		for _, s := range cc.Streams {
			if trunc && (s.InputTruncated || s.OutputTruncated) || !trunc && s.OmittedLines > 0 {
				return true
			}
		}
		return false
	}
	switch {
	case run.interrupted:
		return ReasonInterrupted
	case run.cleanup.Error != nil:
		return ReasonCleanupFailed
	case run.stdoutHeld || run.stderrHeld:
		return ReasonStreamHeld + ": the session"
	case run.watchdog && run.budgetCapped:
		return ReasonSessionWatchdog + ": max_client_ms"
	case run.watchdog:
		return ReasonSessionWatchdog
	case run.probeCut:
		return ReasonEvidenceTruncated + ": the probe events"
	case len(p.Anomalies) > 0:
		return ReasonProbeAnomaly + ": " + strings.Join(p.Anomalies, ", ")
	case p.Receipts == 0:
		return ReasonProbeNotObserved
	case !p.Initialized || !p.Completed || !p.Intact:
		return ReasonProbeIncomplete
	case run.exit == nil || *run.exit != 0:
		return ReasonSessionExit
	case dirty(true):
		return ReasonEvidenceTruncated
	case len(run.transcript.Lines) == 0:
		return ReasonSessionStdoutEmpty
	case dirty(false):
		return ReasonEvidenceOmitted
	}
	return ""
}

// observeProbe extracts the requested case's parsed probe evidence: the
// first initialize clientInfo, its receipts and completion, and anomalies
// (a marker or other receipt, a second receipt, a rejection, a fatal or
// write error, an invalid or cut log).
func observeProbe(raw []byte, cut bool, caseID string) *CaptureProbe {
	p := &CaptureProbe{Anomalies: []string{}}
	if cut {
		p.Anomalies = append(p.Anomalies, "events_truncated")
		return p
	}
	if len(raw) == 0 {
		return p
	}
	evs, intact, err := ParseProbeEvents(raw)
	if err != nil {
		p.Anomalies = append(p.Anomalies, "events_invalid")
		return p
	}
	p.Intact = intact
	add := func(a string) {
		if !strings.Contains(strings.Join(p.Anomalies, ","), a) {
			p.Anomalies = append(p.Anomalies, a)
		}
	}
	// The receipt's identity: its probe instance (a start event begins a
	// new one) and request ID. A completion counts only for exactly that
	// receipt, once, after it (code review C7).
	//
	// Initialization and lifecycle are per instance too (code review round
	// 2, C3): the receipt-bearing instance must itself have initialized
	// before the receipt, and an instance ends at its exit event. The
	// recorded clientInfo stays the first one in the stream.
	//
	// Every instance closes (code review round 3, C2): an instance must
	// reach its exit before the next starts (instance_unclosed), and the
	// last one must too (otherwise the log is not intact), so the
	// receipt-bearing instance always has an intact ending of its own and a
	// later instance's exit never stands in for it.
	instance, recvInstance, recvID, done := 0, -1, "", false
	initialized, exited := false, false
	for _, ev := range evs {
		if exited && ev.Kind != EvStart {
			add("event_after_exit")
		}
		switch ev.Kind {
		case EvStart:
			if instance > 0 && !exited {
				add("instance_unclosed")
			}
			instance++
			initialized, exited = false, false
		case EvExit:
			exited = true
		case EvInitialize:
			initialized = true
			if !p.Initialized {
				p.Initialized = true
				p.ClientName, p.ClientVersion = ev.ClientName, ev.ClientVersion
			}
		case EvReceipt:
			switch ev.CaseID {
			case caseID:
				p.Receipts++
				if p.Receipts > 1 {
					add("extra_receipt")
				}
				if !initialized {
					add("receipt_without_initialize")
				}
				recvInstance, recvID = instance, string(bytes.TrimSpace(ev.RequestID))
			case caseID + "-marker":
				add("marker_receipt")
			default:
				add("unexpected_receipt")
			}
		case EvCompleted:
			switch {
			case ev.CaseID != caseID:
				add("unexpected_completion")
			case recvInstance < 0:
				add("completion_without_receipt")
			case recvInstance != instance || recvID != string(bytes.TrimSpace(ev.RequestID)):
				add("completion_mismatch")
			case done:
				add("duplicate_completion")
			default:
				done, p.Completed = true, true
			}
		case EvRejected:
			add("rejected")
		case EvError:
			add("fatal_error")
		case EvWriteFail:
			add("write_failed")
		}
	}
	return p
}

// captureSessionEvidence writes the session's transcript, stderr and probe
// events in the capture layout (always all three for a launched session;
// an absent stream is an empty file).
type captureSessionEvidence struct {
	c  *CaptureRunner
	cc *CaptureClient
}

func (e *captureSessionEvidence) write(r *Runner, _ caseInputs, run *caseRun) {
	c, cc := e.c, e.cc
	vendor, omitted := transcriptLines(r.redactor, run.transcript)
	c.put(cc, ClientFile(cc.ID, FileVendorEvents), vendor, true, omitted, run.transcript.Truncated)
	c.text(cc, ClientFile(cc.ID, FileVendorStderr), run.stderr, run.stderrCut)
	var server bytes.Buffer
	for _, line := range bytes.Split(run.probeRaw, []byte{'\n'}) {
		if len(line) > 0 {
			server.Write(r.redactor.Line(line))
			server.WriteByte('\n')
		}
	}
	c.put(cc, ClientFile(cc.ID, FileServerEvents), server.Bytes(), true, 0, run.probeCut)
	if c.ioError != nil {
		run.writeErr = c.ioError
	}
}

// captureText sanitizes text under the conservative capture policy: each
// LF-separated line goes through TranscriptLine, so a malformed line with
// an escape, a known literal or a credential trigger is omitted whole
// (only its count is kept); other lines are redacted.
func captureText(red *Redactor, b []byte) ([]byte, int) {
	if len(b) == 0 {
		return nil, 0
	}
	var out bytes.Buffer
	omitted := 0
	lines := bytes.Split(b, []byte{'\n'})
	for i, l := range lines {
		last := i == len(lines)-1
		if last && len(l) == 0 {
			break
		}
		clean, ok := red.TranscriptLine(l)
		if !ok {
			omitted++
			continue
		}
		out.Write(clean)
		if !last {
			out.WriteByte('\n')
		}
	}
	return out.Bytes(), omitted
}

// captureWriter writes payload files (mode 0600, new files only) within
// the per-file, per-client and bundle bounds, cutting at a line boundary
// with a marker when a bound is reached, and lists each with the hash of
// exactly its bytes.
type captureWriter struct {
	root      string
	lim       CaptureLimits
	files     []EvidenceRef
	perClient map[string]int
	total     int
}

func (w *captureWriter) put(client, rel string, clean []byte, jsonl bool) (EvidenceRef, bool, error) {
	allowed := w.lim.File
	if client != "" {
		allowed = min(allowed, w.lim.Client-w.perClient[client])
	}
	allowed = max(0, min(allowed, w.lim.Bundle-w.total))
	clean, cut := boundEvidence(clean, allowed, jsonl)
	if len(clean) > allowed {
		clean = nil
	}
	p := filepath.Join(w.root, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		return EvidenceRef{}, cut, err
	}
	f, err := os.OpenFile(p, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return EvidenceRef{}, cut, err
	}
	_, werr := f.Write(clean)
	if err := errors.Join(werr, f.Close()); err != nil {
		return EvidenceRef{}, cut, err
	}
	ref := EvidenceRef{Path: rel, SHA256: sha256Hex(clean), Bytes: int64(len(clean))}
	w.files = append(w.files, ref)
	w.perClient[client] += len(clean)
	w.total += len(clean)
	return ref, cut, nil
}

func (w *captureWriter) sorted() []EvidenceRef {
	out := append([]EvidenceRef(nil), w.files...)
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out
}

// finalize checks the output directory holds exactly the listed payloads,
// then writes the manifest last through a temporary file and a rename; a
// failure leaves no complete manifest.
func (c *CaptureRunner) finalize(man *CaptureManifest) error {
	c.sanitizeManifest(man)
	listed := map[string]bool{}
	for _, f := range man.Files {
		listed[f.Path] = true
	}
	err := filepath.WalkDir(c.OutDir, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(c.OutDir, p)
		rel = filepath.ToSlash(rel)
		switch {
		case p == c.OutDir || d.IsDir() && (rel == "clients" || path.Dir(rel) == "clients"):
			return nil
		case !d.Type().IsRegular() || !listed[rel]:
			return fmt.Errorf("unexpected entry %s in the capture directory", rel)
		}
		return nil
	})
	if err != nil {
		return err
	}
	if err := man.Validate(); err != nil {
		return fmt.Errorf("internal error: the capture produced an invalid manifest: %w", err)
	}
	b, err := encodeIndent(man)
	if err != nil {
		return err
	}
	if len(b) > c.lim.Manifest {
		return fmt.Errorf("the manifest's %d bytes exceed %d", len(b), c.lim.Manifest)
	}
	tmp, err := os.CreateTemp(c.OutDir, ".manifest-*.tmp")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	_, werr := tmp.Write(b)
	if err := errors.Join(werr, tmp.Chmod(0o600), tmp.Close()); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), filepath.Join(c.OutDir, CaptureManifestName))
}

// sanitizeManifest redacts the manifest's free text (versions, labels,
// reasons, probe clientInfo strings) by content; identifiers, hashes and
// the public nonce are kept exactly.
func (c *CaptureRunner) sanitizeManifest(man *CaptureManifest) {
	red := c.r.redactor
	str := func(p *string) {
		*p = red.String(*p)
	}
	opt := func(p **string) {
		if *p != nil {
			*p = sptr(red.String(**p))
		}
	}
	man.Cleanup.Failures = redactAll(red, man.Cleanup.Failures)
	opt(&man.Reason)
	for i := range man.Clients {
		cc := &man.Clients[i]
		// Every owner-controlled or observed string (code review C3); the
		// identity strings too, so a literal that hits them breaks the
		// required structure and finalize fails closed.
		for _, p := range []*string{&cc.ID, &cc.ExpectedVersion, &cc.Decoder, &cc.DecoderFixture, &cc.Model, &cc.CaseID, &cc.Config.Path, &cc.Config.TimeoutOverride} {
			str(p)
		}
		for _, p := range []**string{&cc.ObservedVersion, &cc.ObservedVersionReason, &cc.ExecutableReason, &cc.Effort, &cc.EffortReason,
			&cc.Config.Prerequisite, &cc.ProbeReason, &cc.Reason} {
			opt(p)
		}
		cc.Argv = redactAll(red, cc.Argv)
		for _, st := range []*CaptureStage{&cc.Version, &cc.Help, &cc.Session} {
			red.Struct(reflect.ValueOf(st).Elem())
		}
		if p := cc.Probe; p != nil {
			opt(&p.ClientName)
			opt(&p.ClientVersion)
			p.Anomalies = redactAll(red, p.Anomalies)
		}
	}
}

func redactAll(red *Redactor, xs []string) []string {
	out := make([]string, len(xs))
	for i, x := range xs {
		out[i] = red.String(x)
	}
	return out
}
