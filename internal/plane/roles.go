package plane

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"mime"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/wedevwork/callsheet/internal/adapter"
	"github.com/wedevwork/callsheet/internal/contract"
)

// roleLookup is the plane's adapter metadata: the product registry, known
// without probing any filesystem.
var roleLookup = adapter.Lookup()

// mutationTimeout bounds one role mutation handler (iteration 04); a
// validation's own controlTimeout is capped by what remains of it.
const mutationTimeout = 6 * time.Second

// roleService serves the role API (iteration 04). Mutations pass one
// nonblocking global gate: a competing add, set or rm is unavailable/busy,
// never queued. Reads never take the gate.
type roleService struct {
	roles  *roleRegistry
	reg    *nodeRegistry
	clock  nodeClock
	logger *slog.Logger
	lookup contract.AdapterLookup
	gate   atomic.Bool
	// events and hook, when non-nil, let tests observe and pause a
	// mutation at named stages ("submitting", "validated", "prepared")
	// with its context.
	events func(string)
	hook   func(stage string, ctx context.Context)
}

func newRoleService(roles *roleRegistry, reg *nodeRegistry, clock nodeClock, logger *slog.Logger) *roleService {
	return &roleService{roles: roles, reg: reg, clock: clock, logger: logger, lookup: roles.lookup}
}

func (rs *roleService) event(e string) {
	if rs.events != nil {
		rs.events(e)
	}
}

func (rs *roleService) at(stage string, ctx context.Context) {
	if rs.hook != nil {
		rs.hook(stage, ctx)
	}
}

// writeBounded writes a 2xx JSON response only if its encoding (plus the
// final LF) fits limit; a larger one is an internal error, never
// truncated.
func writeBounded(w http.ResponseWriter, status int, v any, limit int) {
	b, err := contract.Encode(v)
	if err != nil {
		writeError(w, contract.Wrap(contract.CodeInternal, "cannot encode the response", err))
		return
	}
	if len(b)+1 > limit {
		writeError(w, contract.New(contract.CodeInternal, "the response exceeds "+strconv.Itoa(limit)+" bytes"))
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	w.Write(append(b, '\n'))
}

func invalid(msg string) *contract.Error { return contract.New(contract.CodeInvalidArgument, msg) }

// handleRoles serves /api/v1/roles and /api/v1/roles/{id}. The version
// header is checked first, then the query, method, path, content type
// and body bounds, before any body is interpreted.
func (s *nodeService) handleRoles(w http.ResponseWriter, r *http.Request) {
	if !s.checkVersion(w, r) {
		return
	}
	rs := s.roles
	if r.URL.RawQuery != "" || r.URL.ForceQuery {
		writeError(w, invalid("query strings are not accepted"))
		return
	}
	id, one := strings.CutPrefix(r.URL.Path, contract.PathRoles+"/")
	if one && !contract.ValidSlug(id) {
		writeError(w, invalid("invalid role ID; want a 1-63 character slug of lowercase letters, digits and internal hyphens"))
		return
	}
	allowed := "GET or POST"
	if one {
		allowed = "GET, PATCH or DELETE"
	}
	switch {
	case r.Method == http.MethodGet:
		if hasBody(r) {
			writeError(w, invalid("GET requests must not carry a body"))
			return
		}
		if one {
			rs.show(w, id)
		} else {
			rs.list(w)
		}
		return
	case !one && r.Method == http.MethodPost, one && (r.Method == http.MethodPatch || r.Method == http.MethodDelete):
	default:
		writeError(w, invalid("method not allowed; use "+allowed))
		return
	}
	if !jsonContent(r) {
		writeError(w, invalid("a role change must have Content-Type application/json"))
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, contract.MaxRoleRequestBytes))
	if err != nil {
		writeError(w, invalid("the request body is unreadable or larger than "+strconv.Itoa(contract.MaxRoleRequestBytes)+" bytes"))
		return
	}
	switch r.Method {
	case http.MethodPost:
		rs.add(w, r, body)
	case http.MethodPatch:
		rs.set(w, r, id, body)
	default:
		rs.remove(w, r, id, body)
	}
}

// hasBody reports whether a request carries any body byte.
func hasBody(r *http.Request) bool {
	if r.Body == nil || r.Body == http.NoBody {
		return false
	}
	var b [1]byte
	n, _ := io.ReadFull(r.Body, b[:])
	return n > 0
}

