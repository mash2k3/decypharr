package config

import (
	"errors"
	"fmt"
	"path/filepath"
	"strconv"
)

// DeobfuscateMode controls how obfuscated NZB files are renamed.
type DeobfuscateMode string

const (
	DeobfuscateModeOff      DeobfuscateMode = ""
	DeobfuscateModeIndex    DeobfuscateMode = "index"
	DeobfuscateModeSeasonEp DeobfuscateMode = "season_ep"
)

func (m DeobfuscateMode) IsValid() bool {
	switch m {
	case DeobfuscateModeOff, DeobfuscateModeIndex, DeobfuscateModeSeasonEp:
		return true
	}
	return false
}

type UsenetProvider struct {
	Host           string `json:"host,omitempty"` // Host of the usenet server
	Port           int    `json:"port,omitempty"` // Port of the usenet server
	Username       string `json:"username,omitempty"`
	Password       string `json:"password,omitempty"`
	Backbone       string `json:"backbone,omitempty"`        // Shared article backbone identifier used for failover decisions
	MaxConnections int    `json:"max_connections,omitempty"` // Max connections for this provider (default: 10)
	SSL            bool   `json:"ssl,omitempty"`             // Use SSL/TLS for the connection
	// MinIdleConnections keeps this many idle connections pre-dialed and
	// pooled at all times, so the first file open of a session (or any open
	// after the pool has gone fully idle) doesn't pay a fresh TCP+TLS+AUTH
	// handshake before the first byte can be requested. Default 0 (off) -
	// purely reactive dialing, matching prior behavior.
	MinIdleConnections int `json:"min_idle_connections,omitempty"`
	Priority           int `json:"priority,omitempty"` // Priority for this provider (lower = higher priority)
	// Backup marks this provider as a fallback tier. Backups are only
	// consulted when every non-backup ("primary") provider is excluded
	// — e.g. all primaries returned article-not-found or had connection
	// errors. They are NOT used just because a primary's pool is busy;
	// the request waits for a primary slot instead. This matches the
	// "unlimited primary + block backup for completion" model that most
	// other Usenet clients implement, and prevents block providers from
	// being billed for articles the unlimited could have served.
	Backup bool `json:"backup,omitempty"`

	// SubscriptionExpiry/AutoRenew are informational only - purely for
	// surfacing subscription status in Settings/Stats so users can keep
	// track of when a provider's plan runs out. Never read by any
	// connection/download logic.
	SubscriptionExpiry string `json:"subscription_expiry,omitempty"` // Optional plan end date, e.g. "2026-12-31"
	AutoRenew          bool   `json:"auto_renew,omitempty"`          // Whether the subscription is set to auto-renew
}

