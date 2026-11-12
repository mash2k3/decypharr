package manager

import (
	"os"
	"sync"
	"syscall"
)

// ownMountFile is a file opened on cli_mount's own FUSE mount, served by this
// same process.
//
// Starting a child process (os/exec: ffprobe, rclone, unmount helpers) uses
// vfork: the parent thread can't be preempted until the child calls execve,
// and execve closes the child's copies of every O_CLOEXEC fd. Closing an fd on
// a FUSE mount sends a FLUSH to the FUSE server, which here is this process.
// If a stop-the-world (GC, runtime.ReadMemStats) starts in that window, the
// server goroutines are parked, the FLUSH is never answered, the child never
// reaches execve, the vforked thread never stops, and the stop-the-world never
// finishes. Every goroutine freezes: logs, HTTP, FUSE.
//
// os/exec holds syscall.ForkLock for writing while it forks. Holding it for
// reading for as long as an own-mount fd is open means a fork never happens
// while such an fd exists, so the child has nothing to FLUSH.
type ownMountFile struct {
	*os.File
	release sync.Once
}

// openOnOwnMount opens path, which must be on this process's own mount (or a
// symlink into it), and blocks child-process starts until the file is closed.
// Callers must Close the file; the lock is released after the fd is closed.
func openOnOwnMount(path string) (*ownMountFile, error) {
	syscall.ForkLock.RLock()
	f, err := os.Open(path)
	if err != nil {
		syscall.ForkLock.RUnlock()
		return nil, err
	}
	return &ownMountFile{File: f}, nil
}

// Close closes the fd, then lets child processes start again. Safe to call
// more than once.
func (f *ownMountFile) Close() error {
	err := f.File.Close()
	f.release.Do(syscall.ForkLock.RUnlock)
	return err
}
