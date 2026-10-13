//go:build windows

package update

import (
	"errors"
	"syscall"
	"time"
)

const (
	textBusyAttempts = 8
	textBusyDelay    = 300 * time.Millisecond
)

const (
	errAccessDenied     = syscall.Errno(5)
	errSharingViolation = syscall.Errno(32)
	errLockViolation    = syscall.Errno(33)
)

func isTextBusy(err error) bool { return isBusyErrno(err) }

func isBusyErrno(err error) bool {
	var errno syscall.Errno
	if !errors.As(err, &errno) {
		return false
	}
	switch errno {
	case errAccessDenied, errSharingViolation, errLockViolation:
		return true
	}
	return false
}
