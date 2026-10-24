package reale2e

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/filemode"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/go-git/go-git/v5/storage/memory"

	"github.com/wedevwork/callsheet/internal/contract"
)

// Judge-level cases of the MCP binding closure tests beyond the
// generated member matrix: a missing-authority case for every
// authority-dependent form, canonical and default argument controls, the
// R2–R6 regression labels that need no stream context, the diff page
// recomputation's ordering, row-limit and serialized-byte boundaries, and
// the session-level call policy (pairing, results, forbidden tools,
// cancellation eligibility, enumeration linkage) on synthetic records.

// withArgs is a control's arguments with members set (nil deletes).
func withArgs(t *testing.T, args string, members map[string]any) string {
	t.Helper()
	m := map[string]any{}
	if err := json.Unmarshal([]byte(args), &m); err != nil {
		t.Fatal(err)
	}
	for k, v := range members {
		if v == nil {
			delete(m, k)
		} else {
			m[k] = v
		}
	}
	return mustMarshal(t, m)
}

// firstControl is a form's first control.
func firstControl(t *testing.T, controls map[string][]mcpControl, form string) mcpControl {
	t.Helper()
	if len(controls[form]) == 0 {
		t.Fatalf("no %s control", form)
	}
	return controls[form][0]
}

// toolOfForm is a form's stream tool name.
func toolOfForm(form string) string {
	name, _, _ := strings.Cut(form, "/")
	return mcpToolPrefix + name
}

// isBinding reports an *mcpBindingError.
func isBinding(err error) bool {
	var be *mcpBindingError
	return errors.As(err, &be)
}

// isPolicy reports an *mcpPolicyError.
func isPolicy(err error) bool {
	var pe *mcpPolicyError
	return errors.As(err, &pe)
}

func judgeMissingAuthority(t *testing.T, controls map[string][]mcpControl) {
	idOf := func(c mcpControl, key string) string {
		var m map[string]any
		json.Unmarshal([]byte(c.args), &m)
		s, _ := m[key].(string)
		return s
	}
	for _, form := range []string{"dispatch", "task_ls", "task_show", "task_logs", "task_cancel", "ws_push", "ws_status/task", "ws_status/explicit",
		"ws_diff/task", "ws_diff/explicit", "ws_pull/task", "ws_pull/explicit"} {
		c := firstControl(t, controls, form)
		a := cloneAuthority(c.auth)
		switch form {
		case "dispatch":
			v, _ := contract.ParseDispatchResponse([]byte(c.answer))
			delete(a.views, v.TaskID)
		case "task_ls":
			page, _, _ := contract.ParseTaskListResponse([]byte(c.answer))
			delete(a.views, page[0].TaskID)
		case "task_show", "task_logs", "task_cancel":
			delete(a.views, idOf(c, "id"))
		case "ws_status/task", "ws_diff/task", "ws_pull/task":
			delete(a.views, idOf(c, "task_id"))
		case "ws_push":
			a.seed = nil
		case "ws_status/explicit", "ws_pull/explicit":
			a.main, a.views = "", map[string]contract.TaskView{}
		case "ws_diff/explicit":
			a.objects = nil
		}
		if _, err := judgeMCPCall(a, toolOfForm(form), json.RawMessage(c.args), c.answer); !isBinding(err) {
			t.Errorf("%s without its authority: %v", form, err)
		}
	}
}

