package contract

import (
	"encoding/json"
	"strconv"
	"strings"
	"testing"
	"time"
)

// testLookup is the fake adapter's metadata (the contract package cannot
// import internal/adapter, which imports it).
func testLookup(id string) (AdapterInfo, bool) {
	if id == "fake" {
		return AdapterInfo{Efforts: []string{"low", "medium", "high"}, TestOnly: true}, true
	}
	return AdapterInfo{}, false
}

// roleJSON is a complete add request with fields in table order; with
// substitutes (key → raw JSON value, or "" to omit).
func roleJSON(sub map[string]string) string {
	fields := [][2]string{
		{"id", `"worker-a"`}, {"name", `"implementer"`}, {"node", `"` + testID + `"`}, {"adapter", `"fake"`},
		{"instruction", `"/srv/manuals/instruction.md"`}, {"runbook", `"/srv/manuals/runbook.md"`},
		{"model", `"example model"`}, {"effort", `"medium"`}, {"concurrency", `2`}, {"timeout", `"90m"`},
	}
	var parts []string
	for _, f := range fields {
		v := f[1]
		if s, ok := sub[f[0]]; ok {
			if s == "" {
				continue
			}
			v = s
		}
		parts = append(parts, `"`+f[0]+`":`+v)
	}
	for k, v := range sub {
		known := false
		for _, f := range fields {
			known = known || f[0] == k
		}
		if !known {
			parts = append(parts, `"`+k+`":`+v)
		}
	}
	return "{" + strings.Join(parts, ",") + "}"
}

