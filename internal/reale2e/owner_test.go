package reale2e

import (
	"bufio"
	"bytes"
	"io"
	"net"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/wedevwork/callsheet/internal/contract"
)

// typeLines returns an owner input that types lines on the terminal.
func typeLines(f *fakeWorld, lines ...string) func(*coordScript) {
	return func(*coordScript) {
		for _, l := range lines {
			io.WriteString(f.stdinW, l+"\n")
		}
		settleTerminal()
	}
}

// settleTerminal yields so the terminal reader can run.
func settleTerminal() { time.Sleep(time.Millisecond) }

// holdForDecision holds the fake time, one tick at a time, after the
// owner's input until the supervisor has acted on it: the reader
// goroutine's hand-off of the line or EOF then never races the fake 20m
// deadline, whatever the scheduling (-race, -cpu=1). Only for inputs that
// end the wait (a decision or EOF).
func holdForDecision(f *fakeWorld) func(c *coordScript) bool {
	return func(c *coordScript) bool {
		if c.step == 5 && c.s.owner == ownerAwaiting {
			f.clock.holdNext()
			settleTerminal()
			return true
		}
		return false
	}
}

// coderDispatched reports whether any coder task reached the plane.
func coderDispatched(p *fakePlane) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, id := range p.order {
		if p.tasks[id].Role.ID == HopCoder && p.tasks[id].WorkspaceBinding != nil {
			return true
		}
	}
	return false
}

// TestOwnerGate (FP-5): after an approved design review the supervisor
// presents the reviewed artifacts and their digests and waits; only the
// owner's exact "yes RUN" on the terminal records a decision bound to the
// reviewed task, commit and digests and unlocks coding through a
// read-only receipt. No, EOF, a timeout, ambiguous or blank input and
// coding before the yes never pass.
func TestOwnerGate(t *testing.T) {
	t.Parallel()
	t.Run("yes", func(t *testing.T) {
		t.Parallel()
		f := newFakeWorld(t)
		f.coord.ownerInput = func(c *coordScript) {
			typeLines(f, "", "yes", "yes 20200101T000000Z-00000000", "maybe", "yes "+c.s.runID, "no "+c.s.runID)(c)
		}
		f.coord.before = holdForDecision(f)
		if code := f.attempt(); code != 0 {
			t.Fatalf("attempt = %d %s", code, f.stderr.String())
		}
		out := f.stdout.String()
		if strings.Count(out, "not a decision") < 4 || !strings.Contains(out, "OWNER DECISION NEEDED") || !strings.Contains(out, "continue after recorded decision") {
			t.Fatalf("terminal:\n%s", out)
		}
		b := f.bundle()
		var d DecisionRecord
		readDoc(t, filepath.Join(b, "owner-decision.json"), &d)
		files, h2 := hopFilesAt(t, b, 2)
		if d.Decision != DecisionYes || d.ReviewedTask != f.coord.tasks[1] || d.ResultCommit != h2.String() ||
			d.DesignSHA256 != sha256Hex(files["design.md"]) || d.ReviewSHA256 != sha256Hex(files["design-review.md"]) {
			t.Fatalf("decision %+v", d)
		}
		types := eventTypesOf(t, b)
		if strings.Count(strings.Join(types, " "), EvOwnerDecision) != 1 {
			t.Fatalf("decisions in %v", types)
		}
		if !strings.Contains(out, "sha256 "+d.DesignSHA256) || !strings.Contains(out, "sha256 "+d.ReviewSHA256) {
			t.Fatal("the digests were not shown to the owner")
		}
	})
	t.Run("receipt", func(t *testing.T) {
		t.Parallel()
		f := newFakeWorld(t)
		var receipt []byte
		var mode os.FileMode
		f.coord.before = func(c *coordScript) bool {
			if c.step == 6 && receipt == nil {
				p := filepath.Join(c.s.runtime, "owner-receipt.json")
				receipt, _ = os.ReadFile(p)
				st, _ := os.Stat(p)
				mode = st.Mode().Perm()
				c.s.fail("test_stop", io.EOF)
			}
			return false
		}
		f.attempt()
		var r DecisionRecord
		if err := decodeStrict(receipt, &r); err != nil || r.Decision != DecisionYes || r.RunID != f.sup.runID || mode != 0o400 {
			t.Fatalf("receipt %s %v %v", receipt, err, mode)
		}
	})
	failing := map[string]struct {
		setup func(f *fakeWorld)
		code  string
	}{
		"no": {func(f *fakeWorld) {
			f.coord.ownerInput = func(c *coordScript) { typeLines(f, "no "+c.s.runID)(c) }
			f.coord.before = holdForDecision(f)
		}, "owner_rejected"},
		"eof": {func(f *fakeWorld) {
			f.coord.ownerInput = func(*coordScript) { f.stdinW.Close() }
			f.coord.before = holdForDecision(f)
		}, "owner_decision_missing"},
		"timeout": {func(f *fakeWorld) {
			f.coord.ownerInput = func(*coordScript) {}
		}, "owner_decision_timeout"},
		"blank and ambiguous only": {func(f *fakeWorld) {
			f.coord.ownerInput = typeLines(f, "", "y", "YES", "ok go ahead")
		}, "owner_decision_timeout"},
		"coding before yes": {func(f *fakeWorld) { f.coord.dispatchEarlyCoder = true }, "coding_before_approval"},
		"unobserved coding before yes": {func(f *fakeWorld) {
			f.coord.ownerInput = func(c *coordScript) { c.extra(2) }
		}, "coding_before_approval"},
	}
	for name, tc := range failing {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			f := newFakeWorld(t)
			tc.setup(f)
			if code := f.attempt(); code != 1 || f.sup.failCode != tc.code {
				t.Fatalf("attempt = %d %q, want %s", code, f.sup.failCode, tc.code)
			}
			if name == "coding before yes" && !slices.Contains(f.coord.codes, CodeOwnerGateClosed) {
				t.Fatalf("observe codes %v", f.coord.codes)
			}
			if strings.HasPrefix(name, "coding") || strings.HasPrefix(name, "unobserved") {
				if len(f.plane.cancels) == 0 || !slices.Contains(eventTypesOf(t, f.bundle()), EvPrematureAdmission) {
					t.Fatalf("premature coding not cancelled: %v", f.plane.cancels)
				}
			} else if coderDispatched(f.plane) {
				t.Fatal("a coder task was dispatched")
			}
			r := f.report()
			if r.Result != ResultFail || criterion(r, CritOwner).Result != "fail" {
				t.Fatalf("report %v", failedCodes(r))
			}
		})
	}
	t.Run("input before any decision", func(t *testing.T) {
		t.Parallel()
		f := newFakeWorld(t)
		f.coord.before = func(c *coordScript) bool {
			if c.step == 0 {
				c.s.handleLine("yes "+c.s.runID, true)
			}
			return false
		}
		if code := f.attempt(); code != 0 || !strings.Contains(f.stdout.String(), "no decision is pending") {
			t.Fatalf("attempt = %d", code)
		}
	})
}

