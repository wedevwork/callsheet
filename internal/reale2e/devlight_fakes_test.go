package reale2e

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
)

// A semantic fake Go toolchain for the light recipe (design
// 12a-dev-flow-light; the rig must behave like the real tools so offline
// tests cannot hide live behavior). Its go test reads the exported
// main_test.go table and main.go and evaluates every row against the
// greeting program's actual semantics (the seeded TrimSpace defect makes
// whitespace exit 2 with the empty-name diagnostic); its JSON stream has
// real event shapes and exit codes; its build writes a binary stand-in
// that remembers the built source, whose CLI cases then behave as that
// source would. The real-toolchain tests check the same paths with Go
// itself.

// fakeLightRow is one scripted TestRun table row.
type fakeLightRow struct {
	id     string
	args   []string
	code   int
	stdout string
	stderr bool
}

// fakeLightRows parses a main_test.go table in the fixture's row shape
// {id, args, code, stdout, stderr}.
func fakeLightRows(test []byte) []fakeLightRow {
	var out []fakeLightRow
	for _, r := range tableRows(test) {
		row := fakeLightRow{}
		var strs []string
		var ints []int
		for _, l := range r.literals {
			switch {
			case strings.HasPrefix(l, `"`):
				if v, err := strconv.Unquote(l); err == nil {
					strs = append(strs, v)
				}
			case l == "true" || l == "false":
				row.stderr = l == "true"
			default:
				if n, err := strconv.Atoi(l); err == nil {
					ints = append(ints, n)
				}
			}
		}
		if len(r.args) > 0 {
			row.args = r.args[0]
			// The argument literals are also string literals of the row.
			strs = slices.DeleteFunc(strs, func(s string) bool { return slices.Contains(row.args, s) && s != strs[0] })
		}
		if len(strs) > 0 {
			row.id = strs[0]
		}
		if len(strs) > 1 {
			row.stdout = strs[len(strs)-1]
		}
		if len(ints) > 0 {
			row.code = ints[0]
		}
		out = append(out, row)
	}
	return out
}

// fakeGreeting is the greeting program's behavior for args given its
// main.go: the conforming result, except the seeded defect's whitespace
// refusal.
func fakeGreeting(main []byte, args []string) (int, string, string) {
	defect := bytes.Contains(main, []byte(`strings.TrimSpace(*name) == ""`))
	if defect && slices.Equal(args, []string{"--name", " "}) {
		return 2, "", lightWhitespaceStderr
	}
	for _, c := range FeatureCases {
		if slices.Equal(c.Args, args) {
			b := conformingCase(c)
			return b.exit, b.stdout, b.stderr
		}
	}
	return 2, "", "greeting: invalid arguments\n"
}

// fakeLightTests evaluates the exported table: each row's verdict.
func fakeLightTests(dir string) (rows []fakeLightRow, pass []bool, ok bool) {
	main, err1 := os.ReadFile(filepath.Join(dir, "main.go"))
	test, err2 := os.ReadFile(filepath.Join(dir, "main_test.go"))
	if err1 != nil || err2 != nil {
		return nil, nil, false
	}
	rows = fakeLightRows(test)
	for _, r := range rows {
		code, stdout, stderr := fakeGreeting(main, r.args)
		pass = append(pass, code == r.code && stdout == r.stdout && (stderr != "") == r.stderr)
	}
	return rows, pass, len(rows) > 0
}

