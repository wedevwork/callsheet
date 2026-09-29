package mcpqual

import (
	"strings"
	"testing"
)

// FP-12 unit tests: four decoder fixture sets, exact version selection,
// timeout versus session/auth/refusal failures, unknown events and
// malformed or truncated transcripts. Model prose never classifies.

func TestDecoderFixtureSets(t *testing.T) {
	reg := DefaultRegistry()
	for _, name := range decoderNames {
		spec := reg[name]
		dec := spec.decode
		for _, sc := range []string{scSuccess, scTimeout, scToolError, scAuth, scPermission, scRefusal, scSession, scUnknownEnd, scNoCall, scTwoCalls, scNoModel} {
			t.Run(name+"/"+sc, func(t *testing.T) {
				d := dec(transcriptOf(fixture(name, sc, "case-1", "n-1", 3)))
				count := func(kind string) (n int, ev Event) {
					for _, e := range d.Events {
						if e.Kind == kind {
							n++
							ev = e
						}
					}
					return
				}
				if n, _ := count(KindMCPTimeout); n > 0 && sc != scTimeout {
					t.Fatalf("model prose or another event became a timeout: %+v", d.Events)
				}
				calls, call := count(KindToolCall)
				switch sc {
				case scSuccess:
					n, r := count(KindToolResult)
					if d.Inconclusive != "" || !d.Terminal || calls != 1 || call.CaseID != "case-1" || n != 1 || r.CaseID != "case-1" || r.Nonce != "n-1" || r.RequestID != call.RequestID {
						t.Fatalf("decoded %+v", d)
					}
				case scTimeout:
					n, e := count(KindMCPTimeout)
					if d.Inconclusive != "" || n != 1 || e.CaseID != "case-1" {
						t.Fatalf("decoded %+v", d)
					}
				case scToolError:
					if n, e := count(KindToolError); n != 1 || e.CaseID != "case-1" {
						t.Fatalf("decoded %+v", d)
					}
				case scAuth:
					if n, e := count(KindAuthError); n != 1 || e.CaseID != "" || !d.Terminal {
						t.Fatalf("decoded %+v", d)
					}
				case scPermission:
					if n, _ := count(KindPermissionDenied); n != 1 {
						t.Fatalf("decoded %+v", d)
					}
				case scRefusal:
					if n, _ := count(KindModelRefusal); n != 1 || calls != 0 {
						t.Fatalf("decoded %+v", d)
					}
				case scSession:
					if n, _ := count(KindSessionError); n != 1 {
						t.Fatalf("decoded %+v", d)
					}
				case scNoModel:
					if n, e := count(KindSessionError); n != 1 || e.SafeReason != SafeModelUnavailable {
						t.Fatalf("decoded %+v", d)
					}
				case scUnknownEnd:
					if !strings.HasPrefix(d.Inconclusive, ReasonUnrecognized) {
						t.Fatalf("an unrecognized terminal event classified: %+v", d)
					}
				case scNoCall:
					if calls != 0 || !d.Terminal || d.Inconclusive != "" {
						t.Fatalf("decoded %+v", d)
					}
				case scTwoCalls:
					if calls != 2 {
						t.Fatalf("decoded %+v", d)
					}
				}
			})
		}
		t.Run(name+"/bounds", func(t *testing.T) {
			if d := dec(Transcript{}); d.Inconclusive != ReasonNoTranscript {
				t.Fatalf("empty: %+v", d)
			}
			tr := transcriptOf(fixture(name, scSuccess, "case-1", "n-1", 0))
			tr.Truncated = true
			if d := dec(tr); d.Inconclusive != ReasonTruncated {
				t.Fatalf("truncated: %+v", d)
			}
			if d := dec(transcriptOf([]string{`{"type":`})); d.Inconclusive != ReasonMalformed {
				t.Fatalf("malformed: %+v", d)
			}
		})
	}
}

