package gateway

import (
	"context"
	"errors"
	"sync/atomic"
	"time"
)

// ErrQueueFull is returned by Acquire when the wait queue is already at its
// bound; the caller should refuse the request rather than block.
var ErrQueueFull = errors.New("gateway: admission queue full")

// ErrQueueWait is returned by Acquire when a queued request waits longer than
// the configured wait window without a slot freeing.
var ErrQueueWait = errors.New("gateway: admission queue wait timed out")

// Limiter bounds how many inference requests reach the upstream engine at once,
// and how many may wait for a slot. It is admission control at the edge, not a
// batching engine: the compute plane (vLLM) does continuous batching far better
// than a proxy can, so goSpark's job is to keep the engine from being
// oversubscribed and to shed load predictably when it is.
//
// The zero value (and a nil *Limiter) admits everything, so the feature is
// opt-in via gateway.max_inflight.
type Limiter struct {
	maxInflight int
	maxQueued   int
	wait        time.Duration

	// slots is a token bag: a token is added on admission and removed on
	// release, so len(slots) is the in-flight count and cap(slots) the bound.
	slots chan struct{}

	queued   atomic.Int64
	admitted atomic.Uint64
	rejected atomic.Uint64
}

// NewLimiter builds a limiter. maxInflight <= 0 means unlimited (nil behaviour);
// maxQueued <= 0 means no queue (a full limiter refuses immediately). wait <= 0
// means a queued request waits until its context is cancelled.
func NewLimiter(maxInflight, maxQueued int, wait time.Duration) *Limiter {
	if maxInflight <= 0 {
		return nil
	}
	if maxQueued < 0 {
		maxQueued = 0
	}
	if wait < 0 {
		wait = 0
	}
	return &Limiter{
		maxInflight: maxInflight,
		maxQueued:   maxQueued,
		wait:        wait,
		slots:       make(chan struct{}, maxInflight),
	}
}

// Acquire takes a slot, waiting in the queue if the limiter is full. It returns
// a release func on success; the caller must call it exactly once. On refusal it
// returns ErrQueueFull (queue at its bound) or ErrQueueWait (waited too long),
// or the context error.
func (l *Limiter) Acquire(ctx context.Context) (func(), error) {
	if l == nil {
		return func() {}, nil
	}

	// Fast path: a slot is free.
	select {
	case l.slots <- struct{}{}:
		l.admitted.Add(1)
		return l.release, nil
	default:
	}

	// Slow path: queue, bounded by maxQueued.
	if l.queued.Add(1) > int64(l.maxQueued) {
		l.queued.Add(-1)
		l.rejected.Add(1)
		return nil, ErrQueueFull
	}
	taken := false
	defer func() {
		if !taken {
			l.queued.Add(-1)
		}
	}()

	var timeout <-chan time.Time
	if l.wait > 0 {
		t := time.NewTimer(l.wait)
		defer t.Stop()
		timeout = t.C
	}

	select {
	case l.slots <- struct{}{}:
		taken = true
		l.queued.Add(-1)
		l.admitted.Add(1)
		return l.release, nil
	case <-ctx.Done():
		l.rejected.Add(1)
		return nil, ctx.Err()
	case <-timeout:
		l.rejected.Add(1)
		return nil, ErrQueueWait
	}
}

// release returns the slot to the bag, handing it to a waiting acquirer.
func (l *Limiter) release() {
	<-l.slots
}

// Stats is a snapshot of the limiter for metrics.
type Stats struct {
	MaxInflight int
	MaxQueued   int
	Inflight    int
	Queued      int64
	Admitted    uint64
	Rejected    uint64
}

// Stats reports the current limiter state.
func (l *Limiter) Stats() Stats {
	if l == nil {
		return Stats{}
	}
	return Stats{
		MaxInflight: l.maxInflight,
		MaxQueued:   l.maxQueued,
		Inflight:    len(l.slots),
		Queued:      l.queued.Load(),
		Admitted:    l.admitted.Load(),
		Rejected:    l.rejected.Load(),
	}
}
