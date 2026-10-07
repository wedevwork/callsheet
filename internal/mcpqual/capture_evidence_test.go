package mcpqual

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"testing/fstest"
)

// Design decoder-enrollment UT-3: the capture bundle (manifest round trip
// and every corruption), redaction before any outer JSON or hash, the
// byte bounds at N and N+1 with injected small limits, expansion overflow,
// I/O failures and the RedactionFixedPoint guard.

const (
	planSecret    = "plan-secret-value-0123"
	inheritSecret = "inherited-tok-value"
	shortSecret   = "Zq9"
)

// secretScript is a claude session whose output carries every secret
// shape: plan, inherited and short secrets, the home and workspace paths,
// a bearer token and a nested escaped credential, plus the public nonce.
// Planted values of the help and stderr (design decoder-enrollment r0.3):
// redacted in place while the credential-word prose around them is kept.
const (
	helpPlanted   = "planted-help-value-1"
	stderrPlanted = "planted-stderr-1"
	helpText      = "usage: fake [options]\n  --api-key <KEY>  API key for auth, e.g. API_KEY=" + helpPlanted + "\n  uses a token here\n  --header 'Authorization: Bearer helpbearer123'\n"
	stderrText    = "plain note\nError: authentication required; token=" + stderrPlanted + " via https://user:pw-planted@host/x\n"
)

func secretScript(w *capWorld, omit bool) func(ProcSpec) capBehavior {
	return func(spec ProcSpec) capBehavior {
		b := w.defaults(spec)
		if launchKind(spec) == "help" {
			b.stdout = helpText
		}
		if launchKind(spec) != "session" {
			return b
		}
		nested, _ := json.Marshal(map[string]any{"text": `{"api_key":"` + planSecret + `","note":"keep"}`})
		lines := []string{
			`{"type":"tool_result","nonce":"noncecap1","case_id":"claude-capture-setup","path":"` + spec.Dir + `/x","home":"/home/owner/.claude"}`,
			`{"type":"note","auth":"Bearer abcdefghijklmnop","short":"a` + shortSecret + `b","env":"` + inheritSecret + `"}`,
			string(nested),
		}
		if omit {
			// An unsafe line: a cut, escaped credential member.
			lines = append(lines, `{"password":"prefix\"`+planSecret)
		}
		b.stdout = strings.Join(lines, "\n") + "\n"
		b.stderr = stderrText
		return b
	}
}

func secretPlan(t *testing.T) *Plan {
	return mustCapturePlan(t, filledPlan(t, func(c map[string]any) {
		env := c["env"]
		if env == nil {
			env = map[string]any{}
		}
		env.(map[string]any)["VENDOR_API_KEY"] = planSecret
		c["env"] = env
	}, "claude"))
}

// secretCapture is the shared complete capture of secretScript.
func secretCapture(t *testing.T) *sharedCapture {
	return cachedCapture(t, "secrets", func(w *capWorld) *CaptureRunner {
		w.script = secretScript(w, false)
		return newCapRunner(t, w, secretPlan(t))
	})
}

