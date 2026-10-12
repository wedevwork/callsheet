package reale2e

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"unicode/utf8"
)

// verdictOf is a check command's complete verdict: exit, result, schema
// error and every failing criterion with all its codes, in report order.
func verdictOf(code int, r Report) string {
	parts := []string{fmt.Sprint(code), r.Result}
	if r.Error != "" {
		parts = append(parts, "error="+r.Error)
	}
	for _, c := range r.Criteria {
		if c.Result != "pass" {
			parts = append(parts, c.ID+"="+strings.Join(c.Codes, ","))
		}
	}
	return strings.Join(parts, " ")
}

// TestCheckerVerdictsRecorded (round 5 differential): the checker's
// complete verdict on every mutation case equals the verdict recorded from
// the checker before its per-check memoisation, so the optimised path
// cannot drift from the original one on any case, code or criterion.
func TestCheckerVerdictsRecorded(t *testing.T) {
	resetMutationChecks()
	t.Parallel()
	ms := checkerMutations()
	if len(ms) != len(recordedVerdicts) {
		t.Errorf("%d mutation cases, %d recorded verdicts", len(ms), len(recordedVerdicts))
	}
	for i, m := range ms {
		t.Run(m.name, func(t *testing.T) {
			t.Parallel()
			code, r, _ := checkMutation(t, i)
			got := verdictOf(code, r)
			if i >= len(recordedVerdicts) || recordedVerdicts[i] != [2]string{m.name, got} {
				t.Errorf("case %d: verdict diverged from the recorded one; got\n\t{%q, %q},", i, m.name, got)
			}
		})
	}
}

// referenceChecker is a checker of b without a memo: every criterion,
// audit row, wait reparse and stream decode is computed where it is used.
func referenceChecker(b *Bundle) *checker {
	c := &checker{b: b, codes: map[string][]string{}}
	c.loadHops()
	c.derived = DeriveSession(sessionInputOf(b))
	return c
}

// referenceAudit is the audit derived on a fresh unmemoised checker.
func referenceAudit(b *Bundle, r Report) Audit {
	data, _ := encodeJSON(r)
	return Audit{Schema: AuditSchema, RunID: r.RunID, ReportSHA256: sha256Hex(data), Scope: AuditScope, Mechanical: r.Result, ResultReview: PendingReview,
		Sources: slices.Clone(r.Artifacts), Rows: auditRows(referenceChecker(b))}
}

// referenceCheck is the deliberately simple check: every criterion method
// in turn on one unmemoised checker (resultCorrectness deriving its own
// audit rows), then the stored derived outputs compared with a report and
// an audit recomputed from scratch. It returns the criteria and the rows
// of that recomputed audit.
func referenceCheck(b *Bundle) ([]Criterion, []AuditRow) {
	c := referenceChecker(b)
	for _, f := range criterionMethods {
		f(c)
	}
	out := c.criteria()
	r := BuildReport(b, out)
	au := referenceAudit(b, r)
	if stored, ok := b.Files["report.json"]; ok {
		data, _ := encodeJSON(r)
		want, _ := encodeJSON(au)
		aj, ok := b.Files["audit.json"]
		if !bytes.Equal(stored, data) || !bytes.Equal(b.Files["report.txt"], []byte(RenderText(r))) || !ok || !bytes.Equal(aj, want) ||
			!bytes.Equal(b.Files["audit.md"], []byte(RenderAudit(au))) {
			c.fail(CritResult, CodeDerivedMismatch)
			out = c.criteria()
		}
	}
	return out, au.Rows
}

// TestCheckerReferencePath (round 5 differential): on the passing bundle
// and one mutation per criterion, the memoised check equals the reference
// path's criteria, report and audit rows, byte for byte.
func TestCheckerReferencePath(t *testing.T) {
	t.Parallel()
	cases := []mutation{{name: "passing", mutate: func(*testing.T, string) {}}}
	seen := map[string]bool{}
	for _, m := range checkerMutations() {
		if !seen[m.crit] {
			seen[m.crit] = true
			cases = append(cases, m)
		}
	}
	for _, m := range cases {
		t.Run(m.name, func(t *testing.T) {
			t.Parallel()
			dir := passingBundle(t)
			m.mutate(t, dir)
			b, err := LoadBundle(dir)
			if err != nil {
				t.Fatal(err)
			}
			cs, c := check(b)
			want, rows := referenceCheck(b)
			if !reflect.DeepEqual(cs, want) {
				t.Errorf("criteria %v, reference %v", cs, want)
			}
			r, wr := BuildReport(b, cs), BuildReport(b, want)
			if !bytes.Equal(mustJSON(r), mustJSON(wr)) || RenderText(r) != RenderText(wr) {
				t.Errorf("report differs from the reference")
			}
			au, ra := buildAudit(c, r), buildAudit(c, r)
			ra.Rows = rows
			if got, ref := mustJSON(au), mustJSON(ra); !bytes.Equal(got, ref) {
				t.Errorf("audit differs from the reference:\n%s\n%s", got, ref)
			}
			if RenderAudit(au) != RenderAudit(ra) {
				t.Errorf("audit markdown differs from the reference")
			}
		})
	}
}

