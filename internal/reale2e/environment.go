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
	"regexp"
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
// Owner configuration (the default for every vendor entry) is
// content-hashed with its metadata and must not change; the exact vendor
// housekeeping classes are compared by metadata only and reported, never
// failed (the two credential files admit "modified" only). Repositories
// are compared by HEAD, branch, raw status and a tracked-file manifest.
// The only exclusions are this run's exact evidence subtree and runtime.
// Every bound is enforced while collecting; overflow, read errors and
// timeouts make the proof incomplete, never sampled. A3.4: a symlink in a
// vendor home or under a protected root is fingerprinted as the link itself
// (its target text and its own metadata) and never followed, so a dangling
// link is valid; the evidence snapshots hold one compact record per entry
// (path and a SHA-256 of its full record), the supervisor keeps the full
// records in memory, and diff.json carries the full before and after
// records of exactly the entries that differ.

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

// Entry classes.
const (
	ClassConfig    = "config"
	ClassDirectory = "directory"
	ClassRoot      = "root"
	// ClassLink is a symlink under a protected root (A3.4).
	ClassLink = "symlink"
)

// modifiedOnlyClasses admit change kind "modified" only: the two
// credential files (refreshed in place; a new file identity is a
// replacement) and Codex's app-server control link (A3.5; retargeting
// creates a new link, so its identity may change).
var modifiedOnlyClasses = map[string]bool{"claude.credentials": true, "codex.auth": true, classCodexControl: true}

