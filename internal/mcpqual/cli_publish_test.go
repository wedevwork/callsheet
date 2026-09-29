package mcpqual

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// FP-14/FP-16: publication through Main (conflict exit 4, cleanup-failure
// refusal, interruption skips publication).
func TestMainPublish(t *testing.T) {
	t.Run("published", func(t *testing.T) {
		repo := tempRepo(t)
		w := newWorld(t, fullModel())
		out := filepath.Join(t.TempDir(), "o")
		env, _, _ := testEnv(t, w, "qualify", "--plan", writePlan(t, planWith(Phases{})), "--out", out, "--publish-catalog", repo)
		if r := run(env); r.code != 0 || !strings.Contains(r.stdout, "published run") {
			t.Fatalf("publish = %+v", r)
		}
		if _, err := os.Stat(filepath.Join(repo, EvidenceRoot)); err != nil {
			t.Fatal(err)
		}
		// The publish subcommand retries idempotently.
		env, _, _ = testEnv(t, w, "publish", "--out", out, "--repo", repo)
		if r := run(env); r.code != 0 {
			t.Fatalf("retry = %+v", r)
		}
		// An unrelated edit makes a later publication a conflict.
		p := filepath.Join(repo, CatalogMDPath)
		b, _ := os.ReadFile(p)
		os.WriteFile(p, append(b, '\n'), 0o644)
		env, _, _ = testEnv(t, w, "publish", "--out", out, "--repo", repo)
		if r := run(env); r.code != 4 || !strings.Contains(r.stderr, "conflict") {
			t.Fatalf("conflict = %+v", r)
		}
	})
	t.Run("bad-repo", func(t *testing.T) {
		w := newWorld(t, fullModel())
		env, _, _ := testEnv(t, w, "qualify", "--plan", writePlan(t, planWith(Phases{})), "--out", filepath.Join(t.TempDir(), "o"), "--publish-catalog", t.TempDir())
		if r := run(env); r.code != 2 || len(w.launches) != 0 {
			t.Fatalf("bad repo = %+v", r)
		}
	})
	t.Run("cleanup-failure", func(t *testing.T) {
		repo := tempRepo(t)
		w := newWorld(t, fullModel())
		env, _, _ := testEnv(t, w, "qualify", "--plan", writePlan(t, planWith(Phases{})), "--out", filepath.Join(t.TempDir(), "o"), "--publish-catalog", repo)
		sig, _ := SignalerFor(FaultValue, env.Signaler)
		env.Signaler = sig
		r := run(env)
		if r.code != 5 || !strings.Contains(r.stderr, "publication refused: cleanup failed") {
			t.Fatalf("cleanup failure = %+v", r)
		}
		if _, err := os.Stat(filepath.Join(repo, EvidenceRoot)); err == nil {
			t.Fatal("published despite a cleanup failure")
		}
	})
	t.Run("interrupted-not-published", func(t *testing.T) {
		repo := tempRepo(t)
		w := newWorld(t, fullModel())
		env, _, _ := testEnv(t, w, "qualify", "--plan", writePlan(t, planWith(Phases{})), "--out", filepath.Join(t.TempDir(), "o"), "--publish-catalog", repo)
		env.Notify = func(ctx context.Context) (context.Context, context.CancelFunc) {
			ctx, cancel := context.WithCancel(ctx)
			cancel()
			return ctx, cancel
		}
		if r := run(env); r.code != 130 || !strings.Contains(r.stderr, "publication skipped") {
			t.Fatalf("interrupted = %+v", r)
		}
	})
}
