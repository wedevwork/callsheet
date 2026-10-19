package devcheck

import (
	"bytes"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// TestRaceSelectionCoversModule keeps the Linux race suite whole after
// internal/reale2e moved to its own race step (code review round 5 of
// design 12a-dev-flow-verify): the shared step's listed patterns each
// select packages, and together with RaceReale2ePackage they select
// exactly the module's packages, each by one step only. A package added
// outside the list fails here instead of silently leaving the race suite.
func TestRaceSelectionCoversModule(t *testing.T) {
	root := filepath.Join("..", "..")
	list := func(patterns ...string) []string {
		t.Helper()
		var stderr bytes.Buffer
		cmd := exec.Command("go", append([]string{"list"}, patterns...)...)
		cmd.Dir, cmd.Stderr = root, &stderr
		out, err := cmd.Output()
		if err != nil || stderr.Len() != 0 {
			t.Fatalf("go list %v: %v %s", patterns, err, stderr.String())
		}
		return strings.Fields(string(out))
	}
	all := list("./...")
	// A pattern that selects nothing makes go list warn "matched no
	// packages" on stderr, which list refuses.
	shared := list(RaceSharedPackages...)
	own := list(RaceReale2ePackage)
	if len(own) != 1 || !strings.HasSuffix(own[0], "/internal/reale2e") {
		t.Fatalf("the reale2e race step selects %v", own)
	}
	selected := map[string]int{}
	for _, p := range append(append([]string{}, shared...), own...) {
		selected[p]++
	}
	for _, p := range all {
		if selected[p] != 1 {
			t.Errorf("%s is selected by %d race steps", p, selected[p])
		}
		delete(selected, p)
	}
	if len(selected) != 0 {
		t.Errorf("race steps select packages outside the module's ./...: %v", selected)
	}
	sorted := append([]string(nil), RaceSharedPackages...)
	sort.Strings(sorted)
	if strings.Join(sorted, " ") != strings.Join(RaceSharedPackages, " ") {
		t.Errorf("the shared race patterns are not sorted: %v", RaceSharedPackages)
	}
}
