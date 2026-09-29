package mcpqual

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/wedevwork/callsheet/internal/contract"
)

// Regressions for code review r2 (C1, C2). The full-size C2 reproduction
// (1,500,000-character tool-call IDs against the 8 MiB limit) runs once
// per invocation in the function test TestMCPQualificationSchema/limits.

// r2 C2: the serialized report stays within its byte budget whatever the
// sizes of vendor-supplied strings, although every evidence file is within
// its own limit: the reviewer's shape (four cases whose tool-call IDs sum
// past the budget) at a reduced budget. Bounded cases are inconclusive
// with a reason, keep their transcript references, and ParseReport
// accepts the report; the same run within budget is untouched.
func TestR2AggregateReportBound(t *testing.T) {
	m := fullModel()
	m.toolID = strings.Repeat("x", 6000)
	full := runPlan(t, newRunner(t, newWorld(t, m), planWith(defaultOnly(100, 300))), context.Background())
	for _, ph := range full.Clients[0].Phases {
		for _, cs := range ph.Cases {
			if strings.HasPrefix(deref(cs.Reason), ReasonReportBound) {
				t.Fatalf("a report within budget was bounded: %s", deref(cs.Reason))
			}
		}
	}
	r := newRunner(t, newWorld(t, m), planWith(defaultOnly(100, 300)))
	r.ReportLimit = 16 << 10
	rep, err := r.Run(context.Background())
	if err != nil {
		t.Fatalf("no report: %v", err)
	}
	st, err := os.Stat(filepath.Join(r.OutDir, "report.json"))
	if err != nil || st.Size() > int64(r.ReportLimit) {
		t.Fatalf("report.json is %d bytes, over its %d-byte budget", st.Size(), r.ReportLimit)
	}
	for _, ev := range rep.Evidence {
		if ev.Bytes > MaxEvidenceFileBytes {
			t.Fatalf("%s is %d bytes: the shape needs every file within its limit", ev.Path, ev.Bytes)
		}
	}
	strictReport(t, r)
	bounded := 0
	for _, ph := range rep.Clients[0].Phases {
		for _, cs := range ph.Cases {
			if strings.HasPrefix(deref(cs.Reason), ReasonReportBound) {
				if cs.Outcome != OutcomeInconclusive || cs.VendorEvents == nil || ph.Status == StatusConclusive {
					t.Errorf("bounded case %s: outcome %s, vendor events %v, phase %s", cs.CaseID, cs.Outcome, cs.VendorEvents, ph.Status)
				}
				bounded++
			}
		}
	}
	if bounded < 4 || rep.Clients[0].Outcome == StatusConclusive || rep.Outcome != "partial" || rep.ExitCode() != contract.ExitCode(contract.New(contract.CodeUnavailable, "")) {
		t.Fatalf("%d cases bounded; client %s, report %s", bounded, rep.Clients[0].Outcome, rep.Outcome)
	}
}

