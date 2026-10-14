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
	nc    *nats.Conn
	js    nats.JetStreamContext
	name  string // connection name: the consumer identity
	queue string // optional queue group: subscribers sharing it load-balance
}

// NATSOptions configures the connection.
type NATSOptions struct {
	URL      string
	Name     string
	Timeout  time.Duration
	Stream   string // JetStream stream to ensure (optional)
	Subjects []string
	// Queue, when set, makes all connections that share it load-balance their
	// deliveries instead of each receiving a copy. Two HA control-plane
	// replicas set the same Queue; independent observers leave it empty so each
	// gets its own copy (fan-out).
	Queue string
}

// consumerID is the identity under which JetStream consumers are named. With a
// queue it is the queue (so members share one consumer); without, it is the
// connection name (so distinct connections fan out).
func (n *NATS) consumerID() string {
	if n.queue != "" {
		return n.queue
	}
	if n.name != "" {
		return n.name
	}
	return "anon"
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
	n := &NATS{nc: nc, name: opts.Name, queue: opts.Queue}
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
//
// The handler receives a context carrying a ReplyFn when the message has a
// reply subject, so bus.Reply works identically on NATS and the Memory bus.
// (JetStream messages have no reply subject; Request/Reply is a core-NATS
// feature and is only available on a bus connected without a stream.)
func (n *NATS) Subscribe(_ context.Context, subject string, h Handler) (Subscription, error) {
	wrapped := func(msg *nats.Msg) {
		ctx := context.Background()
		if msg.Reply != "" {
			nc, reply := n.nc, msg.Reply
			ctx = context.WithValue(ctx, replyKey{}, ReplyFn(func(b []byte) {
				_ = nc.Publish(reply, b)
			}))
		}
		if err := h(ctx, msg.Subject, msg.Data); err != nil {
			if n.js != nil {
				_ = msg.Nak()
			}
			return
		}
		if n.js != nil {
			_ = msg.Ack()
		}
	}
	if n.js != nil {
		durable := "gospark_" + sanitise(n.consumerID()) + "_" + sanitise(subject)
		var (
			sub Subscription
			err error
		)
		if n.queue != "" {
			sub, err = n.js.QueueSubscribe(subject, n.queue, wrapped, nats.Durable(durable), nats.ManualAck())
		} else {
			sub, err = n.js.Subscribe(subject, wrapped, nats.Durable(durable), nats.ManualAck())
		}
		if err != nil {
			return nil, fmt.Errorf("bus: js subscribe %s: %w", subject, err)
		}
		return sub, nil
	}
	var (
		sub Subscription
		err error
	)
	if n.queue != "" {
		sub, err = n.nc.QueueSubscribe(subject, n.queue, wrapped)
	} else {
		sub, err = n.nc.Subscribe(subject, wrapped)
	}
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
