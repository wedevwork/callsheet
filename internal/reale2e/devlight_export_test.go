package reale2e

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Export integrity of the verification helpers (review 2d.4 round 1, C2):
// every scratch export is digested after creation and again after the
// commands ran, both manifests are kept in the receipt, and an export a
// worker command mutated or deleted fails the run, light and full alike.

func TestDevReviewExportIntegrity(t *testing.T) {
	t.Parallel()
	// The light recipe: each of its three exports, each mutated and
	// deleted after the red command (before the export's own last use for
	// after/, which the green command and the suites still read).
	for _, export := range []string{"baseline", "before", "after"} {
		for _, kind := range []string{"mutated", "deleted", "added"} {
			name := export + "_" + kind
			t.Run("light_"+name, func(t *testing.T) {
				t.Parallel()
				f := newLightReviewFix(t, nil)
				done := false
				f.wrap = func(s ProcSpec, inner func(ProcSpec) behavior) behavior {
					b := inner(s)
					if !done && strings.HasSuffix(s.Dir, "/before") && strings.Join(s.Args, " ") == "test -count=1 -json ./..." {
						done = true
						dir := filepath.Join(filepath.Dir(s.Dir), export)
						after := b.after
						b.after = func() {
							if after != nil {
								after()
							}
							p := filepath.Join(dir, "main_test.go")
							switch kind {
							case "mutated":
								os.WriteFile(p, []byte("package main\n"), 0o600)
							case "deleted":
								os.Remove(p)
							case "added":
								os.WriteFile(filepath.Join(dir, "extra_test.go"), []byte("package main\n"), 0o600)
							}
						}
					}
					return b
				}
				rec, out, code := f.run()
				if code != 1 || rec.Status != reviewFailed || rec.Code != "export_changed" {
					t.Fatalf("exit %d status %s code %s\n%s", code, rec.Status, rec.Code, out)
				}
				ev := DeriveLightReviewEvidence(f.evidenceInput(out, 1, markerReviewApproved))
				if ev.Status == evidenceVerified {
					t.Fatalf("a changed export verified: %+v", ev)
				}
			})
		}
	}
	// The full recipe's single after/ export.
	for _, kind := range []string{"mutated", "deleted", "added"} {
		kind := kind
		t.Run("full_after_"+kind, func(t *testing.T) {
			t.Parallel()
			f := newReviewFix(t)
			done := false
			inner := f.script.script
			f.launcher = &fakeLauncher{script: func(s ProcSpec) behavior {
				b := inner(s)
				if !done && strings.HasSuffix(s.Dir, "/after") && strings.Join(s.Args, " ") == "test ./..." {
					done = true
					dir := s.Dir
					after := b.after
					b.after = func() {
						if after != nil {
							after()
						}
						switch kind {
						case "mutated":
							os.WriteFile(filepath.Join(dir, "main_test.go"), []byte("package main\n"), 0o600)
						case "deleted":
							os.Remove(filepath.Join(dir, "design.md"))
						case "added":
							os.WriteFile(filepath.Join(dir, "extra.txt"), []byte("x\n"), 0o600)
						}
					}
				}
				return b
			}}
			out, _, code := verifyHelperRun(reviewEnv{cwd: f.co, clock: f.clock, launcher: f.launcher, path: "/usr/bin:/bin"}, f.argv())
			var rec DevReviewRun
			if err := json.Unmarshal([]byte(receiptLine(out)), &rec); err != nil {
				t.Fatal(err)
			}
			if code != 1 || rec.Status != reviewFailed || rec.Code != "export_changed" {
				t.Fatalf("exit %d status %s code %s\n%s", code, rec.Status, rec.Code, out)
			}
		})
	}
	t.Run("light_derivation", func(t *testing.T) {
		t.Parallel()
		// A receipt claiming success binds both manifests of every export to
		// the pinned objects.
		f := newLightReviewFix(t, nil)
		_, out, code := f.run()
		if code != 0 {
			t.Fatalf("control %d", code)
		}
		if ev := DeriveLightReviewEvidence(f.evidenceInput(out, 0, markerReviewApproved)); ev.Status != evidenceVerified {
			t.Fatalf("control %+v", ev)
		}
		line, _, _ := strings.Cut(out, "\n")
		var run DevLightReviewRun
		json.Unmarshal([]byte(line), &run)
		forge := func(mut func(r *DevLightReviewRun)) string {
			r := run
			r.Exports = append([]LightExport{}, run.Exports...)
			mut(&r)
			b, _ := json.Marshal(r)
			tr, _ := renderLightReviewReportLines(r, append(b, '\n'))
			return string(b) + "\n" + tr
		}
		for name, mut := range map[string]func(r *DevLightReviewRun){
			"after_changed": func(r *DevLightReviewRun) {
				r.Exports[1].After = []LightInput{{Path: "main_test.go", SHA256: strings.Repeat("0", 64)}}
			},
			"after_deleted":   func(r *DevLightReviewRun) { r.Exports[2].After = r.Exports[2].After[1:] },
			"created_changed": func(r *DevLightReviewRun) { r.Exports[0].Created = append(r.Exports[0].Created, LightInput{Path: "x"}) },
			"no_after":        func(r *DevLightReviewRun) { r.Exports[0].After = nil },
			"no_exports":      func(r *DevLightReviewRun) { r.Exports = nil },
			"inputs_forged":   func(r *DevLightReviewRun) { r.Exports[1].Inputs = r.Exports[2].Inputs },
		} {
			if ev := DeriveLightReviewEvidence(f.evidenceInput(forge(mut), 0, markerReviewApproved)); ev.Status == evidenceVerified {
				t.Errorf("%s: verified", name)
			}
		}
	})
	t.Run("full_derivation", func(t *testing.T) {
		t.Parallel()
		f := newReviewFix(t)
		_, out, code := f.run()
		if code != 0 {
			t.Fatalf("control %d", code)
		}
		bash := "/bin/bash -lc '" + strings.Join(f.argv(), " ") + "'"
		derive := func(o string, require bool) DevReviewEvidence {
			in := f.evidenceInput(bash, o, 0, markerReviewApproved)
			in.RequireExport = require
			return DeriveDevReviewEvidence(in)
		}
		if ev := derive(out, true); ev.Status != evidenceVerified {
			t.Fatalf("control %+v", ev)
		}
		line, _, _ := strings.Cut(out, "\n")
		var run DevReviewRun
		json.Unmarshal([]byte(line), &run)
		forge := func(mut func(r *DevReviewRun)) string {
			r := run
			if run.Export != nil {
				e := *run.Export
				r.Export = &e
			}
			mut(&r)
			b, _ := json.Marshal(r)
			tr, _ := renderReviewReportLines(r, append(b, '\n'))
			return string(b) + "\n" + tr
		}
		changed := forge(func(r *DevReviewRun) { r.Export.After = r.Export.After[1:] })
		if ev := derive(changed, true); ev.Status == evidenceVerified {
			t.Fatal("a changed export verified")
		}
		historical := forge(func(r *DevReviewRun) { r.Export = nil })
		if ev := derive(historical, true); ev.Status == evidenceVerified || ev.Code != "review_export_mismatch" {
			t.Fatalf("a current receipt without manifests: %+v", ev)
		}
		// A historical (v1) receipt never had manifests: still derived.
		if ev := derive(historical, false); ev.Status != evidenceVerified {
			t.Fatalf("a historical receipt: %+v", ev)
		}
	})
}
