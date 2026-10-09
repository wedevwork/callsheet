package mcpqual

// The measurement scheduler (FP-13): one scripted session per case, the
// five phases in order, stopping as partial on the budget, on
// authentication or approval failures, on cleanup failure and on
// interruption. Brackets use planned delays and probe-clock intervals; a
// timeout needs the vendor's typed, case-correlated timeout event.

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/wedevwork/callsheet/internal/contract"
)

// Phase results.
const (
	ResultHealthy           = "healthy"
	ResultTimeoutObserved   = "timeout_observed"
	ResultLowerBound        = "lower_bound"
	ResultOverrideEffective = "override_effective"
	ResultOverrideBound     = "override_effective_bound_observed"
	ResultOverrideNone      = "override_not_effective"
	ResultExtends           = "progress_extends"
	ResultNoExtension       = "progress_does_not_extend"
	ResultCapObserved       = "absolute_cap_observed"
	ResultNoCapObserved     = "no_absolute_cap_observed"
	ResultNotApplicable     = "not_applicable"
)

type measure struct {
	r          *Runner
	pc         *PlanClient
	cr         *ClientReport
	decode     func(Transcript) Decoded
	dv         DecoderVersion // the selected exact version and its evidence
	start      time.Time
	deadline   time.Time // start + max_client_ms; every launch is capped by it
	sessions   int
	stopped    string
	lastEvents []ProbeEvent // the last case's parsed probe events

	setupOK     bool
	lastSuccess int64
	silentDelay int64  // the known failing silent case's delay
	silentUpper *int64 // its probe interval
	interval    int64
	progress    string
	capUpper    *int64 // an absolute cap already observed in the progress case

	// pinned marks the pinned Cursor version (design decoder-enrollment B3,
	// FP-24); prep is its per-case preparation, set only for the trusted
	// recipe of the linux/amd64 adapter.
	pinned bool
	prep   *cursorPreparer
}

func (m *measure) run(ctx context.Context) {
	for _, name := range requestedPhases(m.pc) {
		var ph PhaseReport
		switch name {
		case PhaseSetup:
			ph = m.setup(ctx)
		case PhaseDefault:
			ph = m.defaults(ctx)
		case PhaseOverride:
			ph = m.override(ctx)
		case PhaseProgress:
			ph = m.progressPhase(ctx)
		case PhaseAbsolute:
			ph = m.absolute(ctx)
		}
		ph.Name = name
		if ph.Cases == nil {
			ph.Cases = []CaseReport{}
		}
		m.cr.Phases = append(m.cr.Phases, ph)
	}
	m.cr.Outcome = StatusConclusive
	ran := false
	for _, ph := range m.cr.Phases {
		ran = ran || len(ph.Cases) > 0
		if ph.Status != StatusConclusive && m.cr.Reason == nil {
			m.cr.Outcome = "partial"
			m.cr.Reason = sptr(ph.Name + ": " + *ph.Reason)
		}
	}
	if !ran {
		m.cr.Outcome = "unqualified"
	}
}

// checkCapability (design decoder-enrollment, Enrollment contract): a
// qualified decoder's outcome counts only when its evidence demonstrates
// the outcome's capabilities on the run's platform (Runner.GOOS/GOARCH,
// from Env, never the runtime's); otherwise the case is inconclusive with
// unverified_decoder_event. Synthetic (unqualified) classification is
// unchanged and can never qualify a fact.
func (m *measure) checkCapability(cs *CaseReport) {
	if !m.dv.Qualified {
		return
	}
	platform := m.r.GOOS + "/" + m.r.GOARCH
	if missing := m.dv.MissingCapabilities(platform, RequiredCapabilities(cs.Outcome)...); len(missing) > 0 {
		cs.Reason = sptr(fmt.Sprintf("%s: %s without %s evidence for %s", ReasonUnverifiedEvent, cs.Outcome, strings.Join(missing, ", "), platform))
		cs.Outcome = OutcomeInconclusive
	}
}

