package reale2e

import (
	"bytes"
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/scanner"
	"go/token"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// Changed-code coverage (design 12a-dev-flow-verify, Reviewer evidence,
// coverage scope). The scope is deterministic for this fixture: the
// initial seed main.go against the candidate main.go. Every added
// function, and every function whose declaration or body token stream
// changed (main included), selects each executable coverage block inside
// it; a changed package-level var declaration with an initializer selects
// its blocks (a function literal's). A removed function contributes
// nothing. Every profile block must lie inside one candidate declaration,
// or the derivation fails: nothing is excluded silently. The profile is a
// `mode: set` profile of the candidate module's main.go only; positions
// are validated against the candidate source, statement counts are
// positive (a zero-statement block is accepted only as an empty body's
// brace span, the shape the Go toolchain emits for `{}`), execution
// counts are nonnegative and blocks never overlap or repeat
// inconsistently. The threshold is integer arithmetic, 100*covered >
// 80*total with a nonzero denominator; the displayed percentage is
// truncated to one decimal and never decides it.

// DevCoverage is one changed-code coverage derivation.
type DevCoverage struct {
	Module        string   `json:"module"`
	Functions     []string `json:"functions"`
	Blocks        int      `json:"blocks"`
	Covered       int      `json:"covered"`
	Total         int      `json:"total"`
	Percent       string   `json:"percent"`
	Pass          bool     `json:"pass"`
	ProfileSHA256 string   `json:"profile_sha256"`
}

// coverBlock is one profile block.
type coverBlock struct {
	sl, sc, el, ec, stmts, count int
}

// coverPos is a (line, column) position.
type coverPos struct{ line, col int }

func (a coverPos) before(b coverPos) bool {
	return a.line < b.line || a.line == b.line && a.col < b.col
}

// region is one candidate top-level declaration's span.
type region struct {
	name       string
	start, end coverPos
	changed    bool
}

// errCoverage is a coverage derivation that cannot be trusted.
var errCoverage = errors.New("coverage unobserved")

func coverageErr(format string, a ...any) error {
	return fmt.Errorf("%w: %s", errCoverage, fmt.Sprintf(format, a...))
}

// profileLineRE is one profile block line.
var profileLineRE = regexp.MustCompile(`^(\S+):([0-9]+)\.([0-9]+),([0-9]+)\.([0-9]+) ([0-9]+) ([0-9]+)$`)

// parseCoverProfile reads a mode: set profile whose only file is file.
// Identical repeated blocks count once; a repeat that disagrees, or two
// distinct overlapping blocks, fails.
func parseCoverProfile(data []byte, file string) ([]coverBlock, error) {
	if len(data) == 0 || data[len(data)-1] != '\n' {
		return nil, coverageErr("the profile is empty or does not end with LF")
	}
	lines := strings.Split(strings.TrimSuffix(string(data), "\n"), "\n")
	if lines[0] != "mode: set" {
		return nil, coverageErr("the profile mode is not set")
	}
	seen := map[[4]int]coverBlock{}
	var out []coverBlock
	for _, l := range lines[1:] {
		m := profileLineRE.FindStringSubmatch(l)
		if m == nil {
			return nil, coverageErr("malformed profile line")
		}
		if m[1] != file {
			return nil, coverageErr("the profile names the unexpected file %s", m[1])
		}
		var n [6]int
		for i := range n {
			v, err := strconv.Atoi(m[i+2])
			if err != nil || len(m[i+2]) > 9 {
				return nil, coverageErr("malformed profile number")
			}
			n[i] = v
		}
		b := coverBlock{sl: n[0], sc: n[1], el: n[2], ec: n[3], stmts: n[4], count: n[5]}
		key := [4]int{b.sl, b.sc, b.el, b.ec}
		if prev, dup := seen[key]; dup {
			if prev != b {
				return nil, coverageErr("a repeated block disagrees")
			}
			continue
		}
		seen[key] = b
		out = append(out, b)
	}
	sort.Slice(out, func(i, j int) bool {
		a, b := coverPos{out[i].sl, out[i].sc}, coverPos{out[j].sl, out[j].sc}
		return a.before(b)
	})
	for i := 1; i < len(out); i++ {
		if (coverPos{out[i].sl, out[i].sc}).before(coverPos{out[i-1].el, out[i-1].ec}) {
			return nil, coverageErr("profile blocks overlap")
		}
	}
	return out, nil
}

// validateBlocks checks every block's positions against src and its
// counts.
func validateBlocks(blocks []coverBlock, src []byte) error {
	lines := strings.Split(string(src), "\n")
	inLine := func(l, c int) bool { return l >= 1 && l <= len(lines) && c >= 1 && c <= len(lines[l-1])+1 }
	for _, b := range blocks {
		start, end := coverPos{b.sl, b.sc}, coverPos{b.el, b.ec}
		switch {
		case !inLine(b.sl, b.sc) || !inLine(b.el, b.ec):
			return coverageErr("a block lies outside the candidate source")
		case !start.before(end):
			return coverageErr("a block ends before it starts")
		case b.count < 0:
			return coverageErr("a negative execution count")
		case b.stmts == 0 && !(b.sl == b.el && b.ec == b.sc+1 && lines[b.sl-1][b.sc-1] == '}'):
			return coverageErr("a block has no statements")
		}
	}
	return nil
}

// declRegions parses a main.go and returns its top-level declarations'
// spans with their token streams (comments excluded).
func declRegions(src []byte) ([]region, map[string]string, error) {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "main.go", src, parser.SkipObjectResolution)
	if err != nil {
		return nil, nil, err
	}
	var regs []region
	streams := map[string]string{}
	add := func(name string, from, to token.Pos) {
		s, e := fset.Position(from), fset.Position(to)
		regs = append(regs, region{name: name, start: coverPos{s.Line, s.Column}, end: coverPos{e.Line, e.Column}})
		streams[name] = tokenStream(src[s.Offset:e.Offset])
	}
	for _, d := range f.Decls {
		switch x := d.(type) {
		case *ast.FuncDecl:
			name := x.Name.Name
			if x.Recv != nil && len(x.Recv.List) == 1 {
				name = exprString(x.Recv.List[0].Type) + "." + name
			}
			add("func "+name, x.Pos(), x.End())
		case *ast.GenDecl:
			if x.Tok != token.VAR {
				continue
			}
			for _, sp := range x.Specs {
				vs, ok := sp.(*ast.ValueSpec)
				if !ok || len(vs.Values) == 0 {
					continue
				}
				var names []string
				for _, n := range vs.Names {
					names = append(names, n.Name)
				}
				add("var "+strings.Join(names, ","), vs.Pos(), vs.End())
			}
		}
	}
	return regs, streams, nil
}

