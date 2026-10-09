package mcpqual

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/wedevwork/callsheet/internal/testkit"
)

// sizedFixture renders decoder's success transcript padded with ignored
// events to just under size bytes, as the harness reads it (readLines).
func sizedFixture(b *testing.B, decoder string, size int) (Transcript, []byte) {
	b.Helper()
	render := func(pad int) []byte {
		return []byte(strings.Join(fixture(decoder, scSuccess, "case-1", "nonce-1", pad), "\n") + "\n")
	}
	base, step := len(render(0)), len(render(100))-len(render(0))
	pad := 0
	if size > base {
		pad = (size - base) * 100 / step
	}
	raw := render(pad)
	for len(raw) > size && pad > 0 {
		pad -= 1 + (len(raw)-size)*100/step
		pad = max(pad, 0)
		raw = render(pad)
	}
	t := readLines(bytes.NewReader(raw), MaxEvidenceFileBytes, func() int64 { return 1 })
	if t.Truncated || len(raw) > size || len(raw) < size/2 {
		b.Fatalf("%s fixture of %d bytes for %d (truncated %v)", decoder, len(raw), size, t.Truncated)
	}
	return t, raw
}

// BenchmarkMCPQualificationTranscript decodes each vendor's fixture at
// 1 KiB and at the 8 MiB per-file limit. The normalized events must be
// exactly the fixture's correlated call and result (the padding is model
// prose mentioning timeouts, never classified), and an oversized
// transcript must end truncated and unqualified.
func BenchmarkMCPQualificationTranscript(b *testing.B) {
	reg := DefaultRegistry()
	for _, name := range decoderNames {
		dec := reg[name].decode
		for _, size := range []int{1 << 10, MaxEvidenceFileBytes} {
			t, raw := sizedFixture(b, name, size)
			b.Run(fmt.Sprintf("%s/%dB", name, size), func(b *testing.B) {
				b.SetBytes(int64(len(raw)))
				b.ReportAllocs()
				var d Decoded
				for i := 0; i < b.N; i++ {
					d = dec(t)
				}
				b.StopTimer()
				if d.Inconclusive != "" || !d.Terminal || len(d.Events) != 2 || d.Events[0].Kind != KindToolCall || d.Events[1].Kind != KindToolResult ||
					d.Events[0].CaseID != "case-1" || d.Events[1].CaseID != "case-1" || d.Events[1].Nonce != "nonce-1" || d.Events[0].RequestID != d.Events[1].RequestID {
					b.Fatalf("%s: decoded %+v", name, d.Events)
				}
			})
		}
		over := append(bytes.Repeat([]byte(" "), MaxEvidenceFileBytes), '\n')
		t := readLines(bytes.NewReader(append(over, []byte(strings.Join(fixture(name, scTimeout, "case-1", "n", 0), "\n")+"\n")...)), MaxEvidenceFileBytes, func() int64 { return 0 })
		if d := dec(t); !t.Truncated || d.Inconclusive != ReasonTruncated {
			b.Fatalf("%s: an oversized transcript classified: %+v", name, d)
		}
	}
	benchRealFixtures(b)
}

