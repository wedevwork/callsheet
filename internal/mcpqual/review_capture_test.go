package mcpqual

import (
	"context"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
)

// Code review round 1 of design decoder-enrollment (C1..C7): each finding
// driven through the paths the reviewer used: the capture run,
// TranscriptLine, RedactionFixedPoint and ValidateEnrollment.

const opaque = "opaque987654321"

// fullyDecoded unescapes s until it stops changing (no budget), so a
// value hidden under any number of escape layers is visible.
func fullyDecoded(s string) string {
	for i := 0; i < 256 && strings.IndexByte(s, '\\') >= 0; i++ {
		u := unescapeOnce(s)
		if u == s {
			break
		}
		s = u
	}
	return s
}

// reenrolled is the shared enrollment with a bundle payload replaced
// (rel -> data, "" deletes nothing) and the manifest optionally mutated,
// with every hash made consistent: only the property under test can fail.
func reenrolled(t *testing.T, payloads map[string][]byte, mutate func(m *CaptureManifest)) (EnrollmentOptions, *enrolled) {
	t.Helper()
	e := enrolledFixture(t)
	dir := e.entry.Bundle
	var m CaptureManifest
	if err := json.Unmarshal(e.files[path.Join(dir, CaptureManifestName)], &m); err != nil {
		t.Fatal(err)
	}
	files := map[string][]byte{}
	for k, v := range e.files {
		files[k] = v
	}
	for rel, data := range payloads {
		files[path.Join(dir, rel)] = data
		for i := range m.Files {
			if m.Files[i].Path == rel {
				m.Files[i].SHA256, m.Files[i].Bytes = sha256Hex(data), int64(len(data))
			}
		}
	}
	if mutate != nil {
		mutate(&m)
	}
	mb, _ := encodeIndent(&m)
	files[path.Join(dir, CaptureManifestName)] = mb
	en := e.entry
	en.ManifestSHA256 = sha256Hex(mb)
	files[EnrollmentIndexPath], _ = encodeIndent(EnrollmentIndex{Schema: EnrollmentSchema, Entries: []EnrollmentEntry{en}})
	return EnrollmentOptions{FS: e.fs(func(f map[string][]byte) {
		for k := range f {
			delete(f, k)
		}
		for k, v := range files {
			f[k] = v
		}
	}), Registry: e.reg}, e
}

// withTranscriptLine appends a raw (unsanitized) wrapped line to the
// enrolled transcript.
func withTranscriptLine(t *testing.T, e *enrolled, line string) map[string][]byte {
	rel := ClientFile("claude", FileVendorEvents)
	data := append(append([]byte(nil), e.bundle.Files[rel]...), wrapLine(1, []byte(line))...)
	return map[string][]byte{rel: append(data, '\n')}
}

// C1: credential names that only Unicode case folding matches.
func TestReviewC1UnicodeCaseFold(t *testing.T) {
	red := NewCaptureRedactor(nil, nil)
	lines := []string{`{"ſecret":"` + opaque + `"}`, `{"paſſword":"` + opaque + `"}`, `{"api_` + "K" + `ey":"` + opaque + `"}`,
		`{"a":"{\"ſecret\":\"` + opaque + `\"}"}`, `{"note":"Bearer ` + opaque + `"}`}
	for _, l := range lines {
		out, ok := red.TranscriptLine([]byte(l))
		if !ok || strings.Contains(fullyDecoded(string(out)), opaque) {
			t.Errorf("TranscriptLine(%s) = %s", l, out)
		}
		if err := RedactionFixedPoint(ClientFile("claude", FileVendorEvents), append(wrapLine(1, []byte(l)), '\n'), red); err == nil {
			t.Errorf("fixed point accepted %s", l)
		}
	}
	// The legacy policy's Line walks such a value too.
	if out := NewRedactor(nil, nil).Line([]byte(lines[0])); strings.Contains(string(out), opaque) {
		t.Fatalf("legacy Line kept %s", out)
	}
	// Through the capture run (once per process).
	_, b := capturedOnce(t, "r1-c1", func(w *capWorld) *CaptureRunner {
		w.script = func(spec ProcSpec) capBehavior {
			b := w.defaults(spec)
			if launchKind(spec) == "session" {
				b.stdout = strings.Join(lines, "\n") + "\n"
			}
			return b
		}
		return newCapRunner(t, w, capPlan(t, "claude"))
	})
	for p, data := range b.Files {
		if strings.Contains(fullyDecoded(string(data)), opaque) {
			t.Fatalf("%s keeps the value", p)
		}
	}
	// Through enrollment: a residual value fails the fixed point.
	e := enrolledFixture(t)
	opts, _ := reenrolled(t, withTranscriptLine(t, e, lines[1]), nil)
	if _, err := ValidateEnrollment(opts); err == nil || !strings.Contains(err.Error(), "redaction fixed point") {
		t.Fatalf("enrollment accepted a residual case-folded credential: %v", err)
	}
}

// depthResult is one wrapped input's sanitization outcome.
type depthResult struct {
	layers   int
	line     string
	out      []byte
	ok       bool
	stable   bool
	fixedErr error
}

