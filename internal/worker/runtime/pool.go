package runtime

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/aarani/hpcc/internal/protocol/gen"
)

// PooledRuntime wraps a Runtime with a per-tenant container pool. The
// first Start for a (TenantID, ImageDigest) pair forwards to the inner
// runtime; on subsequent Starts an idle pooled container — if any —
// is handed back instead, amortizing VM-boot cost across compiles.
//
// Containers returned to the caller are wrapped: their Stop releases
// to the pool rather than tearing the inner container down. The reaper
// goroutine evicts entries on two clocks:
//
//   - idleTTL — parked entries older than this since their last park
//     are stale and get torn down.
//   - maxLifetime — entries whose original (cold-start) age exceeds
//     this are torn down regardless of how recently they were used.
//     This is §4.2's "hard session timeout (e.g. shift change, N
//     hours)": long-lived per-tenant state accumulates and someone
//     will eventually ask what's in it.
//
// A maxParked cap evicts the oldest parked container when a new one
// would push the pool over the limit. Close drains everything.
//
// Key choice — (TenantID, ImageDigest) — is what defines a reusable
// session in §4.4 of the design. VCPUs/MemoryBytes are worker-global
// (driven by cfg.VM) so they don't enter the key. Per-RPC source and
// output mount paths live on ExecRequest, not ContainerSpec, so they
// don't enter the key either — that's what makes pooling tractable.
type PooledRuntime struct {
	inner       Runtime
	idleTTL     time.Duration // 0 = no idle reaping
	maxLifetime time.Duration // 0 = no hard session timeout
	maxParked   int           // 0 = unlimited

	mu          sync.Mutex
	pool        map[string][]*pooledEntry // key: poolKey(spec)
	parkedCount int                       // total entries across all keys
	closed      bool

	stop chan struct{} // closed by Close to wake the reaper
	wg   sync.WaitGroup
}

type pooledEntry struct {
	container Container
	parkedAt  time.Time
	createdAt time.Time // when the inner runtime first started this container
}

// NewPooledRuntime wraps inner and starts the reaper if either timeout
// is enabled.
//
// idleTTL is the maximum time a parked container may sit in the pool
// before it gets evicted; pass 0 to disable idle reaping.
//
// maxLifetime is the hard ceiling on a container's wall-clock age,
// measured from its original cold start. Past this, the container is
// torn down at the next pop or park regardless of how recently it was
// used. Pass 0 to disable.
//
// maxParked is the upper bound on parked containers across all keys;
// when a park would exceed it, the oldest parked container is evicted
// first. Pass 0 for unlimited (the dev/test default).
func NewPooledRuntime(inner Runtime, idleTTL, maxLifetime time.Duration, maxParked int) *PooledRuntime {
	p := &PooledRuntime{
		inner:       inner,
		idleTTL:     idleTTL,
		maxLifetime: maxLifetime,
		maxParked:   maxParked,
		pool:        map[string][]*pooledEntry{},
		stop:        make(chan struct{}),
	}
	if interval := p.reaperInterval(); interval > 0 {
		p.wg.Add(1)
		go p.reaper(interval)
	}
	return p
}

// reaperInterval picks the wake cadence for the reaper. It runs at
// half the smaller of the two enabled clocks so worst-case dwell time
// past either deadline is at most 1.5x. Returns 0 when neither clock
// is configured (caller skips starting the goroutine).
func (p *PooledRuntime) reaperInterval() time.Duration {
	var ttl time.Duration
	switch {
	case p.idleTTL > 0 && p.maxLifetime > 0:
		ttl = p.idleTTL
		if p.maxLifetime < ttl {
			ttl = p.maxLifetime
		}
	case p.idleTTL > 0:
		ttl = p.idleTTL
	case p.maxLifetime > 0:
		ttl = p.maxLifetime
	default:
		return 0
	}
	return ttl / 2
}

