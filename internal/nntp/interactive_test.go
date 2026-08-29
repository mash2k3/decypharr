package nntp

import (
	"context"
	"testing"

	"github.com/sirrobot01/decypharr/internal/config"
)

func TestInteractiveReserveComputation(t *testing.T) {
	state := newInteractiveState(&config.Config{
		Usenet: config.Usenet{
			InteractivePoolReserveEnabled: true,
			InteractivePoolReservePercent: 15,
			InteractivePoolReserveMin:     6,
			InteractivePoolReserveMax:     40,
		},
	})
	state.applyConfig(&config.Config{
		Usenet: config.Usenet{
			InteractivePoolReserveEnabled: true,
			InteractivePoolReservePercent: 15,
			InteractivePoolReserveMin:     6,
			InteractivePoolReserveMax:     40,
		},
	})

	entered, _, _, snap := state.setActive(true, 310, 1, reserveMeta{Entry: "show", File: "ep.mkv", Client: "DFS"})
	if !entered {
		t.Fatal("expected mode enter")
	}
	if snap.Reserved != 40 {
		t.Fatalf("reserved = %d, want 40", snap.Reserved)
	}
	if snap.BackgroundBudget != 270 {
		t.Fatalf("background budget = %d, want 270", snap.BackgroundBudget)
	}

	_, exited, _, _ := state.setActive(false, 310, 0, reserveMeta{})
	if !exited {
		t.Fatal("expected mode exit")
	}
}

func TestInteractiveReserveScalesWithStreams(t *testing.T) {
	state := newInteractiveState(&config.Config{
		Usenet: config.Usenet{
			InteractivePoolReserveEnabled: true,
			InteractivePoolReservePercent: 15,
			InteractivePoolReserveMin:     6,
			InteractivePoolReserveMax:     100,
		},
	})
	_, _, _, snap := state.setActive(true, 310, 1, reserveMeta{})
	if snap.Reserved != 47 {
		t.Fatalf("1 stream reserved = %d, want 47", snap.Reserved)
	}
	changed, snap := state.setStreamCount(310, 2)
	if !changed {
		t.Fatal("expected reserve change for 2 streams")
	}
	if snap.Reserved != 94 {
		t.Fatalf("2 streams reserved = %d, want 94", snap.Reserved)
	}
	if snap.BackgroundBudget != 216 {
		t.Fatalf("2 streams background = %d, want 216", snap.BackgroundBudget)
	}
}

func TestBackgroundBudgetBlocksWhenFull(t *testing.T) {
	state := newInteractiveState(&config.Config{
		Usenet: config.Usenet{InteractivePoolReserveEnabled: true},
	})
	state.setActive(true, 20, 1, reserveMeta{})

	if !state.acquireBackgroundSlot() {
		t.Fatal("first background slot should succeed")
	}
	for i := 0; i < 13; i++ {
		if !state.acquireBackgroundSlot() {
			t.Fatalf("background slot %d should succeed within budget", i+2)
		}
	}
	if state.acquireBackgroundSlot() {
		t.Fatal("background slot should be blocked when budget exhausted")
	}
	state.releaseBackgroundSlot()
	if !state.acquireBackgroundSlot() {
		t.Fatal("background slot should succeed after release")
	}
}

func TestWorkClassFromContext(t *testing.T) {
	ctx := WithWorkClass(context.Background(), WorkClassStream)
	if got := WorkClassFromContext(ctx); got != WorkClassStream {
		t.Fatalf("WorkClassFromContext = %v, want stream", got)
	}
	if got := WorkClassFromContext(context.Background()); got != WorkClassBackground {
		t.Fatalf("default WorkClassFromContext = %v, want background", got)
	}
}

func TestWorkClassIsExplicitlyBackground(t *testing.T) {
	if WorkClassIsExplicitlyBackground(context.Background()) {
		t.Fatal("untagged context must not count as explicitly background")
	}
	bg := WithWorkClass(context.Background(), WorkClassBackground)
	if !WorkClassIsExplicitlyBackground(bg) {
		t.Fatal("tagged background context must be explicitly background")
	}
	stream := WithWorkClass(context.Background(), WorkClassStream)
	if WorkClassIsExplicitlyBackground(stream) {
		t.Fatal("stream context must not be explicitly background")
	}
}

func TestRepairPoolInteractiveCap(t *testing.T) {
	p := &RepairPool{workers: 20}
	p.setInteractiveCap(8)
	if got := p.effectiveCap(); got != 8 {
		t.Fatalf("effectiveCap = %d, want 8", got)
	}
	p.setInteractiveCap(0)
	if got := p.effectiveCap(); got != 20 {
		t.Fatalf("effectiveCap = %d, want 20 when cap cleared", got)
	}
}

func TestProviderHeadroomUsesPerStreamReserve(t *testing.T) {
	state := newInteractiveState(&config.Config{
		Usenet: config.Usenet{
			InteractivePoolReserveEnabled: true,
			InteractivePoolReservePercent: 15,
			InteractivePoolReserveMax:     100,
		},
	})
	_, _, _, snap := state.setActive(true, 310, 1, reserveMeta{})
	if snap.PerStreamReserve != 47 {
		t.Fatalf("perStream = %d, want 47", snap.PerStreamReserve)
	}
	// Provider max 20: reserve 19 slots for playback, background capped at 1.
	if !state.backgroundMayUseProvider(0, 20, 310) {
		t.Fatal("background should be allowed below headroom")
	}
	if state.backgroundMayUseProvider(1, 20, 310) {
		t.Fatal("background should be blocked once provider headroom is taken")
	}
}

func TestProviderHeadroom(t *testing.T) {
	state := newInteractiveState(&config.Config{
		Usenet: config.Usenet{
			InteractivePoolReserveEnabled: true,
			InteractivePoolReservePercent: 15,
			InteractivePoolReserveMax:     100,
		},
	})
	_, _, _, snap := state.setActive(true, 100, 1, reserveMeta{})
	if snap.Reserved != 15 {
		t.Fatalf("reserved = %d, want 15", snap.Reserved)
	}
	if !state.backgroundMayUseProvider(84, 100, 100) {
		t.Fatal("background should be allowed below headroom")
	}
	if state.backgroundMayUseProvider(86, 100, 100) {
		t.Fatal("background should be blocked at headroom")
	}
}
