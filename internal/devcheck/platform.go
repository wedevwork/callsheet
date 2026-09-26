package devcheck

import (
	"errors"
	"fmt"
	"go/ast"
	"go/build/constraint"
	"go/parser"
	"go/scanner"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

// The platform seam convention (docs/ci.md, "Platform code"): host wrappers
// may supply runtime.GOOS; every decision and OS-dependent formatting takes
// an explicit goos argument, so tests exercise linux and darwin on any host.
// CheckPlatformSources enforces it on the repository's non-test Go source.

// wrapperArg is one argument of an approved wrapper's single call.
type wrapperArg struct {
	// sel is "GOOS" or "GOARCH" for a runtime selector; otherwise name is an
	// identifier, which must be one of the wrapper's parameters if param.
	sel   string
	name  string
	param bool
}

func hostArg(sel string) wrapperArg  { return wrapperArg{sel: sel} }
func paramArg(n string) wrapperArg   { return wrapperArg{name: n, param: true} }
func packageArg(n string) wrapperArg { return wrapperArg{name: n} }

func (a wrapperArg) String() string {
	if a.sel != "" {
		return "runtime." + a.sel
	}
	return a.name
}

// platformWrapper is an approved host-OS read: the top-level function fn in
// file (slash-separated, relative to the root) whose entire body is one call
// of callee with exactly args, as a return statement (ret) or else as an
// expression statement.
type platformWrapper struct {
	file, fn string
	ret      bool
	callee   string
	args     []wrapperArg
}

func (w platformWrapper) shape() string {
	parts := make([]string, len(w.args))
	for i, a := range w.args {
		parts[i] = a.String()
	}
	call := w.callee + "(" + strings.Join(parts, ", ") + ")"
	if w.ret {
		return "return " + call
	}
	return call
}

// platformExemption is a build-selected native syscall/signal file exempt
// from the wrapper policy; it must keep exactly its build expression.
type platformExemption struct {
	file, build string
}

type platformPolicy struct {
	wrappers   []platformWrapper
	exemptions []platformExemption
}

// platformGuardPolicy is the fixed production policy: the only five approved
// wrappers and the only four exempt native files.
var platformGuardPolicy = platformPolicy{
	wrappers: []platformWrapper{
		{file: "internal/cli/cli.go", fn: "Run", ret: true, callee: "runFor",
			args: []wrapperArg{paramArg("ctx"), hostArg("GOOS"), paramArg("args"), paramArg("in"), paramArg("out"), paramArg("errOut")}},
		{file: "internal/devcheck/devcheck.go", fn: "Run", ret: true, callee: "runFor",
			args: []wrapperArg{paramArg("ctx"), hostArg("GOOS"), paramArg("args"), paramArg("out"), paramArg("errOut"), paramArg("run")}},
		{file: "internal/spikes/processgroup/experiment.go", fn: "evaluate", ret: false, callee: "evaluateFor",
			args: []wrapperArg{paramArg("r"), hostArg("GOOS")}},
		{file: "internal/spikes/processgroup/experiment.go", fn: "RunHelper", ret: true, callee: "runHelperFor",
			args: []wrapperArg{paramArg("getenv"), hostArg("GOOS"), hostArg("GOARCH")}},
		{file: "internal/testkit/fakeadapter/fakeadapter.go", fn: "Parse", ret: true, callee: "parseFor",
			args: []wrapperArg{paramArg("args"), hostArg("GOOS"), packageArg("signalsSupported")}},
	},
	exemptions: []platformExemption{
		{file: "internal/spikes/processgroup/sys_linux.go", build: "linux"},
		{file: "internal/spikes/processgroup/sys_darwin.go", build: "darwin"},
		{file: "internal/testkit/fakeadapter/signals_unix.go", build: "linux || darwin"},
		{file: "internal/testkit/fakeadapter/signals_other.go", build: "!linux && !darwin"},
	},
}

// skippedDirs are directory names that hold metadata, design documents,
// vendored code or fixtures rather than compiled project source. Every other
// dot-prefixed directory is skipped too.
var skippedDirs = map[string]bool{
	".git": true, ".agents": true, ".codex": true, ".claude": true, ".github": true,
	"design": true, "vendor": true, "testdata": true,
}

func skippedDir(name string) bool { return skippedDirs[name] || strings.HasPrefix(name, ".") }

// PlatformGuardError lists every violation, sorted by file and position.
type PlatformGuardError struct {
	Violations []string
}

func (e *PlatformGuardError) Error() string {
	return "devcheck: platform guard: " + strconv.Itoa(len(e.Violations)) + " violation(s) of the platform seam convention " +
		"(decisions take an explicit goos parameter; only the approved wrappers read runtime.GOOS; see docs/ci.md, Platform code):\n  " +
		strings.Join(e.Violations, "\n  ")
}

type violation struct {
	file      string
	line, col int
	msg       string
}

type platformScanner struct {
	policy   platformPolicy
	readFile func(string) ([]byte, error)
	found    map[string]int // wrapper key -> occurrences
	seen     map[string]bool
	out      []violation
}

// CheckPlatformSources scans every repository-owned non-test Go file under
// root, irrespective of build constraints, and fails unless all host-OS
// (runtime.GOOS) reads are the approved wrappers, each present exactly once
// with its exact shape, and the exempt native files keep their build
// expressions. It uses go/parser only and runs no tool or network command.
func CheckPlatformSources(root string) error {
	return checkPlatformSources(root, platformGuardPolicy, os.ReadFile)
}

func checkPlatformSources(root string, policy platformPolicy, readFile func(string) ([]byte, error)) error {
	if st, err := os.Stat(root); err != nil {
		return fmt.Errorf("devcheck: platform guard: %w", err)
	} else if !st.IsDir() {
		return fmt.Errorf("devcheck: platform guard: %s is not a directory", root)
	}
	s := &platformScanner{policy: policy, readFile: readFile, found: map[string]int{}, seen: map[string]bool{}}
	walkErr := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		rel := relPath(root, path)
		if err != nil {
			s.add(rel, 0, 0, "cannot traverse: %v", err)
			if d != nil && d.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		if path == root {
			return nil
		}
		name := d.Name()
		switch {
		case d.Type()&fs.ModeSymlink != 0:
			s.symlink(path, rel, name)
		case d.IsDir():
			if skippedDir(name) {
				return fs.SkipDir
			}
		case isSource(name):
			if !d.Type().IsRegular() {
				s.add(rel, 0, 0, "Go source is not a regular file")
				return nil
			}
			s.file(path, rel)
		}
		return nil
	})
	if walkErr != nil {
		s.add(".", 0, 0, "cannot traverse: %v", walkErr)
	}
	for _, w := range policy.wrappers {
		switch n := s.found[wrapperKey(w.file, w.fn)]; {
		case n == 0:
			s.add(w.file, 0, 0, "approved wrapper %s is missing: the policy requires exactly one top-level func %s whose body is %q (update the guard policy intentionally if it moved)", w.fn, w.fn, w.shape())
		case n > 1:
			s.add(w.file, 0, 0, "approved wrapper %s is declared %d times; exactly one is allowed", w.fn, n)
		}
	}
	for _, e := range policy.exemptions {
		if !s.seen[e.file] {
			s.add(e.file, 0, 0, "exempt native file is missing (policy requires it with //go:build %s)", e.build)
		}
	}
	if len(s.out) == 0 {
		return nil
	}
	sort.SliceStable(s.out, func(i, j int) bool {
		a, b := s.out[i], s.out[j]
		if a.file != b.file {
			return a.file < b.file
		}
		if a.line != b.line {
			return a.line < b.line
		}
		if a.col != b.col {
			return a.col < b.col
		}
		return a.msg < b.msg
	})
	e := &PlatformGuardError{}
	for _, v := range s.out {
		loc := v.file
		if v.line > 0 {
			loc += ":" + strconv.Itoa(v.line) + ":" + strconv.Itoa(v.col)
		}
		e.Violations = append(e.Violations, loc+": "+v.msg)
	}
	return e
}

