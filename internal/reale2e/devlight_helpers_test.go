package reale2e

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// Shared light fixtures (design 12a-dev-flow-light, Function tests: pure
// mutation tests share immutable synthetic bundles and mutate fresh
// copies): one passing light attempt, one passing scoped qualification and
// one passing combined exercise, each produced once by the offline rig and
// copied per test.

var (
	lightPassing      devAttempt
	probeLightPassing devAttempt
	exercisePassing   devAttempt
)

// cachedCopy produces a's directory once with make and returns a fresh
// copy of it.
func cachedCopy(t *testing.T, a *devAttempt, name string, make func() (string, error)) string {
	t.Helper()
	a.Do(func() {
		src, err := make()
		if err != nil {
			a.err = err
			return
		}
		a.dir = filepath.Join(bundleCacheRoot, name)
		a.err = copyTree(src, a.dir)
	})
	if a.err != nil {
		t.Fatal(a.err)
	}
	dst := filepath.Join(t.TempDir(), name)
	if err := copyTree(a.dir, dst); err != nil {
		t.Fatal(err)
	}
	return dst
}

// lightPassingBundle is a fresh copy of the shared passing light attempt
// (one fix loop, so every light record family is reached).
func lightPassingBundle(t *testing.T) string {
	t.Helper()
	return cachedCopy(t, &lightPassing, "light-passing", func() (string, error) {
		r := newLightRig(t, lightLoopSteps())
		if code := r.run(); code != 0 {
			return "", fmt.Errorf("the shared light attempt failed (%d): %s %s\n%s", code, r.d.stopCode, r.d.stopReason, r.f.stderr.String())
		}
		return r.f.bundle(), nil
	})
}

// lightLoopSteps is a light attempt with one fix loop.
func lightLoopSteps() []devAct {
	return []devAct{{marker: markerTriageConf, designRequired: designRequiredNo}, {marker: markerImplComplete}, {marker: markerReviewChanges},
		{marker: markerFixesComplete}, {marker: markerReviewApproved}}
}

// probeLightBundle is a fresh copy of the shared qualified both-scope
// probe.
func probeLightBundle(t *testing.T) string {
	t.Helper()
	return cachedCopy(t, &probeLightPassing, "probe-light", func() (string, error) {
		r := probeLightRig(t, probeScopeBoth)
		if code := r.run(); code != 0 {
			return "", fmt.Errorf("the shared light probe failed (%d): %s %s", code, r.d.stopCode, r.d.stopReason)
		}
		return r.f.bundle(), nil
	})
}

// exerciseBundle is a fresh copy of the shared passing combined exercise
// root.
func exerciseBundle(t *testing.T) string {
	t.Helper()
	return cachedCopy(t, &exercisePassing, "exercise", func() (string, error) {
		x := newExerciseRig(t, happy(), lightHappy())
		if code := x.run(); code != 0 {
			return "", fmt.Errorf("the shared exercise failed (%d): %s", code, x.full.f.stderr.String())
		}
		return x.root(), nil
	})
}

// lightReforge recomputes a mutated light bundle's inventory and derived
// files (a capable forger) and returns its offline check.
func lightReforge(t *testing.T, dir string) (int, DevReport) {
	t.Helper()
	reforge(t, dir)
	code, rep, _ := devCheck(t, dir)
	return code, rep
}

// failed is the report's failing criterion IDs.
func failed(rep DevReport) []string {
	var out []string
	for _, c := range rep.Criteria {
		if c.Result == ResultCriterionFail {
			out = append(out, c.ID)
		}
	}
	slices.Sort(out)
	return out
}

// eventSeqs are the bundle's event sequences matching typ, hop prefix and
// code ("" any).
func eventSeqs(t *testing.T, dir, typ, hopPrefix, code string) []int {
	t.Helper()
	var out []int
	for _, l := range strings.Split(string(mustRead(t, filepath.Join(dir, "events.jsonl"))), "\n") {
		if l == "" {
			continue
		}
		var e Event
		if err := json.Unmarshal([]byte(l), &e); err != nil {
			t.Fatal(err)
		}
		if (typ == "" || e.Type == typ) && strings.HasPrefix(e.Hop, hopPrefix) && (code == "" || e.Code == code) {
			out = append(out, e.Seq)
		}
	}
	return out
}

// exists reports a bundle path.
func exists(dir, rel string) bool {
	_, err := os.Lstat(filepath.Join(dir, filepath.FromSlash(rel)))
	return err == nil
}
