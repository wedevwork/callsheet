package plane

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/wedevwork/callsheet/internal/contract"
	"github.com/wedevwork/callsheet/internal/taskpublication"
)

// Workspace execution on the plane (iteration 10b): admission's binding,
// the preparation authorization (task_prepared), the task authority the
// publication state machine (internal/taskpublication) drives through the
// per-task writer, the derived workspace result of every terminal
// workspace task, and the publication endpoints. Decisions are made under
// the task lock; durability is awaited outside it through the writer's
// confirmed publications.

// resolveBinding resolves a dispatch's workspace selection, or nil
// without one: not_found for an unknown workspace or a missing branch
// (an unborn main is never an implicit empty base), conflict for a stale
// instance precondition.
func (ts *taskService) resolveBinding(ctx context.Context, req contract.DispatchRequest) (*contract.WorkspaceBinding, error) {
	if req.Workspace == nil {
		return nil, nil
	}
	if ts.ws == nil {
		return nil, contract.TaskError(contract.CodeUnavailable, "workspace", "", "this plane serves no workspaces; nothing was dispatched")
	}
	sel, err := contract.ParseTaskBase(req.Base)
	if err != nil {
		return nil, err
	}
	inst := ""
	if req.WorkspaceInstance != nil {
		inst = *req.WorkspaceInstance
	}
	b, err := ts.ws.ResolveTaskBase(ctx, *req.Workspace, inst, sel)
	if err != nil {
		if ctx.Err() != nil {
			return nil, ts.live(ctx, time.Time{})
		}
		var ce *contract.Error
		if errors.As(err, &ce) {
			return nil, &contract.Error{Code: ce.Code, Message: ce.Message + "; nothing was dispatched", Details: ce.Details}
		}
		return nil, err
	}
	return &b, nil
}

// workspaceTerminal is a terminal workspace record's publication and
// workspace result: a settled publication's own, otherwise derived from
// the decided state (lost and rejected not_applicable, a cancellation
// before release not_started) or the worker's ordinary-path result.
func workspaceTerminal(rec contract.TaskRecord, c *terminalCand) (*contract.TaskPublication, *contract.TaskWorkspaceResult) {
	b := *rec.Workspace
	if c.pub != nil {
		w := *c.workspace
		return c.pub, &w
	}
	var w contract.TaskWorkspaceResult
	switch {
	case rec.State == contract.TaskLost || rec.State == contract.TaskRejected:
		w = contract.NewNotApplicable(b)
	case rec.State == contract.TaskCancelled && (rec.StartedAt == nil || (c.workspace != nil && c.workspace.Publication == contract.PublicationNotStarted)):
		// Cancelled before release: running may already be published
		// (task_prepared), so the worker's not_started is accepted too.
		w = contract.NewNotStarted(b)
	case c.workspace != nil && c.workspace.Publication == contract.PublicationFailed:
		w = *c.workspace
	default:
		// A worker result without its publication (it reported none):
		// nothing is claimed.
		w = contract.NewPublicationFailed(b, contract.PubErrPublicationTransportFail)
	}
	return rec.Publication, &w
}

// prepared applies a task_prepared from attachment gen of node and
// returns its decision: wait (non-nil) until a confirmed publication, or
// release now. A protocol error closes the stream.
func (ts *taskService) prepared(node string, gen uint64, b contract.TaskPreparedBody) (<-chan struct{}, bool, error) {
	if ts == nil {
		return nil, false, errNoTasks
	}
	ts.at("prepared-received", b.TaskID, context.Background())
	ts.mu.Lock()
	defer ts.mu.Unlock()
	e, err := ts.authorizedLocked(node, gen, b.TaskID, b.Execution)
	if err != nil {
		return nil, false, err
	}
	if e.rec.Workspace == nil || e.rec.StartDigest == nil || *e.rec.StartDigest != b.StartDigest {
		return nil, false, errf(contract.CodeInvalidArgument, "task_prepared %s does not name a workspace task's start", b.TaskID)
	}
	return ts.preparedLocked(e, b)
}

