package realdebrid

import (
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/rs/zerolog"
	"github.com/sirrobot01/decypharr/internal/config"
	"github.com/sirrobot01/decypharr/internal/customerror"
	"github.com/sirrobot01/decypharr/internal/request"
	"github.com/sirrobot01/decypharr/pkg/debrid/account"
	"github.com/sirrobot01/decypharr/pkg/debrid/types"
)

// RealDebrid reports a dead file as HTTP 503 with error_code 19/24/35. The
// request client used to retry every 5xx and then return no response, so the
// error code never reached fetchDownloadLink, the file was never reported as
// HosterUnavailable and repair never ran. (Upstream e633bc9.)
func TestUnrestrictDeadFileIsHosterUnavailable(t *testing.T) {
	config.SetConfigPath(t.TempDir())
	t.Cleanup(config.Reset)
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = fmt.Fprint(w, `{"error":"hoster_unavailable","error_code":19}`)
	}))
	t.Cleanup(server.Close)

	acc := account.NewManager(config.Debrid{Name: "realdebrid", DownloadAPIKeys: []string{"k"}}, nil, zerolog.Nop()).Current()
	r := &RealDebrid{Host: server.URL, client: request.New(request.WithMaxRetries(3), request.WithRetryableStatus(http.StatusTooManyRequests)), logger: zerolog.Nop(), config: config.Debrid{Name: "realdebrid"}}

	_, err := r.fetchDownloadLink(acc, "t1", &types.File{Name: "a.mkv", Link: "https://real-debrid.com/d/ABCDEF"})
	if !errors.Is(err, customerror.HosterUnavailableError) {
		t.Fatalf("err = %v, want HosterUnavailableError", err)
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("unrestrict calls = %d, want 1 (a dead file is not retried)", got)
	}
}
