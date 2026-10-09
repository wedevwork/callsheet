package mcpqual

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

// Design decoder-enrollment B3, UT-22 (FP-22): Cursor Agent
// 2026.10.01-e373342's real stream-json format. Vectors are built from Go
// values with json.Marshal, so the opaque call ID carries one actual LF
// byte (never a two-character backslash sequence written by hand).

// cuID is the opaque, LF-bearing call ID of the vectors.
var cuID = "call-1-0" + string(rune(10)) + "fc_1_0"

// cuJSON encodes v as one JSON record.
func cuJSON(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return string(b)
}

// cuArgs is the probe's mcpToolCall.args for case caseID and call ID id.
func cuArgs(caseID, id string) map[string]any {
	return map[string]any{"name": "probe-slow", "args": map[string]any{"case_id": caseID}, "toolCallId": id, "providerIdentifier": "probe",
		"toolName": "slow", "smartModeApprovalOnly": false, "skipApproval": false, "serverIdentifier": "probe"}
}

// cuCall is a tool_call record of subtype sub whose mcpToolCall is m.
func cuCall(sub, id string, m map[string]any, edit func(rec, tc map[string]any)) string {
	tc := map[string]any{"mcpToolCall": m, "hookAdditionalContexts": []any{}, "toolCallId": id, "startedAtMs": "1"}
	rec := map[string]any{"type": "tool_call", "subtype": sub, "call_id": id, "tool_call": tc, "model_call_id": "m", "session_id": "s", "timestamp_ms": 1}
	if edit != nil {
		edit(rec, tc)
	}
	return cuJSON(rec)
}

// cuSuccess is the success result alternative carrying text.
func cuSuccess(text string) map[string]any {
	return map[string]any{"success": map[string]any{"content": []any{map[string]any{"text": map[string]any{"text": text}}}, "isError": false, "systemReminders": []any{}}}
}

// cuPayload is the probe's structured result JSON.
func cuPayload(caseID, nonce string) string {
	return cuJSON(map[string]any{"case_id": caseID, "nonce": nonce})
}

var (
	cuInit     = `{"type":"system","subtype":"init","apiKeySource":"[REDACTED]","cwd":"<workspace>","session_id":"s","model":"m","permissionMode":"default"}`
	cuUser     = `{"type":"user","message":{"role":"user","content":[{"type":"text","text":"Call slow; it may time out"}]},"session_id":"s"}`
	cuThink    = `{"type":"thinking","subtype":"delta","text":"timeout","session_id":"s","timestamp_ms":1}`
	cuThinkEnd = `{"type":"thinking","subtype":"completed","session_id":"s","timestamp_ms":2}`
	cuEnd      = `{"type":"result","subtype":"success","duration_ms":1,"duration_api_ms":1,"is_error":false,"result":"done","session_id":"s","request_id":"r","usage":{}}`
)

func cuStart() string {
	return cuCall("started", cuID, map[string]any{"args": cuArgs("case-1", cuID)}, nil)
}

func cuDone() string {
	return cuCall("completed", cuID, map[string]any{"args": cuArgs("case-1", cuID), "result": cuSuccess(cuPayload("case-1", "n-1"))}, nil)
}

// cuProse is an assistant record echoing the nonce in prose.
func cuProse() string {
	return cuJSON(map[string]any{"type": "assistant", "message": map[string]any{"role": "assistant", "content": []any{map[string]any{"type": "text", "text": cuPayload("case-1", "n-1")}}}, "session_id": "s"})
}

func cuOK() []string {
	return []string{cuInit, cuUser, cuThink, cuThinkEnd, cuStart(), cuDone(), cuProse(), cuEnd}
}

// cuDoneWith is the completion with its mcpToolCall built by edit.
func cuDoneWith(edit func(m map[string]any)) string {
	m := map[string]any{"args": cuArgs("case-1", cuID), "result": cuSuccess(cuPayload("case-1", "n-1"))}
	edit(m)
	return cuCall("completed", cuID, m, nil)
}

