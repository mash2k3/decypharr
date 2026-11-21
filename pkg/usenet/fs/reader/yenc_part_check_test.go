package reader

import (
	"bytes"
	"strings"
	"testing"

	"github.com/rs/zerolog"
	"github.com/sirrobot01/decypharr/internal/nntp"
)

func TestCheckYencPartLogsOnlyMismatches(t *testing.T) {
	var buf bytes.Buffer
	sf := &SegmentFetcher{logger: zerolog.New(&buf)}
	seg := &SegmentMeta{Number: 32}
	sf.checkYencPart(seg, "<a@x>", nntp.YencPart{Part: 32, Total: 68}, 10, 10) // 1-based match
	sf.checkYencPart(seg, "<a@x>", nntp.YencPart{Part: 33, Total: 68}, 10, 10) // 0-based NZB numbering
	sf.checkYencPart(seg, "<a@x>", nntp.YencPart{}, 10, 10)                    // no part in header
	if buf.Len() != 0 {
		t.Fatalf("logged a match: %s", buf.String())
	}
	sf.checkYencPart(seg, "<a@x>", nntp.YencPart{Part: 257, Total: 281}, 12, 10)
	if out := buf.String(); !strings.Contains(out, `"yenc_part":257`) || !strings.Contains(out, `"served":true`) {
		t.Fatalf("mismatch not logged: %s", out)
	}
}