func relPath(root, path string) string {
	rel, err := filepath.Rel(root, path)
	if err != nil {
		rel = path
	}
	return filepath.ToSlash(rel)
}

func isSource(name string) bool {
	return strings.HasSuffix(name, ".go") && !strings.HasSuffix(name, "_test.go")
}

func wrapperKey(file, fn string) string { return file + "\x00" + fn }

func (s *platformScanner) add(file string, line, col int, format string, args ...any) {
	s.out = append(s.out, violation{file: file, line: line, col: col, msg: fmt.Sprintf(format, args...)})
}

// symlink rejects a symlinked Go source or source directory: the guard never
// follows links and never silently omits what they point to.
func (s *platformScanner) symlink(path, rel, name string) {
	if isSource(name) {
		s.add(rel, 0, 0, "symlinked Go source is not allowed (the guard does not follow symlinks)")
		return
	}
	if strings.HasSuffix(name, ".go") || skippedDir(name) {
		return
	}
	if st, err := os.Stat(path); err == nil && st.IsDir() {
		s.add(rel, 0, 0, "symlinked source directory is not allowed (the guard does not follow symlinks)")
	}
}

func (s *platformScanner) file(path, rel string) {
	s.seen[rel] = true
	src, err := s.readFile(path)
	if err != nil {
		s.add(rel, 0, 0, "cannot read: %v", err)
		return
	}
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, rel, src, parser.ParseComments|parser.SkipObjectResolution)
	if err != nil {
		var list scanner.ErrorList
		if errors.As(err, &list) && len(list) > 0 {
			s.add(rel, list[0].Pos.Line, list[0].Pos.Column, "cannot parse: %s", list[0].Msg)
		} else {
			s.add(rel, 0, 0, "cannot parse: %v", err)
		}
		return
	}
	names := s.runtimeNames(fset, f, rel)
	exempt := s.exemption(fset, f, rel)
	approved := map[*ast.SelectorExpr]bool{}
	for _, w := range s.policy.wrappers {
		if w.file == rel {
			s.wrapper(fset, f, rel, w, names, approved)
		}
	}
	if exempt || len(names) == 0 {
		return
	}
	for _, decl := range f.Decls {
		where := declName(decl)
		ast.Inspect(decl, func(n ast.Node) bool {
			sel, ok := n.(*ast.SelectorExpr)
			if !ok || !isHostRead(sel, names, "GOOS") || approved[sel] {
				return true
			}
			p := fset.Position(sel.Pos())
			s.add(rel, p.Line, p.Column, "unapproved host OS read %s.GOOS in %s: take goos as a parameter and let an approved wrapper supply runtime.GOOS", sel.X.(*ast.Ident).Name, where)
			return true
		})
	}
}

