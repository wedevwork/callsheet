package fakeadapter

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// TestFakeProbe is UT FP-2 for the fixture: --callsheet-probe is an
// exclusive mode (no other operand or flag), writes exactly ProbeOutput to
// stdout and nothing to stderr, exits 0, and touches no file, signal or
// descendant; every older mode is unchanged.
func TestFakeProbe(t *testing.T) {
	if o, err := parseFor([]string{ProbeFlag}, "linux", true); err != nil || !o.Probe {
		t.Fatalf("probe parse = %+v %v", o, err)
	}
	for _, args := range [][]string{
		{ProbeFlag, "extra"},
		{ProbeFlag, ProbeFlag},
		{"--model=m", ProbeFlag},
		{ProbeFlag, "--echo-argv"},
		{"-callsheet-probe"},
		{"--callsheet-probe=true"},
		{"--", ProbeFlag},
	} {
		o, err := parseFor(args, "linux", true)
		if _, usage := err.(*UsageError); err != nil && !usage {
			t.Fatalf("%v: %v", args, err)
		}
		if err == nil && o.Probe {
			t.Fatalf("%v accepted as a probe", args)
		}
	}
	// In process: exact output, empty stderr, exit 0, no signal
	// registration and no working-directory access.
	sigs := newFakeSignalsNoBlock()
	dir := t.TempDir()
	var out, errOut bytes.Buffer
	if code := Run(context.Background(), Env{Args: []string{ProbeFlag}, Stdout: &out, Stderr: &errOut, Dir: filepath.Join(dir, "absent"), Signals: sigs}); code != 0 {
		t.Fatalf("code = %d (%s)", code, errOut.String())
	}
	if out.String() != ProbeOutput || errOut.Len() != 0 || len(sigs.registered) != 0 {
		t.Fatalf("probe out %q err %q signals %d", out.String(), errOut.String(), len(sigs.registered))
	}
	if code := Run(context.Background(), Env{Args: []string{ProbeFlag, "x"}, Stdout: &out, Stderr: &errOut, Dir: dir, Signals: sigs}); code != 2 {
		t.Fatalf("probe with an operand = %d", code)
	}
	if code := Run(context.Background(), Env{Args: []string{ProbeFlag}}); code != 0 {
		t.Fatalf("probe without writers = %d", code)
	}
	// As a process (this test binary in helper mode): exact answer, cwd
	// left empty (no files, no descendants to outlive it).
	bin, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	cwd := t.TempDir()
	cmd := exec.Command(bin, ProbeFlag)
	cmd.Dir = cwd
	// A race-built helper otherwise sleeps a second at exit (iteration 03
	// fixture cost); the probe's behavior is unchanged.
	cmd.Env = append(os.Environ(), helperEnv+"=1", "GORACE=atexit_sleep_ms=0")
	var pout, perr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &pout, &perr
	if err := cmd.Run(); err != nil || pout.String() != ProbeOutput || perr.Len() != 0 {
		t.Fatalf("process probe: %v out %q err %q", err, pout.String(), perr.String())
	}
	if entries, _ := os.ReadDir(cwd); len(entries) != 0 {
		t.Fatalf("probe wrote %v", entries)
	}
	// Older modes are unchanged: a plain run still echoes as before.
	out.Reset()
	if code := Run(context.Background(), Env{Args: []string{"--stdout=hi"}, Stdout: &out, Dir: dir, Signals: newFakeSignalsNoBlock()}); code != 0 || out.String() != "hi\n" {
		t.Fatalf("plain run = %d %q", code, out.String())
	}
}
