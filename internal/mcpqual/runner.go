package mcpqual

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/wedevwork/callsheet/internal/contract"
)

// secretName matches plan environment variables whose values are
// redacted from every publishable file.
var secretName = regexp.MustCompile(`(?i)(key|token|secret|password|passwd|auth|credential|cookie|session)`)

// workDir holds the disposable per-case workspaces inside the evidence
// directory; it is removed when the run ends and never published.
const workDir = ".work"

// versionWatchdog bounds a version or help capture.
const versionWatchdog = 30 * time.Second

// Runner executes one validated plan (FP-13) and writes its evidence
// (FP-11). It never publishes; see Publish.
type Runner struct {
	Plan            *Plan
	PlanSHA256      string
	OutDir          string
	AllowModelCalls bool
	GOOS, GOARCH    string
	Hostname        string
	// ServerPath is the absolute mcpqual executable for {server}.
	ServerPath string
	// BaseEnv is the vendor processes' inherited environment (CI removed).
	BaseEnv        []string
	Launcher       Launcher
	Reaper         Reaper
	Clock          Clock
	Registry       Registry
	Log            io.Writer
	RunID, Nonce   string
	CaptureDate    string
	HarnessVersion string
	// Home and User are redacted from evidence.
	Home, User string
	// HashFile hashes an executable; nil uses the file's bytes.
	HashFile func(string) (string, error)
	// StdoutLimit bounds a transcript (default MaxEvidenceFileBytes).
	StdoutLimit int
	// StdoutWait bounds the wait for stdout EOF after cleanup (default 2 s).
	StdoutWait time.Duration
	// EvidenceLimit bounds each written evidence file (default
	// MaxEvidenceFileBytes).
	EvidenceLimit int
	// ReportLimit bounds report.json (default MaxEvidenceFileBytes, the
	// limit ParseReport enforces).
	ReportLimit int

	redactor *Redactor
	mu       sync.Mutex
	evidence []EvidenceRef
	cleanup  CleanupReport
}

// Run executes the plan. It returns the report (also written to OutDir as
// report.json, report.md and manifest.json) and an error only when the
// evidence directory is unusable.
func (r *Runner) Run(ctx context.Context) (*Report, error) {
	if err := prepareOutDir(r.OutDir); err != nil {
		return nil, err
	}
	var secrets []string
	for _, c := range r.Plan.Clients {
		envs := []map[string]string{c.Env, c.Config.Default.Env}
		if c.Config.Raised != nil {
			envs = append(envs, c.Config.Raised.Env)
		}
		for _, env := range envs {
			for k, v := range env {
				if secretName.MatchString(k) {
					secrets = append(secrets, v)
				}
			}
		}
	}
	if r.User != "" {
		secrets = append(secrets, r.User)
	}
	paths := map[string]string{r.OutDir: "<out>"}
	if r.Home != "" {
		paths[r.Home] = "<home>"
	}
	r.redactor = NewRedactor(secrets, paths)
	r.cleanup = CleanupReport{OK: true, Failures: []string{}}
	rep := &Report{Schema: ReportSchema, RunID: r.RunID, CaptureDate: r.CaptureDate, OS: r.GOOS, Arch: r.GOARCH, HarnessVersion: r.HarnessVersion,
		PlanSHA256: r.PlanSHA256, ModelCallsAllowed: r.AllowModelCalls, HarnessStatus: HarnessVerified, VendorBehavior: VendorNotRun,
		Limits: r.Plan.EffectiveLimits(), PlannedUpperBound: r.plannedBound()}
	fmt.Fprintf(r.Log, "mcpqual: run %s: planned upper bound %d sessions, %d ms (model calls allowed: %v)\n",
		r.RunID, rep.PlannedUpperBound.Sessions, rep.PlannedUpperBound.WallMS, r.AllowModelCalls)
	for i := range r.Plan.Clients {
		pc := &r.Plan.Clients[i]
		var cr ClientReport
		switch {
		case ctx.Err() != nil:
			cr = r.skipped(pc, ReasonInterrupted)
		case !r.cleanupOK():
			cr = r.skipped(pc, ReasonCleanupFailed)
		default:
			cr = r.runClient(ctx, pc)
		}
		rep.Clients = append(rep.Clients, cr)
	}
	os.RemoveAll(filepath.Join(r.OutDir, workDir))
	rep.Interrupted = ctx.Err() != nil
	rep.Cleanup = r.cleanupReport()
	rep.Outcome = StatusConclusive
	for _, c := range rep.Clients {
		if c.Outcome != StatusConclusive {
			rep.Outcome = "partial"
		}
		for _, ph := range c.Phases {
			if len(ph.Cases) > 0 {
				rep.VendorBehavior = VendorMeasured
			}
		}
	}
	if rep.Interrupted || !rep.Cleanup.OK {
		rep.Outcome = "partial"
	}
	rep.Evidence = sortedEvidence(r.evidence)
	if err := r.sanitizeReport(rep); err != nil {
		return rep, fmt.Errorf("internal error: sanitizing the report: %w", err)
	}
	if err := boundReport(rep, r.reportLimit()); err != nil {
		return rep, fmt.Errorf("internal error: bounding the report: %w", err)
	}
	if err := rep.Validate(); err != nil {
		return rep, fmt.Errorf("internal error: the run produced an invalid report: %w", err)
	}
	if err := writeReport(r.OutDir, rep); err != nil {
		return rep, err
	}
	return rep, nil
}

