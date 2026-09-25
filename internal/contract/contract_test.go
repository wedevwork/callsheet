package contract

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"testing"
)

func TestMappings(t *testing.T) {
	cases := []struct {
		code Code
		exit int
		http int
	}{
		{CodeInvalidArgument, 2, 400},
		{CodeNotFound, 3, 404},
		{CodeConflict, 4, 409},
		{CodeUnavailable, 5, 503},
		{CodeTrustFailed, 6, 0},
		{CodeProtocolMismatch, 7, 409},
		{CodeNotImplemented, 8, 501},
		{CodeInternal, 1, 500},
		{Code("something_else"), 1, 500},
	}
	for _, c := range cases {
		if got := ExitCode(New(c.code, "m")); got != c.exit {
			t.Errorf("ExitCode(%s) = %d, want %d", c.code, got, c.exit)
		}
		if got := HTTPStatus(c.code); got != c.http {
			t.Errorf("HTTPStatus(%s) = %d, want %d", c.code, got, c.http)
		}
	}
}

func TestExitCodeNilUnknownWrapped(t *testing.T) {
	if ExitCode(nil) != 0 {
		t.Fatal("nil must map to 0")
	}
	if ExitCode(errors.New("boom")) != 1 {
		t.Fatal("unknown error must map to 1")
	}
	wrapped := fmt.Errorf("outer: %w", New(CodeNotFound, "missing"))
	if ExitCode(wrapped) != 3 {
		t.Fatalf("wrapped not_found = %d", ExitCode(wrapped))
	}
	if CodeOf(wrapped) != CodeNotFound || CodeOf(errors.New("x")) != CodeInternal || CodeOf(nil) != "" {
		t.Fatal("CodeOf mismatch")
	}
	if ExitInterrupted != 130 {
		t.Fatal("interrupted exit must be 130")
	}
}

func TestErrorUnwrapAndMessage(t *testing.T) {
	cause := errors.New("secret-cause-payload")
	e := Wrap(CodeUnavailable, "plane unreachable", cause)
	if !errors.Is(e, cause) {
		t.Fatal("Unwrap must expose cause")
	}
	if strings.Contains(e.Error(), "secret") {
		t.Fatalf("Error() leaks cause: %q", e.Error())
	}
	if e.Error() != "unavailable: plane unreachable" {
		t.Fatalf("Error() = %q", e.Error())
	}
}

func TestJSONShapeExcludesCauseAndKeepsUnicode(t *testing.T) {
	e := &Error{
		Code:    CodeProtocolMismatch,
		Message: "版本不匹配 <&>",
		Details: map[string]any{"local": 1, "remote": 2},
		Cause:   errors.New("secret-cause-payload"),
	}
	b, err := json.Marshal(e)
	if err != nil {
		t.Fatal(err)
	}
	s := string(b)
	if strings.Contains(s, "secret") || strings.Contains(s, "Cause") || strings.Contains(s, "cause") {
		t.Fatalf("cause serialized: %s", s)
	}
	if !strings.Contains(s, "版本不匹配") {
		t.Fatalf("unicode not preserved verbatim: %s", s)
	}
	direct, err := e.MarshalJSON()
	if err != nil || !strings.Contains(string(direct), "版本不匹配 <&>") {
		t.Fatalf("MarshalJSON must not HTML-escape: %s %v", direct, err)
	}
	var decoded map[string]map[string]any
	if err := json.Unmarshal(b, &decoded); err != nil {
		t.Fatal(err)
	}
	body := decoded["error"]
	if body["code"] != "protocol_mismatch" || body["message"] != "版本不匹配 <&>" {
		t.Fatalf("body = %v", body)
	}
	det := body["details"].(map[string]any)
	if det["local"].(float64) != 1 || det["remote"].(float64) != 2 {
		t.Fatalf("details = %v", det)
	}
	// Details are omitted when empty.
	b2, _ := json.Marshal(New(CodeNotFound, "x"))
	if string(b2) != `{"error":{"code":"not_found","message":"x"}}` {
		t.Fatalf("got %s", b2)
	}
}

func TestLogValueExcludesCause(t *testing.T) {
	var buf bytes.Buffer
	l := slog.New(slog.NewJSONHandler(&buf, nil))
	l.Error("failed", "err", Wrap(CodeInternal, "safe", errors.New("secret-cause-payload")))
	if strings.Contains(buf.String(), "secret") || !strings.Contains(buf.String(), `"message":"safe"`) {
		t.Fatalf("log = %s", buf.String())
	}
}

func TestProtocolVersion(t *testing.T) {
	if ProtocolVersion != 1 {
		t.Fatal("protocol version must be 1")
	}
}
