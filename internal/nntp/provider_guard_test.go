package nntp

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"net"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sirrobot01/decypharr/internal/config"
)

// guardServer answers greeting with greet (one line) and AUTHINFO PASS with
// pass; any other command gets "223 0 <x>" (a STAT-style success).
type guardServer struct {
	host    string
	port    int
	accepts atomic.Int64
	greet   atomic.Value // string
	pass    atomic.Value // string
}

func startGuardServer(t *testing.T, greet, pass string) *guardServer {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	s := &guardServer{}
	s.greet.Store(greet)
	s.pass.Store(pass)
	a := ln.Addr().(*net.TCPAddr)
	s.host, s.port = a.IP.String(), a.Port
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			s.accepts.Add(1)
			go func(c net.Conn) {
				defer c.Close()
				g := s.greet.Load().(string)
				_, _ = c.Write([]byte(g + "\r\n"))
				if !strings.HasPrefix(g, "200") {
					return
				}
				r := bufio.NewReader(c)
				for {
					line, err := r.ReadString('\n')
					if err != nil {
						return
					}
					u := strings.ToUpper(line)
					switch {
					case strings.HasPrefix(u, "AUTHINFO USER"):
						_, _ = c.Write([]byte("381 more\r\n"))
					case strings.HasPrefix(u, "AUTHINFO PASS"):
						_, _ = c.Write([]byte(s.pass.Load().(string) + "\r\n"))
					default:
						_, _ = c.Write([]byte("223 0 <x>\r\n"))
					}
				}
			}(c)
		}
	}()
	return s
}

