package manager

import (
	"context"
	"net"
	"strings"
	"sync"
	"time"
)

// TorBox per-host connection cap (Ether's conncap, as in cli_debrid's
// built-in engine): TorBox's CDN counts connections, not requests,
// answering the 21st open connection to one host with HTTP 429 and a
// five-minute pause for that whole host, idle keep-alive connections
// included. The stream transport keeps up to 500 idle connections per
// host, so a library scan plus a few players could trip it. Dials to a
// TorBox CDN host wait for a free connection instead (after asking the
// transport to drop idle ones). Other hosts, and dials through a proxy
// (the dial address is then the proxy's), are not capped.

// torboxConnsPerHost: open connections allowed per TorBox CDN host.
const torboxConnsPerHost = 16

// torboxHost reports whether host is TorBox's CDN. A var for tests.
var torboxHost = func(host string) bool {
	host = strings.ToLower(host)
	for _, d := range []string{"tb-cdn.st", "torbox.app"} {
		if host == d || strings.HasSuffix(host, "."+d) {
			return true
		}
	}
	return false
}

type hostConnCap struct {
	mu        sync.Mutex
	open      map[string]int
	wake      chan struct{}
	limit     int
	closeIdle func()
}

func newHostConnCap(limit int) *hostConnCap {
	return &hostConnCap{open: map[string]int{}, wake: make(chan struct{}), limit: limit}
}

func (c *hostConnCap) acquire(ctx context.Context, addr string) error {
	asked := false
	for {
		c.mu.Lock()
		if c.open[addr] < c.limit {
			c.open[addr]++
			c.mu.Unlock()
			return nil
		}
		wake, closeIdle := c.wake, c.closeIdle
		c.mu.Unlock()
		if !asked && closeIdle != nil {
			closeIdle() // idle keep-alive connections count against the limit
			asked = true
			continue
		}
		t := time.NewTimer(time.Second)
		select {
		case <-wake:
		case <-t.C:
			asked = false
		case <-ctx.Done():
			t.Stop()
			return ctx.Err()
		}
		t.Stop()
	}
}

func (c *hostConnCap) release(addr string) {
	c.mu.Lock()
	if c.open[addr] > 0 {
		c.open[addr]--
	}
	if c.open[addr] == 0 {
		delete(c.open, addr)
	}
	close(c.wake)
	c.wake = make(chan struct{})
	c.mu.Unlock()
}

func (c *hostConnCap) inUse(addr string) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.open[addr]
}

func (c *hostConnCap) wrap(next func(ctx context.Context, network, addr string) (net.Conn, error)) func(ctx context.Context, network, addr string) (net.Conn, error) {
	return func(ctx context.Context, network, addr string) (net.Conn, error) {
		if host, _, err := net.SplitHostPort(addr); err != nil || !torboxHost(host) {
			return next(ctx, network, addr)
		}
		if err := c.acquire(ctx, addr); err != nil {
			return nil, err
		}
		conn, err := next(ctx, network, addr)
		if err != nil {
			c.release(addr)
			return nil, err
		}
		return &cappedConn{Conn: conn, release: func() { c.release(addr) }}, nil
	}
}

type cappedConn struct {
	net.Conn
	once    sync.Once
	release func()
}

func (c *cappedConn) Close() error {
	err := c.Conn.Close()
	c.once.Do(c.release)
	return err
}
