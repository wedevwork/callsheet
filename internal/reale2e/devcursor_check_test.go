package reale2e

import (
	"bufio"
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// Unit tests of design 12a-3-cursor-worker, slice 3b (UT-B1, and the
// checker half of UT-B2): the strict Cursor formats, the independent
// qualification reconstruction under every mutation, and the attempt
// checker's Cursor rows. Every mutation runs on a fresh copy of a cached,
// live-collected QUALIFIED probe or passing attempt; a capable forger's
// re-inventory follows every edit, so only a contradiction with primary
// evidence can catch it.

// obj is the nested JSON object at keys of m.
func jobj(m map[string]any, keys ...string) map[string]any {
	for _, k := range keys {
		m = m[k].(map[string]any)
	}
	return m
}

// arr is the nested JSON array at keys of m.
func jarr(m map[string]any, keys ...string) []any {
	last := keys[len(keys)-1]
	return jobj(m, keys[:len(keys)-1]...)[last].([]any)
}

// editEventLines rewrites events.jsonl through f, one generic map per line.
func editEventLines(t *testing.T, dir string, f func(evs []map[string]any) []map[string]any) {
	t.Helper()
	p := filepath.Join(dir, "events.jsonl")
	data, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	var evs []map[string]any
	sc := bufio.NewScanner(bytes.NewReader(data))
	for sc.Scan() {
		dec := json.NewDecoder(strings.NewReader(sc.Text()))
		dec.UseNumber()
		var m map[string]any
		if err := dec.Decode(&m); err != nil {
			t.Fatal(err)
		}
		evs = append(evs, m)
	}
	var out bytes.Buffer
	for _, m := range f(evs) {
		b, _ := json.Marshal(m)
		out.Write(append(b, '\n'))
	}
	if err := os.WriteFile(p, out.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
}

// writeFile replaces a bundle file's bytes.
func writeFile(t *testing.T, dir, rel string, data []byte) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, filepath.FromSlash(rel)), data, 0o600); err != nil {
		t.Fatal(err)
	}
}

// appendFile appends to a bundle file.
func appendFile(t *testing.T, dir, rel, s string) {
	t.Helper()
	writeFile(t, dir, rel, append(mustRead(t, filepath.Join(dir, filepath.FromSlash(rel))), s...))
}

// cursorMutation is one mutation of a qualified Cursor probe and the code
// its independent check must report.
type cursorMutation struct {
	edit func(t *testing.T, dir string)
	code string
}

// zeros is a well-formed but wrong SHA-256.
var zeros = strings.Repeat("0", 64)