// ExitCode maps a finished run: 0 when every requested phase is
// conclusive, 130 when interrupted, 5 (unavailable) otherwise.
func (rep *Report) ExitCode() int {
	switch {
	case rep.Interrupted:
		return contract.ExitInterrupted
	case rep.Outcome == StatusConclusive && rep.Cleanup.OK:
		return contract.ExitOK
	}
	return contract.ExitCode(contract.New(contract.CodeUnavailable, "partial"))
}

func prepareOutDir(dir string) error {
	if !filepath.IsAbs(dir) || filepath.Clean(dir) != dir {
		return contract.New(contract.CodeInvalidArgument, fmt.Sprintf("--out %q must be an absolute clean path", dir))
	}
	entries, err := os.ReadDir(dir)
	switch {
	case errors.Is(err, os.ErrNotExist):
	case err != nil:
		return contract.Wrap(contract.CodeInvalidArgument, fmt.Sprintf("--out %q is not a usable directory", dir), err)
	case len(entries) > 0:
		return contract.New(contract.CodeInvalidArgument, fmt.Sprintf("--out %q must be empty or new (evidence of runs is never mixed)", dir))
	}
	if err := os.MkdirAll(filepath.Join(dir, workDir), 0o700); err != nil {
		return contract.Wrap(contract.CodeInvalidArgument, fmt.Sprintf("--out %q cannot be created", dir), err)
	}
	return nil
}

func writeReport(dir string, rep *Report) error {
	b, err := encodeIndent(rep)
	if err != nil {
		return err
	}
	md := []byte(rep.Markdown())
	files := []EvidenceRef{{Path: "report.json", SHA256: sha256Hex(b), Bytes: int64(len(b))}, {Path: "report.md", SHA256: sha256Hex(md), Bytes: int64(len(md))}}
	files = append(files, rep.Evidence...)
	man, _ := encodeIndent(Manifest{Schema: ReportSchema, RunID: rep.RunID, Files: files})
	for name, data := range map[string][]byte{"report.json": b, "report.md": md, "manifest.json": man} {
		if err := os.WriteFile(filepath.Join(dir, name), data, 0o644); err != nil {
			return err
		}
	}
	return nil
}

// Manifest lists every file of a run with its hash.
type Manifest struct {
	Schema int           `json:"schema"`
	RunID  string        `json:"run_id"`
	Files  []EvidenceRef `json:"files"`
}

// Evidence kinds: how writeEvidence sanitizes before writing.
type evidenceKind int

const (
	evidenceText      evidenceKind = iota // redacted as text
	evidenceJSONL                         // one JSON value per line, redacted per string value
	evidenceSanitized                     // already sanitized by the caller
)

func (r *Runner) evidenceLimit() int {
	if r.EvidenceLimit > 0 {
		return r.EvidenceLimit
	}
	return MaxEvidenceFileBytes
}

