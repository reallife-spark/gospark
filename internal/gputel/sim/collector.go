// Package sim is an opt-in, synthetic GPU-telemetry backend for goSpark.
//
// It reads a JSON snapshot (the gputel.Snapshot wire shape) from a fixture file
// so a host with no NVIDIA driver — a CI runner, the dev box, a container — can
// still drive the full telemetry path (sidecar -> bus -> control /admin/gpu ->
// placement -> metrics) with available=true. Nothing here is ever active by
// default: the backend only registers when the operator sets
// SPARKNET_GPU_FIXTURE / SPARKNET_GPU_SIM_CFG, or passes sparknet-gpu's
// -sim-fixture flag.
//
// It is telemetry, never authority: the fixture cannot grant capabilities, and
// real hosts must not set it (a fixture merged into real NVML readings would
// fake GPU indices). See scripts/make-gpu-fixture.py in the fabric repo.
package sim

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"time"

	"github.com/reallife-spark/goSpark/internal/gputel"
)

var (
	fixtureFile string
	fixtureJSON string
)

func init() {
	if p := os.Getenv("SPARKNET_GPU_FIXTURE"); p != "" {
		fixtureFile = p
		register()
		return
	}
	if c := os.Getenv("SPARKNET_GPU_SIM_CFG"); c != "" {
		fixtureJSON = c
		register()
	}
}

// register installs the backend in gputel's registry. The factory reads the
// package vars at call time, so a later SetFixture wins over the environment.
func register() {
	gputel.Register("sim", func() gputel.Collector {
		return collector{filename: fixtureFile, raw: fixtureJSON}
	})
}

// SetFixture opts in to the sim backend from a flag: it points the backend at
// fixtureFile and registers it if the environment did not already.
func SetFixture(path string) {
	fixtureFile = path
	fixtureJSON = ""
	if _, ok := gputel.Get("sim"); !ok {
		register()
	}
}

type collector struct {
	filename string
	raw      string
}

// New returns a sim collector reading its snapshot from a JSON fixture file.
// The fixture is a gputel.Snapshot (see gputel.go for field names).
func New(filename string) gputel.Collector {
	return collector{filename: filename}
}

func (c collector) Snapshot(ctx context.Context) gputel.Snapshot {
	var b []byte
	var err error
	switch {
	case c.filename != "":
		b, err = os.ReadFile(c.filename)
		if err != nil {
			return unavailable(fmt.Sprintf("sim: read fixture %q: %v", c.filename, err))
		}
	case c.raw != "":
		b = []byte(c.raw)
	default:
		return unavailable("sim: no fixture configured")
	}
	var s gputel.Snapshot
	if err := json.Unmarshal(b, &s); err != nil {
		return unavailable(fmt.Sprintf("sim: unmarshal fixture: %v", err))
	}
	if s.Source == "" {
		s.Source = "sim"
	}
	if s.CollectedAtUnix == 0 {
		s.CollectedAtUnix = time.Now().Unix()
	}
	return s
}

func unavailable(reason string) gputel.Snapshot {
	return gputel.Snapshot{
		Source:          "sim",
		Available:       false,
		Reason:          reason,
		CollectedAtUnix: time.Now().Unix(),
	}
}
