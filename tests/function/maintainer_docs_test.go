package function

import (
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strings"
	"testing"

	"github.com/wedevwork/callsheet/internal/reale2e"
	"github.com/wedevwork/callsheet/internal/testkit"
)

// Iteration 12-1b function tests (design 12-1b-maintainer-docs): one
// top-level TestMaintainerDoc* per FP. The CI reference and the real
// end-to-end runbook live in docs/maintainers/; their old paths are gone;
// every incoming and outgoing link, source reader and path pin follows
// them. Each validator here is test-only, reads the checkout or in-memory
// copies, and is shown to reject its deliberate mutations. Nothing here
// contacts a network, launches a vendor CLI, runs git or modifies the
// checkout (layout mutations use temporary directories).

const (
	maintainerCI  = "docs/maintainers/ci.md"
	maintainerE2E = "docs/maintainers/real-e2e.md"
)

var (
	// maintainerDocs are the relocated documents; removedMaintainerDocs
	// their pre-relocation paths, which must not exist in any form.
	maintainerDocs        = []string{maintainerCI, maintainerE2E}
	removedMaintainerDocs = []string{"docs/ci.md", "docs/real-e2e.md"}
	// realE2EExerciseInputs are the exercise's harness inputs, which stay
	// at docs/examples/real-e2e/ under unchanged names.
	realE2EExerciseInputs = []string{
		"docs/examples/real-e2e/README.md",
		"docs/examples/real-e2e/dev-full/coordinator-prompt.md",
		"docs/examples/real-e2e/dev-full/flow-cursor-coder.json",
		"docs/examples/real-e2e/dev-full/flow.json",
		"docs/examples/real-e2e/dev-full/phase.md",
		"docs/examples/real-e2e/dev-full/scenario-light.md",
		"docs/examples/real-e2e/dev-full/scenario.md",
	}
)

// stableExampleInputs are the exercise inputs and the shared dev-flow
// example's files, none of which moves.
func stableExampleInputs() []string {
	out := append([]string(nil), realE2EExerciseInputs...)
	for _, name := range devFlowFiles {
		out = append(out, devFlowDir+"/"+name)
	}
	return out
}

// checkMaintainerLayout validates FP-1 under root: each relocated document
// is a regular file (not a symlink), each old path is absent in every form
// (Lstat, so a dangling symlink or a directory fails), and every stable
// example input is a regular file at its unchanged path.
func checkMaintainerLayout(root string) error {
	var errs []error
	for _, rel := range maintainerDocs {
		st, err := os.Lstat(filepath.Join(root, filepath.FromSlash(rel)))
		switch {
		case err != nil:
			errs = append(errs, fmt.Errorf("layout: %s is missing: %w", rel, err))
		case !st.Mode().IsRegular():
			errs = append(errs, fmt.Errorf("layout: %s is not a regular file (%s)", rel, st.Mode().Type()))
		}
	}
	for _, rel := range removedMaintainerDocs {
		st, err := os.Lstat(filepath.Join(root, filepath.FromSlash(rel)))
		switch {
		case err == nil:
			errs = append(errs, fmt.Errorf("layout: old path %s still exists (%s)", rel, st.Mode().Type()))
		case !errors.Is(err, fs.ErrNotExist):
			errs = append(errs, fmt.Errorf("layout: old path %s: %w", rel, err))
		}
	}
	for _, rel := range stableExampleInputs() {
		st, err := os.Lstat(filepath.Join(root, filepath.FromSlash(rel)))
		if err != nil || !st.Mode().IsRegular() {
			errs = append(errs, fmt.Errorf("layout: example input %s is not a regular file at its stable path", rel))
		}
	}
	return errors.Join(errs...)
}

