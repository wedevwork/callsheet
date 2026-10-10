package reale2e

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"

	git "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/filemode"
	"github.com/go-git/go-git/v5/plumbing/object"

	"github.com/wedevwork/callsheet/internal/contract"
)

// A passing synthetic bundle (design 12a-real-e2e, UT-6): produced once
// per test binary by a complete offline attempt (the live collector's own
// output) and copied for every mutation.

var passing struct {
	sync.Once
	dir string
	err error
}

// bundleCacheRoot holds the cached bundle; TestMain removes it.
var bundleCacheRoot string

// passingBundle returns a fresh copy of the cached passing bundle.
func passingBundle(t *testing.T) string {
	t.Helper()
	passing.Do(func() {
		f := newFakeWorld(t)
		if code := f.attempt(); code != 0 {
			passing.err = fmt.Errorf("the offline attempt failed (%d): %v\n%s", code, failedCodes(f.report()), f.stderr.String())
			return
		}
		passing.dir = filepath.Join(bundleCacheRoot, "passing")
		passing.err = copyTree(f.bundle(), passing.dir)
	})
	if passing.err != nil {
		t.Fatal(passing.err)
	}
	dst := filepath.Join(t.TempDir(), "bundle")
	if err := copyTree(passing.dir, dst); err != nil {
		t.Fatal(err)
	}
	return dst
}

// copyTree copies a directory of regular files.
func copyTree(src, dst string) error {
	return filepath.WalkDir(src, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(src, p)
		target := filepath.Join(dst, rel)
		if d.IsDir() {
			return os.MkdirAll(target, 0o700)
		}
		in, err := os.Open(p)
		if err != nil {
			return err
		}
		defer in.Close()
		out, err := os.OpenFile(target, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
		if err != nil {
			return err
		}
		_, err = io.Copy(out, in)
		return errorsJoin(err, out.Close())
	})
}

func errorsJoin(a, b error) error {
	if a != nil {
		return a
	}
	return b
}

// readJSON decodes a bundle file into a generic value (numbers kept).
func readJSON(t *testing.T, path string) map[string]any {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.UseNumber()
	var m map[string]any
	if err := dec.Decode(&m); err != nil {
		t.Fatal(err)
	}
	return m
}

// writeJSONFile replaces a bundle file with v.
func writeJSONFile(t *testing.T, path string, v any) {
	t.Helper()
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, append(b, '\n'), 0o600); err != nil {
		t.Fatal(err)
	}
}

// editJSON edits a JSON document of the bundle in place.
func editJSON(t *testing.T, bundle, rel string, f func(m map[string]any)) {
	t.Helper()
	p := filepath.Join(bundle, filepath.FromSlash(rel))
	m := readJSON(t, p)
	f(m)
	writeJSONFile(t, p, m)
}

// editDoc edits a typed document of the bundle in place.
func editDoc[T any](t *testing.T, bundle, rel string, f func(v *T)) {
	t.Helper()
	p := filepath.Join(bundle, filepath.FromSlash(rel))
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	var v T
	if err := json.Unmarshal(b, &v); err != nil {
		t.Fatal(err)
	}
	f(&v)
	writeJSONFile(t, p, v)
}

// editTask edits a stored task view, keeping it valid for the strict
// parser.
func editTask(t *testing.T, bundle, dir string, f func(v *contract.TaskView)) {
	t.Helper()
	p := filepath.Join(bundle, filepath.FromSlash(dir), "task.json")
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	v, err := contract.ParseTaskShowResponse(bytes.TrimSpace(b))
	if err != nil {
		t.Fatal(err)
	}
	f(&v)
	writeJSONFile(t, p, contract.TaskShowResponse{Version: contract.ProtocolVersion, Task: v})
}

// readTask reads a stored task view.
func readTask(t *testing.T, bundle, dir string) contract.TaskView {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(bundle, filepath.FromSlash(dir), "task.json"))
	if err != nil {
		t.Fatal(err)
	}
	v, err := contract.ParseTaskShowResponse(bytes.TrimSpace(b))
	if err != nil {
		t.Fatal(err)
	}
	return v
}

// editEvents rewrites events.jsonl through f, renumbering sequences when
// renumber is set.
func editEvents(t *testing.T, bundle string, renumber bool, f func(evs []Event) []Event) {
	t.Helper()
	p := filepath.Join(bundle, "events.jsonl")
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	var evs []Event
	for _, l := range strings.Split(strings.TrimSpace(string(b)), "\n") {
		var e Event
		if err := json.Unmarshal([]byte(l), &e); err != nil {
			t.Fatal(err)
		}
		evs = append(evs, e)
	}
	evs = f(evs)
	var out strings.Builder
	for i, e := range evs {
		if renumber {
			e.Seq = i + 1
		}
		line, _ := json.Marshal(e)
		out.Write(line)
		out.WriteByte('\n')
	}
	if err := os.WriteFile(p, []byte(out.String()), 0o600); err != nil {
		t.Fatal(err)
	}
}