// preparedLocked decides a preparation: a durable earlier authorization is
// replayed (never a second launch); a decided task, a failed preparation,
// a selected stop intent or an expired preparation deadline refuses
// (latching rejected, or cancelled before start); otherwise the start is
// authorized through the running publication and released once durable.
func (ts *taskService) preparedLocked(e *taskEntry, b contract.TaskPreparedBody) (<-chan struct{}, bool, error) {
	switch {
	case e.terminal() || e.cand != nil:
		// Decided: no adapter is ever released now.
		return nil, false, nil
	case e.started || e.rec.StartedAt != nil:
		// An earlier authorization: replayed once durable, never twice.
		if e.needRunning || !e.confirmed || e.rec.State == contract.TaskPending {
			return e.waiterLocked(), false, nil
		}
		return nil, true, nil
	case !b.OK:
		reason, _ := b.Error.Details["reason"].(string)
		ts.latchRefusalLocked(e, &contract.TaskReason{Code: reason, Message: sanitizeReason(b.Error.Message)})
		ts.event("prepared-refused " + e.rec.TaskID + " " + reason)
		return nil, false, nil
	case e.intentLocked() != nil:
		ts.latchRefusalLocked(e, &contract.TaskReason{Code: contract.ReasonWorkspacePrepareTimeout, Message: "cancelled before release"})
		return nil, false, nil
	}
	deadline := e.prepDeadline
	if deadline.IsZero() {
		deadline = e.rec.CreatedAt.Add(startControl + contract.WorkspacePrepareTimeout)
	}
	now := ts.clock.Now()
	if !now.Before(deadline) {
		ts.latchRefusalLocked(e, &contract.TaskReason{Code: contract.ReasonWorkspacePrepareTimeout,
			Message: "the worker did not prepare the task's workspace within " + contract.WorkspacePrepareTimeout.String()})
		return nil, false, nil
	}
	e.started, e.needRunning, e.startedAt, e.preparing = true, true, now.UTC(), false
	ts.refreshLocked(e)
	ts.wakeLocked(e)
	ts.event("prepared-authorizing " + e.rec.TaskID)
	return e.waiterLocked(), false, nil
}

// preparedRecheck re-decides a pending task_prepared after a confirmed
// publication (the session's asynchronous acknowledgement).
func (ts *taskService) preparedRecheck(b contract.TaskPreparedBody) (<-chan struct{}, bool) {
	ts.mu.Lock()
	defer ts.mu.Unlock()
	e := ts.tasks[b.TaskID]
	if e == nil {
		return nil, false
	}
	w, release, _ := ts.preparedLocked(e, b)
	return w, release
}

// expirePreparationsLocked refuses every preparation whose deadline passed
// without an authorization (the sweep's duty).
func (ts *taskService) expirePreparationsLocked(now time.Time) {
	for _, e := range ts.tasks {
		if e.preparing && !e.started && e.cand == nil && !e.terminal() && !now.Before(e.prepDeadline) {
			e.preparing = false
			ts.latchRefusalLocked(e, &contract.TaskReason{Code: contract.ReasonWorkspacePrepareTimeout,
				Message: "the worker did not prepare the task's workspace within " + contract.WorkspacePrepareTimeout.String()})
			ts.event("preparation-expired " + e.rec.TaskID)
		}
	}
}

// ---- The publication state machine's task authority ----

// settledResult is a settled publication's canonical sealed result: the
// arbitrated candidate (a cancellation's outcome and stop ID) with its
// workspace DTO.
func settledResult(p contract.TaskPublication, stop *contract.StopIntent, w contract.TaskWorkspaceResult) contract.TaskResultBody {
	r := p.Candidate
	switch {
	case p.State == contract.TaskCancelled && r.Outcome != contract.OutcomeCancelled:
		r.Outcome, r.StopID = contract.OutcomeCancelled, nil
		if stop != nil {
			id := stop.ID
			r.StopID = &id
		}
	case p.State == contract.TaskTimedOut:
		r.Outcome, r.StopID = contract.OutcomeTimedOut, nil
	}
	r.Workspace = &w
	return r.Sealed()
}

