package mcpqual

// Decoder enrollment (design decoder-enrollment, Enrollment contract and
// Decoder behavior): the checked-in index of reviewed real capture bundles,
// their expected.json oracles, the offline validators and the replay. A
// production Qualified:true registry version is valid only when every
// fixture its evidence names is an indexed bundle whose provenance,
// redaction fixed point, oracle and replay all hold for exactly that
// version, platform and capability set. There is no auto-enroll command:
// production registries stay compiled, and a fabricated manifest is not
// real provenance (the review gate matters).

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"reflect"
	"slices"
	"sort"
	"strings"
	"syscall"
)

// Enrollment locations and schema.
const (
	EnrollmentSchema    = "mcpqual-enrollment-v1"
	EnrollmentRoot      = "tests/testdata/mcp-transcripts"
	EnrollmentIndexPath = EnrollmentRoot + "/index.json"
	// ExpectedName is a bundle's reviewed oracle, outside its capture file
	// inventory and identified by the index.
	ExpectedName = "expected.json"
	// maxIndexBytes bounds the index and an oracle.
	maxIndexBytes = 1 << 20
	// replayHostname is the fixed hostname of the replayed requester
	// grammar check (the grammar result never depends on the live host).
	replayHostname = "replay-host"
)

// EnrollmentIndex is index.json: the sorted enrolled bundles.
type EnrollmentIndex struct {
	Schema  string            `json:"schema"`
	Entries []EnrollmentEntry `json:"entries"`
}

// EnrollmentEntry is one enrolled real fixture.
type EnrollmentEntry struct {
	Client         string `json:"client"`
	Decoder        string `json:"decoder"`
	Version        string `json:"version"`
	Platform       string `json:"platform"`
	Fixture        string `json:"fixture"`
	Bundle         string `json:"bundle"`
	ManifestSHA256 string `json:"manifest_sha256"`
	ExpectedSHA256 string `json:"expected_sha256"`
	// SanitizationSHA256 hashes the bundle's sanitization.json receipt
	// (design decoder-enrollment B2, FP-18). Absent keeps the unmodified
	// export contract of a legacy or test entry; an explicit null, an empty
	// or malformed hash is refused; every enrolled B2 identity requires it.
	SanitizationSHA256 *string `json:"sanitization_sha256,omitempty"`
}

// ExpectedOracle is expected.json: manually reviewed normalized events and
// observations authored from inspected bytes, never generated from a
// decoder's current output.
type ExpectedOracle struct {
	CaseID              string             `json:"case_id"`
	Nonce               string             `json:"nonce"`
	Events              []Event            `json:"events"`
	Terminal            bool               `json:"terminal"`
	Inconclusive        string             `json:"inconclusive"`
	ClientInfo          ExpectedClientInfo `json:"client_info"`
	RequesterCompatible bool               `json:"requester_compatible"`
	ErrorKinds          []string           `json:"error_kinds"`
	Capabilities        []string           `json:"capabilities"`
	CaptureState        string             `json:"capture_state"`
	SourceCaptureSHA256 string             `json:"source_capture_sha256"`
	// SanitizationPolicy is the export policy of a sanitized B2 fixture
	// (design decoder-enrollment B2, FP-18): required and equal to the
	// receipt's policy exactly when the entry has a sanitization receipt,
	// absent for a legacy entry, never an explicit null.
	SanitizationPolicy *string     `json:"sanitization_policy,omitempty"`
	Attestation        Attestation `json:"attestation"`
}

// ExpectedClientInfo is the probe-observed initialize clientInfo.
type ExpectedClientInfo struct {
	Name    string `json:"name"`
	Version string `json:"version"`
}

// Attestation is the owner and reviewer redaction review of every bundle
// file.
type Attestation struct {
	Owner     string `json:"owner"`
	Reviewer  string `json:"reviewer"`
	Policy    string `json:"policy"`
	Omissions int    `json:"omissions"`
}

// The B2 attestation role handles (design decoder-enrollment B2, FP-18):
// public handles, not personal data. Reviewer means the coordinating
// agent's independent re-derivation of the package from the raw evidence;
// Owner means the repository owner's explicit acceptance of that
// independently verified package, not personal CLI execution or manual
// validation of every byte. Knowing the strings is no permission to fill
// them: the oracle's author leaves both empty, and only the verification
// report and the recorded explicit authorization (outside CI) insert them.
const (
	B2OwnerHandle    = "callsheet-owner"
	B2ReviewerHandle = "decoder-enrollment-coordinator"
)

// CheckB2Attestation is the structural rule of a sanitized B2 entry's
// attestation: exactly the two role handles, the capture policy and zero
// omissions. Empty, whitespace-only, placeholder or synthetic strings
// (TODO, TBD, pending, <owner>, <reviewer>, example, test substitutes)
// never satisfy it. Strings are not signatures: their legitimacy is the
// independent report and the explicit authorization.
func CheckB2Attestation(a Attestation) error {
	if a.Owner != B2OwnerHandle || a.Reviewer != B2ReviewerHandle || a.Policy != CaptureRedactionPolicy || a.Omissions != 0 {
		return fmt.Errorf("a sanitized B2 fixture needs the authorized attestation owner %q and reviewer %q under %s with zero omissions "+
			"(an empty, pending or placeholder attestation is never valid)", B2OwnerHandle, B2ReviewerHandle, CaptureRedactionPolicy)
	}
	return nil
}

// VersionKey is the lowercase hex SHA-256 of an exact observed version
// string (no spaces, slashes or lossy aliases in paths).
func VersionKey(version string) string { return sha256Hex([]byte(version)) }

// EnrolledFixtureID is the stable fixture ID of an enrolled real bundle:
// <decoder>/real/<version-key>/<os>-<arch>.
func EnrolledFixtureID(decoder, version, platform string) string {
	return decoder + "/real/" + VersionKey(version) + "/" + strings.Replace(platform, "/", "-", 1)
}

// EnrolledBundlePath is a bundle's repository directory:
// tests/testdata/mcp-transcripts/<client>/<version-key>/<os>-<arch>/<run-id>.
func EnrolledBundlePath(client, version, platform, runID string) string {
	return path.Join(EnrollmentRoot, client, VersionKey(version), strings.Replace(platform, "/", "-", 1), runID)
}

