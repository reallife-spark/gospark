package authz

import (
	"sync"
	"time"
)

// NonceStore records spent nonces so a captured envelope cannot be replayed.
// It is retained at least as long as the longest task TTL — the same rule the
// fabric applies to grants.
type NonceStore interface {
	// Use records nonce and reports whether it was fresh. A second call with
	// the same nonce returns (false, nil). expiry may be zero for "no TTL".
	Use(nonce string, expiry time.Time) (bool, error)
}

// MemoryNonceStore is an in-process store. The control plane swaps it for a
// persistent backend in a later pass; the interface is what callers depend on.
type MemoryNonceStore struct {
	mu     sync.Mutex
	seen   map[string]time.Time // nonce -> expiry (zero = never)
	now    Clock
	maxAge time.Duration
}

// NewMemoryNonceStore builds a store. now may be nil. maxAge bounds retention
// for nonces with no explicit expiry (0 = keep until process exit).
func NewMemoryNonceStore(now Clock, maxAge time.Duration) *MemoryNonceStore {
	return &MemoryNonceStore{seen: make(map[string]time.Time), now: now, maxAge: maxAge}
}

// Use implements NonceStore.
func (m *MemoryNonceStore) Use(nonce string, expiry time.Time) (bool, error) {
	if nonce == "" {
		return false, nil
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.sweepLocked()
	if _, ok := m.seen[nonce]; ok {
		return false, nil
	}
	if expiry.IsZero() && m.maxAge > 0 {
		expiry = m.currentTime().Add(m.maxAge)
	}
	m.seen[nonce] = expiry
	return true, nil
}

func (m *MemoryNonceStore) currentTime() time.Time {
	if m.now != nil {
		return m.now()
	}
	return time.Now()
}

func (m *MemoryNonceStore) sweepLocked() {
	now := m.currentTime()
	for n, exp := range m.seen {
		if !exp.IsZero() && now.After(exp) {
			delete(m.seen, n)
		}
	}
}
