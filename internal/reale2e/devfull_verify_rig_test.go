package reale2e

import (
	"bytes"
	"encoding/json"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	git "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/filemode"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/go-git/go-git/v5/storage/memory"
)

// Test helpers of design 12a-dev-flow-verify: a synthesized mode: set
// profile for any main.go, the scripted Codex command-execution log, the
// verification helper's scripted processes and a pinned reviewer checkout.

// synthProfile is a mode: set profile of main.go with one block per
// function body (main's block unexecuted, as a table test leaves it).
func synthProfile(module string, main []byte) []byte {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "main.go", main, 0)
	var b bytes.Buffer
	b.WriteString("mode: set\n")
	if err != nil {
		return b.Bytes()
	}
	for _, d := range f.Decls {
		fn, ok := d.(*ast.FuncDecl)
		if !ok || fn.Body == nil {
			continue
		}
		l, r := fset.Position(fn.Body.Lbrace), fset.Position(fn.Body.Rbrace)
		count := 1
		if fn.Name.Name == "main" {
			count = 0
		}
		if len(fn.Body.List) == 0 {
			fmt.Fprintf(&b, "%s/main.go:%d.%d,%d.%d 0 %d\n", module, r.Line, r.Column, r.Line, r.Column+1, count)
			continue
		}
		fmt.Fprintf(&b, "%s/main.go:%d.%d,%d.%d %d %d\n", module, l.Line, l.Column+1, r.Line, r.Column, len(fn.Body.List), count)
	}
	return b.Bytes()
}

// codexRecord is one Codex JSON line.
func codexRecord(v map[string]any) string {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return string(b) + "\n"
}

// codexLog is a reviewer task's merged log: the vendor's top-level
// records around one command execution of command.
func codexLog(command, output string, exit int) []byte {
	var b strings.Builder
	b.WriteString(codexRecord(map[string]any{"type": "thread.started", "thread_id": "0198"}))
	b.WriteString(codexRecord(map[string]any{"type": "turn.started"}))
	b.WriteString(codexRecord(map[string]any{"type": "item.completed", "item": map[string]any{"id": "item_0", "type": "agent_message", "text": "I will run the verification helper."}}))
	b.WriteString(codexRecord(map[string]any{"type": "item.started", "item": map[string]any{"id": "item_1", "type": "command_execution", "command": command,
		"aggregated_output": "", "exit_code": nil, "status": "in_progress"}}))
	b.WriteString(codexRecord(map[string]any{"type": "item.completed", "item": map[string]any{"id": "item_1", "type": "command_execution", "command": command,
		"aggregated_output": output, "exit_code": exit, "status": "completed"}}))
	b.WriteString(codexRecord(map[string]any{"type": "item.completed", "item": map[string]any{"id": "item_2", "type": "agent_message", "text": "Reviewed."}}))
	b.WriteString(codexRecord(map[string]any{"type": "turn.completed", "usage": map[string]any{"input_tokens": 10, "output_tokens": 5}}))
	return []byte(b.String())
}

// reviewScript is the helper's scripted Go: version, the two test
// commands (the coverage run writes a synthesized profile of the exported
// main.go) and a passing go test -json stream. profile and jsonStream
// override the defaults when set.
type reviewScript struct {
	profile    func(module string, main []byte) []byte
	jsonStream string
	testExit   int
}

func (rs reviewScript) script(s ProcSpec) behavior {
	a := strings.Join(s.Args, " ")
	switch {
	case a == "version":
		return behavior{stdout: "go version go1.26.4 linux/amd64\n"}
	case a == "test ./...":
		return behavior{stdout: "ok  \texample.com/greeting\t0.01s\n", exit: rs.testExit}
	case len(s.Args) == 3 && s.Args[0] == "test" && strings.HasPrefix(s.Args[1], "-coverprofile=") && s.Args[2] == "./...":
		out, dir := strings.TrimPrefix(s.Args[1], "-coverprofile="), s.Dir
		return behavior{stdout: "ok  \texample.com/greeting\t0.01s\tcoverage: 88.9% of statements\n", after: func() {
			main, _ := os.ReadFile(filepath.Join(dir, "main.go"))
			gomod, _ := os.ReadFile(filepath.Join(dir, "go.mod"))
			p := synthProfile(modulePathOf(gomod), main)
			if rs.profile != nil {
				p = rs.profile(modulePathOf(gomod), main)
			}
			os.WriteFile(out, p, 0o600)
		}}
	case a == "test -json -count=1 ./...":
		stream := rs.jsonStream
		if stream == "" {
			stream = passingTestJSON()
		}
		return behavior{stdout: stream}
	}
	return behavior{exit: 127, stderr: "unexpected command " + a + "\n"}
}

