package sidecar

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/wedevwork/callsheet/internal/contract"
	"github.com/wedevwork/callsheet/internal/testkit"
)

// Iteration 06a sidecar families: TestControlProtocol (FP-2),
// TestControlLease (FP-3), TestControlLaunch (FP-4), TestControlRestart
// (FP-5), TestControlPlaneRestart (FP-6), TestControlLate (FP-7) and
// BenchmarkControlReplay. Guardians, adapters, groups and the control FIFO
// are injected (zero OS children, asserted by every taskRun); the
// guardian's own entrypoint runs in process over real pipes
// (control_unix_test.go). Tests synchronize on the sidecar's events and
// never advance the fake clock before the responsible event.

const ctlNonce = "0f0f0f0f0f0f0f0f0f0f0f0f0f0f0f0f0f0f0f0f0f0f0f0f0f0f0f0f0f0f0f0f"

// ctlStarted is a previous Run's launch instant.
var ctlStarted = time.Date(2026, 9, 26, 11, 0, 0, 0, time.UTC)

// journalOf is st's journal in phase (a previous Run's durable state).
func journalOf(st contract.TaskStartBody, phase string) contract.ExecutionJournal {
	j := contract.ExecutionJournal{TaskID: st.TaskID, Execution: st.Execution, StartDigest: st.StartDigestHex(), Role: st.Role, Effective: st.Effective,
		TimeoutPolicy: contract.TimeoutPolicyLegacy, Phase: phase}
	if phase != contract.JournalPrepared {
		n, at := ctlNonce, ctlStarted
		j.OwnerNonce, j.StartedAt = &n, &at
	}
	return j
}

// ownerOf is st's guardian owner record.
func ownerOf(st contract.TaskStartBody, pgid int, phase string) *contract.OwnerRecord {
	return &contract.OwnerRecord{TaskID: st.TaskID, Execution: st.Execution, Nonce: ctlNonce, PGID: pgid, GuardianPID: pgid, Phase: phase}
}

// writeJournal writes a previous Run's task directory under root.
func writeJournal(t *testing.T, root string, j contract.ExecutionJournal, o *contract.OwnerRecord) {
	t.Helper()
	dir := filepath.Join(root, journalDir, j.TaskID)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	os.Chmod(filepath.Join(root, journalDir), 0o700)
	b, err := contract.EncodeExecutionJournal(j)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, executionName), b, 0o600); err != nil {
		t.Fatal(err)
	}
	if o != nil {
		ob, _ := contract.EncodeOwner(*o)
		if err := os.WriteFile(filepath.Join(dir, ownerName), ob, 0o600); err != nil {
			t.Fatal(err)
		}
	}
}

// completedJournal is st's completed outbox (exit code, output tail).
func completedJournal(st contract.TaskStartBody, exit int, tail string) (contract.ExecutionJournal, contract.TaskResultBody) {
	j := journalOf(st, contract.JournalCompleted)
	res := contract.TaskResultBody{TaskID: st.TaskID, Execution: st.Execution, Outcome: contract.OutcomeNatural, ExitCode: &exit, OutputBytes: len(tail)}.Sealed()
	j.Result, j.Log = &res, contract.TaskLog{Data: []byte(tail), SourceBytes: len(tail), ReceivedBytes: len(tail)}
	return j, res
}

// recoveryRun is a Run over a root holding previous journals, with the
// given groups and control FIFO sender (installed before Run starts, so
// its recovery sees them).
type recoveryRun struct {
	*taskRun
	g    *fakeGroups
	cmds *fakeCommands
	cfg  contract.RoleConfig
}

// startRecovery prepares root (enrollment, manuals for role a) and calls
// setup to write journals, then starts Run with g and cmds.
func startRecovery(t *testing.T, fp *fakePlane, root string, g *fakeGroups, cmds *fakeCommands, setup func(cfg contract.RoleConfig)) *recoveryRun {
	t.Helper()
	dir := t.TempDir()
	ins, run := manuals(t, dir, "a", "m")
	cfg := roleConfig("a", ins, run)
	if _, err := os.Stat(filepath.Join(root, identityName)); err != nil {
		writeState(t, root, testID, fp.url, fp.caPEM)
	}
	if setup != nil {
		setup(cfg)
	}
	tr := startTaskRun(t, fp, taskOpts{root: root, adjust: func(d *deps) {
		d.taskGroups = g
		d.taskCommand = cmds.send
	}})
	return &recoveryRun{taskRun: tr, g: g, cmds: cmds, cfg: cfg}
}

// statusKeys decodes a heartbeat body's per-role status keys (the
// cleanup diagnostic never reaches the wire).
func statusKeys(t *testing.T, raw []byte) []string {
	t.Helper()
	var hb struct {
		Roles []map[string]any `json:"roles"`
	}
	if err := json.Unmarshal(raw, &hb); err != nil {
		t.Fatal(err)
	}
	var keys []string
	for _, r := range hb.Roles {
		for k := range r {
			keys = append(keys, k)
		}
	}
	slices.Sort(keys)
	return keys
}

