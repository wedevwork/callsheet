package devcheck

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

// BenchmarkContainerEvidence measures the host gate over one complete
// iteration: 14 case records, the end record and the verbose events
// (synthetic, catalog-conforming; reported, never gated).
func BenchmarkContainerEvidence(b *testing.B) {
	meta := validMeta(1, 1)
	stream := streamFor(meta)
	exp := containerExpectation(meta)
	b.SetBytes(int64(len(stream)))
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		raws, err := validateContainerStream(bytes.NewReader(stream), exp)
		if err != nil || len(raws) != 15 {
			b.Fatalf("%d records: %v", len(raws), err)
		}
	}
}

// TestContainerEvidenceParsing covers the strict JSON helpers and the
// stream splitter's edge cases.
func TestContainerEvidenceParsing(t *testing.T) {
	for _, bad := range []string{`[]`, `{"a":1}x`, `{"a":1,"a":2}`, `{"a":`, ``, `{1:2}`} {
		if _, err := strictObject([]byte(bad)); err == nil {
			t.Fatalf("%q accepted", bad)
		}
	}
	if _, err := jsonString([]byte(`1`), "f"); err == nil {
		t.Fatal("number as string")
	}
	if _, err := jsonString([]byte(`"\x"`), "f"); err == nil {
		t.Fatal("bad escape")
	}
	if v, err := jsonNullString([]byte(`null`), "f"); v != nil || err != nil {
		t.Fatal("null string")
	}
	if v, err := jsonNullInt([]byte(`7`), "f"); err != nil || *v != 7 {
		t.Fatal("int")
	}
	for _, bad := range []string{`"7"`, `7.5`, `1e3`, ``, `99999999999999999999999`} {
		if _, err := jsonInt([]byte(bad), "f"); err == nil {
			t.Fatalf("int %q accepted", bad)
		}
	}
	if _, err := jsonStringMap([]byte(`{"a":1}`), "m"); err == nil {
		t.Fatal("non-string map value")
	}
	for _, bad := range []string{`{}`, `[1`} {
		if _, err := jsonArray([]byte(bad), "a"); err == nil {
			t.Fatalf("array %q accepted", bad)
		}
	}
	if _, err := parseContainerTask([]byte(`{"label":"x"}`)); err == nil {
		t.Fatal("partial task accepted")
	}
	if _, err := parseContainerTask([]byte(`[]`)); err == nil {
		t.Fatal("non-object task accepted")
	}
	if _, err := parseContainerRecord([]byte(`{"kind":1}`)); err == nil {
		t.Fatal("non-string kind accepted")
	}
	end := `{"adapter":"fake","arch":"amd64","binary_hashes":{},"case_ids":[1],"cleanup_ok":"yes","error":"","iteration":1,"kind":"end","os":"linux",` +
		`"outcome":"pass","run_id":"r","schema_version":1,"source_revision":"x","total":1}`
	if _, err := parseContainerRecord([]byte(end)); err == nil || !strings.Contains(err.Error(), "cleanup_ok must be a boolean") ||
		!strings.Contains(err.Error(), "case_ids[] must be a string") {
		t.Fatalf("end types: %v", err)
	}
	cs := `{"adapter":"fake","arch":"amd64","binary_hashes":{},"boundary":"","case_id":"x","error":"","iteration":1,"kind":"case","os":"linux",` +
		`"outcome":"pass","run_id":"r","schema_version":1,"source_revision":"x","subcases":{},"tasks":[{"x":1}],"total":1}`
	if _, err := parseContainerRecord([]byte(cs)); err == nil || !strings.Contains(err.Error(), "tasks[0]") {
		t.Fatalf("task error: %v", err)
	}
	// An oversized record is a problem, never skipped or truncated.
	p := &evidenceProblems{}
	scanContainerStream(strings.NewReader(ContainerEvidencePrefix+"{"+strings.Repeat(" ", containerMaxRecord)+"}\n"), p)
	if err := p.err(); err == nil || !strings.Contains(err.Error(), "exceeds") {
		t.Fatalf("oversized: %v", err)
	}
	// A reader error is reported.
	p = &evidenceProblems{}
	scanContainerStream(errReader{}, p)
	if err := p.err(); err == nil || !strings.Contains(err.Error(), "reading the container output") {
		t.Fatalf("reader error: %v", err)
	}
	// The problem list is bounded.
	p = &evidenceProblems{}
	for i := 0; i < 100; i++ {
		p.add("x")
	}
	if len(p.list) != 64 {
		t.Fatalf("%d problems kept", len(p.list))
	}
	// The splitter keeps evidence and event lines and an unterminated
	// record tail, and sends everything to the diagnostics.
	var diag, echo bytes.Buffer
	s := &lineSplitter{diag: &diag, echo: &echo}
	s.Write([]byte("noise\n=== RUN   TestX\n--- PASS: TestX (0.00s)\nPA"))
	s.Write([]byte("SS\n" + ContainerEvidencePrefix + "{\"a\":1}\n" + ContainerEvidencePrefix + "{\"cut"))
	got := string(s.stream())
	if got != "=== RUN   TestX\n--- PASS: TestX (0.00s)\nPASS\n"+ContainerEvidencePrefix+"{\"a\":1}\n"+ContainerEvidencePrefix+"{\"cut" ||
		!strings.Contains(diag.String(), "noise") || strings.Contains(echo.String(), ContainerEvidencePrefix) || !strings.Contains(echo.String(), "--- PASS") {
		t.Fatalf("kept %q echo %q", got, echo.String())
	}
	// Sanitized text keeps newlines and tabs and escapes other controls.
	if got := sanitize("a\x1b[1m\tb\n\x7f"); got != "a\\x1b[1m\tb\n\\x7f" {
		t.Fatalf("sanitize = %q", got)
	}
}

