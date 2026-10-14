package server

import (
	"context"
	"errors"

	"github.com/reallife-spark/goSpark/internal/apiv1"
	"github.com/reallife-spark/goSpark/internal/authz"
	"github.com/reallife-spark/goSpark/internal/placement"
)

// Place ranks the nodes the control plane may dispatch to for a capability and
// chooses one. In fail-closed mode the candidate set is the registry's nodes
// authorized for the capability; in open mode it is every node that has
// registered. A node's availability and capacity come from its last health
// report and its last GPU telemetry (from the agent or the sparknet-gpu
// sidecar, whichever is newer).
func (a *App) Place(req placement.Request, cap authz.Capability) placement.Decision {
	return placement.Select(a.candidates(cap), req)
}

// PlaceAndDispatch selects the best node for the requirement and dispatches the
// task to it. It returns the chosen node and the full decision, so a caller can
// log or surface why a node was picked.
func (a *App) PlaceAndDispatch(ctx context.Context, env apiv1.TaskEnvelope, req placement.Requirement) (string, placement.Decision, error) {
	dec := a.Place(placement.Request{Requirement: req}, authz.CapClusterServe)
	if dec.NodeID == "" {
		return "", dec, errors.New("server: no eligible node for dispatch")
	}
	if err := a.DispatchTask(ctx, dec.NodeID, env); err != nil {
		return dec.NodeID, dec, err
	}
	return dec.NodeID, dec, nil
}

// candidates assembles the placement inputs. Authorization is applied here so
// the placement engine stays a pure ranking function.
func (a *App) candidates(cap authz.Capability) []placement.Candidate {
	if a.registry == nil {
		return nil
	}
	a.regMu.RLock()
	reg := a.nodeRegistry
	a.regMu.RUnlock()

	var ids []string
	if reg != nil {
		for _, id := range reg.IDs() {
			if a.authorizeNode(id, cap) == nil {
				ids = append(ids, id)
			}
		}
	} else {
		ids = a.registry.IDs()
	}

	out := make([]placement.Candidate, 0, len(ids))
	for _, id := range ids {
		c := placement.Candidate{NodeID: id}
		if h, ok := a.registry.Get(id); ok {
			c.Serving = h.Serving
			c.LoadPermille = h.LoadPermille
			c.Telemetry = h.GPUTelemetry
		}
		if tel, ok := a.gpuTelemetry(id); ok {
			c.Telemetry = tel
		}
		out = append(out, c)
	}
	return out
}