// TestControlProtocol is UT FP-2 on the sidecar, delegated from
// tests/function (TestControlReconnect) with the contract's and the
// plane's: attachment order (inventory before any plane request is
// accepted), bounded paging with heartbeat priority, the original
// execution identity across attachments, no start replay, receipt versus
// committed acknowledgement, forget, fencing and unknown dispositions. Do
// not rename or skip it.
func TestControlProtocol(t *testing.T) {
	t.Parallel()
	t.Run("order", func(t *testing.T) {
		t.Parallel()
		// Before the final reconcile acknowledgement the sidecar accepts
		// no roles_replace, role_validate, task_start or early reconcile:
		// each is a protocol error closing the attachment.
		for name, send := range map[string]func(c *fakeConn, cfg contract.RoleConfig){
			"replace":  func(c *fakeConn, cfg contract.RoleConfig) { c.replace("p1", 1, cfg) },
			"validate": func(c *fakeConn, cfg contract.RoleConfig) { c.validate("p1", cfg) },
			"start":    func(c *fakeConn, cfg contract.RoleConfig) { c.sendStart("p1", startBody(1, cfg, 1, 1, "early")) },
			"reconcile": func(c *fakeConn, cfg contract.RoleConfig) {
				c.send(contract.ProtocolVersion, contract.FrameTaskReconcile, "r1", contract.TaskReconcileBody{Final: true})
			},
		} {
			t.Run(name, func(t *testing.T) {
				t.Parallel()
				fp := startFakePlane(t)
				tr := startTaskRun(t, fp, taskOpts{})
				ins, run := manuals(t, tr.dir, "a", "m")
				c := fp.accept(t)
				c.helloOK(testID)
				c.heartbeatAt(1, 0)
				// The empty inventory: one final page 0 without entries.
				f := c.expect(contract.FrameTaskInventory, "i1")
				b, err := contract.DecodeTaskInventory(f.Body)
				if err != nil || b.Page != 0 || !b.Final || len(b.Entries) != 0 || b.RunID != tr.super(t).runID {
					t.Fatalf("inventory %s %v", f.Body, err)
				}
				if name != "reconcile" {
					c.send(contract.ProtocolVersion, contract.FrameTaskInventoryAck, "i1", contract.TaskInventoryAckBody{Page: 0, Received: true})
				}
				send(c, roleConfig("a", ins, run))
				for {
					f := c.recv()
					if f.Type == contract.FrameError {
						break
					}
				}
				if st := c.closed(); st != websocket.StatusPolicyViolation {
					t.Fatalf("close %v", st)
				}
				tr.noChild(t)
			})
		}
	})
	t.Run("paging", func(t *testing.T) {
		t.Parallel()
		// Seventy held executions page as 64 + 6 from a list frozen at the
		// attachment; a heartbeat due while a page is outstanding is sent
		// right after that page's acknowledgement, before the next page.
		fp := startFakePlane(t)
		root := newRoot(t)
		var sts []contract.TaskStartBody
		rr := startRecovery(t, fp, root, &fakeGroups{}, &fakeCommands{}, func(cfg contract.RoleConfig) {
			for i := 1; i <= 70; i++ {
				st := startBody(i, cfg, 1, 1, "held "+strconv.Itoa(i))
				j, _ := completedJournal(st, 0, "")
				writeJournal(t, root, j, nil)
				sts = append(sts, st)
			}
		})
		c := fp.accept(t)
		c.helloOK(testID)
		// b1 holds the one request slot until its late (in-bound) ack; i1
		// follows it; the next heartbeat falls due while i1 is outstanding
		// and goes first once the slot frees, before page i2.
		c.readHeartbeat(1)
		rr.awaitReply(t, 1)
		rr.clk.Advance(stepTimeout - time.Second)
		c.send(contract.ProtocolVersion, contract.FrameHeartbeatAck, "b1", nil)
		f := c.expect(contract.FrameTaskInventory, "i1")
		p0, err := contract.DecodeTaskInventory(f.Body)
		if err != nil || p0.Final || len(p0.Entries) != contract.MaxInventoryEntries {
			t.Fatalf("page 0 %v (%d entries)", err, len(p0.Entries))
		}
		for i, e := range p0.Entries {
			if e.TaskID != sts[i].TaskID || e.Phase != contract.PhaseResult || e.Execution != sts[i].Execution || e.StartDigest != sts[i].StartDigestHex() {
				t.Fatalf("entry %d %+v", i, e)
			}
		}
		rr.ev.awaitMatch(t, evOutputWritten, func(ev event) bool { return ev.id == "i1" })
		rr.clk.Advance(time.Second)
		c.send(contract.ProtocolVersion, contract.FrameTaskInventoryAck, "i1", contract.TaskInventoryAckBody{Page: 0, Received: true})
		c.heartbeatAt(2, 0)
		f = c.expect(contract.FrameTaskInventory, "i2")
		p1, err := contract.DecodeTaskInventory(f.Body)
		if err != nil || !p1.Final || p1.Page != 1 || len(p1.Entries) != 6 || p1.RunID != p0.RunID {
			t.Fatalf("page 1 %+v %v", p1, err)
		}
		c.send(contract.ProtocolVersion, contract.FrameTaskInventoryAck, "i2", contract.TaskInventoryAckBody{Page: 1, Received: true})
		// Seventy dispositions page as 64 + 6.
		all := append(p0.Entries, p1.Entries...)
		for i, rid := range []string{"r1", "r2"} {
			body := contract.TaskReconcileBody{Final: i == 1}
			for _, e := range all[i*64 : min(len(all), (i+1)*64)] {
				body.Entries = append(body.Entries, contract.TaskReconcileEntry{TaskID: e.TaskID, Execution: e.Execution, Action: contract.ActionForget})
			}
			c.send(contract.ProtocolVersion, contract.FrameTaskReconcile, rid, body)
			c.expect(contract.FrameTaskReconcileAck, rid)
		}
		for _, st := range sts {
			rr.ev.awaitMatch(t, evTaskForgotten, func(ev event) bool { return ev.id == st.TaskID })
		}
		if ids, _ := (layout{root: root}).scanJournals(); len(ids) != 0 {
			t.Fatalf("%d journals remain after forget", len(ids))
		}
	})
	t.Run("identity", func(t *testing.T) {
		t.Parallel()
		// The execution keeps its original token and digest on the next
		// attachment; continue reattaches it (no new guardian); a start
		// replaying it there is a protocol error, never a second launch.
		fp := startFakePlane(t)
		tr := startTaskRun(t, fp, taskOpts{})
		ins, run := manuals(t, tr.dir, "a", "m")
		cfg := roleConfig("a", ins, run)
		s := tr.connect(t, 1, 1, cfg)
		st, ch := s.run(t, 1, 0, "stable")
		ch.prompt(t)
		s.detach(t, false, st.TaskID)
		s, inv := tr.reconnect(t, 2, 2, map[string]string{st.TaskID: contract.ActionContinue}, cfg)
		if len(inv) != 1 || inv[0].Execution != st.Execution || inv[0].StartDigest != st.StartDigestHex() || inv[0].Phase != contract.PhaseRunning ||
			inv[0].StartedAt == nil {
			t.Fatalf("inventory %+v", inv)
		}
		replay := st
		replay.Execution.Attachment = 2
		s.c.sendStart(s.nextP(), replay)
		for {
			if f := s.c.recv(); f.Type == contract.FrameError {
				break
			}
		}
		s.c.closed()
		tr.ledger.mu.Lock()
		n := len(tr.ledger.guardians)
		tr.ledger.mu.Unlock()
		if n != 1 {
			t.Fatalf("%d guardians for one execution", n)
		}
		ch.exitCode(0)
		tr.settled(t, st.TaskID)
	})
	t.Run("commit", func(t *testing.T) {
		t.Parallel()
		// A receipt acknowledgement never deletes the outbox; a committed
		// one does (unlink and directory syncs), after which nothing is
		// held for the execution.
		fp := startFakePlane(t)
		tr := startTaskRun(t, fp, taskOpts{})
		ins, run := manuals(t, tr.dir, "a", "m")
		s := tr.connect(t, 1, 1, roleConfig("a", ins, run))
		st, ch := s.run(t, 1, 0, "commit")
		ch.exitCode(0)
		tr.settled(t, st.TaskID)
		r := s.resultAck(t, st, false)
		dir := filepath.Join(tr.root, journalDir, st.TaskID)
		if lj, err := (layout{root: tr.root}).loadJournal(st.TaskID, tr.lookup()); err != nil || lj.j.Result.Digest != r.Digest {
			t.Fatalf("receipt-only: journal %v", err)
		}
		tr.clk.Advance(resultRetry)
		if again := s.result(t, st); again.Digest != r.Digest {
			t.Fatal("the retry changed the digest")
		}
		tr.ev.awaitMatch(t, evTaskForgotten, func(ev event) bool { return ev.id == st.TaskID })
		if _, err := os.Stat(dir); !os.IsNotExist(err) {
			t.Fatalf("the committed outbox remains: %v", err)
		}
	})
	t.Run("fencing", func(t *testing.T) {
		t.Parallel()
		// Output is sent only on an attachment that reconciled the
		// execution: a reconciliation that does not name it (or names it
		// under another token, or an execution not held) binds nothing.
		fp := startFakePlane(t)
		tr := startTaskRun(t, fp, taskOpts{})
		ins, run := manuals(t, tr.dir, "a", "m")
		cfg := roleConfig("a", ins, run)
		s := tr.connect(t, 1, 1, cfg)
		st, ch := s.run(t, 1, 0, "fenced")
		ch.prompt(t)
		s.detach(t, false, st.TaskID)
		c := tr.fp.accept(t)
		c.helloOK(testID)
		c.heartbeatAt(1, 0)
		inv := c.inventory()
		wrong := inv[0]
		wrong.Execution.Attachment = 9
		unknown := inv[0]
		unknown.TaskID = taskID(99)
		c.reconcile([]contract.TaskInventoryEntry{wrong, unknown}, map[string]string{wrong.TaskID: contract.ActionContinue, unknown.TaskID: contract.ActionContinue})
		c.replace("p1", 2, cfg)
		c.expectReplaceAck("p1", 2)
		tr.ev.awaitMatch(t, evAckWritten, func(ev event) bool { return ev.rev == 2 })
		ch.out([]byte("unbound\n"))
		tr.clk.Advance(heartbeatInterval)
		c.heartbeatAt(2, 2)
		tr.logs.await(t, "unknown disposition", func(s string) bool {
			return strings.Contains(s, "reconciliation named an execution this worker does not hold")
		})
		if len(c.held) != 0 {
			t.Fatalf("an unbound execution's output was sent: %+v", c.held)
		}
		ch.exitCode(0)
		tr.settled(t, st.TaskID)
	})
}

// TestControlLease is UT FP-3 on the sidecar, delegated from
// tests/function (TestControlNodeLoss) with the plane's: after the plane
// reclaimed an expired node's reservation, the returning worker keeps its
// local slot and group ownership, stops the lost execution through its
// guardian, refuses every start node-wide until the group is proved gone
// (a removed and re-added role cannot bypass it) and never reroutes; a
// reconnect before expiry keeps the execution running. Do not rename or
// skip it.
func TestControlLease(t *testing.T) {
	t.Parallel()
	t.Run("local-slot", func(t *testing.T) {
		t.Parallel()
		for _, byAck := range []bool{false, true} {
			t.Run(map[bool]string{false: "closed", true: "unacknowledged"}[byAck], func(t *testing.T) {
				t.Parallel()
				sidecarRecoveryRemove(t, byAck)
			})
		}
	})
	t.Run("reconnect-before", func(t *testing.T) {
		t.Parallel()
		sidecarRemainingCapacity(t, false)
	})
	t.Run("stop-slot", func(t *testing.T) {
		t.Parallel()
		// A lost execution told to stop keeps its instance's slot while
		// its group is cleaned: a start of the same instance is refused
		// (local_full) though the plane's count is zero.
		fp := startFakePlane(t)
		tr := startTaskRun(t, fp, taskOpts{})
		ins, run := manuals(t, tr.dir, "a", "m")
		cfg := roleConfig("a", ins, run)
		cfg.Concurrency = 1
		s := tr.connect(t, 1, 1, cfg)
		st, ch := s.run(t, 1, 0, "lost")
		ch.prompt(t)
		s.detach(t, false, st.TaskID)
		release := tr.groups.hold()
		defer release()
		s, _ = tr.reconnect(t, 2, 2, map[string]string{st.TaskID: contract.ActionStopLost}, cfg)
		tr.ev.awaitMatch(t, evCycleDone, func(ev event) bool { return ev.rev == 2 })
		tr.clk.Advance(heartbeatInterval)
		if hb := s.beat(t, 2); hb.Roles[0].Inflight != 1 || hb.Roles[0].CanAccept {
			t.Fatalf("heartbeat while cleaning %+v", hb.Roles)
		}
		nb := startBody(2, cfg, 1, 2, "blocked")
		nb.RolesRevision = 2
		rid := s.nextP()
		s.c.sendStart(rid, nb)
		wantRefusal(t, s.c.startResult(rid), contract.ReasonLocalFull)
		release()
		tr.settled(t, st.TaskID)
		if r, _ := s.drain(t, st); r.Outcome != contract.OutcomeLost {
			t.Fatalf("stopped %+v", r)
		}
	})
}

