package manager

import (
	"testing"
	"time"

	"github.com/sirrobot01/decypharr/internal/config"
)

func TestInteractiveMonitorDetectsSustainedReads(t *testing.T) {
	var active bool
	m := newInteractiveMonitor(&config.Config{
		Usenet: config.Usenet{
			InteractivePoolReserveEnabled: true,
			InteractiveDetectBytes:        "4MB",
			InteractiveDetectWindow:       "5s",
			InteractiveIdleTimeout:        "30s",
		},
	}, func(a bool, _ reserveStreamMeta, _ int, _ int64, _ time.Duration) {
		active = a
	}, nil)

	meta := reserveStreamMeta{Entry: "Show", File: "ep.mkv", Client: "DFS"}
	m.RecordRead(2<<20, 2<<20, false, meta, true)
	if active {
		t.Fatal("should not activate below threshold")
	}
	m.RecordRead(3<<20, 3<<20, false, meta, true)
	if !active {
		t.Fatal("expected interactive mode after sustained reads")
	}
}

func TestInteractiveMonitorIgnoresProbes(t *testing.T) {
	var active bool
	m := newInteractiveMonitor(&config.Config{
		Usenet: config.Usenet{
			InteractivePoolReserveEnabled: true,
			InteractiveDetectBytes:        "1MB",
		},
	}, func(a bool, _ reserveStreamMeta, _ int, _ int64, _ time.Duration) {
		active = a
	}, nil)
	m.RecordRead(5<<20, 5<<20, true, reserveStreamMeta{Entry: "Show", File: "ep.mkv"}, true)
	if active {
		t.Fatal("probe reads must not activate interactive mode")
	}
}

func TestInteractiveMonitorIdleTimeout(t *testing.T) {
	var active bool
	m := newInteractiveMonitor(&config.Config{
		Usenet: config.Usenet{
			InteractivePoolReserveEnabled: true,
			InteractiveDetectBytes:        "1MB",
			InteractiveIdleTimeout:        "1s",
		},
	}, func(a bool, _ reserveStreamMeta, _ int, _ int64, _ time.Duration) {
		active = a
	}, nil)
	m.RecordRead(2<<20, 2<<20, false, reserveStreamMeta{Entry: "Show", File: "ep.mkv"}, true)
	if !active {
		t.Fatal("expected active")
	}
	m.Tick(time.Now().Add(2 * time.Second))
	if active {
		t.Fatal("expected idle timeout to deactivate")
	}
}

func TestInteractiveMonitorCacheHitKeepsAlive(t *testing.T) {
	var active bool
	var deactivate bool
	m := newInteractiveMonitor(&config.Config{
		Usenet: config.Usenet{
			InteractivePoolReserveEnabled: true,
			InteractiveDetectBytes:        "4MB",
			InteractiveIdleTimeout:        "1s",
		},
	}, func(a bool, _ reserveStreamMeta, _ int, _ int64, _ time.Duration) {
		if a {
			active = true
		} else {
			deactivate = true
		}
	}, nil)
	m.RecordRead(4<<20, 4<<20, false, reserveStreamMeta{Entry: "Show", File: "ep.mkv"}, true)
	if !active {
		t.Fatal("expected active")
	}
	m.RecordRead(0, 1<<20, false, reserveStreamMeta{Entry: "Show", File: "ep.mkv"}, true)
	m.Tick(time.Now().Add(500 * time.Millisecond))
	if deactivate {
		t.Fatal("cache-served reads should keep reserve alive")
	}
}
