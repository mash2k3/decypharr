package manager

import (
	"testing"

	"github.com/sirrobot01/decypharr/internal/utils"
	"github.com/sirrobot01/decypharr/pkg/storage"
)

func TestTorrentSubmissionKey(t *testing.T) {
	req := func(hash, debrid string) *ImportRequest {
		return &ImportRequest{Magnet: &utils.Magnet{InfoHash: hash}, SelectedDebrid: debrid}
	}
	if got := torrentSubmissionKey(req(" ABCdef ", "")); got != "auto:abcdef" {
		t.Fatalf("key = %q, want auto:abcdef", got)
	}
	if a, b := torrentSubmissionKey(req("abc", "RealDebrid")), torrentSubmissionKey(req("abc", "torbox")); a == b {
		t.Fatalf("different providers share key %q", a)
	}
	if got := torrentSubmissionKey(req("", "realdebrid")); got != "" {
		t.Fatalf("empty hash should give no key, got %q", got)
	}
}

func TestQueuedSatisfiesAdd(t *testing.T) {
	req := func(debrid string) *ImportRequest {
		return &ImportRequest{Magnet: &utils.Magnet{InfoHash: "abc"}, SelectedDebrid: debrid}
	}
	live := &storage.Entry{State: storage.EntryStateDownloading, ActiveProvider: "realdebrid"}
	failed := &storage.Entry{State: storage.EntryStateError, ActiveProvider: "realdebrid"}

	if !queuedSatisfiesAdd(live, req("")) {
		t.Fatal("a live queued entry should answer an auto add")
	}
	if !queuedSatisfiesAdd(live, req("RealDebrid")) {
		t.Fatal("a live queued entry should answer an add for its own provider")
	}
	if queuedSatisfiesAdd(live, req("torbox")) {
		t.Fatal("an add for another provider must be submitted")
	}
	if queuedSatisfiesAdd(failed, req("")) {
		t.Fatal("a retry of a failed entry must be submitted again")
	}
	if queuedSatisfiesAdd(nil, req("")) {
		t.Fatal("nil entry")
	}
}
