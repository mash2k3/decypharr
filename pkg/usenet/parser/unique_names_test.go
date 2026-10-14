package parser

import (
	"errors"
	"testing"

	"github.com/sirrobot01/decypharr/internal/config"

	"github.com/sirrobot01/decypharr/pkg/storage"
)

// Lucky.2026.S01E07.FINAL.MULTI.XviD-AFG (2026-09-28): two extracted files
// shared the obfuscated name 2fda1f41bb2b47eeb35cff51e7cecf09.avi. The entry
// listed one (620 MB+) while streams read the other (15 MB), so every read
// past 15 MB failed with "invalid resolved byte range", once a minute.
func TestUniquifyFileNames(t *testing.T) {
	files := []storage.NZBFile{
		{Name: "2fda1f41bb2b47eeb35cff51e7cecf09.avi", Size: 15359887},
		{Name: "2fda1f41bb2b47eeb35cff51e7cecf09.avi", Size: 650000000},
		{Name: "2FDA1F41BB2B47EEB35CFF51E7CECF09.avi", Size: 640000000},
		{Name: "other.mkv"},
		{Name: "2fda1f41bb2b47eeb35cff51e7cecf09 (2).avi"},
	}
	if got := uniquifyFileNames(files); got != 2 {
		t.Fatalf("renamed %d files, want 2", got)
	}
	want := []string{
		// The 15 MB file comes first but is the sample: it gives up the name.
		"2fda1f41bb2b47eeb35cff51e7cecf09-sample.avi",
		"2fda1f41bb2b47eeb35cff51e7cecf09.avi",
		// Similar size: a second version, numbered past the existing " (2)".
		"2FDA1F41BB2B47EEB35CFF51E7CECF09 (3).avi",
		"other.mkv",
		"2fda1f41bb2b47eeb35cff51e7cecf09 (2).avi",
	}
	seen := map[string]bool{}
	for i, f := range files {
		if f.Name != want[i] {
			t.Errorf("file %d: got %q, want %q", i, f.Name, want[i])
		}
		if seen[f.Name] {
			t.Errorf("duplicate name %q after uniquify", f.Name)
		}
		seen[f.Name] = true
	}
}

// The renamed sample must be caught by the existing sample filter.
func TestUniquifiedSampleIsFiltered(t *testing.T) {
	files := []storage.NZBFile{
		{Name: "a1b2c3.mkv", Size: 4000000000},
		{Name: "a1b2c3.mkv", Size: 50000000},
	}
	uniquifyFileNames(files)
	if files[0].Name != "a1b2c3.mkv" {
		t.Fatalf("main file renamed to %q", files[0].Name)
	}
	cfg := &config.Config{}
	if err := cfg.IsFileAllowed(files[1].Name, 0); !errors.Is(err, config.ErrFileIsSample) {
		t.Fatalf("%q not treated as a sample: %v", files[1].Name, err)
	}
}

func TestUniquifyFileNamesNoDuplicates(t *testing.T) {
	files := []storage.NZBFile{{Name: "a.mkv"}, {Name: "b.mkv"}}
	if got := uniquifyFileNames(files); got != 0 || files[0].Name != "a.mkv" || files[1].Name != "b.mkv" {
		t.Fatalf("unexpected rename: %d %+v", got, files)
	}
}

// Tires.S03E04.MULTI.1080p.WEB.X264-HiggsBoson (2026-10-14): the real episode
// sat in a nested RAR set, so the only media file left was the sample. The
// single-file rename gave it the NZB name before the sample filter ran, and the
// 85 MB sample was imported as the episode.
func TestDropSamplesBeforeRename(t *testing.T) {
	files := []storage.NZBFile{
		{
			Name:         "tires.s03e04.multi.1080p.web.x264-higgsboson-sample.mkv",
			InternalPath: "Tires.S03E04.MULTI.1080p.WEB.X264-HiggsBoson/Sample/tires.s03e04.multi.1080p.web.x264-higgsboson-sample.mkv",
			FileType:     storage.NZBFileTypeMedia,
			Size:         85336151,
		},
	}
	kept, dropped := dropSamples(files, &config.Config{})
	if dropped != 1 || len(kept) != 0 {
		t.Fatalf("dropped %d, kept %+v; want the sample dropped", dropped, kept)
	}

	// The sample can also hide behind a clean name, known only by its path.
	files = []storage.NZBFile{
		{Name: "episode.mkv", InternalPath: "Release/Sample/release-sample.mkv"},
		{Name: "episode.mkv", InternalPath: "Release/release.mkv"},
	}
	kept, dropped = dropSamples(files, &config.Config{})
	if dropped != 1 || len(kept) != 1 || kept[0].InternalPath != "Release/release.mkv" {
		t.Fatalf("dropped %d, kept %+v; want only the main file kept", dropped, kept)
	}

	// AllowSamples keeps them.
	files = []storage.NZBFile{{Name: "x-sample.mkv"}}
	if kept, dropped = dropSamples(files, &config.Config{AllowSamples: true}); dropped != 0 || len(kept) != 1 {
		t.Fatalf("AllowSamples: dropped %d", dropped)
	}
}
