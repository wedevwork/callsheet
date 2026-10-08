package mcpqual

// Sanitised fixture export (design decoder-enrollment B2, FP-18). A raw
// owner capture can carry owner metadata (session identifiers, tool and
// command inventories, paths) that the capture redaction policy keeps. The
// export replaces only the enumerated non-evidence JSON values of the
// vendor transcript, by byte-span edits inside each decoded wrapper record,
// with the literal neutral values "[fixture metadata]", [] or {}; every
// other byte (keys, whitespace, escaped tool results, evidence subtrees,
// record count, order and offsets, the probe events and every other
// payload) is copied unchanged. It never repairs a partial capture, never
// decodes the transcript with a decoder and never writes an oracle, an
// attestation, an index or registry entry.
//
// The export records sanitization.json: the source and exported manifest
// hashes, every payload's source and exported hash, the complete list of
// replacements (path, wrapper record, RFC 6901 pointer, literal value; no
// original value) and protected_sha256, a fingerprint of the ordered
// protected evidence that is equal for the source and the export (each
// replaced span masked). Enrollment (CheckFixtureSanitization) recomputes
// the replacement list, the neutral values and the fingerprint from the
// exported bytes alone; source fidelity is the owner/reviewer gate, never
// CI. The accepted sources are compiled in: exactly the three inspected
// B2 bundles, by client, exact version, platform, run ID and manifest
// SHA-256. A mismatch blocks the export; nothing updates a pin.
//
// Amendment A1 (policy decoder-enrollment-b2-metadata-v2, after the owner
// declined publishing them) adds Claude's rate_limit_info (replaced by {})
// and the per-run cost values of Claude's result and Grok's end record
// (total_cost_usd, total_cost_usd_ticks and modelUsage.<model>.costUSD),
// the only numbers ever changed, each to "[fixture metadata]". Those rows
// require their member (never created), its observed type and, for
// modelUsage, exactly the one observed model member.

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
	"sort"
	"strconv"
	"strings"
)

// Export schema, policy and literals.
const (
	// SanitizationName is the enrolled bundle's sanitization receipt, one
	// extra top-level file beside expected.json.
	SanitizationName = "sanitization.json"
	// SanitizationSchema and SanitizationPolicyB2 identify the receipt and
	// its one metadata policy.
	SanitizationSchema = "mcpqual-fixture-sanitization-v1"
	// SanitizationPolicyB2 is amendment A1's policy v2; v1 was superseded
	// before any commit and is never accepted.
	SanitizationPolicyB2 = "decoder-enrollment-b2-metadata-v2"
	// FixtureMetadataValue replaces an allowed metadata string.
	FixtureMetadataValue = "[fixture metadata]"
	// protectedMaskToken replaces each allowed value span in the protected
	// fingerprint's hashing representation (never parsed as JSON).
	protectedMaskToken = "@METADATA@"
)

// The literal replacement values by kind.
var replacementLiteral = map[byte]string{'"': `"` + FixtureMetadataValue + `"`, '[': `[]`, '{': `{}`}

// FixtureSanitization is sanitization.json.
type FixtureSanitization struct {
	Schema                 string               `json:"schema"`
	Policy                 string               `json:"policy"`
	SourceManifestSHA256   string               `json:"source_manifest_sha256"`
	ExportedManifestSHA256 string               `json:"exported_manifest_sha256"`
	Files                  []SanitizedFile      `json:"files"`
	Replacements           []FixtureReplacement `json:"replacements"`
	ProtectedSHA256        string               `json:"protected_sha256"`
}

// SanitizedFile is one source payload's source and exported hash.
type SanitizedFile struct {
	Path           string `json:"path"`
	SourceSHA256   string `json:"source_sha256"`
	ExportedSHA256 string `json:"exported_sha256"`
}

// FixtureReplacement is one replaced value: its payload, zero-based wrapper
// record, RFC 6901 pointer within the record's decoded data and the literal
// JSON value written.
type FixtureReplacement struct {
	Path        string          `json:"path"`
	Record      int             `json:"record"`
	Pointer     string          `json:"pointer"`
	Replacement json.RawMessage `json:"replacement"`
}

// FixtureSource is one accepted export source: the client, its exact
// version and platform, the run ID and the source manifest's SHA-256.
type FixtureSource struct {
	Client, Version, Platform, RunID, ManifestSHA256 string
}

// FixturePolicy is the export policy: the B2 metadata rules and the
// accepted sources. Production always uses ProductionFixturePolicy.
type FixturePolicy struct {
	sources []FixtureSource
	// placement is the output placement check's view of the ancestor chain
	// (nil: the operating system). Production never sets it; tests give it
	// a fixture-root boundary (WithPlacementView).
	placement GrokPlacementFS
}

// WithPlacementView returns a copy of p whose output placement check (each
// ancestor's no-follow lookup and its .git entry) observes the filesystem
// through view. It follows the design decoder-enrollment B1 DW10 precedent
// of GrokPlacementFS: tests pass a view whose boundary is their fixture
// root, so what sits above the host's temporary directory cannot decide
// them, while every path at or below the root (each planted marker) is the
// real filesystem. The rule itself never changes, and the maintainer
// command never calls this: its policy always observes the operating
// system (code review B2 round 1).
func (p *FixturePolicy) WithPlacementView(view GrokPlacementFS) *FixturePolicy {
	cp := *p
	cp.placement = view
	return &cp
}

// productionFixtureSources are the inspected B2 bundles (design
// decoder-enrollment B2, Evidence and precedence), pinned by their source
// manifest SHA-256.
var productionFixtureSources = []FixtureSource{
	{Client: "codex", Version: CodexRealVersion, Platform: EnrolledRealPlatform, RunID: "20261008T110604Z-13cb4a",
		ManifestSHA256: "002287b308e4dcee0b09cc776326386a4210e77d2d9cdecaf9a3bc0c01afc18f"},
	{Client: "grok", Version: GrokRealVersion, Platform: EnrolledRealPlatform, RunID: "20261008T110619Z-1663d7",
		ManifestSHA256: "822ab1b93d39d4e6fd16c40c586c876e3d261976093c314e16ac2e1f7cb31f74"},
	{Client: "claude", Version: ClaudeRealVersion, Platform: EnrolledRealPlatform, RunID: "20261008T122513Z-66db37",
		ManifestSHA256: "b6e59475d6118b4690cb9f056b09a4964d8f1f5b87b3034744718db270d42295"},
}

