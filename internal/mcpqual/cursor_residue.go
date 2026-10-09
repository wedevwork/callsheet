package mcpqual

// Run-attributed Cursor session residue (design decoder-enrollment B3,
// FP-25, as amended by A3). After each session Cursor Agent
// 2026.10.01-e373342 on linux/amd64 was observed to leave a Unix socket,
// worker.sock, in one of two layouts:
//
//   - hashed: alone in a second, hashed directory under ~/.cursor/projects
//     whose name is a truncated form of the workspace's project slug and a
//     seven-hex suffix (both observed names cut the slug at 57 characters;
//     a record of two observations, not a pinned algorithm, and no hash is
//     ever computed);
//   - in_place (A3): inside the case's own full-slug project directory S,
//     which project_scoped approval created with exactly mcp-approvals.json,
//     together with worker.log, repo.json, .workspace-trusted and one
//     agent-transcripts/<U>/<U>.jsonl transcript.
//
// The harness admits residue only into an in-memory, run-local ledger, and
// only when attributed to this run: absent immediately before this case's
// model launch (for in_place: every session entry absent and the approval
// directory holding only its unchanged approval file), present after its
// clean cleanup, with exactly one layout's complete predicate and nothing
// else new: an unknown new project entry, both layouts, a partial tree, an
// extra or mistyped entry, a changed approval or any new special file
// anywhere the monitored scan covers fails closed. Admitted entries are
// revalidated in full before and after every later scan of the same run
// (identities, metadata, approval bytes, exact membership); an entry whose
// whole directory disappeared is dropped, any other change fails. The
// session files are never opened, read, hashed or parsed (only their
// metadata is inspected, and later scans fingerprint them by metadata only
// through an identity-bound hook that never opens a file inside an
// admitted residue directory); the socket is never opened or connected
// to; nothing is followed, persisted, inherited by another invocation or
// deleted: a later invocation fails closed on leftovers until the owner,
// with Cursor stopped, inspects and removes them. Recorded evidence carries
// fixed labels, types, sizes and booleans only, never a raw slug, suffix,
// UUID, inode, timestamp or content.

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
)

// Residue evidence (design decoder-enrollment B3, Residue evidence; A3).
const (
	// WorkerResiduePolicy is the historical v1 rule (hashed directories,
	// sole socket); it stays strictly readable with its original meaning.
	WorkerResiduePolicy = "cursor-worker-residue-linux-amd64-2026.10.01-e373342-v1"
	// WorkerResiduePolicyV2 is A3's rule, written by every new run.
	WorkerResiduePolicyV2 = "cursor-worker-residue-linux-amd64-2026.10.01-e373342-v2"
	// Residue states.
	ResidueVerified   = "verified"
	ResidueUnverified = "unverified"
	ResidueNotChecked = "not_checked"
	// ResidueOrigin is the only origin of an admitted entry.
	ResidueOrigin = "appeared_during_session"
	// Residue layouts (A3).
	ResidueLayoutHashed  = "hashed"
	ResidueLayoutInPlace = "in_place"
	// Residue item types.
	ResidueItemDirectory = "directory"
	ResidueItemRegular   = "regular"
	ResidueItemSocket    = "socket"
	// cursorWorkerSocket is the residue socket's name.
	cursorWorkerSocket = "worker.sock"
	// MaxResidueParentSlug is the longest workspace-parent slug P whose
	// observed 57-character residue prefix still keeps two characters of the
	// case basename (r0.13 post-final clarification DW1: 54 + 1 + 2 = 57);
	// A3 applies it only to an observed hashed candidate.
	MaxResidueParentSlug = 54
	// ReasonResidueParentSlug refuses a hashed candidate of a longer parent
	// slug, verbatim (r0.14 post-final clarification DW1).
	ReasonResidueParentSlug = ReasonCursorScopeUnverified + ": cursor residue parent slug exceeds 54 characters"
	// MaxResidueEntries bounds one confirmation's admitted residue (at most
	// one per session, three sessions).
	MaxResidueEntries = 3
)

// The exact in-place tree (A3): names and size limits.
const (
	inPlaceApproval    = cursorApprovalsFile
	inPlaceLog         = "worker.log"
	inPlaceRepo        = "repo.json"
	inPlaceTrusted     = ".workspace-trusted"
	inPlaceTranscripts = "agent-transcripts"
	// transcriptLabel replaces the transcript UUID in recorded items.
	transcriptLabel      = "<transcript-id>"
	maxInPlaceLog        = 1 << 20
	maxInPlaceSmall      = 4 << 10
	maxInPlaceTranscript = 1 << 20
	maxInPlaceSession    = 2<<20 + 8<<10
)

// transcriptIDRe is a lowercase canonical UUID spelling.
var transcriptIDRe = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

// sessionArtifactNames are the reserved session artifact names the
// projects preflight refuses outside the validated candidate and ledger.
var sessionArtifactNames = []string{cursorWorkerSocket, inPlaceLog, inPlaceRepo, inPlaceTrusted, inPlaceTranscripts}

// inPlaceItems are the exact recorded in-place items, lexically sorted.
var inPlaceItems = []struct{ path, typ string }{
	{inPlaceTrusted, ResidueItemRegular},
	{inPlaceTranscripts, ResidueItemDirectory},
	{inPlaceTranscripts + "/" + transcriptLabel, ResidueItemDirectory},
	{inPlaceTranscripts + "/" + transcriptLabel + "/" + transcriptLabel + ".jsonl", ResidueItemRegular},
	{inPlaceApproval, ResidueItemRegular},
	{inPlaceRepo, ResidueItemRegular},
	{inPlaceLog, ResidueItemRegular},
	{cursorWorkerSocket, ResidueItemSocket},
}

// inPlaceLimit is the size limit of a recorded in-place regular item.
func inPlaceLimit(item string) int64 {
	switch item {
	case inPlaceLog:
		return maxInPlaceLog
	case inPlaceRepo, inPlaceTrusted:
		return maxInPlaceSmall
	case inPlaceApproval:
		return DefaultApprovalScanLimits().FileBytes
	}
	return maxInPlaceTranscript
}

// WorkerResidue is the residue evidence object, shared by
// CaptureClient.worker_residue and CaseReport.cursor_preparation.residue:
// verified with a null reason (an empty list then means the complete check
// found none, never that it was skipped), or unverified / not_checked
// with a fixed safe reason.
type WorkerResidue struct {
	Policy  string               `json:"policy"`
	State   string               `json:"state"`
	Reason  *string              `json:"reason"`
	Entries []WorkerResidueEntry `json:"entries"`
}

// WorkerResidueEntry is one admitted residue: its owning case, its
// normalized labeled socket path and origin, and its checks. A v1 entry
// (no layout) has the historical prefix/suffix/sole-entry flags; a v2 entry
// (A3) has its layout, layout_verified, approval_unchanged (null for
// hashed) and the exact items inside the residue directory. Each version
// serializes exactly its own members (MarshalJSON); the version-specific
// fields are omitempty only for the generic field walk, the strict
// per-version member sets being checkResidueRaw's.
type WorkerResidueEntry struct {
	CaseID string `json:"case_id"`
	Path   string `json:"path"`
	Origin string `json:"origin"`
	// v1 only.
	PrefixVerified    bool `json:"prefix_verified,omitempty"`
	SuffixVerified    bool `json:"suffix_verified,omitempty"`
	SoleEntryVerified bool `json:"sole_entry_verified,omitempty"`
	// v2 only.
	Layout            string        `json:"layout,omitempty"`
	LayoutVerified    bool          `json:"layout_verified,omitempty"`
	ApprovalUnchanged *bool         `json:"approval_unchanged,omitempty"`
	Items             []ResidueItem `json:"items,omitempty"`
	// Both versions.
	SocketTypeVerified bool `json:"socket_type_verified"`
	IdentityVerified   bool `json:"identity_verified"`
}