// Usenet configuration for usenet streaming and downloading
type Usenet struct {
	Providers []UsenetProvider `json:"providers,omitempty"` // Usenet provider configurations
	// Per-stream/file configuration
	MaxConnections           int `json:"max_connections,omitempty"`            // Maximum concurrent connections per streaming file (default: 15)
	ProcessingMaxConnections int `json:"processing_max_connections,omitempty"` // Maximum concurrent connections per file for parsing and NZB downloads (default: max_connections)
	// Read-ahead configuration
	ReadAhead string `json:"read_ahead,omitempty"` // Bytes to prefetch ahead of streaming reads e.g. "16MB", "32MB" (default: 16MB)
	// PrefetchAheadSegments is how many segments beyond the requested range
	// the streaming reader queues for read-ahead. Default 0 -> 8 (prior
	// hardcoded value).
	PrefetchAheadSegments int `json:"prefetch_ahead_segments,omitempty"`
	// PreCacheOnOpen pre-fetches the head and tail of a Usenet file whenever
	// the mount receives an open. Keep this opt-in: library scanners can open
	// many newly-added episodes at once and otherwise compete with playback
	// for the shared NNTP connection pool.
	PreCacheOnOpen bool `json:"pre_cache_on_open,omitempty"`
	// InteractivePoolReserveEnabled opts into dynamic NNTP pool reservation during
	// sustained playback reads. When active, background parse/repair/stat work is
	// capped so streaming keeps headroom. Default off.
	InteractivePoolReserveEnabled bool `json:"interactive_pool_reserve_enabled,omitempty"`
	// InteractivePoolReservePercent is the share of total provider slots to hold
	// per active stream when reserve mode is active (default 15).
	InteractivePoolReservePercent int `json:"interactive_pool_reserve_percent,omitempty"`
	// InteractivePoolReservePerStream optionally overrides the computed per-stream
	// reserve. When zero, derived from InteractivePoolReservePercent.
	InteractivePoolReservePerStream int `json:"interactive_pool_reserve_per_stream,omitempty"`
	// InteractivePoolReserveMin is the floor for computed reserve (default 6).
	InteractivePoolReserveMin int `json:"interactive_pool_reserve_min,omitempty"`
	// InteractivePoolReserveMax caps total reserve across all active streams (default 40).
	InteractivePoolReserveMax int `json:"interactive_pool_reserve_max,omitempty"`
	// InteractiveDetectBytes is how many qualifying bytes in the detect window
	// trigger interactive mode (default 4MB).
	InteractiveDetectBytes string `json:"interactive_detect_bytes,omitempty"`
	// InteractiveDetectWindow is the sliding window for sustained-read detection.
	InteractiveDetectWindow string `json:"interactive_detect_window,omitempty"`
	// InteractiveIdleTimeout exits interactive mode after no qualifying reads.
	InteractiveIdleTimeout string `json:"interactive_idle_timeout,omitempty"`
	// SocketReadBuffer / SocketWriteBuffer set the per-connection TCP
	// SO_RCVBUF / SO_SNDBUF (e.g. "4MB"). At high RTT a single connection's
	// throughput is capped at roughly buffer ÷ RTT, so the receive buffer must
	// cover the bandwidth-delay product (BDP = link_speed × RTT). "0" leaves
	// OS autotuning in charge. Note: the OS still caps these
	// (Linux net.core.rmem_max/wmem_max, macOS kern.ipc.maxsockbuf) — raise
	// those sysctls too to actually get large windows. Defaults: 4MB / 1MB.
	SocketReadBuffer  string `json:"socket_read_buffer,omitempty"`
	SocketWriteBuffer string `json:"socket_write_buffer,omitempty"`
	// Processing timeout
	ProcessingTimeout string `json:"processing_timeout,omitempty"` // Timeout for NZB processing e.g. "5m", "10m" (default: 10m). Mark as bad if exceeded.
	// ConnIdleTimeout is how long an unused pooled NNTP connection is kept
	// warm (and keepalive-pinged) before being closed, e.g. "5m". Players
	// read in bursts with quiet gaps; closing too early forces a
	// TCP+TLS+AUTH reconnect storm on every resume. Default: 5m.
	ConnIdleTimeout string `json:"conn_idle_timeout,omitempty"`
	// Availability check sampling
	AvailabilitySamplePercent       int    `json:"availability_sample_percent,omitempty"`        // Percentage of segments to check during repair (1-100, default: 10)
	ImportAvailabilitySamplePercent int    `json:"import_availability_sample_percent,omitempty"` // Percentage of segments to check when adding an NZB (1-100, default: 1)
	DiskBufferPath                  string `json:"disk_buffer_path,omitempty"`                   // Path for disk buffer storage (empty = main_path/usenet/streams)

	// BufferMemory caps the total RAM the usenet streaming buffers hold across
	// all open streams, e.g. "512MB". Per-stream buffers stay generous for
	// smooth playback; this bounds the aggregate so many concurrent streams
	// can't OOM. Empty = default (512MB); "0" disables the cap.
	BufferMemory string `json:"buffer_memory,omitempty"`

	SkipRepair bool `json:"skip_repair,omitempty"` // Skip repairing nzb/usenet files

	DeobfuscateMode DeobfuscateMode `json:"deobfuscate_mode,omitempty"` // Renaming mode for obfuscated files
}

// BufferMemoryBytes resolves the usenet streaming-buffer RAM cap. Empty ->
// 512MB default; "0" -> disabled (0).
func (u Usenet) BufferMemoryBytes() int64 {
	if u.BufferMemory == "" {
		return 512 << 20
	}
	n, err := ParseSize(u.BufferMemory)
	if err != nil {
		return 512 << 20
	}
	return n
}

