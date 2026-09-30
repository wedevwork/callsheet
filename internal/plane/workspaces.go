package plane

import (
	"errors"
	"io/fs"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"

	"github.com/wedevwork/callsheet/internal/contract"
	"github.com/wedevwork/callsheet/internal/workspace"
)

// Workspaces (iteration 09a): the optional workspaces/ directory holds the
// workspace hub's repositories, validated and recovered by the workspace
// manager at startup and inspected read-only by plane status.
const workspacesName = "workspaces"

// errRootBudget is the fixed invalid_argument of an overlong state root.
func errRootBudget() error {
	return errf(contract.CodeInvalidArgument, "plane state root exceeds %d bytes; choose a shorter --state-dir", workspace.MaxRootBytes)
}

// canonicalRoot resolves the symlinks of root's longest existing ancestor
// and appends the not-yet-existing suffix.
func canonicalRoot(root string) (string, error) {
	root = filepath.Clean(root)
	var suffix []string
	for cur := root; ; {
		if _, err := os.Lstat(cur); err == nil {
			r, err := filepath.EvalSymlinks(cur)
			if err != nil {
				return "", err
			}
			return filepath.Join(append([]string{r}, suffix...)...), nil
		} else if !errors.Is(err, fs.ErrNotExist) {
			return "", err
		}
		parent := filepath.Dir(cur)
		if parent == cur {
			return root, nil
		}
		suffix = append([]string{filepath.Base(cur)}, suffix...)
		cur = parent
	}
}

// checkRoot enforces the 256-byte bound on the cleaned root and on its
// canonical form before any state is created, locked, recovered or
// served, and returns the canonical root (the workspace storage root).
func checkRoot(root string) (string, error) {
	if len(filepath.Clean(root)) > workspace.MaxRootBytes {
		return "", errRootBudget()
	}
	canon, err := canonicalRoot(root)
	if err != nil {
		return "", wrapf(contract.CodeInternal, err, "cannot resolve the plane state root %s: %v", root, err)
	}
	if len(canon) > workspace.MaxRootBytes {
		return "", errRootBudget()
	}
	return canon, nil
}

// scanWorkspaces checks only that an existing workspaces/ is a private
// real directory; its entries are the workspace manager's to validate.
func (l layout) scanWorkspaces() (bool, error) {
	p := l.path(workspacesName)
	fi, err := os.Lstat(p)
	if errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, wrapf(contract.CodeInternal, err, "cannot inspect %s: %v", p, err)
	}
	return true, checkDir(p, fi)
}

// wsQuery strictly parses a workspace GET query: only the allowed keys,
// each at most once with a nonempty, validly percent-decoded value.
// Decoded values are validated by their own grammars (never repaired).
func wsQuery(r *http.Request, allowed ...string) (map[string]string, error) {
	q, err := taskQuery(r, allowed...)
	if err != nil {
		return nil, err
	}
	for k, v := range q {
		d, err := url.QueryUnescape(v)
		if err != nil {
			return nil, invalid("query parameter " + k + " is not validly percent-encoded")
		}
		q[k] = d
	}
	return q, nil
}

