package sidecar

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"time"

	"github.com/wedevwork/callsheet/internal/client"
	"github.com/wedevwork/callsheet/internal/contract"
	"github.com/wedevwork/callsheet/internal/taskworkspace"
	"github.com/wedevwork/callsheet/internal/workspacetransfer"
)

// Workspace execution on the worker (iteration 10b). A workspace start is
// journaled with its binding and answered preparing at once (definitely no
// adapter yet); the worker goroutine then prepares the private checkout
// under a five-minute preparation context (the cache's authorized fetch,
// the trusted copy, the checkout and the owned runtime directory), spawns
// its guardian and reports task_prepared; only the plane's release=true
// opens the adapter release barrier. After the adapter's group is proved
// gone and the final message extracted, a five-minute finalization context
// bounds the result snapshot and the plane's arbitration (Begin); the
// authorized push and its settlement observation follow, each step
// checkpointed in publication.json. No filesystem or network work runs in
// the stream session, and no lock is held while a child runs.

// wsPlane is a workspace task's plane surface: the node transfer session
// (fetch) and the publication endpoints.
type wsPlane interface {
	Remote(taskID string, a contract.NodeAssignment) (taskworkspace.Remote, func(), error)
	API(taskID string, a contract.NodeAssignment) (taskworkspace.PlaneAPI, error)
}

// clientPlane is the production wsPlane over the verified client.
type clientPlane struct{ c *client.Client }

func (p clientPlane) Remote(taskID string, a contract.NodeAssignment) (taskworkspace.Remote, func(), error) {
	g, err := p.c.NodeTaskGit(taskID, a, "")
	if err != nil {
		return nil, nil, err
	}
	return g, g.Close, nil
}

func (p clientPlane) API(taskID string, a contract.NodeAssignment) (taskworkspace.PlaneAPI, error) {
	tp, err := p.c.TaskPublications(taskID, a)
	if err != nil {
		return nil, err
	}
	return planeAPI{c: p.c, tp: tp, taskID: taskID, a: a}, nil
}

// planeAPI adapts the publication endpoints and receive sessions.
type planeAPI struct {
	c      *client.Client
	tp     *client.TaskPublications
	taskID string
	a      contract.NodeAssignment
}

func (p planeAPI) Begin(ctx context.Context, req contract.PublicationBeginRequest) (contract.PublicationBeginResponse, error) {
	return p.tp.Begin(ctx, req)
}

func (p planeAPI) Finish(ctx context.Context, pubID, code string) (contract.PublicationStatus, error) {
	return p.tp.Finish(ctx, pubID, code)
}

func (p planeAPI) Observe(ctx context.Context, pubID string) (contract.PublicationStatus, error) {
	return p.tp.Observe(ctx, pubID)
}

func (p planeAPI) Pusher(pubID string) (taskworkspace.Pusher, error) {
	g, err := p.c.NodeTaskPusher(p.taskID, p.a, pubID)
	if err != nil {
		return nil, err
	}
	return g, nil
}

// assignment is w's node assignment (its transfer and publication headers).
func (s *taskSupervisor) assignment(w *taskWorker) contract.NodeAssignment {
	return contract.NodeAssignment{NodeID: s.nodeID, Execution: w.start.Execution, StartDigest: w.digest, Instance: w.start.Workspace.Instance}
}

// workerContext is a worker operation's context: it ends at the clock's
// deadline, at Run shutdown, at a stop request and (onControl) at a latched
// plane control. armed is emitted once its timer is registered. The cancel
// function joins the watcher.
func (s *taskSupervisor) workerContext(w *taskWorker, deadline time.Time, onControl bool, armed eventKind) (context.Context, func()) {
	ctx, cancel := context.WithCancel(context.Background())
	timer, stop := s.d.clock.NewTimerAt(deadline)
	s.event(armed, w)
	var ctl chan struct{}
	if onControl {
		ctl = w.ctlCh
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		select {
		case <-timer:
		case <-s.stopAll:
		case <-w.stopCh:
		case <-ctl:
		case <-ctx.Done():
		}
		cancel()
	}()
	return ctx, func() {
		cancel()
		<-done
		stop()
	}
}

