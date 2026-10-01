package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/wedevwork/callsheet/internal/client"
	"github.com/wedevwork/callsheet/internal/contract"
	"github.com/wedevwork/callsheet/internal/plane"
	"github.com/wedevwork/callsheet/internal/testkit"
)

// Benchmarks (iteration 07a). Timings are reported, never gated; every
// benchmark asserts its bounds and the exact round trip.

// decodeFrameText returns a tool answer frame's text item.
func decodeFrameText(tb testing.TB, frame []byte) (string, bool) {
	tb.Helper()
	var m struct {
		Result struct {
			Content []struct{ Text string } `json:"content"`
			IsError bool                    `json:"isError"`
		} `json:"result"`
	}
	if err := json.Unmarshal(frame, &m); err != nil || len(m.Result.Content) != 1 {
		tb.Fatalf("frame does not decode: %v", err)
	}
	return m.Result.Content[0].Text, m.Result.IsError
}

// BenchmarkMCPCodec encodes the three characteristic frames: the small
// discovery answer, the maximum retained log (10 MiB) and the maximum
// role list (100 roles of maximal text fields).
func BenchmarkMCPCodec(b *testing.B) {
	id := requestID{raw: []byte(`"bench"`), key: `sbench`}
	b.Run("discovery", func(b *testing.B) {
		list, err := describeTools(newTools(NewBudget(DefaultBudget)))
		if err != nil {
			b.Fatal(err)
		}
		var f []byte
		b.ReportAllocs()
		for b.Loop() {
			f = resultFrame(id, list)
		}
		b.ReportMetric(float64(len(f)), "bytes/op")
		var m struct {
			Result json.RawMessage `json:"result"`
		}
		if err := json.Unmarshal(f, &m); err != nil || !bytes.Equal(m.Result, list) || len(f) > 64<<10 {
			b.Fatalf("discovery frame of %d bytes: %v", len(f), err)
		}
	})
	b.Run("max-logs", func(b *testing.B) {
		data := bytes.Repeat([]byte{0xff, 0x00, 'a', '"'}, contract.MaxLogRetainedBytes/4)
		r := contract.TaskLogsResponse{Version: contract.ProtocolVersion, TaskID: taskA, Data: data, RetainedBytes: len(data), SourceBytes: len(data)}
		var f []byte
		b.ReportAllocs()
		for b.Loop() {
			res, err := encodeJSON(r)
			if err != nil {
				b.Fatal(err)
			}
			f = successFrame(id, res)
		}
		b.ReportMetric(float64(len(f)), "bytes/op")
		text, isErr := decodeFrameText(b, f)
		back, err := contract.ParseTaskLogsResponse([]byte(text))
		if isErr || err != nil || !bytes.Equal(back.Data, data) || len(f) > MaxFrameBytes {
			b.Fatalf("maximum logs frame of %d bytes: %v", len(f), err)
		}
	})
	b.Run("max-role-list", func(b *testing.B) {
		views := make([]contract.RoleView, contract.MaxRoles)
		long := "/" + strings.Repeat("é", 511)
		for i := range views {
			v := roleView("role-"+itoa(i), i+1)
			v.Instruction, v.Runbook, v.Model = long, long, strings.Repeat(`"`, 1024)
			views[i] = v
		}
		r := contract.RoleListResponse{Version: contract.ProtocolVersion, Roles: views}
		var f []byte
		b.ReportAllocs()
		for b.Loop() {
			res, err := encodeJSON(r)
			if err != nil {
				b.Fatal(err)
			}
			f = successFrame(id, res)
		}
		b.ReportMetric(float64(len(f)), "bytes/op")
		text, isErr := decodeFrameText(b, f)
		want, _ := contract.Encode(r)
		if isErr || text != string(want) || len(want) > contract.MaxRoleListBytes {
			b.Fatalf("role list frame of %d bytes differs", len(f))
		}
	})
}

