//go:build !race

package mcp

import (
	"context"
	"crypto/rand"
	"strings"
	"testing"

	"github.com/wedevwork/callsheet/internal/contract"
)

// The maximum-size unit case runs in the ordinary and coverage suites but
// not in race builds (the packages stress shard): it is a size and bound
// check whose concurrency is the same as every smaller answer's (covered
// under the race detector by the writer tests), and its 14 MB of
// race-instrumented encoding would dominate the shard. The function tests
// (TestMCPTaskReads/output-bounds, TestMCPLifetime/slow-reader-max-logs)
// and BenchmarkMCPCodec cross the same maximum through the real binary.

// FP-6: the maximum retained log (10 MiB, 16 MiB serialized) crosses
// whole and byte exact, within the global frame bound.
func TestTaskLogsMaximum(t *testing.T) {
	h := start(t, withRealClock())
	h.ready()
	max := make([]byte, contract.MaxLogRetainedBytes)
	rand.Read(max)
	h.fake.taskLogs = func(_ context.Context, id string) (contract.TaskLogsResponse, error) {
		return contract.TaskLogsResponse{Version: contract.ProtocolVersion, TaskID: id, Data: max, RetainedBytes: len(max), SourceBytes: len(max)}, nil
	}
	h.nextID++
	h.call(h.nextID, toolTaskLogs, `{"id":"`+taskA+`"}`)
	line := h.nextRaw()
	// The logical JSON holds no byte that needs escaping but its quotes,
	// so the exact frame is known without decoding 14 MB.
	logical := mustEncode(t, contract.TaskLogsResponse{Version: contract.ProtocolVersion, TaskID: taskA, Data: max, RetainedBytes: len(max), SourceBytes: len(max)})
	want := `{"jsonrpc":"2.0","id":` + itoa(h.nextID) + `,"result":{"content":[{"type":"text","text":"` + strings.ReplaceAll(logical, `"`, `\"`) + `"}],"isError":false}}` + "\n"
	if len(line) > MaxFrameBytes || string(line) != want {
		t.Fatalf("maximum logs frame of %d bytes differs (want %d)", len(line), len(want))
	}
}
