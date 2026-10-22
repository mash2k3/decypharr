package vfs

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sirrobot01/decypharr/pkg/mount/dfs/vfs/ranges"
)

const (
	testKiB = int64(1024)
	testMiB = 1024 * testKiB
)

func TestCurrentKickerInterval(t *testing.T) {
	dls := &Downloaders{}

	if got := dls.currentKickerInterval(); got != kickerInterval {
		t.Fatalf("unexpected interval without waiters: got %s, want %s", got, kickerInterval)
	}

	dls.waiterCount.Store(1)
	if got := dls.currentKickerInterval(); got != activeWaiterKickerInterval {
		t.Fatalf("unexpected interval with waiters: got %s, want %s", got, activeWaiterKickerInterval)
	}
}

func getMaxOffset(dl *downloader) int64 {
	dl.mu.Lock()
	defer dl.mu.Unlock()
	return dl.maxOffset
}

func TestEnsureDownloaderLocked_ExtendsMissByReadAhead(t *testing.T) {
	const (
		reqPos    = 10 * testMiB
		reqSize   = 128 * testKiB
		readAhead = 16 * testMiB
	)

	item := &CacheItem{
		info: ItemInfo{
			Size: 64 * testMiB,
		},
	}

	dl := &downloader{
		start:     reqPos,
		offset:    reqPos,
		maxOffset: reqPos + reqSize,
	}

	dls := &Downloaders{
		item:          item,
		chunkSize:     4 * testMiB,
		readAheadSize: readAhead,
		dls:           []*downloader{dl},
	}

	req := ranges.Range{Pos: reqPos, Size: reqSize}
	if err := dls.ensureDownloaderLocked(req, false); err != nil {
		t.Fatalf("ensureDownloaderLocked returned error: %v", err)
	}

	want := req.End() + readAhead
	got := getMaxOffset(dl)
	if got != want {
		t.Fatalf("unexpected maxOffset: got %d, want %d", got, want)
	}
}

func TestEnsureDownloaderLocked_CachedRequestPrefetchesGap(t *testing.T) {
	const (
		reqPos    = 0
		reqSize   = 128 * testKiB
		readAhead = 16 * testMiB
	)

	item := &CacheItem{
		info: ItemInfo{
			Size: 64 * testMiB,
			Rs: ranges.Ranges{
				{Pos: 0, Size: 1 * testMiB}, // request is cached, look-ahead has a gap after 1 MiB
			},
		},
	}

	dl := &downloader{
		start:     512 * testKiB,
		offset:    2 * testMiB,
		maxOffset: 2 * testMiB,
	}

	dls := &Downloaders{
		item:          item,
		chunkSize:     4 * testMiB,
		readAheadSize: readAhead,
		dls:           []*downloader{dl},
	}

	req := ranges.Range{Pos: reqPos, Size: reqSize}
	if err := dls.ensureDownloaderLocked(req, false); err != nil {
		t.Fatalf("ensureDownloaderLocked returned error: %v", err)
	}

	want := req.End() + readAhead
	got := getMaxOffset(dl)
	if got != want {
		t.Fatalf("unexpected maxOffset: got %d, want %d", got, want)
	}
}

func TestEnsureDownloaderLocked_CachedWindowFullDoesNotExtend(t *testing.T) {
	const (
		reqPos    = 0
		reqSize   = 128 * testKiB
		readAhead = 16 * testMiB
	)

	item := &CacheItem{
		info: ItemInfo{
			Size: 64 * testMiB,
			Rs: ranges.Ranges{
				{Pos: 0, Size: 32 * testMiB}, // request + look-ahead fully cached
			},
		},
	}

	dl := &downloader{
		start:     0,
		offset:    2 * testMiB,
		maxOffset: 2 * testMiB,
	}

	dls := &Downloaders{
		item:          item,
		chunkSize:     4 * testMiB,
		readAheadSize: readAhead,
		dls:           []*downloader{dl},
	}

	req := ranges.Range{Pos: reqPos, Size: reqSize}
	if err := dls.ensureDownloaderLocked(req, false); err != nil {
		t.Fatalf("ensureDownloaderLocked returned error: %v", err)
	}

	want := int64(2 * testMiB)
	got := getMaxOffset(dl)
	if got != want {
		t.Fatalf("unexpected maxOffset when window is full: got %d, want %d", got, want)
	}
}

