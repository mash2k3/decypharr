package parser

import (
	"testing"

	"github.com/sirrobot01/decypharr/pkg/storage"
)

func TestLeftoverOnly(t *testing.T) {
	const mb = int64(1) << 20
	cases := []struct {
		name   string
		kept   []int64
		failed int64 // largest failed archive group
		reject bool
	}{
		// Silicon Valley S01E02 ROVERS: 2.4 GB RAR set without its first
		// volume failed; only the unlabelled 85 MB sample 238235-0.mkv parsed.
		{"sample left after main RAR failed", []int64{85 * mb}, 2400 * mb, true},
		{"several small leftovers still rejected", []int64{85 * mb, 90 * mb, 2 * mb}, 2400 * mb, true},
		{"nothing failed", []int64{85 * mb}, 0, false},
		{"only small extras failed (nfo/sfv/jpg)", []int64{2300 * mb}, 3 * mb, false},
		{"real content parsed beside a failed archive", []int64{2300 * mb}, 2400 * mb, false},
		// Season pack, one RAR set per episode: 8 of 10 failed, 2 parsed.
		// The two that parsed are real episodes and should import.
		{"season pack with most episodes failed", []int64{1000 * mb, 1010 * mb}, 1020 * mb, false},
		{"just at the ratio", []int64{600 * mb}, 2400 * mb, false},
		{"just under the ratio", []int64{599 * mb}, 2400 * mb, true},
	}
	for _, c := range cases {
		files := make([]storage.NZBFile, 0, len(c.kept))
		for _, k := range c.kept {
			files = append(files, storage.NZBFile{Size: k})
		}
		if got := leftoverOnly(files, c.failed) != nil; got != c.reject {
			t.Errorf("%s: reject=%v, want %v", c.name, got, c.reject)
		}
	}
}
