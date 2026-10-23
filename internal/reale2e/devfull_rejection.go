package reale2e

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"slices"
	"unicode/utf8"

	git "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/filemode"
	"github.com/go-git/go-git/v5/plumbing/object"

	"github.com/wedevwork/callsheet/internal/contract"
)

// Rejected-publication retention (design 12a-dev-flow-verify r0.11,
// FP-1, FP-3, FP-8): before a rejected publication inspection is stopped
// and cleaned up, its rejection and the rejected result commit's root
// review.md bytes are retained as diagnostics. They never admit anything
// and never change the verdict.

// Retained diagnostic file names in a task's evidence directory.
const (
	publicationRejectionFile = "publication-rejection.json"
	rejectedReviewFile       = "rejected-review.bin"
)

// Diagnostic bounds.
const (
	maxRejectionRecordBytes = 8 << 10
	maxRejectionReasonBytes = 4096
	maxRejectedReviewBytes  = 32 << 10
	maxCaptureErrorBytes    = 256
)

// Review capture states.
const (
	captureComplete    = "complete"
	captureTruncated   = "truncated"
	captureAbsent      = "absent"
	captureUnsafe      = "unsafe"
	captureUnavailable = "unavailable"
)

// DevPublicationRejection is publication-rejection.json's data.
type DevPublicationRejection struct {
	Task            string        `json:"task"`
	Ordinal         int           `json:"ordinal"`
	Role            string        `json:"role"`
	ExpectedBase    string        `json:"expected_base"`
	ResultCommit    *string       `json:"result_commit"`
	Code            string        `json:"code"`
	Reason          string        `json:"reason"`
	ReasonTruncated bool          `json:"reason_truncated"`
	Review          ReviewCapture `json:"review"`
}

// ReviewCapture describes the rejected result commit's root review.md:
// its state, its Git mode and original size when known, and the captured
// bytes' count and digest when bytes exist (rejected-review.bin).
type ReviewCapture struct {
	State          string  `json:"state"`
	Mode           *string `json:"mode,omitempty"`
	Size           *int64  `json:"size,omitempty"`
	CapturedBytes  *int    `json:"captured_bytes,omitempty"`
	CapturedSHA256 *string `json:"captured_sha256,omitempty"`
	CaptureError   *string `json:"capture_error,omitempty"`
}

// rejectInspection stops the attempt with code and reason for a rejected
// publication inspection, then retains its diagnostics before cleanup.
// The stop comes first, so the original code stands even when a
// diagnostic write fails; that failure is reported apart (a sticky
// evidence failure and its own message) and never admits anything.
func (d *devSupervisor) rejectInspection(in *devInspection, code, reason string) {
	d.stop(code, reason)
	dir := d.planDir(&in.plan)
	capture, data := captureRejectedReview(in.dir, in.commit, in.err)
	r, truncated := boundText(reason, maxRejectionReasonBytes)
	var commit *string
	if in.commit != "" {
		c := in.commit
		commit = &c
	}
	rec := envelope("publication-rejection", d.s.runID, DevPublicationRejection{Task: in.view.TaskID, Ordinal: in.plan.Ordinal, Role: in.plan.Role,
		ExpectedBase: in.plan.ExpectedBase, ResultCommit: commit, Code: code, Reason: r, ReasonTruncated: truncated, Review: capture})
	b, err := encodeJSON(rec)
	if err == nil && len(b) > maxRejectionRecordBytes {
		err = fmt.Errorf("the record exceeds %d bytes", maxRejectionRecordBytes)
	}
	if err == nil && data != nil {
		err = d.s.ev.writeFile(dir+"/"+rejectedReviewFile, data)
	}
	if err == nil {
		err = d.s.ev.writeFile(dir+"/"+publicationRejectionFile, b)
	}
	if err != nil {
		d.s.say("the rejected publication's diagnostics for task %s could not be retained: %s", in.view.TaskID, contract.SafeText(err.Error(), 200))
	}
}