func TestCursorQualificationMutations(t *testing.T) {
	t.Parallel()
	q := func(f func(m map[string]any)) func(t *testing.T, dir string) {
		return func(t *testing.T, dir string) { editJSON(t, dir, devQualificationFile, f) }
	}
	data := func(rel string, f func(m map[string]any)) func(t *testing.T, dir string) {
		return func(t *testing.T, dir string) { editJSON(t, dir, rel, func(m map[string]any) { f(jobj(m, "data")) }) }
	}
	file := func(rel string, f func(m map[string]any)) func(t *testing.T, dir string) {
		return func(t *testing.T, dir string) { editJSON(t, dir, rel, f) }
	}
	captured := func(logical, extra string) func(t *testing.T, dir string) {
		return func(t *testing.T, dir string) { appendFile(t, dir, capturedPath(logical), extra) }
	}
	remove := func(rel string) func(t *testing.T, dir string) {
		return func(t *testing.T, dir string) {
			if err := os.RemoveAll(filepath.Join(dir, filepath.FromSlash(rel))); err != nil {
				t.Fatal(err)
			}
		}
	}
	coder := probeCoderDir + "/"
	val := coder + "validation/"
	cases := map[string]cursorMutation{
		// Step 1: the frozen selection and the method provenance.
		"flow_effort": {file("setup/flow.json", func(m map[string]any) { jarr(m, "roles")[2].(map[string]any)["effort"] = "max" }), CodeCursorSelectionMismatch},
		"flow_default": {func(t *testing.T, dir string) {
			writeFile(t, dir, "setup/flow.json", mustRead(t, capturedFlowPath(dir)))
		}, CodeCursorSelectionMismatch},
		"roles_record":  {q(func(m map[string]any) { jarr(m, "roles")[0].(map[string]any)["model"] = "gpt-x" }), CodeCursorSelectionMismatch},
		"label_default": {q(func(m map[string]any) { m["flow_label"] = FlowDefault }), CodeCursorSelectionMismatch},
		"label_digest":  {file("setup/flow-label.json", func(m map[string]any) { m["selection_sha256"] = zeros }), "flow_label_mismatch"},
		"default_tmpl":  {func(t *testing.T, dir string) { writeFile(t, dir, capturedFlowRel(), cursorFlowBytes(t)) }, CodeCursorSelectionMismatch},
		"manuals": {func(t *testing.T, dir string) {
			editJSON(t, dir, "setup/installed-manuals.json", func(m map[string]any) { jarr(m, "data")[0].(map[string]any)["sha256"] = zeros })
		}, "installed_manuals_mismatch"},
		"feature_request": {file("setup/feature-request.json", func(m map[string]any) { m["generated_sha256"] = zeros }), "feature_request_mismatch"},
		"source_bytes":    {captured("setup/sources/"+DevFlowDir+"/coder-runbook.md", "\nchanged\n"), "source_inventory_mismatch"},
		// Step 2: platform, versions, bindings, preflights, roles and nodes.
		"goos":               {q(func(m map[string]any) { m["goos"] = "darwin" }), "platform_not_linux"},
		"gate":               {q(func(m map[string]any) { jobj(m, "gate")["ci_absent"] = false }), "gate_not_passed"},
		"no_cursor_version":  {q(func(m map[string]any) { vs := jarr(m, "versions"); m["versions"] = append(vs[:3:3], vs[4:]...) }), "versions_incomplete"},
		"version_ineligible": {q(func(m map[string]any) { jarr(m, "versions")[3].(map[string]any)["eligible"] = false }), "version_not_eligible"},
		"harness_changed":    {q(func(m map[string]any) { jobj(m, "executable")["after_sha256"] = zeros }), "executable_changed"},
		"binding_after":      {q(func(m map[string]any) { jobj(m, "cursor", "binding")["after_sha256"] = zeros }), CodeCursorExecutableMismatch},
		"binding_path":       {q(func(m map[string]any) { jobj(m, "cursor", "binding")["path"] = "/other/cursor-agent" }), CodeCursorExecutableMismatch},
		"binding_upper": {q(func(m map[string]any) {
			b := jobj(m, "cursor", "binding")
			b["before_sha256"], b["after_sha256"] = strings.ToUpper(b["before_sha256"].(string)), strings.ToUpper(b["after_sha256"].(string))
		}), CodeCursorExecutableMismatch},
		"binding_unhashed":  {q(func(m map[string]any) { jobj(m, "cursor", "binding")["after_sha256"] = "" }), CodeCursorExecutableMismatch},
		"preflight_record":  {data("preflight/03/observation.json", func(m map[string]any) { m["model"] = "other" }), "preflight_record_mismatch"},
		"preflight_missing": {remove("preflight/02/task.json"), "preflight_missing"},
		"coder_node": {file("setup/roles.json", func(m map[string]any) {
			rs := jarr(m, "roles")
			rs[2].(map[string]any)["node"] = rs[0].(map[string]any)["node"]
		}), CodeCursorNodesMismatch},
		"role_not_ready":  {file("setup/roles.json", func(m map[string]any) { jarr(m, "roles")[1].(map[string]any)["can_accept"] = false }), "role_not_ready"},
		"sidecar_stopped": {data(devCleanupFile, func(m map[string]any) { setProc(m, "sidecar-cursor", "exit_verified", false) }), CodeCursorNodesMismatch},
		"no_fourth_node": {func(t *testing.T, dir string) {
			editEventLines(t, dir, func(evs []map[string]any) []map[string]any {
				var out []map[string]any
				for _, e := range evs {
					if e["handle"] != "sidecar-cursor" {
						out = append(out, e)
					}
				}
				return renumber(out)
			})
		}, CodeCursorNodesMismatch},
		// Step 3: the coder fixture from its pinned sources.
		"input_main":    {captured(coder+"input/main.go", "// x\n"), CodeCursorFixtureMismatch},
		"expected_main": {captured(coder+"expected/main.go", "// x\n"), CodeCursorFixtureMismatch},
		"fixture_rec":   {data(coder+"fixture.json", func(m map[string]any) { jarr(m, "input")[0].(map[string]any)["sha256"] = zeros }), CodeCursorFixtureMismatch},
		"design_bytes":  {captured("setup/probe-fixture/design.md", "\nextra\n"), CodeCursorFixtureMismatch},
		"test_bytes":    {captured("setup/probe-fixture/main_test.go", "\n// extra\n"), CodeCursorFixtureMismatch},
		"gomod_form":    {captured(coder+"input/go.mod", "\nrequire x v1\n"), CodeCursorFixtureMismatch},
		// Step 4: the exact request, the push and the one coder task.
		"request_goal": {file(coder+"request.json", func(m map[string]any) { m["goal"] = m["goal"].(string) + "x" }), CodeCursorRequestMismatch},
		"request_accept": {file(coder+"request.json", func(m map[string]any) {
			m["acceptance"] = devAcceptance(&TaskPlan{Role: HopCoder, Mode: modeImplement})
		}), CodeCursorRequestMismatch},
		"request_target":   {file(coder+"request.json", func(m map[string]any) { jobj(m, "target")["value"] = "designer" }), CodeCursorRequestMismatch},
		"request_instance": {file(coder+"request.json", func(m map[string]any) { m["workspace_instance"] = strings.Repeat("1", 32) }), CodeCursorRequestMismatch},
		"logs_task":        {file(coder+"logs.json", func(m map[string]any) { m["task"] = "t_" + strings.Repeat("9", 32) }), CodeCursorTaskMismatch},
		"logs_text":        {func(t *testing.T, dir string) { appendFile(t, dir, coder+"logs.txt", "x") }, CodeCursorTaskMismatch},
		"push_commit":      {file(coder+"push-response.json", func(m map[string]any) { jobj(m, "result")["commit"] = strings.Repeat("2", 40) }), CodeCursorPushMismatch},
		"push_branch":      {file(coder+"push-request.json", func(m map[string]any) { m["branch"] = "other" }), CodeCursorPushMismatch},
		"task_id":          {q(func(m map[string]any) { jobj(m, "cursor")["task"] = "t_" + strings.Repeat("9", 32) }), CodeCursorTaskMismatch},
		"duplicate_call":   {file(devCostFile, func(m map[string]any) { m["tasks"] = append(jarr(m, "tasks"), jarr(m, "tasks")[5]) }), CodeCursorTaskMismatch},
		"effective_pair":   {file(coder+"task.json", func(m map[string]any) { jobj(m, "task", "effective")["effort"] = "low" }), CodeCursorTaskMismatch},
		// Step 5: the authoritative final and the publication.
		"final_blocked": {func(t *testing.T, dir string) {
			editJSON(t, dir, coder+"task.json", func(m map[string]any) { jobj(m, "task", "result")["final_message"] = "Blocked.\nSTATUS: BLOCKED\n" })
			writeFile(t, dir, coder+"final.txt", []byte("Blocked.\nSTATUS: BLOCKED\n"))
		}, CodeCursorTaskMismatch},
		"final_text":      {func(t *testing.T, dir string) { appendFile(t, dir, coder+"final.txt", "x") }, CodeCursorTaskMismatch},
		"verdict_forged":  {data(coder+"verdict.json", func(m map[string]any) { m["code"] = "forged" }), CodeCursorPublicationMismatch},
		"result_commit":   {q(func(m map[string]any) { jobj(m, "cursor")["result_commit"] = jobj(m, "cursor")["input_commit"] }), CodeCursorPublicationMismatch},
		"publication_rec": {data(coder+"publication.json", func(m map[string]any) { m["changed"] = []any{"main.go", "main_test.go"} }), CodeCursorPublicationMismatch},
		"result_ref":      {remove(probeCoderRepoDir + "/refs/heads/result"), CodeCursorPublicationMismatch},
		// Step 6: the independent validation of the result tree.
		"tests_stream": {func(t *testing.T, dir string) {
			lines := strings.SplitAfter(string(mustRead(t, filepath.Join(dir, filepath.FromSlash(val+"tests.jsonl")))), "\n")
			writeFile(t, dir, val+"tests.jsonl", []byte(strings.Join(lines[1:], "")))
		}, CodeCursorValidationMismatch},
		"coverage_80": {file(val+"coverage.json", func(m map[string]any) {
			c := jobj(m, "coverage")
			c["covered"], c["total"], c["percent"], c["pass"] = 4, 5, "80.0", false
		}), CodeCursorValidationMismatch},
		"final_argv":   {file(val+"final-test.json", func(m map[string]any) { m["argv"] = []any{"go", "test", "./..."} }), CodeCursorValidationMismatch},
		"build_output": {func(t *testing.T, dir string) { appendFile(t, dir, val+"build-stdout.txt", "x") }, CodeCursorValidationMismatch},
		"requirements": {file(val+"requirements.json", func(m map[string]any) { m["run_signature"] = false }), CodeCursorValidationMismatch},
		"case_failed":  {file(val+"cases.json", func(m map[string]any) { jarr(m, "cases")[1].(map[string]any)["pass"] = false }), CodeCursorValidationMismatch},
		"exec_binding": {file(val+"cases.json", func(m map[string]any) { jobj(m, "build")["start_seq"] = 1 }), CodeCursorValidationMismatch},
		"json_argv": {file(val+"requirements.json", func(m map[string]any) { jobj(m, "test_run")["argv"] = []any{"go", "test", "./..."} }),
			CodeCursorValidationMismatch},
		"phase_unverified": {data(devCleanupFile, func(m map[string]any) { setProc(m, "coordinator-phase-1", "exit_verified", false) }),
			"process_exit_unverified"},
		"plane_record": {data(devCleanupFile, func(m map[string]any) { setProc(m, "plane", "pid", 1) }), "process_record_conflict"},
		// Review round 1 counterexamples.
		// C2: an old Cursor version still marked eligible.
		"c2_version_policy": {q(func(m map[string]any) { jarr(m, "versions")[3].(map[string]any)["version"] = "2020.01.01-aaaa" }), "version_not_eligible"},
		"c2_claude_policy":  {q(func(m map[string]any) { jarr(m, "versions")[0].(map[string]any)["version"] = "2.1.290 (Claude Code)" }), "version_not_eligible"},
		// C3: the coder registered on another valid node while its task
		// views still name the Cursor node.
		"c3_coder_node_other": {file("setup/roles.json", func(m map[string]any) {
			jarr(m, "roles")[2].(map[string]any)["node"] = hexID("n_", 99)
		}), CodeCursorNodesMismatch},
		"c3_task_view_node": {file(coder+"task.json", func(m map[string]any) { jobj(m, "task", "role")["node"] = hexID("n_", 99) }), CodeCursorNodesMismatch},
		"c3_preflight_node": {file("preflight/01/task.json", func(m map[string]any) { jobj(m, "task", "role")["node"] = hexID("n_", 98) }),
			CodeCursorNodesMismatch},
		// C4: the attempt window and the coder validation's own time bounds.
		"c4_attempt_deadline": {q(func(m map[string]any) { m["attempt_deadline"] = "2000-01-01T00:00:00Z" }), "attempt_window_invalid"},
		"c4_exec_duration":    {file(val+"coverage.json", func(m map[string]any) { jobj(m, "exec")["duration_ms"] = 120000 }), CodeCursorValidationMismatch},
		"c4_exec_deadline":    {file(val+"cases.json", func(m map[string]any) { jobj(m, "build")["deadline_ms"] = 600000 }), CodeCursorValidationMismatch},
		// Review round 2, C1: overflow boundaries are compared as integers,
		// never wrapped through time.Duration.
		"r2_duration_max_int64": {file(val+"coverage.json", func(m map[string]any) { jobj(m, "exec")["duration_ms"] = json.Number("9223372036854775807") }),
			CodeCursorValidationMismatch},
		"r2_deadline_max_int64": {file(val+"cases.json", func(m map[string]any) { jobj(m, "build")["deadline_ms"] = json.Number("9223372036854775807") }),
			CodeCursorValidationMismatch},
		"r2_duration_negative": {file(val+"coverage.json", func(m map[string]any) { jobj(m, "exec")["duration_ms"] = -1 }), CodeCursorValidationMismatch},
		"r2_final_test_max_int64": {file(val+"final-test.json", func(m map[string]any) { m["duration_ms"] = json.Number("9223372036854775807") }),
			CodeCursorValidationMismatch},
		"r2_deadline_zero": {file(val+"cases.json", func(m map[string]any) { jobj(m, "build")["deadline_ms"] = 0 }), CodeCursorValidationMismatch},
		"r2_deadline_over": {file(val+"cases.json", func(m map[string]any) { jobj(m, "build")["deadline_ms"] = 60001 }), CodeCursorValidationMismatch},
		"r2_duration_over_deadline": {file(val+"requirements.json", func(m map[string]any) {
			jobj(m, "test_run")["duration_ms"], jobj(m, "test_run")["deadline_ms"] = 50000, 40000
		}), CodeCursorValidationMismatch},
		// Review round 2, C2: cleanup's own timestamps shifted away from the
		// collection and cleanup events, later and earlier.
		"r2_cleanup_shifted_later": {data(devCleanupFile, func(m map[string]any) {
			m["started_at"], m["finished_at"] = "2099-01-01T00:00:00Z", "2099-01-01T00:00:00Z"
		}), "cleanup_deadline_exceeded"},
		"r2_cleanup_shifted_earlier": {data(devCleanupFile, func(m map[string]any) {
			m["started_at"], m["finished_at"] = "2026-10-10T12:00:00Z", "2026-10-10T12:00:01Z"
		}), "cleanup_deadline_exceeded"},
		"r2_cleanup_deadline":    {data(devCleanupFile, func(m map[string]any) { m["deadline"] = "2099-01-01T00:00:00Z" }), "cleanup_deadline_exceeded"},
		"r2_cleanup_finish_late": {data(devCleanupFile, func(m map[string]any) { m["finished_at"] = "2026-10-10T12:12:00Z" }), "cleanup_deadline_exceeded"},
		"c4_event_order": {func(t *testing.T, dir string) {
			editEventLines(t, dir, func(evs []map[string]any) []map[string]any {
				evs[2]["time"] = "2026-10-10T11:00:00Z"
				return evs
			})
		}, "time_not_monotonic"},
		"c4_admission_late": {func(t *testing.T, dir string) {
			editEventLines(t, dir, func(evs []map[string]any) []map[string]any {
				evs = append(evs, map[string]any{"type": EvTaskAdmitted, "time": "2099-01-01T00:00:00Z", "hop": "x", "task": "t_x"})
				return renumber(evs)
			})
		}, "attempt_deadline_exceeded"},
		// C5: the final Cursor stop renamed, and a stop method changed.
		"c5_final_stop_renamed": {func(t *testing.T, dir string) {
			editEventLines(t, dir, func(evs []map[string]any) []map[string]any {
				for i := len(evs) - 1; i >= 0; i-- {
					if evs[i]["type"] == EvProcessStopped && evs[i]["handle"] == "sidecar-cursor" {
						evs[i]["handle"] = "unrelated"
						break
					}
				}
				return evs
			})
		}, CodeCursorNodesMismatch},
		"c5_stop_method": {data(devCleanupFile, func(m map[string]any) { setProc(m, "sidecar-cursor", "stopped_by", "SIGKILL") }), CodeCursorNodesMismatch},
		// Step 7: scope, stages, accounting, samples and cleanup.
		"scope_replay":   {q(func(m map[string]any) { m["scope"] = probeScopeLight }), "scope_invalid"},
		"stage_coverage": {q(func(m map[string]any) { m["stage_coverage"] = []any{"models", "resume_wake"} }), "scope_invalid"},
		"call_bound":     {q(func(m map[string]any) { m["worker_calls"] = 13 }), "cost_bound_exceeded"},
		"stage_failed":   {q(func(m map[string]any) { jarr(m, "stages")[1].(map[string]any)["result"] = stageFail }), "not_qualified"},
		"coder_unadmitted": {func(t *testing.T, dir string) {
			editEventLines(t, dir, func(evs []map[string]any) []map[string]any {
				for _, e := range evs {
					if e["type"] == EvTaskAdmitted && e["hop"] == probeCoderLabel {
						e["hop"] = "probe-coder-02"
					}
				}
				return evs
			})
		}, CodeCursorCoderMissing},
		"coder_after_phase": {func(t *testing.T, dir string) {
			editEventLines(t, dir, func(evs []map[string]any) []map[string]any {
				for _, e := range evs {
					if e["type"] == EvValidationFinished && e["hop"] == probeCoderLabel {
						e["hop"] = "x"
					}
				}
				evs = append(evs, map[string]any{"type": EvValidationFinished, "time": evs[len(evs)-1]["time"], "hop": probeCoderLabel, "code": "late"})
				return renumber(evs)
			})
		}, CodeCursorTaskMismatch},
		"activity_missing": {remove(cursorActivityFiles[1]), CodeCursorActivityMissing},
		"activity_sum": {file(cursorActivityFiles[0], func(m map[string]any) {
			c := jobj(jarr(m, "roots")[0].(map[string]any), "observed_counts", "children")
			c["file"] = json.Number("99")
		}), CodeCursorActivityMismatch},
		"cleanup":     {data(devCleanupFile, func(m map[string]any) { m["complete"] = false }), "cleanup_incomplete"},
		"run_id":      {file(coder+"verdict.json", func(m map[string]any) { m["run_id"] = "20990101T000000Z-00000000" }), "run_id_mismatch"},
		"environment": {file("environment/diff.json", func(m map[string]any) { m["complete"] = false }), CodeEnvUnobserved},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			dir := cursorQualifiedBundle(t, probeScopeBoth)
			c.edit(t, dir)
			requalify(t, dir)
			code, rep, errOut := cursorCheck(t, dir)
			if code != 1 || rep.Result != ResultBlocked || !slices.Contains(rep.Codes, c.code) {
				t.Fatalf("exit %d %s codes %v (want %s) %s", code, rep.Result, rep.Codes, c.code, errOut)
			}
		})
	}
}