// benchRealFixtures (design decoder-enrollment B2, and B3 for Cursor)
// measures the four enrolled real fixtures: decode CPU over the in-memory transcript through
// the exact-version decoder, separately from the replay and validation
// I/O (the bundle read from disk with its oracle and receipt, then Replay),
// and the production ValidateEnrollment of the checked-in index. Each
// asserts the exact oracle and no typed error.
func benchRealFixtures(b *testing.B) {
	root := testkit.MustRepoRoot(b)
	ib, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(EnrollmentIndexPath)))
	if err != nil {
		b.Fatal(err)
	}
	idx, err := ParseEnrollmentIndex(ib)
	if err != nil || len(idx.Entries) != 4 {
		b.Fatalf("production index: %v (%d entries)", err, len(idx.Entries))
	}
	reg := DefaultRegistry()
	for _, e := range idx.Entries {
		dir := filepath.Join(root, filepath.FromSlash(e.Bundle))
		raw, err1 := os.ReadFile(filepath.Join(dir, filepath.FromSlash(ClientFile(e.Client, FileVendorEvents))))
		ob, err2 := os.ReadFile(filepath.Join(dir, ExpectedName))
		o, err3 := ParseExpected(ob)
		tr, err4 := ReplayTranscript(raw)
		dec, _, err5 := reg.Select(e.Decoder, e.Version)
		if err := errors.Join(err1, err2, err3, err4, err5); err != nil {
			b.Fatal(err)
		}
		want, _ := encodeJSON(o.Events)
		b.Run("real/"+e.Client+"/decode", func(b *testing.B) {
			b.ReportAllocs()
			b.SetBytes(int64(tr.Size()))
			var d Decoded
			for i := 0; i < b.N; i++ {
				d = dec(tr)
			}
			b.StopTimer()
			b.ReportMetric(float64(tr.Size()), "bytes/op")
			got, _ := encodeJSON(d.Events)
			if !d.Terminal || d.Inconclusive != "" || !bytes.Equal(got, want) {
				b.Fatalf("%s: decoded %s, the oracle says %s", e.Client, got, want)
			}
		})
		b.Run("real/"+e.Client+"/replay-io", func(b *testing.B) {
			b.ReportAllocs()
			var bundle *CaptureBundle
			var err error
			for i := 0; i < b.N; i++ {
				if bundle, err = ValidateCaptureBundle(os.DirFS(dir), ".", ExpectedName, SanitizationName); err == nil {
					err = Replay(reg, e, bundle, o)
				}
			}
			b.StopTimer()
			if err != nil {
				b.Fatal(err)
			}
			n := len(bundle.ManifestBytes)
			for _, f := range bundle.Files {
				n += len(f)
			}
			b.SetBytes(int64(n))
			b.ReportMetric(float64(n), "bytes/op")
		})
	}
	b.Run("real/validate-enrollment-io", func(b *testing.B) {
		b.ReportAllocs()
		var got *EnrollmentIndex
		var err error
		for i := 0; i < b.N; i++ {
			got, err = ValidateEnrollment(EnrollmentOptions{Root: root, Registry: reg})
		}
		b.StopTimer()
		if err != nil || len(got.Entries) != 4 {
			b.Fatalf("production enrollment: %v", err)
		}
		b.ReportMetric(float64(len(ib)), "bytes/op")
	})
}