func (u Usenet) IsZero() bool {
	return len(u.Providers) == 0 && u.MaxConnections == 0 && u.ProcessingMaxConnections == 0 && u.ReadAhead == "" && u.ProcessingTimeout == "" && !u.PreCacheOnOpen && !u.InteractivePoolReserveEnabled
}

// InteractiveDetectBytesValue resolves the sustained-read byte threshold.
func (u Usenet) InteractiveDetectBytesValue() int64 {
	if u.InteractiveDetectBytes == "" {
		return 4 << 20
	}
	n, err := ParseSize(u.InteractiveDetectBytes)
	if err != nil || n <= 0 {
		return 4 << 20
	}
	return n
}

// ComputeInteractiveReserve returns reserved slots for interactive work given
// total provider connection capacity. Kept for backward compatibility.
func ComputeInteractiveReserve(totalConnections, percent, minReserve, maxReserve int) int {
	reserved, _ := ComputeDynamicInteractiveReserve(totalConnections, 1, percent, minReserve, maxReserve, 0)
	return reserved
}

// ComputePerStreamReserveBase returns the per-stream reserve baseline from percent/min.
func ComputePerStreamReserveBase(totalConnections, percent, minReserve int) int {
	if totalConnections <= 0 {
		return 0
	}
	if percent <= 0 {
		percent = 15
	}
	if minReserve <= 0 {
		minReserve = 6
	}
	reserve := (totalConnections*percent + 99) / 100
	if reserve < minReserve {
		reserve = minReserve
	}
	if reserve >= totalConnections {
		return totalConnections
	}
	return reserve
}

// ComputeDynamicInteractiveReserve scales reserve by active playback streams.
// perStreamOverride, when > 0, replaces the percent-derived per-stream baseline.
func ComputeDynamicInteractiveReserve(totalConnections, activeStreams, percent, minReserve, maxTotal, perStreamOverride int) (reserved, perStream int) {
	if totalConnections <= 0 || activeStreams <= 0 {
		return 0, 0
	}
	if maxTotal <= 0 {
		maxTotal = 40
	}
	perStream = perStreamOverride
	if perStream <= 0 {
		perStream = ComputePerStreamReserveBase(totalConnections, percent, minReserve)
	}
	reserved = perStream * activeStreams
	if reserved < minReserve {
		reserved = minReserve
	}
	if reserved > maxTotal {
		reserved = maxTotal
	}
	if reserved >= totalConnections {
		return totalConnections, perStream
	}
	return reserved, perStream
}

// InteractivePerStreamReserve resolves the configured per-stream reserve baseline.
func (u Usenet) InteractivePerStreamReserve(totalConnections int) int {
	if u.InteractivePoolReservePerStream > 0 {
		return u.InteractivePoolReservePerStream
	}
	return ComputePerStreamReserveBase(totalConnections, u.InteractivePoolReservePercent, u.InteractivePoolReserveMin)
}

