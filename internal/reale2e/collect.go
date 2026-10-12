package reale2e

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/go-git/go-git/v5/plumbing"

	"github.com/wedevwork/callsheet/internal/contract"
)

// Collection, cleanup, the final fingerprint and the judgement (design
// 12a-real-e2e, Evidence and checker, Failure and lifecycle, A3): a still
// running coordinator phase is stopped by its captured process group and
// observed; active tasks are cancelled through the plane and awaited; the
// session evidence (raw streams, transcript snapshots, the supervisor's
// own process observation and the machine-derived event map), the raw
// wait files, the evidence repository and manifests are written; the
// owned children are stopped by handle with their exits verified; the
// runtime is removed only when every child was proved gone (ownership
// marker and canonical path, never through a symlink); then the owner
// environment is fingerprinted again and compared, and the bundle is
// judged by the offline checker and audited. Collection is automatic: no
// owner file and no finish command.

// finishRun collects, cleans up, fingerprints, judges and audits; its exit
// code is the report's.
func (s *supervisor) finishRun() int {
	s.closing.Store(true)
	if s.loopDone != nil {
		close(s.loopDone)
	}
	// Cleanup still talks to the plane after an interrupt: it keeps the
	// context's values but not its cancellation (each wait has its bound).
	s.ctx = context.WithoutCancel(s.ctx)
	s.ev.event(Event{Type: EvCollectionStarted})
	start := s.w.Clock.Now()
	s.cleanupBy = start.Add(CleanupBound)
	s.stopPhase()
	s.cancelActive()
	s.collectSession()
	s.collectWaits()
	s.writeRepository()
	rec := s.cleanup(start)
	if s.ctl != nil {
		s.ctl.close()
	}
	if s.closePlane != nil {
		s.closePlane()
	}
	if rec.Complete {
		if err := s.removeRuntime(); err != nil {
			fmt.Fprintf(s.stderr, "reale2e: runtime %s kept: %v\n", s.runtime, err)
		} else if s.runtime != "" {
			rec.RuntimeRemoved = true
			s.ev.event(Event{Type: EvRuntimeRemoved})
		}
	}
	rec.Complete = rec.Complete && (rec.RuntimeRemoved || s.runtime == "")
	rec.FinishedAt = formatTime(s.w.Clock.Now())
	s.ev.writeJSON("cleanup.json", rec)
	s.ev.event(Event{Type: EvCleanupFinished})
	s.manifest.Launches = s.launchHashes()
	s.ev.writeJSON("manifest.json", s.manifest)
	s.compareEnvironment()
	if err := s.ev.failure(); err != nil {
		fmt.Fprintf(s.stderr, "reale2e: evidence is incomplete: %v\n", err)
	}
	s.ev.close()
	code := s.judge()
	if !rec.Complete {
		s.recovery(rec)
	}
	return code
}

// launchHashes is the manifest's launch inventory: each launch record
// written, as encoded, in phase order.
func (s *supervisor) launchHashes() []FileHash {
	out := []FileHash{}
	for i, l := range s.launches {
		if l == nil {
			continue
		}
		if b, err := encodeJSON(l); err == nil {
			out = append(out, FileHash{Name: launchFiles[i], SHA256: sha256Hex(b)})
		}
	}
	return out
}

// recovery prints what cleanup could not prove stopped.
func (s *supervisor) recovery(rec CleanupRecord) {
	if s.runtime != "" {
		s.say("cleanup is unresolved: the runtime %s is kept.", s.runtime)
	} else {
		s.say("cleanup is unresolved (no runtime was created yet).")
	}
	s.say("Recovery: stop these owned process groups yourself after checking them (PID reuse makes stale PIDs unsafe to signal blindly):")
	for _, p := range rec.Processes {
		if !p.ExitVerified {
			s.say("  %s pid %d", p.Name, p.PID)
		}
	}
	for _, t := range rec.UnsettledTasks {
		s.say("  task %s did not settle", t)
	}
}

// left is the time remaining before the cleanup bound, at most max.
func (s *supervisor) left(max time.Duration) time.Duration {
	return min(max, maxDuration(0, s.cleanupBy.Sub(s.w.Clock.Now())))
}

func maxDuration(a, b time.Duration) time.Duration {
	if a > b {
		return a
	}
	return b
}