// TestMaintainerDocLayout (FP-1): the relocated documents are regular
// files, the old paths are absent with no stub, directory or symlink, and
// the example inputs stay where they are. Temporary-tree mutations (UT-1)
// each fail with their own diagnostic.
func TestMaintainerDocLayout(t *testing.T) {
	root := testkit.MustRepoRoot(t)
	if err := checkMaintainerLayout(root); err != nil {
		t.Fatalf("checkout: %v", err)
	}
	// valid is a minimal temporary tree satisfying the layout.
	valid := func(t *testing.T) string {
		t.Helper()
		dir := t.TempDir()
		for _, rel := range append(append([]string(nil), maintainerDocs...), stableExampleInputs()...) {
			p := filepath.Join(dir, filepath.FromSlash(rel))
			if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(p, []byte("# "+rel+"\n"), 0o644); err != nil {
				t.Fatal(err)
			}
		}
		return dir
	}
	at := func(dir, rel string) string { return filepath.Join(dir, filepath.FromSlash(rel)) }
	t.Run("valid_tree", func(t *testing.T) {
		if err := checkMaintainerLayout(valid(t)); err != nil {
			t.Fatal(err)
		}
	})
	for _, c := range []struct {
		name   string
		mutate func(t *testing.T, dir string) error
		want   string
	}{
		{"missing_target", func(t *testing.T, dir string) error { return os.Remove(at(dir, maintainerCI)) },
			"layout: docs/maintainers/ci.md is missing"},
		{"old_regular_stub", func(t *testing.T, dir string) error {
			return os.WriteFile(at(dir, "docs/ci.md"), []byte("Moved to maintainers/ci.md.\n"), 0o644)
		}, "layout: old path docs/ci.md still exists"},
		{"old_dangling_symlink", func(t *testing.T, dir string) error {
			return os.Symlink("maintainers/no-such.md", at(dir, "docs/real-e2e.md"))
		}, "layout: old path docs/real-e2e.md still exists"},
		{"old_symlink_to_new", func(t *testing.T, dir string) error {
			return os.Symlink("maintainers/real-e2e.md", at(dir, "docs/real-e2e.md"))
		}, "layout: old path docs/real-e2e.md still exists"},
		{"old_directory", func(t *testing.T, dir string) error { return os.Mkdir(at(dir, "docs/ci.md"), 0o755) },
			"layout: old path docs/ci.md still exists"},
		{"new_symlink", func(t *testing.T, dir string) error {
			if err := os.Rename(at(dir, maintainerE2E), at(dir, "elsewhere.md")); err != nil {
				return err
			}
			return os.Symlink("../../elsewhere.md", at(dir, maintainerE2E))
		}, "layout: docs/maintainers/real-e2e.md is not a regular file"},
		{"missing_exercise_input", func(t *testing.T, dir string) error {
			return os.Remove(at(dir, "docs/examples/real-e2e/dev-full/flow.json"))
		}, "layout: example input docs/examples/real-e2e/dev-full/flow.json"},
		{"moved_exercise_input", func(t *testing.T, dir string) error {
			if err := os.MkdirAll(at(dir, "docs/maintainers/examples/real-e2e"), 0o755); err != nil {
				return err
			}
			return os.Rename(at(dir, "docs/examples/real-e2e/README.md"), at(dir, "docs/maintainers/examples/real-e2e/README.md"))
		}, "layout: example input docs/examples/real-e2e/README.md"},
		{"moved_dev_flow_example", func(t *testing.T, dir string) error {
			return os.Rename(at(dir, devFlowDir), at(dir, "docs/maintainers/dev-flow"))
		}, "layout: example input " + devFlowDir + "/README.md"},
	} {
		t.Run(c.name, func(t *testing.T) {
			dir := valid(t)
			if err := c.mutate(t, dir); err != nil {
				t.Fatal(err)
			}
			if err := checkMaintainerLayout(dir); err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("err = %v, want one containing %q", err, c.want)
			}
		})
	}
}

