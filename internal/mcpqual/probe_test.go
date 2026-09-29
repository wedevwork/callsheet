package mcpqual

import (
	"errors"
	"io"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"
)

// FP-10 unit tests: the probe's handshake, discovery, framing, ID and token
// type preservation, notification envelopes, delay, progress, cancellation
// and EOF, all on a fake clock advanced only after the expected timer or
// ticker is armed. No real process and no real wait.

func TestProbeHandshake(t *testing.T) {
	h := startProbe(t, testCases())
	h.send(`{"jsonrpc":"2.0","id":1,"method":"ping"}`,
		`{"jsonrpc":"2.0","id":2,"method":"tools/list"}`,
		`{"jsonrpc":"2.0","id":3,"method":"initialize","params":{"protocolVersion":"2024-11-05","capabilities":{},"clientInfo":{"name":"x","version":"1"}}}`,
		`{"jsonrpc":"2.0","id":4,"method":"initialize","params":{"protocolVersion":"2025-06-18","clientInfo":{"name":"x","version":"1"}}}`,
		`{"jsonrpc":"2.0","id":5,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"Claude Code (probe)","version":"2.1.282 beta"}}}`,
		`{"jsonrpc":"2.0","id":6,"method":"tools/list"}`,
		`{"jsonrpc":"2.0","method":"notifications/initialized"}`,
		`{"jsonrpc":"2.0","id":7,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"x","version":"1"}}}`,
		`{"jsonrpc":"2.0","id":8,"method":"tools/list"}`)
	lines := h.out.await(t, "eight answers", countLines(`"jsonrpc":"2.0","id":`, 8))
	want := []string{
		`{"jsonrpc":"2.0","id":1,"result":{}}`,
		`{"jsonrpc":"2.0","id":2,"error":{"code":-32602,"message":"invalid params: the session is not initialized"}}`,
		`{"jsonrpc":"2.0","id":3,"error":{"code":-32602,"message":"invalid params: unsupported protocol version; this probe accepts 2025-06-18 only"}}`,
		`{"jsonrpc":"2.0","id":4,"error":{"code":-32602,"message":"invalid params: initialize needs protocolVersion, capabilities and clientInfo name and version strings"}}`,
		`{"jsonrpc":"2.0","id":5,"result":{"capabilities":{"tools":{"listChanged":false}},"protocolVersion":"2025-06-18","serverInfo":{"name":"mcpqual","version":"test"}}}`,
		`{"jsonrpc":"2.0","id":6,"error":{"code":-32602,"message":"invalid params: the session is not initialized"}}`,
		`{"jsonrpc":"2.0","id":7,"error":{"code":-32600,"message":"invalid request: already initialized"}}`,
		`{"jsonrpc":"2.0","id":8,"result":` + string(toolsListResult) + `}`,
	}
	for i, w := range want {
		if lines[i] != w {
			t.Fatalf("answer %d:\n got %s\nwant %s", i, lines[i], w)
		}
	}
	if err := h.end(); err != nil {
		t.Fatal(err)
	}
	evs := parseEvents(t, h.events.snapshot())
	if kinds(evs) != "start,initialize,eof,exit" || *evs[1].ClientName != "Claude Code (probe)" || *evs[1].ClientVersion != "2.1.282 beta" {
		t.Fatalf("events %+v", evs)
	}
}