func judgeCanonicalControls(t *testing.T, controls map[string][]mcpControl) {
	seed := func(c mcpControl) string { return c.auth.main }
	for name, c := range map[string]struct {
		form string
		args func(c mcpControl) string
	}{
		"dispatch_empty_payload": {"dispatch", func(c mcpControl) string { return withArgs(t, c.args, map[string]any{"payload": []string{}}) }},
		"task_show_default_lines": {"task_show", func(c mcpControl) string {
			return withArgs(t, c.args, map[string]any{"lines": contract.DefaultTailLines})
		}},
		"task_ls_default_limit": {"task_ls", func(c mcpControl) string {
			return withArgs(t, c.args, map[string]any{"limit": contract.DefaultTaskListLimit})
		}},
		"task_logs_late_false": {"task_logs", func(c mcpControl) string { return withArgs(t, c.args, map[string]any{"late": false}) }},
		"ws_push_full_branch": {"ws_push", func(c mcpControl) string {
			return withArgs(t, c.args, map[string]any{"branch": contract.DefaultBranchRef})
		}},
		"ws_pull_full_ref": {"ws_pull/explicit", func(c mcpControl) string {
			return withArgs(t, c.args, map[string]any{"ref": contract.DefaultBranchRef})
		}},
		"ws_pull_task_no_path": {"ws_pull/task", func(c mcpControl) string { return withArgs(t, c.args, map[string]any{"path": nil}) }},
		"ws_diff_base_hash":    {"ws_diff/explicit", func(c mcpControl) string { return withArgs(t, c.args, map[string]any{"base": seed(c)}) }},
		"ws_diff_base_full_ref": {"ws_diff/explicit", func(c mcpControl) string {
			return withArgs(t, c.args, map[string]any{"base": contract.DefaultBranchRef})
		}},
		"ws_status_default_limit": {"ws_status/explicit", func(c mcpControl) string {
			return withArgs(t, c.args, map[string]any{"limit": contract.DefaultWorkspaceLimit})
		}},
	} {
		ctl := firstControl(t, controls, c.form)
		if _, err := judgeMCPCall(ctl.auth, toolOfForm(c.form), json.RawMessage(c.args(ctl)), ctl.answer); err != nil {
			t.Errorf("%s: a canonical-equivalent call was refused: %v", name, err)
		}
	}
	// A legitimate nonterminal view and the later terminal view of the
	// same task are both accepted.
	var nonterminal, terminal bool
	for _, c := range controls["dispatch"] {
		v, _ := contract.ParseDispatchResponse([]byte(c.answer))
		if _, err := judgeMCPCall(c.auth, toolDispatch, json.RawMessage(c.args), c.answer); err == nil {
			nonterminal = nonterminal || !contract.TaskTerminal(v.State)
			terminal = terminal || contract.TaskTerminal(v.State)
		}
	}
	if !nonterminal || !terminal {
		t.Errorf("nonterminal and terminal views of one task: %t %t", nonterminal, terminal)
	}
}