// capturedFlowRel is the captured default template flow's storage path.
func capturedFlowRel() string { return capturedPath("setup/sources/" + devSourceRel(FlowFile)) }

// capturedFlowPath is its absolute path in dir.
func capturedFlowPath(dir string) string {
	return filepath.Join(dir, filepath.FromSlash(capturedFlowRel()))
}

// setProc sets field on cleanup's process records of handle.
func setProc(m map[string]any, handle, field string, v any) {
	for _, p := range m["processes"].([]any) {
		if p.(map[string]any)["name"] == handle {
			p.(map[string]any)[field] = v
		}
	}
}

// renumber gives events consecutive sequence numbers from 1.
func renumber(evs []map[string]any) []map[string]any {
	for i, e := range evs {
		e["seq"] = i + 1
	}
	return evs
}

func TestCursorQualificationControl(t *testing.T) {
	t.Parallel()
	for _, scope := range []string{probeScopeFull, probeScopeBoth} {
		t.Run(scope, func(t *testing.T) {
			t.Parallel()
			dir := cursorQualifiedBundle(t, scope)
			code, rep, errOut := cursorCheck(t, dir)
			if code != 0 || rep.Schema != devCursorQualificationReportSchema || rep.Result != ResultQualified || len(rep.Codes) != 0 || rep.Scope != scope ||
				!ValidRunID(rep.RunID) || rep.Error != "" || rep.Activity.Before != cursorActivityFiles[0] || rep.Activity.After != cursorActivityFiles[1] ||
				rep.Activity.Changes == nil {
				t.Fatalf("control %d %+v %s", code, rep, errOut)
			}
			// The report is JSON with codes never null.
			var out, errb bytes.Buffer
			CheckEvidence(dir, &out, &errb)
			if !strings.Contains(out.String(), `"codes": []`) || strings.Contains(out.String(), "PASS") {
				t.Fatalf("report %s", out.String())
			}
			b, err := LoadDevQualification(dir)
			if err != nil || !b.cursor || len(CheckDevQualification(b)) != 0 {
				t.Fatalf("library %v", err)
			}
		})
	}
}

