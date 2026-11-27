package torbox

import (
	"errors"
	"net"
	"net/url"
	"strings"
	"sync"
	"time"
)

// TorBox hands out links on the CDN server nearest to the requester. When
// that server is down (nexus-405/409.ecan.tb-cdn.io refused every
// connection for hours on 2026-11-26 while other regions served the same
// files), the link is asked for again as if from another region
// (requestdl's user_ip). A server's state is remembered for a few minutes,
// so a normal mint costs one quick TCP connect at most.

// cdnRegions are tried in order, with an address TorBox maps to each.
var cdnRegions = []struct{ region, ip string }{
	{"wnam", "8.8.8.8"}, {"weur", "1.1.1.1"}, {"nord", "95.216.0.1"},
}

const (
	cdnUpTTL       = 5 * time.Minute
	cdnDownTTL     = 2 * time.Minute
	cdnDialTimeout = 3 * time.Second
)

type cdnHostState struct {
	up      bool
	checked time.Time
}

var (
	cdnMu    sync.Mutex
	cdnState = map[string]cdnHostState{}
	// cdnDial is replaced in tests.
	cdnDial = func(addr string) error {
		c, err := net.DialTimeout("tcp", addr, cdnDialTimeout)
		if err == nil {
			c.Close()
		}
		return err
	}
)

func cdnHost(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return ""
	}
	return u.Hostname()
}

// cdnReachable: does the link's server take connections? A refusal, a dead
// route or a timeout is the server; a DNS failure is more likely ours, and
// switching regions wouldn't help, so it counts as reachable.
func cdnReachable(raw string) bool {
	u, err := url.Parse(raw)
	if err != nil || u.Hostname() == "" {
		return true
	}
	host := u.Hostname()
	now := time.Now()
	cdnMu.Lock()
	st, ok := cdnState[host]
	cdnMu.Unlock()
	if ok {
		ttl := cdnDownTTL
		if st.up {
			ttl = cdnUpTTL
		}
		if now.Sub(st.checked) < ttl {
			return st.up
		}
	}
	port := u.Port()
	if port == "" {
		port = "443"
		if u.Scheme == "http" {
			port = "80"
		}
	}
	err = cdnDial(net.JoinHostPort(host, port))
	var dns *net.DNSError
	up := err == nil || errors.As(err, &dns)
	cdnMu.Lock()
	cdnState[host] = cdnHostState{up: up, checked: now}
	cdnMu.Unlock()
	return up
}

// cdnMarkDown: a reader found the server refusing connections; the next
// mint skips it without waiting for the cached state to expire.
func cdnMarkDown(raw string) {
	if h := cdnHost(raw); h != "" {
		cdnMu.Lock()
		cdnState[h] = cdnHostState{up: false, checked: time.Now()}
		cdnMu.Unlock()
	}
}

// IsCDNLink: the URL is on TorBox's CDN.
func IsCDNLink(raw string) bool {
	h := cdnHost(raw)
	return h == "tb-cdn.io" || strings.HasSuffix(h, ".tb-cdn.io") || h == "tb-cdn.st" || strings.HasSuffix(h, ".tb-cdn.st")
}

// MarkCDNDown is cdnMarkDown for readers outside this package.
func MarkCDNDown(raw string) { cdnMarkDown(raw) }
