package workspacetransfer

import (
	"bytes"
	"errors"
	"io"
	"strings"

	"github.com/go-git/go-git/v5/plumbing"
	"golang.org/x/sys/unix"

	"github.com/wedevwork/callsheet/internal/contract"
)

// Local ref reading for git sources and destinations: loose refs are
// reached component by component through no-follow directory descriptors
// (a symlinked refs component or ref file is refused, never followed) and
// packed-refs is read without following it. Values are full hashes or
// "ref: " symbolic targets.

var errUnsafeMetadata = errors.New("unsafe repository metadata")

// refValue is a loose or packed ref's content.
type refValue struct {
	hash     plumbing.Hash
	symbolic string // target of a symbolic ref
}

// validRefName reports a well-formed ref name below refs/ (Git's
// check_refname_format, plus the namespace): slash-separated nonempty
// components, none starting with "." or ending with ".lock", no "..",
// no "@{", no control characters, space, "~", "^", ":", "?", "*", "[" or
// "\\", not ending with "/" or ".". Such a name never leaves the git
// directory when its components are traversed.
func validRefName(name string) bool {
	if !strings.HasPrefix(name, "refs/") || strings.HasSuffix(name, ".") || strings.Contains(name, "..") || strings.Contains(name, "@{") {
		return false
	}
	for _, c := range strings.Split(name, "/") {
		if c == "" || c[0] == '.' || strings.HasSuffix(c, ".lock") {
			return false
		}
		for i := 0; i < len(c); i++ {
			switch b := c[i]; {
			case b < 0x20, b == 0x7f, strings.IndexByte(" ~^:?*[\\", b) >= 0:
				return false
			}
		}
	}
	return true
}

// parseRefContent decodes a loose ref file: a full hash, or "ref: " and
// a valid ref name.
func parseRefContent(b []byte) (refValue, bool) {
	// Git trims only its sane isspace — space, tab, CR and LF — before
	// parse_loose_ref_contents. Vertical tab and form feed are not
	// whitespace there; trimming them accepts a ref Git rejects.
	s := string(bytes.TrimRight(b, " \t\n\r"))
	if t, ok := strings.CutPrefix(s, "ref: "); ok {
		t = strings.Trim(t, " \t\n\r")
		if !validRefName(t) {
			return refValue{}, false
		}
		return refValue{symbolic: t}, true
	}
	if !contract.ValidCommitHash(s) {
		return refValue{}, false
	}
	return refValue{hash: plumbing.NewHash(s)}, true
}

// maxRefFile bounds a loose ref or HEAD file (a hash or "ref: " and a
// name): a larger one is malformed and is not read whole.
const maxRefFile = 4096

// readRefFile reads a loose ref or HEAD file, refusing an oversized one.
func readRefFile(dfd int, name string) ([]byte, bool, error) {
	b, found, err := readFileMax(dfd, name, maxRefFile)
	if errors.Is(err, errTooLarge) {
		return nil, false, errUnsafeMetadata
	}
	return b, found, err
}

var errTooLarge = errors.New("file too large")

// readSmallFile reads a regular file below dfd without following it.
func readSmallFile(dfd int, name string) ([]byte, bool, error) {
	return readFileMax(dfd, name, -1)
}

// readFileMax reads a regular file below dfd without following it; with
// max >= 0, more than max bytes is errTooLarge.
func readFileMax(dfd int, name string, max int) ([]byte, bool, error) {
	f, _, err := openFileAt(dfd, name)
	if err != nil {
		if errors.Is(err, unix.ENOENT) {
			return nil, false, nil
		}
		if errors.Is(err, unix.ELOOP) || errors.Is(err, errNotRegular) || errors.Is(err, unix.ENOTDIR) {
			return nil, false, errUnsafeMetadata
		}
		return nil, false, err
	}
	defer f.Close()
	buf := make([]byte, 0, 256)
	tmp := make([]byte, 32<<10)
	for {
		n, rerr := f.Read(tmp)
		buf = append(buf, tmp[:n]...)
		if max >= 0 && len(buf) > max {
			return nil, false, errTooLarge
		}
		if errors.Is(rerr, io.EOF) {
			break
		}
		if rerr != nil {
			return nil, false, rerr
		}
	}
	return buf, true, nil
}

// looseRef reads the loose ref name below the git directory gitFD. A
// missing component is absence; a symlink or a non-directory ancestor
// component is unsafe (errUnsafeMetadata), a directory at the ref's own
// path is reported as isDir.
type looseResult struct {
	value  refValue
	found  bool
	isDir  bool
	prefix bool // an ancestor component is a file (namespace conflict)
}