// childNamed returns the tracked child named name.
func (s *supervisor) childNamed(name string) *child {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, c := range s.children {
		if c.name == name {
			return c
		}
	}
	return nil
}

// stopPhase stops a still running coordinator phase by its captured
// process group (its MCP child and background commands with it) and
// records the observation.
func (s *supervisor) stopPhase() {
	p := s.current()
	if p == nil {
		return
	}
	c := s.childNamed(phaseHandle(p.phase))
	rec := stopProc(phaseHandle(p.phase), p.proc, s.w.Clock, s.left(15*time.Second))
	if c != nil {
		s.stopped(c, rec)
	}
	p.stopped = true
	s.observePhase(p, rec.StoppedBy, s.left(groupBound))
}

// collectSession writes the supervisor's coordinator process observation
// and the machine-derived event map.
func (s *supervisor) collectSession() {
	if s.phases[0] == nil {
		return
	}
	ce := CoordinatorExit{Schema: CoordExitSchema, Phases: []PhaseProcess{}}
	for _, p := range s.phases {
		if p != nil && p.handled {
			ce.Phases = append(ce.Phases, p.rec)
		}
	}
	s.ev.writeJSON(coordinatorExitFile, ce)
	em := DeriveSession(s.sessionInput())
	s.ev.writeJSON(eventMapFile, em)
}

// cancelActive cancels every known nonterminal task and awaits its
// terminal cleanup, recording the snapshots of observed hops.
func (s *supervisor) cancelActive() {
	if s.plane == nil {
		return
	}
	for i := range s.hops {
		h := &s.hops[i]
		if h.task == "" || h.terminal {
			continue
		}
		v, err := s.show(h.task, contract.DefaultTailLines)
		if err != nil || !contract.TaskTerminal(v.State) {
			s.cancelTask(h.task)
			var settled bool
			var last *contract.TaskView
			if last, settled = s.settle(h.task, s.left(30*time.Second)); !settled {
				s.unsettled = append(s.unsettled, h.task)
			}
			if last == nil {
				continue
			}
			v = *last
		}
		h.terminal, h.view = true, &v
		dir := "hops/" + hopDir(i)
		s.storeTask(dir, v)
		s.storeLogs(dir, v.TaskID)
		s.ev.event(Event{Type: EvTaskTerminal, Hop: Hops[i], Task: v.TaskID})
	}
	// Tasks cancelled outside the chain (a premature or extra admission)
	// are awaited to their terminal cleanup too.
	seen := map[string]bool{}
	for _, h := range s.hops {
		seen[h.task] = true
	}
	for _, id := range s.cancelled {
		if seen[id] {
			continue
		}
		seen[id] = true
		if _, settled := s.settle(id, s.left(30*time.Second)); !settled && !slices.Contains(s.unsettled, id) {
			s.unsettled = append(s.unsettled, id)
		}
	}
}

// Raw wait file names in a hop's evidence.
const (
	waitResultFile = "wait-result.json"
	waitStderrFile = "wait-stderr.txt"
	waitExitFile   = "wait-exit.txt"
)

// readWaitFiles reads hop i's raw wait output from the runtime.
func (s *supervisor) readWaitFiles(i int) (result, stderr, exit []byte, okR, okE, okX bool) {
	if s.runtime == "" {
		return
	}
	dir := filepath.Join(s.runtime, "waits", hopDir(i))
	read := func(name string, limit int64) ([]byte, bool) {
		p := filepath.Join(dir, name)
		st, err := os.Lstat(p)
		if err != nil || !st.Mode().IsRegular() {
			return nil, false
		}
		b, err := readBounded(p, limit)
		return b, err == nil
	}
	result, okR = read("result.json", maxTextFile)
	stderr, okE = read("error.txt", maxTextFile)
	exit, okX = read("exit-code", 64)
	return
}

// waitRecordOf derives a wait record from the raw wait output (the
// unchanged collector rule, reparsing the terminal JSON independently).
func waitRecordOf(hop int, task string, result, stderr, exit []byte, okR, okX bool) WaitRecord {
	w := WaitRecord{Schema: WaitSchema, Hop: Hops[hop], Handle: waitHandle(hop), Task: task}
	if okX {
		if n, err := strconv.Atoi(strings.TrimSpace(string(exit))); err == nil {
			w.ExitCode = &n
		}
	}
	if okR {
		w.ResultSHA256 = sha256Hex(result)
		if wr, err := contract.ParseWaitResponse([]byte(strings.TrimSpace(string(result))), []string{task}, 40*time.Second); err == nil &&
			wr.Status == contract.WaitTerminal && wr.Task != nil {
			w.Winner, w.WinnerState = wr.Winner, wr.Task.State
		}
	}
	w.StderrBytes = len(stderr)
	return w
}

