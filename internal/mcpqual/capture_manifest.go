package mcpqual

// The capture manifest (design decoder-enrollment, Capture evidence): a
// separate operation and schema from qualification's report. A capture
// bundle is the manifest plus allowlisted, sanitized, hash-addressed
// payload files; the manifest lists every payload (never itself) and
// records how each was collected. It asserts nothing about vendor
// behavior: vendor_behavior is always "not_evaluated", and hashes attest
// byte integrity only, not that a human ran the CLI.

import (
	"encoding/json"
	"fmt"
	"path"
	"reflect"
	"regexp"
	"slices"
	"strings"
	"time"
)

// Capture schema identifiers.
const (
	// CaptureSchema is the manifest schema (distinct from qualification's
	// integer report schema).
	CaptureSchema = "mcpqual-capture-v1"
	// CaptureRedactionPolicy names NewCaptureRedactor's policy.
	CaptureRedactionPolicy = "mcpqual-capture-v1"
	// VendorNotEvaluated is the only vendor_behavior a capture records.
	VendorNotEvaluated = "not_evaluated"
	// CaptureManifestName and CapturePlanName are the bundle's top-level
	// files.
	CaptureManifestName = "manifest.json"
	CapturePlanName     = "plan.json"
)

// Capture aggregate and client states.
const (
	CaptureComplete    = "complete"
	CapturePartial     = "partial"
	CaptureInterrupted = "interrupted"
	CaptureNotRun      = "not_run"
)

// Capture stage states: not attempted, attempted without a process, or a
// launched (and reaped) process.
const (
	StageNotRun       = "not_run"
	StageLaunchFailed = "launch_failed"
	StagePrepared     = "prepared"
	StageRan          = "ran"
)

// Capture reasons (design decoder-enrollment, Lifecycle and failures);
// absent_binary, launch_failed, metadata_capture_failed, version_mismatch,
// evidence_truncated, cleanup_failed and interrupted are shared with
// qualification.
const (
	ReasonSessionExit        = "session_exit_nonzero"
	ReasonProbeNotObserved   = "probe_not_observed"
	ReasonProbeIncomplete    = "probe_incomplete"
	ReasonProbeAnomaly       = "probe_anomaly"
	ReasonSessionWatchdog    = "session_watchdog"
	ReasonEvidenceOmitted    = "evidence_omitted"
	ReasonStreamHeld         = "stream_held_after_cleanup"
	ReasonSessionStdoutEmpty = "session_stdout_empty"
)

// Per-client payload files, in stage order.
const (
	FileVersionStdout = "version-stdout.txt"
	FileVersionStderr = "version-stderr.txt"
	FileHelpStdout    = "help-stdout.txt"
	FileHelpStderr    = "help-stderr.txt"
	FileConfig        = "config.txt"
	FileVendorEvents  = "vendor-events.jsonl"
	FileVendorStderr  = "vendor-stderr.txt"
	FileServerEvents  = "server-events.jsonl"
)

// CaptureClientFiles are the allowlisted per-client payload names; with
// plan.json they are a bundle's only payloads (at most nine per client).
var CaptureClientFiles = []string{FileVersionStdout, FileVersionStderr, FileHelpStdout, FileHelpStderr, FileConfig, FileVendorEvents, FileVendorStderr, FileServerEvents}

// CaptureManifest is manifest.json.
type CaptureManifest struct {
	Schema          string              `json:"schema"`
	RunID           string              `json:"run_id"`
	CapturedAt      string              `json:"captured_at"`
	HarnessVersion  string              `json:"harness_version"`
	OS              string              `json:"os"`
	Arch            string              `json:"arch"`
	PlanSHA256      string              `json:"plan_sha256"`
	Limits          CaptureLimitsRecord `json:"limits"`
	RedactionPolicy string              `json:"redaction_policy"`
	State           string              `json:"state"`
	// Reason says why the capture is not complete (null when it is).
	Reason         *string `json:"reason"`
	VendorBehavior string  `json:"vendor_behavior"`
	// Plan records how plan.json was written (cut or omitted lines).
	Plan    CaptureStream   `json:"plan"`
	Cleanup CleanupReport   `json:"cleanup"`
	Clients []CaptureClient `json:"clients"`
	Files   []EvidenceRef   `json:"files"`
}

