package mcpqual

import (
	"strings"
	"testing"
	"time"
)

// The strict scanner behind every plan, case file, report and probe frame:
// what it accepts and why it rejects.
func TestCheckJSONStrict(t *testing.T) {
	const esc = "\\" // a JSON backslash escape, kept out of source escapes
	for _, ok := range []string{
		`{}`, `[]`, `{"a":[1,-2.5e3,0,true,false,null,"s"]}`, ` {"a" : { "b" : [ ] } } `, `"😀 é \/ \b\f\n\r\t"`,
		`0.5`, `-0`, `1E+2`, `[` + strings.Repeat(`[`, maxDepth-2) + strings.Repeat(`]`, maxDepth-2) + `]`,
		`{"x":"a","` + esc + `u0079":"b"}`, `"` + esc + `ud83d` + esc + `ude00` + esc + `u00e9"`,
	} {
		if err := checkJSON([]byte(ok)); err != nil {
			t.Errorf("%s rejected: %v", ok, err)
		}
	}
	for _, c := range []struct{ in, want string }{
		{"", "malformed"},
		{"\xff", "UTF-8"},
		{`{"a":1,"a":2}`, "duplicate object key"},
		{`{"a":1,"` + esc + `u0061":2}`, "duplicate object key"},
		{`[` + strings.Repeat(`[`, maxDepth) + strings.Repeat(`]`, maxDepth) + `]`, "nesting deeper"},
		{`{` + strings.Repeat(`"a":{`, maxDepth) + `}`, "nesting deeper"},
		{`{"a":1} x`, "trailing data"},
		{`{"a" 1}`, "malformed"},
		{`{"a":1,}`, "malformed"},
		{`{"a":1;`, "malformed"},
		{`{1:2}`, "malformed"},
		{`{"a":1`, "malformed"},
		{`{"a":`, "malformed"},
		{`[1 2]`, "malformed"},
		{`[1,`, "malformed"},
		{`tru`, "malformed"},
		{`nul`, "malformed"},
		{`01`, "trailing data"},
		{`-`, "malformed"},
		{`1.`, "malformed"},
		{`1e`, "malformed"},
		{`"abc`, "malformed"},
		{"\"a\x01b\"", "control character"},
		{`"\x"`, "malformed"},
		{`"\u12"`, "malformed"},
		{`"\u12zz"`, "malformed"},
		{`"\`, "malformed"},
		{`"\udc00"`, "unpaired surrogate"},
		{`"\ud800"`, "unpaired surrogate"},
		{`"` + esc + `ud800` + esc + `u0041"`, "unpaired surrogate"},
		{`"\ud800\uzzzz"`, "unpaired surrogate"},
		{`{"\x":1}`, "malformed"},
		{`@`, "malformed"},
	} {
		if err := checkJSON([]byte(c.in)); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%q: want %q, got %v", c.in, c.want, err)
		}
	}
	var v struct{ A int }
	if err := decodeStrict([]byte(`{"A":1,"B":2}`), &v); err == nil {
		t.Fatal("an unknown field decoded")
	}
	if err := decodeStrict([]byte(`{"A":"x"}`), &v); err == nil {
		t.Fatal("a mistyped field decoded")
	}
	if _, err := encodeJSON(make(chan int)); err == nil {
		t.Fatal("an unencodable value encoded")
	}
	if _, err := encodeIndent(func() {}); err == nil {
		t.Fatal("an unencodable value encoded")
	}
	if got := quoteJSON(`<a & "b">`); got != `"<a & \"b\">"` {
		t.Fatalf("quoteJSON escaped HTML: %s", got)
	}
}

// RealClock is the production seam: monotonic Now, timers that fire and
// stop, tickers that tick.
func TestRealClock(t *testing.T) {
	a := RealClock.Now()
	c, stop := RealClock.NewTimer(time.Millisecond)
	<-c
	if stop() {
		t.Fatal("stopping a fired timer reported an active one")
	}
	if !RealClock.Now().After(a) {
		t.Fatal("the clock did not advance")
	}
	_, stop = RealClock.NewTimer(time.Hour)
	if !stop() {
		t.Fatal("an active timer did not stop")
	}
	tc, stopTick := RealClock.NewTicker(time.Millisecond)
	<-tc
	<-tc
	stopTick()
}
