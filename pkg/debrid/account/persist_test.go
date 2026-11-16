package account

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/puzpuzpuz/xsync/v4"
	"github.com/rs/zerolog"
	"github.com/sirrobot01/decypharr/pkg/debrid/types"
)

func newPersistManager(token string) (*Manager, *Account) {
	acc := &Account{Debrid: "alldebrid", Token: token, links: xsync.NewMap[string, types.DownloadLink]()}
	accounts := xsync.NewMap[string, *Account]()
	accounts.Store(token, acc)
	m := &Manager{debrid: "alldebrid", accounts: accounts, logger: zerolog.Nop()}
	m.current.Store(acc)
	return m, acc
}

// Links survive a restart, so files opened after one don't each mint a new
// link (AllDebrid suspends accounts for thousands of unused links).
func TestLinksSurviveRestart(t *testing.T) {
	store := &linkStore{path: filepath.Join(t.TempDir(), "links", "alldebrid.json")}
	m, acc := newPersistManager("secret-token")
	acc.storeLink(types.DownloadLink{Debrid: "alldebrid", Token: "secret-token", Link: "hoster/1", DownloadLink: "https://cdn.example/1", ExpiresAt: time.Now().Add(time.Hour)})
	acc.storeLink(types.DownloadLink{Debrid: "alldebrid", Token: "secret-token", Link: "hoster/2", DownloadLink: "https://cdn.example/2", ExpiresAt: time.Now().Add(-time.Minute)})
	acc.storeLink(types.DownloadLink{Debrid: "alldebrid", Token: "secret-token", Link: "hoster/3", DownloadLink: "https://cdn.example/3"}) // no expiry: not saved
	store.save(m)

	raw, err := os.ReadFile(store.path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "secret-token") {
		t.Fatal("API token written to the link file")
	}

	m2, acc2 := newPersistManager("secret-token")
	store.load(m2)
	if acc2.DownloadLinksCount() != 1 {
		t.Fatalf("restored %d links, want 1 (only the unexpired one with an expiry)", acc2.DownloadLinksCount())
	}
	fetched := false
	dl, err := acc2.GetDownloadLink("7", &types.File{Link: "hoster/1"}, func(*Account, string, *types.File) (types.DownloadLink, error) {
		fetched = true
		return types.DownloadLink{}, nil
	})
	if err != nil || fetched || dl.DownloadLink != "https://cdn.example/1" || dl.Token != "secret-token" {
		t.Fatalf("restored link not reused: %+v fetched=%v err=%v", dl, fetched, err)
	}

	// Another account's links are not handed to this one.
	m3, acc3 := newPersistManager("other-token")
	store.load(m3)
	if acc3.DownloadLinksCount() != 0 {
		t.Fatalf("other account got %d links", acc3.DownloadLinksCount())
	}
}

func TestLinkSaveIsBatched(t *testing.T) {
	store := &linkStore{path: filepath.Join(t.TempDir(), "alldebrid.json")}
	m, _ := newPersistManager("t")
	store.scheduleSave(m)
	first := store.timer
	store.scheduleSave(m)
	if first == nil || store.timer != first {
		t.Fatal("second change scheduled another write instead of joining the pending one")
	}
	first.Stop()
}

func TestLoadIgnoresMissingAndCorruptFiles(t *testing.T) {
	dir := t.TempDir()
	m, acc := newPersistManager("t")
	(&linkStore{path: filepath.Join(dir, "missing.json")}).load(m)
	bad := filepath.Join(dir, "bad.json")
	if err := os.WriteFile(bad, []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	(&linkStore{path: bad}).load(m)
	if acc.DownloadLinksCount() != 0 {
		t.Fatal("links loaded from a missing or corrupt file")
	}
}
