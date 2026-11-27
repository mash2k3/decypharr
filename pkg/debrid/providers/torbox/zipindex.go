package torbox

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/sirrobot01/decypharr/internal/config"
	"github.com/sirrobot01/decypharr/pkg/debrid/types"
)

// TorBox stores a torrent with 100+ files as one .zip when it was added
// with allow_zip (its default). SubmitMagnet sends allow_zip=false, but a
// hash someone else cached zipped first is handed back zipped anyway
// (Pokemon S01-S19 SRS: one 686 GB zip of 2,847 files). The zips TorBox
// builds are stored (uncompressed), so each member is a plain byte range
// of the zip: listed as its own file with a ByteRange, it streams from the
// zip's link like any other file.
//
// The index (member path, size, data offset) is read once from the zip's
// central directory with a few range requests and kept on disk.

const zipIndexMaxMembers = 100_000

type zipMember struct {
	Path   string `json:"path"`
	Size   int64  `json:"size"`
	Offset int64  `json:"offset"`
}

var zipIndexes = struct {
	sync.Mutex
	m       map[string][]zipMember
	failed  map[string]time.Time
	reading map[string]bool
}{m: map[string][]zipMember{}, failed: map[string]time.Time{}, reading: map[string]bool{}}

// A zip whose index couldn't be read is retried after this long.
var zipIndexRetry = 30 * time.Minute

func zipIndexKey(hash string, fileID int, size int64) string {
	return fmt.Sprintf("%s_%d_%d", strings.ToLower(hash), fileID, size)
}

func zipIndexPath(key string) string {
	return filepath.Join(config.GetMainPath(), "torbox_zip_index", key+".json")
}

func cachedZipIndex(key string) ([]zipMember, bool) {
	zipIndexes.Lock()
	m, ok := zipIndexes.m[key]
	zipIndexes.Unlock()
	if ok {
		return m, true
	}
	b, err := os.ReadFile(zipIndexPath(key))
	if err != nil {
		return nil, false
	}
	var stored struct {
		Members []zipMember `json:"members"`
	}
	if json.Unmarshal(b, &stored) != nil || len(stored.Members) == 0 {
		return nil, false
	}
	zipIndexes.Lock()
	zipIndexes.m[key] = stored.Members
	zipIndexes.Unlock()
	return stored.Members, true
}

func storeZipIndex(key string, members []zipMember) {
	zipIndexes.Lock()
	zipIndexes.m[key] = members
	zipIndexes.Unlock()
	p := zipIndexPath(key)
	b, _ := json.Marshal(struct {
		Members []zipMember `json:"members"`
	}{members})
	if os.MkdirAll(filepath.Dir(p), 0o755) == nil && os.WriteFile(p+".tmp", b, 0o644) == nil {
		_ = os.Rename(p+".tmp", p)
	}
}

// singleZip is the torrent's one .zip file, when that is all it has.
func singleZip(data *torboxInfo) (id int, name string, size int64, ok bool) {
	if !data.DownloadFinished || len(data.Files) != 1 {
		return 0, "", 0, false
	}
	f := data.Files[0]
	if !strings.HasSuffix(strings.ToLower(f.Name), ".zip") || f.Size <= 0 {
		return 0, "", 0, false
	}
	return f.Id, f.Name, f.Size, true
}

