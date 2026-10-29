package lock

import (
	"errors"
	"os"

	"golang.org/x/sys/windows"
)

// On Windows a place's lock is a byte-range lock on its file, which Windows
// drops when the process holding it ends, as flock is dropped on Linux and
// macOS. Only the agent takes locks there: the coordinator runs on Linux or
// macOS.

// Busy takes path's exclusive lock without waiting. It returns the open,
// locked file, or nil when someone else holds it.
// The file may be deleted while it is locked, as an agent's place lock is
// when the agent lets go; until the holder closes it, Windows may refuse to
// open it, which is the same as busy.
func Busy(path string) (*os.File, error) {
	p, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return nil, err
	}
	h, err := windows.CreateFile(p, windows.GENERIC_READ|windows.GENERIC_WRITE,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE, nil,
		windows.OPEN_ALWAYS, windows.FILE_ATTRIBUTE_NORMAL, 0)
	if errors.Is(err, windows.ERROR_ACCESS_DENIED) {
		return nil, nil
	}
	if errors.Is(err, windows.ERROR_PATH_NOT_FOUND) {
		return nil, os.ErrNotExist
	}
	if err != nil {
		return nil, &os.PathError{Op: "open", Path: path, Err: err}
	}
	f := os.NewFile(uintptr(h), path)
	ol := new(windows.Overlapped)
	err = windows.LockFileEx(windows.Handle(f.Fd()), windows.LOCKFILE_EXCLUSIVE_LOCK|windows.LOCKFILE_FAIL_IMMEDIATELY, 0, 1, 0, ol)
	if err != nil {
		f.Close()
		if errors.Is(err, windows.ERROR_LOCK_VIOLATION) {
			return nil, nil
		}
		return nil, err
	}
	return f, nil
}

// HeldBy reports whether another process holds path's lock. A missing file is
// free.
func HeldBy(path string) (bool, error) {
	f, err := os.OpenFile(path, os.O_RDONLY, 0)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	defer f.Close()
	ol := new(windows.Overlapped)
	h := windows.Handle(f.Fd())
	if err := windows.LockFileEx(h, windows.LOCKFILE_EXCLUSIVE_LOCK|windows.LOCKFILE_FAIL_IMMEDIATELY, 0, 1, 0, ol); err != nil {
		if errors.Is(err, windows.ERROR_LOCK_VIOLATION) {
			return true, nil
		}
		return false, err
	}
	windows.UnlockFileEx(h, 0, 1, 0, ol)
	return false, nil
}
