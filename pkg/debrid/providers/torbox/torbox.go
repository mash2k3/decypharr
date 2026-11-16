package torbox

import (
	"bytes"
	"cmp"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"path"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"

	json "github.com/bytedance/sonic"

	"github.com/rs/zerolog"
	"github.com/sirrobot01/decypharr/internal/config"
	"github.com/sirrobot01/decypharr/internal/customerror"
	"github.com/sirrobot01/decypharr/internal/logger"
	"github.com/sirrobot01/decypharr/internal/request"
	"github.com/sirrobot01/decypharr/internal/utils"
	"github.com/sirrobot01/decypharr/pkg/debrid/account"
	"github.com/sirrobot01/decypharr/pkg/debrid/types"
	"github.com/sirrobot01/decypharr/pkg/version"
	"go.uber.org/ratelimit"
)

var planSlots = map[string]int{
	"essential": 3,
	"standard":  5,
	"pro":       10,
}

type Torbox struct {
	Host                  string `json:"host"`
	APIKey                string
	accountsManager       *account.Manager
	autoExpiresLinksAfter time.Duration
	client                *request.Client
	logger                zerolog.Logger
	Profile               *types.Profile
	config                config.Debrid
	downloadPresentCache  sync.Map
	downloadPresentMu     sync.Mutex
	downloadPresentLoaded bool

	// createtorrent has its own TorBox limit (60/hour at the time of writing),
	// separate from the 300/min API cap, and every call counts, including
	// add_only_if_cached refusals. Once TorBox refuses an add, further adds are
	// rejected locally until this time instead of each one waiting ~30s for
	// another 429. (Upstream PR #444.)
	addPauseMu    sync.Mutex
	addPauseUntil time.Time
}

// defaultAddLimitPause is used when TorBox refuses an add without Retry-After.
const defaultAddLimitPause = 5 * time.Minute

// maxAddLimitPause caps an unusually large Retry-After from TorBox.
const maxAddLimitPause = time.Hour

func New(dc config.Debrid, ratelimits map[string]ratelimit.Limiter) (*Torbox, error) {
	cfg := config.Get()
	headers := map[string]string{
		"Authorization": fmt.Sprintf("Bearer %s", dc.APIKey),
	}
	if dc.UserAgent != "" {
		headers["User-Agent"] = dc.UserAgent
	} else {
		headers["User-Agent"] = fmt.Sprintf("Decypharr/%s (%s; %s)", version.GetInfo(), runtime.GOOS, runtime.GOARCH)
	}
	_log := logger.New(dc.Name)

	// TorBox enforces a hard cap of 300 req/min per API key, applied
	// synchronously across all servers since v8.4 (Feb 2026, GAP-002).
	// Default to that limit if the user has not configured one explicitly.
	mainRL := ratelimits["main"]
	if mainRL == nil {
		mainRL = ratelimit.New(300, ratelimit.Per(time.Minute), ratelimit.WithSlack(30))
	}

	// TorBox's cap counts per API key. requestdl now goes through the account
	// client, so when the download keys are just the main key it must share the
	// main bucket; two buckets on one key could emit ~550/min against a 300/min
	// cap. (Upstream 3a0e0e2.)
	downloadRL := mainRL
	if !onlyUsesKey(dc.DownloadAPIKeys, dc.APIKey) && ratelimits["download"] != nil {
		downloadRL = ratelimits["download"]
	}

	opts := []request.ClientOption{
		request.WithHeaders(headers),
		request.WithRateLimiter(mainRL),
		request.WithMaxRetries(cfg.Retries),
		request.WithRetryableStatus(http.StatusTooManyRequests, http.StatusBadGateway),
	}
	if dc.Proxy != "" {
		opts = append(opts, request.WithProxy(dc.Proxy))
	}

	autoExpiresLinksAfter, err := utils.ParseDuration(dc.AutoExpireLinksAfter)
	if autoExpiresLinksAfter == 0 || err != nil {
		autoExpiresLinksAfter = 48 * time.Hour
	}

	tb := &Torbox{
		Host:                  "https://api.torbox.app/v1",
		APIKey:                dc.APIKey,
		accountsManager:       account.NewManager(dc, downloadRL, _log),
		config:                dc,
		autoExpiresLinksAfter: autoExpiresLinksAfter,
		client:                request.New(opts...),
		logger:                _log,
	}
	return tb, nil
}

