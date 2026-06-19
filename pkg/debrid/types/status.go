package types

// TorrentStatus represents the current state of a managed torrent
type TorrentStatus string

const (
	TorrentStatusQueued      TorrentStatus = "queued"      // In import queue (too many active downloads)
	TorrentStatusDownloading TorrentStatus = "downloading" // Downloading on debrid (in active bucket)
	TorrentStatusDownloaded  TorrentStatus = "downloaded"  // Fully complete and ready (moved to cached bucket)
	TorrentStatusError       TorrentStatus = "error"       // Failed
)
