package sidecar

import (
	"errors"
	"fmt"
	"path/filepath"

	"github.com/wedevwork/callsheet/internal/contract"
)

// Sidecar restart recovery (iteration 06a, FP-5, resilience.md "Sidecar
// restart"). Under the state lock, before role readiness or any new
// start, every journal is validated one at a time. A completed or lost
// outbox is replayed as it is (the same frozen result and digest on every
// restart); a prepared or running execution is never resumed or launched:
// it is resolved lost (sidecar_restarted) in its journal, keeping the
// latest durable partial tail. An execution whose guardian recorded
// ownership has its group cleaned before the node accepts anything: the
// authentic guardian is asked to stop through its control FIFO and the
// group's disappearance is polled (ESRCH); the sidecar never signals a
// raw, possibly reused PGID itself. When absence cannot be proved (no
// guardian serves the FIFO while the group exists, EPERM, a stopped
// guardian) the owner record is kept, every node start stays blocked and
// cleanup_unconfirmed goes to the sidecar's local structured log only.
// One recovery worker runs per node, never one goroutine per record.

// recoverJournals loads every journal of ids and registers its recovered
// worker; it returns an error (Run does not start) for an invalid journal,
// which is never repaired or deleted.
func (s *taskSupervisor) recoverJournals(ids []string, lookup contract.AdapterLookup) error {
	s.lookup = lookup
	var pending []*taskWorker
	for _, id := range ids {
		lj, err := s.jr.l.loadJournal(id, lookup)
		if err != nil {
			return err
		}
		w := s.recovered(lj)
		if lj.j.Phase == contract.JournalRunning && lj.j.Workspace != nil {
			// Iteration 10b: a checkpointed publication is resumed, never
			// rerun (an authorized intent is only observed); its slot is held
			// until its outbox is durable.
			cp, err := s.jr.l.readCheckpoint(id)
			if err != nil {
				return err
			}
			if cp != nil {
				w.resuming, w.cleanupCh = true, make(chan struct{})
				s.mu.Lock()
				s.occupied[w.key]++
				s.workers[w] = true
				s.mu.Unlock()
				if lj.owner != nil {
					w.cleaning = true
					s.mu.Lock()
					s.blockers[w] = true
					s.mu.Unlock()
					pending = append(pending, w)
				} else {
					w.cleanupOK = true
					close(w.cleanupCh)
				}
				s.wg.Add(1)
				go s.resumeWorkspace(w, *cp)
				continue
			}
		}
		if lj.j.Phase == contract.JournalPrepared || lj.j.Phase == contract.JournalRunning {
			// Never resumed: its outcome is lost, frozen once, keeping a
			// latched stop intent's ID (iteration 06b: never a newly
			// inferred cancelled or timed_out).
			res := contract.TaskResultBody{TaskID: w.id(), Execution: w.start.Execution, Outcome: contract.OutcomeLost,
				OutputBytes: lj.j.Log.SourceBytes, LogIncomplete: true, CounterOverflow: lj.j.Log.CounterOverflow}
			if in := lj.j.StopIntent; in != nil {
				id := in.ID
				res.StopID = &id
			}
			res = workspaceLost(lj.j.Workspace, res)
			j := lj.j
			j.Phase, j.Result = contract.JournalLost, &res
			s.logger.Warn("task execution lost", "task_id", w.id(), "reason", lostSidecarRestarted)
			if err := s.jr.write(j); err != nil {
				s.logger.Error("task journal publication failed; retrying", "task_id", w.id(), "error", err)
				w.journalFailing = true
				w.pendingLost = &res
			} else {
				w.outcome = &res
			}
		} else {
			res := *lj.j.Result
			w.outcome = &res
		}
		if lj.owner != nil {
			// Its group may still exist: it counts against its instance and
			// blocks the node until its absence is proved.
			w.cleaning = true
		} else {
			w.cleanupOK = true
		}
		// Iteration 10b: a journal-owned work directory (an ownerless
		// journal's included) also holds the slot, until the group's absence
		// is proved, the outcome is durable and the work is deleted.
		w.ownsWork = w.work != ""
		s.mu.Lock()
		if w.cleaning || w.ownsWork {
			s.occupied[w.key]++
		}
		s.workers[w] = true
		if w.cleaning || w.journalFailing {
			s.blockers[w] = true
		}
		s.mu.Unlock()
		if w.cleaning || w.journalFailing || w.ownsWork {
			pending = append(pending, w)
		}
	}
	s.wg.Add(1)
	go s.recoveryLoop(pending)
	return nil
}

