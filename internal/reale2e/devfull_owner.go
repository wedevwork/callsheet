package reale2e

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync/atomic"
)

// Owner decisions and receipts (design 12a-dev-flow-harness, FP-5). After
// the coordinator consumed the approving audit's wait and its phase ended
// with the owner-pause result and its group gone, the supervisor presents
// the immutable design, history and audit with their hashes and opens one
// window; only "yes G" or "no G" for that generation decides. Input
// outside an open window, or read (even in part) before it opened, is read
// and discarded: the terminal's reader stamps each line with its window
// epoch. The decision and the
// read-only receipt are persisted, with strictly ordered events, before
// any coder plan exists, and every binding is rechecked when the plan is
// prepared and again at the coder's admission and publication.

// DevDecision is decisions/NN.json.
type DevDecision struct {
	Sequence   int    `json:"sequence"`
	Kind       string `json:"kind"`
	Window     string `json:"window"`
	Generation int    `json:"generation"`
	EventSeq   int    `json:"event_seq"`
	Time       string `json:"time"`
	Answer     string `json:"answer,omitempty"`
}

// presentedFiles are the owner gate's approved artifact set.
var presentedFiles = []string{"design.md", "revision-history.md", "design-review.md"}

// openWindow presents the gate's material and opens its decision window,
// at most 20m from the prompt and never past the attempt deadline.
func (d *devSupervisor) openWindow(phaseExitSeq int) {
	g := d.gate
	d.gate = nil
	w := &devWindow{kind: g.kind, gen: g.gen, phaseExitSeq: phaseExitSeq}
	switch g.kind {
	case pauseOwner:
		sums, err := d.presentDesign(g.gen)
		if err != nil {
			d.stop("decision_binding_changed", err.Error())
			return
		}
		w.sums = sums
	default:
		if !d.presentQuestions(g.gen) {
			return
		}
	}
	ev := d.s.ev.event(Event{Type: EvAwaitingOwner, Hop: fmt.Sprintf("generation-%d", g.gen), Code: g.kind})
	at, ok := parseTime(ev.Time)
	if !ok {
		at = d.s.w.Clock.Now()
	}
	w.promptSeq, w.deadline = ev.Seq, minTime(at.Add(OwnerDecisionBound), d.attemptDeadline())
	if err := d.activateTerminal(); err != nil {
		d.fail(err)
		return
	}
	// The window's epoch advances before its prompt is printed. A line
	// the reader read, even in part, before this instant carries an older
	// epoch wherever it is now (still being read, complete and in flight
	// to the loop, or waiting on the channel): pre-window input, discarded
	// and never this decision.
	if d.term != nil {
		w.epoch = d.term.OpenWindow()
	}
	d.discardPending()
	d.window = w
	if g.kind == pauseOwner {
		fmt.Fprintf(d.s.stdout, "Approve design generation %d and start coding? Type exactly \"yes %d\" or \"no %d\".\n", g.gen, g.gen, g.gen)
	} else {
		fmt.Fprintf(d.s.stdout, "Answer the designer's questions for generation %d: type exactly \"answer %d <text>\" (one line, at most %d bytes, no secrets).\n",
			g.gen, g.gen, triageTaskCeiling)
	}
	if d.termEOF || d.lines == nil {
		d.stop("owner_eof", "the operator terminal is closed")
	}
}

// activateTerminal establishes owner input admission before any workflow
// work: the terminal's one channel is read by the loop from then on, and
// every line outside an open window is read and discarded. The terminal
// must stamp its lines with decision windows (windowedTerminal).
func (d *devSupervisor) activateTerminal() error {
	if d.activated || d.s.w.Terminal == nil {
		return nil
	}
	t, ok := d.s.w.Terminal.(windowedTerminal)
	if !ok {
		return failure("owner_terminal", errors.New("the operator terminal cannot bind its input to decision windows"))
	}
	d.term, d.lines, d.activated = t, t.ActivateWindowed(), true
	return nil
}

