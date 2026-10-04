package reader

import (
	"context"
	"fmt"
	"io"
	"sync"
	"sync/atomic"

	"github.com/rs/zerolog"
	"github.com/sirrobot01/decypharr/internal/crypto"
	"github.com/sirrobot01/decypharr/internal/nntp"
)

var decryptionBufPool = sync.Pool{}

func acquireDecryptionBuffer(size int) []byte {
	v := decryptionBufPool.Get()
	if v == nil {
		return make([]byte, size)
	}
	buf := v.([]byte)
	if cap(buf) < size {
		return make([]byte, size)
	}
	return buf[:size]
}

func releaseDecryptionBuffer(buf []byte) {
	decryptionBufPool.Put(buf)
}

// StreamingReader provides io.ReaderAt over NNTP segments with automatic
// caching, prefetching, and error recovery.
//
// Key features:
//   - Pin/Unpin pattern prevents the "chunk does not exist" race condition
//   - Disk caching with transparent re-download
//   - Request deduplication for concurrent reads of the same segment
//   - Background prefetching for smooth sequential reads
//   - AES-CBC decryption support for encrypted archives
//
// Usage:
//
//	reader, err := NewStreamingReader(ctx, client, segments, opts...)
//	defer reader.Close()
//	n, err := reader.ReadAt(buf, offset)
type StreamingReader struct {
	// Dependencies
	cache   *SegmentCache
	fetcher *SegmentFetcher
	config  Config

	// File metadata
	totalSize int64
	segCount  int

	// Encryption support
	encryption EncryptionConfig

	// Read position for io.Reader interface
	readOffset atomic.Int64

	// lastEndSeg is the final segment index of the most recent ReadAt,
	// used to detect seeks (-1 until the first read). Concurrent reads from
	// kernel readahead land within one prefetch window of each other and
	// never trip the detector; only a genuine jump does.
	lastEndSeg atomic.Int64

	// fetchSegment overrides fetcher.Fetch for the re-fetch paths (tests only).
	fetchSegment func(ctx context.Context, segIdx int) error

	// Lifecycle
	ctx    context.Context
	cancel context.CancelFunc
	closed atomic.Bool
	logger zerolog.Logger

	// Stats
	stats *ReaderStats
}

// NewStreamingReader creates a new streaming reader for NNTP segments.
func NewStreamingReader(
	ctx context.Context,
	client *nntp.Client,
	segments []SegmentMeta,
	opts ...Option,
) (*StreamingReader, error) {
	if len(segments) == 0 {
		return nil, fmt.Errorf("no segments provided")
	}
	if client == nil {
		return nil, fmt.Errorf("NNTP client is required")
	}

	// Apply configuration options
	config := DefaultConfig()
	for _, opt := range opts {
		opt(&config)
	}

	ctx, cancel := context.WithCancel(ctx)
	logger := config.Logger // Defaults to zerolog.Nop() via DefaultConfig; set via WithLogger.

	stats := &ReaderStats{}

	// Create cache
	cache, err := NewSegmentCache(ctx, segments, config, stats, logger)
	if err != nil {
		cancel()
		return nil, fmt.Errorf("create cache: %w", err)
	}

	// Create fetcher
	fetcher := NewSegmentFetcher(ctx, client, cache, config, stats, logger)

	sr := &StreamingReader{
		cache:     cache,
		fetcher:   fetcher,
		config:    config,
		totalSize: cache.TotalSize(),
		segCount:  cache.SegmentCount(),
		ctx:       ctx,
		cancel:    cancel,
		logger:    logger,
		stats:     stats,
	}
	sr.lastEndSeg.Store(-1)

	return sr, nil
}

