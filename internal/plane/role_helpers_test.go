package plane

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	pclient "github.com/wedevwork/callsheet/internal/client"
	"github.com/wedevwork/callsheet/internal/contract"
)

// eventLog keeps every stream and role event in order; waits consume from
// a cursor, so an event is never lost to a full channel.
type eventLog struct {
	mu     sync.Mutex
	events []string
	sig    chan struct{}
	cursor int
}

func newEventLog() *eventLog { return &eventLog{sig: make(chan struct{}, 1)} }

func (l *eventLog) add(e string) {
	l.mu.Lock()
	l.events = append(l.events, e)
	l.mu.Unlock()
	select {
	case l.sig <- struct{}{}:
	default:
	}
}

// find returns the index of want at or after the cursor.
func (l *eventLog) find(want string) int {
	l.mu.Lock()
	defer l.mu.Unlock()
	for i := l.cursor; i < len(l.events); i++ {
		if l.events[i] == want {
			return i
		}
	}
	return -1
}

// await waits (bounded, real time) for want after the cursor and moves the
// cursor past it.
func (l *eventLog) await(t *testing.T, want string) {
	t.Helper()
	deadline := time.After(testWait)
	for {
		if i := l.find(want); i >= 0 {
			l.mu.Lock()
			l.cursor = i + 1
			l.mu.Unlock()
			return
		}
		select {
		case <-l.sig:
		case <-deadline:
			// The event and the deadline may be ready together: recheck.
			if i := l.find(want); i >= 0 {
				l.mu.Lock()
				l.cursor = i + 1
				l.mu.Unlock()
				return
			}
			t.Fatalf("no event %q in %v", want, l.all())
		}
	}
}

// mark is the log position before an action whose events are awaited
// with awaitFrom.
func (l *eventLog) mark() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.events)
}

// awaitFrom waits (bounded, real time) for want at or after position
// from: an event name reused by another session (a request ID) is
// matched only when it follows the action that caused it. The cursor is
// not moved.
func (l *eventLog) awaitFrom(t *testing.T, from int, want string) {
	t.Helper()
	at := func() bool {
		l.mu.Lock()
		defer l.mu.Unlock()
		for i := from; i < len(l.events); i++ {
			if l.events[i] == want {
				return true
			}
		}
		return false
	}
	deadline := time.After(testWait)
	for !at() {
		select {
		case <-l.sig:
		case <-deadline:
			// The event and the deadline may be ready together: recheck.
			if at() {
				return
			}
			t.Fatalf("no event %q after position %d in %v", want, from, l.all())
		}
	}
}

// seen reports whether want occurred at any time.
func (l *eventLog) seen(want string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, e := range l.events {
		if e == want {
			return true
		}
	}
	return false
}

// seenPrefix reports whether an event with prefix occurred.
func (l *eventLog) seenPrefix(prefix string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, e := range l.events {
		if strings.HasPrefix(e, prefix) {
			return true
		}
	}
	return false
}

func (l *eventLog) all() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]string(nil), l.events...)
}

// hooks lets a test pause a mutation at a stage: the handler blocks until
// the test releases it. armed stages pause their next caller once; wait
// finds the paused call whether it arrived before or after wait began.
type hooks struct {
	mu      sync.Mutex
	armed   map[string]chan hookCall
	arrived map[string]chan hookCall
}

type hookCall struct {
	ctx     context.Context
	release chan struct{}
}

func newHooks() *hooks {
	return &hooks{armed: map[string]chan hookCall{}, arrived: map[string]chan hookCall{}}
}

// pause arms stage: the next mutation reaching it is delivered by wait.
func (h *hooks) pause(stage string) {
	c := make(chan hookCall, 1)
	h.mu.Lock()
	h.armed[stage], h.arrived[stage] = c, c
	h.mu.Unlock()
}

func (h *hooks) fn(stage string, ctx context.Context) {
	h.mu.Lock()
	c := h.armed[stage]
	delete(h.armed, stage)
	h.mu.Unlock()
	if c == nil {
		return
	}
	call := hookCall{ctx: ctx, release: make(chan struct{})}
	c <- call
	<-call.release
}

