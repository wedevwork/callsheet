package reale2e

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"unicode/utf8"

	"github.com/wedevwork/callsheet/internal/contract"
)

// Bundle loading (design 12a-real-e2e, Evidence and checker): the
// supplied root is resolved once with EvalSymlinks (an ancestor symlink
// such as macOS /var is fine), then the resolved root and every entry in
// it are Lstat-checked: no symlink and nothing but regular files and
// directories, only the fixed version 2 file names, each text file valid
// UTF-8 within 8 MiB, all text within 64 MiB and each A3 category within
// its budget, before any semantic parsing. Malformed or unsafe evidence,
// a version 1 bundle and (A3.6) a manifest, cleanup, snapshot or diff of
// an earlier version is a schema error (exit 2); missing evidence is a
// failed criterion (exit 1).

// Text bounds.
const (
	maxTextFile  = 8 << 20
	maxTextTotal = 64 << 20
)

// SchemaError is evidence that cannot be validated at all.
type SchemaError struct{ Err error }

func (e *SchemaError) Error() string { return "evidence schema error: " + e.Err.Error() }
func (e *SchemaError) Unwrap() error { return e.Err }

func schemaErr(format string, args ...any) error {
	return &SchemaError{Err: fmt.Errorf(format, args...)}
}

// manualFiles are the rendered role manuals kept in setup/manuals.
func manualFiles() []string {
	var out []string
	for _, h := range Hops {
		out = append(out, "setup/manuals/"+h+"-instruction.md", "setup/manuals/"+h+"-runbook.md")
	}
	return out
}

// bundleFiles is the fixed version 2 file set (repo.git aside).
func bundleFiles() map[string]bool {
	m := map[string]bool{}
	for _, f := range []string{"manifest.json", "events.jsonl", "owner-decision.json", "cleanup.json", "report.json", "report.txt",
		"audit.json", "audit.md", "audit-review.md",
		"setup/coordinator-prompt.md", "setup/" + PhasePrompt1, "setup/" + PhasePrompt2, "setup/flow.json", "setup/mcp-config.json",
		launchFiles[0], launchFiles[1], "setup/roles.json",
		phaseStreamFiles[0], phaseStreamFiles[1], phaseStderrFiles[0], phaseStderrFiles[1], transcriptFiles[0], transcriptFiles[1],
		eventMapFile, coordinatorExitFile,
		"workspace/tree-manifest.json",
		"validation/baseline.json", "validation/baseline-stdout.txt", "validation/baseline-stderr.txt",
		"validation/final-test.json", "validation/final-stdout.txt", "validation/final-stderr.txt",
		"validation/cases.json", "validation/tests.jsonl", "validation/test-json-stderr.txt", "validation/requirements.json",
		buildStdoutFile, buildStderrFile, "environment/diff.json"} {
		m[f] = true
	}
	for _, phase := range []string{"before", "after"} {
		for _, f := range envFiles(phase) {
			m[f] = true
		}
	}
	for _, f := range manualFiles() {
		m[f] = true
	}
	for i := range Hops {
		for _, f := range []string{"task.json", "final.txt"} {
			m["preflight/"+hopDir(i)+"/"+f] = true
		}
		for _, f := range []string{"task.json", "final.txt", "logs.txt", "logs.json", "dispatch.json", "wait.json", waitResultFile, waitStderrFile, waitExitFile} {
			m["hops/"+hopDir(i)+"/"+f] = true
		}
	}
	return m
}

// bundleDirs is the fixed directory set.
func bundleDirs() map[string]bool {
	m := map[string]bool{"setup": true, "setup/manuals": true, "preflight": true, "session": true, "hops": true, "workspace": true,
		"workspace/repo.git": true, "validation": true, "environment": true}
	for i := range Hops {
		m["preflight/"+hopDir(i)] = true
		m["hops/"+hopDir(i)] = true
	}
	return m
}

