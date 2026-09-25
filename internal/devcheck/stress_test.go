package devcheck

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// The literal stress plan is the specification oracle (design 01c, Stress
// execution); it is compared against StressSteps, never derived from it.
const (
	wantStressPackages = "go test -race -count=20 -cpu=1,2,4 -timeout=6m ./internal/testkit ./internal/testkit/fakeadapter ./internal/spikes/processgroup ./internal/spikes/gittransport"
	wantStressFunction = "go test -race -count=20 -cpu=1,2,4 -timeout=6m -run=^(TestFP4TransportHarness|TestFP5GitRoundTrip|TestFP6ProcessGroups)$ ./tests/function"
)

func TestStressPlan(t *testing.T) {
	if StressCount != 20 {
		t.Fatalf("StressCount = %d, the declared repeat count is 20", StressCount)
	}
	for _, goos := range []string{"linux", "darwin"} {
		steps, err := StressSteps(goos)
		if err != nil || len(steps) != 2 {
			t.Fatalf("%s: %+v %v", goos, steps, err)
		}
		if steps[0].Name != "stress packages" || steps[1].Name != "stress function" {
			t.Fatalf("%s names = %q %q", goos, steps[0].Name, steps[1].Name)
		}
		if got := strings.Join(steps[0].Argv, " "); got != wantStressPackages {
			t.Fatalf("%s packages step = %s", goos, got)
		}
		if got := strings.Join(steps[1].Argv, " "); got != wantStressFunction {
			t.Fatalf("%s function step = %s", goos, got)
		}
		for _, s := range steps {
			if strings.Join(s.Env, " ") != "CGO_ENABLED=1" {
				t.Fatalf("%s %s env = %v", goos, s.Name, s.Env)
			}
			for _, a := range s.Argv {
				if strings.HasPrefix(a, "-bench") || strings.Contains(a, "'") || strings.Contains(a, " ") {
					t.Fatalf("%s %s: unexpected argv element %q", goos, s.Name, a)
				}
			}
		}
	}
	// Returned plans are independent: mutating one never changes the next,
	// including nested argv and env slices.
	a, _ := StressSteps("linux")
	a[0].Argv[3] = "-count=1"
	a[0].Env[0] = "CGO_ENABLED=0"
	a[1].Argv = append(a[1].Argv[:2], "mutated")
	a[0].Name = "mutated"
	b, _ := StressSteps("linux")
	if strings.Join(b[0].Argv, " ") != wantStressPackages || b[0].Env[0] != "CGO_ENABLED=1" ||
		strings.Join(b[1].Argv, " ") != wantStressFunction || b[0].Name != "stress packages" {
		t.Fatalf("plan state leaked: %+v", b)
	}
	for _, goos := range []string{"windows", "freebsd", "plan9", "", "Linux"} {
		steps, err := StressSteps(goos)
		if err == nil || steps != nil || !strings.Contains(err.Error(), "stress stage is unsupported on") {
			t.Fatalf("StressSteps(%q) = %v %v", goos, steps, err)
		}
	}
}

