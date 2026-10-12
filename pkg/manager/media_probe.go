package manager

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/sirrobot01/decypharr/pkg/usenet/parser"
)

const (
	repairMediaProbeConcurrency = 4
	repairMediaProbeTimeout     = 60 * time.Second
	repairMediaProbeAttempts    = 2
	// mountedHeaderProbeSize covers every signature parser.MediaHeaderValid checks.
	mountedHeaderProbeSize = 16
)

type mediaProbeState uint8

const (
	mediaProbeUnknown mediaProbeState = iota
	mediaProbeHealthy
	mediaProbeBroken
)

type mediaProbeResult struct {
	state  mediaProbeState
	reason string
}

func (r *Repair) probeMountedMedia(ctx context.Context, path string) mediaProbeResult {
	if ctx == nil {
		ctx = context.Background()
	}
	slots := r.mediaProbeSlots
	if slots == nil {
		slots = make(chan struct{}, repairMediaProbeConcurrency)
	}
	select {
	case slots <- struct{}{}:
		defer func() { <-slots }()
	case <-ctx.Done():
		return mediaProbeResult{state: mediaProbeUnknown, reason: "media_probe_timeout"}
	}
	attempt := r.mediaProbeAttempt
	if attempt == nil {
		attempt = runMountedMediaProbeAttempt
	}
	var result mediaProbeResult
	for i := 0; i < repairMediaProbeAttempts; i++ {
		attemptCtx, cancel := context.WithTimeout(ctx, repairMediaProbeTimeout)
		result = attempt(attemptCtx, path)
		cancel()
		if result.state != mediaProbeBroken {
			return result
		}
	}
	return result
}

func runMountedMediaProbeAttempt(ctx context.Context, path string) mediaProbeResult {
	if err := readMountedMediaRanges(ctx, path); err != nil {
		return classifyMountedReadError(err)
	}
	if header := probeMountedHeader(ctx, path); header.state != mediaProbeHealthy {
		return header
	}
	return runFFProbe(ctx, path)
}

// probeMountedHeader checks that a mounted media file starts with its
// container's signature. A file whose articles all exist can still be
// stitched in the wrong order; ffprobe may then "find" an audio stream in the
// noise, but no player can open it. Only a completed read with the wrong
// signature is broken; read errors and timeouts are unknown.
func probeMountedHeader(ctx context.Context, path string) mediaProbeResult {
	type readResult struct {
		head []byte
		err  error
	}
	// A FUSE read can't be cancelled, so it runs on its own goroutine and the
	// probe gives up on it when ctx ends.
	done := make(chan readResult, 1)
	go func() {
		f, err := os.Open(path)
		if err != nil {
			done <- readResult{err: err}
			return
		}
		defer func() { _ = f.Close() }()
		head := make([]byte, mountedHeaderProbeSize)
		n, err := io.ReadFull(f, head)
		if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
			err = nil
		}
		done <- readResult{head: head[:n], err: err}
	}()
	select {
	case <-ctx.Done():
		return mediaProbeResult{state: mediaProbeUnknown, reason: "media_probe_timeout"}
	case res := <-done:
		if res.err != nil {
			return classifyMountedReadError(res.err)
		}
		if !parser.MediaHeaderValid(path, res.head) {
			return mediaProbeResult{state: mediaProbeBroken, reason: "media_header_invalid"}
		}
		return mediaProbeResult{state: mediaProbeHealthy}
	}
}

func readMountedMediaRanges(ctx context.Context, path string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	info, err := f.Stat()
	if err != nil {
		return err
	}
	size := info.Size()
	if size <= 0 {
		return nil
	}
	head := min(int64(cacheWarmHeadSize), size)
	if err := drainRange(ctx, f, 0, head); err != nil {
		return err
	}
	if size > int64(cacheWarmHeadSize)+int64(cacheWarmTailSize) {
		return drainRange(ctx, f, size-int64(cacheWarmTailSize), int64(cacheWarmTailSize))
	}
	return nil
}

func classifyMountedReadError(err error) mediaProbeResult {
	if err == nil {
		return mediaProbeResult{state: mediaProbeHealthy}
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) ||
		os.IsTimeout(err) || errors.Is(err, syscall.ETIMEDOUT) {
		return mediaProbeResult{state: mediaProbeUnknown, reason: "media_probe_timeout"}
	}
	if errors.Is(err, fs.ErrNotExist) {
		return mediaProbeResult{state: mediaProbeUnknown, reason: "media_probe_unavailable"}
	}
	// FUSE/backend reads, permissions, and resource exhaustion are infrastructure
	// failures. They never make an item replacement-eligible on their own.
	return mediaProbeResult{state: mediaProbeUnknown, reason: "media_probe_unavailable"}
}

type ffprobeDocument struct {
	Streams []struct {
		CodecType string `json:"codec_type"`
	} `json:"streams"`
}

func runFFProbe(ctx context.Context, path string) mediaProbeResult {
	cmd := exec.CommandContext(ctx, "/usr/bin/ffprobe",
		"-v", "error", "-probesize", "5M", "-analyzeduration", "5000000",
		"-show_entries", "stream=codec_type", "-of", "json", path)
	output, err := cmd.Output()
	if ctx.Err() != nil {
		return mediaProbeResult{state: mediaProbeUnknown, reason: "media_probe_timeout"}
	}
	if err != nil {
		var execErr *exec.Error
		if errors.As(err, &execErr) || errors.Is(err, fs.ErrNotExist) || errors.Is(err, fs.ErrPermission) {
			return mediaProbeResult{state: mediaProbeUnknown, reason: "media_probe_unavailable"}
		}
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			stderr := strings.ToLower(string(exitErr.Stderr))
			switch {
			case strings.Contains(stderr, "timed out"), strings.Contains(stderr, "timeout"):
				return mediaProbeResult{state: mediaProbeUnknown, reason: "media_probe_timeout"}
			case strings.Contains(stderr, "permission denied"),
				strings.Contains(stderr, "resource temporarily unavailable"),
				strings.Contains(stderr, "connection reset"),
				strings.Contains(stderr, "connection refused"):
				return mediaProbeResult{state: mediaProbeUnknown, reason: "media_probe_unavailable"}
			}
		}
		return mediaProbeResult{state: mediaProbeBroken, reason: "media_probe_failed"}
	}
	return classifyFFProbeOutput(output)
}

func classifyFFProbeOutput(output []byte) mediaProbeResult {
	var doc ffprobeDocument
	if err := json.Unmarshal(output, &doc); err != nil {
		return mediaProbeResult{state: mediaProbeUnknown, reason: "media_probe_unavailable"}
	}
	for _, stream := range doc.Streams {
		if strings.EqualFold(stream.CodecType, "audio") || strings.EqualFold(stream.CodecType, "video") {
			return mediaProbeResult{state: mediaProbeHealthy}
		}
	}
	return mediaProbeResult{state: mediaProbeBroken, reason: "media_no_playable_stream"}
}

func mountedMediaPath(root, entryName, filename string) (string, bool) {
	entryRoot := filepath.Join(root, EntryAllFolder, entryName)
	rel := filepath.Clean(filepath.FromSlash(filename))
	if rel == "." || filepath.IsAbs(rel) || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", false
	}
	path := filepath.Join(entryRoot, rel)
	relToRoot, err := filepath.Rel(entryRoot, path)
	if err != nil || relToRoot == ".." || strings.HasPrefix(relToRoot, ".."+string(filepath.Separator)) {
		return "", false
	}
	return path, true
}
