package mcpqual

import (
	"bytes"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"testing/fstest"
)

// Design decoder-enrollment B2, UT-18 (FP-18): the export over tiny
// fabricated captures in the three real formats, with a canary inside
// every permitted metadata shape, under an injected test policy (the
// production policy accepts only the three pinned bundles): deterministic
// two-run equality, byte spans, offsets and the protected fingerprint,
// escaped results, unchanged probe and other payloads, allowlist and
// identity refusal, unsafe output and input, write failure, receipt schema
// and corruption, and the B2 attestation rule with pending and
// placeholder candidates refused.

// exportCanary sits in every permitted metadata value; it never survives.
const exportCanary = "CANARY-meta-7f3a"

// exportKept is an unknown metadata member the policy copies unchanged:
// the coordinator's review, not the exporter, decides about it.
const exportKept = "KEEP-unknown-meta"

// fakeExportTranscript is a fabricated real-format setup transcript (case
// <client>-capture-setup, nonce noncecap1) with canaries in the allowed
// metadata.
func fakeExportTranscript(client string) string {
	cv := exportCanary
	res := `{\"case_id\":\"` + client + `-capture-setup\",\"nonce\":\"noncecap1\"}`
	switch client {
	case "codex":
		return strings.Join([]string{
			`{"type":"thread.started","thread_id":"` + cv + `"}`,
			`{"type":"item.completed","item":{"id":"item_0","type":"error","message":"hook ` + cv + `"}}`,
			`{"type":"turn.started"}`,
			`{"type":"item.completed","item":{"id":"item_2","type":"agent_message","text":"calling ` + cv + `"}}`,
			`{"type":"item.started","item":{"id":"item_3","type":"mcp_tool_call","server":"probe","tool":"slow","arguments":{"case_id":"codex-capture-setup"},"result":null,"error":null,"status":"in_progress"}}`,
			`{"type":"item.completed","item":{"id":"item_3","type":"mcp_tool_call","server":"probe","tool":"slow","arguments":{"case_id":"codex-capture-setup"},"result":{"content":[{"type":"text","text":"` + res + `"}],"structured_content":null},"error":null,"status":"completed"}}`,
			`{"type":"item.completed","item":{"id":"item_4","type":"agent_message","text":"` + res + ` ` + cv + `"}}`,
			`{"type":"turn.completed","usage":{"input_tokens":3},"note":"` + exportKept + `"}`,
		}, "\n") + "\n"
	case "grok":
		return strings.Join([]string{
			`{"type":"available_commands","tools":["` + cv + `"],"commands":["` + cv + `"]}`,
			`{"type":"thought","data":"` + cv + `"}`,
			`{"type":"text","data":"` + cv + `"}`,
			`{"type":"usage","usage":{"input_tokens":2},"signature":"` + cv + `"}`,
			`{"type":"tool_call","toolCallId":"call-1","title":"use_tool","kind":"use_tool","status":"pending","toolName":"use_tool","rawInput":{"tool_name":"probe__slow","tool_input":{"case_id":"grok-capture-setup"}},"content":[],"locations":[]}`,
			`{"type":"tool_call_update","toolCallId":"call-1","status":null,"content":[],"rawOutput":null,"locations":[]}`,
			`{"type":"tool_call_update","toolCallId":"call-1","status":"completed","content":[],"rawOutput":{"type":"MCP","tool_name":"slow","server_name":"probe","output":{"OkayOutput":"` + res + `"}},"locations":[]}`,
			`{"type":"end","stopReason":"end_turn","sessionId":"` + cv + `","requestId":"` + cv + `","usage":{"input_tokens":4},"total_cost_usd":0.0316512,` +
				`"total_cost_usd_ticks":316512,"modelUsage":{"grok-4.7-build":{"modelCalls":2,"costUSD":0.0316512}}}`,
		}, "\n") + "\n"
	case "cursor":
		// Design decoder-enrollment B3 (FP-23): every recognized record with
		// a canary in each replaced value (integer timestamps included) and
		// the opaque call ID's JSON newline escape in all three copies.
		return strings.Join(cursorExportRecords(cv, res), "\n") + "\n"
	}
	meta := `"session_id":"` + cv + `","uuid":"` + cv + `","timestamp":"` + cv + `","request_id":"` + cv + `"`
	return "[" + strings.Join([]string{
		`{"type":"system","subtype":"init","cwd":"` + cv + `",` + meta + `,"tools":["` + cv + `"],"slash_commands":["` + cv + `"],"terminal_slash_commands":["` + cv +
			`"],"agents":["` + cv + `"],"skills":["` + cv + `"],"plugins":[{"path":"` + cv + `"}],"capabilities":["` + cv + `"],"memory_paths":{"auto":"` + cv +
			`"},"messaging_socket_path":"` + cv + `","model":"m","unknown_meta":"` + exportKept + `"}`,
		`{"type":"assistant","message":{"id":"` + cv + `","content":[{"type":"text","text":"` + cv + `"},{"type":"tool_use","id":"toolu_S","name":"ToolSearch","input":{"query":"select:mcp__probe__slow"}}]},` + meta + `}`,
		`{"type":"user","message":{"content":[{"type":"tool_result","tool_use_id":"toolu_S","content":[{"type":"tool_reference","tool_name":"mcp__probe__slow"}]}]},` + meta + `}`,
		`{"type":"rate_limit_event","rate_limit_info":{"status":"allowed"},"uuid":"` + cv + `","session_id":null}`,
		`{"type":"assistant","message":{"id":"` + cv + `","content":[{"type":"tool_use","id":"toolu_P","name":"mcp__probe__slow","input":{"case_id":"claude-capture-setup"}}]},` + meta + `}`,
		`{"type":"user","message":{"content":[{"tool_use_id":"toolu_P","type":"tool_result","content":[{"type":"text","text":"` + res + `"}]}]},` + meta + `}`,
		`{"type":"result","subtype":"success","is_error":false,"terminal_reason":"completed","result":"` + cv + `",` + meta +
			`,"total_cost_usd":0.1227972,"modelUsage":{"claude-sonnet-5-5":{"inputTokens":6,"costUSD":0.1227972,"costBasis":"list"}}}`,
	}, ",") + "]\n"
}

// exportSource is a validated fake one-client capture of client whose
// session prints exportTranscript, and the test policy accepting it.
func exportSource(t *testing.T, client string) (*sharedCapture, *FixturePolicy) {
	t.Helper()
	sc := cachedCapture(t, "export-"+client, func(w *capWorld) *CaptureRunner {
		w.script = func(spec ProcSpec) capBehavior {
			b := w.defaults(spec)
			if launchKind(spec) == "session" {
				b.stdout = fakeExportTranscript(client)
			}
			return b
		}
		return newCapRunner(t, w, capPlan(t, client))
	})
	// The placement check observes the test's fixture root (DW10 boundary):
	// a host marker above the temporary directory (a sandbox's /tmp/.git)
	// never decides these tests; every marker they plant is real.
	pol := FixturePolicyForTests(FixtureSource{Client: client, Version: capVersions[client], Platform: "linux/amd64", RunID: sc.man.RunID,
		ManifestSHA256: sha256Hex(sc.bundle.ManifestBytes)}).WithPlacementView(newTempViewAt(filepath.Dir(t.TempDir()), filepath.Dir(sc.dir)))
	return sc, pol
}