// waitRecord is hop i's current wait record.
func (s *supervisor) waitRecord(i int) WaitRecord {
	r, e, x, okR, _, okX := s.readWaitFiles(i)
	return waitRecordOf(i, s.hops[i].task, r, e, x, okR, okX)
}

// collectWaits records each observed hop's background wait from the files
// its own redirected command wrote, with those raw files.
func (s *supervisor) collectWaits() {
	if s.runtime == "" {
		return
	}
	for i := range s.hops {
		h := s.hops[i]
		if h.task == "" {
			continue
		}
		r, e, x, okR, okE, okX := s.readWaitFiles(i)
		dir := "hops/" + hopDir(i) + "/"
		if okR {
			s.ev.writeFile(dir+waitResultFile, []byte(toValidUTF8(r)))
		}
		if okE {
			s.ev.writeFile(dir+waitStderrFile, []byte(toValidUTF8(e)))
		}
		if okX {
			s.ev.writeFile(dir+waitExitFile, []byte(toValidUTF8(x)))
		}
		s.ev.writeJSON(dir+"wait.json", waitRecordOf(i, h.task, r, e, x, okR, okX))
	}
}

// writeRepository writes the evidence repository and tree manifest for
// the seed and every accepted result.
func (s *supervisor) writeRepository() {
	if s.seed == nil {
		return
	}
	tm := TreeManifest{Schema: TreeSchema, SeedCommit: s.seed.Commit.String(), SeedTree: s.seed.Tree.String(), Hops: []HopTree{}}
	commits := []plumbing.Hash{s.seed.Commit}
	parent := s.seed.Commit
	for i, h := range s.hops {
		if h.commit.IsZero() {
			break
		}
		commits = append(commits, h.commit)
		tm.Hops = append(tm.Hops, HopTree{Hop: Hops[i], Task: h.task, Commit: h.commit.String(), Parent: parent.String(), Tree: h.tree.String()})
		parent = h.commit
	}
	if err := writeEvidenceRepo(filepath.Join(s.ev.root, "workspace", "repo.git"), s.collect.Storer, commits); err != nil {
		s.fail("evidence_write_failed", err)
	}
	s.ev.writeJSON("workspace/tree-manifest.json", tm)
}

// cleanup stops every owned child by its handle (the coordinator phases,
// sidecars, then the plane) and returns the cleanup record; it is
// complete only when every task settled, every child's exit was verified
// and every coordinator phase's whole group was observed gone.
func (s *supervisor) cleanup(start time.Time) CleanupRecord {
	s.ev.event(Event{Type: EvCleanupStarted})
	s.mu.Lock()
	children := append([]*child(nil), s.children...)
	s.mu.Unlock()
	for i := len(children) - 1; i >= 0; i-- {
		c := children[i]
		s.mu.Lock()
		done := s.stoppedSet[c]
		s.mu.Unlock()
		if !done {
			s.stopped(c, stopProc(c.name, c.proc, s.w.Clock, s.left(15*time.Second)))
		}
	}
	rec := CleanupRecord{Schema: CleanupSchema, Processes: append([]ProcRecord{}, s.records...), CancelledTasks: append([]string{}, s.cancelled...),
		UnsettledTasks: append([]string{}, s.unsettled...), StartedAt: formatTime(start), Complete: len(s.unsettled) == 0}
	for _, p := range rec.Processes {
		rec.Complete = rec.Complete && p.ExitVerified
	}
	for _, p := range s.phases {
		if p != nil {
			rec.Complete = rec.Complete && p.handled && p.rec.GroupGone
		}
	}
	return rec
}

