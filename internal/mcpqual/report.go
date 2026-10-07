package mcpqual

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// ReportSchema is the report.json version.
const ReportSchema = 1

// Phase names, in run order.
const (
	PhaseSetup    = "setup"
	PhaseDefault  = "default"
	PhaseOverride = "override"
	PhaseProgress = "progress"
	PhaseAbsolute = "absolute"
)

// Phase statuses.
const (
	StatusConclusive   = "conclusive"
	StatusInconclusive = "inconclusive"
	StatusNotRun       = "not_run"
)

// Owner-visible provenance statements (the report distinguishes them).
const (
	HarnessVerified = "harness implementation verified by fake tests"
	VendorMeasured  = "vendor behavior measured locally"
	VendorNotRun    = "vendor behavior not measured"
)

// Report is report.json. Nullable fields are explicit JSON nulls, each
// paired with a nonempty reason where the schema says so.
type Report struct {
	Schema            int            `json:"schema"`
	RunID             string         `json:"run_id"`
	CaptureDate       string         `json:"capture_date"`
	OS                string         `json:"os"`
	Arch              string         `json:"arch"`
	HarnessVersion    string         `json:"harness_version"`
	PlanSHA256        string         `json:"plan_sha256"`
	ModelCallsAllowed bool           `json:"model_calls_allowed"`
	HarnessStatus     string         `json:"harness_status"`
	VendorBehavior    string         `json:"vendor_behavior"`
	Limits            Limits         `json:"limits"`
	PlannedUpperBound PlannedBound   `json:"planned_upper_bound"`
	Clients           []ClientReport `json:"clients"`
	Cleanup           CleanupReport  `json:"cleanup"`
	Interrupted       bool           `json:"interrupted"`
	Outcome           string         `json:"outcome"`
	Evidence          []EvidenceRef  `json:"evidence"`
}

// PlannedBound is the upper bound reported before any launch.
type PlannedBound struct {
	Sessions int   `json:"sessions"`
	WallMS   int64 `json:"wall_ms"`
}

// EvidenceRef names one sanitized evidence file (relative to the evidence
// directory) and the SHA-256 of exactly its published bytes.
type EvidenceRef struct {
	Path   string `json:"path"`
	SHA256 string `json:"sha256"`
	Bytes  int64  `json:"bytes"`
}

// ClientReport is one client's evidence.
type ClientReport struct {
	ID               string          `json:"id"`
	ExecutableName   string          `json:"executable_name"`
	ExecutableSHA256 *string         `json:"executable_sha256"`
	ExpectedVersion  string          `json:"expected_version"`
	ObservedVersion  *string         `json:"observed_version"`
	Driver           string          `json:"driver"`
	Model            *string         `json:"model"`
	Effort           *string         `json:"effort"`
	Decoder          string          `json:"decoder"`
	DecoderVersion   *DecoderVersion `json:"decoder_version"`
	Settings         Settings        `json:"settings"`
	ClientInfo       ClientInfo      `json:"client_info"`
	Phases           []PhaseReport   `json:"phases"`
	Outcome          string          `json:"outcome"`
	Reason           *string         `json:"reason"`
}

// Settings are the sanitized configuration snapshots and the override
// candidate.
type Settings struct {
	Default      *string   `json:"default"`
	Raised       *string   `json:"raised"`
	Override     *Override `json:"override"`
	Prerequisite *string   `json:"prerequisite"`
}

// ClientInfo is the exact initialize clientInfo and its requester-grammar
// compatibility (never normalized).
type ClientInfo struct {
	Name                *string `json:"name"`
	Version             *string `json:"version"`
	RequesterCompatible *bool   `json:"requester_compatible"`
	ValidationReason    *string `json:"validation_reason"`
	UnqualifiedReason   *string `json:"unqualified_reason"`
}

// PhaseReport is one measurement phase.
type PhaseReport struct {
	Name         string       `json:"name"`
	Status       string       `json:"status"`
	Result       *string      `json:"result"`
	Reason       *string      `json:"reason"`
	LowerBoundMS *int64       `json:"lower_bound_ms"`
	UpperBoundMS *int64       `json:"upper_bound_ms"`
	Observations int          `json:"observations"`
	Cases        []CaseReport `json:"cases"`
}

