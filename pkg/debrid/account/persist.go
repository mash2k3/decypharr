package account

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"sync"
	"time"

	"github.com/sirrobot01/decypharr/internal/config"
	"github.com/sirrobot01/decypharr/pkg/debrid/types"
)

// Download links are kept on disk so a restart doesn't throw them away.
// Without this, every file opened after a restart (Plex scans and analysis,
// ffprobe checks, cache warming) mints a new link even though the old one is
// still valid for days. AllDebrid counts each /link/unlock as a generated
// link and suspends accounts that generate thousands of links without
// downloading.
//
// Only links with an ExpiresAt still in the future are saved or loaded.
// API tokens are not written: links are grouped by a hash of the account
// token and get the account's token back on load.

// linkSaveDelay batches link changes into one write.
const linkSaveDelay = 30 * time.Second

var unsafeFileChars = regexp.MustCompile(`[^A-Za-z0-9._-]`)

type linkStore struct {
	path  string
	mu    sync.Mutex
	timer *time.Timer
}

type savedLink struct {
	Debrid       string    `json:"debrid"`
	Filename     string    `json:"filename"`
	Link         string    `json:"link"`
	DownloadLink string    `json:"download_link"`
	Generated    time.Time `json:"generated"`
	Size         int64     `json:"size"`
	Id           string    `json:"id"`
	ExpiresAt    time.Time `json:"expires_at"`
}

func tokenKey(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:8])
}

// newLinkStore returns nil when there is no config directory to write to.
func newLinkStore(debrid string) *linkStore {
	dir := config.GetMainPath()
	if dir == "" || debrid == "" {
		return nil
	}
	name := unsafeFileChars.ReplaceAllString(debrid, "_") + ".json"
	return &linkStore{path: filepath.Join(dir, "links", name)}
}

// load restores unexpired links into the manager's accounts.
func (s *linkStore) load(m *Manager) {
	data, err := os.ReadFile(s.path)
	if err != nil {
		if !os.IsNotExist(err) {
			m.logger.Warn().Err(err).Str("path", s.path).Msg("Failed to read saved download links")
		}
		return
	}
	var saved map[string][]savedLink
	if err := json.Unmarshal(data, &saved); err != nil {
		m.logger.Warn().Err(err).Str("path", s.path).Msg("Ignoring unreadable saved download links")
		return
	}
	now := time.Now()
	restored := 0
	m.accounts.Range(func(_ string, acc *Account) bool {
		for _, l := range saved[tokenKey(acc.Token)] {
			if l.ExpiresAt.IsZero() || !now.Before(l.ExpiresAt) || l.DownloadLink == "" {
				continue
			}
			acc.links.Store(acc.sliceFileLink(l.Link), types.DownloadLink{
				Debrid:       l.Debrid,
				Token:        acc.Token,
				Filename:     l.Filename,
				Link:         l.Link,
				DownloadLink: l.DownloadLink,
				Generated:    l.Generated,
				Size:         l.Size,
				Id:           l.Id,
				ExpiresAt:    l.ExpiresAt,
			})
			restored++
		}
		return true
	})
	if restored > 0 {
		m.logger.Info().Int("links", restored).Msg("Restored saved download links")
	}
}

// scheduleSave writes the links linkSaveDelay after the first unsaved change.
func (s *linkStore) scheduleSave(m *Manager) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.timer != nil {
		return
	}
	s.timer = time.AfterFunc(linkSaveDelay, func() {
		s.mu.Lock()
		s.timer = nil
		s.mu.Unlock()
		s.save(m)
	})
}

func (s *linkStore) save(m *Manager) {
	now := time.Now()
	saved := make(map[string][]savedLink)
	m.accounts.Range(func(_ string, acc *Account) bool {
		key := tokenKey(acc.Token)
		acc.links.Range(func(_ string, l types.DownloadLink) bool {
			if l.ExpiresAt.IsZero() || !now.Before(l.ExpiresAt) || l.DownloadLink == "" {
				return true
			}
			saved[key] = append(saved[key], savedLink{
				Debrid:       l.Debrid,
				Filename:     l.Filename,
				Link:         l.Link,
				DownloadLink: l.DownloadLink,
				Generated:    l.Generated,
				Size:         l.Size,
				Id:           l.Id,
				ExpiresAt:    l.ExpiresAt,
			})
			return true
		})
		return true
	})

	data, err := json.Marshal(saved)
	if err != nil {
		m.logger.Warn().Err(err).Msg("Failed to encode download links")
		return
	}
	if err := os.MkdirAll(filepath.Dir(s.path), 0o755); err != nil {
		m.logger.Warn().Err(err).Str("path", s.path).Msg("Failed to create download link directory")
		return
	}
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		m.logger.Warn().Err(err).Str("path", tmp).Msg("Failed to write download links")
		return
	}
	if err := os.Rename(tmp, s.path); err != nil {
		m.logger.Warn().Err(err).Str("path", s.path).Msg("Failed to save download links")
	}
}
