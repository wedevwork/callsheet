package adapter

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"unicode/utf8"

	"github.com/wedevwork/callsheet/internal/contract"
)

// The production worker adapters (iteration 08): Claude and Codex. Each is
// enabled only by an explicit absolute executable path on the worker, runs
// the owner's captured Linux recipe with the prompt on stdin, and accepts
// only its qualified version and model/effort pair. Neither adapter
// discovers executables, resolves aliases, substitutes operator defaults
// or starts a process outside Probe.
//
// Iteration 11 (wave 2) adds Grok Build and Cursor Agent from the
// coordinator's Linux qualification of 2026-10-04. Grok runs the measured
// dontAsk recipe with the composed prompt as the one -p argument (stdin
// stays empty), on Linux only. Cursor is registered and version-probed but
// its unattended task execution is refused on every OS: the measured
// force/trust candidate wrote outside its scratch directory, the baseline
// refused workspace trust and the sandbox-enabled attempts failed
// authentication. Execution eligibility is ValidateWorkerPosture's
// decision, separate from selection and version knowledge.

// Adapter IDs.
const (
	ClaudeID = "claude"
	CodexID  = "codex"
	// GrokID and CursorID (iteration 11). The captured Cursor executable is
	// cursor-agent; its agent alias was not qualified.
	GrokID   = "grok"
	CursorID = "cursor"
)

// MaxGrokPromptBytes (iteration 11) bounds Grok's complete composed prompt
// (manuals and envelope included), passed as one argv element: a
// conservative product argv policy, not a measured vendor limit. Other
// vendors keep contract.MaxPromptBytes on stdin.
const MaxGrokPromptBytes = 32 << 10

// Grok prompt refusals (iteration 11), checked in this order after the
// task ID, model and selection; each is passed to errInvocation.
const (
	grokPromptEmptyReason    = "the grok prompt must not be empty"
	grokPromptTooLargeReason = "the grok prompt exceeds 32 KiB"
	grokPromptUTF8Reason     = "the grok prompt must be valid UTF-8"
	grokPromptNULReason      = "the grok prompt must not contain NUL"
)

// Worker posture refusals (iteration 11): fixed text, never configuration
// or environment details.
var (
	errGrokPosture        = errors.New("grok worker execution is not qualified on this OS; consult the support catalog")
	errCursorPosture      = errors.New("cursor worker execution is refused: no qualified unattended recipe preserves the operator posture; consult the support catalog")
	errUnsupportedPosture = errors.New("adapter: unknown adapter; registered adapters: " + registeredAdapters)
)

// ValidateWorkerPosture reports whether adapter id may execute tasks on
// goos (the explicit OS seam the sidecar supplies, never a runtime
// lookup), with no I/O: Grok only on linux; Cursor never; claude, codex
// and fake nil here (their existing platform checks still apply); any
// other ID a fixed unsupported-adapter error. It never probes
// configuration and grants no override: a known selection or a matching
// version does not make a worker eligible.
func ValidateWorkerPosture(id, goos string) error {
	switch id {
	case ClaudeID, CodexID, FakeID:
		return nil
	case GrokID:
		if goos == "linux" {
			return nil
		}
		return errGrokPosture
	case CursorID:
		return errCursorPosture
	}
	return errUnsupportedPosture
}

// CodexFinalName is the fixed basename of Codex's final-message file in
// the task's private scratch directory: private to each newly created
// directory, never derived from task or model text.
const CodexFinalName = "callsheet-final.txt"

// VersionArg is the vendors' probe argument.
const VersionArg = "--version"

// Qualification is one vendor's qualified recipe inputs: the exact
// version output (after one trailing LF or CRLF is removed) and the single
// accepted model/effort pair, as captured on Linux on 2026-09-30 (Claude,
// Codex) and 2026-10-04 (Grok, Cursor). It is selection and version
// knowledge only, separate from execution eligibility
// (ValidateWorkerPosture): Cursor's row is a known pair whose execution is
// refused on every OS, Grok's is eligible on Linux only.
type Qualification struct {
	ID      string
	Version string
	Model   string
	Effort  string
}

