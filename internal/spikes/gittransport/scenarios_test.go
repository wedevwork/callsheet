package gittransport

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	git "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/config"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/filemode"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/go-git/go-git/v5/plumbing/protocol/packp"
	"github.com/go-git/go-git/v5/plumbing/protocol/packp/capability"
	"github.com/go-git/go-git/v5/storage/memory"

	"github.com/wedevwork/callsheet/internal/testkit"
)

const (
	// envSubprocess marks a run inside the PATH-empty helper subprocess
	// started by TestFP5GitRoundTrip (PATH is already empty there).
	envSubprocess = "CALLSHEET_GIT_SUBPROCESS"
	// envResults names a file receiving the per-scenario JSON report.
	envResults = "CALLSHEET_GIT_SCENARIO_RESULTS"
	// scenarioDeadline bounds each round-trip scenario.
	scenarioDeadline = 30 * time.Second
)

// ScenarioReport is the parsed result consumed by TestFP5GitRoundTrip.
type ScenarioReport struct {
	PathEmpty      bool              `json:"path_empty"`
	GitLookupError string            `json:"git_lookup_error"`
	Scenarios      map[string]string `json:"scenarios"`
	Order          []string          `json:"order"`
	Facts          map[string]string `json:"facts"`
	Transcript     []exchange        `json:"transcript"`
}

type scenarioEnv struct {
	t        *testing.T
	ctx      context.Context
	h        *smartHandler
	hk       *testkit.Harness
	url      string
	bare     string
	base     string
	c0       plumbing.Hash
	c0tree   plumbing.Hash
	c1, c2   plumbing.Hash
	c1tree   plumbing.Hash
	worker   *git.Repository
	workDir  string
	obs      *git.Repository
	facts    map[string]string
	cloneRsp int64
	dirs     int
}

func (e *scenarioEnv) dir(name string) string {
	e.dirs++
	return filepath.Join(e.base, fmt.Sprintf("%s-%d", name, e.dirs))
}

// assertPathEmpty proves PATH holds only empty directories and git is absent.
func assertPathEmpty(t *testing.T) string {
	t.Helper()
	entries := filepath.SplitList(os.Getenv("PATH"))
	if len(entries) == 0 {
		t.Fatal("PATH must name an empty directory, not be unset")
	}
	for _, d := range entries {
		list, err := os.ReadDir(d)
		if err != nil || len(list) != 0 {
			t.Fatalf("PATH entry %q is not an existing empty directory (%v, %d entries)", d, err, len(list))
		}
	}
	_, err := exec.LookPath("git")
	if err == nil {
		t.Fatal("git executable is reachable through PATH")
	}
	return err.Error()
}

func TestGitScenarios(t *testing.T) {
	report := &ScenarioReport{Scenarios: map[string]string{}, Facts: map[string]string{}}
	if p := os.Getenv(envResults); p != "" {
		t.Cleanup(func() {
			b, _ := json.MarshalIndent(report, "", "  ")
			if err := os.WriteFile(p, b, 0o600); err != nil {
				t.Errorf("write results: %v", err)
			}
		})
	}
	// PATH is emptied before any git work; in the helper subprocess it was
	// already empty when the process started.
	if os.Getenv(envSubprocess) != "1" {
		t.Setenv("PATH", testkit.EmptyDir(t))
	}
	for _, v := range testkit.ProxyVars {
		t.Setenv(v, "")
	}
	report.GitLookupError = assertPathEmpty(t)
	report.PathEmpty = true

	e := &scenarioEnv{t: t, base: t.TempDir(), facts: report.Facts}
	e.bare = filepath.Join(e.base, "server", "repo.git")
	e.c0, e.c0tree = seedBare(t, e.bare)
	hnd, err := NewSmartHandler(e.bare)
	if err != nil {
		t.Fatal(err)
	}
	e.h = hnd.(*smartHandler)
	e.hk = testkit.NewHarness(t, testkit.WithGitHandler(e.h))
	e.url = e.hk.URL + RepoURLPath
	e.facts["c0"], e.facts["c0_tree"] = e.c0.String(), e.c0tree.String()

	run := func(name string, f func(e *scenarioEnv)) {
		report.Order = append(report.Order, name)
		ok := t.Run(name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), scenarioDeadline)
			defer cancel()
			e.t, e.ctx = t, ctx
			defer func() { e.t, e.ctx = nil, nil }()
			f(e)
		})
		if ok {
			report.Scenarios[name] = "pass"
		} else {
			report.Scenarios[name] = "fail"
		}
	}
	run("seed-and-clone", scenarioSeedAndClone)
	run("task-ref-push-and-observer-fetch", scenarioTaskRefPush)
	run("incremental-push-and-persistence", scenarioIncremental)
	run("tls-wrong-ca", scenarioWrongCA)
	run("tls-san-mismatch", scenarioSANMismatch)
	run("stale-old-ref", scenarioStaleOld)
	run("racing-writers", scenarioRace)
	run("absent-object-create", scenarioAbsentObject)
	run("missing-blob-closure", scenarioMissingBlob)
	run("missing-parent-and-tree", scenarioMissingParentTree)
	run("blob-as-new", scenarioBlobAsNew)
	run("truncated-pack", scenarioTruncated)
	run("invalid-ref-delete-multi", scenarioInvalidRequests)
	run("unsupported-capability", scenarioCapabilities)
	run("cancel-stalled-body", scenarioStalledBody)
	run("cancel-before-publication", scenarioCancelBeforePublish)
	run("promotion-failure-then-recovery", scenarioPromotionFailure)

	final := refSnapshot(t, e.bare)
	e.facts["final_main"] = final["refs/heads/main"]
	e.facts["final_t1"] = final["refs/callsheet/tasks/t1"]
	report.Transcript = e.h.transcript()
}

