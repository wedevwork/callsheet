package workspacetransfer

import (
	"bytes"
	"context"
	"crypto/sha1"
	"crypto/tls"
	"encoding/json"
	"errors"
	"flag"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/go-git/go-billy/v5/osfs"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/cache"
	"github.com/go-git/go-git/v5/plumbing/filemode"
	"github.com/go-git/go-git/v5/plumbing/format/index"
	"github.com/go-git/go-git/v5/plumbing/format/objfile"
	"github.com/go-git/go-git/v5/plumbing/storer"
	"github.com/go-git/go-git/v5/storage/filesystem"
	"golang.org/x/sys/unix"

	"github.com/wedevwork/callsheet/internal/client"
	"github.com/wedevwork/callsheet/internal/contract"
	"github.com/wedevwork/callsheet/internal/testkit"
	"github.com/wedevwork/callsheet/internal/workspace"
)

// testWait bounds every wait on an event (a failure bound, never a
// scheduling assumption).
const testWait = 20 * time.Second

// hostGOOS is the native OS value for tests that exercise real
// filesystem behavior (tests may read the host; production code takes
// goos as a parameter).
func hostGOOS() string { return runtime.GOOS }

// ---- Test plane: the production workspace manager on verified TLS ----

// testPlane is the production workspace manager behind a verified TLS
// server with the control endpoints a transfer uses (show, status, diff)
// and the production smart-HTTP handler.
type testPlane struct {
	// gitReq and gitRes count smart-HTTP request and response body bytes
	// (benchmark payloads).
	gitReq, gitRes atomic.Int64
	mp             atomic.Pointer[workspace.Manager]
	srv            *httptest.Server
	url            string
	ca             *testkit.FixtureCA
	root           string
}

func newTestPlane(t testing.TB) *testPlane {
	t.Helper()
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	p, stop, err := newTestPlaneIn(root)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(stop)
	return p
}

// newTestPlaneIn starts a test plane with state root root; stop closes it.
func newTestPlaneIn(root string) (*testPlane, func(), error) {
	if err := os.Chmod(root, 0o700); err != nil {
		return nil, nil, err
	}
	m, err := workspace.Open(context.Background(), root)
	if err != nil {
		return nil, nil, err
	}
	ca, err := testkit.NewFixtureCA()
	if err != nil {
		return nil, nil, err
	}
	leaf, err := ca.ServerCertificate()
	if err != nil {
		return nil, nil, err
	}
	p := &testPlane{ca: ca, root: root}
	p.mp.Store(m)
	srv := httptest.NewUnstartedServer(http.HandlerFunc(p.serve))
	srv.TLS = &tls.Config{Certificates: []tls.Certificate{leaf}}
	srv.StartTLS()
	p.srv = srv
	_, port, _ := net.SplitHostPort(srv.Listener.Addr().String())
	p.url = "https://127.0.0.1:" + port
	return p, func() {
		srv.Close()
		p.m().Close()
	}, nil
}

func writeJSON(w http.ResponseWriter, v any, err error) {
	w.Header().Set(contract.ProtocolHeader, strconv.Itoa(contract.ProtocolVersion))
	w.Header().Set("Content-Type", "application/json")
	if err != nil {
		var ce *contract.Error
		if !errors.As(err, &ce) {
			ce = contract.New(contract.CodeInternal, "internal")
		}
		w.WriteHeader(contract.HTTPStatus(ce.Code))
		b, _ := json.Marshal(ce)
		w.Write(b)
		return
	}
	b, _ := contract.Encode(v)
	w.Write(b)
}

// m is the current manager (replaced by a restart).
func (p *testPlane) m() *workspace.Manager { return p.mp.Load() }

// plantTask writes a task ref with its publication metadata into a
// workspace's current generation and restarts the manager (no public API
// creates task refs before iteration 10).
func (p *testPlane) plantTask(t testing.TB, name, id string, c plumbing.Hash) {
	t.Helper()
	if err := p.plant(name, id, c); err != nil {
		t.Fatal(err)
	}
}

