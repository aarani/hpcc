package runtime

import (
	"context"
	"sync"
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
	spec   ContainerSpec
	parent *fakeRuntime
	stops  atomic.Int32
}

func (c *fakeContainer) ID() string          { return c.spec.ID }
func (c *fakeContainer) TenantID() string    { return c.spec.TenantID }
func (c *fakeContainer) ImageDigest() string { return c.spec.ImageDigest }
func (c *fakeContainer) State() gen.VMState  { return gen.VMState_RUNNING }
func (c *fakeContainer) Exec(context.Context, ExecRequest) (ExecResult, error) {
	return ExecResult{}, nil
}
func (c *fakeContainer) Stop(context.Context) error { c.stops.Add(1); return nil }

// spec1 / spec4 construct ContainerSpecs whose VCPUs sets the per-VM
// concurrency cap in the pool. Most tests need >1 to exercise multi-
// Exec packing; the few that don't can call with 1.
func spec(id, tenant, image string, vcpus int32) ContainerSpec {
	return ContainerSpec{ID: id, TenantID: tenant, ImageDigest: image, VCPUs: vcpus}
}

func TestPool_ReusesContainerForSameKey(t *testing.T) {
	inner := &fakeRuntime{}
	p := NewPooledRuntime(inner, time.Hour, 0, 0)
	defer p.Close()

	s := spec("c1", "t1", "img-a", 4)

	// First Start: forwards to inner.
	c1, err := p.Start(context.Background(), s)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if got := inner.starts.Load(); got != 1 {
		t.Fatalf("after first Start, inner.starts = %d, want 1", got)
	}

	// Release; the entry stays in the pool with refs=0.
	_ = c1.Stop(context.Background())

	// Second Start with same key: pool hit, no new inner.Start.
	c2, err := p.Start(context.Background(), s)
	if err != nil {
		t.Fatalf("Start (reuse): %v", err)
	}
	if got := inner.starts.Load(); got != 1 {
		t.Fatalf("after reuse, inner.starts = %d, want 1", got)
	}

	// Different (tenant, image) key: must miss the pool.
	c3, err := p.Start(context.Background(), spec("c2", "t1", "img-b", 4))
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

	c, _ := p.Start(context.Background(), spec("", "t", "i", 1))
	_ = c.Stop(context.Background())
	_ = c.Stop(context.Background()) // second call must be a no-op (no double-decrement)

	// Reusing should still hit pool with one entry — and refs must be
	// 0 right now (the first Stop set it; the second was suppressed).
	c2, _ := p.Start(context.Background(), spec("", "t", "i", 1))
	if got := inner.starts.Load(); got != 1 {
		t.Fatalf("inner.starts = %d, want 1", got)
	}
	_ = c2.Stop(context.Background())
}

func TestPool_DrainsOnClose(t *testing.T) {
	inner := &fakeRuntime{}
	p := NewPooledRuntime(inner, time.Hour, 0, 0)

	c1, _ := p.Start(context.Background(), spec("", "a", "x", 1))
	c2, _ := p.Start(context.Background(), spec("", "b", "y", 1))
	_ = c1.Stop(context.Background())
	_ = c2.Stop(context.Background())

	if err := p.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	inner1 := c1.(*pooledContainer).entry.container.(*fakeContainer)
	inner2 := c2.(*pooledContainer).entry.container.(*fakeContainer)
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
	// Cap = 2 entries: a third fresh Start must evict the globally-
	// oldest idle entry (smallest lastFreeT), not the same key's older
	// entry.
	p := NewPooledRuntime(inner, time.Hour, 0, 2)
	defer p.Close()

	c1, _ := p.Start(context.Background(), spec("", "a", "x", 1))
	_ = c1.Stop(context.Background()) // refs=0, total=1
	// Sleep tiny amounts so lastFreeT timestamps order strictly.
	time.Sleep(2 * time.Millisecond)

	c2, _ := p.Start(context.Background(), spec("", "b", "y", 1))
	_ = c2.Stop(context.Background()) // refs=0, total=2
	time.Sleep(2 * time.Millisecond)

	c3, _ := p.Start(context.Background(), spec("", "c", "z", 1))
	_ = c3.Stop(context.Background()) // would push total to 3 → evict oldest (a|x)

	inner1 := c1.(*pooledContainer).entry.container.(*fakeContainer)
	inner2 := c2.(*pooledContainer).entry.container.(*fakeContainer)
	inner3 := c3.(*pooledContainer).entry.container.(*fakeContainer)
	if inner1.stops.Load() != 1 {
		t.Errorf("oldest (a|x) should have been evicted; stops=%d, want 1", inner1.stops.Load())
	}
	if inner2.stops.Load() != 0 || inner3.stops.Load() != 0 {
		t.Errorf("non-oldest were evicted: stops=(%d, %d), want (0, 0)",
			inner2.stops.Load(), inner3.stops.Load())
	}

	p.mu.Lock()
	count := p.total
	hasA := len(p.entries["a|x"]) > 0
	hasB := len(p.entries["b|y"]) > 0
	hasC := len(p.entries["c|z"]) > 0
	p.mu.Unlock()
	if count != 2 {
		t.Errorf("total = %d, want 2", count)
	}
	if hasA {
		t.Errorf("a|x slot should be empty after eviction")
	}
	if !hasB || !hasC {
		t.Errorf("b|y or c|z missing after cap-eviction; hasB=%v hasC=%v", hasB, hasC)
	}
}