func (e *scenarioEnv) since(mark int) []exchange { return e.h.transcript()[mark:] }

func hasExchange(xs []exchange, method, path, query string, status int) bool {
	for _, x := range xs {
		if x.Method == method && x.Path == path && x.Query == query && x.Status == status {
			return true
		}
	}
	return false
}

func sumResponse(xs []exchange, path string) (n int64) {
	for _, x := range xs {
		if x.Path == path {
			n += x.ResponseBytes
		}
	}
	return n
}

func sumRequest(xs []exchange, path string) (n int64) {
	for _, x := range xs {
		if x.Path == path {
			n += x.RequestBytes
		}
	}
	return n
}

func (e *scenarioEnv) requireUploadFetch(xs []exchange) {
	e.t.Helper()
	if !hasExchange(xs, "GET", infoRefsPath, "service=git-upload-pack", 200) || !hasExchange(xs, "POST", uploadPackPath, "", 200) {
		e.t.Fatalf("fetch did not use server upload-pack requests: %+v", xs)
	}
}

func scenarioSeedAndClone(e *scenarioEnv) {
	t := e.t
	mark := len(e.h.transcript())
	e.workDir = e.dir("worker")
	w, err := git.PlainCloneContext(e.ctx, e.workDir, false, &git.CloneOptions{URL: e.url, CABundle: e.hk.CAPEM})
	if err != nil {
		t.Fatalf("clone: %v", err)
	}
	e.worker = w
	head, err := w.Head()
	if err != nil || head.Hash() != e.c0 || head.Name() != plumbing.Main {
		t.Fatalf("clone HEAD = %v %v, want main %s", head, err, e.c0)
	}
	c, err := w.CommitObject(e.c0)
	if err != nil || c.TreeHash != e.c0tree {
		t.Fatalf("C0 tree mismatch: %v", err)
	}
	if err := equalFiles(treeFiles(t, w.Storer, c.TreeHash), seedFiles()); err != nil {
		t.Fatalf("C0 tree content: %v", err)
	}
	if treeFiles(t, w.Storer, c.TreeHash)["bin/run.sh"].Mode != filemode.Executable {
		t.Fatal("git tree mode for bin/run.sh is not executable")
	}
	for p, f := range seedFiles() {
		b, err := os.ReadFile(filepath.Join(e.workDir, filepath.FromSlash(p)))
		if err != nil || !bytes.Equal(b, f.Content) {
			t.Fatalf("checkout %s: %v", p, err)
		}
	}
	st, err := os.Stat(filepath.Join(e.workDir, "bin", "run.sh"))
	if err != nil {
		t.Fatalf("stat bin/run.sh: %v", err)
	}
	if st.Mode().Perm()&0o111 == 0 {
		t.Fatalf("bin/run.sh not executable on disk: %v", st.Mode())
	}
	xs := e.since(mark)
	e.requireUploadFetch(xs)
	e.cloneRsp = sumResponse(xs, uploadPackPath)
	e.facts["clone_upload_pack_response_bytes"] = fmt.Sprint(e.cloneRsp)
}

func (e *scenarioEnv) commitWorker(msg string, when time.Time, edit func(dir string, wt *git.Worktree)) plumbing.Hash {
	e.t.Helper()
	wt, err := e.worker.Worktree()
	if err != nil {
		e.t.Fatal(err)
	}
	edit(e.workDir, wt)
	s := sig(when)
	h, err := wt.Commit(msg, &git.CommitOptions{Author: &s, Committer: &s})
	if err != nil {
		e.t.Fatalf("commit: %v", err)
	}
	return h
}

