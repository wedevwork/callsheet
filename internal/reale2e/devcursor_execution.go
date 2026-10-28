package reale2e

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"time"
)

// Cursor run wiring (design 12a-3-cursor-worker, slice 3b FP-2 and FP-3).
// A Cursor-selected run, the qualification probe, a standalone full or
// light attempt or each child of a combined exercise, carries its own
// cursorRun: the explicit executable's binding (its tokenized resolved
// target and that file's SHA-256 before any vendor execution, rechecked
// after cleanup; design 12a-3c-cursor-binding) and the
// report-only shallow cache/tmp samples taken right after the baseline
// fingerprint and alongside the final one. This is internal state, not a
// worker protocol change: the Cursor recipe, minimum version, posture,
// prompts and protected-change policy are unchanged, and nothing here is
// a jail or a confinement claim.

// cursorRun is one Cursor-selected run's binding and samples.
type cursorRun struct {
	// exe is the explicit absolute --cursor path as given (never looked up
	// on PATH): every invocation and every identity recheck uses it.
	exe string
	// frozen is the identity bound before any vendor execution (design
	// 12a-3c-cursor-binding; unset when it could not be acquired), and
	// failed latches an observed binding failure for the rest of the run:
	// a later observation never clears it.
	frozen cursorIdentity
	failed bool
	// binding is the executable's tokenized resolved path and its hashes.
	binding DevCursorBinding
	// roots, fs, uid and output are the shallow observer's inputs (tests
	// replace them); before and after are its two records.
	roots  []activityRootSpec
	fs     activityFS
	uid    uint32
	output int
	before *ActivityAggregate
	after  *ActivityAggregate
}

// newCursorRun is the run state of the explicit Cursor executable exe
// over w: the three named roots (~/.cache, /tmp, /var/tmp) and the live
// filesystem. Each root's Path is the spelled root the observer lstats;
// the cache root is tokened in the tokenizer's canonical home spelling (a
// home reached through a symlink stays $HOME/.cache), never by resolving
// .cache itself.
func newCursorRun(w *World, exe string) *cursorRun {
	return &cursorRun{exe: exe, roots: cursorActivityRoots(w.Home), fs: liveActivityFS{}, uid: uint32(os.Getuid())}
}

// cursorActivityRoots are the three named sample roots for home.
func cursorActivityRoots(home string) []activityRootSpec {
	tok := pathTokenizer{home: home}.canonical()
	var roots []activityRootSpec
	for _, r := range []struct{ scope, path, token string }{
		{activityScopeCache, filepath.Join(home, ".cache"), filepath.Join(tok.home, ".cache")}, {activityScopeTmp, "/tmp", "/tmp"},
		{activityScopeVarTmp, "/var/tmp", "/var/tmp"}} {
		roots = append(roots, activityRootSpec{Scope: r.scope, Path: r.path, Token: tok.token(r.token)})
	}
	return roots
}

// cursorExecutableBound is the harness identity's bounded reader limit.
const cursorExecutableBound = 1 << 30

// cursorHashWithin is a file's SHA-256 read through readBounded(limit).
func cursorHashWithin(file string, limit int64) (string, error) {
	b, err := readBounded(file, limit)
	if err != nil {
		return "", err
	}
	return sha256Hex(b), nil
}

// cursorIdentity is the Cursor executable's identity (design
// 12a-3c-cursor-binding, Frozen identity): the strict symlink-resolved
// absolute target of the explicit --cursor path, raw (never tokenized; only
// evidence tokenizes it), and that file's SHA-256. A retarget to a
// byte-identical wrapper changes the path; a byte replacement changes the
// digest. The zero value is unset.
type cursorIdentity struct {
	path, sha256 string
}

// set reports an acquired identity.
func (i cursorIdentity) set() bool { return i.path != "" && i.sha256 != "" }

// sameTarget compares the resolved targets (the one path comparison every
// binding gate uses).
func (i cursorIdentity) sameTarget(o cursorIdentity) bool { return i.path == o.path }

// equal compares both components.
func (i cursorIdentity) equal(o cursorIdentity) bool { return i.sameTarget(o) && i.sha256 == o.sha256 }