// realDir is a fresh test directory without a symbolic link in its path.
func realDir(t *testing.T) string {
	t.Helper()
	d, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return d
}

// readTree reads every regular file below dir by slash path.
func readTree(t *testing.T, dir string) map[string][]byte {
	t.Helper()
	out := map[string][]byte{}
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		rel, _ := filepath.Rel(dir, p)
		b, err := os.ReadFile(p)
		out[filepath.ToSlash(rel)] = b
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func TestFixtureExportDeterministic(t *testing.T) {
	for _, client := range []string{"codex", "grok", "claude"} {
		sc, pol := exportSource(t, client)
		root := realDir(t)
		a, b := filepath.Join(root, "a"), filepath.Join(root, "b")
		sa, err := ExportFixture(sc.dir, a, pol)
		if err != nil {
			t.Fatalf("%s: %v", client, err)
		}
		if _, err := ExportFixture(sc.dir, b, pol); err != nil {
			t.Fatal(err)
		}
		ta, tb := readTree(t, a), readTree(t, b)
		if len(ta) != len(sc.bundle.Files)+2 || len(ta) != len(tb) {
			t.Fatalf("%s: %d / %d files", client, len(ta), len(tb))
		}
		for p, d := range ta {
			if !bytes.Equal(d, tb[p]) {
				t.Fatalf("%s: %s differs between two runs", client, p)
			}
			if bytes.Contains(d, []byte(exportCanary)) {
				t.Fatalf("%s: the canary survived in %s", client, p)
			}
		}
		// Modes: 0700 directories, 0600 files.
		filepath.WalkDir(a, func(p string, d fs.DirEntry, err error) error {
			info, _ := d.Info()
			if want := fs.FileMode(0o600); d.IsDir() {
				want = 0o700 | fs.ModeDir
				if info.Mode() != want {
					t.Errorf("%s: %s mode %v", client, p, info.Mode())
				}
			} else if info.Mode() != want {
				t.Errorf("%s: %s mode %v", client, p, info.Mode())
			}
			return nil
		})
		// No oracle, attestation or index.
		if _, ok := ta[ExpectedName]; ok {
			t.Fatalf("%s: the exporter wrote an oracle", client)
		}
		if !bytes.Contains(ta[ClientFile(client, FileVendorEvents)], []byte(FixtureMetadataValue)) || len(sa.Replacements) == 0 {
			t.Fatalf("%s: nothing replaced", client)
		}
		// Everything but the transcript is byte-identical; the probe
		// events always are.
		for p, d := range sc.bundle.Files {
			if p != ClientFile(client, FileVendorEvents) && !bytes.Equal(d, ta[p]) {
				t.Fatalf("%s: %s changed", client, p)
			}
		}
		// The unknown metadata member is kept for the review boundary.
		if client != "grok" && !bytes.Contains(ta[ClientFile(client, FileVendorEvents)], []byte(exportKept)) {
			t.Fatalf("%s: an unlisted member was dropped", client)
		}
	}
}

func TestFixtureExportProtectedSpans(t *testing.T) {
	for _, client := range []string{"codex", "grok", "claude"} {
		sc, pol := exportSource(t, client)
		out := filepath.Join(realDir(t), "out")
		san, err := ExportFixture(sc.dir, out, pol)
		if err != nil {
			t.Fatal(err)
		}
		files := readTree(t, out)
		vendor := ClientFile(client, FileVendorEvents)
		src, _ := ReplayTranscript(sc.bundle.Files[vendor])
		exp, err := ReplayTranscript(files[vendor])
		if err != nil || len(src.Lines) != len(exp.Lines) {
			t.Fatalf("%s: records %v", client, err)
		}
		var masked bytes.Buffer
		n := 0
		for i := range src.Lines {
			if src.Lines[i].OffsetNS != exp.Lines[i].OffsetNS {
				t.Fatalf("%s: record %d offset changed", client, i)
			}
			_, se, sp, err1 := recordEdits(client, src.Lines[i].Data)
			_, ee, ep, err2 := recordEdits(client, exp.Lines[i].Data)
			if err1 != nil || err2 != nil || len(se) != len(ee) {
				t.Fatalf("%s: record %d edits %v %v", client, i, err1, err2)
			}
			n += len(se)
			// Every byte outside the edited spans and every protected span
			// is unchanged.
			mask := func(fixtureEdit) string { return protectedMaskToken }
			sm, em := spliceEdits(src.Lines[i].Data, se, mask), spliceEdits(exp.Lines[i].Data, ee, mask)
			if !bytes.Equal(sm, em) || !slices.EqualFunc(protectedBytes(src.Lines[i].Data, sp), protectedBytes(exp.Lines[i].Data, ep), bytes.Equal) {
				t.Fatalf("%s: record %d changed outside its metadata spans", client, i)
			}
			masked.WriteString(strings.TrimSpace(strings.Repeat(" ", 0)) + itoa(src.Lines[i].OffsetNS) + "\n")
			masked.Write(sm)
			masked.WriteByte('\n')
		}
		masked.Write(sc.bundle.Files[ClientFile(client, FileServerEvents)])
		if n != len(san.Replacements) || sha256Hex(masked.Bytes()) != san.ProtectedSHA256 || san.SourceManifestSHA256 != sha256Hex(sc.bundle.ManifestBytes) ||
			san.ExportedManifestSHA256 != sha256Hex(files[CaptureManifestName]) {
			t.Fatalf("%s: receipt %+v", client, san)
		}
		// The escaped tool result survives byte for byte.
		if !bytes.Contains(files[vendor], []byte(`\\\"nonce\\\":\\\"noncecap1\\\"`)) {
			t.Fatalf("%s: the escaped result changed", client)
		}
		// The manifest changes only the transcript's hash and size.
		var a, b map[string]any
		json.Unmarshal(sc.bundle.ManifestBytes, &a)
		json.Unmarshal(files[CaptureManifestName], &b)
		af, bf := a["files"].([]any), b["files"].([]any)
		delete(a, "files")
		delete(b, "files")
		ja, _ := json.Marshal(a)
		jb, _ := json.Marshal(b)
		if !bytes.Equal(ja, jb) || len(af) != len(bf) {
			t.Fatalf("%s: the manifest changed beyond its files", client)
		}
		for i := range af {
			x, y := af[i].(map[string]any), bf[i].(map[string]any)
			if x["path"] != y["path"] || x["path"] != vendor && (x["sha256"] != y["sha256"] || x["bytes"] != y["bytes"]) {
				t.Fatalf("%s: file %v changed", client, x["path"])
			}
		}
		// Every exported file is a capture-policy fixed point.
		for p, d := range files {
			if err := RedactionFixedPoint(p, d, NewCaptureRedactor(nil, nil)); err != nil {
				t.Fatal(err)
			}
		}
	}
	// The Claude rules are scoped: one null metadata value stays null and
	// is not listed; the nested array stays one record.
	sc, pol := exportSource(t, "claude")
	out := filepath.Join(realDir(t), "out")
	san, err := ExportFixture(sc.dir, out, pol)
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range san.Replacements {
		if r.Record != 0 || r.Pointer == "/3/session_id" {
			t.Fatalf("claude replacement %+v", r)
		}
	}
	if b := readTree(t, out)[ClientFile("claude", FileVendorEvents)]; bytes.Count(b, []byte{'\n'}) != 1 || !bytes.Contains(b, []byte(`\"session_id\":null`)) {
		t.Fatal("the claude array was split or its null changed")
	}
}

func itoa(n int64) string { b, _ := json.Marshal(n); return string(b) }

func TestFixtureExportRefusals(t *testing.T) {
	sc, pol := exportSource(t, "codex")
	root := realDir(t)
	fresh := func(name string) string { return filepath.Join(root, name) }
	mustFail := func(name string, err error, want string) {
		t.Helper()
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Fatalf("%s: %v", name, err)
		}
	}
	_, err := ExportFixture(sc.dir, fresh("ok"), pol)
	if err != nil {
		t.Fatal(err)
	}
	// Output safety.
	_, err = ExportFixture(sc.dir, fresh("ok"), pol)
	mustFail("existing", err, "must not exist")
	_, err = ExportFixture(sc.dir, filepath.Join(sc.dir, "inside"), pol)
	mustFail("inside-source", err, "inside the source tree")
	os.Symlink(root, fresh("link"))
	_, err = ExportFixture(sc.dir, filepath.Join(fresh("link"), "out"), pol)
	mustFail("symlink", err, "symbolic link")
	repo := fresh("repo")
	os.MkdirAll(filepath.Join(repo, ".git"), 0o700)
	os.MkdirAll(filepath.Join(repo, "sub"), 0o700)
	_, err = ExportFixture(sc.dir, filepath.Join(repo, "sub", "out"), pol)
	mustFail("worktree", err, "Git work tree")
	// A .git file (a linked work tree) and a .git lookup that fails (a
	// directory without search permission) refuse too: real probes inside
	// the fixture root.
	gitFile := fresh("gitfile")
	os.MkdirAll(filepath.Join(gitFile, "sub"), 0o700)
	os.WriteFile(filepath.Join(gitFile, ".git"), []byte("gitdir: /elsewhere\n"), 0o600)
	_, err = ExportFixture(sc.dir, filepath.Join(gitFile, "sub", "out"), pol)
	mustFail("git-file", err, "Git work tree")
	noSearch := fresh("nosearch")
	os.Mkdir(noSearch, 0o600)
	t.Cleanup(func() { os.Chmod(noSearch, 0o700) })
	_, err = ExportFixture(sc.dir, filepath.Join(noSearch, "out"), pol)
	mustFail("git-lookup-error", err, "or its .git lookup failed")
	// The DW10 boundary: markers above the fixture root are modeled clean
	// (the export succeeds under a hostile .git above it), the same marker
	// at the root refuses, and the operating system's view (nil) is the
	// production default.
	hostile := fresh("hostile")
	os.MkdirAll(filepath.Join(hostile, ".git"), 0o700)
	fixtureRoot := filepath.Join(hostile, "fixture")
	os.Mkdir(fixtureRoot, 0o700)
	bounded := pol.WithPlacementView(newTempViewAt(fixtureRoot))
	if _, err := ExportFixture(sc.dir, filepath.Join(fixtureRoot, "out"), bounded); err != nil {
		t.Fatalf("a marker above the fixture root decided the export: %v", err)
	}
	os.Mkdir(filepath.Join(fixtureRoot, ".git"), 0o700)
	_, err = ExportFixture(sc.dir, filepath.Join(fixtureRoot, "out2"), bounded)
	mustFail("marker-at-root", err, "Git work tree")
	if _, err := ExportFixture(sc.dir, filepath.Join(fixtureRoot, "out3"), FixturePolicyForTests(pol.sources...)); err == nil || !strings.Contains(err.Error(), "Git work tree") {
		t.Fatalf("the operating system's view: %v", err)
	}
	if ProductionFixturePolicy().placement != nil || FixturePolicyForTests(pol.sources...).placement != nil {
		t.Fatal("a policy other than a test's carries a placement view")
	}
	_, err = ExportFixture(sc.dir, filepath.Join(fresh("absent"), "out"), pol)
	mustFail("no-parent", err, "does not exist")
	os.WriteFile(fresh("file"), nil, 0o600)
	_, err = ExportFixture(sc.dir, filepath.Join(fresh("file"), "out"), pol)
	mustFail("file-parent", err, "not a directory")
	_, err = ExportFixture("relative", fresh("x"), pol)
	if !errors.Is(err, errExportUsage) {
		t.Fatalf("relative: %v", err)
	}
	// Write failure: an unwritable parent leaves no manifest.
	ro := fresh("ro")
	os.Mkdir(ro, 0o500)
	t.Cleanup(func() { os.Chmod(ro, 0o700) })
	_, err = ExportFixture(sc.dir, filepath.Join(ro, "out"), pol)
	mustFail("write-failure", err, "permission denied")
	if _, err := os.Stat(filepath.Join(ro, "out", CaptureManifestName)); err == nil {
		t.Fatal("a failed export published a manifest")
	}
	// Identity: the production policy, a wrong pin, version or run.
	_, err = ExportFixture(sc.dir, fresh("prod"), ProductionFixturePolicy())
	mustFail("production-policy", err, "not an accepted source identity")
	src := pol.sources[0]
	for name, s := range map[string]FixtureSource{
		"pin":      {src.Client, src.Version, src.Platform, src.RunID, strings.Repeat("0", 64), src.Policy},
		"version":  {src.Client, src.Version + "x", src.Platform, src.RunID, src.ManifestSHA256, src.Policy},
		"platform": {src.Client, src.Version, "darwin/arm64", src.RunID, src.ManifestSHA256, src.Policy},
		"run":      {src.Client, src.Version, src.Platform, "other", src.ManifestSHA256, src.Policy},
		// Design decoder-enrollment B3: a source bound to another client's
		// policy is cross-policy substitution, never accepted.
		"policy": {src.Client, src.Version, src.Platform, src.RunID, src.ManifestSHA256, SanitizationPolicyB3Cursor},
	} {
		_, err = ExportFixture(sc.dir, fresh("id-"+name), FixturePolicyForTests(s))
		if err == nil || strings.Contains(err.Error(), src.ManifestSHA256) {
			t.Fatalf("%s: %v", name, err)
		}
		if _, err := os.Stat(fresh("id-" + name)); err == nil {
			t.Fatalf("%s: a refused source created the output", name)
		}
	}
	// Unsafe input: a symbolic link, an unlisted file, a policy member of
	// another type and a partial capture.
	bad := func(name string, mutate func(dir string)) error {
		dir := fresh("src-" + name)
		copyDirForTest(t, sc.dir, dir)
		mutate(dir)
		_, err := ExportFixture(dir, fresh("out-"+name), pol)
		return err
	}
	mustFail("symlink-payload", bad("link", func(dir string) {
		p := filepath.Join(dir, "clients", "codex", FileVersionStderr)
		os.Remove(p)
		os.Symlink("/etc/hostname", p)
	}), "not a valid capture bundle")
	mustFail("extra-file", bad("extra", func(dir string) { os.WriteFile(filepath.Join(dir, "extra.txt"), nil, 0o600) }), "not a valid capture bundle")
	mustFail("altered-payload", bad("altered", func(dir string) {
		os.WriteFile(filepath.Join(dir, "clients", "codex", FileVersionStderr), []byte("x"), 0o600)
	}), "not a valid capture bundle")
	// The secret/size of a source is never printed: errors carry classes.
	if err := bad("pin-silent", func(string) {}); err != nil {
		t.Fatalf("an unchanged copy failed: %v", err)
	}
}

