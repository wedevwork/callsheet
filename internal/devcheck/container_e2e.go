package devcheck

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	git "github.com/go-git/go-git/v5"
)

// Container acceptance (design m3-m4-container-e2e): the Linux-only
// container-e2e operation builds static callsheet, fake-adapter and the
// tagged acceptance test binary on the host, wraps them in a FROM scratch
// image with the fixture bundle, runs each iteration in a fresh isolated
// container (no network, read-only root, an executable tmpfs, no
// capabilities), removes container and image, and validates the
// container's evidence records and verbose test events against the static
// case manifest and per-task catalog (container_expected.go). It is a
// separate driver operation after the Linux test plan (never a Step of
// TestSteps) and the standalone container-e2e stage.

// Container limits and fixed names.
const (
	// ContainerMaxCount is the largest --count.
	ContainerMaxCount = 20
	// containerIterationDeadline bounds one iteration (the first also
	// charges the shared setup and build).
	containerIterationDeadline = 10 * time.Minute
	// containerCleanupDeadline bounds explicit cleanup after a failure or
	// an expired deadline.
	containerCleanupDeadline = 2 * time.Minute
	// ContainerEvidencePrefix starts every evidence line.
	ContainerEvidencePrefix = "CALLSHEET_E2E_V1 "
	// ContainerTestTag is the acceptance test package's build tag.
	ContainerTestTag = "containeracceptance"
	// containerTestPackage is the tagged acceptance test package.
	containerTestPackage = "./tests/container"
	// containerMaxRecord bounds one evidence line.
	containerMaxRecord = 1 << 20
	// containerTmpfs is the run's only writable mount: 1 GiB, mode 1777,
	// executable.
	containerTmpfs = "/tmp:rw,exec,nosuid,nodev,size=1073741824,mode=1777"
	// containerUser is the numeric non-root runtime user.
	containerUser = "65532:65532"
)

// Container metadata environment variables (literal docker --env entries).
const (
	ContainerEnvRunID    = "CALLSHEET_E2E_RUN_ID"
	ContainerEnvIter     = "CALLSHEET_E2E_ITERATION"
	ContainerEnvTotal    = "CALLSHEET_E2E_TOTAL"
	ContainerEnvRevision = "CALLSHEET_E2E_REVISION"
	ContainerEnvArch     = "CALLSHEET_E2E_ARCH"
	ContainerEnvHashes   = "CALLSHEET_E2E_HASHES"
)

// Binary hash keys.
const (
	HashCallsheet      = "callsheet"
	HashFakeAdapter    = "fake_adapter"
	HashAcceptanceTest = "acceptance_test"
)

// containerHashKeys are the exact binary hash keys.
var containerHashKeys = []string{HashAcceptanceTest, HashCallsheet, HashFakeAdapter}

// Errors whose text is the exact stage diagnostic.
var (
	errDockerMissing     = errors.New("docker executable not found")
	errDockerUnavailable = errors.New("docker daemon unavailable")
)

// ContainerMetadata is one iteration's identity: the invocation run ID,
// source revision and Linux architecture, its one-based iteration of
// Total, and the host-built binary hashes.
type ContainerMetadata struct {
	RunID, SourceRevision, Architecture string
	Iteration, Total                    int
	BinaryHashes                        map[string]string
}

// ContainerExpectation is what one iteration's evidence must show: its
// metadata, the required case IDs and each parent's required subcases.
type ContainerExpectation struct {
	Metadata ContainerMetadata
	CaseIDs  []string
	Subcases map[string][]string
}

// containerExpectation is the fixed manifest with meta.
func containerExpectation(meta ContainerMetadata) ContainerExpectation {
	return ContainerExpectation{Metadata: meta, CaseIDs: ContainerCaseIDs(), Subcases: ContainerSubcases()}
}