func mustWrite(t *testing.T, p string, content string) {
	t.Helper()
	os.MkdirAll(filepath.Dir(p), 0o755)
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func (e *scenarioEnv) push(refspec string) {
	e.t.Helper()
	err := e.worker.PushContext(e.ctx, &git.PushOptions{RemoteName: "origin", RefSpecs: []config.RefSpec{config.RefSpec(refspec)}, CABundle: e.hk.CAPEM})
	if err != nil {
		e.t.Fatalf("push %s: %v", refspec, err)
	}
}

// fetchObserver fetches ref into a new or existing observer repository.
func (e *scenarioEnv) fetchObserver(obs *git.Repository, ref string) (*git.Repository, plumbing.Hash) {
	e.t.Helper()
	if obs == nil {
		var err error
		obs, err = git.PlainInit(e.dir("observer"), false)
		if err != nil {
			e.t.Fatal(err)
		}
		if _, err := obs.CreateRemote(&config.RemoteConfig{Name: "origin", URLs: []string{e.url}}); err != nil {
			e.t.Fatal(err)
		}
	}
	local := "refs/remotes/origin/" + filepath.Base(ref)
	err := obs.FetchContext(e.ctx, &git.FetchOptions{RemoteName: "origin", RefSpecs: []config.RefSpec{config.RefSpec("+" + ref + ":" + local)}, CABundle: e.hk.CAPEM})
	if err != nil && !errors.Is(err, git.NoErrAlreadyUpToDate) {
		e.t.Fatalf("observer fetch %s: %v", ref, err)
	}
	r, err := obs.Reference(plumbing.ReferenceName(local), true)
	if err != nil {
		e.t.Fatalf("observer ref: %v", err)
	}
	return obs, r.Hash()
}

func (e *scenarioEnv) requireRefs(want map[string]plumbing.Hash) {
	e.t.Helper()
	snap := refSnapshot(e.t, e.bare)
	for name, h := range want {
		if snap[name] != h.String() {
			e.t.Fatalf("server %s = %q, want %s", name, snap[name], h)
		}
	}
}

func scenarioTaskRefPush(e *scenarioEnv) {
	t := e.t
	if e.worker == nil {
		t.Fatal("no worker clone")
	}
	e.c1 = e.commitWorker("C1 task change", fixedWhen.Add(time.Hour), func(dir string, wt *git.Worktree) {
		mustWrite(t, filepath.Join(dir, "README.md"), "# Callsheet spike\nedited by task t1\n")
		mustWrite(t, filepath.Join(dir, "notes", "new.txt"), "added file\n")
		if _, err := wt.Remove("data.bin"); err != nil {
			t.Fatal(err)
		}
		for _, p := range []string{"README.md", "notes/new.txt"} {
			if _, err := wt.Add(p); err != nil {
				t.Fatal(err)
			}
		}
	})
	mark := len(e.h.transcript())
	e.push("refs/heads/main:refs/callsheet/tasks/t1")
	xs := e.since(mark)
	if !hasExchange(xs, "GET", infoRefsPath, "service=git-receive-pack", 200) || !hasExchange(xs, "POST", receivePackPath, "", 200) {
		t.Fatalf("push did not use server receive-pack requests: %+v", xs)
	}
	e.requireRefs(map[string]plumbing.Hash{"refs/heads/main": e.c0, "refs/callsheet/tasks/t1": e.c1})

	mark = len(e.h.transcript())
	obs, got := e.fetchObserver(nil, "refs/callsheet/tasks/t1")
	e.requireUploadFetch(e.since(mark))
	e.obs = obs
	if got != e.c1 {
		t.Fatalf("observer t1 = %s, want C1 %s", got, e.c1)
	}
	oc, err := obs.CommitObject(got)
	if err != nil || len(oc.ParentHashes) != 1 || oc.ParentHashes[0] != e.c0 {
		t.Fatalf("C1 parent: %v %v", oc, err)
	}
	wc, _ := e.worker.CommitObject(e.c1)
	if oc.TreeHash != wc.TreeHash {
		t.Fatal("observer tree differs from worker tree")
	}
	e.c1tree = oc.TreeHash
	want := seedFiles()
	delete(want, "data.bin")
	want["README.md"] = fileSpec{filemode.Regular, []byte("# Callsheet spike\nedited by task t1\n")}
	want["notes/new.txt"] = fileSpec{filemode.Regular, []byte("added file\n")}
	if err := equalFiles(treeFiles(t, obs.Storer, oc.TreeHash), want); err != nil {
		t.Fatalf("observer C1 content: %v", err)
	}
	if oc.Author.When.Unix() != fixedWhen.Add(time.Hour).Unix() || oc.Author.Email != "spike@callsheet.invalid" {
		t.Fatalf("author = %+v", oc.Author)
	}
	e.facts["c1"], e.facts["c1_tree"] = e.c1.String(), e.c1tree.String()
}

func scenarioIncremental(e *scenarioEnv) {
	t := e.t
	if e.c1.IsZero() || e.obs == nil {
		t.Fatal("C1 or observer missing")
	}
	e.c2 = e.commitWorker("C2 small change", fixedWhen.Add(2*time.Hour), func(dir string, wt *git.Worktree) {
		mustWrite(t, filepath.Join(dir, "notes", "new.txt"), "added file, revised\n")
		if _, err := wt.Add("notes/new.txt"); err != nil {
			t.Fatal(err)
		}
	})
	mark := len(e.h.transcript())
	e.push("refs/heads/main:refs/callsheet/tasks/t1")
	pushBytes := sumRequest(e.since(mark), receivePackPath)
	// Exactly the new closure: commit, root tree, notes/ tree and one blob.
	if n := e.h.lastPromoted(); n != 4 {
		t.Fatalf("incremental push promoted %d objects, want 4", n)
	}
	e.requireRefs(map[string]plumbing.Hash{"refs/heads/main": e.c0, "refs/callsheet/tasks/t1": e.c2})

	// The observer that already holds C1 fetches only the increment.
	mark = len(e.h.transcript())
	_, got := e.fetchObserver(e.obs, "refs/callsheet/tasks/t1")
	incBytes := sumResponse(e.since(mark), uploadPackPath)
	mark = len(e.h.transcript())
	_, gotFull := e.fetchObserver(nil, "refs/callsheet/tasks/t1")
	fullBytes := sumResponse(e.since(mark), uploadPackPath)
	if got != e.c2 || gotFull != e.c2 {
		t.Fatalf("observer t1 = %s / %s, want C2 %s", got, gotFull, e.c2)
	}
	e.facts["incremental_push_request_bytes"] = fmt.Sprint(pushBytes)
	e.facts["incremental_fetch_response_bytes"] = fmt.Sprint(incBytes)
	e.facts["full_fetch_response_bytes"] = fmt.Sprint(fullBytes)
	if pushBytes <= 0 || incBytes <= 0 || pushBytes*2 > fullBytes || incBytes*2 > fullBytes {
		t.Fatalf("not incremental: push %d, fetch %d, full fetch %d bytes", pushBytes, incBytes, fullBytes)
	}

	// Persistence: close and reopen the server store, then fetch again.
	if err := e.h.reopen(); err != nil {
		t.Fatalf("reopen: %v", err)
	}
	obs3, got3 := e.fetchObserver(nil, "refs/callsheet/tasks/t1")
	if got3 != e.c2 {
		t.Fatalf("after reopen t1 = %s", got3)
	}
	c2, err := obs3.CommitObject(got3)
	if err != nil || len(c2.ParentHashes) != 1 || c2.ParentHashes[0] != e.c1 {
		t.Fatalf("C2 parent is not C1: %v", err)
	}
	want := treeFiles(t, e.worker.Storer, c2.TreeHash)
	if err := equalFiles(treeFiles(t, obs3.Storer, c2.TreeHash), want); err != nil {
		t.Fatalf("C2 content after reopen: %v", err)
	}
	e.requireRefs(map[string]plumbing.Hash{"refs/heads/main": e.c0, "refs/callsheet/tasks/t1": e.c2})
	// The coordinator still sees main at C0 over the network.
	coord, err := git.PlainCloneContext(e.ctx, e.dir("coordinator"), false, &git.CloneOptions{URL: e.url, CABundle: e.hk.CAPEM})
	if err != nil {
		t.Fatalf("coordinator clone: %v", err)
	}
	if h, _ := coord.Head(); h.Hash() != e.c0 {
		t.Fatalf("coordinator main = %s", h.Hash())
	}
	e.facts["c2"] = e.c2.String()
}

func (e *scenarioEnv) guardUnchanged(f func()) {
	e.t.Helper()
	before := refSnapshot(e.t, e.bare)
	f()
	after := refSnapshot(e.t, e.bare)
	if !sameRefs(before, after) {
		e.t.Fatalf("refs changed:\nbefore %v\nafter  %v", before, after)
	}
	// Also through the live (not reopened) store.
	live := map[string]string{}
	iter, _ := e.h.store.IterReferences()
	iter.ForEach(func(r *plumbing.Reference) error {
		if r.Type() == plumbing.HashReference {
			live[string(r.Name())] = r.Hash().String()
		}
		return nil
	})
	for k, v := range live {
		if after[k] != v {
			e.t.Fatalf("live store ref %s = %s, reopened = %s", k, v, after[k])
		}
	}
}

func scenarioWrongCA(e *scenarioEnv) {
	t := e.t
	other, err := testkit.NewFixtureCA()
	if err != nil {
		t.Fatal(err)
	}
	calls := e.h.callCount()
	e.guardUnchanged(func() {
		_, err = git.PlainCloneContext(e.ctx, e.dir("wrong-ca"), false, &git.CloneOptions{URL: e.url, CABundle: other.CertPEM})
	})
	var ua x509.UnknownAuthorityError
	var cv *tls.CertificateVerificationError
	if err == nil || !(errors.As(err, &ua) || errors.As(err, &cv)) {
		t.Fatalf("wrong CA error = %v", err)
	}
	if e.h.callCount() != calls {
		t.Fatal("handler invoked despite TLS failure")
	}
}

// sanMismatchServer is a second TLS endpoint for the spike's git handler on
// sanMismatchHost. Its leaf is issued by the harness's own fixture CA, so the
// chain verifies, for names that exclude sanMismatchHost, so the client's
// hostname check is the only one that can fail. It owns and joins its listener,
// connections and serve goroutine.
type sanMismatchServer struct {
	srv      *http.Server
	addr     string
	mu       sync.Mutex
	accepted int
	wg       sync.WaitGroup
}

// The SAN-mismatch leaf's names: a reserved DNS name and a TEST-NET-1 address
// (RFC 5737), neither of which is sanMismatchHost.
var (
	sanMismatchDNSNames = []string{"san-mismatch.invalid"}
	sanMismatchIPs      = []net.IP{net.ParseIP("192.0.2.1")}
)

func startSANMismatchServer(t *testing.T, ca *testkit.FixtureCA, h http.Handler) *sanMismatchServer {
	t.Helper()
	leaf, err := ca.Leaf(sanMismatchDNSNames, sanMismatchIPs)
	if err != nil {
		t.Fatal(err)
	}
	opts := x509.VerifyOptions{Roots: ca.Pool(), DNSName: sanMismatchDNSNames[0]}
	if _, err := leaf.Leaf.Verify(opts); err != nil {
		t.Fatalf("SAN-mismatch leaf does not chain to the harness CA: %v", err)
	}
	opts.DNSName = sanMismatchHost
	var he x509.HostnameError
	if _, err := leaf.Leaf.Verify(opts); !errors.As(err, &he) {
		t.Fatalf("SAN-mismatch leaf must fail on hostname alone for %s: %v", sanMismatchHost, err)
	}
	ln, err := net.Listen("tcp", net.JoinHostPort(sanMismatchHost, "0"))
	if err != nil {
		t.Fatalf("listen on %s for the SAN-mismatch test: %v", sanMismatchHost, err)
	}
	mux := http.NewServeMux()
	mux.Handle(testkit.GitPrefix, h)
	s := &sanMismatchServer{addr: ln.Addr().String()}
	tlsConf := &tls.Config{Certificates: []tls.Certificate{leaf}, MinVersion: tls.VersionTLS12, NextProtos: []string{"http/1.1"}}
	s.srv = &http.Server{
		Handler:           mux,
		ErrorLog:          log.New(io.Discard, "", 0),
		ReadHeaderTimeout: testkit.HandshakeTimeout,
		ConnState: func(_ net.Conn, st http.ConnState) {
			if st == http.StateNew {
				s.mu.Lock()
				s.accepted++
				s.mu.Unlock()
			}
		},
	}
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		s.srv.Serve(tls.NewListener(ln, tlsConf))
	}()
	t.Cleanup(func() {
		if err := s.close(testkit.JoinTimeout); err != nil {
			t.Error(err)
		}
	})
	return s
}

