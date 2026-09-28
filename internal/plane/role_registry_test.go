package plane

import (
	"bytes"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/wedevwork/callsheet/internal/contract"
)

// nodeBytes reads every node record under root.
func nodeBytes(t *testing.T, root string) map[string][]byte {
	t.Helper()
	out := map[string][]byte{}
	entries, _ := os.ReadDir(layout{root: root}.path(nodesName))
	for _, e := range entries {
		b, err := os.ReadFile(filepath.Join(layout{root: root}.path(nodesName), e.Name()))
		if err != nil {
			t.Fatal(err)
		}
		out[e.Name()] = b
	}
	return out
}

func sameBytes(t *testing.T, what string, a, b map[string][]byte) {
	t.Helper()
	if len(a) != len(b) {
		t.Fatalf("%s changed: %d vs %d files", what, len(a), len(b))
	}
	for k, v := range a {
		if !bytes.Equal(v, b[k]) {
			t.Fatalf("%s %s changed", what, k)
		}
	}
}

// roleTemps lists temporaries in roles/.
func roleTemps(t *testing.T, root string) []string {
	t.Helper()
	var out []string
	entries, _ := os.ReadDir(layout{root: root}.path(rolesName))
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), tempPrefix) {
			out = append(out, e.Name())
		}
	}
	return out
}

// plainCommands runs init, status and cert reissue on root and, with
// withRun, run with a listener that must never be reached; it returns
// their errors.
func plainCommands(t *testing.T, root string, withRun bool) map[string]error {
	t.Helper()
	d := testDeps(t)
	out := map[string]error{}
	_, out["init"] = d.init(bg, InitOptions{StateDir: root})
	_, out["status"] = d.inspect(bg, root)
	_, out["reissue"] = d.reissue(bg, ReissueOptions{StateDir: root, SANs: []string{"127.0.0.1"}})
	if withRun {
		rd := testDeps(t)
		rd.listen = func(string, string) (net.Listener, error) {
			t.Error("run listened despite invalid role state")
			return nil, os.ErrInvalid
		}
		out["run"] = rd.run(bg, RunOptions{StateDir: root})
	}
	return out
}

