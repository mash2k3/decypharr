package manager

import (
	"testing"

	"github.com/sirrobot01/decypharr/internal/utils"
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
