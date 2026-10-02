package workspacetransfer

import (
	"bytes"
	"crypto/sha1"
	"encoding/binary"
	"errors"

	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/format/index"
)

// The git index for the cleanliness check. go-git's decoder is
// authoritative for versions 2-4 (its errors refuse the repository); this
// reader additionally keeps what go-git drops and the comparison needs:
// the assume-valid flag and the extension signatures, and verifies the
// entries match go-git's. Sparse (sdir) and split (link) indexes,
// assume-unchanged, skip-worktree entries and unmerged stages are
// unsupported: their status is not a plain HEAD/index/worktree
// comparison.

// indexEntry is one index entry.
type indexEntry struct {
	name         string
	mode         uint32
	hash         plumbing.Hash
	stage        int
	assumeValid  bool
	skipWorktree bool
	intentToAdd  bool
}

// gitIndex is a decoded index.
type gitIndex struct {
	version    uint32
	entries    []indexEntry
	extensions []string
}

var errIndexFormat = errors.New("malformed git index")

// decodeVarint is Git's offset varint (index v4 name compression).
func decodeVarint(b []byte) (uint64, int, bool) {
	if len(b) == 0 {
		return 0, 0, false
	}
	c := b[0]
	val := uint64(c & 127)
	i := 1
	for c&128 != 0 {
		val++
		if val == 0 || val>>57 != 0 {
			return 0, 0, false
		}
		if i >= len(b) {
			return 0, 0, false
		}
		c = b[i]
		i++
		val = val<<7 + uint64(c&127)
	}
	return val, i, true
}

// parseIndex decodes an index file.
func parseIndex(data []byte) (*gitIndex, error) {
	if len(data) < 12+sha1.Size || string(data[:4]) != "DIRC" {
		return nil, errIndexFormat
	}
	body, sum := data[:len(data)-sha1.Size], data[len(data)-sha1.Size:]
	if h := sha1.Sum(body); !bytes.Equal(h[:], sum) {
		return nil, errIndexFormat
	}
	idx := &gitIndex{version: binary.BigEndian.Uint32(data[4:8])}
	if idx.version < 2 || idx.version > 4 {
		return nil, errIndexFormat
	}
	n := binary.BigEndian.Uint32(data[8:12])
	off := 12
	prev := ""
	for i := uint32(0); i < n; i++ {
		start := off
		if off+62 > len(body) {
			return nil, errIndexFormat
		}
		e := indexEntry{mode: binary.BigEndian.Uint32(body[off+24 : off+28])}
		copy(e.hash[:], body[off+40:off+60])
		flags := binary.BigEndian.Uint16(body[off+60 : off+62])
		off += 62
		e.assumeValid = flags&0x8000 != 0
		e.stage = int(flags>>12) & 3
		if flags&0x4000 != 0 {
			if idx.version < 3 || off+2 > len(body) {
				return nil, errIndexFormat
			}
			ext := binary.BigEndian.Uint16(body[off : off+2])
			off += 2
			e.skipWorktree = ext&0x4000 != 0
			e.intentToAdd = ext&0x2000 != 0
			if ext&0x8000 != 0 {
				return nil, errIndexFormat
			}
		}
		if idx.version == 4 {
			strip, w, ok := decodeVarint(body[off:])
			if !ok || strip > uint64(len(prev)) {
				return nil, errIndexFormat
			}
			off += w
			end := bytes.IndexByte(body[off:], 0)
			if end < 0 {
				return nil, errIndexFormat
			}
			e.name = prev[:len(prev)-int(strip)] + string(body[off:off+end])
			off += end + 1
		} else {
			end := bytes.IndexByte(body[off:], 0)
			if end < 0 {
				return nil, errIndexFormat
			}
			e.name = string(body[off : off+end])
			off += end
			// 1-8 NULs pad the entry to a multiple of eight bytes.
			pad := 8 - (off-start)%8
			if off+pad > len(body) {
				return nil, errIndexFormat
			}
			for _, b := range body[off : off+pad] {
				if b != 0 {
					return nil, errIndexFormat
				}
			}
			off += pad
		}
		prev = e.name
		idx.entries = append(idx.entries, e)
	}
	for off < len(body) {
		if off+8 > len(body) {
			return nil, errIndexFormat
		}
		sig := string(body[off : off+4])
		size := int(binary.BigEndian.Uint32(body[off+4 : off+8]))
		off += 8
		if size < 0 || off+size > len(body) {
			return nil, errIndexFormat
		}
		idx.extensions = append(idx.extensions, sig)
		off += size
	}
	// A split or sparse index is refused by name (go-git cannot decode
	// either).
	if what := idx.unsupported(); what != "" {
		return idx, nil
	}
	// go-git's decoder is authoritative: its errors refuse, and it must
	// see exactly these entries.
	var gi index.Index
	if err := index.NewDecoder(bytes.NewReader(data)).Decode(&gi); err != nil {
		return nil, errIndexFormat
	}
	if len(gi.Entries) != len(idx.entries) {
		return nil, errIndexFormat
	}
	for i, ge := range gi.Entries {
		e := idx.entries[i]
		if ge.Name != e.name || ge.Hash != e.hash || uint32(ge.Mode) != e.mode {
			return nil, errIndexFormat
		}
	}
	return idx, nil
}

// unsupportedIndex names the first index feature that is not a plain
// comparison, or "".
func (x *gitIndex) unsupported() string {
	for _, s := range x.extensions {
		switch {
		case s == "link":
			return "a split index"
		case s == "sdir":
			return "a sparse index"
		case s[0] < 'A' || s[0] > 'Z':
			return "an unsupported index extension"
		}
	}
	for _, e := range x.entries {
		switch {
		case e.stage != 0:
			return "unresolved merge conflicts in the index"
		case e.skipWorktree:
			return "sparse checkout (skip-worktree entries)"
		case e.assumeValid:
			return "assume-unchanged index entries"
		case e.mode == 0o160000:
			return "submodules (gitlinks)"
		case e.mode != 0o100644 && e.mode != 0o100755 && e.mode != 0o100664 && e.mode != 0o120000:
			return "an unsupported index entry mode"
		}
	}
	return ""
}
