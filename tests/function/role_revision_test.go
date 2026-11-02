package function

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/wedevwork/callsheet/internal/contract"
)

// durableRevision reads one integer field of a plane state file.
func durableRevision(t *testing.T, field string, path ...string) int {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(path...))
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]json.RawMessage
	if err := json.Unmarshal(b, &doc); err != nil {
		t.Fatal(err)
	}
	n, err := strconv.Atoi(string(doc[field]))
	if err != nil {
		t.Fatalf("%s in %v: %v", field, path, err)
	}
	return n
}

// TestCrossNodeRoleRevision drives the built binaries: a role change on one
// node must not cost another node's roles their starts. Nothing restarts
// between the changes and the dispatches. Each task must succeed, and its
// durable stamp must be the revision its own node last received, the only
// revision the sidecar's check can accept.
func TestCrossNodeRoleRevision(t *testing.T) {
	p := newNodeCLI(t)
	np := startNodePlane(t, p)
	fake := fakeAdapterBinary(t)
	w1 := startRoleWorker(t, p, np, fake, nil)
	w2 := startRoleWorker(t, p, np, fake, nil)
	op := newNodeCLI(t)
	ins1, run1 := w1.manual(t, "instruction.md", "i"), w1.manual(t, "runbook.md", "r")
	ins2, run2 := w2.manual(t, "instruction.md", "i"), w2.manual(t, "runbook.md", "r")
	cl := np.client(t)

	registry := func() int { return durableRevision(t, "revision", np.root, "roles", "registry.json") }
	// dispatch runs one task to role and requires it to succeed at stamp.
	dispatch := func(t *testing.T, role string, stamp int) {
		t.Helper()
		r := mustOK(t, op.run(t, append([]string{"dispatch", "--json", "--role-id", role, "--goal", "revision check", "--acceptance", "fixture"}, np.trust()...)...))
		v, err := contract.ParseDispatchResponse(okOut(t, "dispatch", r))
		if err != nil {
			t.Fatal(err)
		}
		deadline := time.Now().Add(nodeWait)
		for !contract.TaskTerminal(v.State) {
			if time.Now().After(deadline) {
				t.Fatalf("task %s of %s never finished: %+v", v.TaskID, role, v)
			}
			time.Sleep(20 * time.Millisecond)
			if v, err = cl.ShowTask(t.Context(), v.TaskID, 0); err != nil {
				t.Fatal(err)
			}
		}
		if v.State != contract.TaskSucceeded {
			t.Fatalf("task %s of %s ended %s: %+v", v.TaskID, role, v.State, v.Reason)
		}
		if got := durableRevision(t, "roles_revision", np.root, "tasks", v.TaskID+".json"); got != stamp {
			t.Fatalf("task %s of %s stamped %d, want its node's %d (registry %d)", v.TaskID, role, got, stamp, registry())
		}
	}

	mustOK(t, op.run(t, roleAdd(np, "a", "coder", w1.id, ins1, run1)...))
	node1 := registry()
	mustOK(t, op.run(t, roleAdd(np, "b", "reviewer", w2.id, ins2, run2)...))
	node2 := registry()
	np.awaitReady(t, "a", "b")

	t.Run("added-on-other-node", func(t *testing.T) {
		dispatch(t, "a", node1)
		dispatch(t, "b", node2)
		dispatch(t, "a", node1)
	})
	t.Run("changed-on-other-node", func(t *testing.T) {
		mustOK(t, op.run(t, append([]string{"role", "set", "b", "--model", "other model"}, np.trust()...)...))
		node2 = registry()
		np.awaitReady(t, "a", "b")
		dispatch(t, "a", node1)
		dispatch(t, "b", node2)
	})
	t.Run("removed-on-other-node", func(t *testing.T) {
		mustOK(t, op.run(t, roleAdd(np, "c", "reviewer", w2.id, ins2, run2)...))
		np.awaitReady(t, "c")
		mustOK(t, op.run(t, append([]string{"role", "rm", "c"}, np.trust()...)...))
		node2 = registry()
		np.awaitReady(t, "a", "b")
		dispatch(t, "a", node1)
		dispatch(t, "b", node2)
	})
	t.Run("changed-on-own-node", func(t *testing.T) {
		mustOK(t, op.run(t, append([]string{"role", "set", "a", "--model", "other model"}, np.trust()...)...))
		node1 = registry()
		np.awaitReady(t, "a", "b")
		dispatch(t, "a", node1)
		dispatch(t, "b", node2)
	})
}
