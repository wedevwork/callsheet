package client

import (
	"bufio"
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
	"sync/atomic"
	"time"

	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/format/pktline"
	"github.com/go-git/go-git/v5/plumbing/protocol/packp"
	"github.com/go-git/go-git/v5/plumbing/protocol/packp/capability"

	"github.com/wedevwork/callsheet/internal/contract"
)

// The workspace git session (iteration 09b): a per-call smart-HTTP client
// of one workspace's /ws/<name>.git endpoint over the client's verified
// trust (the same CA, hostname verification, no proxy, no redirect and
// HTTP/1.1 only). It speaks the protocol with go-git's pkt-line and packp
// primitives on a session-local transport: nothing is registered globally,
// no remote is persisted, no credential helper or git process runs.
//
// Bulk streams are not bounded by the ten-second control operation
// timeout. Dialing and the TLS handshake keep their bounds; every request
// body write, the wait for the response and every response body read is
// guarded by a no-progress deadline (GitIdle), refreshed on each positive
// transfer, and the caller's context cancels at any time.

// GitIdle is the no-progress deadline of a git transfer stream.
const GitIdle = 30 * time.Second

// Clock is the git session's time source (tests inject a fake clock whose
// timers fire only when it is advanced).
type Clock interface {
	Now() time.Time
	NewTimer(d time.Duration) (<-chan time.Time, func() bool)
}

type realClock struct{}

func (realClock) Now() time.Time { return time.Now() }

func (realClock) NewTimer(d time.Duration) (<-chan time.Time, func() bool) {
	t := time.NewTimer(d)
	return t.C, t.Stop
}

// WorkspaceGit is a git session for one workspace.
type WorkspaceGit struct {
	c     *Client
	name  string
	base  string
	tr    *http.Transport
	hc    *http.Client
	clock Clock
	idle  time.Duration
	// hook observes the no-progress watchdog (tests only): "armed" after
	// each timer is registered, "idle" when it cancels a stream.
	hook func(stage string)
}

// WorkspaceGit returns a git session of workspace name. Close it after
// use; it never contacts the plane until a method is called.
func (c *Client) WorkspaceGit(name string) (*WorkspaceGit, error) {
	if err := checkName(name); err != nil {
		return nil, err
	}
	tr := newTransportHeader(c.tr.TLSClientConfig.Clone(), 0)
	return &WorkspaceGit{c: c, name: name, base: c.ep.url + contract.WorkspaceGitPrefix + name + ".git/", tr: tr,
		hc: newHTTPClient(tr), clock: realClock{}, idle: GitIdle}, nil
}

// Close closes the session's idle connections.
func (g *WorkspaceGit) Close() { g.tr.CloseIdleConnections() }

func (g *WorkspaceGit) stage(s string) {
	if g.hook != nil {
		g.hook(s)
	}
}

// errNoProgress cancels a stream that made no progress within the idle
// deadline.
var errNoProgress = errors.New("git transfer made no progress")

// watchdog cancels its stream when no positive transfer happened for the
// idle deadline. A fired timer compares the elapsed time since the last
// progress (not the order of ready events) and re-arms for the remainder.
type watchdog struct {
	g      *WorkspaceGit
	cancel context.CancelCauseFunc
	last   atomic.Int64
	stop   chan struct{}
	done   chan struct{}
	once   sync.Once
}

func (g *WorkspaceGit) watch(cancel context.CancelCauseFunc) *watchdog {
	w := &watchdog{g: g, cancel: cancel, stop: make(chan struct{}), done: make(chan struct{})}
	w.touch()
	go w.run()
	return w
}

func (w *watchdog) touch() { w.last.Store(w.g.clock.Now().UnixNano()) }

func (w *watchdog) run() {
	defer close(w.done)
	wait := w.g.idle
	for {
		ch, stop := w.g.clock.NewTimer(wait)
		w.g.stage("armed")
		select {
		case <-w.stop:
			stop()
			return
		case now := <-ch:
			idle := now.Sub(time.Unix(0, w.last.Load()))
			if idle < w.g.idle {
				wait = w.g.idle - idle
				continue
			}
			w.cancel(errNoProgress)
			w.g.stage("idle")
			return
		}
	}
}

