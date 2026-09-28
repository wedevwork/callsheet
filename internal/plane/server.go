package plane

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/wedevwork/callsheet/internal/contract"
)

// HealthPath proves the real TLS listener and exposes no state. Since
// iteration 03 the service also serves the node API (nodeService), since
// iteration 04 the role API (roleService) and since iteration 05 the task
// API (taskService).
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
	reg, err := loadNodeRegistry(layout{root: o.StateDir}, d.nodeClock)
	if err != nil {
		return err
	}
	reg.d = d
	doc, err := layout{root: o.StateDir}.loadRoleDoc(roleLookup, reg.ids())
	if err != nil {
		return err
	}
	reg.roles = newRoleRegistry(layout{root: o.StateDir}, d, roleLookup, doc)
	reg.roles.adopt = reg.adoptRoles
	// Tasks (iteration 05): every document is validated and nonterminal
	// reservations are rebuilt under their historical instances before
	// listening; no start is replayed.
	loaded, err := layout{root: o.StateDir}.loadTasks(roleLookup, doc, reg.ids())
	if err != nil {
		return err
	}
	// Migration (iteration 06a): schema-1 nonterminal records are resolved
	// lost, one file at a time, under the state lock and before listening.
	store := &taskStore{l: layout{root: o.StateDir}, d: d}
	loaded, err = store.migrate(loaded, d.nodeClock.Now(), func(id string) {
		logger.Warn("legacy task resolved lost", "task_id", id, "reason", contract.ReasonLegacyUnrecoverable)
	})
	if err != nil {
		return err
	}
	ts, err := newTaskService(store, reg, reg.roles, &atomic.Bool{}, d, logger, loaded)
	if err != nil {
		return err
	}
	ts.events, ts.hook = d.streamEvents, d.taskHook
	reg.tasks = ts
	if d.onTasks != nil {
		d.onTasks(ts)
	}
	if err := canceled(ctx); err != nil {
		return err
	}
	return d.serve(ctx, logger, m, fp, reg)
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
// ends or Serve fails. Either way it first shuts the node service down
// (no new streams, every upgraded socket closed and its handler joined,
// the sweep joined), then shuts the HTTP server down (graceful Shutdown
// bounded by what remains of shutdownTimeout, then Close), and joins Serve
// and every in-flight handler before returning. The caller keeps the state
// lock until then.
func (d *deps) serve(ctx context.Context, logger *slog.Logger, m *material, fp string, reg *nodeRegistry) error {
	ln, err := d.listen("tcp", m.bind.String())
	if err != nil {
		return wrapf(contract.CodeUnavailable, err, "cannot listen on %s: %v", m.bind, err)
	}
	// The chain is leaf then CA, so a pinned bootstrap can find the CA in
	// the handshake; the files on disk are unchanged.
	cert := tls.Certificate{Certificate: [][]byte{m.serverCert.Raw, m.caCert.Raw}, PrivateKey: m.serverKey, Leaf: m.serverCert}
	tlsLn := tls.NewListener(ln, &tls.Config{MinVersion: tls.VersionTLS12, Certificates: []tls.Certificate{cert}})
	svc := newNodeService(reg, d.nodeClock, logger, certPEM(m.caCert.Raw), d.streamCloseGrace)
	svc.events = d.streamEvents
	svc.helloRead = d.streamHelloRead
	if reg.roles != nil {
		svc.roles = newRoleService(reg.roles, reg, d.nodeClock, logger)
		svc.roles.events = d.streamEvents
		svc.roles.hook = d.roleHook
	}
	if reg.tasks != nil {
		svc.tasks = reg.tasks
		if svc.roles != nil {
			svc.roles.gate, svc.roles.tasks = reg.tasks.gate, reg.tasks
		}
	}
	var h http.Handler = newServiceHandler(svc)
	if d.wrap != nil {
		h = d.wrap(h)
	}
	tr := newTracker()
	srv := &http.Server{
		Handler:           tr.wrap(h),
		ConnContext:       func(ctx context.Context, c net.Conn) context.Context { return context.WithValue(ctx, connKey{}, c) },
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
	if reg.tasks != nil {
		// Iteration 06a: the loss worker runs, and each node holding loaded
		// nonterminal tasks gets its one startup reconciliation grace,
		// armed at listener readiness immediately before connections are
		// accepted (slow state loading never consumes it).
		reg.tasks.start()
		reg.armGrace(d.nodeClock.Now(), reg.tasks.graceNodes())
		reg.tasks.event("startup-grace-armed")
	}
	svc.startSweep()
	done := make(chan error, 1)
	go func() { done <- srv.Serve(tlsLn) }()
	var serveErr error
	select {
	case <-ctx.Done():
		deadline := time.Now().Add(d.shutdownTimeout)
		svc.shutdown(deadline)
		sctx, cancel := context.WithDeadline(context.Background(), deadline)
		if err := srv.Shutdown(sctx); err != nil {
			srv.Close()
		}
		cancel()
		serveErr = <-done
	case serveErr = <-done:
		svc.shutdown(time.Now().Add(d.shutdownTimeout))
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

// serviceHandler is the health-only service (no node service).
func serviceHandler() http.Handler { return newServiceHandler(nil) }

// newServiceHandler serves GET /api/v1/health, the node API when svc is
// non-nil, and contract JSON errors for everything else. The health route
// never reads or echoes a request body.
func newServiceHandler(svc *nodeService) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := r.URL.Path
		switch {
		case p == HealthPath:
			if r.Method != http.MethodGet {
				writeError(w, contract.New(contract.CodeInvalidArgument, "method not allowed; use GET"))
				return
			}
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			io.WriteString(w, healthBody)
		case svc == nil:
			writeError(w, contract.New(contract.CodeNotFound, "no such endpoint"))
		case p == contract.PathCA:
			svc.handleCA(w, r)
		case p == contract.PathNodeStream:
			svc.handleStream(w, r)
		case p == contract.PathNodes || strings.HasPrefix(p, contract.PathNodes+"/"):
			svc.handleNodes(w, r)
		case svc.roles != nil && (p == contract.PathRoles || strings.HasPrefix(p, contract.PathRoles+"/")):
			svc.handleRoles(w, r)
		case svc.tasks != nil && (p == contract.PathTasks || strings.HasPrefix(p, contract.PathTasks+"/")):
			svc.handleTasks(w, r)
		default:
			writeError(w, contract.New(contract.CodeNotFound, "no such endpoint"))
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
			// Node clients require the protocol header on every response.
			w.Header().Set(contract.ProtocolHeader, fmt.Sprint(contract.ProtocolVersion))
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
