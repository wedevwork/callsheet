package workersmoke

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/wedevwork/callsheet/internal/contract"
	"github.com/wedevwork/callsheet/internal/testkit"
)

// Iteration 11's shell stand-ins: each records every launch (version or
// task) in launches.log beside itself. Grok's task mode validates the exact
// recipe argv with the observed smoke pair and an empty stdin, keeps the -p
// value and answers pong; Cursor's has no task mode: any task launch is
// recorded and fails. Since design 12a-worker-selection the Grok and
// Cursor stand-ins print versions newer than their minimum (eligible, so
// Cursor still reaches its posture refusal) and wrongCursor an older one.
const (
	fakeGrok = `#!/bin/sh
here=$(dirname "$0")
if [ "$#" = 1 ] && [ "$1" = --version ]; then echo version >> "$here/launches.log"; echo 'grok 1.0.47 (0123456789ab) [stable]'; exit 0; fi
stdin=$(cat)
if [ "$#" != 10 ] || [ "$1 $2 $3 $4 $5 $6 $7 $8 $9" != "--output-format json --model grok-4.7 --reasoning-effort low --permission-mode dontAsk -p" ] || [ -n "$stdin" ]; then
  echo "task bad" >> "$here/launches.log"; exit 97
fi
shift 9
printf '%s' "$1" > "$here/prompt.txt"
echo "task ok" >> "$here/launches.log"
printf '{"text":"pong","stopReason":"end_turn","sessionId":"s"}\n'
`
	fakeCursor = `#!/bin/sh
here=$(dirname "$0")
if [ "$#" = 1 ] && [ "$1" = --version ]; then echo version >> "$here/launches.log"; echo '2026.10.02-abcdef0'; exit 0; fi
echo task >> "$here/launches.log"
exit 97
`
	wrongCursor = `#!/bin/sh
here=$(dirname "$0")
echo version >> "$here/launches.log"
echo '2026.09.30-abcdef0'
`
)

// standIn writes script as name in its own directory (with a space).
func standIn(t *testing.T, root, dir, name, script string) string {
	t.Helper()
	p := filepath.Join(root, dir+" bin", name)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return p
}

// launchLog is a stand-in's launch records.
func launchLog(exe string) []string {
	b, _ := os.ReadFile(filepath.Join(filepath.Dir(exe), "launches.log"))
	return strings.Fields(strings.ReplaceAll(string(b), "task ok", "task-ok"))
}