func judgeRegressions(t *testing.T, controls map[string][]mcpControl) {
	d := firstControl(t, controls, "dispatch")
	show := firstControl(t, controls, "task_show")
	list := firstControl(t, controls, "task_ls")
	push := firstControl(t, controls, "ws_push")
	diff := firstControl(t, controls, "ws_diff/task")
	page, _, _ := contract.ParseTaskListResponse([]byte(list.answer))
	contractErr := func(err error) bool { return err != nil && !isBinding(err) && !isPolicy(err) }
	var dispatched contract.TaskView
	dispatched, _ = contract.ParseDispatchResponse([]byte(d.answer))
	// The other captured task with a published result (a diff task
	// substitution's target).
	var other string
	for id := range diff.auth.views {
		var a map[string]any
		json.Unmarshal([]byte(diff.args), &a)
		if _, ok := diff.auth.published(id); ok && id != a["task_id"] && other == "" {
			other = id
		}
	}
	coherentIdentity := func(field string) string {
		tree := jsonTree(d.answer).(map[string]any)
		v := obj(tree, "task")
		cur, _ := getAt(v, "workspace_binding."+field)
		alt, _ := altValue(field, cur)
		setAt(v, "workspace_binding."+field, alt)
		repairView(v, "workspace_binding."+field)
		return encodeTree(tree)
	}
	for name, c := range map[string]struct {
		form, args, answer string
		want               func(error) bool
	}{
		"R2_override":                    {"dispatch", withArgs(t, d.args, map[string]any{"override": map[string]any{"model": "other"}}), d.answer, isBinding},
		"R3_null_wait":                   {"dispatch", strings.TrimSuffix(d.args, "}") + `,"wait":null}`, d.answer, contractErr},
		"R3_response_workspace_name":     {"dispatch", d.args, coherentIdentity("name"), isBinding},
		"R3_response_workspace_instance": {"dispatch", d.args, coherentIdentity("instance"), isBinding},
		"R4_lines_negative":              {"task_show", withArgs(t, show.args, map[string]any{"lines": -1}), show.answer, contractErr},
		"R4_lines_201":                   {"task_show", withArgs(t, show.args, map[string]any{"lines": 201}), show.answer, contractErr},
		"R4_limit_0":                     {"task_ls", withArgs(t, list.args, map[string]any{"limit": 0}), list.answer, contractErr},
		"R4_limit_101":                   {"task_ls", withArgs(t, list.args, map[string]any{"limit": 101}), list.answer, contractErr},
		"R4_cursor_invalid":              {"task_ls", withArgs(t, list.args, map[string]any{"after": "t_x"}), list.answer, contractErr},
		"R4_cursor_exclusive":            {"task_ls", withArgs(t, list.args, map[string]any{"after": page[0].TaskID}), list.answer, isBinding},
		"R5_dispatch_with_wait":          {"dispatch", withArgs(t, d.args, map[string]any{"wait": "30s"}), d.answer, isPolicy},
		"R5_malformed_target":            {"dispatch", withArgs(t, d.args, map[string]any{"target": map[string]any{"kind": "bogus", "value": "designer"}}), d.answer, contractErr},
		"R5_push_answer_empty":           {"ws_push", push.args, `{}`, contractErr},
		"R5_push_answer_partial":         {"ws_push", push.args, `{"commit":"` + push.auth.main + `"}`, contractErr},
		"R6_push_branch_only":            {"ws_push", withArgs(t, push.args, map[string]any{"branch": "other-branch"}), push.answer, isBinding},
		"R6_task_diff_substitution":      {"ws_diff/task", withArgs(t, diff.args, map[string]any{"task_id": other}), diff.answer, isBinding},
	} {
		if _, err := judgeMCPCall(d.auth, toolOfForm(c.form), json.RawMessage(c.args), c.answer); !c.want(err) {
			t.Errorf("%s: %v", name, err)
		}
		if name == "R6_task_diff_substitution" && other == "" {
			t.Error("no second published task to substitute")
		}
	}
	_ = dispatched
	// R12 (design r0.12, sixth probe): task_show's display tails. The
	// nested tail alone is the parser's refusal; both tails changed
	// coherently within the requested bound are accepted whether or not
	// they equal the capture; over the bound is the derived binding's.
	// dispatch and task_cancel keep the nested tail authority-bound.
	tails := func(answer, tail string, nestedOnly bool) string {
		tree := jsonTree(answer).(map[string]any)
		v := obj(tree, "task")
		obj(v, "result")["log_tail"] = tail
		if !nestedOnly {
			v["log_tail"] = tail
		}
		return encodeTree(tree)
	}
	labelled := func(form, label string) mcpControl {
		for _, c := range controls[form] {
			if c.label == label {
				return c
			}
		}
		t.Fatalf("no %s control %q", form, label)
		return mcpControl{}
	}
	dt, ct := labelled("dispatch", "terminal view"), labelled("task_cancel", "terminal task")
	for name, c := range map[string]struct {
		ctl        mcpControl
		form, args string
		answer     string
		field      string
		want       func(error) bool
	}{
		"R12_show_nested_tail_only":     {show, "task_show", show.args, tails(show.answer, "other\n", true), "", contractErr},
		"R12_show_tails_within_bound":   {show, "task_show", withArgs(t, show.args, map[string]any{"lines": 1}), tails(show.answer, "other\n", false), "", func(err error) bool { return err == nil }},
		"R12_show_tails_default_lines":  {show, "task_show", withArgs(t, show.args, map[string]any{"lines": nil}), tails(show.answer, "a\nb\n", false), "", func(err error) bool { return err == nil }},
		"R12_show_tails_over_bound":     {show, "task_show", withArgs(t, show.args, map[string]any{"lines": 1}), tails(show.answer, "a\nb\n", false), "task.log_tail", isBinding},
		"R12_dispatch_tail_mismatch":    {dt, "dispatch", dt.args, tails(dt.answer, "other\n", false), "task.result.log_tail", isBinding},
		"R12_task_cancel_tail_mismatch": {ct, "task_cancel", ct.args, tails(ct.answer, "other\n", false), "task.result.log_tail", isBinding},
	} {
		_, err := judgeMCPCall(c.ctl.auth, toolOfForm(c.form), json.RawMessage(c.args), c.answer)
		if !c.want(err) || c.field != "" && bindingField(err) != c.field {
			t.Errorf("%s: %v", name, err)
		}
	}
	// R5: the valid inspection calls are accepted.
	for _, form := range []string{"ws_status/task", "ws_status/explicit", "ws_diff/task", "ws_diff/explicit", "ws_pull/task", "ws_pull/explicit", "task_logs"} {
		c := firstControl(t, controls, form)
		if _, err := judgeMCPCall(c.auth, toolOfForm(form), json.RawMessage(c.args), c.answer); err != nil {
			t.Errorf("R5 valid %s refused: %v", form, err)
		}
	}
	// R6: role_add and role_set model substitutions, including the former
	// mismatched positive control, fail the declared tool policy.
	node := "n_" + strings.Repeat("1", 32)
	role := `{"id":"worker-a","name":"implementer","node":"` + node + `","adapter":"fake","instruction":"/srv/manuals/instruction.md","runbook":"/srv/manuals/runbook.md","model":"example model","effort":"medium","concurrency":2,"timeout":"90m"}`
	rc, err := contract.ParseRoleAddArgs([]byte(role), devAdapterLookup)
	if err != nil {
		t.Fatal(err)
	}
	spec, _ := devAdapterLookup(rc.Adapter)
	answer := encodeAnswer(t, contract.RoleResponse{Version: contract.ProtocolVersion, Role: contract.RoleView{RoleRecord: contract.RoleRecord{RoleConfig: rc.Resolved(),
		RegistrationOrder: 1}, NodeLiveness: contract.LivenessOffline, AdapterTestOnly: spec.TestOnly}})
	for name, c := range map[string]struct{ tool, args string }{
		"R6_role_add_model":      {mcpToolPrefix + "role_add", strings.Replace(role, "example model", "other model", 1)},
		"R6_role_set_model":      {mcpToolPrefix + "role_set", `{"id":"worker-a","model":"other model"}`},
		"R6_former_role_control": {mcpToolPrefix + "role_set", `{"id":"worker-a","model":"other model"}`},
		"R6_role_add_unchanged":  {mcpToolPrefix + "role_add", role},
	} {
		if _, err := judgeMCPCall(d.auth, c.tool, json.RawMessage(c.args), answer); !isPolicy(err) {
			t.Errorf("%s: %v", name, err)
		}
	}
}

