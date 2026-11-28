package parser

import (
	"context"
	"errors"
	"fmt"
	"testing"

	nzbparser "github.com/Tensai75/nzbparser"
	"github.com/rs/zerolog"
	"github.com/sirrobot01/decypharr/internal/nntp"
)

// surplusGroup: one file posted in 800-byte parts totalling 2000 bytes (3
// articles), listing `listed` articles.
func surplusGroup(listed int) *FileGroup {
	f := sizedFile(1, listed)
	return &FileGroup{
		Files:    []nzbparser.NzbFile{f},
		fileMeta: map[string]filePartMeta{"n:1": {segmentSize: 800, fileSize: 2000}},
	}
}

func tailHeaders(fetched *[]string) func(context.Context, string) (*nntp.YencMetadata, error) {
	return func(_ context.Context, id string) (*nntp.YencMetadata, error) {
		*fetched = append(*fetched, id)
		var i int
		fmt.Sscanf(id, "f1-s%d", &i)
		begin := int64(i)*800 + 1
		if i >= 3 {
			return nil, errors.New("430 no such article") // surplus is dead
		}
		return &nntp.YencMetadata{Size: 2000, Begin: begin, End: min(begin+799, 2000)}, nil
	}
}

func TestTrimTrailingSurplus(t *testing.T) {
	for _, listed := range []int{4, 9} {
		g := surplusGroup(listed)
		var fetched []string
		p := &NZBParser{logger: zerolog.Nop()}
		p.trimTrailingSurplus(context.Background(), g, tailHeaders(&fetched))
		if len(g.Files[0].Segments) != 3 {
			t.Fatalf("listed %d: kept %d segments, want 3", listed, len(g.Files[0].Segments))
		}
		for _, id := range fetched {
			if id != "f1-s2" {
				t.Fatalf("fetched %s; only the last needed article's header may be read", id)
			}
		}
		total, segs := getNZBSegments(0, g.Files[0], &FileGroup{Files: g.Files, fileMeta: g.fileMeta,
			metadata: &fileAnalysisResult{segmentSize: 800, fileSize: 2000, lastFileSize: 2000}})
		if total != 2000 || len(segs) != 3 || segs[2].Bytes != 400 {
			t.Fatalf("layout after trim: total %d segs %+v", total, segs)
		}
	}
}

func TestTrimTrailingSurplusLeavesUnprovenFilesAlone(t *testing.T) {
	p := &NZBParser{logger: zerolog.Nop()}
	noFetch := func(context.Context, string) (*nntp.YencMetadata, error) { return nil, errors.New("timeout") }

	// no surplus: exactly the needed articles, or fewer (shortfall)
	for _, listed := range []int{3, 2} {
		g := surplusGroup(listed)
		p.trimTrailingSurplus(context.Background(), g, func(context.Context, string) (*nntp.YencMetadata, error) {
			t.Fatal("must not fetch when nothing is extra")
			return nil, nil
		})
		if len(g.Files[0].Segments) != listed {
			t.Fatalf("listed %d changed to %d", listed, len(g.Files[0].Segments))
		}
	}
	// header fetch fails: transient, nothing trimmed
	g := surplusGroup(5)
	p.trimTrailingSurplus(context.Background(), g, noFetch)
	if len(g.Files[0].Segments) != 5 {
		t.Fatal("trimmed without proof")
	}
	// uneven parts: the last needed article's header doesn't line up
	g = surplusGroup(5)
	p.trimTrailingSurplus(context.Background(), g, func(context.Context, string) (*nntp.YencMetadata, error) {
		return &nntp.YencMetadata{Size: 2000, Begin: 1001, End: 1700}, nil
	})
	if len(g.Files[0].Segments) != 5 {
		t.Fatal("trimmed an uneven file")
	}
	// gap in the numbering: left for getNZBSegments to reject as before
	g = surplusGroup(5)
	g.Files[0].Segments = append(g.Files[0].Segments[:1], g.Files[0].Segments[2:]...)
	p.trimTrailingSurplus(context.Background(), g, noFetch)
	if len(g.Files[0].Segments) != 4 {
		t.Fatal("touched a file with a gap")
	}
	// single-part file with surplus: no fetch needed
	g = &FileGroup{Files: []nzbparser.NzbFile{sizedFile(1, 3)},
		fileMeta: map[string]filePartMeta{"n:1": {segmentSize: 500, fileSize: 500}}}
	p.trimTrailingSurplus(context.Background(), g, noFetch)
	if len(g.Files[0].Segments) != 1 {
		t.Fatalf("single-part kept %d segments", len(g.Files[0].Segments))
	}
}
