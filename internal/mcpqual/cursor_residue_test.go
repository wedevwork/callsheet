package mcpqual

import (
	"encoding/json"
	"io/fs"
	"reflect"
	"strings"
	"testing"
)

// Design decoder-enrollment B3, UT-25 (FP-25): run-attributed worker
// socket residue on injected filesystem records (no socket is created,
// opened, connected to or removed here): the exact admission predicate,
// both observed truncation patterns through fabricated equivalent paths,
// every wrong shape, extra and multiple candidates, bounds, replacement and
// disappearance, a pre-existing or another run's socket refused, the
// same-run ledger only, and the strict normalized evidence.

const (
	rsHome = "/home/o"
	rsWS1  = "/c/out/.work/cursor-setup"
	rsWS2  = "/c/out/.work/cursor-default-1"
)

var rsProjects = rsHome + "/.cursor/projects"

// rsWorld is a tiny home and two case workspaces.
func rsWorld() *memFS {
	m := newMemFS()
	m.file(rsWS1+"/.cursor/mcp.json", "{}")
	m.file(rsWS2+"/.cursor/mcp.json", "{}")
	m.mkdirs(rsProjects + "/c-out-work-cursor-setup")
	m.file(rsProjects+"/c-out-work-cursor-setup/mcp-approvals.json", `["probe-0123456789abcdef"]`)
	return m
}

// socketAt adds a worker socket residue directory name holding entries
// (name -> mode; worker.sock a socket when mode is 0).
func socketAt(m *memFS, name string, entries map[string]fs.FileMode) {
	m.mkdirs(rsProjects + "/" + name)
	if entries == nil {
		entries = map[string]fs.FileMode{cursorWorkerSocket: fs.ModeSocket | 0o755}
	}
	for e, mode := range entries {
		m.nodes[rsProjects+"/"+name+"/"+e] = &memNode{mode: mode}
	}
}

func rsPreparer(m *memFS, lim ApprovalScanLimits) *cursorPreparer {
	return newCursorPreparer(m, lim, "linux", "amd64", CursorRealVersion, rsHome)
}

// session checks one case: the baseline, then after (the vendor's
// session), then the check.
func session(t *testing.T, p *cursorPreparer, caseID, ws string, after func()) WorkerResidue {
	t.Helper()
	pre, err := p.residueSnapshot()
	if err != nil {
		t.Fatalf("%s baseline: %v", caseID, err)
	}
	if after != nil {
		after()
	}
	return p.checkResidue(caseID, ws, pre)
}

func TestWorkerResidueAdmitted(t *testing.T) {
	// The two observed truncation patterns, as equivalent fabricated paths:
	// "...-work-cursor--e136e0a" (the slug cut inside the case basename after
	// its hyphen, k=7) and "...-work-cu-1b81996" (k=2).
	for _, name := range []string{"c-out-work-cursor--e136e0a", "c-out-work-cu-1b81996"} {
		m := rsWorld()
		p := rsPreparer(m, ApprovalScanLimits{})
		w := session(t, p, "cursor-setup", rsWS1, func() { socketAt(m, name, nil) })
		want := WorkerResidueEntry{CaseID: "cursor-setup", Path: "<home>/.cursor/projects/<case-residue-1>/worker.sock", Origin: ResidueOrigin,
			PrefixVerified: true, SuffixVerified: true, SocketTypeVerified: true, SoleEntryVerified: true, IdentityVerified: true}
		if w.State != ResidueVerified || w.Reason != nil || len(w.Entries) != 1 || w.Entries[0] != want || w.Policy != WorkerResiduePolicy {
			t.Fatalf("%s: %+v", name, w)
		}
		if err := w.validate(func(id string) bool { return id == "cursor-setup" }, 1); err != nil {
			t.Fatal(err)
		}
		// No raw slug, suffix or inode in the evidence.
		b, _ := json.Marshal(w)
		if strings.Contains(string(b), "c-out-work") || strings.Contains(string(b), name[len(name)-7:]) {
			t.Fatalf("raw name in %s", b)
		}
		// The next case of the same run: the ledger entry is revalidated and
		// exempt, the new one admitted as residue 2.
		w2 := session(t, p, "cursor-default-1", rsWS2, func() { socketAt(m, "c-out-work-cursor-defa-0a1b2c3", nil) })
		if w2.State != ResidueVerified || len(w2.Entries) != 2 || w2.Entries[0].CaseID != "cursor-setup" || w2.Entries[1].Path != residuePath(2) {
			t.Fatalf("next case: %+v", w2)
		}
		// A later approval inventory of this run passes over both sockets; a
		// fresh invocation (empty ledger) fails closed on them.
		roots, _ := approvalRoots(m, rsWS2, rsHome)
		sc, _ := p.scanner()
		if s := sc.snapshot(roots); !s.complete {
			t.Fatalf("ledger scan: %s", s.why)
		}
		fresh := rsPreparer(m, ApprovalScanLimits{})
		sc, _ = fresh.scanner()
		if s := sc.snapshot(roots); s.complete || !strings.Contains(s.why, "special file") {
			t.Fatalf("a new invocation accepted leftovers: %+v", s.why)
		}
		if _, err := fresh.residueSnapshot(); err != nil {
			t.Fatalf("a fresh baseline lists, never judges: %v", err)
		}
		w3 := session(t, fresh, "cursor-setup", rsWS1, nil)
		if w3.State != ResidueUnverified || !strings.Contains(deref(w3.Reason), "special file") {
			t.Fatalf("pre-existing sockets: %+v", w3)
		}
		// Nothing was removed.
		if _, ok := m.nodes[rsProjects+"/"+name+"/worker.sock"]; !ok {
			t.Fatal("a socket disappeared")
		}
	}
	// No residue at all: verified with no entries (the check ran).
	m := rsWorld()
	if w := session(t, rsPreparer(m, ApprovalScanLimits{}), "cursor-setup", rsWS1, nil); w.State != ResidueVerified || len(w.Entries) != 0 {
		t.Fatalf("no residue: %+v", w)
	}
	// An ordinary new project directory is not residue and not refused.
	m = rsWorld()
	w := session(t, rsPreparer(m, ApprovalScanLimits{}), "cursor-setup", rsWS1, func() {
		m.file(rsProjects+"/other-project/notes.json", "{}")
	})
	if w.State != ResidueVerified || len(w.Entries) != 0 {
		t.Fatalf("ordinary write: %+v", w)
	}
}

