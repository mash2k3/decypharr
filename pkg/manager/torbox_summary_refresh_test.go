package manager

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/puzpuzpuz/xsync/v4"
	"github.com/rs/zerolog"
	"github.com/sirrobot01/decypharr/internal/config"
	"github.com/sirrobot01/decypharr/pkg/debrid/account"
	debrid "github.com/sirrobot01/decypharr/pkg/debrid/common"
	debridTypes "github.com/sirrobot01/decypharr/pkg/debrid/types"
	"github.com/sirrobot01/decypharr/pkg/storage"
)

type summaryRefreshClient struct {
	summaries   []*debridTypes.Torrent
	details     map[string]*debridTypes.Torrent
	detailNames map[string]string
	mu          sync.Mutex
	updates     []string
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
	if name := f.detailNames[t.Id]; name != "" {
		t.Name = name
		t.OriginalFilename = name
	}
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

func TestTorboxSummaryRefreshRepairsExistingHashName(t *testing.T) {
	store, err := storage.NewStorage(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })

	const (
		hash = "e3d5d8bc1f7a4eb13bd62640229c63f3edfc71e8"
		root = "www.UIndex.org    -    Strange.Harvest.2025.1080p.WEB-DL.EAC3.H264-MG"
	)
	if err := store.AddOrUpdate(&storage.Entry{
		Protocol:       config.ProtocolTorrent,
		InfoHash:       hash,
		Name:           hash,
		ActiveProvider: "Torbox",
		Status:         debridTypes.TorrentStatusDownloaded,
		Files: map[string]*storage.File{
			"movie.mkv": {Name: "movie.mkv", Size: 100, InfoHash: hash},
		},
		Providers: map[string]*storage.ProviderEntry{
			"Torbox": {
				Provider: "Torbox",
				ID:       "147236436",
				Status:   debridTypes.TorrentStatusDownloaded,
				Files: map[string]*storage.ProviderFile{
					"movie.mkv": {Id: "1", Link: "torbox://147236436/1", Path: "movie.mkv"},
				},
			},
		},
	}); err != nil {
		t.Fatal(err)
	}

	client := &summaryRefreshClient{
		detailNames: map[string]string{"147236436": root},
		summaries: []*debridTypes.Torrent{{
			Id:          "147236436",
			InfoHash:    hash,
			Name:        hash,
			Debrid:      "Torbox",
			Status:      debridTypes.TorrentStatusDownloaded,
			SummaryOnly: true,
		}},
	}
	clients := xsync.NewMap[string, debrid.Client]()
	clients.Store("Torbox", client)
	mgr := &Manager{storage: store, clients: clients, logger: zerolog.Nop()}

	if err := mgr.doRefreshTorrents(context.Background(), "Torbox", client); err != nil {
		t.Fatal(err)
	}
	entry, err := store.Get(hash)
	if err != nil {
		t.Fatal(err)
	}
	if entry.Name != root || entry.OriginalFilename != root {
		t.Fatalf("stored names = (%q, %q), want %q", entry.Name, entry.OriginalFilename, root)
	}
	client.mu.Lock()
	defer client.mu.Unlock()
	if len(client.updates) != 1 || client.updates[0] != "147236436" {
		t.Fatalf("detail updates = %v, want one hash-name repair", client.updates)
	}
}
func (f *summaryRefreshClient) GetTorrent(id string) (*debridTypes.Torrent, error) {
	t := f.details[id]
	if t == nil {
		return nil, nil
	}
	copy := *t
	return &copy, nil
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

func TestTargetedProviderSyncMountsBeforeBulkListConverges(t *testing.T) {
	store, err := storage.NewStorage(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })

	const hash = "e3d5d8bc1f7a4eb13bd62640229c63f3edfc71e8"
	client := &summaryRefreshClient{details: map[string]*debridTypes.Torrent{
		"147236436": {
			Id:       "147236436",
			InfoHash: hash,
			Name:     hash,
			Debrid:   "Torbox",
			Status:   debridTypes.TorrentStatusDownloaded,
			Files: map[string]debridTypes.File{
				"Strange.Harvest.2025.1080p.WEB-DL.EAC3.H264-MG.mkv": {
					TorrentId: "147236436", Id: "1", Name: "Strange.Harvest.2025.1080p.WEB-DL.EAC3.H264-MG.mkv", Size: 100,
				},
			},
		},
	}}
	clients := xsync.NewMap[string, debrid.Client]()
	clients.Store("Torbox", client)
	mgr := &Manager{storage: store, clients: clients, logger: zerolog.Nop()}

	entry, err := mgr.RefreshTorrentByProviderID(hash, "Torbox", "147236436")
	if err != nil {
		t.Fatal(err)
	}
	if entry == nil || entry.InfoHash != hash || entry.Providers["Torbox"].ID != "147236436" {
		t.Fatalf("targeted sync entry = %#v", entry)
	}
	if _, err := store.Get(hash); err != nil {
		t.Fatalf("targeted sync was not persisted: %v", err)
	}

	// A stale bulk-list response immediately after the targeted sync must not
	// delete the entry that was just mounted.
	_, _, deleted, err := mgr.detectTorrentChanges("Torbox", map[string]*debridTypes.Torrent{})
	if err != nil {
		t.Fatal(err)
	}
	if len(deleted) != 0 {
		t.Fatalf("recent targeted entry deleted from stale bulk list: %v", deleted)
	}

	entry.CreatedAt = time.Now().Add(-torboxDeletionGrace - time.Minute)
	if err := store.AddOrUpdate(entry); err != nil {
		t.Fatal(err)
	}
	_, _, deleted, err = mgr.detectTorrentChanges("Torbox", map[string]*debridTypes.Torrent{})
	if err != nil {
		t.Fatal(err)
	}
	if len(deleted) != 1 || deleted[0] != hash {
		t.Fatalf("expired missing entry deletions = %v, want [%s]", deleted, hash)
	}
}

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