// copyDirForTest copies a capture bundle directory.
func copyDirForTest(t *testing.T, from, to string) {
	t.Helper()
	for p, d := range readTree(t, from) {
		dst := filepath.Join(to, filepath.FromSlash(p))
		os.MkdirAll(filepath.Dir(dst), 0o700)
		if err := os.WriteFile(dst, d, 0o600); err != nil {
			t.Fatal(err)
		}
	}
}

// exportRecord parses one fabricated record for the rule tests.
func TestFixtureRulesUnsafe(t *testing.T) {
	for name, tc := range map[string]struct {
		client, data, want string
	}{
		"codex-number-thread": {"codex", `{"type":"thread.started","thread_id":7}`, "/thread_id has an unexpected type"},
		"grok-string-tools":   {"grok", `{"type":"available_commands","tools":"x","commands":[]}`, "/tools has an unexpected type"},
		"claude-array-cwd":    {"claude", `[{"type":"system","subtype":"init","cwd":[]}]`, "/0/cwd has an unexpected type"},
		"claude-number-uuid":  {"claude", `[{"type":"assistant","uuid":5,"message":{"content":[]}}]`, "/0/uuid has an unexpected type"},
		"claude-memory-array": {"claude", `[{"type":"system","subtype":"init","memory_paths":[]}]`, "/0/memory_paths has an unexpected type"},
		"not-json":            {"codex", `{"type":`, "not strict JSON"},
		"duplicate":           {"grok", `{"type":"text","type":"text"}`, "not strict JSON"},
	} {
		_, _, _, err := recordEdits(tc.client, []byte(tc.data))
		if err == nil || !strings.Contains(err.Error(), tc.want) || strings.Contains(err.Error(), "x\"") {
			t.Errorf("%s: %v", name, err)
		}
	}
	// Absent members are left absent, nulls stay null, unrelated records
	// and other members (even with a policy key name) are never edited.
	for _, tc := range []struct{ client, data string }{
		{"codex", `{"type":"turn.completed","thread_id":"x"}`},
		{"codex", `{"type":"item.completed","item":{"id":"i","type":"mcp_tool_call","text":"x","message":"y"}}`},
		{"grok", `{"type":"tool_call","data":"x","signature":"y","sessionId":"z"}`},
		{"grok", `{"type":"tool_call","sessionId":"s","total_cost_usd":1}`},
		{"claude", `[{"type":"user","message":{"content":[{"type":"text","text":"x"}]}},{"type":"system","subtype":"other","cwd":"x"}]`},
		{"claude", `{"type":"result","result":"x"}`},
		{"cursor", `{"type":"user","message":{"role":"user","content":[]}}`},
	} {
		_, edits, _, err := recordEdits(tc.client, []byte(tc.data))
		if err != nil || len(edits) != 0 {
			t.Errorf("%s %s: %v %v", tc.client, tc.data, edits, err)
		}
	}
	// The guard: overlapping, repeated and protected-touching edits.
	data := []byte(`{"a":{"b":"c"}}`)
	root, _ := parseSpans(data)
	a, b := root.member("a"), root.member("a").member("b")
	if _, err := checkEdits([]fixtureEdit{{"/a", a, '{'}, {"/a/b", b, '"'}}, nil); err == nil {
		t.Fatal("overlapping edits accepted")
	}
	if _, err := checkEdits([]fixtureEdit{{"/a/b", b, '"'}, {"/a/b", b, '"'}}, nil); err == nil {
		t.Fatal("a repeated edit accepted")
	}
	if _, err := checkEdits([]fixtureEdit{{"/a/b", b, '"'}}, []*jspan{a}); err == nil || !strings.Contains(err.Error(), "protected evidence") {
		t.Fatalf("an edit inside protected evidence: %v", err)
	}
	if got, err := checkEdits([]fixtureEdit{{"/a/b", b, '"'}}, []*jspan{root.member("a")}[:0]); err != nil || len(got) != 1 {
		t.Fatal(err)
	}
	// RFC 6901 escaping.
	if pointerToken("a/b~c") != "a~1b~0c" {
		t.Fatal(pointerToken("a/b~c"))
	}
	// The span parser agrees with the strict checker.
	for _, s := range []string{`[1,-2.5e3,true,false,null,"\"",{"k":[]}]`, `{}`, `[]`} {
		n, err := parseSpans([]byte(s))
		if err != nil || n.start != 0 || n.end != len(s) {
			t.Fatalf("%s: %v", s, err)
		}
	}
	// A non-canonical wrapper is refused.
	if _, err := exportTranscript("codex", "v.jsonl", []byte(`{"offset_ns": 1,"data":"{}"}`+"\n")); err == nil {
		t.Fatal("a non-canonical wrapper exported")
	}
	if _, err := exportTranscript("codex", "v.jsonl", []byte(`{"offset_ns":1,"data":"{}"}`)); err == nil {
		t.Fatal("an unterminated transcript exported")
	}
}

