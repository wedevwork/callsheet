package reale2e

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/filemode"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/go-git/go-git/v5/storage/memory"

	"github.com/wedevwork/callsheet/internal/contract"
)

// The offline development rig (design 12a-dev-flow-harness, Function
// tests). It drives MainWith("run-dev-full") through its real boundaries:
// a scripted headless coordinator writes realistic stream records, pushes
// the seed through the fake plane, dispatches the prepared documents and
// calls the real dev-next/dev-observe helpers and the dev-wait-task control
// exchange over the real socket from separate goroutines (the supervisor
// loop serves them); scripted workers publish immutable in-memory commits
// that the production transfer puller installs; the owner answers on a
// terminal channel only while a window is open; the code reviewer runs
// real processes in private scratch. The coordinator acts inside the fake
// plane's ListTasks calls, on the supervisor goroutine, and holds the fake
// time while a socket exchange is in flight: the loop's poll timer is then
// gated on the exchange's answers, so the loop waits on its own events
// instead of spinning through ticks that fire at once.

// devAct is one scripted workflow task.
type devAct struct {
	marker         string
	blocker        string
	designRequired string
	// alter varies the task: publication (edit-main, scratch, bad-report,
	// no-commit, missing-field, no-digest, replace-questions,
	// blocker-mismatch, before-export, wrong-confidence), timing (timeout,
	// hang, late-wake)
	// or wait evidence (no-wake, foreground, double-wait, deny, wait-exit).
	alter string
}

// rigCall is one in-flight exchange on its own goroutine. all is closed
// once every one of its n results was sent on done.
type rigCall struct {
	n    int
	done chan rigResult
	got  []rigResult
	all  chan struct{}
	left atomic.Int32
}

func newRigCall(n int) *rigCall {
	c := &rigCall{n: n, done: make(chan rigResult, n), all: make(chan struct{})}
	c.left.Store(int32(n))
	return c
}

// answer sends one result; the last one closes all.
func (c *rigCall) answer(res rigResult) {
	c.done <- res
	if c.left.Add(-1) == 0 {
		close(c.all)
	}
}

// rigResult is one exchange's outcome.
type rigResult struct {
	code     int
	out, err string
	resp     controlResponse
	callErr  error
}

// devTerminal is the owner terminal: the rig types a line only while a
// window is open (or deliberately outside one). A typed line is read at
// once: it is stamped with the window epoch current when it is typed.
type devTerminal struct {
	mu    sync.Mutex
	epoch uint64
	ch    chan ownerInput
}

func newDevTerminal() *devTerminal { return &devTerminal{ch: make(chan ownerInput, 16)} }

// Activate is never used: the development path activates it windowed.
func (t *devTerminal) Activate() <-chan string { return nil }

func (t *devTerminal) ActivateWindowed() <-chan ownerInput { return t.ch }

func (t *devTerminal) OpenWindow() uint64 {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.epoch++
	return t.epoch
}

// typeLine types (and reads) one line.
func (t *devTerminal) typeLine(l string) {
	t.mu.Lock()
	e := t.epoch
	t.mu.Unlock()
	t.ch <- ownerInput{line: l, epoch: e}
}

// close ends the terminal (EOF).
func (t *devTerminal) close() { close(t.ch) }

// devRig drives one offline development attempt.
type devRig struct {
	t     *testing.T
	f     *fakeWorld
	d     *devSupervisor
	steps []devAct
	i     int
	// owner are the window answers in order ("" closes the terminal);
	// early are lines typed right after each decision, outside a window.
	owner     []string
	early     []string
	term      *devTerminal
	answered  map[*devWindow]bool
	decisions int
	fault     string
	// delay holds the coordinator before its first dev-next (ticks).
	delay int
	// live state
	pending   *rigCall
	stage     string
	inited    int
	seeded    bool
	lastTick  int
	doc       devDispatchDoc
	task      string
	use       string
	handles   int
	wakeAfter int
	// observable results
	goals     []string
	docs      []devDispatchDoc
	codes     []string
	procs     []*fakeProc
	published map[string]map[string][]byte
	reviews   []reviewRun
	onTick    func(r *devRig)
	stuck     bool
	ended     int
	// phaseExit is the exit code a phase ends with; noResult ends a phase
	// without its result record; pullErr fails every result pull.
	phaseExit int
	noResult  bool
	pullErr   error
	// pullHook runs on the supervisor's owned inspection goroutine inside
	// every result pull, before the transfer (the loop keeps running).
	pullHook func(r *devRig)
	// pullCtx, when set, runs there too with the pull's context; an error
	// it returns is the pull's.
	pullCtx func(ctx context.Context, r *devRig) error
	// eager asks dev-next for the successor without waiting for the
	// supervisor's internal admission: "after-show" right after the
	// consuming task_show, "before-show" between the wake and that
	// task_show (and again after it).
	eager string
	// fragment splits the first consuming task_show result record across
	// two writes on two supervisor ticks; tail is the unwritten rest and
	// tailWritten reports that it reached the stream.
	fragment    bool
	tail        []byte
	tailWritten bool
	// onFinal runs once inside the first ListTasks call after the attempt
	// finished (the final cleanup scan).
	onFinal func(r *devRig)
}

// heldReader is an operator terminal's input, served one byte per Read as
// the line reader asks for it. Before its last byte it closes consumed
// and holds until release is closed; the Read after its last byte closes
// delivered and reports EOF. The reader's progress is then ordered against
// the supervisor by channels alone.
type heldReader struct {
	data                         []byte
	pos                          int
	consumed, release, delivered chan struct{}
}

func newHeldReader(s string) *heldReader {
	return &heldReader{data: []byte(s), consumed: make(chan struct{}), release: make(chan struct{}), delivered: make(chan struct{})}
}

