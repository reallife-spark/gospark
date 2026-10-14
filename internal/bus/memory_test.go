package bus_test

import (
	"context"
	"testing"
	"time"

	"github.com/reallife-spark/goSpark/internal/bus"
)

func TestSubjectHelpers(t *testing.T) {
	if got := bus.Subject("", "task", ""); got != "sparknet.task" {
		t.Errorf("default root: %q", got)
	}
	if got := bus.Subject("sparknet", "task", "node-a"); got != "sparknet.task.node-a" {
		t.Errorf("with node: %q", got)
	}
}

func TestPublishSubscribeExact(t *testing.T) {
	m := bus.NewMemory()
	defer m.Close()

	got := make(chan []byte, 1)
	if _, err := m.Subscribe(context.Background(), "sparknet.task.node-a", func(_ context.Context, _ string, data []byte) error {
		got <- data
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := m.Publish(context.Background(), "sparknet.task.node-a", []byte("hi")); err != nil {
		t.Fatal(err)
	}
	select {
	case b := <-got:
		if string(b) != "hi" {
			t.Fatalf("got %q", b)
		}
	case <-time.After(time.Second):
		t.Fatal("handler not called")
	}
}

func TestWildcardMatch(t *testing.T) {
	m := bus.NewMemory()
	defer m.Close()

	star := make(chan string, 2)
	tail := make(chan string, 2)
	if _, err := m.Subscribe(context.Background(), "sparknet.task.*", func(_ context.Context, s string, _ []byte) error {
		star <- s
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Subscribe(context.Background(), "sparknet.>", func(_ context.Context, s string, _ []byte) error {
		tail <- s
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	ctx := context.Background()
	_ = m.Publish(ctx, "sparknet.task.node-a", nil)
	_ = m.Publish(ctx, "sparknet.result", nil) // matches >, not * (two tokens)

	// star should have only the node-a subject.
	select {
	case s := <-star:
		if s != "sparknet.task.node-a" {
			t.Fatalf("star got %q", s)
		}
	case <-time.After(time.Second):
		t.Fatal("star wildcard did not fire")
	}
	if len(star) != 0 {
		t.Fatal("star wildcard over-matched")
	}

	// tail should have seen both.
	seen := map[string]bool{}
	for i := 0; i < 2; i++ {
		select {
		case s := <-tail:
			seen[s] = true
		case <-time.After(time.Second):
			t.Fatalf("tail wildcard saw only %v", seen)
		}
	}
	if !seen["sparknet.task.node-a"] || !seen["sparknet.result"] {
		t.Fatalf("tail missing subjects: %v", seen)
	}
}

func TestUnsubscribe(t *testing.T) {
	m := bus.NewMemory()
	defer m.Close()
	calls := 0
	sub, err := m.Subscribe(context.Background(), "x", func(_ context.Context, _ string, _ []byte) error {
		calls++
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	_ = m.Publish(context.Background(), "x", nil)
	if err := sub.Unsubscribe(); err != nil {
		t.Fatal(err)
	}
	_ = m.Publish(context.Background(), "x", nil)
	if calls != 1 {
		t.Fatalf("calls = %d, want 1", calls)
	}
}

func TestRequestReply(t *testing.T) {
	m := bus.NewMemory()
	defer m.Close()
	if _, err := m.Subscribe(context.Background(), "svc", func(ctx context.Context, _ string, data []byte) error {
		bus.Reply(ctx, append([]byte("echo:"), data...))
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	rep, err := m.Request(ctx, "svc", []byte("ping"))
	if err != nil {
		t.Fatal(err)
	}
	if string(rep) != "echo:ping" {
		t.Fatalf("reply = %q", rep)
	}
}

func TestClosedBusRefuses(t *testing.T) {
	m := bus.NewMemory()
	if err := m.Close(); err != nil {
		t.Fatal(err)
	}
	if err := m.Publish(context.Background(), "x", nil); err == nil {
		t.Fatal("expected error publishing to a closed bus")
	}
}
