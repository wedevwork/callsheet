package client

import (
	"bytes"
	"context"
	"crypto/sha1"
	"crypto/x509"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/format/pktline"
	"github.com/go-git/go-git/v5/plumbing/protocol/packp"
	"github.com/go-git/go-git/v5/plumbing/protocol/packp/capability"

	"github.com/wedevwork/callsheet/internal/contract"
	"github.com/wedevwork/callsheet/internal/testkit"
)

// Iteration 09b (UT-1): the workspace git session: framing, report and
// status mapping, verified transport posture, and the event-armed
// no-progress watchdog. The production receive handler's CAS proofs are
// in internal/workspacetransfer.

const (
	gInst = "0123456789abcdef0123456789abcdef"
	gHash = "0123456789abcdef0123456789abcdef01234567"
)

var gitWait = 20 * time.Second

// gitServer serves /ws/<name>.git/ with handler h over verified TLS and
// returns a session of workspace "ws".
func gitSession(t *testing.T, h http.HandlerFunc) (*WorkspaceGit, *server) {
	t.Helper()
	ca := newTestCA(t)
	s := startServer(t, ca.leaf(t, false), h)
	c, err := New(s.url, Trust{CAPEM: ca.pem})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(c.Close)
	g, err := c.WorkspaceGit("ws")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(g.Close)
	return g, s
}

// advert encodes an advertisement of refs for service.
func advert(t *testing.T, service string, refs map[string]string) []byte {
	t.Helper()
	ar := packp.NewAdvRefs()
	ar.Capabilities.Set(capability.ReportStatus)
	for n, h := range refs {
		ar.References[n] = plumbing.NewHash(h)
	}
	ar.Prefix = [][]byte{[]byte("# service=" + service), pktline.Flush}
	var b bytes.Buffer
	if err := ar.Encode(&b); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

// emptyPack is a valid pack with no objects.
func emptyPack() []byte {
	b := []byte("PACK\x00\x00\x00\x02\x00\x00\x00\x00")
	sum := sha1.Sum(b)
	return append(b, sum[:]...)
}

func report(unpack, ref, status string) []byte {
	rs := packp.NewReportStatus()
	rs.UnpackStatus = unpack
	rs.CommandStatuses = []*packp.CommandStatus{{ReferenceName: plumbing.ReferenceName(ref), Status: status}}
	var b bytes.Buffer
	rs.Encode(&b)
	return b.Bytes()
}

func TestWorkspaceGitAdvertisement(t *testing.T) {
	var mu sync.Mutex
	var gotInst, gotQuery, gotProto string
	body := advert(t, "git-receive-pack", map[string]string{"refs/heads/main": gHash})
	g, _ := gitSession(t, func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		gotInst, gotQuery, gotProto = r.Header.Get(contract.WorkspaceInstanceHeader), r.URL.RawQuery, r.Proto
		mu.Unlock()
		if r.URL.Path != "/ws/ws.git/info/refs" {
			w.WriteHeader(404)
			return
		}
		w.Write(body)
	})
	ctx := context.Background()
	refs, err := g.ReceiveRefs(ctx, gInst)
	if err != nil || refs["refs/heads/main"] != plumbing.NewHash(gHash) || len(refs) != 1 {
		t.Fatalf("%v %v", refs, err)
	}
	mu.Lock()
	if gotInst != gInst || gotQuery != "service=git-receive-pack" || gotProto != "HTTP/1.1" {
		t.Fatalf("request %q %q %q", gotInst, gotQuery, gotProto)
	}
	mu.Unlock()
	if _, err := g.ReceiveRefs(ctx, "bad"); codeOf(err) != contract.CodeInvalidArgument {
		t.Fatalf("bad instance %v", err)
	}
	if _, err := g.UploadRefs(ctx); err != nil {
		t.Fatalf("upload refs %v", err)
	}
	if _, err := (&Client{}).WorkspaceGit("Bad"); codeOf(err) != contract.CodeInvalidArgument {
		t.Fatalf("bad name %v", err)
	}
}