func (c *Config) updateUsenetConfig() {
	// Per-stream configuration defaults
	if c.Usenet.MaxConnections == 0 {
		c.Usenet.MaxConnections = 15 // Default: 15 connections per file
	}
	if c.Usenet.ProcessingMaxConnections <= 0 {
		c.Usenet.ProcessingMaxConnections = c.Usenet.MaxConnections
	}

	// Read-ahead default - bytes to prefetch ahead of reads
	if c.Usenet.ReadAhead == "" {
		c.Usenet.ReadAhead = "16MB" // Default: 16MB read-ahead buffer
	}

	// TCP socket buffer defaults sized for high-RTT BDP. "0" (explicit) opts
	// into OS autotuning, so only fill when unset.
	if c.Usenet.SocketReadBuffer == "" {
		c.Usenet.SocketReadBuffer = "4MB"
	}
	if c.Usenet.SocketWriteBuffer == "" {
		c.Usenet.SocketWriteBuffer = "1MB"
	}

	// Processing timeout default
	if c.Usenet.ProcessingTimeout == "" {
		c.Usenet.ProcessingTimeout = "10m" // Default: 10 minutes for NZB processing
	}

	// CacheDir: empty = system temp folder (no default needed)

	// Availability sample percent default - clamp to valid range
	if c.Usenet.AvailabilitySamplePercent <= 0 {
		c.Usenet.AvailabilitySamplePercent = 10
	} else if c.Usenet.AvailabilitySamplePercent > 100 {
		c.Usenet.AvailabilitySamplePercent = 100
	}
	if c.Usenet.ImportAvailabilitySamplePercent <= 0 {
		c.Usenet.ImportAvailabilitySamplePercent = 1
	} else if c.Usenet.ImportAvailabilitySamplePercent > 100 {
		c.Usenet.ImportAvailabilitySamplePercent = 100
	}

	if c.Usenet.InteractivePoolReservePercent <= 0 {
		c.Usenet.InteractivePoolReservePercent = 15
	} else if c.Usenet.InteractivePoolReservePercent > 100 {
		c.Usenet.InteractivePoolReservePercent = 100
	}
	if c.Usenet.InteractivePoolReserveMin <= 0 {
		c.Usenet.InteractivePoolReserveMin = 6
	}
	if c.Usenet.InteractivePoolReserveMax <= 0 {
		c.Usenet.InteractivePoolReserveMax = 40
	}
	if c.Usenet.InteractiveDetectBytes == "" {
		c.Usenet.InteractiveDetectBytes = "4MB"
	}
	if c.Usenet.InteractiveDetectWindow == "" {
		c.Usenet.InteractiveDetectWindow = "5s"
	}
	if c.Usenet.InteractiveIdleTimeout == "" {
		c.Usenet.InteractiveIdleTimeout = "30s"
	}

	if c.Usenet.DiskBufferPath == "" {
		c.Usenet.DiskBufferPath = filepath.Join(GetMainPath(), "usenet", "streams")
	}

	for i, provider := range c.Usenet.Providers {
		c.Usenet.Providers[i] = c.updateUsenetProvider(i, provider)
	}
}

func (c *Config) updateUsenetProvider(index int, u UsenetProvider) UsenetProvider {
	if u.Port == 0 {
		u.Port = 119 // Default port for usenet
	}
	if u.MaxConnections == 0 {
		u.MaxConnections = 20 // Default max connections per provider
	}
	if u.Priority == 0 {
		u.Priority = index + 1 // Default priority based on order
	}
	// Auto-enable TLS for ports that only speak implicit TLS.
	// Users who set port 563 (NNTPS) or 443 without ssl:true get a
	// plain-TCP connection that never completes the handshake.
	if !u.SSL && (u.Port == 563 || u.Port == 443) {
		u.SSL = true
	}
	return u
}

func validateUsenet(providers []UsenetProvider) error {
	if len(providers) == 0 {
		return nil
	}
	for _, usenet := range providers {
		// Basic field validation
		if usenet.Host == "" {
			return errors.New("usenet provider host is required")
		}
		if usenet.Username == "" {
			return errors.New("usenet provider username is required")
		}
		if usenet.Password == "" {
			return errors.New("usenet provider password is required")
		}
	}

	return nil
}

