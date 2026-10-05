package manager

import (
	"testing"

	"github.com/puzpuzpuz/xsync/v4"
	"github.com/sirrobot01/decypharr/internal/config"
	debrid "github.com/sirrobot01/decypharr/pkg/debrid/common"
	"github.com/sirrobot01/decypharr/pkg/storage"
)

func childNames(children []FileInfo) map[string]bool {
	names := make(map[string]bool, len(children))
	for _, c := range children {
		names[c.Name()] = true
	}
	return names
}

// TestEntryCacheRefresh_UpdatesPerProviderFolders is a regression test: content
// added after a per-provider folder (e.g. "RealDebrid") was first listed showed up
// in __all__ but never in the provider folder until restart, because Refresh only
// cleared the fixed group names and custom folders.
func TestEntryCacheRefresh_UpdatesPerProviderFolders(t *testing.T) {
	store, err := storage.NewStorage(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })

	add := func(hash, name string) {
		t.Helper()
		if err := store.AddOrUpdate(&storage.Entry{
			Protocol: config.ProtocolTorrent, InfoHash: hash, Name: name, ActiveProvider: "RealDebrid",
			Files: map[string]*storage.File{name + ".mkv": {Name: name + ".mkv", InfoHash: hash, Size: 100}},
		}); err != nil {
			t.Fatal(err)
		}
	}

	clients := xsync.NewMap[string, debrid.Client]()
	clients.Store("RealDebrid", nil)
	mgr := &Manager{storage: store, config: &config.Config{}, clients: clients}
	mgr.initEntryCache()

	add("hash-1", "Old.Show.S01")
	if _, children := mgr.GetEntryChildren("RealDebrid"); !childNames(children)["Old.Show.S01"] {
		t.Fatalf("setup: provider folder should list the first entry, got %v", childNames(children))
	}

	// A later sync (e.g. a torrent added on the provider via DMM) adds an entry and refreshes.
	add("hash-2", "New.Show.S01")
	mgr.RefreshEntries(false)

	for _, group := range []string{EntryAllFolder, "RealDebrid"} {
		_, children := mgr.GetEntryChildren(group)
		if !childNames(children)["New.Show.S01"] {
			t.Errorf("%s should list the newly synced entry after Refresh, got %v", group, childNames(children))
		}
	}
}
