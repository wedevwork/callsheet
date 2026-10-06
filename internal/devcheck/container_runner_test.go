package devcheck

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/wedevwork/callsheet/internal/testkit"
	ca "github.com/wedevwork/callsheet/internal/testkit/containeracceptance"
)

// containerRunner is the synthetic container Runner fixture: it delegates
// every ordinary Go suite, coverage, bench and cross call to base and
// simulates only the container operation's docker and static build
// commands. Each simulated docker run emits, from the invocation's own
// --env metadata, the clearly synthetic complete stream of one passing
// iteration (containeracceptance.SyntheticStream: 14 case records, the
// eleven parents' and every required subtest's pass events and the end
// record after cleanup). It never launches Docker and writes only test
// temporary files. Fields script failures; calls holds the container calls.
type containerRunner struct {
	base runnerLike

	mu    sync.Mutex
	calls []recorded
	// all is every call (delegated or simulated), joined, in order.
	all []string
	// context lists the image build context's files at build time.
	context []string

	// Failure scripts (all zero: a complete passing run).
	missingDocker bool   // docker context inspect: executable not found
	daemonDown    bool   // docker version fails
	buildFail     string // a container build whose argv contains this fails
	runExit       error  // docker run's error (after its output)
	hangRun       bool   // docker run blocks until its context ends
	inspectState  string // container inspect output (default "exited 0")
	leftover      string // container removal check output
	imageLeft     string // image removal check output
	// stream rewrites the synthetic output of iteration it (nil: none).
	stream func(it int, out []byte) []byte
}

// newContainerRunnerFixture wraps base with the synthetic container
// commands.
func newContainerRunnerFixture(t *testing.T, base runnerLike) runnerLike {
	t.Helper()
	return &containerRunner{base: base}
}

// isContainerCall reports whether argv is a container operation command:
// any docker command or a static container build into the context
// directory.
func isContainerCall(argv []string) bool {
	if len(argv) > 0 && argv[0] == "docker" {
		return true
	}
	for _, a := range argv {
		if strings.Contains(a, "/container-context/") {
			return true
		}
	}
	return false
}

// envOf returns the docker run --env values by name.
func envOf(argv []string) map[string]string {
	out := map[string]string{}
	for i := 0; i+1 < len(argv); i++ {
		if argv[i] == "--env" {
			k, v, _ := strings.Cut(argv[i+1], "=")
			out[k] = v
		}
	}
	return out
}

func (c *containerRunner) run(ctx context.Context, argv, env []string, dir string, stdout, stderr io.Writer) error {
	c.mu.Lock()
	c.all = append(c.all, strings.Join(argv, " "))
	c.mu.Unlock()
	if !isContainerCall(argv) {
		return c.base.run(ctx, argv, env, dir, stdout, stderr)
	}
	c.mu.Lock()
	c.calls = append(c.calls, recorded{argv, env, dir})
	c.mu.Unlock()
	joined := strings.Join(argv, " ")
	switch {
	case argv[0] == "go":
		if c.buildFail != "" && strings.Contains(joined, c.buildFail) {
			io.WriteString(stderr, "synthetic build failure\n")
			return errors.New("exit status 1")
		}
		_, err := testkit.FakeGoBuild(argv, env)
		return err
	case strings.HasPrefix(joined, "docker context inspect"):
		if c.missingDocker {
			return &exec.Error{Name: "docker", Err: exec.ErrNotFound}
		}
		io.WriteString(stdout, "unix:///synthetic/docker.sock\n")
	case strings.HasPrefix(joined, "docker version"):
		if c.daemonDown {
			io.WriteString(stderr, "Cannot connect to the Docker daemon (synthetic)\n")
			return errors.New("exit status 1")
		}
		io.WriteString(stdout, "28.0.0-synthetic\n")
	case strings.HasPrefix(joined, "docker run"):
		if c.hangRun {
			<-ctx.Done()
			return ctx.Err()
		}
		e := envOf(argv)
		it, _ := strconv.Atoi(e[ContainerEnvIter])
		total, _ := strconv.Atoi(e[ContainerEnvTotal])
		var hashes map[string]string
		json.Unmarshal([]byte(e[ContainerEnvHashes]), &hashes)
		out := ca.SyntheticStream(ca.Common{RunID: e[ContainerEnvRunID], Iteration: it, Total: total, SourceRevision: e[ContainerEnvRevision],
			Arch: e[ContainerEnvArch], BinaryHashes: hashes})
		if c.stream != nil {
			out = c.stream(it, out)
		}
		stdout.Write(out)
		return c.runExit
	case strings.HasPrefix(joined, "docker container inspect"):
		state := c.inspectState
		if state == "" {
			state = "exited 0"
		}
		io.WriteString(stdout, state+"\n")
	case strings.HasPrefix(joined, "docker container ls"):
		io.WriteString(stdout, c.leftover)
	case strings.HasPrefix(joined, "docker image ls"):
		io.WriteString(stdout, c.imageLeft)
	case strings.HasPrefix(joined, "docker build"):
		// Capture the build context's files (it is scratch, removed on
		// success).
		filepath.Walk(dir, func(p string, info os.FileInfo, err error) error {
			if err == nil && !info.IsDir() {
				rel, _ := filepath.Rel(dir, p)
				c.mu.Lock()
				c.context = append(c.context, filepath.ToSlash(rel))
				c.mu.Unlock()
			}
			return nil
		})
	case strings.HasPrefix(joined, "docker container rm"), strings.HasPrefix(joined, "docker image rm"):
	default:
		return fmt.Errorf("unexpected container command %q", joined)
	}
	return nil
}

// argvs returns the captured container calls, joined.
func (c *containerRunner) argvs() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	var out []string
	for _, r := range c.calls {
		out = append(out, strings.Join(r.argv, " "))
	}
	return out
}

// ordered reports whether the last call containing a precedes the first
// call containing b.
func (c *containerRunner) ordered(a, b string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	last, first := -1, -1
	for i, call := range c.all {
		if strings.Contains(call, a) {
			last = i
		}
		if first < 0 && strings.Contains(call, b) {
			first = i
		}
	}
	return last >= 0 && first >= 0 && last < first
}

// containerKinds summarizes the captured calls as short kinds in order.
func (c *containerRunner) containerKinds() []string {
	var out []string
	for _, a := range c.argvs() {
		f := strings.Fields(a)
		switch {
		case f[0] == "go":
			out = append(out, "build")
		case f[1] == "container" || f[1] == "image" || f[1] == "context":
			out = append(out, f[1]+" "+f[2])
		default:
			out = append(out, f[1])
		}
	}
	return out
}

// oneIterationKinds is the container call sequence of one passing
// single-iteration run.
var oneIterationKinds = []string{"context inspect", "version", "build", "build", "build", "build", "run", "container inspect", "container rm", "container ls",
	"image rm", "image ls"}
