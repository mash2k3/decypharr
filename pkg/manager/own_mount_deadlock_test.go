//go:build fusedeadlock

package manager

// Opt-in reproduction of the fork + own-FUSE-mount + stop-the-world deadlock.
// Needs /dev/fuse and fusermount. A deadlock freezes the whole test binary
// (timers included), so run it under an external timeout:
//
//	timeout 120 go test -tags fusedeadlock -run TestOwnMountForkDeadlock -v ./pkg/manager/
//	OWN_MOUNT_RAW_OPEN=1 timeout 120 go test ...   # old behaviour, expected to hang
//
// Then `fusermount -uz` any mount left in the temp dir if the run was killed.

import (
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hanwen/go-fuse/v2/fs"
)

func TestOwnMountForkDeadlock(t *testing.T) {
	src := t.TempDir()
	mnt := t.TempDir()
	data := make([]byte, 8<<20)
	for i := range 4 {
		if err := os.WriteFile(filepath.Join(src, "f"+string(rune('a'+i))), data, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	root, err := fs.NewLoopbackRoot(src)
	if err != nil {
		t.Fatal(err)
	}
	server, err := fs.Mount(mnt, root, &fs.Options{})
	if err != nil {
		t.Skipf("fuse mount unavailable: %v", err)
	}
	defer func() { _ = server.Unmount() }()

	raw := os.Getenv("OWN_MOUNT_RAW_OPEN") == "1"
	open := func(p string) (io.ReadCloser, error) {
		if raw {
			return os.Open(p)
		}
		return openOnOwnMount(p)
	}

	var stop atomic.Bool
	var reads, forks, stws atomic.Int64
	var wg sync.WaitGroup
	for i := range 16 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			buf := make([]byte, 1<<20)
			p := filepath.Join(mnt, "f"+string(rune('a'+i%4)))
			for !stop.Load() {
				f, err := open(p)
				if err != nil {
					continue
				}
				for range 2 {
					_, _ = f.Read(buf)
				}
				_ = f.Close()
				reads.Add(1)
			}
		}()
	}
	for range 4 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for !stop.Load() {
				_ = exec.Command("/bin/true").Run()
				forks.Add(1)
			}
		}()
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		var ms runtime.MemStats
		for !stop.Load() {
			runtime.ReadMemStats(&ms)
			stws.Add(1)
		}
	}()

	time.Sleep(30 * time.Second)
	stop.Store(true)
	wg.Wait()
	t.Logf("raw=%v reads=%d forks=%d stop-the-worlds=%d", raw, reads.Load(), forks.Load(), stws.Load())
}