// writeEvidence is putEvidence without the cut and error reports (the
// error is logged by putEvidence).
func (r *Runner) writeEvidence(rel string, data []byte, kind evidenceKind) string {
	rel, _, _ = r.putEvidence(rel, data, kind)
	return rel
}

// putEvidence sanitizes data before any outer encoding, bounds the
// sanitized bytes to the evidence limit (cutting at a line boundary and
// marking the cut), writes them and lists them with the hash of exactly the
// written bytes. It reports whether the file was cut and any write failure
// (also logged; an unwritten file is never listed).
func (r *Runner) putEvidence(rel string, data []byte, kind evidenceKind) (string, bool, error) {
	var clean []byte
	switch kind {
	case evidenceText:
		clean = r.redactor.Bytes(data)
	case evidenceJSONL:
		var buf bytes.Buffer
		for _, line := range bytes.Split(data, []byte{'\n'}) {
			if len(line) > 0 {
				buf.Write(r.redactor.Line(line))
				buf.WriteByte('\n')
			}
		}
		clean = buf.Bytes()
	default:
		clean = data
	}
	clean, cut := boundEvidence(clean, r.evidenceLimit(), kind != evidenceText)
	p := filepath.Join(r.OutDir, filepath.FromSlash(rel))
	os.MkdirAll(filepath.Dir(p), 0o755)
	if err := os.WriteFile(p, clean, 0o644); err != nil {
		fmt.Fprintf(r.Log, "mcpqual: write %s: %v\n", rel, err)
		return rel, cut, err
	}
	r.mu.Lock()
	r.evidence = append(r.evidence, EvidenceRef{Path: rel, SHA256: sha256Hex(clean), Bytes: int64(len(clean))})
	r.mu.Unlock()
	return rel, cut, nil
}

// boundEvidence cuts b to at most limit bytes at a line boundary, ending
// with a truncation marker (a JSON line for JSONL evidence).
func boundEvidence(b []byte, limit int, jsonl bool) ([]byte, bool) {
	if len(b) <= limit {
		return b, false
	}
	marker := []byte("[truncated at the evidence limit]\n")
	if jsonl {
		marker = []byte(`{"truncated":true}` + "\n")
	}
	keep := b[:max(0, limit-len(marker))]
	if i := bytes.LastIndexByte(keep, '\n'); i >= 0 {
		keep = keep[:i+1]
	} else {
		keep = keep[:0]
	}
	return append(append([]byte(nil), keep...), marker...), true
}

// sanitizeReport redacts every string reachable from the report (clientInfo,
// versions, settings, reasons, decoded events) in place, before the report
// is encoded or used for catalog facts. Report members are Callsheet's own,
// so redaction is by content.
func (r *Runner) sanitizeReport(rep *Report) error {
	r.redactor.Struct(reflect.ValueOf(rep).Elem())
	return nil
}

func (r *Runner) reportLimit() int {
	if r.ReportLimit > 0 && r.ReportLimit < MaxEvidenceFileBytes {
		return r.ReportLimit
	}
	return MaxEvidenceFileBytes
}

// maxBoundString bounds each string an over-budget report keeps.
const maxBoundString = 512

