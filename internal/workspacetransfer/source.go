package workspacetransfer

import (
	"bytes"
	"context"
	"errors"
	"io"
	"io/fs"
	"math"
	"os"
	"path"
	"path/filepath"
	"strings"

	"github.com/go-git/go-billy/v5/osfs"
	"github.com/go-git/go-git/v5/plumbing/cache"
	"github.com/go-git/go-git/v5/storage/filesystem"
	"golang.org/x/sys/unix"

	"github.com/wedevwork/callsheet/internal/contract"
)

// Repository eligibility (FP-10). A path is classified before anything
// else: an ordinary non-bare working-tree root with a real .git directory
// is a repository; a directory inside a repository (a .git or bare layout
// in any ancestor) is refused with "use the repository root"; a .git
// file, a symlinked .git, a bare layout and every unsupported repository
// feature are refused explicitly, never silently treated as a plain
// folder. Only a directory with none of these is a plain folder.

// dirKind classifies a local directory.
type dirKind int

const (
	kindPlain dirKind = iota
	kindRepo
)

// largeObject is the size above which stored objects are streamed rather
// than read into memory.
const largeObject = 1 << 20

// canonicalDir resolves an absolute path's symlinks once and requires an
// existing directory.
func canonicalDir(abs string) (string, error) {
	canon, err := filepath.EvalSymlinks(abs)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return "", contract.New(contract.CodeNotFound, "the source path does not exist")
		}
		if errors.Is(err, unix.ENOTDIR) {
			return "", contract.New(contract.CodeInvalidArgument, "the source path is not a directory")
		}
		return "", errStorage("cannot resolve the source path", err)
	}
	fi, err := os.Lstat(canon)
	if err != nil {
		return "", errStorage("cannot inspect the source path", err)
	}
	if !fi.IsDir() {
		return "", contract.New(contract.CodeInvalidArgument, "the source path is not a directory")
	}
	return canon, nil
}

// bareLayout reports a directory with a git directory's HEAD, objects/
// and refs/.
func bareLayout(dfd int) bool {
	h, err1 := lstatAt(dfd, "HEAD")
	o, err2 := lstatAt(dfd, "objects")
	r, err3 := lstatAt(dfd, "refs")
	return err1 == nil && err2 == nil && err3 == nil && isRegStat(&h) && isDirStat(&o) && isDirStat(&r)
}

// checkAncestors refuses a canonical directory below a repository, found
// the way Git's discovery finds one (setup.c): an ancestor whose .git is
// a valid git directory (is_git_directory) or a .git file (a gitfile,
// which Git either follows or refuses: never a plain folder), or an
// ancestor that is itself a valid git directory (a bare repository). An
// empty or invalid .git directory is not a repository and the search
// continues upward, as Git's does. Unreadable ancestors are skipped.
func checkAncestors(canon string) error {
	for p := filepath.Dir(canon); ; p = filepath.Dir(p) {
		if ancestorRepository(p) {
			return errInsideRepository()
		}
		if p == filepath.Dir(p) {
			return nil
		}
	}
}

// ancestorRepository reports whether Git's discovery stops at directory
// p. Ancestors are outside the transferred tree, so symlinks are followed
// as Git follows them.
func ancestorRepository(p string) bool {
	g := filepath.Join(p, ".git")
	if fi, err := os.Stat(g); err == nil {
		switch {
		case fi.Mode().IsRegular():
			return true
		case fi.IsDir() && isGitDirectory(g):
			return true
		}
	}
	return isGitDirectory(p)
}

// isGitDirectory is Git's is_git_directory: a valid HEAD
// (validate_headref), and objects/ and refs/ that are searchable
// directories, found through commondir when one is present.
func isGitDirectory(dir string) bool {
	if !validHeadRef(filepath.Join(dir, "HEAD")) {
		return false
	}
	common := dir
	if b, err := os.ReadFile(filepath.Join(dir, "commondir")); err == nil {
		// Git's get_common_dir_noenv strips every trailing CR and LF
		// (a CRLF-terminated path is the same directory).
		c := strings.TrimRight(string(b), "\r\n")
		if c == "" {
			return false
		}
		if !filepath.IsAbs(c) {
			c = filepath.Join(dir, c)
		}
		common = c
	}
	for _, d := range []string{"objects", "refs"} {
		if unix.Access(filepath.Join(common, d), unix.X_OK) != nil {
			return false
		}
	}
	return true
}