func TestStressStageDispatch(t *testing.T) {
	for _, goos := range []string{"linux", "darwin"} {
		f := &fakeRunner{}
		code, out, errOut := runDriver(t, goos, f, "stress")
		if code != 0 || !strings.Contains(out, "stage stress ok") {
			t.Fatalf("%s stress = %d %s", goos, code, errOut)
		}
		if got := strings.Join(f.argvs(), "|"); got != wantStressPackages+"|"+wantStressFunction {
			t.Fatalf("%s calls = %s", goos, got)
		}
		for _, c := range f.calls {
			if c.dir != "" || !strings.Contains(strings.Join(c.env, "\n"), "PATH=") {
				t.Fatalf("%s: child must run in the caller's cwd with the parent environment", goos)
			}
			if !strings.HasSuffix(strings.Join(c.env, "\n"), "CGO_ENABLED=1") {
				t.Fatalf("%s: CGO_ENABLED=1 must override the parent environment", goos)
			}
		}
		if !strings.Contains(out, "devcheck: stress packages: "+wantStressPackages) || !strings.Contains(out, "devcheck: stress function: "+wantStressFunction) {
			t.Fatalf("%s commands not logged: %s", goos, out)
		}
		if _, err := os.Stat(scratchFrom(out)); !os.IsNotExist(err) {
			t.Fatalf("%s: successful scratch not deleted", goos)
		}
	}
	// all stays test, coverage, bench, cross: stress is explicit.
	f := &fakeRunner{coverTotal: "81%", cmdList: cmdList, profile: goodProfile}
	if code, _, errOut := runDriver(t, "linux", f, "all"); code != 0 || len(f.calls) != 18 {
		t.Fatalf("all = %d with %d calls %s", code, len(f.calls), errOut)
	}
	for _, c := range f.argvs() {
		if strings.Contains(c, "-count=20") || strings.Contains(c, "-cpu=") {
			t.Fatalf("all ran a stress command: %s", c)
		}
	}
	for _, args := range [][]string{{"stress", "extra"}, {"stress", "-o", "x"}, {"stress", "-count=1"}} {
		f := &fakeRunner{}
		if code, _, errOut := runDriver(t, "linux", f, args...); code != 2 || !strings.Contains(errOut, "invalid arguments for stress") || len(f.calls) != 0 {
			t.Fatalf("%v = %d %s", args, code, errOut)
		}
	}
}

func TestStressFailFastRetainsLogs(t *testing.T) {
	for _, c := range []struct {
		fail, failed string
		calls        int
	}{{"./internal/testkit", "stress packages", 1}, {"./tests/function", "stress function", 2}} {
		f := &fakeRunner{fail: c.fail}
		code, out, errOut := runDriver(t, "linux", f, "stress")
		scratch := scratchFrom(out)
		if code != 1 || len(f.calls) != c.calls || !strings.Contains(errOut, "stage stress FAILED: "+c.failed+" failed: go test -race") ||
			!strings.Contains(errOut, "boom from child") || !strings.Contains(errOut, "logs retained in "+scratch) || strings.Contains(out, "stage stress ok") {
			t.Fatalf("fail %s = %d calls=%d %s", c.fail, code, len(f.calls), errOut)
		}
		log := filepath.Join(scratch, strings.ReplaceAll(c.failed, " ", "-")+".log")
		if b, err := os.ReadFile(log); err != nil || !strings.Contains(string(b), "boom from child") {
			t.Fatalf("failure log %s: %q %v", log, b, err)
		}
		os.RemoveAll(scratch)
	}
}

func TestUnsupportedStressCreatesNoScratch(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("TMPDIR", tmp)
	for _, goos := range []string{"windows", "freebsd", ""} {
		f := &fakeRunner{}
		code, out, errOut := runDriver(t, goos, f, "stress")
		if code != 1 || len(f.calls) != 0 || !strings.Contains(errOut, "stage stress FAILED") || !strings.Contains(errOut, "unsupported on") {
			t.Fatalf("%q stress = %d calls=%d %s", goos, code, len(f.calls), errOut)
		}
		if strings.Contains(errOut, "logs retained") || strings.Contains(out, "devcheck: scratch") {
			t.Fatalf("%q stress claims a scratch dir: out=%q err=%q", goos, out, errOut)
		}
		if entries, _ := os.ReadDir(tmp); len(entries) != 0 {
			t.Fatalf("%q stress created %v", goos, entries)
		}
	}
}

// ctxRecorder records each child's context and returns what next says.
type ctxRecorder struct {
	mu   sync.Mutex
	ctxs []context.Context
	next func(ctx context.Context) error
}

func (r *ctxRecorder) run(ctx context.Context, _, _ []string, _ string, _, _ io.Writer) error {
	r.mu.Lock()
	r.ctxs = append(r.ctxs, ctx)
	r.mu.Unlock()
	if r.next != nil {
		return r.next(ctx)
	}
	return nil
}