// ResidueItem is one entry inside a residue directory: its fixed relative
// name (the transcript UUID labeled), its type and its Lstat size (zero for
// directories and sockets).
type ResidueItem struct {
	Path string `json:"path"`
	Type string `json:"type"`
	Size int64  `json:"size"`
}

// MarshalJSON writes exactly the entry's version members (v2 when it has a
// layout, otherwise the historical v1 members).
func (e WorkerResidueEntry) MarshalJSON() ([]byte, error) {
	if e.Layout == "" {
		return json.Marshal(struct {
			CaseID             string `json:"case_id"`
			Path               string `json:"path"`
			Origin             string `json:"origin"`
			PrefixVerified     bool   `json:"prefix_verified"`
			SuffixVerified     bool   `json:"suffix_verified"`
			SocketTypeVerified bool   `json:"socket_type_verified"`
			SoleEntryVerified  bool   `json:"sole_entry_verified"`
			IdentityVerified   bool   `json:"identity_verified"`
		}{e.CaseID, e.Path, e.Origin, e.PrefixVerified, e.SuffixVerified, e.SocketTypeVerified, e.SoleEntryVerified, e.IdentityVerified})
	}
	items := e.Items
	if items == nil {
		items = []ResidueItem{}
	}
	return json.Marshal(struct {
		CaseID             string        `json:"case_id"`
		Path               string        `json:"path"`
		Origin             string        `json:"origin"`
		Layout             string        `json:"layout"`
		IdentityVerified   bool          `json:"identity_verified"`
		SocketTypeVerified bool          `json:"socket_type_verified"`
		LayoutVerified     bool          `json:"layout_verified"`
		ApprovalUnchanged  *bool         `json:"approval_unchanged"`
		Items              []ResidueItem `json:"items"`
	}{e.CaseID, e.Path, e.Origin, e.Layout, e.IdentityVerified, e.SocketTypeVerified, e.LayoutVerified, e.ApprovalUnchanged, items})
}

// The strict member sets.
var (
	residueMembers        = []string{"policy", "state", "reason", "entries"}
	residueEntryMembers   = []string{"case_id", "path", "origin", "prefix_verified", "suffix_verified", "socket_type_verified", "sole_entry_verified", "identity_verified"}
	residueEntryMembersV2 = []string{"case_id", "path", "origin", "layout", "identity_verified", "socket_type_verified", "layout_verified", "approval_unchanged", "items"}
	residueItemMembers    = []string{"path", "type", "size"}
)

// residuePathRe is a normalized residue path.
var residuePathRe = regexp.MustCompile(`^<home>/\.cursor/projects/<case-residue-([1-9][0-9]*)>/worker\.sock$`)

// residuePath is the normalized path of run-local residue n.
func residuePath(n int) string {
	return labelHomeProjects + "/<case-residue-" + strconv.Itoa(n) + ">/" + cursorWorkerSocket
}

// notCheckedResidue is the (v2) residue record of a check that did not run.
func notCheckedResidue(reason string) WorkerResidue {
	return WorkerResidue{Policy: WorkerResiduePolicyV2, State: ResidueNotChecked, Reason: sptr(reason), Entries: []WorkerResidueEntry{}}
}

// validate checks a residue record: a known policy (v1 with its original
// meaning, or v2), a known state with its reason pairing, unique normalized
// paths, every entry from a session, one entry per owning case and at most
// limit entries, each owning case accepted by ownedBy (an already run
// case), each entry of exactly its version, and all checks true when
// verified.
func (w *WorkerResidue) validate(ownedBy func(caseID string) bool, limit int) error {
	v2 := w.Policy == WorkerResiduePolicyV2
	switch {
	case w.Policy != WorkerResiduePolicy && !v2:
		return fmt.Errorf("residue: policy %q, want %q or %q", w.Policy, WorkerResiduePolicy, WorkerResiduePolicyV2)
	case w.State != ResidueVerified && w.State != ResidueUnverified && w.State != ResidueNotChecked:
		return fmt.Errorf("residue: state %q", w.State)
	case (w.State == ResidueVerified) != (w.Reason == nil) || w.Reason != nil && *w.Reason == "":
		return errors.New("residue: a reason exactly when not verified")
	case w.Entries == nil:
		return errors.New("residue: entries must be a list")
	case len(w.Entries) > limit:
		return fmt.Errorf("residue: %d entries, at most %d", len(w.Entries), limit)
	case w.State == ResidueNotChecked && len(w.Entries) > 0:
		return errors.New("residue: not_checked with entries")
	}
	paths, owners := map[string]bool{}, map[string]bool{}
	for _, e := range w.Entries {
		switch {
		case !residuePathRe.MatchString(e.Path) || paths[e.Path]:
			return fmt.Errorf("residue: path %q is not a unique normalized residue path", e.Path)
		case e.Origin != ResidueOrigin:
			return fmt.Errorf("residue: origin %q", e.Origin)
		case owners[e.CaseID] || !ownedBy(e.CaseID):
			return fmt.Errorf("residue: case %q owns more than one entry or has not run", e.CaseID)
		}
		var err error
		if v2 {
			err = e.validateV2(w.State == ResidueVerified)
		} else {
			err = e.validateV1(w.State == ResidueVerified)
		}
		if err != nil {
			return err
		}
		paths[e.Path], owners[e.CaseID] = true, true
	}
	return nil
}

// validateV1 checks a historical entry: no v2 member, all checks true when
// verified (it never certifies an in-place tree).
func (e WorkerResidueEntry) validateV1(verified bool) error {
	switch {
	case e.Layout != "" || e.LayoutVerified || e.ApprovalUnchanged != nil || e.Items != nil:
		return errors.New("residue: a v1 entry with v2 members")
	case verified && !(e.PrefixVerified && e.SuffixVerified && e.SocketTypeVerified && e.SoleEntryVerified && e.IdentityVerified):
		return errors.New("residue: verified with a failed check")
	}
	return nil
}

// validateV2 checks an A3 entry: its layout, no v1 member, the flags (all
// true and approval_unchanged true for in_place when verified;
// approval_unchanged null exactly for hashed) and the exact items of its
// layout in order, each of its type within its size limit (the four
// session limits sum to exactly the in-place session total).
func (e WorkerResidueEntry) validateV2(verified bool) error {
	switch {
	case e.Layout != ResidueLayoutHashed && e.Layout != ResidueLayoutInPlace:
		return fmt.Errorf("residue: layout %q", e.Layout)
	case e.PrefixVerified || e.SuffixVerified || e.SoleEntryVerified:
		return errors.New("residue: a v2 entry with v1 members")
	case (e.Layout == ResidueLayoutHashed) != (e.ApprovalUnchanged == nil):
		return errors.New("residue: approval_unchanged is null exactly for the hashed layout")
	case verified && !(e.IdentityVerified && e.SocketTypeVerified && e.LayoutVerified && (e.ApprovalUnchanged == nil || *e.ApprovalUnchanged)):
		return errors.New("residue: verified with a failed check")
	}
	want := []struct{ path, typ string }{{cursorWorkerSocket, ResidueItemSocket}}
	if e.Layout == ResidueLayoutInPlace {
		want = inPlaceItems
	}
	if len(e.Items) != len(want) {
		return fmt.Errorf("residue: %d items, want the %s layout's %d", len(e.Items), e.Layout, len(want))
	}
	for i, it := range e.Items {
		switch {
		case it.Path != want[i].path || it.Type != want[i].typ:
			return fmt.Errorf("residue: item %d is %s %q, want %s %q", i, it.Type, it.Path, want[i].typ, want[i].path)
		case it.Size < 0 || it.Type != ResidueItemRegular && it.Size != 0:
			return fmt.Errorf("residue: item %q has size %d", it.Path, it.Size)
		case it.Type == ResidueItemRegular && it.Size > inPlaceLimit(it.Path):
			return fmt.Errorf("residue: item %q exceeds its size limit", it.Path)
		}
	}
	return nil
}