// seekAbandonedWindow reports whether a read landing on [startSeg, endSeg]
// after a previous read that ended at prevEnd constitutes a seek that
// abandons the queued read-ahead window. Reads within one prefetch window of
// the previous position (in either direction) are sequential-ish — kernel
// readahead reorders and overlaps small reads constantly — and keep their
// queued hints. Anything further is a jump whose pending hints are dead
// weight.
func seekAbandonedWindow(prevEnd int64, startSeg, endSeg, ahead int) bool {
	if prevEnd < 0 || ahead <= 0 {
		return false
	}
	return int64(startSeg) > prevEnd+int64(ahead) || int64(endSeg) < prevEnd-int64(ahead)
}

// NewStreamingReaderWithEncryption creates an encrypted reader.
func NewStreamingReaderWithEncryption(
	ctx context.Context,
	client *nntp.Client,
	segments []SegmentMeta,
	encConfig EncryptionConfig,
	opts ...Option,
) (*StreamingReader, error) {
	sr, err := NewStreamingReader(ctx, client, segments, opts...)
	if err != nil {
		return nil, err
	}
	sr.encryption = encConfig
	return sr, nil
}

// ReadAt implements io.ReaderAt with blocking semantics.
// Blocks until the requested byte range is available.
//
// THE CRITICAL PATH: Uses Pin/Unpin to prevent the race condition.
func (sr *StreamingReader) ReadAt(p []byte, off int64) (int, error) {
	return sr.ReadAtContext(context.Background(), p, off)
}

// ReadAtContext implements context-aware random reads. The context is carried
// into segment waits and NNTP fetches so DFS/FUSE read timeouts can cancel the
// underlying work instead of waiting for the reader lifetime to end.
func (sr *StreamingReader) ReadAtContext(ctx context.Context, p []byte, off int64) (int, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if sr.closed.Load() {
		return 0, io.ErrClosedPipe
	}

	if len(p) == 0 {
		return 0, nil
	}

	if off >= sr.totalSize {
		return 0, io.EOF
	}
	if err := ctx.Err(); err != nil {
		return 0, err
	}

	sr.stats.Reads.Add(1)

	if sr.encryption.Enabled {
		return sr.readAtEncrypted(ctx, p, off)
	}
	return sr.readAtPlain(ctx, p, off)
}

// readAtPlain handles non-encrypted reads.
func (sr *StreamingReader) readAtPlain(ctx context.Context, p []byte, off int64) (int, error) {
	// Clamp to file bounds
	readLen := int64(len(p))
	eofAfter := false
	if off+readLen > sr.totalSize {
		readLen = sr.totalSize - off
		eofAfter = true
	}

	// Determine which segments we need
	startSeg, endSeg := sr.cache.SegmentsForRange(off, readLen)

	// CRITICAL: Pin segments to prevent eviction during read
	sr.cache.PinRange(startSeg, endSeg)
	defer sr.cache.UnpinRange(startSeg, endSeg)

	// On a seek, drop the read-ahead hints queued for the old position
	// BEFORE queueing the new window — otherwise the prefetch workers spend
	// the next seconds downloading up to a full window of data the player
	// jumped away from while this read waits for connection slots.
	prevEnd := sr.lastEndSeg.Swap(int64(endSeg))
	if seekAbandonedWindow(prevEnd, startSeg, endSeg, sr.config.PrefetchAhead) {
		sr.fetcher.CancelPendingPrefetch()
	}

	// Queue prefetch for read-ahead (non-blocking)
	prefetchEnd := min(endSeg+sr.config.PrefetchAhead, sr.segCount-1)
	if prefetchEnd > endSeg {
		sr.fetcher.QueuePrefetchRange(endSeg+1, prefetchEnd)
	}

	// Ensure all required segments are available (may block for downloads)
	if err := sr.fetcher.EnsureSegments(ctx, startSeg, endSeg); err != nil {
		sr.stats.ReadErrors.Add(1)
		return 0, err
	}

	// Read data from cache
	n, err := sr.readFromCache(ctx, p[:readLen], off, startSeg, endSeg)
	if err != nil {
		sr.stats.ReadErrors.Add(1)
		return n, err
	}

	sr.stats.BytesRead.Add(int64(n))

	// Tell the cache what we actually delivered so its sliding-window
	// sweeper can advance the back-window cutoff. Skip on zero-byte reads
	// (probe, short EOF) to avoid moving the high-water mark spuriously.
	if n > 0 {
		sr.cache.MarkConsumed(off, int64(n))
	}

	if eofAfter && int64(n) == readLen {
		return n, io.EOF
	}
	return n, err
}

