package customerror

import "errors"

var HosterUnavailableError = (&Error{
	statusCode: 503,
	err:        errors.New("hoster is unavailable"),
	Code:       "hoster_unavailable",
}).Retryable() // 503 Service Unavailable is transient

var UsenetSegmentMissingError = &Error{
	statusCode: 404,
	err:        errors.New("usenet segment is missing"),
	Code:       "usenet_segment_missing",
}

// A missing or undecodable .meta manifest never recovers on its own: the
// segment map the file needs is gone, so every probe fails the same way.
// Repair treats both as broken instead of deferring them forever.
var UsenetManifestMissingError = &Error{
	statusCode: 404,
	err:        errors.New("usenet metadata manifest is missing"),
	Code:       "usenet_manifest_missing",
}

var UsenetManifestInvalidError = &Error{
	statusCode: 422,
	err:        errors.New("usenet metadata manifest is invalid"),
	Code:       "usenet_manifest_invalid",
}

var TrafficExceededError = &Error{
	statusCode: 503,
	err:        errors.New("traffic limit exceeded"),
	Code:       "traffic_exceeded",
}

var TorrentNotFoundError = &Error{
	statusCode: 404,
	err:        errors.New("torrent not found"),
	Code:       "torrent_not_found",
}

var TooManyActiveDownloadsError = (&Error{
	statusCode: 509,
	err:        errors.New("too many active downloads"),
	Code:       "too_many_active_downloads",
}).Retryable() // slot exhaustion is transient — retry after backoff
