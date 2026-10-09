package mcpqual

// Shared Cursor preparation (design decoder-enrollment B3, FP-24): the
// workspace-local permission file, its no-follow verification, the one
// "mcp enable probe" with its before/after inventories and the
// configuration re-read, factored out of capture so that capture and
// qualify each use the same operations. cursorPreparer owns only the
// filesystem view, the bounded scan limits, the adapter identity, the
// owner's home and the run-local residue ledger (FP-25); every operation
// takes its execution context, case inputs, deadline and the bounded
// launcher explicitly and returns stage and evidence values. It writes no
// capture or report file itself: each caller's writer records the result
// in its own layout.

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"time"
	"unicode/utf8"
)

// cursorPreparer is the shared Cursor preparation of one run.
type cursorPreparer struct {
	fsys                        approvalFS
	limits                      ApprovalScanLimits
	goos, goarch, version, home string
	ledger                      residueLedger
}

// newCursorPreparer builds the preparer: a nil filesystem is the operating
// system, zero limits are DefaultApprovalScanLimits and larger limits are
// lowered to them (tests inject smaller caps, never larger ones).
func newCursorPreparer(fsys approvalFS, limits ApprovalScanLimits, goos, goarch, version, home string) *cursorPreparer {
	if fsys == nil {
		fsys = osApprovalFS{}
	}
	return &cursorPreparer{fsys: fsys, limits: limits.orDefault(), goos: goos, goarch: goarch, version: version, home: home}
}

// applies reports the permission/project adapter's exact identity.
func (p *cursorPreparer) applies() bool {
	return cursorToolPermissionApplies(p.goos, p.goarch, p.version)
}

// scanner is a fresh keyed approval scanner over the preparer's view with
// its limits, the two data directories excluded and the ledger's sockets
// exempt.
func (p *cursorPreparer) scanner() (*approvalScanner, error) {
	sc, err := newApprovalScanner(p.fsys, p.limits)
	if err != nil {
		return nil, errors.New("no fingerprint key")
	}
	sc.exclude(p.home)
	sc.exempt = p.exempt
	return sc, nil
}

// toolPermissionRecord is the written record of the permission file.
func toolPermissionRecord() *CaptureToolPermission {
	return &CaptureToolPermission{Adapter: CursorToolPermissionAdapter, Path: labelToolPermission, Content: CursorToolPermissionContent,
		SHA256: sha256Hex([]byte(CursorToolPermissionContent)), State: PermissionWritten, Reason: sptr(permissionPending)}
}

// writePermission creates the permission file of prepared case in: the
// workspace and its .cursor directory (created by prepareCase for the MCP
// configuration) must be ordinary, non-symlink directories, cli.json must
// not exist (an owner file is never merged or overwritten), and the file is
// created new, no-follow, mode 0600 with the exact bytes. It returns the
// file's identity, or a stop reason before any enable or model launch.
func (p *cursorPreparer) writePermission(in caseInputs) (fs.FileInfo, string) {
	fail := func(why string) (fs.FileInfo, string) {
		return nil, ReasonCursorScopeUnverified + ": the scoped permission file was not created (" + why + "); nothing was launched"
	}
	dir := filepath.Join(in.ws, ".cursor")
	if filepath.Dir(in.configPath) != dir {
		return fail("the configuration is not the workspace's .cursor/mcp.json")
	}
	for _, d := range []string{in.ws, dir} {
		if info, err := p.fsys.Lstat(d); err != nil || !info.IsDir() {
			return fail("the workspace or its .cursor is not an ordinary directory")
		}
	}
	f := filepath.Join(dir, cursorPermissionFile)
	if _, err := p.fsys.Lstat(f); !errors.Is(err, fs.ErrNotExist) {
		return fail("a cli.json already exists or cannot be looked up")
	}
	h, err := createNoFollow(f)
	if err != nil {
		return fail("it cannot be created")
	}
	_, werr := io.WriteString(h, CursorToolPermissionContent)
	if err := errors.Join(werr, h.Close()); err != nil {
		return fail("it cannot be written")
	}
	info, err := p.fsys.Lstat(f)
	if err != nil || !info.Mode().IsRegular() {
		return fail("it is not a regular file after creation")
	}
	return info, ""
}

