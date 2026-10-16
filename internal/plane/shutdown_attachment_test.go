package plane

import (
	"testing"

	"github.com/wedevwork/callsheet/internal/contract"
)

// TestShutdownAttachmentUnavailable covers the two attachment checks that
// run after Close has nilled a live attachment. A closed registry is
// unavailable ("the plane is shutting down"). Detach on a registry that
// is still open stays invalid_argument. Each subtest asserts the
// attachment is current before Close or Detach.
func TestShutdownAttachmentUnavailable(t *testing.T) {
	call := map[string]func(*taskService, string, uint64) error{
		"inventory": func(ts *taskService, node string, gen uint64) error {
			return ts.inventory(node, gen, contract.TaskInventoryBody{RunID: "run", Final: true}, newReconciliation())
		},
		"authorizedLocked": func(ts *taskService, node string, gen uint64) error {
			ts.mu.Lock()
			defer ts.mu.Unlock()
			_, err := ts.authorizedLocked(node, gen, "t_00000000000000000000000000000001", contract.ExecutionToken{})
			return err
		},
	}
	for name, fn := range call {
		t.Run(name+"/closed", func(t *testing.T) {
			ts, gen := attachedService(t)
			requireCurrentAttachment(t, ts.reg, idA, gen)
			ts.reg.Close()
			wantCode(t, fn(ts, idA, gen), contract.CodeUnavailable, "shutting down")
		})
		t.Run(name+"/detached", func(t *testing.T) {
			ts, gen := attachedService(t)
			requireCurrentAttachment(t, ts.reg, idA, gen)
			ts.reg.Detach(idA, gen)
			wantCode(t, fn(ts, idA, gen), contract.CodeInvalidArgument, "no longer")
		})
	}
}

// attachedService enrolls idA, attaches it through a non-nil stream, and
// returns a task service whose registry is that one. The clock is the
// registry's fake clock.
func attachedService(t *testing.T) (*taskService, uint64) {
	t.Helper()
	r, _ := newRegistry(t)
	mustEnroll(t, r, idA)
	gen, err := r.attach(idA, "v", contract.ProtocolVersion, nil, &nodeStream{})
	if err != nil {
		t.Fatal(err)
	}
	return &taskService{reg: r, tasks: map[string]*taskEntry{}, logger: discardLogger()}, gen
}

// requireCurrentAttachment fails unless gen is node's live attachment:
// the current generation, with a non-nil stream.
func requireCurrentAttachment(t *testing.T, r *nodeRegistry, node string, gen uint64) {
	t.Helper()
	if !r.currentAttachment(node, gen) {
		t.Fatalf("attachment %d is not node %s's current attachment", gen, node)
	}
}