// taskEvidence is one stored task: its strictly decoded view and final
// answer file.
type taskEvidence struct {
	View  contract.TaskView
	Final []byte
	// HasFinal reports whether final.txt exists.
	HasFinal bool
}

// hopEvidence is one feature hop's stored evidence.
type hopEvidence struct {
	Task     *taskEvidence
	Dispatch *DispatchRecord
	Wait     *WaitRecord
	LogMeta  *LogMeta
	HasLogs  bool
}

// Bundle is a loaded, layout-checked and strictly parsed evidence bundle.
type Bundle struct {
	Root         string
	Files        map[string][]byte
	Manifest     *Manifest
	Launches     [2]*LaunchRecord
	Roles        *RolesRecord
	Flow         *Flow
	Preflight    [4]*taskEvidence
	Hops         [4]hopEvidence
	Decision     *DecisionRecord
	Events       []Event
	Trees        *TreeManifest
	Baseline     *TestRun
	Final        *TestRun
	Cleanup      *CleanupRecord
	CoordExit    *CoordinatorExit
	EventMap     *EventMap
	Diff         *EnvDiff
	Cases        *CasesRecord
	Requirements *RequirementsRecord
	Repo         *repoView
	RepoErr      error
}

// errV1Bundle is a completed version 1 bundle: unsupported, never
// reinterpreted as A3 proof.
var errV1Bundle = errors.New("a version 1 bundle is unsupported by the A3 checker")

// LoadBundle loads the bundle at root.
func LoadBundle(root string) (*Bundle, error) {
	resolved, err := filepath.EvalSymlinks(root)
	if err != nil {
		return nil, schemaErr("evidence root: %v", err)
	}
	st, err := os.Lstat(resolved)
	if err != nil || !st.IsDir() {
		return nil, schemaErr("evidence root is not a directory")
	}
	if data, err := readBounded(filepath.Join(resolved, "manifest.json"), maxTextFile); err == nil && bytes.Contains(data, []byte(`"`+ManifestSchemaV1+`"`)) {
		return nil, &SchemaError{Err: errV1Bundle}
	}
	b := &Bundle{Root: resolved, Files: map[string][]byte{}}
	if err := b.readFiles(); err != nil {
		return nil, err
	}
	if err := b.parse(); err != nil {
		return nil, err
	}
	if _, ok := b.Files["workspace/repo.git"]; ok {
		delete(b.Files, "workspace/repo.git")
		b.Repo, b.RepoErr = openEvidenceRepo(filepath.Join(resolved, "workspace", "repo.git"))
		if b.RepoErr != nil && !errors.Is(b.RepoErr, errObjectBound) && !errors.Is(b.RepoErr, errRepoSize) && !errors.Is(b.RepoErr, errRepoLayout) {
			// A corrupt or inconsistent repository is a failed criterion.
			return b, nil
		}
		if b.RepoErr != nil {
			return nil, &SchemaError{Err: b.RepoErr}
		}
	}
	return b, nil
}

