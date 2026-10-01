package workspacetransfer

import (
	"context"
	"crypto/sha256"
	"errors"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/filemode"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/go-git/go-git/v5/plumbing/storer"
	"golang.org/x/sys/unix"
	"golang.org/x/text/unicode/norm"

	"github.com/wedevwork/callsheet/internal/contract"
)

// Git cleanliness (FP-2). Dirty is any difference between the HEAD tree
// and the index, any tracked difference between the index and the
// working tree, or any nonignored untracked file or directory. go-git's
// Status is not trusted: this is an explicit comparison with Git's
// semantics for the supported set (core.fileMode, core.symlinks,
// core.ignoreCase, core.autocrlf/core.eol with the text, crlf, eol and
// ident attributes, and the darwin core.precomposeUnicode comparison
// keys). Every tracked file's clean content is hashed (no stat-cache
// shortcut); nothing is written: no index refresh, no stat cache, no
// config.
//
// Unicode policy (post-final clarification DW1): on darwin with
// core.precomposeUnicode effective, only names read from the working
// tree (readdir) are NFC-normalized into comparison keys; index and HEAD
// tree paths and ignore-pattern text are compared as stored bytes. So an
// NFD index path with an NFD file on disk is a deletion plus an untracked
// NFC path (dirty, as git status says), and an NFC index path with an NFD
// file on disk is clean. Linux keys are always raw bytes.

// statusCfg is the effective comparison configuration.
type statusCfg struct {
	fileMode, symlinks, ignoreCase, precompose bool
	eol                                        eolConfig
}

// readStatusConfig reads the status settings for goos.
func readStatusConfig(c *gitConfig, goos string) (statusCfg, error) {
	var s statusCfg
	var err error
	if s.fileMode, err = c.boolean("core.filemode", true); err != nil {
		return s, unsupportedConfig("core.fileMode is not a boolean")
	}
	if s.symlinks, err = c.boolean("core.symlinks", true); err != nil {
		return s, unsupportedConfig("core.symlinks is not a boolean")
	}
	if s.ignoreCase, err = c.boolean("core.ignorecase", false); err != nil {
		return s, unsupportedConfig("core.ignoreCase is not a boolean")
	}
	pre, err := c.boolean("core.precomposeunicode", false)
	if err != nil {
		return s, unsupportedConfig("core.precomposeUnicode is not a boolean")
	}
	// Unset is false on every OS; an explicit true has an effect only on
	// darwin.
	s.precompose = pre && goos == "darwin"
	if e, ok := c.get("core.autocrlf"); ok {
		if !e.blank && strings.EqualFold(e.value, "input") {
			s.eol.autocrlf = autoCRLFInput
		} else if b, err := c.boolean("core.autocrlf", false); err != nil {
			return s, unsupportedConfig("core.autocrlf is not true, false or input")
		} else if b {
			s.eol.autocrlf = autoCRLFTrue
		}
	}
	if v, ok := c.str("core.eol"); ok {
		// Git: lf, crlf and native in any ASCII case; any other value is
		// unset (native, LF on both systems), never an error.
		if asciiFold(v) == "crlf" {
			s.eol.eolCRLF = true
		}
	}
	return s, nil
}

// wtKey is the comparison key of a raw worktree name: NFC when darwin
// precompose is effective (valid UTF-8 only; invalid names keep their
// bytes), then ASCII case folding under core.ignoreCase.
func (s statusCfg) wtKey(name string) string {
	if s.precompose && utf8.ValidString(name) {
		name = norm.NFC.String(name)
	}
	if s.ignoreCase {
		name = asciiFold(name)
	}
	return name
}

// idxKey is the comparison key of a stored (index or tree) path: its
// bytes, ASCII-folded under core.ignoreCase, never normalized.
func (s statusCfg) idxKey(path string) string {
	if s.ignoreCase {
		return asciiFold(path)
	}
	return path
}

// canonMode maps the legacy group-writable regular mode to 100644.
func canonMode(m uint32) uint32 {
	if m == 0o100664 {
		return 0o100644
	}
	return m
}

// treeFiles flattens a commit's tree into raw path -> (mode, hash).
type treeFile struct {
	mode uint32
	hash plumbing.Hash
}

