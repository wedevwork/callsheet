// Package fakeadapter implements the deterministic fake worker used by adapter
// and supervisor tests. It is a test fixture, not a supported adapter: it never
// calls a model or the network and never reads user configuration.
package fakeadapter

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"
)

const (
	// EnvLifetimeFD names the fixture-only variable that identifies the
	// inherited write end of a supervisor-owned lifetime pipe (Unix only).
	EnvLifetimeFD = "CALLSHEET_FAKE_DESCENDANT_LIFETIME_FD"
	// envReadyFD identifies the descendant's inherited readiness pipe.
	envReadyFD = "CALLSHEET_FAKE_READY_FD"
	// DescendantGrace is how long duration-completion cleanup waits after
	// forwarding TERM before it KILLs the descendant.
	DescendantGrace = 200 * time.Millisecond
	// readinessTimeout bounds how long the parent waits for its descendant.
	readinessTimeout = 10 * time.Second
	// SignalFileSuffix is appended to --signal-file for the descendant.
	SignalFileSuffix = ".grandchild"
)

// Term modes.
const (
	TermExit   = "exit"
	TermIgnore = "ignore"
)

// Options are the validated fake flags.
type Options struct {
	Model, Effort      string
	EchoArgv           bool
	Stdout, Stderr     string
	stdoutSet          bool
	stderrSet          bool
	Duration           time.Duration
	ExitCode           int
	Edit, Content      string
	TermMode           string
	SpawnGrandchild    bool
	GrandchildTermMode string
	ReadyFile          string
	SignalFile         string
	Rest               []string

	descendant bool
}

// UsageError is returned by Parse for invalid options (exit 2).
type UsageError struct{ msg string }

func (e *UsageError) Error() string { return e.msg }

func usagef(format string, a ...any) error { return &UsageError{fmt.Sprintf(format, a...)} }