// readFiles walks the resolved root with Lstat, enforcing the fixed
// layout and text bounds, and reads every file.
func (b *Bundle) readFiles() error {
	files, dirs := bundleFiles(), bundleDirs()
	var total, env, envBefore, envAfter int64
	return filepath.WalkDir(b.Root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return schemaErr("evidence walk: %v", err)
		}
		if p == b.Root {
			return nil
		}
		rel, err := filepath.Rel(b.Root, p)
		if err != nil || !utf8.ValidString(rel) || strings.HasPrefix(rel, "..") {
			return schemaErr("evidence path is invalid")
		}
		rel = filepath.ToSlash(rel)
		st, err := os.Lstat(p)
		if err != nil {
			return schemaErr("evidence %s: %v", rel, err)
		}
		switch {
		case st.Mode()&fs.ModeSymlink != 0:
			return schemaErr("evidence entry %s is a symlink", rel)
		case st.IsDir():
			if !dirs[rel] {
				return schemaErr("evidence directory %s is not part of the layout", rel)
			}
			if rel == "workspace/repo.git" {
				b.Files[rel] = nil
				return fs.SkipDir
			}
			return nil
		case !st.Mode().IsRegular():
			return schemaErr("evidence entry %s is not a regular file", rel)
		case !files[rel]:
			return schemaErr("evidence file %s is not part of the layout", rel)
		case st.Size() > maxTextFile:
			return schemaErr("evidence file %s exceeds 8 MiB", rel)
		}
		total += st.Size()
		if total > maxTextTotal {
			return schemaErr("evidence text exceeds 64 MiB")
		}
		// A3 category budgets: each snapshot shard 8 MiB and each snapshot
		// 16 MiB, the diff 4 MiB, the environment 36 MiB, the rest 28 MiB.
		switch {
		case rel == "environment/diff.json" && st.Size() > maxDiffBytes,
			isEnvironmentFile(rel) && st.Size() > maxShardBytes:
			return schemaErr("evidence file %s exceeds its environment bound", rel)
		}
		if isEnvironmentFile(rel) {
			env += st.Size()
			if strings.HasPrefix(rel, "environment/before-") {
				envBefore += st.Size()
			}
			if strings.HasPrefix(rel, "environment/after-") {
				envAfter += st.Size()
			}
		}
		if env > maxEnvironmentAll || envBefore > maxSnapshotBytes || envAfter > maxSnapshotBytes || total-env > maxOtherText {
			return schemaErr("evidence text exceeds a category budget")
		}
		data, err := readBounded(p, maxTextFile)
		if err != nil {
			return schemaErr("evidence %s: %v", rel, err)
		}
		if !utf8.Valid(data) {
			return schemaErr("evidence file %s is not valid UTF-8", rel)
		}
		b.Files[rel] = data
		return nil
	})
}

// readBounded reads at most limit bytes of path, failing beyond it.
func readBounded(path string, limit int64) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	// Sized from the file's current length (still bounded by the limit,
	// however much the read returns).
	var buf bytes.Buffer
	if st, err := f.Stat(); err == nil && st.Size() >= 0 && st.Size() <= limit {
		buf.Grow(int(st.Size()) + bytes.MinRead)
	}
	if _, err := buf.ReadFrom(io.LimitReader(f, limit+1)); err != nil {
		return nil, err
	}
	data := buf.Bytes()
	if int64(len(data)) > limit {
		return nil, errors.New("file exceeds its bound")
	}
	return data, nil
}

// doc strictly decodes the JSON file rel into v when it exists.
func (b *Bundle) doc(rel string, v any, schema func() string, want string) (bool, error) {
	data, ok := b.Files[rel]
	if !ok {
		return false, nil
	}
	// The schema identifier first: an earlier version (A3.6: manifest,
	// cleanup and diff v2 included) is unsupported before its shape is
	// judged.
	if got := peekSchema(data); got != "" && got != want {
		return true, schemaErr("%s: unsupported schema %q", rel, contract.SafeText(got, 64))
	}
	if err := decodeStrict(data, v); err != nil {
		return true, schemaErr("%s: %v", rel, err)
	}
	if got := schema(); got != want {
		return true, schemaErr("%s: unknown schema %q", rel, contract.SafeText(got, 64))
	}
	return true, nil
}

// peekSchema returns a JSON object's first top-level schema identifier,
// or "" when it has none (its strict decoding judges it then). It reads
// only up to that key, which the writers emit first.
func peekSchema(data []byte) string {
	d := json.NewDecoder(bytes.NewReader(data))
	if tok, err := d.Token(); err != nil || tok != json.Delim('{') {
		return ""
	}
	for d.More() {
		key, err := d.Token()
		if err != nil {
			return ""
		}
		var v json.RawMessage
		if err := d.Decode(&v); err != nil {
			return ""
		}
		if key == "schema" {
			var s string
			json.Unmarshal(v, &s)
			return s
		}
	}
	return ""
}

