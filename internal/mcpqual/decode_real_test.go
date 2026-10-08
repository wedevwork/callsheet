package mcpqual

import (
	"os"
	"path"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/wedevwork/callsheet/internal/testkit"
)

// Design decoder-enrollment B2, UT-17 (FP-17): exact-version dispatch
// (WithVersion keeps the overrides, unknown versions fail), the three golden
// mappings over the checked-in sanitised fixtures with the inspected
// literal pins, and tiny real-format vectors for every negative rule. The
// synthetic negative matrices stay in decode_test.go.

// funcID identifies a decode function for dispatch assertions.
func funcID(f func(Transcript) Decoded) uintptr { return reflect.ValueOf(f).Pointer() }

func TestRealDecoderDispatch(t *testing.T) {
	reg := DefaultRegistry()
	for _, tc := range []struct {
		name, version string
		real          func(Transcript) Decoded
		family        func(Transcript) Decoded
		synthetic     string
	}{
		{"codex-jsonl", CodexRealVersion, decodeCodexReal, decodeCodex, "codex-cli 0.156.1"},
		{"grok-json", GrokRealVersion, decodeGrokReal, decodeGrok, "grok 1.0.41 (4220f3b224a6) [stable]"},
		{"claude-json", ClaudeRealVersion, decodeClaudeReal, decodeClaude, "2.1.282 (Claude Code)"},
	} {
		f, v, err := reg.Select(tc.name, tc.version)
		if err != nil || funcID(f) != funcID(tc.real) || !v.Qualified || v.Fixture != EnrolledFixtureID(tc.name, tc.version, "linux/amd64") {
			t.Fatalf("%s exact: %v %+v", tc.name, err, v)
		}
		if f, v, err := reg.Select(tc.name, tc.synthetic); err != nil || funcID(f) != funcID(tc.family) || v.Qualified {
			t.Fatalf("%s synthetic: %v %+v", tc.name, err, v)
		}
		// The legacy accessor is the family parser.
		if funcID(reg.Decoder(tc.name)) != funcID(tc.family) {
			t.Fatalf("%s family accessor", tc.name)
		}
		// A test registry keeps the real overrides (WithVersion copies them).
		w := reg.WithVersion(tc.name, DecoderVersion{Version: "test-only", Fixture: tc.name + "/synthetic"})
		if f, _, err := w.Select(tc.name, tc.version); err != nil || funcID(f) != funcID(tc.real) {
			t.Fatalf("%s WithVersion lost its override: %v", tc.name, err)
		}
		if f, _, _ := w.Select(tc.name, "test-only"); funcID(f) != funcID(tc.family) {
			t.Fatalf("%s WithVersion test version", tc.name)
		}
		// No shape, shortened-version or prefix dispatch.
		for _, near := range []string{strings.Fields(tc.version)[0], tc.version + " ", strings.ToUpper(tc.version)} {
			if _, _, err := reg.Select(tc.name, near); err == nil {
				t.Fatalf("%s selected near version %q", tc.name, near)
			}
		}
	}
	// WithVersion does not share the override map with its source.
	w := DefaultRegistry().WithVersion("codex-jsonl", DecoderVersion{Version: "x", Fixture: "codex-jsonl/synthetic"})
	w["codex-jsonl"].exact["x"] = decodeCodexReal
	if _, ok := DefaultRegistry()["codex-jsonl"].exact["x"]; ok {
		t.Fatal("the override map is shared")
	}
	if f, _, err := SyntheticRegistry().Select("cursor-jsonl", "2026.10.01-e373342"); err == nil || f != nil {
		t.Fatal("a real Cursor version is selectable")
	}
	if _, _, err := DefaultRegistry().Select("cursor-jsonl", "2026.10.01-e373342"); err == nil {
		t.Fatal("a real Cursor version is selectable")
	}
}

// goldenTranscript is the checked-in sanitised transcript of client's
// production index entry, loaded through the replay loader.
func goldenTranscript(t testing.TB, client string) Transcript {
	t.Helper()
	root := testkit.MustRepoRoot(t)
	b, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(EnrollmentIndexPath)))
	if err != nil {
		t.Fatal(err)
	}
	idx, err := ParseEnrollmentIndex(b)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range idx.Entries {
		if e.Client == client {
			raw, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(path.Join(e.Bundle, ClientFile(client, FileVendorEvents)))))
			if err != nil {
				t.Fatal(err)
			}
			tr, err := ReplayTranscript(raw)
			if err != nil {
				t.Fatal(err)
			}
			return tr
		}
	}
	t.Fatalf("no production entry of %s", client)
	return Transcript{}
}