func (h *heldReader) Read(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	switch h.pos {
	case len(h.data):
		close(h.delivered)
		return 0, io.EOF
	case len(h.data) - 1:
		close(h.consumed)
		<-h.release
	}
	p[0] = h.data[h.pos]
	h.pos++
	return 1, nil
}

// errPuller fails every pull.
type errPuller struct{ err error }

func (p errPuller) PullResult(context.Context, string, string) error { return p.err }

// hookPuller runs hook inside every pull, then delegates unless hook
// fails it.
type hookPuller struct {
	p    Puller
	hook func(ctx context.Context) error
}

func (p hookPuller) PullResult(ctx context.Context, task, dir string) error {
	if err := p.hook(ctx); err != nil {
		return err
	}
	return p.p.PullResult(ctx, task, dir)
}

func copyDevDocs(t *testing.T, checkout string) {
	t.Helper()
	for _, rel := range []string{DevFlowDir, DevFullDir} {
		src := filepath.Join(repoRoot(t), filepath.FromSlash(rel))
		err := filepath.WalkDir(src, func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			relPath, _ := filepath.Rel(src, path)
			target := filepath.Join(checkout, filepath.FromSlash(rel), relPath)
			if d.IsDir() {
				return os.MkdirAll(target, 0o755)
			}
			if !d.Type().IsRegular() {
				return nil
			}
			b, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			return os.WriteFile(target, b, 0o644)
		})
		if err != nil {
			t.Fatal(err)
		}
	}
}

func newDevRig(t *testing.T, steps []devAct) *devRig {
	t.Helper()
	f := newFakeWorld(t)
	copyDevDocs(t, f.checkout)
	r := &devRig{t: t, f: f, steps: steps, term: newDevTerminal(), answered: map[*devWindow]bool{},
		published: map[string]map[string][]byte{}, stage: "next"}
	f.liveTerm = r.term
	f.puller = nil
	f.override = func(s ProcSpec) (behavior, bool) {
		if s.Path == fakeClaude && len(s.Args) > 0 && s.Args[0] == "-p" {
			return behavior{long: true, onStart: func(p *fakeProc) {
				r.procs = append(r.procs, p)
				if r.fault == "linger" && len(r.procs) > 1 || r.fault == "linger1" && len(r.procs) == 1 {
					p.mu.Lock()
					p.lingering, p.ignoreTerm, p.ignoreKill = true, true, true
					p.mu.Unlock()
				}
			}}, true
		}
		return behavior{}, false
	}
	return r
}

func (r *devRig) host() Host {
	h := r.f.host()
	args := r.f.args()
	args[0] = "run-dev-full"
	h.Args = args
	h.observe = nil
	h.observeDev = func(d *devSupervisor) { r.d = d }
	world := h.NewWorld
	h.NewWorld = func(h Host) (*World, func(), error) {
		w, release, err := world(h)
		if err == nil {
			conn := w.Connect
			w.Connect = func(url string, ca []byte) (Plane, Puller, func(), error) {
				pl, _, closer, err := conn(url, ca)
				puller := productionPuller(r.f.plane)
				if r.pullErr != nil {
					puller = errPuller{r.pullErr}
				}
				if r.pullHook != nil || r.pullCtx != nil {
					puller = hookPuller{p: puller, hook: func(ctx context.Context) error {
						if r.pullHook != nil {
							r.pullHook(r)
						}
						if r.pullCtx != nil {
							return r.pullCtx(ctx, r)
						}
						return nil
					}}
				}
				return pl, puller, closer, err
			}
		}
		return w, release, err
	}
	return h
}

// run runs the attempt and returns its exit code.
func (r *devRig) run() int {
	r.f.plane.onList = func() {
		if r.d != nil && r.d.finished && r.onFinal != nil {
			f := r.onFinal
			r.onFinal = nil
			f(r)
		}
		if r.d == nil || !r.d.looping || r.d.finished || r.d.ticks == r.lastTick {
			return
		}
		r.lastTick = r.d.ticks
		r.hook()
	}
	code := MainWith(r.host())
	// No hold outlives its attempt (a later run starts on unheld time).
	r.f.clock.release()
	// Exchanges still in flight when the attempt stopped answer from the
	// closed supervisor (each exchange is bounded by its 5s socket
	// deadline); their answers are part of the evidence.
	if c := r.pending; c != nil {
		for len(c.got) < c.n {
			c.got = append(c.got, <-c.done)
		}
		r.pending = nil
		for _, g := range c.got {
			switch {
			case g.callErr != nil:
				r.codes = append(r.codes, "transport")
			case g.resp.Code != "":
				r.codes = append(r.codes, g.resp.Code)
			default:
				r.codes = append(r.codes, strings.Fields(g.out + g.err + " x")[0])
			}
		}
	}
	return code
}

// record is one LF-terminated stream record.
func (r *devRig) record(v map[string]any) []byte {
	if _, ok := v["session_id"]; !ok {
		v["session_id"] = testSession
	}
	b, err := json.Marshal(v)
	if err != nil {
		r.t.Fatal(err)
	}
	return append(b, '\n')
}

// emit writes one stream record to the running phase.
func (r *devRig) emit(v map[string]any) {
	r.d.current().stdout.Write(r.record(v))
}

// toolUse emits one tool use and its result; it returns the use ID.
func (r *devRig) toolUse(name string, input map[string]any, result string, isError bool) string {
	r.handles++
	id := fmt.Sprintf("toolu_dev_%04d", r.handles)
	r.emit(map[string]any{"type": "assistant", "message": map[string]any{"role": "assistant", "content": []any{
		map[string]any{"type": "tool_use", "id": id, "name": name, "input": input}}}})
	r.emit(map[string]any{"type": "user", "message": map[string]any{"role": "user", "content": []any{
		map[string]any{"type": "tool_result", "tool_use_id": id, "content": result, "is_error": isError}}}})
	return id
}

