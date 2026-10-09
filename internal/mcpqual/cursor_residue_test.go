package mcpqual

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"path"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

// Design decoder-enrollment B3, UT-25 (FP-25), as amended by A3: run-
// attributed Cursor session residue on injected filesystem records (no
// socket is created, opened, connected to or removed here): both layouts'
// exact admission predicates (hashed through both observed truncation
// patterns, in_place through the exact bounded tree with its approval
// provenance), each forced independently of the workspace's length; every
// missing, extra, mistyped, linked, oversized and misnamed entry; both
// layouts at once, partial trees, unknown new entries, replacement,
// pre-existing and during-enable artifacts; the conditional parent-slug cap;
// the three-case ledger and its revalidation; an open-spying view that sees
// no session file opened, ever; and the strict v1/v2 evidence.

const (
	rsHome = "/home/o"
	rsWS1  = "/c/out/.work/cursor-setup"
	rsWS2  = "/c/out/.work/cursor-default-1"
	rsWS3  = "/c/out/.work/cursor-default-2"
	rsUUID = "19c97a6a-2baa-4f62-b4b3-60d320442115"
	// rsCanary is in every session file; it must never reach evidence.
	rsCanary   = "canary-session-text-7f3a"
	rsApproval = `["probe-0123456789abcdef"]`
)

var (
	rsProjects   = rsHome + "/.cursor/projects"
	rsLog        = "[info] runServer socketPath=" + rsCanary + "\n"
	rsRepo       = `{"id": "` + rsCanary + `"}`
	rsTrusted    = `{"workspacePath": "` + rsCanary + `", "trustMethod": "cli-flag"}`
	rsTranscript = `{"role":"user","message":"` + rsCanary + `"}` + "\n"
)

// rsWorld is a tiny home with an unrelated owner project and three case
// workspaces.
func rsWorld() *memFS {
	m := newMemFS()
	for _, ws := range []string{rsWS1, rsWS2, rsWS3} {
		m.file(ws+"/.cursor/mcp.json", "{}")
	}
	m.file(rsProjects+"/owner-project/notes.json", "{}")
	return m
}

// rsSlug is a fixture workspace's full project slug S.
func rsSlug(ws string) string { return "c-out-work-" + path.Base(ws) }

// rsDir is ws's full-slug project directory.
func rsDir(ws string) string { return rsProjects + "/" + rsSlug(ws) }

// socketAt adds a hashed residue directory name holding entries (name ->
// mode; nil: worker.sock alone, a socket).
func socketAt(m *memFS, name string, entries map[string]fs.FileMode) {
	m.mkdirs(rsProjects + "/" + name)
	if entries == nil {
		entries = map[string]fs.FileMode{cursorWorkerSocket: fs.ModeSocket | 0o755}
	}
	for e, mode := range entries {
		m.nodes[rsProjects+"/"+name+"/"+e] = &memNode{mode: mode}
	}
}

// rsInPlace leaves the exact in-place session tree in ws's project
// directory (the approval file is enable's).
func rsInPlace(m *memFS, ws string) {
	d := rsDir(ws)
	m.nodes[d+"/"+cursorWorkerSocket] = &memNode{mode: fs.ModeSocket | 0o775}
	m.file(d+"/"+inPlaceLog, rsLog)
	m.file(d+"/"+inPlaceRepo, rsRepo)
	m.file(d+"/"+inPlaceTrusted, rsTrusted)
	m.file(d+"/"+inPlaceTranscripts+"/"+rsUUID+"/"+rsUUID+".jsonl", rsTranscript)
}

// rsDel removes p and everything below it.
func rsDel(m *memFS, p string) {
	for k := range m.nodes {
		if k == p || strings.HasPrefix(k, p+"/") {
			delete(m.nodes, k)
		}
	}
}

// rsSpyFS counts every open by path (and can act on one, or on a listing).
type rsSpyFS struct {
	*memFS
	mu     sync.Mutex
	opens  map[string]int
	onOpen func(p string)
	onList func(p string)
	// afterLstat runs after a lookup returned (the change lands after the
	// observer recorded the old metadata).
	afterLstat func(p string)
}

func (s *rsSpyFS) Lstat(p string) (fs.FileInfo, error) {
	info, err := s.memFS.Lstat(p)
	s.mu.Lock()
	hook := s.afterLstat
	s.mu.Unlock()
	if hook != nil {
		hook(p)
	}
	return info, err
}

func (s *rsSpyFS) ReadDir(p string) ([]fs.DirEntry, error) {
	s.mu.Lock()
	hook := s.onList
	s.mu.Unlock()
	if hook != nil {
		hook(p)
	}
	return s.memFS.ReadDir(p)
}

func rsSpy(m *memFS) *rsSpyFS { return &rsSpyFS{memFS: m, opens: map[string]int{}} }

func (s *rsSpyFS) Open(p string) (approvalFile, error) {
	s.mu.Lock()
	s.opens[p]++
	hook := s.onOpen
	s.mu.Unlock()
	if hook != nil {
		hook(p)
	}
	return s.memFS.Open(p)
}

// sessionOpens counts opens of any session artifact (never allowed).
func (s *rsSpyFS) sessionOpens() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for p, c := range s.opens {
		if slices.Contains(sessionArtifactNames, path.Base(p)) || strings.HasSuffix(p, ".jsonl") || strings.Contains(p, "/"+inPlaceTranscripts+"/") {
			n += c
		}
	}
	return n
}

// approvalOpens counts opens of approval files.
func (s *rsSpyFS) approvalOpens() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for p, c := range s.opens {
		if path.Base(p) == cursorApprovalsFile {
			n += c
		}
	}
	return n
}

func rsPreparer(fsys approvalFS, lim ApprovalScanLimits) *cursorPreparer {
	return newCursorPreparer(fsys, lim, "linux", "amd64", CursorRealVersion, rsHome)
}