// jsonContent requires exactly one Content-Type whose media type is
// application/json.
func jsonContent(r *http.Request) bool {
	vals := r.Header.Values("Content-Type")
	if len(vals) != 1 {
		return false
	}
	mt, _, err := mime.ParseMediaType(vals[0])
	return err == nil && mt == "application/json"
}

// view derives rec's view against the current registry state.
func (rs *roleService) view(rec contract.RoleRecord) contract.RoleView {
	return rs.reg.roleViews(func(*roleState) []contract.RoleRecord { return []contract.RoleRecord{rec} }, rs.lookup)[0]
}

func (rs *roleService) list(w http.ResponseWriter) {
	views := rs.reg.roleViews(func(s *roleState) []contract.RoleRecord { return sortedForList(s.visible.roles) }, rs.lookup)
	writeBounded(w, http.StatusOK, contract.RoleListResponse{Version: contract.ProtocolVersion, Roles: views}, contract.MaxRoleListBytes)
}

func notFound(id string) error {
	return contract.RoleError(contract.CodeNotFound, id, "", "", "", "role %s does not exist", id)
}

func (rs *roleService) show(w http.ResponseWriter, id string) {
	views := rs.reg.roleViews(func(s *roleState) []contract.RoleRecord {
		if rec, _, ok := s.visible.find(id); ok {
			return []contract.RoleRecord{rec}
		}
		return nil
	}, rs.lookup)
	if len(views) == 0 {
		writeCodeError(w, notFound(id))
		return
	}
	writeBounded(w, http.StatusOK, contract.RoleResponse{Version: contract.ProtocolVersion, Role: views[0]}, contract.MaxRoleResponseBytes)
}

// counterErr refuses a mutation that would exceed a counter bound.
func counterErr(what string) error {
	return contract.New(contract.CodeConflict, "the role registry's "+what+" is exhausted; no further role change is possible (restore or rebuild the registry)")
}

// addConflict checks an add against doc: duplicate ID, capacity and the
// counter bounds.
func addConflict(doc *roleDoc, c contract.RoleConfig) error {
	if _, _, ok := doc.find(c.ID); ok {
		return contract.RoleError(contract.CodeConflict, c.ID, "", "id", "", "role %s already exists; role IDs are unique (read it with callsheet role show)", c.ID)
	}
	if len(doc.roles) >= contract.MaxRoles {
		return contract.RoleError(contract.CodeConflict, c.ID, "", "", "", "the plane already holds the maximum of %d roles; remove a role first", contract.MaxRoles)
	}
	if doc.revision >= contract.MaxSafeInteger {
		return counterErr("revision")
	}
	if doc.nextOrder >= contract.MaxSafeInteger {
		return counterErr("registration order")
	}
	return nil
}

func (rs *roleService) nodeKnown(id string) error {
	if !rs.reg.known(id) {
		return contract.RoleError(contract.CodeNotFound, "", id, "node", "", "node %s is not enrolled with this plane", id)
	}
	return nil
}

// begin acquires the mutation gate (nonblocking) and completes a blocked
// registry's directory sync first, distributing the now confirmed state.
func (rs *roleService) begin() (func(), error) {
	if !rs.gate.CompareAndSwap(false, true) {
		return nil, contract.RoleError(contract.CodeUnavailable, "", "", "", contract.ReasonBusy, "another role change is in progress; retry")
	}
	release := func() {
		rs.gate.Store(false)
		rs.event("gate-released")
	}
	node, resynced, err := rs.roles.resync()
	if err != nil {
		release()
		return nil, err
	}
	if resynced {
		// The confirmed state and node's desired snapshot were installed
		// together (roleRegistry.install).
		rs.logger.Info("role registry durability confirmed", "node_id", node)
		rs.at("resynced", context.Background())
	}
	return release, nil
}

// mutation carries one mutation's bounds.
type mutation struct {
	ctx      context.Context
	deadline time.Time
}

func (rs *roleService) newMutation(r *http.Request) (mutation, context.CancelFunc) {
	deadline := rs.clock.Now().Add(mutationTimeout)
	ctx, cancel := clockTimeout(r.Context(), rs.clock, mutationTimeout)
	return mutation{ctx: ctx, deadline: deadline}, cancel
}