// readFromCache reads data from the cache, handling segment boundaries.
//
// Uses ReadRangeInto so each pread fetches only the bytes the caller actually
// needs from that segment — no scratch buffer, no read amplification.
// Previously the code read entire segments (~750 KB) even for 4 KB reads,
// which filled the kernel page cache with mostly-unused data and caused
// progressive performance degradation on large files.
func (sr *StreamingReader) readFromCache(ctx context.Context, p []byte, off int64, startSeg, endSeg int) (int, error) {
	totalRead := 0
	// filled is the absolute offset this read has delivered through. Segments
	// must cover disjoint, ascending ranges; see the overlap check below.
	filled := off

	for segIdx := startSeg; segIdx <= endSeg; segIdx++ {
		// Wait for segment to be ready
		if err := sr.cache.WaitForSegment(ctx, segIdx); err != nil {
			return totalRead, err
		}

		// Calculate the intersection of the caller's range with this segment.
		segStart := sr.cache.SegmentOffset(segIdx)
		segEnd := sr.cache.SegmentOffset(segIdx + 1)

		readStart := max(off, segStart)
		readEnd := min(off+int64(len(p)), segEnd)

		if readStart >= readEnd {
			continue
		}
		if readStart < filled {
			// Two segments claim the same bytes. Their copies would land on
			// top of each other in p while totalRead counted both, so the
			// read would report more bytes than p can hold and panic the
			// caller's copy loop. computeOffsets rejects such tables up
			// front; anything reaching here is a bug, not bad metadata.
			return totalRead, fmt.Errorf("segment %d starts at %d, behind delivered offset %d", segIdx, readStart, filled)
		}

		outOffset := readStart - off
		segDataOffset := readStart - segStart
		copyLen := readEnd - readStart

		// A segment that decoded shorter than its slot can't supply these bytes.
		// ReadRangeInto would otherwise return a partial copy (leaving a zero
		// gap in p) or fail into the heal loop, which re-downloads the same
		// short article 20 times per read while the caller (Plex, rclone)
		// waits, and then fails without marking the file broken.
		if _, short := sr.cache.shortSegment(segIdx, segDataOffset, copyLen); short {
			if err := sr.refetchShortSegment(ctx, segIdx, segDataOffset, copyLen); err != nil {
				return totalRead, err
			}
		}

		// Read only the needed slice directly into the output buffer.
		// No intermediate scratch buffer — zero extra allocation, zero amplification.
		n, ok := sr.cache.ReadRangeInto(segIdx, segDataOffset, copyLen, p[outOffset:outOffset+copyLen])
		if !ok {
			var err error
			n, err = sr.healAndReadSegment(ctx, segIdx, segDataOffset, copyLen, p[outOffset:outOffset+copyLen])
			if err != nil {
				return totalRead, err
			}
		}

		totalRead += n
		filled = readEnd
	}

	return totalRead, nil
}

func (sr *StreamingReader) refetch(ctx context.Context, segIdx int) error {
	if sr.fetchSegment != nil {
		return sr.fetchSegment(ctx, segIdx)
	}
	return sr.fetcher.Fetch(ctx, segIdx)
}

