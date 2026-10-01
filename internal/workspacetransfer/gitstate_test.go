package workspacetransfer

import (
	"bufio"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/go-git/go-git/v5/plumbing"

	"github.com/wedevwork/callsheet/internal/contract"
	"github.com/wedevwork/callsheet/internal/testkit"
)

// Code review r6: repository state Git consults when it reads objects or
// refs during open or status, beyond configuration and metadata files:
// replacement refs and their switches, the stash and its reflog, other
// refs, an upstream, linked worktrees, packs, the commit-graph chain,
// corrupt loose objects, grafts and the ref namespace.
// testdata/gitstate_oracle.tsv holds Git 2.43.0's verdict for each
// scenario (gitoracle_gen_test.go regenerates it).

// stateObjects are the fixture's object names the scenarios refer to: the
// HEAD commit of files f and d/g, its root tree, the subtree d, the blob
// of f, another commit and two names of absent objects.
type stateObjects struct {
	head, tree, subtree, blob, other, missing, missing2 string
}

func (o stateObjects) expand(s string) string {
	return strings.NewReplacer("{HEAD}", o.head, "{HEADUP}", strings.ToUpper(o.head), "{HEADOBJ}", "objects/"+o.head[:2]+"/"+o.head[2:],
		"{TREE}", o.tree, "{SUBTREE}", o.subtree, "{BLOB}", o.blob, "{OTHER}", o.other, "{MISSING}", o.missing,
		"{MISSING2}", o.missing2, "{MISSING2OBJ}", "objects/"+o.missing2[:2]+"/"+o.missing2[2:]).Replace(s)
}

// stateScenario is one repository state.
type stateScenario struct {
	name   string
	files  map[string]string // path below .git ("/" suffix: a directory) and content
	config string            // appended to .git/config
	env    map[string]string
}

const packedHeader = "# pack-refs with: peeled fully-peeled \n"

