// Package gputel collects GPU telemetry for a goSpark node.
//
// The core of this package is deliberately dependency-free: it defines the
// Collector interface and the snapshot types, a Null collector and a registry,
// and knows nothing about NVML or DCGM. The cgo backends live in subpackages
// (gputel/nvml, gputel/dcgm) and register themselves, so the pure-Go control and
// service binaries never link a GPU library — only cmd/sparknet-gpu does.
//
// This is the goSpark face of the session's "Cluster Health Monitor" agent: the
// Python daemon scraped `nvidia-smi`; here the control plane reads NVML and DCGM
// directly and reports the result as telemetry, never as authority.
package gputel

import (
	"context"
	"sort"
	"strings"
	"sync"
	"time"
)

// GPU is one GPU's instantaneous telemetry. A zero value means "not reported",
// which is why the wire projection drops empty fields.
type GPU struct {
	Index           uint32 `json:"index"`
	Name            string `json:"name,omitempty"`
	UUID            string `json:"uuid,omitempty"`
	TempC           uint32 `json:"temp_c,omitempty"`
	MemUsedBytes    uint64 `json:"mem_used_bytes,omitempty"`
	MemTotalBytes   uint64 `json:"mem_total_bytes,omitempty"`
	UtilPct         uint32 `json:"util_pct,omitempty"`
	MemUtilPct      uint32 `json:"mem_util_pct,omitempty"`
	PowerMilliwatts uint32 `json:"power_milliwatts,omitempty"`
	SMClockMHz      uint32 `json:"sm_clock_mhz,omitempty"`
	MemClockMHz     uint32 `json:"mem_clock_mhz,omitempty"`
}

// Health is one GPU's DCGM health verdict.
type Health struct {
	Index   uint32   `json:"index"`
	Status  string   `json:"status"`
	Watches []string `json:"watches,omitempty"`
}

// Snapshot is a collector's answer. When Available is false, Reason explains why
// (for example "no NVIDIA driver") and the GPU/Health slices are empty.
type Snapshot struct {
	Source          string   `json:"source"`
	Available       bool     `json:"available"`
	Reason          string   `json:"reason,omitempty"`
	DriverVersion   string   `json:"driver_version,omitempty"`
	GPUs            []GPU    `json:"gpus,omitempty"`
	Health          []Health `json:"health,omitempty"`
	CollectedAtUnix int64    `json:"collected_at_unix"`
}

// Collector produces a Snapshot. It must never panic: a missing driver or
// library is a normal, reported condition, not a crash.
type Collector interface {
	Snapshot(ctx context.Context) Snapshot
}

// Factory builds a Collector.
type Factory func() Collector

var (
	regMu    sync.RWMutex
	backends = map[string]Factory{}
)

// Register adds a named backend. It is called from a backend package's init; a
// name registered twice panics, which is a programming error, not a runtime one.
func Register(name string, f Factory) {
	regMu.Lock()
	defer regMu.Unlock()
	if _, dup := backends[name]; dup {
		panic("gputel: duplicate backend " + name)
	}
	backends[name] = f
}

// Names lists the registered backends, sorted.
func Names() []string {
	regMu.RLock()
	defer regMu.RUnlock()
	out := make([]string, 0, len(backends))
	for n := range backends {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

// Get returns the named backend, or (nil, false).
func Get(name string) (Collector, bool) {
	regMu.RLock()
	f, ok := backends[name]
	regMu.RUnlock()
	if !ok {
		return nil, false
	}
	return f(), true
}

// Default returns a collector over every registered backend, merged. When none
// is registered — a CGO_ENABLED=0 build, or a host with no GPU backend — it
// returns a Null collector, so a caller always gets a usable, honest answer.
func Default() Collector {
	names := Names()
	switch len(names) {
	case 0:
		return Null{Reason: "no GPU backend built in"}
	case 1:
		c, _ := Get(names[0])
		return c
	}
	cs := make([]Collector, 0, len(names))
	for _, n := range names {
		c, _ := Get(n)
		cs = append(cs, c)
	}
	return Multi(cs)
}

// Null is a collector that always reports telemetry unavailable.
type Null struct{ Reason string }

// Snapshot implements Collector.
func (n Null) Snapshot(context.Context) Snapshot {
	r := n.Reason
	if r == "" {
		r = "gpu telemetry unavailable"
	}
	return Snapshot{Source: "null", Available: false, Reason: r, CollectedAtUnix: time.Now().Unix()}
}

// Multi merges several collectors into one snapshot. A GPU already reported is
// not overwritten, but its empty fields are filled from later collectors, so an
// NVML metrics reading and a DCGM health verdict combine onto one record.
type Multi []Collector

// Snapshot implements Collector.
func (m Multi) Snapshot(ctx context.Context) Snapshot {
	out := Snapshot{Source: "multi", CollectedAtUnix: time.Now().Unix()}
	var sources, reasons []string
	byGPU := map[uint32]int{}
	byHealth := map[uint32]int{}
	for _, c := range m {
		s := c.Snapshot(ctx)
		if s.Source != "" {
			sources = append(sources, s.Source)
		}
		if !s.Available {
			if s.Reason != "" {
				reasons = append(reasons, s.Source+": "+s.Reason)
			}
			continue
		}
		out.Available = true
		if out.DriverVersion == "" {
			out.DriverVersion = s.DriverVersion
		}
		for _, g := range s.GPUs {
			if i, ok := byGPU[g.Index]; ok {
				out.GPUs[i] = fill(out.GPUs[i], g)
			} else {
				byGPU[g.Index] = len(out.GPUs)
				out.GPUs = append(out.GPUs, g)
			}
		}
		for _, h := range s.Health {
			if i, ok := byHealth[h.Index]; ok {
				out.Health[i] = h
			} else {
				byHealth[h.Index] = len(out.Health)
				out.Health = append(out.Health, h)
			}
		}
	}
	out.Source = strings.Join(sources, "+")
	if out.Source == "" {
		out.Source = "multi"
	}
	if !out.Available {
		out.Reason = strings.Join(reasons, "; ")
		if out.Reason == "" {
			out.Reason = "no collector reported telemetry"
		}
	}
	return out
}

// fill fills only the zero-valued fields of base from add.
func fill(base, add GPU) GPU {
	if base.Name == "" {
		base.Name = add.Name
	}
	if base.UUID == "" {
		base.UUID = add.UUID
	}
	if base.TempC == 0 {
		base.TempC = add.TempC
	}
	if base.MemUsedBytes == 0 {
		base.MemUsedBytes = add.MemUsedBytes
	}
	if base.MemTotalBytes == 0 {
		base.MemTotalBytes = add.MemTotalBytes
	}
	if base.UtilPct == 0 {
		base.UtilPct = add.UtilPct
	}
	if base.MemUtilPct == 0 {
		base.MemUtilPct = add.MemUtilPct
	}
	if base.PowerMilliwatts == 0 {
		base.PowerMilliwatts = add.PowerMilliwatts
	}
	if base.SMClockMHz == 0 {
		base.SMClockMHz = add.SMClockMHz
	}
	if base.MemClockMHz == 0 {
		base.MemClockMHz = add.MemClockMHz
	}
	return base
}