// boundReport keeps the serialized, sanitized report within limit (the
// size ParseReport accepts) whatever the sizes of vendor-supplied strings;
// a report that fits is untouched. Otherwise, until it fits: every case
// over its share of half the limit keeps only the decoded events that fit
// the share, with its strings cut; then the client, phase and cleanup
// strings are cut; last, case summaries are dropped. Evidence references
// are never cut. Every case so reduced becomes inconclusive, and every
// phase holding one (or whose strings were cut) inconclusive, with a
// report_bound reason; a client so reduced is partial at best.
func boundReport(rep *Report, limit int) error {
	fits := func() (bool, error) {
		b, err := encodeIndent(rep)
		return len(b) <= limit, err
	}
	if ok, err := fits(); ok || err != nil {
		return err
	}
	defer func() {
		for _, c := range rep.Clients {
			if c.Outcome != StatusConclusive {
				rep.Outcome = "partial"
			}
		}
	}()
	cases := 0
	for _, c := range rep.Clients {
		for _, ph := range c.Phases {
			cases += len(ph.Cases)
		}
	}
	share := limit / 2 / max(1, cases)
	forPhases := func(f func(c *ClientReport, ph *PhaseReport) bool) {
		for i := range rep.Clients {
			c := &rep.Clients[i]
			for j := range c.Phases {
				if ph := &c.Phases[j]; f(c, ph) {
					boundPhase(c, ph)
				}
			}
		}
	}
	forPhases(func(_ *ClientReport, ph *PhaseReport) bool {
		bounded := false
		for k := range ph.Cases {
			if cs := &ph.Cases[k]; caseBytes(cs) > share {
				boundCase(cs, share)
				bounded = true
			}
		}
		return bounded
	})
	if ok, err := fits(); ok || err != nil {
		return err
	}
	for i := range rep.Clients {
		c := &rep.Clients[i]
		phases := c.Phases
		c.Phases = nil
		if cutStrings(reflect.ValueOf(c).Elem()) {
			boundClient(c, "client")
		}
		c.Phases = phases
	}
	forPhases(func(_ *ClientReport, ph *PhaseReport) bool {
		cs := ph.Cases
		ph.Cases = nil
		cut := cutStrings(reflect.ValueOf(ph).Elem())
		ph.Cases = cs
		return cut
	})
	cutStrings(reflect.ValueOf(&rep.Cleanup).Elem())
	if ok, err := fits(); ok || err != nil {
		return err
	}
	forPhases(func(_ *ClientReport, ph *PhaseReport) bool {
		if len(ph.Cases) == 0 {
			return false
		}
		ph.Cases = []CaseReport{}
		return true
	})
	if ok, err := fits(); ok || err != nil {
		return err
	}
	return fmt.Errorf("report exceeds %d bytes after bounding", limit)
}

// boundCase keeps the decoded events of cs that fit share (each with its
// strings cut) and cuts its other strings, keeping its evidence references;
// the case becomes inconclusive, its reason recording what it kept and how
// it had been classified.
func boundCase(cs *CaseReport, share int) {
	was := cs.Outcome
	if cs.Reason != nil {
		was += ": " + *cs.Reason
	}
	evs := cs.Events
	cs.Events = []Event{}
	server, vendor := cs.ServerEvents, cs.VendorEvents
	cs.ServerEvents, cs.VendorEvents = nil, nil
	cutStrings(reflect.ValueOf(cs).Elem())
	cs.ServerEvents, cs.VendorEvents = server, vendor
	cs.Outcome = OutcomeInconclusive
	reason := func(kept int) string {
		r := fmt.Sprintf("%s: kept %d of %d decoded events, strings cut to %d bytes, within the report budget; the vendor_events file holds the transcript; classified %s",
			ReasonReportBound, kept, len(evs), maxBoundString, was)
		return cutString(r)
	}
	cs.Reason = sptr(reason(len(evs)))
	total := caseBytes(cs)
	prefix := strings.Repeat("  ", 8) // an event's depth in report.json
	for _, ev := range evs {
		cutStrings(reflect.ValueOf(&ev).Elem())
		n := jsonLen(ev, prefix) + len(prefix) + 2
		if total+n > share {
			break
		}
		cs.Events = append(cs.Events, ev)
		total += n
	}
	cs.Reason = sptr(reason(len(cs.Events)))
}

// boundPhase makes ph (holding a bounded case or cut strings)
// inconclusive, and its client partial when it was conclusive.
func boundPhase(c *ClientReport, ph *PhaseReport) {
	if ph.Status == StatusConclusive {
		ph.Status = StatusInconclusive
		ph.Reason = sptr(ReasonReportBound + ": the phase's detail exceeded the report budget")
	}
	boundClient(c, ph.Name)
}

func boundClient(c *ClientReport, what string) {
	if c.Outcome == StatusConclusive {
		c.Outcome = "partial"
		c.Reason = sptr(what + ": " + ReasonReportBound)
	}
}

// caseBytes is the indented size of cs at its depth in report.json.
func caseBytes(cs *CaseReport) int { return jsonLen(cs, strings.Repeat("  ", 6)) }

func jsonLen(v any, prefix string) int {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	enc.SetIndent(prefix, "  ")
	enc.Encode(v)
	return buf.Len()
}