func TestWorkspaceGitAdvertisementAnswers(t *testing.T) {
	var mu sync.Mutex
	var answer func(w http.ResponseWriter)
	g, _ := gitSession(t, func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		a := answer
		mu.Unlock()
		a(w)
	})
	set := func(f func(w http.ResponseWriter)) {
		mu.Lock()
		answer = f
		mu.Unlock()
	}
	ctx := context.Background()
	// An empty repository advertises no refs.
	set(func(w http.ResponseWriter) {
		ar := packp.NewAdvRefs()
		ar.Capabilities.Set(capability.ReportStatus)
		ar.Prefix = [][]byte{[]byte("# service=git-upload-pack"), pktline.Flush}
		ar.Encode(w)
	})
	if refs, err := g.UploadRefs(ctx); err != nil || len(refs) != 0 {
		t.Fatalf("empty %v %v", refs, err)
	}
	set(func(w http.ResponseWriter) { w.Write(pktline.FlushPkt) })
	if refs, err := g.UploadRefs(ctx); err != nil || len(refs) != 0 {
		t.Fatalf("flush only %v %v", refs, err)
	}
	set(func(w http.ResponseWriter) {
		w.Write(advert(t, "git-upload-pack", map[string]string{"refs/tags/v1": gHash}))
	})
	if _, err := g.UploadRefs(ctx); codeOf(err) != contract.CodeInternal {
		t.Fatalf("tag ref %v", err)
	}
	set(func(w http.ResponseWriter) { io.WriteString(w, "garbage") })
	if _, err := g.UploadRefs(ctx); codeOf(err) != contract.CodeInternal {
		t.Fatalf("garbage %v", err)
	}
	for status, code := range map[int]contract.Code{404: contract.CodeNotFound, 409: contract.CodeConflict, 400: contract.CodeInvalidArgument,
		503: contract.CodeUnavailable, 505: contract.CodeProtocolMismatch, 500: contract.CodeInternal} {
		set(func(w http.ResponseWriter) {
			w.WriteHeader(status)
			io.WriteString(w, "SENTINEL plane text")
		})
		_, err := g.ReceiveRefs(ctx, gInst)
		if codeOf(err) != code || strings.Contains(err.Error(), "SENTINEL") {
			t.Fatalf("%d: %v", status, err)
		}
	}
	// A redirect is never followed.
	set(func(w http.ResponseWriter) {
		w.Header().Set("Location", "https://example.invalid/")
		w.WriteHeader(302)
	})
	if _, err := g.UploadRefs(ctx); codeOf(err) != contract.CodeInvalidArgument || !strings.Contains(err.Error(), "redirect") {
		t.Fatalf("redirect %v", err)
	}
}

// Push frames exactly one command with report-status and the instance
// header, streams the pack and maps every report to its contract error.
func TestWorkspaceGitPush(t *testing.T) {
	var mu sync.Mutex
	var reply []byte
	var got []byte
	var inst string
	g, _ := gitSession(t, func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		mu.Lock()
		got, inst = b, r.Header.Get(contract.WorkspaceInstanceHeader)
		rep := reply
		mu.Unlock()
		if r.URL.Path != "/ws/ws.git/git-receive-pack" || r.Method != http.MethodPost || r.Header.Get("Content-Type") != "application/x-git-receive-pack-request" {
			w.WriteHeader(400)
			return
		}
		w.Write(rep)
	})
	set := func(b []byte) {
		mu.Lock()
		reply = b
		mu.Unlock()
	}
	ctx := context.Background()
	ref := "refs/heads/main"
	set(report("ok", ref, "ok"))
	if err := g.Push(ctx, gInst, ref, plumbing.ZeroHash, plumbing.NewHash(gHash), bytes.NewReader(emptyPack())); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	req := packp.NewReferenceUpdateRequest()
	if err := req.Decode(bytes.NewReader(got)); err != nil || len(req.Commands) != 1 || req.Commands[0].Name != plumbing.ReferenceName(ref) ||
		!req.Commands[0].Old.IsZero() || req.Commands[0].New.String() != gHash || !req.Capabilities.Supports(capability.ReportStatus) ||
		len(req.Capabilities.All()) != 1 || inst != gInst {
		t.Fatalf("request %v %+v %q", err, req.Commands, inst)
	}
	pack, _ := io.ReadAll(req.Packfile)
	mu.Unlock()
	if !bytes.Equal(pack, emptyPack()) {
		t.Fatalf("pack %x", pack)
	}
	for _, c := range []struct {
		rep  []byte
		code contract.Code
		text string
	}{
		{report("ok", ref, "stale old ref"), contract.CodeConflict, "compare-and-swap"},
		{report("ok", ref, "ref namespace conflict"), contract.CodeConflict, "namespace"},
		{report("not attempted", ref, "invalid ref name"), contract.CodeInvalidArgument, "branch name"},
		{report("unpack failed", ref, "cancelled before publication"), contract.CodeUnavailable, "nothing changed"},
		{report("unpack failed", ref, "incomplete object closure"), contract.CodeInternal, "incomplete"},
		{report("ok", ref, "storage failure"), contract.CodeInternal, "may have succeeded"},
		{report("ok", ref, "SENTINEL odd"), contract.CodeInternal, "unknown status"},
		{report("ok", "refs/heads/other", "ok"), contract.CodeInternal, "may have succeeded"},
		{report("SENTINEL", ref, "ok"), contract.CodeInternal, "unknown status"},
		{[]byte("0010garbage"), contract.CodeInternal, "malformed"},
	} {
		set(c.rep)
		err := g.Push(ctx, gInst, ref, plumbing.ZeroHash, plumbing.NewHash(gHash), bytes.NewReader(emptyPack()))
		if codeOf(err) != c.code || !strings.Contains(err.Error(), c.text) || strings.Contains(err.Error(), "SENTINEL") {
			t.Fatalf("%q: %v", c.rep, err)
		}
	}
	for _, bad := range []struct {
		inst, ref string
		new       plumbing.Hash
	}{{"x", ref, plumbing.NewHash(gHash)}, {gInst, "refs/tags/x", plumbing.NewHash(gHash)}, {gInst, ref, plumbing.ZeroHash}} {
		if err := g.Push(ctx, bad.inst, bad.ref, plumbing.ZeroHash, bad.new, bytes.NewReader(nil)); codeOf(err) != contract.CodeInvalidArgument {
			t.Fatalf("%+v: %v", bad, err)
		}
	}
}