// ParseEnrollmentIndex strictly decodes and validates an index: schema,
// every field present, entries sorted by fixture with no duplicate
// fixture or bundle, and each entry's identity, paths and hashes
// well-formed.
func ParseEnrollmentIndex(b []byte) (*EnrollmentIndex, error) {
	if len(b) > maxIndexBytes {
		return nil, fmt.Errorf("enrollment index: %d bytes exceeds %d", len(b), maxIndexBytes)
	}
	var idx EnrollmentIndex
	if err := decodeStrict(b, &idx); err != nil {
		return nil, fmt.Errorf("enrollment index: %w", err)
	}
	if err := requireFields(b, reflect.TypeOf(idx), "enrollment index"); err != nil {
		return nil, err
	}
	if idx.Schema != EnrollmentSchema || idx.Entries == nil {
		return nil, fmt.Errorf("enrollment index: schema %q and an entries list are required (want %q)", idx.Schema, EnrollmentSchema)
	}
	// The optional receipt hash (design decoder-enrollment B2): absence
	// only denotes a legacy entry, never an explicit null.
	var raw struct {
		Entries []struct {
			Sanitization json.RawMessage `json:"sanitization_sha256"`
		} `json:"entries"`
	}
	if err := json.Unmarshal(b, &raw); err != nil {
		return nil, fmt.Errorf("enrollment index: %w", err)
	}
	for i, e := range raw.Entries {
		if e.Sanitization != nil && string(bytes.TrimSpace(e.Sanitization)) == "null" {
			return nil, fmt.Errorf("enrollment index: entries[%d].sanitization_sha256 is an explicit null (only absence denotes a legacy entry)", i)
		}
	}
	bundles := map[string]bool{}
	for i, e := range idx.Entries {
		if i > 0 && idx.Entries[i-1].Fixture >= e.Fixture {
			return nil, fmt.Errorf("enrollment index: entries are not sorted by fixture or %s is duplicated", e.Fixture)
		}
		if bundles[e.Bundle] {
			return nil, fmt.Errorf("enrollment index: duplicate bundle %s", e.Bundle)
		}
		bundles[e.Bundle] = true
		if err := e.validate(); err != nil {
			return nil, fmt.Errorf("enrollment index: %s: %w", e.Fixture, err)
		}
	}
	return &idx, nil
}

func (e EnrollmentEntry) validate() error {
	decoder, ok := clientDecoders[e.Client]
	runID := path.Base(e.Bundle)
	switch {
	case !ok || e.Decoder != decoder:
		return fmt.Errorf("client %q with decoder %q", e.Client, e.Decoder)
	case strings.TrimSpace(e.Version) == "" || strings.ContainsAny(e.Version, "\r\n"):
		return errors.New("an exact single-line version is required")
	case !platformRe.MatchString(e.Platform):
		return fmt.Errorf("platform %q (want linux/<arch> or darwin/<arch>)", e.Platform)
	case e.Fixture != EnrolledFixtureID(e.Decoder, e.Version, e.Platform):
		return fmt.Errorf("fixture %q is not %s (a synthetic, fake or aliased fixture never enters the index)", e.Fixture, EnrolledFixtureID(e.Decoder, e.Version, e.Platform))
	case !idPattern.MatchString(runID) || e.Bundle != EnrolledBundlePath(e.Client, e.Version, e.Platform, runID):
		return fmt.Errorf("bundle %q is not under %s", e.Bundle, path.Dir(EnrolledBundlePath(e.Client, e.Version, e.Platform, "x")))
	case !hex64.MatchString(e.ManifestSHA256) || !hex64.MatchString(e.ExpectedSHA256):
		return errors.New("manifest_sha256 and expected_sha256 must be SHA-256 hex")
	case e.SanitizationSHA256 != nil && !hex64.MatchString(*e.SanitizationSHA256):
		return errors.New("a present sanitization_sha256 must be SHA-256 hex")
	}
	return nil
}

// ParseExpected strictly decodes an expected.json oracle.
func ParseExpected(b []byte) (*ExpectedOracle, error) {
	if len(b) > maxIndexBytes {
		return nil, fmt.Errorf("expected oracle: %d bytes exceeds %d", len(b), maxIndexBytes)
	}
	var o ExpectedOracle
	if err := decodeStrict(b, &o); err != nil {
		return nil, fmt.Errorf("expected oracle: %w", err)
	}
	if err := requireFields(b, reflect.TypeOf(o), "expected oracle"); err != nil {
		return nil, err
	}
	var raw struct {
		Policy json.RawMessage `json:"sanitization_policy"`
	}
	if err := json.Unmarshal(b, &raw); err != nil {
		return nil, fmt.Errorf("expected oracle: %w", err)
	}
	if raw.Policy != nil && (string(bytes.TrimSpace(raw.Policy)) == "null" || o.SanitizationPolicy == nil || *o.SanitizationPolicy == "") {
		return nil, errors.New("expected oracle: a present sanitization_policy must be a nonempty string (only absence denotes a legacy oracle)")
	}
	return &o, nil
}

// validate checks an initial setup oracle against its capture client:
// a nonempty tool-event oracle for the requested case with the nonce,
// terminal success, no error or inconclusive classification, the observed
// clientInfo with its requester-grammar result, a complete capture,
// demonstrated capabilities and a clean redaction attestation.
func (o *ExpectedOracle) validate(c *CaptureClient) error {
	calls, results := 0, 0
	for _, ev := range o.Events {
		switch {
		case ev.CaseID != o.CaseID:
			return fmt.Errorf("event %s for case %q, not %q", ev.Kind, ev.CaseID, o.CaseID)
		case ev.Kind == KindToolCall:
			calls++
		case ev.Kind == KindToolResult && ev.Nonce == o.Nonce && ev.SafeReason == "":
			results++
		default:
			return fmt.Errorf("event %s (nonce %q) is not a successful setup's tool event", ev.Kind, ev.Nonce)
		}
	}
	p := c.Probe
	switch {
	case len(o.Events) == 0 || calls == 0 || results == 0:
		return errors.New("an empty tool-event oracle (a call and its nonce result are required)")
	case o.CaseID != c.CaseID || o.Nonce != c.Nonce:
		return fmt.Errorf("case %q / nonce %q differ from the capture's %q / %q", o.CaseID, o.Nonce, c.CaseID, c.Nonce)
	case !o.Terminal || o.Inconclusive != "" || len(o.ErrorKinds) > 0:
		return errors.New("an initial setup oracle needs terminal success, no inconclusive reason and no error kind")
	case o.CaptureState != CaptureComplete || c.State != CaptureComplete:
		return fmt.Errorf("capture state %q / %q, want complete", o.CaptureState, c.State)
	case o.ClientInfo.Name == "" || o.ClientInfo.Version == "" || p == nil || p.ClientName == nil || p.ClientVersion == nil ||
		*p.ClientName != o.ClientInfo.Name || *p.ClientVersion != o.ClientInfo.Version:
		return errors.New("the expected clientInfo is missing or differs from the probe's")
	case !hex64.MatchString(o.SourceCaptureSHA256):
		return errors.New("source_capture_sha256 must be SHA-256 hex")
	case strings.TrimSpace(o.Attestation.Owner) == "" || strings.TrimSpace(o.Attestation.Reviewer) == "" || o.Attestation.Policy != CaptureRedactionPolicy || o.Attestation.Omissions != 0:
		return errors.New("the owner and reviewer redaction attestation under " + CaptureRedactionPolicy + " with zero omissions is required")
	}
	if ok, _ := RequesterCompatible(o.ClientInfo.Name, o.ClientInfo.Version, replayHostname); ok != o.RequesterCompatible {
		return fmt.Errorf("requester_compatible %v, the grammar says %v", o.RequesterCompatible, ok)
	}
	return o.demonstrated()
}