func looseRef(gitFD int, name string) (looseResult, error) {
	if !validRefName(name) {
		return looseResult{}, errUnsafeMetadata
	}
	parts := strings.Split(name, "/")
	dfd, err := unix.Dup(gitFD)
	if err != nil {
		return looseResult{}, err
	}
	for i, p := range parts {
		st, err := lstatAt(dfd, p)
		if errors.Is(err, unix.ENOENT) {
			closeFD(dfd)
			return looseResult{}, nil
		}
		if err != nil {
			closeFD(dfd)
			return looseResult{}, err
		}
		last := i == len(parts)-1
		switch {
		case isLinkStat(&st):
			closeFD(dfd)
			return looseResult{}, errUnsafeMetadata
		case last && isDirStat(&st):
			closeFD(dfd)
			return looseResult{isDir: true}, nil
		case last && isRegStat(&st):
			b, found, err := readRefFile(dfd, p)
			closeFD(dfd)
			if err != nil || !found {
				return looseResult{}, err
			}
			v, ok := parseRefContent(b)
			if !ok {
				return looseResult{}, errUnsafeMetadata
			}
			return looseResult{value: v, found: true}, nil
		case last:
			closeFD(dfd)
			return looseResult{}, errUnsafeMetadata
		case isRegStat(&st):
			closeFD(dfd)
			return looseResult{prefix: true}, nil
		case !isDirStat(&st):
			closeFD(dfd)
			return looseResult{}, errUnsafeMetadata
		}
		next, err := openDirAt(dfd, p)
		closeFD(dfd)
		if err != nil {
			return looseResult{}, err
		}
		dfd = next
	}
	closeFD(dfd)
	return looseResult{}, nil
}

// packedRef is one packed-refs line.
type packedRefs struct {
	refs   map[string]plumbing.Hash
	names  []string
	exists bool
}

// checkRefnameFormat is Git's check_refname_format: slash-separated
// components, each nonempty, not starting with "." or ending with
// ".lock", without "..", "@{", control characters, DEL, space, ":", "?",
// "[", "\\", "^", "~" or "*"; the name is not "@" and does not end with
// "."; with oneLevel a single component is allowed.
func checkRefnameFormat(name string, oneLevel bool) bool {
	if name == "@" || strings.HasSuffix(name, ".") {
		return false
	}
	comps := strings.Split(name, "/")
	for _, c := range comps {
		if c == "" || c[0] == '.' || strings.HasSuffix(c, ".lock") || strings.Contains(c, "..") || strings.Contains(c, "@{") {
			return false
		}
		for i := 0; i < len(c); i++ {
			if b := c[i]; b < 0x20 || b == 0x7f || strings.IndexByte(" :?[\\^~*", b) >= 0 {
				return false
			}
		}
	}
	return oneLevel || len(comps) >= 2
}

// refnameIsSafe is Git's refname_is_safe: below refs/, a nonempty rest
// without a leading or trailing slash that path normalization leaves
// unchanged (no empty, "." or ".." component); otherwise only uppercase
// letters and underscores.
func refnameIsSafe(name string) bool {
	if rest, ok := strings.CutPrefix(name, "refs/"); ok {
		if rest == "" || rest[0] == '/' || rest[len(rest)-1] == '/' {
			return false
		}
		for _, c := range strings.Split(rest, "/") {
			if c == "" || c == "." || c == ".." {
				return false
			}
		}
		return true
	}
	if name == "" {
		return false
	}
	for i := 0; i < len(name); i++ {
		if c := name[i]; !(c >= 'A' && c <= 'Z' || c == '_') {
			return false
		}
	}
	return true
}

// parseHashAt reads 40 hexadecimal digits (either case, Git's
// parse_oid_hex) at b[i:].
func parseHashAt(b []byte, i int) (plumbing.Hash, bool) {
	if i+40 > len(b) {
		return plumbing.ZeroHash, false
	}
	for _, c := range b[i : i+40] {
		if hexDigit(c) < 0 {
			return plumbing.ZeroHash, false
		}
	}
	return plumbing.NewHash(strings.ToLower(string(b[i : i+40]))), true
}

