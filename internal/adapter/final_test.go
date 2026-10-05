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

// claudeResultSentence is the captured model-not-found result (exact).
const claudeResultSentence = "There's an issue with the selected model (callsheet-no-such-model). It may not exist or you may not have access to it. Run --model to pick a different model."

// claudeDoc is a minimal Claude result object with result s.
func claudeDoc(isError bool, s string) []byte {
	b, _ := json.Marshal(map[string]any{"type": "result", "subtype": "success", "is_error": isError, "result": s})
	return b
}

func finalOf(fm FinalMessage) string {
	if fm.Error != "" {
		return "error:" + fm.Error
	}
	return msgOf(fm)
}

// TestClaudeFinal is UT FP-4: Claude's stdout JSON result, captured
// successes and failures at every chunk boundary, strict JSON, size
// bounds and sealing.
func TestClaudeFinal(t *testing.T) {
	claude := NewClaude("")
	for rel, want := range map[string]string{
		"runs-scratch/claude-stdin-success/stdout.bin": "pong",
		"runs-scratch/claude-stdin-fail/stdout.bin":    claudeResultSentence,
		"runs/claude-success/stdout.bin":               "pong",
		"runs/claude-fail-model/stdout.bin":            claudeResultSentence,
	} {
		out := readCapture(t, rel)
		// Every chunk boundary buffers exactly the stdout bytes (Finish's
		// only input), and the parsed result is checked at the boundaries
		// and a spread of splits (the stress shard repeats this test, so
		// each split is not re-parsed): the exact result, even for is_error
		// true with subtype success.
		for at := 0; at <= len(out); at++ {
			e := claude.NewFinalExtractor().(*claudeExtractor)
			e.Feed(out[:at])
			e.Feed(out[at:])
			if !bytes.Equal(e.buf, out) {
				t.Fatalf("%s split at %d buffered %d bytes", rel, at, len(e.buf))
			}
			if at%61 != 0 && at > 2 && at < len(out)-2 {
				continue
			}
			if got := e.Finish(); finalOf(got) != want || got.Truncated {
				t.Fatalf("%s split at %d: %+v", rel, at, got)
			}
		}
		e := claude.NewFinalExtractor()
		for i := range out {
			e.Feed(out[i : i+1])
		}
		if got := e.Finish(); finalOf(got) != want {
			t.Fatalf("%s byte at a time: %+v", rel, got)
		}
	}
	// The canaries' denial prose is an ordinary answer.
	for _, rel := range []string{"runs/claude-forbidden-home/stdout.bin", "runs/claude-baseline-forbidden-home/stdout.bin", "runs/claude-permitted/stdout.bin"} {
		got := feedAll(claude.NewFinalExtractor(), readCapture(t, rel), nil)
		var doc struct{ Result string }
		json.Unmarshal(readCapture(t, rel), &doc)
		if got.Message == nil || *got.Message != doc.Result || got.Error != "" {
			t.Fatalf("%s = %+v", rel, got)
		}
	}
	for name, c := range map[string]struct{ out, want string }{
		"empty result":          {`{"type":"result","is_error":false,"result":""}`, ""},
		"unknown metadata":      {`{"x":[1,{"y":null}],"type":"result","modelUsage":{},"is_error":true,"result":"r","subtype":"error_max_turns"}`, "r"},
		"trailing whitespace":   {`{"type":"result","is_error":false,"result":"ok"}` + " \r\n\t\n", "ok"},
		"leading whitespace":    {"\n " + `{"type":"result","is_error":false,"result":"ok"}`, "ok"},
		"escapes decoded once":  {`{"type":"result","is_error":false,"result":"a\\nb\né😀"}`, "a\\nb\né😀"},
		"no trimming":           {`{"type":"result","is_error":false,"result":"  x \n"}`, "  x \n"},
		"literal fffd":          {`{"type":"result","is_error":false,"result":"�"}`, "�"},
		"duplicate type":        {`{"type":"result","type":"result","is_error":false,"result":"r"}`, "error:" + FinalInvalid},
		"duplicate result":      {`{"type":"result","is_error":false,"result":"a","result":"b"}`, "error:" + FinalInvalid},
		"duplicate unknown":     {`{"x":1,"x":2,"type":"result","is_error":false,"result":"r"}`, "error:" + FinalInvalid},
		"missing result":        {`{"type":"result","is_error":false}`, "error:" + FinalInvalid},
		"null result":           {`{"type":"result","is_error":false,"result":null}`, "error:" + FinalInvalid},
		"number result":         {`{"type":"result","is_error":false,"result":1}`, "error:" + FinalInvalid},
		"missing is_error":      {`{"type":"result","result":"r"}`, "error:" + FinalInvalid},
		"string is_error":       {`{"type":"result","is_error":"false","result":"r"}`, "error:" + FinalInvalid},
		"null is_error":         {`{"type":"result","is_error":null,"result":"r"}`, "error:" + FinalInvalid},
		"wrong type":            {`{"type":"assistant","is_error":false,"result":"r"}`, "error:" + FinalInvalid},
		"missing type":          {`{"is_error":false,"result":"r"}`, "error:" + FinalInvalid},
		"array":                 {`[{"type":"result","is_error":false,"result":"r"}]`, "error:" + FinalInvalid},
		"string":                {`"r"`, "error:" + FinalInvalid},
		"trailing object":       {`{"type":"result","is_error":false,"result":"r"}{}`, "error:" + FinalInvalid},
		"second result":         {`{"type":"result","is_error":false,"result":"r"}` + "\n" + `{"type":"result","is_error":false,"result":"s"}`, "error:" + FinalInvalid},
		"trailing garbage":      {`{"type":"result","is_error":false,"result":"r"} x`, "error:" + FinalInvalid},
		"truncated":             {`{"type":"result","is_error":false,"result":"r"`, "error:" + FinalInvalid},
		"invalid utf8":          {"{\"type\":\"result\",\"is_error\":false,\"result\":\"a\xff\"}", "error:" + FinalInvalid},
		"lone surrogate":        {`{"type":"result","is_error":false,"result":"\ud800"}`, "error:" + FinalInvalid},
		"lone low surrogate":    {`{"type":"result","is_error":false,"result":"\udc00"}`, "error:" + FinalInvalid},
		"surrogate in metadata": {`{"meta":"\ud800x","type":"result","is_error":false,"result":"r"}`, "error:" + FinalInvalid},
		"bad escape":            {`{"type":"result","is_error":false,"result":"\q"}`, "error:" + FinalInvalid},
		"empty stdout":          {``, "error:" + FinalInvalid},
		"stream json":           {`{"type":"system"}` + "\n" + `{"type":"result","is_error":false,"result":"r"}`, "error:" + FinalInvalid},
	} {
		if got := feedAll(claude.NewFinalExtractor(), []byte(c.out), nil); finalOf(got) != c.want {
			t.Errorf("%s: %+v, want %q", name, got, c.want)
		}
	}
	// A result over 64 KiB is its first 64 KiB ending at a UTF-8 boundary.
	long := strings.Repeat("a", contract.MaxFinalMessageBytes-1) + "é" + "tail"
	got := feedAll(claude.NewFinalExtractor(), claudeDoc(false, long), nil)
	if !got.Truncated || len(*got.Message) != contract.MaxFinalMessageBytes-1 || got.Error != "" {
		t.Fatalf("truncated: %d %v", len(msgOf(got)), got.Truncated)
	}
	exact := strings.Repeat("b", contract.MaxFinalMessageBytes)
	if got := feedAll(claude.NewFinalExtractor(), claudeDoc(true, exact), nil); got.Truncated || len(*got.Message) != contract.MaxFinalMessageBytes {
		t.Fatal("a 64 KiB result was truncated")
	}
	// The 8 MiB input bound: exactly MaxVendorFinalBytes (padded with
	// whitespace) is buffered whole (BenchmarkVendorFinal parses a
	// near-limit document; the stress shard repeats this test, so it is not
	// parsed here); one byte more is final_output_too_large and the buffer
	// is released, without growing afterwards.
	doc := claudeDoc(false, "bounded")
	padded := append(slices.Clone(doc), bytes.Repeat([]byte(" "), MaxVendorFinalBytes-len(doc))...)
	e := claude.NewFinalExtractor().(*claudeExtractor)
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
	// Ownership and sealing: Feed keeps no caller slice; Finish is
	// idempotent, returns owned values and releases the buffer; Feed after
	// Finish is a no-op.
	buf := claudeDoc(false, "own")
	e = claude.NewFinalExtractor().(*claudeExtractor)
	e.Feed(buf)
	copy(buf, bytes.Repeat([]byte("x"), len(buf)))
	first := e.Finish()
	e.Feed(claudeDoc(false, "late"))
	second := e.Finish()
	if msgOf(first) != "own" || msgOf(second) != "own" || e.buf != nil {
		t.Fatalf("finish %q then %q", msgOf(first), msgOf(second))
	}
	*second.Message = "mutated"
	if msgOf(e.Finish()) != "own" {
		t.Fatal("Finish returned shared state")
	}
	if got := claude.NewFinalExtractor().Finish(); got.Error != FinalInvalid {
		t.Fatalf("fresh extractor state leaked: %+v", got)
	}
	// The fake keeps its null-marker semantics with no error code.
	if got := NewFake("").NewFinalExtractor().Finish(); got.Message != nil || got.Error != "" {
		t.Fatalf("fake extraction %+v", got)
	}
}