// BenchmarkMCPQualificationProbe measures small-frame throughput of one
// probe session on a fake clock: each operation is a slow call with a
// progress token, one progress notification and the result. Every
// progress and result must carry its request's token, nonce and case, and
// no call or timer may be retained at the end.
func BenchmarkMCPQualificationProbe(b *testing.B) {
	cf := CaseFile{Version: 1, RunID: "bench", Nonce: "nonce-b", Cases: []ProbeCase{{CaseID: "tick", DelayMS: 2, ProgressIntervalMS: 1}}}
	inR, inW := io.Pipe()
	outR, outW := io.Pipe()
	clock := testkit.NewFakeClock(epoch)
	var events bytes.Buffer
	done := make(chan error, 1)
	go func() {
		done <- Serve(ProbeConfig{Cases: cf, In: inR, Out: outW, Events: &events, Clock: clock, Version: "bench"})
		outW.Close()
	}()
	lines := bufio.NewReader(outR)
	send := func(s string) {
		if _, err := io.WriteString(inW, s+"\n"); err != nil {
			b.Fatal(err)
		}
	}
	read := func() string {
		l, err := lines.ReadString('\n')
		if err != nil {
			b.Fatal(err)
		}
		return l
	}
	send(`{"jsonrpc":"2.0","id":0,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"bench","version":"1"}}}`)
	read()
	send(`{"jsonrpc":"2.0","method":"notifications/initialized"}`)
	armed := func(ws []testkit.Waiter) bool {
		return testkit.HasTimer(2*time.Millisecond)(ws) && testkit.HasTicker(time.Millisecond)(ws)
	}
	var frames int64
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		id := fmt.Sprint(i + 1)
		call := `{"jsonrpc":"2.0","id":` + id + `,"method":"tools/call","params":{"name":"slow","arguments":{"case_id":"tick"},"_meta":{"progressToken":"t` + id + `"}}}`
		send(call)
		if err := clock.AwaitWaiter(testWait, armed); err != nil {
			b.Fatal(err)
		}
		clock.Advance(time.Millisecond)
		prog := read()
		clock.Advance(time.Millisecond)
		res := read()
		if !strings.Contains(prog, `"progress":1,"progressToken":"t`+id+`"`) || !strings.HasPrefix(res, `{"jsonrpc":"2.0","id":`+id+`,"result"`) ||
			!strings.Contains(res, `\"case_id\":\"tick\",\"nonce\":\"nonce-b\"`) {
			b.Fatalf("op %s: %q %q", id, prog, res)
		}
		frames += int64(len(call) + len(prog) + len(res) + 1)
	}
	b.StopTimer()
	b.SetBytes(frames / int64(b.N))
	inW.Close()
	if err := <-done; err != nil {
		b.Fatal(err)
	}
	if ws := clock.Waiters(); len(ws) != 0 {
		b.Fatalf("retained timers %v", ws)
	}
	evs, intact, err := ParseProbeEvents(events.Bytes())
	if err != nil || !intact || strings.Count(kinds(evs), "receipt") != b.N || strings.Count(kinds(evs), "completed") != b.N || strings.Contains(kinds(evs), "eof,") && evs[len(evs)-2].CaseID != "" {
		b.Fatalf("events %v intact=%v: %s", err, intact, kinds(evs)[:min(200, len(kinds(evs)))])
	}
	// The initialization evidence records the negotiation (design
	// decoder-enrollment B1, FP-10).
	if init := evs[1]; init.Kind != EvInitialize || init.RequestedProtocolVersion == nil || *init.RequestedProtocolVersion != ProtocolVersion || *init.SelectedProtocolVersion != ProtocolVersion {
		b.Fatalf("initialize event %+v", init)
	}
	// The shared analyzer's terminal state on the same events (design
	// decoder-enrollment B1.5, FP-14), untimed: the whole log is intact (an
	// extra receipt per further operation keeps it incomplete beyond one),
	// and its prefix through the first completion, LF-terminated, is the
	// clean terminal observation without exit.
	raw := events.Bytes()
	whole := analyzeProbe(raw, "tick", false, true).obs
	firstDone := bytes.Index(raw, []byte(`"kind":"completed"`))
	prefix := raw[:firstDone+bytes.IndexByte(raw[firstDone:], '\n')+1]
	cut := analyzeProbe(prefix, "tick", false, true).obs
	if !whole.Intact || (b.N == 1) != (whole.EndState == EndIntact) || cut.EndState != EndTerminalWithoutExit || cut.Intact || *cut.TerminalKind != EvCompleted {
		b.Fatalf("analyzer: whole %+v, prefix %+v", whole, cut)
	}
}

// benchTranscript is a transcript of JSON lines, one in sixteen with a
// credential member and a known literal, whose sanitized wrapped evidence
// file holds at most fileLimit bytes (the largest payload a bundle keeps
// at that bound) unless over is set, when one more line crosses it.
func benchTranscript(red *Redactor, fileLimit int, over bool) Transcript {
	var t Transcript
	size := 0
	for i := 0; ; i++ {
		data := fmt.Sprintf(`{"type":"assistant","n":%d,"text":"%s"}`, i, strings.Repeat("prose ", 12))
		if i%16 == 0 {
			data = fmt.Sprintf(`{"type":"note","n":%d,"api_key":"bench-secret-value","path":"/home/bench/x"}`, i)
		}
		clean, _ := red.TranscriptLine([]byte(data))
		n := len(wrapLine(int64(i), clean)) + 1
		if size+n > fileLimit {
			if over {
				t.Lines = append(t.Lines, Line{OffsetNS: int64(i), Data: []byte(data)})
			}
			return t
		}
		size += n
		t.Lines = append(t.Lines, Line{OffsetNS: int64(i), Data: []byte(data)})
	}
}

