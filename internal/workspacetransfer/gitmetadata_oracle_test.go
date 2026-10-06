package workspacetransfer

import (
	"bufio"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// metadataPolicy reports a metadata row Callsheet refuses although Git
// accepts it: any shallow file (a shallow repository), a commondir (a
// linked worktree's git directory), an alternates entry (an alternate
// object store) and a NUL in ref metadata (Git truncates the C string).
func metadataPolicy(file, value string) bool {
	switch file {
	case "shallow", "commondir":
		return true
	case "objects/info/alternates", "objects/info/http-alternates":
		for _, l := range strings.Split(value, "\n") {
			if l != "" && l[0] != '#' {
				return true
			}
		}
	case "HEAD", "refs/heads/main":
		return strings.Contains(value, "\x00")
	}
	return false
}

// TestMetadataFilesMatchGit pushes after writing each metadata file of
// the oracle table (testdata/gitmetadata_oracle.tsv) with each value.
func TestMetadataFilesMatchGit(t *testing.T) {
	f, err := os.Open(filepath.Join("testdata", "gitmetadata_oracle.tsv"))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	type row struct {
		file, value string
		accept      bool
	}
	var rows []row
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		l := sc.Text()
		if strings.HasPrefix(l, "#") {
			continue
		}
		p := strings.Split(l, "\t")
		v, err := strconv.Unquote(p[1])
		if err != nil || len(p) != 3 {
			t.Fatalf("bad row %q", l)
		}
		rows = append(rows, row{p[0], v, p[2] == "accept"})
	}
	if len(rows) < 200 {
		t.Fatalf("metadata oracle truncated: %d rows", len(rows))
	}
	env := emptyEnv(t)
	run := sampler(t)
	for i, c := range rows {
		if !run(i) {
			continue
		}
		r := oneFileTemplate.copy(t)
		p := filepath.Join(r.root, ".git", filepath.FromSlash(c.file))
		content := c.value
		switch c.file {
		case "HEAD":
			content = "ref: refs/heads/main" + strings.TrimSuffix(c.value, "\n") + "\n"
		case "refs/heads/main":
			content = r.head.String() + strings.TrimSuffix(c.value, "\n") + "\n"
		}
		mustWrite(t, p, content)
		err := pushLocal(t, r.root, env)
		want := c.accept && !metadataPolicy(c.file, c.value)
		if want != (err == nil) {
			t.Errorf("%s = %q: Git %v, policy %v; Push: %v", c.file, c.value, map[bool]string{true: "accepts", false: "refuses"}[c.accept],
				metadataPolicy(c.file, c.value), err)
		}
	}
}

// TestRepoTemplatePerUse proves a repoTemplate copy is newRepo's
// repository and is private to its use: each repetition overwrites every
// file of a copy and adds metadata, and both a second copy in the same
// repetition and the first copy of every later repetition (-count) must
// match the template's fingerprint taken when it was built.
func TestRepoTemplatePerUse(t *testing.T) {
	for name, tm := range map[string]*repoTemplate{"one file": oneFileTemplate, "state": stateTemplate} {
		first := tm.copy(t)
		if fingerprint(t, first.root) != tm.print {
			t.Fatalf("%s: a previous use's mutation leaked into the template", name)
		}
		if fresh := newRepo(t, tm.files); fingerprint(t, fresh.root) != tm.print || fresh.head != tm.head || first.head != tm.head {
			t.Fatalf("%s: the copy is not newRepo's repository", name)
		}
		if err := filepath.WalkDir(first.root, func(p string, e fs.DirEntry, err error) error {
			if err != nil || !e.Type().IsRegular() {
				return err
			}
			if err := os.Chmod(p, 0o600); err != nil {
				return err
			}
			return os.WriteFile(p, []byte("scribbled"), 0o600)
		}); err != nil {
			t.Fatal(err)
		}
		mustWrite(t, filepath.Join(first.root, ".git", "shallow"), first.head.String()+"\n")
		again := tm.copy(t)
		if again.root == first.root || fingerprint(t, again.root) != tm.print {
			t.Fatalf("%s: a use's mutation reached the next use", name)
		}
		if fingerprint(t, tm.root) != tm.print {
			t.Fatalf("%s: the template changed", name)
		}
	}
}
