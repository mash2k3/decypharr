package manager

import (
	"context"
	"errors"
	"path/filepath"
	"syscall"
	"testing"

	"github.com/sirrobot01/decypharr/internal/config"
	"github.com/sirrobot01/decypharr/pkg/storage"
)

func TestProbeMountedMediaRetriesDeterministicFailures(t *testing.T) {
	tests := []struct {
		name         string
		results      []mediaProbeResult
		wantState    mediaProbeState
		wantReason   string
		wantAttempts int
	}{
		{
			name: "repeatable EIO becomes broken",
			results: []mediaProbeResult{
				{state: mediaProbeBroken, reason: "mount_read_error"},
				{state: mediaProbeBroken, reason: "mount_read_error"},
			},
			wantState: mediaProbeBroken, wantReason: "mount_read_error", wantAttempts: 2,
		},
		{
			name: "successful retry becomes healthy",
			results: []mediaProbeResult{
				{state: mediaProbeBroken, reason: "media_probe_failed"},
				{state: mediaProbeHealthy},
			},
			wantState: mediaProbeHealthy, wantAttempts: 2,
		},
		{
			name: "timeout remains unknown without retry",
			results: []mediaProbeResult{
				{state: mediaProbeUnknown, reason: "media_probe_timeout"},
			},
			wantState: mediaProbeUnknown, wantReason: "media_probe_timeout", wantAttempts: 1,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			calls := 0
			r := &Repair{
				mediaProbeSlots: make(chan struct{}, repairMediaProbeConcurrency),
				mediaProbeAttempt: func(context.Context, string) mediaProbeResult {
					result := tt.results[calls]
					calls++
					return result
				},
			}
			got := r.probeMountedMedia(context.Background(), "/mount/video.mkv")
			if got.state != tt.wantState || got.reason != tt.wantReason {
				t.Fatalf("probe result = %#v, want state=%v reason=%q", got, tt.wantState, tt.wantReason)
			}
			if calls != tt.wantAttempts {
				t.Fatalf("attempts = %d, want %d", calls, tt.wantAttempts)
			}
		})
	}
}

func TestClassifyMountedReadError(t *testing.T) {
	tests := []struct {
		name       string
		err        error
		wantState  mediaProbeState
		wantReason string
	}{
		{name: "EIO", err: syscall.EIO, wantState: mediaProbeBroken, wantReason: "mount_read_error"},
		{name: "missing", err: syscall.ENOENT, wantState: mediaProbeBroken, wantReason: "mount_file_missing"},
		{name: "permission", err: syscall.EACCES, wantState: mediaProbeUnknown, wantReason: "media_probe_unavailable"},
		{name: "timeout", err: context.DeadlineExceeded, wantState: mediaProbeUnknown, wantReason: "media_probe_timeout"},
		{name: "filesystem timeout", err: syscall.ETIMEDOUT, wantState: mediaProbeUnknown, wantReason: "media_probe_timeout"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := classifyMountedReadError(tt.err)
			if got.state != tt.wantState || got.reason != tt.wantReason {
				t.Fatalf("result = %#v, want state=%v reason=%q", got, tt.wantState, tt.wantReason)
			}
		})
	}
}

func TestClassifyFFProbeOutput(t *testing.T) {
	for _, codecType := range []string{"video", "audio"} {
		t.Run(codecType, func(t *testing.T) {
			got := classifyFFProbeOutput([]byte(`{"streams":[{"codec_type":"` + codecType + `"}]}`))
			if got.state != mediaProbeHealthy {
				t.Fatalf("result = %#v, want healthy", got)
			}
		})
	}
	if got := classifyFFProbeOutput([]byte(`{"streams":[]}`)); got.state != mediaProbeBroken || got.reason != "media_no_playable_stream" {
		t.Fatalf("no-stream result = %#v", got)
	}
	if got := classifyFFProbeOutput([]byte(`not-json`)); got.state != mediaProbeUnknown {
		t.Fatalf("invalid-output result = %#v, want unknown", got)
	}
}

