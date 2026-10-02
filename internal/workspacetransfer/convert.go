package workspacetransfer

import (
	"bufio"
	"bytes"
	"context"
	"io"
	"os"
)

// Git's check-in ("clean") conversion for the cleanliness check
// (convert.c): end-of-line normalization from core.autocrlf, core.eol and
// the text/crlf/eol attributes, then ident collapsing. The index stores
// the clean form, so a worktree file is clean when the hash of its clean
// form equals its index blob. Everything streams through fixed buffers:
// memory is independent of file size.

// crlfAction is Git's convert_crlf_action.
type crlfAction int

const (
	crlfUndefined crlfAction = iota
	crlfBinary
	crlfText
	crlfTextInput
	crlfTextCRLF
	crlfAuto
	crlfAutoInput
	crlfAutoCRLF
)

// autocrlf values.
const (
	autoCRLFFalse = iota
	autoCRLFTrue
	autoCRLFInput
)

// eolConfig is the effective core.autocrlf and core.eol.
type eolConfig struct {
	autocrlf int
	eolCRLF  bool // core.eol=crlf
}

func textEOLIsCRLF(c eolConfig) bool {
	switch c.autocrlf {
	case autoCRLFTrue:
		return true
	case autoCRLFInput:
		return false
	}
	return c.eolCRLF
}

func checkCRLF(s attrState) crlfAction {
	switch s.state {
	case attrSet:
		return crlfText
	case attrUnset:
		return crlfBinary
	case attrValue:
		switch s.value {
		case "input":
			return crlfTextInput
		case "auto":
			return crlfAuto
		}
	}
	return crlfUndefined
}

// conversion is the resolved clean conversion of one path.
type conversion struct {
	action crlfAction
	ident  bool
}

// resolveConversion is Git's convert_attrs for the check-in direction.
func resolveConversion(attrs attrMap, c eolConfig) conversion {
	a := checkCRLF(attrs["text"])
	if a == crlfUndefined {
		a = checkCRLF(attrs["crlf"])
	}
	if a != crlfBinary {
		eol := attrs["eol"]
		isLF := eol.state == attrValue && eol.value == "lf"
		isCRLF := eol.state == attrValue && eol.value == "crlf"
		switch {
		case a == crlfAuto && isLF:
			a = crlfAutoInput
		case a == crlfAuto && isCRLF:
			a = crlfAutoCRLF
		case isLF:
			a = crlfTextInput
		case isCRLF:
			a = crlfTextCRLF
		}
	}
	if a == crlfText {
		if textEOLIsCRLF(c) {
			a = crlfTextCRLF
		} else {
			a = crlfTextInput
		}
	}
	if a == crlfUndefined {
		switch c.autocrlf {
		case autoCRLFFalse:
			a = crlfBinary
		case autoCRLFTrue:
			a = crlfAutoCRLF
		case autoCRLFInput:
			a = crlfAutoInput
		}
	}
	return conversion{action: a, ident: attrs["ident"].state == attrSet}
}

func (a crlfAction) auto() bool { return a == crlfAuto || a == crlfAutoInput || a == crlfAutoCRLF }

// textStats are Git's gather_stats counters.
type textStats struct {
	nul, loneCR, loneLF, crlf, printable, nonprintable int64
	last                                               byte
	size                                               int64
	pendingCR                                          bool
}

func (s *textStats) add(b []byte) {
	for i := 0; i < len(b); i++ {
		c := b[i]
		if s.pendingCR {
			s.pendingCR = false
			if c == '\n' {
				s.crlf++
				continue
			}
			s.loneCR++
		}
		switch {
		case c == '\r':
			s.pendingCR = true
		case c == '\n':
			s.loneLF++
		case c == 127:
			s.nonprintable++
		case c < 32:
			switch c {
			case '\b', '\t', 033, 014:
				s.printable++
			case 0:
				s.nul++
				s.nonprintable++
			default:
				s.nonprintable++
			}
		default:
			s.printable++
		}
	}
	if len(b) > 0 {
		s.last = b[len(b)-1]
		s.size += int64(len(b))
	}
}

// finish closes a trailing CR and discounts a final ^Z.
func (s *textStats) finish() {
	if s.pendingCR {
		s.pendingCR = false
		s.loneCR++
	}
	if s.size >= 1 && s.last == '\032' {
		s.nonprintable--
	}
}

// binary is Git's convert_is_binary.
func (s *textStats) binary() bool {
	return s.loneCR > 0 || s.nul > 0 || (s.printable>>7) < s.nonprintable
}

// gatherStats streams r through the counters.
func gatherStats(ctx context.Context, r io.Reader) (textStats, error) {
	var s textStats
	buf := make([]byte, 32<<10)
	for {
		if err := ctx.Err(); err != nil {
			return s, err
		}
		n, err := r.Read(buf)
		s.add(buf[:n])
		if err == io.EOF {
			s.finish()
			return s, nil
		}
		if err != nil {
			return s, err
		}
	}
}

// crlfReader drops every CR that is followed by LF.
type crlfReader struct {
	r   *bufio.Reader
	err error
}

func (c *crlfReader) Read(p []byte) (int, error) {
	n := 0
	for n < len(p) {
		b, err := c.r.ReadByte()
		if err != nil {
			if n > 0 {
				return n, nil
			}
			return 0, err
		}
		if b == '\r' {
			next, err := c.r.Peek(1)
			if err == nil && next[0] == '\n' {
				continue
			}
		}
		p[n] = b
		n++
	}
	return n, nil
}

