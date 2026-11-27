package torbox

import (
	"archive/zip"
	"bytes"
	"context"
	"fmt"
	"hash/crc32"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/sirrobot01/decypharr/pkg/debrid/types"
)

type zm struct {
	name   string
	data   []byte
	method uint16
}

// buildZip writes members the way TorBox's zips look (stored, sizes known
// up front, no data descriptor) or, with descriptors, the way a streaming
// writer leaves them.
func buildZip(t *testing.T, members []zm, descriptors bool) []byte {
	t.Helper()
	var buf bytes.Buffer
	w := zip.NewWriter(&buf)
	if _, err := w.Create("Pack/"); err != nil {
		t.Fatal(err)
	}
	for _, m := range members {
		if descriptors || m.method != zip.Store {
			f, err := w.CreateHeader(&zip.FileHeader{Name: m.name, Method: m.method})
			if err != nil {
				t.Fatal(err)
			}
			f.Write(m.data)
			continue
		}
		f, err := w.CreateRaw(&zip.FileHeader{Name: m.name, Method: zip.Store, CRC32: crc32.ChecksumIEEE(m.data),
			CompressedSize64: uint64(len(m.data)), UncompressedSize64: uint64(len(m.data))})
		if err != nil {
			t.Fatal(err)
		}
		f.Write(m.data)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func serveRanges(t *testing.T, blob []byte) (*httptest.Server, *atomic.Int64) {
	var n atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n.Add(1)
		spec := strings.TrimPrefix(r.Header.Get("Range"), "bytes=")
		a, _ := strconv.ParseInt(strings.Split(spec, "-")[0], 10, 64)
		b, _ := strconv.ParseInt(strings.Split(spec, "-")[1], 10, 64)
		w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", a, b, len(blob)))
		w.WriteHeader(http.StatusPartialContent)
		w.Write(blob[a : b+1])
	}))
	t.Cleanup(srv.Close)
	return srv, &n
}

var packMembers = []zm{
	{"Pack/S01/Show.S01E01.mkv", bytes.Repeat([]byte("A"), 5000), zip.Store},
	{"Pack/S01/Show.S01E02.mkv", bytes.Repeat([]byte("B"), 7000), zip.Store},
	{"Pack/S02/Show.S02E01.mkv", bytes.Repeat([]byte("C"), 3000), zip.Store},
	{"Pack/notes.nfo", bytes.Repeat([]byte("x"), 900), zip.Deflate},
}

func TestReadZipIndexGivesEachStoredMemberItsBytes(t *testing.T) {
	for _, desc := range []bool{false, true} {
		blob := buildZip(t, packMembers, desc)
		srv, requests := serveRanges(t, blob)
		got, err := readZipIndex(context.Background(), srv.Client(), srv.URL, int64(len(blob)))
		if err != nil {
			t.Fatalf("descriptors=%v: %v", desc, err)
		}
		if len(got) != 3 {
			t.Fatalf("descriptors=%v: want the 3 stored members, got %+v", desc, got)
		}
		for i, m := range got {
			want := packMembers[i]
			if m.Path != want.name || m.Size != int64(len(want.data)) ||
				!bytes.Equal(blob[m.Offset:m.Offset+m.Size], want.data) {
				t.Fatalf("descriptors=%v: member %d = %+v", desc, i, m)
			}
		}
		if !desc && requests.Load() > 5 {
			t.Fatalf("a TorBox-style zip should need the tail, the directory and 3 spot checks, not %d requests", requests.Load())
		}
	}
}

func TestZipFilesListsMembersAsByteRanges(t *testing.T) {
	data := &torboxInfo{Hash: "ABC", DownloadFinished: true}
	data.Files = append(data.Files, struct {
		Id           int         `json:"id"`
		Md5          interface{} `json:"md5"`
		Hash         string      `json:"hash"`
		Name         string      `json:"name"`
		Size         int64       `json:"size"`
		Zipped       bool        `json:"zipped"`
		S3Path       string      `json:"s3_path"`
		Infected     bool        `json:"infected"`
		Mimetype     string      `json:"mimetype"`
		ShortName    string      `json:"short_name"`
		AbsolutePath string      `json:"absolute_path"`
	}{Id: 0, Name: "Root/Root.zip", Size: 99999})
	key := zipIndexKey(data.Hash, 0, 99999)
	zipIndexes.Lock()
	zipIndexes.m[key] = []zipMember{{Path: "Pack/S01/Show.S01E01.mkv", Size: 100, Offset: 40}}
	zipIndexes.Unlock()
	t.Cleanup(func() { zipIndexes.Lock(); delete(zipIndexes.m, key); zipIndexes.Unlock() })

	tb := &Torbox{}
	torrent := &types.Torrent{Id: "7", Files: map[string]types.File{}}
	if !tb.zipFiles(torrent, data, false) {
		t.Fatal("zip not expanded from a stored index")
	}
	f, ok := torrent.Files["Show.S01E01.mkv"]
	if !ok || f.ByteRange == nil || f.ByteRange[0] != 40 || f.ByteRange[1] != 139 || f.Id != "0" ||
		f.Link != "torbox://7/0" || f.Path != "Root/Pack/S01/Show.S01E01.mkv" {
		t.Fatalf("member file = %+v", f)
	}
	if tb.zipFiles(&types.Torrent{Id: "8", Files: map[string]types.File{}}, &torboxInfo{Hash: "ABC", DownloadFinished: true}, false) {
		t.Fatal("a torrent without a single zip was handled")
	}
}
