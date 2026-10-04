// Package taskpublication is the plane-owned publication transaction of a
// workspace task's result (iteration 10b): the arbitration of the worker's
// sealed outcome candidate into a durable intent (Begin), the guarded
// create-once receive of the task ref and its settlement, definitive
// failures (Finish), read-only observation, intent expiry and the startup
// replay of authorized intents between the task and workspace stores.
//
// Task mutations are serialized by the plane's existing per-task writer
// behind the injected Tasks interface; workspace provenance and fences
// come from the injected Workspaces interface. Lock order: a task's
// publication operation mutex, then the per-task writer operation, then a
// workspace transaction; the workspace lock is never held while waiting
// for the task writer. No registry lock spans a scan or a pack stream.
package taskpublication

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"io"
	"log/slog"
	"sync"
	"time"

	"github.com/go-git/go-git/v5/plumbing"

	"github.com/wedevwork/callsheet/internal/contract"
	"github.com/wedevwork/callsheet/internal/workspace"
	"github.com/wedevwork/callsheet/internal/workspacetransfer"
)

// Task is one task's publication-relevant state, observed under the
// plane's task lock.
type Task struct {
	TaskID      string
	Node        string
	Execution   contract.ExecutionToken
	StartDigest string
	RoleID      string
	Model       string
	Binding     contract.WorkspaceBinding
	State       string
	// Live: the task may transfer (nonterminal, its assignment current).
	Live bool
	// Committed: its terminal record is durable.
	Committed   bool
	Publication *contract.TaskPublication
	// Result is the settled canonical result (terminal workspace task).
	Result *contract.TaskResultBody
}

// Settlement is a publication's terminal decision.
type Settlement struct {
	PubID     string
	Status    string
	Error     string
	Workspace contract.TaskWorkspaceResult
}

// Tasks is the plane's task authority.
type Tasks interface {
	// Check returns the task after checking a claimed assignment: the
	// claimed node is its role snapshot's, the execution its stable token
	// on the node's current authorized attachment, the start digest its
	// recorded one and the instance its binding's (a fixed-reason conflict
	// otherwise; an unknown task is not found).
	Check(taskID string, a contract.NodeAssignment) (Task, error)
	// AwaitStart waits, bounded by ctx, while the task's start reply is
	// in flight on the live attachment its start was written to (a node
	// transfer can overtake that reply on another connection); it returns
	// once the reply is applied or no longer awaited and never authorizes
	// anything itself.
	AwaitStart(ctx context.Context, taskID string, a contract.NodeAssignment) error
	// Authorize arbitrates candidate against the task's durable decisions
	// under its lock (the canonical terminal state), lets build construct
	// the intent, selects it (nothing else can then decide the task) and
	// returns once the intent is durable. A duplicate with a byte-identical
	// candidate and tree returns the original intent.
	Authorize(ctx context.Context, taskID string, a contract.NodeAssignment, cand contract.TaskResultBody, tree string,
		build func(t Task, state string) (contract.TaskPublication, error)) (contract.TaskPublication, error)
	// Settle latches the terminal record of publication s.PubID and
	// returns its sealed canonical result once it is durable.
	Settle(ctx context.Context, taskID string, s Settlement) (contract.TaskResultBody, error)
	// AwaitDurable waits until the task's terminal record is durable.
	AwaitDurable(ctx context.Context, taskID string) error
	// Get returns the task without an assignment check (expiry, replay).
	Get(taskID string) (Task, error)
	// Authorized lists every task with an authorized intent.
	Authorized() []Task
}

// Workspaces is the hub's provenance, metadata and fence access.
type Workspaces interface {
	TaskRef(ctx context.Context, name, instance, taskID string) (workspace.TaskRefState, error)
	TaskDiff(ctx context.Context, name, instance string, base, commit plumbing.Hash) (workspace.TaskMetadata, error)
	FencePublication(name, taskID, pubID string) func()
	ReleasePublication(name, taskID, pubID string)
}

// Clock is the service's time source.
type Clock interface {
	Now() time.Time
	NewTimerAt(at time.Time) (<-chan time.Time, func() bool)
}

// Service is one plane run's publication state machine.
type Service struct {
	tasks  Tasks
	ws     Workspaces
	clock  Clock
	logger *slog.Logger
	rand   io.Reader
	// hook observes named stages (tests: barriers and arming events).
	hook func(stage, taskID string)

	mu     sync.Mutex
	ops    map[string]*opLock
	timers map[string]func() bool
	closed bool
	stop   chan struct{}
	wg     sync.WaitGroup
	// base ends at Close (expiry and background releases).
	base   context.Context
	cancel context.CancelFunc
}

