package mcpqual

// Cursor approval preparation (design decoder-enrollment B1, FP-13). The
// canonical Cursor trust recipe runs one fixed, owner-authorized "mcp
// enable probe" in the generated case workspace before its model session.
// The vendor documents that command as adding the server to "the local
// approved list" without saying where that list lives, so success of the
// command is never enough: the generated workspace, the owner's .cursor
// tree and every workspace ancestor's .cursor root are fingerprinted
// before and after it (bounded, no-follow, keyed digests kept in memory
// only), and the session may start only when the command exited 0 and
// every observed change is a regular file under <workspace>/.cursor/, at
// least one of them. Anything else stops before the model session. This
// is a change detector, not confinement: it cannot prevent or undo a write
// elsewhere, and it never restores, deletes or retries anything.

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"errors"
	"fmt"
	"hash"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

// Approval payload files of a launched Cursor approval stage.
const (
	FileApprovalStdout = "approval-stdout.txt"
	FileApprovalStderr = "approval-stderr.txt"
)

// Approval scopes.
const (
	ScopeNotChecked    = "not_checked"
	ScopeWorkspaceOnly = "workspace_only"
	ScopeOutside       = "outside_workspace"
	ScopeUnverifiable  = "unverifiable"
)

// Approval change kinds.
const (
	ChangeAdded    = "added"
	ChangeRemoved  = "removed"
	ChangeModified = "modified"
)

// Approval and placement reasons (design decoder-enrollment B1).
const (
	ReasonCursorScopeUnverified = "cursor_approval_scope_unverified"
	ReasonCursorOutside         = "cursor_approval_outside_workspace"
	ReasonCursorApprovalFailed  = "cursor_approval_failed"
	ReasonGrokPlacement         = "grok_workspace_placement_unverified"
)

// cursorApprovalWatchdog bounds the enable command within the client's
// remaining time.
const cursorApprovalWatchdog = 30 * time.Second

// Labels of the monitored roots in recorded change paths.
const (
	labelWorkspace  = "<workspace>"
	labelHomeCursor = "<home>/.cursor"
)

// CursorApprovalArgv is the fixed approval command after the executable.
func CursorApprovalArgv() []string { return []string{"mcp", "enable", "probe"} }

// ApprovalScanLimits bound one fingerprint snapshot of the monitored
// trees: entries (roots and directories included), total regular-file
// bytes and bytes per file. They are observation caps, never permission
// to skip: reaching one makes the scope unverifiable.
type ApprovalScanLimits struct {
	Entries   int
	Bytes     int64
	FileBytes int64
}

// DefaultApprovalScanLimits are the production caps: 10,000 entries,
// 64 MiB in all and 8 MiB per file per snapshot.
func DefaultApprovalScanLimits() ApprovalScanLimits {
	return ApprovalScanLimits{Entries: 10_000, Bytes: 64 << 20, FileBytes: 8 << 20}
}

// orDefault fills an unset (nonpositive) cap and lowers one above its
// default: tests inject smaller caps, never larger ones.
func (l ApprovalScanLimits) orDefault() ApprovalScanLimits {
	d := DefaultApprovalScanLimits()
	if l.Entries <= 0 || l.Entries > d.Entries {
		l.Entries = d.Entries
	}
	if l.Bytes <= 0 || l.Bytes > d.Bytes {
		l.Bytes = d.Bytes
	}
	if l.FileBytes <= 0 || l.FileBytes > d.FileBytes {
		l.FileBytes = d.FileBytes
	}
	return l
}

// approvalFS is the scan's read-only filesystem: no-follow lookups,
// directory listings, no-follow opens of regular files and path
// resolution. Production uses the operating system (osApprovalFS); the
// package's own unit tests inject tiny in-memory trees.
type approvalFS interface {
	Lstat(name string) (fs.FileInfo, error)
	ReadDir(name string) ([]fs.DirEntry, error)
	Open(name string) (approvalFile, error)
	EvalSymlinks(name string) (string, error)
}

// approvalFile is an opened regular file.
type approvalFile interface {
	io.Reader
	Stat() (fs.FileInfo, error)
	Close() error
}

type osApprovalFS struct{}

func (osApprovalFS) Lstat(name string) (fs.FileInfo, error)     { return os.Lstat(name) }
func (osApprovalFS) ReadDir(name string) ([]fs.DirEntry, error) { return os.ReadDir(name) }
func (osApprovalFS) EvalSymlinks(name string) (string, error)   { return filepath.EvalSymlinks(name) }