func (c *Config) applyUsenetEnvVars() {
	// Per-stream configuration
	processingMaxConns := getEnv("USENET__PROCESSING_MAX_CONNECTIONS")
	if maxConns := getEnv("USENET__MAX_CONNECTIONS"); maxConns != "" {
		if v, err := strconv.Atoi(maxConns); err == nil {
			c.Usenet.MaxConnections = v
			if processingMaxConns == "" {
				c.Usenet.ProcessingMaxConnections = v
			}
		}
	}
	if processingMaxConns != "" {
		if v, err := strconv.Atoi(processingMaxConns); err == nil {
			c.Usenet.ProcessingMaxConnections = v
		}
	}

	if readAhead := getEnv("USENET__READ_AHEAD"); readAhead != "" {
		c.Usenet.ReadAhead = readAhead
	}
	if preCacheOnOpen := getEnv("USENET__PRE_CACHE_ON_OPEN"); preCacheOnOpen != "" {
		c.Usenet.PreCacheOnOpen = parseBool(preCacheOnOpen)
	}
	if v := getEnv("USENET__INTERACTIVE_POOL_RESERVE_ENABLED"); v != "" {
		c.Usenet.InteractivePoolReserveEnabled = parseBool(v)
	}
	if v := getEnv("USENET__INTERACTIVE_POOL_RESERVE_PERCENT"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			c.Usenet.InteractivePoolReservePercent = n
		}
	}
	if v := getEnv("USENET__INTERACTIVE_POOL_RESERVE_PER_STREAM"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			c.Usenet.InteractivePoolReservePerStream = n
		}
	}
	if v := getEnv("USENET__INTERACTIVE_POOL_RESERVE_MIN"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			c.Usenet.InteractivePoolReserveMin = n
		}
	}
	if v := getEnv("USENET__INTERACTIVE_POOL_RESERVE_MAX"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			c.Usenet.InteractivePoolReserveMax = n
		}
	}
	if v := getEnv("USENET__INTERACTIVE_DETECT_BYTES"); v != "" {
		c.Usenet.InteractiveDetectBytes = v
	}
	if v := getEnv("USENET__INTERACTIVE_DETECT_WINDOW"); v != "" {
		c.Usenet.InteractiveDetectWindow = v
	}
	if v := getEnv("USENET__INTERACTIVE_IDLE_TIMEOUT"); v != "" {
		c.Usenet.InteractiveIdleTimeout = v
	}

	if v := getEnv("USENET__SOCKET_READ_BUFFER"); v != "" {
		c.Usenet.SocketReadBuffer = v
	}

	if v := getEnv("USENET__SOCKET_WRITE_BUFFER"); v != "" {
		c.Usenet.SocketWriteBuffer = v
	}

	if processingTimeout := getEnv("USENET__PROCESSING_TIMEOUT"); processingTimeout != "" {
		c.Usenet.ProcessingTimeout = processingTimeout
	}

	if availabilitySample := getEnv("USENET__AVAILABILITY_SAMPLE_PERCENT"); availabilitySample != "" {
		if v, err := strconv.Atoi(availabilitySample); err == nil {
			c.Usenet.AvailabilitySamplePercent = v
		}
	}
	if availabilitySample := getEnv("USENET__IMPORT_AVAILABILITY_SAMPLE_PERCENT"); availabilitySample != "" {
		if v, err := strconv.Atoi(availabilitySample); err == nil {
			c.Usenet.ImportAvailabilitySamplePercent = v
		}
	}

	if skipRepair := getEnv("USENET__SKIP_REPAIR"); skipRepair != "" {
		c.Usenet.SkipRepair = parseBool(skipRepair)
	}

	if deobfuscateMode := getEnv("USENET__DEOBFUSCATE_MODE"); deobfuscateMode != "" {
		c.Usenet.DeobfuscateMode = DeobfuscateMode(deobfuscateMode)
	}

	// Usenet providers array
	for i := 0; i < 10; i++ { // Support up to 10 usenet providers
		prefix := fmt.Sprintf("USENET__PROVIDERS__%d__", i)
		if val := getEnv(prefix + "HOST"); val != "" {
			// Ensure array is large enough
			if i >= len(c.Usenet.Providers) {
				c.Usenet.Providers = append(c.Usenet.Providers, make([]UsenetProvider, i-len(c.Usenet.Providers)+1)...)
			}
			c.Usenet.Providers[i].Host = val

			if port := getEnv(prefix + "PORT"); port != "" {
				if v, err := strconv.Atoi(port); err == nil {
					c.Usenet.Providers[i].Port = v
				}
			}
			if username := getEnv(prefix + "USERNAME"); username != "" {
				c.Usenet.Providers[i].Username = username
			}
			if password := getEnv(prefix + "PASSWORD"); password != "" {
				c.Usenet.Providers[i].Password = password
			}
			if backbone := getEnv(prefix + "BACKBONE"); backbone != "" {
				c.Usenet.Providers[i].Backbone = backbone
			}
			if maxConnections := getEnv(prefix + "MAX_CONNECTIONS"); maxConnections != "" {
				if v, err := strconv.Atoi(maxConnections); err == nil {
					c.Usenet.Providers[i].MaxConnections = v
				}
			}
			if ssl := getEnv(prefix + "SSL"); ssl != "" {
				c.Usenet.Providers[i].SSL = parseBool(ssl)
			}

			if priority := getEnv(prefix + "PRIORITY"); priority != "" {
				if v, err := strconv.Atoi(priority); err == nil {
					c.Usenet.Providers[i].Priority = v
				}
			}

			if backup := getEnv(prefix + "BACKUP"); backup != "" {
				c.Usenet.Providers[i].Backup = parseBool(backup)
			}
		}
	}
}