// pubTaskLocked is e's publication view.
func (ts *taskService) pubTaskLocked(e *taskEntry) taskpublication.Task {
	t := taskpublication.Task{TaskID: e.rec.TaskID, Node: e.rec.Role.Node, Execution: e.rec.Execution, RoleID: e.rec.Role.ID,
		Model: e.rec.Effective.Model, Binding: *e.rec.Workspace, State: e.rec.State, Publication: e.pubLocked()}
	if e.rec.StartDigest != nil {
		t.StartDigest = *e.rec.StartDigest
	}
	t.Live = !e.terminal() && e.cand == nil && e.rgen != 0 && ts.reg.currentAttachment(e.rec.Role.Node, e.rgen)
	t.Committed = e.terminal() && e.released && e.confirmed
	if p := e.rec.Publication; p != nil && p.Phase == contract.PubPhaseSettled && e.rec.WorkspaceResult != nil {
		r := settledResult(*p, e.rec.StopIntent, *e.rec.WorkspaceResult)
		t.Result = &r
	}
	return t
}

func assignmentConflict() error {
	return contract.TaskError(contract.CodeConflict, "", contract.ReasonTaskAssignmentMismatch, "the claimed node, execution or start does not match the task's assignment")
}

// checkLocked validates a claimed assignment against e.
func (ts *taskService) checkLocked(e *taskEntry, a contract.NodeAssignment) error {
	switch {
	case a.NodeID != e.rec.Role.Node || a.Execution != e.rec.Execution || e.rec.StartDigest == nil || *e.rec.StartDigest != a.StartDigest:
		return assignmentConflict()
	case a.Instance != e.rec.Workspace.Instance:
		return contract.TaskError(contract.CodeConflict, "", contract.ReasonWorkspaceInstanceMismatch, "the claimed instance is not the task's bound instance")
	}
	return nil
}

func (ts *taskService) workspaceTaskLocked(id string) (*taskEntry, error) {
	e := ts.tasks[id]
	if e == nil || e.rec.Workspace == nil {
		return nil, contract.TaskError(contract.CodeNotFound, "", "", "no such workspace task")
	}
	return e, nil
}

// Check implements taskpublication.Tasks.
func (ts *taskService) Check(id string, a contract.NodeAssignment) (taskpublication.Task, error) {
	ts.mu.Lock()
	defer ts.mu.Unlock()
	e, err := ts.workspaceTaskLocked(id)
	if err != nil {
		return taskpublication.Task{}, err
	}
	if err := ts.checkLocked(e, a); err != nil {
		return taskpublication.Task{}, err
	}
	return ts.pubTaskLocked(e), nil
}

// startInFlightLocked reports whether e's start was written to its
// still-live attachment and no reply has been applied yet (no answer,
// no reconciliation, no decision).
func (ts *taskService) startInFlightLocked(e *taskEntry) bool {
	return e.send == sendSending && e.rgen == 0 && !e.reconciling && !e.terminal() && e.cand == nil &&
		ts.reg.currentAttachment(e.rec.Role.Node, e.gen)
}

// AwaitStart implements taskpublication.Tasks. The worker answers a
// workspace start preparing on its stream and then fetches over HTTPS, so
// the fetch can overtake the reply. While the reply is in flight the
// transfer waits (the reply, the stream's end or its expiry, or ctx ends
// the wait) and is then judged by the unchanged live check.
func (ts *taskService) AwaitStart(ctx context.Context, id string, a contract.NodeAssignment) error {
	ts.mu.Lock()
	defer ts.mu.Unlock()
	e, err := ts.workspaceTaskLocked(id)
	if err != nil {
		return err
	}
	if err := ts.checkLocked(e, a); err != nil {
		return err
	}
	if ts.startInFlightLocked(e) {
		ts.event("start-awaited " + id)
	}
	return ts.waitLocked(ctx, e, func() bool { return !ts.startInFlightLocked(e) })
}

// Get implements taskpublication.Tasks.
func (ts *taskService) Get(id string) (taskpublication.Task, error) {
	ts.mu.Lock()
	defer ts.mu.Unlock()
	e, err := ts.workspaceTaskLocked(id)
	if err != nil {
		return taskpublication.Task{}, err
	}
	return ts.pubTaskLocked(e), nil
}

// waitLocked waits (releasing the lock) until done reports true under the
// lock, ctx ends or the plane shuts down; it returns with the lock held.
func (ts *taskService) waitLocked(ctx context.Context, e *taskEntry, done func() bool) error {
	for !done() {
		w := e.waiterLocked()
		ts.mu.Unlock()
		var err error
		select {
		case <-w:
		case <-ctx.Done():
			err = ctx.Err()
		case <-ts.stop:
			err = contract.New(contract.CodeUnavailable, "the plane is shutting down")
		}
		ts.mu.Lock()
		if err != nil {
			e.dropWaiterLocked(w)
			if done() {
				return nil
			}
			return err
		}
	}
	return nil
}

