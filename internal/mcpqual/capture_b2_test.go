package mcpqual

import (
	"bytes"
	"encoding/json"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
)

// Design decoder-enrollment B2, UT-20 (FP-20): the Cursor scoped
// permission file. The exact bytes for exactly the pinned adapter, written
// before the approval inventories (harness baseline, never approval
// evidence), checked after the enable command, before the session and
// after its cleanup; creation refusals; capture only (qualify writes
// nothing and reports nothing new); the manifest record's strict schema
// and consistency; no grant for another version or platform.

// permissionSeen records, per launch kind, what the launched process saw
// at <workspace>/.cursor/cli.json.
type permissionSeen struct {
	mu    sync.Mutex
	bytes map[string]string
	ident map[string]fs.FileInfo
}

func (p *permissionSeen) look(spec ProcSpec) {
	f := filepath.Join(spec.Dir, ".cursor", cursorPermissionFile)
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.bytes == nil {
		p.bytes, p.ident = map[string]string{}, map[string]fs.FileInfo{}
	}
	b, err := os.ReadFile(f)
	info, _ := os.Lstat(f)
	if err != nil {
		p.bytes[launchKind(spec)] = "<absent>"
		return
	}
	p.bytes[launchKind(spec)], p.ident[launchKind(spec)] = string(b), info
}

// permissionRun is cursorRun recording the file at enable and session,
// with optional extra behaviour for either.
func permissionRun(t *testing.T, enable, session func(spec ProcSpec, b *capBehavior), mutate func(c *CaptureRunner)) (*permissionSeen, *capWorld, *CaptureManifest, CaptureClient) {
	t.Helper()
	seen := &permissionSeen{}
	w := newCapWorld(t)
	home := filepath.Join(t.TempDir(), "home")
	os.MkdirAll(filepath.Join(home, ".cursor"), 0o700)
	w.script = func(spec ProcSpec) capBehavior {
		b := w.defaults(spec)
		switch launchKind(spec) {
		case "enable":
			seen.look(spec)
			if enable != nil {
				enable(spec, &b)
			}
		case "session":
			seen.look(spec)
			if session != nil {
				session(spec, &b)
			}
		}
		return b
	}
	man, _, _ := b1Capture(t, w, capPlan(t, "cursor"), func(c *CaptureRunner) {
		c.Home = home
		if mutate != nil {
			mutate(c)
		}
	})
	return seen, w, man, capClient(t, man, "cursor")
}

func TestCursorToolPermissionFile(t *testing.T) {
	seen, w, man, cc := permissionRun(t, nil, nil, nil)
	tp := cc.ToolPermission
	if cc.State != CaptureComplete || tp == nil || tp.State != PermissionVerified || tp.Reason != nil || tp.Adapter != CursorToolPermissionAdapter ||
		tp.Path != "<workspace>/.cursor/cli.json" || tp.Content != `{"permissions":{"allow":["Mcp(probe:slow)"]}}` || tp.SHA256 != sha256Hex([]byte(tp.Content)) {
		t.Fatalf("permission record %+v (%q)", tp, reasonOf(cc))
	}
	// The exact bytes, no trailing newline, present before the enable
	// command (baseline state) and at the session, the same regular file.
	if seen.bytes["enable"] != CursorToolPermissionContent || seen.bytes["session"] != CursorToolPermissionContent || !os.SameFile(seen.ident["enable"], seen.ident["session"]) ||
		seen.ident["enable"].Mode() != 0o600 {
		t.Fatalf("seen %+v", seen.bytes)
	}
	// The harness file is never an approval change, and the session argv
	// is unchanged (no force, yolo or approve-mcps).
	for _, ch := range cc.Approval.Changes {
		if ch.Path == labelToolPermission {
			t.Fatalf("the harness file counted as approval evidence: %+v", cc.Approval.Changes)
		}
	}
	for _, spec := range w.launches {
		for _, a := range spec.Args {
			if strings.Contains(a, "--force") || strings.Contains(a, "--yolo") || strings.Contains(a, "--approve-mcps") {
				t.Fatalf("a broad grant %q", a)
			}
		}
	}
	// The manifest round-trips the record, which holds no owner data.
	mb, _ := encodeIndent(man)
	again, err := ParseCaptureManifest(mb)
	if err != nil || !reflect.DeepEqual(again.Clients[0].ToolPermission, tp) || bytes.Contains(mb, []byte("/home/")) {
		t.Fatalf("round trip %v", err)
	}
	// No enable change, no positive approval: the harness file is no
	// approval evidence by itself.
	_, w2, _, cc2 := permissionRun(t, func(_ ProcSpec, b *capBehavior) { b.writes = nil }, nil, nil)
	if !strings.HasPrefix(reasonOf(cc2), ReasonCursorScopeUnverified+": no change") || slices.Contains(w2.kinds(), "cursor:session") ||
		cc2.ToolPermission == nil || cc2.ToolPermission.State != PermissionWritten {
		t.Fatalf("no approval: %q %+v", reasonOf(cc2), cc2.ToolPermission)
	}
}

