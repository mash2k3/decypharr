package usenet

import (
	"testing"

	"github.com/puzpuzpuz/xsync/v4"
	"github.com/rs/zerolog"
	"github.com/sirrobot01/decypharr/pkg/storage"
)

// NZBs imported before names were made unique can hold two files with one
// name. getFile streams the last one, so the failure check and the multi-file
// lookup must judge that same file, not whichever duplicate comes first.
func TestDuplicateNamesResolveToLastFile(t *testing.T) {
	store := &NZBStorage{metaDir: t.TempDir(), logger: zerolog.Nop()}
	seg := []storage.NZBSegment{{MessageID: "m"}}
	if err := store.AddNZB(&storage.NZB{ID: "nzb1", Name: "n", Files: []storage.NZBFile{
		{Name: "x.avi", Size: 15, IsDeleted: true, Segments: seg},
		{Name: "x.avi", Size: 650, Segments: seg},
	}}); err != nil {
		t.Fatalf("AddNZB: %v", err)
	}
	u := &Usenet{logger: zerolog.Nop(), nzbStorage: store, failedFiles: xsync.NewMap[string, error]()}

	if err := u.IsFilePermanentlyFailed("nzb1", "x.avi"); err != nil {
		t.Fatalf("streamed (last) file is healthy, got %v", err)
	}
	files, err := u.getFiles("nzb1", []string{"x.avi"})
	if err != nil || files["x.avi"] == nil || files["x.avi"].Size != 650 {
		t.Fatalf("getFiles resolved %+v, %v; want the 650-byte last file", files["x.avi"], err)
	}
	f, err := u.getFile("nzb1", "x.avi")
	if err != nil || f.Size != 650 {
		t.Fatalf("getFile resolved %+v, %v", f, err)
	}

	// Once the streamed file is marked deleted, all three agree it's gone.
	u.markNZBFileDeleted("nzb1", "x.avi")
	if err := u.IsFilePermanentlyFailed("nzb1", "x.avi"); err == nil {
		t.Fatal("want permanently failed after the streamed file is marked deleted")
	}
	if files, _ := u.getFiles("nzb1", []string{"x.avi"}); files["x.avi"] != nil {
		t.Fatal("getFiles fell back to the other duplicate")
	}
}