// userText emits a user-role text record.
func (r *devRig) userText(text string) {
	r.emit(map[string]any{"type": "user", "message": map[string]any{"role": "user", "content": text}})
}

// endPhase ends the running phase with a successful result of text; the
// coordinator does nothing more in it.
func (r *devRig) endPhase(text string) {
	p := r.d.current()
	if !r.noResult {
		rec := r.record(map[string]any{"type": "result", "subtype": "success", "is_error": false, "result": text})
		if r.fault == "truncated-stream" {
			// The phase exits with its last record cut short.
			rec = rec[:len(rec)/2]
		}
		p.stdout.Write(rec)
	}
	r.ended = p.n
	p.proc.(*fakeProc).exit(r.phaseExit, "")
}

// helper runs one reale2e helper command on its own goroutine.
func (r *devRig) helper(args ...string) {
	c := newRigCall(1)
	go func() {
		var out, errb bytes.Buffer
		code := MainWith(Host{Args: args, Stdout: &out, Stderr: &errb})
		c.answer(rigResult{code: code, out: out.String(), err: errb.String()})
	}()
	r.pending = c
}

// nextNow is a dev-next request that reaches the supervisor between the
// coordinator's consuming task_show and the next monitor tick: the loop's
// own serve answers it here, inside this tick and before its progress
// pass. The answer is what the dev-next helper prints.
func (r *devRig) nextNow() {
	resp := r.d.serve(ControlRequest{Op: opDevNext})
	out := resp.Code
	switch resp.Code {
	case CodePrepared:
		out += " " + resp.Path + " " + resp.Digest
	case CodePaused:
		out += " " + resp.Summary
	}
	c := newRigCall(1)
	c.answer(rigResult{out: out + "\n"})
	r.pending = c
}

// control makes control exchanges on separate goroutines at once.
func (r *devRig) control(reqs ...ControlRequest) {
	c := newRigCall(len(reqs))
	rt := r.d.s.runtime
	var ready sync.WaitGroup
	ready.Add(len(reqs))
	start := make(chan struct{})
	for _, req := range reqs {
		go func(req ControlRequest) {
			ready.Done()
			<-start
			resp, err := devControlCall(rt, req)
			c.answer(rigResult{resp: resp, callErr: err})
		}(req)
	}
	ready.Wait()
	close(start)
	r.pending = c
}

// collect reports whether the in-flight exchanges all answered.
func (r *devRig) collect() ([]rigResult, bool) {
	c := r.pending
	for len(c.got) < c.n {
		select {
		case res := <-c.done:
			c.got = append(c.got, res)
		default:
			return nil, false
		}
	}
	r.pending = nil
	return c.got, true
}

// hook is one coordinator turn per supervisor tick.
func (r *devRig) hook() {
	d := r.d
	if r.onTick != nil {
		r.onTick(r)
	}
	if r.tail != nil {
		// The rest of a fragmented record arrives one tick later.
		if p := d.current(); p != nil {
			p.stdout.Write(r.tail)
			r.tailWritten = true
		}
		r.tail = nil
	}
	if r.pending != nil {
		if _, done := r.peek(); !done {
			// Hold the fake time while a real socket exchange completes:
			// the loop's poll timer fires, unmoved, only once every answer
			// arrived (or the supervisor is closing), while the loop
			// serves the exchange's requests as they come.
			r.f.clock.holdUntil(d.s.w.Poll, r.pending.all, d.s.loopDone)
			return
		}
	}
	if in := d.inspection; in != nil {
		// Hold the fake time the same way while the supervisor's owned
		// publication pull runs: the loop takes its result (or serves a
		// request) instead of spinning through ticks.
		r.f.clock.holdUntil(d.s.w.Poll, in.done, d.s.loopDone)
		return
	}
	r.f.clock.release()
	if r.fault == "attempt" {
		r.fault = ""
		r.f.clock.advance(AttemptBound)
		return
	}
	r.owner1()
	if p := d.current(); p == nil || p.n == r.ended || r.stuck {
		return
	}
	for r.advance() {
	}
}

// peek checks the in-flight exchanges without consuming them.
func (r *devRig) peek() ([]rigResult, bool) {
	c := r.pending
	for len(c.got) < c.n {
		select {
		case res := <-c.done:
			c.got = append(c.got, res)
		default:
			return nil, false
		}
	}
	return c.got, true
}

// owner1 answers an open window once, or types an early line after a
// decision outside any window.
func (r *devRig) owner1() {
	d := r.d
	if w := d.window; w != nil && !r.answered[w] {
		r.answered[w] = true
		if len(r.owner) == 0 {
			return
		}
		line := r.owner[0]
		r.owner = r.owner[1:]
		if line == "" {
			r.term.close()
			return
		}
		for _, l := range strings.Split(line, "\n") {
			r.term.typeLine(l)
		}
		return
	}
	if d.window == nil && d.decisions > r.decisions {
		r.decisions = d.decisions
		if len(r.early) > 0 {
			if r.early[0] == "" {
				r.term.close()
			} else {
				r.term.typeLine(r.early[0])
			}
			r.early = r.early[1:]
		}
	}
}

// act is the current workflow act.
func (r *devRig) act() devAct {
	if r.i < len(r.steps) {
		return r.steps[r.i]
	}
	return devAct{}
}

