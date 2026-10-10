package reale2e

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// keyPath addresses a value inside a generic JSON tree: object keys and
// array indexes.
type keyPath []any

func (p keyPath) String() string {
	var b strings.Builder
	for _, k := range p {
		switch k := k.(type) {
		case string:
			b.WriteString("." + k)
		case int:
			fmt.Fprintf(&b, "[%d]", k)
		}
	}
	return b.String()
}

// decodeTree decodes JSON keeping numbers exact.
func decodeTree(t *testing.T, data []byte) any {
	t.Helper()
	d := json.NewDecoder(bytes.NewReader(data))
	d.UseNumber()
	var v any
	if err := d.Decode(&v); err != nil {
		t.Fatal(err)
	}
	return v
}

// keyPaths lists every object key path of v (only the first element of
// each array is descended into).
func keyPaths(v any, prefix keyPath) []keyPath {
	var out []keyPath
	switch x := v.(type) {
	case map[string]any:
		for k, e := range x {
			p := append(append(keyPath{}, prefix...), k)
			out = append(out, p)
			out = append(out, keyPaths(e, p)...)
		}
	case []any:
		if len(x) > 0 {
			out = append(out, keyPaths(x[0], append(append(keyPath{}, prefix...), 0))...)
		}
	}
	return out
}

// mutateTree returns a copy of v with the value at p deleted (null false)
// or replaced by null.
func mutateTree(t *testing.T, v any, p keyPath, null bool) []byte {
	t.Helper()
	b, _ := json.Marshal(v)
	c := decodeTree(t, b)
	cur := c
	for _, k := range p[:len(p)-1] {
		switch k := k.(type) {
		case string:
			cur = cur.(map[string]any)[k]
		case int:
			cur = cur.([]any)[k]
		}
	}
	m := cur.(map[string]any)
	last := p[len(p)-1].(string)
	if null {
		m[last] = nil
	} else {
		delete(m, last)
	}
	out, _ := json.Marshal(c)
	return out
}

// fieldAt resolves p to its struct field (the last key's).
func fieldAt(t reflect.Type, p keyPath) (reflect.StructField, bool) {
	var f reflect.StructField
	for _, k := range p {
		for t.Kind() == reflect.Pointer || t.Kind() == reflect.Slice || t.Kind() == reflect.Array {
			t = t.Elem()
		}
		if _, ok := k.(int); ok {
			continue
		}
		found := false
		for i := 0; i < t.NumField(); i++ {
			if name, _ := jsonField(t.Field(i)); name == k.(string) {
				f, found = t.Field(i), true
				break
			}
		}
		if !found {
			return f, false
		}
		t = f.Type
	}
	return f, true
}

// strictDocs are every strictly decoded evidence document of the passing
// bundle with its schema type.
func strictDocs() map[string]func() any {
	m := map[string]func() any{
		"manifest.json":                  func() any { return &Manifest{} },
		"setup/launch.json":              func() any { return &LaunchRecord{} },
		"setup/roles.json":               func() any { return &RolesRecord{} },
		"setup/flow.json":                func() any { return &Flow{} },
		"owner-decision.json":            func() any { return &DecisionRecord{} },
		"workspace/tree-manifest.json":   func() any { return &TreeManifest{} },
		"validation/baseline.json":       func() any { return &TestRun{} },
		"validation/final-test.json":     func() any { return &TestRun{} },
		"cleanup.json":                   func() any { return &CleanupRecord{} },
		"session/setup-attestation.json": func() any { return &SetupAttestation{} },
		"session/event-map.json":         func() any { return &EventMap{} },
	}
	for i := range Hops {
		d := "hops/" + hopDir(i)
		m[d+"/dispatch.json"] = func() any { return &DispatchRecord{} }
		m[d+"/wait.json"] = func() any { return &WaitRecord{} }
		m[d+"/logs.json"] = func() any { return &LogMeta{} }
	}
	return m
}

