// Package sidecar is the node side of iteration 03: enrollment with a
// plane over verified TLS, a stable node identity on disk, and the
// outbound-only supervisor connection (version hello, heartbeats and
// bounded reconnect). The sidecar creates no listener.
//
// The public operations use the real clock, entropy, filesystem and
// verified client. Tests inject clocks, jitter and failures through the
// package-private deps; no production testing flag exists.
package sidecar

import (
	"context"
	"crypto/rand"
	"io"
	"log/slog"
	mrand "math/rand/v2"
	"os"
	"time"

	"github.com/coder/websocket"

	"github.com/wedevwork/callsheet/internal/client"
	"github.com/wedevwork/callsheet/internal/contract"
)

// EnrollOptions configures Enroll. StateDir is resolved (ResolveStateDir);
// exactly one of CAFile and CAFingerprint is set by a valid invocation;
// SoftwareVersion is the build version sent to the plane.
type EnrollOptions struct {
	StateDir        string
	PlaneURL        string
	CAFile          string
	CAFingerprint   string
	SoftwareVersion string
}

// Enrollment is the durable result of a successful Enroll.
type Enrollment struct {
	NodeID        string
	PlaneURL      string
	CAFingerprint string
}

// RunOptions configures Run: the resolved state root, the build version
// sent in hello, and the logger for connection diagnostics (nil discards).
type RunOptions struct {
	StateDir        string
	SoftwareVersion string
	Logger          *slog.Logger
}

// planeClient is the verified client surface the sidecar uses.
type planeClient interface {
	EnrollNode(ctx context.Context, id, softwareVersion string) (contract.Node, error)
	DialNodeStream(ctx context.Context) (*websocket.Conn, error)
	Close()
}

// eventKind names an observable transition (tests only).
type eventKind string

const (
	evStarted   eventKind = "started"
	evDialed    eventKind = "dialed"
	evConnected eventKind = "connected"
	evAck       eventKind = "ack"
	// evAwaitReply: the request's write returned and the session now waits
	// for its reply: hello_ok when acks is 0, else heartbeat number acks's
	// acknowledgement.
	evAwaitReply eventKind = "await-reply"
	evStable     eventKind = "stable"
	evEnded      eventKind = "ended"
	evBackoff    eventKind = "backoff"
)

// event is one observed transition: the session number (1-based), the
// acknowledged heartbeat count, a scheduled delay or the ending error.
type event struct {
	kind    eventKind
	session int
	acks    int
	delay   time.Duration
	err     error
}

// deps are the injectable dependencies of Enroll and Run.
type deps struct {
	clock clock
	// rand is the entropy source for node IDs.
	rand io.Reader
	// jitter returns a uniform sample in [0, 1) for backoff jitter.
	jitter func() float64
	// fail, when non-nil, is consulted before each state filesystem
	// boundary (op is create, write, sync, close, publish, rename or
	// dirsync; name is the state-relative path) and returns an injected
	// error.
	fail     func(op, name string) error
	fileSync func(*os.File) error
	rawFsync func(*os.File) error
	// resolveTrust and newClient are the verified client seams.
	resolveTrust func(ctx context.Context, o client.TrustOptions) (client.Trust, error)
	newClient    func(planeURL string, trust client.Trust) (planeClient, error)
	// closeGrace bounds a graceful WebSocket close.
	closeGrace time.Duration
	// observe, when non-nil, receives every transition (tests only).
	observe func(event)
}

// closeGrace is the production bound of a graceful stream close.
const closeGrace = time.Second

func defaultDeps() *deps {
	return &deps{
		clock:        realClock{},
		rand:         rand.Reader,
		jitter:       mrand.Float64,
		fileSync:     (*os.File).Sync,
		rawFsync:     rawFsync,
		resolveTrust: client.ResolveTrust,
		newClient: func(u string, t client.Trust) (planeClient, error) {
			return client.New(u, t)
		},
		closeGrace: closeGrace,
	}
}

// Enroll establishes trust (a verified connection), then under the state
// lock publishes the identity if absent, registers it with the plane and
// atomically records the plane URL and CA. It never starts Run.
func Enroll(ctx context.Context, o EnrollOptions) (Enrollment, error) {
	return defaultDeps().enroll(ctx, o)
}

// Run holds the state lock and the outbound supervisor connection until
// ctx ends, reconnecting with bounded backoff whenever the plane goes
// away. It returns ctx's error after cleanup, or a terminal configuration
// error (protocol mismatch, invalid protocol, unknown node, trust).
func Run(ctx context.Context, o RunOptions) error { return defaultDeps().run(ctx, o) }

func (d *deps) emit(e event) {
	if d.observe != nil {
		d.observe(e)
	}
}
