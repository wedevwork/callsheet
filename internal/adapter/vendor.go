package adapter

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"unicode/utf8"

	"github.com/wedevwork/callsheet/internal/contract"
)

// The production worker adapters (iteration 08): Claude and Codex. Each is
// enabled only by an explicit absolute executable path on the worker and
// runs the owner's captured Linux recipe with the prompt on stdin. Neither
// adapter discovers executables, resolves aliases, substitutes operator
// defaults or starts a process outside Probe.
//
// Selection and versions (design 12a-worker-selection, requirement Q12):
// a role or task names an explicit free-text model (contract.ValidModel,
// passed through unchanged) and an effort from the adapter's static union
// (vendorEfforts); whether the vendor can run that pair is the vendor's
// decision at run time, reported through the existing task-result paths.
// A worker executable is eligible at or above its observed minimum
// version (vendor_version.go), without a version-drift warning. The
// captured recipes and their observed model/effort pairs and versions
// (Qualifications) remain evidence, not an allowlist or a default.
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
// configuration and grants no override: a valid selection or an eligible
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

// Qualification is one vendor's captured observation, as recorded on Linux
// on 2026-09-30 (Claude, Codex) and 2026-10-04 (Grok, Cursor): the exact
// historical version output (after one trailing LF or CRLF is removed),
// which is also the vendor's minimum version baseline, and the observed
// model/effort pair of that capture. The observed pair is evidence only:
// it is not a selection allowlist, the only qualified selection or a
// default (no registration or dispatch path obtains one from it). It is
// separate from execution eligibility (ValidateWorkerPosture): Cursor's
// row is an observation whose execution is refused on every OS, Grok's is
// eligible on Linux only.
type Qualification struct {
	ID      string
	Version string
	Model   string
	Effort  string
}

// qualifications is the immutable table of observations. Its rows are
// historical evidence and never change to follow current selection policy.
//
// Cursor's observed pair (grok-4.7, low) was run as the literal vendor
// model argument grok-4.7-low; Q12's listed (grok-4.7, xhigh) maps to
// grok-4.7-xhigh likewise (and medium/high). These are evidence and
// examples, not a model allowlist and not an executable mapper: Cursor's
// Invocation refuses every valid selection. The observed but rejected
// Cursor candidate was -p --output-format json --model grok-4.7-low
// --force --trust with the prompt on stdin: it is recorded in the fixtures
// and catalog for comparison, never emitted.
var qualifications = []Qualification{
	{ID: ClaudeID, Version: "2.1.285 (Claude Code)", Model: "sonnet", Effort: "low"},
	{ID: CodexID, Version: "codex-cli 0.159.0", Model: "gpt-6.1-sol", Effort: "low"},
	{ID: CursorID, Version: "2026.10.01-e373342", Model: "grok-4.7", Effort: "low"},
	{ID: GrokID, Version: "grok 1.0.46 (2765805b9442) [stable]", Model: "grok-4.7", Effort: "low"},
}

// Qualifications returns an independent copy of the observation table,
// sorted by ID. Presence in this table is evidence, not a ready worker or
// a selection policy: consumers must not equate it with execution
// eligibility (ValidateWorkerPosture and the sidecar's role checks) nor
// take a default model or effort from it.
func Qualifications() []Qualification { return slices.Clone(qualifications) }

// qualified returns the observation of a real vendor.
func qualified(id string) (Qualification, bool) {
	for _, q := range qualifications {
		if q.ID == id {
			return q, true
		}
	}
	return Qualification{}, false
}

// Pair renders the observed pair of q: model MODEL, effort EFFORT. It is
// the pair the capture ran, not the only selection the adapter accepts.
func (q Qualification) Pair() string { return "model " + q.Model + ", effort " + q.Effort }

// vendorEfforts is each real vendor's static effort union, in its stable
// display order, case-sensitive (design 12a-worker-selection, Selection
// policy): claude from its captured --effort enumeration
// (tests/testdata/cli-help/claude-help.txt), codex from the coordinator's
// dated observation and the owner's decision of 2026-10-10, grok from the
// captured accepted-effort list (linux-2026-10-04 NOTES.md), and cursor
// from its listed None, Minimal, Low, Medium, High, Extra High (xhigh) and
// Max model variants (cursor-models.txt). It is the single definition
// behind ValidateSelection and each vendor's Descriptor; nothing here is a
// per-model compatibility table.
var vendorEfforts = map[string][]string{
	ClaudeID: {"low", "medium", "high", "xhigh", "max"},
	CodexID:  {"low", "medium", "high", "xhigh", "max", "ultra"},
	GrokID:   {"low", "medium", "high", "xhigh"},
	CursorID: {"none", "minimal", "low", "medium", "high", "xhigh", "max"},
}

