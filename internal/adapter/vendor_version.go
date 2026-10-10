package adapter

import "strings"

// The worker version policy (design 12a-worker-selection): a vendor's
// --version output is eligible when it is a complete, orderable version
// line of that vendor at or above the observed baseline (the
// Qualification's Version, the exact historical probe output). It is a
// pure, bounded parser and comparator: no third-party dependency, no
// command beyond the existing --version probe, no filesystem or network.
// It is an eligibility policy, not vendor authentication and not a
// qualification of every later release; an accepted newer version is
// silent (no version-drift warning).
//
// Grammars (literal spaces significant), after at most one terminal LF or
// CRLF is removed:
//
//	claude  SEMVER (Claude Code)
//	codex   codex-cli SEMVER
//	grok    grok SEMVER (HASH) [CHANNEL]
//	cursor  YYYY.MM.DD-HASH
//
// SEMVER is major.minor.patch with optional -prerelease and +build, in
// SemVer precedence (build metadata never orders). HASH is 1-64 lowercase
// hex characters and CHANNEL 1-32 ASCII alphanumeric or hyphen characters:
// Grok's hash and channel are identity annotations, never ordering keys,
// and a Cursor date with a different hash is equal (no chronological
// relation between same-date hashes is knowable).

// Version line bounds.
const (
	// maxVersionLine bounds the complete version line, and the first-line
	// prefix that vendor recognition examines, in bytes.
	maxVersionLine = 256
	// maxSemverIdentifier bounds one prerelease or build identifier.
	maxSemverIdentifier = 64
	// maxVersionHash and maxGrokChannel bound the identity annotations.
	maxVersionHash = 64
	maxGrokChannel = 32
)

// Vendor version markers.
const (
	claudeVersionSuffix = " (Claude Code)"
	codexVersionPrefix  = "codex-cli "
	grokVersionPrefix   = "grok "
)

// workerVersion is one parsed, orderable version: a SemVer core with its
// prerelease identifiers, or a calendar date (year, month, day as the
// core, no prerelease). Hashes, channels and build metadata are validated
// and dropped: they never order.
type workerVersion struct {
	core [3]uint32
	pre  []string
}

// checkWorkerVersion is the vendor probe predicate for adapter id with the
// static baseline: "" when out is an eligible version, otherwise the fixed
// reason (id and baseline are trusted constants; out is never echoed). An
// invalid baseline refuses every output.
func checkWorkerVersion(id, baseline, out string) string {
	minimum, ok := parseWorkerVersion(id, baseline)
	if !ok {
		return "has no valid configured " + id + " minimum version"
	}
	if s, ok := strings.CutSuffix(out, "\r\n"); ok {
		out = s
	} else {
		out = strings.TrimSuffix(out, "\n")
	}
	if !recognizedVendor(id, out) {
		return "is not the " + id + " CLI (unexpected version output)"
	}
	v, ok := parseWorkerVersion(id, out)
	if !ok {
		return "has an invalid " + id + " version; expected an orderable version at or above " + baseline
	}
	if compareWorkerVersions(v, minimum) < 0 {
		return "has an older " + id + " version; minimum " + baseline
	}
	return ""
}

// recognizedVendor reports whether out's first line carries id's vendor
// marker, examining at most maxVersionLine bytes of it (the first line ends
// at the first CR or LF): Claude's " (Claude Code)" ending a first line
// that fits the bound, Codex's "codex-cli " and Grok's "grok " prefixes,
// and Cursor's YYYY.MM.DD- prefix of digits. Recognition grants no
// acceptance: it only tells an unorderable version of this vendor from
// another program's output.
func recognizedVendor(id, out string) bool {
	head := out[:min(len(out), maxVersionLine+1)]
	complete := len(out) <= maxVersionLine
	if i := strings.IndexAny(head, "\r\n"); i >= 0 {
		head, complete = head[:i], true
	}
	if !complete {
		head = head[:maxVersionLine]
	}
	switch id {
	case ClaudeID:
		return complete && strings.HasSuffix(head, claudeVersionSuffix)
	case CodexID:
		return strings.HasPrefix(head, codexVersionPrefix)
	case GrokID:
		return strings.HasPrefix(head, grokVersionPrefix)
	case CursorID:
		return len(head) >= 11 && dateShape(head[:11])
	}
	return false
}