func TestCaptureEvidence(t *testing.T) {
	sc := secretCapture(t)
	c, man, b := sc.runner, sc.man, sc.bundle
	cc := capClient(t, man, "claude")
	if man.State != CaptureComplete || man.Schema != CaptureSchema || man.RedactionPolicy != CaptureRedactionPolicy || man.VendorBehavior != VendorNotEvaluated ||
		man.OS != "linux" || man.Arch != "amd64" || man.PlanSHA256 != sha256Hex(b.Files[CapturePlanName]) || man.CapturedAt != "2026-09-29T10:00:00Z" || cc.DecoderValidated {
		t.Fatalf("manifest %+v", man)
	}
	if len(b.Files) != 9 || len(cc.Streams) != 8 {
		t.Fatalf("payloads %d, streams %d", len(b.Files), len(cc.Streams))
	}
	all := append([]byte(nil), b.ManifestBytes...)
	for p, data := range b.Files {
		all = append(all, data...)
		info, err := os.Lstat(filepath.Join(c.OutDir, filepath.FromSlash(p)))
		if err != nil || info.Mode().Perm() != 0o600 {
			t.Fatalf("%s mode %v", p, info.Mode())
		}
	}
	if info, _ := os.Stat(c.OutDir); info.Mode().Perm() != 0o700 {
		t.Fatalf("out mode %v", info.Mode())
	}
	// Even after decoding every wrapper and escaped string, no secret, home
	// or workspace path survives; the nonce and labels do.
	var decoded bytes.Buffer
	for _, l := range bytes.Split(bytes.TrimSpace(b.Files[ClientFile("claude", FileVendorEvents)]), []byte{'\n'}) {
		var wr transcriptWrapper
		if err := json.Unmarshal(l, &wr); err != nil {
			t.Fatal(err)
		}
		decoded.WriteString(unescapeView(wr.Data) + "\n")
	}
	view := string(all) + decoded.String()
	for _, s := range []string{planSecret, inheritSecret, shortSecret, "abcdefghijklmnop", "/home/owner", c.OutDir, "owner", helpPlanted, stderrPlanted, "helpbearer123", "pw-planted"} {
		if strings.Contains(view, s) {
			t.Fatalf("%q survived in the evidence", s)
		}
	}
	// Credential words in prose are kept (r0.3); only values go.
	if h := string(b.Files[ClientFile("claude", FileHelpStdout)]); h != strings.ReplaceAll(strings.ReplaceAll(helpText, helpPlanted, Redacted), "helpbearer123", Redacted) {
		t.Fatalf("help %q", h)
	}
	if e := string(b.Files[ClientFile("claude", FileVendorStderr)]); e != "plain note\nError: authentication required; token=[REDACTED] via https://[REDACTED]host/x\n" {
		t.Fatalf("stderr %q", e)
	}
	for _, s := range cc.Streams {
		if s.OmittedLines != 0 {
			t.Fatalf("an omission in %s", s.Path)
		}
	}
	for _, s := range []string{"noncecap1", "<workspace>/x", "<home>/.claude", "claude-capture-setup", Redacted, "uses a token here", "API key for auth", "authentication required"} {
		if !strings.Contains(view, s) {
			t.Fatalf("%q missing from the evidence", s)
		}
	}
	if strings.Contains(string(b.Files[CapturePlanName]), planSecret) || !strings.Contains(string(b.Files[CapturePlanName]), `"VENDOR_API_KEY":"[REDACTED]"`) {
		t.Fatalf("plan.json %s", b.Files[CapturePlanName])
	}
	// Every final file is a fixed point of the capture policy.
	red := NewCaptureRedactor(nil, nil)
	for p, data := range b.Files {
		if err := RedactionFixedPoint(p, data, red); err != nil {
			t.Fatal(err)
		}
	}
	if err := RedactionFixedPoint(CaptureManifestName, b.ManifestBytes, red); err != nil {
		t.Fatal(err)
	}
	// A malformed line holding a credential trigger is omitted whole, its
	// count recorded, and the capture is partial (not enrollable).
	sc = cachedCapture(t, "omission", func(w *capWorld) *CaptureRunner {
		w.script = secretScript(w, true)
		return newCapRunner(t, w, secretPlan(t))
	})
	man, b = sc.man, sc.bundle
	cc = capClient(t, man, "claude")
	if reasonOf(cc) != ReasonEvidenceOmitted || man.ExitCode() != 5 || !bytes.Contains(b.Files[ClientFile("claude", FileVendorEvents)], []byte(`{"omitted_lines":1}`)) {
		t.Fatalf("omission: %q", reasonOf(cc))
	}
	for _, s := range cc.Streams {
		if s.Path == ClientFile("claude", FileVendorEvents) && s.OmittedLines != 1 {
			t.Fatalf("stream %+v", s)
		}
	}
	if err := RedactionFixedPoint(ClientFile("claude", FileVendorEvents), b.Files[ClientFile("claude", FileVendorEvents)], red); err == nil {
		t.Fatal("an omission marker passed the fixed point")
	}
}

// The redaction fixed point rejects residual patterns, omissions,
// truncation markers and non-canonical wrappers in every file kind.
func TestRedactionFixedPoint(t *testing.T) {
	// The member-name prefilter never disagrees with the expression, also
	// for names that only case folding matches.
	for _, k := range []string{"api_\u212Aey", "\u017Fecret", "TOKEN", "Private-Key", "name", "monkey", "x\u212A", "Cookie", "passwd", "credentials", "AUTHORIZATION"} {
		if isSecretMember(k) != secretMember.MatchString(k) {
			t.Fatalf("member %q", k)
		}
	}
	red := NewCaptureRedactor([]string{"lit"}, map[string]string{"/home/me": "<home>"})
	ok := map[string]string{
		"clients/claude/vendor-events.jsonl": string(wrapLine(5, []byte(`{"a":"[REDACTED]"}`))) + "\n" + string(wrapLine(6, []byte("plain prose"))) + "\n",
		"clients/claude/server-events.jsonl": `{"seq":1,"kind":"start","offset_ns":0,"run_id":"r"}` + "\n",
		"clients/claude/help-stdout.txt":     "usage: x\n  -p prompt\n",
		// r0.3: credential words in prose and already-redacted values are
		// fixed points.
		"clients/codex/help-stdout.txt":     "uses a token here\nAPI key for auth: token=[REDACTED] API_KEY='[REDACTED]'\n",
		"manifest.json":                     "{\n  \"a\": \"b\"\n}\n",
		"clients/claude/version-stderr.txt": "",
	}
	for name, data := range ok {
		if err := RedactionFixedPoint(name, []byte(data), red); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
	}
	bad := map[string]string{
		"clients/claude/vendor-events.jsonl":  string(wrapLine(5, []byte(`{"auth":"Bearer abcdefghijklmn"}`))) + "\n",
		"clients/codex/vendor-events.jsonl":   `{"omitted_lines":1}` + "\n",
		"clients/grok/vendor-events.jsonl":    `{"truncated":true}` + "\n",
		"clients/cursor/vendor-events.jsonl":  `{"offset_ns": 5, "data": "x"}` + "\n",
		"clients/claude/server-events.jsonl":  `{"truncated":true}` + "\n",
		"clients/codex/server-events.jsonl":   `{"token":"secret-value"}` + "\n",
		"clients/grok/server-events.jsonl":    `{"a":1}`,
		"clients/claude/help-stdout.txt":      "token=plain-value\n",
		"clients/codex/help-stdout.txt":       "path /home/me/x\n",
		"clients/claude/help-stderr.txt":      "TOKEN=\"unterminated\n",
		"clients/codex/help-stderr.txt":       `not json "api_key": "x` + "\n",
		"clients/grok/help-stdout.txt":        "cut\n[truncated at the evidence limit]\n",
		"clients/cursor/help-stdout.txt":      "lit\n",
		"manifest.json":                       `{"api_key":"abc"}`,
		"plan.json":                           `not json`,
		"clients/cursor/server-events.jsonl":  "not json\n",
		"clients/claude/vendor-stderr.txt":    "a \\u0074oken\n",
		"clients/claude/version-stdout.txt":   "x\n" + string([]byte{0xff, 't', 'o', 'k', 'e', 'n'}) + "\n",
		"clients/claude/config.txt":           `{"password":"x"}` + "\n",
		"clients/codex/vendor-stderr.txt":     "Bearer abcdefghijkl\n",
		"clients/grok/vendor-stderr.txt":      "sk-abcdefghijklmnopqrst\n",
		"clients/cursor/vendor-stderr.txt":    "https://user:pw@host/x\n",
		"clients/claude/vendor-events.jsonl2": "",
	}
	delete(bad, "clients/claude/vendor-events.jsonl2")
	for name, data := range bad {
		if err := RedactionFixedPoint(name, []byte(data), red); err == nil {
			t.Errorf("%s: %q passed", name, data)
		}
	}
}