// validHeadRef is Git's validate_headref: a symlink whose target starts
// with "refs/", or a file holding "ref:" (optional space, tab, CR or
// LF — not vertical tab or form feed) and a "refs/" target, or starting
// with a full hexadecimal object name.
func validHeadRef(p string) bool {
	fi, err := os.Lstat(p)
	if err != nil {
		return false
	}
	if fi.Mode()&os.ModeSymlink != 0 {
		t, err := os.Readlink(p)
		return err == nil && strings.HasPrefix(t, "refs/")
	}
	f, err := os.Open(p)
	if err != nil {
		return false
	}
	defer f.Close()
	buf := make([]byte, 255)
	n, _ := io.ReadFull(f, buf)
	s := string(buf[:n])
	if t, ok := strings.CutPrefix(s, "ref:"); ok {
		return strings.HasPrefix(strings.TrimLeft(t, " \t\n\r"), "refs/")
	}
	for _, size := range []int{40, 64} {
		if len(s) >= size && isHex(s[:size]) {
			return true
		}
	}
	return false
}

func isHex(s string) bool {
	for i := 0; i < len(s); i++ {
		c := s[i]
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f' || c >= 'A' && c <= 'F') {
			return false
		}
	}
	return true
}

// classify returns the kind of the canonical directory dfd.
func classify(dfd int) (dirKind, error) {
	st, err := lstatAt(dfd, ".git")
	switch {
	case err == nil && isDirStat(&st):
		return kindRepo, nil
	case err == nil && isLinkStat(&st):
		return 0, errUnsupported("a symlinked .git")
	case err == nil && isRegStat(&st):
		return 0, errUnsupported("a .git file (a linked worktree, a submodule or a separate git directory)")
	case err == nil:
		return 0, errUnsupported("a .git entry that is not a directory")
	case !errors.Is(err, unix.ENOENT):
		return 0, errStorage("cannot inspect the directory", err)
	}
	if bareLayout(dfd) {
		return 0, errUnsupported("a bare repository")
	}
	return kindPlain, nil
}

// redirectVars are the environment variables that relocate a
// repository's parts; they are honored only when they name exactly this
// repository's own locations (never followed elsewhere).
var redirectVars = []struct{ name, rel string }{
	{"GIT_DIR", ".git"}, {"GIT_WORK_TREE", ""}, {"GIT_COMMON_DIR", ".git"},
	{"GIT_INDEX_FILE", ".git/index"}, {"GIT_OBJECT_DIRECTORY", ".git/objects"},
}

func checkRedirects(env Env, root string) error {
	for _, v := range redirectVars {
		val := env.get(v.name)
		if val == "" {
			continue
		}
		if !filepath.IsAbs(val) || filepath.Clean(val) != filepath.Join(root, v.rel) {
			return errUnsupported(v.name + " redirects the repository")
		}
	}
	// GIT_SHALLOW_FILE makes Git read a shallow file from elsewhere (a
	// shallow repository, refused like .git/shallow).
	for _, v := range []string{"GIT_ALTERNATE_OBJECT_DIRECTORIES", "GIT_NAMESPACE", "GIT_SHALLOW_FILE"} {
		if env.get(v) != "" {
			return errUnsupported(v + " is set")
		}
	}
	return nil
}

// repo is an opened, eligible repository.
type repo struct {
	root   string
	rootFD int
	gitFD  int
	head   headState
	cfg    *gitConfig
	index  *gitIndex
	// indexRaw is the index file content (nil when absent).
	indexRaw []byte
	store    *filesystem.Storage
	cfgFiles []string
}

