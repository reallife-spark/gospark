// Package telemetry is the control plane's logging and metrics surface. It is
// deliberately thin: a slog logger and a handful of atomic counters, with no
// vendor dependency, so a node's operational story is readable from its own
// process.
package telemetry

import (
	"log/slog"
	"os"
	"strings"
	"sync/atomic"
)

// Logger builds a slog.Logger from level/format strings.
func Logger(level, format string) *slog.Logger {
	var lvl slog.Level
	switch strings.ToLower(level) {
	case "debug":
		lvl = slog.LevelDebug
	case "warn":
		lvl = slog.LevelWarn
	case "error":
		lvl = slog.LevelError
	default:
		lvl = slog.LevelInfo
	}
	opts := &slog.HandlerOptions{Level: lvl}
	var h slog.Handler
	if strings.ToLower(format) == "json" {
		h = slog.NewJSONHandler(os.Stderr, opts)
	} else {
		h = slog.NewTextHandler(os.Stderr, opts)
	}
	return slog.New(h)
}

// Counters is a fixed set of control-plane counters.
type Counters struct {
	TasksVerified   atomic.Uint64
	TasksRefused    atomic.Uint64
	ResultsAttested atomic.Uint64
	BlocksStreamed  atomic.Uint64
	BlocksReceived  atomic.Uint64
	BytesStreamed   atomic.Uint64
}

// Snapshot is a plain-value view of the counters.
type Snapshot struct {
	TasksVerified   uint64 `json:"tasks_verified"`
	TasksRefused    uint64 `json:"tasks_refused"`
	ResultsAttested uint64 `json:"results_attested"`
	BlocksStreamed  uint64 `json:"blocks_streamed"`
	BlocksReceived  uint64 `json:"blocks_received"`
	BytesStreamed   uint64 `json:"bytes_streamed"`
}

// Snapshot reads the counters.
func (c *Counters) Snapshot() Snapshot {
	return Snapshot{
		TasksVerified:   c.TasksVerified.Load(),
		TasksRefused:    c.TasksRefused.Load(),
		ResultsAttested: c.ResultsAttested.Load(),
		BlocksStreamed:  c.BlocksStreamed.Load(),
		BlocksReceived:  c.BlocksReceived.Load(),
		BytesStreamed:   c.BytesStreamed.Load(),
	}
}
