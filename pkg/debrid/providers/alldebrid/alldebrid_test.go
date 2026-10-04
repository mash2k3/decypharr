package alldebrid

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sirrobot01/decypharr/internal/config"
	"github.com/sirrobot01/decypharr/internal/request"
	debridTypes "github.com/sirrobot01/decypharr/pkg/debrid/types"
)

func TestAllDebridStatusClassification(t *testing.T) {
	tests := map[string]struct {
		statusCode int
		want       debridTypes.TorrentStatus
	}{
		"ready":                 {statusCode: 4, want: debridTypes.TorrentStatusDownloaded},
		"processing":            {statusCode: 1, want: debridTypes.TorrentStatusDownloading},
		"no peer timeout":       {statusCode: 7, want: debridTypes.TorrentStatusDownloading},
		"72 hour timeout":       {statusCode: 10, want: debridTypes.TorrentStatusError},
		"deleted by the hoster": {statusCode: 11, want: debridTypes.TorrentStatusError},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			if got := getAlldebridStatus(tt.statusCode); got != tt.want {
				t.Errorf("getAlldebridStatus(%d) = %q, want %q", tt.statusCode, got, tt.want)
			}
		})
	}
}

func TestCheckStatusRestartsStatusSeven(t *testing.T) {
	config.SetConfigPath(t.TempDir())

	var statusChecks atomic.Int32
	var restartCalls atomic.Int32
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v4.1/magnet/status", func(w http.ResponseWriter, r *http.Request) {
		statusCode := 7
		if statusChecks.Add(1) > 1 {
			statusCode = 1
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprintf(w, `{"status":"success","data":{"magnets":[{"id":42,"filename":"Release.mkv","statusCode":%d}]}}`, statusCode)
	})
	mux.HandleFunc("POST /v4/magnet/restart", func(w http.ResponseWriter, r *http.Request) {
		restartCalls.Add(1)
		if got := r.Header.Get("Content-Type"); got != "application/x-www-form-urlencoded" {
			t.Errorf("Content-Type = %q, want application/x-www-form-urlencoded", got)
		}
		if err := r.ParseForm(); err != nil {
			t.Errorf("ParseForm() error = %v", err)
		}
		if got := r.Form.Get("id"); got != "42" {
			t.Errorf("id = %q, want 42", got)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprint(w, `{"status":"success","data":{"message":"Magnet was successfully restarted"}}`)
	})
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)

	ad := testAllDebrid(server.URL + "/v4.1")
	torrent := &debridTypes.Torrent{Id: "42", DownloadUncached: true}
	got, err := ad.CheckStatus(torrent)
	if err != nil {
		t.Fatalf("CheckStatus() error = %v", err)
	}
	if got.Status != debridTypes.TorrentStatusDownloading {
		t.Errorf("CheckStatus() status = %q, want %q", got.Status, debridTypes.TorrentStatusDownloading)
	}
	if got := statusChecks.Load(); got != 2 {
		t.Errorf("status checks = %d, want 2", got)
	}
	if got := restartCalls.Load(); got != 1 {
		t.Errorf("restart calls = %d, want 1", got)
	}
}

func TestCheckStatusBoundsStatusSevenRetries(t *testing.T) {
	config.SetConfigPath(t.TempDir())

	var statusChecks atomic.Int32
	var restartCalls atomic.Int32
	mux := http.NewServeMux()
	mux.HandleFunc("GET /magnet/status", func(w http.ResponseWriter, r *http.Request) {
		statusChecks.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprint(w, `{"status":"success","data":{"magnets":[{"id":42,"filename":"Release.mkv","statusCode":7}]}}`)
	})
	mux.HandleFunc("POST /magnet/restart", func(w http.ResponseWriter, r *http.Request) {
		restartCalls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprint(w, `{"status":"success","data":{"message":"Magnet was successfully restarted"}}`)
	})
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)

	ad := testAllDebrid(server.URL)
	torrent := &debridTypes.Torrent{Id: "42", DownloadUncached: true}
	got, err := ad.CheckStatus(torrent)
	if err == nil || !strings.Contains(err.Error(), "remained at status code 7") {
		t.Fatalf("CheckStatus() error = %v, want bounded status code 7 error", err)
	}
	if got.Status != debridTypes.TorrentStatusError {
		t.Errorf("CheckStatus() status = %q, want %q", got.Status, debridTypes.TorrentStatusError)
	}
	if got := statusChecks.Load(); got != 4 {
		t.Errorf("status checks = %d, want 4", got)
	}
	if got := restartCalls.Load(); got != 1 {
		t.Errorf("restart calls = %d, want 1", got)
	}
}

func TestCheckStatusDoesNotRestartTerminalStatus(t *testing.T) {
	config.SetConfigPath(t.TempDir())

	var restartCalls atomic.Int32
	mux := http.NewServeMux()
	mux.HandleFunc("GET /magnet/status", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprint(w, `{"status":"success","data":{"magnets":[{"id":42,"filename":"Release.mkv","statusCode":10}]}}`)
	})
	mux.HandleFunc("POST /magnet/restart", func(w http.ResponseWriter, r *http.Request) {
		restartCalls.Add(1)
		w.WriteHeader(http.StatusNoContent)
	})
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)

	ad := testAllDebrid(server.URL)
	torrent := &debridTypes.Torrent{Id: "42", DownloadUncached: true}
	got, err := ad.CheckStatus(torrent)
	if err == nil || !strings.Contains(err.Error(), "status code 10") {
		t.Fatalf("CheckStatus() error = %v, want terminal status code 10 error", err)
	}
	if got.Status != debridTypes.TorrentStatusError {
		t.Errorf("CheckStatus() status = %q, want %q", got.Status, debridTypes.TorrentStatusError)
	}
	if got := restartCalls.Load(); got != 0 {
		t.Errorf("restart calls = %d, want 0", got)
	}
}

func testAllDebrid(host string) *AllDebrid {
	return &AllDebrid{
		Host:               host,
		client:             request.New(request.WithMaxRetries(0)),
		config:             config.Debrid{Name: "alldebrid"},
		noPeerRetryBackoff: []time.Duration{0, 0, 0},
	}
}
