package mcpqual

import (
	"bytes"
	"context"
	"encoding/json"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

// Code review B3 round 2, C1: every filesystem or process operation of
// capture and qualify that is followed by a launch or a completion verdict
// re-checks cancellation and the deadline first. The regressions cancel the
// run, or spend the client's budget on the injected fake clock, inside one
// operation through the existing seams (the approval filesystem view, the
// reaper), then assert no later launch and no complete or Qualified result.

// hookFS wraps an approval view: onOpen runs on the nth open of a path
// ending in suffix, onList on the nth listing of dir (every listing when dn
// is negative).
type hookFS struct {
	approvalFS
	suffix string
	n      int
	onOpen func()
	dir    string
	dn     int
	onList func()
	mu     sync.Mutex
	opens  int
	lists  int
}

func (f *hookFS) Open(p string) (approvalFile, error) {
	if f.onOpen != nil && strings.HasSuffix(p, f.suffix) {
		f.mu.Lock()
		f.opens++
		hit := f.opens == f.n
		f.mu.Unlock()
		if hit {
			f.onOpen()
		}
	}
	return f.approvalFS.Open(p)
}

func (f *hookFS) ReadDir(p string) ([]fs.DirEntry, error) {
	if f.onList != nil && p == f.dir {
		f.mu.Lock()
		f.lists++
		hit := f.lists == f.dn || f.dn < 0
		f.mu.Unlock()
		if hit {
			f.onList()
		}
	}
	return f.approvalFS.ReadDir(p)
}

// launchKinds are the launch kinds of w in order (without client prefix).
func (w *capWorld) launchKinds() []string {
	w.mu.Lock()
	defer w.mu.Unlock()
	var out []string
	for _, s := range w.launches {
		out = append(out, launchKind(s))
	}
	return out
}

// cursorCapture is one capture of the pinned Cursor adapter (a fixture home
// with its projects directory), its view wrapped by hook (when set), on
// ctx, the runner adjusted by mutate. It returns the manifest, the client
// and the launch kinds.
func cursorCapture(t *testing.T, ctx context.Context, hook func(c *CaptureRunner, w *capWorld, home string) *hookFS, mutate func(c *CaptureRunner, w *capWorld)) (*CaptureManifest, CaptureClient, []string) {
	t.Helper()
	w := newCapWorld(t)
	home := filepath.Join(t.TempDir(), "home")
	os.MkdirAll(filepath.Join(home, ".cursor", "projects"), 0o700)
	c := newCapRunner(t, w, capPlan(t, "cursor"))
	c.Home = home
	if hook != nil {
		h := hook(c, w, home)
		h.approvalFS = c.approvalFS
		c.approvalFS = h
	}
	if mutate != nil {
		mutate(c, w)
	}
	man, b := runCapture(t, c, ctx)
	if err := man.Validate(); err != nil {
		t.Fatal(err)
	}
	if _, err := ValidateCaptureBundle(os.DirFS(c.OutDir), "."); err != nil {
		t.Fatalf("bundle: %v (%v)", err, b != nil)
	}
	return man, capClient(t, man, "cursor"), w.launchKinds()
}

func TestCaptureCursorDeadlines(t *testing.T) {
	const clientBudget = 181 * time.Second
	permission := "/.cursor/cli.json"
	// The baseline: complete, verified, with its residue checked.
	man, cc, kinds := cursorCapture(t, context.Background(), nil, nil)
	if man.State != CaptureComplete || cc.ToolPermission.State != PermissionVerified || cc.WorkerResidue.State != ResidueVerified ||
		!slices.Equal(kinds, []string{"version", "help", "enable", "session"}) {
		t.Fatalf("baseline %s %q %v", man.State, deref(cc.Reason), kinds)
	}
	for name, tc := range map[string]struct {
		cancel  bool
		hook    func(fire func()) *hookFS
		reason  string
		kinds   []string
		residue string // "" when no residue record is expected checked
	}{
		// Reviewer round 2: a cancellation during the approval pre-scan (the
		// first inventory's read of the permission file) launches no enable.
		"cancel-approval-pre-scan": {true, func(fire func()) *hookFS { return &hookFS{suffix: permission, n: 1, onOpen: fire} },
			ReasonInterrupted, []string{"version", "help"}, ResidueNotChecked},
		"cancel-after-enable-read": {true, func(fire func()) *hookFS { return &hookFS{suffix: permission, n: 3, onOpen: fire} },
			ReasonInterrupted, []string{"version", "help", "enable"}, ResidueNotChecked},
		"cancel-before-model": {true, func(fire func()) *hookFS { return &hookFS{suffix: permission, n: 4, onOpen: fire} },
			ReasonInterrupted, []string{"version", "help", "enable"}, ResidueNotChecked},
		// (A3.1: the projects walk of the pre-launch residue baseline is the
		// first to observe the expiry and records its fixed reason.)
		"budget-before-model": {false, func(fire func()) *hookFS { return &hookFS{suffix: permission, n: 4, onOpen: fire} },
			ReasonForeignDeadline, []string{"version", "help", "enable"}, ResidueNotChecked},
		// Reviewer round 2: the final permission read spends the client's
		// 180 s: partial, the residue not checked, the evidence kept.
		"budget-final-permission-read": {false, func(fire func()) *hookFS { return &hookFS{suffix: permission, n: 5, onOpen: fire} },
			ReasonBudget + ": max_client_ms", []string{"version", "help", "enable", "session"}, ResidueNotChecked},
		"cancel-final-permission-read": {true, func(fire func()) *hookFS { return &hookFS{suffix: permission, n: 5, onOpen: fire} },
			ReasonInterrupted, []string{"version", "help", "enable", "session"}, ResidueNotChecked},
		// The residue check itself spends the budget: partial (fired at the
		// projects root's first listing after the model launch, the check's
		// own baseline listing).
		"budget-residue-check": {false, func(fire func()) *hookFS { return &hookFS{dn: -1, onList: fire} },
			ReasonBudget + ": max_client_ms", []string{"version", "help", "enable", "session"}, ""},
	} {
		ctx, cancel := context.WithCancel(context.Background())
		man, cc, kinds := cursorCapture(t, ctx, func(c *CaptureRunner, w *capWorld, home string) *hookFS {
			fire := func() { w.clock.Advance(clientBudget) }
			if tc.cancel {
				fire = cancel
			}
			if name == "budget-residue-check" {
				once, advance := false, fire
				fire = func() {
					if !once && slices.Contains(w.launchKinds(), "session") {
						once = true
						advance()
					}
				}
			}
			h := tc.hook(fire)
			h.dir = filepath.Join(home, ".cursor", "projects")
			return h
		}, nil)
		cancel()
		if man.State == CaptureComplete || cc.State == CaptureComplete || deref(cc.Reason) != tc.reason || !slices.Equal(kinds, tc.kinds) {
			t.Errorf("%s: %s/%s %q %v", name, man.State, cc.State, deref(cc.Reason), kinds)
			continue
		}
		if cc.WorkerResidue == nil || tc.residue != "" && cc.WorkerResidue.State != tc.residue {
			t.Errorf("%s: residue %+v", name, cc.WorkerResidue)
		}
		if slices.Contains(tc.kinds, "session") && (cc.Session.State != StageRan || len(cc.Streams) == 0) {
			t.Errorf("%s: the session's evidence was not kept: %+v", name, cc.Session)
		}
		if name == "cancel-approval-pre-scan" && (cc.Approval.Stage.State != StageNotRun || deref(cc.Approval.Stage.Reason) != ReasonInterrupted) {
			t.Errorf("%s: approval stage %+v", name, cc.Approval.Stage)
		}
	}
	// A cancellation after the version command launches no help.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	_, cc, kinds = cursorCapture(t, ctx, nil, func(c *CaptureRunner, w *capWorld) {
		w.onReap = func(spec ProcSpec) {
			if launchKind(spec) == "version" {
				cancel()
			}
		}
	})
	if !slices.Equal(kinds, []string{"version"}) || cc.Help.State != StageNotRun || deref(cc.Help.Reason) != ReasonInterrupted {
		t.Fatalf("help after cancellation: %v %+v", kinds, cc.Help)
	}
}