func TestProbeFraming(t *testing.T) {
	h := startProbe(t, testCases())
	cases := []struct{ in, want string }{
		{`not json`, `{"jsonrpc":"2.0","id":null,"error":{"code":-32700,"message":"parse error: malformed JSON"}}`},
		{"{\"jsonrpc\":\"2.0\",\"id\":1,\"method\":\"p\xffing\"}", `{"jsonrpc":"2.0","id":null,"error":{"code":-32700,"message":"parse error: not valid UTF-8"}}`},
		{`{"jsonrpc":"2.0","id":1,"id":2,"method":"ping"}`, `{"jsonrpc":"2.0","id":null,"error":{"code":-32700,"message":"parse error: duplicate object key \"id\""}}`},
		{`{"jsonrpc":"2.0","id":1,"method":"ping","params":` + strings.Repeat(`{"a":`, 33) + `1` + strings.Repeat(`}`, 33) + `}`, `{"jsonrpc":"2.0","id":null,"error":{"code":-32700,"message":"parse error: nesting deeper than 32 levels"}}`},
		{`[{"jsonrpc":"2.0","id":1,"method":"ping"}]`, `{"jsonrpc":"2.0","id":null,"error":{"code":-32600,"message":"invalid request: a message is one JSON object (batches are not supported)"}}`},
		{`{"jsonrpc":"2.0","id":9}`, `{"jsonrpc":"2.0","id":9,"error":{"code":-32600,"message":"invalid request: no method"}}`},
		{`{"jsonrpc":"2.0","id":10,"method":"ping","extra":1}`, `{"jsonrpc":"2.0","id":10,"error":{"code":-32600,"message":"invalid request: unknown member \"extra\""}}`},
		{`{"jsonrpc":"1.0","id":11,"method":"ping"}`, `{"jsonrpc":"2.0","id":11,"error":{"code":-32600,"message":"invalid request: jsonrpc must be \"2.0\""}}`},
		{`{"jsonrpc":"2.0","id":12,"method":7}`, `{"jsonrpc":"2.0","id":12,"error":{"code":-32600,"message":"invalid request: method must be a string"}}`},
		{`{"jsonrpc":"2.0","id":13,"method":"ping","result":1}`, `{"jsonrpc":"2.0","id":13,"error":{"code":-32600,"message":"invalid request: a request carries no result or error"}}`},
		{`{"jsonrpc":"2.0","id":14,"method":"ping","params":[]}`, `{"jsonrpc":"2.0","id":14,"error":{"code":-32602,"message":"invalid params: params must be an object"}}`},
		{`{"jsonrpc":"2.0","id":null,"method":"ping"}`, `{"jsonrpc":"2.0","id":null,"error":{"code":-32600,"message":"invalid request: the id must be a string of at most 128 bytes or an integer within ±(2^53-1)"}}`},
		{`{"jsonrpc":"2.0","id":1.5,"method":"ping"}`, `{"jsonrpc":"2.0","id":null,"error":{"code":-32600,"message":"invalid request: the id must be a string of at most 128 bytes or an integer within ±(2^53-1)"}}`},
		{`{"jsonrpc":"2.0","id":9007199254740992,"method":"ping"}`, `{"jsonrpc":"2.0","id":null,"error":{"code":-32600,"message":"invalid request: the id must be a string of at most 128 bytes or an integer within ±(2^53-1)"}}`},
		{`{"jsonrpc":"2.0","id":"` + strings.Repeat("x", 129) + `","method":"ping"}`, `{"jsonrpc":"2.0","id":null,"error":{"code":-32600,"message":"invalid request: the id must be a string of at most 128 bytes or an integer within ±(2^53-1)"}}`},
		{`{"jsonrpc":"2.0","method":"notifications/unknown"}`, ``},
		{`{"jsonrpc":"2.0","method":"notifications/initialized","params":[]}`, ``},
		{`{"jsonrpc":"1.0","method":"notifications/initialized"}`, ``},
		{`{"jsonrpc":"2.0","id":"r1","result":{}}`, ``},
		{`{"jsonrpc":"2.0","id":"s-7","method":"ping"}` + "\r", `{"jsonrpc":"2.0","id":"s-7","result":{}}`},
		{`{"jsonrpc":"2.0","id":-3,"method":"ping"}`, `{"jsonrpc":"2.0","id":-3,"result":{}}`},
		{`{"jsonrpc":"2.0","id":7,"method":"nope"}`, `{"jsonrpc":"2.0","id":7,"error":{"code":-32601,"message":"method not found: \"nope\""}}`},
	}
	for _, c := range cases {
		h.send(c.in)
		h.send(`{"jsonrpc":"2.0","id":"sync","method":"ping"}`)
		lines := h.out.await(t, "sync "+c.in, hasLine(`"id":"sync"`))
		got := lines[:len(lines)-1]
		switch {
		case c.want == "" && len(got) != 0:
			t.Fatalf("%s answered %q", c.in, got)
		case c.want != "" && (len(got) != 1 || got[0] != c.want):
			t.Fatalf("%s:\n got %q\nwant %s", c.in, got, c.want)
		}
		h.out.mu.Lock()
		h.out.lines = nil
		h.out.mu.Unlock()
	}
	if err := h.end(); err != nil {
		t.Fatal(err)
	}
}