// benchPlane runs an in-process plane with one enrolled node and returns
// its URL, CA file and node ID.
func benchPlane(b *testing.B) (url, ca, node string) {
	b.Helper()
	root := filepath.Join(b.TempDir(), "plane")
	ll := &listenLog{addr: make(chan string, 1)}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- plane.Run(ctx, plane.RunOptions{StateDir: root, Bind: "127.0.0.1:0", BindSet: true, SANs: []string{"127.0.0.1"}, SANsSet: true, Logger: slog.New(ll)})
	}()
	b.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(testWait):
			b.Error("the plane did not stop")
		}
	})
	select {
	case a := <-ll.addr:
		url = "https://" + a
	case err := <-done:
		b.Fatalf("plane: %v", err)
	case <-time.After(testWait):
		b.Fatal("the plane did not listen")
	}
	ca = filepath.Join(root, "pki", "ca.crt")
	pem, err := os.ReadFile(ca)
	if err != nil {
		b.Fatal(err)
	}
	c, err := client.New(url, client.Trust{CAPEM: pem})
	if err != nil {
		b.Fatal(err)
	}
	defer c.Close()
	node = "n_" + strings.Repeat("7", 32)
	if _, err := c.EnrollNode(context.Background(), node, "bench-1"); err != nil {
		b.Fatal(err)
	}
	return url, ca, node
}

// listenLog captures the plane's listening address.
type listenLog struct{ addr chan string }

func (l *listenLog) Enabled(context.Context, slog.Level) bool { return true }
func (l *listenLog) Handle(_ context.Context, r slog.Record) error {
	if r.Message == "listening" {
		r.Attrs(func(a slog.Attr) bool {
			if a.Key == "bind" {
				select {
				case l.addr <- a.Value.String():
				default:
				}
			}
			return true
		})
	}
	return nil
}
func (l *listenLog) WithAttrs([]slog.Attr) slog.Handler { return l }
func (l *listenLog) WithGroup(string) slog.Handler      { return l }

// BenchmarkMCPRelay is one node_show end to end over the session and the
// real local TLS plane: fresh trust resolution, verified TLS and the
// exact answer.
func BenchmarkMCPRelay(b *testing.B) {
	url, ca, node := benchPlane(b)
	h := start(b, withRealClock(), withFactory(PlaneFactory(url, ca, "")))
	h.ready()
	args := `{"id":"` + node + `"}`
	b.ReportAllocs()
	for b.Loop() {
		h.nextID++
		h.call(h.nextID, toolNodeShow, args)
		text, isErr := decodeFrameText(b, h.nextRaw())
		n, err := contract.ParseNodeResponse([]byte(text))
		if isErr || err != nil || n.ID != node || n.Liveness != contract.LivenessOffline || n.LastSeen != nil {
			b.Fatalf("node_show %s: %v", text, err)
		}
		h.await(StageReleased, "n"+itoa(h.nextID))
	}
}

// BenchmarkMCPWaitBudget is a task_wait of 1 and 16 IDs through admission,
// budget arming, the fake client and delivery; no call, slot, reservation
// or timer is retained afterwards.
func BenchmarkMCPWaitBudget(b *testing.B) {
	for _, n := range []int{1, 16} {
		b.Run("ids-"+itoa(n), func(b *testing.B) {
			h := start(b)
			happy(h.fake)
			h.ready()
			ids := make([]string, n)
			for i := range ids {
				ids[i] = "t_" + strings.Repeat(string("0123456789abcdef"[i]), 32)
			}
			raw, _ := json.Marshal(ids)
			args := `{"task_ids":` + string(raw) + `}`
			b.ReportAllocs()
			for b.Loop() {
				h.nextID++
				h.call(h.nextID, toolTaskWait, args)
				text, isErr := decodeFrameText(b, h.nextRaw())
				if r, err := contract.ParseWaitResponse([]byte(text), ids, 8*time.Second); isErr || err != nil || len(r.Tasks) != n || r.EffectiveWaitMS != 8000 {
					b.Fatalf("task_wait %s: %v", text, err)
				}
				// The slot is freed when the writer takes the answer; its
				// call entry and reserved ID are retired once the answer is
				// written (StageWritten), after which nothing may remain.
				h.await(StageReleased, "n"+itoa(h.nextID))
				h.await(StageWritten, "n"+itoa(h.nextID))
			}
			b.StopTimer()
			h.s.mu.Lock()
			retained := len(h.s.calls) + len(h.s.reserved) + h.s.active
			h.s.mu.Unlock()
			if retained != 0 {
				b.Fatalf("%d operations retained", retained)
			}
			if err := h.clock.AwaitWaiter(testWait, func(ws []testkit.Waiter) bool { return len(ws) == 0 }); err != nil {
				b.Fatal(err)
			}
		})
	}
}