// cutStrings cuts every string reachable from v to maxBoundString bytes;
// it reports whether any was cut.
func cutStrings(v reflect.Value) bool {
	cut := false
	switch v.Kind() {
	case reflect.Pointer:
		if !v.IsNil() {
			cut = cutStrings(v.Elem())
		}
	case reflect.Struct:
		for i := 0; i < v.NumField(); i++ {
			if v.Type().Field(i).IsExported() {
				cut = cutStrings(v.Field(i)) || cut
			}
		}
	case reflect.Slice:
		for i := 0; i < v.Len(); i++ {
			cut = cutStrings(v.Index(i)) || cut
		}
	case reflect.String:
		if s := v.String(); len(s) > maxBoundString {
			v.SetString(cutString(s))
			cut = true
		}
	}
	return cut
}

const cutMark = "[cut]"

// cutString shortens s to at most maxBoundString bytes at a rune boundary,
// ending with cutMark.
func cutString(s string) string {
	if len(s) <= maxBoundString {
		return s
	}
	n := maxBoundString - len(cutMark)
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n] + cutMark
}

// writeAux writes a redacted diagnostic file that is not evidence.
func (r *Runner) writeAux(rel string, data []byte) {
	p := filepath.Join(r.OutDir, filepath.FromSlash(rel))
	os.MkdirAll(filepath.Dir(p), 0o755)
	os.WriteFile(p, r.redactor.Bytes(data), 0o600)
}

func (r *Runner) recordCleanup(what string, cc CaseCleanup) {
	if cc.Error == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.cleanup.OK = false
	r.cleanup.Failures = append(r.cleanup.Failures, what+": "+*cc.Error)
}

func (r *Runner) cleanupOK() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.cleanup.OK
}

func (r *Runner) cleanupReport() CleanupReport {
	r.mu.Lock()
	defer r.mu.Unlock()
	return CleanupReport{OK: r.cleanup.OK, Failures: append([]string{}, r.cleanup.Failures...)}
}

// plannedBound is the upper bound on sessions and wall time before any
// launch: every case the requested phases could schedule, capped by the
// limits.
func (r *Runner) plannedBound() PlannedBound {
	lim := r.Plan.EffectiveLimits()
	var b PlannedBound
	for _, c := range r.Plan.Clients {
		n := len(caseBudget(&c))
		if n > lim.MaxSessionsPerClient {
			n = lim.MaxSessionsPerClient
		}
		wall := int64(n) * lim.MaxCaseMS
		if wall > lim.MaxClientMS {
			wall = lim.MaxClientMS
		}
		if c.Driver == DriverModel && !r.AllowModelCalls {
			n, wall = 0, 0
		}
		b.Sessions += n
		b.WallMS += wall
	}
	return b
}

// caseBudget lists the most cases the plan could run for c, in order.
func caseBudget(c *PlanClient) []string {
	out := []string{PhaseSetup}
	if d := c.Phases.Default; d != nil {
		delays := d.DelaysMS
		if len(delays) == 0 {
			delays = DefaultDelaysMS
		}
		for range delays {
			out = append(out, PhaseDefault)
		}
		out = append(out, PhaseDefault) // the repeat observation
	}
	if o := c.Phases.Override; o != nil {
		out = append(out, PhaseOverride)
		if o.BoundDelayMS > 0 {
			out = append(out, PhaseOverride)
		}
	}
	if c.Phases.Progress != nil {
		out = append(out, PhaseProgress)
	}
	if c.Phases.Absolute != nil {
		out = append(out, PhaseAbsolute)
	}
	return out
}

func requestedPhases(c *PlanClient) []string {
	out := []string{PhaseSetup}
	if c.Phases.Default != nil {
		out = append(out, PhaseDefault)
	}
	if c.Phases.Override != nil {
		out = append(out, PhaseOverride)
	}
	if c.Phases.Progress != nil {
		out = append(out, PhaseProgress)
	}
	if c.Phases.Absolute != nil {
		out = append(out, PhaseAbsolute)
	}
	return out
}

