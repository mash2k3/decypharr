package nntp

import (
	"bufio"
	"io"
	"net"
	"testing"
	"time"

	"github.com/sirrobot01/decypharr/internal/utils"
)

// newSilentPipeConnection is a peer that never answers. readLines=false also
// never reads, so the client's write blocks (net.Pipe has no buffer).
func newSilentPipeConnection(t *testing.T, readLines bool) *Connection {
	t.Helper()
	clientSide, serverSide := net.Pipe()
	c := &Connection{
		conn:   clientSide,
		reader: bufio.NewReader(clientSide),
		writer: bufio.NewWriter(clientSide),
	}
	t.Cleanup(func() { _ = c.Close(); _ = serverSide.Close() })
	if readLines {
		go func() { _, _ = io.Copy(io.Discard, serverSide) }()
	}
	return c
}

// Deadlines come from utils.Now(), a cached clock refreshed every 500ms once
// started (the app starts it at boot). Budgets here stay well above that.
func withKeepaliveTimeout(t *testing.T, d time.Duration) {
	t.Helper()
	utils.StartGlobalCachedTime()
	saved := timeouts
	timeouts.KeepalivePingTimeout = d
	t.Cleanup(func() { timeouts = saved })
}

// sendCommandArg used to reset the write deadline to HandshakeTimeout (10s),
// so a peer that stopped reading blocked a "1.5s" ping for 10s.
func TestPingWriteHonoursPingBudget(t *testing.T) {
	utils.StartGlobalCachedTime()
	c := newSilentPipeConnection(t, false)
	start := time.Now()
	err := c.ping(time.Second)
	if err == nil {
		t.Fatal("expected ping to fail against a peer that never reads")
	}
	if elapsed := time.Since(start); elapsed > 3*time.Second {
		t.Fatalf("ping took %s, want about its 1s budget, not the 10s handshake timeout", elapsed)
	}
	if !isTimeoutLike(err) {
		t.Fatalf("expected a timeout error, got %v", err)
	}
}

// With no live replies, the first keepalive timeout means the provider path
// is down: the rest of the batch is closed unpinged and the idle pool flushed,
// instead of the reaper waiting out every connection in turn.
func TestReaperFlushesPoolOnDeadPath(t *testing.T) {
	withKeepaliveTimeout(t, time.Second)
	pp := newTestPool(8)
	for i := 0; i < 5; i++ {
		poolEntry(pp, newSilentPipeConnection(t, true), 40*time.Second)
	}
	c := newReaperTestClient(pp)

	start := time.Now()
	c.reapIdleConnections()
	elapsed := time.Since(start)

	// One keepalive budget, not five in a row.
	if elapsed > 2500*time.Millisecond {
		t.Fatalf("reaper took %s; it pinged every dead connection instead of condemning the path", elapsed)
	}
	if n := len(pp.conns); n != 0 {
		t.Fatalf("pool has %d idle connections, want 0 after flush", n)
	}
	if n := len(pp.slots); n != 0 {
		t.Fatalf("%d slots still held, want all released", n)
	}
}

// A timeout after another connection answered is one wedged session, not an
// outage: only that connection goes, the healthy one stays.
func TestReaperWedgedSessionKeepsPool(t *testing.T) {
	withKeepaliveTimeout(t, time.Second)
	pp := newTestPool(8)
	healthy := newPipeConnection(t, true)
	poolEntry(pp, healthy, 40*time.Second)
	poolEntry(pp, newSilentPipeConnection(t, true), 40*time.Second)
	c := newReaperTestClient(pp)

	c.reapIdleConnections()

	if len(pp.conns) != 1 || pp.conns[0].conn != healthy {
		t.Fatalf("want only the healthy connection kept, got %d entries", len(pp.conns))
	}
	if n := len(pp.slots); n != 0 {
		t.Fatalf("%d slots still held, want all released", n)
	}
}