func (s *sanMismatchServer) acceptedConns() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.accepted
}

// close shuts the server down, waiting within limit for every connection to
// end and for the serve goroutine to return.
func (s *sanMismatchServer) close(limit time.Duration) error {
	ctx, cancel := context.WithTimeout(context.Background(), limit)
	defer cancel()
	err := s.srv.Shutdown(ctx)
	if err != nil {
		s.srv.Close()
	}
	s.wg.Wait()
	if err != nil {
		return fmt.Errorf("SAN-mismatch server did not shut down within limit: %w", err)
	}
	return nil
}

// sanMismatchHost is where the SAN-mismatch case listens and dials. It must be
// 127.0.0.1: macOS lo0 carries no other 127/8 address unless one is aliased.
const sanMismatchHost = "127.0.0.1"

// fatalHelper is the part of *testing.T the SAN-mismatch oracle uses, so a
// recording double can observe its failures.
type fatalHelper interface {
	Helper()
	Fatalf(format string, args ...any)
}

// requireSANMismatchHost fails unless addr is a listener on the literal
// 127.0.0.1. The expected address is deliberately not sanMismatchHost:
// changing that constant must not move the oracle.
func requireSANMismatchHost(t fatalHelper, addr string) {
	t.Helper()
	host, _, err := net.SplitHostPort(addr)
	if err != nil || host != "127.0.0.1" {
		t.Fatalf("SAN-mismatch listener %q is not on 127.0.0.1, the only loopback address bindable on every supported OS (%v)", addr, err)
	}
}