// A lost push answer (the connection closes after the request) is
// unavailable and says the push may have succeeded; the caller's
// cancellation is returned unchanged.
func TestWorkspaceGitPushLost(t *testing.T) {
	g, _ := gitSession(t, func(w http.ResponseWriter, r *http.Request) {
		io.ReadAll(r.Body)
		hj, _ := w.(http.Hijacker)
		c, _, err := hj.Hijack()
		if err == nil {
			c.Close()
		}
	})
	err := g.Push(context.Background(), gInst, "refs/heads/main", plumbing.ZeroHash, plumbing.NewHash(gHash), bytes.NewReader(emptyPack()))
	if codeOf(err) != contract.CodeUnavailable || !strings.Contains(err.Error(), "may have succeeded") {
		t.Fatalf("lost %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := g.Push(ctx, gInst, "refs/heads/main", plumbing.ZeroHash, plumbing.NewHash(gHash), bytes.NewReader(emptyPack())); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled %v", err)
	}
}

// Fetch sends wants, haves and done, decodes NAK and passes the pack to
// the sink; the trailing checksum is verified; a refused want is a
// conflict.
func TestWorkspaceGitFetch(t *testing.T) {
	var mu sync.Mutex
	var reply []byte
	var status = 200
	var got []byte
	g, _ := gitSession(t, func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		mu.Lock()
		got = b
		rep, st := reply, status
		mu.Unlock()
		w.WriteHeader(st)
		w.Write(rep)
	})
	set := func(b []byte, st int) {
		mu.Lock()
		reply, status = b, st
		mu.Unlock()
	}
	ctx := context.Background()
	nak := []byte("0008NAK\n")
	set(append(append([]byte{}, nak...), emptyPack()...), 200)
	var sunk []byte
	err := g.Fetch(ctx, []plumbing.Hash{plumbing.NewHash(gHash)}, []plumbing.Hash{plumbing.NewHash(strings.Repeat("1", 40))}, func(r io.Reader) error {
		sunk, _ = io.ReadAll(r)
		return nil
	})
	if err != nil || !bytes.Equal(sunk, emptyPack()) {
		t.Fatalf("fetch %v %x", err, sunk)
	}
	mu.Lock()
	body := string(got)
	mu.Unlock()
	if !strings.Contains(body, "want "+gHash+" ofs-delta") || !strings.Contains(body, "have "+strings.Repeat("1", 40)) || !strings.HasSuffix(body, "0009done\n") {
		t.Fatalf("request %q", body)
	}
	bad := emptyPack()
	bad[len(bad)-1] ^= 1
	set(append(append([]byte{}, nak...), bad...), 200)
	if err := g.Fetch(ctx, []plumbing.Hash{plumbing.NewHash(gHash)}, nil, func(r io.Reader) error { io.Copy(io.Discard, r); return nil }); codeOf(err) != contract.CodeInternal {
		t.Fatalf("checksum %v", err)
	}
	set(append(append([]byte{}, nak...), emptyPack()...), 200)
	sinkErr := contract.New(contract.CodeInternal, "sink failed")
	if err := g.Fetch(ctx, []plumbing.Hash{plumbing.NewHash(gHash)}, nil, func(io.Reader) error { return sinkErr }); err != sinkErr {
		t.Fatalf("sink %v", err)
	}
	set([]byte("zzzz"), 200)
	if err := g.Fetch(ctx, []plumbing.Hash{plumbing.NewHash(gHash)}, nil, func(io.Reader) error { return nil }); codeOf(err) != contract.CodeInternal {
		t.Fatalf("no NAK %v", err)
	}
	set([]byte("a wanted object is not reachable"), 400)
	if err := g.Fetch(ctx, []plumbing.Hash{plumbing.NewHash(gHash)}, nil, func(io.Reader) error { return nil }); codeOf(err) != contract.CodeConflict {
		t.Fatalf("refused want %v", err)
	}
	if err := g.Fetch(ctx, nil, nil, func(io.Reader) error { return nil }); codeOf(err) != contract.CodeInvalidArgument {
		t.Fatalf("no wants %v", err)
	}
}