// boundText is s cut to at most n bytes at a UTF-8 boundary, and whether
// it was cut.
func boundText(s string, n int) (string, bool) {
	if len(s) <= n {
		return s, false
	}
	i := n
	for i > 0 && !utf8.RuneStart(s[i]) {
		i--
	}
	return s[:i], true
}

// captureRejectedReview reads the rejected result commit's root review.md
// from the inspection repository at dir, never following a link or
// touching the working tree: its exact bytes up to maxRejectedReviewBytes
// (complete) or that prefix (truncated, with the original size); absent
// without the entry; unsafe for a link, tree or special mode; unavailable
// after a failed pull or object read. Bytes exist only when complete or
// truncated.
func captureRejectedReview(dir, commit string, pullErr error) (ReviewCapture, []byte) {
	unavailable := func(err error) (ReviewCapture, []byte) {
		e, _ := boundText(err.Error(), maxCaptureErrorBytes)
		return ReviewCapture{State: captureUnavailable, CaptureError: &e}, nil
	}
	if pullErr != nil {
		return unavailable(fmt.Errorf("the result pull failed: %w", pullErr))
	}
	repo, err := git.PlainOpen(dir)
	if err != nil {
		return unavailable(err)
	}
	return captureRootReview(repo, commit)
}

// captureRootReview is captureRejectedReview's read of commit's root
// review.md in repo (the inspection repository live, the evidence
// repository offline).
func captureRootReview(repo *git.Repository, commit string) (ReviewCapture, []byte) {
	unavailable := func(err error) (ReviewCapture, []byte) {
		e, _ := boundText(err.Error(), maxCaptureErrorBytes)
		return ReviewCapture{State: captureUnavailable, CaptureError: &e}, nil
	}
	if !hex40RE.MatchString(commit) {
		return unavailable(errors.New("no result commit"))
	}
	c, err := repo.CommitObject(plumbing.NewHash(commit))
	if err != nil {
		return unavailable(err)
	}
	tree, err := c.Tree()
	if err != nil {
		return unavailable(err)
	}
	var entry *object.TreeEntry
	for i := range tree.Entries {
		if tree.Entries[i].Name == "review.md" {
			entry = &tree.Entries[i]
		}
	}
	if entry == nil {
		return ReviewCapture{State: captureAbsent}, nil
	}
	mode := entry.Mode.String()
	if entry.Mode != filemode.Regular && entry.Mode != filemode.Executable {
		return ReviewCapture{State: captureUnsafe, Mode: &mode}, nil
	}
	blob, err := repo.BlobObject(entry.Hash)
	if err != nil {
		return unavailable(err)
	}
	rd, err := blob.Reader()
	if err != nil {
		return unavailable(err)
	}
	defer rd.Close()
	data, err := io.ReadAll(io.LimitReader(rd, maxRejectedReviewBytes))
	size := blob.Size
	if err != nil || int64(len(data)) != min(size, maxRejectedReviewBytes) {
		return unavailable(fmt.Errorf("the review.md blob could not be read: %v", err))
	}
	if data == nil {
		data = []byte{}
	}
	state := captureComplete
	if size > maxRejectedReviewBytes {
		state = captureTruncated
	}
	n, sum := len(data), sha256Hex(data)
	return ReviewCapture{State: state, Mode: &mode, Size: &size, CapturedBytes: &n, CapturedSHA256: &sum}, data
}

// rejectionInput is what a retained rejection is judged against, in
// either namespace (a full-flow tasks/NN or the qualification's
// probe/reviewer/01): the bundle, the task directory and its ordinal, the
// run, the verified stop code ("" none) and, for a code review, the
// retained review inputs (nil when they are not retained).
type rejectionInput struct {
	b        *DevBundle
	dir      string
	ordinal  int
	runID    string
	stopCode string
	review   *reviewRecheck
}

// reviewRecheck is a code review's retained inputs for rechecking its
// review.md: the helper recipe, candidate and initial source, the
// candidate tree and the seed's main.go.
type reviewRecheck struct {
	helper             reviewHelperSpec
	candidate, initial string
	candTree           string
	cand               map[string]TreeFile
	seedMain           []byte
}

