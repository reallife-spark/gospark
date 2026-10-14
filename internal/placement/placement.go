// Package placement turns GPU telemetry into a dispatch decision.
//
// The control plane knows, per node, whether it is serving and what its last
// GPU snapshot was. Placement ranks the nodes the caller may dispatch to and
// returns a best-first ordering plus a single chosen node. It is deliberately
// pure: it takes plain candidates and returns a decision, so the same engine is
// used by the dispatch path, the admin surface and the CLI, and is tested with
// no bus, no registry and no GPU.
//
// This is goSpark's control-plane reading of the session's "departmental GPU
// delegation" model: given a set of authorized nodes with last-known capacity,
// pick the one that can take the work. The heavy constrained assignment (the
// x_ij optimization across a department's four nodes) stays in the compute
// plane; here the question is only *which* node, never *how* to shard.
package placement

import (
	"sort"
	"time"

	"github.com/reallife-spark/goSpark/internal/apiv1"
)

// Candidate is one node the control plane could dispatch to. The caller is
// responsible for having already applied authorization (the registry); placement
// only reasons about availability and capacity.
type Candidate struct {
	NodeID       string
	Serving      bool
	LoadPermille uint32
	// Telemetry is the node's last GPU snapshot, or nil if none was ever
	// reported (for example a node with no sidecar).
	Telemetry *apiv1.GpuTelemetry
}

// Requirement bounds which candidates may take the work. A zero Requirement
// imposes no constraint: every serving candidate is eligible and the ranking is
// advisory.
type Requirement struct {
	// MinFreeMemBytes, when > 0, requires a node whose last GPU snapshot shows
	// at least this much free accelerator memory across its GPUs.
	MinFreeMemBytes uint64
	// MaxTempC, when > 0, requires every reported GPU temperature to be at or
	// below this ceiling.
	MaxTempC uint32
	// RequireHealthy demands a GPU health verdict and that every reported GPU is
	// "Healthy".
	RequireHealthy bool
}

// Request is a Requirement plus policy bounds.
type Request struct {
	Requirement
	// Now is the reference time for freshness (zero means time.Now()).
	Now time.Time
	// MaxTelemetryAge, when > 0, treats telemetry older than this as stale. A
	// stale reading cannot satisfy a non-zero Requirement, and ranks behind a
	// fresh one.
	MaxTelemetryAge time.Duration
}

// Scored is one candidate's rank and the numbers behind it.
type Scored struct {
	NodeID        string `json:"node_id"`
	Eligible      bool   `json:"eligible"`
	Reason        string `json:"reason,omitempty"`
	HasTelemetry  bool   `json:"has_telemetry"`
	Available     bool   `json:"available,omitempty"`
	Fresh         bool   `json:"fresh,omitempty"`
	TelemetryAgeS int64  `json:"telemetry_age_seconds,omitempty"`
	GPUs          int    `json:"gpus,omitempty"`
	FreeMemBytes  uint64 `json:"free_mem_bytes,omitempty"`
	MaxTempC      uint32 `json:"max_temp_c,omitempty"`
	MaxUtilPct    uint32 `json:"max_util_pct,omitempty"`
	LoadPermille  uint32 `json:"load_permille,omitempty"`
}

// Decision is the placement result: the chosen node (empty if none is eligible)
// and the full best-first ranking that explains it.
type Decision struct {
	NodeID   string   `json:"node_id,omitempty"`
	Reason   string   `json:"reason"`
	Ranked   []Scored `json:"ranked"`
	Eligible int      `json:"eligible"`
}

type row struct {
	candidate Candidate
	score     Scored
	fresh     bool
}

// Select ranks candidates and chooses one.
func Select(candidates []Candidate, req Request) Decision {
	now := req.Now
	if now.IsZero() {
		now = time.Now()
	}
	eligible := make([]row, 0, len(candidates))
	ineligible := make([]row, 0, len(candidates))
	for _, c := range candidates {
		r := evaluate(c, req, now)
		if r.score.Eligible {
			eligible = append(eligible, r)
		} else {
			ineligible = append(ineligible, r)
		}
	}

	sort.SliceStable(eligible, func(i, j int) bool { return better(eligible[i], eligible[j]) })
	sort.SliceStable(ineligible, func(i, j int) bool {
		return ineligible[i].candidate.NodeID < ineligible[j].candidate.NodeID
	})

	dec := Decision{Eligible: len(eligible)}
	for _, r := range eligible {
		dec.Ranked = append(dec.Ranked, r.score)
	}
	for _, r := range ineligible {
		dec.Ranked = append(dec.Ranked, r.score)
	}
	if len(eligible) == 0 {
		dec.Reason = "no eligible node"
		return dec
	}
	dec.NodeID = eligible[0].candidate.NodeID
	dec.Reason = chooseReason(eligible[0])
	return dec
}

