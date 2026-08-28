package manager

import (
	"testing"
	"time"

	"github.com/sirrobot01/decypharr/internal/config"
	debridTypes "github.com/sirrobot01/decypharr/pkg/debrid/types"
	"github.com/sirrobot01/decypharr/pkg/storage"
)

func TestDeleteEntry_AlsoRemovesQueueRow(t *testing.T) {
	store, err := storage.NewStorage(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })

	hash := "abc123-queue-cleanup-test"
	entry := &storage.Entry{
		Protocol:         config.ProtocolNZB,
		InfoHash:         hash,
		Name:             "Movie.2020.1080p.WEB-DL-GROUP",
		OriginalFilename: "Movie.2020.1080p.WEB-DL-GROUP",
		State:            storage.EntryStatePausedUP,
		Status:           debridTypes.TorrentStatusDownloaded,
		IsComplete:       true,
		Progress:         1,
		AddedOn:          time.Now(),
		Files: map[string]*storage.File{
			"Movie.2020.1080p.WEB-DL-GROUP.mkv": {
				Name:     "Movie.2020.1080p.WEB-DL-GROUP.mkv",
				InfoHash: hash,
				Size:     100,
			},
		},
	}
	if err := store.AddOrUpdate(entry); err != nil {
		t.Fatal(err)
	}
	if err := store.AddQueue(entry); err != nil {
		t.Fatal(err)
	}

	mgr := &Manager{
		storage: store,
		queue:   newQueue(store, ""),
		config:  &config.Config{},
	}
	mgr.initEntryCache()
	if err := mgr.DeleteEntry(hash, false); err != nil {
		t.Fatalf("DeleteEntry failed: %v", err)
	}

	if _, err := store.Get(hash); err == nil {
		t.Fatal("storage entry should be deleted")
	}
	if _, err := store.GetQueued(hash); err == nil {
		t.Fatal("queue entry should be deleted alongside storage entry")
	}
}

func TestPurgeCompletedQueueEntriesWithoutStorage_RemovesGhosts(t *testing.T) {
	store, err := storage.NewStorage(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })

	hash := "ghost-queue-only"
	entry := &storage.Entry{
		Protocol:         config.ProtocolNZB,
		InfoHash:         hash,
		Name:             "Ghost.Job.2020-GROUP",
		OriginalFilename: "Ghost.Job.2020-GROUP",
		State:            storage.EntryStatePausedUP,
		Status:           debridTypes.TorrentStatusDownloaded,
		IsComplete:       true,
		Progress:         1,
		AddedOn:          time.Now(),
	}
	if err := store.AddQueue(entry); err != nil {
		t.Fatal(err)
	}

	mgr := &Manager{storage: store, queue: newQueue(store, "")}
	mgr.purgeCompletedQueueEntriesWithoutStorage()

	if _, err := store.GetQueued(hash); err == nil {
		t.Fatal("completed queue ghost without storage row should be purged on startup")
	}
}
