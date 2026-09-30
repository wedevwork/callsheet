package workspace

import (
	"bufio"
	"bytes"
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/filemode"
	"github.com/go-git/go-git/v5/plumbing/format/pktline"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/go-git/go-git/v5/plumbing/protocol/packp"
	"github.com/go-git/go-git/v5/plumbing/protocol/packp/capability"
	"github.com/go-git/go-git/v5/storage/memory"

	"github.com/wedevwork/callsheet/internal/contract"
	"github.com/wedevwork/callsheet/internal/testkit"
)

// transportFixture: workspace alpha with main = c0 over the production
// handler, plus a local store holding c0 and a child c1.
type transportFixture struct {
	m      *Manager
	inst   string
	local  *memStore
	c0, c1 plumbing.Hash
	rec    *recorder
}

type memStore = memory.Storage

func newTransportFixture(t *testing.T) *transportFixture {
	t.Helper()
	m, _ := newManager(t)
	v := mustCreate(t, m, "alpha")
	f := &transportFixture{m: m, inst: v.Instance, local: testkit.NewMemoryStore(), rec: &recorder{}}
	f.c0 = commit(t, f.local, seedFiles(), "c0")
	files := seedFiles()
	files["next.txt"] = testkit.FileSpec{Mode: filemode.Regular, Content: []byte("next\n")}
	f.c1 = commit(t, f.local, files, "c1", f.c0)
	if r := f.receive(t, cmd("refs/heads/main", plumbing.ZeroHash, f.c0), f.pack(t, f.c0)); !r.ok() {
		t.Fatalf("seed: %s", r)
	}
	m.d.observe = f.rec.observe
	return f
}

func (f *transportFixture) pack(t testing.TB, want plumbing.Hash, haves ...plumbing.Hash) []byte {
	return packOf(t, f.local, objectsFor(t, f.local, want, haves...)...)
}

func (f *transportFixture) receive(t testing.TB, cmds []*packp.Command, pack []byte, extra ...capability.Capability) rpc {
	t.Helper()
	return receiveDirect(t, f.m, "alpha", f.inst, cmds, pack, extra...)
}

// generationWrites counts generation creations since the last reset.
func (f *transportFixture) generationWrites() int {
	n := 0
	for _, o := range f.rec.all() {
		if strings.HasPrefix(o, "mkdir ") && strings.Contains(o, "/"+generationsName+"/") {
			n++
		}
	}
	f.rec.mu.Lock()
	f.rec.ops = nil
	f.rec.mu.Unlock()
	return n
}

func wantReport(t *testing.T, r rpc, unpack, status string) {
	t.Helper()
	if r.status != http.StatusOK || r.report == nil || r.report.UnpackStatus != unpack || len(r.report.CommandStatuses) != 1 || r.report.CommandStatuses[0].Status != status {
		t.Fatalf("want %q/%q, got %s", unpack, status, r)
	}
}

