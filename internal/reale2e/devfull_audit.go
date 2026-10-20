package reale2e

import (
	"fmt"
	"slices"
	"sort"
	"strings"
)

// The full-flow audit (design 12a-dev-flow-verify, Replay and audit):
// dynamic mechanical rows with stable IDs for the setup, each preflight
// and phase, every actual task (dispatch, wait, publication, marker and,
// for a code review, its reviewer execution), transition, decision and
// receipt, the validation, cases, cleanup, environment and cost; ordered
// setup and preflights first, then by the actual event order (ties by row
// ID), then the tail. The next blocked step of a stopped attempt is
// unobserved with its cause; nothing is invented for unused loops. The
// separate judgment layer holds one row per draft, audit, implementation,
// code review and triage, always unobserved here: only the owner's
// external audit-review.md judges them, and it can never upgrade a
// mechanical FAIL.

// DevAuditRow is one audit row.
type DevAuditRow struct {
	ID           string     `json:"id"`
	Layer        string     `json:"layer"`
	Criterion    string     `json:"criterion"`
	Ordinal      int        `json:"ordinal"`
	Task         string     `json:"task"`
	Role         string     `json:"role"`
	Mode         string     `json:"mode"`
	Round        int        `json:"round"`
	InputCommit  string     `json:"input_commit"`
	ResultCommit string     `json:"result_commit"`
	Sources      []AuditRef `json:"sources"`
	Requirement  string     `json:"requirement"`
	Observation  string     `json:"observation"`
	Verdict      string     `json:"verdict"`
	Codes        []string   `json:"codes"`
	Correction   string     `json:"correction"`
	group, seq   int
}

// DevAudit is audit.json.
type DevAudit struct {
	Schema       string           `json:"schema"`
	RunID        string           `json:"run_id"`
	ReportSHA256 string           `json:"report_sha256"`
	Scope        string           `json:"scope"`
	Mechanical   string           `json:"mechanical"`
	Judgments    string           `json:"judgments"`
	Sources      []InventoryEntry `json:"sources"`
	Rows         []DevAuditRow    `json:"rows"`
	JudgmentRows []DevAuditRow    `json:"judgment_rows"`
}

// Row groups.
const (
	groupSetup = iota
	groupEvents
	groupTail
)

// rowBuilder accumulates one mechanical row.
type rowBuilder struct {
	c         *devChecker
	r         DevAuditRow
	crit      string
	obs       []string
	missing   []string
	unreached bool
}

// newRow starts row id under criterion crit.
func (c *devChecker) newRow(id, crit, requirement string, group int) *rowBuilder {
	return &rowBuilder{c: c, crit: crit, r: DevAuditRow{ID: id, Layer: LayerMechanical, Criterion: crit, Requirement: requirement, Sources: []AuditRef{},
		Codes: []string{}, group: group}}
}

// task sets the row's task identity.
func (rb *rowBuilder) task(ordinal int, task, role, mode string, round int, input, result string) *rowBuilder {
	rb.r.Ordinal, rb.r.Task, rb.r.Role, rb.r.Mode, rb.r.Round, rb.r.InputCommit, rb.r.ResultCommit = ordinal, task, role, mode, round, input, result
	return rb
}

// at orders the row by event sequence seq (the earliest wins).
func (rb *rowBuilder) at(seq int) *rowBuilder {
	if seq > 0 && (rb.r.seq == 0 || seq < rb.r.seq) {
		rb.r.seq = seq
	}
	return rb
}

// bad records a contradiction.
func (rb *rowBuilder) bad(code string) {
	rb.c.fail(rb.crit, code)
	if !slices.Contains(rb.r.Codes, code) {
		rb.r.Codes = append(rb.r.Codes, code)
	}
}

// miss records missing proof: a failure, or not reached when the attempt
// verifiably stopped before its step.
func (rb *rowBuilder) miss(code string, reached bool) {
	if rb.c.absent(rb.crit, code, reached) {
		if !slices.Contains(rb.missing, code) {
			rb.missing = append(rb.missing, code)
		}
		return
	}
	rb.unreached = true
}

// see records an observation.
func (rb *rowBuilder) see(format string, a ...any) {
	rb.obs = append(rb.obs, fmt.Sprintf(format, a...))
}

// ref cites a bundle file with optional lines, task and commit.
func (rb *rowBuilder) ref(file string, lines []int, task, commit string) {
	data, ok := rb.c.b.Files[file]
	if !ok {
		return
	}
	if lines == nil {
		lines = []int{}
	}
	rb.c.source(rb.crit, file)
	rb.r.Sources = append(rb.r.Sources, AuditRef{File: file, SHA256: sha256Hex(data), Lines: lines, Task: task, Commit: commit})
}

// refs cites source ranges.
func (rb *rowBuilder) refs(srcs []SourceRef, task string) {
	for _, s := range srcs {
		if s.File == "" {
			continue
		}
		rb.c.source(rb.crit, s.File)
		rb.r.Sources = append(rb.r.Sources, AuditRef{File: s.File, SHA256: s.SHA256, Lines: []int{s.FirstLine, s.LastLine}, Task: task})
	}
}

