package runtime

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aarani/hpcc/internal/protocol/gen"
)

// fakeRuntime is a Runtime stub that counts Start/Close calls and
// returns fakeContainers whose Stop also counts. Used to verify the
// pool only forwards to the inner runtime when it has to.
type fakeRuntime struct {
	starts atomic.Int32
	closes atomic.Int32
}

func (r *fakeRuntime) Start(_ context.Context, spec ContainerSpec) (Container, error) {
	r.starts.Add(1)
	return &fakeContainer{spec: spec, parent: r}, nil
}
func (r *fakeRuntime) Close() error { r.closes.Add(1); return nil }

type fakeContainer struct {
	spec    ContainerSpec
	parent  *fakeRuntime
	stops   atomic.Int32
}

func (c *fakeContainer) ID() string                                            { return c.spec.ID }
func (c *fakeContainer) TenantID() string                                      { return c.spec.TenantID }
func (c *fakeContainer) ImageDigest() string                                   { return c.spec.ImageDigest }
func (c *fakeContainer) State() gen.VMState                                    { return gen.VMState_RUNNING }
func (c *fakeContainer) Exec(context.Context, ExecRequest) (ExecResult, error) { return ExecResult{}, nil }
func (c *fakeContainer) Stop(context.Context) error                            { c.stops.Add(1); return nil }

func TestPool_ReusesContainerForSameKey(t *testing.T) {
	inner := &fakeRuntime{}
	p := NewPooledRuntime(inner, time.Hour, 0, 0)
	defer p.Close()

	spec := ContainerSpec{ID: "c1", TenantID: "t1", ImageDigest: "img-a"}

	// First Start: forwards to inner.
	c1, err := p.Start(context.Background(), spec)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if got := inner.starts.Load(); got != 1 {
		t.Fatalf("after first Start, inner.starts = %d, want 1", got)
	}

	// Stop returns to pool — not a real teardown.
	_ = c1.Stop(context.Background())

	// Second Start with same key: pool hit, no new inner.Start.
	c2, err := p.Start(context.Background(), spec)
	if err != nil {
		t.Fatalf("Start (reuse): %v", err)
	}
	if got := inner.starts.Load(); got != 1 {
		t.Fatalf("after reuse, inner.starts = %d, want 1", got)
	}

	// Different (tenant, image) key: must miss the pool.
	c3, err := p.Start(context.Background(), ContainerSpec{
		ID: "c2", TenantID: "t1", ImageDigest: "img-b",
	})
	if err != nil {
		t.Fatalf("Start (cold): %v", err)
	}
	if got := inner.starts.Load(); got != 2 {
		t.Fatalf("after cold Start, inner.starts = %d, want 2", got)
	}

	_ = c2.Stop(context.Background())
	_ = c3.Stop(context.Background())
}

func TestPool_StopIsIdempotent(t *testing.T) {
	inner := &fakeRuntime{}
	p := NewPooledRuntime(inner, time.Hour, 0, 0)
	defer p.Close()

	c, _ := p.Start(context.Background(), ContainerSpec{TenantID: "t", ImageDigest: "i"})
	_ = c.Stop(context.Background())
	_ = c.Stop(context.Background()) // second call must be a no-op (no double-park)

	// Reusing should still hit pool with one entry.
	c2, _ := p.Start(context.Background(), ContainerSpec{TenantID: "t", ImageDigest: "i"})
	if got := inner.starts.Load(); got != 1 {
		t.Fatalf("inner.starts = %d, want 1", got)
	}
	_ = c2.Stop(context.Background())
}

