package reale2e

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"

	git "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing/filemode"
	"github.com/go-git/go-git/v5/storage/memory"
)

// Design 12a-dev-flow-verify r0.11 (fifth probe, FP-1, FP-3, FP-8): a
// rejected publication inspection retains its rejection and the rejected
// result commit's root review.md before cleanup; diagnostics never admit
// and never change the verdict.

// TestDevFullRejectionCapture covers every capture state on synthetic
// result commits.
func TestDevFullRejectionCapture(t *testing.T) {
	t.Parallel()
	st := memory.NewStorage()
	repo, err := git.Init(st, nil)
	if err != nil {
		t.Fatal(err)
	}
	commit := func(files map[string][]byte, modes map[string]filemode.FileMode) string {
		h, _ := writeCommitMode(t, st, files, modes)
		return h.String()
	}
	big := bytes.Repeat([]byte("r"), maxRejectedReviewBytes+10)
	invalid := []byte("# Review\n\xff\xfe not UTF-8\n")
	for name, c := range map[string]struct {
		commit string
		state  string
		data   []byte
	}{
		"complete":     {commit(map[string][]byte{"review.md": []byte("# Review\n"), "main.go": []byte("x")}, nil), captureComplete, []byte("# Review\n")},
		"zero_byte":    {commit(map[string][]byte{"review.md": {}}, nil), captureComplete, []byte{}},
		"invalid_utf8": {commit(map[string][]byte{"review.md": invalid}, nil), captureComplete, invalid},
		"oversized":    {commit(map[string][]byte{"review.md": big}, nil), captureTruncated, big[:maxRejectedReviewBytes]},
		"absent":       {commit(map[string][]byte{"main.go": []byte("x")}, nil), captureAbsent, nil},
		"symlink":      {commit(map[string][]byte{"review.md": []byte("/etc/passwd")}, map[string]filemode.FileMode{"review.md": filemode.Symlink}), captureUnsafe, nil},
		"no_commit":    {strings.Repeat("ab", 20), captureUnavailable, nil},
	} {
		got, data := captureRootReview(repo, c.commit)
		if got.State != c.state || !bytes.Equal(data, c.data) || (c.data == nil) != (data == nil) {
			t.Errorf("%s: %+v %d bytes", name, got, len(data))
			continue
		}
		if code := checkReviewCapture(got, data, data != nil); code != "" {
			t.Errorf("%s: own capture refused %s", name, code)
		}
		if c.state == captureTruncated && *got.Size != int64(len(big)) {
			t.Errorf("%s: size %d", name, *got.Size)
		}
	}
	if got, data := captureRejectedReview(t.TempDir(), strings.Repeat("ab", 20), errors.New("pull failed")); got.State != captureUnavailable ||
		data != nil || got.CaptureError == nil || !strings.Contains(*got.CaptureError, "pull failed") {
		t.Fatalf("a failed pull %+v", got)
	}
	// The descriptor and the bytes must agree.
	ok, okData := captureRootReview(repo, commit(map[string][]byte{"review.md": []byte("# R\n")}, nil))
	mode := func(rc ReviewCapture, m filemode.FileMode) ReviewCapture {
		s := m.String()
		rc.Mode = &s
		return rc
	}
	trunc, truncData := captureRootReview(repo, commit(map[string][]byte{"review.md": big}, nil))
	for name, c := range map[string]struct {
		rc   ReviewCapture
		data []byte
		has  bool
	}{
		"changed_bytes":   {ok, []byte("# X\n"), true},
		"missing_bytes":   {ok, nil, false},
		"bytes_on_absent": {ReviewCapture{State: captureAbsent}, okData, true},
		"unknown_state":   {ReviewCapture{State: "partial"}, nil, false},
		"error_missing":   {ReviewCapture{State: captureUnavailable}, nil, false},
		// Bytes come from a regular-file blob only; unsafe is nonregular.
		"complete_symlink": {mode(ok, filemode.Symlink), okData, true},
		"truncated_dir":    {mode(trunc, filemode.Dir), truncData, true},
		"no_mode":          {ReviewCapture{State: ok.State, Size: ok.Size, CapturedBytes: ok.CapturedBytes, CapturedSHA256: ok.CapturedSHA256}, okData, true},
		"unsafe_regular":   {mode(ReviewCapture{State: captureUnsafe}, filemode.Regular), nil, false},
		"unsafe_exec":      {mode(ReviewCapture{State: captureUnsafe}, filemode.Executable), nil, false},
		"unsafe_no_mode":   {ReviewCapture{State: captureUnsafe}, nil, false},
	} {
		if checkReviewCapture(c.rc, c.data, c.has) == "" {
			t.Errorf("%s accepted", name)
		}
	}
	for name, rc := range map[string]ReviewCapture{"complete_exec": mode(ok, filemode.Executable), "unsafe_symlink": mode(ReviewCapture{State: captureUnsafe}, filemode.Symlink),
		"unsafe_submodule": mode(ReviewCapture{State: captureUnsafe}, filemode.Submodule)} {
		data := okData
		if rc.State == captureUnsafe {
			data = nil
		}
		if code := checkReviewCapture(rc, data, data != nil); code != "" {
			t.Errorf("%s refused %s", name, code)
		}
	}
	if s, cut := boundText(strings.Repeat("é", 3000), maxRejectionReasonBytes); !cut || len(s) > maxRejectionReasonBytes || !strings.HasPrefix(strings.Repeat("é", 3000), s) {
		t.Fatalf("reason bound %d %t", len(s), cut)
	}
}