// CaseReport is one scripted session. Offsets and the elapsed interval
// are on the probe's monotonic clock only.
type CaseReport struct {
	CaseID               string      `json:"case_id"`
	Setting              string      `json:"setting"`
	DelayMS              int64       `json:"delay_ms"`
	ProgressIntervalMS   int64       `json:"progress_interval_ms"`
	ProgressTokenPresent *bool       `json:"progress_token_present"`
	StartOffsetNS        *int64      `json:"start_offset_ns"`
	EndOffsetNS          *int64      `json:"end_offset_ns"`
	ElapsedMS            *int64      `json:"elapsed_ms"`
	ProgressSent         int         `json:"progress_sent"`
	Exit                 *int        `json:"exit"`
	Signal               *string     `json:"signal"`
	Outcome              string      `json:"outcome"`
	Reason               *string     `json:"reason"`
	Events               []Event     `json:"events"`
	ServerEvents         *string     `json:"server_events"`
	VendorEvents         *string     `json:"vendor_events"`
	Cleanup              CaseCleanup `json:"cleanup"`
}

// CaseCleanup records how the case's process group was reaped.
type CaseCleanup struct {
	TermSent          bool    `json:"term_sent"`
	KillSent          bool    `json:"kill_sent"`
	LeaderExitedFirst bool    `json:"leader_exited_first"`
	GroupGone         bool    `json:"group_gone"`
	Error             *string `json:"error"`
}

// CleanupReport summarizes cleanup across the run.
type CleanupReport struct {
	OK       bool     `json:"ok"`
	Failures []string `json:"failures"`
}

// Case outcomes beyond the decoder kinds.
const (
	OutcomeInconclusive = "inconclusive"
	OutcomeInterrupted  = "interrupted"
	OutcomeNotRun       = "not_run"
)

func sptr(s string) *string { return &s }
func iptr(n int64) *int64   { return &n }
func bptr(b bool) *bool     { return &b }

var reportKeys = map[string][]string{
	"report":  {"schema", "run_id", "capture_date", "os", "arch", "harness_version", "plan_sha256", "model_calls_allowed", "harness_status", "vendor_behavior", "limits", "planned_upper_bound", "clients", "cleanup", "interrupted", "outcome", "evidence"},
	"client":  {"id", "executable_name", "executable_sha256", "expected_version", "observed_version", "driver", "model", "effort", "decoder", "decoder_version", "settings", "client_info", "phases", "outcome", "reason"},
	"info":    {"name", "version", "requester_compatible", "validation_reason", "unqualified_reason"},
	"phase":   {"name", "status", "result", "reason", "lower_bound_ms", "upper_bound_ms", "observations", "cases"},
	"case":    {"case_id", "setting", "delay_ms", "progress_interval_ms", "progress_token_present", "start_offset_ns", "end_offset_ns", "elapsed_ms", "progress_sent", "exit", "signal", "outcome", "reason", "events", "server_events", "vendor_events", "cleanup"},
	"cleanup": {"term_sent", "kill_sent", "leader_exited_first", "group_gone", "error"},
}

func requireKeys(raw json.RawMessage, kind, where string) error {
	var m map[string]json.RawMessage
	if err := json.Unmarshal(raw, &m); err != nil {
		return fmt.Errorf("report: %s: %w", where, err)
	}
	for _, k := range reportKeys[kind] {
		if _, ok := m[k]; !ok {
			return fmt.Errorf("report: %s: missing field %s (nullable fields must be an explicit null)", where, k)
		}
	}
	return nil
}