// onlyUsesKey reports whether the download keys are just the main key, in
// which case download traffic spends the same per-key budget as everything else.
func onlyUsesKey(downloadKeys []string, apiKey string) bool {
	for _, key := range downloadKeys {
		if key != "" && key != apiKey {
			return false
		}
	}
	return true
}

func (tb *Torbox) Config() config.Debrid {
	return tb.config
}

func (tb *Torbox) Logger() zerolog.Logger {
	return tb.logger
}

// doGet performs a GET request and unmarshals the response
func (tb *Torbox) doGet(endpoint string, queryParams map[string]string, result interface{}) (*http.Response, error) {
	return tb.doGetWithClient(tb.client, endpoint, queryParams, result)
}

// doGetWithClient is doGet through a specific (per-account) client.
func (tb *Torbox) doGetWithClient(client *request.Client, endpoint string, queryParams map[string]string, result interface{}) (*http.Response, error) {
	u, err := url.Parse(tb.Host + endpoint)
	if err != nil {
		return nil, err
	}

	if queryParams != nil {
		q := u.Query()
		for k, v := range queryParams {
			q.Set(k, v)
		}
		u.RawQuery = q.Encode()
	}

	req, err := http.NewRequest(http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, err
	}

	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer request.DrainAndClose(resp.Body)

	if result != nil && resp.StatusCode >= 200 && resp.StatusCode < 300 && resp.ContentLength != 0 {
		if err := request.DecodeJSON(resp, result); err != nil {
			return resp, err
		}
	}

	return resp, nil
}

// doPostForm performs a POST request with form data
func (tb *Torbox) doPostForm(endpoint string, formData map[string]string, result interface{}) (*http.Response, error) {
	return tb.postForm(endpoint, formData, result, false)
}

// doPostFormOnce is doPostForm without retries, for endpoints with their own
// tight TorBox limit where a retry only burns more of it.
func (tb *Torbox) doPostFormOnce(endpoint string, formData map[string]string, result interface{}) (*http.Response, error) {
	return tb.postForm(endpoint, formData, result, true)
}