// rejectionVerdict is one retained rejection's judgment: present when
// either diagnostic exists; bad the contradictions of the record, its
// linkage, bounds and capture; unobserved the reason its rejection could
// not be reconstructed from the retained inputs ("" when it was);
// recheck the reconstruction's contradiction ("" none); rechecked when the
// review checks ran on the retained inputs, with the helper receipt's
// log lines they found.
type rejectionVerdict struct {
	present    bool
	task       string
	state      string
	code       string
	reason     string
	bad        []string
	unobserved string
	recheck    string
	note       string
	rechecked  bool
	candidate  string
	receipt    []SourceRef
}

// judgeRejection is the one rejection validator the full-flow checker
// and the qualification check share (design 12a-dev-flow-verify r0.11).
func judgeRejection(in rejectionInput) rejectionVerdict {
	var v rejectionVerdict
	b := in.b
	_, has := b.Files[in.dir+"/"+publicationRejectionFile]
	bin, hasBin := b.Files[in.dir+"/"+rejectedReviewFile]
	if v.present = has || hasBin; !v.present {
		return v
	}
	bad := func(code string) {
		if !slices.Contains(v.bad, code) {
			v.bad = append(v.bad, code)
		}
	}
	rec := doc[devEnvelope[DevPublicationRejection]](b, in.dir+"/"+publicationRejectionFile)
	if rec == nil {
		bad("rejection_record_invalid")
		v.unobserved = "the rejection record is missing or invalid"
		return v
	}
	r := rec.Data
	v.task, v.state, v.code, v.reason = r.Task, r.Review.State, r.Code, r.Reason
	plan := doc[devEnvelope[TaskPlan]](b, in.dir+"/plan.json")
	obs := doc[DevObservation](b, in.dir+"/observation.json")
	var view *contract.TaskView
	if t := doc[contract.TaskShowResponse](b, in.dir+"/task.json"); t != nil {
		view = &t.Task
	}
	// Linkage: every identity field to the task's own authoritative
	// records and the attempt's verified stop.
	var published *string
	if view != nil && view.Result != nil && view.Result.Workspace != nil {
		published = view.Result.Workspace.Commit
	}
	switch {
	case rec.RunID != in.runID || plan == nil || obs == nil || view == nil:
		bad("rejection_linkage_mismatch")
	case r.Task != obs.Task || r.Task != view.TaskID || r.Ordinal != in.ordinal || r.Ordinal != plan.Data.Ordinal || r.Role != plan.Data.Role ||
		r.ExpectedBase != plan.Data.ExpectedBase:
		bad("rejection_linkage_mismatch")
	case (published == nil) != (r.ResultCommit == nil) || published != nil && *published != *r.ResultCommit:
		bad("rejection_linkage_mismatch")
	case in.stopCode == "" || in.stopCode != r.Code:
		bad("rejection_linkage_mismatch")
	}
	if r.Reason == "" || len(r.Reason) > maxRejectionReasonBytes || !utf8.ValidString(r.Reason) || r.ReasonTruncated && len(r.Reason) < maxRejectionReasonBytes-utf8.UTFMax {
		bad("rejection_record_invalid")
	}
	if code := checkReviewCapture(r.Review, bin, hasBin); code != "" {
		bad(code)
	}
	// When the evidence repository holds the rejected result commit, the
	// retained bytes, mode and size are its own root blob's.
	if b.Repo != nil && r.ResultCommit != nil {
		if _, err := b.Repo.repo.CommitObject(plumbing.NewHash(*r.ResultCommit)); err == nil {
			want, data := captureRootReview(b.Repo.repo, *r.ResultCommit)
			if !bytes.Equal(mustJSON(want), mustJSON(r.Review)) || !bytes.Equal(data, bin) {
				bad("rejection_capture_mismatch")
			}
		}
	}
	// Reconstruction: a code review's complete retained review.md is
	// rechecked with the retained receipt, candidate and report inputs.
	switch {
	case len(v.bad) > 0:
		v.unobserved = "the record is contradicted"
	case r.Role != "code-reviewer":
		v.unobserved = "only a code review's rejection is reconstructed from its retained review.md"
	case r.Review.State != captureComplete:
		v.unobserved = "the review.md capture is " + r.Review.State
	case in.review == nil || view.Result == nil || view.Result.FinalMessage == nil:
		v.unobserved = "the review's inputs are not retained"
	default:
		v.candidate = in.review.candidate
		v.recheck, v.unobserved, v.note, v.receipt, v.rechecked = recheckReview(in, view, r.Code, string(bin))
	}
	return v
}

