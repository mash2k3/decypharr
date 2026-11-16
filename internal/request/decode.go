package request

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
)

// DecodeJSON decodes one JSON value from a response body with encoding/json.
//
// Streaming a body through sonic's decoder grows its scratch buffer by a
// fraction of its length on every read, so allocation is quadratic in body
// size (upstream measured >4 GB allocated for one 9 MB response). Large
// provider lists, such as RealDebrid torrent pages, hit that.
// (Upstream 0a487ef.)
func DecodeJSON(resp *http.Response, out any) error {
	if resp == nil || resp.Body == nil || out == nil {
		return nil
	}
	decoder := json.NewDecoder(resp.Body)
	if err := decoder.Decode(out); err != nil {
		return err
	}
	return requireJSONEOF(decoder)
}

func requireJSONEOF(decoder *json.Decoder) error {
	var extra json.RawMessage
	err := decoder.Decode(&extra)
	if errors.Is(err, io.EOF) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("decode trailing JSON: %w", err)
	}
	return errors.New("response body contains more than one JSON value")
}