func TestWorkerResidueRefused(t *testing.T) {
	sock := fs.ModeSocket | 0o755
	for name, tc := range map[string]struct {
		after func(m *memFS)
		want  string
		flags *WorkerResidueEntry
	}{
		"suffix-short":  {func(m *memFS) { socketAt(m, "c-out-work-cu-1b8199", nil) }, "unverifiable", &WorkerResidueEntry{SocketTypeVerified: true, SoleEntryVerified: true, IdentityVerified: true}},
		"suffix-upper":  {func(m *memFS) { socketAt(m, "c-out-work-cu-1B81996", nil) }, "unverifiable", &WorkerResidueEntry{PrefixVerified: true, SocketTypeVerified: true, SoleEntryVerified: true, IdentityVerified: true}},
		"prefix-k1":     {func(m *memFS) { socketAt(m, "c-out-work-c-1b81996", nil) }, "unverifiable", &WorkerResidueEntry{SuffixVerified: true, SocketTypeVerified: true, SoleEntryVerified: true, IdentityVerified: true}},
		"prefix-parent": {func(m *memFS) { socketAt(m, "c-out-work-1b81996", nil) }, "unverifiable", nil},
		"prefix-cut":    {func(m *memFS) { socketAt(m, "c-out-wo-1b81996", nil) }, "unverifiable", nil},
		"full-slug":     {func(m *memFS) { socketAt(m, "c-out-work-cursor-setup-1b81996", nil) }, "unverifiable", nil},
		"other-case":    {func(m *memFS) { socketAt(m, "c-out-work-cursor-defa-1b81996", nil) }, "unverifiable", nil},
		"other-parent":  {func(m *memFS) { socketAt(m, "c-other-work-cu-1b81996", nil) }, "unverifiable", nil},
		"regular": {func(m *memFS) {
			socketAt(m, "c-out-work-cu-1b81996", map[string]fs.FileMode{cursorWorkerSocket: 0o600})
		}, "unverifiable", nil},
		"fifo": {func(m *memFS) {
			socketAt(m, "c-out-work-cu-1b81996", map[string]fs.FileMode{cursorWorkerSocket: fs.ModeNamedPipe | 0o600})
		}, "unverifiable", nil},
		"link": {func(m *memFS) {
			socketAt(m, "c-out-work-cu-1b81996", map[string]fs.FileMode{cursorWorkerSocket: fs.ModeSymlink | 0o777})
		}, "unverifiable", nil},
		"device": {func(m *memFS) {
			socketAt(m, "c-out-work-cu-1b81996", map[string]fs.FileMode{cursorWorkerSocket: fs.ModeDevice | 0o600})
		}, "unverifiable", nil},
		"other-name": {func(m *memFS) { socketAt(m, "c-out-work-cu-1b81996", map[string]fs.FileMode{"agent.sock": sock}) }, "unverifiable", nil},
		"extra-child": {func(m *memFS) {
			socketAt(m, "c-out-work-cu-1b81996", map[string]fs.FileMode{cursorWorkerSocket: sock, "x.json": 0o600})
		}, "unverifiable", nil},
		"two-candidates": {func(m *memFS) { socketAt(m, "c-out-work-cu-1b81996", nil); socketAt(m, "c-out-work-cur-2b81996", nil) }, "more than one", nil},
		"dir-link":       {func(m *memFS) { m.nodes[rsProjects+"/c-out-work-cu-1b81996"] = &memNode{mode: fs.ModeSymlink | 0o777} }, "special file or link", nil},
		"elsewhere":      {func(m *memFS) { m.nodes[rsWS1+"/.cursor/worker.sock"] = &memNode{mode: sock} }, "special file", nil},
		"nested": {func(m *memFS) {
			m.mkdirs(rsProjects + "/x/y")
			m.nodes[rsProjects+"/x/y/worker.sock"] = &memNode{mode: sock}
		}, "special file", nil},
		"replaced-old":    {func(m *memFS) { m.nodes[rsProjects+"/c-out-work-cursor-setup"].mode = fs.ModeDir | 0o755 }, "replaced", nil},
		"unlistable-root": {func(m *memFS) { m.nodes[rsProjects].listError = fs.ErrPermission }, "cannot be listed", nil},
	} {
		m := rsWorld()
		p := rsPreparer(m, ApprovalScanLimits{})
		w := session(t, p, "cursor-setup", rsWS1, func() { tc.after(m) })
		if w.State != ResidueUnverified || !strings.Contains(deref(w.Reason), tc.want) || !strings.HasPrefix(deref(w.Reason), ReasonCursorScopeUnverified) || len(p.ledger.entries) != 0 {
			t.Errorf("%s: %+v", name, w)
			continue
		}
		if tc.flags != nil {
			e := w.Entries[0]
			got := WorkerResidueEntry{PrefixVerified: e.PrefixVerified, SuffixVerified: e.SuffixVerified, SocketTypeVerified: e.SocketTypeVerified,
				SoleEntryVerified: e.SoleEntryVerified, IdentityVerified: e.IdentityVerified}
			if got != *tc.flags {
				t.Errorf("%s flags %+v", name, got)
			}
		}
		// An unverified record keeps its failed flags and still validates.
		if err := w.validate(func(string) bool { return true }, MaxResidueEntries); err != nil {
			t.Errorf("%s: %v", name, err)
		}
	}
	// Bounds: the projects listing and a candidate directory count against
	// the entry cap.
	m := rsWorld()
	p := rsPreparer(m, ApprovalScanLimits{Entries: 1})
	socketAt(m, "a", map[string]fs.FileMode{"x": 0o600})
	if _, err := p.residueSnapshot(); err == nil || !strings.Contains(err.Error(), "bound") {
		t.Fatalf("bounded listing: %v", err)
	}
	// A root that is a link or a file fails closed; an absent root has none.
	m = rsWorld()
	m.nodes[rsProjects] = &memNode{mode: fs.ModeSymlink | 0o777}
	if _, err := rsPreparer(m, ApprovalScanLimits{}).residueSnapshot(); err == nil {
		t.Fatal("a linked projects root")
	}
	if got, err := rsPreparer(newMemFS(), ApprovalScanLimits{}).residueSnapshot(); err != nil || len(got) != 0 {
		t.Fatal("an absent projects root")
	}
}

