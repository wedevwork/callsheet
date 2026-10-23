package reale2e

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"os"
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
	t     testing.TB
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
	// wakeForm is the coordinator's completion-notification form: "" the
	// user <task-notification> text, "system" the top-level system
	// task_notification record Claude Code 2.1.306 printed (design
	// 12a-dev-flow-verify r0.8, run_id and uuid included).
	wakeForm string
	// refusedLine is the stream line of the last notification the rig
	// emitted that the supervisor must refuse (0: none).
	refusedLine int
	// waitTimeout, when set, is the raw JSON number token the coordinator
	// passes as the wait start's Bash timeout (design 12a-dev-flow-verify
	// r0.9; Claude Code 2.1.306 passed 900000).
	waitTimeout string
	// reviewEntries, when set, adds entries to review n's checkout (from
	// 0) before the helper runs: the runtime directory and placeholders a
	// real workspace checkout holds (design 12a-dev-flow-verify r0.10).
	reviewEntries func(co string, n int)
	// replacedRead makes every consuming task_show's first answer the
	// vendor's persisted-output replacement (design 12a-dev-flow-verify
	// r0.10): "replaced" or "replaced-error" (is_error set), then a valid
	// lines:0 read; "replaced-only" and "bash-read" end the phase without a
	// valid read; "other-wording" is a replacement outside the captured
	// grammar, then a valid read; "captured" is the exact captured
	// multiline payload (testdata/task-show-replaced.txt), then a valid
	// read.
	replacedRead string
	// tickDuringPull keeps the monitor ticking (the fake time unmoved)
	// while a publication pull is held, instead of gating its poll timer
	// on the pull.
	tickDuringPull bool
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
	// command replaces the run command ("run-dev-full", "probe-dev-full").
	command string
	// inspect is the coordinator's optional workspace inspection mode
	// (inspectNone, inspectFull or inspectPartial).
	inspection string
	// reviewGo is the verification helper's scripted Go; realHelper runs
	// the helper's commands with the real local toolchain instead.
	reviewGo   reviewScript
	realHelper string
	// helperOut and receipts are each review's helper output and receipt;
	// reviewLogs are the reviewer tasks' scripted vendor logs.
	helperOut  map[string]string
	receipts   map[string]DevReviewRun
	reviewLogs map[string][]byte
}

// offsetClock runs real time from a fake clock's instant (a real helper
// process in a rig on fake time): Now is the base plus the real elapsed
// time, timers are real; settle moves the fake clock past it.
type offsetClock struct {
	fake  *autoClock
	base  time.Time
	start time.Time
}

func newOffsetClock(fake *autoClock) *offsetClock {
	return &offsetClock{fake: fake, base: fake.Now(), start: time.Now()}
}

func (c *offsetClock) Now() time.Time { return c.base.Add(time.Since(c.start)) }

func (c *offsetClock) NewTimer(d time.Duration) (<-chan time.Time, func() bool) {
	return realClock{}.NewTimer(d)
}

// settle advances the fake clock to this clock's now.
func (c *offsetClock) settle() { c.fake.advance(c.Now().Sub(c.fake.Now()) + time.Millisecond) }

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

