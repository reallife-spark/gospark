package gateway

import (
	"context"
	"errors"
	"testing"
	"time"
)

func waitQueued(t *testing.T, l *Limiter, want int64) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for l.Stats().Queued != want {
		if time.Now().After(deadline) {
			t.Fatalf("queued = %d, want %d", l.Stats().Queued, want)
		}
		time.Sleep(time.Millisecond)
	}
}

func TestNilLimiterAdmits(t *testing.T) {
	var l *Limiter
	rel, err := l.Acquire(context.Background())
	if err != nil {
		t.Fatalf("Acquire on nil limiter: %v", err)
	}
	rel()
}

func TestLimiterRefusesWhenQueueFull(t *testing.T) {
	l := NewLimiter(1, 0, 0)
	rel, err := l.Acquire(context.Background())
	if err != nil {
		t.Fatalf("first Acquire: %v", err)
	}
	defer rel()
	if _, err := l.Acquire(context.Background()); !errors.Is(err, ErrQueueFull) {
		t.Fatalf("second Acquire err = %v, want ErrQueueFull", err)
	}
	if got := l.Stats().Rejected; got != 1 {
		t.Fatalf("rejected = %d, want 1", got)
	}
}

func TestLimiterQueuesAndHandsOff(t *testing.T) {
	l := NewLimiter(1, 1, 0)
	rel1, err := l.Acquire(context.Background())
	if err != nil {
		t.Fatalf("first Acquire: %v", err)
	}

	type result struct {
		rel func()
		err error
	}
	ch := make(chan result, 1)
	go func() {
		rel, err := l.Acquire(context.Background())
		ch <- result{rel, err}
	}()
	waitQueued(t, l, 1)

	rel1() // hand the slot to the queued acquirer

	select {
	case r := <-ch:
		if r.err != nil {
			t.Fatalf("queued Acquire: %v", r.err)
		}
		if l.Stats().Queued != 0 {
			t.Fatalf("queued = %d after handoff, want 0", l.Stats().Queued)
		}
		r.rel()
	case <-time.After(2 * time.Second):
		t.Fatal("queued Acquire never returned")
	}
	if got := l.Stats().Admitted; got != 2 {
		t.Fatalf("admitted = %d, want 2", got)
	}
}

func TestLimiterQueueWaitTimesOut(t *testing.T) {
	l := NewLimiter(1, 1, 20*time.Millisecond)
	rel, err := l.Acquire(context.Background())
	if err != nil {
		t.Fatalf("first Acquire: %v", err)
	}
	defer rel()
	if _, err := l.Acquire(context.Background()); !errors.Is(err, ErrQueueWait) {
		t.Fatalf("queued Acquire err = %v, want ErrQueueWait", err)
	}
}

func TestLimiterQueueHonoursContext(t *testing.T) {
	l := NewLimiter(1, 1, 0)
	rel, err := l.Acquire(context.Background())
	if err != nil {
		t.Fatalf("first Acquire: %v", err)
	}
	defer rel()

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(10 * time.Millisecond)
		cancel()
	}()
	if _, err := l.Acquire(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("queued Acquire err = %v, want context.Canceled", err)
	}
}

func TestLimiterStats(t *testing.T) {
	l := NewLimiter(2, 4, 0)
	rel, err := l.Acquire(context.Background())
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	s := l.Stats()
	if s.MaxInflight != 2 || s.MaxQueued != 4 || s.Inflight != 1 || s.Admitted != 1 {
		t.Fatalf("stats = %+v", s)
	}
	rel()
	if got := l.Stats().Inflight; got != 0 {
		t.Fatalf("inflight = %d after release, want 0", got)
	}
}