// injected small limits: each bound at N passes clean and at N+1 is cut
// and recorded (input or output), making the capture partial.
func TestCaptureBounds(t *testing.T) {
	const n = 256
	line := func(size int) string { return strings.Repeat("a", size-1) + "\n" }
	// Raw streams: N bytes retained whole; N+1 cut, the rest drained.
	if tr, end := readLinesEnd(strings.NewReader(line(n)), n, func() int64 { return 0 }); tr.Truncated || len(tr.Lines) != 1 || end == nil {
		t.Fatalf("stdout at N: %+v", tr)
	}
	if tr := readLines(strings.NewReader(line(n+1)+line(10*n)), n, func() int64 { return 0 }); !tr.Truncated || len(tr.Lines) != 0 {
		t.Fatalf("stdout at N+1: %+v", tr)
	}
	if b, cut, _ := readBoundedStream(strings.NewReader(line(n)), n); cut || len(b) != n {
		t.Fatal("stderr at N")
	}
	rd := strings.NewReader(line(n + 1 + 10*n))
	if b, cut, _ := readBoundedStream(rd, n); !cut || len(b) != n || rd.Len() != 0 {
		t.Fatal("stderr at N+1: cut, drained, nothing over N retained")
	}
	// The probe events file is read to at most its bound plus one byte.
	dir := t.TempDir()
	for size, cut := range map[int]bool{n: false, n + 1: true} {
		p := filepath.Join(dir, "events")
		os.WriteFile(p, []byte(line(size)), 0o600)
		if b, c := readFileBounded(p, n); c != cut || len(b) != min(size, n+1) {
			t.Fatalf("events of %d: %d %v", size, len(b), c)
		}
	}
	if b, c := readFileBounded(filepath.Join(dir, "absent"), n); b != nil || c {
		t.Fatal("absent events")
	}
	// A bundle file over its bound is refused before it is read (the
	// production 8 MiB case is BenchmarkMCPCaptureEvidence's).
	small := fstest.MapFS{"f": {Data: []byte(line(n + 1))}, "d/x": {Data: nil}}
	if _, err := (&fsTree{fsys: small, dir: "."}).read("f", n); err == nil {
		t.Fatal("an oversized file was read")
	}
	if b, err := (&fsTree{fsys: small, dir: "."}).read("f", n+1); err != nil || len(b) != n+1 {
		t.Fatal("a file at its bound")
	}
	if _, err := (&fsTree{fsys: small, dir: "."}).read("d", n); err == nil {
		t.Fatal("a directory was read")
	}
	// Payload files: exactly the file bound is kept; one more byte (from
	// redaction expansion of a short literal) is cut at a line boundary
	// with a marker; the per-client and bundle bounds cut the payload that
	// crosses them.
	red := NewCaptureRedactor([]string{shortSecret}, nil)
	w := &captureWriter{root: t.TempDir(), lim: CaptureLimits{File: n, Client: 3 * n, Bundle: 5 * n}, perClient: map[string]int{}}
	if ref, cut, err := w.put("claude", "clients/claude/a.txt", []byte(line(n)), false); err != nil || cut || ref.Bytes != n {
		t.Fatalf("file at N: %+v %v %v", ref, cut, err)
	}
	expanding := `{"v":"` + strings.Repeat("b", n-len(shortSecret)-10) + shortSecret + `"}` + "\n"
	clean, omitted := captureText(red, []byte(expanding))
	if len(expanding) > n || len(clean) <= n || omitted != 0 {
		t.Fatalf("expansion %d -> %d", len(expanding), len(clean))
	}
	if ref, cut, _ := w.put("claude", "clients/claude/b.txt", clean, false); !cut || ref.Bytes > n {
		t.Fatalf("file at N+1: %+v %v", ref, cut)
	}
	cw := &captureWriter{root: t.TempDir(), lim: CaptureLimits{File: n, Client: n + n/2, Bundle: 10 * n}, perClient: map[string]int{}}
	cw.put("claude", "clients/claude/a.txt", []byte(line(n)), false)
	if ref, cut, _ := cw.put("claude", "clients/claude/b.jsonl", []byte(line(n)), true); !cut || ref.Bytes > n/2 {
		t.Fatalf("client bound: %+v %v", ref, cut)
	}
	if ref, cut, _ := cw.put("codex", "clients/codex/a.txt", []byte(line(n)), false); cut || ref.Bytes != n {
		t.Fatalf("another client: %+v %v", ref, cut)
	}
	bw := &captureWriter{root: t.TempDir(), lim: CaptureLimits{File: n, Client: 10 * n, Bundle: n + 3}, perClient: map[string]int{}}
	bw.put("claude", "clients/claude/a.txt", []byte(line(n)), false)
	if ref, cut, _ := bw.put("codex", "clients/codex/a.txt", []byte(line(n)), false); !cut || ref.Bytes != 0 {
		t.Fatalf("bundle bound (no room for a marker): %+v %v", ref, cut)
	}
	if _, _, err := bw.put("codex", "clients/codex/a.txt", nil, false); err == nil {
		t.Fatal("a payload was written twice")
	}
	// Integration: flooding either stream of a real capture run.
	run := func(lim CaptureLimits, b capBehavior) CaptureClient {
		t.Helper()
		w := newCapWorld(t)
		w.script = func(spec ProcSpec) capBehavior {
			if launchKind(spec) != "session" {
				return w.defaults(spec)
			}
			b.events = goodEvents
			return b
		}
		c := newCapRunner(t, w, capPlan(t, "claude"))
		c.Limits = lim
		man, err := c.Run(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		return capClient(t, man, "claude")
	}
	stream := func(cc CaptureClient, name string) CaptureStream {
		for _, s := range cc.Streams {
			if s.Path == ClientFile("claude", name) {
				return s
			}
		}
		t.Fatalf("no stream %s", name)
		return CaptureStream{}
	}
	cc := run(CaptureLimits{Stream: n}, capBehavior{stdout: line(n+1) + capTranscript, floodStdout: 4 * n, stderr: line(n + 1), floodStderr: 4 * n})
	if !stream(cc, FileVendorEvents).InputTruncated || !stream(cc, FileVendorStderr).InputTruncated || reasonOf(cc) != ReasonEvidenceTruncated || cc.Session.StdoutHeld || cc.Session.StderrHeld {
		t.Fatalf("flood: %+v %q", cc.Streams, reasonOf(cc))
	}
	// A manifest over its bound is a finalization failure: no manifest.
	w2 := newCapWorld(t)
	c := newCapRunner(t, w2, capPlan(t, "claude"))
	c.Limits = CaptureLimits{Manifest: 100}
	if _, err := c.Run(context.Background()); err == nil || !strings.Contains(err.Error(), "manifest") {
		t.Fatalf("manifest bound: %v", err)
	}
	if _, err := os.Stat(filepath.Join(c.OutDir, CaptureManifestName)); err == nil {
		t.Fatal("an oversized manifest was written")
	}
}

// Every bundle corruption is rejected by ValidateCaptureBundle.
func TestCaptureBundleCorruption(t *testing.T) {
	b := secretCapture(t).bundle
	base := fstest.MapFS{CaptureManifestName: {Data: b.ManifestBytes, Mode: 0o600}}
	for p, data := range b.Files {
		base[p] = &fstest.MapFile{Data: data, Mode: 0o600}
	}
	if _, err := ValidateCaptureBundle(base, "."); err != nil {
		t.Fatal(err)
	}
	clone := func() fstest.MapFS {
		out := fstest.MapFS{}
		for k, v := range base {
			c := *v
			out[k] = &c
		}
		return out
	}
	// Manifest corruptions are byte edits (no re-encoding).
	edit := func(old, new string) func(fstest.MapFS) {
		return func(fsys fstest.MapFS) {
			m := fsys[CaptureManifestName]
			if !bytes.Contains(m.Data, []byte(old)) {
				t.Fatalf("the manifest lacks %q", old)
			}
			m.Data = bytes.Replace(m.Data, []byte(old), []byte(new), 1)
		}
	}
	vendor := ClientFile("claude", FileVendorEvents)
	for name, mutate := range map[string]func(fstest.MapFS){
		"extra-file":   func(f fstest.MapFS) { f["clients/claude/extra.txt"] = &fstest.MapFile{Data: []byte("x")} },
		"extra-listed": func(f fstest.MapFS) { f["clients/claude/help-stdout.txt.bak"] = &fstest.MapFile{Data: []byte("x")} },
		"extra-top":    func(f fstest.MapFS) { f["notes.txt"] = &fstest.MapFile{Data: []byte("x")} },
		"extra-dir":    func(f fstest.MapFS) { f["clients/zed/x"] = &fstest.MapFile{Data: []byte("x")} },
		"other-client": func(f fstest.MapFS) { f["clients/codex/config.txt"] = &fstest.MapFile{Data: []byte("x")} },
		"missing-file": func(f fstest.MapFS) { delete(f, vendor) },
		"missing-man":  func(f fstest.MapFS) { delete(f, CaptureManifestName) },
		"symlink":      func(f fstest.MapFS) { f[vendor] = &fstest.MapFile{Data: []byte("plan.json"), Mode: fs.ModeSymlink} },
		"fifo":         func(f fstest.MapFS) { f[vendor] = &fstest.MapFile{Mode: fs.ModeNamedPipe} },
		"hash": func(f fstest.MapFS) {
			f[vendor].Data = bytes.Replace(f[vendor].Data, []byte("noncecap1"), []byte("noncecap2"), 1)
		},
		"size": func(f fstest.MapFS) { f[vendor].Data = append(f[vendor].Data, '\n') },
		// Strict JSON: an unknown or missing member, a duplicate key, a
		// wrong type (the semantic rules are TestCaptureManifestValidate's).
		"unknown-field": edit(`{
  "schema"`, `{
  "extra": true,
  "schema"`),
		"missing-field": edit(`  "vendor_behavior": "not_evaluated",
`, ""),
		"duplicate-key": edit(`"state": "complete",`, `"state": "complete", "state": "complete",`),
		"schema-type":   edit(`"schema": "mcpqual-capture-v1"`, `"schema": 1`),
	} {
		fsys := clone()
		mutate(fsys)
		if _, err := ValidateCaptureBundle(fsys, "."); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	// An extra file is accepted only when named (an enrolled expected.json).
	fsys := clone()
	fsys[ExpectedName] = &fstest.MapFile{Data: []byte("{}")}
	if _, err := ValidateCaptureBundle(fsys, ".", ExpectedName); err != nil {
		t.Fatal(err)
	}
	// I/O: a payload path that is already a directory ends the capture
	// with an error and no manifest.
	w := newCapWorld(t)
	c := newCapRunner(t, w, capPlan(t, "claude"))
	w.script = func(spec ProcSpec) capBehavior {
		if launchKind(spec) == "session" {
			os.MkdirAll(filepath.Join(c.OutDir, "clients", "claude", FileVendorEvents), 0o700)
		}
		return w.defaults(spec)
	}
	if _, err := c.Run(context.Background()); err == nil {
		t.Fatal("a write failure was not reported")
	}
	if _, err := os.Stat(filepath.Join(c.OutDir, CaptureManifestName)); err == nil {
		t.Fatal("a manifest after a write failure")
	}
	if _, err := os.Stat(filepath.Join(c.OutDir, workDir)); err == nil {
		t.Fatal("the workspace survived a write failure")
	}
}

// cloneManifest copies m deeply enough for one mutation.
func cloneManifest(m *CaptureManifest) *CaptureManifest {
	c := *m
	c.Files = append([]EvidenceRef(nil), m.Files...)
	c.Cleanup.Failures = append([]string{}, m.Cleanup.Failures...)
	c.Clients = make([]CaptureClient, len(m.Clients))
	for i, cl := range m.Clients {
		cc := cl
		cc.Streams = append([]CaptureStream{}, cl.Streams...)
		cc.Argv = append([]string{}, cl.Argv...)
		if cl.Probe != nil {
			p := *cl.Probe
			p.Anomalies = append([]string{}, cl.Probe.Anomalies...)
			cc.Probe = &p
		}
		c.Clients[i] = cc
	}
	return &c
}

// The manifest's semantic contract, rule by rule, on struct copies.
func TestCaptureManifestValidate(t *testing.T) {
	base := secretCapture(t).man
	if err := cloneManifest(base).Validate(); err != nil {
		t.Fatal(err)
	}
	cl := func(f func(c *CaptureClient)) func(m *CaptureManifest) {
		return func(m *CaptureManifest) { f(&m.Clients[0]) }
	}
	for name, mutate := range map[string]func(m *CaptureManifest){
		"run-id":          func(m *CaptureManifest) { m.RunID = "bad id" },
		"not-utc":         func(m *CaptureManifest) { m.CapturedAt = "2026-09-29T12:00:00+02:00" },
		"no-arch":         func(m *CaptureManifest) { m.Arch = "" },
		"os":              func(m *CaptureManifest) { m.OS = "windows" },
		"policy":          func(m *CaptureManifest) { m.RedactionPolicy = "other" },
		"evaluated":       func(m *CaptureManifest) { m.VendorBehavior = "compatible" },
		"state":           func(m *CaptureManifest) { m.State = "done" },
		"no-clients":      func(m *CaptureManifest) { m.Clients = nil },
		"failures-nil":    func(m *CaptureManifest) { m.Cleanup.Failures = nil },
		"cleanup-silent":  func(m *CaptureManifest) { m.Cleanup.OK, m.State = false, CapturePartial },
		"complete-dirty":  func(m *CaptureManifest) { m.Cleanup = CleanupReport{Failures: []string{"x"}} },
		"limits-raised":   func(m *CaptureManifest) { m.Limits.SessionMS = CaptureMaxSessionMS + 1 },
		"limits-sessions": func(m *CaptureManifest) { m.Limits.SessionsPerClient = 3 },
		"limits-bytes":    func(m *CaptureManifest) { m.Limits.FileBytes = MaxEvidenceFileBytes + 1 },
		"unsorted":        func(m *CaptureManifest) { m.Files[0], m.Files[1] = m.Files[1], m.Files[0] },
		"duplicate":       func(m *CaptureManifest) { m.Files = append(m.Files[:1], m.Files...) },
		"escape":          func(m *CaptureManifest) { m.Files[0].Path = "../plan.json" },
		"not-allowlisted": func(m *CaptureManifest) { m.Files[0].Path = "clients/claude/aaa.txt" },
		"bad-hash":        func(m *CaptureManifest) { m.Files[0].SHA256 = "x" },
		"too-big":         func(m *CaptureManifest) { m.Files[0].Bytes = int64(m.Limits.FileBytes) + 1 },
		"no-plan":         func(m *CaptureManifest) { m.Files = m.Files[:len(m.Files)-1] },
		"bundle-total":    func(m *CaptureManifest) { m.Limits.BundleBytes = 10 },
		"client-total":    func(m *CaptureManifest) { m.Limits.ClientBytes = 10 },
		"orphan-file": func(m *CaptureManifest) {
			m.Files = append([]EvidenceRef{{Path: "clients/codex/config.txt", SHA256: m.Files[0].SHA256}}, m.Files...)
		},
		"dup-client":        func(m *CaptureManifest) { m.Clients = append(m.Clients, m.Clients[0]) },
		"unknown-client":    cl(func(c *CaptureClient) { c.ID = "zed" }),
		"null-no-reason":    cl(func(c *CaptureClient) { c.Effort, c.EffortReason = nil, nil }),
		"probe-no-reason":   cl(func(c *CaptureClient) { c.Probe, c.ProbeReason = nil, nil }),
		"decoder-validated": cl(func(c *CaptureClient) { c.DecoderValidated = true }),
		"decoder-label":     cl(func(c *CaptureClient) { c.Decoder = "codex-jsonl" }),
		"exe-hash":          cl(func(c *CaptureClient) { c.ExecutableSHA256 = sptr("x") }),
		"case-id":           cl(func(c *CaptureClient) { c.CaseID = "claude-setup" }),
		"override":          cl(func(c *CaptureClient) { c.Config.TimeoutOverride = "raised" }),
		"client-state":      cl(func(c *CaptureClient) { c.State = "done" }),
		"no-reason":         cl(func(c *CaptureClient) { c.State, c.Reason = CapturePartial, nil }),
		"argv-nil":          cl(func(c *CaptureClient) { c.Argv = nil }),
		"anomalies-nil":     cl(func(c *CaptureClient) { c.Probe.Anomalies = nil }),
		"stage-state":       cl(func(c *CaptureClient) { c.Help.State = "maybe" }),
		"help-prepared":     cl(func(c *CaptureClient) { c.Help.State, c.Help.Reason = StagePrepared, sptr("x") }),
		"stage-no-reason":   cl(func(c *CaptureClient) { c.Help.State, c.Help.Cleanup = StageNotRun, nil }),
		"stage-cleanup":     cl(func(c *CaptureClient) { c.Help.Cleanup = nil }),
		"stage-files": cl(func(c *CaptureClient) {
			c.Session.State, c.Session.Reason, c.Session.Cleanup = StageLaunchFailed, sptr("x"), nil
		}),
		"config-file":      cl(func(c *CaptureClient) { c.Session = CaptureStage{State: StageNotRun, Reason: sptr("x")} }),
		"stream-foreign":   cl(func(c *CaptureClient) { c.Streams[0].Path = "clients/codex/config.txt" }),
		"stream-repeat":    cl(func(c *CaptureClient) { c.Streams[1] = c.Streams[0] }),
		"stream-missing":   cl(func(c *CaptureClient) { c.Streams = c.Streams[1:] }),
		"complete-cut":     cl(func(c *CaptureClient) { c.Streams[0].InputTruncated = true }),
		"complete-version": cl(func(c *CaptureClient) { c.ObservedVersion = sptr("other") }),
		"complete-exit":    cl(func(c *CaptureClient) { c.Session.Exit = new(int); *c.Session.Exit = 1 }),
		"complete-probe":   cl(func(c *CaptureClient) { c.Probe.Receipts = 2 }),
		"complete-cleanup": cl(func(c *CaptureClient) {
			cc := *c.Session.Cleanup
			cc.GroupGone = false
			c.Session.Cleanup = &cc
		}),
		"complete-stages": cl(func(c *CaptureClient) {
			c.Version.State, c.Version.Reason, c.Version.Cleanup = StageLaunchFailed, sptr("x"), nil
		}),
	} {
		m := cloneManifest(base)
		mutate(m)
		if err := m.Validate(); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

// helpCorpus is the pinned eleven-file historical help inventory (design
// decoder-enrollment r0.3, UT-3): read in place, never changed.
var helpCorpus = []string{"claude-help.txt", "claude-mcp-add.txt", "codex-help.txt", "codex-exec.txt", "codex-mcp-add.txt", "cursor-help.txt",
	"cursor-mcp.txt", "grok-help.txt", "grok-agent.txt", "grok-mcp.txt", "grok-mcp-add.txt"}

// helpCorpusChanges are the in-place changes the capture policy makes to
// the corpus, as literals ("file:line" -> sanitized line). The first three
// are DW8's: an API_KEY value, a Bearer value and the accepted
// --xai-api-base-url over-redaction (the xai- prefix rule and its floor are
// unchanged). The other six follow from the plain-assignment rule as
// written (an identifier matching secretName, which includes "key"):
// placeholder values of KEY=value examples. They are reported to the
// designer as exceeding DW8's three.
var helpCorpusChanges = map[string]string{
	"claude-mcp-add.txt:16": "  claude mcp add my-server -e API_KEY=[REDACTED] -- npx my-mcp-server",
	"grok-mcp-add.txt:64":   `  grok mcp add --transport http api https://mcp.example.com/mcp --header "Authorization: Bearer [REDACTED]"`,
	"grok-agent.txt:37":     "      --[REDACTED] <XAI_API_BASE_URL>",
	"claude-mcp-add.txt:27": "  -e, --env <env...>           Set environment variables (e.g. -e KEY=[REDACTED]",
	"codex-help.txt:46":     "  -c, --config <key=[REDACTED]",
	"codex-exec.txt:21":     "  -c, --config <key=[REDACTED]",
	"codex-mcp-add.txt:13":  "  -c, --config <key=[REDACTED]",
	"codex-mcp-add.txt:21":  "      --env <KEY=[REDACTED]",
	"grok-mcp-add.txt:35":   "  -e, --env <KEY=[REDACTED]",
}

// corpusResult is one help file's sanitization.
type corpusResult struct {
	file              string
	raw, clean, again []byte
	omitted, omitted2 int
	fixed             error
}

// TestCaptureHelpCorpus: every historical help file sanitizes with zero
// omissions, exactly the pinned in-place changes, its ordinary credential
// instructions kept, and is a fixed point with an identical second pass.
func TestCaptureHelpCorpus(t *testing.T) {
	if len(helpCorpus) != 11 {
		t.Fatal("the help inventory is eleven files")
	}
	// The sanitization of the corpus is pure and deterministic: computed
	// once per test process, asserted every run.
	corpus := sharedValue("help-corpus", func() []corpusResult {
		red := NewCaptureRedactor(nil, nil)
		var out []corpusResult
		for _, f := range helpCorpus {
			raw, err := os.ReadFile(filepath.Join("..", "..", "tests", "testdata", "cli-help", f))
			if err != nil {
				t.Fatal(err)
			}
			r := corpusResult{file: f, raw: raw}
			r.clean, r.omitted = captureText(red, raw)
			r.again, r.omitted2 = captureText(red, r.clean)
			r.fixed = RedactionFixedPoint(ClientFile("claude", FileHelpStdout), r.clean, red)
			out = append(out, r)
		}
		return out
	})
	changes := map[string]string{}
	for _, r := range corpus {
		in, out := strings.Split(string(r.raw), "\n"), strings.Split(string(r.clean), "\n")
		if r.omitted != 0 || len(in) != len(out) {
			t.Fatalf("%s: %d omitted, %d -> %d lines", r.file, r.omitted, len(in), len(out))
		}
		for i := range in {
			if in[i] != out[i] {
				changes[fmt.Sprintf("%s:%d", r.file, i+1)] = out[i]
			}
		}
		if r.omitted2 != 0 || !bytes.Equal(r.again, r.clean) || sha256Hex(r.again) != sha256Hex(r.clean) {
			t.Fatalf("%s: the second pass differs", r.file)
		}
		if r.fixed != nil {
			t.Fatal(r.fixed)
		}
	}
	if len(changes) != len(helpCorpusChanges) {
		t.Fatalf("%d changes, want %d: %q", len(changes), len(helpCorpusChanges), changes)
	}
	for k, want := range helpCorpusChanges {
		if changes[k] != want {
			t.Fatalf("%s = %q, want %q", k, changes[k], want)
		}
	}
	// Ordinary credential-word instructions are kept.
	for f, want := range map[string]string{"claude-help.txt": "Anthropic auth is", "codex-help.txt": "bearer token to send", "codex-mcp-add.txt": "bearer token. Only valid",
		"claude-mcp-add.txt": `--header "Authorization: Bearer ..."`} {
		i := slices.Index(helpCorpus, f)
		if !bytes.Contains(corpus[i].clean, []byte(want)) {
			t.Fatalf("%s lost %q", f, want)
		}
	}
}

// TestCapturePlantedSecrets: known literals (inherited, plan, config and a
// 3-byte value), each prefixed form at its floor, Bearer, the three
// assignment forms and URL userinfo are redacted in place in help, stderr
// and decoded transcript data, with exact markers and zero omissions;
// floor-minus-one and already-redacted controls, the public nonce and
// unrelated numbers stay.
func TestCapturePlantedSecrets(t *testing.T) {
	env := []string{"API_TOKEN=inherited-secret-7", "PATH=/usr/bin", "XDG_SESSION_ID=2"}
	red := NewCaptureRedactor(append(CredentialValues(env), "plan-secret-8", "config-secret-9", "Zq9"), nil)
	floor := strings.Repeat("a", 12)
	for in, want := range map[string]string{
		"inherited inherited-secret-7 plan plan-secret-8 config config-secret-9 short xZq9y":     "inherited [REDACTED] plan [REDACTED] config [REDACTED] short x[REDACTED]y",
		"keys sk-" + floor + " pk-" + floor + " xai-" + floor + " key-" + floor + " rk-" + floor: "keys [REDACTED] [REDACTED] [REDACTED] [REDACTED] [REDACTED]",
		"controls sk-" + floor[1:] + " xai-" + floor[1:]:                                         "controls sk-" + floor[1:] + " xai-" + floor[1:],
		"Authorization: Bearer abcd1234 and bearer abc1234":                                      "Authorization: Bearer [REDACTED] and bearer abc1234",
		`token=plain1 MY_SECRET='single 1' session_cookie="double 1" end`:                        `token=[REDACTED] MY_SECRET='[REDACTED]' session_cookie="[REDACTED]" end`,
		`already token=[REDACTED] API_KEY="[REDACTED]" auth = 'x'`:                               `already token=[REDACTED] API_KEY="[REDACTED]" auth = '[REDACTED]'`,
		"fetch https://user:pass@example.com/x":                                                  "fetch https://[REDACTED]example.com/x",
		"version 2.1.291 nonce noncecap2 count 2 uses a token here":                              "version 2.1.291 nonce noncecap2 count 2 uses a token here",
	} {
		for _, kind := range []string{"help", "stderr"} {
			clean, omitted := captureText(red, []byte(in+"\n"))
			if omitted != 0 || string(clean) != want+"\n" {
				t.Fatalf("%s %q = %q (%d omitted)", kind, in, clean, omitted)
			}
		}
		// The same text inside a transcript line's JSON string, escaped and
		// nested, redacts by decoded content.
		inner, _ := json.Marshal(map[string]string{"t": in})
		line, _ := json.Marshal(map[string]string{"text": string(inner), "nonce": "noncecap2"})
		data, ok := red.TranscriptLine(line)
		var outer map[string]string
		var dec map[string]string
		if !ok || json.Unmarshal(data, &outer) != nil || json.Unmarshal([]byte(outer["text"]), &dec) != nil || dec["t"] != want || outer["nonce"] != "noncecap2" {
			t.Fatalf("transcript %q = %s (%v)", in, data, ok)
		}
	}
	// The former safe omission stimulus is now redacted and kept.
	if data, ok := red.TranscriptLine([]byte("not json: token=plan-secret-8")); !ok || string(data) != "not json: token=[REDACTED]" {
		t.Fatalf("safe credential prose: %q %v", data, ok)
	}
}

// TestCaptureUnsafeLines: only unsafe non-JSON lines are omitted whole.
func TestCaptureUnsafeLines(t *testing.T) {
	red := NewCaptureRedactor([]string{"lit-value"}, nil)
	for name, line := range map[string]string{
		"invalid-utf8":   "token " + string([]byte{0xff}) + " x",
		"escaped-cut":    `{"password":"prefix\"ordinary-value`,
		"quoted-member":  `not json "api_key" : "x`,
		"member-shape":   `cut {"X-Session-Token":"abc`,
		"unterminated-d": `export TOKEN="abc def`,
		"unterminated-s": `auth='abc`,
		"backslash":      `path C:\Users\x`,
	} {
		if _, ok := red.TranscriptLine([]byte(line)); ok {
			t.Errorf("%s: %q kept", name, line)
		}
		if _, omitted := captureText(red, []byte("ok line\n"+line+"\n")); omitted != 1 {
			t.Errorf("%s: %d omitted", name, omitted)
		}
	}
	// Not the member shape, not unsafe: kept and redacted in place; an
	// encoded already-redacted assignment in valid JSON stays byte-identical.
	for in, want := range map[string]string{
		`{"a":"token=[REDACTED] x","m":"API_KEY=\"[REDACTED]\""}`: `{"a":"token=[REDACTED] x","m":"API_KEY=\"[REDACTED]\""}`,
		`{"m":"API_KEY=\"v1\" and token=v2"}`:                     `{"m":"API_KEY=\"[REDACTED]\" and token=[REDACTED]"}`,
		`the "token" word: here`:                                  `the "token" word: here`,
		`a "monkey" quote: lit-value`:                             `a "monkey" quote: [REDACTED]`,
		`{"note":"a \"quoted\" lit-value"}`:                       `{"note":"a \"quoted\" [REDACTED]"}`,
	} {
		if data, ok := red.TranscriptLine([]byte(in)); !ok || string(data) != want {
			t.Errorf("%q = %q %v", in, data, ok)
		}
	}
	// The legacy qualification policy is unchanged: it still omits prose
	// with a credential word.
	if _, ok := NewRedactor(nil, nil).TranscriptLine([]byte("uses a token here")); ok {
		t.Fatal("the qualification policy changed")
	}
}

// TestCaptureSessionException: only an inherited, exactly named, decimal
// XDG_SESSION_ID is not a literal; every other credential source is.
func TestCaptureSessionException(t *testing.T) {
	for _, tc := range []struct {
		env  []string
		want []string
	}{
		{[]string{"XDG_SESSION_ID=2"}, nil},
		{[]string{"XDG_SESSION_ID=4217"}, nil},
		{[]string{"XDG_SESSION_ID=c2"}, []string{"c2"}},
		{[]string{"XDG_SESSION_IDX=2", "MY_XDG_SESSION_ID=2", "xdg_session_id=2"}, []string{"2", "2", "2"}},
		{[]string{"XDG_SESSION_CLASS=user", "XDG_SESSION_TYPE=tty"}, []string{"user", "tty"}},
		{[]string{"XDG_SESSION_ID=2", "API_KEY=2"}, []string{"2"}},
		{[]string{"XDG_SESSION_ID=", "PATH=/bin"}, nil},
	} {
		if got := CredentialValues(tc.env); !slices.Equal(got, tc.want) {
			t.Fatalf("%v: %q, want %q", tc.env, got, tc.want)
		}
	}
	// In a capture: the inherited session number leaves versions, JSON
	// numbers and nonces intact; an explicit plan value of the same name
	// is still a literal.
	fixture := `{"version":"2.1.291","n":2,"nonce":"a2b2"}`
	c := &CaptureRunner{Plan: capPlan(t, "claude"), OutDir: "/o", BaseEnv: []string{"XDG_SESSION_ID=2"}}
	if got := c.redactor().Line([]byte(fixture)); string(got) != fixture {
		t.Fatalf("inherited session number: %s", got)
	}
	p := *c.Plan
	p.Clients = []PlanClient{p.Clients[0]}
	p.Clients[0].Env = map[string]string{"XDG_SESSION_ID": "2"}
	c.Plan = &p
	if got := c.redactor().String("2.1.291"); got != "[REDACTED].1.[REDACTED]91" {
		t.Fatalf("explicit plan session value: %q", got)
	}
}
