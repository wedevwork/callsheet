package plane

import (
	"strings"
	"testing"

	"github.com/coder/websocket"

	"github.com/wedevwork/callsheet/internal/contract"
)

// canAccept reads role id's can_accept over HTTP (show and node show must
// agree).
func (rp *rolePlane) canAccept(t *testing.T, id string) bool {
	t.Helper()
	v := rp.view(t, id)
	n := rp.show(t, v.Node)
	for _, rs := range n.Roles {
		if rs.RoleID == id {
			if rs.CanAccept != v.CanAccept || rs.Concurrency != v.Concurrency || rs.Inflight != 0 {
				t.Fatalf("role %s: show %+v disagrees with node %+v", id, v, rs)
			}
			return v.CanAccept
		}
	}
	t.Fatalf("node %s does not list role %s: %+v", v.Node, id, n.Roles)
	return false
}

// TestRoleDistributionContract is the plane side of UT FP-5; its reconnect
// subtest is delegated from tests/function (TestRoleReadiness/reconnect).
// Readiness is observed only through the public HTTP views. Do not rename or
// skip its subtests.
func TestRoleDistributionContract(t *testing.T) {
	t.Parallel()
	t.Run("attach", func(t *testing.T) {
		t.Parallel()
		// After hello_ok the node receives one full snapshot of exactly its
		// own roles (registration order) at the current registry revision;
		// a node without roles gets an empty one at that revision.
		a1, b1, a2 := roleCfg("a1", "coder", idA), roleCfg("b1", "coder", idB), roleCfg("a2", "coder", idA)
		rp := startRolePlaneDoc(t, fast(testDeps(t)), docOf(5, 4, record(a1, 1), record(b1, 2), record(a2, 3)), idA, idB, idC)
		pa := rp.dial(t)
		pa.hello(idA)
		pa.expect(contract.FrameHelloOK, "h1")
		if b := pa.ackReplace("p1"); b.Revision != 5 || len(b.Roles) != 2 || b.Roles[0].ID != "a1" || b.Roles[1].ID != "a2" || b.Roles[1].RegistrationOrder != 3 {
			t.Fatalf("node A snapshot = %+v", b)
		}
		pc := rp.dial(t)
		pc.hello(idC)
		pc.expect(contract.FrameHelloOK, "h1")
		if b := pc.ackReplace("p1"); b.Revision != 5 || len(b.Roles) != 0 {
			t.Fatalf("node C snapshot = %+v", b)
		}
		pa.heartbeat(1)
		pc.heartbeat(1)
		// A mutation on A sends A a snapshot; C gets nothing.
		if err := rp.cl.RemoveRole(bg, "a1", false); err != nil {
			t.Fatal(err)
		}
		if b := pa.ackReplace("p2"); b.Revision != 6 || len(b.Roles) != 1 || b.Roles[0].ID != "a2" {
			t.Fatalf("after rm = %+v", b)
		}
		pc.heartbeat(2) // its next message is the ack, not a snapshot
		pa.heartbeat(2)
		if rp.log.seenPrefix("replace-sent " + idC + " p2") {
			t.Fatal("an unaffected node was sent a snapshot")
		}
	})
	t.Run("publication", func(t *testing.T) {
		t.Parallel()
		// A reader never sees a published roster with readiness from the
		// previous revision: the visible registry and the affected node's
		// readiness invalidation change together, for a publication and
		// for a resync after ambiguous durability. Reads happen while the
		// mutation is paused right after each step.
		inj := &injector{}
		d := fast(testDeps(t))
		d.fail = inj.fail
		a, b := roleCfg("a", "coder", idA), roleCfg("b", "coder", idA)
		rp := startRolePlaneDoc(t, d, docOf(1, 2, record(a, 1)), idA)
		p := rp.online(t, idA)
		p.rev, p.roles = 1, []contract.RoleRecord{record(a, 1)}
		p.ready = map[string]bool{"a": true, "b": true}
		p.heartbeat(2)
		if !rp.canAccept(t, "a") {
			t.Fatal("a not ready at revision 1")
		}
		readAt := func(stage string, want []string) {
			t.Helper()
			call := rp.hooks.wait(t, stage)
			list, err := rp.cl.ListRoles(bg)
			if err != nil {
				t.Fatal(err)
			}
			var ids []string
			for _, v := range list {
				ids = append(ids, v.ID)
				if v.CanAccept {
					t.Errorf("at %s: %s is ready with readiness from the previous revision (roster %v)", stage, v.ID, want)
				}
			}
			if strings.Join(ids, ",") != strings.Join(want, ",") {
				t.Errorf("at %s: roster %v, want %v", stage, ids, want)
			}
			for _, st := range rp.show(t, idA).Roles {
				if st.CanAccept {
					t.Errorf("at %s: node show reports %s ready", stage, st.RoleID)
				}
			}
			close(call.release)
		}
		// (1) Publication: revision 2 adds b; a is unchanged.
		rp.hooks.pause("published")
		res := rp.addAsync(bg, b)
		p.validateOK("p2")
		readAt("published", []string{"a", "b"})
		if _, err := res.wait(t); err != nil {
			t.Fatal(err)
		}
		p.ackReplace("p3")
		p.heartbeat(3)
		if !rp.canAccept(t, "a") || !rp.canAccept(t, "b") {
			t.Fatal("not ready at revision 2")
		}
		// (2) Ambiguous durability, then the resync that confirms it.
		inj.set("dirsync", rolesName)
		wantCode(t, rp.cl.RemoveRole(bg, "b", false), contract.CodeInternal, "durability is unconfirmed")
		p.heartbeat(4)
		inj.clear()
		rp.hooks.pause("resynced")
		rm := rp.rmAsync(bg, "a", false)
		readAt("resynced", []string{"a"})
		if _, err := rm.wait(t); err != nil {
			t.Fatal(err)
		}
		if t.Failed() {
			t.FailNow()
		}
	})
	t.Run("readiness", func(t *testing.T) {
		t.Parallel()
		rp := startRolePlane(t, idA, idB)
		p := rp.online(t, idA)
		rp.add(t, p, "p2", "p3", roleCfg("a", "coder", idA))
		// Acknowledged, but no heartbeat at the new revision yet: false.
		if rp.canAccept(t, "a") {
			t.Fatal("ready before any heartbeat at the revision")
		}
		p.heartbeat(2)
		if rp.canAccept(t, "a") {
			t.Fatal("ready while the heartbeat reports false")
		}
		p.ready = map[string]bool{"a": true}
		p.heartbeat(3)
		if v := rp.view(t, "a"); !v.CanAccept || v.NodeLiveness != contract.LivenessOnline || v.Concurrency != 2 || v.Inflight != 0 || !v.AdapterTestOnly {
			t.Fatalf("ready view = %+v", v)
		}
		// A new role on the node pauses readiness of the whole node,
		// unchanged roles included, from publication until the sidecar
		// acknowledged and reported the new revision.
		res := rp.addAsync(bg, roleCfg("b", "coder", idA))
		p.validateOK("p4")
		if _, err := res.wait(t); err != nil {
			t.Fatal(err)
		}
		if rp.canAccept(t, "a") || rp.canAccept(t, "b") {
			t.Fatal("readiness exposed while the new snapshot is unacknowledged")
		}
		// A heartbeat whose write began before installation (previous
		// revision) is still legitimate while the new snapshot is in
		// flight; it renews the lease but exposes nothing.
		seenBefore := *rp.show(t, idA).LastSeen
		rp.clk.Advance(1)
		p.heartbeat(4)
		if n := rp.show(t, idA); !n.LastSeen.After(seenBefore) {
			t.Fatalf("old-revision heartbeat did not renew the lease: %+v", n)
		}
		if rp.canAccept(t, "a") {
			t.Fatal("old-revision heartbeat exposed readiness")
		}
		b := p.ackReplace("p5")
		if b.Revision != 2 || len(b.Roles) != 2 {
			t.Fatalf("snapshot = %+v", b)
		}
		if rp.canAccept(t, "a") {
			t.Fatal("ready at a new revision before its heartbeat")
		}
		p.ready = map[string]bool{"a": true, "b": false}
		p.heartbeat(5)
		if !rp.canAccept(t, "a") || rp.canAccept(t, "b") {
			t.Fatal("per-role readiness not exact")
		}
		// Control messages never renew the lease: only heartbeats do.
		seen := *rp.show(t, idA).LastSeen
		rp.clk.Advance(1)
		if err := rp.cl.RemoveRole(bg, "b", false); err != nil {
			t.Fatal(err)
		}
		p.ackReplace("p6")
		if n := rp.show(t, idA); !n.LastSeen.Equal(seen) {
			t.Fatalf("a snapshot acknowledgement renewed the lease: %+v", n)
		}
		p.ready = map[string]bool{"a": true}
		p.heartbeat(6)
		if !rp.canAccept(t, "a") {
			t.Fatal("not ready after the rm snapshot")
		}
		// Lease expiry masks readiness on read, without the sweep.
		rp.log.await(t, "ack "+idA+" b6")
		rp.clk.Advance(leaseDuration)
		if v := rp.view(t, "a"); v.CanAccept || v.NodeLiveness != contract.LivenessOffline {
			t.Fatalf("after lease expiry = %+v", v)
		}
		p.closed()
	})
	t.Run("reconnect", func(t *testing.T) {
		t.Parallel()
		rp := startRolePlane(t, idA)
		p := rp.online(t, idA)
		rp.add(t, p, "p2", "p3", roleCfg("a", "coder", idA))
		p.ready = map[string]bool{"a": true}
		p.heartbeat(2)
		if !rp.canAccept(t, "a") {
			t.Fatal("not ready")
		}
		// set is distributed as a full replacement at a new revision; the
		// readiness it had is not reused.
		name := "reviewer"
		res := rp.setAsync(bg, "a", contract.RolePatch{Name: &name})
		if c := p.validateOK("p4"); c.Name != "reviewer" || c.ID != "a" {
			t.Fatalf("set candidate %+v", c)
		}
		if v, err := res.wait(t); err != nil || v.Name != "reviewer" || v.CanAccept {
			t.Fatalf("set = %+v %v", v, err)
		}
		if b := p.ackReplace("p5"); b.Revision != 2 || b.Roles[0].Name != "reviewer" || b.Roles[0].RegistrationOrder != 1 {
			t.Fatalf("set snapshot = %+v", b)
		}
		if rp.canAccept(t, "a") {
			t.Fatal("readiness survived a set before a heartbeat")
		}
		p.heartbeat(3)
		if !rp.canAccept(t, "a") {
			t.Fatal("not ready after the set heartbeat")
		}
		// Disconnect masks readiness at once while the lease keeps the
		// node online.
		p.c.Close(websocket.StatusNormalClosure, "")
		rp.log.await(t, "detached "+idA)
		if v := rp.view(t, "a"); v.CanAccept || v.NodeLiveness != contract.LivenessOnline {
			t.Fatalf("after disconnect = %+v", v)
		}
		// rm works while disconnected and sends nothing; the reconnect's
		// full replacement reflects it exactly.
		if err := rp.cl.RemoveRole(bg, "a", false); err != nil {
			t.Fatal(err)
		}
		q := rp.dial(t)
		q.hello(idA)
		q.expect(contract.FrameHelloOK, "h1")
		if b := q.ackReplace("p1"); b.Revision != 3 || len(b.Roles) != 0 {
			t.Fatalf("reconnect snapshot after rm = %+v", b)
		}
		q.heartbeat(1)
		rp.add(t, q, "p2", "p3", roleCfg("b", "coder", idA))
		q.ready = map[string]bool{"b": true}
		q.heartbeat(2)
		if !rp.canAccept(t, "b") {
			t.Fatal("not ready after reconnect and add")
		}
		// A replaced stream's readiness never carries over: the new session
		// starts from nothing acknowledged and gets the exact full snapshot.
		q.c.CloseNow()
		rp.log.await(t, "detached "+idA)
		q2 := rp.dial(t)
		q2.hello(idA)
		q2.expect(contract.FrameHelloOK, "h1")
		// A first heartbeat at revision 0 while the snapshot is in flight
		// renews the lease only.
		q2.heartbeat(1)
		if rp.canAccept(t, "b") {
			t.Fatal("readiness carried over to a new session")
		}
		b := q2.ackReplace("p1")
		if b.Revision != 4 || len(b.Roles) != 1 || b.Roles[0].ID != "b" || b.Roles[0].RegistrationOrder != 2 {
			t.Fatalf("reconnect snapshot = %+v", b)
		}
		q2.ready = map[string]bool{"b": true}
		q2.heartbeat(2)
		if !rp.canAccept(t, "b") {
			t.Fatal("not ready in the new session")
		}
		// Restart on a restored, older registry: revision 3 is lower than
		// what the node last installed, yet it is the new session's full
		// snapshot; every readiness starts false.
		q2.closeAsync()
		root := rp.root
		if err := rp.stop(t); err == nil {
			t.Fatal("run returned nil")
		}
		writeRegistry(t, root, docOf(3, 3, record(roleCfg("b", "coder", idA), 2)))
		rp2 := &rolePlane{log: newEventLog(), hooks: newHooks()}
		d := testDeps(t)
		d.streamEvents = rp2.log.add
		rp2.nodePlane = serveNodePlaneAt(t, d, root)
		if v := rp2.view(t, "b"); v.CanAccept || v.NodeLiveness != contract.LivenessOffline || v.RegistrationOrder != 2 {
			t.Fatalf("after restart = %+v", v)
		}
		r := rp2.dial(t)
		r.hello(idA)
		r.expect(contract.FrameHelloOK, "h1")
		if b := r.ackReplace("p1"); b.Revision != 3 || len(b.Roles) != 1 {
			t.Fatalf("restored snapshot = %+v", b)
		}
		r.ready = map[string]bool{"b": true}
		r.heartbeat(1)
		if !rp2.canAccept(t, "b") {
			t.Fatal("not ready after restore")
		}
	})
}
