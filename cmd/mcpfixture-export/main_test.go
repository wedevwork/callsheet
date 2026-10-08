package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Design decoder-enrollment B2 (FP-18): the thin entry point forwards its
// arguments to the command runner with the production policy only; an
// invalid invocation exits 2 and a source that is not one of the compiled
// pins exits 1 without writing any manifest. No real bundle is read.
func TestRunWrapper(t *testing.T) {
	var out, errOut bytes.Buffer
	if code := run(nil, &out, &errOut); code != 2 || !strings.Contains(errOut.String(), "usage: mcpfixture-export --source") {
		t.Fatalf("no flags = %d %q", code, errOut.String())
	}
	for _, args := range [][]string{{"--source", "rel", "--out", "/abs"}, {"--source", "/a", "--source", "/b", "--out", "/c"}, {"--plan", "/a"}, {"extra"}} {
		errOut.Reset()
		if code := run(args, &out, &errOut); code != 2 {
			t.Fatalf("%q = %d %q", args, code, errOut.String())
		}
	}
	// A fabricated bundle is not a pinned source: exit 1, no manifest.
	dir := t.TempDir()
	src := filepath.Join(dir, "src")
	if err := os.MkdirAll(src, 0o700); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(src, "manifest.json"), []byte("{}"), 0o600)
	dest := filepath.Join(dir, "out")
	errOut.Reset()
	if code := run([]string{"--source=" + src, "--out=" + dest}, &out, &errOut); code != 1 || !strings.HasPrefix(errOut.String(), "mcpfixture-export: ") {
		t.Fatalf("fabricated source = %d %q", code, errOut.String())
	}
	if _, err := os.Stat(filepath.Join(dest, "manifest.json")); err == nil {
		t.Fatal("a failed export published a manifest")
	}
}

// The production wiring (code review B2 round 1; the B1 DW10 precedent of a
// nil GrokPlacementFS): the command passes exactly the compiled production
// policy, whose placement view is nil (mcpqual's own tests pin that), and
// never a test policy or a placement view.
func TestProductionWiring(t *testing.T) {
	src, err := os.ReadFile("main.go")
	if err != nil {
		t.Fatal(err)
	}
	s := string(src)
	if strings.Count(s, "mcpqual.ProductionFixturePolicy()") != 1 || strings.Contains(s, "WithPlacementView") || strings.Contains(s, "FixturePolicyForTests") ||
		strings.Contains(s, "Getenv") || strings.Contains(s, "LookupEnv") {
		t.Fatal("cmd/mcpfixture-export does not pass exactly the production policy")
	}
}

// main exits with run's code (here an invalid invocation: no flags).
func TestMainExit(t *testing.T) {
	saved, args := exit, os.Args
	defer func() { exit, os.Args = saved, args }()
	got := -1
	exit = func(code int) { got = code }
	os.Args = []string{"mcpfixture-export"}
	main()
	if got != 2 {
		t.Fatalf("main exited %d", got)
	}
}
