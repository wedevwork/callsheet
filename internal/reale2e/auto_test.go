package reale2e

import (
	"encoding/json"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// TestAutoCoordinatorFlow (FP-8) runs both fake coordinator phases through
// launch recording and the checker. Auto init plus the auto-mode argv
// passes setup. An argv mutation, an old allow-list, a non-auto init, a
// missing wait line or a changed MCP path, working directory or session
// fails an existing setup or event-map criterion. Owner sign-off and the
// no-damage check stay mandatory.
func TestAutoCoordinatorFlow(t *testing.T) {
	t.Parallel()
	f := newFakeWorld(t)
	if code := f.attempt(); code != 0 {
		t.Fatalf("attempt = %d\n%s\n%s", code, f.stdout.String(), f.stderr.String())
	}
	specs := f.launcher.phaseSpecs()
	if len(specs) != 2 {
		t.Fatalf("phases %d", len(specs))
	}
	b := f.bundle()
	var l1, l2 LaunchRecord
	readDoc(t, filepath.Join(b, filepath.FromSlash(launchFiles[0])), &l1)
	readDoc(t, filepath.Join(b, filepath.FromSlash(launchFiles[1])), &l2)
	system := string(mustRead(t, filepath.Join(b, "setup", "coordinator-prompt.md")))
	prompts := [2]string{
		string(mustRead(t, filepath.Join(b, "setup", PhasePrompt1))),
		string(mustRead(t, filepath.Join(b, "setup", PhasePrompt2))),
	}
	mcpPath := filepath.Join(f.sup.runtime, "mcp.json")
	for i, got := range [][]string{l1.Argv, l2.Argv} {
		want := launchArgv(i+1, prompts[i], mcpPath, system, testSession)
		if !slices.Equal(got, want) || slices.Contains(got, "--allowed-tools") {
			t.Fatalf("phase %d argv %q", i+1, got)
		}
		if !slices.Equal(specs[i].Args, want) {
			t.Fatalf("phase %d launch %q", i+1, specs[i].Args)
		}
	}
	if l2.ResumeSession != testSession || l1.ResumeSession != "" {
		t.Fatalf("resume %q %q", l1.ResumeSession, l2.ResumeSession)
	}
	loaded, err := LoadBundle(b)
	if err != nil {
		t.Fatal(err)
	}
	c := newChecker(loaded)
	if !c.launchValid(1) || !c.launchValid(2) || len(c.derived.Init) != 2 ||
		c.derived.Init[0].PermissionMode != autoModeFlag || c.derived.Init[1].PermissionMode != autoModeFlag {
		t.Fatalf("checker inits %+v valid %t %t", c.derived.Init, c.launchValid(1), c.launchValid(2))
	}
	r := f.report()
	if r.Result != ResultPass || criterion(r, CritSetup).Result != ResultCriterionPass ||
		criterion(r, CritOwner).Result != ResultCriterionPass || criterion(r, CritOwnerEnv).Result != ResultCriterionPass {
		t.Fatalf("report %v", failedCodes(r))
	}
	v, ok := c.permissionInputs(loaded.Launches[0])
	var waits []string
	for i := range Hops {
		waits = append(waits, waitCommand(v, i))
	}
	if !ok || !waitLinesMatch(system, v, waits) {
		t.Fatal("the passing prompt's wait lines do not match")
	}
	if waitLinesMatch(strings.Replace(system, "\n"+waits[0]+"\n", "\n"+waits[0]+" >/tmp/out\n", 1), v, waits) ||
		waitLinesMatch(system+"capture-wait\n", v, waits) {
		t.Fatal("a redirect or capture-wait matched")
	}
	quoted := *loaded
	ql := *quoted.Launches[0]
	ql.WaitTemplates = slices.Clone(ql.WaitTemplates)
	ql.WaitTemplates[0] = "'/tmp/a*b' wait-task --run /rt --task TASK_ID --hop designer"
	quoted.Launches[0] = &ql
	c.b = &quoted
	if _, _, ok := c.expectedLaunch(1); ok {
		t.Fatal("a metacharacter in the executable was accepted")
	}

	t.Run("old allow-list", func(t *testing.T) {
		t.Parallel()
		bundle := passingBundle(t)
		editDoc(t, bundle, launchFiles[0], func(l *LaunchRecord) { l.Argv = oldAllowListArgv(l.Argv) })
		rebindLaunchHashes(t, bundle)
		_, got, _ := checkBundle(t, bundle)
		if su := criterion(got, CritSetup); !slices.Contains(su.Codes, "launch_shape_invalid") || criterion(got, CritManifest).Result != ResultCriterionPass {
			t.Fatalf("setup %+v (%v)", su, failedCodes(got))
		}
	})
	t.Run("wrong permission mode", func(t *testing.T) {
		t.Parallel()
		bundle := passingBundle(t)
		setStreamPermission(t, bundle, 0, "default", false)
		setStreamPermission(t, bundle, 1, "bypassPermissions", false)
		editDoc(t, bundle, eventMapFile, func(m *EventMap) {
			m.Init[0].PermissionMode = "default"
			m.Init[1].PermissionMode = "bypassPermissions"
		})
		_, got, _ := checkBundle(t, bundle)
		if su := criterion(got, CritSetup); !slices.Contains(su.Codes, "launch_shape_invalid") {
			t.Fatalf("setup %+v (%v)", su, failedCodes(got))
		}
	})
	t.Run("missing permission mode", func(t *testing.T) {
		t.Parallel()
		bundle := passingBundle(t)
		setStreamPermission(t, bundle, 1, "", true)
		editDoc(t, bundle, eventMapFile, func(m *EventMap) { m.Init[1].PermissionMode = "" })
		_, got, _ := checkBundle(t, bundle)
		if su := criterion(got, CritSetup); !slices.Contains(su.Codes, "launch_shape_invalid") {
			t.Fatalf("setup %+v (%v)", su, failedCodes(got))
		}
	})
	t.Run("repeated init", func(t *testing.T) {
		t.Parallel()
		same := passingBundle(t)
		appendRepeatedInit(t, same, 0, autoModeFlag)
		rewriteDerivedMap(t, same)
		_, got, _ := checkBundle(t, same)
		if su := criterion(got, CritSetup); su.Result != ResultCriterionPass || criterion(got, CritEventMap).Result != ResultCriterionPass {
			t.Fatalf("a matching repeated init: setup %+v event %+v (%v)", su, criterion(got, CritEventMap), failedCodes(got))
		}
		other := passingBundle(t)
		appendRepeatedInit(t, other, 1, "default")
		_, got, _ = checkBundle(t, other)
		if ev := criterion(got, CritEventMap); !slices.Contains(ev.Codes, CodeSessionMismatch) {
			t.Fatalf("event map %+v (%v)", ev, failedCodes(got))
		}
	})
	t.Run("missing wait line", func(t *testing.T) {
		t.Parallel()
		bundle := passingBundle(t)
		text := string(mustRead(t, filepath.Join(bundle, "setup", "coordinator-prompt.md")))
		var dropped bool
		lines := strings.Split(text, "\n")
		for i, l := range lines {
			if strings.Contains(l, "wait-task") && strings.Contains(l, "--hop designer") {
				lines = slices.Delete(lines, i, i+1)
				dropped = true
				break
			}
		}
		if !dropped {
			t.Fatal("no designer wait line")
		}
		writeBundleFile(t, bundle, "setup/coordinator-prompt.md", []byte(strings.Join(lines, "\n")))
		_, got, _ := checkBundle(t, bundle)
		if su := criterion(got, CritSetup); !slices.Contains(su.Codes, "launch_shape_invalid") {
			t.Fatalf("setup %+v (%v)", su, failedCodes(got))
		}
	})
	t.Run("modified mcp", func(t *testing.T) {
		t.Parallel()
		bundle := passingBundle(t)
		p := filepath.Join(bundle, "setup", "mcp-config.json")
		data := string(mustRead(t, p))
		const mcpType = `"type": "stdio"`
		if !strings.Contains(data, mcpType) {
			t.Fatalf("mcp config %s", data)
		}
		writeBundleFile(t, bundle, "setup/mcp-config.json", []byte(strings.Replace(data, mcpType, `"type": "http"`, 1)))
		_, got, _ := checkBundle(t, bundle)
		if su := criterion(got, CritSetup); !slices.Contains(su.Codes, "launch_shape_invalid") {
			t.Fatalf("setup %+v (%v)", su, failedCodes(got))
		}
	})
	t.Run("modified cwd", func(t *testing.T) {
		t.Parallel()
		bundle := passingBundle(t)
		editDoc(t, bundle, launchFiles[0], func(l *LaunchRecord) { l.Cwd = "/tmp/not-coord" })
		rebindLaunchHashes(t, bundle)
		_, got, _ := checkBundle(t, bundle)
		if su := criterion(got, CritSetup); !slices.Contains(su.Codes, "launch_shape_invalid") || criterion(got, CritManifest).Result != ResultCriterionPass {
			t.Fatalf("setup %+v (%v)", su, failedCodes(got))
		}
	})
	t.Run("modified session", func(t *testing.T) {
		t.Parallel()
		bundle := passingBundle(t)
		other := "aaaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee"
		editDoc(t, bundle, launchFiles[1], func(l *LaunchRecord) {
			l.ResumeSession = other
			l.Argv = setFlagValue(l.Argv, "--resume", other)
		})
		rebindLaunchHashes(t, bundle)
		_, got, _ := checkBundle(t, bundle)
		if su := criterion(got, CritSetup); !slices.Contains(su.Codes, "session_resume_mismatch") {
			t.Fatalf("setup %+v (%v)", su, failedCodes(got))
		}
	})
	t.Run("sign-off", func(t *testing.T) {
		t.Parallel()
		g := newFakeWorld(t)
		g.term.lines = []string{" no"}
		if code := g.attempt(); code != 1 || g.sup.failCode != "owner_rejected" || len(g.launcher.phaseSpecs()) != 1 || coderDispatched(g.plane) {
			t.Fatalf("no = %d %q phases %d", code, g.sup.failCode, len(g.launcher.phaseSpecs()))
		}
		if ow := criterion(g.report(), CritOwner); ow.Result != ResultCriterionFail || !slices.Contains(ow.Codes, "owner_did_not_approve") {
			t.Fatalf("owner %+v", ow)
		}
		bundle := passingBundle(t)
		removeBundleFile(t, bundle, "owner-decision.json")
		_, got, _ := checkBundle(t, bundle)
		if ow := criterion(got, CritOwner); !slices.Contains(ow.Codes, "decision_missing") {
			t.Fatalf("owner %+v (%v)", ow, failedCodes(got))
		}
	})
	t.Run("no-damage", func(t *testing.T) {
		t.Parallel()
		bundle := passingBundle(t)
		rewriteEnvironment(t, bundle, func(e *envEdit) {
			e.change("claude", "settings.json", func(r *EnvRecord) { r.SHA256 = zeros64 })
		})
		_, got, _ := checkBundle(t, bundle)
		if env := criterion(got, CritOwnerEnv); !slices.Contains(env.Codes, CodeEnvModified) {
			t.Fatalf("owner environment %+v (%v)", env, failedCodes(got))
		}
	})
	// Each phase, crossed with a present non-auto mode and an omitted
	// field. A contradiction is incorrect; missing proof is unobserved.
	t.Run("audit verdict", func(t *testing.T) {
		t.Parallel()
		cases := []struct {
			name    string
			phase   int
			mode    string
			drop    bool
			verdict string
			code    string
			missing string
		}{
			{"phase 1 default", 0, "default", false, VerdictIncorrect, "launch_shape_invalid", ""},
			{"phase 2 default", 1, "default", false, VerdictIncorrect, "launch_shape_invalid", ""},
			{"phase 1 missing", 0, "", true, VerdictUnobserved, "", "phase 1 permission mode"},
			{"phase 2 missing", 1, "", true, VerdictUnobserved, "", "phase 2 permission mode"},
		}
		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				t.Parallel()
				bundle := passingBundle(t)
				setStreamPermission(t, bundle, tc.phase, tc.mode, tc.drop)
				editDoc(t, bundle, eventMapFile, func(m *EventMap) {
					m.Init[tc.phase].PermissionMode = tc.mode
				})
				a, _ := auditOf(t, bundle)
				id := [2]string{"coordinator_phase1", "coordinator_phase2"}[tc.phase]
				row := rowByID(t, a, id)
				if row.Verdict != tc.verdict {
					t.Fatalf("%s verdict %q, want %q (%s)", id, row.Verdict, tc.verdict, row.Observed)
				}
				if tc.code != "" && !slices.Contains(row.Criteria, "code:"+tc.code) {
					t.Fatalf("%s criteria %v", id, row.Criteria)
				}
				if tc.missing != "" && !strings.Contains(row.Observed, "missing: "+tc.missing) {
					t.Fatalf("%s observed %q correction %q", id, row.Observed, row.Correction)
				}
				other := [2]string{"coordinator_phase2", "coordinator_phase1"}[tc.phase]
				if got := rowByID(t, a, other); got.Verdict != VerdictCorrect {
					t.Fatalf("%s verdict %q, want correct", other, got.Verdict)
				}
			})
		}
	})
}