// live reports whether the mutation may still publish.
func (m mutation) live(rs *roleService) error {
	if m.ctx.Err() != nil || !rs.clock.Now().Before(m.deadline) {
		return contract.RoleError(contract.CodeUnavailable, "", "", "", contract.ReasonValidationTimeout, "the role change was canceled or ran out of time before publication; nothing was changed, retry")
	}
	return nil
}

// validateOn has the node's current stream validate c, within the
// mutation's remaining time, and returns the attachment generation it
// validated on.
func (rs *roleService) validateOn(m mutation, c contract.RoleConfig) (uint64, error) {
	rs.at("submitting", m.ctx)
	gen, st, err := rs.reg.target(c.Node)
	if err != nil {
		return 0, err
	}
	deadline := rs.clock.Now().Add(controlTimeout)
	if m.deadline.Before(deadline) {
		deadline = m.deadline
	}
	if err := st.validate(m.ctx, c, deadline); err != nil {
		rs.event("validation-failed " + c.ID + " " + string(contract.CodeOf(err)) + " " + errReason(err))
		return 0, err
	}
	rs.event("validated " + c.ID)
	return gen, nil
}

// commit publishes next after rechecking, twice (after validation and
// immediately before publication), the mutation's liveness, the validated
// attachment (when validated) and the base revision. The second recheck is
// the authorization instant; nothing is rolled back after it.
func (rs *roleService) commit(m mutation, base int, node string, gen uint64, validated bool, next *roleDoc) error {
	recheck := func() error {
		if err := m.live(rs); err != nil {
			return err
		}
		if validated {
			if err := rs.reg.stillTarget(node, gen); err != nil {
				return err
			}
		}
		if rs.roles.load().visible.revision != base {
			return contract.RoleError(contract.CodeUnavailable, "", "", "", contract.ReasonBusy, "the role registry changed during the role change; nothing was changed, retry")
		}
		return nil
	}
	if err := recheck(); err != nil {
		return err
	}
	rs.at("validated", m.ctx)
	tmp, err := rs.roles.prepare(next)
	if err != nil {
		return err
	}
	rs.at("prepared", m.ctx)
	if err := recheck(); err != nil {
		os.Remove(tmp)
		return err
	}
	if err := rs.roles.publish(tmp, next, node); err != nil {
		if errors.Is(err, errUnconfirmed) {
			rs.logger.Error("role registry durability unconfirmed", "node_id", node, "error", err)
		}
		return err
	}
	// publish installed the new state and node's desired snapshot together.
	rs.at("published", m.ctx)
	return nil
}

// respond writes a mutation's outcome. Handlers call it only after the
// gate was released, so a client's next sequential mutation can never find
// the gate still held by the one whose response it already received.
func respond(w http.ResponseWriter) func(status int, v any, err error) {
	return func(status int, v any, err error) {
		if err != nil {
			writeCodeError(w, err)
			return
		}
		writeBounded(w, status, v, contract.MaxRoleResponseBytes)
	}
}

// locked runs f with the mutation gate held (a blocked registry resynced
// first) and releases the gate before returning f's outcome.
func (rs *roleService) locked(r *http.Request, f func(m mutation) (int, any, error)) (int, any, error) {
	release, err := rs.begin()
	if err != nil {
		return 0, nil, err
	}
	defer release()
	m, cancel := rs.newMutation(r)
	defer cancel()
	return f(m)
}

func (rs *roleService) add(w http.ResponseWriter, r *http.Request, body []byte) {
	c, err := contract.ParseRoleConfig(body, rs.lookup)
	if err == nil {
		c = c.Resolved()
		if err = addConflict(rs.roles.load().visible, c); err == nil {
			err = rs.nodeKnown(c.Node)
		}
	}
	if err != nil {
		writeCodeError(w, err)
		return
	}
	respond(w)(rs.locked(r, func(m mutation) (int, any, error) {
		doc := rs.roles.load().visible
		if err := addConflict(doc, c); err != nil {
			return 0, nil, err
		}
		gen, err := rs.validateOn(m, c)
		if err != nil {
			return 0, nil, err
		}
		next, rec := doc.withAdded(c)
		if err := rs.commit(m, doc.revision, c.Node, gen, true, next); err != nil {
			return 0, nil, err
		}
		rs.logger.Info("role added", "role_id", c.ID, "node_id", c.Node, "registration_order", rec.RegistrationOrder)
		rs.event("added " + c.ID)
		return http.StatusCreated, contract.RoleResponse{Version: contract.ProtocolVersion, Role: rs.view(rec)}, nil
	}))
}