// Design decoder-enrollment B2 amendment A1 (policy v2): the map
// replacement of Claude's rate_limit_info, the scoped number-to-string
// replacement of exactly the five cost members (every other number kept),
// refusal of a wrong observed type, an absent member or another modelUsage
// member set, and refusal of policy v1 by the production validators.
func TestFixturePolicyA1(t *testing.T) {
	claude := `[{"type":"rate_limit_event","rate_limit_info":{"status":"allowed","utilization":0.25,"resetsAt":1791470400},"uuid":"u"},` +
		`{"type":"result","subtype":"success","is_error":false,"terminal_reason":"completed","result":"r","duration_api_ms":5713,"total_cost_usd":0.1227972,` +
		`"modelUsage":{"claude-sonnet-5-5":{"inputTokens":6,"costUSD":0.1227972,"costBasis":"list"}}}]`
	grok := `{"type":"end","stopReason":"end_turn","sessionId":"s","requestId":"q","num_turns":2,"total_cost_usd":0.03165128,"total_cost_usd_ticks":316512800,` +
		`"modelUsage":{"grok-4.7-build":{"inputTokens":43422,"modelCalls":2,"costUSD":0.03165128}}}`
	for _, tc := range []struct {
		client, data string
		pointers     []string
		want, keep   []string
	}{
		{"claude", claude, []string{"/0/rate_limit_info", "/0/uuid", "/1/modelUsage/claude-sonnet-5-5/costUSD", "/1/result", "/1/total_cost_usd"},
			[]string{`"rate_limit_info":{}`, `"total_cost_usd":"[fixture metadata]"`, `"costUSD":"[fixture metadata]"`},
			[]string{`"type":"rate_limit_event"`, `"duration_api_ms":5713`, `"inputTokens":6`, `"costBasis":"list"`}},
		{"grok", grok, []string{"/modelUsage/grok-4.7-build/costUSD", "/requestId", "/sessionId", "/total_cost_usd", "/total_cost_usd_ticks"},
			[]string{`"total_cost_usd":"[fixture metadata]"`, `"total_cost_usd_ticks":"[fixture metadata]"`, `"costUSD":"[fixture metadata]"`},
			[]string{`"num_turns":2`, `"inputTokens":43422`, `"modelCalls":2`, `"stopReason":"end_turn"`}},
	} {
		_, edits, _, err := recordEdits(tc.client, []byte(tc.data))
		if err != nil {
			t.Fatalf("%s: %v", tc.client, err)
		}
		var got []string
		for _, e := range edits {
			got = append(got, e.pointer)
		}
		slices.Sort(got)
		if !slices.Equal(got, tc.pointers) {
			t.Fatalf("%s edits %v", tc.client, got)
		}
		exp := string(spliceEdits([]byte(tc.data), edits, func(e fixtureEdit) string { return replacementLiteral[e.want] }))
		for _, w := range append(tc.want, tc.keep...) {
			if !strings.Contains(exp, w) {
				t.Fatalf("%s export lacks %s: %s", tc.client, w, exp)
			}
		}
		if strings.Contains(exp, "0.1227972") || strings.Contains(exp, "0.03165128") || strings.Contains(exp, "utilization") {
			t.Fatalf("%s kept a declined value: %s", tc.client, exp)
		}
		// The exported record holds the same, already-neutral, edits.
		if _, again, _, err := recordEdits(tc.client, []byte(exp)); err != nil || len(again) != len(edits) {
			t.Fatalf("%s re-check: %v", tc.client, err)
		}
	}
	// Refusals: a wrong observed type, an absent member, another member set.
	for name, tc := range map[string]struct{ client, data, want string }{
		"cost-string":       {"claude", strings.Replace(claude, `"total_cost_usd":0.1227972`, `"total_cost_usd":"0.12"`, 1), "/1/total_cost_usd has an unexpected type"},
		"cost-null":         {"grok", strings.Replace(grok, `"total_cost_usd":0.03165128`, `"total_cost_usd":null`, 1), "/total_cost_usd has an unexpected type"},
		"cost-absent":       {"claude", strings.Replace(claude, `"total_cost_usd":0.1227972,`, ``, 1), "/1/total_cost_usd is absent"},
		"ticks-absent":      {"grok", strings.Replace(grok, `"total_cost_usd_ticks":316512800,`, ``, 1), "/total_cost_usd_ticks is absent"},
		"ticks-bool":        {"grok", strings.Replace(grok, `"total_cost_usd_ticks":316512800`, `"total_cost_usd_ticks":true`, 1), "unexpected type"},
		"costusd-absent":    {"grok", strings.Replace(grok, `,"costUSD":0.03165128`, ``, 1), "costUSD is absent"},
		"costusd-string":    {"claude", strings.Replace(claude, `"costUSD":0.1227972`, `"costUSD":"x"`, 1), "costUSD has an unexpected type"},
		"two-models":        {"claude", strings.Replace(claude, `"modelUsage":{`, `"modelUsage":{"other":{"costUSD":1},`, 1), "not exactly the observed model"},
		"other-model":       {"grok", strings.Replace(grok, `"grok-4.7-build"`, `"grok-4.8"`, 1), "not exactly the observed model"},
		"no-model-usage":    {"grok", strings.Replace(grok, `,"modelUsage":{"grok-4.7-build":{"inputTokens":43422,"modelCalls":2,"costUSD":0.03165128}}`, ``, 1), "not exactly the observed model"},
		"rate-limit-array":  {"claude", strings.Replace(claude, `"rate_limit_info":{"status":"allowed","utilization":0.25,"resetsAt":1791470400}`, `"rate_limit_info":[]`, 1), "rate_limit_info has an unexpected type"},
		"rate-limit-null":   {"claude", strings.Replace(claude, `"rate_limit_info":{"status":"allowed","utilization":0.25,"resetsAt":1791470400}`, `"rate_limit_info":null`, 1), "rate_limit_info has an unexpected type"},
		"rate-limit-absent": {"claude", strings.Replace(claude, `"rate_limit_info":{"status":"allowed","utilization":0.25,"resetsAt":1791470400},`, ``, 1), "rate_limit_info is absent"},
	} {
		if _, _, _, err := recordEdits(tc.client, []byte(tc.data)); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: %v", name, err)
		}
	}
	// Policy v1 is refused by the production validators: in the receipt
	// and in the oracle.
	if SanitizationPolicyB2 != "decoder-enrollment-b2-metadata-v2" {
		t.Fatal(SanitizationPolicyB2)
	}
	ok := Attestation{Owner: B2OwnerHandle, Reviewer: B2ReviewerHandle, Policy: CaptureRedactionPolicy}
	fsys, opts, e := sanitizedRepo(t, ok)
	v1 := "decoder-enrollment-b2-metadata-v1"
	sb := bytes.Replace(fsys[path.Join(e.Bundle, SanitizationName)].Data, []byte(SanitizationPolicyB2), []byte(v1), 1)
	if _, err := ParseFixtureSanitization(sb); err == nil || !strings.Contains(err.Error(), "policy") {
		t.Fatalf("a v1 receipt parsed: %v", err)
	}
	for name, mutate := range map[string]func(f fstest.MapFS){
		"receipt": func(f fstest.MapFS) {
			f[path.Join(e.Bundle, SanitizationName)] = &fstest.MapFile{Data: sb, Mode: 0o600}
			en := e
			h := sha256Hex(sb)
			en.SanitizationSHA256 = &h
			ib, _ := encodeIndent(EnrollmentIndex{Schema: EnrollmentSchema, Entries: []EnrollmentEntry{en}})
			f[EnrollmentIndexPath] = &fstest.MapFile{Data: ib, Mode: 0o600}
		},
		"oracle": func(f fstest.MapFS) {
			ob := bytes.Replace(f[path.Join(e.Bundle, ExpectedName)].Data, []byte(SanitizationPolicyB2), []byte(v1), 1)
			f[path.Join(e.Bundle, ExpectedName)] = &fstest.MapFile{Data: ob, Mode: 0o600}
			en := e
			en.ExpectedSHA256 = sha256Hex(ob)
			ib, _ := encodeIndent(EnrollmentIndex{Schema: EnrollmentSchema, Entries: []EnrollmentEntry{en}})
			f[EnrollmentIndexPath] = &fstest.MapFile{Data: ib, Mode: 0o600}
		},
	} {
		f := fstest.MapFS{}
		for k, v := range fsys {
			cp := *v
			f[k] = &cp
		}
		mutate(f)
		o := opts
		o.FS = f
		if _, err := ValidateEnrollment(o); err == nil || !strings.Contains(err.Error(), "policy") {
			t.Errorf("%s v1: %v", name, err)
		}
	}
}