// oldAllowListArgv replaces the auto-mode flags with the retired
// --allowed-tools list. The checker must reject that shape.
func oldAllowListArgv(argv []string) []string {
	i := slices.Index(argv, "--permission-mode")
	head, tail := slices.Clone(argv[:i]), slices.Clone(argv[i+4:])
	rules := []string{"mcp__callsheet__ws_push", "mcp__callsheet__dispatch", "mcp__callsheet__task_show",
		"mcp__callsheet__ws_status", "mcp__callsheet__ws_diff", "mcp__callsheet__ws_pull"}
	for _, h := range []string{"designer", "design-reviewer", "coder", "code-reviewer"} {
		rules = append(rules, "Bash(/bin/reale2e observe --run /rt --task * --hop "+h+")")
	}
	for _, h := range []string{"designer", "design-reviewer", "coder", "code-reviewer"} {
		rules = append(rules, "Bash(/bin/reale2e wait-task --run /rt --task * --hop "+h+")")
	}
	rules = append(rules, "Bash(/bin/reale2e review-material --run /rt)")
	out := append(head, "--allowed-tools")
	out = append(out, rules...)
	return append(out, tail...)
}

// rebindLaunchHashes updates the manifest's launch digests after an edit.
func rebindLaunchHashes(t *testing.T, b string) {
	t.Helper()
	editDoc(t, b, "manifest.json", func(m *Manifest) {
		for i := range m.Launches {
			m.Launches[i].SHA256 = sha256Hex(mustRead(t, filepath.Join(b, filepath.FromSlash(m.Launches[i].Name))))
		}
	})
}