// TestCodexFinal is UT FP-5: Codex's raw UTF-8 final-file extractor over
// the captured bytes, empty files, rune splits, the 64 KiB answer cap, the
// 8 MiB bound, bounded retention and ownership.
func TestCodexFinal(t *testing.T) {
	codex := NewCodex("")
	pong := readCapture(t, "runs-scratch/codex-skip-success/final.saved.txt")
	if string(pong) != "pong" {
		t.Fatalf("captured final %q", pong)
	}
	for _, rel := range []string{"runs-scratch/codex-skip-success/final.saved.txt", "runs-scratch/codex-skip-forbidden-home/final.saved.txt",
		"runs/codex-success/final.saved.txt", "runs-scratch/codex-skip-permitted/final.saved.txt"} {
		b := readCapture(t, rel)
		for at := 0; at <= len(b); at++ {
			if got := feedAll(codex.NewFinalExtractor(), b, []int{at}); got.Message == nil || *got.Message != string(b) || got.Truncated || got.Error != "" {
				t.Fatalf("%s split at %d: %+v", rel, at, got)
			}
		}
	}
	// An existing empty file is the valid empty answer (Feed never called
	// or with nothing); stdout's JSONL is never fed to this extractor.
	if got := codex.NewFinalExtractor().Finish(); got.Message == nil || *got.Message != "" || got.Error != "" {
		t.Fatalf("empty = %+v", got)
	}
	// Multi-byte runes split at every position; invalid UTF-8 anywhere,
	// or a rune cut off at the end, is invalid_final_output.
	text := []byte("ä€😀x\U0010FFFF✓")
	for a := 0; a <= len(text); a++ {
		for b := a; b <= len(text); b++ {
			if got := feedAll(codex.NewFinalExtractor(), text, []int{a, b}); finalOf(got) != string(text) {
				t.Fatalf("split %d,%d: %+v", a, b, got)
			}
		}
	}
	for name, in := range map[string][]byte{
		"invalid byte":    []byte("ok\xffok"),
		"cut rune":        []byte("ok\xe2\x82"),
		"lone cont":       []byte("\x80"),
		"overlong":        []byte("\xc0\xaf"),
		"surrogate":       []byte("\xed\xa0\x80"),
		"bad after 64KiB": append(bytes.Repeat([]byte("a"), contract.MaxFinalMessageBytes+10), 0xff),
	} {
		for at := 0; at <= len(in) && at < 16; at++ {
			if got := feedAll(codex.NewFinalExtractor(), in, []int{at}); got.Message != nil || got.Error != FinalInvalid {
				t.Fatalf("%s split %d: %+v", name, at, got)
			}
		}
	}
	// The 64 KiB answer cap: exactly 64 KiB is whole; more is its prefix
	// ending at a rune boundary, truncated; the rest is still checked.
	exact := bytes.Repeat([]byte("b"), contract.MaxFinalMessageBytes)
	if got := feedAll(codex.NewFinalExtractor(), exact, []int{100}); got.Truncated || len(*got.Message) != contract.MaxFinalMessageBytes {
		t.Fatalf("64 KiB: %d %v", len(msgOf(got)), got.Truncated)
	}
	over := append(bytes.Repeat([]byte("a"), contract.MaxFinalMessageBytes-1), []byte("é tail")...)
	for _, at := range []int{0, contract.MaxFinalMessageBytes - 1, contract.MaxFinalMessageBytes, len(over)} {
		got := feedAll(codex.NewFinalExtractor(), over, []int{at})
		if !got.Truncated || len(*got.Message) != contract.MaxFinalMessageBytes-1 || !utf8.ValidString(*got.Message) {
			t.Fatalf("truncated at split %d: %d %v", at, len(msgOf(got)), got.Truncated)
		}
	}
	// The 8 MiB hard bound: exactly MaxVendorFinalBytes of valid text is a
	// truncated answer retaining at most 64 KiB; one byte more is
	// final_output_too_large, releasing what it held.
	chunk := bytes.Repeat([]byte("ü"), 32<<10) // 64 KiB of two-byte runes
	x := codex.NewFinalExtractor().(*rawExtractor)
	for n := 0; n < MaxVendorFinalBytes; n += len(chunk) {
		x.Feed(chunk)
		if cap(x.kept) > contract.MaxFinalMessageBytes {
			t.Fatalf("retained %d bytes", cap(x.kept))
		}
	}
	if got := x.Finish(); !got.Truncated || len(*got.Message) != contract.MaxFinalMessageBytes || x.kept != nil {
		t.Fatalf("8 MiB: %d %v", len(msgOf(got)), got.Truncated)
	}
	// Crossing the bound within a chunk (the count as after 8 MiB - 1
	// valid bytes), or in one chunk, releases what was held.
	x = codex.NewFinalExtractor().(*rawExtractor)
	x.Feed(chunk)
	x.total = MaxVendorFinalBytes - 1
	x.Feed([]byte("ab"))
	x.Feed(chunk)
	if got := x.Finish(); got.Message != nil || got.Error != FinalTooLarge || x.kept != nil {
		t.Fatalf("over 8 MiB: %+v", got)
	}
	x = codex.NewFinalExtractor().(*rawExtractor)
	x.Feed(make([]byte, MaxVendorFinalBytes+1))
	if got := x.Finish(); got.Message != nil || got.Error != FinalTooLarge {
		t.Fatalf("one oversized chunk: %+v", got)
	}
	// Ownership: no caller slice is kept; Finish seals and repeats owned
	// values; Feed afterwards is ignored.
	buf := []byte("own")
	x = codex.NewFinalExtractor().(*rawExtractor)
	x.Feed(buf)
	copy(buf, "xxx")
	first := x.Finish()
	x.Feed([]byte(" more"))
	second := x.Finish()
	if msgOf(first) != "own" || msgOf(second) != "own" {
		t.Fatalf("finish %q then %q", msgOf(first), msgOf(second))
	}
	*second.Message = "mutated"
	if msgOf(x.Finish()) != "own" {
		t.Fatal("Finish returned shared state")
	}
}

