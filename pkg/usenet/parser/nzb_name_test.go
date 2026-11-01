package parser

import "testing"

func TestDetermineNZBNameOnlyTrimsNZBExtension(t *testing.T) {
	cases := map[string]string{
		"Movie.2019.1080p.BluRay.x264-GRP.nzb":    "Movie.2019.1080p.BluRay.x264-GRP",
		"Movie.2019.1080p.BluRay.x264-GRP.NZB":    "Movie.2019.1080p.BluRay.x264-GRP",
		"Movie.2019.1080p.BluRay.x264-GRP.nzb.gz": "Movie.2019.1080p.BluRay.x264-GRP",
		"Movie.2019.1080p.BluRay.x264-GRP":        "Movie.2019.1080p.BluRay.x264-GRP",
	}
	for in, want := range cases {
		if got := determineNZBName(in, nil); got != want {
			t.Errorf("determineNZBName(%q) = %q, want %q", in, got, want)
		}
	}
}

// A queued job is re-parsed from its stored (already trimmed) name on every
// restart or deferred start; the name must survive any number of re-parses.
func TestDetermineNZBNameStableAcrossReparses(t *testing.T) {
	name := determineNZBName("Batman.The.Long.Halloween.Part.One.2021.1080p.BluRay.x264-GRP.nzb", nil)
	want := name
	for i := 0; i < 8; i++ {
		name = determineNZBName(name, nil)
	}
	if name != want {
		t.Fatalf("name after re-parses = %q, want %q", name, want)
	}
}

func TestDetermineNZBNameFallsBackToMeta(t *testing.T) {
	if got := determineNZBName("", map[string]string{"Name": "From.Meta"}); got != "From.Meta" {
		t.Fatalf("got %q", got)
	}
}
