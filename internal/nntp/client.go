package nntp

import (
	"bufio"
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"net/textproto"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/puzpuzpuz/xsync/v4"
	"github.com/rs/zerolog"
	"github.com/sirrobot01/decypharr/internal/config"
	"github.com/sirrobot01/decypharr/internal/customerror"
	"github.com/sirrobot01/decypharr/internal/logger"
	"github.com/sirrobot01/decypharr/internal/retry"
	"github.com/sirrobot01/decypharr/internal/utils"
	"github.com/sirrobot01/decypharr/pkg/storage/hybrid"
)

// ProviderPool manages connections for a single provider using a LIFO stack
type ProviderPool struct {
	conns       []*connectionEntry // Stack: Push/Pop from end
	mu          sync.Mutex         // Protects conns slice only
	slots       chan struct{}      // Semaphore: capacity = max connections
	max         int
	config      config.UsenetProvider
	activeConns sync.Map // *Connection → struct{}; tracks checked-out connections for force-close on shutdown
}

// Client manages a pool of NNTP connections.
type Client struct {
	pools     map[string]*ProviderPool // Map Key: Provider Host
	providers []config.UsenetProvider
	logger    zerolog.Logger

	retries int // Number of retries per provider for transient errors

	closed atomic.Bool
	// Speed test results storage
	speedTestResults *xsync.Map[string, SpeedTestResult]
	providerMonitor  *providerMonitor

	// repairPool is the shared worker pool that processes BatchStat
	// chunks. Sized at construction from cfg.Repair.NNTPConnectionPercent.
	// Replaces the previous RepairBank counting semaphore + per-call
	// conc.Pool design — that pattern produced N × bank.Capacity
	// goroutines under N concurrent BatchStat calls because each call
	// sized its own pool to the entire bank capacity. The shared pool
	// caps total worker goroutines to exactly pool.Capacity().
	repairPool *RepairPool

	interactive *interactiveState

	// TCP socket buffer sizes (bytes) applied to every new connection. 0 means
	// "leave OS autotuning untouched". Sized from cfg.Usenet.Socket*Buffer.
	// At high RTT the receive buffer is the single-connection throughput cap
	// (≈ buffer ÷ RTT), so it must cover the bandwidth-delay product.
	sockReadBuf  int
	sockWriteBuf int

	// Pool lifecycle tuning, resolved at construction from DefaultTimeouts
	// with an optional cfg.Usenet.ConnIdleTimeout override. Kept per-client
	// (rather than on the package-level timeouts var) so config reloads that
	// rebuild the client can't race connections still using the old one.
	idleTimeout    time.Duration // close pooled conns unused for this long
	staleThreshold time.Duration // verify-ping on checkout after this much inactivity
	pingInterval   time.Duration // reaper keepalive-ping cadence for idle conns
}

// SpeedTestResult holds the result of a provider speed test
type SpeedTestResult struct {
	Provider  string    `json:"provider"`
	SpeedMBps float64   `json:"speed_mbps"`
	LatencyMs int64     `json:"latency_ms"`
	BytesRead int64     `json:"bytes_read"`
	TestedAt  time.Time `json:"tested_at"`
	Error     string    `json:"error,omitempty"`
}

// connectionEntry tracks a connection and its provider
type connectionEntry struct {
	conn     *Connection
	provider config.UsenetProvider
	lastUsed time.Time
	// lastPing is when the reaper last keepalive-pinged this idle entry.
	// Kept separate from lastUsed so pings keep the session alive without
	// counting as use — idle expiry stays keyed to real work only.
	lastPing time.Time
}

// lastActivity returns the most recent proof the connection was alive:
// either real use or a successful keepalive ping.
func (e *connectionEntry) lastActivity() time.Time {
	if e.lastPing.After(e.lastUsed) {
		return e.lastPing
	}
	return e.lastUsed
}

var connectionEntryPool = sync.Pool{
	New: func() any {
		return &connectionEntry{}
	},
}

func acquireConnectionEntry(conn *Connection, provider config.UsenetProvider, lastUsed time.Time) *connectionEntry {
	entry := connectionEntryPool.Get().(*connectionEntry)
	entry.conn = conn
	entry.provider = provider
	entry.lastUsed = lastUsed
	return entry
}

func releaseConnectionEntry(entry *connectionEntry) {
	if entry == nil {
		return
	}
	*entry = connectionEntry{}
	connectionEntryPool.Put(entry)
}

// TimeoutConfig holds all NNTP timeout settings in one place.
// This provides a single location to tune timeout behavior.
type TimeoutConfig struct {
	// Connection establishment timeout
	DialTimeout time.Duration
	// TCP keepalive interval
	KeepAlive time.Duration
	// Auth/handshake deadline after connection
	HandshakeTimeout time.Duration
	// Read deadline for streaming segment data
	StreamBodyTimeout time.Duration
	// Deadline for lightweight health checks (DATE)
	PingTimeout time.Duration
	// Health check connections idle longer than this
	StaleThreshold time.Duration
	// Close connections idle longer than this
	IdleTimeout time.Duration
	// Keepalive-ping idle pooled connections whose last activity (use or
	// ping) is older than this, instead of letting them go stale
	PingInterval time.Duration
	// How often to check for idle connections
	ReaperInterval time.Duration
}

// DefaultTimeouts returns production-tuned timeout values.
//
// IdleTimeout is deliberately long: players read in bursts (fill their
// buffer, go quiet for tens of seconds, read again), and closing warm
// connections between bursts forces a TCP+TLS+AUTH reconnect storm on
// every resume. Stale sessions are handled by keepalive DATE pings
// (PingInterval, in the reaper) plus a verify-ping on checkout
// (StaleThreshold), not by closing early.
var DefaultTimeouts = TimeoutConfig{
	DialTimeout:       10 * time.Second,
	KeepAlive:         30 * time.Second,
	HandshakeTimeout:  10 * time.Second,
	StreamBodyTimeout: 60 * time.Second,
	PingTimeout:       1500 * time.Millisecond,
	StaleThreshold:    60 * time.Second,
	IdleTimeout:       5 * time.Minute,
	PingInterval:      30 * time.Second,
	ReaperInterval:    5 * time.Second,
}

// Package-level timeouts used by all clients
var timeouts = normalizeTimeouts(DefaultTimeouts)

func normalizeTimeouts(in TimeoutConfig) TimeoutConfig {
	if in.DialTimeout <= 0 {
		in.DialTimeout = 10 * time.Second
	}
	if in.KeepAlive <= 0 {
		in.KeepAlive = 30 * time.Second
	}
	if in.HandshakeTimeout <= 0 {
		in.HandshakeTimeout = 10 * time.Second
	}
	if in.StreamBodyTimeout <= 0 {
		in.StreamBodyTimeout = 60 * time.Second
	}
	if in.PingTimeout <= 0 {
		in.PingTimeout = 1500 * time.Millisecond
	}
	if in.IdleTimeout <= 0 {
		in.IdleTimeout = 5 * time.Minute
	}
	// Keep stale checks meaningful: stale must be >0 and below idle timeout.
	if in.StaleThreshold <= 0 || in.StaleThreshold >= in.IdleTimeout {
		in.StaleThreshold = in.IdleTimeout / 2
		if in.StaleThreshold <= 0 {
			in.StaleThreshold = 10 * time.Second
		}
	}
	// Keepalive pings must fire well inside the idle window to be useful.
	if in.PingInterval <= 0 || in.PingInterval >= in.IdleTimeout {
		in.PingInterval = min(30*time.Second, in.IdleTimeout/2)
	}
	if in.ReaperInterval <= 0 {
		in.ReaperInterval = 5 * time.Second
	}
	// Sweep frequently enough to avoid long idle overhang.
	maxReaperInterval := max(in.IdleTimeout/4, time.Second)
	if in.ReaperInterval > maxReaperInterval {
		in.ReaperInterval = maxReaperInterval
	}
	return in
}