// handleWorkspaces serves the workspace control API:
//
//	GET    /api/v1/workspaces?after=&limit=         list
//	POST   /api/v1/workspaces {name}                create (201)
//	GET    /api/v1/workspaces/{name}                show
//	DELETE /api/v1/workspaces/{name} {instance}     remove
//	POST   /api/v1/workspaces/{name}/prune          prune
//	POST   /api/v1/workspaces/{name}/refs/set       ref set
//	GET    /api/v1/workspaces/{name}/status?...     status
//	GET    /api/v1/workspaces/{name}/diff?...       diff
//
// The protocol header is checked first, then the path (the slug grammar
// before any lookup; encoded aliases refused), method, query, body type
// and 64 KiB body bound, before anything is interpreted.
func (s *nodeService) handleWorkspaces(w http.ResponseWriter, r *http.Request) {
	if !s.checkVersion(w, r) {
		return
	}
	if r.URL.RawPath != "" {
		writeError(w, invalid("encoded workspace paths are not accepted"))
		return
	}
	rest := strings.TrimPrefix(r.URL.Path, contract.PathWorkspaces)
	if rest == "" {
		s.workspaceCollection(w, r)
		return
	}
	parts := strings.Split(strings.TrimPrefix(rest, "/"), "/")
	name := parts[0]
	if !strings.HasPrefix(rest, "/") || !contract.ValidWorkspaceName(name) {
		writeError(w, contract.InvalidWorkspaceName())
		return
	}
	op := strings.Join(parts[1:], "/")
	method := map[string]string{"": "", "prune": http.MethodPost, "refs/set": http.MethodPost, "status": http.MethodGet, "diff": http.MethodGet}
	want, known := method[op]
	switch {
	case !known:
		writeError(w, contract.New(contract.CodeNotFound, "no such endpoint"))
		return
	case op == "" && r.Method != http.MethodGet && r.Method != http.MethodDelete:
		methodNotAllowed(w, "GET, DELETE")
		return
	case op != "" && r.Method != want:
		methodNotAllowed(w, want)
		return
	}
	if r.Method == http.MethodGet && hasBody(r) {
		writeError(w, invalid("GET requests must not carry a body"))
		return
	}
	m := s.ws
	switch {
	case op == "" && r.Method == http.MethodGet:
		if _, err := taskQuery(r); err != nil {
			writeCodeError(w, err)
			return
		}
		v, err := m.Show(r.Context(), name)
		reply(w, http.StatusOK, v, err)
	case op == "":
		body, ok := s.wsBody(w, r)
		if !ok {
			return
		}
		req, err := contract.ParseWorkspaceRemoveRequest(body)
		if err != nil {
			writeCodeError(w, err)
			return
		}
		v, err := m.Remove(r.Context(), name, req.Instance)
		reply(w, http.StatusOK, v, err)
	case op == "prune":
		body, ok := s.wsBody(w, r)
		if !ok {
			return
		}
		req, before, err := contract.ParseWorkspacePruneRequest(body)
		if err != nil {
			writeCodeError(w, err)
			return
		}
		v, err := m.Prune(r.Context(), name, req.Instance, before)
		reply(w, http.StatusOK, v, err)
	case op == "refs/set":
		body, ok := s.wsBody(w, r)
		if !ok {
			return
		}
		in, err := contract.ParseWorkspaceRefSetRequest(body)
		if err != nil {
			writeCodeError(w, err)
			return
		}
		v, err := m.SetRef(r.Context(), name, in)
		reply(w, http.StatusOK, v, err)
	case op == "status":
		s.workspaceStatus(w, r, name)
	default:
		s.workspaceDiff(w, r, name)
	}
}

// wsBody checks the query (none), JSON content type and the 64 KiB bound.
func (s *nodeService) wsBody(w http.ResponseWriter, r *http.Request) ([]byte, bool) {
	if _, err := taskQuery(r); err != nil {
		writeCodeError(w, err)
		return nil, false
	}
	return readJSON(w, r, contract.MaxWorkspaceBody, "a workspace request")
}

// reply writes v (bounded) or err.
func reply(w http.ResponseWriter, status int, v any, err error) {
	if err != nil {
		writeCodeError(w, err)
		return
	}
	writeBounded(w, status, v, contract.MaxWorkspaceResponse)
}

func (s *nodeService) workspaceCollection(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		if hasBody(r) {
			writeError(w, invalid("GET requests must not carry a body"))
			return
		}
		q, err := wsQuery(r, "after", "limit")
		if err != nil {
			writeCodeError(w, err)
			return
		}
		limit, err := queryInt(q, "limit", contract.DefaultWorkspaceLimit, 1, contract.MaxWorkspaceLimit)
		if err != nil {
			writeCodeError(w, err)
			return
		}
		after, ok := q["after"]
		if ok && len(after) > 63 {
			writeError(w, invalid("after must be a workspace name of at most 63 bytes"))
			return
		}
		v, err := s.ws.List(after, limit)
		reply(w, http.StatusOK, v, err)
	case http.MethodPost:
		body, ok := s.wsBody(w, r)
		if !ok {
			return
		}
		req, err := contract.ParseWorkspaceCreateRequest(body)
		if err != nil {
			writeCodeError(w, err)
			return
		}
		v, err := s.ws.Create(r.Context(), req.Name)
		reply(w, http.StatusCreated, v, err)
	default:
		methodNotAllowed(w, "GET, POST")
	}
}