func flattenTree(ctx context.Context, s storer.EncodedObjectStorer, tree plumbing.Hash) (map[string]treeFile, error) {
	out := map[string]treeFile{}
	var walk func(prefix string, h plumbing.Hash) error
	walk = func(prefix string, h plumbing.Hash) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		t, err := object.GetTree(s, h)
		if err != nil {
			return err
		}
		for _, e := range t.Entries {
			if e.Mode == filemode.Dir {
				if err := walk(prefix+e.Name+"/", e.Hash); err != nil {
					return err
				}
				continue
			}
			out[prefix+e.Name] = treeFile{mode: uint32(e.Mode), hash: e.Hash}
		}
		return nil
	}
	return out, walk("", tree)
}

// statusWalk is one cleanliness comparison.
type statusWalk struct {
	ctx context.Context
	d   *deps
	r   *repo
	env Env
	c   statusCfg

	byKey   map[string]*indexEntry
	dirKeys map[string]bool
	seen    map[string]bool

	ign      ignoreStack
	attrLow  []*attrFile // system, global
	attrInfo *attrFile
	attrDirs []*attrFile

	man         *manifest
	dirty       bool
	unsupported error

	visited, bytesRead int64
}

// setUnsupported keeps the first unsupported refusal.
func (w *statusWalk) setUnsupported(err error) {
	if w.unsupported == nil {
		w.unsupported = err
	}
}

// checkClean runs the complete comparison and returns nil for a clean
// source, the unsupported refusal, a dirty refusal or an I/O error. The
// manifest it returns covers everything read.
func checkClean(ctx context.Context, d *deps, r *repo, env Env, goos string) (*manifest, error) {
	if r.head.unborn {
		return nil, errUnborn()
	}
	c, err := readStatusConfig(r.cfg, goos)
	if err != nil {
		return nil, err
	}
	w := &statusWalk{ctx: ctx, d: d, r: r, env: env, c: c, byKey: map[string]*indexEntry{}, dirKeys: map[string]bool{},
		seen: map[string]bool{}, man: newManifest()}
	for i := range r.index.entries {
		e := &r.index.entries[i]
		k := c.idxKey(e.name)
		if _, dup := w.byKey[k]; dup {
			return nil, errUnsupported("tracked paths that differ only by case under core.ignoreCase")
		}
		w.byKey[k] = e
		for j := strings.IndexByte(k, '/'); j >= 0; j = nextSlash(k, j) {
			w.dirKeys[k[:j]] = true
		}
	}
	for k := range w.byKey {
		if w.dirKeys[k] {
			return nil, errUnsupported("an index with a path that is both a file and a directory")
		}
	}
	// The baselines are what openRepo read (HEAD and its resolution, the
	// index and every configuration file consulted), so a change between
	// admission and the scan is a source change too.
	gitDir := filepath.Join(r.root, ".git")
	headPath := filepath.Join(gitDir, "HEAD")
	w.man.addBaseline(sha256.Sum256(r.head.raw), func() [32]byte { return digestFile(headPath) })
	w.man.addBaseline(headDigest(r.head), func() [32]byte {
		fd, err := openPathDir(gitDir)
		if err != nil {
			return [32]byte{}
		}
		defer closeFD(fd)
		h, err := readHead(fd)
		if err != nil {
			return [32]byte{}
		}
		return headDigest(h)
	})
	w.man.addBaseline(indexDigestBytes(r.indexRaw), func() [32]byte { return indexDigest(filepath.Join(gitDir, "index")) })
	for _, f := range r.cfgFiles {
		p := f
		w.man.addBaseline(r.cfg.sums[p], func() [32]byte { return digestFile(p) })
	}
	if err := w.loadFixed(gitDir); err != nil {
		return nil, err
	}
	if err := w.staged(); err != nil {
		return nil, err
	}
	d.stage("status-staged")
	if err := w.walk(r.rootFD, "", "", true, false, 0); err != nil {
		return nil, err
	}
	for k := range w.byKey {
		if !w.seen[k] {
			w.dirty = true
			break
		}
	}
	d.emit("status-files", w.visited)
	d.emit("status-bytes", w.bytesRead)
	switch {
	case w.unsupported != nil:
		return nil, w.unsupported
	case w.dirty:
		return nil, errDirty()
	}
	return w.man, nil
}

func nextSlash(s string, j int) int {
	i := strings.IndexByte(s[j+1:], '/')
	if i < 0 {
		return -1
	}
	return j + 1 + i
}

// headDigest digests a resolved HEAD: its symbolic target and commit.
func headDigest(h headState) [32]byte {
	return sha256.Sum256([]byte(h.symbolic + "\x00" + h.hash.String()))
}

