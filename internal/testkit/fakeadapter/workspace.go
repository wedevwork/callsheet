package fakeadapter

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	git "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"
)

// Workspace task mode (iteration 10b, fixture only): with EnvTaskMode
// "workspace" the task's goal, when it starts with WorkspaceGoalPrefix, is
// a JSON WorkspaceScript the child executes in its working directory: file
// edits, go-git mutations of its private checkout (never shell git), an
// optional cwd report and barrier files, then the final marker and the
// script's exit code. Any other goal behaves as success. The fixture never
// reads anything outside its cwd except the script's absolute barrier and
// report paths.
const (
	// WorkspaceMode is the EnvTaskMode value.
	WorkspaceMode = "workspace"
	// WorkspaceGoalPrefix starts a scripted goal.
	WorkspaceGoalPrefix = "ws-script:"
)

// WorkspaceOp is one script step; exactly one action field is set.
type WorkspaceOp struct {
	// Write creates or replaces the regular file Write (relative) with
	// Data, or the decoded DataB64, and Mode (default 0644).
	Write string `json:"write,omitempty"`
	// WriteB64 is Write's path as base64 bytes (any byte but NUL and /
	// in a name, non-UTF-8 included).
	WriteB64 string `json:"write_b64,omitempty"`
	Data     string `json:"data,omitempty"`
	DataB64  string `json:"data_b64,omitempty"`
	Mode     uint32 `json:"mode,omitempty"`
	// Remove deletes a relative path (recursively).
	Remove string `json:"remove,omitempty"`
	// Mkdir creates a relative directory (and parents).
	Mkdir string `json:"mkdir,omitempty"`
	// Symlink creates a relative link to Target.
	Symlink string `json:"symlink,omitempty"`
	Target  string `json:"target,omitempty"`
	// Chmod sets Mode on a relative path.
	Chmod string `json:"chmod,omitempty"`
	// GitCommit stages every worktree change in the child's own .git
	// (go-git) and commits with this message, moving its HEAD; GitBranch
	// also creates and checks out that branch first.
	GitCommit string `json:"git_commit,omitempty"`
	GitBranch string `json:"git_branch,omitempty"`
	// Copy copies the relative regular file Copy to the relative Target.
	Copy string `json:"copy,omitempty"`
	// Many creates Count files "<Many>-<NNNNN>.txt" holding "x".
	Many  string `json:"many,omitempty"`
	Count int    `json:"count,omitempty"`
	// RemoveGit deletes the child's top-level .git.
	RemoveGit bool `json:"remove_git,omitempty"`
	// Touch creates the absolute file Touch (a barrier signal).
	Touch string `json:"touch,omitempty"`
	// WaitFor waits (bounded, 2 minutes) until the absolute file exists.
	WaitFor string `json:"wait_for,omitempty"`
	// SleepMS sleeps.
	SleepMS int `json:"sleep_ms,omitempty"`
}

// WorkspaceScript is a scripted goal's body.
type WorkspaceScript struct {
	// Report, when set, is the absolute path of the WorkspaceReport the
	// child writes before running Ops.
	Report string        `json:"report,omitempty"`
	Ops    []WorkspaceOp `json:"ops,omitempty"`
	// Exit is the child's exit code after the final marker (nonzero: the
	// failure line instead of the marker).
	Exit int `json:"exit,omitempty"`
}

// WorkspaceGoal encodes s as a goal.
func WorkspaceGoal(s WorkspaceScript) string {
	b, _ := json.Marshal(s)
	return WorkspaceGoalPrefix + string(b)
}

// WorkspaceReport is what a scripted child saw before its edits.
type WorkspaceReport struct {
	CWD     string `json:"cwd"`
	CWDMode string `json:"cwd_mode"`
	PWD     string `json:"pwd"`
	PATH    string `json:"path"`
	// PathSet reports whether PATH was present at all.
	PathSet bool `json:"path_set"`
	// HEAD is the raw content of .git/HEAD ("" when absent); HasGit
	// reports a top-level .git.
	HEAD   string `json:"head"`
	HasGit bool   `json:"has_git"`
	// Files maps each relative path below cwd (outside .git) to
	// "<kind> <mode> <sha256>" (kind f, l or d; links hash their target).
	Files map[string]string `json:"files"`
}

