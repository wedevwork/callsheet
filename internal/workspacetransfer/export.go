package workspacetransfer

import (
	"context"
	"errors"
	"io"
	"os"
	"path"
	"path/filepath"
	"strings"
	"unicode/utf8"

	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/filemode"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/go-git/go-git/v5/plumbing/storer"
	"golang.org/x/sys/unix"
	"golang.org/x/text/cases"
	"golang.org/x/text/unicode/norm"

	"github.com/wedevwork/callsheet/internal/contract"
)

// Folder export (FP-6): the selected commit's tree is validated
// completely, materialized in a private sibling staging directory
// (mode 0700, same filesystem) with exclusive creation through
// descriptor-relative no-follow calls, synced bottom-up, and published by
// one atomic rename: a no-replace rename onto an absent destination, or a
// rename over an existing empty directory (the kernel refuses a
// populated one). Nothing outside the staging directory is written
// before the rename, and a failure before it removes only the staging
// directory.

// exportDest is a validated folder destination.
type exportDest struct {
	parent   string
	parentFD int
	name     string
	existing bool
	id       identity
	perm     uint32
}

func (e *exportDest) close() {
	if e.parentFD >= 0 {
		closeFD(e.parentFD)
		e.parentFD = -1
	}
}

// emptyDir reports whether the directory name below dfd has no entries.
func emptyDir(dfd int, name string) (bool, error) {
	fd, err := openDirAt(dfd, name)
	if err != nil {
		return false, err
	}
	defer closeFD(fd)
	names, err := readNames(fd)
	return len(names) == 0, err
}

// stagingPrefix starts every private staging directory name.
const stagingPrefix = ".callsheet-pull-"

// maxTreeDepthExtra is the staging name's length (prefix and token).
var stagingNameLen = len(stagingPrefix) + 32

// exportEntry is one validated tree entry to materialize.
type exportEntry struct {
	path string // slash-separated below the root
	mode filemode.FileMode
	hash plumbing.Hash
}

// isHFSDotGit reports a name HFS+/APFS treats as .git: ".git" in any
// ASCII case after removing the code points HFS+ ignores (Git's
// is_hfs_dotgit).
func isHFSDotGit(name string) bool {
	if !utf8.ValidString(name) {
		return false
	}
	var b strings.Builder
	for _, r := range name {
		if hfsIgnorable(r) {
			continue
		}
		b.WriteRune(r)
	}
	return strings.EqualFold(b.String(), ".git")
}

// validName is one exportable tree entry name on goos.
func validName(goos, n string) bool {
	if !safeEntryName(n) || len(n) > contract.MaxNameBytes {
		return false
	}
	return !(goos == "darwin" && isHFSDotGit(n))
}

