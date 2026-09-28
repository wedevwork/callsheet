package plane

import (
	"context"
	"errors"
	"time"

	"github.com/wedevwork/callsheet/internal/contract"
)

// The per-task persistence writer (iteration 05, extended by 06a FP-1 and
// FP-7): one goroutine per task serializes the running publication,
// checkpoints, the terminal commit of the latched candidate and the late
// evidence, one complete atomic document at a time, without holding the
// gate or any lock across disk I/O. Every outcome is installed under the
// task lock in one critical section with the derived state (counts,
// storage block, reservation release) readers observe.

// terminalOutcome is a latched candidate's terminal fields, applied to the
// task's current record when the commit is written (a running publication
// may precede it).
type terminalOutcome struct {
	state     string
	started   *time.Time
	finished  time.Time
	exit      *int
	signal    *string
	final     *string
	truncated bool
	reason    *contract.TaskReason
	log       contract.TaskLog
}

// apply returns rec with the outcome.
func (o terminalOutcome) apply(rec contract.TaskRecord) contract.TaskRecord {
	f := o.finished
	rec.State, rec.StartedAt, rec.FinishedAt = o.state, o.started, &f
	rec.ExitCode, rec.Signal, rec.FinalMessage, rec.FinalMessageTruncated = o.exit, o.signal, o.final, o.truncated
	rec.Reason, rec.Log, rec.Candidates = o.reason, o.log, nil
	return rec
}

// publication is one prepared document write.
type publication struct {
	kind string // running, checkpoint, terminal, lost, rejected, late
	rec  contract.TaskRecord
	live int // retained bytes of rec's primary log (in-memory metadata)
}

// candKindName is the publication kind of a latched candidate.
func candKindName(k candKind) string {
	switch k {
	case candLost:
		return "lost"
	case candRefusal:
		return "rejected"
	}
	return "terminal"
}

// nextJobLocked selects e's next publication: nothing while its visible
// record is unconfirmed (its resync precedes any later candidate) or a
// retry is not due; then running before any terminal record; then the
// latched terminal candidate; then late evidence once the terminal record
// is confirmed; then a due checkpoint. done reports that the writer may
// exit (terminal, confirmed, nothing pending).
func (ts *taskService) nextJobLocked(e *taskEntry, now time.Time) (job *publication, done bool) {
	if e.terminal() && e.released && e.confirmed && e.late == nil {
		return nil, true
	}
	if !e.confirmed || (e.fault && now.Before(e.retryAt)) {
		return nil, false
	}
	if e.rec.Revision >= contract.MaxSafeInteger {
		// The revision counter is exhausted: keep the safe state and keep
		// the storage failure visible (admissions stay blocked).
		if !e.fault {
			e.fault = true
			// A pending outcome shows its storage condition.
			e.commitFailed = e.cand != nil || e.late != nil
			ts.refreshLocked(e)
			ts.logger.Error("task revision counter exhausted", "task_id", e.rec.TaskID)
		}
		return nil, false
	}
	next := e.rec
	next.Revision++
	switch {
	case e.needRunning && !e.terminal():
		started := e.startedAt
		next.State, next.StartedAt = contract.TaskRunning, &started
		if e.ring != nil {
			next.Log = e.ring.snapshot()
		}
		return &publication{kind: "running", rec: next, live: len(next.Log.Data)}, false
	case e.cand != nil && !e.terminal():
		rec := e.cand.outcome.apply(next)
		if e.cand.digest != "" {
			d := e.cand.digest
			rec.ResultDigest = &d
		}
		if e.cand.kind == candRefusal {
			rec.Candidates = ts.observeLocked(e.rec.Request.Target).cands
		}
		return &publication{kind: candKindName(e.cand.kind), rec: rec, live: len(rec.Log.Data)}, false
	case e.late != nil && e.terminal() && e.released:
		// Composed outside the lock: the primary tail is read from the
		// document (terminal tails are not kept in memory).
		return &publication{kind: "late", rec: next}, false
	case e.ring != nil && e.logDirty && e.rec.State == contract.TaskRunning && e.cand == nil && !now.Before(e.lastCheckpoint.Add(checkpointEvery)):
		next.Log = e.ring.snapshot()
		e.logDirty = false
		e.lastCheckpoint = now
		return &publication{kind: "checkpoint", rec: next, live: len(next.Log.Data)}, false
	}
	return nil, false
}

// writerLoop is e's task-specific persistence writer: it serializes the
// running publication, checkpoints, the terminal commit and the late
// evidence, one document at a time, without holding the gate or a node
// lock. It exits when e is terminal, confirmed and has nothing pending,
// or at plane shutdown.
func (ts *taskService) writerLoop(e *taskEntry) {
	defer ts.wg.Done()
	for {
		now := ts.clock.Now()
		ts.mu.Lock()
		job, done := ts.nextJobLocked(e, now)
		if done {
			e.writer = false
			ts.mu.Unlock()
			return
		}
		id := e.rec.TaskID
		ts.mu.Unlock()
		if job == nil {
			select {
			case <-e.wake:
				continue
			case <-ts.stop:
				ts.mu.Lock()
				e.writer = false
				ts.mu.Unlock()
				return
			}
		}
		// Named events before any syscall of the write (tests hold them).
		ts.at(job.kind+"-queued", id, context.Background())
		switch job.kind {
		case "terminal", "lost", "rejected":
			ts.at("terminal-commit-queued", id, context.Background())
		}
		var err error
		if job.kind == "late" {
			job, err = ts.composeLate(e, job)
		}
		if err == nil {
			err = ts.st.update(job.rec)
		}
		ts.mu.Lock()
		ts.applyLocked(e, job, err)
		ts.mu.Unlock()
	}
}