// canRun is the budget and stop check before a session.
func (m *measure) canRun(ctx context.Context, delay int64) string {
	lim := m.r.Plan.EffectiveLimits()
	switch {
	case ctx.Err() != nil:
		return ReasonInterrupted
	case !m.r.cleanupOK():
		return ReasonCleanupFailed
	case m.stopped != "":
		return m.stopped
	case m.sessions >= lim.MaxSessionsPerClient:
		return ReasonBudget + ": max_sessions_per_client"
	case delay >= lim.MaxCaseMS:
		return ReasonBudget + ": delay does not fit max_case_ms"
	case !m.r.Clock.Now().Before(m.deadline), int64(m.r.Clock.Now().Sub(m.start)/time.Millisecond)+delay > lim.MaxClientMS:
		return ReasonBudget + ": max_client_ms"
	}
	return ""
}

// session runs one case unless the budget or a stop forbids it.
func (m *measure) session(ctx context.Context, spec caseSpec) (CaseReport, string) {
	if reason := m.canRun(ctx, spec.delay); reason != "" {
		return CaseReport{}, reason
	}
	cs := m.runCase(ctx, spec)
	m.observeClientInfo(cs)
	switch {
	case cs.Outcome == OutcomeInterrupted:
		m.stopped = ReasonInterrupted
	case cs.Reason != nil && *cs.Reason == ReasonClientBudget:
		m.stopped = ReasonBudget + ": max_client_ms"
	case cs.Cleanup.Error != nil:
		m.stopped = ReasonCleanupFailed
	case cs.Outcome == KindAuthError || cs.Outcome == KindPermissionDenied || cs.Reason != nil && *cs.Reason == ReasonModelUnavailable:
		m.stopped = *cs.Reason
	case cs.Reason != nil && (*cs.Reason == ReasonAbsentBinary || *cs.Reason == ReasonLaunchFailed || len(*cs.Reason) > len(ReasonConfigFailure) && (*cs.Reason)[:len(ReasonConfigFailure)] == ReasonConfigFailure):
		m.stopped = *cs.Reason
	case cs.CursorPreparation != nil && cs.CursorPreparation.State != PreparationVerified:
		// A failed Cursor preparation stops the client's progression: never
		// a retry until green (design decoder-enrollment B3, FP-24).
		m.stopped = *cs.CursorPreparation.Reason
	}
	return cs, ""
}

// observeClientInfo records the first initialize clientInfo the probe saw,
// exactly, with its requester-grammar compatibility.
func (m *measure) observeClientInfo(cs CaseReport) {
	if m.cr.ClientInfo.Name != nil || cs.ServerEvents == nil {
		return
	}
	for _, ev := range m.lastEvents {
		if ev.Kind == EvInitialize && ev.ClientName != nil && ev.ClientVersion != nil {
			ci := &m.cr.ClientInfo
			ci.Name, ci.Version = sptr(*ev.ClientName), sptr(*ev.ClientVersion)
			ok, reason := RequesterCompatible(*ev.ClientName, *ev.ClientVersion, m.r.Hostname)
			ci.RequesterCompatible = bptr(ok)
			ci.ValidationReason = sptr(reason)
			ci.UnqualifiedReason = nil
			if !ok {
				ci.UnqualifiedReason = sptr("dispatch attribution incompatible: " + reason)
			}
			return
		}
	}
}

// RequesterCompatible applies contract.RequestedBy.Validate to the exact
// clientInfo strings and the local hostname, never normalizing them.
func RequesterCompatible(name, version, hostname string) (bool, string) {
	if err := (contract.RequestedBy{Name: name, Version: version, Hostname: hostname}).Validate(); err != nil {
		if ce, ok := err.(*contract.Error); ok {
			return false, ce.Message
		}
		return false, err.Error()
	}
	return true, "valid requested_by name and version"
}

func conclusive(result string) PhaseReport {
	return PhaseReport{Status: StatusConclusive, Result: sptr(result)}
}

func inconclusive(reason string, cases ...CaseReport) PhaseReport {
	return PhaseReport{Status: StatusInconclusive, Reason: sptr(reason), Cases: cases}
}

func notRunPhase(reason string) PhaseReport {
	return PhaseReport{Status: StatusNotRun, Reason: sptr(reason)}
}

func caseReason(cs CaseReport) string {
	if cs.Reason != nil {
		return cs.Outcome + ": " + *cs.Reason
	}
	return cs.Outcome
}

func (m *measure) id(suffix string) string { return m.pc.ID + "-" + suffix }

