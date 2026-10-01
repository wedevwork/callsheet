package workspacetransfer

import (
	"bytes"
	"crypto/sha1"
	"encoding/binary"
	"errors"
	"strings"
	"testing"

	"github.com/go-git/go-git/v5/plumbing"
)

// TestAttributesParse drives the attributes parser directly over Git's
// line grammar: BOM, comments, over-long lines, C-quoted patterns, state
// tokens, macros and the refused conversion macro.
func TestAttributesParse(t *testing.T) {
	long := strings.Repeat("x", maxAttrLine) + " text\n"
	content := "\xef\xbb\xbf# comment\n\n" + long +
		"   *.txt text eol=crlf -ident !filter\n" +
		"\"quo\\164ed\\040name.c\" ident\n" + // octal escapes: "quoted name.c"
		"\"esc\\a\\b\\f\\n\\r\\t\\v\\\\\\\"\" text\n" +
		"\"unterminated text\n" +
		"\"bad\\q\" text\n" +
		"\"short\\1\" text\n" +
		"\"trail\\\n" +
		"\"oct\\389\" text\n" +
		"bad.* -\n" + // an empty unset name invalidates the whole line
		"inv.* bad@name\n" +
		"!neg.txt text\n" + // negative patterns are ignored with a warning in Git
		"[attr]inert diff merge=ours\n" +
		"[attr] spaced -diff\n" + // the pattern "[attr]": its first token is only the prefix
		"\"[attr]q2 ignored\" -diff\n" + // a quoted macro name stops at the first blank
		"[attr]bad@ text\n" +
		"[attr]binary -diff -merge -text\n" + // an equivalent redefinition of the built-in
		"\"[attr]qmacro\" -merge\n" +
		"*.bin binary\n" +
		"*.m inert\n" +
		"last.go eol=lf"
	f, err := parseAttributes([]byte(content), "", true, false)
	if err != nil {
		t.Fatal(err)
	}
	var pats []string
	for _, l := range f.lines {
		pats = append(pats, l.pat.text)
	}
	if got, want := strings.Join(pats, "|"), "*.txt|quoted name.c|esc\a\b\f\n\r\t\v\\\"|[attr]|*.bin|*.m|last.go"; got != want {
		t.Fatalf("patterns %q, want %q", got, want)
	}
	if len(f.macros) != 3 || len(f.macros["inert"]) != 2 || f.macros["q2"][0].state != attrUnset || f.macros["qmacro"][0].state != attrUnset {
		t.Fatalf("macros %+v", f.macros)
	}
	a := f.lines[0].assigns
	if len(a) != 4 || a[0].state != attrSet || a[1].value != "crlf" || a[2].state != attrUnset || a[3].state != attrUnspecified {
		t.Fatalf("states %+v", a)
	}

	// Below the top level, macro definitions are ignored.
	nested, err := parseAttributes([]byte("[attr]m text\nsub.c text\n"), "dir/", false, false)
	if err != nil || len(nested.macros) != 0 || len(nested.lines) != 1 {
		t.Fatalf("nested %+v %v", nested, err)
	}
	// A user macro that changes a conversion attribute is refused.
	// So is a redefinition of binary that changes its conversion (real
	// Git: "[attr]binary text" then "f binary" gives text: set).
	for _, def := range []string{"[attr]conv text\n", "[attr]conv -eol diff\n", "[attr]conv filter=lfs\n", "[attr]conv binary\n", "[attr]binary text\n"} {
		if _, err := parseAttributes([]byte(def), "", true, false); !errors.As(err, new(errUnsupportedMacro)) || err.Error() != "unsupported attribute macro" {
			t.Fatalf("%q: %v", def, err)
		}
	}

	// Resolution: later files win, binary expands, user macros expand
	// (the latest definition first), unset and value states are recorded.
	low, _ := parseAttributes([]byte("[attr]inert diff\n*.bin text\n*.m -text\n"), "", true, false)
	s := resolveAttrs([]*attrFile{nil, low, f}, "x.bin")
	if s["text"].state != attrUnset || s["diff"].state != attrUnset || s["merge"].state != attrUnset || s["binary"].state != attrSet {
		t.Fatalf("binary %+v", s)
	}
	s = resolveAttrs([]*attrFile{low, f}, "y.m")
	if s["text"].state != attrUnset || s["diff"].state != attrSet || s["merge"].value != "ours" {
		t.Fatalf("macro %+v", s)
	}
	s = resolveAttrs([]*attrFile{f}, "a.txt")
	if s["text"].state != attrSet || s["eol"].value != "crlf" || s["ident"].state != attrUnset || s["filter"].state != attrUnspecified {
		t.Fatalf("txt %+v", s)
	}
	if s := resolveAttrs([]*attrFile{f}, "dir/quoted name.c"); s["ident"].state != attrSet {
		t.Fatalf("quoted %+v", s)
	}
	if s := resolveAttrs([]*attrFile{f}, "other"); len(s) != 0 {
		t.Fatalf("unmatched %+v", s)
	}
	// An unset macro name records only itself.
	s = attrMap{}
	s.apply(attrAssign{name: "binary", attrState: attrState{state: attrUnset}}, nil)
	if len(s) != 1 {
		t.Fatalf("unset binary %+v", s)
	}
}