// composeLate builds the late-evidence document: the current document's
// primary tail (frozen forever) and the newest late suffix that fits the
// remaining 10 MiB combined budget (truncated when it does not). The
// document is read outside the locks; the task's single writer makes its
// revision the visible one.
func (ts *taskService) composeLate(e *taskEntry, job *publication) (*publication, error) {
	ts.mu.Lock()
	id := e.rec.TaskID
	ts.mu.Unlock()
	cur, err := ts.st.l.readTaskFile(id, ts.lookup)
	if err != nil {
		return job, err
	}
	ts.mu.Lock()
	defer ts.mu.Unlock()
	if e.late == nil || cur.Revision != e.rec.Revision {
		return job, errors.New("the task document changed while late evidence was composed")
	}
	rec := e.rec
	rec.Revision++
	rec.Log.Data = cur.Log.Data
	lg := e.late.log
	limit := contract.MaxLogRetainedBytes
	if ts.logCap > 0 {
		limit = ts.logCap
	}
	if budget := limit - len(rec.Log.Data); len(lg.Data) > budget {
		lg.Data = lg.Data[len(lg.Data)-max(0, budget):]
	}
	l := contract.LateFrom(e.late.result, e.late.at, lg)
	rec.Late = &l
	return &publication{kind: "late", rec: rec, live: len(rec.Log.Data)}, nil
}

// applyLocked publishes a write's outcome atomically for readers: a
// confirmed document and its derived reservation change together; a
// visible but unconfirmed one is adopted (latched) with its reservation
// held and admissions blocked until a resync confirms it; a failure
// before visibility leaves the old record authoritative and schedules a
// coalesced retry of the same candidate.
func (ts *taskService) applyLocked(e *taskEntry, job *publication, err error) {
	now := ts.clock.Now()
	switch {
	case err == nil || errors.Is(err, errTaskUnconfirmed):
		e.rec = job.rec
		e.retained = job.live
		if job.rec.Late != nil {
			e.lateRetained = len(job.rec.Late.Log.Data)
		}
		e.confirmed = err == nil
		e.fault = err != nil
		if err != nil {
			e.retryAt = now.Add(storageRetry)
			ts.logger.Error("task durability unconfirmed", "task_id", e.rec.TaskID, "error", err)
		}
		switch job.kind {
		case "running":
			e.needRunning = false
		case "terminal", "lost", "rejected":
			e.cand = nil
		case "late":
			e.late, e.lateRing = nil, nil
		}
		if e.confirmed {
			ts.finishPublicationLocked(e)
		} else if e.rec.Late != nil {
			e.rec.Late.Log.Data = nil
		}
		ts.event("published " + job.kind + " " + e.rec.TaskID)
	default:
		e.fault, e.retryAt = true, now.Add(storageRetry)
		switch job.kind {
		case "terminal", "lost", "rejected", "late":
			e.commitFailed = true
		case "checkpoint":
			e.logDirty = true
		}
		ts.logger.Error("task publication failed", "task_id", e.rec.TaskID, "kind", job.kind, "error", err)
		ts.event("publish-failed " + job.kind + " " + e.rec.TaskID)
	}
	ts.refreshLocked(e)
}

// finishPublicationLocked completes a confirmed publication: a terminal
// record drops the live ring and the latched candidate and releases the
// reservation (refreshLocked) in the same critical section, exactly once;
// log data of any record leaves memory (the ring or the document holds
// it).
func (ts *taskService) finishPublicationLocked(e *taskEntry) {
	if e.terminal() {
		first := !e.released
		e.released = true
		e.ring, e.watchdog, e.commitFailed = nil, false, false
		e.rec.Log.Data = nil
		if first {
			ts.event("terminal-committed " + e.rec.TaskID)
		}
		if e.rec.Late != nil {
			e.rec.Late.Log.Data = nil
			if e.late == nil && !first {
				ts.event("late-committed " + e.rec.TaskID)
			}
		}
	} else if e.ring != nil {
		e.rec.Log.Data = nil
	}
}

// tick is the sweep's once-per-second storage duty: it expires commit
// watchdogs and wakes writers whose retry or checkpoint is due.
func (ts *taskService) tick() {
	if ts == nil {
		return
	}
	now := ts.clock.Now()
	var resync bool
	ts.mu.Lock()
	for _, e := range ts.tasks {
		var since time.Time
		switch {
		case e.cand != nil:
			since = e.cand.at
		case e.late != nil:
			since = e.late.at
		}
		if !since.IsZero() && !e.watchdog && !now.Before(since.Add(commitWatchdog)) {
			e.watchdog = true
			ts.refreshLocked(e)
			ts.logger.Error("task result commit unconfirmed", "task_id", e.rec.TaskID, "reason", contract.ReasonResultStorageUnconfirmed)
			ts.event("commit-watchdog " + e.rec.TaskID)
		}
		if !e.confirmed && !now.Before(e.retryAt) {
			resync = true
		}
		if e.writer && ((e.fault && !now.Before(e.retryAt)) || (e.logDirty && !now.Before(e.lastCheckpoint.Add(checkpointEvery)))) {
			ts.wakeLocked(e)
		}
	}
	ts.mu.Unlock()
	if resync {
		ts.resyncStore()
	}
}
