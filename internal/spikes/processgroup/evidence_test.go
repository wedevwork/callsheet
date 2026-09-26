//go:build linux || darwin

package processgroup

import (
	"bufio"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/wedevwork/callsheet/internal/testkit/fakeadapter"
)

func sigLine(pid int, sig string) string {
	b, _ := json.Marshal(map[string]any{"pid": pid, "signal": sig})
	return string(b) + "\n"
}

// oversized is a single line longer than the default Scanner token limit.
var oversized = strings.Repeat("x", bufio.MaxScanTokenSize+1) + "\n"

var errInjected = errors.New("injected read failure")

// failingReader yields prefix and then fails every later read.
type failingReader struct {
	prefix io.Reader
}

func (f *failingReader) Read(p []byte) (int, error) {
	if n, err := f.prefix.Read(p); n > 0 || err != io.EOF {
		return n, err
	}
	return 0, errInjected
}

func TestScanSignals(t *testing.T) {
	for name, c := range map[string]struct {
		in   string
		want string
	}{
		"empty":         {"", ""},
		"valid":         {sigLine(5, "SIGTERM") + sigLine(5, "SIGINT"), "SIGTERM,SIGINT"},
		"mixed pids":    {sigLine(6, "SIGINT") + sigLine(5, "SIGTERM") + sigLine(7, "SIGTERM"), "SIGTERM"},
		"malformed":     {"not json\n{\"pid\":5,\n" + sigLine(5, "SIGTERM") + "[]\n", "SIGTERM"},
		"no final line": {strings.TrimSuffix(sigLine(5, "SIGTERM"), "\n"), "SIGTERM"},
	} {
		got, err := scanSignals(strings.NewReader(c.in), 5)
		if err != nil || strings.Join(got, ",") != c.want {
			t.Fatalf("%s: %v %v, want %q", name, got, err, c.want)
		}
	}
	// A read error after a valid prefix returns the prefix and the error.
	got, err := scanSignals(&failingReader{strings.NewReader(sigLine(5, "SIGTERM"))}, 5)
	if !errors.Is(err, errInjected) || strings.Join(got, ",") != "SIGTERM" {
		t.Fatalf("read error after prefix = %v %v", got, err)
	}
	got, err = scanSignals(&failingReader{strings.NewReader("")}, 5)
	if !errors.Is(err, errInjected) || got != nil {
		t.Fatalf("read error at start = %v %v", got, err)
	}
}

func TestReadSignalsFailures(t *testing.T) {
	dir := t.TempDir()
	write := func(name, content string) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
		return p
	}
	// The default token limit is kept: an oversized line is an error, with
	// and without valid records before it.
	for name, c := range map[string]struct {
		content, want string
	}{
		"oversized alone":       {oversized, ""},
		"oversized after valid": {sigLine(5, "SIGTERM") + oversized + sigLine(5, "SIGINT"), "SIGTERM"},
	} {
		p := write(strings.ReplaceAll(name, " ", "-"), c.content)
		got, err := readSignals(p, 5)
		if !errors.Is(err, bufio.ErrTooLong) || !strings.Contains(err.Error(), "signal log "+p+": ") || strings.Join(got, ",") != c.want {
			t.Fatalf("%s = %v %v", name, got, err)
		}
	}
	// A path that opens but cannot be read (a directory) is a read error
	// wrapped with the path, not a missing log.
	sub := filepath.Join(dir, "is-a-dir")
	os.Mkdir(sub, 0o700)
	if got, err := readSignals(sub, 5); err == nil || got != nil || !strings.Contains(err.Error(), "signal log "+sub+": ") {
		t.Fatalf("directory = %v %v", got, err)
	}
	// Any open failure other than a missing file is reported.
	notDir := filepath.Join(write("plain", ""), "child")
	if _, err := readSignals(notDir, 5); err == nil || !strings.Contains(err.Error(), "signal log "+notDir+": ") {
		t.Fatalf("open error = %v", err)
	}
	if got, err := readSignals(filepath.Join(dir, "missing"), 5); got != nil || err != nil {
		t.Fatalf("missing = %v %v", got, err)
	}
	if fakeSignalSuffix != fakeadapter.SignalFileSuffix {
		t.Fatal("descendant signal log suffix differs from fakeadapter")
	}
}

// evidenceCase builds a cooperative good result without signal evidence and
// writes leader and descendant logs into a fresh directory.
func evidenceCase(t *testing.T, leaderLog, descLog string) (CaseResult, string) {
	t.Helper()
	r := goodResult("cooperative")
	r.LeaderSignals, r.DescendantSignals = nil, nil
	dir := t.TempDir()
	if leaderLog != "" {
		os.WriteFile(filepath.Join(dir, "signals.jsonl"), []byte(leaderLog), 0o600)
	}
	if descLog != "" {
		os.WriteFile(filepath.Join(dir, "signals.jsonl"+fakeadapter.SignalFileSuffix), []byte(descLog), 0o600)
	}
	return r, dir
}

// verdict applies collectSignals and the evaluation exactly as RunCase's
// deferred cleanup does.
func verdict(r *CaseResult, dir, goos string) {
	collectSignals(r, dir, r.LeaderPID, r.DescendantPID)
	evaluateFor(r, goos)
	r.Pass = len(r.Errors) == 0
}