// advance runs one coordinator stage; it reports whether the next stage
// can run at once.
func (r *devRig) advance() bool {
	d := r.d
	p := d.current()
	if r.inited < len(d.phases) {
		r.inited = len(d.phases)
		r.emit(map[string]any{"type": "system", "subtype": "init", "cwd": filepath.Join(d.s.runtime, "coord"), "model": "claude-opus-5-5",
			"permissionMode": autoModeFlag, "tools": []string{"Bash", toolDispatch, toolTaskShow}})
		if p.n == 1 && r.fault != "no-push" {
			r.seed()
		}
	}
	if r.pending != nil {
		return r.reply()
	}
	act := r.act()
	switch r.stage {
	case "next":
		if r.delay > 0 {
			r.delay--
			return false
		}
		r.helper("dev-next", "--run", d.s.runtime)
		return false
	case "dispatch":
		return r.dispatch(act)
	case "observe":
		r.helper("dev-observe", "--run", d.s.runtime, "--ordinal", itoa(r.doc.Ordinal), "--task", r.task)
		return false
	case "wait":
		cmd := devWaitCommand(d.s.w.Executable, d.s.runtime, r.task, r.doc.Ordinal)
		bg := act.alter != "foreground"
		res := fmt.Sprintf("Command running in background with ID: b%d. Output is being written to: /tmp/x", r.doc.Ordinal)
		if !bg {
			res = "waited"
		}
		r.use = r.toolUse(toolBash, map[string]any{"command": cmd, "run_in_background": bg, "description": "wait"}, res, false)
		req := ControlRequest{Op: opDevWait, Task: r.task, Ordinal: itoa(r.doc.Ordinal)}
		switch {
		case r.fault == "concurrent-wait":
			reqs := []ControlRequest{}
			for k := 0; k < 8; k++ {
				reqs = append(reqs, req, ControlRequest{Op: opDevObserve, Task: r.task, Ordinal: itoa(r.doc.Ordinal)}, ControlRequest{Op: opDevNext})
			}
			r.control(reqs...)
		case r.fault == "concurrent-idempotent" && r.doc.Ordinal == 1:
			reqs := []ControlRequest{req}
			for k := 0; k < 8; k++ {
				reqs = append(reqs, ControlRequest{Op: opDevObserve, Task: r.task, Ordinal: itoa(r.doc.Ordinal)}, ControlRequest{Op: opDevNext})
			}
			r.control(reqs...)
		case act.alter == "double-wait":
			r.control(req, req)
		default:
			r.control(req)
		}
		return false
	case "work":
		return r.work(act)
	case "wake":
		if r.wakeAfter > 0 {
			r.wakeAfter--
			return false
		}
		if !r.wake(act) {
			r.stage = "admitted"
			return false
		}
		switch r.eager {
		case "before-show":
			// Ask for the successor before consuming the wake.
			r.helper("dev-next", "--run", d.s.runtime)
			r.stage = "early-next"
			return false
		case "after-show":
			r.show()
			r.i++
			r.stage = "next"
			r.nextNow()
			return true
		}
		r.show()
		r.stage = "admitted"
		return false
	case "show":
		// The wake is consumed after the early dev-next; the successor is
		// asked for once the supervisor admitted the publication (its
		// owned inspection runs apart from the loop).
		r.show()
		r.stage = "admitted"
		return false
	case "admitted":
		if d.slot != nil && d.slot.plan.Ordinal == r.doc.Ordinal && !d.halted() {
			return false
		}
		r.i++
		r.stage = "next"
		return true
	case "end":
		return false
	}
	return false
}

// reply consumes the in-flight exchange and moves the stage on.
func (r *devRig) reply() bool {
	d := r.d
	got, done := r.collect()
	if !done {
		return false
	}
	switch r.stage {
	case "next":
		res := got[0]
		r.codes = append(r.codes, strings.Fields(res.out + " x")[0])
		f := strings.Fields(res.out)
		switch {
		case len(f) == 3 && f[0] == CodePrepared:
			body := mustRead(r.t, f[1])
			if sha256Hex(body) != f[2] {
				r.t.Errorf("dev-next digest %s is not the document's %s", f[2], sha256Hex(body))
			}
			var doc devDispatchDoc
			if err := json.Unmarshal(body, &doc); err != nil {
				r.t.Fatal(err)
			}
			r.doc = doc
			r.docs = append(r.docs, doc)
			r.goals = append(r.goals, doc.Goal)
			r.toolUse("Read", map[string]any{"file_path": f[1]}, string(body), false)
			r.stage = "dispatch"
			return true
		case len(f) == 3 && f[0] == CodePaused:
			if r.fault == "wrong-gate" {
				f[2] = "generation=9"
			}
			r.endPhase(f[1] + " " + f[2])
			r.stage = "next"
		case len(f) == 1 && f[0] == CodeComplete:
			r.endPhase("Execution complete.")
			r.stage = "end"
		case len(f) == 1 && f[0] == CodePending:
			// The previous result is not admitted yet: ask again later,
			// never dispatch again.
			r.stage = "next"
		default:
			// A refusal (seed_unpublished, closing): end the phase.
			r.endPhase("The supervisor refused: " + strings.TrimSpace(res.out+res.err))
			r.stage = "end"
		}
	case "early-next":
		code := strings.Fields(got[0].out + " x")[0]
		r.codes = append(r.codes, code)
		if code != CodePending {
			r.endPhase("dev-next answered " + code + " before the wake was consumed")
			r.stage = "end"
			return false
		}
		r.stage = "show"
		return true
	case "observe":
		r.codes = append(r.codes, strings.TrimSpace(got[0].out+got[0].err))
		if got[0].code != 0 {
			r.endPhase("observe refused")
			r.stage = "end"
			return false
		}
		r.stage = "wait"
		return true
	case "wait":
		accepted := 0
		for _, g := range got {
			r.codes = append(r.codes, g.resp.Code)
			if g.resp.Code == CodeAccepted && g.callErr == nil {
				accepted++
			}
		}
		if accepted == 0 || d.halted() {
			r.stage = "end"
			return false
		}
		r.stage = "work"
		return true
	}
	return false
}

