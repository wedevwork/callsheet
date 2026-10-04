package taskworkspace

import (
	"context"
	"errors"
	"io"
	"time"

	"github.com/go-git/go-git/v5/plumbing"

	"github.com/wedevwork/callsheet/internal/contract"
	"github.com/wedevwork/callsheet/internal/workspacetransfer"
)

// The worker side of the publication transaction (iteration 10b). The
// plane arbitrates the sealed outcome candidate at Begin and authorizes a
// durable intent; the worker commits the result with the returned state,
// checkpoints the hash, pushes the create-once task ref through the node
// receive route, and obtains the canonical settlement through the
// read-only observation. A definitive refusal is reported through Finish;
// ambiguity is resolved only by observation (a Begin by its idempotent
// repetition), never by a second mutation. Every step is checkpointed
// before the next begins.

// PlaneAPI is the plane's publication endpoints for one task, with the
// assignment headers.
type PlaneAPI interface {
	Begin(ctx context.Context, req contract.PublicationBeginRequest) (contract.PublicationBeginResponse, error)
	Finish(ctx context.Context, pubID, code string) (contract.PublicationStatus, error)
	// Observe reads a publication (pubID "" is the lookup form).
	Observe(ctx context.Context, pubID string) (contract.PublicationStatus, error)
	// Pusher opens the node receive session of publication pubID.
	Pusher(pubID string) (Pusher, error)
}

// Pusher is the node receive session: its advertisement (the bound base
// and the task's own ref only) and the single create command.
type Pusher interface {
	ReceiveRefs(ctx context.Context) (map[string]plumbing.Hash, error)
	PushTask(ctx context.Context, ref string, commit plumbing.Hash, pack io.Reader) error
	Close()
}

// Saver persists the publication checkpoint durably.
type Saver func(contract.PublicationCheckpoint) error

// Clock is the publication client's time source.
type Clock interface {
	NewTimer(d time.Duration) (<-chan time.Time, func() bool)
}

// PublishInput is one workspace task's publication.
type PublishInput struct {
	TaskID  string
	RoleID  string
	Model   string
	TaskDir string
	Binding contract.WorkspaceBinding
	// Checkpoint is the durable checkpoint (sealed at least).
	Checkpoint contract.PublicationCheckpoint
	API        PlaneAPI
	Save       Saver
	Clock      Clock
	// Begin bounds the arbitration (the finalization context); the
	// authorized push and the observation use ctx.
	Begin context.Context
	// Retry spaces repeated observations and Begins (default 1 s).
	Retry time.Duration
	// Recovered marks a publication resumed by a restarted sidecar: an
	// authorized intent (recovered from its checkpoint, or answered by a
	// recovered Begin) is only observed until the plane settles it (its
	// receive or expiry), never pushed, whatever its push checkpoint says.
	// After an intent a sidecar shutdown retains only durable checkpoints
	// for settlement observation.
	Recovered bool
	// Hook observes stages (tests).
	Hook func(stage string)
}

// Publication is a publication's end: the settled canonical result, or
// (no durable intent was authorized) the fixed failure code the ordinary
// result path reports.
type Publication struct {
	Result *contract.TaskResultBody
	Failed string
}

func (in *PublishInput) stage(s string) {
	if in.Hook != nil {
		in.Hook(s)
	}
}

