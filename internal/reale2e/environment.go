package reale2e

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"hash"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"reflect"
	"slices"
	"sort"
	"strings"
	"syscall"
	"time"
	"unicode/utf8"
)

// Owner-environment fingerprints (design 12a-real-e2e, amendments A3 and A3.1,
// FP-11): detect and report, never prevent or repair. Before any vendor,
// Callsheet or Go executable runs, and again after cleanup, the
// supervisor snapshots read-only (1) every Git repository below each
// supplied --protect-root and (2) every entry below the four vendor homes.
// A3.6: a vendor entry is owner-configuration only at the exact strict
// vendor paths (and below listed directories); it is content-hashed (a
// regular file) with its metadata and must not change, be added or be
// removed. Every other vendor entry is tool-maintained: compared by
// metadata only, never content, and summarized per vendor top-level
// directory, never failed. Repositories are compared by HEAD, branch, raw
// status and a tracked-file manifest.
// The only exclusions are this run's exact evidence subtree and runtime.
// Every bound is enforced while collecting; overflow, read errors and
// timeouts make the proof incomplete, never sampled. A3.4: a symlink in a
// vendor home or under a protected root is fingerprinted as the link itself
// (its target text and its own metadata) and never followed, so a dangling
// link is valid; the evidence snapshots hold one compact record per entry
// (path and a SHA-256 of its full record), the supervisor keeps the full
// records in memory, and diff.json carries the full before and after
// records of every differing entry except tool-maintained ones beyond its
// bound (A3.6: a counted, deterministic prefix of those).

// Environment bounds (policy, not performance claims).
const (
	maxEnvEntries     = 100000
	maxEnvRepos       = 1000
	maxConfigFile     = 1 << 20
	maxConfigTotal    = 64 << 20
	maxSnapshotBytes  = 16 << 20
	maxShardBytes     = 8 << 20
	maxDiffBytes      = 4 << 20
	maxGitOutput      = 8 << 20
	maxEnvironmentAll = 2*maxSnapshotBytes + maxDiffBytes
)

// Environment failure codes.
const (
	CodeEnvModified   = "owner_environment_modified"
	CodeEnvUnobserved = "owner_environment_unobserved"
	CodeEnvLimit      = "environment_snapshot_limit"
	CodeInvalidRoot   = "invalid_protect_root"
	// CodeEnvInventory: a stored snapshot lacks a vendor home or supplied
	// root record, or holds a record of an unknown kind, scope or path.
	CodeEnvInventory = "environment_inventory_incomplete"
)

// Vendors are the four protected vendor homes, in snapshot order.
var Vendors = []string{"claude", "codex", "grok", "cursor"}

// Record classes: a vendor entry is owner-configuration or tool-maintained
// (A3.6); a supplied root and a link under it keep their own classes.
const (
	ClassOwner = "owner-configuration"
	ClassTool  = "tool-maintained"
	ClassRoot  = "root"
	// ClassLink is a symlink under a protected root (A3.4).
	ClassLink = "symlink"
)

// GitRunner runs one read-only git command in dir: its standard output
// (streamed to out instead when out is set, A3.2), its exit status, and an
// error only when git could not be run to an observed exit (start failure,
// timeout, captured output over the bound).
type GitRunner func(dir string, out io.Writer, args ...string) (stdout []byte, exit int, err error)

// gitReadOnlyArgs prefix every probe: no fsmonitor or untracked cache.
var gitReadOnlyArgs = []string{"-c", "core.fsmonitor=false", "-c", "core.untrackedCache=false"}

// gitProbeEnv is added to the environment of every probe: optional locks
// (and so index refreshes) off.
const gitProbeEnv = "GIT_OPTIONAL_LOCKS=0"

// EnvRecord is one snapshot line. Kind selects the fields used: header
// (schema, phase, completeness, roots), home (a vendor home's presence),
// root (a supplied root), entry (a vendor-home entry), link (a symlink
// discovery read under a protected root, by its own metadata and target
// text, A3.4), repo (a repository identity and its streamed digests),
// invalid_repo (a .git Git rejects, by its metadata).
type EnvRecord struct {
	Kind     string   `json:"kind"`
	Schema   string   `json:"schema,omitempty"`
	Phase    string   `json:"phase,omitempty"`
	Complete bool     `json:"complete,omitempty"`
	Code     string   `json:"code,omitempty"`
	Roots    []string `json:"roots,omitempty"`
	Records  int      `json:"records,omitempty"`
	Scope    string   `json:"scope,omitempty"`
	Path     string   `json:"path,omitempty"`
	Class    string   `json:"class,omitempty"`
	Method   string   `json:"method,omitempty"`
	Type     string   `json:"type,omitempty"`
	Mode     uint32   `json:"mode,omitempty"`
	Size     int64    `json:"size,omitempty"`
	MtimeNS  int64    `json:"mtime_ns,omitempty"`
	CtimeNS  int64    `json:"ctime_ns,omitempty"`
	Dev      uint64   `json:"dev,omitempty"`
	Ino      uint64   `json:"ino,omitempty"`
	SHA256   string   `json:"sha256,omitempty"`
	// Oversized marks a named configuration file over 1 MiB, fingerprinted
	// by metadata only (A3.3).
	Oversized     bool   `json:"oversized,omitempty"`
	Target        string `json:"target,omitempty"`
	GitDir        string `json:"git_dir,omitempty"`
	CommonDir     string `json:"common_dir,omitempty"`
	Head          string `json:"head,omitempty"`
	Branch        string `json:"branch,omitempty"`
	Bare          bool   `json:"bare,omitempty"`
	StatusSHA256  string `json:"status_sha256,omitempty"`
	TrackedSHA256 string `json:"tracked_sha256,omitempty"`
	Depth         int    `json:"depth,omitempty"`
}

// Record kinds and comparison methods.
const (
	RecHeader  = "header"
	RecHome    = "home"
	RecRoot    = "root"
	RecEntry   = "entry"
	RecLink    = "link"
	RecRepo    = "repo"
	RecInvalid = "invalid_repo"

	MethodContent  = "content"
	MethodMetadata = "metadata"
	MethodIdentity = "identity"
	MethodAbsent   = "absent"
)

// Entry types.
const (
	TypeFile    = "file"
	TypeDir     = "dir"
	TypeSymlink = "symlink"
	TypeOther   = "other"
	TypeAbsent  = "absent"
)

// Snapshot is one parsed or collected environment snapshot. A collected
// snapshot holds the full Records (kept by the supervisor in memory, never
// written as such); a parsed one holds only the stored Compact records.
type Snapshot struct {
	Phase    string
	Complete bool
	Code     string
	Roots    []string
	// Depth is the discovery depth below each root (--protect-depth).
	Depth   int
	Records []EnvRecord
	Compact []EnvCompact
}

// EnvCompact is one stored snapshot record (A3.4): the record's key, the
// SHA-256 of its full record's canonical JSON (every metadata field and,
// where content is hashed, the content digest), that content digest, and
// the oversized-configuration mark. A3.6: a vendor entry's record also
// carries its class and type (required for entries, absent otherwise), so
// a summary needs no full detail.
type EnvCompact struct {
	Kind      string `json:"k"`
	Scope     string `json:"s,omitempty"`
	Path      string `json:"p"`
	Digest    string `json:"h"`
	Content   string `json:"c,omitempty"`
	Oversized bool   `json:"o,omitempty"`
	Class     string `json:"class,omitempty"`
	Type      string `json:"type,omitempty"`
}

// snapshotHeader is a snapshot's first stored line.
type snapshotHeader struct {
	Kind     string   `json:"kind"`
	Schema   string   `json:"schema"`
	Phase    string   `json:"phase"`
	Complete bool     `json:"complete"`
	Code     string   `json:"code,omitempty"`
	Roots    []string `json:"roots"`
	Depth    int      `json:"depth"`
	Records  int      `json:"records"`
}

// EnvDetail is the full before and after record of one entry that differs
// between the snapshots (nil for the side where it is absent), with its
// change kind.
type EnvDetail struct {
	Change string     `json:"change"`
	Before *EnvRecord `json:"before"`
	After  *EnvRecord `json:"after"`
}

// compactOf returns r's stored compact record.
func compactOf(r EnvRecord) EnvCompact {
	b, _ := json.Marshal(r)
	sum := sha256.Sum256(b)
	c := EnvCompact{Kind: r.Kind, Scope: r.Scope, Path: r.Path, Digest: hex.EncodeToString(sum[:]), Oversized: r.Oversized}
	if r.Method == MethodContent {
		c.Content = r.SHA256
	}
	if r.Kind == RecEntry {
		c.Class, c.Type = r.Class, r.Type
	}
	return c
}