// Code review B2 round 1, W1: the export checks the embedded
// structured-result JSON strings with checkJSON (no decoder): Codex's probe
// item text blocks, Grok's OkayOutput and Claude's probe tool_result text
// blocks. Malformed, duplicate-member and over-deep payloads are refused,
// in the exporter and in the receipt check; an unrelated tool's plain text
// result is not an embedded probe result.
func TestFixtureEmbeddedResults(t *testing.T) {
	q := func(s string) string { b, _ := json.Marshal(s); return string(b) }
	deep := strings.Repeat(`{"a":`, maxDepth+1) + `1` + strings.Repeat(`}`, maxDepth+1)
	payloads := map[string]string{
		"malformed": `{"case_id":"c","nonce":`,
		"duplicate": `{"case_id":"c","case_id":"c","nonce":"n"}`,
		"too-deep":  deep,
		"trailing":  `{"case_id":"c","nonce":"n"} x`,
	}
	codex := func(text string) string {
		return `{"type":"item.completed","item":{"id":"item_3","type":"mcp_tool_call","server":"probe","tool":"slow","arguments":{"case_id":"c"},"result":{"content":[{"type":"text","text":` +
			q(text) + `}],"structured_content":null},"error":null,"status":"completed"}}`
	}
	grok := func(text string) string {
		return `{"type":"tool_call_update","toolCallId":"call-1","status":"completed","content":[],"rawOutput":{"type":"MCP","tool_name":"slow","server_name":"probe","output":{"OkayOutput":` +
			q(text) + `}},"locations":[]}`
	}
	claude := func(text string) string {
		return `[{"type":"assistant","message":{"content":[{"type":"tool_use","id":"toolu_P","name":"mcp__probe__slow","input":{"case_id":"c"}}]}},` +
			`{"type":"user","message":{"content":[{"tool_use_id":"toolu_P","type":"tool_result","content":[{"type":"text","text":` + q(text) + `}]}]}}]`
	}
	render := map[string]func(string) string{"codex": codex, "grok": grok, "claude": claude}
	for client, f := range render {
		if _, _, _, err := recordEdits(client, []byte(f(`{"case_id":"c","nonce":"n"}`))); err != nil {
			t.Fatalf("%s valid embedded result: %v", client, err)
		}
		for name, p := range payloads {
			data := []byte(f(p))
			if _, _, _, err := recordEdits(client, data); err == nil || !strings.Contains(err.Error(), "embedded result that is not strict JSON") {
				t.Errorf("%s %s: %v", client, name, err)
			}
			// The exporter refuses the record; the receipt check (which runs
			// the same rules over exported bytes) refuses it too.
			raw := append(wrapLine(1, data), '\n')
			if _, err := exportTranscript(client, ClientFile(client, FileVendorEvents), raw); err == nil {
				t.Errorf("%s %s: exported", client, name)
			}
			if _, err := exportedReplacements(client, ClientFile(client, FileVendorEvents), raw); err == nil {
				t.Errorf("%s %s: receipt check accepted", client, name)
			}
		}
	}
	// A non-string embedded result is refused; an unrelated tool's plain
	// text result is not a probe result and stays acceptable.
	if _, _, _, err := recordEdits("grok", []byte(strings.Replace(grok("x"), `"OkayOutput":"x"`, `"OkayOutput":{"case_id":"c"}`, 1))); err == nil {
		t.Fatal("a non-string OkayOutput passed")
	}
	for client, data := range map[string]string{
		"codex":  strings.Replace(codex("plain text"), `"server":"probe"`, `"server":"files"`, 1),
		"grok":   strings.Replace(grok("plain text"), `"server_name":"probe"`, `"server_name":"files"`, 1),
		"claude": strings.Replace(claude("plain text"), `"name":"mcp__probe__slow"`, `"name":"Read"`, 1),
	} {
		if _, _, _, err := recordEdits(client, []byte(data)); err != nil {
			t.Errorf("%s unrelated plain text: %v", client, err)
		}
	}
}