// zipFiles fills t.Files with the members of a single stored zip. build
// allows reading the index over the network; listings pass false and only
// use an index already read. Reports whether it handled the torrent.
func (tb *Torbox) zipFiles(t *types.Torrent, data *torboxInfo, build bool) bool {
	fid, zname, zsize, ok := singleZip(data)
	if !ok {
		return false
	}
	key := zipIndexKey(data.Hash, fid, zsize)
	members, ok := cachedZipIndex(key)
	if !ok {
		if !build || !tb.startZipRead(key) {
			return false
		}
		var err error
		members, err = tb.readZipIndexFor(t.Id, fid, zsize)
		tb.endZipRead(key, err)
		if err != nil {
			tb.logger.Warn().Err(err).Msgf("TorBox zip %s: index not read", zname)
			return false
		}
		storeZipIndex(key, members)
		tb.logger.Info().Msgf("TorBox zip %s: %d files inside", zname, len(members))
	}
	cfg := config.Get()
	link := fmt.Sprintf("torbox://%s/%d", t.Id, fid)
	root := path.Dir(path.Clean(strings.ReplaceAll(zname, `\`, "/")))
	added := 0
	for _, m := range members {
		name := path.Base(m.Path)
		if cfg.IsFileAllowed(name, m.Size) != nil {
			continue
		}
		if _, dup := t.Files[name]; dup { // same base name in two folders
			name = strings.ReplaceAll(strings.Trim(path.Dir(m.Path), "/"), "/", " - ") + " - " + name
		}
		full := m.Path
		if root != "." && root != "/" {
			full = root + "/" + m.Path
		}
		t.Files[name] = types.File{
			TorrentId: t.Id,
			Id:        strconv.Itoa(fid),
			Name:      name,
			Size:      m.Size,
			Path:      full,
			ByteRange: &[2]int64{m.Offset, m.Offset + m.Size - 1},
			Link:      link,
		}
		added++
	}
	return added > 0
}

func (tb *Torbox) startZipRead(key string) bool {
	zipIndexes.Lock()
	defer zipIndexes.Unlock()
	if zipIndexes.reading[key] || time.Since(zipIndexes.failed[key]) < zipIndexRetry {
		return false
	}
	zipIndexes.reading[key] = true
	return true
}

func (tb *Torbox) endZipRead(key string, err error) {
	zipIndexes.Lock()
	defer zipIndexes.Unlock()
	delete(zipIndexes.reading, key)
	if err != nil {
		zipIndexes.failed[key] = time.Now()
	} else {
		delete(zipIndexes.failed, key)
	}
}

func (tb *Torbox) readZipIndexFor(torrentID string, fid int, size int64) ([]zipMember, error) {
	dl, err := tb.GetDownloadLink(torrentID, &types.File{TorrentId: torrentID, Id: strconv.Itoa(fid),
		Link: fmt.Sprintf("torbox://%s/%d", torrentID, fid)})
	if err != nil {
		return nil, fmt.Errorf("download link: %w", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	return readZipIndex(ctx, &http.Client{Timeout: time.Minute}, dl.DownloadLink, size)
}

type rangeReader struct {
	ctx    context.Context
	client *http.Client
	url    string
	size   int64
}

func (r rangeReader) read(start, n int64) ([]byte, error) {
	if start < 0 || n <= 0 || start+n > r.size {
		return nil, fmt.Errorf("range %d+%d outside the zip (%d)", start, n, r.size)
	}
	var last error
	for attempt := 0; attempt < 3; attempt++ {
		if attempt > 0 {
			select {
			case <-r.ctx.Done():
				return nil, r.ctx.Err()
			case <-time.After(time.Duration(1+2*attempt) * time.Second):
			}
		}
		req, err := http.NewRequestWithContext(r.ctx, http.MethodGet, r.url, nil)
		if err != nil {
			return nil, err
		}
		req.Header.Set("Range", fmt.Sprintf("bytes=%d-%d", start, start+n-1))
		resp, err := r.client.Do(req)
		if err != nil {
			last = err
			continue
		}
		b, err := io.ReadAll(io.LimitReader(resp.Body, n+1))
		resp.Body.Close()
		if resp.StatusCode != http.StatusPartialContent {
			last = fmt.Errorf("range request answered %d", resp.StatusCode)
			continue
		}
		if err != nil {
			last = err
			continue
		}
		if int64(len(b)) != n {
			return nil, fmt.Errorf("range request returned %d of %d bytes", len(b), n)
		}
		return b, nil
	}
	return nil, last
}

type zipRow struct {
	name                string
	nameLen             int
	flags, method       uint16
	csize, usize, local int64
	next, data          int64
}

// readZipIndex lists a zip's stored members over HTTP ranges: the tail
// and the central directory, and three local headers to confirm the
// offsets it derives (a stored member without a data descriptor ends
// where the next local header starts). Other layouts read each header.
func readZipIndex(ctx context.Context, client *http.Client, url string, size int64) ([]zipMember, error) {
	r := rangeReader{ctx: ctx, client: client, url: url, size: size}
	le := binary.LittleEndian
	tailLen := min(size, 22+65535)
	tail, err := r.read(size-tailLen, tailLen)
	if err != nil {
		return nil, err
	}
	i := bytes.LastIndex(tail, []byte("PK\x05\x06"))
	if i < 0 || len(tail)-i < 22 {
		return nil, errors.New("zip end of central directory not found")
	}
	entries := uint64(le.Uint16(tail[i+10:]))
	cdSize, cdOff := int64(le.Uint32(tail[i+12:])), int64(le.Uint32(tail[i+16:]))
	if entries == 0xffff || cdSize == 0xffffffff || cdOff == 0xffffffff {
		j := i - 20
		if j < 0 || !bytes.HasPrefix(tail[j:], []byte("PK\x06\x07")) {
			return nil, errors.New("zip64 locator missing")
		}
		rec, err := r.read(int64(le.Uint64(tail[j+8:])), 56)
		if err != nil {
			return nil, err
		}
		if !bytes.HasPrefix(rec, []byte("PK\x06\x06")) {
			return nil, errors.New("zip64 end record missing")
		}
		entries, cdSize, cdOff = le.Uint64(rec[32:]), int64(le.Uint64(rec[40:])), int64(le.Uint64(rec[48:]))
	}
	if entries > zipIndexMaxMembers || cdOff < 0 || cdOff+cdSize > size {
		return nil, fmt.Errorf("zip central directory out of range (%d entries)", entries)
	}
	cd, err := r.read(cdOff, cdSize)
	if err != nil {
		return nil, err
	}
	rows := make([]*zipRow, 0, entries)
	k := 0
	for e := uint64(0); e < entries; e++ {
		if k+46 > len(cd) || !bytes.HasPrefix(cd[k:], []byte("PK\x01\x02")) {
			return nil, errors.New("bad zip central directory entry")
		}
		x := &zipRow{flags: le.Uint16(cd[k+8:]), method: le.Uint16(cd[k+10:]),
			csize: int64(le.Uint32(cd[k+20:])), usize: int64(le.Uint32(cd[k+24:])), local: int64(le.Uint32(cd[k+42:]))}
		nl, el, cl := int(le.Uint16(cd[k+28:])), int(le.Uint16(cd[k+30:])), int(le.Uint16(cd[k+32:]))
		if k+46+nl+el > len(cd) {
			return nil, errors.New("bad zip central directory entry")
		}
		x.name, x.nameLen = strings.ReplaceAll(string(cd[k+46:k+46+nl]), `\`, "/"), nl
		zip64Fields(cd[k+46+nl:k+46+nl+el], &x.usize, &x.csize, &x.local)
		k += 46 + nl + el + cl
		rows = append(rows, x)
	}
	order := append([]*zipRow(nil), rows...)
	sort.Slice(order, func(a, b int) bool { return order[a].local < order[b].local })
	for n, x := range order {
		x.next = cdOff
		if n+1 < len(order) {
			x.next = order[n+1].local
		}
	}
	var want []*zipRow
	for _, x := range rows {
		if strings.HasSuffix(x.name, "/") || x.usize <= 0 || x.method != 0 || x.flags&1 != 0 || x.csize != x.usize {
			continue
		}
		x.data = -1
		if x.flags&0x08 == 0 {
			start := x.next - x.usize
			if gap := start - x.local - 30 - int64(x.nameLen); gap >= 0 && gap <= 0xffff {
				x.data = start
			}
		}
		want = append(want, x)
	}
	localData := func(x *zipRow) (int64, error) {
		lh, err := r.read(x.local, 30)
		if err != nil {
			return 0, err
		}
		if !bytes.HasPrefix(lh, []byte("PK\x03\x04")) {
			return 0, fmt.Errorf("zip local header missing for %s", x.name)
		}
		return x.local + 30 + int64(le.Uint16(lh[26:])) + int64(le.Uint16(lh[28:])), nil
	}
	var derived []*zipRow
	for _, x := range want {
		if x.data >= 0 {
			derived = append(derived, x)
		}
	}
	readAll := false
	if len(derived) > 0 {
		for _, n := range []int{0, len(derived) / 2, len(derived) - 1} {
			got, err := localData(derived[n])
			if err != nil {
				return nil, err
			}
			if got != derived[n].data {
				readAll = true
				break
			}
		}
	}
	for _, x := range want {
		if readAll || x.data < 0 {
			if x.data, err = localData(x); err != nil {
				return nil, err
			}
		}
	}
	out := make([]zipMember, 0, len(want))
	for _, x := range want {
		if x.data+x.usize <= size {
			out = append(out, zipMember{Path: strings.TrimLeft(x.name, "/"), Size: x.usize, Offset: x.data})
		}
	}
	if len(out) == 0 {
		return nil, errors.New("no stored files in the zip")
	}
	return out, nil
}

// zip64Fields replaces 0xffffffff sizes/offset with their ZIP64 extra values.
func zip64Fields(extra []byte, usize, csize, local *int64) {
	le := binary.LittleEndian
	for len(extra) >= 4 {
		id, n := le.Uint16(extra), int(le.Uint16(extra[2:]))
		if 4+n > len(extra) {
			return
		}
		if id == 0x0001 {
			f := extra[4 : 4+n]
			for _, p := range []*int64{usize, csize, local} {
				if *p == 0xffffffff && len(f) >= 8 {
					*p = int64(le.Uint64(f))
					f = f[8:]
				}
			}
			return
		}
		extra = extra[4+n:]
	}
}