// UT-4: the portable branch-name contract is enforced on raw receive-pack
// targets before unpacking or any generation write, with the fixed
// report; refs are preserved. Boundaries: 200-byte components and a
// 512-byte full ref are accepted, 201/256-byte components and 513 bytes
// rejected.
func TestReceiveRefNames(t *testing.T) {
	f := newTransportFixture(t)
	a200 := strings.Repeat("a", 200)
	full512 := "refs/heads/" + a200 + "/" + a200 + "/" + strings.Repeat("c", 99)
	if len(full512) != 512 {
		t.Fatalf("fixture %d", len(full512))
	}
	before := refsOf(t, f.m, "alpha")
	f.generationWrites()
	for _, bad := range []string{
		"refs/heads/Main", "refs/heads/MAIN", "refs/heads/café", "refs/heads/café", "refs/heads/ma\xffin",
		"refs/heads/" + strings.Repeat("a", 201), "refs/heads/" + strings.Repeat("a", 256), full512 + "d",
		"refs/heads/a..b", "refs/heads/x.lock", "refs/heads/x.", "refs/heads/", "refs/heads/-x", "refs/tags/v1",
		"refs/callsheet/tasks/" + taskID('1'), "HEAD", "refs/heads/a//b", "refs/heads/a~1", "refs/heads/a@{0}",
	} {
		r := f.receive(t, cmd(bad, plumbing.ZeroHash, f.c1), f.pack(t, f.c1, f.c0))
		wantReport(t, r, unpackNotAttempted, reasonInvalidRef)
		if n := f.generationWrites(); n != 0 {
			t.Fatalf("%q wrote %d generations", bad, n)
		}
	}
	if got := refsOf(t, f.m, "alpha"); fmt.Sprint(got) != fmt.Sprint(before) {
		t.Fatalf("refs changed %v", got)
	}
	for _, good := range []string{"refs/heads/" + strings.Repeat("z", 200), full512, "refs/heads/main-2", "refs/heads/a.b_c/d-e"} {
		if r := f.receive(t, cmd(good, plumbing.ZeroHash, f.c1), f.pack(t, f.c1, f.c0)); !r.ok() {
			t.Fatalf("%d bytes: %s", len(good), r)
		}
	}
	if got := refsOf(t, f.m, "alpha"); got[full512] != f.c1.String() || got["refs/heads/main"] != f.c0.String() {
		t.Fatalf("refs %v", got)
	}
}

// UT-4: framing, capability and command policy are HTTP 400 before any
// generation; the instance header is required (400) and must match (409).
func TestReceivePolicy(t *testing.T) {
	f := newTransportFixture(t)
	pack := f.pack(t, f.c1, f.c0)
	f.generationWrites()
	for name, r := range map[string]rpc{
		"two commands": f.receive(t, append(cmd("refs/heads/a", plumbing.ZeroHash, f.c1), cmd("refs/heads/b", plumbing.ZeroHash, f.c1)...), pack),
		"delete":       f.receive(t, cmd("refs/heads/main", f.c0, plumbing.ZeroHash), nil),
		// The library encoder refuses zero/zero, so the line is framed here.
		"zero both": serveDirect(f.m, http.MethodPost, "/ws/alpha.git/git-receive-pack", http.Header{contract.WorkspaceInstanceHeader: {f.inst}},
			pktLines(strings.Repeat("0", 40)+" "+strings.Repeat("0", 40)+" refs/heads/x\x00report-status\n")),
		"delete-refs": f.receive(t, cmd("refs/heads/x", plumbing.ZeroHash, f.c1), pack, capability.DeleteRefs),
		"atomic":      f.receive(t, cmd("refs/heads/x", plumbing.ZeroHash, f.c1), pack, capability.Atomic),
		"side-band":   f.receive(t, cmd("refs/heads/x", plumbing.ZeroHash, f.c1), pack, capability.Sideband64k),
		"no report": serveDirect(f.m, http.MethodPost, "/ws/alpha.git/git-receive-pack", http.Header{contract.WorkspaceInstanceHeader: {f.inst}},
			encodeReceive(t, cmd("refs/heads/x", plumbing.ZeroHash, f.c1), nil, pack)),
		"garbage": serveDirect(f.m, http.MethodPost, "/ws/alpha.git/git-receive-pack", http.Header{contract.WorkspaceInstanceHeader: {f.inst}}, []byte("zzzz")),
	} {
		if r.status != http.StatusBadRequest {
			t.Fatalf("%s: %s", name, r)
		}
	}
	body := encodeReceive(t, cmd("refs/heads/x", plumbing.ZeroHash, f.c1), []capability.Capability{capability.ReportStatus}, pack)
	if r := serveDirect(f.m, http.MethodPost, "/ws/alpha.git/git-receive-pack", nil, body); r.status != http.StatusBadRequest {
		t.Fatalf("missing instance: %s", r)
	}
	if r := serveDirect(f.m, http.MethodPost, "/ws/alpha.git/git-receive-pack", http.Header{contract.WorkspaceInstanceHeader: {f.inst, f.inst}}, body); r.status != http.StatusBadRequest {
		t.Fatalf("two instance headers: %s", r)
	}
	if r := serveDirect(f.m, http.MethodPost, "/ws/alpha.git/git-receive-pack", http.Header{contract.WorkspaceInstanceHeader: {strings.Repeat("0", 32)}}, body); r.status != http.StatusConflict {
		t.Fatalf("stale instance: %s", r)
	}
	if r := serveDirect(f.m, http.MethodPost, "/ws/nope.git/git-receive-pack", http.Header{contract.WorkspaceInstanceHeader: {f.inst}}, body); r.status != http.StatusNotFound {
		t.Fatalf("missing workspace: %s", r)
	}
	if n := f.generationWrites(); n != 0 {
		t.Fatalf("policy failures wrote %d generations", n)
	}
}

