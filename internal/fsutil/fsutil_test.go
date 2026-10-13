package fsutil

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestWriteFileAtomicReplacesAndLeavesNoTemp(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "f")
	for _, content := range []string{"one", "two-longer"} {
		if err := WriteFileAtomic(p, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
		if b, _ := os.ReadFile(p); string(b) != content {
			t.Fatalf("got %q want %q", b, content)
		}
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 1 {
		t.Errorf("leftover files: %v", entries)
	}
}

func TestCopyFileAtomic(t *testing.T) {
	dir := t.TempDir()
	src, dst := filepath.Join(dir, "src"), filepath.Join(dir, "dst")
	os.WriteFile(src, []byte("payload"), 0o644)
	if err := CopyFileAtomic(src, dst, 0o755); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(dst); string(b) != "payload" {
		t.Errorf("got %q", b)
	}
	if err := CopyFileAtomic(filepath.Join(dir, "missing"), dst, 0o755); err == nil {
		t.Error("expected error for missing source")
	}
	if b, _ := os.ReadFile(dst); string(b) != "payload" {
		t.Error("failed copy must not damage the existing destination")
	}
}

func TestFailedWriteCleansUpTemp(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "d")
	os.Mkdir(target, 0o755)
	os.WriteFile(filepath.Join(target, "x"), nil, 0o644)
	if err := WriteFileAtomic(target, []byte("x"), 0o600); err == nil {
		t.Fatal("expected error")
	}
	entries, _ := os.ReadDir(dir)
	if len(entries) != 1 {
		t.Errorf("temp file leaked: %v", entries)
	}
}

func TestDirSyncFailureAfterRenameKeepsNewContent(t *testing.T) {
	old := syncDir
	syncDir = func(string) error { return errors.New("EINVAL") }
	defer func() { syncDir = old }()

	p := filepath.Join(t.TempDir(), "f")
	os.WriteFile(p, []byte("old"), 0o600)
	err := WriteFileAtomic(p, []byte("new"), 0o600)
	if !errors.Is(err, ErrDirSync) {
		t.Fatalf("err = %v, want ErrDirSync", err)
	}
	if b, _ := os.ReadFile(p); string(b) != "new" {
		t.Errorf("content = %q, want new (rename already happened)", b)
	}
	if m, _ := filepath.Glob(filepath.Join(filepath.Dir(p), ".tmp-*")); len(m) != 0 {
		t.Errorf("temp files left: %v", m)
	}
}

func TestCopyFileAtomicReportsDirSyncDistinctly(t *testing.T) {
	old := syncDir
	syncDir = func(string) error { return errors.New("EINVAL") }
	defer func() { syncDir = old }()

	dir := t.TempDir()
	src, dst := filepath.Join(dir, "a"), filepath.Join(dir, "b")
	os.WriteFile(src, []byte("data"), 0o644)
	if err := CopyFileAtomic(src, dst, 0o755); !errors.Is(err, ErrDirSync) {
		t.Fatalf("err = %v, want ErrDirSync", err)
	}
	if b, _ := os.ReadFile(dst); string(b) != "data" {
		t.Errorf("dst = %q", b)
	}
}

func TestQuarantineMovesFileAside(t *testing.T) {
	p := filepath.Join(t.TempDir(), "state.json")
	os.WriteFile(p, []byte("garbage"), 0o600)
	kept, err := Quarantine(p)
	if err != nil || kept != p+".corrupt" {
		t.Fatalf("Quarantine = %q, %v", kept, err)
	}
	if _, err := os.Stat(p); !os.IsNotExist(err) {
		t.Error("original still in place")
	}
	if b, _ := os.ReadFile(kept); string(b) != "garbage" {
		t.Errorf("kept = %q", b)
	}
	if _, err := Quarantine(p); err == nil {
		t.Error("quarantining a missing file should fail")
	}
}

func TestSweepTempOnlyRemovesOldTempFiles(t *testing.T) {
	dir := t.TempDir()
	mk := func(name string, age time.Duration) string {
		p := filepath.Join(dir, name)
		os.WriteFile(p, []byte("x"), 0o600)
		past := time.Now().Add(-age)
		os.Chtimes(p, past, past)
		return p
	}
	oldTmp, freshTmp := mk(".tmp-aaa", 3*time.Hour), mk(".tmp-bbb", time.Minute)
	other, regular := mk(".tmpfile-not-ours", 3*time.Hour), mk("state.json", 3*time.Hour)
	os.Mkdir(filepath.Join(dir, ".tmp-dir"), 0o755)

	SweepTemp(dir, time.Hour)
	exists := func(p string) bool { _, err := os.Stat(p); return err == nil }
	if exists(oldTmp) {
		t.Error("old .tmp- file survived")
	}
	for _, p := range []string{freshTmp, other, regular, filepath.Join(dir, ".tmp-dir")} {
		if !exists(p) {
			t.Errorf("%s was removed", filepath.Base(p))
		}
	}
	SweepTemp(filepath.Join(dir, "missing"), time.Hour)
}
