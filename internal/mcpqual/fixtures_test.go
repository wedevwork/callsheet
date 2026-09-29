package mcpqual

import (
	"fmt"
	"strings"
)

// Synthetic transcript fixtures for the four decoders. They test parser
// behaviour only; none is a vendor's actual output.

// Scenarios.
const (
	scSuccess    = "success"
	scTimeout    = "timeout"
	scAuth       = "auth"
	scPermission = "permission"
	scRefusal    = "refusal"
	scSession    = "session"
	scUnknownEnd = "unknown-terminal"
	scToolError  = "tool-error"
	scNoCall     = "no-call"
	scTwoCalls   = "two-calls"
	scNoModel    = "no-model"
)

var decoderNames = []string{"claude-json", "codex-jsonl", "grok-json", "cursor-jsonl"}

func resultText(caseID, nonce string) string {
	return fmt.Sprintf(`{\"case_id\":\"%s\",\"nonce\":\"%s\"}`, caseID, nonce)
}

// fixture renders one transcript; pad adds that many ignored events (for
// size scaling), and prose is a model narrative that must never classify.
func fixture(decoder, scenario, caseID, nonce string, pad int) []string {
	prose := "The tool call timed out after a long wait; MCP error -32001: timeout."
	switch decoder {
	case "claude-json":
		msgs := []string{`{"type":"system","subtype":"init","model":"m"}`}
		for i := 0; i < pad; i++ {
			msgs = append(msgs, fmt.Sprintf(`{"type":"system","subtype":"telemetry","n":%d}`, i))
		}
		call := func(id, c string) string {
			return fmt.Sprintf(`{"type":"assistant","message":{"content":[{"type":"text","text":%q},{"type":"tool_use","id":%q,"name":"mcp__probe__slow","input":{"case_id":%q}}]}}`, prose, id, c)
		}
		res := func(id, text string, isErr bool) string {
			return fmt.Sprintf(`{"type":"user","message":{"content":[{"type":"tool_result","tool_use_id":%q,"is_error":%v,"content":[{"type":"text","text":"%s"}]}]}}`, id, isErr, text)
		}
		end := func(sub string) string {
			return fmt.Sprintf(`{"type":"result","subtype":%q,"is_error":%v,"result":%q}`, sub, sub != "success", prose)
		}
		switch scenario {
		case scSuccess:
			msgs = append(msgs, call("t1", caseID), res("t1", resultText(caseID, nonce), false), end("success"))
		case scTimeout:
			msgs = append(msgs, call("t1", caseID), res("t1", "MCP error -32001: Request timed out", true), end("success"))
		case scToolError:
			msgs = append(msgs, call("t1", caseID), res("t1", "MCP error -32602: invalid params", true), end("success"))
		case scAuth:
			msgs = append(msgs, end("error_authentication"))
		case scPermission:
			msgs = append(msgs, call("t1", caseID), end("error_permission_denied"))
		case scRefusal:
			msgs = append(msgs, end("error_refusal"))
		case scSession:
			msgs = append(msgs, end("error_during_execution"))
		case scNoModel:
			msgs = append(msgs, end("error_model_unavailable"))
		case scUnknownEnd:
			msgs = append(msgs, call("t1", caseID), res("t1", resultText(caseID, nonce), false), end("error_brand_new"))
		case scNoCall:
			msgs = append(msgs, end("success"))
		case scTwoCalls:
			msgs = append(msgs, call("t1", caseID), res("t1", resultText(caseID, nonce), false), call("t2", caseID), res("t2", resultText(caseID, nonce), false), end("success"))
		}
		return []string{"[" + strings.Join(msgs, ",") + "]"}
	case "codex-jsonl":
		lines := []string{`{"type":"thread.started","thread_id":"th"}`}
		for i := 0; i < pad; i++ {
			lines = append(lines, fmt.Sprintf(`{"type":"item.completed","item":{"id":"m%d","type":"agent_message","text":%q}}`, i, prose))
		}
		start := func(id, c string) string {
			return fmt.Sprintf(`{"type":"item.started","item":{"id":%q,"type":"mcp_tool_call","server":"probe","tool":"slow","arguments":{"case_id":%q},"status":"in_progress"}}`, id, c)
		}
		done := func(id, status, body string) string {
			return fmt.Sprintf(`{"type":"item.completed","item":{"id":%q,"type":"mcp_tool_call","server":"probe","tool":"slow","status":%q%s}}`, id, status, body)
		}
		ok := func(id, c string) string {
			return done(id, "completed", fmt.Sprintf(`,"result":{"content":[{"type":"text","text":"%s"}]}`, resultText(c, nonce)))
		}
		switch scenario {
		case scSuccess:
			lines = append(lines, start("i1", caseID), ok("i1", caseID), `{"type":"turn.completed"}`)
		case scTimeout:
			lines = append(lines, start("i1", caseID), done("i1", "failed", `,"error":{"code":"timeout","message":"tool call timed out"}`), `{"type":"turn.completed"}`)
		case scToolError:
			lines = append(lines, start("i1", caseID), done("i1", "failed", `,"error":{"code":"tool_error"}`), `{"type":"turn.completed"}`)
		case scAuth:
			lines = append(lines, `{"type":"turn.failed","error":{"code":"unauthorized"}}`)
		case scPermission:
			lines = append(lines, start("i1", caseID), done("i1", "failed", `,"error":{"code":"approval_denied"}`), `{"type":"turn.completed"}`)
		case scRefusal:
			lines = append(lines, `{"type":"turn.failed","error":{"code":"refusal"}}`)
		case scSession:
			lines = append(lines, `{"type":"error","error":{"code":"stream_error"}}`)
		case scNoModel:
			lines = append(lines, `{"type":"turn.failed","error":{"code":"model_not_found"}}`)
		case scUnknownEnd:
			lines = append(lines, start("i1", caseID), ok("i1", caseID), `{"type":"turn.failed","error":{"code":"brand_new"}}`)
		case scNoCall:
			lines = append(lines, `{"type":"turn.completed"}`)
		case scTwoCalls:
			lines = append(lines, start("i1", caseID), ok("i1", caseID), start("i2", caseID), ok("i2", caseID), `{"type":"turn.completed"}`)
		}
		return lines
	case "grok-json":
		var evs []string
		for i := 0; i < pad; i++ {
			evs = append(evs, fmt.Sprintf(`{"type":"reasoning","text":%q}`, prose))
		}
		call := func(id, c string) string {
			return fmt.Sprintf(`{"type":"tool_call","id":%q,"name":"slow","arguments":{"case_id":%q}}`, id, c)
		}
		res := func(id, c string) string {
			return fmt.Sprintf(`{"type":"tool_result","id":%q,"content":"%s","error":null}`, id, resultText(c, nonce))
		}
		fail := func(id, kind string) string {
			return fmt.Sprintf(`{"type":"tool_result","id":%q,"content":"","error":{"kind":%q}}`, id, kind)
		}
		status, errKind := "success", ""
		switch scenario {
		case scSuccess:
			evs = append(evs, call("g1", caseID), res("g1", caseID))
		case scTimeout:
			evs = append(evs, call("g1", caseID), fail("g1", "mcp_timeout"))
		case scToolError:
			evs = append(evs, call("g1", caseID), fail("g1", "tool_error"))
		case scAuth:
			status, errKind = "error", "auth"
		case scPermission:
			evs = append(evs, call("g1", caseID), fail("g1", "permission_denied"))
		case scRefusal:
			status, errKind = "error", "refusal"
		case scSession:
			status, errKind = "error", "session"
		case scNoModel:
			status, errKind = "error", "model"
		case scUnknownEnd:
			evs = append(evs, call("g1", caseID), res("g1", caseID))
			status = "partial"
		case scNoCall:
		case scTwoCalls:
			evs = append(evs, call("g1", caseID), res("g1", caseID), call("g2", caseID), res("g2", caseID))
		}
		return []string{fmt.Sprintf(`{"status":%q,"error":{"kind":%q},"response":%q,"events":[%s]}`, status, errKind, prose, strings.Join(evs, ","))}
	case "cursor-jsonl":
		lines := []string{`{"type":"system","subtype":"init"}`}
		for i := 0; i < pad; i++ {
			lines = append(lines, fmt.Sprintf(`{"type":"assistant","message":{"content":[{"type":"text","text":%q}]}}`, prose))
		}
		start := func(id, c string) string {
			return fmt.Sprintf(`{"type":"tool_call","subtype":"started","call_id":%q,"tool_call":{"mcpToolCall":{"args":{"toolName":"slow","args":{"case_id":%q}}}}}`, id, c)
		}
		done := func(id, result string) string {
			return fmt.Sprintf(`{"type":"tool_call","subtype":"completed","call_id":%q,"tool_call":{"mcpToolCall":{"args":{"toolName":"slow"},"result":%s}}}`, id, result)
		}
		ok := func(id, c string) string {
			return done(id, fmt.Sprintf(`{"success":{"content":[{"type":"text","text":"%s"}]}}`, resultText(c, nonce)))
		}
		end := func(sub, kind string) string {
			return fmt.Sprintf(`{"type":"result","subtype":%q,"error_kind":%q,"is_error":%v,"result":%q}`, sub, kind, sub != "success", prose)
		}
		switch scenario {
		case scSuccess:
			lines = append(lines, start("c1", caseID), ok("c1", caseID), end("success", ""))
		case scTimeout:
			lines = append(lines, start("c1", caseID), done("c1", `{"error":{"code":"timeout"}}`), end("success", ""))
		case scToolError:
			lines = append(lines, start("c1", caseID), done("c1", `{"error":{"code":"tool_error"}}`), end("success", ""))
		case scAuth:
			lines = append(lines, end("error", "authentication"))
		case scPermission:
			lines = append(lines, start("c1", caseID), done("c1", `{"error":{"code":"rejected"}}`), end("success", ""))
		case scRefusal:
			lines = append(lines, end("error", "refusal"))
		case scSession:
			lines = append(lines, end("error", "session"))
		case scNoModel:
			lines = append(lines, end("error", "model"))
		case scUnknownEnd:
			lines = append(lines, start("c1", caseID), ok("c1", caseID), end("cancelled", ""))
		case scNoCall:
			lines = append(lines, end("success", ""))
		case scTwoCalls:
			lines = append(lines, start("c1", caseID), ok("c1", caseID), start("c2", caseID), ok("c2", caseID), end("success", ""))
		}
		return lines
	}
	return nil
}

func transcriptOf(lines []string) Transcript {
	var t Transcript
	for i, l := range lines {
		t.Lines = append(t.Lines, Line{OffsetNS: int64(i + 1), Data: []byte(l)})
	}
	return t
}
