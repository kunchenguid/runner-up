//go:build !windows

// Package lock holds the file locks that mark a place busy.
package lock

import (
	"errors"
	"os"
	"syscall"
)

// A place is busy exactly while some process holds its busy.lock. The
// coordinator takes the lock, hands the open file to the place's runner
// process and lets go of its own copy, so the kernel keeps the lock for as
// long as that process lives, across coordinator restarts, and drops it if
// the process dies.

// Busy takes path's exclusive lock without waiting. It returns the open,
// locked file, or nil when someone else holds it.
func Busy(path string) (*os.File, error) {
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		f.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return nil, nil
		}
		return nil, err
	}
	return f, nil
}

// HeldBy reports whether another process holds path's lock. A missing file is
// free. It never creates, writes or removes the file, and lets go at once.
func HeldBy(path string) (bool, error) {
	fd, err := syscall.Open(path, syscall.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK|syscall.O_CLOEXEC, 0)
	if err != nil {
		if errors.Is(err, syscall.ENOENT) {
			return false, nil
		}
		return false, err
	}
	defer syscall.Close(fd)
	if err := syscall.Flock(fd, syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return true, nil
		}
		return false, err
	}
	syscall.Flock(fd, syscall.LOCK_UN)
	return false, nil
}
