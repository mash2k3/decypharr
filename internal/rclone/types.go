package rclone

type TransferringStat struct {
	Bytes    int64   `json:"bytes"`
	ETA      int64   `json:"eta"`
	Name     string  `json:"name"`
	Speed    float64 `json:"speed"`
	Size     int64   `json:"size"`
	Progress float64 `json:"progress"`
}

type VersionResponse struct {
	Arch    string `json:"arch"`
	Version string `json:"version"`
	OS      string `json:"os"`
}

type CoreStatsResponse struct {
	Bytes          int64              `json:"bytes"`
	Checks         int                `json:"checks"`
	DeletedDirs    int                `json:"deletedDirs"`
	Deletes        int                `json:"deletes"`
	ElapsedTime    float64            `json:"elapsedTime"`
	Errors         int                `json:"errors"`
	Eta            int                `json:"eta"`
	Speed          float64            `json:"speed"`
	TotalBytes     int64              `json:"totalBytes"`
	TotalChecks    int                `json:"totalChecks"`
	TotalTransfers int                `json:"totalTransfers"`
	TransferTime   float64            `json:"transferTime"`
	Transfers      int                `json:"transfers"`
	Transferring   []TransferringStat `json:"transferring,omitempty"`
}

type MemoryStats struct {
	Sys        int   `json:"Sys"`
	TotalAlloc int64 `json:"TotalAlloc"`
}

type BandwidthStats struct {
	BytesPerSecond int64  `json:"bytesPerSecond"`
	Rate           string `json:"rate"`
}