// depthCase sanitizes a credential under k JSON-string layers, as a whole
// line or as a member value.
func depthCase(red *Redactor, k int, member bool) depthResult {
	v := `{"token":"` + opaque + `"}`
	for i := 0; i < k; i++ {
		v = quoteASCII(v)
	}
	if member {
		v = `{"m":` + v + `}`
	}
	r := depthResult{layers: k, line: v}
	r.out, r.ok = red.TranscriptLine([]byte(v))
	if r.ok {
		again, ok2 := red.TranscriptLine(r.out)
		r.stable = ok2 && string(again) == string(r.out)
	}
	r.fixedErr = RedactionFixedPoint(ClientFile("claude", FileVendorEvents), append(wrapLine(1, []byte(v)), '\n'), red)
	return r
}

// deepDepthCases (16 to 18 layers, megabyte-scale escapes) are pure and
// deterministic: computed once per test process, asserted every run.
var (
	deepDepthOnce   sync.Once
	deepDepthCache  []depthResult
	deepEnrollErr   error
	deepCaptureLeak bool
)

// captureLeaks reports whether a capture whose transcript holds line keeps
// the credential anywhere in its bundle (fully decoded). The capture runs
// once per process per key.
func captureLeaks(t *testing.T, key, line string) bool {
	t.Helper()
	man, b := capturedOnce(t, key, func(w *capWorld) *CaptureRunner {
		w.script = func(spec ProcSpec) capBehavior {
			b := w.defaults(spec)
			if launchKind(spec) == "session" {
				b.stdout = line + "\n"
			}
			return b
		}
		return newCapRunner(t, w, capPlan(t, "claude"))
	})
	if capClient(t, man, "claude").State == CaptureNotRun {
		t.Fatal("the session did not run")
	}
	for _, data := range b.Files {
		if strings.Contains(fullyDecoded(string(data)), opaque) {
			return true
		}
	}
	return false
}

// C2: a credential under more JSON-string layers than the decode budget.
func TestReviewC2DepthBudget(t *testing.T) {
	red := NewCaptureRedactor(nil, nil)
	var results []depthResult
	// The boundaries of both budgets (8 JSON walks, then 8 unescape
	// layers) and beyond them, including the reviewer's 17 and 18.
	for _, k := range []int{0, 1, 7, 8, 9} {
		results = append(results, depthCase(red, k, false), depthCase(red, k, true))
	}
	deepDepthOnce.Do(func() {
		deepDepthCache = []depthResult{depthCase(red, 16, false), depthCase(red, 17, true), depthCase(red, 18, false)}
		e := enrolledFixture(t)
		opts, _ := reenrolled(t, withTranscriptLine(t, e, deepDepthCache[1].line), nil)
		_, deepEnrollErr = ValidateEnrollment(opts)
		deepCaptureLeak = captureLeaks(t, "r1-c2-17", deepDepthCache[1].line)
	})
	// Through the capture run (once per process): beyond the walk budget
	// (9 layers) and beyond both budgets (17 layers).
	if captureLeaks(t, "r1-c2-9", results[9].line) || deepCaptureLeak {
		t.Fatal("a capture kept a deeply encoded credential")
	}
	for _, r := range append(results, deepDepthCache...) {
		if r.ok && strings.Contains(fullyDecoded(string(r.out)), opaque) {
			t.Fatalf("%d layers: the credential survived", r.layers)
		}
		if r.ok && !r.stable {
			t.Fatalf("%d layers: not a fixed point", r.layers)
		}
		if r.fixedErr == nil {
			t.Fatalf("%d layers: the fixed point accepted the raw value", r.layers)
		}
	}
	if len(results)+len(deepDepthCache) != 13 {
		t.Fatal("depth inventory")
	}
	if deepDepthCache[1].layers != 17 {
		t.Fatal("the 17-layer member case moved")
	}
	if deepEnrollErr == nil {
		t.Fatal("enrollment accepted a 17-layer credential")
	}
}

