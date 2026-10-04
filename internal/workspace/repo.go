package workspace

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/filemode"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/go-git/go-git/v5/plumbing/storer"

	"github.com/wedevwork/callsheet/internal/contract"
)

// Storage layout under the private plane state root:
//
//	workspaces/<name>/identity.json          schema, name, instance, created_at
//	workspaces/<name>/CURRENT                one generation token and LF
//	workspaces/<name>/generations/<gen>/repo.git/
//	workspaces/<name>/generations/<gen>/refs.json
//	workspaces/.create-<token>/              unpublished create staging
//	workspaces/.removed-<token>/             committed removal awaiting cleanup
const (
	dirName         = "workspaces"
	identityName    = "identity.json"
	currentName     = "CURRENT"
	currentTmpName  = "CURRENT.tmp"
	generationsName = "generations"
	repoName        = "repo.git"
	refsName        = "refs.json"
	createPrefix    = ".create-"
	removedPrefix   = ".removed-"
	schemaVersion   = 1

	// headContent is the only HEAD: symbolic refs/heads/main.
	headContent = "ref: " + contract.DefaultBranchRef + "\n"
	// configContent is the only repository configuration: the managed
	// bare-repository settings, no hooks, remotes or alternates.
	configContent = "[core]\n\trepositoryformatversion = 0\n\tfilemode = true\n\tbare = true\n"
	// maxMetaFile bounds identity.json and CURRENT; refs.json is bounded
	// by maxRefsFile.
	maxMetaFile = 4 << 10
	maxRefsFile = 64 << 20
)

// errCorrupt marks invalid managed state: never repaired or regenerated.
type errCorrupt struct {
	path   string
	reason string
}

func (e errCorrupt) Error() string { return e.path + ": " + e.reason }

func corrupt(path, format string, args ...any) error {
	return errCorrupt{path: path, reason: fmt.Sprintf(format, args...)}
}

// identity is identity.json.
type identity struct {
	Schema    int    `json:"schema"`
	Name      string `json:"name"`
	Instance  string `json:"instance"`
	CreatedAt string `json:"created_at"`
}

// taskMeta is one refs.json entry. PublicationID (iteration 10b) is the
// publication transaction's provenance of a ref a task published; planted
// refs without it stay readable and prunable but never satisfy an intent.
type taskMeta struct {
	Commit        string `json:"commit"`
	PublishedAt   string `json:"published_at"`
	PublicationID string `json:"publication_id,omitempty"`
}

// refsDoc is refs.json: the plane publication time of every task ref.
type refsDoc struct {
	Schema   int                 `json:"schema"`
	TaskRefs map[string]taskMeta `json:"task_refs"`
}

func encodeJSON(v any) []byte {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		panic(err) // unreachable: fixed types
	}
	return append(b, '\n')
}

// readBounded reads a regular, private file of at most limit bytes.
func readBounded(p string, limit int64) ([]byte, error) {
	fi, err := os.Lstat(p)
	if err != nil {
		return nil, err
	}
	if err := checkPrivate(p, fi); err != nil {
		return nil, err
	}
	if !fi.Mode().IsRegular() {
		return nil, errUnsafe{p}
	}
	f, err := os.Open(p)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(b)) > limit {
		return nil, corrupt(p, "larger than %d bytes", limit)
	}
	return b, nil
}

func strictJSON(b []byte, v any) error {
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return err
	}
	if _, err := dec.Token(); err != io.EOF {
		return errors.New("trailing data")
	}
	return nil
}

func readIdentity(p, name string) (identity, time.Time, error) {
	var id identity
	b, err := readBounded(p, maxMetaFile)
	if err != nil {
		return id, time.Time{}, err
	}
	if err := strictJSON(b, &id); err != nil {
		return id, time.Time{}, corrupt(p, "malformed identity: %v", err)
	}
	t, ok := contract.ParseTime(id.CreatedAt)
	if id.Schema != schemaVersion || id.Name != name || !contract.ValidWorkspaceToken(id.Instance) || !ok {
		return id, time.Time{}, corrupt(p, "invalid identity (schema, name, instance or created_at)")
	}
	return id, t, nil
}

