package parser

import (
	"testing"

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
		{Name: "2FDA1F41BB2B47EEB35CFF51E7CECF09.avi", Size: 1},
		{Name: "other.mkv"},
		{Name: "2fda1f41bb2b47eeb35cff51e7cecf09 (2).avi"},
	}
	if got := uniquifyFileNames(files); got != 3 {
		t.Fatalf("renamed %d files, want 3", got)
	}
	want := []string{
		"2fda1f41bb2b47eeb35cff51e7cecf09.avi",
		"2fda1f41bb2b47eeb35cff51e7cecf09 (2).avi",
		"2FDA1F41BB2B47EEB35CFF51E7CECF09 (3).avi",
		"other.mkv",
		"2fda1f41bb2b47eeb35cff51e7cecf09 (2) (2).avi",
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

func TestUniquifyFileNamesNoDuplicates(t *testing.T) {
	files := []storage.NZBFile{{Name: "a.mkv"}, {Name: "b.mkv"}}
	if got := uniquifyFileNames(files); got != 0 || files[0].Name != "a.mkv" || files[1].Name != "b.mkv" {
		t.Fatalf("unexpected rename: %d %+v", got, files)
	}
}