// scanRoot is one monitored root: its recorded label and its path. key,
// when set, is its inventory key instead of the label: two distinct roots
// can share a label (a resolved chain longer than the lexical one has the
// same hop count at another directory), and their entries must never merge.
type scanRoot struct {
	label, path, key string
}

// rootKeySep separates a disambiguated root key from its index; it never
// reaches a recorded path.
const rootKeySep = "\x00"

// recordedPath is the recorded form of an inventory key (its label path).
func recordedPath(key string) string {
	if i := strings.Index(key, rootKeySep); i >= 0 {
		rest := key[i+1:]
		j := strings.IndexByte(rest, '/')
		if j < 0 {
			return key[:i]
		}
		return key[:i] + rest[j:]
	}
	return key
}

// scanEntry is one observed entry. A regular file's digest is the keyed
// HMAC-SHA256 of its bytes; unknown marks an entry whose existence and type
// were observed but whose content could not be (a symbolic link, special
// or unreadable file).
type scanEntry struct {
	typ     fs.FileMode
	perm    fs.FileMode
	size    int64
	digest  [sha256.Size]byte
	unknown bool
}

func (e scanEntry) regular() bool { return e.typ == 0 && !e.unknown }

// snapshot is one bounded inventory of the monitored roots, keyed by
// labeled path. complete is false after any symbolic link, special,
// unreadable or unstable file, traversal error or exhausted bound; why
// names the first such cause (a safe text, never a path or content).
type snapshot struct {
	entries  map[string]scanEntry
	complete bool
	why      string
	count    int
	bytes    int64
}

func (s *snapshot) fail(why string) {
	if s.complete {
		s.complete, s.why = false, why
	}
}

// errBound stops a snapshot at an exhausted cap.
var errBound = errors.New("an inventory bound was reached")

// approvalScanner takes the two snapshots with one random ephemeral key.
type approvalScanner struct {
	fs  approvalFS
	lim ApprovalScanLimits
	key []byte
}

func newApprovalScanner(fsys approvalFS, lim ApprovalScanLimits) (*approvalScanner, error) {
	if fsys == nil {
		fsys = osApprovalFS{}
	}
	key := make([]byte, 32)
	if _, err := io.ReadFull(rand.Reader, key); err != nil {
		return nil, err
	}
	return &approvalScanner{fs: fsys, lim: lim.orDefault(), key: key}, nil
}

// snapshot inventories every root: an absent root is recorded absent (its
// creation is then a change); a root that is a symbolic link is never
// followed.
func (sc *approvalScanner) snapshot(roots []scanRoot) *snapshot {
	s := &snapshot{entries: map[string]scanEntry{}, complete: true}
	mac := hmac.New(sha256.New, sc.key)
	for _, r := range roots {
		info, err := sc.fs.Lstat(r.path)
		switch {
		case errors.Is(err, fs.ErrNotExist):
			continue
		case err != nil:
			s.fail("a monitored root cannot be looked up")
			continue
		}
		key := r.label
		if r.key != "" {
			key = r.key
		}
		if sc.entry(s, mac, key, r.path, info) == errBound {
			return s
		}
	}
	return s
}

// entry records one entry (and a directory's subtree, depth first in name
// order); it returns errBound when a cap stops the snapshot.
func (sc *approvalScanner) entry(s *snapshot, mac hash.Hash, key, p string, info fs.FileInfo) error {
	s.count++
	if s.count > sc.lim.Entries {
		s.fail("the entry bound was reached")
		return errBound
	}
	e := scanEntry{typ: info.Mode().Type(), perm: info.Mode().Perm(), size: info.Size()}
	switch {
	case info.IsDir():
		e.size = 0
		s.entries[key] = e
		children, err := sc.fs.ReadDir(p)
		if err != nil {
			s.fail("a directory cannot be listed")
			return nil
		}
		slices.SortFunc(children, func(a, b fs.DirEntry) int { return strings.Compare(a.Name(), b.Name()) })
		for _, c := range children {
			cp := filepath.Join(p, c.Name())
			ci, err := sc.fs.Lstat(cp)
			if err != nil {
				s.count++
				s.entries[key+"/"+c.Name()] = scanEntry{unknown: true}
				s.fail("an entry cannot be looked up")
				continue
			}
			if err := sc.entry(s, mac, key+"/"+c.Name(), cp, ci); err != nil {
				return err
			}
		}
	case info.Mode().IsRegular():
		if info.Size() > sc.lim.FileBytes {
			s.fail("the per-file byte bound was reached")
			return errBound
		}
		if s.bytes+info.Size() > sc.lim.Bytes {
			s.fail("the total byte bound was reached")
			return errBound
		}
		s.bytes += info.Size()
		digest, why := sc.digest(mac, p, info)
		if why != "" {
			e.unknown = true
			s.fail(why)
		}
		e.digest = digest
		s.entries[key] = e
	case info.Mode()&fs.ModeSymlink != 0:
		e.unknown = true
		s.entries[key] = e
		s.fail("a symbolic link is in a monitored tree")
	default:
		e.unknown = true
		s.entries[key] = e
		s.fail("a special file is in a monitored tree")
	}
	return nil
}