// verifyPermission reads the permission file of in bounded and without
// following a link: the same regular file as created (ident, with its
// modification time: an unlinked file's inode can be reused at once), its
// exact bytes, unchanged while read. A write that restores both between
// two checks stays undetectable.
func (p *cursorPreparer) verifyPermission(in caseInputs, ident fs.FileInfo) bool {
	f := filepath.Join(in.ws, ".cursor", cursorPermissionFile)
	pre, err := p.fsys.Lstat(f)
	if err != nil || ident == nil || !pre.Mode().IsRegular() || !sameFile(ident, pre) || !pre.ModTime().Equal(ident.ModTime()) {
		return false
	}
	h, err := p.fsys.Open(f)
	if err != nil {
		return false
	}
	defer h.Close()
	opened, err := h.Stat()
	if err != nil || !sameFile(pre, opened) {
		return false
	}
	b, err := io.ReadAll(io.LimitReader(h, int64(len(CursorToolPermissionContent))+1))
	post, err2 := p.fsys.Lstat(f)
	return err == nil && err2 == nil && string(b) == CursorToolPermissionContent && sameFile(pre, post)
}

// permissionChanged is the stop reason of a permission check that failed.
const permissionChanged = ReasonCursorScopeUnverified + ": the harness-written " + labelToolPermission + " was removed, replaced or changed (no retry or restore)"

// approvalLaunch is the bounded launcher of one enable command: the shared
// runner (launch, allowance, cleanup record, redactor), the executable and
// its environment, the cleanup record's label and the log line's client.
type approvalLaunch struct {
	r          *Runner
	executable string
	env        []string
	what, id   string
	log        io.Writer
}

// approvalOutput is the enable command's output, slug-normalized, with the
// lines withheld whole and whether each stream was cut.
type approvalOutput struct {
	stdout, stderr           []byte
	withheldOut, withheldErr int
	outCut, errCut           bool
}

