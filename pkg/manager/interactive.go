package manager

import (
	"sync"
	"time"

	"github.com/sirrobot01/decypharr/internal/config"
	"github.com/sirrobot01/decypharr/internal/utils"
)

type interactiveSample struct {
	at    time.Time
	bytes int64
}

// InteractiveMonitor tracks sustained playback reads and toggles NNTP reserve mode.
type InteractiveMonitor struct {
	mu sync.Mutex

	enabled bool

	detectBytes  int64
	detectWindow time.Duration
	idleTimeout  time.Duration

	samples []interactiveSample

	lastQualifyingAt time.Time
	lastMeta         reserveStreamMeta

	active bool

	onChange       func(active bool, meta reserveStreamMeta, activeStreams int, bytesInWindow int64, detectWindow time.Duration)
	shouldActivate func() bool
}

type reserveStreamMeta struct {
	Entry  string
	File   string
	Client string
}

func newInteractiveMonitor(cfg *config.Config, onChange func(bool, reserveStreamMeta, int, int64, time.Duration), shouldActivate func() bool) *InteractiveMonitor {
	m := &InteractiveMonitor{onChange: onChange, shouldActivate: shouldActivate}
	if m.shouldActivate == nil {
		m.shouldActivate = func() bool { return true }
	}
	m.applyConfig(cfg)
	return m
}