// indexDigest digests an index's entries (names, modes, hashes and
// flags), not its stat caches: a concurrent stat refresh is no change.
// An absent index digests to zero.
func indexDigest(p string) [32]byte {
	b, err := os.ReadFile(p)
	if err != nil {
		return [32]byte{}
	}
	return indexDigestBytes(b)
}

// indexDigestBytes is indexDigest of an index's content (nil: absent).
func indexDigestBytes(b []byte) [32]byte {
	if b == nil {
		return [32]byte{}
	}
	x, err := parseIndex(b)
	if err != nil {
		return sha256.Sum256(b)
	}
	h := sha256.New()
	for _, e := range x.entries {
		io.WriteString(h, e.name)
		h.Write([]byte{0, byte(e.mode >> 16), byte(e.mode >> 8), byte(e.mode), byte(e.stage)})
		h.Write(e.hash[:])
		if e.assumeValid || e.skipWorktree || e.intentToAdd {
			h.Write([]byte{1})
		}
	}
	var out [32]byte
	copy(out[:], h.Sum(nil))
	return out
}

// loadFixed reads the global excludes, info/exclude and the system,
// global and info attributes files. Missing optional files are empty;
// unreadable ones fail.
func (w *statusWalk) loadFixed(gitDir string) error {
	excl, ok := w.r.cfg.str("core.excludesfile")
	var global string
	var err error
	switch {
	case ok && excl == "":
	case ok:
		if global, err = expandConfigPath(w.env, excl, w.r.root); err != nil {
			return err
		}
	default:
		global = w.env.xdgFile("ignore")
	}
	info, err := w.readOptional(filepath.Join(gitDir, "info", "exclude"))
	if err != nil {
		return err
	}
	w.ign.fixed = append(w.ign.fixed, &ignoreList{patterns: parseIgnore(info, "", w.c.ignoreCase)})
	if global != "" {
		g, err := w.readOptional(global)
		if err != nil {
			return err
		}
		w.ign.fixed = append(w.ign.fixed, &ignoreList{patterns: parseIgnore(g, "", w.c.ignoreCase)})
	}
	fixed, err := fixedAttributeFiles(w.env, w.r.cfg, w.r.root)
	if err != nil {
		return err
	}
	for _, p := range fixed {
		b, err := w.readOptional(p)
		if err != nil {
			return err
		}
		f, err := w.parseAttrs(b, "", true)
		if err != nil {
			return err
		}
		w.attrLow = append(w.attrLow, f)
	}
	b, err := w.readOptional(filepath.Join(gitDir, "info", "attributes"))
	if err != nil {
		return err
	}
	w.attrInfo, err = w.parseAttrs(b, "", true)
	return err
}

func (w *statusWalk) parseAttrs(b []byte, base string, macroOK bool) (*attrFile, error) {
	f, err := parseAttributes(b, base, macroOK, w.c.ignoreCase)
	if err != nil {
		return nil, errUnsupported("an attribute macro that changes content conversion")
	}
	return f, nil
}

// expandConfigPath expands a path-valued setting: "~/" from HOME,
// relative to the repository root.
func expandConfigPath(env Env, p, root string) (string, error) {
	switch {
	case p == "~" || strings.HasPrefix(p, "~/"):
		h := env.home()
		if h == "" {
			return "", unsupportedConfig("a ~/ path without HOME")
		}
		return filepath.Join(h, strings.TrimPrefix(p, "~")), nil
	case strings.HasPrefix(p, "~"):
		return "", unsupportedConfig("~user paths")
	case filepath.IsAbs(p):
		return p, nil
	}
	return filepath.Join(root, p), nil
}

// readOptional reads a file outside the source tree: missing is empty,
// unreadable fails; the file is recorded for change detection.
func (w *statusWalk) readOptional(p string) ([]byte, error) {
	w.man.addFile(p)
	b, err := os.ReadFile(p)
	if err == nil {
		return b, nil
	}
	if errors.Is(err, os.ErrNotExist) || errors.Is(err, unix.ENOTDIR) {
		return nil, nil
	}
	return nil, contract.TransferError(contract.CodeInvalidArgument, contract.ReasonUnsupportedRepository,
		"an ignore or attributes file named by the git configuration cannot be read")
}

