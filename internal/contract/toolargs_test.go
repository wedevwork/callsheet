package contract

import (
	"strings"
	"testing"
)

// TestToolArgParsers: the per-tool argument validators the MCP handlers
// call and the offline verifier reuses, on the accepted and refused
// inputs of internal/mcp's own handler tests.
func TestToolArgParsers(t *testing.T) {
	task := "t_" + strings.Repeat("a", 32)
	t.Run("task_show", func(t *testing.T) {
		for raw, want := range map[string]int{`{"id":"` + task + `"}`: DefaultTailLines, `{"id":"` + task + `","lines":0}`: 0, `{"id":"` + task + `","lines":200}`: 200} {
			if id, lines, err := ParseTaskShowArgs([]byte(raw)); err != nil || id != task || lines != want {
				t.Errorf("%s: %s %d %v", raw, id, lines, err)
			}
		}
		for _, raw := range []string{`{"id":"` + task + `","lines":201}`, `{"id":"` + task + `","lines":-1}`, `{"id":"T"}`, `{}`, `{"id":"` + task + `","lines":null}`,
			`{"id":"` + task + `","tail":5}`} {
			if _, _, err := ParseTaskShowArgs([]byte(raw)); err == nil {
				t.Errorf("%s accepted", raw)
			}
		}
	})
	t.Run("task_ls", func(t *testing.T) {
		if after, limit, err := ParseTaskListArgs(nil); err != nil || after != "" || limit != DefaultTaskListLimit {
			t.Fatalf("default page %q %d %v", after, limit, err)
		}
		if after, limit, err := ParseTaskListArgs([]byte(`{"after":"` + task + `","limit":1}`)); err != nil || after != task || limit != 1 {
			t.Fatalf("cursor page %q %d %v", after, limit, err)
		}
		for _, raw := range []string{`{"limit":0}`, `{"limit":101}`, `{"after":"t_x"}`, `{"after":null}`, `{"cursor":"x"}`} {
			if _, _, err := ParseTaskListArgs([]byte(raw)); err == nil {
				t.Errorf("%s accepted", raw)
			}
		}
	})
	t.Run("ws_push", func(t *testing.T) {
		inst := `"instance":"` + strings.Repeat("0", 32) + `"`
		for _, raw := range []string{`{"name":"alpha",` + inst + `}`, `{"name":"alpha",` + inst + `,"branch":"topic/x","path":"src"}`,
			`{"name":"alpha",` + inst + `,"branch":"refs/heads/main","path":"/abs/dir"}`} {
			if _, err := ParseWsPushArgs([]byte(raw), "linux"); err != nil {
				t.Errorf("%s: %v", raw, err)
			}
		}
		for _, raw := range []string{`{"name":"alpha"}`, `{"name":"alpha",` + inst + `,"branch":"Main"}`, `{"name":"alpha",` + inst + `,"branch":""}`,
			`{"name":"alpha",` + inst + `,"path":""}`, `{"name":"alpha",` + inst + `,"path":"a\u0000b"}`,
			`{"name":"alpha",` + inst + `,"path":"` + strings.Repeat("a", 4096) + `"}`, `{"name":"alpha",` + inst + `,"branch":"x.lock"}`,
			`{"name":"alpha",` + inst + `,"force":true}`, `{"name":"alpha",` + inst + `,"commit":true}`} {
			if _, err := ParseWsPushArgs([]byte(raw), "linux"); err == nil {
				t.Errorf("%s accepted", raw)
			}
		}
	})
	t.Run("dispatch", func(t *testing.T) {
		rb := RequestedBy{Name: "claude-code", Version: "2.1.292", Hostname: "localhost"}
		ok := `{"target":{"kind":"id","value":"w"},"goal":"g","acceptance":"a"}`
		a, err := DecodeDispatchArgs([]byte(ok))
		if err != nil {
			t.Fatal(err)
		}
		if req, err := a.Request(rb); err != nil || req.Goal != "g" || len(req.Payload) != 0 || req.Payload == nil {
			t.Fatalf("request %+v %v", req, err)
		}
		if w, err := a.WaitDuration(); err != nil || w != nil {
			t.Fatalf("an asynchronous dispatch has wait %v %v", w, err)
		}
		withWait, _ := DecodeDispatchArgs([]byte(`{"target":{"kind":"id","value":"w"},"goal":"g","acceptance":"a","wait":"1s"}`))
		if w, err := withWait.WaitDuration(); err != nil || w == nil || w.Seconds() != 1 {
			t.Fatalf("wait %v %v", w, err)
		}
		if _, err := a.Request(RequestedBy{}); err == nil {
			t.Fatal("an invalid attribution was accepted")
		}
		for _, raw := range []string{`{"target":{"kind":"id","value":"w"},"goal":"g","acceptance":"a","wait":null}`, `{"goal":"g"}`,
			`{"target":{"kind":"id","value":"w"},"goal":"g","acceptance":"a","priority":1}`} {
			a, err := DecodeDispatchArgs([]byte(raw))
			if err == nil {
				_, err = a.Request(rb)
			}
			if err == nil {
				t.Errorf("%s accepted", raw)
			}
		}
		bad, _ := DecodeDispatchArgs([]byte(`{"target":{"kind":"id","value":"w"},"goal":"g","acceptance":"a","wait":"soon"}`))
		if _, err := bad.WaitDuration(); err == nil {
			t.Fatal("an invalid wait was accepted")
		}
		timeout, _ := DecodeDispatchArgs([]byte(`{"target":{"kind":"id","value":"w"},"goal":"g","acceptance":"a","override":{"timeout":"forever"}}`))
		if _, err := timeout.Request(rb); err == nil {
			t.Fatal("an invalid override timeout was accepted")
		}
	})
}
