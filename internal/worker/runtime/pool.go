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
// runtime; subsequent Starts return a wrapper around an already-running
// container — refcounted, so up to VM.VCPUs concurrent Execs can share
// one VM. Releasing (wrapper.Stop) decrements the refcount; the entry
// stays in the pool while idle and the reaper evicts it on two clocks:
//
//   - idleTTL — entries whose refcount has been zero for longer than
//     this are torn down. (Entries with refs > 0 are never idle-aged.)
//   - maxLifetime — entries whose original (cold-start) age exceeds
//     this are torn down once their refcount drops to zero. This is
//     §4.2's "hard session timeout": long-lived per-tenant state
//     accumulates and someone will eventually ask what's in it. The
//     ceiling is also enforced at Start time — over-aged entries are
//     never handed back out.
//
// A maxEntries cap evicts the oldest idle container when a new one
// would push the pool over the limit. Close drains everything, waiting
// for in-flight Execs to finish first.
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
	maxEntries  int           // 0 = unlimited

	mu      sync.Mutex
	cond    *sync.Cond
	entries map[string][]*pooledEntry // key: poolKey(spec)
	total   int                       // total entries across all keys
	closed  bool

	stop chan struct{} // closed by Close to wake the reaper
	wg   sync.WaitGroup
}

// pooledEntry is one running container in the pool. refs counts how
// many wrappers are currently holding it (each in-flight Exec dispatch
// equals one). maxRefs is the per-VM concurrency cap — copied from
// spec.VCPUs at start. lastFreeT is the wall-clock moment refs last
// hit zero and serves as the anchor for idleTTL.
type pooledEntry struct {
	container Container
	createdAt time.Time
	refs      int
	maxRefs   int
	lastFreeT time.Time
}

// NewPooledRuntime wraps inner and starts the reaper if either timeout
// is enabled.
//
// idleTTL is the maximum time an entry whose refcount has been zero
// may sit in the pool before it gets evicted; pass 0 to disable idle
// reaping. Entries with refs > 0 are never idle-aged.
//
// maxLifetime is the hard ceiling on a container's wall-clock age,
// measured from its original cold start. Past this, the container is
// refused as a Start hit and torn down once its refcount drops to zero
// (or evicted by the reaper on its next tick). Pass 0 to disable.
//
// maxEntries is the upper bound on total entries (running containers)
// across all keys; when a fresh Start would exceed it, the oldest idle
// container is evicted first. Pass 0 for unlimited.
func NewPooledRuntime(inner Runtime, idleTTL, maxLifetime time.Duration, maxEntries int) *PooledRuntime {
	p := &PooledRuntime{
		inner:       inner,
		idleTTL:     idleTTL,
		maxLifetime: maxLifetime,
		maxEntries:  maxEntries,
		entries:     map[string][]*pooledEntry{},
		stop:        make(chan struct{}),
	}
	p.cond = sync.NewCond(&p.mu)
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

// Start returns a wrapper around either an existing entry for spec's
// (TenantID, ImageDigest) — refcount incremented — or, if no entry has
// capacity, a freshly-booted container. The caller's wrapper.Stop
// releases the refcount.
//
// maxRefs on a fresh entry is taken from spec.VCPUs (clamped to >=1).
// All Starts for the same key carry the same VCPUs in practice (the
// worker derives spec.VCPUs from cfg.VM.VCPUs), so the cap is stable.
func (p *PooledRuntime) Start(ctx context.Context, spec ContainerSpec) (Container, error) {
	key := poolKey(spec)

	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return nil, fmt.Errorf("pool: closed")
	}

	// Prune over-aged or idle-aged free entries for this key inline so
	// we don't hand back something the reaper is about to claim and so
	// stale entries don't pile up between reaper ticks.
	stale := p.pruneStaleLocked(key)

	for _, e := range p.entries[key] {
		if p.entryUsableLocked(e) {
			e.refs++
			p.mu.Unlock()
			stopAll(stale)
			return newPooledContainer(p, key, e), nil
		}
	}

	// Miss: need a fresh container. Honour maxEntries by evicting an
	// idle entry if we'd otherwise grow past the cap.
	var evicted Container
	if p.maxEntries > 0 && p.total >= p.maxEntries {
		evicted = p.evictOldestFreeLocked()
	}
	p.mu.Unlock()

	stopAll(stale)
	if evicted != nil {
		_ = evicted.Stop(context.Background())
	}

	c, err := p.inner.Start(ctx, spec)
	if err != nil {
		return nil, err
	}

	maxRefs := int(spec.VCPUs)
	if maxRefs < 1 {
		maxRefs = 1
	}
	now := time.Now()
	entry := &pooledEntry{
		container: c,
		createdAt: now,
		refs:      1,
		maxRefs:   maxRefs,
		lastFreeT: now,
	}

	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		_ = c.Stop(context.Background())
		return nil, fmt.Errorf("pool: closed")
	}
	p.entries[key] = append(p.entries[key], entry)
	p.total++
	p.mu.Unlock()

	return newPooledContainer(p, key, entry), nil
}