// Start returns a pooled container if one is available for spec's
// (TenantID, ImageDigest); otherwise it forwards to the inner runtime.
// The caller gets a wrapper whose Stop releases back to the pool.
//
// On a warm hit, createdAt rides through from the popped entry into
// the new wrapper so the maxLifetime clock is anchored on the original
// cold start, not the latest reuse — that's what makes "hard session
// timeout" actually hard.
func (p *PooledRuntime) Start(ctx context.Context, spec ContainerSpec) (Container, error) {
	key := poolKey(spec)

	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return nil, fmt.Errorf("pool: closed")
	}
	if entry := p.popLocked(key); entry != nil {
		p.mu.Unlock()
		return &pooledContainer{inner: entry.container, pool: p, key: key, createdAt: entry.createdAt}, nil
	}
	p.mu.Unlock()

	c, err := p.inner.Start(ctx, spec)
	if err != nil {
		return nil, err
	}
	return &pooledContainer{inner: c, pool: p, key: key, createdAt: time.Now()}, nil
}

// Close drains the pool — every parked container gets a real Stop —
// and forwards to the inner runtime's Close. After Close the pool
// rejects further Starts; outstanding pooledContainers will fall
// through to the inner Stop on their next Stop call.
func (p *PooledRuntime) Close() error {
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return p.inner.Close()
	}
	p.closed = true
	close(p.stop)
	pools := p.pool
	p.pool = map[string][]*pooledEntry{}
	p.parkedCount = 0
	p.mu.Unlock()

	p.wg.Wait()

	for _, list := range pools {
		for _, e := range list {
			_ = e.container.Stop(context.Background())
		}
	}
	return p.inner.Close()
}

// popLocked removes and returns the most recently parked entry for
// key, or nil if the slot is empty. Stale entries (idle past idleTTL
// or alive past maxLifetime) are torn down inline and the loop keeps
// looking. Called with p.mu held.
func (p *PooledRuntime) popLocked(key string) *pooledEntry {
	list := p.pool[key]
	for len(list) > 0 {
		entry := list[len(list)-1]
		list = list[:len(list)-1]
		p.parkedCount--
		if p.entryExpired(entry) {
			// Stale — drop it and keep looking. Stop async to keep
			// the lock window short.
			go entry.container.Stop(context.Background())
			continue
		}
		if len(list) == 0 {
			delete(p.pool, key)
		} else {
			p.pool[key] = list
		}
		return entry
	}
	if len(list) == 0 {
		delete(p.pool, key)
	}
	return nil
}

// entryExpired returns true when entry has crossed either configured
// deadline. Pure read; safe to call under p.mu or off it as long as
// the entry isn't being mutated concurrently.
func (p *PooledRuntime) entryExpired(entry *pooledEntry) bool {
	now := time.Now()
	if p.idleTTL > 0 && now.Sub(entry.parkedAt) > p.idleTTL {
		return true
	}
	if p.maxLifetime > 0 && now.Sub(entry.createdAt) > p.maxLifetime {
		return true
	}
	return false
}

// park returns a container to the pool. If the pool is closed, the
// container is stopped instead of parked (worker is shutting down).
// If the container's wall-clock age has crossed maxLifetime, it is
// stopped here rather than re-parked — that's how the hard session
// timeout actually evicts long-lived state. If the pool is at
// capacity, the globally-oldest parked container is evicted to make
// room — bounding total parked count regardless of how many distinct
// (tenant, image) pairs the worker sees.
func (p *PooledRuntime) park(c Container, key string, createdAt time.Time) {
	if p.maxLifetime > 0 && time.Since(createdAt) > p.maxLifetime {
		// Hard session timeout: don't re-park, tear down. Stop
		// outside any lock since the inner runtime may block.
		_ = c.Stop(context.Background())
		return
	}

	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		_ = c.Stop(context.Background())
		return
	}

	var evicted Container
	if p.maxParked > 0 && p.parkedCount >= p.maxParked {
		evicted = p.evictOldestLocked()
	}

	p.pool[key] = append(p.pool[key], &pooledEntry{
		container: c,
		parkedAt:  time.Now(),
		createdAt: createdAt,
	})
	p.parkedCount++
	p.mu.Unlock()

	if evicted != nil {
		// Stop outside the lock — the inner runtime may block.
		_ = evicted.Stop(context.Background())
	}
}

