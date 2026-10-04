package parser

import (
	"bytes"
	"testing"

	nzbparser "github.com/Tensai75/nzbparser"
	"github.com/rs/zerolog"
)

func TestMediaExtensionFromContent(t *testing.T) {
	ts := make([]byte, 200)
	ts[0], ts[188] = 0x47, 0x47
	cases := map[string][]byte{
		".mkv": {0x1A, 0x45, 0xDF, 0xA3, 0, 0},
		".mp4": append([]byte{0, 0, 0, 0x20}, []byte("ftypisom")...),
		".avi": append(append([]byte("RIFF"), 0, 0, 0, 0), []byte("AVI LIST")...),
		".mpg": {0x00, 0x00, 0x01, 0xBA},
		".ts":  ts,
		"":     bytes.Repeat([]byte{0xAB}, 16),
	}
	for want, data := range cases {
		if got := mediaExtensionFromContent(data); got != want {
			t.Errorf("mediaExtensionFromContent(%x...) = %q, want %q", data[:4], got, want)
		}
	}
}

// processMediaFile drops a media group with no extension; the content
// detection now appends one so obfuscated, extensionless posts survive.
func TestProcessMediaFileNeedsExtension(t *testing.T) {
	p := &NZBParser{logger: zerolog.Nop()}
	mk := func(name string) *FileGroup {
		return &FileGroup{
			BaseName: "2fda1f41bb2b47eeb35cff51e7cecf09",
			Files: []nzbparser.NzbFile{{
				Filename: name,
				Segments: nzbparser.NzbSegments{{Number: 1, Id: "a@b", Bytes: 1000}},
			}},
		}
	}
	if f := p.processMediaFile(mk("2fda1f41bb2b47eeb35cff51e7cecf09"), ""); f != nil {
		t.Fatalf("extensionless media unexpectedly kept as %q", f.Name)
	}
	f := p.processMediaFile(mk("2fda1f41bb2b47eeb35cff51e7cecf09.mkv"), "")
	if f == nil || f.Name != "2fda1f41bb2b47eeb35cff51e7cecf09.mkv" {
		t.Fatalf("media with inferred extension not kept: %+v", f)
	}
}
