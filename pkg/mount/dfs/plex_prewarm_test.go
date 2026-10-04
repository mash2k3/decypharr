package dfs

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"github.com/sirrobot01/decypharr/internal/utils"
)

func TestNormalizeEpisodeTitle(t *testing.T) {
	tests := map[string]string{
		"Sons of Anarchy":      "sonsofanarchy",
		"Sons of Anarchy 2008": "sonsofanarchy",
		"The Office (US)":      "theoffice",
		"1923":                 "1923",
		"The 100":              "the100",
	}
	for input, want := range tests {
		if got := normalizeEpisodeTitle(input); got != want {
			t.Errorf("normalizeEpisodeTitle(%q) = %q, want %q", input, got, want)
		}
	}
}

func TestMatchesRenamedCliDebridEpisode(t *testing.T) {
	wantTitle := normalizeEpisodeTitle("Sons of Anarchy")
	original := utils.ParseTorrentName("Sons.of.Anarchy.2008.S03E05.1080p.BDRip.x264-Vialle.mkv")
	if !matchesEpisodeIdentity(wantTitle, 3, 5, utils.ParsedName{}, original) {
		t.Fatalf("original release name did not match Plex's canonical show and episode identity: %#v", original)
	}
	if matchesEpisodeIdentity(wantTitle, 3, 6, utils.ParsedName{}, original) {
		t.Fatal("episode 5 incorrectly matched episode 6")
	}
}

func TestFetchNextEpisodeAcrossSeasonBoundary(t *testing.T) {
	const nextFilename = "Sons of Anarchy (2008) - S04E01 - Out.mkv"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("X-Plex-Token"); got != "test-token" {
			t.Errorf("token = %q, want test-token", got)
		}
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/library/metadata/season-3/children":
			fmt.Fprint(w, `{"MediaContainer":{"Metadata":[{"index":13,"Media":[{"Part":[{"file":"/tv/S03E13.mkv"}]}]}]}}`)
		case "/library/metadata/show/children":
			fmt.Fprint(w, `{"MediaContainer":{"Metadata":[{"index":3,"ratingKey":"season-3"},{"index":4,"ratingKey":"season-4"}]}}`)
		case "/library/metadata/season-4/children":
			fmt.Fprintf(w, `{"MediaContainer":{"Metadata":[{"index":1,"Media":[{"Part":[{"file":%q}]}]}]}}`, "/tv/"+nextFilename)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	target := fetchNextEpisodeFilenameAcrossSeasons(context.Background(), server.Client(), server.URL, "test-token", plexSession{
		ParentRatingKey:      "season-3",
		GrandparentRatingKey: "show",
		ParentIndex:          3,
		Index:                13,
	})
	if target.Filename != nextFilename || target.Season != 4 || target.Episode != 1 {
		t.Fatalf("target = %#v, want S04E01 %q", target, nextFilename)
	}
}

func TestFetchNextEpisodeWithinSeason(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"MediaContainer":{"Metadata":[{"index":5,"Media":[{"Part":[{"file":"/tv/S03E05.mkv"}]}]}]}}`)
	}))
	defer server.Close()

	target := fetchNextEpisodeFilenameAcrossSeasons(context.Background(), server.Client(), server.URL, "", plexSession{
		ParentRatingKey: "season-3",
		ParentIndex:     3,
		Index:           4,
	})
	if target.Filename != "S03E05.mkv" || target.Season != 3 || target.Episode != 5 {
		t.Fatalf("target = %#v, want S03E05", target)
	}
}

func TestPrewarmMissCanRetry(t *testing.T) {
	seen := &prewarmedSessions{entries: make(map[string]time.Time)}
	if !seen.markIfNew("episode") {
		t.Fatal("first attempt was unexpectedly suppressed")
	}
	if seen.markIfNew("episode") {
		t.Fatal("duplicate attempt was not suppressed")
	}
	seen.forget("episode")
	if !seen.markIfNew("episode") {
		t.Fatal("attempt remained suppressed after a miss was forgotten")
	}
}

// ParseTorrentName drops numeric show titles entirely ("24.S02E01..." ->
// title ""), so episodes of "24" never matched Plex's show title.
func TestNumericShowTitleMatches(t *testing.T) {
	if got := titleBeforeEpisodeMarker("24.S02E01.1080p.BluRay.x264-SHORTBREHD.mkv"); got != "24" {
		t.Fatalf("titleBeforeEpisodeMarker = %q, want 24", got)
	}
	if got := titleBeforeEpisodeMarker("S02E01.mkv"); got != "" {
		t.Fatalf("no title before marker should give empty, got %q", got)
	}
	file := utils.ParseTorrentName("24.S02E01.1080p.BluRay.x264-SHORTBREHD.mkv")
	if file.Title != "" {
		t.Skipf("parser now keeps the title (%q); fallback not needed", file.Title)
	}
	want := normalizeEpisodeTitle("24")
	if matchesEpisodeIdentity(want, 2, 1, utils.ParsedName{}, file) {
		t.Fatal("expected the raw parse to miss (documents the bug)")
	}
	file.Title = titleBeforeEpisodeMarker("24.S02E01.1080p.BluRay.x264-SHORTBREHD.mkv")
	if !matchesEpisodeIdentity(want, 2, 1, utils.ParsedName{}, file) {
		t.Fatal("with the fallback title, 24 S02E01 should match")
	}
}

// Plex reports the symlink path; when it is readable here, its target (the
// exact <entry>/<file> in the mount) is tried first.
func TestPlexPathCandidatesFollowsSymlink(t *testing.T) {
	dir := t.TempDir()
	target := dir + "/mnt/__all__/24.S02.1080p.BluRay.x264-SHORTBREHD/24.S02E01.mkv"
	link := dir + "/library/24 (2001) - S02E01 - (24.S02E01.1080p.BluRay.x264-SHORTBREHD).mkv"
	if err := os.MkdirAll(dir+"/library", 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	got := plexPathCandidates(link)
	if len(got) != 2 || got[0] != target || got[1] != link {
		t.Fatalf("candidates = %v, want [target, link]", got)
	}
	if got := plexPathCandidates(dir + "/not-a-link.mkv"); len(got) != 1 {
		t.Fatalf("plain path: %v", got)
	}
}