// qualifications is the immutable qualification table. Further versions
// or pairs need evidence and a table and catalog change; there is no
// allow-unknown switch.
//
// Cursor's Callsheet pair (grok-4.7, low) maps literally to the vendor
// model argument grok-4.7-low (no model-plus-effort concatenation); its
// listed medium/high/xhigh variants and bracket syntax are evidence only.
// The observed but rejected Cursor candidate was
// -p --output-format json --model grok-4.7-low --force --trust with the
// prompt on stdin: it is recorded in the fixtures and catalog for
// comparison, never emitted.
var qualifications = []Qualification{
	{ID: ClaudeID, Version: "2.1.285 (Claude Code)", Model: "sonnet", Effort: "low"},
	{ID: CodexID, Version: "codex-cli 0.159.0", Model: "gpt-6.1-sol", Effort: "low"},
	{ID: CursorID, Version: "2026.10.01-e373342", Model: "grok-4.7", Effort: "low"},
	{ID: GrokID, Version: "grok 1.0.46 (2765805b9442) [stable]", Model: "grok-4.7", Effort: "low"},
}

// Qualifications returns a copy of the qualification table, sorted by ID.
// Presence in this table is selection and version knowledge, not a ready
// worker: consumers must not equate it with execution eligibility, which
// ValidateWorkerPosture and the sidecar's role checks decide.
func Qualifications() []Qualification { return slices.Clone(qualifications) }

// qualified returns the qualification of a real vendor.
func qualified(id string) (Qualification, bool) {
	for _, q := range qualifications {
		if q.ID == id {
			return q, true
		}
	}
	return Qualification{}, false
}

// Pair renders the supported pair of q: model MODEL, effort EFFORT.
func (q Qualification) Pair() string { return "model " + q.Model + ", effort " + q.Effort }

// SelectionError is a refused model/effort selection: a fixed safe
// message naming the adapter and its supported pair, and the offending
// field (model or effort). It never reflects the submitted values.
type SelectionError struct {
	Field string
	msg   string
}

func (e *SelectionError) Error() string { return e.msg }

// registeredAdapters is the unknown-adapter diagnostic's list.
const registeredAdapters = "claude, codex, cursor, fake, grok"

// ValidateSelection checks that adapter id accepts model and effort, with
// no I/O: a real vendor accepts exactly its qualified pair; the fake
// accepts any valid model text with one of its efforts. Unknown IDs and
// unqualified pairs return a *SelectionError with a fixed safe message.
func ValidateSelection(id, model, effort string) error {
	if q, ok := qualified(id); ok {
		msg := "the " + id + " model/effort selection is not qualified; supported: " + q.Pair()
		switch {
		case model != q.Model:
			return &SelectionError{Field: "model", msg: msg}
		case effort != q.Effort:
			return &SelectionError{Field: "effort", msg: msg}
		}
		return nil
	}
	if id == FakeID {
		switch {
		case !contract.ValidModel(model):
			return &SelectionError{Field: "model", msg: "adapter: invalid model"}
		case !slices.Contains(fakeEfforts, effort):
			return &SelectionError{Field: "effort", msg: "adapter: effort is not allowed for adapter fake; allowed: " + strings.Join(fakeEfforts, ", ")}
		}
		return nil
	}
	return &SelectionError{Field: "adapter", msg: "adapter: unknown adapter; registered adapters: " + registeredAdapters}
}

// vendor is a real worker adapter.
type vendor struct {
	q Qualification
	p *prober
}

// NewClaude returns the Claude adapter, whose probes run in dir with the
// process environment (minus the fixture variables).
func NewClaude(dir string) Adapter { return newVendor(ClaudeID, dir) }

