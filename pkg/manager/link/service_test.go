package link

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/puzpuzpuz/xsync/v4"
	"github.com/rs/zerolog"
	"github.com/sirrobot01/decypharr/internal/config"
	"github.com/sirrobot01/decypharr/internal/customerror"
	debrid "github.com/sirrobot01/decypharr/pkg/debrid/common"
	"github.com/sirrobot01/decypharr/pkg/debrid/types"
	"github.com/sirrobot01/decypharr/pkg/storage"
)

func init() {
	// entry.GetFolder() (exercised via fetchLink's error paths) reads
	// config.Get(), which is a sync.Once singleton that os.Exit(1)s if it
	// can't create a config file at the default path. Point it at the test
	// binary's temp dir before any test triggers that first Get() call.
	config.SetConfigPath(os.TempDir())
}

// fakeEmptyLinkClient implements debrid.Client just enough to drive fetchLink
// into the downloadLink.Empty() branch; every other method panics if called,
// since this test never reaches them.
type fakeEmptyLinkClient struct {
	debrid.Client
}

func (fakeEmptyLinkClient) GetDownloadLink(torrentID string, file *types.File) (types.DownloadLink, error) {
	return types.DownloadLink{}, nil
}

// TestGetPlacementFileNilRefreshedEntry reproduces the production panic seen
// in climount.log: a season-pack torrent where RealDebrid's getSelectedFiles
// drops some hoster links, isComplete fails, and Manager.refreshTorrent (the
// wired EntryRefresher) legitimately returns (nil, nil) as documented. Before
// the fix, getPlacementFile dereferenced refreshed.Files[filename] directly,
// panicking on the nil *storage.Entry.
func TestGetPlacementFileNilRefreshedEntry(t *testing.T) {
	const infohash = "c9d010a3d74b5b901e856d281d560c2bc716d55a"
	const filename = "Sweet.Tooth.S03E01.mkv"
	const provider = "realdebrid"

	entry := &storage.Entry{
		InfoHash:       infohash,
		Name:           "Sweet Tooth S03",
		ActiveProvider: provider,
		Files: map[string]*storage.File{
			filename: {Name: filename, AddedOn: time.Now()},
		},
		Providers: map[string]*storage.ProviderEntry{
			provider: {
				Provider: provider,
				// Placement file missing/empty forces the refresher fallback path.
				Files: map[string]*storage.ProviderFile{},
			},
		},
	}

	refresherCalled := false
	refresher := func(gotInfohash string) (*storage.Entry, error) {
		refresherCalled = true
		if gotInfohash != infohash {
			t.Errorf("refresher called with infohash %q, want %q", gotInfohash, infohash)
		}
		// Mirrors Manager.refreshTorrent's documented (nil, nil) contract when
		// processSyncTorrent's isComplete check fails.
		return nil, nil
	}

	svc := New(
		xsync.NewMap[string, debrid.Client](),
		refresher,
		nil,
		func(entry *storage.Entry) error { return nil },
		nil,
		0,
		zerolog.Nop(),
	)

	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("getPlacementFile panicked on nil refreshed entry: %v", r)
		}
	}()

	_, err := svc.getPlacementFile(entry, filename)

	if !refresherCalled {
		t.Fatal("entryRefresher was never called — test didn't exercise the fallback path")
	}
	if err != customerror.HosterUnavailableError {
		t.Fatalf("got err = %v, want customerror.HosterUnavailableError", err)
	}
}

// TestFetchLinkNilRepairerOnEmptyLink reproduces the second, separate panic
// seen live in climount.log at 2026-08-02 02:52:16 (goroutine stack through
// fetchLink -> service.go:294), distinct from getPlacementFile's nil-refresh
// panic above. This fork deliberately configures Service with a nil
// EntryRepairer (see Manager.initLinkService: "CLI handles torrent
// re-insertion via the debrid repair engine"). fetchLink's own empty-link
// reinsertion branch called s.repairer(ctx, entry) directly with no nil
// check — unlike handleBadLink's identical branch a few lines away, which
// already guards s.repairer == nil. Calling a nil func value panics.
//
// This calls fetchLink directly rather than going through GetLink: GetLink's
// singleflight wrapper recovers any panic and folds it into the same
// generic "link fetch returned nil" error/Bad-entry outcome as a clean
// failure, so a panic and a proper error return are indistinguishable from
// the outside — exactly the gap that let this bug through review once
// already after only exercising GetLink.
func TestFetchLinkNilRepairerOnEmptyLink(t *testing.T) {
	const infohash = "c9d010a3d74b5b901e856d281d560c2bc716d55a"
	const filename = "Sweet.Tooth.S03E02.mkv"
	const provider = "realdebrid"

	entry := &storage.Entry{
		InfoHash:       infohash,
		Name:           "Sweet Tooth S03",
		ActiveProvider: provider,
		Files: map[string]*storage.File{
			filename: {Name: filename, AddedOn: time.Now()},
		},
		Providers: map[string]*storage.ProviderEntry{
			provider: {
				Provider: provider,
				ID:       "rd-torrent-id",
				Files: map[string]*storage.ProviderFile{
					// Non-empty placement file — this test targets the empty
					// DownloadLink branch, not the getPlacementFile refresh path.
					filename: {Link: "https://real-debrid.com/d/somefile"},
				},
			},
		},
	}

	clients := xsync.NewMap[string, debrid.Client]()
	clients.Store(provider, fakeEmptyLinkClient{})

	svc := New(
		clients,
		func(infohash string) (*storage.Entry, error) { return nil, nil },
		nil, // deliberate nil repairer, matching Manager.initLinkService
		func(entry *storage.Entry) error { return nil },
		nil,
		0,
		zerolog.Nop(),
	)

	_, err := svc.fetchLink(context.Background(), entry, filename, 0)
	if err == nil {
		t.Fatal("expected an error for entry with no repairer configured, got nil")
	}
	if !entry.Bad {
		t.Fatal("expected entry to be marked Bad after empty link with no repairer")
	}
}

type fakeDeleteLinkClient struct {
	debrid.Client
	deleted []string
}

func (f *fakeDeleteLinkClient) DeleteLink(dl types.DownloadLink) error {
	f.deleted = append(f.deleted, dl.DownloadLink)
	return nil
}

// A CDN URL that expires mid-stream must be forgotten in both places it is
// cached (validation map and account link cache), or every retry reuses it.
func TestInvalidateLinkDropsValidatedAndAccountLink(t *testing.T) {
	client := &fakeDeleteLinkClient{}
	clients := xsync.NewMap[string, debrid.Client]()
	clients.Store("torbox", client)
	svc := New(clients, nil, nil, nil, nil, 0, zerolog.Nop())

	link := types.DownloadLink{Debrid: "torbox", Link: "f", DownloadLink: "https://cdn.example/x"}
	svc.validated.Store(link.DownloadLink, nil)

	svc.InvalidateLink(link)

	if _, ok := svc.validated.Load(link.DownloadLink); ok {
		t.Error("link still marked validated")
	}
	if len(client.deleted) != 1 || client.deleted[0] != link.DownloadLink {
		t.Errorf("account link not deleted: %v", client.deleted)
	}
}