// TestRoleRegistryContract is UT FP-6; its failures subtest is delegated
// from tests/function (TestRolePersistence/failures). Do not rename or skip
// its subtests.
func TestRoleRegistryContract(t *testing.T) {
	t.Parallel()
	t.Run("failures", func(t *testing.T) {
		t.Parallel()
		// First publication (roles/ creation, no-replace link): every
		// boundary before publication leaves nothing published or visible
		// and no temporary; the retry then succeeds. The registry's own
		// transaction is driven directly here; the "publish" case also runs
		// end to end through the plane below.
		for _, c := range []struct{ op, name string }{
			{"mkdir", rolesName}, {"dirsync", rootName},
			{"create", roleRegistryName}, {"write", roleRegistryName}, {"sync", roleRegistryName}, {"close", roleRegistryName},
			{"publish", roleRegistryName},
		} {
			t.Run("first-"+c.op+"-"+strings.ReplaceAll(c.name, "/", "-"), func(t *testing.T) {
				t.Parallel()
				root := freshRoot(t)
				writeNodeRecord(t, root, idA+".json", encodeNodeRecord(idA, t0), 0o600)
				pki, nodes := snapshot(t, root), nodeBytes(t, root)
				inj := &injector{}
				d := fast(testDeps(t))
				d.fail = inj.fail
				reg := newRoleRegistry(layout{root: root}, d, roleLookup, emptyRoleDoc())
				next, _ := emptyRoleDoc().withAdded(roleCfg("a", "coder", idA))
				commit := func() error {
					tmp, err := reg.prepare(next)
					if err != nil {
						return err
					}
					return reg.publish(tmp, next, idA)
				}
				inj.set(c.op, c.name)
				wantCode(t, commit(), contract.CodeInternal, "nothing was changed")
				if inj.hits() != 1 || registryBytes(t, root) != nil || len(roleTemps(t, root)) != 0 {
					t.Fatalf("after %s failure: hits %d, registry %q, temps %v", c.op, inj.hits(), registryBytes(t, root), roleTemps(t, root))
				}
				if st := reg.load(); st.visible.exists || st.blocked || len(st.visible.roles) != 0 {
					t.Fatalf("state after failure = %+v", st)
				}
				if _, err := (layout{root: root}).loadRoles(roleLookup); err != nil {
					t.Fatalf("root after %s failure: %v", c.op, err)
				}
				inj.clear()
				if err := commit(); err != nil {
					t.Fatalf("retry: %v", err)
				}
				doc, err := layout{root: root}.loadRoles(roleLookup)
				if st := reg.load(); err != nil || doc.revision != 1 || len(doc.roles) != 1 || st.visible != st.confirmed || st.blocked {
					t.Fatalf("after retry = %+v %v", doc, err)
				}
				sameSnapshot(t, pki, snapshot(t, root))
				sameBytes(t, "node records", nodes, nodeBytes(t, root))
			})
		}
		t.Run("first-parent-sync", func(t *testing.T) {
			t.Parallel()
			// The parent's sync must succeed before the first publication,
			// on every retry: roles/ left behind by a failed attempt (or an
			// empty roles/ found at startup) is not proof that its entry in
			// the state directory is durable.
			for _, precreated := range []bool{false, true} {
				root := freshRoot(t)
				writeNodeRecord(t, root, idA+".json", encodeNodeRecord(idA, t0), 0o600)
				if precreated {
					if err := os.Mkdir(layout{root: root}.path(rolesName), 0o700); err != nil {
						t.Fatal(err)
					}
				}
				inj := &injector{}
				d := fast(testDeps(t))
				d.fail = inj.fail
				reg := newRoleRegistry(layout{root: root}, d, roleLookup, emptyRoleDoc())
				next, _ := emptyRoleDoc().withAdded(roleCfg("a", "coder", idA))
				commit := func() error {
					tmp, err := reg.prepare(next)
					if err != nil {
						return err
					}
					return reg.publish(tmp, next, idA)
				}
				inj.set("dirsync", rootName)
				for attempt := 1; attempt <= 3; attempt++ {
					wantCode(t, commit(), contract.CodeInternal, "nothing was changed")
					if inj.hits() != attempt || registryBytes(t, root) != nil || len(roleTemps(t, root)) != 0 {
						t.Fatalf("precreated=%v attempt %d: parent syncs %d, registry %q, temps %v", precreated, attempt, inj.hits(), registryBytes(t, root), roleTemps(t, root))
					}
					if st := reg.load(); st.visible.exists || st.blocked {
						t.Fatalf("precreated=%v attempt %d: state %+v", precreated, attempt, st)
					}
				}
				inj.clear()
				if err := commit(); err != nil {
					t.Fatalf("precreated=%v recovery: %v", precreated, err)
				}
				doc, err := layout{root: root}.loadRoles(roleLookup)
				if st := reg.load(); err != nil || doc.revision != 1 || len(doc.roles) != 1 || st.visible != st.confirmed || st.blocked {
					t.Fatalf("precreated=%v after recovery = %+v %v", precreated, doc, err)
				}
				// Once published, later writes sync roles/, not the parent.
				inj.set("dirsync", rootName)
				next2, _ := doc.withAdded(roleCfg("b", "coder", idA))
				tmp, err := reg.prepare(next2)
				if err == nil {
					err = reg.publish(tmp, next2, idA)
				}
				if err != nil || inj.hits() != 0 {
					t.Fatalf("precreated=%v later write: %v, parent syncs %d", precreated, err, inj.hits())
				}
			}
		})
		t.Run("first-publish-end-to-end", func(t *testing.T) {
			t.Parallel()
			inj := &injector{}
			d := fast(testDeps(t))
			d.fail = inj.fail
			rp := startRolePlaneWith(t, d, idA)
			p := rp.online(t, idA)
			inj.set("publish", roleRegistryName)
			res := rp.addAsync(bg, roleCfg("a", "coder", idA))
			p.validateOK("p2")
			_, err := res.wait(t)
			wantCode(t, err, contract.CodeInternal, "nothing was changed")
			if _, err := rp.cl.ShowRole(bg, "a"); !contract.IsCode(err, contract.CodeNotFound) || registryBytes(t, rp.root) != nil {
				t.Fatalf("unpublished role visible: %v", err)
			}
			inj.clear()
			if v := rp.add(t, p, "p3", "p4", roleCfg("a", "coder", idA)); v.RegistrationOrder != 1 {
				t.Fatalf("retry = %+v", v)
			}
		})
		// Later publications (atomic rename of a synced temporary), each
		// failing one rm on a shared plane: the registry is unchanged, no
		// temporary stays, and the retry succeeds.
		t.Run("later", func(t *testing.T) {
			t.Parallel()
			ops := []string{"create", "write", "sync", "close", "rename"}
			var recs []contract.RoleRecord
			for i, op := range ops {
				recs = append(recs, record(roleCfg(op, "coder", idA), i+1))
			}
			inj := &injector{}
			d := fast(testDeps(t))
			d.fail = inj.fail
			rp := startRolePlaneDoc(t, d, docOf(1, len(ops)+1, recs...), idA)
			for _, op := range ops {
				before := registryBytes(t, rp.root)
				inj.set(op, roleRegistryName)
				err := rmRole(rp.cl, bg, op, false)
				wantCode(t, err, contract.CodeInternal, "nothing was changed")
				if !bytes.Equal(before, registryBytes(t, rp.root)) || len(roleTemps(t, rp.root)) != 0 {
					t.Fatalf("after %s failure the registry changed or a temporary stayed: %v", op, roleTemps(t, rp.root))
				}
				rp.view(t, op)
				inj.clear()
				if err := rmRole(rp.cl, bg, op, false); err != nil {
					t.Fatalf("%s retry: %v", op, err)
				}
			}
		})
		t.Run("ambiguous-durability", func(t *testing.T) {
			t.Parallel()
			// A failed directory sync after the rename: the visible change is
			// adopted, not rolled back; readiness is masked and nothing is
			// distributed until a later mutation confirms the sync first.
			inj := &injector{}
			d := fast(testDeps(t))
			d.fail = inj.fail
			a, b := roleCfg("a", "coder", idA), roleCfg("b", "coder", idA)
			rp := startRolePlaneDoc(t, d, docOf(1, 3, record(a, 1), record(b, 2)), idA)
			p := rp.online(t, idA)
			p.ready = map[string]bool{"a": true, "b": true}
			p.heartbeat(2)
			if !rp.canAccept(t, "a") {
				t.Fatal("not ready")
			}
			inj.set("dirsync", rolesName)
			err := rmRole(rp.cl, bg, "b", false)
			wantCode(t, err, contract.CodeInternal, "role change was published but durability is unconfirmed; inspect role show and retry after storage recovery")
			if _, err := rp.cl.ShowRole(bg, "b"); !contract.IsCode(err, contract.CodeNotFound) {
				t.Fatalf("the visible removal was rolled back: %v", err)
			}
			if !strings.Contains(string(registryBytes(t, rp.root)), `"revision": 2`) {
				t.Fatalf("registry = %s", registryBytes(t, rp.root))
			}
			// Unconfirmed: all readiness false, even with a ready heartbeat,
			// and no snapshot pushed.
			p.heartbeat(3)
			if rp.canAccept(t, "a") {
				t.Fatal("readiness exposed on unconfirmed registry state")
			}
			if rp.log.seenPrefix("replace-sent " + idA + " p2") {
				t.Fatal("unconfirmed state was distributed")
			}
			// While the sync keeps failing, the next mutation fails first and
			// changes nothing more; a retried rm conflicts at once (not found).
			if err := rmRole(rp.cl, bg, "b", false); !contract.IsCode(err, contract.CodeNotFound) {
				t.Fatalf("retried rm = %v", err)
			}
			err = rmRole(rp.cl, bg, "a", false)
			wantCode(t, err, contract.CodeInternal, "durability is still unconfirmed")
			rp.view(t, "a")
			// Recovery: the sync succeeds first, the confirmed state is
			// distributed, then the mutation proceeds.
			inj.clear()
			if err := rmRole(rp.cl, bg, "a", true); err != nil {
				t.Fatal(err)
			}
			for next := 2; ; next++ {
				bd := p.ackReplace("p" + strconv.Itoa(next))
				if bd.Revision == 3 {
					if len(bd.Roles) != 0 {
						t.Fatalf("final snapshot = %+v", bd)
					}
					break
				}
				if bd.Revision != 2 || len(bd.Roles) != 1 {
					t.Fatalf("resync snapshot = %+v", bd)
				}
			}
			p.heartbeat(4)
		})
		t.Run("stale-temporary", func(t *testing.T) {
			t.Parallel()
			// A stale regular temporary holding a newer complete revision is
			// ignored by every load: never promoted, deleted or counted.
			a := roleCfg("a", "coder", idA)
			root := freshRoot(t)
			writeNodeRecord(t, root, idA+".json", encodeNodeRecord(idA, t0), 0o600)
			writeRegistry(t, root, docOf(1, 2, record(a, 1)))
			tmp := filepath.Join(layout{root: root}.path(rolesName), tempPrefix+"registry.json-123")
			newer := encodeRoleDoc(docOf(7, 9, record(roleCfg("z", "coder", idA), 8)))
			os.WriteFile(tmp, newer, 0o600)
			doc, err := layout{root: root}.loadRoles(roleLookup)
			if err != nil || doc.revision != 1 || len(doc.roles) != 1 || doc.roles[0].ID != "a" {
				t.Fatalf("load = %+v %v", doc, err)
			}
			if _, err := testDeps(t).inspect(bg, root); err != nil {
				t.Fatal(err)
			}
			reg := newRoleRegistry(layout{root: root}, fast(testDeps(t)), roleLookup, doc)
			next, rec := doc.withAdded(roleCfg("b", "coder", idA))
			tmp2, err := reg.prepare(next)
			if err == nil {
				err = reg.publish(tmp2, next, idA)
			}
			if err != nil || rec.RegistrationOrder != 2 || !strings.Contains(string(registryBytes(t, root)), `"revision": 2`) {
				t.Fatalf("counters advanced from the temporary: %+v %v", rec, err)
			}
			if b, err := os.ReadFile(tmp); err != nil || !bytes.Equal(b, newer) {
				t.Fatalf("temporary touched: %v", err)
			}
		})
	})
	t.Run("format", func(t *testing.T) {
		t.Parallel()
		// The writer's exact bytes: two-space indentation, schema field
		// order, roles by registration order, final LF, no HTML escaping;
		// directory 0700 and file 0600; real syncs.
		root := freshRoot(t)
		reg := newRoleRegistry(layout{root: root}, testDeps(t), roleLookup, emptyRoleDoc())
		commit := func(next *roleDoc) {
			t.Helper()
			tmp, err := reg.prepare(next)
			if err == nil {
				err = reg.publish(tmp, next, idA)
			}
			if err != nil {
				t.Fatal(err)
			}
		}
		c := roleCfg("worker-a", "implementer", idA)
		c.Model = "<model & \"quoted\">"
		c.Timeout, c.HasTimeout = 0, true
		next, _ := emptyRoleDoc().withAdded(c)
		commit(next)
		want := "{\n  \"schema_version\": 2,\n  \"revision\": 1,\n  \"next_registration_order\": 2,\n  \"roles\": [\n    {\n" +
			"      \"id\": \"worker-a\",\n      \"name\": \"implementer\",\n      \"node\": \"" + idA + "\",\n      \"adapter\": \"fake\",\n" +
			"      \"instruction\": \"/srv/manuals/worker-a/instruction.md\",\n      \"runbook\": \"/srv/manuals/worker-a/runbook.md\",\n" +
			"      \"model\": \"<model & \\\"quoted\\\">\",\n      \"effort\": \"medium\",\n      \"concurrency\": 2,\n      \"timeout\": \"0s\",\n" +
			"      \"registration_order\": 1\n    }\n  ],\n  \"removals\": []\n}\n"
		if got := string(registryBytes(t, root)); got != want {
			t.Fatalf("registry bytes:\n%s\nwant:\n%s", got, want)
		}
		for p, mode := range map[string]os.FileMode{layout{root: root}.path(rolesName): 0o700, layout{root: root}.path(roleRegistryName): 0o600} {
			if fi, err := os.Stat(p); err != nil || fi.Mode().Perm() != mode {
				t.Fatalf("%s mode %v %v", p, fi.Mode(), err)
			}
		}
		// Removing the last role keeps the file and its counters.
		commit(next.withRemoved(0))
		if got := string(registryBytes(t, root)); got != "{\n  \"schema_version\": 2,\n  \"revision\": 2,\n  \"next_registration_order\": 2,\n  \"roles\": [],\n  \"removals\": []\n}\n" {
			t.Fatalf("empty registry:\n%s", got)
		}
	})
	t.Run("counters", func(t *testing.T) {
		t.Parallel()
		// Re-adding a removed ID gets a new order; a reload preserves the
		// counters and order; no node or PKI byte changes. (Readiness after
		// a restart is TestRoleDistributionContract/reconnect's.)
		root := freshRoot(t)
		writeNodeRecord(t, root, idA+".json", encodeNodeRecord(idA, t0), 0o600)
		pki, nodes := snapshot(t, root), nodeBytes(t, root)
		reg := newRoleRegistry(layout{root: root}, fast(testDeps(t)), roleLookup, emptyRoleDoc())
		commit := func(next *roleDoc) *roleDoc {
			t.Helper()
			tmp, err := reg.prepare(next)
			if err == nil {
				err = reg.publish(tmp, next, idA)
			}
			if err != nil {
				t.Fatal(err)
			}
			return next
		}
		doc, _ := emptyRoleDoc().withAdded(roleCfg("a", "coder", idA))
		doc = commit(doc)
		doc, _ = doc.withAdded(roleCfg("b", "coder", idA))
		doc = commit(doc)
		doc = commit(doc.withRemoved(0))
		doc, rec := doc.withAdded(roleCfg("a", "coder", idA))
		commit(doc)
		if rec.RegistrationOrder != 3 {
			t.Fatalf("re-added order = %d", rec.RegistrationOrder)
		}
		got, err := layout{root: root}.loadRoles(roleLookup)
		if err != nil || got.revision != 4 || got.nextOrder != 4 || len(got.roles) != 2 || got.roles[0].ID != "b" || got.roles[1].ID != "a" || got.roles[1].RegistrationOrder != 3 {
			t.Fatalf("reloaded = %+v %v", got, err)
		}
		sameSnapshot(t, pki, snapshot(t, root))
		sameBytes(t, "node records", nodes, nodeBytes(t, root))
		// Counter exhaustion refuses the mutation (conflict), never wraps.
		root2 := freshRoot(t)
		writeNodeRecord(t, root2, idA+".json", encodeNodeRecord(idA, t0), 0o600)
		writeRegistry(t, root2, docOf(contract.MaxSafeInteger, contract.MaxSafeInteger, record(roleCfg("x", "coder", idA), 5)))
		d3 := fast(testDeps(t))
		rp3 := &rolePlane{log: newEventLog(), hooks: newHooks()}
		d3.streamEvents = rp3.log.add
		rp3.nodePlane = serveNodePlaneAt(t, d3, root2)
		if err := rmRole(rp3.cl, bg, "x", false); !contract.IsCode(err, contract.CodeConflict) || !strings.Contains(err.Error(), "exhausted") {
			t.Fatalf("exhausted rm = %v", err)
		}
		if _, err := rp3.cl.AddRole(bg, roleCfg("y", "coder", idA)); !contract.IsCode(err, contract.CodeConflict) {
			t.Fatalf("exhausted add = %v", err)
		}
	})
	t.Run("scan", func(t *testing.T) {
		t.Parallel()
		a := roleCfg("a", "coder", idA)
		good := string(encodeRoleDoc(docOf(1, 2, record(a, 1))))
		setup := func(t *testing.T) string {
			root := freshRoot(t)
			writeNodeRecord(t, root, idA+".json", encodeNodeRecord(idA, t0), 0o600)
			return root
		}
		content := func(s string) func(t *testing.T, root string) {
			return func(t *testing.T, root string) {
				os.MkdirAll(layout{root: root}.path(rolesName), 0o700)
				os.WriteFile(layout{root: root}.path(roleRegistryName), []byte(s), 0o600)
			}
		}
		full := map[string]bool{"unknown node": true, "symlinked dir": true}
		var many []contract.RoleRecord
		for i := 1; i <= contract.MaxRoles+1; i++ {
			r := record(roleCfg("r"+strconv.Itoa(i), "coder", idA), i)
			many = append(many, r)
		}
		for name, c := range map[string]struct {
			prep func(t *testing.T, root string)
			code contract.Code
			want string
		}{
			"malformed":       {content(`{"schema_version":1,`), contract.CodeConflict, "invalid role registry"},
			"unknown field":   {content(strings.Replace(good, `"revision"`, `"extra": 1, "revision"`, 1)), contract.CodeConflict, "unknown field"},
			"duplicate key":   {content(strings.Replace(good, `"revision": 1`, `"revision": 1, "revision": 1`, 1)), contract.CodeConflict, "duplicate key"},
			"schema":          {content(strings.Replace(good, `"schema_version": 2`, `"schema_version": 3`, 1)), contract.CodeConflict, "unsupported schema_version"},
			"revision zero":   {content(strings.Replace(good, `"revision": 1`, `"revision": 0`, 1)), contract.CodeConflict, `"revision" must be`},
			"revision float":  {content(strings.Replace(good, `"revision": 1`, `"revision": 1.0`, 1)), contract.CodeConflict, `"revision" must be`},
			"next too low":    {content(strings.Replace(good, `"next_registration_order": 2`, `"next_registration_order": 1`, 1)), contract.CodeConflict, "not below next_registration_order"},
			"null roles":      {content(strings.Replace(good, `"roles": [`, `"roles": null, "x": [`, 1)), contract.CodeConflict, "must not be null"},
			"missing roles":   {content(`{"schema_version": 1, "revision": 1, "next_registration_order": 1}`), contract.CodeConflict, `"roles" is required`},
			"trailing":        {content(good + "{}"), contract.CodeConflict, "trailing data"},
			"noncanonical":    {content(strings.Replace(good, `"2h0m0s"`, `"2h"`, 1)), contract.CodeConflict, "canonical"},
			"bad effort":      {content(strings.Replace(good, `"medium"`, `"max"`, 1)), contract.CodeConflict, "not allowed"},
			"unknown node":    {content(strings.Replace(good, idA, idB, 1)), contract.CodeConflict, "not enrolled"},
			"unsorted":        {content(string(encodeRoleDoc(docOf(2, 3, record(roleCfg("b", "coder", idA), 2), record(a, 1))))), contract.CodeConflict, "not sorted"},
			"duplicate id":    {content(string(encodeRoleDoc(docOf(2, 3, record(a, 1), record(a, 2))))), contract.CodeConflict, "more than once"},
			"duplicate order": {content(string(encodeRoleDoc(docOf(2, 3, record(a, 1), record(roleCfg("b", "coder", idA), 1))))), contract.CodeConflict, "not sorted"},
			"too many":        {content(string(encodeRoleDoc(docOf(1, 200, many...)))), contract.CodeConflict, "exceed the limit of 100"},
			"too large":       {content(good + strings.Repeat(" ", maxRoleRegistry)), contract.CodeConflict, "larger than"},
			"unexpected file": {func(t *testing.T, root string) {
				content(good)(t, root)
				os.WriteFile(filepath.Join(layout{root: root}.path(rolesName), "notes.txt"), nil, 0o600)
			}, contract.CodeConflict, "roles/notes.txt"},
			"subdirectory": {func(t *testing.T, root string) {
				content(good)(t, root)
				os.Mkdir(filepath.Join(layout{root: root}.path(rolesName), "registry.json.d"), 0o700)
			}, contract.CodeConflict, "unexpected"},
			"temp directory": {func(t *testing.T, root string) {
				content(good)(t, root)
				os.Mkdir(filepath.Join(layout{root: root}.path(rolesName), tempPrefix+"x"), 0o700)
			}, contract.CodeConflict, "unexpected"},
			"symlinked dir": {func(t *testing.T, root string) {
				other := t.TempDir()
				os.Chmod(other, 0o700)
				os.WriteFile(filepath.Join(other, "registry.json"), []byte(good), 0o600)
				os.Symlink(other, layout{root: root}.path(rolesName))
			}, contract.CodeTrustFailed, "symbolic link"},
			"symlinked file": {func(t *testing.T, root string) {
				os.MkdirAll(layout{root: root}.path(rolesName), 0o700)
				other := filepath.Join(t.TempDir(), "r.json")
				os.WriteFile(other, []byte(good), 0o600)
				os.Symlink(other, layout{root: root}.path(roleRegistryName))
			}, contract.CodeTrustFailed, "symbolic link"},
			"writable file": {func(t *testing.T, root string) {
				content(good)(t, root)
				os.Chmod(layout{root: root}.path(roleRegistryName), 0o666)
			}, contract.CodeTrustFailed, "writable by group or others"},
			"open dir": {func(t *testing.T, root string) {
				content(good)(t, root)
				os.Chmod(layout{root: root}.path(rolesName), 0o755)
			}, contract.CodeTrustFailed, "accessible by group or others"},
			"roles is a file": {func(t *testing.T, root string) {
				os.WriteFile(layout{root: root}.path(rolesName), nil, 0o600)
			}, contract.CodeTrustFailed, "not a directory"},
		} {
			t.Run(name, func(t *testing.T) {
				t.Parallel()
				root := setup(t)
				c.prep(t, root)
				pki := snapshot(t, root)
				// Every plane command shares the strict scan and loader; run
				// all four for one variant of each kind and the offline
				// status for the rest.
				errs := map[string]error{}
				if full[name] {
					errs = plainCommands(t, root, true)
				} else {
					_, errs["status"] = testDeps(t).inspect(bg, root)
				}
				for cmd, err := range errs {
					if contract.CodeOf(err) != c.code || !strings.Contains(err.Error(), c.want) {
						t.Fatalf("%s: %v, want %s containing %q", cmd, err, c.code, c.want)
					}
				}
				sameSnapshot(t, pki, snapshot(t, root))
			})
		}
		t.Run("empty-directory", func(t *testing.T) {
			t.Parallel()
			// A crash can leave roles/ created and empty: a valid empty
			// registry, published with no-replace link later.
			root := setup(t)
			os.Mkdir(layout{root: root}.path(rolesName), 0o700)
			for cmd, err := range plainCommands(t, root, false) {
				if err != nil {
					t.Fatalf("%s: %v", cmd, err)
				}
			}
		})
		t.Run("partial", func(t *testing.T) {
			t.Parallel()
			// A role registry without plane trust is partial state, never a
			// new plane to initialize around it.
			root := newRoot(t)
			os.MkdirAll(layout{root: root}.path(rolesName), 0o700)
			os.WriteFile(layout{root: root}.path(roleRegistryName), []byte(good), 0o600)
			_, err := testDeps(t).init(bg, initOpts(root, "127.0.0.1"))
			wantCode(t, err, contract.CodeConflict, "holds a role registry", "never bootstrapped")
			if _, err := os.Stat(layout{root: root}.path(caCertName)); !os.IsNotExist(err) {
				t.Fatal("a CA was created around a role registry")
			}
		})
	})
}
