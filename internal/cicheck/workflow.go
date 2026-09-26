// Package cicheck is the development-only, offline validator for the CI
// workflow contract (.github/workflows/ci.yml). It is a deliberately narrow
// structural checker for this one workflow, not a GitHub Actions expression
// interpreter, and it never contacts the network, downloads actions or runs
// a linter. Neither it nor its YAML dependency is part of the product.
package cicheck

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"regexp"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/wedevwork/callsheet/internal/devcheck"
)

// Job is the contract for one required CI job.
type Job struct {
	ID             string
	Name           string // display name and required check context
	RunsOn         string
	TimeoutMinutes int
	Stages         []string // devcheck stages, one check step each, in order
}

// jobs is the fixed four-job contract; order is the documentation order.
// The main jobs keep their verification stages; stress runs in two
// independent jobs of its own (iteration 02b), one per platform, at the
// same count: a Linux-only stress pass cannot qualify Darwin. The stress
// jobs' 20 minutes allow five minutes of setup beyond the 15-minute stress
// watchdog. Jobs never depend on each other: the strict field grammar
// rejects needs, conditions, matrices and continue-on-error.
var jobs = []Job{
	{ID: "linux", Name: "ci-linux", RunsOn: "ubuntu-24.04", TimeoutMinutes: 45, Stages: []string{"test", "coverage", "bench", "cross"}},
	{ID: "macos", Name: "ci-macos", RunsOn: "macos-15", TimeoutMinutes: 30, Stages: []string{"native"}},
	{ID: "linux-stress", Name: "ci-linux-stress", RunsOn: "ubuntu-24.04", TimeoutMinutes: 20, Stages: []string{"stress"}},
	{ID: "macos-stress", Name: "ci-macos-stress", RunsOn: "macos-15", TimeoutMinutes: 20, Stages: []string{"stress"}},
}

// Jobs returns a fresh copy of the required job contract.
func Jobs() []Job {
	out := make([]Job, len(jobs))
	for i, j := range jobs {
		j.Stages = append([]string(nil), j.Stages...)
		out[i] = j
	}
	return out
}

// RequiredChecks returns the required status check contexts in order.
func RequiredChecks() []string {
	var out []string
	for _, j := range jobs {
		out = append(out, j.Name)
	}
	return out
}

// Allowed action identities, pinned by full commit SHA.
const (
	CheckoutAction = "actions/checkout"
	SetupGoAction  = "actions/setup-go"
)

var (
	shaRE   = regexp.MustCompile(`^[0-9a-f]{40}$`)
	usesRE  = regexp.MustCompile(`^([A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+)@(.*)$`)
	tokenRE = regexp.MustCompile(`^[A-Za-z0-9_./=-]+$`)
)

// ValidationError lists every contract violation, each prefixed with the
// path of the offending field (e.g. "jobs.macos.steps[3].run").
type ValidationError struct {
	Problems []string
}

func (e *ValidationError) Error() string {
	return "cicheck: workflow contract violated:\n  " + strings.Join(e.Problems, "\n  ")
}

type validator struct {
	problems []string
	stages   map[string]bool
}

func (v *validator) addf(path, format string, args ...any) {
	if path == "" {
		path = "(root)"
	}
	v.problems = append(v.problems, path+": "+fmt.Sprintf(format, args...))
}

func join(path, key string) string {
	if path == "" {
		return key
	}
	return path + "." + key
}

func index(path string, i int) string { return fmt.Sprintf("%s[%d]", path, i) }

// parse decodes exactly one YAML document and returns its root node.
func parse(data []byte) (*yaml.Node, error) {
	dec := yaml.NewDecoder(bytes.NewReader(data))
	var doc yaml.Node
	if err := dec.Decode(&doc); err != nil {
		if errors.Is(err, io.EOF) {
			return nil, errors.New("cicheck: empty workflow")
		}
		return nil, fmt.Errorf("cicheck: malformed YAML: %v", err)
	}
	var extra yaml.Node
	if err := dec.Decode(&extra); err == nil {
		return nil, errors.New("cicheck: extra YAML document after the workflow")
	} else if !errors.Is(err, io.EOF) {
		return nil, fmt.Errorf("cicheck: malformed YAML: %v", err)
	}
	if doc.Kind != yaml.DocumentNode || len(doc.Content) != 1 {
		return nil, errors.New("cicheck: workflow is not a single YAML document")
	}
	return doc.Content[0], nil
}