// TestControlLaunch is UT FP-4 on the sidecar, delegated from
// tests/function (TestControlLaunchSafety): a fault at every durable
// launch barrier refuses definitely with nothing launched and nothing
// left; the crash state each barrier leaves is recovered safely; the
// guardian's identity (PID = PGID, distinct from the sidecar's group),
// its missing or failed start, the 10 s preparation boundary at minus one,
// exactly and plus one tick with DW6's explicit overrun close, and the
// guardian's private entrypoint (control_unix_test.go). Do not rename or
// skip it.
func TestControlLaunch(t *testing.T) {
	t.Parallel()
	t.Run("barriers", func(t *testing.T) {
		t.Parallel()
		launchBarriers(t)
	})
	t.Run("crash", func(t *testing.T) {
		t.Parallel()
		// The durable state a crash leaves at each barrier, recovered by
		// the next Run: never a second launch; a recorded group is
		// cleaned through its guardian before anything is accepted.
		for name, c := range map[string]struct {
			phase   string
			owner   string
			command bool
		}{
			"prepared":            {contract.JournalPrepared, "", false},
			"owner-armed":         {contract.JournalPrepared, contract.OwnerArmed, true},
			"authorized":          {contract.JournalRunning, contract.OwnerArmed, true},
			"released-unobserved": {contract.JournalRunning, contract.OwnerReleased, true},
		} {
			t.Run(name, func(t *testing.T) {
				t.Parallel()
				fp := startFakePlane(t)
				root := newRoot(t)
				g := &fakeGroups{alive: map[int]bool{7001: c.owner != ""}}
				cmds := &fakeCommands{}
				cmds.deliver = func(string) {
					g.mu.Lock()
					g.alive[7001] = false
					g.mu.Unlock()
				}
				var st contract.TaskStartBody
				rr := startRecovery(t, fp, root, g, cmds, func(cfg contract.RoleConfig) {
					st = startBody(1, cfg, 1, 1, "crashed")
					j := journalOf(st, c.phase)
					if c.phase == contract.JournalPrepared {
						j.OwnerNonce, j.StartedAt = nil, nil
					} else {
						j.StartedAt = nil
					}
					var o *contract.OwnerRecord
					if c.owner != "" {
						o = ownerOf(st, 7001, c.owner)
					}
					writeJournal(t, root, j, o)
				})
				s, inv := rr.reconnect(t, 1, 1, map[string]string{st.TaskID: contract.ActionSendResult}, rr.cfg)
				if len(inv) != 1 || inv[0].Phase != contract.PhaseLost || inv[0].StartedAt != nil {
					t.Fatalf("inventory %+v", inv)
				}
				r, _ := s.drain(t, st)
				if r.Outcome != contract.OutcomeLost || r.ExitCode != nil || r.Signal != nil {
					t.Fatalf("recovered %+v", r)
				}
				if sent := cmds.all(); (len(sent) == 1) != c.command || (c.command && !strings.Contains(sent[0], `"command":"stop"`)) {
					t.Fatalf("commands %v", sent)
				}
				rr.noChild(t)
				if len(g.signals()) != 0 {
					t.Fatal("a recorded group was signaled directly")
				}
			})
		}
	})
	t.Run("stale-owner", func(t *testing.T) {
		t.Parallel()
		// A stale owner record whose numeric PGID now names another live
		// group (PID reuse), with no guardian serving the FIFO or a
		// guardian that does not answer: never a signal to that raw PGID;
		// the cleanup is blocked and the node refuses every start.
		for name, cmdErr := range map[string]error{"pgid-reuse": errNoGuardian, "unresponsive": errors.New("write /x/control: i/o timeout")} {
			t.Run(name, func(t *testing.T) {
				t.Parallel()
				fp := startFakePlane(t)
				root := newRoot(t)
				g := &fakeGroups{alive: map[int]bool{7010: true}}
				cmds := &fakeCommands{err: cmdErr}
				var st contract.TaskStartBody
				rr := startRecovery(t, fp, root, g, cmds, func(cfg contract.RoleConfig) {
					st = startBody(1, cfg, 1, 1, "stale owner")
					writeJournal(t, root, journalOf(st, contract.JournalRunning), ownerOf(st, 7010, contract.OwnerReleased))
				})
				rr.ev.awaitMatch(t, evCleanupBlocked, func(ev event) bool { return ev.id == st.TaskID })
				if !rr.super(t).nodeBlocked() || len(g.signals()) != 0 || len(cmds.all()) != 1 {
					t.Fatalf("blocked %v, signals %v, commands %v", rr.super(t).nodeBlocked(), g.signals(), cmds.all())
				}
				g.mu.Lock()
				cleaned := len(g.cleaned)
				g.mu.Unlock()
				if cleaned != 0 {
					t.Fatal("an unauthenticated group was waited on as if stopped")
				}
			})
		}
	})
	t.Run("identity", func(t *testing.T) {
		t.Parallel()
		// A guardian whose ready names another group, or the sidecar's
		// own group, is abandoned before release: definite refusal, no
		// adapter, its group waited for.
		for name, mut := range map[string]func(tr *taskRun){
			"pgid":      func(tr *taskRun) { tr.ledger.badReady = "pgid" },
			"own-group": func(tr *taskRun) { tr.ledger.badReady = "own" },
		} {
			t.Run(name, func(t *testing.T) {
				t.Parallel()
				fp := startFakePlane(t)
				var tr *taskRun
				tr = startTaskRun(t, fp, taskOpts{adjust: func(d *deps) { d.ownGroup = func() int { return 50002 } }})
				mut(tr)
				ins, run := manuals(t, tr.dir, "a", "m")
				s := tr.connect(t, 1, 1, roleConfig("a", ins, run))
				st, r := s.start(t, 1, 0, name)
				wantRefusal(t, r, contract.ReasonStartFailed)
				tr.noChild(t)
				if _, err := os.Stat(filepath.Join(tr.root, journalDir, st.TaskID)); !os.IsNotExist(err) {
					t.Fatalf("journal kept: %v", err)
				}
				tr.groups.mu.Lock()
				cleaned := slices.Clone(tr.groups.cleaned)
				tr.groups.mu.Unlock()
				if len(cleaned) != 1 {
					t.Fatalf("the abandoned guardian's group was not waited for: %v", cleaned)
				}
			})
		}
	})
	t.Run("guardian-failures", func(t *testing.T) {
		t.Parallel()
		// The guardian cannot start, or reports its adapter's start
		// failed: a definite refusal after its group is gone.
		for name, set := range map[string]func(l *procLedger){
			"guardian": func(l *procLedger) { l.startErr = errUnsupported },
			"adapter":  func(l *procLedger) { l.adapterErr = errUnsupported },
		} {
			t.Run(name, func(t *testing.T) {
				t.Parallel()
				fp := startFakePlane(t)
				tr := startTaskRun(t, fp, taskOpts{})
				tr.ledger.mu.Lock()
				set(tr.ledger)
				tr.ledger.mu.Unlock()
				ins, run := manuals(t, tr.dir, "a", "m")
				s := tr.connect(t, 1, 1, roleConfig("a", ins, run))
				st, r := s.start(t, 1, 0, name)
				wantRefusal(t, r, contract.ReasonStartFailed)
				tr.noChild(t)
				if _, err := os.Stat(filepath.Join(tr.root, journalDir, st.TaskID)); !os.IsNotExist(err) {
					t.Fatalf("journal kept: %v", err)
				}
			})
		}
	})
	t.Run("budget", func(t *testing.T) {
		t.Parallel()
		// The 10 s preparation budget: ready one tick before it launches
		// (and answers ok); at or after it the unready guardian is
		// revoked, the start refused (preparation_timeout), nothing runs.
		for name, offset := range map[string]time.Duration{"minus-one": -time.Nanosecond, "exact": 0, "plus-one": time.Nanosecond} {
			t.Run(name, func(t *testing.T) {
				t.Parallel()
				fp := startFakePlane(t)
				tr := startTaskRun(t, fp, taskOpts{})
				hold := make(chan struct{})
				tr.ledger.mu.Lock()
				tr.ledger.holdReady = hold
				tr.ledger.mu.Unlock()
				ins, run := manuals(t, tr.dir, "a", "m")
				s := tr.connect(t, 1, 1, roleConfig("a", ins, run))
				tr.ev.awaitMatch(t, evCycleDone, nil)
				st := startBody(1, s.cfgs[0], 1, 1, name)
				rid := s.nextP()
				s.c.sendStart(rid, st)
				tr.ev.awaitMatch(t, evTaskPreparing, func(ev event) bool { return ev.id == st.TaskID })
				if err := tr.clk.AwaitWaiter(testWait, testkit.HasTimer(prepBudget)); err != nil {
					t.Fatal(err)
				}
				tr.clk.Advance(heartbeatInterval)
				s.beat(t, 1)
				tr.clk.Advance(prepBudget - heartbeatInterval + offset)
				if offset < 0 {
					close(hold)
					if r := s.c.startResult(rid); r.Err != nil {
						t.Fatalf("ready one tick early: %+v", r)
					}
					ch := tr.child(t)
					ch.exitCode(0)
					tr.settled(t, st.TaskID)
					return
				}
				var r *contract.TaskStartResult
				for r == nil {
					f := s.c.recv()
					switch f.Type {
					case contract.FrameHeartbeat:
						s.c.send(contract.ProtocolVersion, contract.FrameHeartbeatAck, f.RequestID, nil)
						s.b++
					case contract.FrameTaskStartResult:
						got, err := contract.DecodeTaskStartResult(f.Body)
						if err != nil {
							t.Fatal(err)
						}
						r = &got
					default:
						t.Fatalf("unexpected %s", f.Type)
					}
				}
				wantRefusal(t, *r, contract.ReasonPreparationTimeout)
				close(hold)
				tr.ev.awaitMatch(t, evTaskRefused, func(ev event) bool { return ev.id == st.TaskID })
				tr.noChild(t)
			})
		}
	})
	t.Run("entrypoint", func(t *testing.T) {
		t.Parallel()
		guardianEntrypoint(t)
	})
}