// BenchmarkVendorFinal measures every vendor extractor over the captured
// outputs, near-limit valid inputs and oversized inputs at chunk sizes 1,
// 4096 and 65536, checking the exact answer, truncation or error code and
// that nothing is retained after sealing or overflow. Iteration 11 adds
// Grok's success, error and cancelled captures, Cursor's result and absent
// output, and synthetic near-8-MiB valid and oversized Grok and Cursor
// documents.
func BenchmarkVendorFinal(b *testing.B) {
	captured := readCapture(b, "runs-scratch/claude-stdin-success/stdout.bin")
	nearDoc := claudeDoc(false, strings.Repeat("a", MaxVendorFinalBytes-128))
	oversized := append(slices.Clone(nearDoc), bytes.Repeat([]byte(" "), 256)...)
	pong := readCapture(b, "runs-scratch/codex-skip-success/final.saved.txt")
	cap64 := bytes.Repeat([]byte("é"), contract.MaxFinalMessageBytes/2)
	big := bytes.Repeat([]byte("ab"), MaxVendorFinalBytes/2)
	grokSuccess := wave2Capture(b, "runs/grok-stdin-success/stdout.bin")
	grokError := wave2Capture(b, "runs/grok-stdin-fail/stdout.bin")
	grokCancelled := wave2Capture(b, "runs/grok-shell-home/stdout.bin")
	var cancelledDoc struct{ Text string }
	json.Unmarshal(grokCancelled, &cancelledDoc)
	grokNear := grokDoc(strings.Repeat("é", (MaxVendorFinalBytes-256)/2))
	grokOver := append(slices.Clone(grokNear), bytes.Repeat([]byte(" "), MaxVendorFinalBytes-len(grokNear)+1)...)
	cursorResult := wave2Capture(b, "runs/cursor-stdin-success/stdout.bin")
	cursorOver := append(slices.Clone(nearDoc), bytes.Repeat([]byte(" "), MaxVendorFinalBytes-len(nearDoc)+1)...)
	cases := []struct {
		name  string
		a     Adapter
		in    []byte
		check func(FinalMessage) bool
	}{
		{"claude/captured", NewClaude(""), captured, func(m FinalMessage) bool { return finalOf(m) == "pong" && !m.Truncated }},
		{"claude/near-8MiB", NewClaude(""), nearDoc, func(m FinalMessage) bool {
			return m.Truncated && len(*m.Message) == contract.MaxFinalMessageBytes && m.Error == ""
		}},
		{"claude/oversized", NewClaude(""), oversized, func(m FinalMessage) bool { return m.Message == nil && m.Error == FinalTooLarge }},
		{"codex/captured", NewCodex(""), pong, func(m FinalMessage) bool { return finalOf(m) == "pong" && !m.Truncated }},
		{"codex/64KiB", NewCodex(""), cap64, func(m FinalMessage) bool { return !m.Truncated && len(*m.Message) == contract.MaxFinalMessageBytes }},
		{"codex/8MiB", NewCodex(""), big, func(m FinalMessage) bool {
			return m.Truncated && len(*m.Message) == contract.MaxFinalMessageBytes && m.Error == ""
		}},
		{"grok/success", NewGrok(""), grokSuccess, func(m FinalMessage) bool { return finalOf(m) == "pong" && !m.Truncated }},
		{"grok/error", NewGrok(""), grokError, func(m FinalMessage) bool { return finalOf(m) == grokFailMessage && !m.Truncated }},
		{"grok/cancelled", NewGrok(""), grokCancelled, func(m FinalMessage) bool {
			return cancelledDoc.Text != "" && finalOf(m) == cancelledDoc.Text && !m.Truncated
		}},
		{"grok/near-8MiB", NewGrok(""), grokNear, func(m FinalMessage) bool {
			return m.Truncated && len(*m.Message) == contract.MaxFinalMessageBytes && utf8.ValidString(*m.Message) && m.Error == ""
		}},
		{"grok/oversized", NewGrok(""), grokOver, func(m FinalMessage) bool { return m.Message == nil && m.Error == FinalTooLarge }},
		{"cursor/result", NewCursor(""), cursorResult, func(m FinalMessage) bool { return finalOf(m) == "pong" && !m.Truncated }},
		{"cursor/absent", NewCursor(""), nil, func(m FinalMessage) bool { return m.Message == nil && m.Error == FinalInvalid }},
		{"cursor/near-8MiB", NewCursor(""), nearDoc, func(m FinalMessage) bool {
			return m.Truncated && len(*m.Message) == contract.MaxFinalMessageBytes && m.Error == ""
		}},
		{"cursor/oversized", NewCursor(""), cursorOver, func(m FinalMessage) bool { return m.Message == nil && m.Error == FinalTooLarge }},
	}
	for _, c := range cases {
		for _, size := range []int{1, 4096, 65536} {
			b.Run(c.name+"/chunk="+itoa(size), func(b *testing.B) {
				b.SetBytes(int64(len(c.in)))
				b.ReportAllocs()
				for b.Loop() {
					e := c.a.NewFinalExtractor()
					for off := 0; off < len(c.in); off += size {
						e.Feed(c.in[off:min(off+size, len(c.in))])
					}
					m := e.Finish()
					if !c.check(m) {
						b.Fatalf("%s: %+v", c.name, finalOf(m)[:min(len(finalOf(m)), 80)])
					}
					switch x := e.(type) {
					case *claudeExtractor:
						if x.buf != nil {
							b.Fatal("claude retained its buffer after sealing")
						}
					case *grokExtractor:
						if x.buf != nil {
							b.Fatal("grok retained its buffer after sealing")
						}
					case *rawExtractor:
						if x.kept != nil {
							b.Fatal("codex retained its prefix after sealing")
						}
					}
					e.Feed(c.in[:min(len(c.in), 4096)])
					if again := e.Finish(); finalOf(again) != finalOf(m) {
						b.Fatal("Feed after Finish changed the result")
					}
				}
			})
		}
	}
}

