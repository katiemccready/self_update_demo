//go:build windows

package fsutil

import (
	"errors"
	"syscall"
)

func isTransientRenameErr(err error) bool {
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
