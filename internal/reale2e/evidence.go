package reale2e

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// The live evidence writer (design 12a-real-e2e, Evidence and checker):
// a new mode-0700 directory of mode-0600 files. Supervisor events are
// appended to events.jsonl as they happen, each with a strictly increasing
// sequence; every other file is created atomically (a private temporary
// file, synced, then hard-linked to its final name, which fails rather
// than overwrite) so a completed snapshot or decision is never replaced.

// Event types of events.jsonl.
const (
	EvRunStarted          = "run_started"
	EvPreflightStarted    = "preflight_started"
	EvVersionsChecked     = "versions_checked"
	EvRuntimeCreated      = "runtime_created"
	EvProcessStarted      = "process_started"
	EvRolesRegistered     = "roles_registered"
	EvSidecarsReconnected = "sidecars_reconnected"
	EvRolesReady          = "roles_ready"
	EvTaskAdmitted        = "task_admitted"
	EvPreflightFinished   = "preflight_finished"
	EvSeedCreated         = "seed_created"
	EvBaselineTests       = "baseline_tests"
	EvCoordinatorReady    = "coordinator_ready"
	EvObserved            = "observed"
	EvTaskTerminal        = "task_terminal"
	EvAwaitingOwner       = "awaiting_owner"
	EvOwnerDecision       = "owner_decision"
	EvReceiptCreated      = "receipt_created"
	EvPrematureAdmission  = "premature_admission"
	EvExtraAdmission      = "extra_admission"
	EvTaskCancelled       = "task_cancelled"
	EvFinalTests          = "final_tests"
	EvFinishRequested     = "finish_requested"
	EvAttemptFailed       = "attempt_failed"
	EvCollectionStarted   = "collection_started"
	EvCleanupStarted      = "cleanup_started"
	EvProcessStopped      = "process_stopped"
	EvCleanupFinished     = "cleanup_finished"
)

// eventTypes is the closed set the checker accepts.
var eventTypes = map[string]bool{
	EvRunStarted: true, EvPreflightStarted: true, EvVersionsChecked: true, EvRuntimeCreated: true, EvProcessStarted: true,
	EvRolesRegistered: true, EvSidecarsReconnected: true, EvRolesReady: true, EvTaskAdmitted: true, EvPreflightFinished: true,
	EvSeedCreated: true, EvBaselineTests: true, EvCoordinatorReady: true, EvObserved: true, EvTaskTerminal: true,
	EvAwaitingOwner: true, EvOwnerDecision: true, EvReceiptCreated: true, EvPrematureAdmission: true, EvExtraAdmission: true,
	EvTaskCancelled: true, EvFinalTests: true, EvFinishRequested: true, EvAttemptFailed: true, EvCollectionStarted: true,
	EvCleanupStarted: true, EvProcessStopped: true, EvCleanupFinished: true,
}

// Event is one events.jsonl line. Hop is a feature hop name or
// "preflight-NN"; Handle names a process or wait handle; Code is a fixed
// diagnostic code, never raw text.
type Event struct {
	Seq    int    `json:"seq"`
	Type   string `json:"type"`
	Time   string `json:"time"`
	Hop    string `json:"hop,omitempty"`
	Task   string `json:"task,omitempty"`
	Handle string `json:"handle,omitempty"`
	Code   string `json:"code,omitempty"`
}

// formatTime renders an evidence timestamp.
func formatTime(t time.Time) string { return t.UTC().Format(time.RFC3339Nano) }

// parseTime parses an evidence timestamp.
func parseTime(s string) (time.Time, bool) {
	t, err := time.Parse(time.RFC3339Nano, s)
	return t, err == nil && strings.HasSuffix(s, "Z")
}

// evidenceWriter writes one run's bundle.
type evidenceWriter struct {
	root   string
	clock  Clock
	mu     sync.Mutex
	seq    int
	last   time.Time
	events *os.File
	err    error
}

// newEvidenceWriter creates root (which must not exist) mode 0700 and
// its events.jsonl.
func newEvidenceWriter(root string, clock Clock) (*evidenceWriter, error) {
	if err := os.Mkdir(root, 0o700); err != nil {
		return nil, fmt.Errorf("evidence: %w", err)
	}
	f, err := os.OpenFile(filepath.Join(root, "events.jsonl"), os.O_WRONLY|os.O_CREATE|os.O_EXCL|os.O_APPEND, 0o600)
	if err != nil {
		return nil, fmt.Errorf("evidence: %w", err)
	}
	return &evidenceWriter{root: root, clock: clock, events: f}, nil
}

// event appends one event now and returns it. A write failure is sticky:
// the bundle is then incomplete and the run fails.
func (e *evidenceWriter) event(ev Event) Event {
	e.mu.Lock()
	defer e.mu.Unlock()
	now := e.clock.Now()
	if now.Before(e.last) {
		now = e.last
	}
	e.last = now
	e.seq++
	ev.Seq, ev.Time = e.seq, formatTime(now)
	b, _ := json.Marshal(ev)
	if e.err == nil {
		if _, err := e.events.Write(append(b, '\n')); err != nil {
			e.err = fmt.Errorf("evidence: events: %w", err)
		}
	}
	return ev
}

// failure is the first evidence write failure, if any.
func (e *evidenceWriter) failure() error {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.err
}

// close closes events.jsonl.
func (e *evidenceWriter) close() error { return e.events.Close() }

// errExists is an atomic create of an existing evidence file.
var errExists = errors.New("evidence file already exists")

// writeFile creates rel (slash-separated) with data, atomically and never
// overwriting; missing parents are created mode 0700.
func (e *evidenceWriter) writeFile(rel string, data []byte) error {
	err := writeAtomic(e.root, rel, data)
	if err != nil {
		e.mu.Lock()
		if e.err == nil {
			e.err = err
		}
		e.mu.Unlock()
	}
	return err
}

// writeJSON renders v and writes it as rel; an unencodable document is a
// sticky failure, never an empty file.
func (e *evidenceWriter) writeJSON(rel string, v any) error {
	b, err := encodeJSON(v)
	if err != nil {
		err = fmt.Errorf("evidence %s: %w", rel, err)
		e.mu.Lock()
		if e.err == nil {
			e.err = err
		}
		e.mu.Unlock()
		return err
	}
	return e.writeFile(rel, b)
}

// writeAtomic is writeFile without the sticky failure.
func writeAtomic(root, rel string, data []byte) error {
	final := filepath.Join(root, filepath.FromSlash(rel))
	dir := filepath.Dir(final)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("evidence %s: %w", rel, err)
	}
	tmp, err := os.CreateTemp(dir, ".tmp-")
	if err != nil {
		return fmt.Errorf("evidence %s: %w", rel, err)
	}
	defer os.Remove(tmp.Name())
	_, werr := tmp.Write(data)
	serr := tmp.Sync()
	cerr := tmp.Close()
	if err := errors.Join(werr, serr, cerr); err != nil {
		return fmt.Errorf("evidence %s: %w", rel, err)
	}
	if err := os.Link(tmp.Name(), final); err != nil {
		if errors.Is(err, os.ErrExist) {
			return fmt.Errorf("evidence %s: %w", rel, errExists)
		}
		return fmt.Errorf("evidence %s: %w", rel, err)
	}
	return nil
}
