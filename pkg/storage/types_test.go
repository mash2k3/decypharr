package storage

import (
	"testing"

	debridTypes "github.com/sirrobot01/decypharr/pkg/debrid/types"
)

func TestGetFirstFileReturnsOnlyActiveFiles(t *testing.T) {
	active := &File{Name: "active"}
	item := &EntryItem{Files: map[string]*File{"deleted": {Deleted: true}, "nil": nil}}
	if file, err := item.GetFirstFile(); file != nil || err == nil {
		t.Fatalf("no active files: got %v, %v", file, err)
	}
	item.Files["active"] = active
	if file, err := item.GetFirstFile(); file != active || err != nil {
		t.Fatalf("active file: got %v, %v", file, err)
	}
}

func TestProviderEntryNeedsUpdateDoesNotHydrateEmptyRemoteSummary(t *testing.T) {
	placement := &ProviderEntry{
		ID:     "17",
		Status: debridTypes.TorrentStatusDownloaded,
		Files:  map[string]*ProviderFile{},
	}
	remote := &debridTypes.Torrent{
		Id:     "17",
		Status: debridTypes.TorrentStatusDownloaded,
		Files:  map[string]debridTypes.File{},
	}

	if placement.NeedsUpdate(remote) {
		t.Fatal("empty lightweight remote summary must not requeue an unchanged placement")
	}

	remote.Files["movie.mkv"] = debridTypes.File{Name: "movie.mkv", Link: "provider://17/1"}
	if !placement.NeedsUpdate(remote) {
		t.Fatal("remote file metadata should hydrate a placement with no stored files")
	}

	remote.Files = nil
	remote.Status = debridTypes.TorrentStatusDownloading
	if !placement.NeedsUpdate(remote) {
		t.Fatal("status changes must still update lightweight remote summaries")
	}
}