// checkSourceLinks is the source-relative link adapter (FP-2): it resolves
// every simple inline link of md, the text of the repository file src,
// through the existing README validator. Each local destination is
// normalized against src's directory to a repository-relative path, keeping
// its fragment, and checked as a synthetic link by checkReadmeLinks with
// look, so escapes, the ignored design tree, missing files and unmatched
// fragments are rejected exactly as for the README. A fragment-only link is
// checked against md itself. Absolute paths and URI schemes are rejected
// before any lookup; existing http(s) URLs are preserved and never fetched.
func checkSourceLinks(src, md string, look repoLookup) error {
	self := func(rel string) ([]byte, bool) {
		if rel == src {
			return []byte(md), true
		}
		return look(rel)
	}
	var errs []error
	for _, raw := range markdownLinks(md) {
		if strings.HasPrefix(raw, "https://") || strings.HasPrefix(raw, "http://") {
			continue
		}
		file, frag, hasFrag := strings.Cut(raw, "#")
		dest := src
		switch {
		case readmeURIScheme.MatchString(raw):
			errs = append(errs, fmt.Errorf("%s: link %s: URI scheme", src, raw))
			continue
		case strings.HasPrefix(file, "/") || strings.HasPrefix(file, `\`) || filepath.IsAbs(file):
			errs = append(errs, fmt.Errorf("%s: link %s: absolute path", src, raw))
			continue
		case file != "":
			dest = path.Join(path.Dir(src), file)
		}
		if hasFrag {
			dest += "#" + frag
		}
		if err := checkReadmeLinks("[link]("+dest+")", self); err != nil {
			errs = append(errs, fmt.Errorf("%s: link %s: %w", src, raw, err))
		}
	}
	return errors.Join(errs...)
}

// incomingLink is one selected incoming link: its repository-relative
// destination normalized against the source directory, and its fragment
// without "#" ("" for none).
type incomingLink struct{ dest, fragment string }

// incomingMaintainerLedger is the pinned expected multiset of incoming
// maintainer links per incoming-only document (design
// 12-1b-maintainer-docs, FP-2): it is neither derived from the documents
// under test nor read from the design ledger.
var incomingMaintainerLedger = map[string]map[incomingLink]int{
	"docs/coordinator.md":   {{maintainerE2E, ""}: 3},
	"docs/real-adapters.md": {{maintainerE2E, "cursor-coder-qualification"}: 1},
}

// incomingMaintainerLinks selects md's local links whose file destination,
// normalized against src's directory with the fragment ignored, is exactly
// a relocated or a removed maintainer document. It returns them as
// synthetic links carrying the raw destination (for the adapter) and as a
// multiset of (destination, fragment).
func incomingMaintainerLinks(src, md string) (string, map[incomingLink]int) {
	var b strings.Builder
	got := map[incomingLink]int{}
	for _, raw := range markdownLinks(md) {
		file, frag, _ := strings.Cut(raw, "#")
		if file == "" || strings.Contains(raw, "://") {
			continue
		}
		dest := path.Join(path.Dir(src), file)
		for _, d := range append(append([]string(nil), maintainerDocs...), removedMaintainerDocs...) {
			if dest == d {
				b.WriteString("[link](" + raw + ")\n")
				got[incomingLink{dest, frag}]++
			}
		}
	}
	return b.String(), got
}

// checkIncomingLinks validates an incoming-only document: its selected
// links resolve through the source-relative adapter, and their
// (destination, fragment) multiset equals want exactly (duplicate counts
// preserved, order ignored), so a missing, extra or re-fragmented
// occurrence fails.
func checkIncomingLinks(src, md string, want map[incomingLink]int, look repoLookup) error {
	in, got := incomingMaintainerLinks(src, md)
	var errs []error
	keys := map[incomingLink]bool{}
	for k := range want {
		keys[k] = true
	}
	for k := range got {
		keys[k] = true
	}
	for k := range keys {
		if got[k] != want[k] {
			errs = append(errs, fmt.Errorf("%s: incoming link (%s, #%s) occurs %d times, want %d", src, k.dest, k.fragment, got[k], want[k]))
		}
	}
	return errors.Join(append(errs, checkSourceLinks(src, in, look))...)
}

// navigationDocs are the documents whose every local link FP-2 resolves:
// the relocated pair, the README and the two example READMEs.
var navigationDocs = []string{"README.md", maintainerCI, maintainerE2E, "docs/examples/dev-flow/README.md", "docs/examples/real-e2e/README.md"}

// incomingOnlyDocs are changed user documents whose relocated-document
// links FP-2 resolves and counts against incomingMaintainerLedger (their
// other, unchanged links include directory destinations and anchors the
// simple validator does not model, and stay outside this validation).
var incomingOnlyDocs = []string{"docs/coordinator.md", "docs/real-adapters.md"}

// checkNavigation runs the FP-2 link validation over docs (repository path
// to text) with look.
func checkNavigation(docs map[string]string, look repoLookup) error {
	var errs []error
	if err := checkDocMap(docs["README.md"], look); err != nil {
		errs = append(errs, err)
	}
	if body, err := sectionOf(docs["README.md"], "Documentation"); err != nil {
		errs = append(errs, err)
	} else {
		for _, f := range linkFiles(body) {
			if strings.HasPrefix(f, "docs/maintainers/") {
				errs = append(errs, fmt.Errorf("Documentation: maintainer document %s in user navigation", f))
			}
		}
	}
	for _, src := range navigationDocs {
		errs = append(errs, checkSourceLinks(src, docs[src], look))
	}
	for _, src := range incomingOnlyDocs {
		errs = append(errs, checkIncomingLinks(src, docs[src], incomingMaintainerLedger[src], look))
	}
	return errors.Join(errs...)
}

// TestMaintainerDocNavigation (FP-2): the README keeps the relocated
// documents under For maintainers only; every local link of the relocated
// pair, the README and both example READMEs resolves on disk
// (resolveDocLinks) and, with fragments, through the source-relative
// adapter; coordinator.md's and real-adapters.md's links to the runbook
// resolve and equal the pinned destination-and-fragment multiset. Stale,
// missing, misspelled, extra, mis-fragmented and misplaced links fail.
func TestMaintainerDocNavigation(t *testing.T) {
	root := testkit.MustRepoRoot(t)
	look := checkoutLookup(root)
	docs := map[string]string{}
	for _, src := range append(append([]string(nil), navigationDocs...), incomingOnlyDocs...) {
		docs[src] = string(repoFile(t, src))
		if err := checkPrivateText(src, strings.Join(markdownLinks(docs[src]), "\n")); err != nil {
			t.Error(err)
		}
	}
	for _, src := range navigationDocs {
		if err := resolveDocLinks(filepath.Join(root, filepath.FromSlash(src)), docs[src]); err != nil {
			t.Errorf("%s: %v", src, err)
		}
	}
	if err := checkNavigation(docs, look); err != nil {
		t.Fatalf("shipped documents: %v", err)
	}

	t.Run("adapter", func(t *testing.T) {
		// UT-2: the source-relative adapter over an in-memory repository.
		files := map[string]string{
			"docs/a.md":     "# A\n\n## Some heading\n",
			"docs/sub/b.md": "# B\n\n## Deep heading\n",
			"README.md":     "# Root\n",
		}
		look := mapLookup(files)
		for _, c := range []struct {
			name, src, md string
			want          string // "" accepts
		}{
			{"same_directory", "docs/c.md", "[a](a.md) [b](a.md#some-heading) [c](./a.md)", ""},
			{"child_directory", "docs/c.md", "[b](sub/b.md#deep-heading)", ""},
			{"parent_directory", "docs/sub/c.md", "[a](../a.md#some-heading) [r](../../README.md)", ""},
			{"fragment_only", "docs/c.md", "[s](#local-heading)\n\n## Local heading\n", ""},
			{"external_url_not_fetched", "docs/c.md", "[u](https://example.invalid/x.md)", ""},
			{"missing_file", "docs/c.md", "[m](missing.md)", "destination docs/missing.md does not exist"},
			{"wrong_directory", "docs/sub/c.md", "[a](a.md)", "destination docs/sub/a.md does not exist"},
			{"wrong_fragment", "docs/c.md", "[a](a.md#no-heading)", "fragment #no-heading matches no heading"},
			{"wrong_local_fragment", "docs/c.md", "[s](#elsewhere)\n\n## Local heading\n", "fragment #elsewhere matches no heading"},
			{"absolute", "docs/c.md", "[x](/docs/a.md)", "absolute path"},
			{"private_absolute", "docs/c.md", "[x](/home/someone/a.md)", "absolute path"},
			{"uri_scheme", "docs/c.md", "[x](file:///etc/hosts)", "URI scheme"},
			{"escape", "docs/sub/c.md", "[x](../../../outside.md)", "escapes the repository"},
			{"design_tree", "docs/c.md", "[x](../design/roadmap.md)", "ignored design tree"},
		} {
			err := checkSourceLinks(c.src, c.md, look)
			switch {
			case c.want == "" && err != nil:
				t.Errorf("%s: rejected: %v", c.name, err)
			case c.want != "" && (err == nil || !strings.Contains(err.Error(), c.want)):
				t.Errorf("%s: err = %v, want one containing %q", c.name, err, c.want)
			}
		}
	})

	t.Run("incoming_only", func(t *testing.T) {
		// UT-2: the incoming-only selection and its exact multiset equality
		// over an in-memory repository, with the pinned ledger's shape.
		runbook := "# Runbook\n\n## Cursor coder qualification\n"
		files := map[string]string{maintainerE2E: runbook, "docs/other.md": "# Other\n"}
		look := mapLookup(files)
		hidden := mapLookup(map[string]string{"docs/other.md": "# Other\n"})
		three := map[incomingLink]int{{maintainerE2E, ""}: 3}
		one := map[incomingLink]int{{maintainerE2E, "cursor-coder-qualification"}: 1}
		const coord = "[a](maintainers/real-e2e.md) and [b](maintainers/real-e2e.md), [o](other.md), [d](../tests/testdata/dir/), [h](#a-b)\n" +
			"[c](maintainers/real-e2e.md) [u](https://example.invalid/real-e2e.md)\n"
		const adapters = "See [Cursor coder qualification](maintainers/real-e2e.md#cursor-coder-qualification) and [o](other.md).\n"
		for _, c := range []struct {
			name, src, md string
			want          map[incomingLink]int
			look          repoLookup
			err           string // "" accepts
		}{
			{"coordinator_exact", "docs/coordinator.md", coord, three, look, ""},
			{"real_adapters_exact", "docs/real-adapters.md", adapters, one, look, ""},
			{"order_ignored", "docs/coordinator.md", "[o](other.md) [c](maintainers/real-e2e.md)\n[b](maintainers/real-e2e.md)\n\n[a](maintainers/real-e2e.md)\n", three, look, ""},
			{"nested_source", "docs/examples/x/README.md", "[r](../../maintainers/real-e2e.md#cursor-coder-qualification)", one, look, ""},
			{"stale_path", "docs/coordinator.md", strings.Replace(coord, "](maintainers/real-e2e.md)", "](real-e2e.md)", 1), three, look,
				"incoming link (docs/real-e2e.md, #) occurs 1 times, want 0"},
			{"stale_path_unresolved", "docs/real-adapters.md", strings.Replace(adapters, "](maintainers/", "](", 1), one, look,
				"destination docs/real-e2e.md does not exist"},
			{"hidden_target", "docs/coordinator.md", coord, three, hidden, "destination docs/maintainers/real-e2e.md does not exist"},
			{"wrong_fragment", "docs/real-adapters.md", strings.Replace(adapters, "#cursor-coder-qualification", "#cursor-coder", 1), one, look,
				"incoming link (docs/maintainers/real-e2e.md, #cursor-coder-qualification) occurs 0 times, want 1"},
			{"wrong_fragment_unresolved", "docs/real-adapters.md", strings.Replace(adapters, "#cursor-coder-qualification", "#cursor-coder", 1), one, look,
				"fragment #cursor-coder matches no heading"},
			{"all_removed", "docs/coordinator.md", "[o](other.md) [d](../tests/testdata/dir/)\n", three, look,
				"incoming link (docs/maintainers/real-e2e.md, #) occurs 0 times, want 3"},
			{"one_misspelled_siblings_remain", "docs/coordinator.md", strings.Replace(coord, "](maintainers/real-e2e.md)", "](maintainer/real-e2e.md)", 1), three, look,
				"incoming link (docs/maintainers/real-e2e.md, #) occurs 2 times, want 3"},
			{"extra_occurrence", "docs/coordinator.md", coord + "[d](maintainers/real-e2e.md)\n", three, look,
				"incoming link (docs/maintainers/real-e2e.md, #) occurs 4 times, want 3"},
			{"extra_ci_link", "docs/real-adapters.md", adapters + "[ci](maintainers/ci.md)\n", one, look,
				"incoming link (docs/maintainers/ci.md, #) occurs 1 times, want 0"},
		} {
			err := checkIncomingLinks(c.src, c.md, c.want, c.look)
			switch {
			case c.err == "" && err != nil:
				t.Errorf("%s: rejected: %v", c.name, err)
			case c.err != "" && (err == nil || !strings.Contains(err.Error(), c.err)):
				t.Errorf("%s: err = %v, want one containing %q", c.name, err, c.err)
			}
		}
		// The pinned ledger covers exactly the incoming-only documents.
		if len(incomingMaintainerLedger) != len(incomingOnlyDocs) {
			t.Fatalf("ledger covers %d documents, want %d", len(incomingMaintainerLedger), len(incomingOnlyDocs))
		}
		for _, src := range incomingOnlyDocs {
			if len(incomingMaintainerLedger[src]) == 0 {
				t.Fatalf("%s has no pinned incoming links", src)
			}
		}
	})

	t.Run("mutations", func(t *testing.T) {
		edit := func(src, old, repl string) func() (map[string]string, repoLookup) {
			return func() (map[string]string, repoLookup) {
				m := map[string]string{}
				for k, v := range docs {
					m[k] = v
				}
				if !strings.Contains(m[src], old) {
					t.Fatalf("%s: the mutation target %q is absent", src, old)
				}
				m[src] = strings.Replace(m[src], old, repl, 1)
				return m, look
			}
		}
		without := func(hidden string) func() (map[string]string, repoLookup) {
			return func() (map[string]string, repoLookup) {
				return docs, func(rel string) ([]byte, bool) {
					if rel == hidden {
						return nil, false
					}
					return look(rel)
				}
			}
		}
		moveCI := func() (map[string]string, repoLookup) {
			m, _ := edit("README.md", "- [CI](docs/maintainers/ci.md): the CI checks, their commands and budgets.\n", "")()
			s, e := sectionSpan(t, m["README.md"], "Documentation")
			m["README.md"] = m["README.md"][:s] + strings.TrimSuffix(m["README.md"][s:e], "\n") +
				"\n- [CI](docs/maintainers/ci.md): the CI checks, their commands and budgets.\n\n" + m["README.md"][e:]
			return m, look
		}
		for _, c := range []struct {
			name  string
			apply func() (map[string]string, repoLookup)
			want  string
		}{
			{"readme_old_ci_path", edit("README.md", "](docs/maintainers/ci.md)", "](docs/ci.md)"), "destination docs/ci.md does not exist"},
			{"coordinator_old_runbook_link", edit("docs/coordinator.md", "](maintainers/real-e2e.md)", "](real-e2e.md)"), "destination docs/real-e2e.md does not exist"},
			{"real_adapters_old_runbook_link", edit("docs/real-adapters.md", "](maintainers/real-e2e.md#cursor-coder-qualification)", "](real-e2e.md#cursor-coder-qualification)"),
				"destination docs/real-e2e.md does not exist"},
			{"dev_flow_old_runbook_link", edit("docs/examples/dev-flow/README.md", "](../../maintainers/real-e2e.md)", "](../../real-e2e.md)"), "destination docs/real-e2e.md does not exist"},
			{"exercise_old_runbook_link", edit("docs/examples/real-e2e/README.md", "](../../maintainers/real-e2e.md#cursor-coder-qualification)", "](../../real-e2e.md#cursor-coder-qualification)"),
				"destination docs/real-e2e.md does not exist"},
			{"runbook_stale_relative_link", edit(maintainerE2E, "](../examples/dev-flow/README.md)", "](examples/dev-flow/README.md)"),
				"destination docs/maintainers/examples/dev-flow/README.md does not exist"},
			{"ci_stale_relative_link", edit(maintainerCI, "](../workspaces.md#manual-m4-checks)", "](workspaces.md#manual-m4-checks)"),
				"destination docs/maintainers/workspaces.md does not exist"},
			{"ci_stale_platform_link", edit(maintainerCI, "](../../internal/devcheck/platform.go)", "](../internal/devcheck/platform.go)"),
				"destination docs/internal/devcheck/platform.go does not exist"},
			{"missing_runbook", without(maintainerE2E), "destination docs/maintainers/real-e2e.md does not exist"},
			{"missing_ci", without(maintainerCI), "destination docs/maintainers/ci.md does not exist"},
			{"wrong_incoming_fragment", edit("docs/real-adapters.md", "#cursor-coder-qualification)", "#cursor-coder-qualificaton)"), "fragment #cursor-coder-qualificaton matches no heading"},
			{"incoming_fragment_count", edit("docs/real-adapters.md", "#cursor-coder-qualification)", "#cursor-coder-qualificaton)"),
				"docs/real-adapters.md: incoming link (docs/maintainers/real-e2e.md, #cursor-coder-qualification) occurs 0 times, want 1"},
			{"coordinator_destination_misspelled", edit("docs/coordinator.md", "](maintainers/real-e2e.md)", "](maintainer/real-e2e.md)"),
				"docs/coordinator.md: incoming link (docs/maintainers/real-e2e.md, #) occurs 2 times, want 3"},
			{"coordinator_extra_incoming_link", edit("docs/coordinator.md", "\n## ", "\nSee the [runbook](maintainers/real-e2e.md).\n\n## "),
				"docs/coordinator.md: incoming link (docs/maintainers/real-e2e.md, #) occurs 4 times, want 3"},
			{"coordinator_fragment_added", edit("docs/coordinator.md", "](maintainers/real-e2e.md)", "](maintainers/real-e2e.md#run)"),
				"docs/coordinator.md: incoming link (docs/maintainers/real-e2e.md, #run) occurs 1 times, want 0"},
			{"wrong_outgoing_fragment", edit(maintainerCI, "#manual-m4-checks)", "#manual-checks)"), "fragment #manual-checks matches no heading"},
			{"wrong_local_fragment", edit(maintainerE2E, "](#pre-probe-smoke)", "](#pre-probe-smok)"), "fragment #pre-probe-smok matches no heading"},
			{"missing_maintainer_entry", edit("README.md", "- [CI](docs/maintainers/ci.md): the CI checks, their commands and budgets.\n", ""),
				"for maintainers: missing link docs/maintainers/ci.md"},
			{"entry_moved_into_documentation", moveCI, "Documentation: maintainer link docs/maintainers/ci.md outside For maintainers"},
			{"new_maintainer_entry_in_documentation", edit("README.md", "\n## For maintainers\n", "- [Runbook section](docs/maintainers/real-e2e.md#run)\n\n## For maintainers\n"),
				"Documentation: maintainer document docs/maintainers/real-e2e.md in user navigation"},
		} {
			m, lk := c.apply()
			if err := checkNavigation(m, lk); err == nil || !strings.Contains(err.Error(), c.want) {
				t.Errorf("%s: err = %v, want one containing %q", c.name, err, c.want)
			}
		}
		// Every incoming link removed from a changed user document fails.
		m := map[string]string{}
		for k, v := range docs {
			m[k] = v
		}
		m["docs/real-adapters.md"] = strings.ReplaceAll(m["docs/real-adapters.md"], "](maintainers/real-e2e.md#cursor-coder-qualification)", "")
		m["docs/coordinator.md"] = strings.ReplaceAll(m["docs/coordinator.md"], "](maintainers/real-e2e.md)", "")
		err := checkNavigation(m, look)
		for _, want := range []string{"docs/real-adapters.md: incoming link (docs/maintainers/real-e2e.md, #cursor-coder-qualification) occurs 0 times, want 1",
			"docs/coordinator.md: incoming link (docs/maintainers/real-e2e.md, #) occurs 0 times, want 3"} {
			if err == nil || !strings.Contains(err.Error(), want) {
				t.Errorf("no incoming link: err = %v, want one containing %q", err, want)
			}
		}
		// The on-disk resolver rejects a stale runbook-relative destination.
		stale := strings.Replace(docs[maintainerE2E], "](../examples/dev-flow/README.md)", "](examples/dev-flow/README.md)", 1)
		if err := resolveDocLinks(filepath.Join(root, filepath.FromSlash(maintainerE2E)), stale); err == nil {
			t.Error("resolveDocLinks accepted a stale runbook-relative destination")
		}
	})
}

// reale2eSeeLine is the relocated runbook pointer of reale2e's usage.
const reale2eSeeLine = "See docs/maintainers/real-e2e.md."

// checkUsageSeeLine requires the relocated runbook pointer exactly once as
// its own line and no pre-relocation pointer.
func checkUsageSeeLine(usage string) error {
	n := 0
	for _, line := range strings.Split(usage, "\n") {
		switch line {
		case reale2eSeeLine:
			n++
		case "See docs/real-e2e.md.":
			return errors.New("usage: points to the removed docs/real-e2e.md")
		}
	}
	if n != 1 {
		return fmt.Errorf("usage: %d %q lines, want 1", n, reale2eSeeLine)
	}
	return checkPrivateText("usage", usage)
}

// checkWorkflowDocComment requires the workflow's first-line comment to
// cite the relocated CI reference, which must exist, and no old path.
func checkWorkflowDocComment(wf string, look repoLookup) error {
	first, _, _ := strings.Cut(wf, "\n")
	if !strings.HasPrefix(first, "# ") || !strings.Contains(first, "(see "+maintainerCI+")") {
		return fmt.Errorf("workflow: line 1 does not cite %s: %q", maintainerCI, first)
	}
	if strings.Contains(wf, "(see docs/ci.md)") {
		return errors.New("workflow: cites the removed docs/ci.md")
	}
	if _, ok := look(maintainerCI); !ok {
		return fmt.Errorf("workflow: %s does not exist", maintainerCI)
	}
	return nil
}

// TestMaintainerDocConsumers (FP-3): the relocated documents' readers,
// reale2e's usage pointer and the workflow comment follow the move, and
// the existing CI, codex-triage and closeout validators accept the
// relocated content. Pointer and comment mutations fail.
func TestMaintainerDocConsumers(t *testing.T) {
	root := testkit.MustRepoRoot(t)
	look := checkoutLookup(root)
	// The established helpers read the relocated documents.
	for _, rel := range maintainerDocs {
		if len(repoFile(t, rel)) == 0 {
			t.Fatalf("%s is empty", rel)
		}
	}
	if platform := docSection(t, "Platform code"); !strings.Contains(platform, "](../../internal/devcheck/platform.go)") {
		t.Fatal("docSection does not read the relocated CI reference")
	}
	docs := codexDocs(t)
	if docs["e2e"] != string(repoFile(t, maintainerE2E)) {
		t.Fatal("codexDocs does not read the relocated runbook")
	}

	t.Run("reale2e_usage", func(t *testing.T) {
		// The no-argument usage branch, with an explicit platform, an empty
		// environment and captured writers: no live command runs.
		var stdout, stderr bytes.Buffer
		getenv := func(string) string { return "" }
		lookupEnv := func(string) (string, bool) { return "", false }
		code := reale2e.Main("linux", "amd64", nil, getenv, lookupEnv, strings.NewReader(""), &stdout, &stderr)
		if code != 2 || stdout.Len() != 0 || !strings.HasPrefix(stderr.String(), "reale2e: missing subcommand\nusage:") {
			t.Fatalf("Main() = %d, stdout %q, stderr %q", code, stdout.String(), stderr.String())
		}
		if !strings.Contains(stderr.String(), reale2e.Usage) {
			t.Fatal("the usage error does not print Usage")
		}
		for _, text := range []string{stderr.String(), reale2e.Usage} {
			if err := checkUsageSeeLine(text); err != nil {
				t.Fatal(err)
			}
		}
		for name, mutated := range map[string]string{
			"stale":     strings.Replace(reale2e.Usage, reale2eSeeLine, "See docs/real-e2e.md.", 1),
			"removed":   strings.Replace(reale2e.Usage, reale2eSeeLine+"\n", "", 1),
			"duplicate": strings.Replace(reale2e.Usage, reale2eSeeLine, reale2eSeeLine+"\n"+reale2eSeeLine, 1),
			"inline":    strings.Replace(reale2e.Usage, "\n"+reale2eSeeLine, " "+reale2eSeeLine, 1),
			"private":   strings.Replace(reale2e.Usage, reale2eSeeLine, reale2eSeeLine+"\nSee /home/someone/real-e2e.md.", 1),
		} {
			if mutated == reale2e.Usage {
				t.Fatalf("%s: the mutation changed nothing", name)
			}
			if err := checkUsageSeeLine(mutated); err == nil {
				t.Errorf("%s: the mutated usage was accepted", name)
			}
		}
	})

	t.Run("workflow_comment", func(t *testing.T) {
		wf := string(ciWorkflow(t))
		if err := checkWorkflowDocComment(wf, look); err != nil {
			t.Fatal(err)
		}
		for name, c := range map[string]struct {
			wf   string
			look repoLookup
		}{
			"old_path":      {strings.Replace(wf, "(see "+maintainerCI+")", "(see docs/ci.md)", 1), look},
			"no_citation":   {strings.Replace(wf, " (see "+maintainerCI+")", "", 1), look},
			"not_line_one":  {"# CI for callsheet.\n" + wf, look},
			"missing_ci":    {wf, func(string) ([]byte, bool) { return nil, false }},
			"old_elsewhere": {wf + "# (see docs/ci.md)\n", look},
		} {
			if err := checkWorkflowDocComment(c.wf, c.look); err == nil {
				t.Errorf("%s: the mutated workflow was accepted", name)
			}
		}
	})

	t.Run("existing_validators", func(t *testing.T) {
		// The CI reference's hosted closures, the codex-triage historical
		// records and the Cursor closeout, unchanged, over the relocated
		// content.
		first := docSection(t, "First remote run")
		for _, c := range hostedClosures {
			if err := checkHostedClosure(first, c); err != nil {
				t.Errorf("CI: %v", err)
			}
		}
		if err := checkHistoricalRecords(docs); err != nil {
			t.Errorf("codex triage: %v", err)
		}
		if err := checkCursorCloseoutPaths(docs["e2e"]); err != nil {
			t.Errorf("closeout paths: %v", err)
		}
		if err := checkCursorCloseout(docs["e2e"]); err != nil {
			t.Errorf("closeout: %v", err)
		}
	})
}

// linkToken is one moved document's approved destination substitution
// (design-check moved-link-map, grouped): newToken occurs exactly n times
// and oldToken never outside those occurrences.
type linkToken struct {
	file, oldToken, newToken string
	n                        int
}

// maintainerLinkTokens are the relocation's 26 substituted link
// occurrences, grouped by (file, old token, new token).
var maintainerLinkTokens = []linkToken{
	{maintainerCI, "](../internal/devcheck/platform.go)", "](../../internal/devcheck/platform.go)", 1},
	{maintainerCI, "](real-adapters.md)", "](../real-adapters.md)", 2},
	{maintainerCI, "](workspaces.md#manual-m4-checks)", "](../workspaces.md#manual-m4-checks)", 1},
	{maintainerE2E, "](coordinator.md)", "](../coordinator.md)", 1},
	{maintainerE2E, "](examples/dev-flow/README.md)", "](../examples/dev-flow/README.md)", 1},
	{maintainerE2E, "](examples/dev-flow/code-reviewer-instruction.md)", "](../examples/dev-flow/code-reviewer-instruction.md)", 1},
	{maintainerE2E, "](examples/dev-flow/code-reviewer-runbook.md)", "](../examples/dev-flow/code-reviewer-runbook.md)", 1},
	{maintainerE2E, "](examples/dev-flow/coder-instruction.md)", "](../examples/dev-flow/coder-instruction.md)", 1},
	{maintainerE2E, "](examples/dev-flow/coder-runbook.md)", "](../examples/dev-flow/coder-runbook.md)", 1},
	{maintainerE2E, "](examples/dev-flow/coordinator-playbook.md)", "](../examples/dev-flow/coordinator-playbook.md)", 1},
	{maintainerE2E, "](examples/dev-flow/design-reviewer-instruction.md)", "](../examples/dev-flow/design-reviewer-instruction.md)", 1},
	{maintainerE2E, "](examples/dev-flow/design-reviewer-runbook.md)", "](../examples/dev-flow/design-reviewer-runbook.md)", 1},
	{maintainerE2E, "](examples/dev-flow/designer-instruction.md)", "](../examples/dev-flow/designer-instruction.md)", 1},
	{maintainerE2E, "](examples/dev-flow/designer-runbook.md)", "](../examples/dev-flow/designer-runbook.md)", 1},
	{maintainerE2E, "](examples/dev-flow/triage-instruction.md)", "](../examples/dev-flow/triage-instruction.md)", 1},
	{maintainerE2E, "](examples/dev-flow/triage-runbook.md)", "](../examples/dev-flow/triage-runbook.md)", 2},
	{maintainerE2E, "](examples/real-e2e/README.md)", "](../examples/real-e2e/README.md)", 1},
	{maintainerE2E, "](examples/real-e2e/dev-full/coordinator-prompt.md)", "](../examples/real-e2e/dev-full/coordinator-prompt.md)", 1},
	{maintainerE2E, "](examples/real-e2e/dev-full/flow-cursor-coder.json)", "](../examples/real-e2e/dev-full/flow-cursor-coder.json)", 2},
	{maintainerE2E, "](examples/real-e2e/dev-full/flow.json)", "](../examples/real-e2e/dev-full/flow.json)", 2},
	{maintainerE2E, "](examples/real-e2e/dev-full/phase.md)", "](../examples/real-e2e/dev-full/phase.md)", 1},
	{maintainerE2E, "](examples/real-e2e/dev-full/scenario.md)", "](../examples/real-e2e/dev-full/scenario.md)", 1},
}

// checkLinkTokens is FP-4's semantic link-token checker: each row's new
// token occurs exactly n times, and once those approved occurrences are
// removed its old token no longer occurs (so an old token that is a
// substring of its new token is not a false positive). It pins no other
// byte of doc.
func checkLinkTokens(doc string, rows []linkToken) error {
	var errs []error
	for _, r := range rows {
		if got := strings.Count(doc, r.newToken); got != r.n {
			errs = append(errs, fmt.Errorf("%s: %q occurs %d times, want %d", r.file, r.newToken, got, r.n))
		}
		if rest := strings.ReplaceAll(doc, r.newToken, ""); strings.Contains(rest, r.oldToken) {
			errs = append(errs, fmt.Errorf("%s: unrelocated %q remains", r.file, r.oldToken))
		}
	}
	return errors.Join(errs...)
}

// tokensOf returns the rows of file.
func tokensOf(file string) []linkToken {
	var out []linkToken
	for _, r := range maintainerLinkTokens {
		if r.file == file {
			out = append(out, r)
		}
	}
	return out
}

// TestMaintainerDocContentPreserved (FP-4): the relocated documents carry
// every substituted link token at its approved multiplicity and no
// unrelocated form, and resolve on disk. Restored, deleted, duplicated and
// wrong-destination tokens fail; unrelated prose and blank-line edits pass,
// so this is not a document snapshot. The exact relocation bytes are
// proven once by the design-check comparator, not here.
func TestMaintainerDocContentPreserved(t *testing.T) {
	root := testkit.MustRepoRoot(t)
	total := 0
	for _, r := range maintainerLinkTokens {
		total += r.n
	}
	if total != 26 || len(tokensOf(maintainerCI))+len(tokensOf(maintainerE2E)) != len(maintainerLinkTokens) {
		t.Fatalf("token table: %d occurrences in %d rows", total, len(maintainerLinkTokens))
	}
	for _, file := range maintainerDocs {
		doc := string(repoFile(t, file))
		rows := tokensOf(file)
		if err := checkLinkTokens(doc, rows); err != nil {
			t.Fatalf("shipped %s: %v", file, err)
		}
		abs := filepath.Join(root, filepath.FromSlash(file))
		if err := resolveDocLinks(abs, doc); err != nil {
			t.Fatalf("shipped %s: %v", file, err)
		}
		for _, r := range rows {
			for name, mutated := range map[string]string{
				"restore_old":       strings.Replace(doc, r.newToken, r.oldToken, 1),
				"delete_new":        strings.Replace(doc, r.newToken, "]()", 1),
				"duplicate_new":     strings.Replace(doc, r.newToken, r.newToken+" [again"+r.newToken, 1),
				"wrong_destination": strings.Replace(doc, r.newToken, "](../no-such-12-1b-document.md)", 1),
			} {
				if err := checkLinkTokens(mutated, rows); err == nil {
					t.Errorf("%s %s %s: accepted", file, r.newToken, name)
				}
				if name == "wrong_destination" {
					if err := resolveDocLinks(abs, mutated); err == nil {
						t.Errorf("%s %s: resolveDocLinks accepted a nonexistent destination", file, r.newToken)
					}
				}
			}
		}
		// Unrelated prose and whitespace edits pass the token checker.
		for name, edited := range map[string]string{
			"prose":       strings.Replace(doc, "\n\n", "\n\nAn unrelated maintainer note.\n\n", 1),
			"blank_line":  strings.Replace(doc, "\n\n", "\n\n\n", 1),
			"trailing_nl": doc + "\n",
		} {
			if err := checkLinkTokens(edited, rows); err != nil {
				t.Errorf("%s %s: rejected: %v", file, name, err)
			}
		}
	}

	t.Run("checker", func(t *testing.T) {
		// UT-4: an old token inside its new token, and the discriminating
		// mutations on a synthetic document.
		rows := []linkToken{
			{"x.md", "a.md)", "../a.md)", 2},
			{"x.md", "](b.md)", "](../b.md)", 1},
		}
		doc := "See [a](../a.md), [b](../b.md) and [a again](../a.md).\n"
		for _, c := range []struct {
			name, doc string
			ok        bool
		}{
			{"correct", doc, true},
			{"prose", "Intro.\n\n" + doc, true},
			{"newline", doc + "\n", true},
			{"restored_old", strings.Replace(doc, "../a.md)", "a.md)", 1), false},
			{"extra_old", doc + "[c](a.md)\n", false},
			{"deleted_new", strings.Replace(doc, "[b](../b.md)", "b", 1), false},
			{"duplicated_new", doc + "[b](../b.md)\n", false},
			{"wrong_destination", strings.Replace(doc, "](../b.md)", "](../c.md)", 1), false},
		} {
			if err := checkLinkTokens(c.doc, rows); (err == nil) != c.ok {
				t.Errorf("%s: err = %v, want ok %v", c.name, err, c.ok)
			}
		}
	})
}
