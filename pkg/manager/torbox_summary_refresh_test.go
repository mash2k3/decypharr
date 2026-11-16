package manager

import (
	"context"
	"fmt"
	"sync"
	"testing"

	"github.com/puzpuzpuz/xsync/v4"
	"github.com/rs/zerolog"
	"github.com/sirrobot01/decypharr/internal/config"
	"github.com/sirrobot01/decypharr/pkg/debrid/account"
	debrid "github.com/sirrobot01/decypharr/pkg/debrid/common"
	debridTypes "github.com/sirrobot01/decypharr/pkg/debrid/types"
	"github.com/sirrobot01/decypharr/pkg/storage"
)

type summaryRefreshClient struct {
	summaries []*debridTypes.Torrent
	mu        sync.Mutex
	updates   []string
}

func (f *summaryRefreshClient) SubmitMagnet(t *debridTypes.Torrent) (*debridTypes.Torrent, error) {
	return t, nil
}
func (f *summaryRefreshClient) CheckStatus(t *debridTypes.Torrent) (*debridTypes.Torrent, error) {
	return t, nil
}
func (f *summaryRefreshClient) GetDownloadLink(string, *debridTypes.File) (debridTypes.DownloadLink, error) {
	return debridTypes.DownloadLink{}, nil
}
func (f *summaryRefreshClient) DeleteTorrent(string) error { return nil }
func (f *summaryRefreshClient) IsAvailable([]string) map[string]bool {
	return nil
}
func (f *summaryRefreshClient) UpdateTorrent(t *debridTypes.Torrent) error {
	f.mu.Lock()
	f.updates = append(f.updates, t.Id)
	f.mu.Unlock()
	t.Status = debridTypes.TorrentStatusDownloaded
	t.SummaryOnly = false
	t.Files = map[string]debridTypes.File{
		"movie.mkv": {
			TorrentId: t.Id,
			Id:        "1",
			Name:      "movie.mkv",
			Size:      100,
			Path:      "movie.mkv",
			Link:      "torbox://" + t.Id + "/1",
		},
	}
	return nil
}
func (f *summaryRefreshClient) GetTorrent(string) (*debridTypes.Torrent, error) {
	return nil, nil
}
func (f *summaryRefreshClient) GetTorrents() ([]*debridTypes.Torrent, error) {
	return f.summaries, nil
}
func (f *summaryRefreshClient) Config() config.Debrid {
	return config.Debrid{Name: "Torbox", Provider: "torbox"}
}
func (f *summaryRefreshClient) Logger() zerolog.Logger      { return zerolog.Nop() }
func (f *summaryRefreshClient) RefreshDownloadLinks() error { return nil }
func (f *summaryRefreshClient) CheckFile(context.Context, string, string) error {
	return nil
}
func (f *summaryRefreshClient) AccountManager() *account.Manager { return nil }
func (f *summaryRefreshClient) GetProfile() (*debridTypes.Profile, error) {
	return nil, nil
}
func (f *summaryRefreshClient) GetAvailableSlots() (int, error) { return 0, nil }
func (f *summaryRefreshClient) SyncAccounts()                   {}
func (f *summaryRefreshClient) DeleteLink(debridTypes.DownloadLink) error {
	return nil
}
func (f *summaryRefreshClient) SpeedTest(context.Context) debridTypes.SpeedTestResult {
	return debridTypes.SpeedTestResult{}
}
func (f *summaryRefreshClient) SupportsCheck() bool { return false }

func TestTorboxSummaryRefreshHydratesOnlyNewTorrent(t *testing.T) {
	store, err := storage.NewStorage(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })

	client := &summaryRefreshClient{}
	for i := 0; i < 71; i++ {
		id := fmt.Sprintf("old-%d", i)
		hash := fmt.Sprintf("hash-old-%d", i)
		filename := fmt.Sprintf("old-%d.mkv", i)
		if err := store.AddOrUpdate(&storage.Entry{
			Protocol:       config.ProtocolTorrent,
			InfoHash:       hash,
			Name:           id,
			ActiveProvider: "Torbox",
			Status:         debridTypes.TorrentStatusDownloaded,
			Files: map[string]*storage.File{
				filename: {Name: filename, Size: 100, InfoHash: hash},
			},
			Providers: map[string]*storage.ProviderEntry{
				"Torbox": {
					Provider: "Torbox",
					ID:       id,
					Status:   debridTypes.TorrentStatusDownloaded,
					Files: map[string]*storage.ProviderFile{
						filename: {Id: "1", Link: "torbox://" + id + "/1", Path: filename},
					},
				},
			},
		}); err != nil {
			t.Fatal(err)
		}
		client.summaries = append(client.summaries, &debridTypes.Torrent{
			Id:          id,
			InfoHash:    hash,
			Name:        id,
			Debrid:      "Torbox",
			Status:      debridTypes.TorrentStatusDownloading,
			SummaryOnly: true,
		})
	}

	client.summaries = append(client.summaries, &debridTypes.Torrent{
		Id:          "new-1",
		InfoHash:    "hash-new-1",
		Name:        "new-movie",
		Debrid:      "Torbox",
		Status:      debridTypes.TorrentStatusDownloaded,
		SummaryOnly: true,
	})

	clients := xsync.NewMap[string, debrid.Client]()
	clients.Store("Torbox", client)
	mgr := &Manager{
		storage: store,
		clients: clients,
		logger:  zerolog.Nop(),
	}

	for pass := 0; pass < 2; pass++ {
		if err := mgr.doRefreshTorrents(context.Background(), "Torbox", client); err != nil {
			t.Fatalf("refresh pass %d: %v", pass+1, err)
		}
	}

	client.mu.Lock()
	updates := append([]string(nil), client.updates...)
	client.mu.Unlock()
	if len(updates) != 1 || updates[0] != "new-1" {
		t.Fatalf("detail updates = %v, want exactly [new-1]", updates)
	}

	entry, err := store.Get("hash-new-1")
	if err != nil {
		t.Fatalf("new torrent was not stored: %v", err)
	}
	if placement := entry.Providers["Torbox"]; placement == nil || placement.ID != "new-1" || len(placement.Files) != 1 {
		t.Fatalf("new placement = %#v, want hydrated Torbox placement", placement)
	}
}