// ValidateWorkflow checks data against the full CI workflow contract:
// triggers, names, runners, timeouts, permissions, setup steps and inputs,
// pinned action identities, shells, environments and the exact devcheck
// command sequence of each job, whose stages must exist in devcheck's
// dispatch. It rejects duplicate keys, anchors, aliases, merge keys,
// expressions, extra documents and every field outside the contract.
func ValidateWorkflow(data []byte) error {
	root, err := parse(data)
	if err != nil {
		return err
	}
	v := &validator{stages: map[string]bool{}}
	for _, s := range devcheck.Stages() {
		v.stages[s] = true
	}
	v.hygiene(root, "")
	v.workflow(root)
	if len(v.problems) > 0 {
		return &ValidationError{Problems: v.problems}
	}
	return nil
}

// hygiene rejects YAML features and values the contract never needs.
func (v *validator) hygiene(n *yaml.Node, path string) {
	if n.Anchor != "" {
		v.addf(path, "YAML anchors are not allowed")
	}
	switch n.Kind {
	case yaml.AliasNode:
		v.addf(path, "YAML aliases are not allowed")
	case yaml.ScalarNode:
		if strings.Contains(n.Value, "${{") {
			v.addf(path, "expressions are not allowed: %q", n.Value)
		}
	case yaml.SequenceNode:
		for i, c := range n.Content {
			v.hygiene(c, index(path, i))
		}
	case yaml.MappingNode:
		seen := map[string]bool{}
		for i := 0; i+1 < len(n.Content); i += 2 {
			k, val := n.Content[i], n.Content[i+1]
			if k.Kind != yaml.ScalarNode {
				v.addf(path, "non-scalar mapping key")
				continue
			}
			p := join(path, k.Value)
			switch {
			case k.Tag == "!!merge" || k.Value == "<<":
				v.addf(p, "YAML merge keys are not allowed")
			case seen[k.Value]:
				v.addf(p, "duplicate key")
			}
			seen[k.Value] = true
			if strings.Contains(k.Value, "${{") {
				v.addf(p, "expressions are not allowed in keys")
			}
			v.hygiene(val, p)
		}
	}
}

// fields checks that n is a mapping holding every required key, only
// required or optional keys, and returns the values by key.
func (v *validator) fields(n *yaml.Node, path string, required, optional []string) map[string]*yaml.Node {
	if n.Kind != yaml.MappingNode {
		v.addf(path, "must be a mapping")
		return nil
	}
	allowed := map[string]bool{}
	for _, k := range append(append([]string(nil), required...), optional...) {
		allowed[k] = true
	}
	out := map[string]*yaml.Node{}
	for i := 0; i+1 < len(n.Content); i += 2 {
		k := n.Content[i].Value
		if !allowed[k] {
			v.addf(join(path, k), "unknown field")
			continue
		}
		if _, dup := out[k]; !dup {
			out[k] = n.Content[i+1]
		}
	}
	for _, k := range required {
		if _, ok := out[k]; !ok {
			v.addf(join(path, k), "missing required field")
		}
	}
	return out
}

// scalar checks a scalar's value and, when tag is non-empty, its YAML type.
func (v *validator) scalar(n *yaml.Node, path, want, tag string) {
	if n == nil {
		return
	}
	if n.Kind != yaml.ScalarNode {
		v.addf(path, "must be %q", want)
		return
	}
	if n.Value != want || (tag != "" && n.Tag != tag) {
		v.addf(path, "must be %q, got %q", want, n.Value)
	}
}

