package nntp

import (
	"sync"
	"sync/atomic"
	"time"

	"github.com/sirrobot01/decypharr/internal/config"
)

// interactiveReserveConfig holds pool-reserve tuning applied when interactive
// mode is active.
type interactiveReserveConfig struct {
	enabled         bool
	percent         int
	perStream       int
	min             int
	max             int
}

// interactiveState tracks dynamic pool reservation for playback protection.
type interactiveState struct {
	mu sync.RWMutex

	cfg interactiveReserveConfig

	active bool

	activeStreamCount int
	perStreamReserve  int
	reserved          int
	backgroundBudget  int

	backgroundInUse int32
	streamInUse     int32

	startedAt time.Time

	lastEntry  string
	lastFile   string
	lastClient string

	throttleLoggedAt time.Time
}

func newInteractiveState(cfg *config.Config) *interactiveState {
	s := &interactiveState{}
	if cfg != nil {
		s.applyConfig(cfg)
	}
	return s
}

func (s *interactiveState) applyConfig(cfg *config.Config) {
	if s == nil || cfg == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.cfg = interactiveReserveConfig{
		enabled:   cfg.Usenet.InteractivePoolReserveEnabled,
		percent:   cfg.Usenet.InteractivePoolReservePercent,
		perStream: cfg.Usenet.InteractivePoolReservePerStream,
		min:       cfg.Usenet.InteractivePoolReserveMin,
		max:       cfg.Usenet.InteractivePoolReserveMax,
	}
	if !s.cfg.enabled {
		s.active = false
		s.reserved = 0
		s.backgroundBudget = 0
		s.activeStreamCount = 0
		s.perStreamReserve = 0
		atomic.StoreInt32(&s.backgroundInUse, 0)
		atomic.StoreInt32(&s.streamInUse, 0)
		return
	}
	if s.active {
		s.recalculateLocked(0, s.activeStreamCount)
	}
}

