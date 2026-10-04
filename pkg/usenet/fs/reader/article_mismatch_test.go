package reader

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"net"
	"strings"
	"testing"

	"github.com/rs/zerolog"
	"github.com/sirrobot01/decypharr/internal/config"
	"github.com/sirrobot01/decypharr/internal/nntp"
)

func yencArticle(data []byte) string {
	var b strings.Builder
	fmt.Fprintf(&b, "=ybegin line=128 size=%d name=test.bin\r\n", len(data))
	col := 0
	for _, v := range data {
		c := v + 42
		if c == 0 || c == '\n' || c == '\r' || c == '=' || c == '\t' || c == ' ' || c == '.' {
			b.WriteByte('=')
			c += 64
			col++
		}
		b.WriteByte(c)
		col++
		if col >= 128 {
			b.WriteString("\r\n")
			col = 0
		}
	}
	fmt.Fprintf(&b, "\r\n=yend size=%d\r\n.\r\n", len(data))
	return b.String()
}

// fakeNNTP serves body for every BODY command.
func fakeNNTP(t *testing.T, body []byte) (string, int) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	article := yencArticle(body)
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
					cmd := strings.ToUpper(strings.TrimSpace(line))
					switch {
					case strings.HasPrefix(cmd, "AUTHINFO USER"):
						_, _ = c.Write([]byte("381 more\r\n"))
					case strings.HasPrefix(cmd, "AUTHINFO PASS"):
						_, _ = c.Write([]byte("281 ok\r\n"))
					case strings.HasPrefix(cmd, "BODY"):
						_, _ = c.Write([]byte("222 0 <x> body\r\n" + article))
					case strings.HasPrefix(cmd, "QUIT"):
						_, _ = c.Write([]byte("205 bye\r\n"))
						return
					default:
						_, _ = c.Write([]byte("200 ok\r\n"))
					}
				}
			}(c)
		}
	}()
	addr := ln.Addr().(*net.TCPAddr)
	return addr.IP.String(), addr.Port
}

func newMismatchFetcher(t *testing.T, segBytes int64, bodies ...[]byte) *SegmentFetcher {
	t.Helper()
	var providers []config.UsenetProvider
	for i, body := range bodies {
		host, port := fakeNNTP(t, body)
		// Pools are keyed by host, so give each fake provider its own name.
		if i == 1 {
			host = "localhost"
		}
		providers = append(providers, config.UsenetProvider{
			Host: host, Port: port, Username: "u", Password: "p",
			MaxConnections: 2, Priority: i,
		})
	}
	cfg := &config.Config{}
	cfg.Usenet.Providers = providers
	client, err := nntp.NewClient(cfg)
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	t.Cleanup(func() { _ = client.Close() })

	segs := []SegmentMeta{{MessageID: "<a@test>", Number: 1, Bytes: segBytes, StartOffset: 0, EndOffset: segBytes - 1}}
	rcfg := DefaultConfig()
	rcfg.DiskPath = t.TempDir()
	rcfg.MaxConnections = 1
	stats := &ReaderStats{}
	cache, err := NewSegmentCache(context.Background(), segs, rcfg, stats, zerolog.Nop())
	if err != nil {
		t.Fatalf("NewSegmentCache: %v", err)
	}
	t.Cleanup(func() { _ = cache.Close() })
	sf := NewSegmentFetcher(context.Background(), client, cache, rcfg, stats, zerolog.Nop())
	t.Cleanup(sf.Close)
	return sf
}

func fill(n int, v byte) []byte { return bytes.Repeat([]byte{v}, n) }

// Two providers can hold different posts under one Message-ID. The one
// serving an article too short for the segment must be skipped for the one
// with the real article, not committed as the segment's data.
func TestFetchSkipsProviderServingShortArticle(t *testing.T) {
	sf := newMismatchFetcher(t, 1000, fill(600, 'w'), fill(1000, 'r'))
	if err := sf.Fetch(context.Background(), 0); err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if got := sf.cache.segLengths[0].Load(); got != 1000 {
		t.Fatalf("committed %d bytes, want the real 1000-byte article", got)
	}
	dst := make([]byte, 1000)
	n, ok := sf.cache.ReadRangeInto(0, 0, 1000, dst)
	if !ok || n != 1000 || dst[0] != 'r' || dst[999] != 'r' {
		t.Fatalf("read %d ok=%v first=%q; want the real article's data", n, ok, dst[0])
	}
	if sf.acceptShortArticles.Load() {
		t.Fatal("acceptShortArticles set although a provider had the full article")
	}
}

// When every provider serves the same short article the layout overstates
// it (an estimated size); accept it so the layout logic can deal with it,
// never fail the segment as missing.
func TestFetchAcceptsShortArticleWhenEveryProviderAgrees(t *testing.T) {
	sf := newMismatchFetcher(t, 1000, fill(600, 's'))
	if err := sf.Fetch(context.Background(), 0); err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if got := sf.cache.segLengths[0].Load(); got != 600 {
		t.Fatalf("committed %d bytes, want 600", got)
	}
	if !sf.acceptShortArticles.Load() {
		t.Fatal("acceptShortArticles not set after every provider served a short article")
	}
}