// writeFile replaces a bundle file.
func writeBundleFile(t *testing.T, bundle, rel string, data []byte) {
	t.Helper()
	p := filepath.Join(bundle, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, data, 0o600); err != nil {
		t.Fatal(err)
	}
}

// removeBundleFile deletes a bundle path.
func removeBundleFile(t *testing.T, bundle, rel string) {
	t.Helper()
	if err := os.RemoveAll(filepath.Join(bundle, filepath.FromSlash(rel))); err != nil {
		t.Fatal(err)
	}
}

// evidenceRepo opens the bundle's evidence repository for rewriting.
func evidenceRepo(t *testing.T, bundle string) *git.Repository {
	t.Helper()
	r, err := git.PlainOpen(filepath.Join(bundle, "workspace", "repo.git"))
	if err != nil {
		t.Fatal(err)
	}
	return r
}

// refCommit returns the commit of an evidence ref.
func refCommit(t *testing.T, r *git.Repository, ref string) *object.Commit {
	t.Helper()
	h, err := r.Reference(plumbing.ReferenceName(ref), true)
	if err != nil {
		t.Fatal(err)
	}
	c, err := r.CommitObject(h.Hash())
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// filesOf reads a commit's files.
func filesOf(t *testing.T, c *object.Commit) map[string][]byte {
	t.Helper()
	m, err := commitFiles(c)
	if err != nil {
		t.Fatal(err)
	}
	return m
}

// addCommit writes a commit of files with parents into the evidence
// repository as loose objects and points ref at it.
func addCommit(t *testing.T, r *git.Repository, ref string, files map[string][]byte, parents ...plumbing.Hash) plumbing.Hash {
	t.Helper()
	st := r.Storer
	tree := &object.Tree{}
	for _, name := range sortedKeys(files) {
		o := st.NewEncodedObject()
		o.SetType(plumbing.BlobObject)
		w, _ := o.Writer()
		w.Write(files[name])
		w.Close()
		h, err := st.SetEncodedObject(o)
		if err != nil {
			t.Fatal(err)
		}
		tree.Entries = append(tree.Entries, object.TreeEntry{Name: name, Mode: filemode.Regular, Hash: h})
	}
	to := st.NewEncodedObject()
	tree.Encode(to)
	th, _ := st.SetEncodedObject(to)
	sig := object.Signature{Name: "x", Email: "x@invalid"}
	c := &object.Commit{Author: sig, Committer: sig, Message: "mutated\n", TreeHash: th, ParentHashes: parents}
	co := st.NewEncodedObject()
	c.Encode(co)
	h, err := st.SetEncodedObject(co)
	if err != nil {
		t.Fatal(err)
	}
	if ref != "" {
		if err := st.SetReference(plumbing.NewHashReference(plumbing.ReferenceName(ref), h)); err != nil {
			t.Fatal(err)
		}
	}
	return h
}

// checkBundle runs the offline check of a bundle.
func checkBundle(t *testing.T, bundle string) (int, Report, string) {
	t.Helper()
	var out, errOut bytes.Buffer
	code := CheckEvidence(bundle, Redactor{}, &out, &errOut)
	var r Report
	if err := json.Unmarshal(out.Bytes(), &r); err != nil {
		t.Fatalf("check printed no report: %v\n%s", err, out.String())
	}
	return code, r, errOut.String()
}

// criterion returns r's criterion id.
func criterion(r Report, id string) Criterion {
	for _, c := range r.Criteria {
		if c.ID == id {
			return c
		}
	}
	return Criterion{}
}

// requireCode requires a FAIL report whose criterion crit carries code.
func requireCode(t *testing.T, name string, code int, r Report, crit, want string) {
	t.Helper()
	c := criterion(r, crit)
	if code != 1 || r.Result != ResultFail || c.Result != "fail" || !slices.Contains(c.Codes, want) {
		t.Errorf("%s: exit %d result %s, criterion %s = %+v, want code %s (all %v)", name, code, r.Result, crit, c, want, failedCodes(r))
	}
}

// TestMain also serves as the re-executed helper child of the real
// process tests (REALE2E_TEST_CHILD selects its conduct).
func TestMain(m *testing.M) {
	if mode := os.Getenv("REALE2E_TEST_CHILD"); mode != "" {
		os.Exit(childMain(mode))
	}
	d, err := os.MkdirTemp("", "reale2e-bundles-")
	if err != nil {
		panic(err)
	}
	bundleCacheRoot = d
	code := m.Run()
	os.RemoveAll(d)
	os.Exit(code)
}