// Parse validates args (without the program name). No side effects.
func Parse(args []string) (Options, error) {
	var o Options
	fs := flag.NewFlagSet("fake-adapter", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	fs.StringVar(&o.Model, "model", "", "model (echoed verbatim)")
	fs.StringVar(&o.Effort, "effort", "", "effort (echoed verbatim)")
	fs.BoolVar(&o.EchoArgv, "echo-argv", false, "print argv JSON first")
	fs.StringVar(&o.Stdout, "stdout", "", "line for stdout")
	fs.StringVar(&o.Stderr, "stderr", "", "line for stderr")
	fs.DurationVar(&o.Duration, "duration", 0, "run duration after readiness")
	fs.IntVar(&o.ExitCode, "exit-code", 0, "exit code 0-125")
	fs.StringVar(&o.Edit, "edit", "", "cwd-relative file to create/replace")
	fs.StringVar(&o.Content, "content", "", "content for --edit")
	fs.StringVar(&o.TermMode, "term-mode", TermExit, "exit|ignore")
	fs.BoolVar(&o.SpawnGrandchild, "spawn-grandchild", false, "spawn one descendant")
	fs.StringVar(&o.GrandchildTermMode, "grandchild-term-mode", TermExit, "exit|ignore")
	fs.StringVar(&o.ReadyFile, "ready-file", "", "cwd-relative ready JSON")
	fs.StringVar(&o.SignalFile, "signal-file", "", "cwd-relative signal log")
	fs.BoolVar(&o.descendant, "internal-descendant", false, "internal")
	if err := fs.Parse(args); err != nil {
		return Options{}, usagef("%v", err)
	}
	fs.Visit(func(f *flag.Flag) {
		switch f.Name {
		case "stdout":
			o.stdoutSet = true
		case "stderr":
			o.stderrSet = true
		}
	})
	o.Rest = fs.Args()
	if o.Duration < 0 {
		return Options{}, usagef("--duration must be nonnegative")
	}
	if o.ExitCode < 0 || o.ExitCode > 125 {
		return Options{}, usagef("--exit-code must be in 0..125")
	}
	for name, mode := range map[string]string{"--term-mode": o.TermMode, "--grandchild-term-mode": o.GrandchildTermMode} {
		if mode != TermExit && mode != TermIgnore {
			return Options{}, usagef("%s must be exit or ignore", name)
		}
	}
	if o.Content != "" && o.Edit == "" {
		return Options{}, usagef("--content requires --edit")
	}
	for name, p := range map[string]string{"--edit": o.Edit, "--ready-file": o.ReadyFile, "--signal-file": o.SignalFile} {
		if p == "" {
			continue
		}
		if err := validateRelative(p); err != nil {
			return Options{}, usagef("%s: %v", name, err)
		}
	}
	if o.descendant && o.SpawnGrandchild {
		return Options{}, usagef("a descendant cannot spawn descendants")
	}
	if !signalsSupported && (o.SpawnGrandchild || o.SignalFile != "" || o.TermMode == TermIgnore || o.descendant) {
		return Options{}, usagef("signal/process-group mode is unsupported on %s", runtime.GOOS)
	}
	return o, nil
}

// validateRelative rejects absolute paths and any ".." component.
func validateRelative(p string) error {
	if filepath.IsAbs(p) || strings.HasPrefix(p, "/") || strings.HasPrefix(p, `\`) || filepath.VolumeName(p) != "" {
		return errors.New("path must be relative to the working directory")
	}
	for _, part := range strings.FieldsFunc(p, func(r rune) bool { return r == '/' || r == '\\' }) {
		if part == ".." {
			return errors.New("path must not contain '..'")
		}
	}
	if c := path.Clean(filepath.ToSlash(p)); c == "." {
		return errors.New("path must name a file")
	}
	return nil
}

// SignalSource abstracts signal registration so tests can inject signals.
type SignalSource interface {
	Notify(c chan<- os.Signal)
	Stop(c chan<- os.Signal)
}

// Env carries the process environment for Run. Zero fields take OS defaults.
type Env struct {
	Args       []string
	Stdout     io.Writer
	Stderr     io.Writer
	Dir        string
	Getenv     func(string) string
	Environ    func() []string
	Executable string
	Signals    SignalSource
	After      func(time.Duration) <-chan time.Time
	NewFile    func(fd uintptr, name string) *os.File
	PID        int
}

func (e Env) withDefaults() (Env, error) {
	if e.Stdout == nil {
		e.Stdout = io.Discard
	}
	if e.Stderr == nil {
		e.Stderr = io.Discard
	}
	if e.Dir == "" {
		wd, err := os.Getwd()
		if err != nil {
			return e, err
		}
		e.Dir = wd
	}
	if e.Getenv == nil {
		e.Getenv = os.Getenv
	}
	if e.Environ == nil {
		e.Environ = os.Environ
	}
	if e.Executable == "" {
		exe, err := os.Executable()
		if err != nil {
			return e, err
		}
		e.Executable = exe
	}
	if e.Signals == nil {
		e.Signals = osSignals{}
	}
	if e.After == nil {
		e.After = time.After
	}
	if e.NewFile == nil {
		e.NewFile = os.NewFile
	}
	if e.PID == 0 {
		e.PID = os.Getpid()
	}
	return e, nil
}

// ReadyInfo is the JSON published atomically to --ready-file.
type ReadyInfo struct {
	PID           int `json:"pid"`
	DescendantPID int `json:"descendant_pid"`
}

// SignalRecord is one line of --signal-file.
type SignalRecord struct {
	PID    int    `json:"pid"`
	Signal string `json:"signal"`
}

// Run executes the fake and returns its exit code.
func Run(ctx context.Context, env Env) int {
	opts, err := Parse(env.Args)
	if err != nil {
		fmt.Fprintf(env.stderrOrDiscard(), "fake-adapter: %v\n", err)
		return 2
	}
	env, err = env.withDefaults()
	if err != nil {
		fmt.Fprintf(env.Stderr, "fake-adapter: %v\n", err)
		return 1
	}
	root, err := os.OpenRoot(env.Dir)
	if err != nil {
		fmt.Fprintf(env.Stderr, "fake-adapter: open working directory: %v\n", err)
		return 1
	}
	defer root.Close()

	// 1. Register handlers before any other work.
	sigCh := make(chan os.Signal, 8)
	if signalsSupported {
		env.Signals.Notify(sigCh)
		defer env.Signals.Stop(sigCh)
	}

	// A descendant holds its inherited lifetime handle until it exits.
	if opts.descendant {
		if lf := env.inheritedFile(EnvLifetimeFD, "lifetime"); lf != nil {
			defer runtime.KeepAlive(lf)
		}
	}

	// 2. Echo, write, edit.
	if opts.EchoArgv {
		b, _ := marshalNoEscape(map[string][]string{"argv": nonNil(env.Args)})
		env.Stdout.Write(append(b, '\n'))
	}
	if opts.stdoutSet {
		io.WriteString(env.Stdout, opts.Stdout+"\n")
	}
	if opts.stderrSet {
		io.WriteString(env.Stderr, opts.Stderr+"\n")
	}
	if opts.Edit != "" {
		if err := writeConfined(root, opts.Edit, []byte(opts.Content)); err != nil {
			fmt.Fprintf(env.Stderr, "fake-adapter: edit: %v\n", err)
			return 1
		}
	}

	// 3. Descendant.
	var child *descendant
	if opts.SpawnGrandchild {
		child, err = spawnDescendant(env, opts)
		if err != nil {
			fmt.Fprintf(env.Stderr, "fake-adapter: spawn: %v\n", err)
			return 1
		}
	}
	if opts.descendant {
		if rf := env.inheritedFile(envReadyFD, "ready"); rf != nil {
			io.WriteString(rf, "ready\n")
			rf.Close()
		}
	}

	// 4. Ready publication.
	if opts.ReadyFile != "" {
		info := ReadyInfo{PID: env.PID}
		if child != nil {
			info.DescendantPID = child.cmd.Process.Pid
		}
		if err := publishReady(root, opts.ReadyFile, info); err != nil {
			fmt.Fprintf(env.Stderr, "fake-adapter: ready file: %v\n", err)
			if child != nil {
				child.kill()
			}
			return 1
		}
	}

	// 5. Duration wait.
	timer := env.After(opts.Duration)
	for {
		select {
		case sig := <-sigCh:
			name := signalName(sig)
			if opts.SignalFile != "" {
				if err := appendSignal(root, opts.SignalFile, SignalRecord{PID: env.PID, Signal: name}); err != nil {
					fmt.Fprintf(env.Stderr, "fake-adapter: signal file: %v\n", err)
				}
			}
			fmt.Fprintf(env.Stderr, "fake-adapter: received %s\n", name)
			if opts.TermMode == TermExit {
				// Signal-triggered exit: never signal, wait for or kill the
				// descendant; the supervisor owns it from here.
				return 0
			}
		case <-timer:
			if child != nil {
				child.cleanup(env.After)
			}
			return opts.ExitCode
		case <-ctx.Done():
			fmt.Fprintf(env.Stderr, "fake-adapter: cancelled\n")
			return 1
		}
	}
}

func (e Env) stderrOrDiscard() io.Writer {
	if e.Stderr == nil {
		return io.Discard
	}
	return e.Stderr
}

func (e Env) inheritedFile(envName, label string) *os.File {
	v := e.Getenv(envName)
	if v == "" {
		return nil
	}
	fd, err := strconv.Atoi(v)
	if err != nil || fd < 3 {
		return nil
	}
	return e.NewFile(uintptr(fd), label)
}

func nonNil(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}

func marshalNoEscape(v any) ([]byte, error) {
	var sb strings.Builder
	enc := json.NewEncoder(&sb)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return []byte(strings.TrimSuffix(sb.String(), "\n")), nil
}

func ensureParent(root *os.Root, name string) error {
	dir := path.Dir(filepath.ToSlash(name))
	if dir == "." {
		return nil
	}
	return root.MkdirAll(dir, 0o755)
}

func writeConfined(root *os.Root, name string, data []byte) error {
	if err := ensureParent(root, name); err != nil {
		return err
	}
	return root.WriteFile(name, data, 0o644)
}

func publishReady(root *os.Root, name string, info ReadyInfo) error {
	if err := ensureParent(root, name); err != nil {
		return err
	}
	b, _ := json.Marshal(info)
	tmp := fmt.Sprintf("%s.tmp-%d", name, info.PID)
	f, err := root.OpenFile(tmp, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	_, werr := f.Write(append(b, '\n'))
	serr := f.Sync()
	cerr := f.Close()
	if err := errors.Join(werr, serr, cerr); err != nil {
		root.Remove(tmp)
		return err
	}
	if err := root.Rename(tmp, name); err != nil {
		root.Remove(tmp)
		return err
	}
	return nil
}

func appendSignal(root *os.Root, name string, rec SignalRecord) error {
	if err := ensureParent(root, name); err != nil {
		return err
	}
	f, err := root.OpenFile(name, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	b, _ := json.Marshal(rec)
	_, werr := f.Write(append(b, '\n'))
	serr := f.Sync()
	return errors.Join(werr, serr, f.Close())
}

type descendant struct {
	cmd *exec.Cmd
}

func filteredEnv(environ []string) []string {
	var out []string
	for _, kv := range environ {
		if strings.HasPrefix(kv, EnvLifetimeFD+"=") || strings.HasPrefix(kv, envReadyFD+"=") {
			continue
		}
		out = append(out, kv)
	}
	return out
}

func spawnDescendant(env Env, opts Options) (*descendant, error) {
	args := []string{"--internal-descendant", "--term-mode=" + opts.GrandchildTermMode, "--duration=" + opts.Duration.String()}
	if opts.SignalFile != "" {
		args = append(args, "--signal-file="+opts.SignalFile+SignalFileSuffix)
	}
	r, w, err := os.Pipe()
	if err != nil {
		return nil, err
	}
	cmd := exec.Command(env.Executable, args...)
	cmd.Dir = env.Dir
	cmd.Env = append(filteredEnv(env.Environ()), envReadyFD+"=3")
	cmd.ExtraFiles = []*os.File{w}
	lifetime := env.inheritedFile(EnvLifetimeFD, "lifetime")
	if lifetime != nil {
		cmd.ExtraFiles = append(cmd.ExtraFiles, lifetime)
		cmd.Env = append(cmd.Env, EnvLifetimeFD+"=4")
	}
	// The descendant inherits the process group; never Setpgid/Setsid here.
	startErr := cmd.Start()
	w.Close()
	if lifetime != nil {
		lifetime.Close()
	}
	if startErr != nil {
		r.Close()
		return nil, startErr
	}
	d := &descendant{cmd: cmd}
	lineCh := make(chan string, 1)
	go func() {
		line, _ := bufio.NewReader(r).ReadString('\n')
		lineCh <- line
	}()
	var line string
	select {
	case line = <-lineCh:
	case <-env.After(readinessTimeout):
		line = ""
	}
	r.Close()
	if line != "ready\n" {
		d.kill()
		return nil, errors.New("descendant did not become ready")
	}
	return d, nil
}

func (d *descendant) kill() {
	d.cmd.Process.Kill()
	d.cmd.Wait()
}

// cleanup runs only on duration completion: forward TERM, wait up to
// DescendantGrace, then KILL, and always wait.
func (d *descendant) cleanup(after func(time.Duration) <-chan time.Time) {
	done := make(chan struct{})
	go func() {
		d.cmd.Wait()
		close(done)
	}()
	sendTerm(d.cmd.Process)
	select {
	case <-done:
		return
	case <-after(DescendantGrace):
	}
	d.cmd.Process.Kill()
	<-done
}
