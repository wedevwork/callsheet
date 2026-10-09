package mcpqual

import (
	"bytes"
	"encoding/json"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"testing/fstest"
)

// Design decoder-enrollment B3, UT-23 (FP-23): the Cursor export policy
// decoder-enrollment-b3-cursor-metadata-v2 over a tiny fabricated Cursor
// capture: every replacement row with its type, absence and determinism;
// protected spans (the opaque LF-bearing call ID, the structured result
// and nonce, the probe events) unchanged; policy/source binding and
// cross-policy refusal; oracle and provenance corruption with empty and
// placeholder attestations; and the B2 policy left unchanged.

// cursorExportID is the fabricated call ID as it appears inside decoded
// JSON data: the two characters backslash and n, a JSON escape of one LF.
const cursorExportID = `call-x-0\nfc_x_0`

// cursorExportRecords are the fabricated Cursor records (decoded data):
// canary cv in every replaced value, res the escaped structured result.
func cursorExportRecords(cv, res string) []string {
	sess := `"session_id":"` + cv + `"`
	args := `{"name":"probe-slow","args":{"case_id":"cursor-capture-setup"},"toolCallId":"` + cursorExportID +
		`","providerIdentifier":"probe","toolName":"slow","smartModeApprovalOnly":false,"skipApproval":false,"serverIdentifier":"probe"}`
	tool := func(sub, result, completed string) string {
		return `{"type":"tool_call","subtype":"` + sub + `","call_id":"` + cursorExportID + `","tool_call":{"mcpToolCall":{"args":` + args + result +
			`,"description":"Run the slow probe"},"hookAdditionalContexts":[],"toolCallId":"` + cursorExportID + `","startedAtMs":"` + cv + `"` + completed +
			`},"model_call_id":"` + cv + `",` + sess + `,"timestamp_ms":1791494828514}`
	}
	return []string{
		`{"type":"system","subtype":"init","apiKeySource":"[REDACTED]","cwd":"` + cv + `",` + sess + `,"model":"Grok 4.7 256K Low","permissionMode":"default"}`,
		`{"type":"user","message":{"role":"user","content":[{"type":"text","text":"Call the MCP tool slow exactly once"}]},` + sess + `}`,
		`{"type":"thinking","subtype":"delta","text":"` + cv + `",` + sess + `,"timestamp_ms":1791494826710}`,
		`{"type":"thinking","subtype":"completed",` + sess + `,"timestamp_ms":1791494828467}`,
		`{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"calling ` + cv + `"}]},` + sess + `,"model_call_id":"` + cv + `","timestamp_ms":1791494828514}`,
		tool("started", "", ""),
		tool("completed", `,"result":{"success":{"content":[{"text":{"text":"`+res+`"}}],"isError":false,"systemReminders":[]}}`, `,"completedAtMs":"`+cv+`"`),
		`{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"` + res + ` ` + cv + `"}]},` + sess + `}`,
		`{"type":"result","subtype":"success","duration_ms":7113,"duration_api_ms":7113,"is_error":false,"result":"` + res + ` ` + cv + `",` + sess +
			`,"request_id":"` + cv + `","usage":{"inputTokens":28749,"outputTokens":321}}`,
	}
}

// cursorRowPointers are the fabricated transcript's applicable B3 rows,
// by record (apiKeySource is not a row: r0.13 post-final clarification Q1).
var cursorRowPointers = [][]string{
	{"/cwd", "/session_id"},
	{"/session_id"},
	{"/session_id", "/text", "/timestamp_ms"},
	{"/session_id", "/timestamp_ms"},
	{"/message/content/0/text", "/model_call_id", "/session_id", "/timestamp_ms"},
	{"/model_call_id", "/session_id", "/timestamp_ms", "/tool_call/startedAtMs"},
	{"/model_call_id", "/session_id", "/timestamp_ms", "/tool_call/completedAtMs", "/tool_call/startedAtMs"},
	{"/message/content/0/text", "/session_id"},
	{"/request_id", "/result", "/session_id"},
}