func stressDriver(t *testing.T, ctx context.Context, run Runner) *driver {
	return &driver{ctx: ctx, run: run, out: io.Discard, errOut: io.Discard, scratch: t.TempDir(), goos: "linux"}
}

func TestStressWatchdog(t *testing.T) {
	steps, _ := StressSteps("linux")
	// Every child runs under a watchdog deadline stressWatchdog from now,
	// and the watchdog context is canceled once the stage returns.
	r := &ctxRecorder{}
	before := time.Now()
	if err := stressDriver(t, context.Background(), r.run).stress(steps); err != nil {
		t.Fatal(err)
	}
	after := time.Now()
	if len(r.ctxs) != 2 {
		t.Fatalf("calls = %d", len(r.ctxs))
	}
	for i, ctx := range r.ctxs {
		dl, ok := ctx.Deadline()
		if !ok || dl.Before(before.Add(stressWatchdog)) || dl.After(after.Add(stressWatchdog)) {
			t.Fatalf("child %d deadline = %v (ok=%v), want %v from start", i, dl, ok, stressWatchdog)
		}
		if ctx.Err() == nil {
			t.Fatalf("child %d context still live after the stage returned", i)
		}
	}
	if stressWatchdog != 15*time.Minute {
		t.Fatalf("watchdog = %v", stressWatchdog)
	}
	// An earlier caller deadline wins.
	parentDL := time.Now().Add(time.Minute)
	parent, cancel := context.WithDeadline(context.Background(), parentDL)
	defer cancel()
	r = &ctxRecorder{}
	if err := stressDriver(t, parent, r.run).stress(steps); err != nil {
		t.Fatal(err)
	}
	if dl, _ := r.ctxs[0].Deadline(); !dl.Equal(parentDL) {
		t.Fatalf("deadline = %v, want the caller's %v", dl, parentDL)
	}
	if parent.Err() != nil {
		t.Fatal("stress canceled its caller's context")
	}
	// A later caller deadline does not extend the watchdog.
	late, cancelLate := context.WithDeadline(context.Background(), time.Now().Add(time.Hour))
	defer cancelLate()
	r = &ctxRecorder{}
	stressDriver(t, late, r.run).stress(steps)
	if dl, _ := r.ctxs[0].Deadline(); dl.After(time.Now().Add(stressWatchdog)) {
		t.Fatalf("deadline %v exceeds the watchdog", dl)
	}
}

func TestStressCancellationAndExpiry(t *testing.T) {
	steps, _ := StressSteps("linux")
	// A canceled caller starts no child at all.
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	r := &ctxRecorder{}
	err := stressDriver(t, canceled, r.run).stress(steps)
	if !errors.Is(err, context.Canceled) || len(r.ctxs) != 0 || !strings.Contains(err.Error(), "ended before stress packages") {
		t.Fatalf("canceled = %v after %d calls", err, len(r.ctxs))
	}
	// Caller cancellation during a child reaches the child's context; the
	// child's error fails the stage and the next step never starts.
	parent, cancelParent := context.WithCancel(context.Background())
	defer cancelParent()
	r = &ctxRecorder{next: func(ctx context.Context) error {
		cancelParent()
		<-ctx.Done()
		return ctx.Err()
	}}
	err = stressDriver(t, parent, r.run).stress(steps)
	var se *StepError
	if !errors.As(err, &se) || se.Step.Name != "stress packages" || !errors.Is(err, context.Canceled) || len(r.ctxs) != 1 {
		t.Fatalf("propagated cancel = %v after %d calls", err, len(r.ctxs))
	}
	// An expired deadline is a failure even if the child reports success.
	// The deadline expires while the first child runs, under the test's
	// control rather than a wall-clock race.
	expiring := newExpiringCtx()
	r = &ctxRecorder{next: func(ctx context.Context) error {
		expiring.expire()
		<-ctx.Done()
		return nil
	}}
	err = stressDriver(t, expiring, r.run).stress(steps)
	if !errors.Is(err, context.DeadlineExceeded) || len(r.ctxs) != 1 || !strings.Contains(err.Error(), "ended during stress packages") {
		t.Fatalf("expired = %v after %d calls", err, len(r.ctxs))
	}
	// Through dispatch: no success message after an expired watchdog.
	expiring = newExpiringCtx()
	r = &ctxRecorder{next: func(ctx context.Context) error {
		expiring.expire()
		<-ctx.Done()
		return nil
	}}
	var out, errOut bytes.Buffer
	code := runFor(expiring, "darwin", []string{"stress"}, &out, &errOut, r.run)
	if code != 1 || len(r.ctxs) != 1 || strings.Contains(out.String(), "stage stress ok") ||
		!strings.Contains(errOut.String(), "stage stress FAILED: devcheck: stress watchdog (15m0s) ended during stress packages: context deadline exceeded") {
		t.Fatalf("dispatch expired = %d after %d calls: %s", code, len(r.ctxs), errOut.String())
	}
	os.RemoveAll(scratchFrom(out.String()))
}

