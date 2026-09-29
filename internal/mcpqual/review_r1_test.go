package mcpqual

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/wedevwork/callsheet/internal/contract"
	"github.com/wedevwork/callsheet/internal/testkit"
)

// Regressions for code review r1 (C1-C6). Each reproduces the reviewer's
// failure.

// recordingLauncher records every launch; each process exits at once with
// empty output.
type recordingLauncher struct {
	mu   sync.Mutex
	args [][]string
}

func (l *recordingLauncher) Start(s ProcSpec) (Proc, error) {
	l.mu.Lock()
	l.args = append(l.args, append([]string(nil), s.Args...))
	l.mu.Unlock()
	out := ""
	if slices.Equal(s.Args, []string{"--version"}) {
		out = "2.1.282 (Claude Code)\n"
	}
	p := &fakeProc{pgid: 4000 + len(l.args), out: strings.NewReader(out), exited: make(chan struct{}), exit: new(int)}
	close(p.exited)
	return p, nil
}

// C1: metadata commands are fixed before authorization; a model prompt
// hidden in version_argv or help_argv is rejected before any launch.
func TestR1C1MetadataBeforeModelGate(t *testing.T) {
	paid := []string{"-p", "run a paid model prompt", "--model", "paid-model"}
	for _, field := range []string{"version_argv", "help_argv"} {
		p := planWith(Phases{})
		c := &p.Clients[0]
		c.Driver, c.Model = DriverModel, "paid-model"
		c.Session.Argv = append(c.Session.Argv, "--model", "paid-model")
		if field == "version_argv" {
			c.VersionArgv = paid
		} else {
			c.HelpArgv = paid
		}
		if _, err := ParsePlan(planJSON(t, p), DefaultRegistry()); err == nil || !strings.Contains(err.Error(), field) {
			t.Errorf("%s: a model prompt in %s was accepted: %v", field, field, err)
		}
		// The runner refuses it too, even when handed an unvalidated plan.
		rec := &recordingLauncher{}
		r := newRunner(t, newWorld(t, fullModel()), p)
		r.Launcher = rec
		runPlan(t, r, context.Background())
		for _, a := range rec.args {
			if slices.Contains(a, "run a paid model prompt") {
				t.Errorf("%s: AllowModelCalls=false launched argv %q", field, a)
			}
		}
	}
}

