package reale2e

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/go-git/go-git/v5/plumbing"

	"github.com/wedevwork/callsheet/internal/contract"
)

// TestBackgroundWaitEvidence (FP-6): background waits whose completion was
// consumed (with at least one automatic wake) pass; a long blocking MCP
// call, polling only, a foreground wait or an absent wake fail; the
// supervisor records each wait from the files its own redirected command
// wrote, so a killed or failed wait fails too.
func TestBackgroundWaitEvidence(t *testing.T) {
	t.Parallel()
	if code, r, _ := checkBundle(t, passingBundle(t)); code != 0 || criterion(r, CritWaits).Result != "pass" {
		t.Fatalf("background waits = %d %v", code, failedCodes(r))
	}
	for name, m := range map[string]struct {
		code string
		f    func(t *testing.T, b string)
	}{
		"long blocking call": {"unqualified_long_poll", func(t *testing.T, b string) {
			editDoc(t, b, "session/event-map.json", func(m *EventMap) {
				m.Events = append(m.Events, MapEvent{Kind: MapMCPWait, BudgetMS: 300000, Recovery: true, Lines: []int{2, 2}})
			})
		}},
		"polling only": {"automatic_wake_unobserved", func(t *testing.T, b string) {
			editDoc(t, b, "session/event-map.json", func(m *EventMap) {
				for i := range m.Events {
					m.Events[i].AutomaticWake = false
				}
				m.Events = append(m.Events, MapEvent{Kind: MapMCPWait, BudgetMS: 10000, Recovery: true, Lines: []int{2, 2}})
			})
		}},
		"foreground wait": {"wait_not_background", func(t *testing.T, b string) {
			editDoc(t, b, "session/event-map.json", func(m *EventMap) {
				for i := range m.Events {
					m.Events[i].Background = false
				}
			})
		}},
		"absent wake": {"automatic_wake_unobserved", func(t *testing.T, b string) {
			editDoc(t, b, "session/event-map.json", func(m *EventMap) {
				for i := range m.Events {
					m.Events[i].UserMessageBetween = true
				}
			})
		}},
	} {
		b := passingBundle(t)
		m.f(t, b)
		code, r, _ := checkBundle(t, b)
		requireCode(t, name, code, r, CritWaits, m.code)
	}
	// A2: the relay the coordinator's background waits use.
	t.Run("relay", testWaitRelay)
	// A2.1: a wait meeting a loop busy with the final tests.
	t.Run("busy final tests then terminal", testBusyFinalTests)
	t.Run("taken fetch failure", testTakenFetchFailure)
	t.Run("busy lifecycle bounds", testBusyLifecycleBounds)
	t.Run("lifecycle at reply selection", testReplyLifecycle)
	// The live collector's wait records.
	for name, tc := range map[string]struct {
		exit string
		prep func(dir string)
		code string
	}{
		"failed wait":     {"7", nil, "wait_unsuccessful"},
		"killed wait":     {"", func(dir string) { os.Remove(filepath.Join(dir, "exit-code")) }, "wait_unsuccessful"},
		"unreadable wait": {"0", func(dir string) { os.WriteFile(filepath.Join(dir, "result.json"), []byte("{"), 0o600) }, "wait_binding_mismatch"},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			f := newFakeWorld(t)
			f.coord.waitExit = tc.exit
			if tc.prep != nil {
				f.coord.mutateOwner = func(owner string) { tc.prep(filepath.Join(filepath.Dir(owner), "waits", "02")) }
			}
			if code := f.attempt(); code != 1 {
				t.Fatalf("attempt = %d", code)
			}
			r := f.report()
			if c := criterion(r, CritWaits); c.Result != "fail" || !containsCode(c.Codes, tc.code) {
				t.Fatalf("waits %+v", c)
			}
		})
	}
}