// makeWork creates the journal-owned work directory tasks/<task_id>/work
// (0700; the child's cwd) and returns its canonical physical path.
func (s *taskSupervisor) makeWork(w *taskWorker) (string, error) {
	if err := s.d.hook("mkdir", filepath.Join(taskDirRel(w.id()), workName)); err != nil {
		return "", err
	}
	return s.plat.work(s.jr.l.path(taskDirRel(w.id())))
}

// removeWork deletes a task's work directory without following links.
func removeWork(work string) error {
	if work == "" {
		return nil
	}
	return workspacetransfer.RemoveTree(work)
}

// releaseWork removes w's work directory and then frees its slot, after a
// confirmed group cleanup. A failed removal keeps the slot held (cleanup
// pending): the janitor's journal removal, retried and blocking the node
// while it fails, deletes the whole task directory and only then releases
// the slot.
func (s *taskSupervisor) releaseWork(w *taskWorker, work string) {
	err := s.d.hook("remove", filepath.Join(taskDirRel(w.id()), workName))
	if err == nil {
		err = removeWork(work)
	}
	if err != nil {
		s.logger.Warn("task work directory cleanup failed; the slot is held until the janitor removes it", "task_id", w.id())
		w.mu.Lock()
		w.workPending = true
		w.mu.Unlock()
		s.event(evWorkPending, w)
		s.kickJanitor()
		return
	}
	s.release(w.key)
}

// prepareWorkspace runs once the prepared journal and the work directory
// are durable: it lets the session answer preparing, then prepares the
// checkout under the preparation context. It returns the owned runtime
// directory's absolute path and true, or false once it has resolved the
// start itself (a reported refusal, a cancellation before release, or Run
// shutdown).
func (s *taskSupervisor) prepareWorkspace(w *taskWorker, work string) (string, bool) {
	w.mu.Lock()
	if w.expired || w.phase == phaseRefused {
		w.mu.Unlock()
		s.refuseStart(w, contract.ReasonPreparationTimeout, nil, work, true)
		return "", false
	}
	w.prepReply = true
	w.deadline = s.d.clock.Now().Add(contract.WorkspacePrepareTimeout)
	deadline := w.deadline
	w.mu.Unlock()
	s.signal()
	ctx, cancel := s.workerContext(w, deadline, true, evPrepArmed)
	defer cancel()
	var p *taskworkspace.Prepared
	remote, closeRemote, err := s.ws.Remote(w.id(), s.assignment(w))
	if err == nil {
		p, err = taskworkspace.Prepare(ctx, taskworkspace.PrepareInput{GOOS: s.plat.goos, TaskDir: s.jr.l.path(taskDirRel(w.id())),
			Binding: *w.start.Workspace, Remote: remote, Cache: s.cache, RuntimeDir: true})
		closeRemote()
	}
	if err == nil {
		w.mu.Lock()
		w.pathMap, w.runtimeDir = p.Map, p.RuntimeDir
		w.mu.Unlock()
		// Counters only: no path, name or content.
		s.logger.Info("task workspace prepared", "task_id", w.id(), "cache_hit", p.Stats.Hit, "saved_bytes", p.Stats.Saved,
			"fetched_bytes", p.Stats.Fetched, "copied_bytes", p.Stats.Copied, "refetched", p.Stats.Retried, "cache_purged", p.Stats.Purged, "cache_bypassed", p.Stats.Bypassed,
			"files", p.Files, "bytes", p.Bytes)
		s.event(evWorkspacePrepared, w)
		return filepath.Join(work, p.RuntimeDir), true
	}
	switch {
	case w.latched() != nil:
		// A cancellation before release: nothing ran.
		s.cancelBeforeStart(w, work, nil)
	case s.isClosing() || closedCh(w.stopCh):
		// Run shutdown: the prepared journal stays (the next Run resolves
		// it lost); nothing was released.
		s.event(evTaskExited, w)
	default:
		reason := taskworkspace.PrepareReason(err)
		if !s.d.clock.Now().Before(deadline) {
			reason = contract.ReasonWorkspacePrepareTimeout
		}
		s.logger.Warn("task workspace preparation failed", "task_id", w.id(), "reason", reason)
		s.refusePrepared(w, reason, work)
	}
	return "", false
}

