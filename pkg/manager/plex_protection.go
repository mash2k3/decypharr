package manager

import (
	"sync"
	"time"

	"github.com/sirrobot01/decypharr/internal/config"
)

// PlexProtectedStream is a mount file confirmed by Plex /status/sessions.
type PlexProtectedStream struct {
	ID        string `json:"id"`
	EntryName string `json:"entry_name"`
	FileName  string `json:"file_name"`
	RatingKey string `json:"rating_key"`
	Type      string `json:"type"`
	UpdatedAt int64  `json:"updated_at"`
}

type plexProtectionState struct {
	mu sync.RWMutex

	streams   map[string]PlexProtectedStream
	pollOK    bool
	pollAt    time.Time
	pollError string
}

func newPlexProtectionState() *plexProtectionState {
	return &plexProtectionState{streams: make(map[string]PlexProtectedStream)}
}

// SetPlexProtectedStreams replaces the protected stream set from a successful Plex poll.
func (m *Manager) SetPlexProtectedStreams(streams []PlexProtectedStream) {
	if m == nil {
		return
	}
	if m.plexProtection == nil {
		m.plexProtection = newPlexProtectionState()
	}
	m.plexProtection.mu.Lock()
	m.plexProtection.streams = make(map[string]PlexProtectedStream, len(streams))
	for _, s := range streams {
		if s.ID == "" {
			continue
		}
		m.plexProtection.streams[s.ID] = s
	}
	m.plexProtection.pollOK = true
	m.plexProtection.pollAt = time.Now()
	m.plexProtection.pollError = ""
	m.plexProtection.mu.Unlock()

	if m.interactiveProtectionEnabled() {
		if len(streams) == 0 && m.interactive != nil && m.interactive.Active() {
			m.forceDeactivateInteractiveReserve()
		}
		m.NotifyInteractiveStreamCount()
	}
}

// ClearPlexProtectedStreams clears protection after a failed Plex poll.
func (m *Manager) ClearPlexProtectedStreams(err error) {
	if m == nil {
		return
	}
	if m.plexProtection == nil {
		m.plexProtection = newPlexProtectionState()
	}
	m.plexProtection.mu.Lock()
	m.plexProtection.streams = make(map[string]PlexProtectedStream)
	m.plexProtection.pollOK = false
	m.plexProtection.pollAt = time.Now()
	if err != nil {
		m.plexProtection.pollError = err.Error()
	} else {
		m.plexProtection.pollError = ""
	}
	m.plexProtection.mu.Unlock()

	if m.interactiveProtectionEnabled() {
		if m.interactive != nil && m.interactive.Active() {
			m.forceDeactivateInteractiveReserve()
		}
		m.NotifyInteractiveStreamCount()
	}
}

// IsStreamProtected reports whether streamID is in the last successful Plex poll set.
func (m *Manager) IsStreamProtected(streamID string) bool {
	if m == nil || streamID == "" || m.plexProtection == nil {
		return false
	}
	m.plexProtection.mu.RLock()
	defer m.plexProtection.mu.RUnlock()
	if !m.plexProtection.pollOK {
		return false
	}
	_, ok := m.plexProtection.streams[streamID]
	return ok
}

// PlexProtectedStreamCount returns the number of Plex-confirmed streams.
func (m *Manager) PlexProtectedStreamCount() int {
	if m == nil || m.plexProtection == nil {
		return 0
	}
	m.plexProtection.mu.RLock()
	defer m.plexProtection.mu.RUnlock()
	if !m.plexProtection.pollOK {
		return 0
	}
	return len(m.plexProtection.streams)
}

// PlexProtectionAvailable is true after a successful Plex poll.
func (m *Manager) PlexProtectionAvailable() bool {
	if m == nil || m.plexProtection == nil {
		return false
	}
	m.plexProtection.mu.RLock()
	defer m.plexProtection.mu.RUnlock()
	return m.plexProtection.pollOK
}

// GetPlexProtectedStreams returns a snapshot of protected streams.
func (m *Manager) GetPlexProtectedStreams() []PlexProtectedStream {
	if m == nil || m.plexProtection == nil {
		return nil
	}
	m.plexProtection.mu.RLock()
	defer m.plexProtection.mu.RUnlock()
	out := make([]PlexProtectedStream, 0, len(m.plexProtection.streams))
	for _, s := range m.plexProtection.streams {
		out = append(out, s)
	}
	return out
}

// PlexProtectionStats returns poll status for stats/UI.
func (m *Manager) PlexProtectionStats() map[string]interface{} {
	if m == nil || m.plexProtection == nil {
		return map[string]interface{}{
			"poll_ok": false,
		}
	}
	m.plexProtection.mu.RLock()
	defer m.plexProtection.mu.RUnlock()
	streams := make([]map[string]interface{}, 0, len(m.plexProtection.streams))
	for _, s := range m.plexProtection.streams {
		streams = append(streams, map[string]interface{}{
			"id":         s.ID,
			"entry_name": s.EntryName,
			"file_name":  s.FileName,
			"type":       s.Type,
			"rating_key": s.RatingKey,
		})
	}
	stats := map[string]interface{}{
		"poll_ok":    m.plexProtection.pollOK,
		"streams":    streams,
		"updated_at": m.plexProtection.pollAt.UTC().Format(time.RFC3339),
	}
	if m.plexProtection.pollError != "" {
		stats["poll_error"] = m.plexProtection.pollError
	}
	return stats
}

func (m *Manager) interactiveProtectionEnabled() bool {
	if m != nil && m.config != nil {
		return m.config.Usenet.InteractivePoolReserveEnabled
	}
	cfg := config.Get()
	return cfg != nil && cfg.Usenet.InteractivePoolReserveEnabled
}

func (m *Manager) plexCredentialsConfigured() bool {
	if m != nil && m.config != nil {
		return m.config.Mount.DFS.PlexURL != "" && m.config.Mount.DFS.PlexToken != ""
	}
	cfg := config.Get()
	return cfg != nil && cfg.Mount.DFS.PlexURL != "" && cfg.Mount.DFS.PlexToken != ""
}

func (m *Manager) forceDeactivateInteractiveReserve() {
	if m == nil || m.interactive == nil {
		return
	}
	m.interactive.forceDeactivate()
}

// InteractivePoolReserveEnabled reports whether playback protection is enabled.
func (m *Manager) InteractivePoolReserveEnabled() bool {
	return m.interactiveProtectionEnabled()
}