// SelectionError is a refused model/effort selection: a fixed safe
// message built only from static metadata, and the offending field
// (adapter, model or effort). It never reflects the submitted values.
type SelectionError struct {
	Field string
	msg   string
}

func (e *SelectionError) Error() string { return e.msg }

// registeredAdapters is the unknown-adapter diagnostic's list.
const registeredAdapters = "claude, codex, cursor, fake, grok"

// ValidateSelection checks adapter id's input validation of model and
// effort, with no I/O, in order: the adapter must be registered, the model
// must satisfy contract.ValidModel (never trimmed, case-folded, normalized
// or interpreted) and the effort must be one of the adapter's efforts (a
// real vendor's union, the fake's three). A syntactically valid unknown
// model or an unobserved model/effort combination passes without a
// warning: the vendor owns compatibility. A refusal is a *SelectionError
// with a fixed safe message.
func ValidateSelection(id, model, effort string) error {
	efforts, ok := vendorEfforts[id]
	switch {
	case id == FakeID:
		efforts = fakeEfforts
	case !ok:
		return &SelectionError{Field: "adapter", msg: "adapter: unknown adapter; registered adapters: " + registeredAdapters}
	}
	switch {
	case !contract.ValidModel(model):
		return &SelectionError{Field: "model", msg: "adapter: invalid model"}
	case !slices.Contains(efforts, effort):
		return &SelectionError{Field: "effort", msg: "adapter: effort is not allowed for adapter " + id + "; allowed: " + strings.Join(efforts, ", ")}
	}
	return nil
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

// Descriptor is the vendor's metadata with a fresh copy of its effort
// union on every call.
func (v *vendor) Descriptor() Descriptor {
	return Descriptor{ID: v.q.ID, Efforts: slices.Clone(vendorEfforts[v.q.ID])}
}

// Probe checks the explicit absolute executable exactly like the fake's
// probe (regular file, executable bit, no PATH lookup, ProbeTimeout,
// bounded captures, cancellation precedence) and runs "executable
// --version" in the state root: a zero exit whose stdout, after at most
// one trailing LF or CRLF, is a complete orderable version of this vendor
// at or above its observed minimum (checkWorkerVersion). Bounded stderr is
// permitted and never exposed or used as a version. It checks
// invocability, not authentication, model availability or containment. An
// eligible Cursor version (iteration 11) is invocability only: the
// sidecar's posture check then refuses it.
func (v *vendor) Probe(ctx context.Context, executable string) error {
	return v.p.run(ctx, executable)
}

// versionCheck is the vendor's probe predicate: the minimum-version policy
// with the observed version as the baseline. Every vendor has its own
// grammar; none falls through to another's.
func (v *vendor) versionCheck(out, _ string) string {
	return checkWorkerVersion(v.q.ID, v.q.Version, out)
}

// Invocation validates the task ID, the model grammar and the selection
// (ValidateSelection: the adapter's effort union), then builds the
// vendor's recipe through an explicit switch (a new ID never falls through
// to another vendor's) with the selected model and effort passed
// explicitly, each one argv element whatever its spaces, punctuation or
// leading hyphens: Claude and Codex the captured recipe with the prompt
// bound (and, for Codex, the scratch path) and an owned stdin copy, the
// prompt never in argv or a file; Grok (iteration 11) the measured dontAsk
// recipe with the complete composed prompt (at most MaxGrokPromptBytes,
// valid UTF-8, no NUL) as the one -p argument and an empty stdin; Cursor
// (iteration 11) always a zero Invocation and its fixed posture refusal for
// every valid selection, never a force/trust or sandbox-enabled argv and
// no model-name mapping. No shell, default, fallback selection, resume,
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
// selected model and effort and the composed prompt as the last one, a new
// string (no trimming, newline, quoting, splitting or evaluation). Stdin is
// empty and the final source is stdout, so ScratchDir and FinalDir are
// ignored. Grok's fast variant is the model grok-4.7-build-fast (listed in
// the captured grok-models.txt) with a separate union effort, for example
// low: a requested selection, not a product default or a captured run.
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
