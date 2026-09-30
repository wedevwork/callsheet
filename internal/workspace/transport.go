package workspace

import (
	"bytes"
	"context"
	"crypto/sha1"
	"errors"
	"fmt"
	"hash"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/format/pktline"
	"github.com/go-git/go-git/v5/plumbing/protocol/packp"
	"github.com/go-git/go-git/v5/plumbing/protocol/packp/capability"
	"github.com/go-git/go-git/v5/plumbing/storer"
	"github.com/go-git/go-git/v5/plumbing/transport"
	"github.com/go-git/go-git/v5/plumbing/transport/server"

	"github.com/wedevwork/callsheet/internal/contract"
)

// Smart-HTTP routes below /ws/<name>.git/.
const (
	routeInfoRefs   = "info/refs"
	routeUpload     = "git-upload-pack"
	routeReceive    = "git-receive-pack"
	serviceUpload   = "git-upload-pack"
	serviceReceive  = "git-receive-pack"
	contentTypeRPC  = "application/x-%s-result"
	contentTypeAdv  = "application/x-%s-advertisement"
	endpointRepo    = "/repo.git"
	gitSuffix       = ".git/"
	maxInstanceHdrs = 1
)

// Safe report-status strings: fixed reasons, never library text.
const (
	unpackOK           = "ok"
	unpackNotAttempted = "not attempted"
	unpackFailed       = "unpack failed"
	reasonInvalidRef   = "invalid ref name"
	reasonStaleOld     = "stale old ref"
	reasonClosure      = "incomplete object closure"
	reasonCancelled    = "cancelled before publication"
	reasonStorage      = "storage failure"
	reasonConflict     = "ref namespace conflict"
)

// receiveAllow and uploadAllow are the complete advertised and accepted
// capability sets: no delete-refs, atomic, thin-pack, shallow or
// side-band.
var (
	receiveAllow = map[capability.Capability]bool{capability.ReportStatus: true, capability.OFSDelta: true, capability.Agent: true}
	uploadAllow  = map[capability.Capability]bool{capability.OFSDelta: true, capability.Agent: true}
)

var endpoint = func() *transport.Endpoint {
	ep, err := transport.NewEndpoint(endpointRepo)
	if err != nil {
		panic(err) // unreachable: a fixed path
	}
	return ep
}()

// libraryServer is a go-git server bound to exactly one storage.
func libraryServer(s storer.Storer) transport.Transport {
	return server.NewServer(server.MapLoader{endpoint.String(): s})
}

func plainError(w http.ResponseWriter, status int, msg string) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(status)
	io.WriteString(w, msg+"\n")
}

// ServeGit serves the smart-HTTP transport on the plane's TLS server:
// GET /ws/<name>.git/info/refs?service=git-upload-pack|git-receive-pack,
// POST /ws/<name>.git/git-upload-pack and POST /ws/<name>.git/git-receive-pack.
// Names resolve through the manager's handles, never by joining the URL
// into a path; encoded paths, traversal, extra components or queries,
// other methods and HTTP/2 are refused.
func (m *Manager) ServeGit(w http.ResponseWriter, r *http.Request) {
	if r.ProtoMajor != 1 {
		plainError(w, http.StatusHTTPVersionNotSupported, "the git transport is served over HTTP/1.1 only")
		return
	}
	if r.URL.RawPath != "" {
		plainError(w, http.StatusBadRequest, "encoded git paths are not accepted")
		return
	}
	rest, _ := strings.CutPrefix(r.URL.Path, contract.WorkspaceGitPrefix)
	name, route, ok := strings.Cut(rest, gitSuffix)
	if !ok || !contract.ValidWorkspaceName(name) {
		plainError(w, http.StatusBadRequest, "malformed workspace git path; want /ws/<name>.git/...")
		return
	}
	switch route {
	case routeInfoRefs:
		if r.Method != http.MethodGet {
			w.Header().Set("Allow", "GET")
			plainError(w, http.StatusMethodNotAllowed, "method not allowed; use GET")
			return
		}
		svc, ok := strings.CutPrefix(r.URL.RawQuery, "service=")
		if !ok || (svc != serviceUpload && svc != serviceReceive) {
			plainError(w, http.StatusBadRequest, "the query must be exactly service=git-upload-pack or service=git-receive-pack")
			return
		}
		m.advertise(w, r, name, svc)
	case routeUpload, routeReceive:
		if r.Method != http.MethodPost {
			w.Header().Set("Allow", "POST")
			plainError(w, http.StatusMethodNotAllowed, "method not allowed; use POST")
			return
		}
		if r.URL.RawQuery != "" || r.URL.ForceQuery {
			plainError(w, http.StatusBadRequest, "git RPC paths accept no query")
			return
		}
		if route == routeUpload {
			m.uploadPack(w, r, name)
		} else {
			m.receivePack(w, r, name)
		}
	default:
		plainError(w, http.StatusNotFound, "no such git endpoint")
	}
}