// failCounter fails (op, name) at its n-th occurrence only.
type failCounter struct {
	mu       sync.Mutex
	op, name string
	n, seen  int
	hits     int
	// upTo fails every occurrence up to the n-th.
	upTo bool
}

func (f *failCounter) set(op, name string, n int) {
	f.mu.Lock()
	f.op, f.name, f.n, f.seen, f.hits = op, name, n, 0, 0
	f.mu.Unlock()
}

func (f *failCounter) fail(op, name string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.op == "" || op != f.op || name != f.name {
		return nil
	}
	f.seen++
	if f.seen != f.n && !(f.upTo && f.seen < f.n) {
		return nil
	}
	f.hits++
	return errors.New("injected " + op + " failure at " + name)
}

// launchBarriers fails each durable step of the prepared journal (before
// any spawn) and of the running authorization (before the release byte):
// every start is refused definitely, no adapter starts, the journal is
// removed and the slot released.
func launchBarriers(t *testing.T) {
	fc := &failCounter{}
	fp := startFakePlane(t)
	tr := startTaskRun(t, fp, taskOpts{adjust: func(d *deps) { d.fail = fc.fail }})
	ins, run := manuals(t, tr.dir, "a", "m")
	cfg := roleConfig("a", ins, run)
	cfg.Concurrency = 1
	s := tr.connect(t, 1, 1, cfg)
	n := 0
	for _, c := range []struct {
		op, name  string
		nth       int
		guardians int // guardians started for the refused start
	}{
		// The staged task directory with its prepared document, then its
		// publication (rename) and tasks/'s sync.
		{"mkdir", journalDir, 1, 0},
		{"dirsync", rootName, 1, 0},
		{"mkdir", "stage", 1, 0},
		{"create", "stage/" + executionName, 1, 0},
		{"write", "stage/" + executionName, 1, 0},
		{"sync", "stage/" + executionName, 1, 0},
		{"rename", "stage/" + executionName, 1, 0},
		{"dirsync", "stage", 1, 0},
		{"rename", "", 1, 0},
		{"dirsync", journalDir, 1, 0},
		// The running authorization's journal write (the guardian is
		// ready, never released).
		{"create", executionName, 1, 1},
		{"write", executionName, 1, 1},
		{"sync", executionName, 1, 1},
		{"rename", executionName, 1, 1},
		{"dirsync", "", 1, 1},
	} {
		n++
		id := taskID(n)
		name := c.name
		switch {
		case name == "":
			name = taskDirRel(id)
		case name == executionName:
			name = filepath.Join(taskDirRel(id), executionName)
		case name == "stage":
			name = stageRel(id)
		case name == "stage/"+executionName:
			name = filepath.Join(stageRel(id), executionName)
		}
		fc.set(c.op, name, c.nth)
		t.Logf("fault %s %s #%d", c.op, name, c.nth)
		tr.ledger.mu.Lock()
		before := len(tr.ledger.guardians)
		tr.ledger.mu.Unlock()
		st, r := s.start(t, n, 0, c.op+" "+name)
		wantRefusal(t, r, contract.ReasonStartFailed)
		tr.ev.awaitMatch(t, evTaskRefused, func(ev event) bool { return ev.id == st.TaskID })
		fc.mu.Lock()
		hits := fc.hits
		fc.mu.Unlock()
		if hits != 1 {
			t.Fatalf("%s %s #%d was not reached", c.op, name, c.nth)
		}
		tr.noChild(t)
		tr.ledger.mu.Lock()
		gs := tr.ledger.guardians[before:]
		tr.ledger.mu.Unlock()
		if len(gs) != c.guardians {
			t.Fatalf("%s %s: %d guardians", c.op, name, len(gs))
		}
		for _, g := range gs {
			g.mu.Lock()
			released := g.released
			g.mu.Unlock()
			if released {
				t.Fatalf("%s %s: the guardian was released", c.op, name)
			}
		}
		if _, err := os.Stat(filepath.Join(tr.root, journalDir, st.TaskID)); !os.IsNotExist(err) {
			t.Fatalf("%s %s: journal kept (%v)", c.op, name, err)
		}
	}
	// The slot was released every time: a healthy start runs.
	fc.set("", "", 0)
	st, ch := s.run(t, n+1, 0, "healthy")
	ch.exitCode(0)
	tr.settled(t, st.TaskID)
	s.result(t, st)
}

