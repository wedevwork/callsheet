package workspacetransfer

import (
	"context"
	"errors"
	"strings"

	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/filemode"
	"github.com/go-git/go-git/v5/plumbing/object"
	"golang.org/x/sys/unix"
)

// Replacement refs (git replace). Git consults them whenever it reads an
// object, unless GIT_NO_REPLACE_OBJECTS is set (any value) or
// core.useReplaceRefs is false; they live under refs/replace/
// (GIT_REPLACE_REF_BASE, which relocates them, is refused when set). Each
// ref below refs/replace/ names the object it replaces by the first 40
// hexadecimal digits (either case) of its last path component; other
// names are ignored. Git fails when two refs
// replace the same object, and when an object it reads has a replacement
// that is missing, of the wrong type, broken or cyclic. Callsheet pushes
// and compares the original objects, so it refuses any active
// replacement of the objects git status reads (HEAD's commit and its
// trees; Git itself reads the root tree there, and the subtrees whenever
// it compares them), as well as duplicate replacements. Replacements of
// other objects do not change what is transferred and are accepted, as
// Git accepts them.
func (r *repo) checkReplacements(ctx context.Context, env Env) error {
	// GIT_REPLACE_REF_BASE set to any value, the empty string included, is
	// refused: Git treats an empty base as every ref and aborts on a base
	// that is a complete ref name; the default base is the only one
	// supported.
	if _, set := env.Lookup("GIT_REPLACE_REF_BASE"); set {
		return errUnsupported("GIT_REPLACE_REF_BASE is set")
	}
	if _, off := env.Lookup("GIT_NO_REPLACE_OBJECTS"); off {
		return nil
	}
	if use, err := r.cfg.boolean("core.usereplacerefs", true); err == nil && !use {
		return nil
	}
	const base = "refs/replace/"
	replaced := map[plumbing.Hash]bool{}
	note := func(name string) error {
		rest, ok := strings.CutPrefix(name, base)
		if !ok {
			return nil
		}
		if i := strings.LastIndexByte(rest, '/'); i >= 0 {
			rest = rest[i+1:]
		}
		h, ok := parseHashAt([]byte(rest), 0)
		if !ok {
			return nil // Git warns about a bad replace ref name and ignores it
		}
		if replaced[h] {
			return errUnsupported("two replacement refs for the same object")
		}
		replaced[h] = true
		return nil
	}
	loose := map[string]bool{}
	var walk func(dir string, depth int) error
	walk = func(dir string, depth int) error {
		if depth > 64 {
			return errUnsupported("replacement refs nested too deeply")
		}
		fd, err := openRel(r.gitFD, dir)
		if errors.Is(err, unix.ENOENT) || errors.Is(err, unix.ENOTDIR) {
			return nil
		}
		if err != nil {
			return errUnsupported("replacement refs that cannot be read safely")
		}
		defer closeFD(fd)
		names, err := readNames(fd)
		if err != nil {
			return errUnsupported("replacement refs that cannot be read safely")
		}
		for _, n := range names {
			if err := ctx.Err(); err != nil {
				return err
			}
			st, err := lstatAt(fd, n)
			if err != nil {
				continue
			}
			switch {
			case isDirStat(&st):
				if err := walk(dir+"/"+n, depth+1); err != nil {
					return err
				}
			case isRegStat(&st):
				if !strings.HasSuffix(n, ".lock") {
					loose[dir+"/"+n] = true
					if err := note(dir + "/" + n); err != nil {
						return err
					}
				}
			default:
				return errUnsupported("replacement refs that cannot be read safely")
			}
		}
		return nil
	}
	if err := walk("refs/replace", 0); err != nil {
		return err
	}
	packed, err := readPackedRefs(r.gitFD)
	if err != nil {
		return errUnsupported("missing, symlinked or corrupt repository metadata")
	}
	for _, n := range packed.names {
		if !loose[n] {
			if err := note(n); err != nil {
				return err
			}
		}
	}
	if len(replaced) == 0 || r.head.unborn {
		return nil
	}
	refuse := errUnsupported("replacement refs (git replace) for the commit or trees git status reads")
	if replaced[r.head.hash] {
		return refuse
	}
	c, err := object.GetCommit(r.store, r.head.hash)
	if err != nil {
		return errUnsupported("a HEAD commit that cannot be read")
	}
	seen := map[plumbing.Hash]bool{}
	var trees func(h plumbing.Hash) error
	trees = func(h plumbing.Hash) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if seen[h] {
			return nil
		}
		seen[h] = true
		if replaced[h] {
			return refuse
		}
		t, err := object.GetTree(r.store, h)
		if err != nil {
			return errUnsupported("a HEAD tree that cannot be read")
		}
		for _, e := range t.Entries {
			if e.Mode == filemode.Dir {
				if err := trees(e.Hash); err != nil {
					return err
				}
			}
		}
		return nil
	}
	return trees(c.TreeHash)
}