func (in *PublishInput) pause(ctx context.Context) error {
	d := in.Retry
	if d <= 0 {
		d = time.Second
	}
	c, stop := in.Clock.NewTimer(d)
	defer stop()
	in.stage("retry-armed")
	select {
	case <-c:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// ambiguousErr reports an answer that may have been lost: the plane may
// have acted.
func ambiguousErr(err error) bool {
	switch contract.CodeOf(err) {
	case contract.CodeUnavailable, contract.CodeInternal:
		return true
	}
	return errors.Is(err, context.DeadlineExceeded)
}

// refusalCode is a definitive refusal's publication error code.
func refusalCode(err error) string {
	var ce *contract.Error
	if errors.As(err, &ce) {
		if r, _ := ce.Details["reason"].(string); contract.ValidPublicationError(r) {
			return r
		}
		switch ce.Code {
		case contract.CodeNotFound:
			return contract.PubErrWorkspaceUnavailable
		case contract.CodeConflict:
			return contract.PubErrPublicationClosed
		}
	}
	return contract.PubErrPublicationTransportFail
}

// Publish runs (or resumes) the publication from in.Checkpoint and returns
// its end. ctx is the worker's lifetime (shutdown cancels it); an error
// means the outcome is still unresolved (the checkpoint is kept and a
// later Run resumes or observes it). A pushed intent, and any intent of a
// Recovered publication, is only observed.
func Publish(ctx context.Context, in PublishInput) (Publication, error) {
	cp := in.Checkpoint
	for {
		switch cp.Phase {
		case contract.CheckpointSettled:
			return Publication{Result: cp.Result}, nil
		case contract.CheckpointSealed:
			next, pub, err := in.begin(ctx, cp)
			if err != nil || pub != nil {
				if pub != nil {
					return *pub, nil
				}
				return Publication{}, err
			}
			cp = next
		case contract.CheckpointAuthorized:
			if cp.Pushed || in.Recovered {
				next, err := in.observe(ctx, cp)
				if err != nil {
					return Publication{}, err
				}
				cp = next
				continue
			}
			next, err := in.push(ctx, cp)
			if err != nil {
				return Publication{}, err
			}
			cp = next
		default:
			return Publication{}, errors.New("unknown publication checkpoint phase")
		}
	}
}

// begin asks the plane to arbitrate the candidate. A definitive refusal
// means no intent exists (the failure code); an ambiguous answer is
// resolved by repeating the identical Begin (the plane returns the
// original intent); the finalization bound expiring first is resolved by
// observation: no intent fails with publication_timeout, an authorized one
// is finished as such.
func (in *PublishInput) begin(ctx context.Context, cp contract.PublicationCheckpoint) (contract.PublicationCheckpoint, *Publication, error) {
	bctx := in.Begin
	if bctx == nil {
		bctx = ctx
	}
	req := contract.PublicationBeginRequest{Candidate: cp.Candidate, Tree: cp.Tree}
	for {
		in.stage("begin")
		resp, err := in.API.Begin(bctx, req)
		if err == nil {
			return in.authorized(ctx, cp, resp)
		}
		if ctx.Err() != nil {
			return cp, nil, ctx.Err()
		}
		if bctx.Err() == nil && !ambiguousErr(err) {
			return cp, &Publication{Failed: refusalCode(err)}, nil
		}
		if bctx.Err() != nil {
			return in.lateBegin(ctx, cp)
		}
		if err := in.pause(bctx); err != nil && ctx.Err() != nil {
			return cp, nil, ctx.Err()
		}
	}
}

// lateBegin resolves a Begin whose finalization bound expired: the lookup
// decides whether an intent exists (retried until the plane answers).
func (in *PublishInput) lateBegin(ctx context.Context, cp contract.PublicationCheckpoint) (contract.PublicationCheckpoint, *Publication, error) {
	for {
		in.stage("begin-lookup")
		st, err := in.API.Observe(ctx, "")
		if err == nil {
			switch st.Phase {
			case contract.PubPhaseNone:
				return cp, &Publication{Failed: contract.PubErrPublicationTimeout}, nil
			case contract.PubPhaseSettled:
				return in.settled(cp, st)
			}
			// Authorized: the intent exists but its push window is lost to the
			// finalization bound; finishing settles it failed (or published,
			// if a ref exists).
			fs, err := in.API.Finish(ctx, *st.PublicationID, contract.PubErrPublicationTimeout)
			if err == nil && fs.Phase == contract.PubPhaseSettled {
				return in.settled(cp, fs)
			}
		}
		if err := in.pause(ctx); err != nil {
			return cp, nil, err
		}
	}
}

// authorized records the intent and the deterministic commit (stored and
// synced) before any push.
func (in *PublishInput) authorized(ctx context.Context, cp contract.PublicationCheckpoint, resp contract.PublicationBeginResponse) (contract.PublicationCheckpoint, *Publication, error) {
	var base *plumbing.Hash
	if b := in.Binding.BaseCommit; b != nil {
		h := plumbing.NewHash(*b)
		base = &h
	}
	c, h, err := ResultCommit(in.TaskID, in.RoleID, in.Model, resp.State, base, plumbing.NewHash(cp.Tree))
	if err == nil {
		_, err = StoreCommit(ctx, in.TaskDir, c)
	}
	if err != nil {
		// Local failure after Begin: reported through Finish.
		return in.finish(ctx, cp, resp.PublicationID, PublicationCode(err))
	}
	id, state, exp, commit := resp.PublicationID, resp.State, resp.ExpiresAt, h.String()
	next := cp
	next.Phase, next.PublicationID, next.State, next.ExpiresAt, next.Commit = contract.CheckpointAuthorized, &id, &state, &exp, &commit
	if err := in.Save(next); err != nil {
		return cp, nil, err
	}
	in.stage("authorized")
	return next, nil, nil
}

// finish reports a definitive failure after Begin and adopts the
// settlement (the plane inspects the workspace first: an already committed
// own ref settles published).
func (in *PublishInput) finish(ctx context.Context, cp contract.PublicationCheckpoint, pubID, code string) (contract.PublicationCheckpoint, *Publication, error) {
	for {
		in.stage("finish")
		st, err := in.API.Finish(ctx, pubID, code)
		if err == nil && st.Phase == contract.PubPhaseSettled {
			next, pub, err := in.settled(cp, st)
			return next, pub, err
		}
		if err == nil || ambiguousErr(err) {
			// The plane has not settled yet (or the answer was lost): observe.
			if st, err := in.API.Observe(ctx, pubID); err == nil && st.Phase == contract.PubPhaseSettled {
				return in.settled(cp, st)
			}
		}
		if err := in.pause(ctx); err != nil {
			return cp, nil, err
		}
	}
}

// settled records the canonical settlement.
func (in *PublishInput) settled(cp contract.PublicationCheckpoint, st contract.PublicationStatus) (contract.PublicationCheckpoint, *Publication, error) {
	if st.Result == nil || st.Result.TaskID != in.TaskID || st.Result.Execution != cp.Execution {
		return cp, nil, errors.New("the plane's settlement names another execution")
	}
	next := cp
	next.Phase, next.Result = contract.CheckpointSettled, st.Result
	if next.PublicationID == nil {
		// A settlement adopted without a recorded intent (a resolved Begin).
		next.State, next.ExpiresAt, next.Commit = nil, nil, nil
	}
	if err := in.Save(next); err != nil {
		return cp, nil, err
	}
	in.stage("settled")
	return next, &Publication{Result: st.Result}, nil
}

// push checkpoints that a push begins, then sends exactly the result's
// missing objects with the single create command. Success and ambiguity
// are followed by observation; a definitive refusal by Finish.
func (in *PublishInput) push(ctx context.Context, cp contract.PublicationCheckpoint) (contract.PublicationCheckpoint, error) {
	next := cp
	next.Pushed = true
	if err := in.Save(next); err != nil {
		return cp, err
	}
	in.stage("push")
	ref := contract.TaskRefPrefix + in.TaskID
	commit := plumbing.NewHash(*cp.Commit)
	perr := in.sendPack(ctx, *cp.PublicationID, ref, commit)
	switch {
	case perr == nil, ctx.Err() != nil, ambiguousErr(perr):
		if ctx.Err() != nil {
			return next, ctx.Err()
		}
		return in.observe(ctx, next)
	}
	code := refusalCode(perr)
	if contract.TransferReason(perr) == contract.ReasonTaskRefExists {
		code = contract.PubErrTaskRefExists
	}
	settled, pub, err := in.finish(ctx, next, *cp.PublicationID, code)
	if err != nil {
		return next, err
	}
	_ = pub
	return settled, nil
}

func (in *PublishInput) sendPack(ctx context.Context, pubID, ref string, commit plumbing.Hash) error {
	p, err := in.API.Pusher(pubID)
	if err != nil {
		return err
	}
	defer p.Close()
	adv, err := p.ReceiveRefs(ctx)
	if err != nil {
		return err
	}
	if h, ok := adv[ref]; ok {
		// The own ref exists already: the plane decides (observation).
		_ = h
		return contract.New(contract.CodeUnavailable, "the task ref exists; observing the settlement")
	}
	var haves []plumbing.Hash
	for _, h := range adv {
		haves = append(haves, h)
	}
	db, err := workspacetransfer.OpenTaskDB(taskDB(in.TaskDir))
	if err != nil {
		return &SnapshotError{Code: contract.PubErrStorageFailed, Cause: err}
	}
	defer db.Close()
	objs, err := PushObjects(db.Store(), commit, haves)
	if err != nil {
		// The advertised haves are unknown locally: conservatively send the
		// complete closure.
		if objs, err = PushObjects(db.Store(), commit, nil); err != nil {
			return &SnapshotError{Code: contract.PubErrStorageFailed, Cause: err}
		}
	}
	pr, pw := io.Pipe()
	done := make(chan error, 1)
	go func() {
		err := db.WritePack(ctx, objs, pw)
		pw.CloseWithError(err)
		done <- err
	}()
	err = p.PushTask(ctx, ref, commit, pr)
	pr.CloseWithError(errors.New("push ended"))
	if werr := <-done; err == nil && werr != nil && !errors.Is(werr, io.ErrClosedPipe) {
		err = &SnapshotError{Code: contract.PubErrStorageFailed, Cause: werr}
	}
	return err
}

// observe reads the publication until the plane has settled it (the plane
// settles a lapsed intent itself).
func (in *PublishInput) observe(ctx context.Context, cp contract.PublicationCheckpoint) (contract.PublicationCheckpoint, error) {
	for {
		in.stage("observe")
		st, err := in.API.Observe(ctx, *cp.PublicationID)
		if err == nil && st.Phase == contract.PubPhaseSettled {
			next, _, err := in.settled(cp, st)
			return next, err
		}
		if err := in.pause(ctx); err != nil {
			return cp, err
		}
	}
}