// compactRecords returns the compact records of s: computed from its full
// records when it has them, else the parsed ones.
func (s *Snapshot) compactRecords() []EnvCompact {
	if s.Records == nil {
		return s.Compact
	}
	out := make([]EnvCompact, 0, len(s.Records))
	for _, r := range s.Records {
		out = append(out, compactOf(r))
	}
	return out
}

// EnvChange is one difference.
type EnvChange struct {
	Scope      string   `json:"scope"`
	Path       string   `json:"path"`
	Class      string   `json:"class"`
	Kind       string   `json:"kind"`
	Fields     []string `json:"fields"`
	BeforeSize *int64   `json:"before_size"`
	AfterSize  *int64   `json:"after_size"`
	BeforeType string   `json:"before_type"`
	AfterType  string   `json:"after_type"`
	BeforeMode uint32   `json:"before_mode"`
	AfterMode  uint32   `json:"after_mode"`
}

// Change kinds.
const (
	ChangeAdded    = "added"
	ChangeModified = "modified"
	ChangeDeleted  = "deleted"
	ChangeReplaced = "replaced"
	ChangeMode     = "mode_changed"
)

// EnvDiff is environment/diff.json.
type EnvDiff struct {
	Schema         string      `json:"schema"`
	ProtectedRoots []string    `json:"protected_roots"`
	Before         []FileHash  `json:"before"`
	After          []FileHash  `json:"after"`
	Complete       bool        `json:"complete"`
	Codes          []string    `json:"codes"`
	Protected      []EnvChange `json:"protected"`
	// ToolSummary counts the differing tool-maintained entries per vendor
	// top-level directory, from the complete compact inventories (A3.6);
	// ToolDetailCount of them have full detail, ToolOmittedCount do not.
	ToolSummary      []ToolSummaryRow `json:"tool_maintained_summary"`
	ToolDetailCount  int              `json:"tool_maintained_detail_count"`
	ToolOmittedCount int              `json:"tool_maintained_omitted_count"`
	// OversizedConfig names the named configuration files over 1 MiB that
	// either snapshot fingerprinted by metadata only (A3.3), sorted.
	OversizedConfig []string `json:"oversized_config"`
	// Detail holds, sorted by kind, scope and path, the full before and
	// after records of every differing entry that is not tool-maintained
	// and of the first ToolDetailCount differing tool-maintained ones
	// (A3.4, A3.6); Protected is derived from it alone.
	Detail []EnvDetail `json:"detail"`
}

// ToolSummaryRow is one vendor top-level directory's tool-maintained
// differences; "." is the vendor home's immediate non-directory entries.
type ToolSummaryRow struct {
	Scope    string `json:"scope"`
	TopLevel string `json:"top_level"`
	Added    int    `json:"added"`
	Changed  int    `json:"changed"`
	Deleted  int    `json:"deleted"`
	Total    int    `json:"total"`
}

// envFiles are a snapshot phase's two fixed shard names.
func envFiles(phase string) []string {
	return []string{"environment/" + phase + "-00.jsonl", "environment/" + phase + "-01.jsonl"}
}

// ---- Paths ----

// pathTokenizer renders canonical paths for evidence: $CHECKOUT and $HOME
// component-aware prefixes, otherwise the canonical path.
type pathTokenizer struct {
	home, checkout string
}

// under reports whether p is root or below it (component-aware).
func under(p, root string) bool {
	if root == "" {
		return false
	}
	rel, err := filepath.Rel(root, p)
	return err == nil && filepath.IsAbs(p) && filepath.IsAbs(root) && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

// canonical returns t with canonical prefixes, so a canonical path gets its
// token however the home and checkout were spelled (macOS reaches every
// temporary directory through /var -> /private/var).
func (t pathTokenizer) canonical() pathTokenizer {
	for _, p := range []*string{&t.home, &t.checkout} {
		if *p == "" {
			continue
		}
		if c, err := canonicalPath(*p); err == nil {
			*p = c
		}
	}
	return t
}

// canonicalPath resolves p's symlinks and cleans it; a path that does not
// exist (yet) resolves through its nearest existing ancestor, the missing
// elements appended.
func canonicalPath(p string) (string, error) {
	p = filepath.Clean(p)
	c, err := filepath.EvalSymlinks(p)
	if err == nil {
		return c, nil
	}
	parent := filepath.Dir(p)
	if !errors.Is(err, fs.ErrNotExist) || parent == p {
		return "", err
	}
	if c, err = canonicalPath(parent); err != nil {
		return "", err
	}
	return filepath.Join(c, filepath.Base(p)), nil
}

// rootFrame returns t for the paths of the protected-root frame: the
// supplied roots and everything discovered below them. A prefix whose base
// lies strictly inside a supplied root is dropped, so a root and every
// path below it are spelled from the same base (a checkout under a root is
// the root's raw path, never a $CHECKOUT the checker cannot place inside
// it), and the checker's textual containment and nesting checks hold.
// Vendor-home records keep t.
func (t pathTokenizer) rootFrame(roots []string) pathTokenizer {
	for _, base := range []*string{&t.checkout, &t.home} {
		if slices.ContainsFunc(roots, func(r string) bool { return *base != r && under(*base, r) }) {
			*base = ""
		}
	}
	return t
}

func (t pathTokenizer) token(p string) string {
	switch {
	case t.checkout != "" && under(p, t.checkout):
		return encodePath("$CHECKOUT" + strings.TrimPrefix(p, t.checkout))
	case t.home != "" && under(p, t.home):
		return encodePath("$HOME" + strings.TrimPrefix(p, t.home))
	}
	return encodePath(p)
}

// encodePath encodes a path losslessly as JSON text: valid UTF-8 without
// control characters as is, anything else as "b64:" and its base64.
func encodePath(p string) string {
	if utf8.ValidString(p) && !strings.ContainsFunc(p, func(r rune) bool { return r < 0x20 || r == 0x7f }) && !strings.HasPrefix(p, "b64:") {
		return p
	}
	return "b64:" + base64.StdEncoding.EncodeToString([]byte(p))
}

// tokenStrictlyUnder reports whether token path a lies strictly below b
// (component-aware on the tokenized form).
func tokenStrictlyUnder(a, b string) bool {
	return a != b && strings.HasPrefix(a, strings.TrimSuffix(b, "/")+"/")
}

// ---- Root validation ----

// collectRoots implements a repeatable --protect-root flag.
type collectRoots struct{ vals *[]string }

func (c collectRoots) String() string { return "" }

func (c collectRoots) Set(v string) error {
	*c.vals = append(*c.vals, v)
	return nil
}

// validateRoots canonicalizes the supplied roots: each absolute, an
// existing readable directory, symlinks evaluated and cleaned, duplicates
// removed, sorted bytewise; no root may be a strict ancestor of another
// (component-aware) or equal to or inside the evidence destination or the
// runtime (an ancestor of either is allowed: they are the exact
// exclusions). The evidence and runtime are compared canonically too, the
// evidence through its existing parent when it does not exist yet.
func validateRoots(raw []string, evidence, runtime string) ([]string, error) {
	seen := map[string]bool{}
	var out []string
	for _, r := range raw {
		if !filepath.IsAbs(r) {
			return nil, fmt.Errorf("--protect-root %q is not absolute", r)
		}
		canon, err := filepath.EvalSymlinks(r)
		if err != nil {
			return nil, fmt.Errorf("--protect-root %q does not exist: %v", r, err)
		}
		canon = filepath.Clean(canon)
		st, err := os.Stat(canon)
		if err != nil || !st.IsDir() {
			return nil, fmt.Errorf("--protect-root %q is not a directory", r)
		}
		f, err := os.Open(canon)
		if err == nil {
			_, err = f.ReadDir(1)
			f.Close()
			if errors.Is(err, io.EOF) {
				err = nil
			}
		}
		if err != nil {
			return nil, fmt.Errorf("--protect-root %q is not readable: %v", r, err)
		}
		if !seen[canon] {
			seen[canon] = true
			out = append(out, canon)
		}
	}
	if len(out) == 0 {
		return out, nil
	}
	sort.Strings(out)
	var excl []string
	for _, x := range []string{evidence, runtime} {
		if x == "" {
			continue
		}
		c, err := canonicalPath(x)
		if err != nil {
			return nil, fmt.Errorf("the run's evidence or runtime directory %q cannot be resolved: %v", x, err)
		}
		excl = append(excl, c)
	}
	for i, a := range out {
		for _, b := range out[i+1:] {
			if under(a, b) || under(b, a) {
				return nil, fmt.Errorf("--protect-root %q and %q are nested", a, b)
			}
		}
		for _, x := range excl {
			if under(a, x) {
				return nil, fmt.Errorf("--protect-root %q is the run's own evidence or runtime directory, or inside it", a)
			}
		}
	}
	return out, nil
}

// ---- Ownership classes (A3.6) ----

// strictPath is one exact, home-relative owner-configuration path: name
// itself, or a single basename matching prefix*suffix at the home's top
// level when glob is set.
type strictPath struct {
	name, prefix, suffix string
	glob                 bool
}

// strictPaths are each vendor's owner-authored paths (A3.9: Claude's
// plugins/known_marketplaces.json, whose timestamps Claude refreshes, is
// tool-maintained). A path at or below
// one of them (whatever its type: a directory or link substituted for a
// named file stays protected, and so does every descendant of a listed
// directory, even a new name) is owner-configuration.
var strictPaths = map[string][]strictPath{
	"claude": {{name: "CLAUDE.md"}, {prefix: "settings", suffix: ".json", glob: true}, {name: "agents"}, {name: "skills"}, {name: "commands"},
		{name: "hooks"}, {name: "plugins/installed_plugins.json"}, {name: "plugins/blocklist.json"}, {name: "plugins/config.json"}},
	"codex":  {{prefix: "AGENTS", suffix: ".md", glob: true}, {name: "instructions.md"}, {name: "prompts"}},
	"grok":   {{name: "config.toml"}, {name: "managed_config.toml"}, {name: "settings.json"}, {name: "user-settings.json"}},
	"cursor": {{name: "cli-config.json"}, {name: "mcp.json"}},
}

// structuralAncestors are the strict paths' non-root ancestor
// directories: owner-configuration, so a deletion, replacement or mode
// or identity change fails, while their creation alone does not.
var structuralAncestors = map[string][]string{"claude": {"plugins"}}

// matches reports whether rel is exactly this strict path.
func (sp strictPath) matches(rel string) bool {
	if !sp.glob {
		return rel == sp.name
	}
	return !strings.Contains(rel, "/") && strings.HasPrefix(rel, sp.prefix) && strings.HasSuffix(rel, sp.suffix) && len(rel) >= len(sp.prefix)+len(sp.suffix)
}

// strictUnder reports whether rel is at or below one of vendor's strict
// paths.
func strictUnder(vendor, rel string) bool {
	for p := rel; ; {
		for _, sp := range strictPaths[vendor] {
			if sp.matches(p) {
				return true
			}
		}
		i := strings.LastIndex(p, "/")
		if i < 0 {
			return false
		}
		p = p[:i]
	}
}

// structuralAncestor reports whether rel is one of vendor's strict
// ancestor directories.
func structuralAncestor(vendor, rel string) bool {
	return slices.Contains(structuralAncestors[vendor], rel)
}

// ownerClass is the class of the vendor entry at rel (a slash path below
// the vendor home): owner-configuration at, below or as a structural
// ancestor of a strict path, else tool-maintained, regardless of its
// basename, suffix, type or age.
func ownerClass(vendor, rel string) string {
	if strictUnder(vendor, rel) || structuralAncestor(vendor, rel) {
		return ClassOwner
	}
	return ClassTool
}

// toolKey reports whether a record key is a tool-maintained vendor entry.
func toolKey(k envKey) bool {
	return k.kind == RecEntry && ownerClass(k.scope, decodeToken(k.path)) == ClassTool
}

// topLevel is a tool-maintained entry's summary group: its first path
// component, or "." for an immediate entry that is not a directory on
// either side.
func topLevel(path string, dir bool) string {
	if top, _, nested := strings.Cut(path, "/"); nested || dir {
		return top
	}
	return "."
}

// ---- Collection ----

// envScanner collects one snapshot.
type envScanner struct {
	home    string
	roots   []string
	exclude []string
	git     GitRunner
	tok     pathTokenizer
	// rtok tokenizes the root frame: the roots and what lies below them.
	rtok     pathTokenizer
	clock    Clock
	deadline time.Time
	phase    string
	depth    int

	snap      Snapshot
	bytes     int
	entries   int
	repos     int
	hashed    int64
	repoSeen  map[string]bool
	evidenceR string
	// openDir opens a directory for chunked reading (nil: openDir).
	openDir func(string) (dirReader, error)
}

// dirChunk is the number of directory entries read at a time.
const dirChunk = 256

// dirReader is an open directory read a chunk of entries at a time.
type dirReader interface {
	ReadDir(n int) ([]fs.DirEntry, error)
	Close() error
}

// openDir opens p for chunked reading.
func openDir(p string) (dirReader, error) {
	f, err := os.Open(p)
	if err != nil {
		return nil, err
	}
	return f, nil
}

// errDirBudget and errDirDeadline stop a bounded directory read.
var (
	errDirBudget   = errors.New("directory entries exceed the remaining budget")
	errDirDeadline = errors.New("directory read past the deadline")
)

// readDirBounded reads dir a chunk at a time, failing as soon as more than
// budget entries have been read or over (if set) reports the deadline
// passed, so no directory is read whole before its bound applies. The
// entries are returned sorted by name.
func readDirBounded(open func(string) (dirReader, error), dir string, budget int, over func() bool) ([]fs.DirEntry, error) {
	d, err := open(dir)
	if err != nil {
		return nil, err
	}
	defer d.Close()
	var out []fs.DirEntry
	for {
		if over != nil && over() {
			return nil, errDirDeadline
		}
		chunk, err := d.ReadDir(dirChunk)
		out = append(out, chunk...)
		if len(out) > budget {
			return nil, errDirBudget
		}
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, err
		}
	}
	slices.SortFunc(out, func(a, b fs.DirEntry) int { return strings.Compare(a.Name(), b.Name()) })
	return out, nil
}

