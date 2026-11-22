package dfs

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"path"
	"strconv"
	"strings"
)

// Silo (github.com/Silo-Server/silo-server) as a session source for stream
// protection and show precache, next to Plex and Jellyfin/Emby. Its admin
// API is used with an admin API key ("sa_..."):
//
//   - GET /api/v2/admin/sessions: what is playing (item id, file id,
//     position, season/episode), without paths;
//   - GET /api/v2/admin/catalog/items?content_ids=...&include_file_paths=true:
//     the files of those items, the one playing first.
//
// Silo's episode ids are "episode-<provider>-<series>-<season>-<episode>", so
// the next episode is looked up by id (the episode listings need a profile).
// Paths are the ones Silo sees; matching to mount files is by name, as for a
// Plex that sees the mount at another path. Keys carry a "silo:" prefix.

const siloKeyPrefix = "silo:"

type siloSession struct {
	MediaItemID     string  `json:"media_item_id"`
	MediaFileID     string  `json:"media_file_id"`
	PositionSeconds float64 `json:"position_seconds"`
	FileDuration    float64 `json:"file_duration"`
}

type siloCatalogItem struct {
	ContentID     string `json:"content_id"`
	Type          string `json:"type"`
	Title         string `json:"title"`
	Year          int    `json:"year"`
	SeriesID      string `json:"series_id"`
	SeriesTitle   string `json:"series_title"`
	SeasonNumber  int    `json:"season_number"`
	EpisodeNumber int    `json:"episode_number"`
	Versions      []struct {
		FileID   string `json:"file_id"`
		FilePath string `json:"file_path"`
	} `json:"versions"`
}

type siloPage struct {
	HasMore    bool   `json:"has_more"`
	NextCursor string `json:"next_cursor"`
}

func siloGet(ctx context.Context, client *http.Client, baseURL, key, p string, q url.Values, out any) error {
	u := strings.TrimRight(baseURL, "/") + p
	if len(q) > 0 {
		u += "?" + q.Encode()
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+strings.TrimSpace(key))
	req.Header.Set("Accept", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("silo returned status %d", resp.StatusCode)
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

// siloCatalog returns the catalog entries of these content ids, by id.
func siloCatalog(ctx context.Context, client *http.Client, baseURL, key string, ids []string) (map[string]siloCatalogItem, error) {
	out := map[string]siloCatalogItem{}
	if len(ids) == 0 {
		return out, nil
	}
	q := url.Values{"include_file_paths": {"true"}, "limit": {"200"}}
	for _, id := range ids {
		q.Add("content_ids", id)
	}
	var body struct {
		Items []siloCatalogItem `json:"items"`
	}
	if err := siloGet(ctx, client, baseURL, key, "/api/v2/admin/catalog/items", q, &body); err != nil {
		return nil, err
	}
	for _, it := range body.Items {
		out[it.ContentID] = it
	}
	return out, nil
}

// files of an item: the playing file first (when known), then its other versions.
func (it siloCatalogItem) files(playingFileID string) []string {
	var first, rest []string
	for _, v := range it.Versions {
		if v.FilePath == "" {
			continue
		}
		if playingFileID != "" && v.FileID == playingFileID {
			first = append(first, v.FilePath)
		} else {
			rest = append(rest, v.FilePath)
		}
	}
	return append(first, rest...)
}

func (it siloCatalogItem) toPlex(playingFileID string) plexSession {
	s := plexSession{
		RatingKey:        siloKeyPrefix + it.ContentID,
		Type:             strings.ToLower(it.Type),
		Title:            it.Title,
		Year:             it.Year,
		GrandparentTitle: it.SeriesTitle,
		ParentIndex:      it.SeasonNumber,
		Index:            it.EpisodeNumber,
	}
	if it.SeriesID != "" {
		s.GrandparentRatingKey = siloKeyPrefix + it.SeriesID
	}
	for _, f := range it.files(playingFileID) {
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

// fetchSiloSessions lists what Silo is playing, as plexSessions.
func fetchSiloSessions(ctx context.Context, client *http.Client, baseURL, key string) ([]plexSession, error) {
	var raw []siloSession
	cursor := ""
	for i := 0; i < 20; i++ {
		q := url.Values{"limit": {"200"}}
		if cursor != "" {
			q.Set("cursor", cursor)
		}
		var body struct {
			Items []siloSession `json:"items"`
			Page  siloPage      `json:"page"`
		}
		if err := siloGet(ctx, client, baseURL, key, "/api/v2/admin/sessions", q, &body); err != nil {
			return nil, err
		}
		raw = append(raw, body.Items...)
		if !body.Page.HasMore || body.Page.NextCursor == "" {
			break
		}
		cursor = body.Page.NextCursor
	}
	if len(raw) == 0 {
		return nil, nil
	}
	seen := map[string]bool{}
	var ids []string
	for _, s := range raw {
		if s.MediaItemID != "" && !seen[s.MediaItemID] {
			seen[s.MediaItemID] = true
			ids = append(ids, s.MediaItemID)
		}
	}
	items, err := siloCatalog(ctx, client, baseURL, key, ids)
	if err != nil {
		return nil, err
	}
	out := make([]plexSession, 0, len(raw))
	for _, s := range raw {
		it, ok := items[s.MediaItemID]
		if !ok {
			continue
		}
		p := it.toPlex(s.MediaFileID)
		p.ViewOffset = int64(s.PositionSeconds * 1000)
		p.Duration = int64(s.FileDuration * 1000)
		out = append(out, p)
	}
	return out, nil
}

// siloNextEpisodeIDs: the next episode of the season, then the first of the
// next season, for an id "episode-<provider>-<series>-<season>-<episode>".
func siloNextEpisodeIDs(contentID string) []string {
	parts := strings.Split(contentID, "-")
	if len(parts) < 5 || parts[0] != "episode" {
		return nil
	}
	season, err1 := strconv.Atoi(parts[len(parts)-2])
	episode, err2 := strconv.Atoi(parts[len(parts)-1])
	if err1 != nil || err2 != nil {
		return nil
	}
	stem := strings.Join(parts[:len(parts)-2], "-")
	return []string{
		fmt.Sprintf("%s-%d-%d", stem, season, episode+1),
		fmt.Sprintf("%s-%d-1", stem, season+1),
	}
}

// fetchSiloNextEpisode returns the episode after s, or an empty target.
func fetchSiloNextEpisode(ctx context.Context, client *http.Client, baseURL, key string, s plexSession) plexEpisodeTarget {
	cur := strings.TrimPrefix(s.RatingKey, siloKeyPrefix)
	if cur == s.RatingKey {
		return plexEpisodeTarget{}
	}
	ids := siloNextEpisodeIDs(cur)
	items, err := siloCatalog(ctx, client, baseURL, key, ids)
	if err != nil {
		return plexEpisodeTarget{}
	}
	for _, id := range ids {
		it, ok := items[id]
		if !ok {
			continue
		}
		files := it.files("")
		if len(files) == 0 {
			continue
		}
		return plexEpisodeTarget{Filename: path.Base(files[0]), Path: files[0], Season: it.SeasonNumber, Episode: it.EpisodeNumber}
	}
	return plexEpisodeTarget{}
}
