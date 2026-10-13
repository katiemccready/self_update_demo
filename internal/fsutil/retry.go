package fsutil

import (
	"os"
	"time"
)

func Retry(attempts int, delay time.Duration, retryable func(error) bool, fn func() error) error {
	var err error
	for i := 1; i <= attempts; i++ {
		if err = fn(); err == nil || !retryable(err) {
			return err
		}
		if i < attempts {
			time.Sleep(time.Duration(i) * delay)
		}
	}
	return err
}

const (
	renameAttempts = 5
	renameDelay    = 200 * time.Millisecond
)

func Rename(oldpath, newpath string) error {
	return Retry(renameAttempts, renameDelay, isTransientRenameErr, func() error { return os.Rename(oldpath, newpath) })
}

func ReadFile(path string) ([]byte, error) {
	var b []byte
	err := Retry(renameAttempts, renameDelay, isTransientRenameErr, func() (err error) {
		b, err = os.ReadFile(path)
		return err
	})
	return b, err
}