// close stops the watchdog and joins it.
func (w *watchdog) close() {
	w.once.Do(func() { close(w.stop) })
	<-w.done
}

// progressReader refreshes the watchdog on every positive read.
type progressReader struct {
	r io.Reader
	w *watchdog
}

func (p *progressReader) Read(b []byte) (int, error) {
	n, err := p.r.Read(b)
	if n > 0 {
		p.w.touch()
	}
	return n, err
}

// stream is one guarded HTTP exchange: its context, watchdog and response.
type stream struct {
	ctx    context.Context
	cancel context.CancelCauseFunc
	w      *watchdog
	resp   *http.Response
	body   io.Reader
}

// close stops the watchdog, closes the response and releases the context
// (every path; the transport's goroutines end with the closed body).
func (s *stream) close() {
	s.w.close()
	if s.resp != nil {
		s.resp.Body.Close()
	}
	s.cancel(nil)
}

// failure classifies a stream error: the caller's cancellation is
// returned unchanged (exit 130), a watchdog cancellation is unavailable,
// everything else goes through the verified client's classification
// (trust_failed for verification failures).
func (g *WorkspaceGit) failure(parent context.Context, s *stream, err error) error {
	if perr := parent.Err(); perr != nil {
		return perr
	}
	if errors.Is(context.Cause(s.ctx), errNoProgress) {
		return contract.Wrap(contract.CodeUnavailable, fmt.Sprintf("the workspace git transfer made no progress for %s", g.idle), err)
	}
	return classify(g.c.ep, err)
}

// do sends one request whose body (nil for GET) is streamed through the
// watchdog, and returns the open stream of a 200 answer or a contract
// error mapped from any other status.
func (g *WorkspaceGit) do(ctx context.Context, method, path, contentType, instance string, body io.Reader, what string) (*stream, error) {
	sctx, cancel := context.WithCancelCause(ctx)
	s := &stream{ctx: sctx, cancel: cancel}
	s.w = g.watch(cancel)
	var rb io.Reader
	if body != nil {
		rb = &progressReader{r: body, w: s.w}
	}
	req, err := http.NewRequestWithContext(sctx, method, g.base+path, rb)
	if err != nil {
		s.close()
		return nil, contract.Wrap(contract.CodeInternal, "cannot build the git request", err)
	}
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
		req.Header.Set("Accept", strings.Replace(contentType, "-request", "-result", 1))
	}
	if instance != "" {
		req.Header.Set(contract.WorkspaceInstanceHeader, instance)
	}
	resp, err := g.hc.Do(req)
	if err != nil {
		e := g.failure(ctx, s, err)
		s.close()
		return nil, e
	}
	s.resp = resp
	s.w.touch()
	if resp.StatusCode != http.StatusOK {
		s.close()
		return nil, gitStatusError(resp.StatusCode, what)
	}
	s.body = &progressReader{r: resp.Body, w: s.w}
	return s, nil
}

// gitStatusError maps a git endpoint's HTTP error answer (plain text,
// never echoed) to a contract error.
func gitStatusError(status int, what string) error {
	switch status {
	case http.StatusNotFound:
		return contract.New(contract.CodeNotFound, "no such workspace on the plane")
	case http.StatusConflict:
		return contract.New(contract.CodeConflict, "stale workspace instance: the workspace was removed or recreated; read ws show again")
	case http.StatusBadRequest:
		return contract.New(contract.CodeInvalidArgument, "the plane refused the "+what+" request")
	case http.StatusServiceUnavailable:
		return contract.New(contract.CodeUnavailable, "the plane cancelled the "+what+" request")
	case http.StatusHTTPVersionNotSupported:
		return contract.New(contract.CodeProtocolMismatch, "the plane refused the git transport's HTTP version")
	}
	return contract.New(contract.CodeInternal, fmt.Sprintf("the plane answered the %s request with HTTP %d", what, status))
}

func invalidGitAnswer(what string) error {
	return contract.New(contract.CodeInternal, "the plane's "+what+" answer is malformed")
}