// The session keeps the verified posture: another CA, a certificate
// without the host's SAN and an ambient proxy never reach a git handler.
func TestWorkspaceGitTrust(t *testing.T) {
	t.Setenv("HTTPS_PROXY", "http://127.0.0.1:1")
	t.Setenv("https_proxy", "http://127.0.0.1:1")
	ca, other := newTestCA(t), newTestCA(t)
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write(advert(t, "git-upload-pack", map[string]string{"refs/heads/main": gHash}))
	})
	s := startServer(t, ca.leaf(t, false), h)
	c, err := New(s.url, Trust{CAPEM: other.pem})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	g, _ := c.WorkspaceGit("ws")
	defer g.Close()
	if _, err := g.UploadRefs(context.Background()); codeOf(err) != contract.CodeTrustFailed {
		t.Fatalf("other CA %v", err)
	}
	sanless := startServer(t, ca.leaf(t, false, func(c *x509.Certificate) { c.IPAddresses = nil }), h)
	c2, _ := New(sanless.url, Trust{CAPEM: ca.pem})
	defer c2.Close()
	g2, _ := c2.WorkspaceGit("ws")
	defer g2.Close()
	if _, err := g2.UploadRefs(context.Background()); codeOf(err) != contract.CodeTrustFailed {
		t.Fatalf("no SAN %v", err)
	}
	if s.hits.Load() != 0 || sanless.hits.Load() != 0 {
		t.Fatal("an untrusted server was reached")
	}
	// The ambient proxy is ignored: the good CA works directly.
	c3, _ := New(s.url, Trust{CAPEM: ca.pem})
	defer c3.Close()
	g3, _ := c3.WorkspaceGit("ws")
	defer g3.Close()
	if _, err := g3.UploadRefs(context.Background()); err != nil {
		t.Fatalf("direct %v", err)
	}
}

// hookLog records watchdog stages for event-armed waits.
type hookLog struct {
	mu   sync.Mutex
	cond *sync.Cond
	n    map[string]int
}

func newHookLog() *hookLog {
	h := &hookLog{n: map[string]int{}}
	h.cond = sync.NewCond(&h.mu)
	return h
}

func (h *hookLog) record(s string) {
	h.mu.Lock()
	h.n[s]++
	h.mu.Unlock()
	h.cond.Broadcast()
}

func (h *hookLog) count(s string) int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.n[s]
}

// await waits until stage s was recorded n times.
func (h *hookLog) await(t *testing.T, s string, n int) {
	t.Helper()
	done := make(chan struct{})
	go func() {
		h.mu.Lock()
		for h.n[s] < n {
			h.cond.Wait()
		}
		h.mu.Unlock()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(gitWait):
		t.Fatalf("stage %s x%d not observed", s, n)
	}
}

