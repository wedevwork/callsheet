package reale2e

import (
	"regexp"
	"strings"
	"unicode/utf8"
)

// Marker and routing-field parsers for the development path only (design
// 12a-dev-flow-harness, Transition policy). Legacy marker semantics are
// unchanged.
//
// Lines end at LF. Only a CR immediately before that LF is removed; a bare
// CR anywhere else is an ordinary character of its line. A field line is
// plain, unindented text outside Markdown code fences, not a heading,
// table cell, list item or quotation, compared exactly (never trimmed).
// Fences follow CommonMark: an opening run of at least three backticks or
// tildes indented at most three spaces, closed only by a run of the same
// character at least as long followed by nothing but spaces or tabs. An
// unclosed fence runs to the end of the text.

const (
	markerDesignDraft    = "DESIGN_DRAFT"
	markerNeedsInput     = "DESIGN_NEEDS_INPUT"
	markerDesignChanges  = "DESIGN_CHANGES_REQUESTED"
	markerDesignApproved = "DESIGN_REVIEW_APPROVED"
	markerImplComplete   = "IMPL_COMPLETE"
	markerFixesComplete  = "FIXES_COMPLETE"
	markerQuestions      = "DESIGN_QUESTIONS"
	markerNeedsDesign    = "NEEDS_DESIGN"
	markerNeedsTriage    = "NEEDS_TRIAGE"
	markerBlocked        = "BLOCKED"
	markerReviewApproved = "REVIEW_APPROVED"
	markerReviewChanges  = "REVIEW_CHANGES_REQUESTED"
	markerTriageConf     = "TRIAGE_CONFIRMED"
	markerTriageProb     = "TRIAGE_PROBABLE"
	markerTriageUnconf   = "TRIAGE_UNCONFIRMED"
	markerTriageBlocked  = "TRIAGE_BLOCKED"
)

const (
	blockerEnvironment = "environment"
	blockerNone        = "none"
	designRequiredYes  = "yes"
	designRequiredNo   = "no"
)

var errInvalidMarker = errNamed("invalid_marker")
var errInvalidField = errNamed("invalid_field")

type namedErr string

func (e namedErr) Error() string { return string(e) }

func errNamed(code string) error { return namedErr(code) }

// mdLine is one physical line with its classification.
type mdLine struct {
	text string
	// fenced is set for a fence delimiter line and every line inside a
	// fenced block (an unclosed fence runs to the end).
	fenced bool
	// plain is set for a line that can carry a field: outside a fence,
	// unindented, and not a heading, table row, list item or quotation.
	plain bool
	// blank is an empty line or one of only spaces and tabs.
	blank bool
	// heading is set for a Setext heading's text lines.
	heading bool
}

// splitLines splits text on LF and removes exactly one CR that ends a
// line before that LF. A bare CR is not a line boundary.
func splitLines(text string) []string {
	lines := strings.Split(text, "\n")
	for i := 0; i < len(lines)-1; i++ {
		lines[i] = strings.TrimSuffix(lines[i], "\r")
	}
	return lines
}

// scanLines classifies every line of text.
func scanLines(text string) []mdLine {
	raw := splitLines(text)
	out := make([]mdLine, len(raw))
	open := false
	var fenceChar byte
	fenceLen := 0
	for i, l := range raw {
		out[i] = mdLine{text: l, blank: strings.Trim(l, " \t") == ""}
		if open {
			out[i].fenced = true
			if fenceCloses(l, fenceChar, fenceLen) {
				open = false
			}
			continue
		}
		if c, n, ok := fenceOpens(l); ok {
			out[i].fenced, open, fenceChar, fenceLen = true, true, c, n
			continue
		}
		if setextUnderline(l) && i > 0 && paragraphLine(out[i-1]) {
			// A Setext heading: the underline turns the whole paragraph
			// above it into a heading, so none of its lines is plain.
			for j := i - 1; j >= 0 && paragraphLine(out[j]); j-- {
				out[j].plain, out[j].heading = false, true
			}
			continue
		}
		out[i].plain = plainFieldLine(l)
	}
	return out
}

// setextUnderlineRE is a Setext heading underline: a run of = or of -,
// indented at most three spaces, then only spaces or tabs.
var setextUnderlineRE = regexp.MustCompile(`^ {0,3}(?:=+|-+)[ \t]*$`)

func setextUnderline(l string) bool { return setextUnderlineRE.MatchString(l) }