// C3: owner-controlled manifest strings are sanitized before encoding.
func TestReviewC3ManifestStrings(t *testing.T) {
	// Both capture runs once per process; every repetition asserts on
	// their outcomes.
	type c3Outcome struct {
		man                     *CaptureManifest
		b                       *CaptureBundle
		bundleErr               error
		failRunErr, manifestErr error
	}
	o := sharedValue("r1-c3", func() c3Outcome {
		var o c3Outcome
		o.man, o.b = capturedOnce(t, "r1-c3", func(w *capWorld) *CaptureRunner {
			return newCapRunner(t, w, mustCapturePlan(t, filledPlan(t, func(c map[string]any) {
				c["decoder_fixture"] = "token=" + opaque
				c["config"].(map[string]any)["default"].(map[string]any)["path"] = "cookie=" + opaque + ".txt"
			}, "claude")))
		})
		_, o.bundleErr = ValidateCaptureBundle(os.DirFS(filepath.Join(sharedTempDir(t), "once-r1-c3")), ".")
		// A redaction that breaks the required structure (here a
		// credential literal equal to the decoder name) fails closed: no
		// manifest.
		c := newCapRunner(t, newCapWorld(t), capPlan(t, "claude"))
		c.OutDir = filepath.Join(sharedTempDir(t), "r1-c3-closed")
		c.BaseEnv = append(c.BaseEnv, "AUTH_PART=claude-json")
		_, o.failRunErr = c.Run(context.Background())
		_, o.manifestErr = os.Stat(filepath.Join(c.OutDir, CaptureManifestName))
		return o
	})
	man, b := o.man, o.b
	if strings.Contains(fullyDecoded(string(b.ManifestBytes)), opaque) {
		t.Fatalf("the manifest keeps the value: %s", b.ManifestBytes)
	}
	for f, data := range b.Files {
		if strings.Contains(fullyDecoded(string(data)), opaque) {
			t.Fatalf("%s keeps the value", f)
		}
	}
	cc := capClient(t, man, "claude")
	if cc.DecoderFixture != "token="+Redacted || cc.Config.Path != "cookie="+Redacted || man.State != CaptureComplete {
		t.Fatalf("fixture %q path %q state %s", cc.DecoderFixture, cc.Config.Path, man.State)
	}
	if o.bundleErr != nil {
		t.Fatal(o.bundleErr)
	}
	if err := RedactionFixedPoint(CaptureManifestName, b.ManifestBytes, NewCaptureRedactor(nil, nil)); err != nil {
		t.Fatal(err)
	}
	if o.failRunErr == nil {
		t.Fatal("an invalid sanitized path was accepted")
	}
	if o.manifestErr == nil {
		t.Fatal("a manifest was written")
	}
}

// C4: plan.json cut by redaction expansion is never complete.
func TestReviewC4PlanTruncation(t *testing.T) {
	man, b := capturedOnce(t, "r1-c4", func(w *capWorld) *CaptureRunner {
		c := newCapRunner(t, w, mustCapturePlan(t, filledPlan(t, func(c map[string]any) {
			c["env"] = map[string]any{"NOTE": strings.Repeat("z", 3000)}
		}, "claude")))
		c.BaseEnv = append(c.BaseEnv, "TOKEN=z")
		c.Limits = CaptureLimits{File: 8000}
		return c
	})
	if man.State == CaptureComplete || man.ExitCode() == 0 || !man.Plan.OutputTruncated || len(b.Files[CapturePlanName]) > 8000 {
		t.Fatalf("a cut plan: state %s exit %d plan %+v", man.State, man.ExitCode(), man.Plan)
	}
	if !strings.HasPrefix(*man.Reason, ReasonEvidenceTruncated) {
		t.Fatalf("reason %v", man.Reason)
	}
	m := cloneManifest(secretCapture(t).man)
	m.Plan.OutputTruncated = true
	if err := m.Validate(); err == nil {
		t.Fatal("a complete manifest with a cut plan validated")
	}
}

// C5: a symlinked ancestor of the enrollment tree, through a real
// os.DirFS and through os.Root.
func TestReviewC5AncestorSymlink(t *testing.T) {
	// The trees are written and validated once per process (deterministic
	// outcomes); every repetition asserts on them.
	type c5Outcome struct{ outside, ancestor, ancestorRoot, bundle error }
	o := sharedValue("r1-c5", func() c5Outcome {
		var o c5Outcome
		e := enrolledFixture(t)
		top, err := os.MkdirTemp(sharedTempDir(t), "r1-c5-")
		if err != nil {
			t.Fatal(err)
		}
		repo, outside, repo2 := filepath.Join(top, "repo"), filepath.Join(top, "outside"), filepath.Join(top, "repo2")
		writeFiles(t, outside, e.files)
		_, o.outside = ValidateEnrollment(EnrollmentOptions{FS: os.DirFS(outside), Registry: e.reg})
		os.MkdirAll(filepath.Join(repo, "tests", "testdata"), 0o700)
		if err := os.Symlink(filepath.Join(outside, "tests", "testdata", "mcp-transcripts"), filepath.Join(repo, "tests", "testdata", "mcp-transcripts")); err != nil {
			t.Fatal(err)
		}
		_, o.ancestor = ValidateEnrollment(EnrollmentOptions{FS: os.DirFS(repo), Registry: e.reg})
		root, err := os.OpenRoot(repo)
		if err != nil {
			t.Fatal(err)
		}
		_, o.ancestorRoot = ValidateEnrollment(EnrollmentOptions{FS: root.FS(), Registry: e.reg})
		root.Close()
		// A symlinked bundle directory deeper down.
		for rel, data := range e.files {
			if strings.HasPrefix(rel, e.entry.Bundle+"/") {
				continue
			}
			writeFiles(t, repo2, map[string][]byte{rel: data})
		}
		link := filepath.Join(repo2, filepath.FromSlash(e.entry.Bundle))
		os.MkdirAll(filepath.Dir(link), 0o700)
		os.Symlink(filepath.Join(outside, filepath.FromSlash(e.entry.Bundle)), link)
		_, o.bundle = ValidateEnrollment(EnrollmentOptions{FS: os.DirFS(repo2), Registry: e.reg})
		return o
	})
	if o.outside != nil {
		t.Fatal(o.outside)
	}
	if o.ancestor == nil || !strings.Contains(o.ancestor.Error(), "symbolic link") {
		t.Fatalf("a symlinked ancestor enrolled: %v", o.ancestor)
	}
	if o.ancestorRoot == nil {
		t.Fatal("a symlinked ancestor enrolled through os.Root")
	}
	if o.bundle == nil || !strings.Contains(o.bundle.Error(), "symbolic link") {
		t.Fatalf("a symlinked bundle directory enrolled: %v", o.bundle)
	}
}