// testWaitRelay (FP-6, amendment A2) runs the public wait command against a
// live supervisor socket with an injected plane and clocks: pending to
// success, a worker's own failure retained with helper exit 0, a terminal
// but not yet durable task kept pending, every refusal and transport
// failure as exit 1 with no stdout (the shell redirect's empty result and
// "1"), usage as exit 2, no events and no plane access from the helper.
func testWaitRelay(t *testing.T) {
	ctx := context.Background()
	t.Run("pending to success", func(t *testing.T) {
		t.Parallel()
		r := newWaitRig(t)
		r.observe(0)
		r.serve()
		before := r.eventCount()
		c := &pollClock{onPoll: func(n int) {
			if n == 2 {
				r.finish(0, contract.TaskSucceeded)
			}
		}}
		w := r.wait(c, 0)
		v, _ := r.p.fakePlane.ShowTask(ctx, r.tasks[0], 0)
		want, err := contract.EncodeWaitResponse(contract.WaitResponse{Version: contract.ProtocolVersion, Status: contract.WaitTerminal, Winner: r.tasks[0], Task: &v})
		if err != nil || w.code != 0 || w.errOut != "" || w.out != string(want)+"\n" || w.exitCodeFile() != "0\n" {
			t.Fatalf("wait = %d %q %q (%v)", w.code, w.out, w.errOut, err)
		}
		wr, err := contract.ParseWaitResponse([]byte(strings.TrimSuffix(w.out, "\n")), []string{r.tasks[0]}, 40*time.Second)
		if err != nil || wr.EffectiveWaitMS != 0 || wr.Winner != r.tasks[0] || wr.Task.State != contract.TaskSucceeded {
			t.Fatalf("the result does not parse as the collector reads it: %v %+v", err, wr)
		}
		// Two pending answers, one 250ms poll after each; one supervisor
		// fetch per exchange; no event, dispatch or cancellation.
		if c.pollCount() != 2 || r.p.fetches() != 3 || r.eventCount() != before || r.p.n != 4 || len(r.p.cancels) != 0 {
			t.Fatalf("polls %d fetches %d events %d/%d dispatches %d cancels %v", c.pollCount(), r.p.fetches(), r.eventCount(), before, r.p.n, r.p.cancels)
		}
	})
	t.Run("worker exit 7", func(t *testing.T) {
		t.Parallel()
		r := newWaitRig(t)
		r.observe(0)
		r.serve()
		r.finish(0, contract.TaskFailed)
		r.p.edit(r.tasks[0], func(v *contract.TaskView) {
			res := *v.Result
			res.ExitCode = ptr(7)
			v.Result = &res
		})
		w := r.wait(&pollClock{}, 0)
		wr, err := contract.ParseWaitResponse([]byte(strings.TrimSuffix(w.out, "\n")), []string{r.tasks[0]}, 40*time.Second)
		if w.code != 0 || err != nil || wr.Task.State != contract.TaskFailed || *wr.Task.Result.ExitCode != 7 {
			t.Fatalf("a failed worker's wait = %d %q %v", w.code, w.errOut, err)
		}
	})
	t.Run("terminal not durable", func(t *testing.T) {
		t.Parallel()
		r := newWaitRig(t)
		r.observe(0)
		r.serve()
		r.finish(0, contract.TaskSucceeded)
		r.p.edit(r.tasks[0], func(v *contract.TaskView) { v.DurabilityConfirmed = false })
		c := &pollClock{onPoll: func(int) { r.p.edit(r.tasks[0], func(v *contract.TaskView) { v.DurabilityConfirmed = true }) }}
		if w := r.wait(c, 0); w.code != 0 || c.pollCount() != 1 {
			t.Fatalf("wait = %d after %d polls %q", w.code, c.pollCount(), w.errOut)
		}
	})
	t.Run("coder with the owner's receipt", func(t *testing.T) {
		t.Parallel()
		r := newWaitRig(t)
		r.observe(2)
		r.s.decision = &DecisionRecord{Decision: DecisionYes}
		r.serve()
		r.finish(2, contract.TaskSucceeded)
		if w := r.wait(&pollClock{}, 2); w.code != 0 {
			t.Fatalf("coder wait = %d %q", w.code, w.errOut)
		}
	})
	// Refusals and failures: exit 1, a fixed diagnostic, no stdout.
	for name, tc := range map[string]struct {
		prep func(r *waitRig) (task, hop string)
		want string
	}{
		"unobserved task": {func(r *waitRig) (string, string) { return r.tasks[0], HopDesigner }, "refused: task_mismatch"},
		"foreign task": {func(r *waitRig) (string, string) {
			r.observe(0)
			return hexID("t_", 99), HopDesigner
		}, "refused: task_mismatch"},
		"another hop's task": {func(r *waitRig) (string, string) {
			r.observe(0)
			return r.tasks[0], HopDesignReviewer
		}, "refused: observation_conflict"},
		"missing owner receipt": {func(r *waitRig) (string, string) {
			r.observe(2)
			return r.tasks[2], HopCoder
		}, "refused: owner_gate_closed"},
		"wrong base": {func(r *waitRig) (string, string) {
			r.observe(0)
			r.p.edit(r.tasks[0], func(v *contract.TaskView) { v.WorkspaceBinding.BaseCommit = ptr(rigBases[1]) })
			return r.tasks[0], HopDesigner
		}, "refused: task_mismatch"},
		"wrong instance": {func(r *waitRig) (string, string) {
			r.observe(0)
			r.p.edit(r.tasks[0], func(v *contract.TaskView) { v.WorkspaceBinding.Instance = hexID("", 0xdef) })
			return r.tasks[0], HopDesigner
		}, "refused: task_mismatch"},
		"wrong role": {func(r *waitRig) (string, string) {
			r.observe(0)
			r.p.edit(r.tasks[0], func(v *contract.TaskView) { v.Role.ID = HopCoder })
			return r.tasks[0], HopDesigner
		}, "refused: task_mismatch"},
		"wrong run marker": {func(r *waitRig) (string, string) {
			r.observe(0)
			r.p.edit(r.tasks[0], func(v *contract.TaskView) {
				v.Request.Payload = []string{markerFor("20200101T000000Z-00000000", HopDesigner)}
			})
			return r.tasks[0], HopDesigner
		}, "refused: task_mismatch"},
		"foreign run": {func(r *waitRig) (string, string) {
			r.observe(0)
			r.runID = "20200101T000000Z-00000000"
			r.writeRunFile()
			return r.tasks[0], HopDesigner
		}, "refused: run_mismatch"},
		"closing": {func(r *waitRig) (string, string) {
			r.observe(0)
			r.s.closing.Store(true)
			return r.tasks[0], HopDesigner
		}, "refused: closing"},
		"attempt deadline": {func(r *waitRig) (string, string) {
			r.observe(0)
			r.clock.set(r.s.attempt.Add(AttemptBound + time.Second))
			return r.tasks[0], HopDesigner
		}, "refused: closing"},
		"deadline during the fetch": {func(r *waitRig) (string, string) {
			// A terminal task whose fetch outlives the attempt: the
			// observed deadline takes precedence over terminal output.
			r.observe(0)
			r.finish(0, contract.TaskSucceeded)
			r.p.onFetch = func() { r.clock.set(r.s.attempt.Add(AttemptBound + time.Second)) }
			return r.tasks[0], HopDesigner
		}, "refused: closing"},
		"failed fetch": {func(r *waitRig) (string, string) {
			r.observe(0)
			r.p.fetchErr = context.Canceled
			return r.tasks[0], HopDesigner
		}, "refused: internal_error"},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			r := newWaitRig(t)
			task, hop := tc.prep(r)
			if r.s.ctl == nil {
				r.listen()
				r.startLoop()
				if _, err := os.Stat(filepath.Join(r.rt, RunFileName)); err != nil {
					r.writeRunFile()
				}
			}
			before := r.eventCount()
			w := runWait(t, helperWorld(&pollClock{}), "--run", r.rt, "--task", task, "--hop", hop)
			requireWaitError(t, name, w, tc.want)
			if name != "attempt deadline" && name != "deadline during the fetch" && r.eventCount() != before {
				t.Errorf("%s: a refused wait recorded events", name)
			}
		})
	}
	t.Run("supervisor gone", func(t *testing.T) {
		t.Parallel()
		r := newWaitRig(t)
		requireWaitError(t, "no run file", r.wait(&pollClock{}, 0), "cannot read the run file")
		r.writeRunFile()
		requireWaitError(t, "before the wait", r.wait(&pollClock{}, 0), "cannot reach the supervisor")
		r.observe(0)
		r.serve()
		c := &pollClock{onPoll: func(int) { r.shutdown() }}
		requireWaitError(t, "during the wait", r.wait(c, 0), "cannot reach the supervisor")
		long := filepath.Join(shortTemp(t), strings.Repeat("l", 90))
		os.Mkdir(long, 0o700)
		os.WriteFile(filepath.Join(long, RunFileName), []byte(`{"schema":"`+RunFileSchema+`","run_id":"`+rigRun+`"}`), 0o600)
		requireWaitError(t, "long socket", runWait(t, helperWorld(&pollClock{}), "--run", long, "--task", r.tasks[0], "--hop", HopDesigner), "90 bytes")
	})
	t.Run("closing cancels the in-flight fetch", func(t *testing.T) {
		t.Parallel()
		r := newWaitRig(t)
		r.observe(0)
		r.p.block = true
		r.serve()
		res := make(chan waitRun, 1)
		go func() { res <- r.wait(&pollClock{}, 0) }()
		<-r.p.started
		r.s.ctl.close()
		// Cancelled by the close, not by the 4s handoff deadline.
		if err := <-r.p.blocked; !errors.Is(err, context.Canceled) {
			t.Fatalf("the fetch ended with %v", err)
		}
		requireWaitError(t, "closed during the fetch", <-res, "refused: internal_error")
	})
	t.Run("exchange timeout, ceiling and cancellation", func(t *testing.T) {
		t.Parallel()
		silent := fakeWaitServer(t, func(int, []byte) []byte { return nil })
		ww := helperWorld(&pollClock{})
		ww.Exchange = 100 * time.Millisecond
		requireWaitError(t, "silent supervisor", runWait(t, ww, "--run", silent, "--task", hexID("t_", 1), "--hop", HopDesigner), "timed out")
		var exchanges atomic.Int32
		pending := fakeWaitServer(t, func(int, []byte) []byte {
			exchanges.Add(1)
			return []byte(`{"ok":true,"code":"pending"}` + "\n")
		})
		requireWaitError(t, "ceiling", runWait(t, helperWorld(&pollClock{jumpAfter: 3}), "--run", pending, "--task", hexID("t_", 1), "--hop", HopDesigner),
			"90m wait ceiling")
		if exchanges.Load() != 1 {
			t.Fatalf("%d exchanges before the ceiling", exchanges.Load())
		}
		cctx, cancel := context.WithCancel(ctx)
		ww = helperWorld(&pollClock{onPoll: func(int) { cancel() }})
		ww.Context = cctx
		requireWaitError(t, "cancelled while pending", runWait(t, ww, "--run", pending, "--task", hexID("t_", 1), "--hop", HopDesigner), "cancelled")
		cctx, cancel = context.WithCancel(ctx)
		held := fakeWaitServer(t, func(int, []byte) []byte {
			cancel()
			return nil
		})
		ww = helperWorld(&pollClock{})
		ww.Context = cctx
		requireWaitError(t, "cancelled in an exchange", runWait(t, ww, "--run", held, "--task", hexID("t_", 1), "--hop", HopDesigner), "cancelled")
		ww.Context = cctx
		requireWaitError(t, "cancelled before", runWait(t, ww, "--run", held, "--task", hexID("t_", 1), "--hop", HopDesigner), "cancelled")
	})
	t.Run("usage and output", func(t *testing.T) {
		t.Parallel()
		task := hexID("t_", 1)
		for _, args := range [][]string{{}, {"--run", "relative", "--task", task, "--hop", HopDesigner}, {"--run", "/r", "--task", "bad", "--hop", HopDesigner},
			{"--run", "/r", "--task", task, "--hop", "planner"}, {"--run", "/r", "--task", task}, {"--run", "/r", "--task", task, "--hop", HopDesigner, "extra"}} {
			w := runWait(t, helperWorld(&pollClock{}), args...)
			if w.code != 2 || w.out != "" || w.exitCodeFile() != "2\n" || !strings.Contains(w.errOut, "usage:") {
				t.Errorf("%v = %d %q %q", args, w.code, w.out, w.errOut)
			}
		}
		r := newWaitRig(t)
		r.observe(0)
		r.serve()
		r.finish(0, contract.TaskSucceeded)
		var errOut bytes.Buffer
		code := MainWith(Host{Args: []string{"wait", "--run", r.rt, "--task", r.tasks[0], "--hop", HopDesigner}, Stdout: failingWriter{}, Stderr: &errOut,
			NewWaitWorld: func() (WaitWorld, func()) { return helperWorld(&pollClock{}), func() {} }})
		if code != 1 || !strings.Contains(errOut.String(), "cannot write the result") {
			t.Fatalf("write failure = %d %q", code, errOut.String())
		}
		// The live wait world: cancellable, the wall clock, the 5s budget.
		ww, release := LiveWaitWorld()
		if ww.Context == nil || ww.Exchange != controlExchange || ww.Clock == nil {
			t.Fatalf("live wait world %+v", ww)
		}
		release()
		if ww.Context.Err() == nil {
			t.Fatal("releasing the live wait world did not cancel it")
		}
	})
}