func TestPool_PrunesStaleIdleEntryOnStart(t *testing.T) {
	inner := &fakeRuntime{}
	// idleTTL=0 disables the background reaper at construction so we
	// exercise the inline pruning path explicitly.
	p := NewPooledRuntime(inner, 0, 0, 0)
	defer p.Close()

	c, _ := p.Start(context.Background(), spec("", "t", "i", 1))
	_ = c.Stop(context.Background())

	// Turn idleTTL on, then rewind lastFreeT to make the entry stale.
	p.idleTTL = 10 * time.Millisecond
	p.mu.Lock()
	p.entries["t|i"][0].lastFreeT = time.Now().Add(-time.Hour)
	p.mu.Unlock()

	// Next Start with the same key must skip the stale entry, prune
	// it, and fall through to inner.Start.
	c2, _ := p.Start(context.Background(), spec("", "t", "i", 1))
	defer c2.Stop(context.Background())
	if got := inner.starts.Load(); got != 2 {
		t.Fatalf("inner.starts = %d, want 2 (stale entry should have been pruned)", got)
	}
	inner1 := c.(*pooledContainer).entry.container.(*fakeContainer)
	if inner1.stops.Load() != 1 {
		t.Errorf("pruned container was not stopped; stops=%d, want 1", inner1.stops.Load())
	}
}

func TestPool_HardSessionTimeoutSkippedOnStart(t *testing.T) {
	inner := &fakeRuntime{}
	// Both clocks disabled at construction so the background reaper
	// stays asleep; we drive the on-Start path by hand.
	p := NewPooledRuntime(inner, 0, 0, 0)
	defer p.Close()

	c, _ := p.Start(context.Background(), spec("", "t", "i", 1))
	_ = c.Stop(context.Background())

	// Turn maxLifetime on, then rewind createdAt past it.
	p.maxLifetime = 10 * time.Millisecond
	p.mu.Lock()
	p.entries["t|i"][0].createdAt = time.Now().Add(-time.Hour)
	p.mu.Unlock()

	c2, _ := p.Start(context.Background(), spec("", "t", "i", 1))
	defer c2.Stop(context.Background())
	if got := inner.starts.Load(); got != 2 {
		t.Fatalf("inner.starts = %d, want 2 (over-aged entry should have been pruned)", got)
	}
	inner1 := c.(*pooledContainer).entry.container.(*fakeContainer)
	if inner1.stops.Load() != 1 {
		t.Errorf("over-aged container was not stopped on Start; stops=%d, want 1", inner1.stops.Load())
	}
}

func TestPool_HardSessionTimeoutEvictsOnRelease(t *testing.T) {
	inner := &fakeRuntime{}
	// 50ms ceiling so the test runs fast but still gives us room to
	// observe one warm reuse before the hammer drops.
	p := NewPooledRuntime(inner, 0, 50*time.Millisecond, 0)
	defer p.Close()

	c1, _ := p.Start(context.Background(), spec("", "t", "i", 1))
	_ = c1.Stop(context.Background()) // refs=0

	// Warm reuse — same key — must hit the pool.
	c2, _ := p.Start(context.Background(), spec("", "t", "i", 1))
	if got := inner.starts.Load(); got != 1 {
		t.Fatalf("warm reuse missed pool: inner.starts=%d, want 1", got)
	}

	// Sleep past the ceiling, then release. release must tear down
	// rather than re-park the over-aged entry.
	time.Sleep(80 * time.Millisecond)
	_ = c2.Stop(context.Background())

	innerC := c1.(*pooledContainer).entry.container.(*fakeContainer)
	if innerC.stops.Load() != 1 {
		t.Errorf("expected release to Stop the over-aged container; stops=%d, want 1", innerC.stops.Load())
	}

	p.mu.Lock()
	total := p.total
	p.mu.Unlock()
	if total != 0 {
		t.Errorf("over-aged container was re-parked; total=%d, want 0", total)
	}
}

func TestPool_ReaperEvictsBySessionTimeout(t *testing.T) {
	inner := &fakeRuntime{}
	// Idle disabled, hard session timeout enabled — confirm the
	// reaper still runs and evicts on the maxLifetime clock alone.
	p := NewPooledRuntime(inner, 0, 30*time.Millisecond, 0)
	defer p.Close()

	c, _ := p.Start(context.Background(), spec("", "t", "i", 1))
	_ = c.Stop(context.Background())

	// Wait long enough for at least one reaper tick (interval = 15ms)
	// past the deadline.
	time.Sleep(80 * time.Millisecond)

	p.mu.Lock()
	total := p.total
	p.mu.Unlock()
	if total != 0 {
		t.Errorf("reaper did not evict on maxLifetime; total=%d, want 0", total)
	}
}