var stateScenarios = []stateScenario{
	{name: "replace HEAD by a missing object", files: map[string]string{"refs/replace/{HEAD}": "{MISSING}\n"}},
	{name: "replace HEAD by a missing object (packed)", files: map[string]string{"packed-refs": packedHeader + "{MISSING} refs/replace/{HEAD}\n"}},
	{name: "replace HEAD by itself", files: map[string]string{"refs/replace/{HEAD}": "{HEAD}\n"}},
	{name: "replace HEAD by another commit", files: map[string]string{"refs/replace/{HEAD}": "{OTHER}\n"}},
	{name: "replace HEAD by a blob", files: map[string]string{"refs/replace/{HEAD}": "{BLOB}\n"}},
	{name: "replace HEAD in a cycle of two", files: map[string]string{"refs/replace/{HEAD}": "{OTHER}\n", "refs/replace/{OTHER}": "{HEAD}\n"}},
	{name: "replace the root tree by a missing object", files: map[string]string{"refs/replace/{TREE}": "{MISSING}\n"}},
	{name: "replace the subtree by a missing object", files: map[string]string{"refs/replace/{SUBTREE}": "{MISSING}\n"}},
	{name: "replace a blob by a missing object", files: map[string]string{"refs/replace/{BLOB}": "{MISSING}\n"}},
	{name: "replace an unrelated commit by a missing object", files: map[string]string{"refs/replace/{OTHER}": "{MISSING}\n"}},
	{name: "replace an absent object by a missing object", files: map[string]string{"refs/replace/{MISSING2}": "{MISSING}\n"}},
	{name: "a replace ref with a bad name", files: map[string]string{"refs/replace/garbage": "{MISSING}\n"}},
	{name: "a replace ref with a suffixed name", files: map[string]string{"refs/replace/{HEAD}-x": "{MISSING}\n"}},
	{name: "a replace ref with an uppercase name", files: map[string]string{"refs/replace/{HEADUP}": "{MISSING}\n"}},
	{name: "a replace ref in a subdirectory", files: map[string]string{"refs/replace/sub/{HEAD}": "{MISSING}\n"}},
	{name: "duplicate replace refs for an unrelated commit", files: map[string]string{"refs/replace/{OTHER}": "{HEAD}\n", "refs/replace/sub/{OTHER}": "{HEAD}\n"}},
	{name: "a replace ref for HEAD with garbage content", files: map[string]string{"refs/replace/{HEAD}": "garbage\n"}},
	{name: "a replace ref for an unrelated commit with garbage content", files: map[string]string{"refs/replace/{OTHER}": "garbage\n"}},
	{name: "replace HEAD by a missing object, core.useReplaceRefs false", files: map[string]string{"refs/replace/{HEAD}": "{MISSING}\n"},
		config: "[core]\n\tuseReplaceRefs = false\n"},
	{name: "replace HEAD by a missing object, GIT_NO_REPLACE_OBJECTS=1", files: map[string]string{"refs/replace/{HEAD}": "{MISSING}\n"},
		env: map[string]string{"GIT_NO_REPLACE_OBJECTS": "1"}},
	{name: "replace HEAD by a missing object, GIT_NO_REPLACE_OBJECTS empty", files: map[string]string{"refs/replace/{HEAD}": "{MISSING}\n"},
		env: map[string]string{"GIT_NO_REPLACE_OBJECTS": ""}},
	{name: "replace HEAD under refs/replace, GIT_REPLACE_REF_BASE elsewhere", files: map[string]string{"refs/replace/{HEAD}": "{MISSING}\n"},
		env: map[string]string{"GIT_REPLACE_REF_BASE": "refs/other/"}},
	{name: "replace HEAD under GIT_REPLACE_REF_BASE", files: map[string]string{"refs/other/{HEAD}": "{MISSING}\n"},
		env: map[string]string{"GIT_REPLACE_REF_BASE": "refs/other/"}},
	{name: "replace HEAD under GIT_REPLACE_REF_BASE without a slash", files: map[string]string{"refs/other/{HEAD}": "{MISSING}\n"},
		env: map[string]string{"GIT_REPLACE_REF_BASE": "refs/other"}},
	{name: "replace HEAD by a missing object, GIT_REPLACE_REF_BASE empty", files: map[string]string{"refs/replace/{HEAD}": "{MISSING}\n"},
		env: map[string]string{"GIT_REPLACE_REF_BASE": ""}},
	{name: "replace HEAD by a missing object (packed), GIT_REPLACE_REF_BASE empty",
		files: map[string]string{"packed-refs": packedHeader + "{MISSING} refs/replace/{HEAD}\n"}, env: map[string]string{"GIT_REPLACE_REF_BASE": ""}},
	{name: "replace HEAD by a missing object, GIT_REPLACE_REF_BASE the full ref name", files: map[string]string{"refs/replace/{HEAD}": "{MISSING}\n"},
		env: map[string]string{"GIT_REPLACE_REF_BASE": "refs/replace/{HEAD}"}},
	{name: "replace HEAD by a missing object (packed), GIT_REPLACE_REF_BASE the full ref name",
		files: map[string]string{"packed-refs": packedHeader + "{MISSING} refs/replace/{HEAD}\n"}, env: map[string]string{"GIT_REPLACE_REF_BASE": "refs/replace/{HEAD}"}},
	{name: "no replacement refs, GIT_REPLACE_REF_BASE empty", env: map[string]string{"GIT_REPLACE_REF_BASE": ""}},
	{name: "no replacement refs, GIT_REPLACE_REF_BASE the default", env: map[string]string{"GIT_REPLACE_REF_BASE": "refs/replace/"}},
	{name: "refs/stash with garbage", files: map[string]string{"refs/stash": "garbage\n"}},
	{name: "refs/stash to a missing commit", files: map[string]string{"refs/stash": "{MISSING}\n"}},
	{name: "refs/stash to a missing commit, status.showStash", files: map[string]string{"refs/stash": "{MISSING}\n"},
		config: "[status]\n\tshowStash = true\n"},
	{name: "a garbage stash reflog, status.showStash", files: map[string]string{"refs/stash": "{HEAD}\n", "logs/refs/stash": "garbage\n"},
		config: "[status]\n\tshowStash = true\n"},
	{name: "a branch with garbage", files: map[string]string{"refs/heads/other": "garbage\n"}},
	{name: "a tag to a missing object", files: map[string]string{"refs/tags/t": "{MISSING}\n"}},
	{name: "a dangling remote HEAD", files: map[string]string{"refs/remotes/origin/HEAD": "ref: refs/remotes/origin/missing\n"}},
	{name: "an upstream at a missing commit", files: map[string]string{"refs/remotes/origin/main": "{MISSING}\n"},
		config: "[branch \"main\"]\n\tremote = origin\n\tmerge = refs/heads/main\n"},
	{name: "an upstream at a missing commit, status.branch", files: map[string]string{"refs/remotes/origin/main": "{MISSING}\n"},
		config: "[branch \"main\"]\n\tremote = origin\n\tmerge = refs/heads/main\n[status]\n\tbranch = true\n"},
	{name: "an upstream with garbage", files: map[string]string{"refs/remotes/origin/main": "garbage\n"},
		config: "[branch \"main\"]\n\tremote = origin\n\tmerge = refs/heads/main\n"},
	{name: "a linked worktree HEAD with garbage", files: map[string]string{"worktrees/x/HEAD": "garbage\n", "worktrees/x/gitdir": "/nowhere/.git\n"}},
	{name: "a linked worktree gitdir with garbage", files: map[string]string{"worktrees/x/HEAD": "ref: refs/heads/main\n", "worktrees/x/gitdir": "garbage"}},
	{name: "an empty linked worktree directory", files: map[string]string{"worktrees/x/": ""}},
	{name: "a pack .keep file", files: map[string]string{"objects/pack/pack-{MISSING}.keep": ""}},
	{name: "a garbage pack and index", files: map[string]string{"objects/pack/pack-{MISSING}.pack": "garbage", "objects/pack/pack-{MISSING}.idx": "garbage"}},
	{name: "a garbage pack index alone", files: map[string]string{"objects/pack/pack-{MISSING}.idx": "garbage"}},
	{name: "a garbage pack alone", files: map[string]string{"objects/pack/pack-{MISSING}.pack": "garbage"}},
	{name: "a promisor file", files: map[string]string{"objects/pack/pack-{MISSING}.promisor": ""}},
	{name: "a garbage commit-graph chain", files: map[string]string{"objects/info/commit-graphs/commit-graph-chain": "garbage\n"}},
	{name: "a commit-graph chain to a missing graph", files: map[string]string{"objects/info/commit-graphs/commit-graph-chain": "{MISSING}\n"}},
	{name: "a garbage commit-graph in a chain", files: map[string]string{"objects/info/commit-graphs/commit-graph-chain": "{MISSING}\n",
		"objects/info/commit-graphs/graph-{MISSING}.graph": "garbage"}},
	{name: "a corrupt HEAD commit object", files: map[string]string{"{HEADOBJ}": "garbage"}},
	{name: "a corrupt unrelated loose object", files: map[string]string{"{MISSING2OBJ}": "garbage"}},
	{name: "grafts making HEAD a root", files: map[string]string{"info/grafts": "{HEAD}\n"}},
	{name: "grafts to a missing parent", files: map[string]string{"info/grafts": "{HEAD} {MISSING}\n"}},
	{name: "GIT_NAMESPACE set", env: map[string]string{"GIT_NAMESPACE": "x"}},
	{name: "GIT_SHALLOW_FILE naming an absent file", env: map[string]string{"GIT_SHALLOW_FILE": "/nonexistent-callsheet-shallow"}},
	{name: "GIT_GRAFT_FILE naming an absent file", env: map[string]string{"GIT_GRAFT_FILE": "/nonexistent-callsheet-grafts"}},
}