func TestWorkerResidueLedger(t *testing.T) {
	admit := func() (*memFS, *cursorPreparer) {
		m := rsWorld()
		p := rsPreparer(m, ApprovalScanLimits{})
		if w := session(t, p, "cursor-setup", rsWS1, func() { socketAt(m, "c-out-work-cu-1b81996", nil) }); w.State != ResidueVerified {
			t.Fatal(w)
		}
		return m, p
	}
	dir := rsProjects + "/c-out-work-cu-1b81996"
	// Both gone: the entry is dropped; the run goes on.
	m, p := admit()
	delete(m.nodes, dir)
	delete(m.nodes, dir+"/worker.sock")
	if err := p.revalidate(); err != nil || len(p.ledger.entries) != 0 {
		t.Fatalf("both gone: %v", err)
	}
	for name, change := range map[string]func(m *memFS){
		"socket-gone":     func(m *memFS) { delete(m.nodes, dir+"/worker.sock") },
		"dir-gone":        func(m *memFS) { delete(m.nodes, dir) },
		"socket-replaced": func(m *memFS) { m.nodes[dir+"/worker.sock"] = &memNode{mode: fs.ModeSocket | 0o700} },
		"socket-to-file":  func(m *memFS) { m.nodes[dir+"/worker.sock"] = &memNode{mode: 0o600} },
		"dir-replaced":    func(m *memFS) { m.nodes[dir] = &memNode{mode: fs.ModeDir | 0o755} },
		"sibling":         func(m *memFS) { m.nodes[dir+"/x"] = &memNode{mode: 0o600} },
	} {
		m, p := admit()
		change(m)
		if err := p.revalidate(); err == nil {
			t.Errorf("%s: revalidated", name)
		}
		if _, err := p.residueSnapshot(); err == nil {
			t.Errorf("%s: the next baseline accepted it", name)
		}
	}
	// Exemption needs the admitted identity, by exact path.
	_, p = admit()
	if !p.exempt(dir+"/worker.sock", memInfo{name: "worker.sock", mode: fs.ModeSocket | 0o755, mtime: epoch}) ||
		p.exempt(dir+"/worker.sock", memInfo{name: "worker.sock", mode: fs.ModeSocket | 0o700, mtime: epoch}) ||
		p.exempt(dir+"/other.sock", memInfo{name: "other.sock", mode: fs.ModeSocket | 0o755, mtime: epoch}) {
		t.Fatal("exemption")
	}
	// The harness never opens, connects to or deletes: the filesystem view
	// has no write, remove or dial operation.
	typ := reflect.TypeOf((*approvalFS)(nil)).Elem()
	for i := 0; i < typ.NumMethod(); i++ {
		if n := typ.Method(i).Name; n != "Lstat" && n != "ReadDir" && n != "Open" && n != "EvalSymlinks" {
			t.Fatalf("approvalFS gained %s", n)
		}
	}
}