// done renders and appends the row.
func (rb *rowBuilder) done() {
	r := rb.r
	sort.Strings(r.Codes)
	obs := strings.Join(rb.obs, "; ")
	if len(rb.missing) > 0 {
		obs = strings.TrimPrefix(obs+"; missing: "+strings.Join(rb.missing, ", "), "; ")
	}
	switch {
	case len(r.Codes) > 0:
		r.Verdict = VerdictIncorrect
		r.Correction = "The captured evidence contradicts this requirement (" + strings.Join(r.Codes, ", ") + "); the report records a mechanical FAIL. Nothing was repaired; a new attempt creates new evidence."
	case len(rb.missing) > 0:
		r.Verdict = VerdictUnobserved
		r.Codes = append(r.Codes, rb.missing...)
		sort.Strings(r.Codes)
		r.Correction = "Proof is missing (" + strings.Join(rb.missing, ", ") + "); unobserved mechanical proof is a FAIL and nothing was inferred."
	case rb.unreached && rb.c.stop != nil:
		r.Verdict = VerdictUnobserved
		obs = strings.TrimPrefix(obs+"; not reached: "+rb.c.stop.code, "; ")
		r.Correction = fmt.Sprintf("not reached: %s. The attempt stopped (event seq %d) before this step; only that cause is reported and nothing was inferred.",
			rb.c.stop.code, rb.c.stop.seq)
	default:
		r.Verdict = VerdictCorrect
	}
	if obs == "" {
		obs = "nothing observed"
	}
	if r.Codes == nil {
		r.Codes = []string{}
	}
	r.Observation = obs
	rb.c.rows = append(rb.c.rows, r)
}

// setupRow is a whole-criterion row of codes already recorded.
func (c *devChecker) setupRow(crit, requirement string, codes []string, files []string) {
	rb := c.newRow("setup/"+crit, crit, requirement, groupSetup)
	for _, code := range codes {
		rb.r.Codes = append(rb.r.Codes, code)
	}
	rb.r.Codes = slices.Compact(slices.Sorted(slices.Values(rb.r.Codes)))
	for _, f := range files {
		rb.ref(f, nil, "", "")
	}
	if len(codes) == 0 {
		rb.see("criterion %s holds on the captured evidence", crit)
	}
	rb.done()
}

// judgmentRow appends one judgment-layer row (always unobserved here).
func (c *devChecker) judgmentRow(id string, ordinal int, task, role, mode string, round int, commit, requirement string, files ...[2]string) {
	r := DevAuditRow{ID: id, Layer: LayerJudgment, Criterion: "judgment", Ordinal: ordinal, Task: task, Role: role, Mode: mode, Round: round,
		ResultCommit: commit, Requirement: requirement, Observation: "pending the owner's external review in audit-review.md",
		Verdict: VerdictUnobserved, Codes: []string{}, Correction: PendingReview, Sources: []AuditRef{}}
	for _, f := range files {
		if tf, ok := c.trees[commit][f[0]]; ok {
			r.Sources = append(r.Sources, AuditRef{File: f[0], SHA256: sha256Hex(tf.Content), Lines: []int{}, Task: task, Commit: commit})
		} else if data, ok := c.b.Files[f[1]]; ok && f[1] != "" {
			r.Sources = append(r.Sources, AuditRef{File: f[1], SHA256: sha256Hex(data), Lines: []int{}, Task: task})
		}
	}
	c.judge = append(c.judge, r)
}

// judgmentRows derives the judgment layer from the replayed tasks.
func (c *devChecker) judgmentRows() {
	if c.rp == nil {
		return
	}
	for _, t := range c.rp.state.Tasks {
		dir := taskDir(t.Ordinal)
		switch {
		case t.Role == "designer" && t.Marker == markerDesignDraft:
			c.judgmentRow(fmt.Sprintf("judgment/task/%02d/design", t.Ordinal), t.Ordinal, t.TaskID, t.Role, t.Mode, t.Round, t.Commit,
				"The draft is a complete seven-section design with full FP traceability for the request", [2]string{"design.md"}, [2]string{"revision-history.md"})
		case t.Role == "design-reviewer":
			c.judgmentRow(fmt.Sprintf("judgment/task/%02d/audit", t.Ordinal), t.Ordinal, t.TaskID, t.Role, t.Mode, t.Round, t.Commit,
				"The design audit's quality and, after the first round, its delta convergence and finding tags are defensible", [2]string{"design-review.md"})
		case t.Role == "coder" && t.Disposition == dispositionReady:
			c.judgmentRow(fmt.Sprintf("judgment/task/%02d/implementation", t.Ordinal), t.Ordinal, t.TaskID, t.Role, t.Mode, t.Round, t.Commit,
				"The implementation is complete beyond the sampled cases and its tests' assertions exercise every required behavior",
				[2]string{"main.go"}, [2]string{"main_test.go"})
		case t.Role == "code-reviewer":
			c.judgmentRow(fmt.Sprintf("judgment/task/%02d/review", t.Ordinal), t.Ordinal, t.TaskID, t.Role, t.Mode, t.Round, t.Commit,
				"The code review analyzes this candidate against every FP, its finding dispositions are justified and its verdict is defensible",
				[2]string{"review.md", dir + "/final.txt"})
		case t.Role == "triage":
			c.judgmentRow(fmt.Sprintf("judgment/task/%02d/triage", t.Ordinal), t.Ordinal, t.TaskID, t.Role, t.Mode, t.Round, t.Commit,
				"The triage's causal evidence supports its root cause and stated confidence", [2]string{"rca.md"}, [2]string{"fix.md"})
		}
	}
}

