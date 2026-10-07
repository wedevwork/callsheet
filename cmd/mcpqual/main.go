// Command mcpqual is the developer-only MCP timeout qualification harness
// (iteration 07b): "qualify" measures coordinator clients' effective MCP
// tool-call timeouts against the "serve" probe, "capture" records one
// decoder-free setup session per client for decoder enrollment (design
// decoder-enrollment), and "publish" installs a run's proposed catalog
// patch. It is never shipped, never run in CI (any CI presence refuses
// qualify and capture), and launches vendor model sessions only under
// --allow-model-calls. The logic lives in internal/mcpqual; this
// entrypoint only gathers host inputs.
package main

import (
	"context"
	"crypto/rand"
	"io"
	"os"
	"os/signal"
	"os/user"
	"runtime"
	"syscall"
	"time"

	"github.com/wedevwork/callsheet/internal/mcpqual"
	"github.com/wedevwork/callsheet/internal/mcpqual/procexec"
)

func main() {
	os.Exit(run(os.Args[1:], os.Getenv, os.Stdin, os.Stdout, os.Stderr))
}

// lookupEnv is the presence lookup behind the CI prohibition of qualify
// and capture (design decoder-enrollment); tests replace it.
var lookupEnv = os.LookupEnv

// run is the approved host wrapper (docs/ci.md, Platform code): it only
// forwards runtime.GOOS and runtime.GOARCH.
func run(args []string, getenv func(string) string, stdin io.Reader, stdout, stderr io.Writer) int {
	return runFor(runtime.GOOS, runtime.GOARCH, args, getenv, stdin, stdout, stderr)
}

// runFor assembles the host inputs. The developer fault variable is read
// here and only here.
func runFor(goos, goarch string, args []string, getenv func(string) string, stdin io.Reader, stdout, stderr io.Writer) int {
	sig, err := mcpqual.SignalerFor(getenv(mcpqual.FaultEnv), mcpqual.SysSignaler{})
	if err != nil {
		io.WriteString(stderr, "mcpqual: "+err.Error()+"\n")
		return 2
	}
	host, _ := os.Hostname()
	exe, _ := os.Executable()
	home, _ := os.UserHomeDir()
	name := ""
	if u, err := user.Current(); err == nil {
		name = u.Username
	}
	return mcpqual.Main(context.Background(), mcpqual.Env{
		GOOS: goos, GOARCH: goarch, Args: args, Getenv: getenv, LookupEnv: lookupEnv, Environ: os.Environ(),
		Stdin: stdin, Stdout: stdout, Stderr: stderr,
		Signaler: sig, Launcher: procexec.Launcher{}, Clock: mcpqual.RealClock, Registry: mcpqual.DefaultRegistry(),
		Hostname: host, Executable: exe, Home: home, User: name, Now: time.Now, Rand: rand.Reader,
		Notify: func(ctx context.Context) (context.Context, context.CancelFunc) {
			return signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
		},
	})
}
