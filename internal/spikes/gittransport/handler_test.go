package gittransport

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	git "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/filemode"
	"github.com/go-git/go-git/v5/plumbing/format/pktline"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/go-git/go-git/v5/plumbing/protocol/packp"
	"github.com/go-git/go-git/v5/plumbing/protocol/packp/capability"
	"github.com/go-git/go-git/v5/plumbing/storer"
	"github.com/go-git/go-git/v5/storage/memory"

	"github.com/wedevwork/callsheet/internal/testkit"
)

func newTestHandler(t *testing.T) (*smartHandler, string, plumbing.Hash, plumbing.Hash) {
	t.Helper()
	bare := filepath.Join(t.TempDir(), "repo.git")
	c0, tree := seedBare(t, bare)
	h, err := NewSmartHandler(bare)
	if err != nil {
		t.Fatal(err)
	}
	return h.(*smartHandler), bare, c0, tree
}

func serve(h http.Handler, method, target string, body io.Reader) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(method, target, body))
	return rec
}

func TestDecisionRecord(t *testing.T) {
	root := testkit.MustRepoRoot(t)
	b, err := os.ReadFile(filepath.Join(root, "tests", "testdata", "git-spike-decision.json"))
	if err != nil {
		t.Fatal(err)
	}
	var d map[string]any
	dec := json.NewDecoder(bytes.NewReader(b))
	if err := dec.Decode(&d); err != nil {
		t.Fatal(err)
	}
	want := map[string]string{
		"route":              "smart-http",
		"integrity_mode":     "handler-guards",
		"reason":             "go-git v5.16.3 persists smart-HTTP pushes but does not enforce old-ref CAS or new-commit closure; Callsheet guards a quarantined session and publishes one validated ref.",
		"module":             "github.com/go-git/go-git/v5",
		"version":            "v5.16.3",
		"module_origin_hash": "ad9a3a542e845d368853848ef5a2de9ec1f79b1a",
		"source_file":        "plumbing/transport/server/server.go",
		"source_sha256":      "d36d9f87f5dc2a35a7ea674583c670ba78a3bd33fdf1211a4cad4793cebe124f",
	}
	for k, v := range want {
		if d[k] != v {
			t.Fatalf("%s = %v, want %q", k, d[k], v)
		}
	}
	probe := d["unguarded_probe"].(map[string]any)
	for k, v := range map[string]string{
		"clone_push_fetch_reopen": "passed",
		"stale_old":               "accepted_and_ref_changed",
		"missing_object":          "accepted_and_ref_published",
		"truncated_pack":          "rejected_without_ref_publication",
	} {
		if probe[k] != v {
			t.Fatalf("unguarded_probe.%s = %v", k, probe[k])
		}
	}
	if len(d) != 10 || len(probe) != 4 {
		t.Fatalf("unexpected fields: %v", d)
	}
	if strings.Contains(string(b), "auto") || strings.Contains(string(b), "pack-https") || strings.Contains(string(b), "guarded_result") {
		t.Fatal("decision record must be fixed smart-http with no fallback or fabricated guarded results")
	}
	ev := d["evidence"].([]any)
	if len(ev) != 1 || ev[0] != "tests/testdata/receive-pack-probe.txt" {
		t.Fatalf("evidence = %v", ev)
	}
	pb, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(ev[0].(string))))
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range []string{"GOFLAGS=-mod=mod GOPROXY=off go run .", "Scratch module: probe", "2026-09-25", "[stale-old] HTTP 200", "001fok refs/callsheet/tasks/t1", "ghost = 1234567890123456789012345678901234567890", "t2 = absent(reference not found)", "package main"} {
		if !bytes.Contains(pb, []byte(s)) {
			t.Fatalf("probe evidence lacks %q", s)
		}
	}
}

func TestNewSmartHandlerRejectsInvalidPaths(t *testing.T) {
	if _, err := NewSmartHandler(filepath.Join(t.TempDir(), "missing")); err == nil {
		t.Fatal("missing path accepted")
	}
	nonBare := t.TempDir()
	if _, err := git.PlainInit(nonBare, false); err != nil {
		t.Fatal(err)
	}
	if _, err := NewSmartHandler(nonBare); err == nil || !strings.Contains(err.Error(), "not a bare") {
		t.Fatalf("non-bare accepted: %v", err)
	}
	broken := filepath.Join(t.TempDir(), "b.git")
	git.PlainInit(broken, true)
	os.WriteFile(filepath.Join(broken, "config"), []byte("[core\n"), 0o644)
	if _, err := NewSmartHandler(broken); err == nil {
		t.Fatal("broken config accepted")
	}
}