// The parent slug P, its 54-character bound (r0.13 post-final
// clarification DW1) and the independent k >= 2 predicate.
func TestWorkerResidueNames(t *testing.T) {
	m := newMemFS()
	p := rsPreparer(m, ApprovalScanLimits{})
	if parent, base, err := p.parentSlug(rsWS1); err != nil || parent != "c-out-work" || base != "cursor-setup" {
		t.Fatalf("P %q B %q %v", parent, base, err)
	}
	if _, _, err := p.parentSlug("/c/out/work/cursor-setup"); err == nil {
		t.Fatal("a path without the generated .work component")
	}
	if _, _, err := rsPreparer(m, ApprovalScanLimits{}).parentSlug("/c/o.ut/.work/cursor-setup"); err == nil {
		t.Fatal("an unmapped component")
	}
	for _, tc := range []struct {
		name           string
		prefix, suffix bool
	}{
		{"P-cu-1b81996", true, true}, {"P-cursor--e136e0a", true, true}, {"P-cursor-setu-0000000", true, true},
		{"P-c-1b81996", false, true}, {"P-cursor-setup-1b81996", false, true}, {"P--1b81996", false, true},
		{"P-cu-1b8199g", true, false}, {"P-cu_1b81996", false, false}, {"Q-cu-1b81996", false, true}, {"P-du-1b81996", false, true},
	} {
		if pre, suf := residueName(tc.name, "P", "cursor-setup"); pre != tc.prefix || suf != tc.suffix {
			t.Errorf("%s: prefix %v suffix %v", tc.name, pre, suf)
		}
	}
	// At the bound: P of 54 characters leaves two case characters in an
	// observed 57-character prefix (admitted); at 55 the same truncation
	// leaves one, which the predicate refuses on its own.
	for n, want := range map[int]bool{54: true, 55: false} {
		parent := strings.Repeat("p", n-5) + "-work"
		name := (parent + "-cursor-setup")[:57] + "-1b81996"
		if pre, _ := residueName(name, parent, "cursor-setup"); pre != want || len(parent) != n {
			t.Errorf("P length %d: prefix %v", n, pre)
		}
	}
	if MaxResidueParentSlug != 54 || ReasonResidueParentSlug != "cursor_approval_scope_unverified: cursor residue parent slug exceeds 54 characters" {
		t.Fatal(ReasonResidueParentSlug)
	}
}