// NewCodex returns the Codex adapter, likewise.
func NewCodex(dir string) Adapter { return newVendor(CodexID, dir) }

// NewGrok returns the Grok adapter (iteration 11), likewise.
func NewGrok(dir string) Adapter { return newVendor(GrokID, dir) }

// NewCursor returns the Cursor adapter (iteration 11), likewise: its probe
// and extractor work, but its Invocation always refuses.
func NewCursor(dir string) Adapter { return newVendor(CursorID, dir) }

func newVendor(id, dir string) *vendor {
	q, _ := qualified(id)
	v := &vendor{q: q}
	v.p = &prober{dir: dir, environ: os.Environ, clock: realClock{}, args: []string{VersionArg}, check: v.versionCheck}
	return v
}

func (v *vendor) Descriptor() Descriptor {
	return Descriptor{ID: v.q.ID, Efforts: []string{v.q.Effort}}
}

// Probe checks the explicit absolute executable exactly like the fake's
// probe (regular file, executable bit, no PATH lookup, ProbeTimeout,
// bounded captures, cancellation precedence) and runs "executable
// --version" in the state root: a zero exit whose stdout, after at most
// one trailing LF or CRLF, is exactly the qualified version. Bounded
// stderr is permitted and never exposed. It checks invocability, not
// authentication or containment. A matching Cursor version (iteration 11)
// is invocability only: the sidecar's posture check then refuses it.
func (v *vendor) Probe(ctx context.Context, executable string) error {
	return v.p.run(ctx, executable)
}

// versionCheck is the vendor's probe predicate.
func (v *vendor) versionCheck(out, _ string) string {
	switch {
	case strings.HasSuffix(out, "\r\n"):
		out = out[:len(out)-2]
	case strings.HasSuffix(out, "\n"):
		out = out[:len(out)-1]
	}
	if out == v.q.Version {
		return ""
	}
	if !v.looksLikeVendor(out) {
		return "is not the " + v.q.ID + " CLI (unexpected version output)"
	}
	return "has an unqualified " + v.q.ID + " version; expected " + v.q.Version
}

// cursorVersionShape is Cursor's lexical version shape (iteration 11): it
// recognizes 2026.10.01-e373342 without a vendor name; it is neither
// calendar validation nor vendor authentication.
var cursorVersionShape = regexp.MustCompile(`^[0-9]{4}\.[0-9]{2}\.[0-9]{2}-[0-9a-f]+$`)

// looksLikeVendor reports whether one-line output carries the vendor's
// version shape (so a wrong version and a wrong vendor are told apart).
// Every vendor has its own explicit case; none falls through to another's.
func (v *vendor) looksLikeVendor(out string) bool {
	if strings.ContainsAny(out, "\r\n") {
		return false
	}
	switch v.q.ID {
	case ClaudeID:
		return strings.HasSuffix(out, " (Claude Code)")
	case CodexID:
		return strings.HasPrefix(out, "codex-cli ")
	case GrokID:
		return strings.HasPrefix(out, "grok ")
	case CursorID:
		return cursorVersionShape.MatchString(out)
	}
	return false
}

