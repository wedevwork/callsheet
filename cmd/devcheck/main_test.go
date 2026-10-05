package main

import (
	"bytes"
	"context"
	"io"
	"path/filepath"
	"strings"
	"testing"

	"github.com/wedevwork/callsheet/internal/devcheck"
)

func TestRunWrapper(t *testing.T) {
	var out, errOut bytes.Buffer
	// Tests pass their own evidence directory; only main selects the fixed
	// path, which a test never creates, locks or writes.
	opts := devcheck.RunOptions{EvidenceDir: filepath.Join(t.TempDir(), "evidence")}
	if defaultEvidenceDir != "/tmp/callsheet-container-e2e-evidence" {
		t.Fatalf("evidence directory = %q", defaultEvidenceDir)
	}
	if code := run(nil, &out, &errOut, nil, opts); code != 2 || !strings.Contains(errOut.String(), "usage") {
		t.Fatalf("no args = %d %q", code, errOut.String())
	}
	// The wrapper passes the injected runner and options through; it never
	// recursively invokes devcheck itself.
	var calls [][]string
	fake := func(_ context.Context, argv, _ []string, _ string, _, _ io.Writer) error {
		calls = append(calls, argv)
		return nil
	}
	if code := run([]string{"bench"}, &out, &errOut, fake, opts); code != 0 {
		t.Fatalf("bench = %d %s", code, errOut.String())
	}
	// Twelve bench commands: git transport, plane (iteration 02, with the
	// iteration 03 node and iteration 04 role benchmarks), the frame
	// contract (iterations 03 and 04), then the sidecar ready checks and
	// the adapter probe (iteration 04), then the MCP server (iteration
	// 07a), then the qualification harness (iteration 07b), then the
	// workspace hub (iteration 09a), then the workspace local transfers
	// (iteration 09b), then the task workspace (iteration 10b), then the
	// tagged real-adapter final-file helper (iteration 08), then the
	// container evidence parser (m3-m4-container-e2e).
	if len(calls) != 12 || calls[0][0] != "go" || calls[0][1] != "test" || calls[1][2] != "./internal/plane" || calls[2][2] != "./internal/contract" ||
		calls[3][2] != "./internal/sidecar" || calls[4][2] != "./internal/adapter" || calls[5][2] != "./internal/mcp" || calls[6][2] != "./internal/mcpqual" ||
		calls[7][2] != "./internal/workspace" || calls[8][2] != "./internal/workspacetransfer" || calls[9][2] != "./internal/taskworkspace" ||
		calls[10][2] != "./internal/sidecar" || calls[10][3] != "-tags=realadaptercheck" ||
		strings.Join(calls[11], " ") != "go test ./internal/devcheck -run=^$ -bench=^BenchmarkContainerEvidence$ -benchmem -benchtime=3x -count=1 -timeout=180s" {
		t.Fatalf("calls = %v", calls)
	}
	// The stress shard stages (iteration 02c, stress-plane since 05b,
	// stress-sidecar since its sidecar follow-up, stress-plane-cpu1 and
	// stress-sidecar-cpu1 since design 06a-perf) take no operands or flags,
	// and --count belongs to container-e2e alone: rejected with exit 2
	// before any child runs, and the usage names every stage.
	const usage = "usage: devcheck test | coverage [-o profile] | bench | cross | all | native | stress | stress-packages | stress-plane-cpu1 | stress-plane | stress-sidecar-cpu1 | stress-sidecar | stress-processgroup | stress-functions | container-e2e [--count=N]\n"
	calls = nil
	for _, args := range [][]string{{"stress-packages", "x"}, {"stress-plane-cpu1", "-cpu=1"}, {"stress-plane-cpu1", "extra"}, {"stress-plane-cpu1", "-count=1"},
		{"stress-plane", "-cpu=1"}, {"stress-plane", "extra"}, {"stress-sidecar-cpu1", "-cpu=2"}, {"stress-sidecar-cpu1", "extra"}, {"stress-sidecar-cpu1", "-o", "p"},
		{"stress-sidecar", "-cpu=1"}, {"stress-sidecar", "extra"}, {"stress-processgroup", "-cpu=1"}, {"stress-functions", "-count=1"}, {"stress", "-o", "p"},
		{"test", "--count=1"}, {"container-e2e", "--count=21"}, {"container-e2e", "--count=x"}} {
		errOut.Reset()
		if code := run(args, &out, &errOut, fake, opts); code != 2 || len(calls) != 0 || errOut.String() != "devcheck: invalid arguments for "+args[0]+"\n"+usage {
			t.Fatalf("%v = %d, %d calls, %q", args, code, len(calls), errOut.String())
		}
	}
	// The new stages dispatch exactly their one CPU1 invocation.
	for stage, want := range map[string]string{
		"stress-plane-cpu1":   "go test -race -count=20 -cpu=1 -timeout=6m ./internal/plane",
		"stress-sidecar-cpu1": "go test -race -count=20 -cpu=1 -timeout=6m ./internal/sidecar",
	} {
		calls = nil
		out.Reset()
		errOut.Reset()
		if code := run([]string{stage}, &out, &errOut, fake, opts); code != 0 || len(calls) != 1 || strings.Join(calls[0], " ") != want ||
			!strings.Contains(out.String(), "devcheck: stage "+stage+" ok") {
			t.Fatalf("%s = %d, calls %q, %s", stage, code, calls, errOut.String())
		}
	}
}
