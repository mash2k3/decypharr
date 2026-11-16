package premiumize

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/sirrobot01/decypharr/internal/config"
	"github.com/sirrobot01/decypharr/internal/request"
	"github.com/sirrobot01/decypharr/pkg/debrid/account"
	"github.com/sirrobot01/decypharr/pkg/debrid/types"
)

// file.Link is the link from when the transfer was listed and dies after about
// a day; a refetch must ask Premiumize for a fresh one, while keeping the
// original as the cache key. (Upstream 990f403.)
func TestFetchDownloadLinkRemintsFromItem(t *testing.T) {
	config.SetConfigPath(t.TempDir())
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if r.URL.Path != "/api/item/details" || r.URL.Query().Get("id") != "item-1" {
			t.Errorf("request = %s, want item details for item-1", r.URL)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprint(w, `{"status":"success","id":"item-1","name":"a.mkv","size":42,"link":"https://cdn.example/fresh"}`)
	}))
	t.Cleanup(server.Close)

	pm := &Premiumize{
		Host:   server.URL,
		client: request.New(request.WithMaxRetries(0)),
		config: config.Debrid{Name: "premiumize"},
	}
	dl, err := pm.fetchDownloadLink(&account.Account{Token: "t"}, "transfer", &types.File{Id: "item-1", Name: "a.mkv", Link: "https://cdn.example/stale"})
	if err != nil {
		t.Fatal(err)
	}
	if calls != 1 || dl.DownloadLink != "https://cdn.example/fresh" {
		t.Fatalf("DownloadLink = %q after %d calls, want the fresh link from item details", dl.DownloadLink, calls)
	}
	if dl.Link != "https://cdn.example/stale" {
		t.Fatalf("Link = %q, want the original file link kept as the cache key", dl.Link)
	}
}
