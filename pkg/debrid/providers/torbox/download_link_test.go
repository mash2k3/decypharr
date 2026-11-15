package torbox

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/sirrobot01/decypharr/pkg/debrid/account"
	"github.com/sirrobot01/decypharr/pkg/debrid/types"
)

// Streaming the requestdl redirect URL made every range read (every Plex
// scan/analysis read) a TorBox API call. The CDN URL is resolved once instead,
// and kept no longer than cdnLinkLifetime.
func TestFetchDownloadLinkResolvesCDNURL(t *testing.T) {
	var calls int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if r.URL.Path != "/api/torrents/requestdl" || r.URL.Query().Get("redirect") != "false" {
			t.Errorf("request = %s, want requestdl with redirect=false", r.URL)
		}
		if r.URL.Query().Get("token") != "key" || r.URL.Query().Get("torrent_id") != "7" || r.URL.Query().Get("file_id") != "3" {
			t.Errorf("query = %s, want token/torrent_id/file_id", r.URL.RawQuery)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprint(w, `{"success":true,"data":"https://store-1.tb-cdn.st/file?token=x"}`)
	}))
	t.Cleanup(server.Close)

	tb := testTorbox(server.URL)
	tb.autoExpiresLinksAfter = 48 * time.Hour
	dl, err := tb.fetchDownloadLink(&account.Account{Token: "key"}, "7", &types.File{Id: "3", Name: "a.mkv"})
	if err != nil {
		t.Fatal(err)
	}
	if dl.DownloadLink != "https://store-1.tb-cdn.st/file?token=x" {
		t.Fatalf("DownloadLink = %q, want the CDN URL", dl.DownloadLink)
	}
	if calls != 1 {
		t.Fatalf("requestdl calls = %d, want 1", calls)
	}
	if life := time.Until(dl.ExpiresAt); life > cdnLinkLifetime || life <= 0 {
		t.Fatalf("CDN link lifetime = %s, want (0, %s]", life, cdnLinkLifetime)
	}
}

// If requestdl can't be resolved (429, outage), keep streaming through the
// redirect URL, but only briefly so the next open tries to resolve again.
func TestFetchDownloadLinkFallsBackToRedirectURL(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "busy", http.StatusTooManyRequests)
	}))
	t.Cleanup(server.Close)

	tb := testTorbox(server.URL)
	tb.autoExpiresLinksAfter = 48 * time.Hour
	dl, err := tb.fetchDownloadLink(&account.Account{Token: "key"}, "7", &types.File{Id: "3", Name: "a.mkv"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(dl.DownloadLink, "/api/torrents/requestdl?") || !strings.Contains(dl.DownloadLink, "redirect=true") {
		t.Fatalf("DownloadLink = %q, want the redirect URL", dl.DownloadLink)
	}
	if life := time.Until(dl.ExpiresAt); life > redirectFallbackLifetime || life <= 0 {
		t.Fatalf("fallback lifetime = %s, want (0, %s]", life, redirectFallbackLifetime)
	}
}