// readCurrent reads CURRENT: exactly one 32-hex token and LF.
func readCurrent(p string) (string, error) {
	b, err := readBounded(p, maxMetaFile)
	if err != nil {
		return "", err
	}
	tok, ok := strings.CutSuffix(string(b), "\n")
	if !ok || !contract.ValidWorkspaceToken(tok) {
		return "", corrupt(p, "CURRENT must hold exactly one generation token and a newline")
	}
	return tok, nil
}

func readRefsDoc(p string) (map[string]taskMeta, error) {
	b, err := readBounded(p, maxRefsFile)
	if err != nil {
		return nil, err
	}
	var doc refsDoc
	if err := strictJSON(b, &doc); err != nil {
		return nil, corrupt(p, "malformed refs.json: %v", err)
	}
	if doc.Schema != schemaVersion || doc.TaskRefs == nil {
		return nil, corrupt(p, "refs.json needs schema 1 and task_refs")
	}
	for name, m := range doc.TaskRefs {
		if _, ok := contract.ParseTime(m.PublishedAt); !contract.ValidTaskRef(name) || !contract.ValidCommitHash(m.Commit) || !ok ||
			(m.PublicationID != "" && !contract.ValidWorkspaceToken(m.PublicationID)) {
			return nil, corrupt(p, "refs.json has an invalid task entry")
		}
	}
	return doc.TaskRefs, nil
}

func encodeRefsDoc(tasks map[string]taskMeta) []byte {
	if tasks == nil {
		tasks = map[string]taskMeta{}
	}
	return encodeJSON(refsDoc{Schema: schemaVersion, TaskRefs: tasks})
}

// initRepo creates an empty bare repository in the new directory repo:
// HEAD (symbolic main), the managed config, objects/pack and refs/heads.
func (d *deps) initRepo(repo string) error {
	for _, p := range []string{repo, filepath.Join(repo, "objects"), filepath.Join(repo, "objects", "pack"), filepath.Join(repo, "refs"), filepath.Join(repo, "refs", "heads")} {
		if err := d.mkdir(p); err != nil {
			return err
		}
	}
	if err := d.writeFile(filepath.Join(repo, "HEAD"), []byte(headContent)); err != nil {
		return err
	}
	return d.writeFile(filepath.Join(repo, "config"), []byte(configContent))
}

// refPath is the loose ref file of a validated full ref name.
func refPath(repo, name string) string { return filepath.Join(repo, filepath.FromSlash(name)) }

// writeRef writes "<hash>\n" as the loose ref name in a private staged
// generation, creating its parent directories.
func (d *deps) writeRef(repo, name string, h plumbing.Hash) error {
	p := refPath(repo, name)
	if err := d.mkdirAll(filepath.Dir(p)); err != nil {
		return err
	}
	if _, err := os.Lstat(p); err == nil {
		if err := d.remove(p); err != nil {
			return err
		}
	}
	return d.writeFile(p, []byte(h.String()+"\n"))
}

// deleteRef removes a loose ref and its now-empty parent directories up to
// its namespace root, so a later ref cannot collide with a leftover
// directory.
func (d *deps) deleteRef(repo, name string) error {
	p := refPath(repo, name)
	if err := d.remove(p); err != nil {
		return err
	}
	stop := refPath(repo, strings.TrimSuffix(contract.BranchPrefix, "/"))
	if strings.HasPrefix(name, contract.TaskRefPrefix) {
		stop = refPath(repo, strings.TrimSuffix(contract.TaskRefPrefix, "/"))
	}
	for dir := filepath.Dir(p); dir != stop && strings.HasPrefix(dir, stop); dir = filepath.Dir(dir) {
		es, err := os.ReadDir(dir)
		if err != nil || len(es) > 0 {
			return err
		}
		if err := d.remove(dir); err != nil {
			return err
		}
	}
	return nil
}

// validStoredRef is the stored-ref contract: a portable branch or a task
// ref, also valid for go-git.
func validStoredRef(name string) bool {
	if !contract.ValidBranchRef(name) && !contract.ValidTaskRef(name) {
		return false
	}
	return plumbing.ReferenceName(name).Validate() == nil
}