// setStreamPermission sets or drops permissionMode on phase's init records
// (phase is 0-based).
func setStreamPermission(t *testing.T, b string, phase int, mode string, drop bool) {
	t.Helper()
	editBundleStream(t, b, phase, func(lines []string) []string {
		for i, line := range lines {
			var v map[string]any
			if json.Unmarshal([]byte(line), &v) != nil || v["subtype"] != "init" {
				continue
			}
			if drop {
				delete(v, "permissionMode")
			} else {
				v["permissionMode"] = mode
			}
			raw, err := json.Marshal(v)
			if err != nil {
				t.Fatal(err)
			}
			lines[i] = string(raw)
		}
		return lines
	})
}

// appendRepeatedInit appends another turn whose init reports mode. The
// init line is the phase's own init, so a repeat of auto matches exactly.
func appendRepeatedInit(t *testing.T, b string, phase int, mode string) {
	t.Helper()
	editBundleStream(t, b, phase, func(lines []string) []string {
		var init, result string
		for _, line := range lines {
			var v map[string]any
			if json.Unmarshal([]byte(line), &v) != nil {
				continue
			}
			if v["subtype"] == "init" && init == "" {
				init = line
			}
			if v["type"] == "result" {
				result = line
			}
		}
		if init == "" || result == "" {
			t.Fatal("the phase stream has no init and result")
		}
		if mode != autoModeFlag {
			old := `"permissionMode":"` + autoModeFlag + `"`
			init = strings.Replace(init, old, `"permissionMode":"`+mode+`"`, 1)
			if init == "" || !strings.Contains(init, `"permissionMode":"`+mode+`"`) {
				t.Fatal("the init line has no permission mode")
			}
		}
		return append(lines, init, result)
	})
}

// rewriteDerivedMap stores the map the checker re-derives, so a stream
// edit is judged on its own and not on a stale copy of the map.
func rewriteDerivedMap(t *testing.T, b string) {
	t.Helper()
	loaded, err := LoadBundle(b)
	if err != nil {
		t.Fatal(err)
	}
	em, _ := deriveSession(sessionInputOf(loaded))
	if len(em.Findings) != 0 {
		t.Fatalf("derivation findings %v", findingCodes(em))
	}
	raw, err := encodeJSON(em)
	if err != nil {
		t.Fatal(err)
	}
	writeBundleFile(t, b, eventMapFile, raw)
}
