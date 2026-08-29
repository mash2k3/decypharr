package manager

import (
	"errors"
	"testing"
	"time"

	"github.com/puzpuzpuz/xsync/v4"
	"github.com/sirrobot01/decypharr/internal/config"
)

func TestPlexProtectedStreamsSetAndCount(t *testing.T) {
	m := &Manager{plexProtection: newPlexProtectionState()}
	m.SetPlexProtectedStreams([]PlexProtectedStream{
		{ID: "Show:ep1.mkv", EntryName: "Show", FileName: "ep1.mkv", Type: "episode"},
		{ID: "Movie:movie.mkv", EntryName: "Movie", FileName: "movie.mkv", Type: "movie"},
	})
	if got := m.PlexProtectedStreamCount(); got != 2 {
		t.Fatalf("count = %d, want 2", got)
	}
	if !m.IsStreamProtected("Show:ep1.mkv") {
		t.Fatal("expected Show:ep1.mkv to be protected")
	}
	if m.IsStreamProtected("Bazarr:other.mkv") {
		t.Fatal("bazarr stream must not be protected")
	}
}

func TestCountQualifyingStreamsUsesPlexSessions(t *testing.T) {
	m := &Manager{
		plexProtection: newPlexProtectionState(),
		config: &config.Config{
			Usenet: config.Usenet{InteractivePoolReserveEnabled: true},
			Mount: config.Mount{
				DFS: config.DFS{PlexURL: "http://plex", PlexToken: "token"},
			},
		},
		activeStreams: xsync.NewMap[string, *ActiveStream](),
	}
	m.SetPlexProtectedStreams([]PlexProtectedStream{
		{ID: "Show:ep1.mkv"},
		{ID: "Show:ep2.mkv"},
	})
	m.registerStream("Bazarr", "scan.mkv", 1<<30, "nzb", "", "DFS")

	if got := m.countQualifyingStreams(); got != 2 {
		t.Fatalf("countQualifyingStreams() = %d, want 2 Plex streams", got)
	}
}

func TestRecordStreamActivityIgnoresUnprotectedWhenPlexGated(t *testing.T) {
	var active bool
	m := &Manager{
		plexProtection:    newPlexProtectionState(),
		processingEntries: xsync.NewMap[string, struct{}](),
		config: &config.Config{
			Usenet: config.Usenet{
				InteractivePoolReserveEnabled: true,
				InteractiveDetectBytes:        "1MB",
			},
			Mount: config.Mount{
				DFS: config.DFS{PlexURL: "http://plex", PlexToken: "token"},
			},
		},
		activeStreams: xsync.NewMap[string, *ActiveStream](),
	}
	m.interactive = newInteractiveMonitor(m.config, func(a bool, _ reserveStreamMeta, _ int, _ int64, _ time.Duration) {
		active = a
	}, m.hasBackgroundContention)
	m.SetPlexProtectedStreams([]PlexProtectedStream{{ID: "Show:ep1.mkv", EntryName: "Show", FileName: "ep1.mkv"}})
	m.registerStream("Bazarr", "scan.mkv", 1<<30, "nzb", "", "DFS")
	m.registerStream("Show", "ep1.mkv", 1<<30, "nzb", "", "DFS")

	m.RecordStreamActivity("Bazarr:scan.mkv", 5<<20, 5<<20, false)
	if active {
		t.Fatal("bazarr reads must not activate reserve when Plex-gated")
	}

	m.RecordStreamActivity("Show:ep1.mkv", 2<<20, 2<<20, false)
	if active {
		t.Fatal("protected Plex stream reads alone must not activate reserve without background contention")
	}

	m.processingEntries.Store("bulk-import", struct{}{})
	m.RecordStreamActivity("Show:ep1.mkv", 2<<20, 2<<20, false)
	if !active {
		t.Fatal("protected Plex stream reads should activate reserve when background work is active")
	}
}

func TestClearPlexProtectedStreamsDeactivatesReserve(t *testing.T) {
	var deactivated bool
	m := &Manager{
		plexProtection: newPlexProtectionState(),
		config: &config.Config{
			Usenet: config.Usenet{
				InteractivePoolReserveEnabled: true,
				InteractiveDetectBytes:        "1MB",
			},
			Mount: config.Mount{
				DFS: config.DFS{PlexURL: "http://plex", PlexToken: "token"},
			},
		},
	}
	m.interactive = newInteractiveMonitor(m.config, func(a bool, _ reserveStreamMeta, _ int, _ int64, _ time.Duration) {
		if !a {
			deactivated = true
		}
	}, nil)
	m.interactive.RecordRead(2<<20, 2<<20, false, reserveStreamMeta{Entry: "Show", File: "ep1.mkv"}, true)
	m.SetPlexProtectedStreams([]PlexProtectedStream{{ID: "Show:ep1.mkv"}})

	m.ClearPlexProtectedStreams(errors.New("plex down"))
	if !deactivated {
		t.Fatal("expected reserve deactivated on Plex poll failure")
	}
	if m.PlexProtectedStreamCount() != 0 {
		t.Fatal("expected protected set cleared")
	}
}

func TestInteractiveMonitorRequiresBackgroundContention(t *testing.T) {
	var active bool
	background := false
	m := newInteractiveMonitor(&config.Config{
		Usenet: config.Usenet{
			InteractivePoolReserveEnabled: true,
			InteractiveDetectBytes:        "1MB",
		},
	}, func(a bool, _ reserveStreamMeta, _ int, _ int64, _ time.Duration) {
		active = a
	}, func() bool { return background })

	meta := reserveStreamMeta{Entry: "Show", File: "ep.mkv", Client: "DFS"}
	m.RecordRead(2<<20, 2<<20, false, meta, true)
	if active {
		t.Fatal("should not activate without background contention")
	}

	background = true
	m.Tick(time.Now())
	if !active {
		t.Fatal("expected activation once background contention is present")
	}

	background = false
	m.Tick(time.Now())
	if active {
		t.Fatal("expected deactivation when background contention clears")
	}
}