// runtimeNames returns the identifiers under which f imports runtime. A dot
// import is rejected (it would allow unqualified host reads); a blank import
// reads nothing.
func (s *platformScanner) runtimeNames(fset *token.FileSet, f *ast.File, rel string) map[string]bool {
	names := map[string]bool{}
	for _, imp := range f.Imports {
		if path, err := strconv.Unquote(imp.Path.Value); err != nil || path != "runtime" {
			continue
		}
		switch {
		case imp.Name == nil:
			names["runtime"] = true
		case imp.Name.Name == ".":
			p := fset.Position(imp.Pos())
			s.add(rel, p.Line, p.Column, "dot import of runtime is not allowed: it hides host OS reads")
		case imp.Name.Name == "_":
		default:
			names[imp.Name.Name] = true
		}
	}
	return names
}

// isHostRead reports whether sel is <runtime import>.<field>. A local that
// shadows the import name is conservatively still treated as a host read.
func isHostRead(sel *ast.SelectorExpr, names map[string]bool, field string) bool {
	id, ok := sel.X.(*ast.Ident)
	return ok && names[id.Name] && sel.Sel.Name == field
}

// exemption reports whether rel is an exempt native file, recording a
// violation if its build expression differs from the policy's.
func (s *platformScanner) exemption(fset *token.FileSet, f *ast.File, rel string) bool {
	for _, e := range s.policy.exemptions {
		if e.file != rel {
			continue
		}
		want, _ := constraint.Parse("//go:build " + e.build)
		got := buildExpr(f)
		if got == nil || want == nil || got.String() != want.String() {
			have := "none"
			if got != nil {
				have = got.String()
			}
			s.add(rel, 0, 0, "exempt native file must keep //go:build %s (found %s): retagging it would exempt shared code", e.build, have)
		}
		return true
	}
	return false
}