// seed pushes the seed through the fake plane as the coordinator's
// ws_push does.
func (r *devRig) seed() {
	d := r.d
	if err := copyObjects(d.s.seed.repo.Storer, r.f.plane.hub, []plumbing.Hash{d.s.seed.Commit}); err != nil {
		r.t.Fatal(err)
	}
	r.f.plane.pushSeed(d.s.seed.Commit)
	r.toolUse("mcp__callsheet__ws_push", map[string]any{"name": d.s.workspace.Name, "instance": d.s.workspace.Instance, "branch": "main", "path": d.s.seed.Path},
		`{"commit":"`+d.s.seed.Commit.String()+`"}`, false)
	r.seeded = true
}

// dispatch dispatches the prepared document with its exact fields.
func (r *devRig) dispatch(act devAct) bool {
	doc := r.doc
	ws, inst, base := doc.Workspace, doc.Instance, doc.Base
	req := contract.DispatchRequest{Target: doc.Target, Goal: doc.Goal, Acceptance: doc.Acceptance, Payload: []string{},
		RequestedBy: contract.RequestedBy{Name: "claude-code", Version: "2.1.292", Hostname: "localhost"}, Workspace: &ws, Base: &base, WorkspaceInstance: &inst}
	fault := r.fault
	switch fault {
	case "extra", "duplicate", "unobserved", "lost", "premature-coder", "bad-acceptance":
		r.fault = ""
	}
	switch fault {
	case "bad-acceptance":
		req.Acceptance += " (changed)"
	case "extra":
		bad := req
		bad.Goal = "this task is not the prepared plan"
		r.mustDispatch(bad)
	case "duplicate":
		r.mustDispatch(req)
	case "premature-coder":
		bad := req
		bad.Target = contract.TaskTarget{Kind: contract.TargetID, Value: "coder"}
		r.mustDispatch(bad)
	case "lost":
		r.f.plane.dispatchLost = true
		if _, err := r.f.plane.Dispatch(r.f.ctx, req); err == nil {
			r.t.Fatal("the lost dispatch returned a view")
		}
		// Recovery by task_ls: exactly one matching authoritative request.
		var match []string
		for _, id := range r.f.plane.ids() {
			v, _ := r.f.plane.ShowTask(r.f.ctx, id, 0)
			if v.Request.Goal == doc.Goal {
				match = append(match, id)
			}
		}
		if len(match) != 1 {
			r.t.Fatalf("lost reply recovery found %d tasks", len(match))
		}
		r.toolUse("mcp__callsheet__task_ls", map[string]any{}, `{"tasks":["`+match[0]+`"]}`, false)
		r.task = match[0]
		r.stage = "observe"
		return true
	}
	v := r.mustDispatch(req)
	r.task = v.TaskID
	out, _ := json.Marshal(map[string]any{"task_id": v.TaskID, "state": v.State})
	r.toolUse(toolDispatch, map[string]any{"target": doc.Target, "goal": doc.Goal, "acceptance": doc.Acceptance, "workspace": ws,
		"workspace_instance": inst, "base": base}, string(out), false)
	if fault == "unobserved" {
		r.finish(act, v.TaskID)
		r.stage = "unobserved"
		r.endPhase("I did not observe it.")
		return false
	}
	r.stage = "observe"
	return true
}

func (r *devRig) mustDispatch(req contract.DispatchRequest) contract.TaskView {
	v, err := r.f.plane.Dispatch(r.f.ctx, req)
	if err != nil {
		r.t.Fatalf("dispatch %s: %v", req.Target.Value, err)
	}
	return v
}

// work runs the scripted worker for the accepted wait, then the wait
// helper's capture.
func (r *devRig) work(act devAct) bool {
	d := r.d
	switch act.alter {
	case "timeout":
		r.f.clock.advance(FeatureTaskBound)
		r.stage = "end"
		return false
	case "hang":
		r.f.clock.advance(FeatureTaskBound - 30*time.Second)
		r.stage = "end"
		return false
	}
	r.finish(act, r.task)
	if act.alter == "deny" {
		r.emit(map[string]any{"type": "result", "subtype": "success", "is_error": false, "result": "denied",
			"permission_denials": []map[string]string{{"tool_name": "Bash", "tool_use_id": "toolu_x"}}})
	}
	v, _ := r.f.plane.ShowTask(r.f.ctx, r.task, 0)
	wr := contract.WaitResponse{Version: contract.ProtocolVersion, Status: contract.WaitTerminal, EffectiveWaitMS: 30000, Winner: r.task, Task: &v}
	b, err := json.Marshal(wr)
	if err != nil {
		r.t.Fatal(err)
	}
	i := r.doc.Ordinal - 1
	dir := waitDir(d.s.runtime, i)
	if err := os.WriteFile(filepath.Join(dir, runtimeWaitResult), append(b, '\n'), 0o600); err != nil {
		r.t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, runtimeWaitStderr), nil, 0o600); err != nil {
		r.t.Fatal(err)
	}
	exit := 0
	if act.alter == "wait-exit" {
		exit = 1
	}
	if err := captureFiles(d.s.runtime, i, exit, io.Discard, io.Discard); err != nil {
		r.t.Fatal(err)
	}
	r.stage = "wake"
	if act.alter == "late-wake" {
		r.wakeAfter = 5
	}
	return true
}