func (r *repo) close() {
	if r.gitFD >= 0 {
		closeFD(r.gitFD)
	}
	if r.rootFD >= 0 {
		closeFD(r.rootFD)
	}
	if r.store != nil {
		r.store.Close()
	}
}

// checkExtensions checks the repository format the way Git 2.43 reads it
// (setup.c): only from the repository's own .git/config, every
// occurrence. core.repositoryFormatVersion must parse as an int
// (git_config_int); Callsheet supports 0 and 1 (Git also accepts
// negative versions; they are refused). At version 0 Git honors noop,
// preciousObjects, partialClone and worktreeConfig, refuses objectFormat
// (a version-1 extension) and ignores any other extension; at version 1
// it also takes objectFormat (sha1 or sha256) and refuses any other
// extension (refStorage included: it is not a 2.43 extension). Callsheet
// further refuses partial clones, per-worktree configuration and SHA-256.
func checkExtensions(c *gitConfig) error {
	version := int64(0)
	for _, e := range c.entries {
		if !e.repo || e.key != "core.repositoryformatversion" {
			continue
		}
		n, ok := parseGitInt(e.value, math.MaxInt32)
		if e.blank || !ok || n < 0 || n > 1 {
			return errUnsupported("repository format version " + contract.SafeText(e.value, 8))
		}
		version = n
	}
	for _, e := range c.entries {
		name, ok := strings.CutPrefix(e.key, "extensions.")
		if !e.repo || !ok {
			continue
		}
		switch name {
		case "noop":
		case "preciousobjects":
			if !e.blank {
				if _, err := parseGitBool(e.value); err != nil {
					return unsupportedConfig("extensions.preciousObjects is not a boolean")
				}
			}
		case "partialclone":
			return errUnsupported("a partial clone")
		case "worktreeconfig":
			if b, err := parseGitBool(e.value); err != nil || b || e.blank {
				return errUnsupported("per-worktree configuration")
			}
		case "objectformat":
			// Git compares the name exactly (hash_algo_by_name).
			if version == 0 {
				return errUnsupported("extensions.objectFormat at repository format version 0")
			}
			if e.blank || e.value != "sha1" {
				return errUnsupported("an object format other than SHA-1")
			}
		default:
			if version >= 1 {
				return errUnsupported("the repository extension " + contract.SafeText(name, 32))
			}
		}
	}
	return nil
}

// checkConfig refuses configured unsupported features.
func checkConfig(c *gitConfig) error {
	// Every occurrence of every key Git validates while git status reads
	// its configuration (the checked-in oracle of Git 2.43.0) is parsed
	// with Git's rule for that key.
	if err := validateGitConfig(c); err != nil {
		return err
	}
	// The repository's own .git/config decides bareness and the work
	// tree; Git ignores those keys elsewhere.
	for _, e := range c.entries {
		if !e.repo {
			continue
		}
		switch e.key {
		case "core.bare":
			if b, err := parseGitBool(e.value); err != nil || e.blank || b {
				return errUnsupported("a bare repository (core.bare)")
			}
		case "core.worktree":
			return errUnsupported("core.worktree redirects the working tree")
		}
	}
	if err := checkExtensions(c); err != nil {
		return err
	}
	if b, err := c.boolean("core.sparsecheckout", false); err != nil || b {
		return errUnsupported("sparse checkout")
	}
	for _, e := range c.entries {
		switch {
		case strings.HasPrefix(e.key, "submodule.") && strings.Count(e.key, ".") >= 2:
			// A submodule.<name>.* entry configures a submodule; the
			// general submodule.* settings (submodule.recurse, ...) do
			// not.
			return errUnsupported("configured submodules")
		case strings.HasPrefix(e.key, "remote.") && (strings.HasSuffix(e.key, ".promisor") || strings.HasSuffix(e.key, ".partialclonefilter")):
			return errUnsupported("a partial clone")
		}
	}
	return nil
}

// gitmodulesDefines reports a .gitmodules content defining a submodule.
func gitmodulesDefines(b []byte) (bool, error) {
	found := false
	err := parseGitConfig(b, func(sec, sub, _, _ string, _ bool) error {
		if strings.EqualFold(sec, "submodule") && sub != "" {
			found = true
		}
		return nil
	})
	if err != nil {
		return false, errUnsupported("an unparsable .gitmodules")
	}
	return found, nil
}

