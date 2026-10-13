package install

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"

	"selfupdate/internal/fsutil"
	"selfupdate/internal/pack"
	"selfupdate/internal/update/manifest"
)

func Bootstrap(root Root, launcherSrc string, pkg io.Reader, v manifest.Version, entry string) error {
	if err := validateEntry(entry); err != nil {
		return err
	}
	if _, err := os.Stat(root.pointer()); err == nil {
		return fmt.Errorf("%s is already an install root (%s exists)", root.Dir, pointerFile)
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if err := os.MkdirAll(root.Staging(), 0o755); err != nil {
		return err
	}
	if err := os.MkdirAll(root.Versions(), 0o755); err != nil {
		return err
	}
	tmp, err := os.MkdirTemp(root.Staging(), "bootstrap-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(tmp)
	if _, err := pack.Extract(pkg, tmp, entry, pack.DefaultLimits); err != nil {
		return err
	}
	dst := root.VersionDir(v)
	if err := os.RemoveAll(dst); err != nil {
		return err
	}
	if err := fsutil.Rename(tmp, dst); err != nil {
		return err
	}
	if err := fsutil.IgnoreDirSync(fsutil.CopyFileAtomic(launcherSrc, root.Launcher(), 0o755)); err != nil {
		return fmt.Errorf("install the launcher: %w", err)
	}
	cfg, _ := json.Marshal(rootConfig{Entry: entry})
	if err := fsutil.IgnoreDirSync(fsutil.WriteFileAtomic(root.config(), cfg, 0o644)); err != nil {
		return err
	}
	st, _ := json.Marshal(State{LastGood: v.String()})
	if err := fsutil.IgnoreDirSync(fsutil.WriteFileAtomic(root.state(), st, 0o600)); err != nil {
		return err
	}
	return root.WritePointer(v)
}
