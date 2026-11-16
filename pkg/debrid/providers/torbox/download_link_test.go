package torbox

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/rs/zerolog"
	"github.com/sirrobot01/decypharr/internal/config"
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

// Repeated opens of one file (Plex probes and range reads) reuse the resolved
// CDN URL. They must not call requestdl once per open.
func TestGetDownloadLinkCachesResolutionAcrossOpens(t *testing.T) {
	var calls int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls++
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprint(w, `{"success":true,"data":"https://store-1.tb-cdn.st/file?token=x"}`)
	}))
	t.Cleanup(server.Close)

	dc := config.Debrid{Name: "torbox", APIKey: "main-key", DownloadAPIKeys: []string{"download-key"}}
	tb := testTorbox(server.URL)
	tb.autoExpiresLinksAfter = 48 * time.Hour
	tb.accountsManager = account.NewManager(dc, nil, zerolog.Nop())
	file := &types.File{Id: "3", Name: "a.mkv", Link: "torbox://7/3"}

	for i := 0; i < 25; i++ {
		dl, err := tb.GetDownloadLink("7", file)
		if err != nil {
			t.Fatal(err)
		}
		if dl.Token != "download-key" {
			t.Fatalf("Token = %q, want selected download key", dl.Token)
		}
	}
	if calls != 1 {
		t.Fatalf("requestdl calls = %d after 25 opens, want 1", calls)
	}
}

// If requestdl can't be resolved, never hand the API permalink to the stream:
// each range request through it would be an API call outside our limiter.
func TestFetchDownloadLinkFailureDoesNotReturnRedirectURL(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "busy", http.StatusTooManyRequests)
	}))
	t.Cleanup(server.Close)

	tb := testTorbox(server.URL)
	tb.autoExpiresLinksAfter = 48 * time.Hour
	dl, err := tb.fetchDownloadLink(&account.Account{Token: "key"}, "7", &types.File{Id: "3", Name: "a.mkv"})
	if err == nil {
		t.Fatalf("DownloadLink = %q, want resolution error", dl.DownloadLink)
	}
	if dl.DownloadLink != "" {
		t.Fatalf("DownloadLink = %q, want no API redirect fallback", dl.DownloadLink)
	}
}

func TestOnlyUsesKey(t *testing.T) {
	cases := []struct {
		keys []string
		want bool
	}{
		{nil, true},
		{[]string{"main"}, true},
		{[]string{"main", ""}, true},
		{[]string{"main", "other"}, false},
		{[]string{"other"}, false},
	}
	for _, c := range cases {
		if got := onlyUsesKey(c.keys, "main"); got != c.want {
			t.Errorf("onlyUsesKey(%v) = %v, want %v", c.keys, got, c.want)
		}
	}
}