func pubClosed(msg string) error {
	return contract.TaskError(contract.CodeConflict, "", contract.ReasonTaskPublicationClosed, "%s", msg)
}

// Authorize implements taskpublication.Tasks: the candidate is arbitrated
// with the Q26 rules (a durable stop intent makes it cancelled), refused
// when the task is decided, has no released child, reports a lost outcome
// (cleanup unconfirmed) or would not publish (a cancellation before
// release); the intent is selected (nothing else can then decide the
// task) and returned once durable. A duplicate with the identical
// candidate and tree returns the original intent.
func (ts *taskService) Authorize(ctx context.Context, id string, a contract.NodeAssignment, cand contract.TaskResultBody, tree string,
	build func(t taskpublication.Task, state string) (contract.TaskPublication, error)) (contract.TaskPublication, error) {
	ts.mu.Lock()
	defer ts.mu.Unlock()
	e, err := ts.workspaceTaskLocked(id)
	if err != nil {
		return contract.TaskPublication{}, err
	}
	if err := ts.checkLocked(e, a); err != nil {
		return contract.TaskPublication{}, err
	}
	if p := e.pubLocked(); p != nil {
		if p.Tree != tree || !taskpublication.SameCandidate(p.Candidate, cand) {
			return contract.TaskPublication{}, pubClosed("the task has another publication intent")
		}
		in := *p
		err := ts.waitLocked(ctx, e, func() bool { return e.pendingPub == nil && e.rec.Publication != nil && e.confirmed })
		return in, err
	}
	switch {
	case e.terminal() || e.cand != nil:
		return contract.TaskPublication{}, pubClosed("the task is already decided")
	case e.rgen == 0 || !ts.reg.currentAttachment(e.rec.Role.Node, e.rgen):
		return contract.TaskPublication{}, assignmentConflict()
	case !e.started && e.rec.StartedAt == nil, cand.Outcome == contract.OutcomeLost:
		return contract.TaskPublication{}, pubClosed("the task's execution has no publishable outcome")
	case e.ring != nil && cand.OutputBytes < e.ring.next:
		return contract.TaskPublication{}, errf(contract.CodeInvalidArgument, "task %s reports %d output bytes, fewer than the %d received", id, cand.OutputBytes, e.ring.next)
	}
	o, _ := ts.resultOutcome(e, cand, ts.clock.Now())
	if !contract.PublishableState(o.state) || o.started == nil {
		return contract.TaskPublication{}, pubClosed("the task's outcome publishes nothing")
	}
	in, err := build(ts.pubTaskLocked(e), o.state)
	if err != nil {
		return contract.TaskPublication{}, err
	}
	e.pendingPub = &in
	e.reconciling = false
	ts.refreshLocked(e)
	ts.wakeLocked(e)
	ts.event("publication-authorizing " + id)
	err = ts.waitLocked(ctx, e, func() bool {
		return e.pendingPub == nil && e.rec.Publication != nil && e.rec.Publication.ID == in.ID && e.confirmed
	})
	return in, err
}

// Settle implements taskpublication.Tasks: the terminal record of the
// visible authorized intent (its canonical state, the candidate's
// fields, the workspace DTO and settled publication) is latched and the
// sealed canonical result returned once it is durable.
func (ts *taskService) Settle(ctx context.Context, id string, s taskpublication.Settlement) (contract.TaskResultBody, error) {
	ts.mu.Lock()
	defer ts.mu.Unlock()
	e, err := ts.workspaceTaskLocked(id)
	if err != nil {
		return contract.TaskResultBody{}, err
	}
	p := e.rec.Publication
	if p == nil || p.ID != s.PubID {
		return contract.TaskResultBody{}, contract.TaskError(contract.CodeNotFound, "", "", "no such publication")
	}
	durable := func() bool { return e.terminal() && e.released && e.confirmed }
	if p.Phase == contract.PubPhaseSettled || (e.cand != nil && e.cand.pub != nil) {
		if err := ts.waitLocked(ctx, e, durable); err != nil {
			return contract.TaskResultBody{}, err
		}
		return ts.pubTaskLocked(e).Result.Sealed(), nil
	}
	if e.cand != nil {
		return contract.TaskResultBody{}, pubClosed("the task is already decided")
	}
	now := ts.clock.Now()
	o, kind := ts.resultOutcome(e, p.Candidate, now)
	o.state = p.State
	if o.state == contract.TaskCancelled {
		o.reason = nil
	}
	settled := *p
	status := s.Status
	settled.Phase, settled.Status = contract.PubPhaseSettled, &status
	if s.Error != "" {
		code := s.Error
		settled.Error = &code
	}
	w := s.Workspace
	res := settledResult(settled, e.rec.StopIntent, w)
	e.cand = &terminalCand{kind: kind, outcome: o, at: now, digest: res.Digest, pub: &settled, workspace: &w}
	e.reconciling = false
	ts.refreshLocked(e)
	ts.wakeLocked(e)
	ts.event("publication-settling " + id + " " + status)
	if err := ts.waitLocked(ctx, e, durable); err != nil {
		return contract.TaskResultBody{}, err
	}
	return res, nil
}