// classCodexControl is the exact Codex app-server control link (A3.5).
const (
	classCodexControl = "codex.app_server_control"
	codexControlPath  = "app-server-control/app-server-control.sock"
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
// the oversized-configuration mark.
type EnvCompact struct {
	Kind      string `json:"k"`
	Scope     string `json:"s,omitempty"`
	Path      string `json:"p"`
	Digest    string `json:"h"`
	Content   string `json:"c,omitempty"`
	Oversized bool   `json:"o,omitempty"`
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
// between the snapshots (nil for the side where it is absent).
type EnvDetail struct {
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
	Housekeeping   []EnvChange `json:"housekeeping"`
	// OversizedConfig names the named configuration files over 1 MiB that
	// either snapshot fingerprinted by metadata only (A3.3), sorted.
	OversizedConfig []string `json:"oversized_config"`
	// Detail holds the full before and after records of exactly the
	// entries whose compact records differ, sorted by kind, scope and path
	// (A3.4); Protected and Housekeeping are derived from it alone.
	Detail []EnvDetail `json:"detail"`
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
// exclusions).
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
	sort.Strings(out)
	for i, a := range out {
		for _, b := range out[i+1:] {
			if under(a, b) || under(b, a) {
				return nil, fmt.Errorf("--protect-root %q and %q are nested", a, b)
			}
		}
		for _, x := range []string{evidence, runtime} {
			if x != "" && under(a, x) {
				return nil, fmt.Errorf("--protect-root %q is the run's own evidence or runtime directory, or inside it", a)
			}
		}
	}
	return out, nil
}

// ---- Housekeeping classes ----

var (
	idRE   = regexp.MustCompile(`^[A-Za-z0-9-]+$`)
	uuidRE = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)
	yearRE = regexp.MustCompile(`^[0-9]{4}$`)
	twoRE  = regexp.MustCompile(`^[0-9]{2}$`)
)

// component reports a nonempty single filename component (never . or ..).
func component(s string) bool { return s != "" && s != "." && s != ".." && !strings.Contains(s, "/") }

// glob reports whether name is prefix + a nonempty component + suffix.
func glob(name, prefix, suffix string) bool {
	if !strings.HasPrefix(name, prefix) || !strings.HasSuffix(name, suffix) || len(name) <= len(prefix)+len(suffix) {
		return false
	}
	return component(name[len(prefix) : len(name)-len(suffix)])
}

// housekeepingClass returns the exact housekeeping class of a regular
// file at rel (slash path) under vendor's home, or "" (owner
// configuration). debug/latest symlinks are classified by the snapshot.
func housekeepingClass(vendor, rel string) string {
	p := strings.Split(rel, "/")
	n := len(p)
	switch vendor {
	case "claude":
		switch {
		case rel == "history.jsonl":
			return "claude.history"
		case rel == ".credentials.json":
			return "claude.credentials"
		case n == 2 && p[0] == "shell-snapshots" && glob(p[1], "snapshot-", ".sh"):
			return "claude.shell_snapshot"
		case n == 2 && p[0] == "todos" && glob(p[1], "", ".json"):
			return "claude.todo"
		case n == 2 && p[0] == "statsig" && component(p[1]):
			return "claude.statsig"
		case n == 3 && p[0] == "projects" && component(p[1]) && strings.HasSuffix(p[2], ".jsonl") && idRE.MatchString(strings.TrimSuffix(p[2], ".jsonl")):
			return "claude.project_transcript"
		case n == 5 && p[0] == "projects" && component(p[1]) && idRE.MatchString(p[2]) && p[3] == "subagents" &&
			strings.HasPrefix(p[4], "agent-") && strings.HasSuffix(p[4], ".jsonl") && idRE.MatchString(strings.TrimSuffix(strings.TrimPrefix(p[4], "agent-"), ".jsonl")):
			return "claude.subagent_transcript"
		case n == 2 && p[0] == "debug" && strings.HasSuffix(p[1], ".txt") && idRE.MatchString(strings.TrimSuffix(p[1], ".txt")):
			return "claude.debug"
		}
	case "codex":
		switch {
		case rel == "history.jsonl":
			return "codex.history"
		case rel == "auth.json":
			return "codex.auth"
		case n == 5 && p[0] == "sessions" && yearRE.MatchString(p[1]) && twoRE.MatchString(p[2]) && twoRE.MatchString(p[3]) && rolloutName(p[4]):
			return "codex.session"
		case n == 2 && (p[0] == "logs" || p[0] == "log") && glob(p[1], "", ".log"):
			return "codex.log"
		}
	case "grok":
		switch {
		case rel == "history.jsonl":
			return "grok.history"
		case n == 2 && p[0] == "sessions" && (idSuffixed(p[1], ".json") || idSuffixed(p[1], ".jsonl")):
			return "grok.session"
		case n == 3 && p[0] == "sessions" && idRE.MatchString(p[1]) && (glob(p[2], "", ".jsonl") || glob(p[2], "", ".log")):
			return "grok.session"
		case n == 2 && (p[0] == "logs" || p[0] == "log") && glob(p[1], "", ".log"), rel == "grok.log":
			return "grok.log"
		}
	}
	return ""
}

// idSuffixed reports "<id>" + suffix.
func idSuffixed(name, suffix string) bool {
	return strings.HasSuffix(name, suffix) && idRE.MatchString(strings.TrimSuffix(name, suffix))
}

// rolloutName reports "rollout-*-<uuid>.jsonl".
func rolloutName(name string) bool {
	if !strings.HasPrefix(name, "rollout-") || !strings.HasSuffix(name, ".jsonl") {
		return false
	}
	mid := strings.TrimSuffix(strings.TrimPrefix(name, "rollout-"), ".jsonl")
	if len(mid) < 38 || mid[len(mid)-37] != '-' {
		return false
	}
	return uuidRE.MatchString(mid[len(mid)-36:]) && component(mid[:len(mid)-37])
}

// ---- Collection ----

// envScanner collects one snapshot.
type envScanner struct {
	home     string
	roots    []string
	exclude  []string
	git      GitRunner
	tok      pathTokenizer
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
// depth directory levels below each root.
func takeSnapshotDepth(phase, home string, roots, exclude []string, git GitRunner, tok pathTokenizer, clock Clock, deadline time.Time, depth int) *Snapshot {
	sc := &envScanner{home: home, roots: roots, exclude: exclude, git: git, tok: tok, clock: clock, deadline: deadline, phase: phase, depth: depth,
		repoSeen: map[string]bool{}}
	sc.snap = Snapshot{Phase: phase, Complete: true, Roots: []string{}, Depth: depth}
	for _, r := range roots {
		sc.snap.Roots = append(sc.snap.Roots, tok.token(r))
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
		if err := sc.vendorEntry(vendor, root, e.abs, e.rel); err != nil {
			return err
		}
	}
	return nil
}

// vendorEntry records one vendor-home entry.
func (sc *envScanner) vendorEntry(vendor, root, p, rel string) error {
	st, err := os.Lstat(p)
	if err != nil {
		return sc.fail(CodeEnvUnobserved)
	}
	rec := EnvRecord{Kind: RecEntry, Scope: vendor, Path: encodePath(rel)}
	statRecord(&rec, st)
	switch rec.Type {
	case TypeDir:
		rec.Class, rec.Method = ClassDirectory, MethodIdentity
	case TypeFile:
		if c := housekeepingClass(vendor, rel); c != "" {
			rec.Class, rec.Method = c, MethodMetadata
		} else if err := sc.configFile(&rec, vendor, rel, p); err != nil {
			return err
		}
	case TypeSymlink:
		// A3.4: the link itself (target text and its own metadata), never
		// followed; a dangling link is valid.
		if rec.Target, err = os.Readlink(p); err != nil {
			return sc.fail(CodeEnvUnobserved)
		}
		rec.Class, rec.Method = ClassConfig, MethodMetadata
		switch {
		case vendor == "claude" && rel == "debug/latest" && sc.validDebugLatest(root, rec.Target):
			rec.Class = "claude.debug_latest"
		case vendor == "codex" && rel == codexControlPath:
			rec.Class = classCodexControl
		}
	default:
		rec.Class, rec.Method = ClassConfig, MethodIdentity
	}
	return sc.add(rec)
}

// configFile fingerprints a non-housekeeping regular file (A3.3): by
// metadata always, by content only when it is a named configuration file
// of at most 1 MiB; a larger named file is metadata-only and marked.
func (sc *envScanner) configFile(rec *EnvRecord, vendor, rel, p string) error {
	rec.Class, rec.Method = ClassConfig, MethodMetadata
	if !namedConfig(vendor, rel) {
		return nil
	}
	if rec.Size > maxConfigFile {
		rec.Oversized = true
		return nil
	}
	rec.Method = MethodContent
	var err error
	rec.SHA256, err = sc.hashFile(p, rec.Size)
	return err
}

// namedConfig reports a vendor's named small configuration file (A3.3),
// the only entries whose content is hashed. Each list is exact:
//
//   - Claude: CLAUDE.md (the user's instructions), settings*.json at the
//     home's top level (user and local settings), every file below
//     agents/, skills/, commands/ and hooks/ (the user's agents, skills,
//     slash commands and hook scripts), and the plugin configuration
//     plugins/{installed_plugins,known_marketplaces,blocklist,config}.json
//     (never the plugin code and marketplace caches beside them).
//   - Codex: config.toml (its configuration) and its instruction files
//     AGENTS.md, AGENTS.override.md, instructions.md and every file below
//     prompts/ (custom prompts).
//   - Grok: config.toml and managed_config.toml (CLI and managed
//     configuration), settings.json and user-settings.json.
//   - Cursor: cli-config.json (CLI configuration) and mcp.json (its MCP
//     servers).
//
// Everything else (binaries, package and download caches, databases,
// snapshots, other large trees) is fingerprinted by metadata only.
func namedConfig(vendor, rel string) bool {
	top, _, nested := strings.Cut(rel, "/")
	switch vendor {
	case "claude":
		switch {
		case rel == "CLAUDE.md", !nested && strings.HasPrefix(rel, "settings") && strings.HasSuffix(rel, ".json"):
			return true
		case nested && (top == "agents" || top == "skills" || top == "commands" || top == "hooks"):
			return true
		}
		return slices.Contains([]string{"plugins/installed_plugins.json", "plugins/known_marketplaces.json", "plugins/blocklist.json", "plugins/config.json"}, rel)
	case "codex":
		return slices.Contains([]string{"config.toml", "AGENTS.md", "AGENTS.override.md", "instructions.md"}, rel) || (nested && top == "prompts")
	case "grok":
		return slices.Contains([]string{"config.toml", "managed_config.toml", "settings.json", "user-settings.json"}, rel)
	case "cursor":
		return slices.Contains([]string{"cli-config.json", "mcp.json"}, rel)
	}
	return false
}

// validDebugLatest reports a debug/latest target that names a regular
// debug/<id>.txt of the same home: classification only, by an lstat of
// that exact path; nothing is read through the link (A3.4).
func (sc *envScanner) validDebugLatest(root, target string) bool {
	abs := target
	if !filepath.IsAbs(abs) {
		abs = filepath.Join(root, "debug", target)
	}
	abs = filepath.Clean(abs)
	dir, name := filepath.Split(abs)
	if filepath.Clean(dir) != filepath.Join(root, "debug") || housekeepingClass("claude", "debug/"+name) != "claude.debug" {
		return false
	}
	st, err := os.Lstat(abs)
	return err == nil && st.Mode().IsRegular()
}

// rootLink records a symlink that discovery read under a protected root by
// the link itself: its target text and its own metadata, never followed
// (A3.4). Discovery already counted it toward the entry bound.
func (sc *envScanner) rootLink(p string) error {
	st, err := os.Lstat(p)
	if err != nil {
		return sc.fail(CodeEnvUnobserved)
	}
	rec := EnvRecord{Kind: RecLink, Path: sc.tok.token(p), Class: ClassLink, Method: MethodMetadata}
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
		rec := EnvRecord{Kind: RecRoot, Path: sc.tok.token(root), Class: ClassRoot, Method: MethodIdentity}
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
	rec := EnvRecord{Kind: RecRepo, Path: sc.tok.token(canon), GitDir: sc.tok.token(filepath.Clean(lines[0])), CommonDir: sc.tok.token(filepath.Clean(common)),
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
	rec := EnvRecord{Kind: RecInvalid, Path: sc.tok.token(canon), Class: "invalid_repository", Method: MethodMetadata, Bare: bare}
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
// inside the exact exclusions, and notes untracked nested repositories.
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
	if len(e) > 3 && d.sc.excluded(filepath.Join(d.worktree, filepath.FromSlash(e[3:]))) {
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
					return nil, errors.New("snapshot has no version 2 header")
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
// strictly below). Any other kind fails.
func checkInventory(s *Snapshot, roots []string) error {
	homes, seenRoots := map[string]bool{}, map[string]bool{}
	for _, c := range s.Compact {
		ok := false
		switch c.Kind {
		case RecHome:
			ok = slices.Contains(Vendors, c.Scope) && c.Path == "$HOME/."+c.Scope && !homes[c.Scope]
			homes[c.Scope] = true
		case RecEntry:
			ok = slices.Contains(Vendors, c.Scope) && cleanRelative(c.Path)
		case RecRoot:
			ok = c.Scope == "" && slices.Contains(roots, c.Path) && !seenRoots[c.Path]
			seenRoots[c.Path] = true
		case RecLink, RecRepo, RecInvalid:
			ok = c.Scope == "" && slices.ContainsFunc(roots, func(r string) bool {
				p, root := decodeToken(c.Path), decodeToken(r)
				return tokenStrictlyUnder(p, root) || (c.Kind != RecLink && p == root)
			})
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

// isHousekeeping reports a housekeeping class.
func isHousekeeping(class string) bool {
	return class != "" && class != ClassConfig && class != ClassDirectory && class != ClassRoot && class != ClassLink && class != "repository" &&
		class != "invalid_repository"
}

// CompareEnvironment returns the sorted protected differences and
// report-only housekeeping changes of two collected snapshots (full
// records), with the full before and after records of exactly the entries
// that differ (A3.4). An incomplete snapshot yields
// owner_environment_unobserved (never an unchanged claim); every protected
// difference yields owner_environment_modified, whatever else failed.
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

// verifyDetail checks that a stored diff's detail lists exactly the
// entries whose stored compact records differ, in order, each full record
// hashing to its snapshot's compact record (so the checker derives every
// change from evidence bound to the snapshots).
func verifyDetail(before, after *Snapshot, detail []EnvDetail) error {
	bc, ac := before.compactRecords(), after.compactRecords()
	keys := differingKeys(bc, ac)
	if len(keys) != len(detail) {
		return fmt.Errorf("diff detail lists %d entries, the snapshots differ in %d", len(detail), len(keys))
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

// diffFromDetail derives the diff from the two snapshots' headers and
// compact records and the full records of the entries that differ: the
// one comparison both the supervisor and the offline checker run.
func diffFromDetail(before, after *Snapshot, roots []string, detail []EnvDetail) EnvDiff {
	d := EnvDiff{Schema: DiffSchema, ProtectedRoots: slices.Clone(roots), Complete: true, Codes: []string{}, Protected: []EnvChange{}, Housekeeping: []EnvChange{},
		OversizedConfig: []string{}, Detail: detail}
	if d.Detail == nil {
		d.Detail = []EnvDetail{}
	}
	for _, s := range []*Snapshot{before, after} {
		if s == nil {
			continue
		}
		for _, r := range s.compactRecords() {
			if n := r.Scope + "/" + r.Path; r.Oversized && !slices.Contains(d.OversizedConfig, n) {
				d.OversizedConfig = append(d.OversizedConfig, n)
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
		// Unchanged entries cannot differ; between complete snapshots a
		// deleted (added) directory's descendants are all deleted (added),
		// so the housekeeping scaffolding of the detail alone equals that of
		// the full records.
		var bs, as []EnvRecord
		for _, e := range d.Detail {
			if e.Before != nil {
				bs = append(bs, *e.Before)
			}
			if e.After != nil {
				as = append(as, *e.After)
			}
		}
		compareRecords(&d, bs, as)
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

// compareRecords fills d from two record sets.
func compareRecords(d *EnvDiff, before, after []EnvRecord) {
	bm, am := map[envKey]EnvRecord{}, map[envKey]EnvRecord{}
	var keys []envKey
	for _, r := range before {
		if r.Kind == RecHeader {
			continue
		}
		if _, ok := bm[keyOf(r)]; !ok {
			keys = append(keys, keyOf(r))
		}
		bm[keyOf(r)] = r
	}
	for _, r := range after {
		if r.Kind == RecHeader {
			continue
		}
		if _, ok := bm[keyOf(r)]; !ok {
			if _, seen := am[keyOf(r)]; !seen {
				keys = append(keys, keyOf(r))
			}
		}
		am[keyOf(r)] = r
	}
	scaffoldBefore := scaffolding(before)
	scaffoldAfter := scaffolding(after)
	for _, k := range keys {
		b, inB := bm[k]
		a, inA := am[k]
		var bp, ap *EnvRecord
		if inB {
			bp = &b
		}
		if inA {
			ap = &a
		}
		ch := EnvChange{Scope: k.scope, Path: k.path, BeforeSize: sizeOf(bp), AfterSize: sizeOf(ap), Fields: []string{}}
		if inB {
			ch.BeforeType, ch.BeforeMode, ch.Class = b.Type, b.Mode, b.Class
		}
		if inA {
			ch.AfterType, ch.AfterMode = a.Type, a.Mode
			if ch.Class == "" {
				ch.Class = a.Class
			}
		}
		switch {
		case !inA:
			ch.Kind = ChangeDeleted
			reportOnly := k.kind == RecEntry && ((isHousekeeping(b.Class) && !modifiedOnlyClasses[b.Class]) ||
				(b.Type == TypeDir && scaffoldBefore[envKey{RecEntry, k.scope, k.path}]))
			if b.Type == TypeAbsent {
				continue
			}
			appendChange(d, ch, reportOnly)
		case !inB:
			ch.Kind = ChangeAdded
			reportOnly := k.kind == RecEntry && ((isHousekeeping(a.Class) && !modifiedOnlyClasses[a.Class]) ||
				(a.Type == TypeDir && scaffoldAfter[envKey{RecEntry, k.scope, k.path}]))
			appendChange(d, ch, reportOnly)
		default:
			hk := isHousekeeping(b.Class) && b.Class == a.Class
			fields := changedFields(b, a, hk)
			if len(fields) == 0 {
				continue
			}
			ch.Fields = fields
			switch {
			case slices.Contains(fields, "type") || (slices.Contains(fields, "class") && !hk):
				ch.Kind = ChangeReplaced
				appendChange(d, ch, false)
			case hk && modifiedOnlyClasses[b.Class]:
				// A credential refresh in place or a control-link retarget is
				// report-only; a mode change, or a credential file with a new
				// identity (another file renamed over it), is protected.
				ch.Kind = ChangeModified
				switch {
				case slices.Contains(fields, "mode"):
					ch.Kind = ChangeMode
				case b.Type != TypeSymlink && (slices.Contains(fields, "device") || slices.Contains(fields, "inode")):
					ch.Kind = ChangeReplaced
				}
				appendChange(d, ch, ch.Kind == ChangeModified)
			default:
				ch.Kind = ChangeModified
				if modeOnly(fields) {
					ch.Kind = ChangeMode
				}
				appendChange(d, ch, hk)
			}
		}
	}
	sortChanges(d.Protected)
	sortChanges(d.Housekeeping)
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

// appendChange files a change as report-only housekeeping or protected.
func appendChange(d *EnvDiff, ch EnvChange, reportOnly bool) {
	if reportOnly {
		d.Housekeeping = append(d.Housekeeping, ch)
		return
	}
	d.Protected = append(d.Protected, ch)
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

// scaffolding returns the directory entries that are only necessary
// ancestors of housekeeping files: each has at least one housekeeping
// descendant and every descendant is housekeeping or such a directory.
func scaffolding(recs []EnvRecord) map[envKey]bool {
	children := map[envKey][]EnvRecord{}
	dirs := map[envKey]EnvRecord{}
	for _, r := range recs {
		if r.Kind != RecEntry {
			continue
		}
		if r.Type == TypeDir {
			dirs[keyOf(r)] = r
		}
		if i := strings.LastIndex(r.Path, "/"); i > 0 {
			parent := envKey{RecEntry, r.Scope, r.Path[:i]}
			children[parent] = append(children[parent], r)
		}
	}
	memo := map[envKey]int{} // 1 scaffolding, 2 not
	var visit func(k envKey) bool
	visit = func(k envKey) bool {
		if v, ok := memo[k]; ok {
			return v == 1
		}
		memo[k] = 2
		kids := children[k]
		ok := len(kids) > 0
		for _, c := range kids {
			switch {
			case c.Type == TypeDir:
				ok = ok && visit(keyOf(c))
			case isHousekeeping(c.Class) && !modifiedOnlyClasses[c.Class]:
			default:
				ok = false
			}
		}
		if ok {
			memo[k] = 1
		}
		return ok
	}
	out := map[envKey]bool{}
	for k := range dirs {
		if visit(k) {
			out[k] = true
		}
	}
	return out
}

// encodeDiff renders diff.json within its 4 MiB bound.
func encodeDiff(d EnvDiff) ([]byte, error) {
	b, err := encodeJSON(d)
	if err != nil {
		return nil, err
	}
	if len(b) > maxDiffBytes {
		return nil, errors.New(CodeEnvLimit)
	}
	return b, nil
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
