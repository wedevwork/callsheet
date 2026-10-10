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

// Collection, cleanup and the final judgement (design 12a-real-e2e,
// Evidence and checker, Failure and lifecycle): active tasks are cancelled
// through the plane and awaited, the owner-supplied session files and the
// coordinator's wait records are copied (bounded, regular files only),
// the evidence repository and manifests are written, the owned children
// are stopped by handle with their exits verified, and the bundle is
// judged by the offline checker. The runtime is removed only when every
// child was proved gone, through its ownership marker and canonical path,
// never through a symlink.

// ownerFiles maps the owner directory's files to their evidence names.
var ownerFiles = map[string]string{
	"transcript.jsonl":       "session/transcript.jsonl",
	"event-map.json":         "session/event-map.json",
	"setup-attestation.json": "session/setup-attestation.json",
}

// finishRun collects, cleans up, judges and reports; its exit code is
// the report's.
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
	s.cancelActive()
	owner := s.collectOwnerFiles()
	s.collectWaits()
	s.writeRepository()
	s.ev.writeJSON("manifest.json", s.manifest)
	rec := s.cleanup(start, owner)
	if s.ctl != nil {
		s.ctl.close()
	}
	if s.closePlane != nil {
		s.closePlane()
	}
	if err := s.ev.failure(); err != nil {
		fmt.Fprintf(s.stderr, "reale2e: evidence is incomplete: %v\n", err)
	}
	s.ev.close()
	code := s.judge()
	if rec.Complete {
		if err := s.removeRuntime(); err != nil {
			fmt.Fprintf(s.stderr, "reale2e: runtime %s kept: %v\n", s.runtime, err)
		}
	} else {
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
		if !s.ownerChildrenVerified(rec.Owner) {
			s.say("  the owner-started coordinator session, its MCP child and its background waits are not confirmed stopped: close the session,")
			s.say("  stop its waits, put a complete coordinator-exit.json in %s, then remove the runtime yourself", filepath.Join(s.runtime, "owner"))
		}
	}
	return code
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

// maxOwnerFile bounds one owner-supplied file.
const maxOwnerFile = maxTextFile

// readOwnerFile reads a regular, non-symlinked file of the runtime's
// owner directory.
func (s *supervisor) readOwnerFile(name string) ([]byte, error) {
	p := filepath.Join(s.runtime, "owner", name)
	st, err := os.Lstat(p)
	if err != nil {
		return nil, err
	}
	if !st.Mode().IsRegular() {
		return nil, errors.New("not a regular file")
	}
	return readBounded(p, maxOwnerFile)
}

// collectOwnerFiles copies the owner's session files and returns the
// coordinator exit record, if valid.
func (s *supervisor) collectOwnerFiles() *CoordinatorExit {
	if s.runtime == "" {
		return nil
	}
	for name, rel := range ownerFiles {
		b, err := s.readOwnerFile(name)
		if err != nil {
			s.say("owner file %s not collected: %v", name, err)
			continue
		}
		s.ev.writeFile(rel, b)
	}
	b, err := s.readOwnerFile("coordinator-exit.json")
	if err != nil {
		s.say("owner file coordinator-exit.json not collected: %v", err)
		return nil
	}
	var ce CoordinatorExit
	if err := decodeStrict(b, &ce); err != nil || ce.Schema != CoordExitSchema {
		s.say("owner file coordinator-exit.json is not a version 1 record")
		return nil
	}
	return &ce
}

// collectWaits records each observed hop's background wait from the files
// its own redirected command wrote.
func (s *supervisor) collectWaits() {
	if s.runtime == "" {
		return
	}
	for i := range s.hops {
		h := s.hops[i]
		if h.task == "" {
			continue
		}
		dir := filepath.Join(s.runtime, "waits", hopDir(i))
		w := WaitRecord{Schema: WaitSchema, Hop: Hops[i], Handle: waitHandle(i), Task: h.task}
		if b, err := readBounded(filepath.Join(dir, "exit-code"), 64); err == nil {
			if n, err := strconv.Atoi(strings.TrimSpace(string(b))); err == nil {
				w.ExitCode = &n
			}
		}
		if b, err := readBounded(filepath.Join(dir, "result.json"), maxTextFile); err == nil {
			w.ResultSHA256 = sha256Hex(b)
			if wr, err := contract.ParseWaitResponse([]byte(strings.TrimSpace(string(b))), []string{h.task}, 40*time.Second); err == nil &&
				wr.Status == contract.WaitTerminal && wr.Task != nil {
				w.Winner, w.WinnerState = wr.Winner, wr.Task.State
			}
		}
		if st, err := os.Lstat(filepath.Join(dir, "error.txt")); err == nil && st.Mode().IsRegular() {
			w.StderrBytes = int(st.Size())
		}
		s.ev.writeJSON("hops/"+hopDir(i)+"/wait.json", w)
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

// cleanup stops every owned child by its handle (sidecars, then the
// plane) and writes cleanup.json.
func (s *supervisor) cleanup(start time.Time, owner *CoordinatorExit) CleanupRecord {
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
		UnsettledTasks: append([]string{}, s.unsettled...), Owner: owner, StartedAt: formatTime(start), Complete: len(s.unsettled) == 0}
	for _, p := range rec.Processes {
		rec.Complete = rec.Complete && p.ExitVerified
	}
	rec.Complete = rec.Complete && s.ownerChildrenVerified(owner)
	rec.FinishedAt = formatTime(s.w.Clock.Now())
	s.ev.writeJSON("cleanup.json", rec)
	s.ev.event(Event{Type: EvCleanupFinished})
	return rec
}

// ownerChildrenVerified reports whether the owner-started processes are
// accounted for: nothing to account for when the coordinator was never
// offered a launch; otherwise the owner's exit record must show the
// session closed, its MCP child exited and an exit for the background
// wait of every admitted hop.
func (s *supervisor) ownerChildrenVerified(owner *CoordinatorExit) bool {
	if !s.coordinatorReady {
		return true
	}
	if owner == nil || !owner.SessionClosed || !owner.MCPChildExited {
		return false
	}
	handles := map[string]bool{}
	for _, w := range owner.WaitHandles {
		handles[w.Handle] = true
	}
	for i, h := range s.hops {
		if h.task != "" && !handles[waitHandle(i)] {
			return false
		}
	}
	return true
}

// judge runs the offline checker on the bundle, writes report.json and
// report.txt, and prints the summary.
func (s *supervisor) judge() int {
	b, err := LoadBundle(s.ev.root)
	var r Report
	if err != nil {
		r = schemaReport()
		fmt.Fprintf(s.stderr, "reale2e: %v\n", err)
	} else {
		red := s.red
		red.Run = s.runtime
		r = BuildReport(b, Check(b), red)
	}
	data, _ := encodeJSON(r)
	writeAtomic(s.ev.root, "report.json", data)
	text := RenderText(r)
	writeAtomic(s.ev.root, "report.txt", []byte(text))
	fmt.Fprint(s.stdout, text)
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
