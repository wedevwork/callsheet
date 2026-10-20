package contract

import (
	"errors"
	"testing"
)

// TestDecodeToolArgs: the MCP server's tool-argument rule (moved here from
// internal/mcp so a recorded call is judged by the same rule): empty is
// {}, a null anywhere outside strings is refused, then the strict decode.
func TestDecodeToolArgs(t *testing.T) {
	if containsNull([]byte(`{"a":"null","b":"\"null"}`)) || !containsNull([]byte(`{"a":[1,null]}`)) {
		t.Fatal("containsNull")
	}
	var a struct {
		ID   string  `json:"id"`
		Wait *string `json:"wait,omitempty"`
	}
	if err := DecodeToolArgs([]byte(`{"id":"x"}`), &a); err != nil || a.ID != "x" || a.Wait != nil {
		t.Fatalf("plain arguments: %v %+v", err, a)
	}
	var e *Error
	if err := DecodeToolArgs([]byte(`{"id":"x","wait":null}`), &a); !errors.As(err, &e) || e.Code != CodeInvalidArgument {
		t.Fatalf("a null argument was accepted: %v", err)
	}
	if err := DecodeToolArgs([]byte(`{"id":"x","extra":1}`), &a); err == nil {
		t.Fatal("an unknown argument was accepted")
	}
	var empty struct {
		Limit *int `json:"limit,omitempty"`
	}
	if err := DecodeToolArgs(nil, &empty); err != nil || empty.Limit != nil {
		t.Fatalf("empty arguments: %v", err)
	}
}