// evictOldestLocked finds and removes the entry with the smallest
// parkedAt across all keys. Returns its container so the caller can
// Stop it after dropping the lock; nil if the pool is empty. Called
// with p.mu held.
//
// Linear scan over all parked entries — fine at the scales we expect
// (MaxActive on the order of tens). If pool size ever grows enough
// for this to matter, switch to a heap.
func (p *PooledRuntime) evictOldestLocked() Container {
	var (
		oldestKey  string
		oldestIdx  = -1
		oldestTime time.Time
	)
	for k, list := range p.pool {
		for i, e := range list {
			if oldestIdx == -1 || e.parkedAt.Before(oldestTime) {
				oldestKey = k
				oldestIdx = i
				oldestTime = e.parkedAt
			}
		}
	}
	if oldestIdx == -1 {
		return nil
	}
	list := p.pool[oldestKey]
	out := list[oldestIdx].container
	p.pool[oldestKey] = append(list[:oldestIdx], list[oldestIdx+1:]...)
	if len(p.pool[oldestKey]) == 0 {
		delete(p.pool, oldestKey)
	}
	p.parkedCount--
	return out
}

// reaper periodically evicts entries that have crossed either
// idleTTL or maxLifetime. interval is set by reaperInterval so worst-
// case dwell past either deadline is at most 1.5x.
func (p *PooledRuntime) reaper(interval time.Duration) {
	defer p.wg.Done()
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-p.stop:
			return
		case <-t.C:
			p.evictExpired()
		}
	}
}

func (p *PooledRuntime) evictExpired() {
	var toClose []Container
	p.mu.Lock()
	for k, list := range p.pool {
		kept := list[:0]
		for _, e := range list {
			if p.entryExpired(e) {
				toClose = append(toClose, e.container)
				p.parkedCount--
			} else {
				kept = append(kept, e)
			}
		}
		if len(kept) == 0 {
			delete(p.pool, k)
		} else {
			p.pool[k] = kept
		}
	}
	p.mu.Unlock()

	for _, c := range toClose {
		_ = c.Stop(context.Background())
	}
}

func poolKey(spec ContainerSpec) string {
	return spec.TenantID + "|" + spec.ImageDigest
}

// pooledContainer wraps a Container so its Stop releases to the pool
// rather than tearing the inner container down. ID/TenantID/etc. and
// Exec are pure pass-throughs. createdAt rides with the wrapper so
// the maxLifetime clock survives every park/pop cycle.
type pooledContainer struct {
	inner     Container
	pool      *PooledRuntime
	key       string
	createdAt time.Time // when the inner runtime first started this container

	stoppedOnce sync.Once
}

func (c *pooledContainer) ID() string          { return c.inner.ID() }
func (c *pooledContainer) TenantID() string    { return c.inner.TenantID() }
func (c *pooledContainer) ImageDigest() string { return c.inner.ImageDigest() }
func (c *pooledContainer) State() gen.VMState  { return c.inner.State() }

func (c *pooledContainer) Exec(ctx context.Context, req ExecRequest) (ExecResult, error) {
	return c.inner.Exec(ctx, req)
}

// Stop returns the underlying container to the pool. Only the first
// call to Stop has effect — subsequent ones are no-ops, which matches
// the "Idempotent" promise on the Container interface.
func (c *pooledContainer) Stop(_ context.Context) error {
	c.stoppedOnce.Do(func() { c.pool.park(c.inner, c.key, c.createdAt) })
	return nil
}