// ProductionFixturePolicy is the compiled B2 policy with exactly the three
// pinned sources; the maintainer command always uses it.
func ProductionFixturePolicy() *FixturePolicy {
	return &FixturePolicy{sources: append([]FixtureSource(nil), productionFixtureSources...)}
}

// FixturePolicyForTests is the same B2 rules accepting other, fabricated
// sources: only tests use it (in process or through a test helper's
// main), to export tiny fake bundles. No flag, plan or variable of the
// maintainer command reaches it.
func FixturePolicyForTests(sources ...FixtureSource) *FixturePolicy {
	return &FixturePolicy{sources: append([]FixtureSource(nil), sources...)}
}

// source is the accepted source of a client, version, platform and run.
func (p *FixturePolicy) source(client, version, platform, runID string) (FixtureSource, bool) {
	for _, s := range p.sources {
		if s.Client == client && s.Version == version && s.Platform == platform && s.RunID == runID {
			return s, true
		}
	}
	return FixtureSource{}, false
}

// jspan is one JSON value of a decoded record with its byte span: an
// object's members in order, an array's elements, a string's decoded value.
type jspan struct {
	kind       byte // '{', '[', '"', 'n', 't', 'f' or '0' (a number)
	start, end int
	keys       []string
	vals       []*jspan
	str        string
}

// member is object n's member k (nil when absent or n is not an object).
func (n *jspan) member(k string) *jspan {
	if n == nil || n.kind != '{' {
		return nil
	}
	for i, key := range n.keys {
		if key == k {
			return n.vals[i]
		}
	}
	return nil
}

// valsOf is array n's elements (nil when n is absent or not an array).
func (n *jspan) valsOf() []*jspan {
	if n == nil || n.kind != '[' {
		return nil
	}
	return n.vals
}

// strMember is object n's member k when it is a string.
func (n *jspan) strMember(k string) string {
	if m := n.member(k); m != nil && m.kind == '"' {
		return m.str
	}
	return ""
}

// parseSpans checks b with checkJSON (UTF-8, paired surrogates, depth at
// most maxDepth, no duplicate members, no trailing data) and returns its
// span tree.
func parseSpans(b []byte) (*jspan, error) {
	if err := checkJSON(b); err != nil {
		return nil, err
	}
	p := &spanParser{b: b}
	p.ws()
	return p.value()
}

type spanParser struct {
	b []byte
	i int
}

func (p *spanParser) ws() {
	for p.i < len(p.b) && (p.b[p.i] == ' ' || p.b[p.i] == '\t' || p.b[p.i] == '\r' || p.b[p.i] == '\n') {
		p.i++
	}
}

// value parses one value of already checked JSON.
func (p *spanParser) value() (*jspan, error) {
	if p.i >= len(p.b) {
		return nil, errSyntax
	}
	n := &jspan{start: p.i}
	switch c := p.b[p.i]; {
	case c == '{' || c == '[':
		n.kind = c
		closer := byte('}')
		if c == '[' {
			closer = ']'
		}
		p.i++
		p.ws()
		if p.i < len(p.b) && p.b[p.i] == closer {
			p.i++
			n.end = p.i
			return n, nil
		}
		for {
			p.ws()
			if c == '{' {
				k, err := p.value()
				if err != nil || k.kind != '"' {
					return nil, errSyntax
				}
				p.ws()
				if p.i >= len(p.b) || p.b[p.i] != ':' {
					return nil, errSyntax
				}
				p.i++
				p.ws()
				n.keys = append(n.keys, k.str)
			}
			v, err := p.value()
			if err != nil {
				return nil, err
			}
			n.vals = append(n.vals, v)
			p.ws()
			if p.i >= len(p.b) {
				return nil, errSyntax
			}
			switch p.b[p.i] {
			case ',':
				p.i++
			case closer:
				p.i++
				n.end = p.i
				return n, nil
			default:
				return nil, errSyntax
			}
		}
	case c == '"':
		n.kind = '"'
		p.i++
		for p.i < len(p.b) && p.b[p.i] != '"' {
			if p.b[p.i] == '\\' {
				p.i++
			}
			p.i++
		}
		if p.i >= len(p.b) {
			return nil, errSyntax
		}
		p.i++
		n.end = p.i
		if json.Unmarshal(p.b[n.start:n.end], &n.str) != nil {
			return nil, errSyntax
		}
		return n, nil
	case c == 't' || c == 'f' || c == 'n':
		n.kind = c
		lit := map[byte]string{'t': "true", 'f': "false", 'n': "null"}[c]
		if !bytes.HasPrefix(p.b[p.i:], []byte(lit)) {
			return nil, errSyntax
		}
		p.i += len(lit)
		n.end = p.i
		return n, nil
	default:
		n.kind = '0'
		for p.i < len(p.b) && strings.IndexByte("+-0123456789.eE", p.b[p.i]) >= 0 {
			p.i++
		}
		if p.i == n.start {
			return nil, errSyntax
		}
		n.end = p.i
		return n, nil
	}
}

// pointerToken escapes one RFC 6901 reference token.
func pointerToken(s string) string {
	return strings.ReplaceAll(strings.ReplaceAll(s, "~", "~0"), "/", "~1")
}

// fixtureEdit is one allowed replacement: its pointer, the value's span and
// the kind the policy names ('"' string, '[' array, '{' map).
type fixtureEdit struct {
	pointer string
	span    *jspan
	want    byte
}

