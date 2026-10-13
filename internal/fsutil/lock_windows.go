//go:build windows

package fsutil

import (
	"syscall"
	"time"
)

type lockHandle struct{ h syscall.Handle }

const (
	accessDeniedRetries   = 5
	fileFlagDeleteOnClose = 0x04000000
	accessDelete          = 0x00010000

	errAccessDenied     = syscall.Errno(5)
	errSharingViolation = syscall.Errno(32)
	errLockViolation    = syscall.Errno(33)
)

func lockFile(path string) (lockHandle, error) {
	p, err := syscall.UTF16PtrFromString(path)
	if err != nil {
		return lockHandle{}, err
	}

	for i := 0; i < accessDeniedRetries; i++ {
		var h syscall.Handle
		h, err = syscall.CreateFile(p, syscall.GENERIC_WRITE|accessDelete, 0, nil, syscall.OPEN_ALWAYS,
			syscall.FILE_ATTRIBUTE_NORMAL|fileFlagDeleteOnClose, 0)
		if err == nil {
			return lockHandle{h}, nil
		}
		if err == errSharingViolation || err == errLockViolation {
			return lockHandle{}, ErrLockHeld
		}
		if err != errAccessDenied {
			return lockHandle{}, err
		}
		time.Sleep(20 * time.Millisecond)
	}
	return lockHandle{}, err
}

func unlockFile(h lockHandle) { syscall.CloseHandle(h.h) }