// wait returns the paused call at stage.
func (h *hooks) wait(t *testing.T, stage string) hookCall {
	t.Helper()
	h.mu.Lock()
	c := h.arrived[stage]
	h.mu.Unlock()
	if c == nil {
		t.Fatalf("stage %s not armed", stage)
	}
	select {
	case call := <-c:
		return call
	case <-time.After(testWait):
		t.Fatalf("no mutation reached %s", stage)
		return hookCall{}
	}
}

// rolePlane is a served plane for role tests: enrolled nodes, a fake node
// clock, every event recorded and mutation stages pausable.
type rolePlane struct {
	*nodePlane
	log   *eventLog
	hooks *hooks
}

func startRolePlane(t *testing.T, ids ...string) *rolePlane {
	t.Helper()
	return startRolePlaneWith(t, fast(testDeps(t)), ids...)
}

// fast makes d skip file and directory fsyncs: for tests whose subject is
// not durability (streams, distribution, mutation rules, the API). The
// registry contract and the mutation benchmark keep real syncs.
func fast(d *deps) *deps {
	noop := func(*os.File) error { return nil }
	d.syncFile, d.fileSync, d.rawFsync = noop, noop, noop
	return d
}

func startRolePlaneWith(t *testing.T, d *deps, ids ...string) *rolePlane {
	t.Helper()
	return startRolePlaneDoc(t, d, nil, ids...)
}

// startRolePlaneDoc starts a role plane whose root holds doc (when non-nil)
// as roles/registry.json.
func startRolePlaneDoc(t *testing.T, d *deps, doc *roleDoc, ids ...string) *rolePlane {
	t.Helper()
	rp := &rolePlane{log: newEventLog(), hooks: newHooks()}
	d.streamEvents = rp.log.add
	d.roleHook = rp.hooks.fn
	rp.nodePlane = startNodePlaneSetup(t, d, func(root string) {
		if doc != nil {
			writeRegistry(t, root, doc)
		}
	}, ids...)
	return rp
}

// roleCfg is a valid role configuration for node.
func roleCfg(id, name, node string) contract.RoleConfig {
	return contract.RoleConfig{ID: id, Name: name, Node: node, Adapter: "fake", Instruction: "/srv/manuals/" + id + "/instruction.md",
		Runbook: "/srv/manuals/" + id + "/runbook.md", Model: "example model", Effort: "medium", Concurrency: 2}
}

// call is one client operation running in the background.
type call[T any] struct {
	done chan struct{}
	v    T
	err  error
}

func async[T any](f func() (T, error)) *call[T] {
	c := &call[T]{done: make(chan struct{})}
	go func() {
		defer close(c.done)
		c.v, c.err = f()
	}()
	return c
}

func (c *call[T]) wait(t *testing.T) (T, error) {
	t.Helper()
	select {
	case <-c.done:
		return c.v, c.err
	case <-time.After(testWait):
		t.Fatal("the role operation did not return")
		var zero T
		return zero, nil
	}
}

// pending reports whether the call has not returned yet.
func (c *call[T]) pending() bool {
	select {
	case <-c.done:
		return false
	default:
		return true
	}
}

func (rp *rolePlane) addAsync(ctx context.Context, c contract.RoleConfig) *call[contract.RoleView] {
	return async(func() (contract.RoleView, error) { return rp.cl.AddRole(ctx, c) })
}

func (rp *rolePlane) setAsync(ctx context.Context, id string, p contract.RolePatch) *call[contract.RoleView] {
	return async(func() (contract.RoleView, error) { return rp.cl.SetRole(ctx, id, p) })
}

func (rp *rolePlane) rmAsync(ctx context.Context, id string, force bool) *call[struct{}] {
	return async(func() (struct{}, error) { return struct{}{}, rmRole(rp.cl, ctx, id, force) })
}

// rmRole removes id through the verified client (no operation token) and
// returns only the error: a pending forced removal is not an error.
func rmRole(cl *pclient.Client, ctx context.Context, id string, force bool) error {
	_, err := cl.RemoveRole(ctx, id, force, "")
	return err
}

// expectValidate reads role_validate rid and returns its candidate.
func (p *peer) expectValidate(rid string) contract.RoleConfig {
	p.t.Helper()
	f := p.expect(contract.FrameRoleValidate, rid)
	raw, err := contract.DecodeRoleValidate(f.Body)
	if err != nil {
		p.t.Fatal(err)
	}
	c, err := contract.ParseResolvedRoleConfig(raw, roleLookup)
	if err != nil {
		p.t.Fatalf("candidate %s: %v", raw, err)
	}
	return c
}