func TestCursorFixturePolicyRows(t *testing.T) {
	sc, pol := exportSource(t, "cursor")
	root := realDir(t)
	san, err := ExportFixture(sc.dir, filepath.Join(root, "a"), pol)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ExportFixture(sc.dir, filepath.Join(root, "b"), pol); err != nil {
		t.Fatal(err)
	}
	a, b := readTree(t, filepath.Join(root, "a")), readTree(t, filepath.Join(root, "b"))
	if len(a) != len(b) {
		t.Fatal("two exports differ in their files")
	}
	for p := range a {
		if !bytes.Equal(a[p], b[p]) {
			t.Fatalf("%s differs between two exports", p)
		}
	}
	if san.Policy != SanitizationPolicyB3Cursor {
		t.Fatalf("policy %q", san.Policy)
	}
	vendor := ClientFile("cursor", FileVendorEvents)
	var got [][]string
	for _, r := range san.Replacements {
		if r.Path != vendor || string(r.Replacement) != `"`+FixtureMetadataValue+`"` {
			t.Fatalf("replacement %+v", r)
		}
		for len(got) <= r.Record {
			got = append(got, nil)
		}
		got[r.Record] = append(got[r.Record], r.Pointer)
	}
	if !slices.EqualFunc(got, cursorRowPointers, slices.Equal[[]string]) {
		t.Fatalf("replacements %v", got)
	}
	exp := string(a[vendor])
	src := string(sc.bundle.Files[vendor])
	switch {
	case strings.Contains(exp, exportCanary) || !strings.Contains(src, exportCanary):
		t.Fatal("a canary survived the export")
	case strings.Count(exp, `call-x-0\\nfc_x_0`) != 6:
		// Three copies in each of the two tool_call records.
		t.Fatalf("the opaque call ID is not kept in all three copies of both records:\n%s", exp)
	case !strings.Contains(exp, `\"apiKeySource\":\"[REDACTED]\"`):
		t.Fatal("apiKeySource is not kept as the capture policy's [REDACTED]")
	case !strings.Contains(exp, `\"timestamp_ms\":\"[fixture metadata]\"`) || strings.Contains(exp, `1791494828514`):
		t.Fatal("an integer timestamp was not replaced by the metadata string")
	case strings.Count(exp, `noncecap1`) != 1:
		t.Fatalf("the structured nonce is not exactly the one protected copy:\n%s", exp)
	case !strings.Contains(exp, `\"duration_ms\":7113`) || !strings.Contains(exp, `\"inputTokens\":28749`) || !strings.Contains(exp, `Call the MCP tool slow exactly once`):
		t.Fatal("usage, durations or user text changed")
	}
	// Offsets, order and count, and every other payload, unchanged.
	st, _ := ReplayTranscript(sc.bundle.Files[vendor])
	et, _ := ReplayTranscript(a[vendor])
	if len(st.Lines) != len(et.Lines) {
		t.Fatal("record count changed")
	}
	for i := range st.Lines {
		if st.Lines[i].OffsetNS != et.Lines[i].OffsetNS {
			t.Fatalf("record %d offset changed", i)
		}
	}
	for p, d := range sc.bundle.Files {
		if p != vendor && !bytes.Equal(d, a[p]) {
			t.Fatalf("%s changed", p)
		}
	}
	// The exported transcript decodes to the same events as the source.
	if !equalDecoded(decodeCursorReal(st), decodeCursorReal(et)) || decodeCursorReal(et).Inconclusive != "" {
		t.Fatalf("decoding changed: %+v / %+v", decodeCursorReal(st), decodeCursorReal(et))
	}
	bb, err := ValidateCaptureBundle(os.DirFS(filepath.Join(root, "a")), ".", SanitizationName)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := CheckFixtureSanitization(pol, bb, a[SanitizationName]); err != nil {
		t.Fatal(err)
	}
}

// equalDecoded compares two decodes.
func equalDecoded(x, y Decoded) bool {
	bx, _ := json.Marshal(x)
	by, _ := json.Marshal(y)
	return bytes.Equal(bx, by)
}