// UT-4: CAS: zero old is create-only; a nonzero old must equal the current
// value, also when new already equals it (a stale no-op); an explicit
// non-fast-forward update with the matching old is allowed and moves only
// the named branch; namespace conflicts are refused.
func TestReceiveCAS(t *testing.T) {
	f := newTransportFixture(t)
	wantReport(t, f.receive(t, cmd("refs/heads/main", plumbing.ZeroHash, f.c1), f.pack(t, f.c1, f.c0)), unpackNotAttempted, reasonStaleOld)
	wantReport(t, f.receive(t, cmd("refs/heads/main", f.c1, f.c0), f.pack(t, f.c0)), unpackNotAttempted, reasonStaleOld)
	wantReport(t, f.receive(t, cmd("refs/heads/other", f.c0, f.c1), f.pack(t, f.c1, f.c0)), unpackNotAttempted, reasonStaleOld)
	if r := f.receive(t, cmd("refs/heads/side", plumbing.ZeroHash, f.c1), f.pack(t, f.c1, f.c0)); !r.ok() {
		t.Fatal(r)
	}
	// Non-fast-forward (c1 -> an unrelated root) with the matching old.
	root := commit(t, f.local, map[string]testkit.FileSpec{"x": {Mode: filemode.Regular, Content: []byte("x")}}, "unrelated")
	if r := f.receive(t, cmd("refs/heads/side", f.c1, root), f.pack(t, root)); !r.ok() {
		t.Fatal(r)
	}
	if got := refsOf(t, f.m, "alpha"); got["refs/heads/side"] != root.String() || got["refs/heads/main"] != f.c0.String() {
		t.Fatalf("refs %v", got)
	}
	wantReport(t, f.receive(t, cmd("refs/heads/side/x", plumbing.ZeroHash, f.c1), f.pack(t, f.c1, f.c0)), unpackNotAttempted, reasonConflict)
	wantReport(t, f.receive(t, cmd("refs/heads/main/x", plumbing.ZeroHash, f.c1), f.pack(t, f.c1, f.c0)), unpackNotAttempted, reasonConflict)
}

