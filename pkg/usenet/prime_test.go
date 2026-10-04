package usenet

import (
	"context"
	"io"
	"testing"

	"github.com/puzpuzpuz/xsync/v4"
	"github.com/rs/zerolog"
	"github.com/sirrobot01/decypharr/internal/nntp"
	"github.com/sirrobot01/decypharr/pkg/storage"
)

type fakeReader struct{ err error }

func (f fakeReader) ReadAt(p []byte, off int64) (int, error) {
	return f.ReadAtContext(context.Background(), p, off)
}
func (f fakeReader) ReadAtContext(_ context.Context, p []byte, _ int64) (int, error) {
	if f.err != nil {
		return 0, f.err
	}
	p[0] = 'x'
	return 1, nil
}
func (f fakeReader) Prefetch(context.Context, int64, int64) {}

func newPrimeUsenet(t *testing.T, readErr error) *Usenet {
	t.Helper()
	store := &NZBStorage{metaDir: t.TempDir(), logger: zerolog.Nop()}
	if err := store.AddNZB(&storage.NZB{ID: "nzb1", Name: "n", Files: []storage.NZBFile{{Name: "f.mkv"}}}); err != nil {
		t.Fatalf("AddNZB: %v", err)
	}
	u := &Usenet{
		logger:      zerolog.Nop(),
		nzbStorage:  store,
		failedFiles: xsync.NewMap[string, error](),
		fs:          xsync.NewMap[string, *fsEntry](),
	}
	fe := &fsEntry{reader: fakeReader{err: readErr}, readerSize: 10}
	fe.readerOnce.Do(func() {})
	u.fs.Store(fsKey("nzb1", "f.mkv"), fe)
	return u
}

// A missing article used to surface only after 206 headers were committed,
// so WebDAV clients got a truncated body and retried. Prime reports it first.
func TestPrimeMissingArticle(t *testing.T) {
	u := newPrimeUsenet(t, &nntp.Error{Type: nntp.ErrorTypeArticleNotFound, Code: 430, Message: "No Such Article"})
	err := u.Prime(context.Background(), "nzb1", "f.mkv", 0)
	if err == nil {
		t.Fatal("expected an error for a missing article")
	}
	if u.IsFilePermanentlyFailed("nzb1", "f.mkv") == nil {
		t.Fatal("file should be flagged permanently failed")
	}
	nzb, _ := u.nzbStorage.GetNZB("nzb1")
	if nzb == nil || !nzb.Files[0].IsDeleted {
		t.Fatal("deleted status should be persisted")
	}
}

func TestPrimeHealthyAndEOF(t *testing.T) {
	if err := newPrimeUsenet(t, nil).Prime(context.Background(), "nzb1", "f.mkv", 3); err != nil {
		t.Fatalf("healthy read: %v", err)
	}
	if err := newPrimeUsenet(t, io.EOF).Prime(context.Background(), "nzb1", "f.mkv", 9); err != nil {
		t.Fatalf("EOF should not fail priming: %v", err)
	}
}
