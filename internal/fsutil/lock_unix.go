//go:build !windows

package fsutil

import (
	"errors"
	"os"
	"syscall"
)

type lockHandle struct{ f *os.File }

func lockFile(path string) (lockHandle, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return lockHandle{}, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		f.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) || errors.Is(err, syscall.EAGAIN) {
			return lockHandle{}, ErrLockHeld
		}
		return lockHandle{}, err
	}
	return lockHandle{f}, nil
}

func unlockFile(h lockHandle) { h.f.Close() }