// C6: a complete capture needs clean version and help stages too.
func TestReviewC6MetadataStages(t *testing.T) {
	one := 1
	sig := "killed"
	for name, mutate := range map[string]func(m *CaptureManifest){
		"version-exit":        func(m *CaptureManifest) { m.Clients[0].Version.Exit = &one },
		"version-signal":      func(m *CaptureManifest) { m.Clients[0].Version.Exit, m.Clients[0].Version.Signal = nil, &sig },
		"help-watchdog":       func(m *CaptureManifest) { m.Clients[0].Help.Watchdog = true },
		"help-interrupted":    func(m *CaptureManifest) { m.Clients[0].Help.Interrupted = true },
		"version-stdout-held": func(m *CaptureManifest) { m.Clients[0].Version.StdoutHeld = true },
		"help-stderr-held":    func(m *CaptureManifest) { m.Clients[0].Help.StderrHeld = true },
		"session-signal":      func(m *CaptureManifest) { m.Clients[0].Session.Signal = &sig },
	} {
		m := cloneManifest(secretCapture(t).man)
		mutate(m)
		if err := m.Validate(); err == nil {
			t.Errorf("%s: validated", name)
		}
		// The reviewer's two reproductions through the full validator.
		if name != "version-exit" && name != "help-watchdog" {
			continue
		}
		opts, _ := reenrolled(t, nil, mutate)
		if _, err := ValidateEnrollment(opts); err == nil {
			t.Errorf("%s: enrolled", name)
		}
	}
}

// C7: probe receipt and completion correlate by request and instance.
func TestReviewC7ProbeCorrelation(t *testing.T) {
	const id = "claude-capture-setup"
	cf := CaseFile{RunID: "run-cap-1"}
	rid := func(kind, req string) func(CaseFile, string) ProbeEvent {
		return func(_ CaseFile, c string) ProbeEvent {
			return ProbeEvent{Kind: kind, CaseID: c, RequestID: json.RawMessage(req)}
		}
	}
	start := func(CaseFile, string) ProbeEvent { return ProbeEvent{Kind: EvStart} }
	for name, events := range map[string]func(CaseFile, string) string{
		"request-mismatch":    probeWith(rid(EvReceipt, `1`), rid(EvCompleted, `2`), exitEv()),
		"duplicate":           probeWith(rid(EvReceipt, `1`), rid(EvCompleted, `1`), rid(EvCompleted, `1`), exitEv()),
		"before-receipt":      probeWith(rid(EvCompleted, `1`), rid(EvReceipt, `1`), exitEv()),
		"no-receipt":          probeWith(rid(EvCompleted, `1`), exitEv()),
		"other-instance":      probeWith(rid(EvReceipt, `1`), start, rid(EvCompleted, `1`), exitEv()),
		"completion-for-none": probeWith(rid(EvReceipt, `1`), rid(EvCompleted, `1`), func(_ CaseFile, _ string) ProbeEvent { return ProbeEvent{Kind: EvCompleted, CaseID: "other"} }, exitEv()),
	} {
		raw := events(cf, id)
		p := observeProbe([]byte(raw), false, id)
		if len(p.Anomalies) == 0 {
			t.Errorf("%s: no anomaly: %+v", name, p)
		}
		zero := 0
		run := caseRun{exit: &zero, probeRaw: []byte(raw), transcript: Transcript{Lines: []Line{{Data: []byte("x")}}}}
		if got := sessionReason(run, CaptureClient{Probe: p}); !strings.HasPrefix(got, ReasonProbeAnomaly) {
			t.Errorf("%s: reason %q", name, got)
		}
	}
	// Through one capture run: the reviewer's three sequences, one client
	// each.
	seqs := map[string]func(CaseFile, string) string{
		"claude": probeWith(rid(EvReceipt, `1`), rid(EvCompleted, `2`), exitEv()),
		"codex":  probeWith(rid(EvReceipt, `1`), rid(EvCompleted, `1`), rid(EvCompleted, `1`), exitEv()),
		"grok":   probeWith(rid(EvCompleted, `1`), rid(EvReceipt, `1`), exitEv()),
	}
	man, _ := capturedOnce(t, "r1-c7", func(w *capWorld) *CaptureRunner {
		w.script = func(spec ProcSpec) capBehavior {
			b := w.defaults(spec)
			if launchKind(spec) == "session" {
				b.events = seqs[clientOf(spec)]
			}
			return b
		}
		return newCapRunner(t, w, capPlan(t, "claude", "codex", "grok"))
	})
	for id := range seqs {
		if r := reasonOf(capClient(t, man, id)); !strings.HasPrefix(r, ReasonProbeAnomaly) {
			t.Errorf("%s: capture reason %q", id, r)
		}
	}
	// A well-formed sequence still completes.
	if p := observeProbe([]byte(probeWith(rid(EvReceipt, `"a"`), rid(EvScheduled, `"a"`), rid(EvCompleted, `"a"`), exitEv())(cf, id)), false, id); len(p.Anomalies) != 0 || !p.Completed || p.Receipts != 1 {
		t.Fatalf("well formed: %+v", p)
	}
	// Through replay and enrollment: the shared fixture's server events
	// with the completion's request ID changed.
	e := enrolledFixture(t)
	rel := ClientFile("claude", FileServerEvents)
	var out []byte
	forged := false
	for _, l := range strings.Split(strings.TrimSuffix(string(e.bundle.Files[rel]), "\n"), "\n") {
		var ev ProbeEvent
		if err := json.Unmarshal([]byte(l), &ev); err != nil {
			t.Fatal(err)
		}
		if ev.Kind == EvCompleted {
			ev.RequestID, forged = json.RawMessage(`"forged"`), true
		}
		b, _ := encodeJSON(ev)
		out = append(append(out, b...), '\n')
	}
	if !forged {
		t.Fatal("no completion to forge")
	}
	opts, _ := reenrolled(t, map[string][]byte{rel: out}, nil)
	if _, err := ValidateEnrollment(opts); err == nil || !strings.Contains(err.Error(), "probe replay") {
		t.Fatalf("enrollment accepted a mismatched completion: %v", err)
	}
}

