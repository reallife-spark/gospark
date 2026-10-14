// Package bus is the goSpark control-plane message fabric.
//
// The design follows the session's recommendation: typed contracts (gRPC-shaped
// payloads) routed over a resilient, store-and-forward backbone. NATS with
// JetStream is the backbone because it tolerates an intermittent WireGuard WAN
// gracefully — a node that drops off the mesh has its control messages queued
// rather than failing immediately, which is exactly what a direct gRPC dial
// does not give you.
//
// A Memory bus is provided for tests and single-process runs.
package bus

import "context"

// Handler receives one message. Returning an error signals a NAK to a
// JetStream consumer; the Memory bus ignores it.
type Handler func(ctx context.Context, subject string, data []byte) error

// Bus is the minimal control-plane surface.
type Bus interface {
	// Publish sends data to subject.
	Publish(ctx context.Context, subject string, data []byte) error
	// Subscribe registers a handler for subject (wildcards allowed).
	Subscribe(ctx context.Context, subject string, h Handler) (Subscription, error)
	// Request publishes and waits for one reply.
	Request(ctx context.Context, subject string, data []byte) ([]byte, error)
	// Close releases resources.
	Close() error
}

// Subscription cancels a handler.
type Subscription interface {
	Unsubscribe() error
}

// Subject helpers build the canonical subject tree.
func Subject(root, kind, node string) string {
	if root == "" {
		root = "sparknet"
	}
	if node == "" {
		return root + "." + kind
	}
	return root + "." + kind + "." + node
}

const (
	KindRegister = "register"
	KindHealth   = "health"
	KindTask     = "task"
	KindResult   = "result"
	KindJob      = "job"
	KindDecision = "decision"
	KindDelta    = "delta"
)
