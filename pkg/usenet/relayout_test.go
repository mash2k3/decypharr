package usenet

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/puzpuzpuz/xsync/v4"
	"github.com/rs/zerolog"
	"github.com/sirrobot01/decypharr/internal/nntp"
	"github.com/sirrobot01/decypharr/pkg/storage"
	"github.com/sirrobot01/decypharr/pkg/usenet/fs/reader"
)

// estimatedSegments lays n articles out at slot bytes each, the way an import
// without real yEnc sizes did.
func estimatedSegments(n int, slot int64) []storage.NZBSegment {
	segs := make([]storage.NZBSegment, n)
	for i := range segs {
		segs[i] = storage.NZBSegment{
			Number: i + 1, MessageID: fmt.Sprintf("m%d", i), Bytes: slot,
			StartOffset: int64(i) * slot, EndOffset: int64(i+1)*slot - 1,
		}
	}
	return segs
}

// postedHeaders answers header fetches for one posted file of total bytes in
// parts of partSize, by message id "m<i>".
func postedHeaders(n int, partSize, total int64) map[string]*nntp.YencMetadata {
	out := make(map[string]*nntp.YencMetadata, n)
	for i := 0; i < n; i++ {
		begin := int64(i)*partSize + 1
		end := min(begin+partSize-1, total)
		out[fmt.Sprintf("m%d", i)] = &nntp.YencMetadata{Size: total, Begin: begin, End: end}
	}
	return out
}

type relayoutFixture struct {
	u       *Usenet
	fetched []string
	fixed   int
}

func newRelayoutFixture(t *testing.T, file storage.NZBFile, headers map[string]*nntp.YencMetadata, fetchErr error) *relayoutFixture {
	t.Helper()
	store := &NZBStorage{metaDir: t.TempDir(), logger: zerolog.Nop()}
	file.FileType = storage.NZBFileTypeMedia
	if err := store.AddNZB(&storage.NZB{ID: "nzb1", Name: "n", Files: []storage.NZBFile{file}}); err != nil {
		t.Fatalf("AddNZB: %v", err)
	}
	f := &relayoutFixture{}
	f.u = &Usenet{
		logger:      zerolog.Nop(),
		nzbStorage:  store,
		failedFiles: xsync.NewMap[string, error](),
		fs:          xsync.NewMap[string, *fsEntry](),
	}
	f.u.headerFetcher = func(_ context.Context, id string) (*nntp.YencMetadata, error) {
		f.fetched = append(f.fetched, id)
		if fetchErr != nil {
			return nil, fetchErr
		}
		if h, ok := headers[id]; ok {
			return h, nil
		}
		return nil, errors.New("no such header")
	}
	f.u.SetLayoutFixedHook(func(*storage.NZB) { f.fixed++ })
	return f
}

// Slots estimated at 1000 bytes for a file posted in 800-byte parts: the
// headers of the first and last article give the real layout.
func TestRelayoutFixesEstimatedLayout(t *testing.T) {
	f := newRelayoutFixture(t, storage.NZBFile{Name: "f.mkv", Size: 3000, Segments: estimatedSegments(3, 1000)},
		postedHeaders(3, 800, 2000), nil)

	if got, reason := f.u.relayoutFile(context.Background(), "nzb1", "f.mkv", 1, 800); got != relayoutFixed {
		t.Fatalf("outcome = %v (%s), want fixed", got, reason)
	}
	if len(f.fetched) != 2 {
		t.Fatalf("fetched %v, want only the first and last headers", f.fetched)
	}
	nzb, _ := f.u.nzbStorage.GetNZB("nzb1")
	file := nzb.Files[0]
	if file.Size != 2000 || nzb.TotalSize != 2000 {
		t.Fatalf("size = %d (total %d), want 2000", file.Size, nzb.TotalSize)
	}
	wantBytes := []int64{800, 800, 400}
	for i, s := range file.Segments {
		if s.Bytes != wantBytes[i] || s.StartOffset != int64(i)*800 || s.EndOffset != s.StartOffset+s.Bytes-1 {
			t.Fatalf("segment %d = %+v", i, s)
		}
	}
	if f.fixed != 1 {
		t.Fatalf("layout-fixed hook called %d times", f.fixed)
	}
	if err := f.u.IsFilePermanentlyFailed("nzb1", "f.mkv"); err != nil {
		t.Fatalf("a re-measured file must not be marked failed: %v", err)
	}
}

// The layout is already right and the short article's own header says it
// should fill its slot: the article itself is truncated. The only dead path.
func TestRelayoutDeadOnlyWithProof(t *testing.T) {
	headers := postedHeaders(3, 1000, 2500)
	segs := estimatedSegments(3, 1000)
	segs[2].Bytes, segs[2].EndOffset = 500, 2499
	f := newRelayoutFixture(t, storage.NZBFile{Name: "f.mkv", Size: 2500, Segments: segs}, headers, nil)

	if got, reason := f.u.relayoutFile(context.Background(), "nzb1", "f.mkv", 1, 600); got != relayoutDead {
		t.Fatalf("outcome = %v (%s), want dead", got, reason)
	}
	if f.fixed != 0 {
		t.Fatal("nothing should be re-saved for a correct layout")
	}

	// Same layout, but the short article's header doesn't match a uniform
	// post: unproven, so not dead.
	headers["m1"] = &nntp.YencMetadata{Size: 2500, Begin: 1001, End: 1600}
	if got, _ := f.u.relayoutFile(context.Background(), "nzb1", "f.mkv", 1, 600); got != relayoutUnfixable {
		t.Fatalf("outcome = %v, want unfixable", got)
	}
	// No mid-file segment to prove anything with (final-segment case).
	if got, _ := f.u.relayoutFile(context.Background(), "nzb1", "f.mkv", -1, 0); got != relayoutUnfixable {
		t.Fatalf("tail outcome = %v, want unfixable", got)
	}
}