// staged compares the HEAD tree with the index by stored path, mode and
// object: any difference (including an intent-to-add entry) is dirty.
func (w *statusWalk) staged() error {
	c, err := object.GetCommit(w.r.store, w.r.head.hash)
	if err != nil {
		return errUnsupported("a HEAD commit that cannot be read")
	}
	files, err := flattenTree(w.ctx, w.r.store, c.TreeHash)
	if err != nil {
		return ctxOr(w.ctx, errUnsupported("a HEAD tree that cannot be read"))
	}
	if len(files) != len(w.r.index.entries) {
		w.dirty = true
		return nil
	}
	for _, e := range w.r.index.entries {
		f, ok := files[e.name]
		if !ok || e.intentToAdd || canonMode(f.mode) != canonMode(e.mode) || f.hash != e.hash {
			w.dirty = true
			return nil
		}
	}
	return nil
}

// entry is one directory entry of the walk.
type entry struct {
	name, key string
	st        unix.Stat_t
}

// walk compares one directory: fd is open, keyPrefix and rawPrefix carry
// a trailing slash below the root, tracked marks a directory holding
// index entries, excluded one whose untracked contents are ignored.
func (w *statusWalk) walk(fd int, keyPrefix, rawPrefix string, tracked, excluded bool, depth int) error {
	if err := w.ctx.Err(); err != nil {
		return err
	}
	st, err := fstatFD(fd)
	if err != nil {
		return errStorage("cannot read the working tree", err)
	}
	names, err := readNames(fd)
	if err != nil {
		return errStorage("cannot read the working tree", err)
	}
	md := w.man.dir(strings.TrimSuffix(rawPrefix, "/"), names, identityOf(&st))
	if depth > 0 {
		for _, n := range names {
			if n == ".git" {
				w.setUnsupported(errUnsupported("a nested repository in the working tree"))
				return nil
			}
		}
	}
	// Per-directory rules and attributes of this directory.
	var ign *ignoreList
	if !excluded {
		if b, ok, err := w.readTreeFile(fd, md, ".gitignore"); err != nil {
			return err
		} else if ok {
			ign = &ignoreList{patterns: parseIgnore(b, keyPrefix, w.c.ignoreCase)}
		}
	}
	w.ign.push(ign)
	defer w.ign.pop()
	var attrs *attrFile
	if tracked {
		b, ok, err := w.readTreeFile(fd, md, ".gitattributes")
		if err != nil {
			return err
		}
		if !ok {
			b, ok, err = w.indexBlob(keyPrefix + ".gitattributes")
			if err != nil {
				return err
			}
		}
		if ok {
			if attrs, err = w.parseAttrs(b, keyPrefix, depth == 0); err != nil {
				w.setUnsupported(err)
			}
		}
	}
	w.attrDirs = append(w.attrDirs, attrs)
	defer func() { w.attrDirs = w.attrDirs[:len(w.attrDirs)-1] }()

	entries := make([]entry, 0, len(names))
	keys := map[string]bool{}
	for _, n := range names {
		if depth == 0 && n == ".git" {
			continue
		}
		k := keyPrefix + w.c.wtKey(n)
		if keys[k] {
			w.setUnsupported(errUnsupported("working-tree names that differ only by case or Unicode normalization"))
			return nil
		}
		keys[k] = true
		est, err := lstatAt(fd, n)
		if err != nil {
			if errors.Is(err, unix.ENOENT) {
				return errSourceChanged()
			}
			return errStorage("cannot read the working tree", err)
		}
		entries = append(entries, entry{name: n, key: k, st: est})
	}
	for i := range entries {
		e := &entries[i]
		if err := w.entry(fd, md, e, rawPrefix, tracked, excluded, depth); err != nil {
			return err
		}
	}
	return nil
}

// readTreeFile reads a regular .gitignore or .gitattributes in the
// working tree (a symlinked one is not read as rules).
func (w *statusWalk) readTreeFile(fd int, md *manifestDir, name string) ([]byte, bool, error) {
	st, err := lstatAt(fd, name)
	if err != nil || !isRegStat(&st) {
		return nil, false, nil
	}
	f, fst, err := openFileAt(fd, name)
	if err != nil {
		return nil, false, errStorage("cannot read the working tree", err)
	}
	defer f.Close()
	if identityOf(&fst) != identityOf(&st) {
		return nil, false, errSourceChanged()
	}
	b, err := readAllCtx(w.ctx, f)
	if err != nil {
		return nil, false, ctxOr(w.ctx, errStorage("cannot read the working tree", err))
	}
	md.file(name, &st)
	return b, true, nil
}

