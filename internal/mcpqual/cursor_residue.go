package mcpqual

// Run-attributed Cursor worker socket residue (design decoder-enrollment B3,
// FP-25). After each session Cursor Agent 2026.10.01-e373342 on
// linux/amd64 was observed to leave a Unix socket worker.sock, alone in a
// second, hashed directory under ~/.cursor/projects whose name is a
// truncated form of the workspace's project slug and a seven-hex suffix
// (both observed names cut the slug at 57 characters; that is a record of
// two observations, not a pinned algorithm, and no hash is ever computed).
//
// The harness admits such a socket only into an in-memory, run-local
// ledger, and only when it is attributed to this run: a direct child of
// the ordinary projects root absent immediately before this case's model
// launch, present after its clean cleanup, the same ordinary directory
// throughout, named exactly <P>-<B[:k]>-<hex7> (P the workspace parent's
// slug, B the case basename, 2 <= k < len(B)), holding exactly one entry,
// worker.sock, which a no-follow lookup identifies as a socket; at most one
// per session; and no other special file anywhere the normal monitored
// scan covers. Only valid ledger entries are exempt in later scans of the
// same run, each revalidated every time (same directory and socket, still
// the sole entry); an entry whose directory and socket both vanished is
// dropped, any other change fails closed. Nothing is persisted, inherited
// by another invocation, opened, connected to, followed or deleted: a
// later invocation fails closed on leftovers until the owner, with Cursor
// stopped, inspects and removes them. Recorded evidence carries labels and
// booleans only, never the raw directory name, suffix or an inode.

import (
	"errors"
	"fmt"
	"io/fs"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
)

// Residue evidence (design decoder-enrollment B3, Residue evidence).
const (
	// WorkerResiduePolicy names the residue rule of the one adapter.
	WorkerResiduePolicy = "cursor-worker-residue-linux-amd64-2026.10.01-e373342-v1"
	// Residue states.
	ResidueVerified   = "verified"
	ResidueUnverified = "unverified"
	ResidueNotChecked = "not_checked"
	// ResidueOrigin is the only origin of an admitted entry.
	ResidueOrigin = "appeared_during_session"
	// cursorWorkerSocket is the residue directory's one entry.
	cursorWorkerSocket = "worker.sock"
	// MaxResidueParentSlug is the longest workspace-parent slug P whose
	// observed 57-character residue prefix still keeps two characters of the
	// case basename (r0.13 post-final clarification DW1: 54 + 1 + 2 = 57).
	MaxResidueParentSlug = 54
	// ReasonResidueParentSlug refuses a longer parent slug before any enable
	// or model launch (design decoder-enrollment B3, FP-26).
	ReasonResidueParentSlug = ReasonCursorScopeUnverified + ": cursor residue parent slug exceeds 54 characters"
	// MaxResidueEntries bounds one confirmation's admitted residue (at most
	// one per session, three sessions).
	MaxResidueEntries = 3
)

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

// WorkerResidueEntry is one admitted (or rejected) residue socket: its
// owning case, its normalized labeled path, its origin and the checks.
type WorkerResidueEntry struct {
	CaseID             string `json:"case_id"`
	Path               string `json:"path"`
	Origin             string `json:"origin"`
	PrefixVerified     bool   `json:"prefix_verified"`
	SuffixVerified     bool   `json:"suffix_verified"`
	SocketTypeVerified bool   `json:"socket_type_verified"`
	SoleEntryVerified  bool   `json:"sole_entry_verified"`
	IdentityVerified   bool   `json:"identity_verified"`
}

// residueMembers and residueEntryMembers are the strict member sets.
var (
	residueMembers      = []string{"policy", "state", "reason", "entries"}
	residueEntryMembers = []string{"case_id", "path", "origin", "prefix_verified", "suffix_verified", "socket_type_verified", "sole_entry_verified", "identity_verified"}
)

// residuePathRe is a normalized residue path.
var residuePathRe = regexp.MustCompile(`^<home>/\.cursor/projects/<case-residue-([1-9][0-9]*)>/worker\.sock$`)

// residuePath is the normalized path of run-local residue n.
func residuePath(n int) string {
	return labelHomeProjects + "/<case-residue-" + strconv.Itoa(n) + ">/" + cursorWorkerSocket
}

// notCheckedResidue is the residue record of a check that did not run.
func notCheckedResidue(reason string) WorkerResidue {
	return WorkerResidue{Policy: WorkerResiduePolicy, State: ResidueNotChecked, Reason: sptr(reason), Entries: []WorkerResidueEntry{}}
}