// demonstrated rejects an unknown, duplicate or undemonstrated capability:
// each must be shown by the oracle's own events or terminal.
func (o *ExpectedOracle) demonstrated() error {
	kinds := map[string]bool{}
	for _, ev := range o.Events {
		kinds[ev.Kind] = true
	}
	seen := map[string]bool{}
	for _, k := range o.Capabilities {
		ok := false
		switch k {
		case CapToolCall, CapToolResult:
			ok = kinds[k]
		case CapTerminalSuccess:
			ok = o.Terminal && len(o.ErrorKinds) == 0
		default:
			ok = contains(o.ErrorKinds, k)
		}
		if !contains(Capabilities, k) || seen[k] || !ok {
			return fmt.Errorf("capability %q is unknown, repeated or not demonstrated by the oracle", k)
		}
		seen[k] = true
	}
	if len(o.Capabilities) == 0 {
		return errors.New("no capability")
	}
	return nil
}

// Confined reading (code review rounds 1 and 2, C5/C2). A bundle is read
// through a tree of verified directories: each component is checked not
// to be a symbolic link, opened, and the opened handle is compared with a
// fresh no-follow lookup of the same name, so a component replaced
// between its check and its open (by a symbolic link or another file) is
// refused rather than followed. With an os.Root (EnrollmentOptions.Root,
// production) every child is opened relative to its parent's held,
// verified handle, so a later replacement of an ancestor path cannot
// redirect it. Over a plain fs.FS (os.DirFS, os.Root's FS, in-memory test
// trees) opens are by path, so the verified ancestor chain is checked
// again after every open as well.
//
// Platform semantics relied on, the same on Linux and darwin (both build
// os.Root on openat): os.Root opens one component relative to a held
// directory descriptor with O_NOFOLLOW and resolves a symbolic link there
// itself, only within that root, so a link leaving a held directory
// ("..") is refused and one inside it is followed, then refused by the
// post-open lookup; fs.Lstat over os.DirFS and os.Root's FS is lstat
// (fstatat with AT_SYMLINK_NOFOLLOW for os.Root); os.SameFile compares
// st_dev and st_ino; a FIFO opened read-only with O_NONBLOCK returns at
// once (POSIX). The Root path itself is the trust anchor: os.OpenRoot
// follows links among its own ancestors (darwin's /var -> /private/var).
// Only Linux runs here; ci-macos confirms darwin.

// openHook, when set (tests only), runs between a component's no-follow
// check and its open: the seam of the replacement tests.
var openHook func(name string)

// tree is one verified directory of a bundle or repository.
type tree interface {
	// sub opens the verified subdirectory name (one component).
	sub(name string) (tree, error)
	// read reads the verified regular file name (one component) of at
	// most limit bytes.
	read(name string, limit int) ([]byte, error)
	// list returns the directory's entries, sorted by name.
	list() ([]fs.DirEntry, error)
	close()
}

// checkEntry requires a no-follow lookup to be a directory (dir) or a
// regular file, never a symbolic link.
func checkEntry(info fs.FileInfo, name string, dir bool) error {
	switch {
	case info.Mode()&fs.ModeSymlink != 0:
		return fmt.Errorf("%s is a symbolic link", name)
	case dir && !info.IsDir():
		return fmt.Errorf("%s is not a directory", name)
	case !dir && !info.Mode().IsRegular():
		return fmt.Errorf("%s is not a regular file", name)
	}
	return nil
}

// sameFile reports whether two lookups name the same file: by device and
// inode for operating-system files, by their recorded attributes for an
// in-memory tree (whose entries cannot be replaced concurrently).
func sameFile(a, b fs.FileInfo) bool {
	if os.SameFile(a, b) {
		return true
	}
	return a.Sys() == nil && b.Sys() == nil && a.Name() == b.Name() && a.Mode() == b.Mode() && a.Size() == b.Size() && a.ModTime().Equal(b.ModTime())
}

// verifyOpened requires the opened handle, the check before the open and a
// no-follow lookup after it to be one and the same file.
func verifyOpened(name string, pre, opened, post fs.FileInfo, dir bool) error {
	if err := checkEntry(post, name, dir); err != nil {
		return fmt.Errorf("%w (replaced while it was opened)", err)
	}
	if !sameFile(pre, post) || !sameFile(opened, post) {
		return fmt.Errorf("%s was replaced while it was opened (by a symbolic link or another file)", name)
	}
	return nil
}

func readLimited(r io.Reader, name string, limit int) ([]byte, error) {
	b, err := io.ReadAll(io.LimitReader(r, int64(limit)+1))
	if err == nil && len(b) > limit {
		err = fmt.Errorf("%s exceeds %d bytes", name, limit)
	}
	return b, err
}

func preRead(info fs.FileInfo, name string, limit int) error {
	if err := checkEntry(info, name, false); err != nil {
		return err
	}
	if info.Size() > int64(limit) {
		return fmt.Errorf("%s: %d bytes exceeds %d", name, info.Size(), limit)
	}
	return nil
}

// rootTree is a held, verified os.Root directory.
type rootTree struct{ r *os.Root }