// ParseReport strictly decodes and validates a report: every field present
// (nullable ones as explicit nulls with their reasons), unique case IDs,
// and evidence paths that stay inside the evidence directory.
func ParseReport(b []byte) (*Report, error) {
	if len(b) > MaxEvidenceFileBytes {
		return nil, fmt.Errorf("report: %d bytes exceeds %d", len(b), MaxEvidenceFileBytes)
	}
	var r Report
	if err := decodeStrict(b, &r); err != nil {
		return nil, fmt.Errorf("report: %w", err)
	}
	var shape struct {
		Clients []struct {
			ClientInfo json.RawMessage `json:"client_info"`
			Phases     []struct {
				Cases []struct {
					Cleanup json.RawMessage `json:"cleanup"`
				} `json:"cases"`
			} `json:"phases"`
		} `json:"clients"`
	}
	json.Unmarshal(b, &shape)
	if err := requireKeys(b, "report", "top level"); err != nil {
		return nil, err
	}
	var rawClients struct {
		Clients []json.RawMessage `json:"clients"`
	}
	json.Unmarshal(b, &rawClients)
	for i, rc := range rawClients.Clients {
		where := fmt.Sprintf("clients[%d]", i)
		if err := requireKeys(rc, "client", where); err != nil {
			return nil, err
		}
		if err := requireKeys(shape.Clients[i].ClientInfo, "info", where+".client_info"); err != nil {
			return nil, err
		}
		var rp struct {
			Phases []json.RawMessage `json:"phases"`
		}
		json.Unmarshal(rc, &rp)
		for j, ph := range rp.Phases {
			pw := fmt.Sprintf("%s.phases[%d]", where, j)
			if err := requireKeys(ph, "phase", pw); err != nil {
				return nil, err
			}
			var rcs struct {
				Cases []json.RawMessage `json:"cases"`
			}
			json.Unmarshal(ph, &rcs)
			for k, cs := range rcs.Cases {
				cw := fmt.Sprintf("%s.cases[%d]", pw, k)
				if err := requireKeys(cs, "case", cw); err != nil {
					return nil, err
				}
				if err := requireKeys(shape.Clients[i].Phases[j].Cases[k].Cleanup, "cleanup", cw+".cleanup"); err != nil {
					return nil, err
				}
			}
		}
	}
	return &r, r.Validate()
}

// Validate checks the report's semantic contract.
func (r *Report) Validate() error {
	fail := func(format string, a ...any) error { return fmt.Errorf("report: "+format, a...) }
	switch {
	case r.Schema != ReportSchema:
		return fail("schema %d, want %d", r.Schema, ReportSchema)
	case !idPattern.MatchString(r.RunID):
		return fail("run_id %q invalid", r.RunID)
	case r.CaptureDate == "" || r.OS == "" || r.Arch == "" || r.HarnessVersion == "" || len(r.PlanSHA256) != 64:
		return fail("capture_date, os, arch, harness_version and plan_sha256 are required")
	case r.HarnessStatus != HarnessVerified:
		return fail("harness_status must be %q", HarnessVerified)
	case r.VendorBehavior != VendorMeasured && r.VendorBehavior != VendorNotRun:
		return fail("vendor_behavior %q", r.VendorBehavior)
	case r.Outcome != StatusConclusive && r.Outcome != "partial":
		return fail("outcome %q", r.Outcome)
	case len(r.Clients) == 0 || len(r.Clients) > len(clientDecoders):
		return fail("%d clients", len(r.Clients))
	}
	seenPath := map[string]bool{}
	for _, ev := range r.Evidence {
		if err := checkRelPath("evidence path", ev.Path); err != nil {
			return fail("%v", err)
		}
		if seenPath[ev.Path] {
			return fail("duplicate evidence path %s", ev.Path)
		}
		seenPath[ev.Path] = true
		if len(ev.SHA256) != 64 || ev.Bytes < 0 || ev.Bytes > MaxEvidenceFileBytes {
			return fail("evidence %s: bad hash or size", ev.Path)
		}
	}
	cases := map[string]bool{}
	clients := map[string]bool{}
	for _, c := range r.Clients {
		if _, ok := clientDecoders[c.ID]; !ok || clients[c.ID] {
			return fail("client id %q unknown or duplicate", c.ID)
		}
		clients[c.ID] = true
		if c.ObservedVersion == nil && c.Reason == nil {
			return fail("%s: observed_version null without a reason", c.ID)
		}
		if c.Outcome != StatusConclusive && (c.Reason == nil || *c.Reason == "") {
			return fail("%s: outcome %s without a reason", c.ID, c.Outcome)
		}
		if dv := c.DecoderVersion; dv != nil {
			if err := dv.validateEvidence(); err != nil {
				return fail("%s: %v", c.ID, err)
			}
		}
		ci := c.ClientInfo
		if (ci.Name == nil || ci.Version == nil || ci.RequesterCompatible == nil) && (ci.UnqualifiedReason == nil || *ci.UnqualifiedReason == "") {
			return fail("%s: client_info missing without an unqualified_reason", c.ID)
		}
		for _, ph := range c.Phases {
			if ph.Status != StatusConclusive && (ph.Reason == nil || *ph.Reason == "") {
				return fail("%s.%s: status %s without a reason", c.ID, ph.Name, ph.Status)
			}
			if ph.Result == nil && ph.Status == StatusConclusive {
				return fail("%s.%s: conclusive without a result", c.ID, ph.Name)
			}
			for _, cs := range ph.Cases {
				if !idPattern.MatchString(cs.CaseID) || cases[cs.CaseID] {
					return fail("case_id %q invalid or duplicate", cs.CaseID)
				}
				cases[cs.CaseID] = true
				if cs.ElapsedMS == nil && cs.Outcome != KindToolResult && (cs.Reason == nil || *cs.Reason == "") {
					return fail("%s: elapsed_ms null without a reason", cs.CaseID)
				}
				for _, p := range []*string{cs.ServerEvents, cs.VendorEvents} {
					if p != nil && !seenPath[*p] {
						return fail("%s: evidence %s is not listed", cs.CaseID, *p)
					}
				}
			}
		}
	}
	return nil
}