// cursorApprovalBaseline is a case's eligible in-place provenance (A3,
// r0.14 DW2): the full slug S of a project directory that was absent before
// this case's enable and that project_scoped approval created holding
// exactly mcp-approvals.json, the directory's and the file's identities and
// the validated approval bytes and hash. It stays in memory, separate from
// the ledger: it is never admitted residue and exempts nothing.
type cursorApprovalBaseline struct {
	slug      string
	dir, file fs.FileInfo
	data      []byte
	sum       [sha256.Size]byte
}

// caseResidueBaseline is a case's state immediately before its model
// launch: the projects root's direct children and the eligible approval
// baseline (nil without project_scoped approval).
type caseResidueBaseline struct {
	children map[string]fs.FileInfo
	approval *cursorApprovalBaseline
}

// residueEntry is one ledger entry: its owning case, its layout, its raw
// directory name (in memory only), the directory's and the socket's
// no-follow identities, its stable run-local number and, for in_place, the
// approval baseline, every entry's identity and metadata and the items.
type residueEntry struct {
	caseID, name, layout string
	dir, sock            fs.FileInfo
	n                    int
	approval             *cursorApprovalBaseline
	tree                 map[string]fs.FileInfo
	items                []ResidueItem
}

// public is the entry's v2 evidence.
func (e *residueEntry) public() WorkerResidueEntry {
	out := WorkerResidueEntry{CaseID: e.caseID, Path: residuePath(e.n), Origin: ResidueOrigin, Layout: e.layout, IdentityVerified: true, SocketTypeVerified: true,
		LayoutVerified: true, Items: append([]ResidueItem(nil), e.items...)}
	if e.layout == ResidueLayoutInPlace {
		out.ApprovalUnchanged = bptr(true)
	}
	return out
}

// residueLedger is the run-local ledger (never persisted).
type residueLedger struct {
	entries []*residueEntry
	next    int
}

// record is the public form of the ledger's entries, in admission order.
func (l *residueLedger) record() []WorkerResidueEntry {
	out := []WorkerResidueEntry{}
	for _, e := range l.entries {
		out = append(out, e.public())
	}
	return out
}

// projectsRoot is the owner's Cursor projects directory.
func (p *cursorPreparer) projectsRoot() string { return filepath.Join(p.home, ".cursor", "projects") }

// soleSocket checks that directory dir holds exactly worker.sock and that a
// no-follow lookup of it is a socket; it returns the socket's identity and
// which checks held.
func (p *cursorPreparer) soleSocket(dir string) (sock fs.FileInfo, sole, socket bool, err error) {
	children, err := p.fsys.ReadDir(dir)
	if err != nil {
		return nil, false, false, errors.New("a residue directory cannot be listed")
	}
	if len(children) > p.limits.Entries {
		return nil, false, false, errBound
	}
	sole = len(children) == 1 && children[0].Name() == cursorWorkerSocket
	if !sole {
		return nil, false, false, nil
	}
	info, err := p.fsys.Lstat(filepath.Join(dir, cursorWorkerSocket))
	if err != nil {
		return nil, true, false, errors.New("a residue socket cannot be looked up")
	}
	socket, err = residueSocket(info)
	if err != nil {
		return nil, true, false, err
	}
	return info, true, socket, nil
}

// walkTree is the bounded, no-follow, metadata-only inventory of directory
// dir: every descendant's no-follow lookup by slash-separated relative
// path, directories descended, nothing opened. Every entry counts against
// the entry cap; an unlistable directory or a failed lookup fails.
func (p *cursorPreparer) walkTree(dir string) (map[string]fs.FileInfo, error) {
	out := map[string]fs.FileInfo{}
	var walk func(abs, rel string) error
	walk = func(abs, rel string) error {
		children, err := p.fsys.ReadDir(abs)
		if err != nil {
			return errors.New("a residue directory cannot be listed")
		}
		for _, c := range children {
			if len(out)+1 > p.limits.Entries {
				return errBound
			}
			cabs, crel := filepath.Join(abs, c.Name()), c.Name()
			if rel != "" {
				crel = rel + "/" + c.Name()
			}
			info, err := p.fsys.Lstat(cabs)
			if err != nil {
				return errors.New("a residue entry cannot be looked up")
			}
			out[crel] = info
			if info.IsDir() {
				if err := walk(cabs, crel); err != nil {
					return err
				}
			}
		}
		return nil
	}
	return out, walk(dir, "")
}

// approvalOnly reports a tree that is exactly the baseline's approval file
// (the same identity and metadata).
func approvalOnly(tree map[string]fs.FileInfo, b *cursorApprovalBaseline) bool {
	f, ok := tree[inPlaceApproval]
	return len(tree) == 1 && ok && sameMeta(b.file, f)
}

// sameMeta is the same file with the same type, mode, size and modification
// time.
func sameMeta(a, b fs.FileInfo) bool {
	return sameFile(a, b) && a.Mode() == b.Mode() && a.Size() == b.Size() && a.ModTime().Equal(b.ModTime())
}

// sameDir is the same directory with the same type and full mode (code
// review A3 round 1, C2: sameFile alone compares only the device and inode
// on a real filesystem). Its size and modification time are never compared:
// a directory's legitimate additions change them.
func sameDir(a, b fs.FileInfo) bool {
	return a.IsDir() && b.IsDir() && sameFile(a, b) && a.Mode() == b.Mode()
}

// approvalUnchanged rereads the baseline's approval file bounded and
// without following a link: the same identity and metadata and identical
// bytes (the one content read A3 allows; the bytes stay private).
func (p *cursorPreparer) approvalUnchanged(b *cursorApprovalBaseline) bool {
	sc := &approvalScanner{fs: p.fsys, lim: p.limits}
	info, data, why := sc.readApprovalFile(filepath.Join(p.projectsRoot(), b.slug, inPlaceApproval))
	return why == "" && sameMeta(b.file, info) && bytes.Equal(data, b.data) && sha256.Sum256(data) == b.sum
}

// recheckApproval is the pre-launch check of an eligible baseline: the same
// directory still holding only the same, byte-identical approval file.
func (p *cursorPreparer) recheckApproval(b *cursorApprovalBaseline) error {
	dir := filepath.Join(p.projectsRoot(), b.slug)
	info, err := p.fsys.Lstat(dir)
	if err != nil || !sameDir(b.dir, info) {
		return errors.New("the case's project approval directory changed")
	}
	tree, err := p.walkTree(dir)
	if err != nil || !approvalOnly(tree, b) || !p.approvalUnchanged(b) {
		return errors.New("the case's project approval directory no longer holds only its unchanged approval")
	}
	return nil
}

