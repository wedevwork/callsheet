package mcp

import (
	"context"
	"errors"

	"github.com/wedevwork/callsheet/internal/contract"
)

// The shared error mapping (FP-2). Every contract code maps one to one to
// an MCP tool error (isError=true) whose single text item is the existing
// safe serialized contract.Error, {"error":{"code","message","details"?}}.
// Cause, raw HTTP bodies and arbitrary Go error strings are never
// serialized; anything that is not a contract error is a fixed internal
// error. Errors never end the session.

// JSON-RPC and MCP protocol error codes.
const (
	codeParse          = -32700
	codeInvalidRequest = -32600
	codeMethodNotFound = -32601
	codeInvalidParams  = -32602
)

// Detail reasons the server itself produces.
const (
	// ReasonCapacity marks a tools/call refused because two calls are
	// already active.
	ReasonCapacity = "mcp_capacity"
	// ReasonCallBudget marks a waiting call whose outer budget passed.
	ReasonCallBudget = "mcp_call_budget"
)

// fixedInternal is the only message of an unexpected (non-contract)
// failure.
const fixedInternal = "the MCP server failed unexpectedly; nothing more is reported (see the server's stderr)"

// knownCodes are the eight contract codes, each mapped to itself.
var knownCodes = map[contract.Code]bool{
	contract.CodeInvalidArgument: true, contract.CodeNotFound: true, contract.CodeConflict: true, contract.CodeUnavailable: true,
	contract.CodeTrustFailed: true, contract.CodeProtocolMismatch: true, contract.CodeNotImplemented: true, contract.CodeInternal: true,
}

// internalError is the fixed internal tool error.
func internalError() *contract.Error { return contract.New(contract.CodeInternal, fixedInternal) }

// toolError returns the safe contract error for err: a contract error's
// code, message and details (never its cause), a deadline as unavailable,
// and anything else as the fixed internal error.
func toolError(err error) *contract.Error {
	var ce *contract.Error
	if errors.As(err, &ce) {
		if !knownCodes[ce.Code] {
			return internalError()
		}
		return &contract.Error{Code: ce.Code, Message: ce.Message, Details: ce.Details}
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return contract.New(contract.CodeUnavailable, "the plane did not answer within the call's deadline; the plane may be unreachable")
	}
	return internalError()
}

// withGuidance maps err and appends a mutation's ambiguity guidance when
// no plane answer arrived (a transport failure the client classified
// locally, which carries its cause, or a deadline): the operation may
// have taken effect. A plane's own answer (a no_capacity rejection, say)
// is definitive and keeps its message.
func withGuidance(err error, guidance string) *contract.Error {
	e := toolError(err)
	var ce *contract.Error
	lost := !errors.As(err, &ce) || ce.Cause != nil
	if e.Code != contract.CodeUnavailable || !lost {
		return e
	}
	return &contract.Error{Code: e.Code, Message: e.Message + "; " + guidance, Details: e.Details}
}

// Ambiguity guidance of the mutations whose effect a lost answer hides.
const (
	dispatchGuidance = "admission may have happened: inspect task_ls before dispatching again (a dispatch is never retried)"
	removeGuidance   = "a forced removal may have started: read role_show for its removal operation token and retry role_rm with force and that operation; never repeat a bare force, the role ID may be reused"
)

// budgetError is the unavailable answer of a waiting call whose outer
// budget left no valid plane answer (never an invented still_running).
func budgetError(tool string, b Budget) *contract.Error {
	msg := tool + " did not complete within its " + b.String() + " call budget; the plane may be unreachable or slow: repeat task_wait (no answer is invented)"
	if tool == toolDispatch {
		msg = "dispatch did not complete within its " + b.String() + " call budget; " + dispatchGuidance
	}
	return &contract.Error{Code: contract.CodeUnavailable, Message: msg, Details: map[string]any{"reason": ReasonCallBudget}}
}

// capacityError refuses a third concurrent tools/call without contacting
// the plane.
func capacityError() *contract.Error {
	return &contract.Error{Code: contract.CodeUnavailable, Message: "two tool calls are already in progress in this MCP session; retry after one of them answers",
		Details: map[string]any{"reason": ReasonCapacity}}
}

// encodeToolError serializes e for a tool result's text item.
func encodeToolError(e *contract.Error) []byte {
	b, err := e.MarshalJSON()
	if err != nil {
		b, _ = internalError().MarshalJSON()
	}
	return b
}
