package reale2e

import (
	"bytes"
	"compress/zlib"
	"crypto/sha1"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"

	git "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/filemode"
	gitconfig "github.com/go-git/go-git/v5/plumbing/format/config"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/go-git/go-git/v5/plumbing/storer"
)

// The evidence repository (design 12a-real-e2e, Evidence and checker):
// workspace/repo.git is a bare go-git repository holding only the seed
// and the four published results with their complete ancestry, as loose
// objects under five fixed refs and HEAD pointing to the seed. The
// collector writes it through go-git's encoded-object interfaces; the
// checker validates its layout and bounds before go-git opens it, and
// never fetches or invokes Git.

// Evidence repository bounds.
const (
	// maxRepoFileBytes bounds the regular-file bytes of repo.git.
	maxRepoFileBytes = 4 << 20
	// maxDecodedBytes bounds one decoded object and their aggregate.
	maxDecodedBytes = 64 << 20
)

// evidenceRefs are the fixed refs, seed first.
var evidenceRefs = []string{"refs/heads/seed", "refs/heads/hop-01", "refs/heads/hop-02", "refs/heads/hop-03", "refs/heads/hop-04"}

// closure returns every object reachable from roots in src (commits with
// their parents, trees and blobs), each once.
func closure(src storer.EncodedObjectStorer, roots []plumbing.Hash) ([]plumbing.Hash, error) {
	seen := map[plumbing.Hash]bool{}
	var out []plumbing.Hash
	var visit func(h plumbing.Hash, t plumbing.ObjectType) error
	visit = func(h plumbing.Hash, t plumbing.ObjectType) error {
		if seen[h] {
			return nil
		}
		seen[h] = true
		out = append(out, h)
		switch t {
		case plumbing.CommitObject:
			c, err := object.GetCommit(src, h)
			if err != nil {
				return fmt.Errorf("commit %s: %w", h, err)
			}
			if err := visit(c.TreeHash, plumbing.TreeObject); err != nil {
				return err
			}
			for _, p := range c.ParentHashes {
				if err := visit(p, plumbing.CommitObject); err != nil {
					return err
				}
			}
		case plumbing.TreeObject:
			tr, err := object.GetTree(src, h)
			if err != nil {
				return fmt.Errorf("tree %s: %w", h, err)
			}
			for _, e := range tr.Entries {
				switch {
				case e.Mode == filemode.Dir:
					err = visit(e.Hash, plumbing.TreeObject)
				case e.Mode == filemode.Submodule:
					return fmt.Errorf("tree %s has a submodule", h)
				default:
					err = visit(e.Hash, plumbing.BlobObject)
				}
				if err != nil {
					return err
				}
			}
		default:
			if _, err := src.EncodedObject(plumbing.BlobObject, h); err != nil {
				return fmt.Errorf("blob %s: %w", h, err)
			}
		}
		return nil
	}
	for _, r := range roots {
		if err := visit(r, plumbing.CommitObject); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// copyObjects copies the closure of roots from src into dst.
func copyObjects(src, dst storer.EncodedObjectStorer, roots []plumbing.Hash) error {
	hashes, err := closure(src, roots)
	if err != nil {
		return err
	}
	for _, h := range hashes {
		o, err := src.EncodedObject(plumbing.AnyObject, h)
		if err != nil {
			return err
		}
		if _, err := dst.SetEncodedObject(o); err != nil {
			return err
		}
	}
	return nil
}

// writeEvidenceRepo creates dir as a bare repository with the closure of
// commits (seed first, then the hop results in order) copied from src as
// loose objects, the fixed refs for exactly those commits and HEAD
// pointing to the seed.
func writeEvidenceRepo(dir string, src storer.EncodedObjectStorer, commits []plumbing.Hash) error {
	if len(commits) == 0 || len(commits) > len(evidenceRefs) {
		return errors.New("evidence repository: want the seed and at most four results")
	}
	r, err := git.PlainInit(dir, true)
	if err != nil {
		return fmt.Errorf("evidence repository: %w", err)
	}
	if err := copyObjects(src, r.Storer, commits); err != nil {
		return fmt.Errorf("evidence repository: %w", err)
	}
	for i, c := range commits {
		if err := r.Storer.SetReference(plumbing.NewHashReference(plumbing.ReferenceName(evidenceRefs[i]), c)); err != nil {
			return fmt.Errorf("evidence repository: %w", err)
		}
	}
	return r.Storer.SetReference(plumbing.NewSymbolicReference(plumbing.HEAD, plumbing.ReferenceName(evidenceRefs[0])))
}

// looseObjectRE is objects/<2 hex>/<38 hex>.
var looseObjectRE = regexp.MustCompile(`^objects/[0-9a-f]{2}/[0-9a-f]{38}$`)

// repoDirs are the directories PlainInit creates; any directory is
// tolerated only when it is one of these, a loose-object fan-out
// directory, or empty.
var repoDirs = map[string]bool{"objects": true, "objects/info": true, "objects/pack": true, "refs": true, "refs/heads": true, "refs/tags": true}

// fanoutRE is a loose-object fan-out directory.
var fanoutRE = regexp.MustCompile(`^objects/[0-9a-f]{2}$`)

// Repository layout errors.
var (
	errRepoLayout = errors.New("evidence repository layout is not allowed")
	errRepoSize   = errors.New("evidence repository exceeds its byte bounds")
)

// validateRepoLayout walks root (already Lstat-checked as a directory)
// and accepts only HEAD, config, the fixed loose refs and loose objects
// as regular files, their parent directories and empty directories: no
// symlink, hook, alternate, pack, packed ref, shallow file or log. The
// regular-file bytes must not exceed limit. It returns the loose object
// paths.
func validateRepoLayout(root string, limit int64) ([]string, error) {
	allowedFile := map[string]bool{"HEAD": true, "config": true}
	for _, r := range evidenceRefs {
		allowedFile[r] = true
	}
	var objects []string
	var total int64
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel := filepath.ToSlash(strings.TrimPrefix(strings.TrimPrefix(p, root), string(filepath.Separator)))
		if rel == "" {
			return nil
		}
		st, err := os.Lstat(p)
		if err != nil {
			return err
		}
		switch {
		case st.Mode()&fs.ModeSymlink != 0:
			return fmt.Errorf("%w: symlink %s", errRepoLayout, rel)
		case st.IsDir():
			if repoDirs[rel] || fanoutRE.MatchString(rel) {
				return nil
			}
			entries, err := os.ReadDir(p)
			if err != nil {
				return err
			}
			if len(entries) != 0 {
				return fmt.Errorf("%w: directory %s", errRepoLayout, rel)
			}
			return nil
		case !st.Mode().IsRegular():
			return fmt.Errorf("%w: special file %s", errRepoLayout, rel)
		case !allowedFile[rel] && !looseObjectRE.MatchString(rel):
			return fmt.Errorf("%w: file %s", errRepoLayout, rel)
		}
		total += st.Size()
		if total > limit {
			return errRepoSize
		}
		if looseObjectRE.MatchString(rel) {
			objects = append(objects, rel)
		}
		return nil
	})
	return objects, err
}