// ownerInput is one operator terminal line, stamped by the terminal's
// reader with the window epoch current when it read the line's first byte
// (its LF, for an empty line).
type ownerInput struct {
	line  string
	epoch uint64
}

// windowedTerminal is the development path's operator terminal (design
// 12a-dev-flow-harness, Owner decisions and receipts). Its reader stamps
// every line as it reads it; OpenWindow advances the epoch and returns the
// new one. Only a line stamped with an open window's epoch can decide that
// window: one whose first byte was read before the window opened carries
// an older epoch however late it reaches the loop. Bytes the reader has
// not yet read when a window opens (still in the operating system's
// terminal buffer) cannot be told apart from bytes typed after it.
type windowedTerminal interface {
	ActivateWindowed() <-chan ownerInput
	OpenWindow() uint64
}

// ActivateWindowed starts the windowed line reader once and returns its
// lines, closed at EOF.
func (t *readerTerminal) ActivateWindowed() <-chan ownerInput {
	t.wonce.Do(func() {
		t.wch = make(chan ownerInput, 16)
		go readStampedLines(t.r, &t.epoch, t.wch)
	})
	return t.wch
}

// OpenWindow advances the reader's window epoch.
func (t *readerTerminal) OpenWindow() uint64 { return t.epoch.Add(1) }

// readStampedLines is readLines with each line stamped with the epoch
// read right after its first byte was (an over-long line is cut at
// maxControlLine bytes; a partial last line at EOF is dropped).
func readStampedLines(r io.Reader, epoch *atomic.Uint64, out chan<- ownerInput) {
	buf := make([]byte, 0, 512)
	one := make([]byte, 1)
	var stamp uint64
	started := false
	for {
		n, err := r.Read(one)
		if n == 1 {
			if !started {
				stamp, started = epoch.Load(), true
			}
			if one[0] == '\n' {
				out <- ownerInput{line: string(buf), epoch: stamp}
				buf, started = buf[:0], false
			} else if len(buf) < maxControlLine {
				buf = append(buf, one[0])
			}
		}
		if err != nil {
			close(out)
			return
		}
	}
}

// discardPending reads and discards every line already waiting on the
// terminal (EOF included).
func (d *devSupervisor) discardPending() {
	for d.lines != nil {
		select {
		case _, ok := <-d.lines:
			if !ok {
				d.lines, d.termEOF = nil, true
				return
			}
			d.ignoredLines++
		default:
			return
		}
	}
}

// presentDesign writes read-only private copies of the approving audit's
// design, history and report, records them and prints them with their
// hashes. The bytes must match the audit binding.
func (d *devSupervisor) presentDesign(gen int) (map[string]string, error) {
	a := d.state.Audit
	if a == nil || a.Generation != gen {
		return nil, errors.New("no approving audit for this generation")
	}
	tree, ok := d.trees[a.Commit]
	if !ok {
		return nil, errors.New("the approving audit's publication is unavailable")
	}
	want := map[string]string{"design.md": a.Design, "revision-history.md": a.History, "design-review.md": a.Review}
	dir := filepath.Join(d.s.runtime, "review", "generations", fmt.Sprintf("%02d", gen))
	if err := os.Mkdir(dir, 0o700); err != nil {
		return nil, err
	}
	sums := map[string]string{}
	d.s.say("OWNER DECISION NEEDED for design generation %d (within 20m): approving audit ordinal %d, task %s, result commit %s", gen, a.Ordinal, a.TaskID, a.Commit)
	for _, name := range presentedFiles {
		f, ok := tree[name]
		if !ok || sha256Hex(f.Content) != want[name] {
			return nil, fmt.Errorf("%s does not match the approving audit", name)
		}
		p := filepath.Join(dir, name)
		if err := writePrivateReadOnly(p, f.Content); err != nil {
			return nil, err
		}
		if err := d.s.ev.writeFile(fmt.Sprintf("review/%02d/%s", gen, name), f.Content); err != nil {
			return nil, err
		}
		sums[name] = want[name]
		d.s.say("----- %s (%s, sha256 %s) -----", name, p, want[name])
		fmt.Fprintln(d.s.stdout, terminalSafe(f.Content))
		d.s.say("----- end of %s -----", name)
	}
	return sums, nil
}