// AwaitDurable implements taskpublication.Tasks.
func (ts *taskService) AwaitDurable(ctx context.Context, id string) error {
	ts.mu.Lock()
	defer ts.mu.Unlock()
	e := ts.tasks[id]
	if e == nil {
		return taskNotFound(id)
	}
	return ts.waitLocked(ctx, e, func() bool { return e.terminal() && e.released && e.confirmed })
}

// Authorized implements taskpublication.Tasks: every task whose visible
// record holds an authorized intent.
func (ts *taskService) Authorized() []taskpublication.Task {
	ts.mu.Lock()
	defer ts.mu.Unlock()
	var out []taskpublication.Task
	for _, id := range ts.ids {
		e := ts.tasks[id]
		if p := e.rec.Publication; p != nil && p.Phase == contract.PubPhaseAuthorized && e.rec.Workspace != nil {
			out = append(out, ts.pubTaskLocked(e))
		}
	}
	return out
}

// ---- Publication endpoints ----

// handlePublication serves POST/GET /api/v1/tasks/<id>/workspace-publication,
// GET .../<publication_id> and POST .../<publication_id>/finish: the
// assignment headers, then the route's method and body, then the state
// machine.
func (s *nodeService) handlePublication(w http.ResponseWriter, r *http.Request, id, pub string, finish bool) {
	if s.pub == nil {
		writeError(w, contract.New(contract.CodeNotFound, "this plane serves no workspace publications"))
		return
	}
	a, err := contract.ParseNodeAssignment(r.Header.Values)
	if err != nil {
		writeCodeError(w, err)
		return
	}
	if _, err := taskQuery(r); err != nil {
		writeCodeError(w, err)
		return
	}
	want := http.MethodGet
	if finish || pub == "" && r.Method == http.MethodPost {
		want = http.MethodPost
	}
	switch {
	case pub == "" && r.Method != http.MethodGet && r.Method != http.MethodPost:
		methodNotAllowed(w, "GET, POST")
		return
	case pub != "" && r.Method != want:
		methodNotAllowed(w, want)
		return
	case r.Method == http.MethodGet && hasBody(r):
		writeError(w, invalid("GET requests must not carry a body"))
		return
	}
	switch {
	case r.Method == http.MethodGet:
		st, err := s.pub.Observe(id, a, pub)
		reply(w, http.StatusOK, st, err)
	case finish:
		body, ok := readJSON(w, r, contract.MaxPublicationFinish, "a publication finish")
		if !ok {
			return
		}
		req, err := contract.ParsePublicationFinish(body)
		if err != nil {
			writeCodeError(w, err)
			return
		}
		st, err := s.pub.Finish(r.Context(), id, a, pub, req.Error)
		reply(w, http.StatusOK, st, err)
	default:
		body, ok := readJSON(w, r, contract.MaxPublicationBeginBody, "a publication request")
		if !ok {
			return
		}
		req, err := contract.ParsePublicationBegin(body)
		if err != nil {
			writeCodeError(w, err)
			return
		}
		resp, err := s.pub.Begin(r.Context(), id, a, req)
		reply(w, http.StatusOK, resp, err)
	}
}

// sameHashPtr reports equal nullable hashes.
func sameHashPtr(a, b *string) bool { return (a == nil) == (b == nil) && (a == nil || *a == *b) }