// rejectionRecord reads a bundle's task rejection record.
func rejectionRecord(t *testing.T, dir, task string) DevPublicationRejection {
	t.Helper()
	var rec devEnvelope[DevPublicationRejection]
	if err := json.Unmarshal(mustRead(t, filepath.Join(dir, filepath.FromSlash(task), publicationRejectionFile)), &rec); err != nil {
		t.Fatal(err)
	}
	return rec.Data
}

// rejectedBundles caches one rejected full-flow bundle per scripted
// alteration (the live collector's own output), copied for every
// mutation so the offline checks never rerun the attempt.
var rejectedBundles struct {
	sync.Mutex
	m map[string]*rejectedBase
}

type rejectedBase struct {
	sync.Once
	dir string
	err error
}

// rejectionSteps is a four-task flow whose code review publishes with
// alter.
func rejectionSteps(alter string) []devAct {
	return []devAct{{marker: markerDesignDraft}, {marker: markerDesignApproved}, {marker: markerImplComplete}, {marker: markerReviewApproved, alter: alter}}
}

// rejectedBundle returns a fresh copy of the cached bundle whose code
// review published with alter and stopped with stop.
func rejectedBundle(t *testing.T, alter, stop string) string {
	t.Helper()
	rejectedBundles.Lock()
	if rejectedBundles.m == nil {
		rejectedBundles.m = map[string]*rejectedBase{}
	}
	base := rejectedBundles.m[alter]
	if base == nil {
		base = &rejectedBase{err: errors.New("the rejected attempt did not complete")}
		rejectedBundles.m[alter] = base
	}
	rejectedBundles.Unlock()
	base.Do(func() {
		r := newDevRig(t, rejectionSteps(alter))
		r.owner = []string{"yes 1"}
		if code := r.run(); code != 1 || r.d.stopCode != stop {
			base.err = fmt.Errorf("the %s attempt stopped %d %s", alter, code, r.d.stopCode)
			return
		}
		base.dir = filepath.Join(bundleCacheRoot, "dev-rejected-"+alter)
		base.err = copyTree(r.f.bundle(), base.dir)
	})
	if base.err != nil {
		t.Fatal(base.err)
	}
	dst := filepath.Join(t.TempDir(), "rejected")
	if err := copyTree(base.dir, dst); err != nil {
		t.Fatal(err)
	}
	return dst
}