// UT-4: integrity: malformed and truncated packs, absent or wrong-type
// objects and missing ancestors fail with the fixed safe reasons, never
// library text, and preserve every ref; extra objects are fine but stay
// unfetchable by hash.
func TestReceiveIntegrity(t *testing.T) {
	f := newTransportFixture(t)
	s := f.local
	before := refsOf(t, f.m, "alpha")
	full := f.pack(t, f.c1, f.c0)
	blob, _ := testkit.WriteRaw(s, plumbing.BlobObject, []byte("just a blob"))
	// A commit whose parent is absent everywhere.
	orphanTree := mustTree(t, s)
	missingParent, _ := testkit.WriteCommit(s, orphanTree, []plumbing.Hash{plumbing.NewHash(strings.Repeat("e", 40))}, "orphan parent", testkit.FixedWhen)
	// A commit whose "tree" is a blob.
	badTree, _ := testkit.WriteObject(s, &object.Commit{TreeHash: blob, Message: "bad tree", Author: sig(), Committer: sig()})
	// A tree with a gitlink.
	gitlinkTree, _ := testkit.WriteObject(s, &object.Tree{Entries: []object.TreeEntry{{Name: "sub", Mode: filemode.Submodule, Hash: f.c0}}})
	gitlink, _ := testkit.WriteCommit(s, gitlinkTree, nil, "gitlink", testkit.FixedWhen)
	// A tree with an unsafe name.
	dotgitTree, _ := testkit.WriteObject(s, &object.Tree{Entries: []object.TreeEntry{{Name: ".GIT", Mode: filemode.Regular, Hash: blob}}})
	dotgit, _ := testkit.WriteCommit(s, dotgitTree, nil, "dotgit", testkit.FixedWhen)
	// One tree object reached as a directory and as a file (both orders).
	aliasDir, aliasDirObjs := aliasedTreeCommit(t, s, true)
	aliasFile, aliasFileObjs := aliasedTreeCommit(t, s, false)
	cases := []struct {
		name   string
		new    plumbing.Hash
		pack   []byte
		unpack string
		status string
	}{
		{"truncated pack", f.c1, full[:len(full)/2], unpackFailed, reasonClosure},
		{"corrupt pack", f.c1, append(append([]byte(nil), full[:len(full)-8]...), bytes.Repeat([]byte{0xff}, 8)...), unpackFailed, reasonClosure},
		{"garbage pack", f.c1, []byte("PACK garbage"), unpackFailed, reasonClosure},
		{"absent tip", plumbing.NewHash(strings.Repeat("d", 40)), packOf(t, s), unpackOK, reasonClosure},
		{"tip is a blob", blob, packOf(t, s, blob), unpackOK, reasonClosure},
		{"missing ancestor", missingParent, packOf(t, s, append([]plumbing.Hash{missingParent}, treeObjects(t, s, orphanTree)...)...), unpackOK, reasonClosure},
		{"tree is a blob", badTree, packOf(t, s, badTree, blob), unpackOK, reasonClosure},
		{"gitlink", gitlink, packOf(t, s, gitlink, gitlinkTree), unpackOK, reasonClosure},
		{"dot git name", dotgit, packOf(t, s, dotgit, dotgitTree, blob), unpackOK, reasonClosure},
		{"missing tree objects", f.c1, packOf(t, s, f.c1), unpackOK, reasonClosure},
		{"tree reused as a file after a directory", aliasDir, packOf(t, s, aliasDirObjs...), unpackOK, reasonClosure},
		{"tree reused as a directory after a file", aliasFile, packOf(t, s, aliasFileObjs...), unpackOK, reasonClosure},
	}
	for _, c := range cases {
		r := f.receive(t, cmd("refs/heads/x", plumbing.ZeroHash, c.new), c.pack)
		wantReport(t, r, c.unpack, c.status)
		if got := refsOf(t, f.m, "alpha"); fmt.Sprint(got) != fmt.Sprint(before) {
			t.Fatalf("%s changed refs %v", c.name, got)
		}
	}
	// Extra objects ride along but cannot be fetched by hash.
	pack := packOf(t, s, append(objectsFor(t, s, f.c1, f.c0), blob, badTree)...)
	if r := f.receive(t, cmd("refs/heads/x", plumbing.ZeroHash, f.c1), pack); !r.ok() {
		t.Fatal(r)
	}
	if r := upload(t, f.m, "alpha", []plumbing.Hash{badTree}, nil, nil); r.status != http.StatusBadRequest {
		t.Fatalf("orphan want %s", r)
	}
}

// pktLines frames lines as pkt-lines followed by a flush.
func pktLines(lines ...string) []byte {
	var buf bytes.Buffer
	enc := pktline.NewEncoder(&buf)
	for _, l := range lines {
		enc.EncodeString(l)
	}
	enc.Flush()
	return buf.Bytes()
}

// treeObjects lists a tree and its blobs (one level).
func treeObjects(t testing.TB, s *memStore, tree plumbing.Hash) []plumbing.Hash {
	t.Helper()
	tr, err := object.GetTree(s, tree)
	if err != nil {
		t.Fatal(err)
	}
	out := []plumbing.Hash{tree}
	for _, e := range tr.Entries {
		out = append(out, e.Hash)
	}
	return out
}

