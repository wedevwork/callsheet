package plane

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/wedevwork/callsheet/internal/contract"
)

// nodeService serves the node API (iteration 03): CA bootstrap, enrollment,
// the roster and the node stream. It owns stream admission and every
// upgraded socket; Run joins them, and the sweep, before the HTTP server
// shuts down.
type nodeService struct {
	reg    *nodeRegistry
	clock  nodeClock
	logger *slog.Logger
	caPEM  []byte
	// closeGrace bounds a graceful WebSocket close before the socket is
	// force-closed.
	closeGrace time.Duration
	// events, when non-nil, lets tests observe stream lifecycles.
	events func(string)
	// helloRead, when non-nil, runs after a hello read returned a frame
	// and before its deadline is released (tests only).
	helloRead func(context.Context)

	mu      sync.Mutex
	closed  bool
	streams map[*nodeStream]struct{}
	wg      sync.WaitGroup

	sweepStop chan struct{}
	sweepDone chan struct{}
}

func newNodeService(reg *nodeRegistry, clock nodeClock, logger *slog.Logger, caPEM []byte, closeGrace time.Duration) *nodeService {
	return &nodeService{reg: reg, clock: clock, logger: logger, caPEM: caPEM, closeGrace: closeGrace, streams: map[*nodeStream]struct{}{}}
}

// startSweep runs Expire every sweepInterval of the node clock until
// shutdown.
func (s *nodeService) startSweep() {
	s.sweepStop, s.sweepDone = make(chan struct{}), make(chan struct{})
	tick, stop := s.clock.NewTicker(sweepInterval)
	go func() {
		defer close(s.sweepDone)
		defer stop()
		for {
			select {
			case <-s.sweepStop:
				return
			case <-tick:
				s.reg.Expire()
			}
		}
	}()
}

// shutdown stops stream admission, closes every stream (1001, then a
// forced close for blocked I/O), closes the registry's attachments, and
// joins every stream handler and the sweep. Waiting is bounded by
// deadline, after which every socket is force-closed; forced sockets end
// their handlers, so the final join is always reached.
func (s *nodeService) shutdown(deadline time.Time) {
	s.mu.Lock()
	s.closed = true
	streams := make([]*nodeStream, 0, len(s.streams))
	for st := range s.streams {
		streams = append(streams, st)
	}
	s.mu.Unlock()
	for _, st := range streams {
		st.terminate(closeGoingAway, "plane shutting down")
	}
	s.reg.Close()
	if s.sweepStop != nil {
		close(s.sweepStop)
		<-s.sweepDone
	}
	joined := make(chan struct{})
	go func() { s.wg.Wait(); close(joined) }()
	t := time.NewTimer(time.Until(deadline))
	defer t.Stop()
	select {
	case <-joined:
		return
	case <-t.C:
	}
	for _, st := range streams {
		st.force()
	}
	<-joined
}

func (s *nodeService) isClosed() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.closed
}

// admit registers st unless shutdown began.
func (s *nodeService) admit(st *nodeStream) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return false
	}
	s.streams[st] = struct{}{}
	s.wg.Add(1)
	return true
}

func (s *nodeService) release(st *nodeStream) {
	s.mu.Lock()
	delete(s.streams, st)
	s.mu.Unlock()
	s.wg.Done()
}

func (s *nodeService) event(e string) {
	if s.events != nil {
		s.events(e)
	}
}

// writeJSON writes a 2xx JSON response with a final LF.
func writeJSON(w http.ResponseWriter, status int, v any) {
	b, err := contract.Encode(v)
	if err != nil {
		writeError(w, contract.Wrap(contract.CodeInternal, "cannot encode the response", err))
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	w.Write(append(b, '\n'))
}

// writeCodeError writes err's contract error, or internal for any other.
func writeCodeError(w http.ResponseWriter, err error) {
	var ce *contract.Error
	if !errors.As(err, &ce) {
		ce = contract.Wrap(contract.CodeInternal, "internal error", err)
	}
	writeError(w, ce)
}

// handleCA serves GET /api/v1/ca: the public CA certificate PEM, never a
// key. It needs no protocol header: bootstrap retrieves trust only.
func (s *nodeService) handleCA(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, contract.New(contract.CodeInvalidArgument, "method not allowed; use GET"))
		return
	}
	w.Header().Set("Content-Type", "application/x-pem-file")
	w.WriteHeader(http.StatusOK)
	w.Write(s.caPEM)
}

