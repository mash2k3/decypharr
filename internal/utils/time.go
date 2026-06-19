package utils

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"sync/atomic"
	"time"
)

// CachedTime provides a cached time value that updates every second.
// This avoids expensive syscalls when time.Now() is called frequently.
type CachedTime struct {
	unix    atomic.Int64
	unixNs  atomic.Int64
	running atomic.Bool
	stop    chan struct{}
}

var globalCachedTime = NewCachedTime()

// NewCachedTime creates a new CachedTime instance.
func NewCachedTime() *CachedTime {
	ct := &CachedTime{
		stop: make(chan struct{}),
	}
	ct.update()
	return ct
}

// Start begins the background time update goroutine.
func (ct *CachedTime) Start() {
	if ct.running.Swap(true) {
		return // already running
	}
	go ct.run()
}

// Stop stops the background time update goroutine.
func (ct *CachedTime) Stop() {
	if !ct.running.Load() {
		return
	}
	close(ct.stop)
}

func (ct *CachedTime) run() {
	ticker := time.NewTicker(500 * time.Millisecond)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			ct.update()
		case <-ct.stop:
			ct.running.Store(false)
			return
		}
	}
}

func (ct *CachedTime) update() {
	now := time.Now()
	ct.unix.Store(now.Unix())
	ct.unixNs.Store(now.UnixNano())
}

// Unix returns the cached Unix timestamp (seconds since epoch).
func (ct *CachedTime) Unix() int64 {
	return ct.unix.Load()
}

// UnixNano returns the cached Unix timestamp in nanoseconds.
func (ct *CachedTime) UnixNano() int64 {
	return ct.unixNs.Load()
}

// Now returns the cached time as a time.Time value.
// Note: This is slightly less accurate but avoids syscalls.
func (ct *CachedTime) Now() time.Time {
	return time.Unix(0, ct.unixNs.Load())
}

// StartGlobalCachedTime starts the global cached time updater.
func StartGlobalCachedTime() {
	globalCachedTime.Start()
}

// StopGlobalCachedTime stops the global cached time updater.
func StopGlobalCachedTime() {
	globalCachedTime.Stop()
}

// NowUnix returns the cached Unix timestamp from the global instance.
func NowUnix() int64 {
	return globalCachedTime.Unix()
}

// Now returns the cached time.Time from the global instance.
func Now() time.Time {
	return globalCachedTime.Now()
}

// extendedDurationRegex matches duration strings like "2d", "10d", "1w", "2w3d", "1w2d3h"
var extendedDurationRegex = regexp.MustCompile(`^(\d+w)?(\d+d)?(.*)$`)

// ParseDuration extends Go's time.ParseDuration to support:
//   - weeks (w): 1w = 7 days
//   - days (d): 1d = 24 hours
//
// Examples: "2d", "10d", "1w", "2w3d", "1w2d3h30m", "48h"
// Falls back to standard time.ParseDuration for unsupported formats.
func ParseDuration(s string) (time.Duration, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, fmt.Errorf("empty duration string")
	}

	matches := extendedDurationRegex.FindStringSubmatch(s)
	if matches == nil {
		// No match, try standard parsing
		return time.ParseDuration(s)
	}

	var total time.Duration

	// Parse weeks
	if matches[1] != "" {
		weeksStr := strings.TrimSuffix(matches[1], "w")
		weeks, err := strconv.ParseInt(weeksStr, 10, 64)
		if err != nil {
			return 0, fmt.Errorf("invalid weeks value: %s", matches[1])
		}
		total += time.Duration(weeks) * 7 * 24 * time.Hour
	}

	// Parse days
	if matches[2] != "" {
		daysStr := strings.TrimSuffix(matches[2], "d")
		days, err := strconv.ParseInt(daysStr, 10, 64)
		if err != nil {
			return 0, fmt.Errorf("invalid days value: %s", matches[2])
		}
		total += time.Duration(days) * 24 * time.Hour
	}

	// Parse remaining (hours, minutes, seconds, etc.) using standard parser
	remainder := matches[3]
	if remainder != "" {
		dur, err := time.ParseDuration(remainder)
		if err != nil {
			return 0, fmt.Errorf("invalid duration: %s", remainder)
		}
		total += dur
	}

	// If no w/d and no remainder matched, this is not a valid extended format
	// Try standard parsing as fallback
	if matches[1] == "" && matches[2] == "" && matches[3] == "" {
		return time.ParseDuration(s)
	}

	return total, nil
}