// NewClient creates a new connection manager
func NewClient(cfg *config.Config, metricStores ...*hybrid.Store) (*Client, error) {
	providers := cfg.Usenet.Providers
	if len(providers) == 0 {
		return nil, errors.New("no NNTP providers configured")
	}

	// Sort providers by priority (lower number = higher priority)
	sort.Slice(providers, func(i, j int) bool {
		return providers[i].Priority < providers[j].Priority
	})

	// Pre-normalize backbones once. excludes() runs on every connection
	// acquisition (potentially hundreds of times per second under load),
	// and the previous code re-ran strings.ToLower + TrimSpace per call
	// per provider, allocating a fresh string each time. Caching it here
	// turns the hot path into pure map lookups.
	for i := range providers {
		providers[i].Backbone = normalizeBackbone(providers[i].Backbone)
	}

	pools := make(map[string]*ProviderPool)
	for _, p := range providers {
		pp := &ProviderPool{
			conns:  make([]*connectionEntry, 0, p.MaxConnections),
			slots:  make(chan struct{}, p.MaxConnections),
			max:    p.MaxConnections,
			config: p,
		}
		pools[p.Host] = pp
	}

	cm := &Client{
		pools:            pools,
		providers:        providers,
		retries:          cfg.Retries,
		logger:           logger.New("nntp-client"),
		speedTestResults: xsync.NewMap[string, SpeedTestResult](),
		sockReadBuf:      parseSockBuf(cfg.Usenet.SocketReadBuffer),
		sockWriteBuf:     parseSockBuf(cfg.Usenet.SocketWriteBuffer),
		idleTimeout:      timeouts.IdleTimeout,
		staleThreshold:   timeouts.StaleThreshold,
		pingInterval:     timeouts.PingInterval,
	}
	if cfg.Usenet.ConnIdleTimeout != "" {
		if d, err := utils.ParseDuration(cfg.Usenet.ConnIdleTimeout); err != nil || d <= 0 {
			cm.logger.Warn().Str("conn_idle_timeout", cfg.Usenet.ConnIdleTimeout).
				Msg("invalid conn_idle_timeout, using default")
		} else {
			cm.idleTimeout = d
			// Keep the derived thresholds inside the configured window.
			if cm.staleThreshold >= cm.idleTimeout {
				cm.staleThreshold = cm.idleTimeout / 2
			}
			if cm.pingInterval >= cm.idleTimeout {
				cm.pingInterval = cm.idleTimeout / 2
			}
		}
	}
	cm.repairPool = cm.newRepairPool(cfg.Repair.NNTPConnectionPercent)
	cm.interactive = newInteractiveState(cfg)
	cm.logInteractiveConfig(cfg)
	var metricStore *hybrid.Store
	if len(metricStores) > 0 {
		metricStore = metricStores[0]
	}
	cm.providerMonitor = newProviderMonitor(cm, providers, metricStore)

	// Start background reaper
	go cm.reaper()
	// Warm configured idle pools immediately rather than waiting for the
	// first reaper tick.
	go cm.warmIdlePools()
	return cm, nil
}

// StartProviderMonitor starts startup/hourly checks after the Usenet metadata
// store is ready to supply BODY candidates for throughput measurements.
func (c *Client) StartProviderMonitor(testSegments func() []string) {
	if c.providerMonitor == nil {
		return
	}
	c.providerMonitor.testSegments = testSegments
	c.providerMonitor.Start()
}

// put returns a connection to the pool and releases the slot.
func (c *Client) put(conn *Connection, provider config.UsenetProvider) {
	if conn == nil {
		return
	}

	pp, ok := c.pools[provider.Host]
	if !ok {
		_ = conn.Close()
		return
	}

	pp.activeConns.Delete(conn) // Deregister from active tracking
	c.releaseConnectionBudget(conn)

	// Don't return closed connections to pool
	if conn.IsClosed() {
		_ = conn.Close()
		<-pp.slots // Release slot
		return
	}

	if c.closed.Load() {
		_ = conn.Close()
		<-pp.slots // Release slot
		return
	}

	entry := acquireConnectionEntry(conn, provider, utils.Now())

	pp.mu.Lock()
	// Cap stack size (shouldn't happen with semaphore, but be safe)
	if len(pp.conns) >= pp.max {
		pp.mu.Unlock()
		_ = conn.Close()
		<-pp.slots // Release slot
		return
	}
	pp.conns = append(pp.conns, entry) // Push to stack
	pp.mu.Unlock()

	<-pp.slots // Release slot - connection is now available for reuse
}

// release closes a connection without returning it (for error cases)
func (c *Client) release(conn *Connection) {
	if conn != nil {
		c.releaseConnectionBudget(conn)
		_ = conn.Close()
		if pp, ok := c.pools[conn.address]; ok {
			pp.activeConns.Delete(conn) // Deregister from active tracking
			<-pp.slots                  // Release slot
		}
	}
}

// isHealthy checks if a connection entry is still usable
func (c *Client) isHealthy(entry *connectionEntry) bool {
	if entry == nil || entry.conn == nil {
		return false
	}
	// Check if explicitly closed
	if entry.conn.IsClosed() {
		return false
	}
	// Verify with a ping if we haven't heard from this connection recently.
	// A successful reaper keepalive counts as activity, so freshly-pinged
	// connections skip the extra checkout round-trip.
	if time.Since(entry.lastActivity()) > c.staleThreshold {
		if err := entry.conn.ping(); err != nil {
			return false
		}
	}
	return true
}

// isIdleExpired reports whether a pooled connection has gone unused (by
// real work, not keepalive pings) long enough to close.
func (c *Client) isIdleExpired(lastUsed time.Time, now time.Time) bool {
	if lastUsed.IsZero() {
		return false
	}
	return now.Sub(lastUsed) > c.idleTimeout
}

