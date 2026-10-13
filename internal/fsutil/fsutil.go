package fsutil

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

var ErrUnsafeLocation = errors.New("unsafe install location")
var ErrDirSync = errors.New("directory fsync failed after rename")

func IgnoreDirSync(err error) error {
	if errors.Is(err, ErrDirSync) {
		return nil
	}
	return err
}

var syncDir = SyncDir

func WriteFileAtomic(path string, data []byte, perm os.FileMode) error {
	return writeAtomic(path, perm, func(w io.Writer) error {
		_, err := w.Write(data)
		return err
	})
}

func CopyFileAtomic(src, dst string, perm os.FileMode) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	return writeAtomic(dst, perm, func(w io.Writer) error {
		_, err := io.Copy(w, in)
		return err
	})
}

// writeAtomic writes through a temp file in the destination directory (so the
// rename is atomic), fsyncing the file before the rename so a power loss can
// never expose unflushed data, and the directory after it.
func writeAtomic(path string, perm os.FileMode, fill func(io.Writer) error) (err error) {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".tmp-*")
	if err != nil {
		return err
	}
	renamed := false
	defer func() {
		if err != nil && !renamed {
			tmp.Close()
			os.Remove(tmp.Name())
		}
	}()
	if err = fill(tmp); err != nil {
		return err
	}
	if err = tmp.Chmod(perm); err != nil {
		return err
	}
	if err = tmp.Sync(); err != nil {
		return err
	}
	if err = tmp.Close(); err != nil {
		return err
	}
	if err = Rename(tmp.Name(), path); err != nil {
		return err
	}
	renamed = true
	if serr := syncDir(dir); serr != nil {
		return fmt.Errorf("%w: %v", ErrDirSync, serr)
	}
	return nil
}

func SyncDir(dir string) error {
	if runtime.GOOS == "windows" {
		return nil
	}
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Sync()
}

func Hash(r io.Reader) (sum string, n int64, err error) {
	h := sha256.New()
	n, err = io.Copy(h, r)
	return hex.EncodeToString(h.Sum(nil)), n, err
}

func HashFile(path string) (sum string, n int64, err error) {
	f, err := os.Open(path)
	if err != nil {
		return "", 0, err
	}
	defer f.Close()
	return Hash(f)
}

func Quarantine(path string) (string, error) {
	kept := path + ".corrupt"
	if err := Rename(path, kept); err != nil {
		return "", err
	}
	return kept, nil
}

func SweepTemp(dir string, olderThan time.Duration) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	for _, e := range entries {
		if e.IsDir() || !strings.HasPrefix(e.Name(), ".tmp-") {
			continue
		}
		if fi, err := e.Info(); err == nil && time.Since(fi.ModTime()) > olderThan {
			_ = os.Remove(filepath.Join(dir, e.Name()))
		}
	}
}