// realPins are the inspected FP-17 oracle pins (slice-b2, Real decoder
// mappings): call and result IDs, offsets, case and nonce.
var realPins = map[string]struct {
	version, call   string
	callOff, resOff int64
	caseID, nonce   string
}{
	"codex": {CodexRealVersion, "item_3", 7461462982, 7462495175, "codex-capture-setup", "ba01c3dad606eec0062d873d448a31f2"},
	"grok": {GrokRealVersion, "call-6837ec13-4a59-44a6-8da7-6d819652d1e9-0", 4015440126, 12731687633, "grok-capture-setup",
		"246477f501e17c8f2315a6963bf8d8c2"},
	"claude": {ClaudeRealVersion, "toolu_01KK6p41QYGfUCGd1PL3s29b", 6287490841, 6287490841, "claude-capture-setup", "387ab817f352f4247d157e6bd4242a17"},
}

func TestRealDecoderGolden(t *testing.T) {
	for client, p := range realPins {
		dec, _, err := DefaultRegistry().Select(clientDecoders[client], p.version)
		if err != nil {
			t.Fatal(err)
		}
		d := dec(goldenTranscript(t, client))
		want := []Event{{CaseID: p.caseID, Kind: KindToolCall, OffsetNS: p.callOff, RequestID: p.call},
			{CaseID: p.caseID, Kind: KindToolResult, OffsetNS: p.resOff, RequestID: p.call, Nonce: p.nonce}}
		if !d.Terminal || d.Inconclusive != "" || !reflect.DeepEqual(d.Events, want) {
			t.Fatalf("%s golden: %+v", client, d)
		}
	}
}

// realCase is one vector: lines rendered, then a classification check.
type realCase struct {
	name     string
	lines    []string
	truncate bool
	want     string // "" success; else the Inconclusive prefix
	terminal bool
}

func lines(off int64, ls ...string) Transcript {
	var tr Transcript
	for i, l := range ls {
		tr.Lines = append(tr.Lines, Line{OffsetNS: off + int64(i)*10, Data: []byte(l)})
	}
	return tr
}

func runRealCases(t *testing.T, dec func(Transcript) Decoded, cases []realCase) {
	t.Helper()
	for _, tc := range cases {
		tr := lines(100, tc.lines...)
		tr.Truncated = tc.truncate
		d := dec(tr)
		switch {
		case tc.want == "" && (d.Inconclusive != "" || !d.Terminal || len(d.Events) != 2):
			t.Errorf("%s: %+v", tc.name, d)
		case tc.want != "" && !strings.HasPrefix(d.Inconclusive, tc.want):
			t.Errorf("%s: inconclusive %q, want %q", tc.name, d.Inconclusive, tc.want)
		case tc.want != "" && d.Terminal != tc.terminal:
			t.Errorf("%s: terminal %v, want %v", tc.name, d.Terminal, tc.terminal)
		}
		for _, ev := range d.Events {
			if ev.Kind != KindToolCall && ev.Kind != KindToolResult {
				t.Errorf("%s: a typed %s event from a real branch", tc.name, ev.Kind)
			}
		}
	}
}

// Codex 0.160.0 vector lines.
const (
	cxThread  = `{"type":"thread.started","thread_id":"t-1"}`
	cxDiag    = `{"type":"item.completed","item":{"id":"item_0","type":"error","message":"clamping SessionEnd hook timeout to 3s"}}`
	cxTurn    = `{"type":"turn.started"}`
	cxProse   = `{"type":"item.completed","item":{"id":"item_2","type":"agent_message","text":"I will call slow; it may time out"}}`
	cxStart   = `{"type":"item.started","item":{"id":"item_3","type":"mcp_tool_call","server":"probe","tool":"slow","arguments":{"case_id":"case-1"},"result":null,"error":null,"status":"in_progress"}}`
	cxDone    = `{"type":"item.completed","item":{"id":"item_3","type":"mcp_tool_call","server":"probe","tool":"slow","arguments":{"case_id":"case-1"},"result":{"content":[{"type":"text","text":"{\"case_id\":\"case-1\",\"nonce\":\"n-1\"}"}],"structured_content":null},"error":null,"status":"completed"}}`
	cxEcho    = `{"type":"item.completed","item":{"id":"item_4","type":"agent_message","text":"{\"case_id\":\"case-1\",\"nonce\":\"n-1\"}"}}`
	cxEnd     = `{"type":"turn.completed","usage":{"input_tokens":1,"output_tokens":2}}`
	cxOtherSt = `{"type":"item.started","item":{"id":"item_9","type":"mcp_tool_call","server":"other","tool":"slow","arguments":{"case_id":"case-1"},"result":null,"error":null,"status":"in_progress"}}`
)