// ExecuteWithFailover executes an operation with automatic provider failover and retry logic.
// Uses exclusion-based connection acquisition: gets ANY available connection,
// and on retryable errors, retries with exponential backoff before excluding the provider.
// Uses avast/retry-go for retry handling.
func (c *Client) ExecuteWithFailover(ctx context.Context, fn func(conn *Connection) error) error {
	var lastErr error
	var exclusions providerExclusions

	for providerAttempts := 0; providerAttempts < len(c.providers); providerAttempts++ {
		if ctx.Err() != nil {
			return ctx.Err()
		}

		conn, connProvider, err := c.getAnyAvailableConnection(ctx, exclusions)
		if err != nil {
			lastErr = err
			continue
		}

		// Use retry-go for retry logic with exponential backoff.
		// currentProvider tracks which pool currentConn actually belongs to so
		// returnOrReleaseConn always releases the right semaphore slot.
		var currentConn = conn
		var currentProvider = connProvider
		// Healthy streaming is the overwhelmingly common case. Avoid building
		// retry configuration and invoking retry.Do unless the first execution
		// actually fails. When it does fail, pendingErr lets the retry closure
		// process that first error as attempt 1 so retry counts and failover
		// behavior stay identical to the original path.
		pendingErr := c.safeExecute(currentConn, fn)
		if pendingErr == nil {
			c.returnOrReleaseConn(currentConn, currentProvider)
			return nil
		}
		err = retry.Do(
			func() error {
				execErr := pendingErr
				if execErr != nil {
					pendingErr = nil
				} else {
					execErr = c.safeExecute(currentConn, fn)
				}
				if execErr == nil {
					return nil
				}

				var nntpErr *Error
				if errors.As(execErr, &nntpErr) {
					switch nntpErr.Type {
					case ErrorTypeConnection, ErrorTypeTimeout, ErrorTypeServerBusy:
						// Retriable error - release the potentially dead connection.
						// Nil currentConn first to prevent double slot-release if new connection acquisition fails.
						releasedConn := currentConn
						failedProvider := currentProvider
						currentConn = nil
						c.release(releasedConn)

						// Get a fresh connection for retry. Prefer a different
						// provider after a connection/timeout/server-busy error;
						// otherwise one slow provider can consume the whole DFS
						// no-progress window before failover gets a chance. If
						// there is no alternative provider, fall back to retrying
						// the same one.
						retryExclusions := providerExclusions{}
						retryExclusions.excludeHost(failedProvider.Host)
						newConn, newProvider, connErr := c.getAnyAvailableConnection(ctx, retryExclusions)
						if connErr != nil {
							newConn, newProvider, connErr = c.getAnyAvailableConnection(ctx, providerExclusions{})
						}
						if connErr != nil {
							return retry.Unrecoverable(connErr)
						}
						currentConn = newConn
						currentProvider = newProvider
						return execErr // Retriable

					case ErrorTypeArticleNotFound:
						// Article not found - not retriable, try next provider
						return retry.Unrecoverable(execErr)

					default:
						// Non-retriable error
						return retry.Unrecoverable(execErr)
					}
				} else if customerror.IsPanicError(execErr) {
					// Panic error - release connection.
					// Nil currentConn first to prevent double slot-release after retry loop.
					releasedConn := currentConn
					currentConn = nil
					c.release(releasedConn)
					return retry.Unrecoverable(execErr)
				}
				// Unknown error type - don't retry
				return retry.Unrecoverable(execErr)
			},
			retry.Context(ctx),
			retry.Attempts(uint(c.retries)+1),
			retry.Delay(config.DefaultRetryDelay),
			retry.MaxDelay(config.DefaultRetryDelayMax),
			retry.DelayType(retry.BackOffDelay),
			retry.LastErrorOnly(true),
		)

		// Success
		if err == nil {
			c.returnOrReleaseConn(currentConn, currentProvider)
			return nil
		}

		// Handle failure
		c.returnOrReleaseConn(currentConn, currentProvider)
		lastErr = err

		// Check if we should exclude this provider
		var nntpErr *Error
		if errors.As(err, &nntpErr) {
			switch nntpErr.Type {
			case ErrorTypeArticleNotFound:
				excludeForArticleNotFound(&exclusions, connProvider)
			case ErrorTypeConnection, ErrorTypeTimeout, ErrorTypeServerBusy:
				exclusions.excludeHost(connProvider.Host)
			default:
				// Non-retriable error, return immediately
				return err
			}
		} else if customerror.IsPanicError(err) {
			exclusions.excludeHost(connProvider.Host)
		} else {
			// Unknown error type - return immediately
			return err
		}
	}

	if lastErr != nil {
		return lastErr
	}
	return errors.New("all providers failed")
}

// returnOrReleaseConn returns a connection to the pool or releases it if closed
func (c *Client) returnOrReleaseConn(conn *Connection, provider config.UsenetProvider) {
	if conn == nil {
		return
	}
	if conn.IsClosed() {
		c.release(conn)
	} else {
		c.put(conn, provider)
	}
}

// getConnectionFromProvider tries to get a connection from a specific provider
func (c *Client) getConnectionFromProvider(ctx context.Context, provider config.UsenetProvider) (*Connection, config.UsenetProvider, error) {
	pp, ok := c.pools[provider.Host]
	if !ok {
		return nil, provider, fmt.Errorf("provider pool not found: %s", provider.Host)
	}
	return c.checkoutFromPool(ctx, pp, provider)
}

// safeExecute wraps fn execution with panic recovery
func (c *Client) safeExecute(conn *Connection, fn func(conn *Connection) error) (err error) {
	defer func() {
		if r := recover(); r != nil {
			c.logger.Error().
				Interface("panic", r).
				Str("host", conn.address).
				Msg("Recovered from panic in NNTP operation")
			err = customerror.NewPanicError(r)
		}
	}()
	return fn(conn)
}

// getAnyAvailableConnection gets a connection from ANY provider that isn't excluded.
// Phase 1: Non-blocking scan of all eligible providers (fast path)
// Phase 2: If all busy, race goroutines to get first available slot
//
// Tiering: providers with Backup=true are NOT considered until every
// non-backup ("primary") provider is excluded. A primary's pool being
// merely busy is not enough — the caller waits for a primary slot to free
// up rather than dipping into a backup. This matches the
// unlimited-primary + block-backup-for-completion model and prevents
// block providers from being billed for articles the primary could have
// served given a moment's patience.
//
// Within a tier, providers are still consumed opportunistically across
// hosts — so two unlimited primaries split load the way they do today.
func (c *Client) getAnyAvailableConnection(ctx context.Context, exclusions providerExclusions) (*Connection, config.UsenetProvider, error) {
	if err := c.waitBackgroundBudget(ctx); err != nil {
		return nil, config.UsenetProvider{}, err
	}
	budgetReserved := WorkClassFromContext(ctx) == WorkClassBackground && c.interactiveReserveActive()
	releaseBudget := func() {
		if budgetReserved {
			c.interactive.releaseBackgroundSlot()
			budgetReserved = false
		}
	}

	// Determine whether any primary is eligible first. Avoid building provider
	// slices on the common path: when a pool has a free slot, the first scan
	// returns immediately. A slice is only needed for the uncommon all-busy
	// fallback that races across providers.
	useBackups := true
	for _, p := range c.providers {
		if !p.Backup && !exclusions.excludes(p) {
			useBackups = false
			break
		}
	}

	// Phase 1: Non-blocking scan - try to get a free slot from any provider
	// within the current tier.
	eligibleCount := 0
	for _, provider := range c.providers {
		if provider.Backup != useBackups || exclusions.excludes(provider) {
			continue
		}
		eligibleCount++
		pp := c.pools[provider.Host]

		if WorkClassFromContext(ctx) == WorkClassBackground && c.interactiveReserveActive() {
			if !c.interactive.backgroundMayUseProvider(len(pp.slots), pp.max, c.TotalConnections()) {
				continue
			}
		}

		select {
		case pp.slots <- struct{}{}:
			// Got a slot - try to get or create connection
			conn, err := c.getOrCreateFromPool(ctx, pp, provider)
			if err != nil {
				<-pp.slots // Release slot on error
				continue   // Try next provider
			}
			c.markConnectionBudget(conn, ctx)
			budgetReserved = false
			return conn, provider, nil
		default:
			// Pool at capacity, try next provider
			continue
		}
	}

	if eligibleCount == 0 {
		releaseBudget()
		return nil, config.UsenetProvider{}, ErrNoEligibleProviders
	}

	// Phase 2: All providers in this tier busy - race for first available
	// slot in the tier. When the primary tier is in use this is the wait
	// that lets a backup remain idle rather than getting roped in.
	eligible := make([]config.UsenetProvider, 0, eligibleCount)
	for _, provider := range c.providers {
		if provider.Backup == useBackups && !exclusions.excludes(provider) {
			eligible = append(eligible, provider)
		}
	}
	conn, provider, err := c.raceForConnection(ctx, eligible)
	if err != nil {
		releaseBudget()
		return nil, provider, err
	}
	c.markConnectionBudget(conn, ctx)
	budgetReserved = false
	return conn, provider, nil
}

