package manager

import (
	"errors"
	"path/filepath"
	"testing"

	"github.com/rs/zerolog"
	"github.com/sirrobot01/decypharr/internal/config"
	"github.com/sirrobot01/decypharr/pkg/storage"
)

func replacementAckFixture(t *testing.T) (*Repair, *storage.Storage) {
	t.Helper()
	config.Reset()
	config.SetConfigPath(t.TempDir())
	t.Cleanup(config.Reset)
	store, err := storage.NewStorage(filepath.Join(t.TempDir(), "db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })

	oldFile := &storage.File{Name: "S03E01.mkv", InfoHash: "old-hash", Size: 10}
	sibling := &storage.File{Name: "S03E02.mkv", InfoHash: "old-hash", Size: 20}
	entry := &storage.Entry{
		InfoHash: "old-hash", Name: "old-release", Protocol: config.ProtocolNZB,
		Files:        map[string]*storage.File{oldFile.Name: oldFile, sibling.Name: sibling},
		CliDebridIDs: map[string]int64{oldFile.Name: 75299, sibling.Name: 75300},
	}
	if err := store.AddOrUpdate(entry); err != nil {
		t.Fatal(err)
	}
	if err := store.UpdateItem(&storage.EntryItem{
		Name: "The Sopranos S03", Size: 30,
		Files: map[string]*storage.File{oldFile.Name: oldFile, sibling.Name: sibling},
	}); err != nil {
		t.Fatal(err)
	}
	health := &storage.EntryHealth{
		EntryName: "The Sopranos S03", Protocol: config.ProtocolNZB,
		Status: storage.HealthBroken, FileCount: 2,
		BrokenFiles: []storage.BrokenFile{{
			EntryName: "The Sopranos S03", FileName: oldFile.Name,
			InfoHash: "old-hash", Protocol: config.ProtocolNZB,
			CliDebridID: 75299, Reason: "mount_read_error",
		}},
	}
	if err := store.SaveEntryHealth(health); err != nil {
		t.Fatal(err)
	}
	mgr := &Manager{storage: store, config: config.Get()}
	mgr.entry = NewEntryCache(mgr)
	return &Repair{manager: mgr, logger: zerolog.Nop()}, store
}

func TestAcknowledgeReplacementRemovesOnlyTargetFile(t *testing.T) {
	repair, store := replacementAckFixture(t)
	req := ReplacementAckRequest{
		EntryName: "The Sopranos S03", FileName: "S03E01.mkv", InfoHash: "old-hash",
		CliDebridID: 75299, Reason: "mount_read_error",
	}
	result, err := repair.AcknowledgeReplacement(req)
	if err != nil {
		t.Fatal(err)
	}
	if result.Status != "removed" || result.EntryDeleted {
		t.Fatalf("unexpected result: %#v", result)
	}
	item, err := store.GetEntryItem(req.EntryName)
	if err != nil {
		t.Fatal(err)
	}
	if file := item.Files[req.FileName]; file == nil || !file.Deleted {
		t.Fatalf("target was not marked deleted: %#v", file)
	}
	if file := item.Files["S03E02.mkv"]; file == nil || file.Deleted {
		t.Fatalf("healthy sibling was removed: %#v", file)
	}
	health, err := store.GetEntryHealth(req.EntryName)
	if err != nil {
		t.Fatal(err)
	}
	if health.Status != storage.HealthUnknown || len(health.BrokenFiles) != 0 {
		t.Fatalf("health was not cleared precisely: %#v", health)
	}

	// Simulate a crash after the file mutation but before the health update.
	health.Status = storage.HealthBroken
	health.BrokenFiles = []storage.BrokenFile{{
		EntryName: req.EntryName, FileName: req.FileName, InfoHash: req.InfoHash,
		Protocol: config.ProtocolNZB, CliDebridID: req.CliDebridID, Reason: req.Reason,
	}}
	health.BrokenCount = 1
	if err := store.SaveEntryHealth(health); err != nil {
		t.Fatal(err)
	}
	again, err := repair.AcknowledgeReplacement(req)
	if err != nil || again.Status != "already_removed" {
		t.Fatalf("repeat acknowledgement = %#v, %v", again, err)
	}
	health, err = store.GetEntryHealth(req.EntryName)
	if err != nil || health.Status != storage.HealthUnknown || len(health.BrokenFiles) != 0 {
		t.Fatalf("repeat acknowledgement did not finish health cleanup: %#v, %v", health, err)
	}
}

func TestAcknowledgeReplacementRejectsUnsafeTargets(t *testing.T) {
	repair, _ := replacementAckFixture(t)
	tests := []struct {
		name string
		req  ReplacementAckRequest
		code string
	}{
		{"wrong id", ReplacementAckRequest{EntryName: "The Sopranos S03", FileName: "S03E01.mkv", InfoHash: "old-hash", CliDebridID: 999, Reason: "mount_read_error"}, "stale_target"},
		{"wrong hash", ReplacementAckRequest{EntryName: "The Sopranos S03", FileName: "S03E01.mkv", InfoHash: "new-hash", CliDebridID: 75299, Reason: "mount_read_error"}, "stale_target"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := repair.AcknowledgeReplacement(tt.req)
			var ackErr *ReplacementAckError
			if !errors.As(err, &ackErr) || ackErr.Code != tt.code {
				t.Fatalf("error = %#v, want code %q", err, tt.code)
			}
		})
	}
}

func TestAcknowledgeReplacementUsesExactHealthWhenProviderEntryIsMissing(t *testing.T) {
	repair, store := replacementAckFixture(t)
	// Simulate a retained mounted item whose provider Entry has already gone.
	if err := store.Delete("old-hash"); err != nil {
		t.Fatal(err)
	}
	oldFile := &storage.File{Name: "S03E01.mkv", InfoHash: "old-hash", Size: 10}
	sibling := &storage.File{Name: "S03E02.mkv", InfoHash: "old-hash", Size: 20}
	if err := store.UpdateItem(&storage.EntryItem{
		Name: "The Sopranos S03", Size: 30,
		Files: map[string]*storage.File{oldFile.Name: oldFile, sibling.Name: sibling},
	}); err != nil {
		t.Fatal(err)
	}

	result, err := repair.AcknowledgeReplacement(ReplacementAckRequest{
		EntryName: "The Sopranos S03", FileName: "S03E01.mkv", InfoHash: "old-hash",
		CliDebridID: 75299, Reason: "mount_read_error",
	})
	if err != nil || result.Status != "removed" || result.EntryDeleted {
		t.Fatalf("result=%#v err=%v", result, err)
	}
	item, err := store.GetEntryItem("The Sopranos S03")
	if err != nil {
		t.Fatal(err)
	}
	if !item.Files["S03E01.mkv"].Deleted || item.Files["S03E02.mkv"].Deleted {
		t.Fatalf("orphan cleanup changed the wrong files: %#v", item.Files)
	}
}

func TestAcknowledgeReplacementRejectsMissingProviderWithoutExactHealth(t *testing.T) {
	repair, store := replacementAckFixture(t)
	if err := store.Delete("old-hash"); err != nil {
		t.Fatal(err)
	}
	oldFile := &storage.File{Name: "S03E01.mkv", InfoHash: "old-hash", Size: 10}
	if err := store.UpdateItem(&storage.EntryItem{
		Name: "The Sopranos S03", Files: map[string]*storage.File{oldFile.Name: oldFile},
	}); err != nil {
		t.Fatal(err)
	}
	health, err := store.GetEntryHealth("The Sopranos S03")
	if err != nil {
		t.Fatal(err)
	}
	health.BrokenFiles[0].CliDebridID = 999
	if err := store.SaveEntryHealth(health); err != nil {
		t.Fatal(err)
	}

	_, err = repair.AcknowledgeReplacement(ReplacementAckRequest{
		EntryName: "The Sopranos S03", FileName: "S03E01.mkv", InfoHash: "old-hash",
		CliDebridID: 75299, Reason: "mount_read_error",
	})
	var ackErr *ReplacementAckError
	if !errors.As(err, &ackErr) || ackErr.Code != "stale_target" {
		t.Fatalf("error=%#v, want stale_target", err)
	}
}

func TestAcknowledgeReplacementDeletesFinalOrphanedMountedEntry(t *testing.T) {
	repair, store := replacementAckFixture(t)
	if err := store.Delete("old-hash"); err != nil {
		t.Fatal(err)
	}
	oldFile := &storage.File{Name: "S03E01.mkv", InfoHash: "old-hash", Size: 10}
	if err := store.UpdateItem(&storage.EntryItem{
		Name: "The Sopranos S03", Size: 10,
		Files: map[string]*storage.File{oldFile.Name: oldFile},
	}); err != nil {
		t.Fatal(err)
	}

	result, err := repair.AcknowledgeReplacement(ReplacementAckRequest{
		EntryName: "The Sopranos S03", FileName: "S03E01.mkv", InfoHash: "old-hash",
		CliDebridID: 75299, Reason: "mount_read_error",
	})
	if err != nil || result.Status != "removed" || !result.EntryDeleted {
		t.Fatalf("result=%#v err=%v", result, err)
	}
	if _, err := store.GetEntryItem("The Sopranos S03"); err == nil {
		t.Fatal("orphaned mounted entry still exists")
	}
	if health, _ := store.GetEntryHealth("The Sopranos S03"); health != nil {
		t.Fatalf("orphaned health still exists: %#v", health)
	}
}

func TestAcknowledgeReplacementAllowsProtectedMissingSegmentCandidate(t *testing.T) {
	repair, store := replacementAckFixture(t)
	health, err := store.GetEntryHealth("The Sopranos S03")
	if err != nil {
		t.Fatal(err)
	}
	health.BrokenFiles[0].Reason = "usenet_segment_missing"
	health.FailureReason = "usenet_segment_missing"
	if err := store.SaveEntryHealth(health); err != nil {
		t.Fatal(err)
	}
	result, err := repair.AcknowledgeReplacement(ReplacementAckRequest{
		EntryName: "The Sopranos S03", FileName: "S03E01.mkv", InfoHash: "old-hash",
		CliDebridID: 75299, Reason: "usenet_segment_missing",
	})
	if err != nil || result.Status != "removed" {
		t.Fatalf("result=%#v err=%v", result, err)
	}
}

func TestAcknowledgeReplacementDeletesSingleFileEntry(t *testing.T) {
	repair, store := replacementAckFixture(t)
	item, err := store.GetEntryItem("The Sopranos S03")
	if err != nil {
		t.Fatal(err)
	}
	item.Files["S03E02.mkv"].Deleted = true
	if err := store.UpdateItem(item); err != nil {
		t.Fatal(err)
	}

	result, err := repair.AcknowledgeReplacement(ReplacementAckRequest{
		EntryName: "The Sopranos S03", FileName: "S03E01.mkv", InfoHash: "old-hash",
		CliDebridID: 75299, Reason: "mount_read_error",
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.Status != "removed" || !result.EntryDeleted {
		t.Fatalf("unexpected result: %#v", result)
	}
	if _, err := store.Get("old-hash"); err == nil {
		t.Fatal("single-file old entry still exists")
	}
}

func TestAcknowledgeReplacementRejectsBusyRepair(t *testing.T) {
	repair, _ := replacementAckFixture(t)
	repair.activeRunID = "active-run"
	_, err := repair.AcknowledgeReplacement(ReplacementAckRequest{
		EntryName: "The Sopranos S03", FileName: "S03E01.mkv", InfoHash: "old-hash",
		CliDebridID: 75299, Reason: "mount_read_error",
	})
	var ackErr *ReplacementAckError
	if !errors.As(err, &ackErr) || ackErr.Code != "repair_busy" {
		t.Fatalf("error = %#v, want repair_busy", err)
	}
}