// aliasedTreeCommit writes a commit whose root tree names one tree object
// twice: as directory "a" and as regular file "b" (dirFirst), or as file
// "a" and directory "b". It returns the commit and every object the pack
// needs. The object exists and hashes correctly; only the second edge's
// required type is wrong.
func aliasedTreeCommit(t testing.TB, s *memStore, dirFirst bool) (plumbing.Hash, []plumbing.Hash) {
	t.Helper()
	shared := mustTree(t, s)
	modeA, modeB := filemode.Dir, filemode.Regular
	if !dirFirst {
		modeA, modeB = filemode.Regular, filemode.Dir
	}
	root, err := testkit.WriteObject(s, &object.Tree{Entries: []object.TreeEntry{
		{Name: "a", Mode: modeA, Hash: shared},
		{Name: "b", Mode: modeB, Hash: shared},
	}})
	if err != nil {
		t.Fatal(err)
	}
	c, err := testkit.WriteCommit(s, root, nil, fmt.Sprintf("aliased tree dirFirst=%v", dirFirst), testkit.FixedWhen)
	if err != nil {
		t.Fatal(err)
	}
	return c, append([]plumbing.Hash{c, root}, treeObjects(t, s, shared)...)
}

func sig() object.Signature {
	return object.Signature{Name: "x", Email: "x@callsheet.invalid", When: testkit.FixedWhen}
}

func mustTree(t testing.TB, s *memStore) plumbing.Hash {
	t.Helper()
	h, err := testkit.WriteTree(s, map[string]testkit.FileSpec{"f": {Mode: filemode.Regular, Content: []byte("tree")}})
	if err != nil {
		t.Fatal(err)
	}
	return h
}

// upload posts a raw upload-pack request (wants, then haves and done).
func upload(t testing.TB, m *Manager, name string, wants, haves []plumbing.Hash, mutate func(*packp.UploadPackRequest)) rpc {
	t.Helper()
	req := packp.NewUploadPackRequest()
	req.Capabilities.Set(capability.OFSDelta)
	req.Wants = wants
	if mutate != nil {
		mutate(req)
	}
	var buf bytes.Buffer
	enc := pktline.NewEncoder(&buf)
	if len(req.Wants) == 0 {
		// The library encoder refuses a want-less request.
		enc.Flush()
	} else if err := req.UploadRequest.Encode(&buf); err != nil {
		t.Fatal(err)
	}
	for _, h := range haves {
		enc.Encodef("have %s\n", h)
	}
	enc.Encodef("done\n")
	return serveDirect(m, http.MethodPost, "/ws/"+name+".git/git-upload-pack", nil, buf.Bytes())
}

// UT-4: upload wants must be commits reachable from advertised refs
// (ancestors included); shallow, deepen, filters, unknown capabilities and
// malformed haves are refused; unknown haves are dropped.
func TestUploadPolicy(t *testing.T) {
	f := newTransportFixture(t)
	if r := f.receive(t, cmd("refs/heads/main", f.c0, f.c1), f.pack(t, f.c1, f.c0)); !r.ok() {
		t.Fatal(r)
	}
	if r := upload(t, f.m, "alpha", []plumbing.Hash{f.c0}, nil, nil); r.status != http.StatusOK {
		t.Fatalf("ancestor want %s", r)
	}
	unknown := plumbing.NewHash(strings.Repeat("9", 40))
	if r := upload(t, f.m, "alpha", []plumbing.Hash{f.c1}, []plumbing.Hash{unknown, f.c0}, nil); r.status != http.StatusOK {
		t.Fatalf("unknown have %s", r)
	}
	tree := commitTreeOf(t, f.local, f.c1)
	for name, r := range map[string]rpc{
		"tree want":   upload(t, f.m, "alpha", []plumbing.Hash{tree}, nil, nil),
		"absent want": upload(t, f.m, "alpha", []plumbing.Hash{unknown}, nil, nil),
		"no wants":    upload(t, f.m, "alpha", nil, nil, nil),
		"shallow":     upload(t, f.m, "alpha", []plumbing.Hash{f.c1}, nil, func(r *packp.UploadPackRequest) { r.Shallows = []plumbing.Hash{f.c0} }),
		"deepen":      upload(t, f.m, "alpha", []plumbing.Hash{f.c1}, nil, func(r *packp.UploadPackRequest) { r.Depth = packp.DepthCommits(1) }),
		"thin-pack":   upload(t, f.m, "alpha", []plumbing.Hash{f.c1}, nil, func(r *packp.UploadPackRequest) { r.Capabilities.Set(capability.ThinPack) }),
		"garbage":     serveDirect(f.m, http.MethodPost, "/ws/alpha.git/git-upload-pack", nil, []byte("zzzz")),
		"malformed have": serveDirect(f.m, http.MethodPost, "/ws/alpha.git/git-upload-pack", nil, func() []byte {
			var buf bytes.Buffer
			req := packp.NewUploadPackRequest()
			req.Wants = []plumbing.Hash{f.c1}
			req.UploadRequest.Encode(&buf)
			pktline.NewEncoder(&buf).Encodef("have nothex\n")
			return buf.Bytes()
		}()),
	} {
		if r.status != http.StatusBadRequest {
			t.Fatalf("%s: %s", name, r)
		}
	}
}