// r2 C2: at every budget boundReport either yields a valid report within
// the budget (cutting client and phase strings, then dropping case
// summaries, as needed) or refuses; it never writes an oversize report.
func TestR2BoundReportStages(t *testing.T) {
	m := fullModel()
	m.toolID = strings.Repeat("x", 1500)
	rep := runPlan(t, newRunner(t, newWorld(t, m), planWith(defaultOnly(100, 300))), context.Background())
	rep.Clients[0].ObservedVersion = sptr(strings.Repeat("v", 3000))
	rep.Clients[0].Phases[0].Result = sptr(strings.Repeat("r", 3000))
	rep.Cleanup.Failures = []string{strings.Repeat("f", 3000)}
	src, err := encodeJSON(rep)
	if err != nil {
		t.Fatal(err)
	}
	sawCut, sawDrop, refused := false, false, false
	for limit := len(src); limit > 0 && !refused; limit = limit * 15 / 16 {
		var cp Report
		if err := json.Unmarshal(src, &cp); err != nil {
			t.Fatal(err)
		}
		if err := boundReport(&cp, limit); err != nil {
			refused = true
			continue
		}
		b, _ := encodeIndent(&cp)
		if len(b) > limit {
			t.Fatalf("limit %d: %d bytes", limit, len(b))
		}
		if _, err := ParseReport(b); err != nil {
			t.Fatalf("limit %d: %v", limit, err)
		}
		c := cp.Clients[0]
		sawCut = sawCut || len(*c.ObservedVersion) <= maxBoundString && c.Outcome == "partial" && cp.Outcome == "partial"
		dropped := true
		for _, ph := range c.Phases {
			dropped = dropped && len(ph.Cases) == 0 && ph.Status != StatusConclusive
		}
		sawDrop = sawDrop || dropped
	}
	if !sawCut || !sawDrop || !refused {
		t.Fatalf("stages seen: cut %v, dropped %v, refused %v", sawCut, sawDrop, refused)
	}
	if s := cutString(strings.Repeat("é", 400)); len(s) > maxBoundString || !utf8.ValidString(s) || !strings.HasSuffix(s, cutMark) {
		t.Fatalf("cutString = %d bytes %q", len(s), s[len(s)-8:])
	}
}

// r2 C1: redaction decisions are made on unescaped keys and values; the
// fast path keeps only lines whose bytes are their content.
func TestR2RedactLine(t *testing.T) {
	r := NewRedactor([]string{"hunter2-secret-value"}, map[string]string{"/home/owner": "<home>"})
	for _, tc := range []struct{ in, want string }{
		{`{"type":"noise","result":"hunter2-\u0073ecret-value"}`, `{"type":"noise","result":"[REDACTED]"}`},
		{`{"type":"noise","pa\u0073\u0073word":"ordinary-value"}`, `{"type":"noise","password":"[REDACTED]"}`},
		{` 	{"passwd":"x"}`, ` 	{"passwd":"[REDACTED]"}`},
		{`["\/home\/owner\/work"]`, `["<home>/work"]`},
		{`{"\/home\/owner":1}`, `{"<home>":1}`},
		{`{"credentials":{"user":"u1","list":["p1"]},"n":2}`, `{"credentials":{"user":"[REDACTED]","list":["[REDACTED]"]},"n":2}`},
		{`{"text":"{\"api\\u005fkey\":\"v1\"}"}`, `{"text":"{\"api_key\":\"[REDACTED]\"}"}`},
		{`"hunter2-\u0073ecret-value"`, `"[REDACTED]"`},
		{`{"cut":"hunter2-\u0073ecret-value and mo`, `{"cut":"[REDACTED] and mo`},
		{`{"pa\u0073sword":"cut-valu`, `{"password":"[REDACTED]`},
		{`{"cookie":"c1","ok":true,"none":null}`, `{"cookie":"[REDACTED]","ok":true,"none":null}`},
		{`{"emoji":"\ud83d\ude00 hunter2-secret-value"}`, `{"emoji":"😀 [REDACTED]"}`},
		// Unchanged lines keep their bytes, escapes and spacing included.
		{`{"text": "line\nbreak \u00e9 \ud83d"}`, `{"text": "line\nbreak \u00e9 \ud83d"}`},
		{`{"key_count": 3}`, `{"key_count": 3}`},
		{`plain \q \u12 text\`, `plain \q \u12 text\`},
		{`no trigger at all`, `no trigger at all`},
	} {
		if got := string(r.Line([]byte(tc.in))); got != tc.want {
			t.Errorf("Line(%s)\n got %s\nwant %s", tc.in, got, tc.want)
		}
	}
	deep := `"hunter2-secret-value"`
	for range maxRedactDepth + 2 {
		deep = quoteJSON(deep)
	}
	if got := r.String(deep); strings.Contains(unescapeView(got), "hunter2-secret-value") {
		t.Errorf("a deeply nested literal survives: %s", got)
	}
}