// lfsConfigured reports a configured Git LFS filter.
func lfsConfigured(c *gitConfig) bool {
	for _, k := range []string{"filter.lfs.clean", "filter.lfs.smudge", "filter.lfs.process"} {
		if _, ok := c.get(k); ok {
			return true
		}
	}
	return false
}

// checkLFS refuses a repository with an active Git LFS filter: one that
// is configured and that any attributes file Git would read routes paths
// to (the system and global files, info/attributes, every .gitattributes
// of the working tree at any depth, and a tracked one missing from the
// working tree through its index blob). No filter runs; a pointer need
// not be valid.
func (r *repo) checkLFS(ctx context.Context, env Env) error {
	if !lfsConfigured(r.cfg) {
		return nil
	}
	lfs := errUnsupported("Git LFS")
	check := func(b []byte, macroOK bool) error {
		yes, err := assignsLFS(b, macroOK)
		if err != nil {
			return errUnsupported("an attribute macro that changes content conversion")
		}
		if yes {
			return lfs
		}
		return nil
	}
	fixed, err := fixedAttributeFiles(env, r.cfg, r.root)
	if err != nil {
		return err
	}
	fixed = append(fixed, filepath.Join(r.root, ".git", "info", "attributes"))
	for _, p := range fixed {
		b, err := os.ReadFile(p)
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) || errors.Is(err, unix.ENOTDIR) {
				continue
			}
			return errUnsupported("an attributes file that cannot be read")
		}
		if err := check(b, true); err != nil {
			return err
		}
	}
	seen := map[string]bool{}
	var walk func(fd int, rel string, depth int) error
	walk = func(fd int, rel string, depth int) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if depth > 4096 {
			return errUnsupported("a working tree nested too deeply")
		}
		names, err := readNames(fd)
		if err != nil {
			return errStorage("cannot read the working tree", err)
		}
		for _, n := range names {
			if n == ".git" {
				continue
			}
			st, err := lstatAt(fd, n)
			if err != nil {
				continue
			}
			switch {
			case n == ".gitattributes" && isRegStat(&st):
				b, _, err := readSmallFile(fd, n)
				if err != nil {
					return errStorage("cannot read the working tree", err)
				}
				seen[rel+n] = true
				if err := check(b, rel == ""); err != nil {
					return err
				}
			case isDirStat(&st):
				sub, err := openDirAt(fd, n)
				if err != nil {
					continue
				}
				err = walk(sub, rel+n+"/", depth+1)
				closeFD(sub)
				if err != nil {
					return err
				}
			}
		}
		return nil
	}
	if err := walk(r.rootFD, "", 0); err != nil {
		return err
	}
	for _, e := range r.index.entries {
		if path.Base(e.name) != ".gitattributes" || seen[e.name] || e.mode == 0o120000 {
			continue
		}
		b, err := readBlob(r.store, e.hash)
		if err != nil {
			return errUnsupported("an index blob that cannot be read")
		}
		if err := check(b, !strings.Contains(e.name, "/")); err != nil {
			return err
		}
	}
	return nil
}