// editRejection edits a retained rejection record's data.
func editRejection(t *testing.T, dir, ns string, f func(d map[string]any)) {
	t.Helper()
	editJSON(t, dir, ns+"/"+publicationRejectionFile, func(m map[string]any) { f(m["data"].(map[string]any)) })
}

// rejectionRows checks a bundle's two rejection rows for task 04: the
// record row verified, and the reconstruction row correct when the
// retained inputs reproduce the rejection, otherwise unobserved, citing
// the inputs it used (the review's when rechecked); a row whose
// observation leaves anything unobserved never reads correct.
func rejectionRows(t *testing.T, dir string, reproduced, rechecked bool) {
	t.Helper()
	a := bundleDoc[DevAudit](t, dir, "audit.json")
	n := 0
	for _, r := range a.Rows {
		if !strings.HasPrefix(r.ID, "task/04/publication-rejection") {
			continue
		}
		n++
		if (strings.Contains(r.Observation, "unobserved") || strings.Contains(r.Observation, "not retained")) && r.Verdict != VerdictUnobserved {
			t.Fatalf("row %s reads %s while its details are unobserved: %s", r.ID, r.Verdict, r.Observation)
		}
	}
	if row := auditRow(t, dir, "task/04/publication-rejection"); row.Verdict != VerdictCorrect {
		t.Fatalf("record row %+v", row)
	}
	want := VerdictUnobserved
	if reproduced {
		want = VerdictCorrect
	}
	row := auditRow(t, dir, "task/04/publication-rejection-recheck")
	if row.Verdict != want || n != 2 {
		t.Fatalf("reconstruction row (%d rows) %+v", n, row)
	}
	// The reconstruction cites what it used, each a real bundle file with
	// its digest: the retained record and review bytes and, when the
	// review checks ran, the review's log and its metadata, the task, the
	// helper recipe, the candidate's tree and the helper receipt's lines.
	cited := map[string]bool{}
	receipt := false
	for _, src := range row.Sources {
		data, err := os.ReadFile(filepath.Join(dir, filepath.FromSlash(src.File)))
		if err != nil || sha256Hex(data) != src.SHA256 {
			t.Fatalf("source %+v does not name a bundle file: %v", src, err)
		}
		cited[src.File] = true
		receipt = receipt || src.File == "tasks/04/logs.txt" && len(src.Lines) == 2 && src.Lines[0] > 0 && src.Lines[1] >= src.Lines[0]
	}
	want2 := []string{"tasks/04/" + publicationRejectionFile}
	if _, err := os.Stat(filepath.Join(dir, "tasks", "04", rejectedReviewFile)); err == nil {
		want2 = append(want2, "tasks/04/"+rejectedReviewFile)
	}
	if rechecked {
		want2 = append(want2, "tasks/04/logs.txt", "tasks/04/logs.json", "tasks/04/task.json", devManifestFile, devTreesFile)
	}
	for _, f := range want2 {
		if !cited[f] {
			t.Fatalf("the reconstruction row does not cite %s: %+v", f, row.Sources)
		}
	}
	if rechecked != receipt {
		t.Fatalf("the reconstruction row's receipt citation %t: %+v", receipt, row.Sources)
	}
}

// checkedRejection loads a rejected bundle and runs its full check once,
// requiring the attempt's stop.
func checkedRejection(t *testing.T, dir, stop string) (*DevBundle, *devChecker) {
	t.Helper()
	b, err := LoadDevBundle(dir)
	if err != nil {
		t.Fatal(err)
	}
	c := newDevChecker(b)
	c.run()
	if c.stop == nil || c.stop.code != stop {
		t.Fatalf("the checked stop %+v", c.stop)
	}
	return b, c
}

// mutatedBundle is b with rel's bytes replaced by mutate's (nil removes
// the file) and re-decoded; b itself is unchanged.
func mutatedBundle(t *testing.T, b *DevBundle, rel string, mutate func([]byte) []byte) *DevBundle {
	t.Helper()
	nb := *b
	nb.Files, nb.docs = maps.Clone(b.Files), maps.Clone(b.docs)
	delete(nb.docs, rel)
	data := mutate(b.Files[rel])
	if data == nil {
		delete(nb.Files, rel)
		return &nb
	}
	nb.Files[rel] = data
	if err := nb.decodeOne(rel, data); err != nil {
		t.Fatalf("%s: %v", rel, err)
	}
	return &nb
}