// ruleSet collects one record's allowed edits and protected evidence
// spans; err is the first value of an unexpected type.
type ruleSet struct {
	edits     []fixtureEdit
	protected []*jspan
	err       error
}

// replace names value n (absent: nothing; null: stays null, nothing) at
// pointer for replacement with kind want; any other type is unsafe input.
func (r *ruleSet) replace(n *jspan, pointer string, want byte) {
	switch {
	case n == nil || n.kind == 'n':
	case n.kind != want:
		if r.err == nil {
			r.err = fmt.Errorf("%s has an unexpected type for the policy", pointer)
		}
	default:
		r.edits = append(r.edits, fixtureEdit{pointer: pointer, span: n, want: want})
	}
}

// The single modelUsage member observed per client (design
// decoder-enrollment B2, amendment A1, DS1): any other member set is
// refused, never generalised.
const (
	claudeCostModel = "claude-sonnet-5-5"
	grokCostModel   = "grok-4.7-build"
)

// required replaces a member of an amendment A1 row: it must be present
// (an absent member is never created) with its observed kind from, or
// already hold its neutral literal of kind want; any other type is refused.
// Only A1's rows use it, and only its five cost members have from != want
// (the scoped number-to-string exception).
func (r *ruleSet) required(n *jspan, pointer string, from, want byte) {
	neutral := n != nil && (want == '"' && n.kind == '"' && n.str == FixtureMetadataValue || want == '{' && n.kind == '{' && len(n.keys) == 0)
	switch {
	case n == nil:
		r.fail(pointer + " is absent (the policy never creates a member)")
	case n.kind == from || neutral:
		r.edits = append(r.edits, fixtureEdit{pointer: pointer, span: n, want: want})
	default:
		r.fail(pointer + " has an unexpected type for the policy")
	}
}

// modelCost replaces modelUsage.<model>.costUSD of record m (pointer
// prefix pre): modelUsage must hold exactly the one observed model.
func (r *ruleSet) modelCost(m *jspan, pre, model string) {
	mu := m.member("modelUsage")
	if mu == nil || mu.kind != '{' || len(mu.keys) != 1 || mu.keys[0] != model {
		r.fail(pre + "/modelUsage is not exactly the observed model member")
		return
	}
	r.required(mu.vals[0].member("costUSD"), pre+"/modelUsage/"+pointerToken(model)+"/costUSD", '0', '"')
}

// embedded checks an embedded structured-result JSON string with checkJSON
// (UTF-8, paired surrogates, depth at most maxDepth, no duplicate members,
// no trailing data), as the policy requires of embedded result strings
// (code review B2 round 1, W1). No decoder is involved.
func (r *ruleSet) embedded(n *jspan, pointer string) {
	switch {
	case n == nil:
	case n.kind != '"':
		r.fail(pointer + " is not an embedded JSON string")
	case checkJSON([]byte(n.str)) != nil:
		r.fail(pointer + " holds an embedded result that is not strict JSON")
	}
}

// textBlocks checks every text block of a content array as an embedded
// result string.
func (r *ruleSet) textBlocks(content *jspan, pointer string) {
	if content == nil || content.kind != '[' {
		return
	}
	for j, b := range content.vals {
		if b.strMember("type") == "text" {
			r.embedded(b.member("text"), pointer+"/"+strconv.Itoa(j)+"/text")
		}
	}
}

// fail records the first unsafe-input error.
func (r *ruleSet) fail(why string) {
	if r.err == nil {
		r.err = errors.New(why)
	}
}

// protect marks n (when present) as protected evidence.
func (r *ruleSet) protect(ns ...*jspan) {
	for _, n := range ns {
		if n != nil {
			r.protected = append(r.protected, n)
		}
	}
}

