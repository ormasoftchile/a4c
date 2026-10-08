//go:build !windows

package a4c

import (
	"errors"
	"os"
	"syscall"
)

// osFlock attempts non-blocking exclusive flock.
func osFlock(f *os.File, blocking bool) (bool, error) {
	flag := syscall.LOCK_EX
	if !blocking {
		flag |= syscall.LOCK_NB
	}
	err := syscall.Flock(int(f.Fd()), flag)
	if err != nil {
		if errors.Is(err, syscall.EWOULDBLOCK) || errors.Is(err, syscall.EAGAIN) {
			return false, err
		}
		return false, err
	}
	return true, nil
}

// osFlockUnlock unlocks the flock.
func osFlockUnlock(f *os.File) error {
	return syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
}
