package bus_test

import (
	"context"
	"testing"
	"time"

	"github.com/reallife-spark/goSpark/internal/bus"
	"github.com/reallife-spark/goSpark/internal/testutil"
)

func TestNATSPublishSubscribe(t *testing.T) {
	url := testutil.NATSServer(t)
	ctx := context.Background()

	pub, err := bus.Connect(bus.NATSOptions{URL: url, Name: "pub"})
	if err != nil {
		t.Fatal(err)
	}
	defer pub.Close()
	sub, err := bus.Connect(bus.NATSOptions{URL: url, Name: "sub"})
	if err != nil {
		t.Fatal(err)
	}
	defer sub.Close()

	got := make(chan []byte, 1)
	if _, err := sub.Subscribe(ctx, "sptest.hello", func(_ context.Context, _ string, data []byte) error {
		got <- data
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	// Core NATS has no delivery guarantee; give the subscription a beat to
	// register on the server before publishing.
	time.Sleep(200 * time.Millisecond)
	if err := pub.Publish(ctx, "sptest.hello", []byte("world")); err != nil {
		t.Fatal(err)
	}
	select {
	case b := <-got:
		if string(b) != "world" {
			t.Fatalf("got %q", b)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("handler never fired over NATS")
	}
}

func TestNATSRequestReply(t *testing.T) {
	url := testutil.NATSServer(t)
	ctx := context.Background()

	responder, err := bus.Connect(bus.NATSOptions{URL: url, Name: "svc"})
	if err != nil {
		t.Fatal(err)
	}
	defer responder.Close()
	caller, err := bus.Connect(bus.NATSOptions{URL: url, Name: "caller"})
	if err != nil {
		t.Fatal(err)
	}
	defer caller.Close()

	if _, err := responder.Subscribe(ctx, "sptest.svc", func(ctx context.Context, _ string, data []byte) error {
		if !bus.Reply(ctx, append([]byte("echo:"), data...)) {
			t.Error("no reply function in context")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	time.Sleep(200 * time.Millisecond)

	rctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	rep, err := caller.Request(rctx, "sptest.svc", []byte("ping"))
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	if string(rep) != "echo:ping" {
		t.Fatalf("reply = %q", rep)
	}
}

func TestNATSQueueFanOutVsLoadBalance(t *testing.T) {
	url := testutil.NATSServer(t)
	ctx := context.Background()
	const stream = "SPQUEUE"
	const root = "spq"

	connect := func(name, queue string) *natsBus {
		t.Helper()
		b, err := bus.Connect(bus.NATSOptions{URL: url, Name: name, Queue: queue, Stream: stream, Subjects: []string{root + ".>"}})
		if err != nil {
			t.Fatalf("connect %s: %v", name, err)
		}
		t.Cleanup(func() { _ = b.Close() })
		return &natsBus{t: t, b: b}
	}

	// Fan-out: two connections with distinct names (no queue) each get a copy.
	a := connect("obs-a", "")
	b := connect("obs-b", "")
	gotA := a.watch(ctx, root+".fan")
	gotB := b.watch(ctx, root+".fan")
	time.Sleep(300 * time.Millisecond)
	if err := a.b.Publish(ctx, root+".fan", []byte("x")); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "fan-out a", gotA)
	waitFor(t, "fan-out b", gotB)

	// Load-balance: two connections sharing a queue deliver the message once.
	c := connect("worker-c", "workers")
	d := connect("worker-d", "workers")
	gotC := c.watch(ctx, root+".lb")
	gotD := d.watch(ctx, root+".lb")
	time.Sleep(300 * time.Millisecond)
	if err := c.b.Publish(ctx, root+".lb", []byte("once")); err != nil {
		t.Fatal(err)
	}
	select {
	case <-gotC:
		select {
		case <-gotD:
			t.Fatal("queue members both received the message; expected load-balance")
		case <-time.After(700 * time.Millisecond):
		}
	case <-gotD:
		select {
		case <-gotC:
			t.Fatal("queue members both received the message; expected load-balance")
		case <-time.After(700 * time.Millisecond):
		}
	case <-time.After(3 * time.Second):
		t.Fatal("neither queue member received the message")
	}
}

type natsBus struct {
	t *testing.T
	b *bus.NATS
}

func (n *natsBus) watch(ctx context.Context, subject string) <-chan struct{} {
	n.t.Helper()
	ch := make(chan struct{}, 1)
	if _, err := n.b.Subscribe(ctx, subject, func(_ context.Context, _ string, _ []byte) error {
		select {
		case ch <- struct{}{}:
		default:
		}
		return nil
	}); err != nil {
		n.t.Fatal(err)
	}
	return ch
}

func waitFor(t *testing.T, what string, ch <-chan struct{}) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(3 * time.Second):
		t.Fatalf("%s: no delivery", what)
	}
}

func TestNATSJetStreamStoreAndForward(t *testing.T) {
	url := testutil.NATSServer(t)
	ctx := context.Background()
	const stream = "SPTEST"
	const root = "sptestjs"

	pub, err := bus.Connect(bus.NATSOptions{URL: url, Name: "pub", Stream: stream, Subjects: []string{root + ".>"}})
	if err != nil {
		t.Fatal(err)
	}
	defer pub.Close()

	// Publish *before* any subscriber exists. JetStream must retain it and
	// deliver it to a late durable consumer — the store-and-forward property
	// the design needs for a node that drops off the mesh.
	if err := pub.Publish(ctx, root+".job", []byte("deferred")); err != nil {
		t.Fatalf("js publish: %v", err)
	}

	sub, err := bus.Connect(bus.NATSOptions{URL: url, Name: "sub", Stream: stream, Subjects: []string{root + ".>"}})
	if err != nil {
		t.Fatal(err)
	}
	defer sub.Close()

	got := make(chan []byte, 1)
	if _, err := sub.Subscribe(ctx, root+".job", func(_ context.Context, _ string, data []byte) error {
		select {
		case got <- data:
		default:
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	select {
	case b := <-got:
		if string(b) != "deferred" {
			t.Fatalf("got %q", b)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("JetStream did not deliver the retained message to the late consumer")
	}
}