// refetchShortSegment re-downloads a segment once, in case the cached copy was
// truncated, and returns a permanent article-not-found error if it is still
// shorter than the read needs. That error makes Usenet.Stream mark the file
// failed, so later reads fail at once instead of repeating the download.
func (sr *StreamingReader) refetchShortSegment(ctx context.Context, segIdx int, segDataOffset, copyLen int64) error {
	sr.cache.invalidateForRefetch(segIdx)
	if err := sr.refetch(ctx, segIdx); err != nil {
		return fmt.Errorf("re-fetch segment %d: %w", segIdx, err)
	}
	if stored, short := sr.cache.shortSegment(segIdx, segDataOffset, copyLen); short {
		sr.logger.Warn().Int("segment", segIdx).Int64("stored", stored).
			Int64("needed", segDataOffset+copyLen).
			Msg("segment decoded shorter than the file layout expects; data unavailable")
		return &nntp.Error{
			Type: nntp.ErrorTypeArticleNotFound,
			Message: fmt.Sprintf("segment %d decoded to %d bytes, read needs %d",
				segIdx, stored, segDataOffset+copyLen),
		}
	}
	return nil
}

// maxSegmentHealAttempts bounds healAndReadSegment's retry loop. Each
// iteration performs one Fetch call (which itself waits out any in-progress
// eviction or in-flight fetch before returning), so this is not a busy loop;
// the cap exists only to fail with a clear error instead of looping forever
// if the cache is stuck in an unexpected state-transition pattern.
const maxSegmentHealAttempts = 20

// healAndReadSegment is the self-heal path for a segment whose slot says
// OnDisk but whose bytes are unreadable (ReadRangeInto returned !ok).
//
// Multiple concurrent readers (the SegmentCache is shared across every
// ReadAt on this file) can observe the same stale !ok at once. Only the
// reader that wins invalidateForRefetch's CAS (StateOnDisk -> StateEmpty)
// forces the stale slot to re-download; a loser's CAS fails harmlessly
// because the state has already moved on — to StateEmpty (winner got there
// first), StateEvicting (a concurrent evictor claimed it instead), or
// already back to StateOnDisk (winner's fetch already landed).
//
// A loser must not itself call invalidateForRefetch again once the winner's
// fetch completes: that would discard freshly-fetched good data and
// re-fetch for nothing. But it also cannot just call WaitForSegment once and
// assume success: WaitForSegment only returns on StateOnDisk/StateFailed,
// so if it wakes into StateEmpty (e.g. an evictor's OnDisk->Evicting->Empty
// transition ran instead of a fetch) nothing will ever move the segment
// forward and the loser would block until the caller's context deadline —
// exactly the wedge this whole mechanism exists to close.
//
// Fetch itself already waits out StateEvicting and no-ops on StateOnDisk, and
// its own in-flight map dedups concurrent StateFetching callers for free, so
// calling Fetch again after the initial CAS is both correct and sufficient
// for every state the segment could be in — the retry loop just needs to
// re-check the actual bytes after each Fetch, since Fetch reports success
// for "someone's fetch completed," not specifically "the segment we care
// about is now readable."
func (sr *StreamingReader) healAndReadSegment(ctx context.Context, segIdx int, segDataOffset, copyLen int64, dst []byte) (int, error) {
	if sr.cache.invalidateForRefetch(segIdx) {
		sr.logger.Warn().Int("segment", segIdx).Msg("segment data missing after wait, re-fetching")
	}
	for attempt := 0; attempt < maxSegmentHealAttempts; attempt++ {
		if err := sr.refetch(ctx, segIdx); err != nil {
			return 0, fmt.Errorf("re-fetch segment %d: %w", segIdx, err)
		}
		if _, short := sr.cache.shortSegment(segIdx, segDataOffset, copyLen); short {
			if err := sr.refetchShortSegment(ctx, segIdx, segDataOffset, copyLen); err != nil {
				return 0, err
			}
		}
		if n, ok := sr.cache.ReadRangeInto(segIdx, segDataOffset, copyLen, dst); ok {
			return n, nil
		}
	}
	return 0, fmt.Errorf("segment %d still missing after %d heal attempts", segIdx, maxSegmentHealAttempts)
}

