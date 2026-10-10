package sidecar

import (
	"context"
	"errors"
	"io"
	"io/fs"
	"os"
	"sync"
	"syscall"
	"time"

	"github.com/wedevwork/callsheet/internal/adapter"
	"github.com/wedevwork/callsheet/internal/contract"
)

// Role checks on the worker (iteration 04).
const (
	// validationBudget bounds one registration validation from receipt.
	validationBudget = 2 * time.Second
	// cycleBudget bounds one periodic ready-check cycle from its start.
	cycleBudget = 2 * time.Second
	// cycleInterval separates cycle starts.
	cycleInterval = 5 * time.Second
	// freshness is how long a passed cycle keeps a role ready.
	freshness = 10 * time.Second
)

// FakeAdapterWarning is the fixed warning logged when a sidecar starts with
// the fake adapter enabled.
const FakeAdapterWarning = "fake adapter enabled: test/demo adapter; never calls a model"

// DarwinVendorWarning is the fixed warning logged once when a sidecar on
// macOS starts with the Claude or Codex adapter enabled (iteration 08).
const DarwinVendorWarning = "claude/codex adapter enabled on macOS: the worker recipe is Linux-qualified and macOS vendor sandbox and exit behavior are UNVERIFIED; consult the support catalog before production use"

// CursorVendorWarning is the fixed warning logged once when a sidecar
// starts with the Cursor adapter enabled (iteration 11).
const CursorVendorWarning = "cursor adapter enabled for version probing only: unattended worker execution is refused because no qualified recipe preserves the operator posture; consult the support catalog"

// GrokVendorWarning is the fixed warning logged once when a sidecar starts
// with the Grok adapter enabled (iteration 11); on macOS the same single
// warning carries " " + GrokDarwinWarningSuffix.
const GrokVendorWarning = "grok adapter enabled: prompts are passed in argv and may be visible to process inspection; the composed prompt limit is 32 KiB; dontAsk cancelled all measured writes, including permitted writes; exit 0 does not prove requested work completed"

// GrokDarwinWarningSuffix completes GrokVendorWarning on macOS.
const GrokDarwinWarningSuffix = "grok worker execution on macOS is refused pending qualification; consult the support catalog"

// roleEnv is the worker's role configuration for one Run: the adapter
// registry, the enabled executables (adapter ID to absolute path), never
// sent to the plane, and (iteration 11) goos, the OS the worker posture is
// decided for: in production only Run's validated RunOptions.GOOS. Role
// checks and task preparation read it; task filesystem and platform
// operations keep the supervisor's native platform.
type roleEnv struct {
	adapters    adapter.Registry
	executables map[string]string
	goos        string
}

func (e roleEnv) lookup() contract.AdapterLookup { return adapter.ContractLookup(e.adapters) }

// checkError is a failed worker check: a safe message plus field and
// reason for the contract details; never file contents or child output.
func checkError(field, reason, format string, args ...any) *contract.Error {
	return contract.RoleError(contract.CodeInvalidArgument, "", "", field, reason, format, args...)
}

// manualCause maps a filesystem error to a fixed phrase.
func manualCause(err error) string {
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return "it does not exist (or is a broken symbolic link)"
	case errors.Is(err, fs.ErrPermission):
		return "permission denied"
	case errors.Is(err, syscall.ELOOP):
		return "too many levels of symbolic links"
	case errors.Is(err, syscall.ENOTDIR):
		return "a path component is not a directory"
	}
	return "it cannot be read"
}