// collectTree validates the commit's complete tree for export on goos
// and returns its entries in tree order: safe unique names, supported
// modes, no gitlinks, native component and path lengths (including the
// staging and destination prefixes) and safe symlinks.
func collectTree(ctx context.Context, goos string, s storer.EncodedObjectStorer, commit plumbing.Hash, dest *exportDest) ([]exportEntry, error) {
	c, err := object.GetCommit(s, commit)
	if err != nil {
		return nil, errIntegrity("the selected commit cannot be read from the private store")
	}
	limit := contract.MaxPathFor(goos)
	prefixLen := max(len(dest.parent)+1+len(dest.name), len(dest.parent)+1+stagingNameLen) + 1
	var out []exportEntry
	var walk func(prefix string, h plumbing.Hash) error
	walk = func(prefix string, h plumbing.Hash) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		t, err := object.GetTree(s, h)
		if err != nil {
			return errIntegrity("a tree of the selected commit cannot be read")
		}
		seen := map[string]bool{}
		for _, e := range t.Entries {
			if !validName(goos, e.Name) {
				return errUnsafeTree("the selected tree has an entry name that cannot be exported safely (empty, dot, a slash, NUL, too long or .git); pull into a git repository instead")
			}
			if seen[e.Name] {
				return errUnsafeTree("the selected tree has duplicate entry names; pull into a git repository instead")
			}
			seen[e.Name] = true
			p := prefix + e.Name
			if prefixLen+len(p) > limit {
				return errUnsafeTree("the selected tree has a path too long for the destination filesystem; pull into a git repository instead")
			}
			switch e.Mode {
			case filemode.Dir:
				out = append(out, exportEntry{path: p, mode: e.Mode, hash: e.Hash})
				if err := walk(p+"/", e.Hash); err != nil {
					return err
				}
			case filemode.Regular, filemode.Executable, filemode.Deprecated, filemode.Symlink:
				out = append(out, exportEntry{path: p, mode: e.Mode, hash: e.Hash})
			default:
				return errUnsafeTree("the selected tree has a gitlink or an unsupported entry mode; pull into a git repository instead")
			}
		}
		return nil
	}
	if err := walk("", c.TreeHash); err != nil {
		return nil, err
	}
	if err := checkLinks(s, out, nil); err != nil {
		return nil, err
	}
	// APFS and HFS+ alias names by case and normalization: a link must
	// stay inside under that resolution too.
	if goos == "darwin" {
		if err := checkLinks(s, out, aliasKey(cases.Fold())); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// maxLinkTarget bounds a symlink target (PATH_MAX on both systems): a
// longer one cannot be created and is refused before it is read.
const maxLinkTarget = 4096

// readLinkTarget reads a symlink blob, refusing an oversized one before
// reading it.
func readLinkTarget(s storer.EncodedObjectStorer, h plumbing.Hash) (string, error) {
	o, err := s.EncodedObject(plumbing.BlobObject, h)
	if err != nil {
		return "", errIntegrity("a symlink of the selected commit cannot be read")
	}
	if o.Size() >= maxLinkTarget {
		return "", errUnsafeTree("the selected tree has a symlink target too long for the destination filesystem; pull into a git repository instead")
	}
	b, err := readBlob(s, h)
	if err != nil {
		return "", errIntegrity("a symlink of the selected commit cannot be read")
	}
	return string(b), nil
}

// aliasKey is a name's identity on a filesystem that aliases names by
// case and Unicode normalization (APFS and HFS+, case-folding Linux
// directories): the code points HFS+ ignores removed, full Unicode case
// folding and NFC. Invalid UTF-8 is compared as bytes.
func aliasKey(c cases.Caser) func(string) string {
	return func(name string) string {
		if !utf8.ValidString(name) {
			return name
		}
		var b strings.Builder
		for _, r := range norm.NFD.String(name) {
			if hfsIgnorable(r) {
				continue
			}
			b.WriteRune(r)
		}
		return norm.NFC.String(c.String(b.String()))
	}
}

// hfsIgnorable reports the code points HFS+ ignores in names (Git's
// is_hfs_dotgit list).
func hfsIgnorable(r rune) bool {
	return r == 0x200c || r == 0x200d || r == 0x200e || r == 0x200f ||
		r >= 0x202a && r <= 0x202e || r >= 0x206a && r <= 0x206f || r == 0xfeff
}

// maxLinkSteps bounds the work of resolving the tree's symlinks under
// name aliasing (each alias of a component is followed).
const maxLinkSteps = 1 << 20

// checkLinks resolves every symlink of the tree lexically: its target
// must be nonempty, relative, NUL-free and shorter than maxLinkTarget,
// and its complete resolution (through the tree's own directories and
// links, missing names treated as directories) must stay inside the root
// without a cycle, a chain longer than the tree or a traversal through a
// regular file. With fold, a component resolves to every tree entry whose
// name is the same under fold (a filesystem that aliases names), and
// every such resolution must be safe.
func checkLinks(s storer.EncodedObjectStorer, entries []exportEntry, fold func(string) string) error {
	kinds := map[string]filemode.FileMode{}
	links := map[string]string{}
	keyOf := func(comps []string) string {
		if fold == nil {
			return strings.Join(comps, "/")
		}
		k := make([]string, len(comps))
		for i, c := range comps {
			k[i] = fold(c)
		}
		return strings.Join(k, "/")
	}
	aliases := map[string][]string{}
	for _, e := range entries {
		kinds[e.path] = e.mode
		if fold != nil {
			k := keyOf(strings.Split(e.path, "/"))
			aliases[k] = append(aliases[k], e.path)
		}
		if e.mode == filemode.Symlink {
			t, err := readLinkTarget(s, e.hash)
			if err != nil {
				return err
			}
			if t == "" || strings.IndexByte(t, 0) >= 0 || strings.HasPrefix(t, "/") {
				return errUnsafeTree("the selected tree has a symlink with an empty or absolute target; pull into a git repository instead")
			}
			links[e.path] = t
		}
	}
	lookup := func(next []string) []string {
		if fold == nil {
			p := strings.Join(next, "/")
			if _, ok := kinds[p]; ok {
				return []string{p}
			}
			return nil
		}
		return aliases[keyOf(next)]
	}
	budget := len(entries)
	steps := 0
	var resolve func(cur, comps []string, hops int, active map[string]bool, k func([]string) error) error
	resolve = func(cur, comps []string, hops int, active map[string]bool, k func([]string) error) error {
		if steps++; steps > maxLinkSteps {
			return errUnsafeTree("the selected tree's symlinks are too complex to validate; pull into a git repository instead")
		}
		cur = append([]string(nil), cur...)
		for i, c := range comps {
			switch c {
			case "", ".":
				continue
			case "..":
				if len(cur) == 0 {
					return errUnsafeTree("the selected tree has a symlink that escapes the export root; pull into a git repository instead")
				}
				cur = cur[:len(cur)-1]
				continue
			}
			next := append(append([]string(nil), cur...), c)
			cands := lookup(next)
			if len(cands) == 0 {
				cur = next
				continue
			}
			rest, last := comps[i+1:], i == len(comps)-1
			for _, p := range cands {
				cand := strings.Split(p, "/")
				var err error
				switch mode := kinds[p]; {
				case mode == filemode.Dir:
					err = resolve(cand, rest, hops, active, k)
				case mode == filemode.Symlink:
					if active[p] {
						return errUnsafeTree("the selected tree has a symlink cycle; pull into a git repository instead")
					}
					if hops+1 > budget {
						return errUnsafeTree("the selected tree has a symlink chain longer than the tree; pull into a git repository instead")
					}
					active[p] = true
					err = resolve(cand[:len(cand)-1], strings.Split(links[p], "/"), hops+1, active, func(r []string) error {
						delete(active, p)
						defer func() { active[p] = true }()
						return resolve(r, rest, hops, active, k)
					})
					delete(active, p)
				case !last:
					return errUnsafeTree("the selected tree has a symlink that traverses a regular file; pull into a git repository instead")
				case k != nil:
					err = k(cand)
				}
				if err != nil {
					return err
				}
			}
			return nil
		}
		if k != nil {
			return k(cur)
		}
		return nil
	}
	for p, t := range links {
		dir := strings.Split(path.Dir(p), "/")
		if path.Dir(p) == "." {
			dir = nil
		}
		if err := resolve(dir, strings.Split(t, "/"), 0, map[string]bool{p: true}, nil); err != nil {
			return err
		}
	}
	return nil
}

// probeAliasing reports whether the filesystem of the private directory
// fd aliases names by case or by Unicode normalization: it creates a
// probe name and looks up its alias, removing the probe.
func probeAliasing(fd int) (bool, error) {
	for _, pair := range [][2]string{{".callsheet-alias-probe-a", ".CALLSHEET-ALIAS-PROBE-A"}, {".callsheet-alias-\u00e9", ".callsheet-alias-e\u0301"}} {
		f, err := unix.Openat(fd, pair[0], unix.O_WRONLY|unix.O_CREAT|unix.O_EXCL|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0o600)
		if err != nil {
			return false, err
		}
		closeFD(f)
		_, serr := lstatAt(fd, pair[1])
		if err := unix.Unlinkat(fd, pair[0], 0); err != nil {
			return false, err
		}
		if serr == nil {
			return true, nil
		}
		if !errors.Is(serr, unix.ENOENT) {
			return false, serr
		}
	}
	return false, nil
}

// prepareExport validates a folder destination before any network work:
// a nonexistent name in an existing real parent directory, or an existing
// empty real directory, never the filesystem root and never inside a
// repository. Parent symlinks are resolved once; the final component is
// never followed.
func prepareExport(abs string) (*exportDest, error) {
	abs = filepath.Clean(abs)
	if abs == string(filepath.Separator) {
		return nil, contract.New(contract.CodeInvalidArgument, "the filesystem root cannot be an export destination")
	}
	parent, err := filepath.EvalSymlinks(filepath.Dir(abs))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, contract.New(contract.CodeNotFound, "the destination's parent directory does not exist (missing parents are never created)")
		}
		return nil, errStorage("cannot resolve the destination's parent directory", err)
	}
	e := &exportDest{parent: parent, name: filepath.Base(abs), parentFD: -1}
	if e.parentFD, err = openPathDir(parent); err != nil {
		if errors.Is(err, unix.ENOTDIR) {
			return nil, contract.New(contract.CodeInvalidArgument, "the destination's parent is not a directory")
		}
		return nil, errStorage("cannot open the destination's parent directory", err)
	}
	if err := checkAncestors(filepath.Join(parent, e.name)); err != nil {
		e.close()
		return nil, err
	}
	st, err := lstatAt(e.parentFD, e.name)
	switch {
	case errors.Is(err, unix.ENOENT):
		return e, nil
	case err != nil:
		e.close()
		return nil, errStorage("cannot inspect the destination", err)
	case !isDirStat(&st):
		e.close()
		return nil, contract.New(contract.CodeInvalidArgument, "the destination is a symlink, file or special file; give a new or empty directory")
	}
	empty, err := emptyDir(e.parentFD, e.name)
	if err != nil {
		e.close()
		return nil, errStorage("cannot read the destination", err)
	}
	if !empty {
		e.close()
		return nil, errNotEmpty()
	}
	e.existing, e.id, e.perm = true, identityOf(&st), uint32(st.Mode)&0o777
	return e, nil
}