// TestWaitProtocol (UT-3/UT-5 wait protocol, amendment A2): the server
// executes a wait only after EOF follows its single request (buffered and
// delayed extra frames, invalid task IDs, hops and keys are refused), the
// request line keeps its 4096-byte bound, a terminal answer above 4096
// bytes reaches the helper intact, and the helper accepts exactly the
// response union: pending and refusals within 4096 bytes, terminal within
// the wait bound with a nested result within the contract's, nothing
// after the line, no malformed, duplicate or unknown key.
func TestWaitProtocol(t *testing.T) {
	t.Parallel()
	r := newWaitRig(t)
	r.observe(0)
	// A gate armed for one connection holds the server after it read the
	// wait request's frame and before it requires EOF (review round 1 W1).
	type gate struct{ entered, release chan struct{} }
	gates := make(chan gate, 1)
	r.serverOpts = append(r.serverOpts, func(s *controlServer) {
		s.frameRead = func() {
			select {
			case g := <-gates:
				close(g.entered)
				<-g.release
			default:
			}
		}
	})
	r.serve()
	sock := filepath.Join(r.rt, SocketName)
	raw := func(first, later string, halfClose bool) string {
		var g gate
		if later != "" {
			g = gate{entered: make(chan struct{}), release: make(chan struct{})}
			gates <- g
		}
		conn, err := net.Dial("unix", sock)
		if err != nil {
			t.Fatal(err)
		}
		defer conn.Close()
		// An I/O safeguard only; no case waits for it.
		conn.SetDeadline(time.Now().Add(10 * time.Second))
		conn.Write([]byte(first))
		if later != "" {
			// A frame that arrives after the server read the first.
			<-g.entered
			conn.Write([]byte(later))
			close(g.release)
		}
		if halfClose {
			conn.(*net.UnixConn).CloseWrite()
		}
		b, _ := io.ReadAll(conn)
		return string(b)
	}
	req := func(task, hop string) string {
		return `{"run":"` + rigRun + `","op":"wait","task":"` + task + `","hop":"` + hop + `"}` + "\n"
	}
	invalid := `{"ok":false,"code":"invalid_request"}` + "\n"
	pending := `{"ok":true,"code":"pending"}` + "\n"
	ok := req(r.tasks[0], HopDesigner)
	// 4096 bytes with the newline: JSON whitespace pads the line.
	padded := strings.TrimSuffix(ok, "}\n") + strings.Repeat(" ", maxControlLine-len(ok)) + "}\n"
	for name, tc := range map[string]struct {
		first, later string
		want         string
	}{
		"pending":              {ok, "", pending},
		"at the request bound": {padded, "", pending},
		"over the bound":       {strings.TrimSuffix(padded, "}\n") + " }\n", "", invalid},
		"buffered extra frame": {ok + ok, "", invalid},
		"delayed extra frame":  {ok, ok, invalid},
		"stray byte":           {ok, " ", invalid},
		"invalid task":         {req("t_bad", HopDesigner), "", invalid},
		"invalid hop":          {req(r.tasks[0], "planner"), "", `{"ok":false,"code":"invalid_hop"}` + "\n"},
		"run mismatch":         {strings.Replace(ok, rigRun, "20200101T000000Z-00000000", 1), "", `{"ok":false,"code":"run_mismatch"}` + "\n"},
		"unknown key":          {strings.Replace(ok, `"op"`, `"x":1,"op"`, 1), "", invalid},
		"duplicate key":        {strings.Replace(ok, `"op":"wait"`, `"op":"wait","op":"wait"`, 1), "", invalid},
		"missing hop":          {`{"run":"` + rigRun + `","op":"wait","task":"` + r.tasks[0] + `"}` + "\n", "", invalid},
		"premature EOF":        {strings.TrimSuffix(ok, "\n"), "", invalid},
	} {
		if got := raw(tc.first, tc.later, true); got != tc.want {
			t.Errorf("%s = %q, want %q", name, got, tc.want)
		}
	}
	// A server without a wait handler knows no wait operation.
	plain := shortTemp(t)
	srv, err := listenControl(plain, rigRun, func(ControlRequest) string { return CodeAccepted })
	if err != nil {
		t.Fatal(err)
	}
	conn, _ := net.Dial("unix", filepath.Join(plain, SocketName))
	conn.Write([]byte(ok))
	conn.(*net.UnixConn).CloseWrite()
	if b, _ := io.ReadAll(conn); string(b) != invalid {
		t.Errorf("no wait handler = %q", b)
	}
	conn.Close()
	srv.close()
	// A terminal answer above 4096 bytes arrives whole.
	big := strings.Repeat("review line\n", 1000) + "STATUS: DESIGN_DRAFT\n"
	c := plumbing.NewHash(rigBases[1])
	r.p.finish(r.tasks[0], contract.TaskSucceeded, &big, false, &c)
	line := raw(ok, "", true)
	if len(line) <= maxControlLine || !strings.HasPrefix(line, `{"ok":true,"code":"terminal","result":{"version":`) {
		t.Fatalf("terminal line of %d bytes: %.80q", len(line), line)
	}
	w := r.wait(&pollClock{}, 0)
	if w.code != 0 || !strings.Contains(w.out, "review line") || len(w.out) <= maxControlLine {
		t.Fatalf("large terminal wait = %d %d bytes %q", w.code, len(w.out), w.errOut)
	}
	// The helper's request: exactly the four keys, then a half-close (the
	// server reads it to EOF).
	result := strings.TrimSuffix(strings.TrimPrefix(strings.TrimSuffix(line, "\n"), `{"ok":true,"code":"terminal","result":`), "}")
	task := r.tasks[0]
	var seen []byte
	terminal := func(pad int) []byte {
		return []byte(`{"ok":true,"code":"terminal","result":` + result + strings.Repeat(" ", pad) + "}\n")
	}
	got := runWait(t, helperWorld(&pollClock{}), "--run", fakeWaitServer(t, func(_ int, req []byte) []byte {
		seen = req
		return terminal(0)
	}), "--task", task, "--hop", HopDesigner)
	if got.code != 0 || got.out != w.out || string(seen) != `{"hop":"designer","op":"wait","run":"`+rigRun+`","task":"`+task+`"}`+"\n" {
		t.Fatalf("request %q, wait %d %q", seen, got.code, got.errOut)
	}
	// Response bounds and shape.
	base := len(terminal(0))
	refusal := `{"ok":false,"code":"closing"}` + "\n"
	padLine := func(l string, size int) string {
		return strings.TrimSuffix(l, "}\n") + strings.Repeat(" ", size-len(l)) + "}\n"
	}
	nested := `{"ok":true,"code":"terminal","result":{` + strings.Repeat(" ", contract.MaxWaitResponseBytes-len(result)+1) + result[1:] + "}\n"
	stillRunning := `{"ok":true,"code":"terminal","result":{"version":1,"status":"still_running","effective_wait_ms":0,"tasks":[{"task_id":"` + task +
		`","state":"running","elapsed_ms":1,"last_log_line":"","log_truncated":false,"durability_confirmed":false}]}}` + "\n"
	for name, tc := range map[string]struct {
		answers []string
		want    string
	}{
		"pending at 4096 then a refusal": {[]string{padLine(pending, maxControlLine), refusal}, "refused: closing"},
		"pending over 4096":              {[]string{padLine(pending, maxControlLine+1)}, "oversized"},
		"refusal over 4096":              {[]string{padLine(refusal, maxControlLine+1)}, "oversized"},
		"terminal over the line bound":   {[]string{string(terminal(maxWaitLine + 1 - base))}, "oversized"},
		"nested result over its bound":   {[]string{nested}, "oversized"},
		"still running as terminal":      {[]string{stillRunning}, "invalid wait result"},
		"another winner":                 {[]string{strings.ReplaceAll(string(terminal(0)), task, hexID("t_", 77))}, "invalid wait result"},
		"terminal without result":        {[]string{`{"ok":true,"code":"terminal"}` + "\n"}, "malformed"},
		"pending with result":            {[]string{`{"ok":true,"code":"pending","result":{}}` + "\n"}, "malformed"},
		"refusal as ok":                  {[]string{`{"ok":true,"code":"closing"}` + "\n"}, "malformed"},
		"pending as refusal":             {[]string{`{"ok":false,"code":"pending"}` + "\n"}, "malformed"},
		"unknown code":                   {[]string{`{"ok":false,"code":"approved"}` + "\n"}, "malformed"},
		"observe code":                   {[]string{`{"ok":true,"code":"accepted"}` + "\n"}, "malformed"},
		"unknown key":                    {[]string{`{"ok":true,"code":"pending","x":1}` + "\n"}, "malformed"},
		"duplicate key":                  {[]string{`{"ok":true,"ok":true,"code":"pending"}` + "\n"}, "malformed"},
		"missing ok":                     {[]string{`{"code":"pending"}` + "\n"}, "malformed"},
		"string ok":                      {[]string{`{"ok":"true","code":"pending"}` + "\n"}, "malformed"},
		"null ok":                        {[]string{`{"ok":null,"code":"pending"}` + "\n"}, "malformed"},
		"null code":                      {[]string{`{"ok":true,"code":null}` + "\n"}, "malformed"},
		"array":                          {[]string{`[]` + "\n"}, "malformed"},
		"trailing data":                  {[]string{`{"ok":true,"code":"pending"} 1` + "\n"}, "malformed"},
		"invalid UTF-8":                  {[]string{`{"ok":false,"code":"clos` + "\xff" + `"}` + "\n"}, "malformed"},
		"extra frame":                    {[]string{pending + pending}, "malformed"},
		"premature EOF":                  {[]string{`{"ok":true,"code":"pending"}`}, "no complete response"},
		"no answer":                      {[]string{""}, "no complete response"},
	} {
		w := runWait(t, helperWorld(&pollClock{}), "--run", fakeWaitServer(t, func(n int, _ []byte) []byte {
			if n > len(tc.answers) {
				return []byte(refusal)
			}
			return []byte(tc.answers[n-1])
		}), "--task", task, "--hop", HopDesigner)
		requireWaitError(t, name, w, tc.want)
	}
	// Exactly at the line bound, a padded terminal answer is accepted.
	at := runWait(t, helperWorld(&pollClock{}), "--run", fakeWaitServer(t, func(int, []byte) []byte { return terminal(maxWaitLine - base) }),
		"--task", task, "--hop", HopDesigner)
	if at.code != 0 || at.out != w.out {
		t.Fatalf("terminal at the line bound = %d %q", at.code, at.errOut)
	}
}

