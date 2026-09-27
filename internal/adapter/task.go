package adapter

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"slices"
	"unicode/utf8"

	"github.com/wedevwork/callsheet/internal/contract"
)

// The fake adapter's task boundary (iteration 05). Production knows only
// this argv and the final marker; it never interprets fixture modes.
const (
	// TaskArg selects the fake's task mode.
	TaskArg = "--callsheet-task"
	// FinalMarkerType is the type of the fake's final-message NDJSON line.
	FinalMarkerType = "callsheet_final"
	// maxMarkerLine bounds one candidate stdout line of the extractor.
	maxMarkerLine = 512 << 10
)

// TaskInput is one task's adapter input: the task ID, the explicit model
// and effort, and the composed prompt (at most contract.MaxPromptBytes).
type TaskInput struct {
	TaskID string
	Model  string
	Effort string
	Prompt []byte
}

// Invocation is how to run one task: argument vector (arguments only; the
// executable is sidecar-local) and the finite stdin.
type Invocation struct {
	Argv  []string
	Stdin []byte
}

// FinalMessage is an extracted final message: Message nil means no valid
// marker, not an inferred success or failure.
type FinalMessage struct {
	Message   *string
	Truncated bool
}

// FinalExtractor incrementally recognizes a task's final message in its
// stdout. Feed consumes bytes synchronously and retains no caller slice;
// it accepts arbitrary chunk boundaries and reports no error for
// malformed output. Finish handles the unterminated last line, seals the
// extractor and returns an owned value; repeated Finish returns the same
// value and Feed after Finish is a no-op. One goroutine feeds it.
type FinalExtractor interface {
	Feed(stdout []byte)
	Finish() FinalMessage
}

// errInvocation is a safe invocation refusal.
func errInvocation(msg string) error { return errors.New("adapter: " + msg) }

// Invocation validates the task ID, model and effort and the prompt
// bound, and returns owned copies: exactly
// --callsheet-task --model MODEL --effort EFFORT, the prompt on stdin. No
// shell, splitting, environment expansion or vendor default.
func (f *fake) Invocation(in TaskInput) (Invocation, error) {
	switch {
	case !contract.ValidTaskID(in.TaskID):
		return Invocation{}, errInvocation("invalid task ID")
	case !contract.ValidModel(in.Model):
		return Invocation{}, errInvocation("invalid model")
	case !slices.Contains(fakeEfforts, in.Effort):
		return Invocation{}, errInvocation("effort is not allowed for adapter fake")
	case len(in.Prompt) > contract.MaxPromptBytes:
		return Invocation{}, errInvocation("the prompt exceeds 16 MiB")
	}
	return Invocation{Argv: []string{TaskArg, "--model", in.Model, "--effort", in.Effort}, Stdin: bytes.Clone(in.Prompt)}, nil
}

// NewFinalExtractor returns fresh task-local extraction state.
func (f *fake) NewFinalExtractor() FinalExtractor { return &markerExtractor{} }

// markerExtractor recognizes stdout NDJSON objects of exactly
// {"type":"callsheet_final","message":STRING}. A candidate line is at most
// 512 KiB; an overlong one is discarded until its LF without growing
// memory. The last valid marker wins; a message over 64 KiB is kept as
// its first 64 KiB ending at a UTF-8 boundary, marked truncated.
type markerExtractor struct {
	line   []byte
	over   bool
	last   FinalMessage
	sealed bool
}

func (m *markerExtractor) Feed(b []byte) {
	if m.sealed {
		return
	}
	for len(b) > 0 {
		i := bytes.IndexByte(b, '\n')
		chunk := b
		if i >= 0 {
			chunk = b[:i]
		}
		if !m.over {
			if len(m.line)+len(chunk) > maxMarkerLine {
				m.over, m.line = true, m.line[:0]
			} else {
				m.line = appendBounded(m.line, chunk)
			}
		}
		if i < 0 {
			return
		}
		m.endLine()
		b = b[i+1:]
	}
}

func (m *markerExtractor) endLine() {
	if !m.over {
		if msg, trunc, ok := parseMarker(m.line); ok {
			m.last = FinalMessage{Message: &msg, Truncated: trunc}
		}
	}
	m.line, m.over = m.line[:0], false
}

func (m *markerExtractor) Finish() FinalMessage {
	if !m.sealed {
		if len(m.line) > 0 || m.over {
			m.endLine()
		}
		m.sealed, m.line = true, nil
	}
	out := FinalMessage{Truncated: m.last.Truncated}
	if m.last.Message != nil {
		s := *m.last.Message
		out.Message = &s
	}
	return out
}

// appendBounded appends chunk to line, growing its capacity at most to
// maxMarkerLine (the caller checked the length bound).
func appendBounded(line, chunk []byte) []byte {
	if need := len(line) + len(chunk); need > cap(line) {
		grown := make([]byte, len(line), min(max(2*cap(line), need, 256), maxMarkerLine))
		copy(grown, line)
		line = grown
	}
	return append(line, chunk...)
}

// parseMarker recognizes one marker line: one JSON object with exactly
// the keys type and message (no duplicates), type callsheet_final and a
// valid UTF-8 string message.
func parseMarker(line []byte) (string, bool, bool) {
	if !utf8.Valid(line) {
		return "", false, false
	}
	dec := json.NewDecoder(bytes.NewReader(line))
	if tok, err := dec.Token(); err != nil || tok != json.Delim('{') {
		return "", false, false
	}
	var typ, msg *string
	for dec.More() {
		tok, err := dec.Token()
		key, ok := tok.(string)
		if err != nil || !ok {
			return "", false, false
		}
		var raw json.RawMessage
		if err := dec.Decode(&raw); err != nil {
			return "", false, false
		}
		v, ok := contract.JSONString(raw)
		if !ok {
			return "", false, false
		}
		switch {
		case key == "type" && typ == nil:
			typ = &v
		case key == "message" && msg == nil:
			msg = &v
		default:
			return "", false, false
		}
	}
	if tok, err := dec.Token(); err != nil || tok != json.Delim('}') {
		return "", false, false
	}
	if _, err := dec.Token(); err != io.EOF || typ == nil || msg == nil || *typ != FinalMarkerType {
		return "", false, false
	}
	s := *msg
	if len(s) <= contract.MaxFinalMessageBytes {
		return s, false, true
	}
	cut := contract.MaxFinalMessageBytes
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut], true, true
}