func TestCursorToolPermissionMutation(t *testing.T) {
	for name, tc := range map[string]struct {
		enable, session func(spec ProcSpec, b *capBehavior)
		launched        bool
		want            string
	}{
		// The enable command rewrites, removes or replaces the file.
		"enable-modifies": {func(spec ProcSpec, b *capBehavior) {
			b.writes[".cursor/"+cursorPermissionFile] = strp(`{"permissions":{"allow":["Mcp(*:*)"]}}`)
		}, nil, false, ReasonCursorScopeUnverified + ": the harness-written <workspace>/.cursor/cli.json changed during the enable command"},
		"enable-removes": {func(spec ProcSpec, b *capBehavior) { b.writes[".cursor/"+cursorPermissionFile] = nil }, nil, false,
			ReasonCursorScopeUnverified + ": the harness-written <workspace>/.cursor/cli.json changed during the enable command"},
		"enable-replaces-same-bytes": {func(spec ProcSpec, b *capBehavior) {
			// The same bytes and mode in a new file renamed over it: not the
			// created file.
			p := filepath.Join(spec.Dir, ".cursor", cursorPermissionFile)
			os.WriteFile(p+".new", []byte(CursorToolPermissionContent), 0o600)
			os.Rename(p+".new", p)
		}, nil, false, ReasonCursorScopeUnverified + ": the harness-written <workspace>/.cursor/cli.json"},
		// The session changes it: the completed capture becomes partial.
		"session-modifies": {nil, func(spec ProcSpec, b *capBehavior) {
			b.writes = map[string]*string{".cursor/" + cursorPermissionFile: strp(`{}`)}
		}, true, ReasonCursorScopeUnverified + ": the harness-written <workspace>/.cursor/cli.json was removed, replaced or changed"},
		"session-removes": {nil, func(spec ProcSpec, b *capBehavior) {
			b.writes = map[string]*string{".cursor/" + cursorPermissionFile: nil}
		}, true, ReasonCursorScopeUnverified + ": the harness-written <workspace>/.cursor/cli.json was removed, replaced or changed"},
	} {
		_, w, man, cc := permissionRun(t, tc.enable, tc.session, nil)
		tp := cc.ToolPermission
		if !strings.HasPrefix(reasonOf(cc), tc.want) || cc.State != CapturePartial || man.ExitCode() != 5 || slices.Contains(w.kinds(), "cursor:session") != tc.launched ||
			tp == nil || tp.State != PermissionWritten || tp.Reason == nil {
			t.Fatalf("%s: %q %+v %v", name, reasonOf(cc), tp, w.kinds())
		}
	}
	// A session-time change keeps the session's own failure reason first.
	_, _, _, cc := permissionRun(t, nil, func(spec ProcSpec, b *capBehavior) {
		b.writes = map[string]*string{".cursor/" + cursorPermissionFile: nil}
		b.exit = 1
	}, nil)
	if reasonOf(cc) != ReasonSessionExit || cc.ToolPermission.State != PermissionWritten || *cc.ToolPermission.Reason != permissionFailed {
		t.Fatalf("session failure first: %q %+v", reasonOf(cc), cc.ToolPermission)
	}
	// A rejected tool with a final success envelope (no probe call)
	// stays partial although the file verified.
	_, _, _, cc = permissionRun(t, nil, func(spec ProcSpec, b *capBehavior) {
		b.stdout = `{"type":"tool_call","subtype":"completed","tool_call":{"mcpToolCall":{"args":{"providerIdentifier":"probe","toolName":"slow"},"result":{"rejected":{"reason":"User rejected MCP: probe-slow"}}}}}` + "\n" +
			`{"type":"result","subtype":"success","is_error":false,"result":"done"}` + "\n"
		b.events = func(cf CaseFile, caseID string) string {
			name, ver := "cursor-agent", "1"
			return probeLog(cf.RunID, ProbeEvent{Kind: EvStart}, ProbeEvent{Kind: EvInitialize, ClientName: &name, ClientVersion: &ver}, ProbeEvent{Kind: EvEOF},
				ProbeEvent{Kind: EvExit, Reason: "eof"})
		}
	}, nil)
	if reasonOf(cc) != ReasonProbeNotObserved || cc.State != CapturePartial || cc.ToolPermission.State != PermissionVerified {
		t.Fatalf("rejected tool: %q %+v", reasonOf(cc), cc.ToolPermission)
	}
}