// validate checks a residue record: the exact policy, a known state with
// its reason pairing, unique normalized paths, every entry from a session,
// one entry per owning case and at most limit entries, each owning case
// accepted by ownedBy (an already run case), and all checks true when
// verified (an unverified record keeps the flags that failed).
func (w *WorkerResidue) validate(ownedBy func(caseID string) bool, limit int) error {
	switch {
	case w.Policy != WorkerResiduePolicy:
		return fmt.Errorf("residue: policy %q, want %q", w.Policy, WorkerResiduePolicy)
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
		all := e.PrefixVerified && e.SuffixVerified && e.SocketTypeVerified && e.SoleEntryVerified && e.IdentityVerified
		switch {
		case !residuePathRe.MatchString(e.Path) || paths[e.Path]:
			return fmt.Errorf("residue: path %q is not a unique normalized residue path", e.Path)
		case e.Origin != ResidueOrigin:
			return fmt.Errorf("residue: origin %q", e.Origin)
		case owners[e.CaseID] || !ownedBy(e.CaseID):
			return fmt.Errorf("residue: case %q owns more than one entry or has not run", e.CaseID)
		case w.State == ResidueVerified && !all:
			return errors.New("residue: verified with a failed check")
		}
		paths[e.Path], owners[e.CaseID] = true, true
	}
	return nil
}