// pastDeadline reports the scan's deadline passed.
func (sc *envScanner) pastDeadline() bool {
	return !sc.deadline.IsZero() && sc.clock.Now().After(sc.deadline)
}

// readDir reads dir within the scan's deadline and its remaining entry
// budget, and charges every entry read to that budget at once: entries a
// caller holds unprocessed (siblings of the directories it descends into)
// are already counted when any deeper directory is read, so the scan never
// reads more than the bound plus one chunk in total (C11).
func (sc *envScanner) readDir(dir string) ([]fs.DirEntry, error) {
	open := sc.openDir
	if open == nil {
		open = openDir
	}
	ents, err := readDirBounded(open, dir, maxEnvEntries-sc.entries, sc.pastDeadline)
	switch {
	case errors.Is(err, errDirBudget):
		return nil, sc.fail(CodeEnvLimit)
	case err != nil:
		return nil, sc.fail(CodeEnvUnobserved)
	}
	sc.entries += len(ents)
	return ents, nil
}

// errSnapshotIncomplete stops a scan with the snapshot's code set.
var errSnapshotIncomplete = errors.New("environment snapshot incomplete")

// fail marks the snapshot incomplete with code (the first code wins).
func (sc *envScanner) fail(code string) error {
	if sc.snap.Code == "" {
		sc.snap.Code = code
	}
	sc.snap.Complete = false
	return errSnapshotIncomplete
}

// add appends a record within the entry and byte bounds. Each vendor home
// counts toward the entry bound here; its entries, and the directory
// entries discovery reads, were counted by readDir (A3.1) and are not
// counted again. Repository records are bounded by the repository and
// byte caps. The byte bound is on the stored compact form (A3.4).
func (sc *envScanner) add(r EnvRecord) error {
	if sc.pastDeadline() {
		return sc.fail(CodeEnvUnobserved)
	}
	if r.Kind == RecHome {
		sc.entries++
		if sc.entries > maxEnvEntries {
			return sc.fail(CodeEnvLimit)
		}
	}
	b, _ := json.Marshal(compactOf(r))
	sc.bytes += len(b) + 1
	if sc.bytes > maxSnapshotBytes-4096 {
		return sc.fail(CodeEnvLimit)
	}
	sc.snap.Records = append(sc.snap.Records, r)
	return nil
}

// excluded reports the run's exact evidence subtree or runtime.
func (sc *envScanner) excluded(p string) bool {
	for _, x := range sc.exclude {
		if x != "" && under(p, x) {
			return true
		}
	}
	return false
}

// DefaultProtectDepth is --protect-depth's default (A3.2); maxProtectDepth
// bounds it.
const (
	DefaultProtectDepth = 3
	maxProtectDepth     = 64
)

// takeSnapshot collects the snapshot of phase ("before" or "after") at the
// default discovery depth.
func takeSnapshot(phase, home string, roots, exclude []string, git GitRunner, tok pathTokenizer, clock Clock, deadline time.Time) *Snapshot {
	return takeSnapshotDepth(phase, home, roots, exclude, git, tok, clock, deadline, DefaultProtectDepth)
}