func scenarioSANMismatch(e *scenarioEnv) {
	t := e.t
	srv := startSANMismatchServer(t, e.hk.CA(), e.h)
	requireSANMismatchHost(t, srv.addr)
	sanURL := "https://" + srv.addr + RepoURLPath
	calls := e.h.callCount()
	var err error
	e.guardUnchanged(func() {
		_, err = git.PlainCloneContext(e.ctx, e.dir("san"), false, &git.CloneOptions{URL: sanURL, CABundle: e.hk.CAPEM})
	})
	var he x509.HostnameError
	if !errors.As(err, &he) {
		t.Fatalf("SAN mismatch error does not unwrap to x509.HostnameError: %T %v", err, err)
	}
	if he.Host != sanMismatchHost {
		t.Fatalf("hostname error host = %q", he.Host)
	}
	if srv.acceptedConns() == 0 {
		t.Fatal("connection never reached the SAN-mismatch server")
	}
	if e.h.callCount() != calls {
		t.Fatal("handler invoked despite SAN mismatch")
	}
	e.facts["san_error"] = he.Error()
}

func cmd(name string, old, new plumbing.Hash) *packp.Command {
	return &packp.Command{Name: plumbing.ReferenceName(name), Old: old, New: new}
}

func (e *scenarioEnv) raw(cmds []*packp.Command, pack []byte, caps ...capability.Capability) rpcResult {
	return rawReceive(e.t, e.ctx, e.hk.Client, e.url, cmds, pack, caps...)
}

func (e *scenarioEnv) requireRejected(res rpcResult, wantMsg string) {
	e.t.Helper()
	if res.ok() {
		e.t.Fatalf("request succeeded: %s", res)
	}
	if res.Status == http.StatusOK && res.Report == nil {
		e.t.Fatalf("HTTP 200 without a decodable failure report: %s", res)
	}
	if wantMsg != "" && !strings.Contains(res.String(), wantMsg) {
		e.t.Fatalf("rejection %s does not mention %q", res, wantMsg)
	}
}