// inPlaceTree checks an approval directory's tree against A3's exact
// in-place layout with the unchanged approval file: every required entry of
// its type and size, nothing else, the transcript directory and file named
// by one identical lowercase UUID, the bounds. It returns the recorded
// items or why the tree is not the layout (a fixed text, never a name).
func (p *cursorPreparer) inPlaceTree(tree map[string]fs.FileInfo, b *cursorApprovalBaseline) ([]ResidueItem, string) {
	var uuid string
	for rel := range tree {
		if d, rest, ok := strings.Cut(rel, "/"); ok && d == inPlaceTranscripts && !strings.Contains(rest, "/") {
			if uuid != "" {
				return nil, "more than one transcript directory"
			}
			uuid = rest
		}
	}
	if !transcriptIDRe.MatchString(uuid) {
		return nil, "no single lowercase UUID transcript directory"
	}
	actual := map[string]string{}
	for _, it := range inPlaceItems {
		actual[it.path] = strings.ReplaceAll(it.path, transcriptLabel, uuid)
	}
	if len(tree) != len(inPlaceItems) {
		return nil, "the tree is not exactly the in-place layout's entries"
	}
	var items []ResidueItem
	var session, total int64
	for _, it := range inPlaceItems {
		info, ok := tree[actual[it.path]]
		if !ok {
			return nil, "a required in-place entry is missing"
		}
		item := ResidueItem{Path: it.path, Type: it.typ}
		switch it.typ {
		case ResidueItemDirectory:
			if !info.IsDir() {
				return nil, "an in-place directory entry is not an ordinary directory"
			}
		case ResidueItemSocket:
			socket, err := residueSocket(info)
			if err != nil || !socket || info.Size() != 0 {
				return nil, "worker.sock is not an empty Unix socket"
			}
		default:
			limit := min(inPlaceLimit(it.path), p.limits.FileBytes)
			if !info.Mode().IsRegular() || info.Size() < 0 || info.Size() > limit {
				return nil, "an in-place file is not a regular file within its size limit"
			}
			item.Size = info.Size()
			total += info.Size()
			if it.path != inPlaceApproval {
				session += info.Size()
			}
		}
		items = append(items, item)
	}
	switch {
	case session > maxInPlaceSession || total > p.limits.Bytes:
		return nil, "the in-place files exceed their total size limit"
	case !sameMeta(b.file, tree[inPlaceApproval]):
		return nil, "the approval file was replaced or changed"
	}
	return items, ""
}

// recheckEntry verifies one ledger entry or candidate in full against its
// recorded identities: hashed, the same directory holding only the same
// socket; in_place, the same directory, the identical tree (every entry's
// identity, mode, size and modification time, no extra or missing child)
// and the byte-identical approval.
func (p *cursorPreparer) recheckEntry(e *residueEntry) error {
	dir := filepath.Join(p.projectsRoot(), e.name)
	info, err := p.fsys.Lstat(dir)
	if err != nil || !sameDir(e.dir, info) {
		return errors.New("an admitted residue directory changed, was replaced or partly disappeared")
	}
	if e.layout == ResidueLayoutHashed {
		sock, sole, socket, err := p.soleSocket(dir)
		if err != nil || !sole || !socket || !sameFile(e.sock, sock) {
			return errors.New("an admitted residue socket changed, was replaced, disappeared or gained a sibling")
		}
		return nil
	}
	tree, err := p.walkTree(dir)
	if err != nil || len(tree) != len(e.tree) {
		return errors.New("an admitted in-place residue tree gained, lost or hid an entry")
	}
	for rel, was := range e.tree {
		now, ok := tree[rel]
		switch {
		case !ok:
			return errors.New("an admitted in-place residue entry changed or was replaced")
		case was.IsDir():
			if !sameDir(was, now) {
				return errors.New("an admitted in-place residue entry changed or was replaced")
			}
		case !sameMeta(was, now):
			return errors.New("an admitted in-place residue entry changed or was replaced")
		}
	}
	if !p.approvalUnchanged(e.approval) {
		return errors.New("an admitted in-place residue's approval changed")
	}
	return nil
}

// gone reports an entry whose whole directory disappeared: the directory
// and every recorded entry in it absent.
func (p *cursorPreparer) gone(e *residueEntry) bool {
	dir := filepath.Join(p.projectsRoot(), e.name)
	paths := []string{dir, filepath.Join(dir, cursorWorkerSocket)}
	for rel := range e.tree {
		paths = append(paths, filepath.Join(dir, filepath.FromSlash(rel)))
	}
	for _, path := range paths {
		if _, err := p.fsys.Lstat(path); !errors.Is(err, fs.ErrNotExist) {
			return false
		}
	}
	return true
}

// revalidate checks every ledger entry before and after a scan. An entry
// whose whole directory disappeared is dropped; a partial disappearance, a
// replacement, an added entry or any metadata or approval change fails.
func (p *cursorPreparer) revalidate() error {
	kept := p.ledger.entries[:0:0]
	for _, e := range p.ledger.entries {
		if p.gone(e) {
			continue
		}
		if err := p.recheckEntry(e); err != nil {
			return err
		}
		kept = append(kept, e)
	}
	p.ledger.entries = kept
	return nil
}

// socketBound reports whether path, with the no-follow lookup info, is
// entry e's socket with its admitted identity.
func (e *residueEntry) socketBound(root, path string, info fs.FileInfo) bool {
	socket, err := residueSocket(info)
	return path == filepath.Join(root, e.name, cursorWorkerSocket) && err == nil && socket && sameFile(e.sock, info)
}

// reserve reports whether a regular file at path is inside entry e's
// directory other than an in-place approval file (reserved: never opened)
// and, if so, whether it is one of e's in-place session files with its
// admitted identity and metadata (bound: recorded by metadata only).
func (e *residueEntry) reserve(root, path string, info fs.FileInfo) (reserved, bound bool) {
	rel, ok := strings.CutPrefix(path, filepath.Join(root, e.name)+string(filepath.Separator))
	if !ok {
		return false, false
	}
	rel = filepath.ToSlash(rel)
	if e.layout == ResidueLayoutInPlace && rel == inPlaceApproval {
		return false, false
	}
	was, ok := e.tree[rel]
	return true, ok && was.Mode().IsRegular() && sameMeta(was, info)
}

// exempt reports a ledger socket at path with its admitted identity, or a
// foreign baseline socket with its recorded identity and metadata (A3.1):
// the only special files a scan of this run may pass over (never opened or
// connected to).
func (p *cursorPreparer) exempt(path string, info fs.FileInfo) bool {
	for _, e := range p.ledger.entries {
		if e.socketBound(p.projectsRoot(), path, info) {
			return true
		}
	}
	if rel, ok := p.projectsRel(path); ok {
		if n := p.foreign.nodes[rel]; n != nil && n.kind != foreignAncestor && n.info.Mode().Type() == fs.ModeSocket {
			return sameMeta(n.info, info)
		}
	}
	return false
}

// metaOnly is the scanner's hook (A3 and A3.1, scanner privacy
// integration): a regular file inside an admitted residue directory, at a
// foreign baseline path or at a reserved session path (a reserved name or
// inside a reserved directory, recorded or not) is never opened. Only a
// bound in-place session file or an unchanged foreign baseline file is
// recorded (by metadata); a reserved but unbound file fails the inventory,
// with A3.1's fixed reason for a foreign one.
func (p *cursorPreparer) metaOnly(path string, info fs.FileInfo) (reserved, bound bool, fail error) {
	for _, e := range p.ledger.entries {
		if reserved, bound := e.reserve(p.projectsRoot(), path, info); reserved {
			return true, bound, nil
		}
	}
	rel, ok := p.projectsRel(path)
	if !ok {
		return false, false, nil
	}
	if n := p.foreign.nodes[rel]; n != nil {
		if n.kind != foreignAncestor && n.info.Mode().IsRegular() && sameMeta(n.info, info) {
			return true, true, nil
		}
		return true, false, foreignFail(ReasonForeignChanged)
	}
	if reservedRel(rel) {
		return true, false, foreignFail(ReasonForeignNew)
	}
	return false, false, nil
}

// projectsRel is path's slash-separated path relative to the projects root
// ("" for the root itself), or false outside it.
func (p *cursorPreparer) projectsRel(path string) (string, bool) {
	root := p.projectsRoot()
	if path == root {
		return "", true
	}
	rel, ok := strings.CutPrefix(path, root+string(filepath.Separator))
	return filepath.ToSlash(rel), ok
}