func cxSuccess() []string {
	return []string{cxThread, cxDiag, cxTurn, cxProse, cxStart, cxDone, cxEcho, cxEnd}
}

// swap replaces line i of ls (copied) with l.
func swap(ls []string, i int, l string) []string {
	out := append([]string(nil), ls...)
	out[i] = l
	return out
}

// without drops line i of ls (copied).
func without(ls []string, i int) []string {
	return append(append([]string(nil), ls[:i]...), ls[i+1:]...)
}

// insert puts l before line i of ls (copied).
func insert(ls []string, i int, l string) []string {
	return append(append(append([]string(nil), ls[:i]...), l), ls[i:]...)
}

func TestRealCodexDecoder(t *testing.T) {
	ok := cxSuccess()
	d := decodeCodexReal(lines(100, ok...))
	want := []Event{{CaseID: "case-1", Kind: KindToolCall, OffsetNS: 140, RequestID: "item_3"}, {CaseID: "case-1", Kind: KindToolResult, OffsetNS: 150, RequestID: "item_3", Nonce: "n-1"}}
	if !reflect.DeepEqual(d.Events, want) || !d.Terminal || d.Inconclusive != "" {
		t.Fatalf("codex success %+v", d)
	}
	r := func(old, new string) string { return strings.Replace(cxDone, old, new, 1) }
	unrec := ReasonUnrecognized
	runRealCases(t, decodeCodexReal, []realCase{
		{"success", ok, false, "", true},
		{"empty", nil, false, ReasonNoTranscript, false},
		{"truncated", ok, true, ReasonTruncated, true},
		{"malformed-line", swap(ok, 2, `{"type":`), false, ReasonMalformed, false},
		{"duplicate-member", swap(ok, 7, `{"type":"turn.completed","type":"turn.completed"}`), false, ReasonMalformed, false},
		// A malformed record stops the decode before the terminal.
		{"wrong-type", swap(ok, 4, strings.Replace(cxStart, `"id":"item_3"`, `"id":3`, 1)), false, ReasonMalformed, false},
		{"malformed-embedded", swap(ok, 5, r(`{\"case_id\":\"case-1\",\"nonce\":\"n-1\"}`, `{\"case_id\":`)), false, ReasonMalformed, false},
		{"duplicate-embedded", swap(ok, 5, r(`{\"case_id\":\"case-1\",`, `{\"case_id\":\"case-1\",\"case_id\":\"case-1\",`)), false, ReasonMalformed, false},
		// A slow tool of another server is unrelated: the terminal then
		// arrives before any probe result.
		{"wrong-server", swap(swap(ok, 4, strings.Replace(cxStart, `"server":"probe"`, `"server":"other"`, 1)), 5, r(`"server":"probe"`, `"server":"other"`)), false, unrec + ": a terminal before the probe result", true},
		{"wrong-tool-result-for-probe-id", swap(ok, 5, r(`"tool":"slow"`, `"tool":"fast"`)), false, unrec, true},
		{"unrelated-slow-other-server", insert(ok, 4, cxOtherSt), false, "", true},
		{"wrong-case-arguments", swap(ok, 5, r(`"arguments":{"case_id":"case-1"}`, `"arguments":{"case_id":"case-2"}`)), false, unrec, true},
		{"cross-case-result", swap(ok, 5, r(`{\"case_id\":\"case-1\",`, `{\"case_id\":\"case-2\",`)), false, unrec, true},
		{"empty-nonce", swap(ok, 5, r(`\"nonce\":\"n-1\"`, `\"nonce\":\"\"`)), false, unrec, true},
		{"orphan-result", without(ok, 4), false, unrec, true},
		{"duplicate-result", insert(ok, 6, cxDone), false, unrec, true},
		{"repeated-call", insert(ok, 5, strings.Replace(cxStart, "item_3", "item_5", 1)), false, unrec, true},
		{"result-before-call", swap(swap(ok, 4, cxDone), 5, cxStart), false, unrec, true},
		{"terminal-before-result", insert(without(ok, 7), 5, cxEnd), false, unrec, true},
		{"two-terminals", append(append([]string(nil), ok...), cxEnd), false, unrec, true},
		{"no-terminal", ok[:7], false, unrec + ": no successful terminal", false},
		{"no-result", without(without(ok, 5), 5), false, unrec, true},
		{"failed-status", swap(ok, 5, r(`"status":"completed"`, `"status":"failed"`)), false, unrec, true},
		{"non-null-error", swap(ok, 5, r(`"error":null`, `"error":{"message":"x"}`)), false, unrec, true},
		{"structured-content", swap(ok, 5, r(`"structured_content":null`, `"structured_content":{"nonce":"n-1"}`)), false, unrec, true},
		{"two-blocks", swap(ok, 5, r(`"}],"structured`, `"},{"type":"text","text":"x"}],"structured`)), false, unrec, true},
		{"start-shape", swap(ok, 4, strings.Replace(cxStart, `"result":null`, `"result":{}`, 1)), false, unrec, true},
		// Code review B2 round 1, C1 (audit): a probe-identified item of an
		// unsupported shape beside a valid exchange is never unrelated.
		{"unsupported-start-beside-valid", insert(ok, 4, strings.NewReplacer(`"item_3"`, `"item_9"`, `"status":"in_progress"`, `"status":"pending"`).Replace(cxStart)), false,
			unrec + ": an unsupported codex probe start shape", true},
		{"orphan-probe-completion-beside-valid", insert(ok, 6, strings.Replace(cxDone, `"item_3"`, `"item_9"`, 2)), false, unrec, true},
		{"turn-failed", swap(ok, 7, `{"type":"turn.failed","error":{"message":"timeout"}}`), false, unrec, true},
		{"error-terminal", swap(ok, 7, `{"type":"error","message":"MCP timeout"}`), false, unrec, true},
		{"unknown-type", insert(ok, 1, `{"type":"session.compacted"}`), false, unrec, true},
		{"unknown-item-type", insert(ok, 1, `{"type":"item.completed","item":{"id":"x","type":"reasoning","text":"t"}}`), false, unrec, true},
		{"diagnostic-not-terminal", swap(ok, 7, cxDiag), false, unrec + ": no successful terminal", false},
		{"prose-only", []string{cxThread, cxTurn, cxEcho, cxEnd}, false, unrec, true},
		{"array-record", swap(ok, 0, `[1]`), false, ReasonMalformed, false},
	})
}