func scenarioStaleOld(e *scenarioEnv) {
	t := e.t
	ms := memory.NewStorage()
	// probe = C1 via a valid raw create whose objects are already durable.
	res := e.raw([]*packp.Command{cmd("refs/callsheet/tasks/probe", plumbing.ZeroHash, e.c1)}, emptyPack(t, ms))
	if !res.ok() {
		t.Fatalf("valid raw create failed: %s", res)
	}
	e.requireRefs(map[string]plumbing.Hash{"refs/callsheet/tasks/probe": e.c1})
	e.guardUnchanged(func() {
		// The unguarded probe's exact request: old=C0/new=C0 while current=C1.
		e.requireRejected(e.raw([]*packp.Command{cmd("refs/callsheet/tasks/probe", e.c0, e.c0)}, emptyPack(t, ms)), "stale old ref")
		// current == new is no permission to ignore a stale old.
		e.requireRejected(e.raw([]*packp.Command{cmd("refs/callsheet/tasks/probe", e.c0, e.c1)}, emptyPack(t, ms)), "stale old ref")
		// Create-only on an existing ref, update of an absent ref.
		e.requireRejected(e.raw([]*packp.Command{cmd("refs/callsheet/tasks/probe", plumbing.ZeroHash, e.c2)}, emptyPack(t, ms)), "stale old ref")
		e.requireRejected(e.raw([]*packp.Command{cmd("refs/callsheet/tasks/absent", e.c1, e.c2)}, emptyPack(t, ms)), "stale old ref")
		// Stale update of existing t1 (current C2).
		e.requireRejected(e.raw([]*packp.Command{cmd("refs/callsheet/tasks/t1", e.c1, e.c0)}, emptyPack(t, ms)), "stale old ref")
	})
}

func scenarioRace(e *scenarioEnv) {
	t := e.t
	ms := memory.NewStorage()
	d1, _ := writeCommit(ms, e.c1tree, []plumbing.Hash{e.c1}, "race one", fixedWhen.Add(3*time.Hour))
	d2, _ := writeCommit(ms, e.c1tree, []plumbing.Hash{e.c1}, "race two", fixedWhen.Add(3*time.Hour))
	packs := map[plumbing.Hash][]byte{d1: packOf(t, ms, d1), d2: packOf(t, ms, d2)}
	start := make(chan struct{})
	type out struct {
		h   plumbing.Hash
		res rpcResult
		err error
	}
	results := make(chan out, 2)
	for _, h := range []plumbing.Hash{d1, d2} {
		go func(h plumbing.Hash) {
			<-start
			body := encodeRequest(t, []*packp.Command{cmd("refs/callsheet/tasks/probe", e.c1, h)}, []capability.Capability{capability.ReportStatus}, packs[h])
			res, err := postReceive(e.ctx, e.hk.Client, e.url, bytes.NewReader(body))
			results <- out{h, res, err}
		}(h)
	}
	close(start)
	var winners, losers []out
	for range 2 {
		o := <-results
		if o.err != nil {
			t.Fatalf("race request: %v", o.err)
		}
		if o.res.ok() {
			winners = append(winners, o)
		} else {
			losers = append(losers, o)
		}
	}
	if len(winners) != 1 || len(losers) != 1 {
		t.Fatalf("race: %d winners, %d losers", len(winners), len(losers))
	}
	e.requireRejected(losers[0].res, "stale old ref")
	e.requireRefs(map[string]plumbing.Hash{"refs/callsheet/tasks/probe": winners[0].h})
	e.facts["race_winner"] = winners[0].h.String()
}

func scenarioAbsentObject(e *scenarioEnv) {
	t := e.t
	ghost := plumbing.NewHash("1234567890123456789012345678901234567890")
	e.guardUnchanged(func() {
		e.requireRejected(e.raw([]*packp.Command{cmd("refs/callsheet/tasks/ghost", plumbing.ZeroHash, ghost)}, emptyPack(t, memory.NewStorage())), "incomplete object closure")
	})
}

func scenarioMissingBlob(e *scenarioEnv) {
	t := e.t
	ms := memory.NewStorage()
	missing := plumbing.ComputeHash(plumbing.BlobObject, []byte("never sent\n"))
	tree, _ := writeObject(ms, &object.Tree{Entries: []object.TreeEntry{{Name: "x.txt", Mode: filemode.Regular, Hash: missing}}})
	c, _ := writeCommit(ms, tree, []plumbing.Hash{e.c0}, "missing blob", fixedWhen)
	e.guardUnchanged(func() {
		e.requireRejected(e.raw([]*packp.Command{cmd("refs/callsheet/tasks/noblob", plumbing.ZeroHash, c)}, packOf(t, ms, c, tree)), "incomplete object closure")
	})
}