func TestPool_DrainsOnClose(t *testing.T) {
	inner := &fakeRuntime{}
	p := NewPooledRuntime(inner, time.Hour, 0, 0)

	c1, _ := p.Start(context.Background(), ContainerSpec{TenantID: "a", ImageDigest: "x"})
	c2, _ := p.Start(context.Background(), ContainerSpec{TenantID: "b", ImageDigest: "y"})
	_ = c1.Stop(context.Background())
	_ = c2.Stop(context.Background())

	if err := p.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	// Both parked containers must have been Stopped during Close.
	inner1 := c1.(*pooledContainer).inner.(*fakeContainer)
	inner2 := c2.(*pooledContainer).inner.(*fakeContainer)
	if inner1.stops.Load() != 1 || inner2.stops.Load() != 1 {
		t.Fatalf("Close did not drain: stops = (%d, %d), want (1, 1)",
			inner1.stops.Load(), inner2.stops.Load())
	}
	if inner.closes.Load() != 1 {
		t.Errorf("inner.Close not forwarded: closes = %d, want 1", inner.closes.Load())
	}
}

func TestPool_EvictsOldestWhenOverCap(t *testing.T) {
	inner := &fakeRuntime{}
	// Cap = 2 parked: parking a third must evict the globally-oldest
	// entry, not the same key's older entry.
	p := NewPooledRuntime(inner, time.Hour, 0, 2)
	defer p.Close()

	c1, _ := p.Start(context.Background(), ContainerSpec{TenantID: "a", ImageDigest: "x"})
	_ = c1.Stop(context.Background()) // park: count=1
	// Sleep tiny amounts so parkedAt timestamps order strictly.
	time.Sleep(2 * time.Millisecond)

	c2, _ := p.Start(context.Background(), ContainerSpec{TenantID: "b", ImageDigest: "y"})
	_ = c2.Stop(context.Background()) // park: count=2
	time.Sleep(2 * time.Millisecond)

	c3, _ := p.Start(context.Background(), ContainerSpec{TenantID: "c", ImageDigest: "z"})
	_ = c3.Stop(context.Background()) // park: count would hit 3 → evict oldest (a|x)

	// c1 (oldest parked) should have been Stopped during eviction.
	inner1 := c1.(*pooledContainer).inner.(*fakeContainer)
	inner2 := c2.(*pooledContainer).inner.(*fakeContainer)
	inner3 := c3.(*pooledContainer).inner.(*fakeContainer)
	if inner1.stops.Load() != 1 {
		t.Errorf("oldest (a|x) should have been evicted; stops=%d, want 1", inner1.stops.Load())
	}
	if inner2.stops.Load() != 0 || inner3.stops.Load() != 0 {
		t.Errorf("non-oldest were evicted: stops=(%d, %d), want (0, 0)",
			inner2.stops.Load(), inner3.stops.Load())
	}

	// Pool should still hold exactly 2 entries — b|y and c|z.
	p.mu.Lock()
	count := p.parkedCount
	hasA := len(p.pool["a|x"]) > 0
	hasB := len(p.pool["b|y"]) > 0
	hasC := len(p.pool["c|z"]) > 0
	p.mu.Unlock()
	if count != 2 {
		t.Errorf("parkedCount = %d, want 2", count)
	}
	if hasA {
		t.Errorf("a|x slot should be empty after eviction")
	}
	if !hasB || !hasC {
		t.Errorf("b|y or c|z missing after cap-eviction; hasB=%v hasC=%v", hasB, hasC)
	}
}

func TestPool_EvictsExpiredOnPop(t *testing.T) {
	inner := &fakeRuntime{}
	// idleTTL=0 disables the background reaper, so we exercise the
	// inline pop-time eviction path explicitly.
	p := NewPooledRuntime(inner, 0, 0, 0)
	defer p.Close()

	// Manually park an entry with an old timestamp by going through
	// the public path, then rewinding parkedAt under the lock.
	c, _ := p.Start(context.Background(), ContainerSpec{TenantID: "t", ImageDigest: "i"})
	_ = c.Stop(context.Background())

	// Put back a fresh ttl and rewind the parkedAt to make the entry stale.
	p.idleTTL = 10 * time.Millisecond
	p.mu.Lock()
	p.pool["t|i"][0].parkedAt = time.Now().Add(-time.Hour)
	p.mu.Unlock()

	// Next Start with the same key must skip the stale entry and
	// fall through to inner.Start.
	c2, _ := p.Start(context.Background(), ContainerSpec{TenantID: "t", ImageDigest: "i"})
	defer c2.Stop(context.Background())
	if got := inner.starts.Load(); got != 2 {
		t.Fatalf("inner.starts = %d, want 2 (stale entry should have been evicted)", got)
	}
}