// recheckReview reruns the review report and receipt checks on a complete
// retained review.md: the review's execution evidence derived with that
// report, then its blocker and report fields. A refusal they reproduce
// must be the recorded one; when they pass, the rejection came from what
// was not retained (unobserved). It also returns the helper receipt's log
// lines the derivation found and whether the checks ran.
func recheckReview(in rejectionInput, view *contract.TaskView, recorded, report string) (bad, unobserved, note string, receipt []SourceRef, ran bool) {
	marker, err := parseTerminalMarker(*view.Result.FinalMessage)
	if err != nil {
		return "", "the final answer has no marker", "", nil, false
	}
	rv := in.review
	ev := DeriveDevReviewEvidence(bundleReviewInput(in.b, in.dir, in.ordinal, view, rv.helper, rv.candidate, rv.initial, rv.candTree, rv.cand,
		rv.seedMain, marker, report))
	if ev.Started != nil && ev.Completed != nil {
		receipt = []SourceRef{{File: ev.Started.File, SHA256: ev.Started.SHA256, FirstLine: ev.Started.FirstLine, LastLine: ev.Completed.LastLine}}
	}
	got := ""
	if code, stops := ev.blocks(); stops {
		got = code
	} else if _, err := parseReviewBlocker(*view.Result.FinalMessage, report, marker); err != nil {
		got = "publication_rejected"
	} else if err := ReviewReportOK(report, marker, rv.candidate, view.TaskID, rv.cand, ev.runDigest()); err != nil {
		got = "publication_rejected"
	}
	switch {
	case got == "":
		return "", "the retained review.md passes its review checks; the rest of the rejected result is not retained", "", receipt, true
	case got != recorded:
		return "rejection_unreproduced", "", "recomputed " + got + " from the retained review.md", receipt, true
	}
	return "", "", "reproduced " + got + " from the retained review.md", receipt, true
}

// checkReviewCapture checks a capture descriptor against the retained
// bytes: bytes exactly for complete and truncated captures (a regular
// file's mode, within the bound, with their count and digest), a
// nonregular mode for an unsafe one, none otherwise.
func checkReviewCapture(rc ReviewCapture, bin []byte, hasBin bool) string {
	withBytes := rc.State == captureComplete || rc.State == captureTruncated
	regular := rc.Mode != nil && (*rc.Mode == filemode.Regular.String() || *rc.Mode == filemode.Executable.String())
	switch {
	case !slices.Contains([]string{captureComplete, captureTruncated, captureAbsent, captureUnsafe, captureUnavailable}, rc.State):
		return "rejection_capture_invalid"
	case withBytes != hasBin || withBytes != (rc.CapturedBytes != nil) || withBytes != (rc.CapturedSHA256 != nil):
		return "rejection_capture_invalid"
	case (rc.State == captureUnavailable) != (rc.CaptureError != nil) || rc.CaptureError != nil && len(*rc.CaptureError) > maxCaptureErrorBytes:
		return "rejection_capture_invalid"
	case (rc.State == captureAbsent || rc.State == captureUnavailable) && (rc.Mode != nil || rc.Size != nil):
		return "rejection_capture_invalid"
	case withBytes && !regular, rc.State == captureUnsafe && (rc.Mode == nil || regular || rc.Size != nil):
		return "rejection_capture_invalid"
	}
	if !withBytes {
		return ""
	}
	switch {
	case rc.Size == nil || *rc.CapturedBytes != len(bin) || *rc.CapturedSHA256 != sha256Hex(bin) || len(bin) > maxRejectedReviewBytes:
		return "rejection_capture_invalid"
	case rc.State == captureComplete && int64(len(bin)) != *rc.Size:
		return "rejection_capture_invalid"
	case rc.State == captureTruncated && (len(bin) != maxRejectedReviewBytes || *rc.Size <= maxRejectedReviewBytes):
		return "rejection_capture_invalid"
	}
	return ""
}