// recheck confirms the destination before publication: still absent, or
// still the same empty real directory.
func (e *exportDest) recheck() error {
	st, err := lstatAt(e.parentFD, e.name)
	if !e.existing {
		if errors.Is(err, unix.ENOENT) {
			return nil
		}
		return errNotEmpty()
	}
	if err != nil || !isDirStat(&st) || st.Ino != e.id.ino || uint64(st.Dev) != e.id.dev {
		return errNotEmpty()
	}
	empty, err := emptyDir(e.parentFD, e.name)
	if err != nil || !empty {
		return errNotEmpty()
	}
	return nil
}

// removeTreeAt removes name below dfd recursively without following any
// symlink.
func removeTreeAt(dfd int, name string) error {
	st, err := lstatAt(dfd, name)
	if errors.Is(err, unix.ENOENT) {
		return nil
	}
	if err != nil {
		return err
	}
	if !isDirStat(&st) {
		return unix.Unlinkat(dfd, name, 0)
	}
	fd, err := openDirAt(dfd, name)
	if err != nil {
		return err
	}
	names, err := readNames(fd)
	if err == nil {
		for _, n := range names {
			if err = removeTreeAt(fd, n); err != nil {
				break
			}
		}
	}
	closeFD(fd)
	if err != nil {
		return err
	}
	return unix.Unlinkat(dfd, name, unix.AT_REMOVEDIR)
}