// readPackedRefs parses packed-refs with Git's record grammar
// (refs/packed-backend.c), validating every record (Git validates a
// record when a lookup or iteration visits it; Callsheet refuses any
// record Git would refuse on a visit): an optional first line "# pack-refs
// with:..."; then records "<40 hex> <Git whitespace: space, tab, CR or
// LF> <name> LF", each optionally followed by "^<40 hex> LF"; every line
// LF-terminated and at least 42 bytes. A name check_refname_format
// rejects is ignored when refname_is_safe holds (a trailing CR, \v or
// space included) and refuses the repository when it does not (an empty
// name, "//", "." or ".." components, a leading or trailing slash). NUL
// anywhere is refused.
func readPackedRefs(gitFD int) (packedRefs, error) {
	out := packedRefs{refs: map[string]plumbing.Hash{}}
	b, found, err := readSmallFile(gitFD, "packed-refs")
	if err != nil || !found {
		return out, err
	}
	out.exists = true
	if len(b) == 0 {
		return out, nil
	}
	if b[len(b)-1] != '\n' || bytes.IndexByte(b, 0) >= 0 {
		return out, errUnsafeMetadata
	}
	pos := 0
	if b[0] == '#' {
		eol := bytes.IndexByte(b, '\n')
		if !bytes.HasPrefix(b[:eol], []byte("# pack-refs with:")) {
			return out, errUnsafeMetadata
		}
		pos = eol + 1
	}
	for pos < len(b) {
		eol := pos + bytes.IndexByte(b[pos:], '\n')
		if eol-pos < 42 {
			return out, errUnsafeMetadata
		}
		h, ok := parseHashAt(b, pos)
		if !ok || strings.IndexByte(" \t\n\r", b[pos+40]) < 0 {
			return out, errUnsafeMetadata
		}
		nameStart := pos + 41
		nameEnd := nameStart + bytes.IndexByte(b[nameStart:], '\n')
		name := string(b[nameStart:nameEnd])
		pos = nameEnd + 1
		if pos < len(b) && b[pos] == '^' {
			if _, ok := parseHashAt(b, pos+1); !ok || pos+41 >= len(b) || b[pos+41] != '\n' {
				return out, errUnsafeMetadata
			}
			pos += 42
		}
		switch {
		case !checkRefnameFormat(name, true):
			if !refnameIsSafe(name) {
				return out, errUnsafeMetadata
			}
		case validRefName(name):
			out.refs[name] = h
			out.names = append(out.names, name)
		}
	}
	return out, nil
}

// maxSymrefDepth is Git's SYMREF_MAXDEPTH.
const maxSymrefDepth = 5

// resolveRef resolves name (loose first, then packed) through symbolic
// refs (at most five hops, every name validated before it is read) to a
// hash; found is false for a missing ref.
func resolveRef(gitFD int, name string) (plumbing.Hash, bool, error) {
	packed, err := readPackedRefs(gitFD)
	if err != nil {
		return plumbing.ZeroHash, false, err
	}
	for hop := 0; hop < maxSymrefDepth; hop++ {
		lr, err := looseRef(gitFD, name)
		if err != nil {
			return plumbing.ZeroHash, false, err
		}
		if lr.isDir || lr.prefix {
			return plumbing.ZeroHash, false, nil
		}
		if lr.found {
			if lr.value.symbolic == "" {
				return lr.value.hash, true, nil
			}
			name = lr.value.symbolic
			continue
		}
		h, ok := packed.refs[name]
		return h, ok, nil
	}
	return plumbing.ZeroHash, false, errUnsafeMetadata
}

// headState is a repository's HEAD.
type headState struct {
	raw      []byte
	symbolic string // "ref: " target, "" when detached
	hash     plumbing.Hash
	unborn   bool
}

// branch is the short branch name of a HEAD on refs/heads/ ("" otherwise).
func (h headState) branch() string {
	b, _ := strings.CutPrefix(h.symbolic, "refs/heads/")
	if b == h.symbolic {
		return ""
	}
	return b
}

// readHead reads and resolves HEAD.
func readHead(gitFD int) (headState, error) {
	b, found, err := readRefFile(gitFD, "HEAD")
	if err != nil {
		return headState{}, err
	}
	if !found {
		return headState{}, errUnsafeMetadata
	}
	v, ok := parseRefContent(b)
	if !ok {
		return headState{}, errUnsafeMetadata
	}
	hs := headState{raw: b, symbolic: v.symbolic, hash: v.hash}
	if v.symbolic == "" {
		return hs, nil
	}
	h, found, err := resolveRef(gitFD, v.symbolic)
	if err != nil {
		return headState{}, err
	}
	hs.hash, hs.unborn = h, !found
	return hs, nil
}