// merge applies p to the role id in doc and validates the result.
func (rs *roleService) merge(doc *roleDoc, id string, p contract.RolePatch) (contract.RoleRecord, int, contract.RoleConfig, error) {
	cur, i, ok := doc.find(id)
	if !ok {
		return cur, i, contract.RoleConfig{}, notFound(id)
	}
	merged := p.Apply(cur.RoleConfig)
	if err := contract.ValidateRoleConfig(merged, rs.lookup); err != nil {
		return cur, i, merged, err
	}
	return cur, i, merged, nil
}

func (rs *roleService) set(w http.ResponseWriter, r *http.Request, id string, body []byte) {
	p, err := contract.ParseRolePatch(body)
	if err == nil && p.Adapter != nil {
		if _, ok := rs.lookup(*p.Adapter); !ok {
			err = contract.RoleError(contract.CodeInvalidArgument, id, "", "adapter", "", "unknown adapter; registered adapters: fake")
		}
	}
	if err == nil {
		_, _, _, err = rs.merge(rs.roles.load().visible, id, p)
	}
	if err != nil {
		writeCodeError(w, err)
		return
	}
	respond(w)(rs.locked(r, func(m mutation) (int, any, error) {
		doc := rs.roles.load().visible
		cur, i, merged, err := rs.merge(doc, id, p)
		if err == nil {
			err = rs.nodeKnown(cur.Node)
		}
		if err != nil {
			return 0, nil, err
		}
		// Every set, a no-op included, revalidates the whole merged
		// configuration on the worker.
		gen, err := rs.validateOn(m, merged)
		if err != nil {
			return 0, nil, err
		}
		if contract.SameRoleConfig(merged, cur.RoleConfig) {
			// A no-op: the same rechecks, but no temporary, write, revision
			// or observation change.
			err := m.live(rs)
			if err == nil {
				err = rs.reg.stillTarget(cur.Node, gen)
			}
			if err != nil {
				return 0, nil, err
			}
			rs.event("unchanged " + id)
			return http.StatusOK, contract.RoleResponse{Version: contract.ProtocolVersion, Role: rs.view(cur)}, nil
		}
		if doc.revision >= contract.MaxSafeInteger {
			return 0, nil, counterErr("revision")
		}
		next, rec := doc.withReplaced(i, merged)
		if err := rs.commit(m, doc.revision, cur.Node, gen, true, next); err != nil {
			return 0, nil, err
		}
		rs.logger.Info("role changed", "role_id", id, "node_id", cur.Node)
		rs.event("changed " + id)
		return http.StatusOK, contract.RoleResponse{Version: contract.ProtocolVersion, Role: rs.view(rec)}, nil
	}))
}

// remove deletes a role. rm needs no worker and works offline; in this
// iteration force=false and force=true behave identically (no tasks
// exist): no cancellation, task record or grace period.
func (rs *roleService) remove(w http.ResponseWriter, r *http.Request, id string, body []byte) {
	force, err := contract.ParseRoleRemoveRequest(body)
	if err == nil {
		if _, _, ok := rs.roles.load().visible.find(id); !ok {
			err = notFound(id)
		}
	}
	if err != nil {
		writeCodeError(w, err)
		return
	}
	respond(w)(rs.locked(r, func(m mutation) (int, any, error) {
		doc := rs.roles.load().visible
		cur, i, ok := doc.find(id)
		if !ok {
			return 0, nil, notFound(id)
		}
		if doc.revision >= contract.MaxSafeInteger {
			return 0, nil, counterErr("revision")
		}
		if err := rs.commit(m, doc.revision, cur.Node, 0, false, doc.withRemoved(i)); err != nil {
			return 0, nil, err
		}
		rs.logger.Info("role removed", "role_id", id, "node_id", cur.Node, "force", force)
		rs.event("removed " + id)
		return http.StatusOK, contract.RoleRemoveResponse{Version: contract.ProtocolVersion, Removed: id}, nil
	}))
}

// errReason is err's safe reason detail, if any (for test events).
func errReason(err error) string {
	var ce *contract.Error
	if errors.As(err, &ce) && ce.Details != nil {
		if r, ok := ce.Details["reason"].(string); ok {
			return r
		}
	}
	return ""
}
