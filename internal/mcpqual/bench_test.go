package mcpqual

import (
	"bufio"
	"bytes"
	"fmt"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/wedevwork/callsheet/internal/testkit"
)

// sizedFixture renders decoder's success transcript padded with ignored
// events to just under size bytes, as the harness reads it (readLines).
func sizedFixture(b *testing.B, decoder string, size int) (Transcript, []byte) {
	b.Helper()
	render := func(pad int) []byte {
		return []byte(strings.Join(fixture(decoder, scSuccess, "case-1", "nonce-1", pad), "\n") + "\n")
	}
	base, step := len(render(0)), len(render(100))-len(render(0))
	pad := 0
	if size > base {
		pad = (size - base) * 100 / step
	}
	raw := render(pad)
	for len(raw) > size && pad > 0 {
		pad -= 1 + (len(raw)-size)*100/step
		pad = max(pad, 0)
		raw = render(pad)
	}
	t := readLines(bytes.NewReader(raw), MaxEvidenceFileBytes, func() int64 { return 1 })
	if t.Truncated || len(raw) > size || len(raw) < size/2 {
		b.Fatalf("%s fixture of %d bytes for %d (truncated %v)", decoder, len(raw), size, t.Truncated)
	}
	return t, raw
}

// BenchmarkMCPQualificationTranscript decodes each vendor's fixture at
// 1 KiB and at the 8 MiB per-file limit. The normalized events must be
// exactly the fixture's correlated call and result (the padding is model
// prose mentioning timeouts, never classified), and an oversized
// transcript must end truncated and unqualified.
func BenchmarkMCPQualificationTranscript(b *testing.B) {
	reg := DefaultRegistry()
	for _, name := range decoderNames {
		dec := reg[name].decode
		for _, size := range []int{1 << 10, MaxEvidenceFileBytes} {
			t, raw := sizedFixture(b, name, size)
			b.Run(fmt.Sprintf("%s/%dB", name, size), func(b *testing.B) {
				b.SetBytes(int64(len(raw)))
				b.ReportAllocs()
				var d Decoded
				for i := 0; i < b.N; i++ {
					d = dec(t)
				}
				b.StopTimer()
				if d.Inconclusive != "" || !d.Terminal || len(d.Events) != 2 || d.Events[0].Kind != KindToolCall || d.Events[1].Kind != KindToolResult ||
					d.Events[0].CaseID != "case-1" || d.Events[1].CaseID != "case-1" || d.Events[1].Nonce != "nonce-1" || d.Events[0].RequestID != d.Events[1].RequestID {
					b.Fatalf("%s: decoded %+v", name, d.Events)
				}
			})
		}
		over := append(bytes.Repeat([]byte(" "), MaxEvidenceFileBytes), '\n')
		t := readLines(bytes.NewReader(append(over, []byte(strings.Join(fixture(name, scTimeout, "case-1", "n", 0), "\n")+"\n")...)), MaxEvidenceFileBytes, func() int64 { return 0 })
		if d := dec(t); !t.Truncated || d.Inconclusive != ReasonTruncated {
			b.Fatalf("%s: an oversized transcript classified: %+v", name, d)
		}
	}
}

// BenchmarkMCPQualificationProbe measures small-frame throughput of one
// probe session on a fake clock: each operation is a slow call with a
// progress token, one progress notification and the result. Every
// progress and result must carry its request's token, nonce and case, and
// no call or timer may be retained at the end.
func BenchmarkMCPQualificationProbe(b *testing.B) {
	cf := CaseFile{Version: 1, RunID: "bench", Nonce: "nonce-b", Cases: []ProbeCase{{CaseID: "tick", DelayMS: 2, ProgressIntervalMS: 1}}}
	inR, inW := io.Pipe()
	outR, outW := io.Pipe()
	clock := testkit.NewFakeClock(epoch)
	var events bytes.Buffer
	done := make(chan error, 1)
	go func() {
		done <- Serve(ProbeConfig{Cases: cf, In: inR, Out: outW, Events: &events, Clock: clock, Version: "bench"})
		outW.Close()
	}()
	lines := bufio.NewReader(outR)
	send := func(s string) {
		if _, err := io.WriteString(inW, s+"\n"); err != nil {
			b.Fatal(err)
		}
	}
	read := func() string {
		l, err := lines.ReadString('\n')
		if err != nil {
			b.Fatal(err)
		}
		return l
	}
	send(`{"jsonrpc":"2.0","id":0,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"bench","version":"1"}}}`)
	read()
	send(`{"jsonrpc":"2.0","method":"notifications/initialized"}`)
	armed := func(ws []testkit.Waiter) bool {
		return testkit.HasTimer(2*time.Millisecond)(ws) && testkit.HasTicker(time.Millisecond)(ws)
	}
	var frames int64
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		id := fmt.Sprint(i + 1)
		call := `{"jsonrpc":"2.0","id":` + id + `,"method":"tools/call","params":{"name":"slow","arguments":{"case_id":"tick"},"_meta":{"progressToken":"t` + id + `"}}}`
		send(call)
		if err := clock.AwaitWaiter(testWait, armed); err != nil {
			b.Fatal(err)
		}
		clock.Advance(time.Millisecond)
		prog := read()
		clock.Advance(time.Millisecond)
		res := read()
		if !strings.Contains(prog, `"progress":1,"progressToken":"t`+id+`"`) || !strings.HasPrefix(res, `{"jsonrpc":"2.0","id":`+id+`,"result"`) ||
			!strings.Contains(res, `\"case_id\":\"tick\",\"nonce\":\"nonce-b\"`) {
			b.Fatalf("op %s: %q %q", id, prog, res)
		}
		frames += int64(len(call) + len(prog) + len(res) + 1)
	}
	b.StopTimer()
	b.SetBytes(frames / int64(b.N))
	inW.Close()
	if err := <-done; err != nil {
		b.Fatal(err)
	}
	if ws := clock.Waiters(); len(ws) != 0 {
		b.Fatalf("retained timers %v", ws)
	}
	evs, intact, err := ParseProbeEvents(events.Bytes())
	if err != nil || !intact || strings.Count(kinds(evs), "receipt") != b.N || strings.Count(kinds(evs), "completed") != b.N || strings.Contains(kinds(evs), "eof,") && evs[len(evs)-2].CaseID != "" {
		b.Fatalf("events %v intact=%v: %s", err, intact, kinds(evs)[:min(200, len(kinds(evs)))])
	}
}