// workspaceGoal extracts a scripted goal from the prompt, if any.
func workspaceGoal(prompt []byte) (WorkspaceScript, bool, error) {
	var env struct {
		Task struct {
			Goal string `json:"goal"`
		} `json:"task"`
	}
	if err := json.Unmarshal(prompt, &env); err != nil {
		return WorkspaceScript{}, false, err
	}
	body, ok := strings.CutPrefix(env.Task.Goal, WorkspaceGoalPrefix)
	if !ok {
		return WorkspaceScript{}, false, nil
	}
	dec := json.NewDecoder(strings.NewReader(body))
	dec.DisallowUnknownFields()
	var s WorkspaceScript
	if err := dec.Decode(&s); err != nil {
		return WorkspaceScript{}, false, fmt.Errorf("the workspace script is invalid: %v", err)
	}
	return s, true, nil
}

// runWorkspace executes a scripted goal in dir.
func runWorkspace(env Env, prompt []byte, stdout, stderr io.Writer, getenv func(string) string) int {
	s, ok, err := workspaceGoal(prompt)
	if err != nil {
		fmt.Fprintf(stderr, "fake-adapter: %v\n", err)
		return 2
	}
	if !ok {
		io.WriteString(stdout, FinalMarker)
		return 0
	}
	dir := env.Dir
	if dir == "" {
		if dir, err = os.Getwd(); err != nil {
			fmt.Fprintf(stderr, "fake-adapter: cwd: %v\n", err)
			return 1
		}
	}
	if s.Report != "" {
		if err := writeWorkspaceReport(dir, s.Report, getenv); err != nil {
			fmt.Fprintf(stderr, "fake-adapter: report: %v\n", err)
			return 1
		}
	}
	for i, op := range s.Ops {
		if err := applyOp(dir, op); err != nil {
			fmt.Fprintf(stderr, "fake-adapter: op %d: %v\n", i, err)
			return 1
		}
	}
	if s.Exit != 0 {
		io.WriteString(stderr, FailureLine)
		return s.Exit
	}
	io.WriteString(stdout, FinalMarker)
	return 0
}

// rel resolves a script's relative path inside dir.
func rel(dir, p string) (string, error) {
	if p == "" || filepath.IsAbs(p) || strings.HasPrefix(filepath.Clean(p), "..") {
		return "", fmt.Errorf("path %q is not relative to the cwd", p)
	}
	return filepath.Join(dir, p), nil
}

func applyOp(dir string, op WorkspaceOp) error {
	switch {
	case op.Write != "" || op.WriteB64 != "":
		name := op.Write
		if op.WriteB64 != "" {
			b, err := base64.StdEncoding.DecodeString(op.WriteB64)
			if err != nil {
				return err
			}
			name = string(b)
		}
		p, err := rel(dir, name)
		if err != nil {
			return err
		}
		data := []byte(op.Data)
		if op.DataB64 != "" {
			if data, err = base64.StdEncoding.DecodeString(op.DataB64); err != nil {
				return err
			}
		}
		mode := fs.FileMode(op.Mode)
		if mode == 0 {
			mode = 0o644
		}
		os.MkdirAll(filepath.Dir(p), 0o755)
		os.Remove(p)
		if err := os.WriteFile(p, data, mode); err != nil {
			return err
		}
		return os.Chmod(p, mode)
	case op.Remove != "":
		p, err := rel(dir, op.Remove)
		if err != nil {
			return err
		}
		return os.RemoveAll(p)
	case op.Mkdir != "":
		p, err := rel(dir, op.Mkdir)
		if err != nil {
			return err
		}
		return os.MkdirAll(p, 0o755)
	case op.Symlink != "":
		p, err := rel(dir, op.Symlink)
		if err != nil {
			return err
		}
		os.MkdirAll(filepath.Dir(p), 0o755)
		os.Remove(p)
		return os.Symlink(op.Target, p)
	case op.Chmod != "":
		p, err := rel(dir, op.Chmod)
		if err != nil {
			return err
		}
		return os.Chmod(p, fs.FileMode(op.Mode))
	case op.Copy != "":
		src, err := rel(dir, op.Copy)
		if err != nil {
			return err
		}
		dst, err := rel(dir, op.Target)
		if err != nil {
			return err
		}
		b, err := os.ReadFile(src)
		if err != nil {
			return err
		}
		os.MkdirAll(filepath.Dir(dst), 0o755)
		return os.WriteFile(dst, b, 0o644)
	case op.Many != "":
		for i := 0; i < op.Count; i++ {
			p, err := rel(dir, fmt.Sprintf("%s-%05d.txt", op.Many, i))
			if err != nil {
				return err
			}
			os.MkdirAll(filepath.Dir(p), 0o755)
			if err := os.WriteFile(p, []byte("x"), 0o644); err != nil {
				return err
			}
		}
		return nil
	case op.GitCommit != "":
		return gitCommit(dir, op.GitBranch, op.GitCommit)
	case op.RemoveGit:
		return os.RemoveAll(filepath.Join(dir, ".git"))
	case op.Touch != "":
		if !filepath.IsAbs(op.Touch) {
			return fmt.Errorf("touch path %q is not absolute", op.Touch)
		}
		return os.WriteFile(op.Touch, nil, 0o600)
	case op.WaitFor != "":
		if !filepath.IsAbs(op.WaitFor) {
			return fmt.Errorf("wait path %q is not absolute", op.WaitFor)
		}
		deadline := time.Now().Add(2 * time.Minute)
		for {
			if _, err := os.Stat(op.WaitFor); err == nil {
				return nil
			}
			if time.Now().After(deadline) {
				return fmt.Errorf("%s did not appear", op.WaitFor)
			}
			time.Sleep(5 * time.Millisecond)
		}
	case op.SleepMS > 0:
		time.Sleep(time.Duration(op.SleepMS) * time.Millisecond)
		return nil
	}
	return fmt.Errorf("empty op")
}

