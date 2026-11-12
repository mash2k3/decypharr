package manager

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

// forkLockFree reports whether a child process could start right now.
func forkLockFree() bool {
	if !syscall.ForkLock.TryLock() {
		return false
	}
	syscall.ForkLock.Unlock()
	return true
}

func TestOpenOnOwnMountBlocksForkUntilClosed(t *testing.T) {
	path := filepath.Join(t.TempDir(), "media.mkv")
	if err := os.WriteFile(path, []byte("data"), 0o644); err != nil {
		t.Fatal(err)
	}

	f, err := openOnOwnMount(path)
	if err != nil {
		t.Fatal(err)
	}
	if forkLockFree() {
		t.Fatal("child processes can start while an own-mount file is open")
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	if !forkLockFree() {
		t.Fatal("fork lock still held after Close")
	}
	// A second Close must not release the read lock again.
	_ = f.Close()
	if !forkLockFree() {
		t.Fatal("fork lock state broken by a second Close")
	}
}

func TestOpenOnOwnMountReleasesForkLockOnError(t *testing.T) {
	if _, err := openOnOwnMount(filepath.Join(t.TempDir(), "missing")); err == nil {
		t.Fatal("expected an error opening a missing file")
	}
	if !forkLockFree() {
		t.Fatal("fork lock still held after a failed open")
	}
}