// paragraphLine reports whether a classified line is paragraph text a
// Setext underline would turn into a heading: nonblank, outside a fence,
// not already a heading, and not the start of an ATX heading, list item
// or quotation (an indented or table-looking line continues the
// paragraph).
func paragraphLine(l mdLine) bool {
	if l.blank || l.fenced || l.heading {
		return false
	}
	rest := strings.TrimLeft(l.text, " ")
	if len(l.text)-len(rest) <= 3 && rest != "" && (rest[0] == '#' || rest[0] == '>' || listMarkerRE.MatchString(rest)) {
		return false
	}
	return true
}

// fenceIndent strips at most three leading spaces; ok is false for a line
// indented further (an indented code block, never a fence).
func fenceIndent(l string) (string, bool) {
	n := 0
	for n < len(l) && l[n] == ' ' {
		n++
	}
	if n > 3 {
		return "", false
	}
	return l[n:], true
}

// fenceOpens reports an opening code fence and its character and length.
func fenceOpens(l string) (byte, int, bool) {
	rest, ok := fenceIndent(l)
	if !ok || rest == "" || (rest[0] != '`' && rest[0] != '~') {
		return 0, 0, false
	}
	c := rest[0]
	n := 0
	for n < len(rest) && rest[n] == c {
		n++
	}
	if n < 3 {
		return 0, 0, false
	}
	if c == '`' && strings.Contains(rest[n:], "`") {
		return 0, 0, false
	}
	return c, n, true
}

// fenceCloses reports whether l closes a fence of c at least n long.
func fenceCloses(l string, c byte, n int) bool {
	rest, ok := fenceIndent(l)
	if !ok {
		return false
	}
	k := 0
	for k < len(rest) && rest[k] == c {
		k++
	}
	return k >= n && strings.Trim(rest[k:], " \t") == ""
}

// listMarkerRE is a bullet or ordered list item's marker.
var listMarkerRE = regexp.MustCompile(`^(?:[-+*]|[0-9]{1,9}[.)])(?:[ \t]|$)`)

// plainFieldLine reports whether line can carry a routing field: it is
// nonblank and unindented, and it is not a heading, a table row, a list
// item or a quotation.
func plainFieldLine(line string) bool {
	if strings.Trim(line, " \t") == "" {
		return false
	}
	switch line[0] {
	case ' ', '\t', '#', '|', '>':
		return false
	}
	return !listMarkerRE.MatchString(line)
}

// statusTokenRE is a marker token.
var statusTokenRE = regexp.MustCompile(`^[A-Z][A-Z_]*[A-Z]$`)

// looksLikeStatus reports a plain line that claims to be a STATUS line in
// any letter case ("Status:" included).
func looksLikeStatus(line string) bool {
	return strings.HasPrefix(strings.ToUpper(line), "STATUS:")
}

// terminalIndex returns the index of the last nonblank line, or -1.
func terminalIndex(lines []mdLine) int {
	for i := len(lines) - 1; i >= 0; i-- {
		if !lines[i].blank {
			return i
		}
	}
	return -1
}

// parseTerminalMarker returns the single standalone STATUS marker of
// text: the last nonblank line, plain and outside any fence, exactly
// "STATUS: TOKEN", and the only plain line claiming to be a STATUS line.
// Examples inside fences or quotations do not count.
func parseTerminalMarker(text string) (string, error) {
	lines := scanLines(text)
	last := terminalIndex(lines)
	if last < 0 || !lines[last].plain {
		return "", errInvalidMarker
	}
	for i, l := range lines {
		if l.plain && looksLikeStatus(l.text) && i != last {
			return "", errInvalidMarker
		}
	}
	tok, ok := strings.CutPrefix(lines[last].text, "STATUS: ")
	if !ok || !statusTokenRE.MatchString(tok) {
		return "", errInvalidMarker
	}
	return tok, nil
}

// markerAllowed reports whether marker is legal for role and mode. A
// coder emits NEEDS_TRIAGE only in fix mode.
func markerAllowed(role, mode, marker string) bool {
	switch role + "/" + mode {
	case "designer/draft", "designer/revise":
		return marker == markerDesignDraft || marker == markerNeedsInput || marker == markerBlocked
	case "design-reviewer/audit":
		return marker == markerDesignChanges || marker == markerDesignApproved
	case "coder/implement":
		return marker == markerImplComplete || marker == markerQuestions || marker == markerNeedsDesign || marker == markerBlocked
	case "coder/fix":
		return marker == markerFixesComplete || marker == markerQuestions || marker == markerNeedsDesign || marker == markerNeedsTriage || marker == markerBlocked
	case "coder/" + modeDefect, "code-reviewer/" + modeReviewLight:
		// Design 12a-dev-flow-light: the light modes.
		return lightMarkerAllowed(role, mode, marker)
	case "code-reviewer/full":
		return marker == markerReviewApproved || marker == markerReviewChanges || marker == markerNeedsDesign
	case "triage/investigate":
		return marker == markerTriageConf || marker == markerTriageProb || marker == markerTriageUnconf || marker == markerTriageBlocked
	}
	return false
}