// TestRequiredFields (C5 of the round-1 review, UT-6): in every strictly
// decoded document, every field is required and only explicitly nullable
// proofs (pointer fields) may be null; a missing or null field never
// decodes to a zero value read as success.
func TestRequiredFields(t *testing.T) {
	t.Parallel()
	b := passingBundle(t)
	n := 0
	for rel, proto := range strictDocs() {
		data, err := os.ReadFile(filepath.Join(b, filepath.FromSlash(rel)))
		if err != nil {
			t.Fatal(err)
		}
		if err := decodeStrict(data, proto()); err != nil {
			t.Fatalf("%s: the passing document failed: %v", rel, err)
		}
		tree := decodeTree(t, data)
		typ := reflect.TypeOf(proto())
		for _, p := range keyPaths(tree, nil) {
			f, ok := fieldAt(typ, p)
			if !ok {
				t.Fatalf("%s%s: no schema field", rel, p)
			}
			_, omitempty := jsonField(f)
			n++
			if err := decodeStrict(mutateTree(t, tree, p, false), proto()); (err == nil) != omitempty {
				t.Errorf("%s%s omitted: decode error %v", rel, p, err)
			}
			nullable := f.Type.Kind() == reflect.Pointer
			if err := decodeStrict(mutateTree(t, tree, p, true), proto()); (err == nil) != nullable {
				t.Errorf("%s%s null: decode error %v", rel, p, err)
			}
		}
	}
	// events.jsonl: seq, type and time are required; hop, task, handle and
	// code are optional (empty means not applicable, never success).
	for _, k := range []string{"seq", "type", "time"} {
		line := []byte(`{"seq":1,"type":"run_started","time":"2026-10-10T12:00:00Z"}`)
		tree := decodeTree(t, line)
		var ev Event
		if decodeStrict(mutateTree(t, tree, keyPath{k}, false), &ev) == nil || decodeStrict(mutateTree(t, tree, keyPath{k}, true), &ev) == nil {
			t.Errorf("event %s not required", k)
		}
	}
	if n < 150 {
		t.Fatalf("only %d key paths checked", n)
	}
	// Owner-supplied coordinator exit and the run file.
	for name, doc := range map[string]struct {
		data  string
		proto func() any
	}{
		"exit code":      {`{"schema":"` + CoordExitSchema + `","session_closed":true,"mcp_child_exited":true,"wait_handles":[{"handle":"hop-01"}]}`, func() any { return &CoordinatorExit{} }},
		"null exit code": {`{"schema":"` + CoordExitSchema + `","session_closed":true,"mcp_child_exited":true,"wait_handles":[{"handle":"hop-01","exit_code":null}]}`, func() any { return &CoordinatorExit{} }},
		"null handles":   {`{"schema":"` + CoordExitSchema + `","session_closed":true,"mcp_child_exited":true,"wait_handles":null}`, func() any { return &CoordinatorExit{} }},
		"null element":   {`{"schema":"` + CoordExitSchema + `","session_closed":true,"mcp_child_exited":true,"wait_handles":[null]}`, func() any { return &CoordinatorExit{} }},
		"run file":       {`{"schema":"` + RunFileSchema + `"}`, func() any { return &RunFile{} }},
		"not an object":  {`[]`, func() any { return &RunFile{} }},
		"not an array":   {`{"schema":"` + CoordExitSchema + `","session_closed":true,"mcp_child_exited":true,"wait_handles":{}}`, func() any { return &CoordinatorExit{} }},
	} {
		if err := decodeStrict([]byte(doc.data), doc.proto()); err == nil {
			t.Errorf("%s decoded", name)
		}
	}
}

// TestSuccessZeroFields (C5): at the check command, omitting or nulling a
// proof field whose zero value means success is a schema error (exit 2);
// a nulled nullable proof fails its criterion (exit 1). Never PASS.
func TestSuccessZeroFields(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		rel  string
		path keyPath
	}{
		{"validation/final-test.json", keyPath{"exit_code"}},
		{"validation/final-test.json", keyPath{"timed_out"}},
		{"validation/final-test.json", keyPath{"signal"}},
		{"validation/final-test.json", keyPath{"duration_ms"}},
		{"validation/baseline.json", keyPath{"exit_code"}},
		{"cleanup.json", keyPath{"owner", "wait_handles", 0, "exit_code"}},
		{"cleanup.json", keyPath{"unsettled_tasks"}},
		{"session/setup-attestation.json", keyPath{"resumed"}},
		{"session/event-map.json", keyPath{"events", 0, "user_message_between"}},
		{"session/event-map.json", keyPath{"events", 0, "budget_ms"}},
		{"hops/02/wait.json", keyPath{"exit_code"}},
		{"cleanup.json", keyPath{"owner"}},
		{"setup/roles.json", keyPath{"roles", 0, "can_accept"}},
	} {
		for _, null := range []bool{false, true} {
			name := fmt.Sprintf("%s%s null=%v", c.rel, c.path, null)
			b := passingBundle(t)
			p := filepath.Join(b, filepath.FromSlash(c.rel))
			data, _ := os.ReadFile(p)
			os.WriteFile(p, mutateTree(t, decodeTree(t, data), c.path, null), 0o600)
			code, r, _ := checkBundle(t, b)
			f, _ := fieldAt(reflect.TypeOf(strictDocs()[c.rel]()), c.path)
			want := 2
			if null && f.Type.Kind() == reflect.Pointer {
				want = 1
			}
			if code != want || r.Result == ResultPass {
				t.Errorf("%s = exit %d %s, want %d", name, code, r.Result, want)
			}
		}
	}
}