func TestProbeMountedFileAppliesToTorrentAndNZB(t *testing.T) {
	for _, protocol := range []config.Protocol{config.ProtocolTorrent, config.ProtocolNZB} {
		t.Run(string(protocol), func(t *testing.T) {
			var probedPath string
			r := &Repair{
				manager:         &Manager{config: &config.Config{Mount: config.Mount{MountPath: "/media"}}},
				mediaProbeSlots: make(chan struct{}, repairMediaProbeConcurrency),
				mediaProbeAttempt: func(_ context.Context, path string) mediaProbeResult {
					probedPath = path
					return mediaProbeResult{state: mediaProbeBroken, reason: "mount_read_error"}
				},
			}
			got := r.probeMountedFileIfMedia(context.Background(), "Shameless Season 7", "Shameless.S07E07.mkv", fileResult{healthy: true, protocol: protocol})
			if !got.broken || got.reason != "mount_read_error" {
				t.Fatalf("result = %#v, want broken mount_read_error", got)
			}
			if filepath.Base(probedPath) != "Shameless.S07E07.mkv" {
				t.Fatalf("probed path = %q", probedPath)
			}
		})
	}
}

func TestNonMediaSkipsMountedProbe(t *testing.T) {
	r := &Repair{mediaProbeAttempt: func(context.Context, string) mediaProbeResult {
		t.Fatal("mounted probe called for non-media file")
		return mediaProbeResult{}
	}}
	want := fileResult{healthy: true, reason: "provider_ok"}
	got := r.probeMountedFileIfMedia(context.Background(), "entry", "release.nfo", want)
	if got != want {
		t.Fatalf("result = %#v, want %#v", got, want)
	}
}

func TestMountedMediaPathRejectsTraversal(t *testing.T) {
	if _, ok := mountedMediaPath("/mount", "entry", "../outside.mkv"); ok {
		t.Fatal("path traversal was accepted")
	}
	want := filepath.Join("/mount", EntryAllFolder, "entry", "season", "episode.mkv")
	if got, ok := mountedMediaPath("/mount", "entry", "season/episode.mkv"); !ok || got != want {
		t.Fatalf("path = %q, ok=%v, want %q", got, ok, want)
	}
}

func TestBrokenFilesIncludesCliDebridID(t *testing.T) {
	r := &Repair{}
	c := &candidate{
		name: "Shameless Season 7",
		item: &storage.EntryItem{Files: map[string]*storage.File{
			"Shameless.S07E07.mkv": {Name: "Shameless.S07E07.mkv", Size: 1234},
		}},
	}
	files := r.brokenFiles(c, []fileResult{{
		name: "Shameless.S07E07.mkv", cliDebridID: 36810, broken: true, reason: "mount_read_error",
	}})
	if len(files) != 1 || files[0].CliDebridID != 36810 {
		t.Fatalf("broken files = %#v", files)
	}
}

func TestMountedMediaFailuresAreNotAutoHealed(t *testing.T) {
	r := &Repair{}
	results := []fileResult{{
		name: "episode.mkv", infoHash: "hash", protocol: config.ProtocolTorrent,
		broken: true, reason: "media_probe_failed",
	}}
	// A nil manager would panic if this result reached the provider reinsertion
	// path, so returning normally also proves it remains excluded.
	r.autoHealResults(context.Background(), results, newHealCache())
	if !results[0].broken || results[0].reason != "media_probe_failed" {
		t.Fatalf("result was incorrectly auto-healed: %#v", results[0])
	}
}

func TestClassifyMountedReadErrorWrapsErrors(t *testing.T) {
	got := classifyMountedReadError(errors.Join(errors.New("read failed"), syscall.EIO))
	if got.state != mediaProbeBroken || got.reason != "mount_read_error" {
		t.Fatalf("result = %#v", got)
	}
}
