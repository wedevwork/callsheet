package plane

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/wedevwork/callsheet/internal/contract"
)

// HealthPath is the only route: it proves the real TLS listener and
// exposes no state.
const HealthPath = "/api/v1/health"

// healthBody is the exact health response.
var healthBody = fmt.Sprintf("{\"status\":\"ok\",\"version\":%d}\n", contract.ProtocolVersion)

// HTTP server bounds.
const (
	readHeaderTimeout = 5 * time.Second
	readTimeout       = 10 * time.Second
	writeTimeout      = 10 * time.Second
	idleTimeout       = 30 * time.Second
	maxHeaderBytes    = 16 << 10
)

// run owns the state for the whole service lifetime: lock, bootstrap
// initialization, validation, warnings, bind check, listener and shutdown.
func (d *deps) run(ctx context.Context, o RunOptions) error {
	logger := o.Logger
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}
	now := d.clock()
	lk, m, initialized, err := d.open(ctx, now, o.StateDir, o.Bind, o.BindSet, o.SANs, o.SANsSet)
	if err != nil {
		return err
	}
	defer lk.release()
	fp := Fingerprint(m.caCert.Raw)
	if initialized {
		logger.Info("initialized", "state_dir", o.StateDir, "ca_fingerprint", fp)
	}
	for _, w := range m.warnings(now) {
		logWarning(logger, w)
	}
	if err := m.verifyAt(now); err != nil {
		return err
	}
	if err := d.checkPresent(m.bind); err != nil {
		return err
	}
	if err := canceled(ctx); err != nil {
		return err
	}
	return d.serve(ctx, logger, m, fp)
}

// logWarning emits the structured FP-7 warning record.
func logWarning(logger *slog.Logger, w Warning) {
	key := "expires_at"
	if w.Condition == ConditionNotYetValid {
		key = "valid_from"
	}
	logger.Warn(w.Condition, "certificate", w.Certificate, key, rfc3339(w.At))
}

// serve creates the single TCP listener wrapped in TLS and serves until ctx
// ends (graceful Shutdown bounded by shutdownTimeout, then Close) or Serve
// fails. It joins Serve and every in-flight handler before returning.
func (d *deps) serve(ctx context.Context, logger *slog.Logger, m *material, fp string) error {
	ln, err := d.listen("tcp", m.bind.String())
	if err != nil {
		return wrapf(contract.CodeUnavailable, err, "cannot listen on %s: %v", m.bind, err)
	}
	cert := tls.Certificate{Certificate: [][]byte{m.serverCert.Raw}, PrivateKey: m.serverKey, Leaf: m.serverCert}
	tlsLn := tls.NewListener(ln, &tls.Config{MinVersion: tls.VersionTLS12, Certificates: []tls.Certificate{cert}})
	var h http.Handler = serviceHandler()
	if d.wrap != nil {
		h = d.wrap(h)
	}
	tr := newTracker()
	srv := &http.Server{
		Handler:           tr.wrap(h),
		ReadHeaderTimeout: readHeaderTimeout,
		ReadTimeout:       readTimeout,
		WriteTimeout:      writeTimeout,
		IdleTimeout:       idleTimeout,
		MaxHeaderBytes:    maxHeaderBytes,
		// Handshake and request errors can carry peer data; they are
		// discarded, and startup/serve errors are reported once by the CLI.
		ErrorLog: log.New(io.Discard, "", 0),
	}
	logger.Info("listening", "bind", ln.Addr().String(), "ca_fingerprint", fp)
	if d.ready != nil {
		d.ready(ln.Addr())
	}
	done := make(chan error, 1)
	go func() { done <- srv.Serve(tlsLn) }()
	var serveErr error
	select {
	case <-ctx.Done():
		sctx, cancel := context.WithTimeout(context.Background(), d.shutdownTimeout)
		if err := srv.Shutdown(sctx); err != nil {
			srv.Close()
		}
		cancel()
		serveErr = <-done
	case serveErr = <-done:
		srv.Close()
	}
	tr.closeAndWait()
	// Cancellation wins whenever it happened, independent of which of the
	// two events the select observed first.
	if err := ctx.Err(); err != nil {
		return err
	}
	return wrapf(contract.CodeUnavailable, serveErr, "the plane listener stopped unexpectedly: %v", serveErr)
}

// serviceHandler serves GET /api/v1/health and contract JSON errors for
// everything else. It never reads or echoes a request body.
func serviceHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path != HealthPath:
			writeError(w, contract.New(contract.CodeNotFound, "no such endpoint"))
		case r.Method != http.MethodGet:
			writeError(w, contract.New(contract.CodeInvalidArgument, "method not allowed; use GET"))
		default:
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			io.WriteString(w, healthBody)
		}
	})
}

func writeError(w http.ResponseWriter, e *contract.Error) {
	b, _ := json.Marshal(e)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(contract.HTTPStatus(e.Code))
	w.Write(append(b, '\n'))
}

// tracker counts in-flight handlers so shutdown can join them; once
// closed it admits no new handler.
type tracker struct {
	mu     sync.Mutex
	n      int
	closed bool
	idle   chan struct{}
}

func newTracker() *tracker { return &tracker{idle: make(chan struct{})} }

func (t *tracker) wrap(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !t.enter() {
			writeError(w, contract.New(contract.CodeUnavailable, "the plane is shutting down"))
			return
		}
		defer t.leave()
		h.ServeHTTP(w, r)
	})
}

func (t *tracker) enter() bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.closed {
		return false
	}
	t.n++
	return true
}

func (t *tracker) leave() {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.n--
	if t.closed && t.n == 0 {
		close(t.idle)
	}
}

// closeAndWait stops admitting handlers and waits for running ones.
func (t *tracker) closeAndWait() {
	t.mu.Lock()
	t.closed = true
	wait := t.n > 0
	t.mu.Unlock()
	if wait {
		<-t.idle
	}
}
