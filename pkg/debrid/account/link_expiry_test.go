package account

import (
	"testing"
	"time"

	"github.com/puzpuzpuz/xsync/v4"
	"github.com/sirrobot01/decypharr/pkg/debrid/types"
)

// A cached link past its ExpiresAt is refetched instead of served (TorBox CDN
// URLs are now cached and expire); a zero ExpiresAt still means "no expiry".
func TestGetDownloadLinkRefetchesExpiredLink(t *testing.T) {
	acc := &Account{Debrid: "torbox", Token: "t", links: xsync.NewMap[string, types.DownloadLink]()}
	file := &types.File{Id: "1", Link: "file-1"}
	var fetches int
	fetch := func(_ *Account, _ string, f *types.File) (types.DownloadLink, error) {
		fetches++
		return types.DownloadLink{Link: f.Link, DownloadLink: "https://cdn.example/new", ExpiresAt: time.Now().Add(time.Hour)}, nil
	}

	acc.storeLink(types.DownloadLink{Link: "file-1", DownloadLink: "https://cdn.example/old", ExpiresAt: time.Now().Add(-time.Minute)})
	dl, err := acc.GetDownloadLink("7", file, fetch)
	if err != nil {
		t.Fatal(err)
	}
	if dl.DownloadLink != "https://cdn.example/new" || fetches != 1 {
		t.Fatalf("got %q after %d fetches, want a fresh link from 1 fetch", dl.DownloadLink, fetches)
	}

	// Fresh one is reused.
	if _, err := acc.GetDownloadLink("7", file, fetch); err != nil || fetches != 1 {
		t.Fatalf("fresh link refetched (fetches=%d, err=%v)", fetches, err)
	}

	// No expiry set: reused.
	acc.storeLink(types.DownloadLink{Link: "file-2", DownloadLink: "https://cdn.example/forever"})
	dl, err = acc.GetDownloadLink("7", &types.File{Id: "2", Link: "file-2"}, fetch)
	if err != nil || dl.DownloadLink != "https://cdn.example/forever" || fetches != 1 {
		t.Fatalf("zero-expiry link not reused: %q, fetches=%d, err=%v", dl.DownloadLink, fetches, err)
	}
}
