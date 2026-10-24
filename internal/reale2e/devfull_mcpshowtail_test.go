package reale2e

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/go-git/go-git/v5/plumbing"

	"github.com/wedevwork/callsheet/internal/contract"
)

// Design 12a-dev-flow-verify r0.12 (sixth probe, FP-2, FP-8): task_show's
// result.log_tail is the request-selected display tail, mirrored from the
// top-level log_tail under the requested line bound; every other terminal
// result leaf stays bound to the captured DefaultTailLines view.

// showTailFixture is a minimal probe-6 shape: a succeeded workspace task
// whose captured view holds the DefaultTailLines tail of a short retained
// log (synthetic, never the owner's logs).
type showTailFixture struct {
	id   string
	log  []byte
	auth contract.TaskView
	a    *mcpAuthority
}

func newShowTailFixture(t *testing.T, log string) showTailFixture {
	t.Helper()
	p := newFakePlane(t, newAutoClock())
	role, err := p.AddRole(context.Background(), contract.RoleConfig{ID: HopCodeReviewer, Name: HopCodeReviewer, Node: hexID("n_", 1), Adapter: "codex",
		Model: "m", Effort: "low", Concurrency: 1})
	if err != nil {
		t.Fatal(err)
	}
	id := hexID("t_", 6)
	ws, base := "w", strings.Repeat("12", 20)
	req := contract.DispatchRequest{Target: contract.TaskTarget{Kind: contract.TargetID, Value: HopCodeReviewer}, Goal: "g", Acceptance: "a",
		RequestedBy: contract.RequestedBy{Name: "claude-code", Version: "2.1.292", Hostname: "localhost"}, Workspace: &ws, Base: &base}
	v := fakeView(id, req, role, p.clock.Now())
	sel, err := contract.ParseTaskBase(&base)
	if err != nil {
		t.Fatal(err)
	}
	v.WorkspaceBinding = &contract.WorkspaceBinding{Name: ws, Instance: hexID("", 0xabc), BaseSelector: contract.SelectorString(sel), BaseCommit: &base}
	p.tasks[id] = &v
	commit := plumbing.NewHash(strings.Repeat("ab", 20))
	p.finish(id, contract.TaskSucceeded, ptr("STATUS: REVIEW_APPROVED\n"), false, &commit)
	p.setLog(id, []byte(log))
	auth, err := p.ShowTask(context.Background(), id, contract.DefaultTailLines)
	if err != nil {
		t.Fatal(err)
	}
	return showTailFixture{id: id, log: []byte(log), auth: auth, a: &mcpAuthority{goos: "linux", views: map[string]contract.TaskView{id: auth}}}
}

