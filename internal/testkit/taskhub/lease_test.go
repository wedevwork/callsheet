package taskhub

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/wedevwork/callsheet/internal/contract"
	"github.com/wedevwork/callsheet/internal/testkit"
)

// TestNodeKeepsItsLease starts a plane and a node, waits until the role
// can accept, then waits the lease plus one sweep. The role must still
// accept. A node that heartbeats only from the role snapshot loses the
// attachment, and the role is offline.
func TestNodeKeepsItsLease(t *testing.T) {
	t.Parallel()
	p := RunPlane(t, filepath.Join(t.TempDir(), "plane"))
	n := StartNode(t, p, "n_"+strings.Repeat("6", 32))
	Recv(t, n, n.Ready, "first heartbeat")
	dir := t.TempDir()
	ins, run := filepath.Join(dir, "i.md"), filepath.Join(dir, "r.md")
	if err := os.WriteFile(ins, []byte("i"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(run, []byte("r"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := p.Client.AddRole(context.Background(), contract.RoleConfig{ID: "lease-a", Name: "lease-a", Node: n.ID, Adapter: "fake", Instruction: ins, Runbook: run,
		Model: "example-model", Effort: "medium", Concurrency: 1}); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(Wait)
	for {
		v, err := p.Client.ShowRole(context.Background(), "lease-a")
		if err == nil && v.CanAccept && v.NodeLiveness == contract.LivenessOnline {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("role never accepted: %+v %v", v, err)
		}
		time.Sleep(time.Millisecond)
	}
	// sweepInterval in the plane is one second. This wait lives only here.
	time.Sleep(time.Duration(contract.LeaseMS)*time.Millisecond + time.Second)
	v, err := p.Client.ShowRole(context.Background(), "lease-a")
	if err != nil || !v.CanAccept || v.NodeLiveness != contract.LivenessOnline {
		t.Fatalf("role after lease: can_accept=%v liveness=%s err=%v", v.CanAccept, v.NodeLiveness, err)
	}
}

// TestHeartbeatStopsWhenStreamEnds stops the plane, so the reader exits,
// and requires the heartbeat goroutine to exit without Close.
func TestHeartbeatStopsWhenStreamEnds(t *testing.T) {
	t.Parallel()
	p := RunPlane(t, filepath.Join(t.TempDir(), "plane"))
	n := StartNode(t, p, "n_"+strings.Repeat("6", 32))
	Recv(t, n, n.Ready, "first heartbeat")
	p.Stop()
	// WithinBy rechecks the completion channel when the bound is also ready.
	testkit.WithinBy(t, n.done, time.After(Wait), "the reader's end")
	testkit.WithinBy(t, n.hbLoopDone, time.After(Wait), "the heartbeat goroutine's end")
}

// TestHeartbeatStopsOnClose requires explicit Close to join the heartbeat
// goroutine as well as the reader.
func TestHeartbeatStopsOnClose(t *testing.T) {
	t.Parallel()
	p := RunPlane(t, filepath.Join(t.TempDir(), "plane"))
	n := StartNode(t, p, "n_"+strings.Repeat("6", 32))
	Recv(t, n, n.Ready, "first heartbeat")
	n.Close()
	select {
	case <-n.done:
	default:
		t.Fatal("close left the reader running")
	}
	select {
	case <-n.hbLoopDone:
	default:
		t.Fatal("close left the heartbeat goroutine alive")
	}
	n.Close()
}