// jsonQuote renders s as a JSON string literal.
func jsonQuote(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

func wantField(t *testing.T, name string, err error, field, substr string) {
	t.Helper()
	ce, ok := err.(*Error)
	if !ok || ce.Code != CodeInvalidArgument || !strings.Contains(ce.Message, substr) {
		t.Fatalf("%s: err = %v, want invalid_argument containing %q", name, err, substr)
	}
	if field != "" {
		if got, _ := ce.Details["field"].(string); got != field {
			t.Fatalf("%s: field detail = %v, want %q", name, ce.Details, field)
		}
	}
}

// TestRoleConfigContract is UT FP-1: every missing, null, duplicate and
// unknown field; slug edges; UTF-8, control and blank bounds; free model
// text; effort and adapter mismatches; integer and timeout edges; patch
// absence versus zero; encoded size, count and order bounds; precedence.
func TestRoleConfigContract(t *testing.T) {
	t.Run("valid", func(t *testing.T) {
		c, err := ParseRoleConfig([]byte(roleJSON(nil)), testLookup)
		if err != nil || c.ID != "worker-a" || c.Concurrency != 2 || c.Timeout != 90*time.Minute || !c.HasTimeout || c.Model != "example model" {
			t.Fatalf("config = %+v %v", c, err)
		}
		b, _ := json.Marshal(c)
		if string(b) != `{"id":"worker-a","name":"implementer","node":"`+testID+`","adapter":"fake","instruction":"/srv/manuals/instruction.md","runbook":"/srv/manuals/runbook.md","model":"example model","effort":"medium","concurrency":2,"timeout":"1h30m0s"}` {
			t.Fatalf("encoded = %s", b)
		}
		// Omitted timeout resolves to 2h, rendered canonically.
		c, err = ParseRoleConfig([]byte(roleJSON(map[string]string{"timeout": ""})), testLookup)
		if err != nil || c.HasTimeout {
			t.Fatalf("no timeout = %+v %v", c, err)
		}
		if b, _ := json.Marshal(c); strings.Contains(string(b), "timeout") {
			t.Fatalf("omitted timeout encoded: %s", b)
		}
		r := c.Resolved()
		if r.Timeout != 2*time.Hour || !r.HasTimeout {
			t.Fatalf("resolved = %+v", r)
		}
		rec := RoleRecord{RoleConfig: c, RegistrationOrder: 7}
		if b, _ := json.Marshal(rec); !strings.HasSuffix(string(b), `"concurrency":2,"timeout":"2h0m0s","registration_order":7}`) {
			t.Fatalf("record = %s", b)
		}
		// Zero is unlimited, canonical 0s; any zero spelling is accepted.
		for _, z := range []string{`"0"`, `"0s"`, `"0h0m"`, `"-0s"`} {
			c, err := ParseRoleConfig([]byte(roleJSON(map[string]string{"timeout": z})), testLookup)
			if err != nil || c.Timeout != 0 || c.Resolved().Timeout.String() != "0s" {
				t.Fatalf("timeout %s = %+v %v", z, c, err)
			}
		}
		// The largest positive duration is accepted.
		max := time.Duration(1<<63 - 1).String()
		if c, err := ParseRoleConfig([]byte(roleJSON(map[string]string{"timeout": `"` + max + `"`})), testLookup); err != nil || c.Timeout != time.Duration(1<<63-1) {
			t.Fatalf("max timeout = %v", err)
		}
	})
	t.Run("fields", func(t *testing.T) {
		for _, f := range roleFields[:9] {
			_, err := ParseRoleConfig([]byte(roleJSON(map[string]string{f: ""})), testLookup)
			wantField(t, "missing "+f, err, f, `lacks the required field "`+f+`"`)
			_, err = ParseRoleConfig([]byte(roleJSON(map[string]string{f: "null"})), testLookup)
			wantField(t, "null "+f, err, f, "must not be null")
		}
		for _, f := range roleFields {
			bad := `1`
			if f == "concurrency" {
				bad = `"2"`
			}
			_, err := ParseRoleConfig([]byte(roleJSON(map[string]string{f: bad})), testLookup)
			wantField(t, "type "+f, err, f, "must be")
		}
		_, err := ParseRoleConfig([]byte(roleJSON(map[string]string{"timeout": "null"})), testLookup)
		wantField(t, "null timeout", err, "timeout", "must not be null")
		for name, in := range map[string]string{
			"unknown":   roleJSON(map[string]string{"workdir": `"/x"`}),
			"order key": roleJSON(map[string]string{"registration_order": `1`}),
			"duplicate": strings.Replace(roleJSON(nil), `"name":"implementer"`, `"name":"implementer","name":"x"`, 1),
			"trailing":  roleJSON(nil) + "{}",
			"array":     `[]`,
		} {
			if _, err := ParseRoleConfig([]byte(in), testLookup); CodeOf(err) != CodeInvalidArgument {
				t.Fatalf("%s accepted: %v", name, err)
			}
		}
	})
	t.Run("slugs", func(t *testing.T) {
		for s, ok := range map[string]bool{
			"a": true, "0": true, "a-b": true, "a--b": true, strings.Repeat("a", 63): true, strings.Repeat("a", 64): false,
			"": false, "-a": false, "a-": false, "A": false, "a_b": false, "a.b": false, "a/b": false, "é": false, " a": false,
		} {
			if ValidSlug(s) != ok {
				t.Errorf("ValidSlug(%q) != %v", s, ok)
			}
			for _, f := range []string{"id", "name"} {
				_, err := ParseRoleConfig([]byte(roleJSON(map[string]string{f: strconv.Quote(s)})), testLookup)
				if (err == nil) != ok {
					t.Errorf("%s %q: %v", f, s, err)
				}
			}
		}
		_, err := ParseRoleConfig([]byte(roleJSON(map[string]string{"node": `"n_1"`})), testLookup)
		wantField(t, "node", err, "node", "node ID")
	})
	t.Run("text", func(t *testing.T) {
		long := "/" + strings.Repeat("p", 1023)
		for name, c := range map[string]struct {
			field, value string
			ok           bool
		}{
			"path max":        {"instruction", long, true},
			"path over":       {"instruction", long + "p", false},
			"relative":        {"runbook", "manuals/runbook.md", false},
			"tilde":           {"runbook", "~/runbook.md", false},
			"variable":        {"runbook", "$HOME/runbook.md", false},
			"path spaces":     {"instruction", "/srv/my manuals/instruction.md", true},
			"path unicode":    {"instruction", "/srv/手册.md", true},
			"path tab":        {"instruction", "/srv/a\tb", false},
			"path newline":    {"runbook", "/srv/a\nb", false},
			"path del":        {"runbook", "/srv/a\x7fb", false},
			"path c1":         {"runbook", "/srv/a\u0085b", false},
			"path nul":        {"runbook", "/srv/a\x00b", false},
			"model free":      {"model", "gpt-6 sol (preview)", true},
			"model dash":      {"model", "-weird --model", true},
			"model inner":     {"model", " padded ", true},
			"model unicode":   {"model", "模型 ✓", true},
			"model blank":     {"model", "   ", false},
			"model empty":     {"model", "", false},
			"model control":   {"model", "a\rb", false},
			"model max":       {"model", strings.Repeat("m", 1024), true},
			"model over":      {"model", strings.Repeat("m", 1025), false},
			"model 2028 max":  {"model", strings.Repeat(" ", 341), false},
			"model html":      {"model", "<a&b>", true},
			"effort empty":    {"effort", "", false},
			"adapter empty":   {"adapter", "", false},
			"instruction rel": {"instruction", ".", false},
		} {
			_, err := ParseRoleConfig([]byte(roleJSON(map[string]string{c.field: jsonQuote(c.value)})), testLookup)
			if (err == nil) != c.ok {
				t.Errorf("%s: %v", name, err)
			}
			if err != nil && !strings.Contains(err.Error(), c.field) {
				t.Errorf("%s: error does not name %s: %v", name, c.field, err)
			}
		}
		// Invalid UTF-8 is rejected before decoding can replace it.
		raw := strings.Replace(roleJSON(nil), `"example model"`, "\"bad \xff model\"", 1)
		_, err := ParseRoleConfig([]byte(raw), testLookup)
		wantField(t, "invalid utf-8", err, "model", "UTF-8")
		if _, err := ParseRolePatch([]byte("{\"runbook\":\"/a\xfe\"}")); err == nil || !strings.Contains(err.Error(), "UTF-8") {
			t.Fatalf("patch invalid utf-8: %v", err)
		}
		// JSON escaping does not change the value: an escaped value decodes
		// to its text, which is preserved byte for byte.
		c, err := ParseRoleConfig([]byte(roleJSON(map[string]string{"model": `"模 \"x\""`})), testLookup)
		if err != nil || c.Model != "模 \"x\"" {
			t.Fatalf("escaped model = %q %v", c.Model, err)
		}
	})
	t.Run("effort", func(t *testing.T) {
		for _, e := range []string{"low", "medium", "high"} {
			if _, err := ParseRoleConfig([]byte(roleJSON(map[string]string{"effort": strconv.Quote(e)})), testLookup); err != nil {
				t.Fatalf("effort %s: %v", e, err)
			}
		}
		for _, e := range []string{"Medium", "max", " low"} {
			_, err := ParseRoleConfig([]byte(roleJSON(map[string]string{"effort": strconv.Quote(e)})), testLookup)
			wantField(t, "effort "+e, err, "effort", "not allowed for adapter fake")
		}
		_, err := ParseRoleConfig([]byte(roleJSON(map[string]string{"adapter": `"Fake"`})), testLookup)
		wantField(t, "adapter case", err, "adapter", "unknown adapter")
		// Without a lookup only structure is checked.
		if _, err := ParseRoleConfig([]byte(roleJSON(map[string]string{"adapter": `"codex"`, "effort": `"xhigh"`})), nil); err != nil {
			t.Fatalf("structure only: %v", err)
		}
	})
	t.Run("integers", func(t *testing.T) {
		for v, ok := range map[string]bool{
			`1`: true, `2147483647`: true, `0`: false, `-1`: false, `2147483648`: false, `1.0`: false, `1e2`: false, `01`: false, `"1"`: false, `true`: false,
		} {
			_, err := ParseRoleConfig([]byte(roleJSON(map[string]string{"concurrency": v})), testLookup)
			if (err == nil) != ok {
				t.Errorf("concurrency %s: %v", v, err)
			}
		}
		for v, ok := range map[string]bool{
			`"2h"`: true, `"1.5h"`: true, `"+5s"`: true, `"1ns"`: true, `"-1s"`: false, `""`: false, `" "`: false, `"2 h"`: false,
			`"2"`: false, `"9223372036854775808ns"`: false, `"3000000h"`: false, `"forever"`: false, `7200`: false,
		} {
			_, err := ParseRoleConfig([]byte(roleJSON(map[string]string{"timeout": v})), testLookup)
			if (err == nil) != ok {
				t.Errorf("timeout %s: %v", v, err)
			}
		}
	})
	t.Run("precedence", func(t *testing.T) {
		// Table order: an invalid id is reported before a missing model and
		// an unknown adapter; presence before adapter identity; adapter
		// identity before effort.
		_, err := ParseRoleConfig([]byte(roleJSON(map[string]string{"id": `"BAD"`, "model": "", "adapter": `"x"`})), testLookup)
		wantField(t, "id first", err, "id", "id must be")
		_, err = ParseRoleConfig([]byte(roleJSON(map[string]string{"model": "", "adapter": `"x"`})), testLookup)
		wantField(t, "model before adapter", err, "model", "required field")
		_, err = ParseRoleConfig([]byte(roleJSON(map[string]string{"adapter": `"x"`, "effort": `"y"`})), testLookup)
		wantField(t, "adapter before effort", err, "adapter", "unknown adapter")
		_, err = ParseRoleConfig([]byte(roleJSON(map[string]string{"effort": `"y"`, "concurrency": `0`})), testLookup)
		wantField(t, "fields before effort", err, "concurrency", "concurrency must be")
		// Syntax before fields.
		_, err = ParseRoleConfig([]byte(roleJSON(map[string]string{"id": `"BAD"`, "x": `1`})), testLookup)
		if err == nil || !strings.Contains(err.Error(), "unknown field") {
			t.Fatalf("syntax first: %v", err)
		}
	})
	t.Run("patch", func(t *testing.T) {
		p, err := ParseRolePatch([]byte(`{"name":"reviewer","concurrency":1,"timeout":"0"}`))
		if err != nil || *p.Name != "reviewer" || *p.Concurrency != 1 || *p.Timeout != 0 || p.Model != nil || p.Adapter != nil {
			t.Fatalf("patch = %+v %v", p, err)
		}
		// Absence keeps; an explicit zero timeout changes to unlimited.
		base, _ := ParseRoleConfig([]byte(roleJSON(nil)), testLookup)
		m := p.Apply(base)
		if m.Name != "reviewer" || m.Model != base.Model || m.Concurrency != 1 || m.Timeout != 0 || !m.HasTimeout || m.ID != base.ID || m.Node != base.Node {
			t.Fatalf("merged = %+v", m)
		}
		if b, _ := json.Marshal(p); string(b) != `{"name":"reviewer","concurrency":1,"timeout":"0s"}` {
			t.Fatalf("patch encoded = %s", b)
		}
		if err := ValidatePatch(p); err != nil {
			t.Fatal(err)
		}
		for name, c := range map[string]struct{ in, want string }{
			"empty":        {`{}`, "at least one field"},
			"id":           {`{"id":"worker-a"}`, "read-only"},
			"node":         {`{"node":"` + testID + `"}`, "read-only"},
			"order":        {`{"registration_order":1}`, "read-only"},
			"unknown":      {`{"workdir":"/x"}`, "unknown field"},
			"null":         {`{"model":null}`, "must not be null"},
			"blank model":  {`{"model":" "}`, "model must be"},
			"empty name":   {`{"name":""}`, "name must be"},
			"zero conc":    {`{"concurrency":0}`, "concurrency must be"},
			"conc string":  {`{"concurrency":"1"}`, "must be an integer"},
			"neg timeout":  {`{"timeout":"-1m"}`, "must not be negative"},
			"bad timeout":  {`{"timeout":"soon"}`, "Go duration"},
			"type":         {`{"effort":1}`, "must be a string"},
			"relative":     {`{"runbook":"x"}`, "absolute"},
			"empty effort": {`{"effort":""}`, "effort must not be empty"},
			"not object":   {`[]`, "not a JSON object"},
		} {
			if _, err := ParseRolePatch([]byte(c.in)); err == nil || !strings.Contains(err.Error(), c.want) {
				t.Errorf("patch %s: %v, want %q", name, err, c.want)
			}
		}
		empty := ""
		neg := -time.Second
		zero := 0
		for name, p := range map[string]RolePatch{"none": {}, "empty": {Model: &empty}, "neg": {Timeout: &neg}, "zero": {Concurrency: &zero}} {
			if err := ValidatePatch(p); err == nil {
				t.Errorf("ValidatePatch(%s) accepted", name)
			}
		}
	})
	t.Run("bounds", func(t *testing.T) {
		c, _ := ParseRoleConfig([]byte(roleJSON(nil)), testLookup)
		// The field bounds keep every valid configuration within 8 KiB.
		w := c
		w.Instruction = "/" + strings.Repeat("\"", 1023)
		w.Runbook = w.Instruction
		w.Model = strings.Repeat("\\", 1024)
		if err := ValidateRoleConfig(w, testLookup); err != nil || EncodeRoleConfigSize(w) > MaxRoleConfigBytes {
			t.Fatalf("worst case = %d %v", EncodeRoleConfigSize(w), err)
		}
		if !SameRoleConfig(c, c) || SameRoleConfig(c, w) {
			t.Fatal("SameRoleConfig")
		}
		neg := c
		neg.Timeout = -1
		if err := ValidateRoleConfig(neg, nil); err == nil {
			t.Fatal("negative timeout accepted")
		}
		view := func(id, name string, order int) string {
			rec := RoleRecord{RoleConfig: c.Resolved(), RegistrationOrder: order}
			rec.ID, rec.Name = id, name
			b, _ := json.Marshal(RoleView{RoleRecord: rec, NodeLiveness: LivenessOffline, AdapterTestOnly: true})
			return string(b)
		}
		list := func(views ...string) []byte {
			return []byte(`{"version":2,"roles":[` + strings.Join(views, ",") + `]}`)
		}
		got, err := ParseRoleListResponse(list(view("b", "coder", 2), view("a", "coder", 5), view("c", "reviewer", 1)), testLookup)
		if err != nil || len(got) != 3 || got[1].ID != "a" {
			t.Fatalf("grouped list = %+v %v", got, err)
		}
		for name, c := range map[string]struct {
			in   []byte
			want string
		}{
			"unsorted name":  {list(view("a", "reviewer", 1), view("b", "coder", 2)), "not sorted"},
			"unsorted order": {list(view("a", "coder", 3), view("b", "coder", 2)), "not sorted"},
			"dup id":         {list(view("a", "coder", 1), view("a", "coder", 2)), "repeats"},
			"dup order":      {list(view("a", "coder", 1), view("b", "reviewer", 1)), "repeats"},
			"version":        {[]byte(`{"version":1,"roles":[]}`), "local=2 remote=1"},
			"null":           {[]byte(`{"version":2,"roles":null}`), "must not be null"},
		} {
			if _, err := ParseRoleListResponse(c.in, testLookup); err == nil || !strings.Contains(err.Error(), c.want) {
				t.Errorf("list %s: %v", name, err)
			}
		}
		var many []string
		for i := 1; i <= MaxRoles+1; i++ {
			many = append(many, view("r"+strconv.Itoa(i), "n"+strconv.Itoa(1000+i), i))
		}
		if _, err := ParseRoleListResponse(list(many[:MaxRoles]...), testLookup); err != nil {
			t.Fatalf("100 roles: %v", err)
		}
		if _, err := ParseRoleListResponse(list(many...), testLookup); err == nil || !strings.Contains(err.Error(), "at most 100") {
			t.Fatalf("101 roles: %v", err)
		}
		if b, _ := Encode(RoleListResponse{Version: 2}); string(b) != `{"version":2,"roles":[]}` {
			t.Fatalf("empty list = %s", b)
		}
		// Orders are exact positive integers up to 2^53-1.
		for v, ok := range map[string]bool{"1": true, "9007199254740991": true, "0": false, "9007199254740992": false, "1.0": false} {
			rec := strings.Replace(view("a", "coder", 1), `"registration_order":1`, `"registration_order":`+v, 1)
			if _, err := ParseRoleView(json.RawMessage(rec), testLookup); (err == nil) != ok {
				t.Errorf("order %s: %v", v, err)
			}
		}
	})
	t.Run("views", func(t *testing.T) {
		c, _ := ParseRoleConfig([]byte(roleJSON(nil)), testLookup)
		v := RoleView{RoleRecord: RoleRecord{RoleConfig: c, RegistrationOrder: 3}, NodeLiveness: LivenessOnline, CanAccept: true, AdapterTestOnly: true}
		b, _ := Encode(RoleResponse{Version: 2, Role: v})
		if !strings.HasSuffix(string(b), `"timeout":"1h30m0s","registration_order":3,"inflight":0,"can_accept":true,"node_liveness":"online","adapter_test_only":true}}`) {
			t.Fatalf("view = %s", b)
		}
		got, err := ParseRoleResponse(b, testLookup)
		if err != nil || !SameRoleConfig(got.RoleConfig, c) || !got.CanAccept || got.RegistrationOrder != 3 {
			t.Fatalf("round trip = %+v %v", got, err)
		}
		good := string(b)
		for name, c := range map[string]struct{ in, want string }{
			"inflight":   {strings.Replace(good, `"inflight":0`, `"inflight":1`, 1), "inflight must be 0"},
			"liveness":   {strings.Replace(good, `"online"`, `"maybe"`, 1), "node_liveness"},
			"test only":  {strings.Replace(good, `"adapter_test_only":true`, `"adapter_test_only":false`, 1), "adapter_test_only"},
			"missing":    {strings.Replace(good, `,"can_accept":true`, "", 1), `required field "can_accept"`},
			"null":       {strings.Replace(good, `"can_accept":true`, `"can_accept":null`, 1), `required field "can_accept"`},
			"noncanon":   {strings.Replace(good, `"1h30m0s"`, `"90m"`, 1), "canonical"},
			"no timeout": {strings.Replace(good, `"timeout":"1h30m0s",`, "", 1), `required field "timeout"`},
			"extra":      {strings.Replace(good, `"inflight":0`, `"inflight":0,"x":1`, 1), "unknown field"},
			"version":    {strings.Replace(good, `{"version":2`, `{"version":3`, 1), "remote=3"},
		} {
			if _, err := ParseRoleResponse([]byte(c.in), testLookup); err == nil || !strings.Contains(err.Error(), c.want) {
				t.Errorf("view %s: %v, want %q", name, err, c.want)
			}
		}
		id, err := ParseRoleRemoveResponse([]byte(`{"version":2,"removed":"worker-a"}`))
		if err != nil || id != "worker-a" {
			t.Fatalf("removed = %q %v", id, err)
		}
		for _, in := range []string{`{"version":2,"removed":"BAD"}`, `{"version":2}`, `{"version":1,"removed":"a"}`, `{"version":2,"removed":"a","x":1}`} {
			if _, err := ParseRoleRemoveResponse([]byte(in)); err == nil {
				t.Errorf("remove response %s accepted", in)
			}
		}
		for in, want := range map[string]bool{`{"force":true}`: true, `{"force":false}`: false} {
			if f, err := ParseRoleRemoveRequest([]byte(in)); err != nil || f != want {
				t.Fatalf("remove request %s = %v %v", in, f, err)
			}
		}
		for _, in := range []string{`{}`, `{"force":null}`, `{"force":1}`, `{"force":true,"x":1}`, ``} {
			if _, err := ParseRoleRemoveRequest([]byte(in)); err == nil {
				t.Errorf("remove request %q accepted", in)
			}
		}
		e := RoleError(CodeUnavailable, "worker-a", testID, "", ReasonBusy, "busy %d", 1)
		if e.Message != "busy 1" || e.Details["role_id"] != "worker-a" || e.Details["node_id"] != testID || e.Details["reason"] != "busy" || e.Details["field"] != nil {
			t.Fatalf("role error = %+v", e)
		}
		if RoleError(CodeInternal, "", "", "", "", "x").Details != nil {
			t.Fatal("empty details kept")
		}
	})
}

// TestRoleFrames covers the protocol 2 role messages: encodings, strict
// decoders and their rejections.
func TestRoleFrames(t *testing.T) {
	c, _ := ParseRoleConfig([]byte(roleJSON(nil)), testLookup)
	rec := RoleRecord{RoleConfig: c.Resolved(), RegistrationOrder: 1}
	vb, err := EncodeFrame(ProtocolVersion, FrameRoleValidate, "p1", RoleValidateBody{Role: c.Resolved()})
	if err != nil {
		t.Fatal(err)
	}
	f, err := DecodeFrame(vb, FromPlane)
	if err != nil || f.Type != FrameRoleValidate {
		t.Fatalf("validate frame %v", err)
	}
	raw, err := DecodeRoleValidate(f.Body)
	if err != nil {
		t.Fatal(err)
	}
	if got, err := ParseResolvedRoleConfig(raw, testLookup); err != nil || !SameRoleConfig(got, c) {
		t.Fatalf("validated role = %+v %v", got, err)
	}
	if _, err := ParseResolvedRoleConfig([]byte(roleJSON(map[string]string{"timeout": ""})), testLookup); err == nil {
		t.Fatal("unresolved role accepted")
	}
	for _, in := range []string{`{}`, `{"role":1}`, `{"role":{},"x":1}`, `[]`} {
		if _, err := DecodeRoleValidate(json.RawMessage(in)); err == nil {
			t.Errorf("validate body %s accepted", in)
		}
	}
	for want, r := range map[string]RoleValidateResult{
		`{"ok":true}`: {},
		`{"ok":false,"error":{"code":"invalid_argument","message":"m","details":{"field":"runbook","reason":"manual_unreadable"}}}`: {Err: RoleError(CodeInvalidArgument, "", "", "runbook", ReasonManualUnreadable, "m")},
	} {
		b, err := json.Marshal(r)
		if err != nil || string(b) != want {
			t.Fatalf("result = %s %v", b, err)
		}
		e, err := DecodeRoleValidateResult(b)
		if err != nil || (e == nil) != (r.Err == nil) {
			t.Fatalf("decode result %s = %v %v", b, e, err)
		}
	}
	for name, in := range map[string]string{
		"ok extra":   `{"ok":true,"error":{"code":"internal","message":"m"}}`,
		"no ok":      `{"error":{"code":"internal","message":"m"}}`,
		"ok null":    `{"ok":null}`,
		"no error":   `{"ok":false}`,
		"code":       `{"ok":false,"error":{"code":"conflict","message":"m"}}`,
		"bad code":   `{"ok":false,"error":{"code":"bogus","message":"m"}}`,
		"extra":      `{"ok":false,"error":{"code":"internal","message":"m"},"x":1}`,
		"not object": `1`,
		"ok type":    `{"ok":"true"}`,
	} {
		if _, err := DecodeRoleValidateResult(json.RawMessage(in)); err == nil {
			t.Errorf("result %s accepted", name)
		}
	}
	rb, err := EncodeFrame(ProtocolVersion, FrameRolesReplace, "p2", RolesReplaceBody{Revision: 4, Roles: []RoleRecord{rec}})
	if err != nil {
		t.Fatal(err)
	}
	f, _ = DecodeFrame(rb, FromPlane)
	body, err := DecodeRolesReplace(f.Body, testLookup)
	if err != nil || body.Revision != 4 || len(body.Roles) != 1 || body.Roles[0].RegistrationOrder != 1 {
		t.Fatalf("replace = %+v %v", body, err)
	}
	if b, _ := json.Marshal(RolesReplaceBody{}); string(b) != `{"revision":0,"roles":[]}` {
		t.Fatalf("empty replace = %s", b)
	}
	second := rec
	second.ID, second.RegistrationOrder = "worker-b", 2
	recJSON := func(r RoleRecord) string { b, _ := json.Marshal(r); return string(b) }
	for name, c := range map[string]struct{ in, want string }{
		"null roles": {`{"revision":1,"roles":null}`, "must not be null"},
		"negative":   {`{"revision":-1,"roles":[]}`, "integer from 0"},
		"order":      {`{"revision":1,"roles":[` + recJSON(second) + `,` + recJSON(rec) + `]}`, "strictly increasing"},
		"dup":        {`{"revision":1,"roles":[` + recJSON(rec) + `,` + recJSON(rec) + `]}`, "repeats role"},
		"bad role":   {`{"revision":1,"roles":[{}]}`, "required field"},
		"extra":      {`{"revision":1,"roles":[],"x":1}`, "unknown field"},
		"not array":  {`{"revision":1,"roles":{}}`, "must be an array"},
	} {
		if _, err := DecodeRolesReplace(json.RawMessage(c.in), testLookup); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("replace %s: %v, want %q", name, err, c.want)
		}
	}
	var many []string
	for i := 1; i <= MaxRoles+1; i++ {
		r := rec
		r.ID, r.RegistrationOrder = "r"+strconv.Itoa(i), i
		many = append(many, recJSON(r))
	}
	if _, err := DecodeRolesReplace(json.RawMessage(`{"revision":1,"roles":[`+strings.Join(many, ",")+`]}`), testLookup); err == nil || !strings.Contains(err.Error(), "at most 100") {
		t.Fatalf("101 roles: %v", err)
	}
	ab, _ := EncodeFrame(ProtocolVersion, FrameRolesReplaceAck, "p2", RolesReplaceAckBody{Revision: 4})
	f, _ = DecodeFrame(ab, FromSidecar)
	if rev, err := DecodeRolesReplaceAck(f.Body); err != nil || rev != 4 {
		t.Fatalf("ack = %d %v", rev, err)
	}
	for _, in := range []string{`{}`, `{"revision":"4"}`, `{"revision":4,"x":1}`, `{"revision":9007199254740992}`} {
		if _, err := DecodeRolesReplaceAck(json.RawMessage(in)); err == nil {
			t.Errorf("ack %s accepted", in)
		}
	}
	// Directions: plane requests never come from a sidecar and replies
	// never from the plane.
	for typ, from := range map[string]Direction{FrameRoleValidate: FromSidecar, FrameRolesReplace: FromSidecar, FrameRoleValidateResult: FromPlane, FrameRolesReplaceAck: FromPlane} {
		if _, err := DecodeFrame(frame(2, typ, "p1", `{}`), from); err == nil || !strings.Contains(err.Error(), "not valid in this direction") {
			t.Errorf("%s from the wrong side: %v", typ, err)
		}
	}
}

