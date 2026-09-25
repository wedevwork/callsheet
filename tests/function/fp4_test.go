package function

import (
	"context"
	"crypto/x509"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/wedevwork/callsheet/internal/contract"
	"github.com/wedevwork/callsheet/internal/testkit"
)

// TestFP4TransportHarness: a verified-TLS outbound peer exchanges hello and
// echo; a wrong CA and a mismatched version fail; Close joins both peers and
// permits immediate root cleanup.
func TestFP4TransportHarness(t *testing.T) {
	var mu sync.Mutex
	var spyPaths []string
	spy := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		spyPaths = append(spyPaths, r.URL.Path)
		mu.Unlock()
		io.WriteString(w, "spy")
	})
	h := testkit.NewHarness(t, testkit.WithGitHandler(spy))
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	if !strings.HasPrefix(h.URL, "https://127.0.0.1:") || len(h.CAPEM) == 0 || h.Client == nil {
		t.Fatalf("harness = %+v", h)
	}
	resp, err := h.Client.Get(h.URL + "/test/health")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if string(body) != `{"ok":true}` {
		t.Fatalf("health = %s", body)
	}

	sc, err := h.DialSidecar(ctx, contract.ProtocolVersion)
	if err != nil {
		t.Fatalf("hello: %v", err)
	}
	for i, msg := range []string{`{"n":1}`, `{"text":"héllo 你好"}`, `[1,2,3]`} {
		got, err := sc.Echo(ctx, "req-"+string(rune('a'+i)), json.RawMessage(msg))
		if err != nil || string(got) != msg {
			t.Fatalf("echo %s = %s %v", msg, got, err)
		}
	}

	// The git spike mount receives the full /test/git/ path.
	resp, err = h.Client.Get(h.URL + "/test/git/repo.git/info/refs")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	mu.Lock()
	if len(spyPaths) != 1 || spyPaths[0] != "/test/git/repo.git/info/refs" {
		t.Fatalf("spy saw %v", spyPaths)
	}
	mu.Unlock()

	// An unrelated CA rejects the handshake.
	other, err := testkit.NewFixtureCA()
	if err != nil {
		t.Fatal(err)
	}
	_, err = other.Client().Get(h.URL + "/test/health")
	var ua x509.UnknownAuthorityError
	if !errors.As(err, &ua) {
		t.Fatalf("wrong CA = %v", err)
	}

	// A mismatched version fails before application traffic, reporting both.
	_, err = h.DialSidecar(ctx, 7)
	var ce *contract.Error
	if !errors.As(err, &ce) || ce.Code != contract.CodeProtocolMismatch {
		t.Fatalf("mismatch = %v", err)
	}
	if ce.Details["local_version"] != float64(1) || ce.Details["remote_version"] != float64(7) {
		t.Fatalf("mismatch details = %v", ce.Details)
	}

	// Plain HTTP to the TLS port cannot reach a route.
	if resp, err := http.Get("http://" + h.Addr() + "/test/health"); err == nil {
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode == 200 || strings.Contains(string(b), `"ok":true`) {
			t.Fatalf("plain HTTP succeeded: %d %s", resp.StatusCode, b)
		}
	}

	// Closure joins both peers; the root can be removed immediately.
	if err := os.WriteFile(h.Root+"/state.json", []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := h.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	select {
	case <-h.Done:
	case <-time.After(time.Second):
		t.Fatal("Done not closed after Close returned")
	}
	for err := range h.Errs {
		t.Fatalf("harness error: %v", err)
	}
	if _, err := sc.Echo(ctx, "after", json.RawMessage(`{}`)); err == nil {
		t.Fatal("peer still alive after Close")
	}
	if err := os.RemoveAll(h.Root); err != nil {
		t.Fatalf("root cleanup: %v", err)
	}
	if _, err := os.Stat(h.Root); !os.IsNotExist(err) {
		t.Fatal("root still present")
	}
}