// TestControlRestart is UT FP-5 on the sidecar, delegated from
// tests/function (TestControlWorkerRecovery): completed, active and
// prepared records; cleanup through the authentic guardian before
// readiness; a failed, EPERM or unprovable cleanup keeps the owner record
// and blocks every node start with a sidecar-local diagnostic only (the
// heartbeat's role status is unchanged; the plane sees can_accept false);
// host reboot evidence (ESRCH); repeat restarts with the same frozen
// outbox; a failing lost-outcome publication retried; a corrupt journal.
// Do not rename or skip it.
func TestControlRestart(t *testing.T) {
	t.Parallel()
	t.Run("completed", func(t *testing.T) {
		t.Parallel()
		// A completed outbox is replayed as it is: its tail tagged with its
		// digest, then the same result; deleted after the committed ack.
		fp := startFakePlane(t)
		root := newRoot(t)
		var st contract.TaskStartBody
		var res contract.TaskResultBody
		rr := startRecovery(t, fp, root, &fakeGroups{}, &fakeCommands{}, func(cfg contract.RoleConfig) {
			st = startBody(1, cfg, 1, 1, "completed")
			var j contract.ExecutionJournal
			j, res = completedJournal(st, 4, "tail\n")
			writeJournal(t, root, j, ownerOf(st, 7002, contract.OwnerReleased))
		})
		s, inv := rr.reconnect(t, 1, 1, map[string]string{st.TaskID: contract.ActionSendResult}, rr.cfg)
		if len(inv) != 1 || inv[0].Phase != contract.PhaseResult || *inv[0].ResultDigest != res.Digest {
			t.Fatalf("inventory %+v", inv)
		}
		rid := s.nextB()
		lb := s.c.expectLog(rid)
		if lb.LateDigest == nil || *lb.LateDigest != res.Digest || string(lb.Data) != "tail\n" || lb.Offset != 0 {
			t.Fatalf("replayed tail %+v", lb)
		}
		s.c.ackLog(rid, st.TaskID, 5)
		if r := s.result(t, st); r.Digest != res.Digest || *r.ExitCode != 4 {
			t.Fatalf("replayed %+v", r)
		}
		rr.ev.awaitMatch(t, evTaskForgotten, func(ev event) bool { return ev.id == st.TaskID })
		if _, err := os.Stat(filepath.Join(root, journalDir, st.TaskID)); !os.IsNotExist(err) {
			t.Fatalf("outbox kept: %v", err)
		}
	})
	t.Run("active", func(t *testing.T) {
		t.Parallel()
		// A running record whose guardian recorded ownership: the node
		// accepts nothing until the authentic guardian's cleanup is proved
		// (group gone); the execution is lost, never resumed.
		fp := startFakePlane(t)
		root := newRoot(t)
		g := &fakeGroups{alive: map[int]bool{7003: true}}
		release := g.hold()
		defer release()
		cmds := &fakeCommands{}
		var st contract.TaskStartBody
		rr := startRecovery(t, fp, root, g, cmds, func(cfg contract.RoleConfig) {
			st = startBody(1, cfg, 1, 1, "active")
			j := journalOf(st, contract.JournalRunning)
			j.Log = contract.TaskLog{Data: []byte("partial"), SourceBytes: 9, ReceivedBytes: 7, Incomplete: true}
			writeJournal(t, root, j, ownerOf(st, 7003, contract.OwnerReleased))
		})
		s, inv := rr.reconnect(t, 1, 1, map[string]string{st.TaskID: contract.ActionSendResult}, rr.cfg)
		if len(inv) != 1 || inv[0].Phase != contract.PhaseLost {
			t.Fatalf("inventory %+v", inv)
		}
		rr.ev.awaitMatch(t, evCycleDone, nil)
		rr.clk.Advance(heartbeatInterval)
		if hb := s.beat(t, 1); hb.Roles[0].CanAccept || hb.Roles[0].Inflight != 1 {
			t.Fatalf("ready before the cleanup: %+v", hb.Roles)
		}
		if sent := cmds.all(); len(sent) != 1 || !strings.Contains(sent[0], ctlNonce) || !strings.HasSuffix(strings.Fields(sent[0])[0], controlName) {
			t.Fatalf("commands %v", sent)
		}
		release()
		rr.ev.awaitMatch(t, evCleanupConfirmed, func(ev event) bool { return ev.id == st.TaskID })
		r, out := s.drain(t, st)
		if r.Outcome != contract.OutcomeLost || r.OutputBytes != 9 || !r.LogIncomplete || string(out) != "partial" {
			t.Fatalf("lost %+v %q", r, out)
		}
		rr.clk.Advance(heartbeatInterval)
		if hb := s.beat(t, 1); !hb.Roles[0].CanAccept || hb.Roles[0].Inflight != 0 {
			t.Fatalf("after the cleanup %+v", hb.Roles)
		}
	})
	t.Run("blocked", func(t *testing.T) {
		t.Parallel()
		// Absence cannot be proved: no guardian serves the FIFO while the
		// group exists, the group stays (EPERM), or the stopped guardian
		// never ends it. The owner record is kept, every node start is
		// refused, the diagnostic (task_id, a safe class) is local only,
		// the heartbeat's role status is unchanged and nothing signals the
		// raw group.
		for name, c := range map[string]struct {
			cmdErr, goneErr error
			class           string
		}{
			"no-guardian": {errNoGuardian, nil, "guardian_unavailable"},
			"eperm":       {nil, errPermission, "permission"},
			"stays":       {nil, errors.New("the group is still present"), "group_present"},
		} {
			t.Run(name, func(t *testing.T) {
				t.Parallel()
				fp := startFakePlane(t)
				root := newRoot(t)
				g := &fakeGroups{alive: map[int]bool{7004: true}, err: c.goneErr}
				cmds := &fakeCommands{err: c.cmdErr}
				var st contract.TaskStartBody
				rr := startRecovery(t, fp, root, g, cmds, func(cfg contract.RoleConfig) {
					st = startBody(1, cfg, 1, 1, "blocked")
					writeJournal(t, root, journalOf(st, contract.JournalRunning), ownerOf(st, 7004, contract.OwnerReleased))
				})
				rr.ev.awaitMatch(t, evCleanupBlocked, func(ev event) bool { return ev.id == st.TaskID })
				rr.logs.await(t, "local cleanup diagnostic", func(s string) bool {
					return strings.Contains(s, `"reason":"cleanup_unconfirmed"`) && strings.Contains(s, `"error_class":"`+c.class+`"`) &&
						strings.Contains(s, st.TaskID)
				})
				c2 := fp.accept(t)
				c2.helloOK(testID)
				c2.heartbeatAt(1, 0)
				inv := c2.inventory()
				c2.reconcile(inv, map[string]string{st.TaskID: contract.ActionSendResult})
				c2.replace("p1", 1, rr.cfg)
				c2.expectReplaceAck("p1", 1)
				s := &taskSession{c: c2, tr: rr.taskRun, b: 2, p: 2, gen: 1, cfgs: []contract.RoleConfig{rr.cfg}}
				rr.ev.awaitMatch(t, evCycleDone, nil)
				r, _ := s.drain(t, st)
				if r.Outcome != contract.OutcomeLost {
					t.Fatalf("blocked outcome %+v", r)
				}
				rr.clk.Advance(heartbeatInterval)
				rid := s.nextB()
				f := c2.expect(contract.FrameHeartbeat, rid)
				c2.send(contract.ProtocolVersion, contract.FrameHeartbeatAck, rid, nil)
				hb, _ := contract.DecodeHeartbeat(f.Body)
				if hb.Roles[0].CanAccept {
					t.Fatalf("ready with an unproved cleanup: %+v", hb.Roles)
				}
				if keys := statusKeys(t, f.Body); strings.Join(keys, ",") != "can_accept,concurrency,inflight,role_id" {
					t.Fatalf("role status keys %v", keys)
				}
				nb := startBody(2, rr.cfg, 1, 1, "refused")
				prid := s.nextP()
				c2.sendStart(prid, nb)
				wantRefusal(t, c2.startResult(prid), contract.ReasonLocalFull)
				if _, err := os.Stat(filepath.Join(root, journalDir, st.TaskID, ownerName)); err != nil {
					t.Fatalf("the owner record was removed: %v", err)
				}
				if len(g.signals()) != 0 {
					t.Fatal("a raw group was signaled")
				}
			})
		}
	})
	t.Run("reboot", func(t *testing.T) {
		t.Parallel()
		// After a host reboot the recorded group is absent (ESRCH): the
		// cleanup is confirmed at once, no command is sent.
		fp := startFakePlane(t)
		root := newRoot(t)
		g := &fakeGroups{alive: map[int]bool{}}
		cmds := &fakeCommands{}
		var st contract.TaskStartBody
		rr := startRecovery(t, fp, root, g, cmds, func(cfg contract.RoleConfig) {
			st = startBody(1, cfg, 1, 1, "reboot")
			writeJournal(t, root, journalOf(st, contract.JournalRunning), ownerOf(st, 7005, contract.OwnerReleased))
		})
		rr.ev.awaitMatch(t, evCleanupConfirmed, func(ev event) bool { return ev.id == st.TaskID })
		if len(cmds.all()) != 0 {
			t.Fatalf("commands %v", cmds.all())
		}
	})
	t.Run("repeat", func(t *testing.T) {
		t.Parallel()
		// Repeated restarts repeat the same safe recovery and replay the
		// same frozen outbox: one digest, never a second execution.
		fp := startFakePlane(t)
		root := newRoot(t)
		var st contract.TaskStartBody
		rr := startRecovery(t, fp, root, &fakeGroups{}, &fakeCommands{}, func(cfg contract.RoleConfig) {
			st = startBody(1, cfg, 1, 1, "repeat")
			j := journalOf(st, contract.JournalPrepared)
			j.OwnerNonce, j.StartedAt = nil, nil
			writeJournal(t, root, j, nil)
		})
		rr.ev.await(t, evRecoveryDone)
		first, err := (layout{root: root}).loadJournal(st.TaskID, rr.lookup())
		if err != nil || first.j.Phase != contract.JournalLost {
			t.Fatalf("first recovery %+v %v", first.j, err)
		}
		// The first Run's connection to the shared fake plane is consumed
		// and finished before it stops (it then waits out its backoff on
		// the fake clock, dialing nothing), so the second Run's
		// reconciliation reads only its own connection.
		first1 := fp.accept(t)
		first1.finish()
		rr.ev.await(t, evBackoff)
		rr.run.cancel()
		rr.run.result(t)
		rr2 := startRecovery(t, fp, root, &fakeGroups{}, &fakeCommands{}, nil)
		rr2.ev.await(t, evRecoveryDone)
		second, err := (layout{root: root}).loadJournal(st.TaskID, rr2.lookup())
		if err != nil || second.j.Result.Digest != first.j.Result.Digest {
			t.Fatalf("second recovery changed the outbox: %v", err)
		}
		s, inv := rr2.reconnect(t, 1, 1, map[string]string{st.TaskID: contract.ActionSendResult}, rr2.cfg)
		if len(inv) != 1 || *inv[0].ResultDigest != first.j.Result.Digest {
			t.Fatalf("inventory %+v", inv)
		}
		if r := s.result(t, st); r.Digest != first.j.Result.Digest {
			t.Fatal("a new digest was sent")
		}
		rr2.noChild(t)
	})
	t.Run("lost-outbox-retry", func(t *testing.T) {
		t.Parallel()
		// The lost outcome's publication fails: the node is blocked and
		// the publication retried every second until it succeeds.
		fp := startFakePlane(t)
		root := newRoot(t)
		fc := &failCounter{}
		dir := t.TempDir()
		ins, run := manuals(t, dir, "a", "m")
		cfg := roleConfig("a", ins, run)
		writeState(t, root, testID, fp.url, fp.caPEM)
		st := startBody(1, cfg, 1, 1, "retry")
		j := journalOf(st, contract.JournalPrepared)
		j.OwnerNonce, j.StartedAt = nil, nil
		writeJournal(t, root, j, nil)
		// The publication at recovery and its first timed retry fail; the
		// second succeeds.
		fc.set("rename", filepath.Join(taskDirRel(st.TaskID), executionName), 2)
		fc.upTo = true
		tr := startTaskRun(t, fp, taskOpts{root: root, adjust: func(d *deps) { d.fail = fc.fail }})
		for i := 0; i < 2; i++ {
			if err := tr.clk.AwaitWaiter(testWait, testkit.HasTimer(journalRetry)); err != nil {
				t.Fatal(err)
			}
			if !tr.super(t).nodeBlocked() {
				t.Fatal("a failing lost publication did not block the node")
			}
			tr.clk.Advance(journalRetry)
		}
		tr.ev.await(t, evRecoveryDone)
		if tr.super(t).nodeBlocked() {
			t.Fatal("still blocked after the retry")
		}
		if lj, err := (layout{root: root}).loadJournal(st.TaskID, tr.lookup()); err != nil || lj.j.Phase != contract.JournalLost {
			t.Fatalf("journal %+v %v", lj.j, err)
		}
	})
	t.Run("storage-failing-cleanup", func(t *testing.T) {
		t.Parallel()
		// Three recorded groups; the first execution's journal stays
		// unwritable. Cleanup never waits for failing result storage:
		// every guardian is asked to stop and every group proved gone
		// while that publication keeps failing, and the node stays
		// blocked (evidence kept) until both obligations complete.
		fp := startFakePlane(t)
		root := newRoot(t)
		dir := t.TempDir()
		ins, run := manuals(t, dir, "a", "m")
		cfg := roleConfig("a", ins, run)
		writeState(t, root, testID, fp.url, fp.caPEM)
		g := &fakeGroups{alive: map[int]bool{}}
		cmds := &fakeCommands{}
		var sts []contract.TaskStartBody
		for i := 1; i <= 3; i++ {
			st := startBody(i, cfg, 1, 1, "group "+strconv.Itoa(i))
			pgid := 7100 + i
			g.alive[pgid] = true
			writeJournal(t, root, journalOf(st, contract.JournalRunning), ownerOf(st, pgid, contract.OwnerReleased))
			sts = append(sts, st)
		}
		cmds.deliver = func(fifo string) {
			g.mu.Lock()
			defer g.mu.Unlock()
			for i, st := range sts {
				if strings.Contains(fifo, st.TaskID) {
					g.alive[7101+i] = false
				}
			}
		}
		fc := &failCounter{}
		fc.set("rename", filepath.Join(taskDirRel(sts[0].TaskID), executionName), 1<<30)
		fc.upTo = true
		tr := startTaskRun(t, fp, taskOpts{root: root, adjust: func(d *deps) {
			d.fail = fc.fail
			d.taskGroups = g
			d.taskCommand = cmds.send
		}})
		for _, st := range sts {
			tr.ev.awaitMatch(t, evCleanupConfirmed, func(ev event) bool { return ev.id == st.TaskID })
		}
		if n := len(cmds.all()); n != 3 {
			t.Fatalf("%d stop commands, want 3", n)
		}
		if !tr.super(t).nodeBlocked() {
			t.Fatal("the failing publication stopped blocking the node")
		}
		if lj, err := (layout{root: root}).loadJournal(sts[0].TaskID, tr.lookup()); err != nil || lj.j.Phase != contract.JournalRunning || lj.owner == nil {
			t.Fatalf("the failing journal's evidence %+v %v", lj.j.Phase, err)
		}
		// Storage recovers: the next retry publishes the lost outcome and
		// the node accepts again.
		fc.set("", "", 0)
		if err := tr.clk.AwaitWaiter(testWait, testkit.HasTimer(journalRetry)); err != nil {
			t.Fatal(err)
		}
		tr.clk.Advance(journalRetry)
		tr.ev.await(t, evRecoveryDone)
		if tr.super(t).nodeBlocked() {
			t.Fatal("still blocked after both obligations completed")
		}
		if lj, err := (layout{root: root}).loadJournal(sts[0].TaskID, tr.lookup()); err != nil || lj.j.Phase != contract.JournalLost {
			t.Fatalf("journal %+v %v", lj.j.Phase, err)
		}
	})
	t.Run("bounded-replay", func(t *testing.T) {
		t.Parallel()
		// Many completed outboxes: recovery keeps compact metadata only
		// (no output tail in memory) and replays tails through a bounded
		// loader, one recovered tail in memory at a time, each tail and
		// result exact.
		// Twelve outboxes of four chunks each: enough to observe that no
		// tail is held at recovery and that replays never overlap in memory
		// (the property does not depend on the tail size; 10 MiB tails
		// through the race-enabled stress would dominate the shard).
		const n, size = 12, 4 * contract.MaxLogChunkBytes
		fp := startFakePlane(t)
		root := newRoot(t)
		var sts []contract.TaskStartBody
		tails := map[string][]byte{}
		rr := startRecovery(t, fp, root, &fakeGroups{}, &fakeCommands{}, func(cfg contract.RoleConfig) {
			for i := 1; i <= n; i++ {
				st := startBody(i, cfg, 1, 1, "outbox")
				tail := bytes.Repeat([]byte{byte('a' + i)}, size)
				j, _ := completedJournal(st, 0, string(tail))
				writeJournal(t, root, j, nil)
				sts = append(sts, st)
				tails[st.TaskID] = tail
			}
		})
		rr.ev.await(t, evRecoveryDone)
		loaded := func() (int, int) {
			sup := rr.super(t)
			total, tasks := 0, 0
			for _, w := range sup.snapshot() {
				w.ring.mu.Lock()
				if c := cap(w.ring.buf); c > 0 {
					total += c
					tasks++
				}
				w.ring.mu.Unlock()
			}
			return total, tasks
		}
		if total, _ := loaded(); total != 0 {
			t.Fatalf("recovery retained %d bytes of output tails", total)
		}
		actions := map[string]string{}
		for _, st := range sts {
			actions[st.TaskID] = contract.ActionSendResult
		}
		s, inv := rr.reconnect(t, 1, 1, actions, rr.cfg)
		if len(inv) != n {
			t.Fatalf("inventory %d", len(inv))
		}
		got := map[string][]byte{}
		results := 0
		for results < n {
			if total, tasks := loaded(); tasks > 1 || total > contract.MaxLogRetainedBytes {
				t.Fatalf("%d recovered tails (%d bytes) in memory at once", tasks, total)
			}
			rid := s.nextB()
			f := s.c.recv()
			if f.RequestID != rid {
				t.Fatalf("got %s %s, want %s", f.Type, f.RequestID, rid)
			}
			switch f.Type {
			case contract.FrameTaskLog:
				lb, err := contract.DecodeTaskLog(f.Body)
				if err != nil || lb.LateDigest == nil {
					t.Fatalf("log %v", err)
				}
				got[lb.TaskID] = append(got[lb.TaskID], lb.Data...)
				s.c.ackLog(rid, lb.TaskID, lb.Offset+len(lb.Data))
			case contract.FrameTaskResult:
				r, err := contract.DecodeTaskResult(f.Body)
				if err != nil || !bytes.Equal(got[r.TaskID], tails[r.TaskID]) {
					t.Fatalf("result of %s after %d of %d tail bytes: %v", r.TaskID, len(got[r.TaskID]), size, err)
				}
				s.c.ackResult(rid, r.TaskID, r.Digest, true)
				results++
			default:
				t.Fatalf("unexpected %s", f.Type)
			}
		}
	})
	t.Run("journal-crash-windows", func(t *testing.T) {
		t.Parallel()
		journalCrashWindows(t)
	})
	t.Run("corrupt", func(t *testing.T) {
		t.Parallel()
		// An invalid journal stops Run; nothing is repaired or deleted.
		fp := startFakePlane(t)
		root := newRoot(t)
		writeState(t, root, testID, fp.url, fp.caPEM)
		dir := filepath.Join(root, journalDir, taskID(1))
		os.MkdirAll(dir, 0o700)
		os.WriteFile(filepath.Join(dir, executionName), []byte("{"), 0o600)
		d := testDeps(testkit.NewFakeClock(time.Now()))
		err := d.run(bg, RunOptions{StateDir: root, SoftwareVersion: "test-1", Logger: discard(), GOOS: "linux"})
		if contract.CodeOf(err) != contract.CodeConflict || !strings.Contains(err.Error(), "never repaired or deleted") {
			t.Fatalf("run = %v", err)
		}
		if b, _ := os.ReadFile(filepath.Join(dir, executionName)); string(b) != "{" {
			t.Fatal("the corrupt journal was changed")
		}
	})
}