// TestWaitForwarding (UT-5, amendment A2): a wait is forwarded to the loop
// within its shared context: refused with closing while closing or once
// the loop ended, and an internal error when the handoff or the loop's
// answer does not fit the context.
func TestWaitForwarding(t *testing.T) {
	t.Parallel()
	req := ControlRequest{Run: rigRun, Op: opWait, Task: hexID("t_", 1), Hop: HopDesigner}
	clock := &manualClock{now: time.Date(2026, 10, 11, 12, 0, 0, 0, time.UTC)}
	newSup := func() *supervisor {
		return &supervisor{reqs: make(chan ctrlMsg), loopDone: make(chan struct{}), w: &World{Clock: clock}, life: context.Background(), attempt: clock.Now()}
	}
	cancelled := func(cause error) context.Context {
		ctx, cancel := context.WithCancelCause(context.Background())
		cancel(cause)
		return ctx
	}
	// A2.1: the budget's policy values: 4s of work within the 5s exchange,
	// 1s reserved for the answer.
	if waitHandoff != 4*time.Second || controlExchange != 5*time.Second || controlExchange-waitHandoff != time.Second {
		t.Fatalf("budget %v of %v", waitHandoff, controlExchange)
	}
	// Untaken when the work budget expires: the loop is busy (pending).
	if r := newSup().handleWaitControl(cancelled(errWorkExpired), req); r.code != CodePending {
		t.Fatalf("untaken expiry = %+v", r)
	}
	// Untaken, cancelled otherwise (the server closed): an internal error.
	if r := newSup().handleWaitControl(cancelled(context.Canceled), req); r.code != CodeInternalError {
		t.Fatalf("untaken cancellation = %+v", r)
	}
	// Taken, then the budget expires before the loop answers: an internal
	// error, never pending.
	taken := newSup()
	ctx, expire := context.WithCancelCause(context.Background())
	accepted := make(chan struct{})
	go func() {
		<-taken.reqs
		expire(errWorkExpired)
		close(accepted)
	}()
	if r := taken.handleWaitControl(ctx, req); r.code != CodeInternalError {
		t.Fatalf("taken expiry = %+v", r)
	}
	<-accepted
	// Taken and answered: the loop's answer stands.
	answered := make(chan struct{})
	go func() {
		defer close(answered)
		m := <-taken.reqs
		m.wait <- waitReply{code: CodePending}
	}()
	if r := taken.handleWaitControl(context.Background(), req); r.code != CodePending {
		t.Fatalf("taken = %+v", r)
	}
	<-answered
	// Review round 1 C1: a taken request's answer is selected with
	// lifecycle precedence. A terminal answer computed as the lifecycle
	// closes, the loop ending without an answer, and the budget expiring
	// together with closing all answer closing.
	terminal := waitReply{code: CodeTerminal, result: []byte("{}")}
	for name, loop := range map[string]func(s *supervisor, m ctrlMsg, expire context.CancelCauseFunc){
		"answered as closing begins": func(s *supervisor, m ctrlMsg, _ context.CancelCauseFunc) {
			s.closing.Store(true)
			m.wait <- terminal
		},
		"answered as the supervisor is cancelled": func(s *supervisor, m ctrlMsg, _ context.CancelCauseFunc) {
			m.wait <- terminal
		},
		"answered as the attempt expires": func(s *supervisor, m ctrlMsg, _ context.CancelCauseFunc) {
			s.attempt = clock.Now().Add(-AttemptBound - time.Nanosecond)
			m.wait <- terminal
		},
		"loop ended unanswered": func(s *supervisor, _ ctrlMsg, _ context.CancelCauseFunc) { close(s.loopDone) },
		"budget expired while closing": func(s *supervisor, _ ctrlMsg, expire context.CancelCauseFunc) {
			s.closing.Store(true)
			expire(errWorkExpired)
		},
	} {
		s := newSup()
		life, cancelLife := context.WithCancel(context.Background())
		s.life = life
		ctx, expire := context.WithCancelCause(context.Background())
		done := make(chan struct{})
		go func() {
			defer close(done)
			m := <-s.reqs
			if name == "answered as the supervisor is cancelled" {
				cancelLife()
			}
			loop(s, m, expire)
		}()
		if r := s.handleWaitControl(ctx, req); r.code != CodeClosing {
			t.Errorf("%s = %+v", name, r)
		}
		<-done
		cancelLife()
		expire(nil)
	}
	// A closed lifecycle answers closing, also when the work budget has
	// expired at the same time.
	for name, signal := range map[string]func(s *supervisor){
		"closing":    func(s *supervisor) { s.closing.Store(true) },
		"loop ended": func(s *supervisor) { close(s.loopDone) },
		"cancelled":  func(s *supervisor) { s.life = cancelled(context.Canceled) },
		"attempt":    func(s *supervisor) { s.attempt = clock.Now().Add(-AttemptBound - time.Nanosecond) },
	} {
		for _, budget := range []context.Context{context.Background(), cancelled(errWorkExpired), cancelled(context.Canceled)} {
			s := newSup()
			signal(s)
			if r := s.handleWaitControl(budget, req); r.code != CodeClosing {
				t.Errorf("%s with budget %v = %+v", name, context.Cause(budget), r)
			}
		}
	}
	// The lifecycle closing while the handoff waits, then the budget
	// expiring: closing, not busy.
	late := newSup()
	ctx, expire = context.WithCancelCause(context.Background())
	res := make(chan waitReply, 1)
	go func() { res <- late.handleWaitControl(ctx, req) }()
	late.closing.Store(true)
	expire(errWorkExpired)
	if r := <-res; r.code != CodeClosing {
		t.Fatalf("closing before the busy answer = %+v", r)
	}
	// The work budget: anchored at the exchange start (time spent before
	// counts), expiring with errWorkExpired, its timer stopped and its
	// watcher joined on release.
	dc := &durClock{now: clock.Now()}
	srv := &controlServer{clock: dc, ctx: context.Background()}
	start := dc.Now()
	dc.now = dc.now.Add(time.Second)
	work, release := srv.workContext(start)
	if len(dc.durations) != 1 || dc.durations[0] != waitHandoff-time.Second {
		t.Fatalf("work timer %v", dc.durations)
	}
	dc.fire <- time.Time{}
	<-work.Done()
	if context.Cause(work) != errWorkExpired {
		t.Fatalf("work cause %v", context.Cause(work))
	}
	release()
	if dc.stops != 1 {
		t.Fatalf("timer stops %d", dc.stops)
	}
	work, release = srv.workContext(dc.Now().Add(-time.Hour))
	release()
	if dc.durations[1] != 0 || dc.stops != 2 || context.Cause(work) != context.Canceled {
		t.Fatalf("an exhausted budget %v, stops %d, cause %v", dc.durations, dc.stops, context.Cause(work))
	}
	s := newSup()
	expired, cancel := context.WithCancel(context.Background())
	cancel()
	if r := s.handleWaitControl(expired, req); r.code != CodeInternalError {
		t.Fatalf("handoff past its context = %+v", r)
	}
	// A loop that takes the request but never answers: the exchange's
	// cancellation (injected, after acceptance) is an internal error.
	unanswered, cancel := context.WithCancel(context.Background())
	took, joined := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(joined)
		<-s.reqs
		close(took)
	}()
	res = make(chan waitReply, 1)
	go func() { res <- s.handleWaitControl(unanswered, req) }()
	<-took
	cancel()
	if r := <-res; r.code != CodeInternalError {
		t.Fatalf("unanswered = %+v", r)
	}
	<-joined
	close(s.loopDone)
	if r := s.handleWaitControl(context.Background(), req); r.code != CodeClosing {
		t.Fatalf("loop ended = %+v", r)
	}
	s.closing.Store(true)
	if r := s.handleWaitControl(context.Background(), req); r.code != CodeClosing {
		t.Fatalf("closing = %+v", r)
	}
	// A refusal line is always a known refusal code.
	if l := string((waitReply{code: "bogus"}).line()); l != `{"ok":false,"code":"internal_error"}`+"\n" {
		t.Fatalf("unknown code line %q", l)
	}
}

