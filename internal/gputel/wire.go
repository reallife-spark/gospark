package gputel

import "github.com/reallife-spark/goSpark/internal/apiv1"

// Wire projects a Snapshot onto the on-wire GpuTelemetry. It lives here, next to
// the internal types, so the mapping exists in exactly one place: a new metric
// is added to both structs and this function together.
func (s Snapshot) Wire() apiv1.GpuTelemetry {
	out := apiv1.GpuTelemetry{
		Source:          s.Source,
		Available:       s.Available,
		Reason:          s.Reason,
		DriverVersion:   s.DriverVersion,
		CollectedAtUnix: s.CollectedAtUnix,
	}
	if len(s.GPUs) == 0 {
		return out
	}
	health := make(map[uint32]string, len(s.Health))
	for _, h := range s.Health {
		health[h.Index] = h.Status
	}
	out.GPUs = make([]apiv1.GpuMetrics, 0, len(s.GPUs))
	for _, g := range s.GPUs {
		out.GPUs = append(out.GPUs, apiv1.GpuMetrics{
			Index:           g.Index,
			Name:            g.Name,
			UUID:            g.UUID,
			TempC:           g.TempC,
			MemUsedBytes:    g.MemUsedBytes,
			MemTotalBytes:   g.MemTotalBytes,
			UtilPct:         g.UtilPct,
			MemUtilPct:      g.MemUtilPct,
			PowerMilliwatts: g.PowerMilliwatts,
			SMClockMHz:      g.SMClockMHz,
			MemClockMHz:     g.MemClockMHz,
			Health:          health[g.Index],
		})
	}
	return out
}