// readRefs walks refs/ of repo: every file must be a loose hash ref with a
// valid stored name; directories only on those names' paths. Symbolic,
// packed or unexpected refs are corrupt.
func readRefs(repo string) (map[string]plumbing.Hash, error) {
	out := map[string]plumbing.Hash{}
	var walk func(dir, rel string) error
	walk = func(dir, rel string) error {
		es, err := readDir(dir)
		if err != nil {
			return err
		}
		for _, e := range es {
			p := filepath.Join(dir, e.Name())
			name := rel + "/" + e.Name()
			fi, err := os.Lstat(p)
			if err != nil {
				return err
			}
			if err := checkPrivate(p, fi); err != nil {
				return err
			}
			if fi.IsDir() {
				if !refDirAllowed(name) {
					return corrupt(p, "unexpected ref directory")
				}
				if err := walk(p, name); err != nil {
					return err
				}
				continue
			}
			if !validStoredRef(name) {
				return corrupt(p, "ref name violates the portable branch-name contract")
			}
			b, err := readBounded(p, 128)
			if err != nil {
				return err
			}
			hex, ok := strings.CutSuffix(string(b), "\n")
			if !ok || !contract.ValidCommitHash(hex) {
				return corrupt(p, "a ref must hold one full hash and a newline (no symbolic refs)")
			}
			out[name] = plumbing.NewHash(hex)
		}
		return nil
	}
	return out, walk(filepath.Join(repo, "refs"), "refs")
}

// refDirAllowed admits refs/heads and its subdirectories and
// refs/callsheet[/tasks].
func refDirAllowed(name string) bool {
	switch name {
	case "refs/heads", "refs/callsheet", "refs/callsheet/tasks":
		return true
	}
	return strings.HasPrefix(name, contract.BranchPrefix)
}

// namespaceConflict reports whether name is a path prefix of an existing
// ref or has one as a prefix.
func namespaceConflict(refs map[string]plumbing.Hash, name string) bool {
	for o := range refs {
		if o != name && (strings.HasPrefix(o, name+"/") || strings.HasPrefix(name, o+"/")) {
			return true
		}
	}
	return false
}

// checkRepoLayout validates repo's directory structure: exactly HEAD,
// config, objects and refs; the managed HEAD and config bytes; loose
// objects and packs only; private modes; no links or special files.
func checkRepoLayout(repo string) error {
	es, err := readDir(repo)
	if err != nil {
		return err
	}
	seen := map[string]bool{}
	for _, e := range es {
		p := filepath.Join(repo, e.Name())
		fi, err := os.Lstat(p)
		if err != nil {
			return err
		}
		if err := checkPrivate(p, fi); err != nil {
			return err
		}
		switch e.Name() {
		case "HEAD", "config":
			if !fi.Mode().IsRegular() {
				return corrupt(p, "must be a regular file")
			}
		case "objects", "refs":
			if !fi.IsDir() {
				return corrupt(p, "must be a directory")
			}
		default:
			return corrupt(p, "unexpected repository entry (packed refs, hooks, alternates, logs and other files are not managed)")
		}
		seen[e.Name()] = true
	}
	for _, n := range []string{"HEAD", "config", "objects", "refs"} {
		if !seen[n] {
			return corrupt(filepath.Join(repo, n), "missing")
		}
	}
	for n, want := range map[string]string{"HEAD": headContent, "config": configContent} {
		b, err := readBounded(filepath.Join(repo, n), maxMetaFile)
		if err != nil {
			return err
		}
		if string(b) != want {
			return corrupt(filepath.Join(repo, n), "not the managed content")
		}
	}
	return checkObjectsLayout(filepath.Join(repo, "objects"))
}

func isHex(s string) bool {
	for i := 0; i < len(s); i++ {
		c := s[i]
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			return false
		}
	}
	return true
}