// rejectionData is a record mutation of the envelope's data.
func rejectionData(t *testing.T, f func(d map[string]any)) func([]byte) []byte {
	return func(data []byte) []byte {
		t.Helper()
		var m map[string]any
		if err := json.Unmarshal(data, &m); err != nil {
			t.Fatal(err)
		}
		f(m["data"].(map[string]any))
		out, err := json.Marshal(m)
		if err != nil {
			t.Fatal(err)
		}
		return out
	}
}

// rewriteJSONFile applies a byte mutation to a bundle file.
func rewriteJSONFile(t *testing.T, dir, rel string, mutate func([]byte) []byte) {
	t.Helper()
	p := filepath.Join(dir, filepath.FromSlash(rel))
	if err := os.WriteFile(p, mutate(mustRead(t, p)), 0o600); err != nil {
		t.Fatal(err)
	}
}

// rejectionContradictions are the codes a diagnostic's own faults raise.
var rejectionContradictions = []string{"rejection_record_invalid", "rejection_linkage_mismatch", "rejection_capture_invalid", "rejection_capture_mismatch",
	"rejection_unreproduced"}

// testPublicationRejection drives full-flow rejections and checks the
// retained diagnostics live and offline (TestDevFullVerifyFP3 hosts it).
func testPublicationRejection(t *testing.T) {
	for name, c := range map[string]struct {
		alter, stop, reason string
		reproduced          bool
	}{
		// A bad review field (the publication check refuses it, offline
		// too, from the retained review.md).
		"review_field": {"blocker-mismatch", "publication_rejected", "review blocker", true},
		// A published-file failure before whole-tree admission: the
		// retained review.md passes its checks, so the refusal itself
		// stays unobserved.
		"scratch_remnant": {"scratch", "publication_rejected", "scratch remnant", false},
		// The reviewer evidence of pasted lines no longer reconciling is
		// TestDevFullVerifyFP4/rejection_retained, with these checks.
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			r := newDevRig(t, rejectionSteps(c.alter))
			r.owner = []string{"yes 1"}
			r.expect(1, c.stop)
			rec := rejectionRecord(t, r.f.evidence, "tasks/04")
			bin := mustRead(t, filepath.Join(r.f.evidence, "tasks", "04", rejectedReviewFile))
			if rec.Code != c.stop || !strings.Contains(rec.Reason, c.reason) || rec.Role != "code-reviewer" || rec.Ordinal != 4 ||
				rec.ResultCommit == nil || rec.Review.State != captureComplete || !bytes.Equal(bin, r.lastReport) {
				t.Fatalf("record %+v", rec)
			}
			code, rep := r.offline()
			pubs := devCriterion(rep, "publications").Codes
			if code != 1 || rep.Result == ResultPass {
				t.Fatalf("offline %d %v", code, devCodes(rep))
			}
			for _, bad := range rejectionContradictions {
				if containsCode(pubs, bad) {
					t.Fatalf("an honest record fails %s: %v", bad, devCodes(rep))
				}
			}
			rejectionRows(t, r.f.bundle(), c.reproduced, true)
			if containsCode(pubs, "rejection_unobserved") == c.reproduced {
				t.Fatalf("reconstruction %v", pubs)
			}
		})
	}
	// A capable forger's record: each field, bound and capture changed on
	// its own over the cached rejected bundle, judged by the shared
	// validator in memory (one load and check, no rederivation per
	// mutation); then a few recomputed on disk through the full check.
	t.Run("tampered", func(t *testing.T) {
		t.Parallel()
		const ns = "tasks/04"
		b0, c0 := checkedRejection(t, rejectedBundle(t, "blocker-mismatch", "publication_rejected"), "publication_rejected")
		honest := judgeRejection(c0.rejectionInput(4, "publication_rejected"))
		if !honest.present || len(honest.bad) != 0 || honest.recheck != "" || honest.unobserved != "" {
			t.Fatalf("the honest record %+v", honest)
		}
		rec, bin := ns+"/"+publicationRejectionFile, ns+"/"+rejectedReviewFile
		for name, c := range map[string]struct {
			rel    string
			mutate func([]byte) []byte
			code   string
		}{
			"bytes":         {bin, func([]byte) []byte { return []byte("# Review\nall fine\n") }, "rejection_capture_invalid"},
			"no_bytes":      {bin, func([]byte) []byte { return nil }, "rejection_capture_invalid"},
			"no_record":     {rec, func([]byte) []byte { return nil }, "rejection_record_invalid"},
			"hash":          {rec, rejectionData(t, func(d map[string]any) { d["review"].(map[string]any)["captured_sha256"] = strings.Repeat("0", 64) }), "rejection_capture_invalid"},
			"size":          {rec, rejectionData(t, func(d map[string]any) { d["review"].(map[string]any)["size"] = 3 }), "rejection_capture_invalid"},
			"commit":        {rec, rejectionData(t, func(d map[string]any) { d["result_commit"] = strings.Repeat("ab", 20) }), "rejection_linkage_mismatch"},
			"no_commit":     {rec, rejectionData(t, func(d map[string]any) { d["result_commit"] = nil }), "rejection_linkage_mismatch"},
			"task":          {rec, rejectionData(t, func(d map[string]any) { d["task"] = hexID("t_", 77) }), "rejection_linkage_mismatch"},
			"ordinal":       {rec, rejectionData(t, func(d map[string]any) { d["ordinal"] = 3 }), "rejection_linkage_mismatch"},
			"role":          {rec, rejectionData(t, func(d map[string]any) { d["role"] = HopCoder }), "rejection_linkage_mismatch"},
			"expected_base": {rec, rejectionData(t, func(d map[string]any) { d["expected_base"] = strings.Repeat("cd", 20) }), "rejection_linkage_mismatch"},
			"stop_code":     {rec, rejectionData(t, func(d map[string]any) { d["code"] = "stale_receipt" }), "rejection_linkage_mismatch"},
			"reason_4097":   {rec, rejectionData(t, func(d map[string]any) { d["reason"] = strings.Repeat("r", maxRejectionReasonBytes+1) }), "rejection_record_invalid"},
			"reason_empty":  {rec, rejectionData(t, func(d map[string]any) { d["reason"] = "" }), "rejection_record_invalid"},
			"mode_symlink":  {rec, rejectionData(t, func(d map[string]any) { d["review"].(map[string]any)["mode"] = filemode.Symlink.String() }), "rejection_capture_invalid"},
			"mode_dir":      {rec, rejectionData(t, func(d map[string]any) { d["review"].(map[string]any)["mode"] = filemode.Dir.String() }), "rejection_capture_invalid"},
			"run_id": {rec, func(data []byte) []byte {
				var m map[string]any
				json.Unmarshal(data, &m)
				m["run_id"] = "dev-other"
				out, _ := json.Marshal(m)
				return out
			}, "rejection_linkage_mismatch"},
		} {
			in := c0.rejectionInput(4, "publication_rejected")
			in.b = mutatedBundle(t, b0, c.rel, c.mutate)
			if v := judgeRejection(in); !slices.Contains(v.bad, c.code) || v.unobserved == "" || v.recheck != "" {
				t.Errorf("%s: %+v", name, v)
			}
		}
		// A record bound to its stop whose retained review.md recomputes
		// another refusal is contradicted (the stop given as the
		// record's, so only the reconstruction disagrees).
		in := c0.rejectionInput(4, "stale_receipt")
		in.b = mutatedBundle(t, b0, rec, rejectionData(t, func(d map[string]any) { d["code"] = "stale_receipt" }))
		if v := judgeRejection(in); len(v.bad) != 0 || v.recheck != "rejection_unreproduced" || !strings.Contains(v.note, "publication_rejected") {
			t.Fatalf("unreproduced %+v", v)
		}
		// The review's inputs not retained: the reconstruction stays
		// unobserved, no verdict invented.
		in = c0.rejectionInput(4, "publication_rejected")
		if in.review == nil {
			t.Fatal("the review's retained inputs were not found")
		}
		in.review = nil
		if v := judgeRejection(in); !v.present || len(v.bad) != 0 || v.recheck != "" || !strings.Contains(v.unobserved, "not retained") {
			t.Fatalf("missing inputs %+v", v)
		}
	})
	// The shared validator's codes reach the report and the record row
	// through the full check, the derived files recomputed on disk.
	t.Run("tampered_recomputed", func(t *testing.T) {
		t.Parallel()
		const ns = "tasks/04"
		for name, c := range map[string]struct {
			mutate func(dir string)
			code   string
		}{
			"reason_4097": {func(dir string) {
				editRejection(t, dir, ns, func(d map[string]any) { d["reason"] = strings.Repeat("r", maxRejectionReasonBytes+1) })
			}, "rejection_record_invalid"},
			"mode_symlink": {func(dir string) {
				editRejection(t, dir, ns, func(d map[string]any) { d["review"].(map[string]any)["mode"] = filemode.Symlink.String() })
			}, "rejection_capture_invalid"},
		} {
			dir := rejectedBundle(t, "blocker-mismatch", "publication_rejected")
			c.mutate(dir)
			code, rep := reforge(t, dir)
			if code != 1 || !containsCode(devCriterion(rep, "publications").Codes, c.code) {
				t.Fatalf("%s: %d %v", name, code, devCodes(rep))
			}
			if row := auditRow(t, dir, "task/04/publication-rejection"); row.Verdict != VerdictIncorrect || !slices.Contains(row.Codes, c.code) {
				t.Fatalf("%s: record row %+v", name, row)
			}
		}
		// Raw bytes (invalid UTF-8) are admitted for the review capture
		// alone, within 32 KiB.
		for name, data := range map[string][]byte{"invalid_utf8": []byte("# R\n\xff\xfe\n"), "over_bound": bytes.Repeat([]byte("r"), maxRejectedReviewBytes+1)} {
			dir := rejectedBundle(t, "blocker-mismatch", "publication_rejected")
			os.WriteFile(filepath.Join(dir, "tasks", "04", rejectedReviewFile), data, 0o600)
			inv, _ := diskInventory(dir)
			editJSON(t, dir, devManifestFile, func(m map[string]any) {
				var raw []any
				b, _ := json.Marshal(inv)
				json.Unmarshal(b, &raw)
				m["inventory"] = raw
			})
			if _, err := LoadDevBundle(dir); (err == nil) != (name == "invalid_utf8") {
				t.Errorf("%s: load %v", name, err)
			}
		}
		// An older bundle without the diagnostics stays readable and
		// failed, the diagnostics unavailable rather than invented.
		dir := rejectedBundle(t, "blocker-mismatch", "publication_rejected")
		os.Remove(filepath.Join(dir, "tasks", "04", publicationRejectionFile))
		os.Remove(filepath.Join(dir, "tasks", "04", rejectedReviewFile))
		if code, rep := reforge(t, dir); code != 1 || rep.Result == ResultPass {
			t.Fatalf("old bundle %d %v", code, devCodes(rep))
		}
		// No other diagnostic name is admitted.
		os.WriteFile(filepath.Join(dir, "tasks", "04", "rejected-review.txt"), []byte("x"), 0o600)
		if _, err := LoadDevBundle(dir); err == nil {
			t.Fatal("an arbitrary diagnostic name was admitted")
		}
	})
	// A consistent capture that cannot be rechecked (truncated, absent or
	// unavailable) is a verified record whose reconstruction stays
	// unobserved, never correct: in memory for each state, and through
	// the full check's rows for one.
	t.Run("unreconstructable", func(t *testing.T) {
		t.Parallel()
		const ns = "tasks/04"
		rec, bin := ns+"/"+publicationRejectionFile, ns+"/"+rejectedReviewFile
		big := bytes.Repeat([]byte("r"), maxRejectedReviewBytes)
		truncate := rejectionData(t, func(d map[string]any) {
			rc := d["review"].(map[string]any)
			rc["state"], rc["size"], rc["captured_bytes"], rc["captured_sha256"] = captureTruncated, maxRejectedReviewBytes+5, len(big), sha256Hex(big)
		})
		dir := rejectedBundle(t, "scratch", "publication_rejected")
		b0, c0 := checkedRejection(t, dir, "publication_rejected")
		if v := judgeRejection(c0.rejectionInput(4, "publication_rejected")); len(v.bad) != 0 || v.recheck != "" || !strings.Contains(v.unobserved, "passes its review checks") {
			t.Fatalf("the passing review.md %+v", v)
		}
		for name, c := range map[string]struct {
			rec   func([]byte) []byte
			bin   []byte
			state string
		}{
			"truncated": {truncate, big, captureTruncated},
			"absent":    {rejectionData(t, func(d map[string]any) { d["review"] = map[string]any{"state": captureAbsent} }), nil, captureAbsent},
			"unavailable": {rejectionData(t, func(d map[string]any) {
				d["review"] = map[string]any{"state": captureUnavailable, "capture_error": "pull failed"}
			}), nil, captureUnavailable},
			"unsafe": {rejectionData(t, func(d map[string]any) {
				d["review"] = map[string]any{"state": captureUnsafe, "mode": filemode.Symlink.String()}
			}), nil, captureUnsafe},
		} {
			in := c0.rejectionInput(4, "publication_rejected")
			in.b = mutatedBundle(t, mutatedBundle(t, b0, rec, c.rec), bin, func([]byte) []byte { return c.bin })
			if v := judgeRejection(in); len(v.bad) != 0 || v.recheck != "" || !strings.Contains(v.unobserved, c.state) {
				t.Errorf("%s: %+v", name, v)
			}
		}
		os.WriteFile(filepath.Join(dir, "tasks", "04", rejectedReviewFile), big, 0o600)
		rewriteJSONFile(t, dir, rec, truncate)
		code, rep := reforge(t, dir)
		pubs := devCriterion(rep, "publications").Codes
		if code != 1 || !containsCode(pubs, "rejection_unobserved") {
			t.Fatalf("%d %v", code, devCodes(rep))
		}
		for _, bad := range rejectionContradictions {
			if containsCode(pubs, bad) {
				t.Fatalf("a consistent capture fails %s: %v", bad, devCodes(rep))
			}
		}
		rejectionRows(t, dir, false, false)
	})
	t.Run("write_failure", func(t *testing.T) {
		t.Parallel()
		// The diagnostic cannot be written (its path is taken): the
		// original refusal stands, nothing is admitted, and the write
		// failure is reported apart.
		r := newDevRig(t, rejectionSteps("blocker-mismatch"))
		r.owner = []string{"yes 1"}
		r.onTick = func(r *devRig) {
			if len(r.d.state.Tasks) == 3 {
				os.MkdirAll(filepath.Join(r.f.evidence, "tasks", "04", rejectedReviewFile), 0o700)
			}
		}
		if got := r.run(); got != 1 || r.d.stopCode != "publication_rejected" || len(r.d.state.Tasks) != 3 || r.d.s.ev.failure() == nil ||
			!strings.Contains(r.f.stdout.String(), "could not be retained") {
			t.Fatalf("exit %d stop %s tasks %d failure %v", got, r.d.stopCode, len(r.d.state.Tasks), r.d.s.ev.failure())
		}
	})
}