// dateShape reports "DDDD.DD.DD-" with ASCII digits D.
func dateShape(s string) bool {
	return allDigits(s[0:4]) && s[4] == '.' && allDigits(s[5:7]) && s[7] == '.' && allDigits(s[8:10]) && s[10] == '-'
}

// parseWorkerVersion parses one complete version line of adapter id: at
// most maxVersionLine bytes of printable ASCII (no CR, LF, NUL or other
// control byte) in the vendor's exact grammar.
func parseWorkerVersion(id, line string) (workerVersion, bool) {
	if line == "" || len(line) > maxVersionLine {
		return workerVersion{}, false
	}
	for i := 0; i < len(line); i++ {
		if line[i] < 0x20 || line[i] > 0x7e {
			return workerVersion{}, false
		}
	}
	switch id {
	case ClaudeID:
		if sem, ok := strings.CutSuffix(line, claudeVersionSuffix); ok {
			return parseSemver(sem)
		}
	case CodexID:
		if sem, ok := strings.CutPrefix(line, codexVersionPrefix); ok {
			return parseSemver(sem)
		}
	case GrokID:
		return parseGrokVersion(line)
	case CursorID:
		return parseCursorVersion(line)
	}
	return workerVersion{}, false
}

// parseGrokVersion parses "grok SEMVER (HASH) [CHANNEL]".
func parseGrokVersion(line string) (workerVersion, bool) {
	rest, ok := strings.CutPrefix(line, grokVersionPrefix)
	if !ok {
		return workerVersion{}, false
	}
	if rest, ok = strings.CutSuffix(rest, "]"); !ok {
		return workerVersion{}, false
	}
	sem, annotations, ok := strings.Cut(rest, " (")
	if !ok {
		return workerVersion{}, false
	}
	hash, channel, ok := strings.Cut(annotations, ") [")
	if !ok || !validHash(hash) || channel == "" || len(channel) > maxGrokChannel || !identifierChars(channel) {
		return workerVersion{}, false
	}
	return parseSemver(sem)
}

// parseCursorVersion parses "YYYY.MM.DD-HASH": exactly four, two and two
// date digits of a valid Gregorian date with year 0001-9999, then a hash.
func parseCursorVersion(line string) (workerVersion, bool) {
	if len(line) < 12 || !dateShape(line[:11]) || !validHash(line[11:]) {
		return workerVersion{}, false
	}
	y, m, d := decimal(line[0:4]), decimal(line[5:7]), decimal(line[8:10])
	if y < 1 || m < 1 || m > 12 || d < 1 || d > daysIn(y, m) {
		return workerVersion{}, false
	}
	return workerVersion{core: [3]uint32{y, m, d}}, true
}

// daysIn is the number of days in month m of Gregorian year y.
func daysIn(y, m uint32) uint32 {
	switch m {
	case 2:
		if y%4 == 0 && (y%100 != 0 || y%400 == 0) {
			return 29
		}
		return 28
	case 4, 6, 9, 11:
		return 30
	}
	return 31
}