func (p *testPlane) plant(name, id string, c plumbing.Hash) error {
	p.m().Close()
	ws := filepath.Join(p.root, "workspaces", name)
	cur, err := os.ReadFile(filepath.Join(ws, "CURRENT"))
	if err != nil {
		return err
	}
	gen := filepath.Join(ws, "generations", strings.TrimSpace(string(cur)))
	ref := contract.TaskRefPrefix + id
	refPath := filepath.Join(gen, "repo.git", filepath.FromSlash(ref))
	if err := os.MkdirAll(filepath.Dir(refPath), 0o700); err != nil {
		return err
	}
	if err := os.WriteFile(refPath, []byte(c.String()+"\n"), 0o600); err != nil {
		return err
	}
	var doc struct {
		Schema   int                          `json:"schema"`
		TaskRefs map[string]map[string]string `json:"task_refs"`
	}
	b, err := os.ReadFile(filepath.Join(gen, "refs.json"))
	if err != nil {
		return err
	}
	if err := json.Unmarshal(b, &doc); err != nil {
		return err
	}
	doc.TaskRefs[ref] = map[string]string{"commit": c.String(), "published_at": "2026-09-30T12:00:00Z"}
	b, _ = json.Marshal(doc)
	if err := os.WriteFile(filepath.Join(gen, "refs.json"), b, 0o600); err != nil {
		return err
	}
	m, err := workspace.Open(context.Background(), p.root)
	if err != nil {
		return err
	}
	p.mp.Store(m)
	return nil
}

func (p *testPlane) serve(w http.ResponseWriter, r *http.Request) {
	if strings.HasPrefix(r.URL.Path, contract.WorkspaceGitPrefix) {
		r.Body = countBody{ReadCloser: r.Body, n: &p.gitReq}
		p.m().ServeGit(countWriter{ResponseWriter: w, n: &p.gitRes}, r)
		return
	}
	rest, ok := strings.CutPrefix(r.URL.Path, contract.PathWorkspaces+"/")
	if !ok || r.Method != http.MethodGet {
		writeJSON(w, nil, contract.New(contract.CodeNotFound, "no such endpoint"))
		return
	}
	name, op, _ := strings.Cut(rest, "/")
	q := r.URL.Query()
	switch op {
	case "":
		v, err := p.m().Show(r.Context(), name)
		writeJSON(w, v, err)
	case "status":
		limit, _ := strconv.Atoi(q.Get("limit"))
		v, err := p.m().Status(r.Context(), name, workspace.StatusQuery{After: q.Get("after"), Instance: q.Get("instance"), Generation: q.Get("generation"), Limit: limit})
		writeJSON(w, v, err)
	case "diff":
		limit, _ := strconv.Atoi(q.Get("limit"))
		base, err := contract.ParseSelector(q.Get("base"), "base", true)
		if err != nil {
			writeJSON(w, nil, err)
			return
		}
		target, err := contract.ParseSelector(q.Get("target"), "target", false)
		if err != nil {
			writeJSON(w, nil, err)
			return
		}
		v, err := p.m().Diff(r.Context(), name, workspace.DiffQuery{Base: base, Target: target, Limit: limit})
		writeJSON(w, v, err)
	default:
		writeJSON(w, nil, contract.New(contract.CodeNotFound, "no such endpoint"))
	}
}

// client returns a fresh verified client (closed with t).
func (p *testPlane) client(t testing.TB) *client.Client {
	t.Helper()
	c, err := client.New(p.url, client.Trust{CAPEM: p.ca.CertPEM})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(c.Close)
	return c
}

func (p *testPlane) plane(t testing.TB) Plane { return ClientPlane(p.client(t)) }

func (p *testPlane) create(t testing.TB, name string) contract.WorkspaceView {
	t.Helper()
	v, err := p.m().Create(context.Background(), name)
	if err != nil {
		t.Fatal(err)
	}
	return v
}