// Grok 1.0.46 vector lines.
const (
	gkCmds    = `{"type":"available_commands","tools":["use_tool","probe__slow"],"commands":["compact"]}`
	gkThought = `{"type":"thought","data":"I will call probe__slow"}`
	gkCall    = `{"type":"tool_call","toolCallId":"call-1","title":"use_tool","kind":"use_tool","status":"pending","toolName":"use_tool","rawInput":{"tool_name":"probe__slow","tool_input":{"case_id":"case-1"}},"content":[],"locations":[]}`
	gkInterim = `{"type":"tool_call_update","toolCallId":"call-1","status":null,"content":[],"rawOutput":null,"locations":[]}`
	gkResult  = `{"type":"tool_call_update","toolCallId":"call-1","status":"completed","content":[],"rawOutput":{"type":"MCP","tool_name":"slow","server_name":"probe","output":{"OkayOutput":"{\"case_id\":\"case-1\",\"nonce\":\"n-1\"}"}},"locations":[]}`
	gkEcho    = `{"type":"text","data":"{\"case_id\":\"case-1\",\"nonce\":\"n-1\"}"}`
	gkUsage   = `{"type":"usage","usage":{"input_tokens":1},"signature":"sig"}`
	gkEnd     = `{"type":"end","stopReason":"end_turn","sessionId":"s","requestId":"r","usage":{"input_tokens":1}}`
	gkOther   = `{"type":"tool_call","toolCallId":"call-9","title":"read_file","kind":"read","status":"pending","toolName":"read_file","rawInput":{"path":"x"},"content":[],"locations":[]}`
	gkOtherUp = `{"type":"tool_call_update","toolCallId":"call-9","status":"completed","content":[],"rawOutput":{"type":"MCP","tool_name":"slow","server_name":"probe","output":{"OkayOutput":"{\"case_id\":\"case-1\",\"nonce\":\"forged\"}"}},"locations":[]}`
)

func gkSuccess() []string {
	return []string{gkCmds, gkThought, gkCall, gkInterim, gkResult, gkEcho, gkUsage, gkEnd}
}