func (t *rootTree) sub(name string) (tree, error) {
	pre, err := t.r.Lstat(name)
	if err != nil {
		return nil, err
	}
	if err := checkEntry(pre, name, true); err != nil {
		return nil, err
	}
	if openHook != nil {
		openHook(name)
	}
	s, err := t.r.OpenRoot(name)
	if err != nil {
		return nil, err
	}
	opened, err1 := s.Stat(".")
	post, err2 := t.r.Lstat(name)
	if err := errors.Join(err1, err2); err != nil {
		s.Close()
		return nil, err
	}
	if err := verifyOpened(name, pre, opened, post, true); err != nil {
		s.Close()
		return nil, err
	}
	return &rootTree{s}, nil
}

func (t *rootTree) read(name string, limit int) ([]byte, error) {
	pre, err := t.r.Lstat(name)
	if err != nil {
		return nil, err
	}
	if err := preRead(pre, name, limit); err != nil {
		return nil, err
	}
	if openHook != nil {
		openHook(name)
	}
	// os.Root opens the one component relative to the held parent with
	// O_NOFOLLOW; it follows only a symbolic link inside the root, which
	// the post-open lookup then refuses. O_NONBLOCK keeps a FIFO swapped in
	// at this point from blocking the open (it is then refused as not the
	// checked file); it does not change reads of a regular file.
	f, err := t.r.OpenFile(name, os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	opened, err1 := f.Stat()
	post, err2 := t.r.Lstat(name)
	if err := errors.Join(err1, err2); err != nil {
		return nil, err
	}
	if err := verifyOpened(name, pre, opened, post, false); err != nil {
		return nil, err
	}
	return readLimited(f, name, limit)
}

func (t *rootTree) list() ([]fs.DirEntry, error) {
	f, err := t.r.Open(".")
	if err != nil {
		return nil, err
	}
	defer f.Close()
	es, err := f.ReadDir(-1)
	slices.SortFunc(es, func(a, b fs.DirEntry) int { return strings.Compare(a.Name(), b.Name()) })
	return es, err
}

func (t *rootTree) close() { t.r.Close() }

// chainEntry is one verified ancestor of an fsTree.
type chainEntry struct {
	path string
	info fs.FileInfo
}

// fsTree is a verified directory of an fs.FS, opened by path.
type fsTree struct {
	fsys  fs.FS
	dir   string
	chain []chainEntry
}

func (t *fsTree) join(name string) string {
	if t.dir == "." {
		return name
	}
	return t.dir + "/" + name
}

// recheck requires every verified ancestor to be unchanged.
func (t *fsTree) recheck() error {
	for _, c := range t.chain {
		info, err := fs.Lstat(t.fsys, c.path)
		if err != nil {
			return err
		}
		if err := checkEntry(info, c.path, true); err != nil {
			return fmt.Errorf("%w (an ancestor was replaced)", err)
		}
		if !sameFile(c.info, info) {
			return fmt.Errorf("%s was replaced (by a symbolic link or another directory)", c.path)
		}
	}
	return nil
}

func (t *fsTree) open(name string, dir bool, limit int) (fs.File, fs.FileInfo, error) {
	p := t.join(name)
	pre, err := fs.Lstat(t.fsys, p)
	if err != nil {
		return nil, nil, err
	}
	if dir {
		err = checkEntry(pre, p, true)
	} else {
		err = preRead(pre, p, limit)
	}
	if err != nil {
		return nil, nil, err
	}
	if openHook != nil {
		openHook(name)
	}
	f, err := t.fsys.Open(p)
	if err != nil {
		return nil, nil, err
	}
	opened, err1 := f.Stat()
	post, err2 := fs.Lstat(t.fsys, p)
	if err := errors.Join(err1, err2, t.recheck()); err != nil {
		f.Close()
		return nil, nil, err
	}
	if err := verifyOpened(p, pre, opened, post, dir); err != nil {
		f.Close()
		return nil, nil, err
	}
	return f, pre, nil
}

func (t *fsTree) sub(name string) (tree, error) {
	f, info, err := t.open(name, true, 0)
	if err != nil {
		return nil, err
	}
	f.Close()
	return &fsTree{fsys: t.fsys, dir: t.join(name), chain: append(append([]chainEntry(nil), t.chain...), chainEntry{t.join(name), info})}, nil
}

func (t *fsTree) read(name string, limit int) ([]byte, error) {
	f, _, err := t.open(name, false, limit)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	b, err := readLimited(f, t.join(name), limit)
	if err == nil {
		err = t.recheck()
	}
	return b, err
}

func (t *fsTree) list() ([]fs.DirEntry, error) {
	es, err := fs.ReadDir(t.fsys, t.dir)
	if err == nil {
		err = t.recheck()
	}
	return es, err
}

func (t *fsTree) close() {}

// openPath opens the verified directory p of base, one component at a
// time, closing every intermediate handle; the caller closes the result.
func openPath(base tree, p string) (tree, error) {
	if p == "." || p == "" {
		return base, nil
	}
	if !fs.ValidPath(p) {
		return nil, fmt.Errorf("%s is not a valid root-relative path", p)
	}
	cur := base
	for _, c := range strings.Split(p, "/") {
		next, err := cur.sub(c)
		if cur != base {
			cur.close()
		}
		if err != nil {
			return nil, err
		}
		cur = next
	}
	return cur, nil
}

// readPath reads the file p of base through verified directories.
func readPath(base tree, p string, limit int) ([]byte, error) {
	dir, name := path.Split(p)
	t, err := openPath(base, strings.TrimSuffix(dir, "/"))
	if err != nil {
		return nil, err
	}
	if t != base {
		defer t.close()
	}
	return t.read(name, limit)
}

// CaptureBundle is a validated bundle: its manifest, the manifest's exact
// bytes and every payload's bytes by relative path.
type CaptureBundle struct {
	Manifest      *CaptureManifest
	ManifestBytes []byte
	Files         map[string][]byte
}

// ValidateCaptureBundle validates the capture bundle in dir of fsys (an
// injectable filesystem; the enrollment check of a repository uses a held
// os.Root, EnrollmentOptions.Root): a strict manifest, then exactly its
// listed payloads (plus the named extra top-level files, such as an
// enrolled bundle's expected.json) as regular files with their recorded
// sizes and hashes. It rejects unknown manifest fields, duplicate or
// escaping paths, symbolic links (in any component, also one swapped in
// while it is opened), nonregular files, unexpected extra and missing
// files, and hash or size mismatches.
func ValidateCaptureBundle(fsys fs.FS, dir string, extra ...string) (*CaptureBundle, error) {
	base := &fsTree{fsys: fsys, dir: "."}
	t, err := openPath(base, dir)
	if err != nil {
		return nil, fmt.Errorf("capture bundle %s: %v", dir, err)
	}
	return validateBundle(t, dir, nil, extra...)
}

// bundleFile is a present payload: its verified directory and name.
type bundleFile struct {
	t    tree
	name string
}

// validateBundle validates the bundle in t. mb, when not nil, is the
// manifest's bytes already read (and hash-checked) by the caller: they are
// the manifest validated and used, never a second read of the file (code
// review round 3, C1). Every payload is read once, and the bytes hashed
// are the bytes returned.
func validateBundle(t tree, dir string, mb []byte, extra ...string) (*CaptureBundle, error) {
	fail := func(format string, a ...any) error {
		return fmt.Errorf("capture bundle %s: "+format, append([]any{dir}, a...)...)
	}
	// The structure first: only regular files with allowlisted names, in
	// clients/<known client>/ directories, never a symbolic link.
	present, dirs := map[string]bundleFile{}, map[string]bool{}
	var held []tree
	defer func() {
		for _, h := range held {
			h.close()
		}
	}()
	entry := func(d fs.DirEntry, rel string) error {
		switch {
		case d.Type()&fs.ModeSymlink != 0:
			return fmt.Errorf("%s is a symbolic link", rel)
		case !d.IsDir() && !d.Type().IsRegular():
			return fmt.Errorf("%s is not a regular file", rel)
		}
		return nil
	}
	top, err := t.list()
	if err != nil {
		return nil, fail("%v", err)
	}
	for _, d := range top {
		rel := d.Name()
		if err := entry(d, rel); err != nil {
			return nil, fail("%v", err)
		}
		switch {
		case d.IsDir() && rel == "clients":
			ct, err := t.sub("clients")
			if err != nil {
				return nil, fail("%v", err)
			}
			held = append(held, ct)
			dirs["clients"] = true
			ids, err := ct.list()
			if err != nil {
				return nil, fail("%v", err)
			}
			for _, cd := range ids {
				crel := "clients/" + cd.Name()
				if err := entry(cd, crel); err != nil {
					return nil, fail("%v", err)
				}
				if _, known := clientDecoders[cd.Name()]; !cd.IsDir() || !known {
					return nil, fail("unexpected entry %s", crel)
				}
				cl, err := ct.sub(cd.Name())
				if err != nil {
					return nil, fail("%v", err)
				}
				held = append(held, cl)
				dirs[crel] = true
				files, err := cl.list()
				if err != nil {
					return nil, fail("%v", err)
				}
				for _, f := range files {
					frel := crel + "/" + f.Name()
					if err := entry(f, frel); err != nil {
						return nil, fail("%v", err)
					}
					if _, _, ok := payloadOwner(frel); f.IsDir() || !ok {
						return nil, fail("unexpected extra file %s", frel)
					}
					present[frel] = bundleFile{cl, f.Name()}
				}
			}
		case d.IsDir():
			return nil, fail("unexpected directory %s", rel)
		case rel == CaptureManifestName || rel == CapturePlanName || slices.Contains(extra, rel):
			present[rel] = bundleFile{t, rel}
		default:
			return nil, fail("unexpected extra file %s", rel)
		}
	}
	if _, ok := present[CaptureManifestName]; !ok {
		return nil, fail("no %s", CaptureManifestName)
	}
	if mb == nil {
		var err error
		if mb, err = t.read(CaptureManifestName, MaxEvidenceFileBytes); err != nil {
			return nil, fail("%v", err)
		}
	}
	man, err := ParseCaptureManifest(mb)
	if err != nil {
		return nil, fail("%v", err)
	}
	if len(mb) > man.Limits.ManifestBytes {
		return nil, fail("the manifest's %d bytes exceed its bound %d", len(mb), man.Limits.ManifestBytes)
	}
	listed := map[string]bool{CaptureManifestName: true}
	for _, e := range extra {
		listed[e] = true
	}
	for _, f := range man.Files {
		listed[f.Path] = true
	}
	for p := range present {
		if !listed[p] {
			return nil, fail("unexpected extra file %s", p)
		}
	}
	clients := map[string]bool{}
	for _, c := range man.Clients {
		clients[c.ID] = true
	}
	for d := range dirs {
		if d != "clients" && !clients[path.Base(d)] {
			return nil, fail("unexpected directory %s", d)
		}
	}
	b := &CaptureBundle{Manifest: man, ManifestBytes: mb, Files: map[string][]byte{}}
	for _, f := range man.Files {
		loc, ok := present[f.Path]
		if !ok {
			return nil, fail("listed file %s is missing", f.Path)
		}
		data, err := loc.t.read(loc.name, man.Limits.FileBytes)
		switch {
		case err != nil:
			return nil, fail("listed file %s: %v", f.Path, err)
		case int64(len(data)) != f.Bytes || sha256Hex(data) != f.SHA256:
			return nil, fail("%s: bytes or hash differ from the manifest", f.Path)
		}
		b.Files[f.Path] = data
	}
	for i := range man.Clients {
		if err := replayCaptureObservation(&man.Clients[i], b.Files); err != nil {
			return nil, fail("client %q: %v", man.Clients[i].ID, err)
		}
	}
	return b, nil
}

// replayCaptureObservation recomputes a recorded probe observation (design
// decoder-enrollment B1.5, FP-14) from the retained server events, the
// stream records and the session stage, and requires every recorded member
// to match: a supplied label is never trusted. Retained bytes cut on
// output are not the analyzed bytes; such a record can claim neither the
// terminal observation nor a clean session (the recomputed attestation
// already says so). A legacy record (no observation) has nothing to
// replay and keeps its old rules.
func replayCaptureObservation(c *CaptureClient, files map[string][]byte) error {
	if c.Probe == nil || c.Probe.Observation == nil {
		return nil
	}
	o := c.Probe.Observation
	clean := captureSessionClean(c.ID, c.Session, c.Streams)
	var server CaptureStream
	for _, s := range c.Streams {
		if s.Path == ClientFile(c.ID, FileServerEvents) {
			server = s
		}
	}
	if server.OutputTruncated {
		if o.EndState == EndTerminalWithoutExit || o.CleanSession != clean {
			return errors.New("probe replay: a cut server-events file cannot carry this observation")
		}
		return nil
	}
	got := analyzeProbe(files[ClientFile(c.ID, FileServerEvents)], c.CaseID, server.InputTruncated, clean).obs
	if !o.equal(&got) {
		return fmt.Errorf("probe replay: the recorded observation %s differs from the replayed server events (%s)", observationText(o), observationText(&got))
	}
	return nil
}

// observationText renders an observation's members for an error.
func observationText(o *ProbeObservation) string {
	kind := "null"
	if o.TerminalKind != nil {
		kind = *o.TerminalKind
	}
	return fmt.Sprintf("end_state=%s intact=%v terminal_kind=%s clean_session=%v", o.EndState, o.Intact, kind, o.CleanSession)
}

// RedactionFixedPoint is the automated redaction guard of an enrolled
// file: re-running the capture policy (red) over the file, including the
// decoded data of every transcript wrapper line, must reproduce exactly
// its bytes (and so its hash) with zero omissions, and no truncation or
// omission marker may remain. It is a guard, not proof of absence.
func RedactionFixedPoint(name string, data []byte, red *Redactor) error {
	fail := func(format string, a ...any) error {
		return fmt.Errorf("redaction fixed point %s: "+format, append([]any{name}, a...)...)
	}
	lines := func() [][]byte {
		if len(data) == 0 {
			return nil
		}
		return bytes.Split(bytes.TrimSuffix(data, []byte{'\n'}), []byte{'\n'})
	}
	if len(data) > 0 && !bytes.HasSuffix(data, []byte{'\n'}) && strings.HasSuffix(name, ".jsonl") {
		return fail("a JSONL file must end with a newline")
	}
	switch base := path.Base(name); {
	case base == FileVendorEvents:
		for i, l := range lines() {
			// Sanitized data never holds a backslash run of maxEncodedRun
			// (TranscriptLine withholds it), and wrapping doubles a run: a
			// longer run is never a fixed point, and is not decoded.
			if backslashRun(string(l)) > 2*maxEncodedRun {
				return fail("line %d holds an encoded value beyond the decode budget", i+1)
			}
			var w transcriptWrapper
			if err := decodeStrict(l, &w); err != nil {
				return fail("line %d is not a transcript wrapper (a truncation or omission marker, or malformed): %v", i+1, err)
			}
			clean, ok := red.TranscriptLine([]byte(w.Data))
			switch {
			case !ok:
				return fail("line %d would be omitted by the capture policy", i+1)
			case string(clean) != w.Data:
				return fail("line %d's data is not a fixed point of the capture policy", i+1)
			case !bytes.Equal(wrapLine(w.OffsetNS, clean), l):
				return fail("line %d is not the canonical wrapper encoding", i+1)
			}
		}
	case strings.HasSuffix(base, ".jsonl"):
		for i, l := range lines() {
			if bytes.Equal(l, []byte(`{"truncated":true}`)) || !json.Valid(l) {
				return fail("line %d is a truncation marker or invalid", i+1)
			}
			if !bytes.Equal(red.Line(l), l) {
				return fail("line %d is not a fixed point of the capture policy", i+1)
			}
		}
	case strings.HasSuffix(base, ".json"):
		if !json.Valid(data) || !bytes.Equal(red.Line(data), data) {
			return fail("not a fixed point of the capture policy")
		}
	default:
		if bytes.HasSuffix(data, []byte("[truncated at the evidence limit]\n")) {
			return fail("carries a truncation marker")
		}
		clean, omitted := captureText(red, data)
		if omitted > 0 || !bytes.Equal(clean, data) {
			return fail("not a fixed point of the capture policy (%d lines would be omitted)", omitted)
		}
	}
	return nil
}

// ReplayTranscript loads a vendor-events.jsonl wrapper file, in recorded
// order with its stored offsets, into a Transcript; a truncation or
// omission marker is an error (enrolled files have none).
func ReplayTranscript(b []byte) (Transcript, error) {
	var t Transcript
	if len(b) == 0 {
		return t, nil
	}
	for i, l := range bytes.Split(bytes.TrimSuffix(b, []byte{'\n'}), []byte{'\n'}) {
		var w transcriptWrapper
		if err := decodeStrict(l, &w); err != nil {
			return t, fmt.Errorf("transcript line %d: %w", i+1, err)
		}
		t.Lines = append(t.Lines, Line{OffsetNS: w.OffsetNS, Data: []byte(w.Data)})
	}
	return t, nil
}

// Replay checks an enrolled bundle offline: the exact version's decoder
// over the transcript must produce exactly the oracle's events, its
// terminal and inconclusive state and no error event; the probe events,
// parsed independently, must show the oracle's clientInfo and exactly one
// receipt and completion for the oracle's case. Decoders do not produce
// clientInfo; that assertion belongs to the probe replay. Nothing here
// reads the current time, randomness, an executable, the network or a
// platform-dependent path.
func Replay(reg Registry, e EnrollmentEntry, b *CaptureBundle, o *ExpectedOracle) error {
	decode, _, err := reg.Select(e.Decoder, e.Version)
	if err != nil {
		return err
	}
	t, err := ReplayTranscript(b.Files[ClientFile(e.Client, FileVendorEvents)])
	if err != nil {
		return err
	}
	d := decode(t)
	got, _ := encodeJSON(d.Events)
	want, _ := encodeJSON(o.Events)
	switch {
	case d.Inconclusive != o.Inconclusive || d.Terminal != o.Terminal:
		return fmt.Errorf("replay: terminal %v inconclusive %q, the oracle says %v %q", d.Terminal, d.Inconclusive, o.Terminal, o.Inconclusive)
	case !bytes.Equal(got, want):
		return fmt.Errorf("replay: decoded events %s differ from the oracle's %s", got, want)
	}
	for _, ev := range d.Events {
		if ev.Kind != KindToolCall && ev.Kind != KindToolResult {
			return fmt.Errorf("replay: an error event %s in a setup transcript", ev.Kind)
		}
	}
	return replayProbe(b, e.Client, o)
}

// replayProbe applies capture's complete-observation gate (design
// decoder-enrollment B1.5, FP-14) to the bundle's retained probe events of
// client, with the clean-session attestation recomputed from the recorded
// session: an intact log or the clean terminal observation without exit
// whose unique terminal is the completion, and a recorded observation
// equal to the replayed one. A legacy record (no observation) still needs
// an intact log: it cannot claim the new allowance.
func replayProbe(b *CaptureBundle, client string, o *ExpectedOracle) error {
	c := replayClient(b, client)
	if c == nil {
		return errors.New("probe replay: the bundle has no manifest record of " + client)
	}
	an := analyzeProbe(b.Files[ClientFile(client, FileServerEvents)], o.CaseID, false, captureSessionClean(c.ID, c.Session, c.Streams))
	p := an.capture
	p.Observation = &an.obs
	if c.Probe == nil || c.Probe.Observation == nil {
		p.Observation = nil
	}
	switch {
	case len(p.Anomalies) > 0 || !probeEndpointObserved(p) || !p.Initialized || p.Receipts != 1 || !p.Completed:
		return fmt.Errorf("probe replay: anomalies %v, intact %v, end %s, %d receipts, completed %v", p.Anomalies, p.Intact, p.Observation.Label(), p.Receipts, p.Completed)
	case p.Observation != nil && (!c.Probe.Observation.equal(p.Observation) || !p.Observation.CleanSession):
		return errors.New("probe replay: the recorded observation differs from the replayed server events or is not a clean session")
	case p.ClientName == nil || p.ClientVersion == nil || *p.ClientName != o.ClientInfo.Name || *p.ClientVersion != o.ClientInfo.Version:
		return errors.New("probe replay: initialize clientInfo differs from the oracle's")
	}
	return nil
}

// replayClient is the manifest record of client id in bundle b (nil when
// the bundle carries no manifest or no such record).
func replayClient(b *CaptureBundle, id string) *CaptureClient {
	if b.Manifest == nil {
		return nil
	}
	for i := range b.Manifest.Clients {
		if b.Manifest.Clients[i].ID == id {
			return &b.Manifest.Clients[i]
		}
	}
	return nil
}

// EnrollmentOptions select what ValidateEnrollment checks.
type EnrollmentOptions struct {
	// Root is the repository directory (production): it is opened as an
	// os.Root and every component is opened relative to its verified,
	// held parent. Otherwise FS is used.
	Root string
	// FS is the repository as an fs.FS (in-memory test trees and other
	// callers), read by path with every opened component verified.
	FS fs.FS
	// Registry is the compiled registry whose qualified versions the index
	// must back exactly (production: DefaultRegistry).
	Registry Registry
	// Literals and Paths are the owner's known local literals and labeled
	// paths for the local fixed-point check only; they never enter
	// checked-in evidence. CI supplies none.
	Literals []string
	Paths    map[string]string

	// policy is the sanitization policy checking receipts (nil: the
	// production ProductionFixturePolicy); only this package's unit tests
	// set it, to validate tiny fabricated sanitized bundles.
	policy *FixturePolicy
}

// base is the repository tree of opts.
func (opts EnrollmentOptions) base() (tree, error) {
	if opts.Root != "" {
		r, err := os.OpenRoot(opts.Root)
		if err != nil {
			return nil, fmt.Errorf("enrollment root: %w", err)
		}
		return &rootTree{r}, nil
	}
	if opts.FS == nil {
		return nil, errors.New("enrollment: no Root or FS")
	}
	return &fsTree{fsys: opts.FS, dir: "."}, nil
}

// ValidateEnrollment is the offline enrollment self-check (pass the
// repository as Root in production; a symbolic link in any component is
// refused either way): the index
// parses; every entry's bundle validates, is a complete one-client capture
// of exactly the entry's client, version and platform, passes the
// RedactionFixedPoint guard on every file (including expected.json and the
// manifest) and replays to its reviewed oracle; and the registry's
// qualified versions and the index agree exactly (every qualified version
// has evidence, every evidence fixture is indexed with the same version,
// platform and capability set, no index entry is an orphan, and an
// unqualified version carries no evidence).
func ValidateEnrollment(opts EnrollmentOptions) (*EnrollmentIndex, error) {
	base, err := opts.base()
	if err != nil {
		return nil, err
	}
	defer base.close()
	ib, err := readPath(base, EnrollmentIndexPath, maxIndexBytes)
	if err != nil {
		return nil, fmt.Errorf("enrollment index: %w", err)
	}
	idx, err := ParseEnrollmentIndex(ib)
	if err != nil {
		return nil, err
	}
	red := NewCaptureRedactor(opts.Literals, opts.Paths)
	oracles := map[string]*ExpectedOracle{}
	for _, e := range idx.Entries {
		o, err := validateEntry(opts, base, red, e)
		if err != nil {
			return nil, fmt.Errorf("enrollment %s: %w", e.Fixture, err)
		}
		oracles[e.Fixture] = o
	}
	return idx, CheckRegistryEvidence(opts.Registry, idx, oracles)
}

func validateEntry(opts EnrollmentOptions, base tree, red *Redactor, e EnrollmentEntry) (*ExpectedOracle, error) {
	bt, err := openPath(base, e.Bundle)
	if err != nil {
		return nil, fmt.Errorf("capture bundle %s: %v", e.Bundle, err)
	}
	if bt != base {
		defer bt.close()
	}
	// Provenance first: the indexed manifest bytes, then the bundle they
	// describe. The manifest is read once: exactly the hashed bytes are
	// validated and used (code review round 3, C1).
	mb, err := bt.read(CaptureManifestName, MaxEvidenceFileBytes)
	if err != nil {
		return nil, err
	}
	if sha256Hex(mb) != e.ManifestSHA256 {
		return nil, errors.New("the manifest's hash differs from the index (forged or altered provenance)")
	}
	extra := []string{ExpectedName}
	if e.SanitizationSHA256 != nil {
		extra = append(extra, SanitizationName)
	}
	b, err := validateBundle(bt, e.Bundle, mb, extra...)
	if err != nil {
		return nil, err
	}
	m := b.Manifest
	switch {
	case m.State != CaptureComplete || len(m.Clients) != 1 || m.Clients[0].ID != e.Client:
		return nil, fmt.Errorf("the bundle is not a complete one-client capture of %s", e.Client)
	case m.OS+"/"+m.Arch != e.Platform:
		return nil, fmt.Errorf("captured on %s/%s, indexed as %s", m.OS, m.Arch, e.Platform)
	case m.RunID != path.Base(e.Bundle):
		return nil, fmt.Errorf("run %s is not the bundle directory %s", m.RunID, path.Base(e.Bundle))
	}
	c := &m.Clients[0]
	if c.ObservedVersion == nil || *c.ObservedVersion != e.Version || c.ExpectedVersion != e.Version || c.Decoder != e.Decoder {
		return nil, fmt.Errorf("the capture's version or decoder label differs from the index's exact %q", e.Version)
	}
	ob, err := bt.read(ExpectedName, maxIndexBytes)
	if err != nil {
		return nil, err
	}
	if sha256Hex(ob) != e.ExpectedSHA256 {
		return nil, errors.New("expected.json's hash differs from the index")
	}
	o, err := ParseExpected(ob)
	if err != nil {
		return nil, err
	}
	if err := o.validate(c); err != nil {
		return nil, fmt.Errorf("expected.json: %w", err)
	}
	all := map[string][]byte{CaptureManifestName: b.ManifestBytes, ExpectedName: ob}
	if err := validateSanitized(opts, bt, e, b, o, all); err != nil {
		return nil, err
	}
	for p, data := range b.Files {
		all[p] = data
	}
	names := make([]string, 0, len(all))
	for p := range all {
		names = append(names, p)
	}
	sort.Strings(names)
	for _, p := range names {
		if err := RedactionFixedPoint(p, all[p], red); err != nil {
			return nil, err
		}
	}
	return o, Replay(opts.Registry, e, b, o)
}

// validateSanitized applies the B2 provenance rules (design
// decoder-enrollment B2, FP-18) to entry e: an entry with a receipt has its
// sanitization.json read through the held bundle root within the index
// bound, hashing to the index's value, verified against the bundle under
// the compiled policy (CheckFixtureSanitization), its policy and source
// hash equal to the oracle's, the authorized B2 attestation, and the
// receipt added to the fixed-point files (all). An entry without one may
// not be an enrolled B2 identity and its oracle names no policy. There is
// no fallback between the two.
func validateSanitized(opts EnrollmentOptions, bt tree, e EnrollmentEntry, b *CaptureBundle, o *ExpectedOracle, all map[string][]byte) error {
	if e.SanitizationSHA256 == nil {
		switch {
		case isRealVersion(e.Client, e.Version):
			return fmt.Errorf("the enrolled B2 identity %q requires its %s receipt", e.Version, SanitizationName)
		case o.SanitizationPolicy != nil:
			return errors.New("expected.json names a sanitization policy but the entry has no " + SanitizationName)
		}
		return nil
	}
	pol := opts.policy
	if pol == nil {
		pol = ProductionFixturePolicy()
	}
	sb, err := bt.read(SanitizationName, maxIndexBytes)
	if err != nil {
		return err
	}
	if sha256Hex(sb) != *e.SanitizationSHA256 {
		return errors.New(SanitizationName + "'s hash differs from the index")
	}
	san, err := CheckFixtureSanitization(pol, b, sb)
	if err != nil {
		return err
	}
	switch {
	case o.SanitizationPolicy == nil || *o.SanitizationPolicy != san.Policy:
		return fmt.Errorf("expected.json's sanitization_policy is not the receipt's %q", san.Policy)
	case o.SourceCaptureSHA256 != san.SourceManifestSHA256:
		return errors.New("expected.json's source_capture_sha256 is not the receipt's source manifest hash")
	}
	if err := CheckB2Attestation(o.Attestation); err != nil {
		return fmt.Errorf("expected.json: %w", err)
	}
	all[SanitizationName] = sb
	return nil
}

// CheckRegistryEvidence is the registry/index agreement: every qualified
// version carries evidence; each evidence entry names an indexed fixture
// of the same decoder, exact version and platform whose oracle
// demonstrates exactly its capability set; the canonical Fixture is the
// first evidence fixture; every index entry is claimed exactly once; and
// no unqualified version carries evidence.
func CheckRegistryEvidence(reg Registry, idx *EnrollmentIndex, oracles map[string]*ExpectedOracle) error {
	byFixture := map[string]EnrollmentEntry{}
	for _, e := range idx.Entries {
		byFixture[e.Fixture] = e
	}
	claimed := map[string]bool{}
	names := make([]string, 0, len(reg))
	for n := range reg {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, name := range names {
		for _, v := range reg[name].versions {
			if err := v.validateEvidence(); err != nil {
				return fmt.Errorf("registry %s: %w", name, err)
			}
			if !v.Qualified {
				continue
			}
			if len(v.Evidence) == 0 {
				return fmt.Errorf("registry %s %q is qualified without enrolled evidence", name, v.Version)
			}
			if v.Fixture != v.Evidence[0].Fixture {
				return fmt.Errorf("registry %s %q: canonical fixture %q is not its first evidence fixture %q", name, v.Version, v.Fixture, v.Evidence[0].Fixture)
			}
			for _, ev := range v.Evidence {
				e, ok := byFixture[ev.Fixture]
				switch {
				case !ok:
					return fmt.Errorf("registry %s %q: evidence fixture %s is not in the enrollment index", name, v.Version, ev.Fixture)
				case e.Decoder != name || e.Version != v.Version || e.Platform != ev.Platform:
					return fmt.Errorf("registry %s %q: evidence %s is indexed for %s %q on %s", name, v.Version, ev.Fixture, e.Decoder, e.Version, e.Platform)
				case claimed[ev.Fixture]:
					return fmt.Errorf("registry: fixture %s is claimed twice", ev.Fixture)
				}
				claimed[ev.Fixture] = true
				o := oracles[ev.Fixture]
				have, want := append([]string(nil), ev.Kinds...), []string(nil)
				if o != nil {
					want = append(want, o.Capabilities...)
				}
				sort.Strings(have)
				sort.Strings(want)
				if !slices.Equal(have, want) {
					return fmt.Errorf("registry %s %q: capabilities %v differ from the oracle's %v for %s", name, v.Version, ev.Kinds, want, ev.Fixture)
				}
			}
		}
	}
	for _, e := range idx.Entries {
		if !claimed[e.Fixture] {
			return fmt.Errorf("enrollment index: %s is an orphan (no qualified registry version claims it)", e.Fixture)
		}
	}
	return nil
}

// QualifiedVersions lists every qualified registry version as
// "<decoder> <version>".
func (r Registry) QualifiedVersions() []string {
	var out []string
	for name, s := range r {
		for _, v := range s.versions {
			if v.Qualified {
				out = append(out, name+" "+v.Version)
			}
		}
	}
	sort.Strings(out)
	return out
}