// refs returns the workspace's stored refs.
func (p *testPlane) refs(t testing.TB, name string) map[string]string {
	t.Helper()
	r, err := p.m().Status(context.Background(), name, workspace.StatusQuery{Limit: 100})
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]string{}
	for _, ref := range r.Refs {
		out[ref.Name] = ref.Commit
	}
	return out
}

// remote is a testkit git remote of the workspace (seeding helpers).
func (p *testPlane) remote(t testing.TB, name, instance string) testkit.GitRemote {
	t.Helper()
	hc, err := testkit.GitHTTPClient(p.ca.CertPEM)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(hc.CloseIdleConnections)
	return testkit.GitRemote{URL: p.url + "/ws/" + name + ".git", Instance: instance, HTTP: hc}
}

// ---- Fixture repositories built with go-git (no git executable) ----

// fspec is a fixture file: content with a mode (regular, executable or
// symlink with the content as target).
type fspec struct {
	mode    filemode.FileMode
	content string
}

func reg(s string) fspec  { return fspec{filemode.Regular, s} }
func exe(s string) fspec  { return fspec{filemode.Executable, s} }
func link(s string) fspec { return fspec{filemode.Symlink, s} }

// fixtureRepo is a working repository on disk.
type fixtureRepo struct {
	t     testing.TB
	root  string
	store *fixtureStore
	head  plumbing.Hash
	files map[string]fspec
	rd    *filesystem.Storage
}

// fixtureStore writes loose objects straight into a fixture repository
// (cheap enough for the repeated stress runs; it is fixture code, not
// the object install under test). Reads go through go-git's storage.
type fixtureStore struct {
	storer.EncodedObjectStorer
	objects string
	r       *fixtureRepo
}

func (s *fixtureStore) NewEncodedObject() plumbing.EncodedObject { return &plumbing.MemoryObject{} }

func (s *fixtureStore) SetEncodedObject(o plumbing.EncodedObject) (plumbing.Hash, error) {
	var buf bytes.Buffer
	w := objfile.NewWriter(&buf)
	if err := w.WriteHeader(o.Type(), o.Size()); err != nil {
		return plumbing.ZeroHash, err
	}
	rd, err := o.Reader()
	if err != nil {
		return plumbing.ZeroHash, err
	}
	defer rd.Close()
	if _, err := io.Copy(w, rd); err != nil {
		return plumbing.ZeroHash, err
	}
	if err := w.Close(); err != nil {
		return plumbing.ZeroHash, err
	}
	h := w.Hash().String()
	dir := filepath.Join(s.objects, h[:2])
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return plumbing.ZeroHash, err
	}
	if err := os.WriteFile(filepath.Join(dir, h[2:]), buf.Bytes(), 0o444); err != nil && !os.IsExist(err) && !os.IsPermission(err) {
		return plumbing.ZeroHash, err
	}
	return w.Hash(), nil
}

func (s *fixtureStore) reader() storer.EncodedObjectStorer { return s.r.reader() }

func (s *fixtureStore) HasEncodedObject(h plumbing.Hash) error { return s.reader().HasEncodedObject(h) }

func (s *fixtureStore) EncodedObject(t plumbing.ObjectType, h plumbing.Hash) (plumbing.EncodedObject, error) {
	return s.reader().EncodedObject(t, h)
}

// reader is go-git's storage of the fixture (opened on first use).
func (r *fixtureRepo) reader() *filesystem.Storage {
	if r.rd == nil {
		r.rd = filesystem.NewStorageWithOptions(osfs.New(filepath.Join(r.root, ".git")), cache.NewObjectLRUDefault(), filesystem.Options{})
		r.t.Cleanup(func() { r.rd.Close() })
	}
	return r.rd
}

// tempDir is a canonical temporary directory.
func tempDir(t testing.TB) string {
	t.Helper()
	d, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return d
}

const baseConfig = "[core]\n\trepositoryformatversion = 0\n\tfilemode = true\n\tbare = false\n"