// rejectionInput is full-flow task n's rejection input: its namespace,
// the run, the verified stop and, when the evidence repository holds its
// candidate, the review's retained inputs.
func (c *devChecker) rejectionInput(n int, stop string) rejectionInput {
	dir := taskDir(n)
	in := rejectionInput{b: c.b, dir: dir, ordinal: n, runID: c.runID, stopCode: stop}
	if p := doc[devEnvelope[TaskPlan]](c.b, dir+"/plan.json"); p != nil {
		if cand, ok := c.trees[p.Data.ExpectedBase]; ok {
			in.review = &reviewRecheck{helper: c.m.Helper, candidate: p.Data.ExpectedBase, initial: c.m.Workspace.SeedCommit,
				candTree: c.treeHash[p.Data.ExpectedBase], cand: cand, seedMain: fileContent(c.trees[c.m.Workspace.SeedCommit], "main.go")}
		}
	}
	return in
}

// checkRejections judges every retained full-flow rejection record
// offline (judgeRejection) in two rows: the record, its linkage, bounds
// and capture, verified or contradicted; and its reconstruction from the
// retained inputs, correct, contradicted or unobserved. The records are
// diagnostics only: they never admit anything, and a stopped attempt
// stays failed whatever they say.
func (c *devChecker) checkRejections() {
	stop := ""
	if c.stop != nil {
		stop = c.stop.code
	}
	for _, n := range c.b.ordinalsOf("tasks") {
		dir := taskDir(n)
		v := judgeRejection(c.rejectionInput(n, stop))
		if !v.present {
			continue
		}
		rb := c.newRow(fmt.Sprintf("task/%02d/publication-rejection", n), "publications",
			fmt.Sprintf("Task %d's retained publication rejection is bound to its task, plan, result commit and the attempt's stop, within its bounds, and its capture of the rejected review.md is consistent and hashed (diagnostic only, never an admission)", n),
			groupEvents)
		for _, code := range v.bad {
			rb.bad(code)
		}
		rb.ref(dir+"/"+publicationRejectionFile, nil, v.task, "")
		rb.ref(dir+"/"+rejectedReviewFile, nil, v.task, "")
		rb.see("%s: %s (review.md %s)", cmpOr(v.code, "unknown"), auditText(v.reason, 300), cmpOr(v.state, "unknown"))
		rb.done()
		xb := c.newRow(fmt.Sprintf("task/%02d/publication-rejection-recheck", n), "publications",
			fmt.Sprintf("Task %d's rejection recomputes from its retained inputs: the complete rejected review.md rechecked with the retained receipt, candidate and report checks", n),
			groupEvents)
		// It cites what it used: the retained record and review bytes
		// and, when the review checks ran, the review's log and metadata,
		// its task, the helper recipe, the candidate's tree and the
		// helper receipt's lines.
		xb.ref(dir+"/"+publicationRejectionFile, nil, v.task, "")
		xb.ref(dir+"/"+rejectedReviewFile, nil, v.task, "")
		if v.rechecked {
			for _, f := range []string{dir + "/task.json", dir + "/logs.txt", dir + "/logs.json", devManifestFile} {
				xb.ref(f, nil, v.task, "")
			}
			xb.ref(devTreesFile, nil, v.task, v.candidate)
			xb.refs(v.receipt, v.task)
		}
		switch {
		case v.recheck != "":
			xb.bad(v.recheck)
		case v.unobserved != "":
			xb.miss("rejection_unobserved", true)
			xb.see("%s", v.unobserved)
		}
		if v.note != "" {
			xb.see("%s", v.note)
		}
		xb.done()
	}
}