// setup: initialization, tools/list and a zero-delay call through the
// vendor, with the nonce in the vendor's tool result and clientInfo
// observed.
func (m *measure) setup(ctx context.Context) PhaseReport {
	cs, skip := m.session(ctx, caseSpec{id: m.id("setup"), setting: "default"})
	if skip != "" {
		return notRunPhase(skip)
	}
	switch {
	case cs.Outcome != KindToolResult:
		return inconclusive(caseReason(cs), cs)
	case m.cr.ClientInfo.Name == nil:
		return inconclusive("client_info_not_observed", cs)
	}
	m.setupOK = true
	ph := conclusive(ResultHealthy)
	ph.Observations, ph.Cases = 1, []CaseReport{cs}
	return ph
}

// defaults: the silent default-timeout phase, ascending delays until the
// first typed timeout, then a repeat at the selected successful bound.
func (m *measure) defaults(ctx context.Context) PhaseReport {
	if !m.setupOK {
		return notRunPhase("setup not conclusive")
	}
	delays := m.pc.Phases.Default.DelaysMS
	if len(delays) == 0 {
		delays = DefaultDelaysMS
	}
	var cases []CaseReport
	var timeout *CaseReport
	for i, d := range delays {
		cs, skip := m.session(ctx, caseSpec{id: m.id(fmt.Sprintf("default-%d", i+1)), setting: "default", delay: d})
		if skip != "" {
			if len(cases) == 0 || m.lastSuccess == 0 && timeout == nil {
				return inconclusive(skip, cases...)
			}
			break
		}
		cases = append(cases, cs)
		if cs.Outcome == KindToolResult {
			m.lastSuccess = d
			continue
		}
		if cs.Outcome == KindMCPTimeout {
			to := cs
			timeout = &to
			break
		}
		return inconclusive(caseReason(cs), cases...)
	}
	obs := 1
	if m.lastSuccess > 0 {
		cs, skip := m.session(ctx, caseSpec{id: m.id("default-repeat"), setting: "default", delay: m.lastSuccess})
		if skip != "" {
			return inconclusive("repeat observation missing: "+skip, cases...)
		}
		cases = append(cases, cs)
		if cs.Outcome != KindToolResult {
			return inconclusive("repeat observation failed: "+caseReason(cs), cases...)
		}
		obs = 2
	}
	var ph PhaseReport
	if timeout != nil {
		if timeout.ElapsedMS == nil {
			return inconclusive(ReasonProbeEndpoint, cases...)
		}
		m.silentDelay, m.silentUpper = timeout.DelayMS, timeout.ElapsedMS
		ph = conclusive(ResultTimeoutObserved)
		ph.UpperBoundMS = timeout.ElapsedMS
	} else {
		ph = conclusive(ResultLowerBound)
	}
	if m.lastSuccess > 0 {
		ph.LowerBoundMS = iptr(m.lastSuccess)
	}
	ph.Observations, ph.Cases = obs, cases
	return ph
}

// override: the raised configuration must let a silent call longer than
// the observed default complete; an optional longer call exposes the new
// bound.
func (m *measure) override(ctx context.Context) PhaseReport {
	if m.silentUpper == nil {
		return notRunPhase("no observed default timeout to exceed")
	}
	d := m.pc.Phases.Override.DelayMS
	if d <= *m.silentUpper {
		return inconclusive(fmt.Sprintf("override delay %d ms does not exceed the observed default %d ms", d, *m.silentUpper))
	}
	cs, skip := m.session(ctx, caseSpec{id: m.id("override-1"), setting: "raised", delay: d})
	if skip != "" {
		return notRunPhase(skip)
	}
	switch cs.Outcome {
	case KindMCPTimeout:
		ph := conclusive(ResultOverrideNone)
		ph.UpperBoundMS, ph.Observations, ph.Cases = cs.ElapsedMS, 1, []CaseReport{cs}
		return ph
	case KindToolResult:
	default:
		return inconclusive(caseReason(cs), cs)
	}
	ph := conclusive(ResultOverrideEffective)
	ph.LowerBoundMS, ph.Observations, ph.Cases = iptr(d), 1, []CaseReport{cs}
	if b := m.pc.Phases.Override.BoundDelayMS; b > 0 {
		cs2, skip := m.session(ctx, caseSpec{id: m.id("override-bound"), setting: "raised", delay: b})
		if skip != "" {
			return ph
		}
		ph.Cases = append(ph.Cases, cs2)
		switch {
		case cs2.Outcome == KindMCPTimeout && cs2.ElapsedMS != nil:
			ph.Result, ph.UpperBoundMS = sptr(ResultOverrideBound), cs2.ElapsedMS
		case cs2.Outcome == KindToolResult:
			ph.LowerBoundMS = iptr(b)
		}
	}
	return ph
}