// raceForConnection spawns goroutines that race to acquire a connection slot.
// Returns as soon as any provider has availability.
//
// Each goroutine reports exactly one result (success or error) via resultCh, or
// exits silently if it never acquired a slot. A WaitGroup + channel-close ensures
// the receiver loop always terminates, and any extra connections won by multiple
// goroutines are properly returned to the pool — preventing slot leaks under heavy
// concurrent import load.
func (c *Client) raceForConnection(ctx context.Context, eligible []config.UsenetProvider) (*Connection, config.UsenetProvider, error) {
	type result struct {
		conn     *Connection
		provider config.UsenetProvider
		err      error
	}

	innerCtx, cancel := context.WithCancel(ctx)

	// Buffer for all possible results — goroutines that win the slot race send here.
	resultCh := make(chan result, len(eligible))
	var wg sync.WaitGroup

	for _, provider := range eligible {
		wg.Add(1)
		go func(p config.UsenetProvider) {
			defer wg.Done()
			pp := c.pools[p.Host]

			if err := c.waitProviderHeadroom(innerCtx, pp); err != nil {
				return
			}

			// Block waiting for slot (respects context)
			select {
			case pp.slots <- struct{}{}:
				// Got slot
			case <-innerCtx.Done():
				return // Context cancelled before we got a slot — no send needed
			}

			// Check if context was cancelled while we were waiting
			if innerCtx.Err() != nil {
				<-pp.slots // Release slot
				return
			}

			// Try to get or create connection
			conn, err := c.getOrCreateFromPool(innerCtx, pp, p)
			if err != nil {
				<-pp.slots // Release slot on error
				select {
				case resultCh <- result{nil, p, err}:
				case <-innerCtx.Done():
				}
				return
			}

			// Send the result; if the inner context was already cancelled (another
			// goroutine won), return our connection to the pool immediately.
			select {
			case resultCh <- result{conn, p, nil}:
				// Slot is still held — the receiver will call returnOrReleaseConn.
			case <-innerCtx.Done():
				c.put(conn, p) // releases slot
			}
		}(provider)
	}

	// Close resultCh once all goroutines have finished so the receiver loop below
	// can terminate without needing an explicit count.
	go func() {
		wg.Wait()
		close(resultCh)
		cancel()
	}()

	// Drain all results:
	//  - First success becomes the winner; cancel() is called to stop remaining goroutines.
	//  - Any subsequent successes (from goroutines that raced to completion before cancel
	//    reached them) have their connections returned to the pool to prevent slot leaks.
	//  - Errors are collected so we can return a meaningful error if there is no winner.
	var winConn *Connection
	var winProvider config.UsenetProvider
	var lastErr error

	for {
		select {
		case r, ok := <-resultCh:
			if !ok {
				// Channel closed — all goroutines have finished.
				if winConn != nil {
					return winConn, winProvider, nil
				}
				if lastErr != nil {
					return nil, config.UsenetProvider{}, lastErr
				}
				return nil, config.UsenetProvider{}, ErrNoProviderConnection
			}
			if r.err == nil && r.conn != nil {
				if winConn == nil {
					winConn = r.conn
					winProvider = r.provider
					cancel() // Tell losing goroutines to stop ASAP.
				} else {
					// Extra winner arrived before cancel propagated — release it.
					c.returnOrReleaseConn(r.conn, r.provider)
				}
			} else if r.err != nil {
				lastErr = r.err
			}
		case <-ctx.Done():
			// Parent context cancelled — cancel inner, drain remaining connections
			// in background so we don't block the caller.
			cancel()
			go func() {
				for r := range resultCh {
					if r.conn != nil {
						c.returnOrReleaseConn(r.conn, r.provider)
					}
				}
			}()
			return nil, config.UsenetProvider{}, ctx.Err()
		}
	}
}

// getOrCreateFromPool tries to get an existing connection from pool, or creates a new one.
// Caller must have already acquired a slot from pp.slots.
func (c *Client) getOrCreateFromPool(ctx context.Context, pp *ProviderPool, provider config.UsenetProvider) (*Connection, error) {
	// Try to get existing connection from pool (quick lock)
	for {
		pp.mu.Lock()
		if len(pp.conns) > 0 {
			// Pop from end (LIFO)
			n := len(pp.conns)
			entry := pp.conns[n-1]
			pp.conns[n-1] = nil // Avoid memory leak
			pp.conns = pp.conns[:n-1]
			pp.mu.Unlock()

			now := utils.Now()
			if c.isIdleExpired(entry.lastUsed, now) {
				conn := entry.conn
				releaseConnectionEntry(entry)
				_ = conn.Close()
				continue
			}

			// Health check outside lock
			if c.isHealthy(entry) {
				conn := entry.conn
				releaseConnectionEntry(entry)
				pp.activeConns.Store(conn, struct{}{}) // Register as active (checked-out)
				return conn, nil
			}
			// Unhealthy - close and try next pooled connection
			conn := entry.conn
			releaseConnectionEntry(entry)
			_ = conn.Close()
			continue
		}
		pp.mu.Unlock()
		break
	}

	// No pooled connection available, create new one
	conn, err := c.createConnection(ctx, provider)
	if err != nil {
		return nil, err
	}
	pp.activeConns.Store(conn, struct{}{}) // Register as active (checked-out)
	return conn, nil
}

// parseSockBuf converts a size string ("4MB") to a byte count for SO_RCVBUF/
// SO_SNDBUF. Empty/"0"/invalid/negative → 0, meaning "leave OS autotuning
// untouched". Clamped to a sane ceiling so a typo can't request gigabytes.
func parseSockBuf(s string) int {
	n, err := config.ParseSize(s)
	if err != nil || n <= 0 {
		return 0
	}
	const maxSockBuf = 256 << 20 // 256MB
	if n > maxSockBuf {
		n = maxSockBuf
	}
	return int(n)
}

// tuneTCP applies TCP_NODELAY and (re)applies the configured socket buffers
// on the established connection. The pre-connect Control hook does the work
// that matters for window scaling; this reinforces the sizes post-dial and
// covers the TLS-wrapped path. Sizes of 0 are skipped so OS autotuning is
// preserved when the operator opted into it.
func (c *Client) tuneTCP(tcpConn *net.TCPConn) {
	_ = tcpConn.SetNoDelay(true)
	if c.sockReadBuf > 0 {
		_ = tcpConn.SetReadBuffer(c.sockReadBuf)
	}
	if c.sockWriteBuf > 0 {
		_ = tcpConn.SetWriteBuffer(c.sockWriteBuf)
	}
}