// BenchmarkMCPCaptureEvidence (design decoder-enrollment) measures capture
// evidence at 1 KiB and at the production 8 MiB file bound: sanitization
// and wrapper serialization with hashing, and bundle validation from disk
// (file reads included, so I/O is visible). It asserts the stored bound,
// deterministic sanitized hashes and the secret's absence; the over-limit
// case must be cut and its bundle rejected. Only this benchmark uses the
// production 8 MiB capture limits.
func BenchmarkMCPCaptureEvidence(b *testing.B) {
	red := NewCaptureRedactor([]string{"bench-secret-value"}, map[string]string{"/home/bench": "<home>"})
	for _, size := range []int{1 << 10, MaxEvidenceFileBytes} {
		tr := benchTranscript(red, size, false)
		var first string
		b.Run(fmt.Sprintf("sanitize/%dB", size), func(b *testing.B) {
			b.ReportAllocs()
			var out []byte
			var hash string
			for i := 0; i < b.N; i++ {
				out, _ = transcriptLines(red, tr)
				hash = sha256Hex(out)
			}
			b.StopTimer()
			b.SetBytes(int64(len(out)))
			b.ReportMetric(float64(len(out)), "bytes/op")
			if first == "" {
				first = hash
			}
			w := &captureWriter{root: b.TempDir(), lim: DefaultCaptureLimits(), perClient: map[string]int{}}
			ref, cut, err := w.put("claude", ClientFile("claude", FileVendorEvents), out, true)
			switch {
			case err != nil || cut || ref.Bytes > int64(size) || ref.Bytes < int64(size)/2:
				b.Fatalf("stored %+v cut %v: %v", ref, cut, err)
			case hash != first || ref.SHA256 != hash:
				b.Fatal("the sanitized hash is not deterministic")
			case bytes.Contains(out, []byte("bench-secret-value")) || bytes.Contains(out, []byte("/home/bench")):
				b.Fatal("a secret survived sanitization")
			}
		})
		// A one-client bundle around that payload, validated from disk.
		dir := benchBundle(b, red, tr)
		b.Run(fmt.Sprintf("validate/%dB", size), func(b *testing.B) {
			b.ReportAllocs()
			var bundle *CaptureBundle
			var err error
			for i := 0; i < b.N; i++ {
				bundle, err = ValidateCaptureBundle(os.DirFS(dir), ".")
			}
			b.StopTimer()
			if err != nil {
				b.Fatal(err)
			}
			// The validated bundle's replayed observation (design
			// decoder-enrollment B1.5, FP-14): intact, completed, clean.
			if o := bundle.Manifest.Clients[0].Probe.Observation; len(tr.Lines) > 1 && (o == nil || o.EndState != EndIntact || *o.TerminalKind != EvCompleted || !o.CleanSession) {
				b.Fatalf("bundle observation %+v", o)
			}
			n := 0
			for _, f := range bundle.Files {
				n += len(f)
			}
			b.SetBytes(int64(n))
			b.ReportMetric(float64(n), "bytes/op")
		})
	}
	// Design decoder-enrollment B1: the Cursor approval's bounded
	// before/after fingerprint over a fixed small tree (1 KiB of regular
	// files in eight files), from disk, with the comparison; the change
	// inventory is exact and deterministic and exports no fingerprint or
	// file content.
	b.Run("fingerprint/1024B", func(b *testing.B) {
		before, after := filepath.Join(b.TempDir(), "before"), filepath.Join(b.TempDir(), "after")
		for _, root := range []string{before, after} {
			for i := 0; i < 8; i++ {
				p := filepath.Join(root, ".cursor", fmt.Sprintf("state-%d.json", i))
				os.MkdirAll(filepath.Dir(p), 0o700)
				body := fmt.Sprintf(`{"n":%d,"token":"bench-fingerprint-secret"}`, i)
				os.WriteFile(p, []byte(body+strings.Repeat(" ", 128-len(body)-1)+"\n"), 0o600)
			}
		}
		os.WriteFile(filepath.Join(after, ".cursor", "state-3.json"), bytes.Repeat([]byte("x"), 127), 0o600)
		os.WriteFile(filepath.Join(after, ".cursor", "approved.json"), []byte(`["probe"]`), 0o600)
		sc, err := newApprovalScanner(nil, ApprovalScanLimits{})
		if err != nil {
			b.Fatal(err)
		}
		want := []CaptureApprovalChange{{"<workspace>/.cursor/approved.json", ChangeAdded}, {"<workspace>/.cursor/state-3.json", ChangeModified}}
		b.ReportAllocs()
		b.SetBytes(1024)
		var got []approvalChange
		var pre *snapshot
		for i := 0; i < b.N; i++ {
			pre = sc.snapshot([]scanRoot{{label: labelWorkspace, path: before}})
			got = diffSnapshots(pre, sc.snapshot([]scanRoot{{label: labelWorkspace, path: after}}))
		}
		b.StopTimer()
		var changes []CaptureApprovalChange
		for _, c := range got {
			changes = append(changes, c.CaptureApprovalChange)
		}
		raw, _ := json.Marshal(changes)
		switch {
		case !pre.complete || pre.bytes != 1024 || pre.count != 10:
			b.Fatalf("the snapshot %+v", pre)
		case !slices.Equal(changes, want):
			b.Fatalf("changes %+v", changes)
		case bytes.Contains(raw, []byte("bench-fingerprint-secret")) || bytes.Contains(raw, []byte("digest")) || bytes.Contains(raw, []byte(before)):
			b.Fatalf("the change inventory exports data: %s", raw)
		}
		// The two-directory inventory policy (design decoder-enrollment B1.5,
		// FP-16), untimed, on a tiny fixed home fixture inside the
		// benchmark's own directory (never a real home): the excluded
		// directories' contents change without a recorded change while their
		// boundaries and a same-name directory elsewhere are inventoried.
		home := filepath.Join(b.TempDir(), "home")
		for _, d := range []string{"chats", "ai-tracking", "projects/p/chats"} {
			os.MkdirAll(filepath.Join(home, ".cursor", filepath.FromSlash(d)), 0o700)
			os.WriteFile(filepath.Join(home, ".cursor", filepath.FromSlash(d), "old.json"), []byte("{}"), 0o600)
		}
		root := []scanRoot{{label: labelHomeCursor, path: filepath.Join(home, ".cursor")}}
		sc.exclude(home)
		pre = sc.snapshot(root)
		for _, d := range []string{"chats", "ai-tracking"} {
			os.WriteFile(filepath.Join(home, ".cursor", d, "new.json"), []byte("{}"), 0o600)
		}
		if got := diffSnapshots(pre, sc.snapshot(root)); !pre.complete || pre.count != 7 || len(got) != 0 {
			b.Fatalf("the inventory policy: %d entries, changes %+v", pre.count, got)
		}
	})
	// Design decoder-enrollment B2 (FP-18): the metadata export's pure
	// transcript step over a small deterministic real-format transcript
	// (about 1 KiB, every allowed Codex metadata shape), and the protected
	// span check alone; both assert determinism, the replacement count, the
	// unchanged evidence spans and the masked fingerprint equality.
	export := []byte(strings.Join([]string{
		string(wrapLine(1, []byte(`{"type":"thread.started","thread_id":"bench-thread"}`))),
		string(wrapLine(2, []byte(`{"type":"item.completed","item":{"id":"item_0","type":"error","message":"hook diagnostic `+strings.Repeat("d", 200)+`"}}`))),
		string(wrapLine(3, []byte(`{"type":"item.started","item":{"id":"item_3","type":"mcp_tool_call","server":"probe","tool":"slow","arguments":{"case_id":"c"},"result":null,"error":null,"status":"in_progress"}}`))),
		string(wrapLine(4, []byte(`{"type":"item.completed","item":{"id":"item_3","type":"mcp_tool_call","server":"probe","tool":"slow","arguments":{"case_id":"c"},"result":{"content":[{"type":"text","text":"{\"case_id\":\"c\",\"nonce\":\"n\"}"}],"structured_content":null},"error":null,"status":"completed"}}`))),
		string(wrapLine(5, []byte(`{"type":"item.completed","item":{"id":"item_4","type":"agent_message","text":"`+strings.Repeat("prose ", 40)+`"}}`))),
		string(wrapLine(6, []byte(`{"type":"turn.completed","usage":{"input_tokens":1}}`))),
	}, "\n") + "\n")
	b.Run(fmt.Sprintf("export/%dB", len(export)), func(b *testing.B) {
		b.ReportAllocs()
		b.SetBytes(int64(len(export)))
		var te *transcriptExport
		var err error
		for i := 0; i < b.N; i++ {
			te, err = exportTranscript("codex", ClientFile("codex", FileVendorEvents), export)
		}
		b.StopTimer()
		b.ReportMetric(float64(len(export)), "bytes/op")
		again, err2 := exportTranscript("codex", ClientFile("codex", FileVendorEvents), export)
		check, err3 := exportedReplacements("codex", ClientFile("codex", FileVendorEvents), te.data)
		switch {
		case errors.Join(err, err2, err3) != nil:
			b.Fatal(errors.Join(err, err2, err3))
		case !bytes.Equal(te.data, again.data) || len(te.reps) != 3 || !bytes.Equal(te.masked, check.masked) || len(check.reps) != 3:
			b.Fatalf("export: %d replacements, deterministic %v", len(te.reps), bytes.Equal(te.data, again.data))
		case bytes.Contains(te.data, []byte("bench-thread")) || !bytes.Contains(te.data, []byte(`\\\"nonce\\\":\\\"n\\\"`)):
			b.Fatal("the export kept metadata or lost evidence")
		}
	})
	b.Run("protected-span", func(b *testing.B) {
		data := []byte(`[{"type":"system","subtype":"init","cwd":"/x","session_id":"s","tools":["a","b"],"memory_paths":{"auto":"m"}},` +
			`{"type":"assistant","message":{"id":"m1","content":[{"type":"text","text":"t"},{"type":"tool_use","id":"toolu_1","name":"mcp__probe__slow","input":{"case_id":"c"}}]},"session_id":"s"},` +
			`{"type":"result","subtype":"success","is_error":false,"terminal_reason":"completed","result":"r","session_id":"s","total_cost_usd":0.12,` +
			`"modelUsage":{"claude-sonnet-5-5":{"inputTokens":6,"costUSD":0.12}}}]`)
		b.ReportAllocs()
		b.SetBytes(int64(len(data)))
		var masked []byte
		var edits []fixtureEdit
		var prot []*jspan
		for i := 0; i < b.N; i++ {
			_, edits, prot, _ = recordEdits("claude", data)
			masked = spliceEdits(data, edits, func(fixtureEdit) string { return protectedMaskToken })
		}
		b.StopTimer()
		b.ReportMetric(float64(len(data)), "bytes/op")
		exp := spliceEdits(data, edits, func(e fixtureEdit) string { return replacementLiteral[e.want] })
		_, e2, p2, err := recordEdits("claude", exp)
		// cwd, session_id, tools, memory_paths; session_id, message.id, the
		// text block; result, session_id and (amendment A1) total_cost_usd
		// and modelUsage.<model>.costUSD.
		if err != nil || len(edits) != 11 || !bytes.Equal(masked, spliceEdits(exp, e2, func(fixtureEdit) string { return protectedMaskToken })) ||
			!slices.EqualFunc(protectedBytes(data, prot), protectedBytes(exp, p2), bytes.Equal) {
			b.Fatalf("protected spans: %d edits, %v", len(edits), err)
		}
	})
	// Design decoder-enrollment B3 (FP-23/FP-25): the Cursor policy's
	// transcript step over the small fabricated Cursor stream (every row,
	// the opaque newline-bearing call ID), asserting determinism, the row
	// count, the protected IDs and the masked fingerprint; and the in-memory
	// residue check of one attributed socket (an injected tiny tree, never a
	// real home or socket), asserting the admitted normalized entry.
	var cursorExport []byte
	for i, r := range cursorExportRecords("bench-meta", `{\"case_id\":\"c\",\"nonce\":\"n\"}`) {
		cursorExport = append(append(cursorExport, wrapLine(int64(i+1), []byte(r))...), '\n')
	}
	b.Run(fmt.Sprintf("export-cursor/%dB", len(cursorExport)), func(b *testing.B) {
		vendor := ClientFile("cursor", FileVendorEvents)
		b.ReportAllocs()
		b.SetBytes(int64(len(cursorExport)))
		var te *transcriptExport
		var err error
		for i := 0; i < b.N; i++ {
			te, err = exportTranscript("cursor", vendor, cursorExport)
		}
		b.StopTimer()
		b.ReportMetric(float64(len(cursorExport)), "bytes/op")
		again, err2 := exportTranscript("cursor", vendor, cursorExport)
		check, err3 := exportedReplacements("cursor", vendor, te.data)
		rows := 0
		for _, ps := range cursorRowPointers {
			rows += len(ps)
		}
		switch {
		case errors.Join(err, err2, err3) != nil:
			b.Fatal(errors.Join(err, err2, err3))
		case !bytes.Equal(te.data, again.data) || len(te.reps) != rows || !bytes.Equal(te.masked, check.masked) || len(check.reps) != rows:
			b.Fatalf("cursor export: %d replacements of %d", len(te.reps), rows)
		case bytes.Contains(te.data, []byte("bench-meta")) || bytes.Count(te.data, []byte(`call-x-0\\nfc_x_0`)) != 6:
			b.Fatal("the Cursor export kept metadata or lost a call ID")
		}
	})
	// Design decoder-enrollment A3: tiny in-memory exact-tree and ledger
	// vectors (hashed, in_place, and a three-case ledger revalidated by the
	// later cases), reporting the bytes inspected per operation: approval
	// bytes only, never a session file's content. A3.1 adds the foreign
	// baseline's creation and its revalidation across three cases over
	// pre-existing foreign session trees.
	for _, layout := range []string{"hashed", "in-place", "ledger", "foreign-baseline", "foreign-ledger"} {
		b.Run("residue-check-"+layout, func(b *testing.B) {
			b.ReportAllocs()
			var w WorkerResidue
			var s *rsSpyFS
			for i := 0; i < b.N; i++ {
				m := rsWorld()
				s = rsSpy(m)
				p := rsPreparer(s, ApprovalScanLimits{})
				switch layout {
				case "hashed":
					w = session(b, p, "cursor-setup", rsWS1, nil, func() { socketAt(m, "c-out-work-cu-1b81996", nil) })
				case "in-place":
					w = session(b, p, "cursor-setup", rsWS1, rsEnable(b, m, p, rsWS1), func() { rsInPlace(m, rsWS1) })
				case "foreign-baseline":
					rsForeign(m)
					if err := p.preflight(nil, rsSlug(rsWS1)); err != nil {
						b.Fatal(err)
					}
					w = session(b, p, "cursor-setup", rsWS1, nil, func() { socketAt(m, "c-out-work-cu-1b81996", nil) })
				case "foreign-ledger":
					rsForeign(m)
					rsCase(b, m, p, "cursor-setup", rsWS1, "in_place")
					rsCase(b, m, p, "cursor-default-1", rsWS2, "hashed")
					w = rsCase(b, m, p, "cursor-default-2", rsWS3, "in_place")
				default:
					session(b, p, "cursor-setup", rsWS1, rsEnable(b, m, p, rsWS1), func() { rsInPlace(m, rsWS1) })
					session(b, p, "cursor-default-1", rsWS2, rsEnable(b, m, p, rsWS2), func() { socketAt(m, "c-out-work-cursor-defa-0a1b2c3", nil) })
					w = session(b, p, "cursor-default-2", rsWS3, rsEnable(b, m, p, rsWS3), func() { rsInPlace(m, rsWS3) })
				}
			}
			b.StopTimer()
			var approval int
			for path, n := range s.opens {
				if filepath.Base(path) == cursorApprovalsFile {
					approval += n * len(rsApproval)
				}
			}
			b.ReportMetric(float64(approval), "approval-bytes/op")
			b.ReportMetric(float64(s.sessionOpens()), "session-opens/op")
			want := map[string]int{"hashed": 1, "in-place": 1, "ledger": 3, "foreign-baseline": 1, "foreign-ledger": 3}[layout]
			if w.State != ResidueVerified || len(w.Entries) != want || w.Entries[0].Path != residuePath(1) || s.sessionOpens() != 0 ||
				layout != "hashed" && layout != "foreign-baseline" && approval == 0 {
				b.Fatalf("residue %+v, %d session opens", w, s.sessionOpens())
			}
		})
	}
	b.Run("over-limit", func(b *testing.B) {
		tr := benchTranscript(red, MaxEvidenceFileBytes, true)
		b.ReportAllocs()
		var out []byte
		for i := 0; i < b.N; i++ {
			out, _ = transcriptLines(red, tr)
		}
		b.StopTimer()
		b.ReportMetric(float64(len(out)), "bytes/op")
		w := &captureWriter{root: b.TempDir(), lim: DefaultCaptureLimits(), perClient: map[string]int{}}
		ref, cut, err := w.put("claude", ClientFile("claude", FileVendorEvents), out, true)
		if err != nil || !cut || ref.Bytes > MaxEvidenceFileBytes || len(out) <= MaxEvidenceFileBytes {
			b.Fatalf("over-limit stored %+v cut %v (%d bytes)", ref, cut, len(out))
		}
		dir := benchBundle(b, red, Transcript{Lines: []Line{{Data: []byte("x")}}})
		p := filepath.Join(dir, filepath.FromSlash(ClientFile("claude", FileVendorEvents)))
		os.WriteFile(p, out, 0o600)
		if _, err := ValidateCaptureBundle(os.DirFS(dir), "."); err == nil {
			b.Fatal("an oversized payload was accepted")
		}
	})
}