// buildExpr returns f's //go:build expression, if any.
func buildExpr(f *ast.File) constraint.Expr {
	for _, cg := range f.Comments {
		if cg.Pos() >= f.Package {
			break
		}
		for _, c := range cg.List {
			if constraint.IsGoBuild(c.Text) {
				if x, err := constraint.Parse(c.Text); err == nil {
					return x
				}
			}
		}
	}
	return nil
}

// wrapper validates every top-level declaration of w.fn in f and marks the
// host reads of a correctly shaped wrapper as approved.
func (s *platformScanner) wrapper(fset *token.FileSet, f *ast.File, rel string, w platformWrapper, names map[string]bool, approved map[*ast.SelectorExpr]bool) {
	for _, decl := range f.Decls {
		fd, ok := decl.(*ast.FuncDecl)
		if !ok || fd.Recv != nil || fd.Name.Name != w.fn {
			continue
		}
		s.found[wrapperKey(w.file, w.fn)]++
		reads, reason := wrapperReads(fd, w, names)
		if reason != "" {
			p := fset.Position(fd.Pos())
			s.add(rel, p.Line, p.Column, "approved wrapper %s must be exactly %q (no branches, extra statements, closures or host aliases): %s", w.fn, w.shape(), reason)
			continue
		}
		for _, r := range reads {
			approved[r] = true
		}
	}
}

// wrapperReads checks fd against w's whole shape and returns its approved
// host selectors, or a reason the shape is wrong.
func wrapperReads(fd *ast.FuncDecl, w platformWrapper, names map[string]bool) ([]*ast.SelectorExpr, string) {
	if fd.Body == nil || len(fd.Body.List) != 1 {
		return nil, "the body must be a single statement"
	}
	var call *ast.CallExpr
	switch st := fd.Body.List[0].(type) {
	case *ast.ReturnStmt:
		if w.ret && len(st.Results) == 1 {
			call, _ = st.Results[0].(*ast.CallExpr)
		}
	case *ast.ExprStmt:
		if !w.ret {
			call, _ = st.X.(*ast.CallExpr)
		}
	}
	if call == nil {
		return nil, "the statement must be a direct call " + w.shape()
	}
	if id, ok := call.Fun.(*ast.Ident); !ok || id.Name != w.callee || call.Ellipsis.IsValid() {
		return nil, "it must call " + w.callee + " directly"
	}
	if len(call.Args) != len(w.args) {
		return nil, fmt.Sprintf("it must pass %d arguments, got %d", len(w.args), len(call.Args))
	}
	params := map[string]bool{}
	for _, field := range fd.Type.Params.List {
		for _, n := range field.Names {
			params[n.Name] = true
		}
	}
	var reads []*ast.SelectorExpr
	for i, want := range w.args {
		arg := call.Args[i]
		if want.sel != "" {
			sel, ok := arg.(*ast.SelectorExpr)
			if !ok || !isHostRead(sel, names, want.sel) {
				return nil, fmt.Sprintf("argument %d must be runtime.%s", i+1, want.sel)
			}
			if want.sel == "GOOS" {
				reads = append(reads, sel)
			}
			continue
		}
		id, ok := arg.(*ast.Ident)
		if !ok || id.Name != want.name || (want.param && !params[id.Name]) {
			return nil, fmt.Sprintf("argument %d must forward %s", i+1, want.name)
		}
	}
	return reads, ""
}

// declName names a top-level declaration for diagnostics.
func declName(d ast.Decl) string {
	fd, ok := d.(*ast.FuncDecl)
	if !ok {
		return "a package-level declaration"
	}
	if fd.Recv != nil && len(fd.Recv.List) == 1 {
		t := fd.Recv.List[0].Type
		if st, ok := t.(*ast.StarExpr); ok {
			t = st.X
		}
		if id, ok := t.(*ast.Ident); ok {
			return "method " + id.Name + "." + fd.Name.Name
		}
		return "method " + fd.Name.Name
	}
	return "func " + fd.Name.Name
}