func TestCursorToolPermissionAdapter(t *testing.T) {
	// Another platform or version: no file, no record (the B1.5 project
	// adapter's identity is the same; other versions keep the old recipe).
	for name, mutate := range map[string]func(c *CaptureRunner){
		"darwin": func(c *CaptureRunner) { c.GOOS = "darwin" },
		"arm64":  func(c *CaptureRunner) { c.GOARCH = "arm64" },
		"version": func(c *CaptureRunner) {
			p := *c.Plan
			p.Clients = append([]PlanClient(nil), c.Plan.Clients...)
			p.Clients[0].ExpectedVersion = "2026.10.02-aaaaaaa"
			c.Plan = &p
		},
	} {
		seen, _, _, cc := permissionRun(t, nil, nil, mutate)
		if cc.ToolPermission != nil || seen.bytes["enable"] != "<absent>" && seen.bytes["enable"] != "" {
			t.Fatalf("%s: %+v %v", name, cc.ToolPermission, seen.bytes)
		}
	}
	if cursorToolPermissionApplies("linux", "amd64", "2026.10.01-e373342x") || !cursorToolPermissionApplies("linux", "amd64", "2026.10.01-e373342") {
		t.Fatal("adapter identity")
	}
	// No runner, plan or environment field selects or overrides it.
	for _, typ := range []reflect.Type{reflect.TypeOf(CaptureRunner{}), reflect.TypeOf(Env{}), reflect.TypeOf(PlanClient{}), reflect.TypeOf(Plan{})} {
		for i := 0; i < typ.NumField(); i++ {
			if n := strings.ToLower(typ.Field(i).Name); strings.Contains(n, "permission") {
				t.Fatalf("%s.%s overrides the permission adapter", typ.Name(), typ.Field(i).Name)
			}
		}
	}
}