// indexBlob reads the stage-0 index blob of a key path (the attributes
// fallback when the working-tree file is missing).
func (w *statusWalk) indexBlob(key string) ([]byte, bool, error) {
	e, ok := w.byKey[key]
	if !ok || e.mode == 0o120000 {
		return nil, false, nil
	}
	b, err := readBlob(w.r.store, e.hash)
	if err != nil {
		return nil, false, errUnsupported("an index blob that cannot be read")
	}
	return b, true, nil
}

func readBlob(s storer.EncodedObjectStorer, h plumbing.Hash) ([]byte, error) {
	o, err := s.EncodedObject(plumbing.BlobObject, h)
	if err != nil {
		return nil, err
	}
	r, err := o.Reader()
	if err != nil {
		return nil, err
	}
	defer r.Close()
	return io.ReadAll(r)
}

// entry handles one directory entry.
func (w *statusWalk) entry(fd int, md *manifestDir, e *entry, rawPrefix string, tracked, excluded bool, depth int) error {
	isDir := isDirStat(&e.st)
	if ie, ok := w.byKey[e.key]; ok {
		w.seen[e.key] = true
		md.file(e.name, &e.st)
		return w.compare(fd, e, ie)
	}
	if w.dirKeys[e.key] {
		if !isDir {
			// A tracked directory replaced by a file: its entries are
			// missing (dirty); nothing below it is read.
			md.file(e.name, &e.st)
			return nil
		}
		sub, err := openDirAt(fd, e.name)
		if err != nil {
			return errSourceChanged()
		}
		defer closeFD(sub)
		ex := excluded || w.ign.excluded(e.key, true)
		return w.walk(sub, e.key+"/", rawPrefix+e.name+"/", true, ex, depth+1)
	}
	// Untracked.
	switch {
	case isDir:
	case isRegStat(&e.st), isLinkStat(&e.st):
	default:
		// Git does not report special files as untracked.
		return nil
	}
	if excluded || w.ign.excluded(e.key, isDir) {
		return nil
	}
	md.file(e.name, &e.st)
	if !isDir {
		w.dirty = true
		return nil
	}
	sub, err := openDirAt(fd, e.name)
	if err != nil {
		return errSourceChanged()
	}
	defer closeFD(sub)
	return w.walk(sub, e.key+"/", rawPrefix+e.name+"/", false, false, depth+1)
}

// attrsFor resolves the attributes of a tracked index path.
func (w *statusWalk) attrsFor(path string) attrMap {
	files := make([]*attrFile, 0, len(w.attrLow)+len(w.attrDirs)+1)
	files = append(files, w.attrLow...)
	files = append(files, w.attrDirs...)
	files = append(files, w.attrInfo)
	return resolveAttrs(files, w.c.idxKey(path))
}

// conversionFor refuses active external filters and working-tree
// encodings, and returns the check-in conversion.
func (w *statusWalk) conversionFor(path string) (conversion, error) {
	a := w.attrsFor(path)
	if f := a["filter"]; f.state == attrValue && f.value != "" {
		for _, k := range []string{"clean", "smudge", "process"} {
			if _, ok := w.r.cfg.get("filter." + f.value + "." + k); ok {
				return conversion{}, errUnsupported("an active external filter")
			}
		}
		if req, err := w.r.cfg.boolean("filter."+f.value+".required", false); err != nil || req {
			return conversion{}, errUnsupported("a required external filter")
		}
	}
	if a["working-tree-encoding"].state == attrValue || a["working-tree-encoding"].state == attrSet {
		return conversion{}, errUnsupported("a working-tree-encoding attribute")
	}
	return resolveConversion(a, w.c.eol), nil
}

// compare compares one tracked index entry with its worktree entry.
func (w *statusWalk) compare(fd int, e *entry, ie *indexEntry) error {
	w.visited++
	mode := canonMode(ie.mode)
	reg, link := isRegStat(&e.st), isLinkStat(&e.st)
	switch mode {
	case 0o120000:
		switch {
		case link:
			text, err := readlinkAt(fd, e.name)
			if err != nil {
				return errSourceChanged()
			}
			if plumbing.ComputeHash(plumbing.BlobObject, []byte(text)) != ie.hash {
				w.dirty = true
			}
			return nil
		case reg && !w.c.symlinks:
			return w.compareContent(fd, e, ie)
		}
		w.dirty = true
		return nil
	case 0o100644, 0o100755:
		if !reg {
			w.dirty = true
			return nil
		}
		if w.c.fileMode && (e.st.Mode&0o100 != 0) != (mode == 0o100755) {
			w.dirty = true
			return nil
		}
		return w.compareContent(fd, e, ie)
	}
	w.setUnsupported(errUnsupported("an unsupported index entry mode"))
	return nil
}