// tailRank orders the tail: the validation and its cases, cleanup, the
// environment, the deadlines, then the cost.
func tailRank(id string) int {
	ranks := []struct {
		prefix string
		rank   int
	}{{"validation/", 0}, {"case/", 0}, {"cleanup/", 1}, {"environment/", 2}, {"deadlines/", 3}, {"cost/", 4}}
	for _, r := range ranks {
		if strings.HasPrefix(id, r.prefix) {
			return r.rank
		}
	}
	return len(ranks)
}

// buildAudit orders the rows and binds the report.
func (c *devChecker) buildAudit(r DevReport) DevAudit {
	data, _ := encodeJSON(r)
	rows := slices.Clone(c.rows)
	sort.SliceStable(rows, func(i, j int) bool {
		a, b := rows[i], rows[j]
		if a.group != b.group {
			return a.group < b.group
		}
		if a.group == groupEvents && a.seq != b.seq {
			return a.seq < b.seq
		}
		if a.group == groupTail && tailRank(a.ID) != tailRank(b.ID) {
			return tailRank(a.ID) < tailRank(b.ID)
		}
		return a.ID < b.ID
	})
	judge := slices.Clone(c.judge)
	sort.SliceStable(judge, func(i, j int) bool { return judge[i].ID < judge[j].ID })
	if rows == nil {
		rows = []DevAuditRow{}
	}
	if judge == nil {
		judge = []DevAuditRow{}
	}
	return DevAudit{Schema: devKind("audit"), RunID: r.RunID, ReportSHA256: sha256Hex(data), Scope: devScope, Mechanical: r.Result,
		Judgments: PendingReview, Sources: r.Artifacts, Rows: rows, JudgmentRows: judge}
}

// renderDevAudit is audit.md.
func renderDevAudit(a DevAudit) string {
	var b strings.Builder
	fmt.Fprintf(&b, "# Callsheet full development flow audit\n\n")
	fmt.Fprintf(&b, "- run: %s\n- report sha256: %s\n- scope: %s\n- mechanical result: %s\n- judgments: %s\n- source files: %d\n\n",
		a.RunID, a.ReportSHA256, a.Scope, a.Mechanical, a.Judgments, len(a.Sources))
	b.WriteString("Judgment rows stay unobserved until the owner's external dev-flow coordinator reviews them in a separate audit-review.md (run ID, report and audit hashes, reviewer identity and date, every judgment row with its verdict and reasons, artifact commit, hash and line references, and any disputed mechanical row). That review never modifies the evidence and never upgrades a mechanical FAIL.\n")
	write := func(i int, r DevAuditRow) {
		fmt.Fprintf(&b, "\n## %d. %s (%s; %s)\n\n", i, r.ID, r.Layer, r.Criterion)
		if r.Task != "" || r.Ordinal > 0 {
			fmt.Fprintf(&b, "- Task: ordinal %d %s %s/%s round %d, input %s, result %s\n", r.Ordinal, r.Task, r.Role, r.Mode, r.Round, r.InputCommit, r.ResultCommit)
		}
		fmt.Fprintf(&b, "- Requirement: %s\n- Observed: %s\n- Verdict: %s\n", r.Requirement, r.Observation, r.Verdict)
		if r.Correction != "" {
			fmt.Fprintf(&b, "- Correction: %s\n", r.Correction)
		}
		for _, e := range r.Sources {
			fmt.Fprintf(&b, "- Evidence: %s sha256 %s", e.File, e.SHA256)
			if len(e.Lines) == 2 {
				fmt.Fprintf(&b, " lines %d-%d", e.Lines[0], e.Lines[1])
			}
			if e.Task != "" {
				fmt.Fprintf(&b, " task %s", e.Task)
			}
			if e.Commit != "" {
				fmt.Fprintf(&b, " commit %s", e.Commit)
			}
			b.WriteString("\n")
		}
	}
	for i, r := range a.Rows {
		write(i+1, r)
	}
	b.WriteString("\n# Judgment layer\n")
	for i, r := range a.JudgmentRows {
		write(len(a.Rows)+i+1, r)
	}
	return b.String()
}