func TestRoutingAndFraming(t *testing.T) {
	h, _, c0, _ := newTestHandler(t)
	cases := []struct {
		method, target string
		code           int
	}{
		{"GET", "/test/git/other.git/info/refs?service=git-upload-pack", 404},
		{"GET", "/test/git/repo.git/HEAD", 404},
		{"GET", "/test/git/repo.git/../etc/passwd", 404},
		{"POST", infoRefsPath + "?service=git-upload-pack", 405},
		{"GET", uploadPackPath, 405},
		{"PUT", receivePackPath, 405},
		{"GET", infoRefsPath + "?service=git-archive", 400},
		{"GET", infoRefsPath, 400},
	}
	for _, c := range cases {
		if rec := serve(h, c.method, c.target, nil); rec.Code != c.code {
			t.Errorf("%s %s = %d, want %d", c.method, c.target, rec.Code, c.code)
		}
	}
	for _, svc := range []string{serviceUpload, serviceReceive} {
		rec := serve(h, "GET", infoRefsPath+"?service="+svc, nil)
		if rec.Code != 200 || rec.Header().Get("Content-Type") != "application/x-"+svc+"-advertisement" {
			t.Fatalf("%s advertisement: %d %q", svc, rec.Code, rec.Header().Get("Content-Type"))
		}
		body := rec.Body.Bytes()
		prefix := pktlineString("# service="+svc+"\n") + "0000"
		if !bytes.HasPrefix(body, []byte(prefix)) {
			t.Fatalf("%s framing prefix: %q", svc, body[:min(len(body), 60)])
		}
		ar := packp.NewAdvRefs()
		if err := ar.Decode(bytes.NewReader(body)); err != nil {
			t.Fatal(err)
		}
		if ar.References["refs/heads/main"] != c0 {
			t.Fatalf("%s refs = %v", svc, ar.References)
		}
		if svc == serviceReceive {
			for _, c := range ar.Capabilities.All() {
				if !receiveAllow[c] {
					t.Fatalf("receive advertises %s", c)
				}
			}
		} else if !ar.Capabilities.Supports(capability.SymRef) {
			t.Fatal("upload-pack must keep HEAD symref metadata")
		}
	}
	if h.callCount() != len(cases)+2 || len(h.transcript()) != len(cases)+2 {
		t.Fatalf("transcript = %d calls", h.callCount())
	}
}

func pktlineString(s string) string {
	var b bytes.Buffer
	pktline.NewEncoder(&b).EncodeString(s)
	return b.String()
}

func TestUploadPackErrors(t *testing.T) {
	h, _, c0, tree := newTestHandler(t)
	if rec := serve(h, "POST", uploadPackPath, strings.NewReader("zzzz")); rec.Code != 400 {
		t.Fatalf("malformed = %d", rec.Code)
	}
	// A capability the upload session does not support is rejected.
	req := packp.NewUploadPackRequest()
	req.Wants = []plumbing.Hash{c0}
	req.Capabilities.Set(capability.MultiACK)
	var b bytes.Buffer
	req.UploadRequest.Encode(&b)
	io.WriteString(&b, "0009done\n")
	if rec := serve(h, "POST", uploadPackPath, &b); rec.Code != 400 {
		t.Fatalf("unsupported capability = %d", rec.Code)
	}
	// Malformed and unexpected have lines.
	for _, tail := range []string{pktlineString("have nothex\n"), pktlineString("bogus\n"), "zz"} {
		req := packp.NewUploadPackRequest()
		req.Wants = []plumbing.Hash{c0}
		var b bytes.Buffer
		req.UploadRequest.Encode(&b)
		b.WriteString(tail)
		if rec := serve(h, "POST", uploadPackPath, &b); rec.Code != 400 {
			t.Fatalf("tail %q = %d", tail, rec.Code)
		}
	}
	// Unknown haves are dropped rather than failing the fetch.
	req = packp.NewUploadPackRequest()
	req.Wants = []plumbing.Hash{c0}
	b.Reset()
	req.UploadRequest.Encode(&b)
	b.WriteString(pktlineString("have 1111111111111111111111111111111111111111\n"))
	b.WriteString(pktlineString("have " + tree.String() + "\n"))
	b.WriteString("0000")
	b.WriteString(pktlineString("done\n"))
	rec := serve(h, "POST", uploadPackPath, &b)
	if rec.Code != 200 || rec.Header().Get("Content-Type") != "application/x-git-upload-pack-result" {
		t.Fatalf("upload with haves = %d %q", rec.Code, rec.Body.String())
	}
	// Missing "done" (EOF) is tolerated as the end of haves.
	b.Reset()
	req.UploadRequest.Encode(&b)
	if rec := serve(h, "POST", uploadPackPath, &b); rec.Code != 200 {
		t.Fatalf("no done = %d", rec.Code)
	}
}