// TestDuplicateKeyFastPath (round 5 differential): the duplicate-key
// check's fast acceptance never changes a result: on every JSON document
// and line of the passing bundle and on crafted edge cases it returns
// exactly what the token walk returns.
func TestDuplicateKeyFastPath(t *testing.T) {
	t.Parallel()
	many := func(dup string) string {
		var b strings.Builder
		b.WriteString("{")
		for i := 0; i < 40; i++ {
			fmt.Fprintf(&b, `"k%d":%d,`, i, i)
		}
		return b.String() + `"` + dup + `":0}`
	}
	inputs := []string{
		`{}`, `[]`, `1`, `"s"`, ` {"a":1} `, "{\"a\":1}\n", `{"a":1,"b":{"a":2}}`, `[{"a":1},{"a":2}]`,
		`{"a":1,"a":2}`, `{"a":{"b":1,"b":2}}`, `[{"a":1,"a":1}]`, `{"a":1,"\u0061":2}`, `{"\u0061":1}`, `{"a\"b":1,"a\"b":2}`,
		`{"a":"},{\"a\":","b":"[","c":"\\"}`, `{"a":[{"a":1}],"b":[1,{"a":[]}]}`, many("k39"), many("k40"), many("new"),
		``, `{`, `{"a" 1}`, `{"a":1}{}`, `{"a":1,}`, `[1,]`, `{"a":1}x`, `nul`, "{\"a\":\"\xff\"}", "{\"\xfe\":1,\"\xff\":2}",
	}
	// Every whole JSON document of the bundle takes the fast path.
	var docs []string
	b := passingBundle(t)
	err := filepath.WalkDir(b, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(p, ".json") && !strings.HasSuffix(p, ".jsonl") {
			return err
		}
		data, err := os.ReadFile(p)
		if strings.HasSuffix(p, ".json") {
			docs = append(docs, string(data))
		}
		inputs = append(inputs, strings.Split(string(data), "\n")...)
		return err
	})
	if err != nil || len(docs) < 20 {
		t.Fatalf("%d documents: %v", len(docs), err)
	}
	for _, in := range append(inputs, docs...) {
		got, want := checkDuplicateKeys([]byte(in)), walkDuplicateKeys([]byte(in))
		if fmt.Sprint(got) != fmt.Sprint(want) {
			t.Errorf("%.80q: fast path %v, walk %v", in, got, want)
		}
	}
	for _, d := range docs {
		if !utf8.ValidString(d) || !json.Valid([]byte(d)) || !rawKeysUnique([]byte(d)) {
			t.Errorf("%.80q: not accepted by the fast path", d)
		}
	}
}