// CaptureLimitsRecord is the run's effective limits: plan limits may
// lower the capture caps, never raise them.
type CaptureLimitsRecord struct {
	Clients           int   `json:"clients"`
	SessionsPerClient int   `json:"sessions_per_client"`
	SessionMS         int64 `json:"session_ms"`
	ClientMS          int64 `json:"client_ms"`
	MetadataMS        int64 `json:"metadata_ms"`
	StreamBytes       int   `json:"stream_bytes"`
	ServerEventsBytes int   `json:"server_events_bytes"`
	FileBytes         int   `json:"file_bytes"`
	ClientBytes       int   `json:"client_bytes"`
	BundleBytes       int   `json:"bundle_bytes"`
	ManifestBytes     int   `json:"manifest_bytes"`
}

// CaptureClient is one client's capture record. Nullable fields are
// explicit nulls paired with their reason.
type CaptureClient struct {
	ID                    string  `json:"id"`
	ExpectedVersion       string  `json:"expected_version"`
	ObservedVersion       *string `json:"observed_version"`
	ObservedVersionReason *string `json:"observed_version_reason"`
	ExecutableSHA256      *string `json:"executable_sha256"`
	ExecutableReason      *string `json:"executable_sha256_reason"`
	// Decoder and DecoderFixture are the plan's inert labels: capture never
	// consults a decoder, so DecoderValidated is always false.
	Decoder          string          `json:"decoder"`
	DecoderFixture   string          `json:"decoder_fixture"`
	DecoderValidated bool            `json:"decoder_validated"`
	Model            string          `json:"model"`
	Effort           *string         `json:"effort"`
	EffortReason     *string         `json:"effort_reason"`
	Argv             []string        `json:"argv"`
	Config           CaptureConfig   `json:"config"`
	CaseID           string          `json:"case_id"`
	Nonce            string          `json:"nonce"`
	Version          CaptureStage    `json:"version"`
	Help             CaptureStage    `json:"help"`
	Session          CaptureStage    `json:"session"`
	Probe            *CaptureProbe   `json:"probe"`
	ProbeReason      *string         `json:"probe_reason"`
	Streams          []CaptureStream `json:"streams"`
	State            string          `json:"state"`
	Reason           *string         `json:"reason"`
}

// CaptureConfig is the session's sanitized configuration provenance: the
// portable workspace-relative config file (its labeled content is
// config.txt), the owner-prepared prerequisite and the absence of any
// timeout override.
type CaptureConfig struct {
	Path            string  `json:"path"`
	Prerequisite    *string `json:"prerequisite"`
	TimeoutOverride string  `json:"timeout_override"`
}

// TimeoutOverrideNone is the only timeout override a capture records.
const TimeoutOverrideNone = "none"

// CaptureStage is one metadata command or the setup session.
type CaptureStage struct {
	State       string       `json:"state"`
	Reason      *string      `json:"reason"`
	WatchdogMS  int64        `json:"watchdog_ms"`
	Exit        *int         `json:"exit"`
	Signal      *string      `json:"signal"`
	Watchdog    bool         `json:"watchdog"`
	Interrupted bool         `json:"interrupted"`
	StdoutHeld  bool         `json:"stdout_held"`
	StderrHeld  bool         `json:"stderr_held"`
	Cleanup     *CaseCleanup `json:"cleanup"`
}

// CaptureProbe holds only parsed probe-server evidence for the requested
// case: never a compatibility claim, and never proof that the vendor
// received the nonce.
type CaptureProbe struct {
	Intact        bool     `json:"intact"`
	Initialized   bool     `json:"initialized"`
	ClientName    *string  `json:"client_name"`
	ClientVersion *string  `json:"client_version"`
	Receipts      int      `json:"receipts"`
	Completed     bool     `json:"completed"`
	Anomalies     []string `json:"anomalies"`
}