// snapshotSchemas refuses a snapshot whose header names another schema
// identifier (A3.6: snapshot v2 included).
func (b *Bundle) snapshotSchemas() error {
	for _, phase := range []string{"before", "after"} {
		name := envFiles(phase)[0]
		data, ok := b.Files[name]
		if !ok {
			continue
		}
		line, _, _ := bytes.Cut(data, []byte("\n"))
		if got := peekSchema(line); got != "" && got != SnapshotSchema {
			return schemaErr("%s: unsupported schema %q", name, contract.SafeText(got, 64))
		}
	}
	return nil
}

// task strictly decodes a stored task snapshot (the plane's task show
// response, unchanged).
func (b *Bundle) task(dir string) (*taskEvidence, error) {
	data, ok := b.Files[dir+"/task.json"]
	if !ok {
		return nil, nil
	}
	if err := checkDuplicateKeys(data); err != nil {
		return nil, schemaErr("%s/task.json: %v", dir, err)
	}
	v, err := contract.ParseTaskShowResponse(bytes.TrimSuffix(data, []byte("\n")))
	if err != nil {
		return nil, schemaErr("%s/task.json: %v", dir, err)
	}
	t := &taskEvidence{View: v}
	t.Final, t.HasFinal = b.Files[dir+"/final.txt"]
	return t, nil
}

// parse decodes every present document.
func (b *Bundle) parse() error {
	var errs []error
	add := func(_ bool, err error) { errs = append(errs, err) }
	b.Manifest, b.Launches, b.Roles = &Manifest{}, [2]*LaunchRecord{{}, {}}, &RolesRecord{}
	b.Decision, b.Trees, b.Baseline, b.Final = &DecisionRecord{}, &TreeManifest{}, &TestRun{}, &TestRun{}
	b.Cleanup, b.CoordExit, b.EventMap = &CleanupRecord{}, &CoordinatorExit{}, &EventMap{}
	b.Diff, b.Cases, b.Requirements = &EnvDiff{}, &CasesRecord{}, &RequirementsRecord{}
	add(b.doc("manifest.json", b.Manifest, func() string { return b.Manifest.Schema }, ManifestSchema))
	for i := range b.Launches {
		l := b.Launches[i]
		add(b.doc(launchFiles[i], l, func() string { return l.Schema }, LaunchSchema))
	}
	add(b.doc("setup/roles.json", b.Roles, func() string { return b.Roles.Schema }, RolesSchema))
	add(b.doc("owner-decision.json", b.Decision, func() string { return b.Decision.Schema }, DecisionSchema))
	add(b.doc("workspace/tree-manifest.json", b.Trees, func() string { return b.Trees.Schema }, TreeSchema))
	add(b.doc("validation/baseline.json", b.Baseline, func() string { return b.Baseline.Schema }, TestRunSchema))
	add(b.doc("validation/final-test.json", b.Final, func() string { return b.Final.Schema }, TestRunSchema))
	add(b.doc("cleanup.json", b.Cleanup, func() string { return b.Cleanup.Schema }, CleanupSchema))
	add(b.doc(coordinatorExitFile, b.CoordExit, func() string { return b.CoordExit.Schema }, CoordExitSchema))
	add(b.doc(eventMapFile, b.EventMap, func() string { return b.EventMap.Schema }, EventMapSchema))
	add(b.doc("environment/diff.json", b.Diff, func() string { return b.Diff.Schema }, DiffSchema))
	add(b.doc("validation/cases.json", b.Cases, func() string { return b.Cases.Schema }, CasesSchema))
	add(b.doc("validation/requirements.json", b.Requirements, func() string { return b.Requirements.Schema }, RequirementsSchema))
	errs = append(errs, b.snapshotSchemas())
	if err := errors.Join(errs...); err != nil {
		return err
	}
	// Absent documents are nil.
	for rel, p := range map[string]any{"manifest.json": &b.Manifest, launchFiles[0]: &b.Launches[0], launchFiles[1]: &b.Launches[1],
		"setup/roles.json": &b.Roles, "owner-decision.json": &b.Decision, "workspace/tree-manifest.json": &b.Trees,
		"validation/baseline.json": &b.Baseline, "validation/final-test.json": &b.Final, "cleanup.json": &b.Cleanup,
		coordinatorExitFile: &b.CoordExit, eventMapFile: &b.EventMap, "environment/diff.json": &b.Diff, "validation/cases.json": &b.Cases,
		"validation/requirements.json": &b.Requirements} {
		if _, ok := b.Files[rel]; !ok {
			clearPointer(p)
		}
	}
	if data, ok := b.Files["setup/flow.json"]; ok {
		f, err := ParseFlow(data)
		if err != nil {
			return schemaErr("setup/flow.json: %v", err)
		}
		b.Flow = &f
	}
	for i := range Hops {
		t, err := b.task("preflight/" + hopDir(i))
		if err != nil {
			return err
		}
		b.Preflight[i] = t
		dir := "hops/" + hopDir(i)
		if b.Hops[i].Task, err = b.task(dir); err != nil {
			return err
		}
		h := &b.Hops[i]
		h.Dispatch, h.Wait, h.LogMeta = &DispatchRecord{}, &WaitRecord{}, &LogMeta{}
		if ok, err := b.doc(dir+"/dispatch.json", h.Dispatch, func() string { return h.Dispatch.Schema }, DispatchSchema); err != nil {
			return err
		} else if !ok {
			h.Dispatch = nil
		}
		if ok, err := b.doc(dir+"/wait.json", h.Wait, func() string { return h.Wait.Schema }, WaitSchema); err != nil {
			return err
		} else if !ok {
			h.Wait = nil
		}
		if ok, err := b.doc(dir+"/logs.json", h.LogMeta, func() string { return h.LogMeta.Schema }, LogMetaSchema); err != nil {
			return err
		} else if !ok {
			h.LogMeta = nil
		}
		_, h.HasLogs = b.Files[dir+"/logs.txt"]
	}
	return b.parseEvents()
}

