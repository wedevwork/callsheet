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
// cursorRun: the explicit executable's binding (its tokenized path and
// SHA-256 before any vendor execution and after cleanup) and the
// report-only shallow cache/tmp samples taken right after the baseline
// fingerprint and alongside the final one. This is internal state, not a
// worker protocol change: the Cursor recipe, minimum version, posture,
// prompts and protected-change policy are unchanged, and nothing here is
// a jail or a confinement claim.

// cursorRun is one Cursor-selected run's binding and samples.
type cursorRun struct {
	// exe is the explicit absolute --cursor path, resolved once per
	// invocation (never looked up on PATH).
	exe string
	// binding is the executable's tokenized path and its hashes.
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

// cursorExecutableHash is the explicit executable's SHA-256 under the
// harness identity's bounded reader (1 GiB).
func cursorExecutableHash(exe string) (string, error) {
	b, err := readBounded(exe, 1<<30)
	if err != nil {
		return "", err
	}
	return sha256Hex(b), nil
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

// cursorBindBefore records the explicit Cursor executable's binding
// before any vendor execution: its tokenized path (the Cursor version
// record's path) and its SHA-256. An unreadable executable, or (in a
// combined exercise child) one whose hash is not the root's frozen
// binding, stops the run before any vendor (cursor_executable_mismatch).
func (d *devSupervisor) cursorBindBefore() error {
	c := d.cursor
	c.binding = DevCursorBinding{Path: d.s.tok(c.exe)}
	h, err := cursorExecutableHash(c.exe)
	if err != nil {
		return failure(CodeCursorExecutableMismatch, fmt.Errorf("the Cursor executable could not be hashed before any vendor execution: %w", err))
	}
	c.binding.BeforeSHA256 = h
	if want := d.s.args.cursorExpect; want != "" && h != want {
		return failure(CodeCursorExecutableMismatch, errors.New("the Cursor executable is not the one the combined exercise bound before its authorization"))
	}
	return nil
}

// cursorBindAfter hashes the executable again after cleanup; an
// unreadable executable keeps an empty after hash (incomplete evidence,
// never a pass).
func (d *devSupervisor) cursorBindAfter() {
	c := d.cursor
	if c == nil || c.binding.BeforeSHA256 == "" {
		return
	}
	if h, err := cursorExecutableHash(c.exe); err == nil {
		c.binding.AfterSHA256 = h
	}
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
