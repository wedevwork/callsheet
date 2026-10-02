package workspace

import (
	"bytes"
	"context"
	"encoding/pem"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/filemode"
	"github.com/go-git/go-git/v5/plumbing/format/packfile"
	"github.com/go-git/go-git/v5/plumbing/protocol/packp"
	"github.com/go-git/go-git/v5/plumbing/protocol/packp/capability"
	"github.com/go-git/go-git/v5/plumbing/revlist"
	"github.com/go-git/go-git/v5/plumbing/storer"

	"github.com/wedevwork/callsheet/internal/contract"
	"github.com/wedevwork/callsheet/internal/testkit"
)

// testWait bounds every wait on an event (a failure bound, never a
// scheduling assumption).
const testWait = 20 * time.Second

// fastDeps are the production deps with no-op syncs (the durability
// ordering is asserted through the failure seam, not by timing real
// fsyncs); realSync keeps the production sync calls.
func fastDeps() *deps {
	d := defaultDeps()
	d.syncFile = func(*os.File) error { return nil }
	d.syncDir = func(*os.File) error { return nil }
	return d
}

// tempRoot is a canonical private state root.
func tempRoot(t testing.TB) string {
	t.Helper()
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(root, 0o700); err != nil {
		t.Fatal(err)
	}
	return root
}

// openAt opens a manager with d over root.
func openAt(t testing.TB, d *deps, root string) *Manager {
	t.Helper()
	m, err := d.open(context.Background(), root, true)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(m.Close)
	return m
}

// newManager opens a fresh manager with fast syncs.
func newManager(t testing.TB) (*Manager, string) {
	t.Helper()
	root := tempRoot(t)
	return openAt(t, fastDeps(), root), root
}

// gitServer is the production git handler (and control operations) on a
// real TLS test server.
type gitServer struct {
	srv  *httptest.Server
	http *http.Client
}

func serveGit(t testing.TB, m *Manager) *gitServer {
	t.Helper()
	srv := httptest.NewUnstartedServer(http.HandlerFunc(m.ServeGit))
	srv.StartTLS()
	t.Cleanup(srv.Close)
	caPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: srv.Certificate().Raw})
	hc, err := testkit.GitHTTPClient(caPEM)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(hc.CloseIdleConnections)
	return &gitServer{srv: srv, http: hc}
}

func (g *gitServer) remote(name, instance string) testkit.GitRemote {
	return testkit.GitRemote{URL: g.srv.URL + "/ws/" + name + ".git", Instance: instance, HTTP: g.http}
}

// mustCreate creates name.
func mustCreate(t testing.TB, m *Manager, name string) contract.WorkspaceView {
	t.Helper()
	v, err := m.Create(context.Background(), name)
	if err != nil {
		t.Fatalf("create %s: %v", name, err)
	}
	return v
}

// seedFiles is text, an executable, binary bytes, a symlink and a UTF-8
// name.
func seedFiles() map[string]testkit.FileSpec {
	bin := make([]byte, 512)
	for i := range bin {
		bin[i] = byte(i * 7)
	}
	return map[string]testkit.FileSpec{
		"README.md":  {Mode: filemode.Regular, Content: []byte("# workspace\nline two\n")},
		"bin/run.sh": {Mode: filemode.Executable, Content: []byte("#!/bin/sh\necho hi\n")},
		"data.bin":   {Mode: filemode.Regular, Content: bin},
		"link":       {Mode: filemode.Symlink, Content: []byte("README.md")},
		"日本語.txt":    {Mode: filemode.Regular, Content: []byte("こんにちは\n")},
	}
}

// commit writes files as a commit with parents into s.
func commit(t testing.TB, s storer.EncodedObjectStorer, files map[string]testkit.FileSpec, msg string, parents ...plumbing.Hash) plumbing.Hash {
	t.Helper()
	h, err := testkit.CommitFiles(s, files, parents, msg)
	if err != nil {
		t.Fatal(err)
	}
	return h
}

// push sends name old->new and requires success.
func push(t testing.TB, r testkit.GitRemote, s storer.EncodedObjectStorer, name string, old, new plumbing.Hash, opts ...testkit.PushOption) {
	t.Helper()
	res := r.Push(context.Background(), s, name, old, new, opts...)
	if !res.OK() {
		t.Fatalf("push %s: %+v", name, pushString(res))
	}
}

func pushString(p testkit.PushResult) string {
	var b strings.Builder
	if p.Err != nil {
		b.WriteString("err=" + p.Err.Error() + " ")
	}
	if p.Report != nil {
		b.WriteString("unpack=" + p.Report.UnpackStatus)
		for _, c := range p.Report.CommandStatuses {
			b.WriteString(" " + string(c.ReferenceName) + "=" + c.Status)
		}
	}
	return b.String()
}

// refsOf reads the current generation's refs of name.
func refsOf(t testing.TB, m *Manager, name string) map[string]string {
	t.Helper()
	h, err := m.lookup(name)
	if err != nil {
		t.Fatal(err)
	}
	if err := h.lock.RLock(context.Background()); err != nil {
		t.Fatal(err)
	}
	defer h.lock.RUnlock()
	refs, err := readRefs(h.repo())
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]string{}
	for n, c := range refs {
		out[n] = c.String()
	}
	return out
}

