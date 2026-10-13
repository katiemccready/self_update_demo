package install

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"selfupdate/internal/fsutil"
	"selfupdate/internal/update/manifest"
)

const (
	RootEnv = "SELFUPDATE_ROOT"

	RestartExitCode     = 75
	EnvironmentExitCode = 78
)

var ErrNotManaged = errors.New("not running from a managed install root (" + RootEnv + " is not set)")

type Root struct{ Dir string }

func RootFromEnv() (Root, error) {
	d := os.Getenv(RootEnv)
	if d == "" {
		return Root{}, ErrNotManaged
	}
	abs, err := filepath.Abs(d)
	if err != nil {
		return Root{}, err
	}
	return Root{Dir: abs}, nil
}

const (
	pointerFile   = "current"
	stateFile     = "state.json"
	configFile    = "root.json"
	versionsDir   = "versions"
	stagingDir    = "staging"
	stateLockFile = ".state.lock"
	applyLockFile = ".apply.lock"
)

func (r Root) pointer() string   { return filepath.Join(r.Dir, pointerFile) }
func (r Root) state() string     { return filepath.Join(r.Dir, stateFile) }
func (r Root) config() string    { return filepath.Join(r.Dir, configFile) }
func (r Root) stateLock() string { return filepath.Join(r.Dir, stateLockFile) }
func (r Root) Versions() string  { return filepath.Join(r.Dir, versionsDir) }
func (r Root) Staging() string   { return filepath.Join(r.Dir, stagingDir) }
func (r Root) ApplyLock() string { return filepath.Join(r.Dir, applyLockFile) }

func (r Root) VersionDir(v manifest.Version) string {
	return filepath.Join(r.Versions(), v.String())
}

func (r Root) entryPath(v manifest.Version, entry string) string {
	return filepath.Join(r.VersionDir(v), filepath.FromSlash(entry))
}

type rootConfig struct {
	Entry string `json:"entry"`
}

func (r Root) ReadEntry() (string, error) {
	b, err := os.ReadFile(r.config())
	if err != nil {
		return "", fmt.Errorf("read %s: %w", configFile, err)
	}
	var c rootConfig
	if err := json.Unmarshal(b, &c); err != nil {
		return "", fmt.Errorf("decode %s: %w", configFile, err)
	}
	if err := validateEntry(c.Entry); err != nil {
		return "", fmt.Errorf("%s: %w", configFile, err)
	}
	return c.Entry, nil
}

func validateEntry(entry string) error {
	if entry == "" || strings.HasPrefix(entry, "/") || strings.Contains(entry, "\\") {
		return fmt.Errorf("invalid entry %q", entry)
	}
	for _, el := range strings.Split(entry, "/") {
		if el == "" || el == "." || el == ".." {
			return fmt.Errorf("invalid entry %q", entry)
		}
	}
	return nil
}

func (r Root) ReadPointer() (manifest.Version, error) {
	b, err := fsutil.ReadFile(r.pointer())
	if err != nil {
		return manifest.Version{}, err
	}
	v, err := manifest.ParseVersion(strings.TrimSpace(string(b)))
	if err != nil {
		return manifest.Version{}, fmt.Errorf("pointer file %s: %w", pointerFile, err)
	}
	return v, nil
}

func (r Root) WritePointer(v manifest.Version) error {
	return fsutil.IgnoreDirSync(fsutil.WriteFileAtomic(r.pointer(), []byte(v.String()+"\n"), 0o644))
}

func (r Root) usable(v manifest.Version, entry string) bool {
	fi, err := os.Stat(r.entryPath(v, entry))
	return err == nil && fi.Mode().IsRegular()
}

func (r Root) Launcher() string {
	name := "launcher"
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	return filepath.Join(r.Dir, name)
}