// progressPhase repeats the known failing silent case with progress every
// min(1s, T/4), T the observed default lower bound.
func (m *measure) progressPhase(ctx context.Context) PhaseReport {
	if m.silentUpper == nil {
		return notRunPhase("no known failing silent case")
	}
	t := m.lastSuccess
	if t == 0 {
		t = *m.silentUpper
	}
	m.interval = max(1, min(1000, t/4))
	cs, skip := m.session(ctx, caseSpec{id: m.id("progress-1"), setting: "default", delay: m.silentDelay, interval: m.interval})
	if skip != "" {
		return notRunPhase(skip)
	}
	ph := m.progressOutcome(cs, &m.progress, ResultExtends, ResultNoExtension, m.silentDelay)
	// A typed timeout clearly beyond the silent bound (more than one
	// progress interval later) is not "no extension": progress extended the
	// call and a cap ended it, which also establishes the absolute cap.
	if m.progress == ResultNoExtension && *cs.ElapsedMS > *m.silentUpper+m.interval {
		m.progress = ResultExtends
		m.capUpper = cs.ElapsedMS
		ph.Result, ph.LowerBoundMS = sptr(ResultExtends), m.silentUpper
	}
	return ph
}

func (m *measure) progressOutcome(cs CaseReport, record *string, ok, timedOut string, lower int64) PhaseReport {
	if (cs.Outcome == KindToolResult || cs.Outcome == KindMCPTimeout) && (cs.ProgressTokenPresent == nil || !*cs.ProgressTokenPresent) {
		return inconclusive("no progress token: the progress experiment is inconclusive", cs)
	}
	if (cs.Outcome == KindToolResult || cs.Outcome == KindMCPTimeout) && cs.ProgressSent == 0 {
		return inconclusive("no progress notification was delivered for the request: the progress experiment is inconclusive", cs)
	}
	switch {
	case cs.Outcome == KindToolResult:
		*record = ok
		ph := conclusive(ok)
		ph.LowerBoundMS, ph.Observations, ph.Cases = iptr(lower), 1, []CaseReport{cs}
		return ph
	case cs.Outcome == KindMCPTimeout && cs.ElapsedMS != nil:
		*record = timedOut
		ph := conclusive(timedOut)
		ph.UpperBoundMS, ph.Observations, ph.Cases = cs.ElapsedMS, 1, []CaseReport{cs}
		if timedOut == ResultCapObserved {
			ph.LowerBoundMS = iptr(m.silentDelay)
		}
		return ph
	}
	return inconclusive(caseReason(cs), cs)
}

// absolute: with progress continuing, a deliberately longer call either
// hits an observed hard cap or establishes "no cap observed up to X".
func (m *measure) absolute(ctx context.Context) PhaseReport {
	switch {
	case m.progress == ResultNoExtension:
		return conclusive(ResultNotApplicable)
	case m.capUpper != nil:
		ph := conclusive(ResultCapObserved)
		ph.LowerBoundMS, ph.UpperBoundMS, ph.Observations = m.silentUpper, m.capUpper, 1
		return ph
	case m.progress == ResultExtends:
	default:
		return notRunPhase("progress extension not established")
	}
	d := m.pc.Phases.Absolute.DelayMS
	if d <= m.silentDelay {
		return inconclusive(fmt.Sprintf("absolute delay %d ms does not exceed the silent case %d ms", d, m.silentDelay))
	}
	cs, skip := m.session(ctx, caseSpec{id: m.id("absolute-1"), setting: "default", delay: d, interval: m.interval})
	if skip != "" {
		return notRunPhase(skip)
	}
	var rec string
	return m.progressOutcome(cs, &rec, ResultNoCapObserved, ResultCapObserved, d)
}
