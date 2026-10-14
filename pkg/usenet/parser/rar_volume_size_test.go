package parser

import (
	"testing"

	"github.com/sirrobot01/decypharr/pkg/storage"
	"github.com/sirrobot01/decypharr/pkg/usenet/types"
)

// volumeSegments lays out one volume's segments with per-volume offsets, as
// getNZBSegments does.
func volumeSegments(sizes ...int64) []storage.NZBSegment {
	segs := make([]storage.NZBSegment, len(sizes))
	var off int64
	for i, n := range sizes {
		segs[i] = storage.NZBSegment{Number: i + 1, MessageID: "m", Bytes: n, StartOffset: off, EndOffset: off + n - 1}
		off += n
	}
	return segs
}

// Real case (HIMYM S02E07): the last volume's yEnc header wasn't read, its
// last segment was estimated 22102 bytes short, and the file lost its tail.
func TestGrowVolumesToRARPartsRestoresTail(t *testing.T) {
	const seg = 768000
	vol0 := volumeSegments(seg, seg)
	vol1 := volumeSegments(seg, 718213) // real last segment holds 740315+
	base := append(append([]storage.NZBSegment{}, vol0...), vol1...)
	infos := []storage.ArchiveVolumeInfo{
		{Size: 2 * seg, SegmentStart: 0, SegmentEnd: 2},
		{Size: seg + 718213, SegmentStart: 2, SegmentEnd: 4},
	}
	part0 := 2*seg - 103 - 50 // header before, end block after
	part1 := int64(seg + 718213 - 103 + 22102)
	file := &RARFileEntry{
		Name:             "a.mkv",
		UncompressedSize: int64(part0) + part1,
		IsStored:         true,
		VolumeParts: []*types.RARVolumePart{
			{DataOffset: 103, PackedSize: int64(part0), UnpackedSize: int64(part0), PartNumber: 0},
			{DataOffset: 103, PackedSize: part1, UnpackedSize: part1, PartNumber: 1},
		},
	}

	if got := growVolumesToRARParts(base, infos, []*RARFileEntry{file}); got != 1 {
		t.Fatalf("grown = %d, want 1", got)
	}
	if infos[0].Size != 2*seg {
		t.Errorf("volume 0 size changed to %d", infos[0].Size)
	}
	if want := int64(seg + 718213 + 22102); infos[1].Size != want || base[3].Bytes != 718213+22102 {
		t.Errorf("volume 1 size %d / last seg %d, want %d / %d", infos[1].Size, base[3].Bytes, want, 718213+22102)
	}

	segs, err := (&RARParser{}).buildSegmentsForFile(file, base, buildVolumeOffsetMap(infos))
	if err != nil {
		t.Fatal(err)
	}
	var total int64
	for _, s := range segs {
		total += s.Bytes
	}
	if total != file.UncompressedSize {
		t.Errorf("mapped %d bytes, want %d", total, file.UncompressedSize)
	}
}

// A gap of a segment or more is missing data, not a bad estimate.
func TestGrowVolumesToRARPartsIgnoresLargeGap(t *testing.T) {
	base := volumeSegments(1000, 1000)
	infos := []storage.ArchiveVolumeInfo{{Size: 2000, SegmentStart: 0, SegmentEnd: 2}}
	file := &RARFileEntry{VolumeParts: []*types.RARVolumePart{{DataOffset: 50, PackedSize: 3000, PartNumber: 0}}}
	if got := growVolumesToRARParts(base, infos, []*RARFileEntry{file}); got != 0 || infos[0].Size != 2000 {
		t.Errorf("grown = %d, size = %d; want 0, 2000", got, infos[0].Size)
	}
}
