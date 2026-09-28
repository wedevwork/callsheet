package plane

import (
	"context"
	"encoding/hex"
	"errors"
	"io"
	"net/http"
	"sync"
	"time"

	"github.com/wedevwork/callsheet/internal/contract"
)

// Forced role removal (iteration 06b, FP-4, controls.md "Forced role
// removal"): role rm --force persists a fence for the exact instance in
// the registry (schema 2) under the mutation gate, which publishes the
// instance's nonacceptance; the plane then cancels every held task
// (FP-1), awaits their durable resolution and deletes the role and its
// fence in one registry transaction. One Run-owned coordinator, bounded by
// the registry, resumes durable fences after a restart; nothing holds the
// gate while tasks are cancelled, cleaned up or resolved.

// removalRetry is the coordinator's coalesced retry interval.
const removalRetry = time.Second

// removalOp is one fence's in-memory operation: its instance, token and
// node, and the joined response waiters (woken once at confirmed
// deletion).
type removalOp struct {
	key     instanceKey
	op      string
	node    string
	waiters []chan struct{}
}

// removalCoordinator owns every force removal of one Run.
type removalCoordinator struct {
	rs   *roleService
	mu   sync.Mutex
	ops  map[instanceKey]*removalOp
	kick chan struct{}
	stop chan struct{}
	wg   sync.WaitGroup
	once sync.Once
}

func newRemovalCoordinator(rs *roleService) *removalCoordinator {
	return &removalCoordinator{rs: rs, ops: map[instanceKey]*removalOp{}, kick: make(chan struct{}, 1), stop: make(chan struct{})}
}

// start runs the coordinator; it adopts every durable fence first.
func (rc *removalCoordinator) start() {
	rc.wg.Add(1)
	go rc.loop()
	notifyChan(rc.kick)
}

// close stops the coordinator and joins it (plane shutdown); a pending
// removal resumes from its durable fence on the next Run.
func (rc *removalCoordinator) close() {
	rc.once.Do(func() { close(rc.stop) })
	rc.wg.Wait()
	rc.mu.Lock()
	for _, o := range rc.ops {
		o.waiters = nil
	}
	rc.mu.Unlock()
}

// wake requests a pass (nonblocking; called under the task lock by the
// release hook).
func (rc *removalCoordinator) wake(instanceKey) { notifyChan(rc.kick) }

// join registers a response waiter on the operation of key (creating it),
// woken at the confirmed deletion.
func (rc *removalCoordinator) join(key instanceKey, op, node string) chan struct{} {
	c := make(chan struct{})
	rc.mu.Lock()
	o := rc.ops[key]
	if o == nil {
		o = &removalOp{key: key, op: op, node: node}
		rc.ops[key] = o
	}
	o.waiters = append(o.waiters, c)
	rc.mu.Unlock()
	notifyChan(rc.kick)
	return c
}

// leave drops a detached response waiter.
func (rc *removalCoordinator) leave(key instanceKey, c chan struct{}) {
	rc.mu.Lock()
	defer rc.mu.Unlock()
	if o := rc.ops[key]; o != nil {
		for i, w := range o.waiters {
			if w == c {
				o.waiters = append(o.waiters[:i], o.waiters[i+1:]...)
				return
			}
		}
	}
}

func (rc *removalCoordinator) loop() {
	defer rc.wg.Done()
	clock := rc.rs.clock
	var timer <-chan time.Time
	stopTimer := func() bool { return false }
	defer func() { stopTimer() }()
	for {
		select {
		case <-rc.kick:
		case <-timer:
		case <-rc.stop:
			return
		}
		stopTimer()
		timer, stopTimer = nil, func() bool { return false }
		if rc.pass() {
			timer, stopTimer = clock.NewTimer(removalRetry)
			rc.rs.event("removal-retry-armed")
		}
	}
}

// pass advances every operation once and reports whether any remains.
func (rc *removalCoordinator) pass() bool {
	rs := rc.rs
	st := rs.roles.load()
	if st.blocked {
		// Only confirmed fences drive cancellations: resync a visible but
		// unconfirmed registry first (under the gate, like a mutation).
		if !rs.gate.CompareAndSwap(false, true) {
			return true
		}
		_, _, err := rs.roles.resync()
		rs.gate.Store(false)
		if err != nil {
			rs.event("removal-resync-failed")
			return true
		}
		st = rs.roles.load()
	}
	conf := st.confirmed
	// A durable fence is an operation (a restart resumes it).
	rc.mu.Lock()
	for _, f := range conf.removals {
		k := instanceKey{id: f.ID, order: f.RegistrationOrder}
		if rc.ops[k] == nil {
			node := ""
			if r, _, ok := conf.find(f.ID); ok {
				node = r.Node
			}
			rc.ops[k] = &removalOp{key: k, op: f.OperationID, node: node}
		}
	}
	ops := make([]*removalOp, 0, len(rc.ops))
	for _, o := range rc.ops {
		ops = append(ops, o)
	}
	rc.mu.Unlock()
	pending := false
	for _, o := range ops {
		if f := conf.fenceOf(o.key.id, o.key.order); f == nil || f.OperationID != o.op {
			if r, _, ok := conf.find(o.key.id); !ok || r.RegistrationOrder != o.key.order {
				// Deleted (confirmed): the operation completed.
				rc.complete(o)
				continue
			}
			// Not (yet) a confirmed fence: nothing is cancelled for it.
			pending = true
			continue
		}
		held := rs.tasks.heldIDs(o.key)
		for _, id := range held {
			if err := rs.tasks.requestCancel(id); err != nil {
				rs.logger.Error("force removal cannot cancel a task", "role_id", o.key.id, "task_id", id, "error", err)
			}
		}
		if len(held) > 0 {
			pending = true
			continue
		}
		if !rc.delete(o) {
			pending = true
		}
	}
	return pending
}