// errObjectBound is a decoded object, or their aggregate, over its bound.
var errObjectBound = errors.New("evidence repository objects exceed the decoded-object bound")

// verifyLooseObjects inflates every loose object with bounded readers:
// each declared size and the aggregate of decoded bytes must stay within
// limit, every stream must match its declared size, and its SHA-1 must
// equal its file name. Corrupt objects fail before go-git reads them.
func verifyLooseObjects(root string, objects []string, limit int64) error {
	var total int64
	for _, rel := range objects {
		n, err := verifyLooseObject(filepath.Join(root, filepath.FromSlash(rel)), strings.ReplaceAll(strings.TrimPrefix(rel, "objects/"), "/", ""), limit-total)
		if err != nil {
			return err
		}
		total += n
	}
	return nil
}

// verifyLooseObject checks one loose object against name with room
// decoded bytes left and returns its decoded size.
func verifyLooseObject(path, name string, room int64) (int64, error) {
	f, err := os.Open(path)
	if err != nil {
		return 0, err
	}
	defer f.Close()
	zr, err := zlib.NewReader(f)
	if err != nil {
		return 0, fmt.Errorf("evidence repository object %s is corrupt", name)
	}
	defer zr.Close()
	header := make([]byte, 0, 32)
	one := make([]byte, 1)
	for {
		if _, err := io.ReadFull(zr, one); err != nil {
			return 0, fmt.Errorf("evidence repository object %s is corrupt", name)
		}
		if one[0] == 0 {
			break
		}
		if len(header) >= 32 {
			return 0, fmt.Errorf("evidence repository object %s is corrupt", name)
		}
		header = append(header, one[0])
	}
	typ, sizeText, ok := strings.Cut(string(header), " ")
	size, err := strconv.ParseInt(sizeText, 10, 64)
	if !ok || err != nil || size < 0 || (typ != "blob" && typ != "tree" && typ != "commit" && typ != "tag") {
		return 0, fmt.Errorf("evidence repository object %s is corrupt", name)
	}
	if size > room || size > maxDecodedBytes {
		return 0, errObjectBound
	}
	h := sha1.New()
	h.Write(header)
	h.Write([]byte{0})
	n, err := io.Copy(h, io.LimitReader(zr, size+1))
	if err != nil || n != size {
		return 0, fmt.Errorf("evidence repository object %s does not match its declared size", name)
	}
	if hex.EncodeToString(h.Sum(nil)) != name {
		return 0, fmt.Errorf("evidence repository object %s does not match its hash", name)
	}
	return size, nil
}

// maxRepoConfig bounds the evidence repository's config file.
const maxRepoConfig = 4 << 10

