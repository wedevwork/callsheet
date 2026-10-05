package sidecar

import (
	"testing"
	"time"
)

// TestAnswerStartReadsWorkspaceDeadlineUnderLock: answerStart reads the
// worker's preparation deadline under w.mu, so prepareWorkspace's locked
// replacement of that deadline (workspace starts, iteration 10b) is ordered
// with the session's read. Under -race an unlocked read is reported against
// the write below; no pollStart runs in between, since its lock would order
// the two accesses on its own.
func TestAnswerStartReadsWorkspaceDeadlineUnderLock(t *testing.T) {
	now := time.Now()
	w := &taskWorker{deadline: now.Add(prepBudget)}
	rs := &roleSession{d: &deps{clock: realClock{}}}
	e := &startEntry{w: w}

	done := make(chan struct{})
	go func() {
		defer close(done)
		rs.answerStart("r1", "task-1", e)
	}()

	// The write prepareWorkspace makes once the journal is durable.
	w.mu.Lock()
	w.prepReply = true
	w.deadline = now.Add(time.Hour)
	w.mu.Unlock()
	<-done

	if rs.reply != nil {
		t.Fatalf("answerStart queued reply %+v for a start neither refused nor started", rs.reply)
	}
	if rs.pend == nil || rs.pend.w != w || rs.pend.e != e || rs.pend.id != "r1" {
		t.Fatalf("answerStart opened pending start %+v, want r1 on the worker", rs.pend)
	}
	if !rs.pend.timer.on {
		t.Fatal("answerStart did not arm the preparation timer")
	}
	if at := rs.pend.timer.at; !at.Equal(now.Add(prepBudget)) && !at.Equal(now.Add(time.Hour)) {
		t.Fatalf("preparation timer armed at %v, want the initial or the replaced deadline", at)
	}
	rs.pend.timer.clear()
}