// reservedRel reports a projects-relative path with a reserved session
// artifact name in any component (a reserved entry or a descendant of a
// reserved directory).
func reservedRel(rel string) bool {
	for _, c := range strings.Split(rel, "/") {
		if slices.Contains(sessionArtifactNames, c) {
			return true
		}
	}
	return false
}

// preflight is the bounded metadata-only projects preflight (A3, as
// amended by A3.1), before and after every approval-root content scan:
// with own set (this case's full slug S, at its own pre-enable preflight),
// the case-ownership checks first; then, at the preparer's first preflight,
// the foreign baseline's one initialization, and at every later one its
// complete revalidation. Outside the validated ledger entries and the
// candidate (cand, when set), nothing is opened, read or connected to. Its
// failures are A3.1's fixed reasons (a *foreignError). also names further
// top-level directories the walk passes over (the session's own candidate
// locations while its residue is being classified).
func (p *cursorPreparer) preflight(cand *residueEntry, own string, also ...string) error {
	if own != "" {
		if err := p.ownership(own); err != nil {
			return err
		}
	}
	skip := map[string]bool{}
	for _, e := range p.ledger.entries {
		skip[e.name] = true
	}
	if cand != nil {
		skip[cand.name] = true
	}
	for _, name := range also {
		skip[name] = true
	}
	if !p.foreign.initialized {
		p.foreign.initialized = true
		nodes, err := p.stableForeign(skip)
		if err != nil {
			p.foreign.err = err
			return err
		}
		p.foreign.nodes = nodes
		return nil
	}
	if p.foreign.err != nil {
		return p.foreign.err
	}
	return p.revalidateForeign(skip)
}

// sessionBaseline is case residue baseline immediately before the model
// launch: the ledger revalidated, the projects root's direct children, the
// foreign baseline revalidated (A3.1) and,
// for an eligible project_scoped approval, its pre-launch recheck (the same
// directory still holding only the same, byte-identical approval file).
func (p *cursorPreparer) sessionBaseline(approval *cursorApprovalBaseline) (caseResidueBaseline, error) {
	children, err := p.residueSnapshot()
	if err != nil {
		return caseResidueBaseline{}, p.foreignVerdict(err, nil)
	}
	if err := p.preflight(nil, ""); err != nil {
		return caseResidueBaseline{}, err
	}
	if approval != nil {
		if err := p.recheckApproval(approval); err != nil {
			return caseResidueBaseline{}, p.foreignVerdict(err, nil)
		}
	}
	return caseResidueBaseline{children: children, approval: approval}, nil
}

// scan is one approval-root inventory with A3's and A3.1's guards: the
// ledger revalidated in full and the metadata-only projects preflight
// (with own, this case's ownership checks) before it, both again after it.
// A failed guard is an incomplete inventory whose cause is the guard's
// (nothing of the scan is used; an A3.1 failure is carried typed, and a
// failed guard after the scan takes precedence over the scan's own
// failure).
func (p *cursorPreparer) scan(sc *approvalScanner, roots []scanRoot, own string) *snapshot {
	failed := func(err error) *snapshot {
		f := &snapshot{entries: map[string]scanEntry{}, why: err.Error()}
		if isFixed(err) {
			f.typed = err
		}
		return f
	}
	if err := p.revalidate(); err != nil {
		return failed(p.foreignVerdict(err, nil))
	}
	if err := p.preflight(nil, own); err != nil {
		return failed(err)
	}
	s := sc.snapshot(roots)
	if err := p.revalidate(); err != nil {
		return failed(p.foreignVerdict(err, nil))
	}
	if err := p.preflight(nil, ""); err != nil {
		return failed(err)
	}
	return s
}

// classifyResidue returns the case's single candidate (nil for no residue)
// or why the observed residue is not admissible, from the projects root's
// children before and after the session and the eligible approval
// baseline. verbatim is set when the reason is the parent-slug cap's
// complete reason (r0.14 DW1), which is never wrapped.
func (p *cursorPreparer) classifyResidue(caseID, ws string, pre caseResidueBaseline, post map[string]fs.FileInfo) (cand *residueEntry, why string, verbatim bool) {
	root := p.projectsRoot()
	var added []string
	names := make([]string, 0, len(post))
	for name := range post {
		names = append(names, name)
	}
	slices.Sort(names)
	for _, name := range names {
		if before, ok := pre.children[name]; ok {
			if !sameFile(before, post[name]) {
				return nil, "check failed: a Cursor project entry was replaced during the session", false
			}
			continue
		}
		added = append(added, name)
	}
	// The eligible approval directory: approval only, or the in-place tree.
	var inPlace *residueEntry
	if b := pre.approval; b != nil {
		dir := filepath.Join(root, b.slug)
		info, ok := post[b.slug]
		if !ok || !sameDir(b.dir, info) {
			return nil, "check failed: the case's project approval directory changed", false
		}
		tree, err := p.walkTree(dir)
		if err != nil {
			return nil, "check failed: " + err.Error(), false
		}
		if !approvalOnly(tree, b) {
			items, why := p.inPlaceTree(tree, b)
			if why != "" {
				return nil, "is unverifiable: the case's project directory is not the exact in-place layout (" + why + ")", false
			}
			inPlace = &residueEntry{caseID: caseID, name: b.slug, layout: ResidueLayoutInPlace, dir: info, sock: tree[cursorWorkerSocket], approval: b,
				tree: tree, items: items}
		}
		if !p.approvalUnchanged(b) {
			return nil, "is unverifiable: the case's approval file changed", false
		}
	}
	switch {
	case len(added) > 1:
		return nil, "is ambiguous: more than one new Cursor project entry appeared in one session", false
	case len(added) == 1 && inPlace != nil:
		return nil, "is ambiguous: both residue layouts appeared in one session", false
	case inPlace != nil:
		return inPlace, "", false
	case len(added) == 0:
		return nil, "", false
	}
	// The hashed candidate: a new ordinary directory holding only worker.sock,
	// the parent-slug cap (before the k predicate, verbatim), then its name.
	name := added[0]
	dir := filepath.Join(root, name)
	info := post[name]
	if !info.IsDir() {
		return nil, "is unverifiable: a new Cursor project entry is not a residue directory", false
	}
	sock, sole, socket, err := p.soleSocket(dir)
	switch {
	case err != nil:
		return nil, "check failed: " + err.Error(), false
	case !sole || !socket:
		return nil, "is unverifiable: a new Cursor project directory is not a hashed residue directory holding only worker.sock", false
	}
	parent, base, perr := p.parentSlug(ws)
	if perr != nil {
		return nil, "is unverifiable: the workspace has no verified project slug", false
	}
	if len(parent) > MaxResidueParentSlug {
		return nil, ReasonResidueParentSlug, true
	}
	if prefix, suffix := residueName(name, parent, base); !prefix || !suffix {
		return nil, "is unverifiable: a new candidate does not have the attributed hashed name", false
	}
	return &residueEntry{caseID: caseID, name: name, layout: ResidueLayoutHashed, dir: info, sock: sock, items: []ResidueItem{{Path: cursorWorkerSocket, Type: ResidueItemSocket}}}, "", false
}

// foreignVerdict is err, or A3.1's fixed reason when the foreign baseline's
// revalidation (passing over cand and also) fails now: an ordinary failure
// never preempts the fixed reason (code review A3 round 2, C1). Before the
// baseline exists there is no foreign verdict to give.
func (p *cursorPreparer) foreignVerdict(err error, cand *residueEntry, also ...string) error {
	if err == nil || isFixed(err) || !p.foreign.initialized {
		return err
	}
	if ferr := p.preflight(cand, "", also...); isFixed(ferr) {
		return ferr
	}
	return err
}