func checkObjectsLayout(objects string) error {
	es, err := readDir(objects)
	if err != nil {
		return err
	}
	for _, e := range es {
		p := filepath.Join(objects, e.Name())
		fi, err := os.Lstat(p)
		if err != nil {
			return err
		}
		if err := checkPrivate(p, fi); err != nil {
			return err
		}
		n := e.Name()
		switch {
		case n == "pack" && fi.IsDir():
			if err := checkEntries(p, func(name string, fi fs.FileInfo) bool {
				base, ext, _ := strings.Cut(name, ".")
				h, ok := strings.CutPrefix(base, "pack-")
				return fi.Mode().IsRegular() && ok && len(h) == 40 && isHex(h) && (ext == "pack" || ext == "idx" || ext == "rev")
			}); err != nil {
				return err
			}
		case len(n) == 2 && isHex(n) && fi.IsDir():
			if err := checkEntries(p, func(name string, fi fs.FileInfo) bool {
				return fi.Mode().IsRegular() && len(name) == 38 && isHex(name)
			}); err != nil {
				return err
			}
		default:
			return corrupt(p, "unexpected object store entry")
		}
	}
	return nil
}

func checkEntries(dir string, ok func(string, fs.FileInfo) bool) error {
	es, err := readDir(dir)
	if err != nil {
		return err
	}
	for _, e := range es {
		p := filepath.Join(dir, e.Name())
		fi, err := os.Lstat(p)
		if err != nil {
			return err
		}
		if err := checkPrivate(p, fi); err != nil {
			return err
		}
		if !ok(e.Name(), fi) {
			return corrupt(p, "unexpected object store entry")
		}
	}
	return nil
}

// genState is a validated generation: its refs and task metadata.
type genState struct {
	refs  map[string]plumbing.Hash
	tasks map[string]taskMeta
}

// closureStats counts one validation walk.
type closureStats struct {
	objects int
	bytes   int64
}

// validateGeneration checks a whole generation directory: exactly repo.git
// and refs.json, the repository layout, every ref name, task metadata that
// matches the task refs one to one, and the complete typed object closure
// of every ref.
func (d *deps) validateGeneration(ctx context.Context, gen string, st *closureStats) (genState, error) {
	var gs genState
	if err := checkGenerationEntry(gen); err != nil {
		return gs, err
	}
	es, err := readDir(gen)
	if err != nil {
		return gs, err
	}
	names := make([]string, 0, len(es))
	for _, e := range es {
		p := filepath.Join(gen, e.Name())
		fi, err := os.Lstat(p)
		if err != nil {
			return gs, err
		}
		if err := checkPrivate(p, fi); err != nil {
			return gs, err
		}
		names = append(names, e.Name())
	}
	if strings.Join(names, ",") != refsName+","+repoName {
		return gs, corrupt(gen, "a generation holds exactly repo.git and refs.json")
	}
	repo := filepath.Join(gen, repoName)
	if err := checkRepoLayout(repo); err != nil {
		return gs, err
	}
	if gs.refs, err = readRefs(repo); err != nil {
		return gs, err
	}
	if gs.tasks, err = readRefsDoc(filepath.Join(gen, refsName)); err != nil {
		return gs, err
	}
	for name, h := range gs.refs {
		if contract.ValidTaskRef(name) {
			m, ok := gs.tasks[name]
			if !ok || m.Commit != h.String() {
				return gs, corrupt(filepath.Join(gen, refsName), "a task ref has no matching publication metadata")
			}
		}
	}
	for name := range gs.tasks {
		if _, ok := gs.refs[name]; !ok {
			return gs, corrupt(filepath.Join(gen, refsName), "publication metadata names no task ref")
		}
	}
	s, sfs := openStorage(repo, d)
	defer closeStorage(s, sfs)
	if st == nil {
		st = &closureStats{}
	}
	if _, err := walkClosure(ctx, s, tips(gs.refs), st); err != nil {
		return gs, err
	}
	d.emit("visited-objects", int64(st.objects))
	d.emit("visited-bytes", st.bytes)
	return gs, nil
}

// tips returns the ref hashes in ref-name order.
func tips(refs map[string]plumbing.Hash) []plumbing.Hash {
	names := make([]string, 0, len(refs))
	for n := range refs {
		names = append(names, n)
	}
	sort.Strings(names)
	out := make([]plumbing.Hash, 0, len(names))
	for _, n := range names {
		out = append(out, refs[n])
	}
	return out
}

// errClosure marks a closure violation (receive reports "incomplete
// object closure").
type errClosure struct{ reason string }

func (e errClosure) Error() string { return "incomplete object closure: " + e.reason }