// The creation refusals, before any enable or model launch: a configuration
// outside .cursor, a non-directory or linked workspace or .cursor, an
// existing cli.json (never merged or overwritten) and an unwritable
// directory.
func TestCursorToolPermissionCreate(t *testing.T) {
	c := &CaptureRunner{approvalFS: osApprovalFS{}}
	ws := func(t *testing.T) caseInputs {
		d := filepath.Join(realDir(t), "ws")
		os.MkdirAll(filepath.Join(d, ".cursor"), 0o700)
		return caseInputs{ws: d, configPath: filepath.Join(d, ".cursor", "mcp.json")}
	}
	in := ws(t)
	cc := &CaptureClient{}
	info, reason := c.writeCursorPermission(cc, in)
	if reason != "" || info == nil || cc.ToolPermission == nil || cc.ToolPermission.State != PermissionWritten {
		t.Fatalf("create: %q", reason)
	}
	if b, _ := os.ReadFile(filepath.Join(in.ws, ".cursor", "cli.json")); string(b) != CursorToolPermissionContent {
		t.Fatalf("content %q", b)
	}
	if why := c.verifyCursorPermission(cc, in, info); why != "" {
		t.Fatal(why)
	}
	for name, setup := range map[string]func(in *caseInputs){
		"existing": func(in *caseInputs) {
			os.WriteFile(filepath.Join(in.ws, ".cursor", "cli.json"), []byte(`{"permissions":{"deny":["Shell(*)"]}}`), 0o600)
		},
		"existing-link": func(in *caseInputs) { os.Symlink("/etc/hostname", filepath.Join(in.ws, ".cursor", "cli.json")) },
		"cursor-link": func(in *caseInputs) {
			os.RemoveAll(filepath.Join(in.ws, ".cursor"))
			other := filepath.Join(filepath.Dir(in.ws), "elsewhere")
			os.MkdirAll(other, 0o700)
			os.Symlink(other, filepath.Join(in.ws, ".cursor"))
		},
		"cursor-file": func(in *caseInputs) {
			os.RemoveAll(filepath.Join(in.ws, ".cursor"))
			os.WriteFile(filepath.Join(in.ws, ".cursor"), nil, 0o600)
		},
		"cursor-missing": func(in *caseInputs) { os.RemoveAll(filepath.Join(in.ws, ".cursor")) },
		"other-config":   func(in *caseInputs) { in.configPath = filepath.Join(in.ws, ".grok", "config.toml") },
		"unwritable": func(in *caseInputs) {
			os.Chmod(filepath.Join(in.ws, ".cursor"), 0o500)
		},
	} {
		in := ws(t)
		setup(&in)
		before, _ := os.ReadFile(filepath.Join(in.ws, ".cursor", "cli.json"))
		cc := &CaptureClient{}
		if _, reason := c.writeCursorPermission(cc, in); !strings.HasPrefix(reason, ReasonCursorScopeUnverified+": the scoped permission file was not created") || cc.ToolPermission != nil {
			t.Errorf("%s: %q %+v", name, reason, cc.ToolPermission)
		}
		os.Chmod(filepath.Join(in.ws, ".cursor"), 0o700)
		if after, _ := os.ReadFile(filepath.Join(in.ws, ".cursor", "cli.json")); !bytes.Equal(before, after) {
			t.Errorf("%s: an owner file was changed", name)
		}
	}
	// Verification failures: removed, replaced, changed, linked.
	for name, change := range map[string]func(p string){
		"removed": func(p string) { os.Remove(p) },
		"replaced": func(p string) {
			os.WriteFile(p+".new", []byte(CursorToolPermissionContent), 0o600)
			os.Rename(p+".new", p)
		},
		"changed": func(p string) { os.WriteFile(p, []byte(`{"permissions":{"allow":["Mcp(probe:fast)"]}}`), 0o600) },
		"grown":   func(p string) { os.WriteFile(p, []byte(CursorToolPermissionContent+"\n"), 0o600) },
		"linked":  func(p string) { os.Remove(p); os.Symlink("/etc/hostname", p) },
	} {
		in := ws(t)
		cc := &CaptureClient{}
		info, _ := c.writeCursorPermission(cc, in)
		change(filepath.Join(in.ws, ".cursor", "cli.json"))
		if why := c.verifyCursorPermission(cc, in, info); !strings.HasPrefix(why, ReasonCursorScopeUnverified) || cc.ToolPermission.State != PermissionWritten ||
			*cc.ToolPermission.Reason != permissionFailed {
			t.Errorf("%s: %q", name, why)
		}
	}
}