// exportedBundle is an export of client and its validated bundle.
func exportedBundle(t *testing.T, client string) (*CaptureBundle, []byte, *FixturePolicy, string) {
	t.Helper()
	sc, pol := exportSource(t, client)
	out := filepath.Join(realDir(t), "out")
	if _, err := ExportFixture(sc.dir, out, pol); err != nil {
		t.Fatal(err)
	}
	b, err := ValidateCaptureBundle(os.DirFS(out), ".", SanitizationName)
	if err != nil {
		t.Fatal(err)
	}
	sb, _ := os.ReadFile(filepath.Join(out, SanitizationName))
	return b, sb, pol, out
}

func TestFixtureSanitizationReceipt(t *testing.T) {
	b, sb, pol, _ := exportedBundle(t, "grok")
	if _, err := CheckFixtureSanitization(pol, b, sb); err != nil {
		t.Fatal(err)
	}
	edit := func(f func(m map[string]any)) []byte {
		var m map[string]any
		json.Unmarshal(sb, &m)
		f(m)
		out, _ := encodeIndent(m)
		return out
	}
	reps := func(m map[string]any) []any { return m["replacements"].([]any) }
	for name, tc := range map[string]struct {
		sb   []byte
		want string
	}{
		"null-files":       {edit(func(m map[string]any) { m["files"] = nil }), "must be lists"},
		"unknown-member":   {edit(func(m map[string]any) { m["extra"] = 1 }), "unknown field"},
		"missing-member":   {edit(func(m map[string]any) { delete(m, "protected_sha256") }), "missing field"},
		"duplicate-member": {bytes.Replace(sb, []byte(`"schema": `), []byte(`"schema": "x", "schema": `), 1), "duplicate"},
		"schema":           {edit(func(m map[string]any) { m["schema"] = "v2" }), "schema"},
		"policy":           {edit(func(m map[string]any) { m["policy"] = "other" }), "schema"},
		"source-hash":      {edit(func(m map[string]any) { m["source_manifest_sha256"] = strings.Repeat("a", 64) }), "compiled source pin"},
		"exported-hash":    {edit(func(m map[string]any) { m["exported_manifest_sha256"] = strings.Repeat("a", 64) }), "differs from the manifest"},
		"protected-hash":   {edit(func(m map[string]any) { m["protected_sha256"] = strings.Repeat("a", 64) }), "protected_sha256"},
		"bad-hash":         {edit(func(m map[string]any) { m["protected_sha256"] = "x" }), "SHA-256"},
		"pointer":          {edit(func(m map[string]any) { reps(m)[0].(map[string]any)["pointer"] = "/other" }), "not the policy's"},
		"wildcard":         {edit(func(m map[string]any) { reps(m)[0].(map[string]any)["pointer"] = "/*" }), "not the policy's"},
		"value":            {edit(func(m map[string]any) { reps(m)[0].(map[string]any)["replacement"] = "x" }), "literal neutral value"},
		"missing-repl":     {edit(func(m map[string]any) { m["replacements"] = reps(m)[1:] }), "exactly the policy's"},
		"extra-repl":       {edit(func(m map[string]any) { m["replacements"] = append(reps(m), reps(m)[len(reps(m))-1]) }), "repeats"},
		"unsorted":         {edit(func(m map[string]any) { r := reps(m); r[0], r[1] = r[1], r[0] }), "sorted"},
		"record":           {edit(func(m map[string]any) { reps(m)[0].(map[string]any)["record"] = -1 }), "invalid path"},
		"files-hash": {edit(func(m map[string]any) {
			m["files"].([]any)[0].(map[string]any)["exported_sha256"] = strings.Repeat("b", 64)
		}), "manifest's payload"},
		"files-source": {edit(func(m map[string]any) {
			m["files"].([]any)[0].(map[string]any)["source_sha256"] = strings.Repeat("b", 64)
		}), "without a recorded replacement"},
		"files-missing":      {edit(func(m map[string]any) { m["files"] = m["files"].([]any)[1:] }), "exactly the manifest's"},
		"files-source-shape": {edit(func(m map[string]any) { m["files"].([]any)[0].(map[string]any)["source_sha256"] = "z" }), "not a SHA-256"},
	} {
		if _, err := CheckFixtureSanitization(pol, b, tc.sb); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: %v", name, err)
		}
	}
	if _, err := CheckFixtureSanitization(ProductionFixturePolicy(), b, sb); err == nil || !strings.Contains(err.Error(), "accepted source") {
		t.Fatalf("production policy: %v", err)
	}
	if _, err := ParseFixtureSanitization(bytes.Repeat([]byte(" "), maxIndexBytes+1)); err == nil {
		t.Fatal("an oversized receipt parsed")
	}
	// A metadata value that is not its neutral literal in the export.
	vendor := ClientFile("grok", FileVendorEvents)
	files := map[string][]byte{}
	for k, v := range b.Files {
		files[k] = v
	}
	files[vendor] = bytes.Replace(files[vendor], []byte(`\"data\":\"[fixture metadata]\"`), []byte(`\"data\":\"leaked\"`), 1)
	forged := *b
	forged.Files = files
	if _, err := CheckFixtureSanitization(pol, &forged, sb); err == nil || !strings.Contains(err.Error(), "neutral value") {
		t.Fatalf("a non-neutral value: %v", err)
	}
	// A protected change the replacement list cannot explain.
	files[vendor] = bytes.Replace(b.Files[vendor], []byte(`call-1`), []byte(`call-2`), -1)
	if _, err := CheckFixtureSanitization(pol, &forged, sb); err == nil || !strings.Contains(err.Error(), "protected") {
		t.Fatalf("a protected change: %v", err)
	}
}