// readAtEncrypted handles AES-CBC encrypted reads.
func (sr *StreamingReader) readAtEncrypted(ctx context.Context, p []byte, off int64) (int, error) {
	// AES-CBC requires block-aligned reads
	alignedStart := (off / crypto.BlockSize) * crypto.BlockSize

	// Determine actual end
	reqEnd := off + int64(len(p))
	if reqEnd > sr.totalSize {
		reqEnd = sr.totalSize
	}

	// Round up to next block
	alignedEnd := reqEnd
	if remainder := alignedEnd % crypto.BlockSize; remainder != 0 {
		alignedEnd += crypto.BlockSize - remainder
	}

	bufLen := alignedEnd - alignedStart
	buf := acquireDecryptionBuffer(int(bufLen))
	defer releaseDecryptionBuffer(buf)

	// Read aligned data
	n, err := sr.readAtPlain(ctx, buf, alignedStart)
	if n > 0 && len(sr.encryption.Key) > 0 {
		// Handle partial last block
		decryptedLen := int64(n)
		if remainder := decryptedLen % crypto.BlockSize; remainder != 0 {
			// Pad with zeros for decryption
			if int64(len(buf)) >= decryptedLen+(crypto.BlockSize-remainder) {
				for i := int64(0); i < crypto.BlockSize-remainder; i++ {
					buf[decryptedLen+i] = 0
				}
				decryptedLen += crypto.BlockSize - remainder
			}
		}

		// Decrypt
		if decryptErr := sr.decryptInPlace(ctx, buf[:decryptedLen], alignedStart); decryptErr != nil {
			return 0, fmt.Errorf("decrypt: %w", decryptErr)
		}
	}

	if err != nil && err != io.EOF {
		return 0, err
	}

	// Copy requested data to p
	validDataEnd := int64(n)
	if validDataEnd > reqEnd-alignedStart {
		validDataEnd = reqEnd - alignedStart
	}

	startOffset := off - alignedStart
	if startOffset >= validDataEnd {
		return 0, err
	}

	copied := copy(p, buf[startOffset:validDataEnd])

	if err == io.EOF && copied == len(p) {
		return copied, nil
	}

	return copied, err
}

// decryptInPlace decrypts data using AES-256-CBC.
func (sr *StreamingReader) decryptInPlace(ctx context.Context, data []byte, offset int64) error {
	if len(data) == 0 {
		return nil
	}

	if len(data)%crypto.BlockSize != 0 {
		return nil // Not block-aligned, can't decrypt
	}

	iv, err := sr.computeIVForOffset(ctx, offset)
	if err != nil {
		return err
	}

	return crypto.DecryptBlock(data, sr.encryption.Key, iv)
}

// computeIVForOffset computes the IV for decrypting at a given offset.
func (sr *StreamingReader) computeIVForOffset(ctx context.Context, offset int64) ([]byte, error) {
	blockOffset := (offset / crypto.BlockSize) * crypto.BlockSize

	if blockOffset == 0 {
		// First block uses original IV
		iv := make([]byte, crypto.BlockSize)
		copy(iv, sr.encryption.IV)
		return iv, nil
	}

	// For other blocks, IV is the previous ciphertext block
	prevBlockOffset := blockOffset - crypto.BlockSize
	iv := make([]byte, crypto.BlockSize)

	// Read previous block (raw, not decrypted)
	n, err := sr.readAtPlain(ctx, iv, prevBlockOffset)
	if err != nil && err != io.EOF {
		return nil, fmt.Errorf("read IV block at %d: %w", prevBlockOffset, err)
	}
	if n < crypto.BlockSize {
		return nil, fmt.Errorf("short read for IV: got %d, need %d", n, crypto.BlockSize)
	}

	return iv, nil
}

// Prefetch triggers segment downloads for the given byte range without blocking.
func (sr *StreamingReader) Prefetch(ctx context.Context, off, length int64) {
	if sr.closed.Load() {
		return
	}
	if ctx != nil && ctx.Err() != nil {
		return
	}
	if off >= sr.totalSize {
		return
	}

	// Clamp to file bounds
	if off+length > sr.totalSize {
		length = sr.totalSize - off
	}

	startSeg, endSeg := sr.cache.SegmentsForRange(off, length)
	sr.fetcher.QueuePrefetchRange(startSeg, endSeg)
}