// expiringCtx is a parent context whose deadline the test expires on demand;
// derived contexts are then done with context.DeadlineExceeded.
type expiringCtx struct {
	context.Context
	once sync.Once
	done chan struct{}
}

func newExpiringCtx() *expiringCtx {
	return &expiringCtx{Context: context.Background(), done: make(chan struct{})}
}

func (c *expiringCtx) expire()               { c.once.Do(func() { close(c.done) }) }
func (c *expiringCtx) Done() <-chan struct{} { return c.done }
func (c *expiringCtx) Err() error {
	select {
	case <-c.done:
		return context.DeadlineExceeded
	default:
		return nil
	}
}

// TestPlatformSeamContract is the FP-2 contract test for devcheck, executed
// by name from tests/function (TestHardeningPlatformSeams). Its linux and
// darwin subtests exercise every OS-dependent plan and the runFor dispatch
// on any host; unsupported values are rejected. Do not rename or skip.
func TestPlatformSeamContract(t *testing.T) {
	for _, goos := range []string{"linux", "darwin"} {
		t.Run(goos, func(t *testing.T) {
			test := TestSteps(goos)
			wantTest := map[string]int{"linux": 2, "darwin": 1}[goos]
			if len(test) != wantTest {
				t.Fatalf("TestSteps(%s) = %d steps", goos, len(test))
			}
			native, nerr := NativeSteps(goos)
			if (goos == "darwin") != (nerr == nil) || (goos == "darwin" && len(native) != 1) {
				t.Fatalf("NativeSteps(%s) = %v %v", goos, native, nerr)
			}
			stress, serr := StressSteps(goos)
			if serr != nil || len(stress) != 2 {
				t.Fatalf("StressSteps(%s) = %v %v", goos, stress, serr)
			}
			for stage, want := range map[string]int{"test": wantTest, "stress": 2, "native": len(native)} {
				f := &fakeRunner{native: stream(qualification()...)}
				code, out, errOut := runDriver(t, goos, f, stage)
				wantCode := 0
				if stage == "native" && goos != "darwin" {
					wantCode = 1
				}
				if code != wantCode || len(f.calls) != want {
					t.Fatalf("runFor(%s, %s) = %d with %d calls: %s", goos, stage, code, len(f.calls), errOut)
				}
				os.RemoveAll(scratchFrom(out))
			}
		})
	}
	for _, goos := range []string{"windows", "freebsd", ""} {
		if _, err := StressSteps(goos); err == nil {
			t.Fatalf("StressSteps(%q) accepted", goos)
		}
		if _, err := NativeSteps(goos); err == nil {
			t.Fatalf("NativeSteps(%q) accepted", goos)
		}
		if len(TestSteps(goos)) != 1 {
			t.Fatalf("TestSteps(%q) must be the plain suite only", goos)
		}
	}
}