func TestProbeFatalFrames(t *testing.T) {
	for name, in := range map[string]string{
		"oversize":  `{"jsonrpc":"2.0","id":1,"method":"ping","params":{"x":"` + strings.Repeat("a", MaxFrameBytes) + `"}}` + "\n",
		"truncated": `{"jsonrpc":"2.0","id":1,"method":"ping"}`,
	} {
		t.Run(name, func(t *testing.T) {
			h := startProbe(t, testCases())
			go func() { io.WriteString(h.in, in); h.in.Close() }()
			err := <-h.done
			h.done <- err
			if !errors.Is(err, ErrProbeFatal) {
				t.Fatalf("err = %v", err)
			}
			evs := parseEvents(t, h.events.snapshot())
			if kinds(evs) != "start,error,exit" || len(h.out.snapshot()) != 0 {
				t.Fatalf("events %s out %q", kinds(evs), h.out.snapshot())
			}
		})
	}
}

func TestProbeCallArguments(t *testing.T) {
	h := startProbe(t, testCases())
	h.ready()
	bad := map[string]string{
		`{"name":"other","arguments":{"case_id":"slow"}}`:                                                                       "unknown tool",
		`{"name":"slow","arguments":{"case_id":"slow","x":1}}`:                                                                  "exactly {case_id: string}",
		`{"name":"slow","arguments":{}}`:                                                                                        "exactly {case_id: string}",
		`{"name":"slow","arguments":{"case_id":7}}`:                                                                             "exactly {case_id: string}",
		`{"name":"slow","arguments":{"case_id":"nope"}}`:                                                                        "not in the case file",
		`{"name":"slow","arguments":{"case_id":"slow"},"extra":1}`:                                                              "unknown member",
		`{"name":"slow","arguments":{"case_id":"slow"},"_meta":[]}`:                                                             "_meta must be an object",
		`{"name":"slow","arguments":{"case_id":"slow"},"_meta":{"progressToken":null}}`:                                         "string or a number",
		`{"name":"slow","arguments":{"case_id":"slow"},"_meta":{"progressToken":{}}}`:                                           "string or a number",
		`{"name":"slow","arguments":{"case_id":"slow"},"_meta":{"progressToken":[1]}}`:                                          "string or a number",
		`{"name":"slow","arguments":{"case_id":"slow"},"_meta":{"progressToken":true}}`:                                         "string or a number",
		`{"name":"slow","arguments":{"case_id":"slow"},"_meta":{"progressToken":"` + strings.Repeat("t", maxTokenBytes) + `"}}`: "exceeds",
	}
	i := 100
	for params, want := range bad {
		i++
		id := strconv.Itoa(i)
		h.send(`{"jsonrpc":"2.0","id":` + id + `,"method":"tools/call","params":` + params + `}`)
		lines := h.out.await(t, params, hasLine(`"id":`+id+`,`))
		last := lines[len(lines)-1]
		if !strings.Contains(last, `"code":-32602`) || !strings.Contains(last, want) {
			t.Fatalf("%s -> %s", params, last)
		}
	}
	if err := h.end(); err != nil {
		t.Fatal(err)
	}
	if evs := parseEvents(t, h.events.snapshot()); strings.Contains(kinds(evs), "receipt") {
		t.Fatalf("a rejected call was received: %s", kinds(evs))
	}
}

func TestProbeSilentDelay(t *testing.T) {
	h := startProbe(t, testCases())
	h.ready()
	h.call(`"req-1"`, "silent", "")
	h.awaitTimer(3 * time.Second)
	if lines := h.out.snapshot(); hasLine(`"req-1"`)(lines) {
		t.Fatalf("answered before the delay: %q", lines)
	}
	h.clock.Advance(3 * time.Second)
	lines := h.out.await(t, "result", hasLine(`"id":"req-1"`))
	want := `{"jsonrpc":"2.0","id":"req-1","result":{"content":[{"text":"{\"case_id\":\"silent\",\"nonce\":\"nonce1\"}","type":"text"}],"isError":false}}`
	if lines[len(lines)-1] != want {
		t.Fatalf("got %s", lines[len(lines)-1])
	}
	if err := h.end(); err != nil {
		t.Fatal(err)
	}
	evs := parseEvents(t, h.events.snapshot())
	if kinds(evs) != "start,initialize,receipt,scheduled,completed,eof,exit" || *evs[2].TokenPresent || string(evs[2].RequestID) != `"req-1"` ||
		evs[4].OffsetNS-evs[2].OffsetNS != int64(3*time.Second) {
		t.Fatalf("events %+v", evs)
	}
}

