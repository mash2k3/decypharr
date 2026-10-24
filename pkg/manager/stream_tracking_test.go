package manager

import (
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/puzpuzpuz/xsync/v4"
	"github.com/rs/zerolog"
	"github.com/sirrobot01/decypharr/internal/config"
	"github.com/sirrobot01/decypharr/internal/utils"
)

func newStreamTrackingManager() *Manager {
	return &Manager{
		activeStreams: xsync.NewMap[string, *ActiveStream](),
		logger:        zerolog.Nop(),
	}
}

// A downloader keeps writing after its session is untracked (idle timeout or
// StopAll). Its activity updates must never put the stream back: before the
// fix a Load+Store pair straddling the untrack re-inserted the entry, and with
// the session's streamID already cleared nothing ever removed it again.
func TestRecordStreamActivityNeverResurrectsUntrackedStream(t *testing.T) {
	m := newStreamTrackingManager()
	m.config = &config.Config{}
	m.interactive = newInteractiveMonitor(m.config, func(bool, reserveStreamMeta, int, int64, time.Duration) {}, func() bool { return false })
	const id = "Show:ep1.mkv"

	for i := 0; i < 2000; i++ {
		m.registerStream("Show", "ep1.mkv", 1<<30, "torrent", "RealDebrid", "DFS")

		var stop atomic.Bool
		var wg sync.WaitGroup
		for w := 0; w < 4; w++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				for !stop.Load() {
					m.RecordStreamActivity(id, 32<<10, 32<<10, false)
				}
			}()
		}
		m.unregisterStream(id)
		stop.Store(true)
		wg.Wait()

		if _, ok := m.activeStreams.Load(id); ok {
			t.Fatalf("iteration %d: untracked stream was re-inserted by a concurrent activity update", i)
		}
	}
}

func TestRecordStreamActivityRefreshesLastActiveWithoutInteractiveMonitor(t *testing.T) {
	m := newStreamTrackingManager()
	id := m.registerStream("Show", "ep1.mkv", 1<<30, "torrent", "RealDebrid", "DFS")
	s, _ := m.activeStreams.Load(id)
	s.LastActive = utils.NowUnix() - 60

	m.RecordStreamActivity(id, 1, 1, false)

	got, _ := m.activeStreams.Load(id)
	if got.LastActive < utils.NowUnix()-1 {
		t.Fatalf("LastActive = %d, want refreshed to now", got.LastActive)
	}
}

func TestGetActiveStreamsDropsSilentDFSStreams(t *testing.T) {
	m := newStreamTrackingManager()
	stale := m.registerStream("Old", "old.mkv", 1<<30, "torrent", "RealDebrid", "DFS")
	live := m.registerStream("New", "new.mkv", 1<<30, "nzb", "", "DFS")
	webdav := m.registerStream("Dav", "dav.mkv", 1<<30, "torrent", "RealDebrid", "Infuse")

	old := utils.NowUnix() - int64(staleStreamTTL.Seconds()) - 10
	for _, id := range []string{stale, webdav} {
		s, _ := m.activeStreams.Load(id)
		s.LastActive = old
	}

	got := map[string]bool{}
	for _, s := range m.GetActiveStreams() {
		got[s.ID] = true
	}
	if got[stale] {
		t.Fatalf("silent DFS stream %q still listed", stale)
	}
	if _, ok := m.activeStreams.Load(stale); ok {
		t.Fatalf("silent DFS stream %q not removed from the registry", stale)
	}
	if !got[live] {
		t.Fatalf("active DFS stream %q missing", live)
	}
	// WebDAV torrent reads never refresh LastActive, so only DFS entries can be
	// judged stale.
	if !got[webdav] {
		t.Fatalf("WebDAV stream %q dropped; only DFS streams may be pruned", webdav)
	}
}