// takeSnapshotDepth collects a snapshot whose discovery descends at most
// depth directory levels below each root. The roots are canonical
// (validateRoots); the home, the exclusions and the tokenizer's prefixes are
// made canonical here, as every path discovery reaches below a root is.
func takeSnapshotDepth(phase, home string, roots, exclude []string, git GitRunner, tok pathTokenizer, clock Clock, deadline time.Time, depth int) *Snapshot {
	if c, err := canonicalPath(home); home != "" && err == nil {
		home = c
	}
	exclude = slices.Clone(exclude)
	for i, x := range exclude {
		if c, err := canonicalPath(x); x != "" && err == nil {
			exclude[i] = c
		}
	}
	sc := &envScanner{home: home, roots: roots, exclude: exclude, git: git, tok: tok.canonical(), clock: clock, deadline: deadline, phase: phase, depth: depth,
		repoSeen: map[string]bool{}}
	sc.rtok = sc.tok.rootFrame(roots)
	sc.snap = Snapshot{Phase: phase, Complete: true, Roots: []string{}, Depth: depth}
	for _, r := range roots {
		sc.snap.Roots = append(sc.snap.Roots, sc.rtok.token(r))
	}
	if err := sc.scanHomes(); err == nil {
		sc.scanRoots()
	}
	return &sc.snap
}

// statRecord fills a record's metadata from an lstat result.
func statRecord(r *EnvRecord, st fs.FileInfo) {
	r.Mode = uint32(st.Mode().Perm()) | uint32(st.Mode()&(fs.ModeSetuid|fs.ModeSetgid|fs.ModeSticky))
	switch {
	case st.Mode().IsRegular():
		r.Type = TypeFile
	case st.IsDir():
		r.Type = TypeDir
	case st.Mode()&fs.ModeSymlink != 0:
		r.Type = TypeSymlink
	default:
		r.Type = TypeOther
	}
	if r.Type != TypeDir {
		r.Size = st.Size()
		r.MtimeNS = st.ModTime().UnixNano()
	}
	if sys, ok := st.Sys().(*syscall.Stat_t); ok {
		r.Dev, r.Ino = uint64(sys.Dev), uint64(sys.Ino)
		if r.Type != TypeDir {
			r.CtimeNS = ctimeNS(sys)
		}
	}
}

