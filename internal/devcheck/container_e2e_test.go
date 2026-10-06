package devcheck

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	ca "github.com/wedevwork/callsheet/internal/testkit/containeracceptance"
)

// validMeta is a well-formed iteration metadata.
func validMeta(it, total int) ContainerMetadata {
	return ContainerMetadata{RunID: "0123456789abcdef0123456789abcdef", SourceRevision: strings.Repeat("a", 40), Architecture: "amd64",
		Iteration: it, Total: total, BinaryHashes: map[string]string{HashCallsheet: strings.Repeat("1", 64), HashFakeAdapter: strings.Repeat("2", 64),
			HashAcceptanceTest: strings.Repeat("3", 64)}}
}

// runContainer runs a stage through runFor with a fresh evidence
// directory and returns the code, streams and the evidence directory.
func runContainer(t *testing.T, ctx context.Context, goos string, r runnerLike, args ...string) (int, string, string, string) {
	t.Helper()
	opts := testOpts(t)
	var out, errOut bytes.Buffer
	code := runFor(ctx, goos, args, &out, &errOut, r.run, opts)
	t.Cleanup(func() { os.RemoveAll(scratchFrom(out.String())) })
	return code, out.String(), errOut.String(), opts.EvidenceDir
}

func readEvidence(t *testing.T, dir, name string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(dir, name))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// TestContainerDriverContract (FP-1 host contract): the planned Docker
// build, run and cleanup argv and environment, Linux-only routing, the
// count bounds, child failures, the deadline's explicit container
// removal and cleanup on every path, through the public driver with the
// synthetic container Runner fixture. It never launches Docker.
func TestContainerDriverContract(t *testing.T) {
	t.Run("plan", func(t *testing.T) {
		steps, err := ContainerPlan("linux", "callsheet-e2e:r1", "callsheet-e2e-r1-1", validMeta(1, 1))
		if err != nil {
			t.Fatal(err)
		}
		var names []string
		for _, s := range steps {
			names = append(names, s.Name)
			if len(s.Env) != 0 {
				t.Fatalf("%s carries host environment overrides %v", s.Name, s.Env)
			}
		}
		if strings.Join(names, "|") != "container image build|container run|container inspect|container remove|container removal check|image remove|image removal check" {
			t.Fatalf("steps = %v", names)
		}
		if got := strings.Join(steps[0].Argv, " "); got != "docker build --network none --tag callsheet-e2e:r1 --file Dockerfile ." {
			t.Fatalf("build = %s", got)
		}
		run := steps[1].Argv
		want := "docker run --name callsheet-e2e-r1-1 --init --network none --read-only --tmpfs /tmp:rw,exec,nosuid,nodev,size=1073741824,mode=1777 " +
			"--cap-drop ALL --security-opt no-new-privileges --user 65532:65532 --env HOME=/tmp/acceptance/home --env TMPDIR=/tmp " +
			"--env CALLSHEET_E2E_RUN_ID=0123456789abcdef0123456789abcdef --env CALLSHEET_E2E_ITERATION=1 --env CALLSHEET_E2E_TOTAL=1 " +
			"--env CALLSHEET_E2E_REVISION=" + strings.Repeat("a", 40) + " --env CALLSHEET_E2E_ARCH=amd64 " +
			`--env CALLSHEET_E2E_HASHES={"acceptance_test":"` + strings.Repeat("3", 64) + `","callsheet":"` + strings.Repeat("1", 64) + `","fake_adapter":"` + strings.Repeat("2", 64) + `"} ` +
			"callsheet-e2e:r1 -test.v -test.count=1 -test.timeout=6m"
		if got := strings.Join(run, " "); got != want {
			t.Fatalf("run =\n%s\nwant\n%s", got, want)
		}
		// No network, mount, port, socket or privilege leak in any step.
		for _, s := range steps {
			for _, a := range s.Argv {
				for _, leak := range []string{"-v", "--volume", "--mount", "-p", "--publish", "--privileged", "docker.sock", "--network=host", "host", "--cap-add", "--rm"} {
					if a == leak || (strings.Contains(leak, "sock") && strings.Contains(a, leak)) {
						t.Fatalf("%s leaks %q: %v", s.Name, leak, s.Argv)
					}
				}
			}
		}
		if got := strings.Join(steps[2].Argv, " "); got != "docker container inspect --format {{.State.Status}} {{.State.ExitCode}} callsheet-e2e-r1-1" {
			t.Fatalf("inspect = %s", got)
		}
		// Repetition: only iteration 1 builds; only the last removes the
		// image; the metadata alone decides.
		mid, _ := ContainerPlan("linux", "callsheet-e2e:r1", "callsheet-e2e-r1-2", validMeta(2, 3))
		last, _ := ContainerPlan("linux", "callsheet-e2e:r1", "callsheet-e2e-r1-3", validMeta(3, 3))
		if len(mid) != 4 || mid[0].Name != "container run" || len(last) != 6 || last[5].Name != "image removal check" ||
			!strings.Contains(strings.Join(mid[0].Argv, " "), "--env CALLSHEET_E2E_ITERATION=2 --env CALLSHEET_E2E_TOTAL=3 ") {
			t.Fatalf("mid %v last %v", mid, last)
		}
		for name, c := range map[string]struct {
			goos, tag, cname string
			meta             ContainerMetadata
		}{
			"darwin":       {"darwin", "callsheet-e2e:r1", "c1", validMeta(1, 1)},
			"windows":      {"windows", "callsheet-e2e:r1", "c1", validMeta(1, 1)},
			"iteration 0":  {"linux", "callsheet-e2e:r1", "c1", validMeta(0, 1)},
			"beyond total": {"linux", "callsheet-e2e:r1", "c1", validMeta(3, 2)},
			"total 21":     {"linux", "callsheet-e2e:r1", "c1", validMeta(1, 21)},
			"bad tag":      {"linux", "Bad Tag", "c1", validMeta(1, 1)},
			"bad name":     {"linux", "callsheet-e2e:r1", "-c", validMeta(1, 1)},
			"bad run ID":   {"linux", "callsheet-e2e:r1", "c1", func() ContainerMetadata { m := validMeta(1, 1); m.RunID = "A B"; return m }()},
			"short rev":    {"linux", "callsheet-e2e:r1", "c1", func() ContainerMetadata { m := validMeta(1, 1); m.SourceRevision = "abc"; return m }()},
			"arch":         {"linux", "callsheet-e2e:r1", "c1", func() ContainerMetadata { m := validMeta(1, 1); m.Architecture = "riscv64"; return m }()},
			"hash missing": {"linux", "callsheet-e2e:r1", "c1", func() ContainerMetadata { m := validMeta(1, 1); delete(m.BinaryHashes, HashCallsheet); return m }()},
			"hash case": {"linux", "callsheet-e2e:r1", "c1", func() ContainerMetadata {
				m := validMeta(1, 1)
				m.BinaryHashes[HashCallsheet] = strings.Repeat("A", 64)
				return m
			}()},
		} {
			if _, err := ContainerPlan(c.goos, c.tag, c.cname, c.meta); err == nil {
				t.Fatalf("%s planned", name)
			}
		}
		if _, err := ContainerPlan("darwin", "callsheet-e2e:r1", "c1", validMeta(1, 1)); err == nil || err.Error() != `container-e2e is unsupported on "darwin"` {
			t.Fatalf("darwin plan = %v", err)
		}
	})
	t.Run("standalone", func(t *testing.T) {
		cr := newContainerRunnerFixture(t, &fakeRunner{}).(*containerRunner)
		code, out, errOut, dir := runContainer(t, context.Background(), "linux", cr, "container-e2e")
		if code != 0 || !strings.Contains(out, "devcheck: container-e2e: iteration 1 of 1 validated (15 records") || !strings.Contains(out, "stage container-e2e ok") {
			t.Fatalf("container-e2e = %d %s %s", code, out, errOut)
		}
		if got := strings.Join(cr.containerKinds(), "|"); got != strings.Join(oneIterationKinds, "|") {
			t.Fatalf("calls = %s", got)
		}
		// The static offline build of the three binaries.
		calls := cr.argvs()
		for i, want := range []string{"go build -trimpath -o ", "go build -trimpath -o ", "go test -c -trimpath -tags=containeracceptance -o "} {
			if !strings.HasPrefix(calls[2+i], want) {
				t.Fatalf("build %d = %s", i, calls[2+i])
			}
			env := strings.Join(cr.calls[2+i].env, "\n")
			for _, kv := range []string{"CGO_ENABLED=0", "GOOS=linux", "GOPROXY=off", "GOSUMDB=off", "GOFLAGS=-mod=readonly"} {
				if !strings.Contains("\n"+env+"\n", "\n"+kv+"\n") {
					t.Fatalf("build %d env lacks %s", i, kv)
				}
			}
		}
		// Docker children use a private client configuration and the
		// probed endpoint; the image build runs in the context directory
		// holding exactly the explicit artifacts.
		for _, c := range cr.calls {
			if c.argv[0] != "docker" || c.argv[1] == "context" {
				continue
			}
			env := strings.Join(c.env, "\n")
			if !strings.Contains(env, "DOCKER_CONFIG=") || !strings.Contains(env, "DOCKER_HOST=unix:///synthetic/docker.sock") {
				t.Fatalf("%v env lacks the private docker configuration", c.argv)
			}
			if c.argv[1] == "build" {
				names := cr.context
				if !strings.HasSuffix(c.dir, string(filepath.Separator)+"container-context") {
					t.Fatalf("image build runs in %s", c.dir)
				}
				if strings.Join(names, " ") != "Dockerfile acceptance.test callsheet fake-adapter fixtures/docs/ci.md fixtures/docs/real-adapters.md fixtures/docs/workspaces.md "+
					"fixtures/manuals/proof-a/instruction.md fixtures/manuals/proof-a/runbook.md fixtures/manuals/proof-b/instruction.md fixtures/manuals/proof-b/runbook.md "+
					"fixtures/manuals/proof-code/instruction.md fixtures/manuals/proof-code/runbook.md fixtures/manuals/proof-code-review/instruction.md fixtures/manuals/proof-code-review/runbook.md" {
					t.Fatalf("build context = %v", names)
				}
			}
		}
		report := readEvidence(t, dir, evidenceReportFile)
		if !strings.Contains(report, "outcome: pass\n") || !strings.Contains(report, "stage: container-e2e\n") || !strings.Contains(report, "ledger: 15 records\n") ||
			strings.Count(readEvidence(t, dir, evidenceLedgerFile), "\n") != 15 {
			t.Fatalf("report:\n%s", report)
		}
	})
	t.Run("count", func(t *testing.T) {
		cr := newContainerRunnerFixture(t, &fakeRunner{}).(*containerRunner)
		code, out, errOut, dir := runContainer(t, context.Background(), "linux", cr, "container-e2e", "--count=3")
		if code != 0 {
			t.Fatalf("count 3 = %d %s", code, errOut)
		}
		var runs, builds, imageRemovals int
		for i, a := range cr.argvs() {
			switch {
			case strings.HasPrefix(a, "docker run "):
				runs++
				if !strings.Contains(a, "--env CALLSHEET_E2E_ITERATION="+string(rune('0'+runs))+" --env CALLSHEET_E2E_TOTAL=3 ") || !strings.Contains(a, " -test.count=1 ") {
					t.Fatalf("run %d = %s", runs, a)
				}
			case strings.HasPrefix(a, "docker build "):
				builds++
			case strings.HasPrefix(a, "docker image rm "):
				imageRemovals++
				if i != len(cr.argvs())-2 {
					t.Fatalf("image removed before the last iteration")
				}
			}
		}
		if runs != 3 || builds != 1 || imageRemovals != 1 || strings.Count(readEvidence(t, dir, evidenceLedgerFile), "\n") != 45 ||
			strings.Count(out, "validated (15 records") != 3 {
			t.Fatalf("runs %d builds %d image removals %d\n%s", runs, builds, imageRemovals, out)
		}
		// Every argument error is exit 2 before any child.
		for _, args := range [][]string{{"container-e2e", "--count=0"}, {"container-e2e", "--count=21"}, {"container-e2e", "--count=two"}, {"container-e2e", "extra"},
			{"test", "--count=1"}, {"all", "--count=2"}, {"bench", "-count=1"}} {
			f := &fakeRunner{}
			code, _, errOut, dir := runContainer(t, context.Background(), "linux", f, args...)
			if code != 2 || len(f.calls) != 0 || !strings.Contains(errOut, "invalid arguments for "+args[0]) {
				t.Fatalf("%v = %d %d %s", args, code, len(f.calls), errOut)
			}
			if _, err := os.Stat(dir); !os.IsNotExist(err) {
				t.Fatalf("%v created the evidence directory", args)
			}
		}
	})
	t.Run("routing", func(t *testing.T) {
		// Non-Linux: refused at planning, before scratch, evidence or any
		// child.
		for _, goos := range []string{"darwin", "windows", ""} {
			f := &fakeRunner{}
			code, out, errOut, dir := runContainer(t, context.Background(), goos, f, "container-e2e")
			if code != 1 || len(f.calls) != 0 || out != "" || errOut != "devcheck: stage container-e2e FAILED: container-e2e is unsupported on \""+goos+"\"\n" {
				t.Fatalf("%q = %d %q %q", goos, code, out, errOut)
			}
			if _, err := os.Stat(dir); !os.IsNotExist(err) {
				t.Fatalf("%q created the evidence directory", goos)
			}
		}
		// Darwin test and all never invoke the container operation nor
		// create evidence.
		for _, stage := range []string{"test", "all"} {
			f := &fakeRunner{coverTotal: "90%", cmdList: cmdList, profile: goodProfile}
			code, _, errOut, dir := runContainer(t, context.Background(), "darwin", f, stage)
			if code != 0 {
				t.Fatalf("darwin %s = %d %s", stage, code, errOut)
			}
			for _, c := range f.argvs() {
				if strings.HasPrefix(c, "docker") || strings.Contains(c, "container-context") {
					t.Fatalf("darwin %s ran %s", stage, c)
				}
			}
			if _, err := os.Stat(dir); !os.IsNotExist(err) {
				t.Fatalf("darwin %s created the evidence directory", stage)
			}
		}
		// Linux test: the four plan commands first, then the container
		// operation; a failed plan command starts no container and leaves
		// the report "not started" with its failure phase.
		f := &fakeRunner{fail: "-race"}
		cr := newContainerRunnerFixture(t, f).(*containerRunner)
		code, _, errOut, dir := runContainer(t, context.Background(), "linux", cr, "test")
		if code != 1 || len(cr.calls) != 0 || !strings.Contains(errOut, "stage test FAILED: test -race failed") {
			t.Fatalf("race failure = %d %v %s", code, cr.argvs(), errOut)
		}
		report := readEvidence(t, dir, evidenceReportFile)
		if !strings.Contains(report, "container: not started\n") || !strings.Contains(report, "failed before container setup: test -race") ||
			!strings.Contains(report, "outcome: fail\n") {
			t.Fatalf("report:\n%s", report)
		}
		cr = newContainerRunnerFixture(t, &fakeRunner{}).(*containerRunner)
		if code, _, errOut, _ := runContainer(t, context.Background(), "linux", cr, "test"); code != 0 ||
			!cr.ordered("go test -race -tags=realadaptercheck ./internal/sidecar", "docker context inspect") {
			t.Fatalf("linux test = %d %s %v", code, errOut, cr.all)
		}
	})
	t.Run("docker-failures", func(t *testing.T) {
		for _, c := range []struct {
			name  string
			set   func(*containerRunner)
			cause string
		}{
			{"missing", func(c *containerRunner) { c.missingDocker = true }, "docker executable not found"},
			{"daemon", func(c *containerRunner) { c.daemonDown = true }, "docker daemon unavailable"},
		} {
			// Standalone, inside test and inside all: the exact first line,
			// then the retained-logs line; never a skip.
			for _, stage := range []string{"container-e2e", "test", "all"} {
				cr := newContainerRunnerFixture(t, &fakeRunner{coverTotal: "90%", cmdList: cmdList, profile: goodProfile}).(*containerRunner)
				c.set(cr)
				code, out, errOut, dir := runContainer(t, context.Background(), "linux", cr, stage)
				first := "devcheck: stage container-e2e FAILED: " + c.cause
				if stage != "container-e2e" {
					first = "devcheck: stage test FAILED: container-e2e: " + c.cause
				}
				lines := strings.Split(errOut, "\n")
				if code != 1 || lines[0] != first || lines[1] != "devcheck: logs retained in "+scratchFrom(out) || strings.Contains(out, "stage "+stage+" ok") {
					t.Fatalf("%s %s = %d %q", c.name, stage, code, errOut)
				}
				if r := readEvidence(t, dir, evidenceReportFile); !strings.Contains(r, "outcome: fail\n") || !strings.Contains(r, c.cause) {
					t.Fatalf("%s %s report:\n%s", c.name, stage, r)
				}
				for _, a := range cr.argvs() {
					if strings.HasPrefix(a, "docker run") || strings.HasPrefix(a, "go ") {
						t.Fatalf("%s %s went on to %s", c.name, stage, a)
					}
				}
			}
		}
	})
	t.Run("child-failures", func(t *testing.T) {
		// A failed case record and a partial stream (the container died after
		// three records) of iteration 1.
		failedCase := func(_ int, out []byte) []byte {
			return []byte(replaceRecord(out, CaseLost, func(l string) string {
				return strings.Replace(strings.Replace(l, `"outcome":"pass"`, `"outcome":"fail"`, 1), `"error":""`, `"error":"lost: synthetic"`, 1)
			}))
		}
		partial := func(_ int, out []byte) []byte {
			lines := strings.SplitAfter(string(out), "\n")
			var b strings.Builder
			n := 0
			for _, l := range lines {
				if strings.HasPrefix(l, ContainerEvidencePrefix) {
					if n == 3 {
						break
					}
					n++
				}
				b.WriteString(l)
			}
			return []byte(b.String())
		}
		for _, c := range []struct {
			name, want string
			set        func(*containerRunner)
			ran        []string
			// ledger is the number of retained records: every received
			// record of the failed iteration, whatever the verdict.
			ledger int
		}{
			// A failed static build stops before any image exists.
			{"build", "synthetic build failure", func(c *containerRunner) { c.buildFail = "./cmd/fake-adapter" }, nil, 0},
			{"nonzero exit", "container iteration 1: container run failed", func(c *containerRunner) { c.runExit = errors.New("exit status 1") },
				[]string{"docker container rm", "docker image rm"}, 15},
			{"inspect", `state "exited 1"`, func(c *containerRunner) { c.inspectState = "exited 1" }, []string{"docker container rm", "docker image rm"}, 15},
			{"leftover container", "still present after removal", func(c *containerRunner) { c.leftover = "abc123\n" }, []string{"docker image rm"}, 15},
			{"leftover image", "image removal check: still present", func(c *containerRunner) { c.imageLeft = "sha256:1\n" }, []string{"docker image rm"}, 15},
			{"empty output", "missing end record", func(c *containerRunner) { c.stream = func(int, []byte) []byte { return nil } }, []string{"docker container rm"}, 0},
			{"failed case", `case TestContainerLost: outcome "fail"`, func(c *containerRunner) { c.stream = failedCase }, []string{"docker container rm"}, 15},
			{"partial", "missing end record", func(c *containerRunner) { c.stream = partial }, []string{"docker container rm"}, 3},
		} {
			cr := newContainerRunnerFixture(t, &fakeRunner{}).(*containerRunner)
			c.set(cr)
			code, out, errOut, dir := runContainer(t, context.Background(), "linux", cr, "container-e2e")
			if code != 1 || !strings.Contains(errOut, c.want) || strings.Contains(out, "stage container-e2e ok") {
				t.Fatalf("%s = %d %s", c.name, code, errOut)
			}
			ledger := readEvidence(t, dir, evidenceLedgerFile)
			if strings.Count(ledger, "\n") != c.ledger || !strings.Contains(readEvidence(t, dir, evidenceReportFile), fmt.Sprintf("ledger: %d records\n", c.ledger)) {
				t.Fatalf("%s retained %d records, want %d:\n%s", c.name, strings.Count(ledger, "\n"), c.ledger, ledger)
			}
			if c.name == "failed case" && !strings.Contains(ledger, `"error":"lost: synthetic"`) {
				t.Fatalf("the failed case record is not retained:\n%s", ledger)
			}
			for _, r := range c.ran {
				found := false
				for _, a := range cr.argvs() {
					found = found || strings.HasPrefix(a, r)
				}
				if !found {
					t.Fatalf("%s: no %s cleanup in %v", c.name, r, cr.argvs())
				}
			}
			if r := readEvidence(t, dir, evidenceReportFile); !strings.Contains(r, "outcome: fail\n") || strings.Contains(r, "outcome: pass") {
				t.Fatalf("%s report:\n%s", c.name, r)
			}
		}
		// A failed second iteration stops the run: no third container, a
		// failed outcome, and both iterations' received records retained
		// (the failed iteration's included, with its failed cleanup).
		cr := newContainerRunnerFixture(t, &fakeRunner{}).(*containerRunner)
		cr.stream = func(it int, out []byte) []byte {
			if it == 2 {
				return bytes.Replace(out, []byte(`"cleanup_ok":true`), []byte(`"cleanup_ok":false`), 1)
			}
			return out
		}
		code, _, errOut, dir := runContainer(t, context.Background(), "linux", cr, "container-e2e", "--count=3")
		runs := 0
		for _, a := range cr.argvs() {
			if strings.HasPrefix(a, "docker run") {
				runs++
			}
		}
		ledger := readEvidence(t, dir, evidenceLedgerFile)
		if code != 1 || runs != 2 || !strings.Contains(errOut, "cleanup_ok is false") || strings.Count(ledger, "\n") != 30 {
			t.Fatalf("second iteration failure = %d runs=%d %s\n%s", code, runs, errOut, ledger)
		}
		if strings.Count(ledger, `"iteration":2,`) != 15 || !strings.Contains(ledger, `"cleanup_ok":false`) {
			t.Fatalf("the failed second iteration's records are not retained:\n%s", ledger)
		}
		if r := readEvidence(t, dir, evidenceReportFile); !strings.Contains(r, "outcome: fail\n") || !strings.Contains(r, "ledger: 30 records\n") ||
			!strings.Contains(r, `"cleanup_ok":false`) {
			t.Fatalf("report:\n%s", r)
		}
	})
	t.Run("deadline", func(t *testing.T) {
		// An expired deadline stops and removes the container explicitly
		// (a fresh cleanup context), not merely the docker client.
		cr := newContainerRunnerFixture(t, &fakeRunner{}).(*containerRunner)
		cr.hangRun = true
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		code, _, errOut, _ := runContainer(t, ctx, "linux", cr, "container-e2e")
		calls := cr.argvs()
		var removed bool
		for i, a := range calls {
			if strings.HasPrefix(a, "docker run") {
				removed = i+3 < len(calls) && strings.HasPrefix(calls[i+2], "docker container rm --force --volumes callsheet-e2e-")
			}
		}
		if code != 1 || !removed || !strings.Contains(errOut, "deadline") {
			t.Fatalf("deadline = %d removed=%v %s %v", code, removed, errOut, calls)
		}
	})
	t.Run("internal", func(t *testing.T) {
		d := &driver{ctx: context.Background(), goos: "linux"}
		if err := d.containerE2E(1); err == nil || !strings.Contains(err.Error(), "not open") {
			t.Fatalf("no evidence = %v", err)
		}
		rep, err := openEvidence(filepath.Join(t.TempDir(), "e"), "container-e2e", 1, time.Now())
		if err != nil {
			t.Fatal(err)
		}
		defer rep.close()
		d.evidence = rep
		for _, n := range []int{0, 21} {
			if err := d.containerE2E(n); err == nil {
				t.Fatalf("count %d accepted", n)
			}
		}
		d.goos = "darwin"
		if err := d.containerE2E(1); err == nil {
			t.Fatal("darwin accepted")
		}
	})
}