func commitTreeOf(t testing.TB, s *memStore, c plumbing.Hash) plumbing.Hash {
	t.Helper()
	cm, err := object.GetCommit(s, c)
	if err != nil {
		t.Fatal(err)
	}
	return cm.TreeHash
}

// UT-4: routing refuses malformed names, encoded paths, traversal, extra
// components and queries, other methods and HTTP/2.
func TestGitRouting(t *testing.T) {
	f := newTransportFixture(t)
	for _, c := range []struct {
		method, target string
		proto          int
		want           int
	}{
		{http.MethodGet, "/ws/alpha.git/info/refs?service=git-upload-pack", 1, http.StatusOK},
		{http.MethodGet, "/ws/alpha.git/info/refs?service=git-receive-pack", 1, http.StatusOK},
		{http.MethodGet, "/ws/alpha.git/info/refs?service=git-upload-pack", 2, http.StatusHTTPVersionNotSupported},
		{http.MethodGet, "/ws/alpha.git/info/refs", 1, http.StatusBadRequest},
		{http.MethodGet, "/ws/alpha.git/info/refs?service=git-upload-pack&x=1", 1, http.StatusBadRequest},
		{http.MethodGet, "/ws/alpha.git/info/refs?service=git-archive", 1, http.StatusBadRequest},
		{http.MethodPost, "/ws/alpha.git/info/refs?service=git-upload-pack", 1, http.StatusMethodNotAllowed},
		{http.MethodGet, "/ws/alpha.git/git-upload-pack", 1, http.StatusMethodNotAllowed},
		{http.MethodPost, "/ws/alpha.git/git-upload-pack?x=1", 1, http.StatusBadRequest},
		{http.MethodGet, "/ws/Alpha.git/info/refs?service=git-upload-pack", 1, http.StatusBadRequest},
		{http.MethodGet, "/ws/../alpha.git/info/refs?service=git-upload-pack", 1, http.StatusBadRequest},
		{http.MethodGet, "/ws/al%2Fpha.git/info/refs?service=git-upload-pack", 1, http.StatusBadRequest},
		{http.MethodGet, "/ws/alpha.git/objects/info/packs", 1, http.StatusNotFound},
		{http.MethodGet, "/ws/alpha.git/info/refs/x?service=git-upload-pack", 1, http.StatusNotFound},
		{http.MethodGet, "/ws/alpha", 1, http.StatusBadRequest},
		{http.MethodGet, "/ws/nope.git/info/refs?service=git-upload-pack", 1, http.StatusNotFound},
	} {
		req := httptest.NewRequest(c.method, c.target, nil)
		req.ProtoMajor = c.proto
		rec := httptest.NewRecorder()
		f.m.ServeGit(rec, req)
		if rec.Code != c.want {
			t.Fatalf("%s %s HTTP/%d = %d %q", c.method, c.target, c.proto, rec.Code, rec.Body.String())
		}
	}
	// A receive advertisement with a stale instance header answers 409
	// early; the upload advertisement filters capabilities.
	r := serveDirect(f.m, http.MethodGet, "/ws/alpha.git/info/refs?service=git-receive-pack", http.Header{contract.WorkspaceInstanceHeader: {strings.Repeat("0", 32)}}, nil)
	if r.status != http.StatusConflict {
		t.Fatalf("stale advertisement %s", r)
	}
	r = serveDirect(f.m, http.MethodGet, "/ws/alpha.git/info/refs?service=git-receive-pack", nil, nil)
	if strings.Contains(r.body, "delete-refs") || !strings.Contains(r.body, "report-status") {
		t.Fatalf("receive capabilities %q", r.body)
	}
}