func TestStopAllClearsWaiters(t *testing.T) {
	parentCtx := context.Background()
	ctx, cancel := context.WithCancel(parentCtx)

	dls := &Downloaders{
		parentCtx: parentCtx,
		ctx:       ctx,
		cancel:    cancel,
	}
	dls.stopCond = sync.NewCond(&dls.mu)

	errCh := make(chan error, 1)
	dls.waiters = append(dls.waiters, waiter{
		r:       ranges.Range{Pos: 0, Size: 1},
		errChan: errCh,
	})
	dls.waiterCount.Store(1)

	dls.StopAll()

	select {
	case err := <-errCh:
		if err == nil {
			t.Fatal("expected waiter to receive stop error")
		}
	case <-time.After(200 * time.Millisecond):
		t.Fatal("waiter was not unblocked by StopAll")
	}

	if got := dls.waiterCount.Load(); got != 0 {
		t.Fatalf("unexpected waiter count after StopAll: got %d, want 0", got)
	}
}

func TestCacheItemReleaseStopsDownloadersOnZeroOpens(t *testing.T) {
	// Release() only stops downloaders after releaseStopGracePeriodNanos (2s
	// by default, to absorb rapid close/reopen cycles) — shrink it for this
	// test so we don't need a multi-second sleep, and restore it afterward
	// since it's a shared package-level value.
	prevGracePeriod := releaseStopGracePeriodNanos.Swap(int64(10 * time.Millisecond))
	t.Cleanup(func() { releaseStopGracePeriodNanos.Store(prevGracePeriod) })

	parentCtx := context.Background()
	ctx, cancel := context.WithCancel(parentCtx)

	dls := &Downloaders{
		parentCtx: parentCtx,
		ctx:       ctx,
		cancel:    cancel,
	}
	dls.stopCond = sync.NewCond(&dls.mu)

	item := &CacheItem{
		downloaders: dls,
	}
	item.opens.Store(1)

	item.Release()

	select {
	case <-ctx.Done():
	case <-time.After(500 * time.Millisecond):
		t.Fatal("expected downloader context to be canceled when opens reaches zero")
	}

	if got := item.opens.Load(); got != 0 {
		t.Fatalf("unexpected opens after release: got %d, want 0", got)
	}
}

// isCircuitOpen's cooldown-expiry path used to reset circuitOpen/circuitOpenAt
// inline without going through resetCircuitLocked, leaking the cache-wide
// circuitBreakers gauge by one every time a breaker expired via a read
// hitting this path (as opposed to going idle first). This proves the gauge
// comes back down.
func TestIsCircuitOpen_CooldownExpiryDecrementsGauge(t *testing.T) {
	cache := &Cache{}
	item := &CacheItem{cache: cache}
	dls := &Downloaders{item: item}

	dls.mu.Lock()
	dls.openCircuitLocked()
	dls.mu.Unlock()

	if got := cache.circuitBreakers.Load(); got != 1 {
		t.Fatalf("expected circuitBreakers gauge = 1 after opening, got %d", got)
	}

	// Back-date circuitOpenAt so the cooldown reads as already expired.
	dls.circuitOpenAt.Store(time.Now().Add(-circuitCooldownDuration - time.Second).UnixNano())

	if dls.isCircuitOpen() {
		t.Fatal("expected isCircuitOpen to report false once cooldown has expired")
	}
	if dls.circuitOpen.Load() {
		t.Fatal("expected circuitOpen to be cleared after cooldown expiry")
	}
	if got := cache.circuitBreakers.Load(); got != 0 {
		t.Fatalf("expected circuitBreakers gauge decremented back to 0, got %d", got)
	}
}

// Close is a third way a Downloaders' life can end (alongside StopAll and
// checkIdleTimeout, both of which already release the cache-wide
// circuitBreakers gauge slot on their way out) — an item can be torn down
// via idle-timeout eviction, a forced close, or process shutdown while its
// breaker happens to be open, and Close previously never released that slot.
func TestClose_ResetsOpenCircuitBreaker(t *testing.T) {
	cache := &Cache{}
	item := &CacheItem{cache: cache}
	parentCtx := context.Background()
	ctx, cancel := context.WithCancel(parentCtx)

	dls := &Downloaders{
		item:      item,
		parentCtx: parentCtx,
		ctx:       ctx,
		cancel:    cancel,
	}
	dls.stopCond = sync.NewCond(&dls.mu)

	dls.mu.Lock()
	dls.openCircuitLocked()
	dls.mu.Unlock()

	if got := cache.circuitBreakers.Load(); got != 1 {
		t.Fatalf("expected circuitBreakers gauge = 1 after opening, got %d", got)
	}

	if err := dls.Close(nil); err != nil {
		t.Fatalf("Close returned error: %v", err)
	}

	if dls.circuitOpen.Load() {
		t.Fatal("expected circuitOpen to be cleared after Close")
	}
	if got := cache.circuitBreakers.Load(); got != 0 {
		t.Fatalf("expected circuitBreakers gauge decremented back to 0, got %d", got)
	}
}

