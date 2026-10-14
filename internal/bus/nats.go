package bus

import (
	"context"
	"fmt"
	"time"

	"github.com/nats-io/nats.go"
)

// NATS is the production Bus: a NATS connection with a JetStream context.
//
// JetStream gives the store-and-forward behaviour the design calls for. Control
// subjects are still plain pub/sub for low-latency heartbeats; durable job
// subjects use JetStream so a node that drops off the WireGuard mesh resumes
// where it left off instead of losing work.
type NATS struct {
	nc  *nats.Conn
	js  nats.JetStreamContext
	src string // subject root, for logging
}

// NATSOptions configures the connection.
type NATSOptions struct {
	URL      string
	Name     string
	Timeout  time.Duration
	Stream   string // JetStream stream to ensure (optional)
	Subjects []string
}

// Connect dials NATS and, if a stream name is given, ensures the stream exists
// over the supplied subjects.
func Connect(opts NATSOptions) (*NATS, error) {
	if opts.Timeout == 0 {
		opts.Timeout = 5 * time.Second
	}
	nc, err := nats.Connect(opts.URL,
		nats.Name(opts.Name),
		nats.Timeout(opts.Timeout),
		nats.RetryOnFailedConnect(true),
		nats.MaxReconnects(-1),
	)
	if err != nil {
		return nil, fmt.Errorf("bus: connect %s: %w", opts.URL, err)
	}
	n := &NATS{nc: nc}
	if opts.Stream != "" {
		js, err := nc.JetStream()
		if err != nil {
			nc.Close()
			return nil, fmt.Errorf("bus: jetstream: %w", err)
		}
		if err := n.ensureStream(js, opts.Stream, opts.Subjects); err != nil {
			nc.Close()
			return nil, err
		}
		n.js = js
	}
	return n, nil
}

func (n *NATS) ensureStream(js nats.JetStreamContext, name string, subjects []string) error {
	if len(subjects) == 0 {
		subjects = []string{"sparknet.>"}
	}
	_, err := js.StreamInfo(name)
	if err == nil {
		return nil
	}
	_, err = js.AddStream(&nats.StreamConfig{
		Name:      name,
		Subjects:  subjects,
		Storage:   nats.FileStorage,
		Retention: nats.LimitsPolicy,
		MaxAge:    24 * time.Hour,
	})
	if err != nil {
		return fmt.Errorf("bus: ensure stream %s: %w", name, err)
	}
	return nil
}

// Publish sends to subject over JetStream when available, else plain NATS.
func (n *NATS) Publish(ctx context.Context, subject string, data []byte) error {
	if n.js != nil {
		if _, err := n.js.Publish(subject, data, nats.Context(ctx)); err != nil {
			return fmt.Errorf("bus: js publish %s: %w", subject, err)
		}
		return nil
	}
	if err := n.nc.Publish(subject, data); err != nil {
		return fmt.Errorf("bus: publish %s: %w", subject, err)
	}
	return nil
}

// Subscribe registers a durable handler when JetStream is active.
func (n *NATS) Subscribe(_ context.Context, subject string, h Handler) (Subscription, error) {
	wrapped := func(msg *nats.Msg) {
		if err := h(context.Background(), msg.Subject, msg.Data); err != nil {
			_ = msg.Nak()
			return
		}
		_ = msg.Ack()
	}
	if n.js != nil {
		sub, err := n.js.QueueSubscribe(subject, "gospark", wrapped, nats.Durable("gospark-"+sanitise(subject)), nats.ManualAck())
		if err != nil {
			return nil, fmt.Errorf("bus: js subscribe %s: %w", subject, err)
		}
		return sub, nil
	}
	sub, err := n.nc.Subscribe(subject, wrapped)
	if err != nil {
		return nil, fmt.Errorf("bus: subscribe %s: %w", subject, err)
	}
	return sub, nil
}

// Request performs an RPC-style round trip.
func (n *NATS) Request(ctx context.Context, subject string, data []byte) ([]byte, error) {
	msg, err := n.nc.RequestWithContext(ctx, subject, data)
	if err != nil {
		return nil, fmt.Errorf("bus: request %s: %w", subject, err)
	}
	return msg.Data, nil
}

// Close drains and closes the connection.
func (n *NATS) Close() error {
	if n.nc == nil {
		return nil
	}
	return n.nc.Drain()
}

func sanitise(s string) string {
	out := make([]rune, 0, len(s))
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
			out = append(out, r)
		default:
			out = append(out, '_')
		}
	}
	return string(out)
}