// presentQuestions prints the designer's questions with their immutable
// task and commit.
func (d *devSupervisor) presentQuestions(gen int) bool {
	q := d.state.Question
	if q == nil {
		d.stop("phase_result", "no designer questions are open")
		return false
	}
	d.s.say("OWNER ANSWER NEEDED for generation %d (within 20m): designer task %s (ordinal %d, commit %s, final answer sha256 %s) asks:", gen, q.TaskID, q.Ordinal, q.Commit, q.FinalSHA)
	fmt.Fprintln(d.s.stdout, terminalSafe([]byte(q.Final)))
	return true
}

// ownerLine handles one operator terminal line (ok false: EOF). The
// deadline is checked before the line is processed; a line outside an
// open window, or read before it opened, is discarded; a line that does
// not name the window's generation decides nothing and does not extend
// the deadline.
func (d *devSupervisor) ownerLine(in ownerInput, ok bool) {
	if !ok {
		d.lines, d.termEOF = nil, true
		if d.window != nil {
			d.stop("owner_eof", "the operator terminal closed before a decision")
		}
		return
	}
	if d.window == nil {
		d.ignoredLines++
		return
	}
	if d.deadlines() {
		return
	}
	if in.epoch != d.window.epoch {
		d.ignoredLines++
		d.s.say("input read before the generation %d window opened is discarded; the window's deadline is unchanged", d.window.gen)
		return
	}
	kind, text, match := ownerLineKind(in.line, d.window.gen, d.window.kind == pauseAnswer)
	if !match {
		d.s.say("not a decision for generation %d; the window's deadline is unchanged", d.window.gen)
		return
	}
	d.decide(kind, text)
}

// decide records one decision and applies it.
func (d *devSupervisor) decide(kind, text string) {
	w := d.window
	d.window = nil
	d.decisions++
	ev := d.s.ev.event(Event{Type: EvOwnerDecision, Hop: fmt.Sprintf("generation-%d", w.gen), Code: kind})
	rec := DevDecision{Sequence: d.decisions, Kind: kind, Window: w.kind, Generation: w.gen, EventSeq: ev.Seq, Time: ev.Time, Answer: text}
	b, err := encodeJSON(rec)
	if err != nil {
		d.stop("evidence_write_failed", err.Error())
		return
	}
	name := fmt.Sprintf("%02d.json", d.decisions)
	if err := writeExclusive(filepath.Join(d.s.runtime, "decisions", name), b, 0o600); err != nil {
		d.stop("runtime_unavailable", err.Error())
		return
	}
	if !d.writeEvidence("decisions/"+name, b) {
		return
	}
	var st DevFullState
	var res StepResult
	switch kind {
	case "no":
		st, res = OwnerDecision(d.state, w.gen, false, OwnerReceipt{}, rec.Sequence)
	case "answer":
		st, res = OwnerAnswer(d.state, w.gen, text, rec.Sequence)
	default:
		r, ok := d.receipt(w, ev)
		if !ok {
			return
		}
		st, res = OwnerDecision(d.state, w.gen, true, r, rec.Sequence)
	}
	d.apply(st, res)
	if !d.halted() && d.slot != nil {
		if err := d.launchPhase(); err != nil {
			d.fail(err)
		}
	}
}

