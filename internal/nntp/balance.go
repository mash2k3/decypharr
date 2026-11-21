package nntp

import (
	"context"
	"sort"
	"strings"
	"sync"

	"github.com/sirrobot01/decypharr/internal/config"
)

// LoadBalanced reports whether the config asks for balanced primaries
// (Usenet.PrimaryLoadMode = "balanced"); anything else is fill-first.
func LoadBalanced(mode string) bool {
	return strings.EqualFold(strings.TrimSpace(mode), "balanced")
}

// balancedOrder returns the eligible providers of one tier, least loaded
// first (slots in use over max), starting the scan of equal loads at a
// rotating position so idle providers take turns. Providers the request's
// miss hint says keep lacking this file's articles go last: still asked,
// never skipped, so "missing on every provider" proof is unchanged.
func (c *Client) balancedOrder(ctx context.Context, exclusions providerExclusions, useBackups bool) []config.UsenetProvider {
	eligible := make([]config.UsenetProvider, 0, len(c.providers))
	for _, p := range c.providers {
		if p.Backup == useBackups && !exclusions.excludes(p) {
			eligible = append(eligible, p)
		}
	}
	if len(eligible) < 2 {
		return eligible
	}
	n := len(eligible)
	start := int(c.rr.Add(1) % uint64(n))
	rotated := make([]config.UsenetProvider, n)
	for i := range eligible {
		rotated[i] = eligible[(start+i)%n]
	}
	type load struct {
		busy, max int
		demoted   bool
	}
	hint := missHintFrom(ctx)
	loads := make([]load, n)
	for i, p := range rotated {
		pp := c.pools[p.Host]
		loads[i] = load{busy: len(pp.slots), max: max(pp.max, 1)}
		if hint != nil {
			loads[i].demoted = hint.demoted(p.Host)
		}
	}
	idx := make([]int, n)
	for i := range idx {
		idx[i] = i
	}
	sort.SliceStable(idx, func(a, b int) bool {
		la, lb := loads[idx[a]], loads[idx[b]]
		if la.demoted != lb.demoted {
			return lb.demoted
		}
		return la.busy*lb.max < lb.busy*la.max
	})
	out := make([]config.UsenetProvider, n)
	for i, j := range idx {
		out[i] = rotated[j]
	}
	return out
}

// missDemote: after this many "no such article" answers for one file, a
// provider is asked last for that file's segments (balanced mode only), so
// a shorter-retention primary stops costing a round trip per segment of an
// old post.
const missDemote = 3

// MissHint remembers, for one file, which providers (by host) didn't have
// its articles. Safe for concurrent use; the zero value is ready.
type MissHint struct {
	mu     sync.Mutex
	misses map[string]int
}

func (h *MissHint) miss(host string) {
	h.mu.Lock()
	if h.misses == nil {
		h.misses = map[string]int{}
	}
	h.misses[host]++
	h.mu.Unlock()
}

func (h *MissHint) hit(host string) {
	h.mu.Lock()
	if h.misses[host] > 0 {
		h.misses[host]--
	}
	h.mu.Unlock()
}

func (h *MissHint) demoted(host string) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.misses[host] >= missDemote
}

type missHintKey struct{}

// WithMissHint attaches a file's hint to the requests made with ctx.
func WithMissHint(ctx context.Context, h *MissHint) context.Context {
	if h == nil {
		return ctx
	}
	return context.WithValue(ctx, missHintKey{}, h)
}

func missHintFrom(ctx context.Context) *MissHint {
	h, _ := ctx.Value(missHintKey{}).(*MissHint)
	return h
}