func TestQualifyCursorCancellation(t *testing.T) {
	permission := "/.cursor/cli.json"
	for name, tc := range map[string]struct {
		n     int
		kinds []string
	}{
		// Reviewer round 2: cancellation during the approval pre-scan.
		"approval-pre-scan":  {1, []string{"version", "help"}},
		"approval-post-scan": {2, []string{"version", "help", "enable"}},
		"after-enable-read":  {3, []string{"version", "help", "enable"}},
	} {
		ctx, cancel := context.WithCancel(context.Background())
		q := cursorQualify(t, nil, func(q *cqRun) {
			q.ctx = ctx
			q.r.approvalFS = &openHookFS{suffix: permission, n: tc.n, mutate: func(string) { cancel() }}
		})
		cancel()
		cases := cqCases(q.rep)
		if q.err != nil || len(cases) != 1 || cases[0].Outcome != OutcomeInterrupted || deref(cases[0].Reason) != ReasonInterrupted ||
			!slices.Equal(q.kinds(), tc.kinds) || q.rep.Outcome == StatusConclusive {
			t.Errorf("%s: %v %+v %v", name, q.err, cases, q.kinds())
			continue
		}
		if p := cases[0].CursorPreparation; p == nil || p.State != PreparationUnverified {
			t.Errorf("%s: preparation %+v", name, p)
		}
		if tc.n == 1 && (cases[0].CursorPreparation.Approval.Stage.State != StageNotRun || deref(cases[0].CursorPreparation.Approval.Stage.Reason) != ReasonInterrupted) {
			t.Errorf("%s: approval stage %+v", name, cases[0].CursorPreparation.Approval.Stage)
		}
	}
	// A cancellation after the version command launches no help.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	q := cursorQualify(t, nil, func(q *cqRun) {
		q.ctx = ctx
		q.w.onReap = func(spec ProcSpec) {
			if launchKind(spec) == "version" {
				cancel()
			}
		}
	})
	if c := q.rep.Clients[0]; !slices.Equal(q.kinds(), []string{"version"}) || deref(c.Reason) != ReasonInterrupted {
		t.Fatalf("help after cancellation: %v %q", q.kinds(), deref(c.Reason))
	}
}

