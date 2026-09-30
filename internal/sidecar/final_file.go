package sidecar

import (
	"errors"
	"path/filepath"
	"sync"

	"github.com/wedevwork/callsheet/internal/adapter"
)

// Final-message file ownership (iteration 08, Codex). The sidecar owns
// every file operation: the adapter only declares the path. Right after
// the scratch directory is created the sidecar opens and keeps a handle on
// that exact directory, accepts only the fixed basename inside it, and
// requires the name not to exist yet (it never creates the file: absence
// after exit is meaningful). After the guardian reported exit, the output
// drained and the group's absence was confirmed, and before the scratch
// directory is removed, it reads the file relative to the retained handle
// without following links (finalfile_unix.go).

// finalSource is one task's declared final-message file: the retained
// scratch directory handle and the fixed basename.
type finalSource struct {
	dir  finalDir
	name string
	once sync.Once
}

// close releases the retained directory handle (idempotent; nil-safe).
func (f *finalSource) close() {
	if f == nil {
		return
	}
	f.once.Do(func() { f.dir.close() })
}

// errFinalPath is a declared final file other than the scratch
// directory's fixed basename.
var errFinalPath = errors.New("the declared final-message file is not the task's fixed scratch file")

// errFinalExists is a final-message name already present before launch.
var errFinalExists = errors.New("the final-message file exists before launch")

// ownFinal establishes ownership of inv's declared final file in scratch:
// nil for a stdout source; otherwise the retained no-follow directory
// handle, after checking the path is exactly scratch plus the fixed
// basename and that nothing exists under it yet.
func ownFinal(inv adapter.Invocation, scratch string) (*finalSource, error) {
	if inv.FinalFile == "" {
		return nil, nil
	}
	p := inv.FinalFile
	if !filepath.IsAbs(p) || filepath.Clean(p) != p || filepath.Dir(p) != scratch || filepath.Base(p) != adapter.CodexFinalName {
		return nil, errFinalPath
	}
	dir, err := openFinalDir(scratch)
	if err != nil {
		return nil, err
	}
	if err := dir.absent(adapter.CodexFinalName); err != nil {
		dir.close()
		return nil, err
	}
	return &finalSource{dir: dir, name: adapter.CodexFinalName}, nil
}

// finalError is a failed final-file read: its error text is the fixed
// classification only (never contents or a path).
type finalError struct{ code string }

func (e *finalError) Error() string { return e.code }

// failed returns the null final message and error of classification code.
func failed(code string) (adapter.FinalMessage, error) {
	return adapter.FinalMessage{Error: code}, &finalError{code: code}
}

// finalChunk is the final-file reader's fixed buffer size.
const finalChunk = 32 << 10
