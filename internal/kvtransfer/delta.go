package kvtransfer

import "sync"

// DeltaCache mirrors blocks by namespace so a node transfers only the blocks a
// peer lacks. Sending a whole context KV cache on every query is the failure
// mode this exists to avoid (see docs/transport.md, "semantic caching").
type DeltaCache struct {
	mu    sync.RWMutex
	store map[string]map[string]*Block // namespace -> digest -> block
}

// NewDeltaCache builds an empty cache.
func NewDeltaCache() *DeltaCache {
	return &DeltaCache{store: make(map[string]map[string]*Block)}
}

// Put stores a block under a namespace and returns its digest.
func (d *DeltaCache) Put(namespace string, b *Block) string {
	digest := b.Digest()
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.store[namespace] == nil {
		d.store[namespace] = make(map[string]*Block)
	}
	d.store[namespace][digest] = b
	return digest
}

// Get returns a stored block.
func (d *DeltaCache) Get(namespace, digest string) (*Block, bool) {
	d.mu.RLock()
	defer d.mu.RUnlock()
	b, ok := d.store[namespace][digest]
	return b, ok
}

// Have reports whether the namespace holds the digest.
func (d *DeltaCache) Have(namespace, digest string) bool {
	_, ok := d.Get(namespace, digest)
	return ok
}

// Missing returns the digests the namespace lacks, in input order.
func (d *DeltaCache) Missing(namespace string, want []string) []string {
	d.mu.RLock()
	defer d.mu.RUnlock()
	var out []string
	for _, dig := range want {
		if _, ok := d.store[namespace][dig]; !ok {
			out = append(out, dig)
		}
	}
	return out
}

// ResolveDelta returns the blocks a peer should be sent given what it already
// has. The peer's "have" list is treated as authoritative for its own store.
func (d *DeltaCache) ResolveDelta(namespace string, peerHave []string) []*Block {
	have := make(map[string]struct{}, len(peerHave))
	for _, h := range peerHave {
		have[h] = struct{}{}
	}
	d.mu.RLock()
	defer d.mu.RUnlock()
	var out []*Block
	for _, b := range d.store[namespace] {
		if _, ok := have[b.Digest()]; !ok {
			out = append(out, b)
		}
	}
	return out
}

// Len reports how many blocks a namespace holds.
func (d *DeltaCache) Len(namespace string) int {
	d.mu.RLock()
	defer d.mu.RUnlock()
	return len(d.store[namespace])
}
