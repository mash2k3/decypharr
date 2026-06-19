package rar

import (
	"net/http"
)

// File represents a file entry in a RAR archive
type File struct {
	Path           string
	Size           int64
	CompressedSize int64
	Method         byte
	CRC            uint32
	IsDirectory    bool
	DataOffset     int64
	NextOffset     int64
}

// HttpFile represents a RAR file accessible over HTTP
type HttpFile struct {
	URL        string
	Position   int64
	client     *http.Client
	FileSize   int64
	MaxRetries int
}

// Reader reads RAR3 format archives
type Reader struct {
	File         *HttpFile
	ChunkSize    int
	Marker       int64
	HeaderEndPos int64 // Position after the archive header
	Files        []*File
}