// errCollision is an exclusive-creation collision in staging: the
// destination filesystem aliases two tree names.
func errCollision() error {
	return errUnsafeTree("the selected tree has names the destination filesystem cannot keep apart (case or Unicode normalization aliases); nothing was published; pull into a git repository instead")
}

// export materializes entries from s into a staging sibling of dest and
// publishes it. It returns an ambiguous error only after the rename.
func (d *deps) export(ctx context.Context, goos string, s storer.EncodedObjectStorer, entries []exportEntry, dest *exportDest) (err error) {
	tok, err := d.token()
	if err != nil {
		return errStorage("cannot name the staging directory", err)
	}
	staging := stagingPrefix + tok
	if err := d.check("export-mkdir"); err != nil {
		return errStorage("cannot create the staging directory", err)
	}
	if err := unix.Mkdirat(dest.parentFD, staging, 0o700); err != nil {
		return errStorage("cannot create the staging directory", err)
	}
	published := false
	defer func() {
		if !published {
			removeTreeAt(dest.parentFD, staging)
		}
	}()
	rootFD, err := openDirAt(dest.parentFD, staging)
	if err != nil {
		return errStorage("cannot open the staging directory", err)
	}
	// Close before the alias probe: both a probe error and an unsafe-link
	// refusal return before the materializer exists.
	defer closeFD(rootFD)
	// A destination filesystem that aliases names (a case-folding Linux
	// directory, for example) gets darwin's link check before anything
	// is materialized.
	if goos != "darwin" {
		aliasing, err := d.aliasProbe(rootFD)
		if err != nil {
			return errStorage("cannot probe the destination filesystem", err)
		}
		if aliasing {
			if err := checkLinks(s, entries, aliasKey(cases.Fold())); err != nil {
				return err
			}
		}
	}
	m := &materializer{ctx: ctx, d: d, s: s, rootFD: rootFD}
	defer m.closeAll()
	if err := m.run(entries); err != nil {
		return err
	}
	rootPerm := uint32(0o755)
	if dest.existing {
		rootPerm = dest.perm
	}
	if err := m.finishDirs(rootPerm); err != nil {
		return err
	}
	d.stage("export-staged")
	if err := dest.recheck(); err != nil {
		return err
	}
	// The last point before publication: a cancellation publishes
	// nothing (the staging directory, ours, is removed).
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := d.check("export-rename"); err != nil {
		return errStorage("cannot publish the export", err)
	}
	if dest.existing {
		err = unix.Renameat(dest.parentFD, staging, dest.parentFD, dest.name)
	} else {
		err = renameNoReplaceAt(dest.parentFD, staging, dest.parentFD, dest.name)
	}
	if err != nil {
		return publishFailure(goos, err)
	}
	published = true
	d.stage("export-renamed")
	if err := d.sync(dest.parentFD, "export-parent-sync"); err != nil {
		return ambiguousPull(err)
	}
	return nil
}