// answer is the fixture's task_show answer with both display tails set to
// tail and tail_truncated to trunc, edit applied to a copied result.
func (f showTailFixture) answer(t *testing.T, tail string, trunc bool, edit func(r *contract.TaskResult)) string {
	t.Helper()
	v := f.auth
	r := *v.Result
	v.LogTail, v.TailTruncated, r.LogTail = tail, trunc, tail
	if edit != nil {
		edit(&r)
	}
	v.Result = &r
	b, err := json.Marshal(contract.TaskShowResponse{Version: contract.ProtocolVersion, Task: v})
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func (f showTailFixture) args(lines any) string {
	if lines == nil {
		return fmt.Sprintf(`{"id":%q}`, f.id)
	}
	return fmt.Sprintf(`{"id":%q,"lines":%v}`, f.id, lines)
}

// bindingField is a binding refusal's field ("" for none).
func bindingField(err error) string {
	var be *mcpBindingError
	if errors.As(err, &be) {
		return be.Field
	}
	return ""
}

func TestDevFullMCPTaskShowLinesZeroResultTail(t *testing.T) {
	t.Parallel()
	f := newShowTailFixture(t, "review log line\n")
	if f.auth.LogTail != "review log line\n" || f.auth.Result.LogTail != f.auth.LogTail {
		t.Fatalf("the captured tails %q %q", f.auth.LogTail, f.auth.Result.LogTail)
	}
	// The sixth probe's consuming read: lines 0 answers empty display
	// tails, truncated, while the capture holds the DefaultTailLines tail.
	if _, err := judgeTaskShow(f.a, []byte(f.args(0)), []byte(f.answer(t, "", true, nil))); err != nil {
		t.Fatalf("the lines:0 consuming read was refused: %v", err)
	}
	// Every other terminal result leaf stays authority-bound.
	other := plumbing.NewHash(strings.Repeat("cd", 20)).String()
	for name, c := range map[string]struct {
		edit  func(r *contract.TaskResult)
		field string
	}{
		"final_message":           {func(r *contract.TaskResult) { r.FinalMessage = ptr("STATUS: REVIEW_CHANGES_REQUESTED\n") }, "task.result.final_message"},
		"final_message_truncated": {func(r *contract.TaskResult) { r.FinalMessageTruncated = true }, "task.result.final_message_truncated"},
		"workspace_commit": {func(r *contract.TaskResult) {
			w := *r.Workspace
			w.Commit = &other
			*r = r.WithWorkspace(&w)
		}, "task.result."},
	} {
		_, err := judgeTaskShow(f.a, []byte(f.args(0)), []byte(f.answer(t, "", true, c.edit)))
		if !strings.HasPrefix(bindingField(err), c.field) {
			t.Errorf("%s: %v (field %q)", name, err, bindingField(err))
		}
	}
	// A nested tail that does not mirror the top-level one is the strict
	// parser's refusal, before any binding.
	_, err := judgeTaskShow(f.a, []byte(f.args(0)), []byte(f.answer(t, "", true, func(r *contract.TaskResult) { r.LogTail = "review log line\n" })))
	if err == nil || isBinding(err) || !strings.Contains(err.Error(), "inconsistent") {
		t.Fatalf("a nested tail without its mirror: %v", err)
	}
	// The requested line bound, with contract.LogTail counting (a final
	// LF ends the last line; the default when lines is omitted).
	twenty := strings.Repeat("l\n", contract.DefaultTailLines)
	for name, c := range map[string]struct {
		lines any
		tail  string
		ok    bool
	}{
		"zero_nonempty":       {0, "review log line\n", false},
		"one_line":            {1, "b\n", true},
		"two_lines_for_one":   {1, "a\nb\n", false},
		"trailing_lf":         {2, "a\nb\n", true},
		"unterminated":        {2, "a\nb", true},
		"blank_last_line":     {2, "a\nb\n\n", false},
		"default_twenty":      {nil, twenty, true},
		"default_twenty_one":  {nil, twenty + "l\n", false},
		"other_than_captured": {3, "x\ny\n", true},
	} {
		_, err := judgeTaskShow(f.a, []byte(f.args(c.lines)), []byte(f.answer(t, c.tail, true, nil)))
		if c.ok != (err == nil) || !c.ok && bindingField(err) != "task.log_tail" {
			t.Errorf("%s: %v", name, err)
		}
	}
	// Positive control: lines omitted, a log of at most DefaultTailLines
	// lines, the selected tails equal the captured ones.
	g := newShowTailFixture(t, "one\ntwo\nthree\n")
	v, _ := contract.LogTail(g.log, contract.DefaultTailLines)
	if v != g.auth.LogTail {
		t.Fatalf("captured %q, selected %q", g.auth.LogTail, v)
	}
	if _, err := judgeTaskShow(g.a, []byte(g.args(nil)), []byte(g.answer(t, g.auth.LogTail, g.auth.TailTruncated, nil))); err != nil {
		t.Fatalf("the default-lines answer equal to the capture was refused: %v", err)
	}
	// task_cancel keeps the entire terminal result bound, its nested tail
	// included.
	cancelled := f.auth
	r := *cancelled.Result
	cancelled.LogTail, cancelled.TailTruncated, r.LogTail = "other\n", false, "other\n"
	cancelled.Result = &r
	cancel, _ := json.Marshal(contract.CancelResponse{Version: contract.ProtocolVersion, TaskID: f.id, Task: cancelled})
	if _, err := judgeTaskCancel(f.a, []byte(f.args(nil)), cancel); bindingField(err) != "task.result.log_tail" {
		t.Fatalf("task_cancel with a changed result tail: %v", err)
	}
	same, _ := json.Marshal(contract.CancelResponse{Version: contract.ProtocolVersion, TaskID: f.id, Task: f.auth})
	if _, err := judgeTaskCancel(f.a, []byte(f.args(nil)), same); err != nil {
		t.Fatalf("task_cancel of the captured view: %v", err)
	}
}

// TestDevFullFakeShowTask: the fake plane renders the requested display
// tail with contract.LogTail and never shares a mutable result.
func TestDevFullFakeShowTask(t *testing.T) {
	t.Parallel()
	f := newShowTailFixture(t, "")
	p := newFakePlane(t, newAutoClock())
	stored := f.auth
	stored.LogTail, stored.TailTruncated, stored.Result.LogTail = "", false, ""
	p.tasks[f.id] = &stored
	big := strings.Repeat("0123456789abcdef\n", (contract.MaxTailBytes/17)+10)
	for name, c := range map[string]struct {
		log   string
		lines int
	}{
		"empty_zero": {"", 0}, "empty_default": {"", contract.DefaultTailLines}, "one_zero": {"a\n", 0}, "one_default": {"a\n", contract.DefaultTailLines},
		"three_two": {"a\nb\nc\n", 2}, "unterminated": {"a\nb", 1}, "byte_cap": {big, contract.MaxTailLines},
	} {
		p.setLog(f.id, []byte(c.log))
		v, err := p.ShowTask(context.Background(), f.id, c.lines)
		if err != nil {
			t.Fatal(err)
		}
		tail, trunc := contract.LogTail([]byte(c.log), c.lines)
		if v.LogTail != tail || v.TailTruncated != trunc || v.Result == nil || v.Result.LogTail != tail {
			t.Errorf("%s: %q %t (result %q), want %q %t", name, v.LogTail, v.TailTruncated, v.Result.LogTail, tail, trunc)
		}
		p.check(v)
	}
	// The byte cap on its own: one line longer than contract.MaxTailBytes
	// read with lines 1 is cut to its last MaxTailBytes bytes, truncated,
	// and mirrored in the result.
	long := strings.Repeat("x", contract.MaxTailBytes+100) + "\n"
	p.setLog(f.id, []byte(long))
	v, err := p.ShowTask(context.Background(), f.id, 1)
	if err != nil {
		t.Fatal(err)
	}
	if want := long[len(long)-contract.MaxTailBytes:]; v.LogTail != want || !v.TailTruncated || v.Result == nil || v.Result.LogTail != v.LogTail {
		t.Fatalf("overlong line: %d bytes, truncated %t, result mirrors %t; want %d bytes, truncated", len(v.LogTail), v.TailTruncated,
			v.Result != nil && v.Result.LogTail == v.LogTail, len(want))
	}
	p.check(v)
	// Answers are copies: a later answer, or a change to an earlier one,
	// leaves the stored task and the earlier answer as they were.
	p.setLog(f.id, []byte("a\nb\n"))
	first, _ := p.ShowTask(context.Background(), f.id, 1)
	second, _ := p.ShowTask(context.Background(), f.id, 0)
	first.Result.FinalMessage = ptr("changed\n")
	if first.LogTail != "b\n" || first.Result.LogTail != "b\n" || second.LogTail != "" || second.Result.LogTail != "" ||
		*second.Result.FinalMessage != "STATUS: REVIEW_APPROVED\n" || p.tasks[f.id].LogTail != "" || p.tasks[f.id].Result.LogTail != "" ||
		*p.tasks[f.id].Result.FinalMessage != "STATUS: REVIEW_APPROVED\n" {
		t.Fatalf("shared answers: first %q/%q second %q/%q stored %q/%q", first.LogTail, first.Result.LogTail, second.LogTail, second.Result.LogTail,
			p.tasks[f.id].LogTail, p.tasks[f.id].Result.LogTail)
	}
}

// displayTailShape checks a rig bundle has the sixth probe's shape: every
// consuming task_show asks lines 0 and is answered with empty display
// tails, while the task's captured task.json (under one of dirs) holds the
// nonempty DefaultTailLines tail of its retained log.
func displayTailShape(t *testing.T, bundle string, dirs []string) {
	t.Helper()
	captured := map[string]string{}
	for _, d := range dirs {
		v, err := contract.ParseTaskShowResponse(bytes.TrimSuffix(mustRead(t, filepath.Join(bundle, filepath.FromSlash(d), "task.json")), []byte("\n")))
		if err != nil {
			t.Fatalf("%s: %v", d, err)
		}
		if v.Result == nil || v.LogTail == "" || v.Result.LogTail != v.LogTail {
			t.Fatalf("%s: captured tail %q", d, v.LogTail)
		}
		captured[v.TaskID] = v.LogTail
	}
	n := 0
	for _, c := range recordedCalls(t, bundle, toolTaskShow) {
		id, lines, err := contract.ParseTaskShowArgs(c.args)
		if err != nil || c.answer == "" || lines != 0 {
			continue
		}
		v, err := contract.ParseTaskShowResponse([]byte(c.answer))
		if err != nil || v.LogTail != "" || v.Result == nil || v.Result.LogTail != "" || captured[id] == "" {
			t.Fatalf("consuming read of %s: %q %v (captured %q)", id, v.LogTail, err, captured[id])
		}
		n++
	}
	if n < len(dirs) {
		t.Fatalf("%d lines:0 consuming reads for %d captured tasks", n, len(dirs))
	}
}

// TestDevFullMCPTaskShowTailManifest: the field manifest reclassifies only
// task_show's nested display tail; dispatch and task_cancel keep it an
// authority binding, and task_show's other TaskView members keep their
// shared rules (its task_id and top-level log_tail as before).
func TestDevFullMCPTaskShowTailManifest(t *testing.T) {
	t.Parallel()
	shared := map[string]fieldSpec{}
	underPrefix("resp:task", taskViewRules(ruleAuthority, "I(task)"), shared)
	forms := map[string]mcpForm{}
	for _, f := range mcpForms() {
		forms[f.name] = f
	}
	var changed []string
	for k, spec := range shared {
		if forms["task_show"].fields[k] != spec {
			changed = append(changed, k)
		}
	}
	slices.Sort(changed)
	if want := []string{"resp:task.log_tail", "resp:task.result.log_tail", "resp:task.task_id"}; !slices.Equal(changed, want) {
		t.Fatalf("task_show reclassifies %v, want %v", changed, want)
	}
	if got := forms["task_show"].fields["resp:task.result.log_tail"]; got != (fieldSpec{ruleDerived, taskShowResultTailRule, expectBinding}) {
		t.Fatalf("task_show result.log_tail %+v", got)
	}
	if got := forms["task_show"].fields["resp:task.tail_truncated"]; got.rule != ruleContract || got.expect != expectAccept {
		t.Fatalf("task_show tail_truncated %+v", got)
	}
	for _, name := range []string{"dispatch", "task_cancel"} {
		if got := forms[name].fields["resp:task.result.log_tail"]; got.rule != ruleAuthority || got.expect != expectBinding {
			t.Errorf("%s result.log_tail %+v", name, got)
		}
	}
}