// testBusyFinalTests (FP-6, amendment A2.1): a complete synthetic attempt
// whose monitor is held inside the code reviewer's final tests (the
// injected go test process waits at a barrier). The public wait for the
// observed code-reviewer task meets the busy loop: each untaken handoff
// whose injected work budget expires answers pending (no output, no
// fetch, no event), twice; after the final tests are released the freed
// loop takes a fresh poll and returns the authoritative terminal result,
// whose records the collector reads, and the attempt passes.
func testBusyFinalTests(t *testing.T) {
	t.Parallel()
	f := newFakeWorld(t)
	entered, release := make(chan struct{}), make(chan struct{})
	var releaseOnce sync.Once
	releaseFinal := func() { releaseOnce.Do(func() { close(release) }) }
	f.override = func(s ProcSpec) (behavior, bool) {
		if s.Path == fakeGo && strings.Join(s.Args, " ") == "test -count=1 ./..." && filepath.Base(filepath.Dir(s.Dir)) == "final" {
			b := f.base(s)
			b.after = func() {
				close(entered)
				<-release
			}
			return b, true
		}
		return behavior{}, false
	}
	// The work budgets of wait exchanges are fired by the driver only.
	timers := make(chan chan time.Time, 16)
	f.clock.held = func(d time.Duration) (<-chan time.Time, func() bool, bool) {
		if d != 4*time.Second {
			return nil, nil, false
		}
		ch := make(chan time.Time, 1)
		timers <- ch
		return ch, func() bool { return true }, true
	}
	f.coord.helperWait = 3
	var waitDone atomic.Bool
	f.coord.before = func(c *coordScript) bool {
		if c.step == 9 && !waitDone.Load() {
			// The finish waits for the helper's records; the time holds.
			f.clock.holdNext()
			return true
		}
		return false
	}
	var final waitRun
	driver := make(chan string, 1)
	go func() {
		driver <- func() string {
			defer waitDone.Store(true)
			defer releaseFinal()
			<-entered
			s := f.sup
			task := f.coord.tasks[3]
			events := func() int { return bytes.Count(mustRead(t, filepath.Join(s.ev.root, "events.jsonl")), []byte("\n")) }
			polls, proceed := make(chan int), make(chan struct{})
			c := &pollClock{onPoll: func(n int) {
				polls <- n
				<-proceed
			}}
			res := make(chan waitRun, 1)
			go func() {
				res <- runWait(t, helperWorld(c), "--run", s.runtime, "--task", task, "--hop", HopCodeReviewer)
			}()
			shows, evs := f.plane.showCount(), events()
			for n := 1; n <= 2; n++ {
				select {
				case tc := <-timers:
					tc <- time.Time{}
				case w := <-res:
					return fmt.Sprintf("busy poll %d before its work budget: exit %d %q %q", n, w.code, w.out, w.errOut)
				}
				select {
				case got := <-polls:
					if got != n {
						return fmt.Sprintf("poll %d after busy answer %d", got, n)
					}
				case w := <-res:
					return fmt.Sprintf("busy poll %d: exit %d %q %q", n, w.code, w.out, w.errOut)
				}
				// Pending on the wire: the helper polls again and has
				// printed nothing; no fetch and no event happened.
				if f.plane.showCount() != shows || events() != evs {
					return fmt.Sprintf("busy poll %d fetched (%d to %d) or recorded events (%d to %d)", n, shows, f.plane.showCount(), evs, events())
				}
				if n == 1 {
					proceed <- struct{}{}
				}
			}
			// The final tests end; the next poll's handoff is taken (its
			// budget never expires).
			releaseFinal()
			proceed <- struct{}{}
			for got := false; !got; {
				select {
				case <-timers:
				case final = <-res:
					got = true
				}
			}
			if final.code != 0 || final.errOut != "" || strings.Count(final.out, "\n") != 1 {
				return fmt.Sprintf("terminal wait = %d %q %q", final.code, final.out, final.errOut)
			}
			if _, err := contract.ParseWaitResponse([]byte(strings.TrimSuffix(final.out, "\n")), []string{task}, 40*time.Second); err != nil {
				return err.Error()
			}
			dir := filepath.Join(s.runtime, "waits", "04")
			for name, data := range map[string]string{"result.json": final.out, "error.txt": final.errOut, "exit-code": final.exitCodeFile()} {
				if err := os.WriteFile(filepath.Join(dir, name), []byte(data), 0o600); err != nil {
					return err.Error()
				}
			}
			return ""
		}()
	}()
	code := f.attempt()
	if msg := <-driver; msg != "" {
		t.Fatal(msg)
	}
	r := f.report()
	if code != 0 || r.Result != ResultPass || criterion(r, CritWaits).Result != "pass" || criterion(r, CritFinalTests).Result != "pass" {
		t.Fatalf("attempt = %d %s %v", code, r.Result, failedCodes(r))
	}
	var rec WaitRecord
	readDoc(t, filepath.Join(f.bundle(), "hops", "04", "wait.json"), &rec)
	if rec.ResultSHA256 != sha256Hex([]byte(final.out)) || *rec.ExitCode != 0 || rec.WinnerState != contract.TaskSucceeded {
		t.Fatalf("collected wait %+v", rec)
	}
	var run TestRun
	readDoc(t, filepath.Join(f.bundle(), "validation", "final-test.json"), &run)
	if run.ExitCode != 0 || run.TimedOut {
		t.Fatalf("final tests %+v", run)
	}
}