func TestProbeProgress(t *testing.T) {
	for _, tok := range []string{`"tok-a"`, `42`, `-7.5`} {
		t.Run(tok, func(t *testing.T) {
			h := startProbe(t, testCases())
			h.ready()
			h.call(`9`, "slow", `{"progressToken":`+tok+`,"other":1}`)
			h.awaitTimer(4 * time.Second)
			h.awaitTicker(time.Second)
			for i := 1; i <= 3; i++ {
				h.clock.Advance(time.Second)
				h.out.await(t, "progress", countLines(`notifications/progress`, i))
			}
			// The fourth tick and the completion timer are due at the same
			// instant: the result wins however the two are received.
			h.clock.Advance(time.Second)
			lines := h.out.await(t, "result", hasLine(`"id":9,"result"`))
			h.clock.Advance(5 * time.Second)
			h.send(`{"jsonrpc":"2.0","id":"after","method":"ping"}`)
			lines = h.out.await(t, "ping", hasLine(`"id":"after"`))
			var progress []string
			for _, l := range lines {
				if strings.Contains(l, "notifications/progress") {
					progress = append(progress, l)
				}
			}
			for i, p := range progress {
				want := `{"jsonrpc":"2.0","method":"notifications/progress","params":{"progress":` + strconv.Itoa(i+1) + `,"progressToken":` + tok + `}}`
				if p != want {
					t.Fatalf("progress %d = %s, want %s", i, p, want)
				}
			}
			if len(progress) != 3 || !strings.Contains(lines[len(lines)-2], `"id":9,"result"`) {
				t.Fatalf("lines %q", lines)
			}
			h.end()
			evs := parseEvents(t, h.events.snapshot())
			if kinds(evs) != "start,initialize,receipt,progress,progress,progress,scheduled,completed,eof,exit" || string(evs[2].Token) != tok || evs[7].Progress != 3 {
				t.Fatalf("events %s %+v", kinds(evs), evs[2])
			}
		})
	}
}

func TestProbeNoTokenNoProgress(t *testing.T) {
	h := startProbe(t, testCases())
	h.ready()
	h.call(`1`, "slow", `{}`)
	h.awaitTimer(4 * time.Second)
	for _, w := range h.clock.Waiters() {
		if w.Ticker {
			t.Fatal("a ticker was armed without a progress token")
		}
	}
	h.clock.Advance(4 * time.Second)
	lines := h.out.await(t, "result", hasLine(`"id":1,"result"`))
	if countLines("notifications/progress", 1)(lines) {
		t.Fatalf("progress without a token: %q", lines)
	}
	h.end()
	evs := parseEvents(t, h.events.snapshot())
	if evs[2].Kind != EvReceipt || *evs[2].TokenPresent || evs[2].Token != nil {
		t.Fatalf("receipt %+v", evs[2])
	}
}

func TestProbeZeroDelay(t *testing.T) {
	h := startProbe(t, testCases())
	h.ready()
	h.call(`"z"`, "zero", `{"progressToken":"t"}`)
	h.out.await(t, "immediate result", hasLine(`"id":"z","result"`))
	if ws := h.clock.Waiters(); len(ws) != 0 {
		t.Fatalf("zero delay armed %v", ws)
	}
	h.end()
}