// TestPresenceFastPath: fieldsPresent, decodeStrict's walk over the raw
// bytes, never accepts what requireFields refuses, and accepts what it
// accepts unless a key is escaped. Inputs: every strictly decoded document
// of the bundle, each key omitted, nulled or given another kind, and
// crafted documents.
func TestPresenceFastPath(t *testing.T) {
	t.Parallel()
	// agree compares the verdicts on data, whose generic decoding is tree.
	agree := func(t *testing.T, data []byte, tree any, typ reflect.Type) {
		t.Helper()
		got, want := fieldsPresent(data, typ), requireFields(tree, typ, "")
		if got && want != nil || !got && want == nil && !bytes.Contains(data, []byte(`\`)) {
			t.Errorf("%.120q: fast path %v, requireFields %v", data, got, want)
		}
	}
	type inner struct {
		A string `json:"a"`
		B *int   `json:"b"`
	}
	type outer struct {
		X  inner    `json:"x"`
		Y  []inner  `json:"y"`
		Z  []*inner `json:"z,omitempty"`
		W  [2]int   `json:"w"`
		M  map[string]inner
		_  int
		No int `json:"-"`
	}
	typ := reflect.TypeOf(&outer{})
	for _, in := range []string{
		`{"x":{"a":"","b":1},"y":[],"w":[1,2],"M":{}}`,
		` { "x" : { "a" : "" , "b" : null } , "y" : [ { "a":"}","b":0 } ] , "w" : [1,2] , "M" : {"k":{}} } `,
		`{"x":{"a":"","b":1},"y":[],"z":[null,{"a":"x","b":2}],"w":[1,2],"M":null,"extra":{"a":[1,{"b":[]}]}}`,
		`{"x":{"a":"","b":1},"y":[],"w":[1,2]}`, `{"x":{"a":""},"y":[],"w":[1,2],"M":{}}`, `{"x":{"a":null,"b":1},"y":[],"w":[1,2],"M":{}}`,
		`{"x":null,"y":[],"w":[1,2],"M":{}}`, `{"x":{"a":"","b":1},"y":[null],"w":[1,2],"M":{}}`, `{"x":{"a":"","b":1},"y":{},"w":[1,2],"M":{}}`,
		`{"x":[],"y":[],"w":[1,2],"M":{}}`, `{"x":{"a":"","b":1},"y":[{"a":""}],"w":[1,2],"M":{}}`, `{"x":{"a":"","b":1},"y":[],"w":"ab","M":{}}`,
		`{"X":{"a":"","b":1},"y":[],"w":[1,2],"M":{}}`, `{"\u0078":{"a":"","b":1},"y":[],"w":[1,2],"M":{}}`, `{"x":{"a":"","b":1},"y":[],"w":[1,2],"M":{},"No":1}`,
		`null`, `[]`, `"x"`, `1e400`, `{}`,
	} {
		agree(t, []byte(in), decodeTree(t, []byte(in)), typ)
	}
	// mutate omits (absent) or replaces the value at p in tree, compares
	// the verdicts and restores tree.
	absent := new(int)
	mutate := func(t *testing.T, tree any, p keyPath, v any, typ reflect.Type) {
		t.Helper()
		cur := tree
		for _, k := range p[:len(p)-1] {
			if s, ok := k.(string); ok {
				cur = cur.(map[string]any)[s]
			} else {
				cur = cur.([]any)[k.(int)]
			}
		}
		m, key := cur.(map[string]any), p[len(p)-1].(string)
		old := m[key]
		if v == absent {
			delete(m, key)
		} else {
			m[key] = v
		}
		data, err := json.Marshal(tree)
		if err != nil {
			t.Fatal(err)
		}
		agree(t, data, tree, typ)
		m[key] = old
	}
	b := passingBundle(t)
	for rel, proto := range strictDocs() {
		t.Run(rel, func(t *testing.T) {
			t.Parallel()
			data, err := os.ReadFile(filepath.Join(b, filepath.FromSlash(rel)))
			if err != nil {
				t.Fatal(err)
			}
			typ := reflect.TypeOf(proto())
			if !fieldsPresent(data, typ) {
				t.Errorf("%s: the passing document is not accepted by the fast path", rel)
			}
			tree := decodeTree(t, data)
			for _, p := range keyPaths(tree, nil) {
				for _, v := range []any{absent, nil, "x", []any{nil}, map[string]any{}} {
					mutate(t, tree, p, v, typ)
				}
			}
		})
	}
}

// referenceDecodeStrict is decodeStrict as it was before its decode moved
// ahead of the duplicate-key check and its presence check gained a fast
// path.
func referenceDecodeStrict(data []byte, v any) error {
	if !utf8.Valid(data) {
		return errors.New("JSON is not valid UTF-8")
	}
	if err := walkDuplicateKeys(data); err != nil {
		return err
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return fmt.Errorf("invalid JSON: %w", err)
	}
	if _, err := dec.Token(); err != io.EOF {
		return errors.New("invalid JSON: data after the value")
	}
	var tree any
	gd := json.NewDecoder(bytes.NewReader(data))
	gd.UseNumber()
	if err := gd.Decode(&tree); err != nil {
		return fmt.Errorf("invalid JSON: %w", err)
	}
	return requireFields(tree, reflect.TypeOf(v), "")
}

// TestStrictDecodeReference: decodeStrict returns referenceDecodeStrict's
// error and value, and decodeLines refuses exactly the lines the original
// order refused, on every strictly decoded document and JSON line of the
// bundle and on variants with trailing data, truncation, duplicate,
// escaped or unknown keys and invalid UTF-8.
func TestStrictDecodeReference(t *testing.T) {
	t.Parallel()
	b := passingBundle(t)
	variants := func(doc string) []string {
		head, rest, _ := strings.Cut(doc, "{")
		return []string{
			doc, doc + "x", doc + " {}", doc + "\n\n", doc[:len(doc)/2], doc[:len(doc)-2],
			head + `{"schema":"x",` + rest, head + `{"zz_unknown":1,` + rest, head + `{"zz_unknown":1,"schema":"x",` + rest,
			head + `{"\u0073chema":"x",` + rest, head + `{"zz":1,"zz":2,` + rest[:len(rest)/2], head + "{\"zz\":\"\xff\"," + rest,
			"null", "[]", "", "{}",
		}
	}
	same := func(t *testing.T, in string, proto func() any) {
		t.Helper()
		got, want := proto(), proto()
		gerr, werr := decodeStrict([]byte(in), got), referenceDecodeStrict([]byte(in), want)
		if fmt.Sprint(gerr) != fmt.Sprint(werr) || gerr == nil && !reflect.DeepEqual(got, want) {
			t.Errorf("%.120q: decodeStrict %v, reference %v", in, gerr, werr)
		}
	}
	for rel, proto := range strictDocs() {
		data, err := os.ReadFile(filepath.Join(b, filepath.FromSlash(rel)))
		if err != nil {
			t.Fatal(err)
		}
		for _, in := range variants(string(data)) {
			same(t, in, proto)
		}
	}
	events, err := os.ReadFile(filepath.Join(b, "events.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	for _, l := range strings.Split(strings.TrimSpace(string(events)), "\n")[:5] {
		for _, in := range variants(l) {
			same(t, in, func() any { return &Event{} })
		}
	}
	// decodeLines: a line is refused when it is oversized, not UTF-8,
	// repeats a key, is not one JSON object or is null.
	var lines []string
	err = filepath.WalkDir(b, func(p string, d fs.DirEntry, err error) error {
		if err == nil && !d.IsDir() && strings.HasSuffix(p, ".jsonl") {
			data, rerr := os.ReadFile(p)
			for _, l := range strings.Split(string(data), "\n") {
				if l != "" {
					lines = append(lines, variants(l)...)
				}
			}
			return rerr
		}
		return err
	})
	if err != nil || len(lines) < 100 {
		t.Fatalf("%d lines: %v", len(lines), err)
	}
	lines = append(lines, `{"type":"x","type":"y"}`, `{"a":{"b":1,"b":2}}`, `[1]`, `1`, `"s"`, "{\"a\":1}"+strings.Repeat(" ", maxSessionRecordSize))
	for _, l := range lines {
		if l == "" || strings.Contains(l, "\n") {
			continue
		}
		var raw map[string]json.RawMessage
		refused := len(l) > maxSessionRecordSize || !utf8.ValidString(l) || walkDuplicateKeys([]byte(l)) != nil ||
			json.Unmarshal([]byte(l), &raw) != nil || raw == nil
		d := &deriver{codes: map[string]bool{}}
		if got := len(d.decodeLines("f", []byte(l))) == 0; got != refused {
			t.Errorf("%.120q: decodeLines refused %v, original %v", l, got, refused)
		}
	}
}

// recordedVerdicts are checkerMutations()'s verdicts, in order, recorded
// from the unmemoised checker.
var recordedVerdicts = [][2]string{
	{"manifest missing", "1 FAIL manifest=manifest_missing preflight=preflight_binding_mismatch event_map=dispatch_identity_mismatch,event_map_binding_mismatch,event_map_incomplete,event_map_mismatch hops=hop_binding_mismatch chain=tree_manifest_missing owner=decision_binding_mismatch result_correctness=audit_row_not_correct,derived_report_mismatch owner_environment=protect_depth_mismatch,protected_roots_mismatch"},
	{"run ID", "1 FAIL manifest=run_id_invalid preflight=preflight_binding_mismatch event_map=dispatch_identity_mismatch,event_map_incomplete,event_map_mismatch hops=hop_binding_mismatch owner=decision_binding_mismatch result_correctness=audit_row_not_correct,derived_report_mismatch"},
	{"platform", "1 FAIL manifest=platform_not_linux result_correctness=derived_report_mismatch"},
	{"gate", "1 FAIL manifest=gate_not_passed result_correctness=derived_report_mismatch"},
	{"revision", "1 FAIL manifest=revision_missing result_correctness=derived_report_mismatch"},
	{"version eligibility", "1 FAIL manifest=version_not_eligible result_correctness=derived_report_mismatch"},
	{"versions", "1 FAIL manifest=versions_incomplete result_correctness=derived_report_mismatch"},
	{"flow missing", "1 FAIL manifest=flow_missing,rendered_hash_mismatch roles=roles_missing preflight=preflight_selection_mismatch hops=selection_mismatch result_correctness=audit_row_not_correct,derived_report_mismatch"},
	{"flow kind", "1 FAIL manifest=flow_mismatch result_correctness=derived_report_mismatch"},
	{"workspace", "1 FAIL manifest=workspace_invalid chain=workspace_instance_mismatch result_correctness=audit_row_not_correct,derived_report_mismatch"},
	{"templates", "1 FAIL manifest=template_hashes_missing,template_inventory_mismatch result_correctness=derived_report_mismatch"},
	{"rendered", "1 FAIL manifest=rendered_hash_mismatch setup=launch_shape_invalid result_correctness=audit_row_not_correct,derived_report_mismatch"},
	{"started", "1 FAIL manifest=started_at_invalid result_correctness=derived_report_mismatch"},
	{"launch hash", "1 FAIL manifest=launch_hash_mismatch result_correctness=derived_report_mismatch"},
	{"launch inventory short", "1 FAIL manifest=launch_inventory_mismatch result_correctness=derived_report_mismatch"},
	{"launch inventory order", "1 FAIL manifest=launch_inventory_mismatch result_correctness=derived_report_mismatch"},
	{"events missing", "1 FAIL events=events_missing event_map=dispatch_identity_mismatch,event_map_incomplete,event_map_mismatch,phase_result_early,wait_command_mismatch waits=automatic_wake_unobserved,wait_unobserved owner=decision_event_mismatch admissions=admissions_mismatch final_tests=validation_binding_mismatch result_correctness=audit_row_not_correct,derived_report_mismatch deadlines=attempt_deadline_exceeded"},
	{"sequence", "1 FAIL events=lifecycle_out_of_order,sequence_not_increasing result_correctness=derived_report_mismatch"},
	{"time", "1 FAIL events=time_invalid result_correctness=derived_report_mismatch"},
	{"monotonic", "1 FAIL events=time_not_monotonic result_correctness=derived_report_mismatch"},
	{"first", "1 FAIL events=run_started_not_first result_correctness=derived_report_mismatch"},
	{"required", "1 FAIL events=required_event_missing event_map=dispatch_not_observed,event_map_mismatch owner=decision_event_mismatch final_tests=validation_binding_mismatch result_correctness=audit_row_not_correct,derived_report_mismatch"},
	{"failed attempt", "1 FAIL events=attempt_failed result_correctness=derived_report_mismatch"},
	{"roles missing", "1 FAIL roles=roles_missing result_correctness=audit_row_not_correct,derived_report_mismatch"},
	{"role pair", "1 FAIL roles=roles_mismatch result_correctness=audit_row_not_correct,derived_report_mismatch"},
	{"role timeout", "1 FAIL roles=roles_mismatch result_correctness=audit_row_not_correct,derived_report_mismatch"},
	{"role ready", "1 FAIL roles=role_not_ready result_correctness=audit_row_not_correct,derived_report_mismatch"},
	{"role nodes", "1 FAIL roles=adapter_nodes_inconsistent result_correctness=audit_row_not_correct,derived_report_mismatch"},
	{"ready time", "1 FAIL roles=ready_at_invalid result_correctness=audit_row_not_correct,derived_report_mismatch"},
	{"preflight missing", "1 FAIL preflight=preflight_missing admissions=admissions_mismatch result_correctness=audit_row_not_correct,derived_report_mismatch"},
	{"preflight marker", "1 FAIL preflight=preflight_binding_mismatch result_correctness=audit_row_not_correct,derived_report_mismatch"},
	{"preflight role", "1 FAIL preflight=preflight_binding_mismatch result_correctness=audit_row_not_correct,derived_report_mismatch"},
	{"preflight pair", "1 FAIL preflight=preflight_selection_mismatch result_correctness=audit_row_not_correct,derived_report_mismatch"},
	{"preflight answer", "1 FAIL preflight=preflight_not_authenticated result_correctness=audit_row_not_correct,derived_report_mismatch"},
	{"preflight truncated", "1 FAIL preflight=preflight_not_authenticated result_correctness=audit_row_not_correct,derived_report_mismatch"},
	{"preflight final file", "1 FAIL preflight=preflight_not_authenticated result_correctness=audit_row_not_correct,derived_report_mismatch"},
	{"launch missing", "1 FAIL manifest=launch_hash_mismatch,launch_inventory_mismatch setup=launch_missing event_map=coordinator_user_input,event_map_mismatch result_correctness=audit_row_not_correct,derived_report_mismatch"},
	{"permission bypass", "1 FAIL manifest=launch_hash_mismatch setup=launch_shape_invalid result_correctness=audit_row_not_correct,derived_report_mismatch"},
	{"permission mode", "1 FAIL manifest=launch_hash_mismatch setup=launch_shape_invalid result_correctness=audit_row_not_correct,derived_report_mismatch"},
	{"strict MCP", "1 FAIL manifest=launch_hash_mismatch setup=launch_shape_invalid result_correctness=audit_row_not_correct,derived_report_mismatch"},
	{"setting sources", "1 FAIL manifest=launch_hash_mismatch setup=launch_shape_invalid result_correctness=audit_row_not_correct,derived_report_mismatch"},
	{"session pinned", "1 FAIL manifest=launch_hash_mismatch setup=launch_shape_invalid result_correctness=audit_row_not_correct,derived_report_mismatch"},
	{"sandbox kept on", "1 FAIL manifest=launch_hash_mismatch setup=launch_shape_invalid result_correctness=audit_row_not_correct,derived_report_mismatch"},
	{"phase 1 resumed", "1 FAIL manifest=launch_hash_mismatch setup=launch_shape_invalid result_correctness=audit_row_not_correct,derived_report_mismatch"},
	{"phase 2 continue", "1 FAIL manifest=launch_hash_mismatch setup=launch_shape_invalid result_correctness=audit_row_not_correct,derived_report_mismatch"},
	{"phase 2 other session", "1 FAIL manifest=launch_hash_mismatch setup=launch_shape_invalid,session_resume_mismatch result_correctness=audit_row_not_correct,derived_report_mismatch"},
	{"prompt hash", "1 FAIL manifest=launch_hash_mismatch setup=launch_shape_invalid result_correctness=derived_report_mismatch"},
	{"MCP config", "1 FAIL manifest=rendered_hash_mismatch setup=setup_file_missing result_correctness=derived_report_mismatch"},
	{"coordinator exit missing", "1 FAIL setup=coordinator_exit_unobserved result_correctness=audit_row_not_correct,derived_report_mismatch cleanup=coordinator_group_unverified"},
	{"coordinator exit nonzero", "1 FAIL setup=coordinator_exit_nonzero result_correctness=audit_row_not_correct,derived_report_mismatch"},
	{"phase 2 new session", "1 FAIL setup=session_resume_mismatch event_map=event_map_incomplete,event_map_mismatch,session_mismatch result_correctness=audit_row_not_correct,derived_report_mismatch"},
	{"map missing", "1 FAIL event_map=event_map_missing result_correctness=derived_report_mismatch"},
	{"map edited", "1 FAIL event_map=event_map_mismatch result_correctness=derived_report_mismatch"},
	{"map attested", "1 FAIL event_map=event_map_mismatch,event_map_not_derived result_correctness=derived_report_mismatch"},
	{"transcripts missing", "1 FAIL event_map=event_map_incomplete,event_map_mismatch,session_evidence_unobserved waits=automatic_wake_unobserved,wait_unobserved result_correctness=audit_row_not_correct,derived_report_mismatch"},
	{"transcript not a prefix", "1 FAIL event_map=event_map_incomplete,event_map_mismatch,session_evidence_unobserved waits=automatic_wake_unobserved,wait_unobserved result_correctness=audit_row_not_correct,derived_report_mismatch"},
	{"malformed record", "1 FAIL event_map=event_map_mismatch,session_evidence_malformed result_correctness=audit_row_not_correct,derived_report_mismatch"},
	{"dangling tool result", "1 FAIL event_map=event_map_mismatch,session_evidence_malformed result_correctness=audit_row_not_correct,derived_report_mismatch"},
	{"ordinary user input", "1 FAIL event_map=coordinator_user_input,event_map_mismatch result_correctness=audit_row_not_correct,derived_report_mismatch"},
	{"unsupported wake", "1 FAIL event_map=event_map_mismatch,wake_representation_unsupported waits=automatic_wake_unobserved result_correctness=audit_row_not_correct,derived_report_mismatch"},
	{"result unbound", "1 FAIL event_map=event_map_incomplete,event_map_mismatch,session_evidence_malformed result_correctness=audit_row_not_correct,derived_report_mismatch"},
	{"result first", "1 FAIL event_map=event_map_mismatch,phase_result_early,session_evidence_malformed result_correctness=audit_row_not_correct,derived_report_mismatch"},
	{"task_show response forged", "1 FAIL event_map=event_map_incomplete,event_map_mismatch,phase_result_early waits=automatic_wake_unobserved,wait_unobserved result_correctness=audit_row_not_correct,derived_report_mismatch"},
	{"denied tool", "1 FAIL event_map=coordinator_tool_denied,event_map_mismatch result_correctness=audit_row_not_correct,derived_report_mismatch"},
	{"wait missing", "1 FAIL waits=wait_record_missing result_correctness=audit_row_not_correct,derived_report_mismatch"},
	{"wait files missing", "1 FAIL waits=wait_files_missing result_correctness=audit_row_not_correct,derived_report_mismatch"},
	{"wait winner", "1 FAIL waits=wait_binding_mismatch,wait_record_mismatch result_correctness=audit_row_not_correct,derived_report_mismatch"},
	{"wait exit", "1 FAIL waits=wait_record_mismatch,wait_unsuccessful result_correctness=audit_row_not_correct,derived_report_mismatch"},
	{"wait killed", "1 FAIL waits=wait_record_mismatch,wait_unsuccessful result_correctness=audit_row_not_correct,derived_report_mismatch"},
	{"wait raw output", "1 FAIL waits=wait_record_mismatch result_correctness=audit_row_not_correct,derived_report_mismatch"},
	{"long poll", "1 FAIL event_map=coordinator_long_poll,event_map_mismatch waits=coordinator_long_poll result_correctness=audit_row_not_correct,derived_report_mismatch"},
	{"no wake", "1 FAIL event_map=coordinator_user_input,event_map_mismatch waits=automatic_wake_unobserved result_correctness=audit_row_not_correct,derived_report_mismatch"},
	{"hop missing", "1 FAIL event_map=event_map_incomplete waits=wait_binding_mismatch hops=hop_missing chain=result_commit_missing admissions=admissions_mismatch grok_review=review_missing result_correctness=audit_row_not_correct,derived_report_mismatch"},
	{"duplicate task", "1 FAIL event_map=event_map_incomplete waits=wait_binding_mismatch hops=dispatch_record_mismatch,task_not_unique chain=tree_manifest_mismatch owner=decision_binding_mismatch admissions=admissions_mismatch result_correctness=audit_row_not_correct,derived_report_mismatch"},
	{"hop marker", "1 FAIL hops=dispatch_record_mismatch,hop_binding_mismatch result_correctness=audit_row_not_correct,derived_report_mismatch"},
	{"hop pair", "1 FAIL hops=dispatch_record_mismatch,selection_mismatch result_correctness=audit_row_not_correct,derived_report_mismatch"},
	{"hop override", "1 FAIL hops=selection_mismatch result_correctness=audit_row_not_correct,derived_report_mismatch"},
	{"hop failed", "1 FAIL hops=hop_not_succeeded result_correctness=audit_row_not_correct,derived_report_mismatch"},
	{"hop marker line", "1 FAIL hops=final_marker_missing result_correctness=audit_row_not_correct,derived_report_mismatch"},
	{"hop publication", "1 FAIL hops=publication_missing chain=result_commit_mismatch owner=decision_binding_mismatch result_correctness=audit_row_not_correct,derived_report_mismatch"},
	{"hop logs", "1 FAIL hops=logs_missing result_correctness=derived_report_mismatch"},
	{"dispatch missing", "1 FAIL event_map=dispatch_not_observed hops=dispatch_record_missing result_correctness=audit_row_not_correct,derived_report_mismatch"},
	{"dispatch base", "1 FAIL hops=dispatch_record_mismatch result_correctness=audit_row_not_correct,derived_report_mismatch"},
	{"repository missing", "1 FAIL chain=repository_missing artifacts=seed_tree_missing owner=decision_digest_mismatch grok_review=review_missing final_tests=baseline_failed,final_tests_failed,validation_binding_mismatch result_correctness=audit_row_not_correct,derived_report_mismatch,requirements_mismatch,test_mapping_missing"},
	{"repository corrupt", "1 FAIL chain=repository_invalid,repository_missing artifacts=seed_tree_missing owner=decision_digest_mismatch grok_review=review_missing final_tests=baseline_failed,final_tests_failed,validation_binding_mismatch result_correctness=audit_row_not_correct,derived_report_mismatch,requirements_mismatch,test_mapping_missing"},
	{"tree manifest missing", "1 FAIL chain=tree_manifest_missing result_correctness=derived_report_mismatch"},
	{"seed", "1 FAIL chain=seed_mismatch result_correctness=derived_report_mismatch"},
	{"tree manifest hops", "1 FAIL chain=tree_manifest_incomplete result_correctness=derived_report_mismatch"},
	{"tree manifest tree", "1 FAIL chain=tree_manifest_mismatch result_correctness=derived_report_mismatch"},
	{"instance", "1 FAIL hops=dispatch_record_mismatch chain=workspace_instance_mismatch result_correctness=audit_row_not_correct,derived_report_mismatch"},
	{"base", "1 FAIL hops=dispatch_record_mismatch chain=base_binding_mismatch result_correctness=audit_row_not_correct,derived_report_mismatch"},
	{"result commit", "1 FAIL chain=result_commit_mismatch owner=decision_binding_mismatch result_correctness=audit_row_not_correct,derived_report_mismatch"},
	{"parent", "1 FAIL chain=base_binding_mismatch,parent_mismatch,result_commit_mismatch,tree_manifest_mismatch result_correctness=audit_row_not_correct,derived_report_mismatch"},
	{"merge", "1 FAIL chain=base_binding_mismatch,parent_mismatch,result_commit_mismatch,tree_manifest_mismatch result_correctness=audit_row_not_correct,derived_report_mismatch"},
	{"seed files", "1 FAIL chain=seed_mismatch artifacts=disallowed_change,seed_incomplete final_tests=baseline_failed result_correctness=audit_row_not_correct,derived_report_mismatch"},
	{"seed feature", "1 FAIL chain=seed_mismatch artifacts=seed_has_feature_artifacts final_tests=baseline_failed result_correctness=audit_row_not_correct,derived_report_mismatch"},
	{"hop tree", "1 FAIL chain=result_commit_missing artifacts=hop_tree_missing grok_review=review_missing result_correctness=audit_row_not_correct,derived_report_mismatch"},
	{"designer change", "1 FAIL chain=base_binding_mismatch,parent_mismatch,result_commit_mismatch,tree_manifest_mismatch artifacts=disallowed_change result_correctness=audit_row_not_correct,derived_report_mismatch"},
	{"project size", "1 FAIL chain=base_binding_mismatch,parent_mismatch,result_commit_mismatch,tree_manifest_mismatch artifacts=disallowed_change,project_too_large result_correctness=audit_row_not_correct,derived_report_mismatch"},
	{"empty design", "1 FAIL chain=base_binding_mismatch,parent_mismatch,result_commit_mismatch,tree_manifest_mismatch artifacts=design_missing,disallowed_change result_correctness=audit_row_not_correct,derived_report_mismatch"},
	{"review verdict", "1 FAIL chain=base_binding_mismatch,parent_mismatch,result_commit_mismatch,tree_manifest_mismatch artifacts=design_review_not_approved,disallowed_change owner=decision_digest_mismatch result_correctness=audit_row_not_correct,derived_report_mismatch"},
	{"coder edits design", "1 FAIL chain=base_binding_mismatch,parent_mismatch,result_commit_mismatch,tree_manifest_mismatch artifacts=disallowed_change,reviewer_tree_changed grok_review=inline_source_mismatch result_correctness=audit_row_not_correct,derived_report_mismatch"},
	{"no implementation", "1 FAIL chain=base_binding_mismatch,parent_mismatch,result_commit_mismatch,tree_manifest_mismatch artifacts=disallowed_change,implementation_missing,reviewer_tree_changed grok_review=inline_source_mismatch result_correctness=audit_row_not_correct,derived_report_mismatch"},
	{"reviewer writes", "1 FAIL chain=result_commit_mismatch,tree_manifest_mismatch artifacts=disallowed_change,reviewer_tree_changed final_tests=final_tests_failed,validation_binding_mismatch result_correctness=audit_row_not_correct,derived_report_mismatch,requirements_mismatch"},
	{"decision missing", "1 FAIL manifest=rendered_inventory_mismatch owner=decision_missing result_correctness=audit_row_not_correct,derived_report_mismatch"},
	{"decision no", "1 FAIL manifest=rendered_inventory_mismatch owner=owner_did_not_approve result_correctness=audit_row_not_correct,derived_report_mismatch"},
	{"decision task", "1 FAIL owner=decision_binding_mismatch result_correctness=audit_row_not_correct,derived_report_mismatch"},
	{"decision commit", "1 FAIL owner=decision_binding_mismatch result_correctness=audit_row_not_correct,derived_report_mismatch"},
	{"stale digest", "1 FAIL owner=decision_digest_mismatch result_correctness=audit_row_not_correct,derived_report_mismatch"},
	{"decision event", "1 FAIL owner=decision_event_mismatch result_correctness=audit_row_not_correct,derived_report_mismatch"},
	{"two decisions", "1 FAIL events=required_event_missing event_map=dispatch_not_observed,event_map_mismatch owner=decision_event_mismatch final_tests=validation_binding_mismatch result_correctness=audit_row_not_correct,derived_report_mismatch"},
	{"decision order", "1 FAIL events=lifecycle_out_of_order,time_not_monotonic event_map=event_map_mismatch owner=decision_out_of_order result_correctness=audit_row_not_correct,derived_report_mismatch"},
	{"decision deadline", "1 FAIL events=time_not_monotonic owner=decision_deadline_exceeded result_correctness=audit_row_not_correct,derived_report_mismatch"},
	{"premature coding", "1 FAIL events=time_not_monotonic event_map=event_map_mismatch owner=coding_before_approval result_correctness=audit_row_not_correct,derived_report_mismatch"},
	{"coder created early", "1 FAIL owner=coding_before_approval result_correctness=audit_row_not_correct,derived_report_mismatch deadlines=feature_deadline_exceeded"},
	{"premature event", "1 FAIL admissions=premature_admission result_correctness=audit_row_not_correct,derived_report_mismatch"},
	{"extra event", "1 FAIL admissions=extra_admission result_correctness=derived_report_mismatch"},
	{"cancelled task", "1 FAIL admissions=extra_admission result_correctness=derived_report_mismatch"},
	{"admission count", "1 FAIL event_map=dispatch_not_observed,event_map_mismatch owner=decision_event_mismatch admissions=admissions_mismatch final_tests=validation_binding_mismatch result_correctness=audit_row_not_correct,derived_report_mismatch"},
	{"admission order", "1 FAIL admissions=admission_out_of_order result_correctness=derived_report_mismatch"},
	{"review absent", "1 FAIL hops=hop_not_succeeded grok_review=review_missing result_correctness=audit_row_not_correct,derived_report_mismatch"},
	{"review truncated", "1 FAIL hops=hop_not_succeeded grok_review=review_missing result_correctness=audit_row_not_correct,derived_report_mismatch"},
	{"review verdict", "1 FAIL hops=final_marker_missing grok_review=review_not_approved result_correctness=audit_row_not_correct,derived_report_mismatch"},
	{"inline absent", "1 FAIL grok_review=inline_files_invalid,inline_source_mismatch result_correctness=audit_row_not_correct,derived_report_mismatch"},
	{"inline traversal", "1 FAIL grok_review=inline_source_mismatch result_correctness=audit_row_not_correct,derived_report_mismatch"},
	{"inline content", "1 FAIL grok_review=inline_source_mismatch result_correctness=audit_row_not_correct,derived_report_mismatch"},
	{"inline hashes", "1 FAIL grok_review=inline_hashes_missing result_correctness=audit_row_not_correct,derived_report_mismatch"},
	{"inline record", "1 FAIL grok_review=inline_source_mismatch result_correctness=audit_row_not_correct,derived_report_mismatch"},
	{"baseline missing", "1 FAIL final_tests=baseline_missing result_correctness=audit_row_not_correct,derived_report_mismatch"},
	{"baseline failed", "1 FAIL final_tests=baseline_failed result_correctness=audit_row_not_correct,derived_report_mismatch"},
	{"final missing", "1 FAIL final_tests=final_tests_missing result_correctness=audit_row_not_correct,derived_report_mismatch,review_test_contradiction"},
	{"final failed", "1 FAIL final_tests=final_tests_failed result_correctness=audit_row_not_correct,derived_report_mismatch,review_test_contradiction"},
	{"final argv", "1 FAIL final_tests=final_tests_failed result_correctness=audit_row_not_correct,derived_report_mismatch"},
	{"final env", "1 FAIL final_tests=final_tests_failed result_correctness=audit_row_not_correct,derived_report_mismatch"},
	{"final duration", "1 FAIL final_tests=final_tests_failed result_correctness=audit_row_not_correct,derived_report_mismatch"},
	{"final commit", "1 FAIL final_tests=final_tests_failed result_correctness=audit_row_not_correct,derived_report_mismatch"},
	{"final files", "1 FAIL final_tests=final_tests_failed result_correctness=audit_row_not_correct,derived_report_mismatch"},
	{"cleanup missing", "1 FAIL result_correctness=audit_row_not_correct,derived_report_mismatch cleanup=cleanup_missing"},
	{"cleanup incomplete", "1 FAIL result_correctness=audit_row_not_correct,derived_report_mismatch cleanup=cleanup_incomplete"},
	{"unsettled", "1 FAIL result_correctness=audit_row_not_correct,derived_report_mismatch cleanup=cleanup_incomplete"},
	{"exit unverified", "1 FAIL result_correctness=audit_row_not_correct,derived_report_mismatch cleanup=process_exit_unverified"},
	{"process record", "1 FAIL result_correctness=audit_row_not_correct,derived_report_mismatch cleanup=process_record_missing"},
	{"coordinator group alive", "1 FAIL setup=coordinator_exit_nonzero result_correctness=audit_row_not_correct,derived_report_mismatch cleanup=coordinator_group_unverified"},
	{"coordinator not reaped", "1 FAIL setup=coordinator_exit_nonzero result_correctness=audit_row_not_correct,derived_report_mismatch cleanup=coordinator_group_unverified"},
	{"phase process record", "1 FAIL result_correctness=audit_row_not_correct,derived_report_mismatch cleanup=process_record_missing"},
	{"runtime kept", "1 FAIL result_correctness=audit_row_not_correct,derived_report_mismatch cleanup=runtime_not_removed"},
	{"preflight deadline", "1 FAIL result_correctness=audit_row_not_correct,derived_report_mismatch deadlines=preflight_deadline_exceeded"},
	{"feature deadline", "1 FAIL result_correctness=derived_report_mismatch deadlines=feature_deadline_exceeded"},
	{"attempt deadline", "1 FAIL result_correctness=derived_report_mismatch deadlines=attempt_deadline_exceeded"},
	{"cleanup deadline", "1 FAIL result_correctness=audit_row_not_correct,derived_report_mismatch deadlines=cleanup_deadline_exceeded"},
	{"resume before receipt", "1 FAIL events=lifecycle_out_of_order event_map=event_map_mismatch owner=decision_event_mismatch result_correctness=audit_row_not_correct,derived_report_mismatch"},
	{"diff missing", "1 FAIL result_correctness=audit_row_not_correct,derived_report_mismatch owner_environment=environment_diff_missing"},
	{"snapshot missing", "1 FAIL result_correctness=audit_row_not_correct,derived_report_mismatch owner_environment=owner_environment_unobserved"},
	{"configuration changed", "1 FAIL result_correctness=audit_row_not_correct,derived_report_mismatch owner_environment=owner_environment_modified"},
	{"configuration deleted", "1 FAIL result_correctness=audit_row_not_correct,derived_report_mismatch owner_environment=owner_environment_modified"},
	{"after incomplete", "1 FAIL result_correctness=audit_row_not_correct,derived_report_mismatch owner_environment=owner_environment_unobserved"},
	{"diff suppressed", "1 FAIL result_correctness=audit_row_not_correct,derived_report_mismatch owner_environment=environment_diff_mismatch,owner_environment_modified"},
	{"diff detail altered", "1 FAIL result_correctness=audit_row_not_correct,derived_report_mismatch owner_environment=environment_diff_mismatch,owner_environment_modified,owner_environment_unobserved"},
	{"diff detail dropped", "1 FAIL result_correctness=audit_row_not_correct,derived_report_mismatch owner_environment=environment_diff_mismatch,owner_environment_modified,owner_environment_unobserved"},
	{"snapshots empty", "1 FAIL result_correctness=audit_row_not_correct,derived_report_mismatch owner_environment=environment_inventory_incomplete,owner_environment_unobserved"},
	{"home record missing", "1 FAIL result_correctness=audit_row_not_correct,derived_report_mismatch owner_environment=environment_inventory_incomplete,owner_environment_unobserved"},
	{"depth mismatch", "1 FAIL result_correctness=audit_row_not_correct,derived_report_mismatch owner_environment=protect_depth_mismatch"},
	{"roots mismatch", "1 FAIL result_correctness=audit_row_not_correct,derived_report_mismatch owner_environment=environment_inventory_incomplete,owner_environment_unobserved,protected_roots_mismatch"},
	{"case failed", "1 FAIL result_correctness=audit_row_not_correct,derived_report_mismatch,feature_case_failed,requirements_mismatch"},
	{"case record", "1 FAIL result_correctness=audit_row_not_correct,case_record_mismatch,derived_report_mismatch,requirements_mismatch"},
	{"subtest missing", "1 FAIL final_tests=validation_binding_mismatch result_correctness=audit_row_not_correct,derived_report_mismatch,requirements_mismatch,test_mapping_missing"},
	{"requirements edited", "1 FAIL result_correctness=derived_report_mismatch,requirements_mismatch"},
	{"approval with failing tests", "1 FAIL final_tests=final_tests_failed result_correctness=audit_row_not_correct,derived_report_mismatch,review_test_contradiction"},
	{"report edited", "1 FAIL result_correctness=derived_report_mismatch"},
	{"audit row reordered", "1 FAIL result_correctness=derived_report_mismatch"},
	{"audit markdown", "1 FAIL result_correctness=derived_report_mismatch"},
	{"validation missing", "1 FAIL final_tests=validation_missing result_correctness=audit_row_not_correct,derived_report_mismatch,feature_case_failed,requirements_mismatch"},
	{"build failed", "1 FAIL final_tests=validation_binding_mismatch result_correctness=audit_row_not_correct,derived_report_mismatch"},
	{"case commit", "1 FAIL final_tests=validation_binding_mismatch result_correctness=audit_row_not_correct,derived_report_mismatch"},
	{"validation sequence", "1 FAIL final_tests=validation_binding_mismatch result_correctness=audit_row_not_correct,derived_report_mismatch"},
	{"json run failed", "1 FAIL final_tests=final_validation_failed result_correctness=audit_row_not_correct,derived_report_mismatch,review_test_contradiction"},
	{"json run approved", "1 FAIL final_tests=final_validation_failed result_correctness=audit_row_not_correct,derived_report_mismatch,review_test_contradiction"},
	{"validation budget", "1 FAIL final_tests=validation_deadline_exceeded result_correctness=audit_row_not_correct,derived_report_mismatch"},
}