// Close drains the pool — every entry's container gets a real Stop —
// and forwards to the inner runtime's Close. Blocks until all
// outstanding wrappers release (refs == 0 everywhere). After Close the
// pool rejects further Starts; outstanding pooledContainers stop being
// reference-counted and fall through to a no-op on their next Stop.
func (p *PooledRuntime) Close() error {
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return p.inner.Close()
	}
	p.closed = true
	close(p.stop)
	for p.outstandingLocked() {
		p.cond.Wait()
	}
	entries := p.entries
	p.entries = map[string][]*pooledEntry{}
	p.total = 0
	p.mu.Unlock()

	p.wg.Wait()

	for _, list := range entries {
		for _, e := range list {
			_ = e.container.Stop(context.Background())
		}
	}
	return p.inner.Close()
}

// outstandingLocked reports whether any entry still has refs > 0.
// Called with p.mu held.
func (p *PooledRuntime) outstandingLocked() bool {
	for _, list := range p.entries {
		for _, e := range list {
			if e.refs > 0 {
				return true
			}
		}
	}
	return false
}

// pruneStaleLocked removes refs==0 entries for key that have crossed
// either configured deadline. Returns their containers so the caller
// can Stop them outside the lock. Called with p.mu held.
func (p *PooledRuntime) pruneStaleLocked(key string) []Container {
	list := p.entries[key]
	if len(list) == 0 {
		return nil
	}
	var stale []Container
	kept := list[:0]
	for _, e := range list {
		if p.canReapLocked(e) {
			stale = append(stale, e.container)
			p.total--
			continue
		}
		kept = append(kept, e)
	}
	if len(kept) == 0 {
		delete(p.entries, key)
	} else {
		p.entries[key] = kept
	}
	return stale
}

// entryUsableLocked reports whether an entry can absorb one more Exec
// right now: refcount below cap, alive within maxLifetime. An entry
// past maxLifetime is never reused even if its refcount has not yet
// dropped to zero — the next compile gets a fresh VM, and the over-
// aged entry tears down once outstanding refs release. Called with
// p.mu held.
func (p *PooledRuntime) entryUsableLocked(e *pooledEntry) bool {
	if e.refs >= e.maxRefs {
		return false
	}
	if p.maxLifetime > 0 && time.Since(e.createdAt) > p.maxLifetime {
		return false
	}
	return true
}

// canReapLocked reports whether an entry is currently a teardown
// candidate. Both clocks gate on refs == 0 — a live Exec keeps its VM
// alive past either deadline, and the next Start for the key will
// route around the over-aged entry rather than killing in-flight work.
// Called with p.mu held.
func (p *PooledRuntime) canReapLocked(e *pooledEntry) bool {
	if e.refs > 0 {
		return false
	}
	if p.idleTTL > 0 && time.Since(e.lastFreeT) > p.idleTTL {
		return true
	}
	if p.maxLifetime > 0 && time.Since(e.createdAt) > p.maxLifetime {
		return true
	}
	return false
}

// release decrements entry.refs. On a transition to zero it stamps
// lastFreeT and, if the entry has crossed maxLifetime, evicts it
// inline so we don't park a session past the hard ceiling waiting for
// the reaper. Either way, broadcast so Close can re-check.
func (p *PooledRuntime) release(key string, entry *pooledEntry) {
	var toStop Container
	p.mu.Lock()
	entry.refs--
	if entry.refs == 0 {
		entry.lastFreeT = time.Now()
		if p.maxLifetime > 0 && time.Since(entry.createdAt) > p.maxLifetime {
			p.removeEntryLocked(key, entry)
			toStop = entry.container
		}
		p.cond.Broadcast()
	}
	p.mu.Unlock()
	if toStop != nil {
		_ = toStop.Stop(context.Background())
	}
}

