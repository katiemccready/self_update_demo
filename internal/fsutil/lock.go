package fsutil

import (
	"errors"
	"time"
)

type FileLock struct {
	path string
	h    lockHandle
	held bool
}

var ErrLockHeld = errors.New("lock held by another process")

func NewFileLock(path string) *FileLock { return &FileLock{path: path} }

func (l *FileLock) TryLock() error {
	if l.held {
		return errors.New("lock already held by this handle")
	}
	h, err := lockFile(l.path)
	if err != nil {
		return err
	}
	l.h, l.held = h, true
	return nil
}

func (l *FileLock) Lock(timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for {
		err := l.TryLock()
		if !errors.Is(err, ErrLockHeld) || time.Now().After(deadline) {
			return err
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func (l *FileLock) Unlock() {
	if l.held {
		unlockFile(l.h)
		l.h, l.held = lockHandle{}, false
	}
}