type pending struct {
	h plumbing.Hash
	t plumbing.ObjectType
}

// walkClosure validates the complete typed closure of the commit tips in
// s and returns it in traversal order: a tip and every parent is a commit,
// every commit/subtree a tree, every file entry a blob; every object
// exists, decodes and hashes to its ID; gitlinks and unsupported modes
// are rejected; tree entry names are safe single components. A visited
// map of validated types bounds the walk: a repeated hash is traversed
// once, but every edge reaching it must still require the type it was
// validated as. ctx is checked per object.
func walkClosure(ctx context.Context, s storer.EncodedObjectStorer, roots []plumbing.Hash, st *closureStats) ([]plumbing.Hash, error) {
	visited := map[plumbing.Hash]plumbing.ObjectType{}
	var order []plumbing.Hash
	stack := make([]pending, 0, len(roots))
	for i := len(roots) - 1; i >= 0; i-- {
		stack = append(stack, pending{roots[i], plumbing.CommitObject})
	}
	for len(stack) > 0 {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		p := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		if t, ok := visited[p.h]; ok {
			if t != p.t {
				return nil, errClosure{fmt.Sprintf("%s is a %s, want a %s", p.h, t, p.t)}
			}
			continue
		}
		// Recorded before loading: the checks below either confirm p.t or
		// end the walk.
		visited[p.h] = p.t
		o, err := s.EncodedObject(plumbing.AnyObject, p.h)
		if err != nil {
			if errors.Is(err, plumbing.ErrObjectNotFound) {
				return nil, errClosure{fmt.Sprintf("missing %s %s", p.t, p.h)}
			}
			return nil, err
		}
		if o.Type() != p.t {
			return nil, errClosure{fmt.Sprintf("%s is a %s, want a %s", p.h, o.Type(), p.t)}
		}
		if err := verifyHash(o, st); err != nil {
			return nil, err
		}
		order = append(order, p.h)
		switch p.t {
		case plumbing.CommitObject:
			c := &object.Commit{}
			if err := c.Decode(o); err != nil {
				return nil, errClosure{"malformed commit " + p.h.String()}
			}
			stack = append(stack, pending{c.TreeHash, plumbing.TreeObject})
			for i := len(c.ParentHashes) - 1; i >= 0; i-- {
				stack = append(stack, pending{c.ParentHashes[i], plumbing.CommitObject})
			}
		case plumbing.TreeObject:
			t := &object.Tree{}
			if err := t.Decode(o); err != nil {
				return nil, errClosure{"malformed tree " + p.h.String()}
			}
			for i := len(t.Entries) - 1; i >= 0; i-- {
				e := t.Entries[i]
				if !safeEntryName(e.Name) {
					return nil, errClosure{"unsafe tree entry name in " + p.h.String()}
				}
				switch e.Mode {
				case filemode.Dir:
					stack = append(stack, pending{e.Hash, plumbing.TreeObject})
				case filemode.Regular, filemode.Deprecated, filemode.Executable, filemode.Symlink:
					stack = append(stack, pending{e.Hash, plumbing.BlobObject})
				default:
					return nil, errClosure{fmt.Sprintf("unsupported entry mode %s in %s", e.Mode, p.h)}
				}
			}
		}
	}
	return order, nil
}

// verifyHash streams o's content through the object hasher.
func verifyHash(o plumbing.EncodedObject, st *closureStats) error {
	r, err := o.Reader()
	if err != nil {
		return errClosure{"unreadable " + o.Hash().String()}
	}
	h := plumbing.NewHasher(o.Type(), o.Size())
	n, cerr := io.Copy(h, r)
	r.Close()
	if cerr != nil || n != o.Size() || h.Sum() != o.Hash() {
		return errClosure{"hash mismatch for " + o.Hash().String()}
	}
	if st != nil {
		st.objects++
		st.bytes += n
	}
	return nil
}

// safeEntryName rejects empty names, "." and "..", slash or NUL, and .git
// in any ASCII case (a future checkout traversal hazard). Raw non-UTF-8
// names are otherwise valid.
func safeEntryName(n string) bool {
	return n != "" && n != "." && n != ".." && !strings.ContainsAny(n, "/\x00") && !strings.EqualFold(n, ".git")
}