func TestCursorQualificationPartial(t *testing.T) {
	t.Parallel()
	// Blocked-run partial artifacts never panic or pass: every coder,
	// validation and sample record removed in turn.
	var rels []string
	for _, n := range []string{"request.json", "fixture.json", "push-request.json", "push-response.json", "task.json", "final.txt", "logs.txt", "logs.json",
		"publication.json", "verdict.json"} {
		rels = append(rels, probeCoderDir+"/"+n)
	}
	for _, n := range probeCoderValidationFiles {
		rels = append(rels, probeCoderDir+"/validation/"+n)
	}
	for _, n := range probeCoderFixtureFiles {
		rels = append(rels, capturedPath(probeCoderDir+"/input/"+n))
	}
	rels = append(rels, capturedPath(probeCoderDir+"/expected/main.go"), cursorActivityFiles[0], cursorActivityFiles[1], probeCoderRepoDir,
		"setup/roles.json", devCleanupFile, devCostFile)
	dir := cursorQualifiedBundle(t, probeScopeFull)
	for _, rel := range rels {
		t.Run(strings.ReplaceAll(rel, "/", "_"), func(t *testing.T) {
			d := filepath.Join(t.TempDir(), "b")
			if err := copyTree(dir, d); err != nil {
				t.Fatal(err)
			}
			if err := os.RemoveAll(filepath.Join(d, filepath.FromSlash(rel))); err != nil {
				t.Fatal(err)
			}
			requalify(t, d)
			code, rep, errOut := cursorCheck(t, d)
			if code != 1 || rep.Result != ResultBlocked || len(rep.Codes) == 0 {
				t.Fatalf("exit %d %+v %s", code, rep, errOut)
			}
		})
	}
}

