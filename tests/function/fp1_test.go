package function

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/wedevwork/callsheet/internal/contract"
	"github.com/wedevwork/callsheet/internal/logging"
	"github.com/wedevwork/callsheet/internal/testkit"
)

const module = "github.com/wedevwork/callsheet"

func goList(t *testing.T, root string, args ...string) string {
	t.Helper()
	cmd := exec.Command(testkit.GoTool(), append([]string{"list"}, args...)...)
	cmd.Dir = root
	var out, errOut bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errOut
	if err := cmd.Run(); err != nil {
		t.Fatalf("go list %v: %v\n%s", args, err, errOut.String())
	}
	return out.String()
}

// TestFP1Foundation: module/version pins, production dependency boundaries,
// and the contract/logging public API outputs.
func TestFP1Foundation(t *testing.T) {
	root := testkit.MustRepoRoot(t)

	t.Run("go.mod", func(t *testing.T) {
		b, err := os.ReadFile(filepath.Join(root, "go.mod"))
		if err != nil {
			t.Fatal(err)
		}
		mod := string(b)
		for _, want := range []string{
			"module " + module + "\n",
			"\ngo 1.26.0\n",
			"github.com/go-git/go-git/v5 v5.16.3\n",
			"github.com/coder/websocket v1.8.14\n",
		} {
			if !strings.Contains(mod, want) {
				t.Fatalf("go.mod lacks %q", strings.TrimSpace(want))
			}
		}
		if strings.Contains(mod, "\ntoolchain ") {
			t.Fatal("unexpected toolchain directive")
		}
	})

	t.Run("dependency boundaries", func(t *testing.T) {
		forbidden := []string{module + "/internal/testkit", module + "/internal/spikes", module + "/internal/devcheck", "github.com/go-git/"}
		for _, pkg := range []string{"./cmd/callsheet", "./internal/cli", "./internal/contract", "./internal/logging"} {
			for _, dep := range strings.Fields(goList(t, root, "-deps", "-f", "{{.ImportPath}}", pkg)) {
				for _, f := range forbidden {
					if strings.HasPrefix(dep, f) {
						t.Errorf("production package %s depends on %s", pkg, dep)
					}
				}
			}
		}
		// Since iteration 03 the pinned WebSocket library is the production
		// node transport; only the node stream's owners import it directly.
		wsOwners := map[string]bool{module + "/internal/client": true, module + "/internal/plane": true, module + "/internal/sidecar": true}
		for _, line := range strings.Split(strings.TrimSpace(goList(t, root, "-f", `{{.ImportPath}} {{join .Imports " "}}`, "./cmd/...", "./internal/cli/...", "./internal/client/...", "./internal/plane/...", "./internal/sidecar/...", "./internal/contract/...", "./internal/logging/...")), "\n") {
			f := strings.Fields(line)
			for _, imp := range f[1:] {
				if strings.HasPrefix(imp, "github.com/coder/websocket") && !wsOwners[f[0]] {
					t.Errorf("production package %s imports %s directly", f[0], imp)
				}
			}
		}
		// contract imports only the standard library.
		for _, imp := range strings.Fields(goList(t, root, "-f", `{{join .Imports " "}}`, "./internal/contract")) {
			if strings.Contains(strings.SplitN(imp, "/", 2)[0], ".") {
				t.Errorf("internal/contract imports non-stdlib %s", imp)
			}
		}
		// Across the module: only test tooling may import testkit or spikes,
		// and spikes are never imported by cmd/callsheet's graph.
		allowed := func(p string) bool {
			for _, pre := range []string{module + "/internal/testkit", module + "/internal/spikes", module + "/internal/devcheck", module + "/cmd/fake-adapter", module + "/cmd/devcheck", module + "/tests/"} {
				if strings.HasPrefix(p, pre) {
					return true
				}
			}
			return false
		}
		for _, line := range strings.Split(strings.TrimSpace(goList(t, root, "-f", `{{.ImportPath}} {{join .Imports " "}}`, "./...")), "\n") {
			f := strings.Fields(line)
			if len(f) == 0 || allowed(f[0]) {
				continue
			}
			for _, imp := range f[1:] {
				if strings.HasPrefix(imp, module+"/internal/testkit") || strings.HasPrefix(imp, module+"/internal/spikes") {
					t.Errorf("production package %s imports %s", f[0], imp)
				}
			}
		}
	})

	t.Run("contract API", func(t *testing.T) {
		want := map[contract.Code][2]int{
			contract.CodeInvalidArgument:  {2, 400},
			contract.CodeNotFound:         {3, 404},
			contract.CodeConflict:         {4, 409},
			contract.CodeUnavailable:      {5, 503},
			contract.CodeTrustFailed:      {6, 0},
			contract.CodeProtocolMismatch: {7, 409},
			contract.CodeNotImplemented:   {8, 501},
			contract.CodeInternal:         {1, 500},
		}
		for code, w := range want {
			if contract.ExitCode(contract.New(code, "m")) != w[0] || contract.HTTPStatus(code) != w[1] {
				t.Errorf("%s mapping", code)
			}
		}
		if contract.ExitCode(nil) != 0 || contract.ExitCode(errors.New("x")) != 1 || contract.HTTPStatus("nope") != 500 {
			t.Fatal("nil/unknown mappings")
		}
		if contract.ExitCode(fmt.Errorf("wrap: %w", contract.New(contract.CodeConflict, "c"))) != 4 {
			t.Fatal("wrapped mapping")
		}
		if contract.ProtocolVersion != 1 || contract.ExitInterrupted != 130 {
			t.Fatal("constants")
		}
		e := &contract.Error{Code: contract.CodeNotFound, Message: "角色 missing", Details: map[string]any{"role": "coder"}, Cause: errors.New("SECRET")}
		b, err := json.Marshal(e)
		if err != nil {
			t.Fatal(err)
		}
		var decoded struct {
			Error struct {
				Code    string         `json:"code"`
				Message string         `json:"message"`
				Details map[string]any `json:"details"`
			} `json:"error"`
		}
		if err := json.Unmarshal(b, &decoded); err != nil {
			t.Fatal(err)
		}
		if decoded.Error.Code != "not_found" || decoded.Error.Message != "角色 missing" || decoded.Error.Details["role"] != "coder" || strings.Contains(string(b), "SECRET") {
			t.Fatalf("error JSON = %s", b)
		}
		if !errors.Is(e, e.Cause) || e.Error() != "not_found: 角色 missing" {
			t.Fatal("Error/Unwrap")
		}
	})

	t.Run("logging API", func(t *testing.T) {
		var buf bytes.Buffer
		l := logging.Component(logging.New(&buf, slog.LevelInfo), "sidecar")
		l.Debug("hidden")
		l.Info("connected", logging.KeyNode, "n1", logging.KeyTask, "t1", "err", contract.Wrap(contract.CodeInternal, "safe", errors.New("PAYLOAD")))
		var rec map[string]any
		if err := json.Unmarshal(bytes.TrimSpace(buf.Bytes()), &rec); err != nil {
			t.Fatalf("log = %q: %v", buf.String(), err)
		}
		ts, err := time.Parse(time.RFC3339Nano, rec["time"].(string))
		if err != nil || ts.Location() != time.UTC {
			t.Fatalf("time = %v", rec["time"])
		}
		if rec["level"] != "INFO" || rec["msg"] != "connected" || rec["component"] != "sidecar" || rec["node_id"] != "n1" || rec["task_id"] != "t1" {
			t.Fatalf("record = %v", rec)
		}
		if strings.Contains(buf.String(), "PAYLOAD") || strings.Contains(buf.String(), "hidden") {
			t.Fatalf("leak/level: %s", buf.String())
		}
	})
}