// createConnection creates a new NNTP connection to a provider
// createPooledConnection acquires a connection through the same
// MaxConnections-limited pool that real downloads use, instead of dialing a
// raw connection outside the pool's slot accounting. Used by the provider
// health/speed-check paths so they can't push a provider's simultaneous
// connection count above its configured cap during heavy download activity
// (which either gets the extra connection rejected by the provider - a
// false "unhealthy" reading - or, on stricter providers, can disrupt an
// in-flight legitimate download).
//
// The returned release func MUST be called exactly once - never just
// conn.Close(), which would leak the acquired semaphore slot permanently
// and silently shrink the pool's effective MaxConnections by one until the
// process restarts.
func (c *Client) createPooledConnection(ctx context.Context, provider config.UsenetProvider) (*Connection, func(), error) {
	conn, provider, err := c.getConnectionFromProvider(ctx, provider)
	if err != nil {
		return nil, func() {}, err
	}
	release := func() { c.returnOrReleaseConn(conn, provider) }
	return conn, release, nil
}

func (c *Client) createConnection(ctx context.Context, provider config.UsenetProvider) (*Connection, error) {
	address := fmt.Sprintf("%s:%d", provider.Host, provider.Port)

	var netConn net.Conn
	var err error

	dialer := &net.Dialer{
		Timeout:   timeouts.DialTimeout,
		KeepAlive: timeouts.KeepAlive,
		Control:   c.socketControl(),
	}

	// TLS if enabled
	if provider.SSL {
		// Dial with TLS directly if possible, or Dial then Wrap
		tlsConfig := &tls.Config{
			ServerName:         provider.Host,
			InsecureSkipVerify: true,
			MinVersion:         tls.VersionTLS12,
		}
		// Use tls.Dialer for simpler timeout handling
		tlsDialer := &tls.Dialer{
			NetDialer: dialer,
			Config:    tlsConfig,
		}
		netConn, err = tlsDialer.DialContext(ctx, "tcp", address)
	} else {
		netConn, err = dialer.DialContext(ctx, "tcp", address)
	}

	if err != nil {
		return nil, NewConnectionError(fmt.Errorf("dial %s: %w", address, err))
	}

	// Optimize TCP socket (buffer sizing already applied pre-connect via
	// Dialer.Control; this reinforces it and covers the TLS-wrapped conn).
	if tcpConn, ok := netConn.(*net.TCPConn); ok {
		c.tuneTCP(tcpConn)
	}
	if tlsConn, ok := netConn.(*tls.Conn); ok {
		if tcpConn, ok := tlsConn.NetConn().(*net.TCPConn); ok {
			c.tuneTCP(tcpConn)
		}
	}

	reader := bufio.NewReaderSize(netConn, 512*1024)
	writer := bufio.NewWriterSize(netConn, 64*1024)

	conn := &Connection{
		conn:        netConn,
		reader:      reader,
		text:        textproto.NewReader(reader),
		writer:      writer,
		address:     provider.Host,
		port:        provider.Port,
		username:    provider.Username,
		password:    provider.Password,
		logger:      c.logger.With().Str("host", provider.Host).Logger(),
		providerKey: canonicalProviderKey(provider),
	}
	if c.providerMonitor != nil {
		conn.recordUsage = c.providerMonitor.RecordUsage
	}

	// Set deadline for handshake (greeting + auth)
	// If the server doesn't respond quickly during setup, we should abort.
	_ = netConn.SetDeadline(utils.Now().Add(timeouts.HandshakeTimeout))

	// Read greeting
	line, err := reader.ReadString('\n')
	if err != nil {
		_ = netConn.Close()
		return nil, NewConnectionError(fmt.Errorf("read greeting: %w", err))
	}
	if !strings.HasPrefix(line, "200") && !strings.HasPrefix(line, "201") {
		_ = netConn.Close()
		return nil, NewConnectionError(fmt.Errorf("unexpected greeting: %s", line))
	}

	// Authenticate
	if provider.Username != "" {
		if err := conn.authenticate(); err != nil {
			_ = netConn.Close()
			return nil, fmt.Errorf("auth: %w", err)
		}
	}

	// Clear deadline for normal operation
	_ = netConn.SetDeadline(time.Time{})

	return conn, nil
}

// reaper periodically closes idle connections and tops up each provider's
// idle pool toward its configured MinIdleConnections floor.
func (c *Client) reaper() {
	ticker := time.NewTicker(timeouts.ReaperInterval)
	defer ticker.Stop()

	for range ticker.C {
		if c.closed.Load() {
			return
		}
		c.reapIdleConnections()
		c.warmIdlePools()
	}
}

// warmIdlePools opportunistically dials and pools new idle connections for
// any provider configured with MinIdleConnections > 0, so the first request
// of a session (or any request after the pool has gone fully idle) finds an
// already-dialed connection instead of paying a fresh TCP+TLS+AUTH handshake
// before a single byte can be requested. Default MinIdleConnections is 0
// (off), matching the prior purely-reactive behavior.
func (c *Client) warmIdlePools() {
	for _, pp := range c.pools {
		c.warmProviderPool(pp)
	}
}

func (c *Client) warmProviderPool(pp *ProviderPool) {
	if pp.config.MinIdleConnections <= 0 {
		return
	}
	for {
		if c.closed.Load() {
			return
		}
		pp.mu.Lock()
		idle := len(pp.conns)
		pp.mu.Unlock()
		if idle >= pp.config.MinIdleConnections {
			return
		}

		// Best-effort, non-blocking slot acquire: warming must never compete
		// with real traffic for a busy pool. If the pool is fully checked
		// out right now, just try again on the next reaper tick.
		select {
		case pp.slots <- struct{}{}:
		default:
			return
		}

		conn, err := c.createConnection(context.Background(), pp.config)
		if err != nil {
			<-pp.slots
			c.logger.Debug().Err(err).Str("host", pp.config.Host).Msg("Pool warmup: failed to dial idle connection")
			return
		}
		c.put(conn, pp.config)
	}
}

func (c *Client) reapIdleConnections() {
	now := utils.Now()
	for _, pp := range c.pools {
		var toClose, toPing []*connectionEntry

		pp.mu.Lock()
		kept := pp.conns[:0]
		for _, entry := range pp.conns {
			switch {
			case c.isIdleExpired(entry.lastUsed, now):
				toClose = append(toClose, entry)
			case now.Sub(entry.lastActivity()) > c.pingInterval:
				// Candidate for a keepalive ping. Take a connection slot so
				// the provider's total stays capped while the entry is out of
				// the pool being pinged — otherwise a checkout burst could
				// dial replacements and briefly exceed max_connections. If no
				// slot is free the pool is busy and the conn will either be
				// used (real activity) or expire soon; skip this sweep.
				select {
				case pp.slots <- struct{}{}:
					toPing = append(toPing, entry)
				default:
					kept = append(kept, entry)
				}
			default:
				kept = append(kept, entry)
			}
		}
		// Nil out trailing pointers to help GC
		for i := len(kept); i < len(pp.conns); i++ {
			pp.conns[i] = nil
		}
		pp.conns = kept
		pp.mu.Unlock()

		for _, entry := range toClose {
			conn := entry.conn
			releaseConnectionEntry(entry)
			_ = conn.Close()
		}
		// Ping outside the pool lock — a DATE round-trip per connection must
		// not block checkouts.
		for _, entry := range toPing {
			c.keepAlive(pp, entry, now)
		}
	}
}

