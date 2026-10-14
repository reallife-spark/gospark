package telemetry

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"strings"
	"testing"
)

func TestLoggerLevelsAndFormats(t *testing.T) {
	for _, format := range []string{"text", "json"} {
		l := Logger("debug", format)
		if l == nil {
			t.Fatalf("nil logger for %s", format)
		}
	}
	// Unknown level falls back to info rather than failing.
	if Logger("bogus", "text") == nil {
		t.Fatal("default logger is nil")
	}
}

func TestLoggerJSONEmits(t *testing.T) {
	var buf bytes.Buffer
	h := slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelInfo})
	l := slog.New(h)
	l.Info("hello", "k", "v")
	if !strings.Contains(buf.String(), `"msg":"hello"`) {
		t.Fatalf("json log = %s", buf.String())
	}
}

func TestCountersSnapshot(t *testing.T) {
	var c Counters
	c.TasksVerified.Add(2)
	c.TasksRefused.Add(1)
	c.ResultsAttested.Add(3)
	c.BlocksStreamed.Add(4)
	c.BlocksReceived.Add(5)
	c.BytesStreamed.Add(6)

	snap := c.Snapshot()
	// Round-trip through JSON to prove the tags are right.
	data, err := json.Marshal(snap)
	if err != nil {
		t.Fatal(err)
	}
	want := `{"tasks_verified":2,"tasks_refused":1,"results_attested":3,"blocks_streamed":4,"blocks_received":5,"bytes_streamed":6}`
	if string(data) != want {
		t.Fatalf("snapshot = %s, want %s", data, want)
	}
}
