package manager

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	"github.com/sirrobot01/decypharr/internal/config"
	"github.com/sirrobot01/decypharr/pkg/storage"
)

func replacementVerifyFixture(t *testing.T) (*Repair, *storage.Storage) {
	t.Helper()
	repair, store := replacementAckFixture(t)
	cfg := config.Get()
	cfg.Mount.MountPath = t.TempDir()
	repair.manager.config = cfg
	repair.mediaProbeSlots = make(chan struct{}, repairMediaProbeConcurrency)

	file := &storage.File{Name: "S08E06.mkv", InfoHash: "new-hash", Size: 42}
	entry := &storage.Entry{
		InfoHash: "new-hash", Name: "Chicago Med S08", Protocol: config.ProtocolNZB,
		Files:        map[string]*storage.File{file.Name: file},
		CliDebridIDs: map[string]int64{file.Name: 75299},
	}
	if err := store.AddOrUpdate(entry); err != nil {
		t.Fatal(err)
	}
	return repair, store
}

func TestVerifyReplacementPersistsBrokenCandidate(t *testing.T) {
	repair, store := replacementVerifyFixture(t)
	calls := 0
	repair.mediaProbeAttempt = func(_ context.Context, path string) mediaProbeResult {
		calls++
		want := filepath.Join(repair.manager.config.Mount.MountPath, EntryAllFolder, "Chicago Med S08", "S08E06.mkv")
		if path != want {
			t.Fatalf("probe path = %q, want %q", path, want)
		}
		return mediaProbeResult{state: mediaProbeBroken, reason: "media_probe_failed"}
	}
	result, err := repair.VerifyReplacement(context.Background(), ReplacementVerifyRequest{CliDebridID: 75299, InfoHash: "new-hash"})
	if err != nil {
		t.Fatal(err)
	}
	if result.Status != "broken" || result.Reason != "media_probe_failed" || calls != 2 {
		t.Fatalf("result=%#v calls=%d", result, calls)
	}
	health, err := store.GetEntryHealth("Chicago Med S08")
	if err != nil {
		t.Fatal(err)
	}
	if health.Status != storage.HealthBroken || len(health.BrokenFiles) != 1 || health.BrokenFiles[0].CliDebridID != 75299 {
		t.Fatalf("broken verification was not persisted: %#v", health)
	}
}

func TestVerifyReplacementHealthyClearsOnlyCandidateFailure(t *testing.T) {
	repair, store := replacementVerifyFixture(t)
	if err := store.SaveEntryHealth(&storage.EntryHealth{
		EntryName: "Chicago Med S08", Status: storage.HealthBroken,
		BrokenFiles: []storage.BrokenFile{
			{EntryName: "Chicago Med S08", FileName: "S08E06.mkv", InfoHash: "new-hash", CliDebridID: 75299, Reason: "mount_read_error"},
			{EntryName: "Chicago Med S08", FileName: "sibling.mkv", InfoHash: "other", CliDebridID: 9, Reason: "mount_read_error"},
		},
	}); err != nil {
		t.Fatal(err)
	}
	repair.mediaProbeAttempt = func(context.Context, string) mediaProbeResult {
		return mediaProbeResult{state: mediaProbeHealthy}
	}
	result, err := repair.VerifyReplacement(context.Background(), ReplacementVerifyRequest{CliDebridID: 75299, InfoHash: "new-hash"})
	if err != nil || result.Status != "healthy" {
		t.Fatalf("result=%#v err=%v", result, err)
	}
	health, err := store.GetEntryHealth("Chicago Med S08")
	if err != nil || health.Status != storage.HealthBroken || len(health.BrokenFiles) != 1 || health.BrokenFiles[0].FileName != "sibling.mkv" {
		t.Fatalf("sibling health changed: %#v err=%v", health, err)
	}
}

func TestVerifyReplacementUnknownDoesNotPersistFailure(t *testing.T) {
	repair, store := replacementVerifyFixture(t)
	repair.mediaProbeAttempt = func(context.Context, string) mediaProbeResult {
		return mediaProbeResult{state: mediaProbeUnknown, reason: "media_probe_timeout"}
	}
	result, err := repair.VerifyReplacement(context.Background(), ReplacementVerifyRequest{CliDebridID: 75299, InfoHash: "new-hash"})
	if err != nil || result.Status != "unknown" {
		t.Fatalf("result=%#v err=%v", result, err)
	}
	if health, err := store.GetEntryHealth("Chicago Med S08"); err == nil && health.Status == storage.HealthBroken {
		t.Fatalf("unknown probe persisted broken health: %#v", health)
	}
}

func TestVerifyReplacementRejectsStaleAndNotReadyIdentifiers(t *testing.T) {
	repair, _ := replacementVerifyFixture(t)
	tests := []struct {
		name string
		req  ReplacementVerifyRequest
		code string
	}{
		{"stale hash", ReplacementVerifyRequest{CliDebridID: 75299, InfoHash: "wrong-hash"}, "stale_target"},
		{"not registered", ReplacementVerifyRequest{CliDebridID: 88888, InfoHash: "new-hash"}, "replacement_not_ready"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := repair.VerifyReplacement(context.Background(), tt.req)
			var verifyErr *ReplacementAckError
			if !errors.As(err, &verifyErr) || verifyErr.Code != tt.code {
				t.Fatalf("error=%#v want=%q", err, tt.code)
			}
		})
	}
}

func TestVerifyReplacementRejectsBusyRepair(t *testing.T) {
	repair, _ := replacementVerifyFixture(t)
	repair.activeRunID = "active-run"
	_, err := repair.VerifyReplacement(context.Background(), ReplacementVerifyRequest{CliDebridID: 75299, InfoHash: "new-hash"})
	var verifyErr *ReplacementAckError
	if !errors.As(err, &verifyErr) || verifyErr.Code != "repair_busy" {
		t.Fatalf("error=%#v", err)
	}
}
