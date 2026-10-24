package stats

import (
	"strings"
	"testing"

	debridTypes "github.com/sirrobot01/decypharr/pkg/debrid/types"
)

func TestRedactSnapshotJSONDropsAccountIdentifiers(t *testing.T) {
	snap := &Snapshot{
		Debrids: []debridTypes.Stats{{
			Profile: &debridTypes.Profile{Name: "RD", Id: 18091310, Username: "someuser", Email: "som***@example.com", Type: "premium"},
			Library: debridTypes.LibraryStats{Total: 439, TotalSize: 13552767797596},
			Accounts: []map[string]any{{
				"debrid": "RD", "username": "someuser", "token_masked": "IEXB****QCOQ", "links_count": 1677,
			}},
		}},
		Usenet:  map[string]any{"content_size": int64(84756349592155)},
		Content: ContentStats{TotalCount: 11187, TotalSize: 98309117389751},
	}

	out, err := redactSnapshotJSON(snap)
	if err != nil {
		t.Fatalf("redactSnapshotJSON() error = %v", err)
	}
	got := string(out)
	for _, secret := range []string{"someuser", "example.com", "IEXB", "18091310"} {
		if strings.Contains(got, secret) {
			t.Errorf("redacted snapshot still contains %q:\n%s", secret, got)
		}
	}
	for _, kept := range []string{`"name": "RD"`, `"type": "premium"`, `"links_count": 1677`, `"total_size": 98309117389751`, `"content_size": 84756349592155`} {
		if !strings.Contains(got, kept) {
			t.Errorf("redacted snapshot lost %s:\n%s", kept, got)
		}
	}
}

func TestRedactSnapshotJSONNoDebrids(t *testing.T) {
	out, err := redactSnapshotJSON(&Snapshot{})
	if err != nil {
		t.Fatalf("redactSnapshotJSON() error = %v", err)
	}
	if !strings.Contains(string(out), `"debrids": null`) {
		t.Errorf("unexpected output:\n%s", out)
	}
}
