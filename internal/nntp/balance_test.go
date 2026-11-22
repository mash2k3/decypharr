package nntp

import (
	"context"
	"errors"
	"testing"

	"github.com/sirrobot01/decypharr/internal/config"
)

func TestLoadBalancedParse(t *testing.T) {
	for in, want := range map[string]bool{"": false, "fill": false, "balanced": true, " Balanced ": true, "junk": false} {
		if got := LoadBalanced(in); got != want {
			t.Errorf("LoadBalanced(%q) = %v, want %v", in, got, want)
		}
	}
}

func countServed(t *testing.T, client *Client, owners map[*Connection]string, n int) map[string]int {
	t.Helper()
	served := map[string]int{}
	for range n {
		err := client.ExecuteWithFailover(context.Background(), func(conn *Connection) error {
			served[owners[conn]]++
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	return served
}

// Fill first (default): an idle client always uses the highest-priority
// primary. Balanced: idle primaries take turns; the backup is never used.
func TestBalancedSpreadsOverPrimaries(t *testing.T) {
	providers := []config.UsenetProvider{
		{Host: "a.example.com", MaxConnections: 1, Priority: 1},
		{Host: "b.example.com", MaxConnections: 1, Priority: 2},
		{Host: "c.example.com", MaxConnections: 1, Priority: 3},
		{Host: "backup.example.com", MaxConnections: 1, Priority: 4, Backup: true},
	}
	client, owners := failoverTestClient(t, providers)
	if s := countServed(t, client, owners, 30); s["a.example.com"] != 30 {
		t.Fatalf("fill first: %v, want everything on a", s)
	}

	client, owners = failoverTestClient(t, providers)
	client.balanced = true
	s := countServed(t, client, owners, 30)
	if s["backup.example.com"] != 0 {
		t.Fatalf("balanced used the backup: %v", s)
	}
	for _, h := range []string{"a.example.com", "b.example.com", "c.example.com"} {
		if s[h] < 8 {
			t.Fatalf("balanced: %v, want each primary about 10", s)
		}
	}
}

// The busier primary (slots in use over max) goes after the idle one.
func TestBalancedOrderPrefersLeastLoaded(t *testing.T) {
	providers := []config.UsenetProvider{
		{Host: "a.example.com", MaxConnections: 1},
		{Host: "b.example.com", MaxConnections: 1},
	}
	client, _ := failoverTestClient(t, providers)
	client.balanced = true
	client.pools["a.example.com"].slots <- struct{}{} // a fully busy
	for range 5 {
		order := client.balancedOrder(context.Background(), providerExclusions{}, false)
		if order[0].Host != "b.example.com" {
			t.Fatalf("order %v: the idle provider should come first", order)
		}
	}
}

// Balanced never reorders backups: once the primary lacks an article the
// highest-priority backup is asked every time, not a share by load.
func TestBalancedKeepsBackupOrder(t *testing.T) {
	providers := []config.UsenetProvider{
		{Host: "a.example.com", MaxConnections: 1, Priority: 1},
		{Host: "b1.example.com", MaxConnections: 1, Priority: 2, Backup: true},
		{Host: "b2.example.com", MaxConnections: 1, Priority: 3, Backup: true},
	}
	client, owners := failoverTestClient(t, providers)
	client.balanced = true
	served := map[string]int{}
	for range 20 {
		err := client.ExecuteWithFailover(context.Background(), func(conn *Connection) error {
			if owners[conn] == "a.example.com" {
				return classifyNNTPError(430, "No such article")
			}
			served[owners[conn]]++
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	if served["b1.example.com"] != 20 || served["b2.example.com"] != 0 {
		t.Fatalf("backups served %v, want all on b1", served)
	}
}

// Balanced only changes who is asked first: an article missing on every
// provider is still proof, and a transient failure anywhere still isn't.
func TestBalancedKeepsProofRules(t *testing.T) {
	providers := []config.UsenetProvider{
		{Host: "a.example.com", MaxConnections: 1},
		{Host: "b.example.com", MaxConnections: 1},
		{Host: "backup.example.com", MaxConnections: 1, Backup: true},
	}
	client, owners := failoverTestClient(t, providers)
	client.balanced = true
	for range 4 {
		asked := map[string]int{}
		err := client.ExecuteWithFailover(context.Background(), func(conn *Connection) error {
			asked[owners[conn]]++
			return classifyNNTPError(430, "No such article")
		})
		if !IsArticleNotFoundError(err) {
			t.Fatalf("err = %v, want article not found", err)
		}
		if len(asked) != 3 {
			t.Fatalf("asked %v, want every provider incl. the backup", asked)
		}
	}
	err := client.ExecuteWithFailover(context.Background(), func(conn *Connection) error {
		if owners[conn] == "backup.example.com" {
			return NewTimeoutError(errors.New("read deadline"))
		}
		return classifyNNTPError(430, "No such article")
	})
	if err == nil || IsArticleNotFoundError(err) {
		t.Fatalf("err = %v, want transient", err)
	}
}

// A primary that keeps lacking one file's articles is asked last for that
// file, but still asked when the others lack one too.
func TestMissHintAsksShortRetentionProviderLast(t *testing.T) {
	providers := []config.UsenetProvider{
		{Host: "short.example.com", MaxConnections: 1},
		{Host: "full.example.com", MaxConnections: 1},
	}
	client, owners := failoverTestClient(t, providers)
	client.balanced = true
	var hint MissHint
	ctx := WithMissHint(context.Background(), &hint)
	shortAsked := 0
	for range 20 {
		err := client.ExecuteWithFailover(ctx, func(conn *Connection) error {
			if owners[conn] == "short.example.com" {
				shortAsked++
				return classifyNNTPError(430, "No such article")
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	if shortAsked > missDemote {
		t.Fatalf("short-retention provider asked %d times for one file, want at most %d", shortAsked, missDemote)
	}
	before := shortAsked
	err := client.ExecuteWithFailover(ctx, func(conn *Connection) error {
		if owners[conn] == "short.example.com" {
			shortAsked++
		}
		return classifyNNTPError(430, "No such article")
	})
	if !IsArticleNotFoundError(err) || shortAsked != before+1 {
		t.Fatalf("err=%v asked=%d: demoted provider must still be asked for proof", err, shortAsked-before)
	}
}

// A retry that moved from a timed-out provider to another one, which then
// said "no such article", is not proof: the first never answered. Before,
// the not-found excluded the first provider (connProvider) instead of the
// one that answered, and the result came back as article-not-found.
func TestRetrySwitchKeepsResultTransient(t *testing.T) {
	providers := []config.UsenetProvider{
		{Host: "a.example.com", MaxConnections: 1},
		{Host: "b.example.com", MaxConnections: 1},
	}
	client, owners := failoverTestClient(t, providers)
	client.retries = 1
	err := client.ExecuteWithFailover(context.Background(), func(conn *Connection) error {
		if owners[conn] == "a.example.com" {
			return NewTimeoutError(errors.New("read deadline"))
		}
		return classifyNNTPError(430, "No such article")
	})
	if err == nil || IsArticleNotFoundError(err) {
		t.Fatalf("err = %v, want transient (a timed out, never answered)", err)
	}
}