func (tb *Torbox) postForm(endpoint string, formData map[string]string, result interface{}, once bool) (*http.Response, error) {
	form := url.Values{}
	for k, v := range formData {
		form.Set(k, v)
	}

	req, err := http.NewRequest(http.MethodPost, tb.Host+endpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	do := tb.client.Do
	if once {
		do = tb.client.DoOnce
	}
	resp, err := do(req)
	if err != nil {
		return nil, err
	}
	defer request.DrainAndClose(resp.Body)

	if result != nil && resp.StatusCode >= 200 && resp.StatusCode < 300 && resp.ContentLength != 0 {
		if err := request.DecodeJSON(resp, result); err != nil {
			return resp, err
		}
	}

	return resp, nil
}

// doPostJSON performs a POST request with a JSON body.
func (tb *Torbox) doPostJSON(endpoint string, payload any, result any) (*http.Response, error) {
	var body io.Reader
	if payload != nil {
		data, err := json.Marshal(payload)
		if err != nil {
			return nil, err
		}
		body = bytes.NewReader(data)
	}

	req, err := http.NewRequest(http.MethodPost, tb.Host+endpoint, body)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := tb.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer request.DrainAndClose(resp.Body)

	if result != nil && resp.StatusCode >= 200 && resp.StatusCode < 300 && resp.ContentLength != 0 {
		if err := request.DecodeJSON(resp, result); err != nil {
			return resp, err
		}
	}

	return resp, nil
}

func (tb *Torbox) IsAvailable(hashes []string) map[string]bool {
	result := make(map[string]bool)

	for i := 0; i < len(hashes); i += 100 {
		end := i + 100
		if end > len(hashes) {
			end = len(hashes)
		}

		validHashes := make([]string, 0, end-i)
		for _, hash := range hashes[i:end] {
			if hash != "" {
				validHashes = append(validHashes, hash)
			}
		}

		if len(validHashes) == 0 {
			continue
		}

		hashStr := strings.Join(validHashes, ",")
		var res AvailableResponse

		resp, err := tb.doGet("/api/torrents/checkcached", map[string]string{"hash": hashStr}, &res)
		if err != nil || resp.StatusCode < 200 || resp.StatusCode >= 300 {
			continue
		}
		if res.Data == nil {
			return result
		}

		for h, c := range *res.Data {
			if c.Size > 0 {
				result[strings.ToUpper(h)] = true
			}
		}
	}
	return result
}

// isCached reports whether a single info hash is present in TorBox's cache.
//
// The second return value states whether the answer is trustworthy. A transport
// error, a non-2xx reply or a malformed body yields (false, false), and callers
// must not read that as "not cached": absence of information is not evidence of
// absence. Acting on an unknown answer as though it were a negative one is how a
// cache probe turns into a mass false-positive machine.
//
// IsAvailable cannot be reused for this: it skips failed batches with `continue`,
// so a probe that failed and a hash that is genuinely absent both surface as a
// missing map key. That distinction is the whole point here.
func (tb *Torbox) isCached(hash string) (cached bool, known bool) {
	if hash == "" {
		return false, false
	}
	var res AvailableResponse
	resp, err := tb.doGet("/api/torrents/checkcached", map[string]string{"hash": hash}, &res)
	if err != nil || resp == nil || resp.StatusCode < 200 || resp.StatusCode >= 300 || res.Data == nil {
		return false, false
	}
	for h, c := range *res.Data {
		if strings.EqualFold(h, hash) && c.Size > 0 {
			return true, true
		}
	}
	return false, true
}

// addsPausedUntil returns when adds may resume, or zero if they are allowed now.
func (tb *Torbox) addsPausedUntil() time.Time {
	tb.addPauseMu.Lock()
	defer tb.addPauseMu.Unlock()
	if time.Now().Before(tb.addPauseUntil) {
		return tb.addPauseUntil
	}
	return time.Time{}
}

// pauseAdds records a TorBox add-limit refusal and returns when adds resume.
func (tb *Torbox) pauseAdds(resp *http.Response) time.Time {
	wait := defaultAddLimitPause
	if ra := resp.Header.Get("Retry-After"); ra != "" {
		if secs, err := strconv.Atoi(ra); err == nil && secs > 0 {
			wait = time.Duration(secs) * time.Second
		} else if t, err := http.ParseTime(ra); err == nil && time.Until(t) > 0 {
			wait = time.Until(t)
		}
	}
	wait = min(wait, maxAddLimitPause)

	tb.addPauseMu.Lock()
	defer tb.addPauseMu.Unlock()
	if until := time.Now().Add(wait); until.After(tb.addPauseUntil) {
		tb.addPauseUntil = until
	}
	return tb.addPauseUntil
}

func (tb *Torbox) SubmitMagnet(torrent *types.Torrent) (*types.Torrent, error) {
	if until := tb.addsPausedUntil(); !until.IsZero() {
		return nil, fmt.Errorf("torbox add limit reached, torrent adds paused until %s", until.Local().Format("15:04:05"))
	}

	var data AddMagnetResponse

	formData := map[string]string{
		"magnet": torrent.Magnet.Link,
	}
	if !torrent.DownloadUncached {
		formData["add_only_if_cached"] = "true"

		// Ask the cache before calling createtorrent. When a release is not
		// cached TorBox does not reply with a refusal, it does not reply at
		// all: the request wrapper then burns ResponseHeaderTimeout (30s) per
		// attempt and retries cfg.Retries times, so one uncached grab can cost
		// around two minutes. The calling *arr times out well before that and
		// records the failure against the INDEXER, which it eventually
		// disables, for a release the indexer served perfectly well.
		// Failing fast keeps the refusal cheap and keeps the blame off the
		// indexer. An unknown answer falls through to the previous behaviour.
		if cached, known := tb.isCached(torrent.InfoHash); known && !cached {
			return nil, fmt.Errorf("torrent: %s not cached", torrent.Name)
		}
	}

	// Sent once: TorBox holds an over-limit add ~30s before its 429, so the
	// client's retries would hang a single grab for minutes and spend more of
	// the add limit.
	resp, err := tb.doPostFormOnce("/api/torrents/createtorrent", formData, &data)
	if err != nil {
		return nil, err
	}

	if resp.StatusCode == http.StatusTooManyRequests {
		until := tb.pauseAdds(resp)
		tb.logger.Warn().Msgf("TorBox add limit reached, pausing torrent adds until %s", until.Local().Format("15:04:05"))
		return nil, fmt.Errorf("torbox add limit reached, torrent adds paused until %s", until.Local().Format("15:04:05"))
	}

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("torbox API error: Status: %d", resp.StatusCode)
	}
	// An array response can list several torrents; take the one for this hash.
	var id int
	for _, added := range data.Data {
		if len(data.Data) == 1 || strings.EqualFold(added.Hash, torrent.InfoHash) {
			id = cmp.Or(added.TorrentId, added.Id)
			break
		}
	}
	if id == 0 {
		return nil, fmt.Errorf("error adding torrent")
	}
	torrent.Id = strconv.Itoa(id)
	torrent.Debrid = tb.config.Name
	torrent.Added = time.Now()

	return torrent, nil
}