func receiveBody(t *testing.T, cmds []*packp.Command, caps []capability.Capability, pack []byte) io.Reader {
	return bytes.NewReader(encodeRequest(t, cmds, caps, pack))
}

func decodeReport(t *testing.T, rec *httptest.ResponseRecorder) *packp.ReportStatus {
	t.Helper()
	if rec.Code != 200 {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
	rs := packp.NewReportStatus()
	if err := rs.Decode(bytes.NewReader(rec.Body.Bytes())); err != nil {
		t.Fatalf("report: %v (%q)", err, rec.Body.String())
	}
	return rs
}

func TestReceivePackFailureStatuses(t *testing.T) {
	h, bare, c0, tree := newTestHandler(t)
	ms := memory.NewStorage()
	rs := []capability.Capability{capability.ReportStatus}
	if rec := serve(h, "POST", receivePackPath, strings.NewReader("garbage")); rec.Code != 400 {
		t.Fatalf("framing = %d", rec.Code)
	}
	// Pre-unpack rejection: not attempted.
	rec := serve(h, "POST", receivePackPath, receiveBody(t, []*packp.Command{cmd("refs/other/x", plumbing.ZeroHash, c0)}, rs, emptyPack(t, ms)))
	r := decodeReport(t, rec)
	if r.UnpackStatus != msgNotAttempted || r.CommandStatuses[0].Status != msgInvalidRef {
		t.Fatalf("report = %+v %+v", r, r.CommandStatuses[0])
	}
	// Post-unpack closure failure: unpack ok, command ng.
	missing := plumbing.ComputeHash(plumbing.BlobObject, []byte("x"))
	tr, _ := writeObject(ms, &object.Tree{Entries: []object.TreeEntry{{Name: "a", Mode: filemode.Regular, Hash: missing}}})
	c, _ := writeCommit(ms, tr, []plumbing.Hash{c0}, "m", fixedWhen)
	rec = serve(h, "POST", receivePackPath, receiveBody(t, []*packp.Command{cmd("refs/callsheet/tasks/a", plumbing.ZeroHash, c)}, rs, packOf(t, ms, c, tr)))
	r = decodeReport(t, rec)
	if r.UnpackStatus != "ok" || r.CommandStatuses[0].Status != msgClosure {
		t.Fatalf("closure report = %+v %+v", r, r.CommandStatuses[0])
	}
	// Valid create succeeds and reports ok only after publication.
	good, _ := writeCommit(ms, tree, []plumbing.Hash{c0}, "good", fixedWhen)
	rec = serve(h, "POST", receivePackPath, receiveBody(t, []*packp.Command{cmd("refs/callsheet/tasks/good", plumbing.ZeroHash, good)}, rs, packOf(t, ms, good)))
	if r = decodeReport(t, rec); r.Error() != nil {
		t.Fatalf("good push = %v", r.Error())
	}
	if refSnapshot(t, bare)["refs/callsheet/tasks/good"] != good.String() || h.lastPromoted() != 1 {
		t.Fatal("good push not published")
	}
	// Oversized request: 413 without a ref.
	old := maxRequestBytes
	maxRequestBytes = 256
	defer func() { maxRequestBytes = old }()
	big, bigPack := newCommitPack(t, c0, strings.Repeat("big", 200))
	rec = serve(h, "POST", receivePackPath, receiveBody(t, []*packp.Command{cmd("refs/callsheet/tasks/big", plumbing.ZeroHash, big)}, rs, bigPack))
	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("oversize = %d %s", rec.Code, rec.Body.String())
	}
	// Oversized command framing itself is also 413.
	maxRequestBytes = 10
	rec = serve(h, "POST", receivePackPath, receiveBody(t, []*packp.Command{cmd("refs/callsheet/tasks/big", plumbing.ZeroHash, big)}, rs, nil))
	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("oversize framing = %d", rec.Code)
	}
	if _, ok := refSnapshot(t, bare)["refs/callsheet/tasks/big"]; ok {
		t.Fatal("oversize push published")
	}
}