func TestCursorFixturePolicyRefusals(t *testing.T) {
	recs := cursorExportRecords("v", `{\"case_id\":\"c\",\"nonce\":\"n\"}`)
	edits := func(data string) ([]string, error) {
		_, es, _, err := recordEdits("cursor", []byte(data))
		var ps []string
		for _, e := range es {
			ps = append(ps, e.pointer)
		}
		slices.Sort(ps)
		return ps, err
	}
	// Absent members are never created; an already neutral value (the
	// exported form, an integer timestamp's string included) is accepted.
	for _, tc := range []struct {
		data string
		want []string
	}{
		{`{"type":"thinking","subtype":"completed"}`, nil},
		{`{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"x"}]}}`, []string{"/message/content/0/text"}},
		{`{"type":"thinking","subtype":"completed","session_id":"[fixture metadata]","timestamp_ms":"[fixture metadata]"}`, []string{"/session_id", "/timestamp_ms"}},
		{`{"type":"system","subtype":"init","apiKeySource":"[REDACTED]"}`, nil},
	} {
		if got, err := edits(tc.data); err != nil || !slices.Equal(got, tc.want) {
			t.Errorf("%s: %v %v", tc.data, got, err)
		}
	}
	for name, tc := range map[string]struct{ data, want string }{
		"timestamp-float":      {strings.Replace(recs[2], `"timestamp_ms":1791494826710`, `"timestamp_ms":1.5`, 1), "/timestamp_ms has an unexpected type"},
		"timestamp-exp":        {strings.Replace(recs[2], `"timestamp_ms":1791494826710`, `"timestamp_ms":1e3`, 1), "/timestamp_ms has an unexpected type"},
		"timestamp-string":     {strings.Replace(recs[2], `"timestamp_ms":1791494826710`, `"timestamp_ms":"1791494826710"`, 1), "/timestamp_ms has an unexpected type"},
		"session-number":       {strings.Replace(recs[1], `"session_id":"v"`, `"session_id":1`, 1), "/session_id has an unexpected type"},
		"session-null":         {strings.Replace(recs[1], `"session_id":"v"`, `"session_id":null`, 1), "/session_id has an unexpected type"},
		"cwd-array":            {strings.Replace(recs[0], `"cwd":"v"`, `"cwd":[]`, 1), "/cwd has an unexpected type"},
		"started-number":       {strings.Replace(recs[5], `"startedAtMs":"v"`, `"startedAtMs":1`, 1), "/tool_call/startedAtMs has an unexpected type"},
		"text-object":          {strings.Replace(recs[2], `"text":"v"`, `"text":{}`, 1), "/text has an unexpected type"},
		"request-number":       {strings.Replace(recs[8], `"request_id":"v"`, `"request_id":2`, 1), "/request_id has an unexpected type"},
		"unknown-member":       {strings.Replace(recs[3], `"subtype":"completed"`, `"subtype":"completed","host":"owner-box"`, 1), "unreviewed member"},
		"unknown-tool-member":  {strings.Replace(recs[5], `"startedAtMs"`, `"cwd":"x","startedAtMs"`, 1), "unreviewed member"},
		"completed-on-started": {strings.Replace(recs[5], `"startedAtMs":"v"`, `"startedAtMs":"v","completedAtMs":"v"`, 1), "unreviewed member"},
		"unknown-record":       {`{"type":"interaction_query","session_id":"v"}`, "not one of the recognized Cursor records"},
		"unknown-subtype":      {`{"type":"thinking","subtype":"other"}`, "not one of the recognized Cursor records"},
		"result-error":         {`{"type":"result","subtype":"error","is_error":true}`, "not one of the recognized Cursor records"},
		"typed-type":           {`{"type":1}`, "not one of the recognized Cursor records"},
		"not-object":           {`[]`, "not a JSON object"},
		"two-blocks":           {strings.Replace(recs[7], `"content":[{`, `"content":[{"type":"text","text":"a"},{`, 1), "one text block"},
		"assistant-role":       {strings.Replace(recs[7], `"role":"assistant"`, `"role":"user"`, 1), "one text block"},
		"block-type":           {strings.Replace(recs[7], `"type":"text"`, `"type":"image"`, 1), "one text block"},
		"block-member":         {strings.Replace(recs[7], `{"type":"text",`, `{"type":"text","x":1,`, 1), "unreviewed member"},
		"message-member":       {strings.Replace(recs[7], `"role":"assistant",`, `"role":"assistant","id":"m",`, 1), "unreviewed member"},
		"embedded-duplicate":   {strings.Replace(recs[6], `{\"case_id\":\"c\",`, `{\"case_id\":\"c\",\"case_id\":\"c\",`, 1), "embedded result that is not strict JSON"},
		"embedded-not-json":    {strings.Replace(recs[6], `{\"case_id\":\"c\",\"nonce\":\"n\"}`, `nonce n`, 1), "embedded result that is not strict JSON"},
	} {
		if _, err := edits(tc.data); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: %v", name, err)
		}
	}
	// No edit ever touches the protected tool call or its IDs.
	root, es, prot, err := recordEdits("cursor", []byte(recs[6]))
	if err != nil || len(prot) == 0 {
		t.Fatal(err)
	}
	mcp := root.member("tool_call").member("mcpToolCall")
	for _, e := range es {
		if e.span.start < mcp.end && mcp.start < e.span.end {
			t.Fatalf("%s touches the structured call", e.pointer)
		}
	}
}