// approve runs the approval stage of prepared case in (after the
// configuration and permission writes, before the session). It returns the
// approval record and "" when the session may start, otherwise the stop
// reason. A failed pre-scan launches nothing. The enable command runs once,
// with min(30 s, the time left to deadline), through the shared launch and
// cleanup; its output goes to retain (the caller's writer, which reports a
// write failure) before the second snapshot follows its cleanup.
func (p *cursorPreparer) approve(ctx context.Context, l approvalLaunch, in caseInputs, deadline time.Time, permissionWritten bool, retain func(approvalOutput) error) (*CaptureApproval, string) {
	ap := newApproval("not reached")
	unverified := func(why string) (*CaptureApproval, string) {
		reason := ReasonCursorScopeUnverified + ": " + why
		ap.Stage, ap.Scope, ap.Reason = notRunStage(reason), ScopeUnverifiable, sptr(reason)
		return ap, reason
	}
	roots, err := approvalRoots(p.fsys, in.ws, p.home)
	if err != nil {
		return unverified(err.Error())
	}
	if err := p.revalidate(); err != nil {
		return unverified(err.Error())
	}
	sc, err := p.scanner()
	if err != nil {
		return unverified(err.Error())
	}
	// The per-project approval path (FP-15): derived only by the observed
	// adapter; an unsupported platform, version or path derives nothing.
	slug, derr := cursorProjectSlug(sc.fs, p.goos, p.goarch, p.version, in.ws)
	projKey := ""
	if derr == nil {
		projKey = labelHomeProjects + "/" + slug
	}
	pre := sc.snapshot(roots)
	if !pre.complete {
		return unverified("the inventory before the command is incomplete (" + pre.why + ")")
	}
	if _, exists := pre.entries[projKey]; projKey != "" && exists {
		return unverified("the computed Cursor project directory exists before the command (a prior run or a colliding workspace path); nothing was changed or launched")
	}
	r := l.r
	// The pre-scan took time: a cancellation during it launches nothing
	// (code review B3 round 2, C1), recorded as an unlaunched stage with the
	// interruption's precedence; the allowance below is the deadline check.
	if ctx.Err() != nil {
		ap.Stage, ap.Reason = notRunStage(ReasonInterrupted), sptr(ReasonInterrupted)
		return ap, ReasonInterrupted
	}
	wd, capped := r.allowance(cursorApprovalWatchdog, deadline)
	if wd <= 0 {
		reason := ReasonBudget + ": max_client_ms"
		ap.Stage, ap.Reason = notRunStage(reason), sptr(reason)
		return ap, reason
	}
	run := r.launch(ctx, ProcSpec{Path: l.executable, Args: CursorApprovalArgv(), Env: l.env, Dir: in.ws, CaptureStderr: true}, wd, r.stdoutLimit())
	run.budgetCapped = capped
	r.recordCleanup(l.what, run.cleanup)
	ap.Stage = stageOf(run, wd)
	if ap.Stage.State != StageRan {
		reason := ReasonCursorApprovalFailed + ": the enable command did not launch (" + *ap.Stage.Reason + ")"
		ap.Reason = sptr(reason)
		return ap, reason
	}
	var out strings.Builder
	for _, line := range run.transcript.Lines {
		out.Write(line.Data)
		out.WriteByte('\n')
	}
	// The computed slug encodes the absolute workspace: it is replaced by
	// its placeholder before the general redaction (FP-15).
	o := approvalOutput{outCut: run.transcript.Truncated, errCut: run.stderrCut}
	o.stdout, o.withheldOut = normalizeSlugText([]byte(out.String()), slug)
	o.stderr, o.withheldErr = normalizeSlugText(run.stderr, slug)
	if err := retain(o); err != nil {
		reason := "workspace: " + err.Error()
		ap.Scope, ap.Reason = ScopeUnverifiable, sptr(reason)
		return ap, reason
	}
	post := sc.snapshot(roots)
	changes := diffSnapshots(pre, post)
	ap.InventoryComplete = post.complete
	ap.Changes = make([]CaptureApprovalChange, 0, len(changes))
	outside, faithful, workspaceFiles := false, true, true
	projDir, projFile, unattributed, permission := false, false, false, false
	red := r.redactor
	recorded := map[string]bool{}
	for _, ch := range changes {
		// Two roots sharing a label record one path: the list could not
		// tell them apart, so it is incomplete (the decision itself used
		// the distinct keys).
		ch.Path = recordedPath(ch.Path)
		if recorded[ch.Path] {
			faithful = false
			continue
		}
		recorded[ch.Path] = true
		switch q := ch.Path; {
		case inWorkspace(q):
			if !inWorkspaceCursor(q) || !ch.regular {
				workspaceFiles = false
			}
			// The harness-written permission file is baseline state: any
			// change of it is never approval evidence (FP-20).
			permission = permission || permissionWritten && q == labelToolPermission
		case projKey != "" && q == projKey:
			// The computed project directory: allowed only as a newly added
			// ordinary directory.
			e := post.entries[q]
			if ch.Kind == ChangeAdded && e.typ.IsDir() && !e.unknown {
				projDir = true
			} else {
				outside = true
			}
			ch.Path = labelProjectDir
		case projKey != "" && q == projKey+"/"+cursorApprovalsFile:
			if ch.Kind == ChangeAdded && ch.regular {
				projFile = true
			} else {
				outside = true
			}
			ch.Path = labelProjectFile
		case projKey != "" && strings.HasPrefix(q, projKey+"/"):
			// A descendant other than the approval file is forbidden.
			outside = true
			ch.Path = labelProjectDir + strings.TrimPrefix(q, projKey)
		case projKey == "" && inHomeProjects(q):
			// Without a derivation a home-project change cannot be
			// attributed: never guess a mismatch.
			unattributed = true
		default:
			// Any other outside change, another project and the creation of
			// the projects directory itself included.
			outside = true
		}
		// A path the redaction would change (or cannot carry) is not a
		// faithful change list: record it redacted and fail closed.
		if clean := red.String(ch.Path); !utf8.ValidString(ch.Path) || clean != ch.Path {
			faithful = false
			ch.Path = clean
		}
		ap.Changes = append(ap.Changes, ch.CaptureApprovalChange)
	}
	sort.SliceStable(ap.Changes, func(i, j int) bool { return ap.Changes[i].Path < ap.Changes[j].Path })
	if !faithful {
		ap.InventoryComplete = false
	}
	ap.Scope = ScopeUnverifiable
	if outside {
		ap.Scope = ScopeOutside
	}
	var reason string
	switch {
	case run.interrupted:
		reason = ReasonInterrupted
	case run.cleanup.Error != nil:
		reason = ReasonCleanupFailed
	case outside:
		reason = ReasonCursorOutside + ": the enable command may have changed state outside the workspace; investigate before another attempt (nothing was restored)"
	case !cleanStage(ap.Stage):
		reason = ReasonCursorApprovalFailed + ": the enable command did not exit 0 by itself"
	case !post.complete:
		reason = ReasonCursorScopeUnverified + ": the inventory after the command is incomplete (" + post.why + ")"
	case !faithful:
		reason = ReasonCursorScopeUnverified + ": a changed path cannot be recorded faithfully"
	case permission:
		reason = ReasonCursorScopeUnverified + ": the harness-written " + labelToolPermission + " changed during the enable command (it is never approval evidence)"
	case unattributed:
		reason = ReasonCursorScopeUnverified + ": a change under " + labelHomeProjects + "/ cannot be attributed: " + derr.Error()
	case len(changes) == 0:
		reason = ReasonCursorScopeUnverified + ": no change gives positive evidence of a workspace approval"
	case !workspaceFiles:
		reason = ReasonCursorScopeUnverified + ": a change is not a regular file under <workspace>/.cursor/"
	case projDir != projFile:
		reason = ReasonCursorScopeUnverified + ": the project approval needs both the computed project directory and its " + cursorApprovalsFile + " added"
	case projDir:
		if why := sc.projectApprovals(filepath.Join(p.home, ".cursor", "projects", slug, cursorApprovalsFile)); why != "" {
			reason = ReasonCursorScopeUnverified + ": the project approval file " + why
			break
		}
		ap.Scope, ap.Reason = ScopeProjectScoped, nil
		ap.Project = &CaptureApprovalProject{Adapter: CursorProjectAdapter, Directory: labelProjectDir, File: labelProjectFile, ProbeEntryPresent: true}
		fmt.Fprintf(l.log, "mcpqual: %s: approval project-scoped (%s): %s added, inventory policy %s excluding %s\n", l.id, CursorProjectAdapter, labelProjectFile,
			InventoryPolicyV2, strings.Join(InventoryExcludedPaths(), " and "))
		return ap, ""
	default:
		ap.Scope, ap.Reason = ScopeWorkspaceOnly, nil
		fmt.Fprintf(l.log, "mcpqual: %s: approval workspace-only, inventory policy %s excluding %s\n", l.id, InventoryPolicyV2, strings.Join(InventoryExcludedPaths(), " and "))
		return ap, ""
	}
	ap.Reason = sptr(reason)
	return ap, reason
}