// TestObserveSemantics (UT-5): observation is idempotent for the same
// task, conflicting assignments fail the attempt, unknown tasks and role
// mismatches are refused without state change, malformed task IDs are
// invalid requests, plane errors are internal errors, and finish is
// acknowledged once (collection, never a forced PASS).
func TestObserveSemantics(t *testing.T) {
	t.Parallel()
	inject := func(at int, f func(c *coordScript) []string) (*fakeWorld, *[]string) {
		w := newFakeWorld(t)
		var got []string
		done := false
		w.coord.before = func(c *coordScript) bool {
			if c.step == at && !done {
				done = true
				got = f(c)
			}
			return false
		}
		return w, &got
	}
	obs := func(c *coordScript, task, hop string) string {
		return c.s.handleRequest(ControlRequest{Run: c.s.runID, Op: opObserve, Task: task, Hop: hop})
	}
	t.Run("idempotent and refusals", func(t *testing.T) {
		t.Parallel()
		w, got := inject(1, func(c *coordScript) []string {
			return []string{obs(c, c.tasks[0], HopDesigner), obs(c, "t_"+strings.Repeat("9", 32), HopDesignReviewer), obs(c, "t_bad", HopDesignReviewer),
				obs(c, c.s.preflight[1], HopDesignReviewer)}
		})
		code := w.attempt()
		want := []string{CodeAlreadyObserved, CodeTaskMismatch, CodeInvalidRequest, CodeObservationClash}
		if !slices.Equal(*got, want) || code != 1 || w.sup.failCode != "observation_conflict" {
			t.Fatalf("codes %v (want %v), attempt %d %q", *got, want, code, w.sup.failCode)
		}
	})
	t.Run("role mismatch", func(t *testing.T) {
		t.Parallel()
		w, got := inject(2, func(c *coordScript) []string { return []string{obs(c, c.tasks[0], HopDesignReviewer)} })
		w.attempt()
		if !slices.Equal(*got, []string{CodeObservationClash}) {
			t.Fatalf("codes %v", *got)
		}
	})
	t.Run("another task for an observed hop", func(t *testing.T) {
		t.Parallel()
		w, got := inject(1, func(c *coordScript) []string {
			c.extra(0)
			c.p.mu.Lock()
			id := c.p.order[len(c.p.order)-1]
			c.p.mu.Unlock()
			return []string{obs(c, id, HopDesigner)}
		})
		if code := w.attempt(); code != 1 || !slices.Equal(*got, []string{CodeObservationClash}) {
			t.Fatalf("codes %v attempt %d", *got, code)
		}
	})
	t.Run("plane error", func(t *testing.T) {
		t.Parallel()
		w, got := inject(1, func(c *coordScript) []string {
			c.p.showErr = func(string) error { return contract.New(contract.CodeUnavailable, "down") }
			defer func() { c.p.showErr = nil }()
			return []string{obs(c, "t_"+strings.Repeat("8", 32), HopDesignReviewer)}
		})
		w.attempt()
		if !slices.Equal(*got, []string{CodeInternalError}) {
			t.Fatalf("codes %v", *got)
		}
	})
	t.Run("role of another hop", func(t *testing.T) {
		t.Parallel()
		w, got := inject(2, func(c *coordScript) []string {
			c.extra(1)
			c.p.mu.Lock()
			id := c.p.order[len(c.p.order)-1]
			c.p.mu.Unlock()
			return []string{obs(c, id, HopCoder)}
		})
		w.attempt()
		if len(*got) != 1 || (*got)[0] != CodeTaskMismatch {
			t.Fatalf("codes %v", *got)
		}
	})
	t.Run("early finish", func(t *testing.T) {
		t.Parallel()
		w, got := inject(2, func(c *coordScript) []string {
			return []string{c.s.handleRequest(ControlRequest{Run: c.s.runID, Op: opFinish}), c.s.handleRequest(ControlRequest{Run: c.s.runID, Op: opFinish})}
		})
		if code := w.attempt(); code != 1 || !slices.Equal(*got, []string{CodeAccepted, CodeAccepted}) {
			t.Fatalf("codes %v attempt %d", *got, code)
		}
		types := eventTypesOf(t, w.bundle())
		if strings.Count(strings.Join(types, " "), EvFinishRequested) != 1 || w.report().Result != ResultFail {
			t.Fatal("finish forced something")
		}
	})
	t.Run("extra admission", func(t *testing.T) {
		t.Parallel()
		w := newFakeWorld(t)
		w.coord.extraAt = 0
		if code := w.attempt(); code != 1 || w.sup.failCode != "extra_admission" {
			t.Fatalf("attempt %d %q", code, w.sup.failCode)
		}
	})
}