// recovered builds a previous Run's execution from its journal.
func (s *taskSupervisor) recovered(lj loadedJournal) *taskWorker {
	j := lj.j
	ringCap := contract.MaxLogRetainedBytes
	if s.d.taskRingCap > 0 {
		ringCap = s.d.taskRingCap
	}
	// Compact metadata only: the retained tail stays in the journal until
	// the replay loader reads it back for a reconciled attachment.
	w := &taskWorker{sup: s, start: contract.TaskStartBody{TaskID: j.TaskID, Execution: j.Execution, Role: j.Role, Effective: j.Effective, Workspace: j.Workspace},
		digest: j.StartDigest, key: keyOf(j.Role), ring: newOutputRingMeta(ringCap, j.Log), exitedCh: make(chan struct{}),
		stopCh: make(chan struct{}), commitCh: make(chan struct{}), ctlCh: make(chan struct{}), recovered: true, owner: lj.owner, phase: phaseStarted,
		started: j.StartedAt, done: true, ctl: j.StopIntent, release: make(chan bool, 1)}
	close(w.exitedCh)
	if lj.owner != nil {
		w.pgid = lj.owner.PGID
	}
	if j.Work {
		w.work = filepath.Join(lj.dir, workName)
	}
	if j.RuntimeDir != nil {
		w.runtimeDir = *j.RuntimeDir
	}
	return w
}

// recoveryLoop is the node's one recovery worker. Group cleanup never
// waits for failing result storage: every recorded group is cleaned first,
// in turn (each attempt ends confirmed or blocked); then the lost outcomes
// whose journal publication failed are retried every journalRetry until
// they are durable or Run shuts down. A worker stays a node blocker until
// both of its obligations complete. A journal-owned work directory is
// deleted, and its slot released, once both hold (iteration 10b).
func (s *taskSupervisor) recoveryLoop(ws []*taskWorker) {
	defer s.wg.Done()
	for _, w := range ws {
		w.mu.Lock()
		cleaning := w.cleaning
		w.mu.Unlock()
		if !cleaning {
			continue
		}
		if s.isClosing() {
			return
		}
		s.cleanupRecovered(w)
	}
	for _, w := range ws {
		s.releaseRecoveredWork(w)
	}
	for {
		var pending []*taskWorker
		for _, w := range ws {
			w.mu.Lock()
			if w.pendingLost != nil {
				pending = append(pending, w)
			}
			w.mu.Unlock()
		}
		if len(pending) == 0 {
			break
		}
		if !s.pause(journalRetry) {
			return
		}
		for _, w := range pending {
			s.publishLost(w)
			s.releaseRecoveredWork(w)
		}
	}
	s.d.emit(event{kind: evRecoveryDone})
}

// releaseRecoveredWork deletes a recovered execution's journal-owned work
// directory and then frees the slot it holds, once its group's absence is
// proved and its outcome is durable (a lost outcome journaled, or a frozen
// outbox loaded). A failed deletion keeps the slot held (cleanup pending)
// until the janitor's removal of the whole task directory succeeds; an
// unproved cleanup keeps both. The janitor never removes the journal while
// the work is owned here.
func (s *taskSupervisor) releaseRecoveredWork(w *taskWorker) {
	w.mu.Lock()
	ready := w.ownsWork && w.cleanupOK && w.outcome != nil && w.pendingLost == nil && !w.forgotten
	w.mu.Unlock()
	if !ready {
		return
	}
	err := s.d.hook("remove", filepath.Join(taskDirRel(w.id()), workName))
	if err == nil {
		err = removeWork(w.work)
	}
	w.mu.Lock()
	w.ownsWork = false
	if err != nil {
		w.workPending = true
	}
	w.mu.Unlock()
	if err != nil {
		s.logger.Warn("recovered task work directory cleanup failed; the slot is held until the janitor removes it", "task_id", w.id())
		s.event(evWorkPending, w)
		s.kickJanitor()
		return
	}
	s.release(w.key)
	s.event(evWorkReleased, w)
	s.kickJanitor()
}

// publishLost retries one recovered execution's lost-outcome journal
// publication once.
func (s *taskSupervisor) publishLost(w *taskWorker) {
	w.mu.Lock()
	res := w.pendingLost
	w.mu.Unlock()
	if res == nil {
		return
	}
	// The durable tail is the journal's own (still the previous document:
	// the failed publications replaced nothing); memory holds none.
	lj, err := s.jr.l.loadJournal(w.id(), s.lookup)
	if err == nil {
		j := lj.j
		j.Phase, j.Result = contract.JournalLost, res
		err = s.jr.write(j)
	}
	if err != nil {
		s.logger.Error("task journal publication failed; retrying", "task_id", w.id(), "error", err)
		return
	}
	w.mu.Lock()
	w.outcome, w.pendingLost, w.journalFailing = res, nil, false
	w.mu.Unlock()
	s.refreshBlocker(w)
	s.signal()
}