func scenarioMissingParentTree(e *scenarioEnv) {
	t := e.t
	ms := memory.NewStorage()
	noParent, _ := writeCommit(ms, e.c0tree, []plumbing.Hash{plumbing.NewHash("abababababababababababababababababababab")}, "missing parent", fixedWhen)
	noTree, _ := writeCommit(ms, plumbing.NewHash("cdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcd"), []plumbing.Hash{e.c0}, "missing tree", fixedWhen)
	blobParent, _ := writeCommit(ms, e.c0tree, []plumbing.Hash{treeFileHash(t, e, "README.md")}, "blob parent", fixedWhen)
	e.guardUnchanged(func() {
		e.requireRejected(e.raw([]*packp.Command{cmd("refs/callsheet/tasks/noparent", plumbing.ZeroHash, noParent)}, packOf(t, ms, noParent)), "incomplete object closure")
		e.requireRejected(e.raw([]*packp.Command{cmd("refs/callsheet/tasks/notree", plumbing.ZeroHash, noTree)}, packOf(t, ms, noTree)), "incomplete object closure")
		e.requireRejected(e.raw([]*packp.Command{cmd("refs/callsheet/tasks/blobparent", plumbing.ZeroHash, blobParent)}, packOf(t, ms, blobParent)), "incomplete object closure")
	})
}

func treeFileHash(t *testing.T, e *scenarioEnv, name string) plumbing.Hash {
	tr, err := object.GetTree(e.h.store, e.c0tree)
	if err != nil {
		t.Fatal(err)
	}
	en, err := tr.FindEntry(name)
	if err != nil {
		t.Fatal(err)
	}
	return en.Hash
}

func scenarioBlobAsNew(e *scenarioEnv) {
	t := e.t
	ms := memory.NewStorage()
	b, _ := writeRaw(ms, plumbing.BlobObject, []byte("a blob, not a commit\n"))
	e.guardUnchanged(func() {
		e.requireRejected(e.raw([]*packp.Command{cmd("refs/callsheet/tasks/blob", plumbing.ZeroHash, b)}, packOf(t, ms, b)), "new object is not a commit")
	})
}

// newCommitPack builds a commit with one new blob on top of parent.
func newCommitPack(t *testing.T, parent plumbing.Hash, label string) (plumbing.Hash, []byte) {
	ms := memory.NewStorage()
	tree, err := writeTree(ms, map[string]fileSpec{label + ".txt": {filemode.Regular, []byte("content for " + label + "\n")}})
	if err != nil {
		t.Fatal(err)
	}
	c, _ := writeCommit(ms, tree, []plumbing.Hash{parent}, label, fixedWhen)
	blobIter, _ := object.GetTree(ms, tree)
	return c, packOf(t, ms, c, tree, blobIter.Entries[0].Hash)
}

func scenarioTruncated(e *scenarioEnv) {
	t := e.t
	c, pack := newCommitPack(t, e.c0, "truncated")
	e.guardUnchanged(func() {
		res := e.raw([]*packp.Command{cmd("refs/callsheet/tasks/trunc", plumbing.ZeroHash, c)}, pack[:len(pack)/2])
		e.requireRejected(res, "")
		if res.Report != nil && res.Report.UnpackStatus == "ok" {
			t.Fatalf("truncated pack reported unpack ok: %s", res)
		}
	})
	if _, ok := refSnapshot(t, e.bare)["refs/callsheet/tasks/trunc"]; ok {
		t.Fatal("truncated push published a ref")
	}
}

func scenarioInvalidRequests(e *scenarioEnv) {
	t := e.t
	ms := memory.NewStorage()
	e.guardUnchanged(func() {
		for _, name := range []string{"refs/other/x", "refs/tags/v1", "HEAD", "refs/heads/a..b", "refs/callsheet/tasks/", "refs/heads/x.lock", "refs/heads/bad\x01name", "refs/callsheet/tasks/t1/sub"} {
			res := e.raw([]*packp.Command{cmd(name, plumbing.ZeroHash, e.c1)}, emptyPack(t, ms))
			if res.ok() {
				t.Fatalf("invalid ref %q accepted", name)
			}
		}
		e.requireRejected(e.raw([]*packp.Command{cmd("refs/callsheet/tasks/t1", e.c2, plumbing.ZeroHash)}, emptyPack(t, ms)), "deletes are not supported")
		multi := e.raw([]*packp.Command{
			cmd("refs/callsheet/tasks/m1", plumbing.ZeroHash, e.c1),
			cmd("refs/callsheet/tasks/m2", plumbing.ZeroHash, e.c1),
		}, emptyPack(t, ms))
		if multi.Status != http.StatusBadRequest {
			t.Fatalf("multiple commands = %s", multi)
		}
	})
	// Advertisement omits delete-refs and anything outside the allowlist.
	req, _ := http.NewRequestWithContext(e.ctx, http.MethodGet, e.url+"/info/refs?service=git-receive-pack", nil)
	resp, err := e.hk.Client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	ar := packp.NewAdvRefs()
	if err := ar.Decode(resp.Body); err != nil {
		t.Fatalf("decode advertisement: %v", err)
	}
	if ar.Capabilities.Supports(capability.DeleteRefs) {
		t.Fatal("delete-refs advertised")
	}
	for _, c := range ar.Capabilities.All() {
		if !receiveAllow[c] {
			t.Fatalf("capability %s advertised", c)
		}
	}
	if !ar.Capabilities.Supports(capability.ReportStatus) {
		t.Fatal("report-status not advertised")
	}
	e.facts["receive_capabilities"] = ar.Capabilities.String()
}