// DownloadWithPriority used to fail immediately with "downloaders stopping"
// whenever a request landed during a StopAll() teardown window — even
// though StopAll is designed to resume the session (it clears stopping once
// the new ctx is installed), not just to shut down. This proves the call
// now blocks through that window and succeeds once stopping clears, instead
// of failing a request whose only problem was timing.
func TestDownloadWithPriority_WaitsThroughStoppingWindow(t *testing.T) {
	parentCtx := context.Background()
	ctx, cancel := context.WithCancel(parentCtx)
	defer cancel()

	item := &CacheItem{
		info: ItemInfo{
			Size: 1024,
			Rs:   ranges.Ranges{{Pos: 0, Size: 1024}}, // fully present: no network needed
		},
	}

	dls := &Downloaders{
		parentCtx: parentCtx,
		ctx:       ctx,
		cancel:    cancel,
		item:      item,
	}
	dls.stopCond = sync.NewCond(&dls.mu)
	dls.stopping = true

	unblocked := make(chan error, 1)
	go func() {
		unblocked <- dls.DownloadWithPriority(context.Background(), ranges.Range{Pos: 0, Size: 1024}, true)
	}()

	// Give the goroutine time to reach the wait and confirm it's actually
	// blocked, not racing past a bug that no-ops the wait.
	select {
	case err := <-unblocked:
		t.Fatalf("DownloadWithPriority returned early (err=%v) instead of waiting out the stopping window", err)
	case <-time.After(100 * time.Millisecond):
	}

	dls.mu.Lock()
	dls.stopping = false
	dls.stopCond.Broadcast()
	dls.mu.Unlock()

	select {
	case err := <-unblocked:
		if err != nil {
			t.Fatalf("DownloadWithPriority returned an error after stopping cleared: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("DownloadWithPriority did not unblock after stopping cleared and stopCond was broadcast")
	}
}

// The kicker sets idle=true inside checkIdleTimeout but only closes kickerDone
// after mu is released. A read landing in that gap must still start a fresh
// kicker — the old one is about to exit — or no kicker ever runs again and the
// stream is never untracked by the idle timeout.
func TestDownloadWithPriority_RestartsKickerWhileOldOneIsExiting(t *testing.T) {
	parentCtx := context.Background()
	ctx, cancel := context.WithCancel(parentCtx)
	defer cancel()

	item := &CacheItem{
		info: ItemInfo{
			Size: 1024,
			Rs:   ranges.Ranges{{Pos: 0, Size: 1024}}, // fully present: no network needed
		},
	}

	// Old kicker has committed to exiting (idle=true) but hasn't closed its
	// done channel yet.
	exitingKickerDone := make(chan struct{})
	dls := &Downloaders{
		parentCtx:  parentCtx,
		ctx:        ctx,
		cancel:     cancel,
		item:       item,
		idle:       true,
		kickerDone: exitingKickerDone,
	}
	dls.stopCond = sync.NewCond(&dls.mu)

	if err := dls.DownloadWithPriority(context.Background(), ranges.Range{Pos: 0, Size: 1024}, false); err != nil {
		t.Fatalf("DownloadWithPriority: %v", err)
	}
	close(exitingKickerDone) // old kicker finishes exiting

	dls.mu.Lock()
	newKickerDone := dls.kickerDone
	idle := dls.idle
	dls.mu.Unlock()

	if idle {
		t.Fatal("expected idle to be cleared by the read")
	}
	if newKickerDone == exitingKickerDone {
		t.Fatal("no new kicker started: session left with no kicker after the old one exits")
	}
	select {
	case <-newKickerDone:
		t.Fatal("new kicker exited immediately")
	default:
	}

	cancel()
	select {
	case <-newKickerDone:
	case <-time.After(2 * time.Second):
		t.Fatal("new kicker did not exit on ctx cancel")
	}
}

// A Downloaders that is permanently Close()d while a caller is waiting out a
// stopping window must wake that caller with the terminal error instead of
// hanging forever waiting for a stopping->false transition that will now
// never come.
func TestDownloadWithPriority_ErrorsIfClosedWhileWaiting(t *testing.T) {
	parentCtx := context.Background()
	ctx, cancel := context.WithCancel(parentCtx)

	dls := &Downloaders{
		parentCtx: parentCtx,
		ctx:       ctx,
		cancel:    cancel,
		item:      &CacheItem{info: ItemInfo{Size: 1024}},
	}
	dls.stopCond = sync.NewCond(&dls.mu)
	dls.stopping = true

	unblocked := make(chan error, 1)
	go func() {
		unblocked <- dls.DownloadWithPriority(context.Background(), ranges.Range{Pos: 0, Size: 1024}, true)
	}()

	select {
	case err := <-unblocked:
		t.Fatalf("DownloadWithPriority returned early (err=%v) instead of waiting", err)
	case <-time.After(100 * time.Millisecond):
	}

	if err := dls.Close(nil); err != nil {
		t.Fatalf("Close returned error: %v", err)
	}

	select {
	case err := <-unblocked:
		if err == nil {
			t.Fatal("expected DownloadWithPriority to return an error once Close ran, got nil")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("DownloadWithPriority hung instead of waking up when Close ran")
	}
}

// A caller with no deadline of its own (context.Background(), the FUSE read
// path's usual case) must not hang forever if stopping never clears — the
// internal maxStoppingWaitNanos safety net must fire and return a clear
// error instead.
func TestDownloadWithPriority_TimesOutIfStoppingNeverClears(t *testing.T) {
	prevWait := maxStoppingWaitNanos.Swap(int64(50 * time.Millisecond))
	t.Cleanup(func() { maxStoppingWaitNanos.Store(prevWait) })

	parentCtx := context.Background()
	ctx, cancel := context.WithCancel(parentCtx)
	defer cancel()

	dls := &Downloaders{
		parentCtx: parentCtx,
		ctx:       ctx,
		cancel:    cancel,
		item:      &CacheItem{info: ItemInfo{Size: 1024}},
	}
	dls.stopCond = sync.NewCond(&dls.mu)
	dls.stopping = true // deliberately never cleared

	unblocked := make(chan error, 1)
	go func() {
		unblocked <- dls.DownloadWithPriority(context.Background(), ranges.Range{Pos: 0, Size: 1024}, true)
	}()

	select {
	case err := <-unblocked:
		if err == nil {
			t.Fatal("expected DownloadWithPriority to time out and return an error, got nil")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("DownloadWithPriority hung past the shortened safety-net timeout — stuck stopping=true should degrade to an error, not a permanent hang")
	}
}

func TestNoProgressWatchdogCancelsStalledAttempt(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var lastProgressNanos atomic.Int64
	lastProgressNanos.Store(time.Now().Add(-300 * time.Millisecond).UnixNano())

	var timedOut atomic.Bool
	stop := startNoProgressWatchdog(
		ctx,
		120*time.Millisecond,
		10*time.Millisecond,
		&lastProgressNanos,
		cancel,
		&timedOut,
	)
	defer stop()

	select {
	case <-ctx.Done():
	case <-time.After(500 * time.Millisecond):
		t.Fatal("expected context cancellation from no-progress watchdog")
	}

	if !timedOut.Load() {
		t.Fatal("expected watchdog timeout flag to be set")
	}
}

func TestNoProgressWatchdogKeepsAliveWithProgress(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var lastProgressNanos atomic.Int64
	lastProgressNanos.Store(time.Now().UnixNano())

	var timedOut atomic.Bool
	stop := startNoProgressWatchdog(
		ctx,
		160*time.Millisecond,
		20*time.Millisecond,
		&lastProgressNanos,
		cancel,
		&timedOut,
	)
	defer stop()

	deadline := time.Now().Add(300 * time.Millisecond)
	ticker := time.NewTicker(30 * time.Millisecond)
	defer ticker.Stop()

	for time.Now().Before(deadline) {
		select {
		case <-ticker.C:
			lastProgressNanos.Store(time.Now().UnixNano())
		case <-ctx.Done():
			t.Fatal("unexpected watchdog cancellation while progress was advancing")
		}
	}

	if timedOut.Load() {
		t.Fatal("watchdog timed out despite ongoing progress")
	}
}