// cleanupRecovered cleans a previous Run's recorded group: absent (ESRCH)
// is confirmed at once; otherwise its guardian is asked to stop through
// the control FIFO (it verifies the identity and signals only its own
// group) and disappearance is polled. No guardian, EPERM or a group that
// stays is a blocked cleanup: never a signal to a raw PGID.
func (s *taskSupervisor) cleanupRecovered(w *taskWorker) {
	o := w.owner
	alive, err := s.groups.exists(o.PGID)
	if err == nil && !alive {
		s.cleanupDone(w, nil)
		return
	}
	cmd, cerr := contract.EncodeGuardianCommand(contract.GuardianCommand{TaskID: w.id(), Execution: w.start.Execution, Nonce: o.Nonce, Command: contract.GuardianStop})
	if cerr == nil {
		cerr = s.send(filepath.Join(s.jr.l.path(taskDirRel(w.id())), controlName), cmd)
	}
	switch {
	case errors.Is(cerr, errNoGuardian):
		s.cleanupDone(w, fmt.Errorf("no guardian serves the task's control FIFO while its recorded group may exist: %w", errNoGuardian))
	case cerr != nil:
		s.cleanupDone(w, cerr)
	default:
		s.cleanupDone(w, s.groups.gone(o.PGID))
	}
}

// cleanupDone records a recovered group's cleanup outcome.
func (s *taskSupervisor) cleanupDone(w *taskWorker, err error) {
	w.mu.Lock()
	w.cleaning = false
	if err == nil {
		w.cleanupOK = true
	} else {
		w.cleanupFailed = true
	}
	w.mu.Unlock()
	if err == nil {
		// A resumed publication or a journal-owned work directory keeps the
		// slot until the work is deleted (iteration 10b).
		w.mu.Lock()
		keep := w.resuming || w.ownsWork
		w.mu.Unlock()
		if !keep {
			s.release(w.key)
		}
		s.refreshBlocker(w)
		s.event(evCleanupConfirmed, w)
		if w.cleanupCh != nil {
			// A resumed publication continues (iteration 10b).
			close(w.cleanupCh)
		}
		s.kickJanitor()
		return
	}
	s.logger.Error("task process group cleanup unconfirmed", "task_id", w.id(), "role_id", w.key.id, "reason", "cleanup_unconfirmed", "error_class", errorClass(err))
	s.refreshBlocker(w)
	s.event(evCleanupBlocked, w)
}

// errorClass is a safe, fixed classification of a cleanup error.
func errorClass(err error) string {
	switch {
	case errors.Is(err, errNoGuardian):
		return "guardian_unavailable"
	case errors.Is(err, errPermission):
		return "permission"
	}
	return "group_present"
}

// kickReplay wakes the replay loader.
func (s *taskSupervisor) kickReplay() { notify(s.replay) }

// replayLoop is the node's one replay loader (iteration 06a, C4): it reads
// recovered tails back from their journals for executions a reconciled
// attachment asks for (send_result or stop_lost), one tail in memory at a
// time: the next is loaded only after the previous replayed tail was
// acknowledged (its storage released) or forgotten. It runs until Run
// shuts down; the stream owner never reads a journal.
func (s *taskSupervisor) replayLoop() {
	defer s.wg.Done()
	for {
		select {
		case <-s.replay:
		case <-s.stopAll:
			return
		}
		for {
			var next *taskWorker
			busy := false
			for _, w := range s.snapshot() {
				if !w.recovered {
					continue
				}
				if w.ring.holding() {
					busy = true
					break
				}
				w.mu.Lock()
				bound := w.tag != nil && w.replied && !w.forgotten
				w.mu.Unlock()
				if next == nil && bound && w.ring.needsLoad() {
					next = w
				}
			}
			if busy || next == nil {
				break
			}
			var data []byte
			if lj, err := s.jr.l.loadJournal(next.id(), s.lookup); err != nil {
				s.logger.Error("recovered task output unreadable; its replay is incomplete", "task_id", next.id(), "error", err)
			} else {
				data = lj.j.Log.Data
			}
			next.ring.load(data)
			s.signal()
		}
	}
}