func TestCursorQualificationSchema(t *testing.T) {
	t.Parallel()
	// Strict formats: presence, nulls, unknown fields, duplicate keys,
	// layout, path safety and budgets are schema errors (exit 2) before any
	// criterion; historical receipts keep their precedence.
	cases := map[string]func(t *testing.T, dir string){
		"cursor_null": func(t *testing.T, dir string) {
			editJSON(t, dir, devQualificationFile, func(m map[string]any) { m["cursor"] = nil })
		},
		"cursor_missing": func(t *testing.T, dir string) {
			editJSON(t, dir, devQualificationFile, func(m map[string]any) { delete(m, "cursor") })
		},
		"default_with_cursor": func(t *testing.T, dir string) {
			editJSON(t, dir, devQualificationFile, func(m map[string]any) { m["schema"] = devQualificationSchema })
		},
		"unknown_field": func(t *testing.T, dir string) {
			editJSON(t, dir, devQualificationFile, func(m map[string]any) { jobj(m, "cursor")["extra"] = 1 })
		},
		"duplicate_key": func(t *testing.T, dir string) {
			p := filepath.Join(dir, devQualificationFile)
			b := mustRead(t, p)
			writeFile(t, dir, devQualificationFile, bytes.Replace(b, []byte(`"cursor": {`), []byte(`"cursor": {"task": "x", `), 1))
		},
		"request_unknown": func(t *testing.T, dir string) {
			editJSON(t, dir, probeCoderDir+"/request.json", func(m map[string]any) { m["namespace"] = "probe-coder" })
		},
		"request_override": func(t *testing.T, dir string) {
			editJSON(t, dir, probeCoderDir+"/request.json", func(m map[string]any) { m["override"] = map[string]any{"model": "other"} })
		},
		"scope_relabel": func(t *testing.T, dir string) {
			// A full record relabelled both lacks the both layout's records.
			editJSON(t, dir, devQualificationFile, func(m map[string]any) { m["scope"] = probeScopeBoth })
			writeFile(t, dir, "probe/coder/01/extra.json", []byte("{}"))
		},
		"verdict_schema": func(t *testing.T, dir string) {
			editJSON(t, dir, probeCoderDir+"/verdict.json", func(m map[string]any) { m["schema"] = "callsheet-dev-probe-coder-verdict/v9" })
		},
		"activity_schema": func(t *testing.T, dir string) {
			editJSON(t, dir, cursorActivityFiles[0], func(m map[string]any) { m["schema"] = "callsheet-cursor-activity-aggregate/v2" })
		},
		"activity_unknown": func(t *testing.T, dir string) {
			editJSON(t, dir, cursorActivityFiles[1], func(m map[string]any) { m["files"] = []any{} })
		},
		"activity_budget": func(t *testing.T, dir string) {
			writeFile(t, dir, cursorActivityFiles[0], append(mustRead(t, filepath.Join(dir, filepath.FromSlash(cursorActivityFiles[0]))),
				bytes.Repeat([]byte(" "), activityMaxOutput)...))
		},
		"stray_file": func(t *testing.T, dir string) { writeFile(t, dir, probeCoderDir+"/notes.txt", []byte("x")) },
		"stray_go":   func(t *testing.T, dir string) { writeFile(t, dir, probeCoderDir+"/main.go", []byte("package x")) },
		"stray_input": func(t *testing.T, dir string) {
			writeFile(t, dir, capturedPath(probeCoderDir+"/input/x.go"), []byte("x"))
		},
		"symlink": func(t *testing.T, dir string) {
			p := filepath.Join(dir, filepath.FromSlash(probeCoderDir+"/final.txt"))
			os.Remove(p)
			os.Symlink(filepath.Join(dir, "events.jsonl"), p)
		},
		"extra_ref": func(t *testing.T, dir string) {
			ref := filepath.Join(dir, filepath.FromSlash(probeCoderRepoDir), "refs", "heads")
			writeFile(t, dir, probeCoderRepoDir+"/refs/heads/other", mustRead(t, filepath.Join(ref, "input")))
		},
		"not_utf8": func(t *testing.T, dir string) { writeFile(t, dir, probeCoderDir+"/logs.txt", []byte{0xff}) },
		"bad_scope": func(t *testing.T, dir string) {
			editJSON(t, dir, devQualificationFile, func(m map[string]any) { m["scope"] = "half" })
		},
		"diagnostic":  func(t *testing.T, dir string) { historicalReceipt(t, dir) },
		"observation": func(t *testing.T, dir string) { writeFile(t, dir, cursorObservationText, []byte("x")) },
	}
	for name, edit := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			dir := cursorQualifiedBundle(t, probeScopeFull)
			edit(t, dir)
			var out, errb bytes.Buffer
			code := CheckEvidence(dir, &out, &errb)
			if code != 2 || strings.Contains(out.String(), ResultQualified) {
				t.Fatalf("exit %d %s %s", code, out.String(), errb.String())
			}
			if name == "diagnostic" && !strings.Contains(errb.String(), CodeDiagnosticNotQualification) {
				t.Fatalf("diagnostic precedence lost: %s", errb.String())
			}
			if _, err := LoadDevQualification(dir); err == nil && name != "observation" {
				t.Fatalf("the library loaded %s", name)
			}
		})
	}
}

