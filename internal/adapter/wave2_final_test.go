package adapter

import (
	"bytes"
	"encoding/json"
	"slices"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/wedevwork/callsheet/internal/contract"
)

// grokFailMessage is runs/grok-stdin-fail/stdout.bin's whole decoded
// message (the file is authoritative; NOTES' "unknown model id" is
// shorthand).
const grokFailMessage = `Couldn't set model 'does-not-exist': Invalid params: "unknown model id". Run 'grok models' to see available models.`

// grokCanaries are the five dontAsk write canaries (stopReason cancelled).
var grokCanaries = []string{"grok-shell-permitted", "grok-shell-home", "grok-shell-tmp", "grok-edit-permitted", "grok-edit-home"}

// grokDoc is a synthetic normal Grok object with text s.
func grokDoc(s string) []byte {
	b, _ := json.Marshal(map[string]any{"text": s, "stopReason": "end_turn", "sessionId": "s"})
	return b
}

// splitsBuffered feeds out in two chunks at every boundary and requires
// the extractor buffer to hold exactly out; it parses every split when
// parseAll, else the boundaries and a spread (the stress shard repeats the
// test).
func splitsBuffered(t *testing.T, newExt func() FinalExtractor, out []byte, want string, parseAll bool) {
	t.Helper()
	for at := 0; at <= len(out); at++ {
		e := newExt()
		e.Feed(out[:at])
		e.Feed(out[at:])
		var buf []byte
		switch x := e.(type) {
		case *grokExtractor:
			buf = x.buf
		case *claudeExtractor:
			buf = x.buf
		}
		if !bytes.Equal(buf, out) {
			t.Fatalf("split at %d buffered %d bytes", at, len(buf))
		}
		if !parseAll && at%61 != 0 && at > 2 && at < len(out)-2 {
			continue
		}
		if got := e.Finish(); finalOf(got) != want || got.Truncated {
			t.Fatalf("split at %d: %+v", at, got)
		}
	}
	e := newExt()
	for i := range out {
		e.Feed(out[i : i+1])
	}
	if got := e.Finish(); finalOf(got) != want {
		t.Fatalf("byte at a time: %+v", got)
	}
}

