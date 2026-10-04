// Package sidecar is the node side of iteration 03: enrollment with a
// plane over verified TLS, a stable node identity on disk, and the
// outbound-only supervisor connection (version hello, heartbeats and
// bounded reconnect). Since iteration 04 the connection is duplex: the
// sidecar validates candidate roles locally (manuals and the adapter
// executable never leave the node), installs the plane's role snapshots and
// reports per-role readiness from periodic ready checks in its heartbeats.
// The sidecar creates no listener.
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

	"github.com/wedevwork/callsheet/internal/adapter"
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
// sent in hello, the logger for connection diagnostics (nil discards),
// optionally the fake adapter's absolute executable path (iteration 04):
// empty disables the fake adapter; it is local process configuration,
// never persisted and never sent to the plane; and the host OS for task
// execution (iteration 05).
type RunOptions struct {
	StateDir        string
	SoftwareVersion string
	Logger          *slog.Logger
	FakeAdapterPath string
	// ClaudeAdapterPath and CodexAdapterPath (iteration 08) enable the real
	// vendor adapters with explicit absolute executable paths, used
	// literally (spaces allowed), never persisted or sent to the plane;
	// empty disables that vendor on this node whatever is installed.
	ClaudeAdapterPath string
	CodexAdapterPath  string
	// GOOS is the host OS the CLI's runtime entrypoint supplies
	// (iteration 05): task execution decisions take it explicitly; an
	// empty or unsupported value refuses every task start.
	GOOS string
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
	// Iteration 04 role events.
	// evInstalled: a snapshot was installed (rev) and its ack queued.
	evInstalled eventKind = "installed"
	// evAckWritten: the snapshot acknowledgement write completed.
	evAckWritten eventKind = "ack-written"
	// evValidating: a validation's worker started.
	evValidating eventKind = "validating"
	// evReplied: a validation result write completed.
	evReplied eventKind = "replied"
	// evCycleStarted and evCycleDone: a ready-check cycle started, and a
	// cycle result (passed) was published for rev.
	evCycleStarted eventKind = "cycle-started"
	evCycleDone    eventKind = "cycle-done"
	// evValidated and evChecked: a validation worker, or a cycle worker,
	// returned (its slot released and its result published).
	evValidated eventKind = "validated"
	evChecked   eventKind = "checked"
	// Iteration 05 task events (id is the task ID, or a request ID for
	// the write-completion events).
	evTaskPreparing  eventKind = "task-preparing"
	evTaskAuthorized eventKind = "task-authorized"
	evChildStarted   eventKind = "child-started"
	evTaskRefused    eventKind = "task-refused"
	evChildExited    eventKind = "child-exited"
	evTaskExited     eventKind = "task-exited"
	evTaskFenced     eventKind = "task-fenced"
	// evTaskCollected: the supervisor forgot a finished worker (its
	// goroutine completed and its result can no longer be sent).
	evTaskCollected eventKind = "task-collected"
	// evSupervisorClosing: Run's supervisor shutdown finished its scan of
	// the workers (no launch is authorized after it) and now waits.
	evSupervisorClosing eventKind = "supervisor-closing"
	// evStartReplied: a task_start_result write completed (id = task ID).
	evStartReplied eventKind = "start-replied"
	// evOutputWritten: a task_log or task_result write completed (id =
	// request ID); evLogAcked and evResultAcked: its receipt was
	// acknowledged (id = task ID).
	evOutputWritten eventKind = "output-written"
	evLogAcked      eventKind = "log-acked"
	evResultAcked   eventKind = "result-acked"
	// Iteration 06a events: a result's committed acknowledgement (id =
	// task ID); a journal deleted after its commit (id = task ID); an
	// inventory page's acknowledgement (rev = page); a reconcile page
	// applied (id = request ID) and the attachment reconciled; a
	// reconciliation disposition applied to a task (id = task ID,
	// err = nil; the action in rev: see actionCode); a recovered
	// execution's cleanup confirmed or blocked (id = task ID); the
	// supervisor's recovery pass finished.
	evResultCommitted   eventKind = "result-committed"
	evTaskForgotten     eventKind = "task-forgotten"
	evInventoryAcked    eventKind = "inventory-acked"
	evReconciled        eventKind = "reconciled"
	evDisposed          eventKind = "disposed"
	evCleanupConfirmed  eventKind = "cleanup-confirmed"
	evCleanupBlocked    eventKind = "cleanup-blocked"
	evRecoveryDone      eventKind = "recovery-done"
	evPreparationFenced eventKind = "preparation-fenced"
	// Iteration 06b control events (id = task ID): a plane stop intent
	// latched on a worker, journaled, sent to its guardian; the guardian's
	// stopping cause received; the group's disappearance confirmed; a
	// task_cancel acknowledged (id = request ID).
	evControlLatched  eventKind = "control-latched"
	evIntentJournaled eventKind = "intent-journaled"
	evControlSent     eventKind = "control-sent"
	evStopping        eventKind = "stopping"
	evGroupGone       eventKind = "group-gone"
	evCancelAcked     eventKind = "cancel-acked"
	// Iteration 10b workspace events (id = task ID): the preparation and
	// finalization timers armed, the checkout prepared, a task_prepared
	// written (id = request ID) and its release decision received, a
	// publication stage (action = the stage), a failed work directory
	// removal holding its slot, and that slot's release by the janitor.
	evPrepArmed         eventKind = "prep-armed"
	evFinalizeArmed     eventKind = "finalize-armed"
	evWorkspacePrepared eventKind = "workspace-prepared"
	evPreparedAcked     eventKind = "prepared-acked"
	evPublication       eventKind = "publication"
	evWorkPending       eventKind = "work-pending"
	evWorkReleased      eventKind = "work-released"
	// evHeartbeatArmed: the periodic heartbeat timer was registered for
	// instant at, with no exchange outstanding after request number acks
	// (a fake clock advances only after its target timer is armed).
	evHeartbeatArmed eventKind = "heartbeat-armed"
)

