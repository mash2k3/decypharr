package parser

import (
	"fmt"
	"testing"

	nzbparser "github.com/Tensai75/nzbparser"
)

func sizedFile(number, segCount int) nzbparser.NzbFile {
	segs := make(nzbparser.NzbSegments, segCount)
	for i := range segs {
		segs[i] = nzbparser.NzbSegment{Number: i + 1, Id: fmt.Sprintf("f%d-s%d", number, i), Bytes: 1030}
	}
	return nzbparser.NzbFile{Number: number, Segments: segs}
}

// A part whose articles are smaller than the group-wide segment size used to
// get oversized slots; reads past each article's real end then failed.
func TestGetNZBSegmentsUsesFilesOwnSizes(t *testing.T) {
	group := &FileGroup{
		Files:    []nzbparser.NzbFile{sizedFile(1, 3), sizedFile(2, 3)},
		metadata: &fileAnalysisResult{segmentSize: 1000, fileSize: 3000, lastFileSize: 3000},
		fileMeta: map[string]filePartMeta{
			"n:1": {segmentSize: 1000, fileSize: 3000},
			"n:2": {segmentSize: 800, fileSize: 2300},
		},
	}
	total, segs := getNZBSegments(1, group.Files[1], group)
	if total != 2300 {
		t.Fatalf("total = %d, want 2300", total)
	}
	want := []int64{800, 800, 700}
	for i, s := range segs {
		if s.Bytes != want[i] {
			t.Errorf("segment %d: Bytes = %d, want %d", i, s.Bytes, want[i])
		}
	}
}

func TestGetNZBSegmentsIgnoresOwnSizesOnKeyCollision(t *testing.T) {
	// Obfuscated NZBs can give every file the same number; keyed metadata
	// could then belong to another file, so fall back to the group's.
	a, b := sizedFile(1, 3), sizedFile(1, 3)
	group := &FileGroup{
		Files:    []nzbparser.NzbFile{a, b},
		metadata: &fileAnalysisResult{segmentSize: 1000, fileSize: 3000, lastFileSize: 3000},
		fileMeta: map[string]filePartMeta{"n:1": {segmentSize: 800, fileSize: 2300}},
	}
	_, segs := getNZBSegments(0, group.Files[0], group)
	if segs[0].Bytes != 1000 {
		t.Fatalf("Bytes = %d, want group-wide 1000", segs[0].Bytes)
	}
}

func TestBorrowFileMeta(t *testing.T) {
	group := &FileGroup{
		Files: []nzbparser.NzbFile{sizedFile(1, 4), sizedFile(2, 4), sizedFile(3, 2)},
		fileMeta: map[string]filePartMeta{
			"n:2": {segmentSize: 716800, fileSize: 2867200},
			"n:3": {segmentSize: 716800, fileSize: 900000},
		},
	}
	meta, ok := borrowFileMeta(group)
	if !ok || meta.segmentSize != 716800 || meta.fileSize != 2867200 {
		t.Fatalf("borrowed %+v ok=%v, want the middle volume's sizes", meta, ok)
	}

	delete(group.fileMeta, "n:2")
	meta, ok = borrowFileMeta(group)
	if !ok || meta.segmentSize != 716800 || meta.fileSize != 716800*4 {
		t.Fatalf("from last file got %+v ok=%v, want its segment size only", meta, ok)
	}

	group.fileMeta = map[string]filePartMeta{}
	if _, ok := borrowFileMeta(group); ok {
		t.Fatal("expected no metadata to borrow")
	}
}