func judgeDiffPages(t *testing.T) {
	st := memory.NewStorage()
	base := map[string][]byte{"a.txt": []byte("a"), "dir/b.txt": []byte("bb"), "dir.txt": []byte("c"), "gone.txt": []byte("gone"), "keep.txt": []byte("k")}
	target := map[string][]byte{"a.txt": []byte("a2!"), "dir/b.txt": []byte("bb"), "dir/new.txt": []byte("new"), "dir.txt": []byte("c"), "keep.txt": []byte("k"),
		"z/deep/x.txt": []byte("xyz")}
	bh := gitTreeCommit(t, st, base, plumbing.ZeroHash)
	th := gitTreeCommit(t, st, target, bh)
	// An executable-mode change on its own pair of commits.
	mb, _ := writeCommitMode(t, st, map[string][]byte{"run.sh": []byte("#!")}, nil)
	mt, _ := writeCommitMode(t, st, map[string][]byte{"run.sh": []byte("#!")}, map[string]filemode.FileMode{"run.sh": filemode.Executable}, mb)
	b64 := func(s string) string { return base64.StdEncoding.EncodeToString([]byte(s)) }
	str := func(s string) *string { return &s }
	n := func(v int64) *int64 { return &v }
	header := func(b *string, tgt string) contract.WorkspaceDiffResponse {
		return contract.WorkspaceDiffResponse{Name: "w", Instance: strings.Repeat("a", 32), Generation: strings.Repeat("b", 32), BaseCommit: b, TargetCommit: tgt}
	}
	bs := bh.String()
	// Raw path byte order ("dir.txt" before "dir/..."), additions,
	// modifications and deletions with modes and sizes.
	want := []contract.WorkspaceChange{
		{PathBase64: b64("a.txt"), Kind: contract.ChangeModified, OldMode: str("100644"), NewMode: str("100644"), OldBytes: n(1), NewBytes: n(3)},
		{PathBase64: b64("dir/new.txt"), Kind: contract.ChangeAdded, NewMode: str("100644"), NewBytes: n(3)},
		{PathBase64: b64("gone.txt"), Kind: contract.ChangeDeleted, OldMode: str("100644"), OldBytes: n(4)},
		{PathBase64: b64("z/deep/x.txt"), Kind: contract.ChangeAdded, NewMode: str("100644"), NewBytes: n(3)},
	}
	rows, next, err := diffPage(st, header(&bs, th.String()), &bs, th.String(), nil, 100)
	if err != nil || jsonDiff("", rows, want) != "" || next != nil {
		t.Fatalf("full page %s next %v err %v", mustJSON(rows), next, err)
	}
	// The row limit: one row, then its continuation, to exhaustion.
	var got []contract.WorkspaceChange
	var after []byte
	for i := 0; i < 10; i++ {
		page, next, err := diffPage(st, header(&bs, th.String()), &bs, th.String(), after, 1)
		if err != nil || len(page) != 1 {
			t.Fatalf("page %d: %v %v", i, page, err)
		}
		got = append(got, page...)
		if next == nil {
			break
		}
		if *next != page[0].PathBase64 {
			t.Fatalf("next %s for row %s", *next, page[0].PathBase64)
		}
		after, _ = base64.StdEncoding.DecodeString(*next)
	}
	if jsonDiff("", got, want) != "" {
		t.Fatalf("paged rows %s", mustJSON(got))
	}
	// From the empty tree, and a mode-only change.
	all, _, _ := diffPage(st, header(nil, bh.String()), nil, bh.String(), nil, 100)
	if len(all) != len(base) || all[0].Kind != contract.ChangeAdded {
		t.Fatalf("empty base %s", mustJSON(all))
	}
	ms := mb.String()
	mode, _, _ := diffPage(st, header(&ms, mt.String()), &ms, mt.String(), nil, 100)
	if len(mode) != 1 || mode[0].Kind != contract.ChangeModified || *mode[0].OldMode != "100644" || *mode[0].NewMode != "100755" {
		t.Fatalf("mode change %s", mustJSON(mode))
	}
	// The serialized-byte bound ends a page before its row limit, with
	// room reserved for next_after naming its last row.
	big := map[string][]byte{}
	for i := 0; i < 120; i++ {
		big[fmt.Sprintf("%03d-%s", i, strings.Repeat("p", 8000))] = []byte("x")
	}
	bigH := gitTreeCommit(t, st, big, plumbing.ZeroHash)
	h := header(nil, bigH.String())
	page, next, err := diffPage(st, h, nil, bigH.String(), nil, contract.MaxWorkspaceLimit)
	if err != nil || len(page) == 0 || len(page) >= contract.MaxWorkspaceLimit || next == nil || *next != page[len(page)-1].PathBase64 {
		t.Fatalf("byte-bounded page: %d rows, next %v, err %v", len(page), next, err)
	}
	resp := h
	resp.Changes, resp.NextAfter = page, next
	enc, _ := contract.Encode(resp)
	if len(enc)+1 > contract.MaxWorkspaceResponse {
		t.Fatalf("the page exceeds the response bound: %d", len(enc)+1)
	}
	more := resp
	more.Changes = append(slices.Clone(page), contract.WorkspaceChange{PathBase64: page[len(page)-1].PathBase64, Kind: contract.ChangeAdded,
		NewMode: str("100644"), NewBytes: n(1)})
	if enc2, _ := contract.Encode(more); len(enc2)+1 <= contract.MaxWorkspaceResponse {
		t.Fatal("one more row would still fit: the page ended early")
	}
	// The judge: a byte-bounded page is the answer; a full limit-sized
	// page is not demanded, and a truncated one is refused.
	a := &mcpAuthority{goos: "linux", views: map[string]contract.TaskView{}, workspace: "w", instance: h.Instance, objects: st}
	args := fmt.Sprintf(`{"name":"w","base":"empty","target":%q}`, bigH.String())
	if _, err := judgeMCPCall(a, toolWsDiff, json.RawMessage(args), encodeAnswer(t, resp)); err != nil {
		t.Fatalf("byte-bounded page refused: %v", err)
	}
	short := resp
	short.Changes = page[:len(page)-1]
	k := short.Changes[len(short.Changes)-1].PathBase64
	short.NextAfter = &k
	if _, err := judgeMCPCall(a, toolWsDiff, json.RawMessage(args), encodeAnswer(t, short)); !isBinding(err) {
		t.Fatalf("a page ended before the byte bound: %v", err)
	}
}

