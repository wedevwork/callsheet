package mcpqual

// The capture manifest (design decoder-enrollment, Capture evidence): a
// separate operation and schema from qualification's report. A capture
// bundle is the manifest plus allowlisted, sanitized, hash-addressed
// payload files; the manifest lists every payload (never itself) and
// records how each was collected. It asserts nothing about vendor
// behavior: vendor_behavior is always "not_evaluated", and hashes attest
// byte integrity only, not that a human ran the CLI.

import (
	"bytes"
	"encoding/json"
	"errors"
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
	// Approval is the Cursor trusted recipe's approval preparation (design
	// decoder-enrollment B1, FP-13); absent for every other client and in
	// bundles written before it (absence is never retroactive evidence of
	// a scoped approval).
	Approval *CaptureApproval `json:"approval,omitempty"`
}

// CaptureApproval records the one "mcp enable probe" preparation in the
// generated workspace: the fixed command, its stage, the scope decision
// from the before/after fingerprints of the monitored trees, and every
// observed change as a labeled path and kind. Fingerprints, digests and
// file contents never leave memory.
type CaptureApproval struct {
	Argv   []string     `json:"argv"`
	Cwd    string       `json:"cwd"`
	Stage  CaptureStage `json:"stage"`
	Scope  string       `json:"scope"`
	Reason *string      `json:"reason"`
	// InventoryComplete is completeness under the named inventory policy
	// (InventoryPolicy), never over all Cursor data.
	InventoryComplete bool                    `json:"inventory_complete"`
	Changes           []CaptureApprovalChange `json:"changes"`
	// Project is the observed per-project approval's provenance, present
	// exactly for scope project_scoped (design decoder-enrollment B1.5,
	// FP-15): trusted harness provenance, not a vendor attestation.
	Project *CaptureApprovalProject `json:"project,omitempty"`
	// InventoryPolicy and ExcludedPaths name the inventory policy and the
	// two excluded data directories (design decoder-enrollment B1.5,
	// FP-16): present together on every new record, whether or not the
	// directories exist or the scan succeeded; a legacy record omits both
	// and means the original full inventory.
	InventoryPolicy *string  `json:"inventory_policy,omitempty"`
	ExcludedPaths   []string `json:"excluded_paths,omitempty"`
}

// CaptureApprovalProject is the per-project approval provenance: the
// version/platform adapter, the labeled computed project directory and
// its approval file, and the validated probe entry.
type CaptureApprovalProject struct {
	Adapter           string `json:"adapter"`
	Directory         string `json:"directory"`
	File              string `json:"file"`
	ProbeEntryPresent bool   `json:"probe_entry_present"`
}

// projectMembers are CaptureApprovalProject's members.
var projectMembers = []string{"adapter", "directory", "file", "probe_entry_present"}

// CaptureApprovalChange is one observed change: a labeled path
// (<workspace>/..., <home>/.cursor/... or <ancestor-N>/.cursor/...) and
// its kind (added, removed or modified).
type CaptureApprovalChange struct {
	Path string `json:"path"`
	Kind string `json:"kind"`
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
	// Observation is the shared analyzer's observation (design
	// decoder-enrollment B1.5, FP-14): written whenever a new capture
	// analyzes the probe, absent only in legacy records (never null).
	Observation *ProbeObservation `json:"observation,omitempty"`
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
	if err := checkOptionalCaptureMembers(b); err != nil {
		return nil, err
	}
	return &m, m.Validate()
}