// testTakenFetchFailure (FP-6, amendment A2.1): once the loop took the
// handoff, a failed fetch, or one held until the work budget expires, is
// internal_error (exit 1, no stdout), never busy pending, and the helper
// does not retry.
func testTakenFetchFailure(t *testing.T) {
	t.Run("API error", func(t *testing.T) {
		t.Parallel()
		r := newWaitRig(t)
		r.observe(0)
		entered := make(chan struct{}, 1)
		r.p.onFetch = func() { entered <- struct{}{} }
		r.p.fetchErr = contract.New(contract.CodeUnavailable, "the plane is unavailable")
		r.serve()
		before := r.eventCount()
		c := &pollClock{}
		w := r.wait(c, 0)
		<-entered
		requireWaitError(t, "taken fetch error", w, "refused: internal_error")
		if c.pollCount() != 0 || r.p.fetches() != 1 || r.eventCount() != before {
			t.Fatalf("polls %d fetches %d events %d/%d", c.pollCount(), r.p.fetches(), r.eventCount(), before)
		}
	})
	t.Run("fetch outlives the work budget", func(t *testing.T) {
		t.Parallel()
		r := newWaitRig(t)
		r.observe(0)
		work := make(chan chan time.Time, 4)
		r.clock.announce(work)
		r.p.block = true
		r.serve()
		before := r.eventCount()
		c := &pollClock{}
		res := make(chan waitRun, 1)
		go func() { res <- r.wait(c, 0) }()
		budget := <-work
		<-r.p.started // the loop took the handoff and fetches
		budget <- time.Time{}
		if cause := <-r.p.blocked; cause != errWorkExpired {
			t.Fatalf("the taken fetch ended with %v", cause)
		}
		requireWaitError(t, "taken fetch past the budget", <-res, "refused: internal_error")
		if created, stopped := r.clock.workTimers(); c.pollCount() != 0 || r.p.fetches() != 1 || r.eventCount() != before || created != 1 || stopped != 1 {
			t.Fatalf("polls %d fetches %d events %d/%d work timers %d/%d", c.pollCount(), r.p.fetches(), r.eventCount(), before, created, stopped)
		}
	})
}

