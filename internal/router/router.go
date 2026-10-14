// Package router decides where a prompt is resolved: on the edge, on a single
// remote teacher, or as a fan-out cluster job.
//
// It deliberately returns a *decision*, never a result: the caller may proceed
// on the edge immediately while the control plane schedules remote work out of
// band. That is the async pipeline boundary the design insists on — the router
// never makes a caller block on a WAN round trip.
package router

import (
	"strings"

	"github.com/reallife-spark/goSpark/internal/apiv1"
	"github.com/reallife-spark/goSpark/internal/authz"
)

// Mode is a cluster job mode.
type Mode string

const (
	ModeDecompose Mode = "decompose"
	ModeShard     Mode = "shard"
	ModeBestOfN   Mode = "best_of_n"
	ModeMixture   Mode = "mixture"
)

// Config bounds the router.
type Config struct {
	EdgeTokenCeiling  int
	ClusterTokenFloor int
	MaxNodes          int
	// EdgeDomains are subject prefixes the local student is trusted to own.
	EdgeDomains []string
}

// Router makes escalation decisions.
type Router struct {
	cfg      Config
	keywords map[string]struct{}
}

// New builds a router with sensible bounds if cfg is zero-valued.
func New(cfg Config) *Router {
	if cfg.EdgeTokenCeiling <= 0 {
		cfg.EdgeTokenCeiling = 2048
	}
	if cfg.ClusterTokenFloor < cfg.EdgeTokenCeiling {
		cfg.ClusterTokenFloor = 32768
	}
	if cfg.MaxNodes <= 0 {
		cfg.MaxNodes = 4
	}
	// Prompts mentioning these demand cross-domain synthesis the student cannot
	// be trusted to own, regardless of length.
	kws := []string{"synthesi", "cross-domain", "multi-step", "prove", "derive", "architecture", "compare and contrast"}
	m := make(map[string]struct{}, len(kws))
	for _, k := range kws {
		m[k] = struct{}{}
	}
	return &Router{cfg: cfg, keywords: m}
}

// EstimateTokens is a cheap deterministic proxy for token count. It is not a
// tokenizer: the control plane must not depend on the model's tokenizer, and a
// stable estimate is enough to choose a route.
func EstimateTokens(prompt string) int {
	words := len(strings.Fields(prompt))
	chars := len(prompt)
	byWords := (words*4 + 2) / 3 // ~1.33 tokens/word
	byChars := (chars + 3) / 4   // ~4 chars/token
	if byChars > byWords {
		return byChars
	}
	return byWords
}

// Decide returns the escalation decision for a prompt.
func (r *Router) Decide(prompt string, localCaps []string) apiv1.EscalationDecision {
	tokens := EstimateTokens(prompt)
	lower := strings.ToLower(prompt)

	reason := "within local domain and token ceiling; resolve on the edge"
	target := apiv1.TargetEdge

	switch {
	case tokens >= r.cfg.ClusterTokenFloor:
		target = apiv1.TargetCluster
		reason = "prompt exceeds cluster token floor; fan out a cluster job"
	case hasKeyword(lower, r.keywords):
		target = apiv1.TargetDGX
		reason = "prompt demands cross-domain synthesis; escalate to a DGX teacher"
	case tokens > r.cfg.EdgeTokenCeiling:
		target = apiv1.TargetDGX
		reason = "prompt exceeds the edge token ceiling; escalate to a DGX teacher"
	}

	// No capability to dispatch means the control plane cannot fan out; degrade
	// to a single-teacher escalation rather than silently claiming a cluster.
	if target == apiv1.TargetCluster && !contains(localCaps, string(authz.CapClusterDispatch)) {
		target = apiv1.TargetDGX
		reason = "cluster dispatch not held; degrade to a single DGX teacher"
	}

	return apiv1.EscalationDecision{
		Target:                target,
		Reason:                reason,
		EstimatedPromptTokens: uint32(tokens),
		AsyncRequired:         target != apiv1.TargetEdge,
	}
}

// ModeFor picks a cluster mode from the prompt shape.
func (r *Router) ModeFor(prompt string) Mode {
	tokens := EstimateTokens(prompt)
	switch {
	case tokens >= r.cfg.ClusterTokenFloor*4:
		return ModeShard
	case strings.Count(prompt, "?") >= 2 || strings.Contains(prompt, "\n-"):
		return ModeDecompose
	case tokens < 256:
		return ModeBestOfN
	default:
		return ModeMixture
	}
}

// PlanSubTasks divides a prompt into at most maxNodes subtasks. Assignment is
// deterministic given the inputs so the same job re-dispatched partitions the
// same way — the non-determinism belongs to the models, never to who was asked.
func (r *Router) PlanSubTasks(prompt string, nodes []string) []apiv1.SubTask {
	if len(nodes) == 0 {
		return nil
	}
	max := r.cfg.MaxNodes
	if max > len(nodes) {
		max = len(nodes)
	}
	parts := splitPrompt(prompt, max)
	out := make([]apiv1.SubTask, 0, len(parts))
	for i, part := range parts {
		node := nodes[i%len(nodes)]
		out = append(out, apiv1.SubTask{
			NodeID:      node,
			SubtaskID:   subtaskID(i),
			Instruction: part,
		})
	}
	return out
}

func splitPrompt(prompt string, n int) []string {
	if n <= 1 {
		return []string{prompt}
	}
	// Split on blank lines first, then on question boundaries; fall back to one.
	chunks := splitNonEmpty(prompt, "\n\n")
	if len(chunks) < n {
		chunks = splitNonEmpty(prompt, "\n")
	}
	if len(chunks) < n {
		return []string{prompt}
	}
	// Fold extras into the last bucket so count == n.
	if len(chunks) > n {
		head := chunks[:n-1]
		head = append(head, strings.Join(chunks[n-1:], "\n"))
		return head
	}
	return chunks
}

func splitNonEmpty(s, sep string) []string {
	raw := strings.Split(s, sep)
	out := make([]string, 0, len(raw))
	for _, p := range raw {
		if strings.TrimSpace(p) != "" {
			out = append(out, strings.TrimSpace(p))
		}
	}
	return out
}

func subtaskID(i int) string {
	return "st-" + itoa(i+1)
}

func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	var b [20]byte
	pos := len(b)
	for i > 0 {
		pos--
		b[pos] = byte('0' + i%10)
		i /= 10
	}
	return string(b[pos:])
}

func hasKeyword(lower string, kws map[string]struct{}) bool {
	for k := range kws {
		if strings.Contains(lower, k) {
			return true
		}
	}
	return false
}

func contains(list []string, want string) bool {
	for _, s := range list {
		if s == want {
			return true
		}
	}
	return false
}