// receipt builds, orders and persists the receipt of a yes: the presented
// copies must still match the audit binding, and the audit admission, wait
// consumption, phase exit, prompt, decision and receipt events must be
// strictly ordered. The runtime receipt is written read-only.
func (d *devSupervisor) receipt(w *devWindow, decision Event) (OwnerReceipt, bool) {
	r, err := BuildReceipt(d.state, w.gen)
	if err != nil {
		d.stop("stale_receipt", err.Error())
		return r, false
	}
	dir := filepath.Join(d.s.runtime, "review", "generations", fmt.Sprintf("%02d", w.gen))
	for _, name := range presentedFiles {
		b, err := readBounded(filepath.Join(dir, name), maxArtifactBytes)
		if err != nil || sha256Hex(b) != w.sums[name] {
			d.stop("decision_binding_changed", "the presented "+name+" changed before the receipt")
			return r, false
		}
	}
	r.PhaseExitSeq, r.PromptSeq, r.DecisionSeq, r.DecisionTime = w.phaseExitSeq, w.promptSeq, decision.Seq, decision.Time
	ev := d.s.ev.event(Event{Type: EvReceiptCreated, Hop: fmt.Sprintf("generation-%d", w.gen), Task: r.AuditTask})
	r.ReceiptSeq = ev.Seq
	seqs := []int{r.ApprovalSeq, r.ConsumedSeq, r.PhaseExitSeq, r.PromptSeq, r.DecisionSeq, r.ReceiptSeq}
	for i := 1; i < len(seqs); i++ {
		if seqs[i-1] <= 0 || seqs[i] <= seqs[i-1] {
			d.stop("receipt_order", fmt.Sprintf("the approval events are not strictly ordered: %v", seqs))
			return r, false
		}
	}
	b, err := encodeJSON(r)
	if err != nil {
		d.stop("evidence_write_failed", err.Error())
		return r, false
	}
	name := fmt.Sprintf("%02d.json", w.gen)
	if err := writePrivateReadOnly(filepath.Join(d.s.runtime, "receipts", name), b); err != nil {
		d.stop("runtime_unavailable", err.Error())
		return r, false
	}
	if !d.writeEvidence("receipts/"+name, b) {
		return r, false
	}
	d.receiptBytes[w.gen] = b
	return r, true
}

// checkReceipt verifies the active receipt for a coder plan or admission:
// valid, of the current generation and latest approving audit, its
// read-only runtime file unchanged, and the approved specification bytes
// unchanged in the coder's base (a descendant report or triage commit is
// allowed).
func (d *devSupervisor) checkReceipt(base string) error {
	r := d.state.Receipt
	a := d.state.Audit
	switch {
	case r == nil || !r.Valid || r.ReceiptSeq == 0:
		return errors.New("no valid owner receipt")
	case r.Generation != d.state.Generation || a == nil || r.AuditTask != a.TaskID || r.AuditCommit != a.Commit || r.RunID != d.s.runID:
		return errors.New("the receipt is not bound to the latest approving audit of this run")
	}
	b, err := readBounded(filepath.Join(d.s.runtime, "receipts", fmt.Sprintf("%02d.json", r.Generation)), 64<<10)
	if err != nil || !bytes.Equal(b, d.receiptBytes[r.Generation]) {
		return errors.New("the read-only receipt changed")
	}
	tree, ok := d.trees[base]
	if !ok || fileSHA(tree, "design.md") != r.Design || fileSHA(tree, "revision-history.md") != r.History {
		return errors.New("the approved specification changed in the coder's base")
	}
	return nil
}

// coderAdmissible reports whether a coder admission may be accepted now:
// the pending plan is the coder's and its receipt still verifies.
func (d *devSupervisor) coderAdmissible() bool {
	return d.slot != nil && d.slot.plan.Role == "coder" && d.checkReceipt(d.slot.plan.ExpectedBase) == nil
}

// writeExclusive creates p (new) with b.
func writeExclusive(p string, b []byte, mode os.FileMode) error {
	f, err := os.OpenFile(p, os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode)
	if err != nil {
		return err
	}
	_, werr := f.Write(b)
	return errors.Join(werr, f.Close())
}
