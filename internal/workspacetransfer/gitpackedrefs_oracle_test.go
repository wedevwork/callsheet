package workspacetransfer

import (
	"bufio"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/wedevwork/callsheet/internal/contract"
)

// packedStricter are the packed-refs record shapes Callsheet refuses
// although Git 2.43 accepted them in the oracle, because Git validates a
// record only when a lookup or iteration visits it and git status did not
// visit these: each is a record Git rejects on a visit (an unparsable
// hash or separator, or a dangerous name per refname_is_safe).
var packedStricter = map[string]bool{
	"H refs/../x\n": true, "H /refs/tags/a\n": true, "H refs/\n": true, "H @\n": true, "H refs//\n": true,
	"H  refs/tags/a\n": true, "H refs/./tags\n": true, "Hrefs/tags/none\n": true,
	"abababababababababababababababababababa refs/tags/short\n": true, "Ha refs/tags/long\n": true, "H  refs/tags/two\n": true,
}

// TestPackedRefsMatchGit pushes a repository whose HEAD resolves through
// packed-refs, with each record shape of the oracle table
// (testdata/gitpackedrefs_oracle.tsv) after the HEAD record.
func TestPackedRefsMatchGit(t *testing.T) {
	f, err := os.Open(filepath.Join("testdata", "gitpackedrefs_oracle.tsv"))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	type shape struct {
		s      string
		accept bool
	}
	var shapes []shape
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		l := sc.Text()
		if strings.HasPrefix(l, "#") {
			continue
		}
		p := strings.Split(l, "\t")
		s, err := strconv.Unquote(p[0])
		if err != nil || len(p) != 3 {
			t.Fatalf("bad row %q", l)
		}
		shapes = append(shapes, shape{s, p[1] == "accept"})
	}
	if len(shapes) < 60 {
		t.Fatalf("packed-refs oracle truncated: %d rows", len(shapes))
	}
	env := emptyEnv(t)
	r := newRepo(t, map[string]fspec{"f": reg("f\n")})
	h := r.head.String()
	os.Remove(filepath.Join(r.root, ".git/refs/heads/main"))
	run := sampler(t)
	for i, c := range shapes {
		if !run(i) {
			continue
		}
		key := strings.ReplaceAll(strings.ReplaceAll(c.s, strings.Repeat("ab", 20), "H"), strings.ToUpper(strings.Repeat("ab", 20)), "UH")
		body := strings.ReplaceAll(c.s, strings.Repeat("ab", 20), h)
		body = strings.ReplaceAll(body, strings.ToUpper(strings.Repeat("ab", 20)), strings.ToUpper(h))
		body = strings.ReplaceAll(body, strings.Repeat("ab", 20)[:39], h[:39])
		mustWrite(t, filepath.Join(r.root, ".git/packed-refs"), "# pack-refs with: peeled fully-peeled \n"+h+" refs/heads/main\n"+body)
		err := pushLocal(t, r.root, env)
		want := c.accept && !packedStricter[key]
		switch {
		case want && err != nil:
			t.Errorf("%q: Git accepts it, Push refused: %v", key, err)
		case !want && err == nil:
			t.Errorf("%q: Git refuses it (or a stricter full-validation case), Push accepted", key)
		case !want && contract.TransferReason(err) != contract.ReasonUnsupportedRepository:
			t.Errorf("%q: refused with %v", key, err)
		}
	}
}

// TestRefnameRules covers check_refname_format and refname_is_safe.
func TestRefnameRules(t *testing.T) {
	for _, c := range []struct {
		name           string
		format, onelvl bool
		safe           bool
	}{
		{"refs/heads/main", true, true, true}, {"HEAD", false, true, true}, {"foo", false, true, false}, {"@", false, false, false},
		{"refs/tags/a.", false, false, true}, {"refs/tags/a..b", false, false, true}, {"refs/tags/.a", false, false, true},
		{"refs/tags/a.lock", false, false, true}, {"refs/tags/a@{1}", false, false, true}, {"refs/tags/a*", false, false, true},
		{"refs/tags//a", false, false, false}, {"refs/../x", false, false, false}, {"refs/", false, false, false}, {"", false, false, false},
		{"FOO_BAR", false, true, true}, {"FOO-BAR", false, true, false}, {"refs/tags/a\x7f", false, false, true},
	} {
		if checkRefnameFormat(c.name, false) != c.format || checkRefnameFormat(c.name, true) != c.onelvl || refnameIsSafe(c.name) != c.safe {
			t.Errorf("%q: format %v/%v safe %v", c.name, checkRefnameFormat(c.name, false), checkRefnameFormat(c.name, true), refnameIsSafe(c.name))
		}
	}
}