// TestUnquoteC checks Git's C-style unquoting edge cases.
func TestUnquoteC(t *testing.T) {
	for _, tc := range []struct {
		in, name, rest string
		ok             bool
	}{
		{`"a b" x`, "a b", " x", true},
		{`"\101\060"`, "A0", "", true},
		{`"\377"`, "\xff", "", true},
		{`"\400"`, "", "", false},
		{`"\12"`, "", "", false},
		{`"\8"`, "", "", false},
		{`"abc`, "", "", false},
		{`"\`, "", "", false},
	} {
		n, r, ok := unquoteC(tc.in)
		if n != tc.name || r != tc.rest || ok != tc.ok {
			t.Fatalf("%q = %q %q %v", tc.in, n, r, ok)
		}
	}
}

// idxEntry describes one entry for the index builder.
type idxEntry struct {
	name     string
	mode     uint32
	stage    int
	assume   bool
	skip     bool
	ita      bool
	reserved bool
	strip    int // v4 prefix length removed from the previous name
}

// buildIndex encodes an index file of the given version with entries and
// raw extension records, adding the trailing checksum.
func buildIndex(version uint32, entries []idxEntry, exts ...[]byte) []byte {
	var b bytes.Buffer
	b.WriteString("DIRC")
	_ = binary.Write(&b, binary.BigEndian, version)
	_ = binary.Write(&b, binary.BigEndian, uint32(len(entries)))
	prev := ""
	for i, e := range entries {
		start := b.Len()
		hdr := make([]byte, 62)
		binary.BigEndian.PutUint32(hdr[24:], e.mode)
		h := plumbing.ComputeHash(plumbing.BlobObject, []byte{byte(i)})
		copy(hdr[40:], h[:])
		flags := uint16(len(e.name))
		if len(e.name) > 0xfff {
			flags = 0xfff
		}
		flags |= uint16(e.stage&3) << 12
		if e.assume {
			flags |= 0x8000
		}
		ext := e.skip || e.ita || e.reserved
		if ext {
			flags |= 0x4000
		}
		binary.BigEndian.PutUint16(hdr[60:], flags)
		b.Write(hdr)
		if ext {
			var x uint16
			if e.skip {
				x |= 0x4000
			}
			if e.ita {
				x |= 0x2000
			}
			if e.reserved {
				x |= 0x8000
			}
			_ = binary.Write(&b, binary.BigEndian, x)
		}
		if version == 4 {
			b.Write(encodeVarint(uint64(e.strip)))
			b.WriteString(e.name[len(prev)-e.strip:])
			b.WriteByte(0)
		} else {
			b.WriteString(e.name)
			pad := 8 - (b.Len()-start)%8
			b.Write(make([]byte, pad))
		}
		prev = e.name
	}
	for _, x := range exts {
		b.Write(x)
	}
	sum := sha1.Sum(b.Bytes())
	b.Write(sum[:])
	return b.Bytes()
}

// encodeVarint is Git's offset varint encoder.
func encodeVarint(v uint64) []byte {
	var buf [16]byte
	pos := len(buf) - 1
	buf[pos] = byte(v & 127)
	for v >>= 7; v != 0; v >>= 7 {
		v--
		pos--
		buf[pos] = 128 | byte(v&127)
	}
	return buf[pos:]
}

func extRecord(sig string, body []byte) []byte {
	r := []byte(sig)
	r = binary.BigEndian.AppendUint32(r, uint32(len(body)))
	return append(r, body...)
}

// resum replaces the trailing checksum of a hand-edited index.
func resum(data []byte) []byte {
	body := data[:len(data)-sha1.Size]
	sum := sha1.Sum(body)
	return append(append([]byte{}, body...), sum[:]...)
}

// TestIndexParse decodes versions 2-4 (with go-git's cross-check) and
// refuses every malformed or unsupported shape.
func TestIndexParse(t *testing.T) {
	plain := []idxEntry{{name: "a.txt", mode: 0o100644}, {name: "dir/b", mode: 0o100755}, {name: "dir/link", mode: 0o120000}}
	for _, v := range []uint32{2, 3} {
		idx, err := parseIndex(buildIndex(v, plain, extRecord("TREE", nil)))
		if err != nil || idx.version != v || len(idx.entries) != 3 || idx.entries[1].name != "dir/b" || idx.entries[2].mode != 0o120000 ||
			len(idx.extensions) != 1 || idx.extensions[0] != "TREE" || idx.unsupported() != "" {
			t.Fatalf("v%d: %+v %v", v, idx, err)
		}
	}
	v4 := []idxEntry{{name: "dir/a", mode: 0o100644}, {name: "dir/b", mode: 0o100644, strip: 1}, {name: "e", mode: 0o100664, strip: 5}}
	idx, err := parseIndex(buildIndex(4, v4))
	if err != nil || len(idx.entries) != 3 || idx.entries[1].name != "dir/b" || idx.entries[2].name != "e" || idx.unsupported() != "" {
		t.Fatalf("v4: %+v %v", idx, err)
	}
	// Intent-to-add is a supported extended flag (v3).
	idx, err = parseIndex(buildIndex(3, []idxEntry{{name: "new", mode: 0o100644, ita: true}}))
	if err != nil || !idx.entries[0].intentToAdd || idx.unsupported() != "" {
		t.Fatalf("ita: %+v %v", idx, err)
	}

	// Unsupported features are decoded, then named.
	for _, tc := range []struct {
		data []byte
		want string
	}{
		{buildIndex(2, plain, extRecord("link", make([]byte, 20))), "a split index"},
		{buildIndex(2, plain, extRecord("sdir", nil)), "a sparse index"},
		{buildIndex(2, plain, extRecord("abcd", nil)), "an unsupported index extension"},
		{buildIndex(2, []idxEntry{{name: "c", mode: 0o100644, stage: 2}}), "unresolved merge conflicts in the index"},
		{buildIndex(3, []idxEntry{{name: "s", mode: 0o100644, skip: true}}), "sparse checkout (skip-worktree entries)"},
		{buildIndex(2, []idxEntry{{name: "v", mode: 0o100644, assume: true}}), "assume-unchanged index entries"},
		{buildIndex(2, []idxEntry{{name: "sub", mode: 0o160000}}), "submodules (gitlinks)"},
		{buildIndex(2, []idxEntry{{name: "odd", mode: 0o100600}}), "an unsupported index entry mode"},
	} {
		idx, err := parseIndex(tc.data)
		if err != nil || idx.unsupported() != tc.want {
			t.Fatalf("want %q: %+v %v", tc.want, idx, err)
		}
	}

	good := buildIndex(2, plain)
	badPad := append([]byte{}, good...)
	badPad[12+62+len("a.txt")] = 'x' // the first padding byte
	truncExt := buildIndex(2, plain, []byte("TRE"))
	bigExt := buildIndex(2, plain, extRecord("TREE", nil)[:4], binary.BigEndian.AppendUint32(nil, 1<<20))
	badVer := append([]byte{}, good...)
	binary.BigEndian.PutUint32(badVer[4:], 5)
	manyEntries := append([]byte{}, good...)
	binary.BigEndian.PutUint32(manyEntries[8:], 1000)
	noNul := buildIndex(4, []idxEntry{{name: "x", mode: 0o100644}})
	noNul = resum(append(noNul[:len(noNul)-sha1.Size-1], noNul[len(noNul)-sha1.Size:]...))
	noNul2 := buildIndex(2, []idxEntry{{name: "abcdefg", mode: 0o100644}})
	noNul2 = resum(bytes.ReplaceAll(noNul2[:len(noNul2)-sha1.Size], []byte("abcdefg\x00"), []byte("abcdefgh")))
	shortPad := buildIndex(2, []idxEntry{{name: "abcdef", mode: 0o100644}})
	shortPad = resum(shortPad[:len(shortPad)-sha1.Size-1])
	badStrip := buildIndex(4, []idxEntry{{name: "a", mode: 0o100644}})
	badStrip[12+62] = 5 // strip more than the (empty) previous name
	badStrip = resum(badStrip)
	badVarint := buildIndex(4, []idxEntry{{name: "a", mode: 0o100644}})
	badVarint = resum(append(append(append([]byte{}, badVarint[:12+62]...), 0x80), badVarint[len(badVarint)-sha1.Size:]...))
	// go-git's decoder disagrees: a v2 name length field that does not
	// match the NUL-terminated name.
	lenLie := append([]byte{}, good...)
	binary.BigEndian.PutUint16(lenLie[12+60:], uint16(len("a.tx")))
	lenLie = resum(lenLie)
	sumLie := append([]byte{}, good...)
	sumLie[len(sumLie)-1] ^= 1
	for name, data := range map[string][]byte{
		"short":         []byte("DIRC"),
		"signature":     append([]byte("DIRX"), good[4:]...),
		"checksum":      sumLie,
		"version":       resum(badVer),
		"entry count":   resum(manyEntries),
		"padding":       resum(badPad),
		"short padding": shortPad,
		"no NUL v2":     noNul2,
		"no NUL v4":     noNul,
		"strip":         badStrip,
		"varint":        badVarint,
		"ext header":    truncExt,
		"ext size":      bigExt,
		"v2 extended":   buildIndex(2, []idxEntry{{name: "s", mode: 0o100644, skip: true}}),
		"reserved flag": buildIndex(3, []idxEntry{{name: "s", mode: 0o100644, reserved: true}}),
		"go-git":        lenLie,
	} {
		if idx, err := parseIndex(data); !errors.Is(err, errIndexFormat) {
			t.Fatalf("%s: %+v %v", name, idx, err)
		}
	}
}

// TestDecodeVarint covers Git's offset varint bounds.
func TestDecodeVarint(t *testing.T) {
	for _, v := range []uint64{0, 1, 127, 128, 16511, 1 << 40} {
		b := encodeVarint(v)
		got, n, ok := decodeVarint(append(b, 0xff))
		if !ok || got != v || n != len(b) {
			t.Fatalf("%d: %d %d %v", v, got, n, ok)
		}
	}
	for _, b := range [][]byte{nil, {0x80}, bytes.Repeat([]byte{0xff}, 12)} {
		if _, _, ok := decodeVarint(b); ok {
			t.Fatalf("%x decoded", b)
		}
	}
}