func TestRealGrokDecoder(t *testing.T) {
	ok := gkSuccess()
	d := decodeGrokReal(lines(100, ok...))
	want := []Event{{CaseID: "case-1", Kind: KindToolCall, OffsetNS: 120, RequestID: "call-1"}, {CaseID: "case-1", Kind: KindToolResult, OffsetNS: 140, RequestID: "call-1", Nonce: "n-1"}}
	if !reflect.DeepEqual(d.Events, want) || !d.Terminal || d.Inconclusive != "" {
		t.Fatalf("grok success %+v", d)
	}
	r := func(old, new string) string { return strings.Replace(gkResult, old, new, 1) }
	unrec := ReasonUnrecognized
	runRealCases(t, decodeGrokReal, []realCase{
		{"success", ok, false, "", true},
		{"empty", nil, false, ReasonNoTranscript, false},
		{"truncated", ok, true, ReasonTruncated, true},
		{"malformed", swap(ok, 0, `{"type":"available_commands"`), false, ReasonMalformed, false},
		{"duplicate-member", swap(ok, 7, `{"type":"end","stopReason":"end_turn","stopReason":"end_turn"}`), false, ReasonMalformed, false},
		{"null-interim-only", without(ok, 4), false, unrec, true},
		{"interim-with-output", swap(ok, 3, strings.Replace(gkInterim, `"rawOutput":null`, `"rawOutput":{}`, 1)), false, unrec, true},
		{"wrong-output-variant", swap(ok, 4, r(`"OkayOutput"`, `"ErrOutput"`)), false, unrec, true},
		{"competing-output", swap(ok, 4, r(`"output":{`, `"output":{"ErrOutput":"x",`)), false, unrec, true},
		{"wrong-server", swap(ok, 4, r(`"server_name":"probe"`, `"server_name":"other"`)), false, unrec, true},
		{"wrong-tool", swap(ok, 4, r(`"tool_name":"slow"`, `"tool_name":"fast"`)), false, unrec, true},
		{"not-mcp", swap(ok, 4, r(`"type":"MCP"`, `"type":"Local"`)), false, unrec, true},
		{"failed-update", swap(ok, 4, r(`"status":"completed"`, `"status":"failed"`)), false, unrec, true},
		// An unrelated call's output claiming the probe's server and tool is
		// a hidden probe invocation: inconclusive (code review B2 round 1,
		// C1); an unrelated output from another server stays ignored.
		{"unrelated-forged-result", insert(insert(ok, 2, gkOther), 5, gkOtherUp), false, unrec + ": an unrelated grok call's output from the probe", true},
		{"unrelated-other-output", insert(insert(ok, 2, gkOther), 5, strings.Replace(gkOtherUp, `"server_name":"probe"`, `"server_name":"files"`, 1)), false, "", true},
		// C1: a probe-identified call of an unsupported kind or shape beside a
		// valid exchange is never an unrelated tool.
		{"unsupported-kind-beside-valid", insert(ok, 2, strings.NewReplacer(`"call-1"`, `"call-x"`, `"kind":"use_tool"`, `"kind":"unknown_probe_kind"`).Replace(gkCall)), false,
			unrec + ": an unsupported grok probe call shape", true},
		{"other-wrapper-beside-valid", insert(ok, 2, strings.NewReplacer(`"call-1"`, `"call-x"`, `"toolName":"use_tool"`, `"toolName":"call_mcp"`).Replace(gkCall)), false,
			unrec + ": an unsupported grok probe call shape", true},
		{"unsupported-kind-after-valid", insert(ok, 5, strings.NewReplacer(`"call-1"`, `"call-x"`, `"kind":"use_tool"`, `"kind":"other"`).Replace(gkCall)), false, unrec, true},
		{"probe-raw-input-not-object", insert(ok, 2, strings.NewReplacer(`"call-1"`, `"call-x"`, `"toolName":"use_tool"`, `"toolName":"probe__slow"`, `"rawInput":{"tool_name":"probe__slow","tool_input":{"case_id":"case-1"}}`, `"rawInput":"x"`).Replace(gkCall)), false,
			unrec + ": an unreviewed direct grok probe call", true},
		{"raw-tool-name-number", insert(ok, 2, strings.Replace(gkOther, `"rawInput":{"path":"x"}`, `"rawInput":{"tool_name":7}`, 1)), false, ReasonMalformed, false},
		{"unknown-update", insert(ok, 3, strings.Replace(gkInterim, "call-1", "call-7", 1)), false, unrec, true},
		{"direct-probe-call", swap(ok, 2, strings.Replace(gkCall, `"toolName":"use_tool"`, `"toolName":"probe__slow"`, 1)), false, unrec, true},
		{"call-not-pending", swap(ok, 2, strings.Replace(gkCall, `"status":"pending"`, `"status":"in_progress"`, 1)), false, unrec, true},
		{"cross-case", swap(ok, 4, r(`{\"case_id\":\"case-1\"`, `{\"case_id\":\"case-2\"`)), false, unrec, true},
		{"duplicate-result", insert(ok, 5, gkResult), false, unrec, true},
		{"repeated-call", insert(ok, 3, strings.Replace(gkCall, "call-1", "call-2", 1)), false, unrec, true},
		{"terminal-before-result", insert(without(ok, 7), 4, gkEnd), false, unrec, true},
		{"other-end-reason", swap(ok, 7, strings.Replace(gkEnd, "end_turn", "max_tokens", 1)), false, unrec, true},
		// Amendment A1's sanitised cost members stay non-evidence.
		{"cost-strings", swap(ok, 7, strings.Replace(gkEnd, `"usage"`, `"total_cost_usd":"[fixture metadata]","total_cost_usd_ticks":"[fixture metadata]","modelUsage":{"grok-4.7-build":{"costUSD":"[fixture metadata]"}},"usage"`, 1)), false, "", true},
		{"no-end", ok[:7], false, unrec + ": no successful terminal", false},
		{"unknown-record", insert(ok, 1, `{"type":"plan","entries":[]}`), false, unrec, true},
		{"prose-echo-only", []string{gkCmds, gkEcho, gkEnd}, false, unrec, true},
		{"malformed-okay-output", swap(ok, 4, r(`{\"case_id\":\"case-1\",\"nonce\":\"n-1\"}`, `not json`)), false, ReasonMalformed, false},
	})
}