// CheckEvidence verifies every listed evidence file under dir exists with
// exactly the recorded bytes and hash.
func (r *Report) CheckEvidence(dir string) error {
	for _, ev := range r.Evidence {
		b, err := os.ReadFile(filepath.Join(dir, filepath.FromSlash(ev.Path)))
		if err != nil {
			return fmt.Errorf("evidence %s: %w", ev.Path, err)
		}
		if int64(len(b)) != ev.Bytes || sha256Hex(b) != ev.SHA256 {
			return fmt.Errorf("evidence %s: bytes or hash differ from the report", ev.Path)
		}
	}
	return nil
}

func sha256Hex(b []byte) string {
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:])
}

// Markdown renders the owner-visible summary.
func (r *Report) Markdown() string {
	var sb strings.Builder
	fmt.Fprintf(&sb, "# MCP timeout qualification %s\n\n", r.RunID)
	fmt.Fprintf(&sb, "- Captured %s on %s/%s by mcpqual %s; outcome **%s**%s.\n", r.CaptureDate, r.OS, r.Arch, r.HarnessVersion, r.Outcome,
		map[bool]string{true: " (interrupted)", false: ""}[r.Interrupted])
	fmt.Fprintf(&sb, "- Harness: %s. Vendor: %s.\n", r.HarnessStatus, r.VendorBehavior)
	fmt.Fprintf(&sb, "- Model calls allowed: %v. Planned upper bound: %d sessions, %d ms.\n", r.ModelCallsAllowed, r.PlannedUpperBound.Sessions, r.PlannedUpperBound.WallMS)
	if r.Cleanup.OK {
		sb.WriteString("- Cleanup: every launched process group was reaped and proven absent.\n")
	} else {
		fmt.Fprintf(&sb, "- Cleanup FAILED: %s. Nothing is published from this run.\n", strings.Join(r.Cleanup.Failures, "; "))
	}
	for _, c := range r.Clients {
		fmt.Fprintf(&sb, "\n## %s\n\n", c.ID)
		dv := "none"
		if c.DecoderVersion != nil {
			dv = c.DecoderVersion.Fixture
			if !c.DecoderVersion.Qualified {
				dv += " (synthetic fixture only: parser behaviour, not vendor evidence)"
			}
		}
		fmt.Fprintf(&sb, "- Outcome: %s%s. Decoder: %s %s.\n", c.Outcome, reasonSuffix(c.Reason), c.Decoder, dv)
		if c.Settings.Prerequisite != nil {
			fmt.Fprintf(&sb, "- Owner prerequisite: %s.\n", *c.Settings.Prerequisite)
		}
		if c.ClientInfo.Name != nil && c.ClientInfo.Version != nil {
			fmt.Fprintf(&sb, "- clientInfo name %s version %s; requester compatible: %v%s.\n", quoteJSON(*c.ClientInfo.Name), quoteJSON(*c.ClientInfo.Version),
				*c.ClientInfo.RequesterCompatible, reasonSuffix(c.ClientInfo.ValidationReason))
		} else {
			fmt.Fprintf(&sb, "- clientInfo not observed%s.\n", reasonSuffix(c.ClientInfo.UnqualifiedReason))
		}
		for _, ph := range c.Phases {
			res := ""
			if ph.Result != nil {
				res = " " + *ph.Result
			}
			fmt.Fprintf(&sb, "- Phase %s: %s%s%s%s (%d cases).\n", ph.Name, ph.Status, res, bracketText(ph.LowerBoundMS, ph.UpperBoundMS), reasonSuffix(ph.Reason), len(ph.Cases))
		}
		fmt.Fprintf(&sb, "- %s\n", ShortPollDecision(ShortPollBudget, r, c))
	}
	return sb.String()
}