func (m *InteractiveMonitor) applyConfig(cfg *config.Config) {
	if m == nil || cfg == nil {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.enabled = cfg.Usenet.InteractivePoolReserveEnabled
	m.detectBytes = cfg.Usenet.InteractiveDetectBytesValue()
	if d, err := utils.ParseDuration(cfg.Usenet.InteractiveDetectWindow); err == nil && d > 0 {
		m.detectWindow = d
	} else {
		m.detectWindow = 5 * time.Second
	}
	if d, err := utils.ParseDuration(cfg.Usenet.InteractiveIdleTimeout); err == nil && d > 0 {
		m.idleTimeout = d
	} else {
		m.idleTimeout = 30 * time.Second
	}
	if !m.enabled && m.active {
		m.active = false
		m.samples = nil
		if m.onChange != nil {
			meta := m.lastMeta
			m.mu.Unlock()
			m.onChange(false, meta, 0, 0, m.detectWindow)
			m.mu.Lock()
		}
	}
}

// forceDeactivate ends reserve mode immediately (e.g. Plex poll failure).
func (m *InteractiveMonitor) forceDeactivate() {
	if m == nil {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if !m.active {
		return
	}
	m.active = false
	m.samples = nil
	if m.onChange != nil {
		meta := m.lastMeta
		window := m.detectWindow
		m.mu.Unlock()
		m.onChange(false, meta, 0, 0, window)
		m.mu.Lock()
	}
}

// RecordRead records new bytes for activation and total bytes for idle keepalive.
func (m *InteractiveMonitor) RecordRead(newBytes, totalBytes int64, probe bool, meta reserveStreamMeta, protected bool) {
	if m == nil {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if !m.enabled || probe || !protected {
		return
	}
	now := time.Now()
	if totalBytes > 0 {
		m.lastQualifyingAt = now
		if meta.Entry != "" {
			m.lastMeta = meta
		}
	}
	if newBytes <= 0 {
		return
	}
	m.samples = append(m.samples, interactiveSample{at: now, bytes: newBytes})
	m.pruneLocked(now)
	m.evaluateLocked(now)
}

func (m *InteractiveMonitor) Tick(now time.Time) {
	if m == nil {
		return
	}
	m.evaluateNow(now)
}

func (m *InteractiveMonitor) evaluateNow(now time.Time) {
	if m == nil {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if !m.enabled {
		return
	}
	m.pruneLocked(now)
	if m.active {
		if m.shouldActivate != nil && !m.shouldActivate() {
			m.deactivateLocked()
			return
		}
		if !m.lastQualifyingAt.IsZero() && now.Sub(m.lastQualifyingAt) >= m.idleTimeout {
			m.deactivateLocked()
		}
		return
	}
	m.evaluateLocked(now)
}

func (m *InteractiveMonitor) deactivateLocked() {
	if !m.active {
		return
	}
	m.active = false
	if m.onChange != nil {
		meta := m.lastMeta
		window := m.detectWindow
		m.mu.Unlock()
		m.onChange(false, meta, 0, 0, window)
		m.mu.Lock()
	}
}

// touchQualifyingActivity refreshes idle keepalive without adding detect samples.
func (m *InteractiveMonitor) touchQualifyingActivity() {
	if m == nil {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if !m.enabled || !m.active {
		return
	}
	m.lastQualifyingAt = time.Now()
}

func (m *InteractiveMonitor) pruneLocked(now time.Time) {
	cutoff := now.Add(-m.detectWindow)
	i := 0
	for _, s := range m.samples {
		if !s.at.Before(cutoff) {
			m.samples[i] = s
			i++
		}
	}
	m.samples = m.samples[:i]
}

func (m *InteractiveMonitor) evaluateLocked(now time.Time) {
	if m.active {
		return
	}
	var total int64
	for _, s := range m.samples {
		total += s.bytes
	}
	if total < m.detectBytes {
		return
	}
	if m.shouldActivate != nil && !m.shouldActivate() {
		return
	}
	m.active = true
	if m.onChange != nil {
		meta := m.lastMeta
		bytes := total
		window := m.detectWindow
		m.mu.Unlock()
		m.onChange(true, meta, 1, bytes, window)
		m.mu.Lock()
	}
}

// Active reports whether interactive reserve mode is active.
func (m *InteractiveMonitor) Active() bool {
	if m == nil {
		return false
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.active
}

// RecordStreamActivity updates LastActive and records qualifying bytes for a tracked stream.
func (m *Manager) RecordStreamActivity(streamID string, totalBytes, newBytes int64, probe bool) {
	if m == nil || m.interactive == nil {
		return
	}
	var meta reserveStreamMeta
	protected := false
	if streamID != "" {
		if s, ok := m.activeStreams.Load(streamID); ok && s != nil {
			s.LastActive = utils.NowUnix()
			m.activeStreams.Store(streamID, s)
			meta = reserveStreamMeta{Entry: s.EntryName, File: s.FileName, Client: s.Client}
		}
	}
	if m.interactiveProtectionEnabled() {
		if !m.plexCredentialsConfigured() {
			return
		}
		protected = m.IsStreamProtected(streamID)
		if !protected {
			return
		}
	} else {
		protected = true
	}
	m.interactive.RecordRead(newBytes, totalBytes, probe, meta, protected)
	if m.interactive.Active() {
		m.pushInteractiveStreamCount()
	}
}

func (m *Manager) countQualifyingStreams() int {
	if m == nil {
		return 0
	}
	if m.interactiveProtectionEnabled() {
		if !m.plexCredentialsConfigured() || !m.PlexProtectionAvailable() {
			return 0
		}
		count := m.PlexProtectedStreamCount()
		if count == 0 {
			return 0
		}
		return count
	}
	idle := 30 * time.Second
	if cfg := config.Get(); cfg != nil {
		if d, err := utils.ParseDuration(cfg.Usenet.InteractiveIdleTimeout); err == nil && d > 0 {
			idle = d
		}
	}
	cutoff := utils.NowUnix() - int64(idle.Seconds())
	count := 0
	m.activeStreams.Range(func(_ string, stream *ActiveStream) bool {
		if stream != nil && stream.LastActive >= cutoff {
			count++
		}
		return true
	})
	if count == 0 {
		return 1
	}
	return count
}

func (m *Manager) pushInteractiveStreamCount() {
	if m == nil || m.usenet == nil || m.interactive == nil || !m.interactive.Active() {
		return
	}
	m.usenet.SetInteractiveStreamCount(m.countQualifyingStreams())
}

func (m *Manager) startInteractiveMonitor(cfg *config.Config) {
	if m.usenet == nil {
		return
	}
	m.usenet.ConfigureInteractiveReserve(cfg)
	m.usenet.SetStreamBytesRecorder(func(nzoID, filename string, n int64, probe bool) {
		m.RecordStreamActivity(nzoID+":"+filename, n, n, probe)
	})
	m.interactive = newInteractiveMonitor(cfg, func(active bool, meta reserveStreamMeta, activeStreams int, bytesInWindow int64, detectWindow time.Duration) {
		if m.usenet != nil {
			if active && activeStreams <= 0 {
				activeStreams = m.countQualifyingStreams()
			}
			m.usenet.SetInteractiveReserveActive(active, meta.Entry, meta.File, meta.Client, activeStreams, bytesInWindow, detectWindow)
		}
	}, m.hasBackgroundContention)
	go m.interactiveMonitorLoop()
}

func (m *Manager) interactiveMonitorLoop() {
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-m.ctx.Done():
			return
		case now := <-ticker.C:
			if m.interactive != nil {
				if m.interactiveProtectionEnabled() &&
					m.PlexProtectedStreamCount() > 0 &&
					m.interactive.Active() &&
					m.hasBackgroundContention() {
					m.interactive.touchQualifyingActivity()
				}
				m.interactive.Tick(now)
				if m.interactive.Active() {
					m.pushInteractiveStreamCount()
				}
			}
		}
	}
}

// ReconfigureInteractiveMonitor reloads settings after config changes.
func (m *Manager) ReconfigureInteractiveMonitor(cfg *config.Config) {
	if m.interactive != nil {
		m.interactive.applyConfig(cfg)
	}
	if m.usenet != nil {
		m.usenet.ConfigureInteractiveReserve(cfg)
		if m.interactive != nil && m.interactive.Active() {
			m.pushInteractiveStreamCount()
		}
	}
}

// NotifyInteractiveStreamCount pushes the current active stream count to NNTP reserve.
func (m *Manager) NotifyInteractiveStreamCount() {
	m.pushInteractiveStreamCount()
}

func (m *Manager) hasBackgroundContention() bool {
	if m == nil {
		return false
	}
	if m.jobQueue != nil {
		if m.jobQueue.ActiveCount() > 0 || m.jobQueue.Len() > 0 {
			return true
		}
	}
	return m.processingEntries != nil && m.processingEntries.Size() > 0
}

// NotifyBackgroundActivity re-evaluates reserve when import/parse work starts or stops.
func (m *Manager) NotifyBackgroundActivity() {
	if m == nil || m.interactive == nil {
		return
	}
	m.interactive.evaluateNow(time.Now())
	if m.interactive.Active() {
		m.pushInteractiveStreamCount()
	}
}