// sanitizedRepo is a fake repository enrolling a sanitized claude export
// with attestation a, the injected policy and its test registry.
func sanitizedRepo(t *testing.T, a Attestation) (fstest.MapFS, EnrollmentOptions, EnrollmentEntry) {
	t.Helper()
	b, sb, pol, _ := exportedBundle(t, "claude")
	m := b.Manifest
	c := m.Clients[0]
	version := capVersions["claude"]
	tr, _ := ReplayTranscript(b.Files[ClientFile("claude", FileVendorEvents)])
	off := tr.Lines[0].OffsetNS
	policy := SanitizationPolicyB2
	o := &ExpectedOracle{CaseID: c.CaseID, Nonce: c.Nonce,
		Events: []Event{{CaseID: c.CaseID, Kind: KindToolCall, OffsetNS: off, RequestID: "toolu_P"},
			{CaseID: c.CaseID, Kind: KindToolResult, OffsetNS: off, RequestID: "toolu_P", Nonce: c.Nonce}},
		Terminal: true, ClientInfo: ExpectedClientInfo{Name: *c.Probe.ClientName, Version: *c.Probe.ClientVersion}, RequesterCompatible: true,
		ErrorKinds: []string{}, Capabilities: []string{CapToolCall, CapToolResult, CapTerminalSuccess}, CaptureState: CaptureComplete,
		SourceCaptureSHA256: pol.sources[0].ManifestSHA256, SanitizationPolicy: &policy, Attestation: a}
	ob, _ := encodeIndent(o)
	fixture := EnrolledFixtureID("claude-json", version, "linux/amd64")
	dir := EnrolledBundlePath("claude", version, "linux/amd64", m.RunID)
	ss := sha256Hex(sb)
	e := EnrollmentEntry{Client: "claude", Decoder: "claude-json", Version: version, Platform: "linux/amd64", Fixture: fixture, Bundle: dir,
		ManifestSHA256: sha256Hex(b.ManifestBytes), ExpectedSHA256: sha256Hex(ob), SanitizationSHA256: &ss}
	idx, _ := encodeIndent(EnrollmentIndex{Schema: EnrollmentSchema, Entries: []EnrollmentEntry{e}})
	fsys := fstest.MapFS{EnrollmentIndexPath: {Data: idx, Mode: 0o600}, path.Join(dir, CaptureManifestName): {Data: b.ManifestBytes, Mode: 0o600},
		path.Join(dir, ExpectedName): {Data: ob, Mode: 0o600}, path.Join(dir, SanitizationName): {Data: sb, Mode: 0o600}}
	for p, d := range b.Files {
		fsys[path.Join(dir, p)] = &fstest.MapFile{Data: d, Mode: 0o600}
	}
	reg := SyntheticRegistry().WithVersion("claude-json", DecoderVersion{Version: version, Fixture: fixture, Qualified: true,
		Evidence: []DecoderEvidence{{Platform: "linux/amd64", Fixture: fixture, Kinds: o.Capabilities}}})
	spec := reg["claude-json"]
	spec.exact = map[string]func(Transcript) Decoded{version: decodeClaudeReal}
	reg["claude-json"] = spec
	return fsys, EnrollmentOptions{FS: fsys, Registry: reg, policy: pol}, e
}

func TestFixtureAttestationGate(t *testing.T) {
	// Fake evidence exercising the structural rule only: these strings are
	// never real authorization.
	ok := Attestation{Owner: B2OwnerHandle, Reviewer: B2ReviewerHandle, Policy: CaptureRedactionPolicy}
	_, opts, _ := sanitizedRepo(t, ok)
	if _, err := ValidateEnrollment(opts); err != nil {
		t.Fatalf("the structural rule refused the exact handles: %v", err)
	}
	for name, a := range map[string]Attestation{
		"pending":          {Policy: CaptureRedactionPolicy},
		"reviewer-only":    {Reviewer: B2ReviewerHandle, Policy: CaptureRedactionPolicy},
		"owner-only":       {Owner: B2OwnerHandle, Policy: CaptureRedactionPolicy},
		"whitespace":       {Owner: " ", Reviewer: "\t", Policy: CaptureRedactionPolicy},
		"todo":             {Owner: "TODO", Reviewer: "TODO", Policy: CaptureRedactionPolicy},
		"tbd":              {Owner: "TBD", Reviewer: "TBD", Policy: CaptureRedactionPolicy},
		"pending-word":     {Owner: "pending", Reviewer: "pending", Policy: CaptureRedactionPolicy},
		"angle":            {Owner: "<owner>", Reviewer: "<reviewer>", Policy: CaptureRedactionPolicy},
		"example":          {Owner: "example-owner", Reviewer: "example-reviewer", Policy: CaptureRedactionPolicy},
		"test":             {Owner: "test", Reviewer: "test", Policy: CaptureRedactionPolicy},
		"legacy-function":  {Owner: "function-test owner", Reviewer: "function-test reviewer", Policy: CaptureRedactionPolicy},
		"swapped":          {Owner: B2ReviewerHandle, Reviewer: B2OwnerHandle, Policy: CaptureRedactionPolicy},
		"padded":           {Owner: B2OwnerHandle + " ", Reviewer: B2ReviewerHandle, Policy: CaptureRedactionPolicy},
		"case":             {Owner: strings.ToUpper(B2OwnerHandle), Reviewer: B2ReviewerHandle, Policy: CaptureRedactionPolicy},
		"omissions":        {Owner: B2OwnerHandle, Reviewer: B2ReviewerHandle, Policy: CaptureRedactionPolicy, Omissions: 1},
		"redaction-policy": {Owner: B2OwnerHandle, Reviewer: B2ReviewerHandle, Policy: "other"},
	} {
		if err := CheckB2Attestation(a); err == nil {
			t.Errorf("%s: accepted by the structural rule", name)
		}
		_, opts, _ := sanitizedRepo(t, a)
		if _, err := ValidateEnrollment(opts); err == nil || !strings.Contains(err.Error(), "attestation") {
			t.Errorf("%s: a pending or placeholder candidate validated: %v", name, err)
		}
	}
	if err := CheckB2Attestation(ok); err != nil {
		t.Fatal(err)
	}
}

