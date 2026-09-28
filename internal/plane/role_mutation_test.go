package plane

import (
	"bytes"
	"context"
	"errors"
	"strconv"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/wedevwork/callsheet/internal/contract"
)

// leadTime is how long after the node's heartbeat the hooked mutations
// start, so a lease expiry can fall inside a live mutation.
const leadTime = 10 * time.Second

// TestRoleMutationContract is UT FP-8; its races subtest (with
// races/snapshot-in-flight) is delegated from tests/function
// (TestRoleMutation/races). Do not rename or skip its subtests.
func TestRoleMutationContract(t *testing.T) {
	t.Parallel()
	t.Run("races", func(t *testing.T) {
		t.Parallel()
		t.Run("snapshot-in-flight", func(t *testing.T) {
			t.Parallel()
			// A mutation arriving while a background snapshot holds the
			// control slot waits (no busy, no frame), takes the freed slot
			// before any coalesced replacement within its unchanged deadline,
			// and succeeds durably. No sleeps, polling or retries.
			rp := startRolePlane(t, idA)
			p := rp.online(t, idA)
			rp.add(t, p, "p2", "p3", roleCfg("a", "coder", idA))
			res := rp.addAsync(bg, roleCfg("b", "coder", idA))
			p.validateOK("p4")
			if _, err := res.wait(t); err != nil {
				t.Fatal(err)
			}
			// The snapshot of b's publication is written; hold its ack.
			rp.log.await(t, "replace-sent "+idA+" p5 2")
			snap := p.readReplace("p5")
			res = rp.addAsync(bg, roleCfg("c", "coder", idA))
			rp.log.await(t, "validate-queued "+idA)
			if !res.pending() {
				v, err := res.wait(t)
				t.Fatalf("the mutation returned while the snapshot was in flight: %+v %v", v, err)
			}
			if rp.log.seenPrefix("validate-writing " + idA + " p6") {
				t.Fatal("a validation frame was sent while the slot was busy")
			}
			// Release the ack within the deadline (the clock never moved).
			p.rev, p.roles = snap.Revision, snap.Roles
			p.send(contract.ProtocolVersion, contract.FrameRolesReplaceAck, "p5", contract.RolesReplaceAckBody{Revision: snap.Revision})
			if c := p.expectValidate("p6"); c.ID != "c" {
				t.Fatalf("the freed slot went to %+v", c)
			}
			p.reply("p6", nil)
			v, err := res.wait(t)
			if err != nil || v.ID != "c" || v.RegistrationOrder != 3 {
				t.Fatalf("add = %+v %v", v, err)
			}
			if b := p.ackReplace("p7"); b.Revision != 3 || len(b.Roles) != 3 {
				t.Fatalf("replacement after the validation = %+v", b)
			}
			doc, err := layout{root: rp.root}.loadRoles(roleLookup)
			if err != nil || doc.revision != 3 || len(doc.roles) != 3 {
				t.Fatalf("durable registry = %+v %v", doc, err)
			}
		})
		t.Run("concurrent-busy", func(t *testing.T) {
			t.Parallel()
			// While one mutation holds the gate, any other add, set or rm is
			// unavailable/busy at once, never queued or retried; a duplicate
			// add conflicts once the first is published, even an identical
			// retry.
			rp := startRolePlaneDoc(t, fast(testDeps(t)), docOf(1, 2, record(roleCfg("x", "coder", idA), 1)), idA)
			p := rp.online(t, idA)
			res := rp.addAsync(bg, roleCfg("a", "coder", idA))
			p.expectValidate("p2")
			_, err := rp.cl.AddRole(bg, roleCfg("a", "coder", idA))
			wantReason(t, err, contract.CodeUnavailable, contract.ReasonBusy)
			name := "other"
			_, err = rp.cl.SetRole(bg, "x", contract.RolePatch{Name: &name})
			wantReason(t, err, contract.CodeUnavailable, contract.ReasonBusy)
			err = rmRole(rp.cl, bg, "x", true)
			wantReason(t, err, contract.CodeUnavailable, contract.ReasonBusy)
			p.reply("p2", nil)
			if _, err := res.wait(t); err != nil {
				t.Fatal(err)
			}
			p.ackReplace("p3")
			_, err = rp.cl.AddRole(bg, roleCfg("a", "coder", idA))
			wantCode(t, err, contract.CodeConflict, "already exists")
			// Nothing queued ran later: x is unchanged and present.
			if v := rp.view(t, "x"); v.Name != "coder" {
				t.Fatalf("a busy mutation ran later: %+v", v)
			}
			p.heartbeat(2)
		})
		t.Run("sequential", func(t *testing.T) {
			t.Parallel()
			// A mutation issued right after the previous one's response never
			// finds the gate held: the gate is released before responding.
			var recs []contract.RoleRecord
			for i, id := range roleIDs(10) {
				recs = append(recs, record(roleCfg(id, "coder", idA), i+1))
			}
			rp := startRolePlaneDoc(t, fast(testDeps(t)), docOf(1, 11, recs...), idA)
			for _, id := range roleIDs(10) {
				if err := rmRole(rp.cl, bg, id, false); err != nil {
					t.Fatalf("sequential rm %s: %v", id, err)
				}
			}
		})
		t.Run("disconnect-during-validation", func(t *testing.T) {
			t.Parallel()
			rp := startRolePlane(t, idA)
			p := rp.online(t, idA)
			res := rp.addAsync(bg, roleCfg("a", "coder", idA))
			p.expectValidate("p2")
			p.c.CloseNow()
			_, err := res.wait(t)
			wantReason(t, err, contract.CodeUnavailable, contract.ReasonNodeDisconnected)
			if registryBytes(t, rp.root) != nil {
				t.Fatal("published after the node disconnected")
			}
			// The gate was released: the next mutation proceeds.
			rp.log.await(t, "detached "+idA)
			q := rp.online(t, idA)
			rp.add(t, q, "p2", "p3", roleCfg("a", "coder", idA))
		})
		for _, c := range []struct {
			name   string
			stage  string
			act    func(t *testing.T, rp *rolePlane, p *peer, call hookCall)
			reason string
			code   contract.Code
		}{
			{"detach-before-publication", "validated", func(t *testing.T, rp *rolePlane, p *peer, _ hookCall) {
				p.c.CloseNow()
				rp.log.await(t, "detached "+idA)
			}, contract.ReasonNodeDisconnected, contract.CodeUnavailable},
			{"replaced-stream", "prepared", func(t *testing.T, rp *rolePlane, p *peer, _ hookCall) {
				p.c.CloseNow()
				rp.log.await(t, "detached "+idA)
				rp.online(t, idA) // a new generation: validation was on the old one
			}, contract.ReasonNodeDisconnected, contract.CodeUnavailable},
			{"lease-expiry", "validated", func(t *testing.T, rp *rolePlane, p *peer, _ hookCall) {
				// The heartbeat was at T0 and the mutation began at T0+10s:
				// at T0+15s the lease has expired while the mutation (until
				// T0+16s) is still live.
				rp.clk.Advance(leaseDuration - leadTime)
			}, contract.ReasonNodeOffline, contract.CodeUnavailable},
			{"precommit-deadline", "prepared", func(t *testing.T, rp *rolePlane, p *peer, _ hookCall) {
				rp.clk.Advance(mutationTimeout)
			}, contract.ReasonValidationTimeout, contract.CodeUnavailable},
		} {
			t.Run(c.name, func(t *testing.T) {
				t.Parallel()
				rp := startRolePlane(t, idA)
				p := rp.online(t, idA)
				rp.log.await(t, "ack "+idA+" b1")
				rp.clk.Advance(leadTime)
				rp.hooks.pause(c.stage)
				res := rp.addAsync(bg, roleCfg("a", "coder", idA))
				p.validateOK("p2")
				call := rp.hooks.wait(t, c.stage)
				c.act(t, rp, p, call)
				close(call.release)
				_, err := res.wait(t)
				wantReason(t, err, c.code, c.reason)
				if registryBytes(t, rp.root) != nil || len(roleTemps(t, rp.root)) != 0 {
					t.Fatalf("published or left a temporary: %v", roleTemps(t, rp.root))
				}
				if _, err := rp.cl.ShowRole(bg, "a"); !contract.IsCode(err, contract.CodeNotFound) {
					t.Fatalf("visible: %v", err)
				}
			})
		}
		t.Run("precommit-cancellation", func(t *testing.T) {
			t.Parallel()
			// The caller's cancellation after the temporary was prepared, and
			// before the authorization instant, discards it.
			rp := startRolePlane(t, idA)
			p := rp.online(t, idA)
			rp.hooks.pause("prepared")
			ctx, cancel := context.WithCancel(bg)
			res := rp.addAsync(ctx, roleCfg("a", "coder", idA))
			p.validateOK("p2")
			call := rp.hooks.wait(t, "prepared")
			cancel()
			select {
			case <-call.ctx.Done():
			case <-time.After(testWait):
				t.Fatal("the server did not observe the cancellation")
			}
			close(call.release)
			if _, err := res.wait(t); !errors.Is(err, context.Canceled) {
				t.Fatalf("canceled add = %v", err)
			}
			// The client returned at its cancellation; the handler's own
			// end is the gate's release.
			rp.log.await(t, "gate-released")
			if registryBytes(t, rp.root) != nil || len(roleTemps(t, rp.root)) != 0 {
				t.Fatalf("published or left a temporary: %v", roleTemps(t, rp.root))
			}
		})
	})
	t.Run("waiting-cancel", func(t *testing.T) {
		t.Parallel()
		// A validation canceled while waiting behind a snapshot publishes
		// nothing and releases the gate for the next mutation.
		rp := startRolePlane(t, idA)
		p := rp.dial(t)
		p.helloOK(idA)
		snap := p.readReplace("p1")
		p.heartbeat(1)
		ctx, cancel := context.WithCancel(bg)
		res := rp.addAsync(ctx, roleCfg("a", "coder", idA))
		rp.log.await(t, "validate-queued "+idA)
		cancel()
		if _, err := res.wait(t); !errors.Is(err, context.Canceled) {
			t.Fatalf("canceled = %v", err)
		}
		rp.log.await(t, "validate-withdrawn "+idA)
		// The client stopped waiting at cancel; the next mutation may start
		// only once the canceled one released the gate (else: busy).
		rp.log.await(t, "gate-released")
		p.send(contract.ProtocolVersion, contract.FrameRolesReplaceAck, "p1", contract.RolesReplaceAckBody{Revision: snap.Revision})
		rp.add(t, p, "p2", "p3", roleCfg("a", "coder", idA))
	})
	t.Run("noop-set", func(t *testing.T) {
		t.Parallel()
		// A set producing an identical configuration still validates on the
		// worker, but writes nothing, keeps the revision and keeps an
		// otherwise current observation.
		rp := startRolePlaneDoc(t, fast(testDeps(t)), docOf(1, 2, record(roleCfg("a", "coder", idA), 1)), idA)
		p := rp.online(t, idA)
		p.ready = map[string]bool{"a": true}
		p.heartbeat(2)
		before := registryBytes(t, rp.root)
		model := "example model"
		res := rp.setAsync(bg, "a", contract.RolePatch{Model: &model})
		if c := p.expectValidate("p2"); c.Model != model {
			t.Fatalf("candidate %+v", c)
		}
		p.reply("p2", nil)
		v, err := res.wait(t)
		if err != nil || !v.CanAccept || v.RegistrationOrder != 1 {
			t.Fatalf("no-op set = %+v %v", v, err)
		}
		if !bytes.Equal(before, registryBytes(t, rp.root)) || len(roleTemps(t, rp.root)) != 0 {
			t.Fatal("a no-op set wrote the registry")
		}
		p.heartbeat(3) // no snapshot was queued before this ack
		if !rp.canAccept(t, "a") || rp.log.seenPrefix("replace-sent "+idA+" p3") {
			t.Fatal("a no-op set invalidated readiness or distributed")
		}
		// A failed worker check of a no-op set changes nothing either.
		res = rp.setAsync(bg, "a", contract.RolePatch{Model: &model})
		p.expectValidate("p3")
		p.reply("p3", contract.RoleError(contract.CodeInvalidArgument, "", "", "runbook", contract.ReasonManualUnreadable, "gone"))
		_, err = res.wait(t)
		wantReason(t, err, contract.CodeInvalidArgument, contract.ReasonManualUnreadable)
	})
	t.Run("set", func(t *testing.T) {
		t.Parallel()
		// set merges with the committed record (never a client copy), keeps
		// ID, node and order, validates the whole merged configuration and
		// rejects bad patches before any worker contact.
		rp := startRolePlaneDoc(t, fast(testDeps(t)), docOf(2, 3, record(roleCfg("a", "coder", idA), 1), record(roleCfg("b", "coder", idA), 2)), idA)
		p := rp.online(t, idA)
		high, conc, zero := "high", 7, time.Duration(0)
		res := rp.setAsync(bg, "a", contract.RolePatch{Effort: &high, Concurrency: &conc, Timeout: &zero})
		c := p.validateOK("p2")
		if c.Effort != "high" || c.Concurrency != 7 || c.Timeout != 0 || c.Name != "coder" || c.Model != "example model" {
			t.Fatalf("merged candidate %+v", c)
		}
		v, err := res.wait(t)
		if err != nil || v.RegistrationOrder != 1 || v.Node != idA || v.Timeout != 0 || v.Concurrency != 7 {
			t.Fatalf("set = %+v %v", v, err)
		}
		p.ackReplace("p3")
		list, _ := rp.cl.ListRoles(bg)
		if len(list) != 2 || list[0].ID != "a" || list[1].ID != "b" {
			t.Fatalf("order after set = %+v", list)
		}
		bad, fx := "max", "codex"
		for name, c := range map[string]struct {
			id    string
			patch contract.RolePatch
			code  contract.Code
			want  string
		}{
			"effort":  {"a", contract.RolePatch{Effort: &bad}, contract.CodeInvalidArgument, "not allowed"},
			"adapter": {"a", contract.RolePatch{Adapter: &fx}, contract.CodeInvalidArgument, "unknown adapter"},
			"unknown": {"zz", contract.RolePatch{Effort: &high}, contract.CodeNotFound, "does not exist"},
		} {
			_, err := rp.cl.SetRole(bg, c.id, c.patch)
			if contract.CodeOf(err) != c.code || !bytes.Contains([]byte(err.Error()), []byte(c.want)) {
				t.Fatalf("%s: %v", name, err)
			}
		}
		p.heartbeat(2)
	})
	t.Run("offline", func(t *testing.T) {
		t.Parallel()
		// add and set need an online node with a live stream; rm works
		// offline, force or not, identically and without any worker
		// message; an unknown ID is not_found for both.
		rp := startRolePlaneDoc(t, fast(testDeps(t)), docOf(2, 3, record(roleCfg("a", "coder", idA), 1), record(roleCfg("b", "coder", idA), 2)), idA, idB)
		p := rp.online(t, idA)
		_, err := rp.cl.AddRole(bg, roleCfg("c", "coder", idB))
		wantReason(t, err, contract.CodeUnavailable, contract.ReasonNodeOffline)
		_, err = rp.cl.AddRole(bg, roleCfg("c", "coder", "n_ffffffffffffffffffffffffffffffff"))
		wantCode(t, err, contract.CodeNotFound, "not enrolled")
		p.c.Close(websocket.StatusNormalClosure, "")
		rp.log.await(t, "detached "+idA)
		name := "renamed"
		_, err = rp.cl.SetRole(bg, "a", contract.RolePatch{Name: &name})
		wantReason(t, err, contract.CodeUnavailable, contract.ReasonNodeDisconnected)
		rp.clk.Advance(leaseDuration)
		_, err = rp.cl.SetRole(bg, "a", contract.RolePatch{Name: &name})
		wantReason(t, err, contract.CodeUnavailable, contract.ReasonNodeOffline)
		for i, force := range []bool{false, true} {
			id := []string{"a", "b"}[i]
			if err := rmRole(rp.cl, bg, id, force); err != nil {
				t.Fatalf("rm %s force=%v: %v", id, force, err)
			}
			if err := rmRole(rp.cl, bg, id, force); !contract.IsCode(err, contract.CodeNotFound) {
				t.Fatalf("second rm %s = %v", id, err)
			}
		}
		if list, _ := rp.cl.ListRoles(bg); len(list) != 0 {
			t.Fatalf("roles left: %+v", list)
		}
		if !bytes.Contains(registryBytes(t, rp.root), []byte(`"roles": []`)) {
			t.Fatal("registry not emptied")
		}
	})
	t.Run("force", func(t *testing.T) {
		t.Parallel()
		// Online, rm and rm --force behave identically: the worker only
		// receives the replacement snapshot, never a cancellation or task
		// message.
		rp := startRolePlaneDoc(t, fast(testDeps(t)), docOf(2, 3, record(roleCfg("a", "coder", idA), 1), record(roleCfg("b", "coder", idA), 2)), idA)
		p := rp.online(t, idA)
		for i, force := range []bool{false, true} {
			id := []string{"a", "b"}[i]
			if err := rmRole(rp.cl, bg, id, force); err != nil {
				t.Fatal(err)
			}
			f := p.recv()
			if f.Type != contract.FrameRolesReplace || f.RequestID != "p"+strconv.Itoa(2+i) {
				t.Fatalf("rm force=%v sent %s %s", force, f.Type, f.RequestID)
			}
			b, _ := contract.DecodeRolesReplace(f.Body, roleLookup)
			p.rev, p.roles = b.Revision, b.Roles
			p.send(contract.ProtocolVersion, contract.FrameRolesReplaceAck, f.RequestID, contract.RolesReplaceAckBody{Revision: b.Revision})
		}
		p.heartbeat(2)
		if !bytes.Contains([]byte(rp.logs.String()), []byte(`"msg":"role removed","component":"plane","role_id":"b"`)) {
			t.Fatalf("removal not logged: %s", rp.logs.String())
		}
	})
	t.Run("capacity", func(t *testing.T) {
		t.Parallel()
		// 100 roles is the limit: the 101st add conflicts naming it before
		// any worker contact; set and rm still work.
		var recs []contract.RoleRecord
		for i, id := range roleIDs(contract.MaxRoles) {
			recs = append(recs, record(roleCfg(id, "coder", idA), i+1))
		}
		rp := startRolePlaneDoc(t, fast(testDeps(t)), docOf(100, 101, recs...), idA)
		_, err := rp.cl.AddRole(bg, roleCfg("extra", "coder", idA))
		wantCode(t, err, contract.CodeConflict, "maximum of 100 roles")
		if err := rmRole(rp.cl, bg, "role-100", false); err != nil {
			t.Fatal(err)
		}
		// Below the limit again: the add now reaches the node check.
		_, err = rp.cl.AddRole(bg, roleCfg("extra", "coder", idA))
		wantReason(t, err, contract.CodeUnavailable, contract.ReasonNodeOffline)
		if _, err := rp.cl.ShowRole(bg, "role-100"); !contract.IsCode(err, contract.CodeNotFound) {
			t.Fatalf("removed role = %v", err)
		}
		if v := rp.view(t, "role-99"); v.RegistrationOrder != 99 {
			t.Fatalf("role-99 = %+v", v)
		}
	})
}
