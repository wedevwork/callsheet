package mcpqual

import (
	"errors"
	"fmt"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"
)

// Plan limits (design 07b, Measurement phases): the defaults may be lowered
// freely; raising one requires explicit_increase, and never past the hard
// caps.
const (
	DefaultMaxSessions = 12
	DefaultMaxCaseMS   = 15 * 60 * 1000
	DefaultMaxClientMS = 90 * 60 * 1000
	hardMaxSessions    = 64
	hardMaxCaseMS      = MaxDelayMS
	hardMaxClientMS    = 24 * 60 * 60 * 1000
	// MaxPlanBytes bounds a plan file.
	MaxPlanBytes = 1 << 20
	maxArgs      = 64
	maxArgBytes  = 16 << 10
)

// DefaultDelaysMS are the silent default-timeout probe delays used when the
// plan names none (5 s, 15 s, 60 s, 180 s, 600 s).
var DefaultDelaysMS = []int64{5000, 15000, 60000, 180000, 600000}

// Client IDs and their vendor-native decoders.
var clientDecoders = map[string]string{"claude": "claude-json", "codex": "codex-jsonl", "grok": "grok-json", "cursor": "cursor-jsonl"}

// Drivers.
const (
	DriverModel  = "model"  // a model session; needs --allow-model-calls
	DriverDirect = "direct" // a genuinely model-free vendor tool-call driver
)

// Plan is the version-1 qualification plan.
type Plan struct {
	Version int          `json:"version"`
	Limits  *Limits      `json:"limits,omitempty"`
	Clients []PlanClient `json:"clients"`
}

// Limits bound the run.
type Limits struct {
	MaxSessionsPerClient int   `json:"max_sessions_per_client"`
	MaxCaseMS            int64 `json:"max_case_ms"`
	MaxClientMS          int64 `json:"max_client_ms"`
	ExplicitIncrease     bool  `json:"explicit_increase,omitempty"`
}

// PlanClient is one CLI's qualification recipe.
type PlanClient struct {
	ID              string            `json:"id"`
	Executable      string            `json:"executable"`
	ExpectedVersion string            `json:"expected_version"`
	VersionArgv     []string          `json:"version_argv"`
	HelpArgv        []string          `json:"help_argv,omitempty"`
	Driver          string            `json:"driver"`
	Model           string            `json:"model,omitempty"`
	Effort          string            `json:"effort,omitempty"`
	Decoder         string            `json:"decoder"`
	DecoderFixture  string            `json:"decoder_fixture"`
	Session         Recipe            `json:"session"`
	Config          ConfigRecipes     `json:"config"`
	Override        *Override         `json:"override,omitempty"`
	Phases          Phases            `json:"phases"`
	Env             map[string]string `json:"env,omitempty"`
}

// Recipe is an argv-array session recipe (after the executable) with
// placeholders {prompt}, {workspace}, {config}, {server} and {case}. No
// shell ever interprets it.
type Recipe struct {
	Argv []string `json:"argv"`
}

// ConfigRecipes are the default and (optional) raised-timeout
// configurations, written into the disposable case workspace.
type ConfigRecipes struct {
	Default ConfigRecipe  `json:"default"`
	Raised  *ConfigRecipe `json:"raised,omitempty"`
}

// ConfigRecipe is one probe-only MCP configuration: a workspace-relative
// file rendered from a template (placeholders {server}, {case_file},
// {events} and {workspace}, substituted JSON-string-escaped), plus extra
// session argv and environment. Prerequisite names an owner-prepared
// registration when the vendor has no isolated config mechanism.
type ConfigRecipe struct {
	Path         string            `json:"path"`
	Content      string            `json:"content"`
	Argv         []string          `json:"argv,omitempty"`
	Env          map[string]string `json:"env,omitempty"`
	Prerequisite string            `json:"prerequisite,omitempty"`
}

// Override names the raised-timeout setting candidate and its evidence.
type Override struct {
	Setting     string   `json:"setting"`
	Value       string   `json:"value"`
	Explanation string   `json:"explanation"`
	Evidence    []string `json:"evidence"`
}

// Phases lists the requested measurement phases; setup always runs.
type Phases struct {
	Default  *DefaultPhase `json:"default,omitempty"`
	Override *DelayPhase   `json:"override,omitempty"`
	Progress *struct{}     `json:"progress,omitempty"`
	Absolute *DelayPhase   `json:"absolute,omitempty"`
}

