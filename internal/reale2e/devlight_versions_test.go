package reale2e

import (
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// Evidence versions (design 12a-dev-flow-light, FP-6 and FP-7): new
// bundles are callsheet-dev-evidence/v2 with inert captures; a historical
// callsheet-dev-full-evidence/v1 bundle is read at its original paths
// under its original layout and keeps its original full-only scope.

// v2SchemaRE matches a v2 development schema field (indented or compact).
var v2SchemaRE = regexp.MustCompile(`("schema":\s*")callsheet-dev-([a-z-]+)/v2"`)

// downgradeToV1 rewrites a current full bundle into the historical v1
// form (a synthetic v1 fixture; no committed or owner evidence is read):
// captures unpacked at their logical paths, v1 schema identifiers and no
// storage paths, then the inventory and derived files recomputed by the
// version-dispatching checker.
func downgradeToV1(t *testing.T, dir string) {
	t.Helper()
	downgradeCaptures(t, dir)
	editJSON(t, dir, "setup/sources.json", func(m map[string]any) {
		for _, s := range m["data"].([]any) {
			delete(s.(map[string]any), "storage_path")
		}
	})
	reforge(t, dir)
}

// downgradeCaptures unpacks a current bundle's inert captures at their
// logical paths and rewrites its v2 schema identifiers to v1.
func downgradeCaptures(t *testing.T, dir string) {
	t.Helper()
	var moves [][2]string
	filepath.WalkDir(filepath.Join(dir, capturedDir), func(p string, d fs.DirEntry, err error) error {
		if err == nil && d.Type().IsRegular() {
			rel, _ := filepath.Rel(filepath.Join(dir, capturedDir), p)
			moves = append(moves, [2]string{p, filepath.Join(dir, strings.TrimSuffix(rel, ".bin"))})
		}
		return nil
	})
	for _, m := range moves {
		if err := os.MkdirAll(filepath.Dir(m[1]), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.Rename(m[0], m[1]); err != nil {
			t.Fatal(err)
		}
	}
	os.RemoveAll(filepath.Join(dir, capturedDir))
	filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil || !d.Type().IsRegular() || !strings.HasSuffix(p, ".json") || strings.Contains(p, "repo.git") {
			return nil
		}
		b, _ := os.ReadFile(p)
		out := v2SchemaRE.ReplaceAll(b, []byte(`${1}callsheet-dev-full-${2}/v1"`))
		if string(out) != string(b) {
			os.WriteFile(p, out, 0o600)
		}
		return nil
	})
}