// newClientReport fills the static fields.
func (r *Runner) newClientReport(pc *PlanClient) ClientReport {
	cr := ClientReport{ID: pc.ID, ExecutableName: filepath.Base(pc.Executable), ExpectedVersion: pc.ExpectedVersion, Driver: pc.Driver, Decoder: pc.Decoder,
		Phases: []PhaseReport{}}
	if pc.Model != "" {
		cr.Model = sptr(pc.Model)
	}
	if pc.Effort != "" {
		cr.Effort = sptr(pc.Effort)
	}
	cr.Settings.Override = pc.Override
	if pre := pc.Config.Default.Prerequisite; pre != "" {
		cr.Settings.Prerequisite = sptr(pre)
	}
	cr.ClientInfo.UnqualifiedReason = sptr("clientInfo not observed")
	return cr
}

// skipped is a client none of whose phases ran.
func (r *Runner) skipped(pc *PlanClient, reason string) ClientReport {
	cr := r.newClientReport(pc)
	return notRun(cr, pc, reason)
}

func notRun(cr ClientReport, pc *PlanClient, reason string) ClientReport {
	for _, ph := range requestedPhases(pc) {
		if phaseIndex(cr, ph) >= 0 {
			continue
		}
		cr.Phases = append(cr.Phases, PhaseReport{Name: ph, Status: StatusNotRun, Reason: sptr(reason), Cases: []CaseReport{}})
	}
	cr.Outcome = "unqualified"
	cr.Reason = sptr(reason)
	if cr.ObservedVersion == nil && cr.Reason == nil {
		cr.Reason = sptr(reason)
	}
	return cr
}

func phaseIndex(cr ClientReport, name string) int {
	for i, ph := range cr.Phases {
		if ph.Name == name {
			return i
		}
	}
	return -1
}