// better reports whether a ranks ahead of b. Both are known eligible.
func better(a, b row) bool {
	if a.fresh != b.fresh {
		return a.fresh
	}
	if a.score.FreeMemBytes != b.score.FreeMemBytes {
		return a.score.FreeMemBytes > b.score.FreeMemBytes
	}
	if at, bt := tempKey(a.score), tempKey(b.score); at != bt {
		return at < bt
	}
	if a.score.MaxUtilPct != b.score.MaxUtilPct {
		return a.score.MaxUtilPct < b.score.MaxUtilPct
	}
	if a.candidate.LoadPermille != b.candidate.LoadPermille {
		return a.candidate.LoadPermille < b.candidate.LoadPermille
	}
	return a.candidate.NodeID < b.candidate.NodeID
}

// tempKey orders by temperature, treating "no reading" as worst so an unknown
// temperature never outranks a known-cool node.
func tempKey(s Scored) uint32 {
	if s.MaxTempC == 0 {
		return ^uint32(0)
	}
	return s.MaxTempC
}

func chooseReason(r row) string {
	switch {
	case r.fresh && r.score.GPUs > 0:
		return "most free accelerator memory"
	case r.fresh:
		return "best last-known telemetry"
	default:
		return "no telemetry; least loaded eligible node"
	}
}

// evaluate scores one candidate against the request.
func evaluate(c Candidate, req Request, now time.Time) row {
	s := Scored{NodeID: c.NodeID, LoadPermille: c.LoadPermille}
	r := row{candidate: c, score: s}
	if !c.Serving {
		r.score.Reason = "not serving"
		return r
	}

	var available bool
	if tel := c.Telemetry; tel != nil {
		r.score.HasTelemetry = true
		available = tel.Available
		r.score.Available = available
		r.score.GPUs = len(tel.GPUs)
		for _, g := range tel.GPUs {
			if g.MemTotalBytes > g.MemUsedBytes {
				r.score.FreeMemBytes += g.MemTotalBytes - g.MemUsedBytes
			}
			if g.TempC > r.score.MaxTempC {
				r.score.MaxTempC = g.TempC
			}
			if g.UtilPct > r.score.MaxUtilPct {
				r.score.MaxUtilPct = g.UtilPct
			}
		}
		if tel.CollectedAtUnix > 0 {
			r.score.TelemetryAgeS = int64(now.Unix()) - tel.CollectedAtUnix
			if r.score.TelemetryAgeS < 0 {
				r.score.TelemetryAgeS = 0
			}
		}
		// A reading is fresh only if it came from an available accelerator and
		// is within the age bound (when one is set).
		if available {
			r.fresh = true
			if req.MaxTelemetryAge > 0 {
				if tel.CollectedAtUnix <= 0 ||
					r.score.TelemetryAgeS > int64(req.MaxTelemetryAge/time.Second) {
					r.fresh = false
				}
			}
		}
	}
	r.score.Fresh = r.fresh

	// A non-zero requirement needs an available, fresh, usable reading.
	if req.MinFreeMemBytes > 0 || req.MaxTempC > 0 || req.RequireHealthy {
		switch {
		case !r.score.HasTelemetry:
			r.score.Reason = "no telemetry for requirement"
			return r
		case !available:
			r.score.Reason = "accelerator unavailable"
			return r
		case !r.fresh:
			r.score.Reason = "telemetry stale"
			return r
		}
		if req.MinFreeMemBytes > 0 && r.score.FreeMemBytes < req.MinFreeMemBytes {
			r.score.Reason = "insufficient free accelerator memory"
			return r
		}
		if req.MaxTempC > 0 && r.score.MaxTempC > req.MaxTempC {
			r.score.Reason = "temperature above ceiling"
			return r
		}
		if req.RequireHealthy && !allHealthy(c.Telemetry) {
			r.score.Reason = "accelerator not healthy"
			return r
		}
	}

	r.score.Eligible = true
	return r
}

// allHealthy reports whether every GPU that carries a verdict is Healthy.
func allHealthy(tel *apiv1.GpuTelemetry) bool {
	if tel == nil || len(tel.GPUs) == 0 {
		return false
	}
	for _, g := range tel.GPUs {
		if g.Health != "" && g.Health != "Healthy" {
			return false
		}
	}
	return true
}