// genOf returns name's current generation.
func genOf(t testing.TB, m *Manager, name string) string {
	t.Helper()
	h, err := m.lookup(name)
	if err != nil {
		t.Fatal(err)
	}
	h.lock.RLock(context.Background())
	defer h.lock.RUnlock()
	return h.gen
}

// setRef runs one ref set and returns its result.
func setRef(m *Manager, name, instance, branch, expected string, target *string, del bool) (contract.WorkspaceRefSetResponse, error) {
	req := contract.WorkspaceRefSetRequest{Instance: instance, Branch: branch, Expected: expected, Target: target}
	if del {
		req.Delete = &del
	}
	in, err := contract.ValidateRefSet(req)
	if err != nil {
		return contract.WorkspaceRefSetResponse{}, err
	}
	return m.SetRef(context.Background(), name, in)
}

func strp(s string) *string { return &s }

// listFiles lists every path below root (relative, sorted).
func listFiles(t testing.TB, root string) []string {
	t.Helper()
	var out []string
	filepath.Walk(root, func(p string, fi os.FileInfo, err error) error {
		if err == nil && p != root {
			rel, _ := filepath.Rel(root, p)
			out = append(out, rel)
		}
		return nil
	})
	sort.Strings(out)
	return out
}

// recorder captures observed filesystem operations.
type recorder struct {
	mu  sync.Mutex
	ops []string
}

func (r *recorder) observe(op, p string) {
	r.mu.Lock()
	r.ops = append(r.ops, op+" "+p)
	r.mu.Unlock()
}

func (r *recorder) all() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.ops...)
}

func codeOf(err error) contract.Code { return contract.CodeOf(err) }

// packOf encodes a non-thin pack of exactly hashes from s.
func packOf(t testing.TB, s storer.EncodedObjectStorer, hashes ...plumbing.Hash) []byte {
	t.Helper()
	var buf bytes.Buffer
	if _, err := packfile.NewEncoder(&buf, s, false).Encode(hashes, 10); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// objectsFor lists the objects of want not reachable from haves.
func objectsFor(t testing.TB, s storer.EncodedObjectStorer, want plumbing.Hash, haves ...plumbing.Hash) []plumbing.Hash {
	t.Helper()
	objs, err := revlist.Objects(s, []plumbing.Hash{want}, haves)
	if err != nil {
		t.Fatal(err)
	}
	return objs
}

// encodeReceive encodes a raw receive-pack request.
func encodeReceive(t testing.TB, cmds []*packp.Command, caps []capability.Capability, pack []byte) []byte {
	t.Helper()
	req := packp.NewReferenceUpdateRequest()
	for _, c := range caps {
		req.Capabilities.Set(c)
	}
	req.Commands = cmds
	if pack != nil {
		req.Packfile = io.NopCloser(bytes.NewReader(pack))
	}
	var buf bytes.Buffer
	if err := req.Encode(&buf); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// rpc is a raw smart-HTTP answer of the production handler.
type rpc struct {
	status int
	report *packp.ReportStatus
	body   string
}

func (r rpc) ok() bool {
	return r.status == http.StatusOK && r.report != nil && r.report.Error() == nil
}

func (r rpc) String() string {
	if r.report != nil {
		s := fmt.Sprintf("HTTP %d unpack %q", r.status, r.report.UnpackStatus)
		for _, c := range r.report.CommandStatuses {
			s += fmt.Sprintf(" %s %q", c.ReferenceName, c.Status)
		}
		return s
	}
	return fmt.Sprintf("HTTP %d %q", r.status, r.body)
}

// serveDirect calls the production handler in process (no TLS).
func serveDirect(m *Manager, method, target string, header http.Header, body []byte) rpc {
	req := httptest.NewRequest(method, target, bytes.NewReader(body))
	for k, v := range header {
		req.Header[k] = v
	}
	rec := httptest.NewRecorder()
	m.ServeGit(rec, req)
	res := rpc{status: rec.Code, body: rec.Body.String()}
	if rec.Code == http.StatusOK && strings.HasSuffix(target, routeReceive) {
		rs := packp.NewReportStatus()
		if rs.Decode(bytes.NewReader(rec.Body.Bytes())) == nil {
			res.report = rs
		}
	}
	return res
}

// receiveDirect posts one raw receive-pack request with the instance
// header (report-status plus extra capabilities).
func receiveDirect(t testing.TB, m *Manager, name, instance string, cmds []*packp.Command, pack []byte, extra ...capability.Capability) rpc {
	t.Helper()
	caps := append([]capability.Capability{capability.ReportStatus}, extra...)
	h := http.Header{}
	if instance != "" {
		h.Set(contract.WorkspaceInstanceHeader, instance)
	}
	return serveDirect(m, http.MethodPost, "/ws/"+name+".git/git-receive-pack", h, encodeReceive(t, cmds, caps, pack))
}

func cmd(name string, old, new plumbing.Hash) []*packp.Command {
	return []*packp.Command{{Name: plumbing.ReferenceName(name), Old: old, New: new}}
}
