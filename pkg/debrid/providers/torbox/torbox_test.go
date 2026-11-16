package torbox

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/rs/zerolog"
	"github.com/sirrobot01/decypharr/internal/config"
	"github.com/sirrobot01/decypharr/internal/request"
	"github.com/sirrobot01/decypharr/pkg/debrid/types"
)

func TestDeleteTorrentUsesControlEndpoint(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("method = %s, want POST", r.Method)
		}
		if r.URL.Path != "/api/torrents/controltorrent" {
			t.Errorf("path = %q, want /api/torrents/controltorrent", r.URL.Path)
		}
		var payload struct {
			TorrentID int    `json:"torrent_id"`
			Operation string `json:"operation"`
			All       bool   `json:"all"`
		}
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			t.Fatalf("decode request: %v", err)
		}
		if payload.TorrentID != 42 || payload.Operation != "delete" || payload.All {
			t.Errorf("payload = %#v, want torrent 42 delete operation", payload)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprint(w, `{"success":true,"detail":"Torrent deleted successfully"}`)
	}))
	t.Cleanup(server.Close)

	if err := testTorbox(server.URL).DeleteTorrent("42"); err != nil {
		t.Fatalf("DeleteTorrent() error = %v", err)
	}
}

func TestGetTorrentAcceptsObjectAndArrayResponses(t *testing.T) {
	tests := map[string]string{
		"object": `{"success":true,"data":{"id":17,"name":"Release.mkv","size":100,"progress":1,"download_state":"completed","download_finished":true,"created_at":"2026-01-02T03:04:05Z","hash":"ABC","files":[{"id":1,"name":"Release.mkv","absolute_path":"Release.mkv","size":100}]}}`,
		"array":  `{"success":true,"data":[{"id":17,"name":"Release.mkv","size":100,"progress":1,"download_state":"completed","download_finished":true,"created_at":"2026-01-02T03:04:05Z","hash":"ABC","files":[{"id":1,"name":"Release.mkv","absolute_path":"Release.mkv","size":100}]}]}`,
	}

	for name, body := range tests {
		t.Run(name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				_, _ = fmt.Fprint(w, body)
			}))
			t.Cleanup(server.Close)

			torrent, err := testTorbox(server.URL).GetTorrent("17")
			if err != nil {
				t.Fatalf("GetTorrent() error = %v", err)
			}
			if torrent.Id != "17" || torrent.InfoHash != "ABC" || len(torrent.Files) != 1 {
				t.Fatalf("GetTorrent() = %#v, want torrent 17 with one file", torrent)
			}
		})
	}
}

func TestUpdateTorrentFallsBackFromHashNameToFileRoot(t *testing.T) {
	const (
		hash = "e3d5d8bc1f7a4eb13bd62640229c63f3edfc71e8"
		root = "www.UIndex.org    -    Strange.Harvest.2025.1080p.WEB-DL.EAC3.H264-MG"
	)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprintf(w, `{"success":true,"data":{"id":147236436,"name":%q,"hash":%q,"size":5943949483,"progress":1,"download_state":"cached","download_finished":true,"files":[{"id":1,"name":%q,"absolute_path":%q,"size":5943949483}]}}`,
			hash, hash, root+"/Strange.Harvest.2025.1080p.WEB-DL.EAC3.H264-MG.mkv", "/completed/"+hash+"/"+root+"/Strange.Harvest.2025.1080p.WEB-DL.EAC3.H264-MG.mkv")
	}))
	t.Cleanup(server.Close)

	torrent := &types.Torrent{Id: "147236436", InfoHash: hash, Name: hash, SummaryOnly: true}
	if err := testTorbox(server.URL).UpdateTorrent(torrent); err != nil {
		t.Fatalf("UpdateTorrent() error = %v", err)
	}
	if torrent.Name != root || torrent.Filename != root || torrent.OriginalFilename != root {
		t.Fatalf("names = (%q, %q, %q), want file root %q", torrent.Name, torrent.Filename, torrent.OriginalFilename, root)
	}
	if torrent.SummaryOnly {
		t.Fatal("hydrated torrent must not remain summary-only")
	}
}

func testTorbox(host string) *Torbox {
	return &Torbox{
		Host:   host,
		client: request.New(request.WithMaxRetries(0)),
		logger: zerolog.Nop(),
		config: config.Debrid{Name: "torbox"},
	}
}
