package torbox

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rs/zerolog"
	"github.com/sirrobot01/decypharr/internal/config"
	"github.com/sirrobot01/decypharr/internal/request"
	"github.com/sirrobot01/decypharr/internal/utils"
	"github.com/sirrobot01/decypharr/pkg/debrid/types"
)

const submitHash = "81618a3711b5dd4c9374e5874a743fef9e838644"

func submitTorrent() *types.Torrent {
	return &types.Torrent{
		InfoHash:         submitHash,
		Magnet:           &utils.Magnet{InfoHash: submitHash, Link: "magnet:?xt=urn:btih:" + submitHash},
		DownloadUncached: true, // skip the checkcached pre-check
	}
}

// createtorrent can answer with an object or an array. (Upstream edc395f.)
func TestSubmitMagnetAcceptsObjectAndArrayResponses(t *testing.T) {
	tests := map[string]string{
		"object": `{"success":true,"data":{"torrent_id":17,"hash":"` + submitHash + `"}}`,
		"array":  `{"success":true,"data":[{"id":9,"hash":"other"},{"id":17,"hash":"` + strings.ToUpper(submitHash) + `"}]}`,
	}
	for name, body := range tests {
		t.Run(name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				_, _ = fmt.Fprint(w, body)
			}))
			t.Cleanup(server.Close)

			torrent, err := testTorbox(server.URL).SubmitMagnet(submitTorrent())
			if err != nil {
				t.Fatalf("SubmitMagnet() error = %v", err)
			}
			if torrent.Id != "17" {
				t.Fatalf("SubmitMagnet() id = %q, want 17", torrent.Id)
			}
		})
	}
}

// A new uncached torrent passes through "queued" and "checking" before
// downloading; neither is an error. (Upstream 3448c74, PR #473.)
func TestQueuedAndCheckingAreDownloading(t *testing.T) {
	for _, s := range []string{"queued", "checking", "downloading"} {
		if got := (&Torbox{}).getTorboxStatus(s, false); got != types.TorrentStatusDownloading {
			t.Errorf("getTorboxStatus(%q) = %q, want downloading", s, got)
		}
	}
}

// createtorrent has its own 60/hour limit. Over it, TorBox holds the add ~30s
// and answers 429; retrying hangs the grab and spends more of the limit.
// (Upstream PR #444.)
func TestSubmitMagnetRateLimitSendsOnceAndPauses(t *testing.T) {
	var mu sync.Mutex
	creates := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		creates++
		mu.Unlock()
		w.Header().Set("Retry-After", "120")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = fmt.Fprint(w, `{"success":false,"error":"RATE_LIMIT_EXCEEDED","detail":"60 per 1 hour","data":null}`)
	}))
	t.Cleanup(server.Close)

	tb := &Torbox{
		Host: server.URL,
		client: request.New(
			request.WithMaxRetries(3),
			request.WithRetryableStatus(http.StatusTooManyRequests, http.StatusBadGateway),
		),
		logger: zerolog.Nop(),
		config: config.Debrid{Name: "torbox"},
	}

	start := time.Now()
	_, err := tb.SubmitMagnet(submitTorrent())
	if err == nil || !strings.Contains(err.Error(), "add limit reached") {
		t.Fatalf("SubmitMagnet() error = %v, want add limit reached", err)
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Fatalf("SubmitMagnet() took %s, want no retry backoff", elapsed)
	}
	mu.Lock()
	if creates != 1 {
		t.Fatalf("creates = %d, want exactly 1 (no retries)", creates)
	}
	mu.Unlock()

	if wait := time.Until(tb.addsPausedUntil()); wait < 110*time.Second || wait > 125*time.Second {
		t.Fatalf("paused for %s, want about 120s from Retry-After", wait)
	}

	// A second add during the pause never reaches TorBox.
	_, err = tb.SubmitMagnet(submitTorrent())
	if err == nil || !strings.Contains(err.Error(), "paused until") {
		t.Fatalf("second SubmitMagnet() error = %v, want paused", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if creates != 1 {
		t.Fatalf("creates = %d after pause, want 1", creates)
	}
}

func TestPauseAddsDefaultAndCap(t *testing.T) {
	tb := &Torbox{}
	until := tb.pauseAdds(&http.Response{Header: http.Header{}})
	if wait := time.Until(until); wait < defaultAddLimitPause-5*time.Second || wait > defaultAddLimitPause {
		t.Fatalf("paused for %s, want about %s", wait, defaultAddLimitPause)
	}
	tb = &Torbox{}
	until = tb.pauseAdds(&http.Response{Header: http.Header{"Retry-After": []string{"86400"}}})
	if wait := time.Until(until); wait > maxAddLimitPause {
		t.Fatalf("paused for %s, want at most %s", wait, maxAddLimitPause)
	}
}

// TorBox zips 100+ file torrents unless allow_zip=false is sent.
func TestSubmitMagnetSendsAllowZipFalse(t *testing.T) {
	var got string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseMultipartForm(1 << 20)
		got = r.FormValue("allow_zip")
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprint(w, `{"success":true,"data":{"torrent_id":17,"hash":"`+submitHash+`"}}`)
	}))
	t.Cleanup(server.Close)
	if _, err := testTorbox(server.URL).SubmitMagnet(submitTorrent()); err != nil {
		t.Fatal(err)
	}
	if got != "false" {
		t.Fatalf("allow_zip = %q, want false", got)
	}
}