// testBusyLifecycleBounds (FP-6, amendment A2.1): while the handoff is
// untaken (the rig's loop is not running), closing, an ended loop, the
// supervisor's cancellation and an expired attempt answer closing, also
// with the work budget expiring together; a server close without expiry
// stays internal_error; repeated busy answers reach the helper's ceiling
// without output and reset no bound; and a busy answer leaves nothing
// for the loop to execute later.
func testBusyLifecycleBounds(t *testing.T) {
	for name, signal := range map[string]func(r *waitRig){
		"closing":                 func(r *waitRig) { r.s.closing.Store(true) },
		"ended loop":              func(r *waitRig) { close(r.s.loopDone) },
		"supervisor cancellation": func(r *waitRig) { r.cancel() },
		"expired attempt":         func(r *waitRig) { r.clock.set(r.s.attempt.Add(AttemptBound + time.Second)) },
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			r := newWaitRig(t)
			r.observe(0)
			work := make(chan chan time.Time, 4)
			r.clock.announce(work)
			r.writeRunFile()
			r.listen()
			res := make(chan waitRun, 1)
			go func() { res <- r.wait(&pollClock{}, 0) }()
			budget := <-work
			signal(r)
			budget <- time.Time{} // the work budget expires too
			requireWaitError(t, name, <-res, "refused: closing")
		})
	}
	t.Run("server closed", func(t *testing.T) {
		t.Parallel()
		r := newWaitRig(t)
		r.observe(0)
		work := make(chan chan time.Time, 4)
		r.clock.announce(work)
		r.writeRunFile()
		r.listen()
		res := make(chan waitRun, 1)
		go func() { res <- r.wait(&pollClock{}, 0) }()
		<-work
		r.s.ctl.close()
		requireWaitError(t, "server closed before the handoff", <-res, "refused: internal_error")
	})
	t.Run("ceiling and no queued request", func(t *testing.T) {
		t.Parallel()
		r := newWaitRig(t)
		r.observe(0)
		work := make(chan chan time.Time, 4)
		r.clock.announce(work)
		r.writeRunFile()
		r.listen()
		attempt, before := r.s.attempt, r.eventCount()
		c := &pollClock{jumpAfter: 4}
		res := make(chan waitRun, 1)
		go func() { res <- r.wait(c, 0) }()
		var w waitRun
		for got := false; !got; {
			select {
			case budget := <-work:
				budget <- time.Time{}
			case w = <-res:
				got = true
			}
		}
		requireWaitError(t, "busy until the ceiling", w, "90m wait ceiling")
		created, stopped := r.clock.workTimers()
		if c.pollCount() != 2 || created != 2 || stopped != 2 || r.p.fetches() != 0 || r.eventCount() != before || !r.s.attempt.Equal(attempt) {
			t.Fatalf("polls %d work timers %d/%d fetches %d events %d/%d", c.pollCount(), created, stopped, r.p.fetches(), r.eventCount(), before)
		}
		// The loop starts: nothing from the busy polls is executed; only a
		// fresh request (its budget not expiring) is taken.
		r.clock.announce(nil)
		r.startLoop()
		r.finish(0, contract.TaskSucceeded)
		if w := r.wait(&pollClock{}, 0); w.code != 0 || r.p.fetches() != 1 {
			t.Fatalf("fresh wait = %d %q after %d fetches", w.code, w.errOut, r.p.fetches())
		}
	})
}