// The no-progress watchdog cancels a stream that makes no progress for
// the idle bound, measured from the last progress: a fired timer after
// recent progress re-arms for the remainder instead. Every advance of
// the fake clock follows the "armed" event of the timer it targets.
func TestWorkspaceGitNoProgress(t *testing.T) {
	more := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	g, _ := gitSession(t, func(w http.ResponseWriter, r *http.Request) {
		io.ReadAll(r.Body)
		w.Write([]byte("0008NAK\n"))
		w.(http.Flusher).Flush()
		select {
		case <-more:
		case <-r.Context().Done():
			return
		}
		// The pack header arrives after the clock moved: progress.
		w.Write([]byte("PACK\x00\x00\x00\x02"))
		w.(http.Flusher).Flush()
		select {
		case <-r.Context().Done():
		case <-release:
		}
	})
	defer once.Do(func() { close(release) })
	clock := testkit.NewFakeClock(time.Date(2026, 9, 30, 12, 0, 0, 0, time.UTC))
	hooks := newHookLog()
	g.clock, g.hook = clock, hooks.record
	// A stream with progress, then silence.
	errc := make(chan error, 1)
	readOne := make(chan struct{})
	go func() {
		errc <- g.Fetch(context.Background(), []plumbing.Hash{plumbing.NewHash(gHash)}, nil, func(r io.Reader) error {
			b := make([]byte, 1)
			if _, err := io.ReadFull(r, b); err != nil {
				return err
			}
			close(readOne)
			_, err := io.Copy(io.Discard, r)
			return err
		})
	}()
	hooks.await(t, "armed", 1)
	clock.Advance(20 * time.Second)
	close(more)
	select {
	case <-readOne:
	case <-time.After(gitWait):
		t.Fatal("progress byte not read")
	}
	// The first timer (armed at t0) fires at t0+30s, 10s after progress:
	// it re-arms for the remaining 20s instead of cancelling.
	clock.Advance(10 * time.Second)
	hooks.await(t, "armed", 2)
	if hooks.count("idle") != 0 {
		t.Fatal("cancelled despite recent progress")
	}
	clock.Advance(20 * time.Second)
	hooks.await(t, "idle", 1)
	var err error
	select {
	case err = <-errc:
	case <-time.After(gitWait):
		t.Fatal("fetch did not end")
	}
	if codeOf(err) != contract.CodeUnavailable || !strings.Contains(err.Error(), "no progress for 30s") {
		t.Fatalf("idle fetch %v", err)
	}
}

// A request that never gets an answer is cancelled after the idle bound
// and the watchdog is joined. The clock moves only after "armed" and after
// the whole request body was read: a body read after the advance would
// record progress at the fire instant and rightly re-arm the watchdog.
func TestWorkspaceGitSilent(t *testing.T) {
	release := make(chan struct{})
	bodyRead := make(chan struct{})
	var readOnce sync.Once
	g, _ := gitSession(t, func(w http.ResponseWriter, r *http.Request) {
		io.ReadAll(r.Body)
		readOnce.Do(func() { close(bodyRead) })
		select {
		case <-r.Context().Done():
		case <-release:
		}
	})
	defer close(release)
	clock := testkit.NewFakeClock(time.Date(2026, 9, 30, 12, 0, 0, 0, time.UTC))
	hooks := newHookLog()
	g.clock, g.hook = clock, hooks.record
	errc := make(chan error, 1)
	go func() {
		errc <- g.Push(context.Background(), gInst, "refs/heads/main", plumbing.ZeroHash, plumbing.NewHash(gHash), bytes.NewReader(emptyPack()))
	}()
	hooks.await(t, "armed", 1)
	select {
	case <-bodyRead:
	case <-time.After(gitWait):
		t.Fatal("request body not read")
	}
	clock.Advance(GitIdle)
	hooks.await(t, "idle", 1)
	var err error
	select {
	case err = <-errc:
	case <-time.After(gitWait):
		t.Fatal("push did not end")
	}
	if codeOf(err) != contract.CodeUnavailable || !strings.Contains(err.Error(), "may have succeeded") {
		t.Fatalf("silent push %v", err)
	}
	if n := len(clock.Waiters()); n != 0 {
		t.Fatalf("%d timers left", n)
	}
}

// Progress recorded at the very instant the timer fires leaves no idle
// time: the watchdog re-arms for a full GitIdle and does not cancel.
func TestWatchdogRearmWhenProgressIsAtTheFireInstant(t *testing.T) {
	start := time.Date(2026, 9, 30, 12, 0, 0, 0, time.UTC)
	clock := testkit.NewFakeClock(start)
	hooks := newHookLog()
	g := &WorkspaceGit{clock: clock, idle: GitIdle, hook: hooks.record}
	ctx, cancel := context.WithCancelCause(context.Background())
	defer cancel(nil)
	w := g.watch(cancel)
	hooks.await(t, "armed", 1)
	w.last.Store(start.Add(GitIdle).UnixNano())
	clock.Advance(GitIdle)
	hooks.await(t, "armed", 2)
	if n := hooks.count("idle"); n != 0 {
		t.Fatalf("idle staged %d times despite progress at the fire instant", n)
	}
	if cause := context.Cause(ctx); cause != nil {
		t.Fatalf("cancelled despite progress at the fire instant: %v", cause)
	}
	ws := clock.Waiters()
	if len(ws) != 1 || ws[0].Duration != GitIdle || !ws[0].At.Equal(start.Add(2*GitIdle)) {
		t.Fatalf("re-armed timers %+v, want one full GitIdle from the fire instant", ws)
	}
	w.close()
	if n := len(clock.Waiters()); n != 0 {
		t.Fatalf("%d timers left", n)
	}
}