// sessionLocations are the session's own candidate locations as far as a
// best-effort listing of the projects root shows them (for a foreign
// verdict after the residue snapshot itself failed).
func (p *cursorPreparer) sessionLocations(pre caseResidueBaseline) []string {
	post := map[string]fs.FileInfo{}
	// Only an ordinary projects directory, by a no-follow lookup, is listed
	// (code review A3 round 3, W1: never through a link or other type).
	if info, err := p.fsys.Lstat(p.projectsRoot()); err != nil || !info.IsDir() {
		return p.candidateLocations(pre, post)
	}
	if children, err := p.fsys.ReadDir(p.projectsRoot()); err == nil && len(children) <= p.limits.Entries {
		for _, c := range children {
			if info, err := p.fsys.Lstat(filepath.Join(p.projectsRoot(), c.Name())); err == nil {
				post[c.Name()] = info
			}
		}
	}
	return p.candidateLocations(pre, post)
}

// candidateLocations are the session's own candidate locations under the
// projects root: the eligible approval directory (the in-place layout's
// place) and every new directory holding exactly worker.sock (the hashed
// layout's shape, whatever its name). Everything else, a new entry with
// session artifacts included, stays subject to the foreign baseline.
func (p *cursorPreparer) candidateLocations(pre caseResidueBaseline, post map[string]fs.FileInfo) []string {
	var out []string
	if pre.approval != nil {
		out = append(out, pre.approval.slug)
	}
	for name, info := range post {
		if _, existed := pre.children[name]; existed || !info.IsDir() {
			continue
		}
		if _, sole, _, err := p.soleSocket(filepath.Join(p.projectsRoot(), name)); err == nil && sole {
			out = append(out, name)
		}
	}
	return out
}

// residueSnapshot revalidates the ledger, then takes the bounded no-follow
// inventory of the projects root's direct children (names and
// identities). An absent root has none; a root that is a symbolic link or
// not a directory, an unlistable root, a child that cannot be looked up or
// a root replaced while listed fails closed.
func (p *cursorPreparer) residueSnapshot() (map[string]fs.FileInfo, error) {
	if err := p.revalidate(); err != nil {
		return nil, err
	}
	root := p.projectsRoot()
	pre, err := p.fsys.Lstat(root)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return map[string]fs.FileInfo{}, nil
	case err != nil || !pre.IsDir():
		return nil, errors.New("the Cursor projects directory is not an ordinary directory")
	}
	children, err := p.fsys.ReadDir(root)
	if err != nil {
		return nil, errors.New("the Cursor projects directory cannot be listed")
	}
	if len(children) > p.limits.Entries {
		return nil, errors.New("the entry bound was reached")
	}
	out := map[string]fs.FileInfo{}
	for _, c := range children {
		info, err := p.fsys.Lstat(filepath.Join(root, c.Name()))
		if err != nil {
			return nil, errors.New("a Cursor project entry cannot be looked up")
		}
		out[c.Name()] = info
	}
	if post, err := p.fsys.Lstat(root); err != nil || !sameFile(pre, post) {
		return nil, errors.New("the Cursor projects directory was replaced while it was listed")
	}
	return out, nil
}

// parentSlug derives P and B of case workspace ws from the validated
// workspace components (S = P + "-" + B, FP-15's slug of ws), never by
// deriving a slug of the parent alone.
func (p *cursorPreparer) parentSlug(ws string) (parent, base string, err error) {
	s, err := cursorProjectSlug(p.fsys, p.goos, p.goarch, p.version, ws)
	if err != nil {
		return "", "", err
	}
	base = filepath.Base(ws)
	parent, ok := strings.CutSuffix(s, "-"+base)
	if !ok || !strings.HasSuffix(parent, "-work") {
		return "", "", errors.New("the workspace slug has no verified parent prefix")
	}
	return parent, base, nil
}

// residueName checks a candidate directory name against P and B: the
// pre-suffix part is exactly P + "-" + B[:k] for 2 <= k < len(B) (a proper
// prefix of S, so a truncation before the case basename, the full slug or
// another case's prefix fails), and the suffix "-" plus seven lowercase
// hexadecimal characters.
func residueName(name, parent, base string) (prefix, suffix bool) {
	const hexLen = 7
	if len(name) > hexLen+1 && name[len(name)-hexLen-1] == '-' {
		suffix = true
		for _, c := range name[len(name)-hexLen:] {
			if !('0' <= c && c <= '9' || 'a' <= c && c <= 'f') {
				suffix = false
			}
		}
	}
	rest, ok := strings.CutPrefix(name, parent+"-")
	if !ok || len(rest) < hexLen+1 || rest[len(rest)-hexLen-1] != '-' {
		return false, suffix
	}
	mid := rest[:len(rest)-hexLen-1]
	return len(mid) >= 2 && len(mid) < len(base) && base[:len(mid)] == mid, suffix
}

// checkResidue is the post-session check of case caseID (workspace ws,
// baseline pre taken immediately before its model launch), after the
// session's clean cleanup: one of A3's three outcomes (no residue, hashed,
// in_place) with every check, then the bounded safety scan of the monitored
// roots in which only the ledger's and this candidate's bound entries are
// exempt or metadata-only, then the candidate's and the ledger's recheck.
// A verified result admits the candidate atomically; the record lists the
// ledger (a failure lists only earlier admitted entries).
func (p *cursorPreparer) checkResidue(caseID, ws string, pre caseResidueBaseline) WorkerResidue {
	out := WorkerResidue{Policy: WorkerResiduePolicyV2, State: ResidueUnverified, Entries: []WorkerResidueEntry{}}
	fail := func(reason string) WorkerResidue {
		out.Reason, out.Entries = sptr(reason), p.ledger.record()
		return out
	}
	failWhy := func(why string) WorkerResidue { return fail(ReasonCursorScopeUnverified + ": worker residue " + why) }
	failErr := func(err error, wrap string) WorkerResidue {
		if r, ok := fixedReason(err); ok {
			return fail(r)
		}
		return failWhy(wrap + err.Error())
	}
	post, err := p.residueSnapshot()
	if err != nil {
		return failErr(p.foreignVerdict(err, nil, p.sessionLocations(pre)...), "check failed: ")
	}
	cand, why, verbatim := p.classifyResidue(caseID, ws, pre, post)
	if why != "" {
		// A3.1's fixed reasons take precedence over a classification
		// failure (code review A3 round 1, C3): the foreign baseline is
		// revalidated first, passing over only the session's own candidate
		// locations, whose verdict (the hashed cap's verbatim reason
		// included) stands when the foreign artifacts are unchanged.
		if err := p.preflight(nil, "", p.candidateLocations(pre, post)...); err != nil {
			if r, ok := fixedReason(err); ok {
				return fail(r)
			}
		}
		if verbatim {
			return fail(why)
		}
		return failWhy(why)
	}
	if cand != nil {
		cand.n = p.ledger.next + 1
	}
	if err := p.safetyScan(ws, cand); err != nil {
		return failErr(p.foreignVerdict(err, cand), "check failed: ")
	}
	if err := p.revalidate(); err != nil {
		return failErr(p.foreignVerdict(err, cand), "check failed: ")
	}
	if cand != nil {
		if err := p.recheckEntry(cand); err != nil {
			if r, ok := fixedReason(p.foreignVerdict(err, cand)); ok {
				return fail(r)
			}
			return failWhy("is unverifiable: the candidate changed while it was checked")
		}
		p.ledger.entries = append(p.ledger.entries, cand)
		p.ledger.next = cand.n
	}
	out.State, out.Reason, out.Entries = ResidueVerified, nil, p.ledger.record()
	return out
}

