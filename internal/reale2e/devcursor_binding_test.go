package reale2e

import (
	"os"
	"path/filepath"
	"testing"
)

// Unit tests of design 12a-3c-cursor-binding (UT-1, UT-2a, UT-2b, UT-2c,
// UT-4b): the identity reader, the attempt and root binding gates and the
// stored-version extraction, over temporary files only. Expected raw
// identities derive from filepath.EvalSymlinks, never the temporary path
// as created.

// bindingTree is a temporary installation: versions/A and versions/B hold
// byte-identical wrappers, versions/C another one.
type bindingTree struct {
	t              testing.TB
	dir            string
	a, b, c, bin   string
	wrapper, other []byte
}

func newBindingTree(t testing.TB) bindingTree {
	t.Helper()
	bt := bindingTree{t: t, dir: t.TempDir(), wrapper: []byte("#!/bin/sh\nexec node index.js \"$@\"\n"), other: []byte("#!/bin/sh\nexit 1\n")}
	for _, v := range []struct {
		name string
		p    *string
		data []byte
	}{{"A", &bt.a, bt.wrapper}, {"B", &bt.b, bt.wrapper}, {"C", &bt.c, bt.other}} {
		d := filepath.Join(bt.dir, "versions", v.name)
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
		*v.p = filepath.Join(d, "cursor-agent")
		if err := os.WriteFile(*v.p, v.data, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	bt.bin = filepath.Join(bt.dir, "bin")
	if err := os.MkdirAll(bt.bin, 0o700); err != nil {
		t.Fatal(err)
	}
	return bt
}

// link makes (or replaces) bin/name pointing at target and returns it.
func (bt bindingTree) link(name, target string) string {
	bt.t.Helper()
	p := filepath.Join(bt.bin, name)
	os.Remove(p)
	if err := os.Symlink(target, p); err != nil {
		bt.t.Fatal(err)
	}
	return p
}

// want is p's expected identity.
func (bt bindingTree) want(p string) cursorIdentity {
	bt.t.Helper()
	r, err := filepath.EvalSymlinks(p)
	if err != nil {
		bt.t.Fatal(err)
	}
	return cursorIdentity{path: r, sha256: sha256Hex(mustRead(bt.t, r))}
}

func readIdentity(t *testing.T, p string) cursorIdentity {
	t.Helper()
	id, err := readCursorIdentity(p)
	if err != nil {
		t.Fatalf("%s: %v", p, err)
	}
	return id
}

func TestCursorIdentityReaderUnit(t *testing.T) {
	t.Parallel()
	bt := newBindingTree(t)
	a := bt.want(bt.a)
	if (cursorIdentity{}).set() || (cursorIdentity{path: "/x"}).set() || !a.set() {
		t.Fatal("set")
	}
	t.Run("direct", func(t *testing.T) {
		if got := readIdentity(t, bt.a); got != a || got.sha256 != sha256Hex(bt.wrapper) {
			t.Fatalf("%+v want %+v", got, a)
		}
	})
	t.Run("multi_hop", func(t *testing.T) {
		hop := bt.link("hop", bt.a)
		if got := readIdentity(t, bt.link("multi", hop)); got != a {
			t.Fatalf("%+v want %+v", got, a)
		}
	})
	t.Run("relative_target", func(t *testing.T) {
		if got := readIdentity(t, bt.link("rel", filepath.Join("..", "versions", "A", "cursor-agent"))); got != a {
			t.Fatalf("%+v want %+v", got, a)
		}
	})
	t.Run("same_target_through_changed_link", func(t *testing.T) {
		// A spelling change that reaches the same target and bytes keeps
		// the identity.
		l := bt.link("respelled", bt.a)
		before := readIdentity(t, l)
		bt.link("respelled", bt.link("via", filepath.Join(bt.dir, "versions", "A", ".", "cursor-agent")))
		if after := readIdentity(t, l); !after.equal(before) {
			t.Fatalf("%+v != %+v", after, before)
		}
	})
	t.Run("different_targets_equal_bytes", func(t *testing.T) {
		ga, gb := readIdentity(t, bt.link("to-a", bt.a)), readIdentity(t, bt.link("to-b", bt.b))
		if ga.sha256 != gb.sha256 || ga.sameTarget(gb) || ga.equal(gb) || gb != bt.want(bt.b) {
			t.Fatalf("%+v %+v", ga, gb)
		}
	})
	t.Run("same_path_unequal_bytes", func(t *testing.T) {
		d := filepath.Join(bt.dir, "versions", "D")
		os.MkdirAll(d, 0o700)
		p := filepath.Join(d, "cursor-agent")
		os.WriteFile(p, bt.wrapper, 0o700)
		before := readIdentity(t, p)
		os.WriteFile(p, bt.other, 0o700)
		after := readIdentity(t, p)
		if !after.sameTarget(before) || after.equal(before) || after.sha256 != sha256Hex(bt.other) {
			t.Fatalf("%+v %+v", before, after)
		}
	})
	t.Run("dangling", func(t *testing.T) {
		if id, err := readCursorIdentity(bt.link("dangling", filepath.Join(bt.dir, "versions", "missing", "cursor-agent"))); err == nil || id != (cursorIdentity{}) {
			t.Fatalf("%+v %v", id, err)
		}
	})
	t.Run("loop", func(t *testing.T) {
		x := filepath.Join(bt.bin, "loop-x")
		bt.link("loop-y", x)
		if id, err := readCursorIdentity(bt.link("loop-x", filepath.Join(bt.bin, "loop-y"))); err == nil || id != (cursorIdentity{}) {
			t.Fatalf("%+v %v", id, err)
		}
	})
	t.Run("bounded_reader", func(t *testing.T) {
		// Beyond the bound and a directory target: the resolved target is
		// observed, never a digest.
		if id, err := readCursorIdentityWithin(bt.link("bounded", bt.a), 4); err == nil || id.path != a.path || id.sha256 != "" {
			t.Fatalf("%+v %v", id, err)
		}
		r, _ := filepath.EvalSymlinks(filepath.Dir(bt.a))
		if id, err := readCursorIdentity(bt.link("directory", filepath.Dir(bt.a))); err == nil || id.path != r || id.sha256 != "" {
			t.Fatalf("%+v %v", id, err)
		}
		if id, err := readCursorIdentityWithin(bt.a, int64(len(bt.wrapper))); err != nil || id != a {
			t.Fatalf("at the bound: %+v %v", id, err)
		}
	})
	t.Run("relative_input", func(t *testing.T) {
		if _, err := readCursorIdentity("cursor-agent"); err == nil {
			t.Fatal("a relative --cursor path resolved")
		}
	})
}

func TestCursorAfterDigestUnit(t *testing.T) {
	t.Parallel()
	bt := newBindingTree(t)
	a := bt.want(bt.a)
	for name, c := range map[string]struct {
		target  string
		latched bool
		after   string
		failed  bool
	}{
		"unchanged":         {bt.a, false, a.sha256, false},
		"unchanged_latched": {bt.a, true, "", false},
		"retarget":          {bt.b, false, "", true},
		"retarget_latched":  {bt.b, true, "", true},
		"replaced":          {bt.c, false, sha256Hex(bt.other), true},
		"unreadable":        {filepath.Dir(bt.a), false, "", true},
		"dangling":          {filepath.Join(bt.dir, "nowhere"), false, "", true},
	} {
		t.Run(name, func(t *testing.T) {
			l := bt.link("after-"+name, c.target)
			frozen := a
			if name == "replaced" || name == "unchanged" || name == "unchanged_latched" {
				// The frozen target is the link's own target (a same-path
				// replacement for "replaced").
				frozen = cursorIdentity{path: bt.want(l).path, sha256: a.sha256}
			}
			after, failed := cursorAfterDigest(l, frozen, c.latched)
			if after != c.after || failed != c.failed {
				t.Fatalf("after %q failed %t, want %q %t", after, failed, c.after, c.failed)
			}
		})
	}
}

// bindingSupervisor is a Cursor run's minimal supervisor over exe.
func bindingSupervisor(exe string, expect cursorIdentity) *devSupervisor {
	return &devSupervisor{s: &supervisor{args: runArgs{cursor: exe, cursorExpect: expect}}, cursor: &cursorRun{exe: exe}}
}

func TestCursorBindGatesUnit(t *testing.T) {
	t.Parallel()
	bt := newBindingTree(t)
	a, b := bt.want(bt.a), bt.want(bt.b)
	t.Run("before_and_after_unchanged", func(t *testing.T) {
		d := bindingSupervisor(bt.link("ok", bt.a), cursorIdentity{})
		if err := d.cursorBindBefore(); err != nil || d.cursor.frozen != a || d.cursor.binding != (DevCursorBinding{Path: d.s.tok(a.path), BeforeSHA256: a.sha256}) {
			t.Fatalf("%v %+v", err, d.cursor)
		}
		d.cursorBindAfter()
		if d.cursor.binding.AfterSHA256 != a.sha256 || d.cursorBindingFailed() {
			t.Fatalf("%+v", d.cursor)
		}
	})
	t.Run("before_unresolvable", func(t *testing.T) {
		d := bindingSupervisor(bt.link("gone", filepath.Join(bt.dir, "nowhere")), cursorIdentity{})
		err := d.cursorBindBefore()
		if codeOf(err) != CodeCursorExecutableMismatch || d.cursor.binding != (DevCursorBinding{}) || !d.cursorBindingFailed() || d.cursor.frozen.set() {
			t.Fatalf("%v %+v", err, d.cursor)
		}
		d.cursorBindAfter()
		if d.cursor.binding.AfterSHA256 != "" {
			t.Fatal("an unbound run recorded an after digest")
		}
	})
	t.Run("before_unreadable", func(t *testing.T) {
		d := bindingSupervisor(bt.link("dir", filepath.Dir(bt.a)), cursorIdentity{})
		r, _ := filepath.EvalSymlinks(filepath.Dir(bt.a))
		if err := d.cursorBindBefore(); codeOf(err) != CodeCursorExecutableMismatch || d.cursor.binding != (DevCursorBinding{Path: d.s.tok(r)}) {
			t.Fatalf("%v %+v", err, d.cursor.binding)
		}
	})
	t.Run("root_expectation", func(t *testing.T) {
		// The child resolved B (identical bytes) where the root froze A: it
		// keeps its own identity, refuses, and its latched failure leaves
		// no after digest even though B is unchanged.
		d := bindingSupervisor(bt.link("child", bt.b), a)
		if err := d.cursorBindBefore(); codeOf(err) != CodeCursorExecutableMismatch || d.cursor.frozen != b ||
			d.cursor.binding != (DevCursorBinding{Path: d.s.tok(b.path), BeforeSHA256: b.sha256}) {
			t.Fatalf("%v %+v", err, d.cursor)
		}
		d.cursorBindAfter()
		if d.cursor.binding.AfterSHA256 != "" || !d.cursorBindingFailed() {
			t.Fatalf("%+v", d.cursor)
		}
		ok := bindingSupervisor(bt.link("child-ok", bt.a), a)
		if err := ok.cursorBindBefore(); err != nil {
			t.Fatalf("the root's own identity was refused: %v", err)
		}
	})
	t.Run("after_retarget_and_replace", func(t *testing.T) {
		l := bt.link("moved", bt.a)
		d := bindingSupervisor(l, cursorIdentity{})
		d.cursorBindBefore()
		bt.link("moved", bt.b)
		d.cursorBindAfter()
		if d.cursor.binding.AfterSHA256 != "" || !d.cursorBindingFailed() || d.cursor.binding.Path != d.s.tok(a.path) {
			t.Fatalf("%+v", d.cursor)
		}
		// Restoration never clears the latched failure.
		bt.link("moved", bt.a)
		d.cursorBindAfter()
		if d.cursor.binding.AfterSHA256 != "" || !d.cursorBindingFailed() {
			t.Fatalf("%+v", d.cursor)
		}
	})
	t.Run("default_run", func(t *testing.T) {
		d := &devSupervisor{s: &supervisor{}}
		d.cursorBindAfter()
		if d.cursorBindingFailed() {
			t.Fatal("a default run failed a binding")
		}
	})
}

func TestCursorExerciseGateUnit(t *testing.T) {
	t.Parallel()
	bt := newBindingTree(t)
	a := bt.want(bt.a)
	root := func(link string) *devExerciseRun {
		return &devExerciseRun{a: runArgs{cursor: link, cursorExpect: a}, ex: DevExercise{Cursor: &DevCursorBinding{Path: "$X", BeforeSHA256: a.sha256}}}
	}
	t.Run("unchanged", func(t *testing.T) {
		e := root(bt.link("r-ok", bt.a))
		if !e.cursorUnchanged() {
			t.Fatal("an unchanged identity was refused")
		}
		e.cursorAfter()
		if e.ex.Cursor.AfterSHA256 != a.sha256 || e.cursorFailed {
			t.Fatalf("%+v", e.ex.Cursor)
		}
	})
	t.Run("retarget_is_sticky", func(t *testing.T) {
		l := bt.link("r-moved", bt.a)
		e := root(l)
		bt.link("r-moved", bt.b)
		if e.cursorUnchanged() {
			t.Fatal("a byte-identical retarget passed")
		}
		bt.link("r-moved", bt.a)
		if e.cursorUnchanged() {
			t.Fatal("a restored retarget cleared the failure")
		}
		e.cursorAfter()
		if e.ex.Cursor.AfterSHA256 != "" {
			t.Fatalf("%+v", e.ex.Cursor)
		}
	})
	t.Run("child_observed", func(t *testing.T) {
		e := root(bt.link("r-child", bt.a))
		e.cursorObserved(&devSupervisor{})
		e.cursorObserved(&devSupervisor{cursor: &cursorRun{}})
		if e.cursorFailed {
			t.Fatal("a clean or default child latched a failure")
		}
		e.cursorObserved(&devSupervisor{cursor: &cursorRun{failed: true}})
		if e.cursorUnchanged() {
			t.Fatal("a child's observed failure did not stop the light child")
		}
		e.cursorAfter()
		if e.ex.Cursor.AfterSHA256 != "" {
			t.Fatalf("%+v", e.ex.Cursor)
		}
	})
	t.Run("unreadable", func(t *testing.T) {
		e := root(bt.link("r-gone", filepath.Join(bt.dir, "nowhere")))
		if e.cursorUnchanged() {
			t.Fatal("an unreadable identity passed")
		}
	})
	t.Run("replaced_keeps_unequal_after", func(t *testing.T) {
		d := filepath.Join(bt.dir, "versions", "E")
		os.MkdirAll(d, 0o700)
		p := filepath.Join(d, "cursor-agent")
		os.WriteFile(p, bt.wrapper, 0o700)
		e := root(bt.link("r-replaced", p))
		e.a.cursorExpect = bt.want(p)
		os.WriteFile(p, bt.other, 0o700)
		if e.cursorUnchanged() {
			t.Fatal("a byte replacement passed")
		}
		e.cursorAfter()
		if e.ex.Cursor.AfterSHA256 != sha256Hex(bt.other) {
			t.Fatalf("%+v", e.ex.Cursor)
		}
	})
	t.Run("default_exercise", func(t *testing.T) {
		e := &devExerciseRun{}
		e.cursorAfter()
		if !e.cursorUnchanged() || e.ex.Cursor != nil {
			t.Fatal("a default exercise was gated")
		}
	})
}

func TestCursorStoredVersionUnit(t *testing.T) {
	t.Parallel()
	rec := func(tool, v string) VersionRecord {
		return VersionRecord{Tool: tool, Path: "/p/" + tool, Version: v, Eligible: true}
	}
	for name, c := range map[string]struct {
		vs   []VersionRecord
		want string
		ok   bool
	}{
		"one":       {[]VersionRecord{rec("claude", "2.1.299"), rec("cursor", "2026.10.14-0f9edde")}, "2026.10.14-0f9edde", true},
		"verbatim":  {[]VersionRecord{rec("cursor", "2026.10.14-0f9edde\n")}, "2026.10.14-0f9edde\n", true},
		"missing":   {[]VersionRecord{rec("claude", "2.1.299")}, "", false},
		"duplicate": {[]VersionRecord{rec("cursor", "2026.10.14-0f9edde"), rec("cursor", "2026.10.14-0f9edde")}, "", false},
		"empty":     {[]VersionRecord{rec("cursor", " ")}, "", false},
		"none":      {nil, "", false},
	} {
		if got, ok := cursorStoredVersion(c.vs); got != c.want || ok != c.ok {
			t.Errorf("%s: %q %t", name, got, ok)
		}
	}
}