// rawExchange sends raw bytes to a socket and returns its response line.
func rawExchange(t *testing.T, sock string, payload []byte) string {
	t.Helper()
	c, err := net.Dial("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	c.SetDeadline(time.Now().Add(10 * time.Second))
	c.Write(payload)
	line, _ := bufio.NewReader(c).ReadString('\n')
	return line
}

// TestControlProtocol (UT-5): one newline-delimited request per connection
// within its bounds; malformed, duplicate-key, unknown-key, null,
// extra-frame and unknown-operation requests are invalid; a stale run ID
// cannot reach the handler; responses carry only fixed codes; the helpers
// exit 0, 1 or 2.
func TestControlProtocol(t *testing.T) {
	t.Parallel()
	rt := shortTemp(t)
	const run = "20261010T120000Z-0a0b0c0d"
	var seen []ControlRequest
	srv, err := listenControl(rt, run, func(r ControlRequest) string {
		seen = append(seen, r)
		if r.Op == opObserve && r.Task == hexID("t_", 2) {
			return CodeOwnerGateClosed
		}
		return CodeAccepted
	})
	if err != nil {
		t.Fatal(err)
	}
	defer srv.close()
	sock := filepath.Join(rt, SocketName)
	if st, err := os.Stat(sock); err != nil || st.Mode().Perm() != 0o600 {
		t.Fatalf("socket mode %v %v", st, err)
	}
	ok := `{"ok":true,"code":"accepted"}` + "\n"
	invalid := `{"ok":false,"code":"invalid_request"}` + "\n"
	task := hexID("t_", 1)
	for name, tc := range map[string]struct{ in, want string }{
		"observe":        {`{"run":"` + run + `","op":"observe","task":"` + task + `","hop":"designer"}` + "\n", ok},
		"finish":         {`{"run":"` + run + `","op":"finish"}` + "\n", ok},
		"stale run":      {`{"run":"20200101T000000Z-00000000","op":"finish"}` + "\n", `{"ok":false,"code":"run_mismatch"}` + "\n"},
		"bad hop":        {`{"run":"` + run + `","op":"observe","task":"` + task + `","hop":"planner"}` + "\n", `{"ok":false,"code":"invalid_hop"}` + "\n"},
		"malformed":      {`{"run":` + "\n", invalid},
		"array":          {`[1]` + "\n", invalid},
		"null":           {`null` + "\n", invalid},
		"duplicate key":  {`{"run":"` + run + `","run":"` + run + `","op":"finish"}` + "\n", invalid},
		"unknown key":    {`{"run":"` + run + `","op":"finish","x":1}` + "\n", invalid},
		"finish task":    {`{"run":"` + run + `","op":"finish","task":"` + task + `"}` + "\n", invalid},
		"observe no hop": {`{"run":"` + run + `","op":"observe","task":"` + task + `"}` + "\n", invalid},
		"null task":      {`{"run":"` + run + `","op":"observe","task":null,"hop":"designer"}` + "\n", invalid},
		"number run":     {`{"run":1,"op":"finish"}` + "\n", invalid},
		"unknown op":     {`{"run":"` + run + `","op":"approve"}` + "\n", invalid},
		"extra frame":    {`{"run":"` + run + `","op":"finish"}` + "\n" + `{"run":"` + run + `","op":"finish"}` + "\n", invalid},
		"no newline":     {`{"run":"` + run + `","op":"finish"}`, invalid},
		"too long":       {`{"run":"` + strings.Repeat("x", maxControlLine) + `"}` + "\n", invalid},
	} {
		// Sequential: the subtests share the parent's server.
		t.Run(name, func(t *testing.T) {
			conn, err := net.Dial("unix", sock)
			if err != nil {
				t.Fatal(err)
			}
			defer conn.Close()
			conn.SetDeadline(time.Now().Add(10 * time.Second))
			conn.Write([]byte(tc.in))
			if name == "no newline" {
				conn.(*net.UnixConn).CloseWrite()
			}
			line, _ := bufio.NewReader(conn).ReadString('\n')
			if line != tc.want {
				t.Fatalf("%s = %q, want %q", name, line, tc.want)
			}
		})
	}
	if len(seen) != 2 {
		t.Fatalf("the handler saw %d requests: %+v", len(seen), seen)
	}
	// The helpers through MainWith.
	if err := os.WriteFile(filepath.Join(rt, RunFileName), []byte(`{"schema":"`+RunFileSchema+`","run_id":"`+run+`"}`+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	helperRun := func(args ...string) (int, string) {
		var out, errOut bytes.Buffer
		code := MainWith(Host{GOOS: "linux", Args: args, Getenv: func(string) string { return "" }, LookupEnv: func(string) (string, bool) { return "", true },
			Stdout: &out, Stderr: &errOut, NewWorld: func(Host) (*World, func(), error) { t.Fatal("world built"); return nil, nil, nil }})
		return code, out.String() + errOut.String()
	}
	if code, out := helperRun("observe", "--run", rt, "--task", task, "--hop", HopDesigner); code != 0 || out != "accepted\n" {
		t.Fatalf("observe = %d %q", code, out)
	}
	if code, out := helperRun("observe", "--run", rt, "--task", hexID("t_", 2), "--hop", HopCoder); code != 1 || out != "owner_gate_closed\n" {
		t.Fatalf("refused observe = %d %q", code, out)
	}
	if code, out := helperRun("finish", "--run", rt); code != 0 || out != "accepted\n" {
		t.Fatalf("finish = %d %q", code, out)
	}
	other := shortTemp(t)
	if code, _ := helperRun("finish", "--run", other); code != 2 {
		t.Fatal("finish without a run file did not fail")
	}
	os.WriteFile(filepath.Join(other, RunFileName), []byte(`{"schema":"x","run_id":"y"}`), 0o600)
	if code, _ := helperRun("finish", "--run", other); code != 2 {
		t.Fatal("an invalid run file passed")
	}
	os.WriteFile(filepath.Join(other, RunFileName), []byte(`{"schema":"`+RunFileSchema+`","run_id":"`+run+`"}`), 0o600)
	if code, out := helperRun("finish", "--run", other); code != 2 || !strings.Contains(out, "cannot reach the supervisor") {
		t.Fatalf("no supervisor = %d %q", code, out)
	}
	long := filepath.Join(other, strings.Repeat("l", 90))
	os.Mkdir(long, 0o700)
	os.WriteFile(filepath.Join(long, RunFileName), []byte(`{"schema":"`+RunFileSchema+`","run_id":"`+run+`"}`), 0o600)
	if code, out := helperRun("finish", "--run", long); code != 2 || !strings.Contains(out, "90 bytes") {
		t.Fatalf("long socket = %d %q", code, out)
	}
	if _, err := listenControl(long, run, nil); err == nil {
		t.Fatal("a long socket path was served")
	}
	// A server answering garbage or an unknown code is a transport error.
	for name, answer := range map[string]string{"garbage": "nope\n", "unknown code": `{"ok":true,"code":"approved"}` + "\n",
		"no answer": "", "refusal as ok": `{"ok":true,"code":"closing"}` + "\n"} {
		dir := shortTemp(t)
		os.WriteFile(filepath.Join(dir, RunFileName), []byte(`{"schema":"`+RunFileSchema+`","run_id":"`+run+`"}`), 0o600)
		ln, err := net.Listen("unix", filepath.Join(dir, SocketName))
		if err != nil {
			t.Fatal(err)
		}
		go func() {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			bufio.NewReader(c).ReadString('\n')
			c.Write([]byte(answer))
			c.Close()
		}()
		if _, _, err := controlCall(dir, opFinish, "", ""); err == nil {
			t.Errorf("%s: accepted", name)
		}
		ln.Close()
	}
}

// TestControlForwarding (UT-5): the supervisor forwards requests to its
// loop; while closing it acknowledges finish and refuses observation.
func TestControlForwarding(t *testing.T) {
	s := &supervisor{reqs: make(chan ctrlMsg), loopDone: make(chan struct{})}
	go func() {
		m := <-s.reqs
		m.reply <- CodeAlreadyObserved
	}()
	if code := s.handleControl(ControlRequest{Op: opObserve}); code != CodeAlreadyObserved {
		t.Fatal(code)
	}
	close(s.loopDone)
	if s.handleControl(ControlRequest{Op: opFinish}) != CodeAccepted || s.handleControl(ControlRequest{Op: opObserve}) != CodeClosing {
		t.Fatal("loop done")
	}
	s.closing.Store(true)
	if s.handleControl(ControlRequest{Op: opFinish}) != CodeAccepted || s.handleControl(ControlRequest{Op: opObserve}) != CodeClosing {
		t.Fatal("closing")
	}
	// A loop that never answers is an internal error, not a hang.
	// (The shortened wait applies to these two cases only.)
	saved := forwardWait
	forwardWait = 20 * time.Millisecond
	stuck := &supervisor{reqs: make(chan ctrlMsg, 1), loopDone: make(chan struct{})}
	stuckCode := stuck.handleControl(ControlRequest{Op: opObserve})
	busy := &supervisor{reqs: make(chan ctrlMsg), loopDone: make(chan struct{})}
	busyCode := busy.handleControl(ControlRequest{Op: opObserve})
	forwardWait = saved
	if stuckCode != CodeInternalError || busyCode != CodeInternalError {
		t.Fatal(stuckCode, busyCode)
	}
	// The live loop serves a socket request end to end.
	f := newFakeWorld(t)
	result := make(chan string, 1)
	started, answered := false, false
	f.coord.before = func(c *coordScript) bool {
		if c.step != 9 || !c.s.complete || answered {
			return false
		}
		if !started {
			started = true
			go func() {
				code, ok, err := controlCall(c.s.runtime, opObserve, c.tasks[0], HopDesigner)
				if !ok || err != nil {
					code = "error"
				}
				result <- code
			}()
		}
		select {
		case code := <-result:
			answered = true
			if code != CodeAlreadyObserved {
				t.Errorf("socket observe = %q", code)
			}
			return false
		default:
			// Hold the fake time while the real socket round trip
			// completes, so no fake deadline can race it, and yield.
			f.clock.holdNext()
			time.Sleep(time.Millisecond)
			return true
		}
	}
	if code := f.attempt(); code != 0 || !answered {
		t.Fatalf("attempt = %d answered %v", code, answered)
	}
}