// wake delivers the automatic notification; it reports false when the
// coordinator ended its phase instead.
func (r *devRig) wake(act devAct) bool {
	switch act.alter {
	case "no-wake":
		r.endPhase("I stopped before the wake.")
		return false
	case "foreground":
		r.userText("<task-notification>\n<task-id>b99</task-id>\n<tool-use-id>other</tool-use-id>\n<status>completed</status>\n</task-notification>")
		r.emit(map[string]any{"type": "system", "subtype": "status"})
	default:
		r.userText(fmt.Sprintf("<task-notification>\n<task-id>b%d</task-id>\n<tool-use-id>%s</tool-use-id>\n<status>completed</status>\n<summary>done</summary>\n</task-notification>",
			r.doc.Ordinal, r.use))
	}
	return true
}

// show consumes the wake with one task_show of the task. With fragment
// set, the first such result record reaches the stream in two writes.
func (r *devRig) show() {
	v, _ := r.f.plane.ShowTask(r.f.ctx, r.task, 0)
	show, _ := json.Marshal(contract.TaskShowResponse{Version: contract.ProtocolVersion, Task: v})
	if !r.fragment || r.tailWritten || r.tail != nil {
		r.toolUse(toolTaskShow, map[string]any{"id": r.task}, string(show), false)
		return
	}
	r.handles++
	id := fmt.Sprintf("toolu_dev_%04d", r.handles)
	r.emit(map[string]any{"type": "assistant", "message": map[string]any{"role": "assistant", "content": []any{
		map[string]any{"type": "tool_use", "id": id, "name": toolTaskShow, "input": map[string]any{"id": r.task}}}}})
	rec := r.record(map[string]any{"type": "user", "message": map[string]any{"role": "user", "content": []any{
		map[string]any{"type": "tool_result", "tool_use_id": id, "content": string(show), "is_error": false}}}})
	half := len(rec) / 2
	r.d.current().stdout.Write(rec[:half])
	r.tail = rec[half:]
}

// finish publishes the act's result for task and ends it.
func (r *devRig) finish(act devAct, task string) {
	d := r.d
	doc := r.doc
	base := map[string][]byte{}
	for p, f := range d.trees[doc.Base] {
		base[p] = f.Content
	}
	files := r.publication(act, doc, base, task)
	final := r.final(act)
	state, truncated := contract.TaskSucceeded, false
	switch act.alter {
	case "no-commit":
		r.f.plane.finish(task, contract.TaskSucceeded, &final, false, nil)
		return
	case "failed":
		state = contract.TaskFailed
	case "truncated":
		truncated = true
	case "bad-final":
		final = "Done, no marker.\n"
	case "wrong-parent":
		doc.Base = r.d.state.InitialCommit
	}
	h := writeNestedCommit(r.t, r.f.plane.hub, files, plumbing.NewHash(doc.Base))
	r.published[h.String()] = files
	r.f.plane.finish(task, state, &final, truncated, &h)
}

// final is the act's final answer.
func (r *devRig) final(act devAct) string {
	var b strings.Builder
	switch act.marker {
	case markerNeedsInput:
		b.WriteString("Which name should the flag use?\n")
	case markerNeedsTriage:
		b.WriteString("Finding C1 needs investigation.\n")
	case markerNeedsDesign:
		b.WriteString("The design cannot be satisfied: the flag conflicts with help.\n")
	default:
		b.WriteString("Done.\n")
	}
	if r.doc.Role == "code-reviewer" {
		block := cmpOr(act.blocker, blockerNone)
		if act.alter == "blocker-mismatch" {
			block = blockerEnvironment
		}
		b.WriteString("CALLSHEET_REVIEW_BLOCKER: " + block + "\n")
	}
	b.WriteString("STATUS: " + act.marker + "\n")
	return b.String()
}

// goalField returns the value of a goal's "key: " line.
func goalField(goal, key string) string {
	for _, l := range strings.Split(goal, "\n") {
		if v, ok := strings.CutPrefix(l, key+": "); ok {
			return v
		}
	}
	return ""
}

// publication is the scripted worker's visible tree after its process
// group exited.
func (r *devRig) publication(act devAct, doc devDispatchDoc, base map[string][]byte, task string) map[string][]byte {
	files := map[string][]byte{}
	for k, v := range base {
		files[k] = v
	}
	n := doc.Ordinal
	switch doc.Role {
	case "designer":
		if act.marker == markerDesignDraft {
			files["design.md"] = []byte(fmt.Sprintf("# Design\n\nrevision %d\n", n))
			files["revision-history.md"] = []byte(fmt.Sprintf("# History\n\n- revision %d\n", n))
			if q, ok := files["design-questions.md"]; ok && len(q) > 0 {
				files["design-questions.md"] = []byte{}
			}
		}
		if act.alter == "replace-questions" {
			files["design-questions.md"] = []byte("A new unresolved question?\n")
		}
	case "design-reviewer":
		digest := strings.Fields(goalField(doc.Goal, "design_sha256") + " x")[0]
		if act.alter == "no-digest" {
			digest = "not named"
		}
		files["design-review.md"] = []byte(fmt.Sprintf("# Review %d\n\nReviewed design sha256 %s.\n\nSTATUS: %s\n", n, digest, act.marker))
	case "coder":
		switch act.marker {
		case markerImplComplete, markerFixesComplete:
			if _, ok := files["design-questions.md"]; ok {
				files["design-questions.md"] = []byte{}
			}
			files["main.go"] = []byte(strings.Replace(fixtureMain, "package main", fmt.Sprintf("// revision %d\npackage main", n), 1))
			files["main_test.go"] = []byte(fixtureTest)
		case markerQuestions:
			files["design-questions.md"] = []byte("What should the flag be called?\n")
		case markerNeedsDesign, markerNeedsTriage:
			files["main.go"] = []byte("package main\n\n// partial work\nfunc main() {}\n")
		}
	case "code-reviewer":
		return r.review(act, doc, base, task)
	case "triage":
		switch act.marker {
		case markerTriageConf, markerTriageProb:
			files["rca.md"] = []byte(fmt.Sprintf("# RCA %d\n\nThe cause is in main.go.\n\nSTATUS: %s\n", n, act.marker))
			if act.designRequired != "" {
				confidence := strings.TrimPrefix(act.marker, "TRIAGE_")
				if act.alter == "wrong-confidence" {
					confidence = "UNCONFIRMED"
				}
				files["fix.md"] = []byte(fmt.Sprintf("# Fix brief %d\n\nConfidence: %s\nDesign required: %s\n", n, confidence, act.designRequired))
			}
		default:
			files["rca.md"] = []byte(fmt.Sprintf("# RCA %d\n\nThe cause is not established.\n", n))
		}
	}
	if act.alter == "edit-main" {
		files["main.go"] = []byte("package main\n\nfunc main() { println(9) }\n")
	}
	return files
}