// fullSnapshot is a 100-role replacement with maximal manual paths.
func fullSnapshot() RolesReplaceBody {
	c, _ := ParseRoleConfig([]byte(roleJSON(nil)), testLookup)
	b := RolesReplaceBody{Revision: MaxSafeInteger}
	for i := 1; i <= MaxRoles; i++ {
		r := RoleRecord{RoleConfig: c.Resolved(), RegistrationOrder: i}
		r.ID = "role-" + strconv.Itoa(i)
		r.Instruction = "/" + strings.Repeat("i", 1023)
		r.Runbook = "/" + strings.Repeat("r", 1023)
		r.Model = strings.Repeat("m", 1024)
		b.Roles = append(b.Roles, r)
	}
	return b
}

// TestRoleFrameBounds proves a full snapshot of 100 maximal records and a
// 100-status heartbeat fit their bounds.
func TestRoleFrameBounds(t *testing.T) {
	m, err := EncodeFrame(ProtocolVersion, FrameRolesReplace, "p1", fullSnapshot())
	if err != nil || len(m) > MaxFrameBytes {
		t.Fatalf("full snapshot: %d %v", len(m), err)
	}
	hb := HeartbeatBody{RolesRevision: MaxSafeInteger}
	for i := 1; i <= MaxRoles; i++ {
		hb.Roles = append(hb.Roles, RoleStatus{RoleID: strings.Repeat("r", 60) + strconv.Itoa(100+i), Concurrency: MaxConcurrency, CanAccept: true})
	}
	if _, err := EncodeFrame(ProtocolVersion, FrameHeartbeat, "b1", hb); err != nil {
		t.Fatalf("full heartbeat: %v", err)
	}
}