// safetyScan is one bounded inventory of the approval roots of ws,
// bracketed by the metadata-only projects preflight (the foreign baseline
// revalidated before and after it), in which the ledger's, the candidate's
// and the foreign baseline's bound sockets are the only special files
// permitted and their bound session files are recorded by metadata only;
// an incomplete inventory fails (an A3.1 failure typed).
func (p *cursorPreparer) safetyScan(ws string, cand *residueEntry) error {
	if err := p.preflight(cand, ""); err != nil {
		return err
	}
	roots, err := approvalRoots(p.fsys, ws, p.home)
	if err != nil {
		return err
	}
	sc, err := p.scanner()
	if err != nil {
		return err
	}
	if cand != nil {
		ledgerExempt, ledgerMeta, root := sc.exempt, sc.metaOnly, p.projectsRoot()
		sc.exempt = func(path string, info fs.FileInfo) bool {
			return cand.socketBound(root, path, info) || ledgerExempt(path, info)
		}
		sc.metaOnly = func(path string, info fs.FileInfo) (bool, bool, error) {
			if reserved, bound := cand.reserve(root, path, info); reserved {
				return true, bound, nil
			}
			return ledgerMeta(path, info)
		}
	}
	s := sc.snapshot(roots)
	// The check again after the scan, before any candidate is accepted.
	if err := p.preflight(cand, ""); err != nil {
		return err
	}
	if !s.complete {
		if s.typed != nil {
			return s.typed
		}
		return errors.New("the monitored inventory is incomplete (" + s.why + ")")
	}
	return nil
}

// Amendment A3.1 (r0.15): unchanged foreign Cursor session artifacts.
//
// Session artifacts already present when a preparer first looks at the
// projects directory (an owner's unrelated Cursor projects, stale sockets,
// an earlier run's leftovers) may remain while they stay exactly as they
// were: a metadata-only foreign baseline, kept in memory per preparer,
// initialized once at its first projects preflight and revalidated in full
// at every later one. Nothing in it is ever opened, read, hashed or
// connected to, and nothing of it reaches evidence or logs: only the fixed
// reasons below are public. A case's own full-slug project path and its
// possible hashed-candidate directories must still be absent before its
// enable, whatever the baseline holds.

// A3.1's fixed public reasons (never wrapped, never carrying a path).
const (
	ReasonForeignChanged        = ReasonCursorScopeUnverified + ": foreign session artifact changed"
	ReasonForeignNew            = ReasonCursorScopeUnverified + ": new foreign session artifact"
	ReasonForeignRemoved        = ReasonCursorScopeUnverified + ": foreign session artifact removed"
	ReasonCaseProjectExists     = ReasonCursorScopeUnverified + ": case project directory exists before enable"
	ReasonHashedCandidateExists = ReasonCursorScopeUnverified + ": hashed candidate directory exists before enable"
	ReasonForeignLimit          = ReasonCursorScopeUnverified + ": foreign session baseline limit exceeded"
	ReasonForeignDeadline       = ReasonCursorScopeUnverified + ": foreign session baseline deadline exceeded"
	ReasonForeignUnverifiable   = ReasonCursorScopeUnverified + ": foreign session baseline unverifiable"
)

// hashedPrefixLen is the observed hashed residue names' slug prefix length.
const hashedPrefixLen = 57

// foreignError is an A3.1 failure: it carries only its complete fixed
// reason, which every consumer records verbatim (recognized by type before
// any generic wrapping, never by matching error text).
type foreignError struct{ reason string }

func (e *foreignError) Error() string { return e.reason }

func foreignFail(reason string) error { return &foreignError{reason: reason} }

// fixedReason is err's A3.1 fixed reason, if it is one.
func fixedReason(err error) (string, bool) {
	var fe *foreignError
	if errors.As(err, &fe) {
		return fe.reason, true
	}
	return "", false
}

// isFixed reports an A3.1 failure.
func isFixed(err error) bool {
	_, ok := fixedReason(err)
	return ok
}

// foreignKind is a recorded entry's role.
type foreignKind int

const (
	// foreignAncestor is a directory above recorded entries, up to the
	// projects root: compared by identity only (device, inode, type and
	// mode), its membership not frozen.
	foreignAncestor foreignKind = iota
	// foreignArtifact is a reserved name, a socket at any depth, or any
	// descendant of a reserved directory: files and sockets compared by
	// every recorded field (sameMeta), directories by identity, and a
	// reserved directory's membership frozen through its recorded
	// descendants.
	foreignArtifact
)

// foreignNode is one recorded or observed entry: its role and its
// no-follow lookup.
type foreignNode struct {
	kind foreignKind
	info fs.FileInfo
}

// foreignState is a preparer's foreign baseline: initialized at most once
// (a failed initialization stays the preparer's failure), keyed privately
// by projects-relative path ("" the projects root).
type foreignState struct {
	initialized bool
	err         error
	nodes       map[string]*foreignNode
}

// foreignView is one bounded no-follow walk of the projects tree: every
// directory (an ancestor unless reserved) and every artifact, and the links
// and other unsafe types it met without following them (strict: a reserved
// path or a special type other than a link, which no recorded path can
// excuse).
type foreignView struct {
	nodes  map[string]*foreignNode
	unsafe map[string]bool
}

// ownership is the case-ownership check of the case whose full project
// slug is slug, at its own preflight before its enable (A3.1): its
// full-slug path must not exist (any object, an empty or approval-only
// directory included), and when the slug has at least 57 characters no
// direct child directory (by a no-follow lookup) may be named its first 57
// characters, "-" and seven lowercase hexadecimal characters. The baseline and the ledger never
// excuse either.
func (p *cursorPreparer) ownership(slug string) error {
	root := p.projectsRoot()
	switch _, err := p.fsys.Lstat(filepath.Join(root, slug)); {
	case err == nil:
		return foreignFail(ReasonCaseProjectExists)
	case !errors.Is(err, fs.ErrNotExist):
		return foreignFail(ReasonForeignUnverifiable)
	}
	if len(slug) < hashedPrefixLen {
		return nil
	}
	children, err := p.fsys.ReadDir(root)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return nil
	case err != nil:
		return foreignFail(ReasonForeignUnverifiable)
	case len(children) > p.limits.Entries:
		return foreignFail(ReasonForeignLimit)
	}
	prefix := slug[:hashedPrefixLen] + "-"
	for _, c := range children {
		rest, ok := strings.CutPrefix(c.Name(), prefix)
		if !ok || len(rest) != 7 || strings.Trim(rest, "0123456789abcdef") != "" {
			continue
		}
		// Only a directory, by a no-follow lookup (code review A3 round 1,
		// W1): a file or link of that name is not a hashed candidate.
		info, err := p.fsys.Lstat(filepath.Join(root, c.Name()))
		switch {
		case errors.Is(err, fs.ErrNotExist):
		case err != nil:
			return foreignFail(ReasonForeignUnverifiable)
		case info.IsDir():
			return foreignFail(ReasonHashedCandidateExists)
		}
	}
	return nil
}

