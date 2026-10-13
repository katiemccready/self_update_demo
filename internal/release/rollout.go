package release

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"selfupdate/internal/fsutil"
	"selfupdate/internal/update/manifest"
)

const (
	previousFile = "previous.json"
	rolloutFile  = "rollout.json"
)

type Rollout struct {
	Version string `json:"version"`
	Percent int    `json:"percent"`
	Halt    bool   `json:"halt,omitempty"`
	Note    string `json:"note,omitempty"`
}

func (r Rollout) validate() error {
	if _, err := manifest.ParseVersion(r.Version); err != nil {
		return err
	}
	if r.Percent < 0 || r.Percent > 100 {
		return fmt.Errorf("rollout percent %d is outside 0..100", r.Percent)
	}
	return nil
}

func Bucket(clientID, version string) int {
	sum := sha256.Sum256([]byte(version + "\x00" + clientID))
	return int(binary.BigEndian.Uint64(sum[:8]) % 100)
}

func (r Rollout) includes(clientID string) bool {
	if r.Halt {
		return false
	}
	if r.Percent >= 100 {
		return true
	}
	if clientID == "" {
		return false
	}
	return Bucket(clientID, r.Version) < r.Percent
}

func readRollout(dir string) (r Rollout, ok bool, err error) {
	b, err := os.ReadFile(filepath.Join(dir, rolloutFile))
	if errors.Is(err, os.ErrNotExist) {
		return Rollout{}, false, nil
	}
	if err != nil {
		return Rollout{}, false, err
	}
	if err := json.Unmarshal(b, &r); err != nil {
		return Rollout{}, false, fmt.Errorf("%s: %w", rolloutFile, err)
	}
	if err := r.validate(); err != nil {
		return Rollout{}, false, fmt.Errorf("%s: %w", rolloutFile, err)
	}
	return r, true, nil
}

func writeRollout(dir string, r Rollout) error {
	if err := r.validate(); err != nil {
		return err
	}
	b, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		return err
	}
	return fsutil.IgnoreDirSync(fsutil.WriteFileAtomic(filepath.Join(dir, rolloutFile), b, 0o644))
}

type RolloutOptions struct {
	Root, Channel, OS, Arch string
	Percent                 *int
	Halt, Resume            bool
	Note                    string
}

func SetRollout(o RolloutOptions) (Rollout, error) {
	if err := validateTarget(o.Channel, o.OS, o.Arch); err != nil {
		return Rollout{}, err
	}
	if o.Halt && o.Resume {
		return Rollout{}, errors.New("halt and resume are opposites")
	}
	if !o.Halt && !o.Resume && o.Percent == nil {
		return Rollout{}, errors.New("nothing to do: give a percent, halt or resume")
	}
	dir := targetDir(o.Root, o.Channel, o.OS, o.Arch)
	unlock, err := lockTarget(dir, false)
	if err != nil {
		return Rollout{}, err
	}
	defer unlock()
	b, err := os.ReadFile(filepath.Join(dir, manifest.FileName))
	if err != nil {
		return Rollout{}, fmt.Errorf("read published manifest: %w", err)
	}
	latest, err := decodeUnverified(b)
	if err != nil {
		return Rollout{}, fmt.Errorf("published manifest unreadable: %w", err)
	}
	cur, ok, err := readRollout(dir)
	if err != nil {
		return Rollout{}, err
	}
	if !ok || cur.Version != latest.Version {
		cur = Rollout{Version: latest.Version, Percent: 100}
	}
	if o.Percent != nil {
		cur.Percent = *o.Percent
	}
	if o.Halt {
		cur.Halt = true
	}
	if o.Resume {
		cur.Halt = false
	}
	if o.Note != "" {
		cur.Note = o.Note
	}
	return cur, writeRollout(dir, cur)
}

var (
	errNoManifest = errors.New("no manifest for this client")

	errHalted = fmt.Errorf("%w: rollout halted", errNoManifest)
)

type served string

const (
	servedLatest   served = "latest"
	servedPrevious served = "previous"
)

// manifestFor returns the manifest bytes to give the client with this ID. It
// reads three files that Publish writes in the order previous, rollout,
// manifest, so a request racing a publish sees the old state, the new one, or
// the old manifest with a rollout for a version it does not name (which is
// ignored): never a new release at the wrong percentage.
//
// Anything it cannot interpret is an error and not a guess: a rollout file that
// does not parse must not fail open to "everyone", or a mistyped edit during an
// incident would ship the bad release to all clients.
func manifestFor(root, channel, goos, goarch, clientID string) (body []byte, s served, err error) {
	if err := validateTarget(channel, goos, goarch); err != nil {
		return nil, "", err
	}
	dir := targetDir(root, channel, goos, goarch)
	latest, err := os.ReadFile(filepath.Join(dir, manifest.FileName))
	if errors.Is(err, os.ErrNotExist) {
		return nil, "", errNoManifest
	}
	if err != nil {
		return nil, "", err
	}
	r, ok, err := readRollout(dir)
	if err != nil {
		return nil, "", err
	}
	if !ok {
		return latest, servedLatest, nil
	}
	m, err := decodeUnverified(latest)
	if err != nil {
		return nil, "", fmt.Errorf("published manifest unreadable: %w", err)
	}
	if r.Version != m.Version {
		return latest, servedLatest, nil
	}
	if r.Halt {
		return nil, "", errHalted
	}
	if r.includes(clientID) {
		return latest, servedLatest, nil
	}
	prev, err := os.ReadFile(filepath.Join(dir, previousFile))
	if errors.Is(err, os.ErrNotExist) {
		return nil, "", errNoManifest
	}
	if err != nil {
		return nil, "", err
	}
	return prev, servedPrevious, nil
}
