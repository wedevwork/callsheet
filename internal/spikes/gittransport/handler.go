// Package gittransport is the iteration 01 embedded-git transport spike:
// go-git v5 smart HTTP served over TLS, with Callsheet integrity guards
// around a quarantined receive-pack session. It is experimental, test-only
// code and is never imported by cmd/callsheet or wired into the plane.
package gittransport

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"path/filepath"
	"sync"
	"sync/atomic"

	"github.com/go-git/go-billy/v5/osfs"
	git "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/cache"
	"github.com/go-git/go-git/v5/plumbing/format/pktline"
	"github.com/go-git/go-git/v5/plumbing/protocol/packp"
	"github.com/go-git/go-git/v5/plumbing/protocol/packp/capability"
	"github.com/go-git/go-git/v5/plumbing/storer"
	"github.com/go-git/go-git/v5/plumbing/transport"
	"github.com/go-git/go-git/v5/plumbing/transport/server"
	"github.com/go-git/go-git/v5/storage/filesystem"
)

// Fixed fixture routes. The handler resolves exactly one repository.
const (
	RepoURLPath      = "/test/git/repo.git"
	infoRefsPath     = RepoURLPath + "/info/refs"
	uploadPackPath   = RepoURLPath + "/git-upload-pack"
	receivePackPath  = RepoURLPath + "/git-receive-pack"
	serviceUpload    = "git-upload-pack"
	serviceReceive   = "git-receive-pack"
	contentTypeRPC   = "application/x-%s-result"
	contentTypeAdv   = "application/x-%s-advertisement"
	endpointRepoPath = "/repo.git"
)

// maxRequestBytes bounds an entire RPC request body (64MiB); a variable
// only so unit tests can exercise the 413 path with small bodies.
var maxRequestBytes int64 = 64 << 20

// receiveAllow is the complete receive-pack capability allowlist.
var receiveAllow = map[capability.Capability]bool{
	capability.ReportStatus: true,
	capability.OFSDelta:     true,
	capability.Agent:        true,
}

// exchange is one captured HTTP request: method, path and byte counts only,
// never object contents.
type exchange struct {
	Method        string `json:"method"`
	Path          string `json:"path"`
	Query         string `json:"query,omitempty"`
	Status        int    `json:"status"`
	RequestBytes  int64  `json:"request_bytes"`
	ResponseBytes int64  `json:"response_bytes"`
}

// hooks are package-local test seams; they are never exposed as product
// controls or HTTP parameters.
type hooks struct {
	packRead      func()
	afterValidate func(cancel context.CancelFunc)
	promote       func(plumbing.Hash) error
	done          func(path string)
}

type smartHandler struct {
	path  string
	ep    *transport.Endpoint
	mu    sync.RWMutex
	store *filesystem.Storage

	hooks hooks

	tmu       sync.Mutex
	log       []exchange
	calls     int
	promoted  int
	lastError string
}

// NewSmartHandler serves the bare repository at repoPath as the single fixed
// repository /test/git/repo.git. It rejects missing or non-bare paths.
func NewSmartHandler(repoPath string) (http.Handler, error) {
	abs, err := filepath.Abs(repoPath)
	if err != nil {
		return nil, err
	}
	repo, err := git.PlainOpen(abs)
	if err != nil {
		return nil, fmt.Errorf("gittransport: open %s: %w", abs, err)
	}
	cfg, err := repo.Config()
	if err != nil {
		return nil, fmt.Errorf("gittransport: config: %w", err)
	}
	if !cfg.Core.IsBare {
		return nil, fmt.Errorf("gittransport: %s is not a bare repository", abs)
	}
	ep, err := transport.NewEndpoint(endpointRepoPath)
	if err != nil {
		return nil, err
	}
	return &smartHandler{path: abs, ep: ep, store: openStore(abs)}, nil
}

func openStore(path string) *filesystem.Storage {
	return filesystem.NewStorage(osfs.New(path), cache.NewObjectLRUDefault())
}

// reopen closes and reopens the durable store (persistence checks).
func (h *smartHandler) reopen() error {
	h.mu.Lock()
	defer h.mu.Unlock()
	err := h.store.Close()
	h.store = openStore(h.path)
	return err
}

func (h *smartHandler) transportFor(st storer.Storer) transport.Transport {
	return server.NewServer(server.MapLoader{h.ep.String(): st})
}

func (h *smartHandler) transcript() []exchange {
	h.tmu.Lock()
	defer h.tmu.Unlock()
	return append([]exchange(nil), h.log...)
}

func (h *smartHandler) callCount() int {
	h.tmu.Lock()
	defer h.tmu.Unlock()
	return h.calls
}

func (h *smartHandler) lastPromoted() int {
	h.tmu.Lock()
	defer h.tmu.Unlock()
	return h.promoted
}

type countingWriter struct {
	http.ResponseWriter
	n      int64
	status int
}

func (w *countingWriter) WriteHeader(code int) {
	if w.status == 0 {
		w.status = code
	}
	w.ResponseWriter.WriteHeader(code)
}

func (w *countingWriter) Write(p []byte) (int, error) {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	n, err := w.ResponseWriter.Write(p)
	w.n += int64(n)
	return n, err
}

// countingReader counts request body bytes. The count is atomic because
// go-git's context-aware pack reader may still be reading on its own
// goroutine after a cancelled ReceivePack has returned.
type countingReader struct {
	r io.ReadCloser
	n atomic.Int64
}