func (tb *Torbox) getTorboxStatus(status string, finished bool) types.TorrentStatus {
	if finished {
		return types.TorrentStatusDownloaded
	}
	downloading := []string{"paused", "downloading",
		"checkingResumeData", "metaDL", "pausedUP", "queuedUP", "checkingUP",
		"forcedUP", "allocating", "downloading", "metaDL", "pausedDL",
		"queuedDL", "checkingDL", "forcedDL", "checkingResumeData", "moving",
		"incomplete",
		// A new uncached torrent starts as "queued", then "checking", before
		// downloading. Treating them as errors failed healthy adds.
		// (Upstream 3448c74, PR #473.)
		"queued", "checking",
	}

	downloaded := []string{
		"completed", "cached", "uploading", "downloaded",
	}

	status = regexp.MustCompile(`\s*\(.*?\)\s*`).ReplaceAllString(status, "")

	switch {
	case utils.Contains(downloading, status):
		return types.TorrentStatusDownloading
	case utils.Contains(downloaded, status):
		return types.TorrentStatusDownloaded
	default:
		return types.TorrentStatusError
	}
}

func (tb *Torbox) GetTorrent(torrentId string) (*types.Torrent, error) {
	var res InfoResponse

	resp, err := tb.doGet("/api/torrents/mylist", map[string]string{"id": torrentId}, &res)
	if err != nil {
		return nil, err
	}

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("torbox API error: Status: %d", resp.StatusCode)
	}
	data := res.Data
	if data == nil {
		return nil, fmt.Errorf("error getting torrent")
	}
	t := &types.Torrent{
		Id:               strconv.Itoa(data.Id),
		InfoHash:         data.Hash,
		Name:             data.Name,
		Bytes:            data.Size,
		Progress:         data.Progress * 100,
		Status:           tb.getTorboxStatus(data.DownloadState, data.DownloadFinished),
		Speed:            data.DownloadSpeed,
		Seeders:          data.Seeds,
		Filename:         data.Name,
		OriginalFilename: data.Name,
		Debrid:           tb.config.Name,
		Files:            make(map[string]types.File),
		Added:            data.CreatedAt,
	}
	cfg := config.Get()

	for _, f := range data.Files {
		fileName := filepath.Base(f.Name)
		if err := cfg.IsFileAllowed(f.AbsolutePath, f.Size); err != nil {
			continue
		}

		file := types.File{
			TorrentId: t.Id,
			Id:        strconv.Itoa(f.Id),
			Name:      fileName,
			Size:      f.Size,
			Path:      f.Name,
		}

		if data.DownloadFinished {
			file.Link = fmt.Sprintf("torbox://%s/%d", t.Id, f.Id)
		}

		t.Files[fileName] = file
	}
	var cleanPath string
	if len(t.Files) > 0 {
		cleanPath = path.Clean(data.Files[0].Name)
	} else {
		cleanPath = path.Clean(data.Name)
	}

	t.OriginalFilename = strings.Split(cleanPath, "/")[0]
	t.Debrid = tb.config.Name

	return t, nil
}

