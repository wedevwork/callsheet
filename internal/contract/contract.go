// Package contract holds the protocol version, the shared error model and the
// deterministic error-to-exit/HTTP mappings. It imports only the standard
// library so every other package may depend on it.
package contract

import (
	"bytes"
	"encoding/json"
	"errors"
	"log/slog"
)

// ProtocolVersion is the plane/sidecar/client wire protocol version.
const ProtocolVersion = 1

// Code is a stable, machine-readable error code.
type Code string

// Error codes. Their exit and HTTP mappings are fixed by ExitCode/HTTPStatus.
const (
	CodeInvalidArgument  Code = "invalid_argument"
	CodeNotFound         Code = "not_found"
	CodeConflict         Code = "conflict"
	CodeUnavailable      Code = "unavailable"
	CodeTrustFailed      Code = "trust_failed"
	CodeProtocolMismatch Code = "protocol_mismatch"
	CodeNotImplemented   Code = "not_implemented"
	CodeInternal         Code = "internal"
)

// Process exit codes that are not derived from an error code.
const (
	ExitOK          = 0
	ExitInterrupted = 130
)

// Error is the shared error type. Message and Details must be safe to show to
// a user or peer; Cause is for local diagnosis only and is never serialized.
type Error struct {
	Code    Code
	Message string
	Details map[string]any
	Cause   error
}

// New returns an Error with the given code and safe message.
func New(code Code, message string) *Error {
	return &Error{Code: code, Message: message}
}

// Wrap returns an Error with the given code and safe message wrapping cause.
func Wrap(code Code, message string, cause error) *Error {
	return &Error{Code: code, Message: message, Cause: cause}
}

// Error returns "<code>: <message>". The cause is deliberately omitted so
// that formatting an Error cannot leak unsafe payloads.
func (e *Error) Error() string {
	return string(e.Code) + ": " + e.Message
}

// Unwrap exposes the cause to errors.Is/errors.As.
func (e *Error) Unwrap() error { return e.Cause }

type wireBody struct {
	Code    Code           `json:"code"`
	Message string         `json:"message"`
	Details map[string]any `json:"details,omitempty"`
}

type wireError struct {
	Error wireBody `json:"error"`
}

// MarshalJSON renders the reserved wire shape
// {"error":{"code":"...","message":"...","details":{...}}} without the cause
// and without HTML escaping, so Unicode text is preserved verbatim.
func (e *Error) MarshalJSON() ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(wireError{Error: wireBody{Code: e.Code, Message: e.Message, Details: e.Details}}); err != nil {
		return nil, err
	}
	return bytes.TrimRight(buf.Bytes(), "\n"), nil
}

// LogValue makes slog render only the safe fields of an Error.
func (e *Error) LogValue() slog.Value {
	return slog.GroupValue(slog.String("code", string(e.Code)), slog.String("message", e.Message))
}

// ExitCode maps an error to a process exit code: nil is 0, contract errors
// (also when wrapped) use their code's mapping, anything else is 1.
func ExitCode(err error) int {
	if err == nil {
		return ExitOK
	}
	var ce *Error
	if !errors.As(err, &ce) {
		return 1
	}
	switch ce.Code {
	case CodeInvalidArgument:
		return 2
	case CodeNotFound:
		return 3
	case CodeConflict:
		return 4
	case CodeUnavailable:
		return 5
	case CodeTrustFailed:
		return 6
	case CodeProtocolMismatch:
		return 7
	case CodeNotImplemented:
		return 8
	default:
		return 1
	}
}

// HTTPStatus maps a code to an HTTP status. trust_failed is client-side only
// and returns 0 (no HTTP mapping); unknown codes map to 500.
func HTTPStatus(code Code) int {
	switch code {
	case CodeInvalidArgument:
		return 400
	case CodeNotFound:
		return 404
	case CodeConflict:
		return 409
	case CodeUnavailable:
		return 503
	case CodeTrustFailed:
		return 0
	case CodeProtocolMismatch:
		return 409
	case CodeNotImplemented:
		return 501
	default:
		return 500
	}
}

// CodeOf returns the code of the first contract error in err's chain, or
// CodeInternal for any other non-nil error, or "" for nil.
func CodeOf(err error) Code {
	if err == nil {
		return ""
	}
	var ce *Error
	if errors.As(err, &ce) {
		return ce.Code
	}
	return CodeInternal
}
