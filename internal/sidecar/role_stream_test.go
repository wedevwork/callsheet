package sidecar

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/wedevwork/callsheet/internal/contract"
	"github.com/wedevwork/callsheet/internal/testkit"
)

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

// TestRoleStreamContract is the sidecar side of UT FP-4 and is delegated
// from tests/function (TestRoleProtocol duplex and bounds, together with
// the plane's contract of the same name). The session goroutine alone owns
// protocol state and writes; ordering is observed through explicit events
// (installed, ack written, heartbeat written). Do not rename or skip its
// subtests.
func TestRoleStreamContract(t *testing.T) {
	t.Run("duplex", func(t *testing.T) {
		t.Run("crossing", func(t *testing.T) {
			t.Parallel()
			// A heartbeat awaiting its ack and a plane request cross in both
			// orders; neither waits for the other.
			fp := startFakePlane(t)
			rr := startRoleRun(t, fp, true)
			c := fp.accept(t)
			c.setManual() // no roles: no readiness report
			c.connect()   // b1, then reconciliation (protocol 4)
			rr.ev.awaitMatch(t, evAck, func(ev event) bool { return ev.acks == 1 })
			rr.clk.Advance(heartbeatInterval)
			c.readHeartbeat(2) // b2 outstanding
			ins, run := manuals(t, rr.dir, "a", "x")
			c.validate("p1", roleConfig("a", ins, run))
			if e := c.result("p1"); e != nil {
				t.Fatal(e)
			}
			c.send(contract.ProtocolVersion, contract.FrameHeartbeatAck, "b2", nil)
			rr.ev.awaitMatch(t, evAck, func(ev event) bool { return ev.acks == 2 })
			// Now a validation is running when the next heartbeat falls due:
			// b2 was written at T0+5s, so b3 is due at T0+10s; the
			// validation arrives at T0+9s (budget until T0+11s).
			block := make(chan struct{})
			rr.script.set(nil, block)
			rr.clk.Advance(4 * time.Second)
			c.validate("p2", roleConfig("a", ins, run))
			rr.script.awaitProbe(t)
			rr.clk.Advance(time.Second)
			b := c.readHeartbeat(3)
			if b.RolesRevision != 0 || len(b.Roles) != 0 {
				t.Fatalf("b3 = %+v", b)
			}
			close(block)
			if e := c.result("p2"); e != nil {
				t.Fatalf("p2 = %v", e)
			}
			c.send(contract.ProtocolVersion, contract.FrameHeartbeatAck, "b3", nil)
			rr.ev.awaitMatch(t, evAck, func(ev event) bool { return ev.acks == 3 })
		})
		t.Run("install-ordering", func(t *testing.T) {
			t.Parallel()
			// Installation, acknowledgement and heartbeats share the session
			// goroutine: a heartbeat written before an installation carries
			// the old revision; every heartbeat after the ack write carries
			// the new one; IDs never skip. Since iteration 10a the first
			// heartbeat after the ack write is an immediate report. Every
			// probe is held (manual acknowledgements): no cycle result
			// changes readiness here.
			fp := startFakePlane(t)
			rr := startRoleRun(t, fp, true)
			rr.script.set(nil, make(chan struct{})) // canceled when Run ends
			c := fp.accept(t)
			c.setManual()
			c.helloOK(testID)
			rr.beat(c, 1, 0)
			c.reconcileEmpty()
			ins, run := manuals(t, rr.dir, "a", "x")
			a := roleConfig("a", ins, run)
			// (1) Replacement with nothing due: installed, acked, then
			// reported at once (not ready: no cycle result is required).
			c.replace("p1", 3, a)
			if ev := rr.ev.await(t, evInstalled); ev.rev != 3 || ev.id != "p1" {
				t.Fatalf("installed %+v", ev)
			}
			c.expectReplaceAck("p1", 3)
			rr.ev.awaitMatch(t, evAckWritten, func(ev event) bool { return ev.rev == 3 })
			if b := rr.beat(c, 2, 3); len(b.Roles) != 1 || b.Roles[0].RoleID != "a" || b.Roles[0].Concurrency != 2 || b.Roles[0].CanAccept {
				t.Fatalf("b2 = %+v", b)
			}
			// (2) A replacement arriving while a heartbeat awaits its ack:
			// the ack is written at once; the outstanding heartbeat was the
			// old revision; the next one, reported as soon as that exchange
			// ends, the new.
			rr.clk.Advance(heartbeatInterval)
			if b := c.readHeartbeat(3); b.RolesRevision != 3 {
				t.Fatalf("b3 = %+v", b)
			}
			c.replace("p2", 4, a)
			c.expectReplaceAck("p2", 4)
			c.send(contract.ProtocolVersion, contract.FrameHeartbeatAck, "b3", nil)
			rr.ev.awaitMatch(t, evAck, func(ev event) bool { return ev.acks == 3 })
			rr.beat(c, 4, 4)
			// (3) A heartbeat due at the same instant as a replacement: both
			// orders are legal, but the heartbeat written after the ack write
			// always reports the new revision, and the one before it (if
			// any) the old. The clock moves first: moving it while the ack
			// is being written could fire that write's own 5 s bound.
			rr.clk.Advance(heartbeatInterval)
			c.replace("p3", 5, a)
			var order []string
			for len(order) < 2 {
				f := c.recv()
				switch f.Type {
				case contract.FrameRolesReplaceAck:
					order = append(order, "ack")
				case contract.FrameHeartbeat:
					b, _ := contract.DecodeHeartbeat(f.Body)
					if f.RequestID != "b5" {
						t.Fatalf("heartbeat %s, want b5", f.RequestID)
					}
					if (len(order) == 0 && b.RolesRevision != 4) || (len(order) == 1 && b.RolesRevision != 5) {
						t.Fatalf("heartbeat after %v reports revision %d", order, b.RolesRevision)
					}
					order = append(order, "hb")
					c.send(contract.ProtocolVersion, contract.FrameHeartbeatAck, "b5", nil)
				default:
					t.Fatalf("unexpected %s", f.Type)
				}
			}
			// The clock moves only after the ack's write returned.
			rr.ev.awaitWritten(t, evAckWritten, "p3")
			if order[0] == "hb" {
				// The old-revision heartbeat was already being written; the
				// next heartbeat, reported at once after its exchange,
				// reports the new revision.
				rr.ev.awaitMatch(t, evAck, func(ev event) bool { return ev.acks == 5 })
				c.heartbeatAt(6, 5)
			}
		})
		t.Run("first-snapshot", func(t *testing.T) {
			t.Parallel()
			// A session's first replacement may be revision 0 only when
			// empty; a new session accepts any revision (a restored plane),
			// having cleared the installed state at session end.
			fp := startFakePlane(t)
			rr := startRoleRun(t, fp, true)
			c := fp.accept(t)
			c.connect()
			ins, run := manuals(t, rr.dir, "a", "x")
			c.replace("p1", 0)
			c.expectReplaceAck("p1", 0)
			c.replace("p2", 9, roleConfig("a", ins, run))
			c.expectReplaceAck("p2", 9)
			c.heartbeatAt(2, 9) // reported at once (iteration 10a)
			c.finish()
			rr.ev.await(t, evEnded)
			rr.advanceBackoff(t, jitterDelay(0))
			c2 := fp.accept(t)
			c2.helloOK(testID)
			if b := c2.heartbeatAt(1, 0); len(b.Roles) != 0 {
				t.Fatalf("installed state survived the session: %+v", b)
			}
			c2.reconcileEmpty()
			c2.replace("p1", 2, roleConfig("a", ins, run))
			c2.expectReplaceAck("p1", 2)
		})
	})
	t.Run("bounds", func(t *testing.T) {
		t.Run("sequence", func(t *testing.T) {
			t.Parallel()
			// Out-of-sequence, repeated, overlapping, stale and malformed
			// plane requests are protocol errors: an error message naming the
			// request and a 1008 close.
			var probeStarted <-chan string
			for _, cs := range []struct {
				name string
				run  func(c *fakeConn, a contract.RoleConfig)
				rid  string
				msg  string
			}{
				{"skip", func(c *fakeConn, a contract.RoleConfig) { c.validate("p2", a) }, "p2", "out of sequence"},
				{"repeat", func(c *fakeConn, a contract.RoleConfig) {
					a.Effort = "max" // answered without the probe
					c.validate("p1", a)
					c.result("p1")
					c.validate("p1", a)
				}, "p1", "out of sequence"},
				{"overlap", func(c *fakeConn, a contract.RoleConfig) {
					// p1's reply cannot be written while its probe is held.
					c.validate("p1", a)
					<-probeStarted
					c.replace("p2", 1, a)
				}, "p2", "before the reply"},
				{"not-newer", func(c *fakeConn, a contract.RoleConfig) {
					c.replace("p1", 4, a)
					c.expectReplaceAck("p1", 4)
					c.replace("p2", 4, a)
				}, "p2", "not newer"},
				{"zero-nonempty", func(c *fakeConn, a contract.RoleConfig) { c.replace("p1", 0, a) }, "p1", "revision 0 must be empty"},
				{"other-node", func(c *fakeConn, a contract.RoleConfig) {
					a.Node = "n_ffffffffffffffffffffffffffffffff"
					c.replace("p1", 1, a)
				}, "p1", "another node"},
				{"bad-snapshot", func(c *fakeConn, a contract.RoleConfig) {
					c.sendRaw([]byte(`{"version":6,"type":"roles_replace","request_id":"p1","body":{"revision":1,"roles":[{"id":"a"}]}}`))
				}, "p1", "required field"},
				{"bad-validate", func(c *fakeConn, a contract.RoleConfig) {
					c.sendRaw([]byte(`{"version":6,"type":"role_validate","request_id":"p1","body":{"role":{},"x":1}}`))
				}, "p1", "unknown field"},
				{"hello-again", func(c *fakeConn, a contract.RoleConfig) {
					c.send(contract.ProtocolVersion, contract.FrameHelloOK, "h1", contract.HelloOKBody{HeartbeatIntervalMS: 5000, LeaseMS: 15000})
				}, "h1", "unexpected hello_ok"},
			} {
				t.Run(cs.name, func(t *testing.T) {
					fp := startFakePlane(t)
					rr := startRoleRun(t, fp, true)
					rr.script.set(nil, make(chan struct{})) // canceled when Run ends
					probeStarted = rr.script.started
					c := fp.accept(t)
					c.connect()
					ins, run := manuals(t, rr.dir, "a", "x")
					cs.run(c, roleConfig("a", ins, run))
					err := rr.run.result(t)
					wantCode(t, err, contract.CodeInvalidArgument, cs.msg)
					for {
						f := c.recv()
						if f.Type == contract.FrameError {
							if f.RequestID != cs.rid {
								t.Fatalf("error names %s, want %s", f.RequestID, cs.rid)
							}
							break
						}
					}
					if st := c.closed(); st != websocket.StatusPolicyViolation {
						t.Fatalf("close = %v", st)
					}
				})
			}
		})
		t.Run("sizes", func(t *testing.T) {
			t.Parallel()
			// The receive path enforces each type's exact body limit
			// (whitespace counts): a heartbeat_ack body at its 8 KiB limit is
			// accepted, one byte over is a protocol error. Every type's
			// boundary, roles_replace's 1 MiB included, is TestFrameLimits'.
			fp := startFakePlane(t)
			rr := startRoleRun(t, fp, true)
			c := fp.accept(t)
			c.setManual() // no roles: no readiness report
			c.helloOK(testID)
			ack := func(rid string, n int) {
				c.sendRaw([]byte(`{"version":6,"type":"heartbeat_ack","request_id":"` + rid + `","body":{` + strings.Repeat(" ", n-2) + `}}`))
			}
			c.readHeartbeat(1)
			ack("b1", contract.MaxOtherBody)
			rr.ev.awaitMatch(t, evAck, func(ev event) bool { return ev.acks == 1 })
			c.reconcileEmpty()
			rr.clk.Advance(heartbeatInterval)
			c.readHeartbeat(2)
			ack("b2", contract.MaxOtherBody+1)
			wantCode(t, rr.run.result(t), contract.CodeInvalidArgument, "exceeds 8192")
		})
		t.Run("blocked-write", func(t *testing.T) {
			t.Parallel()
			// A plane that stops reading while a validation result is
			// written: the write is bounded (5 s) and ends the session as a
			// lost transport, retried with backoff.
			pp := newPipePlane()
			rr := startRoleRunWith(t, nil, true, func(d *deps) {
				d.newClient = func(string, clientTrust) (planeClient, error) { return pp, nil }
			})
			c := pp.accept(t)
			c.read(t)
			c.send(t, contract.FrameHelloOK, "h1", contract.HelloOKBody{HeartbeatIntervalMS: contract.HeartbeatIntervalMS, LeaseMS: contract.LeaseMS})
			c.read(t)
			c.send(t, contract.FrameHeartbeatAck, "b1", nil)
			rr.ev.await(t, evAck)
			c.reconcileEmpty(t)
			rr.ev.awaitWritten(t, evReplied, "r1")
			ins, run := manuals(t, rr.dir, "a", "x")
			c.send(t, contract.FrameRoleValidate, "p1", contract.RoleValidateBody{Role: roleConfig("a", ins, run)})
			rr.ev.await(t, evValidated)
			// The next heartbeat's due timer and the result write's bound are
			// both 5 s; advance only once the write registered its own.
			if err := rr.clk.AwaitWaiter(testWait, timers(stepTimeout, 2)); err != nil {
				t.Fatal(err)
			}
			rr.clk.Advance(stepTimeout)
			ev := rr.ev.await(t, evEnded)
			wantCode(t, ev.err, contract.CodeUnavailable, "blocked for 5s")
			rr.ev.await(t, evBackoff)
		})
		t.Run("fencing", func(t *testing.T) {
			t.Parallel()
			// A validation whose session ended keeps its slot until its work
			// returns; the next session sees busy, never the late result.
			fp := startFakePlane(t)
			rr := startRoleRun(t, fp, true)
			c := fp.accept(t)
			c.connect()
			ins, run := manuals(t, rr.dir, "a", "x")
			block := make(chan struct{})
			rr.script.set(nil, block)
			rr.script.setStuck(true)
			c.validate("p1", roleConfig("a", ins, run))
			rr.script.awaitProbe(t)
			c.finish()
			rr.ev.await(t, evEnded)
			rr.advanceBackoff(t, jitterDelay(0))
			c2 := fp.accept(t)
			c2.connect()
			c2.validate("p1", roleConfig("a", ins, run))
			wantResult(t, c2.result("p1"), contract.CodeUnavailable, "", contract.ReasonBusy)
			close(block)
			rr.ev.await(t, evValidated)
			rr.script.set(nil, nil)
			c2.validate("p2", roleConfig("a", ins, run))
			if e := c2.result("p2"); e != nil {
				t.Fatalf("p2 = %v", e)
			}
			// Nothing else arrived: the next message is the next heartbeat.
			rr.clk.Advance(heartbeatInterval)
			c2.heartbeatAt(2, 0)
		})
		t.Run("joined", func(t *testing.T) {
			t.Parallel()
			// Stopping Run cancels a running probe and joins its worker
			// before Run returns; no timer is left behind.
			fp := startFakePlane(t)
			rr := startRoleRun(t, fp, true)
			c := fp.accept(t)
			c.connect()
			ins, run := manuals(t, rr.dir, "a", "x")
			rr.script.set(nil, make(chan struct{}))
			c.validate("p1", roleConfig("a", ins, run))
			rr.script.awaitProbe(t)
			rr.run.cancel()
			if err := rr.run.result(t); !errors.Is(err, context.Canceled) {
				t.Fatalf("run = %v", err)
			}
			if ev := rr.ev.await(t, evValidated); !errors.Is(ev.err, context.Canceled) {
				t.Fatalf("worker ended with %v", ev.err)
			}
			if ws := rr.clk.Waiters(); len(ws) != 0 {
				t.Fatalf("timers left: %v", ws)
			}
		})
		t.Run("inbox", func(t *testing.T) {
			t.Parallel()
			clk := testkit.NewFakeClock(time.Date(2026, 9, 26, 12, 0, 0, 0, time.UTC))
			b := newInbox(clk)
			b.put(bg, readResult{data: []byte("1")})
			at := clk.Now()
			clk.Advance(time.Second)
			done := make(chan bool, 1)
			go func() { done <- b.put(bg, readResult{data: []byte("2")}) }()
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
			if r, ok := b.take(); !ok || string(r.data) != "1" || !r.at.Equal(at) {
				t.Fatalf("first = %+v", r)
			}
			if !<-done {
				t.Fatal("second put failed")
			}
			if r, ok := b.take(); !ok || string(r.data) != "2" || !r.at.Equal(at.Add(time.Second)) {
				t.Fatalf("second = %+v", r)
			}
			b.put(bg, readResult{})
			ctx, cancel := context.WithCancel(bg)
			cancel()
			if b.put(ctx, readResult{}) {
				t.Fatal("a canceled put blocked or succeeded")
			}
		})
	})
}