func TestCursorRelabeledFormats(t *testing.T) {
	t.Parallel()
	// A default record relabeled Cursor, or a Cursor record relabeled
	// default, never qualifies: the selection and the format must agree.
	t.Run("default_probe_claims_cursor", func(t *testing.T) {
		t.Parallel()
		dir := devProbeBundle(t)
		editJSON(t, dir, "setup/flow.json", func(m map[string]any) {
			jarr(m, "roles")[2].(map[string]any)["adapter"], jarr(m, "roles")[2].(map[string]any)["model"] = "cursor", "grok-4.7"
		})
		requalify(t, dir)
		if codes := qualificationCodes(t, dir); !slices.Contains(codes, CodeCursorSelectionMismatch) {
			t.Fatalf("codes %v", codes)
		}
	})
	t.Run("cursor_record_default_selection", func(t *testing.T) {
		t.Parallel()
		dir := cursorQualifiedBundle(t, probeScopeFull)
		def := DefaultDevFullFlow()
		b, _ := encodeJSON(def)
		writeFile(t, dir, "setup/flow.json", b)
		editJSON(t, dir, devQualificationFile, func(m map[string]any) {
			var roles []any
			rb, _ := json.Marshal(def.Roles)
			json.Unmarshal(rb, &roles)
			m["roles"], m["flow_label"] = roles, FlowDefault
		})
		requalify(t, dir)
		if code, rep, _ := cursorCheck(t, dir); code != 1 || !slices.Contains(rep.Codes, CodeCursorSelectionMismatch) {
			t.Fatalf("%d %v", code, rep.Codes)
		}
	})
	t.Run("pure_report_requires_format", func(t *testing.T) {
		t.Parallel()
		// The pure result of a default-format bundle is never a Cursor
		// qualification.
		b, err := LoadDevQualification(devProbeBundle(t))
		if err != nil {
			t.Fatal(err)
		}
		if rep := cursorQualificationReport(b); rep.Result != ResultBlocked || !slices.Contains(rep.Codes, CodeCursorSelectionMismatch) {
			t.Fatalf("%+v", rep)
		}
		if rep := cursorQualificationReportAt(filepath.Join(t.TempDir(), "missing")); rep.Result != ResultBlocked || rep.Error != "evidence_schema_error" {
			t.Fatalf("%+v", rep)
		}
	})
}

// ---- Attempts (UT-B2) ----

// auditRow is audit.json's mechanical row id ("" ID when absent).
func cursorAuditRow(t *testing.T, dir, id string) DevAuditRow {
	t.Helper()
	for _, r := range bundleDoc[DevAudit](t, dir, "audit.json").Rows {
		if r.ID == id {
			return r
		}
	}
	return DevAuditRow{}
}