// openRepo opens the repository rooted at the canonical root and refuses
// every unsupported layout or feature. Nothing is written.
func openRepo(ctx context.Context, env Env, root string) (_ *repo, err error) {
	r := &repo{root: root, rootFD: -1, gitFD: -1}
	defer func() {
		if err != nil {
			r.close()
		}
	}()
	if err := checkRedirects(env, root); err != nil {
		return nil, err
	}
	if r.rootFD, err = openPathDir(root); err != nil {
		return nil, errStorage("cannot open the repository", err)
	}
	if r.gitFD, err = openDirAt(r.rootFD, ".git"); err != nil {
		if errors.Is(err, unix.ELOOP) || errors.Is(err, unix.ENOTDIR) {
			return nil, errUnsupported("a symlinked .git")
		}
		return nil, errStorage("cannot open the repository's .git directory", err)
	}
	if _, err := lstatAt(r.gitFD, "commondir"); err == nil {
		return nil, errUnsupported("a linked worktree's git directory (commondir)")
	}
	for _, d := range []string{"objects", "refs"} {
		st, err := lstatAt(r.gitFD, d)
		if err != nil || !isDirStat(&st) {
			return nil, errUnsupported("missing or corrupt repository metadata")
		}
	}
	objFD, err := openDirAt(r.gitFD, "objects")
	if err != nil {
		return nil, errUnsupported("missing or corrupt repository metadata")
	}
	// An alternates entry is any line that is neither empty nor a
	// comment (Git's link_alt_odb_entries); its bytes are never trimmed,
	// so "\v" is an entry. Any entry, or an unreadable file, is refused.
	infoAlt := func(name string) bool {
		ifd, err := openDirAt(objFD, "info")
		if err != nil {
			return false
		}
		defer closeFD(ifd)
		b, found, err := readSmallFile(ifd, name)
		if err != nil {
			return true
		}
		if !found {
			return false
		}
		for _, line := range bytes.Split(b, []byte("\n")) {
			if len(line) > 0 && line[0] != '#' {
				return true
			}
		}
		return false
	}
	alternates := infoAlt("alternates") || infoAlt("http-alternates")
	promisor := false
	if pfd, err := openDirAt(objFD, "pack"); err == nil {
		names, _ := readNames(pfd)
		for _, n := range names {
			if strings.HasSuffix(n, ".promisor") {
				promisor = true
			}
		}
		closeFD(pfd)
	}
	closeFD(objFD)
	if alternates {
		return nil, errUnsupported("an alternate object store")
	}
	if promisor {
		return nil, errUnsupported("a partial clone")
	}
	// Git treats an existing shallow file as a shallow repository (an
	// empty one included) and refuses any line that is not an object
	// name ("bad shallow line"): any shallow entry is refused.
	if _, err := lstatAt(r.gitFD, "shallow"); !errors.Is(err, unix.ENOENT) {
		return nil, errUnsupported("a shallow clone")
	}
	if st, err := lstatAt(r.gitFD, "modules"); err == nil && isDirStat(&st) {
		return nil, errUnsupported("configured submodules")
	}
	if r.head, err = readHead(r.gitFD); err != nil {
		if errors.Is(err, errUnsafeMetadata) {
			return nil, errUnsupported("missing, symlinked or corrupt repository metadata")
		}
		return nil, errStorage("cannot read HEAD", err)
	}
	gitDir := filepath.Join(root, ".git")
	cfg, err := loadConfig(ctx, env, gitDir, r.head.branch())
	if err != nil {
		return nil, err
	}
	r.cfg, r.cfgFiles = cfg, cfg.files
	if err := checkConfig(cfg); err != nil {
		return nil, err
	}
	raw, found, err := readSmallFile(r.gitFD, "index")
	if err != nil {
		return nil, errUnsupported("an unreadable or symlinked index")
	}
	if found {
		r.indexRaw = raw
		if r.index, err = parseIndex(raw); err != nil {
			return nil, errUnsupported("an index that cannot be decoded")
		}
		if what := r.index.unsupported(); what != "" {
			return nil, errUnsupported(what)
		}
	} else {
		r.index = &gitIndex{version: 2}
	}
	if b, found, err := readSmallFile(r.rootFD, ".gitmodules"); err == nil && found {
		defines, err := gitmodulesDefines(b)
		if err != nil {
			return nil, err
		}
		if defines {
			return nil, errUnsupported("configured submodules (.gitmodules)")
		}
	}
	r.store = filesystem.NewStorageWithOptions(osfs.New(gitDir, osfs.WithBoundOS()), cache.NewObjectLRU(8<<20),
		filesystem.Options{LargeObjectThreshold: largeObject})
	if err := r.checkReplacements(ctx, env); err != nil {
		return nil, err
	}
	if err := r.checkLFS(ctx, env); err != nil {
		return nil, err
	}
	return r, nil
}