// Design decoder-enrollment A3, capture parity: capture obtains the case's
// approval baseline from its own approval step and applies the same three
// outcomes: a complete capture with an in_place entry (nothing of the
// session files in the bundle), and mixed, partial or workspace-only
// in-place trees failing closed as a partial capture.
func TestCaptureCursorResidueLayouts(t *testing.T) {
	for name, tc := range map[string]struct {
		layout    string
		workspace bool // workspace-only approval: no in-place allowance
		complete  bool
		want      string
	}{
		"in_place":       {"in_place", false, true, ""},
		"none":           {"", false, true, ""},
		"mixed":          {"mixed", false, false, "both residue layouts"},
		"partial":        {"partial", false, false, "not the exact in-place layout"},
		"workspace-only": {"in_place", true, false, ReasonForeignNew},
	} {
		man, cc, kinds := cursorCapture(t, context.Background(), nil, func(c *CaptureRunner, w *capWorld) {
			residueWorld(t, w, c.Home, func(string) string { return tc.layout })
			if tc.workspace {
				// The enable approves the workspace only; the session creates the
				// project directory with the whole tree, approval included.
				enable := w.script
				w.script = func(spec ProcSpec) capBehavior {
					if launchKind(spec) == "enable" {
						return w.defaults(spec)
					}
					return enable(spec)
				}
				reap := w.onReap
				w.onReap = func(spec ProcSpec) {
					if launchKind(spec) == "session" {
						slug, _ := cursorProjectSlug(osApprovalFS{}, "linux", "amd64", CursorRealVersion, spec.Dir)
						os.MkdirAll(filepath.Join(c.Home, ".cursor", "projects", slug), 0o700)
						os.WriteFile(filepath.Join(c.Home, ".cursor", "projects", slug, cursorApprovalsFile), []byte(`["probe-6e58c4b6c129cbd0"]`), 0o600)
					}
					reap(spec)
				}
			}
		})
		if !slices.Equal(kinds, []string{"version", "help", "enable", "session"}) || (man.State == CaptureComplete) != tc.complete || cc.WorkerResidue == nil {
			t.Errorf("%s: %s %q %v", name, man.State, deref(cc.Reason), kinds)
			continue
		}
		w := cc.WorkerResidue
		switch {
		case tc.complete && tc.layout == "in_place" && (w.State != ResidueVerified || len(w.Entries) != 1 || w.Entries[0].Layout != ResidueLayoutInPlace ||
			cc.Approval.Scope != ScopeProjectScoped || !reflect.DeepEqual(w.Entries[0].Items, rsInPlaceEntry("x", 1).Items)):
			t.Errorf("%s: %+v", name, w)
		case tc.complete && tc.layout == "" && (w.State != ResidueVerified || len(w.Entries) != 0):
			t.Errorf("%s: %+v", name, w)
		case !tc.complete && (w.State != ResidueUnverified || !strings.Contains(deref(w.Reason), tc.want) || len(w.Entries) != 0):
			t.Errorf("%s: %+v %q", name, w, deref(w.Reason))
		}
		b, _ := json.Marshal(man)
		if bytes.Contains(b, []byte(rsCanary)) || bytes.Contains(b, []byte(rsUUID)) {
			t.Errorf("%s: session content in the manifest", name)
		}
	}
}