// ShortPollBudget is the shipping MCP call budget B (internal/mcp's
// DefaultBudget) that the short confirmation checks.
const ShortPollBudget = 10 * time.Second

// ShortPollMargin is the response margin max(2s, 10% of t).
func ShortPollMargin(t time.Duration) time.Duration { return max(2*time.Second, t/10) }

// ShortPollCompatible is the conservative decision B + max(2s, 0.1L) < L
// for a measured lower bound L (sufficient for the unknown timeout T >= L,
// since T - max(2s, 0.1T) grows with T).
func ShortPollCompatible(b, l time.Duration) bool { return b+ShortPollMargin(l) < l }

// shortPollSum renders "B + max(2s, 0.1L) = S <op> L" with values.
func shortPollSum(b, l time.Duration) string {
	op := "<"
	if !ShortPollCompatible(b, l) {
		op = ">="
	}
	return fmt.Sprintf("%v + max(2s, %v) = %v %s %v", b, l/10, b+ShortPollMargin(l), op, l)
}

// ShortPollDecision renders client c's short-poll compatibility for
// budget b from its default phase in run rep (design
// nonblocking-coordinator-waits, Short confirmation): compatible only on a
// measured lower bound L with B + max(2s, 0.1L) < L, stated as a lower
// bound and never as the timeout; incompatible when a typed timeout's
// upper bound itself fails the inequality; otherwise not established. A
// verdict about the vendor needs qualified, correlated evidence (a decoder
// qualified by an actual transcript, a conclusive setup, the successful
// bound repeated, a clean and uninterrupted run); without it the decision
// is UNVERIFIED and the numbers are shown as measurement only. A
// conclusive timeout observation is never by itself a compatibility pass,
// and an upper bound never proves safety.
func ShortPollDecision(b time.Duration, rep *Report, c ClientReport) string {
	head := fmt.Sprintf("Short-poll decision (B=%v, B + max(2s, 0.1L) < L): ", b)
	ph := phaseOf(c, PhaseDefault)
	switch {
	case ph == nil:
		return head + "compatibility UNVERIFIED: the default phase was not requested."
	case ph.Status != StatusConclusive:
		return head + "compatibility UNVERIFIED: the default phase is " + ph.Status + reasonSuffix(ph.Reason) + "; a failed, interrupted or unmeasured run is never a pass."
	}
	verdict, detail := shortPollVerdict(b, ph)
	if detail == "" {
		return head + "compatibility UNVERIFIED: the default phase measured no bound."
	}
	if why := shortPollBlock(rep, c, ph, verdict); why != "" {
		return head + "vendor compatibility UNVERIFIED (" + why + "): measurement only, " + detail
	}
	return head + verdict + detail
}

// Short-poll verdicts.
const (
	verdictCompatible     = "compatible for this measured tuple: "
	verdictNotEstablished = "not established: "
	verdictIncompatible   = "not compatible: "
	verdictInconclusive   = "not established (failed or inconclusive): "
)