func TestProbeCancellationOrders(t *testing.T) {
	t.Run("cancel-before-deadline", func(t *testing.T) {
		h := startProbe(t, testCases())
		h.ready()
		h.call(`5`, "slow", `{"progressToken":"p"}`)
		h.awaitTimer(4 * time.Second)
		h.send(`{"jsonrpc":"2.0","method":"notifications/cancelled","params":{"requestId":"5"}}`) // a string does not match integer 5
		h.send(`{"jsonrpc":"2.0","method":"notifications/cancelled","params":{"requestId":5,"reason":"timeout"}}`)
		h.events.await(t, "cancelled", hasLine(`"kind":"cancelled"`))
		if len(h.clock.Waiters()) != 0 {
			t.Fatalf("timers survive cancellation: %v", h.clock.Waiters())
		}
		h.clock.Advance(10 * time.Second)
		h.send(`{"jsonrpc":"2.0","id":"p2","method":"ping"}`)
		lines := h.out.await(t, "ping", hasLine(`"id":"p2"`))
		if hasLine(`"id":5`)(lines) || hasLine("notifications/progress")(lines) {
			t.Fatalf("late output after cancellation: %q", lines)
		}
		// The session stays usable: a new call on the same case works.
		h.call(`6`, "zero", "")
		h.out.await(t, "second call", hasLine(`"id":6,"result"`))
		h.end()
		if k := kinds(parseEvents(t, h.events.snapshot())); k != "start,initialize,receipt,cancelled,receipt,scheduled,completed,eof,exit" {
			t.Fatalf("events %s", k)
		}
	})
	t.Run("result-before-cancel", func(t *testing.T) {
		h := startProbe(t, testCases())
		h.ready()
		h.call(`5`, "silent", "")
		h.awaitTimer(3 * time.Second)
		h.clock.Advance(3 * time.Second)
		h.out.await(t, "result", hasLine(`"id":5,"result"`))
		h.send(`{"jsonrpc":"2.0","method":"notifications/cancelled","params":{"requestId":5}}`)
		h.events.await(t, "late cancel", hasLine(`"kind":"late_cancel"`))
		h.end()
		if k := kinds(parseEvents(t, h.events.snapshot())); k != "start,initialize,receipt,scheduled,completed,late_cancel,eof,exit" {
			t.Fatalf("events %s", k)
		}
	})
	t.Run("eof-before-deadline", func(t *testing.T) {
		h := startProbe(t, testCases())
		h.ready()
		h.call(`5`, "silent", "")
		h.awaitTimer(3 * time.Second)
		if err := h.end(); err != nil {
			t.Fatal(err)
		}
		h.clock.Advance(3 * time.Second)
		evs := parseEvents(t, h.events.snapshot())
		if kinds(evs) != "start,initialize,receipt,eof,exit" || evs[3].CaseID != "silent" || hasLine(`"id":5`)(h.out.snapshot()) {
			t.Fatalf("events %s", kinds(evs))
		}
	})
	t.Run("result-before-eof", func(t *testing.T) {
		h := startProbe(t, testCases())
		h.ready()
		h.call(`5`, "silent", "")
		h.awaitTimer(3 * time.Second)
		h.clock.Advance(3 * time.Second)
		h.out.await(t, "result", hasLine(`"id":5,"result"`))
		h.end()
		if k := kinds(parseEvents(t, h.events.snapshot())); k != "start,initialize,receipt,scheduled,completed,eof,exit" {
			t.Fatalf("events %s", k)
		}
	})
}

func TestProbeConcurrentCallRejected(t *testing.T) {
	h := startProbe(t, testCases())
	h.ready()
	h.call(`1`, "silent", "")
	h.awaitTimer(3 * time.Second)
	h.call(`2`, "zero", "")
	lines := h.out.await(t, "rejection", hasLine(`"id":2,`))
	if !strings.Contains(lines[len(lines)-1], "already active") {
		t.Fatalf("got %s", lines[len(lines)-1])
	}
	h.events.await(t, "rejected", hasLine(`"kind":"rejected"`))
	h.end()
}