// refuseOwned refuses a start whose journal and work directory exist: a
// workspace start (already answered preparing) reports the refusal as its
// task_prepared first; any other start is refused at once.
func (s *taskSupervisor) refuseOwned(w *taskWorker, reason, work string) {
	if w.start.Workspace != nil {
		if reason == contract.ReasonStartFailed {
			reason = contract.ReasonWorkspaceCheckoutFailed
		}
		s.refusePrepared(w, reason, work)
		return
	}
	s.refuseStart(w, reason, nil, work, true)
}

// lostBeforeLaunch resolves a workspace execution the plane authorized
// whose guardian proved no adapter started: the outcome is lost (no
// trustworthy outcome can be claimed for a running task), with its group
// cleanup's outcome governing the slot as for a cancellation before start.
func (s *taskSupervisor) lostBeforeLaunch(w *taskWorker, work string, cleanupErr error) {
	w.mu.Lock()
	w.cleaning = false
	if cleanupErr != nil {
		w.cleanupFailed = true
	} else {
		w.cleanupOK = true
	}
	w.mu.Unlock()
	s.refreshBlocker(w)
	res := contract.TaskResultBody{TaskID: w.id(), Execution: w.start.Execution, Outcome: contract.OutcomeLost}
	if c := w.latched(); c != nil {
		id := c.ID
		res.StopID = &id
	}
	s.logger.Warn("task execution lost", "task_id", w.id(), "reason", lostUnobserved)
	if cleanupErr != nil {
		s.mu.Lock()
		s.blocked[w.key] = true
		s.mu.Unlock()
		s.freezeThen(w, workspaceLost(w.start.Workspace, res), nil)
	} else {
		s.freezeThen(w, workspaceLost(w.start.Workspace, res), func() { s.releaseWork(w, work) })
	}
	close(w.exitedCh)
	s.event(evTaskExited, w)
	s.signal()
}

// refusePrepared reports a failed preparation (task_prepared ok=false
// with its fixed reason) and, once the plane acknowledged it, refuses the
// start locally: the work directory, trusted objects and journal are
// removed and the slot released (definitely no adapter ran).
func (s *taskSupervisor) refusePrepared(w *taskWorker, reason, work string) {
	e := refuse(reason)
	e.Message = contract.SafeText(e.Message, contract.MaxReasonMessageBytes)
	if _, ok := s.exchangePrepared(w, contract.TaskPreparedBody{TaskID: w.id(), Execution: w.start.Execution, StartDigest: w.digest, Error: e}); !ok {
		return
	}
	s.refuseStart(w, reason, nil, work, true)
}

// exchangePrepared queues w's task_prepared for the session (resent on
// every attachment until acknowledged) and waits for the plane's release
// decision; false when Run shut down or a stop was requested first.
func (s *taskSupervisor) exchangePrepared(w *taskWorker, b contract.TaskPreparedBody) (bool, bool) {
	w.mu.Lock()
	w.prepOut, w.prepSent = &b, false
	w.mu.Unlock()
	s.signal()
	select {
	case r := <-w.release:
		return r, true
	case <-s.stopAll:
	case <-w.stopCh:
	}
	w.mu.Lock()
	w.prepOut = nil
	w.mu.Unlock()
	return false, false
}