func copyDevDocs(t testing.TB, checkout string) {
	t.Helper()
	for _, rel := range []string{DevFlowDir, DevFullDir, devProbeFixtureDir} {
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

func newDevRig(t testing.TB, steps []devAct) *devRig {
	t.Helper()
	f := newFakeWorld(t)
	copyDevDocs(t, f.checkout)
	// The harness executable and the pinned Go are real files (the
	// manifest and the reviewer receipt hash them); every process the
	// supervisor starts with them stays scripted.
	bin := t.TempDir()
	f.executable, f.goPath = stubFile(t, bin, "reale2e"), stubFile(t, bin, "go")
	r := &devRig{t: t, f: f, steps: steps, term: newDevTerminal(), answered: map[*devWindow]bool{},
		published: map[string]map[string][]byte{}, stage: "next", helperOut: map[string]string{}, receipts: map[string]DevReviewRun{},
		reviewLogs: map[string][]byte{}}
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
	h.Args = r.f.args()
	if r.command != "" {
		h.Args[0] = r.command
	}
	h.legacy = false
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
		if r.tickDuringPull {
			// The monitor keeps ticking while the pull is held, the fake
			// time unmoved (as the real poll timer keeps firing).
			r.f.clock.release()
			r.f.clock.holdNext()
			return
		}
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
		if act.alter == "prior-invalid" {
			// The previous slot's exact command, started in the foreground:
			// a dev-wait-task command with an invalid input is never an
			// unrelated job (r0.9).
			prior := devWaitCommand(d.s.w.Executable, d.s.runtime, r.workflowTasks()[0], 1)
			r.toolUse(toolBash, map[string]any{"command": prior, "description": "wait"}, "waited", false)
		}
		input := map[string]any{"command": cmd, "run_in_background": bg, "description": "wait"}
		if r.waitTimeout != "" {
			input["timeout"] = json.Number(r.waitTimeout)
		}
		r.use = r.toolUse(toolBash, input, res, false)
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
			if r.d.probe != nil {
				r.endPhase("probe=complete")
			} else {
				r.endPhase("Execution complete.")
			}
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
	commit, path := d.s.seed.Commit, d.s.seed.Path
	if d.probe != nil {
		// The probe's main branch is the pinned fixture candidate.
		commit = d.probe.cand
	}
	if err := copyObjects(d.s.seed.repo.Storer, r.f.plane.hub, []plumbing.Hash{commit}); err != nil {
		r.t.Fatal(err)
	}
	r.f.plane.pushSeed(commit)
	// The MCP ws_push answer: the complete push result of the seed.
	pushed := mcpAnswer(r, contract.ParseWorkspacePushResult, contract.WorkspacePushResult{Name: d.s.workspace.Name, Instance: d.s.workspace.Instance,
		Branch: "refs/heads/main", Commit: commit.String(), SourceKind: contract.KindGit, Changed: true})
	r.toolUse("mcp__callsheet__ws_push", map[string]any{"name": d.s.workspace.Name, "instance": d.s.workspace.Instance, "branch": "main", "path": path},
		pushed, false)
	r.seeded = true
}

// dispatch dispatches the prepared document with its exact fields.
func (r *devRig) dispatch(act devAct) bool {
	doc := r.doc
	ws, inst, base := doc.Workspace, doc.Instance, doc.Base
	req := contract.DispatchRequest{Target: doc.Target, Goal: doc.Goal, Acceptance: doc.Acceptance, Payload: []string{},
		RequestedBy: contract.RequestedBy{Name: "claude-code", Version: "2.1.292", Hostname: "localhost"}, Workspace: &ws, Base: &base, WorkspaceInstance: &inst}
	if ws == "" {
		// A no-workspace probe task: the document names no workspace.
		req.Workspace, req.Base, req.WorkspaceInstance = nil, nil, nil
	}
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
		r.toolUse(toolDispatch, dispatchInput(doc), "the answer was lost", true)
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
		// The MCP task_ls answers: the plane's own listing pages, followed
		// by their exclusive cursors until the page holding the task.
		hook := r.f.plane.onList
		r.f.plane.onList = nil
		after := ""
		for {
			page, next, err := r.f.plane.ListTasks(r.f.ctx, after, contract.DefaultTaskListLimit)
			if err != nil {
				r.t.Fatal(err)
			}
			in := map[string]any{}
			if after != "" {
				in["after"] = after
			}
			listing, _ := json.Marshal(contract.TaskListResponse{Version: contract.ProtocolVersion, Tasks: page, NextAfter: next})
			r.toolUse("mcp__callsheet__task_ls", in, string(listing), false)
			if slices.ContainsFunc(page, func(s contract.TaskSummary) bool { return s.TaskID == match[0] }) || next == nil {
				break
			}
			after = *next
		}
		r.f.plane.onList = hook
		r.task = match[0]
		r.stage = "observe"
		return true
	}
	v := r.mustDispatch(req)
	r.task = v.TaskID
	// The MCP dispatch answer: the strict asynchronous dispatch response.
	out, _ := json.Marshal(contract.DispatchResponse{Version: contract.ProtocolVersion, TaskID: v.TaskID, Task: &v})
	r.toolUse(toolDispatch, dispatchInput(doc), string(out), false)
	if fault == "unobserved" {
		r.finish(act, v.TaskID)
		r.stage = "unobserved"
		r.endPhase("I did not observe it.")
		return false
	}
	r.stage = "observe"
	return true
}

// mcpAnswer encodes v as the Callsheet MCP server does and requires its
// own strict parser to accept it (a rig answer is always a server answer).
func mcpAnswer[T any](r *devRig, parse func([]byte) (T, error), v any) string {
	b, err := contract.Encode(v)
	if err != nil {
		r.t.Fatal(err)
	}
	if _, err := parse(b); err != nil {
		r.t.Fatalf("the rig's answer %s is not a server answer: %v", b, err)
	}
	return string(b)
}

// dispatchInput is the MCP dispatch tool input of exactly the prepared
// document: optional workspace arguments are absent when the document has
// none (the MCP server never receives an empty or null one).
func dispatchInput(doc devDispatchDoc) map[string]any {
	in := map[string]any{"target": doc.Target, "goal": doc.Goal, "acceptance": doc.Acceptance}
	for k, v := range map[string]string{"workspace": doc.Workspace, "workspace_instance": doc.Instance, "base": doc.Base} {
		if v != "" {
			in[k] = v
		}
	}
	return in
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
	if act.alter == "early-fail" || act.alter == "early-stopped" {
		// The wait dies early (failed, or stopped by its Bash timeout): its
		// notification arrives while the task still runs (never terminal
		// here).
		status := "failed"
		if act.alter == "early-stopped" {
			status = "stopped"
		}
		r.refused(func() { r.notify(r.waitHandle(), r.use, status) })
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
	case "bad-wake":
		// Bound to the wait's handle under another tool use: refused at
		// its record; the valid notification after it repairs nothing.
		r.refused(func() { r.notify(r.waitHandle(), "toolu_dev_wrong", "completed") })
		r.notify(r.waitHandle(), r.use, "completed")
	case "malformed-wake":
		// Refused by the stream decoder itself: an unknown system field, or
		// a user envelope under another session.
		r.refused(func() {
			if r.wakeForm == "system" {
				r.emit(map[string]any{"type": "system", "subtype": "task_notification", "task_id": r.waitHandle(), "tool_use_id": r.use,
					"status": "completed", "exit_code": 0})
				return
			}
			r.emit(map[string]any{"type": "user", "session_id": "11111111-1111-4111-8111-111111111111", "message": map[string]any{"role": "user",
				"content": "<task-notification>\n<task-id>" + r.waitHandle() + "</task-id>\n<tool-use-id>" + r.use + "</tool-use-id>\n<status>completed</status>\n</task-notification>"}})
		})
		r.notify(r.waitHandle(), r.use, "completed")
	case "failed-wake":
		r.refused(func() { r.notify(r.waitHandle(), r.use, "failed") })
		r.notify(r.waitHandle(), r.use, "completed")
	case "deadline-no-wake":
		// No notification ever comes: the slot reaches its bound.
		r.f.clock.advance(FeatureTaskBound)
		return false
	default:
		r.notify(r.waitHandle(), r.use, "completed")
	}
	return true
}

// waitHandle is the slot's background wait handle.
func (r *devRig) waitHandle() string { return fmt.Sprintf("b%d", r.doc.Ordinal) }

// notify emits a completion notification in the rig's wake form.
func (r *devRig) notify(handle, use, status string) { r.emit(r.notification(handle, use, status)) }

// notification is a completion notification record in the rig's wake
// form.
func (r *devRig) notification(handle, use, status string) map[string]any {
	if r.wakeForm == "system" {
		return map[string]any{"type": "system", "subtype": "task_notification", "task_id": handle, "tool_use_id": use, "status": status,
			"output_file": "/tmp/claude/tasks/" + handle + ".output", "summary": "Background command \"wait\" completed (exit code 0)",
			"run_id": "11111111-2222-3333-4444-555555555555", "uuid": "66666666-7777-8888-9999-000000000000"}
	}
	return map[string]any{"type": "user", "message": map[string]any{"role": "user", "content": fmt.Sprintf(
		"<task-notification>\n<task-id>%s</task-id>\n<tool-use-id>%s</tool-use-id>\n<status>%s</status>\n<summary>done</summary>\n</task-notification>",
		handle, use, status)}}
}

// refused emits records through emit and remembers the line of the last
// one: the notification the supervisor must refuse.
func (r *devRig) refused(emit func()) {
	emit()
	r.refusedLine = bytes.Count(r.d.current().stdout.Bytes(), []byte("\n"))
}

// show consumes the wake with one task_show of the task. With fragment
// set, the first such result record reaches the stream in two writes.
// After a consumed wake the coordinator inspects the task's workspace as
// its playbook requires (inspect).
func (r *devRig) show() {
	v, _ := r.f.plane.ShowTask(r.f.ctx, r.task, 0)
	show, _ := json.Marshal(contract.TaskShowResponse{Version: contract.ProtocolVersion, Task: v})
	if mode := r.replacedRead; mode != "" {
		// The vendor replaced the consuming read's answer (fourth probe):
		// no evidence, so a later lines:0 read consumes, a Bash read of the
		// saved file never does, and another wording is refused offline.
		saved := "/tmp/claude/projects/x/tool-results/mcp-callsheet-task_show-1792709393274.txt"
		text := "Error: result (67,506 characters across 1 line) exceeds maximum allowed tokens. Output has been saved to " + saved + "."
		switch mode {
		case "other-wording":
			text = "Error: the result exceeds the maximum allowed tokens; it was saved to " + saved + "."
		case "captured":
			// The fourth probe's multiline payload, its saved path
			// neutralized (r0.10c).
			text = string(mustRead(r.t, filepath.Join("testdata", "task-show-replaced.txt")))
		}
		r.toolUse(toolTaskShow, map[string]any{"id": r.task}, text, mode == "replaced-error")
		switch mode {
		case "replaced-only":
			r.endPhase("The task's answer was too large to read.")
			return
		case "bash-read":
			r.toolUse(toolBash, map[string]any{"command": "cat " + saved, "description": "read the saved answer"}, string(show), false)
			r.endPhase("I read the saved answer instead.")
			return
		}
		r.toolUse(toolTaskShow, map[string]any{"id": r.task, "lines": 0}, string(show), false)
		r.inspect(v)
		return
	}
	if !r.fragment || r.tailWritten || r.tail != nil {
		r.toolUse(toolTaskShow, map[string]any{"id": r.task}, string(show), false)
		r.inspect(v)
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

// inspectGeneration is the rig workspace's generation token.
const inspectGeneration = "11111111111111111111111111111111"

// Inspection modes of the rig's optional coordinator workspace
// inspection (design 12a-dev-flow-verify r0.6: never required, but bound
// when made).
const (
	inspectNone    = ""
	inspectFull    = "full"
	inspectPartial = "partial"
)

// diffChanges are the changed paths from base to result, in raw path
// byte order, as the workspace diff reports them (regular files).
func diffChanges(base, result map[string][]byte) []contract.WorkspaceChange {
	var paths []string
	for p := range base {
		paths = append(paths, p)
	}
	for p := range result {
		if _, ok := base[p]; !ok {
			paths = append(paths, p)
		}
	}
	sort.Strings(paths)
	mode := "100644"
	var changes []contract.WorkspaceChange
	for _, p := range paths {
		b, inBase := base[p]
		n, inResult := result[p]
		c := contract.WorkspaceChange{PathBase64: base64.StdEncoding.EncodeToString([]byte(p))}
		switch {
		case inBase && inResult && bytes.Equal(b, n):
			continue
		case inBase && inResult:
			ob, nb := int64(len(b)), int64(len(n))
			c.Kind, c.OldMode, c.NewMode, c.OldBytes, c.NewBytes = contract.ChangeModified, &mode, &mode, &ob, &nb
		case inBase:
			ob := int64(len(b))
			c.Kind, c.OldMode, c.OldBytes = contract.ChangeDeleted, &mode, &ob
		default:
			nb := int64(len(n))
			c.Kind, c.NewMode, c.NewBytes = contract.ChangeAdded, &mode, &nb
		}
		changes = append(changes, c)
	}
	return changes
}

// rigDiffCall is one recorded ws_diff page call with its answer.
type rigDiffCall struct {
	in     map[string]any
	answer string
}

// diffPages are the paged diff calls of one enumeration (limit rows per
// page): first carries the form's selection, each continuation the
// previous page's cursor, instance and generation.
func (r *devRig) diffPages(first map[string]any, header contract.WorkspaceDiffResponse, changes []contract.WorkspaceChange, limit int) []rigDiffCall {
	var out []rigDiffCall
	in := first
	for i := 0; i == 0 || i < len(changes); i += limit {
		end := min(i+limit, len(changes))
		page := header
		page.Changes = append([]contract.WorkspaceChange{}, changes[i:end]...)
		var after []byte
		if i > 0 {
			after, _ = base64.StdEncoding.DecodeString(changes[i-1].PathBase64)
		}
		if end < len(changes) {
			next := changes[end-1].PathBase64
			page.NextAfter = &next
		}
		answer := mcpAnswer(r, func(b []byte) (contract.WorkspaceDiffResponse, error) {
			return contract.ParseWorkspaceDiff(b, after, limit)
		}, page)
		out = append(out, rigDiffCall{in: in, answer: answer})
		if end >= len(changes) {
			break
		}
		in = map[string]any{"after": changes[end-1].PathBase64, "instance": header.Instance, "generation": inspectGeneration, "limit": limit}
		for _, k := range []string{"task_id", "name", "base", "target"} {
			if v, ok := first[k]; ok {
				in[k] = v
			}
		}
	}
	return out
}

// inspect is the coordinator's optional workspace inspection of a
// consumed task with a published result (r.inspect): task_logs; the task
// forms of ws_status, ws_diff (paged, one change per page) and ws_pull;
// and the explicit forms of ws_status, ws_diff (seed main to the result
// commit, two changes per page) and ws_pull of main, with the two diff
// enumerations interleaved. Partial inspection requests only the task
// form's first diff page. Every answer is the server's answer.
func (r *devRig) inspect(v contract.TaskView) {
	if r.inspection == inspectNone || r.d.probe != nil || v.Result == nil || v.Result.Workspace == nil || v.Result.Workspace.Commit == nil || v.WorkspaceBinding == nil {
		return
	}
	ctx := r.f.ctx
	ws, seed := r.d.s.workspace, r.d.s.workspace.SeedCommit
	commit := *v.Result.Workspace.Commit
	base := map[string][]byte{}
	for p, f := range r.d.trees[*v.WorkspaceBinding.BaseCommit] {
		base[p] = f.Content
	}
	seedFiles := map[string][]byte{}
	for p, f := range r.d.trees[seed] {
		seedFiles[p] = f.Content
	}
	taskDiff := r.diffPages(map[string]any{"task_id": v.TaskID, "limit": 1}, contract.WorkspaceDiffResponse{Name: ws.Name, Instance: ws.Instance,
		Generation: inspectGeneration, BaseCommit: v.WorkspaceBinding.BaseCommit, TargetCommit: commit}, diffChanges(base, r.published[commit]), 1)
	if r.inspection == inspectPartial {
		// Only a multi-page enumeration can stop before exhaustion.
		if len(taskDiff) >= 2 {
			r.toolUse(toolWsDiff, taskDiff[0].in, taskDiff[0].answer, false)
		}
		return
	}
	logs, err := r.f.plane.TaskLogs(ctx, v.TaskID)
	if err != nil {
		r.t.Fatal(err)
	}
	r.toolUse(toolTaskLogs, map[string]any{"id": v.TaskID}, mcpAnswer(r, contract.ParseTaskLogsResponse, logs), false)
	status := contract.TaskWorkspaceStatusResponse{Version: contract.ProtocolVersion, Workspace: contract.TaskWorkspaceStatus{TaskID: v.TaskID, State: v.State,
		WorkspacePhase: v.WorkspacePhase, Binding: *v.WorkspaceBinding, Result: v.Result.Workspace, Available: true}}
	r.toolUse(toolWsStatus, map[string]any{"task_id": v.TaskID}, mcpAnswer(r, contract.ParseTaskWorkspaceStatusResponse, status), false)
	published := "2026-10-10T12:00:00Z"
	refs := []contract.WorkspaceRef{{Name: contract.TaskRefPrefix + v.TaskID, Commit: commit, PublishedAt: &published},
		{Name: contract.DefaultBranchRef, Commit: seed}}
	explicit := contract.WorkspaceStatusResponse{Name: ws.Name, Instance: ws.Instance, Generation: inspectGeneration, DefaultBranch: contract.DefaultBranchRef, Refs: refs}
	r.toolUse(toolWsStatus, map[string]any{"name": ws.Name}, mcpAnswer(r, func(b []byte) (contract.WorkspaceStatusResponse, error) {
		return contract.ParseWorkspaceStatus(b, "", contract.DefaultWorkspaceLimit)
	}, explicit), false)
	explicitDiff := r.diffPages(map[string]any{"name": ws.Name, "base": "main", "target": commit, "limit": 2}, contract.WorkspaceDiffResponse{Name: ws.Name,
		Instance: ws.Instance, Generation: inspectGeneration, BaseCommit: &seed, TargetCommit: commit}, diffChanges(seedFiles, r.published[commit]), 2)
	for i := 0; i < max(len(taskDiff), len(explicitDiff)); i++ {
		if i < len(taskDiff) {
			r.toolUse(toolWsDiff, taskDiff[i].in, taskDiff[i].answer, false)
		}
		if i < len(explicitDiff) {
			r.toolUse(toolWsDiff, explicitDiff[i].in, explicitDiff[i].answer, false)
		}
	}
	pulled := contract.WorkspacePullResult{Name: ws.Name, Instance: ws.Instance, Selector: *v.Result.Workspace.Ref,
		Commit: commit, DestinationKind: contract.KindFolder, Changed: true}
	r.toolUse(toolWsPull, map[string]any{"task_id": v.TaskID, "path": "inspect/" + v.TaskID}, mcpAnswer(r, contract.ParseWorkspacePullResult, pulled), false)
	main := contract.WorkspacePullResult{Name: ws.Name, Instance: ws.Instance, Selector: contract.DefaultBranchRef, Commit: seed,
		DestinationKind: contract.KindFolder, Changed: true}
	r.toolUse(toolWsPull, map[string]any{"name": ws.Name, "ref": "main", "path": "inspect/main-" + v.TaskID}, mcpAnswer(r, contract.ParseWorkspacePullResult, main), false)
}

// finish publishes the act's result for task and ends it.
func (r *devRig) finish(act devAct, task string) {
	d := r.d
	doc := r.doc
	if d.probe != nil && doc.Role == HopDesigner {
		// A harmless wake probe: an answer, no workspace or publication.
		final := "CALLSHEET_PROBE_OK\n"
		state := contract.TaskSucceeded
		switch act.alter {
		case "failed":
			state = contract.TaskFailed
		case "empty":
			final = "  \n"
		}
		r.f.plane.finish(task, state, &final, false, nil)
		return
	}
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
	switch act.alter {
	case "scratch-pub":
		files[reviewScratch+"/"+task+"/after/main.go"] = base["main.go"]
	}
	var h plumbing.Hash
	if act.alter == "exec-mode" {
		h, _ = writeCommitMode(r.t, r.f.plane.hub, files, map[string]filemode.FileMode{"main.go": filemode.Executable}, plumbing.NewHash(doc.Base))
	} else {
		h = writeNestedCommit(r.t, r.f.plane.hub, files, plumbing.NewHash(doc.Base))
	}
	r.published[h.String()] = files
	r.f.plane.finish(task, state, &final, truncated, &h)
	if log, ok := r.reviewLogs[task]; ok {
		r.f.plane.setLog(task, log)
	}
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
			if act.alter == "same-design" {
				// A redraft byte-identical to the approved design.
				files["design.md"] = []byte("# Design\n\nrevision 1\n")
				files["revision-history.md"] = []byte("# History\n\n- revision 1\n")
			}
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
			switch act.alter {
			case "no-signature":
				// run takes its streams in another order.
				files["main.go"] = bytes.Replace(files["main.go"], []byte("func run(args []string, stdout, stderr io.Writer) int"),
					[]byte("func run(stdout, stderr io.Writer, args []string) int"), 1)
			case "no-table":
				// The test calls run once, without its table loop.
				files["main_test.go"] = []byte("package main\n\nimport (\n\t\"io\"\n\t\"testing\"\n)\n\nfunc TestRun(t *testing.T) {\n\tif run(nil, io.Discard, io.Discard) != 0 {\n\t\tt.Fatal(\"run failed\")\n\t}\n}\n")
			}
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

// review is the scripted code reviewer: in a pinned checkout of its base
// it runs the fixed verification helper once (the helper refuses
// pre-existing scratch, exports the pinned candidate into fresh private
// scratch, runs its scripted processes there, removes the scratch and
// verifies it absent), its execution is recorded as one top-level Codex
// command record in the task's log, and it publishes only review.md with
// the receipt's exits, coverage and digest. The returned tree is the
// checkout's visible files after the processes ended.
func (r *devRig) review(act devAct, doc devDispatchDoc, base map[string][]byte, task string) map[string][]byte {
	co := physicalTempDir(r.t)
	pinReviewCheckout(r.t, co, r.f.plane.hub, plumbing.NewHash(doc.Base), base)
	if r.reviewEntries != nil {
		r.reviewEntries(co, len(r.reviews))
	}
	candidate := strings.Fields(goalField(doc.Goal, "candidate_commit") + " x")[0]
	initial := goalField(doc.Goal, "initial_source_commit")
	run := reviewRun{task: task, exits: map[string]int{}}
	argv := r.d.helper.argv(task, candidate, initial)
	if act.alter == "cross-task" {
		argv = r.d.helper.argv(hexID("t_", 99), candidate, initial)
	}
	var out string
	var rec DevReviewRun
	ran := act.alter != "prose-only" && act.alter != "no-helper"
	if ran {
		env := reviewEnv{cwd: co, clock: r.f.clock, launcher: &fakeLauncher{script: r.reviewGo.script}, path: "/usr/bin:/bin"}
		if r.realHelper != "" {
			env = reviewEnv{cwd: co, clock: newOffsetClock(r.f.clock), launcher: ExecLauncher{}, path: r.realHelper}
		}
		var code int
		out, _, code = verifyHelperRun(env, argv)
		if c, ok := env.clock.(*offsetClock); ok {
			c.settle()
		}
		if err := json.Unmarshal([]byte(out), &rec); err != nil {
			r.t.Fatalf("the helper printed no receipt: %v\n%s", err, out)
		}
		r.helperOut[task], r.receipts[task] = out, rec
		if rec.ScratchCreated {
			run.exports = append(run.exports, "after")
		}
		if c := commandNamed(rec.Commands, "go_test"); c != nil {
			run.exits["after/test"] = c.Exit
		}
		if c := commandNamed(rec.Commands, "go_test_cover"); c != nil {
			run.exits["after/cover"] = c.Exit
		}
		run.scratchOK = rec.ScratchRemoved
		command := "/bin/bash -lc '" + strings.Join(argv, " ") + "'"
		switch act.alter {
		case "wrapped":
			command = "cd " + co + " && " + strings.Join(argv, " ")
		}
		log := codexLog(command, out, code)
		switch act.alter {
		case "start-only":
			lines := strings.SplitAfter(string(log), "\n")
			log = []byte(strings.Join(append(lines[:4], lines[5:]...), ""))
		case "nested":
			// The receipt only inside an agent message: never a record.
			log = []byte(codexRecord(map[string]any{"type": "item.completed", "item": map[string]any{"id": "item_9", "type": "agent_message",
				"text": string(codexLog(command, out, code))}}))
		case "split-record":
			lines := strings.SplitAfter(string(log), "\n")
			half := len(lines[4]) / 2
			log = []byte(strings.Join(lines[:4], "") + lines[4][:half] + "\nstderr noise\n" + lines[4][half:] + strings.Join(lines[5:], ""))
		}
		r.reviewLogs[task] = log
	}
	scratch := filepath.Join(co, reviewScratch, task)
	if act.alter == "before-export" {
		// The reviewer's own optional before/ export after the helper, with
		// its own cleanup.
		dir := filepath.Join(scratch, "before")
		if err := os.MkdirAll(dir, 0o700); err != nil {
			r.t.Fatal(err)
		}
		for p, f := range r.d.trees[r.d.state.InitialCommit] {
			if err := os.WriteFile(filepath.Join(dir, p), f.Content, 0o600); err != nil {
				r.t.Fatal(err)
			}
		}
		run.exports = append([]string{"before"}, run.exports...)
		if err := os.RemoveAll(filepath.Join(co, reviewScratch)); err != nil {
			r.t.Fatal(err)
		}
	}
	if act.alter == "scratch" {
		if err := os.MkdirAll(filepath.Join(scratch, "after"), 0o700); err != nil {
			r.t.Fatal(err)
		}
		os.WriteFile(filepath.Join(scratch, "after", "main.go"), base["main.go"], 0o600)
	}
	var rep strings.Builder
	fmt.Fprintf(&rep, "# Review\n\nReviewed candidate: %s\n", candidate)
	for _, p := range devSourceFiles {
		if act.alter == "missing-field" && p == "main.go" {
			continue
		}
		fmt.Fprintf(&rep, "Source sha256: %s %s\n", p, sha256Hex(base[p]))
	}
	scratchPath := filepath.Join(co, reviewScratch, task)
	if rec.Scratch != "" {
		scratchPath = rec.Scratch
	}
	fmt.Fprintf(&rep, "Verification exit %d: go test ./...\n", run.exits["after/test"])
	fmt.Fprintf(&rep, "Verification exit %d: go test -coverprofile=%s/coverage.out ./...\n", run.exits["after/cover"], scratchPath)
	percent := "91.5"
	if rec.Coverage != nil {
		percent = rec.Coverage.Percent
	}
	fmt.Fprintf(&rep, "Coverage: %s%% of the changed statements (the helper's derivation)\n", percent)
	fmt.Fprintf(&rep, "Cleanup: scratch removed and verified absent (%t)\n", run.scratchOK)
	switch {
	case act.alter == "prose-only":
		rep.WriteString("Reviewer run sha256: " + sha256Hex([]byte("I ran the tests and they passed.\n")) + "\n")
	case act.alter == "bad-hash":
		rep.WriteString("Reviewer run sha256: " + strings.Repeat("0", 64) + "\n")
	case ran:
		rep.WriteString("Reviewer run sha256: " + sha256Hex([]byte(out)) + "\n")
	default:
		rep.WriteString("The verification helper could not start: no execution evidence exists.\n")
	}
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
	pub := map[string][]byte{}
	filepath.WalkDir(co, func(p string, e fs.DirEntry, err error) error {
		if err == nil && e.IsDir() && e.Name() == ".git" {
			return fs.SkipDir
		}
		// The task workspace never publishes its owned runtime directory
		// (internal/taskworkspace), and empty placeholders hold no file.
		if err == nil && e.IsDir() && contract.ValidRuntimeDir(e.Name()) {
			return fs.SkipDir
		}
		if err == nil && e.Type().IsRegular() {
			b, _ := os.ReadFile(p)
			rel, _ := filepath.Rel(co, p)
			pub[filepath.ToSlash(rel)] = b
		}
		return nil
	})
	if act.alter == "edit-main" {
		pub["main.go"] = []byte("package main\n\nfunc main() { println(9) }\n")
	}
	return pub
}

// writeNestedCommit stores a commit of files (slash paths make subtrees)
// with parent into st.
func writeNestedCommit(t testing.TB, st *memory.Storage, files map[string][]byte, parent plumbing.Hash) plumbing.Hash {
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

// data decodes an envelope's data into v, requiring this run's schema
// envelope.
func (r *devRig) data(name string, v any) error {
	r.t.Helper()
	var env struct {
		Schema string          `json:"schema"`
		RunID  string          `json:"run_id"`
		Data   json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(r.evidence(name), &env); err != nil {
		return err
	}
	if !strings.HasPrefix(env.Schema, "callsheet-dev-full-") || env.RunID != r.d.s.runID {
		return fmt.Errorf("%s is not this run's envelope", name)
	}
	return json.Unmarshal(env.Data, v)
}

// report reads the attempt's report.json.
func (r *devRig) report() DevReport {
	r.t.Helper()
	var rep DevReport
	if err := json.Unmarshal(r.evidence("report.json"), &rep); err != nil {
		r.t.Fatal(err)
	}
	return rep
}

// manifest reads the attempt's manifest.json.
func (r *devRig) manifest() DevManifest {
	r.t.Helper()
	var m DevManifest
	if err := json.Unmarshal(r.evidence(devManifestFile), &m); err != nil {
		r.t.Fatal(err)
	}
	return m
}

// cleanupRecord reads cleanup.json's data.
func (r *devRig) cleanupRecord() DevCleanup {
	r.t.Helper()
	var c devEnvelope[DevCleanup]
	if err := json.Unmarshal(r.evidence("cleanup.json"), &c); err != nil {
		r.t.Fatal(err)
	}
	return c.Data
}

// expect runs the attempt and requires its exit and stop code. A passing
// attempt's own report is PASS and its offline check repeats it.
func (r *devRig) expect(code int, stop string) {
	r.t.Helper()
	got := r.run()
	if r.d == nil || got != code || r.d.stopCode != stop {
		name := ""
		if r.d != nil {
			name = r.d.stopCode + ": " + r.d.stopReason
		}
		detail := ""
		if b, err := os.ReadFile(filepath.Join(r.f.evidence, "report.txt")); err == nil {
			detail = string(b)
		}
		r.t.Fatalf("exit %d want %d, stop %q want %q (%s); tasks %d, acts %d, stage %s, codes %v\nstderr: %s\nreport:\n%s",
			got, code, name, stop, name, r.taskCount(), r.i, r.stage, r.codes, r.f.stderr.String(), detail)
	}
	if code == 0 && (r.d.s.ev.failure() != nil || r.d.probe == nil && r.report().Result != ResultPass) {
		r.t.Fatalf("a passing attempt has an evidence failure or no PASS report: %v", r.d.s.ev.failure())
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