func (tb *Torbox) loadDownloadPresent() error {
	offset := 0
	total := 0
	for {
		var res TorrentsListResponse
		resp, err := tb.doGet("/api/torrents/mylist", map[string]string{
			"bypass_cache": "true",
			"offset":       fmt.Sprintf("%d", offset),
		}, &res)
		if err != nil {
			return err
		}
		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			return fmt.Errorf("torbox API error: Status: %d", resp.StatusCode)
		}
		if res.Data == nil || len(*res.Data) == 0 {
			break
		}
		for _, t := range *res.Data {
			tb.downloadPresentCache.Store(strconv.Itoa(t.Id), t.DownloadPresent)
		}
		total += len(*res.Data)
		offset += len(*res.Data)
	}
	tb.logger.Info().Int("count", total).Msg("loaded download_present cache for repair")
	return nil
}

func (tb *Torbox) UpdateTorrent(t *types.Torrent) error {
	var res InfoResponse

	resp, err := tb.doGet("/api/torrents/mylist", map[string]string{"id": t.Id}, &res)
	if err != nil {
		return err
	}

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("torbox API error: Status: %d", resp.StatusCode)
	}
	data := res.Data
	name := data.Name

	t.Name = name
	t.Bytes = data.Size
	t.Progress = data.Progress * 100
	t.Status = tb.getTorboxStatus(data.DownloadState, data.DownloadFinished)
	t.Speed = data.DownloadSpeed
	t.Seeders = data.Seeds
	t.Filename = name
	t.OriginalFilename = name
	if data.Hash != "" {
		t.InfoHash = data.Hash
	}
	t.Debrid = tb.config.Name

	t.Files = make(map[string]types.File)

	cfg := config.Get()

	for _, f := range data.Files {
		fileName := filepath.Base(f.Name)

		if err := cfg.IsFileAllowed(f.AbsolutePath, f.Size); err != nil {
			continue
		}

		file := types.File{
			TorrentId: t.Id,
			Id:        strconv.Itoa(f.Id),
			Name:      fileName,
			Size:      f.Size,
			Path:      fileName,
		}

		if data.DownloadFinished {
			file.Link = fmt.Sprintf("torbox://%s/%s", t.Id, strconv.Itoa(f.Id))
		}

		t.Files[fileName] = file
	}

	var cleanPath string
	if len(t.Files) > 0 {
		cleanPath = path.Clean(data.Files[0].Name)
	} else {
		cleanPath = path.Clean(data.Name)
	}

	t.OriginalFilename = strings.Split(cleanPath, "/")[0]
	t.Debrid = tb.config.Name
	return nil
}