// gitTreeCommit writes files as nested trees in git entry order (a
// directory sorts as name/) and a commit on parent (zero: none).
func gitTreeCommit(t *testing.T, st *memory.Storage, files map[string][]byte, parent plumbing.Hash) plumbing.Hash {
	t.Helper()
	var build func(prefix string) plumbing.Hash
	build = func(prefix string) plumbing.Hash {
		tree := &object.Tree{}
		dirs := map[string]bool{}
		for p := range files {
			rest, ok := strings.CutPrefix(p, prefix)
			if !ok {
				continue
			}
			if i := strings.IndexByte(rest, '/'); i >= 0 {
				if !dirs[rest[:i]] {
					dirs[rest[:i]] = true
					tree.Entries = append(tree.Entries, object.TreeEntry{Name: rest[:i], Mode: filemode.Dir, Hash: build(prefix + rest[:i] + "/")})
				}
				continue
			}
			o := st.NewEncodedObject()
			o.SetType(plumbing.BlobObject)
			w, _ := o.Writer()
			w.Write(files[p])
			w.Close()
			h, err := st.SetEncodedObject(o)
			if err != nil {
				t.Fatal(err)
			}
			tree.Entries = append(tree.Entries, object.TreeEntry{Name: rest, Mode: filemode.Regular, Hash: h})
		}
		key := func(e object.TreeEntry) string {
			if e.Mode == filemode.Dir {
				return e.Name + "/"
			}
			return e.Name
		}
		sort.Slice(tree.Entries, func(i, j int) bool { return key(tree.Entries[i]) < key(tree.Entries[j]) })
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
	sig := object.Signature{Name: "Callsheet", Email: "callsheet@invalid", When: time.Unix(0, 0).UTC()}
	c := &object.Commit{Author: sig, Committer: sig, Message: "fixture\n", TreeHash: build("")}
	if !parent.IsZero() {
		c.ParentHashes = []plumbing.Hash{parent}
	}
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

// policyRec is a synthetic stream record.
func policyRec(kind string, line int, blocks ...sessionBlock) sessionRecord {
	return sessionRecord{kind: kind, src: SourceRef{File: "s", FirstLine: line, LastLine: line}, blocks: blocks}
}

// use and result are synthetic tool use and result blocks.
func use(id, tool, args string) sessionBlock {
	return sessionBlock{typ: "tool_use", id: id, name: tool, input: json.RawMessage(args)}
}

func result(id, text string, isError bool) sessionBlock {
	return sessionBlock{typ: "tool_result", toolUseID: id, text: text, isError: isError}
}

// errorBody is a contract error body of code.
func errorBody(code contract.Code) string {
	return `{"error":{"code":"` + string(code) + `","message":"m"}}`
}

func judgeCallPolicy(t *testing.T, controls map[string][]mcpControl) {
	show := firstControl(t, controls, "task_show")
	logs := firstControl(t, controls, "task_logs")
	disp := firstControl(t, controls, "dispatch")
	auth := show.auth
	var dv contract.TaskView
	dv, _ = contract.ParseDispatchResponse([]byte(disp.answer))
	cancelArgs := `{"id":"` + dv.TaskID + `"}`
	terminal := auth.views[dv.TaskID]
	cancelAnswer := encodeAnswer(t, contract.CancelResponse{Version: contract.ProtocolVersion, TaskID: dv.TaskID, Task: terminal})
	stopped := cloneAuthority(auth)
	stopped.stopped, stopped.lastPhase = true, 1
	verdict := func(a *mcpAuthority, recs ...sessionRecord) string {
		out, _ := mcpAnalyze(map[int][]sessionRecord{1: recs}, a)
		return out[1]
	}
	ok := func(name string, got string) {
		if got != "" {
			t.Errorf("%s: refused (%s)", name, got)
		}
	}
	bad := func(name string, got string) {
		if got != devMCPCallInvalid {
			t.Errorf("%s: %q, want %s", name, got, devMCPCallInvalid)
		}
	}
	ok("valid diagnostic call", verdict(auth, policyRec("assistant", 1, use("u1", toolTaskShow, show.args)), policyRec("user", 2, result("u1", show.answer, false))))
	// Forbidden and unknown tools, with a valid pair, an error or no result;
	// task_wait is the stream qualifier's coordinator_long_poll.
	for _, n := range append(slices.Clone(mcpForbiddenTools[1:]), "task_peek") {
		tool := mcpToolPrefix + n
		bad(n+" with an answer", verdict(auth, policyRec("assistant", 1, use("u1", tool, `{}`)), policyRec("user", 2, result("u1", `{}`, false))))
		bad(n+" with an error", verdict(auth, policyRec("assistant", 1, use("u1", tool, `{}`)), policyRec("user", 2, result("u1", errorBody(contract.CodeNotFound), true))))
		bad(n+" without a result", verdict(stopped, policyRec("assistant", 1, use("u1", tool, `{}`))))
	}
	ok("task_wait is the long poll's", verdict(auth, policyRec("assistant", 1, use("u1", mcpToolPrefix+"task_wait", `{"task_ids":["`+dv.TaskID+`"]}`))))
	// Pairing.
	bad("reused tool-use ID", verdict(auth, policyRec("assistant", 1, use("u1", toolTaskShow, show.args)), policyRec("user", 2, result("u1", show.answer, false)),
		policyRec("assistant", 3, use("u1", toolTaskLogs, logs.args)), policyRec("user", 4, result("u1", logs.answer, false))))
	bad("orphaned result", verdict(auth, policyRec("user", 1, result("u9", show.answer, false))))
	bad("result before its use", verdict(auth, policyRec("user", 1, result("u1", show.answer, false)), policyRec("assistant", 2, use("u1", toolTaskShow, show.args))))
	ok("a vendor tool's result", verdict(auth, policyRec("assistant", 1, use("v1", "Bash", `{}`)), policyRec("user", 2, result("v1", "out", false))))
	// Results.
	bad("identical duplicate result", verdict(auth, policyRec("assistant", 1, use("u1", toolTaskShow, show.args)), policyRec("user", 2, result("u1", show.answer, false)),
		policyRec("user", 3, result("u1", show.answer, false))))
	bad("success-flagged error body", verdict(auth, policyRec("assistant", 1, use("u1", toolTaskShow, show.args)), policyRec("user", 2, result("u1", errorBody(contract.CodeNotFound), false))))
	bad("error-flagged success", verdict(auth, policyRec("assistant", 1, use("u1", toolTaskShow, show.args)), policyRec("user", 2, result("u1", show.answer, true))))
	ok("opaque error", verdict(auth, policyRec("assistant", 1, use("u1", toolTaskShow, show.args)), policyRec("user", 2, result("u1", "MCP error -32000: Connection closed", true))))
	for _, code := range []contract.Code{contract.CodeInvalidArgument, contract.CodeNotFound, contract.CodeConflict, contract.CodeUnavailable, contract.CodeTrustFailed,
		contract.CodeProtocolMismatch, contract.CodeNotImplemented, contract.CodeInternal} {
		ok("contract error "+string(code), verdict(auth, policyRec("assistant", 1, use("u1", toolTaskShow, show.args)), policyRec("user", 2, result("u1", errorBody(code), true))))
	}
	bad("invalid arguments with an error", verdict(auth, policyRec("assistant", 1, use("u1", toolTaskShow, `{"id":"`+dv.TaskID+`","lines":-1}`)),
		policyRec("user", 2, result("u1", errorBody(contract.CodeInvalidArgument), true))))
	bad("missing result, unstopped", verdict(auth, policyRec("assistant", 1, use("u1", toolTaskShow, show.args))))
	ok("missing result, stopped", verdict(stopped, policyRec("assistant", 1, use("u1", toolTaskShow, show.args))))
	ok("absent dispatch reply", verdict(auth, policyRec("assistant", 1, use("u1", toolDispatch, disp.args))))
	// The seed publication precedes workflow dispatch.
	push := firstControl(t, controls, "ws_push")
	bad("ws_push after dispatch", verdict(auth, policyRec("assistant", 1, use("u1", toolDispatch, disp.args)), policyRec("user", 2, result("u1", disp.answer, false)),
		policyRec("assistant", 3, use("u2", toolWsPush, push.args)), policyRec("user", 4, result("u2", push.answer, false))))
	bad("ws_push answered after dispatch", verdict(auth, policyRec("assistant", 1, use("u2", toolWsPush, push.args)),
		policyRec("assistant", 2, use("u1", toolDispatch, disp.args)), policyRec("user", 3, result("u2", push.answer, false)),
		policyRec("user", 4, result("u1", disp.answer, false))))
	ok("ws_push answered before dispatch", verdict(auth, policyRec("assistant", 1, use("u2", toolWsPush, push.args)),
		policyRec("user", 2, result("u2", push.answer, false)), policyRec("assistant", 3, use("u1", toolDispatch, disp.args)),
		policyRec("user", 4, result("u1", disp.answer, false))))
	// Enumeration linkage: a continuation needs its preceding page with
	// the same generation; interleaved enumerations are keyed apart.
	var first, cont mcpControl
	for _, c := range controls["ws_diff/task"] {
		if strings.Contains(c.label, "first") {
			first = c
		} else if strings.Contains(c.label, "continuation") {
			cont = c
		}
	}
	explicitFirst := firstControl(t, controls, "ws_diff/explicit")
	ok("linked pages", verdict(auth, policyRec("assistant", 1, use("u1", toolWsDiff, first.args)), policyRec("user", 2, result("u1", first.answer, false)),
		policyRec("assistant", 3, use("u3", toolWsDiff, explicitFirst.args)), policyRec("user", 4, result("u3", explicitFirst.answer, false)),
		policyRec("assistant", 5, use("u2", toolWsDiff, cont.args)), policyRec("user", 6, result("u2", cont.answer, false))))
	ok("partial enumeration", verdict(auth, policyRec("assistant", 1, use("u1", toolWsDiff, first.args)), policyRec("user", 2, result("u1", first.answer, false))))
	bad("continuation without its page", verdict(auth, policyRec("assistant", 1, use("u2", toolWsDiff, cont.args)), policyRec("user", 2, result("u2", cont.answer, false))))
	bad("first page answered after its continuation", verdict(auth, policyRec("assistant", 1, use("u1", toolWsDiff, first.args)),
		policyRec("assistant", 2, use("u2", toolWsDiff, cont.args)), policyRec("user", 3, result("u2", cont.answer, false)),
		policyRec("user", 4, result("u1", first.answer, false))))
	bad("first page answered after its continuation's request", verdict(auth, policyRec("assistant", 1, use("u1", toolWsDiff, first.args)),
		policyRec("assistant", 2, use("u2", toolWsDiff, cont.args)), policyRec("user", 3, result("u1", first.answer, false)),
		policyRec("user", 4, result("u2", cont.answer, false))))
	regen := strings.ReplaceAll(cont.answer, inspectGeneration, strings.Repeat("2", 32))
	bad("generation change", verdict(auth, policyRec("assistant", 1, use("u1", toolWsDiff, first.args)), policyRec("user", 2, result("u1", first.answer, false)),
		policyRec("assistant", 3, use("u2", toolWsDiff, strings.ReplaceAll(cont.args, inspectGeneration, strings.Repeat("2", 32)))),
		policyRec("user", 4, result("u2", regen, false))))
	// Cancellation eligibility: a stopped attempt's final phase, an owned
	// task dispatched before, no dispatch after the first cancellation.
	dispatchRecs := []sessionRecord{policyRec("assistant", 1, use("d1", toolDispatch, disp.args)), policyRec("user", 2, result("d1", disp.answer, false))}
	cancel := []sessionRecord{policyRec("assistant", 3, use("c1", toolTaskCancel, cancelArgs)), policyRec("user", 4, result("c1", cancelAnswer, false))}
	ok("eligible cancellation", verdict(stopped, append(slices.Clone(dispatchRecs), cancel...)...))
	ok("eligible error-answered cancellation", verdict(stopped, append(slices.Clone(dispatchRecs), policyRec("assistant", 3, use("c1", toolTaskCancel, cancelArgs)),
		policyRec("user", 4, result("c1", errorBody(contract.CodeConflict), true)))...))
	bad("cancellation without a stop", verdict(auth, append(slices.Clone(dispatchRecs), cancel...)...))
	nonfinal := cloneAuthority(stopped)
	nonfinal.lastPhase = 2
	bad("cancellation in a nonfinal phase", verdict(nonfinal, append(slices.Clone(dispatchRecs), cancel...)...))
	unowned := cloneAuthority(stopped)
	delete(unowned.owned, dv.TaskID)
	bad("cancellation of an unowned task", verdict(unowned, append(slices.Clone(dispatchRecs), cancel...)...))
	bad("cancellation before the dispatch reply", verdict(stopped, policyRec("assistant", 1, use("d1", toolDispatch, disp.args)),
		policyRec("assistant", 2, use("c1", toolTaskCancel, cancelArgs)), policyRec("user", 3, result("c1", cancelAnswer, false)),
		policyRec("user", 4, result("d1", disp.answer, false))))
	bad("error-answered cancellation before dispatch", verdict(stopped, policyRec("assistant", 1, use("c1", toolTaskCancel, cancelArgs)),
		policyRec("user", 2, result("c1", errorBody(contract.CodeNotFound), true))))
	// Lost-reply recovery for cancellation ownership: the listing must be
	// requested after the error answer.
	listing := encodeAnswer(t, contract.TaskListResponse{Version: contract.ProtocolVersion, Tasks: []contract.TaskSummary{{TaskID: terminal.TaskID,
		Target: terminal.Request.Target, Role: terminal.Role, State: terminal.State, CreatedAt: terminal.CreatedAt, StartedAt: terminal.StartedAt,
		FinishedAt: terminal.FinishedAt, ElapsedMS: terminal.ElapsedMS, Effective: terminal.Effective, RequestedBy: terminal.Request.RequestedBy,
		DurabilityConfirmed: terminal.DurabilityConfirmed}}})
	recovered := func(listFirst bool) []sessionRecord {
		lost := policyRec("user", 3, result("d1", "MCP error -32001: Request timed out", true))
		list := []sessionRecord{policyRec("assistant", 2, use("l1", toolTaskList, `{}`)), policyRec("user", 4, result("l1", listing, false))}
		if !listFirst {
			lost.src.FirstLine, list[0].src.FirstLine = 2, 3
		}
		recs := []sessionRecord{policyRec("assistant", 1, use("d1", toolDispatch, disp.args))}
		if listFirst {
			recs = append(recs, list[0], lost, list[1])
		} else {
			recs = append(recs, lost, list[0], list[1])
		}
		return append(recs, policyRec("assistant", 5, use("c1", toolTaskCancel, cancelArgs)), policyRec("user", 6, result("c1", cancelAnswer, false)))
	}
	ok("cancellation after a recovered lost reply", verdict(stopped, recovered(false)...))
	// Overlapping recovery listings: A requested first but answered after
	// the cancellation, B requested later and answered before it; B's
	// answer recovers the task in time.
	ok("cancellation after an overlapping listing's earlier answer", verdict(stopped,
		policyRec("assistant", 1, use("d1", toolDispatch, disp.args)), policyRec("user", 2, result("d1", "MCP error -32001: Request timed out", true)),
		policyRec("assistant", 3, use("la", toolTaskList, `{}`)), policyRec("assistant", 4, use("lb", toolTaskList, `{}`)),
		policyRec("user", 5, result("lb", listing, false)), policyRec("assistant", 6, use("c1", toolTaskCancel, cancelArgs)),
		policyRec("user", 7, result("c1", cancelAnswer, false)), policyRec("user", 8, result("la", listing, false))))
	bad("cancellation before every overlapping listing's answer", verdict(stopped,
		policyRec("assistant", 1, use("d1", toolDispatch, disp.args)), policyRec("user", 2, result("d1", "MCP error -32001: Request timed out", true)),
		policyRec("assistant", 3, use("la", toolTaskList, `{}`)), policyRec("assistant", 4, use("lb", toolTaskList, `{}`)),
		policyRec("assistant", 5, use("c1", toolTaskCancel, cancelArgs)), policyRec("user", 6, result("c1", cancelAnswer, false)),
		policyRec("user", 7, result("lb", listing, false)), policyRec("user", 8, result("la", listing, false))))
	bad("cancellation after a listing requested before the reply was lost", verdict(stopped, recovered(true)...))
	d2 := firstControl(t, controls, "dispatch")
	bad("dispatch after the first cancellation", verdict(stopped, append(append(slices.Clone(dispatchRecs), cancel...),
		policyRec("assistant", 5, use("d2", toolDispatch, d2.args)), policyRec("user", 6, result("d2", d2.answer, false)))...))
}