// Capture only: the shared case preparation (qualify's too) writes no
// permission file, only the capture session calls the helper, and the
// qualification report gains no tool_permission field.
func TestCursorToolPermissionCaptureOnly(t *testing.T) {
	p := capPlan(t, "cursor")
	r := &Runner{Plan: p, OutDir: filepath.Join(realDir(t), "out"), GOOS: "linux", GOARCH: "amd64", ServerPath: "/opt/mcpqual", RunID: "r", Nonce: "n"}
	in, err := r.prepareCase(&p.Clients[0], caseSpec{id: "cursor-setup", setting: "default"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(filepath.Join(in.ws, ".cursor", "cli.json")); err == nil {
		t.Fatal("the shared case preparation wrote the permission file")
	}
	if info, err := os.Lstat(filepath.Join(in.ws, ".cursor")); err != nil || !info.IsDir() {
		t.Fatal("prepareCase did not create the workspace .cursor directory")
	}
	src := map[string]int{}
	for _, f := range []string{"session.go", "measure.go", "runner.go", "capture.go", "capture_approval.go"} {
		b, _ := os.ReadFile(f)
		src[f] = strings.Count(string(b), ".writeCursorPermission(")
	}
	if src["capture.go"] != 1 || src["session.go"]+src["measure.go"]+src["runner.go"] != 0 {
		t.Fatalf("permission helper callers %v", src)
	}
	for _, typ := range []reflect.Type{reflect.TypeOf(Report{}), reflect.TypeOf(ClientReport{}), reflect.TypeOf(CaseReport{})} {
		for i := 0; i < typ.NumField(); i++ {
			if strings.Contains(typ.Field(i).Tag.Get("json"), "permission") {
				t.Fatalf("%s has a permission field", typ.Name())
			}
		}
	}
}

// The manifest record: strict members, exact identity and content, state
// rules, consistency with the session and the approval, and completeness.
func TestCursorToolPermissionManifest(t *testing.T) {
	_, _, man, _ := permissionRun(t, nil, nil, nil)
	base, _ := encodeIndent(man)
	edit := func(f func(c map[string]any)) []byte {
		var m map[string]any
		json.Unmarshal(base, &m)
		f(m["clients"].([]any)[0].(map[string]any))
		b, _ := encodeIndent(m)
		return b
	}
	tpOf := func(c map[string]any) map[string]any { return c["tool_permission"].(map[string]any) }
	if _, err := ParseCaptureManifest(base); err != nil {
		t.Fatal(err)
	}
	for name, tc := range map[string]struct {
		b    []byte
		want string
	}{
		"null":             {edit(func(c map[string]any) { c["tool_permission"] = nil }), "explicit null"},
		"unknown-member":   {edit(func(c map[string]any) { tpOf(c)["extra"] = 1 }), "unknown field"},
		"missing-member":   {edit(func(c map[string]any) { delete(tpOf(c), "reason") }), "missing"},
		"null-state":       {edit(func(c map[string]any) { tpOf(c)["state"] = nil }), "null"},
		"adapter":          {edit(func(c map[string]any) { tpOf(c)["adapter"] = CursorProjectAdapter }), "exact file path"},
		"path":             {edit(func(c map[string]any) { tpOf(c)["path"] = "<home>/.cursor/cli.json" }), "exact file path"},
		"content-wildcard": {edit(func(c map[string]any) { tpOf(c)["content"] = `{"permissions":{"allow":["Mcp(probe:*)"]}}` }), "exact file path"},
		"hash":             {edit(func(c map[string]any) { tpOf(c)["sha256"] = strings.Repeat("a", 64) }), "exact file path"},
		"state":            {edit(func(c map[string]any) { tpOf(c)["state"] = "granted" }), "state"},
		"verified-reason":  {edit(func(c map[string]any) { tpOf(c)["reason"] = "x" }), "verified with a reason"},
		"written-complete": {edit(func(c map[string]any) { tpOf(c)["state"] = PermissionWritten; tpOf(c)["reason"] = "x" }), "unverified permission file"},
		"written-no-reason": {edit(func(c map[string]any) {
			tpOf(c)["state"] = PermissionWritten
			tpOf(c)["reason"] = nil
		}), "written without a reason"},
		"complete-without-record": {edit(func(c map[string]any) { delete(c, "tool_permission") }), "without its verified permission record"},
		"approval-change": {edit(func(c map[string]any) {
			a := c["approval"].(map[string]any)
			a["changes"] = append(a["changes"].([]any), map[string]any{"path": "<workspace>/.cursor/cli.json", "kind": "modified"})
		}), "change of the harness-written"},
		"other-platform": {bytes.Replace(base, []byte(`"arch": "amd64"`), []byte(`"arch": "arm64"`), 1), "tool_permission: only the trusted cursor recipe"},
	} {
		if _, err := ParseCaptureManifest(tc.b); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: %v", name, err)
		}
	}
	// A record on another client, or verified without a session.
	m, _ := ParseCaptureManifest(base)
	c := m.Clients[0]
	c.ID = "codex"
	if err := c.validateToolPermission("linux", "amd64"); err == nil {
		t.Fatal("a codex record validated")
	}
	c = m.Clients[0]
	c.Session.State, c.State = StagePrepared, CapturePartial
	if err := c.validateToolPermission("linux", "amd64"); err == nil || !strings.Contains(err.Error(), "without a launched session") {
		t.Fatalf("verified without a session: %v", err)
	}
	// A legacy (absent) record on a partial capture stays valid.
	c = m.Clients[0]
	c.ToolPermission, c.State = nil, CapturePartial
	if err := c.validateToolPermission("linux", "amd64"); err != nil {
		t.Fatal(err)
	}
}