// digest streams one regular file through the keyed HMAC: opened without
// following a link, it must be the looked-up file, read to exactly its
// size, and unchanged afterwards (otherwise it is unstable).
func (sc *approvalScanner) digest(mac hash.Hash, p string, pre fs.FileInfo) ([sha256.Size]byte, string) {
	var d [sha256.Size]byte
	f, err := sc.fs.Open(p)
	if err != nil {
		return d, "a file cannot be opened"
	}
	defer f.Close()
	opened, err := f.Stat()
	if err != nil || !opened.Mode().IsRegular() || !sameFile(pre, opened) || opened.Size() != pre.Size() || !opened.ModTime().Equal(pre.ModTime()) {
		return d, "a file changed while it was read"
	}
	mac.Reset()
	n, err := io.Copy(mac, io.LimitReader(f, pre.Size()+1))
	if err != nil {
		return d, "a file cannot be read"
	}
	after, err1 := f.Stat()
	post, err2 := sc.fs.Lstat(p)
	if n != pre.Size() || err1 != nil || err2 != nil || after.Size() != pre.Size() || !after.ModTime().Equal(pre.ModTime()) || !sameFile(pre, post) {
		return d, "a file changed while it was read"
	}
	copy(d[:], mac.Sum(nil))
	return d, ""
}

// approvalChange is one difference with what the scope rule needs.
type approvalChange struct {
	CaptureApprovalChange
	regular bool
}

// diffSnapshots compares the sorted labeled inventories: an entry only in
// post is added, only in pre removed (reported only when post is complete,
// since an incomplete post may simply not have reached it), and in both
// with another type, mode, size or digest modified (unless either side's
// content is unknown).
func diffSnapshots(pre, post *snapshot) []approvalChange {
	keys := map[string]bool{}
	for k := range pre.entries {
		keys[k] = true
	}
	for k := range post.entries {
		keys[k] = true
	}
	sorted := make([]string, 0, len(keys))
	for k := range keys {
		sorted = append(sorted, k)
	}
	sort.Strings(sorted)
	var out []approvalChange
	for _, k := range sorted {
		a, inPre := pre.entries[k]
		b, inPost := post.entries[k]
		switch {
		case !inPre:
			out = append(out, approvalChange{CaptureApprovalChange{k, ChangeAdded}, b.regular()})
		case !inPost:
			if post.complete {
				out = append(out, approvalChange{CaptureApprovalChange{k, ChangeRemoved}, a.regular()})
			}
		case a.unknown || b.unknown:
		case a.typ != b.typ || a.perm != b.perm || a.size != b.size || a.digest != b.digest:
			out = append(out, approvalChange{CaptureApprovalChange{k, ChangeModified}, a.regular() && b.regular()})
		}
	}
	return out
}

// approvalRoots are the monitored roots of workspace ws: the workspace
// itself, the owner's .cursor (from Env.Home, never a child override) and
// the .cursor root of every ancestor of the workspace up to the filesystem
// root, on both its lexical and its resolved chain, deduplicated by
// canonical path (the first label wins).
func approvalRoots(fsys approvalFS, ws, home string) ([]scanRoot, error) {
	if fsys == nil {
		fsys = osApprovalFS{}
	}
	if home == "" || !filepath.IsAbs(home) {
		return nil, errors.New("the home directory is unknown")
	}
	real, err := fsys.EvalSymlinks(ws)
	if err != nil {
		return nil, errors.New("the workspace does not resolve")
	}
	seen := map[string]bool{}
	var roots []scanRoot
	add := func(label, p string) {
		p = filepath.Clean(p)
		canon := p
		if dir, err := fsys.EvalSymlinks(filepath.Dir(p)); err == nil {
			canon = filepath.Join(dir, filepath.Base(p))
		}
		if label == labelWorkspace {
			canon = real
		}
		if !seen[canon] {
			seen[canon] = true
			roots = append(roots, scanRoot{label: label, path: p})
		}
	}
	add(labelWorkspace, ws)
	add(labelHomeCursor, filepath.Join(home, ".cursor"))
	for _, chain := range []string{filepath.Clean(ws), real} {
		for d, n := filepath.Dir(chain), 1; ; d, n = filepath.Dir(d), n+1 {
			add("<ancestor-"+strconv.Itoa(n)+">/.cursor", filepath.Join(d, ".cursor"))
			if d == filepath.Dir(d) {
				break
			}
		}
	}
	labels := map[string]bool{}
	for i := range roots {
		if labels[roots[i].label] {
			roots[i].key = roots[i].label + rootKeySep + strconv.Itoa(i)
		}
		labels[roots[i].label] = true
	}
	return roots, nil
}

