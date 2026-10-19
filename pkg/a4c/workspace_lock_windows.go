//go:build windows

package a4c

import (
	"os"

	"golang.org/x/sys/windows"
)

// lockRegionOffset is where the single-writer lock byte lives. Windows byte
// range locks also block other handles from reading the locked region, so the
// lock is taken far beyond the metadata the file actually holds. The holder's
// PID/coordinator metadata must stay readable by the next process that loses
// the race, so the diagnostic can name the holder.
const lockRegionOffset = 0x7FFFFFFF

// openLockFile opens (creating it if needed) the workspace lock file. It shares
// read, write and DELETE access, matching POSIX semantics where an open, locked
// file can still be unlinked; otherwise a coordinator holding the lock would
// keep the .a4c directory from ever being cleaned up.
func openLockFile(path string) (*os.File, error) {
	pathp, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return nil, err
	}
	h, err := windows.CreateFile(
		pathp,
		windows.GENERIC_READ|windows.GENERIC_WRITE,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE,
		nil,
		windows.OPEN_ALWAYS,
		windows.FILE_ATTRIBUTE_NORMAL,
		0,
	)
	if err != nil {
		return nil, err
	}
	return os.NewFile(uintptr(h), path), nil
}

// osFlock on Windows locks one byte at lockRegionOffset via LockFileEx.
func osFlock(f *os.File, blocking bool) (bool, error) {
	var flags uint32 = windows.LOCKFILE_EXCLUSIVE_LOCK
	if !blocking {
		flags |= windows.LOCKFILE_FAIL_IMMEDIATELY
	}
	ov := windows.Overlapped{Offset: lockRegionOffset}
	err := windows.LockFileEx(windows.Handle(f.Fd()), flags, 0, 1, 0, &ov)
	if err != nil {
		return false, err
	}
	return true, nil
}

// osFlockUnlock on Windows releases the byte locked by osFlock.
func osFlockUnlock(f *os.File) error {
	ov := windows.Overlapped{Offset: lockRegionOffset}
	return windows.UnlockFileEx(windows.Handle(f.Fd()), 0, 1, 0, &ov)
}
