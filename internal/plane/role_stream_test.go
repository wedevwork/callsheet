package plane

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/wedevwork/callsheet/internal/contract"
	"github.com/wedevwork/callsheet/internal/testkit"
)

// frames reads n plane messages in arrival order.
func (p *peer) frames(n int) []contract.NodeFrame {
	p.t.Helper()
	out := make([]contract.NodeFrame, 0, n)
	for range n {
		out = append(out, p.recv())
	}
	return out
}

// timers is an AwaitWaiter condition: at least n one-shot timers of d.
func timers(d time.Duration, n int) func([]testkit.Waiter) bool {
	return func(ws []testkit.Waiter) bool {
		c := 0
		for _, w := range ws {
			if !w.Ticker && w.Duration == d {
				c++
			}
		}
		return c >= n
	}
}

// has reports whether fs holds typ with rid.
func has(fs []contract.NodeFrame, typ, rid string) bool {
	for _, f := range fs {
		if f.Type == typ && f.RequestID == rid {
			return true
		}
	}
	return false
}

// TestRoleStreamContract is the plane side of UT FP-4 and is delegated
// from tests/function (TestRoleProtocol duplex and bounds, together with
// the sidecar's contract of the same name). It serves the production TLS
// and stream handlers with a fake node clock and drives them with raw
// protocol-2 messages. Do not rename or skip its subtests.
func TestRoleStreamContract(t *testing.T) {
	t.Parallel()
	t.Run("duplex", func(t *testing.T) {
		t.Parallel()
		t.Run("arbitration", func(t *testing.T) {
			t.Parallel()
			// A snapshot is in flight and newer ones are already dirty when a
			// mutation's validation arrives: the validation waits (no busy,
			// no frame), takes the freed slot before any dirty snapshot, and
			// distribution then resumes with the newest desired revision only
			// (coalesced: intermediate revisions are never sent).
			x, y, w := roleCfg("x", "coder", idA), roleCfg("y", "coder", idA), roleCfg("w", "coder", idA)
			rp := startRolePlaneDoc(t, fast(testDeps(t)), docOf(3, 4, record(x, 1), record(y, 2), record(w, 3)), idA)
			p := rp.dial(t)
			p.hello(idA)
			p.expect(contract.FrameHelloOK, "h1")
			if b := p.ackReplace("p1"); b.Revision != 3 || len(b.Roles) != 3 || b.Roles[0].ID != "x" || b.Roles[2].ID != "w" {
				t.Fatalf("initial snapshot = %+v", b)
			}
			p.heartbeat(1)
			if err := rp.cl.RemoveRole(bg, "x", false); err != nil {
				t.Fatal(err)
			}
			if b := p.readReplace("p2"); b.Revision != 4 || len(b.Roles) != 2 {
				t.Fatalf("snapshot after rm = %+v", b)
			}
			rp.log.await(t, "replace-sent "+idA+" p2 4")
			for _, id := range []string{"y", "w"} {
				if err := rp.cl.RemoveRole(bg, id, id == "w"); err != nil {
					t.Fatal(err)
				}
			}
			res := rp.addAsync(bg, roleCfg("z", "coder", idA))
			rp.log.await(t, "validate-queued "+idA)
			if !res.pending() || rp.log.seenPrefix("validate-writing") {
				t.Fatal("the validation did not wait behind the snapshot")
			}
			// Heartbeats still cross the busy control slot, at the last
			// acknowledged revision.
			p.heartbeat(2)
			p.send(contract.ProtocolVersion, contract.FrameRolesReplaceAck, "p2", contract.RolesReplaceAckBody{Revision: 4})
			if c := p.validateOK("p3"); c.ID != "z" {
				t.Fatalf("validated %+v", c)
			}
			if v, err := res.wait(t); err != nil || v.ID != "z" || v.RegistrationOrder != 4 {
				t.Fatalf("add = %+v %v", v, err)
			}
			// Revision 6 (y and w removed) or, if the add's publication came
			// first, directly 7; revision 5 is never sent.
			b := p.ackReplace("p4")
			if b.Revision == 6 {
				if len(b.Roles) != 0 {
					t.Fatalf("revision 6 = %+v", b)
				}
				b = p.ackReplace("p5")
			}
			if b.Revision != 7 || len(b.Roles) != 1 || b.Roles[0].ID != "z" {
				t.Fatalf("newest snapshot = %+v", b)
			}
			if rp.log.seenPrefix("replace-sent " + idA + " p4 5") {
				t.Fatalf("an intermediate revision was sent: %v", rp.log.all())
			}
			p.heartbeat(3)
		})
		t.Run("crossing", func(t *testing.T) {
			t.Parallel()
			// A heartbeat and a plane request are outstanding together, in
			// both orders; neither waits for the other.
			rp := startRolePlane(t, idA)
			p := rp.online(t, idA)
			for i, order := range []string{"heartbeat-first", "reply-first"} {
				id := []string{"a", "b"}[i]
				res := rp.addAsync(bg, roleCfg(id, "coder", idA))
				vrid, srid := []string{"p2", "p4"}[i], []string{"p3", "p5"}[i]
				hb := "b" + string(rune('2'+i))
				p.expectValidate(vrid)
				if order == "heartbeat-first" {
					p.send(contract.ProtocolVersion, contract.FrameHeartbeat, hb, p.body())
					p.expect(contract.FrameHeartbeatAck, hb)
					p.reply(vrid, nil)
				} else {
					p.reply(vrid, nil)
					p.send(contract.ProtocolVersion, contract.FrameHeartbeat, hb, p.body())
				}
				if _, err := res.wait(t); err != nil {
					t.Fatalf("%s add: %v", order, err)
				}
				// The snapshot and (reply-first) the ack may come in either
				// order.
				n := 1
				if order == "reply-first" {
					n = 2
				}
				fs := p.frames(n)
				if !has(fs, contract.FrameRolesReplace, srid) || (n == 2 && !has(fs, contract.FrameHeartbeatAck, hb)) {
					t.Fatalf("%s frames %+v", order, fs)
				}
				// The heartbeat before this acknowledgement described the
				// previous revision; acknowledge the new one.
				for _, f := range fs {
					if f.Type == contract.FrameRolesReplace {
						b, _ := contract.DecodeRolesReplace(f.Body, roleLookup)
						p.rev, p.roles = b.Revision, b.Roles
						p.send(contract.ProtocolVersion, contract.FrameRolesReplaceAck, srid, contract.RolesReplaceAckBody{Revision: b.Revision})
					}
				}
			}
			p.heartbeat(4)
		})
		t.Run("sequence", func(t *testing.T) {
			t.Parallel()
			// Stale, future, duplicate and mismatched replies, and heartbeats
			// describing anything but the acknowledged snapshot, are protocol
			// errors that close the stream (1008) and publish nothing more.
			// The cases share one plane: each opens a new stream, whose
			// initial snapshot holds the roles earlier cases published. The
			// complete heartbeat-content matrix is "heartbeat-content" below
			// (the same validation, without a stream per case).
			rp := startRolePlane(t, idA)
			type step struct {
				name string
				// run drives the stream and returns the rejected request ID
				// and the add it started (nil if none).
				run func(p *peer, id string) (string, *call[contract.RoleView])
			}
			add := func(id string) *call[contract.RoleView] { return rp.addAsync(bg, roleCfg(id, "coder", idA)) }
			// published adds id and acknowledges the resulting snapshot.
			published := func(p *peer, id string) *call[contract.RoleView] {
				res := add(id)
				p.validateOK("p2")
				p.ackReplace("p3")
				return res
			}
			for i, c := range []step{
				{"future-reply", func(p *peer, id string) (string, *call[contract.RoleView]) {
					res := add(id)
					p.expectValidate("p2")
					p.reply("p9", nil)
					return "p9", res
				}},
				{"stale-reply", func(p *peer, id string) (string, *call[contract.RoleView]) {
					p.send(contract.ProtocolVersion, contract.FrameRolesReplaceAck, "p1", contract.RolesReplaceAckBody{Revision: p.rev})
					return "p1", nil
				}},
				{"wrong-type", func(p *peer, id string) (string, *call[contract.RoleView]) {
					res := add(id)
					p.expectValidate("p2")
					p.send(contract.ProtocolVersion, contract.FrameRolesReplaceAck, "p2", contract.RolesReplaceAckBody{})
					return "p2", res
				}},
				{"ack-revision", func(p *peer, id string) (string, *call[contract.RoleView]) {
					res := add(id)
					p.validateOK("p2")
					b := p.readReplace("p3")
					p.send(contract.ProtocolVersion, contract.FrameRolesReplaceAck, "p3", contract.RolesReplaceAckBody{Revision: b.Revision + 7})
					return "p3", res
				}},
				{"result-code", func(p *peer, id string) (string, *call[contract.RoleView]) {
					res := add(id)
					p.expectValidate("p2")
					p.reply("p2", contract.New(contract.CodeConflict, "no"))
					return "p2", res
				}},
				{"old-revision", func(p *peer, id string) (string, *call[contract.RoleView]) {
					old := p.body()
					res := published(p, id)
					p.send(contract.ProtocolVersion, contract.FrameHeartbeat, "b2", old)
					return "b2", res
				}},
				{"missing-role", func(p *peer, id string) (string, *call[contract.RoleView]) {
					res := published(p, id)
					hb := p.body()
					hb.Roles = hb.Roles[:len(hb.Roles)-1]
					p.send(contract.ProtocolVersion, contract.FrameHeartbeat, "b2", hb)
					return "b2", res
				}},
				{"inflight", func(p *peer, id string) (string, *call[contract.RoleView]) {
					hb := p.body()
					hb.Roles = append(hb.Roles, contract.RoleStatus{RoleID: "busy", Inflight: 1, Concurrency: 2})
					p.send(contract.ProtocolVersion, contract.FrameHeartbeat, "b2", hb)
					return "b2", nil
				}},
			} {
				t.Run(c.name, func(t *testing.T) {
					p := rp.online(t, idA)
					rid, res := c.run(p, "s"+strconv.Itoa(i))
					// The error names the offending request.
					for {
						f := p.recv()
						if f.Type == contract.FrameError {
							if f.RequestID != rid {
								t.Fatalf("error for %s, want %s", f.RequestID, rid)
							}
							break
						}
					}
					if st := p.closed(); st != websocket.StatusPolicyViolation {
						t.Fatalf("close = %v", st)
					}
					rp.log.await(t, "detached "+idA)
					// The gate is released before a mutation responds, so the
					// next case's add cannot find it held.
					if res != nil {
						res.wait(t)
					}
				})
			}
			// Only the adds whose validation succeeded were published.
			list, err := rp.cl.ListRoles(bg)
			if err != nil || len(list) != 3 {
				t.Fatalf("published = %+v %v", list, err)
			}
		})
		t.Run("heartbeat-content", func(t *testing.T) {
			t.Parallel()
			// Every way a heartbeat can differ from the acknowledged snapshot
			// is invalid_argument, renews nothing and changes no readiness;
			// the stream-level "sequence" cases show such an error closes the
			// stream.
			clk := testkit.NewFakeClock(t0)
			root := freshRoot(t)
			writeNodeRecord(t, root, idA+".json", encodeNodeRecord(idA, t0), 0o600)
			r, err := loadNodeRegistry(layout{root: root}, clk)
			if err != nil {
				t.Fatal(err)
			}
			a, b := record(roleCfg("a", "coder", idA), 1), record(roleCfg("b", "coder", idA), 2)
			gen, err := r.Attach(idA, "v", contract.ProtocolVersion, nil)
			if err != nil {
				t.Fatal(err)
			}
			r.ackSnapshot(idA, gen, roleSnap{rev: 4, roles: []contract.RoleRecord{a, b}})
			st := func(id string, conc int) contract.RoleStatus {
				return contract.RoleStatus{RoleID: id, Concurrency: conc, CanAccept: true}
			}
			good := contract.HeartbeatBody{RolesRevision: 4, Roles: []contract.RoleStatus{st("a", 2), st("b", 2)}}
			for name, hb := range map[string]contract.HeartbeatBody{
				"old revision":      {RolesRevision: 3, Roles: good.Roles},
				"newer revision":    {RolesRevision: 5, Roles: good.Roles},
				"missing role":      {RolesRevision: 4, Roles: good.Roles[:1]},
				"extra role":        {RolesRevision: 4, Roles: append(append([]contract.RoleStatus(nil), good.Roles...), st("c", 2))},
				"duplicate role":    {RolesRevision: 4, Roles: []contract.RoleStatus{st("a", 2), st("a", 2)}},
				"reordered":         {RolesRevision: 4, Roles: []contract.RoleStatus{st("b", 2), st("a", 2)}},
				"wrong concurrency": {RolesRevision: 4, Roles: []contract.RoleStatus{st("a", 3), st("b", 2)}},
				"empty":             {RolesRevision: 4},
			} {
				if err := r.heartbeat(idA, gen, hb); !contract.IsCode(err, contract.CodeInvalidArgument) {
					t.Fatalf("%s = %v", name, err)
				}
				if n := show(t, r, idA); n.Liveness != contract.LivenessOffline {
					t.Fatalf("%s renewed the lease", name)
				}
			}
			if err := r.heartbeat(idA, gen, good); err != nil {
				t.Fatal(err)
			}
			if n := show(t, r, idA); n.Liveness != contract.LivenessOnline {
				t.Fatalf("valid heartbeat = %+v", n)
			}
			// Iteration 05: a worker's inflight is an observation (it may
			// exceed a lowered concurrency); it never books or frees a plane
			// reservation (the plane's counts are TestTaskRoleIntegration's).
			busy := contract.HeartbeatBody{RolesRevision: 4, Roles: []contract.RoleStatus{{RoleID: "a", Inflight: 3, Concurrency: 2}, st("b", 2)}}
			if err := r.heartbeat(idA, gen, busy); err != nil {
				t.Fatalf("an inflight observation was refused: %v", err)
			}
			if n := show(t, r, idA); n.Liveness != contract.LivenessOnline {
				t.Fatalf("after an inflight observation = %+v", n)
			}
		})
		t.Run("result", func(t *testing.T) {
			t.Parallel()
			// A worker's failed check is a nonfatal result: the mutation
			// fails with the node's safe field and reason, nothing is
			// published and the stream carries on.
			rp := startRolePlane(t, idA)
			p := rp.online(t, idA)
			res := rp.addAsync(bg, roleCfg("a", "coder", idA))
			p.expectValidate("p2")
			e := contract.RoleError(contract.CodeInvalidArgument, "", "", "runbook", contract.ReasonManualUnreadable, "the runbook manual is not readable on this node: permission denied")
			e.Details["secret"] = "SENTINEL"
			p.reply("p2", e)
			_, err := res.wait(t)
			wantReason(t, err, contract.CodeInvalidArgument, contract.ReasonManualUnreadable)
			ce := err.(*contract.Error)
			if ce.Details["field"] != "runbook" || ce.Details["role_id"] != "a" || ce.Details["node_id"] != idA || strings.Contains(err.Error()+jsonOf(t, ce.Details), "SENTINEL") {
				t.Fatalf("mapped error %v %v", err, ce.Details)
			}
			res = rp.addAsync(bg, roleCfg("a", "coder", idA))
			p.expectValidate("p3")
			p.reply("p3", contract.RoleError(contract.CodeUnavailable, "", "", "", contract.ReasonBusy, "busy"))
			_, err = res.wait(t)
			wantReason(t, err, contract.CodeUnavailable, contract.ReasonBusy)
			if registryBytes(t, rp.root) != nil {
				t.Fatal("a failed validation published")
			}
			p.heartbeat(2)
		})
	})
	t.Run("bounds", func(t *testing.T) {
		t.Parallel()
		t.Run("in-flight-expiry", func(t *testing.T) {
			t.Parallel()
			// A sent validation unanswered at its deadline fails the
			// mutation and closes the stream: a late reply can never match
			// a later request.
			rp := startRolePlane(t, idA)
			p := rp.online(t, idA)
			res := rp.addAsync(bg, roleCfg("a", "coder", idA))
			p.expectValidate("p2")
			// Only after the write returned is the 4 s timer the request's.
			rp.log.await(t, "validate-sent "+idA+" p2")
			if err := rp.clk.AwaitWaiter(testWait, testkit.HasTimer(controlTimeout)); err != nil {
				t.Fatal(err)
			}
			rp.clk.Advance(controlTimeout)
			_, err := res.wait(t)
			wantReason(t, err, contract.CodeUnavailable, contract.ReasonValidationTimeout)
			if st := p.closed(); st != websocket.StatusPolicyViolation {
				t.Fatalf("close = %v", st)
			}
			rp.log.await(t, "request-expired "+idA+" p2")
			if registryBytes(t, rp.root) != nil {
				t.Fatal("an expired validation published")
			}
		})
		t.Run("waiting-expiry", func(t *testing.T) {
			t.Parallel()
			// DW6: a validation that expires while still waiting behind a
			// snapshot is withdrawn, never sent, publishes nothing and
			// leaves the stream attached; its deadline is the mutation's
			// (6 s from the handler's start), not reset when it queued.
			rp := startRolePlane(t, idA)
			first := rp.online(t, idA)
			rp.hooks.pause("submitting")
			res := rp.addAsync(bg, roleCfg("a", "coder", idA))
			call := rp.hooks.wait(t, "submitting") // handler start T0: deadline T0+6s
			first.c.Close(websocket.StatusNormalClosure, "")
			rp.log.await(t, "detached "+idA)
			rp.clk.Advance(2500 * time.Millisecond)
			p := rp.dial(t)
			p.hello(idA)
			p.expect(contract.FrameHelloOK, "h1")
			p.readReplace("p1") // reserved at T0+2.5s: its deadline is T0+6.5s
			rp.log.await(t, "replace-sent "+idA+" p1 0")
			p.heartbeat(1)
			close(call.release)
			rp.log.await(t, "validate-queued "+idA)
			// Submission at T0+2.5s: 4 s would end at T0+6.5s; the handler's
			// remaining time ends it at T0+6s.
			rp.clk.Advance(3500 * time.Millisecond)
			_, err := res.wait(t)
			wantReason(t, err, contract.CodeUnavailable, contract.ReasonValidationTimeout)
			rp.log.await(t, "validate-withdrawn "+idA)
			// Still attached: heartbeats are acknowledged; after the snapshot
			// ack nothing is sent for the withdrawn validation.
			p.heartbeat(2)
			p.send(contract.ProtocolVersion, contract.FrameRolesReplaceAck, "p1", contract.RolesReplaceAckBody{})
			p.heartbeat(3)
			detached := 0
			for _, e := range rp.log.all() {
				if e == "detached "+idA {
					detached++
				}
			}
			if detached != 1 || rp.log.seenPrefix("validate-writing") || rp.log.seenPrefix("request-expired") {
				t.Fatalf("events %v", rp.log.all())
			}
			if n := rp.show(t, idA); n.Liveness != contract.LivenessOnline {
				t.Fatalf("node = %+v", n)
			}
			if registryBytes(t, rp.root) != nil {
				t.Fatal("a withdrawn validation published")
			}
		})
		t.Run("waiting-cancel", func(t *testing.T) {
			t.Parallel()
			// The caller's cancellation while waiting also withdraws it and
			// leaves the stream attached.
			rp := startRolePlane(t, idA)
			p := rp.dial(t)
			p.hello(idA)
			p.expect(contract.FrameHelloOK, "h1")
			p.readReplace("p1")
			p.heartbeat(1)
			ctx, cancel := context.WithCancel(bg)
			res := rp.addAsync(ctx, roleCfg("a", "coder", idA))
			rp.log.await(t, "validate-queued "+idA)
			cancel()
			if _, err := res.wait(t); !errors.Is(err, context.Canceled) {
				t.Fatalf("canceled add = %v", err)
			}
			rp.log.await(t, "validate-withdrawn "+idA)
			p.heartbeat(2)
			p.send(contract.ProtocolVersion, contract.FrameRolesReplaceAck, "p1", contract.RolesReplaceAckBody{})
			p.heartbeat(3)
			if rp.log.seenPrefix("validate-writing") || registryBytes(t, rp.root) != nil {
				t.Fatalf("a canceled validation was sent or published: %v", rp.log.all())
			}
		})
		t.Run("cancel-before-ack", func(t *testing.T) {
			t.Parallel()
			// DW6 with the session winning the race: the caller cancels a
			// waiting validation and the snapshot ack is processed before
			// the canceled mutation withdraws it. The session must not send
			// it: no role_validate, the stream stays attached, and the
			// mutation fails unavailable/validation_timeout.
			d := fast(testDeps(t))
			rp := &rolePlane{log: newEventLog(), hooks: newHooks()}
			hold := make(chan struct{})
			d.streamEvents = func(e string) {
				rp.log.add(e)
				if e == "validate-withdrawing "+idA {
					<-hold // the canceled mutation resumes only when released
				}
			}
			d.roleHook = rp.hooks.fn
			rp.nodePlane = startNodePlaneSetup(t, d, nil, idA)
			p := rp.dial(t)
			p.hello(idA)
			p.expect(contract.FrameHelloOK, "h1")
			p.readReplace("p1") // the ack is withheld: the slot stays busy
			p.heartbeat(1)
			ctx, cancel := context.WithCancel(bg)
			res := rp.addAsync(ctx, roleCfg("a", "coder", idA))
			rp.log.await(t, "validate-queued "+idA)
			cancel()
			rp.log.await(t, "validate-withdrawing "+idA)
			p.send(contract.ProtocolVersion, contract.FrameRolesReplaceAck, "p1", contract.RolesReplaceAckBody{})
			rp.log.await(t, "replace-acked "+idA+" p1 0")
			// The session's next step after the ack decides the waiter.
			deadline := time.After(testWait)
			for !rp.log.seen("validate-canceled "+idA) && !rp.log.seenPrefix("validate-writing") {
				select {
				case <-rp.log.sig:
				case <-deadline:
					t.Fatalf("the session did not decide the canceled waiter: %v", rp.log.all())
				}
			}
			close(hold)
			if _, err := res.wait(t); !errors.Is(err, context.Canceled) {
				t.Fatalf("canceled add = %v", err)
			}
			rp.log.await(t, "validation-failed a unavailable "+contract.ReasonValidationTimeout)
			p.heartbeat(2)
			if rp.log.seenPrefix("validate-writing") || len(p.held) != 0 {
				t.Fatalf("a canceled validation was sent: held %v, events %v", p.held, rp.log.all())
			}
			if rp.log.seen("detached "+idA) || registryBytes(t, rp.root) != nil {
				t.Fatalf("the stream was detached or something was published: %v", rp.log.all())
			}
		})
		t.Run("blocked-write", func(t *testing.T) {
			t.Parallel()
			// A peer that stops reading while a validation is written: the
			// write is bounded by the validation's deadline and ends the
			// stream (transport loss, not a graceful close).
			rp := startRolePlane(t, idA)
			p := rp.online(t, idA)
			conn := rp.lis.all()[0]
			conn.blocking.Store(true)
			res := rp.addAsync(bg, roleCfg("a", "coder", idA))
			rp.log.await(t, "validate-writing "+idA+" p2")
			// Two 4 s timers: the handler's validation deadline and, once the
			// write has registered it, the write's own bound (shortened to
			// the same deadline). Advancing before the second exists would
			// race the write's bound computation, and advancing before the
			// write reaches the socket would test a close, not a blocked
			// write.
			if err := rp.clk.AwaitWaiter(testWait, timers(controlTimeout, 2)); err != nil {
				t.Fatal(err)
			}
			conn.awaitStalled(t)
			rp.clk.Advance(controlTimeout)
			_, err := res.wait(t)
			wantReason(t, err, contract.CodeUnavailable, contract.ReasonValidationTimeout)
			rp.log.await(t, "detached "+idA)
			p.closed()
			if registryBytes(t, rp.root) != nil {
				t.Fatal("published over a blocked stream")
			}
		})
		t.Run("sizes", func(t *testing.T) {
			t.Parallel()
			rp := startRolePlane(t, idA)
			// A heartbeat body at 32 KiB is accepted and one byte over it is
			// rejected (1008). The 2 MiB message read limit (1009) is the
			// stream's own and is proven by TestNodeStreamProtocol/oversized.
			p := rp.online(t, idA)
			body := `{"roles_revision":0,"roles":[]` + strings.Repeat(" ", contract.MaxHeartbeatBody-len(`{"roles_revision":0,"roles":[]}`)) + `}`
			p.sendRaw([]byte(`{"version":3,"type":"heartbeat","request_id":"b2","body":` + body + `}`))
			p.expect(contract.FrameHeartbeatAck, "b2")
			p.sendRaw([]byte(`{"version":3,"type":"heartbeat","request_id":"b3","body":` + body[:len(body)-1] + ` }` + `}`))
			p.expectError("b3", contract.CodeInvalidArgument)
			if st := p.closed(); st != websocket.StatusPolicyViolation {
				t.Fatalf("oversized heartbeat close = %v", st)
			}
		})
		t.Run("fencing", func(t *testing.T) {
			t.Parallel()
			// A validation whose stream ends is failed at once (never left
			// to a later session), publishes nothing, and the reconnected
			// session numbers its requests from p1 again, so an old reply
			// ID is a protocol error there.
			rp := startRolePlane(t, idA)
			p := rp.online(t, idA)
			res := rp.addAsync(bg, roleCfg("a", "coder", idA))
			p.expectValidate("p2")
			p.c.CloseNow()
			_, err := res.wait(t)
			wantReason(t, err, contract.CodeUnavailable, contract.ReasonNodeDisconnected)
			rp.log.await(t, "detached "+idA)
			p2 := rp.dial(t)
			p2.hello(idA)
			p2.expect(contract.FrameHelloOK, "h1")
			p2.readReplace("p1")
			p2.reply("p2", nil)
			p2.expectError("p2", contract.CodeInvalidArgument)
			if registryBytes(t, rp.root) != nil {
				t.Fatal("a fenced validation published")
			}
		})
		t.Run("inbox", func(t *testing.T) {
			t.Parallel()
			// The reader's handoff holds one message plus the one the
			// blocked reader completed; completion instants are stamped at
			// publication, and a canceled reader never blocks.
			clk := testkit.NewFakeClock(t0)
			b := newInbox(clk)
			if !b.put(bg, readResult{data: []byte("1")}) {
				t.Fatal("first put")
			}
			clk.Advance(time.Second)
			done := make(chan bool, 1)
			go func() { done <- b.put(bg, readResult{data: []byte("2")}) }()
			// The second put waits (full) but its instant is already fixed.
			deadline := time.After(testWait)
			for {
				b.mu.Lock()
				pend := b.pending != nil
				b.mu.Unlock()
				if pend {
					break
				}
				select {
				case <-deadline:
					t.Fatal("second put never published")
				case <-time.After(time.Millisecond):
				}
			}
			clk.Advance(time.Second)
			r, ok := b.take()
			if !ok || string(r.data) != "1" || !r.at.Equal(t0) {
				t.Fatalf("first = %+v", r)
			}
			if !<-done {
				t.Fatal("second put failed")
			}
			r, ok = b.take()
			if !ok || string(r.data) != "2" || !r.at.Equal(t0.Add(time.Second)) {
				t.Fatalf("second = %+v", r)
			}
			if _, ok := b.take(); ok {
				t.Fatal("empty inbox delivered")
			}
			b.put(bg, readResult{})
			ctx, cancel := context.WithCancel(bg)
			cancel()
			if b.put(ctx, readResult{}) {
				t.Fatal("a canceled put blocked or succeeded")
			}
		})
		t.Run("joined", func(t *testing.T) {
			t.Parallel()
			// Shutdown with a validation waiting and a snapshot in flight
			// fails the mutation and joins every handler.
			rp := startRolePlane(t, idA)
			p := rp.dial(t)
			p.hello(idA)
			p.expect(contract.FrameHelloOK, "h1")
			p.readReplace("p1")
			p.heartbeat(1)
			res := rp.addAsync(bg, roleCfg("a", "coder", idA))
			rp.log.await(t, "validate-queued "+idA)
			pc := p.closeAsync()
			if err := rp.stop(t); !errors.Is(err, context.Canceled) {
				t.Fatalf("run = %v", err)
			}
			if _, err := res.wait(t); err == nil {
				t.Fatal("a mutation succeeded across shutdown")
			}
			<-pc
			rp.released(t)
		})
	})
}