func (tb *Torbox) CheckStatus(torrent *types.Torrent) (*types.Torrent, error) {
	for {
		err := tb.UpdateTorrent(torrent)

		if err != nil || torrent == nil {
			return torrent, err
		}

		switch torrent.Status {
		case types.TorrentStatusDownloaded:
			tb.logger.Info().Msgf("Torrent: %s downloaded", torrent.Name)
			return torrent, nil
		case types.TorrentStatusDownloading:
			if !torrent.DownloadUncached {
				return torrent, fmt.Errorf("torrent: %s not cached", torrent.Name)
			}
			return torrent, nil
		default:
			return torrent, fmt.Errorf("torrent: %s has error", torrent.Name)
		}
	}
}

func (tb *Torbox) DeleteTorrent(torrentId string) error {
	id, err := strconv.Atoi(torrentId)
	if err != nil {
		return fmt.Errorf("invalid TorBox torrent id %q: %w", torrentId, err)
	}
	payload := struct {
		TorrentID int    `json:"torrent_id"`
		Operation string `json:"operation"`
		All       bool   `json:"all"`
	}{
		TorrentID: id,
		Operation: "delete",
	}

	resp, err := tb.doPostJSON("/api/torrents/controltorrent", payload, nil)
	if err != nil {
		return err
	}

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("torbox API error: Status: %d", resp.StatusCode)
	}

	tb.logger.Info().Msgf("Torrent %s deleted from Torbox", torrentId)
	return nil
}

const (
	// TorBox documents download links as valid for three hours. Keep 15 minutes
	// of safety margin; an earlier expiry is caught by the stream's 400/403/410
	// invalidation and refetch path.
	cdnLinkLifetime = 2*time.Hour + 45*time.Minute
)

func (tb *Torbox) GetDownloadLink(id string, file *types.File) (types.DownloadLink, error) {
	return tb.accountsManager.GetDownloadLink(id, file, tb.fetchDownloadLink)
}

func (tb *Torbox) fetchDownloadLink(account *account.Account, id string, file *types.File) (types.DownloadLink, error) {
	now := time.Now()
	expiresAt := now.Add(tb.autoExpiresLinksAfter)

	// Resolve the CDN URL once, through the rate-limited client. Streaming the
	// redirect URL makes every range read (every Plex scan/analysis read) a
	// requestdl call against TorBox's account-wide API limit. The CDN URL is
	// kept for at most cdnLinkLifetime and refetched when the CDN rejects it.
	// Do not fall back to the API redirect URL. Every range read through that
	// URL is another TorBox API request outside this limiter, so a Plex probe or
	// scan during an API outage could turn one failed resolution into a burst.
	client := account.Client()
	if client == nil {
		client = tb.client
	}
	var res DownloadLinksResponse
	resp, err := tb.doGetWithClient(client, "/api/torrents/requestdl", map[string]string{
		"token":      account.Token,
		"torrent_id": id,
		"file_id":    file.Id,
		"redirect":   "false",
	}, &res)
	if err == nil && resp.StatusCode == http.StatusOK && res.Success && res.Data != nil && strings.HasPrefix(*res.Data, "http") {
		if cdnExp := now.Add(cdnLinkLifetime); cdnExp.Before(expiresAt) {
			expiresAt = cdnExp
		}
		return types.DownloadLink{
			Filename:     file.Name,
			Size:         file.Size,
			Token:        account.Token,
			Link:         file.Link,
			DownloadLink: *res.Data,
			Debrid:       tb.config.Name,
			Id:           file.Id,
			Generated:    now,
			ExpiresAt:    expiresAt,
		}, nil
	}

	status := 0
	if resp != nil {
		status = resp.StatusCode
	}
	if err != nil {
		return types.DownloadLink{}, fmt.Errorf("resolve TorBox CDN link (status %d): %w", status, err)
	}
	return types.DownloadLink{}, fmt.Errorf("resolve TorBox CDN link: status %d, success=%t", status, res.Success)
}