func TestProbeFatalOutputs(t *testing.T) {
	t.Run("write-failure", func(t *testing.T) {
		h := startProbe(t, testCases(), func(c *ProbeConfig) {
			s := newSink()
			s.failAt = 1
			c.Out = s
		})
		h.send(`{"jsonrpc":"2.0","id":1,"method":"ping"}`)
		err := <-h.done
		h.done <- err
		if !errors.Is(err, ErrProbeFatal) || !strings.Contains(err.Error(), "write failed") {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("event-log-exhausted", func(t *testing.T) {
		h := startProbe(t, testCases(), func(c *ProbeConfig) { c.EventLimit = exitReserve + 100 })
		h.send(`{"jsonrpc":"2.0","id":0,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"a long client name","version":"1"}}}`)
		err := <-h.done
		h.done <- err
		if !errors.Is(err, ErrProbeFatal) || !strings.Contains(err.Error(), "exhausted") {
			t.Fatalf("err = %v", err)
		}
		evs, intact, perr := ParseProbeEvents([]byte(strings.Join(h.events.snapshot(), "\n") + "\n"))
		if perr != nil || !intact || evs[len(evs)-1].Reason == "" {
			t.Fatalf("events %+v intact=%v err=%v", evs, intact, perr)
		}
	})
	t.Run("output-frame-bound", func(t *testing.T) {
		p := &probe{cfg: ProbeConfig{Out: newSink()}, dead: make(chan struct{})}
		if err := p.writeLocked(make([]byte, MaxFrameBytes+2)); err == nil || p.fatal == nil {
			t.Fatal("an oversized output frame was written")
		}
	})
}

func TestProbeInvalidCaseFile(t *testing.T) {
	if err := Serve(ProbeConfig{Cases: CaseFile{Version: 2}}); err == nil {
		t.Fatal("invalid case file served")
	}
}

func TestCaseFileValidation(t *testing.T) {
	good := testCases()
	if b := mustJSON(t, good); true {
		if _, err := ParseCaseFile(b); err != nil {
			t.Fatal(err)
		}
	}
	for name, f := range map[string]func(*CaseFile){
		"version":  func(c *CaseFile) { c.Version = 2 },
		"run":      func(c *CaseFile) { c.RunID = "has space" },
		"nonce":    func(c *CaseFile) { c.Nonce = "" },
		"none":     func(c *CaseFile) { c.Cases = nil },
		"many":     func(c *CaseFile) { c.Cases = make([]ProbeCase, maxCases+1) },
		"case id":  func(c *CaseFile) { c.Cases[0].CaseID = "../x" },
		"dup":      func(c *CaseFile) { c.Cases[1].CaseID = c.Cases[0].CaseID },
		"delay":    func(c *CaseFile) { c.Cases[0].DelayMS = -1 },
		"interval": func(c *CaseFile) { c.Cases[0].ProgressIntervalMS = MaxDelayMS + 1 },
	} {
		c := testCases()
		f(&c)
		if _, err := ParseCaseFile(mustJSON(t, c)); err == nil {
			t.Errorf("%s accepted", name)
		}
	}
	for name, raw := range map[string]string{
		"unknown field": `{"version":1,"run_id":"r","nonce":"n","cases":[{"case_id":"a","delay_ms":0,"progress_interval_ms":0}],"x":1}`,
		"oversize":      `{"version":1,"pad":"` + strings.Repeat("x", MaxCaseFileBytes) + `"}`,
	} {
		if _, err := ParseCaseFile([]byte(raw)); err == nil {
			t.Errorf("%s accepted", name)
		}
	}
}

func TestParseProbeEvents(t *testing.T) {
	good := `{"seq":1,"kind":"start","offset_ns":0,"run_id":"r"}` + "\n" + `{"seq":2,"kind":"exit","offset_ns":5,"run_id":"r"}` + "\n"
	second := `{"seq":1,"kind":"start","offset_ns":0,"run_id":"r"}` + "\n" + `{"seq":2,"kind":"eof","offset_ns":1,"run_id":"r"}` + "\n"
	if evs, intact, err := ParseProbeEvents([]byte(good + second)); err != nil || intact || len(evs) != 4 {
		t.Fatalf("two instances: %v %v %v", evs, intact, err)
	}
	for name, raw := range map[string]string{
		"seq":       `{"seq":2,"kind":"start","offset_ns":0,"run_id":"r"}` + "\n",
		"first":     `{"seq":1,"kind":"eof","offset_ns":0,"run_id":"r"}` + "\n",
		"backwards": `{"seq":1,"kind":"start","offset_ns":9,"run_id":"r"}` + "\n" + `{"seq":2,"kind":"eof","offset_ns":1,"run_id":"r"}` + "\n",
		"unknown":   `{"seq":1,"kind":"start","offset_ns":0,"run_id":"r","x":1}` + "\n",
		"oversize":  strings.Repeat(" ", MaxEvidenceFileBytes+1),
	} {
		if _, _, err := ParseProbeEvents([]byte(raw)); err == nil {
			t.Errorf("%s accepted", name)
		}
	}
}

// C9 support: a stalled probe (the clock jumps past the due instant at
// once) completes a gated case only after its min_progress notifications;
// the ungated case documents the stall schedule that had none.
func TestProbeMinProgressGate(t *testing.T) {
	count := func(lines []string) int {
		return len(slices.DeleteFunc(slices.Clone(lines), func(l string) bool { return !strings.Contains(l, "notifications/progress") }))
	}
	t.Run("stall-ungated", func(t *testing.T) {
		h := startProbe(t, testCases())
		h.ready()
		h.call(`1`, "slow", `{"progressToken":"t"}`)
		h.awaitTimer(4 * time.Second)
		h.awaitTicker(time.Second)
		h.clock.Advance(10 * time.Second)
		if n := count(h.out.await(t, "result", hasLine(`"id":1,"result"`))); n != 0 {
			t.Fatalf("%d progress before the stalled result", n)
		}
		h.end()
	})
	t.Run("stall-gated", func(t *testing.T) {
		h := startProbe(t, testCases())
		h.ready()
		h.call(`2`, "gated", `{"progressToken":"t"}`)
		h.awaitTimer(3 * time.Second)
		h.awaitTicker(time.Second)
		h.clock.Advance(10 * time.Second)
		lines := h.out.await(t, "result", hasLine(`"id":2,"result"`))
		if n := count(lines); n != 1 || !strings.Contains(lines[len(lines)-2], "notifications/progress") {
			t.Fatalf("gated result after %d progress: %q", n, lines)
		}
		h.end()
	})
	t.Run("timer-before-enough-progress", func(t *testing.T) {
		h := startProbe(t, testCases())
		h.ready()
		h.call(`3`, "gated2", `{"progressToken":"t"}`)
		h.awaitTimer(time.Second)
		h.awaitTicker(time.Second)
		h.clock.Advance(time.Second) // due, but only one notification can be sent
		h.out.await(t, "first progress", countLines("notifications/progress", 1))
		h.send(`{"jsonrpc":"2.0","id":"p","method":"ping"}`)
		if lines := h.out.await(t, "ping", hasLine(`"id":"p"`)); hasLine(`"id":3,"result"`)(lines) {
			t.Fatalf("completed before its second progress: %q", lines)
		}
		h.clock.Advance(time.Second)
		lines := h.out.await(t, "result", hasLine(`"id":3,"result"`))
		if n := count(lines); n != 2 {
			t.Fatalf("%d progress", n)
		}
		h.end()
		if k := kinds(parseEvents(t, h.events.snapshot())); k != "start,initialize,receipt,progress,progress,scheduled,completed,eof,exit" {
			t.Fatalf("events %s", k)
		}
	})
	t.Run("no-token-ignores-gate", func(t *testing.T) {
		h := startProbe(t, testCases())
		h.ready()
		h.call(`4`, "gated", `{}`)
		h.awaitTimer(3 * time.Second)
		h.clock.Advance(3 * time.Second)
		h.out.await(t, "result", hasLine(`"id":4,"result"`))
		h.end()
	})
	for name, c := range map[string]ProbeCase{"no-interval": {CaseID: "x", DelayMS: 5, MinProgress: 1}, "no-delay": {CaseID: "x", ProgressIntervalMS: 5, MinProgress: 1},
		"negative": {CaseID: "x", DelayMS: 5, ProgressIntervalMS: 5, MinProgress: -1}, "too-many": {CaseID: "x", DelayMS: 5, ProgressIntervalMS: 5, MinProgress: maxMinProgress + 1}} {
		if err := (CaseFile{Version: 1, RunID: "r", Nonce: "n", Cases: []ProbeCase{c}}).Validate(); err == nil {
			t.Errorf("%s min_progress accepted", name)
		}
	}
}