// compareContent hashes a regular file's clean form and compares it with
// the index blob; the file's identity must hold across the reads.
func (w *statusWalk) compareContent(fd int, e *entry, ie *indexEntry) error {
	conv, err := w.conversionFor(ie.name)
	if err != nil {
		w.setUnsupported(err)
		return nil
	}
	f, fst, err := openFileAt(fd, e.name)
	if err != nil {
		return errSourceChanged()
	}
	defer f.Close()
	if identityOf(&fst) != identityOf(&e.st) {
		return errSourceChanged()
	}
	convert := conv.action != crlfBinary
	if convert && conv.action.auto() {
		stats, err := gatherStats(w.ctx, f)
		if err != nil {
			return ctxOr(w.ctx, errStorage("cannot read the working tree", err))
		}
		w.bytesRead += stats.size
		if stats.size == 0 || stats.binary() {
			convert = false
		} else if has, err := w.crlfInIndex(ie.hash); err != nil {
			return err
		} else if has {
			convert = false
		}
		if _, err := f.Seek(0, io.SeekStart); err != nil {
			return errStorage("cannot read the working tree", err)
		}
	}
	var h plumbing.Hash
	if !convert && !conv.ident {
		h, err = hashStream(w.ctx, f, fst.Size)
		w.bytesRead += fst.Size
	} else {
		var size int64
		cr := cleanReader(w.ctx, f, convert, conv.ident, w.d.tempDir)
		size, err = copyCtx(w.ctx, io.Discard, cr)
		closeReader(cr)
		if err == nil {
			if _, err = f.Seek(0, io.SeekStart); err == nil {
				cr = cleanReader(w.ctx, f, convert, conv.ident, w.d.tempDir)
				h, err = hashStream(w.ctx, cr, size)
				closeReader(cr)
			}
		}
		w.bytesRead += 2 * fst.Size
	}
	if err != nil {
		if errors.Is(err, errShortRead) {
			return errSourceChanged()
		}
		return ctxOr(w.ctx, errStorage("cannot read the working tree", err))
	}
	after, err := fstatFD(int(f.Fd()))
	if err != nil || identityOf(&after) != identityOf(&e.st) {
		return errSourceChanged()
	}
	if h != ie.hash {
		w.dirty = true
	}
	return nil
}

// crlfInIndex is Git's has_crlf_in_index: the index blob holds CRLF and
// is not binary.
func (w *statusWalk) crlfInIndex(h plumbing.Hash) (bool, error) {
	o, err := w.r.store.EncodedObject(plumbing.BlobObject, h)
	if err != nil {
		return false, errUnsupported("an index blob that cannot be read")
	}
	r, err := o.Reader()
	if err != nil {
		return false, errUnsupported("an index blob that cannot be read")
	}
	defer r.Close()
	s, err := gatherStats(w.ctx, r)
	if err != nil {
		return false, ctxOr(w.ctx, errUnsupported("an index blob that cannot be read"))
	}
	return !s.binary() && s.crlf > 0, nil
}

var errShortRead = errors.New("the file changed size while it was read")

// hashStream hashes exactly size bytes of r as a blob; a different
// length is errShortRead.
func hashStream(ctx context.Context, r io.Reader, size int64) (plumbing.Hash, error) {
	h := plumbing.NewHasher(plumbing.BlobObject, size)
	n, err := copyCtx(ctx, h, r)
	if err != nil {
		return plumbing.ZeroHash, err
	}
	if n != size {
		return plumbing.ZeroHash, errShortRead
	}
	return h.Sum(), nil
}

// copyCtx copies with a fixed buffer, checking ctx between chunks.
func copyCtx(ctx context.Context, dst io.Writer, src io.Reader) (int64, error) {
	buf := make([]byte, 32<<10)
	var n int64
	for {
		if err := ctx.Err(); err != nil {
			return n, err
		}
		m, err := src.Read(buf)
		if m > 0 {
			if _, werr := dst.Write(buf[:m]); werr != nil {
				return n, werr
			}
			n += int64(m)
		}
		if err == io.EOF {
			return n, nil
		}
		if err != nil {
			return n, err
		}
	}
}

// sortedKeys is a helper for deterministic iteration in tests.
func sortedKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