// TestWave2Replay (iteration 11) runs the harness end to end with Grok and
// Cursor stand-ins found only through explicit paths: on Linux the Grok
// smoke completes through its exact recipe, on macOS it is the refusal
// smoke; Cursor is refused on both after a matching version probe, with
// zero task launches; the refusal helper never mistakes a wrong version or
// a successful registration for the posture refusal.
func TestWave2Replay(t *testing.T) {
	dir := t.TempDir()
	bin, err := testkit.BuildBinaryAt(dir, "./cmd/callsheet", "callsheet")
	if err != nil {
		t.Fatal(err)
	}
	grok := standIn(t, dir, "grok", "grok", fakeGrok)
	cursor := standIn(t, dir, "cursor", "cursor-agent", fakeCursor)
	wrong := standIn(t, dir, "wrong", "cursor-agent", wrongCursor)
	claude := standIn(t, dir, "claude", "claude", fakeClaude)
	// The stand-ins are reachable only through their explicit paths.
	env := []string{"PATH=/usr/bin:/bin", "HOME=" + t.TempDir()}
	// The injected gate picks them from the wave-2 variables.
	g := Gate{Getenv: func(k string) string {
		return map[string]string{EnvOptIn: "1", "CALLSHEET_GROK_PATH": grok, "CALLSHEET_CURSOR_PATH": cursor}[k]
	}, Stat: os.Stat}
	if g.Smoke().Run != true || g.Vendor("grok").Path != grok || !g.Vendor("grok").Run || g.Vendor("cursor").Path != cursor || !g.Vendor("cursor").Run {
		t.Fatalf("gate %+v %+v", g.Vendor("grok"), g.Vendor("cursor"))
	}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	var wg sync.WaitGroup
	var grokRes Result
	var grokRef, cursorRef Refusal
	var grokErr, cursorErr, wrongErr error
	wg.Go(func() {
		if Refused("grok", runtime.GOOS) {
			grokRef, grokErr = Refuse(ctx, bin, filepath.Join(dir, "grok"), env, env, "grok", grok, runtime.GOOS)
			return
		}
		grokRes, grokErr = Run(ctx, bin, filepath.Join(dir, "grok"), env, env, "grok", grok, "reply pong")
	})
	wg.Go(func() {
		cursorRef, cursorErr = Refuse(ctx, bin, filepath.Join(dir, "cursor"), env, env, "cursor", cursor, runtime.GOOS)
	})
	wg.Go(func() {
		_, wrongErr = Refuse(ctx, bin, filepath.Join(dir, "wrong"), env, env, "cursor", wrong, runtime.GOOS)
	})
	wg.Wait()
	switch runtime.GOOS {
	case "linux":
		if grokErr != nil || grokRes.View.State != contract.TaskSucceeded || grokRes.View.Result == nil || grokRes.View.Result.ExitCode == nil ||
			*grokRes.View.Result.ExitCode != 0 || grokRes.View.Result.FinalMessage == nil || *grokRes.View.Result.FinalMessage != "pong" {
			t.Fatalf("grok smoke %+v %v", grokRes.View, grokErr)
		}
		prompt, err := os.ReadFile(filepath.Join(filepath.Dir(grok), "prompt.txt"))
		if err != nil || !strings.HasPrefix(string(prompt), `{"format":"callsheet-task-v1","instruction":"`+strings.ReplaceAll(SmokeInstruction, "\n", `\n`)) ||
			!strings.Contains(string(prompt), `"goal":"reply pong"`) || len(prompt) > 32<<10 {
			t.Fatalf("grok -p value %q %v", prompt, err)
		}
		if l := launchLog(grok); len(l) < 2 || strings.Count(strings.Join(l, " "), "task-ok") != 1 || strings.Contains(strings.Join(l, " "), "bad") {
			t.Fatalf("grok launches %q", l)
		}
	default:
		if grokErr != nil || grokRef.Message != "grok worker execution is not qualified on this OS; consult the support catalog" {
			t.Fatalf("grok refusal %+v %v", grokRef, grokErr)
		}
		if l := launchLog(grok); len(l) == 0 || strings.Contains(strings.Join(l, " "), "task") {
			t.Fatalf("grok launches %q", l)
		}
	}
	if cursorErr != nil || cursorRef.Message != "cursor worker execution is refused: no qualified unattended recipe preserves the operator posture; consult the support catalog" ||
		!strings.Contains(cursorRef.Logs, "cursor adapter enabled for version probing only") {
		t.Fatalf("cursor refusal %+v %v", cursorRef, cursorErr)
	}
	if l := launchLog(cursor); len(l) == 0 || strings.Contains(strings.Join(l, " "), "task") {
		t.Fatalf("cursor launches %q (a refused vendor must only be version-probed)", l)
	}
	// An older version is a probe failure, never the posture refusal.
	if !errors.Is(wrongErr, ErrNotRefused) || !strings.Contains(wrongErr.Error(), "has an older cursor version; minimum 2026.10.01-e373342") {
		t.Fatalf("wrong cursor version: %v", wrongErr)
	}
	// The refusal helper on a shared deployment: an eligible pair is not a
	// refusal (no registration attempted), a successful registration is not
	// one, the posture refusal is; and AddVendorRole of a refused vendor
	// fails without becoming ready.
	d, err := Start(ctx, bin, filepath.Join(dir, "shared"), env)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	s, err := d.StartSidecar(ctx, "worker", env, "--claude-adapter", claude, "--cursor-adapter", cursor)
	if err != nil {
		t.Fatal(err)
	}
	ins, run, err := d.Manuals("shared", SmokeInstruction, SmokeRunbook)
	if err != nil {
		t.Fatal(err)
	}
	if err := d.RefuseVendorRole(ctx, "c1", "claude", s.NodeID, ins, run, runtime.GOOS); !errors.Is(err, ErrNotRefused) || !strings.Contains(err.Error(), "eligible") {
		t.Fatalf("eligible claude: %v", err)
	}
	if err := d.RefuseVendorRole(ctx, "u0", "unknown-vendor", s.NodeID, ins, run, runtime.GOOS); err == nil || err.Error() != "unknown-vendor is not a qualified vendor" {
		t.Fatalf("unknown vendor: %v", err)
	}
	if err := d.RefuseVendorRole(ctx, "u1", "cursor", s.NodeID, ins, run, runtime.GOOS); err != nil {
		t.Fatalf("cursor posture refusal: %v", err)
	}
	if err := d.AddVendorRole(ctx, "u2", "cursor", s.NodeID, ins, run, 1); err == nil || !strings.Contains(err.Error(), "cursor worker execution is refused") {
		t.Fatalf("cursor AddVendorRole: %v", err)
	}
	if err := d.AddVendorRole(ctx, "c2", "claude", s.NodeID, ins, run, 1); err != nil {
		t.Fatalf("claude beside cursor: %v", err)
	}
	if _, err := d.Client.ShowRole(ctx, "u1"); contract.CodeOf(err) != contract.CodeNotFound {
		t.Fatalf("the refused cursor role is visible: %v", err)
	}
	if l := launchLog(cursor); strings.Contains(strings.Join(l, " "), "task") {
		t.Fatalf("cursor launches %q", l)
	}
}