// passingTestJSON is a go test -json stream with the ten required
// subtests passing.
func passingTestJSON() string {
	var b strings.Builder
	b.WriteString(`{"Action":"start","Package":"example.com/greeting"}` + "\n")
	b.WriteString(`{"Action":"run","Package":"example.com/greeting","Test":"TestRun"}` + "\n")
	for _, c := range FeatureCases {
		fmt.Fprintf(&b, `{"Action":"run","Package":"example.com/greeting","Test":"TestRun/%s"}`+"\n", c.ID)
		fmt.Fprintf(&b, `{"Action":"pass","Package":"example.com/greeting","Test":"TestRun/%s","Elapsed":0}`+"\n", c.ID)
	}
	b.WriteString(`{"Action":"pass","Package":"example.com/greeting","Test":"TestRun","Elapsed":0}` + "\n")
	b.WriteString(`{"Action":"pass","Package":"example.com/greeting","Elapsed":0.01}` + "\n")
	return b.String()
}

// pinReviewCheckout creates a reviewer task checkout at dir: base's files in
// the worktree and a repository holding base's objects (copied from hub)
// with HEAD detached at base.
func pinReviewCheckout(t testing.TB, dir string, hub *memory.Storage, base plumbing.Hash, files map[string][]byte) {
	t.Helper()
	r, err := git.PlainInit(dir, false)
	if err != nil {
		t.Fatal(err)
	}
	if err := copyObjects(hub, r.Storer, []plumbing.Hash{base}); err != nil {
		t.Fatal(err)
	}
	if err := r.Storer.SetReference(plumbing.NewHashReference(plumbing.HEAD, base)); err != nil {
		t.Fatal(err)
	}
	for p, b := range files {
		if err := os.WriteFile(filepath.Join(dir, p), b, 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

// verifyHelperRun runs the verification helper in-process in env with argv
// (argv[0] the reale2e path) and returns its stdout, stderr and exit.
func verifyHelperRun(env reviewEnv, argv []string) (string, string, int) {
	var out, errb bytes.Buffer
	code := MainWith(Host{Args: argv[1:], Stdout: &out, Stderr: &errb, review: &env})
	return out.String(), errb.String(), code
}

// stubFile writes an executable-shaped stub (its bytes are hashed, never
// run) and returns its path.
func stubFile(t testing.TB, dir, name string) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte("stub "+name+"\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	return p
}

// The shared immutable synthetic passing bundles (design 12a-dev-flow-
// verify, Testing plan): complete offline attempts with a genuine
// changes-requested review and its fix loop, produced once per test binary
// by the live collector and copied for every mutation.

// devLoopSteps is the shared bundle's workflow: a design and a code loop
// through review approval.
func devLoopSteps() []devAct {
	return []devAct{{marker: markerDesignDraft}, {marker: markerDesignApproved}, {marker: markerImplComplete}, {marker: markerReviewChanges},
		{marker: markerFixesComplete}, {marker: markerReviewApproved}}
}

// devAttempt is one cached complete synthetic attempt.
type devAttempt struct {
	sync.Once
	dir string
	err error
}

// The shared passing attempts by coordinator inspection mode (design
// 12a-dev-flow-verify r0.6: workspace inspection is optional; calls made
// are bound): the shared bundle inspects every published result with both
// workspace forms; the others make no inspection call, or stop one
// enumeration after its first page.
var devPassingAttempts = map[string]*devAttempt{inspectFull: {}, inspectNone: {}, inspectPartial: {}}

// devPassingBundle returns a fresh copy of the cached passing bundle
// (full optional inspection).
func devPassingBundle(t *testing.T) string {
	t.Helper()
	return devPassingBundleOf(t, inspectFull)
}

// devPassingBundleOf returns a fresh copy of the cached passing attempt
// with the given inspection mode.
func devPassingBundleOf(t *testing.T, mode string) string {
	t.Helper()
	a := devPassingAttempts[mode]
	a.Do(func() {
		r := newDevRig(t, devLoopSteps())
		r.owner = []string{"yes 1"}
		r.inspection = mode
		if code := r.run(); code != 0 {
			a.err = fmt.Errorf("the shared attempt failed (%d): %s\n%s", code, r.d.stopCode, r.f.stderr.String())
			return
		}
		a.dir = filepath.Join(bundleCacheRoot, "dev-passing-"+cmpOr(mode, "none"))
		a.err = copyTree(r.f.bundle(), a.dir)
	})
	if a.err != nil {
		t.Fatal(a.err)
	}
	dst := filepath.Join(t.TempDir(), "bundle")
	if err := copyTree(a.dir, dst); err != nil {
		t.Fatal(err)
	}
	return dst
}

// devCheck runs the offline check of dir and decodes its report.
func devCheck(t testing.TB, dir string) (int, DevReport, string) {
	t.Helper()
	var out, errb bytes.Buffer
	code := CheckEvidence(dir, &out, &errb)
	var r DevReport
	if err := json.Unmarshal(out.Bytes(), &r); err != nil {
		t.Fatalf("check printed no report: %v\n%s", err, out.String())
	}
	return code, r, errb.String()
}

// devCodes is every failing code of a report, by criterion.
func devCodes(r DevReport) map[string][]string {
	out := map[string][]string{}
	for _, c := range r.Criteria {
		if len(c.Codes) > 0 {
			out[c.ID] = c.Codes
		}
	}
	return out
}

// devCriterion is a report's criterion id.
func devCriterion(r DevReport, id string) DevCriterion {
	for _, c := range r.Criteria {
		if c.ID == id {
			return c
		}
	}
	return DevCriterion{}
}

// reforge plays a capable bundle editor after a mutation: the manifest's
// inventory, then the report and audit, are recomputed from the mutated
// bytes, so only a contradiction with primary evidence can catch it. It
// returns the verdict of that recomputation: the exit and report an
// offline check of the reforged bundle gives (its derived files are the
// canonical ones by construction; the forgery matrix runs that check).
func reforge(t testing.TB, dir string) (int, DevReport) {
	t.Helper()
	for _, n := range []string{"report.json", "report.txt", "audit.json", "audit.md"} {
		os.Remove(filepath.Join(dir, n))
	}
	inv, err := diskInventory(dir)
	if err != nil {
		t.Fatal(err)
	}
	editJSON(t, dir, devManifestFile, func(m map[string]any) {
		var raw []any
		b, _ := json.Marshal(inv)
		json.Unmarshal(b, &raw)
		m["inventory"] = raw
	})
	b, err := LoadDevBundle(dir)
	if err != nil {
		t.Fatalf("the forged bundle does not load: %v", err)
	}
	res := checkDevBundle(b, true)
	for name, data := range map[string][]byte{"report.json": res.reportJSON, "report.txt": []byte(res.reportText), "audit.json": res.auditJSON,
		"audit.md": []byte(res.auditText)} {
		if err := os.WriteFile(filepath.Join(dir, name), data, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if res.report.Result == ResultPass {
		return 0, res.report
	}
	return 1, res.report
}

// treeState is a directory's every path with its mode and digest.
func treeState(t *testing.T, dir string) map[string]string {
	t.Helper()
	out := map[string]string{}
	filepath.WalkDir(dir, func(p string, e fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		st, _ := os.Lstat(p)
		rel, _ := filepath.Rel(dir, p)
		v := st.Mode().String()
		if st.Mode().IsRegular() {
			b, _ := os.ReadFile(p)
			v += " " + sha256Hex(b)
		}
		out[rel] = v
		return nil
	})
	return out
}

// replaceAll rewrites every occurrence of each old string with its new one
// in every regular file of a bundle outside the repository's objects (a
// capable forger rewriting every stored record that names an identity).
func replaceAll(t *testing.T, dir string, pairs ...string) {
	t.Helper()
	objects := filepath.Join(dir, filepath.FromSlash(devRepoDir), "objects")
	err := filepath.WalkDir(dir, func(p string, e fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if e.IsDir() && p == objects {
			return fs.SkipDir
		}
		if !e.Type().IsRegular() {
			return nil
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		s := string(b)
		for i := 0; i+1 < len(pairs); i += 2 {
			s = strings.ReplaceAll(s, pairs[i], pairs[i+1])
		}
		if s == string(b) {
			return nil
		}
		st, _ := os.Lstat(p)
		os.Chmod(p, 0o600)
		if err := os.WriteFile(p, []byte(s), 0o600); err != nil {
			return err
		}
		return os.Chmod(p, st.Mode().Perm())
	})
	if err != nil {
		t.Fatal(err)
	}
}

// forgeTask replaces task ordinal's published result in a saved bundle with
// a commit of the same parent whose files edit returns (with optional
// modes), rewrites every stored record naming the old commit and tree to
// the new ones, drops the old commit's unreachable objects and reforges
// the inventory and derived summaries.
func forgeTask(t *testing.T, dir string, ordinal int, edit func(files map[string][]byte) map[string]filemode.FileMode) (int, DevReport) {
	t.Helper()
	repoDir := filepath.Join(dir, filepath.FromSlash(devRepoDir))
	repo, err := git.PlainOpen(repoDir)
	if err != nil {
		t.Fatal(err)
	}
	ref, err := repo.Reference(plumbing.ReferenceName(fmt.Sprintf("refs/heads/task-%02d", ordinal)), false)
	if err != nil {
		t.Fatal(err)
	}
	old, err := repo.CommitObject(ref.Hash())
	if err != nil {
		t.Fatal(err)
	}
	tf, err := readTree(repo, old, "forged")
	if err != nil {
		t.Fatal(err)
	}
	files := map[string][]byte{}
	for _, f := range tf {
		files[f.Path] = f.Content
	}
	modes := edit(files)
	mem := memory.NewStorage()
	if err := copyObjects(repo.Storer, mem, old.ParentHashes); err != nil {
		t.Fatal(err)
	}
	var h, th plumbing.Hash
	if modes != nil {
		h, th = writeCommitMode(t, mem, files, modes, old.ParentHashes...)
	} else {
		h = writeNestedCommit(t, mem, files, old.ParentHashes[0])
		c, _ := object.GetCommit(mem, h)
		th = c.TreeHash
	}
	if err := copyObjects(mem, repo.Storer, []plumbing.Hash{h}); err != nil {
		t.Fatal(err)
	}
	replaceAll(t, dir, old.Hash.String(), h.String(), old.TreeHash.String(), th.String())
	pruneObjects(t, repoDir)
	return reforge(t, dir)
}

// pruneObjects deletes every loose object no ref reaches.
func pruneObjects(t *testing.T, repoDir string) {
	t.Helper()
	repo, err := git.PlainOpen(repoDir)
	if err != nil {
		t.Fatal(err)
	}
	var roots []plumbing.Hash
	iter, _ := repo.References()
	iter.ForEach(func(r *plumbing.Reference) error {
		if r.Type() == plumbing.HashReference {
			roots = append(roots, r.Hash())
		}
		return nil
	})
	keep, err := closure(repo.Storer, roots)
	if err != nil {
		t.Fatal(err)
	}
	live := map[string]bool{}
	for _, h := range keep {
		live[h.String()] = true
	}
	objects := filepath.Join(repoDir, "objects")
	filepath.WalkDir(objects, func(p string, e fs.DirEntry, err error) error {
		if err != nil || e.IsDir() {
			return nil
		}
		rel, _ := filepath.Rel(objects, p)
		if h := strings.ReplaceAll(filepath.ToSlash(rel), "/", ""); len(h) == 40 && !live[h] {
			os.Remove(p)
		}
		return nil
	})
}

// receiptLine is the helper stdout's first line: its JSON receipt
// (design 12a-dev-flow-verify r0.11: the report lines follow it).
func receiptLine(out string) string {
	l, _, _ := strings.Cut(out, "\n")
	return l
}
