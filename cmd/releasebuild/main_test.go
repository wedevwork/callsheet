package main

import (
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/wedevwork/callsheet/internal/devcheck"
	"github.com/wedevwork/callsheet/internal/testkit"
)

func TestRunWrapper(t *testing.T) {
	var out, errOut bytes.Buffer
	if code := run(nil, &out, &errOut, devcheck.ReleaseDeps{}); code != 2 || !strings.Contains(errOut.String(), "usage: releasebuild --version VERSION --out DIRECTORY") {
		t.Fatalf("no args = %d %q", code, errOut.String())
	}
	// The wrapper passes the injected dependencies through: four builds, no
	// real compile, no other child.
	var calls [][]string
	deps := devcheck.ReleaseDeps{
		Run: func(_ context.Context, argv, env []string, _ string, _, _ io.Writer) error {
			calls = append(calls, argv)
			_, err := testkit.FakeGoBuild(argv, env)
			return err
		},
		Stamp: func(string) (string, error) { return "v0.0.0-rc.1", nil },
		Settings: func(p string) (string, map[string]string, error) {
			tg, err := devcheck.InspectExecutable(p)
			return "go-test", map[string]string{"GOOS": tg.GOOS, "GOARCH": tg.GOARCH, "CGO_ENABLED": "0", "-trimpath": "true"}, err
		},
	}
	dir := filepath.Join(t.TempDir(), "out")
	out.Reset()
	errOut.Reset()
	if code := run([]string{"--version", "v0.0.0-rc.1", "--out", dir}, &out, &errOut, deps); code != 0 {
		t.Fatalf("success = %d %s", code, errOut.String())
	}
	if len(calls) != 4 {
		t.Fatalf("calls = %v", calls)
	}
	entries, err := os.ReadDir(dir)
	if err != nil || len(entries) != 5 {
		t.Fatalf("entries = %v, %v", entries, err)
	}
	// A verification failure is exit 1.
	deps.Stamp = func(string) (string, error) { return "dev", nil }
	if code := run([]string{"--version", "v0.0.0-rc.1", "--out", filepath.Join(t.TempDir(), "out")}, &out, &errOut, deps); code != 1 {
		t.Fatalf("failure = %d", code)
	}
}