// Invocation validates the task ID, the model grammar and the qualified
// selection, then builds the vendor's recipe through an explicit switch (a
// new ID never falls through to another vendor's): Claude and Codex the
// captured recipe with the prompt bound (and, for Codex, the scratch path)
// and an owned stdin copy, the prompt never in argv or a file; Grok
// (iteration 11) the measured dontAsk recipe with the complete composed
// prompt (at most MaxGrokPromptBytes, valid UTF-8, no NUL) as the one -p
// argument and an empty stdin; Cursor (iteration 11) always a zero
// Invocation and its fixed posture refusal, never a force/trust or
// sandbox-enabled argv. No shell, default, resume, sandbox or
// permission-bypass flag is ever added.
func (v *vendor) Invocation(in TaskInput) (Invocation, error) {
	switch {
	case !contract.ValidTaskID(in.TaskID):
		return Invocation{}, errInvocation("invalid task ID")
	case !contract.ValidModel(in.Model):
		return Invocation{}, errInvocation("invalid model")
	}
	if err := ValidateSelection(v.q.ID, in.Model, in.Effort); err != nil {
		return Invocation{}, err
	}
	switch v.q.ID {
	case CursorID:
		return Invocation{}, errCursorPosture
	case GrokID:
		return grokInvocation(in)
	}
	if len(in.Prompt) > contract.MaxPromptBytes {
		return Invocation{}, errInvocation("the prompt exceeds 16 MiB")
	}
	switch v.q.ID {
	case ClaudeID:
		return Invocation{Argv: []string{"-p", "--model", in.Model, "--effort", in.Effort,
			"--permission-prompts", "none", "--output-format", "json"}, Stdin: bytes.Clone(in.Prompt)}, nil
	case CodexID:
		return codexInvocation(in)
	}
	return Invocation{}, errUnsupportedPosture
}

// grokInvocation is Grok's measured recipe (iteration 11): the prompt
// checks in their fixed order, then exactly the captured arguments with the
// composed prompt as the last one, a new string (no trimming, newline,
// quoting, splitting or evaluation). Stdin is empty and the final source is
// stdout, so ScratchDir and FinalDir are ignored.
func grokInvocation(in TaskInput) (Invocation, error) {
	switch {
	case len(in.Prompt) == 0:
		return Invocation{}, errInvocation(grokPromptEmptyReason)
	case len(in.Prompt) > MaxGrokPromptBytes:
		return Invocation{}, errInvocation(grokPromptTooLargeReason)
	case !utf8.Valid(in.Prompt):
		return Invocation{}, errInvocation(grokPromptUTF8Reason)
	case bytes.IndexByte(in.Prompt, 0) >= 0:
		return Invocation{}, errInvocation(grokPromptNULReason)
	}
	return Invocation{Argv: []string{"--output-format", "json", "--model", in.Model,
		"--reasoning-effort", in.Effort, "--permission-mode", "dontAsk",
		"-p", string(in.Prompt)}}, nil
}

// codexInvocation is Codex's recipe (iteration 08): the final-message file
// in the task's scratch (or owned final-output) directory.
func codexInvocation(in TaskInput) (Invocation, error) {
	if in.ScratchDir == "" || !filepath.IsAbs(in.ScratchDir) || filepath.Clean(in.ScratchDir) != in.ScratchDir {
		return Invocation{}, errInvocation("codex needs the task's absolute clean scratch directory")
	}
	dir := in.ScratchDir
	if in.FinalDir != "" {
		// Iteration 10b: a workspace task's owned final-output directory.
		if !filepath.IsAbs(in.FinalDir) || filepath.Clean(in.FinalDir) != in.FinalDir {
			return Invocation{}, errInvocation("codex needs the task's absolute clean final-output directory")
		}
		dir = in.FinalDir
	}
	final := filepath.Join(dir, CodexFinalName)
	return Invocation{Argv: []string{"-a", "never", "exec", "--model", in.Model,
		"-c", `model_reasoning_effort="` + in.Effort + `"`, "--json",
		"--skip-git-repo-check", "--output-last-message", final, "-"}, Stdin: bytes.Clone(in.Prompt), FinalFile: final}, nil
}

// NewFinalExtractor returns fresh task-local extraction state: Claude's
// stdout JSON result, Codex's raw UTF-8 final file, Grok's stdout JSON text
// or error message (iteration 11) and Cursor's stdout JSON result
// (iteration 11: the same strict result schema as Claude's, callable for
// fixture handling although Cursor's execution is refused).
func (v *vendor) NewFinalExtractor() FinalExtractor {
	switch v.q.ID {
	case ClaudeID, CursorID:
		return &claudeExtractor{}
	case GrokID:
		return &grokExtractor{}
	}
	return &rawExtractor{}
}
