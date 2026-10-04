package usenet

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/rs/zerolog"
	"github.com/sirrobot01/decypharr/internal/customerror"
)

// A missing or undecodable .meta used to look like any transient probe error,
// so repair deferred the entry forever and cli_debrid never replaced it.
func TestGetNZBManifestErrors(t *testing.T) {
	dir := t.TempDir()
	s := &NZBStorage{metaDir: dir, logger: zerolog.Nop()}

	_, err := s.GetNZB("gone")
	if !errors.Is(err, customerror.UsenetManifestMissingError) || !errors.Is(err, ErrNZBNotFound) {
		t.Fatalf("missing meta: got %v, want manifest-missing and ErrNZBNotFound", err)
	}

	if err := os.WriteFile(s.metaFilePath("bad"), []byte("not a protobuf \xff\xff\xff"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := s.GetNZB("bad"); !errors.Is(err, customerror.UsenetManifestInvalidError) {
		t.Fatalf("invalid meta: got %v, want manifest-invalid", err)
	}

	// An unmounted data dir must not mark everything permanently broken.
	gone := &NZBStorage{metaDir: filepath.Join(dir, "unmounted"), logger: zerolog.Nop()}
	_, err = gone.GetNZB("x")
	if err == nil || errors.Is(err, customerror.UsenetManifestMissingError) {
		t.Fatalf("missing meta dir: got %v, want a non-manifest error", err)
	}
}
