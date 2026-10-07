package mcpqual

// Catalog fact rendering (FP-14). A fact is VERIFIED only when its phases
// are conclusive, the decoder version is qualified by a redacted actual
// transcript (never a synthetic fixture), and the run's platform is the
// entry's platform. Anything else is UNVERIFIED, naming the established
// subset, what is missing and iteration 07b's qualification work.

import (
	"fmt"
	"strings"

	"github.com/wedevwork/callsheet/internal/testkit/catalog"
)

// LegacyInterimSentence is iteration 07a's retired owner-authorized
// interim exception, verbatim. A catalog that still carries it anywhere is
// an old-policy catalog: publication refuses it (update the catalog
// policy first) rather than mix the old paragraph with new facts.
const LegacyInterimSentence = "Owner-authorized interim exception: iteration 07a ships task_wait and dispatch-with-wait with an UNVERIFIED 10s outer call budget, " +
	"shorter plane waits reserve transport/admission/response time, and any increase requires local timeout qualification with an explicit response margin."

// ShortPollPolicy is the support catalog's short-poll policy (design
// nonblocking-coordinator-waits), verbatim: the retired interim anchor's
// paragraph and the shipped UNVERIFIED mcp_timeout values carry it.
const ShortPollPolicy = "The default 10s MCP call budget is a deliberately short poll. Long waits use a harness-managed background CLI command. " +
	"Vendor timeout compatibility is claimed only by named local evidence; an unmeasured client remains UNVERIFIED. " +
	"Increasing the budget requires local timeout qualification with response margin."

// ShortPollAnchor is the policy paragraph's opening in
// docs/support-catalog.md: the kept compatibility anchor and its label.
const ShortPollAnchor = `<a id="interim-mcp-wait-exception"></a>**Retired: short-poll policy.** `

// TimeoutKeys are the three coordinator timeout facts publication updates.
var TimeoutKeys = []string{"mcp_timeout", "mcp_timeout_override", "mcp_progress_extension"}

func phaseOf(c ClientReport, name string) *PhaseReport {
	for i := range c.Phases {
		if c.Phases[i].Name == name {
			return &c.Phases[i]
		}
	}
	return nil
}

func ok(ph *PhaseReport, results ...string) bool {
	if ph == nil || ph.Status != StatusConclusive || ph.Result == nil {
		return false
	}
	for _, r := range results {
		if *ph.Result == r {
			return true
		}
	}
	return false
}

func ms(p *int64) int64 {
	if p == nil {
		return 0
	}
	return *p
}

// ineligible says why a conclusive measurement still cannot be VERIFIED.
func ineligible(rep *Report, c ClientReport, e catalog.Entry) string {
	switch {
	case c.DecoderVersion == nil:
		return "no decoder version was selected"
	case !c.DecoderVersion.Qualified:
		return fmt.Sprintf("decoder %s is a synthetic fixture, so this is harness evidence (%s), not qualified vendor evidence", c.DecoderVersion.Fixture, HarnessVerified)
	case rep.OS+"/"+rep.Arch != e.Platform:
		return fmt.Sprintf("observed on %s/%s, which does not certify %s", rep.OS, rep.Arch, e.Platform)
	}
	return ""
}

func phaseSummary(ph *PhaseReport) string {
	switch {
	case ph == nil:
		return "the phase was not requested"
	case ph.Status != StatusConclusive:
		return fmt.Sprintf("the %s phase was %s (%s)", ph.Name, ph.Status, *ph.Reason)
	}
	return fmt.Sprintf("the %s phase observed %s%s", ph.Name, *ph.Result, bracketText(ph.LowerBoundMS, ph.UpperBoundMS))
}

func (b factBuilder) unverified(subset, missing string) catalog.Fact {
	v := fmt.Sprintf("Established subset from %s: %s. Missing: %s; iteration 07b local timeout qualification must establish it.", b.prov, subset, missing)
	return catalog.Fact{Status: catalog.Unverified, Value: v, Evidence: b.evidence, VerificationIteration: catalog.TimeoutQualification}
}

func (b factBuilder) verified(v string) catalog.Fact {
	return catalog.Fact{Status: catalog.Verified, Value: "Measured by " + b.prov + ": " + v, Evidence: b.evidence, VerificationIteration: catalog.TimeoutQualification}
}

type factBuilder struct {
	prov     string
	evidence []string
	block    string
}

