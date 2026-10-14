package manager

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

func TestClassifyMountedReadErrorIsUnknown(t *testing.T) {
	got := classifyMountedReadError(syscall.EIO)
	if got.state != mediaProbeUnknown || got.reason != "media_probe_unavailable" {
		t.Fatalf("EIO classification = %#v, want unknown/media_probe_unavailable", got)
	}
}

func TestClassifyFFProbeOutput(t *testing.T) {
	tests := []struct {
		name   string
		json   string
		state  mediaProbeState
		reason string
	}{
		{"video", `{"streams":[{"codec_type":"video"}]}`, mediaProbeHealthy, ""},
		{"audio", `{"streams":[{"codec_type":"audio"}]}`, mediaProbeHealthy, ""},
		{"subtitle only", `{"streams":[{"codec_type":"subtitle"}]}`, mediaProbeBroken, "media_no_playable_stream"},
		{"invalid output", `{`, mediaProbeUnknown, "media_probe_unavailable"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := classifyFFProbeOutput([]byte(tt.json))
			if got.state != tt.state || got.reason != tt.reason {
				t.Fatalf("classification = %#v, want state=%v reason=%q", got, tt.state, tt.reason)
			}
		})
	}
}

func TestMountedMediaProbeRetriesOnlyBroken(t *testing.T) {
	attempts := 0
	repair := &Repair{
		mediaProbeSlots: make(chan struct{}, 4),
		mediaProbeAttempt: func(context.Context, string) mediaProbeResult {
			attempts++
			if attempts == 1 {
				return mediaProbeResult{state: mediaProbeBroken, reason: "media_probe_failed"}
			}
			return mediaProbeResult{state: mediaProbeHealthy}
		},
	}
	if got := repair.probeMountedMedia(context.Background(), "unused"); got.state != mediaProbeHealthy || attempts != 2 {
		t.Fatalf("probe = %#v after %d attempts, want healthy after retry", got, attempts)
	}

	attempts = 0
	repair.mediaProbeAttempt = func(context.Context, string) mediaProbeResult {
		attempts++
		return classifyMountedReadError(errors.New("temporary mount failure"))
	}
	if got := repair.probeMountedMedia(context.Background(), "unused"); got.state != mediaProbeUnknown || attempts != 1 {
		t.Fatalf("unknown probe = %#v after %d attempts, want one attempt", got, attempts)
	}
}

func TestMountedMediaPathRejectsTraversal(t *testing.T) {
	if _, ok := mountedMediaPath("/mnt", "entry", "../other/file.mkv"); ok {
		t.Fatal("path traversal unexpectedly accepted")
	}
	if got, ok := mountedMediaPath("/mnt", "entry", "episode.mkv"); !ok || got != "/mnt/__all__/entry/episode.mkv" {
		t.Fatalf("mounted path = %q, %v", got, ok)
	}
}

func TestProbeMountedHeader(t *testing.T) {
	dir := t.TempDir()
	write := func(name string, data []byte) string {
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, data, 0o644); err != nil {
			t.Fatal(err)
		}
		return path
	}
	tests := []struct {
		name   string
		path   string
		state  mediaProbeState
		reason string
	}{
		{"mkv ok", write("ok.mkv", []byte{0x1A, 0x45, 0xDF, 0xA3, 0x01, 0, 0, 0}), mediaProbeHealthy, ""},
		// Real case: volumes stitched out of order; ffprobe still "found" audio.
		{"mkv garbage", write("bad.mkv", []byte{0x72, 0xaf, 0xc1, 0x0c, 0x55, 0x01, 0x02, 0x03}), mediaProbeBroken, "media_header_invalid"},
		{"mp4 ok", write("ok.mp4", append([]byte{0, 0, 0, 0x20}, []byte("ftypisom")...)), mediaProbeHealthy, ""},
		{"mp4 garbage", write("bad.mp4", []byte{0x01, 0x02, 0x03, 0x04, 0xff, 0x00, 0x13, 0x88}), mediaProbeBroken, "media_header_invalid"},
		{"unknown extension", write("x.ts", []byte{1, 2, 3, 4}), mediaProbeHealthy, ""},
		{"missing file", filepath.Join(dir, "gone.mkv"), mediaProbeUnknown, "media_probe_unavailable"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := probeMountedHeader(context.Background(), tt.path)
			if got.state != tt.state || got.reason != tt.reason {
				t.Fatalf("probeMountedHeader = %#v, want state %v reason %q", got, tt.state, tt.reason)
			}
		})
	}
}

func TestProbeMountedHeaderCancelledIsUnknown(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	path := filepath.Join(t.TempDir(), "a.mkv")
	if err := os.WriteFile(path, []byte{1, 2, 3, 4}, 0o644); err != nil {
		t.Fatal(err)
	}
	// Either the read or the cancellation may win; a cancelled probe must never
	// be reported as healthy without having read the header.
	if got := probeMountedHeader(ctx, path); got.state == mediaProbeHealthy {
		t.Fatalf("cancelled probe = %#v, want not healthy", got)
	}
}