// fixtureRules applies the B2 metadata policy (design decoder-enrollment
// B2, Allowed value replacements) to one decoded record of client: rules
// are scoped to the named event and member, never a recursive key match.
func fixtureRules(client string, root *jspan) ruleSet {
	var r ruleSet
	switch client {
	case "codex":
		if root.kind != '{' {
			break
		}
		item := root.member("item")
		r.protect(root.member("type"))
		for _, k := range []string{"type", "id", "server", "tool", "arguments", "result", "error", "status"} {
			r.protect(item.member(k))
		}
		switch root.strMember("type") {
		case "thread.started":
			r.replace(root.member("thread_id"), "/thread_id", '"')
		case "item.completed":
			switch item.strMember("type") {
			case "error":
				r.replace(item.member("message"), "/item/message", '"')
			case "agent_message":
				r.replace(item.member("text"), "/item/text", '"')
			case "mcp_tool_call":
				if item.strMember("server") == "probe" && item.strMember("tool") == "slow" {
					r.textBlocks(item.member("result").member("content"), "/item/result/content")
				}
			}
		}
	case "grok":
		if root.kind != '{' {
			break
		}
		for _, k := range []string{"type", "toolCallId", "toolName", "kind", "status", "rawInput", "rawOutput", "content", "stopReason"} {
			r.protect(root.member(k))
		}
		if ro := root.member("rawOutput"); root.strMember("type") == "tool_call_update" && ro.strMember("server_name") == "probe" && ro.strMember("tool_name") == "slow" {
			r.embedded(ro.member("output").member("OkayOutput"), "/rawOutput/output/OkayOutput")
		}
		switch root.strMember("type") {
		case "available_commands":
			r.replace(root.member("tools"), "/tools", '[')
			r.replace(root.member("commands"), "/commands", '[')
		case "thought", "text":
			r.replace(root.member("data"), "/data", '"')
		case "usage":
			r.replace(root.member("signature"), "/signature", '"')
		case "end":
			r.replace(root.member("sessionId"), "/sessionId", '"')
			r.replace(root.member("requestId"), "/requestId", '"')
			// Amendment A1: the per-run cost values.
			r.required(root.member("total_cost_usd"), "/total_cost_usd", '0', '"')
			r.required(root.member("total_cost_usd_ticks"), "/total_cost_usd_ticks", '0', '"')
			r.modelCost(root, "", grokCostModel)
		}
	case "claude":
		if root.kind != '[' {
			break
		}
		// The probe's tool_use IDs: their tool_result text blocks are the
		// embedded structured results.
		probeIDs := map[string]bool{}
		for _, m := range root.vals {
			for _, b := range m.member("message").member("content").valsOf() {
				if b.strMember("type") == "tool_use" && b.strMember("name") == ClaudeProbeTool {
					probeIDs[b.strMember("id")] = true
				}
			}
		}
		for i, m := range root.vals {
			if m.kind != '{' {
				r.protect(m)
				continue
			}
			pre := "/" + strconv.Itoa(i)
			for j, b := range m.member("message").member("content").valsOf() {
				if b.strMember("type") == "tool_result" && probeIDs[b.strMember("tool_use_id")] {
					r.textBlocks(b.member("content"), pre+"/message/content/"+strconv.Itoa(j)+"/content")
				}
			}
			typ, sub := m.strMember("type"), m.strMember("subtype")
			r.protect(m.member("type"), m.member("subtype"), m.member("is_error"), m.member("terminal_reason"))
			for _, k := range []string{"session_id", "uuid", "timestamp", "request_id"} {
				r.replace(m.member(k), pre+"/"+pointerToken(k), '"')
			}
			msg := m.member("message")
			if content := msg.member("content"); content != nil && content.kind == '[' {
				for j, b := range content.vals {
					if b.kind != '{' || b.strMember("type") != "text" {
						// tool_use, tool_result and every other block stay whole.
						r.protect(b)
						continue
					}
					r.protect(b.member("type"))
					if typ == "assistant" {
						r.replace(b.member("text"), pre+"/message/content/"+strconv.Itoa(j)+"/text", '"')
					}
				}
			} else {
				r.protect(content)
			}
			switch {
			case typ == "system" && sub == "init":
				for _, k := range []string{"cwd", "messaging_socket_path"} {
					r.replace(m.member(k), pre+"/"+k, '"')
				}
				for _, k := range []string{"tools", "slash_commands", "terminal_slash_commands", "agents", "skills", "plugins", "capabilities"} {
					r.replace(m.member(k), pre+"/"+k, '[')
				}
				r.replace(m.member("memory_paths"), pre+"/memory_paths", '{')
			case typ == "assistant":
				r.replace(msg.member("id"), pre+"/message/id", '"')
			case typ == "result":
				r.replace(m.member("result"), pre+"/result", '"')
				// Amendment A1: the per-run cost values.
				r.required(m.member("total_cost_usd"), pre+"/total_cost_usd", '0', '"')
				r.modelCost(m, pre, claudeCostModel)
			case typ == "rate_limit_event":
				// Amendment A1: the account-level rate-limit state; the
				// event's type is kept.
				r.required(m.member("rate_limit_info"), pre+"/rate_limit_info", '{', '{')
			}
		}
	}
	return r
}

// recordEdits parses one record and returns its policy edits sorted by
// position, refusing overlapping or duplicate edits and any edit touching
// protected evidence.
func recordEdits(client string, data []byte) (*jspan, []fixtureEdit, []*jspan, error) {
	root, err := parseSpans(data)
	if err != nil {
		return nil, nil, nil, errors.New("the record is not strict JSON")
	}
	r := fixtureRules(client, root)
	if r.err != nil {
		return nil, nil, nil, r.err
	}
	edits, err := checkEdits(r.edits, r.protected)
	if err != nil {
		return nil, nil, nil, err
	}
	return root, edits, r.protected, nil
}

// checkEdits sorts edits by position and refuses a repeated pointer,
// overlapping spans and any edit intersecting a protected span. The B2
// rules never nest one allowed member in another or in protected
// evidence; this is the guard that keeps a future rule from doing so.
func checkEdits(in []fixtureEdit, protected []*jspan) ([]fixtureEdit, error) {
	edits := append([]fixtureEdit(nil), in...)
	sort.Slice(edits, func(i, j int) bool { return edits[i].span.start < edits[j].span.start })
	seen := map[string]bool{}
	for i, e := range edits {
		if seen[e.pointer] || i > 0 && e.span.start < edits[i-1].span.end {
			return nil, fmt.Errorf("%s: overlapping or ambiguous edits", e.pointer)
		}
		seen[e.pointer] = true
		for _, p := range protected {
			if e.span.start < p.end && p.start < e.span.end {
				return nil, fmt.Errorf("%s: the edit touches protected evidence", e.pointer)
			}
		}
	}
	return edits, nil
}

// spliceEdits returns data with each edit's span replaced by repl(edit).
func spliceEdits(data []byte, edits []fixtureEdit, repl func(fixtureEdit) string) []byte {
	var out bytes.Buffer
	at := 0
	for _, e := range edits {
		out.Write(data[at:e.span.start])
		out.WriteString(repl(e))
		at = e.span.end
	}
	out.Write(data[at:])
	return out.Bytes()
}

// protectedBytes is the ordered protected evidence of one record.
func protectedBytes(data []byte, prot []*jspan) [][]byte {
	out := make([][]byte, len(prot))
	for i, p := range prot {
		out[i] = data[p.start:p.end]
	}
	return out
}

// transcriptExport is one exported vendor transcript: its bytes, its
// replacements and its protected fingerprint stream (each allowed value
// masked).
type transcriptExport struct {
	data   []byte
	reps   []FixtureReplacement
	masked []byte
}