// apply writes the scenario into the repository's git directory.
func (s stateScenario) apply(t testing.TB, gitDir string, o stateObjects) {
	t.Helper()
	for p, c := range s.files {
		full := filepath.Join(gitDir, filepath.FromSlash(o.expand(p)))
		if strings.HasSuffix(p, "/") {
			if err := os.MkdirAll(full, 0o755); err != nil {
				t.Fatal(err)
			}
			continue
		}
		os.MkdirAll(filepath.Dir(full), 0o755)
		os.Remove(full)
		if err := os.WriteFile(full, []byte(o.expand(c)), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if s.config != "" {
		f, err := os.OpenFile(filepath.Join(gitDir, "config"), os.O_APPEND|os.O_WRONLY, 0)
		if err != nil {
			t.Fatal(err)
		}
		f.WriteString(s.config)
		f.Close()
	}
}

// statePolicy are the scenarios Callsheet refuses although Git accepts
// them: an active replacement of HEAD's commit or of any of its trees
// (Git's view of the commit would not be the commit pushed or compared;
// git status reads the subtrees whenever it compares them), a promisor
// pack (a partial clone), a ref namespace, a shallow file named by the
// environment and GIT_REPLACE_REF_BASE set to any value.
var statePolicy = map[string]bool{
	"replace HEAD by another commit": true, "replace the subtree by a missing object": true, "a promisor file": true,
	"GIT_NAMESPACE set": true, "GIT_SHALLOW_FILE naming an absent file": true,
	// GIT_REPLACE_REF_BASE set to any value (the owner's decision).
	"replace HEAD under refs/replace, GIT_REPLACE_REF_BASE elsewhere": true, "no replacement refs, GIT_REPLACE_REF_BASE empty": true,
	"no replacement refs, GIT_REPLACE_REF_BASE the default": true,
}

func TestGitStateOracle(t *testing.T) {
	f, err := os.Open(filepath.Join("testdata", "gitstate_oracle.tsv"))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	verdicts := map[string]bool{}
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		if l := sc.Text(); !strings.HasPrefix(l, "#") {
			name, v, _ := strings.Cut(l, "\t")
			verdicts[name] = v == "accept"
		}
	}
	if len(verdicts) != len(stateScenarios) {
		t.Fatalf("%d verdicts for %d scenarios (regenerate the table)", len(verdicts), len(stateScenarios))
	}
	run := sampler(t)
	for i, s := range stateScenarios {
		if !run(i) {
			continue
		}
		accept, ok := verdicts[s.name]
		if !ok {
			t.Fatalf("no verdict for %q", s.name)
		}
		files := map[string]fspec{"f": reg("f\n"), "d/g": reg("g\n")}
		r := newRepo(t, files)
		other, err := testkit.WriteCommit(r.store, r.treeOf(files), nil, "other", testkit.FixedWhen)
		if err != nil {
			t.Fatal(err)
		}
		o := stateObjects{head: r.head.String(), tree: r.treeOf(files).String(), subtree: r.treeOf(map[string]fspec{"g": reg("g\n")}).String(),
			blob: plumbing.ComputeHash(plumbing.BlobObject, []byte("f\n")).String(), other: other.String(),
			missing: strings.Repeat("ab", 20), missing2: strings.Repeat("cd", 20)}
		s.apply(t, filepath.Join(r.root, ".git"), o)
		m := map[string]string{"HOME": tempDir(t), "GIT_CONFIG_NOSYSTEM": "1"}
		for k, v := range s.env {
			m[k] = o.expand(v)
		}
		err = pushLocal(t, r.root, envOf(m))
		want := accept && !statePolicy[s.name]
		switch {
		case want && err != nil:
			t.Errorf("%s: Git accepts it, Push refused: %v", s.name, err)
		case !want && err == nil:
			t.Errorf("%s: Git refuses it (or a documented policy), Push accepted", s.name)
		case !want && contract.CodeOf(err) != contract.CodeInvalidArgument && contract.CodeOf(err) != contract.CodeInternal:
			t.Errorf("%s: refused with %v", s.name, err)
		}
	}
}

// TestReplacementBases: GIT_REPLACE_REF_BASE set to any value, the empty
// string included, is refused (code review r7; Git treats an empty base
// as every ref and aborts on a complete ref name); unset is the default
// refs/replace/, loose and packed.
func TestReplacementBases(t *testing.T) {
	files := map[string]fspec{"f": reg("f\n")}
	r := newRepo(t, files)
	head := r.head.String()
	push := func(m map[string]string) error {
		m["HOME"], m["GIT_CONFIG_NOSYSTEM"] = tempDir(t), "1"
		return pushLocal(t, r.root, envOf(m))
	}
	refused := func(err error) bool { return contract.TransferReason(err) == contract.ReasonUnsupportedRepository }
	// No replacement refs: unset is accepted, any set value refused.
	if err := push(map[string]string{}); err != nil {
		t.Fatalf("unset, no replacements: %v", err)
	}
	bases := []string{"", "refs/replace/", "refs/replace/" + head, "refs/rep", "/abs/", "refs/../x/", "other/"}
	for _, base := range bases {
		if err := push(map[string]string{"GIT_REPLACE_REF_BASE": base}); !refused(err) {
			t.Errorf("base %q, no replacements: %v", base, err)
		}
		if err := push(map[string]string{"GIT_REPLACE_REF_BASE": base, "GIT_NO_REPLACE_OBJECTS": "1"}); !refused(err) {
			t.Errorf("base %q with GIT_NO_REPLACE_OBJECTS: %v", base, err)
		}
	}
	// A broken HEAD replacement, loose then packed.
	for _, packed := range []bool{false, true} {
		if packed {
			os.Remove(filepath.Join(r.root, ".git/refs/replace", head))
			mustWrite(t, filepath.Join(r.root, ".git/packed-refs"), packedHeader+strings.Repeat("ab", 20)+" refs/replace/"+head+"\n")
		} else {
			mustWrite(t, filepath.Join(r.root, ".git/refs/replace", head), strings.Repeat("ab", 20)+"\n")
		}
		if err := push(map[string]string{}); !refused(err) {
			t.Errorf("unset base, packed %v: %v", packed, err)
		}
		for _, base := range bases {
			if err := push(map[string]string{"GIT_REPLACE_REF_BASE": base}); !refused(err) {
				t.Errorf("base %q, packed %v: %v", base, packed, err)
			}
		}
		if err := push(map[string]string{"GIT_NO_REPLACE_OBJECTS": ""}); err != nil {
			t.Errorf("replacements disabled, packed %v: %v", packed, err)
		}
	}
	os.Remove(filepath.Join(r.root, ".git/packed-refs"))
	os.MkdirAll(filepath.Join(r.root, ".git/refs/replace"), 0o755)
	os.Symlink("/nowhere", filepath.Join(r.root, ".git/refs/replace/link"))
	if err := push(map[string]string{}); !refused(err) {
		t.Errorf("symlinked replace ref: %v", err)
	}
}