func TestValidateCommand(t *testing.T) {
	c := plumbing.NewHash("0123456789012345678901234567890123456789")
	mk := func(caps []capability.Capability, cmds ...*packp.Command) *packp.ReferenceUpdateRequest {
		r := packp.NewReferenceUpdateRequest()
		for _, cp := range caps {
			r.Capabilities.Set(cp)
		}
		r.Commands = cmds
		return r
	}
	rs := []capability.Capability{capability.ReportStatus}
	ok := []string{"refs/heads/main", "refs/heads/feature/x", "refs/callsheet/tasks/t1", "refs/callsheet/tasks/a/b"}
	for _, n := range ok {
		if err := validateCommand(mk(rs, cmd(n, plumbing.ZeroHash, c))); err != nil {
			t.Errorf("%s rejected: %v", n, err)
		}
	}
	if err := validateCommand(mk([]capability.Capability{capability.ReportStatus, capability.OFSDelta, capability.Agent}, cmd("refs/heads/x", c, c))); err != nil {
		t.Fatalf("allowlisted caps rejected: %v", err)
	}
	bad := []struct {
		req  *packp.ReferenceUpdateRequest
		want string
	}{
		{mk(rs), "exactly one"},
		{mk(rs, cmd("refs/heads/a", plumbing.ZeroHash, c), cmd("refs/heads/b", plumbing.ZeroHash, c)), "exactly one"},
		{mk(nil, cmd("refs/heads/a", plumbing.ZeroHash, c)), "report-status"},
		{mk([]capability.Capability{capability.ReportStatus, capability.Atomic}, cmd("refs/heads/a", plumbing.ZeroHash, c)), msgCapability},
		{mk(rs, cmd("refs/heads/a", c, plumbing.ZeroHash)), msgDelete},
		{mk(rs, cmd("refs/heads/a", plumbing.ZeroHash, plumbing.ZeroHash)), msgZeroNew},
	}
	for _, b := range bad {
		if err := validateCommand(b.req); err == nil || !strings.Contains(err.Error(), b.want) {
			t.Errorf("want %q, got %v", b.want, err)
		}
	}
	for _, n := range []string{"", "HEAD", "refs/heads/", "refs/callsheet/tasks/", "refs/tags/v1", "refs/remotes/o/x", "refs/heads/../x", "refs/heads/a..b", "refs/heads/x.lock", "refs/heads/a b", "refs/heads/a\x01", "refs/heads/a\x7f", "refs/heads//x", "/refs/heads/x", "refs/heads/.hidden", "refs/heads/a~1", "main"} {
		if err := validateRefName(plumbing.ReferenceName(n)); err == nil {
			t.Errorf("ref %q accepted", n)
		}
	}
	if safeMessage(errors.New("raw")) != msgStorageFailure || safeMessage(guard(msgClosure, errors.New("x"))) != msgClosure {
		t.Fatal("safeMessage")
	}
	if g := guard(msgClosure, errors.New("inner")); g.Error() != msgClosure+": inner" || errors.Unwrap(g) == nil {
		t.Fatal("guard error")
	}
}

func TestCheckOldAndConflicts(t *testing.T) {
	a := plumbing.NewHash("aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa")
	b := plumbing.NewHash("bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb")
	name := plumbing.ReferenceName("refs/heads/x")
	cur := plumbing.NewHashReference(name, a)
	sym := plumbing.NewSymbolicReference(name, "refs/heads/y")
	cases := []struct {
		cur  *plumbing.Reference
		old  plumbing.Hash
		ok   bool
		desc string
	}{
		{nil, plumbing.ZeroHash, true, "create absent"},
		{cur, plumbing.ZeroHash, false, "create existing"},
		{cur, a, true, "update exact"},
		{cur, b, false, "update stale"},
		{nil, a, false, "update absent"},
		{sym, a, false, "symbolic"},
	}
	for _, c := range cases {
		err := checkOld(c.cur, cmd(string(name), c.old, b))
		if (err == nil) != c.ok {
			t.Errorf("%s: %v", c.desc, err)
		}
	}
	s := memory.NewStorage()
	s.SetReference(plumbing.NewHashReference("refs/callsheet/tasks/t1", a))
	s.SetReference(plumbing.NewHashReference("refs/heads/a/b", a))
	for n, ok := range map[string]bool{"refs/callsheet/tasks/t2": true, "refs/callsheet/tasks/t1": true, "refs/callsheet/tasks/t1/sub": false, "refs/heads/a": false, "refs/heads/ab": true} {
		if err := checkRefConflict(s, plumbing.ReferenceName(n)); (err == nil) != ok {
			t.Errorf("conflict %s: %v", n, err)
		}
	}
}