// readCursorIdentity reads exe's identity: strict filepath.EvalSymlinks of
// the absolute path (no nonexistent-tail fallback, no PATH lookup), the
// bounded SHA-256 of that resolved file, then exe resolved again, which
// must still name the same target. Any resolution or read error is an
// identity failure; the returned identity then carries the resolved target
// when one was observed and never a digest.
func readCursorIdentity(exe string) (cursorIdentity, error) {
	return readCursorIdentityWithin(exe, cursorExecutableBound)
}

// readCursorIdentityWithin is readCursorIdentity under a read limit.
func readCursorIdentityWithin(exe string, limit int64) (cursorIdentity, error) {
	if !filepath.IsAbs(exe) {
		return cursorIdentity{}, fmt.Errorf("the Cursor executable path %q is not absolute", exe)
	}
	target, err := filepath.EvalSymlinks(exe)
	if err != nil {
		return cursorIdentity{}, err
	}
	id := cursorIdentity{path: target}
	h, err := cursorHashWithin(target, limit)
	if err != nil {
		return id, err
	}
	again, err := filepath.EvalSymlinks(exe)
	if err != nil {
		return id, err
	}
	if again != target {
		return id, errors.New("the Cursor executable was retargeted while it was read")
	}
	id.sha256 = h
	return id, nil
}

// cursorAfterDigest is the after_sha256 an observation stores against
// frozen (design 12a-3c-cursor-binding, Evidence and compatibility): the
// actual wrapper digest only when exe still resolves to the frozen target,
// so a same-path byte replacement keeps its unequal digest; empty for a
// changed target, an unreadable identity, or an earlier latched failure
// that a restored digest would otherwise erase. It reports whether the
// observation is itself a binding failure.
func cursorAfterDigest(exe string, frozen cursorIdentity, latched bool) (string, bool) {
	now, err := readCursorIdentity(exe)
	switch {
	case err != nil || !now.sameTarget(frozen):
		return "", true
	case !now.equal(frozen):
		return now.sha256, true
	case latched:
		return "", false
	}
	return now.sha256, false
}

// cursorSteps inserts a Cursor run's two early steps into a setup list
// whose first four steps are the permission template, the harness hash,
// the protected roots and the baseline fingerprint: the executable
// binding right after the harness hash (before any vendor execution) and
// the before sample right after the baseline fingerprint.
func (d *devSupervisor) cursorSteps(steps []func() error) []func() error {
	if d.cursor == nil {
		return steps
	}
	return slices.Concat(steps[:2], []func() error{d.cursorBindBefore}, steps[2:4], []func() error{d.cursorActivityBefore}, steps[4:])
}

// cursorBindBefore freezes the explicit Cursor executable's identity
// before any vendor execution and records its binding: the tokenized
// resolved target (the Cursor version record's path) and that file's
// SHA-256. An unresolvable or unreadable identity, or (in a combined
// exercise child) one that is not the root's frozen identity, stops the
// run before any vendor (cursor_executable_mismatch); a child that resolved
// an identity keeps its own so the root comparison also exposes the
// disagreement.
func (d *devSupervisor) cursorBindBefore() error {
	c := d.cursor
	id, err := readCursorIdentity(c.exe)
	c.binding = DevCursorBinding{}
	if id.path != "" {
		c.binding.Path = d.s.tok(id.path)
	}
	if err != nil {
		c.failed = true
		return failure(CodeCursorExecutableMismatch, fmt.Errorf("the Cursor executable's identity could not be read before any vendor execution: %w", err))
	}
	c.frozen, c.binding.BeforeSHA256 = id, id.sha256
	if want := d.s.args.cursorExpect; want.set() && !id.equal(want) {
		c.failed = true
		return failure(CodeCursorExecutableMismatch, errors.New("the Cursor executable is not the one the combined exercise bound before its authorization"))
	}
	return nil
}

// cursorBindAfter rechecks the original --cursor path after cleanup: a
// changed target, an unreadable identity or changed bytes is a binding
// failure, latched, that keeps the attempt from passing or qualifying
// (an empty or unequal after hash; see cursorAfterDigest).
func (d *devSupervisor) cursorBindAfter() {
	c := d.cursor
	if c == nil || !c.frozen.set() {
		return
	}
	after, failed := cursorAfterDigest(c.exe, c.frozen, c.failed)
	c.binding.AfterSHA256 = after
	c.failed = c.failed || failed
}