// TestControlPlaneRestart is UT FP-6 on the sidecar, delegated from
// tests/function (TestControlPlaneRecovery) with the plane's: the plane
// restarting never stops a running child; the D2 capped reconnect (the
// maximal jitter sample, 30 s, then the dial and both hello phases one
// tick short) reattaches the same execution with no lost decision, no
// cleanup signal and no start replay; a completion during the downtime is
// replayed from the outbox; interrupted paging restarts from page 0. Do
// not rename or skip it.
func TestControlPlaneRestart(t *testing.T) {
	t.Parallel()
	t.Run("timeline", func(t *testing.T) {
		t.Parallel()
		fp := startFakePlane(t)
		tr := startTaskRun(t, fp, taskOpts{adjust: func(d *deps) { d.jitter = func() float64 { return 1.0 } }})
		ins, run := manuals(t, tr.dir, "a", "m")
		cfg := roleConfig("a", ins, run)
		s := tr.connect(t, 1, 1, cfg)
		st, ch := s.run(t, 1, 0, "survives the plane")
		ch.prompt(t)
		// The plane goes away and stays down through the capped backoff.
		fp.setRefuse(http.StatusServiceUnavailable)
		s.c.finish()
		tr.ev.awaitMatch(t, evTaskFenced, func(ev event) bool { return ev.id == st.TaskID })
		for _, want := range []time.Duration{time.Second, 2 * time.Second, 4 * time.Second, 8 * time.Second, 16 * time.Second} {
			tr.advanceBackoff(t, want)
		}
		ev := tr.ev.await(t, evBackoff)
		if ev.delay != 30*time.Second {
			t.Fatalf("capped backoff %v with the maximal jitter sample", ev.delay)
		}
		if err := tr.clk.AwaitWaiter(testWait, testkit.HasTimer(30*time.Second)); err != nil {
			t.Fatal(err)
		}
		armed := tr.clk.Now()
		// The plane is back; the dial waits the whole backoff.
		fp.mu.Lock()
		fp.refuse = 0
		fp.onUpgrade = func() {
			// The dial takes 10 s minus one tick, and the hello write's
			// phase 5 s minus one tick (charged before the write: a live
			// socket completes it at once).
			tr.clk.Advance(10*time.Second - time.Nanosecond + 5*time.Second - time.Nanosecond)
		}
		fp.mu.Unlock()
		tr.clk.Advance(30*time.Second - time.Nanosecond)
		if !testkit.HasTimer(30 * time.Second)(tr.clk.Waiters()) {
			t.Fatal("the capped backoff ended a tick early")
		}
		tr.clk.Advance(time.Nanosecond)
		c := fp.accept(t)
		fp.mu.Lock()
		fp.onUpgrade = nil
		fp.mu.Unlock()
		c.expect(contract.FrameHello, "h1")
		tr.awaitReply(t, 0)
		tr.clk.Advance(5*time.Second - time.Nanosecond)
		if got := tr.clk.Now().Sub(armed); got != 50*time.Second-3*time.Nanosecond {
			t.Fatalf("attached at %v after the capped backoff began", got)
		}
		c.send(contract.ProtocolVersion, contract.FrameHelloOK, "h1", contract.HelloOKBody{HeartbeatIntervalMS: contract.HeartbeatIntervalMS, LeaseMS: contract.LeaseMS})
		tr.ev.await(t, evConnected)
		c.heartbeatAt(1, 0)
		inv := c.inventory()
		if len(inv) != 1 || inv[0].Execution != st.Execution || inv[0].Phase != contract.PhaseRunning || inv[0].StartedAt == nil {
			t.Fatalf("inventory %+v", inv)
		}
		c.reconcile(inv, map[string]string{st.TaskID: contract.ActionContinue})
		c.replace("p1", 2, cfg)
		c.expectReplaceAck("p1", 2)
		s = &taskSession{c: c, tr: tr, b: 2, p: 2, gen: 2, cfgs: []contract.RoleConfig{cfg}}
		ch.out([]byte("after the restart\n"))
		ch.exitCode(0)
		tr.settled(t, st.TaskID)
		r, out := s.drain(t, st)
		if r.Outcome != contract.OutcomeNatural || *r.ExitCode != 0 || string(out) != "after the restart\n" {
			t.Fatalf("result %+v %q", r, out)
		}
		tr.ledger.mu.Lock()
		n := len(tr.ledger.guardians)
		tr.ledger.mu.Unlock()
		if n != 1 || len(tr.groups.signals()) != 0 {
			t.Fatalf("%d guardians, cleanup signals %v", n, tr.groups.signals())
		}
	})
	t.Run("downtime-completion", func(t *testing.T) {
		t.Parallel()
		// The child completes while the plane is down: its outcome is
		// journaled (the outbox) and replayed on the next attachment.
		fp := startFakePlane(t)
		tr := startTaskRun(t, fp, taskOpts{})
		ins, run := manuals(t, tr.dir, "a", "m")
		cfg := roleConfig("a", ins, run)
		s := tr.connect(t, 1, 1, cfg)
		st, ch := s.run(t, 1, 0, "downtime")
		ch.prompt(t)
		s.detach(t, false, st.TaskID)
		ch.out([]byte("done offline\n"))
		ch.exitCode(2)
		tr.settled(t, st.TaskID)
		lj, err := (layout{root: tr.root}).loadJournal(st.TaskID, tr.lookup())
		if err != nil || lj.j.Phase != contract.JournalCompleted {
			t.Fatalf("outbox %+v %v", lj.j, err)
		}
		s, inv := tr.reconnect(t, 2, 2, map[string]string{st.TaskID: contract.ActionSendResult}, cfg)
		if len(inv) != 1 || inv[0].Phase != contract.PhaseResult || *inv[0].ResultDigest != lj.j.Result.Digest {
			t.Fatalf("inventory %+v", inv)
		}
		r, out := s.drain(t, st)
		if r.Digest != lj.j.Result.Digest || *r.ExitCode != 2 || string(out) != "done offline\n" {
			t.Fatalf("replayed %+v %q", r, out)
		}
	})
	t.Run("interrupted-paging", func(t *testing.T) {
		t.Parallel()
		// The attachment ends after page 0: the next one restarts at page
		// 0 with its own frozen list; nothing was decided meanwhile.
		fp := startFakePlane(t)
		root := newRoot(t)
		rr := startRecovery(t, fp, root, &fakeGroups{}, &fakeCommands{}, func(cfg contract.RoleConfig) {
			for i := 1; i <= 65; i++ {
				j, _ := completedJournal(startBody(i, cfg, 1, 1, "paged"), 0, "")
				writeJournal(t, root, j, nil)
			}
		})
		c := fp.accept(t)
		c.helloOK(testID)
		c.heartbeatAt(1, 0)
		c.expect(contract.FrameTaskInventory, "i1")
		c.send(contract.ProtocolVersion, contract.FrameTaskInventoryAck, "i1", contract.TaskInventoryAckBody{Page: 0, Received: true})
		c.expect(contract.FrameTaskInventory, "i2")
		c.finish()
		rr.ev.await(t, evEnded)
		rr.advanceBackoff(t, backoff(0, 0.5))
		c = fp.accept(t)
		c.helloOK(testID)
		c.heartbeatAt(1, 0)
		if inv := c.inventory(); len(inv) != 65 {
			t.Fatalf("restarted inventory %d entries", len(inv))
		}
		if ids, _ := (layout{root: root}).scanJournals(); len(ids) != 65 {
			t.Fatalf("%d journals", len(ids))
		}
	})
}

