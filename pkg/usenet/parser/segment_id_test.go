package parser

import (
	"testing"

	"github.com/Tensai75/nzbparser"
)

func TestUnescapeSegmentIDs(t *testing.T) {
	raw := &nzbparser.Nzb{Files: nzbparser.NzbFiles{{Segments: nzbparser.NzbSegments{
		{Id: "a&amp;b&lt;c&gt;@news"}, {Id: "plain@news"}}}}}
	unescapeSegmentIDs(raw)
	if got := raw.Files[0].Segments[0].Id; got != "a&b<c>@news" {
		t.Fatalf("unescaped id %q", got)
	}
	if got := raw.Files[0].Segments[1].Id; got != "plain@news" {
		t.Fatalf("plain id changed: %q", got)
	}
	unescapeSegmentIDs(nil)
}