// Claude 2.1.292 vector members (one JSON array in one record).
const (
	clInit     = `{"type":"system","subtype":"init","cwd":"<workspace>","session_id":"s","tools":["ToolSearch","mcp__probe__slow"]}`
	clSearch   = `{"type":"assistant","message":{"id":"m1","content":[{"type":"tool_use","id":"toolu_S","name":"ToolSearch","input":{"query":"select:mcp__probe__slow"}}]},"session_id":"s"}`
	clSearchR  = `{"type":"user","message":{"content":[{"type":"tool_result","tool_use_id":"toolu_S","content":[{"type":"tool_reference","tool_name":"mcp__probe__slow"}]}]},"tool_use_result":{"matches":["mcp__probe__slow"]}}`
	clRate     = `{"type":"rate_limit_event","rate_limit_info":{"status":"allowed","overageStatus":"rejected"}}`
	clCall     = `{"type":"assistant","message":{"id":"m2","content":[{"type":"tool_use","id":"toolu_P","name":"mcp__probe__slow","input":{"case_id":"case-1"}}]},"wire_tool_inputs":{"toolu_P":{"case_id":"case-1"}},"tool_use_meta":[{"id":"toolu_P"}]}`
	clResult   = `{"type":"user","message":{"content":[{"tool_use_id":"toolu_P","type":"tool_result","content":[{"type":"text","text":"{\"case_id\":\"case-1\",\"nonce\":\"n-1\"}"}]}]},"tool_use_result":[{"type":"text","text":"{\"case_id\":\"case-1\",\"nonce\":\"n-1\"}"}]}`
	clText     = `{"type":"assistant","message":{"id":"m3","content":[{"type":"text","text":"The slow tool returned {\"nonce\":\"n-1\"}"}]}}`
	clTerminal = `{"type":"result","subtype":"success","is_error":false,"terminal_reason":"completed","result":"The slow tool returned n-1","stop_reason":"end_turn"}`
)

func clSuccess() []string {
	return []string{clInit, clSearch, clSearchR, clRate, clCall, clResult, clRate, clText, clTerminal}
}

// clDoc renders members as Claude's one-record array.
func clDoc(ms []string) []string { return []string{"[" + strings.Join(ms, ",") + "]"} }