// TestControlLate is UT FP-7 on the sidecar, delegated from
// tests/function (TestControlLateResult) with the plane's: a lost
// execution's outcome and tail go out as late evidence (its log frames
// tagged with its digest), the same digest after an acknowledgement loss
// or a crash during the late append, and dispositions naming an unknown
// task or token change nothing. Do not rename or skip it.
func TestControlLate(t *testing.T) {
	t.Parallel()
	t.Run("stop-lost", func(t *testing.T) {
		t.Parallel()
		// stop_lost: the guardian stops the group; the frozen lost outcome
		// is sent with its tail tagged by its digest.
		fp := startFakePlane(t)
		tr := startTaskRun(t, fp, taskOpts{})
		ins, run := manuals(t, tr.dir, "a", "m")
		cfg := roleConfig("a", ins, run)
		s := tr.connect(t, 1, 1, cfg)
		st, ch := s.run(t, 1, 0, "late")
		ch.prompt(t)
		ch.out([]byte("before the loss\n"))
		rid := s.nextB()
		lb := s.c.expectLog(rid)
		s.c.ackLog(rid, st.TaskID, lb.Offset+len(lb.Data))
		ch.out([]byte("unsent\n"))
		s.detach(t, false, st.TaskID)
		// The stopped group's disappearance waits until the reconnection's
		// own events were consumed (its exit event must not be).
		release := tr.groups.hold()
		defer release()
		s, _ = tr.reconnect(t, 2, 2, map[string]string{st.TaskID: contract.ActionStopLost}, cfg)
		release()
		tr.settled(t, st.TaskID)
		var tagged []contract.TaskLogBody
		var res contract.TaskResultBody
		for {
			rid := s.nextB()
			f := s.c.recv()
			if f.Type == contract.FrameTaskLog {
				b, _ := contract.DecodeTaskLog(f.Body)
				tagged = append(tagged, b)
				s.c.ackLog(rid, st.TaskID, b.Offset+len(b.Data))
				continue
			}
			if f.Type != contract.FrameTaskResult || f.RequestID != rid {
				t.Fatalf("got %s %s", f.Type, f.RequestID)
			}
			res, _ = contract.DecodeTaskResult(f.Body)
			s.c.ackResult(rid, st.TaskID, res.Digest, true)
			break
		}
		if res.Outcome != contract.OutcomeLost || len(tagged) == 0 {
			t.Fatalf("late outcome %+v, %d tagged chunks", res, len(tagged))
		}
		for _, b := range tagged {
			if b.LateDigest == nil || *b.LateDigest != res.Digest || b.Offset < 16 {
				t.Fatalf("late chunk %+v", b)
			}
		}
		tr.ev.awaitMatch(t, evTaskForgotten, func(ev event) bool { return ev.id == st.TaskID })
	})
	t.Run("ack-loss", func(t *testing.T) {
		t.Parallel()
		// The late result's acknowledgement is lost: the next attachment
		// is told send_result again and restarts the tail and the result
		// with the same digest.
		fp := startFakePlane(t)
		root := newRoot(t)
		var st contract.TaskStartBody
		var res contract.TaskResultBody
		rr := startRecovery(t, fp, root, &fakeGroups{}, &fakeCommands{}, func(cfg contract.RoleConfig) {
			st = startBody(1, cfg, 1, 1, "ack loss")
			var j contract.ExecutionJournal
			j, res = completedJournal(st, 0, "late tail\n")
			writeJournal(t, root, j, nil)
		})
		s, _ := rr.reconnect(t, 1, 1, map[string]string{st.TaskID: contract.ActionSendResult}, rr.cfg)
		rid := s.nextB()
		s.c.expectLog(rid)
		s.c.ackLog(rid, st.TaskID, 10)
		rid = s.nextB()
		if r := s.c.expectResult(rid); r.Digest != res.Digest {
			t.Fatal("digest")
		}
		s.c.finish()
		rr.ev.await(t, evEnded)
		rr.advanceBackoff(t, backoff(0, 0.5))
		s, inv := rr.reconnect(t, 2, 2, map[string]string{st.TaskID: contract.ActionSendResult}, rr.cfg)
		if len(inv) != 1 || *inv[0].ResultDigest != res.Digest {
			t.Fatalf("inventory %+v", inv)
		}
		// The plane received the tail on the earlier attachment (its
		// staging holds it): only the result is resent, same digest.
		r, out := s.drain(t, st)
		if r.Digest != res.Digest || len(out) != 0 {
			t.Fatalf("resent %+v %q", r, out)
		}
	})
	t.Run("crash-during-append", func(t *testing.T) {
		t.Parallel()
		// Run ends while the late evidence is unacknowledged: the next Run
		// replays the same outbox (digest and tail).
		fp := startFakePlane(t)
		root := newRoot(t)
		var st contract.TaskStartBody
		var res contract.TaskResultBody
		rr := startRecovery(t, fp, root, &fakeGroups{}, &fakeCommands{}, func(cfg contract.RoleConfig) {
			st = startBody(1, cfg, 1, 1, "crash")
			var j contract.ExecutionJournal
			j, res = completedJournal(st, 1, "x\n")
			writeJournal(t, root, j, nil)
		})
		s, _ := rr.reconnect(t, 1, 1, map[string]string{st.TaskID: contract.ActionSendResult}, rr.cfg)
		s.c.expectLog(s.nextB())
		// The first Run's only connection is this reconciled one (the test
		// read it): stopping the Run dials nothing more.
		rr.run.cancel()
		rr.run.result(t)
		rr2 := startRecovery(t, fp, root, &fakeGroups{}, &fakeCommands{}, nil)
		s, _ = rr2.reconnect(t, 1, 1, map[string]string{st.TaskID: contract.ActionSendResult}, rr2.cfg)
		if r, out := s.drain(t, st); r.Digest != res.Digest || !bytes.Equal(out, []byte("x\n")) {
			t.Fatalf("replayed %+v %q", r, out)
		}
	})
}

