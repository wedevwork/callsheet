package sidecar

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/wedevwork/callsheet/internal/adapter"
	"github.com/wedevwork/callsheet/internal/contract"
	"github.com/wedevwork/callsheet/internal/testkit"
)

// sentinel is manual content that must never leave the node.
const sentinel = "SENTINEL-MANUAL-BODY-7c1e"

// openOracle reports whether this process can actually open and read p,
// the oracle for permission cases (root or ACLs may read a mode-000 file).
func openOracle(p string) bool {
	f, err := os.Open(p)
	if err != nil {
		return false
	}
	defer f.Close()
	var b [1]byte
	_, err = f.Read(b[:])
	return err == nil || err.Error() == "EOF"
}

// TestRoleValidationContract is UT FP-3 on the worker: every manual
// outcome (actual and injected), the executable checks, precedence,
// timeout and late-result handling, and that no manual body or child
// output reaches the wire or the logs.
func TestRoleValidationContract(t *testing.T) {
	t.Run("manuals", func(t *testing.T) {
		t.Parallel()
		fp := startFakePlane(t)
		rr := startRoleRun(t, fp, true)
		c := fp.accept(t)
		c.connect()
		dir := rr.dir
		good, _ := manuals(t, dir, "good", sentinel)
		empty := filepath.Join(dir, "empty.md")
		os.WriteFile(empty, nil, 0o644)
		spaced := filepath.Join(dir, "with space", "manual file.md")
		os.MkdirAll(filepath.Dir(spaced), 0o755)
		os.WriteFile(spaced, []byte(sentinel), 0o644)
		link := filepath.Join(dir, "link.md")
		os.Symlink(good, link)
		parent := filepath.Join(dir, "parent-link")
		os.Symlink(filepath.Dir(good), parent)
		broken := filepath.Join(dir, "broken.md")
		os.Symlink(filepath.Join(dir, "absent.md"), broken)
		loop := filepath.Join(dir, "loop.md")
		os.Symlink(loop, loop)
		fifo := filepath.Join(dir, "fifo.md")
		if err := syscall.Mkfifo(fifo, 0o644); err != nil {
			t.Fatal(err)
		}
		locked := filepath.Join(dir, "locked.md")
		os.WriteFile(locked, []byte(sentinel), 0o000)
		wantLocked := contract.ReasonManualUnreadable
		if openOracle(locked) {
			wantLocked = "" // this user can read it despite the mode bits
		}
		n := 0
		for _, cs := range []struct {
			name, path, reason string
		}{
			{"regular", good, ""},
			{"empty", empty, ""},
			{"spaces", spaced, ""},
			{"symlink", link, ""},
			{"parent-symlink", filepath.Join(parent, "instruction.md"), ""},
			{"missing", filepath.Join(dir, "absent.md"), contract.ReasonManualUnreadable},
			{"broken-symlink", broken, contract.ReasonManualUnreadable},
			{"symlink-loop", loop, contract.ReasonManualUnreadable},
			{"not-a-directory", filepath.Join(good, "x"), contract.ReasonManualUnreadable},
			{"directory", dir, contract.ReasonManualNotRegular},
			{"fifo", fifo, contract.ReasonManualNotRegular},
			{"device", "/dev/null", contract.ReasonManualNotRegular},
			{"mode-000", locked, wantLocked},
		} {
			for _, field := range []string{"instruction", "runbook"} {
				n++
				rid := "p" + itoa(n)
				ins, run := good, good
				if field == "instruction" {
					ins = cs.path
				} else {
					run = cs.path
				}
				c.validate(rid, roleConfig("r", ins, run))
				e := c.result(rid)
				if cs.reason == "" {
					if e != nil {
						t.Fatalf("%s %s: %v", cs.name, field, e)
					}
					continue
				}
				wantResult(t, e, contract.CodeInvalidArgument, field, cs.reason)
				if strings.Contains(e.Message, cs.path) || strings.Contains(e.Message, sentinel) {
					t.Fatalf("%s %s: message leaks %q", cs.name, field, e.Message)
				}
			}
		}
		// Every successful check ran the probe with the configured path; the
		// manual bodies never appeared in logs.
		if rr.script.count() == 0 || rr.script.exes[0] != fakeExe || strings.Contains(rr.logs.String(), sentinel) {
			t.Fatalf("probes %d %v; logs:\n%s", rr.script.count(), rr.script.exes, rr.logs.String())
		}
	})
	t.Run("injected", func(t *testing.T) {
		t.Parallel()
		// A deterministic EACCES from open, and a manual whose descriptor
		// turns out not to be regular after the stat.
		fp := startFakePlane(t)
		var open func(string) (*os.File, error)
		rr := startRoleRunWith(t, fp, true, func(d *deps) {
			d.openManual = func(p string) (*os.File, error) { return open(p) }
		})
		c := fp.accept(t)
		c.connect()
		ins, run := manuals(t, rr.dir, "a", sentinel)
		open = func(p string) (*os.File, error) {
			if p == run {
				return nil, &fs.PathError{Op: "open", Path: p, Err: syscall.EACCES}
			}
			return openManual(p)
		}
		c.validate("p1", roleConfig("a", ins, run))
		e := c.result("p1")
		wantResult(t, e, contract.CodeInvalidArgument, "runbook", contract.ReasonManualUnreadable)
		if !strings.Contains(e.Message, "permission denied") {
			t.Fatalf("message %q", e.Message)
		}
		open = func(p string) (*os.File, error) { return os.Open(rr.dir) } // swapped for a directory
		c.validate("p2", roleConfig("a", ins, run))
		wantResult(t, c.result("p2"), contract.CodeInvalidArgument, "instruction", contract.ReasonManualNotRegular)
	})
	t.Run("executable", func(t *testing.T) {
		t.Parallel()
		// Disabled adapter, failing probe (its safe reason only), and a
		// structural precedence: node, then fields, adapter and effort,
		// then instruction, runbook and executable.
		fp := startFakePlane(t)
		off := startRoleRun(t, fp, false)
		c := fp.accept(t)
		c.connect()
		ins, run := manuals(t, off.dir, "a", sentinel)
		c.validate("p1", roleConfig("a", ins, run))
		e := c.result("p1")
		wantResult(t, e, contract.CodeInvalidArgument, "adapter", contract.ReasonAdapterDisabled)
		if e.Message != "fake adapter is disabled on node; start sidecar with --fake-adapter ABSOLUTE_PATH" {
			t.Fatalf("disabled message %q", e.Message)
		}
		if off.script.count() != 0 {
			t.Fatal("a disabled adapter was probed")
		}
		fp2 := startFakePlane(t)
		on := startRoleRun(t, fp2, true)
		c2 := fp2.accept(t)
		c2.connect()
		on.script.set(&adapter.ProbeError{Reason: "exited unsuccessfully"}, nil)
		c2.validate("p1", roleConfig("a", ins, run))
		e = c2.result("p1")
		wantResult(t, e, contract.CodeInvalidArgument, "adapter", contract.ReasonProbeFailed)
		if !strings.Contains(e.Message, "exited unsuccessfully") {
			t.Fatalf("probe message %q", e.Message)
		}
		on.script.set(errors.New("child stderr: SECRET-OUTPUT"), nil)
		c2.validate("p2", roleConfig("a", ins, run))
		if e := c2.result("p2"); strings.Contains(e.Message, "SECRET") {
			t.Fatalf("child output leaked: %q", e.Message)
		}
		on.script.set(nil, nil)
		other := roleConfig("a", ins, run)
		other.Node = "n_ffffffffffffffffffffffffffffffff"
		other.Instruction = "/absent"
		c2.validate("p3", other)
		wantResult(t, c2.result("p3"), contract.CodeInvalidArgument, "node", "")
		bad := roleConfig("a", "/absent", run)
		bad.Effort = "max"
		c2.validate("p4", bad)
		wantResult(t, c2.result("p4"), contract.CodeInvalidArgument, "effort", "")
		c2.validate("p5", roleConfig("a", "/absent", "/absent"))
		wantResult(t, c2.result("p5"), contract.CodeInvalidArgument, "instruction", contract.ReasonManualUnreadable)
		c2.validate("p6", roleConfig("a", ins, "/absent"))
		wantResult(t, c2.result("p6"), contract.CodeInvalidArgument, "runbook", contract.ReasonManualUnreadable)
		before := on.script.count()
		c2.validate("p7", roleConfig("a", ins, run))
		if e := c2.result("p7"); e != nil || on.script.count() != before+1 {
			t.Fatalf("success = %v, probes %d", e, on.script.count())
		}
	})
	t.Run("timeout", func(t *testing.T) {
		t.Parallel()
		// Budget 2 s from receipt: at expiry the reply is
		// unavailable/validation_timeout even though the probe has not
		// returned; the one slot stays occupied (busy) until it does; its
		// late result is discarded; afterwards the slot is free again.
		fp := startFakePlane(t)
		rr := startRoleRun(t, fp, true)
		t.Cleanup(func() {
			if t.Failed() {
				t.Log(rr.logs.String())
			}
		})
		c := fp.accept(t)
		c.connect()
		ins, run := manuals(t, rr.dir, "a", "x")
		block := make(chan struct{})
		rr.script.set(nil, block)
		rr.script.setStuck(true)
		c.validate("p1", roleConfig("a", ins, run))
		rr.script.awaitProbe(t)
		if err := rr.clk.AwaitWaiter(testWait, testkit.HasTimer(validationBudget)); err != nil {
			t.Fatal(err)
		}
		rr.clk.Advance(validationBudget)
		wantResult(t, c.result("p1"), contract.CodeUnavailable, "", contract.ReasonValidationTimeout)
		c.validate("p2", roleConfig("a", ins, run))
		wantResult(t, c.result("p2"), contract.CodeUnavailable, "", contract.ReasonBusy)
		// Once the stuck work returns, the worker frees the slot and its
		// late result is discarded: nothing is sent.
		close(block)
		rr.ev.await(t, evValidated)
		rr.script.set(nil, nil)
		c.validate("p3", roleConfig("a", ins, run))
		if e := c.result("p3"); e != nil {
			t.Fatalf("after the slot freed: %v", e)
		}
		// The stream and its heartbeats carry on.
		rr.clk.Advance(heartbeatInterval)
		c.heartbeatAt(2, 0)
	})
	t.Run("completion-before-deadline", func(t *testing.T) {
		t.Parallel()
		// Completion counts by the instant the worker finished, not by which
		// of the timer and the result the session sees first: the session is
		// held in a blocked heartbeat write while the check completes before
		// the deadline and the deadline then passes.
		pp := newPipePlane()
		rr := startRoleRunWith(t, nil, true, func(d *deps) {
			d.newClient = func(string, clientTrust) (planeClient, error) { return pp, nil }
		})
		c := pp.accept(t)
		if f := c.read(t); f.Type != contract.FrameHello {
			t.Fatalf("hello = %+v", f)
		}
		c.send(t, contract.FrameHelloOK, "h1", contract.HelloOKBody{HeartbeatIntervalMS: contract.HeartbeatIntervalMS, LeaseMS: contract.LeaseMS})
		if f := c.read(t); f.RequestID != "b1" {
			t.Fatalf("b1 = %+v", f)
		}
		c.send(t, contract.FrameHeartbeatAck, "b1", nil)
		rr.ev.await(t, evAck) // T0: b2 is due at T0+5s
		c.reconcileEmpty(t)
		rr.ev.awaitWritten(t, evReplied, "r1")
		ins, run := manuals(t, rr.dir, "a", "x")
		block := make(chan struct{})
		rr.script.set(nil, block)
		rr.clk.Advance(4 * time.Second)
		c.send(t, contract.FrameRoleValidate, "p1", contract.RoleValidateBody{Role: roleConfig("a", ins, run)}) // deadline T0+6s
		rr.script.awaitProbe(t)
		rr.clk.Advance(time.Second) // T0+5s: b2 is written and blocks (unread)
		rr.ev.awaitMatch(t, evValidating, nil)
		if err := rr.clk.AwaitWaiter(testWait, testkit.HasTimer(stepTimeout)); err != nil {
			t.Fatal(err)
		}
		close(block)
		if ev := rr.ev.await(t, evValidated); ev.err != nil { // finished at T0+5s
			t.Fatal(ev.err)
		}
		rr.clk.Advance(time.Second) // T0+6s: the deadline fires meanwhile
		if f := c.read(t); f.Type != contract.FrameHeartbeat || f.RequestID != "b2" {
			t.Fatalf("b2 = %+v", f)
		}
		f := c.read(t)
		e, err := contract.DecodeRoleValidateResult(f.Body)
		if f.Type != contract.FrameRoleValidateResult || f.RequestID != "p1" || err != nil || e != nil {
			t.Fatalf("result = %+v %v %v", f, e, err)
		}
		c.send(t, contract.FrameHeartbeatAck, "b2", nil)
		rr.ev.awaitMatch(t, evAck, func(ev event) bool { return ev.acks == 2 })
		rr.run.cancel()
		c.ws.Close(websocket.StatusNormalClosure, "")
	})
	t.Run("body-free-wire", func(t *testing.T) {
		t.Parallel()
		// Across a success, a failure, a snapshot and heartbeats, no
		// message the sidecar sends carries a manual's content.
		fp := startFakePlane(t)
		rr := startRoleRun(t, fp, true)
		c := fp.accept(t)
		c.setManual()
		f := c.expect(contract.FrameHello, "h1")
		wire := string(f.Body)
		c.send(contract.ProtocolVersion, contract.FrameHelloOK, "h1", contract.HelloOKBody{HeartbeatIntervalMS: contract.HeartbeatIntervalMS, LeaseMS: contract.LeaseMS})
		ins, run := manuals(t, rr.dir, "a", sentinel)
		b := c.readHeartbeat(1)
		wire += mustJSON(t, b)
		c.send(contract.ProtocolVersion, contract.FrameHeartbeatAck, "b1", nil)
		wire += mustJSON(t, c.inventory())
		c.reconcile(nil, nil)
		c.validate("p1", roleConfig("a", ins, run))
		if e := c.result("p1"); e != nil {
			t.Fatal(e)
		}
		c.validate("p2", roleConfig("a", ins, filepath.Join(rr.dir, "absent")))
		wire += mustJSON(t, c.result("p2"))
		c.replace("p3", 1, roleConfig("a", ins, run))
		c.expectReplaceAck("p3", 1)
		// The snapshot's immediate report (iteration 10a).
		wire += mustJSON(t, c.heartbeatAt(2, 1))
		if strings.Contains(wire, sentinel) || strings.Contains(rr.logs.String(), sentinel) {
			t.Fatal("a manual body left the node")
		}
	})
}

func itoa(n int) string { return strconv.Itoa(n) }