// finalizeWorkspace produces a workspace execution's final outcome after
// its group is gone: a lost outcome or unconfirmed cleanup is
// not_applicable, a cancellation before any adapter not_started; otherwise
// the result snapshot (bounded by the finalization context), the sealed
// checkpoint and the publication. It returns nil when Run shut down before
// the publication ended (its checkpoint is resumed by the next Run).
func (s *taskSupervisor) finalizeWorkspace(w *taskWorker, res contract.TaskResultBody, cleanupErr error) *contract.TaskResultBody {
	b := *w.start.Workspace
	w.mu.Lock()
	started, pathMap, runtime := w.started, w.pathMap, w.runtimeDir
	w.mu.Unlock()
	switch {
	case cleanupErr != nil, res.Outcome == contract.OutcomeLost:
		dto := contract.NewNotApplicable(b)
		res.Workspace = &dto
		r := res.Sealed()
		return &r
	case started == nil:
		dto := contract.NewNotStarted(b)
		res.Workspace = &dto
		r := res.Sealed()
		return &r
	}
	deadline := s.d.clock.Now().Add(contract.WorkspaceFinalizeTimeout)
	fctx, cancel := s.workerContext(w, deadline, false, evFinalizeArmed)
	defer cancel()
	cand := res.Sealed()
	tree, _, err := taskworkspace.Snapshot(fctx, taskworkspace.SnapshotInput{GOOS: s.plat.goos, TaskDir: s.jr.l.path(taskDirRel(w.id())), Map: pathMap, RuntimeDir: runtime})
	if err != nil {
		if s.isClosing() {
			return nil
		}
		code := taskworkspace.PublicationCode(err)
		if !s.d.clock.Now().Before(deadline) {
			code = contract.PubErrPublicationTimeout
		}
		s.logger.Warn("task workspace snapshot failed", "task_id", w.id(), "code", code)
		dto := contract.NewPublicationFailed(b, code)
		res.Workspace = &dto
		r := res.Sealed()
		return &r
	}
	cp := contract.PublicationCheckpoint{TaskID: w.id(), Execution: w.start.Execution, Phase: contract.CheckpointSealed, Candidate: cand, Tree: tree.String()}
	if !s.saveCheckpoint(w, cp) {
		return nil
	}
	return s.publish(w, cp, fctx, false)
}

// publish runs (or resumes) a checkpointed publication; nil when Run shut
// down first. A recovered publication (a previous Run's checkpoint) only
// observes an authorized intent: it never pushes after a shutdown.
func (s *taskSupervisor) publish(w *taskWorker, cp contract.PublicationCheckpoint, begin context.Context, recovered bool) *contract.TaskResultBody {
	ctx, cancel := context.WithCancel(context.Background())
	stopped := context.AfterFunc(s.base, cancel)
	defer func() { stopped(); cancel() }()
	api, err := s.ws.API(w.id(), s.assignment(w))
	if err != nil {
		return s.publicationFailed(w, cp, contract.PubErrPublicationTransportFail)
	}
	pub, err := taskworkspace.Publish(ctx, taskworkspace.PublishInput{TaskID: w.id(), RoleID: w.start.Role.ID, Model: w.start.Effective.Model,
		TaskDir: s.jr.l.path(taskDirRel(w.id())), Binding: *w.start.Workspace, Checkpoint: cp, API: api, Clock: s.d.clock, Begin: begin, Recovered: recovered,
		// A failing checkpoint write blocks the node and is retried (never
		// skipped): progress waits for its durable predecessor.
		Save: func(c contract.PublicationCheckpoint) error {
			if !s.saveCheckpoint(w, c) {
				return errRunClosing
			}
			return nil
		},
		Hook: func(stage string) { s.d.emit(event{kind: evPublication, id: w.id(), action: stage}) }})
	if err != nil {
		return nil
	}
	if pub.Result != nil {
		return pub.Result
	}
	return s.publicationFailed(w, cp, pub.Failed)
}