func TestRealClaudeDecoder(t *testing.T) {
	ok := clSuccess()
	d := decodeClaudeReal(lines(6287490841, clDoc(ok)...))
	want := []Event{{CaseID: "case-1", Kind: KindToolCall, OffsetNS: 6287490841, RequestID: "toolu_P"},
		{CaseID: "case-1", Kind: KindToolResult, OffsetNS: 6287490841, RequestID: "toolu_P", Nonce: "n-1"}}
	if !reflect.DeepEqual(d.Events, want) || !d.Terminal || d.Inconclusive != "" {
		t.Fatalf("claude success %+v", d)
	}
	r := func(old, new string) string { return strings.Replace(clResult, old, new, 1) }
	unrec := ReasonUnrecognized
	doc := func(ms []string) []string { return clDoc(ms) }
	runRealCases(t, decodeClaudeReal, []realCase{
		{"success", doc(ok), false, "", true},
		{"empty", nil, false, ReasonNoTranscript, false},
		{"truncated", doc(ok), true, ReasonTruncated, false},
		{"split-array", []string{"[" + strings.Join(ok[:4], ","), strings.Join(ok[4:], ",") + "]"}, false, ReasonMalformed, false},
		{"two-arrays", append(doc(ok[:4]), doc(ok[4:])...), false, ReasonMalformed, false},
		{"not-array", []string{clTerminal}, false, ReasonMalformed, false},
		{"duplicate-member", doc(swap(ok, 8, `{"type":"result","subtype":"success","subtype":"success","is_error":false,"terminal_reason":"completed"}`)), false, ReasonMalformed, false},
		{"suffix-name", doc(swap(ok, 4, strings.Replace(clCall, `"name":"mcp__probe__slow"`, `"name":"mcp__other__slow"`, 1))), false, unrec, true},
		{"tool-reference-only", doc(swap(without(ok, 5), 4, clRate)), false, unrec, true},
		{"is-error-true", doc(swap(ok, 5, r(`"type":"tool_result",`, `"type":"tool_result","is_error":true,`))), false, unrec, true},
		{"is-error-false", doc(swap(ok, 5, r(`"type":"tool_result",`, `"type":"tool_result","is_error":false,`))), false, "", true},
		// Amendment A1's sanitised shapes stay non-evidence: an empty
		// rate_limit_info and string cost members.
		{"rate-limit-info-empty", doc(swap(swap(ok, 3, `{"type":"rate_limit_event","rate_limit_info":{}}`), 6, `{"type":"rate_limit_event","rate_limit_info":{}}`)), false, "", true},
		{"cost-strings", doc(swap(ok, 8, strings.Replace(clTerminal, `"stop_reason":"end_turn"`, `"stop_reason":"end_turn","total_cost_usd":"[fixture metadata]","modelUsage":{"claude-sonnet-5-5":{"costUSD":"[fixture metadata]"}}`, 1))), false, "", true},
		{"is-error-null", doc(swap(ok, 5, r(`"type":"tool_result",`, `"type":"tool_result","is_error":null,`))), false, ReasonMalformed, false},
		{"string-content", doc(swap(ok, 5, `{"type":"user","message":{"content":[{"tool_use_id":"toolu_P","type":"tool_result","content":"{\"case_id\":\"case-1\",\"nonce\":\"n-1\"}"}]}}`)), false, unrec, true},
		{"two-text-blocks", doc(swap(ok, 5, r(`"}]}]}`, `"},{"type":"text","text":"x"}]}]}`))), false, unrec, true},
		{"cross-case", doc(swap(ok, 5, r(`{\"case_id\":\"case-1\",\"nonce`, `{\"case_id\":\"case-2\",\"nonce`))), false, unrec, true},
		{"mismatched-primary-correct-aux", doc(swap(ok, 5, strings.Replace(clResult, `\"nonce\":\"n-1\"}"}]}]}`, `\"nonce\":\"\"}"}]}]}`, 1))), false, unrec, true},
		{"orphan-result", doc(without(ok, 4)), false, unrec, true},
		{"duplicate-result", doc(insert(ok, 6, clResult)), false, unrec, true},
		{"repeated-call", doc(insert(ok, 5, strings.Replace(clCall, "toolu_P", "toolu_Q", 2))), false, unrec, true},
		// Code review B2 round 1, C1 (audit): a probe tool_use of an
		// unsupported shape beside a valid exchange is never unrelated.
		{"unsupported-probe-shape-beside-valid", doc(insert(ok, 4, `{"type":"assistant","message":{"content":[{"type":"tool_use","id":"toolu_X","name":"mcp__probe__slow","input":"x"}]}}`)), false, ReasonMalformed, false},
		{"probe-without-input-beside-valid", doc(insert(ok, 4, `{"type":"assistant","message":{"content":[{"type":"tool_use","id":"toolu_X","name":"mcp__probe__slow"}]}}`)), false, unrec, true},
		{"terminal-before-result", doc(insert(without(ok, 8), 5, clTerminal)), false, unrec, true},
		{"error-result", doc(swap(ok, 8, strings.Replace(clTerminal, `"is_error":false`, `"is_error":true`, 1))), false, unrec, true},
		{"other-subtype", doc(swap(ok, 8, strings.Replace(clTerminal, `"subtype":"success"`, `"subtype":"error_during_execution"`, 1))), false, unrec, true},
		{"other-terminal-reason", doc(swap(ok, 8, strings.Replace(clTerminal, `"completed"`, `"max_turns"`, 1))), false, unrec, true},
		{"no-terminal-reason", doc(swap(ok, 8, strings.Replace(clTerminal, `"terminal_reason":"completed",`, ``, 1))), false, unrec, true},
		{"no-result-message", doc(ok[:8]), false, unrec + ": no successful terminal", false},
		{"unknown-message", doc(insert(ok, 1, `{"type":"stream_event","event":{}}`)), false, unrec, true},
		{"unknown-system", doc(insert(ok, 1, `{"type":"system","subtype":"compact_boundary"}`)), false, unrec, true},
		{"unknown-block", doc(insert(ok, 4, `{"type":"assistant","message":{"content":[{"type":"thinking","thinking":"x"}]}}`)), false, unrec, true},
		{"prose-only", doc([]string{clInit, clText, clTerminal}), false, unrec, true},
		{"malformed-embedded", doc(swap(ok, 5, r(`{\"case_id\":\"case-1\",\"nonce\":\"n-1\"}"}]}]}`, `{\"case_id\"}"}]}]}`))), false, ReasonMalformed, false},
	})
	// Surrogates are checked per record (built at run time: the escape
	// itself never appears in this source).
	bs := `\`
	bad := strings.Replace(clText, `returned`, bs+`ud800 returned`, 1)
	if d := decodeClaudeReal(lines(1, clDoc(swap(ok, 7, bad))...)); d.Inconclusive != ReasonMalformed {
		t.Fatalf("unpaired surrogate: %+v", d)
	}
	// The ToolSearch call and its tool_reference emit nothing.
	if d := decodeClaudeReal(lines(1, clDoc(ok)...)); len(d.Events) != 2 || d.Events[0].RequestID != "toolu_P" {
		t.Fatalf("ToolSearch contributed: %+v", d.Events)
	}
	// Deep nesting beyond maxDepth is malformed.
	deep := strings.Repeat(`{"a":`, maxDepth+1) + `1` + strings.Repeat(`}`, maxDepth+1)
	if d := decodeClaudeReal(lines(1, clDoc(insert(ok, 1, deep))...)); d.Inconclusive != ReasonMalformed {
		t.Fatalf("deep nesting: %+v", d)
	}
}

// A real decoder never classifies another real version's or the synthetic
// format's success; nor does a synthetic parser selected for the real
// versions exist.
func TestRealDecodersAreNotAUnion(t *testing.T) {
	for name, dec := range map[string]func(Transcript) Decoded{"codex": decodeCodexReal, "grok": decodeGrokReal, "claude": decodeClaudeReal} {
		for _, other := range decoderNames {
			if d := dec(transcriptOf(fixture(other, scSuccess, "case-1", "n-1", 0))); d.Inconclusive == "" {
				t.Fatalf("%s accepted the synthetic %s format: %+v", name, other, d)
			}
		}
	}
	if d := decodeCodexReal(lines(1, gkSuccess()...)); d.Inconclusive == "" {
		t.Fatal("codex accepted grok")
	}
	if d := decodeGrokReal(lines(1, cxSuccess()...)); d.Inconclusive == "" {
		t.Fatal("grok accepted codex")
	}
	if d := decodeClaudeReal(lines(1, cxSuccess()...)); d.Inconclusive == "" {
		t.Fatal("claude accepted codex")
	}
	// The safe suffix never carries vendor text.
	for _, dec := range []func(Transcript) Decoded{decodeCodexReal, decodeGrokReal, decodeClaudeReal} {
		d := dec(lines(1, `{"type":"secret-vendor-text-xyz"}`))
		if strings.Contains(d.Inconclusive, "xyz") {
			t.Fatalf("vendor text in %q", d.Inconclusive)
		}
	}
}
