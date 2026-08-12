package manager

import (
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

const (
	repairMediaProbeConcurrency = 4
	repairMediaProbeTimeout     = 60 * time.Second
	repairMediaProbeAttempts    = 2
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

// probeMountedMedia validates a media file through the same mounted path used
// by Plex and other consumers. Deterministic failures are retried once with a
// fresh file handle and ffprobe process; inconclusive failures are not retried.
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
	return runFFProbe(ctx, path)
}

// readMountedMediaRanges reads the same bounded head/tail windows used by the
// cache warmer. This catches files whose metadata exists but whose FUSE reads
// immediately return EIO before ffprobe is launched.
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
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return mediaProbeResult{state: mediaProbeUnknown, reason: "media_probe_timeout"}
	}
	if os.IsTimeout(err) || errors.Is(err, syscall.ETIMEDOUT) {
		return mediaProbeResult{state: mediaProbeUnknown, reason: "media_probe_timeout"}
	}
	if errors.Is(err, fs.ErrNotExist) {
		return mediaProbeResult{state: mediaProbeBroken, reason: "mount_file_missing"}
	}
	if errors.Is(err, fs.ErrPermission) ||
		errors.Is(err, syscall.EMFILE) || errors.Is(err, syscall.ENFILE) ||
		errors.Is(err, syscall.ENOMEM) || errors.Is(err, syscall.EAGAIN) ||
		errors.Is(err, syscall.EBUSY) || errors.Is(err, syscall.EINTR) ||
		errors.Is(err, syscall.ECONNRESET) || errors.Is(err, syscall.ECONNREFUSED) ||
		errors.Is(err, syscall.ENETUNREACH) || errors.Is(err, syscall.EHOSTUNREACH) {
		return mediaProbeResult{state: mediaProbeUnknown, reason: "media_probe_unavailable"}
	}
	return mediaProbeResult{state: mediaProbeBroken, reason: "mount_read_error"}
}

type ffprobeDocument struct {
	Streams []struct {
		CodecType string `json:"codec_type"`
	} `json:"streams"`
}

func runFFProbe(ctx context.Context, path string) mediaProbeResult {
	cmd := exec.CommandContext(ctx, "/usr/bin/ffprobe",
		"-v", "error",
		"-probesize", "5M",
		"-analyzeduration", "5000000",
		"-show_entries", "stream=codec_type",
		"-of", "json",
		path,
	)
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
			case strings.Contains(stderr, "no such file"):
				return mediaProbeResult{state: mediaProbeBroken, reason: "mount_file_missing"}
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
		switch strings.ToLower(stream.CodecType) {
		case "audio", "video":
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

func isMountedMediaFailure(reason string) bool {
	switch reason {
	case "mount_file_missing", "mount_read_error", "media_probe_failed", "media_no_playable_stream":
		return true
	default:
		return false
	}
}