func TestDecoderUnrecognizedToolErrors(t *testing.T) {
	reg := DefaultRegistry()
	for name, lines := range map[string][]string{
		"claude-json": {`[{"type":"assistant","message":{"content":[{"type":"tool_use","id":"t","name":"mcp__p__slow","input":{"case_id":"c"}}]}},` +
			`{"type":"user","message":{"content":[{"type":"tool_result","tool_use_id":"t","is_error":true,"content":[{"type":"text","text":"it timed out"}]}]}}]`},
		"codex-jsonl": {`{"type":"item.started","item":{"id":"i","type":"mcp_tool_call","tool":"slow","arguments":{"case_id":"c"}}}`,
			`{"type":"item.completed","item":{"id":"i","type":"mcp_tool_call","tool":"slow","status":"failed","error":{"code":"new"}}}`},
		"grok-json": {`{"status":"success","events":[{"type":"tool_call","id":"g","name":"slow","arguments":{"case_id":"c"}},{"type":"tool_result","id":"g","error":{"kind":"new"}}]}`},
		"cursor-jsonl": {`{"type":"tool_call","subtype":"started","call_id":"x","tool_call":{"mcpToolCall":{"args":{"toolName":"slow","args":{"case_id":"c"}}}}}`,
			`{"type":"tool_call","subtype":"completed","call_id":"x","tool_call":{"mcpToolCall":{"result":{"error":{"code":"new"}}}}}`,
			`{"type":"tool_call","subtype":"completed","call_id":"unknown","tool_call":{"mcpToolCall":{"result":{"error":{"code":"timeout"}}}}}`,
			`{"type":"tool_call","subtype":"started","call_id":"y","tool_call":{}}`},
	} {
		d := reg[name].decode(transcriptOf(lines))
		if !strings.HasPrefix(d.Inconclusive, ReasonUnrecognized) {
			t.Errorf("%s: %+v", name, d)
		}
		for _, e := range d.Events {
			if e.Kind == KindMCPTimeout {
				t.Errorf("%s: an uncorrelated or unrecognized error became a timeout", name)
			}
		}
	}
}

func TestDecoderResultCaseMismatch(t *testing.T) {
	lines := fixture("codex-jsonl", scSuccess, "case-1", "n", 0)
	lines[2] = strings.Replace(lines[2], `\"case_id\":\"case-1\"`, `\"case_id\":\"other\"`, 1)
	d := DefaultRegistry()["codex-jsonl"].decode(transcriptOf(lines))
	for _, e := range d.Events {
		if e.Kind == KindToolResult && e.SafeReason != "result_case_mismatch" {
			t.Fatalf("mismatched result case accepted: %+v", e)
		}
	}
}

func TestDecoderSelection(t *testing.T) {
	reg := DefaultRegistry()
	for name, v := range map[string]string{"claude-json": "2.1.282 (Claude Code)", "codex-jsonl": "codex-cli 0.156.1",
		"grok-json": "grok 1.0.41 (4220f3b224a6) [stable]", "cursor-jsonl": "2026.09.23-86fc751"} {
		d, dv, err := reg.Select(name, v)
		if err != nil || d == nil || dv.Qualified || dv.Fixture != name+"/synthetic" || !reg.hasFixture(name, dv.Fixture) {
			t.Fatalf("%s: %+v %v", name, dv, err)
		}
		// No fallback: a neighbouring version is refused, naming the
		// supported one.
		if _, _, err := reg.Select(name, v+".1"); err == nil || !strings.Contains(err.Error(), v) {
			t.Fatalf("%s fell back: %v", name, err)
		}
	}
	if _, _, err := reg.Select("nope", "1"); err == nil {
		t.Fatal("unknown decoder selected")
	}
	q := reg.WithVersion("claude-json", DecoderVersion{Version: "9", Fixture: "claude-json/actual-9", Qualified: true})
	if _, dv, err := q.Select("claude-json", "9"); err != nil || !dv.Qualified {
		t.Fatal(err)
	}
	if _, _, err := reg.Select("claude-json", "9"); err == nil {
		t.Fatal("WithVersion mutated the original registry")
	}
	if n := (Transcript{Lines: []Line{{Data: []byte("ab")}, {Data: nil}}}).Size(); n != 4 {
		t.Fatalf("size %d", n)
	}
}