func TestFixtureProvenanceChain(t *testing.T) {
	ok := Attestation{Owner: B2OwnerHandle, Reviewer: B2ReviewerHandle, Policy: CaptureRedactionPolicy}
	fsys, opts, e := sanitizedRepo(t, ok)
	dir := e.Bundle
	rewrite := func(mutate func(f fstest.MapFS)) EnrollmentOptions {
		f := fstest.MapFS{}
		for k, v := range fsys {
			cp := *v
			f[k] = &cp
		}
		mutate(f)
		o := opts
		o.FS = f
		return o
	}
	index := func(f fstest.MapFS, mut func(*EnrollmentEntry)) {
		en := e
		mut(&en)
		b, _ := encodeIndent(EnrollmentIndex{Schema: EnrollmentSchema, Entries: []EnrollmentEntry{en}})
		f[EnrollmentIndexPath] = &fstest.MapFile{Data: b, Mode: 0o600}
	}
	oracle := func(f fstest.MapFS, mut func(m map[string]any)) {
		var m map[string]any
		json.Unmarshal(f[path.Join(dir, ExpectedName)].Data, &m)
		mut(m)
		b, _ := encodeIndent(m)
		f[path.Join(dir, ExpectedName)] = &fstest.MapFile{Data: b, Mode: 0o600}
		index(f, func(en *EnrollmentEntry) { en.ExpectedSHA256 = sha256Hex(b) })
	}
	for name, tc := range map[string]struct {
		mutate func(f fstest.MapFS)
		want   string
	}{
		"receipt-hash": {func(f fstest.MapFS) {
			index(f, func(en *EnrollmentEntry) { x := strings.Repeat("c", 64); en.SanitizationSHA256 = &x })
		}, "sanitization.json's hash differs"},
		"receipt-missing": {func(f fstest.MapFS) { delete(f, path.Join(dir, SanitizationName)) }, "sanitization.json"},
		"receipt-unlisted": {func(f fstest.MapFS) {
			index(f, func(en *EnrollmentEntry) { en.SanitizationSHA256 = nil })
		}, "unexpected extra file sanitization.json"},
		"oracle-without-policy": {func(f fstest.MapFS) { oracle(f, func(m map[string]any) { delete(m, "sanitization_policy") }) }, "sanitization_policy"},
		"oracle-wrong-policy":   {func(f fstest.MapFS) { oracle(f, func(m map[string]any) { m["sanitization_policy"] = "other" }) }, "sanitization_policy"},
		"oracle-null-policy":    {func(f fstest.MapFS) { oracle(f, func(m map[string]any) { m["sanitization_policy"] = nil }) }, "nonempty string"},
		"oracle-source-hash": {func(f fstest.MapFS) {
			oracle(f, func(m map[string]any) { m["source_capture_sha256"] = strings.Repeat("d", 64) })
		}, "source manifest hash"},
		"null-receipt-hash": {func(f fstest.MapFS) {
			b := bytes.Replace(f[EnrollmentIndexPath].Data, []byte(`"sanitization_sha256": "`+*e.SanitizationSHA256+`"`), []byte(`"sanitization_sha256": null`), 1)
			f[EnrollmentIndexPath] = &fstest.MapFile{Data: b, Mode: 0o600}
		}, "explicit null"},
		"empty-receipt-hash": {func(f fstest.MapFS) {
			index(f, func(en *EnrollmentEntry) { x := ""; en.SanitizationSHA256 = &x })
		}, "SHA-256"},
	} {
		_, err := ValidateEnrollment(rewrite(tc.mutate))
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: %v", name, err)
		}
	}
	// The receipt is under the redaction fixed-point guard: an owner
	// literal found only in it fails the local check.
	san, err := ParseFixtureSanitization(fsys[path.Join(dir, SanitizationName)].Data)
	if err != nil {
		t.Fatal(err)
	}
	lit := opts
	lit.Literals = []string{san.ProtectedSHA256}
	if _, err := ValidateEnrollment(lit); err == nil || !strings.Contains(err.Error(), "redaction fixed point "+SanitizationName) {
		t.Fatalf("receipt fixed point: %v", err)
	}
	// A legacy entry (no receipt) whose oracle names no policy keeps its
	// rules; an enrolled B2 identity without a receipt never does.
	if err := validateSanitized(EnrollmentOptions{}, nil, EnrollmentEntry{Client: "codex", Version: CodexRealVersion}, nil, &ExpectedOracle{}, nil); err == nil ||
		!strings.Contains(err.Error(), "requires its sanitization.json receipt") {
		t.Fatalf("a B2 identity without its receipt: %v", err)
	}
	if err := validateSanitized(EnrollmentOptions{}, nil, EnrollmentEntry{Client: "codex", Version: "x"}, nil, &ExpectedOracle{}, nil); err != nil {
		t.Fatal(err)
	}
}

func TestRunFixtureExport(t *testing.T) {
	sc, pol := exportSource(t, "codex")
	root := realDir(t)
	var out, errOut bytes.Buffer
	run := func(args ...string) int {
		out.Reset()
		errOut.Reset()
		return RunFixtureExport(args, &out, &errOut, pol)
	}
	if code := run("--source", sc.dir, "--out", filepath.Join(root, "a")); code != 0 || !strings.Contains(out.String(), "no oracle, attestation, index or registry entry was written") {
		t.Fatalf("export = %d %q %q", code, out.String(), errOut.String())
	}
	if code := run("-source="+sc.dir, "-out="+filepath.Join(root, "b")); code != 0 {
		t.Fatalf("single-dash = %d %q", code, errOut.String())
	}
	for _, args := range [][]string{nil, {"--source", sc.dir}, {"--source", sc.dir, "--out"}, {"--source", "rel", "--out", "/x"},
		{"--source", sc.dir, "--out", filepath.Join(root, "c"), "--out", filepath.Join(root, "d")}, {"--source", sc.dir, "--out", filepath.Join(root, "c"), "--policy", "x"},
		{"positional"}, {"--source=", "--out=" + filepath.Join(root, "c")}} {
		if code := run(args...); code != 2 || !strings.Contains(errOut.String(), "usage: mcpfixture-export") {
			t.Fatalf("%q = %d %q", args, code, errOut.String())
		}
	}
	if code := run("--source", sc.dir, "--out", filepath.Join(root, "a")); code != 1 || strings.Contains(errOut.String(), exportCanary) {
		t.Fatalf("existing out = %d %q", code, errOut.String())
	}
	if _, err := os.Stat(filepath.Join(root, "c")); err == nil {
		t.Fatal("a refused invocation created output")
	}
}