func TestCursorAttemptChecks(t *testing.T) {
	t.Parallel()
	for _, scenario := range []string{scenarioFull, scenarioLight} {
		t.Run("control/"+scenario, func(t *testing.T) {
			t.Parallel()
			dir := cursorAttemptBundle(t, scenario)
			code, rep, errOut := devCheck(t, dir)
			m := bundleDoc[DevManifest](t, dir, devManifestFile)
			if code != 0 || rep.Schema != devCursorReportSchema || rep.Activity == nil || rep.FlowLabel != FlowOverride ||
				!strings.Contains(rep.Claim, "the owner's default pairs are not demonstrated") && !strings.Contains(rep.Claim, "not demonstrated") ||
				m.Schema != DevCursorBundleSchema || m.Cursor == nil || m.Scenario != scenario || len(m.Versions) != 6 {
				t.Fatalf("control %d %+v %s", code, devCodes(rep), errOut)
			}
			if a := bundleDoc[DevAudit](t, dir, "audit.json"); a.Schema != devCursorAuditSchema {
				t.Fatalf("audit schema %s", a.Schema)
			}
			row := cursorAuditRow(t, dir, "environment/cursor-activity")
			if row.Criterion != "owner_environment" || row.Verdict != VerdictCorrect || len(row.Sources) != 2 || !strings.Contains(row.Observation, "flags") {
				t.Fatalf("activity row %+v", row)
			}
			text := string(mustRead(t, filepath.Join(dir, "report.txt")))
			for _, s := range []string{"cursor activity (report-only shallow samples of all processes", "not attribution to Cursor", "no confinement claim",
				"before cache $HOME/.cache: status", "after tmp /tmp: status", "not covered: deep or transient writes"} {
				if !strings.Contains(text, s) {
					t.Fatalf("report.txt lacks %q:\n%s", s, text)
				}
			}
		})
	}
	// A capable forger recomputes the inventory, report and audit after
	// each mutation: only the primary contradiction can catch it.
	man := func(f func(m map[string]any)) func(t *testing.T, dir string) {
		return func(t *testing.T, dir string) { editJSON(t, dir, devManifestFile, f) }
	}
	for name, c := range map[string]struct {
		scenario   string
		edit       func(t *testing.T, dir string)
		crit, code string
	}{
		"versions": {scenarioFull, man(func(m map[string]any) { vs := jarr(m, "versions"); m["versions"] = append(vs[:3:3], vs[4:]...) }), "manifest",
			"versions_incomplete"},
		"binding_after": {scenarioFull, man(func(m map[string]any) { jobj(m, "cursor")["after_sha256"] = zeros }), "manifest", CodeCursorExecutableMismatch},
		"binding_path":  {scenarioLight, man(func(m map[string]any) { jobj(m, "cursor")["path"] = "/x/cursor-agent" }), "manifest", CodeCursorExecutableMismatch},
		"binding_empty": {scenarioFull, man(func(m map[string]any) { jobj(m, "cursor")["before_sha256"] = "" }), "manifest", CodeCursorExecutableMismatch},
		"flow_effort": {scenarioFull, func(t *testing.T, dir string) {
			editJSON(t, dir, "setup/flow.json", func(m map[string]any) { jarr(m, "roles")[2].(map[string]any)["effort"] = "max" })
		}, "provenance", CodeCursorSelectionMismatch},
		"coder_node": {scenarioLight, func(t *testing.T, dir string) {
			editJSON(t, dir, "setup/roles.json", func(m map[string]any) {
				rs := jarr(m, "roles")
				rs[2].(map[string]any)["node"] = rs[0].(map[string]any)["node"]
			})
		}, "roles", CodeCursorNodesMismatch},
		"sidecar_unverified": {scenarioFull, func(t *testing.T, dir string) {
			editJSON(t, dir, devCleanupFile, func(m map[string]any) { setProc(jobj(m, "data"), "sidecar-grok", "exit_verified", false) })
		}, "roles", CodeCursorNodesMismatch},
		"activity_missing": {scenarioFull, func(t *testing.T, dir string) {
			os.Remove(filepath.Join(dir, filepath.FromSlash(cursorActivityFiles[1])))
		},
			"owner_environment", CodeCursorActivityMissing},
		"activity_sum": {scenarioLight, func(t *testing.T, dir string) {
			editJSON(t, dir, cursorActivityFiles[0], func(m map[string]any) {
				jobj(jarr(m, "roots")[0].(map[string]any), "observed_counts", "children")["file"] = json.Number("77")
			})
		}, "owner_environment", CodeCursorActivityMismatch},
		// Review round 2, C2, on attempts: cleanup's timestamps shifted away
		// from the collection and cleanup events.
		"r2_cleanup_shifted_later": {scenarioFull, func(t *testing.T, dir string) {
			editJSON(t, dir, devCleanupFile, func(m map[string]any) {
				d := jobj(m, "data")
				d["started_at"], d["finished_at"] = "2099-01-01T00:00:00Z", "2099-01-01T00:00:00Z"
			})
		}, "cleanup", "cleanup_deadline_exceeded"},
		"r2_cleanup_shifted_earlier": {scenarioLight, func(t *testing.T, dir string) {
			editJSON(t, dir, devCleanupFile, func(m map[string]any) {
				d := jobj(m, "data")
				d["started_at"], d["finished_at"] = "2026-10-10T12:00:00Z", "2026-10-10T12:00:01Z"
			})
		}, "cleanup", "cleanup_deadline_exceeded"},
		"r2_final_test_max_int64": {scenarioFull, func(t *testing.T, dir string) {
			editJSON(t, dir, "validation/final-test.json", func(m map[string]any) { m["duration_ms"] = json.Number("9223372036854775807") })
		}, "final_tests", "test_deadline_exceeded"},
		// Review round 1 counterexamples on attempts (C2, C3, C5).
		"c2_version_policy": {scenarioLight, man(func(m map[string]any) { jarr(m, "versions")[3].(map[string]any)["version"] = "2020.01.01-aaaa" }),
			"manifest", "version_not_eligible"},
		"c3_coder_node_other": {scenarioFull, func(t *testing.T, dir string) {
			editJSON(t, dir, "setup/roles.json", func(m map[string]any) { jarr(m, "roles")[2].(map[string]any)["node"] = hexID("n_", 99) })
		}, "roles", CodeCursorNodesMismatch},
		"c5_final_stop_renamed": {scenarioFull, func(t *testing.T, dir string) {
			editEventLines(t, dir, func(evs []map[string]any) []map[string]any {
				for i := len(evs) - 1; i >= 0; i-- {
					if evs[i]["type"] == EvProcessStopped && evs[i]["handle"] == "sidecar-cursor" {
						evs[i]["handle"] = "unrelated"
						break
					}
				}
				return evs
			})
		}, "roles", CodeCursorNodesMismatch},
		"activity_run": {scenarioFull, func(t *testing.T, dir string) {
			editJSON(t, dir, cursorActivityFiles[1], func(m map[string]any) { m["run_id"] = "20990101T000000Z-00000000" })
		}, "manifest", "run_id_mismatch"},
	} {
		t.Run("mutation/"+name, func(t *testing.T) {
			t.Parallel()
			dir := cursorAttemptBundle(t, c.scenario)
			c.edit(t, dir)
			reforge(t, dir)
			code, rep, errOut := devCheck(t, dir)
			if code != 1 || rep.Result != ResultFail || !slices.Contains(devCodes(rep)[c.crit], c.code) {
				t.Fatalf("exit %d %v (want %s/%s) %s", code, devCodes(rep), c.crit, c.code, errOut)
			}
		})
	}
	// Valid limited, foreign or delta samples stay mechanically eligible:
	// the activity is reported, never a gate.
	t.Run("delta_is_report_only", func(t *testing.T) {
		t.Parallel()
		dir := cursorAttemptBundle(t, scenarioFull)
		p := filepath.Join(dir, filepath.FromSlash(cursorActivityFiles[1]))
		var a ActivityAggregate
		if err := decodeStrict(mustRead(t, p), &a); err != nil {
			t.Fatal(err)
		}
		r := &a.Roots[1]
		d, mt := zeros, int64(7)
		r.Rows = append(r.Rows, ActivityRow{Name: "zz-new", Type: actDir, Status: actForeign, MetadataSHA256: &d},
			ActivityRow{Name: "zz-own", Type: actDir, Status: actOK, MetadataSHA256: &d, ObservedCounts: ActivityCounts{File: 1}, ObservedFileBytes: 3,
				NewestMtimeNS: &mt, Flags: ActivityFlags{EntryLimited: true}})
		r.ObservedCounts.Direct.Directory += 2
		r.ObservedCounts.Children.File++
		r.ObservedFileBytes.Children += 3
		r.Status, r.Exhaustive, r.TopLevelExhaustive, r.Flags.EntryLimited, r.Flags.ForeignEntries = actForeign, false, false, true, 1
		b, _ := json.Marshal(a)
		writeFile(t, dir, cursorActivityFiles[1], append(b, '\n'))
		reforge(t, dir)
		code, rep, errOut := devCheck(t, dir)
		if code != 0 || rep.Result != ResultPass || len(rep.Activity.Changes) == 0 {
			t.Fatalf("a valid delta failed: %d %v %s", code, devCodes(rep), errOut)
		}
		marks := map[string]string{}
		for _, ch := range rep.Activity.Changes {
			marks[ch.Name] = ch.Mark
		}
		// The before tmp root was enumerated exhaustively: 3a's compare rule
		// marks the unmatched rows new (an observation, never a proven
		// creation).
		if marks["zz-new"] != markNew || marks["zz-own"] != markNew {
			t.Fatalf("marks %v", marks)
		}
		// W1 (review round 1): a retained row's own quality flags (here an
		// entry-limited child sample under a root whose flags would not show
		// it) are disclosed in the report and audit text.
		row := "after tmp /tmp row zz-own: type directory, status ok; sampled children file=1 directory=0 symlink=0 other=0 unknown=0 bytes 3; " +
			"children exhaustive false; flags entry_limited"
		if text := string(mustRead(t, filepath.Join(dir, "report.txt"))); !strings.Contains(text, row) {
			t.Fatalf("report.txt lacks the row disclosure %q:\n%s", row, text)
		}
		if a := cursorAuditRow(t, dir, "environment/cursor-activity"); !strings.Contains(a.Observation, row) {
			t.Fatalf("the audit row lacks the row disclosure: %s", a.Observation)
		}
		if md := string(mustRead(t, filepath.Join(dir, "audit.md"))); !strings.Contains(md, row) {
			t.Fatal("audit.md lacks the row disclosure")
		}
	})
	t.Run("schema_matrix", func(t *testing.T) {
		t.Parallel()
		for name, c := range map[string]struct {
			cursor bool
			edit   func(t *testing.T, dir string)
			code   int
			want   string
		}{
			"default_with_field": {false, man(func(m map[string]any) {
				m["cursor"] = map[string]any{"path": "x", "before_sha256": "", "after_sha256": ""}
			}), 2, ""},
			"default_with_null": {false, man(func(m map[string]any) { m["cursor"] = nil }), 2, ""},
			"cursor_no_field":   {true, man(func(m map[string]any) { delete(m, "cursor") }), 2, ""},
			"cursor_null_field": {true, man(func(m map[string]any) { m["cursor"] = nil }), 2, ""},
			"cursor_as_default": {true, man(func(m map[string]any) { m["schema"] = DevBundleSchema }), 2, ""},
			"cursor_stripped": {true, man(func(m map[string]any) {
				m["schema"] = DevBundleSchema
				delete(m, "cursor")
			}), 2, ""},
			"activity_unknown": {true, func(t *testing.T, dir string) {
				editJSON(t, dir, cursorActivityFiles[0], func(m map[string]any) { m["paths"] = []any{} })
			}, 2, ""},
			"default_as_cursor": {false, man(func(m map[string]any) {
				m["schema"] = DevCursorBundleSchema
				m["cursor"] = map[string]any{"path": "x", "before_sha256": zeros, "after_sha256": zeros}
			}), 1, "versions_incomplete"},
			"default_claims_cursor": {false, func(t *testing.T, dir string) {
				editJSON(t, dir, "setup/flow.json", func(m map[string]any) {
					r := jarr(m, "roles")[2].(map[string]any)
					r["adapter"], r["model"] = "cursor", "grok-4.7"
				})
			}, 1, CodeCursorSelectionMismatch},
		} {
			dir := devPassingBundle(t)
			if c.cursor {
				dir = cursorAttemptBundle(t, scenarioFull)
			}
			c.edit(t, dir)
			if c.code == 1 {
				reforge(t, dir)
			}
			var out, errb bytes.Buffer
			code := CheckEvidence(dir, &out, &errb)
			if code != c.code || c.want != "" && !strings.Contains(out.String(), c.want) {
				t.Errorf("%s: exit %d %s", name, code, errb.String())
			}
		}
		// A default report never carries the Cursor activity field.
		if strings.Contains(string(mustRead(t, filepath.Join(devPassingBundle(t), "report.json"))), `"activity"`) {
			t.Error("a default report carries activity")
		}
	})
}

