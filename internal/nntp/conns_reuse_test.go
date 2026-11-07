package nntp

import (
	"bufio"
	"bytes"
	"fmt"
	"io"
	"net"
	"net/textproto"
	"testing"

	"github.com/rs/zerolog"
	nntpyenc "github.com/sirrobot01/decypharr/internal/nntp/yenc"
)

// TestStreamBodyReusesConnectionAcrossArticles drives two BODY commands over
// one connection with a production-sized (256 KiB) reader, which the pure-Go
// decoder shares instead of wrapping. The Windows release is built without CGO,
// so it always takes the pure-Go path; there the article's ".\r\n" terminator
// used to stay on the reader and the second BODY failed with
// "invalid response code: .".
func TestStreamBodyReusesConnectionAcrossArticles(t *testing.T) {
	for _, pureGo := range []bool{false, true} {
		t.Run(fmt.Sprintf("pureGo=%v", pureGo), func(t *testing.T) {
			original := nntpyenc.UsePureGo
			nntpyenc.UsePureGo = pureGo
			defer func() { nntpyenc.UsePureGo = original }()

			payloads := [][]byte{
				bytes.Repeat([]byte("first-article-"), 64),
				bytes.Repeat([]byte("second-article-"), 64),
			}
			clientSide, serverSide := net.Pipe()
			reader := bufio.NewReaderSize(clientSide, 256*1024)
			conn := &Connection{
				conn: clientSide, reader: reader, text: textproto.NewReader(reader),
				writer: bufio.NewWriter(clientSide), logger: zerolog.Nop(),
			}
			t.Cleanup(func() {
				_ = clientSide.Close()
				_ = serverSide.Close()
			})
			go func() {
				commandReader := bufio.NewReader(serverSide)
				for _, payload := range payloads {
					if _, err := commandReader.ReadString('\n'); err != nil {
						return
					}
					_, _ = io.WriteString(serverSide, "222 0 <test> body follows\r\n"+yencTestBody(payload))
				}
			}()

			for i, payload := range payloads {
				var output bytes.Buffer
				if _, err := conn.StreamBody(fmt.Sprintf("article-%d", i), &output); err != nil {
					t.Fatalf("article %d on reused connection: %v", i, err)
				}
				if !bytes.Equal(output.Bytes(), payload) {
					t.Fatalf("article %d: got %d bytes, want %d", i, output.Len(), len(payload))
				}
			}
		})
	}
}