// continuation reads the three continuation keys: all present or none.
func continuation(q map[string]string) (instance, generation string, ok bool, err error) {
	_, a := q["after"]
	instance, i := q["instance"]
	generation, g := q["generation"]
	switch {
	case !a && !i && !g:
		return "", "", false, nil
	case !(a && i && g):
		return "", "", false, invalid("a continuation needs after, instance and generation together (the previous page's next_after, instance and generation); the first page omits all three")
	case !contract.ValidWorkspaceToken(instance) || !contract.ValidWorkspaceToken(generation):
		return "", "", false, invalid("instance and generation must be 32-hex tokens from the previous page")
	}
	return instance, generation, true, nil
}

func (s *nodeService) workspaceStatus(w http.ResponseWriter, r *http.Request, name string) {
	q, err := wsQuery(r, "after", "generation", "instance", "limit")
	if err != nil {
		writeCodeError(w, err)
		return
	}
	limit, err := queryInt(q, "limit", contract.DefaultWorkspaceLimit, 1, contract.MaxWorkspaceLimit)
	if err != nil {
		writeCodeError(w, err)
		return
	}
	instance, generation, cont, err := continuation(q)
	if err != nil {
		writeCodeError(w, err)
		return
	}
	sq := workspace.StatusQuery{Limit: limit}
	if cont {
		if !contract.ValidStatusCursor(q["after"]) {
			writeError(w, invalid("after must be a complete portable branch ref (refs/heads/...) or task ref (refs/callsheet/tasks/...) from the previous page"))
			return
		}
		sq.After, sq.Instance, sq.Generation = q["after"], instance, generation
	}
	v, err := s.ws.Status(r.Context(), name, sq)
	reply(w, http.StatusOK, v, err)
}

func (s *nodeService) workspaceDiff(w http.ResponseWriter, r *http.Request, name string) {
	q, err := wsQuery(r, "base", "target", "after", "generation", "instance", "limit")
	if err != nil {
		writeCodeError(w, err)
		return
	}
	limit, err := queryInt(q, "limit", contract.DefaultWorkspaceLimit, 1, contract.MaxWorkspaceLimit)
	if err != nil {
		writeCodeError(w, err)
		return
	}
	base, okB := q["base"]
	target, okT := q["target"]
	if !okB || !okT {
		writeError(w, invalid("diff needs base and target"))
		return
	}
	dq := workspace.DiffQuery{Limit: limit}
	if dq.Base, err = contract.ParseSelector(base, "base", true); err != nil {
		writeCodeError(w, err)
		return
	}
	if dq.Target, err = contract.ParseSelector(target, "target", false); err != nil {
		writeCodeError(w, err)
		return
	}
	instance, generation, cont, err := continuation(q)
	if err != nil {
		writeCodeError(w, err)
		return
	}
	if cont {
		if dq.After, err = contract.ParsePathCursor(q["after"]); err != nil {
			writeCodeError(w, err)
			return
		}
		if (dq.Base.Kind != contract.SelectorKindHash && dq.Base.Kind != contract.SelectorKindEmpty) || dq.Target.Kind != contract.SelectorKindHash {
			writeError(w, invalid("a continuation must name base and target by the previous page's full commit hashes (or empty for the base)"))
			return
		}
		dq.Instance, dq.Generation = instance, generation
	}
	v, err := s.ws.Diff(r.Context(), name, dq)
	reply(w, http.StatusOK, v, err)
}
