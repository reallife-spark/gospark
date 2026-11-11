// Package authz is the goSpark control-plane verifier: it decides whether a
// signed envelope is authentic, live, and carries the capability it claims.
//
// It mirrors the fabric rule that authority comes from key possession, not from
// a capability name: holding the vocabulary without the signing key grants
// nothing. A peer with every capability but no private key can never authorise.
package authz

import "sort"

// Capability names the closed vocabulary. It mirrors
// contracts/capabilities.json in the sparknet repo (snapshotted under
// spec/sparknet/capabilities.json). Adding one is a contract change.
type Capability string

const (
	CapTelemetry        Capability = "telemetry"
	CapP2PSync          Capability = "p2p-sync"
	CapPushReceive      Capability = "push-receive"
	CapGuardApply       Capability = "guard-apply"
	CapClusterVote      Capability = "cluster-vote"
	CapClusterDispatch  Capability = "cluster-dispatch"
	CapClusterServe     Capability = "cluster-serve"
	CapAdminSupervise   Capability = "admin-supervise"
	CapSandboxSupervise Capability = "sandbox-supervise"
	CapModelUplift      Capability = "model-uplift"
	CapMemorySupervise  Capability = "memory-supervise"
	CapIssueReport      Capability = "issue-report"

	// CapCrossDepartmentShare lets a node share with one whose Master-attested
	// department differs from its own. Inside a department no extra grant is
	// needed; crossing out of one is the privilege this names.
	CapCrossDepartmentShare Capability = "cross-department-share"
)

// KnownCapabilities is the complete vocabulary. Anything not listed is refused
// by both signer and verifier.
var KnownCapabilities = []Capability{
	CapTelemetry,
	CapP2PSync,
	CapPushReceive,
	CapGuardApply,
	CapClusterVote,
	CapClusterDispatch,
	CapClusterServe,
	CapAdminSupervise,
	CapSandboxSupervise,
	CapCrossDepartmentShare,
	CapModelUplift,
	CapMemorySupervise,
	CapIssueReport,
}

var knownSet = func() map[Capability]struct{} {
	m := make(map[Capability]struct{}, len(KnownCapabilities))
	for _, c := range KnownCapabilities {
		m[c] = struct{}{}
	}
	return m
}()

// Sensitive names the capabilities whose holder incurs the risk, so the control
// plane can require an explicit operator opt-in before serving them.
var sensitive = map[Capability]bool{
	CapGuardApply:           true,
	CapClusterServe:         true,
	CapAdminSupervise:       true,
	CapSandboxSupervise:     true,
	CapCrossDepartmentShare: true,
	CapModelUplift:          true,
	CapMemorySupervise:      true,
}

// MasterImpliedCapabilities are not granted to any peer: they are *implied by
// possession of the master's private key*, per contracts/capabilities.json
// (`master_capabilities.implied_by_private_key`). Holding the vocabulary
// without the key grants nothing, so these have no verifier path — they exist
// to name what the signer alone can do.
var MasterImpliedCapabilities = []string{
	"sign-authorization",
	"revoke-node",
	"distribute-registry",
}

// IsKnown reports whether c is in the closed vocabulary.
func IsKnown(c Capability) bool {
	_, ok := knownSet[c]
	return ok
}

// IsSensitive reports whether c is a sensitive (holder-bears-risk) capability.
func IsSensitive(c Capability) bool { return sensitive[c] }

// KnownSorted returns the vocabulary in sorted order for stable output.
func KnownSorted() []Capability {
	out := append([]Capability(nil), KnownCapabilities...)
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}