// exportTranscript applies the policy to every wrapper record of a
// vendor-events.jsonl file rel of client. The file must be canonical
// wrappers only (no marker); each record keeps its offset and is
// re-wrapped canonically; the edited data must keep every unedited byte and
// every protected span, and mask to the same fingerprint stream as the
// source.
func exportTranscript(client, rel string, raw []byte) (*transcriptExport, error) {
	out := &transcriptExport{}
	if len(raw) == 0 {
		return out, nil
	}
	if !bytes.HasSuffix(raw, []byte{'\n'}) {
		return nil, fmt.Errorf("%s: not newline-terminated wrapper records", rel)
	}
	var data, masked bytes.Buffer
	for i, l := range bytes.Split(bytes.TrimSuffix(raw, []byte{'\n'}), []byte{'\n'}) {
		var w transcriptWrapper
		if err := decodeStrict(l, &w); err != nil || !bytes.Equal(wrapLine(w.OffsetNS, []byte(w.Data)), l) {
			return nil, fmt.Errorf("%s record %d: not a canonical transcript wrapper", rel, i)
		}
		src := []byte(w.Data)
		_, edits, prot, err := recordEdits(client, src)
		if err != nil {
			return nil, fmt.Errorf("%s record %d: %v", rel, i, err)
		}
		exp := spliceEdits(src, edits, func(e fixtureEdit) string { return replacementLiteral[e.want] })
		// Independently of any decoder: the edited record parses to the
		// same edit set, its protected evidence is byte-identical and both
		// mask to the same stream.
		_, again, prot2, err := recordEdits(client, exp)
		if err != nil || len(again) != len(edits) {
			return nil, fmt.Errorf("%s record %d: the edited record does not keep the policy's shape", rel, i)
		}
		for j := range again {
			if again[j].pointer != edits[j].pointer || string(exp[again[j].span.start:again[j].span.end]) != replacementLiteral[edits[j].want] {
				return nil, fmt.Errorf("%s record %d: %s does not hold its neutral value", rel, i, edits[j].pointer)
			}
		}
		mask := func(e fixtureEdit) string { return protectedMaskToken }
		srcMask, expMask := spliceEdits(src, edits, mask), spliceEdits(exp, again, mask)
		if !bytes.Equal(srcMask, expMask) || !slices.EqualFunc(protectedBytes(src, prot), protectedBytes(exp, prot2), bytes.Equal) {
			return nil, fmt.Errorf("%s record %d: an edit would change protected evidence", rel, i)
		}
		for _, e := range edits {
			out.reps = append(out.reps, FixtureReplacement{Path: rel, Record: i, Pointer: e.pointer, Replacement: json.RawMessage(replacementLiteral[e.want])})
		}
		data.Write(wrapLine(w.OffsetNS, exp))
		data.WriteByte('\n')
		masked.WriteString(strconv.FormatInt(w.OffsetNS, 10))
		masked.WriteByte('\n')
		masked.Write(expMask)
		masked.WriteByte('\n')
	}
	out.data, out.masked = data.Bytes(), masked.Bytes()
	return out, nil
}

// sortReplacements orders replacements by path, record, then pointer.
func sortReplacements(rs []FixtureReplacement) {
	sort.SliceStable(rs, func(i, j int) bool { return replacementLess(rs[i], rs[j]) })
}

func replacementLess(a, b FixtureReplacement) bool {
	switch {
	case a.Path != b.Path:
		return a.Path < b.Path
	case a.Record != b.Record:
		return a.Record < b.Record
	}
	return a.Pointer < b.Pointer
}

// errExportUsage marks an invalid maintainer invocation (exit 2).
var errExportUsage = errors.New("usage: mcpfixture-export --source ABSOLUTE_BUNDLE --out ABSOLUTE_NEW_STAGING_DIRECTORY")

// ioClass is a filesystem error's class, never its path.
func ioClass(err error) string {
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return "does not exist"
	case errors.Is(err, fs.ErrPermission):
		return "permission denied"
	case errors.Is(err, fs.ErrExist):
		return "already exists"
	}
	return "an I/O failure"
}

// within reports whether p is root or inside it (both clean, absolute).
func within(p, root string) bool {
	return p == root || strings.HasPrefix(p, strings.TrimSuffix(root, string(filepath.Separator))+string(filepath.Separator))
}

// checkExportOut requires out to be absent, its parent an existing
// directory, no component of its path a symbolic link, out outside the
// source tree and no ancestor directory holding a .git entry (a Git work
// tree). The ancestor chain and the output are observed through view (nil:
// the operating system; see FixturePolicy.WithPlacementView).
func checkExportOut(view GrokPlacementFS, source, out string) error {
	fsys := placementFS(view)
	for d := filepath.Dir(out); ; d = filepath.Dir(d) {
		info, err := fsys.Lstat(d)
		switch {
		case err != nil:
			return fmt.Errorf("an output ancestor %s", ioClass(err))
		case info.Mode()&fs.ModeSymlink != 0:
			return errors.New("an output path component is a symbolic link")
		case !info.IsDir():
			return errors.New("an output ancestor is not a directory")
		}
		if _, err := fsys.Lstat(filepath.Join(d, ".git")); err == nil || !errors.Is(err, fs.ErrNotExist) {
			return errors.New("the output is inside a Git work tree (or its .git lookup failed)")
		}
		if d == filepath.Dir(d) {
			break
		}
	}
	if _, err := fsys.Lstat(out); !errors.Is(err, fs.ErrNotExist) {
		return errors.New("the output directory must not exist (nothing is overwritten)")
	}
	real := source
	if r, err := filepath.EvalSymlinks(source); err == nil {
		real = r
	}
	if within(out, source) || within(out, real) {
		return errors.New("the output is inside the source tree")
	}
	return nil
}