func TestPool_HardSessionTimeoutEvictsOnPop(t *testing.T) {
	inner := &fakeRuntime{}
	// Both clocks disabled at construction so the background reaper
	// stays asleep; we drive the on-pop eviction path by hand.
	p := NewPooledRuntime(inner, 0, 0, 0)
	defer p.Close()

	c, _ := p.Start(context.Background(), ContainerSpec{TenantID: "t", ImageDigest: "i"})
	_ = c.Stop(context.Background())

	// Turn maxLifetime on, then rewind createdAt past it.
	p.maxLifetime = 10 * time.Millisecond
	p.mu.Lock()
	p.pool["t|i"][0].createdAt = time.Now().Add(-time.Hour)
	p.mu.Unlock()

	c2, _ := p.Start(context.Background(), ContainerSpec{TenantID: "t", ImageDigest: "i"})
	defer c2.Stop(context.Background())
	if got := inner.starts.Load(); got != 2 {
		t.Fatalf("inner.starts = %d, want 2 (over-aged entry should have been evicted on pop)", got)
	}
	inner1 := c.(*pooledContainer).inner.(*fakeContainer)
	// The eviction in popLocked Stops async via a goroutine; give it
	// a moment to run before asserting.
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) && inner1.stops.Load() == 0 {
		time.Sleep(time.Millisecond)
	}
	if inner1.stops.Load() != 1 {
		t.Errorf("over-aged container was not stopped on pop; stops=%d, want 1", inner1.stops.Load())
	}
}

func TestPool_HardSessionTimeoutPreservedAcrossReuse(t *testing.T) {
	inner := &fakeRuntime{}
	// 50ms ceiling so the test runs fast but still gives us room to
	// observe one warm reuse before the hammer drops.
	p := NewPooledRuntime(inner, 0, 50*time.Millisecond, 0)
	defer p.Close()

	c1, _ := p.Start(context.Background(), ContainerSpec{TenantID: "t", ImageDigest: "i"})
	_ = c1.Stop(context.Background()) // park

	// Warm reuse — same key — must hit the pool.
	c2, _ := p.Start(context.Background(), ContainerSpec{TenantID: "t", ImageDigest: "i"})
	if got := inner.starts.Load(); got != 1 {
		t.Fatalf("warm reuse missed pool: inner.starts=%d, want 1", got)
	}

	// Sleep past the ceiling, then return c2 to the pool. Park must
	// refuse to re-park and Stop the container instead.
	time.Sleep(80 * time.Millisecond)
	_ = c2.Stop(context.Background())

	innerC := c1.(*pooledContainer).inner.(*fakeContainer)
	if innerC.stops.Load() != 1 {
		t.Errorf("expected park to Stop the over-aged container; stops=%d, want 1", innerC.stops.Load())
	}

	p.mu.Lock()
	parked := p.parkedCount
	p.mu.Unlock()
	if parked != 0 {
		t.Errorf("over-aged container was re-parked; parkedCount=%d, want 0", parked)
	}
}

func TestPool_ReaperEvictsBySessionTimeout(t *testing.T) {
	inner := &fakeRuntime{}
	// Idle disabled, hard session timeout enabled — confirm the
	// reaper still runs and evicts on the maxLifetime clock alone.
	p := NewPooledRuntime(inner, 0, 30*time.Millisecond, 0)
	defer p.Close()

	c, _ := p.Start(context.Background(), ContainerSpec{TenantID: "t", ImageDigest: "i"})
	_ = c.Stop(context.Background())

	// Wait long enough for at least one reaper tick (interval = 15ms)
	// past the deadline.
	time.Sleep(80 * time.Millisecond)

	p.mu.Lock()
	parked := p.parkedCount
	p.mu.Unlock()
	if parked != 0 {
		t.Errorf("reaper did not evict on maxLifetime; parkedCount=%d, want 0", parked)
	}
}