// shortPollVerdict is the default phase's verdict and its numbers ("" and
// "" without any bound).
func shortPollVerdict(b time.Duration, ph *PhaseReport) (string, string) {
	upper := ""
	if ph.UpperBoundMS != nil {
		upper = fmt.Sprintf(" A typed timeout was observed at or below %v; it is reported, never used as the safe value.", time.Duration(*ph.UpperBoundMS)*time.Millisecond)
	}
	if ph.LowerBoundMS != nil {
		l := time.Duration(*ph.LowerBoundMS) * time.Millisecond
		if ShortPollCompatible(b, l) {
			return verdictCompatible, fmt.Sprintf("L = %v is the longest silent call that completed (a lower bound, not the timeout): %s.", l, shortPollSum(b, l)) + upper
		}
		if ph.UpperBoundMS == nil {
			return verdictNotEstablished, fmt.Sprintf("the lower bound L = %v is too short: %s; a longer silent call must complete.", l, shortPollSum(b, l))
		}
	}
	if ph.UpperBoundMS != nil {
		u := time.Duration(*ph.UpperBoundMS) * time.Millisecond
		if !ShortPollCompatible(b, u) {
			return verdictIncompatible, fmt.Sprintf("the client timed out at or below %v, and even that bound fails: %s.", u, shortPollSum(b, u))
		}
		return verdictInconclusive, fmt.Sprintf("a typed timeout at or below %v without a lower bound that satisfies the inequality; an upper bound never proves safety.", u)
	}
	return "", ""
}

// shortPollBlock says why the evidence cannot carry a vendor verdict (""
// when it can): a decoder qualified by an actual transcript, a conclusive
// setup, for a compatible verdict the successful bound repeated, and a
// clean, uninterrupted run.
func shortPollBlock(rep *Report, c ClientReport, ph *PhaseReport, verdict string) string {
	setup := phaseOf(c, PhaseSetup)
	switch {
	case c.DecoderVersion == nil:
		return "no decoder version was selected"
	case !c.DecoderVersion.Qualified:
		return "decoder " + c.DecoderVersion.Fixture + " is a synthetic fixture, not an actual-transcript qualification"
	case len(c.DecoderVersion.Evidence) == 0:
		return "decoder " + c.DecoderVersion.Fixture + " is marked qualified without enrolled real-transcript evidence"
	case rep != nil && len(c.DecoderVersion.MissingCapabilities(rep.OS+"/"+rep.Arch, RequiredCapabilities(KindToolResult)...)) > 0:
		return "decoder " + c.DecoderVersion.Fixture + " has no enrolled success-path evidence for " + rep.OS + "/" + rep.Arch
	case rep != nil && ph.UpperBoundMS != nil && len(c.DecoderVersion.MissingCapabilities(rep.OS+"/"+rep.Arch, CapMCPTimeout)) > 0:
		return "decoder " + c.DecoderVersion.Fixture + " has no enrolled mcp_timeout evidence for " + rep.OS + "/" + rep.Arch
	case setup == nil || setup.Status != StatusConclusive:
		return "the setup phase is not conclusive"
	case verdict == verdictCompatible && ph.Observations < 2:
		return "the successful bound was not repeated"
	case rep != nil && rep.Interrupted:
		return "the run was interrupted"
	case rep != nil && !rep.Cleanup.OK:
		return "the run's cleanup failed"
	}
	return ""
}

func reasonSuffix(r *string) string {
	if r == nil || *r == "" {
		return ""
	}
	return " (" + *r + ")"
}

func bracketText(lo, hi *int64) string {
	switch {
	case lo != nil && hi != nil:
		return fmt.Sprintf(" (%d ms, %d ms]", *lo, *hi)
	case lo != nil:
		return fmt.Sprintf(" (> %d ms tested)", *lo)
	case hi != nil:
		return fmt.Sprintf(" (<= %d ms)", *hi)
	}
	return ""
}

// sortedEvidence orders evidence by path for stable output.
func sortedEvidence(evs []EvidenceRef) []EvidenceRef {
	out := append([]EvidenceRef(nil), evs...)
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out
}