// checkOptionalCaptureMembers checks the raw optional extensions of design
// decoder-enrollment B1.5 (FP-14..16): a present probe observation, approval
// project object, inventory policy or excluded path list is never an
// explicit null (only absence denotes a legacy record), and a present
// object has exactly its members, null only where the schema allows it.
func checkOptionalCaptureMembers(b []byte) error {
	var shape struct {
		Clients []struct {
			Probe *struct {
				Observation json.RawMessage `json:"observation"`
			} `json:"probe"`
			Approval *struct {
				Project         json.RawMessage `json:"project"`
				InventoryPolicy json.RawMessage `json:"inventory_policy"`
				ExcludedPaths   json.RawMessage `json:"excluded_paths"`
			} `json:"approval"`
		} `json:"clients"`
	}
	if err := json.Unmarshal(b, &shape); err != nil {
		return fmt.Errorf("capture manifest: %w", err)
	}
	for i, c := range shape.Clients {
		where := fmt.Sprintf("capture manifest: clients[%d]", i)
		if c.Probe != nil {
			if err := checkObservationRaw(c.Probe.Observation, where+".probe.observation"); err != nil {
				return err
			}
		}
		if a := c.Approval; a != nil {
			if err := checkObjectRaw(a.Project, where+".approval.project", projectMembers, nil); err != nil {
				return err
			}
			for name, raw := range map[string]json.RawMessage{"inventory_policy": a.InventoryPolicy, "excluded_paths": a.ExcludedPaths} {
				if raw != nil && string(bytes.TrimSpace(raw)) == "null" {
					return fmt.Errorf("%s.approval.%s: an explicit null (only absence denotes a legacy record)", where, name)
				}
			}
		}
	}
	return nil
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

// CaptureApprovalFiles are the two extra payloads of a launched Cursor
// approval stage (design decoder-enrollment B1, FP-13); no other client
// may carry them.
var CaptureApprovalFiles = []string{FileApprovalStdout, FileApprovalStderr}

// payloadOwner returns the client of a client payload path and its file
// name, or ok false for any path outside the allowlist.
func payloadOwner(p string) (client, name string, ok bool) {
	parts := strings.Split(p, "/")
	if len(parts) != 3 || parts[0] != "clients" {
		return "", "", false
	}
	if !slices.Contains(CaptureClientFiles, parts[2]) && !(parts[1] == "cursor" && slices.Contains(CaptureApprovalFiles, parts[2])) {
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
		if err := c.validateProjectIdentity(m.OS, m.Arch); err != nil {
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
	if err := c.validateObservation(); err != nil {
		return err
	}
	stages := []struct {
		name  string
		stage CaptureStage
		files []string
	}{{"version", c.Version, []string{FileVersionStdout, FileVersionStderr}}, {"help", c.Help, []string{FileHelpStdout, FileHelpStderr}},
		{"session", c.Session, []string{FileVendorEvents, FileVendorStderr, FileServerEvents}}}
	if err := c.validateApproval(); err != nil {
		return err
	}
	if c.Approval != nil {
		stages = append(stages, struct {
			name  string
			stage CaptureStage
			files []string
		}{"approval", c.Approval.Stage, CaptureApprovalFiles})
	} else if c.ID == "cursor" {
		for _, f := range CaptureApprovalFiles {
			if _, ok := files[ClientFile(c.ID, f)]; ok {
				return fmt.Errorf("file %s without an approval record", f)
			}
		}
	}
	for _, st := range stages {
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
		case p == nil || !p.Initialized || !probeEndpointObserved(p) || p.Receipts != 1 || !p.Completed || len(p.Anomalies) > 0:
			return fmt.Errorf("complete without one matching receipt and completion")
		case p.Observation != nil && !p.Observation.CleanSession:
			return fmt.Errorf("complete without a clean-session observation")
		}
		for _, s := range []CaptureStage{c.Version, c.Help, c.Session} {
			if s.Cleanup == nil || s.Cleanup.Error != nil || !s.Cleanup.GroupGone {
				return fmt.Errorf("complete without proven cleanup")
			}
		}
		// A new canonical Cursor capture is complete only after a clean,
		// scoped approval with positive workspace or project evidence.
		if a := c.Approval; a != nil && (!sessionScope(a.Scope) || !provenClean(a.Stage) || !a.InventoryComplete || len(a.Changes) == 0) {
			return fmt.Errorf("complete without a clean workspace-only or project-scoped approval")
		}
	}
	return nil
}

// validateObservation checks a recorded probe observation (design
// decoder-enrollment B1.5, FP-14): its own consistency, intact equal to the
// probe's, an intact end only without anomalies, the terminal observation
// without exit only for one initialized receipt without anomalies, and
// clean_session exactly the attestation the session's recorded stage and
// stream facts give. Absence is a legacy record.
func (c *CaptureClient) validateObservation() error {
	if c.Probe == nil || c.Probe.Observation == nil {
		return nil
	}
	p, o := c.Probe, c.Probe.Observation
	if err := o.validate(); err != nil {
		return fmt.Errorf("probe.%v", err)
	}
	switch {
	case o.Intact != p.Intact:
		return errors.New("probe.observation: intact differs from the probe's intact")
	case o.EndState != EndIncomplete && len(p.Anomalies) > 0:
		return fmt.Errorf("probe.observation: %s with anomalies", o.EndState)
	case o.EndState == EndTerminalWithoutExit && (!p.Initialized || p.Receipts != 1):
		return errors.New("probe.observation: " + EndTerminalWithoutExit + " without one initialized receipt")
	case o.CleanSession != captureSessionClean(c.ID, c.Session, c.Streams):
		return errors.New("probe.observation: clean_session differs from the recorded session stage and streams")
	}
	return nil
}

// validateApproval checks the Cursor approval record: present exactly for
// a Cursor client whose recorded argv asks for --trust (the canonical
// recipe; older bundles have neither), the fixed command and working
// directory, a valid scope consistent with the stage and the changes, and
// sorted, labeled, unique changes. No model session may follow a scope
// other than workspace_only.
func (c *CaptureClient) validateApproval() error {
	a := c.Approval
	trusted := c.ID == "cursor" && slices.Contains(c.Argv, trustFlag)
	switch {
	case a == nil && trusted:
		return errors.New("a trusted cursor recipe without its approval record")
	case a == nil:
		return nil
	case !trusted:
		return errors.New("an approval record belongs only to the trusted cursor recipe")
	case !slices.Equal(a.Argv, CursorApprovalArgv()) || a.Cwd != labelWorkspace:
		return fmt.Errorf("approval: argv %q in %q is not the fixed enable command in the workspace", a.Argv, a.Cwd)
	case a.Changes == nil:
		return errors.New("approval: changes must be a list")
	case a.Reason != nil && *a.Reason == "":
		return errors.New("approval: an empty reason")
	case sessionScope(a.Scope) == (a.Reason != nil):
		return fmt.Errorf("approval: scope %s needs a reason exactly when it is neither workspace_only nor project_scoped", a.Scope)
	case (a.Project != nil) != (a.Scope == ScopeProjectScoped):
		return errors.New("approval: a project record belongs exactly to scope project_scoped")
	}
	if err := a.validatePolicy(); err != nil {
		return err
	}
	outside, unattributed := false, false
	for i, ch := range a.Changes {
		switch {
		case !validChangePath(ch.Path):
			return fmt.Errorf("approval: change path %q is not a labeled monitored path", ch.Path)
		case ch.Kind != ChangeAdded && ch.Kind != ChangeRemoved && ch.Kind != ChangeModified:
			return fmt.Errorf("approval: change kind %q", ch.Kind)
		case i > 0 && a.Changes[i-1].Path >= ch.Path:
			return fmt.Errorf("approval: changes are not sorted or %s repeats", ch.Path)
		}
		outside = outside || !inWorkspace(ch.Path)
		unattributed = unattributed || !inWorkspace(ch.Path) && !inHomeProjects(ch.Path)
	}
	ran := a.Stage.State == StageRan
	switch a.Scope {
	case ScopeNotChecked:
		if ran || len(a.Changes) > 0 || a.InventoryComplete {
			return errors.New("approval: not_checked after a launched command, with changes or a complete inventory")
		}
	case ScopeUnverifiable:
		// A home-project change may stay unverified (an unsupported
		// derivation, a lone directory or an unproven approval file); any
		// other outside change is outside_workspace.
		if !ran && (len(a.Changes) > 0 || a.InventoryComplete) || unattributed {
			return errors.New("approval: unverifiable with changes but no command, or with an outside change")
		}
	case ScopeOutside:
		if !ran || !outside {
			return errors.New("approval: outside_workspace without a launched command and an outside change")
		}
	case ScopeWorkspaceOnly:
		if !provenClean(a.Stage) || !a.InventoryComplete || len(a.Changes) == 0 {
			return errors.New("approval: workspace_only without a clean command, proven cleanup, complete inventories and a change")
		}
		for _, ch := range a.Changes {
			if !inWorkspaceCursor(ch.Path) {
				return fmt.Errorf("approval: workspace_only with a change outside <workspace>/.cursor/: %s", ch.Path)
			}
		}
	case ScopeProjectScoped:
		if err := a.validateProject(); err != nil {
			return err
		}
	default:
		return fmt.Errorf("approval: scope %q", a.Scope)
	}
	if !sessionScope(a.Scope) && c.Session.State == StageRan {
		return fmt.Errorf("approval: a model session ran after scope %s", a.Scope)
	}
	return nil
}

// sessionScope is a scope after which the model session may start:
// workspace_only, or project_scoped (design decoder-enrollment B1.5,
// FP-15).
func sessionScope(scope string) bool {
	return scope == ScopeWorkspaceOnly || scope == ScopeProjectScoped
}

// validatePolicy checks the inventory policy fields (design
// decoder-enrollment B1.5, FP-16): both present with the named policy and
// exactly the two excluded paths in order, or both absent (a legacy full
// inventory); a project-scoped record always names the policy.
func (a *CaptureApproval) validatePolicy() error {
	switch {
	case (a.InventoryPolicy == nil) != (a.ExcludedPaths == nil):
		return errors.New("approval: inventory_policy and excluded_paths are present together or not at all")
	case a.InventoryPolicy == nil && a.Scope == ScopeProjectScoped:
		return errors.New("approval: project_scoped without the inventory policy")
	case a.InventoryPolicy == nil:
		return nil
	case *a.InventoryPolicy != InventoryPolicyV2 || !slices.Equal(a.ExcludedPaths, InventoryExcludedPaths()):
		return fmt.Errorf("approval: inventory policy %q with excluded paths %q, want %q with %q", *a.InventoryPolicy, a.ExcludedPaths, InventoryPolicyV2, InventoryExcludedPaths())
	}
	return nil
}

// validateProjectIdentity binds a project_scoped record to the one
// adapter's identity in its enclosing records (code review B1.5 round 1,
// C4): the manifest's platform is linux/amd64 and the client's expected
// and observed versions are exactly 2026.10.01-e373342. Every reader of a
// manifest (bundle validation and enrollment included) applies it.
func (c *CaptureClient) validateProjectIdentity(goos, goarch string) error {
	if c.Approval == nil || c.Approval.Scope != ScopeProjectScoped {
		return nil
	}
	if goos != cursorProjectGOOS || goarch != cursorProjectGOARCH || c.ExpectedVersion != cursorProjectVersion || c.ObservedVersion == nil || *c.ObservedVersion != cursorProjectVersion {
		return fmt.Errorf("approval: project_scoped provenance of %s on %s/%s with version %q, not the %s adapter's identity", c.ID, goos, goarch, c.ExpectedVersion, CursorProjectAdapter)
	}
	return nil
}

// validateProject checks a project_scoped record: a clean command with
// proven cleanup, complete inventories, the exact adapter provenance with
// the validated probe entry, both the project directory and its approval
// file recorded as added, and every other change a workspace .cursor
// change.
func (a *CaptureApproval) validateProject() error {
	p := a.Project
	if !provenClean(a.Stage) || !a.InventoryComplete {
		return errors.New("approval: project_scoped without a clean command, proven cleanup and complete inventories")
	}
	if p.Adapter != CursorProjectAdapter || p.Directory != labelProjectDir || p.File != labelProjectFile || !p.ProbeEntryPresent {
		return fmt.Errorf("approval: project provenance %+v is not the %s adapter's validated %s", *p, CursorProjectAdapter, labelProjectFile)
	}
	dir, file := false, false
	for _, ch := range a.Changes {
		switch {
		case ch.Path == labelProjectDir && ch.Kind == ChangeAdded:
			dir = true
		case ch.Path == labelProjectFile && ch.Kind == ChangeAdded:
			file = true
		case !inWorkspaceCursor(ch.Path):
			return fmt.Errorf("approval: project_scoped with a forbidden change %s (%s)", ch.Path, ch.Kind)
		}
	}
	if !dir || !file {
		return errors.New("approval: project_scoped without both the project directory and its approval file added")
	}
	return nil
}

// provenClean is a clean stage whose recorded cleanup proved the group gone
// without error; a missing (null) cleanup record is never proof (code
// review B1 round 1, C1: it is checked, never dereferenced).
func provenClean(s CaptureStage) bool {
	return cleanStage(s) && s.Cleanup != nil && s.Cleanup.Error == nil && s.Cleanup.GroupGone
}

// cleanStage is a launched stage that exited 0 by itself: no signal,
// watchdog, interruption or stream held after cleanup (code review C6).
func cleanStage(s CaptureStage) bool {
	return s.State == StageRan && s.Exit != nil && *s.Exit == 0 && s.Signal == nil && !s.Watchdog && !s.Interrupted && !s.StdoutHeld && !s.StderrHeld
}