// rereadConfig is the generated .cursor/mcp.json after a scoped approval,
// within the case-file bound, with the substituted paths labeled again (as
// the configuration snapshot labels them), so evidence shows the
// configuration the session will actually read.
func (p *cursorPreparer) rereadConfig(in caseInputs, server string) ([]byte, error) {
	info, err := p.fsys.Lstat(in.configPath)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() || info.Size() > MaxCaseFileBytes {
		return nil, fmt.Errorf("it is not a regular file of at most %d bytes", MaxCaseFileBytes)
	}
	f, err := p.fsys.Open(in.configPath)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	b, err := readLimited(f, path.Base(in.configPath), MaxCaseFileBytes)
	if err != nil {
		return nil, err
	}
	esc := func(s string) string { q := quoteJSON(s); return q[1 : len(q)-1] }
	pairs := [][2]string{{esc(in.casePath), "<workspace>/case.json"}, {esc(in.eventsPath), "<workspace>/server-events.jsonl"}, {esc(server), "<server>"}, {esc(in.ws), labelWorkspace}}
	sort.SliceStable(pairs, func(i, j int) bool { return len(pairs[i][0]) > len(pairs[j][0]) })
	s := string(b)
	for _, q := range pairs {
		if q[0] != "" {
			s = strings.ReplaceAll(s, q[0], q[1])
		}
	}
	return []byte(s), nil
}

// qualifyPrep is one qualify case's preparation in progress: its report
// record, the permission file's identity and the projects baseline taken
// immediately before the model launch.
type qualifyPrep struct {
	rec        *CursorPreparation
	ident      fs.FileInfo
	residuePre map[string]fs.FileInfo
}