// gitIO bounds a git request's streaming: the inherited total server
// read/write deadlines are cleared at entry, and every body read or
// response write installs (refreshes) a no-progress deadline of idle.
// halt forces pending reads to return and joins them.
type gitIO struct {
	rc   *http.ResponseController
	idle time.Duration
	body io.Reader
	w    http.ResponseWriter

	mu       sync.Mutex
	inflight int
	idleCond *sync.Cond
	halted   bool
	written  int64
}

func newGitIO(w http.ResponseWriter, r *http.Request, idle time.Duration) *gitIO {
	g := &gitIO{rc: http.NewResponseController(w), idle: idle, body: r.Body, w: w}
	g.idleCond = sync.NewCond(&g.mu)
	g.rc.SetReadDeadline(time.Time{})
	g.rc.SetWriteDeadline(time.Time{})
	return g
}

// Read is the request body with a refreshed read deadline.
func (g *gitIO) Read(p []byte) (int, error) {
	g.mu.Lock()
	if g.halted {
		g.mu.Unlock()
		return 0, errHalted
	}
	g.inflight++
	g.mu.Unlock()
	g.rc.SetReadDeadline(time.Now().Add(g.idle))
	n, err := g.body.Read(p)
	g.mu.Lock()
	g.inflight--
	if g.inflight == 0 {
		g.idleCond.Broadcast()
	}
	g.mu.Unlock()
	return n, err
}

var errHalted = errors.New("the git request body is closed")

// halt ends the request phase: later reads fail, a pending read is forced
// to return by an expired deadline, and every read is joined.
func (g *gitIO) halt() {
	g.mu.Lock()
	g.halted = true
	if g.inflight > 0 {
		g.rc.SetReadDeadline(time.Now())
	}
	for g.inflight > 0 {
		g.idleCond.Wait()
	}
	g.mu.Unlock()
}

// Write is the response with a refreshed write deadline.
func (g *gitIO) Write(p []byte) (int, error) {
	g.rc.SetWriteDeadline(time.Now().Add(g.idle))
	n, err := g.w.Write(p)
	g.written += int64(n)
	return n, err
}

// finish clears the response phase's deadline.
func (g *gitIO) finish() { g.rc.SetWriteDeadline(time.Time{}) }

func (g *gitIO) header(svc string, adv bool) {
	format := contentTypeRPC
	if adv {
		format = contentTypeAdv
	}
	g.w.Header().Set("Content-Type", fmt.Sprintf(format, svc))
	g.w.Header().Set("Cache-Control", "no-cache")
}

// lockStatus maps a lock or lookup failure to its HTTP answer.
func lockStatus(w http.ResponseWriter, err error) {
	switch contract.CodeOf(err) {
	case contract.CodeNotFound:
		plainError(w, http.StatusNotFound, "no such workspace")
	case contract.CodeConflict:
		plainError(w, http.StatusConflict, "stale workspace instance; read workspace show again")
	default:
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			plainError(w, http.StatusServiceUnavailable, "the request was cancelled")
			return
		}
		plainError(w, http.StatusInternalServerError, "workspace unavailable")
	}
}

// instanceHeader returns the request's single valid instance token.
func instanceHeader(r *http.Request) (string, bool) {
	vals := r.Header.Values(contract.WorkspaceInstanceHeader)
	if len(vals) != maxInstanceHdrs || !contract.ValidWorkspaceToken(vals[0]) {
		return "", false
	}
	return vals[0], true
}

func filterCapabilities(in *capability.List, allow map[capability.Capability]bool) (*capability.List, error) {
	out := capability.NewList()
	for _, c := range in.All() {
		if allow[c] {
			if err := out.Set(c, in.Get(c)...); err != nil {
				return nil, err
			}
		}
	}
	return out, nil
}