// reviewRun is one scripted review's observable process evidence.
type reviewRun struct {
	task      string
	exports   []string
	exits     map[string]int
	scratchOK bool
}

// review is the scripted code reviewer: it refuses pre-existing scratch,
// exports the pinned candidate (and optionally the initial source) into a
// fresh private scratch, runs owned test processes there, removes the
// scratch and verifies it absent, then publishes only review.md. The
// returned tree is the checkout's visible files after the processes ended.
func (r *devRig) review(act devAct, doc devDispatchDoc, base map[string][]byte, task string) map[string][]byte {
	co := r.t.TempDir()
	for p, b := range base {
		if err := os.WriteFile(filepath.Join(co, p), b, 0o644); err != nil {
			r.t.Fatal(err)
		}
	}
	manifest := func() string {
		var lines []string
		filepath.WalkDir(co, func(p string, e fs.DirEntry, err error) error {
			if err == nil && e.Type().IsRegular() {
				b, _ := os.ReadFile(p)
				rel, _ := filepath.Rel(co, p)
				lines = append(lines, filepath.ToSlash(rel)+" "+sha256Hex(b))
			}
			return nil
		})
		sort.Strings(lines)
		return strings.Join(lines, "\n")
	}
	scratch := filepath.Join(co, reviewScratch, task)
	if _, err := os.Lstat(filepath.Join(co, reviewScratch)); err == nil {
		r.t.Fatal("pre-existing review scratch")
	}
	initial := manifest()
	run := reviewRun{task: task, exits: map[string]int{}}
	exports := map[string]map[string][]byte{"after": base}
	if act.alter == "before-export" {
		seed := map[string][]byte{}
		for p, f := range r.d.trees[r.d.state.InitialCommit] {
			seed[p] = f.Content
		}
		exports["before"] = seed
	}
	for _, name := range []string{"before", "after"} {
		files, ok := exports[name]
		if !ok {
			continue
		}
		dir := filepath.Join(scratch, name)
		if err := os.MkdirAll(dir, 0o700); err != nil {
			r.t.Fatal(err)
		}
		for p, b := range files {
			if err := os.WriteFile(filepath.Join(dir, p), b, 0o644); err != nil {
				r.t.Fatal(err)
			}
		}
		for p, b := range files {
			if got, err := os.ReadFile(filepath.Join(dir, p)); err != nil || !bytes.Equal(got, b) {
				r.t.Fatalf("%s export of %s does not match the pinned commit", name, p)
			}
		}
		run.exports = append(run.exports, name)
		// The owned test processes: the plain run and the coverage run.
		for _, c := range []struct{ label, script string }{
			{"test", "test -f main.go && test -f main_test.go"},
			{"cover", "test -f main.go && printf 'mode: set\\n' > ../coverage.out"},
		} {
			cmd := exec.Command("sh", "-c", c.script)
			cmd.Dir = dir
			err := cmd.Run()
			code := 0
			if ee, ok := err.(*exec.ExitError); ok {
				code = ee.ExitCode()
			} else if err != nil {
				r.t.Fatal(err)
			}
			if cmd.ProcessState == nil || !cmd.ProcessState.Exited() {
				r.t.Fatal("a review process did not exit")
			}
			run.exits[name+"/"+c.label] = code
		}
	}
	if act.alter != "scratch" {
		if err := os.RemoveAll(filepath.Join(co, reviewScratch)); err != nil {
			r.t.Fatal(err)
		}
		_, err := os.Lstat(filepath.Join(co, reviewScratch))
		run.scratchOK = os.IsNotExist(err)
		if manifest() != initial {
			r.t.Fatal("the review changed the candidate checkout")
		}
	}
	candidate := goalField(doc.Goal, "candidate_commit")
	candidate = strings.Fields(candidate + " x")[0]
	var rep strings.Builder
	fmt.Fprintf(&rep, "# Review\n\nReviewed candidate: %s\n", candidate)
	for _, p := range devSourceFiles {
		if act.alter == "missing-field" && p == "main.go" {
			continue
		}
		fmt.Fprintf(&rep, "Source sha256: %s %s\n", p, sha256Hex(base[p]))
	}
	fmt.Fprintf(&rep, "Verification exit %d: go test ./...\n", run.exits["after/test"])
	fmt.Fprintf(&rep, "Verification exit %d: go test -coverprofile=%s/%s/coverage.out ./...\n", run.exits["after/cover"], reviewScratch, task)
	rep.WriteString("Coverage: 91.5% of statements in ./...\n")
	fmt.Fprintf(&rep, "Cleanup: scratch removed and verified absent (%t)\n", run.scratchOK)
	marker := act.marker
	if act.alter == "bad-report" {
		marker = markerBlocked
	}
	fmt.Fprintf(&rep, "\nCALLSHEET_REVIEW_BLOCKER: %s\nSTATUS: %s\n", cmpOr(act.blocker, blockerNone), marker)
	if err := os.WriteFile(filepath.Join(co, "review.md"), []byte(rep.String()), 0o644); err != nil {
		r.t.Fatal(err)
	}
	r.reviews = append(r.reviews, run)
	// The publication: every visible file once the processes ended.
	out := map[string][]byte{}
	filepath.WalkDir(co, func(p string, e fs.DirEntry, err error) error {
		if err == nil && e.Type().IsRegular() {
			b, _ := os.ReadFile(p)
			rel, _ := filepath.Rel(co, p)
			out[filepath.ToSlash(rel)] = b
		}
		return nil
	})
	if act.alter == "edit-main" {
		out["main.go"] = []byte("package main\n\nfunc main() { println(9) }\n")
	}
	return out
}