func looksLikeBlocker(line string) bool {
	return strings.HasPrefix(strings.ToUpper(line), "CALLSHEET_REVIEW_BLOCKER")
}

func looksLikeDesignRequired(line string) bool {
	return strings.HasPrefix(strings.ToLower(line), "design required")
}

// parseReviewBlocker requires exactly one exact blocker line in the final
// text and in the report, each immediately before its terminal STATUS
// line, with matching values. environment is legal only with
// REVIEW_CHANGES_REQUESTED.
func parseReviewBlocker(final, report, marker string) (string, error) {
	fv, err := oneBlocker(final)
	if err != nil {
		return "", err
	}
	rv, err := oneBlocker(report)
	if err != nil {
		return "", err
	}
	if fv != rv || (fv == blockerEnvironment && marker != markerReviewChanges) {
		return "", errInvalidField
	}
	return fv, nil
}

func oneBlocker(text string) (string, error) {
	lines := scanLines(text)
	hit, value := -1, ""
	for i, l := range lines {
		if !l.plain || !looksLikeBlocker(l.text) {
			continue
		}
		switch {
		case hit >= 0:
			return "", errInvalidField
		case l.text == "CALLSHEET_REVIEW_BLOCKER: environment":
			hit, value = i, blockerEnvironment
		case l.text == "CALLSHEET_REVIEW_BLOCKER: none":
			hit, value = i, blockerNone
		default:
			return "", errInvalidField
		}
	}
	if hit < 0 || hit+1 != terminalIndex(lines) {
		return "", errInvalidField
	}
	if _, err := parseTerminalMarker(text); err != nil {
		return "", errInvalidField
	}
	return value, nil
}

// parseDesignRequired returns the single exact Design required value of a
// confidence-qualified fix.md. Any other plain line that looks like the
// field is malformed.
func parseDesignRequired(fix string) (string, error) {
	value := ""
	for _, l := range scanLines(fix) {
		if !l.plain || !looksLikeDesignRequired(l.text) {
			continue
		}
		if value != "" {
			return "", errInvalidField
		}
		switch l.text {
		case "Design required: yes":
			value = designRequiredYes
		case "Design required: no":
			value = designRequiredNo
		default:
			return "", errInvalidField
		}
	}
	if value == "" {
		return "", errInvalidField
	}
	return value, nil
}

// parseConfidence returns the single exact Confidence value of a
// confidence-qualified fix.md: CONFIRMED or PROBABLE, on a plain field line
// "Confidence: CONFIRMED" or "Confidence: PROBABLE". Any other plain line
// that looks like the field (any case) is malformed.
func parseConfidence(fix string) (string, error) {
	value := ""
	for _, l := range scanLines(fix) {
		if !l.plain || !strings.HasPrefix(strings.ToLower(l.text), "confidence") {
			continue
		}
		if value != "" {
			return "", errInvalidField
		}
		switch l.text {
		case "Confidence: CONFIRMED":
			value = "CONFIRMED"
		case "Confidence: PROBABLE":
			value = "PROBABLE"
		default:
			return "", errInvalidField
		}
	}
	if value == "" {
		return "", errInvalidField
	}
	return value, nil
}

// fieldValues returns the values of every plain line starting with key
// (exact case), and whether any other plain line claims key in another
// spelling.
func fieldValues(text, key string) (values []string, malformed bool) {
	upper := strings.ToUpper(key)
	for _, l := range scanLines(text) {
		if !l.plain {
			continue
		}
		if v, ok := strings.CutPrefix(l.text, key+" "); ok {
			values = append(values, v)
			continue
		}
		if strings.HasPrefix(strings.ToUpper(l.text), upper) {
			malformed = true
		}
	}
	return values, malformed
}

// ownerLineKind classifies one owner terminal line. text is set only for
// an answer. A line that does not name the open generation is invalid
// and decides nothing.
func ownerLineKind(line string, generation int, answering bool) (kind, text string, ok bool) {
	line = strings.TrimSuffix(line, "\r")
	if answering {
		prefix := "answer " + itoa(generation) + " "
		body, found := strings.CutPrefix(line, prefix)
		if !found || !utf8.ValidString(body) || len(body) > triageTaskCeiling || strings.TrimSpace(body) == "" {
			return "", "", false
		}
		return "answer", body, true
	}
	switch line {
	case "yes " + itoa(generation):
		return "yes", "", true
	case "no " + itoa(generation):
		return "no", "", true
	}
	return "", "", false
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var b [24]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		b[i] = '-'
	}
	return string(b[i:])
}