// hashFile hashes a regular configuration file within the read budget.
func (sc *envScanner) hashFile(p string, size int64) (string, error) {
	if size > maxConfigFile || sc.hashed+size > maxConfigTotal {
		return "", sc.fail(CodeEnvLimit)
	}
	f, err := os.Open(p)
	if err != nil {
		return "", sc.fail(CodeEnvUnobserved)
	}
	defer f.Close()
	h := sha256.New()
	n, err := io.Copy(h, io.LimitReader(f, maxConfigFile+1))
	sc.hashed += n
	if err != nil || n > maxConfigFile || sc.hashed > maxConfigTotal {
		return "", sc.fail(CodeEnvLimit)
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// scanHomes snapshots the four vendor homes.
func (sc *envScanner) scanHomes() error {
	for _, v := range Vendors {
		p := filepath.Join(sc.home, "."+v)
		st, err := os.Lstat(p)
		rec := EnvRecord{Kind: RecHome, Scope: v, Path: sc.tok.token(p)}
		switch {
		case errors.Is(err, fs.ErrNotExist):
			rec.Type, rec.Method = TypeAbsent, MethodAbsent
			if err := sc.add(rec); err != nil {
				return err
			}
			continue
		case err != nil:
			return sc.fail(CodeEnvUnobserved)
		}
		statRecord(&rec, st)
		rec.Method = MethodIdentity
		root := p
		if rec.Type == TypeSymlink {
			// The vendor home itself is the protected place: a symlinked
			// home's entries are walked at its resolved directory; a dangling
			// home link is recorded as a link with nothing below it (A3.4).
			if rec.Target, err = os.Readlink(p); err != nil {
				return sc.fail(CodeEnvUnobserved)
			}
			if root, err = filepath.EvalSymlinks(p); errors.Is(err, fs.ErrNotExist) {
				root = ""
			} else if err != nil {
				return sc.fail(CodeEnvUnobserved)
			}
		}
		if err := sc.add(rec); err != nil {
			return err
		}
		if root == "" {
			continue
		}
		if err := sc.walkVendor(v, root); err != nil {
			return err
		}
	}
	return nil
}

// walkVendor records every entry below a vendor home, deterministically:
// lexical pre-order, directories entered but links never followed, each
// directory read within the remaining entry budget (C11).
func (sc *envScanner) walkVendor(vendor, root string) error {
	type item struct{ abs, rel string }
	var entries []item
	var walk func(dir, rel string) error
	walk = func(dir, rel string) error {
		ents, err := sc.readDir(dir)
		if err != nil {
			return err
		}
		for _, e := range ents {
			p := filepath.Join(dir, e.Name())
			if sc.excluded(p) {
				continue
			}
			r := path.Join(rel, e.Name())
			entries = append(entries, item{p, r})
			if e.IsDir() {
				if err := walk(p, r); err != nil {
					return err
				}
			}
		}
		return nil
	}
	if err := walk(root, ""); err != nil {
		return err
	}
	for _, e := range entries {
		if err := sc.vendorEntry(vendor, e.abs, e.rel); err != nil {
			return err
		}
	}
	return nil
}

// vendorEntry records one vendor-home entry.
func (sc *envScanner) vendorEntry(vendor, p, rel string) error {
	st, err := os.Lstat(p)
	if err != nil {
		return sc.fail(CodeEnvUnobserved)
	}
	rec := EnvRecord{Kind: RecEntry, Scope: vendor, Path: encodePath(rel), Class: ownerClass(vendor, rel)}
	statRecord(&rec, st)
	switch rec.Type {
	case TypeDir:
		rec.Method = MethodIdentity
	case TypeFile:
		rec.Method = MethodMetadata
		if rec.Class == ClassOwner {
			if err := sc.configFile(&rec, p); err != nil {
				return err
			}
		}
	case TypeSymlink:
		// A3.4: the link itself (target text and its own metadata), never
		// followed; a dangling link is valid.
		if rec.Target, err = os.Readlink(p); err != nil {
			return sc.fail(CodeEnvUnobserved)
		}
		rec.Method = MethodMetadata
	default:
		rec.Method = MethodIdentity
	}
	return sc.add(rec)
}

// configFile content-hashes an owner-configuration regular file (A3.3):
// within 1 MiB, else it stays metadata-only and is marked oversized.
// Tool-maintained state is never read.
func (sc *envScanner) configFile(rec *EnvRecord, p string) error {
	if rec.Size > maxConfigFile {
		rec.Oversized = true
		return nil
	}
	rec.Method = MethodContent
	var err error
	rec.SHA256, err = sc.hashFile(p, rec.Size)
	return err
}

// rootLink records a symlink that discovery read under a protected root by
// the link itself: its target text and its own metadata, never followed
// (A3.4). Discovery already counted it toward the entry bound.
func (sc *envScanner) rootLink(p string) error {
	st, err := os.Lstat(p)
	if err != nil {
		return sc.fail(CodeEnvUnobserved)
	}
	rec := EnvRecord{Kind: RecLink, Path: sc.rtok.token(p), Class: ClassLink, Method: MethodMetadata}
	statRecord(&rec, st)
	if rec.Type == TypeSymlink {
		if rec.Target, err = os.Readlink(p); err != nil {
			return sc.fail(CodeEnvUnobserved)
		}
	}
	return sc.add(rec)
}

// scanRoots records each supplied root and every repository below it.
func (sc *envScanner) scanRoots() {
	for _, root := range sc.roots {
		st, err := os.Lstat(root)
		rec := EnvRecord{Kind: RecRoot, Path: sc.rtok.token(root), Class: ClassRoot, Method: MethodIdentity}
		if err != nil {
			rec.Type, rec.Method = TypeAbsent, MethodAbsent
			if sc.add(rec) != nil {
				return
			}
			continue
		}
		statRecord(&rec, st)
		if sc.add(rec) != nil || sc.discover(root) != nil {
			return
		}
	}
}

// isBare reports a bare repository directory (HEAD file, objects and refs
// directories, no .git).
func isBare(dir string) bool {
	st, err := os.Lstat(filepath.Join(dir, "HEAD"))
	if err != nil || !st.Mode().IsRegular() {
		return false
	}
	for _, d := range []string{"objects", "refs"} {
		if st, err := os.Lstat(filepath.Join(dir, d)); err != nil || !st.IsDir() {
			return false
		}
	}
	return true
}

// discover walks root (each directory once, at most sc.depth directory
// levels below root; a symlink it reads is recorded as a link and never
// followed, A3.4) and records every repository (A3.1, A3.2): a directory
// holding .git (a
// directory or a gitfile) is one working-tree repository and is not
// entered, its streamed status and tracked-state digests covering every
// change below it (a nested repository the parent does not ignore shows in
// the parent's status: a tracked gitlink as a change, an untracked one as
// "?? dir/"; repo records both as their own repositories; one the parent
// ignores is not separately protected); a bare repository is recorded
// without entering it. Every other directory above and between
// repositories, down to the depth bound, is enumerated, each entry read
// counting toward the entry bound; deeper trees are not walked.
func (sc *envScanner) discover(root string) error {
	type item struct {
		dir   string
		level int
	}
	visited := map[string]bool{}
	stack := []item{{root, 0}}
	for len(stack) > 0 {
		it := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		dir := it.dir
		if visited[dir] || sc.excluded(dir) {
			continue
		}
		visited[dir] = true
		if sc.pastDeadline() {
			return sc.fail(CodeEnvUnobserved)
		}
		if isBare(dir) {
			if err := sc.repo(dir, true); err != nil {
				return err
			}
			continue
		}
		switch _, err := os.Lstat(filepath.Join(dir, ".git")); {
		case err == nil:
			if err := sc.repo(dir, false); err != nil {
				return err
			}
			continue
		case !errors.Is(err, fs.ErrNotExist):
			return sc.fail(CodeEnvUnobserved)
		}
		if it.level >= sc.depth {
			continue
		}
		ents, err := sc.readDir(dir)
		if err != nil {
			return err
		}
		var subdirs []string
		for _, e := range ents {
			p := filepath.Join(dir, e.Name())
			switch {
			case e.Type().IsDir():
				subdirs = append(subdirs, p)
			case e.Type()&fs.ModeSymlink != 0 && !sc.excluded(p):
				if err := sc.rootLink(p); err != nil {
					return err
				}
			}
		}
		for i := len(subdirs) - 1; i >= 0; i-- {
			stack = append(stack, item{subdirs[i], it.level + 1})
		}
	}
	return nil
}

// errGitOutputLimit is a probe whose output passed its bound: an overflow
// of the snapshot's evidence (environment_snapshot_limit), not an
// unreadable repository.
var errGitOutputLimit = errors.New("git probe output exceeds its bound")

// gitText runs a probe whose output is text and whose exit must be ok.
func (sc *envScanner) gitText(dir string, ok []int, args ...string) ([]byte, int, error) {
	out, exit, err := sc.git(dir, nil, append(slices.Clone(gitReadOnlyArgs), args...)...)
	if errors.Is(err, errGitOutputLimit) {
		return nil, exit, sc.fail(CodeEnvLimit)
	}
	if err != nil || !slices.Contains(ok, exit) {
		return nil, exit, sc.fail(CodeEnvUnobserved)
	}
	return out, exit, nil
}

// repo records one repository (dir is the worktree, or the bare gitdir).
func (sc *envScanner) repo(dir string, bare bool) error {
	canon, err := filepath.EvalSymlinks(dir)
	if err != nil {
		return sc.fail(CodeEnvUnobserved)
	}
	if sc.repoSeen[canon] {
		return nil
	}
	sc.repoSeen[canon] = true
	sc.repos++
	if sc.repos > maxEnvRepos {
		return sc.fail(CodeEnvLimit)
	}
	out, exit, err := sc.git(canon, nil, append(slices.Clone(gitReadOnlyArgs), "rev-parse", "--absolute-git-dir", "--git-common-dir")...)
	switch {
	case errors.Is(err, errGitOutputLimit):
		return sc.fail(CodeEnvLimit)
	case err != nil:
		return sc.fail(CodeEnvUnobserved)
	case exit != 0:
		// A3.2: Git rejects this .git; it is recorded by the metadata of
		// its .git entry instead of failing the scan.
		return sc.invalidRepo(canon, bare)
	}
	lines := strings.Split(strings.TrimRight(string(out), "\n"), "\n")
	if len(lines) != 2 {
		return sc.fail(CodeEnvUnobserved)
	}
	common := lines[1]
	if !filepath.IsAbs(common) {
		common = filepath.Join(canon, common)
	}
	rec := EnvRecord{Kind: RecRepo, Path: sc.rtok.token(canon), GitDir: sc.rtok.token(filepath.Clean(lines[0])), CommonDir: sc.rtok.token(filepath.Clean(common)),
		Bare: bare, Class: "repository", Method: MethodContent}
	head, exit, err := sc.gitText(canon, []int{0, 1}, "rev-parse", "-q", "--verify", "HEAD")
	if err != nil {
		return err
	}
	rec.Head = "unborn"
	if exit == 0 {
		rec.Head = strings.TrimSpace(string(head))
	}
	branch, exit, err := sc.gitText(canon, []int{0, 1}, "symbolic-ref", "-q", "HEAD")
	if err != nil {
		return err
	}
	rec.Branch = "detached"
	if exit == 0 {
		rec.Branch = strings.TrimSpace(string(branch))
	}
	var nested []string
	if bare {
		// The refs, streamed into the tracked-state digest.
		h := sha256.New()
		if err := sc.gitStream(canon, h, "for-each-ref", "--format=%(refname)%00%(objectname)"); err != nil {
			return err
		}
		rec.TrackedSHA256, rec.StatusSHA256 = hex.EncodeToString(h.Sum(nil)), sha256Hex(nil)
	} else {
		st := &statusDigest{sc: sc, worktree: canon, h: sha256.New()}
		if err := sc.gitStream(canon, newNULSplitter(st.entry),
			"status", "--porcelain=v1", "-z", "--untracked-files=all", "--ignore-submodules=none"); err != nil {
			return err
		}
		rec.StatusSHA256, nested = hex.EncodeToString(st.h.Sum(nil)), st.nested
		td := &trackedDigest{sc: sc, worktree: canon, h: sha256.New()}
		if err := sc.gitStream(canon, newNULSplitter(td.entry), "ls-files", "--stage", "-z"); err != nil {
			return err
		}
		if td.failed {
			return sc.fail(CodeEnvUnobserved)
		}
		rec.TrackedSHA256 = hex.EncodeToString(td.h.Sum(nil))
		nested = append(nested, td.nested...)
	}
	if err := sc.add(rec); err != nil {
		return err
	}
	for _, child := range nested {
		if err := sc.repo(child, false); err != nil {
			return err
		}
	}
	return nil
}

// gitStream runs a probe streaming its output to out; it must exit 0.
func (sc *envScanner) gitStream(dir string, out io.Writer, args ...string) error {
	_, exit, err := sc.git(dir, out, append(slices.Clone(gitReadOnlyArgs), args...)...)
	if s, ok := out.(*nulSplitter); ok && err == nil {
		err = s.close()
	}
	if err != nil || exit != 0 {
		return sc.fail(CodeEnvUnobserved)
	}
	return nil
}

// invalidRepo records a directory whose .git Git rejects by the metadata
// of that .git entry: type, mode, size, mtime and, for a directory, a
// digest of its sorted child names (A3.2). Any change to it fails.
func (sc *envScanner) invalidRepo(canon string, bare bool) error {
	p := filepath.Join(canon, ".git")
	if bare {
		p = canon
	}
	st, err := os.Lstat(p)
	if err != nil {
		return sc.fail(CodeEnvUnobserved)
	}
	rec := EnvRecord{Kind: RecInvalid, Path: sc.rtok.token(canon), Class: "invalid_repository", Method: MethodMetadata, Bare: bare}
	statRecord(&rec, st)
	rec.Size, rec.MtimeNS = st.Size(), st.ModTime().UnixNano()
	if rec.Type == TypeDir {
		ents, err := sc.readDir(p)
		if err != nil {
			return err
		}
		h := sha256.New()
		for _, e := range ents {
			h.Write([]byte(e.Name() + "\x00"))
		}
		rec.SHA256 = hex.EncodeToString(h.Sum(nil))
	}
	if rec.Type == TypeSymlink {
		rec.Target, _ = os.Readlink(p)
	}
	return sc.add(rec)
}

// maxStreamEntry bounds one NUL-delimited entry of a streamed probe.
const maxStreamEntry = 1 << 20

// errStreamEntry is a streamed entry over its bound.
var errStreamEntry = errors.New("a streamed git entry exceeds its bound")

// nulSplitter splits a streamed output into NUL-terminated entries and
// hands each to fn, holding at most one bounded entry in memory.
type nulSplitter struct {
	fn   func(entry string)
	buf  []byte
	over bool
}

func newNULSplitter(fn func(string)) *nulSplitter { return &nulSplitter{fn: fn} }

func (s *nulSplitter) Write(p []byte) (int, error) {
	for _, c := range p {
		if c == 0 {
			if !s.over {
				s.fn(string(s.buf))
			}
			s.buf = s.buf[:0]
			continue
		}
		if len(s.buf) >= maxStreamEntry {
			s.over = true
			continue
		}
		s.buf = append(s.buf, c)
	}
	return len(p), nil
}

// close flushes an unterminated last entry and reports an oversized one.
func (s *nulSplitter) close() error {
	if len(s.buf) > 0 && !s.over {
		s.fn(string(s.buf))
		s.buf = s.buf[:0]
	}
	if s.over {
		return errStreamEntry
	}
	return nil
}

// statusDigest hashes the kept entries of a streamed status (each with its
// NUL; a rename or copy entry with its second path), leaving out entries
// inside the exact exclusions and untracked entries of the worktree's
// top-level .claude/ (A3.7: Claude Code sandbox placeholders), and notes
// untracked nested repositories.
// untrackedClaude prefixes every untracked status entry inside a
// worktree's top-level .claude/ directory.
const untrackedClaude = "?? .claude/"

type statusDigest struct {
	sc       *envScanner
	worktree string
	h        hash.Hash
	pending  string
	nested   []string
}

func (d *statusDigest) entry(e string) {
	if d.pending != "" {
		e, d.pending = d.pending+"\x00"+e, ""
		d.keep(e)
		return
	}
	if e == "" {
		return
	}
	if len(e) > 3 && (e[0] == 'R' || e[0] == 'C') {
		d.pending = e
		return
	}
	d.keep(e)
}

func (d *statusDigest) keep(e string) {
	if strings.HasPrefix(e, untrackedClaude) || (len(e) > 3 && d.sc.excluded(filepath.Join(d.worktree, filepath.FromSlash(e[3:])))) {
		return
	}
	d.h.Write([]byte(e + "\x00"))
	// An untracked, unignored nested repository is one opaque "?? dir/"
	// entry whose inner changes the parent's status cannot show: it is
	// recorded as its own repository (from the status line, never by
	// enumerating the working tree).
	if rel, ok := strings.CutPrefix(e, "?? "); ok && strings.HasSuffix(rel, "/") {
		child := filepath.Join(d.worktree, filepath.FromSlash(strings.TrimSuffix(rel, "/")))
		if _, err := os.Lstat(filepath.Join(child, ".git")); err == nil {
			d.nested = append(d.nested, child)
		}
	}
}

// trackedDigest hashes each streamed ls-files --stage entry with its
// working-tree metadata (type, mode, size, mtime, ctime, device, inode,
// symlink target, or absence), so any tracked-state change alters it. A
// tracked gitlink with a checked-out repository is noted in nested: its
// directory metadata cannot show a change inside an already-dirty
// submodule, so repo records it as its own repository.
type trackedDigest struct {
	sc       *envScanner
	worktree string
	h        hash.Hash
	failed   bool
	nested   []string
}

func (d *trackedDigest) entry(e string) {
	meta, path, ok := strings.Cut(e, "\t")
	if !ok {
		return
	}
	p := filepath.Join(d.worktree, filepath.FromSlash(path))
	if d.sc.excluded(p) {
		return
	}
	t := EnvRecord{Kind: "tracked", Path: encodePath(path), Class: meta}
	st, err := os.Lstat(p)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		t.Type = TypeAbsent
	case err != nil:
		d.failed = true
		return
	default:
		statRecord(&t, st)
		if t.Type == TypeSymlink {
			t.Target, _ = os.Readlink(p)
		}
		if strings.HasPrefix(meta, "160000 ") && t.Type == TypeDir {
			if _, err := os.Lstat(filepath.Join(p, ".git")); err == nil {
				d.nested = append(d.nested, p)
			}
		}
	}
	b, _ := json.Marshal(t)
	d.h.Write(append(b, '\n'))
}

// ---- Shards ----

// encodeSnapshot renders a snapshot as its two shards (header first, then
// one compact record per entry, A3.4), each at most 8 MiB and together at
// most 16 MiB.
func encodeSnapshot(s *Snapshot) ([][]byte, error) {
	recs := s.compactRecords()
	head := snapshotHeader{Kind: RecHeader, Schema: SnapshotSchema, Phase: s.Phase, Complete: s.Complete, Code: s.Code, Roots: s.Roots, Depth: s.Depth,
		Records: len(recs)}
	if head.Roots == nil {
		head.Roots = []string{}
	}
	shards := [][]byte{nil, nil}
	cur, total := 0, 0
	for i := -1; i < len(recs); i++ {
		var r any = head
		if i >= 0 {
			r = recs[i]
		}
		b, err := json.Marshal(r)
		if err != nil {
			return nil, err
		}
		b = append(b, '\n')
		if len(shards[cur])+len(b) > maxShardBytes {
			cur++
			if cur > 1 {
				return nil, errors.New(CodeEnvLimit)
			}
		}
		shards[cur] = append(shards[cur], b...)
		total += len(b)
		if total > maxSnapshotBytes {
			return nil, errors.New(CodeEnvLimit)
		}
	}
	return shards, nil
}

// parseSnapshot strictly decodes a snapshot's shards: a header, then
// compact records with unique keys.
func parseSnapshot(shards [][]byte) (*Snapshot, error) {
	var s *Snapshot
	n := 0
	seen := map[envKey]bool{}
	for _, data := range shards {
		if len(data) > maxShardBytes {
			return nil, errors.New("snapshot shard over 8 MiB")
		}
		for _, line := range strings.SplitAfter(string(data), "\n") {
			if line == "" {
				continue
			}
			if !strings.HasSuffix(line, "\n") {
				return nil, errors.New("snapshot shard ends without a newline")
			}
			text := []byte(strings.TrimSuffix(line, "\n"))
			if s == nil {
				var h snapshotHeader
				if err := decodeStrict(text, &h); err != nil {
					return nil, err
				}
				if h.Kind != RecHeader || h.Schema != SnapshotSchema || (h.Phase != "before" && h.Phase != "after") {
					return nil, errors.New("snapshot has no version 3 header")
				}
				s = &Snapshot{Phase: h.Phase, Complete: h.Complete, Code: h.Code, Roots: h.Roots, Depth: h.Depth, Compact: []EnvCompact{}}
				n = h.Records
				continue
			}
			var r EnvCompact
			if err := decodeStrict(text, &r); err != nil {
				return nil, fmt.Errorf("snapshot record: %w", err)
			}
			k := envKey{r.Kind, r.Scope, r.Path}
			if r.Kind == RecHeader || seen[k] {
				return nil, errors.New("snapshot has a second header or a repeated record")
			}
			if !hex64RE.MatchString(r.Digest) || (r.Content != "" && !hex64RE.MatchString(r.Content)) {
				return nil, errors.New("snapshot record digest is not SHA-256 hex")
			}
			seen[k] = true
			s.Compact = append(s.Compact, r)
		}
	}
	if s == nil {
		return nil, errors.New("snapshot missing")
	}
	if len(s.Compact) != n {
		return nil, errors.New("snapshot record count mismatch")
	}
	return s, nil
}

// checkInventory verifies a stored snapshot's structure before its
// completeness is believed: exactly one home record per vendor at
// $HOME/.<vendor>, exactly one root record per supplied root, entries
// scoped to a vendor with a clean relative path, and links, repositories
// and invalid repositories unscoped at or below a supplied root (links
// strictly below). Any other kind fails. A3.6: an entry's class is the
// one the checker derives from its vendor and decoded path (never the
// stored claim), its type one of file, dir, symlink and other, and a
// tool-maintained entry carries no content digest; other kinds carry
// neither class nor type.
func checkInventory(s *Snapshot, roots []string) error {
	homes, seenRoots := map[string]bool{}, map[string]bool{}
	for _, c := range s.Compact {
		ok := c.Kind == RecEntry || (c.Class == "" && c.Type == "")
		switch c.Kind {
		case RecHome:
			ok = ok && slices.Contains(Vendors, c.Scope) && c.Path == "$HOME/."+c.Scope && !homes[c.Scope]
			homes[c.Scope] = true
		case RecEntry:
			ok = slices.Contains(Vendors, c.Scope) && cleanRelative(c.Path) && c.Class == ownerClass(c.Scope, decodeToken(c.Path)) &&
				slices.Contains([]string{TypeFile, TypeDir, TypeSymlink, TypeOther}, c.Type) && (c.Class == ClassOwner || (c.Content == "" && !c.Oversized))
		case RecRoot:
			ok = ok && c.Scope == "" && slices.Contains(roots, c.Path) && !seenRoots[c.Path]
			seenRoots[c.Path] = true
		case RecLink, RecRepo, RecInvalid:
			ok = ok && c.Scope == "" && slices.ContainsFunc(roots, func(r string) bool {
				p, root := decodeToken(c.Path), decodeToken(r)
				return tokenStrictlyUnder(p, root) || (c.Kind != RecLink && p == root)
			})
		default:
			ok = false
		}
		if !ok {
			return fmt.Errorf("snapshot record %s %q %q is not part of a valid inventory", c.Kind, c.Scope, c.Path)
		}
	}
	if len(homes) != len(Vendors) || len(seenRoots) != len(roots) {
		return errors.New("snapshot lacks a vendor home or supplied root record")
	}
	return nil
}

// decodeToken returns an evidence path's text (base64-decoded when
// encoded).
func decodeToken(p string) string {
	if b, ok := strings.CutPrefix(p, "b64:"); ok {
		if raw, err := base64.StdEncoding.DecodeString(b); err == nil {
			return string(raw)
		}
	}
	return p
}

// cleanRelative reports a vendor entry path: relative, clean and inside
// the home.
func cleanRelative(p string) bool {
	p = decodeToken(p)
	return p != "" && p != "." && !filepath.IsAbs(p) && filepath.Clean(p) == p && p != ".." && !strings.HasPrefix(p, "../")
}

// ---- Comparison ----

// envKey identifies a record across snapshots.
type envKey struct{ kind, scope, path string }

func keyOf(r EnvRecord) envKey { return envKey{r.Kind, r.Scope, r.Path} }

// size returns a pointer to a size (nil for an absent entry).
func sizeOf(r *EnvRecord) *int64 {
	if r == nil || r.Type == TypeAbsent {
		return nil
	}
	v := r.Size
	return &v
}

// changedFields lists the compared fields that differ.
func changedFields(a, b EnvRecord, metadataOnly bool) []string {
	var f []string
	add := func(name string, diff bool) {
		if diff {
			f = append(f, name)
		}
	}
	add("type", a.Type != b.Type)
	add("mode", a.Mode != b.Mode)
	add("target", a.Target != b.Target)
	add("device", a.Dev != b.Dev)
	add("inode", a.Ino != b.Ino)
	if a.Type != TypeDir || b.Type != TypeDir || a.Kind == RecInvalid {
		add("size", a.Size != b.Size)
		add("mtime", a.MtimeNS != b.MtimeNS)
		add("ctime", a.CtimeNS != b.CtimeNS)
		if !metadataOnly {
			add("content", a.SHA256 != b.SHA256)
		}
	}
	add("head", a.Head != b.Head)
	add("branch", a.Branch != b.Branch)
	add("status", a.StatusSHA256 != b.StatusSHA256)
	add("tracked", a.TrackedSHA256 != b.TrackedSHA256)
	add("git_dir", a.GitDir != b.GitDir)
	add("class", a.Class != b.Class)
	return f
}

// ownerConfigFields keeps the fields that change an owner-configuration
// entry (A3.7): its content, type, mode, link target and class. Device,
// inode and timestamps alone are no change, so a rewrite with identical
// bytes is not one. A file over the content-hash bound has no content
// digest, so its size and mtime remain the only witnesses of its content.
func ownerConfigFields(a, b EnvRecord, fields []string) []string {
	proxy := a.Type == TypeFile && b.Type == TypeFile && (a.Oversized || b.Oversized)
	return slices.DeleteFunc(fields, func(f string) bool {
		switch f {
		case "device", "inode", "ctime":
			return true
		case "size", "mtime":
			return !proxy
		}
		return false
	})
}

// CompareEnvironment returns the sorted protected differences of two
// collected snapshots (full records), the tool-maintained summary and the
// full before and after records of every entry that differs (A3.4, A3.6;
// encodeDiff then fits the optional tool-maintained detail into its
// bound). An incomplete snapshot yields owner_environment_unobserved
// (never an unchanged claim); every protected difference yields
// owner_environment_modified, whatever else failed.
func CompareEnvironment(before, after *Snapshot, roots []string) EnvDiff {
	var detail []EnvDetail
	if before != nil && after != nil {
		detail = changedDetail(before, after)
	}
	return diffFromDetail(before, after, roots, detail)
}

// compactKey is a compact record's key.
func compactKey(c EnvCompact) envKey { return envKey{c.Kind, c.Scope, c.Path} }

// differingKeys returns, sorted by kind, scope and path, the keys whose
// compact records differ or exist in one snapshot only.
func differingKeys(before, after []EnvCompact) []envKey {
	bm, am := map[envKey]EnvCompact{}, map[envKey]EnvCompact{}
	for _, c := range before {
		bm[compactKey(c)] = c
	}
	for _, c := range after {
		am[compactKey(c)] = c
	}
	var keys []envKey
	for k, c := range bm {
		if a, ok := am[k]; !ok || a != c {
			keys = append(keys, k)
		}
	}
	for k := range am {
		if _, ok := bm[k]; !ok {
			keys = append(keys, k)
		}
	}
	sort.Slice(keys, func(i, j int) bool {
		a, b := keys[i], keys[j]
		if a.kind != b.kind {
			return a.kind < b.kind
		}
		if a.scope != b.scope {
			return a.scope < b.scope
		}
		return a.path < b.path
	})
	return keys
}

// changedDetail returns the full records of every entry whose compact
// record differs between two collected snapshots.
func changedDetail(before, after *Snapshot) []EnvDetail {
	full := func(recs []EnvRecord) map[envKey]EnvRecord {
		m := map[envKey]EnvRecord{}
		for _, r := range recs {
			m[keyOf(r)] = r
		}
		return m
	}
	bm, am := full(before.Records), full(after.Records)
	detail := []EnvDetail{}
	for _, k := range differingKeys(before.compactRecords(), after.compactRecords()) {
		var e EnvDetail
		if r, ok := bm[k]; ok {
			e.Before = &r
		}
		if r, ok := am[k]; ok {
			e.After = &r
		}
		detail = append(detail, e)
	}
	return detail
}

// detailKeys are the differing keys a diff's detail must list, in order:
// every key that is not a tool-maintained entry, and the first tools of
// the tool-maintained ones.
func detailKeys(keys []envKey, tools int) []envKey {
	var out []envKey
	n := 0
	for _, k := range keys {
		if !toolKey(k) {
			out = append(out, k)
		} else if n < tools {
			out = append(out, k)
			n++
		}
	}
	return out
}

// verifyDetail checks that a stored diff's detail lists, in order, every
// entry whose stored compact records differ except tool-maintained ones
// beyond a prefix of them (A3.6), each full record hashing to its
// snapshot's compact record and no tool-maintained one holding a content
// digest (so the checker derives every change from evidence bound to the
// snapshots).
func verifyDetail(before, after *Snapshot, detail []EnvDetail) error {
	bc, ac := before.compactRecords(), after.compactRecords()
	tools := 0
	for _, e := range detail {
		for _, r := range []*EnvRecord{e.Before, e.After} {
			if r != nil && toolKey(keyOf(*r)) {
				if r.SHA256 != "" {
					return errors.New("diff detail holds a content digest of tool-maintained state")
				}
			}
		}
		if r := detailRecord(e); r != nil && toolKey(keyOf(*r)) {
			tools++
		}
	}
	keys := detailKeys(differingKeys(bc, ac), tools)
	if len(keys) != len(detail) {
		return fmt.Errorf("diff detail lists %d entries, the snapshots require %d", len(detail), len(keys))
	}
	index := func(cs []EnvCompact) map[envKey]EnvCompact {
		m := map[envKey]EnvCompact{}
		for _, c := range cs {
			m[compactKey(c)] = c
		}
		return m
	}
	bm, am := index(bc), index(ac)
	for i, k := range keys {
		for _, side := range []struct {
			rec *EnvRecord
			m   map[envKey]EnvCompact
		}{{detail[i].Before, bm}, {detail[i].After, am}} {
			c, in := side.m[k]
			if in != (side.rec != nil) || (side.rec != nil && compactOf(*side.rec) != c) {
				return fmt.Errorf("diff detail %d (%s %s %s) does not match its snapshot record", i, k.kind, k.scope, k.path)
			}
		}
	}
	return nil
}

// detailRecord is a detail entry's present record (before, else after).
func detailRecord(e EnvDetail) *EnvRecord {
	if e.Before != nil {
		return e.Before
	}
	return e.After
}

// diffFromDetail derives the diff from the two snapshots' headers and
// compact records and the full records of the entries that differ: the
// one comparison both the supervisor and the offline checker run. The
// tool-maintained summary and counts come from the complete compact
// inventories, the protected changes and change kinds from the detail.
func diffFromDetail(before, after *Snapshot, roots []string, detail []EnvDetail) EnvDiff {
	d := EnvDiff{Schema: DiffSchema, ProtectedRoots: slices.Clone(roots), Complete: true, Codes: []string{}, Protected: []EnvChange{},
		ToolSummary: []ToolSummaryRow{}, OversizedConfig: []string{}, Detail: []EnvDetail{}}
	oversized := func(scope, path string, over bool) {
		if n := scope + "/" + path; over && !slices.Contains(d.OversizedConfig, n) {
			d.OversizedConfig = append(d.OversizedConfig, n)
		}
	}
	for _, s := range []*Snapshot{before, after} {
		switch {
		case s == nil:
		case s.Records != nil:
			for _, r := range s.Records {
				oversized(r.Scope, r.Path, r.Oversized)
			}
		default:
			for _, r := range s.Compact {
				oversized(r.Scope, r.Path, r.Oversized)
			}
		}
	}
	sort.Strings(d.OversizedConfig)
	if d.ProtectedRoots == nil {
		d.ProtectedRoots = []string{}
	}
	codes := map[string]bool{}
	if before == nil || after == nil || !before.Complete || !after.Complete {
		d.Complete = false
		codes[CodeEnvUnobserved] = true
		for _, s := range []*Snapshot{before, after} {
			if s != nil && s.Code == CodeEnvLimit {
				codes[CodeEnvLimit] = true
			}
		}
	}
	if before != nil && after != nil {
		if !slices.Equal(before.Roots, after.Roots) || !slices.Equal(before.Roots, d.ProtectedRoots) || before.Depth != after.Depth {
			codes[CodeEnvUnobserved] = true
			d.Complete = false
		}
		var tools int
		d.ToolSummary, tools = toolSummary(before.compactRecords(), after.compactRecords())
		for _, e := range detail {
			ch, protected := classifyChange(e.Before, e.After)
			e.Change = ch.Kind
			d.Detail = append(d.Detail, e)
			if r := detailRecord(e); r != nil && toolKey(keyOf(*r)) {
				d.ToolDetailCount++
			} else if protected {
				d.Protected = append(d.Protected, ch)
			}
		}
		d.ToolOmittedCount = tools - d.ToolDetailCount
		sortChanges(d.Protected)
	} else if detail != nil {
		d.Detail = detail
	}
	if len(d.Protected) > 0 {
		codes[CodeEnvModified] = true
	}
	for c := range codes {
		d.Codes = append(d.Codes, c)
	}
	sort.Strings(d.Codes)
	return d
}

// classifyChange describes one differing record (nil on the side where it
// is absent) and reports whether it is protected: every difference of a
// record that is not a tool-maintained entry is, except the creation of a
// vendor home or of a structural ancestor as a real directory (A3.6); a
// tool-maintained difference of any kind is report-only.
func classifyChange(bp, ap *EnvRecord) (EnvChange, bool) {
	r := bp
	if r == nil {
		r = ap
	}
	if r == nil {
		return EnvChange{Fields: []string{}}, false
	}
	k := keyOf(*r)
	tool := toolKey(k)
	ch := EnvChange{Scope: k.scope, Path: k.path, BeforeSize: sizeOf(bp), AfterSize: sizeOf(ap), Fields: []string{}}
	if bp != nil {
		ch.BeforeType, ch.BeforeMode, ch.Class = bp.Type, bp.Mode, bp.Class
	}
	if ap != nil {
		ch.AfterType, ch.AfterMode = ap.Type, ap.Mode
		if ch.Class == "" {
			ch.Class = ap.Class
		}
	}
	switch {
	case ap == nil:
		ch.Kind = ChangeDeleted
		return ch, !tool && bp.Type != TypeAbsent
	case bp == nil:
		ch.Kind = ChangeAdded
		return ch, !tool && !(k.kind == RecEntry && ap.Type == TypeDir && structuralAncestor(k.scope, decodeToken(k.path)))
	}
	fields := changedFields(*bp, *ap, tool)
	if k.kind == RecEntry && !tool {
		fields = ownerConfigFields(*bp, *ap, fields)
	}
	if len(fields) == 0 {
		ch.Kind = ChangeModified
		return ch, false
	}
	ch.Fields = fields
	switch {
	case k.kind == RecHome && bp.Type == TypeAbsent:
		ch.Kind = ChangeAdded
		return ch, ap.Type != TypeDir
	case slices.Contains(fields, "type") || slices.Contains(fields, "class"):
		ch.Kind = ChangeReplaced
	case modeOnly(fields):
		ch.Kind = ChangeMode
	default:
		ch.Kind = ChangeModified
	}
	return ch, !tool
}

// toolSummary counts the differing tool-maintained entries of two
// complete compact inventories per vendor top-level directory (sorted)
// and returns their total. Each entry counts once, a directory in its own
// group; an existing entry with a differing digest is changed, whatever
// its subtype.
func toolSummary(before, after []EnvCompact) ([]ToolSummaryRow, int) {
	bm, am := map[envKey]EnvCompact{}, map[envKey]EnvCompact{}
	for _, c := range before {
		bm[compactKey(c)] = c
	}
	for _, c := range after {
		am[compactKey(c)] = c
	}
	rows := map[[2]string]*ToolSummaryRow{}
	total := 0
	for _, k := range differingKeys(before, after) {
		if !toolKey(k) {
			continue
		}
		b, inB := bm[k]
		a, inA := am[k]
		top := encodePath(topLevel(decodeToken(k.path), (inB && b.Type == TypeDir) || (inA && a.Type == TypeDir)))
		row := rows[[2]string{k.scope, top}]
		if row == nil {
			row = &ToolSummaryRow{Scope: k.scope, TopLevel: top}
			rows[[2]string{k.scope, top}] = row
		}
		switch {
		case !inB:
			row.Added++
		case !inA:
			row.Deleted++
		default:
			row.Changed++
		}
		row.Total++
		total++
	}
	out := []ToolSummaryRow{}
	for _, r := range rows {
		out = append(out, *r)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Scope != out[j].Scope {
			return out[i].Scope < out[j].Scope
		}
		return out[i].TopLevel < out[j].TopLevel
	})
	return out, total
}