// streamFor is the synthetic passing stream of meta.
func streamFor(meta ContainerMetadata) []byte {
	return ca.SyntheticStream(ca.Common{RunID: meta.RunID, Iteration: meta.Iteration, Total: meta.Total, SourceRevision: meta.SourceRevision,
		Arch: meta.Architecture, BinaryHashes: meta.BinaryHashes})
}

// replaceRecord rewrites the record of caseID in stream with f applied to
// its JSON line.
func replaceRecord(stream []byte, caseID string, f func(string) string) string {
	lines := strings.SplitAfter(string(stream), "\n")
	for i, l := range lines {
		if strings.HasPrefix(l, ContainerEvidencePrefix) && strings.Contains(l, `"case_id":"`+caseID+`"`) {
			lines[i] = f(l)
		}
	}
	return strings.Join(lines, "")
}

// TestContainerEvidenceContract (FP-10 host contract): complete synthetic
// ledgers and events pass, including several iterations; every schema,
// manifest, event, catalog and relationship defect is rejected with its
// case, task index and field named; a nonzero exit or failed cleanup
// cannot pass the driver even with a complete ledger.
func TestContainerEvidenceContract(t *testing.T) {
	// The fixed manifest: 14 case IDs (eleven parents and three
	// partial-result subcases), the eight required subtests (the
	// coordinator's background_wait since design
	// nonblocking-coordinator-waits: 3+2+3), fresh copies.
	ids, subs := ContainerCaseIDs(), ContainerSubcases()
	parents := 0
	for _, id := range ids {
		if !strings.Contains(id, "/") {
			parents++
		}
	}
	if len(ids) != 14 || parents != 11 || len(subs[CaseCoordinator]) != 3 || len(subs[CaseContinuation]) != 2 || len(subs[CasePartialResults]) != 3 ||
		len(subs[CaseCoordinator])+len(subs[CaseContinuation])+len(subs[CasePartialResults]) != 8 || strings.Join(subs[CaseCoordinator], ",") != "prepare,goal_answer,background_wait" ||
		ContainerBoundary(CasePublicationRestart) != "after_acknowledged_publication_before_coordinator_delivery" || ContainerBoundary(CaseClaims) != "" {
		t.Fatalf("manifest %v %v", ids, subs)
	}
	ids[0], subs[CaseCoordinator][0] = "x", "x"
	if ContainerCaseIDs()[0] != CaseRuntime || ContainerSubcases()[CaseCoordinator][0] != "prepare" {
		t.Fatal("the manifest accessors expose shared state")
	}
	meta := validMeta(1, 1)
	exp := containerExpectation(meta)
	valid := streamFor(meta)
	if err := ValidateContainerEvidence(bytes.NewReader(valid), exp); err != nil {
		t.Fatalf("complete synthetic stream: %v", err)
	}
	reject := func(name, stream string, wants ...string) {
		t.Helper()
		err := ValidateContainerEvidence(strings.NewReader(stream), exp)
		if err == nil {
			t.Fatalf("%s accepted", name)
		}
		for _, w := range wants {
			if !strings.Contains(err.Error(), w) {
				t.Fatalf("%s: %v lacks %q", name, err, w)
			}
		}
	}
	vs := string(valid)
	drop := func(match string) string {
		var b strings.Builder
		for _, l := range strings.SplitAfter(vs, "\n") {
			if !strings.Contains(l, match) {
				b.WriteString(l)
			}
		}
		return b.String()
	}
	t.Run("manifest", func(t *testing.T) {
		pub := ""
		for _, l := range strings.SplitAfter(vs, "\n") {
			if strings.Contains(l, `"case_id":"TestContainerPublication"`) {
				pub = l
			}
		}
		reject("missing case", drop(`"case_id":"TestContainerLost"`), "case TestContainerLost: missing case record")
		reject("duplicate case", strings.Replace(vs, pub, pub+pub, 1), "duplicate case record for iteration 1")
		reject("unknown case", strings.Replace(vs, `"case_id":"TestContainerClaims"`, `"case_id":"TestContainerExtra"`, 1), "case TestContainerExtra: unknown case ID",
			"case TestContainerClaims: missing case record")
		reject("missing end", drop(`"kind":"end"`), "missing end record")
		reject("duplicate end", vs+strings.SplitAfter(vs, "PASS\n")[1], "a record follows the end record")
		reject("record after end", vs+pub, "a record follows the end record")
		reject("end manifest", strings.Replace(vs, `"TestContainerClaims","TestContainerContinuation"`, `"TestContainerContinuation"`, 1), "case_ids")
		reject("cleanup failed", strings.Replace(vs, `"cleanup_ok":true`, `"cleanup_ok":false`, 1), "end record: cleanup_ok is false")
		reject("end failed", strings.Replace(vs, `"cleanup_ok":true,"error":""`, `"cleanup_ok":true,"error":"boom"`, 1), "end record: outcome")
		reject("failed case", replaceRecord(valid, CaseLost, func(l string) string {
			return strings.Replace(strings.Replace(l, `"outcome":"pass"`, `"outcome":"fail"`, 1), `"error":""`, `"error":"x"`, 1)
		}), "case TestContainerLost: outcome \"fail\"")
		reject("subcase failed", strings.Replace(vs, `"goal_answer":"pass"`, `"goal_answer":"fail"`, 1), "subcase goal_answer is \"fail\"")
		reject("subcase missing", strings.Replace(vs, `"failed":"pass",`, ``, 1), "case TestContainerPartialResults: subcases")
		// The coordinator's background-wait proof is mandatory: a record
		// without it, or with it failed, never passes.
		reject("background_wait missing", strings.Replace(vs, `"background_wait":"pass",`, ``, 1), "case TestContainerCoordinator: subcases")
		reject("background_wait failed", strings.Replace(vs, `"background_wait":"pass"`, `"background_wait":"fail"`, 1), "subcase background_wait is \"fail\"")
		reject("subcase extra", strings.Replace(vs, `"subcases":{}`, `"subcases":{"x":"pass"}`, 1), "subcases map[x:pass], want exactly []")
		reject("boundary", strings.Replace(vs, BoundaryPublicationRestart, "anywhere", 1), "boundary \"anywhere\"")
	})
	t.Run("schema", func(t *testing.T) {
		rt := func(f func(string) string) string { return replaceRecord(valid, CaseRuntime, f) }
		reject("malformed JSON", rt(func(l string) string { return strings.Replace(l, `{"adapter"`, `{"adapter`, 1) }), "line ", "not one compact JSON object")
		reject("non-compact", rt(func(l string) string { return strings.Replace(l, `{"adapter":`, `{ "adapter":`, 1) }), "not one compact JSON object")
		reject("truncated", strings.TrimSuffix(vs, "\n")[:len(vs)-30], "truncated")
		reject("unknown field", rt(func(l string) string { return strings.Replace(l, `{"adapter":`, `{"zzz":1,"adapter":`, 1) }), "unknown field \"zzz\"")
		reject("missing field", rt(func(l string) string { return strings.Replace(l, `"boundary":"",`, ``, 1) }), "missing field \"boundary\"")
		reject("duplicate field", rt(func(l string) string {
			return strings.Replace(l, `{"adapter":"fake",`, `{"adapter":"fake","adapter":"fake",`, 1)
		}),
			"duplicate field \"adapter\"")
		reject("string iteration", rt(func(l string) string { return strings.Replace(l, `"iteration":1`, `"iteration":"1"`, 1) }), "iteration must be an integer")
		reject("float iteration", rt(func(l string) string { return strings.Replace(l, `"iteration":1`, `"iteration":1.0`, 1) }), "iteration must be an integer")
		reject("schema version", rt(func(l string) string { return strings.Replace(l, `"schema_version":1`, `"schema_version":2`, 1) }), "schema_version must be 1")
		reject("kind", rt(func(l string) string { return strings.Replace(l, `"kind":"case"`, `"kind":"note"`, 1) }), "unknown kind \"note\"")
		reject("bindings elsewhere", replaceRecord(valid, CaseClaims, func(l string) string {
			return strings.Replace(l, `"kind":"case"`, `"kind":"case","node_bindings":{"a":"`+ca.SyntheticNodeA+`","b":"`+ca.SyntheticNodeB+`"}`, 1)
		}), "node_bindings is allowed only on TestContainerRuntime")
		cut := func(l, repl string) string {
			i := strings.Index(l, `"node_bindings":`)
			j := strings.Index(l[i:], "},") + i + 2
			return l[:i] + repl + l[j:]
		}
		reject("bindings null", rt(func(l string) string { return cut(l, `"node_bindings":null,`) }), "field node_bindings must be an object")
		reject("bindings absent", rt(func(l string) string { return cut(l, "") }), "missing field node_bindings")
		reject("bindings same", rt(func(l string) string {
			return strings.Replace(l, `"b":"`+ca.SyntheticNodeB, `"b":"`+ca.SyntheticNodeA, 1)
		}), "same node")
		reject("bindings invalid", rt(func(l string) string { return strings.Replace(l, `"b":"`+ca.SyntheticNodeB, `"b":"n_x`, 1) }), "invalid node ID")
		reject("bindings late", drop(`"case_id":"TestContainerRuntime"`), "precedes the TestContainerRuntime node bindings")
		// Prefix-less records and records in ordinary log lines never count.
		reject("log line", strings.Replace(vs, "\n"+ContainerEvidencePrefix+`{"adapter":"fake","arch":"amd64","binary_hashes"`, "\nlog: "+ContainerEvidencePrefix+`{"adapter":"fake","arch":"amd64","binary_hashes"`, 1),
			"case TestContainerRuntime: missing case record")
		for field, mut := range map[string][2]string{
			"run_id":          {`"run_id":"` + meta.RunID + `"`, `"run_id":"other"`},
			"iteration":       {`"iteration":1`, `"iteration":2`},
			"total":           {`"total":1`, `"total":2`},
			"source_revision": {`"source_revision":"` + meta.SourceRevision + `"`, `"source_revision":"` + strings.Repeat("b", 40) + `"`},
			"os":              {`"os":"linux"`, `"os":"darwin"`},
			"arch":            {`"arch":"amd64"`, `"arch":"arm64"`},
			"adapter":         {`"adapter":"fake"`, `"adapter":"claude"`},
			"binary_hashes":   {strings.Repeat("1", 64), strings.Repeat("9", 64)},
		} {
			reject("common "+field, rt(func(l string) string { return strings.Replace(l, mut[0], mut[1], 1) }), "case TestContainerRuntime: "+field)
		}
	})
	t.Run("events", func(t *testing.T) {
		reject("skipped subtest", strings.Replace(vs, "--- PASS: TestContainerPartialResults/timed_out", "--- SKIP: TestContainerPartialResults/timed_out", 1),
			"test TestContainerPartialResults/timed_out: SKIP, want PASS")
		reject("failed parent", strings.Replace(vs, "--- PASS: TestContainerLost", "--- FAIL: TestContainerLost", 1), "test TestContainerLost: FAIL, want PASS")
		reject("missing subtest", strings.Replace(vs, "    --- PASS: TestContainerContinuation/sibling (0.01s)\n", "", 1), "test TestContainerContinuation/sibling: no result")
		reject("missing background_wait", strings.Replace(strings.Replace(vs, "    --- PASS: TestContainerCoordinator/background_wait (0.01s)\n", "", 1),
			"=== RUN   TestContainerCoordinator/background_wait\n", "", 1), "test TestContainerCoordinator/background_wait: no run event",
			"test TestContainerCoordinator/background_wait: no result")
		reject("missing run", strings.Replace(vs, "=== RUN   TestContainerClaims\n", "", 1), "test TestContainerClaims: no run event")
		reject("other failure", strings.Replace(vs, "PASS\n"+ContainerEvidencePrefix, "--- FAIL: TestOther (0.00s)\nPASS\n"+ContainerEvidencePrefix, 1), "test TestOther FAIL")
		reject("no PASS", strings.Replace(vs, "\nPASS\n", "\n", 1), "did not report PASS")
		reject("FAIL", strings.Replace(vs, "\nPASS\n", "\nFAIL\n", 1), "did not report PASS")
		reject("panic", strings.Replace(vs, "\nPASS\n", "\npanic: boom\nPASS\n", 1), "did not report PASS")
	})
	t.Run("catalog", func(t *testing.T) {
		recs, _ := ca.SyntheticRecords(ca.Common{RunID: meta.RunID, Iteration: 1, Total: 1, SourceRevision: meta.SourceRevision, Arch: "amd64", BinaryHashes: meta.BinaryHashes})
		byID := map[string]ca.CaseRecord{}
		for _, r := range recs {
			byID[r.CaseID] = r
		}
		// mutate rewrites one task of one case and requires rejection naming
		// the case, task index and field.
		mutate := func(caseID string, idx int, field string, f func(*ca.Task)) {
			t.Helper()
			rec := byID[caseID]
			rec.Tasks = append([]ca.Task(nil), rec.Tasks...)
			f(&rec.Tasks[idx])
			var line bytes.Buffer
			ca.WriteRecord(&line, ca.Record{Case: &rec})
			stream := replaceRecord(valid, caseID, func(string) string { return line.String() })
			reject(caseID+" "+field, stream, "case "+caseID+" task "+strconv.Itoa(idx)+" field "+field)
		}
		sp := func(s string) *string { return &s }
		ip := func(n int) *int { return &n }
		mutate(CaseCoordinator, 0, "label", func(x *ca.Task) { x.Label = "answer-z" })
		mutate(CaseCoordinator, 1, "role_id", func(x *ca.Task) { x.RoleID = "proof-a" })
		mutate(CaseCoordinator, 1, "node_id", func(x *ca.Task) { x.NodeID = ca.SyntheticNodeA })
		mutate(CaseCoordinator, 0, "task_id", func(x *ca.Task) { x.TaskID = "t_bad" })
		mutate(CaseCoordinator, 1, "task_id", func(x *ca.Task) { x.TaskID = byID[CaseCoordinator].Tasks[0].TaskID })
		mutate(CaseCoordinator, 0, "terminal_state", func(x *ca.Task) { x.TerminalState = "failed" })
		mutate(CaseCoordinator, 0, "publication_status", func(x *ca.Task) { x.PublicationStatus = sp("published") })
		mutate(CaseCoordinator, 0, "exit_code", func(x *ca.Task) { x.ExitCode = ip(1) })
		mutate(CaseCoordinator, 0, "exit_code", func(x *ca.Task) { x.ExitCode = nil })
		mutate(CaseCoordinator, 0, "final_message", func(x *ca.Task) { x.FinalMessage = sp("pong") })
		mutate(CaseCoordinator, 0, "final_message", func(x *ca.Task) { x.FinalMessage = nil })
		mutate(CaseCoordinator, 0, "workspace_instance", func(x *ca.Task) { x.WorkspaceInstance = sp(strings.Repeat("c", 32)) })
		mutate(CaseCoordinator, 0, "base_commit", func(x *ca.Task) { x.BaseCommit = sp(strings.Repeat("c", 40)) })
		mutate(CaseCoordinator, 0, "result_commit", func(x *ca.Task) { x.ResultCommit = sp(strings.Repeat("c", 40)) })
		mutate(CaseCoordinator, 0, "result_ref", func(x *ca.Task) { x.ResultRef = sp("refs/callsheet/tasks/x") })
		mutate(CasePublication, 0, "workspace_instance", func(x *ca.Task) { x.WorkspaceInstance = nil })
		mutate(CasePublication, 0, "workspace_instance", func(x *ca.Task) { x.WorkspaceInstance = sp("XYZ") })
		mutate(CasePublication, 0, "base_commit", func(x *ca.Task) { x.BaseCommit = nil })
		mutate(CasePublication, 0, "base_commit", func(x *ca.Task) { x.BaseCommit = sp("abc") })
		mutate(CasePublication, 0, "result_commit", func(x *ca.Task) { x.ResultCommit = nil })
		mutate(CasePublication, 0, "result_ref", func(x *ca.Task) { x.ResultRef = nil })
		mutate(CasePublication, 0, "result_ref", func(x *ca.Task) { x.ResultRef = sp("refs/callsheet/tasks/t_" + strings.Repeat("0", 32)) })
		mutate(CasePublication, 0, "publication_status", func(x *ca.Task) { x.PublicationStatus = sp("failed") })
		mutate(CasePublication, 0, "publication_status", func(x *ca.Task) { x.PublicationStatus = nil })
		mutate(CasePartialFailed, 0, "exit_code", func(x *ca.Task) { x.ExitCode = ip(7) })
		mutate(CasePartialFailed, 0, "final_message", func(x *ca.Task) { x.FinalMessage = sp(ca.FinalMessage) })
		mutate(CasePartialCancelled, 0, "exit_code", func(x *ca.Task) { x.ExitCode = ip(0) })
		mutate(CasePartialCancelled, 0, "terminal_state", func(x *ca.Task) { x.TerminalState = "succeeded" })
		mutate(CasePartialTimedOut, 0, "final_message", func(x *ca.Task) { x.FinalMessage = sp(ca.FinalMessage) })
		mutate(CaseLost, 0, "result_commit", func(x *ca.Task) { x.ResultCommit = sp(strings.Repeat("d", 40)) })
		mutate(CaseLost, 0, "result_ref", func(x *ca.Task) { x.ResultRef = sp("refs/callsheet/tasks/" + x.TaskID) })
		mutate(CaseLost, 0, "publication_status", func(x *ca.Task) { x.PublicationStatus = sp("published") })
		mutate(CaseLost, 0, "workspace_instance", func(x *ca.Task) { x.WorkspaceInstance = nil })
		mutate(CaseLost, 0, "base_commit", func(x *ca.Task) { x.BaseCommit = nil })
		// Relationships: the chain links, the sibling's base and the shared
		// instance and base.
		mutate(CaseSampleFlow, 2, "base_commit", func(x *ca.Task) { x.BaseCommit = sp(strings.Repeat("e", 40)) })
		mutate(CaseSampleFlow, 3, "workspace_instance", func(x *ca.Task) { x.WorkspaceInstance = sp(strings.Repeat("e", 32)) })
		mutate(CaseContinuation, 1, "base_commit", func(x *ca.Task) { x.BaseCommit = sp(strings.Repeat("e", 40)) })
		mutate(CaseContinuation, 2, "base_commit", func(x *ca.Task) { x.BaseCommit = byID[CaseContinuation].Tasks[0].ResultCommit })
		mutate(CaseContinuation, 2, "workspace_instance", func(x *ca.Task) { x.WorkspaceInstance = sp(strings.Repeat("e", 32)) })
		mutate(CaseLost, 1, "base_commit", func(x *ca.Task) { x.BaseCommit = sp(strings.Repeat("e", 40)) })
		mutate(CasePublicationRestart, 1, "workspace_instance", func(x *ca.Task) { x.WorkspaceInstance = sp(strings.Repeat("e", 32)) })
		// Missing, extra and reordered items.
		count := func(caseID string, f func([]ca.Task) []ca.Task, want string) {
			t.Helper()
			rec := byID[caseID]
			rec.Tasks = f(append([]ca.Task(nil), rec.Tasks...))
			var line bytes.Buffer
			ca.WriteRecord(&line, ca.Record{Case: &rec})
			reject(caseID+" "+want, replaceRecord(valid, caseID, func(string) string { return line.String() }), "case "+caseID+want)
		}
		count(CaseSampleFlow, func(ts []ca.Task) []ca.Task { return ts[:3] }, ": 3 tasks, want 4")
		count(CaseClaims, func([]ca.Task) []ca.Task { return byID[CasePublication].Tasks }, ": 1 tasks, want 0")
		count(CaseContinuation, func(ts []ca.Task) []ca.Task { return []ca.Task{ts[0], ts[2], ts[1]} }, " task 1 field label")
		count(CasePublication, func(ts []ca.Task) []ca.Task { return append(ts, ts[0]) }, ": 2 tasks, want 1")
	})
	t.Run("iterations", func(t *testing.T) {
		var all [][]byte
		for it := 1; it <= 3; it++ {
			m := validMeta(it, 3)
			raws, err := validateContainerStream(bytes.NewReader(streamFor(m)), containerExpectation(m))
			if err != nil || len(raws) != 15 {
				t.Fatalf("iteration %d: %d records %v", it, len(raws), err)
			}
			all = append(all, raws...)
		}
		if err := validateContainerLedger(all, 3, ContainerCaseIDs()); err != nil {
			t.Fatalf("three iterations: %v", err)
		}
		// Iteration 2's stream is not iteration 1's container.
		if err := ValidateContainerEvidence(bytes.NewReader(streamFor(validMeta(2, 3))), containerExpectation(validMeta(1, 3))); err == nil ||
			!strings.Contains(err.Error(), "iteration 2, want 1") {
			t.Fatalf("wrong container: %v", err)
		}
		for name, c := range map[string]struct {
			raws  [][]byte
			total int
			want  string
		}{
			"short":         {all[:30], 3, "iteration 3 has no end record"},
			"duplicate":     {append(append([][]byte{}, all...), all[0]), 3, "duplicate case TestContainerRuntime in iteration 1"},
			"duplicate end": {append(append([][]byte{}, all...), all[14]), 3, "duplicate end record for iteration 1"},
			"out of range":  {all, 2, "outside 1..2"},
			"malformed":     {append(append([][]byte{}, all...), []byte("{")), 3, "ledger record 46"},
		} {
			if err := validateContainerLedger(c.raws, c.total, ContainerCaseIDs()); err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("%s: %v", name, err)
			}
		}
	})
	t.Run("driver", func(t *testing.T) {
		// A complete ledger with a nonzero exit, or a failed cleanup, never
		// passes the driver.
		for name, set := range map[string]func(*containerRunner){
			"exit":    func(c *containerRunner) { c.runExit = errors.New("exit status 1") },
			"cleanup": func(c *containerRunner) { c.leftover = "deadbeef\n" },
			"skip": func(c *containerRunner) {
				c.stream = func(_ int, b []byte) []byte {
					return bytes.Replace(b, []byte("--- PASS: TestContainerSampleFlow"), []byte("--- SKIP: TestContainerSampleFlow"), 1)
				}
			},
		} {
			cr := newContainerRunnerFixture(t, &fakeRunner{}).(*containerRunner)
			set(cr)
			if code, _, errOut, _ := runContainer(t, context.Background(), "linux", cr, "container-e2e"); code != 1 {
				t.Fatalf("%s passed: %s", name, errOut)
			}
		}
		if err := ValidateContainerEvidence(bytes.NewReader(valid), ContainerExpectation{Metadata: validMeta(0, 1)}); err == nil {
			t.Fatal("invalid expectation accepted")
		}
	})
}