// compareEnvironment takes the final fingerprint and writes the diff
// (when a baseline was taken): every protected difference is recorded,
// whatever else failed.
func (s *supervisor) compareEnvironment() {
	if s.before == nil {
		return
	}
	after := s.snapshot("after", s.cleanupBy)
	diff := CompareEnvironment(s.before, after, s.manifest.ProtectedRoots)
	for _, phase := range []string{"before", "after"} {
		for _, name := range envFiles(phase) {
			sum := ""
			if b, err := readBounded(filepath.Join(s.ev.root, filepath.FromSlash(name)), maxShardBytes); err == nil {
				sum = sha256Hex(b)
			}
			fh := FileHash{Name: name, SHA256: sum}
			if phase == "before" {
				diff.Before = append(diff.Before, fh)
			} else {
				diff.After = append(diff.After, fh)
			}
		}
	}
	data, err := encodeDiff(diff)
	if err != nil {
		diff = EnvDiff{Schema: DiffSchema, ProtectedRoots: diff.ProtectedRoots, Before: diff.Before, After: diff.After, Codes: []string{CodeEnvLimit},
			Protected: []EnvChange{}, Housekeeping: []EnvChange{}, OversizedConfig: []string{}, Detail: []EnvDetail{}}
		data, _ = encodeDiff(diff)
	}
	s.ev.writeFile("environment/diff.json", data)
	for _, c := range diff.Protected {
		s.say("PROTECTED CHANGE (%s): %s %s %s", CodeEnvModified, c.Scope, c.Path, c.Kind)
	}
	if len(diff.Housekeeping) > 0 {
		s.say("%d vendor housekeeping changes (report-only), listed in report.txt", len(diff.Housekeeping))
	}
}

// judge runs the offline checker on the bundle, writes report.json and
// report.txt, then audit.json and audit.md from that report, and prints
// the summary and the paths.
func (s *supervisor) judge() int {
	b, err := LoadBundle(s.ev.root)
	var r Report
	var a *Audit
	var c *checker
	if err != nil {
		r = schemaReport()
		fmt.Fprintf(s.stderr, "reale2e: %v\n", err)
	} else {
		var cs []Criterion
		cs, c = check(b)
		r = BuildReport(b, cs)
	}
	data, _ := encodeJSON(r)
	text := RenderText(r)
	writes := []struct {
		name string
		data []byte
	}{{"report.json", data}, {"report.txt", []byte(text)}}
	if b != nil {
		au := buildAudit(c, r)
		a = &au
		aj, _ := encodeJSON(au)
		writes = append(writes, struct {
			name string
			data []byte
		}{"audit.json", aj}, struct {
			name string
			data []byte
		}{"audit.md", []byte(RenderAudit(au))})
	}
	for _, w := range writes {
		if err := s.ev.reserve(w.name, len(w.data)); err != nil {
			fmt.Fprintf(s.stderr, "reale2e: %s not written: %v\n", w.name, err)
			continue
		}
		writeAtomic(s.ev.root, w.name, w.data)
	}
	fmt.Fprint(s.stdout, text)
	s.say("report: %s", filepath.Join(s.ev.root, "report.txt"))
	if a != nil {
		s.say("audit: %s (%s)", filepath.Join(s.ev.root, "audit.md"), PendingReview)
	}
	if r.Result == ResultPass {
		return 0
	}
	return 1
}

// removeRuntime deletes the supervisor-created runtime after its children
// were proved gone: the path must be canonical, a direct child of the
// temporary root named ce-*, a real directory (not a symlink) and hold
// the ownership marker with this run's ID. os.RemoveAll never follows a
// symlink inside it.
func (s *supervisor) removeRuntime() error {
	rt := s.runtime
	if rt == "" {
		return nil
	}
	canon, err := filepath.EvalSymlinks(rt)
	if err != nil || canon != rt {
		return errors.New("the runtime path is not canonical")
	}
	root, err := filepath.EvalSymlinks(s.w.TempRoot)
	if err != nil || filepath.Dir(rt) != root || !strings.HasPrefix(filepath.Base(rt), "ce-") {
		return errors.New("the runtime is not a supervisor-created directory")
	}
	st, err := os.Lstat(rt)
	if err != nil || st.Mode()&fs.ModeSymlink != 0 || !st.IsDir() {
		return errors.New("the runtime is not a real directory")
	}
	marker, err := readBounded(filepath.Join(rt, ownerMarker), 128)
	if err != nil || string(marker) != s.runID+"\n" {
		return errors.New("the runtime's ownership marker does not name this run")
	}
	// Read-only receipts and review copies need write permission on
	// their directories only; RemoveAll handles 0400 files.
	if err := os.RemoveAll(rt); err != nil {
		return err
	}
	s.say("runtime %s removed", rt)
	return nil
}