// advertise serves the ref advertisement of the current generation under
// the read lock. A receive advertisement that carries an instance header
// must match (an early stale answer); absence is allowed here.
func (m *Manager) advertise(w http.ResponseWriter, r *http.Request, name, svc string) {
	ctx, done := m.opCtx(r.Context())
	defer done()
	g := newGitIO(w, r, m.d.idle)
	instance := ""
	if svc == serviceReceive && len(r.Header.Values(contract.WorkspaceInstanceHeader)) > 0 {
		var ok bool
		if instance, ok = instanceHeader(r); !ok {
			plainError(w, http.StatusBadRequest, "malformed "+contract.WorkspaceInstanceHeader+" header")
			return
		}
	}
	h, release, err := m.read(ctx, name, instance)
	if err != nil {
		lockStatus(w, err)
		return
	}
	defer release()
	s, sfs := openStorage(h.repo(), m.d)
	defer closeStorage(s, sfs)
	var ar *packp.AdvRefs
	tr := libraryServer(s)
	if svc == serviceUpload {
		var sess transport.UploadPackSession
		if sess, err = tr.NewUploadPackSession(endpoint, nil); err == nil {
			ar, err = sess.AdvertisedReferencesContext(ctx)
			sess.Close()
		}
		if err == nil {
			ar.Capabilities, err = filterCapabilities(ar.Capabilities, uploadAllow)
		}
	} else {
		var sess transport.ReceivePackSession
		if sess, err = tr.NewReceivePackSession(endpoint, nil); err == nil {
			ar, err = sess.AdvertisedReferencesContext(ctx)
			sess.Close()
		}
		if err == nil {
			ar.Capabilities, err = filterCapabilities(ar.Capabilities, receiveAllow)
		}
	}
	if err != nil {
		plainError(w, http.StatusInternalServerError, "advertisement failed")
		return
	}
	ar.Prefix = [][]byte{[]byte("# service=" + svc), pktline.Flush}
	var buf bytes.Buffer
	if err := ar.Encode(&buf); err != nil {
		plainError(w, http.StatusInternalServerError, "advertisement failed")
		return
	}
	g.header(svc, true)
	g.Write(buf.Bytes())
	g.finish()
}

// decodeHaves reads the stateless-RPC "have" lines after the upload
// request's flush, up to "done", with go-git's pkt-line scanner: haves the
// store lacks are dropped, malformed lines rejected.
func decodeHaves(r io.Reader, req *packp.UploadPackRequest, s storer.EncodedObjectStorer) error {
	sc := pktline.NewScanner(r)
	for sc.Scan() {
		line := bytes.TrimSuffix(sc.Bytes(), []byte("\n"))
		switch {
		case len(line) == 0:
			continue
		case string(line) == "done":
			return nil
		case bytes.HasPrefix(line, []byte("have ")):
			hex := string(line[len("have "):])
			if !contract.ValidCommitHash(hex) {
				return errors.New("malformed have line")
			}
			h := plumbing.NewHash(hex)
			if s.HasEncodedObject(h) == nil {
				req.Haves = append(req.Haves, h)
			}
		default:
			return errors.New("unexpected upload-pack line")
		}
	}
	return sc.Err()
}

// uploadPack serves a fetch from the locked current generation: wants
// must be commits reachable from the advertised refs (ancestors included;
// never orphaned objects); no shallow, deepen or filter. The read lock is
// held until the response is closed and the library's storage use joined.
func (m *Manager) uploadPack(w http.ResponseWriter, r *http.Request, name string) {
	ctx, done := m.opCtx(r.Context())
	defer done()
	g := newGitIO(w, r, m.d.idle)
	defer g.halt()
	h, release, err := m.read(ctx, name, "")
	if err != nil {
		lockStatus(w, err)
		return
	}
	defer release()
	s, sfs := openStorage(h.repo(), m.d)
	defer closeStorage(s, sfs)
	req := packp.NewUploadPackRequest()
	if err := req.Decode(g); err != nil {
		plainError(w, http.StatusBadRequest, "malformed upload-pack request")
		return
	}
	if err := decodeHaves(g, req, s); err != nil {
		plainError(w, http.StatusBadRequest, "malformed upload-pack request")
		return
	}
	g.halt()
	if len(req.Shallows) > 0 || req.Depth != packp.DepthCommits(0) || req.Filter != "" || len(req.Wants) == 0 {
		plainError(w, http.StatusBadRequest, "shallow, deepen, filtered and empty fetches are not supported")
		return
	}
	for _, c := range req.Capabilities.All() {
		if !uploadAllow[c] {
			plainError(w, http.StatusBadRequest, "unsupported upload-pack capability")
			return
		}
	}
	refs, err := readRefs(h.repo())
	if err != nil {
		plainError(w, http.StatusInternalServerError, "storage failure")
		return
	}
	reach, err := reachableCommits(ctx, s, tips(refs), nil)
	if err != nil {
		plainError(w, http.StatusInternalServerError, "storage failure")
		return
	}
	for _, want := range req.Wants {
		if !reach[want] {
			plainError(w, http.StatusBadRequest, "a wanted object is not a commit reachable from an advertised ref")
			return
		}
	}
	m.d.stage("upload-locked", ctx)
	sess, err := libraryServer(s).NewUploadPackSession(endpoint, nil)
	if err != nil {
		plainError(w, http.StatusInternalServerError, "session failed")
		return
	}
	defer sess.Close()
	resp, err := sess.UploadPack(ctx, req)
	if err != nil {
		plainError(w, http.StatusBadRequest, "upload-pack rejected")
		return
	}
	g.header(serviceUpload, false)
	resp.Encode(g)
	resp.Close()
	g.finish()
}