// checkManual checks one manual: the native-resolved path must be a
// regular file; it is opened for read as this user without blocking (a
// FIFO swapped in after the stat cannot stall the check), the descriptor
// is checked again, and one byte is read (EOF counts: empty manuals are
// allowed). Nothing is parsed, kept or sent; every descriptor is closed.
func (d *deps) checkManual(ctx context.Context, field, p string) error {
	if d.observeCheck != nil {
		d.observeCheck("manual", p)
	}
	fi, err := os.Stat(p)
	if err != nil {
		return checkError(field, contract.ReasonManualUnreadable, "the %s manual is not readable on this node: %s", field, manualCause(err))
	}
	if !fi.Mode().IsRegular() {
		return checkError(field, contract.ReasonManualNotRegular, "the %s manual is not a regular file on this node", field)
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	f, err := d.openManual(p)
	if err != nil {
		return checkError(field, contract.ReasonManualUnreadable, "the %s manual is not readable on this node: %s", field, manualCause(err))
	}
	defer f.Close()
	fi, err = f.Stat()
	if err != nil {
		return checkError(field, contract.ReasonManualUnreadable, "the %s manual is not readable on this node: %s", field, manualCause(err))
	}
	if !fi.Mode().IsRegular() {
		return checkError(field, contract.ReasonManualNotRegular, "the %s manual is not a regular file on this node", field)
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	var b [1]byte
	if _, err := f.Read(b[:]); err != nil && !errors.Is(err, io.EOF) {
		return checkError(field, contract.ReasonManualUnreadable, "the %s manual is not readable on this node: %s", field, manualCause(err))
	}
	return nil
}

// openManual opens a manual for reading without blocking.
func openManual(p string) (*os.File, error) {
	return os.OpenFile(p, os.O_RDONLY|syscall.O_NONBLOCK, 0)
}

// probe runs the adapter's probe of its enabled executable, then
// (iteration 11) checks the worker posture for env.goos: a probe error
// keeps its precedence, and a refused posture (Cursor everywhere, Grok
// outside Linux) is invalid_argument, field adapter, reason probe_failed
// with the fixed posture text, so candidate validation and every
// ready-check cycle refuse it however the version matches.
func (d *deps) probe(ctx context.Context, env roleEnv, adapterID string) error {
	exe, ok := env.executables[adapterID]
	if !ok {
		return checkError("adapter", contract.ReasonAdapterDisabled, "%s adapter is disabled on node; start sidecar with --%s-adapter ABSOLUTE_PATH", adapterID, adapterID)
	}
	a, ok := env.adapters.Lookup(adapterID)
	if !ok {
		return checkError("adapter", contract.ReasonAdapterDisabled, "adapter %s is not registered on this node", adapterID)
	}
	if d.observeCheck != nil {
		d.observeCheck("probe", adapterID)
	}
	if err := a.Probe(ctx, exe); err != nil {
		if cerr := ctx.Err(); cerr != nil {
			return cerr
		}
		var pe *adapter.ProbeError
		reason := "the probe failed"
		if errors.As(err, &pe) {
			reason = pe.Error()
		}
		return checkError("adapter", contract.ReasonProbeFailed, "the %s adapter cannot be invoked on this node: %s", adapterID, reason)
	}
	if err := adapter.ValidateWorkerPosture(adapterID, env.goos); err != nil {
		return checkError("adapter", contract.ReasonProbeFailed, "%s", err.Error())
	}
	return nil
}

// checkCandidate is a registration validation's worker checks, in order:
// instruction, runbook, the model/effort selection (pure: the model
// grammar and the adapter's effort union, never a paid probe of vendor
// compatibility), then the adapter executable's minimum-version probe and
// the worker posture.
func (d *deps) checkCandidate(ctx context.Context, env roleEnv, c contract.RoleConfig) error {
	if err := d.checkManual(ctx, "instruction", c.Instruction); err != nil {
		return err
	}
	if err := d.checkManual(ctx, "runbook", c.Runbook); err != nil {
		return err
	}
	if err := checkSelection(c.Adapter, c.Model, c.Effort); err != nil {
		return err
	}
	return d.probe(ctx, env, c.Adapter)
}

// checkSelection maps a refused model/effort selection (an invalid model
// grammar or an effort outside the adapter's union) to the role error:
// invalid_argument, the offending field, reason probe_failed, the
// adapter's fixed safe message (never the submitted values). A valid
// selection says nothing about whether the vendor can run it.
func checkSelection(adapterID, model, effort string) error {
	err := adapter.ValidateSelection(adapterID, model, effort)
	if err == nil {
		return nil
	}
	var se *adapter.SelectionError
	field := "model"
	if errors.As(err, &se) {
		field = se.Field
	}
	return checkError(field, contract.ReasonProbeFailed, "%s", err.Error())
}

// checkCycle is one ready-check cycle over an installed snapshot: each
// distinct enabled adapter executable is probed once (its minimum-version
// policy and posture) and its result shared, so an old or malformed
// version makes every role of that adapter false; each role's selection is
// revalidated and its two manuals are checked. A failed selection or file
// check makes only that role false. Readiness tests neither vendor
// authentication, model existence nor pair compatibility.
func (d *deps) checkCycle(ctx context.Context, env roleEnv, roles []contract.RoleRecord) []bool {
	probes := map[string]bool{}
	for _, r := range roles {
		if _, done := probes[r.Adapter]; done {
			continue
		}
		if ctx.Err() != nil {
			return make([]bool, len(roles))
		}
		probes[r.Adapter] = d.probe(ctx, env, r.Adapter) == nil
	}
	out := make([]bool, len(roles))
	for i, r := range roles {
		if ctx.Err() != nil {
			return make([]bool, len(roles))
		}
		out[i] = probes[r.Adapter] && checkSelection(r.Adapter, r.Model, r.Effort) == nil &&
			d.checkManual(ctx, "instruction", r.Instruction) == nil && d.checkManual(ctx, "runbook", r.Runbook) == nil
	}
	return out
}

// workers are the Run's two worker slots: one registration validation and
// one ready-check cycle, never queued and never replaced behind a stuck
// call. A slot is released when its work returns, before the result is
// made available; freed signals a release to the current session.
type workers struct {
	mu        sync.Mutex
	valBusy   bool
	cycleBusy bool
	freed     chan struct{}
	wg        sync.WaitGroup
	// tasks is the Run's task supervisor (iteration 05).
	tasks *taskSupervisor
}

func newWorkers() *workers { return &workers{freed: make(chan struct{}, 1)} }

func (w *workers) release(slot *bool) {
	w.mu.Lock()
	*slot = false
	w.mu.Unlock()
	select {
	case w.freed <- struct{}{}:
	default:
	}
}

// job is one worker's result mailbox: the result and its completion
// instant, captured under the same lock that publishes them.
type job struct {
	cancel context.CancelFunc
	ready  chan struct{}

	mu     sync.Mutex
	done   bool
	at     time.Time
	err    error
	passed []bool
}

func (j *job) finish(c clock, err error, passed []bool) {
	j.mu.Lock()
	j.done, j.at, j.err, j.passed = true, c.Now(), err, passed
	j.mu.Unlock()
	select {
	case j.ready <- struct{}{}:
	default:
	}
}

// result returns the published result, if any.
func (j *job) result() (done bool, at time.Time, err error, passed []bool) {
	j.mu.Lock()
	defer j.mu.Unlock()
	return j.done, j.at, j.err, j.passed
}

// startValidation occupies the validation slot and checks c in a worker.
// It returns nil when the slot is busy.
func (d *deps) startValidation(parent context.Context, w *workers, env roleEnv, c contract.RoleConfig) *job {
	w.mu.Lock()
	if w.valBusy {
		w.mu.Unlock()
		return nil
	}
	w.valBusy = true
	w.mu.Unlock()
	ctx, cancel := context.WithCancel(parent)
	j := &job{cancel: cancel, ready: make(chan struct{}, 1)}
	w.wg.Add(1)
	go func() {
		defer w.wg.Done()
		err := d.checkCandidate(ctx, env, c)
		cancel()
		w.release(&w.valBusy)
		j.finish(d.clock, err, nil)
		d.emit(event{kind: evValidated, err: err})
	}()
	return j
}

// startCycle occupies the cycle slot and checks roles in a worker. It
// returns nil when the slot is busy.
func (d *deps) startCycle(parent context.Context, w *workers, env roleEnv, roles []contract.RoleRecord) *job {
	w.mu.Lock()
	if w.cycleBusy {
		w.mu.Unlock()
		return nil
	}
	w.cycleBusy = true
	w.mu.Unlock()
	ctx, cancel := context.WithCancel(parent)
	j := &job{cancel: cancel, ready: make(chan struct{}, 1)}
	w.wg.Add(1)
	go func() {
		defer w.wg.Done()
		passed := d.checkCycle(ctx, env, roles)
		cancel()
		w.release(&w.cycleBusy)
		j.finish(d.clock, nil, passed)
		d.emit(event{kind: evChecked, passed: passed})
	}()
	return j
}