var (
	runIDRE    = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,63}$`)
	hex40RE    = regexp.MustCompile(`^[0-9a-f]{40}$`)
	hex64RE    = regexp.MustCompile(`^[0-9a-f]{64}$`)
	hex32RE    = regexp.MustCompile(`^[0-9a-f]{32}$`)
	taskID32RE = regexp.MustCompile(`^t_[0-9a-f]{32}$`)
	nodeID32RE = regexp.MustCompile(`^n_[0-9a-f]{32}$`)
	dockerName = regexp.MustCompile(`^[a-z0-9][a-z0-9_.-]{0,127}$`)
	imageTagRE = regexp.MustCompile(`^[a-z0-9][a-z0-9_.-]{0,127}:[a-z0-9][a-z0-9_.-]{0,127}$`)
)

// validateMetadata checks meta's grammar and the repetition bounds.
func validateMetadata(meta ContainerMetadata) error {
	switch {
	case meta.Iteration < 1 || meta.Total < meta.Iteration || meta.Total > ContainerMaxCount:
		return fmt.Errorf("devcheck: container iteration %d of %d is outside 1 <= iteration <= total <= %d", meta.Iteration, meta.Total, ContainerMaxCount)
	case !runIDRE.MatchString(meta.RunID):
		return fmt.Errorf("devcheck: container run ID %q is invalid", meta.RunID)
	case !hex40RE.MatchString(meta.SourceRevision):
		return fmt.Errorf("devcheck: container source revision %q is not a full commit hash", meta.SourceRevision)
	case meta.Architecture != "amd64" && meta.Architecture != "arm64":
		return fmt.Errorf("devcheck: container architecture %q is not amd64 or arm64", meta.Architecture)
	}
	if len(meta.BinaryHashes) != len(containerHashKeys) {
		return fmt.Errorf("devcheck: container binary hashes must hold exactly %v", containerHashKeys)
	}
	for _, k := range containerHashKeys {
		if !hex64RE.MatchString(meta.BinaryHashes[k]) {
			return fmt.Errorf("devcheck: container binary hash %s is not a lowercase SHA-256", k)
		}
	}
	return nil
}

// hashesJSON is the compact JSON object of the three hashes (sorted keys).
func hashesJSON(h map[string]string) string {
	b, _ := json.Marshal(h)
	return string(b)
}

// containerUnsupported is the planning refusal on every non-Linux host.
func containerUnsupported(goos string) error {
	if goos != "linux" {
		return fmt.Errorf("container-e2e is unsupported on %q", goos)
	}
	return nil
}

// ContainerPlan returns one iteration's Docker argv: the image build (only
// for iteration 1; it runs in the build context directory), the isolated
// run, its inspection, the container removal and its check, and, for the
// last iteration, the image removal and its check. Only meta.Iteration and
// meta.Total select the repetition steps. Any goos but linux is refused.
func ContainerPlan(goos string, imageTag, containerName string, meta ContainerMetadata) ([]Step, error) {
	if err := containerUnsupported(goos); err != nil {
		return nil, err
	}
	if !imageTagRE.MatchString(imageTag) {
		return nil, fmt.Errorf("devcheck: container image tag %q is invalid", imageTag)
	}
	if !dockerName.MatchString(containerName) {
		return nil, fmt.Errorf("devcheck: container name %q is invalid", containerName)
	}
	if err := validateMetadata(meta); err != nil {
		return nil, err
	}
	var steps []Step
	if meta.Iteration == 1 {
		steps = append(steps, Step{Name: "container image build", Argv: []string{"docker", "build", "--network", "none", "--tag", imageTag, "--file", "Dockerfile", "."}})
	}
	run := []string{"docker", "run", "--name", containerName, "--init", "--network", "none", "--read-only",
		"--tmpfs", containerTmpfs, "--cap-drop", "ALL", "--security-opt", "no-new-privileges", "--user", containerUser,
		"--env", "HOME=/tmp/acceptance/home", "--env", "TMPDIR=/tmp",
		"--env", ContainerEnvRunID + "=" + meta.RunID,
		"--env", ContainerEnvIter + "=" + strconv.Itoa(meta.Iteration),
		"--env", ContainerEnvTotal + "=" + strconv.Itoa(meta.Total),
		"--env", ContainerEnvRevision + "=" + meta.SourceRevision,
		"--env", ContainerEnvArch + "=" + meta.Architecture,
		"--env", ContainerEnvHashes + "=" + hashesJSON(meta.BinaryHashes),
		imageTag, "-test.v", "-test.count=1", "-test.timeout=6m"}
	steps = append(steps,
		Step{Name: "container run", Argv: run},
		Step{Name: "container inspect", Argv: []string{"docker", "container", "inspect", "--format", "{{.State.Status}} {{.State.ExitCode}}", containerName}},
		Step{Name: "container remove", Argv: []string{"docker", "container", "rm", "--force", "--volumes", containerName}},
		Step{Name: "container removal check", Argv: []string{"docker", "container", "ls", "--all", "--quiet", "--filter", "name=^" + containerName + "$"}},
	)
	if meta.Iteration == meta.Total {
		steps = append(steps,
			Step{Name: "image remove", Argv: []string{"docker", "image", "rm", "--force", imageTag}},
			Step{Name: "image removal check", Argv: []string{"docker", "image", "ls", "--quiet", imageTag}},
		)
	}
	return steps, nil
}

// ---- evidence validation ----

var (
	runEventRE  = regexp.MustCompile(`^=== RUN\s+(\S+)$`)
	doneEventRE = regexp.MustCompile(`^\s*--- (PASS|FAIL|SKIP): (\S+) \(\d+(?:\.\d+)?s\)$`)
)

// containerTask is one strictly decoded task observation.
type containerTask struct {
	Label, RoleID, NodeID, TaskID                          string
	WorkspaceInstance, BaseCommit, ResultCommit, ResultRef *string
	TerminalState                                          string
	PublicationStatus                                      *string
	ExitCode                                               *int
	FinalMessage                                           *string
}

// containerRecord is one strictly decoded evidence record.
type containerRecord struct {
	kind                                     string
	runID, revision, os, arch, adapter, outc string
	iteration, total                         int
	hashes                                   map[string]string
	caseID, boundary, errText                string
	subcases                                 map[string]string
	tasks                                    []containerTask
	bindings                                 map[string]string
	hasBindings                              bool
	caseIDs                                  []string
	cleanupOK                                bool
	raw                                      []byte
}

// strictObject decodes one JSON object, rejecting duplicate keys and
// trailing data.
func strictObject(raw []byte) (map[string]json.RawMessage, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	tok, err := dec.Token()
	if err != nil || tok != json.Delim('{') {
		return nil, errors.New("not a JSON object")
	}
	out := map[string]json.RawMessage{}
	for dec.More() {
		kt, err := dec.Token()
		if err != nil {
			return nil, err
		}
		k, ok := kt.(string)
		if !ok {
			return nil, errors.New("malformed object key")
		}
		var v json.RawMessage
		if err := dec.Decode(&v); err != nil {
			return nil, err
		}
		if _, dup := out[k]; dup {
			return nil, fmt.Errorf("duplicate field %q", k)
		}
		out[k] = v
	}
	if _, err := dec.Token(); err != nil {
		return nil, err
	}
	if _, err := dec.Token(); err != io.EOF {
		return nil, errors.New("trailing data after the object")
	}
	return out, nil
}

// exactKeys requires obj to hold exactly want (plus any of optional).
func exactKeys(obj map[string]json.RawMessage, want, optional []string) error {
	allowed := map[string]bool{}
	for _, k := range append(append([]string(nil), want...), optional...) {
		allowed[k] = true
	}
	var unknown []string
	for k := range obj {
		if !allowed[k] {
			unknown = append(unknown, k)
		}
	}
	sort.Strings(unknown)
	if len(unknown) > 0 {
		return fmt.Errorf("unknown field %q", unknown[0])
	}
	for _, k := range want {
		if _, ok := obj[k]; !ok {
			return fmt.Errorf("missing field %q", k)
		}
	}
	return nil
}

func isNull(raw json.RawMessage) bool { return string(bytes.TrimSpace(raw)) == "null" }

func jsonString(raw json.RawMessage, field string) (string, error) {
	if len(raw) == 0 || raw[0] != '"' {
		return "", fmt.Errorf("field %s must be a string", field)
	}
	var s string
	if err := json.Unmarshal(raw, &s); err != nil {
		return "", fmt.Errorf("field %s must be a string", field)
	}
	return s, nil
}

func jsonNullString(raw json.RawMessage, field string) (*string, error) {
	if isNull(raw) {
		return nil, nil
	}
	s, err := jsonString(raw, field)
	return &s, err
}

func jsonInt(raw json.RawMessage, field string) (int, error) {
	s := string(bytes.TrimSpace(raw))
	if s == "" || strings.ContainsAny(s, ".eE+\"") {
		return 0, fmt.Errorf("field %s must be an integer", field)
	}
	n, err := strconv.Atoi(s)
	if err != nil {
		return 0, fmt.Errorf("field %s must be an integer", field)
	}
	return n, nil
}

func jsonNullInt(raw json.RawMessage, field string) (*int, error) {
	if isNull(raw) {
		return nil, nil
	}
	n, err := jsonInt(raw, field)
	return &n, err
}

func jsonStringMap(raw json.RawMessage, field string) (map[string]string, error) {
	obj, err := strictObject(raw)
	if err != nil {
		return nil, fmt.Errorf("field %s must be an object of strings: %v", field, err)
	}
	out := map[string]string{}
	for k, v := range obj {
		s, err := jsonString(v, field+"."+k)
		if err != nil {
			return nil, err
		}
		out[k] = s
	}
	return out, nil
}

func jsonArray(raw json.RawMessage, field string) ([]json.RawMessage, error) {
	if len(raw) == 0 || raw[0] != '[' {
		return nil, fmt.Errorf("field %s must be an array", field)
	}
	var out []json.RawMessage
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, fmt.Errorf("field %s must be an array", field)
	}
	return out, nil
}

var (
	commonKeys = []string{"schema_version", "kind", "run_id", "iteration", "total", "source_revision", "os", "arch", "binary_hashes", "adapter", "outcome"}
	caseKeys   = []string{"case_id", "subcases", "tasks", "boundary", "error"}
	endKeys    = []string{"case_ids", "cleanup_ok", "error"}
	taskKeys   = []string{"label", "role_id", "node_id", "task_id", "workspace_instance", "base_commit", "result_commit", "result_ref",
		"terminal_state", "publication_status", "exit_code", "final_message"}
)

// parseContainerTask strictly decodes one task object.
func parseContainerTask(raw json.RawMessage) (containerTask, error) {
	obj, err := strictObject(raw)
	if err != nil {
		return containerTask{}, err
	}
	if err := exactKeys(obj, taskKeys, nil); err != nil {
		return containerTask{}, err
	}
	var t containerTask
	var errs []error
	str := func(k string, dst *string) {
		v, err := jsonString(obj[k], k)
		errs = append(errs, err)
		*dst = v
	}
	nstr := func(k string, dst **string) {
		v, err := jsonNullString(obj[k], k)
		errs = append(errs, err)
		*dst = v
	}
	str("label", &t.Label)
	str("role_id", &t.RoleID)
	str("node_id", &t.NodeID)
	str("task_id", &t.TaskID)
	str("terminal_state", &t.TerminalState)
	nstr("workspace_instance", &t.WorkspaceInstance)
	nstr("base_commit", &t.BaseCommit)
	nstr("result_commit", &t.ResultCommit)
	nstr("result_ref", &t.ResultRef)
	nstr("publication_status", &t.PublicationStatus)
	nstr("final_message", &t.FinalMessage)
	exit, err := jsonNullInt(obj["exit_code"], "exit_code")
	errs = append(errs, err)
	t.ExitCode = exit
	return t, errors.Join(errs...)
}

// parseContainerRecord strictly decodes one record's JSON.
func parseContainerRecord(raw []byte) (containerRecord, error) {
	r := containerRecord{raw: raw}
	obj, err := strictObject(raw)
	if err != nil {
		return r, err
	}
	kind, err := jsonString(obj["kind"], "kind")
	if err != nil {
		return r, err
	}
	r.kind = kind
	switch kind {
	case "case":
		var opt []string
		if _, ok := obj["node_bindings"]; ok {
			opt = []string{"node_bindings"}
		}
		if err := exactKeys(obj, append(append([]string(nil), commonKeys...), caseKeys...), opt); err != nil {
			return r, err
		}
	case "end":
		if err := exactKeys(obj, append(append([]string(nil), commonKeys...), endKeys...), nil); err != nil {
			return r, err
		}
	default:
		return r, fmt.Errorf("unknown kind %q", kind)
	}
	var errs []error
	str := func(k string, dst *string) {
		v, err := jsonString(obj[k], k)
		errs = append(errs, err)
		*dst = v
	}
	num := func(k string, dst *int) {
		v, err := jsonInt(obj[k], k)
		errs = append(errs, err)
		*dst = v
	}
	var schema int
	num("schema_version", &schema)
	if schema != 1 {
		errs = append(errs, fmt.Errorf("schema_version must be 1"))
	}
	str("run_id", &r.runID)
	num("iteration", &r.iteration)
	num("total", &r.total)
	str("source_revision", &r.revision)
	str("os", &r.os)
	str("arch", &r.arch)
	str("adapter", &r.adapter)
	str("outcome", &r.outc)
	str("error", &r.errText)
	r.hashes, err = jsonStringMap(obj["binary_hashes"], "binary_hashes")
	errs = append(errs, err)
	if kind == "end" {
		items, err := jsonArray(obj["case_ids"], "case_ids")
		errs = append(errs, err)
		for _, it := range items {
			s, err := jsonString(it, "case_ids[]")
			errs = append(errs, err)
			r.caseIDs = append(r.caseIDs, s)
		}
		switch strings.TrimSpace(string(obj["cleanup_ok"])) {
		case "true":
			r.cleanupOK = true
		case "false":
		default:
			errs = append(errs, errors.New("field cleanup_ok must be a boolean"))
		}
		return r, errors.Join(errs...)
	}
	str("case_id", &r.caseID)
	str("boundary", &r.boundary)
	r.subcases, err = jsonStringMap(obj["subcases"], "subcases")
	errs = append(errs, err)
	items, err := jsonArray(obj["tasks"], "tasks")
	errs = append(errs, err)
	for i, it := range items {
		t, err := parseContainerTask(it)
		if err != nil {
			errs = append(errs, fmt.Errorf("tasks[%d]: %v", i, err))
		}
		r.tasks = append(r.tasks, t)
	}
	if raw, ok := obj["node_bindings"]; ok {
		r.hasBindings = true
		r.bindings, err = jsonStringMap(raw, "node_bindings")
		errs = append(errs, err)
	}
	return r, errors.Join(errs...)
}

// evidenceProblems collects one stream's validation problems.
type evidenceProblems struct {
	list []string
}

func (p *evidenceProblems) add(format string, args ...any) {
	if len(p.list) < 64 {
		p.list = append(p.list, fmt.Sprintf(format, args...))
	}
}

func (p *evidenceProblems) err() error {
	if len(p.list) == 0 {
		return nil
	}
	return errors.New("devcheck: container evidence rejected:\n  " + strings.Join(p.list, "\n  "))
}

// containerStream is one iteration's parsed output.
type containerStream struct {
	records []containerRecord
	runs    map[string]bool
	results map[string]string // test name -> PASS, FAIL or SKIP (worst)
	passed  bool              // a final PASS line
	failed  bool              // a final FAIL line or a panic
}

// scanContainerStream splits r into evidence records and verbose test
// events; every other line is diagnostic and ignored. A malformed or
// oversized record is a problem, never skipped.
func scanContainerStream(r io.Reader, p *evidenceProblems) containerStream {
	s := containerStream{runs: map[string]bool{}, results: map[string]string{}}
	br := bufio.NewReaderSize(r, 64<<10)
	n := 0
	for {
		line, err := br.ReadBytes('\n')
		if len(line) > 0 {
			n++
			complete := line[len(line)-1] == '\n'
			text := strings.TrimRight(string(line), "\r\n")
			switch {
			case strings.HasPrefix(text, ContainerEvidencePrefix):
				raw := []byte(strings.TrimPrefix(text, ContainerEvidencePrefix))
				var compact bytes.Buffer
				switch {
				case !complete:
					p.add("line %d: evidence record truncated (no final newline)", n)
				case len(raw) > containerMaxRecord:
					p.add("line %d: evidence record exceeds %d bytes", n, containerMaxRecord)
				case json.Compact(&compact, raw) != nil || !bytes.Equal(compact.Bytes(), raw):
					p.add("line %d: evidence record is not one compact JSON object", n)
				default:
					rec, err := parseContainerRecord(raw)
					if err != nil {
						p.add("line %d: malformed evidence record: %v", n, err)
						continue
					}
					s.records = append(s.records, rec)
				}
			case runEventRE.MatchString(text):
				s.runs[runEventRE.FindStringSubmatch(text)[1]] = true
			case doneEventRE.MatchString(text):
				m := doneEventRE.FindStringSubmatch(text)
				if prev := s.results[m[2]]; prev == "" || prev == "PASS" {
					s.results[m[2]] = m[1]
				}
			case text == "PASS":
				s.passed = true
			case text == "FAIL" || strings.HasPrefix(text, "panic: "):
				s.failed = true
			}
		}
		if err != nil {
			if err != io.EOF {
				p.add("reading the container output: %v", err)
			}
			return s
		}
	}
}

// ValidateContainerEvidence validates one iteration's container output:
// its prefixed evidence records (strict schema, matching common fields,
// exactly the expected case IDs once each with their required subcases and
// passing outcomes, the per-task expected catalog with the iteration's
// frozen runtime node bindings, and one final end record with completed
// cleanup) and its verbose test events (every parent and required subtest
// ran and passed; nothing failed or skipped).
func ValidateContainerEvidence(r io.Reader, expected ContainerExpectation) error {
	_, err := validateContainerStream(r, expected)
	return err
}

// validateContainerStream is ValidateContainerEvidence, also returning the
// raw JSON of every received well-formed record in order, whatever the
// verdict: a failed, partial or rejected iteration's evidence is retained
// in the ledger as received (the error, never the ledger, decides the
// gate). Malformed lines are not records; they stay in the diagnostics and
// the error.
func validateContainerStream(r io.Reader, expected ContainerExpectation) ([][]byte, error) {
	p := &evidenceProblems{}
	meta := expected.Metadata
	if err := validateMetadata(meta); err != nil {
		return nil, err
	}
	s := scanContainerStream(r, p)
	want := map[string]bool{}
	for _, id := range expected.CaseIDs {
		want[id] = true
	}
	seen := map[string]bool{}
	var bindings map[string]string
	endSeen := false
	var raws [][]byte
	for i, rec := range s.records {
		where := fmt.Sprintf("record %d", i+1)
		if rec.kind == "case" {
			where = "case " + rec.caseID
		}
		raws = append(raws, rec.raw)
		checkCommon(p, where, rec, meta)
		if endSeen {
			p.add("%s: a record follows the end record", where)
		}
		if rec.kind == "end" {
			endSeen = true
			checkEnd(p, rec, expected.CaseIDs, seen)
			continue
		}
		switch {
		case !want[rec.caseID]:
			p.add("%s: unknown case ID", where)
			continue
		case seen[rec.caseID]:
			p.add("%s: duplicate case record for iteration %d", where, rec.iteration)
			continue
		}
		seen[rec.caseID] = true
		if rec.outc != "pass" || rec.errText != "" {
			p.add("%s: outcome %q (error %q), want pass", where, rec.outc, rec.errText)
		}
		checkSubcases(p, where, rec.subcases, expected.Subcases[rec.caseID])
		if b := containerBoundaries[rec.caseID]; rec.boundary != b {
			p.add("%s: boundary %q, want %q", where, rec.boundary, b)
		}
		if rec.caseID == CaseRuntime {
			bindings = checkBindings(p, where, rec)
		} else if rec.hasBindings {
			p.add("%s: field node_bindings is allowed only on %s", where, CaseRuntime)
		}
		if len(rec.tasks) > 0 && bindings == nil {
			p.add("%s: task-bearing record precedes the %s node bindings", where, CaseRuntime)
		}
		auditTasks(p, rec, bindings)
	}
	for _, id := range expected.CaseIDs {
		if !seen[id] {
			p.add("case %s: missing case record", id)
		}
	}
	if !endSeen {
		p.add("missing end record")
	}
	checkEvents(p, s, expected)
	return raws, p.err()
}

// checkCommon compares a record's common fields with the expected metadata.
func checkCommon(p *evidenceProblems, where string, rec containerRecord, meta ContainerMetadata) {
	switch {
	case rec.runID != meta.RunID:
		p.add("%s: run_id %q, want %q", where, rec.runID, meta.RunID)
	case rec.iteration != meta.Iteration:
		p.add("%s: iteration %d, want %d", where, rec.iteration, meta.Iteration)
	case rec.total != meta.Total:
		p.add("%s: total %d, want %d", where, rec.total, meta.Total)
	case rec.revision != meta.SourceRevision:
		p.add("%s: source_revision %q, want %q", where, rec.revision, meta.SourceRevision)
	case rec.os != "linux":
		p.add("%s: os %q, want linux", where, rec.os)
	case rec.arch != meta.Architecture:
		p.add("%s: arch %q, want %q", where, rec.arch, meta.Architecture)
	case rec.adapter != "fake":
		p.add("%s: adapter %q, want fake", where, rec.adapter)
	case hashesJSON(rec.hashes) != hashesJSON(meta.BinaryHashes):
		p.add("%s: binary_hashes %s, want %s", where, hashesJSON(rec.hashes), hashesJSON(meta.BinaryHashes))
	}
}

// checkEnd checks the end record: the sorted exact manifest, completed
// cleanup and a passing outcome after every case record.
func checkEnd(p *evidenceProblems, rec containerRecord, caseIDs []string, seen map[string]bool) {
	want := append([]string(nil), caseIDs...)
	sort.Strings(want)
	if strings.Join(rec.caseIDs, "\n") != strings.Join(want, "\n") {
		p.add("end record: case_ids %v, want the sorted manifest %v", rec.caseIDs, want)
	}
	if !rec.cleanupOK {
		p.add("end record: cleanup_ok is false")
	}
	if rec.outc != "pass" || rec.errText != "" {
		p.add("end record: outcome %q (error %q), want pass", rec.outc, rec.errText)
	}
	for _, id := range caseIDs {
		if !seen[id] {
			p.add("end record: precedes the case record %s", id)
		}
	}
}

// checkSubcases requires exactly the required subcase names, all passing.
func checkSubcases(p *evidenceProblems, where string, got map[string]string, want []string) {
	if len(got) != len(want) {
		p.add("%s: subcases %v, want exactly %v", where, got, want)
		return
	}
	for _, name := range want {
		if v, ok := got[name]; !ok {
			p.add("%s: subcase %s missing", where, name)
		} else if v != "pass" {
			p.add("%s: subcase %s is %q, want pass", where, name, v)
		}
	}
}

// checkBindings validates the runtime record's frozen a/b node bindings.
func checkBindings(p *evidenceProblems, where string, rec containerRecord) map[string]string {
	b := rec.bindings
	switch {
	case !rec.hasBindings:
		p.add("%s: missing field node_bindings", where)
		return nil
	case len(b) != 2 || b["a"] == "" || b["b"] == "":
		p.add("%s: node_bindings %v must map exactly a and b", where, b)
		return nil
	case !nodeID32RE.MatchString(b["a"]) || !nodeID32RE.MatchString(b["b"]):
		p.add("%s: node_bindings %v holds an invalid node ID", where, b)
		return nil
	case b["a"] == b["b"]:
		p.add("%s: node_bindings a and b are the same node", where)
		return nil
	}
	return b
}

// auditTasks checks a case record's tasks against the static catalog: the
// exact ordered rows, every profile field and nullability, the frozen node
// bindings, unique valid task IDs, task-derived refs and the in-case chain
// relationships.
func auditTasks(p *evidenceProblems, rec containerRecord, bindings map[string]string) {
	rows := containerCatalog[rec.caseID]
	where := "case " + rec.caseID
	if len(rec.tasks) != len(rows) {
		p.add("%s: %d tasks, want %d", where, len(rec.tasks), len(rows))
		return
	}
	ids := map[string]bool{}
	for i, t := range rec.tasks {
		row := rows[i]
		bad := func(field, format string, args ...any) {
			p.add("%s task %d field %s: %s", where, i, field, fmt.Sprintf(format, args...))
		}
		prof := taskProfiles[row.profile]
		if t.Label != row.label {
			bad("label", "%q, want %q", t.Label, row.label)
		}
		if t.RoleID != row.role {
			bad("role_id", "%q, want %q", t.RoleID, row.role)
		}
		if bindings != nil && t.NodeID != bindings[row.alias] {
			bad("node_id", "%q, want node %s (%s)", t.NodeID, row.alias, bindings[row.alias])
		}
		switch {
		case !taskID32RE.MatchString(t.TaskID):
			bad("task_id", "%q is not a task ID", t.TaskID)
		case ids[t.TaskID]:
			bad("task_id", "%q is duplicated in the case", t.TaskID)
		}
		ids[t.TaskID] = true
		if t.TerminalState != prof.state {
			bad("terminal_state", "%q, want %q", t.TerminalState, prof.state)
		}
		if !eqStr(t.PublicationStatus, prof.publication) {
			bad("publication_status", "%s, want %s", showStr(t.PublicationStatus), showStr(prof.publication))
		}
		if !eqInt(t.ExitCode, prof.exit) {
			bad("exit_code", "%s, want %s", showInt(t.ExitCode), showInt(prof.exit))
		}
		if !eqStr(t.FinalMessage, prof.final) {
			bad("final_message", "%s, want %s", showStr(t.FinalMessage), showStr(prof.final))
		}
		nullability := func(field string, v *string, wantSet bool, valid func(string) bool) {
			switch {
			case wantSet && v == nil:
				bad(field, "null, want a value")
			case !wantSet && v != nil:
				bad(field, "%q, want null", *v)
			case v != nil && !valid(*v):
				bad(field, "%q is malformed", *v)
			}
		}
		nullability("workspace_instance", t.WorkspaceInstance, prof.instanceBase, hex32RE.MatchString)
		nullability("base_commit", t.BaseCommit, prof.instanceBase, hex40RE.MatchString)
		nullability("result_commit", t.ResultCommit, prof.resultRef, hex40RE.MatchString)
		nullability("result_ref", t.ResultRef, prof.resultRef, func(s string) bool { return s == "refs/callsheet/tasks/"+t.TaskID })
	}
	relate := func(i int, field string, got, want *string, what string) {
		if got == nil || want == nil || *got != *want {
			p.add("%s task %d field %s: %s, want %s (%s)", where, i, field, showStr(got), showStr(want), what)
		}
	}
	ts := rec.tasks
	switch containerRelations[rec.caseID] {
	case relChain:
		for i := 1; i < len(ts); i++ {
			relate(i, "workspace_instance", ts[i].WorkspaceInstance, ts[0].WorkspaceInstance, "one sample workspace")
			relate(i, "base_commit", ts[i].BaseCommit, ts[i-1].ResultCommit, "the preceding result")
		}
	case relContinuation:
		relate(1, "workspace_instance", ts[1].WorkspaceInstance, ts[0].WorkspaceInstance, "A's instance")
		relate(2, "workspace_instance", ts[2].WorkspaceInstance, ts[0].WorkspaceInstance, "A's instance")
		relate(1, "base_commit", ts[1].BaseCommit, ts[0].ResultCommit, "A's result")
		relate(2, "base_commit", ts[2].BaseCommit, ts[0].BaseCommit, "A's original base")
	case relSameBase:
		relate(1, "workspace_instance", ts[1].WorkspaceInstance, ts[0].WorkspaceInstance, "the first task's instance")
		relate(1, "base_commit", ts[1].BaseCommit, ts[0].BaseCommit, "the original base")
	}
}

func eqStr(a, b *string) bool { return (a == nil) == (b == nil) && (a == nil || *a == *b) }
func eqInt(a, b *int) bool    { return (a == nil) == (b == nil) && (a == nil || *a == *b) }

func showStr(s *string) string {
	if s == nil {
		return "null"
	}
	return strconv.Quote(*s)
}

func showInt(n *int) string {
	if n == nil {
		return "null"
	}
	return strconv.Itoa(*n)
}

// checkEvents requires every parent and required subtest to have run and
// passed, nothing to have failed or been skipped, and the final PASS.
func checkEvents(p *evidenceProblems, s containerStream, expected ContainerExpectation) {
	var required []string
	for _, id := range expected.CaseIDs {
		if !strings.Contains(id, "/") {
			required = append(required, id)
		}
	}
	parents := append([]string(nil), required...)
	for _, parent := range parents {
		for _, sub := range expected.Subcases[parent] {
			required = append(required, parent+"/"+sub)
		}
	}
	for _, name := range required {
		if !s.runs[name] {
			p.add("test %s: no run event", name)
		}
		if got := s.results[name]; got != "PASS" {
			if got == "" {
				got = "no result"
			}
			p.add("test %s: %s, want PASS", name, got)
		}
	}
	var others []string
	for name, res := range s.results {
		if res != "PASS" {
			others = append(others, name+" "+res)
		}
	}
	sort.Strings(others)
	for _, o := range others {
		p.add("test %s", o)
	}
	if s.failed || !s.passed {
		p.add("the test binary did not report PASS")
	}
}

// validateContainerLedger checks the invocation's complete ledger: the
// iterations are the contiguous set 1..total, each with exactly the
// manifest's case records and one end record.
func validateContainerLedger(raws [][]byte, total int, caseIDs []string) error {
	p := &evidenceProblems{}
	type key struct {
		it int
		id string
	}
	cases := map[key]bool{}
	ends := map[int]bool{}
	nCase := 0
	for i, raw := range raws {
		rec, err := parseContainerRecord(raw)
		if err != nil {
			p.add("ledger record %d: %v", i+1, err)
			continue
		}
		if rec.iteration < 1 || rec.iteration > total {
			p.add("ledger record %d: iteration %d outside 1..%d", i+1, rec.iteration, total)
		}
		if rec.kind == "end" {
			if ends[rec.iteration] {
				p.add("ledger: duplicate end record for iteration %d", rec.iteration)
			}
			ends[rec.iteration] = true
			continue
		}
		k := key{rec.iteration, rec.caseID}
		if cases[k] {
			p.add("ledger: duplicate case %s in iteration %d", rec.caseID, rec.iteration)
		}
		cases[k] = true
		nCase++
	}
	for it := 1; it <= total; it++ {
		if !ends[it] {
			p.add("ledger: iteration %d has no end record", it)
		}
		for _, id := range caseIDs {
			if !cases[key{it, id}] {
				p.add("ledger: iteration %d lacks case %s", it, id)
			}
		}
	}
	if nCase != len(caseIDs)*total || len(ends) != total {
		p.add("ledger: %d case and %d end records, want %d and %d", nCase, len(ends), len(caseIDs)*total, total)
	}
	return p.err()
}

// ---- the driver operation ----

// moduleRoot walks up from the working directory to the go.mod directory.
func moduleRoot() (string, error) {
	dir, err := os.Getwd()
	if err != nil {
		return "", err
	}
	for {
		if st, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil && st.Mode().IsRegular() {
			return dir, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", errors.New("devcheck: no go.mod above the working directory")
		}
		dir = parent
	}
}

// sourceRevision is root's HEAD commit (read with go-git; no git child).
func sourceRevision(root string) (string, error) {
	repo, err := git.PlainOpenWithOptions(root, &git.PlainOpenOptions{DetectDotGit: true})
	if err != nil {
		return "", fmt.Errorf("devcheck: source revision: %w", err)
	}
	head, err := repo.Head()
	if err != nil {
		return "", fmt.Errorf("devcheck: source revision: %w", err)
	}
	return head.Hash().String(), nil
}

// fileSHA256 is a file's lowercase SHA-256.
func fileSHA256(p string) (string, error) {
	f, err := os.Open(p)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// copyFile copies a regular file with mode.
func copyFile(dst, src string, mode fs.FileMode) error {
	b, err := os.ReadFile(src)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	return os.WriteFile(dst, b, mode)
}

// containerBinaries are the host-built static binaries: context file name,
// hash key and argv after "go".
type containerBinary struct {
	file, key string
	argv      func(out string) []string
}

var containerBinaries = []containerBinary{
	{"callsheet", HashCallsheet, func(out string) []string { return []string{"go", "build", "-trimpath", "-o", out, "./cmd/callsheet"} }},
	{"fake-adapter", HashFakeAdapter, func(out string) []string {
		return []string{"go", "build", "-trimpath", "-o", out, "./cmd/fake-adapter"}
	}},
	{"acceptance.test", HashAcceptanceTest, func(out string) []string {
		return []string{"go", "test", "-c", "-trimpath", "-tags=" + ContainerTestTag, "-o", out, containerTestPackage}
	}},
}

// containerFixtureDocs are the bundled milestone documents.
var containerFixtureDocs = []string{"docs/real-adapters.md", "docs/workspaces.md", "docs/ci.md"}

// containerBuildEnv is the static, offline build environment.
func containerBuildEnv(arch string) []string {
	return []string{"CGO_ENABLED=0", "GOOS=linux", "GOARCH=" + arch, "GOPROXY=off", "GOSUMDB=off", "GOFLAGS=-mod=readonly"}
}

// containerRunner runs one docker or go child with the driver's runner,
// logging its argv and capturing bounded diagnostics.
func (d *driver) containerChild(ctx context.Context, s Step, dir string, stdout io.Writer) error {
	fmt.Fprintf(d.out, "devcheck: %s: %s\n", s.Name, strings.Join(s.Argv, " "))
	diag := d.evidence.diag
	fmt.Fprintf(diag, "== %s: %s\n", s.Name, strings.Join(s.Argv, " "))
	var stderr bytes.Buffer
	errw := io.MultiWriter(&stderr, diag)
	if stdout == nil {
		stdout = diag
	}
	env := s.Env
	if len(s.Argv) > 0 && s.Argv[0] == "docker" {
		env = append(append([]string(nil), d.dockerEnv...), env...)
	}
	err := d.run(ctx, s.Argv, MergeEnv(os.Environ(), env), dir, stdout, errw)
	if err != nil {
		return &StepError{Step: s, Err: err, Stderr: stderr.String()}
	}
	return nil
}

// dockerProbe distinguishes a missing docker executable from an
// unreachable daemon. It reads only the current context's daemon endpoint
// from the user's client configuration; every later docker child runs with
// an empty, private client configuration in scratch (DOCKER_CONFIG) and
// that endpoint, so no registry credential or helper of the host is read
// or used by the build and run.
func (d *driver) dockerProbe(ctx context.Context) error {
	var endpoint bytes.Buffer
	err := d.containerChild(ctx, Step{Name: "docker endpoint", Argv: []string{"docker", "context", "inspect", "--format", "{{.Endpoints.docker.Host}}"}}, "", &endpoint)
	switch {
	case errors.Is(err, exec.ErrNotFound):
		return errDockerMissing
	case err != nil:
		fmt.Fprintf(d.evidence.diag, "docker endpoint failed: %v\n", err)
		return errDockerUnavailable
	}
	config := filepath.Join(d.scratch, "docker-config")
	if err := os.MkdirAll(config, 0o700); err != nil {
		return err
	}
	d.dockerEnv = []string{"DOCKER_CONFIG=" + config}
	if host := strings.TrimSpace(endpoint.String()); host != "" {
		d.dockerEnv = append(d.dockerEnv, "DOCKER_HOST="+host)
	}
	var out bytes.Buffer
	err = d.containerChild(ctx, Step{Name: "docker probe", Argv: []string{"docker", "version", "--format", "{{.Server.Version}}"}}, "", &out)
	switch {
	case err == nil:
		fmt.Fprintf(d.evidence.diag, "docker server %s\n", strings.TrimSpace(out.String()))
		return nil
	case errors.Is(err, exec.ErrNotFound):
		return errDockerMissing
	}
	fmt.Fprintf(d.evidence.diag, "docker probe failed: %v\n", err)
	return errDockerUnavailable
}

// buildContainerContext compiles the three static binaries into dir,
// checks they are linux/arch executables, hashes them and adds the
// Dockerfile and fixture bundle (manuals and the working tree's milestone
// docs).
func (d *driver) buildContainerContext(ctx context.Context, root, dir, arch string) (map[string]string, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	hashes := map[string]string{}
	for _, b := range containerBinaries {
		out := filepath.Join(dir, b.file)
		if err := d.containerChild(ctx, Step{Name: "container build " + b.file, Argv: b.argv(out), Env: containerBuildEnv(arch)}, root, nil); err != nil {
			return nil, err
		}
		got, err := InspectExecutable(out)
		if err != nil {
			return nil, fmt.Errorf("devcheck: container binary %s: %v", b.file, err)
		}
		if want := (Target{"linux", arch}); got != want {
			return nil, fmt.Errorf("devcheck: container binary %s is built for %s, want %s", b.file, got, want)
		}
		if err := os.Chmod(out, 0o755); err != nil {
			return nil, err
		}
		if hashes[b.key], err = fileSHA256(out); err != nil {
			return nil, err
		}
	}
	if err := copyFile(filepath.Join(dir, "Dockerfile"), filepath.Join(root, "tests", "container", "Dockerfile"), 0o644); err != nil {
		return nil, fmt.Errorf("devcheck: container Dockerfile: %w", err)
	}
	manuals := filepath.Join(root, "tests", "container", "fixtures", "manuals")
	err := filepath.WalkDir(manuals, func(p string, e fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !e.Type().IsRegular() {
			if e.IsDir() {
				return nil
			}
			return fmt.Errorf("%s is not a regular file", p)
		}
		rel, _ := filepath.Rel(manuals, p)
		return copyFile(filepath.Join(dir, "fixtures", "manuals", rel), p, 0o644)
	})
	if err != nil {
		return nil, fmt.Errorf("devcheck: container manuals: %w", err)
	}
	for _, doc := range containerFixtureDocs {
		if err := copyFile(filepath.Join(dir, "fixtures", "docs", filepath.Base(doc)), filepath.Join(root, filepath.FromSlash(doc)), 0o644); err != nil {
			return nil, fmt.Errorf("devcheck: container docs: %w", err)
		}
	}
	return hashes, nil
}

// lineSplitter keeps evidence and test-event lines complete (they are
// validated) and sends every line to the bounded diagnostics.
type lineSplitter struct {
	mu      sync.Mutex
	keep    bytes.Buffer
	partial []byte
	diag    io.Writer
	echo    io.Writer
}

func (l *lineSplitter) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.diag.Write(p)
	l.partial = append(l.partial, p...)
	for {
		i := bytes.IndexByte(l.partial, '\n')
		if i < 0 {
			break
		}
		l.line(l.partial[:i+1])
		l.partial = l.partial[i+1:]
	}
	return len(p), nil
}

func (l *lineSplitter) line(b []byte) {
	text := strings.TrimRight(string(b), "\r\n")
	if strings.HasPrefix(text, ContainerEvidencePrefix) || runEventRE.MatchString(text) || doneEventRE.MatchString(text) ||
		text == "PASS" || text == "FAIL" || strings.HasPrefix(text, "panic: ") {
		l.keep.Write(b)
		if !strings.HasPrefix(text, ContainerEvidencePrefix) && l.echo != nil {
			l.echo.Write(b)
		}
	}
}

// stream returns the kept lines plus any unterminated tail (kept so a
// truncated record is detected).
func (l *lineSplitter) stream() []byte {
	l.mu.Lock()
	defer l.mu.Unlock()
	out := append([]byte(nil), l.keep.Bytes()...)
	if len(l.partial) > 0 && strings.HasPrefix(string(l.partial), ContainerEvidencePrefix) {
		out = append(out, l.partial...)
	}
	return out
}

// containerCleanup removes the container (if named) and the image,
// bounded on a fresh context so an expired deadline still cleans up, and
// verifies both are gone.
func (d *driver) containerCleanup(name, tag string) error {
	ctx, cancel := context.WithTimeout(context.Background(), containerCleanupDeadline)
	defer cancel()
	var errs []error
	if name != "" {
		errs = append(errs, d.removeChecked(ctx, Step{Name: "container remove", Argv: []string{"docker", "container", "rm", "--force", "--volumes", name}},
			Step{Name: "container removal check", Argv: []string{"docker", "container", "ls", "--all", "--quiet", "--filter", "name=^" + name + "$"}}))
	}
	if tag != "" {
		errs = append(errs, d.removeChecked(ctx, Step{Name: "image remove", Argv: []string{"docker", "image", "rm", "--force", tag}},
			Step{Name: "image removal check", Argv: []string{"docker", "image", "ls", "--quiet", tag}}))
	}
	return errors.Join(errs...)
}

// removeChecked runs a removal, then its check, which must list nothing.
// The removal's own failure (for example an object already gone) is
// diagnostic only: the check decides.
func (d *driver) removeChecked(ctx context.Context, remove, check Step) error {
	if err := d.containerChild(ctx, remove, "", nil); err != nil {
		fmt.Fprintf(d.evidence.diag, "%s: %v (the removal check decides)\n", remove.Name, err)
	}
	var out bytes.Buffer
	if err := d.containerChild(ctx, check, "", &out); err != nil {
		return err
	}
	if left := strings.TrimSpace(out.String()); left != "" {
		return fmt.Errorf("devcheck: %s: still present after removal: %s", check.Name, left)
	}
	return nil
}

// containerIteration runs one planned iteration: the build (iteration 1),
// the container, its inspection and removal, then the evidence
// validation. It returns every received record (validated or not, so a
// failed iteration's evidence is retained) and the iteration's error. The
// container is removed on every path; a deadline stops it explicitly.
func (d *driver) containerIteration(ctx context.Context, ctxDir, tag string, meta ContainerMetadata) ([][]byte, error) {
	name := fmt.Sprintf("callsheet-e2e-%s-%d", meta.RunID, meta.Iteration)
	steps, err := ContainerPlan(d.goos, tag, name, meta)
	if err != nil {
		return nil, err
	}
	byName := map[string]Step{}
	for _, s := range steps {
		byName[s.Name] = s
	}
	if s, ok := byName["container image build"]; ok {
		if err := d.containerChild(ctx, s, ctxDir, nil); err != nil {
			return nil, err
		}
	}
	split := &lineSplitter{diag: d.evidence.diag, echo: d.out}
	runErr := d.containerChild(ctx, byName["container run"], "", split)
	var errs []error
	if ctx.Err() != nil {
		errs = append(errs, fmt.Errorf("devcheck: container iteration %d deadline (%v) expired: %w", meta.Iteration, containerIterationDeadline, ctx.Err()))
	}
	if runErr != nil {
		errs = append(errs, fmt.Errorf("devcheck: container iteration %d: %w", meta.Iteration, runErr))
	}
	cctx, cancel := context.WithTimeout(context.Background(), containerCleanupDeadline)
	defer cancel()
	var inspect bytes.Buffer
	if err := d.containerChild(cctx, byName["container inspect"], "", &inspect); err != nil {
		errs = append(errs, err)
	} else if got := strings.TrimSpace(inspect.String()); got != "exited 0" {
		errs = append(errs, fmt.Errorf("devcheck: container iteration %d state %q, want \"exited 0\"", meta.Iteration, got))
	}
	errs = append(errs, d.removeChecked(cctx, byName["container remove"], byName["container removal check"]))
	if s, ok := byName["image remove"]; ok {
		errs = append(errs, d.removeChecked(cctx, s, byName["image removal check"]))
	}
	raws, verr := validateContainerStream(bytes.NewReader(split.stream()), containerExpectation(meta))
	if verr != nil {
		errs = append(errs, verr)
	}
	return raws, errors.Join(errs...)
}

// containerE2E is the container acceptance operation for count fresh
// containers (one shared build). It needs the open evidence report and
// records every phase, the validated ledger and the outcome there; the
// overall pass is written only after every iteration, exit and cleanup
// validated.
func (d *driver) containerE2E(count int) (err error) {
	rep := d.evidence
	if rep == nil {
		return errors.New("devcheck: container evidence is not open")
	}
	if err := containerUnsupported(d.goos); err != nil {
		return err
	}
	if count < 1 || count > ContainerMaxCount {
		return fmt.Errorf("devcheck: container count %d is outside 1..%d", count, ContainerMaxCount)
	}
	rep.count = count
	rep.outcome, rep.progress = "running", "setup"
	defer func() {
		if err != nil {
			rep.fail(err)
		}
	}()
	ctx, cancel := context.WithTimeout(d.ctx, time.Duration(count)*containerIterationDeadline)
	defer cancel()
	// Iteration 1's deadline also charges the shared setup and build.
	ictx, icancel := context.WithTimeout(ctx, containerIterationDeadline)
	defer icancel()
	root, err := moduleRoot()
	if err != nil {
		return err
	}
	if rep.revision, err = sourceRevision(root); err != nil {
		return err
	}
	arch := runtime.GOARCH
	if err := ValidateTarget(Target{"linux", arch}); err != nil {
		return err
	}
	rep.phase("docker probe")
	if err := d.dockerProbe(ictx); err != nil {
		return err
	}
	rep.phase("build: static binaries, image context")
	ctxDir := filepath.Join(d.scratch, "container-context")
	buildStart := time.Now()
	hashes, err := d.buildContainerContext(ictx, root, ctxDir, arch)
	if err != nil {
		return err
	}
	// Elapsed times are reported (never gated): the static build here, the
	// image build with iteration 1.
	fmt.Fprintf(d.out, "devcheck: container-e2e: static binaries built in %.1fs\n", time.Since(buildStart).Seconds())
	rep.phase("static binaries built in %.1fs", time.Since(buildStart).Seconds())
	tag := "callsheet-e2e:" + rep.RunID
	imageOwned := true
	defer func() {
		if imageOwned {
			if cerr := d.containerCleanup("", tag); cerr != nil {
				err = errors.Join(err, cerr)
			}
		}
	}()
	for it := 1; it <= count; it++ {
		meta := ContainerMetadata{RunID: rep.RunID, SourceRevision: rep.revision, Architecture: arch, Iteration: it, Total: count, BinaryHashes: hashes}
		rep.progress = fmt.Sprintf("iteration %d of %d", it, count)
		rep.phase("iteration %d of %d: run", it, count)
		start := time.Now()
		var raws [][]byte
		if it == 1 {
			raws, err = d.containerIteration(ictx, ctxDir, tag, meta)
		} else {
			// Every later iteration has its own fresh deadline.
			raws, err = func() ([][]byte, error) {
				c, cancel := context.WithTimeout(ctx, containerIterationDeadline)
				defer cancel()
				return d.containerIteration(c, ctxDir, tag, meta)
			}()
		}
		// Retain the received evidence before the verdict: a failed or
		// partial iteration's records reach ledger.jsonl and the report too.
		if len(raws) > 0 {
			if lerr := rep.appendLedger(raws); lerr != nil {
				err = errors.Join(err, lerr)
			}
		}
		if err != nil {
			return err
		}
		fmt.Fprintf(d.out, "devcheck: container-e2e: iteration %d of %d validated (%d records, %.1fs)\n", it, count, len(raws), time.Since(start).Seconds())
		if it == count {
			// The last iteration's plan removed the image.
			imageOwned = false
		}
	}
	if err := validateContainerLedger(rep.ledger, count, ContainerCaseIDs()); err != nil {
		return err
	}
	rep.outcome, rep.progress = "pass", "complete"
	return rep.phase("all %d iterations validated; containers and image removed", count)
}
