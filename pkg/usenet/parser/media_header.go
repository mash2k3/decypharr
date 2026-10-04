package parser

import (
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/sirrobot01/decypharr/internal/nntp"
	"github.com/sirrobot01/decypharr/pkg/storage"
)

// mediaHeaderProbeSize is how many bytes of a file's start we need to
// recognise its container.
const mediaHeaderProbeSize = 16

// errBadMediaHeader marks an extracted media file whose first bytes don't match
// its container, which means the archive was stitched together wrong.
var errBadMediaHeader = fmt.Errorf("extracted media file has an invalid container header")

// mediaHeaderValid reports whether head looks like the start of a file with the
// given extension. Unknown extensions are always accepted.
func mediaHeaderValid(name string, head []byte) bool {
	switch strings.ToLower(filepath.Ext(name)) {
	case ".mkv", ".webm":
		return bytes.HasPrefix(head, []byte{0x1A, 0x45, 0xDF, 0xA3})
	case ".mp4", ".m4v", ".mov":
		return isoBoxHeader(head)
	case ".avi":
		return bytes.HasPrefix(head, []byte("RIFF"))
	}
	// Anything else, .mpg/.mpeg included, is accepted: MPEG files may hold a
	// program stream, a transport stream (0x47 sync) or start with padding,
	// so their first bytes can't prove the file was stitched wrong.
	return true
}

// isoBoxHeader reports whether head starts with a plausible ISO-BMFF box: a
// 32-bit size (0 = to end of file, 1 = 64-bit size follows, else at least the
// 8-byte header) and a four-character type. Any box type is accepted, so
// fragmented and DASH-style files (styp, sidx, moof, uuid) pass as well as
// ftyp/moov/mdat; mid-stream data almost never has four printable type bytes.
func isoBoxHeader(head []byte) bool {
	if len(head) < 8 {
		return false
	}
	size := binary.BigEndian.Uint32(head[:4])
	if size != 0 && size != 1 && size < 8 {
		return false
	}
	for _, b := range head[4:8] {
		isAlnum := (b >= 'a' && b <= 'z') || (b >= 'A' && b <= 'Z') || (b >= '0' && b <= '9')
		if !isAlnum && b != ' ' && b != 0xA9 { // 0xA9: '©' in QuickTime atom names
			return false
		}
	}
	return true
}

// verifyMediaHeaders reads the first bytes of each stored, unencrypted media
// file extracted from an archive and checks they match the file's container.
// Obfuscated multi-volume archives whose volumes were put in the wrong order
// still parse "successfully" but produce a file that starts mid-stream, which
// players can't open (Plex reports it as a bare audio track with a huge
// runtime). Read failures are ignored so a flaky provider never fails an import.
func (p *NZBParser) verifyMediaHeaders(ctx context.Context, files []*storage.NZBFile) error {
	for _, f := range files {
		if f == nil || f.FileType != storage.NZBFileTypeMedia || !f.IsStored || f.IsEncrypted || len(f.Segments) == 0 {
			continue
		}
		first := f.Segments[0]
		start := int(first.SegmentDataStart)

		var data *nntp.YencMetadata
		err := p.manager.ExecuteWithFailover(ctx, func(conn *nntp.Connection) error {
			var e error
			data, e = conn.GetHeaderPrefix(first.MessageID, start+mediaHeaderProbeSize)
			return e
		})
		if err != nil || data == nil || len(data.Snippet) < start+mediaHeaderProbeSize {
			p.logger.Debug().Err(err).Str("file", f.Name).Msg("Could not read media header for verification, skipping check")
			continue
		}

		head := data.Snippet[start : start+mediaHeaderProbeSize]
		if !mediaHeaderValid(f.Name, head) {
			p.logger.Warn().
				Str("file", f.Name).
				Hex("head", head).
				Msg("Extracted media file does not start with a valid container header (archive volumes likely out of order)")
			return fmt.Errorf("%w: %s", errBadMediaHeader, f.Name)
		}
	}
	return nil
}
