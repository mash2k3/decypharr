package dfs

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"testing"

	fuseconfig "github.com/sirrobot01/decypharr/pkg/mount/dfs/config"
)

// Payloads trimmed from a real Silo (2026-11, admin API key).
const siloSessions = `{"items":[{"session_id":"s1","media_item_id":"episode-tvdb-73244-1-1","media_file_id":"2",
 "media_type":"episode","position_seconds":30,"file_duration":60,"season_number":1,"episode_number":1}],
 "page":{"has_more":false}}`

var siloItems = map[string]string{
	"episode-tvdb-73244-1-1": `{"content_id":"episode-tvdb-73244-1-1","type":"episode","title":"Pilot",
 "series_id":"series-tvdb-73244","series_title":"The Office (US)","season_number":1,"episode_number":1,
 "versions":[{"file_id":"9","file_path":"/media/TV Shows/Office/e1-4k.mkv"},
             {"file_id":"2","file_path":"/media/TV Shows/Office/The Office (2005) - S01E01.mkv"}]}`,
	"episode-tvdb-73244-2-1": `{"content_id":"episode-tvdb-73244-2-1","type":"episode","title":"The Dundies",
 "series_id":"series-tvdb-73244","season_number":2,"episode_number":1,
 "versions":[{"file_id":"5","file_path":"/media/TV Shows/Office/The Office (2005) - S02E01.mkv"}]}`,
}

func siloServer(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer sa_key" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		switch r.URL.Path {
		case "/api/v2/admin/sessions":
			w.Write([]byte(siloSessions))
		case "/api/v2/admin/catalog/items":
			if r.URL.Query().Get("include_file_paths") != "true" {
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			ids := r.URL.Query()["content_ids"]
			sort.Strings(ids)
			var found []string
			for _, id := range ids {
				if it, ok := siloItems[id]; ok {
					found = append(found, it)
				}
			}
			w.Write([]byte(`{"items":[` + strings.Join(found, ",") + `],"page":{"has_more":false}}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestSiloSessionsAsPlexSessions(t *testing.T) {
	srv := siloServer(t)
	got, err := fetchSiloSessions(context.Background(), srv.Client(), srv.URL+"/", "sa_key")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("got %d sessions, want 1", len(got))
	}
	s := got[0]
	if s.Type != "episode" || s.GrandparentTitle != "The Office (US)" || s.ParentIndex != 1 || s.Index != 1 ||
		s.ViewOffset != 30000 || s.Duration != 60000 || s.RatingKey != "silo:episode-tvdb-73244-1-1" ||
		s.GrandparentRatingKey != "silo:series-tvdb-73244" {
		t.Fatalf("unexpected session %+v", s)
	}
	// The file being played (media_file_id 2) comes first, not the 4K version.
	if s.sessionBasename() != "The Office (2005) - S01E01.mkv" {
		t.Fatalf("playing file = %q", s.sessionBasename())
	}
}

func TestSiloNextEpisodeCrossesTheSeason(t *testing.T) {
	srv := siloServer(t)
	s := plexSession{RatingKey: "silo:episode-tvdb-73244-1-1"}
	got := fetchSiloNextEpisode(context.Background(), srv.Client(), srv.URL, "sa_key", s)
	if got.Filename != "The Office (2005) - S02E01.mkv" || got.Season != 2 || got.Episode != 1 {
		t.Fatalf("next = %+v", got)
	}
	if got := fetchSiloNextEpisode(context.Background(), srv.Client(), srv.URL, "sa_key", plexSession{RatingKey: "jf:x"}); got.Filename != "" {
		t.Fatalf("a non-Silo key must give no target, got %+v", got)
	}
}

func TestSiloNextEpisodeIDs(t *testing.T) {
	got := siloNextEpisodeIDs("episode-tvdb-73244-1-9")
	if len(got) != 2 || got[0] != "episode-tvdb-73244-1-10" || got[1] != "episode-tvdb-73244-2-1" {
		t.Fatalf("got %v", got)
	}
	if siloNextEpisodeIDs("movie-tmdb-5") != nil || siloNextEpisodeIDs("episode-x-y-a-b") != nil {
		t.Fatal("ids of another shape must give nothing")
	}
}

func TestSiloBadKeyIsAnError(t *testing.T) {
	srv := siloServer(t)
	if _, err := fetchSiloSessions(context.Background(), srv.Client(), srv.URL, "wrong"); err == nil {
		t.Fatal("want an error for a refused key")
	}
}

func TestSiloSessionSourceFromConfig(t *testing.T) {
	m := &Manager{config: &fuseconfig.FuseConfig{SiloURL: "http://silo", SiloAPIKey: "sa_key"}}
	src := m.sessionSources(http.DefaultClient)
	if len(src) != 1 || src[0].name != "Silo" {
		t.Fatalf("sources = %+v", src)
	}
}
