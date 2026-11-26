package torbox

import (
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"syscall"
	"testing"

	"github.com/sirrobot01/decypharr/pkg/debrid/account"
	"github.com/sirrobot01/decypharr/pkg/debrid/types"
)

// Tests never dial real CDN hosts: every server is up unless a test says so.
func init() { cdnDial = func(string) error { return nil } }

func withDial(t *testing.T, down map[string]error) {
	t.Helper()
	old := cdnDial
	cdnMu.Lock()
	cdnState = map[string]cdnHostState{}
	cdnMu.Unlock()
	cdnDial = func(addr string) error {
		host, _, _ := net.SplitHostPort(addr)
		return down[host]
	}
	t.Cleanup(func() { cdnDial = old })
}

// TorBox's nearest CDN server refusing connections: the link is minted
// again as if from another region, and that region's server is used.
func TestFetchDownloadLinkSwitchesRegionWhenCDNIsDown(t *testing.T) {
	withDial(t, map[string]error{"nexus-405.ecan.tb-cdn.io": syscall.ECONNREFUSED})
	var asked []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ip := r.URL.Query().Get("user_ip")
		asked = append(asked, ip)
		host := "nexus-405.ecan.tb-cdn.io"
		if ip != "" {
			host = "nexus-336.wnam.tb-cdn.io"
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprintf(w, `{"success":true,"data":"https://%s/dld/x?token=y"}`, host)
	}))
	t.Cleanup(server.Close)

	dl, err := testTorbox(server.URL).fetchDownloadLink(&account.Account{Token: "key"}, "7", &types.File{Id: "3", Name: "a.mkv"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(dl.DownloadLink, "nexus-336.wnam") {
		t.Fatalf("DownloadLink = %q, want the wnam server", dl.DownloadLink)
	}
	if len(asked) != 2 || asked[0] != "" || asked[1] != cdnRegions[0].ip {
		t.Fatalf("requestdl user_ip = %q, want ours then %s", asked, cdnRegions[0].ip)
	}
}

// Every region down: the first link is kept (the reader's retries take over).
func TestFetchDownloadLinkKeepsFirstLinkWhenNoRegionWorks(t *testing.T) {
	refused := map[string]error{}
	for _, h := range []string{"a.tb-cdn.io", "b.tb-cdn.io"} {
		refused[h] = syscall.ECONNREFUSED
	}
	withDial(t, refused)
	var n int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n++
		host := "a.tb-cdn.io"
		if r.URL.Query().Get("user_ip") != "" {
			host = "b.tb-cdn.io"
		}
		_, _ = fmt.Fprintf(w, `{"success":true,"data":"https://%s/f"}`, host)
	}))
	t.Cleanup(server.Close)
	dl, err := testTorbox(server.URL).fetchDownloadLink(&account.Account{Token: "key"}, "7", &types.File{Id: "3"})
	if err != nil || dl.DownloadLink != "https://a.tb-cdn.io/f" {
		t.Fatalf("got %q, %v; want the first link", dl.DownloadLink, err)
	}
	if n != 1+len(cdnRegions) {
		t.Fatalf("requestdl calls = %d, want %d", n, 1+len(cdnRegions))
	}
}

func TestCDNReachable(t *testing.T) {
	withDial(t, map[string]error{
		"down.tb-cdn.io":  syscall.ECONNREFUSED,
		"nodns.tb-cdn.io": &net.DNSError{Name: "nodns.tb-cdn.io", IsNotFound: true},
	})
	if cdnReachable("https://down.tb-cdn.io/x") {
		t.Fatal("refused server counted as up")
	}
	if !cdnReachable("https://nodns.tb-cdn.io/x") {
		t.Fatal("a DNS failure is ours, not the server's")
	}
	if !cdnReachable("https://up.tb-cdn.io/x") {
		t.Fatal("working server counted as down")
	}
	MarkCDNDown("https://up.tb-cdn.io/y")
	if cdnReachable("https://up.tb-cdn.io/x") {
		t.Fatal("a server a reader marked down is skipped until rechecked")
	}
	if !IsCDNLink("https://nexus-405.ecan.tb-cdn.io/dld/x") || IsCDNLink("https://download.real-debrid.com/x") {
		t.Fatal("IsCDNLink")
	}
}