// UT-4: exactly one of several concurrent writers with the same old value
// wins; the rest get stale old ref and the winner's value stands.
func TestReceiveConcurrentCAS(t *testing.T) {
	f := newTransportFixture(t)
	const n = 6
	var heads []plumbing.Hash
	for i := range n {
		files := seedFiles()
		files["w.txt"] = testkit.FileSpec{Mode: filemode.Regular, Content: []byte(fmt.Sprint("writer ", i))}
		heads = append(heads, commit(t, f.local, files, fmt.Sprint("w", i), f.c0))
	}
	start := make(chan struct{})
	var wg sync.WaitGroup
	results := make([]rpc, n)
	for i := range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			results[i] = f.receive(t, cmd("refs/heads/main", f.c0, heads[i]), f.pack(t, heads[i], f.c0))
		}()
	}
	close(start)
	wg.Wait()
	winners := 0
	var won plumbing.Hash
	for i, r := range results {
		switch {
		case r.ok():
			winners++
			won = heads[i]
		default:
			wantReport(t, r, unpackNotAttempted, reasonStaleOld)
		}
	}
	if winners != 1 || refsOf(t, f.m, "alpha")["refs/heads/main"] != won.String() {
		t.Fatalf("%d winners", winners)
	}
}

// UT-4: cancellation before publication reports "cancelled before
// publication" and leaves refs and the generation untouched.
func TestReceiveCancellation(t *testing.T) {
	f := newTransportFixture(t)
	gen := genOf(t, f.m, "alpha")
	body := encodeReceive(t, cmd("refs/heads/main", f.c0, f.c1), []capability.Capability{capability.ReportStatus}, f.pack(t, f.c1, f.c0))
	for _, stage := range []string{"receive-locked", "receive-unpacked", "before-publish"} {
		ctx, cancel := context.WithCancel(context.Background())
		f.m.d.hook = func(s string, _ context.Context) {
			if s == stage {
				cancel()
			}
		}
		req := httptest.NewRequest(http.MethodPost, "/ws/alpha.git/git-receive-pack", bytes.NewReader(body)).WithContext(ctx)
		req.Header.Set(contract.WorkspaceInstanceHeader, f.inst)
		rec := httptest.NewRecorder()
		f.m.ServeGit(rec, req)
		f.m.d.hook = nil
		rs := packp.NewReportStatus()
		if err := rs.Decode(bytes.NewReader(rec.Body.Bytes())); err != nil || rs.CommandStatuses[0].Status != reasonCancelled {
			t.Fatalf("%s: %d %q", stage, rec.Code, rec.Body.String())
		}
		if genOf(t, f.m, "alpha") != gen || refsOf(t, f.m, "alpha")["refs/heads/main"] != f.c0.String() {
			t.Fatalf("%s changed state", stage)
		}
		cancel()
	}
	// A cancelled lock wait never enters.
	ctx, cancel := context.WithCancel(context.Background())
	h, _ := f.m.lookup("alpha")
	h.lock.Lock(context.Background())
	req := httptest.NewRequest(http.MethodPost, "/ws/alpha.git/git-receive-pack", bytes.NewReader(body)).WithContext(ctx)
	req.Header.Set(contract.WorkspaceInstanceHeader, f.inst)
	rec := httptest.NewRecorder()
	done := make(chan struct{})
	go func() { f.m.ServeGit(rec, req); close(done) }()
	cancel()
	<-done
	h.lock.Unlock()
	if !strings.Contains(rec.Body.String(), reasonCancelled) {
		t.Fatalf("cancelled lock wait %q", rec.Body.String())
	}
}