// rsEnable is a successful project_scoped enable of ws: its project
// directory created holding exactly the approval file, and the baseline
// approve keeps.
func rsEnable(t testing.TB, m *memFS, p *cursorPreparer, ws string) *cursorApprovalBaseline {
	t.Helper()
	m.file(rsDir(ws)+"/"+cursorApprovalsFile, rsApproval)
	sc, err := p.scanner()
	if err != nil {
		t.Fatal(err)
	}
	info, data, why := sc.projectApprovalFile(rsDir(ws) + "/" + cursorApprovalsFile)
	if why != "" {
		t.Fatal(why)
	}
	b, err := p.approvalBaseline(rsSlug(ws), info, data)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// session checks one case: the pre-launch baseline, then after (the
// vendor's session), then the check.
func session(t testing.TB, p *cursorPreparer, caseID, ws string, approval *cursorApprovalBaseline, after func()) WorkerResidue {
	t.Helper()
	pre, err := p.sessionBaseline(approval)
	if err != nil {
		t.Fatalf("%s baseline: %v", caseID, err)
	}
	if after != nil {
		after()
	}
	return p.checkResidue(caseID, ws, pre)
}

// rsHashedEntry and rsInPlaceEntry are the expected public entries.
func rsHashedEntry(caseID string, n int) WorkerResidueEntry {
	return WorkerResidueEntry{CaseID: caseID, Path: residuePath(n), Origin: ResidueOrigin, Layout: ResidueLayoutHashed, IdentityVerified: true,
		SocketTypeVerified: true, LayoutVerified: true, Items: []ResidueItem{{cursorWorkerSocket, ResidueItemSocket, 0}}}
}

func rsInPlaceEntry(caseID string, n int) WorkerResidueEntry {
	return WorkerResidueEntry{CaseID: caseID, Path: residuePath(n), Origin: ResidueOrigin, Layout: ResidueLayoutInPlace, IdentityVerified: true,
		SocketTypeVerified: true, LayoutVerified: true, ApprovalUnchanged: bptr(true), Items: []ResidueItem{
			{inPlaceTrusted, ResidueItemRegular, int64(len(rsTrusted))},
			{inPlaceTranscripts, ResidueItemDirectory, 0},
			{inPlaceTranscripts + "/" + transcriptLabel, ResidueItemDirectory, 0},
			{inPlaceTranscripts + "/" + transcriptLabel + "/" + transcriptLabel + ".jsonl", ResidueItemRegular, int64(len(rsTranscript))},
			{cursorApprovalsFile, ResidueItemRegular, int64(len(rsApproval))},
			{inPlaceRepo, ResidueItemRegular, int64(len(rsRepo))},
			{inPlaceLog, ResidueItemRegular, int64(len(rsLog))},
			{cursorWorkerSocket, ResidueItemSocket, 0},
		}}
}

// rsPrivate fails when evidence carries a raw name, UUID, suffix or canary.
func rsPrivate(t *testing.T, what string, v any) {
	t.Helper()
	b, _ := json.Marshal(v)
	for _, raw := range []string{"c-out-work", rsUUID, rsCanary, "1b81996", "e136e0a", "0a1b2c3", "/home/o"} {
		if strings.Contains(string(b), raw) {
			t.Fatalf("%s: %q in %s", what, raw, b)
		}
	}
}

func TestWorkerResidueAdmitted(t *testing.T) {
	// hashed: the two observed truncation patterns as equivalent fabricated
	// paths ("...-work-cursor--e136e0a", k=7; "...-work-cu-1b81996", k=2),
	// with workspace-only approval and with an eligible approval directory
	// that stays approval-only.
	for _, name := range []string{"c-out-work-cursor--e136e0a", "c-out-work-cu-1b81996"} {
		for _, scoped := range []bool{false, true} {
			m := rsWorld()
			s := rsSpy(m)
			p := rsPreparer(s, ApprovalScanLimits{})
			var b *cursorApprovalBaseline
			if scoped {
				b = rsEnable(t, m, p, rsWS1)
			}
			w := session(t, p, "cursor-setup", rsWS1, b, func() { socketAt(m, name, nil) })
			if w.State != ResidueVerified || w.Reason != nil || w.Policy != WorkerResiduePolicyV2 || len(w.Entries) != 1 ||
				!reflect.DeepEqual(w.Entries[0], rsHashedEntry("cursor-setup", 1)) {
				t.Fatalf("%s scoped %v: %+v", name, scoped, w)
			}
			if err := w.validate(func(id string) bool { return id == "cursor-setup" }, 1); err != nil {
				t.Fatal(err)
			}
			rsPrivate(t, name, w)
		}
	}
	// in_place: the exact tree in the case's own approval directory, then a
	// hashed second case and an in_place third case of the same run.
	m := rsWorld()
	s := rsSpy(m)
	p := rsPreparer(s, ApprovalScanLimits{})
	b1 := rsEnable(t, m, p, rsWS1)
	w := session(t, p, "cursor-setup", rsWS1, b1, func() { rsInPlace(m, rsWS1) })
	if w.State != ResidueVerified || w.Reason != nil || len(w.Entries) != 1 || !reflect.DeepEqual(w.Entries[0], rsInPlaceEntry("cursor-setup", 1)) {
		t.Fatalf("in_place: %+v", w)
	}
	rsPrivate(t, "in_place", w)
	b2 := rsEnable(t, m, p, rsWS2)
	w2 := session(t, p, "cursor-default-1", rsWS2, b2, func() { socketAt(m, "c-out-work-cursor-defa-0a1b2c3", nil) })
	b3 := rsEnable(t, m, p, rsWS3)
	w3 := session(t, p, "cursor-default-2", rsWS3, b3, func() { rsInPlace(m, rsWS3) })
	want := []WorkerResidueEntry{rsInPlaceEntry("cursor-setup", 1), rsHashedEntry("cursor-default-1", 2), rsInPlaceEntry("cursor-default-2", 3)}
	if w2.State != ResidueVerified || !reflect.DeepEqual(w2.Entries, want[:2]) || w3.State != ResidueVerified || !reflect.DeepEqual(w3.Entries, want) {
		t.Fatalf("three cases: %+v %+v", w2, w3)
	}
	if err := w3.validate(func(string) bool { return true }, MaxResidueEntries); err != nil {
		t.Fatal(err)
	}
	rsPrivate(t, "ledger", w3)
	// A later approval inventory of this run passes over the ledger: the
	// sockets exempt, the session files metadata-only; the approvals are
	// read (and their bytes compared) as before.
	roots, _ := approvalRoots(s, rsWS1, rsHome)
	sc, _ := p.scanner()
	before := s.approvalOpens()
	if got := p.scan(sc, roots, ""); !got.complete {
		t.Fatalf("ledger scan: %s", got.why)
	}
	if s.sessionOpens() != 0 || s.approvalOpens() <= before {
		t.Fatalf("opens %v", s.opens)
	}
	// A fresh invocation (empty ledger, A3.1) refuses its own case paths
	// before any content scan; elsewhere it baselines the unchanged
	// leftovers as foreign and opens none of them.
	fresh := rsPreparer(s, ApprovalScanLimits{})
	sc, _ = fresh.scanner()
	if got := fresh.scan(sc, roots, rsSlug(rsWS1)); got.complete || got.why != ReasonCaseProjectExists || !isFixed(got.typed) {
		t.Fatalf("a new invocation reused a case path: %q", got.why)
	}
	if got := fresh.scan(sc, roots, ""); !got.complete {
		t.Fatalf("unchanged leftovers: %q", got.why)
	}
	if w := session(t, fresh, "cursor-setup", rsWS1, nil, nil); w.State != ResidueVerified || len(w.Entries) != 0 {
		t.Fatalf("unchanged leftovers in a session: %+v", w)
	}
	if s.sessionOpens() != 0 {
		t.Fatalf("a session file was opened: %v", s.opens)
	}
	// Nothing was removed.
	for _, p := range []string{rsDir(rsWS1) + "/worker.log", rsDir(rsWS3) + "/" + cursorWorkerSocket, rsProjects + "/c-out-work-cursor-defa-0a1b2c3/worker.sock"} {
		if _, ok := m.nodes[p]; !ok {
			t.Fatalf("%s disappeared", p)
		}
	}
	// No residue: verified with no entries (the check ran), with and
	// without an approval directory, which stays approval-only.
	for _, scoped := range []bool{false, true} {
		m := rsWorld()
		p := rsPreparer(m, ApprovalScanLimits{})
		var b *cursorApprovalBaseline
		if scoped {
			b = rsEnable(t, m, p, rsWS1)
		}
		if w := session(t, p, "cursor-setup", rsWS1, b, nil); w.State != ResidueVerified || len(w.Entries) != 0 || w.Entries == nil {
			t.Fatalf("no residue (scoped %v): %+v", scoped, w)
		}
	}
	// At each per-file limit (the four session limits sum to the session
	// total): admitted with the declared sizes.
	m = rsWorld()
	p = rsPreparer(m, ApprovalScanLimits{})
	b1 = rsEnable(t, m, p, rsWS1)
	d := rsDir(rsWS1)
	w = session(t, p, "cursor-setup", rsWS1, b1, func() {
		rsInPlace(m, rsWS1)
		m.nodes[d+"/"+inPlaceLog].size = maxInPlaceLog
		m.nodes[d+"/"+inPlaceRepo].size = maxInPlaceSmall
		m.nodes[d+"/"+inPlaceTrusted].size = maxInPlaceSmall
		m.nodes[d+"/"+inPlaceTranscripts+"/"+rsUUID+"/"+rsUUID+".jsonl"].size = maxInPlaceTranscript
	})
	if w.State != ResidueVerified || len(w.Entries) != 1 || w.Entries[0].Items[6].Size != maxInPlaceLog || w.Entries[0].Items[0].Size != maxInPlaceSmall {
		t.Fatalf("at the limits: %+v", w)
	}
	if maxInPlaceLog+2*maxInPlaceSmall+maxInPlaceTranscript != maxInPlaceSession {
		t.Fatal("the session total")
	}
	if err := w.validate(func(string) bool { return true }, 1); err != nil {
		t.Fatal(err)
	}
}

func TestWorkerResidueRefused(t *testing.T) {
	sock := fs.ModeSocket | 0o755
	d := rsDir(rsWS1)
	tdir := d + "/" + inPlaceTranscripts + "/" + rsUUID
	tfile := tdir + "/" + rsUUID + ".jsonl"
	// inPlace applies change after the exact in-place tree.
	inPlace := func(change func(m *memFS)) func(m *memFS) {
		return func(m *memFS) { rsInPlace(m, rsWS1); change(m) }
	}
	set := func(p string, n *memNode) func(m *memFS) { return func(m *memFS) { m.nodes[p] = n } }
	del := func(p string) func(m *memFS) { return func(m *memFS) { rsDel(m, p) } }
	link := &memNode{mode: fs.ModeSymlink | 0o777}
	const layout = "not the exact in-place layout"
	type vector struct {
		scoped bool
		after  func(m *memFS)
		want   string
		lim    ApprovalScanLimits
	}
	vectors := map[string]vector{
		// hashed names: the cap holds (P is 10 characters), the name fails.
		"suffix-short":  {false, func(m *memFS) { socketAt(m, "c-out-work-cu-1b8199", nil) }, "attributed hashed name", ApprovalScanLimits{}},
		"suffix-upper":  {false, func(m *memFS) { socketAt(m, "c-out-work-cu-1B81996", nil) }, "attributed hashed name", ApprovalScanLimits{}},
		"prefix-k1":     {false, func(m *memFS) { socketAt(m, "c-out-work-c-1b81996", nil) }, "attributed hashed name", ApprovalScanLimits{}},
		"prefix-parent": {false, func(m *memFS) { socketAt(m, "c-out-work-1b81996", nil) }, "attributed hashed name", ApprovalScanLimits{}},
		"prefix-cut":    {false, func(m *memFS) { socketAt(m, "c-out-wo-1b81996", nil) }, "attributed hashed name", ApprovalScanLimits{}},
		"full-slug":     {false, func(m *memFS) { socketAt(m, "c-out-work-cursor-setup-1b81996", nil) }, "attributed hashed name", ApprovalScanLimits{}},
		"other-case":    {false, func(m *memFS) { socketAt(m, "c-out-work-cursor-defa-1b81996", nil) }, "attributed hashed name", ApprovalScanLimits{}},
		"other-parent":  {false, func(m *memFS) { socketAt(m, "c-other-work-cu-1b81996", nil) }, "attributed hashed name", ApprovalScanLimits{}},
		// hashed shapes.
		"hashed-regular": {false, func(m *memFS) {
			socketAt(m, "c-out-work-cu-1b81996", map[string]fs.FileMode{cursorWorkerSocket: 0o600})
		}, "not a hashed", ApprovalScanLimits{}},
		"hashed-fifo": {false, func(m *memFS) {
			socketAt(m, "c-out-work-cu-1b81996", map[string]fs.FileMode{cursorWorkerSocket: fs.ModeNamedPipe | 0o600})
		}, "not a hashed", ApprovalScanLimits{}},
		"hashed-link": {false, func(m *memFS) {
			socketAt(m, "c-out-work-cu-1b81996", map[string]fs.FileMode{cursorWorkerSocket: fs.ModeSymlink | 0o777})
		}, "not a hashed", ApprovalScanLimits{}},
		"hashed-device": {false, func(m *memFS) {
			socketAt(m, "c-out-work-cu-1b81996", map[string]fs.FileMode{cursorWorkerSocket: fs.ModeDevice | 0o600})
		}, "not a hashed", ApprovalScanLimits{}},
		"hashed-other-name": {false, func(m *memFS) { socketAt(m, "c-out-work-cu-1b81996", map[string]fs.FileMode{"agent.sock": sock}) }, ReasonForeignNew, ApprovalScanLimits{}},
		"hashed-sibling": {false, func(m *memFS) {
			socketAt(m, "c-out-work-cu-1b81996", map[string]fs.FileMode{cursorWorkerSocket: sock, "x.json": 0o600})
		}, ReasonForeignNew, ApprovalScanLimits{}},
		"hashed-session-files": {false, func(m *memFS) {
			socketAt(m, "c-out-work-cu-1b81996", map[string]fs.FileMode{cursorWorkerSocket: sock, inPlaceLog: 0o600})
		}, ReasonForeignNew, ApprovalScanLimits{}},
		"two-candidates": {false, func(m *memFS) { socketAt(m, "c-out-work-cu-1b81996", nil); socketAt(m, "c-out-work-cur-2b81996", nil) }, "more than one", ApprovalScanLimits{}},
		"dir-link":       {false, set(rsProjects+"/c-out-work-cu-1b81996", link), "not a residue directory", ApprovalScanLimits{}},
		// Unknown new project entries (A3 tightens: even ordinary ones fail).
		"unknown-dir":   {false, func(m *memFS) { m.file(rsProjects+"/other-project/notes.json", "{}") }, "not a hashed", ApprovalScanLimits{}},
		"unknown-empty": {true, func(m *memFS) { m.mkdirs(rsProjects + "/x") }, "not a hashed", ApprovalScanLimits{}},
		"unknown-file":  {true, func(m *memFS) { m.file(rsProjects+"/notes.txt", "x") }, "not a residue directory", ApprovalScanLimits{}},
		"nested": {false, func(m *memFS) {
			m.mkdirs(rsProjects + "/x/y")
			m.nodes[rsProjects+"/x/y/worker.sock"] = &memNode{mode: sock}
		}, ReasonForeignNew, ApprovalScanLimits{}},
		// Elsewhere, replacement, listing.
		"elsewhere":    {false, set(rsWS1+"/.cursor/worker.sock", &memNode{mode: sock}), "special file", ApprovalScanLimits{}},
		"replaced-old": {false, func(m *memFS) { m.nodes[rsProjects+"/owner-project"].mtime = epoch.Add(time.Second) }, "replaced", ApprovalScanLimits{}},
		// (Code review A3 round 2, C1: the foreign verdict, not the snapshot's.)
		"unlistable-root": {false, func(m *memFS) { m.nodes[rsProjects].listError = fs.ErrPermission }, ReasonForeignUnverifiable, ApprovalScanLimits{}},
		"owner-dir-session-files": {true, func(m *memFS) {
			m.file(rsProjects+"/owner-project/"+inPlaceLog, rsLog)
			m.file(rsProjects+"/owner-project/"+inPlaceTranscripts+"/"+rsUUID+"/"+rsUUID+".jsonl", rsTranscript)
		}, ReasonForeignNew, ApprovalScanLimits{}},
		// in_place: each missing required entry.
		"missing-socket":      {true, inPlace(del(d + "/" + cursorWorkerSocket)), layout, ApprovalScanLimits{}},
		"missing-log":         {true, inPlace(del(d + "/" + inPlaceLog)), layout, ApprovalScanLimits{}},
		"missing-repo":        {true, inPlace(del(d + "/" + inPlaceRepo)), layout, ApprovalScanLimits{}},
		"missing-trusted":     {true, inPlace(del(d + "/" + inPlaceTrusted)), layout, ApprovalScanLimits{}},
		"missing-transcript":  {true, inPlace(del(tfile)), layout, ApprovalScanLimits{}},
		"missing-uuid-dir":    {true, inPlace(del(tdir)), layout, ApprovalScanLimits{}},
		"missing-transcripts": {true, inPlace(del(d + "/" + inPlaceTranscripts)), layout, ApprovalScanLimits{}},
		"missing-approval":    {true, inPlace(del(d + "/" + cursorApprovalsFile)), layout, ApprovalScanLimits{}},
		// Partial trees, with and without the socket.
		"partial-socket": {true, set(d+"/"+cursorWorkerSocket, &memNode{mode: sock}), layout, ApprovalScanLimits{}},
		"partial-socket-log": {true, func(m *memFS) {
			m.nodes[d+"/"+cursorWorkerSocket] = &memNode{mode: sock}
			m.file(d+"/"+inPlaceLog, rsLog)
		}, layout, ApprovalScanLimits{}},
		"partial-no-socket": {true, inPlace(del(d + "/" + cursorWorkerSocket)), layout, ApprovalScanLimits{}},
		"partial-log":       {true, func(m *memFS) { m.file(d+"/"+inPlaceLog, rsLog) }, layout, ApprovalScanLimits{}},
		// Every extra entry at each level, hidden and nested included.
		"extra-top":         {true, inPlace(func(m *memFS) { m.file(d+"/state.json", "{}") }), layout, ApprovalScanLimits{}},
		"extra-hidden":      {true, inPlace(func(m *memFS) { m.file(d+"/.hidden", "") }), layout, ApprovalScanLimits{}},
		"extra-dir":         {true, inPlace(func(m *memFS) { m.mkdirs(d + "/sub") }), layout, ApprovalScanLimits{}},
		"extra-transcripts": {true, inPlace(func(m *memFS) { m.file(d+"/"+inPlaceTranscripts+"/index.json", "{}") }), layout, ApprovalScanLimits{}},
		"extra-uuid-dir":    {true, inPlace(func(m *memFS) { m.file(tdir+"/"+rsUUID+".meta", "") }), layout, ApprovalScanLimits{}},
		"extra-uuid": {true, inPlace(func(m *memFS) {
			m.file(d+"/"+inPlaceTranscripts+"/0c97a6a0-2baa-4f62-b4b3-60d320442115/0c97a6a0-2baa-4f62-b4b3-60d320442115.jsonl", "")
		}), layout, ApprovalScanLimits{}},
		// Wrong types and links on every component.
		"socket-regular":   {true, inPlace(set(d+"/"+cursorWorkerSocket, &memNode{mode: 0o600})), layout, ApprovalScanLimits{}},
		"socket-fifo":      {true, inPlace(set(d+"/"+cursorWorkerSocket, &memNode{mode: fs.ModeNamedPipe | 0o600})), layout, ApprovalScanLimits{}},
		"socket-device":    {true, inPlace(set(d+"/"+cursorWorkerSocket, &memNode{mode: fs.ModeDevice | 0o600})), layout, ApprovalScanLimits{}},
		"socket-sized":     {true, inPlace(set(d+"/"+cursorWorkerSocket, &memNode{mode: sock, size: 1})), layout, ApprovalScanLimits{}},
		"log-dir":          {true, inPlace(set(d+"/"+inPlaceLog, &memNode{mode: fs.ModeDir | 0o700})), layout, ApprovalScanLimits{}},
		"repo-socket":      {true, inPlace(set(d+"/"+inPlaceRepo, &memNode{mode: sock})), layout, ApprovalScanLimits{}},
		"trusted-fifo":     {true, inPlace(set(d+"/"+inPlaceTrusted, &memNode{mode: fs.ModeNamedPipe | 0o600})), layout, ApprovalScanLimits{}},
		"transcript-dir":   {true, inPlace(set(tfile, &memNode{mode: fs.ModeDir | 0o700})), layout, ApprovalScanLimits{}},
		"uuid-dir-file":    {true, inPlace(func(m *memFS) { rsDel(m, tdir); m.file(tdir, "") }), layout, ApprovalScanLimits{}},
		"transcripts-file": {true, inPlace(func(m *memFS) { rsDel(m, d+"/"+inPlaceTranscripts); m.file(d+"/"+inPlaceTranscripts, "") }), layout, ApprovalScanLimits{}},
		"link-socket":      {true, inPlace(set(d+"/"+cursorWorkerSocket, link)), layout, ApprovalScanLimits{}},
		"link-log":         {true, inPlace(set(d+"/"+inPlaceLog, link)), layout, ApprovalScanLimits{}},
		"link-repo":        {true, inPlace(set(d+"/"+inPlaceRepo, link)), layout, ApprovalScanLimits{}},
		"link-trusted":     {true, inPlace(set(d+"/"+inPlaceTrusted, link)), layout, ApprovalScanLimits{}},
		"link-transcript":  {true, inPlace(set(tfile, link)), layout, ApprovalScanLimits{}},
		"link-uuid-dir":    {true, inPlace(func(m *memFS) { rsDel(m, tdir); m.nodes[tdir] = link }), layout, ApprovalScanLimits{}},
		"link-transcripts": {true, inPlace(func(m *memFS) { rsDel(m, d+"/"+inPlaceTranscripts); m.nodes[d+"/"+inPlaceTranscripts] = link }), layout, ApprovalScanLimits{}},
		"link-approval":    {true, inPlace(set(d+"/"+cursorApprovalsFile, link)), layout, ApprovalScanLimits{}},
		"link-project-dir": {true, func(m *memFS) { rsDel(m, d); m.nodes[d] = link }, "replaced", ApprovalScanLimits{}},
		"uuid-upper": {true, inPlace(func(m *memFS) {
			up := strings.ToUpper(rsUUID)
			rsDel(m, tdir)
			m.file(d+"/"+inPlaceTranscripts+"/"+up+"/"+up+".jsonl", "")
		}), layout, ApprovalScanLimits{}},
		"uuid-mismatch":         {true, inPlace(func(m *memFS) { rsDel(m, tfile); m.file(tdir+"/0c97a6a0-2baa-4f62-b4b3-60d320442115.jsonl", "") }), layout, ApprovalScanLimits{}},
		"uuid-not":              {true, inPlace(func(m *memFS) { rsDel(m, tdir); m.file(d+"/"+inPlaceTranscripts+"/session/session.jsonl", "") }), layout, ApprovalScanLimits{}},
		"uuid-no-ext":           {true, inPlace(func(m *memFS) { rsDel(m, tfile); m.file(tdir+"/"+rsUUID, "") }), layout, ApprovalScanLimits{}},
		"over-log":              {true, inPlace(func(m *memFS) { m.nodes[d+"/"+inPlaceLog].size = maxInPlaceLog + 1 }), layout, ApprovalScanLimits{}},
		"over-repo":             {true, inPlace(func(m *memFS) { m.nodes[d+"/"+inPlaceRepo].size = maxInPlaceSmall + 1 }), layout, ApprovalScanLimits{}},
		"over-trusted":          {true, inPlace(func(m *memFS) { m.nodes[d+"/"+inPlaceTrusted].size = maxInPlaceSmall + 1 }), layout, ApprovalScanLimits{}},
		"over-transcript":       {true, inPlace(func(m *memFS) { m.nodes[tfile].size = maxInPlaceTranscript + 1 }), layout, ApprovalScanLimits{}},
		"over-negative":         {true, inPlace(func(m *memFS) { m.nodes[d+"/"+inPlaceRepo].size = -1 }), layout, ApprovalScanLimits{}},
		"over-injected-file":    {true, inPlace(func(m *memFS) { m.nodes[d+"/"+inPlaceLog].size = 101 }), layout, ApprovalScanLimits{FileBytes: 100}},
		"over-injected-bytes":   {true, inPlace(func(*memFS) {}), "total size limit", ApprovalScanLimits{Bytes: 60}},
		"over-injected-entries": {true, inPlace(func(*memFS) {}), "bound", ApprovalScanLimits{Entries: 7}},
		"unlistable-uuid-dir":   {true, inPlace(func(m *memFS) { m.nodes[tdir].listError = fs.ErrPermission }), "cannot be listed", ApprovalScanLimits{}},
		"unlookable-log":        {true, inPlace(func(m *memFS) { m.nodes[d+"/"+inPlaceLog].lstatErr = fs.ErrPermission }), "cannot be looked up", ApprovalScanLimits{}},
		// Both layouts at once, even when each matches on its own.
		"both-layouts": {true, inPlace(func(m *memFS) { socketAt(m, "c-out-work-cu-1b81996", nil) }), "both residue layouts", ApprovalScanLimits{}},
		"hashed-and-partial": {true, func(m *memFS) {
			socketAt(m, "c-out-work-cu-1b81996", nil)
			m.file(d+"/"+inPlaceLog, rsLog)
		}, layout, ApprovalScanLimits{}},
		// The approval and its directory.
		"approval-same-size":    {true, inPlace(func(m *memFS) { m.nodes[d+"/"+cursorApprovalsFile].data = `["probe-fedcba9876543210"]` }), "approval file changed", ApprovalScanLimits{}},
		"approval-same-size-no": {true, func(m *memFS) { m.nodes[d+"/"+cursorApprovalsFile].data = `["probe-fedcba9876543210"]` }, "approval file changed", ApprovalScanLimits{}},
		"approval-replaced":     {true, inPlace(func(m *memFS) { m.nodes[d+"/"+cursorApprovalsFile].mtime = epoch.Add(time.Second) }), "replaced or changed", ApprovalScanLimits{}},
		"approval-removed":      {true, del(d + "/" + cursorApprovalsFile), layout, ApprovalScanLimits{}},
		"project-dir-removed":   {true, del(d), "approval directory changed", ApprovalScanLimits{}},
		"project-dir-replaced":  {true, inPlace(func(m *memFS) { m.nodes[d].mtime = epoch.Add(time.Second) }), "replaced", ApprovalScanLimits{}},
		// No eligible approval (workspace-only): the session creating the
		// whole tree is an unknown new entry, never in_place; its session
		// artifacts are new foreign ones (A3.1 takes precedence).
		"workspace-only-tree": {false, func(m *memFS) { m.file(d+"/"+cursorApprovalsFile, rsApproval); rsInPlace(m, rsWS1) }, ReasonForeignNew, ApprovalScanLimits{}},
		// Another case's tree.
		"wrong-case": {true, func(m *memFS) { m.file(rsDir(rsWS2)+"/"+cursorApprovalsFile, rsApproval); rsInPlace(m, rsWS2) }, ReasonForeignNew, ApprovalScanLimits{}},
	}
	for name, tc := range vectors {
		m := rsWorld()
		s := rsSpy(m)
		p := rsPreparer(s, tc.lim)
		var b *cursorApprovalBaseline
		if tc.scoped {
			b = rsEnable(t, m, p, rsWS1)
		}
		w := session(t, p, "cursor-setup", rsWS1, b, func() { tc.after(m) })
		if w.State != ResidueUnverified || !strings.Contains(deref(w.Reason), tc.want) ||
			!strings.HasPrefix(deref(w.Reason), ReasonCursorScopeUnverified+": worker residue ") && deref(w.Reason) != tc.want ||
			len(p.ledger.entries) != 0 || len(w.Entries) != 0 || w.Entries == nil || w.Policy != WorkerResiduePolicyV2 {
			t.Errorf("%s: %+v %s", name, w, deref(w.Reason))
			continue
		}
		if n := s.sessionOpens(); n != 0 {
			t.Errorf("%s: %d session-file opens %v", name, n, s.opens)
		}
		if err := w.validate(func(string) bool { return true }, MaxResidueEntries); err != nil {
			t.Errorf("%s: %v", name, err)
		}
		rsPrivate(t, name, w)
	}
	// Bounds: the projects listing counts against the entry cap.
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

// Pre-existing, during-enable and pre-launch (A3 as amended by A3.1): the
// metadata-only projects preflight tolerates unchanged pre-existing session
// artifacts and sockets as foreign (never opening them), refuses an unsafe
// or unreadable initial view and any artifact appearing after the baseline
// with the exact fixed reasons, and the pre-launch recheck holds the
// approval directory to exactly its unchanged file.
func TestWorkerResiduePreflight(t *testing.T) {
	d := rsDir(rsWS1)
	scanOf := func(m *memFS) (*rsSpyFS, *cursorPreparer, func(own string) *snapshot) {
		s := rsSpy(m)
		p := rsPreparer(s, ApprovalScanLimits{})
		roots, _ := approvalRoots(s, rsWS2, rsHome)
		return s, p, func(own string) *snapshot {
			sc, _ := p.scanner()
			return p.scan(sc, roots, own)
		}
	}
	for name, setup := range map[string]func(m *memFS){
		"leftover-in-place": func(m *memFS) { m.file(d+"/"+cursorApprovalsFile, rsApproval); rsInPlace(m, rsWS1) },
		"leftover-hashed":   func(m *memFS) { socketAt(m, "c-out-work-cu-1b81996", nil) },
		"leftover-socket": func(m *memFS) {
			m.nodes[rsProjects+"/owner-project/agent.sock"] = &memNode{mode: fs.ModeSocket | 0o755}
		},
		"leftover-partial": func(m *memFS) { m.file(d+"/"+cursorApprovalsFile, rsApproval); m.file(d+"/"+inPlaceLog, rsLog) },
		"leftover-nested":  func(m *memFS) { m.file(rsProjects+"/owner-project/a/b/"+inPlaceRepo, rsRepo) },
		"leftover-at-root": func(m *memFS) { m.file(rsProjects+"/"+inPlaceTrusted, rsTrusted) },
		"empty-reserved":   func(m *memFS) { m.mkdirs(rsProjects + "/owner-project/" + inPlaceTranscripts) },
		"arbitrary-names": func(m *memFS) {
			m.file(rsProjects+"/owner-project/"+inPlaceTranscripts+"/x/notes.txt", rsCanary)
			m.file(rsProjects+"/owner-project/"+inPlaceTranscripts+"/x/y/z.jsonl", rsCanary)
		},
	} {
		m := rsWorld()
		setup(m)
		s, _, scan := scanOf(m)
		for i := 0; i < 2; i++ {
			if got := scan(rsSlug(rsWS2)); !got.complete {
				t.Errorf("%s scan %d: %q", name, i, got.why)
			}
		}
		if s.sessionOpens() != 0 {
			t.Errorf("%s: opens %v", name, s.opens)
		}
	}
	// An unsafe or unreadable initial view: no baseline, nothing opened.
	for name, tc := range map[string]struct {
		setup func(m *memFS)
		want  string
	}{
		"fifo": {func(m *memFS) { m.nodes[rsProjects+"/owner-project/q"] = &memNode{mode: fs.ModeNamedPipe | 0o600} }, ReasonForeignUnverifiable},
		"reserved-link": {func(m *memFS) {
			m.nodes[rsProjects+"/owner-project/"+inPlaceLog] = &memNode{mode: fs.ModeSymlink | 0o777}
		}, ReasonForeignUnverifiable},
		"reserved-device": {func(m *memFS) { m.nodes[rsProjects+"/owner-project/repo.json"] = &memNode{mode: fs.ModeDevice | 0o600} }, ReasonForeignUnverifiable},
		"unlistable":      {func(m *memFS) { m.nodes[rsProjects+"/owner-project"].listError = fs.ErrPermission }, ReasonForeignUnverifiable},
		"unlookable":      {func(m *memFS) { m.nodes[rsProjects+"/owner-project/notes.json"].lstatErr = fs.ErrPermission }, ReasonForeignUnverifiable},
		"root-is-file":    {func(m *memFS) { rsDel(m, rsProjects); m.file(rsProjects, "") }, ReasonForeignUnverifiable},
		"negative-size":   {func(m *memFS) { m.file(rsProjects+"/owner-project/"+inPlaceLog, "").size = -1 }, ReasonForeignUnverifiable},
	} {
		m := rsWorld()
		tc.setup(m)
		s, p, scan := scanOf(m)
		got := scan("")
		if got.complete || got.why != tc.want || len(got.entries) != 0 || len(s.opens) != 0 || len(p.foreign.nodes) != 0 {
			t.Errorf("%s: %q %v", name, got.why, s.opens)
		}
		// A failed initialization is never retried: the preparer stays refused.
		if got := scan(""); got.complete || got.why != tc.want {
			t.Errorf("%s: retried", name)
		}
	}
	// After the baseline: new artifacts (during an enable, a session or at
	// the root) are refused, never exempted, and never opened.
	for name, change := range map[string]func(m *memFS){
		"enable-socket": func(m *memFS) {
			m.file(d+"/"+cursorApprovalsFile, rsApproval)
			m.nodes[d+"/"+cursorWorkerSocket] = &memNode{mode: fs.ModeSocket | 0o755}
		},
		"enable-transcript": func(m *memFS) {
			m.file(d+"/"+cursorApprovalsFile, rsApproval)
			m.file(d+"/"+inPlaceTranscripts+"/"+rsUUID+"/"+rsUUID+".jsonl", rsTranscript)
		},
		"at-root": func(m *memFS) { m.file(rsProjects+"/"+inPlaceTrusted, rsTrusted) },
	} {
		m := rsWorld()
		s, _, scan := scanOf(m)
		if got := scan(""); !got.complete {
			t.Fatal(got.why)
		}
		change(m)
		if got := scan(""); got.complete || got.why != ReasonForeignNew || len(got.entries) != 0 || s.sessionOpens() != 0 {
			t.Errorf("%s: %q %v", name, got.why, s.opens)
		}
	}
	// The preflight's entry cap; an absent projects root has nothing.
	m := rsWorld()
	if err := rsPreparer(m, ApprovalScanLimits{Entries: 1}).preflight(nil, ""); err == nil || err.Error() != ReasonForeignLimit {
		t.Fatalf("preflight bound: %v", err)
	}
	if err := rsPreparer(newMemFS(), ApprovalScanLimits{}).preflight(nil, ""); err != nil {
		t.Fatal(err)
	}
	// Ordinary owner trees, a link outside any reserved path (the scanner's
	// own policy) and approval-only directories pass.
	m = rsWorld()
	m.file(d+"/"+cursorApprovalsFile, rsApproval)
	m.nodes[rsProjects+"/owner-project/link"] = &memNode{mode: fs.ModeSymlink | 0o777}
	if err := rsPreparer(m, ApprovalScanLimits{}).preflight(nil, ""); err != nil {
		t.Fatal(err)
	}
	// The pre-launch recheck after enable.
	for name, change := range map[string]func(m *memFS){
		"session-file":    func(m *memFS) { m.file(d+"/"+inPlaceLog, rsLog) },
		"socket":          func(m *memFS) { m.nodes[d+"/"+cursorWorkerSocket] = &memNode{mode: fs.ModeSocket | 0o755} },
		"same-size":       func(m *memFS) { m.nodes[d+"/"+cursorApprovalsFile].data = `["probe-fedcba9876543210"]` },
		"approval-mtime":  func(m *memFS) { m.nodes[d+"/"+cursorApprovalsFile].mtime = epoch.Add(time.Second) },
		"approval-gone":   func(m *memFS) { rsDel(m, d+"/"+cursorApprovalsFile) },
		"dir-replaced":    func(m *memFS) { m.nodes[d].mtime = epoch.Add(time.Second) },
		"dir-gone":        func(m *memFS) { rsDel(m, d) },
		"dir-unlistable":  func(m *memFS) { m.nodes[d].listError = fs.ErrPermission },
		"approval-unread": func(m *memFS) { m.nodes[d+"/"+cursorApprovalsFile].openErr = fs.ErrPermission },
	} {
		m := rsWorld()
		p := rsPreparer(m, ApprovalScanLimits{})
		b := rsEnable(t, m, p, rsWS1)
		change(m)
		if _, err := p.sessionBaseline(b); err == nil {
			t.Errorf("pre-launch %s: accepted", name)
		}
	}
	// approve's baseline needs exactly the approval file in an ordinary
	// directory.
	m = rsWorld()
	p := rsPreparer(m, ApprovalScanLimits{})
	m.file(d+"/"+cursorApprovalsFile, rsApproval)
	m.file(d+"/other.json", "{}")
	sc, _ := p.scanner()
	info, data, _ := sc.projectApprovalFile(d + "/" + cursorApprovalsFile)
	if _, err := p.approvalBaseline(rsSlug(rsWS1), info, data); err == nil {
		t.Fatal("a baseline with another file")
	}
	m.nodes[d] = &memNode{mode: 0o600}
	if _, err := p.approvalBaseline(rsSlug(rsWS1), info, data); err == nil {
		t.Fatal("a baseline of a file")
	}
}

func TestWorkerResidueLedger(t *testing.T) {
	admitHashed := func() (*memFS, *cursorPreparer) {
		m := rsWorld()
		p := rsPreparer(m, ApprovalScanLimits{})
		if w := session(t, p, "cursor-setup", rsWS1, nil, func() { socketAt(m, "c-out-work-cu-1b81996", nil) }); w.State != ResidueVerified {
			t.Fatal(w)
		}
		return m, p
	}
	admitInPlace := func() (*memFS, *rsSpyFS, *cursorPreparer) {
		m := rsWorld()
		s := rsSpy(m)
		p := rsPreparer(s, ApprovalScanLimits{})
		b := rsEnable(t, m, p, rsWS1)
		if w := session(t, p, "cursor-setup", rsWS1, b, func() { rsInPlace(m, rsWS1) }); w.State != ResidueVerified {
			t.Fatal(w)
		}
		return m, s, p
	}
	dir := rsProjects + "/c-out-work-cu-1b81996"
	// Whole-directory disappearance drops the entry; the run goes on.
	m, p := admitHashed()
	rsDel(m, dir)
	if err := p.revalidate(); err != nil || len(p.ledger.entries) != 0 {
		t.Fatalf("hashed gone: %v", err)
	}
	m, _, p = admitInPlace()
	rsDel(m, rsDir(rsWS1))
	if err := p.revalidate(); err != nil || len(p.ledger.entries) != 0 {
		t.Fatalf("in_place gone: %v", err)
	}
	// Its number is never reused.
	if w := session(t, p, "cursor-default-1", rsWS2, nil, func() { socketAt(m, "c-out-work-cursor-defa-0a1b2c3", nil) }); w.State != ResidueVerified ||
		len(w.Entries) != 1 || w.Entries[0].Path != residuePath(2) {
		t.Fatalf("after a drop: %+v", w)
	}
	for name, change := range map[string]func(m *memFS){
		"socket-gone":     func(m *memFS) { delete(m.nodes, dir+"/worker.sock") },
		"dir-gone":        func(m *memFS) { delete(m.nodes, dir) },
		"socket-replaced": func(m *memFS) { m.nodes[dir+"/worker.sock"] = &memNode{mode: fs.ModeSocket | 0o700} },
		"socket-to-file":  func(m *memFS) { m.nodes[dir+"/worker.sock"] = &memNode{mode: 0o600} },
		"dir-replaced":    func(m *memFS) { m.nodes[dir] = &memNode{mode: fs.ModeDir | 0o755} },
		"sibling":         func(m *memFS) { m.nodes[dir+"/x"] = &memNode{mode: 0o600} },
		"unlistable":      func(m *memFS) { m.nodes[dir].listError = fs.ErrPermission },
	} {
		m, p := admitHashed()
		change(m)
		if err := p.revalidate(); err == nil {
			t.Errorf("hashed %s: revalidated", name)
		}
		if _, err := p.residueSnapshot(); err == nil {
			t.Errorf("hashed %s: the next baseline accepted it", name)
		}
	}
	d := rsDir(rsWS1)
	tdir := d + "/" + inPlaceTranscripts + "/" + rsUUID
	tfile := tdir + "/" + rsUUID + ".jsonl"
	changes := map[string]func(m *memFS){
		"log-grew":          func(m *memFS) { m.nodes[d+"/"+inPlaceLog].data += "more" },
		"transcript-mtime":  func(m *memFS) { m.nodes[tfile].mtime = epoch.Add(time.Second) },
		"repo-mode":         func(m *memFS) { m.nodes[d+"/"+inPlaceRepo].mode = 0o644 },
		"trusted-size":      func(m *memFS) { m.nodes[d+"/"+inPlaceTrusted].size = 1 },
		"socket-replaced":   func(m *memFS) { m.nodes[d+"/"+cursorWorkerSocket] = &memNode{mode: fs.ModeSocket | 0o700} },
		"uuid-dir-mtime":    func(m *memFS) { m.nodes[tdir].mtime = epoch.Add(time.Second) },
		"dir-replaced":      func(m *memFS) { m.nodes[d].mtime = epoch.Add(time.Second) },
		"approval-bytes":    func(m *memFS) { m.nodes[d+"/"+cursorApprovalsFile].data = `["probe-fedcba9876543210"]` },
		"approval-mtime":    func(m *memFS) { m.nodes[d+"/"+cursorApprovalsFile].mtime = epoch.Add(time.Second) },
		"extra-top":         func(m *memFS) { m.file(d+"/x", "") },
		"extra-transcripts": func(m *memFS) { m.file(d+"/"+inPlaceTranscripts+"/x", "") },
		"extra-uuid-dir":    func(m *memFS) { m.file(tdir+"/x", "") },
		"unlistable":        func(m *memFS) { m.nodes[tdir].listError = fs.ErrPermission },
	}
	for _, e := range []string{cursorWorkerSocket, inPlaceLog, inPlaceRepo, inPlaceTrusted, cursorApprovalsFile, inPlaceTranscripts, inPlaceTranscripts + "/" + rsUUID,
		inPlaceTranscripts + "/" + rsUUID + "/" + rsUUID + ".jsonl"} {
		changes["gone-"+e] = func(m *memFS) { rsDel(m, d+"/"+e) }
	}
	for name, change := range changes {
		m, s, p := admitInPlace()
		change(m)
		if err := p.revalidate(); err == nil {
			t.Errorf("in_place %s: revalidated", name)
		}
		// The next case is blocked: neither its baseline nor its approval
		// inventory accepts the change, and no session file is opened.
		if _, err := p.sessionBaseline(nil); err == nil {
			t.Errorf("in_place %s: the next baseline accepted it", name)
		}
		roots, _ := approvalRoots(s, rsWS2, rsHome)
		sc, _ := p.scanner()
		if got := p.scan(sc, roots, ""); got.complete {
			t.Errorf("in_place %s: the next approval inventory accepted it", name)
		}
		if s.sessionOpens() != 0 {
			t.Errorf("in_place %s: opens %v", name, s.opens)
		}
	}
	// Exemption and metadata-only recording need the admitted identity, by
	// exact path; a reserved path whose metadata changed is never opened.
	_, p = admitHashed()
	if !p.exempt(dir+"/worker.sock", memInfo{name: "worker.sock", mode: fs.ModeSocket | 0o755, mtime: epoch}) ||
		p.exempt(dir+"/worker.sock", memInfo{name: "worker.sock", mode: fs.ModeSocket | 0o700, mtime: epoch}) ||
		p.exempt(dir+"/other.sock", memInfo{name: "other.sock", mode: fs.ModeSocket | 0o755, mtime: epoch}) {
		t.Fatal("exemption")
	}
	if r, b, _ := p.metaOnly(dir+"/x", memInfo{name: "x", mode: 0o600, mtime: epoch}); !r || b {
		t.Fatal("a regular file inside a hashed residue directory")
	}
	m, _, p = admitInPlace()
	logInfo, _ := m.Lstat(d + "/" + inPlaceLog)
	for _, tc := range []struct {
		p               string
		info            fs.FileInfo
		reserved, bound bool
	}{
		{d + "/" + inPlaceLog, logInfo, true, true},
		{d + "/" + inPlaceLog, memInfo{name: inPlaceLog, mode: 0o600, size: logInfo.Size(), mtime: epoch.Add(1)}, true, false},
		{d + "/" + inPlaceRepo, logInfo, true, false},
		{d + "/" + cursorApprovalsFile, logInfo, false, false},
		// A3.1: a reserved name outside the ledger is never opened either.
		{rsProjects + "/owner-project/" + inPlaceLog, logInfo, true, false},
		{d + "x/" + inPlaceLog, logInfo, true, false},
		{rsProjects + "/owner-project/notes.json", logInfo, false, false},
	} {
		if r, b, _ := p.metaOnly(tc.p, tc.info); r != tc.reserved || b != tc.bound {
			t.Errorf("metaOnly %s: %v %v", tc.p, r, b)
		}
	}
	if !p.exempt(d+"/"+cursorWorkerSocket, memInfo{name: cursorWorkerSocket, mode: fs.ModeSocket | 0o775, mtime: epoch}) {
		t.Fatal("the in-place socket exemption")
	}
	// Atomic admission: a candidate changed while the safety scan ran is
	// never admitted, and its changed file is still never opened.
	m = rsWorld()
	s := rsSpy(m)
	p = rsPreparer(s, ApprovalScanLimits{})
	b := rsEnable(t, m, p, rsWS1)
	w := session(t, p, "cursor-setup", rsWS1, b, func() {
		rsInPlace(m, rsWS1)
		s.onOpen = func(path string) {
			if strings.HasSuffix(path, "/.cursor/mcp.json") {
				s.mu.Lock()
				m.nodes[d+"/"+inPlaceLog].mtime = epoch.Add(time.Second)
				s.mu.Unlock()
			}
		}
	})
	if w.State != ResidueUnverified || len(p.ledger.entries) != 0 || len(w.Entries) != 0 || s.sessionOpens() != 0 {
		t.Fatalf("a changing candidate: %+v %v", w, s.opens)
	}
	// The harness never opens, connects to or deletes: the filesystem view
	// has no write, remove or dial operation.
	typ := reflect.TypeOf((*approvalFS)(nil)).Elem()
	for i := 0; i < typ.NumMethod(); i++ {
		if n := typ.Method(i).Name; n != "Lstat" && n != "ReadDir" && n != "Open" && n != "EvalSymlinks" {
			t.Fatalf("approvalFS gained %s", n)
		}
	}
	// The ledger is run-local: a second preparer starts empty.
	if q := rsPreparer(m, ApprovalScanLimits{}); len(q.ledger.entries) != 0 || q.ledger.next != 0 {
		t.Fatal("ledger reuse")
	}
}

// The parent slug P and A3's conditional cap (r0.14 post-final
// clarification DW1): only an observed hashed candidate is capped, at 54,
// with the verbatim reason, before the independent k >= 2 predicate; an
// in_place tree or no residue needs no cap; each layout is forced here
// independently of the workspace's length.
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
	if MaxResidueParentSlug != 54 || ReasonResidueParentSlug != "cursor_approval_scope_unverified: cursor residue parent slug exceeds 54 characters" {
		t.Fatal(ReasonResidueParentSlug)
	}
	// A workspace whose P has n characters, with its observed 57-character
	// hashed truncation and its k=2 form.
	world := func(n int) (*memFS, *cursorPreparer, string, string) {
		top := strings.Repeat("p", n-5)
		ws := "/" + top + "/.work/cursor-setup"
		m := newMemFS()
		m.file(ws+"/.cursor/mcp.json", "{}")
		m.mkdirs(rsProjects)
		p := rsPreparer(m, ApprovalScanLimits{})
		if parent, _, err := p.parentSlug(ws); err != nil || len(parent) != n {
			t.Fatalf("P %q %v", parent, err)
		}
		return m, p, ws, top + "-work-cursor-setup"
	}
	for _, tc := range []struct {
		n      int
		layout string // "hashed57", "hashedk2", "hashedk1", "in_place" or "none"
		want   string // "" verified, else the exact reason or a fragment
	}{
		{54, "hashed57", ""}, {54, "hashedk2", ""}, {54, "hashedk1", "attributed hashed name"},
		{55, "hashed57", ReasonResidueParentSlug}, {55, "hashedk2", ReasonResidueParentSlug}, {55, "hashedk1", ReasonResidueParentSlug},
		{55, "in_place", ""}, {55, "none", ""}, {90, "in_place", ""}, {90, "none", ""}, {90, "hashedk2", ReasonResidueParentSlug},
		{10, "in_place", ""},
	} {
		m, p, ws, slug := world(tc.n)
		parent := slug[:tc.n]
		var b *cursorApprovalBaseline
		if tc.layout == "in_place" || tc.layout == "none" {
			m.file(rsProjects+"/"+slug+"/"+cursorApprovalsFile, rsApproval)
			sc, _ := p.scanner()
			info, data, _ := sc.projectApprovalFile(rsProjects + "/" + slug + "/" + cursorApprovalsFile)
			var err error
			if b, err = p.approvalBaseline(slug, info, data); err != nil {
				t.Fatal(err)
			}
		}
		w := session(t, p, "cursor-setup", ws, b, func() {
			switch tc.layout {
			case "hashed57":
				socketAt(m, (slug)[:57]+"-1b81996", nil)
			case "hashedk2":
				socketAt(m, parent+"-cu-1b81996", nil)
			case "hashedk1":
				socketAt(m, parent+"-c-1b81996", nil)
			case "in_place":
				d := rsProjects + "/" + slug
				m.nodes[d+"/"+cursorWorkerSocket] = &memNode{mode: fs.ModeSocket | 0o775}
				m.file(d+"/"+inPlaceLog, rsLog)
				m.file(d+"/"+inPlaceRepo, rsRepo)
				m.file(d+"/"+inPlaceTrusted, rsTrusted)
				m.file(d+"/"+inPlaceTranscripts+"/"+rsUUID+"/"+rsUUID+".jsonl", rsTranscript)
			}
		})
		label := fmt.Sprintf("%s/P=%d", tc.layout, tc.n)
		switch {
		case tc.want == "" && (w.State != ResidueVerified || tc.layout != "none" && len(w.Entries) != 1):
			t.Errorf("%s: %+v", label, w)
		case tc.want == ReasonResidueParentSlug && (w.State != ResidueUnverified || deref(w.Reason) != ReasonResidueParentSlug):
			t.Errorf("%s: want the verbatim cap reason, got %q", label, deref(w.Reason))
		case tc.want != "" && tc.want != ReasonResidueParentSlug && !strings.Contains(deref(w.Reason), tc.want):
			t.Errorf("%s: %q", label, deref(w.Reason))
		}
	}
}

// The residue record's strict validation (shared by capture and report):
// v1 keeps its historical meaning and is strictly readable; v2 has exactly
// its members, its layout's items and flags.
func TestWorkerResidueRecord(t *testing.T) {
	v1 := WorkerResidueEntry{CaseID: "a", Path: residuePath(1), Origin: ResidueOrigin, PrefixVerified: true, SuffixVerified: true, SocketTypeVerified: true,
		SoleEntryVerified: true, IdentityVerified: true}
	ran := func(id string) bool { return id == "a" || id == "b" || id == "c" || id == "d" }
	good := map[string]WorkerResidue{
		"v1":       {Policy: WorkerResiduePolicy, State: ResidueVerified, Entries: []WorkerResidueEntry{v1}},
		"hashed":   {Policy: WorkerResiduePolicyV2, State: ResidueVerified, Entries: []WorkerResidueEntry{rsHashedEntry("a", 1)}},
		"in_place": {Policy: WorkerResiduePolicyV2, State: ResidueVerified, Entries: []WorkerResidueEntry{rsInPlaceEntry("a", 1)}},
		"both":     {Policy: WorkerResiduePolicyV2, State: ResidueVerified, Entries: []WorkerResidueEntry{rsInPlaceEntry("a", 1), rsHashedEntry("b", 2)}},
	}
	for name, w := range good {
		if err := w.validate(ran, 3); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		// The strict raw members and a lossless roundtrip.
		b, _ := json.Marshal(w)
		if err := checkResidueRaw(b, "r"); err != nil {
			t.Fatalf("%s raw: %v", name, err)
		}
		var back WorkerResidue
		if err := json.Unmarshal(b, &back); err != nil || !reflect.DeepEqual(back, w) {
			t.Fatalf("%s roundtrip: %+v %v", name, back, err)
		}
		again, _ := json.Marshal(back)
		if string(again) != string(b) {
			t.Fatalf("%s: %s != %s", name, again, b)
		}
	}
	if b, _ := json.Marshal(good["v1"]); strings.Contains(string(b), "layout") || strings.Contains(string(b), "items") {
		t.Fatalf("v1 with v2 members: %s", b)
	}
	if b, _ := json.Marshal(good["hashed"]); strings.Contains(string(b), "prefix_verified") || !strings.Contains(string(b), `"approval_unchanged":null`) {
		t.Fatalf("v2 hashed members: %s", b)
	}
	mut := func(base string, f func(w *WorkerResidue)) WorkerResidue {
		w := good[base]
		w.Entries = append([]WorkerResidueEntry(nil), w.Entries...)
		for i := range w.Entries {
			w.Entries[i].Items = append([]ResidueItem(nil), w.Entries[i].Items...)
		}
		f(&w)
		return w
	}
	for name, w := range map[string]WorkerResidue{
		"policy":       mut("hashed", func(w *WorkerResidue) { w.Policy = "x" }),
		"state":        mut("hashed", func(w *WorkerResidue) { w.State = "ok" }),
		"reason":       mut("hashed", func(w *WorkerResidue) { w.Reason = sptr("x") }),
		"no-reason":    mut("hashed", func(w *WorkerResidue) { w.State = ResidueUnverified }),
		"empty-reason": mut("hashed", func(w *WorkerResidue) { w.State, w.Reason = ResidueUnverified, sptr("") }),
		"nil-entries":  mut("hashed", func(w *WorkerResidue) { w.Entries = nil }),
		"not-checked":  mut("hashed", func(w *WorkerResidue) { w.State, w.Reason = ResidueNotChecked, sptr("x") }),
		"dup-path":     mut("both", func(w *WorkerResidue) { w.Entries[1].Path = residuePath(1) }),
		"dup-owner":    mut("both", func(w *WorkerResidue) { w.Entries[1].CaseID = "a" }),
		"raw-path": mut("hashed", func(w *WorkerResidue) {
			w.Entries[0].Path = "<home>/.cursor/projects/c-out-work-cu-1b81996/worker.sock"
		}),
		"origin":  mut("hashed", func(w *WorkerResidue) { w.Entries[0].Origin = "pre_existing" }),
		"not-run": mut("hashed", func(w *WorkerResidue) { w.Entries[0].CaseID = "z" }),
		"too-many": mut("both", func(w *WorkerResidue) {
			w.Entries = append(w.Entries, rsHashedEntry("c", 3), rsHashedEntry("d", 4))
		}),
		// v1 never certifies a v2 layout; v2 has no v1 member.
		"v1-flag":         mut("v1", func(w *WorkerResidue) { w.Entries[0].SoleEntryVerified = false }),
		"v1-in-place":     mut("v1", func(w *WorkerResidue) { w.Entries[0].Layout = ResidueLayoutInPlace }),
		"v1-items":        mut("v1", func(w *WorkerResidue) { w.Entries[0].Items = []ResidueItem{} }),
		"v1-approval":     mut("v1", func(w *WorkerResidue) { w.Entries[0].ApprovalUnchanged = bptr(true) }),
		"v1-layout-flag":  mut("v1", func(w *WorkerResidue) { w.Entries[0].LayoutVerified = true }),
		"v2-v1-entry":     mut("hashed", func(w *WorkerResidue) { w.Entries[0] = v1 }),
		"v2-prefix":       mut("hashed", func(w *WorkerResidue) { w.Entries[0].PrefixVerified = true }),
		"v2-sole":         mut("in_place", func(w *WorkerResidue) { w.Entries[0].SoleEntryVerified = true }),
		"layout":          mut("hashed", func(w *WorkerResidue) { w.Entries[0].Layout = "nested" }),
		"layout-flag":     mut("hashed", func(w *WorkerResidue) { w.Entries[0].LayoutVerified = false }),
		"identity-flag":   mut("in_place", func(w *WorkerResidue) { w.Entries[0].IdentityVerified = false }),
		"socket-flag":     mut("in_place", func(w *WorkerResidue) { w.Entries[0].SocketTypeVerified = false }),
		"approval-null":   mut("in_place", func(w *WorkerResidue) { w.Entries[0].ApprovalUnchanged = nil }),
		"approval-false":  mut("in_place", func(w *WorkerResidue) { w.Entries[0].ApprovalUnchanged = bptr(false) }),
		"approval-hashed": mut("hashed", func(w *WorkerResidue) { w.Entries[0].ApprovalUnchanged = bptr(true) }),
		"hashed-items":    mut("hashed", func(w *WorkerResidue) { w.Entries[0].Items = rsInPlaceEntry("a", 1).Items }),
		"in-place-items":  mut("in_place", func(w *WorkerResidue) { w.Entries[0].Items = rsHashedEntry("a", 1).Items }),
		"no-items":        mut("hashed", func(w *WorkerResidue) { w.Entries[0].Items = nil }),
		"item-order": mut("in_place", func(w *WorkerResidue) {
			it := w.Entries[0].Items
			it[5], it[6] = it[6], it[5]
		}),
		"item-name": mut("in_place", func(w *WorkerResidue) {
			w.Entries[0].Items[3].Path = "agent-transcripts/" + rsUUID + "/" + rsUUID + ".jsonl"
		}),
		"item-type":       mut("in_place", func(w *WorkerResidue) { w.Entries[0].Items[1].Type = ResidueItemRegular }),
		"item-type-other": mut("hashed", func(w *WorkerResidue) { w.Entries[0].Items[0].Type = "fifo" }),
		"item-negative":   mut("in_place", func(w *WorkerResidue) { w.Entries[0].Items[5].Size = -1 }),
		"item-over-log":   mut("in_place", func(w *WorkerResidue) { w.Entries[0].Items[6].Size = maxInPlaceLog + 1 }),
		"item-over-repo":  mut("in_place", func(w *WorkerResidue) { w.Entries[0].Items[5].Size = maxInPlaceSmall + 1 }),
		"item-over-trust": mut("in_place", func(w *WorkerResidue) { w.Entries[0].Items[0].Size = maxInPlaceSmall + 1 }),
		"item-over-trans": mut("in_place", func(w *WorkerResidue) { w.Entries[0].Items[3].Size = maxInPlaceTranscript + 1 }),
		"item-over-appr":  mut("in_place", func(w *WorkerResidue) { w.Entries[0].Items[4].Size = DefaultApprovalScanLimits().FileBytes + 1 }),
		"item-socket-sz":  mut("hashed", func(w *WorkerResidue) { w.Entries[0].Items[0].Size = 1 }),
		"item-dir-sz":     mut("in_place", func(w *WorkerResidue) { w.Entries[0].Items[1].Size = 4096 }),
	} {
		if err := w.validate(ran, 3); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	// An unverified record may keep false flags (never a v1 member in v2).
	unv := mut("in_place", func(w *WorkerResidue) {
		w.State, w.Reason = ResidueUnverified, sptr("x")
		w.Entries[0].ApprovalUnchanged = bptr(false)
	})
	if err := unv.validate(ran, 3); err != nil {
		t.Fatal(err)
	}
	// Strict raw members, per policy version.
	if err := checkResidueRaw(json.RawMessage(`null`), "r"); err == nil {
		t.Fatal("a null residue")
	}
	b1, _ := json.Marshal(good["v1"])
	b2, _ := json.Marshal(good["in_place"])
	bh, _ := json.Marshal(good["hashed"])
	for _, bad := range []string{
		strings.Replace(string(b1), `"identity_verified":true`, `"identity_verified":null`, 1),
		strings.Replace(string(b1), `"origin"`, `"x":1,"origin"`, 1),
		strings.Replace(string(b1), `"entries":[`, `"entries":[null,`, 1),
		strings.Replace(string(b1), `"case_id"`, `"layout":"hashed","case_id"`, 1),
		strings.Replace(string(b2), `"approval_unchanged":true`, `"approval_unchanged":true,"sole_entry_verified":true`, 1),
		strings.Replace(string(b2), `"layout":"in_place",`, ``, 1),
		strings.Replace(string(b2), `"items":[`, `"items":[null,`, 1),
		strings.Replace(string(b2), `"type":"socket"`, `"type":"socket","inode":7`, 1),
		strings.Replace(string(b2), `"size":0}]`, `}]`, 1),
		strings.Replace(string(bh), `"items":[{"path":"worker.sock","type":"socket","size":0}]`, `"items":null`, 1),
		strings.Replace(string(bh), `"layout":"hashed"`, `"layout":null`, 1),
		strings.Replace(string(bh), `"policy":"`+WorkerResiduePolicyV2+`"`, `"policy":7`, 1),
		strings.Replace(string(bh), `"entries":[`, `"entries":["x",`, 1),
	} {
		if err := checkResidueRaw(json.RawMessage(bad), "r"); err == nil {
			t.Errorf("raw %s accepted", bad)
		}
	}
	// Capture: only the pinned adapter's trusted recipe, only its own case;
	// a verified in_place entry needs this capture's project_scoped
	// approval and cleanly reaped session.
	reaped := &CaseCleanup{GroupGone: true}
	scoped := newApproval("x")
	scoped.Scope = ScopeProjectScoped
	for name, w := range map[string]WorkerResidue{"v1": good["v1"], "hashed": good["hashed"], "in_place": good["in_place"]} {
		w := w
		c := &CaptureClient{ID: "cursor", CaseID: "a", ExpectedVersion: CursorRealVersion, Approval: scoped, Session: CaptureStage{State: StageRan, Cleanup: reaped},
			State: CaptureComplete, WorkerResidue: &w}
		if err := c.validateWorkerResidue("linux", "amd64"); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
	}
	inPlace := good["in_place"]
	c := &CaptureClient{ID: "cursor", CaseID: "a", ExpectedVersion: CursorRealVersion, Approval: scoped, Session: CaptureStage{State: StageRan, Cleanup: reaped},
		State: CaptureComplete, WorkerResidue: &inPlace}
	for name, f := range map[string]func(c *CaptureClient){
		"darwin":         func(c *CaptureClient) {},
		"no-adapter":     func(c *CaptureClient) { c.Approval = nil },
		"other-case":     func(c *CaptureClient) { c.CaseID = "b" },
		"unverified":     func(c *CaptureClient) { c.WorkerResidue = sptrResidue(notCheckedResidue("x")) },
		"no-session":     func(c *CaptureClient) { c.Session.State = StagePrepared; c.State = CapturePartial },
		"workspace-only": func(c *CaptureClient) { a := *scoped; a.Scope = ScopeWorkspaceOnly; c.Approval = &a },
		"not-reaped":     func(c *CaptureClient) { c.Session.Cleanup = &CaseCleanup{} },
		"no-cleanup":     func(c *CaptureClient) { c.Session.Cleanup = nil },
		"two-entries": func(c *CaptureClient) {
			w := good["both"]
			c.WorkerResidue = &w
		},
	} {
		cc := *c
		f(&cc)
		goos := "linux"
		if name == "darwin" {
			goos = "darwin"
		}
		if err := cc.validateWorkerResidue(goos, "amd64"); err == nil {
			t.Errorf("capture %s: accepted", name)
		}
	}
}

// Across case records (A3): a path keeps its owner, layout and items, may
// only disappear whole, never reappears; a verified in_place entry's owner
// has its provenance; at most three in all.
func TestWorkerResidueAcrossCases(t *testing.T) {
	ver := func(es ...WorkerResidueEntry) WorkerResidue {
		return WorkerResidue{Policy: WorkerResiduePolicyV2, State: ResidueVerified, Entries: append([]WorkerResidueEntry{}, es...)}
	}
	a, b, c := rsInPlaceEntry("a", 1), rsHashedEntry("b", 2), rsInPlaceEntry("c", 3)
	tr := newResidueTrack()
	for i, step := range []struct {
		id string
		w  WorkerResidue
		ok bool
	}{
		{"a", ver(a), true}, {"b", ver(a, b), true}, {"c", ver(a, b, c), true},
	} {
		if err := tr.track(step.id, step.w, true); err != nil {
			t.Fatalf("step %d: %v", i, err)
		}
	}
	for name, steps := range map[string][]struct {
		id         string
		w          WorkerResidue
		provenance bool
	}{
		"owner": {{"a", ver(a), true}, {"b", ver(func() WorkerResidueEntry { e := a; e.CaseID = "b"; return e }()), true}},
		"layout": {{"a", ver(a), true}, {"b", ver(func() WorkerResidueEntry {
			e := rsHashedEntry("a", 1)
			return e
		}()), true}},
		"items": {{"a", ver(a), true}, {"b", ver(func() WorkerResidueEntry {
			e := rsInPlaceEntry("a", 1)
			e.Items[6].Size++
			return e
		}()), true}},
		"reappears":     {{"a", ver(a), true}, {"b", ver(), true}, {"c", ver(a), true}},
		"no-provenance": {{"a", ver(a), false}},
		"later-claims":  {{"a", ver(), false}, {"b", ver(a), true}},
		"too-many": {{"a", ver(a), true}, {"b", ver(a, b), true}, {"c", ver(a, b, c), true},
			{"d", ver(a, b, c, rsHashedEntry("d", 4)), true}},
	} {
		tr := newResidueTrack()
		var err error
		for _, s := range steps {
			if err = tr.track(s.id, s.w, s.provenance); err != nil {
				break
			}
		}
		if err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	// A not_checked record says nothing about disappearance; an unverified
	// record without provenance only lists earlier admitted entries.
	tr = newResidueTrack()
	if err := errorsJoin(tr.track("a", ver(a), true), tr.track("b", notCheckedResidue("x"), false), tr.track("c", ver(a), false),
		tr.track("d", WorkerResidue{Policy: WorkerResiduePolicyV2, State: ResidueUnverified, Reason: sptr("x"), Entries: []WorkerResidueEntry{a}}, false)); err != nil {
		t.Fatal(err)
	}
	// A whole entry may disappear (then stays gone).
	tr = newResidueTrack()
	if err := errorsJoin(tr.track("a", ver(a), true), tr.track("b", ver(b), true)); err != nil || !tr.gone[a.Path] {
		t.Fatalf("disappearance: %v", err)
	}
}

func errorsJoin(errs ...error) error {
	for _, e := range errs {
		if e != nil {
			return e
		}
	}
	return nil
}

// Scanner privacy integration (A3): the metadata-only hook records bound
// files without opening them, refuses a reserved but unbound one without
// opening it, and the diff never compares an absent digest as content.
func TestWorkerResidueScannerHook(t *testing.T) {
	m := newMemFS()
	m.file("/r/a.log", "secret")
	m.file("/r/b.json", "{}")
	s := rsSpy(m)
	sc, err := newApprovalScanner(s, ApprovalScanLimits{})
	if err != nil {
		t.Fatal(err)
	}
	roots := []scanRoot{{label: "<r>", path: "/r"}}
	bound := true
	sc.metaOnly = func(p string, _ fs.FileInfo) (bool, bool, error) { return p == "/r/a.log", bound, nil }
	pre := sc.snapshot(roots)
	if !pre.complete || !pre.entries["<r>/a.log"].meta || pre.entries["<r>/b.json"].meta || s.opens["/r/a.log"] != 0 || s.opens["/r/b.json"] != 1 ||
		pre.bytes != int64(len("secret")+len("{}")) || pre.entries["<r>/a.log"].mtime != epoch.UnixNano() {
		t.Fatalf("pre %+v %v", pre, s.opens)
	}
	// A changed modification time or a lost marker is a modification.
	m.nodes["/r/a.log"].mtime = epoch.Add(time.Second)
	post := sc.snapshot(roots)
	if ch := diffSnapshots(pre, post); len(ch) != 1 || ch[0].Path != "<r>/a.log" || ch[0].Kind != ChangeModified {
		t.Fatalf("mtime diff %+v", ch)
	}
	sc.metaOnly = nil
	digested := sc.snapshot(roots)
	if ch := diffSnapshots(post, digested); len(ch) != 1 || ch[0].Kind != ChangeModified || s.opens["/r/a.log"] != 1 {
		t.Fatalf("marker diff %+v", ch)
	}
	// Reserved but no longer bound: incomplete, never opened.
	bound = false
	sc.metaOnly = func(p string, _ fs.FileInfo) (bool, bool, error) { return p == "/r/a.log", bound, nil }
	before := s.opens["/r/a.log"]
	if got := sc.snapshot(roots); got.complete || !strings.Contains(got.why, "never read") || s.opens["/r/a.log"] != before {
		t.Fatalf("unbound %+v", got)
	}
	// The per-file cap still applies to a metadata-only file.
	m.nodes["/r/a.log"].size = 101
	sc.lim.FileBytes = 100
	if got := sc.snapshot(roots); got.complete {
		t.Fatal("over the per-file cap")
	}
}

// Design decoder-enrollment A3.1 (UT-25 extension): unchanged foreign
// Cursor session artifacts, on injected trees.

// rsDev is an unrelated owner project with a complete session tree.
var rsDev = rsProjects + "/home-o-dev-flow"

// rsForeign adds pre-existing foreign session artifacts: a complete tree
// with nested transcript files of arbitrary names and a stale socket, a
// partial tree, an empty reserved directory, a socket at a nonreserved
// name and a pre-existing directory of A3's general hashed shape for
// rsWS1 (not a 57-character prefix).
func rsForeign(m *memFS) {
	m.file(rsDev+"/"+cursorApprovalsFile, `["probe-aaaaaaaaaaaaaaaa"]`)
	m.file(rsDev+"/"+inPlaceLog, "dev "+rsCanary)
	m.file(rsDev+"/"+inPlaceRepo, `{"id":"`+rsCanary+`"}`)
	m.file(rsDev+"/"+inPlaceTrusted, `{"note":"`+rsCanary+`"}`)
	m.file(rsDev+"/"+inPlaceTranscripts+"/u1/u1.jsonl", rsCanary)
	m.file(rsDev+"/"+inPlaceTranscripts+"/u1/subagents/notes.txt", rsCanary)
	m.nodes[rsDev+"/"+cursorWorkerSocket] = &memNode{mode: fs.ModeSocket | 0o755}
	m.file(rsProjects+"/old-partial/"+inPlaceLog, rsCanary)
	m.mkdirs(rsProjects + "/old-empty/" + inPlaceTranscripts)
	m.mkdirs(rsProjects + "/old-sock")
	m.nodes[rsProjects+"/old-sock/agent.sock"] = &memNode{mode: fs.ModeSocket | 0o755}
	socketAt(m, "c-out-work-cu-0123abc", nil)
}

// rsCase runs one case's preparation and session on p as approve and the
// residue check do: the inventory before the enable (with the case's
// ownership checks), the enable, the inventory after it, the session
// leaving layout's residue ("in_place", "hashed" or "").
func rsCase(t testing.TB, m *memFS, p *cursorPreparer, caseID, ws, layout string) WorkerResidue {
	t.Helper()
	roots, _ := approvalRoots(p.fsys, ws, rsHome)
	sc, _ := p.scanner()
	if got := p.scan(sc, roots, rsSlug(ws)); !got.complete {
		t.Fatalf("%s before the enable: %q", caseID, got.why)
	}
	b := rsEnable(t, m, p, ws)
	sc, _ = p.scanner()
	if got := p.scan(sc, roots, ""); !got.complete {
		t.Fatalf("%s after the enable: %q", caseID, got.why)
	}
	return session(t, p, caseID, ws, b, func() {
		switch layout {
		case "in_place":
			rsInPlace(m, ws)
		case "hashed":
			socketAt(m, rsSlug(ws)[:len("c-out-work-")+2]+"-"+map[string]string{rsWS1: "1b81996", rsWS2: "2b81996", rsWS3: "3b81996"}[ws], nil)
		}
	})
}

// rsForeignIntact fails when a foreign artifact changed or disappeared.
func rsForeignIntact(t *testing.T, m *memFS, want *memFS) {
	t.Helper()
	for k, n := range want.nodes {
		if strings.HasPrefix(k, rsDev) || strings.Contains(k, "/old-") || strings.Contains(k, "c-out-work-cu-0123abc") {
			if got := m.nodes[k]; got == nil || got.mode != n.mode || got.data != n.data {
				t.Fatalf("%s changed", k)
			}
		}
	}
}

func TestForeignBaselineTolerated(t *testing.T) {
	m := rsWorld()
	rsForeign(m)
	orig := m.clone()
	s := rsSpy(m)
	p := rsPreparer(s, ApprovalScanLimits{})
	w1 := rsCase(t, m, p, "cursor-setup", rsWS1, "in_place")
	w2 := rsCase(t, m, p, "cursor-default-1", rsWS2, "hashed")
	w3 := rsCase(t, m, p, "cursor-default-2", rsWS3, "")
	for i, w := range []WorkerResidue{w1, w2, w3} {
		if w.State != ResidueVerified || len(w.Entries) != min(i+1, 2) {
			t.Fatalf("case %d: %+v %s", i, w, deref(w.Reason))
		}
		// No foreign field, count or name in the evidence.
		b, _ := json.Marshal(w)
		for _, raw := range []string{"dev-flow", "old-", "0123abc", "foreign", rsCanary} {
			if strings.Contains(string(b), raw) {
				t.Fatalf("case %d evidence carries %q: %s", i, raw, b)
			}
		}
	}
	// The pre-existing hashed-shaped directory is never added or admitted.
	if w2.Entries[1].Layout != ResidueLayoutHashed || len(p.ledger.entries) != 2 || p.ledger.entries[1].name != "c-out-work-cu-2b81996" {
		t.Fatalf("ledger %+v", p.ledger.entries)
	}
	// Zero session-content opens (foreign or run residue) through every
	// inventory, safety scan and recheck; the approvals were read.
	if s.sessionOpens() != 0 || s.approvalOpens() == 0 {
		t.Fatalf("opens %v", s.opens)
	}
	for k := range s.opens {
		if strings.Contains(k, "/old-") || strings.HasPrefix(k, rsDev+"/"+inPlaceTranscripts) {
			t.Fatalf("a foreign session file was opened: %s", k)
		}
	}
	rsForeignIntact(t, m, orig)
	// A separate preparer (a capture, or another client) initializes its
	// own baseline from the current state, independently.
	q := rsPreparer(s, ApprovalScanLimits{})
	if err := q.preflight(nil, ""); err != nil || len(q.foreign.nodes) <= len(p.foreign.nodes) {
		t.Fatalf("an independent baseline: %v (%d, %d)", err, len(q.foreign.nodes), len(p.foreign.nodes))
	}
	m.file(rsProjects+"/later/"+inPlaceLog, "x")
	if p.preflight(nil, "") == nil || q.preflight(nil, "") == nil {
		t.Fatal("a later artifact was accepted")
	}
	if !p.foreign.initialized || p.foreign.err != nil {
		t.Fatal("the baseline was reset")
	}
}

// Every kind of change at each observation point (the next preflight, an
// injected change during an inventory, and a change during the session):
// the exact fixed reason, no admission, no session-content open, no leaked
// name.
func TestForeignBaselineChanges(t *testing.T) {
	sock := fs.ModeSocket | 0o755
	link := &memNode{mode: fs.ModeSymlink | 0o777}
	vectors := map[string]struct {
		change func(m *memFS)
		want   string
	}{
		"size":                {func(m *memFS) { m.nodes[rsDev+"/"+inPlaceLog].data += "x" }, ReasonForeignChanged},
		"mtime":               {func(m *memFS) { m.nodes[rsDev+"/"+inPlaceLog].mtime = epoch.Add(time.Nanosecond) }, ReasonForeignChanged},
		"mode":                {func(m *memFS) { m.nodes[rsDev+"/"+inPlaceRepo].mode = 0o644 }, ReasonForeignChanged},
		"socket-to-fifo":      {func(m *memFS) { m.nodes[rsDev+"/"+cursorWorkerSocket] = &memNode{mode: fs.ModeNamedPipe | 0o600} }, ReasonForeignChanged},
		"socket-replaced":     {func(m *memFS) { m.nodes[rsProjects+"/old-sock/agent.sock"] = &memNode{mode: fs.ModeSocket | 0o700} }, ReasonForeignChanged},
		"regular-to-link":     {func(m *memFS) { m.nodes[rsDev+"/"+inPlaceRepo] = link }, ReasonForeignChanged},
		"regular-to-dir":      {func(m *memFS) { m.nodes[rsDev+"/"+inPlaceRepo] = &memNode{mode: fs.ModeDir | 0o700} }, ReasonForeignChanged},
		"dir-to-link":         {func(m *memFS) { rsDel(m, rsDev+"/"+inPlaceTranscripts); m.nodes[rsDev+"/"+inPlaceTranscripts] = link }, ReasonForeignChanged},
		"ancestor-replaced":   {func(m *memFS) { m.nodes[rsDev].mode = fs.ModeDir | 0o755 }, ReasonForeignChanged},
		"ancestor-to-link":    {func(m *memFS) { rsDel(m, rsDev); m.nodes[rsDev] = link }, ReasonForeignChanged},
		"ancestor-to-file":    {func(m *memFS) { rsDel(m, rsProjects+"/old-sock"); m.file(rsProjects+"/old-sock", "") }, ReasonForeignChanged},
		"new-reserved":        {func(m *memFS) { m.file(rsProjects+"/new-project/"+inPlaceLog, "x") }, ReasonForeignNew},
		"new-socket":          {func(m *memFS) { m.nodes[rsProjects+"/old-sock/new.sock"] = &memNode{mode: sock} }, ReasonForeignNew},
		"new-descendant":      {func(m *memFS) { m.file(rsDev+"/"+inPlaceTranscripts+"/u1/u2.jsonl", "x") }, ReasonForeignNew},
		"new-in-empty":        {func(m *memFS) { m.file(rsProjects+"/old-empty/"+inPlaceTranscripts+"/x", "") }, ReasonForeignNew},
		"removed-file":        {func(m *memFS) { rsDel(m, rsDev+"/"+inPlaceRepo) }, ReasonForeignRemoved},
		"removed-socket":      {func(m *memFS) { rsDel(m, rsProjects+"/old-sock/agent.sock") }, ReasonForeignRemoved},
		"removed-subtree":     {func(m *memFS) { rsDel(m, rsDev+"/"+inPlaceTranscripts) }, ReasonForeignRemoved},
		"removed-empty":       {func(m *memFS) { rsDel(m, rsProjects+"/old-empty/"+inPlaceTranscripts) }, ReasonForeignRemoved},
		"removed-project":     {func(m *memFS) { rsDel(m, rsDev) }, ReasonForeignRemoved},
		"removed-and-changed": {func(m *memFS) { rsDel(m, rsDev+"/"+inPlaceRepo); m.nodes[rsDev+"/"+inPlaceLog].data += "x" }, ReasonForeignRemoved},
		"changed-and-new":     {func(m *memFS) { m.nodes[rsDev+"/"+inPlaceLog].data += "x"; m.file(rsProjects+"/n/"+inPlaceRepo, "") }, ReasonForeignChanged},
		"unrecorded-fifo":     {func(m *memFS) { m.nodes[rsDev+"/q"] = &memNode{mode: fs.ModeNamedPipe | 0o600} }, ReasonForeignUnverifiable},
		"unrecorded-link":     {func(m *memFS) { m.nodes[rsProjects+"/old-partial/"+inPlaceRepo] = link }, ReasonForeignUnverifiable},
		"unverifiable-first": {func(m *memFS) {
			rsDel(m, rsDev+"/"+inPlaceRepo)
			m.nodes[rsProjects+"/old-sock"].listError = fs.ErrPermission
		}, ReasonForeignUnverifiable},
		"unrelated-file-added": {func(m *memFS) { m.file(rsDev+"/README.md", "x") }, ""},
	}
	for name, tc := range vectors {
		for _, point := range []string{"preflight", "during-scan", "during-session"} {
			label := name + "/" + point
			m := rsWorld()
			rsForeign(m)
			s := rsSpy(m)
			p := rsPreparer(s, ApprovalScanLimits{})
			roots, _ := approvalRoots(s, rsWS1, rsHome)
			sc, _ := p.scanner()
			if got := p.scan(sc, roots, rsSlug(rsWS1)); !got.complete {
				t.Fatalf("%s: initial %q", label, got.why)
			}
			b := rsEnable(t, m, p, rsWS1)
			var reason string
			switch point {
			case "preflight":
				tc.change(m)
				sc, _ = p.scanner()
				reason = p.scan(sc, roots, "").why
			case "during-scan":
				var once sync.Once
				s.onOpen = func(path string) {
					if strings.HasSuffix(path, "/.cursor/mcp.json") {
						once.Do(func() { tc.change(m) })
					}
				}
				sc, _ = p.scanner()
				got := p.scan(sc, roots, "")
				if got.complete != (tc.want == "") || !got.complete && len(got.entries) != 0 {
					t.Errorf("%s: %+v", label, got)
				}
				reason = got.why
			default:
				w := session(t, p, "cursor-setup", rsWS1, b, func() { rsInPlace(m, rsWS1); tc.change(m) })
				if len(p.ledger.entries) != 0 && tc.want != "" || len(w.Entries) != 0 && tc.want != "" {
					t.Errorf("%s: admitted %+v", label, w)
				}
				reason = deref(w.Reason)
			}
			// Every observation point records A3.1's exact reason, also when
			// A3's classification fails too (a replaced or new project-root
			// child: code review A3 round 1, C3).
			if reason != tc.want {
				t.Errorf("%s: %q, want %q", label, reason, tc.want)
			}
			if s.sessionOpens() != 0 {
				t.Errorf("%s: opens %v", label, s.opens)
			}
		}
	}
}

// The ownership checks (A3.1): this case's full-slug path absent and no
// possible hashed-candidate directory, at its own preflight before its
// enable, before everything else and whatever the baseline holds.
func TestForeignBaselineOwnership(t *testing.T) {
	scan := func(p *cursorPreparer, ws, own string) *snapshot {
		roots, _ := approvalRoots(p.fsys, ws, rsHome)
		sc, _ := p.scanner()
		return p.scan(sc, roots, own)
	}
	d := rsDir(rsWS1)
	for name, tc := range map[string]struct {
		setup func(m *memFS)
		want  string
	}{
		"empty-dir":       {func(m *memFS) { m.mkdirs(d) }, ReasonCaseProjectExists},
		"approval-only":   {func(m *memFS) { m.file(d+"/"+cursorApprovalsFile, rsApproval) }, ReasonCaseProjectExists},
		"full-tree":       {func(m *memFS) { m.file(d+"/"+cursorApprovalsFile, rsApproval); rsInPlace(m, rsWS1) }, ReasonCaseProjectExists},
		"file":            {func(m *memFS) { m.file(d, "") }, ReasonCaseProjectExists},
		"link":            {func(m *memFS) { m.nodes[d] = &memNode{mode: fs.ModeSymlink | 0o777} }, ReasonCaseProjectExists},
		"before-traverse": {func(m *memFS) { m.mkdirs(d); m.nodes[rsProjects+"/owner-project"].listError = fs.ErrPermission }, ReasonCaseProjectExists},
		"unlookable":      {func(m *memFS) { m.mkdirs(d); m.nodes[d].lstatErr = fs.ErrPermission }, ReasonForeignUnverifiable},
		"short-no-hashed": {func(m *memFS) { socketAt(m, rsSlug(rsWS1)+"-0123abc", nil) }, ""},
	} {
		m := rsWorld()
		tc.setup(m)
		s := rsSpy(m)
		got := scan(rsPreparer(s, ApprovalScanLimits{}), rsWS1, rsSlug(rsWS1))
		if got.why != tc.want || got.complete != (tc.want == "") || len(s.opens) != 0 && tc.want != "" {
			t.Errorf("%s: %q", name, got.why)
		}
	}
	// A slug of at least 57 characters: S[:57]-hex7 directories are refused
	// (P>54 and a failed k predicate included); other names are not.
	long := func(n int, base string) string { return "/" + strings.Repeat("p", n-5) + "/.work/" + base }
	slugOf := func(ws string) string {
		return strings.Repeat("p", strings.Count(ws, "p")-strings.Count(path.Base(ws), "p")) + "-work-" + path.Base(ws)
	}
	for _, tc := range []struct {
		p      int
		suffix string
		want   string
	}{
		{54, "-0123abc", ReasonHashedCandidateExists}, {55, "-0123abc", ReasonHashedCandidateExists}, {60, "-fffffff", ReasonHashedCandidateExists},
		{54, "-0123ABC", ""}, {54, "-0123ab", ""}, {54, "-0123abcd", ""}, {54, "-0123abg", ""}, {54, "_0123abc", ""},
	} {
		ws := long(tc.p, "cursor-setup")
		m := rsWorld()
		m.file(ws+"/.cursor/mcp.json", "{}")
		slug := slugOf(ws)
		if len(slug) < hashedPrefixLen {
			t.Fatalf("slug %q", slug)
		}
		m.mkdirs(rsProjects + "/" + slug[:hashedPrefixLen] + tc.suffix)
		got := scan(rsPreparer(m, ApprovalScanLimits{}), ws, slug)
		if got.why != tc.want {
			t.Errorf("P=%d %s: %q", tc.p, tc.suffix, got.why)
		}
	}
	// An adaptive later case: its colliding artifacts were recorded in the
	// baseline initialized at the first case, and are refused only at its
	// own preflight (no later case is derived at the start).
	for _, tc := range []struct {
		name  string
		plant func(m *memFS, slug string)
		want  string
	}{
		{"own-full-slug", func(m *memFS, slug string) { m.file(rsProjects+"/"+slug+"/"+inPlaceLog, "x") }, ReasonCaseProjectExists},
		{"own-hashed", func(m *memFS, slug string) { socketAt(m, slug[:hashedPrefixLen]+"-0123abc", nil) }, ReasonHashedCandidateExists},
	} {
		wsA, wsB := long(45, "cursor-setup"), long(45, "cursor-default-1")
		m := rsWorld()
		m.file(wsA+"/.cursor/mcp.json", "{}")
		m.file(wsB+"/.cursor/mcp.json", "{}")
		tc.plant(m, slugOf(wsB))
		p := rsPreparer(m, ApprovalScanLimits{})
		if got := scan(p, wsA, slugOf(wsA)); !got.complete || len(p.foreign.nodes) == 0 {
			t.Fatalf("%s: the first case: %q", tc.name, got.why)
		}
		if got := scan(p, wsB, slugOf(wsB)); got.why != tc.want {
			t.Errorf("%s: the later case: %q", tc.name, got.why)
		}
	}
	// An empty or absent initial baseline is never reinitialized.
	for _, absent := range []bool{true, false} {
		m := newMemFS()
		m.file(rsWS1+"/.cursor/mcp.json", "{}")
		if !absent {
			m.mkdirs(rsProjects)
		}
		p := rsPreparer(m, ApprovalScanLimits{})
		if got := scan(p, rsWS1, rsSlug(rsWS1)); !got.complete || !p.foreign.initialized || len(p.foreign.nodes) != 0 {
			t.Fatalf("empty baseline: %q", got.why)
		}
		m.file(rsProjects+"/later/"+inPlaceLog, "x")
		if got := scan(p, rsWS1, ""); got.why != ReasonForeignNew {
			t.Errorf("absent %v: %q", absent, got.why)
		}
	}
}

// The bounded walk: entry, per-file and aggregate caps at the boundary and
// one over, the deadline, failed listings and lookups, and an unstable
// initial view, each failing closed without an open.
func TestForeignBaselineBounds(t *testing.T) {
	world := func() *memFS { m := rsWorld(); rsForeign(m); return m }
	m := world()
	entries, largest, total := 0, int64(0), int64(0)
	for k, n := range m.nodes {
		if strings.HasPrefix(k, rsProjects+"/") {
			entries++
			if n.mode.IsRegular() {
				largest, total = max(largest, int64(len(n.data))), total+int64(len(n.data))
			}
		}
	}
	for _, tc := range []struct {
		lim  ApprovalScanLimits
		want string
	}{
		{ApprovalScanLimits{Entries: entries}, ""}, {ApprovalScanLimits{Entries: entries - 1}, ReasonForeignLimit},
		{ApprovalScanLimits{FileBytes: largest}, ""}, {ApprovalScanLimits{FileBytes: largest - 1}, ReasonForeignLimit},
		{ApprovalScanLimits{Bytes: total}, ""}, {ApprovalScanLimits{Bytes: total - 1}, ReasonForeignLimit},
	} {
		s := rsSpy(world())
		err := rsPreparer(s, tc.lim).preflight(nil, "")
		if got := ""; err != nil && err.Error() != tc.want || err == nil && tc.want != "" || len(s.opens) != 0 {
			_ = got
			t.Errorf("%+v: %v", tc.lim, err)
		}
	}
	// The deadline: at the first walk, and at a later revalidation.
	p := rsPreparer(world(), ApprovalScanLimits{})
	p.expired = func() bool { return true }
	if err := p.preflight(nil, ""); err == nil || err.Error() != ReasonForeignDeadline {
		t.Fatalf("deadline: %v", err)
	}
	p = rsPreparer(world(), ApprovalScanLimits{})
	if err := p.preflight(nil, ""); err != nil {
		t.Fatal(err)
	}
	calls := 0
	p.expired = func() bool { calls++; return calls > 3 }
	if err := p.preflight(nil, ""); err == nil || err.Error() != ReasonForeignDeadline {
		t.Fatalf("deadline mid-walk: %v", err)
	}
	// Failed listings and lookups after the baseline.
	for name, change := range map[string]func(m *memFS){
		"readdir": func(m *memFS) { m.nodes[rsDev+"/"+inPlaceTranscripts].listError = fs.ErrPermission },
		"lstat":   func(m *memFS) { m.nodes[rsDev+"/"+inPlaceLog].lstatErr = fs.ErrPermission },
		"root":    func(m *memFS) { m.nodes[rsProjects].lstatErr = fs.ErrPermission },
	} {
		m := world()
		p := rsPreparer(m, ApprovalScanLimits{})
		if err := p.preflight(nil, ""); err != nil {
			t.Fatal(err)
		}
		change(m)
		if err := p.preflight(nil, ""); err == nil || err.Error() != ReasonForeignUnverifiable {
			t.Errorf("%s: %v", name, err)
		}
	}
	// An unstable initial view (a change between its two walks): no
	// baseline, never retried.
	m = world()
	s := rsSpy(m)
	lists := 0
	s.onList = func(p string) {
		if p == rsDev {
			if lists++; lists == 2 {
				m.nodes[rsDev+"/"+inPlaceLog].mtime = epoch.Add(time.Second)
			}
		}
	}
	p = rsPreparer(s, ApprovalScanLimits{})
	if err := p.preflight(nil, ""); err == nil || err.Error() != ReasonForeignUnverifiable || p.foreign.nodes != nil {
		t.Fatalf("unstable: %v", err)
	}
	s.onList = nil
	if err := p.preflight(nil, ""); err == nil || err.Error() != ReasonForeignUnverifiable {
		t.Fatal("a failed baseline was retried")
	}
	if len(s.opens) != 0 {
		t.Fatalf("opens %v", s.opens)
	}
	// The typed error: recognized by type, never by text.
	if _, ok := fixedReason(errors.New(ReasonForeignChanged)); ok {
		t.Fatal("a plain error with the reason's text was taken as typed")
	}
	if r, ok := fixedReason(fmt.Errorf("wrapped: %w", foreignFail(ReasonForeignNew))); !ok || r != ReasonForeignNew {
		t.Fatal("a wrapped typed error")
	}
}

// Scanner binding (A3.1): baseline files are recorded by metadata only
// while unchanged, reserved paths never opened, baseline sockets exempt by
// exact identity.
func TestForeignBaselineScannerBinding(t *testing.T) {
	m := rsWorld()
	rsForeign(m)
	p := rsPreparer(m, ApprovalScanLimits{})
	if err := p.preflight(nil, ""); err != nil {
		t.Fatal(err)
	}
	logInfo, _ := m.Lstat(rsDev + "/" + inPlaceLog)
	sockInfo, _ := m.Lstat(rsDev + "/" + cursorWorkerSocket)
	notes, _ := m.Lstat(rsDev + "/" + cursorApprovalsFile)
	changed := memInfo{name: inPlaceLog, mode: 0o600, size: logInfo.Size(), mtime: epoch.Add(1)}
	for _, tc := range []struct {
		p               string
		info            fs.FileInfo
		reserved, bound bool
		want            string
	}{
		{rsDev + "/" + inPlaceLog, logInfo, true, true, ""},
		{rsDev + "/" + inPlaceLog, changed, true, false, ReasonForeignChanged},
		{rsDev + "/" + inPlaceTranscripts + "/u1/subagents/notes.txt", func() fs.FileInfo {
			i, _ := m.Lstat(rsDev + "/" + inPlaceTranscripts + "/u1/subagents/notes.txt")
			return i
		}(), true, true, ""},
		{rsDev + "/" + inPlaceTranscripts + "/u1/new.txt", logInfo, true, false, ReasonForeignNew},
		{rsProjects + "/x/" + inPlaceRepo, logInfo, true, false, ReasonForeignNew},
		{rsDev + "/" + cursorApprovalsFile, notes, false, false, ""},
		{rsDev, logInfo, true, false, ReasonForeignChanged},
		{rsHome + "/.cursor/cli-config.json", logInfo, false, false, ""},
	} {
		r, b, fail := p.metaOnly(tc.p, tc.info)
		got := ""
		if fail != nil {
			got = fail.Error()
		}
		if r != tc.reserved || b != tc.bound || got != tc.want {
			t.Errorf("metaOnly %s: %v %v %q", tc.p, r, b, got)
		}
	}
	if !p.exempt(rsDev+"/"+cursorWorkerSocket, sockInfo) ||
		p.exempt(rsDev+"/"+cursorWorkerSocket, memInfo{name: cursorWorkerSocket, mode: fs.ModeSocket | 0o700, mtime: epoch}) ||
		p.exempt(rsDev+"/other.sock", sockInfo) {
		t.Fatal("foreign socket exemption")
	}
	// The scanner itself: a reserved but unbound file fails typed, before
	// any open.
	s := rsSpy(m)
	p.fsys = s
	sc, _ := p.scanner()
	m.nodes[rsDev+"/"+inPlaceLog].mtime = epoch.Add(time.Second)
	got := sc.snapshot([]scanRoot{{label: labelHomeCursor, path: rsHome + "/.cursor"}})
	if got.complete || got.typed == nil || got.why != ReasonForeignChanged || s.sessionOpens() != 0 {
		t.Fatalf("scanner %+v %v", got, s.opens)
	}
}

// Code review A3 round 1, C1: the final foreign revalidation before a
// candidate is accepted establishes a stable view. A foreign file changed
// right after that revalidation looked it up (its old metadata recorded) is
// detected by the view's second walk and refused as unverifiable: the
// candidate is not admitted. The change is armed by the safety scan's read
// of the foreign approval file; after it, the scan's own lookup of the log
// passes and the next lookup is the final revalidation's.
func TestForeignBaselineFinalBoundary(t *testing.T) {
	m := rsWorld()
	rsForeign(m)
	s := rsSpy(m)
	p := rsPreparer(s, ApprovalScanLimits{})
	roots, _ := approvalRoots(s, rsWS1, rsHome)
	sc, _ := p.scanner()
	if got := p.scan(sc, roots, rsSlug(rsWS1)); !got.complete {
		t.Fatal(got.why)
	}
	b := rsEnable(t, m, p, rsWS1)
	logPath := rsDev + "/" + inPlaceLog
	armed, lookups, fired := false, 0, false
	w := session(t, p, "cursor-setup", rsWS1, b, func() {
		rsInPlace(m, rsWS1)
		s.onOpen = func(path string) {
			if path == rsDev+"/"+cursorApprovalsFile {
				armed = true
			}
		}
		s.afterLstat = func(path string) {
			if armed && !fired && path == logPath {
				if lookups++; lookups == 2 {
					fired = true
					m.nodes[logPath].mtime = epoch.Add(time.Second)
				}
			}
		}
	})
	if !fired {
		t.Fatal("the change never fired")
	}
	if w.State != ResidueUnverified || deref(w.Reason) != ReasonForeignUnverifiable || len(p.ledger.entries) != 0 || len(w.Entries) != 0 || s.sessionOpens() != 0 {
		t.Fatalf("final boundary: %+v %q ledger %d", w, deref(w.Reason), len(p.ledger.entries))
	}
}

// Code review A3 round 1, W1 and W2: the hashed ownership rule applies only
// to a directory (a no-follow lookup; a file or link of that name is not a
// candidate), and revalidation never looks anything up beneath a recorded
// directory replaced by a link (or beneath a removed one).
func TestForeignBaselineReviewWarnings(t *testing.T) {
	ws := "/" + strings.Repeat("p", 49) + "/.work/cursor-setup"
	slug := strings.Repeat("p", 49) + "-work-cursor-setup"
	name := slug[:hashedPrefixLen] + "-0123abc"
	for kind, tc := range map[string]struct {
		plant func(m *memFS)
		want  string
	}{
		"directory": {func(m *memFS) { m.mkdirs(rsProjects + "/" + name) }, ReasonHashedCandidateExists},
		"file":      {func(m *memFS) { m.file(rsProjects+"/"+name, "x") }, ""},
		"link":      {func(m *memFS) { m.nodes[rsProjects+"/"+name] = &memNode{mode: fs.ModeSymlink | 0o777} }, ""},
	} {
		m := rsWorld()
		m.file(ws+"/.cursor/mcp.json", "{}")
		tc.plant(m)
		got := ""
		if err := rsPreparer(m, ApprovalScanLimits{}).ownership(slug); err != nil {
			got = err.Error()
		}
		if got != tc.want {
			t.Errorf("ownership %s: %q", kind, got)
		}
	}
	for name, tc := range map[string]struct {
		change func(m *memFS)
		under  string
		want   string
	}{
		"dir-to-link": {func(m *memFS) {
			rsDel(m, rsDev+"/"+inPlaceTranscripts)
			m.nodes[rsDev+"/"+inPlaceTranscripts] = &memNode{mode: fs.ModeSymlink | 0o777}
		}, rsDev + "/" + inPlaceTranscripts + "/", ReasonForeignChanged},
		"project-to-link": {func(m *memFS) { rsDel(m, rsDev); m.nodes[rsDev] = &memNode{mode: fs.ModeSymlink | 0o777} }, rsDev + "/", ReasonForeignChanged},
		"project-removed": {func(m *memFS) { rsDel(m, rsDev) }, rsDev + "/", ReasonForeignRemoved},
	} {
		m := rsWorld()
		rsForeign(m)
		s := rsSpy(m)
		p := rsPreparer(s, ApprovalScanLimits{})
		if err := p.preflight(nil, ""); err != nil {
			t.Fatal(err)
		}
		tc.change(m)
		var beneath []string
		s.afterLstat = func(path string) {
			if strings.HasPrefix(path, tc.under) {
				beneath = append(beneath, path)
			}
		}
		if err := p.preflight(nil, ""); err == nil || err.Error() != tc.want {
			t.Errorf("%s: %v", name, err)
		}
		if len(beneath) != 0 {
			t.Errorf("%s: looked up beneath it: %v", name, beneath)
		}
	}
}

// Code review A3 round 2, C1: when an admitted ledger entry and a foreign
// artifact change together, the ledger's ordinary failure (which comes
// first) never preempts A3.1's fixed reason: at the pre-launch baseline, in
// an approval inventory and in the residue check. Before a baseline exists
// there is no foreign verdict.
func TestForeignBaselineVerdictPrecedence(t *testing.T) {
	setup := func() (*memFS, *cursorPreparer) {
		m := rsWorld()
		rsForeign(m)
		p := rsPreparer(m, ApprovalScanLimits{})
		if w := rsCase(t, m, p, "cursor-setup", rsWS1, "in_place"); w.State != ResidueVerified {
			t.Fatal(w)
		}
		m.nodes[rsDir(rsWS1)+"/"+inPlaceLog].data += "grown"
		m.nodes[rsDev+"/"+inPlaceLog].data += "grown"
		return m, p
	}
	_, p := setup()
	if _, err := p.sessionBaseline(nil); err == nil || err.Error() != ReasonForeignChanged {
		t.Errorf("pre-launch baseline: %v", err)
	}
	_, p = setup()
	roots, _ := approvalRoots(p.fsys, rsWS2, rsHome)
	sc, _ := p.scanner()
	if got := p.scan(sc, roots, ""); got.why != ReasonForeignChanged || got.typed == nil {
		t.Errorf("inventory: %q", got.why)
	}
	_, p = setup()
	if w := p.checkResidue("cursor-default-1", rsWS2, caseResidueBaseline{children: map[string]fs.FileInfo{}}); deref(w.Reason) != ReasonForeignChanged {
		t.Errorf("residue check: %q", deref(w.Reason))
	}
	// No baseline yet: the ordinary failure stands.
	q := rsPreparer(rsWorld(), ApprovalScanLimits{})
	plain := errors.New("x")
	if q.foreignVerdict(plain, nil) != plain || q.foreign.initialized {
		t.Fatal("a verdict without a baseline")
	}
}

// Code review A3 round 2, W1: with the whole projects root removed, the
// root is the shallowest missing path, so nothing beneath it is looked up
// (repeated: the old depth order tied the root with its children and
// depended on map order).
func TestForeignBaselineRootRemovedOrder(t *testing.T) {
	for i := 0; i < 50; i++ {
		m := rsWorld()
		rsForeign(m)
		s := rsSpy(m)
		p := rsPreparer(s, ApprovalScanLimits{})
		if err := p.preflight(nil, ""); err != nil {
			t.Fatal(err)
		}
		rsDel(m, rsProjects)
		var beneath []string
		s.afterLstat = func(path string) {
			if strings.HasPrefix(path, rsProjects+"/") {
				beneath = append(beneath, path)
			}
		}
		if err := p.preflight(nil, ""); err == nil || err.Error() != ReasonForeignRemoved {
			t.Fatalf("iteration %d: %v", i, err)
		}
		if len(beneath) != 0 {
			t.Fatalf("iteration %d: looked up beneath the removed root: %v", i, beneath)
		}
	}
}

// Code review A3 round 3, W1: after the residue snapshot rejects the
// projects root's type, nothing lists the root (the fallback discovery of
// the session's locations needs a no-follow lookup proving an ordinary
// directory first); the foreign verdict is exact.
func TestForeignBaselineLinkedRootNotListed(t *testing.T) {
	m := rsWorld()
	rsForeign(m)
	s := rsSpy(m)
	p := rsPreparer(s, ApprovalScanLimits{})
	roots, _ := approvalRoots(s, rsWS1, rsHome)
	sc, _ := p.scanner()
	if got := p.scan(sc, roots, rsSlug(rsWS1)); !got.complete {
		t.Fatal(got.why)
	}
	b := rsEnable(t, m, p, rsWS1)
	listed := 0
	w := session(t, p, "cursor-setup", rsWS1, b, func() {
		rsInPlace(m, rsWS1)
		// The root becomes a link; its children stay visible through it (the
		// in-memory view lists by prefix, as following the link would).
		m.nodes[rsProjects] = &memNode{mode: fs.ModeSymlink | 0o777}
		s.onList = func(path string) {
			if path == rsProjects {
				listed++
			}
		}
	})
	if deref(w.Reason) != ReasonForeignChanged || listed != 0 || len(p.ledger.entries) != 0 {
		t.Fatalf("linked root: %q, %d listings", deref(w.Reason), listed)
	}
}