// identReader collapses "$Id:<no '$' or LF>$" into "$Id$" (Git's
// ident_to_git); an unterminated or line-broken one is kept. Memory is
// bounded: a candidate's text is held in memory up to identMemory bytes
// and spooled to a private unlinked temporary file beyond that (Git's
// semantics need the whole candidate before the first byte can be
// decided), and the context is checked while a candidate is scanned.
type identReader struct {
	ctx    context.Context
	r      *bufio.Reader
	tmpDir string
	out    []byte   // pending output
	spool  *os.File // pending verbatim candidate text (after out)
	tail   []byte   // pending output after the spool
	eof    bool
	term   error // fill failed; this Read and every later one return it
	buf    []byte
}

// identMemory bounds the in-memory part of one candidate.
const identMemory = 64 << 10

// createIdentSpool opens the private spill file for one candidate that
// does not fit in memory. Tests replace it to force creation and write
// failures.
var createIdentSpool = os.CreateTemp

func (d *identReader) Read(p []byte) (int, error) {
	if d.term != nil {
		return 0, d.term
	}
	n := 0
	for n < len(p) {
		switch {
		case len(d.out) > 0:
			c := copy(p[n:], d.out)
			d.out = d.out[c:]
			n += c
		case d.spool != nil:
			c, err := d.spool.Read(p[n:])
			n += c
			if err == io.EOF {
				d.spool.Close()
				d.spool, d.out, d.tail = nil, d.tail, nil
			} else if err != nil {
				d.term = err
				return n, err
			}
		case d.eof:
			if n > 0 {
				return n, nil
			}
			return 0, io.EOF
		default:
			if n > 0 && d.r.Buffered() == 0 {
				// Return what is ready rather than block on more input.
				return n, nil
			}
			if err := d.fill(); err != nil {
				// A candidate can fail after earlier bytes were already
				// copied into p. Keep the error: dropping it yields
				// truncated content with err=nil.
				d.term = err
				return n, err
			}
		}
	}
	return n, nil
}

// Close releases a pending spool (a reader abandoned before its end).
func (d *identReader) Close() error {
	if d.spool != nil {
		d.spool.Close()
		d.spool = nil
	}
	return nil
}

// closeReader closes r when it holds resources.
func closeReader(r io.Reader) {
	if c, ok := r.(io.Closer); ok {
		c.Close()
	}
}

// fill moves the next unit of output into d.out (and d.spool, d.tail).
func (d *identReader) fill() error {
	// A run without a dollar passes through unchanged.
	if b, err := d.r.Peek(1); err == nil && b[0] != '$' {
		chunk, _ := d.r.Peek(d.r.Buffered())
		if i := bytes.IndexByte(chunk, '$'); i >= 0 {
			chunk = chunk[:i]
		}
		d.out = append(d.out[:0], chunk...)
		d.r.Discard(len(chunk))
		return nil
	}
	if _, err := d.r.ReadByte(); err == io.EOF { // a dollar, or the end
		d.eof = true
		return nil
	} else if err != nil {
		return err
	}
	head, _ := d.r.Peek(4)
	// C's "len > 3": at least four bytes must follow the dollar.
	if len(head) < 4 || string(head[:3]) != "Id:" {
		d.out = append(d.out[:0], '$')
		return nil
	}
	d.r.Discard(3)
	body := d.buf[:0]
	var spool *os.File
	fail := func(err error) error {
		if spool != nil {
			spool.Close()
		}
		return err
	}
	keep := func(c []byte) error {
		if spool == nil && len(body)+len(c) <= identMemory {
			body = append(body, c...)
			return nil
		}
		if spool == nil {
			f, err := createIdentSpool(d.tmpDir, "callsheet-ident-")
			if err != nil {
				return err
			}
			os.Remove(f.Name())
			spool = f
			if _, err := spool.Write(body); err != nil {
				return err
			}
			body = body[:0]
		}
		_, err := spool.Write(c)
		return err
	}
	verbatim := func(tail []byte) error {
		d.out = append(append(d.out[:0], "$Id:"...), body...)
		d.buf = body[:0]
		if spool != nil {
			if _, err := spool.Seek(0, io.SeekStart); err != nil {
				return fail(err)
			}
			d.spool, d.tail = spool, tail
			return nil
		}
		d.out = append(d.out, tail...)
		return nil
	}
	for {
		if err := d.ctx.Err(); err != nil {
			return fail(err)
		}
		chunk, err := d.r.Peek(max(d.r.Buffered(), 1))
		if len(chunk) == 0 {
			if err == io.EOF {
				// No closing dollar: the text is kept (Git's break).
				return verbatim(nil)
			}
			return fail(err)
		}
		i := bytes.IndexAny(chunk, "$\n")
		if i < 0 {
			if err := keep(chunk); err != nil {
				return fail(err)
			}
			d.r.Discard(len(chunk))
			continue
		}
		if err := keep(chunk[:i]); err != nil {
			return fail(err)
		}
		stop := chunk[i]
		d.r.Discard(i + 1)
		if stop == '$' {
			if spool != nil {
				spool.Close()
			}
			d.buf = body[:0]
			d.out = append(d.out[:0], "$Id$"...)
			return nil
		}
		// A line break before the next dollar: the text is kept and
		// scanning resumes after it (the next dollar starts afresh).
		return verbatim([]byte{'\n'})
	}
}

// cleanReader returns r's clean form: CRLF conversion when convert, then
// ident collapsing when ident.
func cleanReader(ctx context.Context, r io.Reader, convert, ident bool, tmpDir string) io.Reader {
	if convert {
		r = &crlfReader{r: bufio.NewReaderSize(r, 32<<10)}
	}
	if ident {
		r = &identReader{ctx: ctx, r: bufio.NewReaderSize(r, 32<<10), tmpDir: tmpDir}
	}
	return r
}