// advertisement reads a service's ref advertisement: only refs/heads/ and
// refs/callsheet/tasks/ names with full hashes.
func (g *WorkspaceGit) advertisement(ctx context.Context, service, instance string) (map[string]plumbing.Hash, error) {
	s, err := g.do(ctx, http.MethodGet, "info/refs?service="+service, "", instance, nil, service+" advertisement")
	if err != nil {
		return nil, err
	}
	defer s.close()
	ar := packp.NewAdvRefs()
	if err := ar.Decode(s.body); err != nil {
		if errors.Is(err, packp.ErrEmptyAdvRefs) {
			return map[string]plumbing.Hash{}, nil
		}
		if s.ctx.Err() != nil {
			return nil, g.failure(ctx, s, err)
		}
		return nil, invalidGitAnswer(service + " advertisement")
	}
	out := map[string]plumbing.Hash{}
	if ar.IsEmpty() {
		return out, nil
	}
	for name, h := range ar.References {
		if !contract.ValidBranchRef(name) && !contract.ValidTaskRef(name) {
			return nil, invalidGitAnswer(service + " advertisement")
		}
		out[name] = h
	}
	return out, nil
}

// ReceiveRefs reads the receive-pack advertisement with the instance
// header: the observed refs (a stale instance conflicts before any
// mutation).
func (g *WorkspaceGit) ReceiveRefs(ctx context.Context, instance string) (map[string]plumbing.Hash, error) {
	if err := contract.ValidateInstance(instance); err != nil {
		return nil, err
	}
	return g.advertisement(ctx, "git-receive-pack", instance)
}

// UploadRefs reads the upload-pack advertisement.
func (g *WorkspaceGit) UploadRefs(ctx context.Context) (map[string]plumbing.Hash, error) {
	return g.advertisement(ctx, "git-upload-pack", "")
}

// Push sends exactly one receive-pack command ref old->new (old zero:
// create only) with the pack streamed from pack, and succeeds only on an
// ok unpack status and an ok command status for ref in the report.
// Nothing is retried; a lost answer is ambiguous (the error says so).
func (g *WorkspaceGit) Push(ctx context.Context, instance, ref string, old, new plumbing.Hash, pack io.Reader) error {
	if err := contract.ValidateInstance(instance); err != nil {
		return err
	}
	if !contract.ValidBranchRef(ref) || new.IsZero() {
		return contract.New(contract.CodeInvalidArgument, "a push needs a portable branch ref and a nonzero commit")
	}
	caps := capability.NewList()
	caps.Set(capability.ReportStatus)
	var head bytes.Buffer
	e := pktline.NewEncoder(&head)
	if err := e.Encodef("%s %s %s\x00%s", old, new, ref, caps.String()); err != nil {
		return contract.Wrap(contract.CodeInternal, "cannot encode the push command", err)
	}
	if err := e.Flush(); err != nil {
		return contract.Wrap(contract.CodeInternal, "cannot encode the push command", err)
	}
	s, err := g.do(ctx, http.MethodPost, "git-receive-pack", "application/x-git-receive-pack-request", instance,
		io.MultiReader(&head, pack), "receive-pack")
	if err != nil {
		if contract.CodeOf(err) == contract.CodeUnavailable {
			return ambiguous(err)
		}
		return err
	}
	defer s.close()
	rs := packp.NewReportStatus()
	if err := rs.Decode(s.body); err != nil {
		if s.ctx.Err() != nil || ctx.Err() != nil {
			if ferr := g.failure(ctx, s, err); contract.CodeOf(ferr) == contract.CodeUnavailable {
				return ambiguous(ferr)
			} else {
				return ferr
			}
		}
		return contract.New(contract.CodeInternal, "the plane's push report is malformed; "+contract.PushAmbiguity)
	}
	return reportError(rs, ref)
}

// ambiguous marks a lost push answer.
func ambiguous(err error) error {
	var ce *contract.Error
	if errors.As(err, &ce) {
		return &contract.Error{Code: ce.Code, Message: ce.Message + "; " + contract.PushAmbiguity, Details: ce.Details, Cause: ce.Cause}
	}
	return err
}