// ExportFixture exports the one-client capture bundle at source (absolute)
// into the new staging directory out (absolute) under pol: the payloads,
// the regenerated manifest.json (written last) and sanitization.json, but
// never an oracle, attestation, index or registry entry. The source is read
// once through the confined, bounded bundle reader and never written; the
// staging directory is created 0700 with 0600 files and, on any failure,
// is left without a manifest (never overwritten or cleaned up). The
// exported bundle is validated in memory (bundle rules, the receipt check
// and the capture redaction fixed point) before anything is written.
func ExportFixture(source, out string, pol *FixturePolicy) (*FixtureSanitization, error) {
	if !filepath.IsAbs(source) || !filepath.IsAbs(out) {
		return nil, errExportUsage
	}
	source, out = filepath.Clean(source), filepath.Clean(out)
	root, err := os.OpenRoot(source)
	if err != nil {
		return nil, fmt.Errorf("source: the bundle directory %s", ioClass(err))
	}
	t := &rootTree{root}
	defer t.close()
	mb, err := t.read(CaptureManifestName, MaxEvidenceFileBytes)
	if err != nil {
		return nil, fmt.Errorf("source %s: cannot be read safely", CaptureManifestName)
	}
	b, err := validateBundle(t, ".", mb)
	if err != nil {
		return nil, errors.New("source: not a valid capture bundle (validation failed)")
	}
	m := b.Manifest
	if m.State != CaptureComplete || len(m.Clients) != 1 || m.Clients[0].State != CaptureComplete || m.Clients[0].ObservedVersion == nil {
		return nil, errors.New("source: not a complete one-client capture")
	}
	c := &m.Clients[0]
	src, ok := pol.source(c.ID, *c.ObservedVersion, m.OS+"/"+m.Arch, m.RunID)
	switch {
	case !ok || c.ExpectedVersion != src.Version:
		return nil, errors.New("source: not an accepted source identity (client, exact version, platform, run)")
	case sha256Hex(mb) != src.ManifestSHA256:
		return nil, errors.New("source: the manifest's SHA-256 differs from the compiled source pin (a pin is never updated to fit)")
	}
	if enc, err := encodeIndent(m); err != nil || !bytes.Equal(enc, mb) {
		return nil, errors.New("source: the manifest is not in its canonical encoding")
	}
	// The output placement, after the read-only source checks (so an
	// unaccepted source is refused as such wherever the output would be)
	// and before anything is written.
	if err := checkExportOut(pol.placement, source, out); err != nil {
		return nil, fmt.Errorf("output: %w", err)
	}
	files := map[string][]byte{}
	var reps []FixtureReplacement
	var masked []byte
	vendor := ClientFile(c.ID, FileVendorEvents)
	for _, f := range m.Files {
		data := b.Files[f.Path]
		if f.Path != vendor {
			files[f.Path] = data
			continue
		}
		te, err := exportTranscript(c.ID, f.Path, data)
		if err != nil {
			return nil, fmt.Errorf("source: %v", err)
		}
		files[f.Path], reps, masked = te.data, te.reps, te.masked
	}
	masked = append(masked, b.Files[ClientFile(c.ID, FileServerEvents)]...)
	nm := *m
	nm.Files = append([]EvidenceRef(nil), m.Files...)
	san := &FixtureSanitization{Schema: SanitizationSchema, Policy: SanitizationPolicyB2, SourceManifestSHA256: src.ManifestSHA256,
		Files: []SanitizedFile{}, Replacements: reps, ProtectedSHA256: sha256Hex(masked)}
	if san.Replacements == nil {
		san.Replacements = []FixtureReplacement{}
	}
	sortReplacements(san.Replacements)
	for i, f := range nm.Files {
		data := files[f.Path]
		nm.Files[i].SHA256, nm.Files[i].Bytes = sha256Hex(data), int64(len(data))
		san.Files = append(san.Files, SanitizedFile{Path: f.Path, SourceSHA256: f.SHA256, ExportedSHA256: nm.Files[i].SHA256})
	}
	nmb, err := encodeIndent(&nm)
	if err != nil {
		return nil, err
	}
	san.ExportedManifestSHA256 = sha256Hex(nmb)
	sb, err := encodeIndent(san)
	if err != nil {
		return nil, err
	}
	if err := checkExportedInMemory(pol, nmb, sb, files); err != nil {
		return nil, fmt.Errorf("export self-check: %v", err)
	}
	if err := writeExport(out, nmb, sb, files); err != nil {
		return nil, fmt.Errorf("output: %v", err)
	}
	return san, nil
}

// checkExportedInMemory validates the exported bundle before it is
// written: the bundle rules with the receipt as its one extra file, the
// receipt check and the capture redaction fixed point of every file (no
// local literals: the owner's own literal check is separate).
func checkExportedInMemory(pol *FixturePolicy, mb, sb []byte, files map[string][]byte) error {
	mem := map[string][]byte{CaptureManifestName: mb, SanitizationName: sb}
	for p, d := range files {
		mem[p] = d
	}
	b, err := validateBundle(memTree(mem), ".", mb, SanitizationName)
	if err != nil {
		return err
	}
	if _, err := CheckFixtureSanitization(pol, b, sb); err != nil {
		return err
	}
	red := NewCaptureRedactor(nil, nil)
	names := make([]string, 0, len(mem))
	for p := range mem {
		names = append(names, p)
	}
	sort.Strings(names)
	for _, p := range names {
		if err := RedactionFixedPoint(p, mem[p], red); err != nil {
			return errors.New(p + ": not a fixed point of the capture redaction policy")
		}
	}
	return nil
}

