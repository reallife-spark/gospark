package bus

import (
	"context"
	"errors"
	"strings"
	"sync"
	"time"
)

type replyKey struct{}

// ReplyFn is retrieved from a request context to answer the requester.
type ReplyFn func(data []byte)

// Reply answers the request that delivered this context, if any.
func Reply(ctx context.Context, data []byte) bool {
	fn, ok := ctx.Value(replyKey{}).(ReplyFn)
	if !ok {
		return false
	}
	fn(data)
	return true
}

// Memory is an in-process Bus. It implements NATS-style subject matching with
// `*` (one token) and `>` (tail wildcard), so it exercises the same routing the
// live NATS bus uses.
type Memory struct {
	mu       sync.RWMutex
	nextID   int
	handlers map[int]memSub
	closed   bool
}

type memSub struct {
	pattern string
	h       Handler
}

// NewMemory builds an empty in-memory bus.
func NewMemory() *Memory {
	return &Memory{handlers: make(map[int]memSub)}
}

func (m *Memory) Publish(ctx context.Context, subject string, data []byte) error {
	m.mu.RLock()
	if m.closed {
		m.mu.RUnlock()
		return errors.New("bus: closed")
	}
	targets := make([]memSub, 0, len(m.handlers))
	for _, s := range m.handlers {
		if match(s.pattern, subject) {
			targets = append(targets, s)
		}
	}
	m.mu.RUnlock()
	var firstErr error
	for _, s := range targets {
		if err := s.h(ctx, subject, data); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

func (m *Memory) Subscribe(_ context.Context, subject string, h Handler) (Subscription, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return nil, errors.New("bus: closed")
	}
	m.nextID++
	id := m.nextID
	m.handlers[id] = memSub{pattern: subject, h: h}
	return Subscription(subscriptionFunc(func() error {
		m.mu.Lock()
		defer m.mu.Unlock()
		delete(m.handlers, id)
		return nil
	})), nil
}

func (m *Memory) Request(ctx context.Context, subject string, data []byte) ([]byte, error) {
	reply := make(chan []byte, 1)
	rctx := context.WithValue(ctx, replyKey{}, ReplyFn(func(b []byte) {
		select {
		case reply <- b:
		default:
		}
	}))
	if err := m.Publish(rctx, subject, data); err != nil {
		return nil, err
	}
	select {
	case b := <-reply:
		return b, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-time.After(5 * time.Second):
		return nil, errors.New("bus: request timed out")
	}
}

func (m *Memory) Close() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.closed = true
	m.handlers = make(map[int]memSub)
	return nil
}

type subscriptionFunc func() error

func (f subscriptionFunc) Unsubscribe() error { return f() }

// match implements NATS subject matching: `*` matches exactly one token, `>`
// matches one or more trailing tokens and must be last.
func match(pattern, subject string) bool {
	if pattern == subject {
		return true
	}
	p := strings.Split(pattern, ".")
	s := strings.Split(subject, ".")
	for i := 0; i < len(p); i++ {
		if p[i] == ">" {
			return i < len(s)
		}
		if i >= len(s) {
			return false
		}
		if p[i] == "*" {
			continue
		}
		if p[i] != s[i] {
			return false
		}
	}
	return len(p) == len(s)
}