// blockingBody blocks a read until released.
type blockingBody struct {
	started chan struct{}
	release chan struct{}
	once    sync.Once
}

func (b *blockingBody) Read(p []byte) (int, error) {
	b.once.Do(func() { close(b.started) })
	<-b.release
	return 0, errors.New("released")
}

// deadlineRecorder records deadlines and releases the body on an expired
// read deadline, like a real connection.
type deadlineRecorder struct {
	*httptest.ResponseRecorder
	mu    sync.Mutex
	reads []time.Time
	body  *blockingBody
}

func (d *deadlineRecorder) SetReadDeadline(t time.Time) error {
	d.mu.Lock()
	d.reads = append(d.reads, t)
	d.mu.Unlock()
	if !t.IsZero() && !t.After(time.Now()) {
		close(d.body.release)
	}
	return nil
}

func (d *deadlineRecorder) SetWriteDeadline(time.Time) error { return nil }

// UT-4 (resource close/join): halt forces a pending body read to return
// through an expired deadline and joins it; every read installs a fresh
// no-progress deadline after the inherited ones are cleared.
func TestGitIOHaltJoins(t *testing.T) {
	body := &blockingBody{started: make(chan struct{}), release: make(chan struct{})}
	rec := &deadlineRecorder{ResponseRecorder: httptest.NewRecorder(), body: body}
	req := httptest.NewRequest(http.MethodPost, "/", io.NopCloser(body))
	g := newGitIO(rec, req, time.Minute)
	read := make(chan error, 1)
	go func() {
		_, err := g.Read(make([]byte, 8))
		read <- err
	}()
	<-body.started
	g.halt()
	// halt joined the read: the body read returned and left the in-flight
	// set before halt did. The reader's result send happens after that, so
	// it is awaited (bounded) rather than required to be ready already.
	g.mu.Lock()
	inflight, halted := g.inflight, g.halted
	g.mu.Unlock()
	if inflight != 0 || !halted {
		t.Fatalf("halt returned with %d reads in flight (halted %v)", inflight, halted)
	}
	select {
	case err := <-read:
		if err == nil {
			t.Fatal("the read did not fail")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the halted read never returned")
	}
	if _, err := g.Read(make([]byte, 1)); !errors.Is(err, errHalted) {
		t.Fatalf("read after halt: %v", err)
	}
	rec.mu.Lock()
	defer rec.mu.Unlock()
	if len(rec.reads) < 3 || !rec.reads[0].IsZero() || rec.reads[1].Sub(time.Now()) < 50*time.Second {
		t.Fatalf("deadlines %v", rec.reads)
	}
}

// UT-4: over real TLS, a request body that stops making progress is cut
// off by the no-progress deadline (not a total timeout) and the handler
// returns.
func TestIdleDeadline(t *testing.T) {
	m, _ := newManager(t)
	v := mustCreate(t, m, "alpha")
	m.d.idle = 200 * time.Millisecond
	done := make(chan struct{}, 1)
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		m.ServeGit(w, r)
		done <- struct{}{}
	}))
	srv.StartTLS()
	defer srv.Close()
	conn, err := tls.Dial("tcp", srv.Listener.Addr().String(), &tls.Config{InsecureSkipVerify: true})
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	fmt.Fprintf(conn, "POST /ws/alpha.git/git-receive-pack HTTP/1.1\r\nHost: x\r\n%s: %s\r\nContent-Length: 100000\r\n\r\n0014", contract.WorkspaceInstanceHeader, v.Instance)
	select {
	case <-done:
	case <-time.After(testWait):
		t.Fatal("a stalled body was never cut off")
	}
	resp, err := http.ReadResponse(bufio.NewReader(conn), nil)
	if err == nil && resp.StatusCode == http.StatusOK {
		t.Fatalf("stalled request answered %d", resp.StatusCode)
	}
}
