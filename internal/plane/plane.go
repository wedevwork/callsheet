// Package plane implements the plane's local trust foundation: the private
// state directory, the internal certificate authority and server
// certificate, the private HTTPS listener and offline status inspection;
// since iteration 03 also the node registry, leases, the node stream and
// the read-only roster API.
//
// The four public operations (Init, Run, Reissue, Inspect) use the real
// clock, entropy, interface enumeration, filesystem and listener. Tests use
// the package-private deps seams to inject clocks and failures; no
// production testing flag exists.
package plane

import (
	"context"
	"crypto/rand"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"os"
	"time"

	"github.com/wedevwork/callsheet/internal/contract"
)

// InitOptions configures Init. StateDir must be an absolute, resolved path
// (see ResolveStateDir). BindSet and SANsSet record whether the operator
// supplied --bind and --san at all.
type InitOptions struct {
	StateDir string
	Bind     string
	BindSet  bool
	SANs     []string
	SANsSet  bool
}

// RunOptions configures Run. On empty state Run performs exactly Init with
// the bootstrap inputs, then serves. Logger receives the structured
// warning and listening records; nil discards them.
type RunOptions struct {
	StateDir string
	Bind     string
	BindSet  bool
	SANs     []string
	SANsSet  bool
	Logger   *slog.Logger
}

// ReissueOptions configures Reissue: SANs is the complete replacement list.
type ReissueOptions struct {
	StateDir string
	SANs     []string
}

// Status is the offline view of initialized plane state. Paths are
// absolute. Warnings are rendered on stderr by the CLI, never as a stdout
// field.
type Status struct {
	StateDir        string    `json:"state_dir"`
	Bind            string    `json:"bind"`
	CACertPath      string    `json:"ca_cert_path"`
	CAKeyPath       string    `json:"ca_key_path"`
	ServerCertPath  string    `json:"server_cert_path"`
	ServerKeyPath   string    `json:"server_key_path"`
	DNSNames        []string  `json:"dns_names"`
	IPAddresses     []string  `json:"ip_addresses"`
	CAFingerprint   string    `json:"ca_fingerprint"`
	CANotBefore     time.Time `json:"ca_not_before"`
	CANotAfter      time.Time `json:"ca_not_after"`
	ServerNotBefore time.Time `json:"server_not_before"`
	ServerNotAfter  time.Time `json:"server_not_after"`
	Warnings        []Warning `json:"warnings"`
}

// Warning is one certificate time condition: Certificate is "ca" or
// "server"; Condition is ConditionNotYetValid, ConditionExpired or
// ConditionExpiresSoon; At is NotBefore for not-yet-valid, else NotAfter.
type Warning struct {
	Certificate string
	Condition   string
	At          time.Time
}

// Warning conditions, in precedence order.
const (
	ConditionNotYetValid = "not yet valid"
	ConditionExpired     = "expired"
	ConditionExpiresSoon = "expires soon"
)

// deps are the injectable dependencies of every operation.
type deps struct {
	// now is sampled once per operation.
	now func() time.Time
	// rand is the entropy source for serials, keys and signatures.
	rand io.Reader
	// addrs enumerates local interface addresses.
	addrs func() ([]netip.Addr, error)
	// fail, when non-nil, is consulted before each state filesystem
	// boundary (op is create, write, sync, close, publish, rename or
	// dirsync; name is the root-relative managed path) and returns an
	// injected error.
	fail func(op, name string) error
	// listen creates the TCP listener.
	listen func(network, address string) (net.Listener, error)
	// shutdownTimeout bounds graceful shutdown before Close.
	shutdownTimeout time.Duration
	// ready, when non-nil, is called with the bound address after the
	// listening record and before serving.
	ready func(net.Addr)
	// wrap, when non-nil, wraps the service handler (tests only).
	wrap func(http.Handler) http.Handler
	// fileSync and rawFsync sync an open directory: fileSync first, then
	// rawFsync as the fallback for ENOTSUP, ENOTTY and EINVAL.
	fileSync func(*os.File) error
	rawFsync func(*os.File) error
	// nodeClock drives leases, the sweep and node stream timeouts
	// (iteration 03).
	nodeClock nodeClock
	// streamCloseGrace bounds a graceful node stream close.
	streamCloseGrace time.Duration
	// streamEvents, when non-nil, observes node stream lifecycles (tests
	// only).
	streamEvents func(string)
	// streamHelloRead, when non-nil, runs after a hello read returned a
	// frame, before its deadline is released (tests only).
	streamHelloRead func(context.Context)
}

// shutdownTimeout is the production graceful-shutdown bound.
const shutdownTimeout = 5 * time.Second

func defaultDeps() *deps {
	return &deps{
		now:              time.Now,
		rand:             rand.Reader,
		addrs:            interfaceAddrs,
		listen:           net.Listen,
		shutdownTimeout:  shutdownTimeout,
		fileSync:         (*os.File).Sync,
		rawFsync:         rawFsync,
		nodeClock:        realClock{},
		streamCloseGrace: streamCloseGrace,
	}
}

// Init creates plane state and trust on empty state, or validates and
// reports existing state without rewriting any durable file.
func Init(ctx context.Context, o InitOptions) (Status, error) { return defaultDeps().init(ctx, o) }

// Run serves HTTPS on the configured private bind until ctx ends, then
// shuts down and returns ctx's error.
func Run(ctx context.Context, o RunOptions) error { return defaultDeps().run(ctx, o) }

// Reissue replaces only the server certificate, under the unchanged CA and
// server key, with the given complete SAN list.
func Reissue(ctx context.Context, o ReissueOptions) (Status, error) {
	return defaultDeps().reissue(ctx, o)
}

// Inspect reports initialized state offline: no lock, network or writes.
func Inspect(ctx context.Context, stateDir string) (Status, error) {
	return defaultDeps().inspect(ctx, stateDir)
}

// clock samples the injected clock once, in UTC, truncated to seconds.
func (d *deps) clock() time.Time { return d.now().UTC().Truncate(time.Second) }

func (d *deps) hook(op, name string) error {
	if d.fail == nil {
		return nil
	}
	return d.fail(op, name)
}

func errf(code contract.Code, format string, args ...any) *contract.Error {
	return contract.New(code, fmt.Sprintf(format, args...))
}

func wrapf(code contract.Code, cause error, format string, args ...any) *contract.Error {
	return contract.Wrap(code, fmt.Sprintf(format, args...), cause)
}

// canceled reports ctx's error unchanged so callers can map it to exit 130.
func canceled(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	return nil
}