// changePathRe is a recorded change path: a monitored root's label and a
// clean relative path inside it.
var changePathRe = regexp.MustCompile(`^(<workspace>|<home>/\.cursor|<ancestor-[1-9][0-9]*>/\.cursor)(/.*)?$`)

// validChangePath checks a recorded change path's label and relative part.
func validChangePath(p string) bool {
	m := changePathRe.FindStringSubmatch(p)
	if m == nil {
		return false
	}
	return m[2] == "" || checkRelPath("change", m[2][1:]) == nil
}

// inWorkspaceCursor reports a path under <workspace>/.cursor/.
func inWorkspaceCursor(p string) bool { return strings.HasPrefix(p, labelWorkspace+"/.cursor/") }

// inWorkspace reports the workspace root or a path inside it.
func inWorkspace(p string) bool {
	return p == labelWorkspace || strings.HasPrefix(p, labelWorkspace+"/")
}

// newApproval is a configured, not yet launched approval record.
func newApproval(reason string) *CaptureApproval {
	return &CaptureApproval{Argv: CursorApprovalArgv(), Cwd: labelWorkspace, Stage: notRunStage(reason), Scope: ScopeNotChecked, Reason: sptr(reason),
		Changes: []CaptureApprovalChange{}}
}

// cursorTrusted reports a Cursor plan that entered and passed the trusted
// recipe validator: only it gets the approval preparation.
func cursorTrusted(pc *PlanClient) bool {
	entered, err := pc.trustedRecipe()
	return pc.ID == "cursor" && entered && err == nil
}