// residueEntry is one ledger entry: its owning case, its raw directory name
// (in memory only), the directory's and the socket's no-follow identities
// and its stable run-local number.
type residueEntry struct {
	caseID, name string
	dir, sock    fs.FileInfo
	n            int
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
		out = append(out, WorkerResidueEntry{CaseID: e.caseID, Path: residuePath(e.n), Origin: ResidueOrigin, PrefixVerified: true, SuffixVerified: true,
			SocketTypeVerified: true, SoleEntryVerified: true, IdentityVerified: true})
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

// revalidate checks every ledger entry before a scan: unchanged directory
// and socket identities, the socket still the sole entry. An entry whose
// directory and socket both disappeared is dropped; a partial
// disappearance, a replacement or an added entry fails closed.
func (p *cursorPreparer) revalidate() error {
	kept := p.ledger.entries[:0:0]
	for _, e := range p.ledger.entries {
		dir := filepath.Join(p.projectsRoot(), e.name)
		dinfo, derr := p.fsys.Lstat(dir)
		_, serr := p.fsys.Lstat(filepath.Join(dir, cursorWorkerSocket))
		switch {
		case errors.Is(derr, fs.ErrNotExist) && errors.Is(serr, fs.ErrNotExist):
			continue
		case derr != nil || !dinfo.IsDir() || !sameFile(e.dir, dinfo):
			return errors.New("an admitted residue directory changed, was replaced or partly disappeared")
		}
		sock, sole, socket, err := p.soleSocket(dir)
		if err != nil || !sole || !socket || !sameFile(e.sock, sock) {
			return errors.New("an admitted residue socket changed, was replaced, disappeared or gained a sibling")
		}
		kept = append(kept, e)
	}
	p.ledger.entries = kept
	return nil
}

// exempt reports a ledger socket at p with its admitted identity: the only
// special file a scan of this run may pass over.
func (p *cursorPreparer) exempt(path string, info fs.FileInfo) bool {
	for _, e := range p.ledger.entries {
		if path == filepath.Join(p.projectsRoot(), e.name, cursorWorkerSocket) {
			socket, err := residueSocket(info)
			return err == nil && socket && sameFile(e.sock, info)
		}
	}
	return false
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
// projects children pre taken immediately before its model launch),
// after the session's clean cleanup: the newly appeared children, at most
// one attributed residue candidate with every check, then the bounded
// safety scan of the monitored roots in which only ledger entries and this
// candidate may be special files, then the candidate's recheck. A
// verified result admits the candidate; the record lists the ledger.
func (p *cursorPreparer) checkResidue(caseID, ws string, pre map[string]fs.FileInfo) WorkerResidue {
	out := WorkerResidue{Policy: WorkerResiduePolicy, State: ResidueUnverified, Entries: []WorkerResidueEntry{}}
	fail := func(why string) WorkerResidue {
		out.Reason = sptr(ReasonCursorScopeUnverified + ": worker residue " + why)
		if len(out.Entries) == 0 {
			out.Entries = p.ledger.record()
		}
		return out
	}
	post, err := p.residueSnapshot()
	if err != nil {
		return fail("check failed: " + err.Error())
	}
	root := p.projectsRoot()
	var candidates []string
	names := make([]string, 0, len(post))
	for name := range post {
		names = append(names, name)
	}
	slices.Sort(names)
	for _, name := range names {
		info := post[name]
		if before, ok := pre[name]; ok {
			if !sameFile(before, info) {
				return fail("check failed: a Cursor project entry was replaced during the session")
			}
			continue
		}
		if !info.IsDir() {
			if !info.Mode().IsRegular() {
				return fail("check failed: a new special file or link in the Cursor projects directory")
			}
			continue
		}
		children, err := p.fsys.ReadDir(filepath.Join(root, name))
		if err != nil {
			return fail("check failed: a new Cursor project directory cannot be listed")
		}
		for _, c := range children {
			if c.Name() == cursorWorkerSocket || !c.IsDir() && !c.Type().IsRegular() {
				candidates = append(candidates, name)
				break
			}
		}
	}
	if len(candidates) > 1 {
		return fail("is ambiguous: more than one new residue candidate appeared in one session")
	}
	var cand *residueEntry
	if len(candidates) == 1 {
		name := candidates[0]
		dir := filepath.Join(root, name)
		parent, base, perr := p.parentSlug(ws)
		e := WorkerResidueEntry{CaseID: caseID, Path: residuePath(p.ledger.next + 1), Origin: ResidueOrigin}
		if perr == nil {
			e.PrefixVerified, e.SuffixVerified = residueName(name, parent, base)
		}
		sock, sole, socket, err := p.soleSocket(dir)
		e.SoleEntryVerified, e.SocketTypeVerified = sole, socket
		if err != nil {
			out.Entries = append(p.ledger.record(), e)
			return fail("check failed: " + err.Error())
		}
		dinfo, derr := p.fsys.Lstat(dir)
		e.IdentityVerified = derr == nil && dinfo.IsDir() && sameFile(post[name], dinfo)
		if !(e.PrefixVerified && e.SuffixVerified && e.SoleEntryVerified && e.SocketTypeVerified && e.IdentityVerified) {
			out.Entries = append(p.ledger.record(), e)
			return fail("is unverifiable: a new candidate does not have the attributed name, sole socket entry or stable identity")
		}
		cand = &residueEntry{caseID: caseID, name: name, dir: dinfo, sock: sock, n: p.ledger.next + 1}
	}
	// The bounded safety scan: no other special file or link anywhere the
	// normal monitored scan covers; the ledger and the candidate exempt.
	if err := p.safetyScan(ws, cand); err != nil {
		return fail("check failed: " + err.Error())
	}
	if cand != nil {
		dir := filepath.Join(root, cand.name)
		dinfo, derr := p.fsys.Lstat(dir)
		sock, sole, socket, err := p.soleSocket(dir)
		if derr != nil || err != nil || !sole || !socket || !sameFile(cand.dir, dinfo) || !sameFile(cand.sock, sock) {
			e := WorkerResidueEntry{CaseID: caseID, Path: residuePath(cand.n), Origin: ResidueOrigin, PrefixVerified: true, SuffixVerified: true,
				SocketTypeVerified: socket, SoleEntryVerified: sole}
			out.Entries = append(p.ledger.record(), e)
			return fail("is unverifiable: the candidate changed while it was checked")
		}
		p.ledger.entries = append(p.ledger.entries, cand)
		p.ledger.next = cand.n
	}
	out.State, out.Reason, out.Entries = ResidueVerified, nil, p.ledger.record()
	return out
}

// safetyScan is one bounded inventory of the approval roots of ws in which
// the ledger's sockets and the candidate's are the only special files
// permitted; an incomplete inventory fails.
func (p *cursorPreparer) safetyScan(ws string, cand *residueEntry) error {
	roots, err := approvalRoots(p.fsys, ws, p.home)
	if err != nil {
		return err
	}
	sc, err := p.scanner()
	if err != nil {
		return err
	}
	if cand != nil {
		ledgerExempt := sc.exempt
		candPath := filepath.Join(p.projectsRoot(), cand.name, cursorWorkerSocket)
		sc.exempt = func(path string, info fs.FileInfo) bool {
			if path == candPath {
				socket, err := residueSocket(info)
				return err == nil && socket && sameFile(cand.sock, info)
			}
			return ledgerExempt(path, info)
		}
	}
	if s := sc.snapshot(roots); !s.complete {
		return errors.New("the monitored inventory is incomplete (" + s.why + ")")
	}
	return nil
}