// publishFailure maps a failed publication rename on goos.
func publishFailure(goos string, err error) error {
	switch {
	case errors.Is(err, unix.EEXIST), errors.Is(err, unix.ENOTEMPTY):
		return errNotEmpty()
	case goos == "linux" && errors.Is(err, unix.EINVAL), goos == "darwin" && errors.Is(err, unix.ENOTSUP):
		return errStorage("the destination filesystem does not support atomic no-replace publication", err)
	case errors.Is(err, unix.EXDEV), errors.Is(err, unix.EBUSY):
		return errStorage("the destination cannot be replaced (a mount point or another filesystem)", err)
	}
	return errStorage("cannot publish the export", err)
}

// materializer writes the staging tree. Directories are held open only
// along the current path (a deep or wide tree never exhausts
// descriptors); later passes reopen them below the staging root without
// following links.
type materializer struct {
	ctx    context.Context
	d      *deps
	s      storer.EncodedObjectStorer
	rootFD int
	// stack holds the open directories of the current path.
	stack []openDir
	order []string
	bytes int64
}

type openDir struct {
	path string
	fd   int
}

func (m *materializer) closeAll() {
	for _, o := range m.stack {
		closeFD(o.fd)
	}
	m.stack = nil
}

func split(p string) (string, string) {
	i := strings.LastIndexByte(p, '/')
	if i < 0 {
		return "", p
	}
	return p[:i], p[i+1:]
}

// dirFD returns the open descriptor of dir, closing finished directories
// (entries come in depth-first tree order).
func (m *materializer) dirFD(dir string) int {
	for len(m.stack) > 0 {
		top := m.stack[len(m.stack)-1]
		if top.path == dir {
			return top.fd
		}
		closeFD(top.fd)
		m.stack = m.stack[:len(m.stack)-1]
	}
	return m.rootFD
}