// keepAlive pings an idle connection that was removed from the pool (with a
// slot held) and returns it on success. Failed pings close the connection —
// exactly the sessions the old aggressive idle timeout existed to avoid
// handing out, caught here without sacrificing the warm pool.
func (c *Client) keepAlive(pp *ProviderPool, entry *connectionEntry, now time.Time) {
	discard := func() {
		conn := entry.conn
		releaseConnectionEntry(entry)
		_ = conn.Close()
		<-pp.slots // Release slot
	}

	if err := entry.conn.ping(); err != nil {
		c.logger.Debug().Err(err).Str("provider", entry.provider.Host).
			Msg("keepalive ping failed, closing idle connection")
		discard()
		return
	}
	entry.lastPing = now

	pp.mu.Lock()
	if c.closed.Load() || len(pp.conns) >= pp.max {
		pp.mu.Unlock()
		discard()
		return
	}
	pp.conns = append(pp.conns, entry)
	pp.mu.Unlock()
	<-pp.slots // Release slot - connection is available again
}

// Stats returns current pool statistics
func (c *Client) Stats() map[string]interface{} {
	if c.closed.Load() {
		return nil
	}

	stats := make(map[string]interface{})
	providers := make([]map[string]interface{}, 0, len(c.providers))

	totalActive := 0
	totalIdle := 0
	totalMax := 0

	for _, p := range c.providers {
		pp, ok := c.pools[p.Host]
		if !ok {
			continue
		}

		pp.mu.Lock()
		idle := len(pp.conns)
		pp.mu.Unlock()

		// Active = slots in use (tokens in the semaphore channel)
		active := len(pp.slots)
		maxC := pp.max

		totalActive += active
		totalIdle += idle
		totalMax += maxC

		providerInfo := map[string]interface{}{
			"host":            p.Host,
			"port":            p.Port,
			"max_connections": maxC,
			"active":          active,
			"idle":            idle,
			"ssl":             p.SSL,
		}

		if p.SubscriptionExpiry != "" {
			providerInfo["subscription_expiry"] = p.SubscriptionExpiry
			providerInfo["auto_renew"] = p.AutoRenew
		}

		if c.providerMonitor != nil {
			health, usage := c.providerMonitor.Stats(p)
			providerInfo["provider_key"] = canonicalProviderKey(p)
			providerInfo["health"] = health
			providerInfo["usage"] = usage
		}

		// Add speed test result if available
		if result, ok := c.speedTestResults.Load(p.Host); ok {
			providerInfo["speed_test"] = map[string]interface{}{
				"latency_ms": result.LatencyMs,
				"speed_mbps": result.SpeedMBps,
				"bytes_read": result.BytesRead,
				"tested_at":  result.TestedAt.Format("2006-01-02T15:04:05Z07:00"),
				"error":      result.Error,
			}
		}

		providers = append(providers, providerInfo)
	}

	poolStats := map[string]interface{}{
		"max_connections": totalMax,
		"total_created":   totalActive + totalIdle,
		"active":          totalActive,
		"idle":            totalIdle,
	}

	stats["pool"] = poolStats
	stats["providers"] = providers

	if c.interactive != nil {
		snap := c.interactive.snapshot()
		stats["interactive"] = map[string]interface{}{
			"enabled":              snap.Enabled,
			"active":               snap.Active,
			"active_streams":       snap.ActiveStreams,
			"per_stream_reserve":   snap.PerStreamReserve,
			"reserved":             snap.Reserved,
			"background_budget":    snap.BackgroundBudget,
			"background_in_use":    snap.BackgroundInUse,
			"stream_in_use":        snap.StreamInUse,
			"reserve_percent":      snap.ReservePercent,
			"reserve_min":          snap.ReserveMin,
			"reserve_max":          snap.ReserveMax,
			"reserve_per_stream":   snap.ReservePerStream,
			"last_entry":           snap.LastEntry,
			"last_file":            snap.LastFile,
			"last_client":          snap.LastClient,
			"started_at":           snap.StartedAt,
		}
	}

	return stats
}

func (c *Client) Stat(ctx context.Context, messageID string) (int, string, error) {
	if c.closed.Load() {
		return 0, "", errors.New("nntp client is closed")
	}

	var num int
	var id string
	err := c.ExecuteWithFailover(ctx, func(conn *Connection) error {
		n, echoed, statErr := conn.Stat(messageID)
		if statErr != nil {
			return statErr
		}
		num = n
		id = echoed
		return nil
	})
	return num, id, err
}

type batchStatState struct {
	sawNotFound bool
	sawOtherErr bool
	lastErr     error
	exclusions  providerExclusions
}

type providerExclusions struct {
	hosts     map[string]struct{}
	backbones map[string]struct{}
}

func (e *providerExclusions) excludeHost(host string) {
	if host == "" {
		return
	}
	if e.hosts == nil {
		e.hosts = make(map[string]struct{})
	}
	e.hosts[host] = struct{}{}
}

func (e *providerExclusions) excludeBackbone(backbone string) {
	backbone = normalizeBackbone(backbone)
	if backbone == "" {
		return
	}
	if e.backbones == nil {
		e.backbones = make(map[string]struct{})
	}
	e.backbones[backbone] = struct{}{}
}

func (e providerExclusions) excludes(provider config.UsenetProvider) bool {
	// Fast path: the overwhelming majority of acquisitions happen with
	// no exclusions in flight (first attempt before any failover). Skip
	// the map lookups and backbone work entirely.
	if e.hosts == nil && e.backbones == nil {
		return false
	}
	if _, ok := e.hosts[provider.Host]; ok {
		return true
	}
	// Backbone is pre-normalized at NewClient time, so no per-call
	// strings.ToLower / TrimSpace allocation here.
	if provider.Backbone == "" {
		return false
	}
	_, ok := e.backbones[provider.Backbone]
	return ok
}

func normalizeBackbone(backbone string) string {
	return strings.ToLower(strings.TrimSpace(backbone))
}

func excludeForArticleNotFound(exclusions *providerExclusions, provider config.UsenetProvider) {
	if exclusions == nil {
		return
	}
	// Backbone is pre-normalized at NewClient time, so we read it raw.
	if provider.Backbone != "" {
		exclusions.excludeBackbone(provider.Backbone)
		return
	}
	exclusions.excludeHost(provider.Host)
}