func TestCursorFixturePolicyBinding(t *testing.T) {
	// Production: exactly four sources, each bound to its client's policy;
	// the Cursor pin is the inspected source manifest.
	prod := ProductionFixturePolicy()
	if len(prod.sources) != 4 {
		t.Fatalf("%d production sources", len(prod.sources))
	}
	for _, s := range prod.sources {
		if s.Policy != fixturePolicyFor(s.Client) || s.Platform != EnrolledRealPlatform {
			t.Fatalf("source %+v", s)
		}
	}
	if s, ok := prod.source("cursor", CursorRealVersion, EnrolledRealPlatform, "20261008T212659Z-a5af9a"); !ok || s.Policy != SanitizationPolicyB3Cursor ||
		s.ManifestSHA256 != "b3e57897b6242029f873a738569f6ad124e3ffe6f0c3406feeb8c716bd6a7a2f" {
		t.Fatalf("cursor pin %+v", s)
	}
	if fixturePolicyFor("codex") != SanitizationPolicyB2 || fixturePolicyFor("cursor") != SanitizationPolicyB3Cursor {
		t.Fatal("policy binding")
	}
	// Cross-policy substitution: a Cursor source bound to the B2 policy is
	// never accepted, and a receipt naming the other policy fails.
	sc, pol := exportSource(t, "cursor")
	wrong := pol.sources[0]
	wrong.Policy = SanitizationPolicyB2
	if _, err := ExportFixture(sc.dir, filepath.Join(realDir(t), "x"), &FixturePolicy{sources: []FixtureSource{wrong}, placement: pol.placement}); err == nil ||
		!strings.Contains(err.Error(), "not an accepted source identity") {
		t.Fatalf("a B2-bound Cursor source: %v", err)
	}
	b, sb, cpol, _ := exportedBundle(t, "cursor")
	swapped := bytes.Replace(sb, []byte(SanitizationPolicyB3Cursor), []byte(SanitizationPolicyB2), 1)
	if _, err := CheckFixtureSanitization(cpol, b, swapped); err == nil || !strings.Contains(err.Error(), "bound to this source") {
		t.Fatalf("a Cursor receipt under the B2 policy: %v", err)
	}
	gb, gsb, gpol, _ := exportedBundle(t, "grok")
	if _, err := CheckFixtureSanitization(gpol, gb, bytes.Replace(gsb, []byte(SanitizationPolicyB2), []byte(SanitizationPolicyB3Cursor), 1)); err == nil ||
		!strings.Contains(err.Error(), "bound to this source") {
		t.Fatalf("a B2 receipt under the Cursor policy: %v", err)
	}
	// Policy v1 is never a Cursor policy.
	for _, v1 := range []string{"decoder-enrollment-b3-cursor-metadata-v1", "decoder-enrollment-b2-metadata-v1"} {
		if _, err := ParseFixtureSanitization(bytes.Replace(sb, []byte(SanitizationPolicyB3Cursor), []byte(v1), 1)); err == nil {
			t.Fatalf("receipt policy %s parsed", v1)
		}
	}
	// Under the B2 rules nothing of a Cursor record is a row: the B2 policy
	// is unchanged and never applies to Cursor.
	if _, es, _, err := recordEdits("codex", []byte(`{"type":"tool_call","session_id":"x","cwd":"y"}`)); err != nil || len(es) != 0 {
		t.Fatalf("B2 rules on a Cursor-shaped record: %v %v", es, err)
	}
}