// CaptureStream records how one payload file was collected and written:
// lines omitted whole by redaction, and whether its input (a stream or
// file bound) or its serialized output (a file or bundle bound) was cut.
type CaptureStream struct {
	Path            string `json:"path"`
	OmittedLines    int    `json:"omitted_lines"`
	InputTruncated  bool   `json:"input_truncated"`
	OutputTruncated bool   `json:"output_truncated"`
}

// Clean reports a stream with nothing cut or omitted.
func (s CaptureStream) Clean() bool {
	return s.OmittedLines == 0 && !s.InputTruncated && !s.OutputTruncated
}

var hex64 = regexp.MustCompile(`^[0-9a-f]{64}$`)

// ParseCaptureManifest strictly decodes and validates a manifest: unknown
// fields anywhere are rejected and every field must be present (nullable
// ones as explicit nulls).
func ParseCaptureManifest(b []byte) (*CaptureManifest, error) {
	if len(b) > MaxEvidenceFileBytes {
		return nil, fmt.Errorf("capture manifest: %d bytes exceeds %d", len(b), MaxEvidenceFileBytes)
	}
	var m CaptureManifest
	if err := decodeStrict(b, &m); err != nil {
		return nil, fmt.Errorf("capture manifest: %w", err)
	}
	if err := requireFields(b, reflect.TypeOf(m), "capture manifest"); err != nil {
		return nil, err
	}
	return &m, m.Validate()
}

// requireFields requires every json member of t (and of its nested structs
// and struct slices) that is not omitempty to be present in raw, decoding
// raw once.
func requireFields(raw json.RawMessage, t reflect.Type, where string) error {
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		return fmt.Errorf("%s: %w", where, err)
	}
	return requireIn(v, t, where)
}

func requireIn(v any, t reflect.Type, where string) error {
	for t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	switch t.Kind() {
	case reflect.Struct:
		if v == nil {
			return nil
		}
		m, ok := v.(map[string]any)
		if !ok {
			return fmt.Errorf("%s: not an object", where)
		}
		for i := 0; i < t.NumField(); i++ {
			f := t.Field(i)
			name, opts, _ := strings.Cut(f.Tag.Get("json"), ",")
			if !f.IsExported() || name == "-" || name == "" {
				continue
			}
			fv, ok := m[name]
			if !ok {
				if strings.Contains(opts, "omitempty") {
					continue
				}
				return fmt.Errorf("%s: missing field %s (nullable fields must be an explicit null)", where, name)
			}
			if err := requireIn(fv, f.Type, where+"."+name); err != nil {
				return err
			}
		}
	case reflect.Slice:
		items, ok := v.([]any)
		if !ok || t.Elem().Kind() == reflect.Uint8 {
			return nil
		}
		for i, it := range items {
			if err := requireIn(it, t.Elem(), fmt.Sprintf("%s[%d]", where, i)); err != nil {
				return err
			}
		}
	}
	return nil
}

// payloadOwner returns the client of a client payload path and its file
// name, or ok false for any path outside the allowlist.
func payloadOwner(p string) (client, name string, ok bool) {
	parts := strings.Split(p, "/")
	if len(parts) != 3 || parts[0] != "clients" || !slices.Contains(CaptureClientFiles, parts[2]) {
		return "", "", false
	}
	if _, known := clientDecoders[parts[1]]; !known {
		return "", "", false
	}
	return parts[1], parts[2], true
}

// ClientFile is client id's payload path.
func ClientFile(id, name string) string { return path.Join("clients", id, name) }

