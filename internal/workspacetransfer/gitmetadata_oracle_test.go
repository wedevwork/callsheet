package workspacetransfer

import (
	"bufio"
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
		r := newRepo(t, map[string]fspec{"f": reg("f\n")})
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