// cursorSanitizedRepo is a fake repository enrolling a sanitized Cursor
// export with attestation a under the injected policy, with a registry
// holding the production Cursor version (its real parser) only.
func cursorSanitizedRepo(t *testing.T, a Attestation) (fstest.MapFS, EnrollmentOptions, EnrollmentEntry) {
	t.Helper()
	b, sb, pol, _ := exportedBundle(t, "cursor")
	m := b.Manifest
	c := m.Clients[0]
	tr, _ := ReplayTranscript(b.Files[ClientFile("cursor", FileVendorEvents)])
	var call, res int64
	for _, l := range tr.Lines {
		switch {
		case bytes.Contains(l.Data, []byte(`"subtype":"started"`)):
			call = l.OffsetNS
		case bytes.Contains(l.Data, []byte(`"subtype":"completed","call_id"`)):
			res = l.OffsetNS
		}
	}
	id := "call-x-0" + string(rune(10)) + "fc_x_0"
	policy := SanitizationPolicyB3Cursor
	o := &ExpectedOracle{CaseID: c.CaseID, Nonce: c.Nonce,
		Events:   []Event{{CaseID: c.CaseID, Kind: KindToolCall, OffsetNS: call, RequestID: id}, {CaseID: c.CaseID, Kind: KindToolResult, OffsetNS: res, RequestID: id, Nonce: c.Nonce}},
		Terminal: true, ClientInfo: ExpectedClientInfo{Name: *c.Probe.ClientName, Version: *c.Probe.ClientVersion}, RequesterCompatible: true,
		ErrorKinds: []string{}, Capabilities: []string{CapToolCall, CapToolResult, CapTerminalSuccess}, CaptureState: CaptureComplete,
		SourceCaptureSHA256: pol.sources[0].ManifestSHA256, SanitizationPolicy: &policy, Attestation: a}
	ob, _ := encodeIndent(o)
	fixture := EnrolledFixtureID("cursor-jsonl", CursorRealVersion, "linux/amd64")
	dir := EnrolledBundlePath("cursor", CursorRealVersion, "linux/amd64", m.RunID)
	ss := sha256Hex(sb)
	e := EnrollmentEntry{Client: "cursor", Decoder: "cursor-jsonl", Version: CursorRealVersion, Platform: "linux/amd64", Fixture: fixture, Bundle: dir,
		ManifestSHA256: sha256Hex(b.ManifestBytes), ExpectedSHA256: sha256Hex(ob), SanitizationSHA256: &ss}
	idx, _ := encodeIndent(EnrollmentIndex{Schema: EnrollmentSchema, Entries: []EnrollmentEntry{e}})
	fsys := fstest.MapFS{EnrollmentIndexPath: {Data: idx, Mode: 0o600}, path.Join(dir, CaptureManifestName): {Data: b.ManifestBytes, Mode: 0o600},
		path.Join(dir, ExpectedName): {Data: ob, Mode: 0o600}, path.Join(dir, SanitizationName): {Data: sb, Mode: 0o600}}
	for p, d := range b.Files {
		fsys[path.Join(dir, p)] = &fstest.MapFile{Data: d, Mode: 0o600}
	}
	reg := SyntheticRegistry()
	reg["cursor-jsonl"] = DefaultRegistry()["cursor-jsonl"]
	return fsys, EnrollmentOptions{FS: fsys, Registry: reg, policy: pol}, e
}