// TestProvenanceInventories (C6 of the round-1 review): the manifest's
// source-template and rendered inventories must be exact and unique; each
// individual omission or duplicate fails, as does a missing role manual,
// also when the inventories are reduced to match it.
func TestProvenanceInventories(t *testing.T) {
	t.Parallel()
	for i := range TemplateNames() {
		for _, dup := range []bool{false, true} {
			b := passingBundle(t)
			editDoc(t, b, "manifest.json", func(m *Manifest) {
				if dup {
					m.Templates = append(m.Templates, m.Templates[i])
				} else {
					m.Templates = append(m.Templates[:i:i], m.Templates[i+1:]...)
				}
			})
			code, r, _ := checkBundle(t, b)
			requireCode(t, fmt.Sprintf("template %d dup=%v", i, dup), code, r, CritManifest, "template_inventory_mismatch")
		}
	}
	for i := range renderedInventory() {
		for _, dup := range []bool{false, true} {
			b := passingBundle(t)
			editDoc(t, b, "manifest.json", func(m *Manifest) {
				if dup {
					m.Rendered = append(m.Rendered, m.Rendered[i])
				} else {
					m.Rendered = append(m.Rendered[:i:i], m.Rendered[i+1:]...)
				}
			})
			code, r, _ := checkBundle(t, b)
			requireCode(t, fmt.Sprintf("rendered %d dup=%v", i, dup), code, r, CritManifest, "rendered_inventory_mismatch")
		}
	}
	for _, f := range manualFiles() {
		b := passingBundle(t)
		removeBundleFile(t, b, f)
		code, r, _ := checkBundle(t, b)
		requireCode(t, "missing "+f, code, r, CritManifest, "rendered_hash_mismatch")
	}
	// The reviewer's probe: a missing manual and both lists cut to one.
	b := passingBundle(t)
	removeBundleFile(t, b, "setup/manuals/designer-instruction.md")
	editDoc(t, b, "manifest.json", func(m *Manifest) { m.Templates, m.Rendered = m.Templates[:1], m.Rendered[:1] })
	code, r, _ := checkBundle(t, b)
	requireCode(t, "reduced inventories", code, r, CritManifest, "rendered_inventory_mismatch")
	requireCode(t, "reduced inventories", code, r, CritManifest, "template_inventory_mismatch")
}

// TestRepositoryConfig (C7 of the round-1 review): the evidence
// repository's config holds only the generated minimal core settings; any
// remote, include, extension or other section, option or value is refused
// before go-git opens it (exit 2).
func TestRepositoryConfig(t *testing.T) {
	t.Parallel()
	const base = "[core]\n\tbare = true\n"
	for name, cfg := range map[string]string{
		"generated":             base,
		"with a format version": "[core]\n\trepositoryformatversion = 0\n\tbare = true\n",
	} {
		b := passingBundle(t)
		writeBundleFile(t, b, "workspace/repo.git/config", []byte(cfg))
		if code, r, errOut := checkBundle(t, b); code != 0 {
			t.Errorf("%s = %d %v %s", name, code, failedCodes(r), errOut)
		}
	}
	for name, cfg := range map[string]string{
		"remote":           base + "[remote \"origin\"]\n\turl = https://example.invalid/x.git\n",
		"include":          base + "[include]\n\tpath = /etc/gitconfig\n",
		"includeIf":        base + "[includeIf \"gitdir:/x\"]\n\tpath = /x\n",
		"extensions":       "[core]\n\trepositoryformatversion = 1\n\tbare = true\n[extensions]\n\tobjectformat = sha256\n",
		"format version 1": "[core]\n\trepositoryformatversion = 1\n\tbare = true\n",
		"worktree":         base + "\tworktree = /elsewhere\n",
		"hooks path":       base + "\thooksPath = /x\n",
		"not bare":         "[core]\n\tbare = false\n",
		"no bare":          "[core]\n\trepositoryformatversion = 0\n",
		"repeated bare":    base + "\tbare = true\n",
		"core subsection":  "[core \"x\"]\n\tbare = true\n",
		"empty":            "",
		"malformed":        "[core\n",
		"oversized":        base + "#" + strings.Repeat("x", maxRepoConfig) + "\n",
	} {
		b := passingBundle(t)
		writeBundleFile(t, b, "workspace/repo.git/config", []byte(cfg))
		if code, r, _ := checkBundle(t, b); code != 2 || r.Error != "evidence_schema_error" {
			t.Errorf("%s = %d %+v", name, code, r.Error)
		}
	}
	b := passingBundle(t)
	removeBundleFile(t, b, "workspace/repo.git/config")
	if code, _, _ := checkBundle(t, b); code != 2 {
		t.Errorf("missing config = %d", code)
	}
	if got := contractSafe("a\x01b" + strings.Repeat("c", 40)); len(got) != 32 || got[1] != '?' {
		t.Errorf("contractSafe = %q", got)
	}
}
