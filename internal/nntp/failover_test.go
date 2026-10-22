package nntp

import (
	"context"
	"errors"
	"net"
	"testing"
	"time"

	"github.com/rs/zerolog"
	"github.com/sirrobot01/decypharr/internal/config"
)

// failoverTestClient builds a client whose pools each hold one idle
// connection, and returns which provider host owns each connection so the
// operation under test can fail per provider.
func failoverTestClient(t *testing.T, providers []config.UsenetProvider) (*Client, map[*Connection]string) {
	t.Helper()
	client := &Client{
		providers:      providers,
		pools:          make(map[string]*ProviderPool),
		logger:         zerolog.Nop(),
		idleTimeout:    time.Hour,
		staleThreshold: time.Hour,
	}
	owners := make(map[*Connection]string)
	for _, provider := range providers {
		clientSide, serverSide := net.Pipe()
		t.Cleanup(func() {
			_ = clientSide.Close()
			_ = serverSide.Close()
		})
		conn := &Connection{conn: clientSide, logger: zerolog.Nop()}
		owners[conn] = provider.Host
		client.pools[provider.Host] = &ProviderPool{
			slots:  make(chan struct{}, 1),
			max:    1,
			config: provider,
			conns:  []*connectionEntry{{conn: conn, provider: provider, lastUsed: time.Now()}},
		}
	}
	return client, owners
}

func TestExecuteWithFailoverKeepsNotFoundWhenBackboneExcluded(t *testing.T) {
	// One not-found excludes the shared backbone, so the second loop pass
	// finds no eligible provider. That must not replace the real 430 with
	// the pool-contention error, or the read is never treated as missing.
	providers := []config.UsenetProvider{
		{Host: "a.example.com", Backbone: "omicron", MaxConnections: 1},
		{Host: "b.example.com", Backbone: "omicron", MaxConnections: 1},
	}
	client, _ := failoverTestClient(t, providers)

	err := client.ExecuteWithFailover(context.Background(), func(conn *Connection) error {
		return classifyNNTPError(430, "No such article")
	})
	if !IsArticleNotFoundError(err) {
		t.Fatalf("err = %v, want article not found", err)
	}
	if IsPoolCapacityError(err) {
		t.Fatalf("err = %v, must not read as pool contention", err)
	}
}

func TestExecuteWithFailoverNotFoundOnEveryHost(t *testing.T) {
	providers := []config.UsenetProvider{
		{Host: "a.example.com", MaxConnections: 1},
		{Host: "b.example.com", MaxConnections: 1},
	}
	client, _ := failoverTestClient(t, providers)

	err := client.ExecuteWithFailover(context.Background(), func(conn *Connection) error {
		return classifyNNTPError(430, "No such article")
	})
	if !IsArticleNotFoundError(err) {
		t.Fatalf("err = %v, want article not found", err)
	}
}

func TestExecuteWithFailoverMixedFailureStaysTransient(t *testing.T) {
	// A timeout on one provider and a not-found on the other is not proof
	// the article is gone.
	providers := []config.UsenetProvider{
		{Host: "a.example.com", MaxConnections: 1},
		{Host: "b.example.com", MaxConnections: 1},
	}
	client, owners := failoverTestClient(t, providers)

	err := client.ExecuteWithFailover(context.Background(), func(conn *Connection) error {
		if owners[conn] == "a.example.com" {
			return NewTimeoutError(errors.New("read deadline"))
		}
		return classifyNNTPError(430, "No such article")
	})
	if err == nil || IsArticleNotFoundError(err) {
		t.Fatalf("err = %v, want a transient error", err)
	}
}
