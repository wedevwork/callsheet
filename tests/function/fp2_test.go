package function

import (
	"os"
	"runtime"
	"strings"
	"testing"

	"github.com/wedevwork/callsheet/internal/cli"
	"github.com/wedevwork/callsheet/internal/testkit"
)

// TestFP2CommandTree drives the built callsheet binary through every leaf,
// group and help form plus invalid commands, in a fresh cwd and home, and
// confirms no files are created. The four plane leaves implemented in
// iteration 02 get help-only checks here (their behavior is FP-1 of
// iteration 02, TestPlaneCommands); every other leaf is a stub.
func TestFP2CommandTree(t *testing.T) {
	bin := testkit.BuildBinary(t, "./cmd/callsheet", "callsheet")
	home := t.TempDir()
	cwd := t.TempDir()
	env := []string{
		"PATH=" + os.Getenv("PATH"),
		"HOME=" + home,
		"XDG_CONFIG_HOME=" + home + "/.config",
		"XDG_STATE_HOME=" + home + "/.local/state",
		"XDG_CACHE_HOME=" + home + "/.cache",
	}
	run := func(args ...string) result { return runBin(t, bin, cwd, env, args...) }
	argsOf := func(c *cli.Command) []string { return strings.Fields(strings.TrimPrefix(c.Path(), "callsheet")) }
	tree := cli.NewTree(runtime.GOOS)

	t.Run("leaves", func(t *testing.T) {
		leaves := tree.Leaves()
		if len(leaves) != 32 {
			t.Fatalf("leaf count = %d", len(leaves))
		}
		implemented := map[string]bool{"callsheet plane init": true, "callsheet plane run": true, "callsheet plane status": true, "callsheet plane cert reissue": true}
		for _, leaf := range leaves {
			args := argsOf(leaf)
			if implemented[leaf.Path()] {
				help := run(append(args, "--help")...)
				if help.code != 0 || help.stderr != "" || !strings.HasPrefix(help.stdout, "Usage: "+leaf.Path()+" [--state-dir PATH]") ||
					!strings.Contains(help.stdout, "Status: implemented.") || strings.Contains(help.stdout, "future stub") {
					t.Fatalf("%v --help = %+v", args, help)
				}
				for _, other := range [][]string{append(args, "-h"), append([]string{"help"}, args...)} {
					if r := run(other...); r.code != 0 || r.stdout != help.stdout || r.stderr != "" {
						t.Fatalf("%v = %+v", other, r)
					}
				}
				continue
			}
			r := run(args...)
			if leaf.Name == "version" {
				if r.code != 0 || r.stdout != "callsheet dev protocol=1\n" || r.stderr != "" {
					t.Fatalf("version = %+v", r)
				}
			} else {
				want := "callsheet: not_implemented: \"" + leaf.Path() + "\" is not implemented yet\n"
				if r.code != 8 || r.stdout != "" || r.stderr != want {
					t.Fatalf("%v = %+v", args, r)
				}
				// Opaque future tokens are ignored, not accepted.
				if r2 := run(append(args, "--future", "x")...); r2.code != 8 || r2.stdout != "" {
					t.Fatalf("%v with tokens = %+v", args, r2)
				}
			}
			help := run(append(args, "--help")...)
			if help.code != 0 || help.stderr != "" || !strings.HasPrefix(help.stdout, "Usage: "+leaf.Path()+"\n") {
				t.Fatalf("%v --help = %+v", args, help)
			}
			for _, other := range [][]string{append(args, "-h"), append([]string{"help"}, args...)} {
				if r := run(other...); r.code != 0 || r.stdout != help.stdout || r.stderr != "" {
					t.Fatalf("%v = %+v", other, r)
				}
			}
			if leaf.Name != "version" && !strings.Contains(help.stdout, "future stub") {
				t.Fatalf("help does not mark %s as a future stub", leaf.Path())
			}
		}
	})

	t.Run("groups", func(t *testing.T) {
		for _, g := range tree.Groups() {
			args := argsOf(g)
			r := run(args...)
			if r.code != 0 || r.stderr != "" || !strings.HasPrefix(r.stdout, "Usage: "+g.Path()+" <command>\n") {
				t.Fatalf("%v = %+v", args, r)
			}
			for _, c := range g.Children {
				if !strings.Contains(r.stdout, "\n  "+c.Name+" ") {
					t.Fatalf("%v help lacks child %s", args, c.Name)
				}
			}
			forms := [][]string{append(args, "--help"), append(args, "-h"), append([]string{"help"}, args...)}
			for _, f := range forms {
				if r2 := run(f...); r2.code != 0 || r2.stdout != r.stdout || r2.stderr != "" {
					t.Fatalf("%v = %+v", f, r2)
				}
			}
		}
		if r := run("--version"); r.code != 0 || r.stdout != "callsheet dev protocol=1\n" {
			t.Fatalf("--version = %+v", r)
		}
	})

	t.Run("invalid", func(t *testing.T) {
		for _, args := range [][]string{
			{"bogus"}, {"task", "bogus"}, {"ws", "ref", "bogus"}, {"--bogus"}, {"-x", "task", "ls"},
			{"task", "--state-dir", "/x", "ls"}, {"help", "bogus"}, {"help", "task", "bogus"}, {"version", "extra"},
		} {
			r := run(args...)
			if r.code != 2 || r.stdout != "" || !strings.HasPrefix(r.stderr, "callsheet: invalid_argument: ") || !strings.Contains(r.stderr, "Usage: ") {
				t.Fatalf("%v = %+v", args, r)
			}
		}
	})

	t.Run("platform tree", func(t *testing.T) {
		for _, g := range []string{"plane", "sidecar"} {
			r := run(g)
			if r.code != 0 || r.stderr != "" || !strings.HasPrefix(r.stdout, "Usage: callsheet "+g+" <command>\n") {
				t.Fatalf("%s = %+v", g, r)
			}
			if h := run("help", g); h.code != 0 || h.stderr != "" || h.stdout != r.stdout {
				t.Fatalf("help %s = %+v", g, h)
			}
		}
	})

	t.Run("mcp stub keeps stdout clean", func(t *testing.T) {
		r := run("mcp", "--plane", "https://127.0.0.1:1", "--ca", "ca.pem")
		if r.code != 8 || r.stdout != "" || !strings.Contains(r.stderr, "not implemented yet") {
			t.Fatalf("mcp = %+v", r)
		}
	})

	if files := listTree(t, cwd); len(files) != 0 {
		t.Fatalf("commands created files in cwd: %v", files)
	}
	if files := listTree(t, home); len(files) != 0 {
		t.Fatalf("commands created files in home: %v", files)
	}
}