// delete runs the ordinary durable registry removal of o's instance: under
// the gate (nonblocking; a busy gate retries), with a fresh mutation
// budget, rechecking the exact instance, the fence and zero held
// reservations at the authorization instant.
func (rc *removalCoordinator) delete(o *removalOp) bool {
	rs := rc.rs
	if !rs.gate.CompareAndSwap(false, true) {
		return false
	}
	// Waiters are woken only after the gate is released: a caller answered
	// 200 may mutate at once without finding the gate busy.
	done := false
	defer func() {
		rs.gate.Store(false)
		if done {
			rc.complete(o)
			rs.at("removal-completed", context.Background())
		}
	}()
	if _, _, err := rs.roles.resync(); err != nil {
		return false
	}
	doc := rs.roles.load().visible
	cur, i, ok := doc.find(o.key.id)
	if !ok || cur.RegistrationOrder != o.key.order {
		done = true
		return true
	}
	if f := doc.fenceOf(cur.ID, cur.RegistrationOrder); f == nil || f.OperationID != o.op {
		return false
	}
	guard := func() error {
		if c := rs.tasks.heldCount(o.key); c.held != 0 {
			return errors.New("the instance holds reservations")
		}
		return nil
	}
	if guard() != nil {
		return false
	}
	ctx, cancel := clockTimeout(context.Background(), rs.clock, mutationTimeout)
	defer cancel()
	m := mutation{ctx: ctx, deadline: rs.clock.Now().Add(mutationTimeout)}
	if err := rs.commit(m, doc.revision, cur.Node, 0, false, doc.withRemoved(i), guard); err != nil {
		rs.logger.Error("force removal deletion not confirmed; retrying", "role_id", cur.ID, "error", err)
		rs.event("removal-delete-failed " + cur.ID)
		return false
	}
	rs.logger.Info("role removed", "role_id", cur.ID, "node_id", cur.Node, "force", true, "operation_id", o.op)
	rs.event("removed " + cur.ID)
	done = true
	return true
}

// complete wakes o's waiters and forgets it.
func (rc *removalCoordinator) complete(o *removalOp) {
	rc.mu.Lock()
	defer rc.mu.Unlock()
	if rc.ops[o.key] == o {
		delete(rc.ops, o.key)
	}
	for _, c := range o.waiters {
		close(c)
	}
	o.waiters = nil
}

// heldCount returns instance k's reservation counts.
func (ts *taskService) heldCount(k instanceKey) heldCount {
	ts.mu.Lock()
	defer ts.mu.Unlock()
	return ts.heldLocked(k)
}