// BenchmarkRoleFrames encodes and strictly decodes a full 100-role
// replacement and a 100-status heartbeat per iteration, asserting exact
// bounds and the round trip.
func BenchmarkRoleFrames(b *testing.B) {
	for _, n := range []int{1, MaxRoles} {
		b.Run(strconv.Itoa(n), func(b *testing.B) {
			snap := fullSnapshot()
			snap.Roles = snap.Roles[:n]
			hb := HeartbeatBody{RolesRevision: snap.Revision}
			for _, r := range snap.Roles {
				hb.Roles = append(hb.Roles, RoleStatus{RoleID: r.ID, Concurrency: r.Concurrency, CanAccept: true})
			}
			b.ReportAllocs()
			for b.Loop() {
				m, err := EncodeFrame(ProtocolVersion, FrameRolesReplace, "p1", snap)
				if err != nil || len(m) > MaxFrameBytes {
					b.Fatalf("encode replace: %v", err)
				}
				f, err := DecodeFrame(m, FromPlane)
				if err != nil || len(f.Body) > MaxRolesReplaceBody {
					b.Fatalf("decode replace: %v", err)
				}
				got, err := DecodeRolesReplace(f.Body, testLookup)
				if err != nil || len(got.Roles) != n || got.Revision != snap.Revision || !SameRoleConfig(got.Roles[n-1].RoleConfig, snap.Roles[n-1].RoleConfig) {
					b.Fatalf("replace round trip: %v", err)
				}
				hm, err := EncodeFrame(ProtocolVersion, FrameHeartbeat, "b1", hb)
				if err != nil {
					b.Fatal(err)
				}
				f, err = DecodeFrame(hm, FromSidecar)
				if err != nil || len(f.Body) > MaxHeartbeatBody {
					b.Fatalf("decode heartbeat: %v", err)
				}
				body, err := DecodeHeartbeat(f.Body)
				if err != nil || len(body.Roles) != n || body.RolesRevision != snap.Revision {
					b.Fatalf("heartbeat round trip: %v", err)
				}
			}
		})
	}
}
