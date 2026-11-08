package kvtransfer

import "sync"

const (
	defaultMaxEntries = 4096
	defaultMaxBytes   = 256 << 20
)

type deltaKey struct {
	namespace string
	digest    string
}

// DeltaCache mirrors blocks by namespace so a node transfers only the blocks a
// peer lacks. Sending a whole context KV cache on every query is the failure
// mode this exists to avoid (see docs/transport.md, "semantic caching").
//
// It is bounded: BlockID is peer-chosen, so an unbounded map is a remote
// memory-exhaustion primitive. The cache evicts oldest entries first.
type DeltaCache struct {
	mu         sync.RWMutex
	store      map[string]map[string]*Block // namespace -> digest -> block
	order      []deltaKey
	sizes      map[deltaKey]int
	totalBytes int
	maxEntries int
	maxBytes   int
}

// NewDeltaCache builds a bounded cache with the deployment defaults.
func NewDeltaCache() *DeltaCache { return NewDeltaCacheWithLimits(defaultMaxEntries, defaultMaxBytes) }

// NewDeltaCacheWithLimits builds a cache with explicit bounds. Non-positive
// values fall back to the defaults.
func NewDeltaCacheWithLimits(maxEntries, maxBytes int) *DeltaCache {
	if maxEntries <= 0 {
		maxEntries = defaultMaxEntries
	}
	if maxBytes <= 0 {
		maxBytes = defaultMaxBytes
	}
	return &DeltaCache{
		store:      make(map[string]map[string]*Block),
		sizes:      make(map[deltaKey]int),
		maxEntries: maxEntries,
		maxBytes:   maxBytes,
	}
}

func blockSize(b *Block) int {
	return len(b.Packed) + len(b.BlockScale)*4 + 64
}

// Put stores a block under a namespace and returns its digest. It evicts the
// oldest entries until the cache is back inside its entry and byte bounds.
func (d *DeltaCache) Put(namespace string, b *Block) string {
	digest := b.Digest()
	key := deltaKey{namespace: namespace, digest: digest}
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.store[namespace] == nil {
		d.store[namespace] = make(map[string]*Block)
	}
	if _, exists := d.store[namespace][digest]; !exists {
		d.order = append(d.order, key)
	}
	d.store[namespace][digest] = b
	if old, ok := d.sizes[key]; ok {
		d.totalBytes -= old
	}
	size := blockSize(b)
	d.sizes[key] = size
	d.totalBytes += size
	for (len(d.sizes) > d.maxEntries || d.totalBytes > d.maxBytes) && len(d.order) > 0 {
		oldest := d.order[0]
		d.order = d.order[1:]
		if stored, ok := d.store[oldest.namespace][oldest.digest]; ok && stored == b && oldest == key {
			// Never evict the block just inserted; a single oversized block is
			// still one block and retrying the same Put would loop forever.
			d.order = append(d.order, oldest)
			break
		}
		if _, ok := d.store[oldest.namespace][oldest.digest]; !ok {
			continue
		}
		delete(d.store[oldest.namespace], oldest.digest)
		if d.sizes[oldest] > 0 {
			d.totalBytes -= d.sizes[oldest]
			delete(d.sizes, oldest)
		}
	}
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