// cursorBindingFailed reports a Cursor run that observed a binding failure
// (false for a default run).
func (d *devSupervisor) cursorBindingFailed() bool {
	return d.cursor != nil && d.cursor.failed
}

// cursorObserve takes one bounded shallow sample: the 5s budget clipped
// to the existing deadline, the run's evidence and runtime excluded.
func (d *devSupervisor) cursorObserve(phase string, clip time.Time) *ActivityAggregate {
	c := d.cursor
	var exclude []string
	for _, x := range []string{d.s.evidenceDir, d.s.runtime} {
		if x == "" {
			continue
		}
		if cx, err := canonicalPath(x); err == nil {
			x = cx
		}
		exclude = append(exclude, x)
	}
	o := activityObserver{fs: c.fs, clock: d.s.w.Clock, uid: c.uid, exclude: exclude, output: c.output}
	deadline := minTime(d.s.w.Clock.Now().Add(activityScanBudget), clip)
	return o.observe(d.s.runID, phase, c.roots, deadline)
}

// cursorActivityBefore samples the roots right after the baseline
// fingerprint, before any vendor execution. Sample quality never fails
// the run; only an evidence write failure does (the existing evidence
// failure).
func (d *devSupervisor) cursorActivityBefore() error {
	c := d.cursor
	c.before = d.cursorObserve("before", d.attemptDeadline())
	if !d.writeActivity(cursorActivityFiles[0], c.before) {
		return d.halt()
	}
	return nil
}

// cursorEnvironment samples the roots again alongside the after-cleanup
// fingerprint (when the before sample exists): an early stopped run keeps
// only its before sample and is incomplete, never zero activity. The
// samples never change the strict comparison's outcome.
func (d *devSupervisor) cursorEnvironment() {
	c := d.cursor
	if c == nil || c.before == nil {
		return
	}
	c.after = d.cursorObserve("after", d.s.cleanupBy)
	d.writeActivity(cursorActivityFiles[1], c.after)
}

// cursorSelectionBinding rechecks the frozen selection against the bound
// command: a Cursor-coder selection needs a bound --cursor executable and
// is exactly the shipped coder-only selection; any other selection must
// not be bound to one.
func (d *devSupervisor) cursorSelectionBinding(flow DevFullFlow) error {
	bound := d.cursor != nil
	switch {
	case flow.CursorCoder() && !bound:
		return failure(CodeCursorAdapterRequired, errors.New("the selected coder uses the cursor adapter but no --cursor executable is bound"))
	case !flow.CursorCoder() && bound:
		return failure(CodeCursorAdapterUnused, errors.New("--cursor is bound but the selected coder does not use the cursor adapter"))
	case bound && !cursorSelectionOK(flow):
		// Only the shipped coder-only cursor / grok-4.7 / high selection can
		// produce Cursor evidence: refused before any vendor or paid work.
		return failure(CodeCursorSelectionMismatch, errors.New("a Cursor selection must be exactly the shipped coder-only cursor grok-4.7/high flow"))
	}
	return nil
}

// versionVendors are the vendor version records in order: a Cursor run's
// four, otherwise the existing three.
func (d *devSupervisor) versionVendors() []string {
	if d.cursor != nil {
		return cursorVersionVendors
	}
	return defaultVersionVendor
}

// attemptLayout is the closed layout the collector loads its own attempt
// evidence under: the Cursor attempt layout for a Cursor run.
func (d *devSupervisor) attemptLayout() devLayout {
	if d.cursor != nil {
		return cursorAttemptLayout(d.scenario())
	}
	return devLayoutFor(devBundleV2, d.scenario())
}

// manifestSchema is the attempt manifest's outer schema.
func (d *devSupervisor) manifestSchema() string {
	if d.cursor != nil {
		return DevCursorBundleSchema
	}
	return DevBundleSchema
}

// cursorManifestBinding is the manifest's Cursor binding (nil for a
// default run, which never carries the field).
func (d *devSupervisor) cursorManifestBinding() *DevCursorBinding {
	if d.cursor == nil {
		return nil
	}
	b := d.cursor.binding
	return &b
}