// gitCommit commits every worktree change in the child's own repository
// with go-git (optionally on a new branch), moving its HEAD.
func gitCommit(dir, branch, msg string) error {
	r, err := git.PlainOpen(dir)
	if err != nil {
		return err
	}
	w, err := r.Worktree()
	if err != nil {
		return err
	}
	if branch != "" {
		if err := w.Checkout(&git.CheckoutOptions{Branch: plumbing.NewBranchReferenceName(branch), Create: true, Keep: true}); err != nil {
			return err
		}
	}
	if err := w.AddWithOptions(&git.AddOptions{All: true}); err != nil {
		return err
	}
	sig := &object.Signature{Name: "Child", Email: "child@example.invalid", When: time.Unix(1700000000, 0).UTC()}
	_, err = w.Commit(msg, &git.CommitOptions{Author: sig, Committer: sig, AllowEmptyCommits: true})
	return err
}

// writeWorkspaceReport writes what the child sees in dir to the absolute
// path out.
func writeWorkspaceReport(dir, out string, getenv func(string) string) error {
	if !filepath.IsAbs(out) {
		return fmt.Errorf("report path %q is not absolute", out)
	}
	fi, err := os.Stat(dir)
	if err != nil {
		return err
	}
	path, pathSet := os.LookupEnv("PATH")
	rep := WorkspaceReport{CWD: dir, CWDMode: fmt.Sprintf("%04o", fi.Mode().Perm()), PWD: getenv("PWD"), PATH: path, PathSet: pathSet, Files: map[string]string{}}
	if b, err := os.ReadFile(filepath.Join(dir, ".git", "HEAD")); err == nil {
		rep.HEAD = string(b)
	}
	if _, err := os.Lstat(filepath.Join(dir, ".git")); err == nil {
		rep.HasGit = true
	}
	err = filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		r, _ := filepath.Rel(dir, p)
		if r == "." {
			return nil
		}
		if r == ".git" {
			return filepath.SkipDir
		}
		info, err := os.Lstat(p)
		if err != nil {
			return err
		}
		var kind string
		var content []byte
		switch {
		case info.Mode()&fs.ModeSymlink != 0:
			kind = "l"
			t, err := os.Readlink(p)
			if err != nil {
				return err
			}
			content = []byte(t)
		case info.IsDir():
			kind = "d"
		default:
			kind = "f"
			if content, err = os.ReadFile(p); err != nil {
				return err
			}
		}
		sum := sha256.Sum256(content)
		rep.Files[filepath.ToSlash(r)] = fmt.Sprintf("%s %04o %s", kind, info.Mode().Perm(), hex.EncodeToString(sum[:]))
		return nil
	})
	if err != nil {
		return err
	}
	b, err := json.Marshal(rep)
	if err != nil {
		return err
	}
	tmp := out + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, out)
}

// SortedPaths returns the report's paths in byte order.
func (r WorkspaceReport) SortedPaths() []string {
	out := make([]string, 0, len(r.Files))
	for k := range r.Files {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
