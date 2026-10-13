//go:build windows

package fsutil

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestAtomicWriteWaitsForAReaderToLetGo(t *testing.T) {
	path := filepath.Join(t.TempDir(), "current")
	if err := os.WriteFile(path, []byte("1.0.0\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	r, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	go func() { time.Sleep(400 * time.Millisecond); r.Close() }()
	if err := IgnoreDirSync(WriteFileAtomic(path, []byte("1.1.0\n"), 0o644)); err != nil {
		t.Fatalf("write while a reader held the file: %v", err)
	}
	if b, _ := os.ReadFile(path); string(b) != "1.1.0\n" {
		t.Errorf("content %q", b)
	}
}