// testReplyLifecycle (FP-6, review round 1 C1): the loop takes a wait and
// computes the authoritative terminal answer, but the supervisor is
// cancelled, or starts closing, at its fetch, after the loop's own
// lifecycle checks: the answer is selected with closing precedence, so the
// public wait command exits 1 with no stdout instead of a terminal result.
func testReplyLifecycle(t *testing.T) {
	for name, signal := range map[string]func(r *waitRig, cancelLife context.CancelFunc){
		"supervisor cancellation": func(_ *waitRig, cancelLife context.CancelFunc) { cancelLife() },
		"closing":                 func(r *waitRig, _ context.CancelFunc) { r.s.closing.Store(true) },
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			r := newWaitRig(t)
			r.observe(0)
			r.finish(0, contract.TaskSucceeded)
			// The socket's lifecycle context is the supervisor's
			// cancellation alone: the loop's own context stays live, so the
			// loop answers terminal and only reply selection sees it.
			life, cancelLife := context.WithCancel(context.Background())
			t.Cleanup(cancelLife)
			r.s.life = life
			fetched := make(chan struct{}, 1)
			r.p.onFetch = func() {
				signal(r, cancelLife)
				fetched <- struct{}{}
			}
			r.serve()
			before := r.eventCount()
			w := r.wait(&pollClock{}, 0)
			<-fetched
			requireWaitError(t, name, w, "refused: closing")
			if r.p.fetches() != 1 || r.eventCount() != before {
				t.Fatalf("fetches %d events %d/%d", r.p.fetches(), r.eventCount(), before)
			}
		})
	}
}

// durClock records its timers' durations and fires them only through
// fire (one at a time, single goroutine use).
type durClock struct {
	now       time.Time
	durations []time.Duration
	stops     int
	fire      chan time.Time
}

func (c *durClock) Now() time.Time { return c.now }

func (c *durClock) NewTimer(d time.Duration) (<-chan time.Time, func() bool) {
	c.durations = append(c.durations, d)
	c.fire = make(chan time.Time, 1)
	return c.fire, func() bool { c.stops++; return true }
}

// failingWriter fails every write.
type failingWriter struct{}

func (failingWriter) Write([]byte) (int, error) { return 0, errors.New("closed pipe") }

func containsCode(codes []string, c string) bool {
	for _, x := range codes {
		if x == c {
			return true
		}
	}
	return false
}
