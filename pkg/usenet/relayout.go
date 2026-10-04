package usenet

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/sirrobot01/decypharr/internal/customerror"
	"github.com/sirrobot01/decypharr/internal/nntp"
	"github.com/sirrobot01/decypharr/pkg/storage"
	"github.com/sirrobot01/decypharr/pkg/usenet/fs/reader"
)

// A segment that decodes shorter than its slot usually means the stored
// layout is wrong, not that data is missing: NZBs imported before real yEnc
// sizing gave every article of a file the group's (or an estimated) segment
// size. relayoutFile re-measures such a file from the yEnc headers of its
// first and last articles. It is deliberately conservative about calling a
// file dead, so a bad layout never floods repair:
//
//   - relayoutFixed: the headers give a different, self-consistent layout;
//     it is saved and the next read uses it.
//   - relayoutDead: the headers prove the stored layout is already right
//     AND the short article's own header says it should fill its slot, so
//     the article itself is truncated. Only this marks the file failed.
//   - relayoutUnfixable: the file can't be re-measured this way (archive-
//     extracted, split parts, encrypted, non-uniform part sizes, headers that
//     disagree). Reads keep failing, but nothing is persisted or reported.
//   - relayoutTransient: a header fetch failed; try again later.
type relayoutOutcome int

const (
	relayoutUnfixable relayoutOutcome = iota
	relayoutFixed
	relayoutDead
	relayoutTransient
)

func (o relayoutOutcome) String() string {
	switch o {
	case relayoutFixed:
		return "fixed"
	case relayoutDead:
		return "dead"
	case relayoutTransient:
		return "transient"
	default:
		return "unfixable"
	}
}

// relayoutRetryWindow: an unfixable or transient result is reused for this
// long, so repeated reads don't re-download the same headers.
const relayoutRetryWindow = 30 * time.Minute

// relayoutTimeout bounds a re-measure triggered by a read.
const relayoutTimeout = 60 * time.Second

type relayoutAttempt struct {
	at      time.Time
	outcome relayoutOutcome
}

// fetchYencHeader returns an article's yEnc header (part begin/end, total
// size). It downloads the article; callers fetch two or three at most.
func (u *Usenet) fetchYencHeader(ctx context.Context, messageID string) (*nntp.YencMetadata, error) {
	if u.headerFetcher != nil {
		return u.headerFetcher(ctx, messageID)
	}
	var meta *nntp.YencMetadata
	err := u.nntp.ExecuteWithFailover(ctx, func(conn *nntp.Connection) error {
		m, e := conn.GetHeaderPrefix(messageID, 0)
		meta = m
		return e
	})
	return meta, err
}

// relayout runs relayoutFile once per file at a time and reuses a recent
// unfixable/transient answer. shortSeg is the mid-file segment that came up
// short, or -1 when only the final segment did.
func (u *Usenet) relayout(ctx context.Context, nzoID, filename string, shortSeg int, stored int64) relayoutOutcome {
	key := fsKey(nzoID, filename)
	if v, ok := u.relayoutAttempts.Load(key); ok {
		a := v.(relayoutAttempt)
		if time.Since(a.at) < relayoutRetryWindow && (a.outcome == relayoutUnfixable || a.outcome == relayoutTransient) {
			return a.outcome
		}
	}
	v, _, _ := u.relayoutSG.Do(key, func() (any, error) {
		ctx, cancel := context.WithTimeout(ctx, relayoutTimeout)
		defer cancel()
		outcome, reason := u.relayoutFile(ctx, nzoID, filename, shortSeg, stored)
		u.relayoutAttempts.Store(key, relayoutAttempt{at: time.Now(), outcome: outcome})
		u.logger.Info().Str("nzo_id", nzoID).Str("file", filename).Int("short_segment", shortSeg).
			Str("outcome", outcome.String()).Str("reason", reason).
			Msg("Re-measured usenet file layout from yEnc headers")
		return outcome, nil
	})
	return v.(relayoutOutcome)
}