// BenchmarkControlReplay (sidecar) encodes one maximum outbox (a maximum
// result with a 10 MiB retained tail) as its journal, and one 64-entry
// inventory page from held executions, asserting their bounds.
func BenchmarkControlReplay(b *testing.B) {
	role := contract.RoleRecord{RoleConfig: roleConfig("a", "/srv/i.md", "/srv/r.md"), RegistrationOrder: 1}
	st := startBody(1, role.RoleConfig, 1, 1, "bench")
	msg := strings.Repeat("\x02", contract.MaxFinalMessageBytes)
	code := 1
	res := contract.TaskResultBody{TaskID: st.TaskID, Execution: st.Execution, Outcome: contract.OutcomeNatural, ExitCode: &code, FinalMessage: &msg,
		FinalMessageTruncated: true, OutputBytes: contract.MaxLogRetainedBytes, LogIncomplete: true}.Sealed()
	j := journalOf(st, contract.JournalCompleted)
	j.Result = &res
	j.Log = contract.TaskLog{Data: bytes.Repeat([]byte("y"), contract.MaxLogRetainedBytes), SourceBytes: contract.MaxLogRetainedBytes, ReceivedBytes: contract.MaxLogRetainedBytes}
	b.Run("outbox", func(b *testing.B) {
		b.ReportAllocs()
		var n int
		for b.Loop() {
			enc, err := contract.EncodeExecutionJournal(j)
			if err != nil {
				b.Fatal(err)
			}
			n = len(enc)
		}
		if n > contract.MaxExecutionJournalBytes {
			b.Fatalf("outbox %d bytes", n)
		}
		b.ReportMetric(float64(n), "journal-bytes")
	})
	b.Run("inventory", func(b *testing.B) {
		sup := &taskSupervisor{workers: map[*taskWorker]bool{}, runID: strings.Repeat("a", 32)}
		var ids []string
		for i := 1; i <= contract.MaxInventoryEntries; i++ {
			s := startBody(i, role.RoleConfig, 1, 1, "held")
			at := ctlStarted
			w := &taskWorker{start: s, digest: s.StartDigestHex(), phase: phaseStarted, started: &at}
			sup.workers[w] = true
			ids = append(ids, s.TaskID)
		}
		rs := &roleSession{tasks: sup, invIDs: ids}
		b.ReportAllocs()
		var n int
		for b.Loop() {
			page, final := rs.inventoryPage(0)
			if !final || len(page.Entries) != contract.MaxInventoryEntries {
				b.Fatalf("page of %d entries (final %v)", len(page.Entries), final)
			}
			f, err := contract.EncodeFrame(contract.ProtocolVersion, contract.FrameTaskInventory, "i1", page)
			if err != nil {
				b.Fatal(err)
			}
			n = len(f)
		}
		if n > contract.MaxInventoryBody+256 {
			b.Fatalf("inventory frame %d bytes", n)
		}
		b.ReportMetric(float64(n), "frame-bytes")
	})
}