// newRepo creates an ordinary repository at a new directory with files
// committed on main, the working tree and index matching.
func newRepo(t testing.TB, files map[string]fspec) *fixtureRepo {
	t.Helper()
	return newRepoAt(t, filepath.Join(tempDir(t), "repo"), files)
}

func newRepoAt(t testing.TB, root string, files map[string]fspec) *fixtureRepo {
	t.Helper()
	for _, d := range []string{".git/objects/pack", ".git/objects/info", ".git/refs/heads", ".git/refs/tags", ".git/info"} {
		if err := os.MkdirAll(filepath.Join(root, d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	mustWrite(t, filepath.Join(root, ".git/HEAD"), "ref: refs/heads/main\n")
	mustWrite(t, filepath.Join(root, ".git/config"), baseConfig)
	r := &fixtureRepo{t: t, root: root, files: map[string]fspec{}}
	r.store = &fixtureStore{objects: filepath.Join(root, ".git", "objects"), r: r}
	if files != nil {
		r.commit(files, "initial")
	}
	return r
}

func mustWrite(t testing.TB, p, s string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(s), 0o644); err != nil {
		t.Fatal(err)
	}
}

// writeFile materializes one fixture file in the working tree.
func (r *fixtureRepo) writeFile(p string, f fspec) {
	r.t.Helper()
	full := filepath.Join(r.root, filepath.FromSlash(p))
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		r.t.Fatal(err)
	}
	os.Remove(full)
	switch f.mode {
	case filemode.Symlink:
		if err := os.Symlink(f.content, full); err != nil {
			r.t.Fatal(err)
		}
	case filemode.Executable:
		if err := os.WriteFile(full, []byte(f.content), 0o755); err != nil {
			r.t.Fatal(err)
		}
		os.Chmod(full, 0o755)
	default:
		if err := os.WriteFile(full, []byte(f.content), 0o644); err != nil {
			r.t.Fatal(err)
		}
		os.Chmod(full, 0o644)
	}
}

// treeOf stores files as a tree.
func (r *fixtureRepo) treeOf(files map[string]fspec) plumbing.Hash {
	r.t.Helper()
	specs := map[string]testkit.FileSpec{}
	for p, f := range files {
		specs[p] = testkit.FileSpec{Mode: f.mode, Content: []byte(f.content)}
	}
	h, err := testkit.WriteTree(r.store, specs)
	if err != nil {
		r.t.Fatal(err)
	}
	return h
}

// commit replaces the committed state with files: objects, a commit on
// main (parent: the previous head), the working tree and the index.
func (r *fixtureRepo) commit(files map[string]fspec, msg string) plumbing.Hash {
	r.t.Helper()
	tree := r.treeOf(files)
	var parents []plumbing.Hash
	if !r.head.IsZero() {
		parents = []plumbing.Hash{r.head}
	}
	h, err := testkit.WriteCommit(r.store, tree, parents, msg, testkit.FixedWhen)
	if err != nil {
		r.t.Fatal(err)
	}
	for p := range r.files {
		if _, ok := files[p]; !ok {
			os.Remove(filepath.Join(r.root, filepath.FromSlash(p)))
		}
	}
	for p, f := range files {
		if old, ok := r.files[p]; ok && old == f {
			continue
		}
		r.writeFile(p, f)
	}
	r.files = files
	r.head = h
	mustWrite(r.t, filepath.Join(r.root, ".git/refs/heads/main"), h.String()+"\n")
	r.writeIndex(files)
	return h
}

// writeIndex writes a v2 index of files (stage 0).
func (r *fixtureRepo) writeIndex(files map[string]fspec) {
	r.t.Helper()
	r.writeIndexEntries(r.indexEntries(files))
}

func (r *fixtureRepo) indexEntries(files map[string]fspec) []*index.Entry {
	var out []*index.Entry
	for p, f := range files {
		out = append(out, &index.Entry{Name: p, Mode: f.mode, Hash: plumbing.ComputeHash(plumbing.BlobObject, []byte(f.content)), Size: uint32(len(f.content))})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

func (r *fixtureRepo) writeIndexEntries(entries []*index.Entry) {
	r.t.Helper()
	var buf bytes.Buffer
	if err := index.NewEncoder(&buf).Encode(&index.Index{Version: 2, Entries: entries}); err != nil {
		r.t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(r.root, ".git/index"), buf.Bytes(), 0o644); err != nil {
		r.t.Fatal(err)
	}
}

// config appends configuration text to .git/config.
func (r *fixtureRepo) config(s string) {
	r.t.Helper()
	f, err := os.OpenFile(filepath.Join(r.root, ".git/config"), os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		r.t.Fatal(err)
	}
	defer f.Close()
	f.WriteString(s)
}

// emptyEnv is an environment with nothing but an empty HOME and no
// system files: tests never read the developer's configuration.
func emptyEnv(t testing.TB) Env {
	t.Helper()
	home := tempDir(t)
	return envOf(map[string]string{"HOME": home, "GIT_CONFIG_NOSYSTEM": "1"})
}

func envOf(m map[string]string) Env {
	return Env{Lookup: func(k string) (string, bool) { v, ok := m[k]; return v, ok }}
}

// fastDeps are the production deps with no-op syncs (durability ordering
// is asserted through the failure seam, not by timing real fsyncs).
func fastDeps() *deps {
	d := defaultDeps()
	d.syncFD = func(int) error { return nil }
	// Batches fall back to the per-path walk over the no-op sync.
	d.syncBatchFD = nil
	return d
}

func codeOf(err error) contract.Code { return contract.CodeOf(err) }

func unixMkfifo(t testing.TB, p string) {
	t.Helper()
	if err := unix.Mkfifo(p, 0o644); err != nil {
		t.Fatal(err)
	}
}

func sha1Sum(b []byte) []byte {
	s := sha1.Sum(b)
	return s[:]
}

// testkitNow is a prune cutoff after every planted publication time.
func testkitNow() time.Time { return time.Now().Add(time.Hour) }

func unixUmask(m int) int { return unix.Umask(m) }

var (
	errUnixEEXIST    error = unix.EEXIST
	errUnixENOTEMPTY error = unix.ENOTEMPTY
	errUnixEINVAL    error = unix.EINVAL
	errUnixENOTSUP   error = unix.ENOTSUP
	errUnixEXDEV     error = unix.EXDEV
)

type countBody struct {
	io.ReadCloser
	n *atomic.Int64
}

func (c countBody) Read(p []byte) (int, error) {
	n, err := c.ReadCloser.Read(p)
	c.n.Add(int64(n))
	return n, err
}

type countWriter struct {
	http.ResponseWriter
	n *atomic.Int64
}

func (c countWriter) Write(p []byte) (int, error) {
	n, err := c.ResponseWriter.Write(p)
	c.n.Add(int64(n))
	return n, err
}

// Unwrap exposes the connection for the handler's deadline controller.
func (c countWriter) Unwrap() http.ResponseWriter { return c.ResponseWriter }

// sampleRuns counts each test's invocations in this process.
var sampleRuns sync.Map

// sampler returns which cases of a deterministic case matrix the calling
// test runs in this invocation: all of them in an ordinary run; under a
// repeated run (-count=N, the packages stress shard, which repeats N
// times at each CPU setting) every N-th case with an offset rotating per
// repetition, so the N repetitions at each CPU setting together run every
// case exactly once (the convention of 09a's TestDurabilityBoundaries).
// These matrices are not timing-dependent: repetition adds no evidence,
// only cost.
func sampler(t testing.TB) func(i int) bool {
	v, _ := sampleRuns.LoadOrStore(t.Name(), new(atomic.Int64))
	run := int(v.(*atomic.Int64).Add(1) - 1)
	count := 1
	if f := flag.Lookup("test.count"); f != nil {
		if n, err := strconv.Atoi(f.Value.String()); err == nil && n > 1 {
			count = n
		}
	}
	return func(i int) bool { return i%count == run%count }
}
