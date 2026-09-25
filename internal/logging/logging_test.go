package logging

import (
	"bytes"
	"encoding/json"
	"errors"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/wedevwork/callsheet/internal/contract"
)

func decodeLines(t *testing.T, b []byte) []map[string]any {
	t.Helper()
	var out []map[string]any
	for _, line := range bytes.Split(bytes.TrimSpace(b), []byte("\n")) {
		if len(line) == 0 {
			continue
		}
		var m map[string]any
		if err := json.Unmarshal(line, &m); err != nil {
			t.Fatalf("not JSON: %q: %v", line, err)
		}
		out = append(out, m)
	}
	return out
}

func TestUTCTimestampLevelAndFields(t *testing.T) {
	var buf bytes.Buffer
	l := Component(New(&buf, slog.LevelInfo), "plane")
	l.Info("started", KeyTask, "t1", KeyRequest, "r1", KeyNode, "n1", KeyRole, "coder")
	recs := decodeLines(t, buf.Bytes())
	if len(recs) != 1 {
		t.Fatalf("records = %d", len(recs))
	}
	r := recs[0]
	ts, err := time.Parse(time.RFC3339Nano, r["time"].(string))
	if err != nil {
		t.Fatal(err)
	}
	if _, off := ts.Zone(); off != 0 || !strings.HasSuffix(r["time"].(string), "Z") {
		t.Fatalf("timestamp not UTC: %v", r["time"])
	}
	if r["level"] != "INFO" || r["msg"] != "started" || r["component"] != "plane" {
		t.Fatalf("record = %v", r)
	}
	for k, v := range map[string]string{"task_id": "t1", "request_id": "r1", "node_id": "n1", "role": "coder"} {
		if r[k] != v {
			t.Fatalf("%s = %v", k, r[k])
		}
	}
}

func TestLevelFiltering(t *testing.T) {
	var buf bytes.Buffer
	l := New(&buf, slog.LevelWarn)
	l.Info("hidden")
	l.Debug("hidden")
	l.Warn("shown")
	recs := decodeLines(t, buf.Bytes())
	if len(recs) != 1 || recs[0]["msg"] != "shown" || recs[0]["level"] != "WARN" {
		t.Fatalf("records = %v", recs)
	}
}

func TestNoPayloadLeakage(t *testing.T) {
	var buf bytes.Buffer
	l := New(&buf, slog.LevelDebug)
	err := contract.Wrap(contract.CodeInternal, "write failed", errors.New("PROMPT-SECRET"))
	l.Error("task failed", "err", err)
	if strings.Contains(buf.String(), "PROMPT-SECRET") {
		t.Fatalf("cause leaked: %s", buf.String())
	}
	// Only the message and caller-supplied fields are present.
	recs := decodeLines(t, buf.Bytes())
	for k := range recs[0] {
		switch k {
		case "time", "level", "msg", "err":
		default:
			t.Fatalf("unexpected implicit field %q", k)
		}
	}
}

func TestGroupedTimeAttrUntouched(t *testing.T) {
	var buf bytes.Buffer
	l := New(&buf, slog.LevelInfo)
	local := time.Date(2026, 1, 2, 3, 4, 5, 0, time.FixedZone("X", 3600))
	l.Info("m", slog.Group("g", slog.Time("time", local)))
	if !strings.Contains(buf.String(), "+01:00") {
		t.Fatalf("grouped time must not be rewritten: %s", buf.String())
	}
}
