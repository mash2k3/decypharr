package manager

import (
	"testing"

	"github.com/sirrobot01/decypharr/internal/config"
	"github.com/sirrobot01/decypharr/pkg/storage"
)

// Same cases as cli_debrid's built-in mount (engine/internal/engine/extras_test.go).
func TestExtraNames(t *testing.T) {
	const gb = int64(1) << 30
	cases := []struct {
		name string
		in   []namedFile
		want map[string]string
	}{
		{"movie with bonus files", []namedFile{
			{"Inception.2010.Bluray.1080p-Grym.mkv", 19 * gb},
			{"Inception.Extras1.Jump.Right.into.the.Action-Grym.mkv", 3 * gb},
			{"Inception.Trailer.mkv", gb / 10},
			{"Behind The Scenes.mp4", gb},
			{"Inception.nfo", 100},
		}, map[string]string{
			"Inception.Extras1.Jump.Right.into.the.Action-Grym.mkv": "Inception.Extras1.Jump.Right.into.the.Action-Grym-featurette.mkv",
			"Inception.Trailer.mkv":                                 "Inception.Trailer-trailer.mkv",
			"Behind The Scenes.mp4":                                 "Behind The Scenes-behindthescenes.mp4",
		}},
		{"small unnamed video in a movie entry", []namedFile{
			{"Movie.2001.1080p.mkv", 10 * gb},
			{"clip01.mkv", gb / 2},
		}, map[string]string{"clip01.mkv": "clip01-other.mkv"}},
		{"two cuts stay versions", []namedFile{
			{"Movie.2001.Extended.mkv", 12 * gb},
			{"Movie.2001.Theatrical.mkv", 10 * gb},
		}, nil},
		{"season pack episodes untouched", []namedFile{
			{"Show.S01E01.mkv", 2 * gb},
			{"Show.S01E02.Behind.the.Scenes.mkv", gb},
			{"Show.S01E03.mkv", gb / 10},
		}, nil},
		{"word in the title is not an extra", []namedFile{
			{"Trailer.Park.Boys.The.Movie.2006.mkv", 8 * gb},
			{"Trailer.Park.Boys.Countdown.2009.mkv", 7 * gb},
		}, nil},
		{"already suffixed", []namedFile{
			{"Movie.mkv", 8 * gb},
			{"Making Of-featurette.mkv", gb},
		}, nil},
		{"extras in a subfolder keep their folder", []namedFile{
			{"Movie.2001.mkv", 8 * gb},
			{"Featurettes/Making Of.mkv", gb},
		}, map[string]string{"Featurettes/Making Of.mkv": "Featurettes/Making Of-behindthescenes.mkv"}},
		{"single video", []namedFile{{"Extras.2005.S01.mkv", gb}}, nil},
	}
	for _, c := range cases {
		got := extraNames(c.in)
		if len(got) != len(c.want) {
			t.Errorf("%s: got %v, want %v", c.name, got, c.want)
			continue
		}
		for k, v := range c.want {
			if got[k] != v {
				t.Errorf("%s: %s -> %q, want %q", c.name, k, got[k], v)
			}
		}
	}
}

// The mount lists an extra under its suffixed name; looking it up by that
// name or by its real one (an existing symlink) finds the same file, and the
// FileInfo keeps the real name for streaming and repair.
func TestExtrasListedAndLookedUp(t *testing.T) {
	store, err := storage.NewStorage(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	const gb = int64(1) << 30
	if err := store.AddOrUpdate(&storage.Entry{
		Protocol: config.ProtocolTorrent, InfoHash: "h", Name: "Inception.2010", ActiveProvider: "RealDebrid",
		Files: map[string]*storage.File{
			"Inception.2010.mkv":    {Name: "Inception.2010.mkv", InfoHash: "h", Size: 19 * gb},
			"Inception.Trailer.mkv": {Name: "Inception.Trailer.mkv", InfoHash: "h", Size: gb / 10},
		},
	}); err != nil {
		t.Fatal(err)
	}
	mgr := &Manager{storage: store, config: &config.Config{}}
	mgr.initEntryCache()

	_, children := mgr.GetTorrentChildren("Inception.2010")
	listed := map[string]string{}
	for _, c := range children {
		listed[c.ListName()] = c.Name()
	}
	if listed["Inception.Trailer-trailer.mkv"] != "Inception.Trailer.mkv" || listed["Inception.2010.mkv"] != "Inception.2010.mkv" {
		t.Fatalf("listing: %v", listed)
	}
	for _, name := range []string{"Inception.Trailer-trailer.mkv", "Inception.Trailer.mkv"} {
		f, err := mgr.GetTorrentFile("Inception.2010", name)
		if err != nil {
			t.Fatalf("lookup %q: %v", name, err)
		}
		if f.Name() != "Inception.Trailer.mkv" || f.ListName() != "Inception.Trailer-trailer.mkv" {
			t.Fatalf("lookup %q: name %q listed %q", name, f.Name(), f.ListName())
		}
	}
}
