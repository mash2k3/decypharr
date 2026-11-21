package dfs

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	fuseconfig "github.com/sirrobot01/decypharr/pkg/mount/dfs/config"
)

// Payloads trimmed from real Jellyfin 12.4 / Emby 4.10 answers.
const jfSessions = `[
 {"Id":"s1","UserName":"alice","NowPlayingItem":{"Id":"e1","Type":"Episode","Name":"Pilot","SeriesName":"Show",
  "SeriesId":"ser","SeasonId":"sea1","ParentIndexNumber":1,"IndexNumber":1,"RunTimeTicks":10000000,
  "Path":"/media/tv/Show/Season 01/Show - S01E01 - Pilot - (Show.S01E01.1080p-GRP.mkv).mkv"},
  "PlayState":{"PositionTicks":8000000,"MediaSourceId":"e1"}},
 {"Id":"idle","UserName":"bob"}]`

const jfEpisodes = `{"Items":[
 {"Id":"e0","ParentIndexNumber":0,"IndexNumber":1,"Path":"/media/special.mkv"},
 {"Id":"e3","ParentIndexNumber":2,"IndexNumber":1,"Path":"/m/__all__/Show.S02E01-GRP/Show.S02E01-GRP.mkv"},
 {"Id":"e2","ParentIndexNumber":1,"IndexNumber":2,"Path":"/m/__all__/Show.S01E02-GRP/Show.S01E02-GRP.mkv"},
 {"Id":"e1","ParentIndexNumber":1,"IndexNumber":1,"Path":"/m/x.mkv"}]}`

func jellyfinServer(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != `MediaBrowser Token="key"` || r.Header.Get("X-Emby-Token") != "key" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		switch r.URL.Path {
		case "/Sessions":
			w.Write([]byte(jfSessions))
		case "/Shows/ser/Episodes":
			w.Write([]byte(jfEpisodes))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestJellyfinSessionsAsPlexSessions(t *testing.T) {
	srv := jellyfinServer(t)
	got, err := fetchJellyfinSessions(context.Background(), srv.Client(), srv.URL+"/", "key")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("got %d sessions, want 1 (the idle one has nothing playing)", len(got))
	}
	s := got[0]
	if s.Type != "episode" || s.GrandparentTitle != "Show" || s.ParentIndex != 1 || s.Index != 1 ||
		s.ViewOffset != 800 || s.Duration != 1000 || s.RatingKey != "jf:e1" || s.GrandparentRatingKey != "jf:ser" {
		t.Fatalf("unexpected session %+v", s)
	}
	if s.sessionBasename() != "Show - S01E01 - Pilot - (Show.S01E01.1080p-GRP.mkv).mkv" {
		t.Fatalf("path %q", s.sessionPath())
	}
}

func TestJellyfinNextEpisode(t *testing.T) {
	srv := jellyfinServer(t)
	s := plexSession{GrandparentRatingKey: "jf:ser", ParentIndex: 1, Index: 1}
	got := fetchJellyfinNextEpisode(context.Background(), srv.Client(), srv.URL, "key", s)
	if got.Season != 1 || got.Episode != 2 || got.Filename != "Show.S01E02-GRP.mkv" {
		t.Fatalf("next = %+v", got)
	}
	s.Index = 2 // season finale: next season's first episode
	if got := fetchJellyfinNextEpisode(context.Background(), srv.Client(), srv.URL, "key", s); got.Season != 2 || got.Episode != 1 {
		t.Fatalf("after finale = %+v", got)
	}
	s.GrandparentRatingKey = "123" // a Plex key is not looked up on Jellyfin
	if got := fetchJellyfinNextEpisode(context.Background(), srv.Client(), srv.URL, "key", s); got.Filename != "" {
		t.Fatalf("plex key answered: %+v", got)
	}
}

func TestJellyfinBadKeyIsAnError(t *testing.T) {
	srv := jellyfinServer(t)
	if _, err := fetchJellyfinSessions(context.Background(), srv.Client(), srv.URL, "wrong"); err == nil {
		t.Fatal("want an error for a refused key")
	}
}

func TestSessionSourcesFromConfig(t *testing.T) {
	client := http.DefaultClient
	m := &Manager{config: &fuseconfig.FuseConfig{}}
	if n := len(m.sessionSources(client)); n != 0 {
		t.Fatalf("no servers set: %d sources", n)
	}
	m.config.PlexURL, m.config.PlexToken = "http://plex", "t"
	m.config.JellyfinURL, m.config.JellyfinToken = "http://jf", "k"
	src := m.sessionSources(client)
	if len(src) != 2 || src[0].name != "Plex" || src[1].name != "Jellyfin/Emby" {
		t.Fatalf("sources %+v", src)
	}
}