func (r *Runner) hashExecutable(p string) (string, error) {
	if r.HashFile != nil {
		return r.HashFile(p)
	}
	f, err := os.Open(p)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// capture runs an allowed version or help command (never a model session)
// within the client's remaining wall time and returns its bounded stdout.
func (r *Runner) capture(ctx context.Context, pc *PlanClient, argv []string, name string, deadline time.Time) (string, caseRun) {
	ws := filepath.Join(r.OutDir, workDir, pc.ID+"-"+name)
	os.MkdirAll(ws, 0o700)
	env := append([]string(nil), r.BaseEnv...)
	for _, k := range sortedKeys(pc.Env) {
		env = append(env, k+"="+pc.Env[k])
	}
	wd, capped := r.allowance(min(versionWatchdog, time.Duration(r.Plan.EffectiveLimits().MaxCaseMS)*time.Millisecond), deadline)
	if wd <= 0 {
		return "", caseRun{budgetCapped: true, watchdog: true}
	}
	run := r.launch(ctx, ProcSpec{Path: pc.Executable, Args: argv, Env: env, Dir: ws}, wd, 64<<10)
	run.budgetCapped = capped
	r.recordCleanup(pc.ID+" "+name, run.cleanup)
	var sb strings.Builder
	for _, l := range run.transcript.Lines {
		sb.Write(l.Data)
		sb.WriteByte('\n')
	}
	return sb.String(), run
}

// allowance caps a launch's watchdog by the client's remaining wall time;
// capped reports that the client deadline, not the case limit, bounds it.
// Cleanup after the watchdog has its own time and is never cut.
func (r *Runner) allowance(caseLimit time.Duration, deadline time.Time) (time.Duration, bool) {
	remaining := deadline.Sub(r.Clock.Now())
	if remaining < caseLimit {
		return remaining, true
	}
	return caseLimit, false
}

// metadataOK is a successful metadata capture: exit 0, not cut by the
// watchdog, the budget or an interruption, and not truncated.
func metadataOK(run caseRun) bool {
	return run.launchErr == nil && run.exit != nil && *run.exit == 0 && !run.watchdog && !run.interrupted && !run.transcript.Truncated
}

func firstLine(s string) string {
	for _, l := range strings.Split(s, "\n") {
		if t := strings.TrimSpace(l); t != "" {
			return t
		}
	}
	return ""
}

// observedVersion is the version command's output when it is exactly one
// nonempty line.
func observedVersion(out string) (string, bool) {
	t := strings.TrimSpace(out)
	return t, t != "" && !strings.ContainsAny(t, "\r\n")
}

// runClient qualifies one client: allowed metadata commands only, the
// exact observed version from a successful capture, the decoder of that
// validated version, the explicit model-call gate, then the measurement
// phases. The client's wall-time budget starts before its first launch.
func (r *Runner) runClient(ctx context.Context, pc *PlanClient) ClientReport {
	cr := r.newClientReport(pc)
	start := r.Clock.Now()
	deadline := start.Add(time.Duration(r.Plan.EffectiveLimits().MaxClientMS) * time.Millisecond)
	if !MetadataAllowed(pc.VersionArgv, allowedVersionArgv) || len(pc.HelpArgv) > 0 && !MetadataAllowed(pc.HelpArgv, allowedHelpArgv) {
		return notRun(cr, pc, ReasonMetadataNotAllowed)
	}
	h, err := r.hashExecutable(pc.Executable)
	if err != nil {
		return notRun(cr, pc, ReasonAbsentBinary)
	}
	cr.ExecutableSHA256 = sptr(h)
	out, run := r.capture(ctx, pc, pc.VersionArgv, "version", deadline)
	switch {
	case run.launchErr != nil && absentErr(run.launchErr):
		return notRun(cr, pc, ReasonAbsentBinary)
	case run.launchErr != nil:
		return notRun(cr, pc, ReasonLaunchFailed)
	case run.cleanup.Error != nil:
		return notRun(cr, pc, ReasonCleanupFailed)
	case run.interrupted:
		return notRun(cr, pc, ReasonInterrupted)
	case run.budgetCapped && run.watchdog:
		return notRun(cr, pc, ReasonBudget+": max_client_ms")
	}
	r.writeEvidence(path.Join("versions", pc.ID+"-version.txt"), []byte(out), evidenceText)
	observed, single := observedVersion(out)
	cr.ObservedVersion = sptr(r.redactor.String(firstLine(out)))
	switch {
	case !metadataOK(run):
		return notRun(cr, pc, ReasonMetadataFailed+": the version command did not succeed")
	case !single || observed != pc.ExpectedVersion:
		return notRun(cr, pc, ReasonVersionMismatch)
	}
	if len(pc.HelpArgv) > 0 {
		help, run := r.capture(ctx, pc, pc.HelpArgv, "help", deadline)
		switch {
		case run.cleanup.Error != nil:
			return notRun(cr, pc, ReasonCleanupFailed)
		case run.interrupted:
			return notRun(cr, pc, ReasonInterrupted)
		case run.budgetCapped && run.watchdog:
			return notRun(cr, pc, ReasonBudget+": max_client_ms")
		case !metadataOK(run):
			return notRun(cr, pc, ReasonMetadataFailed+": the help command did not succeed")
		}
		r.writeEvidence(path.Join("versions", pc.ID+"-help.txt"), []byte(help), evidenceText)
	}
	decode, dv, err := r.Registry.Select(pc.Decoder, observed)
	if err != nil {
		return notRun(cr, pc, ReasonDecoderVersion+": "+err.Error())
	}
	// The plan's fixture must be the selected exact version's canonical
	// fixture, never merely another fixture of the same decoder family.
	if dv.Fixture != pc.DecoderFixture {
		return notRun(cr, pc, fmt.Sprintf("%s: the plan names %q, version %q is backed by %q", ReasonDecoderFixture, pc.DecoderFixture, observed, dv.Fixture))
	}
	cr.DecoderVersion = &dv
	labels := map[string]string{"{server}": "<server>", "{case_file}": "<case-file>", "{events}": "<events>", "{workspace}": "<workspace>"}
	cr.Settings.Default = sptr(r.writeEvidence(path.Join("config", pc.ID+"-default-"+path.Base(pc.Config.Default.Path)), []byte(substitute(pc.Config.Default.Content, labels, false)), evidenceText))
	if rc := pc.Config.Raised; rc != nil {
		cr.Settings.Raised = sptr(r.writeEvidence(path.Join("config", pc.ID+"-raised-"+path.Base(rc.Path)), []byte(substitute(rc.Content, labels, false)), evidenceText))
	}
	if pc.Driver == DriverModel && !r.AllowModelCalls {
		return notRun(cr, pc, ReasonModelCallsDenied)
	}
	m := &measure{r: r, pc: pc, cr: &cr, decode: decode, dv: dv, start: start, deadline: deadline}
	m.run(ctx)
	return cr
}