// exactMap checks a mapping of scalars against want exactly.
func (v *validator) exactMap(n *yaml.Node, path string, want map[string]string, tags map[string]string) {
	if n == nil {
		return
	}
	keys := make([]string, 0, len(want))
	for k := range want {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	got := v.fields(n, path, keys, nil)
	for _, k := range keys {
		v.scalar(got[k], join(path, k), want[k], tags[k])
	}
}

func (v *validator) workflow(root *yaml.Node) {
	top := v.fields(root, "", []string{"name", "on", "permissions", "jobs"}, nil)
	if top == nil {
		return
	}
	v.scalar(top["name"], "name", "CI", "")
	if on := top["on"]; on != nil {
		events := v.fields(on, "on", []string{"push", "pull_request"}, nil)
		for _, e := range []string{"push", "pull_request"} {
			if n := events[e]; n != nil {
				f := v.fields(n, join("on", e), []string{"branches"}, nil)
				v.mainOnly(f["branches"], join("on", e)+".branches")
			}
		}
	}
	v.exactMap(top["permissions"], "permissions", map[string]string{"contents": "read"}, nil)
	if js := top["jobs"]; js != nil {
		var ids []string
		for _, j := range jobs {
			ids = append(ids, j.ID)
		}
		got := v.fields(js, "jobs", ids, nil)
		for _, j := range jobs {
			if n := got[j.ID]; n != nil {
				v.job(n, "jobs."+j.ID, j)
			}
		}
	}
}

func (v *validator) mainOnly(n *yaml.Node, path string) {
	if n == nil {
		return
	}
	if n.Kind != yaml.SequenceNode || len(n.Content) != 1 {
		v.addf(path, "must be exactly [main]")
		return
	}
	v.scalar(n.Content[0], index(path, 0), "main", "")
}

func (v *validator) job(n *yaml.Node, path string, j Job) {
	f := v.fields(n, path, []string{"name", "runs-on", "timeout-minutes", "defaults", "env", "steps"}, nil)
	if f == nil {
		return
	}
	v.scalar(f["name"], join(path, "name"), j.Name, "")
	v.scalar(f["runs-on"], join(path, "runs-on"), j.RunsOn, "")
	v.scalar(f["timeout-minutes"], join(path, "timeout-minutes"), fmt.Sprint(j.TimeoutMinutes), "!!int")
	if d := f["defaults"]; d != nil {
		df := v.fields(d, join(path, "defaults"), []string{"run"}, nil)
		v.exactMap(df["run"], join(path, "defaults.run"), map[string]string{"shell": "bash"}, nil)
	}
	v.exactMap(f["env"], join(path, "env"), map[string]string{"GOTOOLCHAIN": "local", "GOFLAGS": "-mod=readonly"}, nil)
	steps := f["steps"]
	if steps == nil {
		return
	}
	sp := join(path, "steps")
	if steps.Kind != yaml.SequenceNode {
		v.addf(sp, "must be a sequence")
		return
	}
	want := 3 + len(j.Stages)
	if len(steps.Content) != want {
		v.addf(sp, "must have exactly %d steps (checkout, setup-go, go mod download, then devcheck %s), got %d",
			want, strings.Join(j.Stages, ", "), len(steps.Content))
	}
	present := map[string]int{}
	for i, st := range steps.Content {
		p := index(sp, i)
		switch {
		case i == 0:
			v.action(st, p, CheckoutAction, map[string]string{"persist-credentials": "false"}, map[string]string{"persist-credentials": "!!bool"})
		case i == 1:
			v.action(st, p, SetupGoAction,
				map[string]string{"go-version-file": "go.mod", "cache": "true", "cache-dependency-path": "go.sum", "check-latest": "false"},
				map[string]string{"cache": "!!bool", "check-latest": "!!bool"})
		case i == 2:
			v.download(st, p)
		case i-3 < len(j.Stages):
			present[v.check(st, p, j.Stages[i-3])]++
		default:
			v.addf(p, "unexpected extra step")
		}
	}
	// Name the contract stages no remaining step invokes (reordering or
	// replacing a step is reported per step above, not here).
	for _, stage := range j.Stages {
		if present[stage] > 0 {
			present[stage]--
			continue
		}
		v.addf(sp, "missing check step for devcheck stage %q", stage)
	}
}

func (v *validator) stepName(f map[string]*yaml.Node, path string) {
	if n, ok := f["name"]; ok && (n.Kind != yaml.ScalarNode || n.Tag != "!!str" || strings.TrimSpace(n.Value) == "") {
		v.addf(join(path, "name"), "must be a nonempty string")
	}
}

func (v *validator) action(n *yaml.Node, path, identity string, with, tags map[string]string) {
	f := v.fields(n, path, []string{"uses", "with"}, []string{"name"})
	if f == nil {
		return
	}
	v.stepName(f, path)
	if u := f["uses"]; u != nil {
		up := join(path, "uses")
		m := usesRE.FindStringSubmatch(u.Value)
		switch {
		case u.Kind != yaml.ScalarNode || m == nil:
			v.addf(up, "must be %s@<40-hex commit SHA>, got %q", identity, u.Value)
		case m[1] != identity:
			v.addf(up, "must use action %s, got %q", identity, m[1])
		case !shaRE.MatchString(m[2]):
			v.addf(up, "must pin a full 40-hex commit SHA, got %q", m[2])
		}
	}
	v.exactMap(f["with"], join(path, "with"), with, tags)
}

func (v *validator) download(n *yaml.Node, path string) {
	f := v.fields(n, path, []string{"run"}, []string{"name"})
	if f == nil {
		return
	}
	v.stepName(f, path)
	if fields, ok := v.command(f["run"], join(path, "run")); ok && strings.Join(fields, " ") != "go mod download" {
		v.addf(join(path, "run"), "must be \"go mod download\", got %q", f["run"].Value)
	}
}

// check validates a devcheck check step expecting stage and returns the
// stage token its command invokes ("" if it is not a devcheck invocation).
func (v *validator) check(n *yaml.Node, path, stage string) string {
	f := v.fields(n, path, []string{"run", "env"}, []string{"name"})
	if f == nil {
		return ""
	}
	v.stepName(f, path)
	v.exactMap(f["env"], join(path, "env"), map[string]string{"GOPROXY": "off", "GOSUMDB": "off"}, nil)
	rp := join(path, "run")
	fields, ok := v.command(f["run"], rp)
	if !ok {
		return ""
	}
	if len(fields) != 4 || fields[0] != "go" || fields[1] != "run" || fields[2] != "./cmd/devcheck" {
		v.addf(rp, "must be \"go run ./cmd/devcheck %s\", got %q", stage, f["run"].Value)
		return ""
	}
	switch got := fields[3]; {
	case !v.stages[got]:
		v.addf(rp, "unknown devcheck stage %q (dispatch has: %s)", got, strings.Join(devcheck.Stages(), ", "))
	case got != stage:
		v.addf(rp, "must run devcheck stage %q, got %q", stage, got)
	}
	return fields[3]
}

// command applies the literal single-command grammar: one line of plain
// tokens, no shell operators, quoting or interpolation.
func (v *validator) command(n *yaml.Node, path string) ([]string, bool) {
	if n == nil {
		return nil, false
	}
	if n.Kind != yaml.ScalarNode {
		v.addf(path, "must be a single command string")
		return nil, false
	}
	line := strings.TrimSuffix(n.Value, "\n")
	if strings.ContainsAny(line, "\r\n") {
		v.addf(path, "must be a single command line, got %q", n.Value)
		return nil, false
	}
	fields := strings.Fields(line)
	if len(fields) == 0 {
		v.addf(path, "empty command")
		return nil, false
	}
	for _, tok := range fields {
		if !tokenRE.MatchString(tok) {
			v.addf(path, "token %q contains a shell operator, quoting or interpolation", tok)
			return nil, false
		}
	}
	return fields, true
}

// ExtractStages returns the devcheck stage tokens each job's run steps
// invoke, in order, without judging the rest of the contract.
func ExtractStages(data []byte) (map[string][]string, error) {
	root, err := parse(data)
	if err != nil {
		return nil, err
	}
	out := map[string][]string{}
	js := child(root, "jobs")
	if js == nil || js.Kind != yaml.MappingNode {
		return nil, errors.New("cicheck: workflow has no jobs mapping")
	}
	for i := 0; i+1 < len(js.Content); i += 2 {
		id := js.Content[i].Value
		out[id] = []string{}
		steps := child(js.Content[i+1], "steps")
		if steps == nil || steps.Kind != yaml.SequenceNode {
			continue
		}
		for _, st := range steps.Content {
			run := child(st, "run")
			if run == nil || run.Kind != yaml.ScalarNode {
				continue
			}
			f := strings.Fields(run.Value)
			if len(f) >= 4 && f[0] == "go" && f[1] == "run" && f[2] == "./cmd/devcheck" {
				out[id] = append(out[id], f[3])
			}
		}
	}
	return out, nil
}

func child(n *yaml.Node, key string) *yaml.Node {
	if n == nil || n.Kind != yaml.MappingNode {
		return nil
	}
	for i := 0; i+1 < len(n.Content); i += 2 {
		if n.Content[i].Value == key {
			return n.Content[i+1]
		}
	}
	return nil
}

// CheckJobNames scans every repository workflow (file name to content) and
// rejects duplicate job display names across them (a job without a name is
// displayed by its ID) and required checks that are missing.
func CheckJobNames(workflows map[string][]byte) error {
	files := make([]string, 0, len(workflows))
	for f := range workflows {
		files = append(files, f)
	}
	sort.Strings(files)
	seen := map[string]string{}
	var problems []string
	for _, file := range files {
		root, err := parse(workflows[file])
		if err != nil {
			problems = append(problems, file+": "+err.Error())
			continue
		}
		js := child(root, "jobs")
		if js == nil || js.Kind != yaml.MappingNode {
			problems = append(problems, file+": no jobs mapping")
			continue
		}
		for i := 0; i+1 < len(js.Content); i += 2 {
			id := js.Content[i].Value
			name := id
			if n := child(js.Content[i+1], "name"); n != nil && n.Kind == yaml.ScalarNode {
				name = n.Value
			}
			where := file + " jobs." + id
			if prev, dup := seen[name]; dup {
				problems = append(problems, fmt.Sprintf("job name %q is used by both %s and %s", name, prev, where))
				continue
			}
			seen[name] = where
		}
	}
	for _, c := range RequiredChecks() {
		if _, ok := seen[c]; !ok {
			problems = append(problems, fmt.Sprintf("required check %q is not defined by any workflow", c))
		}
	}
	if len(problems) > 0 {
		return &ValidationError{Problems: problems}
	}
	return nil
}