// DefaultPhase lists ascending silent probe delays (empty: DefaultDelaysMS).
type DefaultPhase struct {
	DelaysMS []int64 `json:"delays_ms,omitempty"`
}

// DelayPhase is a phase with one planned delay and, for override, an
// optional longer bound-exposing delay.
type DelayPhase struct {
	DelayMS      int64 `json:"delay_ms"`
	BoundDelayMS int64 `json:"bound_delay_ms,omitempty"`
}

var (
	placeholderRe = regexp.MustCompile(`\{[a-z_]+\}`)
	templateRe    = regexp.MustCompile(`<[a-z][a-z0-9-]*>`)
	envNameRe     = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]{0,127}$`)
)

var (
	sessionPlaceholders = map[string]bool{"{prompt}": true, "{workspace}": true, "{config}": true, "{server}": true, "{case}": true}
	configPlaceholders  = map[string]bool{"{server}": true, "{case_file}": true, "{events}": true, "{workspace}": true}
)

// Metadata commands run before the --allow-model-calls gate, so they are
// never free-form plan data: version_argv and help_argv must each be one of
// these fixed, model-free help or version invocations (the shapes the
// captured evidence under tests/testdata/cli-help used).
var (
	allowedVersionArgv = [][]string{{"--version"}}
	allowedHelpArgv    = [][]string{{"--help"}, {"exec", "--help"}, {"mcp", "--help"}, {"mcp", "add", "--help"}, {"agent", "--help"}}
)

// MetadataAllowed reports whether argv is one of the allowed metadata
// commands.
func MetadataAllowed(argv []string, allowed [][]string) bool {
	for _, a := range allowed {
		if slices.Equal(argv, a) {
			return true
		}
	}
	return false
}

// ErrTemplate reports an example plan whose owner placeholders (such as
// <model>) are not yet completed.
var ErrTemplate = errors.New("plan: unfilled owner placeholder")

// planCheck selects the plan validation: qualification checks the
// decoder fixture against a registry; capture (design decoder-enrollment)
// keeps the decoder name and a nonempty fixture as inert metadata and
// adds its own setup-only rules; a template accepts unfilled owner
// placeholders.
type planCheck struct {
	reg      Registry
	template bool
	capture  bool
}

// ParsePlan strictly decodes and validates a plan.
func ParsePlan(b []byte, reg Registry) (*Plan, error) {
	return parsePlan(b, planCheck{reg: reg})
}

// ParseCapturePlan strictly decodes and validates a version-1 plan for
// "mcpqual capture" (design decoder-enrollment, Capture interface): every
// structural check of ParsePlan (paths, argv, environment, placeholders,
// client IDs, sizes and budgets), the correct decoder family name and a
// nonempty decoder_fixture kept as inert metadata (no registry is
// consulted), and a model-driven setup only: driver model with its
// explicit model, a nonempty allowed help_argv, no raised configuration,
// override or override/progress/absolute phase, and either no phases or
// exactly the short plans' default delays_ms [15000], which capture never
// executes.
func ParseCapturePlan(b []byte) (*Plan, error) {
	return parsePlan(b, planCheck{capture: true})
}

func parsePlan(b []byte, chk planCheck) (*Plan, error) {
	if len(b) > MaxPlanBytes {
		return nil, fmt.Errorf("plan: %d bytes exceeds %d", len(b), MaxPlanBytes)
	}
	var p Plan
	if err := decodeStrict(b, &p); err != nil {
		return nil, fmt.Errorf("plan: %w", err)
	}
	if err := p.validate(chk); err != nil {
		return nil, err
	}
	return &p, nil
}

// ValidateTemplate checks an example plan's schema and templates, accepting
// unfilled owner placeholders.
func ValidateTemplate(b []byte, reg Registry) (*Plan, error) {
	var p Plan
	if err := decodeStrict(b, &p); err != nil {
		return nil, fmt.Errorf("plan: %w", err)
	}
	return &p, p.validate(planCheck{reg: reg, template: true})
}

// CaptureDefaultDelaysMS is the only default phase a capture plan may
// carry (the short plans'); capture ignores it.
var CaptureDefaultDelaysMS = []int64{15000}

// HasIgnoredDefault reports whether capture plan p names the short plans'
// default phase, which capture never executes.
func (p *Plan) HasIgnoredDefault() bool {
	for _, c := range p.Clients {
		if c.Phases.Default != nil {
			return true
		}
	}
	return false
}

// validateCapture is the capture-only client rule set.
func (c *PlanClient) validateCapture() error {
	ph := c.Phases
	switch {
	case c.Driver != DriverModel:
		return fmt.Errorf("capture needs driver %q (one model setup session), not %q", DriverModel, c.Driver)
	case len(c.HelpArgv) == 0:
		return errors.New("capture needs a help_argv (its help output is part of the capture)")
	case c.Config.Raised != nil || c.Override != nil:
		return errors.New("capture takes no raised configuration or override (setup only)")
	case ph.Override != nil || ph.Progress != nil || ph.Absolute != nil:
		return errors.New("capture takes no override, progress or absolute phase (setup only)")
	case ph.Default != nil && !slices.Equal(ph.Default.DelaysMS, CaptureDefaultDelaysMS):
		return fmt.Errorf("capture accepts no phases or exactly phases.default.delays_ms %v (never executed), not %v", CaptureDefaultDelaysMS, ph.Default.DelaysMS)
	}
	return nil
}

// EffectiveLimits returns the limits with defaults applied.
func (p *Plan) EffectiveLimits() Limits {
	l := Limits{MaxSessionsPerClient: DefaultMaxSessions, MaxCaseMS: DefaultMaxCaseMS, MaxClientMS: DefaultMaxClientMS}
	if p.Limits != nil {
		l = *p.Limits
	}
	return l
}

func (p *Plan) validate(chk planCheck) error {
	fail := func(format string, a ...any) error { return fmt.Errorf("plan: "+format, a...) }
	if p.Version != 1 {
		return fail("version %d, want 1", p.Version)
	}
	if p.Limits != nil {
		l := *p.Limits
		switch {
		case l.MaxSessionsPerClient < 1 || l.MaxSessionsPerClient > hardMaxSessions:
			return fail("limits.max_sessions_per_client %d outside 1-%d", l.MaxSessionsPerClient, hardMaxSessions)
		case l.MaxCaseMS < 1 || l.MaxCaseMS > hardMaxCaseMS:
			return fail("limits.max_case_ms %d outside 1-%d", l.MaxCaseMS, hardMaxCaseMS)
		case l.MaxClientMS < l.MaxCaseMS || l.MaxClientMS > hardMaxClientMS:
			return fail("limits.max_client_ms %d outside max_case_ms-%d", l.MaxClientMS, hardMaxClientMS)
		case !l.ExplicitIncrease && (l.MaxSessionsPerClient > DefaultMaxSessions || l.MaxCaseMS > DefaultMaxCaseMS || l.MaxClientMS > DefaultMaxClientMS):
			return fail("limits above the defaults (12 sessions, 15 min per case, 90 min per client) need explicit_increase")
		}
	}
	if len(p.Clients) == 0 || len(p.Clients) > len(clientDecoders) {
		return fail("%d clients, want 1-%d", len(p.Clients), len(clientDecoders))
	}
	seen := map[string]bool{}
	for i := range p.Clients {
		c := &p.Clients[i]
		if seen[c.ID] {
			return fail("duplicate client id %q", c.ID)
		}
		seen[c.ID] = true
		if err := c.validate(chk, p.EffectiveLimits()); err != nil {
			return fmt.Errorf("plan: client %q: %w", c.ID, err)
		}
	}
	return nil
}

func (c *PlanClient) validate(chk planCheck, lim Limits) error {
	template := chk.template
	want, ok := clientDecoders[c.ID]
	if !ok {
		return errors.New("unknown id (want claude, codex, grok or cursor)")
	}
	filled := func(field, s string) error { return checkFilled(template, field, s) }
	if err := filled("executable", c.Executable); err != nil {
		return err
	}
	if !templateRe.MatchString(c.Executable) && (!filepath.IsAbs(c.Executable) || filepath.Clean(c.Executable) != c.Executable) {
		return fmt.Errorf("executable %q must be an absolute clean path (no PATH lookup)", c.Executable)
	}
	if strings.TrimSpace(c.ExpectedVersion) == "" {
		return errors.New("expected_version is required")
	}
	// The exact version the owner's CLI reports is an owner input of a
	// template (the short plans), never filled in for them.
	if err := filled("expected_version", c.ExpectedVersion); err != nil {
		return err
	}
	if !MetadataAllowed(c.VersionArgv, allowedVersionArgv) {
		return fmt.Errorf("version_argv %q is not an allowed metadata command (it runs before the --allow-model-calls gate): use %q", c.VersionArgv, allowedVersionArgv)
	}
	if len(c.HelpArgv) > 0 && !MetadataAllowed(c.HelpArgv, allowedHelpArgv) {
		return fmt.Errorf("help_argv %q is not an allowed metadata command (it runs before the --allow-model-calls gate): use one of %q", c.HelpArgv, allowedHelpArgv)
	}
	switch c.Driver {
	case DriverModel:
		if strings.TrimSpace(c.Model) == "" {
			return errors.New("a model driver needs an explicit model")
		}
		if err := filled("model", c.Model); err != nil {
			return err
		}
		if !contains(c.Session.Argv, c.Model) && !template {
			return fmt.Errorf("the explicit model %q must appear in session.argv", c.Model)
		}
	case DriverDirect:
		if c.Model != "" || c.Effort != "" {
			return errors.New("a direct driver takes no model or effort")
		}
	default:
		return fmt.Errorf("driver %q, want model or direct", c.Driver)
	}
	if c.Decoder != want {
		return fmt.Errorf("decoder %q, want %s for this client", c.Decoder, want)
	}
	switch {
	case chk.capture && strings.TrimSpace(c.DecoderFixture) == "":
		return errors.New("decoder_fixture is required (inert metadata for capture)")
	case !chk.capture && !chk.reg.hasFixture(c.Decoder, c.DecoderFixture):
		return fmt.Errorf("decoder_fixture %q is not a tested %s fixture", c.DecoderFixture, c.Decoder)
	}
	for _, list := range []struct {
		name string
		argv []string
		ph   map[string]bool
	}{{"version_argv", c.VersionArgv, nil}, {"help_argv", c.HelpArgv, nil}, {"session.argv", c.Session.Argv, sessionPlaceholders},
		{"config.default.argv", c.Config.Default.Argv, sessionPlaceholders}} {
		if err := checkArgv(list.name, list.argv, list.ph); err != nil {
			return err
		}
		for _, a := range list.argv {
			if err := filled(list.name, a); err != nil {
				return err
			}
		}
	}
	if len(c.Session.Argv) == 0 || !containsPlaceholder(c.Session.Argv, "{case}") && !containsPlaceholder(c.Session.Argv, "{prompt}") {
		return errors.New("session.argv must carry {prompt} or {case}")
	}
	if err := checkEnv("env", c.Env, template); err != nil {
		return err
	}
	if err := c.Config.Default.validate("config.default", template); err != nil {
		return err
	}
	if c.Config.Raised != nil {
		if err := c.Config.Raised.validate("config.raised", template); err != nil {
			return err
		}
		if err := checkArgv("config.raised.argv", c.Config.Raised.Argv, sessionPlaceholders); err != nil {
			return err
		}
	}
	if o := c.Override; o != nil {
		if strings.TrimSpace(o.Setting) == "" || strings.TrimSpace(o.Value) == "" || strings.TrimSpace(o.Explanation) == "" {
			return errors.New("override needs setting, value and explanation")
		}
		if err := filled("override.value", o.Value); err != nil {
			return err
		}
		for _, ev := range o.Evidence {
			if err := checkRelPath("override.evidence", ev); err != nil {
				return err
			}
		}
	}
	ph := c.Phases
	if d := ph.Default; d != nil {
		prev := int64(0)
		for _, v := range d.DelaysMS {
			if v <= prev || v > MaxDelayMS {
				return fmt.Errorf("phases.default.delays_ms must be strictly ascending, positive and at most %d", MaxDelayMS)
			}
			prev = v
		}
	}
	if ph.Override != nil {
		if ph.Default == nil {
			return errors.New("phases.override needs phases.default (a raised call must outlast an observed default)")
		}
		if c.Config.Raised == nil || c.Override == nil {
			return errors.New("phases.override needs config.raised and override")
		}
		if err := checkDelay("phases.override", ph.Override, lim); err != nil {
			return err
		}
		if b := ph.Override.BoundDelayMS; b != 0 && (b <= ph.Override.DelayMS || b > lim.MaxCaseMS) {
			return errors.New("phases.override.bound_delay_ms must exceed delay_ms and fit max_case_ms")
		}
	}
	if ph.Progress != nil && ph.Default == nil {
		return errors.New("phases.progress needs phases.default (it repeats a known failing silent case)")
	}
	if ph.Absolute != nil {
		if ph.Progress == nil {
			return errors.New("phases.absolute needs phases.progress")
		}
		if ph.Absolute.BoundDelayMS != 0 {
			return errors.New("phases.absolute takes no bound_delay_ms")
		}
		if err := checkDelay("phases.absolute", ph.Absolute, lim); err != nil {
			return err
		}
	}
	if chk.capture {
		return c.validateCapture()
	}
	return nil
}

func checkDelay(name string, d *DelayPhase, lim Limits) error {
	if d.DelayMS <= 0 || d.DelayMS > lim.MaxCaseMS {
		return fmt.Errorf("%s.delay_ms %d must be positive and fit max_case_ms %d", name, d.DelayMS, lim.MaxCaseMS)
	}
	return nil
}

// checkFilled rejects an unfilled owner placeholder such as <model>
// unless an example plan template is being checked.
func checkFilled(template bool, field, s string) error {
	if !template && templateRe.MatchString(s) {
		return fmt.Errorf("%w in %s: %q", ErrTemplate, field, templateRe.FindString(s))
	}
	return nil
}

func (r ConfigRecipe) validate(name string, template bool) error {
	if err := checkRelPath(name+".path", r.Path); err != nil {
		return err
	}
	if err := checkFilled(template, name+".content", r.Content); err != nil {
		return err
	}
	for _, a := range r.Argv {
		if err := checkFilled(template, name+".argv", a); err != nil {
			return err
		}
	}
	if len(r.Content) > MaxCaseFileBytes {
		return fmt.Errorf("%s.content exceeds %d bytes", name, MaxCaseFileBytes)
	}
	for _, ph := range placeholderRe.FindAllString(r.Content, -1) {
		if !configPlaceholders[ph] {
			return fmt.Errorf("%s.content has unknown placeholder %s", name, ph)
		}
	}
	if err := checkArgv(name+".argv", r.Argv, sessionPlaceholders); err != nil {
		return err
	}
	return checkEnv(name+".env", r.Env, template)
}

func checkArgv(name string, argv []string, allowed map[string]bool) error {
	if len(argv) > maxArgs {
		return fmt.Errorf("%s has %d arguments, at most %d", name, len(argv), maxArgs)
	}
	for _, a := range argv {
		if len(a) > maxArgBytes || strings.ContainsRune(a, 0) {
			return fmt.Errorf("%s has an argument over %d bytes or with NUL", name, maxArgBytes)
		}
		for _, ph := range placeholderRe.FindAllString(a, -1) {
			if !allowed[ph] {
				return fmt.Errorf("%s has unknown placeholder %s", name, ph)
			}
		}
	}
	return nil
}

func checkEnv(name string, env map[string]string, template bool) error {
	keys := make([]string, 0, len(env))
	for k := range env {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		if !envNameRe.MatchString(k) || k == "CI" || len(env[k]) > maxArgBytes || strings.ContainsRune(env[k], 0) {
			return fmt.Errorf("%s: invalid variable %q (CI can never be set by a plan)", name, k)
		}
		if err := checkFilled(template, name+"."+k, env[k]); err != nil {
			return err
		}
	}
	return nil
}

// checkRelPath accepts a clean, relative, forward-slash path that stays
// inside its base directory.
func checkRelPath(name, p string) error {
	switch {
	case p == "":
		return fmt.Errorf("%s is empty", name)
	case strings.HasPrefix(p, "/") || filepath.IsAbs(p) || strings.Contains(p, `\`):
		return fmt.Errorf("%s %q must be relative with forward slashes", name, p)
	case path.Clean(p) != p || p == "." || p == ".." || strings.HasPrefix(p, "../"):
		return fmt.Errorf("%s %q escapes its directory or is not clean", name, p)
	}
	return nil
}

func contains(xs []string, s string) bool {
	for _, x := range xs {
		if x == s {
			return true
		}
	}
	return false
}

func containsPlaceholder(xs []string, ph string) bool {
	for _, x := range xs {
		if strings.Contains(x, ph) {
			return true
		}
	}
	return false
}
