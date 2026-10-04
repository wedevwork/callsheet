package client

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"

	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/protocol/packp"

	"github.com/wedevwork/callsheet/internal/contract"
)

// TestNodeWorkspaceClient (iteration 10b): the node transfer session's
// assignment and publication headers, its refusal mapping, the task
// push's report handling, and the publication endpoints' requests and
// replies.
func TestNodeWorkspaceClient(t *testing.T) {
	ca := newTestCA(t)
	const task = "t_0123456789abcdef0123456789abcdef"
	pub := strings.Repeat("a", 32)
	a := contract.NodeAssignment{NodeID: "n_" + strings.Repeat("1", 32), Execution: contract.ExecutionToken{Epoch: strings.Repeat("e", 32), Attachment: 2},
		StartDigest: strings.Repeat("d", 64), Instance: strings.Repeat("b", 32)}
	var mu sync.Mutex
	var hdr http.Header
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {})
	s := startServer(t, ca.leaf(t, false), http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		hdr = r.Header.Clone()
		h := handler
		mu.Unlock()
		h(w, r)
	}))
	set := func(h http.HandlerFunc) {
		mu.Lock()
		handler = h
		mu.Unlock()
	}
	c, err := New(s.url, Trust{CAPEM: ca.pem})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	ctx := context.Background()
	t.Run("sessions", func(t *testing.T) {
		if _, err := c.NodeTaskGit("t_x", a, ""); err == nil {
			t.Fatal("an invalid task ID opened a session")
		}
		if _, err := c.NodeTaskGit(task, a, "x"); err == nil {
			t.Fatal("an invalid publication ID opened a session")
		}
		if _, err := c.NodeTaskPusher(task, a, ""); err == nil {
			t.Fatal("a push without a publication opened a session")
		}
		if _, err := c.TaskPublications("t_x", a); err == nil {
			t.Fatal("an invalid task ID opened the publication endpoints")
		}
	})
	t.Run("refusals", func(t *testing.T) {
		g, _ := c.NodeTaskGit(task, a, "")
		defer g.Close()
		for _, x := range []struct {
			status int
			body   string
			code   contract.Code
			reason string
		}{
			{400, "invalid_argument", contract.CodeInvalidArgument, ""},
			{404, "not_found", contract.CodeNotFound, ""},
			{409, contract.ReasonTaskRefExists, contract.CodeConflict, contract.ReasonTaskRefExists},
			{409, contract.ReasonWorkspaceBaseUnavailable, contract.CodeConflict, contract.ReasonWorkspaceBaseUnavailable},
			{409, "something else", contract.CodeConflict, contract.ReasonTaskAssignmentMismatch},
			{503, "unavailable", contract.CodeUnavailable, ""},
			{500, "internal", contract.CodeInternal, ""},
		} {
			set(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(x.status); io.WriteString(w, x.body) })
			_, err := g.UploadRefs(ctx)
			var ce *contract.Error
			if !errorsAs(err, &ce) || ce.Code != x.code {
				t.Fatalf("%d %s: %v", x.status, x.body, err)
			}
			if r, _ := ce.Details["reason"].(string); r != x.reason {
				t.Fatalf("%d %s: reason %q", x.status, x.body, r)
			}
		}
		// The assignment headers travel on every request.
		mu.Lock()
		got := hdr.Clone()
		mu.Unlock()
		for k, v := range a.Headers() {
			if got.Get(k) != v {
				t.Fatalf("header %s = %q, want %q", k, got.Get(k), v)
			}
		}
	})
	t.Run("push", func(t *testing.T) {
		p, err := c.NodeTaskPusher(task, a, pub)
		if err != nil {
			t.Fatal(err)
		}
		defer p.Close()
		commit := plumbing.NewHash(strings.Repeat("c", 40))
		ref := contract.TaskRefPrefix + task
		if err := p.PushTask(ctx, "refs/heads/main", commit, bytes.NewReader(nil)); contract.CodeOf(err) != contract.CodeInvalidArgument {
			t.Fatalf("foreign ref: %v", err)
		}
		if err := p.PushTask(ctx, ref, plumbing.ZeroHash, bytes.NewReader(nil)); contract.CodeOf(err) != contract.CodeInvalidArgument {
			t.Fatalf("zero commit: %v", err)
		}
		report := func(unpack, refName, status string) http.HandlerFunc {
			return func(w http.ResponseWriter, r *http.Request) {
				io.Copy(io.Discard, r.Body)
				rs := packp.NewReportStatus()
				rs.UnpackStatus = unpack
				rs.CommandStatuses = []*packp.CommandStatus{{ReferenceName: plumbing.ReferenceName(refName), Status: status}}
				w.Header().Set("Content-Type", "application/x-git-receive-pack-result")
				rs.Encode(w)
			}
		}
		for name, x := range map[string]struct {
			h      http.HandlerFunc
			code   contract.Code
			reason string
		}{
			"ok":        {report("ok", ref, "ok"), "", ""},
			"storage":   {report("ok", ref, "storage failure"), contract.CodeUnavailable, ""},
			"cancelled": {report("ok", ref, "cancelled before publication"), contract.CodeUnavailable, ""},
			"overflow":  {report("ok", ref, "metadata overflow"), contract.CodeInvalidArgument, contract.PubErrMetadataOverflow},
			"refused":   {report("unpack failed", ref, "closure invalid"), contract.CodeInvalidArgument, contract.PubErrPublicationTransportFail},
			"other-ref": {report("ok", "refs/heads/x", "ok"), contract.CodeUnavailable, ""},
			"malformed": {func(w http.ResponseWriter, r *http.Request) {
				io.Copy(io.Discard, r.Body)
				io.WriteString(w, "garbage")
			}, contract.CodeUnavailable, ""},
			"conflict": {func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(409)
				io.WriteString(w, contract.ReasonTaskRefExists)
			}, contract.CodeConflict, contract.ReasonTaskRefExists},
		} {
			set(x.h)
			err := p.PushTask(ctx, ref, commit, bytes.NewReader([]byte("PACK")))
			if x.code == "" {
				if err != nil {
					t.Fatalf("%s: %v", name, err)
				}
				continue
			}
			var ce *contract.Error
			if !errorsAs(err, &ce) || ce.Code != x.code {
				t.Fatalf("%s: %v", name, err)
			}
			if r, _ := ce.Details["reason"].(string); x.reason != "" && r != x.reason {
				t.Fatalf("%s: reason %q", name, r)
			}
		}
		mu.Lock()
		got := hdr.Get(contract.PublicationIDHeader)
		mu.Unlock()
		if got != pub {
			t.Fatalf("publication header %q", got)
		}
		// The receive advertisement is read with the instance.
		set(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(409)
			io.WriteString(w, contract.ReasonTaskPublicationClosed)
		})
		if _, err := p.ReceiveRefs(ctx); contract.CodeOf(err) != contract.CodeConflict {
			t.Fatalf("receive refs: %v", err)
		}
	})
	t.Run("publications", func(t *testing.T) {
		p, err := c.TaskPublications(task, a)
		if err != nil {
			t.Fatal(err)
		}
		var method, path, body string
		reply := func(status int, b string) {
			set(func(w http.ResponseWriter, r *http.Request) {
				rb, _ := io.ReadAll(r.Body)
				mu.Lock()
				method, path, body = r.Method, r.URL.Path, string(rb)
				mu.Unlock()
				w.Header().Set(contract.ProtocolHeader, "6")
				w.WriteHeader(status)
				io.WriteString(w, b)
			})
		}
		zero := 0
		cand := contract.TaskResultBody{TaskID: task, Execution: a.Execution, Outcome: contract.OutcomeNatural, ExitCode: &zero}.Sealed()
		reply(200, `{"publication_id":"`+pub+`","state":"succeeded","expires_at":"2026-10-03T10:05:00Z"}`)
		r, err := p.Begin(ctx, contract.PublicationBeginRequest{Candidate: cand, Tree: strings.Repeat("f", 40)})
		if err != nil || r.PublicationID != pub || method != http.MethodPost || path != contract.PublicationPath(task, "", false) || !strings.Contains(body, `"tree":"`) {
			t.Fatalf("begin %+v %v (%s %s %s)", r, err, method, path, body)
		}
		reply(409, `{"error":{"code":"conflict","message":"closed","details":{"reason":"task_publication_closed"}}}`)
		if _, err := p.Begin(ctx, contract.PublicationBeginRequest{Candidate: cand, Tree: strings.Repeat("f", 40)}); contract.CodeOf(err) != contract.CodeConflict {
			t.Fatalf("begin refusal: %v", err)
		}
		reply(200, `{"publication_id":"x"}`)
		if _, err := p.Begin(ctx, contract.PublicationBeginRequest{Candidate: cand, Tree: strings.Repeat("f", 40)}); err == nil {
			t.Fatal("a malformed Begin reply was accepted")
		}
		authorized := `{"phase":"authorized","result":null,"task_state":"running","committed":false}`
		reply(200, authorized)
		st, err := p.Finish(ctx, pub, contract.PubErrStorageFailed)
		if err != nil || st.Phase != contract.PubPhaseAuthorized || path != contract.PublicationPath(task, pub, true) || body != `{"error":"workspace_storage_failed"}` {
			t.Fatalf("finish %+v %v (%s %s)", st, err, path, body)
		}
		reply(500, `{"error":{"code":"internal","message":"x"}}`)
		if _, err := p.Finish(ctx, pub, contract.PubErrStorageFailed); err == nil {
			t.Fatal("a failed Finish succeeded")
		}
		reply(200, authorized)
		if st, err := p.Observe(ctx, pub); err != nil || st.Phase != contract.PubPhaseAuthorized || method != http.MethodGet || path != contract.PublicationPath(task, pub, false) {
			t.Fatalf("observe %+v %v", st, err)
		}
		reply(200, `{"publication_id":null,"phase":"none","result":null,"task_state":"running","committed":false}`)
		if st, err := p.Observe(ctx, ""); err != nil || st.Phase != contract.PubPhaseNone || !st.WithID {
			t.Fatalf("lookup %+v %v", st, err)
		}
		reply(404, `{"error":{"code":"not_found","message":"x"}}`)
		if _, err := p.Observe(ctx, pub); contract.CodeOf(err) != contract.CodeNotFound {
			t.Fatalf("observe missing: %v", err)
		}
		mu.Lock()
		got := hdr.Clone()
		mu.Unlock()
		if got.Get(contract.NodeIDHeader) != a.NodeID || got.Get(contract.StartDigestHeader) != a.StartDigest {
			t.Fatalf("publication headers %v", got)
		}
	})
}

// errorsAs is errors.As for a *contract.Error.
func errorsAs(err error, target **contract.Error) bool {
	for e := err; e != nil; {
		if ce, ok := e.(*contract.Error); ok {
			*target = ce
			return true
		}
		u, ok := e.(interface{ Unwrap() error })
		if !ok {
			return false
		}
		e = u.Unwrap()
	}
	return false
}