func TestRelayoutNeverDeadWithoutProof(t *testing.T) {
	archive := storage.NZBFile{Name: "f.mkv", Size: 3000, InternalPath: "x.rar/f.mkv", Segments: estimatedSegments(3, 1000)}
	sliced := storage.NZBFile{Name: "f.mkv", Size: 3000, Segments: estimatedSegments(3, 1000)}
	sliced.Segments[0].SegmentDataStart = 120
	encrypted := storage.NZBFile{Name: "f.mkv", Size: 3000, IsEncrypted: true, Segments: estimatedSegments(3, 1000)}
	plain := storage.NZBFile{Name: "f.mkv", Size: 3000, Segments: estimatedSegments(3, 1000)}
	splitParts := postedHeaders(3, 800, 2000)
	splitParts["m2"] = &nntp.YencMetadata{Size: 900, Begin: 1, End: 400} // a second posted part

	cases := []struct {
		name     string
		file     storage.NZBFile
		headers  map[string]*nntp.YencMetadata
		fetchErr error
		want     relayoutOutcome
	}{
		{"archive-extracted", archive, postedHeaders(3, 800, 2000), nil, relayoutUnfixable},
		{"archive slice", sliced, postedHeaders(3, 800, 2000), nil, relayoutUnfixable},
		{"encrypted", encrypted, postedHeaders(3, 800, 2000), nil, relayoutUnfixable},
		{"split parts", plain, splitParts, nil, relayoutUnfixable},
		{"missing first article", plain, nil, &nntp.Error{Type: nntp.ErrorTypeArticleNotFound}, relayoutUnfixable},
		{"provider timeout", plain, nil, &nntp.Error{Type: nntp.ErrorTypeTimeout}, relayoutTransient},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newRelayoutFixture(t, tc.file, tc.headers, tc.fetchErr)
			if got, reason := f.u.relayoutFile(context.Background(), "nzb1", "f.mkv", 1, 600); got != tc.want {
				t.Fatalf("outcome = %v (%s), want %v", got, reason, tc.want)
			}
		})
	}
}

// What a read returns for each outcome: only dead marks the file failed.
func TestHandleLayoutMismatch(t *testing.T) {
	short := &reader.ShortSegmentError{Segment: 1, Stored: 600, Needed: 1800}
	key := fsKey("nzb1", "f.mkv")

	t.Run("unfixable is temporary and cached", func(t *testing.T) {
		archive := storage.NZBFile{Name: "f.mkv", Size: 3000, InternalPath: "x.rar/f.mkv", Segments: estimatedSegments(3, 1000)}
		f := newRelayoutFixture(t, archive, nil, nil)
		for range 3 {
			err := f.u.handleLayoutMismatch(context.Background(), key, "nzb1", "f.mkv", short)
			if !nntp.IsLayoutMismatchError(err) || nntp.IsArticleNotFoundError(err) {
				t.Fatalf("want the layout mismatch back, got %v", err)
			}
		}
		if f.u.IsFilePermanentlyFailed("nzb1", "f.mkv") != nil {
			t.Fatal("an unfixable layout must not mark the file failed")
		}
	})

	t.Run("fixed retires the cached reader", func(t *testing.T) {
		f := newRelayoutFixture(t, storage.NZBFile{Name: "f.mkv", Size: 3000, Segments: estimatedSegments(3, 1000)},
			postedHeaders(3, 800, 2000), nil)
		old := &fsEntry{}
		f.u.fs.Store(key, old)
		err := f.u.handleLayoutMismatch(context.Background(), key, "nzb1", "f.mkv", short)
		if err == nil || nntp.IsArticleNotFoundError(err) {
			t.Fatalf("want a retryable error after re-measuring, got %v", err)
		}
		if _, ok := f.u.fs.Load(key); ok {
			t.Fatal("the old-layout reader is still cached")
		}
		if !old.retired.Load() {
			t.Fatal("old entry not retired")
		}
	})

	t.Run("dead marks the file failed", func(t *testing.T) {
		segs := estimatedSegments(3, 1000)
		segs[2].Bytes, segs[2].EndOffset = 500, 2499
		f := newRelayoutFixture(t, storage.NZBFile{Name: "f.mkv", Size: 2500, Segments: segs}, postedHeaders(3, 1000, 2500), nil)
		err := f.u.handleLayoutMismatch(context.Background(), key, "nzb1", "f.mkv", short)
		if !nntp.IsArticleNotFoundError(err) || nntp.IsLayoutMismatchError(err) {
			t.Fatalf("want article-not-found (reported to repair), got %v", err)
		}
		if f.u.IsFilePermanentlyFailed("nzb1", "f.mkv") == nil {
			t.Fatal("dead file not marked failed")
		}
	})
}

// A stream still holding a retired entry closes it on its last release.
func TestRetiredEntryClosesOnLastRelease(t *testing.T) {
	u := &Usenet{fs: xsync.NewMap[string, *fsEntry]()}
	closed := false
	fe := &fsEntry{readerCleanup: func() { closed = true }, reader: fakeReader{}}
	fe.refCount.Store(1)
	u.fs.Store("k", fe)

	u.retireEntry("k")
	if closed {
		t.Fatal("closed while a stream still held it")
	}
	u.releaseEntry(fe)
	if !closed {
		t.Fatal("not closed on last release")
	}
}