// Validate checks the manifest's semantic contract: identity, policy,
// states, sorted allowlisted files within their bounds, null-with-reason
// pairs, stage/file agreement and what a complete capture requires.
func (m *CaptureManifest) Validate() error {
	fail := func(format string, a ...any) error { return fmt.Errorf("capture manifest: "+format, a...) }
	at, err := time.Parse(time.RFC3339, m.CapturedAt)
	switch {
	case m.Schema != CaptureSchema:
		return fail("schema %q, want %q", m.Schema, CaptureSchema)
	case !idPattern.MatchString(m.RunID):
		return fail("run_id %q invalid", m.RunID)
	case err != nil || at.Location() != time.UTC || !strings.HasSuffix(m.CapturedAt, "Z"):
		return fail("captured_at %q is not an RFC 3339 UTC timestamp", m.CapturedAt)
	case m.HarnessVersion == "" || m.Arch == "" || !hex64.MatchString(m.PlanSHA256):
		return fail("harness_version, arch and plan_sha256 are required")
	case m.OS != "linux" && m.OS != "darwin":
		return fail("os %q (linux or darwin)", m.OS)
	case m.RedactionPolicy != CaptureRedactionPolicy:
		return fail("redaction_policy %q, want %q", m.RedactionPolicy, CaptureRedactionPolicy)
	case m.VendorBehavior != VendorNotEvaluated:
		return fail("vendor_behavior %q: a capture never evaluates vendor behavior", m.VendorBehavior)
	case m.State != CaptureComplete && m.State != CapturePartial && m.State != CaptureInterrupted:
		return fail("state %q", m.State)
	case len(m.Clients) == 0 || len(m.Clients) > CaptureMaxClients:
		return fail("%d clients, want 1-%d", len(m.Clients), CaptureMaxClients)
	case m.Cleanup.Failures == nil:
		return fail("cleanup.failures must be a list")
	}
	if err := m.Limits.validate(); err != nil {
		return fail("%v", err)
	}
	files := map[string]EvidenceRef{}
	perClient := map[string]int64{}
	var total int64
	for i, f := range m.Files {
		if i > 0 && m.Files[i-1].Path >= f.Path {
			return fail("files are not sorted by path or %s is duplicated", f.Path)
		}
		if err := checkRelPath("file path", f.Path); err != nil {
			return fail("%v", err)
		}
		client, _, ok := payloadOwner(f.Path)
		if !ok && f.Path != CapturePlanName {
			return fail("file %s is not an allowlisted payload", f.Path)
		}
		if !hex64.MatchString(f.SHA256) || f.Bytes < 0 || f.Bytes > int64(m.Limits.FileBytes) {
			return fail("file %s: bad hash or size", f.Path)
		}
		files[f.Path] = f
		perClient[client] += f.Bytes
		total += f.Bytes
	}
	if _, ok := files[CapturePlanName]; !ok {
		return fail("plan.json is not listed")
	}
	if total > int64(m.Limits.BundleBytes) {
		return fail("the payloads total %d bytes, over the bundle bound %d", total, m.Limits.BundleBytes)
	}
	seen := map[string]bool{}
	allComplete := true
	for i := range m.Clients {
		c := &m.Clients[i]
		if seen[c.ID] {
			return fail("duplicate client %q", c.ID)
		}
		seen[c.ID] = true
		if err := c.validate(files, m.Limits); err != nil {
			return fail("client %q: %v", c.ID, err)
		}
		if perClient[c.ID] > int64(m.Limits.ClientBytes) {
			return fail("client %q: payloads total %d bytes, over the client bound %d", c.ID, perClient[c.ID], m.Limits.ClientBytes)
		}
		allComplete = allComplete && c.State == CaptureComplete
	}
	for p := range files {
		if client, _, ok := payloadOwner(p); ok && !seen[client] {
			return fail("file %s belongs to no recorded client", p)
		}
	}
	switch {
	case m.Plan.Path != CapturePlanName || m.Plan.OmittedLines < 0:
		return fail("plan stream record %+v", m.Plan)
	case m.State == CaptureComplete && (!allComplete || !m.Cleanup.OK || !m.Plan.Clean()):
		return fail("state complete with an incomplete client, a failed cleanup or a cut plan")
	case (m.State == CaptureComplete) != (m.Reason == nil) || m.Reason != nil && *m.Reason == "":
		return fail("state %s needs a reason exactly when it is not complete", m.State)
	}
	if !m.Cleanup.OK && len(m.Cleanup.Failures) == 0 {
		return fail("cleanup failed without a recorded failure")
	}
	return nil
}