// newPreparationRecord is a preparation record before preparation starts:
// unverified, nothing checked.
func newPreparationRecord() *CursorPreparation {
	return &CursorPreparation{State: PreparationUnverified, Reason: sptr("not reached"), Approval: *newApproval("not reached"),
		Residue: notCheckedResidue("the session did not run")}
}

// prepareQualifyCase prepares qualify case spec's fresh workspace in
// (design decoder-enrollment B3, FP-24), its time within deadline: the
// residue parent slug bound (FP-26), the permission file, the one enable
// command with the shared inventories (its output retained as the case's
// approval-stdout.txt and approval-stderr.txt evidence), the read after
// enable and the configuration re-read. Nothing from another workspace is
// copied or reused. It returns the record and "" when the case may
// proceed to its model launch, otherwise the stop reason (no model
// launch).
func (m *measure) prepareQualifyCase(ctx context.Context, spec caseSpec, in caseInputs, deadline time.Time) (*qualifyPrep, string) {
	p, r := m.prep, m.r
	q := &qualifyPrep{rec: newPreparationRecord()}
	fail := func(reason string) (*qualifyPrep, string) {
		q.rec.Reason = sptr(reason)
		return q, reason
	}
	// The observed residue names cut the slug at 57 characters: a parent
	// slug longer than 54 leaves fewer than two case characters, so the
	// session's residue could never be attributed. Never guess another
	// truncation (r0.13 post-final clarification DW1).
	if parent, _, err := p.parentSlug(in.ws); err == nil && len(parent) > MaxResidueParentSlug {
		q.rec.Approval = *newApproval(ReasonResidueParentSlug)
		return fail(ReasonResidueParentSlug)
	}
	if reason := m.budgetReason(ctx, deadline); reason != "" {
		q.rec.Approval = *newApproval(reason)
		return fail(reason)
	}
	ident, reason := p.writePermission(in)
	if reason == "" {
		q.ident, q.rec.ToolPermission = ident, toolPermissionRecord()
		reason = m.budgetReason(ctx, deadline)
	}
	if reason != "" {
		q.rec.Approval = *newApproval(reason)
		return fail(reason)
	}
	faithful := true
	l := approvalLaunch{r: r, executable: m.pc.Executable, env: in.proc.Env, what: spec.id + " approval", id: m.pc.ID, log: r.Log}
	ap, reason := p.approve(ctx, l, in, deadline, true, func(o approvalOutput) error {
		for _, f := range []struct {
			name     string
			raw      []byte
			withheld int
			cut      bool
			dst      **string
		}{{FileApprovalStdout, o.stdout, o.withheldOut, o.outCut, &q.rec.ApprovalStdout}, {FileApprovalStderr, o.stderr, o.withheldErr, o.errCut, &q.rec.ApprovalStderr}} {
			clean, omitted := captureText(r.redactor, f.raw)
			rel, cut, err := r.putEvidence(path.Join("cases", spec.id, f.name), clean, evidenceSanitized)
			if err != nil {
				return errors.New("the enable command's output cannot be written")
			}
			*f.dst = sptr(rel)
			faithful = faithful && omitted+f.withheld == 0 && !f.cut && !cut
		}
		return nil
	})
	q.rec.Approval = *ap
	switch {
	case reason != "":
		return fail(reason)
	case !faithful:
		return fail(ReasonCursorScopeUnverified + ": the enable command's output cannot be retained faithfully (lines withheld, omitted or cut)")
	}
	if reason := m.budgetReason(ctx, deadline); reason != "" {
		return fail(reason)
	}
	if !p.verifyPermission(in, ident) {
		q.rec.ToolPermission.Reason = sptr(permissionFailed)
		return fail(permissionChanged)
	}
	q.rec.PermissionChecks.AfterEnable = true
	if reason := m.budgetReason(ctx, deadline); reason != "" {
		return fail(reason)
	}
	if _, err := p.rereadConfig(in, r.ServerPath); err != nil {
		return fail(ReasonConfigFailure + ": the generated " + CursorTrustedPath + " cannot be re-read after the approval (" + err.Error() + ")")
	}
	if reason := m.budgetReason(ctx, deadline); reason != "" {
		return fail(reason)
	}
	return q, ""
}