// reply answers validation rid (nil is success).
func (p *peer) reply(rid string, e *contract.Error) {
	p.t.Helper()
	p.send(contract.ProtocolVersion, contract.FrameRoleValidateResult, rid, contract.RoleValidateResult{Err: e})
}

// validateOK answers the next validation rid successfully.
func (p *peer) validateOK(rid string) contract.RoleConfig {
	p.t.Helper()
	c := p.expectValidate(rid)
	p.reply(rid, nil)
	return c
}

// add registers c through peer p, which answers validation vrid and then
// acknowledges the resulting snapshot srid.
func (rp *rolePlane) add(t *testing.T, p *peer, vrid, srid string, c contract.RoleConfig) contract.RoleView {
	t.Helper()
	res := rp.addAsync(bg, c)
	p.validateOK(vrid)
	v, err := res.wait(t)
	if err != nil {
		t.Fatalf("add %s: %v", c.ID, err)
	}
	p.ackReplace(srid)
	return v
}

// online connects a peer for id with one heartbeat after its initial
// snapshot, so the node is leased online with a current stream.
func (rp *rolePlane) online(t *testing.T, id string) *peer {
	t.Helper()
	p := rp.dial(t)
	p.connect(id, 1)
	return p
}

// view returns role id's current view (show).
func (rp *rolePlane) view(t *testing.T, id string) contract.RoleView {
	t.Helper()
	v, err := rp.cl.ShowRole(bg, id)
	if err != nil {
		t.Fatalf("show %s: %v", id, err)
	}
	return v
}

// registryBytes reads roles/registry.json (nil when absent).
func registryBytes(t *testing.T, root string) []byte {
	t.Helper()
	b, err := os.ReadFile(layout{root: root}.path(roleRegistryName))
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	return b
}

// writeRegistry writes a registry document directly (0600, roles/ 0700).
func writeRegistry(t testing.TB, root string, d *roleDoc) {
	t.Helper()
	dir := layout{root: root}.path(rolesName)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "registry.json"), encodeRoleDoc(d), 0o600); err != nil {
		t.Fatal(err)
	}
}

// docOf builds a registry document of recs (orders as given).
func docOf(revision, next int, recs ...contract.RoleRecord) *roleDoc {
	return &roleDoc{exists: true, revision: revision, nextOrder: next, roles: recs}
}

func record(c contract.RoleConfig, order int) contract.RoleRecord {
	return contract.RoleRecord{RoleConfig: c.Resolved(), RegistrationOrder: order}
}

// injector fails one (op, name) boundary on demand, concurrency-safe.
type injector struct {
	mu       sync.Mutex
	op, name string
	count    int
}

func (i *injector) set(op, name string) {
	i.mu.Lock()
	i.op, i.name, i.count = op, name, 0
	i.mu.Unlock()
}

func (i *injector) clear() { i.set("", "") }

func (i *injector) fail(op, name string) error {
	i.mu.Lock()
	defer i.mu.Unlock()
	if op == i.op && name == i.name {
		i.count++
		return fmt.Errorf("injected %s failure at %s", op, name)
	}
	return nil
}

func (i *injector) hits() int {
	i.mu.Lock()
	defer i.mu.Unlock()
	return i.count
}

// reason is an error's safe reason detail.
func reason(err error) string {
	var ce *contract.Error
	if e, ok := err.(*contract.Error); ok {
		ce = e
	}
	if ce == nil {
		return ""
	}
	r, _ := ce.Details["reason"].(string)
	return r
}

func wantReason(t *testing.T, err error, code contract.Code, why string) {
	t.Helper()
	if contract.CodeOf(err) != code || reason(err) != why {
		t.Fatalf("err = %v (reason %q), want %s/%s", err, reason(err), code, why)
	}
}

// ids returns n distinct role IDs.
func roleIDs(n int) []string {
	out := make([]string, n)
	for i := range out {
		out[i] = "role-" + strconv.Itoa(i+1)
	}
	return out
}

// jsonOf renders v compactly.
func jsonOf(t testing.TB, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}
