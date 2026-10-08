//go:build windows

package a4c

import (
	"os"
	"syscall"
)

// osFlock on Windows uses syscall.LockFileEx.
func osFlock(f *os.File, blocking bool) (bool, error) {
	var flags uint32 = syscall.LOCKFILE_EXCLUSIVE_LOCK
	if !blocking {
		flags |= syscall.LOCKFILE_FAIL_IMMEDIATELY
	}
	var ov syscall.Overlapped
	err := syscall.LockFileEx(syscall.Handle(f.Fd()), flags, 0, 1, 0, &ov)
	if err != nil {
		return false, err
	}
	return true, nil
}

// osFlockUnlock on Windows uses syscall.UnlockFileEx.
func osFlockUnlock(f *os.File) error {
	var ov syscall.Overlapped
	return syscall.UnlockFileEx(syscall.Handle(f.Fd()), 0, 1, 0, &ov)
}
