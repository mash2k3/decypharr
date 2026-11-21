package manager

import (
	"testing"

	"github.com/rs/zerolog"
	"github.com/sirrobot01/decypharr/internal/config"
	"github.com/sirrobot01/decypharr/pkg/storage"
)

// A playback read that failed without proof (connection errors, retries
// used up) must not mark the file broken: only the usenet layer's
// permanent-failure flag (missing or short articles) is proof.
func TestLiveReadFailureWithoutProofIsNotMarked(t *testing.T) {
	store, err := storage.NewStorage(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	if err := store.AddOrUpdate(&storage.Entry{
		Protocol: config.ProtocolNZB, InfoHash: "uuid-1", Name: "Movie.2020",
		Files: map[string]*storage.File{"Movie.2020.mkv": {Name: "Movie.2020.mkv", InfoHash: "uuid-1", Size: 100}},
	}); err != nil {
		t.Fatal(err)
	}
	mgr := &Manager{storage: store}
	repair := &Repair{manager: mgr, logger: zerolog.Nop()}
	mgr.repair = repair

	mgr.ReportLiveReadFailure("uuid-1", "Movie.2020", "Movie.2020.mkv", 100)

	if h, _ := store.GetEntryHealth("Movie.2020"); h != nil && len(h.BrokenFiles) > 0 {
		t.Fatalf("marked broken without proof: %+v", h.BrokenFiles)
	}
}
