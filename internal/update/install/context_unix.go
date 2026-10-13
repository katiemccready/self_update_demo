//go:build !windows

package install

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"

	"selfupdate/internal/fsutil"
)

func CheckContext(dir string, allowUnsafe bool) error {
	if allowUnsafe {
		return nil
	}
	di, err := os.Stat(dir)
	if err != nil {
		return err
	}
	if di.Mode().Perm()&0o002 != 0 {
		return fmt.Errorf("%w: %s is world-writable (mode %o); any local user could replace the update while it is being installed",
			fsutil.ErrUnsafeLocation, dir, di.Mode().Perm())
	}
	if os.Geteuid() != 0 {
		return nil
	}
	if err := checkAncestors(dir); err != nil {
		return err
	}
	for _, p := range []string{dir, filepath.Join(dir, versionsDir)} {
		fi, err := os.Stat(p)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return err
		}
		st, ok := fi.Sys().(*syscall.Stat_t)
		if !ok {
			continue
		}
		if st.Uid != 0 {
			return fmt.Errorf("%w: running as root but %s is owned by uid %d; that user could get root to run a file of their choosing",
				fsutil.ErrUnsafeLocation, p, st.Uid)
		}
		if fi.Mode().Perm()&0o022 != 0 {
			return fmt.Errorf("%w: running as root but %s is writable by group/others (mode %o)",
				fsutil.ErrUnsafeLocation, p, fi.Mode().Perm())
		}
	}
	return nil
}

func checkAncestors(dir string) error {
	for p := filepath.Dir(dir); ; p = filepath.Dir(p) {
		fi, err := os.Stat(p)
		if err != nil {
			return err
		}
		if err := ancestorProblem(p, fi); err != nil {
			return err
		}
		if filepath.Dir(p) == p {
			return nil
		}
	}
}

func ancestorProblem(p string, fi os.FileInfo) error {
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return nil
	}
	if st.Uid != 0 {
		return fmt.Errorf("%w: running as root but ancestor directory %s is owned by uid %d; that user could swap the install directory",
			fsutil.ErrUnsafeLocation, p, st.Uid)
	}
	if fi.Mode().Perm()&0o022 != 0 && fi.Mode()&os.ModeSticky == 0 {
		return fmt.Errorf("%w: running as root but ancestor directory %s is writable by group/others (mode %o)",
			fsutil.ErrUnsafeLocation, p, fi.Mode().Perm())
	}
	return nil
}