// BatchStat checks the availability of many message IDs using NNTP STAT. Each
// worker holds one repair-bank token for its lifetime, so the total number of
// concurrent NNTP connections used by all in-flight BatchStat calls never
// exceeds the bank's capacity. When the client has no bank configured, a small
// default worker count is used. Does NOT fail-fast: every chunk is processed so
// the caller sees complete per-segment visibility.
func (c *Client) BatchStat(ctx context.Context, messageIDs []string) (*BatchStatResult, error) {
	if c.closed.Load() {
		return nil, errors.New("nntp client is closed")
	}
	if len(messageIDs) == 0 {
		return &BatchStatResult{}, nil
	}

	ctx = WithWorkClass(ctx, WorkClassBackground)

	// Early-bailout: cancelled the moment a segment is found definitively
	// missing (not-found across all providers), so the remaining sample's
	// workers stop at their per-chunk ctx.Err() checks instead of completing
	// the full STAT sweep. defer cancel() also covers the normal return path.
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	// Per-chunk batch size is adaptive: we want enough chunks to keep
	// every pool worker busy on this BatchStat call, but not so few IDs
	// per chunk that we pay the per-chunk overhead (provider-acquire
	// round-trips, per-call slice allocations in batchStatAcrossProviders,
	// callback dispatch) on near-nothing.
	//
	// Ceiling: keeps cancellation latency and connection-drop blast
	// radius bounded for a single chunk's worth of STATs.
	// Floor: smaller than this and per-chunk overhead starts dominating
	// the actual STAT round-trip.
	const (
		statBatchSize    = 50
		statBatchMinSize = 10
	)
	batchSize := pickStatBatchSize(len(messageIDs), c.repairPool.Capacity(), statBatchSize, statBatchMinSize)

	type chunk struct {
		startIdx   int
		messageIDs []string
	}
	chunks := make([]chunk, 0, (len(messageIDs)+batchSize-1)/batchSize)
	for i := 0; i < len(messageIDs); i += batchSize {
		end := min(i+batchSize, len(messageIDs))
		chunks = append(chunks, chunk{startIdx: i, messageIDs: messageIDs[i:end]})
	}

	allResults := make([]StatResult, len(messageIDs))
	for i, msgID := range messageIDs {
		allResults[i].MessageID = msgID
	}

	// Each chunk submits to the shared RepairPool. Concurrency is bounded
	// by the pool's worker count, NOT by a per-call pool — so M concurrent
	// BatchStat calls share the same pool.Capacity() workers in FIFO
	// arrival order instead of each spinning up its own bank-sized pool.
	// Tasks write disjoint index ranges of allResults; the only shared
	// mutable state is the early-bailout cancel.
	markChunkErr := func(startIdx, n int, e error) {
		for i := startIdx; i < startIdx+n; i++ {
			allResults[i].Available = false
			allResults[i].Error = e
		}
	}
	var bailOnce sync.Once
	var wg sync.WaitGroup
	for _, ch := range chunks {
		ch := ch
		wg.Add(1)
		err := c.repairPool.Submit(ctx, ch.messageIDs, func(results []StatResult, taskErr error) {
			defer wg.Done()
			if taskErr != nil {
				// Mirrors the previous behaviour: a chunk-level connection
				// error fails the whole chunk (partial results discarded).
				markChunkErr(ch.startIdx, len(ch.messageIDs), taskErr)
				return
			}
			for i := range results {
				allResults[ch.startIdx+i] = results[i]
			}
			// Bail out the rest of the sample as soon as one segment is
			// definitively missing — not-found on every provider, so the
			// terminal classification carries an ArticleNotFound error.
			// Per-segment provider failover has already completed inside
			// this chunk before we get here, so this never short-circuits
			// failover.
			for _, r := range results {
				if !r.Available && IsArticleNotFoundError(r.Error) {
					bailOnce.Do(cancel)
					break
				}
			}
		})
		if err != nil {
			// Submit refused the task — caller's ctx expired before a worker
			// took it, or the pool is shutting down. Synthesize a chunk-wide
			// error so the result vector still has the right shape.
			markChunkErr(ch.startIdx, len(ch.messageIDs), err)
			wg.Done()
		}
	}
	wg.Wait()

	result := &BatchStatResult{
		Results:    allResults,
		TotalCount: len(messageIDs),
	}
	for _, r := range allResults {
		if r.Available {
			result.FoundCount++
			continue
		}
		if r.Error == nil {
			continue
		}
		// Article-not-found doesn't count as an error for the caller's
		// availability decision; only true connection/protocol failures do.
		var nntpErr *Error
		if errors.As(r.Error, &nntpErr) && nntpErr.Type == ErrorTypeArticleNotFound {
			continue
		}
		result.ErrorCount++
	}
	return result, nil
}

func (c *Client) batchStatAcrossProviders(ctx context.Context, messageIDs []string) ([]StatResult, error) {
	results := make([]StatResult, len(messageIDs))
	states := make([]batchStatState, len(messageIDs))
	unresolved := make([]int, len(messageIDs))
	for i, msgID := range messageIDs {
		results[i].MessageID = msgID
		unresolved[i] = i
	}

	for _, provider := range c.providers {
		if len(unresolved) == 0 {
			break
		}
		if ctx.Err() != nil {
			return results, ctx.Err()
		}

		queryIdxs := make([]int, 0, len(unresolved))
		chunkIDs := make([]string, 0, len(unresolved))
		for _, idx := range unresolved {
			if states[idx].exclusions.excludes(provider) {
				continue
			}
			queryIdxs = append(queryIdxs, idx)
			chunkIDs = append(chunkIDs, messageIDs[idx])
		}
		if len(queryIdxs) == 0 {
			continue
		}

		providerResults, err := c.batchStatOnProvider(ctx, provider, chunkIDs)
		if err != nil && len(providerResults) == 0 {
			for _, idx := range queryIdxs {
				states[idx].sawOtherErr = true
				states[idx].lastErr = err
			}
			continue
		}

		nextUnresolved := make([]int, 0, len(unresolved))
		queryPos := 0
		for _, idx := range unresolved {
			if states[idx].exclusions.excludes(provider) {
				nextUnresolved = append(nextUnresolved, idx)
				continue
			}

			if queryPos >= len(providerResults) {
				states[idx].sawOtherErr = true
				if err != nil {
					states[idx].lastErr = err
				} else {
					states[idx].lastErr = NewConnectionError(fmt.Errorf("provider %s returned incomplete batch results", provider.Host))
				}
				nextUnresolved = append(nextUnresolved, idx)
				continue
			}

			res := providerResults[queryPos]
			queryPos++
			if res.Available {
				results[idx] = res
				continue
			}

			var nntpErr *Error
			if res.Error != nil && errors.As(res.Error, &nntpErr) && nntpErr.Type == ErrorTypeArticleNotFound {
				states[idx].sawNotFound = true
				excludeForArticleNotFound(&states[idx].exclusions, provider)
			} else {
				states[idx].sawOtherErr = true
				if res.Error != nil {
					states[idx].lastErr = res.Error
				} else if err != nil {
					states[idx].lastErr = err
				} else {
					states[idx].lastErr = NewConnectionError(fmt.Errorf("provider %s returned an empty STAT result for %s", provider.Host, res.MessageID))
				}
			}
			nextUnresolved = append(nextUnresolved, idx)
		}
		unresolved = nextUnresolved
	}

	for _, idx := range unresolved {
		switch {
		case states[idx].sawNotFound && !states[idx].sawOtherErr:
			results[idx].Available = false
			results[idx].Error = classifyNNTPError(430, fmt.Sprintf("segment %s not found on any provider", results[idx].MessageID))
		case states[idx].lastErr != nil:
			results[idx].Available = false
			results[idx].Error = states[idx].lastErr
		case states[idx].sawNotFound:
			results[idx].Available = false
			results[idx].Error = NewConnectionError(fmt.Errorf("segment %s not found on some providers but could not be verified on others", results[idx].MessageID))
		default:
			results[idx].Available = false
			results[idx].Error = NewConnectionError(fmt.Errorf("segment %s could not be verified on any provider", results[idx].MessageID))
		}
	}

	return results, nil
}

func (c *Client) batchStatOnProvider(ctx context.Context, provider config.UsenetProvider, messageIDs []string) ([]StatResult, error) {
	conn, providerCfg, err := c.getConnectionFromProvider(ctx, provider)
	if err != nil {
		return nil, err
	}

	results := make([]StatResult, len(messageIDs))
	for i, msgID := range messageIDs {
		results[i].MessageID = msgID
		if ctx.Err() != nil {
			results[i].Available = false
			results[i].Error = ctx.Err()
			c.release(conn)
			return results, ctx.Err()
		}

		_, _, statErr := conn.Stat(msgID)
		if statErr == nil {
			results[i].Available = true
			continue
		}

		results[i].Available = false
		results[i].Error = statErr

		var nntpErr *Error
		if errors.As(statErr, &nntpErr) && nntpErr.Type != ErrorTypeConnection && nntpErr.Type != ErrorTypeTimeout {
			continue
		}

		connErr := NewConnectionError(fmt.Errorf("failed to STAT %s at %d/%d: %w", msgID, i+1, len(messageIDs), statErr))
		results[i].Error = connErr
		for j := i + 1; j < len(messageIDs); j++ {
			results[j].MessageID = messageIDs[j]
			results[j].Available = false
			results[j].Error = connErr
		}
		c.release(conn)
		return results, connErr
	}

	c.returnOrReleaseConn(conn, providerCfg)
	return results, nil
}