// reportError maps a report-status to success or a contract error with
// the plane's fixed reason strings (09a).
func reportError(rs *packp.ReportStatus, ref string) error {
	if len(rs.CommandStatuses) != 1 || string(rs.CommandStatuses[0].ReferenceName) != ref {
		return contract.New(contract.CodeInternal, "the plane's push report does not name exactly the pushed ref; "+contract.PushAmbiguity)
	}
	status := rs.CommandStatuses[0].Status
	if rs.UnpackStatus == "ok" && status == "ok" {
		return nil
	}
	switch status {
	case "stale old ref":
		return contract.New(contract.CodeConflict, "the branch moved since it was observed (compare-and-swap failed); nothing changed: read ws status and push again")
	case "ref namespace conflict":
		return contract.New(contract.CodeConflict, "the branch name conflicts with an existing branch's namespace; nothing changed")
	case "invalid ref name":
		return contract.New(contract.CodeInvalidArgument, "the plane refused the branch name")
	case "cancelled before publication":
		return contract.New(contract.CodeUnavailable, "the plane cancelled the push before publication; nothing changed")
	case "incomplete object closure":
		return contract.TransferError(contract.CodeInternal, contract.ReasonStorageFailure, "the plane rejected the pushed objects as incomplete or invalid; nothing changed")
	case "storage failure":
		return contract.TransferError(contract.CodeInternal, contract.ReasonStorageFailure, "the plane reported a storage failure; "+contract.PushAmbiguity)
	}
	return contract.New(contract.CodeInternal, "the plane refused the push with an unknown status; "+contract.PushAmbiguity)
}

// Fetch asks upload-pack for wants (commits reachable from the plane's
// refs) with haves, and passes the pack stream to sink. The pack's
// trailing checksum is verified over the whole stream after sink returns
// (go-git's parser reads the trailer without checking it).
func (g *WorkspaceGit) Fetch(ctx context.Context, wants, haves []plumbing.Hash, sink func(io.Reader) error) error {
	if len(wants) == 0 {
		return contract.New(contract.CodeInvalidArgument, "a fetch needs at least one wanted commit")
	}
	req := packp.NewUploadPackRequest()
	req.Wants = append(req.Wants, wants...)
	req.Haves = append(req.Haves, haves...)
	req.Capabilities.Set(capability.OFSDelta)
	var body bytes.Buffer
	if err := req.UploadRequest.Encode(&body); err != nil {
		return contract.Wrap(contract.CodeInternal, "cannot encode the fetch request", err)
	}
	if err := req.UploadHaves.Encode(&body, false); err != nil {
		return contract.Wrap(contract.CodeInternal, "cannot encode the fetch request", err)
	}
	if err := pktline.NewEncoder(&body).EncodeString("done\n"); err != nil {
		return contract.Wrap(contract.CodeInternal, "cannot encode the fetch request", err)
	}
	s, err := g.do(ctx, http.MethodPost, "git-upload-pack", "application/x-git-upload-pack-request", "", &body, "upload-pack")
	if err != nil {
		if contract.CodeOf(err) == contract.CodeInvalidArgument {
			return contract.New(contract.CodeConflict, "the plane no longer offers the selected commit (its ref moved or it was pruned); nothing was written locally")
		}
		return err
	}
	defer s.close()
	br := bufio.NewReader(s.body)
	var sr packp.ServerResponse
	if err := sr.Decode(br, false); err != nil {
		if s.ctx.Err() != nil || ctx.Err() != nil {
			return g.failure(ctx, s, err)
		}
		return invalidGitAnswer("fetch")
	}
	check := &packCheck{r: br, h: sha1.New()}
	serr := sink(check)
	if s.ctx.Err() != nil || ctx.Err() != nil {
		return g.failure(ctx, s, s.ctx.Err())
	}
	if serr != nil {
		return serr
	}
	if err := check.verify(); err != nil {
		if s.ctx.Err() != nil || ctx.Err() != nil {
			return g.failure(ctx, s, err)
		}
		return contract.New(contract.CodeInternal, "the fetched pack's checksum does not match its content")
	}
	return nil
}

// packCheck hashes every byte of a pack stream but its last 20 and
// compares them with that trailer.
type packCheck struct {
	r     io.Reader
	h     hash.Hash
	tail  []byte
	total int64
}

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
