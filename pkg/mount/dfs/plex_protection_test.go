package dfs

import (
	"testing"

	"github.com/sirrobot01/decypharr/pkg/manager"
)

func TestResolveProtectedStreamsSkipsInactiveSessions(t *testing.T) {
	out := resolveProtectedStreams(nil, []plexSession{
		{Type: "episode", Duration: 0, ViewOffset: 0, GrandparentTitle: "Show", ParentIndex: 1, Index: 1},
		{Type: "movie", Duration: 0, ViewOffset: 500, Title: "Inception"},
	})
	if len(out) != 0 {
		t.Fatalf("expected 0 protected streams, got %d", len(out))
	}
}

func TestMountBasenameCandidatesKeepsDirectMountName(t *testing.T) {
	release := "Shrek.2.2004.NORDiC.1080p.WEB-DL.H.264.DDP5.1-NoTrace.mkv"
	got := mountBasenameCandidates(release)
	if len(got) != 1 || got[0] != release {
		t.Fatalf("direct mount candidates = %v, want [%q]", got, release)
	}
}

func TestMountBasenameCandidatesExtractsCliDebridSymlink(t *testing.T) {
	symlink := "Shrek 2 (2004) - tt0298148 - 1080p - (Shrek.2.2004.NORDiC.1080p.WEB-DL.H.264.DDP5.1-NoTrace.mkv"
	got := mountBasenameCandidates(symlink)
	if len(got) != 2 {
		t.Fatalf("candidates = %v, want 2 entries", got)
	}
	if got[1] != "Shrek.2.2004.NORDiC.1080p.WEB-DL.H.264.DDP5.1-NoTrace.mkv" {
		t.Fatalf("release candidate = %q", got[1])
	}
}

func TestResolveProtectedStreamsDedupesByFile(t *testing.T) {
	// Without a manager file match returns empty; verify dedupe path doesn't panic.
	out := resolveProtectedStreams(&manager.Manager{}, []plexSession{
		{Type: "episode", Duration: 1000, ViewOffset: 500, GrandparentTitle: "Show", ParentIndex: 1, Index: 1, RatingKey: "1"},
		{Type: "episode", Duration: 1000, ViewOffset: 600, GrandparentTitle: "Show", ParentIndex: 1, Index: 1, RatingKey: "2"},
	})
	if len(out) != 0 {
		t.Fatalf("expected no matches without library entries, got %d", len(out))
	}
}