func TestQuarantineIsolation(t *testing.T) {
	h, _, c0, _ := newTestHandler(t)
	q, err := newQuarantine(h.store)
	if err != nil {
		t.Fatal(err)
	}
	head, _ := q.Reference(plumbing.HEAD)
	if head == nil || head.Type() != plumbing.SymbolicReference || head.Target() != plumbing.Main {
		t.Fatalf("quarantine HEAD = %v", head)
	}
	if r, _ := q.Reference(plumbing.Main); r == nil || r.Hash() != c0 {
		t.Fatal("quarantine main")
	}
	if _, err := validateClosure(q, c0); err != nil {
		t.Fatalf("quarantine closure: %v", err)
	}
	// Writes to quarantine never reach the durable store.
	nb, _ := writeRaw(q, plumbing.BlobObject, []byte("quarantine only"))
	q.SetReference(plumbing.NewHashReference("refs/heads/q", nb))
	if h.store.HasEncodedObject(nb) == nil {
		t.Fatal("quarantine object leaked into durable store")
	}
	if r, _ := readRef(h.store, "refs/heads/q"); r != nil {
		t.Fatal("quarantine ref leaked")
	}
	// Object bytes are independent copies.
	qo, _ := q.EncodedObject(plumbing.AnyObject, c0)
	do, _ := h.store.EncodedObject(plumbing.AnyObject, c0)
	qb, _ := readContent(qo)
	db, _ := readContent(do)
	if !bytes.Equal(qb, db) {
		t.Fatal("copied bytes differ")
	}
	// copyObject detects hash drift.
	if _, err := copyObject(memory.NewStorage(), &lyingObject{EncodedObject: qo, content: []byte("tampered")}); err == nil {
		t.Fatal("hash drift not detected")
	}
}

// lyingObject reports one hash but carries other content.
type lyingObject struct {
	plumbing.EncodedObject
	content []byte
}

func (l *lyingObject) Size() int64 { return int64(len(l.content)) }
func (l *lyingObject) Reader() (io.ReadCloser, error) {
	return io.NopCloser(bytes.NewReader(l.content)), nil
}

// lyingStore returns tampered content for one object id.
type lyingStore struct {
	storer.EncodedObjectStorer
	target plumbing.Hash
}

func (l lyingStore) EncodedObject(t plumbing.ObjectType, h plumbing.Hash) (plumbing.EncodedObject, error) {
	o, err := l.EncodedObjectStorer.EncodedObject(t, h)
	if err != nil || h != l.target {
		return o, err
	}
	c, _ := readContent(o)
	return &lyingObject{EncodedObject: o, content: append(c, '!')}, nil
}