func (l CaptureLimitsRecord) validate() error {
	d := DefaultCaptureLimits()
	switch {
	case l.Clients < 1 || l.Clients > CaptureMaxClients || l.SessionsPerClient != CaptureSessionsPerClient:
		return fmt.Errorf("limits: %d clients and %d sessions per client", l.Clients, l.SessionsPerClient)
	case l.SessionMS < 1 || l.SessionMS > CaptureMaxSessionMS || l.ClientMS < 1 || l.ClientMS > CaptureMaxClientMS || l.MetadataMS < 1 || l.MetadataMS > int64(versionWatchdog/time.Millisecond):
		return fmt.Errorf("limits: session %d ms, client %d ms, metadata %d ms exceed the capture caps", l.SessionMS, l.ClientMS, l.MetadataMS)
	case l.StreamBytes < 1 || l.StreamBytes > d.Stream || l.ServerEventsBytes < 1 || l.ServerEventsBytes > d.ServerEvents || l.FileBytes < 1 || l.FileBytes > d.File ||
		l.ClientBytes < 1 || l.ClientBytes > d.Client || l.BundleBytes < 1 || l.BundleBytes > d.Bundle || l.ManifestBytes < 1 || l.ManifestBytes > d.Manifest:
		return fmt.Errorf("limits: a byte bound is outside 1 and its capture cap")
	}
	return nil
}

// nullPair requires a null value to carry a nonempty reason.
func nullPair[T any](name string, v *T, reason *string) error {
	if v == nil && (reason == nil || *reason == "") {
		return fmt.Errorf("%s is null without a reason", name)
	}
	return nil
}