// containsCode reports code in codes.
func containsCode(codes []string, code string) bool {
	for _, c := range codes {
		if c == code {
			return true
		}
	}
	return false
}

// auditRow is a bundle's audit row by id.
func auditRow(t *testing.T, dir, id string) DevAuditRow {
	t.Helper()
	a := bundleDoc[DevAudit](t, dir, "audit.json")
	for _, r := range a.Rows {
		if r.ID == id {
			return r
		}
	}
	t.Fatalf("no audit row %s", id)
	return DevAuditRow{}
}

// TestDevQualificationRejection checks the reviewer probe's retained
// rejection with the full flow's validator (design 12a-dev-flow-verify
// r0.11, FP-8): each identity field, bound and capture mode changed on its
// own in a copy of one rejected qualification record is a contradiction,
// and the honest record is not.
func TestDevQualificationRejection(t *testing.T) {
	t.Parallel()
	r := probeRig(t)
	r.steps[3].alter = "extra-exit"
	r.expect(1, CodeReviewMismatch)
	const ns = "probe/reviewer/01"
	base := r.f.bundle()
	if v := probeRejection(mustLoadQualification(t, base), ptrQualification(t, base)); !v.present || len(v.bad) != 0 || v.recheck != "" ||
		v.unobserved != "" || !strings.Contains(v.note, "reproduced "+CodeReviewMismatch) {
		t.Fatalf("the honest record %+v", v)
	}
	codes := qualificationCodes(t, base)
	for _, bad := range rejectionContradictions {
		if slices.Contains(codes, bad) {
			t.Fatalf("the honest record fails %s: %v", bad, codes)
		}
	}
	for name, c := range map[string]struct {
		mutate func(dir string)
		code   string
	}{
		"task":    {func(dir string) { editRejection(t, dir, ns, func(d map[string]any) { d["task"] = hexID("t_", 77) }) }, "rejection_linkage_mismatch"},
		"ordinal": {func(dir string) { editRejection(t, dir, ns, func(d map[string]any) { d["ordinal"] = 2 }) }, "rejection_linkage_mismatch"},
		"role":    {func(dir string) { editRejection(t, dir, ns, func(d map[string]any) { d["role"] = HopCoder }) }, "rejection_linkage_mismatch"},
		"expected_base": {func(dir string) {
			editRejection(t, dir, ns, func(d map[string]any) { d["expected_base"] = strings.Repeat("cd", 20) })
		}, "rejection_linkage_mismatch"},
		"result_commit": {func(dir string) {
			editRejection(t, dir, ns, func(d map[string]any) { d["result_commit"] = strings.Repeat("ab", 20) })
		}, "rejection_linkage_mismatch"},
		"code": {func(dir string) {
			editRejection(t, dir, ns, func(d map[string]any) { d["code"] = "publication_rejected" })
		}, "rejection_linkage_mismatch"},
		"run_id": {func(dir string) {
			editJSON(t, dir, ns+"/"+publicationRejectionFile, func(m map[string]any) { m["run_id"] = "dev-other" })
		}, "rejection_linkage_mismatch"},
		"reason_4097": {func(dir string) {
			editRejection(t, dir, ns, func(d map[string]any) { d["reason"] = strings.Repeat("r", maxRejectionReasonBytes+1) })
		}, "rejection_record_invalid"},
		"mode_symlink": {func(dir string) {
			editRejection(t, dir, ns, func(d map[string]any) { d["review"].(map[string]any)["mode"] = filemode.Symlink.String() })
		}, "rejection_capture_invalid"},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			dir := filepath.Join(t.TempDir(), "probe")
			if err := copyTree(base, dir); err != nil {
				t.Fatal(err)
			}
			c.mutate(dir)
			requalify(t, dir)
			if codes := qualificationCodes(t, dir); !slices.Contains(codes, c.code) || !slices.Contains(codes, "publication_rejected") {
				t.Fatalf("codes %v", codes)
			}
		})
	}
}

// mustLoadQualification loads a qualification record.
func mustLoadQualification(t *testing.T, dir string) *DevBundle {
	t.Helper()
	b, err := LoadDevQualification(dir)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// ptrQualification is a record's qualification document.
func ptrQualification(t *testing.T, dir string) *DevQualification {
	t.Helper()
	q := doc[DevQualification](mustLoadQualification(t, dir), devQualificationFile)
	if q == nil {
		t.Fatal("no qualification document")
	}
	return q
}