func TestValidateClosure(t *testing.T) {
	ms := memory.NewStorage()
	tree, _ := writeTree(ms, map[string]fileSpec{
		"a.txt":   {filemode.Regular, []byte("a")},
		"x.sh":    {filemode.Executable, []byte("x")},
		"link":    {filemode.Symlink, []byte("a.txt")},
		"d/b.txt": {filemode.Regular, []byte("b")},
	})
	c1, _ := writeCommit(ms, tree, nil, "one", fixedWhen)
	c2, _ := writeCommit(ms, tree, []plumbing.Hash{c1}, "two", fixedWhen)
	order, err := validateClosure(ms, c2)
	if err != nil {
		t.Fatal(err)
	}
	// 2 commits + 2 trees + 4 blobs (shared tree visited once).
	if len(order) != 8 {
		t.Fatalf("closure size = %d", len(order))
	}
	if _, err := validateClosure(ms, plumbing.NewHash("1234567890123456789012345678901234567890")); err == nil || safeMessage(err) != msgClosure {
		t.Fatalf("absent tip = %v", err)
	}
	blob, _ := writeRaw(ms, plumbing.BlobObject, []byte("b"))
	if _, err := validateClosure(ms, blob); safeMessage(err) != msgNotCommit {
		t.Fatalf("blob tip = %v", err)
	}
	// A tree entry claiming "blob" that is actually a commit.
	wrong, _ := writeObject(ms, &object.Tree{Entries: []object.TreeEntry{{Name: "f", Mode: filemode.Regular, Hash: c1}}})
	cw, _ := writeCommit(ms, wrong, nil, "wrong type", fixedWhen)
	if _, err := validateClosure(ms, cw); err == nil || !strings.Contains(err.Error(), "want blob") {
		t.Fatalf("wrong type = %v", err)
	}
	// Gitlinks are unsupported.
	gl, _ := writeObject(ms, &object.Tree{Entries: []object.TreeEntry{{Name: "sub", Mode: filemode.Submodule, Hash: c1}}})
	cg, _ := writeCommit(ms, gl, nil, "gitlink", fixedWhen)
	if _, err := validateClosure(ms, cg); err == nil || !strings.Contains(err.Error(), "unsupported entry mode") {
		t.Fatalf("gitlink = %v", err)
	}
	// Tampered bytes fail the hash check.
	if _, err := validateClosure(lyingStore{ms, tree}, c2); err == nil || !strings.Contains(err.Error(), "size mismatch") && !strings.Contains(err.Error(), "hash mismatch") {
		t.Fatalf("tampered = %v", err)
	}
	// Undecodable commit and tree bodies.
	badCommit, _ := writeRaw(ms, plumbing.CommitObject, []byte("tree nothex\n\nmsg"))
	if _, err := validateClosure(ms, badCommit); err == nil {
		t.Fatal("malformed commit accepted")
	}
	badTree, _ := writeRaw(ms, plumbing.TreeObject, []byte("100644 x"))
	cbt, _ := writeCommit(ms, badTree, nil, "bad tree", fixedWhen)
	if _, err := validateClosure(ms, cbt); err == nil {
		t.Fatal("malformed tree accepted")
	}
}

func TestPromoteAndPublish(t *testing.T) {
	h, bare, c0, tree := newTestHandler(t)
	ms := memory.NewStorage()
	nb, _ := writeRaw(ms, plumbing.BlobObject, []byte("promote me"))
	nt, _ := writeObject(ms, &object.Tree{Entries: []object.TreeEntry{{Name: "p", Mode: filemode.Regular, Hash: nb}}})
	nc, _ := writeCommit(ms, nt, []plumbing.Hash{c0}, "promote", fixedWhen)
	closure := []plumbing.Hash{nc, nt, nb, c0, tree}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := h.promoteObjects(ctx, ms, closure); safeMessage(err) != msgCancelled {
		t.Fatalf("cancelled promote = %v", err)
	}
	h.hooks.promote = func(plumbing.Hash) error { return errors.New("injected") }
	if _, err := h.promoteObjects(context.Background(), ms, closure); safeMessage(err) != msgPromoteFailed {
		t.Fatalf("hook failure = %v", err)
	}
	h.hooks = hooks{}
	if _, err := h.promoteObjects(context.Background(), memory.NewStorage(), closure); safeMessage(err) != msgPromoteFailed {
		t.Fatalf("missing in quarantine = %v", err)
	}
	n, err := h.promoteObjects(context.Background(), ms, closure)
	if err != nil || n != 3 {
		t.Fatalf("promote = %d %v", n, err)
	}
	// Existing durable objects are skipped.
	if n, _ := h.promoteObjects(context.Background(), ms, closure); n != 0 {
		t.Fatalf("second promote copied %d", n)
	}
	if err := publishRef(bare, "refs/callsheet/tasks/p", nc); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(filepath.Join(bare, "refs", "callsheet", "tasks", "p"))
	if string(b) != nc.String()+"\n" {
		t.Fatalf("ref file = %q", b)
	}
	if refSnapshot(t, bare)["refs/callsheet/tasks/p"] != nc.String() {
		t.Fatal("published ref not visible")
	}
	entries, _ := os.ReadDir(filepath.Join(bare, "refs", "callsheet", "tasks"))
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".tmp") {
			t.Fatalf("temp ref left behind: %s", e.Name())
		}
	}
	// Failure paths: parent is a file; target is a directory.
	os.WriteFile(filepath.Join(bare, "refs", "heads", "file"), []byte("x"), 0o644)
	if err := publishRef(bare, "refs/heads/file/x", nc); err == nil {
		t.Fatal("publish under a file succeeded")
	}
	os.MkdirAll(filepath.Join(bare, "refs", "heads", "dir", "x"), 0o755)
	if err := publishRef(bare, "refs/heads/dir", nc); err == nil {
		t.Fatal("publish over a directory succeeded")
	}
	entries, _ = os.ReadDir(filepath.Join(bare, "refs", "heads"))
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".tmp") {
			t.Fatalf("temp ref left behind after failure: %s", e.Name())
		}
	}
}