// writeNestedCommit stores a commit of files (slash paths make subtrees)
// with parent into st.
func writeNestedCommit(t *testing.T, st *memory.Storage, files map[string][]byte, parent plumbing.Hash) plumbing.Hash {
	t.Helper()
	var build func(prefix string) plumbing.Hash
	build = func(prefix string) plumbing.Hash {
		tree := &object.Tree{}
		dirs := map[string]bool{}
		var names []string
		for p := range files {
			if !strings.HasPrefix(p, prefix) {
				continue
			}
			rest := strings.TrimPrefix(p, prefix)
			if i := strings.IndexByte(rest, '/'); i >= 0 {
				dirs[rest[:i]] = true
				continue
			}
			names = append(names, rest)
		}
		for dname := range dirs {
			names = append(names, dname+"/")
		}
		sort.Slice(names, func(i, j int) bool { return strings.TrimSuffix(names[i], "/") < strings.TrimSuffix(names[j], "/") })
		for _, name := range names {
			if strings.HasSuffix(name, "/") {
				dname := strings.TrimSuffix(name, "/")
				tree.Entries = append(tree.Entries, object.TreeEntry{Name: dname, Mode: filemode.Dir, Hash: build(prefix + dname + "/")})
				continue
			}
			o := st.NewEncodedObject()
			o.SetType(plumbing.BlobObject)
			w, _ := o.Writer()
			w.Write(files[prefix+name])
			w.Close()
			h, err := st.SetEncodedObject(o)
			if err != nil {
				t.Fatal(err)
			}
			tree.Entries = append(tree.Entries, object.TreeEntry{Name: name, Mode: filemode.Regular, Hash: h})
		}
		to := st.NewEncodedObject()
		if err := tree.Encode(to); err != nil {
			t.Fatal(err)
		}
		h, err := st.SetEncodedObject(to)
		if err != nil {
			t.Fatal(err)
		}
		return h
	}
	th := build("")
	sig := object.Signature{Name: "Callsheet", Email: "callsheet@invalid", When: time.Unix(0, 0).UTC()}
	c := &object.Commit{Author: sig, Committer: sig, Message: "result\n", TreeHash: th, ParentHashes: []plumbing.Hash{parent}}
	co := st.NewEncodedObject()
	if err := c.Encode(co); err != nil {
		t.Fatal(err)
	}
	h, err := st.SetEncodedObject(co)
	if err != nil {
		t.Fatal(err)
	}
	return h
}

// evidence reads one bundle file.
func (r *devRig) evidence(name string) []byte {
	r.t.Helper()
	b, err := os.ReadFile(filepath.Join(r.f.evidence, filepath.FromSlash(name)))
	if err != nil {
		r.t.Fatal(err)
	}
	return b
}

// execution reads dev-execution.json.
func (r *devRig) execution() DevExecution {
	r.t.Helper()
	var ex DevExecution
	if err := json.Unmarshal(r.evidence(DevExecutionFile), &ex); err != nil {
		r.t.Fatal(err)
	}
	return ex
}

// cleanupRecord reads cleanup.json.
func (r *devRig) cleanupRecord() DevCleanup {
	r.t.Helper()
	var c DevCleanup
	if err := json.Unmarshal(r.evidence("cleanup.json"), &c); err != nil {
		r.t.Fatal(err)
	}
	return c
}

// expect runs the attempt and requires its exit and stop code.
func (r *devRig) expect(code int, stop string) {
	r.t.Helper()
	got := r.run()
	if r.d == nil || got != code || r.d.stopCode != stop {
		name := ""
		if r.d != nil {
			name = r.d.stopCode + ": " + r.d.stopReason
		}
		r.t.Fatalf("exit %d want %d, stop %q want %q (%s); tasks %d, acts %d, stage %s, codes %v\nstderr: %s",
			got, code, name, stop, name, r.taskCount(), r.i, r.stage, r.codes, r.f.stderr.String())
	}
	if code == 3 && r.d.s.ev.failure() != nil {
		r.t.Fatalf("an executed attempt has an evidence failure: %v", r.d.s.ev.failure())
	}
}

func (r *devRig) taskCount() int {
	if r.d == nil {
		return -1
	}
	return len(r.d.state.Tasks)
}

// workflowTasks are the plane's tasks other than the preflights.
func (r *devRig) workflowTasks() []string {
	var out []string
	for _, id := range r.f.plane.ids() {
		if !slices.Contains(r.d.s.preflight, id) {
			out = append(out, id)
		}
	}
	return out
}

// terminalState is a plane task's state.
func (r *devRig) terminalState(id string) string {
	v, err := r.f.plane.ShowTask(r.f.ctx, id, 0)
	if err != nil {
		return "missing"
	}
	return v.State
}