// budgetReason is why the case may not go on after a preparation, residue
// or verification operation ("" when it may): the run's cancellation, then
// the case deadline (max_case_ms) or the client deadline it is capped by
// (code review B3 round 1, C1: every operation counts against the case's
// budget, and an expired deadline launches nothing further).
func (m *measure) budgetReason(ctx context.Context, deadline time.Time) string {
	now := m.r.Clock.Now()
	switch {
	case ctx.Err() != nil:
		return ReasonInterrupted
	case now.Before(deadline):
		return ""
	case now.Before(m.deadline):
		return ReasonBudget + ": max_case_ms"
	}
	// The client's own budget is spent (whichever deadline came first):
	// the client stops.
	return ReasonClientBudget
}

// beforeSession is the permission file's read and the projects baseline
// immediately before the model launch; any deviation blocks the launch.
func (m *measure) beforeSession(ctx context.Context, q *qualifyPrep, in caseInputs, deadline time.Time) string {
	if !m.prep.verifyPermission(in, q.ident) {
		q.rec.ToolPermission.Reason = sptr(permissionFailed)
		q.rec.Reason = sptr(permissionChanged)
		return permissionChanged
	}
	q.rec.PermissionChecks.BeforeSession = true
	if reason := m.budgetReason(ctx, deadline); reason != "" {
		q.rec.Reason = sptr(reason)
		return reason
	}
	pre, err := m.prep.residueSnapshot()
	if err != nil {
		reason := ReasonCursorScopeUnverified + ": worker residue check failed: " + err.Error()
		q.rec.Residue, q.rec.Reason = notCheckedResidue(reason), sptr(reason)
		return reason
	}
	q.residuePre = pre
	if reason := m.budgetReason(ctx, deadline); reason != "" {
		q.rec.Reason = sptr(reason)
		return reason
	}
	return ""
}

// afterSession completes case cs's preparation after the session's
// process-group cleanup: the third permission read, the worker residue
// check of a cleanly reaped session, then the verdict. Verified needs the
// scoped clean enable, all three reads, a clean session and cleanup and
// verified residue; anything else makes a provisional conclusive outcome
// inconclusive (an interruption or cleanup reason keeps its precedence)
// and preserves every recorded piece of evidence.
func (m *measure) afterSession(ctx context.Context, cs *CaseReport, q *qualifyPrep, in caseInputs, run caseRun, deadline time.Time) {
	rec, tp := q.rec, q.rec.ToolPermission
	if m.prep.verifyPermission(in, q.ident) {
		rec.PermissionChecks.AfterCleanup = true
	}
	// The final verification counts against the case's budget too (code
	// review B3 round 1, C1): an expired deadline or a cancellation after
	// the read checks nothing further and verifies nothing.
	expired := m.budgetReason(ctx, deadline)
	clean := run.cleanup.Error == nil && run.cleanup.GroupGone
	switch {
	case expired != "":
		rec.Residue = notCheckedResidue(ReasonCursorScopeUnverified + ": worker residue not checked: " + expired)
	case clean:
		rec.Residue = m.prep.checkResidue(cs.CaseID, in.ws, q.residuePre)
		expired = m.budgetReason(ctx, deadline)
	default:
		rec.Residue = notCheckedResidue(ReasonCursorScopeUnverified + ": worker residue not checked: the session's cleanup is not proven")
	}
	ck := rec.PermissionChecks
	reason := ""
	switch {
	case !ck.AfterCleanup:
		tp.Reason = sptr(permissionFailed)
		reason = permissionChanged
	case run.interrupted:
		reason = ReasonInterrupted
	case !clean:
		reason = ReasonCleanupFailed
	case run.watchdog || run.exit == nil || *run.exit != 0 || run.signal != nil:
		reason = ReasonCursorScopeUnverified + ": the session did not exit 0 by itself"
	case expired != "":
		reason = expired
	case rec.Residue.State != ResidueVerified:
		reason = *rec.Residue.Reason
	}
	if ck.AfterEnable && ck.BeforeSession && ck.AfterCleanup {
		tp.State, tp.Reason = PermissionVerified, nil
	}
	if reason != "" {
		rec.State, rec.Reason = PreparationUnverified, sptr(reason)
		if conclusiveCase(cs.Outcome) {
			cs.Outcome, cs.Reason = OutcomeInconclusive, sptr(reason)
		}
		return
	}
	rec.State, rec.Reason = PreparationVerified, nil
}