// writeExport creates out (0700) and writes every payload and the receipt
// (0600, new files only), then the manifest last.
func writeExport(out string, mb, sb []byte, files map[string][]byte) error {
	if err := os.Mkdir(out, 0o700); err != nil {
		return fmt.Errorf("the output directory %s", ioClass(err))
	}
	put := func(rel string, data []byte) error {
		p := filepath.Join(out, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
			return fmt.Errorf("%s: directory %s", rel, ioClass(err))
		}
		f, err := os.OpenFile(p, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
		if err != nil {
			return fmt.Errorf("%s %s", rel, ioClass(err))
		}
		_, werr := f.Write(data)
		if err := errors.Join(werr, f.Close()); err != nil {
			return fmt.Errorf("%s: %s", rel, ioClass(err))
		}
		return nil
	}
	names := make([]string, 0, len(files))
	for p := range files {
		names = append(names, p)
	}
	sort.Strings(names)
	for _, p := range names {
		if err := put(p, files[p]); err != nil {
			return err
		}
	}
	if err := put(SanitizationName, sb); err != nil {
		return err
	}
	return put(CaptureManifestName, mb)
}

// memTree is an in-memory bundle (relative slash paths to bytes) for the
// pre-write self-check: a tree whose directories are the paths' prefixes.
func memTree(files map[string][]byte) tree {
	return &memDir{files: files, dir: ""}
}

type memDir struct {
	files map[string][]byte
	dir   string // "" or "a/b/"
}

func (m *memDir) sub(name string) (tree, error) {
	prefix := m.dir + name + "/"
	for p := range m.files {
		if strings.HasPrefix(p, prefix) {
			return &memDir{files: m.files, dir: prefix}, nil
		}
	}
	return nil, fmt.Errorf("%s: %w", name, fs.ErrNotExist)
}

func (m *memDir) read(name string, limit int) ([]byte, error) {
	d, ok := m.files[m.dir+name]
	switch {
	case !ok:
		return nil, fmt.Errorf("%s: %w", name, fs.ErrNotExist)
	case len(d) > limit:
		return nil, fmt.Errorf("%s exceeds %d bytes", name, limit)
	}
	return d, nil
}

func (m *memDir) list() ([]fs.DirEntry, error) {
	seen := map[string]bool{}
	var out []fs.DirEntry
	for p := range m.files {
		if !strings.HasPrefix(p, m.dir) {
			continue
		}
		name, rest, sub := strings.Cut(strings.TrimPrefix(p, m.dir), "/")
		if !seen[name] {
			seen[name] = true
			out = append(out, memEntry{name: name, dir: sub && rest != ""})
		}
	}
	slices.SortFunc(out, func(a, b fs.DirEntry) int { return strings.Compare(a.Name(), b.Name()) })
	return out, nil
}

func (m *memDir) close() {}

// memEntry is one memDir entry: a regular file or a directory.
type memEntry struct {
	name string
	dir  bool
}

func (e memEntry) Name() string { return e.name }
func (e memEntry) IsDir() bool  { return e.dir }
func (e memEntry) Type() fs.FileMode {
	if e.dir {
		return fs.ModeDir
	}
	return 0
}
func (e memEntry) Info() (fs.FileInfo, error) { return nil, errors.New("no file info in memory") }

// CheckFixtureSanitization verifies an exported bundle's receipt sb
// against its validated bundle b under pol, from the exported bytes alone
// (design decoder-enrollment B2, Provenance schema and validation): the
// strict schema and policy; an accepted source whose pin is the receipt's
// source hash; the exported manifest hash; the files list exactly the
// manifest's payloads with their exported hashes, unchanged hashes equal
// for a file without replacements; the replacement list exactly the policy's
// applicable replacements recomputed from the exported transcript, each
// holding its neutral value; and the protected fingerprint recomputed from
// the exported bytes. It cannot prove the original bytes: that is the
// owner/reviewer gate.
func CheckFixtureSanitization(pol *FixturePolicy, b *CaptureBundle, sb []byte) (*FixtureSanitization, error) {
	fail := func(format string, a ...any) error { return fmt.Errorf("sanitization: "+format, a...) }
	san, err := ParseFixtureSanitization(sb)
	if err != nil {
		return nil, err
	}
	m := b.Manifest
	if len(m.Clients) != 1 || m.Clients[0].ObservedVersion == nil {
		return nil, fail("the bundle is not a one-client capture")
	}
	c := &m.Clients[0]
	src, ok := pol.source(c.ID, *c.ObservedVersion, m.OS+"/"+m.Arch, m.RunID)
	switch {
	case !ok:
		return nil, fail("the bundle is not an accepted source identity")
	case san.SourceManifestSHA256 != src.ManifestSHA256:
		return nil, fail("source_manifest_sha256 is not the compiled source pin")
	case san.ExportedManifestSHA256 != sha256Hex(b.ManifestBytes):
		return nil, fail("exported_manifest_sha256 differs from the manifest")
	case len(san.Files) != len(m.Files):
		return nil, fail("files do not list exactly the manifest's payloads")
	}
	vendor := ClientFile(c.ID, FileVendorEvents)
	te, err := exportedReplacements(c.ID, vendor, b.Files[vendor])
	if err != nil {
		return nil, fail("%v", err)
	}
	if len(te.reps) != len(san.Replacements) {
		return nil, fail("the replacement list is not exactly the policy's %d applicable replacements", len(te.reps))
	}
	for i, r := range san.Replacements {
		w := te.reps[i]
		if r.Path != w.Path || r.Record != w.Record || r.Pointer != w.Pointer || !bytes.Equal(compactJSON(r.Replacement), compactJSON(w.Replacement)) {
			return nil, fail("replacement %d (%s record %d %s) is not the policy's applicable replacement", i, r.Path, r.Record, r.Pointer)
		}
	}
	replaced := map[string]bool{}
	for _, r := range san.Replacements {
		replaced[r.Path] = true
	}
	for i, f := range san.Files {
		mf := m.Files[i]
		switch {
		case f.Path != mf.Path || f.ExportedSHA256 != mf.SHA256:
			return nil, fail("files[%d] %s is not the manifest's payload and hash", i, f.Path)
		case !hex64.MatchString(f.SourceSHA256):
			return nil, fail("files[%d] source_sha256 is not a SHA-256", i)
		case f.SourceSHA256 != f.ExportedSHA256 && !replaced[f.Path]:
			return nil, fail("%s changed without a recorded replacement", f.Path)
		}
	}
	masked := append(te.masked, b.Files[ClientFile(c.ID, FileServerEvents)]...)
	if sha256Hex(masked) != san.ProtectedSHA256 {
		return nil, fail("protected_sha256 differs from the fingerprint of the exported protected evidence")
	}
	return san, nil
}

// exportedReplacements recomputes, from an exported transcript, the
// policy's applicable replacements (each value must already be its neutral
// literal) and the protected fingerprint stream.
func exportedReplacements(client, rel string, raw []byte) (*transcriptExport, error) {
	out := &transcriptExport{}
	if len(raw) == 0 {
		return out, nil
	}
	var masked bytes.Buffer
	for i, l := range bytes.Split(bytes.TrimSuffix(raw, []byte{'\n'}), []byte{'\n'}) {
		var w transcriptWrapper
		if err := decodeStrict(l, &w); err != nil {
			return nil, fmt.Errorf("%s record %d: not a transcript wrapper", rel, i)
		}
		data := []byte(w.Data)
		_, edits, _, err := recordEdits(client, data)
		if err != nil {
			return nil, fmt.Errorf("%s record %d: %v", rel, i, err)
		}
		for _, e := range edits {
			lit := replacementLiteral[e.want]
			if string(data[e.span.start:e.span.end]) != lit {
				return nil, fmt.Errorf("%s record %d: %s does not hold the neutral value", rel, i, e.pointer)
			}
			out.reps = append(out.reps, FixtureReplacement{Path: rel, Record: i, Pointer: e.pointer, Replacement: json.RawMessage(lit)})
		}
		masked.WriteString(strconv.FormatInt(w.OffsetNS, 10))
		masked.WriteByte('\n')
		masked.Write(spliceEdits(data, edits, func(fixtureEdit) string { return protectedMaskToken }))
		masked.WriteByte('\n')
	}
	sortReplacements(out.reps)
	out.masked = masked.Bytes()
	return out, nil
}

// compactJSON is raw compacted (raw itself when it is not JSON).
func compactJSON(raw json.RawMessage) []byte {
	var b bytes.Buffer
	if json.Compact(&b, raw) != nil {
		return raw
	}
	return b.Bytes()
}

// ParseFixtureSanitization strictly decodes sanitization.json: bounded,
// unknown members and missing members refused, the schema and policy
// exact, hashes SHA-256, every replacement one of the three literal
// values, and files and replacements sorted and unique.
func ParseFixtureSanitization(b []byte) (*FixtureSanitization, error) {
	fail := func(format string, a ...any) error { return fmt.Errorf("sanitization: "+format, a...) }
	if len(b) > maxIndexBytes {
		return nil, fail("%d bytes exceeds %d", len(b), maxIndexBytes)
	}
	var s FixtureSanitization
	if err := decodeStrict(b, &s); err != nil {
		return nil, fail("%v", err)
	}
	if err := requireFields(b, reflect.TypeOf(s), "sanitization"); err != nil {
		return nil, err
	}
	switch {
	case s.Schema != SanitizationSchema || s.Policy != SanitizationPolicyB2:
		return nil, fail("schema %q policy %q, want %q %q", s.Schema, s.Policy, SanitizationSchema, SanitizationPolicyB2)
	case !hex64.MatchString(s.SourceManifestSHA256) || !hex64.MatchString(s.ExportedManifestSHA256) || !hex64.MatchString(s.ProtectedSHA256):
		return nil, fail("the manifest and protected hashes must be SHA-256 hex")
	case s.Files == nil || s.Replacements == nil:
		return nil, fail("files and replacements must be lists")
	}
	for i, f := range s.Files {
		if i > 0 && s.Files[i-1].Path >= f.Path {
			return nil, fail("files are not sorted by path or %s repeats", f.Path)
		}
	}
	allowed := map[string]bool{}
	for _, lit := range replacementLiteral {
		allowed[lit] = true
	}
	for i, r := range s.Replacements {
		switch {
		case r.Record < 0 || !strings.HasPrefix(r.Pointer, "/") || checkRelPath("replacement path", r.Path) != nil:
			return nil, fail("replacement %d has an invalid path, record or pointer", i)
		case !allowed[string(compactJSON(r.Replacement))]:
			return nil, fail("replacement %d is not a literal neutral value", i)
		case i > 0 && !replacementLess(s.Replacements[i-1], r):
			return nil, fail("replacements are not sorted or replacement %d repeats", i)
		}
	}
	return &s, nil
}

// RunFixtureExport is the maintainer command mcpfixture-export: exactly
// --source ABSOLUTE_BUNDLE and --out ABSOLUTE_NEW_STAGING_DIRECTORY, each
// once (also as --flag=value). Exit 2 is an invalid, missing or repeated
// flag or a nonabsolute path; 1 any validation, policy, safety or I/O
// failure; 0 only a completed candidate export. It makes no model call,
// network access, decoder invocation, index or registry update or commit.
// cmd/mcpfixture-export always passes ProductionFixturePolicy.
func RunFixtureExport(args []string, stdout, stderr io.Writer, pol *FixturePolicy) int {
	vals := map[string]string{}
	usage := func(why string) int {
		fmt.Fprintf(stderr, "mcpfixture-export: %s\n%v\n", why, errExportUsage)
		return 2
	}
	for i := 0; i < len(args); i++ {
		name, val, eq := strings.Cut(strings.TrimPrefix(strings.TrimPrefix(args[i], "-"), "-"), "=")
		if !strings.HasPrefix(args[i], "-") || name != "source" && name != "out" {
			return usage("unknown argument")
		}
		if _, dup := vals[name]; dup {
			return usage("--" + name + " is repeated")
		}
		if !eq {
			if i+1 >= len(args) {
				return usage("--" + name + " needs a value")
			}
			i++
			val = args[i]
		}
		vals[name] = val
	}
	src, out := vals["source"], vals["out"]
	if src == "" || out == "" || !filepath.IsAbs(src) || !filepath.IsAbs(out) {
		return usage("--source and --out must both be absolute paths")
	}
	san, err := ExportFixture(src, out, pol)
	if err != nil {
		if errors.Is(err, errExportUsage) {
			return usage("invalid paths")
		}
		fmt.Fprintf(stderr, "mcpfixture-export: %v\n", err)
		return 1
	}
	fmt.Fprintf(stdout, "mcpfixture-export: candidate export complete: %d payloads, %d replacements, policy %s; no oracle, attestation, index or registry entry was written\n",
		len(san.Files), len(san.Replacements), san.Policy)
	return 0
}
