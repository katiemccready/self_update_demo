package release

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"selfupdate/internal/fsutil"
	"selfupdate/internal/update/manifest"
)

func TestPublishWaitsForAndRefusesWhileTargetIsLocked(t *testing.T) {
	old := publishLockTimeout
	publishLockTimeout = 50 * time.Millisecond
	defer func() { publishLockTimeout = old }()

	root := t.TempDir()
	o := opts(t, root, "1.0.0")
	dir := targetDir(root, o.Channel, o.OS, o.Arch)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	held := fsutil.NewFileLock(filepath.Join(dir, ".publish.lock"))
	if err := held.TryLock(); err != nil {
		t.Fatal(err)
	}
	if _, err := Publish(o); !errors.Is(err, fsutil.ErrLockHeld) {
		t.Fatalf("Publish while locked: err = %v, want ErrLockHeld", err)
	}
	if _, err := Refresh(RefreshOptions{Root: root, Channel: o.Channel, OS: o.OS, Arch: o.Arch, Key: o.Key}); !errors.Is(err, fsutil.ErrLockHeld) {
		t.Fatalf("Refresh while locked: err = %v, want ErrLockHeld", err)
	}
	held.Unlock()
	if _, err := Publish(o); err != nil {
		t.Fatalf("Publish after unlock: %v", err)
	}
}

func TestRefreshRefusesMisplacedManifest(t *testing.T) {
	root := t.TempDir()
	o := opts(t, root, "1.0.0")
	if _, err := Publish(o); err != nil {
		t.Fatal(err)
	}
	src := filepath.Join(targetDir(root, "stable", "linux", "amd64"), manifest.FileName)
	dst := targetDir(root, "stable", "darwin", "arm64")
	if err := os.MkdirAll(dst, 0o755); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(src)
	os.WriteFile(filepath.Join(dst, manifest.FileName), b, 0o644)

	_, err := Refresh(RefreshOptions{Root: root, Channel: "stable", OS: "darwin", Arch: "arm64", Key: o.Key})
	if err == nil {
		t.Fatal("Refresh re-signed a manifest that describes a different target")
	}
}
