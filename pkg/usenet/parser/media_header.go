package parser

import (
	"bytes"
	"context"
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
		if len(head) < 8 {
			return false
		}
		switch string(head[4:8]) {
		case "ftyp", "moov", "mdat", "free", "skip", "wide", "pnot":
			return true
		}
		return false
	case ".avi":
		return bytes.HasPrefix(head, []byte("RIFF"))
	case ".mpg", ".mpeg":
		return bytes.HasPrefix(head, []byte{0x00, 0x00, 0x01, 0xBA}) ||
			bytes.HasPrefix(head, []byte{0x00, 0x00, 0x01, 0xB3})
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