// prepareCursorApproval runs the approval stage of cc's prepared case in
// (after version, help and the configuration write, before the session).
// It returns "" when the session may start, otherwise the client's stop
// reason. A failed pre-scan launches nothing. The enable command runs
// once, with min(30 s, the client's remaining time), through the shared
// launch and cleanup; its streams become approval-stdout.txt and
// approval-stderr.txt; the second snapshot follows its cleanup.
func (c *CaptureRunner) prepareCursorApproval(ctx context.Context, pc *PlanClient, cc *CaptureClient, in caseInputs, deadline time.Time) string {
	ap := newApproval("not reached")
	cc.Approval = ap
	unverified := func(why string) string {
		reason := ReasonCursorScopeUnverified + ": " + why
		ap.Stage, ap.Scope, ap.Reason = notRunStage(reason), ScopeUnverifiable, sptr(reason)
		return reason
	}
	roots, err := approvalRoots(c.approvalFS, in.ws, c.Home)
	if err != nil {
		return unverified(err.Error())
	}
	sc, err := newApprovalScanner(c.approvalFS, c.ApprovalLimits)
	if err != nil {
		return unverified("no fingerprint key")
	}
	pre := sc.snapshot(roots)
	if !pre.complete {
		return unverified("the inventory before the command is incomplete (" + pre.why + ")")
	}
	wd, capped := c.r.allowance(cursorApprovalWatchdog, deadline)
	if wd <= 0 {
		reason := ReasonBudget + ": max_client_ms"
		ap.Stage, ap.Reason = notRunStage(reason), sptr(reason)
		return reason
	}
	run := c.r.launch(ctx, ProcSpec{Path: pc.Executable, Args: CursorApprovalArgv(), Env: in.proc.Env, Dir: in.ws, CaptureStderr: true}, wd, c.lim.Stream)
	run.budgetCapped = capped
	c.r.recordCleanup(pc.ID+" approval", run.cleanup)
	ap.Stage = stageOf(run, wd)
	if ap.Stage.State != StageRan {
		reason := ReasonCursorApprovalFailed + ": the enable command did not launch (" + *ap.Stage.Reason + ")"
		ap.Reason = sptr(reason)
		return reason
	}
	var out strings.Builder
	for _, l := range run.transcript.Lines {
		out.Write(l.Data)
		out.WriteByte('\n')
	}
	c.text(cc, ClientFile(pc.ID, FileApprovalStdout), []byte(out.String()), run.transcript.Truncated)
	c.text(cc, ClientFile(pc.ID, FileApprovalStderr), run.stderr, run.stderrCut)
	if c.ioError != nil {
		reason := "workspace: " + c.ioError.Error()
		ap.Scope, ap.Reason = ScopeUnverifiable, sptr(reason)
		return reason
	}
	post := sc.snapshot(roots)
	changes := diffSnapshots(pre, post)
	ap.InventoryComplete = post.complete
	ap.Changes = make([]CaptureApprovalChange, 0, len(changes))
	outside, faithful, workspaceFiles := false, true, true
	red := c.r.redactor
	recorded := map[string]bool{}
	for _, ch := range changes {
		// Two roots sharing a label record one path: the list could not
		// tell them apart, so it is incomplete (the decision itself used
		// the distinct keys).
		ch.Path = recordedPath(ch.Path)
		if recorded[ch.Path] {
			faithful = false
			continue
		}
		recorded[ch.Path] = true
		if !inWorkspace(ch.Path) {
			outside = true
		}
		if !inWorkspaceCursor(ch.Path) || !ch.regular {
			workspaceFiles = false
		}
		// A path the redaction would change (or cannot carry) is not a
		// faithful change list: record it redacted and fail closed.
		if clean := red.String(ch.Path); !utf8.ValidString(ch.Path) || clean != ch.Path {
			faithful = false
			ch.Path = clean
		}
		ap.Changes = append(ap.Changes, ch.CaptureApprovalChange)
	}
	sort.SliceStable(ap.Changes, func(i, j int) bool { return ap.Changes[i].Path < ap.Changes[j].Path })
	if !faithful {
		ap.InventoryComplete = false
	}
	ap.Scope = ScopeUnverifiable
	if outside {
		ap.Scope = ScopeOutside
	}
	var reason string
	switch {
	case run.interrupted:
		reason = ReasonInterrupted
	case run.cleanup.Error != nil:
		reason = ReasonCleanupFailed
	case outside:
		reason = ReasonCursorOutside + ": the enable command may have changed state outside the workspace; investigate before another attempt (nothing was restored)"
	case !cleanStage(ap.Stage):
		reason = ReasonCursorApprovalFailed + ": the enable command did not exit 0 by itself"
	case !post.complete:
		reason = ReasonCursorScopeUnverified + ": the inventory after the command is incomplete (" + post.why + ")"
	case !faithful:
		reason = ReasonCursorScopeUnverified + ": a changed path cannot be recorded faithfully"
	case len(changes) == 0:
		reason = ReasonCursorScopeUnverified + ": no change gives positive evidence of a workspace approval"
	case !workspaceFiles:
		reason = ReasonCursorScopeUnverified + ": a change is not a regular file under <workspace>/.cursor/"
	default:
		ap.Scope, ap.Reason = ScopeWorkspaceOnly, nil
		return ""
	}
	ap.Reason = sptr(reason)
	return reason
}

// rereadCursorConfig is the generated .cursor/mcp.json after a scoped
// approval, within the case-file bound, with the substituted paths
// labeled again (as the configuration snapshot labels them), so config.txt
// shows the configuration the session will actually read.
func (c *CaptureRunner) rereadCursorConfig(in caseInputs) ([]byte, error) {
	fsys := c.approvalFS
	if fsys == nil {
		fsys = osApprovalFS{}
	}
	info, err := fsys.Lstat(in.configPath)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() || info.Size() > MaxCaseFileBytes {
		return nil, fmt.Errorf("it is not a regular file of at most %d bytes", MaxCaseFileBytes)
	}
	f, err := fsys.Open(in.configPath)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	b, err := readLimited(f, path.Base(in.configPath), MaxCaseFileBytes)
	if err != nil {
		return nil, err
	}
	esc := func(s string) string { q := quoteJSON(s); return q[1 : len(q)-1] }
	pairs := [][2]string{{esc(in.casePath), "<workspace>/case.json"}, {esc(in.eventsPath), "<workspace>/server-events.jsonl"}, {esc(c.ServerPath), "<server>"}, {esc(in.ws), labelWorkspace}}
	sort.SliceStable(pairs, func(i, j int) bool { return len(pairs[i][0]) > len(pairs[j][0]) })
	s := string(b)
	for _, p := range pairs {
		if p[0] != "" {
			s = strings.ReplaceAll(s, p[0], p[1])
		}
	}
	return []byte(s), nil
}
