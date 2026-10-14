//go:build cgo && linux

// Package dcgm is the DCGM health backend for gputel. It reads per-GPU health
// verdicts through go-dcgm, which wraps libdcgm. It is imported only by the
// sparknet-gpu sidecar.
//
// DCGM runs in embedded mode (the hostengine lives in this process), so no
// external nv-hostengine is required. A host without libdcgm degrades to a
// typed reason, not a crash.
package dcgm

import (
	"context"
	"time"

	dcgmapi "github.com/NVIDIA/go-dcgm/pkg/dcgm"

	"github.com/reallife-spark/goSpark/internal/gputel"
)

func init() {
	gputel.Register("dcgm", func() gputel.Collector { return collector{} })
}

type collector struct{}

// Snapshot runs a health check for every DCGM-supported GPU. Health is
// telemetry: a failure verdict is reported, never acted on.
func (collector) Snapshot(context.Context) gputel.Snapshot {
	s := gputel.Snapshot{Source: "dcgm", CollectedAtUnix: time.Now().Unix()}

	cleanup, err := dcgmapi.Init(dcgmapi.Embedded)
	if err != nil {
		s.Reason = "dcgm: init: " + err.Error()
		return s
	}
	defer cleanup()

	ids, err := dcgmapi.GetSupportedDevices()
	if err != nil {
		s.Reason = "dcgm: enumerate devices: " + err.Error()
		return s
	}
	for _, id := range ids {
		dh, err := dcgmapi.HealthCheckByGpuId(id)
		if err != nil {
			s.Health = append(s.Health, gputel.Health{Index: uint32(id), Status: "N/A", Watches: []string{"error: " + err.Error()}})
			continue
		}
		h := gputel.Health{Index: uint32(id), Status: dh.Status}
		for _, w := range dh.Watches {
			h.Watches = append(h.Watches, w.Type+": "+w.Status)
		}
		s.Health = append(s.Health, h)
	}
	s.Available = true
	return s
}