type errReader struct{}

func (errReader) Read([]byte) (int, error) { return 0, errors.New("broken pipe") }

// TestContainerEvidenceDirectory: the selected directory is created under
// its existing parent, locked through a sibling lock before anything is
// written, initialized "not started", and refused when its final
// component, the lock or an output file is a symlink; symlinked ancestors
// are allowed; distinct directories never contend, the same directory
// does.
func TestContainerEvidenceDirectory(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "evidence")
	rep, err := openEvidence(dir, "test", 1, time.Unix(0, 0))
	if err != nil {
		t.Fatal(err)
	}
	report, _ := os.ReadFile(filepath.Join(dir, evidenceReportFile))
	if !strings.Contains(string(report), "outcome: not started\n") || !strings.Contains(string(report), "container: not started\n") ||
		!strings.Contains(string(report), "run_id: "+rep.RunID+"\n") || len(rep.RunID) != 32 {
		t.Fatalf("initial report:\n%s", report)
	}
	for _, f := range []string{evidenceLedgerFile, evidenceRuntimeFile} {
		if st, err := os.Lstat(filepath.Join(dir, f)); err != nil || st.Size() != 0 {
			t.Fatalf("%s not initialized empty: %v", f, err)
		}
	}
	// Same directory: the second writer fails before replacing anything;
	// a distinct directory proceeds concurrently.
	if _, err := openEvidence(dir, "test", 1, time.Now()); err == nil || !strings.Contains(err.Error(), "in use by another devcheck run") {
		t.Fatalf("same-directory writer: %v", err)
	}
	after, _ := os.ReadFile(filepath.Join(dir, evidenceReportFile))
	if string(after) != string(report) {
		t.Fatal("a refused writer replaced the report")
	}
	other, err := openEvidence(filepath.Join(root, "other"), "test", 1, time.Now())
	if err != nil {
		t.Fatalf("distinct directory: %v", err)
	}
	other.close()
	// Phases, failures, the ledger and diagnostics reach the report; close
	// releases the lock (the lock file stays) and is idempotent.
	fmtWrite(rep.diag, "diagnostic line\n")
	rep.phase("phase one")
	rep.appendLedger([][]byte{[]byte(`{"x":1}`)})
	rep.fail(errors.New("boom\x01"))
	if err := rep.close(); err != nil || rep.close() != nil {
		t.Fatal(err)
	}
	final, _ := os.ReadFile(filepath.Join(dir, evidenceReportFile))
	for _, want := range []string{"outcome: fail\n", "  - phase one\n", "  boom\\x01\n", "ledger: 1 records\n{\"x\":1}\n", "  | diagnostic line\n", "truncated: false"} {
		if !strings.Contains(string(final), want) {
			t.Fatalf("final report lacks %q:\n%s", want, final)
		}
	}
	if b, _ := os.ReadFile(filepath.Join(dir, evidenceLedgerFile)); string(b) != "{\"x\":1}\n" {
		t.Fatalf("ledger %q", b)
	}
	if _, err := os.Lstat(dir + ".lock"); err != nil {
		t.Fatal("the lock file was removed")
	}
	again, err := openEvidence(dir, "container-e2e", 2, time.Now())
	if err != nil {
		t.Fatalf("reopen after release: %v", err)
	}
	again.close()
	// Concurrent writers on one directory: exactly one wins at a time.
	var wg sync.WaitGroup
	var mu sync.Mutex
	wins, losses := 0, 0
	held := make(chan struct{})
	first, err := openEvidence(dir, "test", 1, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 4; i++ {
		wg.Go(func() {
			<-held
			r, err := openEvidence(dir, "test", 1, time.Now())
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				losses++
				return
			}
			wins++
			r.close()
		})
	}
	close(held)
	wg.Wait()
	first.close()
	if wins != 0 || losses != 4 {
		t.Fatalf("while held: %d wins, %d losses", wins, losses)
	}
	// Symlinks: the final component, the lock and each output file.
	target := t.TempDir()
	link := filepath.Join(root, "link")
	os.Symlink(target, link)
	if _, err := openEvidence(link, "test", 1, time.Now()); err == nil || !strings.Contains(err.Error(), "is a symlink") {
		t.Fatalf("symlinked directory: %v", err)
	}
	lockLink := filepath.Join(root, "locked")
	os.Symlink(filepath.Join(target, "x"), lockLink+".lock")
	if _, err := openEvidence(lockLink, "test", 1, time.Now()); err == nil || !strings.Contains(err.Error(), "is a symlink") {
		t.Fatalf("symlinked lock: %v", err)
	}
	for _, f := range []string{evidenceReportFile, evidenceLedgerFile, evidenceRuntimeFile} {
		d := filepath.Join(root, "out-"+f)
		os.Mkdir(d, 0o755)
		os.Symlink(filepath.Join(target, f), filepath.Join(d, f))
		if _, err := openEvidence(d, "test", 1, time.Now()); err == nil || !strings.Contains(err.Error(), "is a symlink") {
			t.Fatalf("symlinked %s: %v", f, err)
		}
		if _, err := os.Lstat(filepath.Join(target, f)); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("a symlinked %s was followed", f)
		}
	}
	// Non-directory final component and non-regular lock.
	plain := filepath.Join(root, "plain")
	os.WriteFile(plain, nil, 0o644)
	if _, err := openEvidence(plain, "test", 1, time.Now()); err == nil || !strings.Contains(err.Error(), "not a directory") {
		t.Fatalf("file as directory: %v", err)
	}
	os.Mkdir(filepath.Join(root, "dirlock.lock"), 0o755)
	if _, err := openEvidence(filepath.Join(root, "dirlock"), "test", 1, time.Now()); err == nil || !strings.Contains(err.Error(), "not a regular file") {
		t.Fatalf("directory as lock: %v", err)
	}
	// A missing parent fails (the lock cannot be created).
	if _, err := openEvidence(filepath.Join(root, "missing", "evidence"), "test", 1, time.Now()); err == nil {
		t.Fatal("missing parent accepted")
	}
	// Symlinked ancestors are allowed.
	anc := filepath.Join(root, "ancestor")
	os.Symlink(target, anc)
	ok, err := openEvidence(filepath.Join(anc, "evidence"), "test", 1, time.Now())
	if err != nil {
		t.Fatalf("symlinked ancestor: %v", err)
	}
	ok.close()
	if _, err := os.Stat(filepath.Join(target, "evidence", evidenceReportFile)); err != nil {
		t.Fatal(err)
	}
	// An unwritable directory fails initialization (not for root).
	if os.Geteuid() != 0 {
		ro := filepath.Join(root, "ro")
		os.Mkdir(ro, 0o555)
		if _, err := openEvidence(ro, "test", 1, time.Now()); err == nil {
			t.Fatal("unwritable directory accepted")
		}
		os.Chmod(ro, 0o755)
	}
	// The bounded diagnostics keep at most the limit and count the rest.
	var sink bytes.Buffer
	l := newBoundedLog(&sink, 10)
	l.Write([]byte("0123456789abc"))
	l.Write([]byte("def"))
	if written, dropped, tail, err := l.state(); written != 10 || dropped != 6 || sink.String() != "0123456789" || !strings.HasSuffix(tail, "def") || err != nil {
		t.Fatalf("bounded %d %d %q %v", written, dropped, sink.String(), err)
	}
	failing := newBoundedLog(failWriter{}, 100)
	failing.Write([]byte("x"))
	if _, _, _, err := failing.state(); !errors.Is(err, syscall.EIO) {
		t.Fatalf("write error %v", err)
	}
	if !strings.Contains((&evidenceReport{diag: failing, outcome: "x"}).render(), "runtime_log_error") {
		t.Fatal("write error not reported")
	}
	if err := validateRunOptions(RunOptions{EvidenceDir: "/abs"}); err != nil {
		t.Fatal(err)
	}
}

type failWriter struct{}

func (failWriter) Write([]byte) (int, error) { return 0, syscall.EIO }

func fmtWrite(l *boundedLog, s string) { l.Write([]byte(s)) }