// proposeFacts renders c's three timeout facts and, when clientInfo was
// observed, its mcp_config observation.
func proposeFacts(rep *Report, c ClientReport, e catalog.Entry, reportPath string) map[string]catalog.Fact {
	version := c.ExpectedVersion
	if c.ObservedVersion != nil {
		version = *c.ObservedVersion
	}
	b := factBuilder{prov: fmt.Sprintf("qualification run %s on %s/%s with %s and the plan's default configuration", rep.RunID, rep.OS, rep.Arch, version),
		block: ineligible(rep, c, e)}
	out := map[string]catalog.Fact{}
	withEvidence := func(key string) factBuilder {
		fb := b
		fb.evidence = appendUnique(e.Facts[key].Evidence, reportPath)
		return fb
	}

	def := phaseOf(c, PhaseDefault)
	fb := withEvidence("mcp_timeout")
	switch {
	case b.block == "" && ok(def, ResultTimeoutObserved):
		out["mcp_timeout"] = fb.verified(fmt.Sprintf("a silent tool call ended with the client's typed MCP timeout event after %d ms of probe time; the last completed silent call took %d ms (%d observations at that bound).",
			ms(def.UpperBoundMS), ms(def.LowerBoundMS), def.Observations))
	case b.block == "" && ok(def, ResultLowerBound):
		out["mcp_timeout"] = fb.verified(fmt.Sprintf("silent tool calls of up to %d ms completed (%d observations); this is a lower bound, not the default.",
			ms(def.LowerBoundMS), def.Observations))
	default:
		out["mcp_timeout"] = fb.unverified(phaseSummary(def), missing(b.block, def))
	}

	ovr := phaseOf(c, PhaseOverride)
	fb = withEvidence("mcp_timeout_override")
	if b.block == "" && ok(ovr, ResultOverrideEffective, ResultOverrideBound) && c.Settings.Override != nil {
		o := c.Settings.Override
		v := fmt.Sprintf("setting %s = %s (%s) let a %d ms silent call complete beyond the observed default.", o.Setting, o.Value, o.Explanation, ms(ovr.LowerBoundMS))
		if ovr.UpperBoundMS != nil {
			v += fmt.Sprintf(" A longer call then timed out after %d ms.", *ovr.UpperBoundMS)
		}
		out["mcp_timeout_override"] = fb.verified(v)
	} else {
		out["mcp_timeout_override"] = fb.unverified(phaseSummary(ovr), missing(b.block, ovr))
	}

	prog, abs := phaseOf(c, PhaseProgress), phaseOf(c, PhaseAbsolute)
	fb = withEvidence("mcp_progress_extension")
	switch {
	case b.block == "" && ok(prog, ResultExtends) && ok(abs, ResultCapObserved):
		out["mcp_progress_extension"] = fb.verified(fmt.Sprintf("progress notifications extended a %d ms call past the silent bound; with continued progress a longer call still ended with the typed timeout event after %d ms (the observed absolute cap).",
			ms(prog.LowerBoundMS), ms(abs.UpperBoundMS)))
	case b.block == "" && ok(prog, ResultNoExtension):
		out["mcp_progress_extension"] = fb.verified(fmt.Sprintf("progress notifications did not extend the call: it ended with the typed timeout event after %d ms, so no separate absolute cap applies.",
			ms(prog.UpperBoundMS)))
	default:
		subset := phaseSummary(prog)
		if ok(abs, ResultNoCapObserved) {
			subset += fmt.Sprintf("; no absolute cap observed up to %d ms, so the maximum remains UNVERIFIED", ms(abs.LowerBoundMS))
		}
		miss := missing(b.block, prog)
		if b.block == "" && ok(prog, ResultExtends) {
			miss = "an observed absolute cap (" + phaseSummary(abs) + ")"
		}
		out["mcp_progress_extension"] = fb.unverified(subset, miss)
	}

	if ci := c.ClientInfo; ci.Name != nil && ci.Version != nil && ci.RequesterCompatible != nil {
		f := e.Facts["mcp_config"]
		f.Status = catalog.Unverified
		f.Evidence = appendUnique(f.Evidence, reportPath)
		obs := fmt.Sprintf(" Qualification run %s observed initialize clientInfo name %s and version %s (requested_by compatible: %v", rep.RunID, quoteJSON(*ci.Name), quoteJSON(*ci.Version), *ci.RequesterCompatible)
		if !*ci.RequesterCompatible {
			obs += "; dispatch attribution fails: " + strings.TrimSuffix(*ci.ValidationReason, ".")
		}
		f.Value += obs + "); registration through the vendor command is not qualified by it."
		out["mcp_config"] = f
	}
	return out
}

func missing(block string, ph *PhaseReport) string {
	if block != "" {
		return block
	}
	if ph == nil {
		return "the phase was not requested"
	}
	if ph.Status != StatusConclusive {
		return "a conclusive " + ph.Name + " phase"
	}
	return "a qualifying " + ph.Name + " result"
}

func appendUnique(xs []string, x string) []string {
	out := append([]string(nil), xs...)
	for _, v := range out {
		if v == x {
			return out
		}
	}
	return append(out, x)
}