func (tb *Torbox) GetTorrents() ([]*types.Torrent, error) {
	offset := 0
	allTorrents := make([]*types.Torrent, 0)

	for {
		torrents, err := tb.getTorrents(offset)
		if err != nil {
			// A mid-pagination failure must propagate as an error, not be
			// swallowed — returning the partial list with a nil error here
			// makes the caller (syncTorrents) believe the fetch was complete,
			// causing it to treat every torrent on the un-fetched remaining
			// pages as "removed from remote" and delete it from local storage.
			return nil, err
		}
		if len(torrents) == 0 {
			break
		}
		allTorrents = append(allTorrents, torrents...)
		offset += len(torrents)
	}
	return allTorrents, nil
}

func (tb *Torbox) getTorrents(offset int) ([]*types.Torrent, error) {
	var res TorrentsListResponse

	resp, err := tb.doGet("/api/torrents/mylist", map[string]string{
		"bypass_cache": "true",
		"offset":       fmt.Sprintf("%d", offset),
	}, &res)
	if err != nil {
		return nil, err
	}

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("torbox API error: Status: %d", resp.StatusCode)
	}

	if !res.Success || res.Data == nil {
		return nil, fmt.Errorf("torbox API error: %v", res.Error)
	}

	torrents := make([]*types.Torrent, 0, len(*res.Data))
	cfg := config.Get()

	for _, data := range *res.Data {
		t := &types.Torrent{
			Id:               strconv.Itoa(data.Id),
			Name:             data.Name,
			Bytes:            data.Size,
			Progress:         data.Progress * 100,
			Status:           tb.getTorboxStatus(data.DownloadState, data.DownloadFinished),
			Speed:            data.DownloadSpeed,
			Seeders:          data.Seeds,
			Filename:         data.Name,
			OriginalFilename: data.Name,
			Debrid:           tb.config.Name,
			Files:            make(map[string]types.File),
			Added:            data.CreatedAt,
			InfoHash:         data.Hash,
		}

		for _, f := range data.Files {
			fileName := filepath.Base(f.Name)
			if err := cfg.IsFileAllowed(f.AbsolutePath, f.Size); err != nil {
				continue
			}
			file := types.File{
				TorrentId: t.Id,
				Id:        strconv.Itoa(f.Id),
				Name:      fileName,
				Size:      f.Size,
				Path:      f.Name,
			}

			if data.DownloadFinished {
				file.Link = fmt.Sprintf("torbox://%s/%d", t.Id, f.Id)
			}

			t.Files[fileName] = file
		}

		var cleanPath string
		if len(t.Files) > 0 {
			cleanPath = path.Clean(data.Files[0].Name)
		} else {
			cleanPath = path.Clean(data.Name)
		}
		t.OriginalFilename = strings.Split(cleanPath, "/")[0]

		torrents = append(torrents, t)
	}

	return torrents, nil
}

func (tb *Torbox) fetchDownloadLinks(account *account.Account) ([]types.DownloadLink, error) {
	return []types.DownloadLink{}, nil
}

func (tb *Torbox) RefreshDownloadLinks() error {
	return tb.accountsManager.RefreshLinks(tb.fetchDownloadLinks)
}

func (tb *Torbox) CheckFile(ctx context.Context, infohash, link string) error {
	tb.downloadPresentMu.Lock()
	if !tb.downloadPresentLoaded {
		if err := tb.loadDownloadPresent(); err != nil {
			tb.downloadPresentMu.Unlock()
			return err
		}
		tb.downloadPresentLoaded = true
	}
	tb.downloadPresentMu.Unlock()

	torrentID := link
	if strings.HasPrefix(link, "torbox://") {
		parts := strings.SplitN(strings.TrimPrefix(link, "torbox://"), "/", 2)
		if len(parts) > 0 {
			torrentID = parts[0]
		}
	}

	if present, ok := tb.downloadPresentCache.Load(torrentID); ok {
		if !present.(bool) {
			return customerror.HosterUnavailableError
		}
		return nil
	}
	return customerror.HosterUnavailableError
}

