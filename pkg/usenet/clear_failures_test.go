package usenet

import (
	"testing"

	"github.com/puzpuzpuz/xsync/v4"
	"github.com/rs/zerolog"
	"github.com/sirrobot01/decypharr/pkg/storage"
)

// A manual recheck clears marks an older build persisted (a short segment
// from an estimated layout counted as a missing article), so the file is read
// again instead of answering "permanently unavailable" without a read.
func TestClearFileFailuresUndoesPersistedAndInMemoryMarks(t *testing.T) {
	store := &NZBStorage{metaDir: t.TempDir(), logger: zerolog.Nop()}
	seg := []storage.NZBSegment{{MessageID: "m"}}
	if err := store.AddNZB(&storage.NZB{ID: "nzb1", Name: "n", Files: []storage.NZBFile{
		{Name: "a.mkv", Size: 10, IsDeleted: true, Segments: seg},
		{Name: "b.mkv", Size: 10, Segments: seg},
	}}); err != nil {
		t.Fatalf("AddNZB: %v", err)
	}
	u := &Usenet{
		logger:      zerolog.Nop(),
		nzbStorage:  store,
		failedFiles: xsync.NewMap[string, error](),
		fs:          xsync.NewMap[string, *fsEntry](),
	}
	if err := u.IsFilePermanentlyFailed("nzb1", "a.mkv"); err == nil {
		t.Fatal("a.mkv should start out failed")
	}

	n, err := u.ClearFileFailures("nzb1")
	if err != nil || n != 1 {
		t.Fatalf("ClearFileFailures = %d, %v; want 1, nil", n, err)
	}
	if err := u.IsFilePermanentlyFailed("nzb1", "a.mkv"); err != nil {
		t.Fatalf("a.mkv still failed after clear: %v", err)
	}
	if _, err := u.getFile("nzb1", "a.mkv"); err != nil {
		t.Fatalf("getFile after clear: %v", err)
	}
	nzb, err := store.GetNZB("nzb1")
	if err != nil || nzb.Files[0].IsDeleted {
		t.Fatalf("IsDeleted not cleared in storage: %+v, %v", nzb, err)
	}

	// Nothing marked: nothing written, nothing counted.
	if n, err := u.ClearFileFailures("nzb1"); err != nil || n != 0 {
		t.Fatalf("second clear = %d, %v; want 0, nil", n, err)
	}
}