// report writes a single-command report-status with safe fixed strings.
func report(g *gitIO, unpack string, ref plumbing.ReferenceName, status string) {
	rs := packp.NewReportStatus()
	rs.UnpackStatus = unpack
	rs.CommandStatuses = []*packp.CommandStatus{{ReferenceName: ref, Status: status}}
	var buf bytes.Buffer
	if err := rs.Encode(&buf); err != nil {
		plainError(g.w, http.StatusInternalServerError, "report failed")
		return
	}
	g.header(serviceReceive, false)
	g.Write(buf.Bytes())
	g.finish()
}

// receivePack is the guarded, durable single-ref receive: the instance
// header, framing, capabilities, one non-delete command, a nonzero new
// hash and a portable refs/heads/ target are checked before any lookup,
// generation or unpacking; under the workspace lock the CAS (zero old:
// create only; otherwise equal to the current value, also when new
// equals it) and namespace are checked, the library receives into a
// private copied generation, the staged ref and the complete closure are
// validated, and the generation is published. The library report stays
// private; success is reported only after publication.
func (m *Manager) receivePack(w http.ResponseWriter, r *http.Request, name string) {
	ctx, done := m.opCtx(r.Context())
	defer done()
	g := newGitIO(w, r, m.d.idle)
	defer g.halt()
	h, err := m.lookup(name)
	if err != nil {
		lockStatus(w, err)
		return
	}
	instance, ok := instanceHeader(r)
	if !ok {
		plainError(w, http.StatusBadRequest, "the request needs exactly one valid "+contract.WorkspaceInstanceHeader+" header (from workspace show)")
		return
	}
	if instance != h.instance {
		lockStatus(w, errStaleInstance())
		return
	}
	req := packp.NewReferenceUpdateRequest()
	if err := req.Decode(g); err != nil {
		plainError(w, http.StatusBadRequest, "malformed receive-pack request")
		return
	}
	if len(req.Commands) != 1 {
		plainError(w, http.StatusBadRequest, "exactly one command is supported")
		return
	}
	for _, c := range req.Capabilities.All() {
		if !receiveAllow[c] {
			plainError(w, http.StatusBadRequest, "unsupported receive-pack capability")
			return
		}
	}
	if !req.Capabilities.Supports(capability.ReportStatus) {
		plainError(w, http.StatusBadRequest, "the report-status capability is required")
		return
	}
	cmd := req.Commands[0]
	if cmd.New.IsZero() {
		plainError(w, http.StatusBadRequest, "delete commands are not supported; the new object ID must be nonzero")
		return
	}
	if !contract.ValidBranchRef(string(cmd.Name)) || cmd.Name.Validate() != nil {
		report(g, unpackNotAttempted, cmd.Name, reasonInvalidRef)
		return
	}
	fail := func(unpack, reason string) { report(g, unpack, cmd.Name, reason) }
	h, release, err := m.write(ctx, name, instance)
	if err != nil {
		switch {
		case errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded):
			fail(unpackNotAttempted, reasonCancelled)
		case contract.CodeOf(err) == contract.CodeInternal:
			fail(unpackNotAttempted, reasonStorage)
		default:
			lockStatus(w, err)
		}
		return
	}
	defer release()
	m.d.stage("receive-locked", ctx)
	refs, err := readRefs(h.repo())
	if err != nil {
		fail(unpackNotAttempted, reasonStorage)
		return
	}
	cur, exists := refs[string(cmd.Name)]
	switch {
	case cmd.Old.IsZero() && exists, !cmd.Old.IsZero() && (!exists || cur != cmd.Old):
		fail(unpackNotAttempted, reasonStaleOld)
		return
	case !exists && namespaceConflict(refs, string(cmd.Name)):
		fail(unpackNotAttempted, reasonConflict)
		return
	}
	t, err := m.begin(ctx, h, true)
	if err != nil {
		fail(unpackNotAttempted, cancelOr(ctx, reasonStorage))
		return
	}
	s, sfs := openStorage(t.repo(), m.d)
	var check *packCheck
	if req.Packfile != nil {
		check = newPackCheck(req.Packfile)
		req.Packfile = readCloser{Reader: check, Closer: req.Packfile}
	}
	var rs *packp.ReportStatus
	sess, rerr := libraryServer(s).NewReceivePackSession(endpoint, nil)
	if rerr == nil {
		rs, rerr = sess.ReceivePack(ctx, req)
		sess.Close()
	}
	cerr := closeStorage(s, sfs)
	if rerr == nil && rs != nil && rs.Error() == nil && check != nil {
		if err := check.verify(); err != nil {
			rs.UnpackStatus = unpackFailed
		}
	}
	g.halt()
	if req.Packfile != nil {
		req.Packfile.Close()
	}
	if rerr != nil || rs == nil || rs.Error() != nil || cerr != nil {
		t.discard()
		switch {
		case ctx.Err() != nil:
			fail(unpackFailed, reasonCancelled)
		case rs != nil && rs.UnpackStatus == unpackOK:
			fail(unpackOK, reasonStorage)
		default:
			fail(unpackFailed, reasonClosure)
		}
		return
	}
	m.d.stage("receive-unpacked", ctx)
	staged, err := readRefs(t.repo())
	if err != nil || staged[string(cmd.Name)] != cmd.New {
		t.discard()
		fail(unpackOK, reasonStorage)
		return
	}
	if _, err := m.d.validateGeneration(ctx, t.dir, nil); err != nil {
		t.discard()
		var cl errClosure
		switch {
		case ctx.Err() != nil:
			fail(unpackOK, reasonCancelled)
		case errors.As(err, &cl):
			fail(unpackOK, reasonClosure)
		default:
			fail(unpackOK, reasonStorage)
		}
		return
	}
	if err := t.commit(ctx); err != nil {
		var te txnError
		if errors.As(err, &te) && !te.committed {
			fail(unpackOK, cancelOr(ctx, reasonStorage))
			return
		}
		fail(unpackOK, reasonStorage)
		return
	}
	report(g, unpackOK, cmd.Name, "ok")
}