// Close shuts down the connection manager
func (c *Client) Close() error {
	if c.closed.Swap(true) {
		return nil
	}

	var totalClosed int
	for _, pp := range c.pools {
		pp.mu.Lock()
		// Close idle connections
		for _, entry := range pp.conns {
			_ = entry.conn.Close()
			releaseConnectionEntry(entry)
			totalClosed++
		}
		pp.conns = nil
		pp.mu.Unlock()

		// Force-close active (checked-out) connections to unblock any in-flight operations.
		// This causes StreamBody/sendCommand reads to fail immediately, allowing prefetch
		// workers to exit and SegmentFetcher.Close() to complete without hanging.
		pp.activeConns.Range(func(key, _ any) bool {
			_ = key.(*Connection).Close()
			totalClosed++
			return true
		})
	}

	// Stop the BatchStat worker pool last — its workers may be holding
	// connections we just force-closed, which makes them return with
	// errors and exit cleanly.
	c.repairPool.Stop()
	if c.providerMonitor != nil {
		c.providerMonitor.Close()
	}

	c.logger.Info().
		Int("total_closed", totalClosed).
		Msg("Connection manager closed")

	return nil
}

// SpeedTest runs a speed test for a specific provider.
func (c *Client) SpeedTest(ctx context.Context, providerHost string, messageIDs []string) SpeedTestResult {
	result := SpeedTestResult{
		Provider: providerHost,
		TestedAt: utils.Now(),
	}

	// Find the provider by host
	var targetProvider *config.UsenetProvider
	for i := range c.providers {
		if c.providers[i].Host == providerHost {
			targetProvider = &c.providers[i]
			break
		}
	}

	if targetProvider == nil {
		result.Error = "provider not found"
		c.speedTestResults.Store(providerHost, result)
		return result
	}
	defer func() {
		if result.Error == "" && result.SpeedMBps > 0 {
			c.speedTestResults.Store(providerHost, result)
		}
		if c.providerMonitor != nil && result.Error == "" && result.SpeedMBps > 0 {
			c.providerMonitor.setSpeedTest(*targetProvider, result)
		}
	}()

	// Create connection (through the same pooled/MaxConnections-limited path
	// real downloads use, so a manual speed test can't push this provider's
	// connection count above its configured cap during active downloads).
	connectionStarted := time.Now()
	conn, release, err := c.createPooledConnection(ctx, *targetProvider)
	if err != nil {
		result.Error = fmt.Sprintf("connection failed: %v", err)
		if c.providerMonitor != nil {
			c.providerMonitor.setHealth(canonicalProviderKey(*targetProvider), ProviderHealth{
				Status: "unhealthy", CheckedAt: result.TestedAt.Format(time.RFC3339),
				Error: sanitizeProviderError(err, *targetProvider),
			})
		}
		return result
	}
	connectionLatency := time.Since(connectionStarted).Milliseconds()
	defer release()

	// Measure latency using ping (true network RTT)
	pingStart := utils.Now()
	if err := conn.ping(); err != nil {
		message := sanitizeProviderError(err, *targetProvider)
		if strings.Contains(strings.ToLower(message), "unexpected date response") {
			// DATE is optional on some otherwise usable providers. Use the full
			// connection/TLS/authentication time as the latency measurement and
			// continue to the BODY throughput test.
			result.LatencyMs = connectionLatency
			if c.providerMonitor != nil {
				c.providerMonitor.setHealth(canonicalProviderKey(*targetProvider), ProviderHealth{
					Status: "healthy", CheckedAt: result.TestedAt.Format(time.RFC3339),
					Detail: "Authenticated successfully; DATE is not supported by this server",
				})
			}
		} else {
			result.Error = fmt.Sprintf("ping failed: %v", err)
			if c.providerMonitor != nil {
				c.providerMonitor.setHealth(canonicalProviderKey(*targetProvider), ProviderHealth{
					Status: "unknown", CheckedAt: result.TestedAt.Format(time.RFC3339), Error: message,
				})
			}
			return result
		}
	} else {
		result.LatencyMs = time.Since(pingStart).Milliseconds()
		if c.providerMonitor != nil {
			latency := result.LatencyMs
			c.providerMonitor.setHealth(canonicalProviderKey(*targetProvider), ProviderHealth{
				Status: "healthy", CheckedAt: result.TestedAt.Format(time.RFC3339), LatencyMs: &latency,
			})
		}
	}

	measured := measureProviderSpeed(ctx, *targetProvider, messageIDs, result.TestedAt, result.LatencyMs, c.createPooledConnection)
	result.SpeedMBps = measured.SpeedMBps
	result.BytesRead = measured.BytesRead
	result.Error = measured.Error

	return result
}

func measureProviderSpeed(
	ctx context.Context,
	provider config.UsenetProvider,
	messageIDs []string,
	testedAt time.Time,
	latencyMs int64,
	openConnection func(context.Context, config.UsenetProvider) (*Connection, func(), error),
) SpeedTestResult {
	result := SpeedTestResult{Provider: provider.Host, TestedAt: testedAt, LatencyMs: latencyMs}
	if len(messageIDs) == 0 {
		result.Error = "no NZB article is available for a throughput test"
		return result
	}

	var lastErr error
	for _, messageID := range messageIDs {
		if ctx.Err() != nil {
			lastErr = ctx.Err()
			break
		}
		conn, release, err := openConnection(ctx, provider)
		if err != nil {
			lastErr = err
			continue
		}
		downloadStart := time.Now()
		data, downloadErr := conn.GetBody(messageID)
		downloadDuration := time.Since(downloadStart)
		release()
		if downloadErr != nil {
			lastErr = downloadErr
			continue
		}
		result.BytesRead = int64(len(data))
		if downloadDuration > 0 && result.BytesRead > 0 {
			result.SpeedMBps = float64(result.BytesRead) / downloadDuration.Seconds() / (1024 * 1024)
			return result
		}
		lastErr = errors.New("article body was empty")
	}

	result.Error = "no usable NZB article was found for this provider"
	if lastErr != nil {
		result.Error += ": " + sanitizeProviderError(lastErr, provider)
	}
	return result
}

// GetSpeedTestResults returns all stored speed test results
func (c *Client) GetSpeedTestResults() map[string]SpeedTestResult {
	results := make(map[string]SpeedTestResult)
	c.speedTestResults.Range(func(host string, result SpeedTestResult) bool {
		results[host] = result
		return true
	})
	return results
}

// FlushProviderMetrics persists any usage reported after connection shutdown.
// Usenet calls this after all active readers have completed cleanup.
func (c *Client) FlushProviderMetrics() {
	if c.providerMonitor != nil {
		c.providerMonitor.Flush()
	}
}

// GetSpeedTestResult returns the speed test result for a specific provider
func (c *Client) GetSpeedTestResult(providerHost string) (SpeedTestResult, bool) {
	return c.speedTestResults.Load(providerHost)
}
