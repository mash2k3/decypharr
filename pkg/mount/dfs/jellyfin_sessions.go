package dfs

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"path"
	"sort"
	"strings"
)

// Jellyfin and Emby (same API) as a session source for stream protection
// and show precache, next to Plex. Their /Sessions entries are turned into
// plexSession values so the matching code works the same for every server;
// keys carry a "jf:" prefix so they never collide with Plex ratingKeys.
// Checked against Jellyfin 12.4 and Emby 4.10: both answer /Sessions and
// /Shows/{id}/Episodes at the root path with an API key.

const jellyfinKeyPrefix = "jf:"

type jellyfinItem struct {
	ID                string `json:"Id"`
	Type              string `json:"Type"`
	Name              string `json:"Name"`
	ProductionYear    int    `json:"ProductionYear"`
	SeriesName        string `json:"SeriesName"`
	SeriesID          string `json:"SeriesId"`
	SeasonID          string `json:"SeasonId"`
	ParentIndexNumber int    `json:"ParentIndexNumber"`
	IndexNumber       int    `json:"IndexNumber"`
	RunTimeTicks      int64  `json:"RunTimeTicks"`
	Path              string `json:"Path"`
	MediaSources      []struct {
		Path string `json:"Path"`
	} `json:"MediaSources"`
}

type jellyfinSession struct {
	NowPlayingItem *jellyfinItem `json:"NowPlayingItem"`
	PlayState      struct {
		PositionTicks int64  `json:"PositionTicks"`
		MediaSourceID string `json:"MediaSourceId"`
	} `json:"PlayState"`
}

func jellyfinGet(ctx context.Context, client *http.Client, baseURL, token, p string, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(baseURL, "/")+p, nil)
	if err != nil {
		return err
	}
	tok := strings.ReplaceAll(strings.TrimSpace(token), `"`, "")
	// Jellyfin reads the Authorization header (the legacy X-Emby-Token is
	// off by default on new servers); Emby reads X-Emby-Token.
	req.Header.Set("Authorization", `MediaBrowser Token="`+tok+`"`)
	req.Header.Set("X-Emby-Token", tok)
	req.Header.Set("Accept", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("jellyfin/emby returned status %d", resp.StatusCode)
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

// files of an item: the version being played first (when known), then the
// item's own path and its other versions.
func (it *jellyfinItem) files(first string) []string {
	var out []string
	seen := map[string]bool{}
	add := func(p string) {
		if p != "" && !seen[p] {
			seen[p] = true
			out = append(out, p)
		}
	}
	add(first)
	add(it.Path)
	for _, ms := range it.MediaSources {
		add(ms.Path)
	}
	return out
}

func (it *jellyfinItem) toPlex(first string) plexSession {
	s := plexSession{
		RatingKey:        jellyfinKeyPrefix + it.ID,
		Type:             strings.ToLower(it.Type),
		Title:            it.Name,
		Year:             it.ProductionYear,
		GrandparentTitle: it.SeriesName,
		ParentIndex:      it.ParentIndexNumber,
		Index:            it.IndexNumber,
		Duration:         it.RunTimeTicks / 10_000,
	}
	if it.SeasonID != "" {
		s.ParentRatingKey = jellyfinKeyPrefix + it.SeasonID
	}
	if it.SeriesID != "" {
		s.GrandparentRatingKey = jellyfinKeyPrefix + it.SeriesID
	}
	for _, f := range it.files(first) {
		s.Media = append(s.Media, struct {
			Part []struct {
				File string `json:"file"`
			} `json:"Part"`
		}{Part: []struct {
			File string `json:"file"`
		}{{File: f}}})
	}
	return s
}

// fetchJellyfinSessions lists what Jellyfin/Emby is playing, as plexSessions.
func fetchJellyfinSessions(ctx context.Context, client *http.Client, baseURL, token string) ([]plexSession, error) {
	var raw []jellyfinSession
	if err := jellyfinGet(ctx, client, baseURL, token, "/Sessions", &raw); err != nil {
		return nil, err
	}
	out := make([]plexSession, 0, len(raw))
	for _, s := range raw {
		it := s.NowPlayingItem
		if it == nil {
			continue
		}
		// Several versions merged into one item: MediaSourceId is the one
		// playing (its own item id).
		first := ""
		if ms := s.PlayState.MediaSourceID; ms != "" && ms != it.ID {
			var found struct {
				Items []jellyfinItem `json:"Items"`
			}
			if err := jellyfinGet(ctx, client, baseURL, token,
				"/Items?Fields=Path,MediaSources&Ids="+url.QueryEscape(ms), &found); err == nil && len(found.Items) > 0 {
				first = found.Items[0].Path
			}
		}
		p := it.toPlex(first)
		p.ViewOffset = s.PlayState.PositionTicks / 10_000
		out = append(out, p)
	}
	return out, nil
}

// fetchJellyfinNextEpisode returns the episode after s in its series (by
// season, then episode; specials left out).
func fetchJellyfinNextEpisode(ctx context.Context, client *http.Client, baseURL, token string, s plexSession) plexEpisodeTarget {
	series := strings.TrimPrefix(s.GrandparentRatingKey, jellyfinKeyPrefix)
	if series == "" || series == s.GrandparentRatingKey {
		return plexEpisodeTarget{}
	}
	var eps struct {
		Items []jellyfinItem `json:"Items"`
	}
	if err := jellyfinGet(ctx, client, baseURL, token,
		"/Shows/"+url.PathEscape(series)+"/Episodes?Fields=Path,MediaSources", &eps); err != nil {
		return plexEpisodeTarget{}
	}
	later := eps.Items[:0]
	for _, e := range eps.Items {
		if e.ParentIndexNumber > 0 && (e.ParentIndexNumber > s.ParentIndex ||
			e.ParentIndexNumber == s.ParentIndex && e.IndexNumber > s.Index) {
			later = append(later, e)
		}
	}
	if len(later) == 0 {
		return plexEpisodeTarget{}
	}
	sort.SliceStable(later, func(i, j int) bool {
		if later[i].ParentIndexNumber != later[j].ParentIndexNumber {
			return later[i].ParentIndexNumber < later[j].ParentIndexNumber
		}
		return later[i].IndexNumber < later[j].IndexNumber
	})
	next := later[0]
	files := next.files("")
	if len(files) == 0 {
		return plexEpisodeTarget{}
	}
	return plexEpisodeTarget{Filename: path.Base(files[0]), Path: files[0], Season: next.ParentIndexNumber, Episode: next.IndexNumber}
}