// TestSignalEvidenceContract is the FP-5 contract test, executed by name from
// tests/function (TestHardeningSignalEvidence). Its leader and descendant
// subtests prove that a scanner failure fails the case even when valid
// SIGTERM acknowledgments were read before it; normal proves correct logs
// still pass. Do not rename or skip.
func TestSignalEvidenceContract(t *testing.T) {
	good := func(pid int) string { return sigLine(pid, "SIGTERM") }
	for _, goos := range []string{"linux", "darwin"} {
		t.Run("normal/"+goos, func(t *testing.T) {
			r, dir := evidenceCase(t, good(10)+sigLine(11, "SIGTERM"), good(11))
			verdict(&r, dir, goos)
			if !r.Pass || len(r.Errors) != 0 || strings.Join(r.LeaderSignals, ",") != "SIGTERM" || strings.Join(r.DescendantSignals, ",") != "SIGTERM" {
				t.Fatalf("normal evidence failed: %+v", r)
			}
		})
		t.Run("leader/"+goos, func(t *testing.T) {
			r, dir := evidenceCase(t, good(10)+oversized, good(11))
			verdict(&r, dir, goos)
			if r.Pass || len(r.Errors) != 1 || !strings.HasPrefix(r.Errors[0], "leader 10 signal evidence: signal log ") ||
				!strings.Contains(r.Errors[0], bufio.ErrTooLong.Error()) || strings.Join(r.LeaderSignals, ",") != "SIGTERM" || strings.Join(r.DescendantSignals, ",") != "SIGTERM" {
				t.Fatalf("leader scanner failure: pass=%v errors=%q signals=%v/%v", r.Pass, r.Errors, r.LeaderSignals, r.DescendantSignals)
			}
		})
		t.Run("descendant/"+goos, func(t *testing.T) {
			r, dir := evidenceCase(t, good(10), good(11)+oversized)
			verdict(&r, dir, goos)
			if r.Pass || len(r.Errors) != 1 || !strings.HasPrefix(r.Errors[0], "descendant 11 signal evidence: signal log ") ||
				strings.Join(r.LeaderSignals, ",") != "SIGTERM" || strings.Join(r.DescendantSignals, ",") != "SIGTERM" {
				t.Fatalf("descendant scanner failure: pass=%v errors=%q", r.Pass, r.Errors)
			}
		})
		t.Run("both/"+goos, func(t *testing.T) {
			// The second read still runs after the first one fails.
			r, dir := evidenceCase(t, oversized, oversized)
			verdict(&r, dir, goos)
			all := strings.Join(r.Errors, "\n")
			if r.Pass || !strings.Contains(all, "leader 10 signal evidence") || !strings.Contains(all, "descendant 11 signal evidence") {
				t.Fatalf("both failures: %q", r.Errors)
			}
		})
		t.Run("missing/"+goos, func(t *testing.T) {
			// Missing logs are not read errors, but the started case still
			// fails its acknowledgment assertions.
			r, dir := evidenceCase(t, "", "")
			verdict(&r, dir, goos)
			all := strings.Join(r.Errors, "\n")
			if r.Pass || strings.Contains(all, "signal evidence") || !strings.Contains(all, "leader did not acknowledge SIGTERM") || !strings.Contains(all, "descendant did not acknowledge SIGTERM") {
				t.Fatalf("missing logs: %q", r.Errors)
			}
		})
	}
	// Without a known descendant only the leader log is read.
	r, dir := evidenceCase(t, good(10), oversized)
	r.DescendantPID = 0
	collectSignals(&r, dir, r.LeaderPID, 0)
	if len(r.Errors) != 0 || r.DescendantSignals != nil || strings.Join(r.LeaderSignals, ",") != "SIGTERM" {
		t.Fatalf("no descendant: %+v", r)
	}
}

// TestPlatformSeamContract is the FP-2 contract test for processgroup,
// executed by name from tests/function (TestHardeningPlatformSeams). Its
// linux and darwin subtests run the full evaluate matrix and the helper's
// report labels for that goos on any host, without any syscall. Do not
// rename or skip.
func TestPlatformSeamContract(t *testing.T) {
	for _, goos := range []string{"linux", "darwin"} {
		t.Run(goos, func(t *testing.T) {
			checkEvaluateMatrix(t, goos)
			for _, goarch := range []string{"amd64", "arm64"} {
				for name, env := range map[string]map[string]string{
					"non-helper": {EnvFake: "x", EnvWorkDir: "y", EnvGroups: "z"},
					"incomplete": {EnvHelper: "1"},
				} {
					results := filepath.Join(t.TempDir(), "r.json")
					env[EnvResults] = results
					if code := runHelperFor(func(k string) string { return env[k] }, goos, goarch); code != 1 {
						t.Fatalf("%s %s/%s = %d", name, goos, goarch, code)
					}
					var rep Report
					b, _ := os.ReadFile(results)
					if err := json.Unmarshal(b, &rep); err != nil || rep.Platform != goos+"/"+goarch || len(rep.Errors) != 1 || rep.Subreaper || len(rep.Cases) != 0 {
						t.Fatalf("%s %s/%s report = %+v %v", name, goos, goarch, rep, err)
					}
				}
			}
		})
	}
}