func scenarioCapabilities(e *scenarioEnv) {
	t := e.t
	ms := memory.NewStorage()
	e.guardUnchanged(func() {
		for _, c := range []capability.Capability{capability.Atomic, capability.DeleteRefs, capability.PushOptions, capability.Sideband64k, capability.ThinPack} {
			e.requireRejected(e.raw([]*packp.Command{cmd("refs/callsheet/tasks/caps", plumbing.ZeroHash, e.c1)}, emptyPack(t, ms), c), "unsupported capability")
		}
		body := encodeRequest(t, []*packp.Command{cmd("refs/callsheet/tasks/caps", plumbing.ZeroHash, e.c1)}, []capability.Capability{capability.OFSDelta}, emptyPack(t, ms))
		res, err := postReceive(e.ctx, e.hk.Client, e.url, bytes.NewReader(body))
		if err != nil || res.Status != http.StatusBadRequest {
			t.Fatalf("missing report-status = %s %v", res, err)
		}
	})
}

// stallReader blocks until its context ends.
type stallReader struct{ ctx context.Context }

func (s stallReader) Read([]byte) (int, error) {
	<-s.ctx.Done()
	return 0, s.ctx.Err()
}

func scenarioStalledBody(e *scenarioEnv) {
	t := e.t
	c, pack := newCommitPack(t, e.c0, "stalled")
	entered := make(chan struct{}, 1)
	finished := make(chan struct{}, 1)
	e.h.hooks.packRead = func() {
		select {
		case entered <- struct{}{}:
		default:
		}
	}
	e.h.hooks.done = func(p string) {
		if p == receivePackPath {
			select {
			case finished <- struct{}{}:
			default:
			}
		}
	}
	defer func() { e.h.hooks = hooks{} }()
	e.guardUnchanged(func() {
		header := encodeRequest(t, []*packp.Command{cmd("refs/callsheet/tasks/stalled", plumbing.ZeroHash, c)}, []capability.Capability{capability.ReportStatus}, nil)
		cctx, ccancel := context.WithCancel(e.ctx)
		defer ccancel()
		body := io.MultiReader(bytes.NewReader(header), bytes.NewReader(pack[:12]), stallReader{cctx})
		errCh := make(chan error, 1)
		go func() {
			_, err := postReceive(cctx, e.hk.Client, e.url, body)
			errCh <- err
		}()
		select {
		case <-entered:
		case <-time.After(10 * time.Second):
			t.Fatal("handler never entered the pack read")
		}
		ccancel()
		if err := <-errCh; err == nil {
			t.Fatal("cancelled transfer returned success")
		}
		select {
		case <-finished:
		case <-time.After(10 * time.Second):
			t.Fatal("handler did not finish after cancellation")
		}
	})
	if _, ok := refSnapshot(t, e.bare)["refs/callsheet/tasks/stalled"]; ok {
		t.Fatal("stalled transfer published a ref")
	}
}

func scenarioCancelBeforePublish(e *scenarioEnv) {
	t := e.t
	c, pack := newCommitPack(t, e.c0, "cancelled")
	called := false
	e.h.hooks.afterValidate = func(cancel context.CancelFunc) {
		called = true
		cancel()
	}
	defer func() { e.h.hooks = hooks{} }()
	e.guardUnchanged(func() {
		e.requireRejected(e.raw([]*packp.Command{cmd("refs/callsheet/tasks/cancelled", plumbing.ZeroHash, c)}, pack), "cancelled before publication")
	})
	if !called {
		t.Fatal("cancellation hook not reached after validation")
	}
}

func scenarioPromotionFailure(e *scenarioEnv) {
	t := e.t
	c, pack := newCommitPack(t, e.c0, "promotion")
	var n int
	e.h.hooks.promote = func(plumbing.Hash) error {
		n++
		if n == 2 {
			return errors.New("injected promotion failure")
		}
		return nil
	}
	e.guardUnchanged(func() {
		e.requireRejected(e.raw([]*packp.Command{cmd("refs/callsheet/tasks/promo", plumbing.ZeroHash, c)}, pack), "object promotion failed")
	})
	e.h.hooks = hooks{}
	if n < 2 {
		t.Fatalf("promotion hook ran %d times", n)
	}
	// Recovery: a subsequent valid push and observer fetch succeed.
	c3 := e.commitWorker("C3 after failure", fixedWhen.Add(4*time.Hour), func(dir string, wt *git.Worktree) {
		mustWrite(t, filepath.Join(dir, "recovered.txt"), "recovered\n")
		if _, err := wt.Add("recovered.txt"); err != nil {
			t.Fatal(err)
		}
	})
	e.push("refs/heads/main:refs/callsheet/tasks/t2")
	obs, got := e.fetchObserver(nil, "refs/callsheet/tasks/t2")
	if got != c3 {
		t.Fatalf("observer t2 = %s, want %s", got, c3)
	}
	oc, _ := obs.CommitObject(got)
	files := treeFiles(t, obs.Storer, oc.TreeHash)
	if string(files["recovered.txt"].Content) != "recovered\n" {
		t.Fatal("recovered content mismatch")
	}
	e.requireRefs(map[string]plumbing.Hash{"refs/heads/main": e.c0, "refs/callsheet/tasks/t1": e.c2, "refs/callsheet/tasks/t2": c3})
}