// newOperationID returns 32 random lowercase hex digits.
func (rs *roleService) newOperationID() (string, error) {
	var b [16]byte
	if _, err := io.ReadFull(rs.rand, b[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(b[:]), nil
}

// confirmedFence reports whether st's directory-confirmed document holds
// cur's exact instance fenced by operation op.
func confirmedFence(st *roleState, cur contract.RoleRecord, op string) bool {
	c, _, ok := st.confirmed.find(cur.ID)
	if !ok || c.RegistrationOrder != cur.RegistrationOrder {
		return false
	}
	f := st.confirmed.fenceOf(c.ID, c.RegistrationOrder)
	return f != nil && f.OperationID == op
}

// removing is the conflict for a mutation of a fenced instance.
func removing(id string) error {
	return contract.RoleError(contract.CodeConflict, id, "", "", contract.ReasonRoleRemoving, "role %s is being removed (role rm --force is in progress; see role show); nothing was changed", id)
}

// operationMismatch refuses a token naming no fence on id's instance.
func operationMismatch(id string) error {
	return contract.RoleError(contract.CodeConflict, id, "", "", contract.ReasonRemovalOperationMismatch,
		"the operation ID does not name role %s's current removal (see role show's removal); nothing was changed", id)
}

// forceRemove serves role rm --force: it persists (or joins) the exact
// instance's fence, then awaits the durable deletion within the mutation
// budget: 200 when it is directory-confirmed, else 202 with the operation.
// The initial no-token request starts or joins the current instance's
// operation; a token joins only the matching fence on that instance.
func (rs *roleService) forceRemove(r *http.Request, id, op string) (int, any, error) {
	m, cancel := rs.newMutation(r)
	defer cancel()
	st := rs.roles.load()
	doc := st.visible
	cur, _, ok := doc.find(id)
	if !ok {
		return 0, nil, notFound(id)
	}
	f := doc.fenceOf(cur.ID, cur.RegistrationOrder)
	if f != nil && !confirmedFence(st, cur, f.OperationID) {
		// A visible fence is accepted only once that exact fence is
		// directory-confirmed: complete the blocked registry's sync first
		// (under the gate). While it keeps failing the storage error is the
		// answer; the coordinator keeps ownership and retries.
		release, err := rs.begin()
		if err != nil {
			rs.rm.wake(instanceKey{})
			return 0, nil, err
		}
		release()
		st = rs.roles.load()
		doc = st.visible
		if cur, _, ok = doc.find(id); !ok {
			return 0, nil, notFound(id)
		}
		f = doc.fenceOf(cur.ID, cur.RegistrationOrder)
		if f != nil && !confirmedFence(st, cur, f.OperationID) {
			return 0, nil, contract.RoleError(contract.CodeUnavailable, id, "", "", contract.ReasonBusy, "role %s's removal fence is not yet confirmed durable; retry", id)
		}
	}
	switch {
	case f != nil && op != "" && f.OperationID != op, f == nil && op != "":
		return 0, nil, operationMismatch(id)
	}
	if f == nil {
		release, err := rs.begin()
		if err != nil {
			return 0, nil, err
		}
		doc = rs.roles.load().visible
		cur, i, ok := doc.find(id)
		if !ok {
			release()
			return 0, nil, notFound(id)
		}
		f = doc.fenceOf(cur.ID, cur.RegistrationOrder)
		if f == nil {
			if doc.revision >= contract.MaxSafeInteger {
				release()
				return 0, nil, counterErr("revision")
			}
			if rs.tasks.heldCount(keyOf(cur)).held == 0 {
				// Nothing to cancel: the fence and its deletion are one
				// transaction, the ordinary removal (the gate excludes
				// admissions, and held reservations only ever decrease).
				guard := func() error {
					if c := rs.tasks.heldCount(keyOf(cur)); c.held != 0 {
						return rs.tasks.removalCheck(keyOf(cur))
					}
					return nil
				}
				err := rs.commit(m, doc.revision, cur.Node, 0, false, doc.withRemoved(i), guard)
				release()
				if err != nil {
					return 0, nil, err
				}
				rs.logger.Info("role removed", "role_id", id, "node_id", cur.Node, "force", true)
				rs.event("removed " + id)
				return http.StatusOK, contract.RoleRemoveResponse{Version: contract.ProtocolVersion, Removed: id}, nil
			}
			tok, err := rs.newOperationID()
			if err != nil {
				release()
				return 0, nil, contract.Wrap(contract.CodeInternal, "cannot generate a removal operation ID", err)
			}
			next := doc.withFence(i, tok)
			if err := rs.commit(m, doc.revision, cur.Node, 0, false, next, nil); err != nil {
				release()
				if errors.Is(err, errUnconfirmed) {
					// Visible but unconfirmed: the coordinator resyncs it and only
					// then drives its cancellations.
					rs.rm.wake(instanceKey{})
				}
				return 0, nil, err
			}
			rs.logger.Info("role removal fenced", "role_id", id, "node_id", cur.Node, "registration_order", cur.RegistrationOrder, "operation_id", tok)
			rs.event("fenced " + id)
			f = next.fenceOf(cur.ID, cur.RegistrationOrder)
		}
		release()
	}
	pending := contract.RoleRemovePendingResponse{Version: contract.ProtocolVersion, OperationID: f.OperationID, RoleID: id,
		RegistrationOrder: f.RegistrationOrder, Removing: true}
	key := instanceKey{id: id, order: f.RegistrationOrder}
	done := rs.rm.join(key, f.OperationID, cur.Node)
	rs.at("force-joined", m.ctx)
	select {
	case <-done:
		return http.StatusOK, contract.RoleRemoveResponse{Version: contract.ProtocolVersion, Removed: id}, nil
	case <-m.ctx.Done():
	}
	rs.rm.leave(key, done)
	select {
	case <-done:
		return http.StatusOK, contract.RoleRemoveResponse{Version: contract.ProtocolVersion, Removed: id}, nil
	default:
	}
	if err := r.Context().Err(); err != nil {
		// The caller detached: the plane owns completion.
		return 0, nil, err
	}
	return http.StatusAccepted, pending, nil
}