// C2: report-bound strings and wrapped transcripts are sanitized before
// their outer encoding; hashes cover the sanitized bytes.
func TestR1C2RedactionBeforeEncoding(t *testing.T) {
	m := fullModel()
	m.clientName = "hunter2-secret-value"
	// r2 C1: escapes hide a plan secret and a credential-named member from
	// any check on the raw bytes (the reviewer's two lines); one line also
	// starts with whitespace, one nests escaped JSON in a string, one is
	// cut mid-value and one escapes a secret used as a key. r4: the
	// reviewer's cut line (invalid JSON, an escaped quote ending the
	// member early) is omitted whole, as is the other cut line. r5: so is a
	// credential-bearing line whose bytes are not UTF-8 (the reviewer's raw
	// 0xff in a key), though json.Valid accepts it.
	const esc = "\\"
	cutLine := `{"type":"noise","password":"prefix` + esc + `"ordinary-value`
	badUTF8Line := `{"type":"noise","passw` + "\xff" + `ord":"ordinary-value"}`
	m.transcriptTail = `{"type":"noise","password":"ordinary-password-value","result":"{\"api_key\":\"abcd1234efgh5678wxyz\"}"}` + "\n" +
		`{"type":"noise","result":"hunter2-` + esc + `u0073ecret-value"}` + "\n" +
		`{"type":"noise","pa` + esc + `u0073` + esc + `u0073word":"ordinary-value"}` + "\n" +
		`  {"type":"noise","pa` + esc + `u0073` + esc + `u0073word":"ordinary-escaped-value"}` + "\n" +
		`{"type":"noise","result":"{\"pa` + esc + esc + `u0073sword\":\"nested-escaped-value\"}"}` + "\n" +
		`{"type":"noise","pa` + esc + `u0073` + esc + `u0073word":"cut-escaped-value` + "\n" +
		`{"type":"noise","hunter2-` + esc + `u0073ecret-value":1}` + "\n" +
		badUTF8Line + "\n" +
		cutLine
	repo := tempRepo(t)
	base, err := ReadCatalogBase(repo)
	if err != nil {
		t.Fatal(err)
	}
	r := newRunner(t, newWorld(t, m), planWith(Phases{}))
	rep := runPlan(t, r, context.Background())
	patch, err := ProposePatch(rep, base, r.OutDir)
	if err != nil {
		t.Fatal(err)
	}
	var all bytes.Buffer
	filepath.Walk(r.OutDir, func(p string, info os.FileInfo, err error) error {
		if err == nil && !info.IsDir() {
			b, _ := os.ReadFile(p)
			all.Write(b)
		}
		return nil
	})
	vendor, err := os.ReadFile(filepath.Join(r.OutDir, "cases", "claude-setup", "vendor-events.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	decoded := strings.Join(decodedLeaves(vendor), "\n")
	for _, line := range strings.Split(strings.TrimSuffix(string(vendor), "\n"), "\n") {
		var ev struct {
			Data *string `json:"data"`
		}
		if err := json.Unmarshal([]byte(line), &ev); err != nil {
			t.Fatalf("vendor-events.jsonl line %s: %v", line, err)
		}
		if ev.Data != nil && !json.Valid([]byte(*ev.Data)) {
			t.Errorf("a cut credential line is published in part: %s", line)
		}
	}
	if !bytes.HasSuffix(vendor, []byte(`{"omitted_lines":3}`+"\n")) {
		t.Errorf("vendor-events.jsonl does not end with the omission count:\n%s", vendor)
	}
	for _, secret := range []string{"hunter2-secret-value", "ordinary-password-value", "abcd1234efgh5678wxyz", "ecret-value", "ordinary-value",
		"ordinary-escaped-value", "nested-escaped-value", "cut-escaped-value", "prefix"} {
		if bytes.Contains(all.Bytes(), []byte(secret)) {
			t.Errorf("%q survives in the evidence directory", secret)
		}
		if strings.Contains(decoded, secret) {
			t.Errorf("%q is recoverable from vendor-events.jsonl after JSON decoding", secret)
		}
		if b, _ := json.Marshal(patch); bytes.Contains(b, []byte(secret)) {
			t.Errorf("%q survives in the catalog patch", secret)
		}
	}
	if err := strictReport(t, r).CheckEvidence(r.OutDir); err != nil {
		t.Fatal(err)
	}
	// The omission decision itself: only an invalid line with a backslash,
	// a known literal or a credential trigger is omitted.
	red := NewRedactor([]string{"hunter2-secret-value"}, map[string]string{"/home/owner": "<home>"})
	for _, tc := range []struct {
		in, want string
		kept     bool
	}{
		{cutLine, "", false},
		{badUTF8Line, "", false},
		{`{"note":"caf` + "\xff" + `"}`, `{"note":"caf` + "\xff" + `"}`, true},
		{`{"type":"noise","result":"hunter2-` + esc + `u0073ecret-value"}`, `{"type":"noise","result":"[REDACTED]"}`, true},
		{`{"type":"noise","pa` + esc + `u0073` + esc + `u0073word":"ordinary-value"}`, `{"type":"noise","password":"[REDACTED]"}`, true},
		{" \t" + `{"passwd":"x"}` + " \r", " \t" + `{"passwd":"[REDACTED]"}`, true},
		{`{"note":"plain \q text`, "", false},
		{`{"note":"hunter2-secret-value and mo`, "", false},
		{`{"path":"/home/owner/work`, "", false},
		{`{"Session_TOKEN_count":3,`, "", false},
		{`{"note":"cut ordinary text`, `{"note":"cut ordinary text`, true},
		{`not json at all`, `not json at all`, true},
	} {
		got, kept := red.TranscriptLine([]byte(tc.in))
		if string(got) != tc.want || kept != tc.kept {
			t.Errorf("TranscriptLine(%s) = %q, %v; want %q, %v", tc.in, got, kept, tc.want, tc.kept)
		}
	}
}

// decodedLeaves is every key and string of each JSONL line, JSON-decoded
// and, for strings that are JSON themselves, decoded again, each also with
// any escapes left in it decoded; a line that is not JSON counts whole.
func decodedLeaves(b []byte) []string {
	var out []string
	var walk func(v any)
	add := func(s string) {
		out = append(out, s, testUnescape(s))
		var inner any
		if json.Unmarshal([]byte(s), &inner) == nil {
			walk(inner)
		}
	}
	walk = func(v any) {
		switch v := v.(type) {
		case map[string]any:
			for k, x := range v {
				add(k)
				walk(x)
			}
		case []any:
			for _, x := range v {
				walk(x)
			}
		case string:
			add(v)
		}
	}
	for _, line := range strings.Split(string(b), "\n") {
		var v any
		if json.Unmarshal([]byte(line), &v) != nil {
			add(line)
			continue
		}
		walk(v)
	}
	return out
}

var testEscape = regexp.MustCompile(`\\(u[0-9a-fA-F]{4}|["\\/])`)

// testUnescape decodes \uXXXX, \", \\ and \/ escapes, layer after layer,
// independently of the redactor's own decoder.
func testUnescape(s string) string {
	for range 8 {
		u := testEscape.ReplaceAllStringFunc(s, func(e string) string {
			if e[1] != 'u' {
				return e[1:]
			}
			n, _ := strconv.ParseUint(e[2:], 16, 32)
			return string(rune(n))
		})
		if u == s {
			break
		}
		s = u
	}
	return s
}

// C3: every launch is capped by the client's remaining wall time, and a
// budget cut is reported distinctly from an interruption.
func TestR1C3ClientDeadlineCapsLaunches(t *testing.T) {
	w := newWorld(t, fullModel())
	w.hang = func(s ProcSpec) bool { return strings.Contains(strings.Join(s.Args, " "), "claude-default-1") }
	w.onStart = func(s ProcSpec) {
		if strings.Contains(strings.Join(s.Args, " "), "claude-setup") {
			w.clock.Advance(9800 * time.Millisecond) // the setup session's model latency
		}
	}
	p := planWith(defaultOnly(100, 300))
	p.Limits = &Limits{MaxSessionsPerClient: 12, MaxCaseMS: 10000, MaxClientMS: 10000}
	r := newRunner(t, w, p)
	done := make(chan *Report, 1)
	go func() { rep, _ := r.Run(context.Background()); done <- rep }()
	for s := range w.started {
		if strings.Contains(strings.Join(s.Args, " "), "claude-default-1") {
			break
		}
	}
	var armed time.Duration
	if err := w.clock.AwaitWaiter(testWait, func(ws []testkit.Waiter) bool {
		for _, x := range ws {
			if !x.Ticker && x.Duration >= 100*time.Millisecond {
				armed = x.Duration
				return true
			}
		}
		return false
	}); err != nil {
		t.Fatal(err)
	}
	if armed != 200*time.Millisecond {
		w.clock.Advance(armed)
		<-done
		t.Fatalf("launch allowed with 200ms remaining but the session was granted a %v watchdog", armed)
	}
	w.clock.Advance(armed)
	rep := <-done
	cs := phase(t, rep, "claude", PhaseDefault).Cases[0]
	if cs.Outcome != OutcomeInconclusive || deref(cs.Reason) != ReasonClientBudget || len(w.sessions()) != 2 || rep.Interrupted {
		t.Fatalf("case %+v, %d sessions", cs, len(w.sessions()))
	}
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

// C4: the observed version must equal the expected one exactly, from a
// successful capture; the decoder follows the validated observation.
func TestR1C4ExactVersion(t *testing.T) {
	for name, tc := range map[string]struct {
		m    vendorModel
		want string
	}{
		"substring":   {vendorModel{version: "unsupported release (previous: 2.1.282 (Claude Code))"}, ReasonVersionMismatch},
		"failed-exit": {vendorModel{versionExit: 1}, ReasonMetadataFailed},
		"extra-line":  {vendorModel{version: "2.1.282 (Claude Code)\nwarning: update available"}, ReasonVersionMismatch},
	} {
		w := newWorld(t, tc.m)
		rep := runPlan(t, newRunner(t, w, planWith(Phases{})), context.Background())
		c := rep.Clients[0]
		if len(w.sessions()) != 0 || !strings.HasPrefix(deref(c.Reason), tc.want) {
			t.Errorf("%s: %d sessions, reason %q", name, len(w.sessions()), deref(c.Reason))
		}
	}
	// Publication compares exactly as well.
	repo := tempRepo(t)
	base, _ := ReadCatalogBase(repo)
	r := newRunner(t, newWorld(t, fullModel()), planWith(Phases{}))
	rep := runPlan(t, r, context.Background())
	rep.Clients[0].ObservedVersion = sptr("preview " + *rep.Clients[0].ObservedVersion)
	if _, err := ProposePatch(rep, base, r.OutDir); contract.ExitCode(err) != 4 {
		t.Fatalf("a substring version was published: %v", err)
	}
}

// C5: progress conclusions need delivered, request-correlated progress.
func TestR1C5ProgressNeedsDeliveredProgress(t *testing.T) {
	for name, m := range map[string]vendorModel{
		"extends":         {timeoutMS: 500, raisedMS: 1000, resets: true, noProgress: true},
		"does-not-extend": {timeoutMS: 500, raisedMS: 1000, noProgress: true},
	} {
		p := planWith(Phases{Default: &DefaultPhase{DelaysMS: []int64{300, 900}}, Progress: &struct{}{}, Absolute: &DelayPhase{DelayMS: 1500}})
		rep := runPlan(t, newRunner(t, newWorld(t, m), p), context.Background())
		ph := phase(t, rep, "claude", PhaseProgress)
		if ph.Status != StatusInconclusive || ph.Cases[0].ProgressSent != 0 {
			t.Errorf("%s: zero messages classified as %s", name, ph)
		}
	}
}

// C6: the serialized, sanitized evidence stays within the per-file limit;
// overflow is inconclusive and a valid report is still written.
func TestR1C6EvidenceBoundAfterEscaping(t *testing.T) {
	m := fullModel()
	// A valid JSON string (an invalid line with a backslash is omitted
	// whole); it doubles when wrapped as a JSON string.
	quotes := `"` + strings.Repeat(`\"`, 1500) + `"`
	m.transcriptTail = quotes
	r := newRunner(t, newWorld(t, m), planWith(Phases{}))
	r.StdoutLimit, r.EvidenceLimit = 4096, 4096
	rep, err := r.Run(context.Background())
	if err != nil {
		t.Fatalf("no report: %v", err)
	}
	for _, ev := range rep.Evidence {
		if ev.Bytes > 4096 {
			t.Errorf("%s is %d bytes, over the 4096-byte limit", ev.Path, ev.Bytes)
		}
	}
	cs := phase(t, rep, "claude", PhaseSetup).Cases[0]
	if cs.Outcome != OutcomeInconclusive || deref(cs.Reason) != ReasonEvidenceTruncated {
		t.Errorf("overflowing evidence classified %s (%s)", cs.Outcome, deref(cs.Reason))
	}
	strictReport(t, r)
	_ = io.Discard
}