// fakeLightJSON is the go test -json stream of an exported table and its
// exit code.
func fakeLightJSON(dir string) (string, int) {
	rows, pass, ok := fakeLightTests(dir)
	if !ok {
		return `{"Action":"start","Package":"example.com/greeting"}` + "\n" + `{"Action":"output","Package":"example.com/greeting","Output":"# example.com/greeting\n"}` + "\n" +
			`{"Action":"fail","Package":"example.com/greeting","Elapsed":0,"FailedBuild":"example.com/greeting"}` + "\n", 1
	}
	var b strings.Builder
	pkg := "example.com/greeting"
	fmt.Fprintf(&b, `{"Action":"start","Package":"%s"}`+"\n", pkg)
	fmt.Fprintf(&b, `{"Action":"run","Package":"%s","Test":"TestRun"}`+"\n", pkg)
	all := true
	for i, r := range rows {
		name := "TestRun/" + r.id
		fmt.Fprintf(&b, `{"Action":"run","Package":"%s","Test":"%s"}`+"\n", pkg, name)
		action := "pass"
		if !pass[i] {
			action, all = "fail", false
			out, _ := json.Marshal(map[string]any{"Action": "output", "Package": pkg, "Test": name, "Output": fmt.Sprintf("    main_test.go:30: run(%q) failed\n", r.args)})
			b.Write(append(out, '\n'))
		}
		fmt.Fprintf(&b, `{"Action":"%s","Package":"%s","Test":"%s","Elapsed":0}`+"\n", action, pkg, name)
	}
	final := "pass"
	if !all {
		final = "fail"
	}
	fmt.Fprintf(&b, `{"Action":"%s","Package":"%s","Test":"TestRun","Elapsed":0}`+"\n", final, pkg)
	fmt.Fprintf(&b, `{"Action":"%s","Package":"%s","Elapsed":0.01}`+"\n", final, pkg)
	if all {
		return b.String(), 0
	}
	return b.String(), 1
}

// fakeLightSuite is the plain go test output of an exported table.
func fakeLightSuite(dir string) behavior {
	_, pass, ok := fakeLightTests(dir)
	if ok && !slices.Contains(pass, false) {
		return behavior{stdout: "ok  \texample.com/greeting\t0.01s\n"}
	}
	return behavior{stdout: "--- FAIL: TestRun\nFAIL\nFAIL\texample.com/greeting\t0.01s\n", exit: 1}
}

// fakeBinaryMark is the built binary stand-in's content prefix.
const fakeBinaryMark = "fake-greeting-binary\n"

// lightGoBehavior is the semantic fake for the light recipe's commands
// (ok false: not a light command).
func lightGoBehavior(s ProcSpec) (behavior, bool) {
	a := strings.Join(s.Args, " ")
	switch {
	case a == "test -count=1 ./...":
		return fakeLightSuite(s.Dir), true
	case a == "test -count=1 -json ./...":
		stream, exit := fakeLightJSON(s.Dir)
		return behavior{stdout: stream, exit: exit}, true
	case len(s.Args) == 5 && strings.Join(s.Args[:3], " ") == "test -count=1 -covermode=set" && strings.HasPrefix(s.Args[3], "-coverprofile=") &&
		s.Args[4] == "./...":
		out, dir := strings.TrimPrefix(s.Args[3], "-coverprofile="), s.Dir
		b := fakeLightSuite(dir)
		b.after = func() {
			main, _ := os.ReadFile(filepath.Join(dir, "main.go"))
			gomod, _ := os.ReadFile(filepath.Join(dir, "go.mod"))
			os.WriteFile(out, synthProfile(modulePathOf(gomod), main), 0o600)
		}
		return b, true
	case len(s.Args) == 5 && strings.Join(s.Args[:3], " ") == "build -buildvcs=false -o" && s.Args[4] == "." && fileExists(filepath.Join(s.Dir, "main.go")):
		bin, dir := s.Args[3], s.Dir
		return behavior{after: func() {
			main, _ := os.ReadFile(filepath.Join(dir, "main.go"))
			os.WriteFile(bin, append([]byte(fakeBinaryMark), main...), 0o700)
		}}, true
	}
	if b, err := os.ReadFile(s.Path); err == nil && bytes.HasPrefix(b, []byte(fakeBinaryMark)) {
		code, stdout, stderr := fakeGreeting(b[len(fakeBinaryMark):], s.Args)
		return behavior{exit: code, stdout: stdout, stderr: stderr}, true
	}
	return behavior{}, false
}

// fileExists reports a regular file at p.
func fileExists(p string) bool {
	st, err := os.Stat(p)
	return err == nil && st.Mode().IsRegular()
}