// packCheck verifies a received pack's trailing SHA-1 over the stream the
// library parses (go-git v5.16.3 reads the trailer without checking it):
// every byte but the last 20 is hashed, and verify drains the remainder
// and compares.
type packCheck struct {
	r     io.Reader
	h     hash.Hash
	tail  []byte
	total int64
}

func newPackCheck(r io.Reader) *packCheck { return &packCheck{r: r, h: sha1.New()} }

func (p *packCheck) Read(b []byte) (int, error) {
	n, err := p.r.Read(b)
	if n > 0 {
		p.total += int64(n)
		p.tail = append(p.tail, b[:n]...)
		if over := len(p.tail) - sha1.Size; over > 0 {
			p.h.Write(p.tail[:over])
			p.tail = append(p.tail[:0], p.tail[over:]...)
		}
	}
	return n, err
}

// errPackChecksum marks a pack whose trailer does not match its content.
var errPackChecksum = errors.New("pack checksum mismatch")

func (p *packCheck) verify() error {
	if _, err := io.Copy(io.Discard, p); err != nil {
		return err
	}
	if p.total < 12+sha1.Size || !bytes.Equal(p.h.Sum(nil), p.tail) {
		return errPackChecksum
	}
	return nil
}

type readCloser struct {
	io.Reader
	io.Closer
}

func cancelOr(ctx context.Context, reason string) string {
	if ctx.Err() != nil {
		return reasonCancelled
	}
	return reason
}