func TestDevEvidenceVersions(t *testing.T) {
	t.Parallel()
	t.Run("v2_full", func(t *testing.T) {
		t.Parallel()
		dir := devPassingBundle(t)
		if m := bundleDoc[DevManifest](t, dir, devManifestFile); m.Schema != DevBundleSchema || m.Scenario != scenarioFull {
			t.Fatalf("manifest %s %s", m.Schema, m.Scenario)
		}
		if code, rep, errOut := devCheck(t, dir); code != 0 || rep.Schema != devKind("report") {
			t.Fatalf("v2 full %d %s %v %s", code, rep.Schema, devCodes(rep), errOut)
		}
	})
	t.Run("v1_full", func(t *testing.T) {
		t.Parallel()
		dir := devPassingBundle(t)
		downgradeToV1(t, dir)
		if _, err := os.Stat(filepath.Join(dir, "setup", "sources", filepath.FromSlash(DevFlowDir), DevPlaybook)); err != nil {
			t.Fatalf("the v1 fixture has no unpacked capture: %v", err)
		}
		before := snapshotTree(t, dir)
		code, rep, errOut := devCheck(t, dir)
		if code != 0 || rep.Schema != kindFor(devBundleV1, "report") || rep.Scope != devScope {
			t.Fatalf("v1 full %d %s %v %s", code, rep.Schema, devCodes(rep), errOut)
		}
		if after := snapshotTree(t, dir); after != before {
			t.Fatal("the check changed a v1 bundle")
		}
		// v1 keeps its original layout: an inert capture path is not part of
		// it, and a v2 schema record is refused.
		os.MkdirAll(filepath.Join(dir, capturedDir), 0o700)
		os.WriteFile(filepath.Join(dir, capturedDir, "x.bin"), []byte("x"), 0o600)
		if code, _, _ := devCheck(t, dir); code != 2 {
			t.Fatalf("a v1 bundle with an inert capture: %d", code)
		}
	})
	t.Run("v2_light", func(t *testing.T) {
		t.Parallel()
		dir := lightPassingBundle(t)
		if m := bundleDoc[DevManifest](t, dir, devManifestFile); m.Schema != DevBundleSchema || m.Scenario != scenarioLight {
			t.Fatalf("manifest %s %s", m.Schema, m.Scenario)
		}
		if code, rep, errOut := devCheck(t, dir); code != 0 || rep.Scope != devLightScope || !strings.Contains(rep.Claim, "light") {
			t.Fatalf("v2 light %d %s %v %s", code, rep.Scope, devCodes(rep), errOut)
		}
	})
	t.Run("legacy_v3", func(t *testing.T) {
		t.Parallel()
		// A legacy bundle keeps its own checker and scope.
		var out, errb strings.Builder
		if code := CheckEvidence(passingBundle(t), &out, &errb); code != 0 || !strings.Contains(out.String(), ReportSchema) {
			t.Fatalf("legacy v3 %d %s", code, errb.String())
		}
	})
	t.Run("execution_only", func(t *testing.T) {
		t.Parallel()
		d := t.TempDir()
		os.WriteFile(filepath.Join(d, DevExecutionFile), []byte(`{"schema":"`+DevExecutionSchema+`"}`), 0o600)
		var out, errb strings.Builder
		if code := CheckEvidence(d, &out, &errb); code != 2 {
			t.Fatalf("execution-only %d", code)
		}
	})
	for name, c := range map[string]struct {
		mutate func(t *testing.T, dir string)
		code   int
	}{
		// A light bundle cannot borrow full records or the v1 version.
		"light_as_v1": {func(t *testing.T, dir string) {
			editJSON(t, dir, devManifestFile, func(m map[string]any) { m["schema"] = DevBundleSchemaV1 })
		}, 2},
		"light_as_full": {func(t *testing.T, dir string) {
			editJSON(t, dir, devManifestFile, func(m map[string]any) { m["scenario"] = scenarioFull })
		}, 2},
		"light_full_record": {func(t *testing.T, dir string) {
			os.MkdirAll(filepath.Join(dir, "receipts"), 0o700)
			os.WriteFile(filepath.Join(dir, "receipts", "01.json"), []byte("{}\n"), 0o600)
		}, 2},
		"light_mixed_record": {func(t *testing.T, dir string) {
			editJSON(t, dir, lightDecisionFile, func(m map[string]any) { m["schema"] = kindFor(devBundleV1, "intake-decision") })
		}, 2},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			dir := lightPassingBundle(t)
			c.mutate(t, dir)
			if code, rep, errOut := devCheck(t, dir); code != c.code {
				t.Fatalf("exit %d want %d %v %s", code, c.code, devCodes(rep), errOut)
			}
		})
	}
	for name, c := range map[string]struct {
		mutate func(t *testing.T, dir string)
		code   int
	}{
		"mixed_record": {func(t *testing.T, dir string) {
			editJSON(t, dir, devLedgerFile, func(m map[string]any) { m["schema"] = kindFor(devBundleV1, "ledger") })
		}, 2},
		"unknown_version": {func(t *testing.T, dir string) {
			editJSON(t, dir, devManifestFile, func(m map[string]any) { m["schema"] = "callsheet-dev-evidence/v3" })
		}, 2},
		"unknown_scenario": {func(t *testing.T, dir string) {
			editJSON(t, dir, devManifestFile, func(m map[string]any) { m["scenario"] = "both" })
		}, 2},
		"unpacked_capture": {func(t *testing.T, dir string) {
			p := filepath.Join(dir, "setup", "sources", filepath.FromSlash(DevFlowDir))
			os.MkdirAll(p, 0o700)
			os.WriteFile(filepath.Join(p, DevPlaybook), mustRead(t, filepath.Join(dir, filepath.FromSlash(capturedPath("setup/sources/"+DevFlowDir+"/"+DevPlaybook)))), 0o600)
		}, 2},
		"go_file": {func(t *testing.T, dir string) {
			os.WriteFile(filepath.Join(dir, "main.go"), []byte("package main\n"), 0o600)
		}, 2},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			dir := devPassingBundle(t)
			c.mutate(t, dir)
			if code, rep, errOut := devCheck(t, dir); code != c.code {
				t.Fatalf("exit %d want %d %v %s", code, c.code, devCodes(rep), errOut)
			}
		})
	}
}

// snapshotTree is a directory's every path, mode and content digest.
func snapshotTree(t *testing.T, dir string) string {
	t.Helper()
	var b strings.Builder
	filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		st, _ := os.Lstat(p)
		rel, _ := filepath.Rel(dir, p)
		b.WriteString(rel + " " + st.Mode().String())
		if d.Type().IsRegular() {
			data, _ := os.ReadFile(p)
			b.WriteString(" " + sha256Hex(data))
		}
		b.WriteString("\n")
		return nil
	})
	return b.String()
}