// parseSemver parses major.minor.patch[-prerelease][+build]: decimal
// uint32 core components without leading zeros (except 0 itself), and
// dot-separated nonempty identifiers of ASCII alphanumerics and hyphens,
// each at most maxSemverIdentifier bytes, numeric prerelease identifiers
// without leading zeros. There is no implicit v, partial version or fourth
// component.
func parseSemver(s string) (workerVersion, bool) {
	s, build, hasBuild := strings.Cut(s, "+")
	if hasBuild {
		for id := range strings.SplitSeq(build, ".") {
			if !validIdentifier(id) {
				return workerVersion{}, false
			}
		}
	}
	s, prerelease, hasPre := strings.Cut(s, "-")
	var v workerVersion
	if hasPre {
		v.pre = strings.Split(prerelease, ".")
		for _, id := range v.pre {
			if !validIdentifier(id) || allDigits(id) && len(id) > 1 && id[0] == '0' {
				return workerVersion{}, false
			}
		}
	}
	parts := strings.Split(s, ".")
	if len(parts) != 3 {
		return workerVersion{}, false
	}
	for i, p := range parts {
		n, ok := coreComponent(p)
		if !ok {
			return workerVersion{}, false
		}
		v.core[i] = n
	}
	return v, true
}

// coreComponent parses one decimal uint32 core component with no leading
// zero (except "0"); overflow is refused, never wrapped.
func coreComponent(p string) (uint32, bool) {
	if p == "" || len(p) > 10 || !allDigits(p) || len(p) > 1 && p[0] == '0' {
		return 0, false
	}
	var n uint64
	for i := 0; i < len(p); i++ {
		n = n*10 + uint64(p[i]-'0')
	}
	if n > 1<<32-1 {
		return 0, false
	}
	return uint32(n), true
}

// compareWorkerVersions orders a and b of one vendor (-1, 0, 1): the core
// numerically, then (SemVer) a release above its prereleases and
// prerelease identifiers pairwise: numeric below nonnumeric, numeric by
// length then lexically (bounded digit strings, no integer overflow),
// nonnumeric in ASCII order, and the shorter list first when every shared
// identifier is equal.
func compareWorkerVersions(a, b workerVersion) int {
	for i := range a.core {
		if a.core[i] != b.core[i] {
			return order(a.core[i] < b.core[i])
		}
	}
	switch {
	case len(a.pre) == 0 && len(b.pre) == 0:
		return 0
	case len(a.pre) == 0 || len(b.pre) == 0:
		return order(len(a.pre) > 0)
	}
	for i := 0; i < len(a.pre) && i < len(b.pre); i++ {
		x, y := a.pre[i], b.pre[i]
		if x == y {
			continue
		}
		xn, yn := allDigits(x), allDigits(y)
		switch {
		case xn != yn:
			return order(xn)
		case xn && len(x) != len(y):
			return order(len(x) < len(y))
		}
		return order(x < y)
	}
	if len(a.pre) == len(b.pre) {
		return 0
	}
	return order(len(a.pre) < len(b.pre))
}

// order is -1 when less, else 1.
func order(less bool) int {
	if less {
		return -1
	}
	return 1
}

// validIdentifier is a nonempty bounded SemVer identifier.
func validIdentifier(id string) bool {
	return id != "" && len(id) <= maxSemverIdentifier && identifierChars(id)
}

// identifierChars reports ASCII alphanumerics and hyphens only.
func identifierChars(s string) bool {
	for i := 0; i < len(s); i++ {
		c := s[i]
		if !('0' <= c && c <= '9' || 'a' <= c && c <= 'z' || 'A' <= c && c <= 'Z' || c == '-') {
			return false
		}
	}
	return true
}

// validHash is 1-maxVersionHash lowercase hex characters.
func validHash(s string) bool {
	if s == "" || len(s) > maxVersionHash {
		return false
	}
	for i := 0; i < len(s); i++ {
		if c := s[i]; !('0' <= c && c <= '9' || 'a' <= c && c <= 'f') {
			return false
		}
	}
	return true
}

// allDigits reports a nonempty string of ASCII digits.
func allDigits(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return true
}

// decimal is the value of a short ASCII digit string (a date field).
func decimal(s string) uint32 {
	var n uint32
	for i := 0; i < len(s); i++ {
		n = n*10 + uint32(s[i]-'0')
	}
	return n
}