// observeForeign walks the projects tree bounded and without following
// links or opening anything, skipping the run's validated top-level
// directories (skip): every entry counts against the entry cap and every
// regular file's declared size against the per-file and aggregate byte
// caps; the deadline is checked at every entry. A cap, the deadline, a
// failed listing or lookup or a negative size fails with its fixed reason.
// The deadline is checked before the walk too (an empty tree has no entry).
func (p *cursorPreparer) observeForeign(skip map[string]bool) (*foreignView, error) {
	v := &foreignView{nodes: map[string]*foreignNode{}, unsafe: map[string]bool{}}
	if p.expired != nil && p.expired() {
		return nil, foreignFail(ReasonForeignDeadline)
	}
	root := p.projectsRoot()
	info, err := p.fsys.Lstat(root)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return v, nil
	case err != nil:
		return nil, foreignFail(ReasonForeignUnverifiable)
	case !info.IsDir():
		v.unsafe[""] = true
		return v, nil
	}
	v.nodes[""] = &foreignNode{kind: foreignAncestor, info: info}
	count := 0
	var total int64
	var walk func(abs, rel string, reserved bool) error
	walk = func(abs, rel string, reserved bool) error {
		children, err := p.fsys.ReadDir(abs)
		if err != nil {
			return foreignFail(ReasonForeignUnverifiable)
		}
		for _, c := range children {
			if count++; count > p.limits.Entries {
				return foreignFail(ReasonForeignLimit)
			}
			if p.expired != nil && p.expired() {
				return foreignFail(ReasonForeignDeadline)
			}
			if rel == "" && skip[c.Name()] {
				continue
			}
			crel, cabs := c.Name(), filepath.Join(abs, c.Name())
			if rel != "" {
				crel = rel + "/" + c.Name()
			}
			ci, err := p.fsys.Lstat(cabs)
			if err != nil {
				return foreignFail(ReasonForeignUnverifiable)
			}
			res := reserved || slices.Contains(sessionArtifactNames, c.Name())
			kind := foreignAncestor
			if res {
				kind = foreignArtifact
			}
			switch typ := ci.Mode().Type(); {
			case ci.IsDir():
				v.nodes[crel] = &foreignNode{kind: kind, info: ci}
				if err := walk(cabs, crel, res); err != nil {
					return err
				}
			case ci.Mode().IsRegular():
				if ci.Size() < 0 {
					return foreignFail(ReasonForeignUnverifiable)
				}
				if total += ci.Size(); ci.Size() > p.limits.FileBytes || total > p.limits.Bytes {
					return foreignFail(ReasonForeignLimit)
				}
				if res {
					v.nodes[crel] = &foreignNode{kind: foreignArtifact, info: ci}
				}
			case typ == fs.ModeSocket:
				v.nodes[crel] = &foreignNode{kind: foreignArtifact, info: ci}
			default:
				// Never followed or traversed: a link outside a reserved path
				// keeps the scanner's own policy unless it replaced a recorded
				// path.
				v.unsafe[crel] = res || typ != fs.ModeSymlink
			}
		}
		return nil
	}
	if err := walk(root, "", false); err != nil {
		return nil, err
	}
	return v, nil
}

// sameForeign compares a recorded entry with an observed one: directories
// by identity (device, inode, type and mode), never by their size or
// modification time; files and sockets by every recorded field.
func sameForeign(was, now *foreignNode) bool {
	if was.kind != now.kind {
		return false
	}
	if was.info.IsDir() {
		return now.info.IsDir() && sameFile(was.info, now.info) && was.info.Mode() == now.info.Mode()
	}
	return sameMeta(was.info, now.info)
}

// stableView is one stable view of the projects tree (A3.1: initial and
// every later one, code review A3 round 1, C1): two bounded walks that must
// agree exactly (every entry with the same identity and metadata, the same
// unsafe entries), so a change after one walk looked an entry up cannot
// pass unseen. A disagreement is a detected instability (unverifiable).
func (p *cursorPreparer) stableView(skip map[string]bool) (*foreignView, error) {
	first, err := p.observeForeign(skip)
	if err != nil {
		return nil, err
	}
	second, err := p.observeForeign(skip)
	if err != nil {
		return nil, err
	}
	if len(first.nodes) != len(second.nodes) || len(first.unsafe) != len(second.unsafe) {
		return nil, foreignFail(ReasonForeignUnverifiable)
	}
	for rel, n := range first.nodes {
		if m := second.nodes[rel]; m == nil || !sameForeign(n, m) {
			return nil, foreignFail(ReasonForeignUnverifiable)
		}
	}
	for rel, strict := range first.unsafe {
		if other, ok := second.unsafe[rel]; !ok || other != strict {
			return nil, foreignFail(ReasonForeignUnverifiable)
		}
	}
	return second, nil
}

// stableForeign takes the initial baseline from a stable view without an
// unsafe entry, keeping only the artifacts and their ancestors up to the
// projects root. An empty or absent projects root is an empty baseline.
func (p *cursorPreparer) stableForeign(skip map[string]bool) (map[string]*foreignNode, error) {
	first, err := p.stableView(skip)
	if err != nil {
		return nil, err
	}
	for _, strict := range first.unsafe {
		if strict {
			return nil, foreignFail(ReasonForeignUnverifiable)
		}
	}
	nodes := map[string]*foreignNode{}
	for rel, n := range first.nodes {
		if n.kind != foreignArtifact {
			continue
		}
		nodes[rel] = n
		for a := rel; a != ""; {
			if i := strings.LastIndex(a, "/"); i >= 0 {
				a = a[:i]
			} else {
				a = ""
			}
			nodes[a] = first.nodes[a]
		}
	}
	return nodes, nil
}

// revalidateForeign compares a fresh stable view with the baseline (never
// refreshing it), in A3.1's order: the bounded traversal (its caps,
// deadline and errors, a detected instability, and an unsafe type at a path
// the baseline does not record), then a recorded path missing (unless a
// replaced ancestor accounts for it), then a recorded path replaced or
// changed (a link or other unsafe type at a recorded path included, never
// followed), then a new artifact.
func (p *cursorPreparer) revalidateForeign(skip map[string]bool) error {
	v, err := p.stableView(skip)
	if err != nil {
		return err
	}
	base := p.foreign.nodes
	for rel, strict := range v.unsafe {
		if base[rel] == nil && strict {
			return foreignFail(ReasonForeignUnverifiable)
		}
	}
	changed := map[string]bool{}
	var missing []string
	for rel, was := range base {
		if _, unsafe := v.unsafe[rel]; unsafe {
			changed[rel] = true
			continue
		}
		now := v.nodes[rel]
		switch {
		case now == nil:
			missing = append(missing, rel)
		case !sameForeign(was, now):
			changed[rel] = true
		}
	}
	// Shallowest first: nothing beneath a changed (an unsafe type at a
	// recorded path included) or missing ancestor is probed (code review A3
	// round 1, W2: a lookup through a replaced link would traverse it); a
	// missing path beneath a changed ancestor does not count, beneath a
	// missing one it is removed with it.
	// The projects root ("") is strictly the shallowest, and the order is
	// deterministic (code review A3 round 2, W1).
	depth := func(rel string) int {
		if rel == "" {
			return -1
		}
		return strings.Count(rel, "/")
	}
	slices.SortFunc(missing, func(a, b string) int {
		if d := depth(a) - depth(b); d != 0 {
			return d
		}
		return strings.Compare(a, b)
	})
	gone := map[string]bool{}
	removed := false
	beneath := func(rel string, set map[string]bool) bool {
		for a := range set {
			if a == "" || strings.HasPrefix(rel, a+"/") {
				return true
			}
		}
		return false
	}
	for _, rel := range missing {
		switch {
		case beneath(rel, changed):
		case beneath(rel, gone):
			removed = true
		default:
			// Something else now at a recorded path is a replacement.
			if _, err := p.fsys.Lstat(filepath.Join(p.projectsRoot(), filepath.FromSlash(rel))); !errors.Is(err, fs.ErrNotExist) {
				changed[rel] = true
				continue
			}
			gone[rel], removed = true, true
		}
	}
	if removed {
		return foreignFail(ReasonForeignRemoved)
	}
	if len(changed) > 0 {
		return foreignFail(ReasonForeignChanged)
	}
	for rel, now := range v.nodes {
		if now.kind == foreignArtifact && base[rel] == nil {
			return foreignFail(ReasonForeignNew)
		}
	}
	return nil
}
