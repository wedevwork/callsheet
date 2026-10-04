package adapter

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/wedevwork/callsheet/internal/contract"
)

// The production worker adapters (iteration 08): Claude and Codex. Each is
// enabled only by an explicit absolute executable path on the worker, runs
// the owner's captured Linux recipe with the prompt on stdin, and accepts
// only its qualified version and model/effort pair. Neither adapter
// discovers executables, resolves aliases, substitutes operator defaults
// or starts a process outside Probe.

// Adapter IDs.
const (
	ClaudeID = "claude"
	CodexID  = "codex"
)

// CodexFinalName is the fixed basename of Codex's final-message file in
// the task's private scratch directory: private to each newly created
// directory, never derived from task or model text.
const CodexFinalName = "callsheet-final.txt"

// VersionArg is the vendors' probe argument.
const VersionArg = "--version"

// Qualification is one vendor's qualified recipe inputs: the exact
// version output (after one trailing LF or CRLF is removed) and the single
// accepted model/effort pair, as captured on Linux on 2026-09-30.
type Qualification struct {
	ID      string
	Version string
	Model   string
	Effort  string
}

// qualifications is the immutable qualification table. Further versions
// or pairs need evidence and a table and catalog change; there is no
// allow-unknown switch.
var qualifications = []Qualification{
	{ID: ClaudeID, Version: "2.1.285 (Claude Code)", Model: "sonnet", Effort: "low"},
	{ID: CodexID, Version: "codex-cli 0.159.0", Model: "gpt-6.1-sol", Effort: "low"},
}

// Qualifications returns a copy of the qualification table, sorted by ID.
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
const registeredAdapters = "claude, codex, fake"

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
// authentication or containment.
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

// looksLikeVendor reports whether one-line output carries the vendor's
// version shape (so a wrong version and a wrong vendor are told apart).
func (v *vendor) looksLikeVendor(out string) bool {
	if strings.ContainsAny(out, "\r\n") {
		return false
	}
	switch v.q.ID {
	case ClaudeID:
		return strings.HasSuffix(out, " (Claude Code)")
	default:
		return strings.HasPrefix(out, "codex-cli ")
	}
}

// Invocation validates the task ID, the model grammar, the qualified
// selection and the prompt bound (and, for Codex, the scratch path), then
// returns a new argv and an owned stdin copy: the captured recipe with the
// prompt on stdin, never in argv or a file. No shell, default, resume,
// sandbox or permission-bypass flag is ever added.
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
	if len(in.Prompt) > contract.MaxPromptBytes {
		return Invocation{}, errInvocation("the prompt exceeds 16 MiB")
	}
	if v.q.ID == ClaudeID {
		return Invocation{Argv: []string{"-p", "--model", in.Model, "--effort", in.Effort,
			"--permission-prompts", "none", "--output-format", "json"}, Stdin: bytes.Clone(in.Prompt)}, nil
	}
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
// stdout JSON result, Codex's raw UTF-8 final file.
func (v *vendor) NewFinalExtractor() FinalExtractor {
	if v.q.ID == ClaudeID {
		return &claudeExtractor{}
	}
	return &rawExtractor{}
}
