package reale2e

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"
)

// Owner-environment tests (design 12a-real-e2e, amendment A3, FP-11,
// UT-10): every protected root and vendor home is a t.TempDir(); repositories
// are real Git repositories created and probed with the development Git
// (never a vendor CLI, never the owner's real homes).

// gitEnv is an isolated environment for test Git commands.
func gitEnv(t *testing.T) []string {
	return []string{"PATH=" + os.Getenv("PATH"), "HOME=" + t.TempDir(), "GIT_CONFIG_NOSYSTEM=1", "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@invalid",
		"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@invalid", "GIT_AUTHOR_DATE=2026-01-01T00:00:00Z", "GIT_COMMITTER_DATE=2026-01-01T00:00:00Z"}
}

// gitIn runs a test Git command in dir.
func gitIn(t *testing.T, dir string, args ...string) string {
	t.Helper()
	c := exec.Command("git", append([]string{"-c", "init.defaultBranch=main", "-c", "commit.gpgsign=false"}, args...)...)
	c.Dir, c.Env = dir, gitEnv(t)
	out, err := c.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return string(out)
}

// newRepo creates a repository with one committed file.
func newRepo(t *testing.T, dir string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	gitIn(t, dir, "init", "-q")
	os.WriteFile(filepath.Join(dir, "a.txt"), []byte("a\n"), 0o644)
	gitIn(t, dir, "add", "a.txt")
	gitIn(t, dir, "commit", "-q", "-m", "a")
}

// realGit is the live read-only Git runner, isolated from user config.
func realGit(t *testing.T) GitRunner {
	return execGit(ExecLauncher{}, realClock{}, exec.LookPath, []string{"PATH=" + os.Getenv("PATH"), "HOME=" + t.TempDir()})
}

// linkedParent returns dir reached through a symlink to its parent: the
// shape of macOS's /var -> /private/var above every t.TempDir.
func linkedParent(t *testing.T, dir string) string {
	t.Helper()
	link := filepath.Join(t.TempDir(), "l")
	if err := os.Symlink(filepath.Dir(dir), link); err != nil {
		t.Fatal(err)
	}
	return filepath.Join(link, filepath.Base(dir))
}

// snap takes one snapshot of home and roots.
func snap(t *testing.T, phase, home string, roots, exclude []string) *Snapshot {
	t.Helper()
	s := takeSnapshot(phase, home, roots, exclude, realGit(t), pathTokenizer{home: home}, realClock{}, time.Time{})
	if !s.Complete {
		t.Fatalf("%s snapshot incomplete: %s", phase, s.Code)
	}
	return s
}