// Options configures New.
type Options struct {
	Tasks      Tasks
	Workspaces Workspaces
	Clock      Clock
	Logger     *slog.Logger
	Rand       io.Reader
	Hook       func(stage, taskID string)
}

// New returns a service; Replay must run before it serves.
func New(o Options) *Service {
	s := &Service{tasks: o.Tasks, ws: o.Workspaces, clock: o.Clock, logger: o.Logger, rand: o.Rand, hook: o.Hook,
		ops: map[string]*opLock{}, timers: map[string]func() bool{}, stop: make(chan struct{})}
	if s.logger == nil {
		s.logger = slog.New(slog.DiscardHandler)
	}
	if s.rand == nil {
		s.rand = rand.Reader
	}
	s.base, s.cancel = context.WithCancel(context.Background())
	return s
}

func (s *Service) at(stage, id string) {
	if s.hook != nil {
		s.hook(stage, id)
	}
}

// opLock is one task's publication operation mutex (context-aware),
// reference counted in the service's map outside the task lock.
type opLock struct {
	ch   chan struct{}
	refs int
}

// lock acquires taskID's operation mutex unless ctx ends first.
func (s *Service) lock(ctx context.Context, taskID string) (func(), error) {
	s.mu.Lock()
	l := s.ops[taskID]
	if l == nil {
		l = &opLock{ch: make(chan struct{}, 1)}
		s.ops[taskID] = l
	}
	l.refs++
	s.mu.Unlock()
	drop := func() {
		s.mu.Lock()
		if l.refs--; l.refs == 0 {
			delete(s.ops, taskID)
		}
		s.mu.Unlock()
	}
	if err := ctx.Err(); err != nil {
		drop()
		return nil, err
	}
	select {
	case l.ch <- struct{}{}:
	case <-ctx.Done():
		drop()
		return nil, ctx.Err()
	}
	return func() { <-l.ch; drop() }, nil
}

// Close stops every expiry timer and joins the service's goroutines.
func (s *Service) Close() {
	s.mu.Lock()
	if !s.closed {
		s.closed = true
		close(s.stop)
		s.cancel()
		for _, stop := range s.timers {
			stop()
		}
	}
	s.mu.Unlock()
	s.wg.Wait()
}

