package adapter

import (
	"bytes"
	"slices"
	"strings"
	"syscall"
	"testing"

	"github.com/wedevwork/callsheet/internal/contract"
)

const taskID = "t_0123456789abcdef0123456789abcdef"

// countedFake is the fake whose probe executor counts child starts: the
// task boundary must never start one (launch ledger: 0).
func countedFake(t *testing.T) (*fake, *int) {
	n := 0
	f := &fake{p: &prober{dir: t.TempDir(), environ: func() []string { return nil }, clock: realClock{}, started: func(int) { n++ }}}
	t.Cleanup(func() {
		if n != 0 {
			t.Errorf("the task boundary started %d probe children, want 0", n)
		}
	})
	return f, &n
}

func feedAll(e FinalExtractor, out []byte, split []int) FinalMessage {
	prev := 0
	for _, at := range split {
		e.Feed(out[prev:at])
		prev = at
	}
	e.Feed(out[prev:])
	return e.Finish()
}

func msgOf(fm FinalMessage) string {
	if fm.Message == nil {
		return "<nil>"
	}
	return *fm.Message
}

// TestTaskAdapter is UT FP-4/5 for the adapter's task boundary; its
// invocation, extraction and signals subtests are delegated from
// tests/function (TestTaskExecution/invoke). They are pure: no Probe and no
// executable is started. Do not rename or skip them.
func TestTaskAdapter(t *testing.T) {
	t.Run("invocation", func(t *testing.T) {
		f, _ := countedFake(t)
		prompt := []byte(`{"format":"callsheet-task-v1"}` + "\n")
		inv, err := f.Invocation(TaskInput{TaskID: taskID, Model: "example model -x", Effort: "medium", Prompt: prompt})
		if err != nil {
			t.Fatal(err)
		}
		// Model and effort are always two explicit option/value pairs; a
		// model with spaces or a leading dash is one argv value.
		if want := []string{"--callsheet-task", "--model", "example model -x", "--effort", "medium"}; !slices.Equal(inv.Argv, want) || !bytes.Equal(inv.Stdin, prompt) {
			t.Fatalf("invocation = %q %q", inv.Argv, inv.Stdin)
		}
		// Owned copies: mutating the input or the output changes nothing.
		prompt[0] = 'X'
		if inv.Stdin[0] != '{' {
			t.Fatal("stdin aliases the input prompt")
		}
		inv.Argv[2] = "mutated"
		again, _ := f.Invocation(TaskInput{TaskID: taskID, Model: "m", Effort: "low", Prompt: nil})
		if again.Argv[2] != "m" || len(again.Stdin) != 0 {
			t.Fatalf("second invocation %q", again.Argv)
		}
		for name, in := range map[string]TaskInput{
			"task id":  {TaskID: "t_x", Model: "m", Effort: "low"},
			"model":    {TaskID: taskID, Model: " ", Effort: "low"},
			"control":  {TaskID: taskID, Model: "a\nb", Effort: "low"},
			"effort":   {TaskID: taskID, Model: "m", Effort: "extreme"},
			"no model": {TaskID: taskID, Effort: "low"},
			"prompt":   {TaskID: taskID, Model: "m", Effort: "low", Prompt: make([]byte, contract.MaxPromptBytes+1)},
		} {
			if _, err := f.Invocation(in); err == nil {
				t.Fatalf("%s accepted", name)
			}
		}
		if _, err := f.Invocation(TaskInput{TaskID: taskID, Model: "m", Effort: "high", Prompt: make([]byte, contract.MaxPromptBytes)}); err != nil {
			t.Fatalf("a 16 MiB prompt: %v", err)
		}
		// The product and the fixture agree on the task argument.
		if TaskArg != "--callsheet-task" || FinalMarkerType != "callsheet_final" {
			t.Fatal("task protocol constants changed")
		}
		// The registry's fake exposes the same boundary.
		a, _ := Builtin("").Lookup(FakeID)
		if inv, err := a.Invocation(TaskInput{TaskID: taskID, Model: "m", Effort: "low"}); err != nil || inv.Argv[0] != TaskArg {
			t.Fatalf("builtin invocation %v %v", inv, err)
		}
	})
	t.Run("extraction", func(t *testing.T) {
		f, _ := countedFake(t)
		marker := `{"type":"callsheet_final","message":"fake task completed"}`
		out := []byte("progress 1\n" + marker + "\nnot json\n{\"type\":\"other\",\"message\":\"x\"}\n")
		// Every chunk boundary yields the same result.
		for at := 0; at <= len(out); at++ {
			if got := feedAll(f.NewFinalExtractor(), out, []int{at}); msgOf(got) != "fake task completed" || got.Truncated {
				t.Fatalf("split at %d: %+v", at, got)
			}
		}
		// Byte-at-a-time feeding.
		e := f.NewFinalExtractor()
		for i := range out {
			e.Feed(out[i : i+1])
		}
		if got := e.Finish(); msgOf(got) != "fake task completed" {
			t.Fatalf("byte at a time: %+v", got)
		}
		for name, c := range map[string]struct {
			out, want string
			trunc     bool
		}{
			"none":            {"hello\nworld\n", "<nil>", false},
			"last wins":       {`{"type":"callsheet_final","message":"a"}` + "\n" + `{"type":"callsheet_final","message":"b"}` + "\n", "b", false},
			"unterminated":    {"x\n" + `{"type":"callsheet_final","message":"eof"}`, "eof", false},
			"crlf":            {`{"type":"callsheet_final","message":"crlf"}` + "\r\n", "crlf", false},
			"extra key":       {`{"type":"callsheet_final","message":"a","x":1}` + "\n", "<nil>", false},
			"duplicate":       {`{"type":"callsheet_final","message":"a","message":"b"}` + "\n", "<nil>", false},
			"number":          {`{"type":"callsheet_final","message":1}` + "\n", "<nil>", false},
			"null":            {`{"type":"callsheet_final","message":null}` + "\n", "<nil>", false},
			"wrong type":      {`{"type":"final","message":"a"}` + "\n", "<nil>", false},
			"array":           {`[{"type":"callsheet_final","message":"a"}]` + "\n", "<nil>", false},
			"trailing":        {`{"type":"callsheet_final","message":"a"} {}` + "\n", "<nil>", false},
			"invalid utf8":    {"{\"type\":\"callsheet_final\",\"message\":\"a\xff\"}\n", "<nil>", false},
			"lone surrogate":  {`{"type":"callsheet_final","message":"\ud800"}` + "\n", "<nil>", false},
			"literal fffd":    {`{"type":"callsheet_final","message":"�"}` + "\n", "�", false},
			"escaped fffd":    {`{"type":"callsheet_final","message":"a\ufffdb"}` + "\n", "a�b", false},
			"paired escapes":  {`{"type":"callsheet_final","message":"\ud800\udc00"}` + "\n", "\U00010000", false},
			"lone low":        {`{"type":"callsheet_final","message":"\udc00x"}` + "\n", "<nil>", false},
			"mixed literal":   {`{"type":"callsheet_final","message":"\ud800 �"}` + "\n", "<nil>", false},
			"mixed escaped":   {`{"type":"callsheet_final","message":"\ufffd\ud800"}` + "\n", "<nil>", false},
			"escaped in type": {`{"type":"callsheet\u005ffinal","message":"t"}` + "\n", "t", false},
			"escaped slash":   {`{"type":"callsheet_final","message":"\\ud800"}` + "\n", "\\ud800", false},
			"empty message":   {`{"type":"callsheet_final","message":""}` + "\n", "", false},
			"malformed later": {`{"type":"callsheet_final","message":"kept"}` + "\n" + `{"type":"callsheet_final",` + "\n", "kept", false},
		} {
			if got := feedAll(f.NewFinalExtractor(), []byte(c.out), nil); msgOf(got) != c.want || got.Truncated != c.trunc {
				t.Errorf("%s: %+v", name, got)
			}
		}
		// A message over 64 KiB is kept as its first 64 KiB ending at a
		// UTF-8 boundary, when its line stays within 512 KiB.
		long := strings.Repeat("a", contract.MaxFinalMessageBytes-1) + "é" + "tail"
		got := feedAll(f.NewFinalExtractor(), []byte(`{"type":"callsheet_final","message":"`+long+`"}`+"\n"), nil)
		if !got.Truncated || len(*got.Message) != contract.MaxFinalMessageBytes-1 || !strings.HasSuffix(*got.Message, "a") {
			t.Fatalf("truncated: %d %v", len(msgOf(got)), got.Truncated)
		}
		exact := strings.Repeat("b", contract.MaxFinalMessageBytes)
		if got := feedAll(f.NewFinalExtractor(), []byte(`{"type":"callsheet_final","message":"`+exact+`"}`), nil); got.Truncated || len(*got.Message) != contract.MaxFinalMessageBytes {
			t.Fatal("a 64 KiB message was truncated")
		}
		// An overlong line (over 512 KiB) is discarded until LF: the
		// earlier marker survives and memory does not grow with it.
		over := `{"type":"callsheet_final","message":"` + strings.Repeat("c", maxMarkerLine) + `"}`
		e = f.NewFinalExtractor()
		e.Feed([]byte(marker + "\n"))
		for i := 0; i < len(over); i += 4096 {
			e.Feed([]byte(over[i:min(i+4096, len(over))]))
		}
		if me := e.(*markerExtractor); cap(me.line) > maxMarkerLine+4096 || !me.over {
			t.Fatalf("overlong line retained %d bytes", cap(me.line))
		}
		e.Feed([]byte("\n" + `{"type":"callsheet_final","message":"after"}`))
		if got := e.Finish(); msgOf(got) != "after" {
			t.Fatalf("after overlong: %+v", got)
		}
		// Feed retains no caller slice; Finish seals and repeats.
		buf := []byte(`{"type":"callsheet_final","message":"own"}`)
		e = f.NewFinalExtractor()
		e.Feed(buf)
		copy(buf, bytes.Repeat([]byte("x"), len(buf)))
		first := e.Finish()
		e.Feed([]byte("\n" + `{"type":"callsheet_final","message":"late"}` + "\n"))
		second := e.Finish()
		if msgOf(first) != "own" || msgOf(second) != "own" {
			t.Fatalf("finish %q then %q", msgOf(first), msgOf(second))
		}
		*second.Message = "mutated"
		if msgOf(e.Finish()) != "own" {
			t.Fatal("Finish returned shared state")
		}
		// Fresh task-local state per extractor.
		if msgOf(f.NewFinalExtractor().Finish()) != "<nil>" {
			t.Fatal("extractor state leaked")
		}
	})
	t.Run("signals", func(t *testing.T) {
		countedFake(t)
		// The native table maps every entry into the closed wire set, and
		// the three signals a supervisor sends keep their names.
		if len(nativeSignals) < 29 {
			t.Fatalf("native table has %d entries", len(nativeSignals))
		}
		for sig, name := range nativeSignals {
			if !contract.ValidWireSignal(name) || name == contract.SignalUnknown {
				t.Fatalf("%d maps to %q", int(sig), name)
			}
		}
		for sig, want := range map[syscall.Signal]string{syscall.SIGTERM: "SIGTERM", syscall.SIGINT: "SIGINT", syscall.SIGKILL: "SIGKILL",
			syscall.SIGABRT: "SIGABRT", syscall.SIGSEGV: "SIGSEGV"} {
			if got := SignalName(sig); got != want {
				t.Fatalf("SignalName(%d) = %q, want %q", int(sig), got, want)
			}
		}
		// Every enumerated entry of this platform maps as listed: aliases
		// to their canonical name.
		for _, e := range nativeSignalEntries {
			if got := SignalName(e.sig); got != e.name {
				t.Fatalf("entry %d %s maps to %s", int(e.sig), e.name, got)
			}
		}
		// Unknown native values (a real-time signal, an impossible number)
		// are SIGUNKNOWN, never a success or a localized String().
		for _, sig := range []syscall.Signal{0, 34, 64, 200} {
			if _, known := nativeSignals[sig]; known {
				continue
			}
			if got := SignalName(sig); got != contract.SignalUnknown {
				t.Fatalf("SignalName(%d) = %q", int(sig), got)
			}
		}
		// Injected tables: the mapping logic independent of the host,
		// for a Linux-like and a Darwin-like numbering.
		linuxLike := mustSignals([]signalEntry{{15, "SIGTERM"}, {6, "SIGABRT"}, {6, "SIGABRT"}, {16, "SIGSTKFLT"}, {17, "SIGCHLD"}})
		darwinLike := mustSignals([]signalEntry{{15, "SIGTERM"}, {7, "SIGEMT"}, {29, "SIGINFO"}, {20, "SIGCHLD"}})
		for _, c := range []struct {
			table map[syscall.Signal]string
			sig   syscall.Signal
			want  string
		}{
			{linuxLike, 15, "SIGTERM"}, {linuxLike, 16, "SIGSTKFLT"}, {linuxLike, 7, contract.SignalUnknown}, {linuxLike, 17, "SIGCHLD"},
			{darwinLike, 7, "SIGEMT"}, {darwinLike, 29, "SIGINFO"}, {darwinLike, 16, contract.SignalUnknown}, {darwinLike, 20, "SIGCHLD"},
		} {
			if got := signalNameIn(c.table, c.sig); got != c.want {
				t.Fatalf("injected %d = %q, want %q", int(c.sig), got, c.want)
			}
		}
		// A table can never map one value to two names, or to a name
		// outside the wire set (aliases included).
		for name, entries := range map[string][]signalEntry{
			"conflict": {{6, "SIGABRT"}, {6, "SIGIOT"}},
			"alias":    {{6, "SIGIOT"}},
			"bare":     {{15, "TERM"}},
			"unknown":  {{99, contract.SignalUnknown}},
		} {
			if _, err := buildSignals(entries); err == nil {
				t.Fatalf("%s table accepted", name)
			}
		}
	})
}