// checkVersion validates the request's protocol header, sets the
// response's, and writes the error itself when it fails. A mismatch is
// logged once, here at the request boundary.
func (s *nodeService) checkVersion(w http.ResponseWriter, r *http.Request) bool {
	w.Header().Set(contract.ProtocolHeader, strconv.Itoa(contract.ProtocolVersion))
	vals := r.Header.Values(contract.ProtocolHeader)
	if len(vals) != 1 {
		writeError(w, contract.New(contract.CodeInvalidArgument, "the request must carry exactly one "+contract.ProtocolHeader+" header"))
		return false
	}
	n, ok := contract.ParseInteger(strings.TrimSpace(vals[0]))
	if !ok {
		writeError(w, contract.New(contract.CodeInvalidArgument, "the "+contract.ProtocolHeader+" header must be an integer"))
		return false
	}
	if n != contract.ProtocolVersion {
		s.logger.Warn("protocol version mismatch", "local_version", contract.ProtocolVersion, "remote_version", n, "path", r.URL.Path)
		writeError(w, contract.VersionMismatch(contract.ProtocolVersion, n))
		return false
	}
	return true
}

// handleNodes serves the versioned roster and enrollment routes. The
// version header is checked first; query strings and other methods are
// invalid_argument; no request content is echoed.
func (s *nodeService) handleNodes(w http.ResponseWriter, r *http.Request) {
	if !s.checkVersion(w, r) {
		return
	}
	if r.URL.RawQuery != "" || r.URL.ForceQuery {
		writeError(w, contract.New(contract.CodeInvalidArgument, "query strings are not accepted"))
		return
	}
	switch path := r.URL.Path; {
	case path == contract.PathEnroll:
		if r.Method != http.MethodPost {
			writeError(w, contract.New(contract.CodeInvalidArgument, "method not allowed; use POST"))
			return
		}
		s.enroll(w, r)
	case r.Method != http.MethodGet:
		writeError(w, contract.New(contract.CodeInvalidArgument, "method not allowed; use GET"))
	case path == contract.PathNodes:
		writeJSON(w, http.StatusOK, contract.NodeListResponse{Version: contract.ProtocolVersion, Nodes: s.reg.Snapshot()})
	default:
		id := strings.TrimPrefix(path, contract.PathNodes+"/")
		if !contract.ValidNodeID(id) {
			writeError(w, contract.New(contract.CodeInvalidArgument, "invalid node ID; want n_ followed by 32 lowercase hex digits"))
			return
		}
		n, err := s.reg.Show(id)
		if err != nil {
			writeCodeError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, contract.NodeResponse{Version: contract.ProtocolVersion, Node: n})
	}
}

// enroll serves POST /api/v1/nodes/enroll: 201 for a new node, 200 with
// the unchanged record for a known one.
func (s *nodeService) enroll(w http.ResponseWriter, r *http.Request) {
	b, err := io.ReadAll(http.MaxBytesReader(w, r.Body, contract.MaxEnrollRequestBytes))
	if err != nil {
		writeError(w, contract.New(contract.CodeInvalidArgument, "the enrollment request is unreadable or larger than "+strconv.Itoa(contract.MaxEnrollRequestBytes)+" bytes"))
		return
	}
	req, err := contract.ParseEnrollRequest(b)
	if err != nil {
		writeCodeError(w, err)
		return
	}
	n, created, err := s.reg.Enroll(r.Context(), req.NodeID)
	if err != nil {
		s.logger.Warn("node enrollment failed", "node_id", req.NodeID, "error", err)
		writeCodeError(w, err)
		return
	}
	status := http.StatusOK
	if created {
		status = http.StatusCreated
		s.logger.Info("node enrolled", "node_id", req.NodeID)
	}
	writeJSON(w, status, contract.NodeResponse{Version: contract.ProtocolVersion, Node: n})
}
