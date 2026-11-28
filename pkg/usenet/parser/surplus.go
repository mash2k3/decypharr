package parser

import (
	"context"
	"sort"

	"github.com/sirrobot01/decypharr/internal/nntp"
)

// Some posters list extra articles past the end of a file (often dead). They
// carry none of its bytes, so they are not damage. The file's own yEnc
// header says how many articles it needs; when the NZB lists more and the
// last needed article's header proves the parts are uniform up to the end of
// the file, the extras are dropped here, before any layout is built. Every
// later stage (segment offsets, archive volume sizing, the stored layout,
// reads, repair sampling) then sees only the needed articles. The extras are
// never fetched and their state never counts for or against the file.
//
// Anything unproven leaves the file untouched, so a shortfall, a gap in the
// numbering or uneven part sizes behave exactly as before.

// surplusKeep returns how many articles a file of fileSize in partSize parts
// needs, and ok when the NZB lists more than that.
func surplusKeep(listed int, partSize, fileSize int64) (keep int, ok bool) {
	if partSize <= 0 || fileSize <= 0 {
		return 0, false
	}
	needed := (fileSize + partSize - 1) / partSize
	if needed < 1 || needed >= int64(listed) {
		return 0, false
	}
	return int(needed), true
}

// trimTrailingSurplus drops surplus articles from every file of the group
// whose own yEnc sizes are known. fetch returns an article's yEnc header.
func (p *NZBParser) trimTrailingSurplus(ctx context.Context, group *FileGroup, fetch func(ctx context.Context, id string) (*nntp.YencMetadata, error)) {
	for i := range group.Files {
		file := group.Files[i]
		if len(file.Segments) < 2 {
			continue
		}
		meta, ok := ownFileMeta(group, file)
		if !ok {
			continue
		}
		keep, ok := surplusKeep(len(file.Segments), meta.segmentSize, meta.fileSize)
		if !ok {
			continue
		}
		segs := append(file.Segments[:0:0], file.Segments...)
		sort.Slice(segs, func(a, b int) bool { return segs[a].Number < segs[b].Number })
		// Numbers must form one contiguous range, else getNZBSegments
		// rejects the file as it always did.
		if segs[len(segs)-1].Number-segs[0].Number+1 != len(segs) {
			continue
		}
		if keep == 1 {
			if meta.segmentSize != meta.fileSize {
				continue
			}
		} else {
			h, err := fetch(ctx, segs[keep-1].Id)
			wantBegin := meta.segmentSize*int64(keep-1) + 1
			if err != nil || h == nil || h.Size != meta.fileSize || h.Begin != wantBegin || h.End != meta.fileSize {
				p.logger.Debug().Str("file", file.Filename).Int("needed", keep).Int("listed", len(segs)).
					Msg("not trimming extra articles: last needed article not proven to end the file")
				continue
			}
		}
		p.logger.Info().Str("file", file.Filename).Int("needed", keep).Int("listed", len(segs)).
			Int("ignored", len(segs)-keep).
			Msg("NZB lists more articles than the file needs; ignoring the trailing extras")
		group.Files[i].Segments = segs[:keep]
	}
}