func (c *countingReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n.Add(int64(n))
	return n, err
}

func (c *countingReader) Close() error { return c.r.Close() }

func (h *smartHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	cw := &countingWriter{ResponseWriter: w}
	cr := &countingReader{r: r.Body}
	r.Body = cr
	h.tmu.Lock()
	h.calls++
	h.tmu.Unlock()
	defer func() {
		h.tmu.Lock()
		h.log = append(h.log, exchange{Method: r.Method, Path: r.URL.Path, Query: r.URL.RawQuery, Status: cw.status, RequestBytes: cr.n.Load(), ResponseBytes: cw.n})
		h.tmu.Unlock()
		if h.hooks.done != nil {
			h.hooks.done(r.URL.Path)
		}
	}()
	switch r.URL.Path {
	case infoRefsPath:
		if r.Method != http.MethodGet {
			http.Error(cw, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		h.advertise(cw, r)
	case uploadPackPath:
		if r.Method != http.MethodPost {
			http.Error(cw, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		h.uploadPack(cw, r)
	case receivePackPath:
		if r.Method != http.MethodPost {
			http.Error(cw, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		h.receivePack(cw, r)
	default:
		http.NotFound(cw, r)
	}
}

func (h *smartHandler) advertise(w http.ResponseWriter, r *http.Request) {
	svc := r.URL.Query().Get("service")
	if svc != serviceUpload && svc != serviceReceive {
		http.Error(w, "unsupported service", http.StatusBadRequest)
		return
	}
	h.mu.RLock()
	defer h.mu.RUnlock()
	tr := h.transportFor(h.store)
	var ar *packp.AdvRefs
	var err error
	if svc == serviceUpload {
		var s transport.UploadPackSession
		if s, err = tr.NewUploadPackSession(h.ep, nil); err == nil {
			ar, err = s.AdvertisedReferencesContext(r.Context())
			s.Close()
		}
	} else {
		var s transport.ReceivePackSession
		if s, err = tr.NewReceivePackSession(h.ep, nil); err == nil {
			ar, err = s.AdvertisedReferencesContext(r.Context())
			s.Close()
		}
		if err == nil {
			ar.Capabilities, err = filterCapabilities(ar.Capabilities, receiveAllow)
		}
	}
	if err != nil {
		http.Error(w, "advertisement failed", http.StatusInternalServerError)
		return
	}
	writeAdvertisement(w, svc, ar)
}

// writeAdvertisement encodes ar with the smart-HTTP service prefix into a
// buffer first, so an encoding failure answers 500 instead of a partial 200.
func writeAdvertisement(w http.ResponseWriter, svc string, ar *packp.AdvRefs) {
	ar.Prefix = [][]byte{[]byte("# service=" + svc), pktline.Flush}
	var buf bytes.Buffer
	if err := ar.Encode(&buf); err != nil {
		http.Error(w, "advertisement encoding failed", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", fmt.Sprintf(contentTypeAdv, svc))
	w.Header().Set("Cache-Control", "no-cache")
	w.Write(buf.Bytes())
}

// filterCapabilities keeps only allowed capabilities (and their values).
func filterCapabilities(in *capability.List, allow map[capability.Capability]bool) (*capability.List, error) {
	out := capability.NewList()
	for _, c := range in.All() {
		if !allow[c] {
			continue
		}
		if err := out.Set(c, in.Get(c)...); err != nil {
			return nil, err
		}
	}
	return out, nil
}

func (h *smartHandler) uploadPack(w http.ResponseWriter, r *http.Request) {
	body := http.MaxBytesReader(w, r.Body, maxRequestBytes)
	h.mu.RLock()
	defer h.mu.RUnlock()
	req := packp.NewUploadPackRequest()
	if err := req.Decode(body); err != nil {
		httpDecodeError(w, err)
		return
	}
	if err := decodeHaves(body, req, h.store); err != nil {
		httpDecodeError(w, err)
		return
	}
	s, err := h.transportFor(h.store).NewUploadPackSession(h.ep, nil)
	if err != nil {
		http.Error(w, "session failed", http.StatusInternalServerError)
		return
	}
	defer s.Close()
	resp, err := s.UploadPack(r.Context(), req)
	if err != nil {
		http.Error(w, "upload-pack rejected", http.StatusBadRequest)
		return
	}
	defer resp.Close()
	w.Header().Set("Content-Type", fmt.Sprintf(contentTypeRPC, serviceUpload))
	w.Header().Set("Cache-Control", "no-cache")
	resp.Encode(w)
}

// decodeHaves reads the stateless-RPC "have" lines that follow the
// upload-request flush, up to "done", using go-git's pkt-line scanner.
// packp.UploadPackRequest.Decode stops at the flush, so without this the
// library session would ignore the client's haves and always send the full
// closure. Haves the server does not hold are dropped (not common objects).
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
			if !plumbing.IsHash(hex) {
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
	if err := sc.Err(); err != nil {
		return err
	}
	return nil
}

func httpDecodeError(w http.ResponseWriter, err error) {
	var mb *http.MaxBytesError
	if errors.As(err, &mb) {
		http.Error(w, "request too large", http.StatusRequestEntityTooLarge)
		return
	}
	http.Error(w, "malformed request", http.StatusBadRequest)
}