// The residue record's strict validation (shared by capture and report).
func TestWorkerResidueRecord(t *testing.T) {
	ok := WorkerResidueEntry{CaseID: "a", Path: residuePath(1), Origin: ResidueOrigin, PrefixVerified: true, SuffixVerified: true, SocketTypeVerified: true,
		SoleEntryVerified: true, IdentityVerified: true}
	ran := func(id string) bool { return id == "a" || id == "b" }
	good := WorkerResidue{Policy: WorkerResiduePolicy, State: ResidueVerified, Entries: []WorkerResidueEntry{ok}}
	if err := good.validate(ran, 3); err != nil {
		t.Fatal(err)
	}
	mut := func(f func(w *WorkerResidue)) WorkerResidue {
		w := good
		w.Entries = append([]WorkerResidueEntry(nil), good.Entries...)
		f(&w)
		return w
	}
	for name, w := range map[string]WorkerResidue{
		"policy":       mut(func(w *WorkerResidue) { w.Policy = "x" }),
		"state":        mut(func(w *WorkerResidue) { w.State = "ok" }),
		"reason":       mut(func(w *WorkerResidue) { w.Reason = sptr("x") }),
		"no-reason":    mut(func(w *WorkerResidue) { w.State = ResidueUnverified }),
		"empty-reason": mut(func(w *WorkerResidue) { w.State, w.Reason = ResidueUnverified, sptr("") }),
		"nil-entries":  mut(func(w *WorkerResidue) { w.Entries = nil }),
		"not-checked":  mut(func(w *WorkerResidue) { w.State, w.Reason = ResidueNotChecked, sptr("x") }),
		"dup-path":     mut(func(w *WorkerResidue) { e := ok; e.CaseID = "b"; w.Entries = append(w.Entries, e) }),
		"dup-owner":    mut(func(w *WorkerResidue) { e := ok; e.Path = residuePath(2); w.Entries = append(w.Entries, e) }),
		"raw-path": mut(func(w *WorkerResidue) {
			w.Entries[0].Path = "<home>/.cursor/projects/c-out-work-cu-1b81996/worker.sock"
		}),
		"origin":  mut(func(w *WorkerResidue) { w.Entries[0].Origin = "pre_existing" }),
		"not-run": mut(func(w *WorkerResidue) { w.Entries[0].CaseID = "z" }),
		"flag":    mut(func(w *WorkerResidue) { w.Entries[0].IdentityVerified = false }),
		"too-many": mut(func(w *WorkerResidue) {
			for i, id := range []string{"b", "c", "d"} {
				e := ok
				e.CaseID, e.Path = id, residuePath(i+2)
				w.Entries = append(w.Entries, e)
			}
		}),
	} {
		if err := w.validate(func(id string) bool { return ran(id) || id == "c" || id == "d" }, 3); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	// Capture: only the pinned adapter's trusted recipe, only its own case,
	// strict raw members.
	if err := checkResidueRaw(json.RawMessage(`null`), "r"); err == nil {
		t.Fatal("a null residue")
	}
	b, _ := json.Marshal(good)
	if err := checkResidueRaw(b, "r"); err != nil {
		t.Fatal(err)
	}
	for _, bad := range []string{strings.Replace(string(b), `"identity_verified":true`, `"identity_verified":null`, 1),
		strings.Replace(string(b), `"origin"`, `"x":1,"origin"`, 1), strings.Replace(string(b), `"entries":[`, `"entries":[null,`, 1)} {
		if err := checkResidueRaw(json.RawMessage(bad), "r"); err == nil {
			t.Errorf("raw %s accepted", bad)
		}
	}
	c := &CaptureClient{ID: "cursor", CaseID: "a", ExpectedVersion: CursorRealVersion, Approval: newApproval("x"), Session: CaptureStage{State: StageRan},
		State: CaptureComplete, WorkerResidue: &good}
	if err := c.validateWorkerResidue("linux", "amd64"); err != nil {
		t.Fatal(err)
	}
	for name, f := range map[string]func(c *CaptureClient){
		"darwin":     func(c *CaptureClient) {},
		"no-adapter": func(c *CaptureClient) { c.Approval = nil },
		"other-case": func(c *CaptureClient) { c.CaseID = "b" },
		"unverified": func(c *CaptureClient) { c.WorkerResidue = sptrResidue(notCheckedResidue("x")) },
		"no-session": func(c *CaptureClient) { c.Session.State = StagePrepared; c.State = CapturePartial },
	} {
		cc := *c
		f(&cc)
		goos := "linux"
		if name == "darwin" {
			goos = "darwin"
		}
		if err := cc.validateWorkerResidue(goos, "amd64"); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}
