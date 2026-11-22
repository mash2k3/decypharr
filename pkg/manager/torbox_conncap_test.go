package manager

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestTorboxHost(t *testing.T) {
	for host, want := range map[string]bool{
		"store-1.tb-cdn.st": true, "STORE-031.WEUR.TB-CDN.ST": true, "torbox.app": true,
		"download.real-debrid.com": false, "notb-cdn.st": false, "tb-cdn.st.evil.com": false,
	} {
		if got := torboxHost(host); got != want {
			t.Errorf("torboxHost(%q) = %v, want %v", host, got, want)
		}
	}
}

func capClient(c *hostConnCap) *http.Client {
	tr := &http.Transport{DialContext: c.wrap((&net.Dialer{}).DialContext)}
	c.closeIdle = tr.CloseIdleConnections
	return &http.Client{Transport: tr}
}

// TorBox hosts never get more than the cap open; others are not held back.
func TestTorboxConnCap(t *testing.T) {
	var active, peak atomic.Int64
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := active.Add(1)
		for p := peak.Load(); n > p && !peak.CompareAndSwap(p, n); p = peak.Load() {
		}
		<-release
		active.Add(-1)
	}))
	defer srv.Close()
	run := func(isTorbox bool) int64 {
		old := torboxHost
		torboxHost = func(string) bool { return isTorbox }
		defer func() { torboxHost = old }()
		active.Store(0)
		peak.Store(0)
		c := newHostConnCap(2)
		client := capClient(c)
		var wg sync.WaitGroup
		for i := 0; i < 5; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
				defer cancel()
				req, _ := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL, nil)
				if resp, err := client.Do(req); err == nil {
					resp.Body.Close()
				}
			}()
		}
		time.Sleep(300 * time.Millisecond)
		got := peak.Load()
		if isTorbox {
			if n := c.inUse(strings.TrimPrefix(srv.URL, "http://")); n != 2 {
				t.Errorf("%d connections counted, want 2", n)
			}
		}
		for i := 0; i < 5; i++ {
			release <- struct{}{}
		}
		wg.Wait()
		return got
	}
	if p := run(true); p != 2 {
		t.Fatalf("TorBox host saw %d at once, want the cap of 2", p)
	}
	if p := run(false); p != 5 {
		t.Fatalf("non-TorBox host saw %d at once, want all 5", p)
	}
}
