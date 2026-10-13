package fsutil

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestRetryRidesOutTransientErrors(t *testing.T) {
	locked := errors.New("sharing violation")
	n := 0
	flaky := func() error {
		if n++; n < 3 {
			return locked
		}
		return nil
	}
	if err := Retry(5, time.Millisecond, func(err error) bool { return errors.Is(err, locked) }, flaky); err != nil || n != 3 {
		t.Fatalf("err=%v after %d attempts, want success on the 3rd", err, n)
	}

	n = 0
	permanent := errors.New("access denied (real)")
	err := Retry(5, time.Millisecond, func(error) bool { return false }, func() error { n++; return permanent })
	if !errors.Is(err, permanent) || n != 1 {
		t.Errorf("non-transient error retried %d times (err=%v)", n, err)
	}

	n = 0
	err = Retry(4, time.Millisecond, func(error) bool { return true }, func() error { n++; return locked })
	if !errors.Is(err, locked) || n != 4 {
		t.Errorf("gave up after %d attempts (err=%v), want exactly 4", n, err)
	}
}

func TestReadFileReadsAndReportsMissing(t *testing.T) {
	p := filepath.Join(t.TempDir(), "f")
	if _, err := ReadFile(p); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("missing file: %v", err)
	}
	os.WriteFile(p, []byte("x"), 0o644)
	if b, err := ReadFile(p); err != nil || string(b) != "x" {
		t.Fatalf("got %q, %v", b, err)
	}
}
