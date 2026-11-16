package torbox

import (
	"fmt"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
)

// TorBox's own list endpoint can serve a server-side cached page across a
// pagination sweep, so a torrent added or removed between offset 0 and the
// next page can silently disappear from (or duplicate in) the merged result.
// bypass_cache=true makes every page reflect the current state.
func TestGetTorrentsBypassesTorboxCache(t *testing.T) {
	var (
		mu      sync.Mutex
		offsets []string
	)
	tb := newTestTorbox(t, func(w http.ResponseWriter, r *http.Request) {
		if got := r.URL.Query().Get("bypass_cache"); got != "true" {
			t.Errorf("bypass_cache = %q, want true", got)
		}
		if got := r.URL.Query().Get("files"); got != "false" {
			t.Errorf("files = %q, want false", got)
		}
		if got := r.URL.Query().Get("limit"); got != "1000" {
			t.Errorf("limit = %q, want 1000", got)
		}

		offset := r.URL.Query().Get("offset")
		mu.Lock()
		offsets = append(offsets, offset)
		mu.Unlock()

		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprint(w, `{"success":true,"data":[{"id":17,"name":"Release.mkv","size":100,"progress":1,"download_state":"completed","download_finished":true,"hash":"ABC"}]}`)
	})

	torrents, err := tb.GetTorrents()
	if err != nil {
		t.Fatalf("GetTorrents() error = %v", err)
	}
	if len(torrents) != 1 || torrents[0].Id != "17" {
		t.Fatalf("GetTorrents() = %#v, want torrent 17", torrents)
	}

	mu.Lock()
	defer mu.Unlock()
	if !slices.Equal(offsets, []string{"0"}) {
		t.Fatalf("offsets = %v, want [0] (short final page must not request an empty page)", offsets)
	}
}

// A mid-pagination failure must propagate as an error, not be swallowed —
// returning the partial list with a nil error would make the caller believe
// the fetch was complete and treat every torrent on the un-fetched remaining
// pages as removed from remote.
func TestGetTorrentsReturnsPaginationErrors(t *testing.T) {
	tb := newTestTorbox(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("offset") == "0" {
			w.Header().Set("Content-Type", "application/json")
			var body strings.Builder
			body.WriteString(`{"success":true,"data":[`)
			for i := 0; i < 1000; i++ {
				if i > 0 {
					body.WriteByte(',')
				}
				body.WriteString(`{"id":`)
				body.WriteString(strconv.Itoa(i + 1))
				body.WriteString(`,"name":"Release.mkv","hash":"hash-`)
				body.WriteString(strconv.Itoa(i + 1))
				body.WriteString(`"}`)
			}
			body.WriteString(`]}`)
			_, _ = fmt.Fprint(w, body.String())
			return
		}
		http.Error(w, "temporary failure", http.StatusInternalServerError)
	})

	torrents, err := tb.GetTorrents()
	if err == nil {
		t.Fatal("GetTorrents() error = nil, want pagination error")
	}
	if torrents != nil {
		t.Fatalf("GetTorrents() torrents = %#v, want nil after pagination error", torrents)
	}
	if err.Error() == "" {
		t.Fatal("GetTorrents() error message is empty")
	}
}