// validateRepoConfig accepts only the minimal configuration
// git.PlainInit(path, true) generates: one [core] section with bare =
// true and, optionally, repositoryformatversion = 0. Any other section
// (remote, include, includeIf, extensions, worktree, ...), subsection,
// key or value, or a repeated key, is refused before go-git reads it.
func validateRepoConfig(path string) error {
	data, err := readBounded(path, maxRepoConfig)
	if err != nil {
		return fmt.Errorf("%w: config: %v", errRepoLayout, err)
	}
	cfg := gitconfig.New()
	if err := gitconfig.NewDecoder(bytes.NewReader(data)).Decode(cfg); err != nil {
		return fmt.Errorf("%w: config is malformed", errRepoLayout)
	}
	if len(cfg.Sections) != 1 || !strings.EqualFold(cfg.Sections[0].Name, "core") || len(cfg.Sections[0].Subsections) != 0 {
		return fmt.Errorf("%w: config may hold only the core section", errRepoLayout)
	}
	allowed := map[string]string{"bare": "true", "repositoryformatversion": "0"}
	seen := map[string]bool{}
	for _, o := range cfg.Sections[0].Options {
		k := strings.ToLower(o.Key)
		if want, ok := allowed[k]; !ok || o.Value != want || seen[k] {
			return fmt.Errorf("%w: config option core.%s is not allowed", errRepoLayout, contractSafe(o.Key))
		}
		seen[k] = true
	}
	if !seen["bare"] {
		return fmt.Errorf("%w: config must declare core.bare = true", errRepoLayout)
	}
	return nil
}

// contractSafe bounds a config key for a diagnostic.
func contractSafe(s string) string {
	if len(s) > 32 {
		s = s[:32]
	}
	return strings.Map(func(r rune) rune {
		if r < 0x20 || r > 0x7e {
			return '?'
		}
		return r
	}, s)
}

// repoView is the checker's read-only view of a validated evidence
// repository.
type repoView struct {
	repo *git.Repository
	refs map[string]plumbing.Hash
}

// openEvidenceRepo validates root's layout, size and objects, opens it
// and resolves exactly the five fixed refs (HEAD to the seed).
func openEvidenceRepo(root string) (*repoView, error) {
	objects, err := validateRepoLayout(root, maxRepoFileBytes)
	if err != nil {
		return nil, err
	}
	if err := validateRepoConfig(filepath.Join(root, "config")); err != nil {
		return nil, err
	}
	if err := verifyLooseObjects(root, objects, maxDecodedBytes); err != nil {
		return nil, err
	}
	r, err := git.PlainOpen(root)
	if err != nil {
		return nil, fmt.Errorf("evidence repository: %w", err)
	}
	v := &repoView{repo: r, refs: map[string]plumbing.Hash{}}
	iter, err := r.References()
	if err != nil {
		return nil, fmt.Errorf("evidence repository: %w", err)
	}
	err = iter.ForEach(func(ref *plumbing.Reference) error {
		name := ref.Name().String()
		if name == "HEAD" {
			if ref.Type() != plumbing.SymbolicReference || ref.Target().String() != evidenceRefs[0] {
				return errors.New("evidence repository HEAD must point to refs/heads/seed")
			}
			return nil
		}
		if ref.Type() != plumbing.HashReference {
			return fmt.Errorf("evidence repository ref %s is not a hash ref", name)
		}
		v.refs[name] = ref.Hash()
		return nil
	})
	if err != nil {
		return nil, err
	}
	for name := range v.refs {
		ok := false
		for _, r := range evidenceRefs {
			ok = ok || r == name
		}
		if !ok {
			return nil, fmt.Errorf("evidence repository has the extra ref %s", name)
		}
	}
	return v, nil
}

// commit returns the commit of ref, or false when the ref is absent.
func (v *repoView) commit(ref string) (*object.Commit, bool, error) {
	h, ok := v.refs[ref]
	if !ok {
		return nil, false, nil
	}
	c, err := v.repo.CommitObject(h)
	if err != nil {
		return nil, true, fmt.Errorf("evidence repository commit %s: %w", h, err)
	}
	return c, true, nil
}

// treeFiles returns every regular file path of tree with its blob bytes
// (at most limit bytes in all).
func treeFiles(t *object.Tree, limit int64) (map[string][]byte, error) {
	out := map[string][]byte{}
	var total int64
	err := t.Files().ForEach(func(f *object.File) error {
		if f.Mode != filemode.Regular && f.Mode != filemode.Executable {
			return fmt.Errorf("tree entry %s is not a regular file", f.Name)
		}
		total += f.Size
		if total > limit {
			return errObjectBound
		}
		r, err := f.Reader()
		if err != nil {
			return err
		}
		defer r.Close()
		var b bytes.Buffer
		if _, err := io.Copy(&b, io.LimitReader(r, f.Size+1)); err != nil {
			return err
		}
		out[f.Name] = b.Bytes()
		return nil
	})
	return out, err
}

// changedPaths compares two file maps and returns the sorted paths that
// were added, removed or changed.
func changedPaths(before, after map[string][]byte) []string {
	var out []string
	for p, b := range after {
		if a, ok := before[p]; !ok || !bytes.Equal(a, b) {
			out = append(out, p)
		}
	}
	for p := range before {
		if _, ok := after[p]; !ok {
			out = append(out, p)
		}
	}
	sort.Strings(out)
	return out
}