// event is one observed transition: the session number (1-based), the
// acknowledged heartbeat count, a scheduled delay or the ending error.
type event struct {
	kind    eventKind
	session int
	acks    int
	delay   time.Duration
	err     error
	// Role events: the snapshot revision, plane request ID, a heartbeat's
	// statuses and a cycle's per-role results.
	rev      int
	id       string
	statuses []contract.RoleStatus
	passed   []bool
	// action is a disposition's reconcile action (evDisposed).
	action string
	// at is a ready-check cycle's start instant (evCycleDone) or a
	// heartbeat's send instant (evAwaitReply).
	at time.Time
	// prompt marks an immediate readiness report (iteration 10a,
	// evAwaitReply), as opposed to a periodic heartbeat.
	prompt bool
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
	// adapters builds the worker's adapter registry for a state root
	// (iteration 04); openManual opens a manual without blocking; and
	// observeCheck, when non-nil, sees every manual check and probe
	// (tests only).
	adapters     func(dir string) adapter.Registry
	openManual   func(p string) (*os.File, error)
	observeCheck func(kind, name string)
	// Iteration 05 task seams (nil: production), guardian-backed since
	// 06a: the guardian factory, the process-group watcher, the control
	// command sender, the guardian executable (the callsheet binary), the
	// child environment source and the scratch temp root; noCadence
	// disables the periodic ready-check cadence (a snapshot's first cycle
	// still runs).
	taskGuardians guardianFactory
	taskGroups    groupWatcher
	taskCommand   commandSender
	guardianExe   func() (string, error)
	taskEnviron   func() []string
	taskTempDir   func() string
	noCadence     bool
	// ownGroup is this process's group (the guardian's must differ).
	ownGroup func() int
	// taskRingCap, when positive, replaces the 10 MiB retained-output
	// cap (tests exercising eviction through a session).
	taskRingCap int
	// onSupervisor, when non-nil, receives Run's task supervisor (tests
	// only).
	onSupervisor func(*taskSupervisor)
	// taskAuthHook, when non-nil, runs at a worker's launch authorization
	// point with the worker's lock held; taskGCHook runs where worker
	// collection is about to inspect workers (tests only: lock-order
	// barriers).
	taskAuthHook func(id string)
	taskGCHook   func()
	// taskDrainHook, when non-nil, runs at the start of each of a
	// supervised execution's pipe drains, before it reads (tests only: a
	// drain scheduled late).
	taskDrainHook func()
	// taskFinalRead, when non-nil, replaces the final-file reader
	// (iteration 08; tests injecting read faults).
	taskFinalRead func(*finalSource, adapter.FinalExtractor) (adapter.FinalMessage, error)
	// taskWorkspacePlane, when non-nil, replaces the workspace execution's
	// plane surface (iteration 10b; tests).
	taskWorkspacePlane wsPlane
	// heartbeatHook, when non-nil, runs on the session goroutine after
	// each heartbeat's write, with whether it was a prompt report and the
	// session's inbox (tests only: an acknowledgement queued before the
	// session resumes).
	heartbeatHook func(prompt bool, in *inbox)
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
		adapters:   adapter.Builtin,
		openManual: openManual,
		ownGroup:   ownProcessGroup,
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