func (s *Service) newID() (string, error) {
	var b [16]byte
	if _, err := io.ReadFull(s.rand, b[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(b[:]), nil
}

// ExpectedCommit is the deterministic result commit a publication must
// receive: the intent's tree, the binding's base as the only parent, the
// fixed identity and the fixed message of the task, role, effective model
// and canonical state.
func ExpectedCommit(taskID, roleID, model, state string, b contract.WorkspaceBinding, tree string) (plumbing.Hash, error) {
	for _, v := range []string{taskID, roleID, model, state} {
		if !contract.ValidMessageValue(v) {
			return plumbing.ZeroHash, contract.TaskError(contract.CodeInvalidArgument, "", contract.PubErrSnapshotFailed, "a result commit message value is not printable ASCII")
		}
	}
	var base plumbing.Hash
	if b.BaseCommit != nil {
		base = plumbing.NewHash(*b.BaseCommit)
	}
	return workspacetransfer.CommitHash(workspacetransfer.TaskResultCommit(plumbing.NewHash(tree), base, contract.TaskResultMessage(taskID, roleID, model, state)))
}

func closed(msg string) error {
	return contract.TaskError(contract.CodeConflict, "", contract.ReasonTaskPublicationClosed, "%s", msg)
}

// Begin arbitrates the worker's sealed candidate (no workspace extension)
// and its result tree into a durable intent: ID a random 128-bit token,
// expiry five minutes after durable authorization. The intent's expiry is
// armed before the answer.
func (s *Service) Begin(ctx context.Context, taskID string, a contract.NodeAssignment, req contract.PublicationBeginRequest) (contract.PublicationBeginResponse, error) {
	var resp contract.PublicationBeginResponse
	if req.Candidate.TaskID != taskID || req.Candidate.Execution != a.Execution {
		return resp, contract.TaskError(contract.CodeInvalidArgument, "", "", "the candidate names another task or execution")
	}
	unlock, err := s.lock(ctx, taskID)
	if err != nil {
		return resp, err
	}
	defer unlock()
	s.at("begin-locked", taskID)
	in, err := s.tasks.Authorize(ctx, taskID, a, req.Candidate, req.Tree, func(t Task, state string) (contract.TaskPublication, error) {
		id, err := s.newID()
		if err != nil {
			return contract.TaskPublication{}, err
		}
		commit, err := ExpectedCommit(taskID, t.RoleID, t.Model, state, t.Binding, req.Tree)
		if err != nil {
			return contract.TaskPublication{}, err
		}
		return contract.TaskPublication{ID: id, State: state, Tree: req.Tree, Commit: commit.String(), Instance: t.Binding.Instance,
			Deadline: s.clock.Now().UTC().Add(contract.PublicationExpiry), Phase: contract.PubPhaseAuthorized, Candidate: req.Candidate}, nil
	})
	if err != nil {
		return resp, err
	}
	if in.Phase == contract.PubPhaseAuthorized {
		s.arm(taskID, in.ID, in.Deadline)
	}
	return contract.PublicationBeginResponse{PublicationID: in.ID, State: in.State, ExpiresAt: contract.FormatTime(in.Deadline)}, nil
}

// arm starts (once) the expiry of intent pubID at deadline.
func (s *Service) arm(taskID, pubID string, deadline time.Time) {
	key := taskID + "/" + pubID
	s.mu.Lock()
	if s.closed || s.timers[key] != nil {
		s.mu.Unlock()
		return
	}
	c, stop := s.clock.NewTimerAt(deadline)
	s.timers[key] = stop
	s.wg.Add(1)
	s.mu.Unlock()
	s.at("expiry-armed", taskID)
	go func() {
		defer s.wg.Done()
		select {
		case <-c:
		case <-s.stop:
			return
		}
		s.mu.Lock()
		delete(s.timers, key)
		s.mu.Unlock()
		s.expire(taskID, pubID)
	}()
}

// expire settles a lapsed intent once no receive owns its operation: the
// receive's own deadline ended it (joined through the mutex), then the
// workspace provenance decides published, else failed/publication_timeout.
func (s *Service) expire(taskID, pubID string) {
	ctx := s.base
	unlock, err := s.lock(ctx, taskID)
	if err != nil {
		return
	}
	defer unlock()
	s.at("expiry-locked", taskID)
	if _, err := s.settleByProvenance(ctx, taskID, pubID, contract.PubErrPublicationTimeout); err != nil {
		s.logger.Error("task publication expiry could not settle; it stays completion-pending", "task_id", taskID, "error", err)
		s.at("expiry-unsettled", taskID)
	}
}

// current returns the task's authorized intent pubID (nil when settled:
// its settled result is then returned).
func (s *Service) current(taskID, pubID string, t Task) (*contract.TaskPublication, error) {
	p := t.Publication
	switch {
	case p == nil || p.ID != pubID:
		return nil, contract.TaskError(contract.CodeNotFound, "", "", "no such publication")
	case p.Phase == contract.PubPhaseSettled:
		return nil, nil
	}
	return p, nil
}

// settleByProvenance inspects the workspace before deciding: an own
// committed ref with this intent's provenance settles published (its
// metadata recomputed from the hub); a ref without matching provenance is
// corruption (an error: never guessed); absence settles failed with code.
// The caller holds the operation mutex.
func (s *Service) settleByProvenance(ctx context.Context, taskID, pubID, code string) (*contract.TaskResultBody, error) {
	t, err := s.lookup(taskID)
	if err != nil {
		return nil, err
	}
	p, err := s.current(taskID, pubID, t)
	if err != nil || p == nil {
		return t.Result, err
	}
	b := t.Binding
	st, err := s.ws.TaskRef(ctx, b.Name, b.Instance, taskID)
	if err != nil {
		return nil, err
	}
	if st.Exists {
		if st.PublicationID != p.ID || st.Commit != p.Commit {
			return nil, errors.New("the task ref exists without this publication's provenance")
		}
		return s.published(ctx, t, p, st.PublishedAt)
	}
	dto := contract.NewPublicationFailed(b, code)
	r, err := s.tasks.Settle(ctx, taskID, Settlement{PubID: p.ID, Status: contract.PublicationFailed, Error: code, Workspace: dto})
	if err != nil {
		return nil, err
	}
	return &r, nil
}

// published settles a committed own ref: its metadata is recomputed from
// the hub and the terminal record persisted; the fence is released once
// durable.
func (s *Service) published(ctx context.Context, t Task, p *contract.TaskPublication, at time.Time) (*contract.TaskResultBody, error) {
	b := t.Binding
	var base plumbing.Hash
	if b.BaseCommit != nil {
		base = plumbing.NewHash(*b.BaseCommit)
	}
	md, err := s.ws.TaskDiff(ctx, b.Name, b.Instance, base, plumbing.NewHash(p.Commit))
	if err != nil {
		return nil, err
	}
	dto, err := PublishedDTO(b, t.TaskID, p.Commit, md)
	if err != nil {
		return nil, err
	}
	_ = at
	r, err := s.tasks.Settle(ctx, t.TaskID, Settlement{PubID: p.ID, Status: contract.PublicationPublished, Workspace: dto})
	if err != nil {
		return nil, err
	}
	s.ws.ReleasePublication(b.Name, t.TaskID, p.ID)
	return &r, nil
}

// PublishedDTO is a published result's DTO with exact totals and bounded
// sorted rows.
func PublishedDTO(b contract.WorkspaceBinding, taskID, commit string, md workspace.TaskMetadata) (contract.TaskWorkspaceResult, error) {
	ref := contract.TaskRefPrefix + taskID
	ds := md.Diffstat
	r := contract.TaskWorkspaceResult{Name: b.Name, Instance: b.Instance, BaseCommit: b.BaseCommit, Publication: contract.PublicationPublished,
		Commit: &commit, Ref: &ref, Diffstat: &ds}
	if err := contract.BoundChanges(&r, md.Rows); err != nil {
		return r, err
	}
	return r, r.Validate()
}

func (s *Service) lookup(taskID string) (Task, error) { return s.tasks.Get(taskID) }

// Finish reports a definitive push refusal or local failure after Begin:
// under the operation mutex the workspace provenance is inspected first
// (an already committed own ref settles published), otherwise the
// publication settles failed with the fixed code. It answers the
// settlement envelope.
func (s *Service) Finish(ctx context.Context, taskID string, a contract.NodeAssignment, pubID, code string) (contract.PublicationStatus, error) {
	t, err := s.tasks.Check(taskID, a)
	if err != nil {
		return contract.PublicationStatus{}, err
	}
	if _, err := s.current(taskID, pubID, t); err != nil {
		return contract.PublicationStatus{}, err
	}
	unlock, err := s.lock(ctx, taskID)
	if err != nil {
		return contract.PublicationStatus{}, err
	}
	defer unlock()
	s.at("finish-locked", taskID)
	if _, err := s.settleByProvenance(ctx, taskID, pubID, code); err != nil {
		return contract.PublicationStatus{}, err
	}
	return s.Observe(taskID, a, pubID)
}

// Observe is the read-only settlement envelope of publication pubID (""
// is the lookup form with the nullable publication ID and phase none).
func (s *Service) Observe(taskID string, a contract.NodeAssignment, pubID string) (contract.PublicationStatus, error) {
	t, err := s.tasks.Check(taskID, a)
	if err != nil {
		return contract.PublicationStatus{}, err
	}
	st := contract.PublicationStatus{WithID: pubID == "", Phase: contract.PubPhaseNone, TaskState: t.State, Committed: t.Committed}
	p := t.Publication
	if pubID != "" && (p == nil || p.ID != pubID) {
		return st, contract.TaskError(contract.CodeNotFound, "", "", "no such publication")
	}
	if p == nil {
		return st, nil
	}
	id := p.ID
	st.PublicationID, st.Phase = &id, p.Phase
	if pubID != "" {
		st.PublicationID = nil
	}
	if p.Phase == contract.PubPhaseSettled {
		st.Result = t.Result
		if st.Result == nil || !t.Committed {
			// Settled but not yet durable: still authorized from outside.
			st.Phase, st.Result = contract.PubPhaseAuthorized, nil
		}
	}
	return st, nil
}

// ---- Node route guard ----

// Upload implements workspace.TaskGuard: a live task's bound access. A
// fetch that overtook its own start reply is judged once that reply is
// applied.
func (s *Service) Upload(ctx context.Context, taskID string, a contract.NodeAssignment) (workspace.TaskAccess, error) {
	if err := s.tasks.AwaitStart(ctx, taskID, a); err != nil {
		return workspace.TaskAccess{}, err
	}
	t, err := s.tasks.Check(taskID, a)
	if err != nil {
		return workspace.TaskAccess{}, err
	}
	if !t.Live {
		return workspace.TaskAccess{}, closed("the task is no longer transferring")
	}
	return access(t), nil
}

func access(t Task) workspace.TaskAccess {
	acc := workspace.TaskAccess{Name: t.Binding.Name, Instance: t.Binding.Instance, TaskRef: contract.TaskRefPrefix + t.TaskID}
	if t.Binding.BaseCommit != nil {
		acc.Base = plumbing.NewHash(*t.Binding.BaseCommit)
	}
	return acc
}

// Receive implements workspace.TaskGuard: the publication's operation
// mutex is taken and held until Done; the intent must be open (authorized,
// not expired); Published settles the terminal record before the success
// report.
func (s *Service) Receive(ctx context.Context, taskID string, a contract.NodeAssignment, pubID string) (*workspace.TaskReceive, error) {
	t, err := s.tasks.Check(taskID, a)
	if err != nil {
		return nil, err
	}
	if t.Publication == nil || t.Publication.ID != pubID {
		return nil, closed("no open publication intent with this ID")
	}
	unlock, err := s.lock(ctx, taskID)
	if err != nil {
		return nil, err
	}
	ok := false
	defer func() {
		if !ok {
			unlock()
		}
	}()
	s.at("receive-locked", taskID)
	t, err = s.tasks.Check(taskID, a)
	if err != nil {
		return nil, err
	}
	p := t.Publication
	if p == nil || p.ID != pubID || p.Phase != contract.PubPhaseAuthorized || !s.clock.Now().Before(p.Deadline) {
		return nil, closed("the publication intent is closed or expired")
	}
	commit := plumbing.NewHash(p.Commit)
	rv := &workspace.TaskReceive{Access: access(t), Commit: commit, PubID: p.ID, Deadline: p.Deadline, Done: unlock,
		Published: func(ctx context.Context, at time.Time, md workspace.TaskMetadata) error {
			dto, err := PublishedDTO(t.Binding, taskID, p.Commit, md)
			if err != nil {
				return err
			}
			if _, err := s.tasks.Settle(ctx, taskID, Settlement{PubID: p.ID, Status: contract.PublicationPublished, Workspace: dto}); err != nil {
				// The ref is durable, the terminal record not yet: the fence
				// stays until it is (released in the background).
				s.releaseWhenDurable(t.Binding.Name, taskID, p.ID)
				return err
			}
			return nil
		}}
	ok = true
	return rv, nil
}

// releaseWhenDurable releases a kept fence once the task's terminal
// record is durable (or at shutdown, when replay reconstructs it).
func (s *Service) releaseWhenDurable(name, taskID, pubID string) {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return
	}
	s.wg.Add(1)
	s.mu.Unlock()
	go func() {
		defer s.wg.Done()
		if err := s.tasks.AwaitDurable(s.base, taskID); err == nil {
			s.ws.ReleasePublication(name, taskID, pubID)
		}
	}()
}

// ---- Startup replay ----

// Replay settles every authorized intent before the plane accepts streams
// or requests: each intent's fence is reconstructed first (before any
// workspace mutation is permitted); a matching current ref settles
// published, an absent one failed/publication_interrupted (never a fresh
// wait), a ref without matching provenance fails startup for that record.
// The fences are released as each settles durably.
func (s *Service) Replay(ctx context.Context) error {
	pending := s.tasks.Authorized()
	var releases []func()
	for _, t := range pending {
		releases = append(releases, s.ws.FencePublication(t.Binding.Name, t.TaskID, t.Publication.ID))
	}
	var errs []error
	for i, t := range pending {
		if _, err := s.settleByProvenance(ctx, t.TaskID, t.Publication.ID, contract.PubErrPublicationInterrupted); err != nil {
			errs = append(errs, err)
			continue
		}
		releases[i]()
	}
	return errors.Join(errs...)
}

// sameCandidate reports byte-identical canonical candidates.
func sameCandidate(a, b contract.TaskResultBody) bool {
	ea, err1 := contract.Encode(a)
	eb, err2 := contract.Encode(b)
	return err1 == nil && err2 == nil && bytes.Equal(ea, eb)
}

// SameCandidate reports whether two candidates are byte-identical in their
// canonical encoding (a duplicate Begin).
func SameCandidate(a, b contract.TaskResultBody) bool { return sameCandidate(a, b) }