// tokenStream renders src's tokens (no comments, no positions).
func tokenStream(src []byte) string {
	var s scanner.Scanner
	fset := token.NewFileSet()
	s.Init(fset.AddFile("", -1, len(src)), src, nil, 0)
	var b strings.Builder
	for {
		_, tok, lit := s.Scan()
		if tok == token.EOF {
			return b.String()
		}
		fmt.Fprintf(&b, "%d:%s\x00", tok, lit)
	}
}

// deriveChangedCoverage derives the changed-code coverage of candidate
// main.go from profile against the seed main.go. module is the candidate
// module path.
func deriveChangedCoverage(seedMain, candMain, profile []byte, module string) (DevCoverage, error) {
	cov := DevCoverage{Module: module, Functions: []string{}, ProfileSHA256: sha256Hex(profile)}
	if module == "" {
		return cov, coverageErr("no module path")
	}
	blocks, err := parseCoverProfile(profile, module+"/main.go")
	if err != nil {
		return cov, err
	}
	if err := validateBlocks(blocks, candMain); err != nil {
		return cov, err
	}
	regs, cand, err := declRegions(candMain)
	if err != nil {
		return cov, coverageErr("the candidate main.go does not parse")
	}
	_, seed, err := declRegions(seedMain)
	if err != nil {
		return cov, coverageErr("the seed main.go does not parse")
	}
	for i := range regs {
		old, had := seed[regs[i].name]
		regs[i].changed = !had || old != cand[regs[i].name]
		if regs[i].changed {
			cov.Functions = append(cov.Functions, regs[i].name)
		}
	}
	mapped := map[int]bool{}
	for _, b := range blocks {
		in := -1
		for i, r := range regs {
			if !(coverPos{b.sl, b.sc}).before(r.start) && !(r.end).before(coverPos{b.el, b.ec}) {
				in = i
				break
			}
		}
		if in < 0 {
			return cov, coverageErr("a profile block maps to no declaration")
		}
		mapped[in] = true
		if !regs[in].changed {
			continue
		}
		cov.Blocks++
		cov.Total += b.stmts
		if b.count > 0 {
			cov.Covered += b.stmts
		}
	}
	for i, r := range regs {
		if r.changed && !mapped[i] && strings.HasPrefix(r.name, "func ") {
			return cov, coverageErr("the changed %s has no coverage block", r.name)
		}
	}
	if cov.Total == 0 {
		return cov, coverageErr("no changed executable statement")
	}
	p10 := 1000 * cov.Covered / cov.Total
	cov.Percent = fmt.Sprintf("%d.%d", p10/10, p10%10)
	cov.Pass = 100*cov.Covered > 80*cov.Total
	return cov, nil
}

// modulePathOf is go.mod's module path ("" when it has none).
func modulePathOf(gomod []byte) string {
	for _, l := range bytes.Split(gomod, []byte("\n")) {
		f := strings.Fields(string(l))
		if len(f) == 2 && f[0] == "module" {
			return f[1]
		}
	}
	return ""
}