func TestCursorRealDecoderGoldenVector(t *testing.T) {
	d := decodeCursorReal(lines(100, cuOK()...))
	want := []Event{{CaseID: "case-1", Kind: KindToolCall, OffsetNS: 140, RequestID: cuID}, {CaseID: "case-1", Kind: KindToolResult, OffsetNS: 150, RequestID: cuID, Nonce: "n-1"}}
	if !d.Terminal || d.Inconclusive != "" || !reflect.DeepEqual(d.Events, want) {
		t.Fatalf("golden vector: %+v", d)
	}
	// The ID is opaque: its LF is kept, never trimmed, split or normalized.
	if !strings.Contains(d.Events[0].RequestID, string(rune(10))) || d.Events[0].RequestID != d.Events[1].RequestID {
		t.Fatalf("call ID %q", d.Events[0].RequestID)
	}
}

func TestCursorRealDecoderNegative(t *testing.T) {
	ok := cuOK()
	const iStart, iDone, iEnd = 4, 5, 7
	args := func(edit func(a map[string]any)) string {
		a := cuArgs("case-1", cuID)
		edit(a)
		return cuCall("started", cuID, map[string]any{"args": a}, nil)
	}
	other := "call-2"
	cases := []realCase{
		{name: "golden", lines: ok},
		// Precedence: truncation, then empty input, then malformed.
		{name: "truncated", lines: ok, truncate: true, want: ReasonTruncated, terminal: true},
		{name: "empty", lines: nil, want: ReasonNoTranscript},
		{name: "not-json", lines: swap(ok, 2, `{"type":`), want: ReasonMalformed},
		{name: "trailing", lines: swap(ok, 2, cuThink+` {}`), want: ReasonMalformed},
		{name: "duplicate-top", lines: swap(ok, 2, `{"type":"thinking","type":"thinking","subtype":"delta"}`), want: ReasonMalformed},
		{name: "duplicate-nested", lines: swap(ok, iStart, strings.Replace(cuStart(), `"toolName":"slow"`, `"toolName":"slow","toolName":"slow"`, 1)), want: ReasonMalformed},
		{name: "bad-utf8", lines: swap(ok, 2, "{\"type\":\"thinking\",\"subtype\":\"delta\",\"text\":\"\xff\"}"), want: ReasonMalformed},
		{name: "lone-surrogate", lines: swap(ok, 2, `{"type":"thinking","subtype":"delta","text":"\`+`ud800"}`), want: ReasonMalformed},
		{name: "type-number", lines: swap(ok, 2, `{"type":1}`), want: ReasonMalformed},
		{name: "type-missing", lines: swap(ok, 2, `{"subtype":"delta"}`), want: ReasonUnrecognized, terminal: true},
		{name: "unknown-type", lines: swap(ok, 2, `{"type":"interaction_query"}`), want: ReasonUnrecognized, terminal: true},
		{name: "unknown-thinking", lines: swap(ok, 2, `{"type":"thinking","subtype":"other"}`), want: ReasonUnrecognized, terminal: true},
		{name: "unknown-system", lines: swap(ok, 0, `{"type":"system","subtype":"other"}`), want: ReasonUnrecognized, terminal: true},
		{name: "unknown-tool-subtype", lines: swap(ok, iStart, strings.Replace(cuStart(), `"subtype":"started"`, `"subtype":"updated"`, 1)), want: ReasonUnrecognized, terminal: true},
		// Identity and flags.
		{name: "wrong-provider", lines: swap(ok, iStart, args(func(a map[string]any) { a["providerIdentifier"] = "other" })), want: ReasonUnrecognized, terminal: true},
		{name: "wrong-server", lines: swap(ok, iStart, args(func(a map[string]any) { a["serverIdentifier"] = "other" })), want: ReasonUnrecognized, terminal: true},
		{name: "wrong-tool", lines: swap(ok, iStart, args(func(a map[string]any) { a["toolName"] = "fast" })), want: ReasonUnrecognized, terminal: true},
		{name: "wrong-alias", lines: swap(ok, iStart, args(func(a map[string]any) { a["name"] = "probe_slow" })), want: ReasonUnrecognized, terminal: true},
		{name: "skip-approval", lines: swap(ok, iStart, args(func(a map[string]any) { a["skipApproval"] = true })), want: ReasonUnrecognized, terminal: true},
		{name: "smart-mode", lines: swap(ok, iStart, args(func(a map[string]any) { a["smartModeApprovalOnly"] = true })), want: ReasonUnrecognized, terminal: true},
		{name: "flag-missing", lines: swap(ok, iStart, args(func(a map[string]any) { delete(a, "skipApproval") })), want: ReasonUnrecognized, terminal: true},
		{name: "flag-string", lines: swap(ok, iStart, args(func(a map[string]any) { a["skipApproval"] = "false" })), want: ReasonMalformed},
		{name: "extra-arg", lines: swap(ok, iStart, args(func(a map[string]any) { a["args"] = map[string]any{"case_id": "case-1", "x": 1} })), want: ReasonUnrecognized, terminal: true},
		{name: "empty-case", lines: swap(ok, iStart, args(func(a map[string]any) { a["args"] = map[string]any{"case_id": ""} })), want: ReasonUnrecognized, terminal: true},
		{name: "case-number", lines: swap(ok, iStart, args(func(a map[string]any) { a["args"] = map[string]any{"case_id": 1} })), want: ReasonMalformed},
		{name: "args-toolcallid-differs", lines: swap(ok, iStart, args(func(a map[string]any) { a["toolCallId"] = other })), want: ReasonUnrecognized, terminal: true},
		{name: "call-id-trimmed", lines: swap(ok, iStart, cuCall("started", cuID, map[string]any{"args": cuArgs("case-1", cuID)},
			func(rec, _ map[string]any) {
				rec["call_id"] = strings.TrimSpace(strings.Split(cuID, string(rune(10)))[0])
			})), want: ReasonUnrecognized, terminal: true},
		{name: "tc-toolcallid-differs", lines: swap(ok, iStart, cuCall("started", cuID, map[string]any{"args": cuArgs("case-1", cuID)},
			func(_, tc map[string]any) { tc["toolCallId"] = other })), want: ReasonUnrecognized, terminal: true},
		{name: "empty-ids", lines: swap(swap(ok, iStart, cuCall("started", "", map[string]any{"args": cuArgs("case-1", "")}, nil)), iDone,
			cuCall("completed", "", map[string]any{"args": cuArgs("case-1", ""), "result": cuSuccess(cuPayload("case-1", "n-1"))}, nil)), want: ReasonUnrecognized, terminal: true},
		{name: "hooks", lines: swap(ok, iStart, cuCall("started", cuID, map[string]any{"args": cuArgs("case-1", cuID)},
			func(_, tc map[string]any) { tc["hookAdditionalContexts"] = []any{"x"} })), want: ReasonUnrecognized, terminal: true},
		{name: "two-variants", lines: swap(ok, iStart, cuCall("started", cuID, map[string]any{"args": cuArgs("case-1", cuID)},
			func(_, tc map[string]any) { tc["readToolCall"] = map[string]any{} })), want: ReasonUnrecognized, terminal: true},
		{name: "other-tool-only", lines: swap(ok, iStart, cuCall("started", cuID, map[string]any{"args": cuArgs("case-1", cuID)},
			func(_, tc map[string]any) { tc["readToolCall"] = tc["mcpToolCall"]; delete(tc, "mcpToolCall") })), want: ReasonUnrecognized, terminal: true},
		{name: "start-with-result", lines: swap(ok, iStart, cuCall("started", cuID, map[string]any{"args": cuArgs("case-1", cuID), "result": cuSuccess("x")}, nil)),
			want: ReasonUnrecognized, terminal: true},
		// Correlation and order.
		{name: "no-call", lines: []string{cuInit, cuEnd}, want: ReasonUnrecognized, terminal: true},
		{name: "orphan", lines: []string{cuInit, cuDone(), cuEnd}, want: ReasonUnrecognized, terminal: true},
		{name: "reordered", lines: []string{cuInit, cuDone(), cuStart(), cuEnd}, want: ReasonUnrecognized, terminal: true},
		{name: "repeated-start", lines: []string{cuInit, cuStart(), cuStart(), cuDone(), cuEnd}, want: ReasonUnrecognized, terminal: true},
		{name: "duplicate-result", lines: []string{cuInit, cuStart(), cuDone(), cuDone(), cuEnd}, want: ReasonUnrecognized, terminal: true},
		{name: "no-result", lines: []string{cuInit, cuStart(), cuEnd}, want: ReasonUnrecognized, terminal: true},
		{name: "terminal-first", lines: []string{cuInit, cuStart(), cuEnd, cuDone(), cuEnd}, want: ReasonUnrecognized, terminal: true},
		{name: "call-after-terminal", lines: append(append([]string(nil), ok...), cuStart()), want: ReasonUnrecognized, terminal: true},
		{name: "second-terminal", lines: append(append([]string(nil), ok...), cuEnd), want: ReasonUnrecognized, terminal: true},
		{name: "no-terminal", lines: ok[:iEnd], want: ReasonUnrecognized},
		{name: "completion-other-case", lines: swap(ok, iDone, cuCall("completed", cuID, map[string]any{"args": cuArgs("case-2", cuID), "result": cuSuccess(cuPayload("case-2", "n-1"))}, nil)),
			want: ReasonUnrecognized, terminal: true},
		{name: "completion-other-id", lines: swap(ok, iDone, cuCall("completed", other, map[string]any{"args": cuArgs("case-1", other), "result": cuSuccess(cuPayload("case-1", "n-1"))}, nil)),
			want: ReasonUnrecognized, terminal: true},
		// The result structure.
		{name: "payload-other-case", lines: swap(ok, iDone, cuDoneWith(func(m map[string]any) { m["result"] = cuSuccess(cuPayload("case-2", "n-1")) })), want: ReasonUnrecognized, terminal: true},
		{name: "payload-no-nonce", lines: swap(ok, iDone, cuDoneWith(func(m map[string]any) { m["result"] = cuSuccess(`{"case_id":"case-1"}`) })), want: ReasonUnrecognized, terminal: true},
		{name: "payload-empty-nonce", lines: swap(ok, iDone, cuDoneWith(func(m map[string]any) { m["result"] = cuSuccess(cuPayload("case-1", "")) })), want: ReasonUnrecognized, terminal: true},
		{name: "payload-extra", lines: swap(ok, iDone, cuDoneWith(func(m map[string]any) { m["result"] = cuSuccess(`{"case_id":"case-1","nonce":"n-1","x":1}`) })), want: ReasonUnrecognized, terminal: true},
		{name: "payload-duplicate", lines: swap(ok, iDone, cuDoneWith(func(m map[string]any) { m["result"] = cuSuccess(`{"case_id":"case-1","nonce":"n-1","nonce":"n-2"}`) })), want: ReasonMalformed},
		{name: "payload-not-json", lines: swap(ok, iDone, cuDoneWith(func(m map[string]any) { m["result"] = cuSuccess(`nonce n-1`) })), want: ReasonMalformed},
		{name: "payload-nonce-number", lines: swap(ok, iDone, cuDoneWith(func(m map[string]any) { m["result"] = cuSuccess(`{"case_id":"case-1","nonce":1}`) })), want: ReasonMalformed},
		{name: "direct-text", lines: swap(ok, iDone, cuDoneWith(func(m map[string]any) {
			m["result"] = map[string]any{"success": map[string]any{"content": []any{map[string]any{"text": cuPayload("case-1", "n-1")}}, "isError": false, "systemReminders": []any{}}}
		})), want: ReasonMalformed},
		{name: "typed-block", lines: swap(ok, iDone, cuDoneWith(func(m map[string]any) {
			m["result"] = map[string]any{"success": map[string]any{"content": []any{map[string]any{"type": "text", "text": map[string]any{"text": cuPayload("case-1", "n-1")}}}, "isError": false, "systemReminders": []any{}}}
		})), want: ReasonUnrecognized, terminal: true},
		{name: "two-blocks", lines: swap(ok, iDone, cuDoneWith(func(m map[string]any) {
			b := map[string]any{"text": map[string]any{"text": cuPayload("case-1", "n-1")}}
			m["result"] = map[string]any{"success": map[string]any{"content": []any{b, b}, "isError": false, "systemReminders": []any{}}}
		})), want: ReasonUnrecognized, terminal: true},
		{name: "is-error", lines: swap(ok, iDone, cuDoneWith(func(m map[string]any) {
			m["result"].(map[string]any)["success"].(map[string]any)["isError"] = true
		})), want: ReasonUnrecognized, terminal: true},
		{name: "reminders", lines: swap(ok, iDone, cuDoneWith(func(m map[string]any) {
			m["result"].(map[string]any)["success"].(map[string]any)["systemReminders"] = []any{"x"}
		})), want: ReasonUnrecognized, terminal: true},
		{name: "success-extra", lines: swap(ok, iDone, cuDoneWith(func(m map[string]any) {
			m["result"].(map[string]any)["success"].(map[string]any)["structured"] = cuPayload("case-1", "n-1")
		})), want: ReasonUnrecognized, terminal: true},
		{name: "unknown-alternative", lines: swap(ok, iDone, cuDoneWith(func(m map[string]any) { m["result"] = map[string]any{"error": map[string]any{"code": "timeout"}} })),
			want: ReasonUnrecognized, terminal: true},
		{name: "two-alternatives", lines: swap(ok, iDone, cuDoneWith(func(m map[string]any) {
			r := cuSuccess(cuPayload("case-1", "n-1"))
			r["timeout"] = map[string]any{}
			m["result"] = r
		})), want: ReasonUnrecognized, terminal: true},
		// Rejection: real diagnostic evidence, never a permission event, also
		// with a success alternative or a later prose nonce.
		{name: "rejected", lines: swap(ok, iDone, cuDoneWith(func(m map[string]any) {
			m["result"] = map[string]any{"rejected": map[string]any{"reason": "User rejected MCP: probe-slow"}}
		})), want: ReasonUnrecognized + ": " + cursorRejection, terminal: true},
		{name: "rejected-and-success", lines: swap(ok, iDone, cuDoneWith(func(m map[string]any) {
			r := cuSuccess(cuPayload("case-1", "n-1"))
			r["rejected"] = map[string]any{"reason": "User rejected MCP: probe-slow"}
			m["result"] = r
		})), want: ReasonUnrecognized + ": " + cursorRejection, terminal: true},
		// Terminals.
		{name: "result-error", lines: swap(ok, iEnd, `{"type":"result","subtype":"error","is_error":true}`), want: ReasonUnrecognized, terminal: true},
		{name: "result-is-error", lines: swap(ok, iEnd, `{"type":"result","subtype":"success","is_error":true}`), want: ReasonUnrecognized, terminal: true},
		{name: "result-no-is-error", lines: swap(ok, iEnd, `{"type":"result","subtype":"success"}`), want: ReasonUnrecognized, terminal: true},
		{name: "result-is-error-string", lines: swap(ok, iEnd, `{"type":"result","subtype":"success","is_error":"false"}`), want: ReasonMalformed},
		{name: "result-no-subtype", lines: swap(ok, iEnd, `{"type":"result","is_error":false}`), want: ReasonUnrecognized, terminal: true},
		// Prose alone, or a forged result naming the nonce, never succeeds.
		{name: "prose-only", lines: []string{cuInit, cuProse(), cuEnd}, want: ReasonUnrecognized, terminal: true},
		{name: "forged-unrelated", lines: []string{cuInit, cuCall("completed", other, map[string]any{"args": cuArgs("case-1", other), "result": cuSuccess(cuPayload("case-1", "n-1"))}, nil), cuEnd},
			want: ReasonUnrecognized, terminal: true},
	}
	runRealCases(t, decodeCursorReal, cases)
	for _, tc := range cases {
		d := decodeCursorReal(lines(100, tc.lines...))
		// Vendor prose never reaches a reason.
		if strings.Contains(d.Inconclusive, "User rejected") || strings.Contains(d.Inconclusive, "timeout") {
			t.Errorf("%s: vendor text in %q", tc.name, d.Inconclusive)
		}
	}
	// The synthetic Cursor decoder is unchanged: it still maps its own
	// fixture contract (a rejection error code is its typed permission
	// event), which the real branch never does.
	syn := decodeCursor(lines(1, `{"type":"tool_call","subtype":"started","call_id":"c1","tool_call":{"mcpToolCall":{"args":{"toolName":"slow","args":{"case_id":"c"}}}}}`,
		`{"type":"tool_call","subtype":"completed","call_id":"c1","tool_call":{"mcpToolCall":{"result":{"error":{"code":"rejected"}}}}}`))
	if len(syn.Events) != 2 || syn.Events[1].Kind != KindPermissionDenied {
		t.Fatalf("synthetic cursor decoder changed: %+v", syn)
	}
}
