package reader

import (
	"bufio"
	"context"
	"net"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/rs/zerolog"
	"github.com/sirrobot01/decypharr/internal/config"
	"github.com/sirrobot01/decypharr/internal/nntp"
)

// stallFirstNNTP never answers its first BODY (a stalled connection);
// every later BODY gets body.
func stallFirstNNTP(t *testing.T, body []byte) (string, int, *atomic.Int64) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	article := yencArticle(body)
	var bodies atomic.Int64
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer c.Close()
				_, _ = c.Write([]byte("200 ready\r\n"))
				r := bufio.NewReader(c)
				for {
					line, err := r.ReadString('\n')
					if err != nil {
						return
					}
					if strings.HasPrefix(strings.ToUpper(line), "BODY") {
						if bodies.Add(1) == 1 {
							_, _ = r.ReadString('\n') // hang until the client closes
							return
						}
						_, _ = c.Write([]byte("222 0 <x> body\r\n" + article))
						continue
					}
					_, _ = c.Write([]byte("200 ok\r\n"))
				}
			}(c)
		}
	}()
	a := ln.Addr().(*net.TCPAddr)
	return a.IP.String(), a.Port, &bodies
}

func newHedgeFetcher(t *testing.T, host string, port int) *SegmentFetcher {
	t.Helper()
	cfg := &config.Config{}
	cfg.Usenet.Providers = []config.UsenetProvider{{Host: host, Port: port, MaxConnections: 4}}
	client, err := nntp.NewClient(cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = client.Close() })
	segs := []SegmentMeta{{MessageID: "<a@test>", Number: 1, Bytes: 1000, StartOffset: 0, EndOffset: 999}}
	rcfg := DefaultConfig()
	rcfg.DiskPath = t.TempDir()
	rcfg.MaxConnections = 3
	stats := &ReaderStats{}
	cache, err := NewSegmentCache(context.Background(), segs, rcfg, stats, zerolog.Nop())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cache.Close() })
	sf := NewSegmentFetcher(context.Background(), client, cache, rcfg, stats, zerolog.Nop())
	t.Cleanup(sf.Close)
	return sf
}

// A playback read stuck behind a stalled prefetch cancels it after
// hedgeAfter and gets the segment itself.
func TestPlaybackHedgesStalledPrefetch(t *testing.T) {
	oldDef := hedgeDefault
	hedgeDefault = 100 * time.Millisecond
	defer func() { hedgeDefault = oldDef; fetchNs.Store(0) }()
	fetchNs.Store(0)
	host, port, bodies := stallFirstNNTP(t, fill(1000, 'h'))
	sf := newHedgeFetcher(t, host, port)

	bg := nntp.WithWorkClass(context.Background(), nntp.WorkClassBackground)
	go sf.Fetch(bg, 0)
	deadline := time.Now().Add(2 * time.Second)
	for bodies.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	start := time.Now()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := sf.Fetch(ctx, 0); err != nil {
		t.Fatalf("playback fetch: %v", err)
	}
	if took := time.Since(start); took > time.Second {
		t.Fatalf("playback waited %s on a stalled prefetch", took)
	}
	if sf.stats.Hedges.Load() != 1 {
		t.Fatalf("hedges = %d, want 1", sf.stats.Hedges.Load())
	}
	dst := make([]byte, 1000)
	if n, ok := sf.cache.ReadRangeInto(0, 0, 1000, dst); !ok || n != 1000 || dst[0] != 'h' {
		t.Fatalf("segment not readable after the hedge (n=%d ok=%v)", n, ok)
	}
}