// Read implements io.Reader using ReadAt with tracked position.
func (sr *StreamingReader) Read(p []byte) (int, error) {
	if sr.closed.Load() {
		return 0, io.ErrClosedPipe
	}
	if len(p) == 0 {
		return 0, nil
	}

	offset := sr.readOffset.Load()
	if offset >= sr.totalSize {
		return 0, io.EOF
	}

	n, err := sr.ReadAt(p, offset)
	if n > 0 {
		sr.readOffset.Add(int64(n))
	}
	return n, err
}

// Seek implements io.Seeker.
func (sr *StreamingReader) Seek(offset int64, whence int) (int64, error) {
	if sr.closed.Load() {
		return 0, io.ErrClosedPipe
	}

	var newPos int64
	switch whence {
	case io.SeekStart:
		newPos = offset
	case io.SeekCurrent:
		newPos = sr.readOffset.Load() + offset
	case io.SeekEnd:
		newPos = sr.totalSize + offset
	default:
		return 0, fmt.Errorf("invalid seek whence %d", whence)
	}

	if newPos < 0 {
		return 0, fmt.Errorf("seek before beginning")
	}
	if newPos > sr.totalSize {
		newPos = sr.totalSize
	}

	sr.readOffset.Store(newPos)
	return newPos, nil
}

// Size returns the total size of the virtual file.
func (sr *StreamingReader) Size() int64 {
	return sr.totalSize
}

// Stats returns a snapshot of current statistics.
func (sr *StreamingReader) Stats() map[string]int64 {
	return sr.stats.Snapshot()
}

// Close releases all resources.
func (sr *StreamingReader) Close() error {
	if sr.closed.Swap(true) {
		return nil
	}

	sr.cancel()

	// Close fetcher first (stops downloads)
	sr.fetcher.Close()

	// Then close cache (cleans up files)
	return sr.cache.Close()
}

// Pool manages a pool of readers for efficient resource sharing.
// This is useful when multiple files need to be read concurrently.
type Pool struct {
	client *nntp.Client
	config Config

	readers sync.Map // map[string]*StreamingReader
}

// GetReader returns a reader for the given segments, creating one if needed.
func (rp *Pool) GetReader(
	ctx context.Context,
	key string,
	segments []SegmentMeta,
	encryption EncryptionConfig,
) (*StreamingReader, error) {
	// Check if reader exists
	if v, ok := rp.readers.Load(key); ok {
		return v.(*StreamingReader), nil
	}

	// Create new reader
	var reader *StreamingReader
	var err error
	if encryption.Enabled {
		reader, err = NewStreamingReaderWithEncryption(
			ctx, rp.client, segments, encryption,
			WithMaxDisk(rp.config.MaxDisk),
			WithMaxConnections(rp.config.MaxConnections),
			WithPrefetchAhead(rp.config.PrefetchAhead),
		)
	} else {
		reader, err = NewStreamingReader(
			ctx, rp.client, segments,
			WithMaxDisk(rp.config.MaxDisk),
			WithMaxConnections(rp.config.MaxConnections),
			WithPrefetchAhead(rp.config.PrefetchAhead),
		)
	}
	if err != nil {
		return nil, err
	}

	// Store (race-safe: LoadOrStore)
	actual, loaded := rp.readers.LoadOrStore(key, reader)
	if loaded {
		// Another goroutine beat us, close ours
		_ = reader.Close()
		return actual.(*StreamingReader), nil
	}

	return reader, nil
}

// RemoveReader closes and removes a reader from the pool.
func (rp *Pool) RemoveReader(key string) {
	if v, ok := rp.readers.LoadAndDelete(key); ok {
		_ = v.(*StreamingReader).Close()
	}
}

// Close closes all readers in the pool.
func (rp *Pool) Close() {
	rp.readers.Range(func(key, value any) bool {
		_ = value.(*StreamingReader).Close()
		rp.readers.Delete(key)
		return true
	})
}