func TestCursorFixtureEnrollmentGate(t *testing.T) {
	ok := Attestation{Owner: B2OwnerHandle, Reviewer: B2ReviewerHandle, Policy: CaptureRedactionPolicy}
	fsys, opts, e := cursorSanitizedRepo(t, ok)
	if _, err := ValidateEnrollment(opts); err != nil {
		t.Fatalf("the structural rule refused the exact handles: %v", err)
	}
	// Empty and placeholder attestations never validate a Cursor entry.
	for name, a := range map[string]Attestation{
		"pending":       {Policy: CaptureRedactionPolicy},
		"reviewer-only": {Reviewer: B2ReviewerHandle, Policy: CaptureRedactionPolicy},
		"owner-only":    {Owner: B2OwnerHandle, Policy: CaptureRedactionPolicy},
		"placeholder":   {Owner: "<owner>", Reviewer: "<reviewer>", Policy: CaptureRedactionPolicy},
		"todo":          {Owner: "TODO", Reviewer: "TBD", Policy: CaptureRedactionPolicy},
	} {
		_, opts, _ := cursorSanitizedRepo(t, a)
		if _, err := ValidateEnrollment(opts); err == nil || !strings.Contains(err.Error(), "attestation") {
			t.Errorf("%s: a pending or placeholder Cursor candidate validated: %v", name, err)
		}
	}
	dir := e.Bundle
	rewrite := func(mutate func(f fstest.MapFS)) EnrollmentOptions {
		f := fstest.MapFS{}
		for k, v := range fsys {
			cp := *v
			f[k] = &cp
		}
		mutate(f)
		o := opts
		o.FS = f
		return o
	}
	oracle := func(f fstest.MapFS, mut func(m map[string]any)) {
		var m map[string]any
		json.Unmarshal(f[path.Join(dir, ExpectedName)].Data, &m)
		mut(m)
		b, _ := encodeIndent(m)
		f[path.Join(dir, ExpectedName)] = &fstest.MapFile{Data: b, Mode: 0o600}
		en := e
		en.ExpectedSHA256 = sha256Hex(b)
		ib, _ := encodeIndent(EnrollmentIndex{Schema: EnrollmentSchema, Entries: []EnrollmentEntry{en}})
		f[EnrollmentIndexPath] = &fstest.MapFile{Data: ib, Mode: 0o600}
	}
	ev := func(m map[string]any, i int) map[string]any { return m["events"].([]any)[i].(map[string]any) }
	for name, tc := range map[string]struct {
		mutate func(f fstest.MapFS)
		want   string
	}{
		"b2-policy": {func(f fstest.MapFS) {
			oracle(f, func(m map[string]any) { m["sanitization_policy"] = SanitizationPolicyB2 })
		}, "sanitization_policy"},
		"no-policy": {func(f fstest.MapFS) { oracle(f, func(m map[string]any) { delete(m, "sanitization_policy") }) }, "sanitization_policy"},
		"source-hash": {func(f fstest.MapFS) {
			oracle(f, func(m map[string]any) { m["source_capture_sha256"] = strings.Repeat("e", 64) })
		}, "source manifest hash"},
		"id-without-lf": {func(f fstest.MapFS) { oracle(f, func(m map[string]any) { ev(m, 0)["request_id"] = "call-x-0fc_x_0" }) }, "replay"},
		"result-offset": {func(f fstest.MapFS) { oracle(f, func(m map[string]any) { ev(m, 1)["monotonic_offset"] = 1 }) }, "replay"},
		"nonce":         {func(f fstest.MapFS) { oracle(f, func(m map[string]any) { ev(m, 1)["nonce"] = "other" }) }, "not a successful setup's tool event"},
		"error-capability": {func(f fstest.MapFS) {
			oracle(f, func(m map[string]any) { m["capabilities"] = []string{"tool_call", "permission_denied"} })
		}, "capability"},
		"inconclusive": {func(f fstest.MapFS) { oracle(f, func(m map[string]any) { m["inconclusive"] = "x" }) }, "inconclusive"},
		"protected": {func(f fstest.MapFS) {
			p := path.Join(dir, ClientFile("cursor", FileVendorEvents))
			f[p] = &fstest.MapFile{Data: bytes.ReplaceAll(f[p].Data, []byte(`noncecap1`), []byte(`noncecap2`)), Mode: 0o600}
		}, "differ from the manifest"},
	} {
		if _, err := ValidateEnrollment(rewrite(tc.mutate)); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: %v", name, err)
		}
	}
	// Legacy synthetic, other-platform and unsupported-capability refusals:
	// a Linux Cursor fixture never qualifies Darwin or an error capability.
	v, _ := DefaultRegistry().Versions("cursor-jsonl"), 0
	q := v[len(v)-1]
	if len(q.MissingCapabilities("darwin/arm64", RequiredCapabilities(KindToolResult)...)) == 0 ||
		len(q.MissingCapabilities("linux/amd64", RequiredCapabilities(KindPermissionDenied)...)) == 0 ||
		len(q.MissingCapabilities("linux/amd64", RequiredCapabilities(KindToolResult)...)) != 0 || v[0].Qualified {
		t.Fatalf("cursor capabilities %+v", v)
	}
}