func (u *Usenet) relayoutFile(ctx context.Context, nzoID, filename string, shortSeg int, stored int64) (relayoutOutcome, string) {
	nzb, err := u.nzbStorage.GetNZB(nzoID)
	if err != nil {
		return relayoutTransient, "metadata load failed: " + err.Error()
	}
	fi := -1
	for i := range nzb.Files {
		if nzb.Files[i].Name == filename {
			fi = i // last match, the file getFile streams
		}
	}
	if fi < 0 {
		return relayoutUnfixable, "file not in NZB"
	}
	file := &nzb.Files[fi]
	if why := relayoutIneligible(file); why != "" {
		return relayoutUnfixable, why
	}
	segs := file.Segments
	n := len(segs)

	first, err := u.fetchYencHeader(ctx, segs[0].MessageID)
	if err != nil {
		return headerFetchOutcome(err), "first header: " + err.Error()
	}
	partSize := first.End - first.Begin + 1
	total := first.Size
	if first.Begin != 1 || partSize <= 0 || total <= 0 {
		return relayoutUnfixable, "first article is not the start of a posted file"
	}
	lastSize := total - partSize*int64(n-1)
	if lastSize <= 0 || lastSize > partSize {
		return relayoutUnfixable, fmt.Sprintf("%d articles of %d bytes don't make a %d-byte file", n, partSize, total)
	}
	if n > 1 {
		last, err := u.fetchYencHeader(ctx, segs[n-1].MessageID)
		if err != nil {
			return headerFetchOutcome(err), "last header: " + err.Error()
		}
		if last.Size != total || last.Begin != partSize*int64(n-1)+1 || last.End != total {
			return relayoutUnfixable, "first and last article headers disagree (split parts or uneven article sizes)"
		}
	}

	layout := uniformLayout(segs, partSize, lastSize)
	if !sameLayout(segs, layout) || file.Size != total {
		file.Segments = layout
		file.Size = total
		file.SegmentSize = partSize
		var sum int64
		for i := range nzb.Files {
			sum += nzb.Files[i].Size
		}
		nzb.TotalSize = sum
		if err := u.nzbStorage.AddNZB(nzb); err != nil {
			return relayoutTransient, "saving re-measured layout: " + err.Error()
		}
		u.retireEntry(fsKey(nzoID, filename))
		u.failedFiles.Delete(fsKey(nzoID, filename))
		if u.onLayoutFixed != nil {
			u.onLayoutFixed(nzb)
		}
		return relayoutFixed, fmt.Sprintf("%d articles of %d bytes, file %d bytes", n, partSize, total)
	}

	// The stored layout already matches the posted file. Only a mid-file
	// article whose own header says it fills its slot, yet decodes short, is
	// proof that data is gone.
	if shortSeg <= 0 || shortSeg >= n-1 {
		return relayoutUnfixable, "layout already correct; no mid-file segment to check"
	}
	h, err := u.fetchYencHeader(ctx, segs[shortSeg].MessageID)
	if err != nil {
		return headerFetchOutcome(err), "short segment header: " + err.Error()
	}
	wantBegin := partSize*int64(shortSeg) + 1
	if h.Size != total || h.Begin != wantBegin || h.End != wantBegin+partSize-1 {
		return relayoutUnfixable, "short article's header doesn't match a uniform layout"
	}
	if stored <= 0 || stored >= partSize {
		return relayoutUnfixable, "short article decodes to its full size"
	}
	return relayoutDead, fmt.Sprintf("article %d decodes to %d of %d bytes its header declares", shortSeg, stored, partSize)
}

// relayoutIneligible explains why a file can't be re-measured from its first
// and last article headers, or returns "" when it can: it must be one posted
// file read whole, article by article.
func relayoutIneligible(f *storage.NZBFile) string {
	switch {
	case len(f.Segments) == 0:
		return "no segments"
	case f.FileType != storage.NZBFileTypeMedia:
		return "not a directly posted media file"
	case f.InternalPath != "":
		return "extracted from an archive"
	case f.IsEncrypted:
		return "encrypted"
	}
	for _, s := range f.Segments {
		if s.SegmentDataStart != 0 {
			return "reads part of an article (archive slice)"
		}
	}
	return ""
}

// headerFetchOutcome: a missing article (430) can't be re-measured; anything
// else may succeed later. Neither marks the file failed here; a missing
// article is caught by the normal read path.
func headerFetchOutcome(err error) relayoutOutcome {
	if nntp.IsArticleNotFoundError(err) {
		return relayoutUnfixable
	}
	return relayoutTransient
}

func uniformLayout(segs []storage.NZBSegment, partSize, lastSize int64) []storage.NZBSegment {
	out := make([]storage.NZBSegment, len(segs))
	var off int64
	for i, s := range segs {
		size := partSize
		if i == len(segs)-1 {
			size = lastSize
		}
		s.Bytes = size
		s.SegmentDataStart = 0
		s.StartOffset = off
		s.EndOffset = off + size - 1
		off += size
		out[i] = s
	}
	return out
}

func sameLayout(a, b []storage.NZBSegment) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i].Bytes != b[i].Bytes || a[i].StartOffset != b[i].StartOffset {
			return false
		}
	}
	return true
}

// handleLayoutMismatch turns a reader's short-segment error into the error a
// read returns, re-measuring the layout first. Only relayoutDead marks the
// file failed (persisted, so repair sees it); every other outcome leaves the
// file as it is and returns a non-permanent error.
func (u *Usenet) handleLayoutMismatch(ctx context.Context, key, nzoID, filename string, readErr error) error {
	shortSeg, stored := -1, int64(0)
	var sse *reader.ShortSegmentError
	if errors.As(readErr, &sse) {
		shortSeg, stored = sse.Segment, sse.Stored
	}
	switch u.relayout(context.WithoutCancel(ctx), nzoID, filename, shortSeg, stored) {
	case relayoutFixed:
		return fmt.Errorf("layout of %s re-measured from yEnc headers; retry the read: %w", filename, readErr)
	case relayoutDead:
		dead := &nntp.Error{Type: nntp.ErrorTypeArticleNotFound, Message: readErr.Error()}
		u.failedFiles.Store(key, dead)
		u.markNZBFileDeleted(nzoID, filename)
		return customerror.NewArticleNotFoundError(dead)
	default:
		return readErr
	}
}

// relayoutTail re-measures a file whose final segment came up short. The read
// was already served (zeros past the data), so this only ever fixes the
// layout; it never marks the file failed.
func (u *Usenet) relayoutTail(nzoID, filename string) {
	go func() {
		_ = u.relayout(context.Background(), nzoID, filename, -1, 0)
	}()
}
