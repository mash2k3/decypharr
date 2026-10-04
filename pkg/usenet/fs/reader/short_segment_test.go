package reader

import (
	"bytes"
	"context"
	"errors"
	"testing"

	"github.com/rs/zerolog"
	"github.com/sirrobot01/decypharr/internal/nntp"
)

// storeSegment writes data into a segment slot the way doFetch does.
func storeSegment(t *testing.T, sc *SegmentCache, segIdx int, data []byte) {
	t.Helper()
	sc.invalidateForRefetch(segIdx)
	if !sc.MarkFetching(segIdx) {
		t.Fatalf("segment %d: MarkFetching failed", segIdx)
	}
	w := sc.StreamWriter(segIdx)
	if _, err := w.Write(data); err != nil {
		t.Fatalf("segment %d: write: %v", segIdx, err)
	}
	if !w.Finalize() {
		t.Fatalf("segment %d: Finalize committed nothing", segIdx)
	}
}

func newShortSegmentReader(t *testing.T, fetches *int, refetchData []byte) (*StreamingReader, *SegmentCache) {
	t.Helper()
	sf := newTestFetcher(t, 3) // 3 segments of 1000 bytes
	sc := sf.cache
	sr := &StreamingReader{
		cache:     sc,
		fetcher:   sf,
		config:    sf.config,
		totalSize: 3000,
		segCount:  3,
		logger:    zerolog.Nop(),
	}
	sr.fetchSegment = func(ctx context.Context, segIdx int) error {
		*fetches++
		storeSegment(t, sc, segIdx, refetchData)
		return nil
	}
	return sr, sc
}

// A segment whose article decodes shorter than its slot used to send every
// read into the heal loop: 20 re-downloads of the same short article per
// read. It now fails after one re-fetch with a LayoutMismatch error that says
// which segment, and never ArticleNotFound: a wrong stored layout must not
// mark the file dead (the usenet layer re-measures the layout first).
func TestShortSegmentFailsFastAsLayoutMismatch(t *testing.T) {
	fetches := 0
	short := bytes.Repeat([]byte{'b'}, 600)
	sr, sc := newShortSegmentReader(t, &fetches, short)

	storeSegment(t, sc, 0, bytes.Repeat([]byte{'a'}, 1000))
	storeSegment(t, sc, 1, short)
	storeSegment(t, sc, 2, bytes.Repeat([]byte{'c'}, 1000))

	// Bytes 1700-1799 lie past the 600 bytes segment 1 actually holds.
	p := make([]byte, 100)
	_, err := sr.readFromCache(context.Background(), p, 1700, 1, 1)
	if err == nil {
		t.Fatal("expected an error reading past the short segment's data")
	}
	if !nntp.IsLayoutMismatchError(err) {
		t.Fatalf("expected a layout mismatch, got %v", err)
	}
	if nntp.IsArticleNotFoundError(err) {
		t.Fatal("a short segment must not be reported as a missing article")
	}
	var sse *ShortSegmentError
	if !errors.As(err, &sse) || sse.Segment != 1 || sse.Stored != 600 || sse.Needed != 800 {
		t.Fatalf("ShortSegmentError = %+v", sse)
	}
	if fetches != 1 {
		t.Fatalf("expected exactly one re-fetch, got %d", fetches)
	}
}

// The final segment coming up short is what an overstated file size looks
// like. That used to play (zeros past the data); it must not fail, and the
// owner is told once so it can re-measure the layout.
func TestShortFinalSegmentIsZeroFilled(t *testing.T) {
	fetches := 0
	short := bytes.Repeat([]byte{'c'}, 600)
	sr, sc := newShortSegmentReader(t, &fetches, short)
	hooks := 0
	sr.config.OnShortTail = func() { hooks++ }
	storeSegment(t, sc, 2, short)

	p := bytes.Repeat([]byte{0xFF}, 200) // 2500-2699: 100 bytes of data, 100 past it
	n, err := sr.readFromCache(context.Background(), p, 2500, 2, 2)
	if err != nil || n != 200 {
		t.Fatalf("tail read: n=%d err=%v", n, err)
	}
	if !bytes.Equal(p[:100], short[:100]) || !bytes.Equal(p[100:], make([]byte, 100)) {
		t.Fatalf("want data then zeros, got %x...%x", p[:4], p[196:])
	}
	if _, err := sr.readFromCache(context.Background(), p, 2600, 2, 2); err != nil {
		t.Fatalf("second tail read: %v", err)
	}
	if fetches != 1 || hooks != 1 {
		t.Fatalf("want one re-fetch and one hook call, got %d and %d", fetches, hooks)
	}
}

// Without the check, a read straddling the short segment's end returned a
// partial copy with a zero-filled hole and no error.
func TestShortSegmentNoSilentGap(t *testing.T) {
	fetches := 0
	short := bytes.Repeat([]byte{'b'}, 600)
	sr, sc := newShortSegmentReader(t, &fetches, short)
	storeSegment(t, sc, 1, short)
	storeSegment(t, sc, 2, bytes.Repeat([]byte{'c'}, 1000))

	p := make([]byte, 600) // 1500-2099: tail of seg 1 (past its data) + head of seg 2
	n, err := sr.readFromCache(context.Background(), p, 1500, 1, 2)
	if err == nil {
		t.Fatalf("expected an error, got n=%d with data %q...", n, p[:20])
	}
}

func TestShortSegmentRecoversWhenRefetchIsComplete(t *testing.T) {
	fetches := 0
	full := bytes.Repeat([]byte{'b'}, 1000)
	sr, sc := newShortSegmentReader(t, &fetches, full)
	storeSegment(t, sc, 1, bytes.Repeat([]byte{'b'}, 600)) // truncated cached copy

	p := make([]byte, 100)
	n, err := sr.readFromCache(context.Background(), p, 1700, 1, 1)
	if err != nil {
		t.Fatalf("unexpected error after a complete re-fetch: %v", err)
	}
	if n != 100 || !bytes.Equal(p, full[:100]) {
		t.Fatalf("got n=%d data %q", n, p[:10])
	}
	if fetches != 1 {
		t.Fatalf("expected one re-fetch, got %d", fetches)
	}
}

func TestReadWithinShortSegmentDataStillWorks(t *testing.T) {
	fetches := 0
	short := bytes.Repeat([]byte{'b'}, 600)
	sr, sc := newShortSegmentReader(t, &fetches, short)
	storeSegment(t, sc, 1, short)

	p := make([]byte, 100)
	n, err := sr.readFromCache(context.Background(), p, 1100, 1, 1)
	if err != nil || n != 100 {
		t.Fatalf("read inside the stored data: n=%d err=%v", n, err)
	}
	if fetches != 0 {
		t.Fatalf("no re-fetch expected, got %d", fetches)
	}
}