// benchBundle captures a fake claude session whose transcript is tr (with
// the production limits) and returns the bundle directory.
func benchBundle(b *testing.B, red *Redactor, tr Transcript) string {
	b.Helper()
	var raw strings.Builder
	for _, l := range tr.Lines {
		raw.Write(l.Data)
		raw.WriteByte('\n')
	}
	w := newCapWorld(b)
	w.script = func(spec ProcSpec) capBehavior {
		bh := w.defaults(spec)
		if launchKind(spec) == "session" {
			bh.stdout = raw.String()
		}
		return bh
	}
	c := &CaptureRunner{Plan: capPlan(b, "claude"), OutDir: filepath.Join(b.TempDir(), "out"), GOOS: "linux", GOARCH: "amd64", ServerPath: "/opt/mcpqual",
		BaseEnv: []string{"API_TOKEN=bench-secret-value"}, Launcher: w, Reaper: w, Clock: w.clock, Log: io.Discard, RunID: "run-bench", Nonce: "noncebench",
		CapturedAt: epoch, HarnessVersion: "bench", Home: "/home/bench", HashFile: func(string) (string, error) { return strings.Repeat("d", 64), nil }}
	man, err := c.Run(context.Background())
	if err != nil {
		b.Fatal(err)
	}
	if cc := man.Clients[0]; cc.State != CaptureComplete && len(tr.Lines) > 1 {
		b.Fatalf("bench capture %s", reasonOf(cc))
	}
	return c.OutDir
}