func (c *CaptureClient) validate(files map[string]EvidenceRef, lim CaptureLimitsRecord) error {
	if _, ok := clientDecoders[c.ID]; !ok {
		return fmt.Errorf("unknown client")
	}
	for _, err := range []error{nullPair("observed_version", c.ObservedVersion, c.ObservedVersionReason), nullPair("executable_sha256", c.ExecutableSHA256, c.ExecutableReason),
		nullPair("effort", c.Effort, c.EffortReason), nullPair("probe", c.Probe, c.ProbeReason)} {
		if err != nil {
			return err
		}
	}
	switch {
	case c.ExpectedVersion == "" || c.Model == "" || c.DecoderFixture == "" || c.Decoder != clientDecoders[c.ID]:
		return fmt.Errorf("expected_version, model, decoder and decoder_fixture are required")
	case c.DecoderValidated:
		return fmt.Errorf("decoder_validated: a capture never consults a decoder")
	case c.ExecutableSHA256 != nil && !hex64.MatchString(*c.ExecutableSHA256):
		return fmt.Errorf("executable_sha256 is not a SHA-256")
	case c.CaseID != c.ID+"-capture-setup" || !idPattern.MatchString(c.Nonce):
		return fmt.Errorf("case_id %q or nonce invalid", c.CaseID)
	case c.Config.TimeoutOverride != TimeoutOverrideNone || checkRelPath("config.path", c.Config.Path) != nil:
		return fmt.Errorf("config: path %q and timeout_override %q", c.Config.Path, c.Config.TimeoutOverride)
	case c.State != CaptureComplete && c.State != CapturePartial && c.State != CaptureNotRun:
		return fmt.Errorf("state %q", c.State)
	case c.State != CaptureComplete && (c.Reason == nil || *c.Reason == ""):
		return fmt.Errorf("state %s without a reason", c.State)
	case c.Argv == nil || c.Streams == nil:
		return fmt.Errorf("argv and streams must be lists")
	}
	if c.Probe != nil && c.Probe.Anomalies == nil {
		return fmt.Errorf("probe.anomalies must be a list")
	}
	for _, st := range []struct {
		name  string
		stage CaptureStage
		files []string
	}{{"version", c.Version, []string{FileVersionStdout, FileVersionStderr}}, {"help", c.Help, []string{FileHelpStdout, FileHelpStderr}},
		{"session", c.Session, []string{FileVendorEvents, FileVendorStderr, FileServerEvents}}} {
		s := st.stage
		switch s.State {
		case StageNotRun, StageLaunchFailed, StagePrepared, StageRan:
		default:
			return fmt.Errorf("%s: state %q", st.name, s.State)
		}
		if s.State == StagePrepared && st.name != "session" {
			return fmt.Errorf("%s: state %q", st.name, s.State)
		}
		if s.State != StageRan && (s.Reason == nil || *s.Reason == "") {
			return fmt.Errorf("%s: state %s without a reason", st.name, s.State)
		}
		if (s.State == StageRan) != (s.Cleanup != nil) {
			return fmt.Errorf("%s: a launched stage, and only one, records its cleanup", st.name)
		}
		for _, f := range st.files {
			if _, ok := files[ClientFile(c.ID, f)]; ok != (s.State == StageRan) {
				return fmt.Errorf("%s: file %s listed %v for stage state %s", st.name, f, ok, s.State)
			}
		}
	}
	if _, ok := files[ClientFile(c.ID, FileConfig)]; ok != (c.Session.State != StageNotRun) {
		return fmt.Errorf("config.txt listed %v for session state %s", ok, c.Session.State)
	}
	streams := map[string]bool{}
	for _, s := range c.Streams {
		owner, _, ok := payloadOwner(s.Path)
		if _, listed := files[s.Path]; !ok || owner != c.ID || !listed || streams[s.Path] || s.OmittedLines < 0 {
			return fmt.Errorf("stream %s is not one of the client's listed files, or repeats", s.Path)
		}
		streams[s.Path] = true
		if c.State == CaptureComplete && !s.Clean() {
			return fmt.Errorf("complete with a cut or omitted stream %s", s.Path)
		}
	}
	for p := range files {
		if owner, _, ok := payloadOwner(p); ok && owner == c.ID && !streams[p] {
			return fmt.Errorf("file %s has no stream record", p)
		}
	}
	if c.State == CaptureComplete {
		p := c.Probe
		switch {
		case c.Version.State != StageRan || c.Help.State != StageRan || c.Session.State != StageRan:
			return fmt.Errorf("complete without every stage launched")
		case c.ObservedVersion == nil || *c.ObservedVersion != c.ExpectedVersion || c.ExecutableSHA256 == nil:
			return fmt.Errorf("complete without the exact observed version and executable hash")
		case !cleanStage(c.Version) || !cleanStage(c.Help) || !cleanStage(c.Session):
			return fmt.Errorf("complete without a clean exit 0 of version, help and session (no signal, watchdog, interruption or held stream)")
		case p == nil || !p.Initialized || !p.Intact || p.Receipts != 1 || !p.Completed || len(p.Anomalies) > 0:
			return fmt.Errorf("complete without one matching receipt and completion")
		}
		for _, s := range []CaptureStage{c.Version, c.Help, c.Session} {
			if s.Cleanup == nil || s.Cleanup.Error != nil || !s.Cleanup.GroupGone {
				return fmt.Errorf("complete without proven cleanup")
			}
		}
	}
	return nil
}

// cleanStage is a launched stage that exited 0 by itself: no signal,
// watchdog, interruption or stream held after cleanup (code review C6).
func cleanStage(s CaptureStage) bool {
	return s.State == StageRan && s.Exit != nil && *s.Exit == 0 && s.Signal == nil && !s.Watchdog && !s.Interrupted && !s.StdoutHeld && !s.StderrHeld
}