// BenchmarkVendorInvocation measures building Claude's and Codex's
// invocations with a small and the maximum legal prompt, checking the
// literal argv and that stdin is an owned copy; iteration 11 adds Grok's
// with a small and the 32 KiB prompt (the prompt the last, owned argv
// element, stdin empty) and Cursor's refusal (a zero Invocation).
func BenchmarkVendorInvocation(b *testing.B) {
	scratch := "/tmp/callsheet-task-" + taskID + "-1"
	for _, size := range []int{64, MaxGrokPromptBytes} {
		prompt := bytes.Repeat([]byte("p"), size)
		b.Run("grok/prompt="+itoa(size), func(b *testing.B) {
			grok := NewGrok("")
			b.SetBytes(int64(size))
			b.ReportAllocs()
			for b.Loop() {
				inv, err := grok.Invocation(TaskInput{TaskID: taskID, Model: "grok-4.7", Effort: "low", Prompt: prompt, ScratchDir: scratch})
				if err != nil || len(inv.Argv) != 10 || inv.Argv[9] != string(prompt) || inv.Stdin != nil || inv.Argv[3] != "grok-4.7" {
					b.Fatalf("grok invocation %v", err)
				}
			}
		})
	}
	b.Run("cursor/refused", func(b *testing.B) {
		cursor := NewCursor("")
		prompt := []byte("Reply with exactly the single word pong.")
		b.ReportAllocs()
		for b.Loop() {
			inv, err := cursor.Invocation(TaskInput{TaskID: taskID, Model: "grok-4.7", Effort: "low", Prompt: prompt, ScratchDir: scratch})
			if err == nil || inv.Argv != nil || inv.Stdin != nil {
				b.Fatalf("cursor invocation %+v %v", inv, err)
			}
		}
	})
	for _, size := range []int{64, contract.MaxPromptBytes} {
		prompt := bytes.Repeat([]byte("p"), size)
		for _, q := range Qualifications()[:2] {
			if q.ID != ClaudeID && q.ID != CodexID {
				b.Fatalf("qualification order %+v", Qualifications())
			}
			a, _ := Builtin("").Lookup(q.ID)
			b.Run(q.ID+"/prompt="+itoa(size), func(b *testing.B) {
				b.SetBytes(int64(size))
				b.ReportAllocs()
				for b.Loop() {
					inv, err := a.Invocation(TaskInput{TaskID: taskID, Model: q.Model, Effort: q.Effort, Prompt: prompt, ScratchDir: scratch})
					if err != nil || inv.Argv[slices.Index(inv.Argv, "--model")+1] != q.Model || len(inv.Stdin) != size || &inv.Stdin[0] == &prompt[0] {
						b.Fatalf("%s invocation %q %v", q.ID, inv.Argv, err)
					}
				}
			})
		}
	}
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var d []byte
	for ; n > 0; n /= 10 {
		d = append([]byte{byte('0' + n%10)}, d...)
	}
	return string(d)
}