// run creates directories and files in tree order, then symlinks.
func (m *materializer) run(entries []exportEntry) error {
	var links []exportEntry
	for _, e := range entries {
		if err := m.ctx.Err(); err != nil {
			return err
		}
		dir, name := split(e.path)
		dfd := m.dirFD(dir)
		switch e.mode {
		case filemode.Dir:
			if err := m.d.check("export-mkdir"); err != nil {
				return errStorage("cannot write the export", err)
			}
			if err := unix.Mkdirat(dfd, name, 0o700); err != nil {
				if errors.Is(err, unix.EEXIST) {
					return errCollision()
				}
				return errStorage("cannot write the export", err)
			}
			fd, err := openDirAt(dfd, name)
			if err != nil {
				return errStorage("cannot write the export", err)
			}
			m.stack = append(m.stack, openDir{path: e.path, fd: fd})
			m.order = append(m.order, e.path)
		case filemode.Symlink:
			links = append(links, e)
		default:
			if err := m.file(dfd, name, e); err != nil {
				return err
			}
		}
	}
	m.closeAll()
	for _, e := range links {
		if err := m.ctx.Err(); err != nil {
			return err
		}
		dir, name := split(e.path)
		target, err := readLinkTarget(m.s, e.hash)
		if err != nil {
			return err
		}
		if err := m.d.check("export-symlink"); err != nil {
			return errStorage("cannot write the export", err)
		}
		dfd, err := openRel(m.rootFD, dir)
		if err != nil {
			return errStorage("cannot write the export", err)
		}
		err = unix.Symlinkat(target, dfd, name)
		closeFD(dfd)
		if err != nil {
			if errors.Is(err, unix.EEXIST) {
				return errCollision()
			}
			return errStorage("cannot write the export", err)
		}
	}
	return nil
}

// file writes one blob with exclusive creation, verifies its length and
// hash while streaming, sets its final mode and then syncs it.
func (m *materializer) file(dfd int, name string, e exportEntry) error {
	o, err := m.s.EncodedObject(plumbing.BlobObject, e.hash)
	if err != nil {
		return errIntegrity("a file of the selected commit cannot be read")
	}
	if err := m.d.check("export-create"); err != nil {
		return errStorage("cannot write the export", err)
	}
	fd, err := unix.Openat(dfd, name, unix.O_WRONLY|unix.O_CREAT|unix.O_EXCL|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0o600)
	if err != nil {
		if errors.Is(err, unix.EEXIST) {
			return errCollision()
		}
		return errStorage("cannot write the export", err)
	}
	f := os.NewFile(uintptr(fd), name)
	werr := func() error {
		r, err := o.Reader()
		if err != nil {
			return errIntegrity("a file of the selected commit cannot be read")
		}
		defer r.Close()
		h := plumbing.NewHasher(plumbing.BlobObject, o.Size())
		if err := m.d.check("export-write"); err != nil {
			return err
		}
		n, err := copyCtx(m.ctx, io.MultiWriter(f, h), r)
		if err != nil {
			return err
		}
		if n != o.Size() || h.Sum() != e.hash {
			return errIntegrity("a file of the selected commit does not match its hash")
		}
		m.bytes += n
		// The final mode is set before the sync, so the synced inode
		// carries it.
		perm := uint32(0o644)
		if e.mode == filemode.Executable {
			perm = 0o755
		}
		if err := unix.Fchmod(fd, perm); err != nil {
			return err
		}
		return m.d.sync(fd, "export-sync")
	}()
	if cerr := f.Close(); werr == nil {
		werr = cerr
	}
	if werr == nil {
		werr = m.d.check("export-close")
	}
	if werr != nil {
		return orStorage(ctxOr(m.ctx, werr), "cannot write the export")
	}
	return nil
}

// finishDirs sets directory modes (0755 below the root, rootPerm at the
// root) and syncs directories bottom-up.
func (m *materializer) finishDirs(rootPerm uint32) error {
	for i := len(m.order) - 1; i >= 0; i-- {
		if err := m.ctx.Err(); err != nil {
			return err
		}
		fd, err := openRel(m.rootFD, m.order[i])
		if err != nil {
			return errStorage("cannot write the export", err)
		}
		err = unix.Fchmod(fd, 0o755)
		if err == nil {
			err = m.d.sync(fd, "export-dirsync")
		}
		closeFD(fd)
		if err != nil {
			return errStorage("cannot sync the export", err)
		}
	}
	if err := unix.Fchmod(m.rootFD, rootPerm); err != nil {
		return errStorage("cannot write the export", err)
	}
	if err := m.d.sync(m.rootFD, "export-dirsync"); err != nil {
		return errStorage("cannot sync the export", err)
	}
	return nil
}
