package webdav

import (
	"errors"
	"fmt"
	"net/http"

	"github.com/sirrobot01/decypharr/internal/customerror"
	"github.com/sirrobot01/decypharr/pkg/manager"
	"github.com/sirrobot01/decypharr/pkg/storage"
)

func getDownloadByteRange(info *manager.FileInfo) *[2]int64 {
	return info.ByteRange()
}

func (h *Handler) StreamResponse(entry *storage.Entry, info *manager.FileInfo, w http.ResponseWriter, r *http.Request) error {
	start, end := h.getRange(info, r)

	// Extract client identifier from User-Agent header
	client := r.UserAgent()
	if client == "" {
		client = "Unknown"
	}

	streamID := h.manager.TrackStream(entry, info.Name(), client)
	if streamID != "" {
		defer h.manager.UntrackStream(streamID)
	}

	headersWritten := false
	err := h.manager.Stream(r.Context(), entry, info.Name(), start, end, w, func(meta *manager.StreamMetadata) error {
		if err := h.handleSuccessfulResponse(w, meta, start, end); err != nil {
			return err
		}
		headersWritten = true
		return nil
	}, client)
	if err != nil {
		var customErr *customerror.Error
		if errors.As(err, &customErr) {
			customErr.HeadersWritten = headersWritten
			return customErr
		}

		return customerror.NewError(err, http.StatusInternalServerError, "server.internal_error", false, headersWritten)
	}
	return nil
}

func (h *Handler) handleSuccessfulResponse(w http.ResponseWriter, meta *manager.StreamMetadata, start, end int64) error {
	statusCode := http.StatusOK
	if meta != nil {
		if meta.Header != nil {
			if contentLength := meta.Header.Get("Content-Length"); contentLength != "" {
				w.Header().Set("Content-Length", contentLength)
			} else if meta.ContentLength > 0 {
				w.Header().Set("Content-Length", fmt.Sprintf("%d", meta.ContentLength))
			}

			if contentRange := meta.Header.Get("Content-Range"); contentRange != "" {
				w.Header().Set("Content-Range", contentRange)
			}

			if contentType := meta.Header.Get("Content-Type"); contentType != "" {
				w.Header().Set("Content-Type", contentType)
			}
		}
		if meta.StatusCode != 0 {
			statusCode = meta.StatusCode
		} else if start > 0 || end > 0 {
			statusCode = http.StatusPartialContent
		}
	} else if start > 0 || end > 0 {
		statusCode = http.StatusPartialContent
	}

	w.Header().Set("Accept-Ranges", "bytes")
	w.WriteHeader(statusCode)
	return nil
}

func (h *Handler) getRange(info *manager.FileInfo, r *http.Request) (int64, int64) {
	rangeHeader := r.Header.Get("Range")
	if rangeHeader == "" {
		if byteRange := getDownloadByteRange(info); byteRange != nil {
			return byteRange[0], byteRange[1]
		}
		// Signal downstream streaming code to serve the entire file
		return 0, -1
	}

	ranges, err := parseRange(rangeHeader, info.Size())
	if err != nil || len(ranges) != 1 {
		return 0, 0
	}

	byteRange := getDownloadByteRange(info)
	start, end := ranges[0].start, ranges[0].end

	if byteRange != nil {
		start += byteRange[0]
		end += byteRange[0]
	}
	return start, end
}