// --- Multi-Exec packing ----------------------------------------------

func TestPool_PacksMultipleExecsIntoOneVM(t *testing.T) {
	inner := &fakeRuntime{}
	p := NewPooledRuntime(inner, time.Hour, 0, 0)
	defer p.Close()

	s := spec("", "t1", "img", 2) // VCPUs=2 → maxRefs=2

	// Two concurrent Starts for the same key: refs=2 on one entry,
	// inner.Start called once.
	c1, err := p.Start(context.Background(), s)
	if err != nil {
		t.Fatalf("Start 1: %v", err)
	}
	c2, err := p.Start(context.Background(), s)
	if err != nil {
		t.Fatalf("Start 2: %v", err)
	}
	if got := inner.starts.Load(); got != 1 {
		t.Fatalf("inner.starts = %d, want 1 (two execs should share one VM)", got)
	}
	if c1.(*pooledContainer).entry != c2.(*pooledContainer).entry {
		t.Fatalf("two concurrent Starts returned different entries; should share")
	}
	p.mu.Lock()
	refs := c1.(*pooledContainer).entry.refs
	total := p.total
	p.mu.Unlock()
	if refs != 2 || total != 1 {
		t.Fatalf("refs=%d total=%d, want refs=2 total=1", refs, total)
	}

	_ = c1.Stop(context.Background())
	_ = c2.Stop(context.Background())
}

func TestPool_StartsFreshAtMaxRefs(t *testing.T) {
	inner := &fakeRuntime{}
	p := NewPooledRuntime(inner, time.Hour, 0, 0)
	defer p.Close()

	s := spec("", "t1", "img", 2)

	c1, _ := p.Start(context.Background(), s)
	c2, _ := p.Start(context.Background(), s)
	// First entry now at refs=2 (its cap). A third concurrent Start
	// must spin up a fresh container.
	c3, _ := p.Start(context.Background(), s)

	if got := inner.starts.Load(); got != 2 {
		t.Fatalf("inner.starts = %d, want 2 (third exec should start fresh VM)", got)
	}
	if c1.(*pooledContainer).entry == c3.(*pooledContainer).entry {
		t.Fatalf("third Start landed on saturated entry; should have started a fresh container")
	}

	p.mu.Lock()
	total := p.total
	p.mu.Unlock()
	if total != 2 {
		t.Fatalf("total = %d, want 2", total)
	}

	_ = c1.Stop(context.Background())
	_ = c2.Stop(context.Background())
	_ = c3.Stop(context.Background())
}

func TestPool_ReaperDoesNotEvictActiveEntry(t *testing.T) {
	inner := &fakeRuntime{}
	// Aggressive idle reaping: 20ms TTL, reaper ticks every 10ms.
	p := NewPooledRuntime(inner, 20*time.Millisecond, 0, 0)
	defer p.Close()

	c, _ := p.Start(context.Background(), spec("", "t", "i", 1))
	// Do NOT Stop: refs stays at 1. Even though idleTTL is well past
	// any reasonable timestamp by the time the reaper runs, an active
	// entry must not be torn down — that would kill the in-flight
	// Exec.

	// Rewind lastFreeT just to be extra sure the reaper would target
	// this entry if it ignored refs.
	p.mu.Lock()
	p.entries["t|i"][0].lastFreeT = time.Now().Add(-time.Hour)
	p.mu.Unlock()

	time.Sleep(60 * time.Millisecond) // ~6 reaper ticks

	innerC := c.(*pooledContainer).entry.container.(*fakeContainer)
	if innerC.stops.Load() != 0 {
		t.Fatalf("reaper evicted entry with refs>0; stops=%d, want 0", innerC.stops.Load())
	}
	p.mu.Lock()
	total := p.total
	p.mu.Unlock()
	if total != 1 {
		t.Fatalf("active entry vanished from pool; total=%d, want 1", total)
	}

	_ = c.Stop(context.Background())
}

func TestPool_CloseBlocksUntilOutstandingRelease(t *testing.T) {
	inner := &fakeRuntime{}
	p := NewPooledRuntime(inner, time.Hour, 0, 0)

	c, _ := p.Start(context.Background(), spec("", "t", "i", 1))

	// Kick off Close in a goroutine; it must not return while c
	// still holds the entry.
	closeDone := make(chan error, 1)
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		closeDone <- p.Close()
	}()

	select {
	case err := <-closeDone:
		t.Fatalf("Close returned before outstanding refs released (err=%v)", err)
	case <-time.After(30 * time.Millisecond):
	}

	_ = c.Stop(context.Background())

	select {
	case err := <-closeDone:
		if err != nil {
			t.Fatalf("Close: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatalf("Close did not return after refs hit zero")
	}
	wg.Wait()

	innerC := c.(*pooledContainer).entry.container.(*fakeContainer)
	if innerC.stops.Load() != 1 {
		t.Errorf("Close did not Stop the underlying container; stops=%d, want 1", innerC.stops.Load())
	}
	if inner.closes.Load() != 1 {
		t.Errorf("inner.Close not forwarded: closes=%d, want 1", inner.closes.Load())
	}
}
