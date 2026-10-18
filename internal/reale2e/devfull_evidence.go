package reale2e

import (
	"errors"
	"fmt"
	"io/fs"
	"path/filepath"

	git "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/storer"
)

// Development execution evidence (design 12a-dev-flow-harness, FP-7).
// This is not a Report and it is not a PASS. The legacy checker rejects it.

const (
	DevExecutionSchema = "callsheet-dev-full-execution/v1"
	DevExecutionFile   = "dev-execution.json"
	DevRunSchema       = "callsheet-dev-full-run/v1"

	outcomeUnverified = "executed-unverified"
	outcomeStopped    = "stopped"
	outcomeFailed     = "failed"

	devUnverifiedLine = "EXECUTED — UNVERIFIED: slice 2d.3 verification required"
)

// DevBounds is the development full flow's cost and time disclosure.
const DevBounds = `Development full flow cost and time bounds (policy, not measurements):
  - five paid authentication preflight tasks (one per role pair), each at most 2m;
  - four paid workflow tasks on the happy path; the two bounded revision loops allow up to
    twelve ordinary-pair calls and at most two recovery calls (at most fourteen workflow
    tasks), each at most 10m from admission through publication: at most 19 paid worker
    calls including the five preflights;
  - the headless Claude Code coordinator adds vendor-dependent turns and tool calls in up to
    four phases; there is no extra paid coordinator preflight;
  - each owner window at most 20m; the whole attempt at most 90m from preflight start (the
    first owner-environment fingerprint included), then at most 2m of cleanup; 60s of final
    validation is reserved for slice 2d.3;
  - no automatic model retries; no token-price ceiling is claimed. The task-time sum can exceed
    90m; the attempt deadline stops first.
`

// DevExecution is the development attempt's manifest. It never carries a
// PASS result.
type DevExecution struct {
	Schema          string `json:"schema"`
	RunID           string `json:"run_id"`
	Outcome         string `json:"outcome"`
	StopCode        string `json:"stop_code,omitempty"`
	StopReason      string `json:"stop_reason,omitempty"`
	FlowLabel       string `json:"flow_label"`
	FlowSHA256      string `json:"flow_sha256"`
	SelectionSHA256 string `json:"selection_sha256"`
	CandidateTask   string `json:"final_candidate_task,omitempty"`
	FinalCandidate  string `json:"final_candidate,omitempty"`
	ReviewTask      string `json:"final_review_task,omitempty"`
	FinalReview     string `json:"final_review,omitempty"`
	Preflights      int    `json:"preflights"`
	Tasks           int    `json:"tasks"`
	Rejected        int    `json:"rejected_admissions"`
	Transitions     int    `json:"transitions"`
	Decisions       int    `json:"decisions"`
	Receipts        int    `json:"receipts"`
	Phases          int    `json:"phases"`
	IgnoredLines    int    `json:"ignored_owner_lines"`
	Cleanup         string `json:"cleanup"`
	ProtectedChange bool   `json:"protected_change"`
	EvidenceError   string `json:"evidence_error,omitempty"`
}

// DevRunFile is the development runtime's run file. Legacy helpers reject
// it, and development helpers reject a legacy run file.
type DevRunFile struct {
	Schema    string `json:"schema"`
	RunID     string `json:"run_id"`
	Callsheet string `json:"callsheet"`
	Plane     string `json:"plane"`
	CA        string `json:"ca"`
}

// devCommit is one admitted publication for the evidence repository.
type devCommit struct {
	Ordinal int    `json:"ordinal"`
	Task    string `json:"task"`
	Commit  string `json:"commit"`
	Parent  string `json:"parent"`
	Tree    string `json:"tree"`
}

// DevTreeManifest is workspace/tree-manifest.json.
type DevTreeManifest struct {
	SeedCommit string      `json:"seed_commit"`
	SeedTree   string      `json:"seed_tree"`
	Tasks      []devCommit `json:"tasks"`
}

// errDevRepoLimit is an evidence repository over its stored bound.
var errDevRepoLimit = errors.New("the development evidence repository exceeds its 4 MiB stored bound")

// writeDevEvidenceRepo creates dir as a bare repository holding the seed
// and every admitted result with its full ancestry under refs/heads/seed
// and refs/heads/task-NN, HEAD at the seed, and checks its stored size.
func writeDevEvidenceRepo(dir string, src storer.EncodedObjectStorer, seed plumbing.Hash, tasks []devCommit) error {
	r, err := git.PlainInit(dir, true)
	if err != nil {
		return fmt.Errorf("evidence repository: %w", err)
	}
	roots := []plumbing.Hash{seed}
	for _, t := range tasks {
		roots = append(roots, plumbing.NewHash(t.Commit))
	}
	if err := copyObjects(src, r.Storer, roots); err != nil {
		return fmt.Errorf("evidence repository: %w", err)
	}
	refs := map[string]plumbing.Hash{"refs/heads/seed": seed}
	for _, t := range tasks {
		refs[fmt.Sprintf("refs/heads/task-%02d", t.Ordinal)] = plumbing.NewHash(t.Commit)
	}
	for name, h := range refs {
		if err := r.Storer.SetReference(plumbing.NewHashReference(plumbing.ReferenceName(name), h)); err != nil {
			return fmt.Errorf("evidence repository: %w", err)
		}
	}
	if err := r.Storer.SetReference(plumbing.NewSymbolicReference(plumbing.HEAD, "refs/heads/seed")); err != nil {
		return fmt.Errorf("evidence repository: %w", err)
	}
	var total int64
	err = filepath.WalkDir(dir, func(p string, e fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if e.Type().IsRegular() {
			st, err := e.Info()
			if err != nil {
				return err
			}
			total += st.Size()
		}
		return nil
	})
	if err != nil {
		return fmt.Errorf("evidence repository: %w", err)
	}
	if total > maxRepoFileBytes {
		return errDevRepoLimit
	}
	return nil
}