func (s *interactiveState) reserveEnabled() bool {
	if s == nil {
		return false
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.cfg.enabled
}

func (s *interactiveState) recalculateLocked(totalConnections, activeStreams int) (changed bool) {
	if totalConnections <= 0 {
		totalConnections = 1
	}
	if activeStreams <= 0 {
		activeStreams = 1
	}
	reserved, perStream := config.ComputeDynamicInteractiveReserve(
		totalConnections,
		activeStreams,
		s.cfg.percent,
		s.cfg.min,
		s.cfg.max,
		s.cfg.perStream,
	)
	background := totalConnections - reserved
	if background < 0 {
		background = 0
	}
	changed = reserved != s.reserved ||
		background != s.backgroundBudget ||
		activeStreams != s.activeStreamCount ||
		perStream != s.perStreamReserve
	s.activeStreamCount = activeStreams
	s.perStreamReserve = perStream
	s.reserved = reserved
	s.backgroundBudget = background
	return changed
}

func (s *interactiveState) setActive(active bool, totalConnections int, activeStreams int, meta reserveMeta) (entered, exited, changed bool, snapshot InteractiveSnapshot) {
	if s == nil {
		return false, false, false, InteractiveSnapshot{}
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	if !s.cfg.enabled {
		if s.active {
			s.active = false
			s.reserved = 0
			s.backgroundBudget = 0
			s.activeStreamCount = 0
			s.perStreamReserve = 0
			atomic.StoreInt32(&s.backgroundInUse, 0)
			atomic.StoreInt32(&s.streamInUse, 0)
			exited = true
		}
		return false, exited, false, s.snapshotLocked()
	}

	wasActive := s.active
	if active {
		if activeStreams <= 0 {
			activeStreams = 1
		}
		changed = s.recalculateLocked(totalConnections, activeStreams)
		if meta.Entry != "" {
			s.lastEntry = meta.Entry
		}
		if meta.File != "" {
			s.lastFile = meta.File
		}
		if meta.Client != "" {
			s.lastClient = meta.Client
		}
		if !wasActive {
			s.startedAt = time.Now()
			entered = true
		}
		s.active = true
	} else if wasActive {
		s.active = false
		s.reserved = 0
		s.backgroundBudget = 0
		s.activeStreamCount = 0
		s.perStreamReserve = 0
		atomic.StoreInt32(&s.backgroundInUse, 0)
		atomic.StoreInt32(&s.streamInUse, 0)
		exited = true
	}

	return entered, exited, changed, s.snapshotLocked()
}

func (s *interactiveState) setStreamCount(totalConnections, activeStreams int) (changed bool, snapshot InteractiveSnapshot) {
	if s == nil {
		return false, InteractiveSnapshot{}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.cfg.enabled || !s.active {
		return false, s.snapshotLocked()
	}
	if activeStreams <= 0 {
		activeStreams = 1
	}
	changed = s.recalculateLocked(totalConnections, activeStreams)
	return changed, s.snapshotLocked()
}

func (s *interactiveState) snapshotLocked() InteractiveSnapshot {
	return InteractiveSnapshot{
		Enabled:           s.cfg.enabled,
		Active:            s.active,
		ActiveStreams:     s.activeStreamCount,
		PerStreamReserve:  s.perStreamReserve,
		Reserved:          s.reserved,
		BackgroundBudget:  s.backgroundBudget,
		BackgroundInUse:   int(atomic.LoadInt32(&s.backgroundInUse)),
		StreamInUse:       int(atomic.LoadInt32(&s.streamInUse)),
		StartedAt:         s.startedAt,
		LastEntry:         s.lastEntry,
		LastFile:          s.lastFile,
		LastClient:        s.lastClient,
		ReservePercent:    s.cfg.percent,
		ReserveMin:        s.cfg.min,
		ReserveMax:        s.cfg.max,
		ReservePerStream:  s.cfg.perStream,
	}
}

func (s *interactiveState) snapshot() InteractiveSnapshot {
	if s == nil {
		return InteractiveSnapshot{}
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.snapshotLocked()
}

func (s *interactiveState) acquireBackgroundSlot() bool {
	if s == nil {
		return true
	}
	s.mu.RLock()
	enabled := s.cfg.enabled && s.active
	budget := s.backgroundBudget
	s.mu.RUnlock()
	if !enabled {
		return true
	}
	for {
		cur := atomic.LoadInt32(&s.backgroundInUse)
		if int(cur) >= budget {
			return false
		}
		if atomic.CompareAndSwapInt32(&s.backgroundInUse, cur, cur+1) {
			return true
		}
	}
}

func (s *interactiveState) releaseBackgroundSlot() {
	if s == nil {
		return
	}
	for {
		cur := atomic.LoadInt32(&s.backgroundInUse)
		if cur <= 0 {
			return
		}
		if atomic.CompareAndSwapInt32(&s.backgroundInUse, cur, cur-1) {
			return
		}
	}
}

func (s *interactiveState) acquireStreamSlot() {
	if s == nil {
		return
	}
	atomic.AddInt32(&s.streamInUse, 1)
}

func (s *interactiveState) releaseStreamSlot() {
	if s == nil {
		return
	}
	for {
		cur := atomic.LoadInt32(&s.streamInUse)
		if cur <= 0 {
			return
		}
		if atomic.CompareAndSwapInt32(&s.streamInUse, cur, cur-1) {
			return
		}
	}
}

func (s *interactiveState) providerStreamHeadroom(providerMax int) int {
	if s == nil || providerMax <= 0 {
		return 0
	}
	s.mu.RLock()
	perStream := s.perStreamReserve
	s.mu.RUnlock()
	if perStream <= 0 {
		perStream = 1
	}
	if perStream >= providerMax {
		return providerMax - 1
	}
	return perStream
}

func (s *interactiveState) providerReserveSlots(providerMax, totalConnections int) int {
	_ = totalConnections
	return s.providerStreamHeadroom(providerMax)
}

func (s *interactiveState) backgroundMayUseProvider(providerInUse, providerMax, totalConnections int) bool {
	if s == nil {
		return true
	}
	s.mu.RLock()
	active := s.active && s.cfg.enabled
	s.mu.RUnlock()
	if !active {
		return true
	}
	headroom := s.providerStreamHeadroom(providerMax)
	if headroom <= 0 {
		return true
	}
	return providerInUse < providerMax-headroom
}

type ReserveMeta struct {
	Entry  string
	File   string
	Client string
}

type reserveMeta struct {
	Entry  string
	File   string
	Client string
}

// InteractiveSnapshot exposes reserve state for stats/logging.
type InteractiveSnapshot struct {
	Enabled          bool      `json:"enabled"`
	Active           bool      `json:"active"`
	ActiveStreams    int       `json:"active_streams,omitempty"`
	PerStreamReserve int       `json:"per_stream_reserve,omitempty"`
	Reserved         int       `json:"reserved"`
	BackgroundBudget int       `json:"background_budget"`
	BackgroundInUse  int       `json:"background_in_use"`
	StreamInUse      int       `json:"stream_in_use,omitempty"`
	StartedAt        time.Time `json:"started_at,omitempty"`
	LastEntry        string    `json:"last_entry,omitempty"`
	LastFile         string    `json:"last_file,omitempty"`
	LastClient       string    `json:"last_client,omitempty"`
	ReservePercent   int       `json:"reserve_percent,omitempty"`
	ReserveMin       int       `json:"reserve_min,omitempty"`
	ReserveMax       int       `json:"reserve_max,omitempty"`
	ReservePerStream int       `json:"reserve_per_stream_config,omitempty"`
}

func (s *interactiveState) markThrottleLogged(now time.Time) bool {
	if s == nil {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if !now.After(s.throttleLoggedAt.Add(time.Minute)) {
		return false
	}
	s.throttleLoggedAt = now
	return true
}
