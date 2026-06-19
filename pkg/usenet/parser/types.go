package parser

// FileEntry is a common interface for archive file entries
type FileEntry interface {
	GetName() string
	GetSize() int64
	IsStreamable() bool // Can be streamed without decompression
}

// Ensure our specific types implement FileEntry
var (
	_ FileEntry = (*RARFileEntry)(nil)
	_ FileEntry = (*ZIPFileEntry)(nil)
)

// GetName returns the file name
func (r *RARFileEntry) GetName() string { return r.Name }

// GetSize returns the uncompressed size
func (r *RARFileEntry) GetSize() int64 { return r.UncompressedSize }

// IsStreamable returns true if file is stored without compression
func (r *RARFileEntry) IsStreamable() bool { return r.IsStored }

// GetName returns the file name
func (z *ZIPFileEntry) GetName() string { return z.Name }

// GetSize returns the uncompressed size
func (z *ZIPFileEntry) GetSize() int64 { return z.UncompressedSize }

// IsStreamable returns true if file is stored without compression
func (z *ZIPFileEntry) IsStreamable() bool { return z.IsStored }