func TestLockWriteCancellation(t *testing.T) {
	h, _, _, _ := newTestHandler(t)
	h.mu.RLock()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if err := h.lockWrite(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("lockWrite = %v", err)
	}
	h.mu.RUnlock()
	// The abandoned acquisition releases itself; the lock becomes free.
	deadline := time.Now().Add(5 * time.Second)
	for !h.mu.TryLock() {
		if time.Now().After(deadline) {
			t.Fatal("abandoned write lock never released")
		}
		time.Sleep(time.Millisecond)
	}
	h.mu.Unlock()
	if err := h.lockWrite(context.Background()); err != nil {
		t.Fatal(err)
	}
	h.mu.Unlock()
}

func TestReceiveCancelledWhileWaitingForLock(t *testing.T) {
	h, _, c0, tree := newTestHandler(t)
	ms := memory.NewStorage()
	nc, _ := writeCommit(ms, tree, []plumbing.Hash{c0}, "waiting", fixedWhen)
	h.mu.RLock()
	ctx, cancel := context.WithCancel(context.Background())
	req := httptest.NewRequest("POST", receivePackPath, receiveBody(t, []*packp.Command{cmd("refs/callsheet/tasks/w", plumbing.ZeroHash, nc)}, []capability.Capability{capability.ReportStatus}, packOf(t, ms, nc))).WithContext(ctx)
	rec := httptest.NewRecorder()
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		h.ServeHTTP(rec, req)
	}()
	time.Sleep(20 * time.Millisecond)
	cancel()
	wg.Wait()
	h.mu.RUnlock()
	r := decodeReport(t, rec)
	if r.CommandStatuses[0].Status != msgCancelled {
		t.Fatalf("report = %+v", r.CommandStatuses[0])
	}
	if !strings.Contains(h.lastError, "context canceled") {
		t.Fatalf("lastError = %q", h.lastError)
	}
}

func TestFilterCapabilitiesAndReopen(t *testing.T) {
	in := capability.NewList()
	in.Set(capability.Agent, "x/1")
	in.Set(capability.DeleteRefs)
	in.Set(capability.ReportStatus)
	in.Add(capability.SymRef, "HEAD:refs/heads/main")
	out, err := filterCapabilities(in, receiveAllow)
	if err != nil || out.Supports(capability.DeleteRefs) || out.Supports(capability.SymRef) || out.Get(capability.Agent)[0] != "x/1" {
		t.Fatalf("filtered = %v %v", out, err)
	}
	h, _, c0, _ := newTestHandler(t)
	if err := h.reopen(); err != nil {
		t.Fatal(err)
	}
	if r, _ := readRef(h.store, plumbing.Main); r == nil || r.Hash() != c0 {
		t.Fatal("reopened store lost main")
	}
}

func TestAdvertisementEncodeFailureIs500(t *testing.T) {
	ar := packp.NewAdvRefs()
	// A ref name longer than a pkt-line payload makes Encode fail.
	ar.References["refs/heads/"+strings.Repeat("x", pktline.MaxPayloadSize)] = plumbing.NewHash("0123456789012345678901234567890123456789")
	rec := httptest.NewRecorder()
	writeAdvertisement(rec, serviceUpload, ar)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("encode failure = %d, want 500", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); strings.Contains(ct, "advertisement") {
		t.Fatalf("failed advertisement kept content type %q", ct)
	}
	if strings.Contains(rec.Body.String(), "# service=") {
		t.Fatalf("partial advertisement written: %q", rec.Body.String())
	}
}