// quoteASCII is quoteJSON for ASCII text without control characters,
// cheap for the megabyte-scale depth fixtures.
func quoteASCII(s string) string {
	var b strings.Builder
	b.Grow(2*len(s) + 2)
	b.WriteByte('"')
	for i := 0; i < len(s); i++ {
		if s[i] == '"' || s[i] == '\\' {
			b.WriteByte('\\')
		}
		b.WriteByte(s[i])
	}
	b.WriteByte('"')
	return b.String()
}

// Code review round 2.

// encodedKey is `token` with its first letter escaped and every backslash
// then replaced by \ n times: n+1 decoding layers name the member.
func encodedKey(n int) string {
	const bs = `\`
	k := bs + "u" + "0074oken"
	for i := 0; i < n; i++ {
		k = strings.ReplaceAll(k, bs, bs+"u"+"005c")
	}
	return k
}

// R2-C1: a member name still encoded when its budget runs out withholds
// its whole value subtree.
func TestReviewR2EncodedMemberNames(t *testing.T) {
	red := NewCaptureRedactor(nil, nil)
	layers := []int{0, 7, 8, 9, 10, 16, 17, 18}
	var lines []string
	for _, n := range layers {
		for _, l := range []string{`{"` + encodedKey(n) + `":"` + opaque + `"}`, `{"` + encodedKey(n) + `":{"inner":["` + opaque + `"]}}`} {
			lines = append(lines, l)
			out, ok := red.TranscriptLine([]byte(l))
			if !ok || strings.Contains(fullyDecoded(string(out)), opaque) {
				t.Errorf("%d layers: %s -> %s", n, l, out)
			}
			if again, ok2 := red.TranscriptLine(out); !ok2 || string(again) != string(out) {
				t.Errorf("%d layers: not a fixed point: %s", n, out)
			}
			if err := RedactionFixedPoint(ClientFile("claude", FileVendorEvents), append(wrapLine(1, []byte(l)), '\n'), red); err == nil {
				t.Errorf("%d layers: the fixed point accepted the raw line", n)
			}
			if err := RedactionFixedPoint(ClientFile("claude", FileHelpStdout), []byte(l+"\n"), red); err == nil {
				t.Errorf("%d layers: the help fixed point accepted the raw line", n)
			}
		}
	}
	// Through the capture run (once per process): in the transcript and in
	// the help output.
	_, b := capturedOnce(t, "r2-c1", func(w *capWorld) *CaptureRunner {
		w.script = func(spec ProcSpec) capBehavior {
			b := w.defaults(spec)
			switch launchKind(spec) {
			case "help":
				b.stdout = strings.Join(lines, "\n") + "\n"
			case "session":
				b.stdout = strings.Join(lines, "\n") + "\n"
			}
			return b
		}
		return newCapRunner(t, w, capPlan(t, "claude"))
	})
	for p, data := range b.Files {
		if strings.Contains(fullyDecoded(string(data)), opaque) {
			t.Errorf("%s keeps the value", p)
		}
	}
	// Through enrollment: the captured help evidence is a fixed point and
	// enrolls, and what enrolls holds no value; the raw lines never do.
	help := ClientFile("claude", FileHelpStdout)
	captured := b.Files[help]
	if err := RedactionFixedPoint(help, captured, red); err != nil {
		t.Fatalf("captured help is not a fixed point: %v", err)
	}
	opts, _ := reenrolled(t, map[string][]byte{help: captured}, nil)
	if _, err := ValidateEnrollment(opts); err != nil {
		t.Fatalf("captured help did not enroll: %v", err)
	}
	if got, _ := fs.ReadFile(opts.FS, path.Join(enrolledFixture(t).entry.Bundle, help)); strings.Contains(fullyDecoded(string(got)), opaque) {
		t.Errorf("enrollment accepted help evidence that keeps the value")
	}
	opts, _ = reenrolled(t, map[string][]byte{help: []byte(lines[4] + "\n")}, nil)
	if _, err := ValidateEnrollment(opts); err == nil || !strings.Contains(err.Error(), "redaction fixed point") {
		t.Fatalf("enrollment accepted an encoded member name: %v", err)
	}
}

// R2-C3: initialization and lifecycle are per probe instance.
func TestReviewR2ProbeInstances(t *testing.T) {
	const id = "claude-capture-setup"
	nameA, nameB, ver := "client-a", "client-b", "1"
	initA := ProbeEvent{Kind: EvInitialize, ClientName: &nameA, ClientVersion: &ver}
	initB := ProbeEvent{Kind: EvInitialize, ClientName: &nameB, ClientVersion: &ver}
	recv := ProbeEvent{Kind: EvReceipt, CaseID: id, RequestID: json.RawMessage(`1`)}
	done := ProbeEvent{Kind: EvCompleted, CaseID: id, RequestID: json.RawMessage(`1`)}
	exit := ProbeEvent{Kind: EvExit, Reason: "eof"}
	start := ProbeEvent{Kind: EvStart}
	split := probeLog("r", start, initA, exit) + probeLog("r", start, recv, done, exit)
	p := observeProbe([]byte(split), false, id)
	if len(p.Anomalies) == 0 || !strings.Contains(strings.Join(p.Anomalies, ","), "receipt_without_initialize") {
		t.Errorf("an uninitialized instance's receipt: %+v", p)
	}
	// Within one instance: a receipt before its initialize, and events
	// after its exit.
	for name, log := range map[string]string{
		"receipt_without_initialize": probeLog("r", start, recv, initA, done, exit),
		"event_after_exit":           probeLog("r", start, initA, exit, recv, done, exit),
	} {
		if p := observeProbe([]byte(log), false, id); !strings.Contains(strings.Join(p.Anomalies, ","), name) {
			t.Errorf("%s: %+v", name, p)
		}
	}
	// The first clientInfo is still the one recorded; a receipt in its own
	// initialized instance is accepted.
	ok := probeLog("r", start, initA, exit) + probeLog("r", start, initB, recv, done, exit)
	if p := observeProbe([]byte(ok), false, id); len(p.Anomalies) != 0 || !p.Completed || *p.ClientName != nameA {
		t.Fatalf("two initialized instances: %+v", p)
	}
	// Through a capture run (once per process) and through replay in
	// enrollment.
	man, _ := capturedOnce(t, "r2-c3", func(w *capWorld) *CaptureRunner {
		w.script = func(spec ProcSpec) capBehavior {
			b := w.defaults(spec)
			if launchKind(spec) == "session" {
				b.events = func(cf CaseFile, c string) string {
					return probeLog(cf.RunID, start, initA, exit) + probeLog(cf.RunID, start, ProbeEvent{Kind: EvReceipt, CaseID: c, RequestID: json.RawMessage(`1`)},
						ProbeEvent{Kind: EvCompleted, CaseID: c, RequestID: json.RawMessage(`1`)}, exit)
				}
			}
			return b
		}
		return newCapRunner(t, w, capPlan(t, "claude"))
	})
	if r := reasonOf(capClient(t, man, "claude")); !strings.HasPrefix(r, ReasonProbeAnomaly) {
		t.Errorf("capture reason %q", r)
	}
	e := enrolledFixture(t)
	var evs []ProbeEvent
	for _, l := range strings.Split(strings.TrimSuffix(string(e.bundle.Files[ClientFile("claude", FileServerEvents)]), "\n"), "\n") {
		var ev ProbeEvent
		if err := json.Unmarshal([]byte(l), &ev); err != nil {
			t.Fatal(err)
		}
		evs = append(evs, ev)
	}
	// The fixture's own instance without its initialize, after an instance
	// that only initializes.
	var rest []ProbeEvent
	var first *ProbeEvent
	for i := range evs {
		if evs[i].Kind == EvInitialize {
			first = &evs[i]
			continue
		}
		rest = append(rest, evs[i])
	}
	if first == nil {
		t.Fatal("no initialize in the fixture")
	}
	forged := probeLog(evs[0].RunID, start, *first, exit) + probeLog(evs[0].RunID, rest...)
	opts, _ := reenrolled(t, map[string][]byte{ClientFile("claude", FileServerEvents): []byte(forged)}, nil)
	if _, err := ValidateEnrollment(opts); err == nil || !strings.Contains(err.Error(), "probe replay") {
		t.Fatalf("enrollment accepted an uninitialized instance: %v", err)
	}
}

// writeFiles writes repository-relative files under dir.
func writeFiles(t *testing.T, dir string, files map[string][]byte) {
	t.Helper()
	for rel, data := range files {
		dst := filepath.Join(dir, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(dst), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(dst, data, 0o600); err != nil {
			t.Fatal(err)
		}
	}
}

// swapAt runs check with openHook armed to run swap once, at the nth open
// of the component name (after its check): the deterministic replacement
// seam. It returns check's error.
func swapAt(name string, nth int, swap func(), check func() error) error {
	seen := 0
	openHook = func(n string) {
		if n == name {
			if seen++; seen == nth {
				swap()
			}
		}
	}
	defer func() { openHook = nil }()
	return check()
}

// relink replaces the path at by a relative symbolic link to target (both
// inside the repository), moving the original aside out of the tree read.
func relink(t *testing.T, repo, at, target string) {
	t.Helper()
	if err := os.Rename(at, filepath.Join(repo, "aside-"+filepath.Base(at))); err != nil {
		t.Fatal(err)
	}
	rel, err := filepath.Rel(filepath.Dir(at), target)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(rel, at); err != nil {
		t.Fatal(err)
	}
}

// relinkNear replaces the path at by a symbolic link to its own original,
// renamed beside it: a link inside the same directory, which os.Root
// follows itself, to the very file or directory that was checked.
func relinkNear(t *testing.T, at string) {
	t.Helper()
	if err := os.Rename(at, at+".decoy"); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Base(at)+".decoy", at); err != nil {
		t.Fatal(err)
	}
}

// refused reports whether err refuses a replaced component: by the
// post-open identity check, or (a link leaving a held directory) by
// os.Root itself.
func refused(err error) bool {
	return err != nil && (strings.Contains(err.Error(), "symbolic link") || strings.Contains(err.Error(), "replaced") ||
		strings.Contains(err.Error(), "path escapes from parent"))
}

// swapOutcome is one replacement case's result (built once per process:
// the cases are deterministic and sequential, and each writes two
// repository trees).
type swapOutcome struct {
	err     error
	swapped bool // the replaced path is a symbolic link afterwards
}

// R2-C2: a payload or an ancestor replaced by a symbolic link between its
// no-follow check and its open is never followed: through os.DirFS,
// os.Root's FS (in-root relative links, which os.Root itself follows) and
// the held os.Root of EnrollmentOptions.Root. A "far" link points at an
// identical copy elsewhere in the repository, a "near" one at the
// original itself, renamed beside it (so following it reads exactly the
// checked bytes and only the identity check can tell).
func TestReviewR2SwapAtOpen(t *testing.T) {
	e := enrolledFixture(t)
	bundle := filepath.FromSlash(e.entry.Bundle)
	client := filepath.Join(bundle, "clients", "claude")
	help := FileHelpStdout
	type swapCase struct {
		name string
		at   string // the component name the swap is armed on
		nth  int    // at its nth open
		path string // the repository path replaced
	}
	// The repository ancestor is opened for the index and again for the
	// bundle: the second open is the last check of it.
	swaps := []swapCase{
		{"payload", help, 1, filepath.Join(client, help)},
		{"bundle-ancestor", "clients", 1, filepath.Join(bundle, "clients")},
		{"repository-ancestor", "mcp-transcripts", 2, filepath.FromSlash(EnrollmentRoot)},
		{"held-ancestor", help, 1, client},
	}
	kinds := []string{"dirfs", "rootfs", "root"}
	outcomes := sharedValue("swap-at-open", func() map[string]swapOutcome {
		out := map[string]swapOutcome{}
		// setup writes one repository; decoy, when not empty, is the
		// repository path whose identical copy a far link targets (only
		// that subtree is copied under decoy/).
		setup := func(decoy string) string {
			repo, err := os.MkdirTemp(sharedTempDir(t), "swap-")
			if err != nil {
				t.Fatal(err)
			}
			writeFiles(t, repo, e.files)
			if decoy != "" {
				p := filepath.ToSlash(decoy)
				sub := map[string][]byte{}
				for rel, data := range e.files {
					if rel == p || strings.HasPrefix(rel, p+"/") {
						sub[rel] = data
					}
				}
				if len(sub) == 0 {
					t.Fatalf("no decoy files under %s", p)
				}
				writeFiles(t, filepath.Join(repo, "decoy"), sub)
			}
			return repo
		}
		decoyOf := func(sc swapCase, near bool) string {
			if near {
				return ""
			}
			return sc.path
		}
		swap := func(repo string, sc swapCase, near bool) func() {
			return func() {
				if near {
					relinkNear(t, filepath.Join(repo, sc.path))
				} else {
					relink(t, repo, filepath.Join(repo, sc.path), filepath.Join(repo, "decoy", sc.path))
				}
			}
		}
		isLink := func(p string) bool {
			fi, err := os.Lstat(p)
			return err == nil && fi.Mode()&fs.ModeSymlink != 0
		}
		options := func(kind, repo string) EnrollmentOptions {
			opts := EnrollmentOptions{Registry: e.reg}
			switch kind {
			case "dirfs":
				opts.FS = os.DirFS(repo)
			case "rootfs":
				r, err := os.OpenRoot(repo)
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { r.Close() })
				opts.FS = r.FS()
			case "root":
				opts.Root = repo
			}
			return opts
		}
		// The unswapped baselines only read: one repository serves them all.
		plain := setup("")
		for _, kind := range kinds {
			_, err := ValidateEnrollment(options(kind, plain))
			out["unswapped/"+kind] = swapOutcome{err: err}
		}
		// undo restores a swapped path (the link removed, the original
		// renamed back), so one repository serves every filesystem kind of
		// a case; a repository that cannot be restored exactly fails the
		// test.
		undo := func(repo string, sc swapCase, near bool) {
			at := filepath.Join(repo, sc.path)
			orig := filepath.Join(repo, "aside-"+filepath.Base(at))
			if near {
				orig = at + ".decoy"
			}
			if err := os.Remove(at); err != nil {
				t.Fatal(err)
			}
			if err := os.Rename(orig, at); err != nil {
				t.Fatal(err)
			}
			if isLink(at) {
				t.Fatalf("%s was not restored", at)
			}
		}
		repos := map[string]string{}
		for _, sc := range swaps {
			for _, near := range []bool{false, true} {
				repo := setup(decoyOf(sc, near))
				repos[fmt.Sprintf("%s/near=%v", sc.name, near)] = repo
				for _, kind := range kinds {
					opts := options(kind, repo)
					err := swapAt(sc.at, sc.nth, swap(repo, sc, near), func() error { _, err := ValidateEnrollment(opts); return err })
					swapped := isLink(filepath.Join(repo, sc.path))
					out[fmt.Sprintf("%s/%s/near=%v", sc.name, kind, near)] = swapOutcome{err, swapped}
					if swapped {
						undo(repo, sc, near)
					}
				}
			}
		}
		// The held os.Root reads the file it verified even when the
		// replaced ancestor's decoy differs: had it followed the link, the
		// hash would not match.
		repo := setup(client)
		os.WriteFile(filepath.Join(repo, "decoy", client, help), []byte("decoy\n"), 0o600)
		err := swapAt(help, 1, swap(repo, swaps[3], false), func() error {
			_, err := ValidateEnrollment(EnrollmentOptions{Root: repo, Registry: e.reg})
			return err
		})
		out["held-ancestor/root/differing"] = swapOutcome{err, isLink(filepath.Join(repo, client))}
		// A FIFO swapped in for a payload neither blocks the open nor is
		// read.
		repo = setup("")
		at := filepath.Join(repo, client, help)
		err = swapAt(help, 1, func() {
			if err := os.Rename(at, filepath.Join(repo, "aside")); err != nil {
				t.Fatal(err)
			}
			if err := syscall.Mkfifo(at, 0o600); err != nil {
				t.Fatal(err)
			}
		}, func() error { _, err := ValidateEnrollment(EnrollmentOptions{Root: repo, Registry: e.reg}); return err })
		out["payload/root/fifo"] = swapOutcome{err, true}
		// ValidateCaptureBundle (always an fs.FS) over the bundle directory.
		_, err = ValidateCaptureBundle(os.DirFS(plain), e.entry.Bundle, ExpectedName)
		out["bundle/unswapped"] = swapOutcome{err: err}
		for _, sc := range []swapCase{swaps[0], swaps[1], swaps[3]} {
			for _, near := range []bool{false, true} {
				repo := repos[fmt.Sprintf("%s/near=%v", sc.name, near)]
				err := swapAt(sc.at, sc.nth, swap(repo, sc, near), func() error {
					_, err := ValidateCaptureBundle(os.DirFS(repo), e.entry.Bundle, ExpectedName)
					return err
				})
				out[fmt.Sprintf("bundle/%s/near=%v", sc.name, near)] = swapOutcome{err, isLink(filepath.Join(repo, sc.path))}
			}
		}
		return out
	})
	if len(outcomes) != 3+24+2+1+6 {
		t.Fatalf("%d outcomes", len(outcomes))
	}
	for name, o := range outcomes {
		switch {
		case strings.HasSuffix(name, "unswapped") || strings.HasPrefix(name, "unswapped/"):
			if o.err != nil {
				t.Errorf("%s: %v", name, o.err)
			}
		case !o.swapped:
			t.Errorf("%s: the swap did not run", name)
		case name == "payload/root/fifo":
			if o.err == nil || !strings.Contains(o.err.Error(), "not a regular file") {
				t.Errorf("%s: a FIFO swapped in at the open: %v", name, o.err)
			}
		case strings.HasPrefix(name, "held-ancestor/root/"):
			// The held directory handle is read, not the new path.
			if o.err != nil {
				t.Errorf("%s: a held ancestor's replacement redirected the read: %v", name, o.err)
			}
		case !refused(o.err):
			t.Errorf("%s: a replacement at the open was followed: %v", name, o.err)
		}
	}
}
