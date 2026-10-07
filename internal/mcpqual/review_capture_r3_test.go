package mcpqual

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// Code review round 3 of design decoder-enrollment (C1, C2): each finding
// driven through full ValidateEnrollment (and, for C2, a capture run).

// executableHash is the manifest's client executable hash member.
var executableHash = regexp.MustCompile(`"executable_sha256": "[0-9a-f]{64}"`)

// R3-C1: the manifest whose hash the index pins is exactly the manifest
// enrollment validates and uses. The manifest is rewritten in place (same
// inode, so no identity check can tell) at its second open; the bytes
// used must be the hashed ones, so the manifest, like every hashed file,
// is opened once.
func TestReviewR3ManifestReadOnce(t *testing.T) {
	e := enrolledFixture(t)
	manifest := filepath.Join(filepath.FromSlash(e.entry.Bundle), CaptureManifestName)
	for _, kind := range []string{"root", "dirfs"} {
		t.Run(kind, func(t *testing.T) {
			repo := t.TempDir()
			writeFiles(t, repo, e.files)
			opts := EnrollmentOptions{Registry: e.reg, Root: repo}
			if kind == "dirfs" {
				opts = EnrollmentOptions{Registry: e.reg, FS: os.DirFS(repo)}
			}
			orig, err := os.ReadFile(filepath.Join(repo, manifest))
			if err != nil {
				t.Fatal(err)
			}
			forged := executableHash.ReplaceAll(orig, []byte(`"executable_sha256": "`+strings.Repeat("0", 64)+`"`))
			if string(forged) == string(orig) {
				t.Fatal("no executable hash in the manifest")
			}
			opens := map[string]int{}
			mutated := false
			openHook = func(n string) {
				opens[n]++
				if n == CaptureManifestName && opens[n] == 2 {
					// In place: the same inode, new bytes.
					f, err := os.OpenFile(filepath.Join(repo, manifest), os.O_WRONLY|os.O_TRUNC, 0)
					if err != nil {
						t.Fatal(err)
					}
					f.Write(forged)
					f.Close()
					mutated = true
				}
			}
			defer func() { openHook = nil }()
			_, err = ValidateEnrollment(opts)
			if mutated && err == nil {
				t.Fatal("enrollment accepted a manifest whose hash differs from the index")
			}
			if err != nil {
				t.Fatalf("enrollment: %v", err)
			}
			// Every hashed file (the index, the manifest, expected.json and
			// each payload) is opened exactly once.
			for n, c := range opens {
				if strings.Contains(n, ".") && c != 1 {
					t.Errorf("%s opened %d times", n, c)
				}
			}
			for _, n := range []string{"index.json", CaptureManifestName, ExpectedName, FileServerEvents, FileHelpStdout} {
				if opens[n] != 1 {
					t.Errorf("%s opened %d times", n, opens[n])
				}
			}
		})
	}
	// A manifest altered before its one read fails the index hash.
	repo := t.TempDir()
	writeFiles(t, repo, e.files)
	orig, _ := os.ReadFile(filepath.Join(repo, manifest))
	forged := executableHash.ReplaceAll(orig, []byte(`"executable_sha256": "`+strings.Repeat("0", 64)+`"`))
	if err := os.WriteFile(filepath.Join(repo, manifest), forged, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := ValidateEnrollment(EnrollmentOptions{Root: repo, Registry: e.reg}); err == nil || !strings.Contains(err.Error(), "hash differs from the index") {
		t.Fatalf("an altered manifest enrolled: %v", err)
	}
}

// R3-C2: every probe instance closes (its exit event) before the next
// starts, and the receipt-bearing instance has an intact ending; a later
// instance's exit does not stand in for it.
func TestReviewR3ProbeInstanceClosed(t *testing.T) {
	const id = "claude-capture-setup"
	name, ver := "client-a", "1"
	start := ProbeEvent{Kind: EvStart}
	initA := ProbeEvent{Kind: EvInitialize, ClientName: &name, ClientVersion: &ver}
	recv := ProbeEvent{Kind: EvReceipt, CaseID: id, RequestID: json.RawMessage(`1`)}
	done := ProbeEvent{Kind: EvCompleted, CaseID: id, RequestID: json.RawMessage(`1`)}
	exit := ProbeEvent{Kind: EvExit, Reason: "eof"}
	for label, log := range map[string]string{
		// The reviewer's sequence.
		"receipt instance unclosed": probeLog("r", start, initA, recv, done) + probeLog("r", start, exit),
		// An earlier instance without a receipt, unclosed.
		"earlier instance unclosed": probeLog("r", start, initA) + probeLog("r", start, initA, recv, done, exit),
	} {
		p := observeProbe([]byte(log), false, id)
		if !strings.Contains(strings.Join(p.Anomalies, ","), "instance_unclosed") {
			t.Errorf("%s: %+v", label, p)
		}
	}
	// The receipt-bearing instance last, without its exit: not intact (the
	// existing probe-incomplete classification).
	if p := observeProbe([]byte(probeLog("r", start, initA, exit)+probeLog("r", start, initA, recv, done)), false, id); p.Intact {
		t.Errorf("a final receipt instance without its exit: %+v", p)
	}
	// Closed instances are accepted.
	if p := observeProbe([]byte(probeLog("r", start, initA, exit)+probeLog("r", start, initA, recv, done, exit)), false, id); len(p.Anomalies) != 0 || !p.Completed {
		t.Errorf("two closed instances: %+v", p)
	}
	// Through a capture run.
	w := newCapWorld(t)
	w.script = func(spec ProcSpec) capBehavior {
		b := w.defaults(spec)
		if launchKind(spec) == "session" {
			b.events = func(cf CaseFile, c string) string {
				return probeLog(cf.RunID, start, initA, ProbeEvent{Kind: EvReceipt, CaseID: c, RequestID: json.RawMessage(`1`)},
					ProbeEvent{Kind: EvCompleted, CaseID: c, RequestID: json.RawMessage(`1`)}) + probeLog(cf.RunID, start, exit)
			}
		}
		return b
	}
	man, _ := runCapture(t, newCapRunner(t, w, capPlan(t, "claude")), context.Background())
	if r := reasonOf(capClient(t, man, "claude")); man.State == CaptureComplete || !strings.HasPrefix(r, ReasonProbeAnomaly) {
		t.Errorf("capture %s with an unclosed receipt-bearing instance (reason %q)", man.State, r)
	}
	// Through replay in full enrollment: the fixture's own instance without
	// its eof and exit, then an instance that only starts and exits.
	e := enrolledFixture(t)
	rel := ClientFile("claude", FileServerEvents)
	var evs []ProbeEvent
	for _, l := range strings.Split(strings.TrimSuffix(string(e.bundle.Files[rel]), "\n"), "\n") {
		var ev ProbeEvent
		if err := json.Unmarshal([]byte(l), &ev); err != nil {
			t.Fatal(err)
		}
		if ev.Kind != EvEOF && ev.Kind != EvExit {
			evs = append(evs, ev)
		}
	}
	forged := probeLog(evs[0].RunID, evs...) + probeLog(evs[0].RunID, start, exit)
	opts, _ := reenrolled(t, map[string][]byte{rel: []byte(forged)}, nil)
	if _, err := ValidateEnrollment(opts); err == nil || !strings.Contains(err.Error(), "probe replay") {
		t.Errorf("enrollment accepted an unclosed receipt-bearing instance: %v", err)
	}
}