// removeEntryLocked deletes entry from p.entries[key] and decrements
// p.total. No-op if entry is no longer present. Called with p.mu held.
func (p *PooledRuntime) removeEntryLocked(key string, entry *pooledEntry) {
	list := p.entries[key]
	for i, e := range list {
		if e == entry {
			p.entries[key] = append(list[:i], list[i+1:]...)
			if len(p.entries[key]) == 0 {
				delete(p.entries, key)
			}
			p.total--
			return
		}
	}
}

// evictOldestFreeLocked finds and removes the entry with the smallest
// lastFreeT across all keys among entries with refs == 0. Returns its
// container so the caller can Stop it after dropping the lock; nil if
// no idle entry exists (in which case Start grows past maxEntries
// rather than tearing down work in progress). Called with p.mu held.
//
// Linear scan over all entries — fine at the scales we expect
// (maxEntries on the order of tens). If pool size ever grows enough
// for this to matter, switch to a heap.
func (p *PooledRuntime) evictOldestFreeLocked() Container {
	var (
		oldestKey  string
		oldestIdx  = -1
		oldestTime time.Time
	)
	for k, list := range p.entries {
		for i, e := range list {
			if e.refs != 0 {
				continue
			}
			if oldestIdx == -1 || e.lastFreeT.Before(oldestTime) {
				oldestKey = k
				oldestIdx = i
				oldestTime = e.lastFreeT
			}
		}
	}
	if oldestIdx == -1 {
		return nil
	}
	list := p.entries[oldestKey]
	out := list[oldestIdx].container
	p.entries[oldestKey] = append(list[:oldestIdx], list[oldestIdx+1:]...)
	if len(p.entries[oldestKey]) == 0 {
		delete(p.entries, oldestKey)
	}
	p.total--
	return out
}

// reaper periodically evicts reapable entries. interval is set by
// reaperInterval so worst-case dwell past either deadline is at most
// 1.5x.
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
	for k, list := range p.entries {
		kept := list[:0]
		for _, e := range list {
			if p.canReapLocked(e) {
				toClose = append(toClose, e.container)
				p.total--
				continue
			}
			kept = append(kept, e)
		}
		if len(kept) == 0 {
			delete(p.entries, k)
		} else {
			p.entries[k] = kept
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

// stopAll fires Stop on every container in cs. Used for inline cleanup
// in Start, where we accumulate stale containers under the lock and
// tear them down after releasing it.
func stopAll(cs []Container) {
	for _, c := range cs {
		_ = c.Stop(context.Background())
	}
}

// pooledContainer is the per-wrapper handle returned by Start. Stop
// decrements the entry's refcount once; ID/State/Exec pass straight
// through to the shared underlying container. Multiple wrappers may
// point at the same entry concurrently.
type pooledContainer struct {
	pool        *PooledRuntime
	entry       *pooledEntry
	key         string
	stoppedOnce sync.Once
}

func newPooledContainer(p *PooledRuntime, key string, e *pooledEntry) *pooledContainer {
	return &pooledContainer{pool: p, entry: e, key: key}
}

func (c *pooledContainer) ID() string          { return c.entry.container.ID() }
func (c *pooledContainer) TenantID() string    { return c.entry.container.TenantID() }
func (c *pooledContainer) ImageDigest() string { return c.entry.container.ImageDigest() }
func (c *pooledContainer) State() gen.VMState  { return c.entry.container.State() }

func (c *pooledContainer) Exec(ctx context.Context, req ExecRequest) (ExecResult, error) {
	return c.entry.container.Exec(ctx, req)
}

// Stop releases this wrapper's hold on the underlying entry. Only the
// first call has effect — subsequent ones are no-ops, which matches
// the "Idempotent" promise on the Container interface and prevents
// over-decrementing the refcount on double-Stop from defers.
func (c *pooledContainer) Stop(_ context.Context) error {
	c.stoppedOnce.Do(func() { c.pool.release(c.key, c.entry) })
	return nil
}