func (tb *Torbox) GetAvailableSlots() (int, error) {
	var accountSlots = 1
	profile, err := tb.GetProfile()
	if err != nil {
		return 0, err
	}

	if slots, ok := planSlots[profile.Type]; ok {
		accountSlots = slots
	}
	return accountSlots, nil
}

func (tb *Torbox) GetProfile() (*types.Profile, error) {
	if tb.Profile != nil {
		return tb.Profile, nil
	}
	var data ProfileResponse

	resp, err := tb.doGet("/api/user/me", map[string]string{"settings": "true"}, &data)
	if err != nil {
		return nil, err
	}

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("torbox API error: Status: %d", resp.StatusCode)
	}

	userData := data.Data
	if userData == nil {
		return nil, fmt.Errorf("error getting user profile")
	}

	expiration, err := time.Parse(time.RFC3339, userData.PremiumExpiresAt)
	if err != nil {
		expiration = time.Time{}
	}

	profile := &types.Profile{
		Name:       tb.config.Name,
		Id:         userData.Id,
		Username:   userData.Email,
		Email:      userData.Email,
		Expiration: expiration,
	}

	switch userData.Plan {
	case 1:
		profile.Type = "essential"
	case 2:
		profile.Type = "pro"
	case 3:
		profile.Type = "standard"
	default:
		profile.Type = "free"
	}

	tb.Profile = profile

	return profile, nil
}

func (tb *Torbox) AccountManager() *account.Manager {
	return tb.accountsManager
}

func (tb *Torbox) syncAccount(account *account.Account) error {
	return nil
}

func (tb *Torbox) SyncAccounts() {
	tb.accountsManager.Sync(tb.syncAccount)
}

func (tb *Torbox) deleteDownloadLink(account *account.Account, downloadLink types.DownloadLink) error {
	return nil
}

func (tb *Torbox) DeleteLink(downloadLink types.DownloadLink) error {
	return tb.accountsManager.DeleteDownloadLink(downloadLink, tb.deleteDownloadLink)
}

// SpeedTest measures API latency and download speed using cached links
func (tb *Torbox) SpeedTest(ctx context.Context) types.SpeedTestResult {
	result := types.SpeedTestResult{
		Provider: tb.config.Name,
		TestedAt: time.Now(),
	}

	start := time.Now()
	resp, err := tb.doGet("/api/user/me", nil, nil)
	latency := time.Since(start)

	if err != nil {
		result.Error = fmt.Sprintf("latency test failed: %v", err)
		return result
	}

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		result.Error = fmt.Sprintf("latency test unexpected status: %d", resp.StatusCode)
		return result
	}
	result.LatencyMs = latency.Milliseconds()

	// Try to measure download speed using a cached link
	current := tb.accountsManager.Current()
	if current == nil {
		return result
	}

	link, found := current.GetRandomLink()
	if !found || link.DownloadLink == "" {
		return result
	}

	// Download first 1MB to measure speed
	const downloadSize = 1 * 1024 * 1024 // 1MB
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, link.DownloadLink, nil)
	if err != nil {
		return result
	}
	req.Header.Set("Range", fmt.Sprintf("bytes=0-%d", downloadSize-1))

	downloadStart := time.Now()
	dlResp, err := current.Client().Do(req)
	if err != nil {
		return result
	}
	defer dlResp.Body.Close()

	data, err := io.ReadAll(dlResp.Body)
	downloadDuration := time.Since(downloadStart)

	if err != nil || len(data) == 0 {
		return result
	}

	result.BytesRead = int64(len(data))
	if downloadDuration.Seconds() > 0 {
		result.SpeedMBps = float64(result.BytesRead) / downloadDuration.Seconds() / (1024 * 1024)
	}

	return result
}

func (tb *Torbox) SupportsCheck() bool {
	return true
}
