package function

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/wedevwork/callsheet/internal/testkit"
)

type gitReport struct {
	PathEmpty      bool              `json:"path_empty"`
	GitLookupError string            `json:"git_lookup_error"`
	Scenarios      map[string]string `json:"scenarios"`
	Order          []string          `json:"order"`
	Facts          map[string]string `json:"facts"`
	Transcript     []struct {
		Method        string `json:"method"`
		Path          string `json:"path"`
		Query         string `json:"query"`
		Status        int    `json:"status"`
		RequestBytes  int64  `json:"request_bytes"`
		ResponseBytes int64  `json:"response_bytes"`
	} `json:"transcript"`
}

var gitScenarios = []string{
	"seed-and-clone",
	"task-ref-push-and-observer-fetch",
	"incremental-push-and-persistence",
	"tls-wrong-ca",
	"tls-san-mismatch",
	"stale-old-ref",
	"racing-writers",
	"absent-object-create",
	"missing-blob-closure",
	"missing-parent-and-tree",
	"blob-as-new",
	"truncated-pack",
	"invalid-ref-delete-multi",
	"unsupported-capability",
	"cancel-stalled-body",
	"cancel-before-publication",
	"promotion-failure-then-recovery",
}

// TestFP5GitRoundTrip builds the spike package's own test binary and runs
// every risk-spikes.md git scenario in it as a subprocess whose PATH is a new
// empty directory, then asserts its exit status and parsed per-scenario
// results, including commit/tree/ref preservation and transport capture.
func TestFP5GitRoundTrip(t *testing.T) {
	root := testkit.MustRepoRoot(t)
	bin := testkit.BuildTestBinary(t, "./internal/spikes/gittransport", "gittransport")
	empty := testkit.EmptyDir(t)
	results := filepath.Join(t.TempDir(), "git-results.json")
	env := testkit.EnvWithout(os.Environ(), append([]string{"PATH", "HOME", "GIT_CONFIG_PARAMETERS", "GIT_CONFIG_COUNT"}, testkit.ProxyVars...),
		"PATH="+empty, "HOME="+t.TempDir(), "CALLSHEET_GIT_SUBPROCESS=1", "CALLSHEET_GIT_SCENARIO_RESULTS="+results)
	cmd := exec.Command(bin, "-test.run=^TestGitScenarios$", "-test.v", "-test.count=1", "-test.timeout=170s")
	cmd.Dir = filepath.Join(root, "internal", "spikes", "gittransport")
	cmd.Env = env
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	err := cmd.Run()
	var ee *exec.ExitError
	if err != nil && !errors.As(err, &ee) {
		t.Fatalf("helper did not run: %v", err)
	}
	if err != nil {
		t.Fatalf("PATH-empty git scenarios failed (%v):\n%s", err, out.String())
	}
	if !strings.Contains(out.String(), "--- PASS: TestGitScenarios") {
		t.Fatalf("scenario test did not pass:\n%s", out.String())
	}
	b, rerr := os.ReadFile(results)
	if rerr != nil {
		t.Fatalf("results: %v\n%s", rerr, out.String())
	}
	var rep gitReport
	if err := json.Unmarshal(b, &rep); err != nil {
		t.Fatal(err)
	}
	if !rep.PathEmpty || !strings.Contains(rep.GitLookupError, "not found") {
		t.Fatalf("PATH not proven empty: %+v", rep.GitLookupError)
	}
	if strings.Join(rep.Order, ",") != strings.Join(gitScenarios, ",") {
		t.Fatalf("scenario order = %v", rep.Order)
	}
	for _, name := range gitScenarios {
		if rep.Scenarios[name] != "pass" {
			t.Errorf("scenario %s = %q", name, rep.Scenarios[name])
		}
		if !strings.Contains(out.String(), "--- PASS: TestGitScenarios/"+name) {
			t.Errorf("no PASS line for %s", name)
		}
	}
	f := rep.Facts
	for _, k := range []string{"c0", "c1", "c2", "c0_tree", "c1_tree", "final_main", "final_t1"} {
		if len(f[k]) != 40 {
			t.Fatalf("fact %s = %q", k, f[k])
		}
	}
	if f["final_main"] != f["c0"] || f["final_t1"] != f["c2"] || f["c1"] == f["c0"] || f["c2"] == f["c1"] || f["c0_tree"] == f["c1_tree"] {
		t.Fatalf("ref preservation facts = %v", f)
	}
	if !strings.Contains(f["san_error"], "not 127.0.0.2") || strings.Contains(f["receive_capabilities"], "delete-refs") {
		t.Fatalf("TLS/capability facts = %v", f)
	}
	counts := map[string]int{}
	for _, x := range rep.Transcript {
		counts[x.Method+" "+x.Path+"?"+x.Query]++
		if x.RequestBytes < 0 || x.ResponseBytes < 0 {
			t.Fatalf("bad byte counts %+v", x)
		}
	}
	for _, k := range []string{
		"GET /test/git/repo.git/info/refs?service=git-upload-pack",
		"POST /test/git/repo.git/git-upload-pack?",
		"GET /test/git/repo.git/info/refs?service=git-receive-pack",
		"POST /test/git/repo.git/git-receive-pack?",
	} {
		if counts[k] == 0 {
			t.Fatalf("transport capture lacks %s: %v", k, counts)
		}
	}
}