// TestGrokFinal is UT FP-4 (iteration 11): Grok's stdout JSON over every
// captured output, the normal and error shapes, strictness, bounds and
// sealing.
func TestGrokFinal(t *testing.T) {
	grok := NewGrok("")
	newExt := grok.NewFinalExtractor
	if _, ok := newExt().(*grokExtractor); !ok {
		t.Fatal("grok's extractor is not the grok extractor")
	}
	// The small captures at every chunk boundary, each split parsed.
	splitsBuffered(t, newExt, wave2Capture(t, "runs/grok-stdin-success/stdout.bin"), "pong", true)
	splitsBuffered(t, newExt, wave2Capture(t, "runs/grok-stdin-fail/stdout.bin"), grokFailMessage, true)
	if strings.Contains(grokFailMessage, "\n") || !strings.HasPrefix(string(wave2Capture(t, "runs/grok-stdin-fail/stderr.bin")), "Error: "+grokFailMessage) {
		t.Fatal("the captured failure message changed")
	}
	// Every other capture: the dontAsk canaries' and baselines' exact text
	// (cancelled is an ordinary answer, not a Callsheet cancellation and
	// not evidence that a write completed), and the stdin-missing run's
	// empty stdout as no answer.
	for _, run := range append(slices.Clone(grokCanaries), "grok-baseline-permitted", "grok-baseline-home") {
		out := wave2Capture(t, "runs/"+run+"/stdout.bin")
		var doc struct {
			Text       *string `json:"text"`
			StopReason string  `json:"stopReason"`
			Thought    string  `json:"thought"`
		}
		if err := json.Unmarshal(out, &doc); err != nil || doc.Text == nil {
			t.Fatalf("%s: %v", run, err)
		}
		wantStop := "end_turn"
		if slices.Contains(grokCanaries, run) {
			wantStop = "cancelled"
		}
		if doc.StopReason != wantStop {
			t.Fatalf("%s stopReason %q", run, doc.StopReason)
		}
		splitsBuffered(t, newExt, out, *doc.Text, false)
		if doc.Thought != "" && strings.Contains(finalOf(feedAll(newExt(), out, nil)), doc.Thought) {
			t.Fatalf("%s: the thought was extracted", run)
		}
	}
	if missing := wave2Capture(t, "runs/grok-stdin-missing/stdout.bin"); len(missing) != 0 {
		t.Fatalf("stdin-missing stdout %q", missing)
	} else if got := feedAll(newExt(), missing, nil); got.Message != nil || got.Error != FinalInvalid {
		t.Fatalf("empty stdout = %+v", got)
	}
	// Its stderr is never the answer either.
	if got := feedAll(newExt(), wave2Capture(t, "runs/grok-stdin-missing/stderr.bin"), nil); got.Message != nil || got.Error != FinalInvalid {
		t.Fatalf("stderr as stdout = %+v", got)
	}
	inv := "error:" + FinalInvalid
	for name, c := range map[string]struct{ out, want string }{
		"normal":                 {`{"text":"pong","stopReason":"end_turn"}`, "pong"},
		"empty text":             {`{"text":"","stopReason":"end_turn"}`, ""},
		"any stop reason":        {`{"text":"t","stopReason":"max_tokens"}`, "t"},
		"empty stop reason":      {`{"stopReason":"","text":"t"}`, "t"},
		"cancelled":              {`{"text":"I will write it.","stopReason":"cancelled"}`, "I will write it."},
		"metadata ignored":       {`{"thought":"secret plan","usage":{"total_tokens":1},"sessionId":"s","requestId":"r","modelUsage":{"grok-4.7-build":{}},"text":"answer","stopReason":"end_turn"}`, "answer"},
		"nested text ignored":    {`{"data":{"text":"nested","message":"m","type":"error"},"text":"top","stopReason":"end_turn"}`, "top"},
		"error":                  {`{"type":"error","message":"boom"}`, "boom"},
		"empty message":          {`{"type":"error","message":""}`, ""},
		"error metadata":         {`{"code":1,"type":"error","message":"boom"}`, "boom"},
		"no trimming":            {`{"text":"  x \n","stopReason":"end_turn"}`, "  x \n"},
		"escapes decoded once":   {`{"text":"a\\nb\né😀","stopReason":"s"}`, "a\\nb\né😀"},
		"whitespace around":      {"\n\t " + `{"text":"ok","stopReason":"s"}` + " \r\n", "ok"},
		"duplicate text":         {`{"text":"a","text":"b","stopReason":"s"}`, inv},
		"duplicate stopReason":   {`{"text":"a","stopReason":"s","stopReason":"s"}`, inv},
		"duplicate unknown":      {`{"x":1,"x":1,"text":"a","stopReason":"s"}`, inv},
		"duplicate message":      {`{"type":"error","message":"a","message":"b"}`, inv},
		"missing stopReason":     {`{"text":"a"}`, inv},
		"missing text":           {`{"stopReason":"end_turn"}`, inv},
		"null text":              {`{"text":null,"stopReason":"s"}`, inv},
		"number text":            {`{"text":1,"stopReason":"s"}`, inv},
		"object text":            {`{"text":{"v":"a"},"stopReason":"s"}`, inv},
		"null stopReason":        {`{"text":"a","stopReason":null}`, inv},
		"bool stopReason":        {`{"text":"a","stopReason":true}`, inv},
		"text with type":         {`{"text":"a","stopReason":"s","type":"result"}`, inv},
		"text with message":      {`{"text":"a","stopReason":"s","message":"m"}`, inv},
		"text with error type":   {`{"type":"error","message":"m","text":"a"}`, inv},
		"error with stopReason":  {`{"type":"error","message":"m","stopReason":"s"}`, inv},
		"type not error":         {`{"type":"result","message":"m"}`, inv},
		"type case":              {`{"type":"Error","message":"m"}`, inv},
		"null type":              {`{"type":null,"message":"m"}`, inv},
		"number type":            {`{"type":1,"message":"m"}`, inv},
		"message only":           {`{"message":"m"}`, inv},
		"type only":              {`{"type":"error"}`, inv},
		"null message":           {`{"type":"error","message":null}`, inv},
		"number message":         {`{"type":"error","message":2}`, inv},
		"claude shape":           {`{"type":"result","is_error":false,"result":"pong"}`, inv},
		"empty object":           {`{}`, inv},
		"array":                  {`[{"text":"a","stopReason":"s"}]`, inv},
		"string":                 {`"pong"`, inv},
		"plain text":             {"pong\n", inv},
		"two objects":            {`{"text":"a","stopReason":"s"}` + "\n" + `{"text":"b","stopReason":"s"}`, inv},
		"trailing empty object":  {`{"text":"a","stopReason":"s"}{}`, inv},
		"trailing garbage":       {`{"text":"a","stopReason":"s"} x`, inv},
		"truncated":              {`{"text":"a","stopReason":"s"`, inv},
		"invalid utf8 in string": {"{\"text\":\"a\xff\",\"stopReason\":\"s\"}", inv},
		"invalid utf8 outside":   {"{\"text\":\"a\",\"stopReason\":\"s\"}\xff", inv},
		"lone surrogate":         {`{"text":"\ud800","stopReason":"s"}`, inv},
		"lone low surrogate":     {`{"text":"\udc00","stopReason":"s"}`, inv},
		"surrogate in metadata":  {`{"thought":"\ud800x","text":"a","stopReason":"s"}`, inv},
		"surrogate in message":   {`{"type":"error","message":"\ud83d"}`, inv},
		"bad escape":             {`{"text":"\q","stopReason":"s"}`, inv},
		"empty stdout":           {``, inv},
		"whitespace only":        {" \n", inv},
		"stream json":            {`{"type":"system"}` + "\n" + `{"text":"a","stopReason":"s"}`, inv},
	} {
		if got := feedAll(newExt(), []byte(c.out), nil); finalOf(got) != c.want || (c.want == inv) != (got.Message == nil) {
			t.Errorf("%s: %+v, want %q", name, got, c.want)
		}
	}
	// The 64 KiB answer cap: exactly 64 KiB whole; longer cut at a UTF-8
	// boundary and marked truncated (text and message alike).
	long := strings.Repeat("a", contract.MaxFinalMessageBytes-1) + "é" + "tail"
	got := feedAll(newExt(), grokDoc(long), nil)
	if !got.Truncated || len(*got.Message) != contract.MaxFinalMessageBytes-1 || !utf8.ValidString(*got.Message) || got.Error != "" {
		t.Fatalf("truncated text: %d %v", len(msgOf(got)), got.Truncated)
	}
	errDoc, _ := json.Marshal(map[string]string{"type": "error", "message": long})
	if got := feedAll(newExt(), errDoc, nil); !got.Truncated || len(*got.Message) != contract.MaxFinalMessageBytes-1 {
		t.Fatalf("truncated message: %d %v", len(msgOf(got)), got.Truncated)
	}
	exact := strings.Repeat("b", contract.MaxFinalMessageBytes)
	if got := feedAll(newExt(), grokDoc(exact), nil); got.Truncated || len(*got.Message) != contract.MaxFinalMessageBytes {
		t.Fatal("a 64 KiB answer was truncated")
	}
	// The 8 MiB input bound: exactly MaxVendorFinalBytes is buffered whole
	// (BenchmarkVendorFinal parses a near-limit document); one byte more is
	// final_output_too_large, the buffer released and never regrown.
	doc := grokDoc("bounded")
	padded := append(slices.Clone(doc), bytes.Repeat([]byte(" "), MaxVendorFinalBytes-len(doc))...)
	e := newExt().(*grokExtractor)
	e.Feed(padded[:len(doc)/2])
	e.Feed(padded[len(doc)/2:])
	if e.over || len(e.buf) != MaxVendorFinalBytes {
		t.Fatalf("8 MiB input: over %v, %d bytes", e.over, len(e.buf))
	}
	e.Feed([]byte(" "))
	if !e.over || e.buf != nil {
		t.Fatalf("overflow retained %d bytes", cap(e.buf))
	}
	e.Feed(bytes.Repeat([]byte(" "), 4096))
	if e.buf != nil {
		t.Fatal("input after overflow retained")
	}
	if got := e.Finish(); got.Message != nil || got.Error != FinalTooLarge || got.Truncated {
		t.Fatalf("oversized: %+v", got)
	}
	e = newExt().(*grokExtractor)
	e.Feed(make([]byte, MaxVendorFinalBytes+1))
	if got := e.Finish(); got.Error != FinalTooLarge || e.buf != nil {
		t.Fatalf("one oversized chunk: %+v", got)
	}
	// Ownership and sealing: Feed keeps no caller slice; Finish is
	// idempotent with owned values and releases the buffer; a later Feed is
	// ignored.
	buf := grokDoc("own")
	e = newExt().(*grokExtractor)
	e.Feed(buf)
	copy(buf, bytes.Repeat([]byte("x"), len(buf)))
	first := e.Finish()
	e.Feed(grokDoc("late"))
	second := e.Finish()
	if msgOf(first) != "own" || msgOf(second) != "own" || e.buf != nil {
		t.Fatalf("finish %q then %q", msgOf(first), msgOf(second))
	}
	*second.Message = "mutated"
	if msgOf(e.Finish()) != "own" {
		t.Fatal("Finish returned shared state")
	}
	if got := newExt().Finish(); got.Error != FinalInvalid || got.Message != nil {
		t.Fatalf("fresh extractor state leaked: %+v", got)
	}
}