// modeOnly reports a change of the mode alone (a chmod also moves the
// status-change time).
func modeOnly(fields []string) bool {
	for _, f := range fields {
		if f != "mode" && f != "ctime" {
			return false
		}
	}
	return slices.Contains(fields, "mode")
}

func sortChanges(cs []EnvChange) {
	sort.SliceStable(cs, func(i, j int) bool {
		if cs[i].Scope != cs[j].Scope {
			return cs[i].Scope < cs[j].Scope
		}
		if cs[i].Path != cs[j].Path {
			return cs[i].Path < cs[j].Path
		}
		return cs[i].Kind < cs[j].Kind
	})
}

// encodeDiff renders diff.json within its 4 MiB bound (A3.6): every
// mandatory detail record, the summary and the counts always; of the
// tool-maintained detail, the longest key-ordered prefix of whole records
// that keeps the complete document within the bound, the rest counted as
// omitted. It returns the fitted diff; a mandatory part over the bound
// fails.
func encodeDiff(d EnvDiff) ([]byte, EnvDiff, error) {
	var tools []int
	for i, e := range d.Detail {
		if r := detailRecord(e); r != nil && toolKey(keyOf(*r)) {
			tools = append(tools, i)
		}
	}
	all := len(tools) + d.ToolOmittedCount
	keep := func(n int) EnvDiff {
		out := d
		out.Detail = make([]EnvDetail, 0, len(d.Detail)-len(tools)+n)
		t := 0
		for i, e := range d.Detail {
			if t < len(tools) && tools[t] == i {
				t++
				if t > n {
					continue
				}
			}
			out.Detail = append(out.Detail, e)
		}
		out.ToolDetailCount, out.ToolOmittedCount = n, all-n
		return out
	}
	fits := func(n int) ([]byte, bool) {
		b, err := encodeJSON(keep(n))
		return b, err == nil && len(b) <= maxDiffBytes
	}
	b, ok := fits(0)
	if !ok {
		return nil, d, errors.New(CodeEnvLimit)
	}
	// Each record's encoded size at its place in the detail array (its
	// own indented lines, the line break and the separator) gives the
	// estimate; whole-document encodings settle the exact prefix.
	size, n := len(b), 0
	for _, i := range tools {
		e, err := json.MarshalIndent(d.Detail[i], "    ", "  ")
		if err != nil || size+len(e)+6 > maxDiffBytes {
			break
		}
		size += len(e) + 6
		n++
	}
	for ; n > 0; n-- {
		if e, ok := fits(n); ok {
			b = e
			break
		}
	}
	for n < len(tools) {
		e, ok := fits(n + 1)
		if !ok {
			break
		}
		b, n = e, n+1
	}
	return b, keep(n), nil
}

// ctimeNS returns a stat result's status-change time in nanoseconds
// (Ctim on Linux, Ctimespec on macOS; one untagged file serves both).
func ctimeNS(st *syscall.Stat_t) int64 {
	v := reflect.ValueOf(st).Elem()
	for _, name := range []string{"Ctim", "Ctimespec"} {
		if f := v.FieldByName(name); f.IsValid() {
			return f.FieldByName("Sec").Int()*int64(time.Second) + f.FieldByName("Nsec").Int()
		}
	}
	return 0
}