func TestCursorProtectedInvariants(t *testing.T) {
	t.Parallel()
	// Each protected input mutated while the activity samples are truncated
	// keeps its original fatal code; a tool-maintained Cursor file stays
	// report-only.
	for _, name := range []string{"strictPaths_still_fail", "protect_root_still_fails", "owner_repo_still_fails", "claude_protection_still_fails",
		"protected_repo_under_cache_still_fails", "tool_maintained_report_only"} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			r := newCursorFullRig(t, happy())
			var repo string
			switch name {
			case "protect_root_still_fails", "owner_repo_still_fails":
				root := t.TempDir()
				repo = filepath.Join(root, "proj")
				newRepo(t, repo)
				r.f.git, r.f.roots = realGit(t), []string{root}
			case "protected_repo_under_cache_still_fails":
				cache := filepath.Join(r.f.home, ".cache")
				repo = filepath.Join(cache, "proj")
				newRepo(t, repo)
				r.f.git, r.f.roots = realGit(t), []string{cache}
			}
			prev := r.devHook
			r.devHook = func(d *devSupervisor) {
				prev(d)
				// Truncated samples: the rows do not fit the output bound.
				d.cursor.output = activitySkeleton + 1
			}
			done := false
			r.onTick = func(r *devRig) {
				if done {
					return
				}
				done = true
				switch name {
				case "strictPaths_still_fail":
					write(t, filepath.Join(r.f.home, ".cursor", "mcp.json"), "{}")
				case "protect_root_still_fails", "protected_repo_under_cache_still_fails":
					write(t, filepath.Join(repo, "a.txt"), "changed\n")
				case "owner_repo_still_fails":
					write(t, filepath.Join(repo, ".git", "HEAD"), "ref: refs/heads/elsewhere\n")
				case "claude_protection_still_fails":
					write(t, filepath.Join(r.f.home, ".claude", "skills", "a", "SKILL.md"), "changed skill\n")
				case "tool_maintained_report_only":
					write(t, filepath.Join(r.f.home, ".cursor", "cli-config.json"), `{"trust":true}`+"\n")
				}
			}
			if name == "tool_maintained_report_only" {
				r.expect(0, "")
				return
			}
			r.expect(1, "")
			rep := r.report()
			if !slices.Contains(devCodes(rep)["owner_environment"], CodeEnvModified) {
				t.Fatalf("codes %v", devCodes(rep))
			}
			var a ActivityAggregate
			if err := decodeStrict(r.evidence(cursorActivityFiles[1]), &a); err != nil || len(a.Roots) != 3 || !a.Roots[0].Flags.OutputLimited {
				t.Fatalf("the samples were not truncated: %v %+v", err, a.Roots)
			}
			if row := cursorAuditRow(t, r.f.bundle(), "environment/cursor-activity"); row.Verdict != VerdictCorrect {
				t.Fatalf("a truncated sample failed its own row: %+v", row)
			}
		})
	}
}

func TestCursorCheckNoWriteNoExec(t *testing.T) {
	// Not parallel: PATH holds an execution trap for the whole check.
	dirs := []string{cursorQualifiedBundle(t, probeScopeFull), cursorAttemptBundle(t, scenarioLight)}
	trap := t.TempDir()
	marker := filepath.Join(t.TempDir(), "executed")
	for _, name := range []string{"git", "go", "sh", "cursor-agent", "claude", "codex", "grok", "callsheet"} {
		if err := os.WriteFile(filepath.Join(trap, name), []byte("#!/bin/sh\necho \"$0\" >> "+marker+"\n"), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("PATH", trap)
	for _, dir := range dirs {
		before := treeFingerprint(t, dir)
		var out, errb bytes.Buffer
		if code := CheckEvidence(dir, &out, &errb); code != 0 {
			t.Fatalf("%s: exit %d %s", dir, code, errb.String())
		}
		if after := treeFingerprint(t, dir); after != before {
			t.Fatalf("the check changed the evidence of %s", dir)
		}
	}
	if _, err := os.Stat(marker); err == nil {
		t.Fatalf("the check executed %s", mustRead(t, marker))
	}
}

// treeFingerprint digests every entry's path, mode, size, modification
// time and bytes below dir.
func treeFingerprint(t testing.TB, dir string) string {
	t.Helper()
	var b strings.Builder
	err := filepath.WalkDir(dir, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		st, err := os.Lstat(p)
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(dir, p)
		b.WriteString(rel + " " + st.Mode().String() + " " + st.ModTime().String())
		if st.Mode().IsRegular() {
			b.WriteString(" " + sha256Hex(mustRead(t, p)))
		}
		b.WriteString("\n")
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return sha256Hex([]byte(b.String()))
}