// TestCursorFinal is UT FP-5 (iteration 11): Cursor's captured json result
// format through the shared strict result extractor, offline; absent
// output stays null; the parser does not enable execution.
func TestCursorFinal(t *testing.T) {
	cursor := NewCursor("")
	newExt := cursor.NewFinalExtractor
	if _, ok := newExt().(*claudeExtractor); !ok {
		t.Fatal("cursor's extractor is not the shared result extractor")
	}
	splitsBuffered(t, newExt, wave2Capture(t, "runs/cursor-stdin-success/stdout.bin"), "pong", true)
	// The other result records extract their exact result strings; their
	// outside writes justify nothing.
	for _, run := range []string{"cursor-shell-permitted", "cursor-shell-home", "cursor-shell-tmp", "cursor-edit-permitted", "cursor-edit-home"} {
		out := wave2Capture(t, "runs/"+run+"/stdout.bin")
		var doc struct {
			Type    string  `json:"type"`
			IsError *bool   `json:"is_error"`
			Result  *string `json:"result"`
		}
		if err := json.Unmarshal(out, &doc); err != nil || doc.Type != "result" || doc.IsError == nil || *doc.IsError || doc.Result == nil {
			t.Fatalf("%s: %+v %v", run, doc, err)
		}
		splitsBuffered(t, newExt, out, *doc.Result, false)
	}
	// Every failed run has zero stdout: null plus invalid_final_output,
	// never its stderr as an answer.
	for _, run := range []string{"cursor-stdin-fail", "cursor-baseline-permitted", "cursor-baseline-home", "cursor-sandbox-permitted", "cursor-sandbox-home"} {
		if out := wave2Capture(t, "runs/"+run+"/stdout.bin"); len(out) != 0 {
			t.Fatalf("%s stdout %q", run, out)
		}
		if got := newExt().Finish(); got.Message != nil || got.Error != FinalInvalid {
			t.Fatalf("%s absent = %+v", run, got)
		}
		stderr := wave2Capture(t, "runs/"+run+"/stderr.bin")
		if got := feedAll(newExt(), stderr, nil); len(stderr) == 0 || got.Message != nil || got.Error != FinalInvalid {
			t.Fatalf("%s stderr as stdout = %+v", run, got)
		}
	}
	inv := "error:" + FinalInvalid
	for name, c := range map[string]struct{ out, want string }{
		"is_error true":     {`{"type":"result","subtype":"error","is_error":true,"result":"synthetic"}`, "synthetic"},
		"empty result":      {`{"type":"result","is_error":false,"result":""}`, ""},
		"unknown metadata":  {`{"usage":{"inputTokens":1},"subtype":"success","type":"result","is_error":false,"result":"r","session_id":"s"}`, "r"},
		"duplicate result":  {`{"type":"result","is_error":false,"result":"a","result":"b"}`, inv},
		"duplicate unknown": {`{"usage":1,"usage":2,"type":"result","is_error":false,"result":"r"}`, inv},
		"string is_error":   {`{"type":"result","is_error":"false","result":"r"}`, inv},
		"missing is_error":  {`{"type":"result","result":"r"}`, inv},
		"null result":       {`{"type":"result","is_error":false,"result":null}`, inv},
		"wrong type":        {`{"type":"assistant","is_error":false,"result":"r"}`, inv},
		"grok normal shape": {`{"text":"pong","stopReason":"end_turn"}`, inv},
		"grok error shape":  {`{"type":"error","message":"m"}`, inv},
		"plain text":        {"pong\n", inv},
		"stream events":     {`{"type":"system","subtype":"init"}` + "\n" + `{"type":"result","is_error":false,"result":"r"}`, inv},
		"two results":       {`{"type":"result","is_error":false,"result":"a"}{"type":"result","is_error":false,"result":"b"}`, inv},
		"invalid utf8":      {"{\"type\":\"result\",\"is_error\":false,\"result\":\"a\xff\"}", inv},
		"lone surrogate":    {`{"type":"result","is_error":false,"result":"\udc00"}`, inv},
		"empty":             {``, inv},
	} {
		if got := feedAll(newExt(), []byte(c.out), nil); finalOf(got) != c.want {
			t.Errorf("%s: %+v, want %q", name, got, c.want)
		}
	}
	// The same 64 KiB answer cap and 8 MiB input bound.
	long := strings.Repeat("a", contract.MaxFinalMessageBytes-1) + "é" + "tail"
	if got := feedAll(newExt(), claudeDoc(false, long), nil); !got.Truncated || len(*got.Message) != contract.MaxFinalMessageBytes-1 {
		t.Fatalf("truncated: %d %v", len(msgOf(got)), got.Truncated)
	}
	if got := feedAll(newExt(), claudeDoc(false, strings.Repeat("b", contract.MaxFinalMessageBytes)), nil); got.Truncated || len(*got.Message) != contract.MaxFinalMessageBytes {
		t.Fatal("a 64 KiB result was truncated")
	}
	doc := claudeDoc(false, "bounded")
	e := newExt().(*claudeExtractor)
	e.Feed(append(slices.Clone(doc), bytes.Repeat([]byte(" "), MaxVendorFinalBytes-len(doc))...))
	if e.over || len(e.buf) != MaxVendorFinalBytes {
		t.Fatalf("8 MiB input: over %v, %d bytes", e.over, len(e.buf))
	}
	e.Feed([]byte(" "))
	if got := e.Finish(); !e.over || e.buf != nil || got.Message != nil || got.Error != FinalTooLarge {
		t.Fatalf("oversized: %+v", got)
	}
	// Sealing: idempotent owned results, later Feed ignored.
	e = newExt().(*claudeExtractor)
	e.Feed(wave2Capture(t, "runs/cursor-stdin-success/stdout.bin"))
	first := e.Finish()
	e.Feed(claudeDoc(false, "late"))
	if second := e.Finish(); msgOf(first) != "pong" || msgOf(second) != "pong" || e.buf != nil {
		t.Fatalf("finish %q then %q", msgOf(first), msgOf(second))
	}
	// Parser availability does not enable execution.
	if _, err := cursor.Invocation(TaskInput{TaskID: taskID, Model: "grok-4.7", Effort: "low", Prompt: []byte("p")}); err == nil || err.Error() != cursorPostureText {
		t.Fatalf("cursor invocation after parsing: %v", err)
	}
}