func guardClient(t *testing.T, providers ...config.UsenetProvider) *Client {
	t.Helper()
	cfg := &config.Config{}
	cfg.Usenet.Providers = providers
	c, err := NewClient(cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c
}

func withGuardTimes(t *testing.T, auth, conn time.Duration) {
	t.Helper()
	oa, oam, oc, ocm := authMin, authMax, connMin, connMax
	authMin, authMax, connMin, connMax = auth, 4*auth, conn, 4*conn
	t.Cleanup(func() { authMin, authMax, connMin, connMax = oa, oam, oc, ocm })
}

func stat(c *Client) error {
	return c.ExecuteWithFailover(context.Background(), func(conn *Connection) error {
		_, _, err := conn.Stat("<x@test>")
		return err
	})
}

// A refused login pauses the provider: later requests go straight to the
// other one (a backup here) instead of logging in and failing each time.
func TestRefusedLoginPausesProviderAndUsesBackup(t *testing.T) {
	withGuardTimes(t, time.Hour, time.Hour)
	bad := startGuardServer(t, "200 ready", "481 authentication rejected")
	good := startGuardServer(t, "200 ready", "281 ok")
	c := guardClient(t,
		config.UsenetProvider{Host: bad.host, Port: bad.port, Username: "u", Password: "p", MaxConnections: 4},
		config.UsenetProvider{Host: "localhost", Port: good.port, Username: "u", Password: "p", MaxConnections: 4, Backup: true})
	for i := 0; i < 6; i++ {
		if err := stat(c); err != nil {
			t.Fatalf("request %d: %v", i, err)
		}
	}
	if n := bad.accepts.Load(); n != 1 {
		t.Fatalf("expired provider dialed %d times for 6 requests, want 1", n)
	}
	state, retry, _, _, _ := c.pools[bad.host].guardState()
	if state != "auth_failed" || retry <= 0 {
		t.Fatalf("state %q retry %s, want auth_failed with a retry", state, retry)
	}
}

// A provider that keeps failing to connect is paused after connFailures,
// probed after the backoff and used again once it works.
func TestUnreachableProviderPausedThenProbed(t *testing.T) {
	withGuardTimes(t, time.Hour, 150*time.Millisecond)
	flaky := startGuardServer(t, "500 broken", "281 ok")
	good := startGuardServer(t, "200 ready", "281 ok")
	c := guardClient(t,
		config.UsenetProvider{Host: flaky.host, Port: flaky.port, MaxConnections: 1},
		config.UsenetProvider{Host: "localhost", Port: good.port, MaxConnections: 4})
	for i := 0; i < 8; i++ {
		if err := stat(c); err != nil {
			t.Fatalf("request %d: %v", i, err)
		}
	}
	if n := flaky.accepts.Load(); n != int64(connFailures) {
		t.Fatalf("failing provider dialed %d times, want %d before pausing", n, connFailures)
	}
	flaky.greet.Store("200 ready")
	time.Sleep(200 * time.Millisecond)
	before := flaky.accepts.Load()
	for i := 0; i < 3 && flaky.accepts.Load() == before; i++ {
		_ = stat(c)
	}
	if flaky.accepts.Load() == before {
		t.Fatal("recovered provider not probed after its backoff")
	}
	if state, _, _, _, _ := c.pools[flaky.host].guardState(); state != "ok" {
		t.Fatalf("state %q after a working probe, want ok", state)
	}
}

// With every provider paused the request fails at once as pool contention,
// never as a missing article.
func TestAllPausedFailsFastNotProof(t *testing.T) {
	withGuardTimes(t, time.Hour, time.Hour)
	bad := startGuardServer(t, "200 ready", "481 authentication rejected")
	c := guardClient(t, config.UsenetProvider{Host: bad.host, Port: bad.port, Username: "u", Password: "p", MaxConnections: 2})
	_ = stat(c)
	start := time.Now()
	err := stat(c)
	if err == nil || IsArticleNotFoundError(err) {
		t.Fatalf("err = %v, want a non-proof failure", err)
	}
	if time.Since(start) > 200*time.Millisecond || bad.accepts.Load() != 1 {
		t.Fatalf("paused provider still dialed (%d accepts, %s)", bad.accepts.Load(), time.Since(start))
	}
}

// "Too many connections" parks the slots above what the server carried;
// a stated limit is learned (minus 10%) and kept.
func TestBusyRefusalCapsPool(t *testing.T) {
	pp := &ProviderPool{slots: make(chan struct{}, 10), max: 10, closed: make(chan struct{})}
	defer close(pp.closed)
	for i := 0; i < 5; i++ { // 4 connections open + the refused dial's slot
		pp.slots <- struct{}{}
	}
	pp.throttle(fmt.Errorf("unexpected greeting: 502 connection limit (8) reached"))
	pp.adjustParking()
	_, _, learned, capNow, parked := pp.guardState()
	if learned != 8 { // 8 minus 10% rounds to 8
		t.Fatalf("learned %d, want 8", learned)
	}
	if capNow != 4 || parked != 5 {
		t.Fatalf("cap %d parked %d, want 4 and 5 (10 - 4, the refused slot is still held)", capNow, parked)
	}
	// After the hold the cap widens; tokens are given back.
	pp.guard.mu.Lock()
	pp.guard.capUntil = time.Now().Add(-time.Hour)
	pp.guard.mu.Unlock()
	<-pp.slots // the refused dial's slot is released by its caller
	pp.adjustParking()
	_, _, _, capNow, parked = pp.guardState()
	if capNow != 8 || parked != 2 {
		t.Fatalf("after widening cap %d parked %d, want the learned 8 and 2 parked", capNow, parked)
	}
}

func TestClassifyDialError(t *testing.T) {
	for name, tc := range map[string]struct {
		err  error
		want dialKind
	}{
		"481 login":       {fmt.Errorf("auth: %w", classifyNNTPError(481, "rejected")), dialAuth},
		"502 too many":    {fmt.Errorf("auth: %w", classifyNNTPError(502, "Too many connections")), dialBusy},
		"greeting busy":   {NewConnectionError(errors.New("unexpected greeting: 502 Max connections reached")), dialBusy},
		"greeting 400":    {NewConnectionError(errors.New("unexpected greeting: 400 unavailable")), dialBusy},
		"greeting denied": {NewConnectionError(errors.New("unexpected greeting: 502 access denied")), dialAuth},
		"dial refused":    {NewConnectionError(errors.New("dial x: connection refused")), dialOther},
		"unexpected 500":  {NewConnectionError(errors.New("unexpected greeting: 500 broken")), dialOther},
	} {
		if got := classifyDialError(tc.err, nil); got != tc.want {
			t.Errorf("%s: got %d, want %d", name, got, tc.want)
		}
	}
	if classifyDialError(errors.New("x"), context.Canceled) != dialGaveUp {
		t.Error("a cancelled dial must say nothing")
	}
}

func TestStatedLimitGuard(t *testing.T) {
	for msg, want := range map[string]int{
		"502 Too many connections":                      0,
		"481 Max connections exceeded (ip 10.0.0.1)":    0,
		"502 connection limit (8) reached":              8,
		"502 you have 25 connections open, limit is 20": 20,
		"481 only 10 simultaneous connections allowed":  10,
	} {
		if got := statedLimit(msg); got != want {
			t.Errorf("statedLimit(%q) = %d, want %d", msg, got, want)
		}
	}
}