// publicationFailed is the ordinary-path result of a publication that
// never got a durable intent: the candidate with the failed DTO.
func (s *taskSupervisor) publicationFailed(w *taskWorker, cp contract.PublicationCheckpoint, code string) *contract.TaskResultBody {
	r := cp.Candidate
	dto := contract.NewPublicationFailed(*w.start.Workspace, code)
	r.Workspace = &dto
	r = r.Sealed()
	return &r
}

// saveCheckpoint writes publication.json, retrying every journalRetry
// while it fails (the node stays nonaccepting meanwhile); false at Run
// shutdown.
func (s *taskSupervisor) saveCheckpoint(w *taskWorker, cp contract.PublicationCheckpoint) bool {
	for {
		err := s.jr.writeCheckpoint(cp)
		if err == nil {
			w.mu.Lock()
			failing := w.journalFailing
			w.journalFailing = false
			w.mu.Unlock()
			if failing {
				s.refreshBlocker(w)
			}
			return true
		}
		s.logger.Error("task publication checkpoint failed; retrying", "task_id", w.id(), "error", err)
		w.mu.Lock()
		w.journalFailing = true
		w.mu.Unlock()
		s.refreshBlocker(w)
		if !s.pause(journalRetry) {
			return false
		}
	}
}

// resumeWorkspace resumes a previous Run's checkpointed publication after
// its group's cleanup was confirmed: a settled one is its outbox at once,
// otherwise the publication continues in recovery mode (an authorized
// intent, pushed or not, is only observed: never a fresh push). The slot
// it holds is released after the outbox is durable and the work deleted.
func (s *taskSupervisor) resumeWorkspace(w *taskWorker, cp contract.PublicationCheckpoint) {
	defer s.wg.Done()
	select {
	case <-w.cleanupCh:
	case <-s.stopAll:
		return
	}
	var r *contract.TaskResultBody
	if cp.Phase == contract.CheckpointSettled {
		r = cp.Result
	} else {
		r = s.publish(w, cp, nil, true)
	}
	if r == nil {
		return
	}
	s.freezeThen(w, *r, func() { s.releaseWork(w, w.work) })
	s.kickJanitor()
	s.signal()
}

// errRunClosing ends a publication whose checkpoint could not be written
// before Run shut down (the next Run resumes from the last durable one).
var errRunClosing = errors.New("the sidecar is shutting down")

// workspaceLost is a workspace execution's lost result DTO (nothing is
// claimed).
func workspaceLost(b *contract.WorkspaceBinding, res contract.TaskResultBody) contract.TaskResultBody {
	if b != nil {
		dto := contract.NewNotApplicable(*b)
		res.Workspace = &dto
	}
	return res.Sealed()
}

// workspaceCancelled is a cancellation before release: not_started (lost
// executions not_applicable).
func workspaceCancelled(b *contract.WorkspaceBinding, res contract.TaskResultBody) contract.TaskResultBody {
	if b == nil {
		return res.Sealed()
	}
	dto := contract.NewNotStarted(*b)
	if res.Outcome == contract.OutcomeLost {
		dto = contract.NewNotApplicable(*b)
	}
	res.Workspace = &dto
	return res.Sealed()
}

// work creates the journal-owned work directory in taskDir: 0700, its
// absolute cleaned path, physical on darwin (whose temp roots live under a
// /var symlink: the child's PWD must name what the kernel reports).
func (p taskPlatform) work(taskDir string) (string, error) {
	dir := filepath.Join(taskDir, workName)
	if err := os.Mkdir(dir, 0o700); err != nil {
		return "", err
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		return "", err
	}
	abs, err := filepath.Abs(dir)
	if err != nil {
		return "", err
	}
	if p.physicalScratch {
		if abs, err = filepath.EvalSymlinks(abs); err != nil {
			return "", err
		}
	}
	return filepath.Clean(abs), nil
}