// clearPointer sets the pointer that p addresses to nil.
func clearPointer(p any) {
	switch q := p.(type) {
	case **Manifest:
		*q = nil
	case **LaunchRecord:
		*q = nil
	case **RolesRecord:
		*q = nil
	case **DecisionRecord:
		*q = nil
	case **TreeManifest:
		*q = nil
	case **TestRun:
		*q = nil
	case **CleanupRecord:
		*q = nil
	case **CoordinatorExit:
		*q = nil
	case **EventMap:
		*q = nil
	case **EnvDiff:
		*q = nil
	case **CasesRecord:
		*q = nil
	case **RequirementsRecord:
		*q = nil
	}
}

// parseEvents strictly decodes events.jsonl, one event per line.
func (b *Bundle) parseEvents() error {
	data, ok := b.Files["events.jsonl"]
	if !ok {
		return nil
	}
	for i, line := range strings.Split(strings.TrimSuffix(string(data), "\n"), "\n") {
		if line == "" && len(data) == 0 {
			break
		}
		var ev Event
		if err := decodeStrict([]byte(line), &ev); err != nil {
			return schemaErr("events.jsonl line %d: %v", i+1, err)
		}
		if !eventTypes[ev.Type] {
			return schemaErr("events.jsonl line %d: unknown event type %q", i+1, contract.SafeText(ev.Type, 32))
		}
		b.Events = append(b.Events, ev)
	}
	return nil
}

// transcriptLines splits the private transcript into its lines (without
// the final empty one).
func transcriptLines(data []byte) []string {
	s := strings.TrimSuffix(string(data), "\n")
	if s == "" {
		return nil
	}
	return strings.Split(s, "\n")
}