// write writes a file, creating its parents.
func write(t *testing.T, p, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

// changeOf finds the change of path in cs.
func changeOf(cs []EnvChange, path string) (EnvChange, bool) {
	for _, c := range cs {
		if c.Path == path {
			return c, true
		}
	}
	return EnvChange{}, false
}

// TestOwnerEnvironment (FP-11): complete offline runs with t.TempDir()
// roots and vendor homes. Repeated --protect-root values are canonicalized,
// deduplicated and recorded identically in the manifest, both snapshots
// and diff.json; zero roots says no repository was protected; A3.6
// tool-maintained changes are summarized per vendor top-level directory
// and never fail; a protected configuration or repository change fails
// owner_environment_modified, even after an earlier task failure, and is
// never repaired; invalid roots fail before any executable runs.
func TestOwnerEnvironment(t *testing.T) {
	t.Parallel()
	t.Run("zero roots and tool-maintained state", func(t *testing.T) {
		t.Parallel()
		f := newFakeWorld(t)
		f.coord.afterPhase = func(c *coordScript, p int) {
			if p == 1 {
				// The vendors' own state during the run, including Claude
				// state whose content stands for changed tool grants and a
				// path no earlier revision knew.
				write(t, filepath.Join(f.home, ".claude", "history.jsonl"), `{"display":"old"}`+"\n"+`{"display":"new"}`+"\n")
				write(t, filepath.Join(f.home, ".claude", ".credentials.json"), `{"token":"refreshed!"}`+"\n")
				write(t, filepath.Join(f.home, ".claude", ".config.json"), `{"projects":{"/x":{"allowedTools":["Bash(*)"]}}}`+"\n")
				write(t, filepath.Join(f.home, ".claude", "backups", ".config.json.backup.1"), `{"projects":{}}`+"\n")
				write(t, filepath.Join(f.home, ".codex", "sessions", "2026", "10", "10", "rollout-2026-10-10T12-00-00-0f1e2d3c-4b5a-4968-8786-a5b4c3d2e1f0.jsonl"), "{}\n")
				os.Remove(filepath.Join(f.home, ".codex", "auth.json"))
				write(t, filepath.Join(f.home, ".grok", "logs", "grok-1.log"), "log\n")
				write(t, filepath.Join(f.home, ".cursor", "future-state", "x.db"), "db\n")
			}
		}
		if code := f.attempt(); code != 0 {
			t.Fatalf("attempt = %d %v\n%s", code, failedCodes(f.report()), f.stderr.String())
		}
		r := f.report()
		text := string(mustRead(t, filepath.Join(f.bundle(), "report.txt")))
		if len(r.ProtectedRoots) != 0 || len(r.Protected) != 0 || len(r.Housekeeping) != 0 || !strings.Contains(text, "No repository root was protected") ||
			!strings.Contains(text, strings.Join(ClaudeStateExclusions, "\n")+"\n") {
			t.Fatalf("report %+v\n%s", r, text)
		}
		var d EnvDiff
		readDoc(t, filepath.Join(f.bundle(), "environment", "diff.json"), &d)
		rows := map[string]ToolSummaryRow{}
		total := 0
		for _, row := range d.ToolSummary {
			rows[row.Scope+" "+row.TopLevel] = row
			total += row.Total
			if row.Total != row.Added+row.Changed+row.Deleted {
				t.Errorf("row %+v", row)
			}
		}
		for key, want := range map[string]ToolSummaryRow{
			"claude .":            {Scope: "claude", TopLevel: ".", Added: 1, Changed: 2, Total: 3},
			"claude backups":      {Scope: "claude", TopLevel: "backups", Added: 2, Total: 2},
			"codex .":             {Scope: "codex", TopLevel: ".", Deleted: 1, Total: 1},
			"codex sessions":      {Scope: "codex", TopLevel: "sessions", Added: 5, Total: 5},
			"grok logs":           {Scope: "grok", TopLevel: "logs", Added: 2, Total: 2},
			"cursor future-state": {Scope: "cursor", TopLevel: "future-state", Added: 2, Total: 2},
		} {
			if rows[key] != want {
				t.Errorf("summary %s = %+v, want %+v", key, rows[key], want)
			}
			if !strings.Contains(text, fmt.Sprintf("  tool-maintained %s: %d added, %d changed, %d deleted, %d total\n", key, want.Added, want.Changed, want.Deleted, want.Total)) {
				t.Errorf("report.txt lacks the %s row:\n%s", key, text)
			}
		}
		// The coordinator's own new transcript and its project directory.
		if rows["claude projects"].Added != 2 {
			t.Errorf("transcript summary %+v", rows["claude projects"])
		}
		if d.ToolDetailCount != total || d.ToolOmittedCount != 0 || len(d.Protected) != 0 ||
			!strings.Contains(text, fmt.Sprintf("tool-maintained changes (report-only): %d (%d detailed in diff.json, 0 omitted)\n", total, total)) {
			t.Fatalf("tool detail %d of %d\n%s", d.ToolDetailCount, total, text)
		}
		for _, dt := range d.Detail {
			for _, r := range []*EnvRecord{dt.Before, dt.After} {
				if r != nil && r.SHA256 != "" {
					t.Errorf("tool state content digest %s", r.Path)
				}
			}
		}
	})
	t.Run("supplied roots", func(t *testing.T) {
		t.Parallel()
		f := newFakeWorld(t)
		f.git = realGit(t)
		a, b := t.TempDir(), t.TempDir()
		newRepo(t, filepath.Join(a, "one"))
		newRepo(t, filepath.Join(a, "one", "nested"))
		gitIn(t, filepath.Join(b), "init", "-q", "--bare", "bare.git")
		newRepo(t, filepath.Join(a, "deeper", "two"))
		alias := filepath.Join(t.TempDir(), "alias")
		os.Symlink(a, alias)
		f.roots = []string{b, alias, a, a}
		f.depth = "1"
		if code := f.attempt(); code != 0 {
			t.Fatalf("attempt = %d %v\n%s", code, failedCodes(f.report()), f.stderr.String())
		}
		bl := f.bundle()
		ca, _ := filepath.EvalSymlinks(a)
		cb, _ := filepath.EvalSymlinks(b)
		want := []string{ca, cb}
		slices.Sort(want)
		var m Manifest
		readDoc(t, filepath.Join(bl, "manifest.json"), &m)
		before, after := readSnapshot(t, bl, "before"), readSnapshot(t, bl, "after")
		var d EnvDiff
		readDoc(t, filepath.Join(bl, "environment", "diff.json"), &d)
		if !slices.Equal(m.ProtectedRoots, want) || !slices.Equal(before.Roots, want) || !slices.Equal(after.Roots, want) || !slices.Equal(d.ProtectedRoots, want) {
			t.Fatalf("roots %v %v %v %v, want %v", m.ProtectedRoots, before.Roots, after.Roots, d.ProtectedRoots, want)
		}
		repos := 0
		for _, r := range before.Compact {
			if r.Kind == RecRepo {
				repos++
			}
		}
		if repos != 3 || m.ProtectDepth != 1 || before.Depth != 1 {
			t.Fatalf("%d repositories recorded at depth %d/%d", repos, m.ProtectDepth, before.Depth)
		}
		if text := string(mustRead(t, filepath.Join(bl, "report.txt"))); !strings.Contains(text, "at most 1 directory levels") {
			t.Fatalf("report.txt:\n%s", text)
		}
		if r := f.report(); r.Result != ResultPass || !slices.Equal(r.ProtectedRoots, want) {
			t.Fatalf("report %v", failedCodes(r))
		}
	})
	t.Run("canonical root order survives tokenization", func(t *testing.T) {
		t.Parallel()
		// C10: a sibling sorting before the home canonically comes first,
		// although its token sorts after "$HOME".
		f := newFakeWorld(t)
		f.git = realGit(t)
		sibling := filepath.Join(filepath.Dir(f.home), "000")
		if err := os.Mkdir(sibling, 0o700); err != nil {
			t.Fatal(err)
		}
		f.roots = []string{f.home, sibling}
		if code := f.attempt(); code != 0 {
			t.Fatalf("attempt = %d %v\n%s", code, failedCodes(f.report()), f.stderr.String())
		}
		cs, _ := filepath.EvalSymlinks(sibling)
		var m Manifest
		readDoc(t, filepath.Join(f.bundle(), "manifest.json"), &m)
		if !slices.Equal(m.ProtectedRoots, []string{cs, "$HOME"}) {
			t.Fatalf("manifest roots %v", m.ProtectedRoots)
		}
		if code, r, _ := checkBundle(t, f.bundle()); code != 0 || r.Result != ResultPass {
			t.Fatalf("check = %d %v", code, failedCodes(r))
		}
	})
	t.Run("home, checkout and roots through a symlinked parent", func(t *testing.T) {
		t.Parallel()
		// The macOS shape: $HOME, the working directory and the supplied
		// roots are spelled through a symlinked ancestor; every root and
		// vendor home is still tokenized from its canonical form.
		f := newFakeWorld(t)
		f.git = realGit(t)
		f.home = linkedParent(t, f.home)
		f.checkout = linkedParent(t, f.checkout)
		f.evidence = filepath.Join(f.checkout, "design", "real-e2e-runs", "attempt")
		sibling := filepath.Join(filepath.Dir(f.home), "000")
		if err := os.Mkdir(sibling, 0o700); err != nil {
			t.Fatal(err)
		}
		f.roots = []string{f.home, sibling, f.checkout}
		if code := f.attempt(); code != 0 {
			t.Fatalf("attempt = %d %v\n%s", code, failedCodes(f.report()), f.stderr.String())
		}
		ch, _ := filepath.EvalSymlinks(f.home)
		cc, _ := filepath.EvalSymlinks(f.checkout)
		cs, _ := filepath.EvalSymlinks(sibling)
		// Report redaction knows both spellings of the home and checkout.
		for in, out := range map[string]string{
			filepath.Join(ch, ".local", "bin", "claude"): "$HOME/.local/bin/claude", filepath.Join(f.home, ".local", "bin", "claude"): "$HOME/.local/bin/claude",
			filepath.Join(cc, "bin", "callsheet"): "$CHECKOUT/bin/callsheet", filepath.Join(f.checkout, "bin", "callsheet"): "$CHECKOUT/bin/callsheet",
		} {
			if got := f.sup.tok(in); got != out {
				t.Errorf("tok(%s) = %s, want %s", in, got, out)
			}
		}
		canon := []string{ch, cc, cs}
		slices.Sort(canon)
		var want []string
		for _, c := range canon {
			want = append(want, map[string]string{ch: "$HOME", cc: "$CHECKOUT", cs: cs}[c])
		}
		var m Manifest
		readDoc(t, filepath.Join(f.bundle(), "manifest.json"), &m)
		before, after := readSnapshot(t, f.bundle(), "before"), readSnapshot(t, f.bundle(), "after")
		var d EnvDiff
		readDoc(t, filepath.Join(f.bundle(), "environment", "diff.json"), &d)
		if !slices.Equal(m.ProtectedRoots, want) || !slices.Equal(before.Roots, want) || !slices.Equal(after.Roots, want) || !slices.Equal(d.ProtectedRoots, want) {
			t.Fatalf("roots %v %v %v %v, want %v", m.ProtectedRoots, before.Roots, after.Roots, d.ProtectedRoots, want)
		}
		if r := f.report(); r.Result != ResultPass {
			t.Fatalf("report %v", failedCodes(r))
		}
		if code, r, _ := checkBundle(t, f.bundle()); code != 0 || r.Result != ResultPass {
			t.Fatalf("check = %d %v", code, failedCodes(r))
		}
	})
	t.Run("protected changes are reported, never repaired", func(t *testing.T) {
		t.Parallel()
		f := newFakeWorld(t)
		f.git = realGit(t)
		root := t.TempDir()
		repo := filepath.Join(root, "work")
		newRepo(t, repo)
		f.roots = []string{root}
		f.coord.failHop = 0
		f.coord.before = func(c *coordScript) bool {
			if c.step == 1 {
				// A vendor (or anyone) changes owner configuration and a
				// repository while the attempt runs, and the designer fails.
				write(t, filepath.Join(f.home, ".claude", "settings.json"), `{"permissions":{"allow":["Bash(*)"]}}`+"\n")
				write(t, filepath.Join(repo, "a.txt"), "changed\n")
				write(t, filepath.Join(f.home, ".cursor", "cli-config.json"), "{}")
			}
			return false
		}
		if code := f.attempt(); code != 1 || f.sup.failCode != "hop_failed" {
			t.Fatalf("attempt = %d %q", code, f.sup.failCode)
		}
		r := f.report()
		if c := criterion(r, CritOwnerEnv); c.Result != "fail" || !slices.Contains(c.Codes, CodeEnvModified) {
			t.Fatalf("owner environment %+v", c)
		}
		for _, p := range []string{"settings.json", "cli-config.json"} {
			if _, ok := changeOf(r.Protected, p); !ok {
				t.Fatalf("%s not reported: %+v", p, r.Protected)
			}
		}
		crepo, _ := filepath.EvalSymlinks(repo)
		if c, ok := changeOf(r.Protected, crepo); !ok || !slices.Contains(c.Fields, "status") {
			t.Fatalf("repository change not reported: %+v", r.Protected)
		}
		if string(mustRead(t, filepath.Join(f.home, ".claude", "settings.json"))) != `{"permissions":{"allow":["Bash(*)"]}}`+"\n" ||
			string(mustRead(t, filepath.Join(repo, "a.txt"))) != "changed\n" {
			t.Fatal("the harness repaired an owner file")
		}
		if !strings.Contains(f.stdout.String(), "PROTECTED CHANGE") {
			t.Fatal("no protected change printed")
		}
	})
	t.Run("stored tool-maintained evidence", func(t *testing.T) {
		t.Parallel()
		// A3.6: the checker recomputes the summary and counts from the
		// compact inventories and accepts an omitted optional tool
		// detail; a corrupted summary, count or class fails.
		base := passingBundle(t)
		var stored EnvDiff
		readDoc(t, filepath.Join(base, "environment", "diff.json"), &stored)
		if stored.ToolDetailCount == 0 || len(stored.ToolSummary) == 0 {
			t.Fatalf("the passing run has no tool-maintained change: %+v", stored)
		}
		fresh := func() string {
			c := filepath.Join(t.TempDir(), "b")
			if err := copyTree(base, c); err != nil {
				t.Fatal(err)
			}
			return c
		}
		// All tool detail omitted: the environment criterion still
		// passes, and report and audit show the omission.
		b := fresh()
		n := stored.ToolDetailCount
		editDoc(t, b, "environment/diff.json", func(d *EnvDiff) {
			d.Detail = slices.DeleteFunc(d.Detail, func(e EnvDetail) bool { return toolKey(keyOf(*detailRecord(e))) })
			d.ToolDetailCount, d.ToolOmittedCount = 0, n
		})
		bl, err := LoadBundle(b)
		if err != nil {
			t.Fatal(err)
		}
		cs, ck := check(bl)
		r := BuildReport(bl, cs)
		if c := criterion(r, CritOwnerEnv); c.Result != "pass" {
			t.Fatalf("omitted tool detail: %+v", c)
		}
		if text := RenderText(r); !strings.Contains(text, fmt.Sprintf("tool-maintained changes (report-only): %d (0 detailed in diff.json, %d omitted)\n", n, n)) ||
			!strings.Contains(text, strings.Join(ClaudeStateExclusions, "\n")+"\n") {
			t.Fatalf("report.txt:\n%s", text)
		}
		for _, row := range auditRows(ck) {
			if row.ID == "owner_environment" && !strings.Contains(row.Observed, fmt.Sprintf("(report-only; 0 detailed, %d omitted)", n)) {
				t.Fatalf("audit row %q", row.Observed)
			}
		}
		for name, edit := range map[string]func(d *EnvDiff){
			"summary row added": func(d *EnvDiff) {
				d.ToolSummary = append(d.ToolSummary, ToolSummaryRow{Scope: "grok", TopLevel: "x", Added: 1, Total: 1})
			},
			"summary count":       func(d *EnvDiff) { d.ToolSummary[0].Total++ },
			"summary removed":     func(d *EnvDiff) { d.ToolSummary = []ToolSummaryRow{} },
			"omitted count":       func(d *EnvDiff) { d.ToolOmittedCount++ },
			"detail count":        func(d *EnvDiff) { d.ToolDetailCount-- },
			"detail class":        func(d *EnvDiff) { d.Detail[0].After.Class = ClassOwner },
			"detail digest":       func(d *EnvDiff) { d.Detail[0].After.MtimeNS++ },
			"tool content digest": func(d *EnvDiff) { d.Detail[0].After.SHA256 = zeros64 },
		} {
			b := fresh()
			editDoc(t, b, "environment/diff.json", edit)
			bl, err := LoadBundle(b)
			if err != nil {
				t.Fatalf("%s: %v", name, err)
			}
			if codes := newChecker(bl).methodCodes((*checker).ownerEnvironment); len(codes[CritOwnerEnv]) == 0 {
				t.Errorf("%s: %v", name, codes)
			}
		}
		// A compact record claiming another class than the derived one.
		b = fresh()
		p := filepath.Join(b, "environment", "after-00.jsonl")
		data := string(mustRead(t, p))
		if !strings.Contains(data, `"class":"tool-maintained"`) {
			t.Fatal("no tool-maintained compact record")
		}
		writeBundleFile(t, b, "environment/after-00.jsonl", []byte(strings.Replace(data, `"class":"tool-maintained"`, `"class":"owner-configuration"`, 1)))
		if _, r, _ := checkBundle(t, b); criterion(r, CritOwnerEnv).Result != "fail" {
			t.Errorf("a fabricated class: %v", failedCodes(r))
		}
		// Earlier schema versions are unsupported (exit 2), identified
		// before any semantic validation; the unchanged documents stay v2.
		for rel, schema := range map[string]string{"manifest.json": ManifestSchema, "cleanup.json": CleanupSchema, "environment/diff.json": DiffSchema,
			"environment/before-00.jsonl": SnapshotSchema, "environment/after-00.jsonl": SnapshotSchema} {
			b := fresh()
			v2 := strings.TrimSuffix(schema, "v3") + "v2"
			data := string(mustRead(t, filepath.Join(b, filepath.FromSlash(rel))))
			old := strings.Replace(data, `"`+schema+`"`, `"`+v2+`"`, 1)
			if old == data {
				t.Fatalf("%s carries no %s", rel, schema)
			}
			writeBundleFile(t, b, rel, []byte(old))
			if code, _, errOut := checkBundle(t, b); code != 2 || !strings.Contains(errOut, "unsupported schema") {
				t.Errorf("%s v2 = %d %q", rel, code, errOut)
			}
			if !strings.HasSuffix(rel, ".jsonl") {
				writeBundleFile(t, b, rel, []byte(`{"schema":"`+v2+`"}`+"\n"))
				if code, _, errOut := checkBundle(t, b); code != 2 || !strings.Contains(errOut, "unsupported schema") {
					t.Errorf("%s v2 without its fields = %d %q", rel, code, errOut)
				}
			}
		}
		for _, s := range []string{LaunchSchema, EventMapSchema, CoordExitSchema, ReportSchema} {
			if !strings.HasSuffix(s, "/v2") {
				t.Errorf("schema %s changed", s)
			}
		}
		for _, s := range []string{ManifestSchema, CleanupSchema, SnapshotSchema, DiffSchema} {
			if !strings.HasSuffix(s, "/v3") {
				t.Errorf("schema %s is not v3", s)
			}
		}
	})
	t.Run("created structural paths must be real directories", func(t *testing.T) {
		t.Parallel()
		// A3.6: a previously absent vendor home or structural ancestor may
		// appear only as a real directory. The supervisor's comparison and
		// the offline checker's recomputation both fail any other shape.
		external := t.TempDir()
		write(t, filepath.Join(external, "installed_plugins.json"), `{"plugins":{}}`)
		for name, tc := range map[string]struct {
			create func(home string) error
			path   string
			fail   bool
		}{
			"plugins symlink to external selection": {func(h string) error { return os.Symlink(external, filepath.Join(h, ".claude", "plugins")) }, "plugins", true},
			"plugins regular file": {func(h string) error {
				return os.WriteFile(filepath.Join(h, ".claude", "plugins"), []byte("x"), 0o600)
			}, "plugins", true},
			"cursor home dangling symlink": {func(h string) error { return os.Symlink("/nonexistent/cursor", filepath.Join(h, ".cursor")) }, "$HOME/.cursor", true},
			"plugins and cursor home directories": {func(h string) error {
				if err := os.MkdirAll(filepath.Join(h, ".claude", "plugins", "cache"), 0o700); err != nil {
					return err
				}
				return os.MkdirAll(filepath.Join(h, ".cursor", "chats"), 0o700)
			}, "", false},
		} {
			t.Run(name, func(t *testing.T) {
				t.Parallel()
				f := newFakeWorld(t)
				f.coord.afterPhase = func(c *coordScript, p int) {
					if p == 2 {
						if err := tc.create(f.home); err != nil {
							t.Error(err)
						}
					}
				}
				code := f.attempt()
				r := f.report()
				var d EnvDiff
				readDoc(t, filepath.Join(f.bundle(), "environment", "diff.json"), &d)
				cc, cr, _ := checkBundle(t, f.bundle())
				if !tc.fail {
					if code != 0 || len(r.Protected) != 0 || len(d.Protected) != 0 || cc != 0 || criterion(cr, CritOwnerEnv).Result != "pass" {
						t.Fatalf("attempt %d check %d %v protected %+v", code, cc, failedCodes(cr), d.Protected)
					}
					return
				}
				if c, ok := changeOf(d.Protected, tc.path); code != 1 || !ok || c.Kind != ChangeAdded || len(d.Protected) != 1 || !slices.Contains(d.Codes, CodeEnvModified) {
					t.Fatalf("supervisor: attempt %d protected %+v codes %v", code, d.Protected, d.Codes)
				}
				if _, ok := changeOf(r.Protected, tc.path); !ok || !slices.Contains(failedCodes(r), CodeEnvModified) {
					t.Fatalf("report %v %+v", failedCodes(r), r.Protected)
				}
				if c := criterion(cr, CritOwnerEnv); cc != 1 || c.Result != "fail" || !slices.Contains(c.Codes, CodeEnvModified) || slices.Contains(c.Codes, "environment_diff_mismatch") {
					t.Fatalf("checker: %d %+v", cc, c)
				}
			})
		}
		// The checker alone, over synthetic records consistently rebound
		// into a passing bundle: only directory creations pass.
		for name, tc := range map[string]struct {
			edit func(e *envEdit)
			fail bool
		}{
			"ancestor symlink": {func(e *envEdit) {
				e.put(e.after, envKey{RecEntry, "claude", "plugins"}, &EnvRecord{Kind: RecEntry, Scope: "claude", Path: "plugins", Class: ClassOwner,
					Method: MethodMetadata, Type: TypeSymlink, Mode: 0o777, Size: 9, Target: "/elsewhere"})
			}, true},
			"ancestor file": {func(e *envEdit) {
				e.put(e.after, envKey{RecEntry, "claude", "plugins"}, &EnvRecord{Kind: RecEntry, Scope: "claude", Path: "plugins", Class: ClassOwner,
					Method: MethodMetadata, Type: TypeFile, Mode: 0o600, Size: 1})
			}, true},
			"ancestor directory": {func(e *envEdit) {
				e.put(e.after, envKey{RecEntry, "claude", "plugins"}, &EnvRecord{Kind: RecEntry, Scope: "claude", Path: "plugins", Class: ClassOwner,
					Method: MethodIdentity, Type: TypeDir, Mode: 0o700, Dev: 1, Ino: 2})
			}, false},
			"home dangling symlink": {func(e *envEdit) {
				k := envKey{RecHome, "cursor", "$HOME/.cursor"}
				e.put(e.before, k, &EnvRecord{Kind: RecHome, Scope: "cursor", Path: "$HOME/.cursor", Type: TypeAbsent, Method: MethodAbsent})
				e.put(e.after, k, &EnvRecord{Kind: RecHome, Scope: "cursor", Path: "$HOME/.cursor", Method: MethodIdentity, Type: TypeSymlink, Mode: 0o777,
					Size: 9, Target: "/nonexistent"})
			}, true},
			"home directory": {func(e *envEdit) {
				k := envKey{RecHome, "cursor", "$HOME/.cursor"}
				e.put(e.before, k, &EnvRecord{Kind: RecHome, Scope: "cursor", Path: "$HOME/.cursor", Type: TypeAbsent, Method: MethodAbsent})
				e.put(e.after, k, &EnvRecord{Kind: RecHome, Scope: "cursor", Path: "$HOME/.cursor", Method: MethodIdentity, Type: TypeDir,
					Mode: 0o700, Dev: 1, Ino: 3})
			}, false},
		} {
			t.Run("checker "+name, func(t *testing.T) {
				t.Parallel()
				b := passingBundle(t)
				rewriteEnvironment(t, b, tc.edit)
				_, r, _ := checkBundle(t, b)
				if c := criterion(r, CritOwnerEnv); tc.fail != (c.Result == "fail") || tc.fail != slices.Contains(c.Codes, CodeEnvModified) {
					t.Fatalf("%s: %+v", name, c)
				}
			})
		}
	})
	t.Run("configuration deleted beside credentials", func(t *testing.T) {
		t.Parallel()
		// A3.6: a credential deletion is the tool's; the owner's
		// configuration deleted in the same run fails.
		f := newFakeWorld(t)
		f.coord.afterPhase = func(c *coordScript, p int) {
			if p == 2 {
				os.Remove(filepath.Join(f.home, ".codex", "auth.json"))
				os.Remove(filepath.Join(f.home, ".codex", "config.toml"))
			}
		}
		if code := f.attempt(); code != 1 {
			t.Fatalf("attempt = %d", code)
		}
		r := f.report()
		if c, ok := changeOf(r.Protected, "config.toml"); !ok || c.Kind != ChangeDeleted || len(r.Protected) != 1 {
			t.Fatalf("protected %+v", r.Protected)
		}
		text := string(mustRead(t, filepath.Join(f.bundle(), "report.txt")))
		if !strings.Contains(text, strings.Join(ClaudeStateExclusions, "\n")+"\n") || !strings.Contains(text, "  tool-maintained codex .: 0 added, 0 changed, 1 deleted, 1 total\n") {
			t.Fatalf("a failed report lacks the disclosures:\n%s", text)
		}
	})
	for name, tc := range map[string]struct {
		roots func(f *fakeWorld) []string
		want  string
	}{
		"missing": {func(f *fakeWorld) []string { return []string{filepath.Join(t.TempDir(), "missing")} }, CodeInvalidRoot},
		"a file": {func(f *fakeWorld) []string {
			p := filepath.Join(t.TempDir(), "f")
			os.WriteFile(p, nil, 0o600)
			return []string{p}
		}, CodeInvalidRoot},
		"nested": {func(f *fakeWorld) []string {
			d := t.TempDir()
			os.Mkdir(filepath.Join(d, "x"), 0o700)
			return []string{d, filepath.Join(d, "x")}
		}, CodeInvalidRoot},
		"evidence": {func(f *fakeWorld) []string { return []string{filepath.Dir(f.evidence)} }, ""},
	} {
		t.Run("root "+name, func(t *testing.T) {
			t.Parallel()
			f := newFakeWorld(t)
			f.roots = tc.roots(f)
			if name == "evidence" {
				// An ancestor of the evidence destination is allowed (the
				// exact evidence subtree is excluded); the destination
				// itself or anything inside it is not (validateRoots).
				f.git = realGit(t)
				if code := f.attempt(); code != 0 {
					t.Fatalf("attempt = %d %v %s", code, failedCodes(f.report()), f.stderr.String())
				}
				return
			}
			if code := f.attempt(); code != 1 || f.sup.failCode != tc.want {
				t.Fatalf("attempt = %d %q", code, f.sup.failCode)
			}
			if n := len(f.launcher.argv()); n != 0 {
				t.Fatalf("%d executables ran after an invalid root", n)
			}
		})
	}
	t.Run("invalid depth", func(t *testing.T) {
		t.Parallel()
		for _, d := range []string{"-1", "65", "x"} {
			f := newFakeWorld(t)
			f.depth = d
			if code := f.attempt(); code != 2 || !strings.Contains(f.stderr.String(), "--protect-depth") {
				t.Fatalf("depth %s = %d %q", d, code, f.stderr.String())
			}
		}
	})
	t.Run("relative root", func(t *testing.T) {
		t.Parallel()
		f := newFakeWorld(t)
		f.roots = []string{"relative/dir"}
		if code := f.attempt(); code != 2 || !strings.Contains(f.stderr.String(), "--protect-root") {
			t.Fatalf("attempt = %d %q", code, f.stderr.String())
		}
	})
}

// TestEnvironmentUnits (UT-10): root validation, the exact housekeeping
// classes, snapshot collection (missing homes, symlinks recorded as links
// and never followed (A3.4), debug/latest, nested, gitfile, bare, detached
// and unborn repositories, the exact evidence exclusion), compact records
// and the diff's bound detail (A3.4), the comparison of every change kind,
// and every bound.
func TestEnvironmentUnits(t *testing.T) {
	t.Parallel()
	t.Run("inventory", func(t *testing.T) {
		t.Parallel()
		// C6: the checker believes a complete snapshot only with every
		// vendor home and supplied root observed and every record valid.
		roots := []string{"/srv/a", "$HOME/src"}
		valid := func() *Snapshot {
			s := &Snapshot{Phase: "before", Complete: true}
			for _, v := range Vendors {
				s.Compact = append(s.Compact, EnvCompact{Kind: RecHome, Scope: v, Path: "$HOME/." + v, Digest: zeros64})
			}
			for _, r := range roots {
				s.Compact = append(s.Compact, EnvCompact{Kind: RecRoot, Path: r, Digest: zeros64})
			}
			return s
		}
		good := []EnvCompact{
			{Kind: RecEntry, Scope: "claude", Path: "settings.json", Class: ClassOwner, Type: TypeFile, Content: zeros64},
			{Kind: RecEntry, Scope: "codex", Path: encodePath("sessions/\x01.jsonl"), Class: ClassTool, Type: TypeFile},
			{Kind: RecEntry, Scope: "codex", Path: encodePath("prompts/\x01"), Class: ClassOwner, Type: TypeSymlink},
			{Kind: RecRepo, Path: "/srv/a"},
			{Kind: RecRepo, Path: "$HOME/src/x"},
			{Kind: RecInvalid, Path: encodePath("/srv/a/\x02")},
			{Kind: RecLink, Path: "/srv/a/l"},
		}
		s := valid()
		s.Compact = append(s.Compact, good...)
		if err := checkInventory(s, roots); err != nil {
			t.Fatalf("valid inventory: %v", err)
		}
		for name, bad := range map[string]func(s *Snapshot){
			"empty":        func(s *Snapshot) { s.Compact = nil },
			"home missing": func(s *Snapshot) { s.Compact = s.Compact[1:] },
			"root missing": func(s *Snapshot) { s.Compact = s.Compact[:len(s.Compact)-1] },
			"home path":    func(s *Snapshot) { s.Compact[0].Path = "/elsewhere/.claude" },
			"second home": func(s *Snapshot) {
				s.Compact = append(s.Compact, EnvCompact{Kind: RecHome, Scope: "claude", Path: "$HOME/.claude/"})
			},
			"unknown vendor": func(s *Snapshot) { s.Compact = append(s.Compact, EnvCompact{Kind: RecEntry, Scope: "vim", Path: "x"}) },
			"absolute entry": func(s *Snapshot) {
				s.Compact = append(s.Compact, EnvCompact{Kind: RecEntry, Scope: "claude", Path: "/etc/passwd"})
			},
			"escaping entry": func(s *Snapshot) {
				s.Compact = append(s.Compact, EnvCompact{Kind: RecEntry, Scope: "claude", Path: "../.ssh/id"})
			},
			"unclean entry": func(s *Snapshot) {
				s.Compact = append(s.Compact, EnvCompact{Kind: RecEntry, Scope: "claude", Path: "a//b"})
			},
			"unknown root":       func(s *Snapshot) { s.Compact = append(s.Compact, EnvCompact{Kind: RecRoot, Path: "/srv/b"}) },
			"repo outside roots": func(s *Snapshot) { s.Compact = append(s.Compact, EnvCompact{Kind: RecRepo, Path: "/srv/ab"}) },
			"scoped repo": func(s *Snapshot) {
				s.Compact = append(s.Compact, EnvCompact{Kind: RecRepo, Scope: "claude", Path: "/srv/a/x"})
			},
			"link at a root": func(s *Snapshot) { s.Compact = append(s.Compact, EnvCompact{Kind: RecLink, Path: "/srv/a"}) },
			"unknown kind":   func(s *Snapshot) { s.Compact = append(s.Compact, EnvCompact{Kind: "file", Path: "/srv/a/x"}) },
			// A3.6: the class is derived, never trusted; the type is
			// required; tool state holds no content digest.
			"fabricated class": func(s *Snapshot) {
				s.Compact = append(s.Compact, EnvCompact{Kind: RecEntry, Scope: "claude", Path: "settings.json", Class: ClassTool, Type: TypeFile})
			},
			"claimed owner": func(s *Snapshot) {
				s.Compact = append(s.Compact, EnvCompact{Kind: RecEntry, Scope: "claude", Path: "history.jsonl", Class: ClassOwner, Type: TypeFile})
			},
			"no class": func(s *Snapshot) {
				s.Compact = append(s.Compact, EnvCompact{Kind: RecEntry, Scope: "claude", Path: "history.jsonl", Type: TypeFile})
			},
			"no type": func(s *Snapshot) {
				s.Compact = append(s.Compact, EnvCompact{Kind: RecEntry, Scope: "claude", Path: "history.jsonl", Class: ClassTool})
			},
			"unknown type": func(s *Snapshot) {
				s.Compact = append(s.Compact, EnvCompact{Kind: RecEntry, Scope: "claude", Path: "history.jsonl", Class: ClassTool, Type: "socket"})
			},
			"tool content": func(s *Snapshot) {
				s.Compact = append(s.Compact, EnvCompact{Kind: RecEntry, Scope: "claude", Path: "history.jsonl", Class: ClassTool, Type: TypeFile, Content: zeros64})
			},
			"tool oversized": func(s *Snapshot) {
				s.Compact = append(s.Compact, EnvCompact{Kind: RecEntry, Scope: "claude", Path: "history.jsonl", Class: ClassTool, Type: TypeFile, Oversized: true})
			},
			"classed home": func(s *Snapshot) { s.Compact[0].Class = ClassOwner },
			"typed repo": func(s *Snapshot) {
				s.Compact = append(s.Compact, EnvCompact{Kind: RecRepo, Path: "/srv/a/y", Type: TypeDir})
			},
		} {
			s := valid()
			bad(s)
			if err := checkInventory(s, roots); err == nil {
				t.Errorf("%s: accepted", name)
			}
		}
		if decodeToken("b64:!") != "b64:!" || cleanRelative("") || cleanRelative("..") {
			t.Fatal("token helpers")
		}
	})
	t.Run("roots", func(t *testing.T) {
		t.Parallel()
		d := t.TempDir()
		for _, p := range []string{"a/b", "ab", "ev/x", "rt"} {
			os.MkdirAll(filepath.Join(d, p), 0o700)
		}
		os.Symlink(filepath.Join(d, "a"), filepath.Join(d, "link"))
		got, err := validateRoots([]string{filepath.Join(d, "ab"), filepath.Join(d, "link"), filepath.Join(d, "a")}, filepath.Join(d, "ev", "x"), filepath.Join(d, "rt"))
		cd, _ := filepath.EvalSymlinks(d)
		if err != nil || !slices.Equal(got, []string{filepath.Join(cd, "a"), filepath.Join(cd, "ab")}) {
			t.Fatalf("roots %v %v", got, err)
		}
		if got, err := validateRoots(nil, "", ""); err != nil || got != nil {
			t.Fatalf("zero roots %v %v", got, err)
		}
		if _, err := validateRoots([]string{filepath.Join(d, "ev")}, filepath.Join(d, "ev", "x"), ""); err != nil {
			t.Fatalf("an ancestor of the evidence refused: %v", err)
		}
		for name, raw := range map[string][]string{
			"relative": {"a"}, "missing": {filepath.Join(d, "none")}, "nested": {filepath.Join(d, "a"), filepath.Join(d, "a", "b")},
			"evidence": {filepath.Join(d, "ev", "x")}, "inside runtime": {filepath.Join(d, "rt")},
		} {
			if _, err := validateRoots(raw, filepath.Join(d, "ev", "x"), filepath.Join(d, "rt")); err == nil {
				t.Errorf("%s accepted", name)
			}
		}
		locked := filepath.Join(d, "locked")
		os.Mkdir(locked, 0o000)
		defer os.Chmod(locked, 0o700)
		if f, err := os.Open(locked); err != nil {
			if _, err := validateRoots([]string{locked}, "", ""); err == nil {
				t.Error("an unreadable root accepted")
			}
		} else {
			f.Close() // running privileged: unreadability cannot be staged
		}
		if !under("/x/a/b", "/x/a") || under("/x/ab", "/x/a") || under("/x/a", "") || !tokenStrictlyUnder("$HOME/a/b", "$HOME/a") || tokenStrictlyUnder("$HOME/ab", "$HOME/a") {
			t.Fatal("component-aware nesting")
		}
	})
	t.Run("roots through a symlinked parent", func(t *testing.T) {
		t.Parallel()
		// The macOS shape: the evidence and runtime are spelled through a
		// symlinked ancestor, the roots compared with them canonically.
		d := linkedParent(t, t.TempDir())
		for _, p := range []string{"ev/x/in", "rt/sub"} {
			os.MkdirAll(filepath.Join(d, p), 0o700)
		}
		cd, _ := filepath.EvalSymlinks(d)
		ev, rt := filepath.Join(d, "ev", "x"), filepath.Join(d, "rt")
		for name, raw := range map[string][]string{
			"evidence": {ev}, "inside evidence": {filepath.Join(ev, "in")}, "canonical evidence": {filepath.Join(cd, "ev", "x")},
			"runtime": {rt}, "inside runtime": {filepath.Join(rt, "sub")}, "canonical runtime": {filepath.Join(cd, "rt", "sub")},
		} {
			if _, err := validateRoots(raw, ev, rt); err == nil {
				t.Errorf("%s accepted", name)
			}
		}
		// A new evidence directory resolves through its existing parent.
		for _, root := range []string{filepath.Join(d, "ev"), filepath.Join(cd, "ev")} {
			if got, err := validateRoots([]string{root}, filepath.Join(d, "ev", "new"), rt); err != nil || !slices.Equal(got, []string{filepath.Join(cd, "ev")}) {
				t.Errorf("an ancestor of a new evidence directory: %v %v", got, err)
			}
		}
		if _, err := validateRoots([]string{filepath.Join(cd, "ev")}, filepath.Join(d, "ev", "new", "deeper"), rt); err != nil {
			t.Errorf("a new evidence directory below a missing parent: %v", err)
		}
		write(t, filepath.Join(d, "file"), "x")
		if _, err := validateRoots([]string{filepath.Join(d, "ev")}, filepath.Join(d, "file", "x"), rt); err == nil {
			t.Error("an evidence directory that cannot be resolved accepted")
		}
	})
	t.Run("ownership classes", func(t *testing.T) {
		t.Parallel()
		// A3.6: the exact owner-configuration list, its descendants and
		// structural ancestors; everything else is tool-maintained,
		// whatever its name, suffix or type. Pairs put each strict path
		// next to the tool state it resembles.
		owner, tool := ClassOwner, ClassTool
		for _, c := range []struct{ vendor, rel, class string }{
			{"claude", "CLAUDE.md", owner}, {"claude", "CLAUDE.md.bak", tool}, {"claude", "claude.md", tool},
			{"claude", "settings.json", owner}, {"claude", "settings.local.json", owner}, {"claude", "settings-x.json", owner},
			{"claude", "settings", tool}, {"claude", "settings.json.tmp", tool}, {"claude", "projects/-x/settings.json", tool},
			{"claude", "agents", owner}, {"claude", "agents/a.md", owner}, {"claude", "agents/new/deep.md", owner}, {"claude", "agents.bak", tool},
			{"claude", "skills", owner}, {"claude", "skills/a/SKILL.md", owner}, {"claude", "commands/c.md", owner}, {"claude", "hooks/h.sh", owner},
			{"claude", "plugins", owner}, {"claude", "plugins/installed_plugins.json", owner}, {"claude", "plugins/known_marketplaces.json", owner},
			{"claude", "plugins/blocklist.json", owner}, {"claude", "plugins/config.json", owner}, {"claude", "plugins/config.json.bak", tool},
			{"claude", "plugins/cache/m/p/plugin.json", tool}, {"claude", "plugins/marketplaces/m/.git/HEAD", tool}, {"claude", "plugins/repos", tool},
			{"claude", ".config.json", tool}, {"claude", "backups/.config.json.backup.1", tool}, {"claude", "projects/-opt-x/memory/MEMORY.md", tool},
			{"claude", "history.jsonl", tool}, {"claude", ".credentials.json", tool}, {"claude", "statsig/x", tool},
			{"codex", "config.toml", owner}, {"codex", "config.toml.bak", tool}, {"codex", "AGENTS.md", owner}, {"codex", "AGENTS.override.md", owner},
			{"codex", "AGENTS-team.md", owner}, {"codex", "AGENTS", tool}, {"codex", "AGENTS.md.orig", tool}, {"codex", "sub/AGENTS.md", tool},
			{"codex", "instructions.md", owner}, {"codex", "prompts", owner}, {"codex", "prompts/p.md", owner}, {"codex", "prompts.bak/p.md", tool},
			{"codex", "auth.json", tool}, {"codex", "state_5.sqlite", tool}, {"codex", ".tmp/plugins/cache", tool}, {"codex", "plugins/cache/x", tool},
			{"grok", "config.toml", owner}, {"grok", "managed_config.toml", owner}, {"grok", "settings.json", owner}, {"grok", "user-settings.json", owner},
			{"grok", "settings_cache.json", tool}, {"grok", "cache/config.toml", tool}, {"grok", "bundled/skills/x.md", tool}, {"grok", "auth.json", tool},
			{"cursor", "cli-config.json", owner}, {"cursor", "mcp.json", owner}, {"cursor", "cli-config.json.bad", tool}, {"cursor", "projects/x/mcp.json", tool},
			{"cursor", "chats/x/store.db", tool}, {"cursor", "settings.json", tool}, {"cursor", "agents/a.md", tool},
			{"grok", "CLAUDE.md", tool}, {"codex", "skills/a", tool}, {"cursor", "config.toml", tool},
		} {
			if got := ownerClass(c.vendor, c.rel); got != c.class {
				t.Errorf("%s %s = %q, want %q", c.vendor, c.rel, got, c.class)
			}
		}
		for _, c := range []struct {
			path string
			dir  bool
			want string
		}{{"history.jsonl", false, "."}, {"projects", true, "projects"}, {"projects/-x/a.jsonl", false, "projects"}, {"link", false, "."}} {
			if got := topLevel(c.path, c.dir); got != c.want {
				t.Errorf("topLevel(%s) = %q", c.path, got)
			}
		}
	})
	t.Run("strict owner configuration", func(t *testing.T) {
		t.Parallel()
		// Every named pattern and directory on each vendor: an addition,
		// a same-size edit, a mode change, a deletion, a substituted
		// symlink or directory, and a new descendant of a listed directory
		// all fail, beside tool state that changes freely.
		compare := func(t *testing.T, setup, mutate func(home string)) EnvDiff {
			t.Helper()
			home := t.TempDir()
			setup(home)
			before := snap(t, "before", home, nil, nil)
			mutate(home)
			return CompareEnvironment(before, snap(t, "after", home, nil, nil), nil)
		}
		files := map[string][]string{
			"claude": {"CLAUDE.md", "settings.json", "settings.local.json", "agents/a.md", "skills/s/SKILL.md", "commands/c.md", "hooks/h.sh",
				"plugins/installed_plugins.json", "plugins/known_marketplaces.json", "plugins/blocklist.json", "plugins/config.json"},
			"codex":  {"config.toml", "AGENTS.md", "AGENTS.override.md", "instructions.md", "prompts/p.md"},
			"grok":   {"config.toml", "managed_config.toml", "settings.json", "user-settings.json"},
			"cursor": {"cli-config.json", "mcp.json"},
		}
		neighbors := map[string]string{"claude": "plugins/cache/m/plugin.json", "codex": "plugins/cache/x.json", "grok": "settings_cache.json",
			"cursor": "chats/store.db"}
		mutations := []struct {
			name, kind string
			f          func(p string) error
		}{
			{"same-size edit", ChangeModified, func(p string) error { return os.WriteFile(p, []byte("bbbb"), 0o600) }},
			{"mode", ChangeMode, func(p string) error { return os.Chmod(p, 0o644) }},
			{"deleted", ChangeDeleted, os.Remove},
			{"symlink", ChangeReplaced, func(p string) error {
				os.Remove(p)
				return os.Symlink("/nonexistent/target", p)
			}},
			{"directory", ChangeReplaced, func(p string) error {
				os.Remove(p)
				return os.Mkdir(p, 0o700)
			}},
		}
		for vendor, rels := range files {
			home := "." + vendor
			for _, rel := range rels {
				setup := func(h string) {
					write(t, filepath.Join(h, home, filepath.FromSlash(rel)), "aaaa")
					write(t, filepath.Join(h, home, filepath.FromSlash(neighbors[vendor])), "n")
				}
				for _, m := range mutations {
					d := compare(t, setup, func(h string) {
						if err := m.f(filepath.Join(h, home, filepath.FromSlash(rel))); err != nil {
							t.Fatal(err)
						}
						write(t, filepath.Join(h, home, filepath.FromSlash(neighbors[vendor])), "changed")
					})
					if c, ok := changeOf(d.Protected, rel); !ok || c.Kind != m.kind || !slices.Contains(d.Codes, CodeEnvModified) {
						t.Errorf("%s %s %s = %+v (protected %+v)", vendor, rel, m.name, c, d.Protected)
					}
					if _, ok := changeOf(d.Protected, neighbors[vendor]); ok || len(d.ToolSummary) == 0 {
						t.Errorf("%s %s %s: neighbour %+v", vendor, rel, m.name, d.ToolSummary)
					}
				}
				d := compare(t, func(h string) { os.MkdirAll(filepath.Join(h, home, filepath.Dir(filepath.FromSlash(rel))), 0o700) }, func(h string) {
					write(t, filepath.Join(h, home, filepath.FromSlash(rel)), "aaaa")
				})
				if c, ok := changeOf(d.Protected, rel); !ok || c.Kind != ChangeAdded {
					t.Errorf("%s %s added = %+v", vendor, rel, d.Protected)
				}
			}
		}
		// A new descendant of each listed directory is a strict addition.
		for _, dir := range []string{".claude/agents", ".claude/skills", ".claude/commands", ".claude/hooks", ".codex/prompts"} {
			d := compare(t, func(h string) { os.MkdirAll(filepath.Join(h, dir), 0o700) }, func(h string) { write(t, filepath.Join(h, dir, "new", "x.md"), "x") })
			rel := dir[strings.Index(dir, "/")+1:]
			if _, ok := changeOf(d.Protected, rel+"/new/x.md"); !ok {
				t.Errorf("%s descendant %+v", dir, d.Protected)
			}
			if _, ok := changeOf(d.Protected, rel+"/new"); !ok {
				t.Errorf("%s new directory %+v", dir, d.Protected)
			}
		}
		// Claude plugins/ is a structural ancestor: its creation and its
		// children's churn alone pass; its deletion or replacement fails.
		cache := func(h string) { write(t, filepath.Join(h, ".claude", "plugins", "cache", "p.json"), "1") }
		if d := compare(t, func(string) {}, cache); len(d.Protected) != 0 || len(d.ToolSummary) == 0 {
			t.Errorf("plugins created %+v", d.Protected)
		}
		if d := compare(t, cache, func(h string) { write(t, filepath.Join(h, ".claude", "plugins", "cache", "q.json"), "22") }); len(d.Protected) != 0 {
			t.Errorf("plugin cache churn %+v", d.Protected)
		}
		if d := compare(t, cache, func(h string) { os.RemoveAll(filepath.Join(h, ".claude", "plugins")) }); !slices.ContainsFunc(d.Protected, func(c EnvChange) bool {
			return c.Path == "plugins" && c.Kind == ChangeDeleted
		}) {
			t.Errorf("plugins deleted %+v", d.Protected)
		}
		if d := compare(t, cache, func(h string) {
			os.RemoveAll(filepath.Join(h, ".claude", "plugins"))
			os.Symlink("/nonexistent", filepath.Join(h, ".claude", "plugins"))
		}); !slices.ContainsFunc(d.Protected, func(c EnvChange) bool { return c.Path == "plugins" && c.Kind == ChangeReplaced }) {
			t.Errorf("plugins replaced %+v", d.Protected)
		}
		if d := compare(t, cache, func(h string) { os.Chmod(filepath.Join(h, ".claude", "plugins"), 0o700) }); !slices.ContainsFunc(d.Protected, func(c EnvChange) bool {
			return c.Path == "plugins" && c.Kind == ChangeMode
		}) {
			t.Errorf("plugins mode %+v", d.Protected)
		}
		// Only a real directory may be created there: a regular file or a
		// symlink (never followed, even to plugin selection elsewhere) at
		// the previously absent ancestor is a strict addition.
		external := t.TempDir()
		write(t, filepath.Join(external, "installed_plugins.json"), `{"plugins":{}}`)
		for name, create := range map[string]func(p string) error{
			"symlink to external plugins": func(p string) error { return os.Symlink(external, p) },
			"regular file":                func(p string) error { return os.WriteFile(p, []byte("x"), 0o600) },
			"dangling symlink":            func(p string) error { return os.Symlink("/nonexistent/plugins", p) },
		} {
			d := compare(t, func(h string) { os.MkdirAll(filepath.Join(h, ".claude"), 0o700) }, func(h string) {
				if err := create(filepath.Join(h, ".claude", "plugins")); err != nil {
					t.Fatal(err)
				}
			})
			if c, ok := changeOf(d.Protected, "plugins"); !ok || c.Kind != ChangeAdded || c.AfterType == TypeDir || !slices.Contains(d.Codes, CodeEnvModified) {
				t.Errorf("plugins created as %s = %+v", name, d.Protected)
			}
		}
		// A vendor home appearing as a directory is the tool's; one
		// appearing as anything else, or disappearing, fails.
		if d := compare(t, func(string) {}, func(h string) { write(t, filepath.Join(h, ".cursor", "chats", "x"), "x") }); len(d.Protected) != 0 {
			t.Errorf("cursor home created %+v", d.Protected)
		}
		for name, create := range map[string]func(p string) error{
			"dangling symlink":       func(p string) error { return os.Symlink("/nonexistent/cursor", p) },
			"symlink to a directory": func(p string) error { return os.Symlink(external, p) },
		} {
			d := compare(t, func(string) {}, func(h string) {
				if err := create(filepath.Join(h, ".cursor")); err != nil {
					t.Fatal(err)
				}
			})
			if c, ok := changeOf(d.Protected, "$HOME/.cursor"); !ok || c.Kind != ChangeAdded || c.Scope != "cursor" || !slices.Contains(d.Codes, CodeEnvModified) {
				t.Errorf("cursor home created as %s = %+v", name, d.Protected)
			}
		}
		if d := compare(t, func(h string) { write(t, filepath.Join(h, ".grok", "cache", "x"), "x") }, func(h string) {
			os.RemoveAll(filepath.Join(h, ".grok"))
		}); len(d.Protected) == 0 {
			t.Error("a vendor home deletion passed")
		}
	})
	t.Run("tool-maintained changes are report-only", func(t *testing.T) {
		t.Parallel()
		// Each formerly special family and an unknown future path: every
		// change kind, including symlink retargets and the substitution
		// of a symlink, is summarized and never protected; no link is
		// followed and no token content is hashed.
		home := t.TempDir()
		paths := []string{".claude/.credentials.json", ".claude/.config.json", ".claude/backups/.config.json.backup.1", ".claude/history.jsonl",
			".claude/projects/-x/s.jsonl", ".claude/debug/latest", ".claude/plugins/cache/p.json", ".codex/auth.json", ".codex/state_5.sqlite",
			".codex/sessions/a.jsonl", ".grok/auth.json", ".grok/sessions/s.json", ".grok/bundled/x.md", ".cursor/chats/c.db", ".cursor/never-seen/v9/x"}
		for _, p := range paths {
			write(t, filepath.Join(home, filepath.FromSlash(p)), `{"token":"secret-token-value"}`)
		}
		os.MkdirAll(filepath.Join(home, ".codex", "app-server-control"), 0o700)
		link := filepath.Join(home, ".codex", "app-server-control", "app-server-control.sock")
		os.Symlink("/tmp/codex-a/s.sock", link)
		base := snap(t, "before", home, nil, nil)
		for _, r := range base.Records {
			if r.Kind == RecEntry && r.Path != "plugins" && (r.Class != ClassTool || r.SHA256 != "") {
				t.Errorf("%s %s class %q sha %q", r.Scope, r.Path, r.Class, r.SHA256)
			}
		}
		step := func(name string, f func()) EnvDiff {
			t.Helper()
			before := snap(t, "before", home, nil, nil)
			f()
			d := CompareEnvironment(before, snap(t, "after", home, nil, nil), nil)
			if len(d.Protected) != 0 || len(d.ToolSummary) == 0 || slices.Contains(d.Codes, CodeEnvModified) {
				t.Errorf("%s: protected %+v summary %+v", name, d.Protected, d.ToolSummary)
			}
			return d
		}
		step("modified", func() {
			for _, p := range paths {
				write(t, filepath.Join(home, filepath.FromSlash(p)), `{"token":"refreshed-token"}`)
			}
		})
		step("mode", func() {
			for _, p := range paths {
				os.Chmod(filepath.Join(home, filepath.FromSlash(p)), 0o644)
			}
		})
		step("replaced", func() {
			for _, p := range paths {
				full := filepath.Join(home, filepath.FromSlash(p))
				os.WriteFile(full+".new", []byte("x"), 0o600)
				os.Rename(full+".new", full)
			}
		})
		step("retargeted", func() {
			os.Remove(link)
			os.Symlink("/tmp/codex-b/s.sock", link)
		})
		step("link substituted", func() {
			p := filepath.Join(home, ".codex", "auth.json")
			os.Remove(p)
			os.Symlink("/nonexistent/auth.json", p)
		})
		step("deleted", func() {
			for _, p := range paths[:5] {
				os.Remove(filepath.Join(home, filepath.FromSlash(p)))
			}
		})
		d := step("added", func() { write(t, filepath.Join(home, ".grok", "sessions", "t.json"), "{}") })
		if want := []ToolSummaryRow{{Scope: "grok", TopLevel: "sessions", Added: 1, Total: 1}}; !slices.Equal(d.ToolSummary, want) {
			t.Errorf("summary %+v", d.ToolSummary)
		}
		// A tool-maintained path inside a supplied protected root is a
		// repository change there and stays strict.
		h2, err := filepath.EvalSymlinks(t.TempDir())
		if err != nil {
			t.Fatal(err)
		}
		repo := filepath.Join(h2, ".claude", "plugins", "marketplaces", "m")
		newRepo(t, repo)
		roots := []string{filepath.Join(h2, ".claude", "plugins")}
		before := snap(t, "before", h2, roots, nil)
		write(t, filepath.Join(repo, "dirty.txt"), "x")
		if d := CompareEnvironment(before, snap(t, "after", h2, roots, nil), roots); !slices.Contains(d.Codes, CodeEnvModified) {
			t.Errorf("a repository below a tool path changed silently %+v", d)
		}
	})
	t.Run("bounded directory reads", func(t *testing.T) {
		t.Parallel()
		// C11: a directory too large for the remaining entry budget, or
		// still being read at the deadline, fails after a bounded number
		// of entries, never read whole first.
		canon := func(p string) string {
			c, err := filepath.EvalSymlinks(p)
			if err != nil {
				t.Fatal(err)
			}
			return c
		}
		home, root, inv := canon(t.TempDir()), canon(t.TempDir()), canon(t.TempDir())
		for _, d := range []string{filepath.Join(home, ".claude", "big"), filepath.Join(root, "big"), filepath.Join(inv, ".git")} {
			os.MkdirAll(d, 0o700)
		}
		big := map[string]bool{filepath.Join(home, ".claude", "big"): true, filepath.Join(root, "big"): true, filepath.Join(inv, ".git"): true}
		scanner := func(e *endlessDirs, clock Clock, deadline time.Time, entries int) *envScanner {
			return &envScanner{clock: clock, deadline: deadline, home: home, tok: pathTokenizer{home: home}, depth: DefaultProtectDepth,
				snap: Snapshot{Complete: true}, repoSeen: map[string]bool{}, openDir: e.open, entries: entries}
		}
		for name, scan := range map[string]func(*envScanner) error{
			"vendor home":        (*envScanner).scanHomes,
			"discovery":          func(sc *envScanner) error { return sc.discover(root) },
			"invalid repository": func(sc *envScanner) error { return sc.invalidRepo(inv, false) },
		} {
			for _, entries := range []int{0, maxEnvEntries - 10} {
				e := &endlessDirs{t: t, big: big}
				sc := scanner(e, realClock{}, time.Time{}, entries)
				if err := scan(sc); err == nil || sc.snap.Code != CodeEnvLimit || e.read > maxEnvEntries-entries+dirChunk {
					t.Errorf("%s from %d entries: %v %q after %d entries read", name, entries, err, sc.snap.Code, e.read)
				}
			}
			e := &endlessDirs{t: t, big: big}
			clock := &stepClock{}
			sc := scanner(e, clock, clock.now.Add(20*time.Second), 0)
			if err := scan(sc); err == nil || sc.snap.Code != CodeEnvUnobserved || e.read > 20*dirChunk {
				t.Errorf("%s at the deadline: %v %q after %d entries read", name, err, sc.snap.Code, e.read)
			}
		}
	})
	t.Run("bounded nested directory reads", func(t *testing.T) {
		t.Parallel()
		// C11: entries read into an ancestor directory's list but not yet
		// processed count toward the global bound before any deeper read,
		// so four nested directories of 60,000 entries each stop after the
		// bound plus one chunk in total, not 240,000 entries.
		home, root := t.TempDir(), t.TempDir()
		os.MkdirAll(filepath.Join(home, ".claude"), 0o700)
		const size = 60000
		nested := func(top string) map[string]int {
			m := map[string]int{}
			for i, p := 0, top; i < 4; i, p = i+1, filepath.Join(p, "a") {
				m[p] = size
			}
			return m
		}
		for name, c := range map[string]struct {
			top  string
			scan func(*envScanner) error
		}{
			"vendor home": {filepath.Join(home, ".claude"), (*envScanner).scanHomes},
			"discovery":   {root, func(sc *envScanner) error { return sc.discover(root) }},
		} {
			e := &nestedDirs{tree: nested(c.top)}
			sc := &envScanner{clock: realClock{}, home: home, tok: pathTokenizer{home: home}, depth: DefaultProtectDepth,
				snap: Snapshot{Complete: true}, repoSeen: map[string]bool{}, openDir: e.open}
			if err := c.scan(sc); err == nil || sc.snap.Code != CodeEnvLimit || e.read > maxEnvEntries+dirChunk {
				t.Errorf("%s: %v %q after %d entries read (bound %d)", name, err, sc.snap.Code, e.read, maxEnvEntries)
			}
		}
	})
	t.Run("collection and comparison", func(t *testing.T) {
		t.Parallel()
		home := t.TempDir()
		outside := t.TempDir()
		write(t, filepath.Join(home, ".claude", "settings.json"), "{}")
		write(t, filepath.Join(home, ".claude", "CLAUDE.md"), "aaaa")
		write(t, filepath.Join(home, ".claude", "history.jsonl"), "1\n")
		write(t, filepath.Join(home, ".claude", ".credentials.json"), "t1")
		write(t, filepath.Join(home, ".claude", "debug", "s1.txt"), "d")
		os.Symlink("s1.txt", filepath.Join(home, ".claude", "debug", "latest"))
		write(t, filepath.Join(outside, "skills", "s", "SKILL.md"), "skill")
		os.Symlink(filepath.Join(outside, "skills"), filepath.Join(home, ".claude", "skills"))
		os.Symlink(filepath.Join(outside, "skills"), filepath.Join(outside, "skills", "loop"))
		write(t, filepath.Join(home, ".codex", "config.toml"), "x")
		write(t, filepath.Join(home, ".codex", "auth.json"), "a1")
		write(t, filepath.Join(home, ".codex", "logs", "x.log"), "l")
		grok := t.TempDir()
		write(t, filepath.Join(grok, "settings.json"), "{}")
		os.Symlink(grok, filepath.Join(home, ".grok"))
		// Repositories: plain, nested, gitfile worktree, bare, detached and
		// unborn, plus an exact exclusion and its sibling.
		root := t.TempDir()
		newRepo(t, filepath.Join(root, "plain"))
		newRepo(t, filepath.Join(root, "plain", "inner"))
		gitIn(t, filepath.Join(root, "plain"), "worktree", "add", "-q", filepath.Join(root, "wt"))
		gitIn(t, root, "init", "-q", "--bare", "b.git")
		newRepo(t, filepath.Join(root, "det"))
		gitIn(t, filepath.Join(root, "det"), "checkout", "-q", "--detach")
		os.MkdirAll(filepath.Join(root, "unborn"), 0o755)
		gitIn(t, filepath.Join(root, "unborn"), "init", "-q")
		os.MkdirAll(filepath.Join(root, "plain", "runs", "mine"), 0o755)
		exclude := []string{filepath.Join(root, "plain", "runs", "mine")}
		before := snap(t, "before", home, []string{root}, exclude)
		heads := map[string]string{}
		for _, r := range before.Records {
			if r.Kind == RecRepo {
				heads[filepath.Base(r.Path)] = r.Head + "|" + r.Branch
			}
			if r.Kind == RecHome && r.Scope == "cursor" && r.Type != TypeAbsent {
				t.Fatal("a missing vendor home is not absent")
			}
		}
		if len(heads) != 6 || !strings.HasSuffix(heads["det"], "|detached") || heads["unborn"] != "unborn|refs/heads/main" || !strings.HasSuffix(heads["wt"], "|refs/heads/wt") {
			t.Fatalf("repositories %v", heads)
		}
		// Unchanged: equal snapshots compare clean.
		if d := CompareEnvironment(before, snap(t, "after", home, []string{root}, exclude), before.Roots); !d.Complete || len(d.Protected)+len(d.ToolSummary) != 0 || len(d.Codes) != 0 {
			t.Fatalf("an unchanged environment differs: %+v", d)
		}
		// Changes of every kind.
		write(t, filepath.Join(home, ".claude", "CLAUDE.md"), "bbbb") // equal size, other content
		os.Chmod(filepath.Join(home, ".claude", "settings.json"), 0o644)
		write(t, filepath.Join(home, ".claude", "history.jsonl"), "1\n2\n")
		write(t, filepath.Join(home, ".claude", ".credentials.json"), "t2")
		write(t, filepath.Join(home, ".claude", "todos", "n.json"), "[]")
		os.Remove(filepath.Join(home, ".codex", "logs", "x.log"))
		write(t, filepath.Join(home, ".codex", "notes.md"), "unlisted")
		write(t, filepath.Join(home, ".codex", "AGENTS.md"), "new instructions")
		os.Chmod(filepath.Join(home, ".codex", "auth.json"), 0o644)
		os.MkdirAll(filepath.Join(home, ".codex", "empty"), 0o700)
		write(t, filepath.Join(outside, "skills", "s", "SKILL.md"), "SKILL")
		os.Remove(filepath.Join(home, ".claude", "debug", "latest"))
		os.Symlink(filepath.Join(outside, "skills", "s", "SKILL.md"), filepath.Join(home, ".claude", "debug", "latest"))
		write(t, filepath.Join(root, "plain", "runs", "mine", "bundle.txt"), "evidence")
		write(t, filepath.Join(root, "plain", "runs", "other.txt"), "sibling")
		gitIn(t, filepath.Join(root, "det"), "checkout", "-q", "main")
		after := snap(t, "after", home, []string{root}, exclude)
		d := CompareEnvironment(before, after, before.Roots)
		want := map[string]string{"CLAUDE.md": ChangeModified, "settings.json": ChangeMode, "AGENTS.md": ChangeAdded}
		for p, kind := range want {
			if c, ok := changeOf(d.Protected, p); !ok || c.Kind != kind {
				t.Errorf("protected %s = %+v, want %s", p, c, kind)
			}
		}
		if c, _ := changeOf(d.Protected, "CLAUDE.md"); !slices.Contains(c.Fields, "content") {
			t.Errorf("equal-size content change fields %v", c.Fields)
		}
		// A3.6: every other vendor change is tool-maintained, summarized
		// per top-level directory, with full detail of each.
		if want := []ToolSummaryRow{{Scope: "claude", TopLevel: ".", Changed: 2, Total: 2}, {Scope: "claude", TopLevel: "debug", Changed: 1, Total: 1},
			{Scope: "claude", TopLevel: "todos", Added: 2, Total: 2}, {Scope: "codex", TopLevel: ".", Added: 1, Changed: 1, Total: 2},
			{Scope: "codex", TopLevel: "empty", Added: 1, Total: 1}, {Scope: "codex", TopLevel: "logs", Deleted: 1, Total: 1}}; !slices.Equal(d.ToolSummary, want) {
			t.Errorf("tool summary %+v", d.ToolSummary)
		}
		kinds := map[string]string{}
		for _, dt := range d.Detail {
			if r := detailRecord(dt); r.Kind == RecEntry && r.Class == ClassTool {
				kinds[r.Path] = dt.Change
			}
		}
		if d.ToolDetailCount != 9 || d.ToolOmittedCount != 0 || kinds["todos/n.json"] != ChangeAdded || kinds["logs/x.log"] != ChangeDeleted ||
			kinds["auth.json"] != ChangeMode || kinds["history.jsonl"] != ChangeModified {
			t.Errorf("tool detail %d %v", d.ToolDetailCount, kinds)
		}
		// A3.4: the skills link is the link itself; its target's content
		// change is not observed through it.
		repos := map[string][]string{}
		for _, c := range d.Protected {
			if strings.HasSuffix(c.Path, "SKILL.md") {
				t.Errorf("a symlink's target was followed: %+v", c)
			}
			if c.Class == "repository" {
				repos[filepath.Base(c.Path)] = c.Fields
			}
		}
		if !slices.Contains(repos["plain"], "status") || !slices.Contains(repos["det"], "branch") || len(repos) != 2 {
			t.Errorf("repository changes %v (the exact exclusion must not count, its sibling must)", repos)
		}
		if !slices.Equal(d.Codes, []string{CodeEnvModified}) {
			t.Errorf("codes %v", d.Codes)
		}
		// A credential replaced by a link is the tool's; a named
		// configuration file replaced by one is not.
		os.Remove(filepath.Join(home, ".claude", ".credentials.json"))
		os.Symlink(filepath.Join(home, ".codex", "config.toml"), filepath.Join(home, ".claude", ".credentials.json"))
		os.Remove(filepath.Join(home, ".claude", "CLAUDE.md"))
		os.Symlink(filepath.Join(home, ".codex", "config.toml"), filepath.Join(home, ".claude", "CLAUDE.md"))
		d = CompareEnvironment(after, snap(t, "after", home, []string{root}, exclude), before.Roots)
		if c, ok := changeOf(d.Protected, "CLAUDE.md"); !ok || c.Kind != ChangeReplaced || len(d.Protected) != 1 {
			t.Errorf("replaced configuration %+v", d.Protected)
		}
		// A repository overlapping a tool-maintained path stays protected.
		over := t.TempDir()
		ohome := filepath.Join(over, "home")
		newRepo(t, filepath.Join(ohome, ".claude", "projects", "p"))
		uuid := "0f1e2d3c-4b5a-4968-8786-a5b4c3d2e1f0"
		write(t, filepath.Join(ohome, ".claude", "projects", "p", uuid+".jsonl"), "1\n")
		proot := filepath.Join(ohome, ".claude", "projects", "p")
		gitIn(t, proot, "add", uuid+".jsonl")
		gitIn(t, proot, "commit", "-q", "-m", "t")
		ob := snap(t, "before", ohome, []string{proot}, nil)
		write(t, filepath.Join(proot, uuid+".jsonl"), "1\n2\n")
		d = CompareEnvironment(ob, snap(t, "after", ohome, []string{proot}, nil), ob.Roots)
		if len(d.ToolSummary) != 1 || d.ToolSummary[0].TopLevel != "projects" || len(d.Protected) == 0 || !slices.Contains(d.Codes, CodeEnvModified) {
			t.Errorf("overlap: protected %+v tool summary %+v", d.Protected, d.ToolSummary)
		}
		// A root that disappeared, a root inventory mismatch, an incomplete
		// snapshot.
		gone := t.TempDir()
		gb := snap(t, "before", home, []string{gone}, nil)
		os.Remove(gone)
		ga := takeSnapshot("after", home, []string{gone}, nil, realGit(t), pathTokenizer{}, realClock{}, time.Time{})
		if d := CompareEnvironment(gb, ga, gb.Roots); !slices.Contains(d.Codes, CodeEnvModified) {
			t.Errorf("a vanished root: %+v", d)
		}
		if d := CompareEnvironment(gb, gb, []string{"/other"}); d.Complete || !slices.Contains(d.Codes, CodeEnvUnobserved) {
			t.Errorf("roots mismatch: %+v", d)
		}
		inc := &Snapshot{Phase: "after", Code: CodeEnvLimit, Roots: gb.Roots}
		if d := CompareEnvironment(gb, inc, gb.Roots); d.Complete || !slices.Equal(d.Codes, []string{CodeEnvLimit, CodeEnvModified, CodeEnvUnobserved}) {
			t.Errorf("incomplete: %+v", d.Codes)
		}
		if d := CompareEnvironment(nil, nil, nil); d.Complete || !slices.Equal(d.Codes, []string{CodeEnvUnobserved}) {
			t.Errorf("no snapshots: %+v", d)
		}
	})
	t.Run("discovery stops at working trees", func(t *testing.T) {
		t.Parallel()
		// A3.1: several repositories with large working trees under a
		// non-repository directory: discovery reads only the directories
		// above them and records each repository once.
		root := t.TempDir()
		for _, name := range []string{"group/a", "group/b", "c"} {
			dir := filepath.Join(root, filepath.FromSlash(name))
			newRepo(t, dir)
			write(t, filepath.Join(dir, ".gitignore"), "build/\n")
			gitIn(t, dir, "add", ".gitignore")
			gitIn(t, dir, "commit", "-q", "-m", "ignore")
			for i := 0; i < 1500; i++ {
				write(t, filepath.Join(dir, "build", fmt.Sprintf("d%02d", i%30), fmt.Sprintf("f%04d.o", i)), "x")
			}
		}
		sc := &envScanner{git: realGit(t), clock: realClock{}, depth: DefaultProtectDepth, snap: Snapshot{Complete: true}, repoSeen: map[string]bool{}}
		if err := sc.discover(root); err != nil || !sc.snap.Complete {
			t.Fatalf("discover: %v %s", err, sc.snap.Code)
		}
		var repos []string
		for _, r := range sc.snap.Records {
			if r.Kind == RecRepo {
				repos = append(repos, r.Path)
			}
		}
		if len(repos) != 3 || sc.entries > 5 {
			t.Fatalf("repositories %v after %d entries read", repos, sc.entries)
		}
		// Nested repositories: a tracked gitlink's inner change shows in the
		// parent's status digest and the gitlink's repository is recorded as
		// its own; an untracked unignored one (an opaque "?? dir/" status
		// line) is recorded as its own repository; an ignored one is not
		// separately protected.
		top := filepath.Join(t.TempDir(), "top")
		newRepo(t, top)
		write(t, filepath.Join(top, ".gitignore"), "vendor/\n")
		gitIn(t, top, "add", ".gitignore")
		gitIn(t, top, "commit", "-q", "-m", "ignore")
		newRepo(t, filepath.Join(top, "linked"))
		gitIn(t, top, "add", "linked")
		gitIn(t, top, "commit", "-q", "-m", "gitlink")
		newRepo(t, filepath.Join(top, "loose"))
		newRepo(t, filepath.Join(top, "vendor", "lib"))
		home := t.TempDir()
		before := snap(t, "before", home, []string{top}, nil)
		recorded := map[string]bool{}
		for _, r := range before.Records {
			if r.Kind == RecRepo {
				recorded[filepath.Base(r.Path)] = true
			}
		}
		if !recorded["top"] || !recorded["loose"] || !recorded["linked"] || recorded["lib"] || len(recorded) != 3 {
			t.Fatalf("recorded repositories %v", recorded)
		}
		for _, n := range []string{"linked", "loose", "vendor/lib"} {
			write(t, filepath.Join(top, filepath.FromSlash(n), "a.txt"), "changed\n")
		}
		d := CompareEnvironment(before, snap(t, "after", home, []string{top}, nil), before.Roots)
		changed := map[string][]string{}
		for _, c := range d.Protected {
			if c.Class == "repository" {
				changed[filepath.Base(c.Path)] = c.Fields
			}
		}
		if !slices.Contains(changed["top"], "status") || !slices.Contains(changed["loose"], "status") || !slices.Contains(changed["linked"], "status") ||
			len(changed) != 3 {
			t.Fatalf("repository changes %v (the ignored nested repository is not separately protected)", changed)
		}
		if !strings.Contains(RenderText(Report{}), "not covered: "+NestedRepoLimitation) {
			t.Fatal("the report does not state the nested-repository limitation")
		}
	})
	t.Run("dirty gitlink", func(t *testing.T) {
		t.Parallel()
		// C7: a tracked gitlink already dirty before the run keeps the same
		// parent status line when another tracked file inside it changes;
		// its own repository record catches the change.
		top := filepath.Join(t.TempDir(), "top")
		newRepo(t, top)
		sub := filepath.Join(top, "sub")
		newRepo(t, sub)
		write(t, filepath.Join(sub, "b.txt"), "bbbb\n")
		gitIn(t, sub, "add", "b.txt")
		gitIn(t, sub, "commit", "-q", "-m", "b")
		gitIn(t, top, "add", "sub")
		gitIn(t, top, "commit", "-q", "-m", "gitlink")
		write(t, filepath.Join(sub, "a.txt"), "dirty before\n")
		home := t.TempDir()
		before := snap(t, "before", home, []string{top}, nil)
		write(t, filepath.Join(sub, "b.txt"), "BBBB\n")
		after := snap(t, "after", home, []string{top}, nil)
		d := CompareEnvironment(before, after, before.Roots)
		var paths []string
		for _, c := range d.Protected {
			paths = append(paths, filepath.Base(c.Path))
		}
		if !slices.Contains(d.Codes, CodeEnvModified) || !slices.Contains(paths, "sub") {
			t.Fatalf("the change inside the dirty gitlink was missed: %v %+v", d.Codes, d.Protected)
		}
	})
	t.Run("streamed tracked digest", func(t *testing.T) {
		t.Parallel()
		// A3.2: a tracked listing far over the old 8 MiB output cap is one
		// streamed digest, no per-file record; any tracked change alters it.
		root := t.TempDir()
		repo := filepath.Join(root, "big")
		newRepo(t, repo)
		deep := filepath.Join(repo, strings.Repeat("d", 150), strings.Repeat("e", 150), strings.Repeat("f", 150), strings.Repeat("g", 150))
		if err := os.MkdirAll(deep, 0o755); err != nil {
			t.Fatal(err)
		}
		for i := 0; i < 12500; i++ {
			if err := os.WriteFile(filepath.Join(deep, fmt.Sprintf("%s%05d", strings.Repeat("n", 90), i)), nil, 0o644); err != nil {
				t.Fatal(err)
			}
		}
		gitIn(t, repo, "add", "-A")
		gitIn(t, repo, "commit", "-q", "-m", "big")
		if n := len(gitIn(t, repo, "ls-files", "--stage", "-z")); n <= maxGitOutput {
			t.Fatalf("the listing is only %d bytes", n)
		}
		home := t.TempDir()
		before := snap(t, "before", home, []string{root}, nil)
		shards, err := encodeSnapshot(before)
		if err != nil || len(shards[0]) > 64<<10 || len(before.Records) > 10 {
			t.Fatalf("snapshot of %d records, %d bytes: %v", len(before.Records), len(shards[0]), err)
		}
		os.WriteFile(filepath.Join(deep, strings.Repeat("n", 90)+"00042"), []byte("x"), 0o644)
		d := CompareEnvironment(before, snap(t, "after", home, []string{root}, nil), before.Roots)
		if c, ok := changeOf(d.Protected, before.Records[len(before.Records)-1].Path); !ok || !slices.Contains(c.Fields, "tracked") || !slices.Contains(d.Codes, CodeEnvModified) {
			t.Fatalf("a tracked change: %+v", d)
		}
	})
	t.Run("depth bound", func(t *testing.T) {
		t.Parallel()
		root := t.TempDir()
		newRepo(t, filepath.Join(root, "x", "atdepth"))
		newRepo(t, filepath.Join(root, "x", "y", "below"))
		home := t.TempDir()
		take := func(phase string) *Snapshot {
			s := takeSnapshotDepth(phase, home, []string{root}, nil, realGit(t), pathTokenizer{}, realClock{}, time.Time{}, 2)
			if !s.Complete || s.Depth != 2 {
				t.Fatalf("snapshot %+v", s)
			}
			return s
		}
		before := take("before")
		var repos []string
		for _, r := range before.Records {
			if r.Kind == RecRepo {
				repos = append(repos, filepath.Base(r.Path))
			}
		}
		if !slices.Equal(repos, []string{"atdepth"}) {
			t.Fatalf("repositories within depth 2: %v", repos)
		}
		write(t, filepath.Join(root, "x", "atdepth", "a.txt"), "changed\n")
		write(t, filepath.Join(root, "x", "y", "below", "a.txt"), "changed\n")
		d := CompareEnvironment(before, take("after"), before.Roots)
		if len(d.Protected) != 1 || filepath.Base(d.Protected[0].Path) != "atdepth" {
			t.Fatalf("protected changes %+v (the deeper repository is not protected)", d.Protected)
		}
		other := takeSnapshotDepth("after", home, []string{root}, nil, realGit(t), pathTokenizer{}, realClock{}, time.Time{}, 3)
		if d := CompareEnvironment(before, other, before.Roots); d.Complete || !slices.Contains(d.Codes, CodeEnvUnobserved) {
			t.Fatalf("snapshots of different depths compared: %+v", d.Codes)
		}
		if !strings.Contains(RenderText(Report{ProtectDepth: 2}), "protected depth: repositories at most 2 directory levels below each protected root") {
			t.Fatal("the report does not state the depth")
		}
	})
	t.Run("invalid repositories", func(t *testing.T) {
		t.Parallel()
		// A3.2: a .git Git rejects is recorded by its metadata; the scan
		// completes; any change to that metadata fails.
		root := t.TempDir()
		os.MkdirAll(filepath.Join(root, "emptydir", ".git"), 0o755)
		write(t, filepath.Join(root, "badfile", ".git"), "gitdir: /nowhere\n")
		home := t.TempDir()
		before := snap(t, "before", home, []string{root}, nil)
		kinds := map[string]string{}
		for _, r := range before.Records {
			if r.Kind == RecInvalid {
				kinds[filepath.Base(r.Path)] = r.Type
			}
		}
		if kinds["emptydir"] != TypeDir || kinds["badfile"] != TypeFile || len(kinds) != 2 {
			t.Fatalf("invalid repositories %v", kinds)
		}
		if d := CompareEnvironment(before, snap(t, "after", home, []string{root}, nil), before.Roots); len(d.Protected) != 0 {
			t.Fatalf("an unchanged invalid repository differs: %+v", d.Protected)
		}
		write(t, filepath.Join(root, "emptydir", ".git", "HEAD"), "ref: refs/heads/main\n")
		write(t, filepath.Join(root, "badfile", ".git"), "gitdir: /elsewhere\n")
		d := CompareEnvironment(before, snap(t, "after", home, []string{root}, nil), before.Roots)
		got := map[string][]string{}
		for _, c := range d.Protected {
			got[filepath.Base(c.Path)] = c.Fields
		}
		if !slices.Contains(got["emptydir"], "content") || !slices.Contains(got["badfile"], "size") || !slices.Contains(d.Codes, CodeEnvModified) {
			t.Fatalf("invalid repository changes %v", got)
		}
	})
	t.Run("metadata and named content fingerprints", func(t *testing.T) {
		t.Parallel()
		// A3.3: every vendor-home entry by metadata; content only for the
		// named small configuration files.
		home := t.TempDir()
		blob := filepath.Join(home, ".codex", "packages", "standalone", "codex.bin")
		write(t, blob, strings.Repeat("b", 3<<20))
		cache := filepath.Join(home, ".grok", "downloads", "big.tar")
		write(t, cache, "")
		truncateTo(t, cache, 70<<20) // never read
		settings := filepath.Join(home, ".claude", "settings.json")
		write(t, settings, `{"a":1}`)
		snapshot := filepath.Join(home, ".claude", "flow-snapshots", "s.json")
		write(t, snapshot, "snapshot-1")
		write(t, filepath.Join(home, ".claude", "CLAUDE.md"), strings.Repeat("i", maxConfigFile+1))
		write(t, filepath.Join(home, ".claude", "history.jsonl"), "1\n")
		write(t, filepath.Join(home, ".codex", "prompts", "review.md"), "p")
		write(t, filepath.Join(home, ".cursor", "cli-config.json"), "{}")
		sc := &envScanner{clock: realClock{}, snap: Snapshot{Complete: true}, repoSeen: map[string]bool{}, home: home, tok: pathTokenizer{home: home}}
		if err := sc.scanHomes(); err != nil || sc.hashed > 64 {
			t.Fatalf("scan %v, %d content bytes read", err, sc.hashed)
		}
		before := &sc.snap
		method := map[string]EnvRecord{}
		for _, r := range before.Records {
			if r.Kind == RecEntry {
				method[r.Scope+"/"+r.Path] = r
			}
		}
		for path, want := range map[string]string{"codex/packages/standalone/codex.bin": MethodMetadata, "grok/downloads/big.tar": MethodMetadata,
			"claude/settings.json": MethodContent, "claude/flow-snapshots/s.json": MethodMetadata, "claude/CLAUDE.md": MethodMetadata,
			"codex/prompts/review.md": MethodContent, "cursor/cli-config.json": MethodContent} {
			if r := method[path]; r.Method != want || (want == MethodContent) != (r.SHA256 != "") || r.CtimeNS == 0 {
				t.Errorf("%s = %+v, want %s", path, r, want)
			}
		}
		if !method["claude/CLAUDE.md"].Oversized {
			t.Fatal("the oversized named file is not marked")
		}
		// Changes: a grown blob; a same-size rewrite of a metadata-only file
		// with its mtime restored (only ctime moves); a same-size content
		// change of a named file with its mtime restored; housekeeping.
		write(t, blob, strings.Repeat("b", 3<<20+1))
		keep := func(p, content string) {
			st, _ := os.Lstat(p)
			write(t, p, content)
			os.Chtimes(p, st.ModTime(), st.ModTime())
		}
		// Rewrite until the status-change time moves past the recorded one
		// (coarse filesystem timestamps), with no wall-clock sleep.
		for _, c := range []struct{ path, content, key string }{{snapshot, "snapshot-2", "claude/flow-snapshots/s.json"}, {settings, `{"a":2}`, "claude/settings.json"}} {
			for i := 0; ; i++ {
				keep(c.path, c.content)
				st, _ := os.Lstat(c.path)
				var r EnvRecord
				statRecord(&r, st)
				if r.CtimeNS != method[c.key].CtimeNS {
					break
				}
				if i > 1000000 {
					t.Fatal("the status-change time never moved")
				}
			}
		}
		write(t, filepath.Join(home, ".claude", "history.jsonl"), "1\n2\n")
		after := takeSnapshot("after", home, nil, nil, nil, pathTokenizer{home: home}, realClock{}, time.Time{})
		d := CompareEnvironment(before, after, nil)
		fields := map[string][]string{}
		for _, dt := range d.Detail {
			c, _ := classifyChange(dt.Before, dt.After)
			fields[c.Scope+"/"+c.Path] = c.Fields
		}
		if !slices.Contains(fields["codex/packages/standalone/codex.bin"], "size") ||
			!slices.Equal(fields["claude/flow-snapshots/s.json"], []string{"ctime"}) ||
			!slices.Contains(fields["claude/settings.json"], "content") || slices.Contains(fields["claude/settings.json"], "size") ||
			!slices.Contains(d.Codes, CodeEnvModified) {
			t.Fatalf("changed fields %v", fields)
		}
		// A3.6: only the named file is protected; the blob, the snapshot
		// and the history are tool-maintained.
		if len(d.Protected) != 1 || d.Protected[0].Path != "settings.json" || d.ToolDetailCount != 3 || len(d.ToolSummary) != 3 {
			t.Fatalf("tool summary %+v protected %+v", d.ToolSummary, d.Protected)
		}
		if !slices.Equal(d.OversizedConfig, []string{"claude/CLAUDE.md"}) {
			t.Fatalf("oversized %v", d.OversizedConfig)
		}
		if text := RenderText(Report{OversizedConfig: d.OversizedConfig}); !strings.Contains(text, "configuration file over 1 MiB, fingerprinted by metadata only: claude/CLAUDE.md") {
			t.Fatal(text)
		}
		// The 64 MiB content budget applies to named files only.
		named := t.TempDir()
		for i := 0; i < 65; i++ {
			p := filepath.Join(named, ".claude", "skills", "s", fmt.Sprintf("f%02d", i))
			write(t, p, "")
			truncateTo(t, p, maxConfigFile)
		}
		if s := takeSnapshot("before", named, nil, nil, nil, pathTokenizer{}, realClock{}, time.Time{}); s.Complete || s.Code != CodeEnvLimit {
			t.Fatalf("named content over 64 MiB: %+v", s.Code)
		}
	})
	t.Run("symlinks are never followed", func(t *testing.T) {
		t.Parallel()
		// A3.4: in a vendor home and under a protected root a symlink is
		// the link itself (target text and its own metadata); a dangling
		// link is valid; a directory link is not traversed; a retargeted,
		// removed or new link fails.
		home, outside, root := t.TempDir(), t.TempDir(), t.TempDir()
		write(t, filepath.Join(outside, "skills", "s", "SKILL.md"), "skill")
		newRepo(t, filepath.Join(outside, "elsewhere"))
		write(t, filepath.Join(home, ".claude", "settings.json"), "{}")
		os.MkdirAll(filepath.Join(home, ".claude", "debug"), 0o700)
		os.MkdirAll(filepath.Join(home, ".codex", "app-server-control"), 0o700)
		os.MkdirAll(filepath.Join(root, "sub"), 0o700)
		links := map[string]string{
			filepath.Join(home, ".claude", "skills"):                                       filepath.Join(outside, "skills"),
			filepath.Join(home, ".claude", "debug", "latest"):                              filepath.Join(home, ".claude", "debug", "0f1e2d3c.txt"),
			filepath.Join(home, ".codex", "app-server-control", "app-server-control.sock"): filepath.Join(outside, "daemon", "gone.sock"),
			filepath.Join(root, "alias"):                                                   filepath.Join(outside, "elsewhere"),
			filepath.Join(root, "dangling"):                                                filepath.Join(root, "missing"),
			filepath.Join(root, "sub", "deep"):                                             filepath.Join(outside, "skills"),
		}
		for link, target := range links {
			if err := os.Symlink(target, link); err != nil {
				t.Fatal(err)
			}
		}
		newRepo(t, filepath.Join(root, "repo"))
		before := snap(t, "before", home, []string{root}, nil)
		got := map[string]EnvRecord{}
		for _, r := range before.Records {
			got[r.Kind+":"+r.Scope+":"+r.Path] = r
			if strings.HasPrefix(r.Path, "skills/") || strings.Contains(r.Path, "elsewhere") || strings.Contains(r.Path, "sub/deep/") {
				t.Errorf("a directory link was traversed: %+v", r)
			}
		}
		rootTok := pathTokenizer{home: home}.token(root)
		for key, want := range map[string]struct{ target, class string }{
			"entry:claude:skills":                                    {filepath.Join(outside, "skills"), ClassOwner},
			"entry:claude:debug/latest":                              {filepath.Join(home, ".claude", "debug", "0f1e2d3c.txt"), ClassTool},
			"entry:codex:app-server-control/app-server-control.sock": {filepath.Join(outside, "daemon", "gone.sock"), ClassTool},
			"link::" + rootTok + "/alias":                            {filepath.Join(outside, "elsewhere"), ClassLink},
			"link::" + rootTok + "/dangling":                         {filepath.Join(root, "missing"), ClassLink},
			"link::" + rootTok + "/sub/deep":                         {filepath.Join(outside, "skills"), ClassLink},
		} {
			r, ok := got[key]
			if !ok || r.Type != TypeSymlink || r.Target != want.target || r.Class != want.class || r.Method != MethodMetadata || r.SHA256 != "" ||
				r.Size != int64(len(want.target)) || r.CtimeNS == 0 {
				t.Errorf("%s = %+v, want the link to %s", key, r, want.target)
			}
		}
		repos := 0
		for _, r := range before.Records {
			if r.Kind == RecRepo {
				repos++
			}
		}
		if repos != 1 {
			t.Fatalf("%d repositories (a link to a repository is not followed)", repos)
		}
		// Changes behind a link are not observed through it.
		write(t, filepath.Join(outside, "skills", "s", "SKILL.md"), "SKILL")
		write(t, filepath.Join(outside, "elsewhere", "a.txt"), "changed\n")
		if d := CompareEnvironment(before, snap(t, "after", home, []string{root}, nil), before.Roots); len(d.Protected)+len(d.Detail) != 0 || !d.Complete {
			t.Fatalf("a change behind a link was observed: %+v", d)
		}
		// Retargeted, removed and new links fail, except tool-maintained
		// ones (A3.6).
		relink := func(link, target string) {
			os.Remove(link)
			if err := os.Symlink(target, link); err != nil {
				t.Fatal(err)
			}
		}
		relink(filepath.Join(home, ".claude", "skills"), filepath.Join(outside, "other"))
		relink(filepath.Join(root, "alias"), filepath.Join(outside, "other"))
		os.Remove(filepath.Join(home, ".codex", "app-server-control", "app-server-control.sock"))
		os.Remove(filepath.Join(root, "dangling"))
		os.Symlink(outside, filepath.Join(root, "new"))
		d := CompareEnvironment(before, snap(t, "after", home, []string{root}, nil), before.Roots)
		for path, kind := range map[string]string{"skills": ChangeModified, rootTok + "/alias": ChangeModified,
			rootTok + "/dangling": ChangeDeleted, rootTok + "/new": ChangeAdded} {
			c, ok := changeOf(d.Protected, path)
			if !ok || c.Kind != kind || (kind == ChangeModified && !slices.Contains(c.Fields, "target")) {
				t.Errorf("%s = %+v, want protected %s", path, c, kind)
			}
		}
		if len(d.Protected) != 4 || !slices.Equal(d.ToolSummary, []ToolSummaryRow{{Scope: "codex", TopLevel: "app-server-control", Deleted: 1, Total: 1}}) ||
			!slices.Equal(d.Codes, []string{CodeEnvModified}) {
			t.Fatalf("protected %+v tool %+v codes %v", d.Protected, d.ToolSummary, d.Codes)
		}
	})
	t.Run("home and exclusion through a symlinked parent", func(t *testing.T) {
		t.Parallel()
		// The macOS shape: a home reached through a symlinked ancestor or
		// a linked vendor home is tokenized; a link's class never depends
		// on its target (A3.6: debug/latest is tool-maintained wherever it
		// points).
		home := linkedParent(t, t.TempDir())
		ch, _ := filepath.EvalSymlinks(home)
		write(t, filepath.Join(home, ".claude", "debug", "s1.txt"), "d")
		other := t.TempDir()
		write(t, filepath.Join(other, ".claude", "debug", "s1.txt"), "d")
		// A Claude home that is itself a link: the target spells it
		// through that link.
		lhome, real := t.TempDir(), t.TempDir()
		write(t, filepath.Join(real, "debug", "s1.txt"), "d")
		os.Symlink(real, filepath.Join(lhome, ".claude"))
		for _, c := range []struct {
			home, target, class string
		}{
			{home, "s1.txt", ClassTool},
			{home, filepath.Join(home, ".claude", "debug", "s1.txt"), ClassTool},
			{home, filepath.Join(ch, ".claude", "debug", "s1.txt"), ClassTool},
			{lhome, filepath.Join(lhome, ".claude", "debug", "s1.txt"), ClassTool},
			{lhome, filepath.Join(real, "debug", "s1.txt"), ClassTool},
			{home, filepath.Join(other, ".claude", "debug", "s1.txt"), ClassTool},
			{home, filepath.Join(home, ".claude", "debug", "none.txt"), ClassTool},
		} {
			dir := filepath.Join(c.home, ".claude", "debug")
			os.Remove(filepath.Join(dir, "latest"))
			if err := os.Symlink(c.target, filepath.Join(dir, "latest")); err != nil {
				t.Fatal(err)
			}
			s := snap(t, "before", c.home, nil, nil)
			var got EnvRecord
			for _, r := range s.Records {
				if r.Kind == RecEntry && r.Scope == "claude" && r.Path == "debug/latest" {
					got = r
				}
				if r.Kind == RecHome && r.Path != "$HOME/."+r.Scope {
					t.Errorf("vendor home %+v is not tokenized", r)
				}
			}
			if got.Class != c.class {
				t.Errorf("debug/latest -> %s: class %q, want %q", c.target, got.Class, c.class)
			}
		}
		// The exact exclusion spelled through the link still excludes the
		// canonical paths discovery and status report below a root.
		root := linkedParent(t, t.TempDir())
		croot, _ := filepath.EvalSymlinks(root)
		newRepo(t, filepath.Join(root, "repo"))
		ev := filepath.Join(root, "repo", "ev")
		write(t, filepath.Join(ev, "a.txt"), "a")
		os.Symlink("a.txt", filepath.Join(ev, "link"))
		hb := t.TempDir()
		before := snap(t, "before", hb, []string{croot}, []string{ev})
		write(t, filepath.Join(ev, "b.txt"), "b")
		if d := CompareEnvironment(before, snap(t, "after", hb, []string{croot}, []string{ev}), before.Roots); len(d.Protected)+len(d.Detail) != 0 || !d.Complete {
			t.Errorf("a change inside the exclusion was observed: %+v", d.Protected)
		}
		plain := filepath.Join(root, "plain")
		write(t, filepath.Join(plain, "ev", "a.txt"), "a")
		before = snap(t, "before", hb, []string{filepath.Join(croot, "plain")}, []string{filepath.Join(plain, "ev")})
		os.Symlink("a.txt", filepath.Join(plain, "ev", "new"))
		if d := CompareEnvironment(before, snap(t, "after", hb, []string{filepath.Join(croot, "plain")}, []string{filepath.Join(plain, "ev")}), before.Roots); len(d.Protected)+len(d.Detail) != 0 {
			t.Fatalf("a link inside the exclusion was observed: %+v", d)
		}
	})
	t.Run("compact records", func(t *testing.T) {
		t.Parallel()
		// A3.4: a compact record changes whenever any field of the full
		// record (every metadata field, the content digest) changes.
		base := EnvRecord{Kind: RecEntry, Scope: "claude", Path: "settings.json", Class: ClassOwner, Method: MethodContent, Type: TypeFile, Mode: 0o600,
			Size: 2, MtimeNS: 1, CtimeNS: 2, Dev: 3, Ino: 4, SHA256: zeros64, Target: "t", GitDir: "g", CommonDir: "c", Head: "h", Branch: "b",
			StatusSHA256: "s", TrackedSHA256: "k", Depth: 1, Records: 1, Roots: []string{"r"}, Phase: "p", Schema: "x", Code: "c"}
		c0 := compactOf(base)
		if c0.Content != zeros64 || c0.Kind != RecEntry || c0.Scope != "claude" || c0.Path != "settings.json" || !hex64RE.MatchString(c0.Digest) ||
			c0.Class != ClassOwner || c0.Type != TypeFile {
			t.Fatalf("compact %+v", c0)
		}
		if compactOf(base) != c0 {
			t.Fatal("the compact record is not deterministic")
		}
		v := reflect.ValueOf(&base).Elem()
		for i := 0; i < v.NumField(); i++ {
			r := base
			r.Roots = slices.Clone(base.Roots)
			f := reflect.ValueOf(&r).Elem().Field(i)
			switch f.Kind() {
			case reflect.String:
				f.SetString(f.String() + "1")
			case reflect.Bool:
				f.SetBool(!f.Bool())
			case reflect.Int, reflect.Int64:
				f.SetInt(f.Int() + 1)
			case reflect.Uint32, reflect.Uint64:
				f.SetUint(f.Uint() + 1)
			case reflect.Slice:
				f.Set(reflect.Append(f, reflect.ValueOf("more")))
			default:
				t.Fatalf("field %s of kind %s not covered", v.Type().Field(i).Name, f.Kind())
			}
			if c := compactOf(r); c.Digest == c0.Digest {
				t.Errorf("a change of %s leaves the compact record's digest unchanged", v.Type().Field(i).Name)
			}
		}
		meta := base
		meta.Method = MethodMetadata
		if c := compactOf(meta); c.Content != "" {
			t.Fatalf("a metadata-only record carries content %q", c.Content)
		}
		// Size: one compact line is its path plus a fixed ~145 bytes (150
		// with the longest scope, class and newline), plus 71 for a content
		// digest.
		home := t.TempDir()
		for i := 0; i < 300; i++ {
			write(t, filepath.Join(home, ".claude", "flow-snapshots", fmt.Sprintf("run-%03d", i/30), fmt.Sprintf("%s-%d.json", strings.Repeat("s", i%90), i)), "x")
		}
		write(t, filepath.Join(home, ".claude", "settings.json"), "{}")
		write(t, filepath.Join(home, ".codex", "config.toml"), "x")
		s := snap(t, "before", home, nil, nil)
		shards, err := encodeSnapshot(s)
		if err != nil {
			t.Fatal(err)
		}
		lines := strings.Split(strings.TrimSuffix(string(shards[0])+string(shards[1]), "\n"), "\n")
		if len(lines) != len(s.Records)+1 {
			t.Fatalf("%d lines for %d records", len(lines), len(s.Records))
		}
		const overhead, contentOverhead = 150, 71
		bound := len(lines[0]) + 1
		for i, r := range s.Records {
			path, _ := json.Marshal(r.Path)
			limit := len(path) - 2 + overhead
			if r.Method == MethodContent {
				limit += contentOverhead
			}
			if n := len(lines[i+1]) + 1; n > limit {
				t.Errorf("record %s is %d bytes, over its %d-byte bound", r.Path, n, limit)
			}
			bound += limit
			// A directory's identity-only full record is already small.
			if full, _ := json.Marshal(r); len(full) <= len(lines[i+1]) && r.Kind == RecEntry && r.Type == TypeFile {
				t.Errorf("the compact record of %s (%d bytes) is not smaller than the full one (%d)", r.Path, len(lines[i+1]), len(full))
			}
		}
		if total := len(shards[0]) + len(shards[1]); total > bound {
			t.Fatalf("snapshot %d bytes over the structural bound %d", total, bound)
		}
		// diff.json: full detail for exactly the changed, added and deleted
		// entries; the checker's derivation from the compact snapshots and
		// that detail reproduces the supervisor's diff.
		write(t, filepath.Join(home, ".claude", "settings.json"), `{"a":1}`)
		write(t, filepath.Join(home, ".claude", "history.jsonl"), "1\n")
		os.Remove(filepath.Join(home, ".codex", "config.toml"))
		after := snap(t, "after", home, nil, nil)
		d := CompareEnvironment(s, after, nil)
		sides := map[string]string{}
		for _, e := range d.Detail {
			k := ""
			switch {
			case e.Before != nil && e.After != nil:
				k = e.Before.Path + ":changed"
			case e.Before != nil:
				k = e.Before.Path + ":deleted"
			default:
				k = e.After.Path + ":added"
			}
			sides[k] = ""
		}
		if len(d.Detail) != 3 || len(sides) != 3 {
			t.Fatalf("detail %v", sides)
		}
		for _, k := range []string{"settings.json:changed", "history.jsonl:added", "config.toml:deleted"} {
			if _, ok := sides[k]; !ok {
				t.Errorf("detail lacks %s: %v", k, sides)
			}
		}
		if d.ToolDetailCount != 1 || d.ToolOmittedCount != 0 || len(d.Protected) != 2 {
			t.Fatalf("protected %+v tool detail %d", d.Protected, d.ToolDetailCount)
		}
		parse := func(snap *Snapshot) *Snapshot {
			shards, err := encodeSnapshot(snap)
			if err != nil {
				t.Fatal(err)
			}
			p, err := parseSnapshot(shards)
			if err != nil {
				t.Fatal(err)
			}
			return p
		}
		pb, pa := parse(s), parse(after)
		if err := verifyDetail(pb, pa, d.Detail); err != nil {
			t.Fatal(err)
		}
		if got := diffFromDetail(pb, pa, nil, d.Detail); string(mustJSON(got)) != string(mustJSON(d)) {
			t.Fatalf("the checker's derivation differs:\n%s\n%s", mustJSON(got), mustJSON(d))
		}
		if data, _, err := encodeDiff(d); err != nil || !strings.Contains(string(data), `"detail"`) {
			t.Fatalf("diff.json %v", err)
		}
		// Detail not bound to the snapshots.
		clone := func() []EnvDetail {
			var out []EnvDetail
			for _, e := range d.Detail {
				c := EnvDetail{}
				if e.Before != nil {
					b := *e.Before
					c.Before = &b
				}
				if e.After != nil {
					a := *e.After
					c.After = &a
				}
				out = append(out, c)
			}
			return out
		}
		unchanged := s.Records[0]
		for name, detail := range map[string]func() []EnvDetail{
			"altered": func() []EnvDetail {
				x := clone()
				for _, e := range x {
					if e.Before != nil {
						e.Before.Size++
						break
					}
				}
				return x
			},
			"dropped":   func() []EnvDetail { x := clone(); return x[:len(x)-1] },
			"unchanged": func() []EnvDetail { return append(clone(), EnvDetail{Before: &unchanged, After: &unchanged}) },
			"reordered": func() []EnvDetail { x := clone(); x[0], x[1] = x[1], x[0]; return x },
			"side":      func() []EnvDetail { x := clone(); x[0].After, x[0].Before = x[0].Before, nil; return x },
		} {
			if err := verifyDetail(pb, pa, detail()); err == nil {
				t.Errorf("%s detail verified", name)
			}
		}
		// The tool-maintained history record is optional detail (A3.6):
		// omitting it is a valid, counted prefix.
		if d.Detail[0].After.Path != "history.jsonl" || verifyDetail(pb, pa, clone()[1:]) != nil {
			t.Fatal("an omitted tool detail record was refused")
		}
		if got := diffFromDetail(pb, pa, nil, clone()[1:]); got.ToolDetailCount != 0 || got.ToolOmittedCount != 1 || len(got.Protected) != 2 {
			t.Fatalf("omitted tool detail %+v", got)
		}
	})
	t.Run("collection failures", func(t *testing.T) {
		t.Parallel()
		// A3.4: a dangling vendor-home link or configuration link is valid,
		// recorded as the link.
		home := t.TempDir()
		os.Symlink(filepath.Join(home, "nowhere"), filepath.Join(home, ".codex"))
		os.MkdirAll(filepath.Join(home, ".claude"), 0o700)
		os.Symlink(filepath.Join(home, "nowhere"), filepath.Join(home, ".claude", "settings.json"))
		s := takeSnapshot("before", home, nil, nil, nil, pathTokenizer{}, realClock{}, time.Time{})
		targets := map[string]string{}
		for _, r := range s.Records {
			if r.Type == TypeSymlink {
				targets[r.Kind+":"+r.Scope] = r.Target
			}
		}
		if !s.Complete || len(targets) != 2 || targets["home:codex"] != filepath.Join(home, "nowhere") || targets["entry:claude"] != filepath.Join(home, "nowhere") {
			t.Fatalf("dangling links: %+v %v", s, targets)
		}
		// A link that cannot be read is unobserved, not skipped.
		sc := &envScanner{clock: realClock{}, snap: Snapshot{Complete: true}, tok: pathTokenizer{}}
		if err := sc.rootLink(filepath.Join(home, "missing")); err == nil || sc.snap.Code != CodeEnvUnobserved {
			t.Fatal("an unreadable root link")
		}
		// Deadlines, entry, byte, repository and read bounds.
		clock := newAutoClock()
		if s := takeSnapshot("before", t.TempDir(), nil, nil, nil, pathTokenizer{}, clock, clock.Now().Add(-time.Second)); s.Complete || s.Code != CodeEnvUnobserved {
			t.Fatalf("past the deadline: %+v", s)
		}
		sc = &envScanner{clock: realClock{}, snap: Snapshot{Complete: true}}
		sc.entries = maxEnvEntries
		if err := sc.add(EnvRecord{Kind: RecEntry}); err != nil || sc.entries != maxEnvEntries {
			t.Fatalf("an entry readDir already counted was counted again: %v %d", err, sc.entries)
		}
		if err := sc.add(EnvRecord{Kind: RecHome}); !errors.Is(err, errSnapshotIncomplete) || sc.snap.Code != CodeEnvLimit {
			t.Fatalf("entry bound: %v %s", err, sc.snap.Code)
		}
		sc = &envScanner{clock: realClock{}, snap: Snapshot{Complete: true}, bytes: maxSnapshotBytes - 4096}
		if err := sc.add(EnvRecord{Kind: RecEntry, Path: "x"}); err == nil || sc.snap.Code != CodeEnvLimit {
			t.Fatal("byte bound")
		}
		big := filepath.Join(t.TempDir(), "big")
		truncateTo(t, big, maxConfigFile+1)
		sc = &envScanner{clock: realClock{}, snap: Snapshot{Complete: true}}
		if _, err := sc.hashFile(big, maxConfigFile+1); err == nil || sc.snap.Code != CodeEnvLimit {
			t.Fatal("file read bound")
		}
		sc = &envScanner{clock: realClock{}, snap: Snapshot{Complete: true}, hashed: maxConfigTotal - 1}
		small := filepath.Join(t.TempDir(), "s")
		os.WriteFile(small, []byte("xx"), 0o600)
		if _, err := sc.hashFile(small, 2); err == nil || sc.snap.Code != CodeEnvLimit {
			t.Fatal("total read bound")
		}
		sc = &envScanner{clock: realClock{}, snap: Snapshot{Complete: true}}
		if _, err := sc.hashFile(filepath.Join(t.TempDir(), "missing"), 1); err == nil || sc.snap.Code != CodeEnvUnobserved {
			t.Fatal("unreadable configuration")
		}
		sc = &envScanner{clock: realClock{}, snap: Snapshot{Complete: true}, repos: maxEnvRepos, repoSeen: map[string]bool{}}
		if err := sc.repo(t.TempDir(), false); err == nil || sc.snap.Code != CodeEnvLimit {
			t.Fatal("repository bound")
		}
		// A probe whose output passes its bound is an overflow
		// (environment_snapshot_limit), not an unreadable repository.
		huge := func(string, io.Writer, ...string) ([]byte, int, error) { return nil, 0, errGitOutputLimit }
		hroot := t.TempDir()
		newRepo(t, filepath.Join(hroot, "r"))
		if s := takeSnapshot("before", t.TempDir(), []string{hroot}, nil, huge, pathTokenizer{}, realClock{}, time.Time{}); s.Complete || s.Code != CodeEnvLimit {
			t.Fatalf("an oversized probe: %+v", s)
		}
		// The live runner: output over the bound, no git, a failed start.
		bigOut := &fakeLauncher{script: func(ProcSpec) behavior { return behavior{stdout: strings.Repeat("x", maxGitOutput+1)} }}
		look := func(string) (string, error) { return "/fake/git", nil }
		if _, _, err := execGit(bigOut, newAutoClock(), look, nil)(t.TempDir(), nil, "ls-files"); !errors.Is(err, errGitOutputLimit) {
			t.Fatalf("oversized captured output = %v", err)
		}
		// A3.2: the same output streamed is not capped; all of it arrives.
		var counted countWriter
		if _, code, err := execGit(bigOut, newAutoClock(), look, nil)(t.TempDir(), &counted, "ls-files"); err != nil || code != 0 || counted != maxGitOutput+1 {
			t.Fatalf("streamed output = %d %v (%d bytes)", code, err, counted)
		}
		if _, _, err := execGit(bigOut, newAutoClock(), func(string) (string, error) { return "", errors.New("no git") }, nil)(t.TempDir(), nil); err == nil {
			t.Fatal("ran without git")
		}
		killed := &fakeLauncher{script: func(ProcSpec) behavior { return behavior{long: true} }}
		if _, _, err := execGit(killed, newAutoClock(), look, []string{"GIT_DIR=/x", "PATH=/bin"})(t.TempDir(), nil, "status"); err == nil || errors.Is(err, errGitOutputLimit) {
			t.Fatalf("a timed-out probe = %v", err)
		}
		failing := func(string, io.Writer, ...string) ([]byte, int, error) { return nil, 0, errors.New("git broke") }
		root := t.TempDir()
		newRepo(t, filepath.Join(root, "r"))
		if s := takeSnapshot("before", t.TempDir(), []string{root}, nil, failing, pathTokenizer{}, realClock{}, time.Time{}); s.Complete || s.Code != CodeEnvUnobserved {
			t.Fatalf("a failing probe: %+v", s)
		}
	})
	t.Run("tool detail within the diff bound", func(t *testing.T) {
		t.Parallel()
		// A3.6: all strict detail, the summary and the counts always; of
		// the tool-maintained detail the longest whole-record prefix that
		// keeps diff.json within 4 MiB, the rest counted as omitted, and a
		// checker derivation that accepts exactly that.
		rec := func(path string, size int64, class, sha string) EnvRecord {
			return EnvRecord{Kind: RecEntry, Scope: "claude", Path: path, Class: class, Method: MethodMetadata, Type: TypeFile, Mode: 0o600, Size: size,
				SHA256: sha}
		}
		build := func(n int) (*Snapshot, *Snapshot) {
			before := &Snapshot{Phase: "before", Complete: true, Roots: []string{}}
			after := &Snapshot{Phase: "after", Complete: true, Roots: []string{}}
			strict := rec("settings.json", 1, ClassOwner, zeros64)
			before.Records = append(before.Records, strict)
			strict.Size, strict.SHA256 = 2, strings.Repeat("1", 64)
			after.Records = append(after.Records, strict)
			for i := 0; i < n; i++ {
				p := fmt.Sprintf("projects/%05d-%s.jsonl", i, strings.Repeat("p", 3000))
				before.Records = append(before.Records, rec(p, 1, ClassTool, ""))
				after.Records = append(after.Records, rec(p, 2, ClassTool, ""))
			}
			return before, after
		}
		check := func(name string, n int, full bool) EnvDiff {
			before, after := build(n)
			d := CompareEnvironment(before, after, nil)
			d.Before, d.After = []FileHash{}, []FileHash{}
			data, fitted, err := encodeDiff(d)
			if err != nil || len(data) > maxDiffBytes || fitted.ToolDetailCount+fitted.ToolOmittedCount != n ||
				!slices.Equal(fitted.ToolSummary, []ToolSummaryRow{{Scope: "claude", TopLevel: "projects", Changed: n, Total: n}}) {
				t.Fatalf("%s: %v %d bytes, %d+%d", name, err, len(data), fitted.ToolDetailCount, fitted.ToolOmittedCount)
			}
			if !full {
				return fitted
			}
			var stored EnvDiff
			if err := decodeStrict(data, &stored); err != nil {
				t.Fatal(err)
			}
			last := stored.Detail[len(stored.Detail)-1]
			if last.Before.Path != "settings.json" || len(stored.Protected) != 1 || len(stored.Detail) != 1+stored.ToolDetailCount {
				t.Fatalf("%s: the strict detail is not retained", name)
			}
			for i, dt := range stored.Detail[:len(stored.Detail)-1] {
				if !strings.HasPrefix(dt.Before.Path, fmt.Sprintf("projects/%05d-", i)) {
					t.Fatalf("%s: detail %d is not the key-ordered prefix", name, i)
				}
			}
			// The checker's derivation, from the compact inventories it
			// reads, accepts the prefix and reproduces the counts.
			cb, ca := *before, *after
			cb.Compact, cb.Records = before.compactRecords(), nil
			ca.Compact, ca.Records = after.compactRecords(), nil
			if err := verifyDetail(&cb, &ca, stored.Detail); err != nil {
				t.Fatalf("%s: %v", name, err)
			}
			got := diffFromDetail(&cb, &ca, nil, stored.Detail)
			got.Before, got.After = stored.Before, stored.After
			if !reflect.DeepEqual(got, stored) {
				t.Fatalf("%s: the derivation differs", name)
			}
			// One more whole tool record would not fit.
			if stored.ToolOmittedCount > 0 {
				more := d
				more.Detail = append(slices.Clone(d.Detail[:stored.ToolDetailCount+1]), d.Detail[len(d.Detail)-1])
				more.ToolDetailCount, more.ToolOmittedCount = stored.ToolDetailCount+1, stored.ToolOmittedCount-1
				if b, _ := encodeJSON(more); len(b) <= maxDiffBytes {
					t.Fatalf("%s: a fitting record was omitted (%d bytes)", name, len(b))
				}
			}
			return stored
		}
		if d := check("below", 30, true); d.ToolOmittedCount != 0 || d.ToolDetailCount != 30 {
			t.Fatalf("below the bound %d omitted", d.ToolOmittedCount)
		}
		at := check("above", 700, true)
		if at.ToolOmittedCount == 0 || at.ToolDetailCount == 0 {
			t.Fatalf("over the bound: %d kept, %d omitted", at.ToolDetailCount, at.ToolOmittedCount)
		}
		// Exactly at the bound: the kept prefix alone, now complete.
		if d := check("at", at.ToolDetailCount, false); d.ToolOmittedCount != 0 || d.ToolDetailCount != at.ToolDetailCount {
			t.Fatalf("at the bound %d omitted", d.ToolOmittedCount)
		}
		// Verification refuses a non-prefix, a content digest of tool
		// state or a dropped strict record.
		before, after := build(3)
		full := CompareEnvironment(before, after, nil).Detail
		if verifyDetail(before, after, full) != nil || verifyDetail(before, after, []EnvDetail{full[0], full[3]}) != nil {
			t.Fatal("a valid prefix was refused")
		}
		if verifyDetail(before, after, []EnvDetail{full[0], full[2], full[3]}) == nil || verifyDetail(before, after, full[1:]) == nil ||
			verifyDetail(before, after, full[:3]) == nil {
			t.Fatal("a non-prefix or a detail without its strict record verified")
		}
		bad := *full[1].After
		bad.SHA256 = zeros64
		if verifyDetail(before, after, []EnvDetail{full[0], {Before: full[1].Before, After: &bad}, full[2], full[3]}) == nil {
			t.Fatal("a tool content digest verified")
		}
	})
	t.Run("shards and diff bounds", func(t *testing.T) {
		t.Parallel()
		s := &Snapshot{Phase: "before", Complete: true, Roots: []string{}}
		// The bounds are bytes, not records: long paths reach them with
		// few records.
		entry := func(i int) EnvRecord {
			return EnvRecord{Kind: RecEntry, Scope: "claude", Path: fmt.Sprintf("%04000d", i), Class: ClassOwner, Method: MethodContent, SHA256: zeros64}
		}
		line, _ := json.Marshal(compactOf(entry(0)))
		for i := 0; len(s.Records)*(len(line)+1) < 9<<20; i++ {
			s.Records = append(s.Records, entry(i))
		}
		shards, err := encodeSnapshot(s)
		if err != nil || len(shards[0]) > maxShardBytes || len(shards[1]) == 0 {
			t.Fatalf("two shards: %v %d %d", err, len(shards[0]), len(shards[1]))
		}
		back, err := parseSnapshot(shards)
		if err != nil || !slices.Equal(back.Compact, s.compactRecords()) || back.Records != nil {
			t.Fatalf("round trip %v", err)
		}
		for i := len(s.Records); len(s.Records)*(len(line)+1) < 17<<20; i++ {
			s.Records = append(s.Records, entry(i))
		}
		if _, err := encodeSnapshot(s); err == nil {
			t.Fatal("a snapshot over 16 MiB encoded")
		}
		head := `{"kind":"header","schema":"` + SnapshotSchema + `","phase":"before","complete":true,"roots":[],"depth":3,"records":1}` + "\n"
		rec := `{"k":"entry","s":"claude","p":"a","h":"` + zeros64 + `"}` + "\n"
		if back, err := parseSnapshot([][]byte{[]byte(head + rec)}); err != nil || len(back.Compact) != 1 || back.Depth != 3 {
			t.Fatalf("one compact record: %v", err)
		}
		head2 := strings.Replace(head, `"records":1`, `"records":2`, 1)
		for name, shards := range map[string][][]byte{
			"no header":     {[]byte(rec)},
			"two headers":   {[]byte(head + head)},
			"count":         {[]byte(head)},
			"no newline":    {[]byte(head + strings.TrimSuffix(rec, "\n"))},
			"unknown field": {[]byte(head + strings.Replace(rec, `"k"`, `"x":1,"k"`, 1))},
			"full record":   {[]byte(head + `{"kind":"entry","scope":"claude","path":"a"}` + "\n")},
			"header kind":   {[]byte(head + strings.Replace(rec, `"entry"`, `"header"`, 1))},
			"repeated":      {[]byte(head2 + rec + rec)},
			"digest":        {[]byte(head + strings.Replace(rec, zeros64, "xyz", 1))},
			"content":       {[]byte(head + strings.Replace(rec, `"}`, `","c":"short"}`, 1))},
			"empty":         {nil, nil},
			"oversized":     {make([]byte, maxShardBytes+1)},
			"phase":         {[]byte(strings.Replace(head, "before", "during", 1))},
			"version 1":     {[]byte(strings.Replace(head, "/v3", "/v1", 1) + rec)},
			"version 2":     {[]byte(strings.Replace(head, "/v3", "/v2", 1) + rec)},
		} {
			if _, err := parseSnapshot(shards); err == nil {
				t.Errorf("%s parsed", name)
			}
		}
		var d EnvDiff
		d.Schema = DiffSchema
		for len(d.Protected)*4000 < 5<<20 {
			d.Protected = append(d.Protected, EnvChange{Scope: "claude", Path: strings.Repeat("x", 4000), Kind: ChangeAdded, Fields: []string{}})
		}
		if _, _, err := encodeDiff(d); err == nil || err.Error() != CodeEnvLimit {
			t.Fatalf("a diff over 4 MiB encoded: %v", err)
		}
		// The evidence writer's category budgets: environment 36 MiB, the
		// other text 28 MiB, 64 MiB together; nothing past them is written.
		w, err := newEvidenceWriter(filepath.Join(t.TempDir(), "ev"), realClock{})
		if err != nil {
			t.Fatal(err)
		}
		defer w.close()
		if w.reserve("environment/before-00.jsonl", maxEnvironmentAll) != nil || w.reserve("environment/diff.json", 1) == nil ||
			w.reserve("report.txt", maxOtherText+1) == nil || w.reserve("report.txt", maxOtherText-1024) != nil || w.failure() == nil {
			t.Fatal("category budgets")
		}
		if maxEnvironmentAll != 36<<20 || maxOtherText != 28<<20 || maxEnvironmentAll+maxOtherText != maxTextTotal {
			t.Fatal("budget arithmetic")
		}
		// The bundle loader refuses an oversized diff or snapshot.
		for name, f := range map[string]func(b string){
			"diff":     func(b string) { truncateTo(t, filepath.Join(b, "environment", "diff.json"), maxDiffBytes+1) },
			"snapshot": func(b string) { truncateTo(t, filepath.Join(b, "environment", "after-01.jsonl"), maxShardBytes+1) },
		} {
			b := passingBundle(t)
			f(b)
			if code, _, _ := checkBundle(t, b); code != 2 {
				t.Errorf("%s = %d", name, code)
			}
		}
	})
}

// endlessDirs opens each directory in big as an endless directory of
// regular files, counting the entries handed out, and any other for real;
// a whole read (n <= 0) of an endless directory fails the test.
type endlessDirs struct {
	t    *testing.T
	big  map[string]bool
	read int
}

func (e *endlessDirs) open(p string) (dirReader, error) {
	if !e.big[p] {
		return openDir(p)
	}
	return endlessDir{e}, nil
}

type endlessDir struct{ e *endlessDirs }

func (d endlessDir) ReadDir(n int) ([]fs.DirEntry, error) {
	if n <= 0 {
		d.e.t.Error("an endless directory was read whole")
		return nil, errors.New("whole read")
	}
	out := make([]fs.DirEntry, n)
	for i := range out {
		out[i] = fakeDirEntry(fmt.Sprintf("f%09d", d.e.read))
		d.e.read++
	}
	return out, nil
}

func (endlessDir) Close() error { return nil }

// nestedDirs opens each directory in tree as a finite directory of that
// many entries, the first being the subdirectory "a" when tree holds it,
// counting the entries handed out; any other directory is opened for real.
type nestedDirs struct {
	tree map[string]int
	read int
}

func (n *nestedDirs) open(p string) (dirReader, error) {
	size, ok := n.tree[p]
	if !ok {
		return openDir(p)
	}
	_, sub := n.tree[filepath.Join(p, "a")]
	return &nestedDir{n: n, size: size, sub: sub}, nil
}

type nestedDir struct {
	n          *nestedDirs
	size, next int
	sub        bool
}

func (d *nestedDir) ReadDir(n int) ([]fs.DirEntry, error) {
	if d.next >= d.size {
		return nil, io.EOF
	}
	var out []fs.DirEntry
	for ; d.next < d.size && len(out) < n; d.next++ {
		if d.next == 0 && d.sub {
			out = append(out, fakeSubdir("a"))
		} else {
			out = append(out, fakeDirEntry(fmt.Sprintf("f%09d", d.next)))
		}
		d.n.read++
	}
	return out, nil
}

func (*nestedDir) Close() error { return nil }

// fakeSubdir is a directory's directory entry.
type fakeSubdir string

func (e fakeSubdir) Name() string             { return string(e) }
func (fakeSubdir) IsDir() bool                { return true }
func (fakeSubdir) Type() fs.FileMode          { return fs.ModeDir }
func (fakeSubdir) Info() (fs.FileInfo, error) { return nil, fs.ErrNotExist }

// fakeDirEntry is a regular file's directory entry.
type fakeDirEntry string

func (e fakeDirEntry) Name() string             { return string(e) }
func (fakeDirEntry) IsDir() bool                { return false }
func (fakeDirEntry) Type() fs.FileMode          { return 0 }
func (fakeDirEntry) Info() (fs.FileInfo, error) { return nil, fs.ErrNotExist }

// stepClock advances one second on every reading.
type stepClock struct{ now time.Time }

func (c *stepClock) Now() time.Time {
	c.now = c.now.Add(time.Second)
	return c.now
}

func (c *stepClock) NewTimer(time.Duration) (<-chan time.Time, func() bool) {
	return nil, func() bool { return false }
}

// countWriter counts the bytes written to it.
type countWriter int

func (c *countWriter) Write(p []byte) (int, error) {
	*c += countWriter(len(p))
	return len(p), nil
}
